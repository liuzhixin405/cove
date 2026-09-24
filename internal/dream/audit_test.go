package dream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/liuzhixin405/cove/internal/api"
)

// "find" is on the read-only allowlist, but its -delete and -exec actions
// write and run arbitrary programs without any shell operator, so the
// metacharacter check never saw them: `find ~ -delete` passed as read-only.
func TestDreamBashRefusesFindActionsThatWriteOrExecute(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep.md")
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Runner{memoryRoot: dir}
	for _, cmd := range []string{
		"find " + dir + " -delete",
		"find " + dir + " -exec rm {} +",
		"find " + dir + " -execdir rm {} +",
		"find " + dir + " -ok rm {} +",
		"find " + dir + " -fprint " + filepath.Join(dir, "out.txt"),
		"find " + dir + " -fls " + filepath.Join(dir, "out.txt"),
		"FIND " + dir + " -DELETE",
	} {
		out := r.executeDreamBash(api.ToolCall{Input: map[string]any{"command": cmd}})
		if !strings.HasPrefix(out, "Error: only read-only") {
			t.Errorf("%q was not refused; got %q", cmd, out)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("a refused find command removed files: %v", err)
	}
}

func TestDreamBashStillAllowsPlainFind(t *testing.T) {
	if err := dreamBashAllowed("find . -name *.md -type f"); err != nil {
		t.Fatalf("a plain find was refused: %v", err)
	}
}

// The consolidation prompt told the agent to run `grep ... | tail -50`, which
// the bash tool refuses (pipes are blocked), so every dream spent turns on a
// command it could never run. Every command the prompt suggests must pass.
func TestConsolidationPromptOnlySuggestsAllowedCommands(t *testing.T) {
	prompt := BuildConsolidationPrompt("/mem", "/sessions", []string{"s1"})
	suggested := 0
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "grep ") && !strings.HasPrefix(line, "ls ") && !strings.HasPrefix(line, "find ") {
			continue
		}
		suggested++
		if err := dreamBashAllowed(line); err != nil {
			t.Errorf("prompt suggests %q, which the dream bash tool refuses: %v", line, err)
		}
	}
	if suggested == 0 {
		t.Log("prompt suggests no shell commands")
	}
}

// grepFiles clipped long lines with a byte slice, cutting multi-byte runes in
// half; the invalid UTF-8 then went into the next request body.
func TestGrepFilesClipsLongLinesOnRuneBoundary(t *testing.T) {
	dir := t.TempDir()
	line := "记忆" + strings.Repeat("中文内容", 100) // far past the 400-byte clip
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := grepFiles("记忆", dir)
	if !strings.Contains(got, "记忆") {
		t.Fatalf("grepFiles missed the match: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("grepFiles output is not valid UTF-8: %q", got)
	}
}
