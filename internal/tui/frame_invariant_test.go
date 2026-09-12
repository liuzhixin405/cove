package tui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// The smoke test's frame check only asserts the frame is not too SHORT, which
// is why chrome overflow went unnoticed. A layout that "falls apart mid-run" is
// the opposite failure: one chrome row grows from one line to two and the frame
// becomes taller than the terminal, so the renderer scrolls and the whole view
// shifts under the user.
//
// This asserts the missing half — the frame must never exceed the terminal, and
// no rendered line may exceed the terminal width.
func TestFrameNeverExceedsTerminal(t *testing.T) {
	sizes := [][2]int{{80, 24}, {60, 20}, {40, 12}, {30, 10}, {24, 8}}

	variants := []struct {
		name   string
		status StatusInfo
	}{
		{"bare", StatusInfo{}},
		{"long provider and perm mode", StatusInfo{
			Model:    "deepseek-flash",
			Provider: "openrouter/anthropic",
			PermMode: "accept-edits",
			Git:      "feature/very-long-branch-name*",
		}},
		{"expanded git panel", StatusInfo{
			GitStatus: " M internal/engine/loopdetect.go\n" +
				" M internal/tui/styles.go\n" +
				"?? internal/engine/filetouch.go\n" +
				"?? internal/tui/styles_test.go\n",
		}},
		{"long file names in git panel", StatusInfo{
			GitStatus: " M internal/some/deeply/nested/package/with/a/really/long/name/file.go\n",
		}},
		{"budget and elapsed", StatusInfo{
			TokensIn: 1234567, TokensOut: 765432, Cost: 12.34, Budget: 50, Elapsed: "12m34s",
		}},
		{"styled activity line", StatusInfo{Model: "m", Provider: "p"}},
	}

	for _, sz := range sizes {
		w, h := sz[0], sz[1]
		for _, v := range variants {
			t.Run(fmt.Sprintf("%dx%d/%s", w, h, v.name), func(t *testing.T) {
				m := newSmokeModel(t, w, h, nil, nil)
				if v.name == "expanded git panel" || v.name == "long file names in git panel" {
					m.gitExpanded = true
				}
				m.Update(statusUpdateMsg(v.status))

				// A live run also has streaming text and a transient activity
				// line, which is when the corruption is reported.
				m.Update(streamBeginMsg{echo: "帮我重构这个模块"})
				m.Update(streamDeltaMsg("正在分析代码，这里有一段比较长的中文说明文字，用来触发换行与宽度计算。\n第二行内容\n"))
				m.Update(activityMsg("执行 bash: go test ./..."))
				m.Update(taskStateMsg(TaskInfo{Running: true, Current: "go test", Queued: []string{"a"}}))

				content := m.View().Content
				if got := strings.Count(content, "\n") + 1; got > h {
					t.Fatalf("frame is %d lines but terminal is %d — overflow shifts the whole view", got, h)
				}
				for i, line := range strings.Split(content, "\n") {
					if gw := lipgloss.Width(line); gw > w {
						t.Fatalf("line %d is %d columns wide but terminal is %d: %q", i, gw, w, line)
					}
				}
			})
		}
	}
}
