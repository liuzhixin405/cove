package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

var (
	accent = lipgloss.Color("14")  // ANSI Bright Cyan (Maps exactly to \x1b[96m, which is the exact color of the Cove Logo)
	subtle = lipgloss.Color("240") // grey
	good   = lipgloss.Color("70")  // green
	warn   = lipgloss.Color("214") // amber

	dimStyle  = lipgloss.NewStyle().Foreground(subtle)
	userStyle = lipgloss.NewStyle().Foreground(accent).Bold(true)
	warnStyle = lipgloss.NewStyle().Foreground(warn).Bold(true)
	// thinkHeaderStyle marks the clickable fold/unfold line for a turn's thinking.
	thinkHeaderStyle = lipgloss.NewStyle().Foreground(subtle).Italic(true)

	mainAreaStyle = lipgloss.NewStyle().Padding(0, 1)

	bottomBarStyle = lipgloss.NewStyle().Foreground(subtle)

	activityStyle = lipgloss.NewStyle().Foreground(accent)

	overlayBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(accent).
			Padding(0, 1)

	overlayTitleStyle = lipgloss.NewStyle().Foreground(accent).Bold(true)

	selectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Background(accent)

	statusBarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("232")). // Muted dark text for superb contrast on light cyan bar
			Background(accent).                // Soft Cyan palette (matching BrightCyan logo)
			Bold(true)

	btnAllowStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("231")).
			Background(good).
			Bold(true)

	btnDenyStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("231")).
			Background(lipgloss.Color("196")). // Red background
			Bold(true)

	btnAlwaysStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("231")).
			Background(warn).
			Bold(true)
)

func (m *Model) renderStatusBar() string {
	model := m.status.Model
	if model == "" {
		model = "?"
	}

	// Create centered content
	ver := m.status.Version
	if ver == "" {
		ver = "5.0.0" // fallback
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

	state := "就绪"
	if m.task.Running {
		state = "运行中 ⚡"
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
	right := "Ctrl+R 历史 · / 命令 · Ctrl+C 退出 "

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
		left = "⚙ 处理中…"
	}
	if m.task.Elapsed != "" && (m.activity != "" || m.task.Running) {
		left += "  " + m.task.Elapsed
	}

	right := ""
	if len(m.task.Queued) > 0 {
		right = fmt.Sprintf("+%d 排队", len(m.task.Queued))
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
	innerW := m.width - 4
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
		title = "命令面板"
		hint = "↑/↓ 选择 · Enter 执行 · Esc 关闭"
		for _, c := range m.filteredCommands() {
			label := "/" + c.Name
			if c.Desc != "" {
				label += " — " + c.Desc
			}
			labels = append(labels, label)
		}
	} else {
		title = "历史会话"
		hint = "↑/↓ 选择 · Enter 恢复 · Esc 关闭"
		for _, h := range m.filteredHistory() {
			t := h.Title
			if t == "" {
				t = "(未命名)"
			}
			labels = append(labels, t)
		}
	}

	b.WriteString(overlayTitleStyle.Render(title) + "\n")
	b.WriteString(m.search.View() + "\n\n")

	rowsShown := 0
	if len(labels) == 0 {
		b.WriteString(dimStyle.Render("（无匹配项）") + "\n")
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
	return overlayBoxStyle.Width(m.width - 2).MaxHeight(height).Render(content)
}

// renderPermission renders the interactive permission-confirmation overlay. The
// row region is padded to a constant height (matching renderOverlay) so the box
// is always exactly `height` lines and never shifts the input box.
func (m *Model) renderPermission(height int) string {
	innerW := m.width - 4
	if innerW < 4 {
		innerW = 4
	}
	maxRows := height - 6
	if maxRows < 1 {
		maxRows = 1
	}

	var b strings.Builder
	b.WriteString(overlayTitleStyle.Render("权限确认") + "\n\n")

	tool := m.permTool
	if tool == "" {
		tool = "?"
	}
	rows := []string{
		"工具 " + warnStyle.Render(tool) + " 请求执行：",
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

	allowBtn := btnAllowStyle.Render(" 允许 (y) ")
	denyBtn := btnDenyStyle.Render(" 拒绝 (n) ")
	alwaysBtn := btnAlwaysStyle.Render(" 始终允许 (a) ")
	buttonsLine := allowBtn + "  " + denyBtn + "  " + alwaysBtn + "  " + dimStyle.Render("(或按 Esc/n 拒绝)")

	content := b.String() + "\n" + buttonsLine
	return overlayBoxStyle.Width(m.width - 2).MaxHeight(height).Render(content)
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

	w := m.width
	branchInfo := ""
	if m.status.Git != "" {
		// Strip possible trailing dirty star to get clean branch name
		branchInfo = " [" + strings.TrimSuffix(m.status.Git, "*") + "]"
	}

	if !m.gitExpanded {
		text := fmt.Sprintf("  ▸ 工作区%s有 %d 个文件变更 (按 Ctrl+G 或点击此处展开变动详情)", branchInfo, len(files))
		return lipgloss.NewStyle().Foreground(warn).Bold(true).Width(w).Render(text)
	}

	var sb strings.Builder
	header := fmt.Sprintf("  ▾ 工作区%s变动文件列表 (共 %d 个，按 Ctrl+G 或点击此处折叠隐藏)：", branchInfo, len(files))
	sb.WriteString(lipgloss.NewStyle().Foreground(warn).Bold(true).Render(header) + "\n")

	for _, f := range files {
		sb.WriteString("    " + lipgloss.NewStyle().Foreground(subtle).Render(f) + "\n")
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
