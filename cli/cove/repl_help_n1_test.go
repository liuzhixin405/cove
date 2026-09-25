package main

import (
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/command"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/tool"
)

// /history clean stays a command but is no longer advertised in /help.
func TestHelpDoesNotListHistoryClean(t *testing.T) {
	buf := captureOut(t)
	printHelp(command.NewRegistry(), tool.NewRegistry(), nil)
	out := buf.String()
	if strings.Contains(out, "/history clean") {
		t.Fatalf("/help still lists /history clean:\n%s", out)
	}
	if !strings.Contains(out, "/history") {
		t.Fatalf("/help lost /history:\n%s", out)
	}
}

func TestCompactReportLine(t *testing.T) {
	cases := []struct {
		r    engine.CompactReport
		want []string
	}{
		{engine.CompactReport{Compressed: true, Summarized: true, BeforeTokens: 9000, AfterTokens: 1200}, []string{"压缩前 9000 tokens", "压缩后 1200 tokens"}},
		{engine.CompactReport{Reason: "对话只有 2 条消息，至少 4 条才能压缩", BeforeTokens: 50}, []string{"未压缩", "至少 4 条"}},
		{engine.CompactReport{Compressed: true, Reason: "摘要生成失败，已改为截断旧历史", BeforeTokens: 9000, AfterTokens: 3000}, []string{"部分压缩", "截断"}},
	}
	for _, c := range cases {
		got := compactReportLine(c.r)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%+v: %q lacks %q", c.r, got, w)
			}
		}
	}
}
