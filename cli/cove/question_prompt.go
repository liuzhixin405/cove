package main

import (
	"strings"
	"time"

	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/mcp"
	"github.com/liuzhixin405/cove/internal/repl"
	"github.com/liuzhixin405/cove/internal/termui"
)

// questionPromptTimeout bounds how long the question tool waits for an
// answer; an unanswered question gets an empty answer. A variable for tests.
var questionPromptTimeout = 15 * time.Minute

// installQuestionPrompt gives the question tool a way to ask: Runtime.AskUser
// was never assigned, so the tool always answered "requires interactive
// mode". It uses the answer relay the permission prompt uses
// (repl.SetPermInputCh), so only the REPL installs it; -p and headless runs
// do not register the tool at all.
func installQuestionPrompt(eng *engine.Engine) {
	if eng == nil || eng.Runtime() == nil {
		return
	}
	eng.Runtime().AskUser = askUserQuestion
}

// askUserQuestion shows prompt above the input line and returns the next
// line the user enters, or "" when nothing reads answers or on timeout.
// Ctrl+C at the question returns promptInterrupt (tool.AskUserCancelled),
// which ends the question tool.
func askUserQuestion(prompt string) string {
	if !replInteractive {
		return ""
	}
	answerCh := make(chan string, 1)
	repl.SetPermInputCh(answerCh)
	repl.BeginPromptInput()
	termui.PrintAbove(termui.Styled(termui.Bold, strings.TrimRight(prompt, "\n")) + "\n  " +
		termui.Styled(termui.Dim, "输入选项编号或直接输入回答") + "\n")

	timer := time.NewTimer(questionPromptTimeout)
	defer timer.Stop()
	var answer string
	select {
	case answer = <-answerCh:
	case <-timer.C:
		termui.PrintAbove("  " + termui.Styled(termui.Dim, "等待回答超时，按未回答处理") + "\n")
	}
	repl.EndPromptInput()
	repl.ClearPermInputCh()
	return answer
}

// wireToolDefsVersion makes the engine rebuild its tool definitions when the
// MCP pool's tool list changes (/mcp connect, tools/list_changed): the mcp
// proxy's description lists the pool's tools.
func wireToolDefsVersion(eng *engine.Engine, pool *mcp.Pool) {
	if eng == nil || pool == nil {
		return
	}
	eng.SetToolDefsVersion(func() int { return int(pool.Version()) })
}
