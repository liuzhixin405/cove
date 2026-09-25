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

// permissionRuleAdder is the part of *engine.Engine the prompt needs: the
// engine consults its own manager, so "always" rules must be added there.
type permissionRuleAdder interface {
	AddPermissionRule(permission.Decision, permission.Rule)
}

// permissionRulePersister is implemented by *engine.Engine: it writes allow
// rules to the policies file (engine.PolicyFilePath) in one save, scoped to
// the engine's project root, and on success installs them for this session as
// rules loaded from policies.json (so /cd drops them). The "[p]" option is
// only offered when the adder can persist.
type permissionRulePersister interface {
	PersistPermissionRules(rules []permission.Rule, scope string) error
	PermissionScope() string
}

// askToolPermission renders the approval box, waits for the answer line that
// the REPL loop relays through repl.TakePermInputCh, and reports the decision.
//
// Answers: "y"/"yes" allow this call once, "a"/"always" additionally remember
// the scope from alwaysAllowScope for the rest of the session (an engine-wide
// policy rule), "p"/"permanent" also write that rule to the policies file
// (engine.PolicyFilePath) for this project, anything else denies.
func askToolPermission(eng permissionRuleAdder, toolName string, input map[string]any, reason string) bool {
	if !replInteractive {
		// Nothing is reading answer lines (e.g. a -p one-shot run), so deny
		// rather than block on a prompt that cannot be answered.
		return false
	}

	rules, scope, canRemember := alwaysAllowScope(toolName, input)
	persister, canPersist := eng.(permissionRulePersister)
	canPersist = canPersist && canRemember
	options := "[y] 允许"
	if canRemember {
		options += "   [a] 本次会话总是允许"
		if scope != "" {
			options += " " + scope
		}
	}
	if canPersist {
		options += "   [p] 永久允许"
		if scope != "" {
			options += " " + scope
		}
		options += "（本项目）"
	}
	options += "   [n] 拒绝"

	answerCh := make(chan string, 1)
	repl.SetPermInputCh(answerCh)
	repl.BeginPromptInput()
	termui.PrintAbove(termui.PermissionPrompt(toolName, permissionPromptDescription(input, reason)) +
		"  " + termui.Styled(termui.Bold, options) + "\n")

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
	if answer == promptInterrupt {
		// Ctrl+C: a denial, not an answer the user typed.
		return false
	}

	allow, always, persist := permissionAnswerDecision(answer)
	switch {
	case allow && always && !canRemember:
		// "a"/"p" was typed although it was not offered: honour the allow, but
		// remembering a wider scope than the user saw would be a surprise.
		termui.PrintAbove("  " + termui.Styled(termui.Dim, "此命令无法按前缀记住，仅允许本次") + "\n")
		return true
	case allow && persist && canPersist:
		what := toolName
		if scope != "" {
			what += " 中 " + scope
		}
		root := persister.PermissionScope()
		// A successful persist installs the rules itself, registered as
		// rules loaded from policies.json so /cd to another project drops
		// them; adding a session copy here would outlive the /cd.
		if err := persister.PersistPermissionRules(rules, root); err != nil {
			for _, r := range rules {
				eng.AddPermissionRule(permission.DAllow, r)
			}
			termui.PrintAbove("  " + termui.Styled(termui.Dim, "已允许 "+what+"；未能写入，仅本次会话有效（policies.json: "+err.Error()+"）") + "\n")
			return true
		}
		termui.PrintAbove("  " + termui.Styled(termui.Dim, "已永久允许 "+what+"（项目 "+root+"，已写入 "+policiesFileForDisplay()+"）") + "\n")
		return true
	case allow && always:
		// The engine consults its own manager (e.perm), so the rules have to be
		// added there; they apply for this session only and are not persisted.
		for _, r := range rules {
			eng.AddPermissionRule(permission.DAllow, r)
		}
		what := toolName
		if scope != "" {
			what += " 中 " + scope
		}
		termui.PrintAbove("  " + termui.Styled(termui.Dim, "已允许 "+what+"（本次会话）") + "\n")
		return true
	case allow:
		return true
	default:
		return false
	}
}

// alwaysAllowScope decides what an "a" answer remembers. Shell tools get one
// rule per command prefix in the line (see permission.CommandPrefixes), with
// scope naming them for the prompt, e.g. `"go test" 开头的命令`; allowing the
// whole bash tool would let every later command, rm -rf included, run
// unasked. The MCP proxy gets one rule for the server tool being called, e.g.
// `MCP 工具 "github/create_issue"`. Other tools keep a whole-tool rule and an
// empty scope. ok is false when a shell command has no prefix that can be
// remembered safely (or an MCP call names no server tool), so "a" is not
// offered at all.
func alwaysAllowScope(toolName string, input map[string]any) (rules []permission.Rule, scope string, ok bool) {
	if toolName == "mcp" {
		// The MCP proxy fronts every tool of every connected server; a
		// whole-tool rule allowed all of them, destructive ones included, for
		// the rest of the session. Remember the one server tool instead.
		server, _ := input["serverName"].(string)
		name, _ := input["toolName"].(string)
		if strings.TrimSpace(server) == "" || strings.TrimSpace(name) == "" {
			return nil, "", false
		}
		rule := permission.Rule{ToolPattern: toolName, InputEquals: map[string]string{"serverName": server, "toolName": name}}
		return []permission.Rule{rule}, `MCP 工具 "` + server + "/" + name + `"`, true
	}
	if !permission.IsShellTool(toolName) {
		return []permission.Rule{{ToolPattern: toolName}}, "", true
	}
	command, _ := input["command"].(string)
	prefixes, ok := permission.CommandPrefixesFor(command, permission.ToolShellKind(toolName))
	if !ok {
		return nil, "", false
	}
	quoted := make([]string, len(prefixes))
	for i, p := range prefixes {
		rules = append(rules, permission.Rule{ToolPattern: toolName, CommandPrefix: p})
		quoted[i] = `"` + p + `"`
	}
	return rules, strings.Join(quoted, "、") + " 开头的命令", true
}

// permissionAnswerDecision maps a raw answer line to a decision: allow reports
// whether the call is permitted, always requests a session-wide rule for the
// tool, persist additionally asks for that rule to be saved for this project.
// Anything unrecognised denies, so a stray keypress fails closed.
func permissionAnswerDecision(answer string) (allow, always, persist bool) {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes", "是", "允许":
		return true, false, false
	case "a", "always", "总是":
		return true, true, false
	case "p", "permanent", "永久":
		return true, true, true
	default:
		return false, false, false
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

// policiesFileForDisplay names the policies file a "[p]" rule was written to:
// policies.json in the config directory, which COVE_CONFIG_DIR moves away
// from ~/.cove.
func policiesFileForDisplay() string {
	if p, err := engine.PolicyFilePath(); err == nil {
		return p
	}
	return "policies.json"
}

// PolicyLoadError forwards Engine.PolicyLoadError to the commands, so /cd can
// warn when policies.json failed to reload for the new project. It sits here
// with the rest of the permission plumbing rather than beside the other
// replEngineAdapter forwarders in main.go.
func (a replEngineAdapter) PolicyLoadError() error { return a.eng.PolicyLoadError() }
