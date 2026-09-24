package tool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s", out)
	}
}

// withoutRipgrep runs the grep tool as it behaves on a machine without rg.
func withoutRipgrep(t *testing.T) {
	t.Helper()
	orig := lookRipgrep
	lookRipgrep = func() (string, error) { return "", errors.New("not installed") }
	t.Cleanup(func() { lookRipgrep = orig })
}

// grepModes runs a test with ripgrep (when installed) and with the fallback.
func grepModes(t *testing.T, fn func(t *testing.T)) {
	t.Run("fallback", func(t *testing.T) { withoutRipgrep(t); fn(t) })
	t.Run("ripgrep", func(t *testing.T) {
		if _, err := lookRipgrep(); err != nil {
			t.Skip("rg not installed")
		}
		fn(t)
	})
}

func grep(t *testing.T, dir string, in Input) Result {
	t.Helper()
	res, err := NewGrepTool().Call(context.Background(), in, Context{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestGrepFindsMatchesAndFiltersByInclude(t *testing.T) {
	grepModes(t, func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{"pkg/a.go": "package pkg\nfunc Foo() {}\n", "notes.txt": "func Foo in prose\n"})
		res := grep(t, dir, Input{"pattern": `func Foo`, "include": "*.go"})
		if res.IsError || !strings.Contains(res.Data, "a.go:2:") || strings.Contains(res.Data, "notes.txt") {
			t.Fatalf("grep = %q", res.Data)
		}
	})
}

func TestGrepCapsOutput(t *testing.T) {
	grepModes(t, func(t *testing.T) {
		dir := t.TempDir()
		files := map[string]string{}
		for i := 0; i < 150; i++ {
			files[fmt.Sprintf("f%03d.txt", i)] = "needle\n"
		}
		writeTree(t, dir, files)
		res := grep(t, dir, Input{"pattern": "needle"})
		lines := strings.Split(strings.TrimSpace(res.Data), "\n")
		if len(lines) > grepMaxLines+1 || !strings.Contains(res.Data, "more match") {
			t.Fatalf("grep returned %d lines, want at most %d plus a note", len(lines), grepMaxLines)
		}
	})
}

func TestGrepPatternStartingWithDash(t *testing.T) {
	grepModes(t, func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{"run.sh": "ls -la /tmp\n"})
		res := grep(t, dir, Input{"pattern": "-la"})
		if res.IsError || !strings.Contains(res.Data, "run.sh") {
			t.Fatalf("grep -la = %q", res.Data)
		}
	})
}

func TestGrepReportsBadRegex(t *testing.T) {
	grepModes(t, func(t *testing.T) {
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{"a.txt": "x\n"})
		res := grep(t, dir, Input{"pattern": "(unclosed"})
		if !res.IsError || !strings.Contains(strings.ToLower(res.Data), "regex") {
			t.Fatalf("bad regex = %q, want an error that names the regex problem", res.Data)
		}
	})
}

func TestGrepHonorsGitignore(t *testing.T) {
	grepModes(t, func(t *testing.T) {
		dir := t.TempDir()
		gitInit(t, dir)
		writeTree(t, dir, map[string]string{".gitignore": "bin/\n", "bin/out.txt": "needle\n", "src/a.txt": "needle\n"})
		res := grep(t, dir, Input{"pattern": "needle"})
		if !strings.Contains(res.Data, "a.txt") || strings.Contains(res.Data, "out.txt") {
			t.Fatalf("grep = %q, want src/a.txt only", res.Data)
		}
	})
}

func TestGrepStaysInWorkingDirectory(t *testing.T) {
	grepModes(t, func(t *testing.T) {
		dir, outside := t.TempDir(), t.TempDir()
		writeTree(t, outside, map[string]string{"secret.txt": "needle\n"})
		res := grep(t, dir, Input{"pattern": "needle", "path": outside})
		if !res.IsError || strings.Contains(res.Data, "secret.txt") {
			t.Fatalf("grep outside cwd = %q, want an error", res.Data)
		}
	})
}

func glob(t *testing.T, dir string, in Input) Result {
	t.Helper()
	res, err := NewGlobTool().Call(context.Background(), in, Context{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestGlobFindsFilesInDotDirectories(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".github/workflows/ci.yml": "on: push\n", ".git/config": "x"})
	for _, pattern := range []string{".github/**/*.yml", "**/*.yml"} {
		if res := glob(t, dir, Input{"pattern": pattern}); !strings.Contains(res.Data, "ci.yml") {
			t.Errorf("glob %s = %q, want .github/workflows/ci.yml", pattern, res.Data)
		}
	}
	if res := glob(t, dir, Input{"pattern": "**/config"}); strings.Contains(res.Data, ".git") {
		t.Errorf("glob listed .git internals: %q", res.Data)
	}
}

func TestGlobHonorsGitignore(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	writeTree(t, dir, map[string]string{".gitignore": "bin/\nobj/\n", "bin/Debug/App.dll": "x", "obj/App.cs": "x", "src/App.cs": "x"})
	res := glob(t, dir, Input{"pattern": "**/*.cs"})
	if !strings.Contains(res.Data, "App.cs") || strings.Contains(res.Data, "obj") {
		t.Fatalf("glob = %q, want src/App.cs only", res.Data)
	}
}

func TestGlobStaysInWorkingDirectory(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	writeTree(t, outside, map[string]string{"id_rsa": "x"})
	if res := glob(t, dir, Input{"pattern": "*", "path": outside}); !res.IsError {
		t.Fatalf("glob outside cwd = %q, want an error", res.Data)
	}
}
