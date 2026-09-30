package main

import (
	"context"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/repl"
)

// Only a bare "继续"/"continue" is the resume request. Any line that merely
// started with "继续" used to count: idle, "继续把 README 翻译成英文" resumed
// some other session and sent it a bare "继续", dropping the actual request;
// while a task ran, "continue with the tests" was refused instead of steered.
func TestIsContinueCommandOnlyMatchesABareRequest(t *testing.T) {
	for _, in := range []string{"继续", "continue", "Continue", "  继续  ", "继续。", "继续!", "继续！", "continue.", "CONTINUE!", "继续…", "继续~"} {
		if !isContinueCommand(in) {
			t.Errorf("%q should be a continue request", in)
		}
	}
	for _, in := range []string{"继续把 README 翻译成英文", "继续写测试", "continue with the tests", "continue fixing the bug", "continued", "继续 修复", "不继续", "/continue", ""} {
		if isContinueCommand(in) {
			t.Errorf("%q is a normal message, not a continue request", in)
		}
	}
}

// fakeExitTasks records the order the exit routine drives the task runner.
type fakeExitTasks struct {
	running bool
	calls   []string
	// idle is closed once the "task" has returned; nil means it never does.
	idle     chan bool
	waitedOK bool
}

func (f *fakeExitTasks) CancelForExit() bool {
	f.calls = append(f.calls, "cancel")
	return f.running
}

func (f *fakeExitTasks) WaitIdleUntil(deadline time.Time) bool {
	f.calls = append(f.calls, "wait")
	select {
	case <-f.idle:
		f.waitedOK = true
	case <-time.After(time.Until(deadline)):
	}
	return f.waitedOK
}

// Ctrl+D (stdin EOF) saved the session straight away while the task kept
// running: a half turn was saved, the save raced the task's appends, and the
// exit then killed the task with no interrupted draft. /exit cancelled first.
// Every exit path now goes through leaveREPL.
func TestLeaveREPLCancelsTheTaskBeforeSaving(t *testing.T) {
	repl.ClearPermInputCh()
	idle := make(chan bool)
	close(idle)
	tasks := &fakeExitTasks{running: true, idle: idle}
	leaveREPL(tasks, func() { tasks.calls = append(tasks.calls, "save") })
	if got := strings.Join(tasks.calls, ","); got != "cancel,wait,save" {
		t.Fatalf("exit order = %s, want cancel,wait,save", got)
	}

	idleTasks := &fakeExitTasks{}
	leaveREPL(idleTasks, func() { idleTasks.calls = append(idleTasks.calls, "save") })
	if got := strings.Join(idleTasks.calls, ","); got != "cancel,save" {
		t.Fatalf("with nothing running, exit order = %s, want cancel,save", got)
	}
}

// A task blocked on an approval prompt waits on the prompt's answer channel,
// not on its context: /exit used to cancel it, wait the full 3 seconds for a
// task that could not return, and save while it was still blocked (its
// afterRun never ran). The exit now answers the prompt first.
func TestLeaveREPLAnswersAWaitingPromptFirst(t *testing.T) {
	buf := captureTurnOutput(t)
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh() })

	done := make(chan bool, 1)
	idle := make(chan bool)
	go func() {
		done <- askToolPermission(nil, "bash", map[string]any{"command": "rm -rf build"}, "")
		close(idle)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "需要授权") {
		if time.Now().After(deadline) {
			t.Fatal("prompt never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}

	tasks := &fakeExitTasks{running: true, idle: idle}
	start := time.Now()
	leaveREPL(tasks, func() {})
	if !tasks.waitedOK {
		t.Fatalf("the task was still blocked on its prompt after %v", time.Since(start))
	}
	if allow := <-done; allow {
		t.Fatal("exiting allowed the pending tool call")
	}
}

// After bare /history (allowed while a task runs) a bare number went straight
// to ResumeSession, swapping the session under the running task — the very
// thing the dispatcher refuses for "/history 2".
func TestHistoryPickIsRefusedWhileATaskRuns(t *testing.T) {
	r := newREPLTaskRunner(nil)
	r.mu.Lock()
	r.running = true
	r.current = api.Message{Role: "user", Content: "first"}
	r.mu.Unlock()

	var printed []string
	fe := &frontend{tasks: r, historyPickPending: true, print: func(s string) { printed = append(printed, s) }}
	resumed := ""
	if !fe.takeHistoryPick("2", func(in string) { resumed = in }) {
		t.Fatal("the number was not taken as a history pick")
	}
	if resumed != "" {
		t.Fatalf("resumed session %q while a task was running", resumed)
	}
	if len(printed) != 1 || !strings.Contains(printed[0], "任务运行中不能") {
		t.Fatalf("refusal hint = %q", printed)
	}

	// Once the task has ended the same number resumes.
	r.mu.Lock()
	r.running = false
	r.mu.Unlock()
	if !fe.takeHistoryPick("2", func(in string) { resumed = in }) || resumed != "2" {
		t.Fatalf("idle pick: resumed = %q", resumed)
	}
	if fe.historyPickPending {
		t.Fatal("the pick stayed pending after resuming")
	}

	// A line that is not a number ends the pick and is handled as usual.
	fe.historyPickPending = true
	if fe.takeHistoryPick("hello", func(string) { t.Fatal("resumed on a non-number") }) {
		t.Fatal("a normal message was swallowed by the history pick")
	}
	if fe.historyPickPending {
		t.Fatal("a normal message did not end the pick")
	}
}

// /base-url reloads the provider like /model and /api-key; it had no
// mutates, so it ran mid-turn.
func TestBaseURLIsBlockedWhileATaskRuns(t *testing.T) {
	if !commandMutatesEngine("/base-url http://x") {
		t.Error("/base-url <url> should be blocked while a task runs")
	}
	if commandMutatesEngine("/base-url") {
		t.Error("bare /base-url only shows usage and should stay allowed")
	}
}

// The per-turn signal watcher used to be `go func() { <-sigCh; cancel() }()`:
// signal.Stop does not close the channel, so every turn without a signal left
// a goroutine behind.
func TestWatchTurnSignalEndsWithTheTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	watched := watchTurnSignal(ctx, sigCh, cancel)
	cancel()
	select {
	case sig, ok := <-watched:
		if ok && sig != nil {
			t.Fatalf("reported signal %v that never came", sig)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher outlived its turn")
	}
}

// SIGTERM cancels the turn and is reported, so headless ends the run instead
// of reading the next input line.
func TestWatchTurnSignalReportsSIGTERM(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	watched := watchTurnSignal(ctx, sigCh, cancel)
	sigCh <- syscall.SIGTERM
	select {
	case sig := <-watched:
		if sig != syscall.SIGTERM {
			t.Fatalf("reported %v, want SIGTERM", sig)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no signal reported")
	}
	if ctx.Err() == nil {
		t.Fatal("the turn was not cancelled")
	}
	if !turnEndsRun(syscall.SIGTERM) || turnEndsRun(syscall.SIGINT) || turnEndsRun(nil) {
		t.Fatal("only SIGTERM should end the headless run")
	}
}
