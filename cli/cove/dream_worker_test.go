package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/dream"
	"github.com/liuzhixin405/cove/internal/hooks"
)

// spawnRecorder replaces the detached-worker spawn for a test and records
// the arguments of every call; err is what the spawn reports.
type spawnRecorder struct {
	calls [][]string
	err   error
}

func stubDreamSpawn(t *testing.T, err error) *spawnRecorder {
	t.Helper()
	rec := &spawnRecorder{err: err}
	oldSpawn, oldNotice, oldBudget := dreamSpawn, dreamNotice, dreamInlineBudget
	dreamSpawn = func(args []string) (int, error) {
		rec.calls = append(rec.calls, append([]string(nil), args...))
		if rec.err != nil {
			return 0, rec.err
		}
		return 4242, nil
	}
	dreamNotice = func(string) {}
	resetTurnsCompleted()
	t.Cleanup(func() {
		dreamSpawn, dreamNotice, dreamInlineBudget = oldSpawn, oldNotice, oldBudget
		resetTurnsCompleted()
	})
	return rec
}

func twoTurnMessages() []api.Message {
	return []api.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "t", Name: "read"}}},
		{Role: "tool", ToolCallID: "t", Content: "x"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
	}
}

// The exit path starts the worker once the session had two assistant turns.
func TestSessionEndSpawnsDreamWorker(t *testing.T) {
	eng := newTestEngine(t)
	resetSessionEnd(t, hooks.NewManager())
	rec := stubDreamSpawn(t, nil)
	eng.LoadMessages(twoTurnMessages())
	noteTurnCompleted()
	noteTurnCompleted()

	finishSession(eng, nil)

	if len(rec.calls) != 1 {
		t.Fatalf("spawn called %d times, want 1", len(rec.calls))
	}
	args := rec.calls[0]
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".cove", "sessions")
	if len(args) < 2 || args[len(args)-2] != dream.WorkerFlag || args[len(args)-1] != want {
		t.Fatalf("spawn args = %q, want [... %s %s]", args, dream.WorkerFlag, want)
	}
	// SessionEnd fires once per process: the second call must not spawn again.
	fireSessionEnd(eng)
	if len(rec.calls) != 1 {
		t.Fatalf("spawn called %d times after a second fireSessionEnd", len(rec.calls))
	}
}

func TestSessionEndSkipsDreamForOneTurn(t *testing.T) {
	eng := newTestEngine(t)
	resetSessionEnd(t, hooks.NewManager())
	rec := stubDreamSpawn(t, nil)
	eng.LoadMessages([]api.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}})
	noteTurnCompleted()

	finishSession(eng, nil)

	if len(rec.calls) != 0 {
		t.Fatalf("spawn called for a one-turn session: %q", rec.calls)
	}
}

func TestSessionEndSkipsDreamWithNoAuto(t *testing.T) {
	eng := newTestEngine(t)
	resetSessionEnd(t, hooks.NewManager())
	rec := stubDreamSpawn(t, nil)
	old := noAuto
	noAuto = true
	t.Cleanup(func() { noAuto = old })
	eng.LoadMessages(twoTurnMessages())
	noteTurnCompleted()
	noteTurnCompleted()

	finishSession(eng, nil)

	if len(rec.calls) != 0 {
		t.Fatal("spawn called with --no-auto")
	}
}

// hangingProvider never answers until its context ends.
type hangingProvider struct{}

func (hangingProvider) Name() string        { return "hang" }
func (hangingProvider) DisplayName() string { return "hang" }
func (hangingProvider) Validate() error     { return nil }
func (hangingProvider) Chat(ctx context.Context, _ api.ChatRequest) (*api.ChatResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (p hangingProvider) ChatStream(ctx context.Context, req api.ChatRequest, _ api.StreamHandler) (*api.ChatResponse, error) {
	return p.Chat(ctx, req)
}

// When the worker cannot be started the run happens inline, bounded by the
// budget, and the lock is rolled back when it runs out.
func TestSessionEndInlineFallbackRespectsBudget(t *testing.T) {
	newTestEngine(t)
	stubDreamSpawn(t, errors.New("no fork for you"))
	dreamInlineBudget = 200 * time.Millisecond
	home, _ := os.UserHomeDir()
	sessions := filepath.Join(home, ".cove", "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "s.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := dream.NewRunner(hangingProvider{}, "m", "s")

	start := time.Now()
	startSessionEndDream(2, r)
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("inline fallback took %v", el)
	}
	if _, err := os.Stat(filepath.Join(home, ".cove", "memory", ".consolidate-lock")); !os.IsNotExist(err) {
		t.Fatalf("lock not rolled back after the inline budget (stat err %v)", err)
	}
	if lr, _ := dream.ReadLastRun(); lr.Mode != "inline" || lr.Result != dream.ResultFailed {
		t.Fatalf("last run = %+v", lr)
	}
}

func TestParseDreamWorkerFlag(t *testing.T) {
	opts, err := parseCLIArgs([]string{"--profile", "p", dream.WorkerFlag, "/tmp/s"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.action != actionDreamWorker || opts.dreamWorkerDir != "/tmp/s" || opts.profile != "p" {
		t.Fatalf("opts = %+v", opts)
	}
	if _, err := parseCLIArgs([]string{dream.WorkerFlag}); err == nil {
		t.Fatal("--dream-worker without a directory parsed")
	}
}

// scriptedProvider finishes a dream run on its first call.
type scriptedProvider struct{ hangingProvider }

func (scriptedProvider) Chat(context.Context, api.ChatRequest) (*api.ChatResponse, error) {
	return &api.ChatResponse{Content: "nothing to change"}, nil
}

// Worker mode: one consolidation with the configured provider, recorded in
// dream-last.json in the config directory, exit code 0.
func TestRunDreamWorkerRecordsLastRun(t *testing.T) {
	newTestEngine(t)
	old := dreamWorkerProvider
	dreamWorkerProvider = func(string) (api.Provider, string, error) { return scriptedProvider{}, "m", nil }
	t.Cleanup(func() { dreamWorkerProvider = old })
	sessions := t.TempDir()
	if err := os.WriteFile(filepath.Join(sessions, "s.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runDreamWorker(sessions, "", ""); code != 0 {
		t.Fatalf("runDreamWorker = %d", code)
	}
	lr, err := dream.ReadLastRun()
	if err != nil || lr.Mode != "worker" || lr.Result != dream.ResultCompleted || lr.SessionsReviewed != 1 {
		t.Fatalf("last run = %+v, %v", lr, err)
	}
	cfgDir := os.Getenv("COVE_CONFIG_DIR")
	if _, err := os.Stat(filepath.Join(cfgDir, "dream-last.json")); err != nil {
		t.Fatalf("dream-last.json: %v", err)
	}
	if !strings.HasPrefix(dream.LogPath(), cfgDir) {
		t.Fatalf("LogPath %q not under %q", dream.LogPath(), cfgDir)
	}
}

// Fix round 1 (item 1): the turns counted are the ones this process ran. A
// resumed session exited straight away ran none, however long its history.
func TestSessionEndResumedWithoutTurnsDoesNotSpawn(t *testing.T) {
	eng := newTestEngine(t)
	resetSessionEnd(t, hooks.NewManager())
	rec := stubDreamSpawn(t, nil)
	eng.LoadMessages(twoTurnMessages()) // the resumed history

	finishSession(eng, nil)

	if len(rec.calls) != 0 {
		t.Fatalf("spawn called for a resumed session with no new turn: %q", rec.calls)
	}
}

// Fix round 1 (item 1): a compacted history (summary + tail) holds one
// assistant reply, but the three turns this process ran count.
func TestSessionEndCountsTurnsAfterCompaction(t *testing.T) {
	eng := newTestEngine(t)
	resetSessionEnd(t, hooks.NewManager())
	rec := stubDreamSpawn(t, nil)
	eng.LoadMessages([]api.Message{
		{Role: "user", Content: "[summary of the earlier conversation]"},
		{Role: "user", Content: "q3"},
		{Role: "assistant", Content: "a3"},
	})
	for range 3 {
		noteTurnCompleted()
	}

	finishSession(eng, nil)

	if len(rec.calls) != 1 {
		t.Fatalf("spawn called %d times after 3 turns in a compacted session, want 1", len(rec.calls))
	}
}

// Fix round 1 (item 1): the interactive turn path counts a turn that
// finished, not one that failed.
func TestChatInteractionCountsCompletedTurns(t *testing.T) {
	resetTurnsCompleted()
	t.Cleanup(resetTurnsCompleted)
	_ = captureOut(t)
	if _, err := runChatInteraction(context.Background(), fakeChatRunner{reply: "ok"}, "hi"); err != nil {
		t.Fatal(err)
	}
	_, _ = runChatInteraction(context.Background(), fakeChatRunner{err: errors.New("bad request")}, "hi")
	if got := completedTurns(); got != 1 {
		t.Fatalf("completedTurns = %d, want 1 (the failed turn does not count)", got)
	}
}

type fakeChatRunner struct {
	reply string
	err   error
}

func (f fakeChatRunner) RunWithStream(_ context.Context, _ string, onDelta func(string)) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	onDelta(f.reply)
	return f.reply, nil
}

// Fix round 1 (item 3): the worker starts after the SessionEnd hooks have
// run, so a hook that writes to the session or memory is seen by it.
func TestSessionEndSpawnsAfterHooks(t *testing.T) {
	eng := newTestEngine(t)
	var order []string
	m := hooks.NewManager()
	m.Register(hooks.HookConfig{Event: hooks.SessionEnd, Type: hooks.HookRuntime, Sequential: true,
		RuntimeFn: func(hooks.HookInput) (hooks.HookOutput, error) {
			order = append(order, "hook")
			return hooks.HookOutput{Continue: true}, nil
		}})
	resetSessionEnd(t, m)
	oldNotice := sessionEndNotice
	sessionEndNotice = func(string) {}
	t.Cleanup(func() { sessionEndNotice = oldNotice })
	stubDreamSpawn(t, nil)
	dreamSpawn = func([]string) (int, error) {
		order = append(order, "spawn")
		return 1, nil
	}
	eng.LoadMessages(twoTurnMessages())
	noteTurnCompleted()
	noteTurnCompleted()

	finishSession(eng, nil)

	if strings.Join(order, ",") != "hook,spawn" {
		t.Fatalf("order = %v, want the hook before the spawn", order)
	}
}
