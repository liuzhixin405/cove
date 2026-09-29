package main

// End-to-end scenario harness.
//
// Every incident of 2026-09-27 (input swallowed by a prompt, a queued task
// misreported, a local model's 16K window, the same task restarted four
// times) passed its unit tests and failed in the person's terminal. These
// tests run the real REPL loop against a fake OpenAI-compatible server and a
// real (piped) stdin, in the plain-readline mode the terminal falls back to
// when it is not a TTY, so the whole chain — input dispatch, task runner,
// permission relay, engine, provider, diagnostics, session store — is
// exercised the way a person drives it. What they cannot cover is the raw
// terminal rendering (the pinned row), which pinned_test.go covers.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/diagnostic"
	"github.com/liuzhixin405/cove-agent/internal/repl"
	"github.com/liuzhixin405/cove-agent/internal/termui"
)

// fakeToolCall is a tool call the fake model asks for.
type fakeToolCall struct {
	Name string
	Args string // JSON
}

// fakeStep is one scripted answer of the fake model, consumed in order.
type fakeStep struct {
	// Status != 0 answers with this HTTP status and Body.
	Status int
	Body   string
	// Delay holds the answer this long first (cancelled with the request).
	Delay time.Duration
	// Content and ToolCalls form a normal reply.
	Content   string
	ToolCalls []fakeToolCall
	// StopReason overrides the finish reason ("length" for a truncation).
	StopReason string
}

// capturedMessage is one message of a request the fake model received.
type capturedMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
}

// Text is the message's text content (a string or the joined text parts).
func (m capturedMessage) Text() string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) == nil {
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(p.Text)
		}
		return sb.String()
	}
	return string(m.Content)
}

type capturedRequest struct {
	Model    string            `json:"model"`
	Stream   bool              `json:"stream"`
	Messages []capturedMessage `json:"messages"`
}

// fakeModel is an OpenAI-compatible chat server answering from a script.
type fakeModel struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	steps []fakeStep
	next  int
	reqs  []capturedRequest
}

func newFakeModel(t *testing.T, steps ...fakeStep) *fakeModel {
	t.Helper()
	f := &fakeModel{t: t, steps: steps}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// BaseURL is what config.json's provider.base_url takes (the provider adds
// /chat/completions).
func (f *fakeModel) BaseURL() string { return f.srv.URL + "/v1" }

// Requests returns every request received so far.
func (f *fakeModel) Requests() []capturedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedRequest, len(f.reqs))
	copy(out, f.reqs)
	return out
}

// Append adds steps to the script (for a second session against the same
// server).
func (f *fakeModel) Append(steps ...fakeStep) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, steps...)
}

func (f *fakeModel) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req capturedRequest
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	step := fakeStep{Content: "done"}
	if f.next < len(f.steps) {
		step = f.steps[f.next]
	}
	f.next++
	f.mu.Unlock()

	if step.Delay > 0 {
		select {
		case <-time.After(step.Delay):
		case <-r.Context().Done():
			return
		}
	}
	if step.Status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(step.Status)
		_, _ = io.WriteString(w, step.Body)
		return
	}
	finish := "stop"
	if len(step.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	if step.StopReason != "" {
		finish = step.StopReason
	}
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		}
		if step.Content != "" {
			write(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": step.Content}}}})
		}
		for i, tc := range step.ToolCalls {
			write(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": i, "id": fmt.Sprintf("call_%d", i+1), "type": "function",
					"function": map[string]any{"name": tc.Name, "arguments": tc.Args}},
			}}}}})
		}
		write(map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 10},
		})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	msg := map[string]any{"role": "assistant", "content": step.Content}
	if len(step.ToolCalls) > 0 {
		var calls []any
		for i, tc := range step.ToolCalls {
			calls = append(calls, map[string]any{"id": fmt.Sprintf("call_%d", i+1), "type": "function",
				"function": map[string]any{"name": tc.Name, "arguments": tc.Args}})
		}
		msg["tool_calls"] = calls
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model":   req.Model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 10},
	})
}

// overflowBody is llama.cpp's answer to a request over its window.
const overflowBody = `{"error":{"code":400,"message":"request (17964 tokens) exceeds the available context size (16384 tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":17964,"n_ctx":16384}}`

// e2eHome prepares an isolated home and config directory pointing cove at
// the fake model, in plain (non-raw) readline mode, and returns the project
// directory the session runs in.
func e2eHome(t *testing.T, model *fakeModel) (home, project string) {
	t.Helper()
	home = t.TempDir()
	cfgDir := filepath.Join(home, ".cove")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"model":           "qwen-test",
		"permission_mode": "default",
		// The done-check would spend a scripted answer on its own question.
		"done_check": "off",
		"provider":   map[string]any{"name": "openai-compatible", "base_url": model.BaseURL(), "api_key": "test-key"},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("COVE_CONFIG_DIR", cfgDir)
	t.Setenv("COVE_PLAIN_REPL", "1")
	t.Setenv("COVE_PIN_INPUT", "0")
	project = filepath.Join(home, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	return home, project
}

// e2eSession is one running REPL: typed lines go in, everything printed
// comes out.
type e2eSession struct {
	t       *testing.T
	app     *appBootstrap
	stdinW  *os.File
	outMu   sync.Mutex
	out     strings.Builder
	done    chan struct{}
	restore func()
}

// startREPL boots the app like main does and runs the REPL on a goroutine.
func startREPL(t *testing.T) *e2eSession {
	t.Helper()
	resetE2EGlobals()
	app, err := bootstrapApp(false, "", "", "", true)
	if err != nil {
		t.Fatalf("bootstrapApp: %v", err)
	}
	// Background extraction and review call the model too and would consume
	// the script out of order; scenarios count the calls they cause.
	app.eng.SetAutoExtract(false)
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	s := &e2eSession{t: t, app: app, stdinW: inW, done: make(chan struct{})}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := outR.Read(buf)
			if n > 0 {
				s.outMu.Lock()
				s.out.Write(buf[:n])
				s.outMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	s.restore = func() {
		os.Stdin, os.Stdout = oldIn, oldOut
		_ = inR.Close()
		_ = outW.Close()
		_ = outR.Close()
		resetE2EGlobals()
	}
	go func() {
		defer close(s.done)
		runREPL(app, registerAllCommands(), "")
	}()
	t.Cleanup(func() { s.Exit() })
	return s
}

// resetE2EGlobals clears the package-level state a REPL run leaves behind.
func resetE2EGlobals() {
	diagnostic.ResetForTest()
	api.ClearModelContextWindows()
	repl.ClearPermInputCh()
	termui.SetWriter(nil)
}

// Type sends one line as the person would.
func (s *e2eSession) Type(line string) {
	s.t.Helper()
	if _, err := io.WriteString(s.stdinW, line+"\n"); err != nil {
		s.t.Fatalf("type %q: %v", line, err)
	}
}

// Output is everything printed so far.
func (s *e2eSession) Output() string {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	return s.out.String()
}

// WaitFor blocks until want appears in the output (past skip occurrences)
// or fails the test after timeout.
func (s *e2eSession) WaitFor(want string, timeout time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(s.Output(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.t.Fatalf("timed out waiting for %q; output so far:\n%s\n\n--- goroutines ---\n%s", want, s.Output(), goroutineDump())
}

// goroutineDump is every goroutine's stack, for a scenario that hangs: the
// stacks say which lock or channel the loop is stuck on.
func goroutineDump() string {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return string(buf[:n])
}

// WaitForCount blocks until want appears at least n times.
func (s *e2eSession) WaitForCount(want string, n int, timeout time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Count(s.Output(), want) >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.t.Fatalf("timed out waiting for %d× %q; output so far:\n%s", n, want, s.Output())
}

// Exit ends the REPL (typing exit, then closing stdin) and restores the
// process's stdin/stdout. Safe to call twice.
func (s *e2eSession) Exit() {
	if s.restore == nil {
		return
	}
	select {
	case <-s.done:
	default:
		_, _ = io.WriteString(s.stdinW, "exit\n")
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			_ = s.stdinW.Close()
			select {
			case <-s.done:
			case <-time.After(5 * time.Second):
				s.t.Log("REPL did not exit; leaving it")
			}
		}
	}
	_ = s.stdinW.Close()
	s.restore()
	s.restore = nil
}

// e2eTimeout is how long a scenario waits for the fake model's answer to
// show up; generous for CI, far below any real stall.
const e2eTimeout = 20 * time.Second
