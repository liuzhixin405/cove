// Package tui implements a full-screen, Bubble Tea based terminal UI for cove.
//
// Design rationale (see /memories/repo/tui-architecture.md): the legacy
// interactive layer (internal/repl) drives the terminal with hand-written ANSI
// escape sequences and in-place erase/redraw. That line-based model cannot
// support a split layout reliably once streaming output, async tasks, resize
// and Windows consoles are combined. This package replaces that approach with
// an alternate-screen, whole-frame-redraw model (Model-Update-View), where the
// layout is computed every frame instead of nudged with cursor moves.
//
// The package is intentionally decoupled from the engine: it only knows how to
// render data and emit user submissions through a callback. The caller bridges
// engine streaming callbacks into TUI messages via the App.Send* helpers.
package tui

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/liuzhixin405/cove/internal/tui/scrollstate"
	"github.com/liuzhixin405/cove/internal/tui/theme"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// renderMarkdown renders markdown text s to ANSI-styled terminal output using
// goldmark with simple inline styling (bold, italic, code, links, headers).
// Returns s unchanged on any error.
func renderMarkdown(s string) string {
	md := goldmark.New()
	reader := text.NewReader([]byte(s))
	root := md.Parser().Parse(reader)
	if root == nil {
		return s
	}
	var out strings.Builder
	renderNode(&out, root, []byte(s), 0)
	return strings.TrimRight(out.String(), "\n")
}

func renderNode(w *strings.Builder, node ast.Node, source []byte, depth int) {
	th := theme.Current()
	switch n := node.(type) {
	case *ast.Document:
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			renderNode(w, child, source, depth)
		}
	case *ast.Paragraph:
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			renderNode(w, child, source, depth)
		}
		w.WriteString("\n\n")

	case *ast.Heading:
		level := n.Level
		var prefix string
		switch level {
		case 1:
			prefix = "█ "
		case 2:
			prefix = "▌ "
		default:
			prefix = "▪ "
		}
		w.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(th.Primary)).Render(prefix))
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			renderNode(w, child, source, depth+1)
		}
		w.WriteString("\n\n")

	case *ast.Text:
		val := string(n.Segment.Value(source))
		if n.SoftLineBreak() {
			val += "\n"
		}
		w.WriteString(val)

	case *ast.String:
		w.WriteString(string(n.Value))

	case *ast.CodeSpan:
		var code strings.Builder
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			if text, ok := child.(*ast.Text); ok {
				code.WriteString(string(text.Segment.Value(source)))
			}
		}
		w.WriteString(lipgloss.NewStyle().Background(lipgloss.Color(th.CodeBG)).Foreground(lipgloss.Color(th.CodeAccent)).Render(code.String()))

	case *ast.FencedCodeBlock:
		w.WriteString("\n")
		lines := n.Lines()
		for i := 0; i < lines.Len(); i++ {
			line := lines.At(i)
			code := string(line.Value(source))
			w.WriteString(lipgloss.NewStyle().
				Background(lipgloss.Color(th.CodeBG)).
				Foreground(lipgloss.Color(th.CodeFG)).
				PaddingLeft(2).
				Render(code))
		}
		w.WriteString("\n\n")

	case *ast.List:
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			renderNode(w, child, source, depth+1)
		}

	case *ast.ListItem:
		prefix := "  "
		for i := 0; i < depth; i++ {
			prefix += "  "
		}
		if parentList, ok := n.Parent().(*ast.List); ok {
			if !parentList.IsOrdered() {
				prefix += "• "
			} else {
				prefix += "  "
			}
		}
		w.WriteString(prefix)
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			renderNode(w, child, source, depth+1)
		}
		w.WriteString("\n")

	case *ast.Blockquote:
		w.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(th.Blockquote)).Italic(true).Render("│ "))
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			renderNode(w, child, source, depth+1)
		}

	case *ast.Emphasis:
		tag := 1
		if n.Attributes() != nil {
			if v, ok := n.AttributeString("level"); ok {
				if l, ok := v.(int); ok && l > 1 {
					tag = l
				}
			}
		}
		style := lipgloss.NewStyle()
		if tag >= 2 {
			style = style.Bold(true)
		} else {
			style = style.Italic(true)
		}
		var inner strings.Builder
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			renderNode(&inner, child, source, depth+1)
		}
		w.WriteString(style.Render(inner.String()))

	case *ast.Link:
		dest := string(n.Destination)
		var label strings.Builder
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			renderNode(&label, child, source, depth+1)
		}
		if label.Len() > 0 {
			w.WriteString(label.String())
			w.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(th.Link)).Underline(true).Render(" (" + dest + ")"))
		} else {
			w.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(th.Link)).Underline(true).Render(dest))
		}

	case *ast.AutoLink:
		url := string(n.URL(source))
		w.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(th.Link)).Underline(true).Render(url))

	case *ast.Image:
		alt := string(n.Text(source))
		if alt != "" {
			w.WriteString("[图: " + alt + "]")
		} else {
			w.WriteString("[图]")
		}

	case *ast.ThematicBreak:
		w.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(th.Border)).Render(strings.Repeat("─", 40)) + "\n")

	default:
		// Fallback: render children only
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			renderNode(w, child, source, depth+1)
		}
	}
}

// TaskInfo is a render-only snapshot of the async task runner state. The caller
// converts its own task snapshot into this struct to avoid a dependency cycle.
type TaskInfo struct {
	Running      bool
	Current      string
	Elapsed      string
	Queued       []string
	PendingRetry string
}

// StatusInfo carries status-bar data shown in the top and bottom bars.
type StatusInfo struct {
	Version   string
	Model     string
	Provider  string
	Git       string // e.g. "main*" ("" when not a repo)
	GitStatus string // raw git status output representing modified files
	PermMode  string
	TokensIn  int
	TokensOut int
	Cost      float64
	Budget    float64
	Elapsed   string
}

// HistoryItem is a render-only session entry (used by the history overlay).
type HistoryItem struct {
	ID       string
	Title    string
	Subtitle string
}

// CommandItem is a render-only slash-command entry (used by the command palette).
type CommandItem struct {
	Name string
	Desc string
}

// PermDecision is the user's answer to an interactive permission prompt.
type PermDecision int

const (
	PermDeny   PermDecision = iota // reject this tool call
	PermAllow                      // allow this tool call once
	PermAlways                     // allow and remember (caller adds a rule)
)

// permRequest carries an interactive permission prompt from the worker
// goroutine into the UI plus the reply channel the worker blocks on.
type permRequest struct {
	tool  string
	desc  string
	reply chan PermDecision
}

// Internal Bubble Tea messages. They are sent into the program from background
// goroutines via App.Send* helpers (which call (*tea.Program).Send).
type (
	streamBeginMsg     struct{ echo string }
	streamDeltaMsg     string
	streamReasoningMsg string
	engineLineMsg      string
	streamEndMsg       struct{ final string }
	taskStateMsg       TaskInfo
	statusUpdateMsg    StatusInfo
	historyMsg         []HistoryItem
	activityMsg        string
	permRequestMsg     permRequest
	// copyDoneMsg reports the result of an async clipboard copy back to the UI
	// goroutine so the copy subprocess never blocks rendering.
	copyDoneMsg struct {
		label string
		err   error
	}
)

// overlay modes for the modal palette (history search / command palette).
const (
	overlayNone = iota
	overlayHistory
	overlayCommand
	overlayHelp
	overlayPermission
)

// turn is one structured exchange in the conversation transcript. Keeping turns
// structured (rather than a flat text stream) is what lets each turn's thinking
// be folded independently and toggled by Alt+T.
//
// A turn is normally a user message plus the assistant's reasoning + answer.
// A system turn (system==true) carries standalone engine output (resume notices,
// command results) and has no foldable thinking.
type turn struct {
	user         string          // user input ("" for system/assistant-only turns)
	reasoning    strings.Builder // streamed thinking; display/fold only
	answer       strings.Builder // streamed answer + interleaved engine/tool lines
	streamedText strings.Builder // ONLY text deltas (no engine lines); used for end-of-stream alignment
	expanded     bool            // true = show thinking (toggled by Alt+T)
	system       bool            // standalone engine output, not foldable
}

// clickRegion maps a viewport content line range to a conversation turn, so
// mouse clicks on the thinking header toggle the fold state of that turn.
type clickRegion struct {
	startLine int
	endLine   int
	turnIdx   int
}

// Model is the root Bubble Tea model holding all UI state.
type Model struct {
	vp     viewport.Model
	ta     textarea.Model
	width  int
	height int
	ready  bool

	// turns is the structured conversation transcript. It is re-rendered into
	// the viewport on every change (refreshViewport).
	turns     []*turn
	streaming bool
	// curTurn is the active exchange turn index (-1 when none). streamTurn is the
	// turn currently receiving streamed deltas (-1 when not streaming).
	curTurn    int
	streamTurn int

	status  StatusInfo
	task    TaskInfo
	history []HistoryItem

	// commands is the static slash-command catalog shown in the / palette.
	commands []CommandItem

	// activity is a short transient line shown just above the input while a
	// task/tool is running ("" hides the transient zone entirely).
	activity string

	gitExpanded bool

	// Mouse mode: captures click/release/wheel so the app can toggle thinking
	// headers and scroll inside the conversation body. Native text selection
	// still works in most terminals by holding Shift while clicking/dragging.
	// overlay is the modal layer drawn over the conversation body
	// (overlayNone when hidden). search/overlayIdx drive it.
	overlay    int
	search     textinput.Model
	overlayIdx int

	// permission-prompt overlay state. permReply is the channel the blocked
	// worker goroutine waits on; it is non-nil only while a prompt is showing.
	permTool     string
	permDesc     string
	permReply    chan PermDecision
	permSelected int // 0=Allow, 1=AllowAlways, 2=Deny

	showQuit       bool
	quitSelectedNo bool // true = "No" selected (default, safe)

	onSubmit    func(string)
	onResume    func(string)
	onInterrupt func()
	quitting    bool
	// clickRegions tracks where each turn's thinking header is rendered in the viewport
	// content, enabling mouse-click toggling of the fold state.
	clickRegions []clickRegion
	// scrolledUp is set when the user manually scrolls the viewport with PgUp/Dn or arrows.
	// When true, stream deltas do NOT auto-scroll to the bottom, preserving the user's
	// reading position. Reset on new message submit or PgDn to bottom.
	scrolledUp bool
	// mouseCapture controls whether Bubble Tea captures mouse events.
	// false means native terminal selection works without Shift.
	mouseCapture bool
	// copyNotice is a transient one-line status shown above the input after a
	// copy key (Ctrl+Y/F6/F7). It is dismissed on the next key press and kept
	// OUT of the transcript, so copying never grows the conversation.
	copyNotice string
}

// New constructs a Model. onSubmit is invoked (on the UI goroutine) whenever the
// user submits a line from the input box. onResume is invoked with a session ID
// when the user picks an entry from the history overlay. onInterrupt is invoked
// when the user requests to interrupt a running task (Esc in the main view).
// commands is the static catalog shown in the command palette.
func New(modelName string, onSubmit, onResume func(string), onInterrupt func(), commands []CommandItem) *Model {
	ta := textarea.New()
	ta.Placeholder = "输入指令…"
	// ASCII prompt only: ambiguous-width glyphs (e.g. ┃) render as 2 columns in
	// CJK terminals, which can push the input line past the terminal width and
	// wrap it onto a second row.
	ta.Prompt = "> "
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	// Use the real terminal cursor (positioned via the tea.View in View()) rather
	// than a rendered "virtual" block. This is what lets a CJK IME draw its
	// preedit (pinyin) at the actual input position, including inside overlays.
	ta.SetVirtualCursor(false)
	ta.Focus()

	si := textinput.New()
	si.Placeholder = "搜索会话…"
	si.Prompt = "🔍 "
	si.SetVirtualCursor(false)

	vp := viewport.New(viewport.WithWidth(10), viewport.WithHeight(10))
	vp.FillHeight = true

	return &Model{
		ta:             ta,
		vp:             vp,
		search:         si,
		status:         StatusInfo{Model: modelName},
		commands:       commands,
		onSubmit:       onSubmit,
		onResume:       onResume,
		onInterrupt:    onInterrupt,
		curTurn:        -1,
		streamTurn:     -1,
		gitExpanded:    false,
		quitSelectedNo: true,
		mouseCapture:   defaultMouseCapture(),
	}
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, textinput.Blink)
}

// appendSystem records standalone engine output (resume notices, command
// results, errors) that is not part of an active streaming exchange. Consecutive
// system output coalesces into a single non-foldable system turn.
func (m *Model) appendSystem(s string) {
	if n := len(m.turns); n > 0 && m.turns[n-1].system {
		m.turns[n-1].answer.WriteString(s)
	} else {
		t := &turn{system: true}
		t.answer.WriteString(s)
		m.turns = append(m.turns, t)
	}
	m.refreshViewport(true)
}

// refreshViewport re-renders the structured transcript into the viewport.
func (m *Model) refreshViewport(stick bool) {
	if !m.ready {
		return
	}
	w := m.vp.Width()
	if w < 1 {
		w = 1
	}
	wrap := lipgloss.NewStyle().Width(w)

	var b strings.Builder

	// contentLine tracks the current line number within the built content.
	// Used to build clickRegions for mouse click -> thinking fold toggle.
	contentLine := 0

	write := func(s string) {
		r := wrap.Render(s)
		if b.Len() > 0 {
			b.WriteByte('\n')
			contentLine++
		}
		b.WriteString(r)
	}

	m.clickRegions = m.clickRegions[:0]

	for ti, t := range m.turns {
		if ti > 0 {
			write("")
		}
		if t.system {
			write(strings.TrimRight(t.answer.String(), "\n"))
			continue
		}
		if t.user != "" {
			write(userStyle.Render("\u203a " + t.user))
		}
		reasoning := strings.TrimRight(t.reasoning.String(), "\n")
		answer := strings.TrimRight(t.answer.String(), "\n")
		if reasoning != "" {
			live := ti == m.streamTurn && m.streaming && answer == ""
			expanded := t.expanded || live

			regionStart := contentLine
			if expanded {
				write(thinkHeaderStyle.Render("\u25be \u601d\u8003\u8fc7\u7a0b"))
				write(dimStyle.Render(reasoning))
				if answer != "" {
					write("")
				}
			} else {
				write(thinkHeaderStyle.Render("\u25b8 \u601d\u8003\u8fc7\u7a0b (Alt+T\u5c55\u5f00)"))
			}
			m.clickRegions = append(m.clickRegions, clickRegion{
				startLine: regionStart,
				endLine:   contentLine,
				turnIdx:   ti,
			})
		}
		if answer != "" {
			write(answer)
		}
	}

	m.vp.SetContent(b.String())
	// Only auto-scroll when the user hasn't manually scrolled up.
	// This preserves the user's reading position when new stream content arrives.
	if stick && !m.scrolledUp {
		m.vp.GotoBottom()
	}
	m.syncScrollLock()
}

// syncScrollLock recalculates whether auto-stick should remain disabled.
// It is true only when the user is not at the bottom of the transcript.
func (m *Model) syncScrollLock() {
	m.scrolledUp = scrollstate.IsUserScrolledUp(m.vp.YOffset(), m.vp.TotalLineCount(), m.vp.Height())
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.ready = true
		m.refreshViewport(true)

	case tea.KeyPressMsg:
		// Any key press dismisses a lingering copy notice.
		m.copyNotice = ""
		if m.showQuit {
			return m.handleQuitDialog(msg)
		}
		if msg.String() == "ctrl+c" {
			// OpenCode-style global quit shortcut.
			m.showQuit = true
			m.quitSelectedNo = true
			m.ta.Blur()
			return m, nil
		}
		if m.overlay != overlayNone {
			return m.updateOverlay(msg)
		}
		if isTabKey(msg) {
			if m.tabCompleteInput() {
				return m, nil
			}
			return m, nil
		}
		switch msg.String() {
		case "ctrl+s":
			// OpenCode: Ctrl+S switches/restores sessions.
			m.openHistory()
			return m, nil
		case "ctrl+k":
			// OpenCode: Ctrl+K command palette.
			m.openCommandsWithQuery("")
			return m, nil
		case "ctrl+o":
			// OpenCode: Ctrl+O model picker. Map to command palette filtered to model commands.
			m.openCommandsWithQuery("model")
			return m, nil
		case "ctrl+f":
			// OpenCode: Ctrl+F file picker. Map to attachment command flow.
			m.openCommandsWithQuery("attach")
			return m, nil
		case "ctrl+l":
			// OpenCode: Ctrl+L logs. Map to task/log related commands in palette.
			m.openCommandsWithQuery("tasks")
			return m, nil
		case "ctrl+h", "ctrl+_", "?":
			// OpenCode: help overlay.
			m.openHelp()
			return m, nil
		case "esc":
			// OpenCode-style cancel behavior in chat view.
			if (m.task.Running || m.streaming) && m.onInterrupt != nil {
				m.onInterrupt()
			}
			return m, nil
		case "ctrl+g":
			status := strings.TrimSpace(m.status.GitStatus)
			if status != "" && status != "(clean)" {
				m.gitExpanded = !m.gitExpanded
				m.layout()
				m.refreshViewport(true)
			}
			return m, nil
		case "ctrl+t":
			// Cycle through available themes.
			names := theme.Names()
			for i, n := range names {
				if n == theme.Current().Name {
					next := names[(i+1)%len(names)]
					if _, ok := theme.SetTheme(next); ok {
						applyTheme()
						m.refreshViewport(false)
					}
					break
				}
			}
			return m, nil
		case "alt+t":
			// Toggle thinking fold on the last assistant turn.
			for i := len(m.turns) - 1; i >= 0; i-- {
				if !m.turns[i].system && m.turns[i].reasoning.Len() > 0 {
					m.turns[i].expanded = !m.turns[i].expanded
					m.refreshViewport(false)
					break
				}
			}
			return m, nil
		case "ctrl+y":
			text := m.exportSessionText()
			if strings.TrimSpace(text) == "" {
				m.copyNotice = "[复制] 当前无可复制内容"
				return m, nil
			}
			m.copyNotice = "[复制] 正在复制…"
			return m, copyCmd(text, "当前会话")
		case "f6":
			// Copy only the currently visible conversation viewport.
			text := m.exportVisibleScreenText()
			if strings.TrimSpace(text) == "" {
				m.copyNotice = "[复制] 当前无可复制内容"
				return m, nil
			}
			m.copyNotice = "[复制] 正在复制…"
			return m, copyCmd(text, "当前屏幕内容")
		case "f7":
			text := m.exportTranscriptText()
			if strings.TrimSpace(text) == "" {
				m.copyNotice = "[复制] 当前无可复制内容"
				return m, nil
			}
			m.copyNotice = "[复制] 正在复制…"
			return m, copyCmd(text, "所有内容")
		case "enter":
			m.scrolledUp = false
			input := strings.TrimRight(m.ta.Value(), "\r\n")
			if len(input) > 0 && input[len(input)-1] == '\\' {
				// OpenCode-compatible continuation: trailing "\\" + Enter inserts
				// a newline instead of sending the message.
				m.ta.SetValue(input[:len(input)-1] + "\n")
				return m, nil
			}
			if strings.TrimSpace(input) != "" {
				m.echoUser(input)
				if m.onSubmit != nil {
					m.onSubmit(input)
				}
			}
			m.ta.Reset()
			return m, nil
		case "ctrl+j":
			// Insert a literal newline into the input box.
			m.ta.InsertRune('\n')
			return m, nil
		case "ctrl+u":
			// Clear the input box (bash/readline convention).
			m.ta.Reset()
			return m, nil
		case "up", "down", "pgup", "pgdown", "ctrl+d":
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			m.syncScrollLock()
			return m, cmd
		}

	case tea.MouseClickMsg:
		// Left-click on a thinking header toggles fold state.
		// MouseClickMsg is a Mouse struct with X, Y fields directly.
		if m.showQuit || m.overlay != overlayNone {
			return m, nil
		}
		// Viewport starts at terminal line 1 (after the 1-line status bar).
		vpTop := 1
		if msg.Y < vpTop || msg.Y >= vpTop+m.vp.Height() {
			return m, nil
		}
		vpLine := msg.Y - vpTop            // 0-indexed within viewport display
		absLine := vpLine + m.vp.YOffset() // absolute content line
		for _, r := range m.clickRegions {
			if absLine >= r.startLine && absLine < r.endLine {
				t := m.turns[r.turnIdx]
				if !t.system {
					t.expanded = !t.expanded
					m.refreshViewport(false)
				}
				break
			}
		}
		return m, nil

	case tea.MouseWheelMsg:
		if m.showQuit || m.overlay != overlayNone {
			return m, nil
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		m.syncScrollLock()
		return m, cmd

	case streamBeginMsg:
		m.streaming = true
		if m.curTurn < 0 {
			m.turns = append(m.turns, &turn{})
			m.curTurn = len(m.turns) - 1
		}
		m.streamTurn = m.curTurn
		if msg.echo != "" {
			m.turns[m.streamTurn].answer.WriteString(msg.echo)
		}
		m.refreshViewport(true)
	case streamDeltaMsg:
		if m.streamTurn >= 0 {
			m.turns[m.streamTurn].answer.WriteString(string(msg))
			m.turns[m.streamTurn].streamedText.WriteString(string(msg))
			m.refreshViewport(true)
			// Force visible-line computation so viewport scroll state is
			// fully settled before the next View() paint. Without this,
			// the cursor offset can briefly lag behind the grown content,
			// making the input box look like it jumped up.
			m.vp.VisibleLineCount()
		}
	case streamReasoningMsg:
		if m.streamTurn >= 0 {
			m.turns[m.streamTurn].reasoning.WriteString(string(msg))
			m.refreshViewport(true)
			// Force visible-line computation so viewport scroll state is
			// fully settled before the next View() paint. Without this,
			// the cursor offset can briefly lag behind the grown content,
			// making the input box look like it jumped up.
			m.vp.VisibleLineCount()
		}
	case engineLineMsg:
		switch {
		case m.streamTurn >= 0:
			m.turns[m.streamTurn].answer.WriteString(string(msg))
			m.refreshViewport(true)
			// Force visible-line computation so viewport scroll state is
			// fully settled before the next View() paint. Without this,
			// the cursor offset can briefly lag behind the grown content,
			// making the input box look like it jumped up.
			m.vp.VisibleLineCount()
		case m.curTurn >= 0:
			m.turns[m.curTurn].answer.WriteString(string(msg))
			m.refreshViewport(true)
			// Force visible-line computation so viewport scroll state is
			// fully settled before the next View() paint. Without this,
			// the cursor offset can briefly lag behind the grown content,
			// making the input box look like it jumped up.
			m.vp.VisibleLineCount()
		default:
			m.appendSystem(string(msg))
		}
	case streamEndMsg:
		m.streaming = false
		m.layout()
		turnIdx := m.streamTurn
		m.streamTurn = -1
		m.curTurn = -1
		if msg.final != "" && turnIdx >= 0 && turnIdx < len(m.turns) {
			streamed := m.turns[turnIdx].streamedText.String()
			if len(msg.final) > len(streamed) {
				missing := msg.final[len(streamed):]
				m.turns[turnIdx].answer.WriteString(missing)
			}
		}
		m.refreshViewport(true)
		// Force visible-line computation so viewport scroll state is
		// fully settled before the next View() paint. Without this,
		// the cursor offset can briefly lag behind the grown content,
		// making the input box look like it jumped up.
		m.vp.VisibleLineCount()
	case taskStateMsg:
		prev := m.transientVisible()
		m.task = TaskInfo(msg)
		if m.transientVisible() != prev {
			m.layout()
			m.refreshViewport(true)
			// Force visible-line computation so viewport scroll state is
			// fully settled before the next View() paint. Without this,
			// the cursor offset can briefly lag behind the grown content,
			// making the input box look like it jumped up.
			m.vp.VisibleLineCount()
		}
	case statusUpdateMsg:
		m.status = StatusInfo(msg)
		m.layout()
		m.refreshViewport(false)
	case historyMsg:
		m.history = []HistoryItem(msg)
	case permRequestMsg:
		// A tool needs interactive confirmation. Show the permission overlay and
		// stash the reply channel; the worker goroutine is blocked until the user
		// answers (see App.RequestPermission).
		m.overlay = overlayPermission
		m.permTool = msg.tool
		m.permDesc = msg.desc
		m.permReply = msg.reply
		m.ta.Blur()
	case copyDoneMsg:
		if msg.err != nil {
			m.copyNotice = "[复制] 复制失败: " + msg.err.Error()
		} else {
			m.copyNotice = "[复制] 已复制" + msg.label + "到系统剪贴板"
		}
		return m, nil

	case activityMsg:
		prev := m.transientVisible()
		m.activity = string(msg)
		if m.transientVisible() != prev {
			m.layout()
			m.refreshViewport(true)
			// Force visible-line computation so viewport scroll state is
			// fully settled before the next View() paint. Without this,
			// the cursor offset can briefly lag behind the grown content,
			// making the input box look like it jumped up.
			m.vp.VisibleLineCount()
		}
	}

	// Forward anything else to the focused input box.
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

func (m *Model) echoUser(input string) {
	m.turns = append(m.turns, &turn{user: input})
	m.curTurn = len(m.turns) - 1
	m.refreshViewport(true)
}

// handleQuitDialog handles key input while the quit confirmation is showing.
func (m *Model) handleQuitDialog(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc":
			m.showQuit = false
			m.ta.Focus()
			return m, nil
		case "left", "right", "tab":
			m.quitSelectedNo = !m.quitSelectedNo
			return m, nil
		case "enter", " ":
			if !m.quitSelectedNo {
				// Yes selected — quit
				m.quitting = true
				return m, tea.Quit
			}
			// No selected — dismiss
			m.showQuit = false
			m.ta.Focus()
			return m, nil
		case "y", "Y":
			m.quitting = true
			return m, tea.Quit
		case "n", "N":
			m.showQuit = false
			m.ta.Focus()
			return m, nil
		}
	}
	return m, nil
}

// openHistory opens the history search overlay and moves focus to its search box.
func (m *Model) openHistory() {
	m.overlay = overlayHistory
	m.overlayIdx = 0
	m.search.SetValue("")
	m.search.Placeholder = "搜索会话…"
	m.search.Focus()
	m.ta.Blur()
}

// openCommands opens the slash-command palette and focuses its search box.
func (m *Model) openCommands() {
	m.openCommandsWithQuery("")
}

func (m *Model) openCommandsWithQuery(q string) {
	m.overlay = overlayCommand
	m.overlayIdx = 0
	m.search.SetValue(q)
	m.search.Placeholder = "搜索命令…"
	m.search.Focus()
	m.ta.Blur()
}

func (m *Model) openHelp() {
	m.overlay = overlayHelp
	m.overlayIdx = 0
	m.search.SetValue("")
	m.search.Blur()
	m.ta.Blur()
}

// closeOverlay dismisses any modal and returns focus to the input box.
func (m *Model) closeOverlay() {
	m.overlay = overlayNone
	m.search.Blur()
	m.ta.Focus()
}

// overlayLen returns the number of entries in the active overlay's filtered list.
func (m *Model) overlayLen() int {
	if m.overlay == overlayCommand {
		return len(m.filteredCommands())
	}
	return len(m.filteredHistory())
}

// updateOverlay handles key input while a modal overlay is active.
func (m *Model) updateOverlay(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.overlay == overlayPermission {
		return m.updatePermission(msg)
	}
	if m.overlay == overlayHelp {
		switch msg.String() {
		case "esc", "enter", " ", "?":
			m.closeOverlay()
			return m, nil
		}
		return m, nil
	}
	switch msg.String() {
	case "esc", "ctrl+c":
		m.closeOverlay()
		return m, nil
	case "up":
		if m.overlayIdx > 0 {
			m.overlayIdx--
		}
		return m, nil
	case "down":
		if m.overlayIdx < m.overlayLen()-1 {
			m.overlayIdx++
		}
		return m, nil
	case "enter":
		if m.overlay == overlayHistory {
			q := strings.TrimSpace(m.search.Value())
			if n, err := strconv.Atoi(q); err == nil {
				items := m.filteredHistory()
				if n >= 1 && n <= len(items) {
					m.overlayIdx = n - 1
				}
			}
		}
		m.activateOverlaySelection()
		return m, nil
	}
	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	m.overlayIdx = 0
	return m, cmd
}

func (m *Model) resolvePermission(d PermDecision) {
	if m.permReply != nil {
		m.permReply <- d
		m.permReply = nil
	}
	m.permTool = ""
	m.permDesc = ""
	m.closeOverlay()
}

// updatePermission handles key input while the permission-confirmation overlay
// is active. It always sends exactly one decision back to the blocked worker
// goroutine and then dismisses the overlay. Esc/n reject; y/Enter allow once; a
// allows and asks the caller to remember the rule.
func (m *Model) updatePermission(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc":
		m.resolvePermission(PermDeny)
		return m, nil
	case "left", "right", "tab":
		m.permSelected = (m.permSelected + 1) % 3
		return m, nil
	case "enter", " ":
		switch m.permSelected {
		case 0:
			m.resolvePermission(PermAllow)
		case 1:
			m.resolvePermission(PermAlways)
		case 2:
			m.resolvePermission(PermDeny)
		}
		return m, nil
	}
	switch strings.ToLower(msg.Text) {
	case "y": // single allow
		m.resolvePermission(PermAllow)
	case "a": // allow always
		m.resolvePermission(PermAlways)
	case "n", "d": // deny
		m.resolvePermission(PermDeny)
	}
	return m, nil
}

// activateOverlaySelection acts on the currently highlighted overlay entry:
// commands are submitted through the normal queue, history entries are resumed.
func (m *Model) activateOverlaySelection() {
	if m.overlay == overlayCommand {
		items := m.filteredCommands()
		if m.overlayIdx >= 0 && m.overlayIdx < len(items) {
			full := "/" + items[m.overlayIdx].Name
			m.closeOverlay()
			m.echoUser(full)
			if m.onSubmit != nil {
				m.onSubmit(full)
			}
			return
		}
		m.closeOverlay()
		return
	}

	items := m.filteredHistory()
	if m.overlayIdx >= 0 && m.overlayIdx < len(items) && m.onResume != nil {
		m.onResume(items[m.overlayIdx].ID)
	}
	m.closeOverlay()
}

// filteredHistory returns history entries matching the overlay search query.
func (m *Model) filteredHistory() []HistoryItem {
	q := strings.ToLower(strings.TrimSpace(m.search.Value()))
	if q == "" {
		return m.history
	}
	if _, err := strconv.Atoi(q); err == nil {
		// Numeric query is used as direct index selection mode.
		return m.history
	}
	var out []HistoryItem
	for _, h := range m.history {
		if strings.Contains(strings.ToLower(h.Title), q) ||
			strings.Contains(strings.ToLower(h.Subtitle), q) {
			out = append(out, h)
		}
	}
	return out
}

// filteredCommands returns command entries matching the palette search query.
func (m *Model) filteredCommands() []CommandItem {
	q := strings.ToLower(strings.TrimSpace(m.search.Value()))
	if q == "" {
		return m.commands
	}
	var out []CommandItem
	for _, c := range m.commands {
		if strings.Contains(strings.ToLower(c.Name), q) ||
			strings.Contains(strings.ToLower(c.Desc), q) {
			out = append(out, c)
		}
	}
	return out
}

// tabCompleteInput restores classic REPL-style slash-command completion in the
// main TUI input box. It completes the command token before the first space.
// - single match: fill full command (adds a trailing space if no args yet)
// - multi match with longer common prefix: extend current token
// - no further extension: open command palette filtered by current token
func (m *Model) tabCompleteInput() bool {
	line := strings.TrimRight(m.ta.Value(), "\r\n")
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, "/") {
		return false
	}

	leading := line[:len(line)-len(trimmed)]
	body := trimmed[1:]
	cmdPart := body
	argPart := ""
	if i := strings.IndexAny(body, " \t"); i >= 0 {
		cmdPart = body[:i]
		argPart = body[i:]
	}
	if cmdPart == "" {
		m.openCommandsWithQuery("")
		return true
	}

	matches := make([]CommandItem, 0, len(m.commands))
	needle := strings.ToLower(cmdPart)
	for _, c := range m.commands {
		if strings.HasPrefix(strings.ToLower(c.Name), needle) {
			matches = append(matches, c)
		}
	}
	if len(matches) == 0 {
		m.openCommandsWithQuery(cmdPart)
		return true
	}

	if len(matches) == 1 {
		completed := "/" + matches[0].Name + argPart
		if strings.TrimSpace(argPart) == "" {
			completed += " "
		}
		m.ta.SetValue(leading + completed)
		return true
	}

	prefix := commonPrefixCommandNames(matches)
	if len(prefix) > len(cmdPart) {
		m.ta.SetValue(leading + "/" + prefix + argPart)
		return true
	}

	m.openCommandsWithQuery(cmdPart)
	return true
}

// slashCommandHint returns a short inline hint for slash-command completion in
// the main input box (for example while typing "/h").
func (m *Model) slashCommandHint(maxItems int) string {
	line := strings.TrimRight(m.ta.Value(), "\r\n")
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, "/") {
		return ""
	}
	body := strings.TrimPrefix(trimmed, "/")
	cmdPart := body
	if i := strings.IndexAny(body, " \t"); i >= 0 {
		cmdPart = body[:i]
	}
	if cmdPart == "" {
		return "补全: 输入命令名后按 Tab 自动补全"
	}

	needle := strings.ToLower(cmdPart)
	matches := make([]string, 0, len(m.commands))
	for _, c := range m.commands {
		if strings.HasPrefix(strings.ToLower(c.Name), needle) {
			matches = append(matches, "/"+c.Name)
		}
	}
	if len(matches) == 0 {
		return "补全: 无匹配，按 Tab 打开命令列表"
	}
	if len(matches) == 1 {
		return "补全: " + matches[0] + "  (Tab 确认)"
	}
	if maxItems < 1 {
		maxItems = 1
	}
	shown := matches
	if len(shown) > maxItems {
		shown = shown[:maxItems]
	}
	hint := "补全: " + strings.Join(shown, "  ")
	if len(matches) > len(shown) {
		hint += fmt.Sprintf("  +%d", len(matches)-len(shown))
	}
	hint += "  (Tab 自动补全)"
	return hint
}

func commonPrefixCommandNames(items []CommandItem) string {
	if len(items) == 0 {
		return ""
	}
	p := items[0].Name
	for _, it := range items[1:] {
		for !strings.HasPrefix(it.Name, p) {
			if p == "" {
				return ""
			}
			p = p[:len(p)-1]
		}
	}
	return p
}

func isTabKey(msg tea.KeyPressMsg) bool {
	if msg.String() == "tab" || msg.String() == "ctrl+i" {
		return true
	}
	if msg.Text == "\t" {
		return true
	}
	return msg.Code == tea.KeyTab
}

func (m *Model) gitPanelHeight() int {
	status := strings.TrimSpace(m.status.GitStatus)
	if status == "" || status == "(clean)" {
		return 0
	}
	if !m.gitExpanded {
		return 1 // folding line
	}
	lines := strings.Split(status, "\n")
	numFiles := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			numFiles++
		}
	}
	if numFiles == 0 {
		return 0
	}
	return numFiles + 1
}

// layout recomputes child component sizes from the current terminal size.
//
// Layout philosophy: the conversation is the body and spans the full width.
// Only thin chrome surrounds it — a top status bar, an optional one-line
// transient zone (tool/queue activity), a horizontal rule + input, and a bottom
// status line. No sidebars or nested frames.
func (m *Model) layout() {
	const statusH = 1    // top status bar
	const bottomH = 1    // bottom status line
	const inputH = 2     // horizontal rule + one input line
	const transientH = 1 // activity line is always reserved to keep layout stable
	gitH := m.gitPanelHeight()
	midH := m.height - statusH - transientH - bottomH - inputH - gitH
	if midH < 3 {
		midH = 3
	}

	// main content area has 1 column of horizontal padding on each side.
	vw := m.width - 2
	if vw < 1 {
		vw = 1
	}
	m.vp.SetWidth(vw)
	vh := midH
	if vh < 1 {
		vh = 1
	}
	m.vp.SetHeight(vh)

	m.ta.SetWidth(m.width - 4)
	m.ta.SetHeight(1)
}

// transientVisible reports whether the one-line transient zone should show.
func (m *Model) transientVisible() bool {
	return m.task.Running || m.activity != "" || len(m.task.Queued) > 0
}

// View implements tea.Model. In Bubble Tea v2 it returns a tea.View, which
// declares terminal features (alt-screen) and the real cursor position per
// frame. Positioning the cursor ourselves is what makes a CJK IME draw its
// preedit (pinyin) at the focused input — including inside overlays — instead
// of wherever the renderer last parked it.
func (m *Model) View() tea.View {
	if !m.ready {
		return tea.NewView("正在初始化界面…")
	}
	if m.quitting {
		return tea.NewView("")
	}

	statusBar := m.renderStatusBar()
	hr := dimStyle.Render(strings.Repeat("─", max(m.width, 1)))
	// The transient line is always reserved (blank when idle) so running a
	// command never resizes the conversation body or shifts the input box.
	transient := m.renderTransient()
	bottomBar := m.renderBottomBar()

	var v tea.View
	// OpenCode-aligned fullscreen rendering path.
	v.AltScreen = useAltScreen()
	// Enable wheel/click reporting for reliable in-app scrolling across terminals.
	v.MouseMode = m.currentMouseMode()

	if m.showQuit {
		mid := m.renderCenteredOverlay(m.renderQuitDialog(), m.vp.Height())
		v.Content = lipgloss.JoinVertical(lipgloss.Left, statusBar, mid, transient, bottomBar, hr, m.ta.View())
		return v
	}

	if m.overlay != overlayNone {
		oH := m.height - 5 // status + transient + bottom + rule + input
		if oH < 3 {
			oH = 3
		}
		var mid string
		if m.overlay == overlayPermission {
			mid = m.renderCenteredOverlay(m.renderPermission(oH), oH)
		} else if m.overlay == overlayHelp {
			mid = m.renderCenteredOverlay(m.renderHelp(oH), oH)
		} else {
			mid = m.renderCenteredOverlay(m.renderOverlay(oH), oH)
		}
		v.Content = lipgloss.JoinVertical(lipgloss.Left, statusBar, mid, transient, bottomBar, hr, m.ta.View())
		v.Cursor = m.overlayCursor()
		return v
	}

	// Width must be the full terminal width here: mainAreaStyle adds 1 column of
	// horizontal padding on each side, so the inner content area becomes
	// m.width-2 == m.vp.Width(). Passing m.vp.Width() instead would shrink the
	// inner area by another 2 columns and re-wrap full-width lines, growing the
	// body past m.vp.Height() and pushing the input box around as content
	// changes. MaxHeight clamps any residual overflow (e.g. wide glyphs).
	main := mainAreaStyle.Width(m.width).Height(m.vp.Height()).MaxHeight(m.vp.Height()).Render(m.vp.View())

	gitPanel := m.renderGitPanel()
	if gitPanel != "" {
		v.Content = lipgloss.JoinVertical(lipgloss.Left, statusBar, gitPanel, main, transient, bottomBar, hr, m.ta.View())
	} else {
		v.Content = lipgloss.JoinVertical(lipgloss.Left, statusBar, main, transient, bottomBar, hr, m.ta.View())
	}
	v.Cursor = m.inputCursor()
	return v
}

func (m *Model) renderCenteredOverlay(overlay string, areaH int) string {
	centered := lipgloss.NewStyle().Width(m.width).Align(lipgloss.Center).Render(overlay)
	oh := lipgloss.Height(centered)
	if oh >= areaH {
		return centered
	}
	top := (areaH - oh) / 2
	bottom := areaH - oh - top
	return strings.Repeat("\n", top) + centered + strings.Repeat("\n", bottom)
}

// inputCursor returns the real cursor position for the main input box. The
// textarea is the last line of the frame; its own cursor position is offset by
// the chrome rendered above it (status bar + body + transient + bottom + rule).
func (m *Model) inputCursor() *tea.Cursor {
	c := m.ta.Cursor()
	if c == nil {
		return nil
	}
	gitH := m.gitPanelHeight()
	c.Y += m.vp.Height() + 4 + gitH
	return c
}

// overlayCursor returns the real cursor position for the active overlay's search
// box. The permission overlay has no text input, so the cursor is hidden. The
// search input sits at frame line 3 (status bar, box top border, title, search)
// and column 2 (box left border + left padding).
func (m *Model) overlayCursor() *tea.Cursor {
	if m.overlay == overlayPermission {
		return nil
	}
	c := m.search.Cursor()
	if c == nil {
		return nil
	}
	c.X += 2
	c.Y += 3
	return c
}

func useAltScreen() bool {
	v := strings.TrimSpace(os.Getenv("COVE_TUI_ALTSCREEN"))
	if v != "" {
		return v != "0"
	}
	// Copy-friendly default: keep normal screen buffer so users can rely on
	// native terminal selection behavior.
	return false
}

// currentMouseMode returns the mouse mode for the current frame.
func (m *Model) currentMouseMode() tea.MouseMode {
	if m.mouseCapture {
		return tea.MouseModeCellMotion
	}
	return tea.MouseModeNone
}

func defaultMouseCapture() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("COVE_TUI_MOUSE")))
	if v == "1" || v == "on" || v == "cell" || v == "cellmotion" {
		return true
	}
	if v == "0" || v == "off" || v == "none" {
		return false
	}
	// Default to wheel-friendly mode: capture mouse events.
	return true
}

var ansiEscapeRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func stripANSI(s string) string {
	return ansiEscapeRE.ReplaceAllString(s, "")
}

func (m *Model) exportVisibleScreenText() string {
	view := stripANSI(m.vp.View())
	return strings.TrimSpace(view)
}

func (m *Model) exportSessionText() string {
	var b strings.Builder
	for _, t := range m.turns {
		if t.system {
			continue
		}
		if u := strings.TrimSpace(t.user); u != "" {
			b.WriteString("[USER]\n")
			b.WriteString(u)
			b.WriteString("\n\n")
		}
		if a := strings.TrimSpace(t.answer.String()); a != "" {
			b.WriteString("[ASSISTANT]\n")
			b.WriteString(a)
			b.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}

func (m *Model) exportTranscriptText() string {
	var b strings.Builder
	for _, t := range m.turns {
		if t.system {
			s := strings.TrimSpace(t.answer.String())
			if s != "" {
				b.WriteString("[SYSTEM]\n")
				b.WriteString(s)
				b.WriteString("\n\n")
			}
			continue
		}
		if u := strings.TrimSpace(t.user); u != "" {
			b.WriteString("[USER]\n")
			b.WriteString(u)
			b.WriteString("\n\n")
		}
		if r := strings.TrimSpace(t.reasoning.String()); r != "" {
			b.WriteString("[REASONING]\n")
			b.WriteString(r)
			b.WriteString("\n\n")
		}
		if a := strings.TrimSpace(t.answer.String()); a != "" {
			b.WriteString("[ASSISTANT]\n")
			b.WriteString(a)
			b.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// copyCmd copies text to the clipboard OFF the UI goroutine so a slow clipboard
// subprocess (e.g. PowerShell on Windows) never freezes rendering. The result
// is delivered back to Update via copyDoneMsg.
func copyCmd(text, label string) tea.Cmd {
	return func() tea.Msg {
		return copyDoneMsg{label: label, err: copyToClipboard(text)}
	}
}

func copyToClipboard(text string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("empty content")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// clip.exe may mangle non-ASCII text depending on console code page.
		// Use PowerShell Set-Clipboard with UTF-8 stdin to preserve CJK text.
		cmd = exec.Command(
			"powershell",
			"-NoProfile",
			"-NonInteractive",
			"-Command",
			"[Console]::InputEncoding=[System.Text.Encoding]::UTF8; $t=[Console]::In.ReadToEnd(); Set-Clipboard -Value $t",
		)
	case "darwin":
		cmd = exec.Command("pbcopy")
	default:
		if _, err := exec.LookPath("wl-copy"); err == nil {
			cmd = exec.Command("wl-copy")
		} else if _, err := exec.LookPath("xclip"); err == nil {
			cmd = exec.Command("xclip", "-selection", "clipboard")
		} else if _, err := exec.LookPath("xsel"); err == nil {
			cmd = exec.Command("xsel", "--clipboard", "--input")
		} else {
			return fmt.Errorf("clipboard tool not found")
		}
	}
	cmd.Stdin = strings.NewReader(text)
	if out, err := cmd.CombinedOutput(); err != nil {
		if runtime.GOOS == "windows" {
			// Fallback for environments where PowerShell is unavailable.
			fallback := exec.Command("cmd", "/c", "clip")
			fallback.Stdin = strings.NewReader(text)
			if fbOut, fbErr := fallback.CombinedOutput(); fbErr == nil {
				return nil
			} else {
				return fmt.Errorf("%w: %s; fallback: %s", err, strings.TrimSpace(string(out)), strings.TrimSpace(string(fbOut)))
			}
		}
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
