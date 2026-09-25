package engine

import (
	"context"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/extract"
)

// panicProvider panics on every call.
type panicProvider struct{ *slowExtractProvider }

func (panicProvider) Chat(context.Context, api.ChatRequest) (*api.ChatResponse, error) {
	panic("extraction exploded")
}

// A panic in the turn-end work before the review ran must not leave the
// review marked running, which would disable it for the rest of the session.
func TestReviewRunningResetAfterEarlyPanic(t *testing.T) {
	prov := &mockProvider{responses: workTurnResponses()}
	eng := workReviewEngine(t, prov)
	armReview(eng)
	eng.setExtractRunner(extract.NewRunner(panicProvider{&slowExtractProvider{}}, "m"))
	if _, err := run(t, eng, "question"); err != nil {
		t.Fatal(err)
	}
	eng.WaitBackground(context.Background())
	eng.waitReview()
	eng.bgMu.Lock()
	running := eng.reviewRunning
	eng.bgMu.Unlock()
	if running {
		t.Fatal("reviewRunning stuck after a panic before the review")
	}
}
