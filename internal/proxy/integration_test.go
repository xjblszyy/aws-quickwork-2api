package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"quickwork-openai/internal/accounts"
	"quickwork-openai/internal/config"
	"quickwork-openai/internal/store"
)

type testBackend func(context.Context, *Session, ChatInput, EventFunc) (ChatResult, error)

func (f testBackend) Chat(ctx context.Context, sess *Session, in ChatInput, event EventFunc) (ChatResult, error) {
	return f(ctx, sess, in, event)
}

func testServer(t *testing.T, backend testBackend) (*Server, *store.Store) {
	t.Helper()
	cfg := config.Default()
	cfg.APIKeys = []string{"test-key"}
	cfg.ModelMappings = map[string]string{"opus5.5": "smart"}
	cfg.SessionReuse = false
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Save(context.Background(), &store.Account{Email: "test@example.invalid", Provider: "social", BuildStage: "prod", IDToken: "test-jwt", WSHost: "test.invalid", ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(&cfg, accounts.New(st, &cfg))
	s.upstream = backend
	return s, st
}

func TestProtocolHTTPAndRecords(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(protocol+map[bool]string{false: "/json", true: "/stream"}[stream], func(t *testing.T) {
				s, st := testServer(t, func(ctx context.Context, sess *Session, in ChatInput, event EventFunc) (ChatResult, error) {
					if in.Model != "smart" || !strings.Contains(in.Prompt, "private test prompt") {
						t.Errorf("bad normalized input %+v", in)
					}
					if event != nil {
						if err := event(StreamEvent{Type: "text", Text: "hello"}); err != nil {
							return ChatResult{}, err
						}
					}
					return ChatResult{Text: "hello", ThreadID: "test-thread", Usage: Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}}, nil
				})
				body := map[string]any{"model": "opus5.5", "stream": stream}
				path := "/v1/chat/completions"
				if protocol == "responses" {
					path = "/v1/responses"
					body["input"] = "private test prompt"
				} else {
					body["messages"] = []map[string]any{{"role": "user", "content": "private test prompt"}}
				}
				if protocol == "messages" {
					path = "/v1/messages"
					body["max_tokens"] = 128
				}
				data, _ := json.Marshal(body)
				r := httptest.NewRequest("POST", path, strings.NewReader(string(data)))
				r.Header.Set("X-Api-Key", "test-key")
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				if w.Code != 200 || !strings.Contains(w.Body.String(), "hello") || !strings.Contains(w.Body.String(), "opus5.5") {
					t.Fatalf("%d %s", w.Code, w.Body.String())
				}
				if stream {
					ending := map[string]string{"chat": "[DONE]", "responses": "response.completed", "messages": "message_stop"}[protocol]
					if !strings.Contains(w.Body.String(), ending) {
						t.Fatalf("missing %s: %s", ending, w.Body.String())
					}
					if protocol != "chat" && !strings.Contains(w.Body.String(), `"output_tokens":2`) {
						t.Fatalf("missing usage: %s", w.Body.String())
					}
				} else {
					var decoded any
					if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
						t.Fatal(err)
					}
				}
				rows, total, err := st.ListRequests(context.Background(), store.RequestFilter{Limit: 20})
				if err != nil || total != 1 || rows[0].Status != 200 || rows[0].Protocol != protocol || rows[0].Attempts != 1 || rows[0].UpstreamModel != "smart" || rows[0].TotalTokens != 5 {
					t.Fatalf("records %+v %d %v", rows, total, err)
				}
				encoded, _ := json.Marshal(rows)
				if strings.Contains(string(encoded), "private test prompt") || strings.Contains(string(encoded), "test-key") {
					t.Fatal("request record leaked sensitive data")
				}
			})
		}
	}
}

func TestProtocolsRejectUnauthorizedAndInvalidInput(t *testing.T) {
	s, st := testServer(t, func(context.Context, *Session, ChatInput, EventFunc) (ChatResult, error) {
		t.Fatal("unauthorized request reached upstream")
		return ChatResult{}, nil
	})
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(`{}`)))
		if w.Code != 401 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if path == "/v1/messages" && !strings.Contains(w.Body.String(), "authentication_error") {
			t.Fatal(w.Body.String())
		}
	}
	_, total, err := st.ListRequests(context.Background(), store.RequestFilter{Failed: true})
	if err != nil || total != 3 {
		t.Fatalf("%d %v", total, err)
	}
}

func TestProtocolsReportNoAccountsAsUnavailable(t *testing.T) {
	cfg := config.Default()
	cfg.APIKeys = []string{"test-key"}
	cfg.SessionReuse = false
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := NewServer(&cfg, accounts.New(st, &cfg))
	s.upstream = testBackend(func(context.Context, *Session, ChatInput, EventFunc) (ChatResult, error) {
		t.Fatal("request reached upstream without an account")
		return ChatResult{}, nil
	})
	body := `{"model":"smart","max_tokens":16,"input":"hi","messages":[{"role":"user","content":"hi"}]}`
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer test-key")
		s.Handler().ServeHTTP(w, r)
		want := "upstream_session_unavailable"
		if path == "/v1/messages" {
			want = `"type":"api_error"`
		}
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
}

func TestProtocolToolResponses(t *testing.T) {
	for _, protocol := range []string{"responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(protocol+map[bool]string{false: "/json", true: "/stream"}[stream], func(t *testing.T) {
				s, _ := testServer(t, func(context.Context, *Session, ChatInput, EventFunc) (ChatResult, error) {
					return ChatResult{Text: `<client_tool_call>{"name":"lookup","arguments":{"query":"test"}}</client_tool_call>`, Usage: Usage{TotalTokens: 5}}, nil
				})
				body := map[string]any{"model": "opus5.5", "stream": stream}
				path := "/v1/responses"
				if protocol == "responses" {
					body["input"] = "look up test"
					body["tools"] = []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}
				} else {
					path = "/v1/messages"
					body["max_tokens"] = 128
					body["messages"] = []any{map[string]any{"role": "user", "content": "look up test"}}
					body["tools"] = []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}}
				}
				data, _ := json.Marshal(body)
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(data)))
				r.Header.Set("Authorization", "Bearer test-key")
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				if w.Code != 200 || !strings.Contains(w.Body.String(), "lookup") || !strings.Contains(w.Body.String(), map[string]string{"responses": "function_call", "messages": "tool_use"}[protocol]) {
					t.Fatalf("%d %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestConcurrentChildConversationIsolation(t *testing.T) {
	var mu sync.Mutex
	threads := map[string]string{}
	s, _ := testServer(t, func(ctx context.Context, sess *Session, in ChatInput, event EventFunc) (ChatResult, error) {
		mu.Lock()
		defer mu.Unlock()
		if in.CreateSession {
			if in.ThreadID != "" {
				t.Error("client supplied upstream thread ID")
			}
			id := uuid()
			threads[in.Prompt] = id
			return ChatResult{Text: "done", ThreadID: id}, nil
		}
		found := false
		for _, id := range threads {
			if id == in.ThreadID {
				found = true
			}
		}
		if !found {
			t.Error("resumed unrecognized upstream thread")
		}
		return ChatResult{Text: "done", ThreadID: in.ThreadID}, nil
	})
	s.cfg.SessionReuse = true
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			first := fmt.Sprintf("child-%d", i)
			for turn := 0; turn < 2; turn++ {
				messages := []Message{{Role: "user", Content: first}}
				if turn > 0 {
					messages = append(messages, Message{Role: "assistant", Content: "done"}, Message{Role: "user", Content: "next"})
				}
				body, _ := json.Marshal(ChatRequest{Model: "opus5.5", Messages: messages})
				r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body)))
				r.Header.Set("X-Api-Key", "test-key")
				r.Header.Set("X-Conversation-Id", first)
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				if w.Code != 200 {
					t.Errorf("%d %s", w.Code, w.Body.String())
				}
			}
		}(i)
	}
	wg.Wait()
	if len(threads) != 8 {
		t.Fatalf("child requests shared sessions: %d", len(threads))
	}
}

func TestProtocolToolResultRoundTrip(t *testing.T) {
	for _, test := range []struct{ path, body string }{
		{"/v1/responses", `{"model":"opus5.5","input":[{"role":"user","content":"calculate"},{"type":"function_call","call_id":"call-1","name":"calculator","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","output":"tool-result-42"}]}`},
		{"/v1/messages", `{"model":"opus5.5","max_tokens":128,"messages":[{"role":"user","content":"calculate"},{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"calculator","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"tool-result-42"}]}]}`},
	} {
		t.Run(test.path, func(t *testing.T) {
			s, _ := testServer(t, func(ctx context.Context, sess *Session, in ChatInput, event EventFunc) (ChatResult, error) {
				if !strings.Contains(in.Prompt, "tool-result-42") {
					t.Error("tool output lost")
				}
				return ChatResult{Text: "42"}, nil
			})
			r := httptest.NewRequest("POST", test.path, strings.NewReader(test.body))
			r.Header.Set("X-Api-Key", "test-key")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "42") {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCustomToolStreamingOutput(t *testing.T) {
	s, _ := testServer(t, func(context.Context, *Session, ChatInput, EventFunc) (ChatResult, error) {
		return ChatResult{Text: `<client_tool_call>{"name":"apply_patch","arguments":{"input":"raw patch input"}}</client_tool_call>`}, nil
	})
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"opus5.5","stream":true,"input":"use patch","tools":[{"type":"custom","name":"apply_patch"}]}`))
	r.Header.Set("X-Api-Key", "test-key")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "response.custom_tool_call_input.done") || !strings.Contains(w.Body.String(), `"input":"raw patch input"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestResponsesContentPartIsAddedOnlyOnce(t *testing.T) {
	s, _ := testServer(t, func(ctx context.Context, sess *Session, in ChatInput, event EventFunc) (ChatResult, error) {
		if event != nil {
			_ = event(StreamEvent{Type: "text", Text: "hello"})
		}
		return ChatResult{Text: "hello"}, nil
	})
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"opus5.5","stream":true,"input":"hello"}`))
	r.Header.Set("X-Api-Key", "test-key")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			continue
		}
		if event["type"] == "response.output_item.added" {
			item := event["item"].(map[string]any)
			if item["type"] == "message" && len(item["content"].([]any)) != 0 {
				t.Fatal("content is prepopulated before response.content_part.added, which breaks the official SDK accumulator")
			}
		}
	}
}
