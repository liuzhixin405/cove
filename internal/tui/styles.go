package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/liuzhixin405/cove/internal/tui/theme"
)

// Package-level styles are applied with theme colors at init time.
// applyTheme() is called whenever the theme changes to rebuild these.
var (
	dimStyle          lipgloss.Style
	userStyle         lipgloss.Style
	warnStyle         lipgloss.Style
	thinkHeaderStyle  lipgloss.Style
	mainAreaStyle     = lipgloss.NewStyle().Padding(0, 1)
	bottomBarStyle    lipgloss.Style
	activityStyle     lipgloss.Style
	overlayBoxStyle   lipgloss.Style
	overlayTitleStyle lipgloss.Style
	selectedStyle     lipgloss.Style
	statusBarStyle    lipgloss.Style
	// btnAllowStyle/btnDenyStyle/btnAlwaysStyle removed — buttons are
	// rendered inline with theme colors in renderPermission().
)

// applyTheme rebuilds all color-dependent styles with the current theme.
func applyTheme() {
	t := theme.Current()
	accent := lipgloss.Color(t.Primary)
	subtle := lipgloss.Color(t.TextMuted)
	warn := lipgloss.Color(t.Warning)

	dimStyle = lipgloss.NewStyle().Foreground(subtle)
	userStyle = lipgloss.NewStyle().Foreground(accent).Bold(true)
	warnStyle = lipgloss.NewStyle().Foreground(warn).Bold(true)
	thinkHeaderStyle = lipgloss.NewStyle().Foreground(subtle).Italic(true)
	bottomBarStyle = lipgloss.NewStyle().Foreground(subtle)
	activityStyle = lipgloss.NewStyle().Foreground(accent)

	overlayBoxStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accent).
		Padding(0, 1)
	overlayTitleStyle = lipgloss.NewStyle().Foreground(accent).Bold(true)
	selectedStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color(t.SelectedFG)).
		Background(lipgloss.Color(t.SelectedBG))
	statusBarStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color(t.SelectedFG)).
		Background(accent).
		Bold(true)

}

func init() {
	applyTheme()
}

func (m *Model) renderStatusBar() string {
	model := m.status.Model
	if model == "" {
		model = "?"
	}

	// Create centered content
	ver := m.status.Version
	if ver == "" {
		ver = "6.2.1" // fallback
	}

	centerParts := []string{"cove v" + ver, model}
	if m.status.Provider != "" {
		centerParts = append(centerParts, m.status.Provider)
	}
	if m.status.Git != "" {
		centerParts = append(centerParts, m.status.Git)
	}
	if m.status.PermMode != "" {
		centerParts = append(centerParts, "⏵ "+m.status.PermMode)
	}

	centerText := " " + strings.Join(centerParts, " · ") + " "

	state := "Ready"
	if m.task.Running {
		state = "Busy"
	}
	right := " " + state + " "

	w := m.width

	// Determine how much space is left
	contentLen := lipgloss.Width(centerText)
	rightLen := lipgloss.Width(right)

	var bar string
	if w <= contentLen+rightLen {
		// Terminal too narrow, just render centerText
		bar = centerText
		if len(bar) > w {
			bar = bar[:w]
		}
	} else {
		// Center alignment calculation
		// We want centerText to be exactly centered in total width w.
		// Total left padding for centerText = (w - contentLen) / 2.
		// Then we place 'right' at the far right.
		leftPad := (w - contentLen) / 2
		if leftPad < 0 {
			leftPad = 0
		}

		rightStart := w - rightLen
		if leftPad+contentLen > rightStart {
			// Offset if overlap with right state
			leftPad = rightStart - contentLen
			if leftPad < 0 {
				leftPad = 0
			}
		}

		leftSpaces := strings.Repeat(" ", leftPad)
		middleAndLeft := leftSpaces + centerText
		rightPad := w - len(middleAndLeft) - rightLen
		if rightPad < 0 {
			rightPad = 0
		}
		bar = middleAndLeft + strings.Repeat(" ", rightPad) + right
	}

	return statusBarStyle.Width(w).Render(bar)
}

// renderBottomBar shows usage figures and a few compact hints. Keeping the hints
// here (rather than in the input placeholder) avoids redundant prompts.
func (m *Model) renderBottomBar() string {
	tokens := m.status.TokensIn + m.status.TokensOut
	left := fmt.Sprintf(" %s tokens", humanCount(tokens))
	if m.status.Budget > 0 {
		left += fmt.Sprintf(" · $%.2f / $%.2f", m.status.Cost, m.status.Budget)
	} else if m.status.Cost > 0 {
		left += fmt.Sprintf(" · $%.4f", m.status.Cost)
	}
	if m.status.Elapsed != "" {
		left += " · " + m.status.Elapsed
	}
	right := "Ctrl+S Sessions · Ctrl+K Commands · ? Help · Esc Cancel · Ctrl+C Quit "

	w := m.width
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return bottomBarStyle.Width(w).Render(left)
	}
	return bottomBarStyle.Width(w).Render(left + strings.Repeat(" ", gap) + right)
}

// humanCount renders large token counts compactly (1234 -> 1.2k).
func humanCount(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// renderTransient renders the one-line activity/queue zone shown above the
// input while work is in flight. The line is always reserved by the layout, so
// when idle this returns a blank line (keeps the body height/input position
// stable instead of resizing on every command).
func (m *Model) renderTransient() string {
	left := ""
	switch {
	case m.activity != "":
		left = "⚙ " + m.activity
	case m.task.Running:
		left = "⚙ Running..."
	}
	if m.task.Elapsed != "" && (m.activity != "" || m.task.Running) {
		left += "  " + m.task.Elapsed
	}

	right := ""
	if len(m.task.Queued) > 0 {
		right = fmt.Sprintf("+%d queued", len(m.task.Queued))
	}

	w := m.width
	body := truncate(left, w-lipgloss.Width(right)-2)
	gap := w - lipgloss.Width(body) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return activityStyle.Render(body) + strings.Repeat(" ", gap) + dimStyle.Render(right)
}

// renderOverlay renders the modal panel (history search or command palette)
// over the conversation body.
func (m *Model) renderOverlay(height int) string {
	var b strings.Builder
	boxW := m.width - 2
	if boxW > 88 {
		boxW = 88
	}
	if boxW < 28 {
		boxW = 28
	}
	innerW := boxW - 2
	if innerW < 4 {
		innerW = 4
	}
	maxRows := height - 7
	if maxRows < 1 {
		maxRows = 1
	}

	var title, hint string
	var labels []string
	if m.overlay == overlayCommand {
		title = "Commands"
		hint = "Up/Down navigate · Enter run · Esc close"
		for _, c := range m.filteredCommands() {
			label := "/" + c.Name
			if c.Desc != "" {
				label += " — " + c.Desc
			}
			labels = append(labels, label)
		}
	} else {
		title = "Sessions"
		hint = "Up/Down navigate · Number+Enter resume · Esc close"
		for i, h := range m.filteredHistory() {
			t := h.Title
			if t == "" {
				t = "(untitled)"
			}
			labels = append(labels, fmt.Sprintf("%2d. %s", i+1, t))
		}
	}

	b.WriteString(overlayTitleStyle.Render(title) + "\n")
	b.WriteString(m.search.View() + "\n\n")

	rowsShown := 0
	if len(labels) == 0 {
		b.WriteString(dimStyle.Render("(no matches)") + "\n")
		rowsShown = 1
	} else {
		start := 0
		if m.overlayIdx >= maxRows {
			start = m.overlayIdx - maxRows + 1
		}
		for i := start; i < len(labels) && i < start+maxRows; i++ {
			line := truncate(labels[i], innerW-2)
			if i == m.overlayIdx {
				b.WriteString(selectedStyle.Render("▸ "+line) + "\n")
			} else {
				b.WriteString("  " + line + "\n")
			}
			rowsShown++
		}
	}
	// Pad the row region to a constant height. Without this the overlay box
	// (and therefore the whole frame) shrinks as the filtered result count
	// drops, which shifts the input box up and down.
	for ; rowsShown < maxRows; rowsShown++ {
		b.WriteString("\n")
	}

	content := b.String() + "\n" + dimStyle.Render(hint)
	return overlayBoxStyle.Width(boxW).MaxHeight(height).Render(content)
}

// renderPermission renders the interactive permission-confirmation overlay. The
// row region is padded to a constant height (matching renderOverlay) so the box
// is always exactly `height` lines and never shifts the input box.
func (m *Model) renderQuitDialog() string {
	t := theme.Current()
	w := m.width - 4
	if w > 64 {
		w = 64
	}
	if w < 20 {
		w = 20
	}

	question := "Quit cove?"
	yesStr := " Yes (Y) "
	noStr := " No (N) "

	yesStyle := lipgloss.NewStyle().Padding(0, 2)
	noStyle := lipgloss.NewStyle().Padding(0, 2)

	if m.quitSelectedNo {
		noStyle = noStyle.
			Background(lipgloss.Color(t.Primary)).
			Foreground(lipgloss.Color(t.SelectedFG)).
			Bold(true)
		yesStyle = yesStyle.
			Foreground(lipgloss.Color(t.TextMuted))
	} else {
		yesStyle = yesStyle.
			Background(lipgloss.Color(t.Primary)).
			Foreground(lipgloss.Color(t.SelectedFG)).
			Bold(true)
		noStyle = noStyle.
			Foreground(lipgloss.Color(t.TextMuted))
	}

	yesBtn := yesStyle.Render(yesStr)
	noBtn := noStyle.Render(noStr)
	buttons := lipgloss.JoinHorizontal(lipgloss.Left, yesBtn, "  ", noBtn)

	content := lipgloss.JoinVertical(lipgloss.Center,
		question,
		"",
		buttons,
	)

	return overlayBoxStyle.
		Width(w).
		Render(content)
}

func (m *Model) renderPermission(height int) string {
	t := theme.Current()
	boxW := m.width - 2
	if boxW > 88 {
		boxW = 88
	}
	if boxW < 28 {
		boxW = 28
	}
	innerW := boxW - 2
	if innerW < 4 {
		innerW = 4
	}
	maxRows := height - 6
	if maxRows < 1 {
		maxRows = 1
	}

	var b strings.Builder
	b.WriteString(overlayTitleStyle.Render("Permission") + "\n\n")

	tool := m.permTool
	if tool == "" {
		tool = "?"
	}
	rows := []string{
		"Tool " + warnStyle.Render(tool) + " requests:",
	}
	if d := strings.TrimSpace(m.permDesc); d != "" {
		rows = append(rows, "  "+truncate(d, innerW-2))
	}

	rowsShown := 0
	for _, r := range rows {
		if rowsShown >= maxRows {
			break
		}
		b.WriteString(r + "\n")
		rowsShown++
	}
	for ; rowsShown < maxRows; rowsShown++ {
		b.WriteString("\n")
	}

	// Style buttons based on selected option (like opencode)
	primary := lipgloss.Color(t.Primary)
	selectedFG := lipgloss.Color(t.SelectedFG)
	muted := lipgloss.Color(t.TextMuted)
	errColor := lipgloss.Color(t.Error)

	baseBtn := lipgloss.NewStyle().Padding(0, 1)
	allowStyle := baseBtn
	sessionStyle := baseBtn
	denyStyle := baseBtn

	switch m.permSelected {
	case 0:
		allowStyle = allowStyle.Background(primary).Foreground(selectedFG).Bold(true)
		sessionStyle = sessionStyle.Foreground(muted)
		denyStyle = denyStyle.Foreground(muted)
	case 1:
		allowStyle = allowStyle.Foreground(muted)
		sessionStyle = sessionStyle.Background(primary).Foreground(selectedFG).Bold(true)
		denyStyle = denyStyle.Foreground(muted)
	case 2:
		allowStyle = allowStyle.Foreground(muted)
		sessionStyle = sessionStyle.Foreground(muted)
		denyStyle = denyStyle.Background(errColor).Foreground(selectedFG).Bold(true)
	}

	allowBtn := allowStyle.Render(" Allow ")
	sessionBtn := sessionStyle.Render(" Always ")
	denyBtn := denyStyle.Render(" Deny ")
	buttonsLine := allowBtn + "  " + sessionBtn + "  " + denyBtn + "  " + dimStyle.Render("Left/Right switch · Enter confirm")

	content := b.String() + "\n" + buttonsLine
	return overlayBoxStyle.Width(boxW).MaxHeight(height).Render(content)
}

func (m *Model) renderHelp(height int) string {
	boxW := m.width - 2
	if boxW > 76 {
		boxW = 76
	}
	if boxW < 28 {
		boxW = 28
	}
	var b strings.Builder
	b.WriteString(overlayTitleStyle.Render("Help") + "\n\n")
	b.WriteString("  Session / Commands\n")
	b.WriteString("    Ctrl+S   Sessions\n")
	b.WriteString("    Ctrl+K   Commands\n")
	b.WriteString("    Ctrl+O   Model\n")
	b.WriteString("    Ctrl+F   Attachments\n")
	b.WriteString("    Ctrl+L   Tasks/Logs\n")
	b.WriteString("\n")
	b.WriteString("  Chat\n")
	b.WriteString("    Enter    Send message\n")
	b.WriteString("    \\ + Enter New line\n")
	b.WriteString("    PgUp/Dn  Scroll\n")
	b.WriteString("    Alt+T    Toggle reasoning\n")
	b.WriteString("\n")
	b.WriteString("  System\n")
	b.WriteString("    Ctrl+T   Theme\n")
	b.WriteString("    Esc      Cancel running task\n")
	b.WriteString("    Ctrl+C   Quit confirm\n")
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("Esc / Enter / ? close help"))
	return overlayBoxStyle.Width(boxW).MaxHeight(height).Render(b.String())
}

func (m *Model) renderGitPanel() string {
	status := strings.TrimSpace(m.status.GitStatus)
	if status == "" || status == "(clean)" {
		return ""
	}

	lines := strings.Split(status, "\n")
	var files []string
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed != "" {
			files = append(files, trimmed)
		}
	}
	if len(files) == 0 {
		return ""
	}

	t := theme.Current()
	w := m.width
	branchInfo := ""
	if m.status.Git != "" {
		// Strip possible trailing dirty star to get clean branch name
		branchInfo = " [" + strings.TrimSuffix(m.status.Git, "*") + "]"
	}

	warnFG := lipgloss.Color(t.Warning)
	subtleFG := lipgloss.Color(t.TextMuted)

	if !m.gitExpanded {
		text := fmt.Sprintf("  ▸ Workspace%s has %d changed files (Ctrl+G to expand)", branchInfo, len(files))
		return lipgloss.NewStyle().Foreground(warnFG).Bold(true).Width(w).Render(text)
	}

	var sb strings.Builder
	header := fmt.Sprintf("  ▾ Workspace%s changed files (%d total, Ctrl+G to collapse):", branchInfo, len(files))
	sb.WriteString(lipgloss.NewStyle().Foreground(warnFG).Bold(true).Render(header) + "\n")

	for _, f := range files {
		sb.WriteString("    " + lipgloss.NewStyle().Foreground(subtleFG).Render(f) + "\n")
	}

	return strings.TrimSuffix(sb.String(), "\n")
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max < 1 {
		return ""
	}
	return string(r[:max-1]) + "…"
}
