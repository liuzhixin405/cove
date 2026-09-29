package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/config"
)

// The window the E2008 remedy learns is written to config.json under the
// model's name and applied at the next start, before any model call.
func TestPersistedModelContextWindowIsAppliedAtStartup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfgDir := filepath.Join(home, ".cove")
	t.Setenv("COVE_CONFIG_DIR", cfgDir)
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"model":"qwen3.6-27b","provider":{"name":"openai-compatible","api_key":"local","base_url":"http://127.0.0.1:8080/v1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	api.ClearModelContextWindows()
	t.Cleanup(api.ClearModelContextWindows)

	if err := (diagRuntime{}).PersistModelContextWindow("qwen3.6-27b", 16384); err != nil {
		t.Fatalf("persist: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelContextWindows["qwen3.6-27b"] != 16384 {
		t.Fatalf("config.json lacks the learned window: %+v", cfg.ModelContextWindows)
	}
	if cfg.Model != "qwen3.6-27b" {
		t.Fatalf("Save changed unrelated keys: model = %q", cfg.Model)
	}

	api.ClearModelContextWindows()
	installDiagnostics(cfg, true)
	if got := api.ContextWindowForModel("qwen3.6-27b"); got != 16384 {
		t.Fatalf("window at startup = %d, want the persisted 16384", got)
	}
	if !api.ContextWindowKnown("qwen3.6-27b") {
		t.Fatalf("persisted window not counted as known")
	}
}
