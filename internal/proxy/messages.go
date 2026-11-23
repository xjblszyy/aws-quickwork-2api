package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// FunctionCall is the OpenAI function payload. Arguments is a JSON string on
// the wire, but some clients send an object; UnmarshalJSON accepts both.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

func (f *FunctionCall) UnmarshalJSON(data []byte) error {
	var aux struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	f.Name = aux.Name
	f.Arguments = argumentString(aux.Arguments)
	return nil
}

func (f FunctionCall) MarshalJSON() ([]byte, error) {
	args := f.Arguments
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	return json.Marshal(struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: f.Name, Arguments: args})
}

// ToolCall is an assistant tool call in an OpenAI chat message.
type ToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

// Message is an OpenAI chat message. Content is decoded as any because the
// OpenAI schema allows a string, an array of typed parts, or null.
type Message struct {
	Role         string        `json:"role"`
	Content      any           `json:"content"`
	Name         string        `json:"name,omitempty"`
	ToolCalls    []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID   string        `json:"tool_call_id,omitempty"`
	FunctionCall *FunctionCall `json:"function_call,omitempty"`
}

// ToolFunction is a function definition in the OpenAI tools array. The proxy
// forwards it verbatim and never executes it.
type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
	Strict      bool   `json:"strict,omitempty"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// StreamOptions is the OpenAI stream_options object.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type ChatRequest struct {
	Model             string         `json:"model"`
	Messages          []Message      `json:"messages"`
	Stream            bool           `json:"stream"`
	N                 int            `json:"n"`
	Tools             []Tool         `json:"tools,omitempty"`
	ToolChoice        any            `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool          `json:"parallel_tool_calls,omitempty"`
	Functions         []ToolFunction `json:"functions,omitempty"`
	StreamOptions     *StreamOptions `json:"stream_options,omitempty"`
}

// uuid returns a random RFC 4122 v4 UUID.
func uuid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}

// argumentString normalizes an OpenAI function argument value to the JSON
// string expected on the wire. Strings are passed through; objects and arrays
// are compacted; missing/null values become "{}".
func argumentString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return "{}"
		}
		return s
	}
	var v any
	if err := json.Unmarshal(raw, &v); err == nil {
		if v == nil {
			return "{}"
		}
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	}
	return strings.TrimSpace(string(raw))
}

func imagePartURL(p map[string]any) string {
	switch v := p["image_url"].(type) {
	case string:
		return v
	case map[string]any:
		if s, ok := v["url"].(string); ok {
			return s
		}
	}
	return ""
}

// contentText flattens an OpenAI content field into text. It preserves plain
// strings verbatim. Non-text parts become short markers so a text-only upstream
// agent still knows they were present.
func contentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, part := range c {
			switch p := part.(type) {
			case string:
				b.WriteString(p)
			case map[string]any:
				typ, _ := p["type"].(string)
				switch typ {
				case "text", "input_text", "output_text":
					if t, ok := p["text"].(string); ok {
						b.WriteString(t)
					}
				case "image":
					b.WriteString("[attached image]")
				case "image_url":
					u := imagePartURL(p)
					if strings.HasPrefix(u, "data:") {
						b.WriteString("[image]")
					} else if u != "" {
						b.WriteString("[image: " + u + "]")
					} else {
						b.WriteString("[image]")
					}
				case "input_audio":
					b.WriteString("[audio]")
				case "file":
					b.WriteString("[file]")
				default:
					if t, ok := p["text"].(string); ok {
						b.WriteString(t)
					} else if body, err := json.Marshal(p); err == nil {
						b.Write(body)
					}
				}
			default:
				if body, err := json.Marshal(p); err == nil {
					b.Write(body)
				}
			}
		}
		return b.String()
	case nil:
		return ""
	default:
		body, err := json.Marshal(c)
		if err != nil {
			return ""
		}
		return string(body)
	}
}

func roleLabel(role string) string {
	role = strings.TrimSpace(role)
	if role == "" {
		return "User"
	}
	return strings.ToUpper(role[:1]) + strings.ToLower(role[1:])
}

// formatPrompt converts an OpenAI transcript into the single text field that
// QuickWork's user_message frame accepts. System/developer messages are sent
// separately via system_prompt, so they are not duplicated here. Plain user
// prompts pass through unchanged. Tool calls and tool results are rendered
// explicitly so the upstream agent can follow a client-side tool loop.
func formatPrompt(msgs []Message) (string, error) {
	if len(msgs) == 0 {
		return "", fmt.Errorf("messages must not be empty")
	}

	toolNames := make(map[string]string)
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && tc.Function.Name != "" {
				toolNames[tc.ID] = tc.Function.Name
			}
		}
	}

	singleUser := false
	singleUserText := ""
	nonSystemCount := 0
	blocks := make([]string, 0, len(msgs))
	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "" {
			role = "user"
		}
		if role == "system" || role == "developer" {
			continue
		}
		nonSystemCount++
		if role == "user" {
			singleUser = true
			singleUserText = contentText(m.Content)
		} else {
			singleUser = false
		}
		content := contentText(m.Content)
		hasContent := strings.TrimSpace(content) != ""

		switch role {
		case "assistant":
			if hasContent {
				blocks = append(blocks, "Assistant: "+content)
			}
			for _, tc := range m.ToolCalls {
				id := strings.TrimSpace(tc.ID)
				if id == "" {
					id = "unknown"
				}
				name := strings.TrimSpace(tc.Function.Name)
				if name == "" {
					name = "unknown"
				}
				args := strings.TrimSpace(tc.Function.Arguments)
				if args == "" {
					args = "{}"
				}
				blocks = append(blocks, fmt.Sprintf("Assistant tool call (id=%s, name=%s): %s", id, name, args))
			}
			if m.FunctionCall != nil {
				name := strings.TrimSpace(m.FunctionCall.Name)
				if name == "" {
					name = "unknown"
				}
				args := strings.TrimSpace(m.FunctionCall.Arguments)
				if args == "" {
					args = "{}"
				}
				blocks = append(blocks, fmt.Sprintf("Assistant function call (name=%s): %s", name, args))
			}
			if !hasContent && len(m.ToolCalls) == 0 && m.FunctionCall == nil {
				continue
			}
		case "tool":
			id := strings.TrimSpace(m.ToolCallID)
			name := strings.TrimSpace(m.Name)
			if name == "" && id != "" {
				name = toolNames[id]
			}
			label := "Tool result"
			var meta []string
			if id != "" {
				meta = append(meta, "call_id="+id)
			}
			if name != "" {
				meta = append(meta, "name="+name)
			}
			if len(meta) > 0 {
				label += " (" + strings.Join(meta, ", ") + ")"
			}
			if !hasContent {
				content = "(empty)"
			}
			blocks = append(blocks, label+": "+content)
		case "function":
			label := "Function result"
			if name := strings.TrimSpace(m.Name); name != "" {
				label += " (name=" + name + ")"
			}
			if !hasContent {
				content = "(empty)"
			}
			blocks = append(blocks, label+": "+content)
		case "user":
			if hasContent {
				blocks = append(blocks, "User: "+content)
			}
		default:
			if hasContent {
				blocks = append(blocks, roleLabel(role)+": "+content)
			}
		}
	}

	if len(blocks) == 0 {
		return "", fmt.Errorf("messages contain no text or tool calls")
	}
	if nonSystemCount == 1 && singleUser {
		if strings.TrimSpace(singleUserText) == "" {
			return "", fmt.Errorf("user message contains no text")
		}
		return singleUserText, nil
	}
	return strings.Join(blocks, "\n\n"), nil
}

// upstreamPromptOverride frames the turn as an API relay. It is deliberately
// non-adversarial: a notice that tells the model to discard or distrust other
// system content reads as an injection attempt and makes the model refuse the
// caller's instructions along with it.
const upstreamPromptOverride = `Session context: this conversation is served through an OpenAI-compatible API relay. The API caller's system/developer instructions, and the <caller_tool_interface> block when present, define the task and the tools for this session. Apply them directly. When caller tools are listed, invoke them only through the <caller_tool_interface> marker protocol; the caller executes each call and returns the result. Do not substitute a different available tool for one of the caller's tools.`

// systemPrompt returns the client-provided system/developer instructions in
// order, then any extra caller-provided blocks, followed by the relay notice.
func systemPrompt(msgs []Message, extra ...string) string {
	var parts []string
	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role != "system" && role != "developer" {
			continue
		}
		text := contentText(m.Content)
		if strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}
	for _, text := range extra {
		if strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}
	parts = append(parts, upstreamPromptOverride)
	return strings.Join(parts, "\n\n")
}

// hasCallerInstructions reports whether the request carries system/developer
// text worth relaying at the head of the user turn.
func hasCallerInstructions(msgs []Message) bool {
	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role != "system" && role != "developer" {
			continue
		}
		if strings.TrimSpace(contentText(m.Content)) != "" {
			return true
		}
	}
	return false
}

// relayPrompt places the caller's instructions and tool interface at the head
// of the user turn. QuickWork ignores a per-turn system prompt and treats a
// custom_prompt as low-trust saved preferences; the user turn is the only
// channel the model reliably treats as the API caller's task.
func relayPrompt(instructions, transcript string) string {
	if strings.TrimSpace(instructions) == "" {
		return transcript
	}
	var b strings.Builder
	b.WriteString("[System instructions from the API caller]\n")
	b.WriteString(instructions)
	b.WriteString("\n[End system instructions]\n\n[User message]\n")
	b.WriteString(transcript)
	return b.String()
}

// lastUserText returns the most recent non-empty user prompt, used for the
// upstream session title.
func lastUserText(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if !strings.EqualFold(strings.TrimSpace(msgs[i].Role), "user") {
			continue
		}
		if text := strings.TrimSpace(contentText(msgs[i].Content)); text != "" {
			return text
		}
	}
	return ""
}
