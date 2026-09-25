package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/config"
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/state"
)

func TestProviderSwitchModel(t *testing.T) {
	cases := []struct{ oldProv, newProv, model, want string }{
		// The old provider's default follows the switch...
		{"anthropic", "deepseek", config.DefaultModelForProvider("anthropic"), config.DefaultModelForProvider("deepseek")},
		{"deepseek", "openai", config.DefaultModelForProvider("deepseek"), config.DefaultModelForProvider("openai")},
		{"anthropic", "deepseek", "", config.DefaultModelForProvider("deepseek")},
		{"anthropic", "deepseek", "auto", config.DefaultModelForProvider("deepseek")},
		// ...a model the user chose stays.
		{"openai-compatible", "glm", "glm-4.6", "glm-4.6"},
		{"deepseek", "openrouter", "deepseek-v4-flash", "deepseek-v4-flash"},
	}
	for _, c := range cases {
		if got := providerSwitchModel(c.oldProv, c.newProv, c.model); got != c.want {
			t.Errorf("providerSwitchModel(%q, %q, %q) = %q, want %q", c.oldProv, c.newProv, c.model, got, c.want)
		}
	}
}

func savedConfig(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv("COVE_CONFIG_DIR"), "config.json"))
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	return string(data)
}

// "/provider deepseek" kept claude-sonnet-4 as the model, and saved the file
// before the model was even looked at.
func TestProviderCommandMovesDefaultModelAndSavesIt(t *testing.T) {
	eng := newTestEngine(t)
	captureOut(t)
	cfg := config.DefaultConfig()
	cfg.Provider.Name = "anthropic"
	cfg.Provider.APIKey = "placeholder"
	cfg.Model = config.DefaultModelForProvider("anthropic")
	as := &state.AppState{}

	handleBuiltinConfigCommand("/provider deepseek", cfg, eng, permission.NewManager(permission.Default), as)

	want := config.DefaultModelForProvider("deepseek")
	if cfg.Model != want || as.Model != want {
		t.Fatalf("model = %q (app state %q), want %q", cfg.Model, as.Model, want)
	}
	if saved := savedConfig(t); !strings.Contains(saved, want) {
		t.Fatalf("saved config does not carry the new model:\n%s", saved)
	}
}

// config.Save refuses to overwrite a config.json it cannot parse; /mode and
// /budget discarded that error and reported success.
func TestConfigCommandsReportSaveFailure(t *testing.T) {
	for _, in := range []string{"/mode auto", "/budget save"} {
		eng := newTestEngine(t)
		dir := os.Getenv("COVE_CONFIG_DIR")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		buf := captureOut(t)
		handleBuiltinConfigCommand(in, config.DefaultConfig(), eng, permission.NewManager(permission.Default), &state.AppState{})
		if !strings.Contains(buf.String(), "保存失败") {
			t.Errorf("%s: save failure not reported, output %q", in, buf.String())
		}
	}
}
