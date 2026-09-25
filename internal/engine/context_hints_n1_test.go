package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	ctxt "github.com/liuzhixin405/cove/internal/context"
)

// grep and glob name the directory they search with "path"; that touches the
// directory as much as a read does, so its AGENTS.md is shown too.
func TestSubdirHintsTriggeredByGrepAndGlobPath(t *testing.T) {
	work := t.TempDir()
	sub := filepath.Join(work, "svc")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "AGENTS.md"), []byte("svc rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"grep", "glob"} {
		e := &Engine{subdirHints: ctxt.NewSubdirHints(work)}
		got := e.toolContextHints(api.ToolCall{Name: name, Input: map[string]any{"pattern": "x", "path": sub}})
		if !strings.Contains(got, "svc rules") {
			t.Errorf("%s path did not trigger the hint: %q", name, got)
		}
	}
}

// Compaction may summarize injected hints and file-type skills away, so
// both are shown again afterwards.
func TestCompactionResetsShownHintsAndSkills(t *testing.T) {
	work := t.TempDir()
	sub := filepath.Join(work, "svc")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "AGENTS.md"), []byte("svc rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	prov := &mockProvider{responses: []mockResponse{{content: strings.Repeat("summary of svc/a.go work ", 10)}}}
	eng := newTestEngine(prov)
	eng.subdirHints = ctxt.NewSubdirHints(work)
	eng.injectedSkills = map[string]bool{"go": true}
	tc := api.ToolCall{Name: "read", Input: map[string]any{"filePath": filepath.Join(sub, "a.go")}}
	if eng.toolContextHints(tc) == "" {
		t.Fatal("first read: no hint")
	}
	if eng.toolContextHints(tc) != "" {
		t.Fatal("second read: hint repeated")
	}
	eng.messages = longHistory(20)
	eng.compactIfNeeded(context.Background(), 0)
	if len(eng.injectedSkills) != 0 {
		t.Fatalf("injectedSkills not reset: %v", eng.injectedSkills)
	}
	if !strings.Contains(eng.toolContextHints(tc), "svc rules") {
		t.Fatal("after compaction: hint not shown again")
	}
}

// longHistory is n alternating user/assistant messages.
func longHistory(n int) []api.Message {
	var msgs []api.Message
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, api.Message{Role: role, Content: strings.Repeat("word ", 50)})
	}
	return msgs
}
