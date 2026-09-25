package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProjectDataDir_StableAndCreated(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("COVE_CONFIG_DIR", cfg)
	root := filepath.Join(t.TempDir(), "proj")

	d1, err := ProjectDataDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d1, filepath.Join(cfg, "projects")+string(filepath.Separator)) {
		t.Fatalf("unexpected dir %q", d1)
	}
	if len(filepath.Base(d1)) != 8 {
		t.Fatalf("hash component should be 8 hex chars, got %q", filepath.Base(d1))
	}
	if st, err := os.Stat(d1); err != nil || !st.IsDir() {
		t.Fatalf("dir not created: %v", err)
	}
	d2, _ := ProjectDataDir(root + string(filepath.Separator) + ".")
	if d1 != d2 {
		t.Fatalf("clean path should give same dir: %q vs %q", d1, d2)
	}
	if runtime.GOOS == "windows" {
		d3, _ := ProjectDataDir(strings.ToUpper(root))
		if d1 != d3 {
			t.Fatalf("windows should be case-insensitive: %q vs %q", d1, d3)
		}
	}
	other, _ := ProjectDataDir(root + "x")
	if other == d1 {
		t.Fatal("different roots must differ")
	}
}
