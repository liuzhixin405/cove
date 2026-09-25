package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// Sessions are pruned automatically (session.Store.AutoPrune); max_sessions
// is how many are kept, 200 unless configured.
func TestMaxSessionsDefaultsTo200(t *testing.T) {
	isolate(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxSessions != DefaultMaxSessions || DefaultMaxSessions != 200 {
		t.Fatalf("MaxSessions = %d (default const %d), want 200", cfg.MaxSessions, DefaultMaxSessions)
	}
	if DefaultConfig().MaxSessions != 200 {
		t.Fatalf("DefaultConfig().MaxSessions = %d", DefaultConfig().MaxSessions)
	}
}

func TestMaxSessionsFromFileAndNegativeDisables(t *testing.T) {
	global, _ := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"), `{"max_sessions": 50}`)
	cfg, _ := Load()
	if cfg.MaxSessions != 50 {
		t.Fatalf("MaxSessions = %d, want 50", cfg.MaxSessions)
	}
	writeFile(t, filepath.Join(global, "config.json"), `{"max_sessions": -1}`)
	cfg, _ = Load()
	if cfg.MaxSessions != -1 {
		t.Fatalf("MaxSessions = %d, want -1 (pruning off)", cfg.MaxSessions)
	}
}

// The default is not copied into config.json by an unrelated Save.
func TestMaxSessionsDefaultNotSaved(t *testing.T) {
	global, _ := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"), `{"model": "m"}`)
	cfg, _ := Load()
	cfg.Model = "other"
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	if body := readFile(t, filepath.Join(global, "config.json")); strings.Contains(body, "max_sessions") {
		t.Fatalf("Save wrote the default max_sessions:\n%s", body)
	}
}
