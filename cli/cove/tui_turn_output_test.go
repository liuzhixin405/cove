package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/termui"
)

// syncBuffer is a bytes.Buffer the spinner goroutine and the test can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureTurnOutput(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	termui.SetWriter(buf)
	t.Cleanup(func() { termui.SetWriter(nil) })
	return buf
}

// lineOf returns the terminal row that ends just before s: the text between
// the last \r or \n preceding s and s itself. It is what the user sees to the
// left of s.
func lineBefore(out, s string) (string, bool) {
	i := strings.Index(out, s)
	if i < 0 {
		return "", false
	}
	head := out[:i]
	if j := strings.LastIndexAny(head, "\r\n"); j >= 0 {
		head = head[j+1:]
	}
	return head, true
}

type hostileRunner struct{ reply string }

func (h hostileRunner) RunWithStream(ctx context.Context, input string, onDelta func(string)) (string, error) {
	return h.RunMessageWithStream(ctx, api.Message{Content: input}, onDelta, nil)
}

func (h hostileRunner) RunMessageWithStream(ctx context.Context, msg api.Message, onDelta func(string), onReasoning func(string)) (string, error) {
	if onReasoning != nil {
		onReasoning("thinking \x1b]0;pwned-by-reasoning\x07")
	}
	// Split mid-sequence, the way a stream cuts chunks.
	onDelta("see \x1b]52;c;cm0gLXJm")
	onDelta("IH4=\x07 and \x1b[2J\x1b[H done")
	return h.reply, nil
}

// The model's reply and reasoning were written to the terminal verbatim, so a
// prompt-injected answer could retitle the window, write the clipboard or wipe
// the screen.
func TestTurnOutputDropsEscapeSequencesFromTheModel(t *testing.T) {
	buf := captureTurnOutput(t)
	old := showReasoning
	showReasoning = true
	t.Cleanup(func() { showReasoning = old })

	if _, err := runChatInteractionMessage(context.Background(), hostileRunner{}, api.Message{Role: "user", Content: "q"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, bad := range []string{"\x1b]", "\x1b[2J", "\x1b[H", "\a"} {
		if strings.Contains(out, bad) {
			t.Errorf("model output reached the terminal with %q: %q", bad, out)
		}
	}
	if !strings.Contains(out, "done") || !strings.Contains(out, "see ") {
		t.Errorf("the reply's text went missing: %q", out)
	}
}

// With show_reasoning on, the first answer delta was printed straight after
// the last reasoning chunk, on the same row, in the middle of a sentence.
func TestTurnOutputStartsTheAnswerOnItsOwnLineAfterReasoning(t *testing.T) {
	buf := captureTurnOutput(t)
	old := showReasoning
	showReasoning = true
	t.Cleanup(func() { showReasoning = old })

	if _, err := runChatInteractionMessage(context.Background(), reasoningRunner{}, api.Message{Role: "user", Content: "q"}); err != nil {
		t.Fatal(err)
	}
	row, ok := lineBefore(buf.String(), "the answer")
	if !ok {
		t.Fatalf("answer missing: %q", buf.String())
	}
	if strings.Contains(row, "RAW-REASONING") || strings.Contains(row, "detail") {
		t.Fatalf("answer shares a row with the reasoning: %q", row)
	}
}

// A tool block arriving while the "思考中" spinner was animating was printed
// after the spinner frame on the same row ("⠋ 思考中...  ▸ bash ls"), and
// the next frame then landed on the following row.
func TestEngineLineDoesNotShareARowWithTheSpinner(t *testing.T) {
	buf := captureTurnOutput(t)
	p := newTurnPrinter()
	p.beginAttempt()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "思考中") {
		if time.Now().After(deadline) {
			t.Fatal("spinner never painted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	p.engineLine("  ▸ bash ls\n    ⎿ ✓ ok\n")
	p.stop()

	row, ok := lineBefore(buf.String(), "▸ bash ls")
	if !ok {
		t.Fatalf("tool line missing: %q", buf.String())
	}
	if strings.Contains(row, "思考中") {
		t.Fatalf("tool line printed on the spinner's row: %q", row)
	}
}

// The model usually says "我来看看文件：" and then calls a tool. That text has
// no trailing newline, so the tool block was glued to it:
// "我来看看文件：  ▸ read main.go".
func TestEngineLineStartsOnAFreshRowAfterPartialText(t *testing.T) {
	buf := captureTurnOutput(t)
	p := newTurnPrinter()
	p.delta("我来看看文件：")
	p.engineLine("  ▸ read main.go\n")
	p.engineLine("  ! blocked: no newline here")
	p.delta("next")
	p.stop()

	out := buf.String()
	if row, _ := lineBefore(out, "▸ read"); strings.Contains(row, "文件") {
		t.Fatalf("tool block glued to the model's text: %q", row)
	}
	if row, _ := lineBefore(out, "next"); strings.Contains(row, "blocked") {
		t.Fatalf("an engine line without a newline swallowed the next output: %q", row)
	}
}

// Live output of a shell command (a file being cat'ed, a curl'ed page) went to
// the terminal verbatim; its colour is kept, its control sequences are not.
func TestToolProgressKeepsColourButDropsControlSequences(t *testing.T) {
	buf := captureTurnOutput(t)
	p := newTurnPrinter()
	p.toolProgress("bash", "\x1b[32mPASS\x1b[0m\n\x1b]0;evil\x07\x1b[5A")
	p.stop()

	out := buf.String()
	if !strings.Contains(out, "\x1b[32mPASS\x1b[0m") {
		t.Errorf("tool colour lost: %q", out)
	}
	if strings.Contains(out, "\x1b]") || strings.Contains(out, "\x1b[5A") {
		t.Errorf("tool control sequence reached the terminal: %q", out)
	}
}

// A provider error body is echoed in "Request failed: ..."; it is untrusted.
func TestRequestFailureMessageIsSanitised(t *testing.T) {
	buf := captureTurnOutput(t)
	_, err := runChatInteractionMessage(context.Background(), failingRunner{}, api.Message{Role: "user", Content: "q"})
	if err == nil {
		t.Fatal("want the runner's error back")
	}
	out := buf.String()
	if strings.Contains(out, "\x1b]") {
		t.Fatalf("error text reached the terminal raw: %q", out)
	}
	if !strings.Contains(out, "quota") {
		t.Fatalf("error text went missing: %q", out)
	}
}

type failingRunner struct{}

func (failingRunner) RunWithStream(ctx context.Context, input string, onDelta func(string)) (string, error) {
	return "", errString("403 quota exceeded \x1b]0;pwned\x07")
}

type errString string

func (e errString) Error() string { return string(e) }
