package context

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The files-only facts agree with what git itself reports.
func TestGitDirFactsMatchGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(root, "init", "-q", "-b", "trunk")
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(root, "add", "a")
	run(root, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init")
	run(root, "branch", "master")
	sub := filepath.Join(root, "deep", "er")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	gd, top := findGitDir(sub)
	if !sameDir(top, run(sub, "rev-parse", "--show-toplevel")) {
		t.Fatalf("root %q, git says %q", top, run(sub, "rev-parse", "--show-toplevel"))
	}
	if got := headBranch(gd); got != "trunk" {
		t.Fatalf("branch %q, want trunk", got)
	}
	if got := mainBranch(gd); got != "master" {
		t.Fatalf("main branch %q, want master", got)
	}
	run(root, "pack-refs", "--all")
	if got := mainBranch(gd); got != "master" {
		t.Fatalf("packed refs: main branch %q, want master", got)
	}

	// A worktree: .git is a file, refs live in the main repository.
	wt := filepath.Join(t.TempDir(), "wt")
	run(root, "worktree", "add", "-q", "-b", "feature", wt)
	gd, top = findGitDir(wt)
	if !sameDir(top, wt) || headBranch(gd) != "feature" || mainBranch(gd) != "master" {
		t.Fatalf("worktree: root %q branch %q main %q", top, headBranch(gd), mainBranch(gd))
	}

	run(root, "checkout", "-q", "--detach")
	if gd, _ := findGitDir(root); headBranch(gd) != "HEAD" {
		t.Fatalf("detached HEAD = %q", headBranch(gd))
	}
	if gd, top := findGitDir(t.TempDir()); gd != "" || top != "" {
		t.Fatalf("outside a repository: %q %q", gd, top)
	}
}

func sameDir(a, b string) bool {
	ea, _ := filepath.EvalSymlinks(a)
	eb, _ := filepath.EvalSymlinks(filepath.FromSlash(b))
	return strings.EqualFold(filepath.Clean(ea), filepath.Clean(eb))
}
