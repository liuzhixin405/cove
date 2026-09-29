package engine

import (
	"context"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/session"
)

func TestNewSessionSavesTheOldOneAndStartsEmpty(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{responses: []mockResponse{{content: "first answer"}, {content: "second answer"}}})
	eng.store = mustSessionStore(t)
	eng.session = &session.Record{ID: "session-old", Title: "New session"}

	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "fix the parser"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	oldCount := len(eng.Messages())

	saved := eng.NewSession(context.Background())
	if saved != "session-old" {
		t.Fatalf("saved = %q, want session-old", saved)
	}
	if eng.HasMessages() {
		t.Fatalf("conversation not emptied: %d messages", len(eng.Messages()))
	}
	if eng.Session() == nil || eng.Session().ID == "session-old" {
		t.Fatalf("no new session ID: %+v", eng.Session())
	}
	if _, ok := eng.InterruptedTurn(); ok {
		t.Fatal("the old conversation's interrupted turn survived /new")
	}

	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "something else"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	old, err := eng.store.Load("session-old")
	if err != nil {
		t.Fatalf("old session not saved: %v", err)
	}
	if len(old.Messages) != oldCount {
		t.Fatalf("old session has %d messages, want %d (the new turn went into it)", len(old.Messages), oldCount)
	}
	for _, m := range eng.Messages() {
		if m.Content == "fix the parser" {
			t.Fatal("the new session still carries the old conversation")
		}
	}
}

func TestNewSessionWithNothingToSave(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	eng.store = mustSessionStore(t)
	eng.session = &session.Record{ID: "session-empty"}

	if saved := eng.NewSession(context.Background()); saved != "" {
		t.Fatalf("saved = %q for an empty conversation", saved)
	}
	if _, err := eng.store.Load("session-empty"); err == nil {
		t.Fatal("an empty session was written to history")
	}
}

// Resuming kept the previous conversation's interrupted turn (so /continue
// resumed a request the loaded history had never seen), did not save the
// current session, and overwrote the resumed record's tokens with the
// process's totals.
func TestResumeSessionSwitchesLikeNewSession(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{responses: []mockResponse{{content: "a answer"}, {content: "b answer"}}})
	eng.store = mustSessionStore(t)
	eng.session = &session.Record{ID: "session-a", Title: "New session"}
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "work on a"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	eng.interrupted = &interruption{user: api.Message{Role: "user", Content: "work on a"}, reason: "test"}
	eng.bgMu.Lock()
	eng.lastReviewMsgCount = 40
	eng.bgMu.Unlock()
	eng.costTracker.AddWithCacheWrite("test-model", 1000, 500, 0, 0, 0)

	b := &session.Record{ID: "session-b", Title: "b", TokensIn: 300, TokensOut: 100,
		Messages: []api.Message{{Role: "user", Content: "b request"}, {Role: "assistant", Content: "b reply"}}}
	eng.ResumeSession(b)

	if _, ok := eng.InterruptedTurn(); ok {
		t.Fatal("session a's interrupted turn survived resuming b")
	}
	if eng.lastReviewMsgCount != 0 {
		t.Fatalf("review throttle kept a's message count: %d", eng.lastReviewMsgCount)
	}
	if _, err := eng.store.Load("session-a"); err != nil {
		t.Fatalf("session a was not saved before switching: %v", err)
	}

	eng.costTracker.AddWithCacheWrite("test-model", 70, 30, 0, 0, 0)
	eng.saveSession()
	got, err := eng.store.Load("session-b")
	if err != nil {
		t.Fatal(err)
	}
	if got.TokensIn != 300+70 || got.TokensOut != 100+30 {
		t.Fatalf("resumed session tokens = %d/%d, want its own 300/100 plus the 70/30 spent since", got.TokensIn, got.TokensOut)
	}
}

// Resuming the session already in use must not replace newer in-memory
// messages with the older copy on disk.
func TestResumeSessionOfTheCurrentSessionKeepsMemory(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{responses: []mockResponse{{content: "answer"}}})
	eng.store = mustSessionStore(t)
	eng.session = &session.Record{ID: "session-cur", Title: "New session"}
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "hello"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	n := len(eng.Messages())
	eng.ResumeSession(&session.Record{ID: "session-cur", Messages: []api.Message{{Role: "user", Content: "stale"}}})
	if len(eng.Messages()) != n {
		t.Fatalf("messages = %d after resuming the current session, want %d", len(eng.Messages()), n)
	}
}

// After a switch to another provider, background bookkeeping kept asking for
// the model cove had started with.
func TestReloadProviderMovesTheBackgroundModel(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	if err := eng.ReloadProvider("openai", "new-main-model", "http://127.0.0.1:1", "k"); err != nil {
		t.Fatal(err)
	}
	if got := eng.reviewRequest("用户: hi").Model; got != "new-main-model" {
		t.Fatalf("review model = %q after the switch, want new-main-model", got)
	}
}
