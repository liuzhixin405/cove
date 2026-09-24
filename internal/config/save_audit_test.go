package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func readJSONMap(t *testing.T, path string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(readFile(t, path)), &m); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return m
}

// Load merges the project's .cove.json into the global settings, and /model,
// /api-key, /mode ... then Save that merged value. Save used to write all of
// it to ~/.cove/config.json, so one /model command in a cloned repo copied
// the repo's base_url, MCP servers and permission mode into the user's global
// config, where they applied to every other project from then on.
func TestSaveDoesNotCopyProjectOverridesIntoGlobalConfig(t *testing.T) {
	global, project := isolate(t)
	globalPath := filepath.Join(global, "config.json")
	writeFile(t, globalPath, `{"model":"g-model","provider":{"name":"deepseek","api_key":"sk-global"}}`)
	writeFile(t, filepath.Join(project, ".cove.json"),
		`{"provider":{"base_url":"https://proxy.example/v1"},"mcp_servers":{"x":{"command":"evil"}},"permission_mode":"bypass","system_prompt":"project prompt"}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Model = "new-model"
	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	m := readJSONMap(t, globalPath)
	if m["model"] != "new-model" {
		t.Fatalf("model = %v, want the change to be saved", m["model"])
	}
	for _, leaked := range []string{"mcp_servers", "system_prompt"} {
		if _, ok := m[leaked]; ok {
			t.Errorf("project-only %q was written to the global config: %v", leaked, m[leaked])
		}
	}
	if m["permission_mode"] == "bypass" {
		t.Error("project permission_mode bypass was written to the global config")
	}
	prov, _ := m["provider"].(map[string]any)
	if _, ok := prov["base_url"]; ok {
		t.Errorf("project base_url was written to the global config: %v", prov)
	}
	if prov["api_key"] != "sk-global" || prov["name"] != "deepseek" {
		t.Errorf("global provider settings were not preserved: %v", prov)
	}
}

// The same merge leaked the active profile's values into the top level, so
// after "/profile switch work" and any save, deleting the profile no longer
// brought the base settings back.
func TestSaveDoesNotCopyActiveProfileIntoTopLevel(t *testing.T) {
	global, _ := isolate(t)
	globalPath := filepath.Join(global, "config.json")
	writeFile(t, globalPath, `{"model":"base-model","active_profile":"work","profiles":{"work":{"model":"work-model"}}}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model != "work-model" {
		t.Fatalf("profile not applied: %q", cfg.Model)
	}
	cfg.MaxBudgetUsd = 3
	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	m := readJSONMap(t, globalPath)
	if m["model"] != "base-model" {
		t.Fatalf("top-level model = %v, want base-model (profile value leaked)", m["model"])
	}
	if m["max_budget_usd"] != 3.0 {
		t.Fatalf("max_budget_usd = %v, want the change to be saved", m["max_budget_usd"])
	}
}

// Save rebuilt the file from the Config struct, so any key the struct does not
// know — an option from a newer cove, a note the user added — vanished the
// first time a REPL command saved.
func TestSaveKeepsKeysItDoesNotKnow(t *testing.T) {
	global, _ := isolate(t)
	globalPath := filepath.Join(global, "config.json")
	writeFile(t, globalPath, `{"model":"a","future_option":{"x":1},"provider":{"name":"deepseek","api_key":"sk-k","organization":"org-1"}}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Model = "b"
	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	m := readJSONMap(t, globalPath)
	if _, ok := m["future_option"]; !ok {
		t.Errorf("unknown top-level key was dropped: %v", m)
	}
	prov, _ := m["provider"].(map[string]any)
	if prov["organization"] != "org-1" {
		t.Errorf("unknown provider key was dropped: %v", prov)
	}
}

// With a typo in config.json, Load falls back to defaults (and says so), but
// the next /model or /api-key then overwrote the broken file with those
// defaults — the user's whole config, including the API key, was gone.
func TestSaveRefusesToOverwriteMalformedConfig(t *testing.T) {
	global, _ := isolate(t)
	globalPath := filepath.Join(global, "config.json")
	broken := `{"model":"deepseek-v4-pro", "provider":{"name":"deepseek","api_key":"sk-precious"},}`
	writeFile(t, globalPath, broken)

	cfg, err := Load()
	if err == nil {
		t.Fatal("Load should report the parse error")
	}
	cfg.Model = "other"
	if err := Save(cfg); err == nil {
		t.Fatal("Save overwrote a config file it could not parse")
	}
	if got := readFile(t, globalPath); got != broken {
		t.Fatalf("broken config was modified:\n%s", got)
	}
}

// Two cove windows each hold the config they loaded. Saving wrote the whole
// struct, so the second window's /budget silently reverted the first window's
// /model. Only the fields a session changed should be written.
func TestSaveFromTwoSessionsKeepsBothChanges(t *testing.T) {
	global, _ := isolate(t)
	globalPath := filepath.Join(global, "config.json")
	writeFile(t, globalPath, `{"model":"m0","max_budget_usd":10}`)

	a, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	a.Model = "m1"
	if err := Save(a); err != nil {
		t.Fatal(err)
	}
	b.MaxBudgetUsd = 20
	if err := Save(b); err != nil {
		t.Fatal(err)
	}

	m := readJSONMap(t, globalPath)
	if m["model"] != "m1" || m["max_budget_usd"] != 20.0 {
		t.Fatalf("model=%v budget=%v, want both sessions' changes", m["model"], m["max_budget_usd"])
	}
}

// Clearing a value (here: deleting the last profile, which also clears
// active_profile) must remove it from the file, not leave the old one behind.
func TestSaveRemovesClearedValues(t *testing.T) {
	global, _ := isolate(t)
	globalPath := filepath.Join(global, "config.json")
	writeFile(t, globalPath, `{"model":"m","active_profile":"work","profiles":{"work":{"model":"w"}}}`)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	delete(cfg.Profiles, "work")
	cfg.ActiveProfile = ""
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}

	m := readJSONMap(t, globalPath)
	if _, ok := m["active_profile"]; ok {
		t.Errorf("active_profile still present: %v", m)
	}
	if p, ok := m["profiles"]; ok && len(p.(map[string]any)) != 0 {
		t.Errorf("deleted profile still present: %v", p)
	}
}

// Load fills an absent model_fast with the main model. Save used to write that
// derived value back, so after "/model b" the file said model_fast "a" and
// every "simple" turn kept going to the old model.
func TestSaveDoesNotBakeTheDerivedFastModel(t *testing.T) {
	global, _ := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"), `{"model":"a"}`)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model = "b"
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	again, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if again.ModelFast != "b" {
		t.Fatalf("model_fast = %q after /model b, want it to follow the main model", again.ModelFast)
	}
}

// The file holds an API key. os.WriteFile only applies 0600 when it creates
// the file, so a config.json that started life as 0644 stayed world-readable.
func TestSaveLeavesOnlyAPrivateConfigFile(t *testing.T) {
	global, _ := isolate(t)
	globalPath := filepath.Join(global, "config.json")
	writeFile(t, globalPath, `{"model":"m"}`)
	if err := os.Chmod(globalPath, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Provider.APIKey = "sk-secret-value"
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(global)
	for _, e := range entries {
		if e.Name() != "config.json" {
			t.Errorf("Save left %q behind in the config directory", e.Name())
		}
	}
	if !strings.Contains(readFile(t, globalPath), "sk-secret-value") {
		t.Fatal("API key was not saved")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(globalPath)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("config.json mode = %o, want 600", perm)
		}
	}
}
