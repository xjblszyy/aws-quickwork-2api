package proxy

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestQuickWorkFrameBudget(t *testing.T) {
	source := strings.Repeat("中文<&\n", 10000)
	msgs := []Message{
		{Role: "user", Content: "original request"},
		{Role: "tool", ToolCallID: "one", Content: source},
		{Role: "tool", ToolCallID: "two", Content: "latest result"},
	}
	transcript, err := formatPrompt(msgs)
	if err != nil {
		t.Fatal(err)
	}
	in := ChatInput{Messages: msgs, Prompt: transcript, SystemPrompt: "caller rules and catalog"}
	p := map[string]any{"text": relayPrompt(in.SystemPrompt, in.Prompt)}
	if err := fitUserMessage(p, in); err != nil {
		t.Fatal(err)
	}
	n, _ := userMessageBytes(p)
	text := p["text"].(string)
	if n > maxUserMessageBytes || !utf8.ValidString(text) || msgs[1].Content != source {
		t.Fatal("invalid frame or original history changed")
	}
	for _, want := range []string{"caller rules and catalog", "User: original request", "call_id=one", "Middle omitted", "call_id=two", "latest result"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q", want)
		}
	}
	in.Prompt = source
	in.Messages = []Message{{Role: "user", Content: source}}
	p["text"] = relayPrompt(in.SystemPrompt, source)
	if err = fitUserMessage(p, in); !errors.Is(err, ErrUserMessageTooLarge) {
		t.Fatal("expected oversized input error", err)
	}
}

func TestFrameBudgetPreservesPendingToolResults(t *testing.T) {
	args := strings.Repeat("old write content ", 1000)
	latest := strings.Repeat("unread result ", 200)
	msgs := []Message{
		{Role: "user", Content: "read after writing"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "w", Function: FunctionCall{Name: "write", Arguments: args}}}},
		{Role: "tool", ToolCallID: "w", Content: "file created"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "r", Function: FunctionCall{Name: "read", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "r", Content: latest},
	}
	transcript, err := formatPrompt(msgs)
	if err != nil {
		t.Fatal(err)
	}
	in := ChatInput{Messages: msgs, Prompt: transcript}
	got, err := fitPrompt(in, 6000)
	if err != nil || !strings.Contains(got, latest) || !strings.Contains(got, omitted) || msgs[1].ToolCalls[0].Function.Arguments != args {
		t.Fatalf("pending result or original call changed (error %v)", err)
	}
}
