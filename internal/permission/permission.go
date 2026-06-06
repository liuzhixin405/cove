package permission

import "strings"

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
}

type Policy struct {
	Allow []Rule `json:"allow,omitempty"`
	Deny  []Rule `json:"deny,omitempty"`
	Ask   []Rule `json:"ask,omitempty"`
}

type Manager struct {
	mode            Mode
	allow           []Rule
	deny            []Rule
	ask             []Rule
	bypassAvailable bool
}

func NewManager(mode Mode) *Manager {
	return &Manager{mode: mode}
}

func (m *Manager) SetMode(mode Mode)         { m.mode = mode }
func (m *Manager) Mode() Mode                { return m.mode }
func (m *Manager) SetBypassAvailable(v bool) { m.bypassAvailable = v }
func (m *Manager) SetPolicy(p Policy) {
	m.allow = p.Allow
	m.deny = p.Deny
	m.ask = p.Ask
}

func (m *Manager) AddRule(decision Decision, rule Rule) {
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

func (m *Manager) Policy() Policy {
	return Policy{
		Allow: append([]Rule(nil), m.allow...),
		Deny:  append([]Rule(nil), m.deny...),
		Ask:   append([]Rule(nil), m.ask...),
	}
}

func (m *Manager) Check(toolName string, toolInput map[string]any, defaultDecision Decision) (Decision, string) {
	for _, r := range m.deny {
		if matchRule(r, toolName, toolInput) {
			return DDeny, "denied by policy rule"
		}
	}
	if m.mode == Bypass && m.bypassAvailable {
		return DBypass, "bypass mode"
	}
	for _, r := range m.allow {
		if matchRule(r, toolName, toolInput) {
			return DAllow, "allowed by policy rule"
		}
	}
	for _, r := range m.ask {
		if matchRule(r, toolName, toolInput) {
			return DAsk, "approval required by policy rule"
		}
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

func matchRule(r Rule, toolName string, input map[string]any) bool {
	if r.ToolPattern != "*" && r.ToolPattern != toolName {
		return false
	}
	if r.ArgPattern != "" && input != nil {
		for _, v := range input {
			if s, ok := v.(string); ok && strings.Contains(s, r.ArgPattern) {
				return true
			}
		}
		return false
	}
	return true
}
