package tool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// gitRepo makes a repository with one commit inside its own parent directory,
// so the sibling worktree ../<branch> lands in a temp dir too.
func gitRepo(t *testing.T) string {
	t.Helper()
	requireGit(t)
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return repo
}

// exit_worktree ignored git's exit status: when `git worktree remove` refused
// (uncommitted changes, wrong directory) it still said "Worktree removed." and
// forgot the worktree, leaving it on disk with nothing tracking it.
func TestExitWorktreeReportsFailureAndKeepsState(t *testing.T) {
	requireGit(t)
	rt := &Runtime{WorktreeDir: filepath.Join(t.TempDir(), "gone")}
	res, _ := NewExitWorktreeTool().Call(context.Background(), Input{}, Context{Cwd: t.TempDir(), Runtime: rt})
	if !res.IsError {
		t.Fatalf("failed removal reported as success: %q", res.Data)
	}
	if rt.GetWorktreeDir() == "" {
		t.Fatal("worktree state was cleared although the removal failed")
	}
}

// The recorded path was cwd + "/../" + branch, e.g. `D:\proj/../feat`, a
// mixed-separator, uncleaned path shown to the user and handed back to git.
func TestWorktreeRecordsACleanPathAndRemovesIt(t *testing.T) {
	repo := gitRepo(t)
	rt := &Runtime{}
	res, _ := NewEnterWorktreeTool().Call(context.Background(), Input{"branch": "feat"}, Context{Cwd: repo, Runtime: rt})
	if res.IsError {
		t.Fatalf("worktree: %s", res.Data)
	}
	want := filepath.Join(filepath.Dir(repo), "feat")
	if got := rt.GetWorktreeDir(); got != want {
		t.Fatalf("recorded worktree %q, want %q", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("worktree directory missing: %v", err)
	}

	res, _ = NewExitWorktreeTool().Call(context.Background(), Input{}, Context{Cwd: repo, Runtime: rt})
	if res.IsError {
		t.Fatalf("exit_worktree: %s", res.Data)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("worktree directory still present after exit_worktree (err %v)", err)
	}
}

// A branch starting with "-" is parsed by git as an option, and ".." walks the
// new worktree out of the project's parent directory.
func TestWorktreeRejectsUnsafeBranchNames(t *testing.T) {
	requireGit(t)
	for _, branch := range []string{"", "-f", "--detach", "../../elsewhere", "a/../../b"} {
		res, _ := NewEnterWorktreeTool().Call(context.Background(), Input{"branch": branch}, Context{Cwd: t.TempDir(), Runtime: &Runtime{}})
		if !res.IsError || !strings.Contains(res.Data, "invalid branch") {
			t.Errorf("branch %q: want an invalid-branch error, got %q", branch, res.Data)
		}
	}
}
