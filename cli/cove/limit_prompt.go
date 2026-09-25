package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/liuzhixin405/cove/internal/config"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/repl"
	"github.com/liuzhixin405/cove/internal/termui"
)

// limitPromptTimeout bounds how long a turn waits at its limit prompt; an
// unanswered prompt stops the turn (it stays resumable with /continue). A
// variable so tests can shorten it.
var limitPromptTimeout = 15 * time.Minute

// installLimitPrompt wires eng.IterationLimitPrompt for a front end whose
// input loop relays answer lines through repl.TakePermInputCh (the same relay
// the permission prompt uses). Without it the engine stops at the limits.
func installLimitPrompt(eng *engine.Engine) {
	if eng == nil {
		return
	}
	eng.IterationLimitPrompt = askTurnLimit
}

// askTurnLimit shows the limit box and waits for "c" (go on for one more
// window) or anything else (stop). A non-interactive run always stops.
func askTurnLimit(stats engine.LimitStats) engine.LimitDecision {
	if !replInteractive {
		return engine.LimitStop
	}
	answerCh := make(chan string, 1)
	repl.SetPermInputCh(answerCh)
	repl.BeginPromptInput()
	termui.PrintAbove(limitPromptText(stats))

	timer := time.NewTimer(limitPromptTimeout)
	defer timer.Stop()

	var answer string
	select {
	case answer = <-answerCh:
	case <-timer.C:
		repl.EndPromptInput()
		repl.ClearPermInputCh()
		// The relay is gone: a "c" typed from now on is an ordinary message.
		termui.PrintAbove("  " + termui.Styled(termui.Yellow, "等待超时，已按停止处理（此后单独输入 c 会作为新消息发送）") + "\n")
		return engine.LimitStop
	}
	repl.EndPromptInput()
	repl.ClearPermInputCh()

	// A stop needs no line of its own here: the turn then ends with the limit
	// error, whose line says /continue resumes it (turnErrorLine).
	return limitAnswerDecision(answer)
}

// limitPromptText renders the box: why the turn paused, what it has done so
// far (model calls, time, cost, the last few tools) and the two answers.
func limitPromptText(s engine.LimitStats) string {
	var title, cont string
	switch s.Reason {
	case engine.LimitReasonTime:
		title = fmt.Sprintf("本轮已运行超过 %d 分钟", s.Window)
		cont = fmt.Sprintf("[c] 继续 %d 分钟", s.Window)
	case engine.LimitReasonStagnation:
		title = "连续多轮没有新的文件读写，任务可能卡住了"
		cont = "[c] 继续"
	case engine.LimitReasonLoop:
		title = "检测到重复操作，模型可能在原地打转"
		cont = "[c] 本轮禁用循环检测并继续"
	default:
		title = fmt.Sprintf("本轮已调用模型 %d 次，达到单轮上限", s.Iterations)
		cont = fmt.Sprintf("[c] 继续 %d 次", s.Window)
	}
	var b strings.Builder
	b.WriteString("\n  " + termui.Styled(termui.Yellow, "⏸ "+title) + "\n")
	fmt.Fprintf(&b, "  已调用模型 %d 次 · 用时 %s · 本轮费用 $%.4f\n",
		s.Iterations, s.Elapsed.Truncate(time.Second), s.Cost)
	if len(s.RecentSteps) > 0 {
		b.WriteString("  最近步骤: " + strings.Join(s.RecentSteps, " → ") + "\n")
	}
	if s.Detail != "" {
		b.WriteString("  " + s.Detail + "\n") // what the loop detector saw
	}
	b.WriteString("  " + termui.Styled(termui.Bold, cont+"  [s] 停止") + "\n")
	return b.String()
}

// limitAnswerDecision maps an answer line to a decision. Anything that is
// not a clear "go on" stops, so a stray keypress does not extend the run.
func limitAnswerDecision(answer string) engine.LimitDecision {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "c", "continue", "继续", "y", "yes":
		return engine.LimitContinue
	default:
		return engine.LimitStop
	}
}

// interactiveErrorText is how the shell reports a turn error: a limit stop
// says the turn can be resumed with /continue and which setting moves the
// limit; other errors are unchanged.
func interactiveErrorText(err error) string {
	var le *engine.LimitError
	if !errors.As(err, &le) {
		return err.Error()
	}
	switch le.Reason {
	case engine.LimitReasonIterations:
		return fmt.Sprintf("已达到单轮最大迭代次数 %d。输入 /continue 可继续；可通过配置 max_iterations 调整", le.Limit)
	case engine.LimitReasonTime:
		return fmt.Sprintf("已达到单轮时间上限 %d 分钟。输入 /continue 可继续；可通过配置 max_turn_minutes 调整（0 为不限制）", le.Limit)
	default:
		return le.Error() + "。输入 /continue 可继续"
	}
}

// printModeErrorText is how -p reports a turn error: the iteration cap
// points at --max-turns; there is no /continue in a one-shot run.
func printModeErrorText(err error) string {
	var le *engine.LimitError
	if errors.As(err, &le) && le.Reason == engine.LimitReasonIterations {
		return fmt.Sprintf("已达到单轮最大迭代次数 %d（可用 --max-turns 调整）", le.Limit)
	}
	return err.Error()
}

// unattendedLimiter is the part of *engine.Engine applyUnattendedLimits sets.
type unattendedLimiter interface {
	SetMaxIterations(n int)
	SetMaxTurnMinutes(n int)
}

// applyUnattendedLimits sets the hard limits of a run nobody can answer a
// limit prompt in (-p, headless): the iteration cap (--max-turns, else
// max_iterations) and the time limit, which applies there only when the user
// configured max_turn_minutes explicitly.
func applyUnattendedLimits(l unattendedLimiter, opts cliOptions, cfg *config.Config) {
	l.SetMaxIterations(printModeIterationLimit(opts, cfg.MaxIterations))
	l.SetMaxTurnMinutes(cfg.UnattendedTurnMinutes())
}

// printModeIterationLimit is the -p turn's model-call cap: --max-turns when
// given (0 = none), else the configured max_iterations.
func printModeIterationLimit(opts cliOptions, configured int) int {
	if !opts.maxTurnsSet {
		return configured
	}
	if opts.maxTurns == 0 {
		return engine.UnlimitedIterations
	}
	return opts.maxTurns
}
