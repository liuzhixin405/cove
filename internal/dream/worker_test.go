package dream

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
)

// fakeProvider answers every Chat from a script; once the script runs out it
// answers with plain text (no tool calls), which ends a dream run. block makes
// Chat wait for the context instead.
type fakeProvider struct {
	script []*api.ChatResponse
	calls  atomic.Int32
	block  bool
	panics bool
	// validateErr is what Validate reports (a missing API key).
	validateErr error
}

func (p *fakeProvider) Name() string        { return "fake" }
func (p *fakeProvider) DisplayName() string { return "fake" }
func (p *fakeProvider) Validate() error     { return p.validateErr }
func (p *fakeProvider) ChatStream(ctx context.Context, req api.ChatRequest, _ api.StreamHandler) (*api.ChatResponse, error) {
	return p.Chat(ctx, req)
}
func (p *fakeProvider) Chat(ctx context.Context, _ api.ChatRequest) (*api.ChatResponse, error) {
	n := int(p.calls.Add(1)) - 1
	if p.panics {
		panic("provider exploded")
	}
	if p.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if n < len(p.script) {
		return p.script[n], nil
	}
	return &api.ChatResponse{Content: "done"}, nil
}

// workerTestEnv points the home and config directories at temp dirs and
// returns the config dir and a sessions dir.
func workerTestEnv(t *testing.T) (cfgDir, sessions string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfgDir = filepath.Join(t.TempDir(), "cfg")
	t.Setenv("COVE_CONFIG_DIR", cfgDir)
	return cfgDir, t.TempDir()
}

func writeDreamJSON(t *testing.T, cfgDir, body string) {
	t.Helper()
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "dream.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigDefaultsToSessionEnd(t *testing.T) {
	workerTestEnv(t)
	cfg := LoadConfig()
	if cfg.Trigger != TriggerSessionEnd || cfg.MinTurns != 2 {
		t.Fatalf("defaults = %+v, want trigger session_end, min_turns 2", cfg)
	}
}

// dream.json is read from the config directory (COVE_CONFIG_DIR aware).
func TestLoadConfigFollowsConfigDir(t *testing.T) {
	cfgDir, _ := workerTestEnv(t)
	writeDreamJSON(t, cfgDir, `{"enabled": true, "trigger": "Threshold", "min_turns": 5}`)
	cfg := LoadConfig()
	if cfg.Trigger != TriggerThreshold || cfg.MinTurns != 5 {
		t.Fatalf("cfg = %+v, want threshold / 5", cfg)
	}
	writeDreamJSON(t, cfgDir, `{"trigger": "sometimes"}`)
	if got := LoadConfig().Trigger; got != TriggerSessionEnd {
		t.Fatalf("unknown trigger -> %q, want the default", got)
	}
}

// In session_end mode the per-turn check never starts a run.
func TestSessionEndModeAutoDreamDoesNotStart(t *testing.T) {
	_, sessions := workerTestEnv(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		touchSession(t, sessions, id)
	}
	r := &Runner{provider: &fakeProvider{}, model: "m", memoryRoot: memoryDir(), sessionsDir: sessions}
	r.ExecuteAutoDream(context.Background())
	if ActiveTask() != nil {
		t.Fatal("a consolidation started in session_end mode")
	}
	if _, err := os.Stat(lockPath()); !os.IsNotExist(err) {
		t.Fatalf("session_end mode took the lock (stat err %v)", err)
	}
}

// threshold mode keeps the old gates: no history + enough sessions fires.
func TestThresholdModeStillFires(t *testing.T) {
	cfgDir, sessions := workerTestEnv(t)
	writeDreamJSON(t, cfgDir, `{"enabled": true, "trigger": "threshold"}`)
	for _, id := range []string{"a", "b", "c", "d"} {
		touchSession(t, sessions, id)
	}
	p := &fakeProvider{}
	r := &Runner{provider: p, model: "m", memoryRoot: memoryDir(), sessionsDir: sessions}
	r.ExecuteAutoDream(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for (ActiveTask() != nil || p.calls.Load() == 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if p.calls.Load() == 0 {
		t.Fatal("threshold mode did not run the consolidation")
	}
}

func TestSessionEndDue(t *testing.T) {
	cfgDir, sessions := workerTestEnv(t)
	if due, _ := SessionEndDue(5, sessions); due {
		t.Fatal("due with no session on disk")
	}
	touchSession(t, sessions, "cur")
	if due, _ := SessionEndDue(1, sessions); due {
		t.Fatal("due after a single assistant turn (min_turns 2)")
	}
	if due, why := SessionEndDue(2, sessions); !due {
		t.Fatalf("not due after 2 turns with a new session: %s", why)
	}
	// Nothing touched since the last consolidation: not due.
	if err := RecordConsolidation(); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	_ = os.Chtimes(lockPath(), future, future)
	if due, _ := SessionEndDue(3, sessions); due {
		t.Fatal("due with no session since the last consolidation")
	}
	_ = os.Remove(lockPath())
	writeDreamJSON(t, cfgDir, `{"trigger": "threshold"}`)
	if due, _ := SessionEndDue(3, sessions); due {
		t.Fatal("due in threshold mode")
	}
	writeDreamJSON(t, cfgDir, `{"enabled": false}`)
	if due, _ := SessionEndDue(3, sessions); due {
		t.Fatal("due while disabled")
	}
}

// The worker runs one consolidation with the given provider, reviews the
// session that just ended too, and writes dream-last.json.
func TestRunWorkerConsolidatesAndWritesLastRun(t *testing.T) {
	cfgDir, sessions := workerTestEnv(t)
	touchSession(t, sessions, "just-ended")
	target := filepath.Join(memoryDir(), "note.md")
	p := &fakeProvider{script: []*api.ChatResponse{{
		ToolCalls: []api.ToolCall{{ID: "1", Name: "write", Input: map[string]any{"filePath": target, "content": "remember this"}}},
	}}}
	if err := RunWorker(context.Background(), p, "m", sessions, ""); err != nil {
		t.Fatalf("RunWorker: %v", err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "remember this" {
		t.Fatalf("memory file = %q, %v", b, err)
	}
	lr, err := ReadLastRun()
	if err != nil {
		t.Fatal(err)
	}
	if lr.Result != ResultCompleted || lr.Mode != "worker" || lr.SessionsReviewed != 1 || lr.FilesTouched != 1 {
		t.Fatalf("last run = %+v", lr)
	}
	if lr.StartedAt.IsZero() || lr.FinishedAt.IsZero() || lr.PID != os.Getpid() {
		t.Fatalf("last run lacks times/pid: %+v", lr)
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "dream-last.json")); err != nil {
		t.Fatalf("dream-last.json not in the config dir: %v", err)
	}
	if at, _ := ReadLastConsolidatedAt(); at.IsZero() {
		t.Fatal("the lock was not stamped by a completed run")
	}
	st := StatusFromDisk()
	if st.Trigger != TriggerSessionEnd || st.LastWorkerStartedAt.IsZero() || !strings.Contains(st.LastWorkerResult, "完成") {
		t.Fatalf("status = trigger %q started %v result %q", st.Trigger, st.LastWorkerStartedAt, st.LastWorkerResult)
	}
}

// A failed worker run rolls the lock back and records the failure.
func TestRunWorkerFailureRollsBackLock(t *testing.T) {
	_, sessions := workerTestEnv(t)
	touchSession(t, sessions, "s")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunWorker(ctx, &fakeProvider{block: true}, "m", sessions, ""); err == nil {
		t.Fatal("RunWorker with a cancelled context succeeded")
	}
	if _, err := os.Stat(lockPath()); !os.IsNotExist(err) {
		t.Fatalf("lock not rolled back (stat err %v)", err)
	}
	lr, _ := ReadLastRun()
	if lr.Result != ResultFailed || lr.Error == "" {
		t.Fatalf("last run = %+v, want failed with an error", lr)
	}
}

// With nothing new to review the worker gives the lock back.
func TestRunWorkerSkipsWithoutSessions(t *testing.T) {
	_, sessions := workerTestEnv(t)
	p := &fakeProvider{}
	if err := RunWorker(context.Background(), p, "m", sessions, ""); err != nil {
		t.Fatalf("RunWorker: %v", err)
	}
	if p.calls.Load() != 0 {
		t.Fatal("the model was called with nothing to review")
	}
	if _, err := os.Stat(lockPath()); !os.IsNotExist(err) {
		t.Fatalf("lock kept for a skipped run (stat err %v)", err)
	}
	if lr, _ := ReadLastRun(); lr.Result != ResultSkipped {
		t.Fatalf("last run = %+v, want skipped", lr)
	}
}

// The inline fallback returns within its budget even when the model hangs,
// rolls the lock back and shows progress.
func TestRunInlineHonoursBudget(t *testing.T) {
	_, sessions := workerTestEnv(t)
	touchSession(t, sessions, "s")
	r := &Runner{provider: &fakeProvider{block: true}, model: "m", memoryRoot: memoryDir(), sessionsDir: sessions}
	var ticks atomic.Int32
	start := time.Now()
	err := r.RunInline(context.Background(), 150*time.Millisecond, 20*time.Millisecond, func() { ticks.Add(1) })
	if err == nil {
		t.Fatal("RunInline with a hanging model succeeded")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("RunInline returned after %v", el)
	}
	if ticks.Load() == 0 {
		t.Fatal("no progress ticks")
	}
	if _, err := os.Stat(lockPath()); !os.IsNotExist(err) {
		t.Fatalf("lock not rolled back after the budget ran out (stat err %v)", err)
	}
	if lr, _ := ReadLastRun(); lr.Mode != "inline" || lr.Result != ResultFailed {
		t.Fatalf("last run = %+v", lr)
	}
}

// A worker that died without recording its end is reported as such.
func TestStatusReportsDeadWorker(t *testing.T) {
	workerTestEnv(t)
	if err := writeLastRun(LastRun{Mode: "worker", PID: 1 << 30, StartedAt: time.Now(), Result: ResultRunning}); err != nil {
		t.Fatal(err)
	}
	st := StatusFromDisk()
	if !strings.Contains(st.LastWorkerResult, "异常退出") {
		t.Fatalf("LastWorkerResult = %q", st.LastWorkerResult)
	}
}

// In session_end mode the gates are not "waiting" for hours or sessions, so
// the turn-end summary has nothing to report about them.
func TestSessionEndModeNeedsNothing(t *testing.T) {
	st := Status{Enabled: true, Trigger: TriggerSessionEnd, MinHours: 12, MinSessions: 3, HoursSinceLast: 1}
	if st.SessionsNeeded() != 0 || st.HoursNeeded() != 0 {
		t.Fatalf("needed = %d sessions / %.1f hours", st.SessionsNeeded(), st.HoursNeeded())
	}
	if !strings.Contains(st.Summary(), "对话结束") {
		t.Fatalf("Summary = %q", st.Summary())
	}
}

func TestLogPathInConfigDir(t *testing.T) {
	cfgDir, _ := workerTestEnv(t)
	if got := LogPath(); got != filepath.Join(cfgDir, "dream.log") {
		t.Fatalf("LogPath = %q", got)
	}
}

// Fix round 1 (item 2): a panic inside the run rolls the lock back and
// records the run as failed instead of killing the worker with the lock
// stamped as done.
func TestRunWorkerPanicRollsBackLock(t *testing.T) {
	_, sessions := workerTestEnv(t)
	touchSession(t, sessions, "s")
	err := RunWorker(context.Background(), &fakeProvider{panics: true}, "m", sessions, "")
	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("RunWorker = %v, want a panic error", err)
	}
	if _, err := os.Stat(lockPath()); !os.IsNotExist(err) {
		t.Fatalf("lock not rolled back after a panic (stat err %v)", err)
	}
	if lr, _ := ReadLastRun(); lr.Result != ResultFailed || !strings.Contains(lr.Error, "panic") {
		t.Fatalf("last run = %+v, want failed with the panic", lr)
	}
}

// Fix round 1 (item 2): a worker killed outright leaves a "running" record
// with a dead PID and the lock stamped at its start; the next check rolls
// the lock back to where it was before that run.
func TestDeadWorkerLockRolledBackBeforeNextCheck(t *testing.T) {
	_, sessions := workerTestEnv(t)
	touchSession(t, sessions, "s")
	past := time.Now().Add(-time.Minute)
	_ = os.Chtimes(filepath.Join(sessions, "s.jsonl"), past, past)
	prior, ok, err := TryAcquireConsolidationLock() // the killed worker's lock
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	if err := writeLastRun(LastRun{Mode: "worker", PID: 1 << 30, StartedAt: time.Now(), Result: ResultRunning, PriorConsolidatedAt: prior}); err != nil {
		t.Fatal(err)
	}
	if due, why := SessionEndDue(2, sessions); !due {
		t.Fatalf("not due after the dead worker's lock: %s", why)
	}
	if _, err := os.Stat(lockPath()); !os.IsNotExist(err) {
		t.Fatalf("dead worker's lock not rolled back (stat err %v)", err)
	}
	if lr, _ := ReadLastRun(); lr.Result != ResultFailed {
		t.Fatalf("last run = %+v, want failed", lr)
	}
}

// Fix round 1 (item 4): a second worker that finds the lock held (or nothing
// left to review) does not overwrite the running worker's record.
func TestSkippedWorkerKeepsLiveRunningRecord(t *testing.T) {
	_, sessions := workerTestEnv(t)
	live := os.Getppid() // a process that is alive and is not this one
	running := LastRun{Mode: "worker", PID: live, StartedAt: time.Now(), Result: ResultRunning}
	if err := writeLastRun(running); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(memoryDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath(), []byte(strconv.Itoa(live)), 0o644); err != nil {
		t.Fatal(err)
	}
	touchSession(t, sessions, "s")
	future := time.Now().Add(time.Second)
	_ = os.Chtimes(filepath.Join(sessions, "s.jsonl"), future, future)
	p := &fakeProvider{}
	if err := RunWorker(context.Background(), p, "m", sessions, ""); err != nil {
		t.Fatalf("RunWorker: %v", err)
	}
	if p.calls.Load() != 0 {
		t.Fatal("the second worker ran despite the held lock")
	}
	if lr, _ := ReadLastRun(); lr.Result != ResultRunning || lr.PID != live {
		t.Fatalf("running record overwritten: %+v", lr)
	}
}

// Fix round 1 (item 5): a provider that cannot work (no API key) fails the
// worker up front with the reason, without taking the lock.
func TestRunWorkerValidatesProvider(t *testing.T) {
	_, sessions := workerTestEnv(t)
	touchSession(t, sessions, "s")
	p := &fakeProvider{validateErr: errors.New("API key is not set")}
	if err := RunWorker(context.Background(), p, "m", sessions, ""); err == nil {
		t.Fatal("RunWorker with an invalid provider succeeded")
	}
	if p.calls.Load() != 0 {
		t.Fatal("the model was called")
	}
	if _, err := os.Stat(lockPath()); !os.IsNotExist(err) {
		t.Fatalf("lock taken (stat err %v)", err)
	}
	if lr, _ := ReadLastRun(); lr.Result != ResultFailed || !strings.Contains(lr.Error, "API key") {
		t.Fatalf("last run = %+v", lr)
	}
}
