package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A request naming an existing directory outside the working directory
// gets a /cd hint before it is submitted; one inside, or naming nothing
// that exists, does not.
func TestOutsideCwdHintNamesTheDirectoryAndTheCdCommand(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "cove-main")
	other := filepath.Join(root, "agent")
	for _, d := range []string{cwd, other, filepath.Join(cwd, "internal")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	hint := outsideCwdHint(other+" 在该目录写一个netcore的agent框架的项目", cwd)
	if !strings.Contains(hint, other) || !strings.Contains(hint, "/cd "+other) {
		t.Fatalf("hint lacks the directory or the /cd command:\n%q", hint)
	}
	if got := outsideCwdHint("整理 "+filepath.Join(cwd, "internal")+" 下的代码", cwd); got != "" {
		t.Fatalf("a directory inside cwd got a hint: %q", got)
	}
	if got := outsideCwdHint("把 "+filepath.Join(other, "new.cs")+" 写出来", cwd); !strings.Contains(got, other) {
		t.Fatalf("a new file in an existing outside directory should hint with that directory, got %q", got)
	}
	if got := outsideCwdHint(filepath.Join(root, "nope", "x")+" 写个文件", cwd); got != "" {
		t.Fatalf("a path that exists nowhere got a hint: %q", got)
	}
	if got := outsideCwdHint("修复 internal/engine/turn.go 的报错", cwd); got != "" {
		t.Fatalf("a relative path got a hint: %q", got)
	}
}
