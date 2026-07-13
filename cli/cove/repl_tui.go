package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/liuzhixin405/cove/internal/command"
	"github.com/liuzhixin405/cove/internal/config"
	ctxt "github.com/liuzhixin405/cove/internal/context"
	"github.com/liuzhixin405/cove/internal/cost"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/plugin"
	"github.com/liuzhixin405/cove/internal/session"
	"github.com/liuzhixin405/cove/internal/skills"
	"github.com/liuzhixin405/cove/internal/tool"
	"github.com/liuzhixin405/cove/internal/tui"
)

// useTUI reports whether the Bubble Tea UI should be used. The TUI
// is now the DEFAULT for interactive sessions. It is skipped when explicitly
// disabled (--no-tui or COVE_TUI=0) or when stdin/stdout is not a terminal
// (piped/redirected), where the classic line REPL is more robust. --tui or
// COVE_TUI=1 force it on even in those cases.
// cleanANSI strips ANSI escape sequences from a string for TUI rendering.
func cleanANSI(s string) string {
	// Remove ANSI color/style codes and carriage returns from engine output
	result := s
	// Carriage returns and clear-line sequences
	result = strings.ReplaceAll(result, "\r", "")
	result = strings.ReplaceAll(result, "\x1b[K", "")
	// Remove ANSI escape sequences (ESC[...m)
	for {
		i := strings.Index(result, "\x1b[")
		if i < 0 {
			break
		}
		j := strings.IndexByte(result[i:], 'm')
		if j < 0 {
			break
		}
		result = result[:i] + result[i+j+1:]
	}
	return result
}

func useTUI() bool {
	if noTUI || os.Getenv("COVE_TUI") == "0" {
		return false
	}
	if tuiMode || os.Getenv("COVE_TUI") == "1" {
		return true
	}
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

// runTUI launches the experimental full-screen Bubble Tea UI. It wires the
// engine's streaming output into the conversation body, keeps the status bars
// (model/provider/git/permission/cost) in sync, shows transient tool/queue
// activity, offers a Ctrl+S history overlay to restore past sessions and a Ctrl+K
// command palette to run slash commands. Attachments and interactive permission
// prompts are not yet covered; those remain available in the classic REPL.
//
// commands is the static catalog shown in the palette; runCommand executes a
// "/name args" line and returns its rendered output (run on the task worker so
// commands stay serialized with engine turns and never race shared state).
func runTUI(appVersion string, bannerText string, debugMode bool, eng *engine.Engine, cfg *config.Config, projCtx *ctxt.ProjectContext, permMgr *permission.Manager, commands []tui.CommandItem, runCommand func(string) string, cmdReg *command.Registry, toolReg *tool.Registry, pluginMgr *plugin.Manager) {
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

	queue := session.NewTaskRunner()
	var running atomic.Bool
	taskSnapshot := func() (bool, []string) {
		return running.Load(), queue.Snapshot()
	}

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
		if running.Load() {
			// Agent is busy: interrupt current task and re-queue the new input
			// at the front so it runs immediately (matches Hermes interrupt pattern).
			interrupt()
			queue.EnqueueFront(input)
			return
		}
		queue.Enqueue(input)
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
			if title == "" || title == "New session" || looksSyntheticHistoryText(title) {
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
					if !msg.Synthetic && !looksSyntheticHistoryText(msg.Content) {
						app.EngineLine(fmt.Sprintf("[用户 (User)]:\n  %s\n\n", strings.TrimSpace(msg.Content)))
					} else {
						app.EngineLine(fmt.Sprintf("[内置微调状态(System)]:\n  %s\n\n", strings.TrimSpace(msg.Content)))
					}
				case "assistant":
					if msg.Content != "" {
						app.EngineLine(fmt.Sprintf("[助手 (Assistant)]:\n%s\n\n", strings.TrimSpace(msg.Content)))
					}
					for _, tc := range msg.ToolCalls {
						app.EngineLine(fmt.Sprintf("  -> 触发核心工具: %s, 传入参数: %v\n", tc.Name, tc.Input))
					}
					if len(msg.ToolCalls) > 0 {
						app.EngineLine("\n")
					}
				case "tool":
					toolContent := strings.TrimSpace(msg.Content)
					if len(toolContent) > 200 {
						toolContent = toolContent[:200] + " ... [数据已截断]"
					}
					app.EngineLine(fmt.Sprintf("  [工具返回] %s\n\n", toolContent))
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
			// Skip sessions with no genuine user input: they only contain
			// engine-injected synthetic prompts (loop guidance, circuit-breaker
			// hints, compaction summaries) and were polluting the Ctrl+S list with
			// records the user never actually started.
			if r.UserTurns == 0 {
				continue
			}
			title := r.Title
			if title == "New session" || title == "" || looksSyntheticHistoryText(title) {
				title = sessionPreview(r)
			}
			if strings.TrimSpace(title) == "" {
				continue
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
			input, rest, ok := queue.Next()
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

				// Handle missing slash commands that the classic REPL supports
				// but are not registered in the command registry.
				handled, cmdOut := handleTUISlashCommand(trimmed, eng, cfg, permMgr, app, interrupt, runCommand, appVersion, cmdReg, toolReg, pluginMgr, taskSnapshot)
				if handled {
					app.SetTask(tui.TaskInfo{Running: false})
					if strings.TrimSpace(cmdOut) != "" {
						app.EngineLine("\n" + strings.TrimRight(cmdOut, "\n") + "\n")
					}
					app.SetStatus(makeStatus(""))
					continue
				}

				// Skill invocation: bare "/<skillname>" shows the skill prompt
				// (parity with the classic REPL's handleSkillInvocation).
				if tuiIsSkillInvocation(trimmed, eng) {
					app.SetTask(tui.TaskInfo{Running: false})
					if out := tuiSkillInvocationText(trimmed, eng); strings.TrimSpace(out) != "" {
						app.EngineLine("\n" + strings.TrimRight(out, "\n") + "\n")
					}
					app.SetStatus(makeStatus(""))
					continue
				}

				// Plugin command: inject the plugin command's prompt into the
				// engine as a normal user turn (parity with handlePluginCommand).
				if prompt, label, ok := tuiPluginCommandPrompt(trimmed, pluginMgr); ok {
					app.EngineLine("\n[插件命令: /" + label + "]\n")
					queue.EnqueueFront(prompt)
					continue
				}

				// Unknown slash command (not registered): show fuzzy suggestions
				// instead of falling through to a doomed engine request.
				if fields := strings.Fields(trimmed); len(fields) > 0 {
					base := strings.TrimPrefix(fields[0], "/")
					if base != "" && cmdReg != nil {
						if _, known := cmdReg.Find(base); !known {
							app.SetTask(tui.TaskInfo{Running: false})
							app.EngineLine("\n" + strings.TrimRight(tuiUnknownCmdText(trimmed, cmdReg), "\n") + "\n")
							app.SetStatus(makeStatus(""))
							continue
						}
					}
				}

				app.SetTask(tui.TaskInfo{Running: true, Current: input, Queued: rest})
				app.SetActivity(tuiCommandActivityText())
				var out string
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
			app.SetActivity(tuiThinkingActivityText())

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

			// Auto-switch to a vision model when an image attachment is detected
			// but the current model can't see it (parity with the classic REPL).
			// Rebuild the message afterwards so the switched model receives it.
			if shouldAutoSwitchToVision(warnings) {
				if visionModel := preferredVisionModelForProvider(pc.Name, cfg.Model); visionModel != "" && visionModel != cfg.Model {
					if switchErr := applyProviderConfigChange(cfg, eng, func() error {
						cfg.Model = visionModel
						return nil
					}); switchErr == nil {
						app.EngineLine("[视觉] 检测到图片附件，已自动切换到视觉模型 " + visionModel + "\n")
						userMsg, warnings, err = buildUserMessage(input, cwd, attachedFiles, cfg.Model)
						if err != nil {
							app.EngineLine("\n[附件] 构建消息失败: " + err.Error() + "\n")
							app.SetActivity("")
							app.SetTask(tui.TaskInfo{Running: false})
							continue
						}
						for _, w := range warnings {
							app.EngineLine("[附件] " + w + "\n")
						}
					}
				}
			}

			start := time.Now()
			eng.OnToolProgress = func(name string, _ string) { app.SetActivity(tuiToolActivityText(name)) }
			eng.OnEngineOutput = func(line string) { app.EngineLine(cleanANSI(line)) }
			eng.OnToolStart = func(name string) { app.SetActivity(tuiToolActivityText(name)) }

			app.BeginStream("")
			running.Store(true)
			ctx, cancel := context.WithCancel(context.Background())
			setCancel(cancel)
			reply, err := eng.RunMessageWithStream(
				ctx,
				userMsg,
				func(delta string) { app.Delta(delta) },
				func(reasoning string) { app.Reasoning(reasoning) },
			)
			cancel()
			setCancel(nil)
			running.Store(false)
			if err != nil {
				// Persist an interrupted/failed draft so a later "继续" can
				// recover it (parity with the classic REPL's task runner, which
				// was the only place that wrote drafts).
				_ = saveInterruptedDraft(userMsg, err)
				if ctx.Err() != nil {
					app.EngineLine("\n[已取消] 当前任务已终止\n")
				} else {
					app.EngineLine("\n[错误] " + err.Error() + "\n")
				}
			} else {
				// Turn completed cleanly: drop any stale interrupted draft.
				_ = clearInterruptedDraft()
			}

			// Auto save session at the end of each turn in TUI mode as well!
			if eng.HasMessages() {
				eng.SaveSession()
			}

			app.EndStreamAlign(reply)
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
			intro.WriteString("\n\x1b[2m提示: 使用 Ctrl+S 可查看全部可恢复的历史会话\x1b[0m")
		}
		if s := strings.TrimSpace(intro.String()); s != "" {
			app.EngineLine(intro.String() + "\n")
		}
	}()

	if err := app.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "TUI 错误:", err)
	}
}

// handleTUISlashCommand intercepts slash commands that the classic REPL handles
// through its own hard-coded dispatch but are not registered in the command
// registry. It returns (handled=true, output) when it recognises the command,
// or (handled=false, "") to let the caller fall through to runCommand.
func handleTUISlashCommand(
	trimmed string,
	eng *engine.Engine,
	cfg *config.Config,
	permMgr *permission.Manager,
	app *tui.App,
	interrupt func(),
	runCommand func(string) string,
	appVersion string,
	cmdReg *command.Registry,
	toolReg *tool.Registry,
	pluginMgr *plugin.Manager,
	taskSnapshot func() (running bool, queued []string),
) (bool, string) {
	cmdName := strings.TrimPrefix(trimmed, "/")
	parts := strings.Fields(cmdName)
	if len(parts) == 0 {
		return true, ""
	}
	name := strings.ToLower(parts[0])
	args := parts[1:]

	switch name {
	case "help":
		var sb strings.Builder
		sb.WriteString("\n=== cove v" + appVersion + " ===\n")
		sb.WriteString("\n供应商 / 模型:\n")
		sb.WriteString("  /model <名称>       设置模型\n")
		sb.WriteString("  /provider <名称>    设置供应商\n")
		sb.WriteString("  /api-key <密钥>     保存 API 密钥\n")
		sb.WriteString("  /base-url <地址>    设置自定义接口地址\n")
		sb.WriteString("  /mode <模式>        设置权限模式 (default|plan|auto|bypass)\n")
		sb.WriteString("  /budget <金额|auto> 设置每会话预算上限($)\n")
		sb.WriteString("  /cost               查看用量和费用\n")
		sb.WriteString("  /ratelimit          查看 API 速率限制状态\n")
		sb.WriteString("  /attach <文件...>   挂载图片或文件\n")
		sb.WriteString("  /config             查看完整配置\n")
		sb.WriteString("\n会话:\n")
		sb.WriteString("  /compact            压缩对话历史\n")
		sb.WriteString("  /history            查看和继续历史会话\n")
		sb.WriteString("  /history clean      清洗历史会话噪音并自动备份\n")
		sb.WriteString("  /resume [id]        恢复已保存会话\n")
		sb.WriteString("  /memory             管理持久化记忆\n")
		sb.WriteString("  /export             导出对话到文件\n")
		sb.WriteString("\n后台任务:\n")
		sb.WriteString("  /tasks              查看运行中/排队任务\n")
		sb.WriteString("  /stop               取消当前任务 (别名 /cancel)\n")
		sb.WriteString("\n系统:\n")
		sb.WriteString("  /mcp                管理 MCP 服务器\n")
		sb.WriteString("  /plugin             管理插件\n")
		sb.WriteString("  /skills             列出技能\n")
		sb.WriteString("  /diagnose           运行系统诊断\n")
		sb.WriteString("  /doctor             检查 Go、git 环境\n")
		if cmdReg != nil {
			sb.WriteString("\n命令:\n")
			for _, c := range cmdReg.All() {
				sb.WriteString(fmt.Sprintf("  /%-16s %s\n", c.Name(), c.Description()))
			}
		}
		if toolReg != nil {
			sb.WriteString("\n工具:\n")
			for _, t := range toolReg.All() {
				d := t.Def()
				sb.WriteString(fmt.Sprintf("  [%s] %-12s %s\n", roLabel(d.IsReadOnly), d.Name, truncateDesc(d.Description, 48)))
			}
		}
		return true, sb.String()
	case "exit", "quit":
		app.Quit()
		return true, ""
	case "stop", "cancel":
		interrupt()
		return true, "已发送取消信号"
	case "tasks":
		if taskSnapshot == nil {
			return true, "任务快照不可用"
		}
		running, queued := taskSnapshot()
		return true, formatTaskSnapshotText(running, queued)
	case "compact":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		eng.Compact(ctx)
		return true, "上下文窗口已压缩。"
	case "resume":
		return true, tuiResumeText(strings.Join(args, " "), eng)
	case "export":
		return true, tuiExportText(trimmed, eng)
	case "history":
		if len(args) > 0 && strings.EqualFold(args[0], "clean") {
			handleHistoryClean()
			tuiRefreshHistoryOverlay(app, eng)
			return true, "历史清洗已执行。"
		}
		if len(args) > 0 && strings.EqualFold(args[0], "detail") {
			return true, tuiHistoryDetailText(strings.Join(args[1:], " "), eng)
		}
		store := eng.Store()
		if store == nil {
			return true, "会话存储不可用"
		}
		records := listHistoryRecords(store)
		if len(records) == 0 {
			return true, "暂无可恢复历史。"
		}
		limit := len(records)
		if limit > 20 {
			limit = 20
		}
		var sb strings.Builder
		sb.WriteString("\n历史记录:\n\n")
		for i, r := range records[:limit] {
			title := effectiveHistoryTitle(r)
			if title == "" {
				title = r.UpdatedAt.Format("01-02 15:04")
			}
			if len(title) > 50 {
				title = title[:50] + "..."
			}
			sb.WriteString(fmt.Sprintf("  %2d. [%s] %s\n", i+1, r.UpdatedAt.Format("01-02 15:04"), title))
		}
		sb.WriteString("\n提示: 通过 Ctrl+S 可交互搜索并恢复历史会话。\n")
		return true, sb.String()
	case "cost":
		var sb strings.Builder
		if eng.CostTracker() != nil {
			sb.WriteString("本次会话: " + eng.CostTracker().Summary() + "\n")
		}
		ch := cost.NewCostHistory()
		if len(ch.Records) > 0 {
			sb.WriteString(fmt.Sprintf("近 24小时: $%.4f | 近 7天: $%.4f | 总计: $%.4f (%d 个会话)\n", ch.Last24Hours(), ch.Last7Days(), ch.TotalAllTime(), len(ch.Records)))
		}
		return true, sb.String()
	case "skills", "skill":
		rt := eng.Runtime()
		prompts := rt.SkillPrompts
		if len(args) == 0 || args[0] == "list" {
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("\n已安装的技能 (%d):\n", len(prompts)))
			names := make([]string, 0, len(prompts))
			for n := range prompts {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				prompt := prompts[n]
				desc := strings.SplitN(prompt, "\n", 2)
				d := ""
				if len(desc) > 0 {
					d = strings.TrimSpace(desc[0])
					if len(d) > 60 {
						d = d[:57] + "..."
					}
				}
				sb.WriteString(fmt.Sprintf("  %-16s %s\n", n, d))
			}
			return true, sb.String()
		}
		switch args[0] {
		case "marketplace", "registry", "search":
			entries, err := skills.FetchRegistry()
			if err != nil {
				return true, fmt.Sprintf("获取技能市场列表失败: %v\n", err)
			}
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("\n技能市场 (%d 个可用技能):\n", len(entries)))
			for _, e := range entries {
				installed := ""
				if _, ok := prompts[e.Name]; ok {
					installed = " [installed]"
				}
				sb.WriteString(fmt.Sprintf("  %-16s %s%s\n", e.Name, truncateDesc(e.Description, 48), installed))
			}
			sb.WriteString("\n使用 /skill install <name> 安装技能\n")
			return true, sb.String()
		case "install":
			if len(args) < 2 {
				return true, "用法: /skill install <名称>\n"
			}
			name := args[1]
			if _, ok := prompts[name]; ok {
				return true, fmt.Sprintf("技能 '%s' 已安装\n", name)
			}
			entries, _ := skills.FetchRegistry()
			found := false
			for _, e := range entries {
				if e.Name == name {
					skills.InstallSkill(name, "url", e.URL)
					found = true
					return true, fmt.Sprintf("成功安装技能 %s！现在可以使用 /skill %s 调用它。\n", name, name)
				}
			}
			if !found {
				skills.InstallSkill(name, "local", "")
				return true, fmt.Sprintf("成功创建本地技能目录 %s，请编辑 ~/.cove/skills/%s/SKILL.md\n", name, name)
			}
		case "create":
			if len(args) < 2 {
				return true, "用法: /skill create <名称>\n"
			}
			name := args[1]
			skills.InstallSkill(name, "local", "")
			return true, fmt.Sprintf("成功创建本地技能目录 %s，请编辑 ~/.cove/skills/%s/SKILL.md\n", name, name)
		default:
			name := args[0]
			prompt, ok := prompts[name]
			if !ok {
				return true, fmt.Sprintf("\n未找到技能 %s。您可以使用 /skill marketplace 浏览可用技能，或自建技能。\n", name)
			}
			return true, fmt.Sprintf("\n[Skill %s]\n\n%s\n\n", name, prompt)
		}
		return true, ""
	case "model", "provider", "api-key", "api_key", "base-url", "base_url", "mode", "budget":
		configLine := "/config " + name + " " + strings.Join(args, " ")
		if runCommand != nil {
			return true, runCommand(configLine)
		}
		return true, ""
	}
	return false, ""
}

func formatTaskSnapshotText(running bool, queued []string) string {
	var sb strings.Builder
	sb.WriteString("任务状态:\n")
	if running {
		sb.WriteString("- 运行中: 是\n")
	} else {
		sb.WriteString("- 运行中: 否\n")
	}
	if len(queued) == 0 {
		sb.WriteString("- 排队: 0\n")
		return strings.TrimRight(sb.String(), "\n")
	}
	sb.WriteString(fmt.Sprintf("- 排队: %d\n", len(queued)))
	limit := len(queued)
	if limit > 8 {
		limit = 8
	}
	for i := 0; i < limit; i++ {
		sb.WriteString(fmt.Sprintf("  %d. %s\n", i+1, strings.TrimSpace(queued[i])))
	}
	if len(queued) > limit {
		sb.WriteString(fmt.Sprintf("  ... 其余 %d 项\n", len(queued)-limit))
	}
	return strings.TrimRight(sb.String(), "\n")
}

func roLabel(readOnly bool) string {
	if readOnly {
		return "R"
	}
	return " "
}

func tuiRefreshHistoryOverlay(app *tui.App, eng *engine.Engine) {
	store := eng.Store()
	if store == nil {
		return
	}
	recs := listHistoryRecords(store)
	items := make([]tui.HistoryItem, 0, len(recs))
	for _, r := range recs {
		title := effectiveHistoryTitle(r)
		if strings.TrimSpace(title) == "" {
			continue
		}
		items = append(items, tui.HistoryItem{
			ID:       r.ID,
			Title:    title,
			Subtitle: fmt.Sprintf("%s · %d 条", r.UpdatedAt.Format("01-02 15:04"), r.MessageCount),
		})
	}
	app.SetHistory(items)
}

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
	if title == "New session" || title == "" || looksSyntheticHistoryText(title) {
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

// tuiAttachCommand handles "/attach ..." in the TUI worker, mutating the mount
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
	return fmt.Sprintf("已移除附件 %s\n%s", removed, tuiAttachList(*attached))
}

func tuiCommandActivityText() string {
	if tui.PreferASCIIText() {
		return "running command..."
	}
	return "执行命令…"
}

func tuiThinkingActivityText() string {
	if tui.PreferASCIIText() {
		return "thinking..."
	}
	return "思考中…"
}

func tuiToolActivityText(name string) string {
	if tui.PreferASCIIText() {
		return "running " + name
	}
	return "执行 " + name
}
