package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/agentgo/internal/api"
	"github.com/agentgo/internal/cost"
	"github.com/agentgo/internal/engine"
	"github.com/agentgo/internal/repl"
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
	repl.BeginOutput()
	defer repl.EndOutput()
	var totalOutput strings.Builder
	var finalErr error

	maxAttempts := 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		spinner := repl.NewSpinner("思考中...")
		spinner.Start()
		firstDelta := true
		gotDelta := false

		if eng, ok := runner.(*engine.Engine); ok {
			eng.OnPermissionPause = func() {
				spinner.Stop()
			}
			eng.OnPermissionDone = nil
			defer func() { eng.OnPermissionPause = nil }()
		}

		var err error
		if richRunner, ok := runner.(interface {
			RunMessageWithStream(context.Context, api.Message, func(string)) (string, error)
		}); ok {
			_, err = richRunner.RunMessageWithStream(ctx, userMsg, func(delta string) {
				if firstDelta {
					spinner.Stop()
					firstDelta = false
				}
				gotDelta = true
				repl.StreamPrint(delta)
				totalOutput.WriteString(delta)
			})
		} else {
			if len(userMsg.Parts) > 0 {
				err = fmt.Errorf("当前运行器不支持附件消息")
			} else {
				_, err = runner.RunWithStream(ctx, userMsg.Content, func(delta string) {
					if firstDelta {
						spinner.Stop()
						firstDelta = false
					}
					gotDelta = true
					repl.StreamPrint(delta)
					totalOutput.WriteString(delta)
				})
			}
		}
		spinner.Stop()

		if err == nil {
			finalErr = nil
			break
		}
		finalErr = err
		if gotDelta || attempt == maxAttempts || ctx.Err() != nil || !isTransientRequestError(err) {
			break
		}
		note := fmt.Sprintf("\n网络波动，自动重试中 (%d/%d)...\n", attempt, maxAttempts)
		repl.StreamPrint(fmt.Sprintf("%s%s%s", repl.Yellow, note, repl.Reset))
		totalOutput.WriteString(note)
		time.Sleep(time.Duration(attempt) * 1200 * time.Millisecond)
	}

	if finalErr != nil {
		errMsg := fmt.Sprintf("\nRequest failed: %s", finalErr.Error())
		repl.StreamPrint(fmt.Sprintf("%s%s%s", repl.Red, errMsg, repl.Reset))
		totalOutput.WriteString(errMsg)
	}
	totalOutput.WriteString("\r\n\r\n")
	return totalOutput.String(), finalErr
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
