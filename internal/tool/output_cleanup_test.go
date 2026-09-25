package tool

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneOldToolOutputs(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.txt")
	fresh := filepath.Join(dir, "fresh.txt")
	sub := filepath.Join(dir, "sub")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sub, past, past); err != nil {
		t.Fatal(err)
	}
	n := PruneOldToolOutputs(dir, 7*24*time.Hour)
	if n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old file kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh file removed")
	}
	if _, err := os.Stat(sub); err != nil {
		t.Fatal("directories are left alone")
	}
	if PruneOldToolOutputs(filepath.Join(dir, "missing"), time.Hour) != 0 {
		t.Fatal("missing dir should be a no-op")
	}
}
