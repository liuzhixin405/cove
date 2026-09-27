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

	cases := map[string]string{
		"dotnet build":              "dotnet build",     // not remembered: names the command
		"mkdir a && dotnet new sln": "dotnet new",       // the uncovered one of a compound line
		"mkdir $(date +%F)":         "命令替换",             // substitution
		"mkdir a > log.txt":         "重定向",              // writes a file
		"sudo mkdir a":              "sudo",             // runner
		"git push --force":          "git push --force", // in the group's tool but refused
		"mkdir a; rm -rf b":         "rm",               // uncovered command named
	}
	for cmd, want := range cases {
		got := m.ExplainUncovered("bash", map[string]any{"command": cmd})
		if !strings.Contains(got, want) {
			t.Errorf("ExplainUncovered(%q) = %q, want it to mention %q", cmd, got, want)
		}
		if !strings.Contains(got, "mkdir") || !strings.Contains(got, "git 常规操作") {
			t.Errorf("ExplainUncovered(%q) = %q does not list what is remembered", cmd, got)
		}
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
