package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/cost"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/repl"
	"github.com/liuzhixin405/cove/internal/session"
)

func handleExport(input string, eng *engine.Engine) {
	filename := "conversation.md"
	parts := strings.Fields(input)
	if len(parts) > 1 {
		filename = parts[1]
	}
	var sb strings.Builder
	sb.WriteString("# 对话导出\r\n\r\n")
	for _, m := range eng.Messages() {
		sb.WriteString(fmt.Sprintf("**%s**: %s\r\n\r\n", m.Role, m.Content))
		for _, tc := range m.ToolCalls {
			sb.WriteString(fmt.Sprintf("  > 工具: %s(%v)\r\n\r\n", tc.Name, tc.Input))
		}
	}
	if err := os.WriteFile(filename, []byte(sb.String()), 0644); err != nil {
		repl.PrintSafe("导出失败: %v\n", err)
		return
	}
	repl.PrintSafe("已导出 %d 条消息到 %s\n", len(eng.Messages()), filename)
}

func handleResume(ctx context.Context, sessionID string, eng *engine.Engine) {
	store := eng.Store()
	if store == nil {
		repl.PrintSafe("会话存储不可用\n")
		return
	}
	if sessionID == "" {
		records, _ := store.List()
		if len(records) == 0 {
			repl.PrintSafe("没有已保存的会话\n")
			return
		}
		repl.PrintSafe("%d 个已保存的会话:\n", len(records))
		for _, r := range records {
			repl.PrintSafe("  %s  %s  (%d tokens)  %s\n", r.ID, r.Title, r.TokensIn+r.TokensOut, r.UpdatedAt.Format("15:04"))
		}
		return
	}
	r, err := store.Load(sessionID)
	if err != nil {
		repl.PrintSafe("会话 %s 未找到\n", sessionID)
		return
	}
	eng.LoadMessages(r.Messages)
	repl.PrintSafe("已恢复: %s (%d 条消息, %d tokens)\n", r.Title, len(r.Messages), r.TokensIn+r.TokensOut)
}

func autoSaveSession(eng *engine.Engine) {
	if eng.HasMessages() {
		eng.SaveSession()
		ch := cost.NewCostHistory()
		sessionID := ""
		model := ""
		if s := eng.Session(); s != nil {
			sessionID = s.ID
			model = s.Model
		}
		ch.Add(sessionID, model, eng.CostTracker())
		ch.Save()
		fmt.Println("会话已自动保存。")
	}
}

type interruptedDraft struct {
	UpdatedAt   time.Time `json:"updated_at"`
	Title       string    `json:"title"`
	UserContent string    `json:"user_content"`
	Error       string    `json:"error"`
}

func interruptedDraftPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cove", "interrupted.json"), nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func saveInterruptedDraft(msg api.Message, reqErr error) error {
	p, err := interruptedDraftPath()
	if err != nil {
		return err
	}
	d := interruptedDraft{
		UpdatedAt:   time.Now(),
		Title:       shortDesc(msg.Content),
		UserContent: strings.TrimSpace(msg.Content),
	}
	if d.Title == "" {
		d.Title = "(未命名中断任务)"
	}
	if reqErr != nil {
		d.Error = reqErr.Error()
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p, raw, 0600)
}

func loadInterruptedDraft() (*interruptedDraft, error) {
	p, err := interruptedDraftPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var d interruptedDraft
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	if strings.TrimSpace(d.UserContent) == "" {
		return nil, fmt.Errorf("empty draft")
	}
	return &d, nil
}

func clearInterruptedDraft() error {
	p, err := interruptedDraftPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err != nil {
		return nil
	}
	return os.Remove(p)
}

func handleHistory(eng *engine.Engine) {
	store := eng.Store()
	if store == nil {
		repl.PrintSafe("会话存储不可用\n")
		return
	}
	records, _ := store.List()
	draft, _ := loadInterruptedDraft()
	if len(records) == 0 && draft == nil {
		repl.PrintSafe("暂无历史。退出时会自动保存会话。\n")
		return
	}
	repl.PrintSafe("\n  历史记录 (%d 个会话):\n\n", len(records))
	if draft != nil {
		repl.PrintSafe("  ⚠ 中断草稿 [%s] %s\n", draft.UpdatedAt.Format("01-02 15:04"), shortDesc(draft.Title))
	}
	limit := 20
	if len(records) < limit {
		limit = len(records)
	}
	for i, r := range records[:limit] {
		msgCount := r.MessageCount
		if msgCount == 0 && len(r.Messages) > 0 {
			msgCount = len(r.Messages)
		}
		toolCount := r.ToolMessages
		if toolCount == 0 && len(r.Messages) > 0 {
			toolCount = countToolMessages(r.Messages)
		}
		turns := r.UserTurns
		if turns == 0 && len(r.Messages) > 0 {
			turns = countUserTurns(r.Messages)
		}
		date := r.UpdatedAt.Format("01-02 15:04")
		title := r.Title
		if title == "New session" || title == "" || isLowSignalHistoryTitle(title) {
			title = sessionPreview(r)
		}
		if len(title) > 50 {
			title = title[:50] + "..."
		}
		repl.PrintSafe("  %2d. [%s] %s  (%d 轮 / %d 工具 / %d 消息)\n", i+1, date, title, turns, toolCount, msgCount)
	}
	if len(records) > limit {
		repl.PrintSafe("\n  ... 还有 %d 条。\n", len(records)-limit)
	}
	repl.PrintSafe("\n  继续会话: /history <编号>  (例如 /history 1)\n")
	repl.PrintSafe("  或直接输入编号: 1 / 2 / 3 ...\n")
	repl.PrintSafe("  查看详情: /history detail <编号>\n\n")
	if draft != nil {
		repl.PrintSafe("  中断详情: /history detail interrupted\n\n")
	}
}

func sessionPreview(r session.Record) string {
	if r.Preview != "" {
		return r.Preview
	}
	fallback := ""
	for _, m := range r.Messages {
		if m.Role == "user" && m.Content != "" {
			content := strings.ReplaceAll(m.Content, "\n", " ")
			if len(content) > 50 {
				content = content[:50] + "..."
			}
			if !isLowSignalHistoryTitle(content) {
				return content
			}
			if fallback == "" {
				fallback = content
			}
		}
	}
	if fallback != "" {
		return fallback
	}
	return "(空)"
}

func handleHistoryResume(input string, eng *engine.Engine) {
	store := eng.Store()
	if store == nil {
		repl.PrintSafe("会话存储不可用\n")
		return
	}

	records, _ := store.List()
	var idx int
	if _, err := fmt.Sscanf(input, "%d", &idx); err == nil && idx >= 1 && idx <= len(records) {
		rMeta := records[idx-1]
		r, err := store.Load(rMeta.ID)
		if err != nil {
			repl.PrintSafe("恢复失败: %v\n", err)
			return
		}
		eng.LoadMessages(r.Messages)
		title := r.Title
		if title == "New session" || title == "" {
			title = sessionPreview(*r)
		}
		repl.PrintSafe("已恢复 #%d: %s (%d 条消息)\n", idx, title, len(r.Messages))
		repl.PrintSafe("可以继续对话了。\n\n")
		return
	}

	r, err := store.Load(input)
	if err != nil {
		repl.PrintSafe("无效选择: %s\n输入 /history 查看可用会话。\n", input)
		return
	}
	eng.LoadMessages(r.Messages)
	title := r.Title
	if title == "New session" || title == "" || isLowSignalHistoryTitle(title) {
		title = sessionPreview(*r)
	}
	repl.PrintSafe("已恢复: %s (%d 条消息)\n", title, len(r.Messages))
	repl.PrintSafe("可以继续对话了。\n\n")
}

func handleHistoryDetail(input string, eng *engine.Engine) {
	if strings.TrimSpace(input) == "" {
		repl.PrintSafe("用法: /history detail <编号|session-id>\n")
		return
	}
	if strings.EqualFold(strings.TrimSpace(input), "interrupted") {
		draft, _ := loadInterruptedDraft()
		if draft == nil {
			repl.PrintSafe("当前没有中断草稿。\n")
			return
		}
		repl.PrintSafe("\n  中断草稿详情\n")
		repl.PrintSafe("  更新时间: %s\n", draft.UpdatedAt.Format("2006-01-02 15:04:05"))
		repl.PrintSafe("  标题: %s\n", draft.Title)
		repl.PrintSafe("  错误: %s\n\n", shortDesc(draft.Error))
		repl.PrintSafe("  用户输入:\n")
		repl.PrintSafe("  %s\n\n", draft.UserContent)
		return
	}
	store := eng.Store()
	if store == nil {
		repl.PrintSafe("会话存储不可用\n")
		return
	}

	resolve := func(sel string) (*session.Record, error) {
		records, _ := store.List()
		var idx int
		if _, err := fmt.Sscanf(sel, "%d", &idx); err == nil && idx >= 1 && idx <= len(records) {
			return store.Load(records[idx-1].ID)
		}
		return store.Load(sel)
	}

	r, err := resolve(strings.TrimSpace(input))
	if err != nil {
		repl.PrintSafe("无效选择: %s\n输入 /history 查看可用会话。\n", input)
		return
	}

	title := r.Title
	if title == "" || title == "New session" || isLowSignalHistoryTitle(title) {
		title = sessionPreview(*r)
	}

	repl.PrintSafe("\n  会话详情\n")
	repl.PrintSafe("  ID: %s\n", r.ID)
	repl.PrintSafe("  标题: %s\n", title)
	repl.PrintSafe("  更新时间: %s\n", r.UpdatedAt.Format("2006-01-02 15:04:05"))
	repl.PrintSafe("  用户轮次: %d\n", countUserTurns(r.Messages))
	repl.PrintSafe("  工具消息: %d\n", countToolMessages(r.Messages))
	repl.PrintSafe("  总消息数: %d\n\n", len(r.Messages))

	if len(r.Messages) == 0 {
		repl.PrintSafe("  该会话暂无消息。\n\n")
		return
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

	repl.PrintSafe("  消息预览:\n")
	for i, idx := range indices {
		if total > window && i == 3 {
			repl.PrintSafe("    ...\n")
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
		repl.PrintSafe("  [%03d] %-9s %s\n", idx+1, role, shortDesc(content))
	}
	repl.PrintSafe("\n")
}

func handleHistoryResumeMostRelevant(eng *engine.Engine) bool {
	store := eng.Store()
	if store == nil {
		repl.PrintSafe("会话存储不可用\n")
		return false
	}
	records, _ := store.List()
	if len(records) == 0 {
		repl.PrintSafe("暂无历史。\n")
		return false
	}

	type candidate struct {
		rec   *session.Record
		idx   int
		score int
	}

	best := candidate{score: -1}
	for i, meta := range records {
		rec, err := store.Load(meta.ID)
		if err != nil {
			continue
		}
		s := scoreSessionForResume(*rec)
		if s > best.score {
			best = candidate{rec: rec, idx: i + 1, score: s}
		}
		if i >= 30 {
			break
		}
	}

	if best.rec == nil {
		handleHistoryResume("1", eng)
		return true
	}

	eng.LoadMessages(best.rec.Messages)
	title := best.rec.Title
	if title == "New session" || title == "" || isLowSignalHistoryTitle(title) {
		title = sessionPreview(*best.rec)
	}
	userTurns := countUserTurns(best.rec.Messages)
	repl.PrintSafe("已自动恢复最近有效任务 #%d: %s (%d 轮对话 / %d 工具 / %d 消息)\n", best.idx, title, userTurns, countToolMessages(best.rec.Messages), len(best.rec.Messages))
	return true
}

// countUserTurns reports how many *genuine* user-authored turns a session
// contains, which is a far more meaningful number to the user than the raw
// message count (which also includes assistant replies, tool-result messages,
// and engine-injected synthetic prompts).
//
// The engine stores several non-user entries under Role=="user" — e.g. the
// truncation-continuation nudge and the circuit-breaker hint, both prefixed
// with "[system:". Those must be excluded or the "轮对话" figure looks wrong.
func countUserTurns(msgs []api.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(m.Content), "[system:") {
			continue
		}
		n++
	}
	return n
}

func countToolMessages(msgs []api.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "tool" {
			n++
		}
	}
	return n
}

// resumeAndContinue loads the most relevant past session AND then actually
// drives the agent forward by enqueuing a real "继续" turn. Previously the
// resume step only reloaded messages into context and stopped, so typing
// "继续" appeared to do nothing — the model was never invoked.
func resumeAndContinue(eng *engine.Engine, tasks *replTaskRunner) {
	if !handleHistoryResumeMostRelevant(eng) {
		return
	}
	if tasks == nil {
		return
	}
	repl.PrintSafe("正在继续该任务…\n\n")
	_, _ = tasks.Enqueue(api.Message{Role: "user", Content: "继续"})
	// Don't block the main loop — the task runs in the background.
}

func scoreSessionForResume(r session.Record) int {
	if len(r.Messages) == 0 {
		return -100
	}
	score := 0
	if len(r.Messages) >= 6 {
		score += 4
	} else {
		score += len(r.Messages)
	}

	userText := ""
	toolCount := 0
	assistantCount := 0
	for _, m := range r.Messages {
		if userText == "" && m.Role == "user" {
			userText = strings.TrimSpace(m.Content)
		}
		if m.Role == "tool" {
			toolCount++
		}
		if m.Role == "assistant" {
			assistantCount++
		}
	}

	if toolCount > 0 {
		score += 4
	}
	if assistantCount > 1 {
		score += 2
	}

	if isTrivialResumePrompt(userText) {
		score -= 6
	} else {
		score += 3
		if strings.Contains(userText, "http") || strings.Contains(userText, "https") {
			score += 2
		}
		if len([]rune(userText)) >= 20 {
			score += 1
		}
	}

	return score
}

func isTrivialResumePrompt(s string) bool {
	v := strings.TrimSpace(strings.ToLower(s))
	if v == "" {
		return true
	}
	trivial := map[string]bool{
		"继续": true, "continue": true,
		"hi": true, "hello": true, "你好": true,
		"你": true, "我": true, "嗯": true, "好的": true,
		"1": true, "2": true, "3": true, "4": true, "?": true,
	}
	if trivial[v] {
		return true
	}
	if strings.HasPrefix(v, "继续") {
		return true
	}
	if strings.HasPrefix(v, "/history") || strings.HasPrefix(v, "/resume") {
		return true
	}
	return false
}

func isLowSignalHistoryTitle(s string) bool {
	v := strings.TrimSpace(strings.ToLower(s))
	if v == "" {
		return true
	}
	if len([]rune(v)) <= 2 {
		return true
	}
	noise := map[string]bool{
		"write":        true,
		"write a file": true,
		"read":         true,
		"read file":    true,
		"grep":         true,
		"继续":           true,
		"continue":     true,
		"hi":           true,
		"hello":        true,
		"你好":           true,
		"?":            true,
	}
	if noise[v] {
		return true
	}
	if strings.HasPrefix(v, "/") {
		return true
	}
	return false
}

func isLowSignalResumeInput(s string) bool {
	v := strings.TrimSpace(s)
	if v == "" {
		return true
	}
	if len([]rune(v)) <= 1 {
		return true
	}
	return isTrivialResumePrompt(v)
}
