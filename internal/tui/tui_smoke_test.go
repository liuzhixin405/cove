package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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

// TestScrollLockResetsAtBottom verifies that PgUp disables auto-stick and
// PgDn back to bottom re-enables it.
func TestScrollLockResetsAtBottom(t *testing.T) {
	m := newSmokeModel(t, 40, 10, nil, nil)

	var long strings.Builder
	for i := 0; i < 120; i++ {
		long.WriteString("line\n")
	}
	m.appendSystem(long.String())

	if m.scrolledUp {
		t.Fatal("expected auto-stick enabled at bottom initially")
	}

	pressKey(m, tea.KeyPgUp, 0)
	if !m.scrolledUp {
		t.Fatal("expected PgUp to disable auto-stick")
	}

	pressKey(m, tea.KeyPgDown, 0)
	if m.scrolledUp {
		t.Fatal("expected PgDn at bottom to re-enable auto-stick")
	}
}

// TestMouseWheelUpdatesScrollLock verifies wheel scrolling updates auto-stick
// state the same way as keyboard scrolling.
func TestMouseWheelUpdatesScrollLock(t *testing.T) {
	m := newSmokeModel(t, 40, 10, nil, nil)

	var long strings.Builder
	for i := 0; i < 120; i++ {
		long.WriteString("line\n")
	}
	m.appendSystem(long.String())

	if m.scrolledUp {
		t.Fatal("expected auto-stick enabled at bottom initially")
	}

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if !m.scrolledUp {
		t.Fatal("expected wheel up to disable auto-stick")
	}

	for i := 0; i < 10; i++ {
		m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	}

	if m.scrolledUp {
		t.Fatal("expected wheel down back to bottom to re-enable auto-stick")
	}
}

// TestScrollDuringStreamingRecoversAutoStick verifies the full chain:
// user scrolls up to read history, streaming continues without stealing focus,
// then PgDn returns to bottom and auto-stick resumes for new deltas.
func TestScrollDuringStreamingRecoversAutoStick(t *testing.T) {
	m := newSmokeModel(t, 40, 10, nil, nil)

	maxOffset := func() int {
		mo := m.vp.TotalLineCount() - m.vp.Height()
		if mo < 0 {
			return 0
		}
		return mo
	}

	var long strings.Builder
	for i := 0; i < 140; i++ {
		long.WriteString("seed line\n")
	}
	m.appendSystem(long.String())

	if m.scrolledUp {
		t.Fatal("expected to start at bottom with auto-stick enabled")
	}

	pressKey(m, tea.KeyPgUp, 0)
	if !m.scrolledUp {
		t.Fatal("expected PgUp to disable auto-stick")
	}

	frozenOffset := m.vp.YOffset()
	m.Update(streamBeginMsg{})
	for i := 0; i < 30; i++ {
		m.Update(streamDeltaMsg("stream line\n"))
	}

	if got := m.vp.YOffset(); got != frozenOffset {
		t.Fatalf("expected viewport offset to stay frozen while manually scrolled, got=%d want=%d", got, frozenOffset)
	}
	if !m.scrolledUp {
		t.Fatal("expected auto-stick to remain disabled while reading history")
	}

	for i := 0; i < 20; i++ {
		pressKey(m, tea.KeyPgDown, 0)
	}

	if m.scrolledUp {
		t.Fatal("expected auto-stick to be re-enabled after returning to bottom")
	}
	if got, want := m.vp.YOffset(), maxOffset(); got != want {
		t.Fatalf("expected viewport at bottom after PgDn, got=%d want=%d", got, want)
	}

	m.Update(streamDeltaMsg("tail\n"))
	if got, want := m.vp.YOffset(), maxOffset(); got != want {
		t.Fatalf("expected new stream delta to keep sticking to bottom, got=%d want=%d", got, want)
	}

	m.Update(streamEndMsg{})
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

func TestTabCompletesSingleCommandInTUI(t *testing.T) {
	m := newSmokeModel(t, 80, 24, nil, nil)
	typeText(m, "/he")
	pressKey(m, tea.KeyTab, 0)

	if got, want := m.ta.Value(), "/help "; got != want {
		t.Fatalf("expected single command completion, got %q want %q", got, want)
	}
}

func TestTabExpandsCommonPrefixInTUI(t *testing.T) {
	m := New("test-model", nil, nil, nil, []CommandItem{
		{Name: "task", Desc: "task root"},
		{Name: "tasks", Desc: "task list"},
		{Name: "help", Desc: "help"},
	})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	typeText(m, "/ta")
	pressKey(m, tea.KeyTab, 0)

	if got, want := m.ta.Value(), "/task"; got != want {
		t.Fatalf("expected common-prefix expansion, got %q want %q", got, want)
	}
	if m.overlay != overlayNone {
		t.Fatalf("expected overlay to stay closed when prefix can expand, got %d", m.overlay)
	}
}

func TestTabNoMatchOpensCommandPalette(t *testing.T) {
	m := newSmokeModel(t, 80, 24, nil, nil)
	typeText(m, "/zzz")
	pressKey(m, tea.KeyTab, 0)

	if m.overlay != overlayCommand {
		t.Fatalf("expected command palette on no-match tab completion, got %d", m.overlay)
	}
	if got, want := m.search.Value(), "zzz"; got != want {
		t.Fatalf("expected palette query to keep typed token, got %q want %q", got, want)
	}
}

func TestCtrlIAlsoTriggersTabCompletion(t *testing.T) {
	m := newSmokeModel(t, 80, 24, nil, nil)
	typeText(m, "/he")
	pressKey(m, 'i', tea.ModCtrl)

	if got, want := m.ta.Value(), "/help "; got != want {
		t.Fatalf("expected Ctrl+I to behave as Tab completion, got %q want %q", got, want)
	}
}

func TestSlashInputShowsCompletionHint(t *testing.T) {
	m := newSmokeModel(t, 80, 24, nil, nil)
	typeText(m, "/h")
	view := m.View().Content
	if !strings.Contains(view, "补全:") {
		t.Fatal("expected live completion hint while typing slash command")
	}
	if !strings.Contains(view, "/help") {
		t.Fatal("expected hint to include matching /help command")
	}
}

func TestSlashUnknownShowsNoMatchHint(t *testing.T) {
	m := newSmokeModel(t, 80, 24, nil, nil)
	typeText(m, "/zzzz")
	view := m.View().Content
	if !strings.Contains(view, "无匹配") {
		t.Fatal("expected no-match hint for unknown slash command")
	}
}

func TestNarrowViewIsValidUTF8(t *testing.T) {
	m := newSmokeModel(t, 18, 10, nil, nil)
	m.Update(statusUpdateMsg(StatusInfo{
		Version:  "6.2.1",
		Model:    "gpt-5.3-codex",
		Provider: "openai",
		PermMode: "default",
		TokensIn: 39800,
		Budget:   10,
	}))
	v := m.View().Content
	if !utf8.ValidString(v) {
		t.Fatal("expected rendered view to be valid UTF-8 on narrow width")
	}
}

func TestReportedLayoutScenario_NoGarbleNoOverflow(t *testing.T) {
	m := newSmokeModel(t, 96, 12, nil, nil)
	m.Update(statusUpdateMsg(StatusInfo{
		Version:  "6.2.1",
		Model:    "gpt-5.3-codex",
		Provider: "openai",
		PermMode: "default",
		TokensIn: 39800,
		Budget:   10,
	}))
	m.Update(activityMsg(".   .   . thinking..."))

	v := m.View().Content
	if !utf8.ValidString(v) {
		t.Fatal("expected rendered frame to remain valid UTF-8 in reported scenario")
	}

	for i, line := range strings.Split(v, "\n") {
		if w := ansi.StringWidth(line); w > m.width {
			t.Fatalf("line %d overflowed viewport width: got %d > %d; line=%q", i+1, w, m.width, line)
		}
	}
}

func TestTransientLineIdleIsBlank(t *testing.T) {
	m := newSmokeModel(t, 96, 12, nil, nil)
	m.Update(statusUpdateMsg(StatusInfo{
		Version:  "7.1.0",
		Model:    "gpt-5.3-codex",
		Provider: "openai",
		PermMode: "default",
	}))

	line := stripANSI(m.renderTransient())
	if strings.TrimSpace(line) != "" {
		t.Fatalf("expected idle transient line to be blank, got %q", line)
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

func TestCtrlYCopiesCurrentSession(t *testing.T) {
	t.Setenv("COVE_TUI_MOUSE", "")
	m := newSmokeModel(t, 40, 10, nil, nil)
	if m.currentMouseMode() != tea.MouseModeCellMotion {
		t.Fatalf("expected mouse capture by default, got %v", m.currentMouseMode())
	}
	m.appendSystem("\n[系统] startup\n")
	m.echoUser("hello")
	m.Update(streamBeginMsg{})
	m.Update(streamDeltaMsg("world"))
	m.Update(streamEndMsg{})
	pressKey(m, 'y', tea.ModCtrl)
	if !m.mouseCapture || m.currentMouseMode() != tea.MouseModeCellMotion {
		t.Fatal("expected Ctrl+Y to keep mouse capture unchanged")
	}
	if !strings.Contains(m.copyNotice, "[复制]") {
		t.Fatalf("expected Ctrl+Y to set a copy notice, got %q", m.copyNotice)
	}
}

func TestF6CopiesVisibleScreenAndKeepsMouseMode(t *testing.T) {
	t.Setenv("COVE_TUI_MOUSE", "")
	m := newSmokeModel(t, 40, 10, nil, nil)
	m.echoUser("u1")
	m.Update(streamBeginMsg{})
	m.Update(streamDeltaMsg("a1\na2\na3\na4\na5\na6\na7\na8"))
	m.Update(streamEndMsg{})
	m.vp.GotoBottom()
	pressKey(m, tea.KeyF6, 0)
	if !m.mouseCapture || m.currentMouseMode() != tea.MouseModeCellMotion {
		t.Fatal("expected F6 to keep mouse capture unchanged")
	}
	if !strings.Contains(m.copyNotice, "[复制]") {
		t.Fatalf("expected F6 to set a copy notice, got %q", m.copyNotice)
	}
}

func TestF7CopiesAllAndKeepsMouseMode(t *testing.T) {
	t.Setenv("COVE_TUI_MOUSE", "")
	m := newSmokeModel(t, 40, 10, nil, nil)
	m.appendSystem("\n[系统] startup\n")
	m.echoUser("hello")
	m.Update(streamBeginMsg{})
	m.Update(streamDeltaMsg("world"))
	m.Update(streamEndMsg{})
	pressKey(m, tea.KeyF7, 0)
	if !m.mouseCapture || m.currentMouseMode() != tea.MouseModeCellMotion {
		t.Fatal("expected F7 to keep mouse capture unchanged")
	}
	if !strings.Contains(m.copyNotice, "[复制]") {
		t.Fatalf("expected F7 to set a copy notice, got %q", m.copyNotice)
	}
}

func TestF7KeepsMouseModeWhenNoTranscript(t *testing.T) {
	t.Setenv("COVE_TUI_MOUSE", "")
	m := newSmokeModel(t, 40, 10, nil, nil)
	pressKey(m, tea.KeyF7, 0)
	if !m.mouseCapture || m.currentMouseMode() != tea.MouseModeCellMotion {
		t.Fatal("expected F7 to keep mouse capture on empty transcript")
	}
	if !strings.Contains(m.copyNotice, "当前无可复制内容") {
		t.Fatalf("expected F7 empty-transcript notice, got %q", m.copyNotice)
	}
}

func TestExportSessionTextPrefersCleanStreamedAnswer(t *testing.T) {
	m := newSmokeModel(t, 80, 24, nil, nil)
	m.echoUser("请总结")
	m.Update(streamBeginMsg{})
	m.Update(streamReasoningMsg("先思考..."))
	m.Update(streamDeltaMsg("最终答案"))
	m.Update(engineLineMsg("\n  [工具返回] read file\n"))
	m.Update(streamEndMsg{final: "最终答案"})

	got := m.exportSessionText()
	if strings.Contains(got, "工具返回") {
		t.Fatalf("session export should not contain tool logs, got: %q", got)
	}
	if strings.Contains(got, "先思考") {
		t.Fatalf("session export should not contain reasoning, got: %q", got)
	}
	if !strings.Contains(got, "最终答案") {
		t.Fatalf("session export should contain assistant final answer, got: %q", got)
	}
}

func TestExportTranscriptTextIncludesFullContent(t *testing.T) {
	m := newSmokeModel(t, 80, 24, nil, nil)
	m.appendSystem("\n[系统] startup\n")
	m.echoUser("你好")
	m.Update(streamBeginMsg{})
	m.Update(streamReasoningMsg("thinking..."))
	m.Update(streamDeltaMsg("hello"))
	m.Update(streamEndMsg{final: "hello"})

	got := m.exportTranscriptText()
	if !strings.Contains(got, "[SYSTEM]") || !strings.Contains(got, "startup") {
		t.Fatalf("transcript export should contain system lines, got: %q", got)
	}
	if !strings.Contains(got, "[REASONING]") || !strings.Contains(got, "thinking") {
		t.Fatalf("transcript export should contain reasoning lines, got: %q", got)
	}
	if !strings.Contains(got, "[USER]") || !strings.Contains(got, "[ASSISTANT]") {
		t.Fatalf("transcript export should contain USER/ASSISTANT sections, got: %q", got)
	}
}

func TestExportCurrentTurnTextReturnsLatestDialogue(t *testing.T) {
	m := newSmokeModel(t, 80, 24, nil, nil)
	m.echoUser("第一问")
	m.Update(streamBeginMsg{})
	m.Update(streamDeltaMsg("第一答"))
	m.Update(streamEndMsg{final: "第一答"})
	m.echoUser("第二问")
	m.Update(streamBeginMsg{})
	m.Update(streamDeltaMsg("第二答"))
	m.Update(streamEndMsg{final: "第二答"})

	got := m.exportCurrentTurnText()
	if strings.Contains(got, "第一问") || strings.Contains(got, "第一答") {
		t.Fatalf("current turn export should only include latest turn, got: %q", got)
	}
	if !strings.Contains(got, "第二问") || !strings.Contains(got, "第二答") {
		t.Fatalf("current turn export should include latest turn, got: %q", got)
	}
}
