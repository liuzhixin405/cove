package covemobile

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

// fakeProvider streams a fixed reasoning and answer, the way the real
// OpenAI-compatible provider does: every piece goes to the handler AND is
// accumulated into the returned response.
type fakeProvider struct{ sawDeadline bool }

func (p *fakeProvider) Name() string        { return "fake" }
func (p *fakeProvider) DisplayName() string { return "fake" }
func (p *fakeProvider) Validate() error     { return nil }
func (p *fakeProvider) Chat(ctx context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
	return p.ChatStream(ctx, req, nil)
}

func (p *fakeProvider) ChatStream(ctx context.Context, req api.ChatRequest, onEvent api.StreamHandler) (*api.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_, p.sawDeadline = ctx.Deadline()
	if onEvent != nil {
		onEvent(api.StreamEvent{Reasoning: "think-1 "})
		onEvent(api.StreamEvent{Reasoning: "think-2"})
		onEvent(api.StreamEvent{Delta: "answer"})
	}
	return &api.ChatResponse{Content: "answer", ReasoningContent: "think-1 think-2"}, nil
}

type toolCall struct{ name, input string }

type recorder struct {
	mu        sync.Mutex
	deltas    strings.Builder
	reasoning strings.Builder
	calls     []toolCall
	done      string
	err       string
}

func (r *recorder) OnDelta(s string) { r.mu.Lock(); r.deltas.WriteString(s); r.mu.Unlock() }
func (r *recorder) OnToolCall(name, input string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, toolCall{name, input})
	return "tapped"
}
func (r *recorder) OnDone(resp string)   { r.done = resp }
func (r *recorder) OnReasoning(s string) { r.mu.Lock(); r.reasoning.WriteString(s); r.mu.Unlock() }
func (r *recorder) OnError(e string)     { r.err = e }

func newTestEngine(p api.Provider) *MobileEngine {
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

func TestResolveProviderNameKeepsMobileDefaults(t *testing.T) {
	cases := []struct{ provider, model, want string }{
		{"", "deepseek-chat", "deepseek"},
		{"", "DeepSeek-Reasoner", "deepseek"},
		{"", "claude-sonnet-4", "anthropic"},
		{"", "gpt-4o", "openai"},
		{"", "some-local-model", "openai"}, // api.DetectProvider would say anthropic
		{"kimi", "moonshot-v1", "kimi"},
	}
	for _, c := range cases {
		if got := resolveProviderName(c.provider, c.model); got != c.want {
			t.Errorf("resolveProviderName(%q,%q) = %q, want %q", c.provider, c.model, got, c.want)
		}
	}
}

// sseTurns serves one scripted SSE response per request, in order, and
// records every request body.
type sseTurns struct {
	mu     sync.Mutex
	turns  [][]string
	bodies []map[string]any
}

func (s *sseTurns) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		i := len(s.bodies)
		s.bodies = append(s.bodies, body)
		s.mu.Unlock()
		if i >= len(s.turns) {
			http.Error(w, "no more turns", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range s.turns[i] {
			_, _ = io.WriteString(w, l+"\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func realEngine(t *testing.T, s *sseTurns) *MobileEngine {
	t.Helper()
	srv := s.server(t)
	e := &MobileEngine{}
	e.Init("k", "deepseek-chat", "deepseek", srv.URL)
	e.AddTool("tap", "Tap the screen", `{"type":"object","properties":{"x":{"type":"integer"},"y":{"type":"integer"}}}`)
	return e
}

// TestChatStreamToolLoopThroughInternalAPI drives the whole loop over the
// real internal/api OpenAI-compatible provider: the SSE parse ("data:" with
// no space), tool-argument JSON repair (stray text around the object), dispatch to the
// Kotlin callback, and the tool result plus reasoning_content going back on
// the next request.
func TestChatStreamToolLoopThroughInternalAPI(t *testing.T) {
	s := &sseTurns{turns: [][]string{
		{
			`data:{"choices":[{"delta":{"reasoning_content":"need a tap"}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"tap","arguments":"args: {\"x\":1,"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"y\":2} (end)"}}]},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		},
		{
			`data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		},
	}}
	rec := &recorder{}
	realEngine(t, s).ChatStream("tap it", 30, rec)
	if rec.err != "" {
		t.Fatalf("OnError(%q)", rec.err)
	}
	if rec.done != "done" || rec.deltas.String() != "done" {
		t.Fatalf("done=%q deltas=%q", rec.done, rec.deltas.String())
	}
	if rec.reasoning.String() != "need a tap" {
		t.Fatalf("reasoning=%q", rec.reasoning.String())
	}
	if len(rec.calls) != 1 || rec.calls[0].name != "tap" {
		t.Fatalf("tool calls = %+v", rec.calls)
	}
	var in map[string]any
	if err := json.Unmarshal([]byte(rec.calls[0].input), &in); err != nil || in["x"] != float64(1) || in["y"] != float64(2) {
		t.Fatalf("tool input %q not repaired: %v", rec.calls[0].input, err)
	}
	if len(s.bodies) != 2 {
		t.Fatalf("got %d requests, want 2", len(s.bodies))
	}
	msgs, _ := s.bodies[1]["messages"].([]any)
	var sawTool, sawReasoning bool
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["role"] == "tool" && mm["tool_call_id"] == "c1" && mm["content"] == "tapped" {
			sawTool = true
		}
		if mm["role"] == "assistant" && mm["reasoning_content"] == "need a tap" {
			sawReasoning = true
		}
	}
	if !sawTool || !sawReasoning {
		t.Fatalf("second request lacks tool result (%v) or reasoning_content (%v): %v", sawTool, sawReasoning, msgs)
	}
}

// TestChatStreamUnparseableOrNamelessToolCallsNotDispatched: arguments that
// cannot be repaired, and fragments at bogus stream indices that never name a
// tool, must not reach the phone; the model gets an error result instead.
// (Negative indices used to panic the old mobileapi copy.)
func TestChatStreamUnparseableOrNamelessToolCallsNotDispatched(t *testing.T) {
	s := &sseTurns{turns: [][]string{
		{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"tap","arguments":"not json at all"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":-99,"function":{"arguments":"{}"}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		},
		{
			`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		},
	}}
	rec := &recorder{}
	realEngine(t, s).ChatStream("tap it", 30, rec)
	if rec.err != "" {
		t.Fatalf("OnError(%q)", rec.err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("dispatched %+v to the phone", rec.calls)
	}
	if rec.done != "ok" {
		t.Fatalf("OnDone(%q)", rec.done)
	}
	msgs, _ := s.bodies[1]["messages"].([]any)
	errs := 0
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if c, _ := mm["content"].(string); mm["role"] == "tool" && strings.HasPrefix(c, "Error:") {
			errs++
		}
	}
	if errs == 0 {
		t.Fatalf("model got no error tool result: %v", msgs)
	}
}

// TestChatStreamReportsTruncatedStream: a stream that closes with neither
// finish_reason nor [DONE] is an error, not a complete answer.
func TestChatStreamReportsTruncatedStream(t *testing.T) {
	s := &sseTurns{turns: [][]string{{`data: {"choices":[{"delta":{"content":"half"}}]}`}}}
	rec := &recorder{}
	realEngine(t, s).ChatStream("hi", 30, rec)
	if rec.err == "" || rec.done != "" {
		t.Fatalf("err=%q done=%q, want an error", rec.err, rec.done)
	}
}

// An installed app with "openai-compatible" (or a name the old client did
// not know) and no base URL kept talking to DeepSeek; it must still.
func TestMobileBaseURLKeepsTheOldDefault(t *testing.T) {
	cases := []struct{ provider, baseURL, want string }{
		{"openai-compatible", "", legacyMobileBaseURL},
		{"my-proxy", "", legacyMobileBaseURL},
		{"deepseek", "", ""},
		{"openai", "", ""},
		{"glm", "", ""},
		{"", "", ""},
		{"openai-compatible", "https://example.test/v1", "https://example.test/v1"},
	}
	for _, c := range cases {
		if got := mobileBaseURL(c.provider, c.baseURL); got != c.want {
			t.Errorf("mobileBaseURL(%q, %q) = %q, want %q", c.provider, c.baseURL, got, c.want)
		}
	}
}
