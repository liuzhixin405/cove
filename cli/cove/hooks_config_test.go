package main

import (
	"os"
	"path/filepath"
	"testing"
)

// hooks.json is read from the config directory, like policies.json: with
// COVE_CONFIG_DIR set it lives there, not in ~/.cove.
func TestUserHooksReadFromConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("COVE_CONFIG_DIR", cfgDir)
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"hooks":{"BeforeTool":[{"matcher":"bash","command":"echo hi"}]}}`
	if err := os.WriteFile(filepath.Join(cfgDir, "hooks.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// A hooks.json in ~/.cove must not be picked up instead.
	if err := os.MkdirAll(filepath.Join(home, ".cove"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".cove", "hooks.json"), []byte(`{"hooks":{"AfterTool":[{"command":"echo no"},{"command":"echo no2"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	defs, err := loadUserHooks()
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Matcher != "bash" {
		t.Fatalf("defs = %+v, want the one hook from COVE_CONFIG_DIR", defs)
	}
}
