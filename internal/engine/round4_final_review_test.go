package engine

import (
	"context"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/permission"
)

// workTurnResponses are the two model replies of a turn that runs a tool
// that is not read-only ("write") and then answers.
func workTurnResponses() []mockResponse {
	return []mockResponse{
		{toolCalls: []api.ToolCall{{ID: "w1", Name: "write", Input: map[string]any{"input": "x"}}}},
		{content: "The change is done and nothing remains to be handled here."},
	}
}

// workReviewEngine is reviewEngine with a write tool, permissions bypassed
// and the done check off, so a work turn costs exactly workTurnResponses.
func workReviewEngine(t *testing.T, prov *mockProvider) *Engine {
	t.Helper()
	eng := reviewEngine(t, prov)
	eng.registry.Register(&mockTool{name: "write", result: "written"})
	eng.perm.SetMode(permission.Bypass)
	eng.config.DoneCheck = "off"
	var history []api.Message
	for i := 0; i < 4; i++ {
		history = append(history, api.Message{Role: "user", Content: "q"}, api.Message{Role: "assistant", Content: "a"})
	}
	eng.LoadMessages(history)
	return eng
}

// armReview makes the next turn end the reviewMinTurns-th since the last
// review, for tests about what the review does rather than when it runs.
func armReview(eng *Engine) {
	eng.bgMu.Lock()
	eng.turnsSinceReview = reviewMinTurns - 1
	eng.bgMu.Unlock()
}

func providerCalls(p *mockProvider) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.callCount
}

// Important 2: a -p engine (SetNonInteractive) never sends the review
// request, even on a turn that would otherwise qualify.
func TestNonInteractiveEngineNeverReviews(t *testing.T) {
	var resps []mockResponse
	for i := 0; i < reviewMinTurns; i++ {
		resps = append(resps, workTurnResponses()...)
	}
	prov := &mockProvider{responses: append(resps, mockResponse{content: "SKILL: x | y | z"})}
	eng := workReviewEngine(t, prov)
	eng.SetNonInteractive(true)
	for i := 0; i < reviewMinTurns; i++ {
		if _, err := run(t, eng, "change it"); err != nil {
			t.Fatal(err)
		}
		eng.WaitBackground(context.Background())
		eng.waitReview()
	}
	if got, want := providerCalls(prov), 2*reviewMinTurns; got != want {
		t.Fatalf("model calls = %d, want %d (no review request)", got, want)
	}
}

// Important 2: an interactive engine reviews on the third tool-using turn,
// not on the first two.
func TestInteractiveReviewWaitsForThirdWorkTurn(t *testing.T) {
	var resps []mockResponse
	for i := 0; i < reviewMinTurns; i++ {
		resps = append(resps, workTurnResponses()...)
	}
	prov := &mockProvider{responses: append(resps, mockResponse{content: "NONE"})}
	eng := workReviewEngine(t, prov)
	for i := 1; i <= reviewMinTurns; i++ {
		if _, err := run(t, eng, "change it"); err != nil {
			t.Fatal(err)
		}
		eng.WaitBackground(context.Background())
		eng.waitReview()
		want := 2 * i
		if i == reviewMinTurns {
			want++ // the review request
		}
		if got := providerCalls(prov); got != want {
			t.Fatalf("after turn %d: model calls = %d, want %d", i, got, want)
		}
	}
}

// Important 2: a turn that ran no tool that changes anything is not
// reviewed, however many turns have passed.
func TestReviewSkipsTurnWithoutWorkTool(t *testing.T) {
	prov := &mockProvider{responses: []mockResponse{{content: "The answer is forty-two, as computed."}, {content: "NONE"}}}
	eng := workReviewEngine(t, prov)
	armReview(eng)
	if _, err := run(t, eng, "question"); err != nil {
		t.Fatal(err)
	}
	eng.WaitBackground(context.Background())
	eng.waitReview()
	time.Sleep(50 * time.Millisecond)
	if got := providerCalls(prov); got != 1 {
		t.Fatalf("model calls = %d, want 1 (no review after a question)", got)
	}
}
