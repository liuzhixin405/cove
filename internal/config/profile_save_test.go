package config

import (
	"path/filepath"
	"testing"
)

// rawView rebuilt each profile by hand from a list of fields that stopped at
// system_prompt, and Save then replaced the whole "profiles" key: saving any
// profile ("/profile save home") wiped max_turn_minutes: 0, max_iterations ...
// and every key this version does not know from all the other profiles.
func TestSaveKeepsEveryProfileField(t *testing.T) {
	global, _ := isolate(t)
	globalPath := filepath.Join(global, "config.json")
	writeFile(t, globalPath, `{"model":"m","profiles":{"work":{"model":"w","max_iterations":7,"max_turn_minutes":0,"subagent_max_iterations":3,"max_sessions":9,"future":{"x":1}}}}`)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Profiles["home"] = &Profile{Model: "h"}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}

	m := readJSONMap(t, globalPath)
	work, _ := m["profiles"].(map[string]any)["work"].(map[string]any)
	for k, want := range map[string]any{"model": "w", "max_iterations": 7.0, "max_turn_minutes": 0.0,
		"subagent_max_iterations": 3.0, "max_sessions": 9.0} {
		if work[k] != want {
			t.Errorf("profile work %s = %v, want %v (all: %v)", k, work[k], want, work)
		}
	}
	if _, ok := work["future"]; !ok {
		t.Errorf("unknown profile key dropped: %v", work)
	}
	if _, ok := m["profiles"].(map[string]any)["home"]; !ok {
		t.Error("new profile not saved")
	}
}

// With an active profile that sets model and provider, applyProfile runs after
// config.json on every start. /model and /api-key saved to the top level, so
// the next start applied the profile over them and the change was silently
// lost. The change must land in the profile that supplied the value.
func TestSaveWithActiveProfilePersistsModelAndAPIKey(t *testing.T) {
	global, _ := isolate(t)
	globalPath := filepath.Join(global, "config.json")
	writeFile(t, globalPath, `{"model":"base","provider":{"name":"anthropic","api_key":"sk-base"},"active_profile":"work",
		"profiles":{"work":{"model":"w","provider":{"name":"deepseek","api_key":"sk-work"}}}}`)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model = "w2"
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Provider.APIKey = "sk-new"
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}

	again, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if again.Model != "w2" {
		t.Fatalf("model = %q after /model w2, want w2", again.Model)
	}
	if again.Provider.APIKey != "sk-new" || again.Provider.Name != "deepseek" {
		t.Fatalf("provider = %+v after /api-key, want deepseek/sk-new", again.Provider)
	}
	// The base settings are not touched: removing the profile restores them.
	m := readJSONMap(t, globalPath)
	if m["model"] != "base" {
		t.Errorf("top-level model = %v, want base", m["model"])
	}
	if prov, _ := m["provider"].(map[string]any); prov["api_key"] != "sk-base" || prov["name"] != "anthropic" {
		t.Errorf("top-level provider = %v, want untouched", prov)
	}

	// A field the profile does not set still goes to the top level.
	again.MaxBudgetUsd = 42
	if err := Save(again); err != nil {
		t.Fatal(err)
	}
	if m := readJSONMap(t, globalPath); m["max_budget_usd"] != 42.0 {
		t.Fatalf("max_budget_usd = %v, want 42 at the top level", m["max_budget_usd"])
	}
}

// --profile selects a profile for one run without making it active; a /model
// in that run belongs to that profile as well.
func TestSaveWithFlagProfilePersistsIntoThatProfile(t *testing.T) {
	global, _ := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"), `{"model":"base","profiles":{"work":{"model":"w"}}}`)
	cfg, err := LoadWithProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model = "w3"
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	again, err := LoadWithProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if again.Model != "w3" {
		t.Fatalf("model = %q, want w3", again.Model)
	}
	base, _ := Load()
	if base.Model != "base" {
		t.Fatalf("base model = %q, want base", base.Model)
	}
}

// Clearing a profile-owned field to a value the profile cannot hold (budget
// 0) is kept: the profile blob written used to be the one rendered before
// the change, so the next start applied the old budget again.
func TestSaveClearingProfileOwnedFieldSticks(t *testing.T) {
	global, _ := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"),
		`{"max_budget_usd":10,"active_profile":"p","profiles":{"p":{"max_budget_usd":5}}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxBudgetUsd != 5 {
		t.Fatalf("loaded budget = %v, want the profile's 5", cfg.MaxBudgetUsd)
	}
	cfg.MaxBudgetUsd = 0
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	again, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if again.MaxBudgetUsd != 0 {
		t.Fatalf("budget after clearing = %v, want 0", again.MaxBudgetUsd)
	}
}

// Deleting the active profile keeps what it set in effect and writes it at
// the top level. The values used to look unchanged against the loaded view,
// so neither the delete nor a later /model with the same value was written
// and the next start fell back to the old top-level model.
func TestDeletingTheActiveProfileWritesItsValuesToTheTopLevel(t *testing.T) {
	global, _ := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"),
		`{"model":"base","max_budget_usd":10,"active_profile":"p","profiles":{"p":{"model":"prof","max_budget_usd":5}}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "prof" || cfg.MaxBudgetUsd != 5 {
		t.Fatalf("loaded model=%q budget=%v, want the profile's", cfg.Model, cfg.MaxBudgetUsd)
	}
	delete(cfg.Profiles, "p")
	cfg.ActiveProfile = ""
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	again, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if again.Model != "prof" || again.MaxBudgetUsd != 5 || again.ActiveProfile != "" || len(again.Profiles) != 0 {
		t.Fatalf("after deleting the active profile: model=%q budget=%v active=%q profiles=%d, want prof/5/\"\"/0",
			again.Model, again.MaxBudgetUsd, again.ActiveProfile, len(again.Profiles))
	}
	// A later change on the same session is an ordinary top-level write.
	cfg.Model = "next"
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	if again, _ = Load(); again.Model != "next" {
		t.Fatalf("model after a later change = %q, want next", again.Model)
	}
}
