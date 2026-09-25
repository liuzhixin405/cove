package context

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A repository created after cove started ("git init" in the session) is
// picked up by the next turn's refresh; it used to stay "not a git repo"
// until restart.
func TestRefreshGitAllDetectsNewRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	c := &ProjectContext{Cwd: dir}
	c.RefreshGitAll()
	if c.IsGitRepo {
		t.Fatal("a plain directory must not count as a repository")
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	c.RefreshGitAll()
	if !c.IsGitRepo || c.GitRoot == "" {
		t.Fatalf("after git init: IsGitRepo=%v GitRoot=%q", c.IsGitRepo, c.GitRoot)
	}
	if branch, _ := c.GetGitInfo(); branch == "" {
		t.Fatal("the new repository's branch was not read")
	}
}

// Outside any repository the refresh does not spawn git on every turn.
func TestHasGitMarkerAbove(t *testing.T) {
	dir := t.TempDir()
	if hasGitMarkerAbove(dir) {
		t.Skip("the temp directory is inside a repository")
	}
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if hasGitMarkerAbove(sub) {
		t.Fatal("no .git anywhere above, but a marker was found")
	}
	// A worktree or submodule has a .git file rather than a directory.
	if err := os.WriteFile(filepath.Join(dir, "a", ".git"), []byte("gitdir: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !hasGitMarkerAbove(sub) {
		t.Fatal("the .git in a parent directory was not found")
	}
}
