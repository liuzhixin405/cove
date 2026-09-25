package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/cost"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/session"
	"github.com/liuzhixin405/cove/internal/termui"
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
		fmt.Fprintf(&sb, "**%s**: %s\r\n\r\n", m.Role, m.Content)
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&sb, "  > 工具: %s(%v)\r\n\r\n", tc.Name, tc.Input)
		}
	}
	if err := os.WriteFile(filename, []byte(sb.String()), 0644); err != nil {
		termui.PrintSafe("导出失败: %v\n", err)
		return
	}
	termui.PrintSafe("已导出 %d 条消息到 %s\n", len(eng.Messages()), filename)
}

func handleResume(ctx context.Context, sessionID string, eng *engine.Engine) {
	store := eng.Store()
	if store == nil {
		termui.PrintSafe("会话存储不可用\n")
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	if all := strings.EqualFold(sessionID, "all"); sessionID == "" || all {
		records, _ := store.List()
		hidden := 0
		if !all {
			project := session.FilterByProject(records, currentProjectDir())
			hidden = len(records) - len(project)
			records = project
		}
		if len(records) == 0 {
			termui.PrintSafe("没有已保存的会话\n")
		} else {
			termui.PrintSafe("%d 个已保存的会话:\n", len(records))
			for _, r := range records {
				termui.PrintSafe("  %s  %s  (%d tokens)  %s\n", r.ID, r.Title, r.TokensIn+r.TokensOut, r.UpdatedAt.Format("15:04"))
			}
		}
		printHiddenSessionsHint(hidden, "/resume all")
		return
	}
	r, err := store.Load(sessionID)
	if err != nil {
		termui.PrintSafe("会话 %s 未找到\n", sessionID)
		return
	}
	// An explicit ID resumes whatever project it came from; the warning is
	// what tells the user the conversation is about another codebase.
	printProjectMismatchWarning(r)
	// ResumeSession, not LoadMessages: loading only the messages continued in
	// a fresh session, so every resume saved a copy under a new ID and the
	// original never grew.
	eng.ResumeSession(r)
	termui.PrintSafe("已恢复: %s (%d 条消息, %d tokens)\n", r.Title, len(r.Messages), r.TokensIn+r.TokensOut)
}

// finishSession is the exit work every front end owes: save the session,
// record its cost in the history /cost reads, and disconnect MCP servers so
// stdio servers can shut down cleanly (pool may be nil). It prints nothing,
// since the headless and -p paths keep stdout for answers.
//
// Only the TUI's /exit and Ctrl+D used to record cost (via autoSaveSession),
// so /cost's 24h and 7-day figures ignored every headless and -p run, and no
// path disconnected MCP servers. It also fires the SessionEnd hooks (once per
// process, after the save so a hook sees the saved session).
func finishSession(eng *engine.Engine, pool interface{ DisconnectAll() }) {
	if eng != nil && eng.HasMessages() {
		eng.SaveSession()
		ch := cost.NewCostHistory()
		sessionID := ""
		model := ""
		if s := eng.Session(); s != nil {
			sessionID = s.ID
			model = s.Model
		}
		ch.Add(sessionID, model, eng.CostTracker())
		if err := ch.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "费用记录保存失败: %v\n", err)
		}
	}
	waitExitBackground(eng)
	fireSessionEnd(eng)
	if pool != nil {
		pool.DisconnectAll()
	}
}

// exitBackgroundWait bounds how long an exit waits for the last turn's
// memory extraction; -p has already waited longer (printModeBackgroundWait).
// A variable so tests can shorten it.
var exitBackgroundWait = 10 * time.Second

// pendingWaiter is the engine's BackgroundPending plus WaitBackground.
type pendingWaiter interface {
	backgroundWaiter
	BackgroundPending() bool
}

// waitExitBackground lets the last turn's memory extraction finish before
// the process exits (bounded by exitBackgroundWait): the interactive exit
// used to kill it, so the last turn was never learned. The stderr line is
// printed only when there is something to wait for.
func waitExitBackground(eng *engine.Engine) {
	if eng == nil {
		return
	}
	waitPending(eng, exitBackgroundWait)
}

// waitPending is waitExitBackground for any pendingWaiter.
func waitPending(w pendingWaiter, limit time.Duration) {
	if !w.BackgroundPending() {
		return
	}
	fmt.Fprintln(os.Stderr, "正在保存本轮记忆…")
	waitForBackground(w, limit)
}

func autoSaveSession(eng *engine.Engine) {
	if eng.HasMessages() {
		finishSession(eng, nil)
		outln("会话已自动保存。")
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
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
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

// historyPickAll records whether the last /history listing was the
// all-projects view. The REPL loops resolve a bare number typed after a
// listing through handleHistoryResume, which must index the list the user
// just saw: otherwise "3" after /history all resumes the project list's #3.
var historyPickAll bool

// currentProjectDir is the directory the per-project history views filter
// on. The engine records a new session's directory with os.Getwd too, so the
// two always agree.
func currentProjectDir() string {
	dir, _ := os.Getwd()
	return dir
}

// printHiddenSessionsHint says where the other projects' (and legacy)
// sessions went, so the per-project default never looks like lost history.
func printHiddenSessionsHint(hidden int, allCmd string) {
	if hidden > 0 {
		termui.PrintSafe("\n  另有 %d 个其他项目或旧版本的会话未显示，使用 %s 查看全部。\n", hidden, allCmd)
	}
}

func printProjectMismatchWarning(r *session.Record) {
	if w := session.ProjectMismatchWarning(r, currentProjectDir()); w != "" {
		termui.PrintSafe("%s%s%s", termui.Yellow, w, termui.Reset)
	}
}

// historyProjectLabel names the project a session belongs to in the
// all-projects view, where rows from different directories sit side by side.
func historyProjectLabel(r session.Record) string {
	if r.Cwd == "" {
		return "旧版会话，未记录目录"
	}
	return filepath.Base(r.Cwd)
}

func handleHistory(eng *engine.Engine, all bool) {
	store := eng.Store()
	if store == nil {
		termui.PrintSafe("会话存储不可用\n")
		return
	}
	historyPickAll = all
	records, hidden := listHistoryRecords(store, currentProjectDir(), all)
	draft, _ := loadInterruptedDraft()
	if len(records) == 0 && draft == nil {
		termui.PrintSafe("当前项目暂无历史。退出时会自动保存会话。\n")
		printHiddenSessionsHint(hidden, "/history all")
		return
	}
	if all {
		termui.PrintSafe("\n  历史记录 (所有项目, %d 个会话):\n\n", len(records))
	} else {
		termui.PrintSafe("\n  历史记录 (当前项目, %d 个会话):\n\n", len(records))
	}
	if draft != nil {
		termui.PrintSafe("  ⚠ 中断草稿 [%s] %s\n", draft.UpdatedAt.Format("01-02 15:04"), shortDesc(draft.Title))
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
		turns := r.UserTurns
		if turns == 0 && len(r.Messages) > 0 {
			turns = countUserTurns(r.Messages)
		}
		date := r.UpdatedAt.Format("01-02 15:04")
		title := effectiveHistoryTitle(r)
		if title == "" {
			title = r.UpdatedAt.Format("01-02 15:04")
		}
		// compactRunes, not title[:50]: these titles are mostly Chinese, and a
		// byte slice cut one mid-rune so the history list showed mojibake.
		title = compactRunes(title, 50)
		if all {
			termui.PrintSafe("  %2d. [%s] %s  (%d 轮 / %d 条)  <%s>\n", i+1, date, title, turns, msgCount, historyProjectLabel(r))
		} else {
			termui.PrintSafe("  %2d. [%s] %s  (%d 轮 / %d 条)\n", i+1, date, title, turns, msgCount)
		}
	}
	if len(records) > limit {
		termui.PrintSafe("\n  ... 还有 %d 条。\n", len(records)-limit)
	}
	printHiddenSessionsHint(hidden, "/history all")
	if all {
		termui.PrintSafe("\n  继续会话: /history all <编号>  (例如 /history all 1)\n")
		termui.PrintSafe("  或直接输入编号: 1 / 2 / 3 ...\n")
		termui.PrintSafe("  查看详情: /history all detail <编号>\n\n")
	} else {
		termui.PrintSafe("\n  继续会话: /history <编号>  (例如 /history 1)\n")
		termui.PrintSafe("  或直接输入编号: 1 / 2 / 3 ...\n")
		termui.PrintSafe("  查看详情: /history detail <编号>\n")
		termui.PrintSafe("  所有项目: /history all\n\n")
	}
	termui.PrintSafe("  清洗历史: /history clean\n\n")
	if draft != nil {
		termui.PrintSafe("  中断详情: /history detail interrupted\n\n")
	}
}

type historyCleanStats struct {
	Scanned       int
	Modified      int
	ParseFailed   int
	BackupFailed  int
	WriteFailed   int
	TitlesFixed   int
	SyntheticFlag int
}

func sessionsDirPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cove", "sessions"), nil
}

func handleHistoryClean() {
	dir, err := sessionsDirPath()
	if err != nil {
		termui.PrintSafe("历史清洗失败: %v\n", err)
		return
	}
	historyCleanIn(dir)
}

// historyCleanIn repairs the sessions in dir and prints what it did. Session
// files are enumerated by the session package (<id>.jsonl and legacy
// <id>.json); index.json and backups are not sessions. JSONL sessions are
// rewritten through the store, which keeps the index in step and leaves
// UpdatedAt alone so cleaning does not reorder the history list.
func historyCleanIn(dir string) historyCleanStats {
	stats := historyCleanStats{}
	names, err := session.ListSessionFiles(dir)
	if err != nil {
		termui.PrintSafe("历史清洗失败: %v\n", err)
		return stats
	}
	store := session.NewStoreAt(dir)
	stamp := time.Now().Format("20060102-150405")

	for _, name := range names {
		stats.Scanned++
		path := filepath.Join(dir, name)
		jsonl := filepath.Ext(name) == ".jsonl"

		raw, err := os.ReadFile(path)
		if err != nil {
			stats.ParseFailed++
			continue
		}

		var rec *session.Record
		if jsonl {
			rec, err = store.Load(session.SessionIDFromFile(name))
		} else {
			rec = &session.Record{}
			err = json.Unmarshal(raw, rec)
		}
		if err != nil {
			stats.ParseFailed++
			continue
		}

		changed := false

		// Repair older sessions where injected user prompts were not marked as synthetic.
		for i := range rec.Messages {
			m := &rec.Messages[i]
			if m.Role == "user" && !m.Synthetic && looksSyntheticHistoryText(m.Content) {
				m.Synthetic = true
				stats.SyntheticFlag++
				changed = true
			}
		}

		oldTitle := strings.TrimSpace(rec.Title)
		if oldTitle == "New session" || oldTitle == "" || looksSyntheticHistoryText(oldTitle) || isLowSignalResumeInput(oldTitle) {
			newTitle := deriveCleanTitle(rec.Messages)
			if newTitle != "" && newTitle != rec.Title {
				rec.Title = newTitle
				stats.TitlesFixed++
				changed = true
			}
		}

		if !changed {
			continue
		}

		backupPath := path + ".bak." + stamp
		if err := os.WriteFile(backupPath, raw, 0600); err != nil {
			stats.BackupFailed++
			continue
		}

		if jsonl {
			if err := store.Replace(rec); err != nil {
				stats.WriteFailed++
				continue
			}
		} else {
			newRaw, err := json.MarshalIndent(rec, "", "  ")
			if err != nil {
				stats.WriteFailed++
				continue
			}
			if err := writeFileAtomic(path, newRaw, 0600); err != nil {
				stats.WriteFailed++
				continue
			}
		}
		stats.Modified++
	}

	termui.PrintSafe("历史清洗完成。\n")
	termui.PrintSafe("  扫描文件: %d\n", stats.Scanned)
	termui.PrintSafe("  修改文件: %d\n", stats.Modified)
	termui.PrintSafe("  标题修复: %d\n", stats.TitlesFixed)
	termui.PrintSafe("  Synthetic修复: %d\n", stats.SyntheticFlag)
	termui.PrintSafe("  解析失败: %d\n", stats.ParseFailed)
	termui.PrintSafe("  备份失败: %d\n", stats.BackupFailed)
	termui.PrintSafe("  写回失败: %d\n", stats.WriteFailed)
	termui.PrintSafe("  备份后缀: .bak.%s\n", stamp)
	return stats
}

func deriveCleanTitle(msgs []api.Message) string {
	for _, m := range msgs {
		if m.Role != "user" || m.Synthetic {
			continue
		}
		text := strings.TrimSpace(m.Content)
		if text == "" || looksSyntheticHistoryText(text) || isLowSignalResumeInput(text) {
			continue
		}
		return compactRunes(strings.ReplaceAll(text, "\n", " "), 60)
	}

	// Fallback: choose the longest non-synthetic user text if all are low-signal.
	type cand struct {
		text string
		len  int
	}
	var cands []cand
	for _, m := range msgs {
		if m.Role != "user" || m.Synthetic {
			continue
		}
		text := strings.TrimSpace(m.Content)
		if text == "" || looksSyntheticHistoryText(text) {
			continue
		}
		cands = append(cands, cand{text: text, len: len([]rune(text))})
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].len > cands[j].len })
	return compactRunes(strings.ReplaceAll(cands[0].text, "\n", " "), 60)
}

func compactRunes(s string, maxLen int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= maxLen {
		return string(r)
	}
	return string(r[:maxLen]) + "..."
}

func sessionPreview(r session.Record) string {
	if r.Preview != "" && !looksSyntheticHistoryText(r.Preview) {
		return r.Preview
	}
	for _, m := range r.Messages {
		if m.Role == "user" && m.Content != "" && !m.Synthetic && !looksSyntheticHistoryText(m.Content) {
			// compactRunes, not a byte slice: this string is the session label
			// in the Ctrl+S history overlay, and content[:50] cut Chinese
			// titles mid-rune so they rendered as mojibake in the picker.
			return compactRunes(strings.ReplaceAll(m.Content, "\n", " "), 50)
		}
	}
	// Don't use low-signal message as preview
	return ""
}

// handleHistoryResume resumes a number typed after a /history listing,
// indexing whichever view (project or all) was listed last.
func handleHistoryResume(input string, eng *engine.Engine) {
	handleHistoryResumeIn(input, eng, historyPickAll)
}

// handleHistoryResumeIn resumes by list number or session ID. A number
// indexes the current project's list, or every project's when all is set; an
// ID resolves regardless of project.
func handleHistoryResumeIn(input string, eng *engine.Engine, all bool) {
	store := eng.Store()
	if store == nil {
		termui.PrintSafe("会话存储不可用\n")
		return
	}

	records, _ := listHistoryRecords(store, currentProjectDir(), all)
	var idx int
	var r *session.Record
	var err error
	var numberIdx int

	if _, errScan := fmt.Sscanf(input, "%d", &idx); errScan == nil && idx >= 1 && idx <= len(records) {
		rMeta := records[idx-1]
		r, err = store.Load(rMeta.ID)
		numberIdx = idx
	} else {
		r, err = store.Load(input)
		// Try to find the matching alphabetical index for visual logging
		for i, rec := range records {
			if rec.ID == input {
				numberIdx = i + 1
				break
			}
		}
	}

	if err != nil {
		termui.PrintSafe("恢复会话失败或无效选择: %s\n输入 /history 查看可用会话。\n", input)
		return
	}

	eng.ResumeSession(r)
	title := effectiveHistoryTitle(*r)

	// Dynamic interactive feedback: print last 4 messages on main console instead of a dry summary!
	termui.PrintSafe("\n==================================================\n")
	if numberIdx > 0 {
		termui.PrintSafe("  ★ 已成功拉回历史会话 #%d: %s\n", numberIdx, title)
	} else {
		termui.PrintSafe("  ★ 已成功拉回历史会话: %s\n", title)
	}
	termui.PrintSafe("==================================================\n\n")
	printProjectMismatchWarning(r)

	if len(r.Messages) == 0 {
		termui.PrintSafe("  (该历史会话为空，现在可以输入指令开始新的对话)\n\n")
		return
	}

	// Show recent conversation messages to restore full context on the main interface
	startIndex := 0
	if len(r.Messages) > 4 {
		startIndex = len(r.Messages) - 4
		termui.PrintSafe("  ... (已隐藏前面 %d 条对话细节) ...\n\n", len(r.Messages)-4)
	}

	for i := startIndex; i < len(r.Messages); i++ {
		msg := r.Messages[i]
		// Format user instructions and assistant remarks beautifully
		switch strings.ToLower(msg.Role) {
		case "user":
			if !strings.HasPrefix(strings.TrimSpace(msg.Content), "[system:") {
				termui.PrintSafe("%s用户 (User):%s\n  %s\n\n", termui.Yellow, termui.Reset, strings.TrimSpace(msg.Content))
			} else {
				// Internal state prompts in dim/italics
				termui.PrintSafe("%s内置微调状态 (System):%s\n  %s\n\n", termui.Dim, termui.Reset, strings.TrimSpace(msg.Content))
			}
		case "assistant":
			if msg.Content != "" {
				termui.PrintSafe("%s助手 (Assistant):%s\n%s\n\n", termui.Green, termui.Reset, strings.TrimSpace(msg.Content))
			}
			for _, tc := range msg.ToolCalls {
				termui.PrintSafe("  %s↳ 触发核心工具: %s, 传入参数: %v%s\n", termui.Dim, tc.Name, tc.Input, termui.Reset)
			}
			if len(msg.ToolCalls) > 0 {
				termui.PrintSafe("\n")
			}
		case "tool":
			// Compress raw execution content so we don't dump 100 lines of compilation logs
			toolContent := strings.TrimSpace(msg.Content)
			if len(toolContent) > 200 {
				toolContent = toolContent[:200] + " ... [数据包已在上下文内激活]"
			}
			termui.PrintSafe("  %s🛠️  工具返回结果: %s%s\n\n", termui.Dim, toolContent, termui.Reset)
		}
	}

	termui.PrintSafe("%s历史会话与运行上下文已被完整恢复。您可以直接继续向 Cove 提问了：%s\n\n", termui.Green, termui.Reset)
}

func handleHistoryDetail(input string, eng *engine.Engine, all bool) {
	if strings.TrimSpace(input) == "" {
		termui.PrintSafe("用法: /history detail <编号|session-id>\n")
		return
	}
	if strings.EqualFold(strings.TrimSpace(input), "interrupted") {
		draft, _ := loadInterruptedDraft()
		if draft == nil {
			termui.PrintSafe("当前没有中断草稿。\n")
			return
		}
		termui.PrintSafe("\n  中断草稿详情\n")
		termui.PrintSafe("  更新时间: %s\n", draft.UpdatedAt.Format("2006-01-02 15:04:05"))
		termui.PrintSafe("  标题: %s\n", draft.Title)
		termui.PrintSafe("  错误: %s\n\n", shortDesc(draft.Error))
		termui.PrintSafe("  用户输入:\n")
		termui.PrintSafe("  %s\n\n", draft.UserContent)
		return
	}
	store := eng.Store()
	if store == nil {
		termui.PrintSafe("会话存储不可用\n")
		return
	}

	resolve := func(sel string) (*session.Record, error) {
		records, _ := listHistoryRecords(store, currentProjectDir(), all)
		var idx int
		if _, err := fmt.Sscanf(sel, "%d", &idx); err == nil && idx >= 1 && idx <= len(records) {
			return store.Load(records[idx-1].ID)
		}
		return store.Load(sel)
	}

	r, err := resolve(strings.TrimSpace(input))
	if err != nil {
		termui.PrintSafe("无效选择: %s\n输入 /history 查看可用会话。\n", input)
		return
	}

	title := effectiveHistoryTitle(*r)

	termui.PrintSafe("\n  会话详情\n")
	termui.PrintSafe("  ID: %s\n", r.ID)
	termui.PrintSafe("  标题: %s\n", title)
	termui.PrintSafe("  更新时间: %s\n", r.UpdatedAt.Format("2006-01-02 15:04:05"))
	termui.PrintSafe("  消息数: %d\n\n", len(r.Messages))

	if len(r.Messages) == 0 {
		termui.PrintSafe("  该会话暂无消息。\n\n")
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

	termui.PrintSafe("  消息预览:\n")
	for i, idx := range indices {
		if total > window && i == 3 {
			termui.PrintSafe("    ...\n")
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
		termui.PrintSafe("  [%03d] %-9s %s\n", idx+1, role, shortDesc(content))
	}
	termui.PrintSafe("\n")
}

func handleHistoryResumeMostRelevant(eng *engine.Engine) bool {
	store := eng.Store()
	if store == nil {
		termui.PrintSafe("会话存储不可用\n")
		return false
	}
	// Only this project's sessions: auto-resuming another codebase's task
	// and then sending "继续" would have the model edit the wrong project.
	all, _ := store.List()
	records := session.FilterByProject(all, currentProjectDir())
	if len(records) == 0 {
		termui.PrintSafe("当前项目暂无历史。\n")
		printHiddenSessionsHint(len(all), "/history all")
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
		handleHistoryResumeIn("1", eng, false)
		return true
	}

	eng.ResumeSession(best.rec)
	title := effectiveHistoryTitle(*best.rec)
	userTurns := countUserTurns(best.rec.Messages)
	termui.PrintSafe("已自动恢复最近有效任务 #%d: %s (%d 轮对话 / %d 条消息)\n", best.idx, title, userTurns, len(best.rec.Messages))
	return true
}

// listHistoryRecords returns the sessions /history (and the Ctrl+R picker)
// shows: cwd's project only, or every project when all is set, minus empty
// and low-signal ones. hidden counts the listable sessions the project view
// left out, so the caller can point at /history all.
func listHistoryRecords(store *session.Store, cwd string, all bool) (records []session.Record, hidden int) {
	records, _ = store.List()
	out := make([]session.Record, 0, len(records))
	for _, r := range records {
		if r.UserTurns == 0 {
			continue
		}
		title := effectiveHistoryTitle(r)
		if title == "" {
			continue
		}
		// Hide low-signal one-liners in /history by default.
		if isLowSignalResumeInput(title) && r.UserTurns <= 1 {
			continue
		}
		out = append(out, r)
	}
	if all {
		return out, 0
	}
	project := session.FilterByProject(out, cwd)
	return project, len(out) - len(project)
}

func effectiveHistoryTitle(r session.Record) string {
	title := strings.TrimSpace(r.Title)
	if title == "New session" || title == "" || looksSyntheticHistoryText(title) {
		title = strings.TrimSpace(sessionPreview(r))
	}
	return title
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
		if m.Synthetic || looksSyntheticHistoryText(m.Content) {
			continue
		}
		n++
	}
	return n
}

func looksSyntheticHistoryText(s string) bool {
	c := strings.TrimSpace(s)
	if c == "" {
		return true
	}
	knownPrefixes := []string{
		"[system:", "[Conversation Summary]",
		"[系统检测到重复操作循环]", "[Context truncated",
		"[用户指引]", "[Continue the task", "[会话摘要]",
	}
	for _, p := range knownPrefixes {
		if strings.HasPrefix(c, p) || strings.EqualFold(c, p) {
			return true
		}
	}
	return false
}

// resumeAndContinue loads the most relevant past session AND then actually
// drives the agent forward by enqueuing a real "继续" turn. Previously the
// resume step only reloaded messages into context and stopped, so typing
// "继续" appeared to do nothing — the model was never invoked.
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
			score++
		}
	}

	return score
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
	termui.PrintSafe("正在继续该任务…\n\n")
	_, _ = tasks.Enqueue(api.Message{Role: "user", Content: "继续"})
	// Don't block the main loop — the task runs in the background.
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
