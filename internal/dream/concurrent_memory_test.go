package dream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// appendFile stands in for a turn-end extraction appending to a memory.
func appendFile(t *testing.T, path, text string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("\n"+text)...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Dream's write tool replaced a memory with the copy the model read minutes
// earlier, so a fact extraction appended meanwhile was lost. A write over a
// file that changed since dream last read it is now refused.
func TestDreamWriteRefusesFileChangedSinceRead(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "notes.md")
	if err := os.WriteFile(target, []byte("old fact"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Runner{memoryRoot: root}
	r.snapshotMemory() // what runDream does when the run starts

	if got := r.executeDreamRead(api.ToolCall{Input: map[string]any{"filePath": target}}); got != "old fact" {
		t.Fatalf("read = %q", got)
	}
	appendFile(t, target, "fact extracted meanwhile")

	out := r.executeDreamWrite(api.ToolCall{Input: map[string]any{"filePath": target, "content": "old fact, reworded"}})
	if !strings.HasPrefix(out, "Error") {
		t.Fatalf("write over a concurrently changed memory accepted: %q", out)
	}
	if data, _ := os.ReadFile(target); !strings.Contains(string(data), "fact extracted meanwhile") {
		t.Fatalf("the concurrently extracted fact was lost: %q", data)
	}

	// Once the model has read the current content, the write goes through.
	r.executeDreamRead(api.ToolCall{Input: map[string]any{"filePath": target}})
	out = r.executeDreamWrite(api.ToolCall{Input: map[string]any{"filePath": target, "content": "merged: old fact; fact extracted meanwhile"}})
	if strings.HasPrefix(out, "Error") {
		t.Fatalf("write after a fresh read refused: %q", out)
	}
}

// A memory created after the run started (by extraction) must not be
// replaced by a dream write that never read it.
func TestDreamWriteRefusesFileCreatedDuringRun(t *testing.T) {
	root := t.TempDir()
	r := &Runner{memoryRoot: root}
	r.snapshotMemory()
	target := filepath.Join(root, "new.md")
	if err := os.WriteFile(target, []byte("extracted during the run"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := r.executeDreamWrite(api.ToolCall{Input: map[string]any{"filePath": target, "content": "dream's own"}})
	if !strings.HasPrefix(out, "Error") {
		t.Fatalf("write over a memory created during the run accepted: %q", out)
	}
}

// The stale-reference check read every memory, walked the project, then
// wrote each memory back from the copy it read first: an append that landed
// during the walk was erased.
func TestStaleMarkerKeepsAppendDuringSearch(t *testing.T) {
	proj, mem := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(mem, "arch.md")
	if err := os.WriteFile(target, []byte("The entry is `runGoneFunction`."), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := searchProjectFn
	t.Cleanup(func() { searchProjectFn = orig })
	searchProjectFn = func(root string, names, symbols map[string]bool) (map[string]bool, map[string]bool) {
		appendFile(t, target, "fact extracted during the check")
		return orig(root, names, symbols)
	}

	reports := markStaleMemories(proj, mem, time.Now())
	if len(reports) != 1 {
		t.Fatalf("reports = %+v, want the one stale memory", reports)
	}
	data, _ := os.ReadFile(target)
	if !strings.Contains(string(data), "fact extracted during the check") {
		t.Fatalf("the append made during the check was lost:\n%s", data)
	}
	if !strings.Contains(string(data), staleMarkerPrefix) {
		t.Fatalf("stale marker missing:\n%s", data)
	}
}
