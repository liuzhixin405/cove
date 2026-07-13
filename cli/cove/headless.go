package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/command"
	"github.com/liuzhixin405/cove/internal/config"
	ctxt "github.com/liuzhixin405/cove/internal/context"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/mcp"
	"github.com/liuzhixin405/cove/internal/memory"
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/plugin"
	"github.com/liuzhixin405/cove/internal/skills"
	"github.com/liuzhixin405/cove/internal/state"
	"github.com/liuzhixin405/cove/internal/tool"
)

// runHeadless is the non-interactive frontend used when stdin/stdout is not a
// terminal (pipes, redirects) or when the TUI is explicitly disabled
// (--no-tui / COVE_TUI=0). It replaces the classic line REPL for these cases:
// it reads commands/prompts line-by-line from stdin and runs them synchronously,
// writing engine output to stdout and diagnostics to stderr. There is no
// alternate screen, raw-mode reader, or task queue — output is script-friendly.
func runHeadless(bannerText string, eng *engine.Engine, cmdReg *command.Registry, toolReg *tool.Registry, pm *permission.Manager, as *state.AppState, cfg *config.Config, mcpPool *mcp.Pool, skillMgr *skills.Manager, memStore *memory.Store, pluginMgr *plugin.Manager, projCtx *ctxt.ProjectContext) {
	// Banner goes to stderr so stdout carries only assistant/command output.
	if strings.TrimSpace(bannerText) != "" {
		fmt.Fprint(os.Stderr, bannerText)
	}

	scanner := bufio.NewScanner(os.Stdin)
	// Allow long single-line inputs (e.g. pasted prompts) up to 8 MiB.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var attachedFiles []string
	historyPickPending := false

	for scanner.Scan() {
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if historyPickPending && isPositiveNumber(input) {
			handleHistoryResume(input, eng)
			historyPickPending = false
			continue
		}
		if input == "exit" || input == "/exit" || input == "quit" || input == "/quit" {
			break
		}

		if strings.HasPrefix(input, "/") {
			switch {
			case input == "/tasks":
				fmt.Println("headless 模式按行同步执行，不维护后台任务队列。")
				continue
			case input == "/stop" || input == "/cancel":
				fmt.Println("headless 模式当前没有可取消的后台任务。")
				continue
			case input == "/attach" || strings.HasPrefix(input, "/attach "):
				cwd, _ := os.Getwd()
				handleAttachCommand(input, cwd, &attachedFiles)
				continue
			case input == "/help":
				printHelp(cmdReg, toolReg, pluginMgr)
				continue
			case input == "/doctor":
				runDoctor()
				continue
			case input == "/skill" || strings.HasPrefix(input, "/skill ") || input == "/skills" || strings.HasPrefix(input, "/skills "):
				handleSkill(input, eng)
				continue
			case handleBuiltinConfigCommand(input, cfg, eng, pm, as):
				continue
			case handleSessionCommand(input, eng, &historyPickPending):
				continue
			}

			// Skill invocation: bare "/<skillname>".
			if tuiIsSkillInvocation(input, eng) {
				fmt.Println(tuiSkillInvocationText(input, eng))
				continue
			}
			// Plugin command: run its prompt body as an engine turn.
			if prompt, label, ok := tuiPluginCommandPrompt(input, pluginMgr); ok {
				fmt.Fprintf(os.Stderr, "[插件命令: /%s]\n", label)
				runHeadlessTurn(eng, api.Message{Role: "user", Content: prompt})
				continue
			}
			// Unknown command → fuzzy suggestions.
			if _, known := cmdReg.Find(strings.TrimPrefix(strings.Fields(input)[0], "/")); !known {
				handleUnknownCmd(input, cmdReg)
				continue
			}
			// Registered command.
			withInterrupt(func(ctx context.Context) {
				handleCommand(ctx, input, cmdReg, cfg, eng, mcpPool, skillMgr, memStore, pluginMgr, pm, projCtx, as)
			})
			continue
		}

		// Normal prompt → engine turn.
		pc := cfg.EffectiveProvider()
		if eng.CostTracker() != nil && eng.CostTracker().OverBudget() {
			fmt.Fprintln(os.Stderr, budgetExceededRetryHint(eng.CostTracker()))
			continue
		}
		if pc.APIKey == "" {
			fmt.Fprintln(os.Stderr, missingAPIKeyMessage(pc.Name))
			continue
		}

		cwd, _ := os.Getwd()
		userMsg, warnings, err := buildUserMessage(input, cwd, attachedFiles, cfg.Model)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			continue
		}

		// Auto-switch to a vision model when an image attachment is detected but
		// the current model can't see it (parity with the classic REPL).
		if shouldAutoSwitchToVision(warnings) {
			if visionModel := preferredVisionModelForProvider(pc.Name, cfg.Model); visionModel != "" && visionModel != cfg.Model {
				if switchErr := applyProviderConfigChange(cfg, eng, func() error {
					cfg.Model = visionModel
					as.Model = visionModel
					return nil
				}); switchErr == nil {
					fmt.Fprintf(os.Stderr, "[视觉] 检测到图片附件，已自动切换到视觉模型 %s。\n", visionModel)
					userMsg, warnings, err = buildUserMessage(input, cwd, attachedFiles, cfg.Model)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Error: %v\n", err)
						continue
					}
				}
			}
		}
		for _, w := range warnings {
			fmt.Fprintf(os.Stderr, "⚠ %s\n", w)
		}
		// Clear one-shot attachments after sending (avoids resending each turn).
		attachedFiles = nil

		runHeadlessTurn(eng, userMsg)
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "stdin 读取错误: %v\n", err)
	}
}

// runHeadlessTurn drives a single engine turn synchronously, printing the reply
// to stdout. SIGINT/SIGTERM cancel the in-flight turn instead of killing the
// process outright.
func runHeadlessTurn(eng *engine.Engine, userMsg api.Message) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() { <-sigCh; cancel() }()

	resp, err := eng.RunMessageWithStream(ctx, userMsg, nil, nil)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "[已取消] 当前任务已终止")
		} else {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return
	}
	fmt.Println(resp)
	if eng.HasMessages() {
		eng.SaveSession()
	}
}
