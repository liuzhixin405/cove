package permission

import (
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/safety"
)

// PolicyAction is the action to take when a policy matches.
type PolicyAction string

const (
	ActionAllow PolicyAction = "allow"
	ActionDeny  PolicyAction = "deny"
	ActionAsk   PolicyAction = "ask"
)

// PolicyRule defines a single permission rule with optional parameter matching.
//
// Rules persisted by the "[p] 本项目记住" prompt answer use CommandPrefix or
// CommandGroup (shell tools), InputEquals (the MCP proxy) or none of them (a
// whole tool), and Scope: the absolute project root they were granted in.
// The engine ignores a rule whose non-empty Scope is a different project.
type PolicyRule struct {
	ID          string       `json:"id"`
	Description string       `json:"description"`
	ToolPattern string       `json:"tool_pattern"` // glob pattern: "bash", "write:*", "*"
	Action      PolicyAction `json:"action"`
	Priority    int          `json:"priority"` // higher = evaluated first
	Enabled     bool         `json:"enabled"`
	// ParamMatch maps an input field to a glob. On the "command" field of a
	// shell tool the glob applies to each simple command of the line, not
	// the raw line (see commandParamMatches).
	ParamMatch map[string]string `json:"param_match,omitempty"`
	// CommandPrefix scopes a shell-tool rule to commands starting with these
	// words, with the same semantics as Rule.CommandPrefix.
	CommandPrefix string `json:"command_prefix,omitempty"`
	// CommandGroup scopes a shell-tool rule to the commands of a named group
	// (GroupGitRoutine), with the same semantics as Rule.CommandGroup.
	CommandGroup string `json:"command_group,omitempty"`
	// InputEquals requires each input field to equal this string exactly.
	InputEquals map[string]string `json:"input_equals,omitempty"`
	// Scope is the project root the rule applies to; empty means everywhere.
	Scope string `json:"scope,omitempty"`
}

// ToRule converts the rule to the session Manager's form, so persisted allow
// rules are enforced by the same matcher (prefix pooling, plan mode) as
// session ones. A rule with a CommandPrefix or CommandGroup and no tool
// defaults to bash. It reports false for rules the Manager cannot express:
// glob tool patterns other than "*", ParamMatch, an unknown group, or no tool
// at all. The Decision is left for Manager.AddRule to set.
func (r PolicyRule) ToRule() (Rule, bool) {
	tool := r.ToolPattern
	if tool == "" && (r.CommandPrefix != "" || r.CommandGroup != "") {
		tool = "bash"
	}
	if tool == "" {
		return Rule{}, false
	}
	if r.CommandGroup != "" && !KnownGroup(r.CommandGroup) {
		return Rule{}, false
	}
	// Every other rule converts: glob tool patterns and param_match used to
	// stay behind in a second evaluator with different precedence, so the
	// same deny written two ways behaved two ways.
	out := Rule{ToolPattern: tool, CommandPrefix: r.CommandPrefix, CommandGroup: r.CommandGroup, Priority: r.Priority}
	if len(r.InputEquals) > 0 {
		out.InputEquals = make(map[string]string, len(r.InputEquals))
		for k, v := range r.InputEquals {
			out.InputEquals[k] = v
		}
	}
	if len(r.ParamMatch) > 0 {
		out.ParamMatch = make(map[string]string, len(r.ParamMatch))
		for k, v := range r.ParamMatch {
			out.ParamMatch[k] = v
		}
	}
	return out, true
}

// commandParamMatches is param_match on the "command" field of a shell tool.
// It used to be matchGlob over the whole line, so an allow of "git status*"
// allowed "git status; rm -rf ./src". The glob now applies per simple
// command, like a command prefix:
//
//   - allow: the line must be one commandCovered would vouch for (no
//     substitution, no file redirect, no hostile characters; see
//     coverableCommands) and every simple command must match;
//   - deny and ask: the whole line matching (as before), or any one simple
//     command, nested ones included (see denyReading), is enough.
//
// A pattern of words ending in "*" and holding no other "*" ("git status*",
// "npm run *") is a word prefix: "git status" and "git status -s" match,
// "git statusx" does not. Any other pattern is matchGlob on the command's
// words joined by single spaces.
func commandParamMatches(pattern, command string, decision Decision, kind ShellKind) bool {
	prefix, isPrefix := globWordPrefix(pattern)
	if decision == DAllow {
		cmds, ok := coverableCommands(command, kind)
		if !ok {
			return false
		}
		for _, words := range cmds {
			if isPrefix && !hasWordPrefixFold(words, prefix) || !isPrefix && !matchGlob(pattern, strings.Join(words, " ")) {
				return false
			}
		}
		return true
	}
	if matchGlob(pattern, command) {
		return true
	}
	if isPrefix {
		return anyCommandHasPrefixNormalized(command, prefix)
	}
	cmds, opaque := denyReading(command)
	if opaque {
		return true
	}
	for _, c := range cmds {
		if matchGlob(pattern, strings.Join(c.Words, " ")) ||
			matchGlob(pattern, strings.Join(normalizeProgram(safety.StripCommandRunners(c.Words)), " ")) {
			return true
		}
	}
	return false
}

// globWordPrefix returns the words of a "words*" pattern.
func globWordPrefix(pattern string) ([]string, bool) {
	if !strings.HasSuffix(pattern, "*") || strings.Count(pattern, "*") != 1 {
		return nil, false
	}
	words := strings.Fields(strings.TrimSuffix(pattern, "*"))
	return words, len(words) > 0
}

// hasWordPrefixFold is hasWordPrefix ignoring letter case, as matchGlob does.
func hasWordPrefixFold(words, prefix []string) bool {
	if len(words) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if !strings.EqualFold(words[i], p) {
			return false
		}
	}
	return true
}

// PolicyStorage persists policy rules. *FilePolicyStorage implements it.
type PolicyStorage interface {
	Load() ([]PolicyRule, error)
	Save(rules []PolicyRule) error
}

// Basic glob matching: supports * wildcard
func matchGlob(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return strings.EqualFold(pattern, value)
	}
	parts := strings.Split(pattern, "*")
	// Simple prefix*suffix matching
	if len(parts) == 2 {
		if parts[0] != "" && !strings.HasPrefix(strings.ToLower(value), strings.ToLower(parts[0])) {
			return false
		}
		if parts[1] != "" && !strings.HasSuffix(strings.ToLower(value), strings.ToLower(parts[1])) {
			return false
		}
		return true
	}
	// Fallback: substring check
	for _, part := range parts {
		if part != "" && !strings.Contains(strings.ToLower(value), strings.ToLower(part)) {
			return false
		}
	}
	return true
}
