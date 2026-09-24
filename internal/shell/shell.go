// Package shell picks the command interpreter cove runs shell commands with.
//
// Every place that executes a command string — the bash tool, the done-verify
// gate, the environment line of the system prompt — has to agree on the shell,
// or the model writes bash syntax for a cmd.exe it was never told about.
package shell

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// Kind is the family of a shell, which decides its argument syntax.
type Kind string

const (
	Bash       Kind = "bash"
	PowerShell Kind = "powershell"
	Cmd        Kind = "cmd"
)

// Shell is a resolved interpreter.
type Shell struct {
	Kind Kind
	Path string
}

// Args returns the arguments that make the shell run command and exit.
func (s Shell) Args(command string) []string {
	switch s.Kind {
	case PowerShell:
		// -NoProfile for startup time, -NonInteractive so nothing waits on a
		// prompt, and UTF-8 output: PowerShell otherwise writes to a pipe in
		// the console code page (GBK on a Chinese system).
		return []string{"-NoProfile", "-NonInteractive", "-Command", psUTF8 + command}
	case Cmd:
		return []string{"/C", command}
	default:
		return []string{"-c", command}
	}
}

// Command builds an exec.Cmd that runs command in this shell.
func (s Shell) Command(command string) *exec.Cmd {
	return exec.Command(s.Path, s.Args(command)...)
}

// Describe names the shell for the model, e.g. "bash (D:\Program Files\Git\bin\bash.exe)".
func (s Shell) Describe() string {
	return string(s.Kind) + " (" + s.Path + ")"
}

// Env returns base with the variables that keep a command from waiting on
// input nobody can type: no commit-message editor, no terminal credential
// prompt, no pager. A later entry wins, so these override the user's own.
func Env(base []string) []string {
	return append(append([]string(nil), base...),
		"GIT_EDITOR=true",
		"GIT_SEQUENCE_EDITOR=true",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"PAGER=cat",
	)
}

// psUTF8 switches PowerShell's output to UTF-8 without a byte-order mark.
const psUTF8 = "$OutputEncoding=[Console]::OutputEncoding=New-Object System.Text.UTF8Encoding $false; "

var (
	once     sync.Once
	resolved Shell
)

// Default returns the shell for this machine, resolved once per process.
func Default() Shell {
	once.Do(func() {
		resolved = detect(probe{
			goos:     runtime.GOOS,
			lookPath: exec.LookPath,
			exists: func(p string) bool {
				info, err := os.Stat(p)
				return err == nil && !info.IsDir()
			},
			getenv: os.Getenv,
		})
	})
	return resolved
}

// probe is the view of the host detect needs, injectable for tests.
type probe struct {
	goos     string
	lookPath func(string) (string, error)
	exists   func(string) bool
	getenv   func(string) string
}

func detect(p probe) Shell {
	if p.goos != "windows" {
		for _, name := range []string{"bash", "sh"} {
			if path, err := p.lookPath(name); err == nil {
				return Shell{Kind: Bash, Path: path}
			}
		}
		return Shell{Kind: Bash, Path: "/bin/sh"}
	}

	if path := gitBash(p); path != "" {
		return Shell{Kind: Bash, Path: path}
	}
	// A bash on PATH is only usable when it is not one of the WSL launchers:
	// System32\bash.exe and WindowsApps\bash.exe hand the command to a Linux
	// distro, which may not exist and never shares the Windows toolchain.
	if path, err := p.lookPath("bash"); err == nil && !isWSLLauncher(path) {
		return Shell{Kind: Bash, Path: path}
	}
	for _, name := range []string{"pwsh", "powershell"} {
		if path, err := p.lookPath(name); err == nil {
			return Shell{Kind: PowerShell, Path: path}
		}
	}
	return Shell{Kind: Cmd, Path: "cmd"}
}

// gitBash locates Git for Windows' bash. Git\bin\bash.exe is preferred over
// Git\usr\bin\bash.exe: it is the wrapper that puts the coreutils on PATH.
func gitBash(p probe) string {
	var roots []string
	if git, err := p.lookPath("git"); err == nil {
		// git.exe lives in <root>\cmd, <root>\bin or <root>\mingw64\bin.
		dir := winDir(git)
		roots = append(roots, winDir(dir), winDir(winDir(dir)))
	}
	for _, env := range []string{"ProgramFiles", "ProgramW6432", "LOCALAPPDATA"} {
		if base := p.getenv(env); base != "" {
			if env == "LOCALAPPDATA" {
				base += `\Programs`
			}
			roots = append(roots, base+`\Git`)
		}
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		for _, rel := range []string{`\bin\bash.exe`, `\usr\bin\bash.exe`} {
			if candidate := root + rel; p.exists(candidate) {
				return candidate
			}
		}
	}
	return ""
}

func isWSLLauncher(path string) bool {
	lower := strings.ToLower(strings.ReplaceAll(path, "/", `\`))
	return strings.HasSuffix(lower, `\system32\bash.exe`) ||
		strings.HasSuffix(lower, `\sysnative\bash.exe`) ||
		strings.Contains(lower, `\windowsapps\`)
}

// winDir is filepath.Dir for Windows paths, independent of the build OS.
func winDir(path string) string {
	if i := strings.LastIndexAny(path, `\/`); i > 0 {
		return path[:i]
	}
	return ""
}
