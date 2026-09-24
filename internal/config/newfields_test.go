package config

import (
	"os"
	"path/filepath"
	"testing"
)

func loadIn(t *testing.T, projectJSON string) *Config {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	project := filepath.Join(tmp, "project")
	if err := os.MkdirAll(project, 0755); err != nil {
		t.Fatal(err)
	}
	if projectJSON != "" {
		if err := os.WriteFile(filepath.Join(project, ".cove.json"), []byte(projectJSON), 0644); err != nil {
			t.Fatal(err)
		}
	}
	oldWD, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// A project's .cove.json must be able to set the new options; the override
// merge silently dropped fields it did not list, which is how earlier options
// ended up having no effect.
func TestProjectOverrideSetsThinkingEffortAndVerifyAuto(t *testing.T) {
	cfg := loadIn(t, `{"thinking":"adaptive","effort":"high","done_verify_auto":false}`)
	if cfg.Thinking != "adaptive" || cfg.Effort != "high" {
		t.Fatalf("thinking=%q effort=%q", cfg.Thinking, cfg.Effort)
	}
	if cfg.VerifyAutoEnabled() {
		t.Fatal("done_verify_auto:false was ignored")
	}
}

func TestProjectOverrideSetsShowReasoning(t *testing.T) {
	if !loadIn(t, `{"show_reasoning":true}`).ShowReasoning {
		t.Fatal("show_reasoning:true was ignored")
	}
	if loadIn(t, "").ShowReasoning {
		t.Fatal("reasoning should be folded by default")
	}
}

func TestProjectOverrideSetsDisabledSkills(t *testing.T) {
	cfg := loadIn(t, `{"disabled_skills":["spike","plan"]}`)
	if len(cfg.DisabledSkills) != 2 || cfg.DisabledSkills[0] != "spike" || cfg.DisabledSkills[1] != "plan" {
		t.Fatalf("disabled_skills = %v", cfg.DisabledSkills)
	}
}

func TestVerifyAutoIsOnByDefault(t *testing.T) {
	if !loadIn(t, "").VerifyAutoEnabled() {
		t.Fatal("automatic completion verification should default to on")
	}
}
