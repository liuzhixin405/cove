package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSavePreservesAPIKeys(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cove-config-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Use env var to redirect ConfigDir
	os.Setenv("COVE_CONFIG_DIR", tmpDir)
	defer os.Unsetenv("COVE_CONFIG_DIR")

	cfg := DefaultConfig()
	cfg.Model = "deepseek-v4-pro"
	cfg.Provider.Name = "deepseek"
	cfg.Provider.APIKey = "sk-234...cdef"
	cfg.Provider.APIKeys = []string{"sk-aaa...aaaa", "sk-bbb...bbbb"}
	cfg.Provider.BaseURL = "https://api.deepseek.com/v1"
	cfg.MaxBudgetUsd = 5.0
	cfg.ThinkingTokens = 1024
	cfg.Debug = true
	cfg.SystemPrompt = "You are a helpful assistant."

	// Save
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}

	// Read raw file and verify no masked keys
	raw, err := os.ReadFile(filepath.Join(tmpDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	rawStr := string(raw)

	if contains(rawStr, "****") {
		t.Errorf("raw config.json contains masked key!\n%s", rawStr)
	}
	if !contains(rawStr, "sk-234...cdef") {
		t.Errorf("full API key not found in raw config!\n%s", rawStr)
	}

	// Load and verify
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Provider.APIKey != "sk-234...cdef" {
		t.Errorf("APIKey corrupted after roundtrip: got %q", loaded.Provider.APIKey)
	}
	if len(loaded.Provider.APIKeys) != 2 || loaded.Provider.APIKeys[0] != "sk-aaa...aaaa" {
		t.Errorf("APIKeys corrupted: %v", loaded.Provider.APIKeys)
	}
	if loaded.MaxBudgetUsd != 5.0 {
		t.Errorf("MaxBudgetUsd corrupted: %f", loaded.MaxBudgetUsd)
	}
	if !loaded.Debug {
		t.Error("Debug flag lost")
	}

	// Verify display masking still works
	display, _ := json.Marshal(loaded.Provider)
	dispStr := string(display)
	if !contains(dispStr, "****") {
		t.Errorf("display should mask key, got: %s", dispStr)
	}
	if contains(dispStr, "sk-234...cdef") {
		t.Errorf("display leaked full key: %s", dispStr)
	}
}

func TestDisplayMasking(t *testing.T) {
	pc := ProviderConfig{
		Name:   "deepseek",
		APIKey: "sk-123...cdef",
	}
	data, err := json.Marshal(pc)
	if err != nil {
		t.Fatal(err)
	}
	disp := string(data)
	if !contains(disp, "****") {
		t.Errorf("display should contain masked portion, got: %s", disp)
	}
	if contains(disp, "sk-123...cdef") {
		t.Errorf("display leaked full key: %s", disp)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && len(substr) > 0 && findSubstr(s, substr)
}

func findSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
