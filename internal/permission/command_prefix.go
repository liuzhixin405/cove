package permission

import (
	"regexp"
	"strings"

	"github.com/liuzhixin405/cove/internal/safety"
)

// IsShellTool reports whether a tool runs its "command" input through a shell,
// so an "always allow" answer for it is scoped to a command prefix instead of
// the whole tool.
func IsShellTool(name string) bool {
	return strings.EqualFold(name, "bash") || strings.EqualFold(name, "powershell")
}

// CommandPrefixes returns the prefixes an "always allow" answer for command
// should remember: one per distinct simple command in the line, deduplicated
// and in order. A prefix is the executable plus its subcommand for tools whose
// first argument picks what they do (git status, go test, npm run, docker
// compose), and just the executable otherwise (ls, cat). "Allow git" would
// otherwise grant git push and git reset --hard when the user only saw git
// status.
//
// It reports false when no prefix can be remembered safely: a command that
// runs another command (sudo, env, xargs, bash -c ...), a VAR=value or
// variable in front, a subcommand tool without a subcommand, or a line that
// commandCovered would refuse anyway. The rules it returns always cover the
// command they came from.
func CommandPrefixes(command string) ([]string, bool) {
	cmds, ok := coverableCommands(command)
	if !ok {
		return nil, false
	}
	var out []string
	seen := map[string]bool{}
	for _, words := range cmds {
		p, ok := commandPrefix(words)
		if !ok {
			return nil, false
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, true
}

var (
	// subcommandTools take a subcommand as their first argument, and the
	// subcommands differ enough in risk that allowing one must not allow all.
	subcommandTools = map[string]bool{
		"git": true, "gh": true, "glab": true,
		"go": true, "cargo": true, "rustup": true, "dotnet": true,
		"npm": true, "pnpm": true, "yarn": true, "bun": true, "deno": true, "npx": true,
		"pip": true, "pip3": true, "uv": true, "poetry": true, "conda": true,
		"docker": true, "podman": true, "kubectl": true, "helm": true, "terraform": true,
		"mvn": true, "gradle": true, "gradlew": true, "composer": true, "bundle": true, "gem": true,
		"brew": true, "apt": true, "apt-get": true, "dnf": true, "yum": true,
		"winget": true, "choco": true, "scoop": true, "systemctl": true,
		"az": true, "aws": true, "gcloud": true, "flutter": true, "dart": true,
	}
	// commandRunners exist to run some other command, so a prefix made of
	// them would allow anything at all.
	commandRunners = map[string]bool{
		"sudo": true, "doas": true, "su": true, "runas": true,
		"env": true, "nohup": true, "time": true, "command": true, "nice": true,
		"exec": true, "eval": true, "source": true, ".": true, "xargs": true,
		"timeout": true, "watch": true, "chroot": true, "ssh": true, "busybox": true,
		"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
		"cmd": true, "pwsh": true, "powershell": true, "start": true, "call": true,
		"iex": true, "invoke-expression": true, "invoke-command": true, "icm": true,
		"start-process": true,
	}
	subcommandWord = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._:-]*$`)
)

func commandPrefix(words []string) (string, bool) {
	exe := words[0]
	if strings.ContainsAny(exe, " \t$*?[=") || strings.HasPrefix(exe, "-") {
		return "", false
	}
	name := programName(exe)
	if commandRunners[name] {
		return "", false
	}
	if !subcommandTools[name] {
		return exe, true
	}
	if len(words) < 2 || !subcommandWord.MatchString(words[1]) {
		return "", false
	}
	return exe + " " + words[1], true
}

// programName normalizes an executable word the way the shell resolves it:
// case-insensitive on Windows, directory and .exe dropped.
func programName(exe string) string {
	name := strings.ToLower(exe)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSuffix(name, ".exe")
}

// commandCovered reports whether every simple command in command starts with
// one of prefixes. This is the whole policy for prefix allow rules:
//
//   - Each command of a compound line (&& || ; & | newlines, subshells) must
//     match on its own, so "go test ./... | tee out.txt" also needs "tee".
//   - Words are compared as written: "sudo go test", "FOO=1 go test" and
//     "env go test" do not start with "go test".
//   - Command and process substitution ($(...), backticks, <(...), >(...))
//     never matches, even inside quotes where bash and PowerShell still
//     expand it.
//   - Output redirects only match when they discard (/dev/null, NUL, $null);
//     writing a file is not part of running the allowed command.
//   - A word holding an operator character (; & | < > parentheses) never
//     matches. The tokenizer read it as quoted, but the real shell may not:
//     \" in bash, or a single quote in cmd, leaves the operator live.
//
// Anything refused here simply falls back to asking the user again.
func commandCovered(command string, prefixes [][]string) bool {
	cmds, ok := coverableCommands(command)
	if !ok {
		return false
	}
	for _, words := range cmds {
		matched := false
		for _, p := range prefixes {
			if hasWordPrefix(words, p) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// coverableCommands splits command into the words of each simple command, or
// reports false when the line contains something commandCovered refuses.
func coverableCommands(command string) ([][]string, bool) {
	if strings.ContainsAny(command, "`") ||
		strings.Contains(command, "$(") || strings.Contains(command, "<(") || strings.Contains(command, ">(") {
		return nil, false
	}
	simple := safety.SimpleCommands(command)
	if len(simple) == 0 {
		return nil, false
	}
	out := make([][]string, 0, len(simple))
	for _, c := range simple {
		if len(c.Words) == 0 {
			return nil, false
		}
		for _, r := range c.Redirects {
			if !discardTarget(r) {
				return nil, false
			}
		}
		for _, w := range c.Words {
			if strings.ContainsAny(w, ";&|<>()\n\r") {
				return nil, false
			}
		}
		out = append(out, c.Words)
	}
	return out, true
}

func discardTarget(path string) bool {
	switch strings.ToLower(path) {
	case "/dev/null", "nul", "$null":
		return true
	}
	return false
}

func hasWordPrefix(words, prefix []string) bool {
	if len(prefix) == 0 || len(words) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if words[i] != p {
			return false
		}
	}
	return true
}

// anyCommandHasPrefix is the deny/ask reading of a prefix rule: the rule
// applies as soon as one simple command in the line starts with the prefix,
// including commands inside substitutions.
func anyCommandHasPrefix(command string, prefix []string) bool {
	for _, c := range safety.SimpleCommands(command) {
		if hasWordPrefix(c.Words, prefix) {
			return true
		}
	}
	return false
}

func inputCommand(input map[string]any) string {
	s, _ := input["command"].(string)
	return s
}
