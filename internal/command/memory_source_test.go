package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/memory"
)

func TestMemoryListShowsSource(t *testing.T) {
	proj, glob := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, "p.md"), []byte("project fact"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(glob, "g.md"), []byte("global fact"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := memory.NewStoreForProject(proj, glob)
	out, err := NewMemoryCmd().Execute(context.Background(), Input{Args: []string{"list"}, MemoryStore: store})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Message, "p.md (项目)") || !strings.Contains(out.Message, "g.md (全局)") {
		t.Fatalf("sources not shown:\n%s", out.Message)
	}
}

func TestMemoryAddOverGlobalKeepsGlobalContent(t *testing.T) {
	proj, glob := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(glob, "prefs.md"), []byte("Prefers tabs."), 0o644); err != nil {
		t.Fatal(err)
	}
	store := memory.NewStoreForProject(proj, glob)
	if _, err := NewMemoryCmd().Execute(context.Background(), Input{Args: []string{"add", "prefs.md", "Uses", "pnpm."}, MemoryStore: store}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(proj, "prefs.md"))
	if !strings.Contains(string(data), "Prefers tabs.") || !strings.Contains(string(data), "Uses pnpm.") {
		t.Fatalf("project copy = %q", data)
	}
}
