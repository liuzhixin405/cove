package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/dream"
	"github.com/liuzhixin405/cove/internal/hooks"
)

// slowPending is a pendingWaiter whose background work takes delay.
type slowPending struct {
	delay time.Duration
	saved atomic.Bool
}

func (s *slowPending) BackgroundPending() bool { return !s.saved.Load() }

func (s *slowPending) WaitBackground(ctx context.Context) {
	select {
	case <-time.After(s.delay):
		s.saved.Store(true)
	case <-ctx.Done():
	}
}

func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	f()
	_ = w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

// Important 3: an exit with the last turn's extraction still running waits
// for it (and says so); with nothing running it neither waits nor prints.
func TestExitWaitsForInFlightExtraction(t *testing.T) {
	w := &slowPending{delay: 100 * time.Millisecond}
	out := captureStderr(t, func() { waitPending(w, time.Second) })
	if !w.saved.Load() {
		t.Fatal("exit did not wait for the extraction")
	}
	if !strings.Contains(out, "正在保存本轮记忆") {
		t.Fatalf("stderr = %q, want the saving notice", out)
	}
	out = captureStderr(t, func() { waitPending(w, time.Second) })
	if out != "" {
		t.Fatalf("nothing pending, stderr = %q", out)
	}
}

// Important 3: the wait is bounded.
func TestExitWaitForExtractionIsBounded(t *testing.T) {
	w := &slowPending{delay: time.Hour}
	start := time.Now()
	captureStderr(t, func() { waitPending(w, 50*time.Millisecond) })
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("waited %v past a 50ms bound", el)
	}
	if exitBackgroundWait != 10*time.Second {
		t.Fatalf("exitBackgroundWait = %v, want 10s", exitBackgroundWait)
	}
}

// The session-end notice tells the user the consolidation costs model calls.
func TestSessionEndDreamNoticeMentionsCalls(t *testing.T) {
	eng := newTestEngine(t)
	resetSessionEnd(t, hooks.NewManager())
	stubDreamSpawn(t, nil)
	var got strings.Builder
	dreamNotice = func(s string) { got.WriteString(s) }
	eng.LoadMessages(twoTurnMessages())
	noteTurnCompleted()
	noteTurnCompleted()
	finishSession(eng, nil)
	want := fmt.Sprintf("约 %d 次后台模型调用", dream.MaxDreamIterations)
	if !strings.Contains(got.String(), want) {
		t.Fatalf("notice = %q, want it to mention %q", got.String(), want)
	}
}
