package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type wireUsage struct {
	Prompt     int `json:"prompt_tokens"`
	Completion int `json:"completion_tokens"`
	Total      int `json:"total_tokens"`
	Details    struct {
		Cached  int `json:"cached_tokens"`
		Created int `json:"cache_creation_tokens"`
	} `json:"prompt_tokens_details"`
}
type wireMessage struct {
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls"`
}
type chatPacket struct {
	ID      string     `json:"id"`
	Model   string     `json:"model"`
	Created int64      `json:"created"`
	Usage   *wireUsage `json:"usage"`
	Choices []struct {
		Message wireMessage `json:"message"`
		Delta   struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Finish *string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Type    string `json:"type"`
	} `json:"error"`
}

type outputCall struct {
	Call        ToolCall
	ItemID      string
	OutputIndex int
}

// protocolWriter translates the shared executor's JSON and SSE incrementally.
// It never buffers the entire upstream stream before returning text deltas.
type protocolWriter struct {
	w                    http.ResponseWriter
	protocol             string
	stream               bool
	status               int
	pending              string
	started, finished    bool
	id, model, messageID string
	created              int64
	sequence             int
	text                 strings.Builder
	textIndex            int
	textClosed           bool
	calls                []*outputCall
	callIndexes          map[int]*outputCall
	toolRoutes           map[string]responseToolRoute
	usage                wireUsage
}

func newProtocolWriter(w http.ResponseWriter, protocol string, stream bool) *protocolWriter {
	return &protocolWriter{w: w, protocol: protocol, stream: stream, status: 200, textIndex: -1, callIndexes: make(map[int]*outputCall)}
}
func (p *protocolWriter) Header() http.Header    { return p.w.Header() }
func (p *protocolWriter) WriteHeader(status int) { p.status = status }
func (p *protocolWriter) Flush() {
	if f, ok := p.w.(http.Flusher); ok {
		f.Flush()
	}
}
func (p *protocolWriter) Unwrap() http.ResponseWriter { return p.w }

func (p *protocolWriter) Write(data []byte) (int, error) {
	if p.finished {
		return len(data), nil
	}
	if p.stream && p.status < 400 && strings.Contains(p.Header().Get("Content-Type"), "text/event-stream") {
		p.pending += string(data)
		for {
			i := strings.Index(p.pending, "\n\n")
			if i < 0 {
				break
			}
			frame := p.pending[:i]
			p.pending = p.pending[i+2:]
			for _, line := range strings.Split(frame, "\n") {
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				body := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if body == "[DONE]" {
					p.finish()
					continue
				}
				var packet chatPacket
				if err := json.Unmarshal([]byte(body), &packet); err != nil {
					return 0, err
				}
				p.consume(packet)
			}
		}
		return len(data), nil
	}
	var packet chatPacket
	if err := json.Unmarshal(data, &packet); err != nil {
		return 0, err
	}
	if packet.Error != nil {
		if p.protocol == "messages" {
			kind := "api_error"
			if p.status == 401 {
				kind = "authentication_error"
			} else if p.status == 400 || p.status == 413 {
				kind = "invalid_request_error"
			} else if p.status == 429 {
				kind = "rate_limit_error"
			}
			p.json(map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": packet.Error.Message}})
		} else {
			p.json(map[string]any{"error": packet.Error})
		}
		p.finished = true
		return len(data), nil
	}
	p.init(packet)
	if packet.Usage != nil {
		p.usage = *packet.Usage
	}
	if len(packet.Choices) > 0 {
		message := packet.Choices[0].Message
		p.text.WriteString(message.Content)
		for _, call := range message.ToolCalls {
			p.calls = append(p.calls, &outputCall{Call: call, ItemID: "fc_" + uuid()})
		}
	}
	if p.protocol == "messages" {
		p.json(p.messageObject())
	} else {
		p.json(p.responseObject("completed"))
	}
	p.finished = true
	return len(data), nil
}

func (p *protocolWriter) json(body any) {
	p.Header().Set("Content-Type", "application/json; charset=utf-8")
	p.w.WriteHeader(p.status)
	_ = json.NewEncoder(p.w).Encode(body)
}
func (p *protocolWriter) event(kind string, body map[string]any) {
	body["type"] = kind
	if p.protocol == "responses" {
		body["sequence_number"] = p.sequence
		p.sequence++
	}
	data, _ := json.Marshal(body)
	_, _ = fmt.Fprintf(p.w, "event: %s\ndata: %s\n\n", kind, data)
	p.Flush()
}

func (p *protocolWriter) init(packet chatPacket) {
	if p.id != "" {
		return
	}
	p.id = "resp_" + uuid()
	if p.protocol == "messages" {
		p.id = "msg_" + uuid()
	}
	p.messageID = "msg_" + uuid()
	p.model = packet.Model
	p.created = packet.Created
}
func (p *protocolWriter) start(packet chatPacket) {
	if p.started {
		return
	}
	p.init(packet)
	p.started = true
	p.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	p.w.WriteHeader(200)
	if p.protocol == "messages" {
		p.event("message_start", map[string]any{"message": map[string]any{"id": p.id, "type": "message", "role": "assistant", "model": p.model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0}}})
	} else {
		p.event("response.created", map[string]any{"response": p.responseObject("in_progress")})
		p.event("response.in_progress", map[string]any{"response": p.responseObject("in_progress")})
	}
}

func (p *protocolWriter) consume(packet chatPacket) {
	p.start(packet)
	if packet.Error != nil {
		if p.protocol == "messages" {
			p.event("error", map[string]any{"error": map[string]any{"type": "api_error", "message": packet.Error.Message}})
		} else {
			response := p.responseObject("failed")
			response["error"] = packet.Error
			p.event("response.failed", map[string]any{"response": response})
		}
		p.finished = true
		return
	}
	if packet.Usage != nil {
		p.usage = *packet.Usage
	}
	for _, choice := range packet.Choices {
		if text := choice.Delta.Content; text != "" {
			if p.textIndex < 0 {
				p.textIndex = len(p.calls)
				if p.protocol == "messages" {
					p.event("content_block_start", map[string]any{"index": p.textIndex, "content_block": map[string]any{"type": "text", "text": ""}})
				} else {
					item := p.textItem("in_progress", "")
					item["content"] = []any{}
					p.event("response.output_item.added", map[string]any{"output_index": p.textIndex, "item": item})
					p.event("response.content_part.added", map[string]any{"item_id": p.messageID, "output_index": p.textIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
				}
			}
			p.text.WriteString(text)
			if p.protocol == "messages" {
				p.event("content_block_delta", map[string]any{"index": p.textIndex, "delta": map[string]any{"type": "text_delta", "text": text}})
			} else {
				p.event("response.output_text.delta", map[string]any{"item_id": p.messageID, "output_index": p.textIndex, "content_index": 0, "delta": text})
			}
		}
		for _, delta := range choice.Delta.ToolCalls {
			p.closeText()
			call := p.callIndexes[delta.Index]
			if call == nil {
				index := len(p.calls)
				if p.textIndex >= 0 {
					index++
				}
				call = &outputCall{Call: ToolCall{ID: delta.ID, Type: "function", Function: FunctionCall{Name: delta.Function.Name}}, ItemID: "fc_" + uuid(), OutputIndex: index}
				p.calls = append(p.calls, call)
				p.callIndexes[delta.Index] = call
				if p.protocol == "messages" {
					p.event("content_block_start", map[string]any{"index": index, "content_block": map[string]any{"type": "tool_use", "id": call.Call.ID, "name": call.Call.Function.Name, "input": map[string]any{}}})
				} else {
					p.event("response.output_item.added", map[string]any{"output_index": index, "item": p.callItem(call, "in_progress")})
				}
			}
			call.Call.Function.Arguments += delta.Function.Arguments
			if delta.Function.Arguments != "" {
				if p.protocol == "messages" {
					p.event("content_block_delta", map[string]any{"index": call.OutputIndex, "delta": map[string]any{"type": "input_json_delta", "partial_json": delta.Function.Arguments}})
				} else if !p.isCustom(call) {
					p.event("response.function_call_arguments.delta", map[string]any{"item_id": call.ItemID, "output_index": call.OutputIndex, "delta": delta.Function.Arguments})
				}
			}
		}
	}
}

func (p *protocolWriter) closeText() {
	if p.textIndex < 0 || p.textClosed {
		return
	}
	p.textClosed = true
	if p.protocol == "messages" {
		p.event("content_block_stop", map[string]any{"index": p.textIndex})
	} else {
		p.event("response.output_text.done", map[string]any{"item_id": p.messageID, "output_index": p.textIndex, "content_index": 0, "text": p.text.String()})
		p.event("response.content_part.done", map[string]any{"item_id": p.messageID, "output_index": p.textIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": p.text.String(), "annotations": []any{}}})
		p.event("response.output_item.done", map[string]any{"output_index": p.textIndex, "item": p.textItem("completed", p.text.String())})
	}
}

func (p *protocolWriter) finish() {
	if p.finished {
		return
	}
	p.finished = true
	p.closeText()
	for _, call := range p.calls {
		if p.protocol == "messages" {
			p.event("content_block_stop", map[string]any{"index": call.OutputIndex})
		} else {
			if p.isCustom(call) {
				input := p.customInput(call)
				p.event("response.custom_tool_call_input.delta", map[string]any{"item_id": call.ItemID, "output_index": call.OutputIndex, "delta": input})
				p.event("response.custom_tool_call_input.done", map[string]any{"item_id": call.ItemID, "output_index": call.OutputIndex, "input": input})
			} else {
				p.event("response.function_call_arguments.done", map[string]any{"item_id": call.ItemID, "output_index": call.OutputIndex, "arguments": call.Call.Function.Arguments})
			}
			p.event("response.output_item.done", map[string]any{"output_index": call.OutputIndex, "item": p.callItem(call, "completed")})
		}
	}
	if p.protocol == "messages" {
		stop := "end_turn"
		if len(p.calls) > 0 {
			stop = "tool_use"
		}
		p.event("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": p.messageUsage()})
		p.event("message_stop", map[string]any{})
	} else {
		p.event("response.completed", map[string]any{"response": p.responseObject("completed")})
	}
}

func (p *protocolWriter) textItem(status, text string) map[string]any {
	return map[string]any{"id": p.messageID, "type": "message", "status": status, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
}
func (p *protocolWriter) callItem(call *outputCall, status string) map[string]any {
	item := map[string]any{"id": call.ItemID, "type": "function_call", "status": status, "call_id": call.Call.ID, "name": call.Call.Function.Name, "arguments": call.Call.Function.Arguments}
	if route, ok := p.toolRoutes[call.Call.Function.Name]; ok {
		item["name"] = route.Name
		if route.Namespace != "" {
			item["namespace"] = route.Namespace
		}
		if route.Custom {
			item["type"] = "custom_tool_call"
			delete(item, "arguments")
			item["input"] = p.customInput(call)
		}
	}
	return item
}

func (p *protocolWriter) responseObject(status string) map[string]any {
	output := []any{}
	if p.text.Len() > 0 {
		output = append(output, p.textItem(status, p.text.String()))
	}
	for _, call := range p.calls {
		output = append(output, p.callItem(call, status))
	}
	return map[string]any{"id": p.id, "object": "response", "created_at": p.created, "status": status, "model": p.model, "output": output, "error": nil, "incomplete_details": nil, "parallel_tool_calls": true, "tools": []any{}, "tool_choice": "auto", "store": false, "usage": map[string]any{"input_tokens": p.usage.Prompt, "output_tokens": p.usage.Completion, "total_tokens": p.usage.Total, "input_tokens_details": map[string]int{"cached_tokens": p.usage.Details.Cached}, "output_tokens_details": map[string]int{"reasoning_tokens": 0}}}
}
func (p *protocolWriter) messageUsage() map[string]int {
	return map[string]int{"input_tokens": max(p.usage.Prompt-p.usage.Details.Cached-p.usage.Details.Created, 0), "output_tokens": p.usage.Completion, "cache_read_input_tokens": p.usage.Details.Cached, "cache_creation_input_tokens": p.usage.Details.Created}
}
func (p *protocolWriter) messageObject() map[string]any {
	content := []any{}
	if p.text.Len() > 0 {
		content = append(content, map[string]any{"type": "text", "text": p.text.String()})
	}
	for _, call := range p.calls {
		var args any
		if json.Unmarshal([]byte(call.Call.Function.Arguments), &args) != nil {
			args = map[string]any{}
		}
		content = append(content, map[string]any{"type": "tool_use", "id": call.Call.ID, "name": call.Call.Function.Name, "input": args})
	}
	stop := "end_turn"
	if len(p.calls) > 0 {
		stop = "tool_use"
	}
	return map[string]any{"id": p.id, "type": "message", "role": "assistant", "model": p.model, "content": content, "stop_reason": stop, "stop_sequence": nil, "usage": p.messageUsage()}
}
