package render

import (
	"strings"
	"testing"
)

// A tool block's header is the model's command and its summary is the first
// line of whatever the tool read, so both are untrusted. With Styles{} the
// renderer adds no escapes of its own; any ESC in the output came from the
// block and would have been executed by the terminal.
func TestCollapsedNeverEmitsEscapesFromBlockText(t *testing.T) {
	title := "\x1b]0;pwned\x07"
	blocks := []Block{
		ToolBlock("1", "bash", "cat README.md"+title, "", "\x1b[2J\x1b[Hfirst line"+title+"\nsecond", false, 0),
		ToolBlock("2", "read", "notes.txt", "\x1b[1A\x1b[2Kfake ok", "", false, 0),
		UserBlock("hi" + title),
		AnswerBlock("answer\x1b[6n"),
		SystemBlock("resumed\x1bc", true),
		{Kind: KindThinking, ID: "3", Header: "1.2s" + title, Full: "x"},
	}
	for _, b := range blocks {
		out := Collapsed(b, 80, Styles{})
		if strings.ContainsAny(out, "\x1b\a") {
			t.Errorf("Collapsed(kind %d) passed a control sequence through: %q", b.Kind, out)
		}
	}
}
