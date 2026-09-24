package main

import (
	"context"
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
		handleHistory(eng, false)
		*historyPickPending = true
		return true
	case strings.HasPrefix(input, "/history "):
		histID := strings.TrimSpace(strings.TrimPrefix(input, "/history "))
		if strings.EqualFold(histID, "clean") {
			handleHistoryClean()
			*historyPickPending = false
			return true
		}
		// "/history all ..." addresses the all-projects list; without it,
		// numbers index the current project's list.
		all := false
		if strings.EqualFold(histID, "all") {
			handleHistory(eng, true)
			*historyPickPending = true
			return true
		}
		if strings.HasPrefix(strings.ToLower(histID), "all ") {
			all = true
			histID = strings.TrimSpace(histID[len("all "):])
		}
		if strings.HasPrefix(strings.ToLower(histID), "detail ") {
			handleHistoryDetail(strings.TrimSpace(histID[len("detail "):]), eng, all)
			*historyPickPending = false
			return true
		}
		handleHistoryResumeIn(histID, eng, all)
		*historyPickPending = false
		return true
	case input == "/compact":
		withInterrupt(func(ctx context.Context) {
			eng.Compact(ctx)
			outln("上下文窗口已压缩。")
		})
		return true
	default:
		return false
	}
}
