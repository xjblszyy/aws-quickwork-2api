package proxy

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompactRoundTripAndKeyIsolation(t *testing.T) {
	s, _ := testServer(t, func(context.Context, *Session, ChatInput, EventFunc) (ChatResult, error) {
		return ChatResult{Text: "Summary: user requested a tool lookup; preserve pending call ID call-1.", Usage: Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}, nil
	})
	r := httptest.NewRequest("POST", "/v1/responses/compact", strings.NewReader(`{"model":"opus5.5","input":[{"role":"user","content":"private conversation"}]}`))
	r.Header.Set("X-Api-Key", "test-key")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var result struct {
		Object string
		Output []struct {
			Type             string
			EncryptedContent string `json:"encrypted_content"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Object != "response.compaction" || len(result.Output) != 1 || result.Output[0].Type != "compaction" || result.Output[0].EncryptedContent == "" {
		t.Fatalf("%s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "private conversation") || strings.Contains(w.Body.String(), "Summary:") {
		t.Fatal("compaction content was not encrypted")
	}
	token := result.Output[0].EncryptedContent
	msgs, err := s.openCompaction(token, "test-key")
	if err != nil || !strings.Contains(contentText(msgs[len(msgs)-1].Content), "Summary:") {
		t.Fatalf("%+v %v", msgs, err)
	}
	if _, err := s.openCompaction(token, "another-key"); err == nil {
		t.Fatal("compaction accepted for a different API key")
	}
	if _, err := s.openCompaction(token+"tampered", "test-key"); err == nil {
		t.Fatal("tampered token accepted")
	}
	other, _ := testServer(t, func(context.Context, *Session, ChatInput, EventFunc) (ChatResult, error) { return ChatResult{}, nil })
	if _, err := other.openCompaction(token, "test-key"); err != nil {
		t.Fatal("API-key-scoped compaction must survive a server restart")
	}
}
