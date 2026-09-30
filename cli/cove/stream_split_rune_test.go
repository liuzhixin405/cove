package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// A character split across deltas is printed whole, and one cut off by the
// end of the stream is still accounted for: the sanitizer used to print the
// lead byte and drop the rest, and after it learned to hold the start back,
// nothing printed what it held when the stream ended.
func TestTurnPrinterKeepsCharactersSplitAcrossChunks(t *testing.T) {
	buf := captureTurnOutput(t)
	p := newTurnPrinter()
	word := "文件"
	p.delta(word[:1])
	p.delta(word[1:4])
	p.delta(word[4:] + "已保存")
	p.toolOutputStart("bash", "ls")
	p.toolProgress("bash", "目录"[:2])
	p.toolProgress("bash", "目录"[2:]+"\n")
	p.stop()
	out := ansi.Strip(buf.String())
	for _, want := range []string{"文件已保存", "目录"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing from %q", want, out)
		}
	}
}

func TestTurnPrinterFlushesAnUnfinishedCharacterAtTheEnd(t *testing.T) {
	buf := captureTurnOutput(t)
	p := newTurnPrinter()
	p.delta("完成" + "了"[:2])
	p.stop()
	out := ansi.Strip(buf.String())
	if !strings.Contains(out, "完成") || !strings.Contains(out, "�") {
		t.Fatalf("output = %q, want the text and a replacement character", out)
	}
}
