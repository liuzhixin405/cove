package main

import (
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/repl"
	"github.com/liuzhixin405/cove-agent/internal/termui"
)

// waitForOutput waits until buf holds want.
func waitForOutput(t *testing.T, buf *lockedBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("%q never shown; output: %q", want, buf.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// The question tool's prompt is written by the model (header, question,
// option labels and descriptions) and was printed raw: a question carrying
// "ESC[2A ESC[2K  1. delete nothing ESC[2B" repainted the option list above
// before the user pressed a digit. Controls are shown as inert text, the way
// the permission box shows them.
func TestQuestionPromptShowsControlsAsText(t *testing.T) {
	var buf lockedBuffer
	termui.SetWriter(&buf)
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh(); termui.SetWriter(nil) })

	prompt := "Proceed?\x1b[2A\x1b[2K  1. delete nothing\x1b[2B\n  1. delete everything\x1b]0;t\x07\n  2. keep"
	done := make(chan string, 1)
	go func() { done <- askUserQuestion(prompt) }()
	waitForOutput(t, &buf, "Proceed?")
	waitForPermInputCh(t) <- "1"
	select {
	case got := <-done:
		if got != "1" {
			t.Fatalf("answer = %q, want \"1\"", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("askUserQuestion did not return")
	}
	out := buf.String()
	for _, raw := range []string{"\x1b[2A", "\x1b[2K", "\x1b[2B", "\x1b]0;t"} {
		if strings.Contains(out, raw) {
			t.Errorf("model-supplied %q reached the terminal raw: %q", raw, out)
		}
	}
	if !strings.Contains(out, `\e[2A`) || !strings.Contains(out, `\e]0;t^G`) {
		t.Errorf("controls not shown as text: %q", out)
	}
}

// The 记住范围 line names the executable or the model-supplied MCP server and
// tool, and was printed raw, so an MCP call could redraw the prompt it was
// being approved in.
func TestPermissionScopeLineShowsControlsAsText(t *testing.T) {
	var buf lockedBuffer
	termui.SetWriter(&buf)
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh(); termui.SetWriter(nil) })

	done := make(chan bool, 1)
	go func() {
		done <- askToolPermission(nil, "mcp", map[string]any{"serverName": "gh\x1b[1A\x1b[2K", "toolName": "read\x1b[31m"}, "")
	}()
	waitForOutput(t, &buf, "记住范围")
	waitForPermInputCh(t) <- "n"
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("askToolPermission did not return")
	}
	out := buf.String()
	i := strings.Index(out, "记住范围")
	line := out[i:]
	if j := strings.Index(line, "\n"); j >= 0 {
		line = line[:j]
	}
	if strings.Contains(line, "\x1b[1A") || strings.Contains(line, "\x1b[2K") || strings.Contains(line, "\x1b[31m") {
		t.Errorf("scope line carries raw controls: %q", line)
	}
	if !strings.Contains(line, `gh\e[1A\e[2K/read\e[31m`) {
		t.Errorf("scope line does not show the names as text: %q", line)
	}
}
