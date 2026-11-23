package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesConversion(t *testing.T) {
	var in responsesRequest
	if err := json.Unmarshal([]byte(`{"model":"opus5.5","instructions":"rules","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,YQ=="}]},{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","output":"result"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`), &in); err != nil {
		t.Fatal(err)
	}
	got, err := in.chatRequest()
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "opus5.5" || len(got.Messages) != 4 || got.Messages[0].Role != "system" || got.Messages[3].ToolCallID != "call-1" || got.Tools[0].Function.Name != "lookup" {
		t.Fatalf("%+v", got)
	}
	in.PreviousResponseID = "resp-other"
	if _, err = in.chatRequest(); err == nil {
		t.Fatal("unsupported stateful continuation silently accepted")
	}
}

func TestMessagesConversion(t *testing.T) {
	var in messagesRequest
	err := json.Unmarshal([]byte(`{"model":"opus5.5","max_tokens":128,"system":[{"type":"text","text":"rules"}],"messages":[{"role":"user","content":"question"},{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Task","input":{"prompt":"child"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"child result"}]}],"tools":[{"name":"Task","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"Task"}}`), &in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := in.chatRequest()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 4 || got.Messages[2].ToolCalls[0].Function.Name != "Task" || got.Messages[3].ToolCallID != "tool-1" || got.Messages[3].Content != "child result" {
		t.Fatalf("%+v", got)
	}
	if mode, name := parseToolChoice(got.ToolChoice); mode != "function" || name != "Task" {
		t.Fatalf("choice %v", got.ToolChoice)
	}
}

func TestResponsesCustomTools(t *testing.T) {
	var in responsesRequest
	if err := json.Unmarshal([]byte(`{"model":"opus5.5","input":[{"role":"user","content":"patch"},{"type":"custom_tool_call","call_id":"call-c","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"},{"type":"custom_tool_call_output","call_id":"call-c","output":"done"}],"tools":[{"type":"custom","name":"apply_patch","description":"apply a patch"}]}`), &in); err != nil {
		t.Fatal(err)
	}
	got, err := in.chatRequest()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "apply_patch" || !strings.Contains(got.Messages[1].ToolCalls[0].Function.Arguments, "Begin Patch") {
		t.Fatalf("%+v", got)
	}
}

func TestMessagesAcceptClientSystemReminders(t *testing.T) {
	var in messagesRequest
	if err := json.Unmarshal([]byte(`{"model":"opus5.5","max_tokens":128,"messages":[{"role":"user","content":"delegate"},{"role":"system","content":[{"type":"text","text":"client reminder"}]}]}`), &in); err != nil {
		t.Fatal(err)
	}
	got, err := in.chatRequest()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 || got.Messages[1].Role != "system" {
		t.Fatalf("%+v", got)
	}
}
