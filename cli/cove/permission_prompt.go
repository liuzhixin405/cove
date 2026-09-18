package main

import (
	"strings"
	"time"

	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/repl"
	"github.com/liuzhixin405/cove/internal/termui"
)

// permissionPromptTimeout bounds how long a gated tool waits for the user's
// answer. The prompt is rendered on the task goroutine while the main loop sits
// in ReadLine, so a prompt nobody answers (Ctrl+D at the prompt, a detached
// terminal, /exit) would otherwise block the run forever. It is a variable so
// tests can shorten it.
var permissionPromptTimeout = 15 * time.Minute

// installPermissionPrompt wires eng.PermissionPrompt so tools that need
// approval ask the user instead of being denied outright.
//
// The engine denies every "ask" decision when PermissionPrompt is nil
// (see Engine.authorizeTool), which makes write/bash tools unusable in the
// default permission mode with a misleading "no interactive approval handler is
// installed" error. Only a frontend whose input loop serves
// repl.TakePermInputCh may install a handler; without the handler the deny path
// stays in place, which is the correct fail-closed behaviour.
func installPermissionPrompt(eng *engine.Engine) {
	if eng == nil {
		return
	}
	eng.PermissionPrompt = func(toolName string, input map[string]any, reason string) bool {
		return askToolPermission(eng, toolName, input, reason)
	}
}

// askToolPermission renders the approval box, waits for the answer line that
// the REPL loop relays through repl.TakePermInputCh, and reports the decision.
//
// Answers: "y"/"yes" allow this call once, "a"/"always" allow this tool for the
// rest of the session (an engine-wide policy rule), anything else denies.
func askToolPermission(eng *engine.Engine, toolName string, input map[string]any, reason string) bool {
	if !replInteractive {
		// Nothing is reading answer lines (e.g. a -p one-shot run), so deny
		// rather than block on a prompt that cannot be answered.
		return false
	}

	answerCh := make(chan string, 1)
	repl.SetPermInputCh(answerCh)
	repl.BeginPromptInput()
	termui.PrintAbove(termui.PermissionPrompt(toolName, permissionPromptDescription(input, reason)) +
		"  " + termui.Styled(termui.Bold, "[y] 允许   [n] 拒绝   [a] 本次会话总是允许") + "\n")

	timer := time.NewTimer(permissionPromptTimeout)
	defer timer.Stop()

	var answer string
	select {
	case answer = <-answerCh:
	case <-timer.C:
		repl.EndPromptInput()
		repl.ClearPermInputCh()
		termui.PrintAbove("  " + termui.Styled(termui.Dim, "授权超时，已拒绝 "+toolName) + "\n")
		return false
	}

	repl.EndPromptInput()
	// TakePermInputCh already unregisters the channel when the loop relays a
	// line; ClearPermInputCh only matters for the timeout path above.
	repl.ClearPermInputCh()

	switch allow, always := permissionAnswerDecision(answer); {
	case allow && always:
		// The engine consults its own manager (e.perm), so the rule has to be
		// added there; it applies for this session only and is not persisted.
		eng.AddPermissionRule(permission.DAllow, permission.Rule{ToolPattern: toolName})
		termui.PrintAbove("  " + termui.Styled(termui.Dim, "已允许 "+toolName+"（本次会话）") + "\n")
		return true
	case allow:
		return true
	default:
		return false
	}
}

// permissionAnswerDecision maps a raw answer line to a decision: allow reports
// whether the call is permitted, always requests a session-wide rule for the
// tool. Anything unrecognised denies, so a stray keypress fails closed.
func permissionAnswerDecision(answer string) (allow, always bool) {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes", "是", "允许":
		return true, false
	case "a", "always", "总是":
		return true, true
	default:
		return false, false
	}
}

// permissionPromptDescription picks the most informative line to show inside the
// approval box: the concrete argument the user is being asked about, falling
// back to the engine's reason when the tool takes no recognisable argument.
func permissionPromptDescription(input map[string]any, reason string) string {
	for _, key := range []string{"command", "file_path", "filePath", "path", "pattern", "query", "url"} {
		if v, ok := input[key]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return reason
}
