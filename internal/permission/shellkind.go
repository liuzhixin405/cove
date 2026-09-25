package permission

import (
	"path/filepath"
	"strings"

	"github.com/liuzhixin405/cove/internal/shell"
)

// ShellKind is the quoting family of the interpreter a shell tool runs its
// command line with. It decides whether an operator character inside a quoted
// word can be trusted to be an argument (see coverableCommands).
type ShellKind string

const (
	// ShellPOSIX is sh/bash (Git Bash on Windows): '...' and "..." quote.
	ShellPOSIX ShellKind = "posix"
	// ShellPowerShell is Windows PowerShell or pwsh: '...' and "..." quote.
	ShellPowerShell ShellKind = "powershell"
	// ShellCmd is cmd.exe: single quotes are ordinary characters and ^ and %
	// still act inside double quotes, so quoted operators are never trusted.
	// The zero value "" is treated the same way.
	ShellCmd ShellKind = "cmd"
)

// trustsQuotes reports whether a fully quoted word is literal for k.
func (k ShellKind) trustsQuotes() bool {
	return k == ShellPOSIX || k == ShellPowerShell
}

// ShellKindOf maps a resolved shell to its quoting family, from its Kind and,
// when that is empty, from the executable name. nil or unrecognised shells
// get the strict ShellCmd.
func ShellKindOf(s *shell.Shell) ShellKind {
	if s == nil {
		return ShellCmd
	}
	switch s.Kind {
	case shell.Bash:
		return ShellPOSIX
	case shell.PowerShell:
		return ShellPowerShell
	case shell.Cmd:
		return ShellCmd
	}
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(s.Path, `\`, "/")))
	base = strings.TrimSuffix(base, ".exe")
	switch base {
	case "sh", "bash", "zsh", "dash", "ksh":
		return ShellPOSIX
	case "pwsh", "powershell":
		return ShellPowerShell
	}
	return ShellCmd
}

// shellKindFor is the quoting family a given tool's command runs under: the
// powershell tool always runs PowerShell; the bash tool runs the configured
// default shell.
func shellKindFor(toolName string, configured ShellKind) ShellKind {
	if strings.EqualFold(toolName, "powershell") {
		return ShellPowerShell
	}
	return configured
}

// ToolShellKind is the quoting family toolName's commands run under on this
// machine: PowerShell for the powershell tool, the resolved default shell
// (shell.Default) for bash.
func ToolShellKind(toolName string) ShellKind {
	if strings.EqualFold(toolName, "powershell") {
		return ShellPowerShell
	}
	sh := shell.Default()
	return ShellKindOf(&sh)
}
