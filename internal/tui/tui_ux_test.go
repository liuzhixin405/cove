package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestQuestionMarkTypable is the regression test for "?" being unusable as a
// character: it was bound unconditionally to the help overlay, so every "?"
// typed into a message opened help and was swallowed.
func TestQuestionMarkTypable(t *testing.T) {
	m := newSmokeModel(t, 80, 24, nil, nil)

	// On an empty input line "?" is still the help shortcut.
	typeText(m, "?")
	if m.overlay != overlayHelp {
		t.Fatalf("? on an empty line did not open help (overlay=%d)", m.overlay)
	}
	pressKey(m, tea.KeyEscape, 0)
	if m.overlay != overlayNone {
		t.Fatal("Esc did not close the help overlay")
	}

	// With text in the box, "?" must be inserted as a character.
	typeText(m, "这样对吗?")
	if got := m.ta.Value(); got != "这样对吗?" {
		t.Fatalf("input = %q, want %q", got, "这样对吗?")
	}
	if m.overlay != overlayNone {
		t.Fatalf("typing ? mid-message opened an overlay (overlay=%d)", m.overlay)
	}

	// Several in a row, and one in the middle.
	m.ta.SetValue("")
	typeText(m, "a??b?")
	if got := m.ta.Value(); got != "a??b?" {
		t.Fatalf("input = %q, want %q", got, "a??b?")
	}
}

// TestMouseCaptureToggle covers the F2 binding added so native drag-to-select
// copy can be turned back on. Before it, capture was decided once at startup
// from COVE_TUI_MOUSE and there was no in-app way to release the mouse.
func TestMouseCaptureToggle(t *testing.T) {
	m := newSmokeModel(t, 80, 24, nil, nil)
	start := m.mouseCapture

	pressKey(m, tea.KeyF2, 0)
	if m.mouseCapture == start {
		t.Fatal("F2 did not toggle mouse capture")
	}
	if m.currentMouseMode() != tea.MouseModeNone && !m.mouseCapture {
		t.Fatal("capture off but mouse mode still reports capture")
	}
	if m.copyNotice == "" {
		t.Fatal("toggling mouse capture produced no user-visible notice")
	}

	pressKey(m, tea.KeyF2, 0)
	if m.mouseCapture != start {
		t.Fatal("F2 did not toggle mouse capture back")
	}

	// The bottom bar must advertise the current state, otherwise the user has
	// no way to tell why selection stopped working.
	bar := ansi.Strip(m.renderBottomBar())
	if !strings.Contains(bar, "F2") {
		t.Fatalf("bottom bar does not mention F2: %q", bar)
	}
}

// TestCopyExportsAreEscapeFree is the regression test for copied text carrying
// terminal escapes into the clipboard. Every export path must yield plain text.
func TestCopyExportsAreEscapeFree(t *testing.T) {
	m := newSmokeModel(t, 80, 24, nil, nil)

	m.Update(streamBeginMsg{})
	// Styled engine/tool output, as the engine emits it.
	m.Update(engineLineMsg("\x1b[36m  ✓ read\x1b[0m internal/tui/tui.go\n"))
	m.Update(streamDeltaMsg("这是回答 with \x1b[1mbold\x1b[0m text\n"))
	m.Update(streamReasoningMsg("思考中 \x1b[2mdim\x1b[0m\n"))
	m.Update(streamEndMsg{})

	for name, got := range map[string]string{
		"session":    m.exportSessionText(),
		"transcript": m.exportTranscriptText(),
		"screen":     m.exportVisibleScreenText(),
		"turn":       m.exportCurrentTurnText(),
	} {
		if strings.ContainsRune(got, 0x1b) {
			t.Errorf("%s export still contains an ESC byte: %q", name, got)
		}
		for _, r := range got {
			if r == '\n' || r == '\t' {
				continue
			}
			if r < 0x20 || r == 0x7f {
				t.Errorf("%s export contains control byte %#x: %q", name, r, got)
				break
			}
		}
	}
}

// TestClampFrameNeverExceedsTerminal covers the frame guard: an over-tall or
// over-wide block must not be allowed to push the frame past the terminal,
// which is what scrolled the alt-screen origin and corrupted the layout.
func TestClampFrameNeverExceedsTerminal(t *testing.T) {
	m := newSmokeModel(t, 40, 12, nil, nil)

	tooTall := strings.Repeat("row\n", 40)
	got := m.clampFrame(tooTall)
	if n := strings.Count(got, "\n") + 1; n > m.height {
		t.Fatalf("clampFrame returned %d rows, terminal has %d", n, m.height)
	}

	tooWide := strings.Repeat("宽", 60)
	got = m.clampFrame(tooWide)
	if w := ansi.StringWidth(got); w > m.width {
		t.Fatalf("clampFrame returned width %d, terminal has %d", w, m.width)
	}

	// A frame that already fits must pass through untouched.
	fits := "a\nb\nc"
	if got := m.clampFrame(fits); got != fits {
		t.Fatalf("clampFrame altered a fitting frame: %q", got)
	}
}

// TestThinkingClickRegionsMatchRenderedRows is the regression test for the
// mouse click-to-fold mapping. It was computed by counting one line per write
// call, so soft-wrapped blocks threw the indices off and clicking a header did
// nothing (or hit the wrong turn).
func TestThinkingClickRegionsMatchRenderedRows(t *testing.T) {
	// Tall enough that the whole transcript fits in the viewport, so the
	// viewport's own rows line up 1:1 with the content lines the regions index.
	m := newSmokeModel(t, 40, 40, nil, nil)

	// Two turns whose user line and reasoning both soft-wrap at width 40.
	for i := 0; i < 2; i++ {
		m.turns = append(m.turns, &turn{user: strings.Repeat("问题很长会折行 ", 4)})
		tn := m.turns[len(m.turns)-1]
		tn.reasoning.WriteString(strings.Repeat("思考内容需要折行 ", 4))
		tn.answer.WriteString("回答")
	}
	m.refreshViewport(false)
	m.vp.GotoTop()

	if len(m.clickRegions) != 2 {
		t.Fatalf("got %d click regions, want 2", len(m.clickRegions))
	}

	rows := strings.Split(m.vp.View(), "\n")
	for _, r := range m.clickRegions {
		if r.endLine <= r.startLine {
			t.Fatalf("degenerate region %+v", r)
		}
		if r.startLine < 0 || r.startLine >= len(rows) {
			t.Fatalf("region %+v points outside the %d rendered rows", r, len(rows))
		}
		// The region must start on a row that actually holds the header. If the
		// wrapped user line and reasoning body were not counted, this lands on
		// ordinary text instead.
		row := ansi.Strip(rows[r.startLine])
		if !strings.Contains(row, "思考过程") {
			t.Fatalf("region %+v starts at %q, which is not a thinking header", r, row)
		}
	}
	if m.clickRegions[0].startLine == m.clickRegions[1].startLine {
		t.Fatal("both regions resolved to the same row")
	}
}

// TestThinkingClickTogglesCorrectTurn drives a real click through Update.
func TestThinkingClickTogglesCorrectTurn(t *testing.T) {
	m := newSmokeModel(t, 60, 24, nil, nil)

	for i := 0; i < 2; i++ {
		m.turns = append(m.turns, &turn{user: "q"})
		tn := m.turns[len(m.turns)-1]
		tn.reasoning.WriteString(strings.Repeat("推理 ", 40))
		tn.answer.WriteString("answer")
	}
	m.refreshViewport(false)
	m.vp.GotoTop()

	target := m.clickRegions[1]
	before := m.turns[target.turnIdx].expanded

	vpTop := statusH + m.gitPanelHeight()
	y := vpTop + (target.startLine - m.vp.YOffset())
	m.Update(tea.MouseClickMsg{Y: y, Button: tea.MouseLeft})

	if m.turns[target.turnIdx].expanded == before {
		t.Fatalf("clicking the header of turn %d did not toggle its fold state", target.turnIdx)
	}
}
