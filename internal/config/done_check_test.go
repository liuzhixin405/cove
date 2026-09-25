package config

import (
	"path/filepath"
	"testing"
)

func TestDoneCheckDefaultsToAuto(t *testing.T) {
	isolate(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.DoneCheckMode(); got != "auto" {
		t.Fatalf("DoneCheckMode = %q, want auto", got)
	}
}

func TestDoneCheckFromGlobalAndProject(t *testing.T) {
	global, project := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"), `{"done_check": "off"}`)
	cfg, _ := Load()
	if got := cfg.DoneCheckMode(); got != "off" {
		t.Fatalf("global: DoneCheckMode = %q, want off", got)
	}
	writeFile(t, filepath.Join(project, ".cove.json"), `{"done_check": " ON "}`)
	cfg, _ = Load()
	if got := cfg.DoneCheckMode(); got != "on" {
		t.Fatalf(".cove.json: DoneCheckMode = %q, want on", got)
	}
}

func TestDoneCheckUnknownValueIsAuto(t *testing.T) {
	c := &Config{DoneCheck: "sometimes"}
	if got := c.DoneCheckMode(); got != "auto" {
		t.Fatalf("DoneCheckMode = %q, want auto", got)
	}
}
