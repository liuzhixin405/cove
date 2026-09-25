package context

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for git: copied into a temp dir as
// git(.exe) and put first on PATH, it acts out COVE_FAKE_GIT instead of
// running the tests.
func TestMain(m *testing.M) {
	switch os.Getenv("COVE_FAKE_GIT") {
	case "":
		os.Exit(m.Run())
	case "hang":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "env":
		// Report the lock setting the caller gave us, as porcelain output.
		os.Stdout.WriteString("?? GIT_OPTIONAL_LOCKS=" + os.Getenv("GIT_OPTIONAL_LOCKS") + "\n")
		os.Exit(0)
	}
	os.Exit(2)
}

// installFakeGit puts a copy of the test binary first on PATH as "git".
func installFakeGit(t *testing.T, mode string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	name := "git"
	if runtime.GOOS == "windows" {
		name = "git.exe"
	}
	src, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	// Launch the copy once, untimed, before any test measures it: the first
	// start of a new .exe can take seconds while antivirus scans it, longer
	// than gitTimeout when the whole suite runs in parallel.
	warm := exec.Command(filepath.Join(dir, name))
	warm.Env = append(os.Environ(), "COVE_FAKE_GIT=env")
	_ = warm.Run()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("COVE_FAKE_GIT", mode)
}

// TestGitCallsAreBoundedByATimeout: Collect runs at startup and waits for every
// git call. None of them had a timeout, so a git that never returns (a huge
// repo's status, a stuck credential helper, a network filesystem) kept cove
// from starting at all.
func TestGitCallsAreBoundedByATimeout(t *testing.T) {
	installFakeGit(t, "hang")
	old := gitTimeout
	gitTimeout = 300 * time.Millisecond
	defer func() { gitTimeout = old }()

	start := time.Now()
	_ = gitStatus(t.TempDir())
	_ = findGitRoot(t.TempDir())
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("two git calls against a hanging git took %v", elapsed)
	}
}

// TestGitStatusFailureIsNotReportedClean: gitStatus ignored git's error, so a
// status that failed or timed out printed nothing and was reported to the
// model as "(clean)" — a worktree full of changes described as clean.
func TestGitStatusFailureIsNotReportedClean(t *testing.T) {
	installFakeGit(t, "hang")
	old := gitTimeout
	gitTimeout = 300 * time.Millisecond
	defer func() { gitTimeout = old }()

	if got := gitStatus(t.TempDir()); strings.Contains(got, "clean") {
		t.Fatalf("a timed-out git status was reported as %q", got)
	}
}

// TestGitStatusTakesNoOptionalLocks: "git status" refreshes the index and
// takes .git/index.lock to do it. Run in the background while the user (or
// the model's own bash call) commits, it made that command fail with
// "index.lock: File exists".
func TestGitStatusTakesNoOptionalLocks(t *testing.T) {
	installFakeGit(t, "env")
	if got := gitStatus(t.TempDir()); !strings.Contains(got, "GIT_OPTIONAL_LOCKS=0") {
		t.Fatalf("git status ran without GIT_OPTIONAL_LOCKS=0: %q", got)
	}
}

// TestGitStatusShowsCJKNamesVerbatim: by default git quotes non-ASCII paths as
// octal escapes, so the prompt showed "\344\270\255\346\226\207.go" instead of
// 中文.go for every Chinese file name.
func TestGitStatusShowsCJKNamesVerbatim(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	init := exec.Command("git", "init", "-q")
	init.Dir = root
	if out, err := init.CombinedOutput(); err != nil {
		t.Skipf("git init failed: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(root, "中文.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := gitStatus(root); !strings.Contains(got, "中文.go") {
		t.Fatalf("git status = %q, want the file name verbatim", got)
	}
}

// TestRefreshGitAllRereadsBranchStatusAndLog: the per-turn refresh re-reads
// all three git fields the turn note shows. RefreshGit only covered branch and
// status, so "Recent commits" stayed as they were at startup.
func TestRefreshGitAllRereadsBranchStatusAndLog(t *testing.T) {
	installFakeGit(t, "env") // answers every git command with one line
	root := t.TempDir()
	c := &ProjectContext{Cwd: root, GitRoot: root, IsGitRepo: true, FileTree: "tree", RepoMap: "map"}
	c.RefreshGitAll()
	branch, status := c.GetGitInfo()
	if branch == "" || status == "" || c.GitLog == "" {
		t.Fatalf("after RefreshGitAll branch=%q status=%q log=%q; want all three re-read", branch, status, c.GitLog)
	}
	if c.FileTree != "tree" || c.RepoMap != "map" {
		t.Fatal("RefreshGitAll touched the file tree or repo map")
	}
}
