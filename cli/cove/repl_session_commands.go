package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/liuzhixin405/cove/internal/engine"
)

func handleSessionCommand(input string, eng *engine.Engine, historyPickPending *bool) bool {
	switch {
	case strings.HasPrefix(input, "/export"):
		handleExport(input, eng)
		return true
	case strings.HasPrefix(input, "/resume") || input == "/resume":
		sessionID := ""
		if strings.HasPrefix(input, "/resume ") {
			sessionID = strings.TrimPrefix(input, "/resume ")
		}
		withInterrupt(func(ctx context.Context) { handleResume(ctx, sessionID, eng) })
		return true
	case input == "/history":
		// Display the history list and set pick-pending state so the next numeric
		// input is treated as a session selection by the main loop.
		handleHistory(eng)
		*historyPickPending = true
		return true
	case strings.HasPrefix(input, "/history "):
		histID := strings.TrimSpace(strings.TrimPrefix(input, "/history "))
		if strings.EqualFold(histID, "clean") {
			handleHistoryClean()
			*historyPickPending = false
			return true
		}
		if strings.HasPrefix(strings.ToLower(histID), "detail ") {
			handleHistoryDetail(strings.TrimSpace(histID[len("detail "):]), eng)
			*historyPickPending = false
			return true
		}
		handleHistoryResume(histID, eng)
		*historyPickPending = false
		return true
	case input == "/compact":
		withInterrupt(func(ctx context.Context) {
			eng.Compact(ctx)
			fmt.Println("上下文窗口已压缩。")
		})
		return true
	default:
		return false
	}
}
