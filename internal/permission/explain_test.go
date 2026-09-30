package permission

import (
	"strings"
	"testing"
)

// When a remembered rule does not cover the line being asked about, the
// manager says why, so the person sees the cause instead of "asked again".
func TestExplainUncovered(t *testing.T) {
	m := NewManager(Default)
	m.SetShellKind(ShellPOSIX)
	if got := m.ExplainUncovered("bash", map[string]any{"command": "mkdir a"}); got != "" {
		t.Fatalf("nothing remembered yet, want no explanation, got %q", got)
	}
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "mkdir"})
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandGroup: GroupGitRoutine})

	// Each explanation names the remembered rule of a program the line runs,
	// and only that one.
	cases := []struct{ cmd, want, rule, notRule string }{
		{"mkdir a && dotnet new sln", "dotnet new", `"mkdir"`, "git"},   // the uncovered one of a compound line
		{"mkdir $(date +%F)", "命令替换", `"mkdir"`, "git"},                 // substitution
		{"mkdir a > log.txt", "重定向", `"mkdir"`, "git"},                  // writes a file
		{"sudo mkdir a", "sudo", `"mkdir"`, "git"},                      // runner
		{"git push --force", "git push --force", "git 常规操作", `"mkdir"`}, // in the group's tool but refused
		{"mkdir a; rm -rf b", "rm", `"mkdir"`, "git"},                   // uncovered command named
	}
	for _, c := range cases {
		got := m.ExplainUncovered("bash", map[string]any{"command": c.cmd})
		if !strings.Contains(got, c.want) || !strings.Contains(got, c.rule) {
			t.Errorf("ExplainUncovered(%q) = %q, want it to mention %q and the rule %s", c.cmd, got, c.want, c.rule)
		}
		if strings.Contains(got, c.notRule) {
			t.Errorf("ExplainUncovered(%q) = %q names the unrelated rule %s", c.cmd, got, c.notRule)
		}
	}
	// A line that runs none of the remembered programs has no gap to
	// explain: "sed … > s2.json" was said to fall outside a docref.exe rule.
	// Only why it cannot be remembered at all is said.
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: `C:\Users\u\.cove\plugins\docref\bin\docref.exe`})
	if got := m.ExplainUncovered("bash", map[string]any{"command": "dotnet build"}); got != "" {
		t.Errorf("dotnet build = %q, want nothing: no remembered rule concerns it", got)
	}
	// A line that cannot be remembered is not explained either: the prompt
	// offers no [a], which says it.
	if got := m.ExplainUncovered("bash", map[string]any{"command": `sed "s/a/b/" s.json > s2.json`}); got != "" {
		t.Errorf("sed > s2.json = %q, want nothing: no remembered rule concerns it", got)
	}
	// A covered line has nothing to explain.
	if got := m.ExplainUncovered("bash", map[string]any{"command": "mkdir a && git commit -m x"}); got != "" {
		t.Errorf("covered line explained: %q", got)
	}
	// The other shell tool: the rules belong to bash.
	if got := m.ExplainUncovered("powershell", map[string]any{"command": "mkdir a"}); !strings.Contains(got, "bash") || !strings.Contains(got, "powershell") {
		t.Errorf("powershell line with bash rules = %q, want a note that the rules are bash's", got)
	}
}

// Build tools get a routine group too: one answer covers the new/build/
// test/add cycle, while installing tools system-wide, forcing overwrites,
// publishing and watch loops keep asking.
func TestBuildToolRoutineGroups(t *testing.T) {
	cases := []struct {
		group   string
		allowed []string
		asked   []string
	}{
		{GroupDotnetRoutine,
			[]string{"dotnet new sln -n Agent", "dotnet new classlib -o src/Core", "dotnet sln add src/Core", "dotnet add src/App reference src/Core",
				"dotnet add package Serilog", "dotnet restore", "dotnet build -c Release", "dotnet test", "dotnet run --project src/App", "dotnet format", "dotnet clean"},
			[]string{"dotnet new console --force", "dotnet tool install -g x", "dotnet watch run", "dotnet nuget push a.nupkg", "dotnet publish -c Release", "dotnet"}},
		{GroupNpmRoutine,
			[]string{"npm install", "npm ci", "npm run build", "npm test", "npm init -y", "pnpm install", "yarn build", "npm install lodash", "npm uninstall lodash"},
			[]string{"npm publish", "npm install -g typescript", "npm exec -- rm -rf x", "npx create-react-app app", "npm login"}},
		{GroupGoRoutine,
			[]string{"go build ./...", "go test ./... -run X", "go vet ./...", "go mod tidy", "go fmt ./...", "go run ./cmd/x", "go get github.com/x/y", "go generate ./..."},
			[]string{"go install golang.org/x/tools/cmd/goimports@latest", "go clean -modcache", "go env -w GOFLAGS=-mod=mod", "go tool pprof x"}},
		{GroupCargoRoutine,
			[]string{"cargo build --release", "cargo test", "cargo check", "cargo fmt", "cargo clippy", "cargo add serde", "cargo run"},
			[]string{"cargo install ripgrep", "cargo publish", "cargo clean", "cargo yank --vers 1.0.0 x"}},
	}
	for _, c := range cases {
		m := NewManager(Default)
		m.SetShellKind(ShellPOSIX)
		m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandGroup: c.group})
		for _, cmd := range c.allowed {
			if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAllow {
				t.Errorf("[%s] %q = %v, want allow", c.group, cmd, d)
			}
		}
		for _, cmd := range c.asked {
			if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAsk {
				t.Errorf("[%s] %q = %v, want ask", c.group, cmd, d)
			}
		}
		if !KnownGroup(c.group) {
			t.Errorf("%s is not a known group", c.group)
		}
	}
	// ShellRememberRules offers the group for a routine build command and
	// a plain prefix for anything else.
	rules, ok := ShellRememberRules("bash", "dotnet new sln -n Agent && dotnet build", ShellPOSIX)
	if !ok || len(rules) != 1 || rules[0].CommandGroup != GroupDotnetRoutine {
		t.Errorf("dotnet cycle remembered as %+v", rules)
	}
	rules, ok = ShellRememberRules("bash", "dotnet publish -c Release", ShellPOSIX)
	if !ok || len(rules) != 1 || rules[0].CommandPrefix != "dotnet publish" {
		t.Errorf("dotnet publish remembered as %+v, want its own prefix", rules)
	}
}
