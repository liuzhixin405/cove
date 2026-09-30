package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A committed `.cove.json -> /dev/zero` (or /dev/tty, a fifo) used to be read
// with os.ReadFile, which follows the link and reads without a limit: cove
// hung or ran out of memory at startup, and so did the dream worker. Only a
// regular file is read now.
func TestProjectConfigSymlinkIsRefused(t *testing.T) {
	_, project := isolate(t)
	target := filepath.Join(project, "elsewhere.json")
	writeFile(t, target, `{"model":"linked-model"}`)
	if err := os.Symlink(target, filepath.Join(project, ".cove.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model == "linked-model" || cfg.projectConfigPath != "" {
		t.Fatalf("a symlinked .cove.json was applied: model=%q path=%q", cfg.Model, cfg.projectConfigPath)
	}
}

func TestProjectConfigSymlinkToDeviceDoesNotHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /dev/zero")
	}
	_, project := isolate(t)
	if err := os.Symlink("/dev/zero", filepath.Join(project, ".cove.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestOversizedProjectConfigIsRefused(t *testing.T) {
	_, project := isolate(t)
	body := `{"model":"huge-model"}` + strings.Repeat(" ", maxProjectConfigBytes)
	writeFile(t, filepath.Join(project, ".cove.json"), body)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model == "huge-model" {
		t.Fatal("a .cove.json over the size limit was applied")
	}
	if ok, err := IsProjectConfigTrusted(filepath.Join(project, ".cove.json")); ok || err == nil {
		t.Fatalf("IsProjectConfigTrusted on an oversized file = %v, %v; want a refusal", ok, err)
	}
}

// A device is refused without being read; this runs where symlinks cannot be
// created (Windows without developer mode).
func TestProjectConfigDeviceIsRefused(t *testing.T) {
	dev := "/dev/null"
	if runtime.GOOS == "windows" {
		dev = "NUL"
	}
	if _, err := readProjectConfigFile(dev); !errors.Is(err, errProjectConfigRefused) {
		t.Fatalf("readProjectConfigFile(%s) err = %v, want a refusal", dev, err)
	}
}
