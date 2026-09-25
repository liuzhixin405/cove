package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/config"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/repl"
	"github.com/liuzhixin405/cove/internal/termui"
)

func TestLimitPromptTextShowsStatsAndOptions(t *testing.T) {
	text := limitPromptText(engine.LimitStats{
		Iterations:  200,
		Elapsed:     3*time.Minute + 12*time.Second,
		Cost:        0.4321,
		RecentSteps: []string{"read", "grep", "bash", "edit", "bash"},
		Reason:      engine.LimitReasonIterations,
		Window:      200,
	})
	for _, want := range []string{"200", "3m12s", "$0.4321", "read → grep → bash → edit → bash", "[c] 继续 200 次", "[s] 停止"} {
		if !strings.Contains(text, want) {
			t.Fatalf("prompt text lacks %q:\n%s", want, text)
		}
	}
}

func TestLimitPromptTextPerReason(t *testing.T) {
	timed := limitPromptText(engine.LimitStats{Reason: engine.LimitReasonTime, Window: 60, Elapsed: time.Hour})
	if !strings.Contains(timed, "60 分钟") || !strings.Contains(timed, "[c] 继续 60 分钟") {
		t.Fatalf("time prompt:\n%s", timed)
	}
	stuck := limitPromptText(engine.LimitStats{Reason: engine.LimitReasonStagnation, Iterations: 60})
	if !strings.Contains(stuck, "[c] 继续") || !strings.Contains(stuck, "文件") {
		t.Fatalf("stagnation prompt:\n%s", stuck)
	}
}

func TestLimitAnswerDecision(t *testing.T) {
	for in, want := range map[string]engine.LimitDecision{
		"c": engine.LimitContinue, "C ": engine.LimitContinue, "继续": engine.LimitContinue, "y": engine.LimitContinue,
		"s": engine.LimitStop, "": engine.LimitStop, "n": engine.LimitStop, "停止": engine.LimitStop, "x": engine.LimitStop,
	} {
		if got := limitAnswerDecision(in); got != want {
			t.Fatalf("limitAnswerDecision(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestAskLimitRelaysAnswer(t *testing.T) {
	var buf bytes.Buffer
	termui.SetWriter(&buf)
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh(); termui.SetWriter(nil) })

	done := make(chan engine.LimitDecision, 1)
	go func() {
		done <- askTurnLimit(engine.LimitStats{Iterations: 3, Reason: engine.LimitReasonIterations, Window: 3})
	}()
	waitForPermInputCh(t) <- "c"
	select {
	case d := <-done:
		if d != engine.LimitContinue {
			t.Fatalf("decision = %v, want continue", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("askTurnLimit did not return after the answer")
	}
}

func TestAskLimitTimeoutStops(t *testing.T) {
	var buf bytes.Buffer
	termui.SetWriter(&buf)
	oldInteractive, oldTimeout := replInteractive, limitPromptTimeout
	replInteractive = true
	limitPromptTimeout = 30 * time.Millisecond
	t.Cleanup(func() {
		replInteractive, limitPromptTimeout = oldInteractive, oldTimeout
		repl.ClearPermInputCh()
		termui.SetWriter(nil)
	})
	if d := askTurnLimit(engine.LimitStats{Reason: engine.LimitReasonIterations, Window: 3}); d != engine.LimitStop {
		t.Fatalf("an unanswered prompt must stop, got %v", d)
	}
	if ch := repl.TakePermInputCh(); ch != nil {
		t.Fatal("the input channel must be unregistered after a timeout")
	}
	// A "c" typed later is an ordinary message now; the notice says so. (The
	// /continue hint is on the stop line that follows the notice.)
	out := buf.String()
	for _, want := range []string{"已按停止处理", "新消息"} {
		if !strings.Contains(out, want) {
			t.Fatalf("timeout notice lacks %q: %q", want, out)
		}
	}
}

func TestAskLimitNotInteractiveStops(t *testing.T) {
	oldInteractive := replInteractive
	replInteractive = false
	t.Cleanup(func() { replInteractive = oldInteractive })
	if d := askTurnLimit(engine.LimitStats{Reason: engine.LimitReasonIterations}); d != engine.LimitStop {
		t.Fatalf("decision = %v, want stop", d)
	}
}

func TestParseCLIArgsMaxTurns(t *testing.T) {
	opts, err := parseCLIArgs([]string{"-p", "hi", "--max-turns", "5"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.maxTurnsSet || opts.maxTurns != 5 {
		t.Fatalf("maxTurns = %d set=%v", opts.maxTurns, opts.maxTurnsSet)
	}
	opts, err = parseCLIArgs([]string{"--max-turns", "0", "-p", "hi"})
	if err != nil || !opts.maxTurnsSet || opts.maxTurns != 0 {
		t.Fatalf("--max-turns 0: %+v %v", opts, err)
	}
	for _, bad := range [][]string{
		{"-p", "hi", "--max-turns", "x"},
		{"-p", "hi", "--max-turns", "-2"},
		{"-p", "hi", "--max-turns"},
		{"--max-turns", "5"}, // only with -p
	} {
		if _, err := parseCLIArgs(bad); err == nil {
			t.Fatalf("%v: want an error", bad)
		}
	}
}

func TestPrintModeIterationLimit(t *testing.T) {
	cases := []struct {
		set        bool
		turns, cfg int
		want       int
	}{
		{false, 0, 150, 150},
		{true, 7, 150, 7},
		{true, 0, 150, engine.UnlimitedIterations},
	}
	for _, c := range cases {
		if got := printModeIterationLimit(cliOptions{maxTurnsSet: c.set, maxTurns: c.turns}, c.cfg); got != c.want {
			t.Fatalf("%+v: got %d, want %d", c, got, c.want)
		}
	}
}

type fakeLimiter struct{ iters, minutes int }

func (f *fakeLimiter) SetMaxIterations(n int)  { f.iters = n }
func (f *fakeLimiter) SetMaxTurnMinutes(n int) { f.minutes = n }

func TestApplyUnattendedLimits(t *testing.T) {
	cfg := config.DefaultConfig() // no max_turn_minutes written by the user
	f := &fakeLimiter{minutes: -1}
	applyUnattendedLimits(f, cliOptions{}, cfg)
	if f.minutes != 0 || f.iters != cfg.MaxIterations {
		t.Fatalf("defaults: %+v, want no time limit and max_iterations", f)
	}
	f = &fakeLimiter{}
	applyUnattendedLimits(f, cliOptions{maxTurnsSet: true, maxTurns: 9}, cfg)
	if f.iters != 9 {
		t.Fatalf("--max-turns 9: %+v", f)
	}
}

func TestLimitErrorTextPerFrontEnd(t *testing.T) {
	iters := &engine.LimitError{Reason: engine.LimitReasonIterations, Limit: 200}
	shell := interactiveErrorText(iters)
	for _, want := range []string{"200", "输入 /continue 可继续", "max_iterations"} {
		if !strings.Contains(shell, want) {
			t.Fatalf("interactive text %q lacks %q", shell, want)
		}
	}
	if strings.Contains(shell, "--max-turns") {
		t.Fatalf("interactive text names --max-turns: %q", shell)
	}
	var buf bytes.Buffer
	if code := printModeFailure(&buf, false, fmt.Errorf("wrapped: %w", iters)); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if p := buf.String(); !strings.Contains(p, "--max-turns") || strings.Contains(p, "/continue") || !strings.HasPrefix(p, "Error: ") {
		t.Fatalf("-p text = %q, want --max-turns and no /continue", p)
	}
	timed := interactiveErrorText(&engine.LimitError{Reason: engine.LimitReasonTime, Limit: 60})
	if !strings.Contains(timed, "max_turn_minutes") || !strings.Contains(timed, "/continue") {
		t.Fatalf("interactive time text = %q", timed)
	}
	if got := interactiveErrorText(errors.New("boom")); got != "boom" {
		t.Fatalf("other errors unchanged, got %q", got)
	}
}

func TestLimitPromptTextLoopOptions(t *testing.T) {
	text := limitPromptText(engine.LimitStats{
		Iterations: 12,
		Reason:     engine.LimitReasonLoop,
		Detail:     "检测到工具调用循环(L1): 相同模式连续出现 10/14 次",
	})
	for _, want := range []string{"检测到重复操作", "[c] 本轮禁用循环检测并继续", "[s] 停止", "检测到工具调用循环(L1)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("loop prompt lacks %q:\n%s", want, text)
		}
	}
}

func TestInteractiveErrorTextLoopStop(t *testing.T) {
	got := interactiveErrorText(&engine.LimitError{Reason: engine.LimitReasonLoop})
	if !strings.Contains(got, "循环") || !strings.Contains(got, "/continue") {
		t.Fatalf("loop stop text = %q", got)
	}
}
