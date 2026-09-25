package main

import (
	"context"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/config"
	"github.com/liuzhixin405/cove/internal/dream"
	"github.com/liuzhixin405/cove/internal/hooks"
)

type fakeWaiter struct {
	calls    int
	deadline time.Time
}

func (f *fakeWaiter) WaitBackground(ctx context.Context) {
	f.calls++
	f.deadline, _ = ctx.Deadline()
}

// -p waits for the engine's background work (memory extraction) before it
// exits, bounded by printModeBackgroundWait.
func TestWaitForBackgroundIsBounded(t *testing.T) {
	w := &fakeWaiter{}
	start := time.Now()
	if !waitForBackground(w, printModeBackgroundWait) {
		t.Fatal("a WaitBackground implementation was not used")
	}
	if w.calls != 1 {
		t.Fatalf("WaitBackground called %d times", w.calls)
	}
	if w.deadline.IsZero() || w.deadline.Sub(start) > printModeBackgroundWait+time.Second {
		t.Fatalf("deadline %v is not within %v", w.deadline, printModeBackgroundWait)
	}
	if printModeBackgroundWait != 20*time.Second {
		t.Fatalf("printModeBackgroundWait = %v, want 20s", printModeBackgroundWait)
	}
}

// Until the engine implements WaitBackground the wait is skipped.
func TestWaitForBackgroundSkipsWithoutWaiter(t *testing.T) {
	if waitForBackground(struct{}{}, time.Second) || waitForBackground(nil, time.Second) {
		t.Fatal("waited on a value without WaitBackground")
	}
}

// Under -p automatic dream is switched off for the process (it would be
// killed at exit with the lock stamped as done).
func TestRunPrintModeSessionSuppressesDream(t *testing.T) {
	eng := newTestEngine(t)
	t.Cleanup(func() { dream.SuppressAuto("") })
	// runPrintModeSession ends in finishSession, which fires SessionEnd once
	// per process; reset that so later tests can fire it again.
	resetSessionEnd(t, hooks.NewManager())
	code := runPrintModeSession(eng, "/commit", "/commit", false, nil, config.DefaultConfig(), nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 for a refused slash command", code)
	}
	if dream.StatusFromDisk().Suppressed == "" {
		t.Fatal("auto dream is not suppressed under -p")
	}
}

// SessionEnd hooks run on exit, once, with the session ID.
func TestFinishSessionFiresSessionEndOnce(t *testing.T) {
	eng := newTestEngine(t)
	eng.LoadMessages([]api.Message{{Role: "user", Content: "hi"}})
	var got []hooks.HookInput
	m := hooks.NewManager()
	m.Register(hooks.HookConfig{Event: hooks.SessionEnd, Type: hooks.HookRuntime, Sequential: false,
		RuntimeFn: func(in hooks.HookInput) (hooks.HookOutput, error) {
			got = append(got, in)
			return hooks.HookOutput{Continue: true}, nil
		}})
	oldLoad := sessionEndHooks
	sessionEndHooks = func() *hooks.Manager { return m }
	sessionEndFired.Store(false)
	t.Cleanup(func() { sessionEndHooks = oldLoad; sessionEndFired.Store(false) })

	finishSession(eng, nil)
	fireSessionEnd(eng) // the TUI exit path calls it again after autoSaveSession

	if len(got) != 1 {
		t.Fatalf("SessionEnd fired %d times, want 1", len(got))
	}
	if got[0].Event != hooks.SessionEnd || got[0].SessionID != eng.SessionID() || got[0].Cwd == "" {
		t.Fatalf("hook input = %+v", got[0])
	}
}

// An empty session still ends: SessionEnd pairs with SessionStart, which
// fires for every engine.
func TestFireSessionEndForEmptySession(t *testing.T) {
	eng := newTestEngine(t)
	n := 0
	m := hooks.NewManager()
	m.Register(hooks.HookConfig{Event: hooks.SessionEnd, Type: hooks.HookRuntime, Sequential: true,
		RuntimeFn: func(hooks.HookInput) (hooks.HookOutput, error) { n++; return hooks.HookOutput{Continue: true}, nil }})
	oldLoad := sessionEndHooks
	sessionEndHooks = func() *hooks.Manager { return m }
	sessionEndFired.Store(false)
	t.Cleanup(func() { sessionEndHooks = oldLoad; sessionEndFired.Store(false) })

	finishSession(eng, nil)
	if n != 1 {
		t.Fatalf("SessionEnd fired %d times for an empty session, want 1", n)
	}
}
