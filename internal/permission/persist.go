package permission

import (
	"fmt"
	"sort"
	"sync"
)

// persistMu serializes AppendAllowRule's read-modify-write within a process.
var persistMu sync.Mutex

// PersistedRuleID is the policies.json ID of an allow rule granted from the
// prompt, e.g. "allow-bash-git commit", "allow-write",
// "allow-mcp-github-create_issue" (InputEquals values in key order).
func PersistedRuleID(rule Rule) string {
	id := "allow-" + rule.ToolPattern
	switch {
	case rule.CommandPrefix != "":
		id += "-" + rule.CommandPrefix
	case len(rule.InputEquals) > 0:
		keys := make([]string, 0, len(rule.InputEquals))
		for k := range rule.InputEquals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			id += "-" + rule.InputEquals[k]
		}
	}
	return id
}

// AppendAllowRule is AppendAllowRules for one rule.
func AppendAllowRule(store *FilePolicyStorage, rule Rule, scope string) error {
	return AppendAllowRules(store, []Rule{rule}, scope)
}

// AppendAllowRules adds each rule as an enabled allow PolicyRule scoped to
// scope ("" = every project) to the file behind store, replacing entries with
// the same ID and scope and keeping every other entry, other projects'
// included. The new rule set is assembled in memory and saved once, so either
// all rules are written or none. A file that cannot be read or parsed is left
// untouched and reported. Concurrent cove processes are not coordinated: the
// last writer wins.
func AppendAllowRules(store *FilePolicyStorage, add []Rule, scope string) error {
	persistMu.Lock()
	defer persistMu.Unlock()
	rules, err := store.Load()
	if err != nil {
		return fmt.Errorf("read %s: %w", store.Path(), err)
	}
next:
	for _, rule := range add {
		pr := PolicyRule{
			ID:            PersistedRuleID(rule),
			ToolPattern:   rule.ToolPattern,
			Action:        ActionAllow,
			Enabled:       true,
			CommandPrefix: rule.CommandPrefix,
			InputEquals:   rule.InputEquals,
			Scope:         scope,
		}
		for i, r := range rules {
			if r.ID == pr.ID && SameProject(r.Scope, pr.Scope) {
				rules[i] = pr
				continue next
			}
		}
		rules = append(rules, pr)
	}
	return store.Save(rules)
}
