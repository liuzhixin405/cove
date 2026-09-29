package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// On a fresh turn there is no old history to summarise, yet the request
// can still be over the window: the turn note (repo map excerpt, memories)
// and a huge tool result are what does not fit. The retry drops the note
// and cuts the result instead of resending the same request.
func TestContextLengthErrorOnAFreshTurnDropsTheNoteAndCutsToolResults(t *testing.T) {
	big := strings.Repeat("skill text line\n", 4000)
	prov := &mockProvider{responses: []mockResponse{
		{toolCalls: []api.ToolCall{{ID: "c1", Name: "skill_view", Input: map[string]any{"name": "x"}}}},
		{err: &api.StatusError{Status: 400, Msg: `{"error":{"code":400,"message":"request (17773 tokens) exceeds the available context size (16384 tokens), try increasing it"}}`}},
		{content: "ok"},
	}}
	eng := newTestEngine(prov, &mockTool{name: "skill_view", readOnly: true, result: big})
	eng.LoadMessages([]api.Message{
		{Role: "user", Content: "hello earlier"},
		{Role: "user", Content: "<repo_map_excerpt>\nlots of symbols\n</repo_map_excerpt>", Synthetic: true},
		{Role: "assistant", Content: "hi"},
	})

	reply, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: `D:\other\proj 写一个项目`}, nil, nil)
	if err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	if reply != "ok" {
		t.Fatalf("reply = %q, want the retry's answer", reply)
	}
	prov.mu.Lock()
	last := prov.lastReq
	prov.mu.Unlock()
	var sawRequest bool
	for _, m := range last.Messages {
		if strings.Contains(m.Content, "<repo_map_excerpt>") {
			t.Fatalf("retry still carries the repo map excerpt")
		}
		if m.Role == "tool" && len(m.Content) > len(big)/2 {
			t.Fatalf("retry still carries the full %d-byte tool result (%d bytes)", len(big), len(m.Content))
		}
		if m.Role == "user" && strings.Contains(m.Content, "写一个项目") {
			sawRequest = true
		}
	}
	if !sawRequest {
		t.Fatalf("the retry lost the user's request:\n%+v", last.Messages)
	}
}

// Re-sending a request after an overflow used to append one more
// "[system: 上一次执行被中断…]" marker each time: four attempts grew the
// request by 80 tokens each and never got smaller. One marker is enough.
func TestRepeatedResumeAfterOverflowKeepsOneMarker(t *testing.T) {
	overflow := &api.StatusError{Status: 400, Msg: "maximum context length exceeded"}
	prov := &mockProvider{responses: []mockResponse{
		{err: overflow}, {err: overflow}, // first attempt + its retry
		{err: overflow}, {err: overflow}, // second attempt
		{err: overflow}, {err: overflow}, // third attempt
	}}
	eng := newTestEngine(prov)
	msg := api.Message{Role: "user", Content: "把 README 翻译成英文"}
	for i := 0; i < 3; i++ {
		if _, err := eng.RunMessageWithStream(context.Background(), msg, nil, nil); err == nil {
			t.Fatalf("attempt %d succeeded unexpectedly", i)
		}
	}
	markers := 0
	for _, m := range eng.messages {
		if m.Synthetic && strings.Contains(m.Content, "上一次执行被中断") {
			markers++
		}
	}
	if markers > 1 {
		t.Fatalf("%d resume markers in history, want at most 1:\n%+v", markers, eng.messages)
	}
}
