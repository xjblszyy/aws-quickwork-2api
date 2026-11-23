package config

import (
	"path/filepath"
	"testing"
)

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
}
