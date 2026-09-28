package permission

import (
	"fmt"
	"strings"
	"sync"
)

type Mode string

const (
	Default Mode = "default"
	Plan    Mode = "plan"
	Auto    Mode = "auto"
	Bypass  Mode = "bypass"
)

func ValidMode(m Mode) bool {
	switch m {
	case Default, Plan, Auto, Bypass:
		return true
	}
	return false
}

func Modes() string {
	return "default|plan|auto|bypass"
}

type Decision string

// ReasonAskRule is the reason Check gives when an ask rule matched, so the
// engine can tell "an ask rule wants a prompt" (which nothing but a deny may
// override) from "the mode default is to ask".
const ReasonAskRule = "approval required by policy rule"

// ReasonDenyRule is the reason Check gives when a deny rule matched.
const ReasonDenyRule = "denied by policy rule"

const (
	DAllow  Decision = "allow"
	DDeny   Decision = "deny"
	DAsk    Decision = "ask"
	DBypass Decision = "bypass"
)

type Rule struct {
	ToolPattern string
	Decision    Decision
	ArgPattern  string
	// CommandPrefix scopes a bash/powershell rule to command lines built from
	// commands that start with these words; see commandCovered.
	CommandPrefix string
	// CommandGroup scopes a bash/powershell rule to command lines built from
	// the commands of a named group (GroupGitRoutine); pooled with prefix
	// rules like CommandPrefix is.
	CommandGroup string
	// InputEquals scopes a rule to calls whose input has each of these
	// fields set to exactly this string, e.g. serverName+toolName for the
	// MCP proxy tool.
	InputEquals map[string]string
	// ParamMatch scopes a rule to calls whose input fields match these glob
	// patterns (policies.json "param_match").
	ParamMatch map[string]string
	// Priority orders ask against allow rules (policies.json "priority";
	// rules remembered in the session have 0): an ask wins unless an allow
	// has a strictly higher priority. Deny rules ignore it and always win.
	Priority int
}

type Manager struct {
	// mu guards everything below: parallel tool calls Check while the
	// approval prompt adds session rules.
	mu              sync.RWMutex
	mode            Mode
	allow           []Rule
	deny            []Rule
	ask             []Rule
	bypassAvailable bool
	// shellKind is the quoting family the bash tool's commands run under;
	// the zero value is the strict cmd behaviour.
	shellKind ShellKind
}

func NewManager(mode Mode) *Manager {
	return &Manager{mode: mode}
}

func (m *Manager) SetMode(mode Mode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode = mode
}

func (m *Manager) Mode() Mode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.mode
}

// SetShellKind records which shell the bash tool runs commands with, which
// decides how quoted operator characters are treated by prefix rules.
func (m *Manager) SetShellKind(k ShellKind) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shellKind = k
}

// ShellKindFor is the quoting family toolName's command runs under: always
// PowerShell for the powershell tool, the configured shell for bash.
func (m *Manager) ShellKindFor(toolName string) ShellKind {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return shellKindFor(toolName, m.shellKind)
}

func (m *Manager) SetBypassAvailable(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bypassAvailable = v
}

func (m *Manager) AddRule(decision Decision, rule Rule) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rule.Decision = decision
	switch decision {
	case DAllow:
		m.allow = append(m.allow, rule)
	case DDeny:
		m.deny = append(m.deny, rule)
	case DAsk:
		m.ask = append(m.ask, rule)
	}
}

// RemoveRules takes rules previously added with decision out again, one
// instance per given rule; rules not present are ignored. The engine uses it
// to drop the rules it loaded from policies.json when /cd moves it to another
// project, leaving rules added during the session in place.
func (m *Manager) RemoveRules(decision Decision, rules []Rule) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var list *[]Rule
	switch decision {
	case DAllow:
		list = &m.allow
	case DDeny:
		list = &m.deny
	case DAsk:
		list = &m.ask
	default:
		return
	}
	for _, r := range rules {
		// Search from the end so the most recently added copy goes first.
		for i := len(*list) - 1; i >= 0; i-- {
			if sameRule((*list)[i], r) {
				*list = append((*list)[:i:i], (*list)[i+1:]...)
				break
			}
		}
	}
}

// SameRule reports whether a and b match the same calls; Decision is ignored.
func SameRule(a, b Rule) bool { return sameRule(a, b) }

// sameRule reports whether a and b match the same calls; Decision is ignored.
func sameRule(a, b Rule) bool {
	if a.ToolPattern != b.ToolPattern || a.ArgPattern != b.ArgPattern ||
		a.CommandPrefix != b.CommandPrefix || a.CommandGroup != b.CommandGroup ||
		a.Priority != b.Priority || len(a.InputEquals) != len(b.InputEquals) ||
		len(a.ParamMatch) != len(b.ParamMatch) {
		return false
	}
	for k, v := range a.InputEquals {
		if bv, ok := b.InputEquals[k]; !ok || bv != v {
			return false
		}
	}
	for k, v := range a.ParamMatch {
		if bv, ok := b.ParamMatch[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func (m *Manager) Check(toolName string, toolInput map[string]any, defaultDecision Decision) (Decision, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.deny {
		if matchRule(r, toolName, toolInput) {
			return DDeny, ReasonDenyRule
		}
	}
	// Plan mode is read-only whatever rules were added earlier: an "always
	// allow bash" answered in default mode must not let plan mode run bash.
	if m.mode == Plan && defaultDecision != DAllow {
		return DDeny, "plan mode - only read operations allowed"
	}
	if m.mode == Bypass && m.bypassAvailable {
		return DBypass, "bypass mode"
	}
	// Ask and allow rules. An ask wins over an allow unless the allow has a
	// strictly higher priority: "always ask for git push" is not silenced by
	// an earlier "[a]"/"[p]" answer or a whole-tool allow (all priority 0),
	// while a policies.json allow ranked above an ask still applies.
	// Bypass mode (above) skips them; only deny rules and plan mode stop it.
	askPri, asked := 0, false
	for _, r := range m.ask {
		if matchRule(r, toolName, toolInput) && (!asked || r.Priority > askPri) {
			askPri, asked = r.Priority, true
		}
	}
	allowPri, allowed, allowReason := 0, false, ""
	cov := coverage{kind: shellKindFor(toolName, m.shellKind)}
	poolPri, pooling := 0, false
	for _, r := range m.allow {
		if r.CommandPrefix != "" || r.CommandGroup != "" {
			// Prefix and group rules are pooled: a compound line is allowed
			// when each of its commands is covered by some rule, not
			// necessarily the same one. The pool ranks as its lowest rule.
			if toolMatches(r, toolName) {
				if r.CommandPrefix != "" {
					cov.prefixes = append(cov.prefixes, strings.Fields(r.CommandPrefix))
				} else {
					cov.groups = append(cov.groups, r.CommandGroup)
				}
				if !pooling || r.Priority < poolPri {
					poolPri, pooling = r.Priority, true
				}
			}
			continue
		}
		if matchRule(r, toolName, toolInput) && (!allowed || r.Priority > allowPri) {
			allowPri, allowed, allowReason = r.Priority, true, "allowed by policy rule"
		}
	}
	if pooling && (!allowed || poolPri > allowPri) && lineCovered(inputCommand(toolInput), cov) {
		allowPri, allowed, allowReason = poolPri, true, "allowed by command prefix rule"
	}
	if asked && (!allowed || allowPri <= askPri) {
		return DAsk, ReasonAskRule
	}
	if allowed {
		return DAllow, allowReason
	}
	switch m.mode {
	case Auto:
		if defaultDecision == DAllow {
			return DAllow, "auto mode: safe tool"
		}
		return defaultDecision, "auto mode: approval required"
	case Plan:
		if defaultDecision == DAllow {
			return DAllow, "plan mode: read-only tool"
		}
		return DDeny, "plan mode - only read operations allowed"
	default:
		return defaultDecision, ""
	}
}

// toolMatches compares a rule's tool pattern with a canonical tool name:
// "*", a glob such as "mcp__*", or a name (case-insensitive, like the
// policies.json matcher it replaces).
func toolMatches(r Rule, toolName string) bool {
	return matchGlob(r.ToolPattern, toolName)
}

func matchRule(r Rule, toolName string, input map[string]any) bool {
	if !toolMatches(r, toolName) {
		return false
	}
	// Allow rules with a prefix or group never reach here (Check pools them);
	// for deny and ask such a rule applies when any command in the line
	// matches it.
	if r.CommandPrefix != "" {
		return anyCommandHasPrefixNormalized(inputCommand(input), strings.Fields(r.CommandPrefix))
	}
	if r.CommandGroup != "" {
		return groupMatchesAny(r.CommandGroup, inputCommand(input))
	}
	for field, want := range r.InputEquals {
		if got, ok := input[field].(string); !ok || got != want {
			return false
		}
	}
	for field, pattern := range r.ParamMatch {
		got, ok := input[field]
		if !ok || !matchGlob(pattern, fmt.Sprintf("%v", got)) {
			return false
		}
	}
	// An arg-restricted rule only matches when some string argument contains the
	// pattern. A nil/empty input must NOT match (previously it fell through to
	// `return true`, so an allow rule with an ArgPattern auto-allowed any call
	// whose Input was nil — an argument-scoping bypass).
	if r.ArgPattern != "" {
		for _, v := range input {
			if s, ok := v.(string); ok && strings.Contains(s, r.ArgPattern) {
				return true
			}
		}
		return false
	}
	return true
}
