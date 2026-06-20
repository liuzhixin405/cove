package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/liuzhixin405/cove/internal/config"
	ctxt "github.com/liuzhixin405/cove/internal/context"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/session"
	"github.com/liuzhixin405/cove/internal/tui"
)

// useTUI reports whether the full-screen Bubble Tea UI should be used. The TUI
// is now the DEFAULT for interactive sessions. It is skipped when explicitly
// disabled (--no-tui or COVE_TUI=0) or when stdin/stdout is not a terminal
// (piped/redirected), where the classic line REPL is more robust. --tui or
// COVE_TUI=1 force it on even in those cases.
func useTUI() bool {
	if noTUI || os.Getenv("COVE_TUI") == "0" {
		return false
	}
	if tuiMode || os.Getenv("COVE_TUI") == "1" {
		return true
	}
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

// tuiJobQueue is a small FIFO with condition-variable blocking. The UI goroutine
// pushes user submissions; a single worker goroutine pops them serially. pop
// also returns a snapshot of the still-queued items so the sidebar can show them.
type tuiJobQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []string
	closed bool
}

func newTUIJobQueue() *tuiJobQueue {
	q := &tuiJobQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *tuiJobQueue) push(s string) {
	q.mu.Lock()
	q.items = append(q.items, s)
	q.mu.Unlock()
	q.cond.Signal()
}

// pop blocks until an item is available, returning the next item plus a snapshot
// of the remaining queue. ok is false only when the queue is closed and drained.
func (q *tuiJobQueue) pop() (cur string, rest []string, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		return "", nil, false
	}
	cur = q.items[0]
	q.items = q.items[1:]
	rest = append([]string(nil), q.items...)
	return cur, rest, true
}

// runTUI launches the experimental full-screen Bubble Tea UI. It wires the
// engine's streaming output into the conversation body, keeps the status bars
// (model/provider/git/permission/cost) in sync, shows transient tool/queue
// activity, offers a Ctrl+R history overlay to restore past sessions and a "/"
// command palette to run slash commands. Attachments and interactive permission
// prompts are not yet covered; those remain available in the classic REPL.
//
// commands is the static catalog shown in the palette; runCommand executes a
// "/name args" line and returns its rendered output (run on the task worker so
// commands stay serialized with engine turns and never race shared state).
func runTUI(appVersion string, bannerText string, debugMode bool, eng *engine.Engine, cfg *config.Config, projCtx *ctxt.ProjectContext, permMgr *permission.Manager, commands []tui.CommandItem, runCommand func(string) string) {
	modelName := cfg.Model
	provider := eng.ProviderName()

	permMode := ""
	if permMgr != nil {
		permMode = string(permMgr.Mode())
	}

	// makeStatus snapshots current usage from the cost tracker for the bars.
	makeStatus := func(elapsed string) tui.StatusInfo {
		displayGit := ""
		gitStatusStr := ""
		if projCtx != nil && projCtx.IsGitRepo {
			projCtx.RefreshGit()
			branch, status := projCtx.GetGitInfo()
			displayGit = branch
			gitStatusStr = status
			if strings.TrimSpace(status) != "" && strings.TrimSpace(status) != "(clean)" {
				displayGit += "*"
			}
		}

		s := tui.StatusInfo{
			Version:   appVersion,
			Model:     modelName,
			Provider:  provider,
			Git:       displayGit,
			GitStatus: gitStatusStr,
			PermMode:  permMode,
			Budget:    cfg.MaxBudgetUsd,
			Elapsed:   elapsed,
		}
		if ct := eng.CostTracker(); ct != nil {
			s.TokensIn = ct.TotalInput
			s.TokensOut = ct.TotalOutput
			s.Cost = ct.TotalCost
		}
		return s
	}

	queue := newTUIJobQueue()

	// cancelCurrent cancels the context of the in-flight engine task. It is set
	// by the worker before each run and invoked by the Ctrl+C handler so an
	// interrupt cancels the running task instead of quitting the program.
	var (
		cancelMu      sync.Mutex
		cancelCurrent context.CancelFunc
	)
	setCancel := func(c context.CancelFunc) {
		cancelMu.Lock()
		cancelCurrent = c
		cancelMu.Unlock()
	}
	interrupt := func() {
		cancelMu.Lock()
		c := cancelCurrent
		cancelMu.Unlock()
		if c != nil {
			c()
		}
	}

	var app *tui.App
	app = tui.NewApp(modelName, func(input string) {
		queue.push(input)
	}, func(id string) {
		// Restore a past session picked from the history overlay in TUI mode.
		go func() {
			r, err := eng.Store().Load(id)
			if err != nil {
				app.EngineLine("\n[错误] 恢复会话失败: " + err.Error() + "\n")
				return
			}
			eng.LoadMessages(r.Messages)
			title := r.Title
			if title == "" || title == "New session" || isLowSignalHistoryTitle(title) {
				title = sessionPreview(*r)
			}

			// Show clean rich history in TUI display line-by-line!
			app.EngineLine(fmt.Sprintf("\n==================================================\n  ★ 已恢复会话: %s\n==================================================\n\n", title))

			if len(r.Messages) == 0 {
				app.EngineLine("  (该历史会话为空，可直接输入指令开始新对话)\n\n")
				return
			}

			startIndex := 0
			if len(r.Messages) > 4 {
				startIndex = len(r.Messages) - 4
				app.EngineLine(fmt.Sprintf("  ... (已隐藏前面 %d 条对话细节) ...\n\n", len(r.Messages)-4))
			}

			for i := startIndex; i < len(r.Messages); i++ {
				msg := r.Messages[i]
				switch strings.ToLower(msg.Role) {
				case "user":
					if !strings.HasPrefix(strings.TrimSpace(msg.Content), "[system:") {
						app.EngineLine(fmt.Sprintf("[用户 (User)]:\n  %s\n\n", strings.TrimSpace(msg.Content)))
					} else {
						app.EngineLine(fmt.Sprintf("[内置微调状态 (System)]:\n  %s\n\n", strings.TrimSpace(msg.Content)))
					}
				case "assistant":
					if msg.Content != "" {
						app.EngineLine(fmt.Sprintf("[助手 (Assistant)]:\n%s\n\n", strings.TrimSpace(msg.Content)))
					}
					for _, tc := range msg.ToolCalls {
						app.EngineLine(fmt.Sprintf("  ↳ 触发核心工具: %s, 传入参数: %v\n", tc.Name, tc.Input))
					}
					if len(msg.ToolCalls) > 0 {
						app.EngineLine("\n")
					}
				case "tool":
					toolContent := strings.TrimSpace(msg.Content)
					if len(toolContent) > 200 {
						toolContent = toolContent[:200] + " ... [数据已装载]"
					}
					app.EngineLine(fmt.Sprintf("  🛠️  工具返回: %s\n\n", toolContent))
				}
			}

			app.EngineLine("运行上下文与消息历史已被完整恢复。您可以直接继续对话了：\n\n")
		}()
	}, interrupt, commands)

	// Interactive permission prompts are surfaced through a modal overlay. The
	// engine calls this on the task worker goroutine; RequestPermission blocks
	// it until the user answers in the UI, so turns stay correctly serialized.
	eng.PermissionPrompt = func(toolName string, input map[string]any, reason string) bool {
		switch app.RequestPermission(toolName, permPromptDesc(toolName, input, reason)) {
		case tui.PermAllow:
			return true
		case tui.PermAlways:
			eng.AddPermissionRule(permission.DAllow, permission.Rule{ToolPattern: toolName})
			return true
		default:
			return false
		}
	}

	// Seed the status bars (Send blocks until the program starts consuming).
	go func() { app.SetStatus(makeStatus("")) }()

	// Background ticker to periodically refresh git status so manual edits
	// made outside Cove are automatically picked up and dynamically shown in the UI.
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			app.SetStatus(makeStatus(""))
		}
	}()

	// Load recent sessions into the history overlay. Send blocks until the
	// program starts consuming, so this runs in its own goroutine before Run().
	go func() {
		recs, err := eng.Store().List()
		if err != nil {
			return
		}
		sort.Slice(recs, func(i, j int) bool {
			return recs[i].UpdatedAt.After(recs[j].UpdatedAt)
		})
		items := make([]tui.HistoryItem, 0, len(recs))
		for _, r := range recs {
			title := r.Title
			if title == "New session" || title == "" || isLowSignalHistoryTitle(title) {
				title = sessionPreview(r)
			}
			items = append(items, tui.HistoryItem{
				ID:       r.ID,
				Title:    title,
				Subtitle: fmt.Sprintf("%s · %d 条", r.UpdatedAt.Format("01-02 15:04"), r.MessageCount),
			})
		}
		app.SetHistory(items)
	}()

	go func() {
		// attachedFiles is the worker-local mount list (managed by /attach). It
		// persists across turns until cleared, mirroring the classic REPL.
		var attachedFiles []string
		for {
			input, rest, ok := queue.pop()
			if !ok {
				return
			}

			// Slash commands run synchronously on this worker so they stay
			// serialized with engine turns and never race shared engine state.
			if strings.HasPrefix(strings.TrimSpace(input), "/") {
				trimmed := strings.TrimSpace(input)
				// /attach is handled locally so it can mutate the mount list and
				// reuse the classic attachment helpers.
				if trimmed == "/attach" || strings.HasPrefix(trimmed, "/attach ") {
					cwd, _ := os.Getwd()
					out := tuiAttachCommand(trimmed, cwd, &attachedFiles)
					if strings.TrimSpace(out) != "" {
						app.EngineLine("\n" + strings.TrimRight(out, "\n") + "\n")
					}
					continue
				}

				app.SetTask(tui.TaskInfo{Running: true, Current: input, Queued: rest})
				app.SetActivity("执行命令…")
				out := ""
				if runCommand != nil {
					out = runCommand(input)
				}
				app.ClearActivity()
				if strings.TrimSpace(out) != "" {
					app.EngineLine("\n" + strings.TrimRight(out, "\n") + "\n")
				}
				app.SetStatus(makeStatus(""))
				app.SetTask(tui.TaskInfo{Running: false})
				continue
			}

			// Preflight: mirror the classic REPL so we never fire a doomed request.
			pc := cfg.EffectiveProvider()
			if eng.CostTracker() != nil && eng.CostTracker().OverBudget() {
				app.EngineLine("\n" + budgetExceededRetryHint(eng.CostTracker()) + "\n")
				app.SetStatus(makeStatus(""))
				app.SetTask(tui.TaskInfo{Running: false})
				continue
			}
			if pc.APIKey == "" {
				app.EngineLine("\n" + missingAPIKeyMessage(pc.Name) + "\n")
				app.SetStatus(makeStatus(""))
				app.SetTask(tui.TaskInfo{Running: false})
				continue
			}

			// "继续" recovery: mirror the classic REPL. Resolve the effective
			// message from the interrupted draft or the most relevant past
			// session before driving the engine forward.
			if isContinueCommand(input) {
				resolved, ok := tuiResolveContinue(eng, app)
				if !ok {
					app.SetStatus(makeStatus(""))
					app.SetTask(tui.TaskInfo{Running: false})
					continue
				}
				input = resolved
			}

			app.SetTask(tui.TaskInfo{Running: true, Current: input, Queued: rest})
			app.SetActivity("思考中…")

			// Build the message with inline @path tokens and any mounted
			// attachments, surfacing image/encoding warnings inline.
			cwd, _ := os.Getwd()
			userMsg, warnings, err := buildUserMessage(input, cwd, attachedFiles, cfg.Model)
			if err != nil {
				app.EngineLine("\n[附件] 构建消息失败: " + err.Error() + "\n")
				app.SetActivity("")
				app.SetTask(tui.TaskInfo{Running: false})
				continue
			}
			for _, w := range warnings {
				app.EngineLine("[附件] " + w + "\n")
			}

			start := time.Now()
			eng.OnToolProgress = func(name string, _ string) { app.SetActivity("执行 " + name) }
			eng.OnEngineOutput = func(line string) { app.EngineLine(line) }

			app.BeginStream("")
			ctx, cancel := context.WithCancel(context.Background())
			setCancel(cancel)
			_, err = eng.RunMessageWithStream(
				ctx,
				userMsg,
				func(delta string) { app.Delta(delta) },
				func(reasoning string) { app.Reasoning(reasoning) },
			)
			cancel()
			setCancel(nil)
			if err != nil {
				if ctx.Err() != nil {
					app.EngineLine("\n[已取消] 当前任务已终止\n")
				} else {
					app.EngineLine("\n[错误] " + err.Error() + "\n")
				}
			}

			// Auto save session at the end of each turn in TUI mode as well!
			if eng.HasMessages() {
				eng.SaveSession()
			}

			app.EndStream()
			app.ClearActivity()

			app.SetStatus(makeStatus(time.Since(start).Truncate(time.Millisecond).String()))
			app.SetTask(tui.TaskInfo{Running: false})
		}
	}()

	// Seed the startup info into the conversation body so the TUI shows the same
	// launch context the classic REPL prints (banner, startup diagnostics and the
	// interrupted-draft notice). The alternate screen would otherwise wipe any
	// pre-Run stdout/stderr output. Send blocks until the program loop consumes,
	// so this runs in its own goroutine before Run().
	go func() {
		var intro strings.Builder
		intro.WriteString(strings.TrimRight(bannerText, "\n"))
		if d := strings.TrimRight(startupDiagnosticsText(cfg, debugMode), "\n"); d != "" {
			intro.WriteString("\n" + d)
		}
		if draft, _ := loadInterruptedDraft(); draft != nil && strings.TrimSpace(draft.UserContent) != "" {
			age := time.Since(draft.UpdatedAt).Truncate(time.Second)
			intro.WriteString(fmt.Sprintf("\n\n您有一个未完成的片段草稿（创建于 %v 前）。输入「继续」恢复，或直接输入新指令忽略。", age))
			intro.WriteString("\n\x1b[2m提示: 使用 Ctrl+R 可查看全部可恢复的历史会话\x1b[0m")
		}
		if s := strings.TrimSpace(intro.String()); s != "" {
			app.EngineLine(intro.String() + "\n")
		}
	}()

	if err := app.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "TUI 错误:", err)
	}
}

// tuiResolveContinue mirrors the classic REPL's "继续" handling for the TUI. It
// resolves what the engine should actually do next and returns the effective
// user message plus whether to proceed. When it loads recovery context into the
// engine it surfaces a notice through the transcript (app.EngineLine), since the
// alternate screen swallows direct stdout writes used by the classic path.
func tuiResolveContinue(eng *engine.Engine, app *tui.App) (string, bool) {
	if draft, _ := loadInterruptedDraft(); draft != nil && strings.TrimSpace(draft.UserContent) != "" {
		if isLowSignalResumeInput(draft.UserContent) {
			_ = clearInterruptedDraft()
			if notice := tuiResumeMostRelevant(eng); notice != "" {
				app.EngineLine("\n[恢复] " + notice + "\n正在继续该任务…\n")
				return "继续", true
			}
			app.EngineLine("\n[提示] 没有可恢复的历史任务。\n")
			return "", false
		}
		_ = clearInterruptedDraft()
		app.EngineLine("\n[恢复] 已加载未完成草稿，正在继续…\n")
		return draft.UserContent, true
	}

	if notice := tuiResumeMostRelevant(eng); notice != "" {
		app.EngineLine("\n[恢复] " + notice + "\n正在继续该任务…\n")
		return "继续", true
	}
	app.EngineLine("\n[提示] 没有可恢复的草稿或历史任务。\n")
	return "", false
}

// tuiResumeMostRelevant loads the most relevant past session into the engine and
// returns a human-readable notice (or "" when nothing could be resumed). It is a
// TUI-friendly variant of handleHistoryResumeMostRelevant that returns text
// instead of printing to stdout.
func tuiResumeMostRelevant(eng *engine.Engine) string {
	store := eng.Store()
	if store == nil {
		return ""
	}
	records, _ := store.List()
	if len(records) == 0 {
		return ""
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
		return ""
	}

	eng.LoadMessages(best.rec.Messages)
	title := best.rec.Title
	if title == "New session" || title == "" || isLowSignalHistoryTitle(title) {
		title = sessionPreview(*best.rec)
	}
	userTurns := countUserTurns(best.rec.Messages)
	return fmt.Sprintf("已自动恢复最近有效任务 #%d: %s (%d 轮对话 / %d 条消息)", best.idx, title, userTurns, len(best.rec.Messages))
}

// permPromptDesc builds a short, human-readable summary for the permission
// overlay, mirroring the classic REPL's prompt (file path for write/edit, the
// command for shells, otherwise the engine-supplied reason).
func permPromptDesc(toolName string, input map[string]any, reason string) string {
	switch toolName {
	case "write", "edit":
		if p, ok := input["filePath"].(string); ok && p != "" {
			return p
		}
	case "bash", "powershell":
		if cmd, ok := input["command"].(string); ok && cmd != "" {
			if len(cmd) > 80 {
				cmd = cmd[:80] + "..."
			}
			return cmd
		}
	}
	return reason
}

// tuiAttachCommand handles "/attach …" in the TUI worker, mutating the mount
// list and returning rendered text (instead of printing to stdout like the
// classic REPL). It reuses the shared splitQuotedFields/normalizeAttachmentPath
// helpers from attachments.go.
func tuiAttachCommand(input, cwd string, attached *[]string) string {
	argsText := strings.TrimSpace(strings.TrimPrefix(input, "/attach"))
	if argsText == "" {
		return tuiAttachList(*attached)
	}
	args, err := splitQuotedFields(argsText)
	if err != nil {
		return fmt.Sprintf("附件命令解析失败: %v", err)
	}
	if len(args) == 0 {
		return tuiAttachList(*attached)
	}
	switch strings.ToLower(args[0]) {
	case "list", "ls":
		return tuiAttachList(*attached)
	case "clear":
		*attached = nil
		return "已清空附件列表"
	case "remove", "rm":
		return tuiAttachRemove(args[1:], attached)
	case "add":
		return tuiAttachAdd(args[1:], cwd, attached)
	default:
		return tuiAttachAdd(args, cwd, attached)
	}
}

func tuiAttachList(paths []string) string {
	if len(paths) == 0 {
		return "当前没有挂载附件。用 /attach <文件...> 添加。"
	}
	var b strings.Builder
	b.WriteString("当前挂载附件:")
	for i, p := range paths {
		b.WriteString(fmt.Sprintf("\n  %d. %s", i+1, p))
	}
	return b.String()
}

func tuiAttachAdd(paths []string, cwd string, attached *[]string) string {
	if len(paths) == 0 {
		return "用法: /attach <文件...> | /attach list | /attach remove <序号> | /attach clear"
	}
	seen := map[string]bool{}
	for _, existing := range *attached {
		seen[existing] = true
	}
	var b strings.Builder
	added := 0
	for _, rawPath := range paths {
		absPath, err := normalizeAttachmentPath(cwd, rawPath)
		if err != nil {
			b.WriteString(fmt.Sprintf("跳过 %s: %v\n", rawPath, err))
			continue
		}
		if seen[absPath] {
			continue
		}
		*attached = append(*attached, absPath)
		seen[absPath] = true
		added++
	}
	b.WriteString(fmt.Sprintf("已挂载 %d 个附件，当前共 %d 个。\n", added, len(*attached)))
	b.WriteString(tuiAttachList(*attached))
	return b.String()
}

func tuiAttachRemove(args []string, attached *[]string) string {
	if len(args) == 0 {
		return "用法: /attach remove <序号>"
	}
	idx, err := strconv.Atoi(args[0])
	if err != nil || idx < 1 || idx > len(*attached) {
		return fmt.Sprintf("无效附件序号: %s", args[0])
	}
	removed := (*attached)[idx-1]
	*attached = append((*attached)[:idx-1], (*attached)[idx:]...)
	return fmt.Sprintf("已移除附件: %s\n%s", removed, tuiAttachList(*attached))
}
