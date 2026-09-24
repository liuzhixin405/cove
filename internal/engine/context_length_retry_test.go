package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

// A request over the model's context window used to end the turn with an
// error, although compacting the history is exactly the remedy: the user
// had to notice, run /compact and send the message again.
func TestContextLengthErrorCompactsAndRetriesOnce(t *testing.T) {
	var history []api.Message
	for i := 0; i < 12; i++ {
		history = append(history,
			api.Message{Role: "user", Content: fmt.Sprintf("question %d about the parser", i)},
			api.Message{Role: "assistant", Content: fmt.Sprintf("answer %d about the parser", i)})
	}
	prov := &mockProvider{responses: []mockResponse{
		{err: &api.StatusError{Status: 400, Msg: `{"error":{"message":"This model's maximum context length is 131072 tokens"}}`}},
		{content: "Summary: the user asked a series of questions about the parser and got answers."},
		{content: "ok"},
	}}
	eng := newTestEngine(prov)
	eng.LoadMessages(history)

	reply, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "next"}, nil, nil)
	if err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	if reply != "ok" {
		t.Fatalf("reply = %q, want the answer from the retry", reply)
	}
}

// When compaction cannot shrink anything, the error is reported as before
// instead of retrying forever.
func TestContextLengthErrorWithNothingToCompactFails(t *testing.T) {
	ctxErr := &api.StatusError{Status: 400, Msg: "maximum context length exceeded"}
	prov := &mockProvider{responses: []mockResponse{{err: ctxErr}, {err: ctxErr}, {err: ctxErr}}}
	eng := newTestEngine(prov)
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "hi"}, nil, nil); err == nil {
		t.Fatal("turn succeeded although the request could not be made to fit")
	}
	prov.mu.Lock()
	calls := prov.callCount
	prov.mu.Unlock()
	if calls > 2 {
		t.Fatalf("provider called %d times, want at most one retry", calls)
	}
}
