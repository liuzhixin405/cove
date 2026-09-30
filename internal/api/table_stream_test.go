package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// T5: table-driven tests of the streamed reply parsers (anthropicProvider and
// openAICompatProvider ChatStream) behind an httptest server. Each case in
// table_stream_corpus_test.go is a scripted SSE reply; the runner serves it
// under every chunking, and checks per case:
//
//   - the final ChatResponse, the handler's callbacks and the error's
//     Classify / isTemporary / statusOf against the case's expectation;
//   - (a) chunking independence: every chunking yields the same response,
//     the same callbacks and the same error classification;
//   - (b) stream vs non-stream: Chat on the equivalent JSON body yields the
//     same content, tool calls, thinking, stop reason and usage;
//   - (c) an in-stream error classifies like an HTTP error with its status
//     (through both ChatStream and Chat);
//   - (d) no goroutine leak, and the server's Close does not hang (the
//     client closed the body, so a hanging handler saw its request end).

const (
	t5Anthropic = "anthropic"
	t5OpenAI    = "openai_compat"
)

// t5Term is how the server ends the reply after the scripted events.
type t5Term int

const (
	t5Normal t5Term = iota // handler returns: clean end of body
	t5Abort                // connection dropped without the chunked terminator
	t5Hang                 // server stops sending and keeps the connection → idle watchdog
	t5Cancel               // server hangs; the caller cancels ctx at the first callback
)

func (t t5Term) String() string {
	return [...]string{"normal", "abort", "hang", "ctx-cancel"}[t]
}

// t5View is the comparable projection of a ChatResponse.
type t5View struct {
	Content   string
	Reasoning string
	ToolCalls []t5TC
	Thinking  []string // canonical JSON of each thinking block
	Stop      string
	In        int
	Out       int
	Hit       int
	Miss      int
	Write     int
	ReasonTok int
}

type t5TC struct {
	ID         string
	Name       string
	Input      string // canonical JSON
	ParseError bool
}

// t5Err is the classification of a returned error.
type t5Err struct {
	Kind   ErrorKind
	Temp   bool
	Status int
}

func (e t5Err) String() string {
	return fmt.Sprintf("{Kind:%v Temp:%v Status:%d}", e.Kind, e.Temp, e.Status)
}

type t5Case struct {
	name     string
	note     string
	provider string
	events   []string // SSE events, each with its own terminating blank line
	term     t5Term
	// nonStream, when set, is the Chat (non-streaming) body of the same
	// logical reply, for invariant (b).
	nonStream string
	want      *t5View
	wantErr   *t5Err
	// httpStatus/httpBody: the HTTP error the in-stream error stands for,
	// for invariant (c).
	httpStatus int
	httpBody   string
	// anyIDs: tool call IDs are generated; compared as "<generated>".
	anyIDs bool
	// only, when set, limits the chunkings (by name), e.g. for a body too
	// large to write one byte at a time.
	only []string
	skip string
}

// t5Chunking cuts the scripted reply into the writes the server makes.
type t5Chunking struct {
	name string
	cut  func(events []string) []string
	// eolSensitive: drops the final newline, so it changes what the stream
	// means when the server does not end the body itself (abort/hang).
	eolSensitive bool
}

func t5Bytes(s string) []string {
	out := make([]string, 0, len(s))
	for i := 0; i < len(s); i++ {
		out = append(out, s[i:i+1])
	}
	return out
}

func t5CutAt(s string, cuts []int) []string {
	var out []string
	prev := 0
	for _, c := range cuts {
		if c > prev && c < len(s) {
			out = append(out, s[prev:c])
			prev = c
		}
	}
	return append(out, s[prev:])
}

func t5ToCRLF(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

// t5MixedEOL turns every other "\n" into "\r\n".
func t5MixedEOL(s string) string {
	var b strings.Builder
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if n%2 == 1 {
				b.WriteByte('\r')
			}
			n++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func t5Fixed(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	return append(out, s)
}

var t5Chunkings = []t5Chunking{
	{name: "whole", cut: func(ev []string) []string { return []string{strings.Join(ev, "")} }},
	{name: "bytewise", cut: func(ev []string) []string { return t5Bytes(strings.Join(ev, "")) }},
	{name: "split-data-prefix", cut: func(ev []string) []string {
		body := strings.Join(ev, "")
		var cuts []int
		for i := 0; i < len(body); i++ {
			if strings.HasPrefix(body[i:], "data:") {
				cuts = append(cuts, i+2) // "da" | "ta:"
			}
		}
		return t5CutAt(body, cuts)
	}},
	{name: "split-json", cut: func(ev []string) []string {
		body := strings.Join(ev, "")
		var cuts []int
		start := 0
		for _, line := range strings.SplitAfter(body, "\n") {
			if strings.HasPrefix(line, "data:") && len(line) > 8 {
				cuts = append(cuts, start+5+(len(line)-5)/2)
			}
			start += len(line)
		}
		return t5CutAt(body, cuts)
	}},
	{name: "events-x3", cut: func(ev []string) []string {
		var out []string
		for i := 0; i < len(ev); i += 3 {
			j := min(i+3, len(ev))
			out = append(out, strings.Join(ev[i:j], ""))
		}
		return out
	}},
	{name: "crlf-bytewise", cut: func(ev []string) []string { return t5Bytes(t5ToCRLF(strings.Join(ev, ""))) }},
	{name: "mixed-eol-7byte", cut: func(ev []string) []string { return t5Fixed(t5MixedEOL(strings.Join(ev, "")), 7) }},
	{name: "no-final-eol", eolSensitive: true, cut: func(ev []string) []string {
		return []string{strings.TrimRight(strings.Join(ev, ""), "\r\n")}
	}},
}

// t5Plan is what the server does for the next request.
type t5Plan struct {
	status      int
	contentType string
	chunks      []string
	term        t5Term
}

type t5Server struct {
	srv   *httptest.Server
	mu    sync.Mutex
	plan  t5Plan
	stuck int // hanging handlers whose request never ended: body not closed
}

func newT5Server() *t5Server {
	s := &t5Server{}
	s.srv = httptest.NewServer(s)
	return s
}

func (s *t5Server) set(p t5Plan) {
	s.mu.Lock()
	s.plan = p
	s.mu.Unlock()
}

func (s *t5Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	s.mu.Lock()
	p := s.plan
	s.mu.Unlock()
	w.Header().Set("Content-Type", p.contentType)
	w.WriteHeader(p.status)
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}
	for _, c := range p.chunks {
		_, _ = io.WriteString(w, c)
		if fl != nil {
			fl.Flush()
		}
	}
	switch p.term {
	case t5Abort:
		panic(http.ErrAbortHandler)
	case t5Hang, t5Cancel:
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
			s.mu.Lock()
			s.stuck++
			s.mu.Unlock()
		}
	}
}

func t5Request(provider string) ChatRequest {
	model := "deepseek-v4-pro"
	if provider == t5Anthropic {
		model = "claude-opus-5"
	}
	return ChatRequest{Model: model, MaxTokens: 64, Messages: []Message{{Role: "user", Content: "hi"}}}
}

func t5Provider(provider, url string, c *http.Client) Provider {
	if provider == t5Anthropic {
		return &anthropicProvider{apiKey: "k", baseURL: url + "/v1", client: c}
	}
	return &openAICompatProvider{apiKey: "k", baseURL: url + "/v1", client: c}
}

func t5Canon(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<marshal error %v>", err)
	}
	return string(b)
}

func t5CanonRaw(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "<invalid json " + string(raw) + ">"
	}
	return t5Canon(v)
}

func t5ViewOf(r *ChatResponse, anyIDs bool) *t5View {
	if r == nil {
		return nil
	}
	v := &t5View{
		Content: r.Content, Reasoning: r.ReasoningContent, Stop: r.StopReason,
		In: r.InputTokens, Out: r.OutputTokens, Hit: r.PromptCacheHitTokens,
		Miss: r.PromptCacheMissTokens, Write: r.PromptCacheWriteTokens, ReasonTok: r.ReasoningTokens,
	}
	for _, tc := range r.ToolCalls {
		id := tc.ID
		if anyIDs && id != "" {
			id = "<generated>"
		}
		v.ToolCalls = append(v.ToolCalls, t5TC{ID: id, Name: tc.Name, Input: t5Canon(tc.Input), ParseError: tc.ParseError})
	}
	for _, raw := range r.ThinkingBlocks {
		v.Thinking = append(v.Thinking, t5CanonRaw(raw))
	}
	return v
}

func t5ErrOf(err error) *t5Err {
	if err == nil {
		return nil
	}
	return &t5Err{Kind: Classify(err), Temp: isTemporary(err), Status: statusOf(err)}
}

func t5ErrString(e *t5Err) string {
	if e == nil {
		return "<nil>"
	}
	return e.String()
}

// t5Outcome is what one call produced.
type t5Outcome struct {
	view   *t5View
	err    *t5Err
	rawErr error
	events []string
}

func (s *t5Server) stream(t *testing.T, c t5Case, chunks []string) t5Outcome {
	t.Helper()
	s.set(t5Plan{status: http.StatusOK, contentType: "text/event-stream", chunks: chunks, term: c.term})
	idle := 5 * time.Second
	if c.term == t5Hang {
		idle = 80 * time.Millisecond
	}
	old := streamIdleTimeout
	streamIdleTimeout = idle
	defer func() { streamIdleTimeout = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []string
	handler := func(ev StreamEvent) {
		switch ev.Type {
		case "delta":
			events = append(events, "delta:"+ev.Delta)
		case "reasoning":
			events = append(events, "reasoning:"+ev.Reasoning)
		default:
			tc := ""
			if ev.ToolCall != nil {
				tc = ev.ToolCall.ID + "/" + ev.ToolCall.Name
			}
			events = append(events, ev.Type+":"+ev.Delta+ev.Reasoning+tc)
		}
		if c.term == t5Cancel {
			cancel()
		}
	}
	p := t5Provider(c.provider, s.srv.URL, s.srv.Client())
	done := make(chan t5Outcome, 1)
	go func() {
		resp, err := p.ChatStream(ctx, t5Request(c.provider), handler)
		done <- t5Outcome{view: t5ViewOf(resp, c.anyIDs), err: t5ErrOf(err), rawErr: err, events: events}
	}()
	select {
	case o := <-done:
		return o
	case <-time.After(5 * time.Second):
		t.Fatalf("ChatStream did not return within 5s (term=%v)", c.term)
		return t5Outcome{}
	}
}

func (s *t5Server) chat(c t5Case, status int, body string) t5Outcome {
	s.set(t5Plan{status: status, contentType: "application/json", chunks: []string{body}})
	p := t5Provider(c.provider, s.srv.URL, s.srv.Client())
	resp, err := p.Chat(context.Background(), t5Request(c.provider))
	return t5Outcome{view: t5ViewOf(resp, c.anyIDs), err: t5ErrOf(err), rawErr: err}
}

func (s *t5Server) streamHTTPError(c t5Case) t5Outcome {
	s.set(t5Plan{status: c.httpStatus, contentType: "application/json", chunks: []string{c.httpBody}})
	p := t5Provider(c.provider, s.srv.URL, s.srv.Client())
	resp, err := p.ChatStream(context.Background(), t5Request(c.provider), nil)
	return t5Outcome{view: t5ViewOf(resp, c.anyIDs), err: t5ErrOf(err), rawErr: err}
}

func t5Check(t *testing.T, c t5Case, where, outlet string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("case %q provider=%s term=%v %s outlet=%s\nbody: %q\n got: %+v\nwant: %+v",
			c.name, c.provider, c.term, where, outlet, strings.Join(c.events, ""), got, want)
	}
}

func runT5Case(t *testing.T, c t5Case) {
	oldRetry := defaultRetry
	defaultRetry = retryConfig{MaxRetries: 0, BaseDelay: time.Millisecond}
	defer func() { defaultRetry = oldRetry }()

	before := runtime.NumGoroutine()
	s := newT5Server()
	client := s.srv.Client()

	var first *t5Outcome
	firstName := ""
	for _, ch := range t5Chunkings {
		if ch.eolSensitive && c.term != t5Normal {
			continue
		}
		if len(c.only) > 0 && !slices.Contains(c.only, ch.name) {
			continue
		}
		o := s.stream(t, c, ch.cut(c.events))
		where := "chunking=" + ch.name
		t5Check(t, c, where, "ChatStream.response", o.view, c.want)
		if !reflect.DeepEqual(o.err, c.wantErr) {
			t.Errorf("case %q provider=%s term=%v %s outlet=ChatStream.error\nbody: %q\n got: %s (%v)\nwant: %s",
				c.name, c.provider, c.term, where, strings.Join(c.events, ""), t5ErrString(o.err), o.rawErr, t5ErrString(c.wantErr))
		}
		// Callback consistency: the text callbacks add up to the content
		// and the reasoning; no tool-call callback contradicts ToolCalls.
		if o.view != nil {
			var text, reasoning strings.Builder
			for _, e := range o.events {
				switch {
				case strings.HasPrefix(e, "delta:"):
					text.WriteString(strings.TrimPrefix(e, "delta:"))
				case strings.HasPrefix(e, "reasoning:"):
					reasoning.WriteString(strings.TrimPrefix(e, "reasoning:"))
				default:
					t.Errorf("case %q provider=%s %s outlet=handler: unexpected callback %q", c.name, c.provider, where, e)
				}
			}
			t5Check(t, c, where, "handler.delta-concat==Content", text.String(), o.view.Content)
			wantReasoning := o.view.Reasoning
			if c.provider == t5Anthropic {
				wantReasoning = t5ThinkingText(o.view.Thinking)
			}
			t5Check(t, c, where, "handler.reasoning-concat", reasoning.String(), wantReasoning)
		}
		// (a) chunking independence.
		if first == nil {
			oc := o
			first, firstName = &oc, ch.name
			continue
		}
		cmp := where + " vs chunking=" + firstName
		t5Check(t, c, cmp, "invariant(a).response", o.view, first.view)
		t5Check(t, c, cmp, "invariant(a).error", o.err, first.err)
		if c.term != t5Cancel {
			t5Check(t, c, cmp, "invariant(a).callbacks", o.events, first.events)
		}
	}

	// (b) stream vs non-stream.
	if c.nonStream != "" && first != nil {
		o := s.chat(c, http.StatusOK, c.nonStream)
		where := fmt.Sprintf("Chat(non-stream body %q)", c.nonStream)
		t5Check(t, c, where, "invariant(b).response", o.view, first.view)
		t5Check(t, c, where, "invariant(b).error", o.err, first.err)
	}

	// (c) in-stream error == HTTP error with the same status.
	if c.httpStatus != 0 && first != nil {
		for _, call := range []string{"ChatStream", "Chat"} {
			var o t5Outcome
			if call == "Chat" {
				o = s.chat(c, c.httpStatus, c.httpBody)
			} else {
				o = s.streamHTTPError(c)
			}
			where := fmt.Sprintf("%s(HTTP %d body %q)", call, c.httpStatus, c.httpBody)
			if o.err == nil {
				t.Errorf("case %q provider=%s %s outlet=invariant(c): no error", c.name, c.provider, where)
				continue
			}
			t5Check(t, c, where, "invariant(c).error", o.err, first.err)
		}
	}

	// (d) Close must not hang, the body must have been closed, nothing leaks.
	closed := make(chan struct{})
	go func() { s.srv.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Errorf("case %q provider=%s term=%v outlet=server.Close: hangs (a handler never finished)", c.name, c.provider, c.term)
	}
	client.CloseIdleConnections()
	s.mu.Lock()
	stuck := s.stuck
	s.mu.Unlock()
	if stuck > 0 {
		t.Errorf("case %q provider=%s term=%v outlet=response-body: %d hanging request(s) never ended: the client did not close the body", c.name, c.provider, c.term, stuck)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		buf := make([]byte, 1<<16)
		buf = buf[:runtime.Stack(buf, true)]
		t.Errorf("case %q provider=%s term=%v outlet=goroutines: %d before, %d after\n%s", c.name, c.provider, c.term, before, n, buf)
	}
}

// t5ThinkingText is the concatenated thinking text of Anthropic thinking
// blocks (redacted blocks carry none).
func t5ThinkingText(blocks []string) string {
	var b strings.Builder
	for _, raw := range blocks {
		var m struct {
			Thinking string `json:"thinking"`
		}
		_ = json.Unmarshal([]byte(raw), &m)
		b.WriteString(m.Thinking)
	}
	return b.String()
}

func TestTableStream(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range t5Corpus() {
		name := c.provider + "/" + c.name
		if seen[name] {
			t.Fatalf("duplicate case name %q", name)
		}
		seen[name] = true
		t.Run(name, func(t *testing.T) {
			// T5_NOSKIP=1 runs the cases marked as known product bugs.
			if c.skip != "" && os.Getenv("T5_NOSKIP") == "" {
				t.Skip(c.skip)
			}
			runT5Case(t, c)
		})
	}
}
