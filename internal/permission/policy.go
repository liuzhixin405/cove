package permission

import (
	"strings"
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
	ID          string            `json:"id"`
	Description string            `json:"description"`
	ToolPattern string            `json:"tool_pattern"` // glob pattern: "bash", "write:*", "*"
	Action      PolicyAction      `json:"action"`
	Priority    int               `json:"priority"` // higher = evaluated first
	Enabled     bool              `json:"enabled"`
	ParamMatch  map[string]string `json:"param_match,omitempty"` // param key -> glob value
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
