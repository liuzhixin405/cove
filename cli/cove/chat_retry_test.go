package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

// flakyRunner streams part of an answer, then fails once with a network error.
type flakyRunner struct{ calls int }

func (f *flakyRunner) RunWithStream(ctx context.Context, input string, onDelta func(string)) (string, error) {
	return f.RunMessageWithStream(ctx, api.Message{Role: "user", Content: input}, onDelta, nil)
}

func (f *flakyRunner) RunMessageWithStream(ctx context.Context, msg api.Message, onDelta func(string), onReasoning func(string)) (string, error) {
	f.calls++
	if f.calls == 1 {
		onDelta("partial answer")
		return "", errors.New("read tcp: connection reset by peer")
	}
	onDelta(" and the rest")
	return " and the rest", nil
}

// Retrying after text had already streamed used to be skipped: a retry re-ran
// the whole turn, duplicating output and work. The engine now keeps completed
// steps and resumes when the same message is sent again, so a transient
// failure mid-answer is retried like any other.
func TestTransientFailureAfterStreamingIsRetried(t *testing.T) {
	r := &flakyRunner{}
	transcript, err := runChatInteractionMessage(context.Background(), r, api.Message{Role: "user", Content: "q"})
	if err != nil {
		t.Fatalf("err = %v, want a successful retry", err)
	}
	if !strings.Contains(transcript, "and the rest") {
		t.Fatalf("resumed answer missing from the transcript: %q", transcript)
	}
	if r.calls != 2 {
		t.Fatalf("runner called %d times, want 2", r.calls)
	}
}
