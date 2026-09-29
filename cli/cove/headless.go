package main

import (
	"bufio"
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
	"github.com/liuzhixin405/cove/internal/dream"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/log"
)

// runHeadless is the non-interactive frontend used when stdin/stdout is not a
// terminal (pipes, redirects) or when the TUI is explicitly disabled
// (--no-tui / COVE_TUI=0). It replaces the classic line REPL for these cases:
// it reads commands/prompts line-by-line from stdin and runs them synchronously,
// writing engine output to stdout and diagnostics to stderr. There is no
// alternate screen, raw-mode reader, or task queue — output is script-friendly.
func runHeadless(app *appBootstrap, cmdReg *command.Registry, bannerText string) {
	eng, toolReg, cfg, mcpPool := app.eng, app.toolReg, app.cfg, app.mcpPool
	skillMgr, memStore, pluginMgr, projCtx := app.skillMgr, app.memStore, app.pluginMgr, app.projCtx
	// Headless runs exit when stdin ends, so a skill review started on the last
	// turn would be abandoned mid-request like in -p; skip it here too.
	if eng != nil {
		eng.SetNonInteractive(true)
		wireNonInteractiveOutput(eng)
	}
	// Banner goes to stderr so stdout carries only assistant/command output.
	if strings.TrimSpace(bannerText) != "" {
		fmt.Fprint(os.Stderr, bannerText)
	}

	scanner := bufio.NewScanner(os.Stdin)
	// Allow long single-line inputs (e.g. pasted prompts) up to 8 MiB.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	// The same command registry and dispatch as the REPL (frontend.go); what
	// differs is that each line runs synchronously and notices go to stdout.
	fe := &frontend{eng: eng, cfg: cfg, toolReg: toolReg, mcpPool: mcpPool, skillMgr: skillMgr,
		memStore: memStore, pluginMgr: pluginMgr, projCtx: projCtx,
		print:   func(s string) { outln(s) },
		enqueue: func(msg api.Message) { runHeadlessTurn(eng, msg) },
	}
	cmdReg = fe.install(cmdReg)

	first := true
	for scanner.Scan() {
		line := scanner.Text()
		if first {
			// Windows PowerShell pipes text into a program with a UTF-8
			// byte-order mark: "/context" arrived as BOM + "/context", was
			// not a command, and went to the model as a message.
			line, first = strings.TrimPrefix(line, utf8BOM), false
		}
		input := strings.TrimSpace(line)
		if input == "" {
			continue
		}
		if fe.historyPickPending && isPositiveNumber(input) {
			handleHistoryResume(input, eng)
			fe.historyPickPending = false
			continue
		}
		if input == "exit" || input == "quit" {
			break
		}
		if fe.dispatch(input) {
			if fe.exitRequested {
				break
			}
			continue
		}

		// Normal prompt → engine turn.
		pc := cfg.EffectiveProvider()
		if eng.CostTracker() != nil && eng.CostTracker().OverBudget() {
			fmt.Fprintln(os.Stderr, budgetExceededRetryHint(eng.CostTracker()))
			continue
		}
		if runNeedsAPIKey(pc.APIKey, replayDir != "") {
			fmt.Fprintln(os.Stderr, missingAPIKeyMessage(pc.Name))
			continue
		}

		cwd, _ := os.Getwd()
		userMsg, warnings, err := buildUserMessage(input, cwd, fe.attachedFiles, cfg.Model)
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
					return nil
				}); switchErr == nil {
					fmt.Fprintf(os.Stderr, "[视觉] 检测到图片附件，已自动切换到视觉模型 %s。\n", visionModel)
					userMsg, warnings, err = buildUserMessage(input, cwd, fe.attachedFiles, cfg.Model)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Error: %v\n", err)
						continue
					}
				}
			}
		}
		for _, w := range warnings {
			fmt.Fprintln(os.Stderr, w) // the warning carries its own ⚠
		}
		// Clear one-shot attachments after sending (avoids resending each turn).
		fe.attachedFiles = nil

		runHeadlessTurn(eng, userMsg)
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "stdin 读取错误: %v\n", err)
	}
}

// printModeBackgroundWait bounds how long cove -p waits, after the answer,
// for the engine's background work (memory extraction) before exiting. A -p
// process used to exit the moment the answer was printed, so the extraction
// started at the end of the turn was always killed and -p never learned.
const printModeBackgroundWait = 20 * time.Second

// backgroundWaiter is the engine's WaitBackground (waits for its background
// goroutines, returning early when ctx ends). Asserted rather than called
// directly so this builds against an engine that does not have it yet.
type backgroundWaiter interface {
	WaitBackground(ctx context.Context)
}

// waitForBackground waits for v's background work up to limit and reports
// whether v supports waiting at all. The limit is enforced here, not left to
// WaitBackground: a waiter that ignores its context still cannot hold the
// exit up (its goroutine is simply abandoned; the process is exiting).
func waitForBackground(v any, limit time.Duration) bool {
	w, ok := v.(backgroundWaiter)
	if !ok || w == nil {
		log.Debugf("[-p] engine has no WaitBackground; exiting without waiting for background work")
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.WaitBackground(ctx)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		log.Debugf("[-p] background work still running after %v; exiting anyway", limit)
	}
	return true
}

// runPrintModeSession is the whole -p run: the turn (runPrintMode), then the
// wrap-up every -p exit owes — waiting for memory extraction, then
// finishSession. Automatic dream is switched off for the process first: its
// run would be killed at exit with the consolidation lock already stamped, so
// the sessions it was reviewing would count as consolidated.
func runPrintModeSession(eng *engine.Engine, argPrompt, prompt string, debug bool, attachmentPaths []string, cfg *config.Config, pool interface{ DisconnectAll() }) int {
	dream.SuppressAuto("-p 模式：回答后进程立即退出，整理会被中途终止")
	log.Debugf("[autoDream] skipped for this process: -p mode exits right after the answer")
	if eng != nil {
		// The skill review would be abandoned at exit after its paid call.
		eng.SetNonInteractive(true)
		wireNonInteractiveOutput(eng)
	}
	code := runPrintMode(eng, argPrompt, prompt, debug, attachmentPaths, cfg)
	if eng != nil {
		waitForBackground(eng, printModeBackgroundWait)
	}
	finishSession(eng, pool)
	return code
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
		if s := eng.LastWrapUp(); s != "" {
			outln(s) // the stopped turn's no-tool summary
		}
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "[已取消] 当前任务已终止")
		} else {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return
	}
	noteTurnCompleted()
	outln(resp)
	if eng.HasMessages() {
		eng.SaveSession()
	}
}
