package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

// alwaysToolProvider asks for one read_tool call on every request until
// finishAfter requests have been answered (0 = never), then answers "完成".
func alwaysToolProvider(finishAfter int) *seqProvider {
	return &seqProvider{reply: func(_ context.Context, n int, _ api.ChatRequest) (*api.ChatResponse, error) {
		if finishAfter > 0 && n >= finishAfter {
			return &api.ChatResponse{Content: "完成"}, nil
		}
		return toolCallResp(fmt.Sprintf("c%d", n), "read_tool", map[string]any{"query": fmt.Sprintf("q%d", n)}), nil
	}}
}

func limitEngine(t *testing.T, prov *seqProvider, limit int) *Engine {
	t.Helper()
	eng := newPatternEngine(t, prov, func(c *Config) {
		c.MaxIterations = limit
		c.LoopDetectionDisabled = true
	}, &mockTool{name: "read_tool", readOnly: true, safe: true, result: "ok"})
	return eng
}

type limitRecorder struct {
	mu        sync.Mutex
	calls     []LimitStats
	decisions []LimitDecision
}

func (r *limitRecorder) prompt(s LimitStats) LimitDecision {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, s)
	if len(r.decisions) == 0 {
		return LimitStop
	}
	d := r.decisions[0]
	r.decisions = r.decisions[1:]
	return d
}

func TestIterationLimitAsksOnceAtCap(t *testing.T) {
	prov := alwaysToolProvider(0)
	eng := limitEngine(t, prov, 3)
	rec := &limitRecorder{}
	eng.IterationLimitPrompt = rec.prompt

	_, err := run(t, eng, "做点事")
	if err == nil {
		t.Fatal("stopping at the cap must end the turn with an error")
	}
	if len(rec.calls) != 1 {
		t.Fatalf("prompt called %d times, want 1", len(rec.calls))
	}
	got := rec.calls[0]
	if got.Iterations != 3 || got.Reason != LimitReasonIterations {
		t.Fatalf("stats = %+v, want Iterations=3 Reason=%q", got, LimitReasonIterations)
	}
	if len(got.RecentSteps) == 0 || got.RecentSteps[len(got.RecentSteps)-1] != "read_tool" {
		t.Fatalf("RecentSteps = %v, want the tool names", got.RecentSteps)
	}
	if n := len(toolRequests(prov)); n != 3 { // the tool-less wrap-up call is not a turn step
		t.Fatalf("model called %d times, want 3", n)
	}
}

func TestIterationLimitContinueExtendsByOneWindow(t *testing.T) {
	prov := alwaysToolProvider(0)
	eng := limitEngine(t, prov, 3)
	rec := &limitRecorder{decisions: []LimitDecision{LimitContinue, LimitStop}}
	eng.IterationLimitPrompt = rec.prompt

	_, _ = run(t, eng, "做点事")
	if len(rec.calls) != 2 {
		t.Fatalf("prompt called %d times, want 2 (once per window)", len(rec.calls))
	}
	if rec.calls[1].Iterations != 6 {
		t.Fatalf("second prompt at %d iterations, want 6", rec.calls[1].Iterations)
	}
	if n := len(toolRequests(prov)); n != 6 {
		t.Fatalf("model called %d times, want 6", n)
	}
}

func TestIterationLimitStopKeepsWorkAndResumes(t *testing.T) {
	prov := alwaysToolProvider(3) // requests 0..2 call tools, the 4th answers
	eng := limitEngine(t, prov, 3)
	eng.IterationLimitPrompt = func(LimitStats) LimitDecision { return LimitStop }

	if _, err := run(t, eng, "做点事"); err == nil {
		t.Fatal("want an error at the cap")
	}
	tools := 0
	for _, m := range eng.messages {
		if m.Role == "tool" && m.Content == "ok" {
			tools++
		}
	}
	if tools != 3 {
		t.Fatalf("completed tool results kept = %d, want 3", tools)
	}
	if eng.interrupted == nil {
		t.Fatal("the stopped turn must be resumable")
	}
	if !eng.HasInterruptedTurn() {
		t.Fatal("HasInterruptedTurn = false after a stop at the cap")
	}
	if msg, ok := eng.InterruptedTurn(); !ok || msg.Content != "做点事" {
		t.Fatalf("InterruptedTurn = %q, %v", msg.Content, ok)
	}

	reply, err := run(t, eng, "做点事")
	if err != nil || reply != "完成" {
		t.Fatalf("resume: reply=%q err=%v", reply, err)
	}
	users := 0
	for _, m := range eng.messages {
		if m.Role == "user" && m.Content == "做点事" {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("user message appears %d times after resume, want 1", users)
	}
}

func TestIterationLimitNilPromptNamesMaxTurns(t *testing.T) {
	eng := limitEngine(t, alwaysToolProvider(0), 3)
	_, err := run(t, eng, "做点事")
	if err == nil {
		t.Fatal("want an error at the cap")
	}
	var le *LimitError
	if !errors.As(err, &le) || le.Reason != LimitReasonIterations || le.Limit != 3 {
		t.Fatalf("err = %#v, want a *LimitError for 3 iterations", err)
	}
	// The engine does not know the front end: its text names the config key
	// only; /continue and --max-turns are added by the front end that has them.
	for _, want := range []string{"max_iterations", "3"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
	for _, not := range []string{"--max-turns", "/continue"} {
		if strings.Contains(err.Error(), not) {
			t.Fatalf("error %q names %q, which not every front end has", err, not)
		}
	}
}

func TestIterationLimitUnlimited(t *testing.T) {
	prov := alwaysToolProvider(20)
	eng := limitEngine(t, prov, 3)
	eng.SetMaxIterations(UnlimitedIterations)
	asked := false
	eng.IterationLimitPrompt = func(LimitStats) LimitDecision { asked = true; return LimitStop }

	reply, err := run(t, eng, "做点事")
	if err != nil || reply != "完成" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if asked {
		t.Fatal("an unlimited turn must never ask")
	}
	if n := len(prov.requests()); n != 21 {
		t.Fatalf("model called %d times, want 21", n)
	}
}

func TestIterationLimitDefaultIs200(t *testing.T) {
	eng := limitEngine(t, alwaysToolProvider(0), 0)
	if got := eng.MaxIterations(); got != DefaultMaxIterations || DefaultMaxIterations != 200 {
		t.Fatalf("MaxIterations() = %d (default %d), want 200", got, DefaultMaxIterations)
	}
}

// toolRequests are the requests that offered tools: the turn's own model
// calls, without the tool-less wrap-up summary a stopped turn ends with.
func toolRequests(p *seqProvider) []api.ChatRequest {
	var out []api.ChatRequest
	for _, r := range p.requests() {
		if len(r.Tools) > 0 {
			out = append(out, r)
		}
	}
	return out
}
