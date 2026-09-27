package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/diagnostic"
)

// Guidance typed before a task was cancelled or failed stays pending in the
// engine, so /continue (and the retry hint just printed) still hold: the
// guidance reaches the model at the resumed turn's first call. It used to
// start at once as a new task, which cleared the interrupted turn and made
// the "/continue 可继续" line a lie.
func TestSteerIsKeptWhenTheTaskFails(t *testing.T) {
	eng := steerTestEngine(t)
	r := newREPLTaskRunner(eng)
	r.mu.Lock()
	r.running = true
	r.mu.Unlock()
	eng.Steer("别改测试文件")

	r.afterRun(api.Message{Role: "user", Content: "task"}, errors.New("api: boom"))

	if _, n := eng.PendingSteer(); n != 1 {
		t.Fatalf("pending steer count = %d, want 1 (kept for /continue)", n)
	}
	if snap := r.Snapshot(); len(snap.Queued) != 0 || snap.Running {
		t.Errorf("snapshot = %+v, want idle with an empty queue", snap)
	}
}

// After a task completes normally, guidance it never consumed runs as the
// next task (there is nothing to continue).
func TestSteerRunsAsNextTaskWhenTheTaskCompletes(t *testing.T) {
	eng := steerTestEngine(t)
	r := newREPLTaskRunner(eng)
	r.mu.Lock()
	r.running = true
	r.mu.Unlock()
	eng.Steer("再加一个测试")

	r.afterRun(api.Message{Role: "user", Content: "task"}, nil)

	if _, n := eng.PendingSteer(); n != 0 {
		t.Fatalf("steer still pending after a completed task: %d", n)
	}
	snap := r.Snapshot()
	if !snap.Running || snap.Current != "再加一个测试" {
		t.Errorf("the reclaimed guidance did not start as the next task: %+v", snap)
	}
	r.CancelRunning()
}

// The retry hint after a failed turn carries the diagnostic code, so the
// user sees the same E-number /diagnose errors shows.
func TestTaskErrorHintCarriesTheCode(t *testing.T) {
	overflow := &api.StatusError{Status: 400, Msg: "request (17964 tokens) exceeds the available context size (16384 tokens)"}
	code, _ := diagnostic.Classify(overflow, diagnostic.Context{})
	if code == "" {
		t.Fatal("overflow not classified")
	}
	if h := taskErrorHint(overflow); !strings.HasPrefix(h, "["+string(code)+"] ") {
		t.Fatalf("hint %q does not start with [%s]", h, code)
	}
	if h := taskErrorHint(errors.New("api: boom")); strings.HasPrefix(h, "[") {
		t.Errorf("an uncoded error got a code prefix: %q", h)
	}
}

// Guidance reclaimed after a task completed runs as its own request, so it
// carries the task it was meant for: on its own, "只要llama不要其他的" told
// the model nothing and showed up in /history as a task and a draft.
func TestReclaimedSteerNamesTheTaskItWasFor(t *testing.T) {
	eng := steerTestEngine(t)
	r := newREPLTaskRunner(eng)
	r.mu.Lock()
	r.running = true
	r.current = api.Message{Role: "user", Content: "在该目录写一个netcore的agent框架的项目，使用ai本地模型"}
	r.mu.Unlock()
	eng.Steer("只要llama不要其他的")

	r.afterRun(r.current, nil)

	snap := r.Snapshot()
	r.CancelRunning()
	if !snap.Running {
		t.Fatalf("reclaimed guidance did not start: %+v", snap)
	}
	if !strings.Contains(snap.Current, "只要llama不要其他的") || !strings.Contains(snap.Current, "netcore") {
		t.Fatalf("the new task does not carry both the guidance and the task it was for: %q", snap.Current)
	}
}
