package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/uiout"
)

// A finished edit shows "+N −M" and carries its diff for /x.
func TestEditBlockCarriesDiff(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("package a\n\nfunc A() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng := newTestEngine(&mockProvider{})
	capture := uiout.NewCapture()
	eng.SetOutput(capture)

	before, ok := readForDiff(path)
	if !ok {
		t.Fatal("readForDiff failed")
	}
	if err := os.WriteFile(path, []byte("package a\n\nfunc A() int { return 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng.recordFileDiff("call-1", path, before)
	eng.emitToolResult("call-1", "edit", map[string]any{"filePath": path}, "Edited a.go: 1 replacement(s)", false, time.Millisecond)

	bs := capture.Blocks()
	if len(bs) != 1 || bs[0].Summary != "+1 −1" || !bs[0].Diff || !strings.Contains(bs[0].Full, "+func A() int { return 2 }") {
		t.Fatalf("block = %+v", bs)
	}
	if _, left := eng.takeFileDiff("call-1"); left {
		t.Fatal("the diff was not consumed")
	}
}

// A shell command that exited non-zero is shown failed, with its code.
func TestShellBlockShowsNonZeroExit(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	capture := uiout.NewCapture()
	eng.SetOutput(capture)
	eng.emitToolResult("c", "bash", map[string]any{"command": "go test ./..."}, "Command: run tests\n--- FAIL: TestX\nFAIL\n[exit code: 1]", false, time.Second)
	eng.emitToolResult("d", "bash", map[string]any{"command": "ls"}, "a\nb", false, time.Millisecond)
	eng.emitToolResult("e", "bash", map[string]any{"command": "gh pr list"}, "[stderr]\n/usr/bin/bash: line 1: gh: command not found\n[exit code: 127]", false, time.Second)
	eng.emitToolResult("f", "agent", map[string]any{"description": "check"}, "[exit: error, steps: 0, truncated: no]\nSub-agent did not finish: API error 503", false, time.Second)
	bs := capture.Blocks()
	if !bs[2].IsError || !strings.Contains(bs[2].Summary, "退出码 127 · /usr/bin/bash: line 1: gh: command not found") {
		t.Fatalf("stderr marker shown instead of the error: %+v", bs[2])
	}
	if !bs[3].IsError || !strings.HasPrefix(bs[3].Summary, "Sub-agent did not finish") {
		t.Fatalf("failed sub-agent shown as a success: %+v", bs[3])
	}
	if !bs[0].IsError || !strings.HasPrefix(bs[0].Summary, "退出码 1 · --- FAIL: TestX") {
		t.Fatalf("failing command block = %+v", bs[0])
	}
	if bs[1].IsError {
		t.Fatalf("passing command marked failed: %+v", bs[1])
	}
}

func TestPreviewFileChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, ok := PreviewFileChange("edit", map[string]any{"filePath": "b.txt", "oldString": "two", "newString": "TWO"}, dir)
	if !ok || d.Added != 1 || d.Removed != 1 {
		t.Fatalf("edit preview %+v %v", d, ok)
	}
	d, ok = PreviewFileChange("write", map[string]any{"filePath": "new.txt", "content": "x\n"}, dir)
	if !ok || d.Added != 1 {
		t.Fatalf("write preview of a new file %+v %v", d, ok)
	}
	if _, ok := PreviewFileChange("edit", map[string]any{"filePath": "b.txt", "oldString": "absent", "newString": "x"}, dir); ok {
		t.Fatal("an edit whose text is not in the file got a preview")
	}
	if _, ok := PreviewFileChange("bash", map[string]any{"command": "ls"}, dir); ok {
		t.Fatal("bash got a file preview")
	}
}
