package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/dream"
	"github.com/liuzhixin405/cove/internal/hooks"
)

type stuckWaiter struct{}

func (stuckWaiter) WaitBackground(context.Context) { select {} }

// Fix round 1 (item 5): the bound holds even for a WaitBackground that
// ignores its context. (The budget is generous so a loaded CI machine does
// not flake; a stuck waiter would never return at all.)
func TestWaitForBackgroundBoundedAgainstStuckWaiter(t *testing.T) {
	start := time.Now()
	waitForBackground(stuckWaiter{}, 50*time.Millisecond)
	if el := time.Since(start); el > time.Second {
		t.Fatalf("waitForBackground returned after %v", el)
	}
}

// Fix round 1 (item 6): a consolidation still running at exit is cancelled
// and its lock rolled back, so the next start does not treat it as done.
func TestExitCancelsRunningDream(t *testing.T) {
	eng := newTestEngine(t)
	resetSessionEnd(t, hooks.NewManager())
	prior, ok, err := dream.TryAcquireConsolidationLock()
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	task := dream.NewTask(1, prior, cancel)
	t.Cleanup(func() {
		if task.CurrentStatus() == dream.StatusRunning {
			task.Fail()
		}
	})

	finishSession(eng, nil)

	if ctx.Err() == nil {
		t.Fatal("the running dream was not cancelled at exit")
	}
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(home, ".cove", "memory", ".consolidate-lock")); !os.IsNotExist(err) {
		t.Fatalf("consolidation lock not rolled back: stat err %v", err)
	}
}

// Fix round 1 (item 7): when a SessionEnd hook is configured the exit says
// what it is waiting for (on stderr, so -p's stdout stays the answer).
func TestSessionEndAnnouncesHooks(t *testing.T) {
	eng := newTestEngine(t)
	m := hooks.NewManager()
	m.Register(hooks.HookConfig{Event: hooks.SessionEnd, Type: hooks.HookRuntime, Sequential: true,
		RuntimeFn: func(hooks.HookInput) (hooks.HookOutput, error) { return hooks.HookOutput{Continue: true}, nil }})
	resetSessionEnd(t, m)
	var got strings.Builder
	old := sessionEndNotice
	sessionEndNotice = func(s string) { got.WriteString(s) }
	t.Cleanup(func() { sessionEndNotice = old })

	fireSessionEnd(eng)
	if !strings.Contains(got.String(), "正在运行 SessionEnd hook") {
		t.Fatalf("notice = %q", got.String())
	}

	got.Reset()
	resetSessionEnd(t, hooks.NewManager())
	fireSessionEnd(eng)
	if got.Len() != 0 {
		t.Fatalf("notice printed with no SessionEnd hook: %q", got.String())
	}
}

func resetSessionEnd(t *testing.T, m *hooks.Manager) {
	t.Helper()
	old := sessionEndHooks
	sessionEndHooks = func() *hooks.Manager { return m }
	sessionEndFired.Store(false)
	t.Cleanup(func() { sessionEndHooks = old; sessionEndFired.Store(false); dream.SuppressAuto("") })
}

// Final fix (Important 1): memory extraction finishing while the SessionEnd
// hooks run calls ExecuteAutoDream; exit has already switched automatic runs
// off, so no consolidation starts and the lock is left as it was.
func TestExitSuppressesDreamStartedDuringSessionEnd(t *testing.T) {
	eng := newTestEngine(t)
	home, _ := os.UserHomeDir()
	sessions := filepath.Join(home, ".cove", "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		if err := os.WriteFile(filepath.Join(sessions, id+".jsonl"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lock := filepath.Join(home, ".cove", "memory", ".consolidate-lock")
	var lockTaken, taskStarted bool
	m := hooks.NewManager()
	m.Register(hooks.HookConfig{Event: hooks.SessionEnd, Type: hooks.HookRuntime, Sequential: true,
		RuntimeFn: func(hooks.HookInput) (hooks.HookOutput, error) {
			// what runBackgroundWork does once extraction completes
			if r := dream.Current(); r != nil {
				r.ExecuteAutoDream(context.Background())
			}
			_, err := os.Stat(lock)
			lockTaken = err == nil
			taskStarted = dream.ActiveTask() != nil
			return hooks.HookOutput{Continue: true}, nil
		}})
	resetSessionEnd(t, m)
	t.Cleanup(func() { dream.CancelActive() })

	fireSessionEnd(eng)

	if taskStarted || lockTaken {
		t.Fatalf("a dream started during the SessionEnd wait (task %v, lock %v)", taskStarted, lockTaken)
	}
	if dream.ActiveTask() != nil {
		t.Fatal("a dream is still running after exit")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("lock timestamp changed at exit: stat err %v", err)
	}
}

// Final fix (Important 1): a consolidation that got past the gate just before
// exit suppressed it is cancelled after the hooks return, lock rolled back.
func TestExitCancelsDreamStartedDuringSessionEnd(t *testing.T) {
	eng := newTestEngine(t)
	var ctx context.Context
	var task *dream.Task
	m := hooks.NewManager()
	m.Register(hooks.HookConfig{Event: hooks.SessionEnd, Type: hooks.HookRuntime, Sequential: true,
		RuntimeFn: func(hooks.HookInput) (hooks.HookOutput, error) {
			prior, ok, err := dream.TryAcquireConsolidationLock()
			if err != nil || !ok {
				t.Errorf("lock: %v %v", ok, err)
			}
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(context.Background())
			task = dream.NewTask(1, prior, cancel)
			return hooks.HookOutput{Continue: true}, nil
		}})
	resetSessionEnd(t, m)
	t.Cleanup(func() {
		if task != nil && task.CurrentStatus() == dream.StatusRunning {
			task.Fail()
		}
	})

	fireSessionEnd(eng)

	if ctx == nil || ctx.Err() == nil {
		t.Fatal("the dream started during the SessionEnd wait was not cancelled")
	}
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(home, ".cove", "memory", ".consolidate-lock")); !os.IsNotExist(err) {
		t.Fatalf("consolidation lock not rolled back: stat err %v", err)
	}
}
