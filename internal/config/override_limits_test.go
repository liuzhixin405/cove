package config

import (
	"path/filepath"
	"testing"
)

// Final fix (Important 3): the per-turn limits and max_sessions work from a
// project's .cove.json and from a profile, not only from config.json.
func TestProjectOverrideSetsMaxIterations(t *testing.T) {
	global, project := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"), `{"max_iterations": 80}`)
	writeFile(t, filepath.Join(project, ".cove.json"),
		`{"max_iterations": 30, "subagent_max_iterations": 12, "max_sessions": -1}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxIterations != 30 || cfg.SubagentMaxIterations != 12 || cfg.MaxSessions != -1 {
		t.Fatalf("limits = %d/%d/%d, want 30/12/-1 from .cove.json",
			cfg.MaxIterations, cfg.SubagentMaxIterations, cfg.MaxSessions)
	}
}

func TestProjectMaxTurnMinutesCountsAsExplicit(t *testing.T) {
	_, project := isolate(t)
	writeFile(t, filepath.Join(project, ".cove.json"), `{"max_turn_minutes": 5}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxTurnMinutes != 5 || cfg.UnattendedTurnMinutes() != 5 {
		t.Fatalf("interactive %d, unattended %d; want 5, 5", cfg.MaxTurnMinutes, cfg.UnattendedTurnMinutes())
	}
}

// A .cove.json without the key leaves an explicit global value alone.
func TestProjectOverrideWithoutTurnMinutesKeepsGlobal(t *testing.T) {
	global, project := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"), `{"max_turn_minutes": 7}`)
	writeFile(t, filepath.Join(project, ".cove.json"), `{"model": "m"}`)
	cfg, _ := Load()
	if cfg.MaxTurnMinutes != 7 || cfg.UnattendedTurnMinutes() != 7 {
		t.Fatalf("interactive %d, unattended %d; want 7, 7", cfg.MaxTurnMinutes, cfg.UnattendedTurnMinutes())
	}
}

func TestProfileOverridesMaxSessionsAndLimits(t *testing.T) {
	global, _ := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"), `{
		"max_sessions": 50,
		"active_profile": "w",
		"profiles": {"w": {"max_sessions": 10, "max_iterations": 40, "subagent_max_iterations": 9, "max_turn_minutes": 3}}
	}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxSessions != 10 || cfg.MaxIterations != 40 || cfg.SubagentMaxIterations != 9 {
		t.Fatalf("profile limits = %d/%d/%d, want 10/40/9", cfg.MaxSessions, cfg.MaxIterations, cfg.SubagentMaxIterations)
	}
	if cfg.MaxTurnMinutes != 3 || cfg.UnattendedTurnMinutes() != 3 {
		t.Fatalf("profile max_turn_minutes: interactive %d, unattended %d; want 3, 3", cfg.MaxTurnMinutes, cfg.UnattendedTurnMinutes())
	}
}
