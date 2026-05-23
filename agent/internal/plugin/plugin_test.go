package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagerInstallDisableEnableAndReload(t *testing.T) {
	tmp := t.TempDir()
	oldHome := os.Getenv("HOME")
	oldUserProfile := os.Getenv("USERPROFILE")
	t.Cleanup(func() {
		_ = os.Setenv("HOME", oldHome)
		_ = os.Setenv("USERPROFILE", oldUserProfile)
	})
	_ = os.Setenv("HOME", tmp)
	_ = os.Setenv("USERPROFILE", tmp)

	mgr := NewManager()
	mgr.Init()
	if err := mgr.Install("demo", "https://example.com/plugin"); err != nil {
		t.Fatalf("install plugin: %v", err)
	}

	manifestPath := filepath.Join(mgr.dir, "demo", "manifest.json")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("expected manifest written to disk: %v", err)
	}

	reloaded := NewManager()
	reloaded.Init()
	plugins := reloaded.AllPlugins()
	if len(plugins) != 1 || plugins[0].Manifest.Name != "demo" || plugins[0].State != Enabled {
		t.Fatalf("expected installed plugin to reload as enabled, got %#v", plugins)
	}

	if err := reloaded.Disable("demo"); err != nil {
		t.Fatalf("disable plugin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(reloaded.dir, "demo.disabled")); err != nil {
		t.Fatalf("expected disabled plugin directory to exist: %v", err)
	}

	disabledReload := NewManager()
	disabledReload.Init()
	plugins = disabledReload.AllPlugins()
	if len(plugins) != 1 || plugins[0].State != Disabled {
		t.Fatalf("expected disabled plugin to reload as disabled, got %#v", plugins)
	}

	if err := disabledReload.Enable("demo"); err != nil {
		t.Fatalf("enable plugin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(disabledReload.dir, "demo")); err != nil {
		t.Fatalf("expected enabled plugin directory to exist: %v", err)
	}

	if err := disabledReload.Uninstall("demo"); err != nil {
		t.Fatalf("uninstall plugin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(disabledReload.dir, "demo")); !os.IsNotExist(err) {
		t.Fatalf("expected plugin directory removed, stat err=%v", err)
	}
}