package checkpoint

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// withGlobalGitConfig points git's global config at a temp file holding body,
// the way a user's ~/.gitconfig (or Git for Windows' defaults) would leak into
// the shadow store.
func withGlobalGitConfig(t *testing.T, body string) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	writeFile(t, cfg, body)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
}

func TestUndoRemovesNonASCIIPaths(t *testing.T) {
	isolatedGit(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "keep.txt"), "k")
	mgr, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := mgr.Create("before")
	if err != nil {
		t.Fatal(err)
	}
	added := filepath.Join(dir, "中文.txt")
	writeFile(t, added, "new")
	if err := os.MkdirAll(filepath.Join(dir, "目录"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "目录", "文件 空格.txt"), "new")

	if _, err := mgr.Restore(cp); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, p := range []string{added, filepath.Join(dir, "目录", "文件 空格.txt")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s created after the checkpoint survived /undo (stat err %v)", p, err)
		}
	}
	// The directory existed only because of the new file.
	if _, err := os.Stat(filepath.Join(dir, "目录")); !os.IsNotExist(err) {
		t.Fatalf("directory created after the checkpoint survived /undo (stat err %v)", err)
	}
}

func TestUndoIsByteExactDespiteUserEOLConfig(t *testing.T) {
	isolatedGit(t)
	for _, tc := range []struct {
		name, config, content string
	}{
		{"autocrlf=true keeps LF", "[core]\n\tautocrlf = true\n", "line1\nline2\n"},
		{"autocrlf=input keeps CRLF", "[core]\n\tautocrlf = input\n", "line1\r\nline2\r\n"},
		{"eol=crlf keeps mixed", "[core]\n\teol = crlf\n", "a\nb\r\nc\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withGlobalGitConfig(t, tc.config)
			dir := t.TempDir()
			path := filepath.Join(dir, "f.txt")
			writeFile(t, path, tc.content)
			// A project .gitattributes asking for conversion must not apply
			// to the shadow store either.
			writeFile(t, filepath.Join(dir, ".gitattributes"), "* text=auto eol=crlf\n")
			mgr, err := New(dir)
			if err != nil {
				t.Fatal(err)
			}
			cp, err := mgr.Create("before")
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, path, "changed")
			if _, err := mgr.Restore(cp); err != nil {
				t.Fatalf("Restore: %v", err)
			}
			if got := readFile(t, path); got != tc.content {
				t.Fatalf("restored %q, want byte-identical %q", got, tc.content)
			}
		})
	}
}

// initNestedRepo makes dir a git repository with one commit.
func initNestedRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "inner.txt"), "inner")
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "inner.txt"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "i"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func TestUndoSkipsNestedRepoAndFinishesTheRest(t *testing.T) {
	isolatedGit(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "old")
	mgr, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := mgr.Create("before")
	if err != nil {
		t.Fatal(err)
	}
	// After the checkpoint: a nested repo (sorted first, so the old code
	// aborted before reaching the rest), a modified file and a new file.
	nested := filepath.Join(dir, "aaa-repo")
	initNestedRepo(t, nested)
	writeFile(t, filepath.Join(dir, "a.txt"), "new")
	writeFile(t, filepath.Join(dir, "zzz-new.txt"), "new")

	backup, err := mgr.Restore(cp)
	if backup == "" {
		t.Fatalf("Restore returned no backup (err %v); the rollback could not be undone", err)
	}
	if err == nil || !strings.Contains(err.Error(), "aaa-repo") {
		t.Fatalf("Restore error = %v, want one naming the skipped nested repo", err)
	}
	if got := readFile(t, filepath.Join(dir, "a.txt")); got != "old" {
		t.Fatalf("a.txt = %q, want old", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "zzz-new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new file after the nested repo was not removed (stat err %v)", err)
	}
	// The nested repo's content is not in any checkpoint: deleting it would
	// lose it for good.
	if got := readFile(t, filepath.Join(nested, "inner.txt")); got != "inner" {
		t.Fatalf("nested repo content = %q, want it left alone", got)
	}
}

func TestUndoKeepsNewDirWithUncapturedContent(t *testing.T) {
	isolatedGit(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".gitignore"), "*.log\n")
	mgr, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := mgr.Create("before")
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "newdir")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sub, "code.go"), "package x")
	writeFile(t, filepath.Join(sub, "run.log"), "ignored, not in any checkpoint")

	if _, err := mgr.Restore(cp); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, "code.go")); !os.IsNotExist(err) {
		t.Fatalf("new file not removed (stat err %v)", err)
	}
	if got := readFile(t, filepath.Join(sub, "run.log")); got == "" {
		t.Fatal("ignored file inside a new directory was deleted")
	}
}

// "git init" in a subdirectory, no commit yet: git add --all refused the
// whole tree ("does not have a commit checked out"), so no checkpoint could
// be taken and /undo refused for want of its backup.
func TestCheckpointWithEmptyNestedRepo(t *testing.T) {
	isolatedGit(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "old")
	cmd := exec.Command("git", "init", "-q", filepath.Join(dir, "fresh"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	writeFile(t, filepath.Join(dir, "fresh", "untracked.txt"), "x")
	mgr, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := mgr.Create("before")
	if err != nil {
		t.Fatalf("Create with an empty nested repo: %v", err)
	}
	writeFile(t, filepath.Join(dir, "a.txt"), "new")
	if _, err := mgr.Restore(cp); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "a.txt")); got != "old" {
		t.Fatalf("a.txt = %q, want old", got)
	}
	if got := readFile(t, filepath.Join(dir, "fresh", "untracked.txt")); got != "x" {
		t.Fatalf("the nested repo's file = %q, want it left alone", got)
	}
}
