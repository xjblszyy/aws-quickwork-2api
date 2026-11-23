package proxy

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

func jsonStringBytes(s string) int {
	b, _ := json.Marshal(s)
	return len(b) - 2
}

const omitted = "\n[Middle omitted to fit QuickWork's message limit.]\n"

func historyExcerpt(s string, keep int) string {
	if len(s) <= keep+len(omitted) {
		return s
	}
	head, tail := keep*2/3, len(s)-keep/3
	for !utf8.RuneStart(s[head]) {
		head--
	}
	for !utf8.RuneStart(s[tail]) {
		tail++
	}
	return s[:head] + omitted + s[tail:]
}

// budget is the remaining byte capacity for the JSON-escaped text field in
// the actual user_message. Work on a private transcript so retries and client
// history keep the originals. Keep user instructions and tool schemas intact.
func fitPrompt(in ChatInput, budget int) (string, error) {
	excess := jsonStringBytes(in.Prompt) - budget
	if excess <= 0 {
		return in.Prompt, nil
	}
	if len(in.Messages) == 0 {
		return "", ErrUserMessageTooLarge
	}
	msgs := append([]Message(nil), in.Messages...)
	shorten := func(value string, keep int) string {
		if excess <= 0 {
			return value
		}
		// Keep more of the result when the frame only slightly exceeds its limit.
		keep = max(keep, len(value)-excess-len(omitted)-8)
		excerpt := historyExcerpt(value, keep)
		if len(excerpt) != len(value) {
			excess -= jsonStringBytes(value) - jsonStringBytes(excerpt)
		}
		return excerpt
	}
	// A client can return several calls together. Preserve the whole pending
	// result group while there is still room to reclaim from older turns.
	lastAssistant := -1
	for i, msg := range msgs {
		if strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") {
			lastAssistant = i
		}
	}
	compactResults := func(group []Message, keep int) {
		for i := range group {
			if excess <= 0 {
				return
			}
			switch strings.ToLower(strings.TrimSpace(group[i].Role)) {
			case "tool", "function":
				group[i].Content = shorten(contentText(group[i].Content), keep)
			}
		}
	}
	compactResults(msgs[:lastAssistant+1], 1600)
	for i := 0; i <= lastAssistant && excess > 0; i++ {
		m := &msgs[i]
		if !strings.EqualFold(strings.TrimSpace(m.Role), "assistant") {
			continue
		}
		m.Content = shorten(contentText(m.Content), 512)
		m.ToolCalls = append([]ToolCall(nil), m.ToolCalls...)
		for j := range m.ToolCalls {
			f := &m.ToolCalls[j].Function
			f.Arguments = shorten(f.Arguments, 512)
		}
		if m.FunctionCall != nil {
			f := *m.FunctionCall
			f.Arguments = shorten(f.Arguments, 512)
			m.FunctionCall = &f
		}
	}
	compactResults(msgs[:lastAssistant+1], 384)
	compactResults(msgs[lastAssistant+1:], 1600)
	compactResults(msgs[lastAssistant+1:], 384)
	prompt, err := formatPrompt(msgs)
	if err != nil {
		return "", err
	}
	if jsonStringBytes(prompt) > budget {
		return "", ErrUserMessageTooLarge
	}
	return prompt, nil
}
