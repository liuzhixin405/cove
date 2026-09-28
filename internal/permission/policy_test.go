package permission

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMatchRule_ArgPatternNilInputNoBypass(t *testing.T) {
	r := Rule{ToolPattern: "bash", Decision: DAllow, ArgPattern: "rm -rf"}
	// nil input must NOT match an arg-restricted rule (previously it fell through
	// to true, auto-allowing any bash call with a nil Input map).
	if matchRule(r, "bash", nil) {
		t.Fatal("arg-restricted rule must not match nil input")
	}
	if matchRule(r, "bash", map[string]any{"command": "ls"}) {
		t.Fatal("should not match when arg pattern absent")
	}
	if !matchRule(r, "bash", map[string]any{"command": "rm -rf /tmp/x"}) {
		t.Fatal("should match when arg pattern present")
	}
}

// Rules written to the policies file are read back by a fresh storage on
// the same file (a "[p]" answer survives a restart).
func TestPolicyStoragePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policies.json")
	store, err := NewFilePolicyStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []PolicyRule{{ID: "user-allow-write", ToolPattern: "write", Action: ActionAllow, Enabled: true, Priority: 100}}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	store2, _ := NewFilePolicyStorage(path)
	got, err := store2.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "user-allow-write" || got[0].Priority != 100 || !got[0].Enabled {
		t.Fatalf("rules after restart = %+v", got)
	}
	if err := store2.Save(nil); err != nil {
		t.Fatal(err)
	}
	store3, _ := NewFilePolicyStorage(path)
	if rules, _ := store3.Load(); len(rules) != 0 {
		t.Fatalf("removal not persisted: %d rules remain", len(rules))
	}
}

func TestPolicyRuleToRule(t *testing.T) {
	got, ok := PolicyRule{CommandPrefix: "git commit"}.ToRule()
	if !ok || !reflect.DeepEqual(got, Rule{ToolPattern: "bash", CommandPrefix: "git commit"}) {
		t.Fatalf("ToRule = %+v, %v; want bash/git commit", got, ok)
	}
	got, ok = PolicyRule{ToolPattern: "powershell", CommandPrefix: "go test"}.ToRule()
	if !ok || got.ToolPattern != "powershell" || got.CommandPrefix != "go test" {
		t.Fatalf("powershell prefix rule = %+v, %v", got, ok)
	}
	got, ok = PolicyRule{ToolPattern: "mcp", InputEquals: map[string]string{"serverName": "gh", "toolName": "x"}}.ToRule()
	if !ok || got.InputEquals["toolName"] != "x" {
		t.Fatalf("mcp rule = %+v, %v", got, ok)
	}
	if got, ok := (PolicyRule{ToolPattern: "write"}).ToRule(); !ok || got.ToolPattern != "write" {
		t.Fatalf("whole-tool rule = %+v, %v", got, ok)
	}
	// Globs, param matches and priorities convert too: the Manager is the
	// only evaluator, so a rule that did not convert used to be judged by a
	// second one with different precedence.
	got, ok = PolicyRule{ToolPattern: "mcp_*", Priority: 3}.ToRule()
	if !ok || got.ToolPattern != "mcp_*" || got.Priority != 3 {
		t.Fatalf("glob rule = %+v, %v", got, ok)
	}
	got, ok = PolicyRule{ToolPattern: "bash", ParamMatch: map[string]string{"command": "git *"}}.ToRule()
	if !ok || got.ParamMatch["command"] != "git *" {
		t.Fatalf("param_match rule = %+v, %v", got, ok)
	}
	if got, ok := (PolicyRule{}).ToRule(); ok {
		t.Errorf("empty rule converted to %+v", got)
	}
}

// The precedence the manual documents, now in one evaluator: a deny wins
// whatever its priority; among ask and allow the higher priority wins and a
// tie goes to ask; a glob tool pattern and param_match apply like any rule.
func TestManagerPrecedenceWithPolicyRules(t *testing.T) {
	add := func(m *Manager, d Decision, r PolicyRule) {
		rule, ok := r.ToRule()
		if !ok {
			t.Fatalf("%+v did not convert", r)
		}
		m.AddRule(d, rule)
	}
	git := map[string]any{"command": "git push"}

	m := NewManager(Default)
	add(m, DAllow, PolicyRule{ToolPattern: "bash", CommandPrefix: "git", Priority: 100})
	add(m, DDeny, PolicyRule{ToolPattern: "bash", Priority: 1})
	if d, _ := m.Check("bash", git, DAsk); d != DDeny {
		t.Errorf("a low-priority deny lost to a high-priority allow: %s", d)
	}

	m = NewManager(Default)
	add(m, DAllow, PolicyRule{ToolPattern: "bash", CommandPrefix: "git push", Priority: 5})
	add(m, DAsk, PolicyRule{ToolPattern: "bash", CommandPrefix: "git push"})
	if d, _ := m.Check("bash", git, DAsk); d != DAllow {
		t.Errorf("a higher-priority allow did not beat a lower ask: %s", d)
	}
	m = NewManager(Default)
	add(m, DAllow, PolicyRule{ToolPattern: "bash", CommandPrefix: "git push"})
	add(m, DAsk, PolicyRule{ToolPattern: "bash", CommandPrefix: "git push"})
	if d, _ := m.Check("bash", git, DAsk); d != DAsk {
		t.Errorf("ask did not win a priority tie: %s", d)
	}

	m = NewManager(Bypass)
	m.SetBypassAvailable(true)
	add(m, DDeny, PolicyRule{ToolPattern: "mcp__*"})
	if d, _ := m.Check("mcp__github__delete_repo", nil, DAllow); d != DDeny {
		t.Errorf("a glob deny did not stop bypass: %s", d)
	}
	add(m, DDeny, PolicyRule{ToolPattern: "write", ParamMatch: map[string]string{"filePath": "*.env"}})
	if d, _ := m.Check("write", map[string]any{"filePath": "prod.env"}, DAllow); d != DDeny {
		t.Errorf("a param_match deny did not apply: %s", d)
	}
	if d, _ := m.Check("write", map[string]any{"filePath": "main.go"}, DAllow); d != DBypass {
		t.Errorf("a param_match deny applied to another file: %s", d)
	}
}

func TestPolicyRuleJSONFormat(t *testing.T) {
	data, err := json.Marshal(PolicyRule{ID: "allow-bash-git commit", ToolPattern: "bash", Action: ActionAllow, Enabled: true, CommandPrefix: "git commit", Scope: `D:\proj`})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"command_prefix":"git commit"`, `"scope":"D:\\proj"`, `"tool_pattern":"bash"`, `"action":"allow"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("JSON %s lacks %s", data, want)
		}
	}
}
