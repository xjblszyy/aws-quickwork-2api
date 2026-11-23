package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"quickwork-openai/internal/config"
)

func TestModelAliases(t *testing.T) {
	cfg := config.Default()
	cfg.ModelMappings = map[string]string{"opus5.5": "smart", "claude-test": "fast"}
	s := NewServer(&cfg, nil)
	for model, want := range map[string]string{"opus5.5": "smart", "claude-test": "fast", "balanced": "balanced"} {
		if got := s.mapModel(model); got != want {
			t.Fatalf("%s: got %s, want %s", model, got, want)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, model := range body.Data {
		if model.ID == "opus5.5" {
			found = true
		}
	}
	if !found {
		t.Fatal("mapped model missing from model list")
	}
	cfg.ModelMappings["bad"] = "not-an-upstream-model"
	if cfg.Validate() == nil {
		t.Fatal("invalid mapping accepted")
	}
}

func TestInferenceAuthentication(t *testing.T) {
	auth := NewAPIKeyAuth([]string{"test-only-key"})
	for _, header := range []string{"Authorization", "X-Api-Key"} {
		r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		if auth.Allowed(r) {
			t.Fatal("missing key accepted")
		}
		key := "test-only-key"
		if header == "Authorization" {
			key = "Bearer " + key
		}
		r.Header.Set(header, key)
		if !auth.Allowed(r) {
			t.Fatalf("%s rejected", header)
		}
		r.Header.Set(header, "wrong")
		if auth.Allowed(r) {
			t.Fatal("wrong key accepted")
		}
	}
}
