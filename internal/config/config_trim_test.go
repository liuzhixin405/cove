package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadTrimsWhitespaceFromPersistedProviderSettings(t *testing.T) {
	home := t.TempDir()
	oldHome := os.Getenv("HOME")
	oldUserProfile := os.Getenv("USERPROFILE")
	t.Cleanup(func() {
		_ = os.Setenv("HOME", oldHome)
		_ = os.Setenv("USERPROFILE", oldUserProfile)
	})
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("set HOME: %v", err)
	}
	if err := os.Setenv("USERPROFILE", home); err != nil {
		t.Fatalf("set USERPROFILE: %v", err)
	}

	dir := filepath.Join(home, ".cove")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	raw := map[string]any{
		"model": "  deepseek-v4-pro  ",
		"provider": map[string]any{
			"name":     "  deepseek  ",
			"api_key":  "  sk-test  ",
			"base_url": "  https://api.deepseek.com  ",
		},
		"permission_mode": "default",
		"max_budget_usd":  10,
		"thinking_tokens": 16000,
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Model, "deepseek-v4-pro"; got != want {
		t.Fatalf("cfg.Model = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.Name, "deepseek"; got != want {
		t.Fatalf("cfg.Provider.Name = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.APIKey, "sk-test"; got != want {
		t.Fatalf("cfg.Provider.APIKey = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.BaseURL, "https://api.deepseek.com"; got != want {
		t.Fatalf("cfg.Provider.BaseURL = %q, want %q", got, want)
	}
}

func TestLoadResolvesAutoModelForDeepSeekProvider(t *testing.T) {
	home := t.TempDir()
	oldHome := os.Getenv("HOME")
	oldUserProfile := os.Getenv("USERPROFILE")
	t.Cleanup(func() {
		_ = os.Setenv("HOME", oldHome)
		_ = os.Setenv("USERPROFILE", oldUserProfile)
	})
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("set HOME: %v", err)
	}
	if err := os.Setenv("USERPROFILE", home); err != nil {
		t.Fatalf("set USERPROFILE: %v", err)
	}

	dir := filepath.Join(home, ".cove")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	raw := map[string]any{
		"model": "auto",
		"provider": map[string]any{
			"name": "deepseek",
		},
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Model, "deepseek-v4-pro"; got != want {
		t.Fatalf("cfg.Model = %q, want %q", got, want)
	}
}

func TestLoadReturnsErrorForInvalidConfigJSON(t *testing.T) {
	home := t.TempDir()
	oldHome := os.Getenv("HOME")
	oldUserProfile := os.Getenv("USERPROFILE")
	t.Cleanup(func() {
		_ = os.Setenv("HOME", oldHome)
		_ = os.Setenv("USERPROFILE", oldUserProfile)
	})
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("set HOME: %v", err)
	}
	if err := os.Setenv("USERPROFILE", home); err != nil {
		t.Fatalf("set USERPROFILE: %v", err)
	}

	dir := filepath.Join(home, ".cove")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"model":`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err == nil {
		t.Fatal("expected invalid JSON error")
	}
	if cfg == nil || cfg.Model == "" {
		t.Fatalf("expected defaulted config with error, got %#v", cfg)
	}
}
