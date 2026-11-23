package proxy

import (
	"net/http/httptest"
	"testing"
)

func TestConversationIdentityScopesExplicitIDs(t *testing.T) {
	req := &ChatRequest{Model: "smart", Messages: []Message{{Role: "user", Content: "same first prompt"}}}
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("X-Conversation-Id", "same-child-id")
	r.Header.Set("X-Api-Key", "tenant-a")
	a := conversationKey(r, req, "rules")
	r.Header.Set("X-Api-Key", "tenant-b")
	b := conversationKey(r, req, "rules")
	if a == b {
		t.Fatal("different API keys shared an explicitly named conversation")
	}
	r.Header.Set("X-Api-Key", "tenant-a")
	req.Model = "fast"
	if a == conversationKey(r, req, "rules") {
		t.Fatal("different models shared a conversation")
	}
}
