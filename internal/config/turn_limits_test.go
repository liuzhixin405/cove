package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestTurnLimitDefaults(t *testing.T) {
	isolate(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxIterations != 200 || DefaultMaxIterations != 200 {
		t.Fatalf("MaxIterations = %d, want 200", cfg.MaxIterations)
	}
	if cfg.MaxTurnMinutes != 60 || DefaultMaxTurnMinutes != 60 {
		t.Fatalf("MaxTurnMinutes = %d, want 60", cfg.MaxTurnMinutes)
	}
	if cfg.SubagentMaxIterations != 60 || DefaultSubagentMaxIterations != 60 {
		t.Fatalf("SubagentMaxIterations = %d, want 60", cfg.SubagentMaxIterations)
	}
	d := DefaultConfig()
	if d.MaxIterations != 200 || d.MaxTurnMinutes != 60 || d.SubagentMaxIterations != 60 {
		t.Fatalf("DefaultConfig limits = %d/%d/%d", d.MaxIterations, d.MaxTurnMinutes, d.SubagentMaxIterations)
	}
}

func TestTurnLimitsFromFile(t *testing.T) {
	global, _ := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"),
		`{"max_iterations": 50, "max_turn_minutes": 0, "subagent_max_iterations": 15}`)
	cfg, _ := Load()
	if cfg.MaxIterations != 50 || cfg.SubagentMaxIterations != 15 {
		t.Fatalf("limits = %d/%d, want 50/15", cfg.MaxIterations, cfg.SubagentMaxIterations)
	}
	// 0 turns the time limit off; it must not be taken for "unset".
	if cfg.MaxTurnMinutes != 0 {
		t.Fatalf("MaxTurnMinutes = %d, want 0 (off)", cfg.MaxTurnMinutes)
	}
}

func TestTurnLimitsNonPositiveIterationsFallBack(t *testing.T) {
	global, _ := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"),
		`{"max_iterations": -3, "subagent_max_iterations": 0, "max_turn_minutes": -1}`)
	cfg, _ := Load()
	if cfg.MaxIterations != 200 || cfg.SubagentMaxIterations != 60 {
		t.Fatalf("limits = %d/%d, want the defaults 200/60", cfg.MaxIterations, cfg.SubagentMaxIterations)
	}
	if cfg.MaxTurnMinutes != 0 {
		t.Fatalf("MaxTurnMinutes = %d, want 0 (negative = off)", cfg.MaxTurnMinutes)
	}
}

func TestTurnLimitDefaultsNotSaved(t *testing.T) {
	global, _ := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"), `{"model": "m"}`)
	cfg, _ := Load()
	cfg.Model = "other"
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	body := readFile(t, filepath.Join(global, "config.json"))
	for _, key := range []string{"max_iterations", "max_turn_minutes", "subagent_max_iterations"} {
		if strings.Contains(body, key) {
			t.Fatalf("Save wrote the default %s:\n%s", key, body)
		}
	}
}

// -p and headless runs cannot answer the time-limit prompt, so there the
// limit applies only when the user wrote max_turn_minutes themselves.
func TestUnattendedTurnMinutesOnlyWhenExplicit(t *testing.T) {
	global, _ := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"), `{"model": "m"}`)
	cfg, _ := Load()
	if cfg.MaxTurnMinutes != 60 || cfg.UnattendedTurnMinutes() != 0 {
		t.Fatalf("no explicit key: interactive %d, unattended %d; want 60, 0", cfg.MaxTurnMinutes, cfg.UnattendedTurnMinutes())
	}
	writeFile(t, filepath.Join(global, "config.json"), `{"max_turn_minutes": 5}`)
	cfg, _ = Load()
	if cfg.MaxTurnMinutes != 5 || cfg.UnattendedTurnMinutes() != 5 {
		t.Fatalf("explicit 5: interactive %d, unattended %d", cfg.MaxTurnMinutes, cfg.UnattendedTurnMinutes())
	}
	if DefaultConfig().UnattendedTurnMinutes() != 0 {
		t.Fatal("DefaultConfig has no explicit max_turn_minutes")
	}
}
