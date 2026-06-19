package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// frameLines returns the number of rendered lines in the current View().
func frameLines(m *Model) int {
	v := m.View().Content
	if v == "" {
		return 0
	}
	return strings.Count(v, "\n") + 1
}

// typeText feeds printable runes one key press at a time, mirroring how the
// runtime delivers IME-committed text (one KeyPressMsg per rune with Text set).
func typeText(m *Model, s string) {
	for _, r := range s {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// pressKey sends a single non-text key (Enter/Esc/arrows/etc.), optionally with
// a modifier such as Ctrl.
func pressKey(m *Model, code rune, mod tea.KeyMod) {
	m.Update(tea.KeyPressMsg{Code: code, Mod: mod})
}

// newSmokeModel builds a ready Model sized to w×h with a couple of commands.
func newSmokeModel(t *testing.T, w, h int, onSubmit, onResume func(string)) *Model {
	t.Helper()
	m := New("test-model", onSubmit, onResume, nil, []CommandItem{
		{Name: "help", Desc: "显示帮助"},
		{Name: "clear", Desc: "清空会话"},
		{Name: "model", Desc: "切换模型"},
	})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	if !m.ready {
		t.Fatal("model not ready after WindowSizeMsg")
	}
	return m
}

// TestSmokeFrameHeightStable drives the Model through a realistic session and
// asserts the rendered frame always exactly fills the terminal height. This is
// the non-interactive stand-in for hands-on gray testing: it exercises submit,
// streaming, the command palette, the history overlay, the permission overlay,
// transient activity and resizes without a live terminal.
func TestSmokeFrameHeightStable(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {100, 40}, {60, 20}, {40, 12}} {
		w, h := sz[0], sz[1]
		var submitted []string
		m := newSmokeModel(t, w, h, func(s string) { submitted = append(submitted, s) }, nil)

		check := func(stage string) {
			if got := frameLines(m); got != h {
				t.Fatalf("%dx%d %s: frame=%d want %d", w, h, stage, got, h)
			}
		}
		check("initial")

		// Type and submit a message.
		typeText(m, "你好世界")
		check("typed")
		pressKey(m, tea.KeyEnter, 0)
		check("submitted")
		if len(submitted) != 1 || submitted[0] != "你好世界" {
			t.Fatalf("submit got %v", submitted)
		}

		// Stream an assistant reply.
		m.Update(streamBeginMsg{echo: ""})
		for _, d := range []string{"这是", "一段", "很长很长很长很长很长很长很长的回复\n", "第二行\n"} {
			m.Update(streamDeltaMsg(d))
		}
		m.Update(streamReasoningMsg("思考过程……"))
		m.Update(engineLineMsg("\n[工具] 执行 read\n"))
		m.Update(streamEndMsg{})
		check("streamed")

		// Transient activity on/off must not change frame height.
		m.Update(activityMsg("执行 bash"))
		check("activity-on")
		m.Update(taskStateMsg(TaskInfo{Running: true, Current: "x", Queued: []string{"a", "b"}}))
		check("task-running")
		m.Update(activityMsg(""))
		m.Update(taskStateMsg(TaskInfo{Running: false}))
		check("activity-off")

		// Command palette: open, filter, frame stable, then select.
		typeText(m, "/")
		if m.overlay != overlayCommand {
			t.Fatalf("%dx%d: palette did not open", w, h)
		}
		check("palette-open")
		typeText(m, "mo")
		check("palette-filter")
		typeText(m, "zzz") // no matches
		check("palette-nomatch")
		pressKey(m, tea.KeyEsc, 0)
		if m.overlay != overlayNone {
			t.Fatalf("%dx%d: palette did not close", w, h)
		}
		check("palette-closed")

		// History overlay.
		m.Update(historyMsg([]HistoryItem{
			{ID: "1", Title: "会话甲", Subtitle: "今天"},
			{ID: "2", Title: "会话乙", Subtitle: "昨天"},
		}))
		pressKey(m, 'r', tea.ModCtrl)
		if m.overlay != overlayHistory {
			t.Fatalf("%dx%d: history did not open", w, h)
		}
		check("history-open")
		pressKey(m, tea.KeyDown, 0)
		check("history-move")
		pressKey(m, tea.KeyEsc, 0)
		check("history-closed")

		// Permission overlay.
		ch := make(chan PermDecision, 1)
		m.Update(permRequestMsg(permRequest{tool: "bash", desc: "rm -rf /tmp/x", reply: ch}))
		if m.overlay != overlayPermission {
			t.Fatalf("%dx%d: perm overlay did not open", w, h)
		}
		check("perm-open")
		typeText(m, "n")
		if got := <-ch; got != PermDeny {
			t.Fatalf("%dx%d: perm decision %d", w, h, got)
		}
		check("perm-closed")
	}
}

// TestSmokeResizeKeepsFrame ensures the frame tracks the terminal across a
// sequence of resizes, including while an overlay is open.
func TestSmokeResizeKeepsFrame(t *testing.T) {
	m := newSmokeModel(t, 80, 24, nil, nil)
	typeText(m, "/") // open palette

	for _, sz := range [][2]int{{120, 50}, {30, 10}, {200, 60}, {50, 16}} {
		w, h := sz[0], sz[1]
		m.Update(tea.WindowSizeMsg{Width: w, Height: h})
		if got := frameLines(m); got != h {
			t.Fatalf("resize %dx%d: frame=%d want %d", w, h, got, h)
		}
	}
}

// TestThinkingFoldToggle verifies the per-turn thinking fold: thinking shows
// live while the answer is pending, folds to a clickable header once the answer
// arrives, and a mouse click on that header toggles it back open.
func TestThinkingFoldToggle(t *testing.T) {
	m := newSmokeModel(t, 80, 24, nil, nil)

	// Streaming reasoning with no answer yet -> thinking is shown live.
	m.Update(streamBeginMsg{echo: ""})
	m.Update(streamReasoningMsg("思考内容XYZ"))
	if c := m.View().Content; !strings.Contains(c, "思考内容XYZ") {
		t.Fatal("live thinking should be visible before the answer arrives")
	}

	// Answer arrives and stream ends -> thinking folds, reasoning hidden.
	m.Update(streamDeltaMsg("回答内容ABC"))
	m.Update(streamEndMsg{})
	c := m.View().Content
	if strings.Contains(c, "思考内容XYZ") {
		t.Fatal("thinking should be folded once the answer is present")
	}
	if !strings.Contains(c, "回答内容ABC") {
		t.Fatal("answer should remain visible")
	}
	if !strings.Contains(c, "▸") {
		t.Fatal("folded thinking header (▸) should be shown")
	}

	// Click the thinking header -> it expands and the reasoning reappears.
	if len(m.clickMap) != 1 {
		t.Fatalf("expected exactly 1 clickable header, got %d", len(m.clickMap))
	}
	var row int
	for r := range m.clickMap {
		row = r
	}
	m.Update(tea.MouseClickMsg{Y: row + 1, Button: tea.MouseLeft}) // +1: status bar row 0
	c = m.View().Content
	if !strings.Contains(c, "思考内容XYZ") {
		t.Fatal("thinking should be visible after clicking the header")
	}
	if !strings.Contains(c, "▾") {
		t.Fatal("expanded thinking header (▾) should be shown")
	}
}
