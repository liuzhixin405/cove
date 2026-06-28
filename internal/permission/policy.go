package permission

import (
	"fmt"
	"strings"
	"sync"
)

// PolicyAction is the action to take when a policy matches.
type PolicyAction string

const (
	ActionAllow PolicyAction = "allow"
	ActionDeny  PolicyAction = "deny"
	ActionAsk   PolicyAction = "ask"
)

// PolicyRule defines a single permission rule with optional parameter matching.
type PolicyRule struct {
	ID          string            `json:"id"`
	Description string            `json:"description"`
	ToolPattern string            `json:"tool_pattern"` // glob pattern: "bash", "write:*", "*"
	Action      PolicyAction      `json:"action"`
	Priority    int               `json:"priority"`    // higher = evaluated first
	Enabled     bool              `json:"enabled"`
	ParamMatch  map[string]string `json:"param_match,omitempty"` // param key -> glob value
}

// Match checks if this rule matches a tool call.
func (r *PolicyRule) Match(toolName string, params map[string]any) bool {
	if !r.Enabled {
		return false
	}
	if !matchGlob(r.ToolPattern, toolName) {
		return false
	}
	for k, v := range r.ParamMatch {
		actual, ok := params[k]
		if !ok {
			return false
		}
		actualStr := fmt.Sprintf("%v", actual)
		if !matchGlob(v, actualStr) {
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

// PolicyEngine evaluates permission rules for tool invocations. It is the single
// authoritative store for permission rules; when a PolicyStorage is attached,
// rule mutations are persisted so choices like "始终允许" survive restarts.
type PolicyEngine struct {
	mu      sync.RWMutex
	rules   []PolicyRule
	storage PolicyStorage
}

// NewPolicyEngine creates an empty policy engine.
func NewPolicyEngine() *PolicyEngine {
	return &PolicyEngine{
		rules: make([]PolicyRule, 0),
	}
}

// SetStorage attaches a persistent backing store. Subsequent AddRule/RemoveRule
// calls write the full rule set through it (best-effort).
func (pe *PolicyEngine) SetStorage(s PolicyStorage) {
	pe.mu.Lock()
	defer pe.mu.Unlock()
	pe.storage = s
}

// persist writes the current rule set to storage. Caller must hold pe.mu.
func (pe *PolicyEngine) persistLocked() {
	if pe.storage == nil {
		return
	}
	snapshot := make([]PolicyRule, len(pe.rules))
	copy(snapshot, pe.rules)
	_ = pe.storage.Save(snapshot) // best-effort; rules still apply in-memory if save fails
}

// Evaluate checks all rules and returns the first matching action.
// Returns ActionAsk if no rule matches.
func (pe *PolicyEngine) Evaluate(toolName string, params map[string]any, mode string) PolicyAction {
	pe.mu.RLock()
	defer pe.mu.RUnlock()

	// Sort by priority (higher first) — rules are kept sorted on insert
	for _, rule := range pe.rules {
		if rule.Match(toolName, params) {
			return rule.Action
		}
	}

	// Default: ask for confirmation in default mode, allow in auto mode
	if mode == "auto" {
		return ActionAllow
	}
	return ActionAsk
}

// AddRule inserts a rule and maintains priority ordering.
func (pe *PolicyEngine) AddRule(rule PolicyRule) error {
	if rule.ID == "" {
		return fmt.Errorf("rule ID is required")
	}
	pe.mu.Lock()
	defer pe.mu.Unlock()

	// Replace existing rule with same ID
	for i, existing := range pe.rules {
		if existing.ID == rule.ID {
			pe.rules[i] = rule
			pe.sortRules()
			pe.persistLocked()
			return nil
		}
	}

	pe.rules = append(pe.rules, rule)
	pe.sortRules()
	pe.persistLocked()
	return nil
}

// RemoveRule deletes a rule by ID.
func (pe *PolicyEngine) RemoveRule(ruleID string) error {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	for i, rule := range pe.rules {
		if rule.ID == ruleID {
			pe.rules = append(pe.rules[:i], pe.rules[i+1:]...)
			pe.persistLocked()
			return nil
		}
	}
	return fmt.Errorf("rule %q not found", ruleID)
}

// ListRules returns all rules, sorted by priority.
func (pe *PolicyEngine) ListRules() []PolicyRule {
	pe.mu.RLock()
	defer pe.mu.RUnlock()

	result := make([]PolicyRule, len(pe.rules))
	copy(result, pe.rules)
	return result
}

// LoadRules replaces all rules with the given set.
func (pe *PolicyEngine) LoadRules(rules []PolicyRule) {
	pe.mu.Lock()
	defer pe.mu.Unlock()
	pe.rules = make([]PolicyRule, len(rules))
	copy(pe.rules, rules)
	pe.sortRules()
}

func (pe *PolicyEngine) sortRules() {
	// Sort by priority descending (stable to preserve insertion order for equal priority)
	for i := 1; i < len(pe.rules); i++ {
		j := i
		for j > 0 && pe.rules[j].Priority > pe.rules[j-1].Priority {
			pe.rules[j], pe.rules[j-1] = pe.rules[j-1], pe.rules[j]
			j--
		}
	}
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
