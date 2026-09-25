package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/session"
)

// A reply that arrives within 10 seconds of the question used to be skipped by
// the save debounce, so closing the terminal (or a crash) before the next turn
// lost the answer from the saved session.
func TestTurnEndSavesTheReply(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	prov := &mockProvider{responses: []mockResponse{{content: "the answer"}}}
	eng := newTestEngine(prov)
	eng.store = mustSessionStore(t)
	if eng.session == nil {
		t.Skip("engine has no session record")
	}
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "question"}, nil, nil); err != nil {
		t.Fatal(err)
	}

	rec, err := eng.store.Load(eng.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := rec.Messages[len(rec.Messages)-1]
	if last.Role != "assistant" || last.Content != "the answer" {
		t.Fatalf("saved session ends with %s %q, want the assistant reply", last.Role, last.Content)
	}
}

// Without the project directory on the record, /history and /resume listed
// every project's conversations together.
func TestSavedSessionRecordsProjectDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	project := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)

	prov := &mockProvider{responses: []mockResponse{{content: "ok"}}}
	eng := newTestEngine(prov)
	eng.store = mustSessionStore(t)
	if eng.session == nil {
		t.Skip("engine has no session record")
	}
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "question"}, nil, nil); err != nil {
		t.Fatal(err)
	}

	rec, err := eng.store.Load(eng.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := session.NormalizeProjectDir(project); rec.Cwd != want {
		t.Fatalf("saved session Cwd = %q, want %q", rec.Cwd, want)
	}
}
