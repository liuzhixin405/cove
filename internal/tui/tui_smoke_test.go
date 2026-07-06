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
			got := frameLines(m)
			if got < h {
				t.Fatalf("%dx%d %s: frame=%d shorter than terminal height %d", w, h, stage, got, h)
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
		pressKey(m, 'k', tea.ModCtrl)
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
		pressKey(m, 's', tea.ModCtrl)
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
	pressKey(m, 'k', tea.ModCtrl) // open palette

	for _, sz := range [][2]int{{120, 50}, {30, 10}, {200, 60}, {50, 16}} {
		w, h := sz[0], sz[1]
		m.Update(tea.WindowSizeMsg{Width: w, Height: h})
		if got := frameLines(m); got < h {
			t.Fatalf("resize %dx%d: frame=%d shorter than terminal height %d", w, h, got, h)
		}
	}
}

// TestNarrowOverlayStable ensures overlays remain frame-stable on narrow
// terminals while filtering results and switching modal types.
func TestNarrowOverlayStable(t *testing.T) {
	const (
		w = 32
		h = 12
	)

	m := New("test-model", nil, nil, nil, []CommandItem{
		{Name: "model", Desc: "select model and provider"},
		{Name: "memory", Desc: "manage persistent memory"},
		{Name: "permissions", Desc: "permission settings"},
	})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	if !m.ready {
		t.Fatal("model not ready after WindowSizeMsg")
	}

	checkStable := func(stage string, want int) {
		got := frameLines(m)
		if got != want {
			t.Fatalf("%s: frame lines changed, got %d want %d", stage, got, want)
		}
	}

	// Command overlay: baseline -> filtered -> no matches should keep same frame.
	pressKey(m, 'k', tea.ModCtrl)
	if m.overlay != overlayCommand {
		t.Fatal("expected command overlay to open")
	}
	baseline := frameLines(m)
	checkStable("command-open", baseline)

	typeText(m, "mo")
	checkStable("command-filter", baseline)

	typeText(m, "zz")
	checkStable("command-no-match", baseline)

	pressKey(m, tea.KeyEsc, 0)
	checkStable("command-closed", h)

	// History overlay should also be stable under filtering.
	m.Update(historyMsg([]HistoryItem{
		{ID: "1", Title: "session alpha", Subtitle: "today"},
		{ID: "2", Title: "session beta with long title", Subtitle: "yesterday"},
	}))
	pressKey(m, 's', tea.ModCtrl)
	if m.overlay != overlayHistory {
		t.Fatal("expected history overlay to open")
	}
	hBase := frameLines(m)
	checkStable("history-open", hBase)

	typeText(m, "beta")
	checkStable("history-filter", hBase)

	pressKey(m, tea.KeyEsc, 0)
	checkStable("history-closed", h)

	// Help overlay on narrow terminals should be stable too.
	pressKey(m, '?', 0)
	if m.overlay != overlayHelp {
		t.Fatal("expected help overlay to open")
	}
	checkStable("help-open", frameLines(m))
	pressKey(m, tea.KeyEsc, 0)
	checkStable("help-closed", h)
}

// TestThinkingFoldToggle verifies the per-turn thinking fold: thinking shows
// live while the answer is pending, folds to a folded header once the answer
// arrives, and Alt+T toggles it back open.
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

	// Alt+T toggles the thinking fold open.
	pressKey(m, 't', tea.ModAlt)
	c = m.View().Content
	if !strings.Contains(c, "思考内容XYZ") {
		t.Fatal("thinking should be visible after pressing Alt+T")
	}
	if !strings.Contains(c, "▾") {
		t.Fatal("expanded thinking header (▾) should be shown")
	}
}

// TestEnterTrailingBackslashContinues verifies OpenCode-style multiline input:
// when Enter is pressed with a trailing "\\", the message is not submitted
// and a newline is inserted instead.
func TestEnterTrailingBackslashContinues(t *testing.T) {
	m := newSmokeModel(t, 80, 24, nil, nil)
	var submitted []string
	m.onSubmit = func(s string) { submitted = append(submitted, s) }

	typeText(m, "line1\\")
	pressKey(m, tea.KeyEnter, 0)

	if len(submitted) != 0 {
		t.Fatalf("expected no submit, got %v", submitted)
	}
	if got := m.ta.Value(); got != "line1\n" {
		t.Fatalf("expected continued input with newline, got %q", got)
	}
}

// TestMainViewUpDownScroll verifies that up/down keys scroll the transcript in
// the main view. Many terminals map wheel to up/down while in alt-screen mode,
// so this keeps wheel scrolling functional without mouse capture.
func TestMainViewUpDownScroll(t *testing.T) {
	m := newSmokeModel(t, 40, 10, nil, nil)

	var long strings.Builder
	for i := 0; i < 80; i++ {
		long.WriteString("line\n")
	}
	m.appendSystem(long.String())

	if got := m.vp.YOffset(); got <= 0 {
		t.Fatalf("expected initial view at bottom, got yOffset=%d", got)
	}

	start := m.vp.YOffset()
	pressKey(m, tea.KeyUp, 0)
	if got := m.vp.YOffset(); got >= start {
		t.Fatalf("expected up to scroll up, start=%d got=%d", start, got)
	}

	afterUp := m.vp.YOffset()
	pressKey(m, tea.KeyDown, 0)
	if got := m.vp.YOffset(); got <= afterUp {
		t.Fatalf("expected down to scroll down, before=%d got=%d", afterUp, got)
	}
}

// TestCtrlCAndEscBehavior ensures opencode-style contract:
// Esc cancels while running; Ctrl+C always opens quit confirmation.
func TestCtrlCAndEscBehavior(t *testing.T) {
	interrupted := false
	m := newSmokeModel(t, 80, 24, nil, nil)
	m.onInterrupt = func() { interrupted = true }

	// Running: Esc should interrupt, not open quit dialog.
	m.Update(taskStateMsg(TaskInfo{Running: true}))
	pressKey(m, tea.KeyEsc, 0)
	if !interrupted {
		t.Fatal("expected interrupt callback while running on Esc")
	}
	if m.showQuit {
		t.Fatal("quit dialog should not open while running")
	}

	// Idle + empty input: Ctrl+C should open quit confirmation.
	m.Update(taskStateMsg(TaskInfo{Running: false}))
	interrupted = false
	m.ta.Reset()
	pressKey(m, 'c', tea.ModCtrl)
	if interrupted {
		t.Fatal("interrupt callback should not fire while idle on Ctrl+C")
	}
	if !m.showQuit {
		t.Fatal("quit dialog should open when idle and input is empty")
	}
}

// TestEndStreamAlignIgnoresEngineLines verifies end-of-stream alignment uses
// only text deltas, so interleaved engine/tool lines do not break final suffix
// completion.
func TestEndStreamAlignIgnoresEngineLines(t *testing.T) {
	m := newSmokeModel(t, 80, 24, nil, nil)
	m.Update(streamBeginMsg{})
	m.Update(streamDeltaMsg("abc"))
	m.Update(engineLineMsg("\n[工具] read\n"))
	m.Update(streamEndMsg{final: "abcXYZ"})

	c := m.View().Content
	if !strings.Contains(c, "XYZ") {
		t.Fatalf("expected aligned final suffix in transcript, got: %q", c)
	}
}

func TestHistorySelectByIndex(t *testing.T) {
	var resumed string
	m := newSmokeModel(t, 80, 24, nil, func(id string) { resumed = id })
	m.Update(historyMsg([]HistoryItem{
		{ID: "sess-1", Title: "第一个会话", Subtitle: "今天"},
		{ID: "sess-2", Title: "第二个会话", Subtitle: "昨天"},
	}))

	// Open history overlay and select item #2 via numeric direct selection.
	pressKey(m, 's', tea.ModCtrl)
	typeText(m, "2")
	pressKey(m, tea.KeyEnter, 0)

	if resumed != "sess-2" {
		t.Fatalf("expected resumed session sess-2, got %q", resumed)
	}
}

func TestHistorySelectByArrow(t *testing.T) {
	var resumed string
	m := newSmokeModel(t, 80, 24, nil, func(id string) { resumed = id })
	m.Update(historyMsg([]HistoryItem{
		{ID: "sess-1", Title: "第一个会话", Subtitle: "今天"},
		{ID: "sess-2", Title: "第二个会话", Subtitle: "昨天"},
	}))

	// Open history overlay, move to second row, then resume.
	pressKey(m, 's', tea.ModCtrl)
	pressKey(m, tea.KeyDown, 0)
	pressKey(m, tea.KeyEnter, 0)

	if resumed != "sess-2" {
		t.Fatalf("expected resumed session sess-2 via arrow selection, got %q", resumed)
	}
}

func TestHelpOverlayOpenClose(t *testing.T) {
	m := newSmokeModel(t, 80, 24, nil, nil)
	pressKey(m, 'h', tea.ModCtrl)
	if m.overlay != overlayHelp {
		t.Fatalf("expected help overlay, got %d", m.overlay)
	}
	pressKey(m, tea.KeyEsc, 0)
	if m.overlay != overlayNone {
		t.Fatalf("expected help overlay to close, got %d", m.overlay)
	}
}

func TestQuestionMarkOpensHelpOverlay(t *testing.T) {
	m := newSmokeModel(t, 80, 24, nil, nil)
	typeText(m, "?")
	if m.overlay != overlayHelp {
		t.Fatalf("expected ? to open help overlay, got %d", m.overlay)
	}
}

func TestQuitDialogEnterDismissesDefaultNo(t *testing.T) {
	m := newSmokeModel(t, 80, 24, nil, nil)
	pressKey(m, 'c', tea.ModCtrl)
	if !m.showQuit {
		t.Fatal("expected Ctrl+C to open quit dialog")
	}
	pressKey(m, tea.KeyEnter, 0)
	if m.showQuit {
		t.Fatal("expected Enter on default No to dismiss quit dialog")
	}
	if m.quitting {
		t.Fatal("should not quit when default No is selected")
	}
}
