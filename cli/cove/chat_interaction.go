package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/cost"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/render"
	"github.com/liuzhixin405/cove/internal/termui"
)

func isTransientRequestError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	transientHints := []string{
		"timeout", "timed out", "awaiting response headers", "deadline exceeded",
		"connection reset", "broken pipe", "connection refused", "eof",
		"temporary", "temporarily unavailable", "server error 5", "bad gateway",
	}
	for _, h := range transientHints {
		if strings.Contains(s, h) {
			return true
		}
	}
	return false
}

func runChatInteraction(ctx context.Context, runner chatRunner, input string) (string, error) {
	return runChatInteractionMessage(ctx, runner, api.Message{Role: "user", Content: input})
}

func runChatInteractionMessage(ctx context.Context, runner chatRunner, userMsg api.Message) (string, error) {
	termui.BeginOutput()
	defer termui.EndOutput()
	var totalOutput strings.Builder
	var finalErr error
	var reply string

	maxAttempts := 3
	p := newTurnPrinter()
	defer p.stop()
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		p.beginAttempt()

		if eng, ok := runner.(*engine.Engine); ok {
			eng.OnPermissionPause = p.permissionPause
			eng.OnPermissionDone = p.permissionDone
			// Surface live output from long-running tools (bash/powershell) so the
			// user can tell what a slow command is actually doing instead of only
			// seeing the stall warning.
			eng.OnToolProgress = p.toolProgress
			// Route engine diagnostic lines (tool start/finish, stall warnings,
			// memory/skill extraction notices) through the printer so they appear
			// in the conversation area.
			eng.OnEngineOutput = p.engineLine
			defer func() {
				eng.OnPermissionPause = nil
				eng.OnPermissionDone = nil
				eng.OnToolProgress = nil
				eng.OnEngineOutput = nil
			}()
		}

		var err error
		onDelta := func(delta string) {
			p.delta(delta)
			totalOutput.WriteString(delta)
		}
		if richRunner, ok := runner.(interface {
			RunMessageWithStream(context.Context, api.Message, func(string), func(string)) (string, error)
		}); ok {
			reply, err = richRunner.RunMessageWithStream(ctx, userMsg, onDelta, p.reasoning)
		} else {
			if len(userMsg.Parts) > 0 {
				err = fmt.Errorf("当前运行器不支持附件消息")
			} else {
				reply, err = runner.RunWithStream(ctx, userMsg.Content, onDelta)
			}
		}
		p.stopSpinner()

		if err == nil {
			finalErr = nil
			break
		}
		finalErr = err
		// Retrying is safe even after part of the answer streamed: the engine
		// keeps the steps it completed and resumes when the same message is
		// sent again, instead of re-running the turn from the start.
		if attempt == maxAttempts || ctx.Err() != nil || !isTransientRequestError(err) {
			break
		}
		note := fmt.Sprintf("\n网络波动，自动重试中 (%d/%d)...\n", attempt, maxAttempts)
		if p.gotDelta() {
			note = fmt.Sprintf("\n网络波动，从中断处继续 (%d/%d)...\n", attempt, maxAttempts)
		}
		p.system(termui.Yellow + note + termui.Reset)
		totalOutput.WriteString(note)
		time.Sleep(time.Duration(attempt) * 1200 * time.Millisecond)
	}

	if finalErr == nil {
		if missing := missingStreamedSuffix(reply, totalOutput.String()); missing != "" {
			p.delta(missing)
			totalOutput.WriteString(missing)
		}
	}
	if finalErr != nil {
		errMsg := fmt.Sprintf("\nRequest failed: %s", p.errorText(finalErr))
		p.system(termui.Red + errMsg + termui.Reset)
		totalOutput.WriteString(errMsg)
	}
	totalOutput.WriteString("\r\n\r\n")
	return totalOutput.String(), finalErr
}

// turnPrinter owns the terminal while one turn streams: the spinner, the
// model's text and reasoning, live tool output and the engine's tool blocks
// all come through it, from the engine's goroutines.
//
// It exists because those four sources used to write independently and
// trampled each other:
//
//   - A tool block arriving while the "思考中" spinner animated was printed
//     after the spinner frame, on the same row ("⠋ 思考中...  ▸ bash ls").
//   - The model's "我来看看文件：" has no trailing newline, so the tool block
//     that follows was glued to it, and so was the first answer delta after a
//     shown reasoning trace.
//   - Everything was printed verbatim, so an escape sequence in the model's
//     reply or in a command's output was executed by the terminal (see
//     render.SanitizeStream).
//
// So it tracks which source printed last and whether the cursor is mid-row,
// breaks the row when the source changes, keeps the spinner off any row that
// holds text (a spinner frame starts with \r ESC[K and would erase it), and
// sanitises every untrusted chunk.
type turnPrinter struct {
	mu             sync.Mutex
	spinner        *termui.Spinner
	textStarted    bool // a text delta was printed in this attempt
	anyDelta       bool
	reasoningChars int
	last           outputKind
	atLineStart    bool

	// One sanitiser per stream, so a sequence split across two chunks of
	// the same stream is reassembled rather than half-printed.
	text, thought, progress render.StreamSanitizer
}

// outputKind is the source of the last thing printed.
type outputKind int

const (
	outNone outputKind = iota
	outText
	outReasoning
	outProgress
	outEngine
	outSystem
)

// newTurnPrinter returns a printer for a turn. termui.BeginOutput has just
// moved to a fresh row, so the cursor starts at the beginning of one.
func newTurnPrinter() *turnPrinter { return &turnPrinter{atLineStart: true} }

// beginAttempt starts the "思考中" spinner for a new request attempt.
func (p *turnPrinter) beginAttempt() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.spinner != nil {
		p.spinner.Stop()
	}
	p.spinner = termui.NewSpinner("思考中...")
	p.textStarted = false
	p.reasoningChars = 0
	p.startSpinnerLocked()
}

// startSpinnerLocked starts the spinner only on an empty row: each frame
// begins with \r ESC[K, which would wipe text already on the row.
func (p *turnPrinter) startSpinnerLocked() {
	if p.spinner != nil && p.atLineStart {
		p.spinner.Start()
	}
}

func (p *turnPrinter) stopSpinnerLocked() {
	if p.spinner != nil {
		p.spinner.Stop()
	}
}

func (p *turnPrinter) stopSpinner() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopSpinnerLocked()
}

// stop ends the turn's output.
func (p *turnPrinter) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopSpinnerLocked()
	if p.last == outReasoning {
		termui.StreamPrint(termui.Reset)
	}
}

func (p *turnPrinter) gotDelta() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.anyDelta
}

// printLocked writes s and records whether it left the cursor mid-row.
func (p *turnPrinter) printLocked(s string) {
	if s == "" {
		return
	}
	termui.StreamPrint(s)
	if plain := render.StripControls(s); plain != "" {
		p.atLineStart = strings.HasSuffix(plain, "\n")
	}
}

func (p *turnPrinter) ensureLineStartLocked() {
	if !p.atLineStart {
		termui.StreamPrint("\n")
		p.atLineStart = true
	}
}

// switchToLocked starts output from source k on a fresh row when a different
// source printed last. The answer after a reasoning trace also gets a blank
// line, so the two read as separate blocks.
func (p *turnPrinter) switchToLocked(k outputKind) {
	prev := p.last
	if prev == k {
		return
	}
	p.last = k
	if prev == outReasoning {
		termui.StreamPrint(termui.Reset)
	}
	p.ensureLineStartLocked()
	if prev == outReasoning && k == outText {
		termui.StreamPrint("\n")
	}
}

func (p *turnPrinter) delta(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.textStarted {
		p.stopSpinnerLocked()
	}
	p.textStarted = true
	p.anyDelta = true
	out := p.text.Write(s)
	if out == "" {
		return
	}
	p.switchToLocked(outText)
	p.printLocked(out)
}

func (p *turnPrinter) reasoning(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !showReasoning {
		p.reasoningChars += utf8.RuneCountInString(s)
		if !p.textStarted && p.spinner != nil {
			p.spinner.SetMessage(reasoningStatus(p.reasoningChars))
		}
		return
	}
	if !p.textStarted {
		p.stopSpinnerLocked()
	}
	out := p.thought.Write(s)
	if out == "" {
		return
	}
	p.switchToLocked(outReasoning)
	p.printLocked(termui.ReasoningStyle + out + termui.Reset)
}

// toolProgress surfaces live output from long-running tools (bash,
// powershell) so the user can tell what a slow command is doing. The
// command's own colour survives; its control sequences do not.
func (p *turnPrinter) toolProgress(toolName, chunk string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopSpinnerLocked()
	out := p.progress.Write(chunk)
	if out == "" {
		return
	}
	p.switchToLocked(outProgress)
	p.printLocked(termui.Dim + out + termui.Reset)
}

// engineLine prints one of the engine's lines (a tool block, a stall or
// safety notice). They carry untrusted text — the command, a file's first
// line — so they are sanitised too; a line without a trailing newline is
// given one so it does not swallow the next output. While the model has not
// started its answer, the spinner comes back underneath: the next step is the
// model thinking again.
func (p *turnPrinter) engineLine(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopSpinnerLocked()
	out := render.SanitizeStream(line)
	if out == "" {
		return
	}
	p.switchToLocked(outEngine)
	p.printLocked(out)
	p.ensureLineStartLocked()
	if !p.textStarted {
		p.startSpinnerLocked()
	}
}

// system prints cove's own status text (retry notes, the failure line).
func (p *turnPrinter) system(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.switchToLocked(outSystem)
	p.printLocked(s)
}

// errorText is an error for display. A provider's error body is echoed in
// it, so it is untrusted like any other text from the network.
func (p *turnPrinter) errorText(err error) string {
	return render.StripControls(err.Error())
}

func (p *turnPrinter) permissionPause() { p.stopSpinner() }

// permissionDone brings the indicator back once the prompt is answered, but
// only while nothing has been streamed yet: once the model has started
// talking, an animated spinner would overwrite the reply.
func (p *turnPrinter) permissionDone() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.textStarted {
		p.startSpinnerLocked()
	}
}

func missingStreamedSuffix(reply, streamed string) string {
	if reply == "" || reply == streamed {
		return ""
	}
	if strings.HasPrefix(reply, streamed) {
		return reply[len(streamed):]
	}
	maxOverlap := len(reply)
	if len(streamed) < maxOverlap {
		maxOverlap = len(streamed)
	}
	for i := maxOverlap; i > 0; i-- {
		if strings.HasSuffix(streamed, reply[:i]) {
			return reply[i:]
		}
	}
	return ""
}

func isBudgetExceededError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "budget exceeded")
}

func budgetExceededRetryHint(tr *cost.Tracker) string {
	if tr != nil {
		suggested := tr.SuggestedBudget()
		if suggested > 0 {
			return fmt.Sprintf("预算已超限，继续重试不会成功。可执行 /budget auto 一键提高到 $%.2f，或手动 /budget <金额>，然后再输入“继续”。", suggested)
		}
	}
	return "预算已超限，继续重试不会成功。请先执行 /budget auto 或 /budget <更大金额>，然后再输入“继续”。"
}

// showReasoning streams the model's full reasoning into the conversation when
// true (config show_reasoning). By default a thinking model's reasoning is
// summarised in the status line instead: it can run to pages of scratch work
// that bury the answer.
var showReasoning bool

// reasoningStatus is the status-line text shown while a model is reasoning.
func reasoningStatus(chars int) string {
	return fmt.Sprintf("思考中… 已推理 %d 字", chars)
}
