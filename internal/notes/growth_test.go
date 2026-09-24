package notes

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestFlushOverCapKeepsNewestEntries: the notes file is per project and every
// session Loads it, adds to it and writes it back, so it only grows. At the
// 25KB cap the rendered file was clipped at the end — the newest entries — and
// the "[truncated]" marker was skipped on the next Load. From then on every new
// note was thrown away on write while the oldest ones stayed forever.
func TestFlushOverCapKeepsNewestEntries(t *testing.T) {
	s, projectDir := newTestNotes(t)
	line := strings.Repeat("会话笔记内容", 17) // 306 bytes
	for i := 0; i < 250; i++ {
		s.AddTask(fmt.Sprintf("entry-%03d %s", i, line))
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	path := filepath.Join(projectDir, ".cove", "session_notes.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "entry-249 ") {
		t.Fatal("the newest entry was dropped from the notes file")
	}
	if len(data) > 25600 {
		t.Fatalf("notes file is %d bytes, cap is 25600", len(data))
	}
	if !utf8.Valid(data) {
		t.Fatal("notes file is not valid UTF-8")
	}
	if strings.Contains(string(data), "entry-000 ") {
		t.Fatal("the oldest entry was kept although the file is over the cap")
	}

	// The next session loads the file and keeps adding: its new note must
	// survive the next write.
	next := New(projectDir)
	next.Load()
	next.AddDecision("after-reload decision")
	if err := next.Flush(); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), "after-reload decision") {
		t.Fatal("a note added after reloading a full notes file was lost on write")
	}
	if len(data) > 25600 {
		t.Fatalf("notes file grew to %d bytes", len(data))
	}
}
