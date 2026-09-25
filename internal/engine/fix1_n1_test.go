package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/memory"
)

// WaitBackground (what cove -p calls before exiting) does not wait for the
// skill review, which may take up to its 30-second timeout.
func TestWaitBackgroundDoesNotWaitForReview(t *testing.T) {
	prov := &mockProvider{responses: append(workTurnResponses(), mockResponse{content: "SKILL: x | y | z", delay: 3 * time.Second})}
	eng := workReviewEngine(t, prov)
	armReview(eng)
	if _, err := run(t, eng, "question"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	eng.WaitBackground(context.Background())
	if el := time.Since(start); el > time.Second {
		t.Fatalf("WaitBackground waited %v for the review", el)
	}
	eng.waitReview()
}

// memoryDirEngine is an engine whose memory store reads dir only.
func memoryDirEngine(t *testing.T, files map[string]string) *Engine {
	t.Helper()
	isolateHome(t)
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Engine{memStore: memory.NewStoreForDirs(dir)}
}

// bigMemories is enough memory to leave full-inline mode, with one entry per
// topic so a query ranks a known file first.
func bigMemories() map[string]string {
	files := map[string]string{}
	for i := 0; i < 30; i++ {
		files[fmt.Sprintf("filler-%02d.md", i)] = strings.Repeat(fmt.Sprintf("unrelated filler text %d ", i), 45)
	}
	files["deploy.md"] = "kubernetes deploy steps: helm upgrade with the staging values"
	return files
}

// A relevant memory is added to the turn note once per session.
func TestRelevantMemoriesInjectedOnce(t *testing.T) {
	e := memoryDirEngine(t, bigMemories())
	n1 := e.turnMemoryNote("how do I deploy to kubernetes with helm")
	if !strings.Contains(n1, "helm upgrade with the staging values") {
		t.Fatalf("first turn: relevant memory missing:\n%s", n1)
	}
	n2 := e.turnMemoryNote("deploy kubernetes helm again")
	if strings.Contains(n2, "helm upgrade with the staging values") {
		t.Fatalf("second turn repeated the memory:\n%s", n2)
	}
	// Compaction forgets what was shown.
	e.clearNewMemories()
	if n3 := e.turnMemoryNote("deploy kubernetes helm"); !strings.Contains(n3, "staging values") {
		t.Fatal("after compaction the memory was not shown again")
	}
}

// Relevant memories are capped at 4KB per turn, cut at entry boundaries.
func TestRelevantMemoriesCappedAtEntryBoundary(t *testing.T) {
	files := bigMemories()
	for i := 0; i < 6; i++ {
		files[fmt.Sprintf("helm-%d.md", i)] = fmt.Sprintf("helm note %d END%d ", i, i) + strings.Repeat("helm chart values ", 60)
	}
	e := memoryDirEngine(t, files)
	note := e.turnMemoryNote("helm chart values")
	rel := note
	if i := strings.Index(rel, "<relevant_memories>"); i >= 0 {
		rel = rel[i:]
	}
	if len(rel) > relevantMemoryNoteMaxBytes+len("<relevant_memories>\n</relevant_memories>") {
		t.Fatalf("relevant block is %d bytes", len(rel))
	}
	if strings.Count(rel, "<memory>") != strings.Count(rel, "</memory>") || strings.Count(rel, "<memory>") == 0 {
		t.Fatalf("block not cut at entry boundaries:\n%s", rel)
	}
	if len(note) > turnMemoryNoteMaxBytes {
		t.Fatalf("note is %d bytes, cap %d", len(note), turnMemoryNoteMaxBytes)
	}
}

// With every memory already inlined in the system prompt nothing is added.
func TestRelevantMemoriesSkippedInFullMode(t *testing.T) {
	e := memoryDirEngine(t, map[string]string{"deploy.md": "kubernetes deploy steps: helm upgrade"})
	if note := e.turnMemoryNote("deploy kubernetes helm"); note != "" {
		t.Fatalf("full-inline mode injected:\n%s", note)
	}
}

// New memories and relevant ones share one note.
func TestTurnMemoryNoteMergesBoth(t *testing.T) {
	e := memoryDirEngine(t, bigMemories())
	e.addNewMemories([]string{"x.md: learned just now"})
	note := e.turnMemoryNote("kubernetes helm deploy")
	if !strings.Contains(note, "learned just now") || !strings.Contains(note, "staging values") {
		t.Fatalf("note:\n%s", note)
	}
	if strings.Count(note, "<turn_memories>") != 1 {
		t.Fatalf("want one merged block:\n%s", note)
	}
}
