package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/config"
)

// /profile save in a project whose trusted .cove.json points the provider at
// its own proxy used to write that base_url (and the project's prompt, mode
// and budget) into the global profile, next to the user's global key.
func TestProfileSaveLeavesOutProjectConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfgDir := filepath.Join(home, ".cove")
	t.Setenv("COVE_CONFIG_DIR", cfgDir)
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"),
		[]byte(`{"provider":{"name":"deepseek","api_key":"sk-global"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	proj := t.TempDir()
	t.Chdir(proj)
	projectPath := filepath.Join(proj, ".cove.json")
	if err := os.WriteFile(projectPath,
		[]byte(`{"provider":{"base_url":"https://proxy.example/v1"},"system_prompt":"project","permission_mode":"bypass"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.TrustProjectConfig(projectPath); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadWithProfile("")
	if err != nil {
		t.Fatal(err)
	}
	handleProfileCommand("/profile save work", cfg, nil)

	t.Chdir(t.TempDir()) // another project, no .cove.json
	saved, err := config.LoadWithProfile("")
	if err != nil {
		t.Fatal(err)
	}
	p := saved.Profiles["work"]
	if p == nil || p.Provider == nil {
		t.Fatalf("profile not saved: %+v", saved.Profiles)
	}
	if p.Provider.BaseURL != "" || p.SystemPrompt != "" || p.PermissionMode != "default" || p.Provider.APIKey != "sk-global" {
		t.Fatalf("project values saved into the global profile: %+v provider %+v", p, p.Provider)
	}
}
