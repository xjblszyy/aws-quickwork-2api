package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFirstRunGeneratesDefaultAPIKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.APIKeys) != 1 || !strings.HasPrefix(cfg.APIKeys[0], "sk-qw-") || len(cfg.APIKeys[0]) != len("sk-qw-")+48 {
		t.Fatalf("unexpected default keys %v", cfg.APIKeys)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions %v %v", info, err)
	}
	reloaded, err := Load(path)
	if err != nil || len(reloaded.APIKeys) != 1 || reloaded.APIKeys[0] != cfg.APIKeys[0] {
		t.Fatalf("default key not persisted: %v %v", reloaded, err)
	}
}

func TestSetRoutingIsolatesCallerMaps(t *testing.T) {
	cfg := Default()
	cfg.ModelMappings = map[string]string{"startup": "smart"}
	if cfg.Routing().Mappings["startup"] != "smart" {
		t.Fatal("startup mappings not used before SetRouting")
	}
	mappings := map[string]string{"live": "fast"}
	cfg.SetRouting([]string{"smart", "fast"}, mappings, "fast")
	mappings["live"] = "auto"
	routing := cfg.Routing()
	if routing.Mappings["live"] != "fast" || routing.DefaultModel != "fast" || len(routing.Models) != 2 {
		t.Fatalf("unexpected routing %+v", routing)
	}
}

func TestExistingEmptyAPIKeysStayDisabled(t *testing.T) {
	cfg := Default()
	cfg.Path = filepath.Join(t.TempDir(), "config.json")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(cfg.Path)
	if err != nil || len(loaded.APIKeys) != 0 {
		t.Fatalf("existing config gained keys: %v %v", loaded.APIKeys, err)
	}
}

func TestEnvironmentAPIKeysAreNotSaved(t *testing.T) {
	cfg := Default()
	cfg.Path = filepath.Join(t.TempDir(), "config.json")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUICKPROXY_API_KEYS", "test-env-only-key")
	loaded, err := Load(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.APIKeys) != 1 || loaded.APIKeys[0] != "test-env-only-key" || !loaded.APIKeysFromEnv() {
		t.Fatal("environment key not enabled")
	}
	if len(loaded.FileValues().APIKeys) != 0 {
		t.Fatal("environment key would be written to config")
	}
	if err := loaded.Save(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(cfg.Path)
	if err != nil || !strings.Contains(string(body), `"api_keys": []`) || strings.Contains(string(body), "test-env-only-key") {
		t.Fatalf("unexpected saved config: %s %v", body, err)
	}
}
