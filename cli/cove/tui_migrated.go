package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/liuzhixin405/cove/internal/command"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/plugin"
	"github.com/liuzhixin405/cove/internal/session"
)

// This file holds TUI-side variants of behaviors that used to live only in the
// classic line REPL. They return rendered strings (instead of printing to
// stdout via repl.PrintSafe) so the caller can surface them through
// App.EngineLine while the alternate screen is active. Removing the REPL must
// not drop any of these features.

// tuiIsSkillInvocation reports whether input is a bare "/<skillname>" that maps
// to an installed skill (not /skill, /skills, or a registered command).
func tuiIsSkillInvocation(input string, eng *engine.Engine) bool {
	if eng == nil || eng.Runtime() == nil {
		return false
	}
	parts := strings.Fields(input)
	if len(parts) == 0 || !strings.HasPrefix(parts[0], "/") {
		return false
	}
	name := strings.TrimPrefix(parts[0], "/")
	if name == "" || name == "skill" || name == "skills" {
		return false
	}
	_, ok := eng.Runtime().SkillPrompts[name]
	return ok
}

// tuiSkillInvocationText renders an installed skill's prompt for display,
// mirroring the classic REPL's handleSkillInvocation.
func tuiSkillInvocationText(input string, eng *engine.Engine) string {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return ""
	}
	name := strings.TrimPrefix(parts[0], "/")
	prompt, ok := eng.Runtime().SkillPrompts[name]
	if !ok {
		return fmt.Sprintf("未找到配置文件: %s", name)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[Skill: %s]\n\n%s\n", name, prompt))
	if args := strings.TrimSpace(strings.TrimPrefix(input, parts[0])); args != "" {
		sb.WriteString(fmt.Sprintf("\n无效的参数: %s\n", args))
	}
	return sb.String()
}

// tuiPluginCommandPrompt resolves a "/<plugincmd> [args]" into the plugin
// command's prompt body (with any trailing args appended). ok is false when the
// command does not match an enabled plugin command. The caller feeds the
// returned prompt to the engine as a normal user turn.
func tuiPluginCommandPrompt(input string, pluginMgr *plugin.Manager) (prompt string, label string, ok bool) {
	if pluginMgr == nil {
		return "", "", false
	}
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return "", "", false
	}
	name := strings.TrimPrefix(parts[0], "/")
	cmd, found := pluginMgr.CommandPrompts()[name]
	if !found {
		return "", "", false
	}
	p := cmd.Prompt
	if args := strings.TrimSpace(strings.TrimPrefix(input, parts[0])); args != "" {
		p = p + "\n\n" + args
	}
	return p, fmt.Sprintf("%s (%s)", name, cmd.Plugin), true
}

// tuiUnknownCmdText builds the "unknown command" message with fuzzy suggestions,
// mirroring the classic REPL's handleUnknownCmd but returning a string.
func tuiUnknownCmdText(input string, cmdReg *command.Registry) string {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return ""
	}
	name := strings.TrimPrefix(parts[0], "/")
	if cmdReg != nil {
		if suggestions := fuzzyMatch(name, cmdReg); len(suggestions) > 0 {
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("未知命令: /%s\n你是不是想输入?\n", name))
			for _, s := range suggestions {
				sb.WriteString(fmt.Sprintf("  /%s\n", s))
			}
			return sb.String()
		}
	}
	return fmt.Sprintf("未知命令: /%s。输入 /help 查看可用命令。", name)
}

// tuiExportText exports the current conversation to a markdown file and returns
// a status string, mirroring the classic REPL's handleExport.
func tuiExportText(input string, eng *engine.Engine) string {
	filename := "conversation.md"
	parts := strings.Fields(input)
	if len(parts) > 1 {
		filename = parts[1]
	}
	var sb strings.Builder
	sb.WriteString("# 对话导出\n\n")
	for _, m := range eng.Messages() {
		sb.WriteString(fmt.Sprintf("**%s**: %s\n\n", m.Role, m.Content))
		for _, tc := range m.ToolCalls {
			sb.WriteString(fmt.Sprintf("  > 工具: %s(%v)\n\n", tc.Name, tc.Input))
		}
	}
	if err := os.WriteFile(filename, []byte(sb.String()), 0644); err != nil {
		return fmt.Sprintf("导出失败: %v", err)
	}
	return fmt.Sprintf("已导出 %d 条消息到 %s", len(eng.Messages()), filename)
}

// tuiResumeText loads a saved session by id (or lists saved sessions when id is
// empty) and returns a status string, mirroring the classic REPL's handleResume.
func tuiResumeText(sessionID string, eng *engine.Engine) string {
	store := eng.Store()
	if store == nil {
		return "会话存储不可用"
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		records, _ := store.List()
		if len(records) == 0 {
			return "没有已保存的会话"
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("%d 个已保存的会话:\n", len(records)))
		for _, r := range records {
			sb.WriteString(fmt.Sprintf("  %s  %s  (%d tokens)  %s\n", r.ID, r.Title, r.TokensIn+r.TokensOut, r.UpdatedAt.Format("15:04")))
		}
		return sb.String()
	}
	r, err := store.Load(sessionID)
	if err != nil {
		return fmt.Sprintf("会话 %s 未找到", sessionID)
	}
	eng.LoadMessages(r.Messages)
	return fmt.Sprintf("已恢复: %s (%d 条消息, %d tokens)", r.Title, len(r.Messages), r.TokensIn+r.TokensOut)
}

// tuiHistoryDetailText renders details for a saved session (or the interrupted
// draft), mirroring the classic REPL's handleHistoryDetail but returning a
// string.
func tuiHistoryDetailText(input string, eng *engine.Engine) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return "用法: /history detail <编号|session-id>"
	}
	if strings.EqualFold(input, "interrupted") {
		draft, _ := loadInterruptedDraft()
		if draft == nil {
			return "当前没有中断草稿。"
		}
		var sb strings.Builder
		sb.WriteString("\n  中断草稿详情\n")
		sb.WriteString(fmt.Sprintf("  更新时间: %s\n", draft.UpdatedAt.Format("2006-01-02 15:04:05")))
		sb.WriteString(fmt.Sprintf("  标题: %s\n", draft.Title))
		sb.WriteString(fmt.Sprintf("  错误: %s\n\n", shortDesc(draft.Error)))
		sb.WriteString("  用户输入:\n")
		sb.WriteString(fmt.Sprintf("  %s\n", draft.UserContent))
		return sb.String()
	}
	store := eng.Store()
	if store == nil {
		return "会话存储不可用"
	}
	resolve := func(sel string) (*session.Record, error) {
		records := listHistoryRecords(store)
		var idx int
		if _, err := fmt.Sscanf(sel, "%d", &idx); err == nil && idx >= 1 && idx <= len(records) {
			return store.Load(records[idx-1].ID)
		}
		return store.Load(sel)
	}
	r, err := resolve(input)
	if err != nil {
		return fmt.Sprintf("无效选择: %s\n输入 /history 查看可用会话。", input)
	}
	title := effectiveHistoryTitle(*r)
	var sb strings.Builder
	sb.WriteString("\n  会话详情\n")
	sb.WriteString(fmt.Sprintf("  ID: %s\n", r.ID))
	sb.WriteString(fmt.Sprintf("  标题: %s\n", title))
	sb.WriteString(fmt.Sprintf("  更新时间: %s\n", r.UpdatedAt.Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("  消息数: %d\n\n", len(r.Messages)))
	if len(r.Messages) == 0 {
		sb.WriteString("  该会话暂无消息。\n")
		return sb.String()
	}
	const window = 6
	total := len(r.Messages)
	indices := make([]int, 0, window)
	if total <= window {
		for i := 0; i < total; i++ {
			indices = append(indices, i)
		}
	} else {
		indices = append(indices, 0, 1, 2, total-3, total-2, total-1)
	}
	sb.WriteString("  消息预览:\n")
	for i, idx := range indices {
		if total > window && i == 3 {
			sb.WriteString("    ...\n")
		}
		m := r.Messages[idx]
		role := strings.ToUpper(strings.TrimSpace(m.Role))
		if role == "" {
			role = "UNKNOWN"
		}
		content := m.Content
		if strings.TrimSpace(content) == "" && len(m.Parts) > 0 {
			content = fmt.Sprintf("[%d part(s)]", len(m.Parts))
		}
		if strings.TrimSpace(content) == "" {
			content = "(空)"
		}
		sb.WriteString(fmt.Sprintf("  [%03d] %-9s %s\n", idx+1, role, shortDesc(content)))
	}
	return sb.String()
}
