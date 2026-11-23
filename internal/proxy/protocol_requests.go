package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

type responsesRequest struct {
	Model              string            `json:"model"`
	Input              json.RawMessage   `json:"input"`
	Instructions       string            `json:"instructions"`
	Stream             bool              `json:"stream"`
	Tools              []json.RawMessage `json:"tools"`
	ToolChoice         any               `json:"tool_choice"`
	ParallelToolCalls  *bool             `json:"parallel_tool_calls"`
	PreviousResponseID string            `json:"previous_response_id"`
	Background         bool              `json:"background"`
}

func (r responsesRequest) chatRequest() (ChatRequest, error) {
	out := ChatRequest{Model: r.Model, Stream: r.Stream, ToolChoice: r.ToolChoice, ParallelToolCalls: r.ParallelToolCalls}
	if r.PreviousResponseID != "" || r.Background {
		return out, fmt.Errorf("previous_response_id and background execution are not supported; send the complete input history with store:false")
	}
	if r.Instructions != "" {
		out.Messages = append(out.Messages, Message{Role: "system", Content: r.Instructions})
	}
	var text string
	if json.Unmarshal(r.Input, &text) == nil {
		out.Messages = append(out.Messages, Message{Role: "user", Content: text})
	} else {
		var items []struct {
			Type, Role, ID string
			Content        any
			CallID         string `json:"call_id"`
			Name           string
			Arguments      json.RawMessage
			Output         any
			Input          string
			Namespace      string
		}
		if err := json.Unmarshal(r.Input, &items); err != nil {
			return out, fmt.Errorf("input must be a string or an array of response items")
		}
		for _, item := range items {
			switch item.Type {
			case "", "message":
				role := item.Role
				if role == "" {
					role = "user"
				}
				content, err := responseContent(item.Content)
				if err != nil {
					return out, err
				}
				out.Messages = append(out.Messages, Message{Role: role, Content: content})
			case "function_call", "custom_tool_call":
				if item.CallID == "" || item.Name == "" {
					return out, fmt.Errorf("function_call requires call_id and name")
				}
				name := item.Name
				if item.Namespace != "" {
					name = item.Namespace + "." + name
				}
				args := argumentString(item.Arguments)
				if item.Type == "custom_tool_call" {
					raw, _ := json.Marshal(map[string]string{"input": item.Input})
					args = string(raw)
				}
				out.Messages = append(out.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: item.CallID, Type: "function", Function: FunctionCall{Name: name, Arguments: args}}}})
			case "function_call_output", "custom_tool_call_output":
				if item.CallID == "" {
					return out, fmt.Errorf("function_call_output requires call_id")
				}
				content, err := responseContent(item.Output)
				if err != nil {
					return out, err
				}
				out.Messages = append(out.Messages, Message{Role: "tool", ToolCallID: item.CallID, Content: content})
			case "reasoning":
				// Reasoning items are opaque client history, not caller instructions.
			default:
				return out, fmt.Errorf("unsupported Responses input item type %q", item.Type)
			}
		}
	}
	tools, _, err := collectResponseTools(r.Tools, "")
	if err != nil {
		return out, err
	}
	out.Tools = tools
	if choice, ok := out.ToolChoice.(map[string]any); ok && choice["type"] == "function" {
		if name, ok := choice["name"].(string); ok {
			out.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return out, nil
}

func responseContent(content any) (any, error) {
	parts, ok := content.([]any)
	if !ok {
		return content, nil
	}
	out := make([]any, 0, len(parts))
	for _, value := range parts {
		part, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid content block")
		}
		switch part["type"] {
		case "input_text", "output_text", "text":
			out = append(out, map[string]any{"type": "text", "text": part["text"]})
		case "input_image":
			imageURL, _ := part["image_url"].(string)
			if imageURL == "" {
				return nil, fmt.Errorf("input_image requires image_url; file_id references are not supported")
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}})
		default:
			return nil, fmt.Errorf("unsupported Responses content block %q", part["type"])
		}
	}
	return out, nil
}

type messagesRequest struct {
	Model    string `json:"model"`
	System   any    `json:"system"`
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
	MaxTokens int  `json:"max_tokens"`
	Stream    bool `json:"stream"`
	Tools     []struct {
		Name, Description string
		InputSchema       any    `json:"input_schema"`
		Type              string `json:"type"`
	} `json:"tools"`
	ToolChoice struct {
		Type, Name      string
		DisableParallel bool `json:"disable_parallel_tool_use"`
	} `json:"tool_choice"`
}

func (r messagesRequest) chatRequest() (ChatRequest, error) {
	out := ChatRequest{Model: r.Model, Stream: r.Stream}
	if r.MaxTokens <= 0 {
		return out, fmt.Errorf("max_tokens must be positive")
	}
	if r.System != nil {
		out.Messages = append(out.Messages, Message{Role: "system", Content: contentText(r.System)})
	}
	for _, item := range r.Messages {
		// Recent Claude Code clients include caller-owned system reminders in
		// the messages array as well as in the top-level system field.
		if item.Role != "user" && item.Role != "assistant" && item.Role != "system" && item.Role != "developer" {
			return out, fmt.Errorf("unsupported message role")
		}
		if text, ok := item.Content.(string); ok {
			out.Messages = append(out.Messages, Message{Role: item.Role, Content: text})
			continue
		}
		parts, ok := item.Content.([]any)
		if !ok {
			return out, fmt.Errorf("message content must be text or an array")
		}
		message := Message{Role: item.Role}
		content := []any{}
		flush := func() {
			if len(content) > 0 || len(message.ToolCalls) > 0 {
				message.Content = content
				out.Messages = append(out.Messages, message)
				message = Message{Role: item.Role}
				content = []any{}
			}
		}
		for _, value := range parts {
			part, ok := value.(map[string]any)
			if !ok {
				return out, fmt.Errorf("invalid message content block")
			}
			switch part["type"] {
			case "text":
				content = append(content, map[string]any{"type": "text", "text": part["text"]})
			case "image":
				source, _ := part["source"].(map[string]any)
				var imageURL string
				switch source["type"] {
				case "base64":
					mime, _ := source["media_type"].(string)
					data, _ := source["data"].(string)
					imageURL = "data:" + mime + ";base64," + data
				case "url":
					imageURL, _ = source["url"].(string)
				default:
					return out, fmt.Errorf("unsupported image source")
				}
				content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}})
			case "tool_use":
				if item.Role != "assistant" {
					return out, fmt.Errorf("tool_use is only valid in assistant messages")
				}
				id, _ := part["id"].(string)
				name, _ := part["name"].(string)
				args, _ := json.Marshal(part["input"])
				if id == "" || name == "" {
					return out, fmt.Errorf("tool_use requires id and name")
				}
				message.ToolCalls = append(message.ToolCalls, ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: name, Arguments: argumentString(args)}})
			case "tool_result":
				if item.Role != "user" {
					return out, fmt.Errorf("tool_result is only valid in user messages")
				}
				flush()
				id, _ := part["tool_use_id"].(string)
				if id == "" {
					return out, fmt.Errorf("tool_result requires tool_use_id")
				}
				result := part["content"]
				if result == nil {
					result = ""
				}
				out.Messages = append(out.Messages, Message{Role: "tool", ToolCallID: id, Content: result})
			case "thinking", "redacted_thinking":
				if item.Role != "assistant" {
					return out, fmt.Errorf("thinking is only valid in assistant history")
				}
			default:
				return out, fmt.Errorf("unsupported Messages content block %q", part["type"])
			}
		}
		flush()
	}
	for _, tool := range r.Tools {
		if tool.Type != "" && tool.Type != "custom" {
			return out, fmt.Errorf("server-executed tools are not supported")
		}
		if tool.Name == "" {
			return out, fmt.Errorf("tool requires name")
		}
		out.Tools = append(out.Tools, Tool{Type: "function", Function: ToolFunction{Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema}})
	}
	switch r.ToolChoice.Type {
	case "", "auto":
		out.ToolChoice = "auto"
	case "any":
		out.ToolChoice = "required"
	case "none":
		out.ToolChoice = "none"
	case "tool":
		out.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": r.ToolChoice.Name}}
	default:
		return out, fmt.Errorf("unsupported tool_choice")
	}
	if r.ToolChoice.DisableParallel {
		parallel := false
		out.ParallelToolCalls = &parallel
	}
	return out, nil
}

func (s *Server) decodeInference(w http.ResponseWriter, r *http.Request, value any) bool {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, 405, "method_not_allowed", "POST required")
		return false
	}
	if !s.tokens.Allowed(r) {
		writeOpenAIError(w, 401, "invalid_api_key", "invalid proxy API key")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20))
	if err := dec.Decode(value); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeOpenAIError(w, 413, "request_too_large", "Request body exceeds the 32 MiB limit")
		} else {
			writeOpenAIError(w, 400, "invalid_request_error", "Invalid JSON request body")
		}
		return false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		writeOpenAIError(w, 400, "invalid_request_error", "Request body must contain one JSON object")
		return false
	}
	return true
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	var in responsesRequest
	if !s.decodeInference(w, r, &in) {
		return
	}
	if row := requestRecord(r); row != nil {
		row.Model = in.Model
		row.Stream = in.Stream
	}
	req, err := s.responsesChatRequest(in, tokenFromRequest(r))
	if err != nil {
		writeOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}
	if req.Stream {
		req.StreamOptions = &StreamOptions{IncludeUsage: true}
	}
	writer := newProtocolWriter(w, "responses", in.Stream)
	_, writer.toolRoutes, _ = collectResponseTools(in.Tools, "")
	s.executeChat(writer, r, req)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	writer := newProtocolWriter(w, "messages", false)
	var in messagesRequest
	if !s.decodeInference(writer, r, &in) {
		return
	}
	if row := requestRecord(r); row != nil {
		row.Model = in.Model
		row.Stream = in.Stream
	}
	req, err := in.chatRequest()
	if err != nil {
		writeOpenAIError(writer, 400, "invalid_request_error", err.Error())
		return
	}
	if req.Stream {
		req.StreamOptions = &StreamOptions{IncludeUsage: true}
	}
	writer.stream = in.Stream
	s.executeChat(writer, r, req)
}
