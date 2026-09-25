package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/extract"
	"github.com/liuzhixin405/cove/internal/memory"
)

// A memory extracted at the end of one turn reaches the model on the next
// turn as a note after the user message, without rebuilding the system
// prompt (it used to wait for the next compaction or session).
func TestNewMemoriesReachNextTurnNote(t *testing.T) {
	eng, _ := extractingEngine(t, 10*time.Millisecond)
	eng.memStore = memory.NewStore()
	eng.setExtractRunner(extract.NewRunner(&slowExtractProvider{delay: time.Millisecond}, "m"))
	if _, err := run(t, eng, "记住这个"); err != nil {
		t.Fatal(err)
	}
	eng.WaitBackground(context.Background())
	sp := eng.systemPrompt

	if _, err := run(t, eng, "下一个问题"); err != nil {
		t.Fatal(err)
	}
	eng.WaitBackground(context.Background())
	var note string
	for _, m := range eng.messages {
		if m.Synthetic && strings.Contains(m.Content, "<session_memories>") {
			note = m.Content
		}
	}
	if !strings.Contains(note, "fact.md") || !strings.Contains(note, "项目使用 Go 1.25") {
		t.Fatalf("no session-memory note in history; last note %q", note)
	}
	if eng.systemPrompt != sp {
		t.Fatal("the system prompt was rebuilt")
	}
	// Shown once: the third turn carries no second copy.
	if _, err := run(t, eng, "再一个"); err != nil {
		t.Fatal(err)
	}
	eng.WaitBackground(context.Background())
	n := 0
	for _, m := range eng.messages {
		if strings.Contains(m.Content, "<session_memories>") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("session-memory note repeated %d times", n)
	}
}

// The note stays under 2KB however much was learned.
func TestSessionMemoryNoteIsBounded(t *testing.T) {
	e := &Engine{}
	for i := 0; i < 100; i++ {
		e.addNewMemories([]string{"m.md: " + strings.Repeat("长", 150)})
	}
	note := e.takeNewMemoriesNote()
	if len(note) > sessionMemoryNoteMaxBytes {
		t.Fatalf("note is %d bytes, cap %d", len(note), sessionMemoryNoteMaxBytes)
	}
	if e.takeNewMemoriesNote() != "" {
		t.Fatal("note shown twice")
	}
}

func TestDiffNewMemories(t *testing.T) {
	before := map[string]string{"/m/a.md": "old fact", "/m/b.md": "same"}
	after := []memory.Entry{
		{Name: "a.md", Path: "/m/a.md", Content: "old fact\nnew appended line"},
		{Name: "b.md", Path: "/m/b.md", Content: "same"},
		{Name: "c.md", Path: "/m/c.md", Content: "\n\nbrand new\nsecond"},
	}
	got := diffNewMemories(before, after)
	want := []string{"a.md: new appended line", "c.md: brand new"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("diff = %q, want %q", got, want)
	}
}

func TestDiffNewMemoriesSkipsReappendedFact(t *testing.T) {
	before := map[string]string{"/m/a.md": "fact one"}
	after := []memory.Entry{{Name: "a.md", Path: "/m/a.md", Content: "fact one\nfact one"}}
	if got := diffNewMemories(before, after); len(got) != 0 {
		t.Fatalf("re-appended fact reported as new: %q", got)
	}
}
