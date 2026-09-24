package covemobile

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/mobile/mobileapi"
)

// fakeProvider streams a fixed reasoning and answer, the way the real
// OpenAI-compatible provider does: every piece goes to onEvent AND is
// accumulated into the returned response.
type fakeProvider struct{ sawDeadline bool }

func (p *fakeProvider) ChatStream(ctx context.Context, req mobileapi.ChatRequest, onEvent func(mobileapi.StreamEvent)) (mobileapi.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return mobileapi.ChatResponse{}, err
	}
	_, p.sawDeadline = ctx.Deadline()
	onEvent(mobileapi.StreamEvent{Reasoning: "think-1 "})
	onEvent(mobileapi.StreamEvent{Reasoning: "think-2"})
	onEvent(mobileapi.StreamEvent{Delta: "answer"})
	return mobileapi.ChatResponse{Content: "answer", ReasoningContent: "think-1 think-2"}, nil
}

type recorder struct {
	reasoning strings.Builder
	done      string
	err       string
}

func (r *recorder) OnDelta(string)                   {}
func (r *recorder) OnToolCall(string, string) string { return "" }
func (r *recorder) OnDone(resp string)               { r.done = resp }
func (r *recorder) OnReasoning(s string)             { r.reasoning.WriteString(s) }
func (r *recorder) OnError(e string)                 { r.err = e }

func newTestEngine(p mobileapi.Provider) *MobileEngine {
	e := &MobileEngine{}
	e.Init("", "deepseek-chat", "deepseek", "http://127.0.0.1:0")
	e.provider = p
	return e
}

// TestChatStreamDeliversReasoningOnce: the reasoning was streamed to the
// callback piece by piece and then, once the response finished, sent again
// in full — so the app showed every thought twice.
func TestChatStreamDeliversReasoningOnce(t *testing.T) {
	rec := &recorder{}
	newTestEngine(&fakeProvider{}).ChatStream("hi", 30, rec)
	if rec.err != "" {
		t.Fatalf("OnError(%q)", rec.err)
	}
	if got := rec.reasoning.String(); got != "think-1 think-2" {
		t.Fatalf("reasoning delivered as %q, want it exactly once", got)
	}
	if rec.done != "answer" {
		t.Fatalf("OnDone(%q)", rec.done)
	}
}

// TestChatStreamNonPositiveTimeoutMeansNoDeadline: timeoutSecs <= 0 produced
// an already-expired context, so a caller passing 0 ("no timeout") got
// "timeout" before any request was sent.
func TestChatStreamNonPositiveTimeoutMeansNoDeadline(t *testing.T) {
	for _, secs := range []int{0, -1} {
		p := &fakeProvider{}
		rec := &recorder{}
		newTestEngine(p).ChatStream("hi", secs, rec)
		if rec.err != "" || rec.done != "answer" {
			t.Fatalf("timeoutSecs=%d: err=%q done=%q", secs, rec.err, rec.done)
		}
		if p.sawDeadline {
			t.Fatalf("timeoutSecs=%d: the request carried a deadline", secs)
		}
	}
}
