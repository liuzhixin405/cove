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
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

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
	streamEndMsg       struct{}
	taskStateMsg       TaskInfo
	statusUpdateMsg    StatusInfo
	historyMsg         []HistoryItem
	activityMsg        string
	permRequestMsg     permRequest
)

// overlay modes for the modal palette (history search / command palette).
const (
	overlayNone = iota
	overlayHistory
	overlayCommand
	overlayPermission
)

// turn is one structured exchange in the conversation transcript. Keeping turns
// structured (rather than a flat text stream) is what lets each turn's thinking
// be folded independently and toggled by a mouse click.
//
// A turn is normally a user message plus the assistant's reasoning + answer.
// A system turn (system==true) carries standalone engine output (resume notices,
// command results) and has no foldable thinking.
type turn struct {
	user      string          // user input ("" for system/assistant-only turns)
	reasoning strings.Builder // streamed thinking; display/fold only
	answer    strings.Builder // streamed answer + interleaved engine/tool lines
	expanded  bool            // user clicked the thinking header open
	system    bool            // standalone engine output, not foldable
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
	// clickMap maps a wrapped viewport row to the turn index whose thinking
	// header sits on that row, so a mouse click can toggle the right fold.
	clickMap map[int]int

	status  StatusInfo
	task    TaskInfo
	history []HistoryItem

	// commands is the static slash-command catalog shown in the / palette.
	commands []CommandItem

	// activity is a short transient line shown just above the input while a
	// task/tool is running ("" hides the transient zone entirely).
	activity string

	gitExpanded bool

	// overlay is the modal layer drawn over the conversation body
	// (overlayNone when hidden). search/overlayIdx drive it.
	overlay    int
	search     textinput.Model
	overlayIdx int

	// permission-prompt overlay state. permReply is the channel the blocked
	// worker goroutine waits on; it is non-nil only while a prompt is showing.
	permTool  string
	permDesc  string
	permReply chan PermDecision

	onSubmit    func(string)
	onResume    func(string)
	onInterrupt func()
	quitting    bool
}

// New constructs a Model. onSubmit is invoked (on the UI goroutine) whenever the
// user submits a line from the input box. onResume is invoked with a session ID
// when the user picks an entry from the history overlay. onInterrupt is invoked
// when the user presses Ctrl+C while a task is running (to cancel it instead of
// quitting). commands is the static catalog shown in the / command palette.
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

	return &Model{
		ta:          ta,
		vp:          vp,
		search:      si,
		status:      StatusInfo{Model: modelName},
		commands:    commands,
		onSubmit:    onSubmit,
		onResume:    onResume,
		onInterrupt: onInterrupt,
		curTurn:     -1,
		streamTurn:  -1,
		gitExpanded: false,
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

// refreshViewport re-renders the structured transcript into the viewport and
// rebuilds clickMap (wrapped-row -> turn index for thinking headers). Each
// logical line is wrapped independently so the cumulative wrapped-row count
// stays accurate for hit-testing mouse clicks.
func (m *Model) refreshViewport(stick bool) {
	if !m.ready {
		return
	}
	w := m.vp.Width()
	if w < 1 {
		w = 1
	}
	wrap := lipgloss.NewStyle().Width(w)
	m.clickMap = make(map[int]int)

	var b strings.Builder
	row := 0
	write := func(s string) {
		r := wrap.Render(s)
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(r)
		row += lipgloss.Height(r)
	}

	for ti, t := range m.turns {
		if ti > 0 {
			write("") // blank line between turns
		}
		if t.system {
			write(strings.TrimRight(t.answer.String(), "\n"))
			continue
		}
		if t.user != "" {
			write(userStyle.Render("› " + t.user))
		}
		reasoning := strings.TrimRight(t.reasoning.String(), "\n")
		answer := strings.TrimRight(t.answer.String(), "\n")
		if reasoning != "" {
			// While streaming and before any answer has arrived, show the live
			// thinking. Once the answer appears (or the stream ends) it folds back
			// to a one-line header the user can click to re-open.
			live := ti == m.streamTurn && m.streaming && answer == ""
			expanded := t.expanded || live
			m.clickMap[row] = ti // header occupies this row
			if expanded {
				write(thinkHeaderStyle.Render("▾ 思考过程"))
				write(dimStyle.Render(reasoning))
				if answer != "" {
					write("") // blank line separating thinking from the answer
				}
			} else {
				write(thinkHeaderStyle.Render("▸ 思考过程（点击展开）"))
			}
		}
		if answer != "" {
			write(answer)
		}
	}

	m.vp.SetContent(b.String())
	if stick {
		m.vp.GotoBottom()
	}
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
		if m.overlay != overlayNone {
			return m.updateOverlay(msg)
		}
		switch msg.String() {
		case "ctrl+c":
			// While a task is running, Ctrl+C cancels it (mirrors the classic
			// REPL) instead of quitting. Press it again when idle to exit.
			if (m.task.Running || m.streaming) && m.onInterrupt != nil {
				m.onInterrupt()
				return m, nil
			}
			m.quitting = true
			return m, tea.Quit
		case "ctrl+r":
			m.openHistory()
			return m, nil
		case "ctrl+g":
			status := strings.TrimSpace(m.status.GitStatus)
			if status != "" && status != "(clean)" {
				m.gitExpanded = !m.gitExpanded
				m.layout()
				m.refreshViewport(true)
			}
			return m, nil
		case "/":
			// "/" on an empty input opens the command palette.
			if strings.TrimSpace(m.ta.Value()) == "" {
				m.openCommands()
				return m, nil
			}
		case "enter":
			input := strings.TrimRight(m.ta.Value(), "\r\n")
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
		case "pgup", "pgdown", "ctrl+u", "ctrl+d":
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		}

	case tea.MouseClickMsg:
		if m.overlay == overlayPermission && msg.Button == tea.MouseLeft {
			oH := m.height - 5
			if oH < 3 {
				oH = 3
			}
			if msg.Y == oH-1 {
				xOffset := 2
				wAllow := lipgloss.Width(" 允许 (y) ")
				wDeny := lipgloss.Width(" 拒绝 (n) ")
				wAlways := lipgloss.Width(" 始终允许 (a) ")

				allowStart := xOffset
				allowEnd := allowStart + wAllow

				denyStart := allowEnd + 2
				denyEnd := denyStart + wDeny

				alwaysStart := denyEnd + 2
				alwaysEnd := alwaysStart + wAlways

				if msg.X >= allowStart-1 && msg.X <= allowEnd+1 {
					m.resolvePermission(PermAllow)
					return m, nil
				} else if msg.X >= denyStart-1 && msg.X <= denyEnd+1 {
					m.resolvePermission(PermDeny)
					return m, nil
				} else if msg.X >= alwaysStart-1 && msg.X <= alwaysEnd+1 {
					m.resolvePermission(PermAlways)
					return m, nil
				}
			}
		}

		// A left click on a folded/expanded thinking header toggles it. The
		// viewport body starts at screen row 1 (row 0 is the status bar); add the
		// scroll offset to map the click to a wrapped content row.
		if m.overlay == overlayNone && msg.Button == tea.MouseLeft {
			gitH := m.gitPanelHeight()
			if gitH > 0 && msg.Y >= 1 && msg.Y <= gitH {
				m.gitExpanded = !m.gitExpanded
				m.layout()
				m.refreshViewport(true)
				return m, nil
			}
			contentRow := (msg.Y - 1 - gitH) + m.vp.YOffset()
			if ti, ok := m.clickMap[contentRow]; ok {
				m.turns[ti].expanded = !m.turns[ti].expanded
				m.refreshViewport(false)
			}
		}
		return m, nil

	case tea.MouseWheelMsg:
		// Scroll the conversation body with the mouse wheel. Overlays manage
		// their own selection, so the wheel only drives the main viewport.
		if m.overlay == overlayNone {
			switch msg.Button {
			case tea.MouseWheelUp:
				m.vp.ScrollUp(3)
			case tea.MouseWheelDown:
				m.vp.ScrollDown(3)
			}
		}
		return m, nil

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
			m.refreshViewport(true)
		}
	case streamReasoningMsg:
		if m.streamTurn >= 0 {
			m.turns[m.streamTurn].reasoning.WriteString(string(msg))
			m.refreshViewport(true)
		}
	case engineLineMsg:
		switch {
		case m.streamTurn >= 0:
			m.turns[m.streamTurn].answer.WriteString(string(msg))
			m.refreshViewport(true)
		case m.curTurn >= 0:
			m.turns[m.curTurn].answer.WriteString(string(msg))
			m.refreshViewport(true)
		default:
			m.appendSystem(string(msg))
		}
	case streamEndMsg:
		m.streaming = false
		m.streamTurn = -1
		m.curTurn = -1
		m.refreshViewport(true)
	case taskStateMsg:
		prev := m.transientVisible()
		m.task = TaskInfo(msg)
		if m.transientVisible() != prev {
			m.layout()
			m.refreshViewport(true)
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
	case activityMsg:
		prev := m.transientVisible()
		m.activity = string(msg)
		if m.transientVisible() != prev {
			m.layout()
			m.refreshViewport(true)
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
	m.overlay = overlayCommand
	m.overlayIdx = 0
	m.search.SetValue("")
	m.search.Placeholder = "搜索命令…"
	m.search.Focus()
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
	case "ctrl+c":
		m.resolvePermission(PermDeny)
		m.quitting = true
		return m, tea.Quit
	case "esc":
		m.resolvePermission(PermDeny)
		return m, nil
	case "enter":
		m.resolvePermission(PermAllow)
		return m, nil
	}
	switch strings.ToLower(msg.Text) {
	case "y":
		m.resolvePermission(PermAllow)
	case "n":
		m.resolvePermission(PermDeny)
	case "a":
		m.resolvePermission(PermAlways)
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
	v.AltScreen = true
	// Enable mouse so the conversation body responds to the scroll wheel
	// (handled as tea.MouseWheelMsg in Update). Cell-motion mode is the most
	// widely supported and still delivers wheel events.
	v.MouseMode = tea.MouseModeCellMotion

	if m.overlay != overlayNone {
		oH := m.height - 5 // status + transient + bottom + rule + input
		if oH < 3 {
			oH = 3
		}
		var mid string
		if m.overlay == overlayPermission {
			mid = m.renderPermission(oH)
		} else {
			mid = m.renderOverlay(oH)
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
