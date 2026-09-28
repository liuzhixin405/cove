package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/command"
	"github.com/liuzhixin405/cove/internal/config"
	ctxt "github.com/liuzhixin405/cove/internal/context"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/mcp"
	"github.com/liuzhixin405/cove/internal/memory"
	"github.com/liuzhixin405/cove/internal/plugin"
	"github.com/liuzhixin405/cove/internal/repl"
	"github.com/liuzhixin405/cove/internal/skills"
	"github.com/liuzhixin405/cove/internal/tool"
)

// frontend is what the slash commands of an interactive or headless front
// end work on. Both front ends dispatch every slash command through one
// command.Registry (dispatch): the generic commands of internal/command and
// the front-end commands below, which close over this state. There used to
// be three mechanisms — a switch in each front end's loop, two handler
// functions that intercepted lines before the registry, and the registry
// itself — with the command list written out five times (the switches, the
// handlers, the registry, the completion table, the "must not run while a
// task runs" list), and the two front ends had drifted apart: headless let
// a plugin command replace /status, and did not know /continue or /clear.
type frontend struct {
	eng       *engine.Engine
	cfg       *config.Config
	toolReg   *tool.Registry
	mcpPool   *mcp.Pool
	skillMgr  *skills.Manager
	memStore  *memory.Store
	pluginMgr *plugin.Manager
	projCtx   *ctxt.ProjectContext
	reg       *command.Registry

	// tasks is the interactive front end's task runner; nil in headless,
	// which runs each line synchronously.
	tasks *replTaskRunner

	attachedFiles []string
	// historyPickPending: /history listed sessions, so a bare number picks
	// one.
	historyPickPending bool
	// freshFromNew: /new emptied the conversation and nothing was sent since.
	freshFromNew bool
	// exitRequested is set by /exit; the loop leaves after the command.
	exitRequested bool

	// print shows a notice line; enqueue runs a message as a task (queued
	// in the REPL, synchronously in headless).
	print   func(string)
	enqueue func(api.Message)
}

func (fe *frontend) interactive() bool { return fe.tasks != nil }

func (fe *frontend) running() bool { return fe.tasks != nil && fe.tasks.IsRunning() }

// feCmd is a front-end command. run gets the line as typed. A command that
// overlays a generic one of the same name (base) handles what it knows and
// hands the rest to base: bare /config shows the configuration, /config
// <key> <value> is the generic command.
type feCmd struct {
	name, desc, help, category string
	aliases                    []string
	hints                      []string
	mutates                    func(args []string) bool
	run                        func(ctx context.Context, in command.Input) (handled bool)
	base                       command.Command
}

func (c *feCmd) Name() string        { return c.name }
func (c *feCmd) Aliases() []string   { return c.aliases }
func (c *feCmd) Description() string { return c.desc }
func (c *feCmd) Help() string {
	if c.help != "" {
		return c.help
	}
	return "/" + c.name + " - " + c.desc
}
func (c *feCmd) Category() string   { return c.category }
func (c *feCmd) ArgHints() []string { return c.hints }
func (c *feCmd) MutatesEngine(args []string) bool {
	if c.mutates != nil {
		return c.mutates(args)
	}
	return c.base != nil && command.Mutates(c.base, args)
}
func (c *feCmd) Execute(ctx context.Context, in command.Input) (command.Output, error) {
	if c.run(ctx, in) || c.base == nil {
		return command.Output{}, nil
	}
	return c.base.Execute(ctx, in)
}

func always([]string) bool        { return true }
func withArgs(args []string) bool { return len(args) > 0 }

// Help sections, in the order /help shows them.
const (
	catModel   = "供应商 / 模型"
	catSession = "会话"
	catTasks   = "后台任务"
	catSystem  = "系统"
)

// helpCategories is the /help order; commands without a category come last,
// under "命令".
var helpCategories = []string{catModel, catSession, catTasks, catSystem}

// install registers fe's commands over the generic ones in reg and makes
// reg the registry fe dispatches through.
func (fe *frontend) install(reg *command.Registry) *command.Registry {
	fe.reg = reg
	base := func(name string) command.Command {
		c, _ := reg.Find(name)
		return c
	}
	config := func(ctx context.Context, in command.Input) bool {
		return handleBuiltinConfigCommand(in.Raw, fe.cfg, fe.eng)
	}
	usage := func(text string) func(context.Context, command.Input) bool {
		return func(ctx context.Context, in command.Input) bool {
			if handleBuiltinConfigCommand(in.Raw, fe.cfg, fe.eng) {
				return true
			}
			fe.print("用法: " + text)
			return true
		}
	}
	session := func(ctx context.Context, in command.Input) bool {
		return handleSessionCommand(in.Raw, fe.eng, &fe.historyPickPending)
	}

	for _, c := range []*feCmd{
		// Provider and model.
		{name: "model", desc: "设置模型", category: catModel, mutates: withArgs, run: usage("/model <名称>")},
		{name: "profile", desc: "管理配置档案 (list/switch/save/delete/show)", category: catModel,
			hints: []string{"list", "switch", "save", "delete", "show"},
			mutates: func(a []string) bool {
				return len(a) > 0 && (a[0] == "switch" || a[0] == "use" || a[0] == "delete")
			}, run: config},
		{name: "provider", desc: "设置供应商", category: catModel, hints: providerNameSuggestions(), mutates: withArgs,
			run: func(ctx context.Context, in command.Input) bool {
				if !handleBuiltinConfigCommand(in.Raw, fe.cfg, fe.eng) {
					fe.print("用法: /provider <名称>\n" + providerHelpLine())
				}
				return true
			}},
		{name: "api-key", desc: "设置 API 密钥", category: catModel, mutates: withArgs, run: usage("/api-key <密钥>")},
		{name: "base-url", desc: "设置 API 地址", category: catModel, run: usage("/base-url <地址>")},
		{name: "mode", desc: "设置权限模式 (default|plan|auto|bypass)", category: catModel,
			hints: []string{"default", "plan", "auto", "bypass"},
			run: func(ctx context.Context, in command.Input) bool {
				if handleBuiltinConfigCommand(in.Raw, fe.cfg, fe.eng) {
					return true
				}
				fe.print(fmt.Sprintf("当前模式: %s（可选: %s）", fe.eng.PermissionMode(), "default|plan|auto|bypass"))
				return true
			}},
		{name: "budget", desc: "本会话预算上限 ($)；save 写入配置", category: catModel, run: config},
		{name: "record", desc: "录制会话事件 (status/start/stop)", category: catModel,
			hints: []string{"status", "start", "stop"}, run: config},
		{name: "config", desc: "查看完整配置；/config <键> <值> 修改", category: catModel, base: base("config"), run: config},
		{name: "cost", desc: "查看用量和费用", category: catModel, base: base("cost"), run: config},

		// Session.
		{name: "new", desc: "保存当前会话并开始新会话（清空对话上下文）", category: catSession, mutates: always,
			run: func(ctx context.Context, in command.Input) bool {
				saved := startNewSession(fe.eng, fe.tasks, &fe.attachedFiles)
				fe.freshFromNew = true
				fe.print(newSessionNotice(saved))
				return true
			}},
		{name: "compact", desc: "压缩对话历史", category: catSession, mutates: always, base: base("compact"), run: session},
		{name: "history", desc: "查看和继续历史会话（clear/delete/detail/all）", category: catSession,
			hints:   []string{"clear", "delete", "clean", "detail", "all"},
			mutates: func(a []string) bool { return len(a) > 0 && isPositiveNumber(a[0]) },
			base:    base("history"), run: session},
		{name: "resume", desc: "恢复已保存的会话", category: catSession, mutates: withArgs, base: base("resume"), run: session},
		{name: "export", desc: "导出当前会话为 Markdown", category: catSession, base: base("export"), run: session},
		{name: "continue", desc: "从中断处继续上一轮", category: catSession,
			run: func(ctx context.Context, in command.Input) bool {
				if t := fe.eng.CostTracker(); t != nil && t.OverBudget() {
					fe.print(budgetExceededRetryHint(t))
					return true
				}
				fe.print(continueInterruptedTurn(fe.eng, fe.running(), func(msg api.Message) {
					if fe.tasks != nil {
						// The engine resumes the turn when its message is sent
						// again; the retry bookkeeping of "继续" is now stale.
						fe.tasks.ClearPendingFailed()
					}
					_ = clearInterruptedDraft()
					fe.enqueue(msg)
				}))
				return true
			}},
		{name: "attach", desc: "挂载图片或文件到后续提问（list/remove/clear）", category: catSession,
			hints: []string{"list", "clear", "remove", "add"},
			run: func(ctx context.Context, in command.Input) bool {
				cwd, _ := os.Getwd()
				handleAttachCommand(in.Raw, cwd, &fe.attachedFiles)
				return true
			}},

		// Background tasks.
		{name: "tasks", desc: "查看运行中/排队的后台任务", category: catTasks,
			run: func(ctx context.Context, in command.Input) bool {
				if fe.tasks == nil {
					fe.print("headless 模式按行同步执行，不维护后台任务队列。")
				} else {
					fe.print(strings.TrimRight(formatTaskSnapshot(fe.tasks.Snapshot()), "\r\n"))
				}
				return true
			}},
		{name: "stop", aliases: []string{"cancel"}, desc: "取消当前运行的任务", category: catTasks,
			run: func(ctx context.Context, in command.Input) bool {
				fe.stop()
				return true
			}},

		// System.
		{name: "help", desc: "显示帮助", category: catSystem,
			run: func(ctx context.Context, in command.Input) bool {
				printHelp(fe.reg, fe.toolReg, fe.pluginMgr)
				return true
			}},
		{name: "tools", desc: "列出可用工具", category: catSystem,
			run: func(ctx context.Context, in command.Input) bool {
				printTools(fe.toolReg, fe.pluginMgr)
				return true
			}},
		{name: "skill", aliases: []string{"skills"}, desc: "列出、查看或调用技能", category: catSystem,
			run: func(ctx context.Context, in command.Input) bool {
				handleSkill(in.Raw, fe.eng)
				return true
			}},
		{name: "doctor", desc: "检查运行环境", category: catSystem, base: base("doctor"),
			run: func(ctx context.Context, in command.Input) bool {
				if len(in.Args) > 0 {
					return false
				}
				runDoctor()
				return true
			}},
		{name: "clear", aliases: []string{"cls"}, desc: "清屏，不影响对话上下文（快捷键 Ctrl+L）", category: catSystem,
			run: func(ctx context.Context, in command.Input) bool {
				if !fe.interactive() {
					fe.print("headless 模式没有可清的屏幕。")
				} else if !repl.ClearScreen() {
					fe.print("[提示] 任务输出中，暂不清屏；任务结束后再试（对话上下文不受影响）")
				}
				return true
			}},
		{name: "exit", aliases: []string{"quit"}, desc: "退出", category: catSystem,
			run: func(ctx context.Context, in command.Input) bool {
				fe.exitRequested = true
				return true
			}},
	} {
		reg.Register(c)
	}
	return reg
}

// stop is /stop: cancel the running task and say when it is really gone.
func (fe *frontend) stop() {
	if fe.tasks == nil {
		fe.print("headless 模式当前没有可取消的后台任务。")
		return
	}
	if !fe.tasks.IsRunning() {
		fe.print("[提示] 当前没有运行中的任务")
		return
	}
	denyPendingPermissionPrompt()
	if fe.tasks.CancelRunning() {
		// Cancelling asks the task to stop; it is gone only once its
		// goroutine has returned, and saying "terminated" a moment early
		// made the next /continue answer "still running".
		if fe.tasks.WaitIdleUntil(time.Now().Add(1500 * time.Millisecond)) {
			fe.print("[已取消] 当前任务已终止，输入 /continue 可从中断处继续")
		} else {
			fe.print("[已中断] 正在停止当前任务…结束后可用 /continue 继续")
		}
	}
}

// mutates reports whether the slash line input must wait while a task runs.
func (fe *frontend) mutates(input string) bool {
	fields := strings.Fields(input)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return false
	}
	c, ok := fe.reg.Find(strings.TrimPrefix(fields[0], "/"))
	return ok && command.Mutates(c, fields[1:])
}

// dispatch runs the slash command line input and reports whether input was
// one. A built-in command wins over a skill or plugin command of the same
// name (and says so), so a plugin cannot replace /config or /permissions.
func (fe *frontend) dispatch(input string) bool {
	if !strings.HasPrefix(input, "/") {
		return false
	}
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return false
	}
	name := strings.TrimPrefix(fields[0], "/")
	if fe.running() && fe.mutates(input) {
		fe.print(fmt.Sprintf("[提示] 任务运行中不能执行 %s：它会改写正在使用的会话状态。请等任务结束，或先 /stop。", fields[0]))
		return true
	}
	// A number right after /history picks from its list; any command in
	// between ends that (and /history starts it again).
	fe.historyPickPending = false

	var skillPrompts map[string]string
	if fe.eng != nil && fe.eng.Runtime() != nil {
		skillPrompts = fe.eng.Runtime().SkillPrompts
	}
	var pluginCmds map[string]plugin.CommandPrompt
	if fe.pluginMgr != nil {
		pluginCmds = fe.pluginMgr.CommandPrompts()
	}
	target, shadowed := resolveSlashCommand(name, fe.reg, skillPrompts, pluginCmds)
	switch target {
	case slashSkill:
		if fe.interactive() {
			handleSkillInvocation(input, fe.eng)
		} else {
			outln(skillInvocationText(input, fe.eng))
		}
		return true
	case slashPlugin:
		if fe.interactive() {
			handlePluginCommand(input, fe.pluginMgr, fe.tasks)
		} else if prompt, label, ok := pluginCommandPrompt(input, fe.pluginMgr); ok {
			fmt.Fprintf(os.Stderr, "[插件命令: /%s]\n", label)
			fe.enqueue(api.Message{Role: "user", Content: prompt})
		}
		return true
	case slashUnknown:
		handleUnknownCmd(input, fe.reg)
		return true
	}
	if shadowed != "" {
		fe.print(fmt.Sprintf("[提示] %s 与内置命令同名，已执行内置命令 /%s", shadowed, name))
	}
	withInterrupt(func(ctx context.Context) {
		fe.execute(ctx, input)
	})
	return true
}

// commandMutatesEngine reports whether the typed line input is a command
// that must not run while a task runs, from the commands' own metadata.
func commandMutatesEngine(input string) bool {
	fe := &frontend{}
	fe.install(registerAllCommands())
	return fe.mutates(input)
}
