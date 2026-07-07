package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateDoesNotRewriteConfigWhenNoMigrationNeeded(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("COVE_CONFIG_DIR", tmpDir)

	configPath := filepath.Join(tmpDir, "config.json")
	initial := []byte(`{"model":"claude-sonnet-4-20250514","permission_mode":"default","max_budget_usd":10,"thinking_tokens":16000}`)
	if err := os.WriteFile(configPath, initial, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	if err := Migrate(cfg, 0); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}

	if string(before) != string(after) {
		t.Fatalf("expected config to remain unchanged, before=%q after=%q", string(before), string(after))
	}
}
