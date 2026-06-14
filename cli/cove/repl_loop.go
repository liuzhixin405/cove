package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/command"
	"github.com/liuzhixin405/cove/internal/config"
	ctxt "github.com/liuzhixin405/cove/internal/context"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/mcp"
	"github.com/liuzhixin405/cove/internal/memory"
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/plugin"
	"github.com/liuzhixin405/cove/internal/repl"
	"github.com/liuzhixin405/cove/internal/skills"
	"github.com/liuzhixin405/cove/internal/state"
	"github.com/liuzhixin405/cove/internal/tool"
)

// replInteractive 为 true 时表示处于交互式 REPL，主循环会经 TakePermInputCh
// 转发权限确认输入；-p 一次性模式为 false，权限回退到直接读 stdin。
var replInteractive bool

func runREPL(bannerText string, eng *engine.Engine, cmdReg *command.Registry, toolReg *tool.Registry, pm *permission.Manager, as *state.AppState, cfg *config.Config, mcpPool *mcp.Pool, skillMgr *skills.Manager, memStore *memory.Store, pluginMgr *plugin.Manager, projCtx *ctxt.ProjectContext) {

	replInteractive = true

	allCommands := buildCommandList(cmdReg, toolReg)

	for name, c := range pluginMgr.CommandPrompts() {

		allCommands = append(allCommands, cmdEntry{Name: "/" + name, Desc: c.Description, Type: "cmd"})

	}

	// Build skill description map (short descriptions, not full prompts)

	skillDescs := buildSkillDescs(skillMgr)

	reader := repl.New(func(input string) []string {

		return complete(input, allCommands, skillDescs)

	})

	var attachedFiles []string

	historyPickPending := false

	tasks := newREPLTaskRunner(eng)

	// Print the banner directly to the terminal (inline rendering).
	fmt.Print(bannerText)

	// Ctrl+C while a task is running:

	// During reader.ReadLine() the terminal is in raw mode, so Ctrl+C arrives as

	// a literal 0x03 byte and is handled as ErrInterrupt below. But while a task

	// runs, the main loop blocks in tasks.WaitIdle() with the terminal restored

	// to cooked mode, so Ctrl+C is delivered as a SIGINT signal instead. Without

	// a handler the Go runtime's default action terminates the whole process —

	// the reported "提示按 Ctrl+C 可中断，实际却直接退出" bug. Install a persistent

	// handler that cancels the running task on SIGINT instead of killing the

	// program. (SIGTERM is intentionally left to the default action so external

	// `kill` still stops the program.)

	taskSigCh := make(chan os.Signal, 1)

	signal.Notify(taskSigCh, syscall.SIGINT)

	defer signal.Stop(taskSigCh)

	go func() {

		for range taskSigCh {

			// In raw-mode ReadLine no SIGINT is generated (Ctrl+C is read as a

			// byte), so this only fires during task execution / cooked mode.

			if tasks.CancelRunning() {

				repl.PrintAbove(fmt.Sprintf("\r\n%s[已中断] 正在停止当前任务…%s\r\n", repl.Yellow, repl.Reset))

			}

		}

	}()

	// On startup, check for interrupted draft and notify user

	if draft, _ := loadInterruptedDraft(); draft != nil && strings.TrimSpace(draft.UserContent) != "" {

		title := draft.Title

		if title == "" {

			title = shortDesc(draft.UserContent)

		}

		age := time.Since(draft.UpdatedAt).Truncate(time.Second)

		repl.PrintAbove(fmt.Sprintf("您有一个未完成的片段草稿 (创建于 %v 前)。输入\u300e继续\u300f恢复，或直接输入新指令忽略。\r\n", age))

		repl.PrintAbove("  \x1b[2m提示: 使用 /history 可查看全部可恢复的历史会话\x1b[0m\r\n\r\n")

	}

	for {
		// Dynamic prompt: show ⚡ when a background task is running.
		if tasks.IsRunning() {
			reader.SetPrompt(repl.PromptRunning())
		} else {
			reader.SetPrompt(repl.Prompt())
		}

		input, err := reader.ReadLine()

		if err == repl.ErrInterrupt {

			if tasks.IsRunning() {

				if tasks.CancelRunning() {

					repl.PrintAbove("[系统] 等待任务停止...\r\n")

				}

				continue

			}

			repl.PrintAbove("Type /exit or Ctrl+D to exit.\r\n")

			continue

		}

		if err == repl.ErrExit {

			autoSaveSession(eng)

			repl.PrintAbove("Goodbye!\r\n")

			return

		}

		if err != nil {

			repl.PrintSafe("\r\n终端读取错误，正在重新初始化 REPL...\r\n")

			reader = repl.New(func(input string) []string {

				return complete(input, allCommands, skillDescs)

			})

			continue

		}

		// If a permission prompt is waiting for an answer, route this line to it

		// instead of processing it as a task.

		if ch := repl.TakePermInputCh(); ch != nil {

			ch <- input

			continue

		}

		if input == "" {

			continue

		}

		if historyPickPending && !strings.HasPrefix(input, "/") {

			if isPositiveNumber(input) {

				handleHistoryResume(input, eng)

				historyPickPending = false

				continue

			}

			historyPickPending = false

		}

		switch {

		case input == "exit" || input == "/exit":

			// Wait briefly for the task goroutine to finish its cleanup (save draft)

			if tasks.CancelRunning() {

				_ = tasks.WaitIdleUntil(time.Now().Add(3 * time.Second))

			}

			autoSaveSession(eng)

			repl.PrintAbove("Goodbye!\r\n")

			return

		case isContinueCommand(input):

			if tasks.IsRunning() {

				repl.PrintAbove("[提示] 当前有任务正在运行，请等待其结束后再重试。\r\n")

				historyPickPending = false

				continue

			}

			if eng.CostTracker() != nil && eng.CostTracker().OverBudget() {

				repl.PrintAbove(budgetExceededRetryHint(eng.CostTracker()) + "\r\n")

				historyPickPending = false

				continue

			}

			pf := tasks.PendingFailed()

			if pf != nil {

				if isLowSignalResumeInput(pf.Content) {

					tasks.ClearPendingFailed()

					_ = clearInterruptedDraft()

					repl.PrintAbove("[提示] 已为您推荐相关历史任务...\r\n")

					resumeAndContinue(eng, tasks)

					historyPickPending = false

					continue

				}

				tasks.ClearPendingFailed()

				_, merged := tasks.Enqueue(*pf)

				if merged {

					repl.PrintAbove("[恢复] 任务记录已合并。\r\n")

				} else {

					repl.PrintAbove("[恢复] 任务已排队，即将开始处理。\r\n")

				}

				// Don't block: task runs in the background.

				historyPickPending = false

				continue

			}

			if draft, _ := loadInterruptedDraft(); draft != nil && strings.TrimSpace(draft.UserContent) != "" {

				if isLowSignalResumeInput(draft.UserContent) {

					_ = clearInterruptedDraft()

					repl.PrintAbove("[提示] 已为您推荐相关历史任务...\r\n")

					resumeAndContinue(eng, tasks)

					historyPickPending = false

					continue

				}

				msg := api.Message{Role: "user", Content: draft.UserContent}

				_, merged := tasks.Enqueue(msg)

				if merged {

					repl.PrintAbove("[恢复] 任务记录已合并。\r\n")

				} else {

					repl.PrintAbove("[恢复] 任务已排队，即将开始处理。\r\n")

				}

				// Don't block: task runs in the background.

				historyPickPending = false

				continue

			}

			resumeAndContinue(eng, tasks)

			historyPickPending = false

			continue

		case input == "/stop" || input == "/cancel":

			if tasks.IsRunning() {
				if tasks.CancelRunning() {
					repl.PrintAbove("[已取消] 当前任务已终止\r\n")
				}
			} else {
				repl.PrintAbove("[提示] 当前没有运行中的任务\r\n")
			}
			continue

		case input == "/tasks":

			repl.PrintAbove(formatTaskSnapshot(tasks.Snapshot()))
			continue

		case input == "/help":

			printHelp(cmdReg, toolReg, pluginMgr)

		case input == "/":

			showQuickCommands(allCommands)

			continue

		case input == "/doctor":

			runDoctor()

		case input == "/attach" || strings.HasPrefix(input, "/attach "):

			cwd, _ := os.Getwd()

			handleAttachCommand(input, cwd, &attachedFiles)

		case input == "/skill" || strings.HasPrefix(input, "/skill ") || input == "/skills" || strings.HasPrefix(input, "/skills "):

			handleSkill(input, eng)
			continue

		case handleBuiltinConfigCommand(input, cfg, eng, pm, as):

			continue

		case handleSessionCommand(input, eng, &historyPickPending):

			continue

		case strings.HasPrefix(input, "/"):

			if isSkillInvocation(input, eng) {
				handleSkillInvocation(input, eng)
				continue
			}

			if handlePluginCommand(input, pluginMgr, tasks) {

				continue

			}

			if handleUnknownCmd(input, cmdReg) {

				continue

			}

			withInterrupt(func(ctx context.Context) {

				handleCommand(ctx, input, cmdReg, cfg, eng, mcpPool, skillMgr, memStore, pluginMgr, pm, projCtx, as)

			})

		default:

			pc := cfg.EffectiveProvider()

			if eng.CostTracker() != nil && eng.CostTracker().OverBudget() {

				repl.PrintAbove(budgetExceededRetryHint(eng.CostTracker()) + "\r\n")

				continue

			}

			if pc.APIKey == "" {

				repl.PrintAbove(missingAPIKeyMessage(pc.Name) + "\r\n")

				continue

			}

			cwd, _ := os.Getwd()

			userMsg, warnings, err := buildUserMessage(input, cwd, attachedFiles, cfg.Model)

			if err != nil {

				fmt.Printf("警告: 构建用户消息时出错: %v\n", err)

				continue

			}

			if shouldAutoSwitchToVision(warnings) {

				if visionModel := preferredVisionModelForProvider(pc.Name, cfg.Model); visionModel != "" && visionModel != cfg.Model {

					if err := applyProviderConfigChange(cfg, eng, func() error {

						cfg.Model = visionModel

						as.Model = visionModel

						return nil

					}); err == nil {

						fmt.Printf("[视觉] 检测到图片附件，已自动切换到视觉模型 %s。\n", visionModel)

						userMsg, warnings, err = buildUserMessage(input, cwd, attachedFiles, cfg.Model)

						if err != nil {

							fmt.Printf("警告: 构建用户消息时出错: %v\n", err)

							continue

						}

					}

				}

			}

			// Print warnings (e.g., non-vision model with image)

			for _, w := range warnings {

				repl.PrintAbove(fmt.Sprintf("  \x1b[33m%s\x1b[0m\r\n", w))

			}

			// Auto-clear attachments after sending (avoids resending images every turn)

			if len(attachedFiles) > 0 {

				attachedFiles = nil

			}

			queuedAhead, merged := tasks.Enqueue(userMsg)

			if merged {

				if queuedAhead > 0 {

					repl.PrintAbove(fmt.Sprintf("[已补充] 先前任务还在跑，前面排队: %d\r\n", queuedAhead))

				} else {

					repl.PrintAbove("[已补充] 已合并进当前处理任务\r\n")

				}

			} else if queuedAhead > 0 {

				repl.PrintAbove(fmt.Sprintf("[任务排队中] 前方排队数: %d\r\n", queuedAhead))

			} else if !tasks.IsRunning() {

				// Brief feedback only when this is the first and only task
				repl.PrintAbove("[输入已接收]\r\n")

			}

			// Don't block: tasks run in the background, user can type again immediately.

		}

	}

}

// handlePluginCommand checks whether the slash command matches a command
// provided by an enabled plugin. If so, it injects the command's prompt body
// (plus any trailing arguments) into the engine as a user message and returns
// true. Returns false when no plugin command matches.
func handlePluginCommand(input string, pluginMgr *plugin.Manager, tasks *replTaskRunner) bool {
	if pluginMgr == nil {
		return false
	}
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return false
	}
	name := strings.TrimPrefix(parts[0], "/")
	cmds := pluginMgr.CommandPrompts()
	cmd, ok := cmds[name]
	if !ok {
		return false
	}
	prompt := cmd.Prompt
	if args := strings.TrimSpace(strings.TrimPrefix(input, parts[0])); args != "" {
		prompt = prompt + "\n\n" + args
	}
	repl.PrintAbove(fmt.Sprintf("[插件命令: /%s (%s)]\r\n", name, cmd.Plugin))
	_, merged := tasks.Enqueue(api.Message{Role: "user", Content: prompt})
	if merged {
		repl.PrintAbove("[已补充] 已合并进当前处理任务\r\n")
	} else {
		repl.PrintAbove("[输入已接收]\r\n")
	}
	return true
}

func isSkillInvocation(input string, eng *engine.Engine) bool {

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

func handleSkillInvocation(input string, eng *engine.Engine) {

	parts := strings.Fields(input)

	if len(parts) == 0 {

		return

	}

	name := strings.TrimPrefix(parts[0], "/")

	prompt, ok := eng.Runtime().SkillPrompts[name]

	if !ok {

		repl.PrintSafe("未找到配置文件: %s\n", name)

		return

	}

	args := strings.TrimSpace(strings.TrimPrefix(input, parts[0]))

	repl.PrintSafe("\n[Skill: %s]\n\n%s\n", name, prompt)

	if args != "" {

		repl.PrintSafe("\n无效的参数: %s\n", args)

	}

	repl.PrintSafe("\n")

}
