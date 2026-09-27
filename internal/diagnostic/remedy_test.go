package diagnostic

import (
	"fmt"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

type stubRuntime struct {
	windows map[string]int
	notes   []string
}

func newStubRuntime() *stubRuntime { return &stubRuntime{windows: map[string]int{}} }

func (s *stubRuntime) SetModelContextWindow(model string, tokens int) { s.windows[model] = tokens }
func (s *stubRuntime) Notify(line string)                             { s.notes = append(s.notes, line) }

// The real window is the one the server names, not the request's size and
// not the first number in the text.
func TestParseContextWindow(t *testing.T) {
	cases := map[string]int{
		llamaOverflow: 16384,
		`request (17964 tokens) exceeds the available context size (16384 tokens), try increasing it`: 16384,
		`This model's maximum context length is 65536 tokens. However, you requested 70000 tokens`:    65536,
		`prompt is too long: 210000 tokens > 200000 maximum`:                                          200000,
		`"n_ctx": 8192`:                   8192,
		`max_tokens must be at most 8192`: 0,
		``:                                0,
	}
	for msg, want := range cases {
		if got := parseContextWindow(msg); got != want {
			t.Errorf("parseContextWindow(%q) = %d, want %d", msg, got, want)
		}
	}
}

// Reporting an overflow with a Runtime installed shrinks the model's window
// to what the server said, tells the user, and records what it did.
func TestContextWindowRemedyLearnsTheServersWindow(t *testing.T) {
	resetRuntimeEvents(t)
	ResetRemedyState()
	t.Cleanup(api.ClearModelContextWindows)
	rt := newStubRuntime()
	SetRuntime(rt)

	err := fmt.Errorf("api: %w", &api.StatusError{Status: 400, Msg: llamaOverflow})
	ReportError(err, Context{Provider: "openai-compatible", Model: "qwen3.6-27b"})

	if rt.windows["qwen3.6-27b"] != 16384 {
		t.Fatalf("window not set: %v", rt.windows)
	}
	if len(rt.notes) != 1 || !strings.Contains(rt.notes[0], "16384") || !strings.Contains(rt.notes[0], "context_window") {
		t.Errorf("user not told, or not told how to persist: %v", rt.notes)
	}
	events := RecentRuntime()
	if len(events) != 2 || events[1].Severity != SevRecovered || events[1].Code != ErrAPIContextLength {
		t.Fatalf("recovered event missing: %+v", events)
	}
	if !strings.Contains(events[1].Message, "32000") || !strings.Contains(events[1].Message, "16384") {
		t.Errorf("applied text = %q", events[1].Message)
	}
}

// Nothing happens when there is no Runtime, when the text names no window,
// when the named window is not smaller than the current estimate, or when
// the model is unknown.
func TestContextWindowRemedyDeclines(t *testing.T) {
	resetRuntimeEvents(t)
	ResetRemedyState()
	t.Cleanup(api.ClearModelContextWindows)

	overflow := func(msg string) error {
		return fmt.Errorf("api: %w", &api.StatusError{Status: 400, Msg: msg})
	}
	// No runtime.
	ReportError(overflow(llamaOverflow), Context{Model: "qwen3.6-27b"})
	if n := len(RecentRuntime()); n != 1 {
		t.Fatalf("without a Runtime %d events were recorded, want 1", n)
	}
	rt := newStubRuntime()
	SetRuntime(rt)
	// No window in the text.
	ReportError(overflow("input is too long"), Context{Model: "qwen3.6-27b"})
	// Window not smaller than the estimate (32000 for an unknown name).
	ReportError(overflow(`request exceeds the available context size (65536 tokens)`), Context{Model: "qwen3.6-27b"})
	// No model.
	ReportError(overflow(llamaOverflow), Context{})
	if len(rt.windows) != 0 || len(rt.notes) != 0 {
		t.Errorf("remedy applied when it should have declined: %v %v", rt.windows, rt.notes)
	}
	for _, ev := range RecentRuntime() {
		if ev.Severity == SevRecovered {
			t.Errorf("recovered event recorded without a remedy: %+v", ev)
		}
	}
}

// Invalid tool arguments are counted per model; the user hears about it at
// the third occurrence and then every fifth.
func TestToolArgsRemedyCountsPerModel(t *testing.T) {
	resetRuntimeEvents(t)
	ResetRemedyState()
	rt := newStubRuntime()
	SetRuntime(rt)
	for i := 0; i < 8; i++ {
		ReportError(&api.ToolArgsInvalidError{Tool: "bash"}, Context{Model: "qwen3.6-27b", Tool: "bash"})
	}
	ReportError(&api.ToolArgsInvalidError{Tool: "edit"}, Context{Model: "other", Tool: "edit"})
	if len(rt.notes) != 2 {
		t.Fatalf("notes = %v, want one at 3 and one at 8", rt.notes)
	}
	if !strings.Contains(rt.notes[0], "qwen3.6-27b") || !strings.Contains(rt.notes[0], "3 次") {
		t.Errorf("first note = %q", rt.notes[0])
	}
	recovered := 0
	for _, ev := range RecentRuntime() {
		if ev.Severity == SevRecovered {
			recovered++
		}
	}
	if recovered != 2 {
		t.Errorf("%d recovered events, want 2", recovered)
	}
}

// A remedy that panics is a remedy that did not apply.
func TestPanickingRemedyIsContained(t *testing.T) {
	resetRuntimeEvents(t)
	def := registry[ErrAPIContextLength]
	saved := def.Remedy
	def.Remedy = func(RuntimeEvent, Runtime) (string, bool) { panic("boom") }
	t.Cleanup(func() { def.Remedy = saved })
	SetRuntime(newStubRuntime())
	ev := ReportError(fmt.Errorf("api: %w", &api.StatusError{Status: 400, Msg: llamaOverflow}), Context{Model: "m"})
	if ev.Code != ErrAPIContextLength || len(RecentRuntime()) != 1 {
		t.Fatalf("panic leaked or event lost: %+v", RecentRuntime())
	}
}
