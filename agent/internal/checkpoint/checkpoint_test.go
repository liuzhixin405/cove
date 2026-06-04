package checkpoint

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagerCreateAndRestore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GIT_AUTHOR_NAME", "agentgo test")
	t.Setenv("GIT_AUTHOR_EMAIL", "agentgo@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "agentgo test")
	t.Setenv("GIT_COMMITTER_EMAIL", "agentgo@example.test")

	workDir := t.TempDir()
	filePath := filepath.Join(workDir, "note.txt")
	if err := os.WriteFile(filePath, []byte("before"), 0o600); err != nil {
		t.Fatalf("write initial file: %v", err)
	}

	mgr, err := New(workDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hash, err := mgr.Create("before edit")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if strings.TrimSpace(hash) == "" {
		t.Fatal("expected checkpoint hash")
	}

	if err := os.WriteFile(filePath, []byte("after"), 0o600); err != nil {
		t.Fatalf("write modified file: %v", err)
	}
	if err := mgr.Restore(hash); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if got := string(data); got != "before" {
		t.Fatalf("restored file = %q, want before", got)
	}
	if mgr.Count() != 1 {
		t.Fatalf("Count() = %d, want 1", mgr.Count())
	}
	if len(mgr.List()) == 0 {
		t.Fatal("expected checkpoint to appear in List")
	}
}
