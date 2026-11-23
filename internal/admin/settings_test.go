package admin

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"quickwork-openai/internal/accounts"
	"quickwork-openai/internal/config"
	"quickwork-openai/internal/store"
)

func TestModelRoutingAppliesWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	server, err := NewServer(cfg, st, accounts.New(st, cfg), false)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	save := func(edit func(*settings)) map[string]any {
		t.Helper()
		server.configMu.Lock()
		value := settingsFromConfig(server.saved)
		server.configMu.Unlock()
		edit(&value)
		body, _ := json.Marshal(map[string]any{"settings": value})
		w := httptest.NewRecorder()
		r := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		server.handleConfigSave(w, r)
		if w.Code != 200 {
			t.Fatalf("save %d %s", w.Code, w.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}

	out := save(func(value *settings) {
		value.ModelMappings = map[string]string{"claude-test": "fast"}
		value.DefaultModel = "balanced"
	})
	if out["restart_required"] != false {
		t.Fatalf("mapping-only change required a restart: %v", out)
	}
	routing := cfg.Routing()
	if routing.Mappings["claude-test"] != "fast" || routing.DefaultModel != "balanced" {
		t.Fatalf("routing not applied live: %+v", routing)
	}
	reloaded, err := config.Load(cfg.Path)
	if err != nil || reloaded.ModelMappings["claude-test"] != "fast" {
		t.Fatalf("mapping not persisted: %v %v", reloaded, err)
	}

	out = save(func(value *settings) { value.MaxAttempts = 5 })
	if out["restart_required"] != true {
		t.Fatalf("non-routing change did not require a restart: %v", out)
	}
}
