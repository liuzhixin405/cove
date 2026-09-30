package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	ctxt "github.com/liuzhixin405/cove-agent/internal/context"
)

func TestParseGitStatusAndLine(t *testing.T) {
	for _, tc := range []struct {
		name, out, want string
	}{
		{"dirty", "## main...origin/main\n M a.go\n?? b.go\n", "git：2 个文件有未提交的改动"},
		{"dirty and ahead", "## main...origin/main [ahead 2]\n M a.go\n", "git：1 个文件有未提交的改动，2 个提交未推送到 origin/main"},
		{"committed, not pushed", "## main...origin/main [ahead 3, behind 1]\n", "git：改动已提交，3 个提交未推送到 origin/main"},
		// A state, not "已提交并推送": the line also follows turns that
		// committed nothing.
		{"in sync", "## main...origin/main\n", "git：工作区干净，与 origin/main 同步（按本地记录）"},
		{"behind only has nothing unpushed", "## main...origin/main [behind 4]\n", "git：工作区干净，没有未推送的提交，落后 origin/main 4 个提交（按本地记录）"},
		{"no upstream", "## feature\n", "git：改动已提交，分支 feature 没有上游，未推送"},
		{"upstream deleted", "## feature...origin/feature [gone]\n", "git：改动已提交，分支 feature 没有上游，未推送"},
		{"no commits yet", "## No commits yet on main\n?? a.go\n", "git：1 个文件有未提交的改动，分支 main 没有上游，未推送"},
		{"detached", "## HEAD (no branch)\n M a.go\n", "git：1 个文件有未提交的改动"},
		{"crlf", "## main...origin/main\r\n M a.go\r\n", "git：1 个文件有未提交的改动"},
	} {
		if got := parseGitStatus(tc.out).Line(); got != tc.want {
			t.Errorf("%s: Line() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestGitCommandRe(t *testing.T) {
	for cmd, want := range map[string]bool{
		"git commit -m x":             true,
		"cd sub && git push":          true,
		"git.exe status":              true,
		"(git add -A)":                true,
		"echo digit test":             false,
		"cat .gitignore":              false,
		"go test ./internal/gitdir/":  false,
		"github-cli version":          false,
		"npm run lint; git status -s": true,
	} {
		if got := gitCommandRe.MatchString(cmd); got != want {
			t.Errorf("%q: %v, want %v", cmd, got, want)
		}
	}
}

// The line appears only after a turn that changed files or ran a
// repository-changing git command, in a repository.
func TestReportGitWorkState(t *testing.T) {
	eng := newTestEngine(&mockProvider{})
	var lines []string
	eng.SetOutput(LineSink(func(s string) { lines = append(lines, s) }))
	eng.projCtx = &ctxt.ProjectContext{Cwd: t.TempDir(), IsGitRepo: true}
	orig := readGitWorkStateFn
	defer func() { readGitWorkStateFn = orig }()
	readGitWorkStateFn = func(context.Context, string) (gitWorkState, error) {
		return gitWorkState{Branch: "main", Upstream: "origin/main", Changed: 1}, nil
	}

	eng.reportGitWorkState(context.Background())
	if len(lines) != 0 {
		t.Fatalf("a turn that changed nothing got a git line: %q", lines)
	}

	eng.turnFilesChanged = true
	eng.reportGitWorkState(context.Background())
	if len(lines) != 1 || !strings.Contains(lines[0], "1 个文件有未提交的改动") {
		t.Fatalf("lines = %q", lines)
	}

	lines = nil
	eng.turnFilesChanged, eng.turnRanGit = false, true
	eng.reportGitWorkState(context.Background())
	if len(lines) != 1 {
		t.Fatalf("a turn that ran git got no line: %q", lines)
	}

	lines = nil
	readGitWorkStateFn = func(context.Context, string) (gitWorkState, error) {
		return gitWorkState{}, errors.New("not a repository")
	}
	eng.reportGitWorkState(context.Background())
	if len(lines) != 0 {
		t.Fatalf("a failed git status printed %q", lines)
	}

	eng.projCtx.IsGitRepo = false
	eng.turnFilesChanged = true
	readGitWorkStateFn = orig
	eng.reportGitWorkState(context.Background())
	if len(lines) != 0 {
		t.Fatalf("outside a repository: %q", lines)
	}
}

// Against a real git: uncommitted, committed but not pushed, pushed.
func TestReadGitWorkStateRealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	remote, work := filepath.Join(root, "remote.git"), filepath.Join(root, "work")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(root, "init", "--bare", "-b", "main", remote)
	git(root, "clone", remote, work)
	git(work, "checkout", "-b", "main")
	state := func() gitWorkState {
		t.Helper()
		s, err := readGitWorkState(context.Background(), work)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := state(); s.Changed != 1 || s.Upstream != "" {
		t.Fatalf("new file: %+v", s)
	}
	git(work, "add", "a.txt")
	git(work, "commit", "-m", "a")
	git(work, "push", "-u", "origin", "main")
	if got := state().Line(); got != "git：工作区干净，与 origin/main 同步（按本地记录）" {
		t.Fatalf("after push: %q", got)
	}
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(work, "commit", "-am", "b")
	if got := state().Line(); got != "git：改动已提交，1 个提交未推送到 origin/main" {
		t.Fatalf("after commit: %q", got)
	}
}
