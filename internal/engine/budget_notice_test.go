package engine

import (
	"strings"
	"testing"
	"time"
)

const budgetNoticeTail = "do not stop solely because of this notice.]"

func budgetNotices(eng *Engine) (onTool, elsewhere int) {
	for _, m := range eng.messages {
		if !strings.Contains(m.Content, "[budget: about ") {
			continue
		}
		if m.Role == "tool" && strings.HasSuffix(m.Content, budgetNoticeTail) {
			onTool++
		} else {
			elsewhere++
		}
	}
	return
}

func TestBudgetNoticeOncePerIterationWindow(t *testing.T) {
	eng := limitEngine(t, alwaysToolProvider(0), 5)
	rec := &limitRecorder{decisions: []LimitDecision{LimitContinue, LimitStop}}
	eng.IterationLimitPrompt = rec.prompt
	_, _ = run(t, eng, "做点事")
	onTool, elsewhere := budgetNotices(eng)
	if onTool != 2 || elsewhere != 0 {
		t.Fatalf("notices on tool results = %d (elsewhere %d), want one per window (2)", onTool, elsewhere)
	}
	for _, m := range eng.messages {
		if strings.Contains(m.Content, "[budget: about ") && !strings.Contains(m.Content, "[budget: about 1 model calls remain in this window.") {
			t.Fatalf("notice %q, want 1 model call remaining at 80%% of 5", m.Content)
		}
	}
}

func TestBudgetNoticeTimeWindow(t *testing.T) {
	eng := limitEngine(t, alwaysToolProvider(0), 0)
	eng.SetMaxIterations(UnlimitedIterations)
	eng.config.MaxTurnMinutes = 10
	l := eng.newTurnLimits()
	if s := eng.budgetNotice(l, 1); s != "" {
		t.Fatalf("notice %q at the start of the window", s)
	}
	l.start = time.Now().Add(-8*time.Minute - 30*time.Second)
	s := eng.budgetNotice(l, 1)
	if !strings.Contains(s, "[budget: about 2 minutes remain in this window.") {
		t.Fatalf("notice = %q, want about 2 minutes remaining", s)
	}
	if again := eng.budgetNotice(l, 2); again != "" {
		t.Fatalf("second notice %q in the same window", again)
	}
}

func TestBudgetNoticeBothWindows(t *testing.T) {
	eng := limitEngine(t, alwaysToolProvider(0), 10)
	eng.config.MaxTurnMinutes = 10
	l := eng.newTurnLimits()
	s := eng.budgetNotice(l, 8)
	if !strings.Contains(s, "about 2 model calls / 10 minutes remain") {
		t.Fatalf("notice = %q, want both remainders", s)
	}
}
