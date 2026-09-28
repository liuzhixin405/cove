package termui

import (
	"bytes"
	"testing"
)

type recordingConsole struct{ above, stream []string }

func (c *recordingConsole) PrintAbove(s string)  { c.above = append(c.above, s) }
func (c *recordingConsole) StreamPrint(s string) { c.stream = append(c.stream, s) }
func (c *recordingConsole) Transient(string)     {}
func (c *recordingConsole) BeginOutput()         {}
func (c *recordingConsole) EndOutput()           {}

// With a line editor on screen, command output goes through it (above the
// input line); without one it is written byte for byte, since there it is
// the answer on stdout.
func TestTextGoesThroughTheEditorOrVerbatim(t *testing.T) {
	var buf bytes.Buffer
	SetWriter(&buf)
	t.Cleanup(func() { SetWriter(nil); SetConsole(nil) })

	Text("line one\nline two\n")
	if buf.String() != "line one\nline two\n" {
		t.Fatalf("without an editor the text changed: %q", buf.String())
	}

	c := &recordingConsole{}
	SetConsole(c)
	buf.Reset()
	Text("done\n")
	Text("partial")
	if buf.Len() != 0 {
		t.Fatalf("with an editor the text bypassed it: %q", buf.String())
	}
	if len(c.above) != 1 || c.above[0] != "done\n" || len(c.stream) != 1 || c.stream[0] != "partial" {
		t.Fatalf("editor got above=%q stream=%q", c.above, c.stream)
	}
}
