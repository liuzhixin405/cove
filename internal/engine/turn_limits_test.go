package engine

import (
	"strings"
	"testing"
	"time"
)

// timeLimitEngine runs turns whose time limit is `minutes` units of 1ms, with
// a tool that takes 3ms, so the limit is reached after a couple of rounds.
func timeLimitEngine(t *testing.T, finishAfter, minutes int) (*Engine, *seqProvider) {
	t.Helper()
	prov := alwaysToolProvider(finishAfter)
	eng := newPatternEngine(t, prov, func(c *Config) {
		c.MaxTurnMinutes = minutes
		c.LoopDetectionDisabled = true
	}, &mockTool{name: "read_tool", readOnly: true, safe: true, result: "ok", delay: 3 * time.Millisecond})
	eng.turnTimeUnit = time.Millisecond
	return eng, prov
}

func TestTurnTimeLimitAsksWithReasonTime(t *testing.T) {
	eng, _ := timeLimitEngine(t, 0, 5)
	rec := &limitRecorder{}
	eng.IterationLimitPrompt = rec.prompt

	_, err := run(t, eng, "慢慢做")
	if err == nil {
		t.Fatal("stopping at the time limit must end the turn with an error")
	}
	if len(rec.calls) != 1 || rec.calls[0].Reason != LimitReasonTime {
		t.Fatalf("prompt calls = %+v, want one with Reason %q", rec.calls, LimitReasonTime)
	}
	if rec.calls[0].Elapsed < 5*time.Millisecond {
		t.Fatalf("Elapsed = %v, want >= the limit", rec.calls[0].Elapsed)
	}
	if !strings.Contains(err.Error(), "max_turn_minutes") {
		t.Fatalf("error %q should name max_turn_minutes", err)
	}
	if eng.interrupted == nil {
		t.Fatal("a turn stopped at the time limit must be resumable")
	}
}

func TestTurnTimeLimitContinueExtends(t *testing.T) {
	eng, _ := timeLimitEngine(t, 0, 5)
	rec := &limitRecorder{decisions: []LimitDecision{LimitContinue, LimitStop}}
	eng.IterationLimitPrompt = rec.prompt

	_, _ = run(t, eng, "慢慢做")
	if len(rec.calls) != 2 {
		t.Fatalf("prompt called %d times, want 2", len(rec.calls))
	}
	if rec.calls[1].Elapsed < 10*time.Millisecond {
		t.Fatalf("second ask after %v, want after a second window (>=10ms)", rec.calls[1].Elapsed)
	}
}

// -p installs no prompt: the time limit stops the turn with an error.
func TestTurnTimeLimitWithoutPromptStops(t *testing.T) {
	eng, _ := timeLimitEngine(t, 0, 5)
	if _, err := run(t, eng, "慢慢做"); err == nil || !strings.Contains(err.Error(), "max_turn_minutes") {
		t.Fatalf("err = %v, want the time-limit error", err)
	}
}

func TestTurnTimeLimitZeroIsOff(t *testing.T) {
	eng, _ := timeLimitEngine(t, 6, 0)
	eng.IterationLimitPrompt = func(LimitStats) LimitDecision { t.Fatal("asked with the time limit off"); return LimitStop }
	if reply, err := run(t, eng, "慢慢做"); err != nil || reply != "完成" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
}

func stagnationEngine(t *testing.T, finishAfter int) *Engine {
	t.Helper()
	eng := newPatternEngine(t, alwaysToolProvider(finishAfter), nil,
		&mockTool{name: "read_tool", readOnly: true, safe: true, result: "ok"})
	eng.loopDetector.stallThresh = 2
	return eng
}

func TestStagnationAsksOncePerTurn(t *testing.T) {
	eng := stagnationEngine(t, 7)
	rec := &limitRecorder{decisions: []LimitDecision{LimitContinue}}
	eng.IterationLimitPrompt = rec.prompt

	reply, err := run(t, eng, "查资料")
	if err != nil || reply != "完成" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if len(rec.calls) != 1 || rec.calls[0].Reason != LimitReasonStagnation {
		t.Fatalf("prompt calls = %+v, want exactly one stagnation ask", rec.calls)
	}
}

func TestStagnationStopEndsTurn(t *testing.T) {
	eng := stagnationEngine(t, 7)
	eng.IterationLimitPrompt = func(LimitStats) LimitDecision { return LimitStop }
	if _, err := run(t, eng, "查资料"); err == nil {
		t.Fatal("stop at a stagnation ask must end the turn")
	}
	if eng.interrupted == nil {
		t.Fatal("the turn must be resumable")
	}
}

// Without a prompt (-p, headless) stagnation stays a log line.
func TestStagnationWithoutPromptIsLogOnly(t *testing.T) {
	eng := stagnationEngine(t, 7)
	if reply, err := run(t, eng, "查资料"); err != nil || reply != "完成" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
}
