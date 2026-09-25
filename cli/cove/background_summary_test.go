package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/liuzhixin405/cove/internal/dream"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/termui"
)

func TestBackgroundSummaryLine(t *testing.T) {
	waiting := dream.Status{Enabled: true, MinSessions: 3, SessionsSinceLast: 1, HoursSinceLast: -1}
	cases := []struct {
		s    engine.BackgroundSummary
		want string
	}{
		{engine.BackgroundSummary{MemoriesExtracted: 2, DreamStatus: waiting, DreamChanged: true, SessionSaved: true},
			"已提取 2 条记忆 · dream 还差 2 个会话"},
		{engine.BackgroundSummary{MemoriesExtracted: 1, DreamStatus: waiting, SessionSaved: true},
			"已提取 1 条记忆"},
		{engine.BackgroundSummary{DreamStatus: dream.Status{Enabled: true, Running: true}, DreamFired: true, DreamChanged: true, SessionSaved: true},
			"dream 已开始整理记忆"},
		{engine.BackgroundSummary{SessionSaved: false},
			"会话保存失败"},
		{engine.BackgroundSummary{SessionSaved: true}, ""},
	}
	for _, c := range cases {
		got := backgroundSummaryLine(c.s)
		if c.want == "" {
			if got != "" {
				t.Fatalf("%+v: got %q, want nothing", c.s, got)
			}
			continue
		}
		if !strings.HasPrefix(got, c.want) {
			t.Fatalf("%+v: got %q, want %q", c.s, got, c.want)
		}
	}
}

func TestPrintBackgroundSummaryIsOneDimLine(t *testing.T) {
	var buf bytes.Buffer
	termui.SetWriter(&buf)
	t.Cleanup(func() { termui.SetWriter(nil) })
	printBackgroundSummary(engine.BackgroundSummary{MemoriesExtracted: 2, SessionSaved: true})
	out := buf.String()
	if !strings.Contains(out, termui.Dim) {
		t.Fatalf("summary is not dim: %q", out)
	}
	if plain := strings.TrimSpace(ansi.Strip(out)); strings.Count(plain, "\n") != 0 || plain != "已提取 2 条记忆" {
		t.Fatalf("summary = %q, want one line", plain)
	}
}

// -p and a stdout that is not a terminal get no summary line.
func TestInstallBackgroundSummaryOnlyOnTerminal(t *testing.T) {
	eng := &engine.Engine{}
	installBackgroundSummary(eng, false)
	if eng.OnBackgroundSummary != nil {
		t.Fatal("installed without a terminal")
	}
	installBackgroundSummary(eng, true)
	if eng.OnBackgroundSummary == nil {
		t.Fatal("not installed on a terminal")
	}
}

// Text the Markdown renderer held back after a shown reasoning trace (a lone
// "*" at the end of the answer) was flushed without switching the printer
// to text, so it was glued to the reasoning row and drawn in its style.
func TestStopFlushesHeldBackTextOnItsOwnRowAfterReasoning(t *testing.T) {
	old := showReasoning
	showReasoning = true
	t.Cleanup(func() { showReasoning = old })
	buf := captureTurnOutput(t)
	p := newTurnPrinter()
	p.reasoning("先想一想")
	p.delta("*")
	p.stop()

	out := ansi.Strip(buf.String())
	row, ok := lineBefore(out, "*")
	if !ok {
		t.Fatalf("held-back text missing: %q", out)
	}
	if strings.Contains(row, "先想一想") {
		t.Fatalf("held-back text shares the reasoning row: %q in %q", row, out)
	}
	raw := buf.String()
	if i, j := strings.LastIndex(raw, termui.Reset), strings.LastIndex(raw, "*"); i < 0 || i > j {
		t.Fatalf("reasoning style not closed before the held-back text: %q", raw)
	}
}

func TestBeginAttemptFlushesHeldBackTextAfterReasoning(t *testing.T) {
	old := showReasoning
	showReasoning = true
	t.Cleanup(func() { showReasoning = old })
	buf := captureTurnOutput(t)
	p := newTurnPrinter()
	p.reasoning("想")
	p.delta("*")
	p.beginAttempt()
	p.stop()

	row, ok := lineBefore(ansi.Strip(buf.String()), "*")
	if !ok || strings.Contains(row, "想") {
		t.Fatalf("held-back text row = %q (found %v) in %q", row, ok, buf.String())
	}
}

// lateSummaryRunner streams an answer and, in the middle of it, delivers the
// background summary of the previous turn (its extraction outlived the turn).
type lateSummaryRunner struct{}

func (lateSummaryRunner) RunWithStream(_ context.Context, _ string, onDelta func(string)) (string, error) {
	onDelta("第一段回答\n")
	printBackgroundSummary(engine.BackgroundSummary{MemoriesExtracted: 3, SessionSaved: true})
	onDelta("第二段回答\n")
	return "第一段回答\n第二段回答\n", nil
}

func TestLateBackgroundSummaryWaitsForTheTurnToEnd(t *testing.T) {
	buf := captureTurnOutput(t)
	if _, err := runChatInteraction(context.Background(), lateSummaryRunner{}, "问题"); err != nil {
		t.Fatal(err)
	}
	out := ansi.Strip(buf.String())
	i, j := strings.Index(out, "已提取 3 条记忆"), strings.Index(out, "第二段回答")
	if i < 0 || j < 0 {
		t.Fatalf("output missing parts: %q", out)
	}
	if i < j {
		t.Fatalf("the summary was printed inside the streamed answer: %q", out)
	}
}

// Between turns (the prompt is showing) a summary is printed at once.
func TestBackgroundSummaryBetweenTurnsPrintsAtOnce(t *testing.T) {
	buf := captureTurnOutput(t)
	printBackgroundSummary(engine.BackgroundSummary{MemoriesExtracted: 1, SessionSaved: true})
	if !strings.Contains(ansi.Strip(buf.String()), "已提取 1 条记忆") {
		t.Fatalf("summary not printed while idle: %q", buf.String())
	}
}

// Final fix (Important 2a): pruned sessions are named on the turn-end line.
func TestBackgroundSummaryLineShowsPrunedSessions(t *testing.T) {
	got := backgroundSummaryLine(engine.BackgroundSummary{SessionsPruned: 3, MaxSessions: 200, SessionSaved: true})
	if got != "已清理 3 个旧会话（max_sessions=200）" {
		t.Fatalf("line = %q", got)
	}
}
