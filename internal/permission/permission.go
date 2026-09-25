package permission

import (
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
	// InputEquals scopes a rule to calls whose input has each of these
	// fields set to exactly this string, e.g. serverName+toolName for the
	// MCP proxy tool.
	InputEquals map[string]string
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
		a.CommandPrefix != b.CommandPrefix || len(a.InputEquals) != len(b.InputEquals) {
		return false
	}
	for k, v := range a.InputEquals {
		if bv, ok := b.InputEquals[k]; !ok || bv != v {
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
			return DDeny, "denied by policy rule"
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
	// Ask rules come before allow rules: "always ask for git push" must not
	// be silenced by an earlier "[a]"/"[p]" answer or a whole-tool allow.
	// Bypass mode (above) skips them; only deny rules and plan mode stop it.
	for _, r := range m.ask {
		if matchRule(r, toolName, toolInput) {
			return DAsk, ReasonAskRule
		}
	}
	var prefixes [][]string
	for _, r := range m.allow {
		if r.CommandPrefix != "" {
			// Prefix rules are pooled: a compound line is allowed when each of
			// its commands is covered by some rule, not necessarily the same one.
			if toolMatches(r, toolName) {
				prefixes = append(prefixes, strings.Fields(r.CommandPrefix))
			}
			continue
		}
		if matchRule(r, toolName, toolInput) {
			return DAllow, "allowed by policy rule"
		}
	}
	if len(prefixes) > 0 && commandCovered(inputCommand(toolInput), prefixes, shellKindFor(toolName, m.shellKind)) {
		return DAllow, "allowed by command prefix rule"
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

func toolMatches(r Rule, toolName string) bool {
	return r.ToolPattern == "*" || r.ToolPattern == toolName
}

func matchRule(r Rule, toolName string, input map[string]any) bool {
	if !toolMatches(r, toolName) {
		return false
	}
	// Allow rules with a prefix never reach here (Check pools them); for deny
	// and ask a prefix rule applies when any command in the line matches it.
	if r.CommandPrefix != "" {
		return anyCommandHasPrefixNormalized(inputCommand(input), strings.Fields(r.CommandPrefix))
	}
	for field, want := range r.InputEquals {
		if got, ok := input[field].(string); !ok || got != want {
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
