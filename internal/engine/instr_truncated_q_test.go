package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/memory"
)

// Project instruction files over memory.MaxInstructionBytes are clipped; the
// user is told once per session when the system prompt is built.
func TestInstructionFilesTruncatedNoticeOnce(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	big := strings.Repeat("rule line for the project\n", memory.MaxInstructionBytes/20)
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	eng := newTestEngine(&mockProvider{})
	eng.memStore = memory.NewStoreForDirs(t.TempDir())
	var lines []string
	eng.OnEngineOutput = func(s string) { lines = append(lines, s) }

	for i := 0; i < 2; i++ {
		eng.systemPrompt = ""
		_ = eng.SystemPrompt()
	}
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "项目指令文件超过 32KB，已截断（保留前 32KB）") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("truncation notice shown %d times, want 1; output: %q", n, lines)
	}
}

func TestInstructionFilesNotTruncatedNoNotice(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("small rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	eng := newTestEngine(&mockProvider{})
	eng.memStore = memory.NewStoreForDirs(t.TempDir())
	var lines []string
	eng.OnEngineOutput = func(s string) { lines = append(lines, s) }
	eng.systemPrompt = ""
	_ = eng.SystemPrompt()
	for _, l := range lines {
		if strings.Contains(l, "已截断") {
			t.Fatalf("notice shown for small instruction files: %q", l)
		}
	}
}
