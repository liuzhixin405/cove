package permission

import (
	"strings"
)

type CmdCategory int

const (
	CatUnknown   CmdCategory = iota
	CatSafe                  // 只读，永远安全
	CatGit                   // git 操作，需区分读/写
	CatBuild                 // 构建/测试，需看具体命令
	CatInstall               // 包管理器安装
	CatDangerous             // rm -rf, fork bomb, etc.
)

type Classifier struct{}

func NewClassifier() *Classifier { return &Classifier{} }

func (c *Classifier) Classify(cmd string) CmdCategory {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return CatSafe
	}

	base := c.baseCmd(cmd)

	if c.isDangerous(cmd) {
		return CatDangerous
	}
	if c.hasShellControlOperator(cmd) {
		return CatUnknown
	}

	switch base {
	case "git":
		return c.classifyGit(cmd)
	case "ls", "dir", "pwd", "echo", "cat", "head", "tail", "wc", "du", "df", "env", "printenv",
		"which", "where", "whoami", "hostname", "date", "uname", "uptime", "id", "groups":
		return CatSafe
	case "find", "grep", "rg", "ag", "locate", "file", "stat", "tree":
		return CatSafe
	case "go", "cargo", "rustc", "javac", "tsc", "make", "cmake", "ninja", "bazel", "meson":
		return c.classifyBuild(cmd)
	case "npm", "yarn", "pnpm", "pip", "pip3", "gem", "composer", "nuget", "apt", "apt-get",
		"yum", "dnf", "brew", "choco", "winget", "pacman", "zypper", "snap", "flatpak":
		return c.classifyPackageManager(cmd)
	case "docker", "podman", "nerdctl":
		return c.classifyDocker(cmd)
	case "curl", "wget":
		return c.classifyNetworking(cmd)
	default:
		return CatUnknown
	}
}

func (c *Classifier) baseCmd(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	for i, ch := range cmd {
		if ch == ' ' || ch == '\t' {
			return cmd[:i]
		}
	}
	return cmd
}

func (c *Classifier) isDangerous(cmd string) bool {
	dangerous := []string{
		"rm ", "rmdir", "del ", "rd ",
		"dd ", "mkfs", "fdisk", "parted",
		"shutdown", "reboot", "halt", "poweroff",
		"fork bomb", ":(){", "(){",
		"chmod 777", "chmod -R 777",
		"mv /*", "cp /*",
		"> /dev/sda", "of=/dev/",
		"wget -O - | sh", "curl | sh", "curl | bash", "| sh", "| bash",
		"eval ", "exec ",
		"format c:", "format d:",
		"del /f /s", "rd /s /q",
	}
	lower := strings.ToLower(cmd)
	for _, d := range dangerous {
		if strings.Contains(lower, d) {
			return true
		}
	}
	// Structural checks below catch risk patterns that a pure keyword scan
	// misses — they look at how the command is built, not just what
	// substrings appear in it.
	if c.pipesIntoInterpreter(cmd) {
		return true
	}
	if c.hasIFSObfuscation(cmd) {
		return true
	}
	return false
}

// pipesIntoInterpreter reports whether any stage of a pipe chain feeds
// into a shell/scripting interpreter. The keyword list above only catches
// this when the *source* is literally curl/wget ("curl | sh"), but the
// same risk exists for any source: "echo <payload> | base64 -d | bash" or
// "printf ... | xxd -r -p | python3" smuggle an arbitrary decoded command
// into an interpreter without ever mentioning curl, wget, eval, or exec.
func (c *Classifier) pipesIntoInterpreter(cmd string) bool {
	if !strings.Contains(cmd, "|") {
		return false
	}
	interpreters := map[string]bool{
		"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
		"csh": true, "tcsh": true, "fish": true, "python": true, "python3": true,
		"perl": true, "ruby": true, "node": true, "php": true,
		"powershell": true, "pwsh": true,
	}
	for _, stage := range splitPipeStages(cmd) {
		stage = strings.TrimSpace(stage)
		if stage == "" {
			continue
		}
		base := strings.ToLower(strings.TrimSuffix(c.baseCmd(stage), ".exe"))
		if interpreters[base] {
			return true
		}
	}
	return false
}

// splitPipeStages splits cmd on single "|" pipe boundaries while treating
// "||" (logical OR, unrelated to piping) as a single token so it isn't
// mistaken for two pipe stages.
func splitPipeStages(cmd string) []string {
	const orPlaceholder = "\x00OR\x00"
	normalized := strings.ReplaceAll(cmd, "||", orPlaceholder)
	stages := strings.Split(normalized, "|")
	for i, s := range stages {
		stages[i] = strings.ReplaceAll(s, orPlaceholder, "||")
	}
	return stages
}

// hasIFSObfuscation reports whether cmd references the shell IFS
// (internal field separator) variable — a well-known technique for
// evading substring-based keyword filters. "rm${IFS}-rf${IFS}/" contains
// no literal "rm " substring (IFS substitutes for the space that the
// isDangerous keyword scan looks for), so without this check the command
// above would sail through undetected.
func (c *Classifier) hasIFSObfuscation(cmd string) bool {
	lower := strings.ToLower(cmd)
	return strings.Contains(lower, "$ifs") || strings.Contains(lower, "${ifs")
}

func (c *Classifier) hasShellControlOperator(cmd string) bool {
	// "${" is included alongside the classic "$(" / backtick substitution
	// markers: brace parameter expansion can also be used to construct or
	// hide command content that a keyword scan wouldn't recognize (beyond
	// the specific $IFS case already escalated to CatDangerous above), so
	// it's treated the same as other substitution syntax — forced to
	// CatUnknown for manual review rather than silently classified.
	operators := []string{"&&", "||", ";", "|", "`", "$(", "${", " >", "<"}
	for _, op := range operators {
		if strings.Contains(cmd, op) {
			return true
		}
	}
	return false
}

func (c *Classifier) classifyGit(cmd string) CmdCategory {
	readOnly := []string{"status", "log", "diff", "show", "branch", "remote -v", "ls-files", "ls-tree",
		"rev-parse", "rev-list", "describe", "tag -l", "stash list", "config --get", "config --list",
		"blame", "grep", "shortlog", "reflog", "bisect", "whatchanged", "cherry",
		"checkout --", "restore --source", "worktree list", "submodule status",
		"fetch", "merge-base", "for-each-ref", "cat-file", "count-objects", "fsck --name-objects",
		"gc --dry-run", "notes show", "range-diff", "submodule foreach",
	}

	writeOps := []string{"commit", "add ", "rm ", "mv ", "reset", "rebase", "push", "pull",
		"merge", "branch -d", "branch -D", "tag -d", "stash drop", "stash pop", "stash apply",
		"checkout", "switch", "clean", "gc", "prune",
	}

	for _, ro := range readOnly {
		if strings.Contains(cmd, ro) && !strings.Contains(cmd, "-d") && !strings.Contains(cmd, "-D") {
			if strings.Contains(ro, "--") {
				return CatSafe
			}
			parts := strings.Fields(cmd)
			if len(parts) >= 2 && parts[1] == strings.Fields(ro)[0] {
				return CatSafe
			}
			if strings.Contains(cmd, " "+ro+" ") || strings.HasSuffix(cmd, " "+ro) {
				return CatSafe
			}
		}
	}

	for _, wo := range writeOps {
		if strings.Contains(cmd, " "+wo) || strings.HasPrefix(cmd, "git "+wo) {
			return CatGit
		}
	}

	for _, ro := range readOnly {
		if strings.Contains(cmd, ro) {
			return CatSafe
		}
	}

	return CatGit
}

func (c *Classifier) classifyBuild(cmd string) CmdCategory {
	buildCmds := []string{"build", "run", "test", "bench", "compile", "lint", "vet", "fmt",
		"check", "clippy", "doc", "generate", "init", "mod", "install"}
	for _, bc := range buildCmds {
		if strings.Contains(cmd, " "+bc) || strings.HasSuffix(cmd, " "+bc) {
			return CatBuild
		}
	}
	return CatUnknown
}

func (c *Classifier) classifyPackageManager(cmd string) CmdCategory {
	base := c.baseCmd(cmd)

	installCmds := map[string][]string{
		"npm":  {"install", "i ", "add", "update", "upgrade", "uninstall", "remove", "rm "},
		"yarn": {"add", "remove", "upgrade", "install"},
		"pip":  {"install", "uninstall", "freeze", "download"},
		"pip3": {"install", "uninstall", "freeze", "download"},
		"go":   {"get", "install", "mod tidy"},
	}

	if installs, ok := installCmds[base]; ok {
		for _, ic := range installs {
			if strings.Contains(cmd, " "+ic) || strings.HasPrefix(cmd, base+" "+ic) {
				return CatInstall
			}
		}
	}

	listCmds := []string{"list", "ls ", "info", "show", "view", "search", "find", "outdated", "audit", "why", "explain", "help", "--help", "-h", "version", "--version", "-v", "config list", "config get", "doctor", "cache", "clean", "prune"}
	for _, lc := range listCmds {
		if strings.Contains(cmd, " "+lc) || strings.HasSuffix(cmd, " "+lc) {
			return CatSafe
		}
	}

	return CatInstall
}

func (c *Classifier) classifyDocker(cmd string) CmdCategory {
	readOnly := []string{"ps", "images", "inspect", "logs", "stats", "info", "version", "network ls", "network inspect", "volume ls", "volume inspect", "compose ps", "compose logs", "compose config", "context ls", "system info", "system df"}
	for _, ro := range readOnly {
		if strings.Contains(cmd, ro) {
			return CatSafe
		}
	}
	return CatUnknown
}

func (c *Classifier) classifyNetworking(cmd string) CmdCategory {
	for _, method := range []string{"GET", "HEAD", "OPTIONS"} {
		if strings.Contains(cmd, "-X "+method) {
			return CatSafe
		}
	}
	if strings.Contains(cmd, " -o ") || strings.Contains(cmd, " -O ") {
		return CatUnknown
	}
	return CatSafe
}

func (c *Classifier) ShouldAutoApprove(cmd string) bool {
	cat := c.Classify(cmd)
	return cat == CatSafe || cat == CatBuild
}

func (c *Classifier) Explain(cmd string) string {
	cat := c.Classify(cmd)
	switch cat {
	case CatSafe:
		return "safe: read-only operation"
	case CatGit:
		return "git: may modify repository"
	case CatBuild:
		return "build/test: runs code, may write artifacts"
	case CatInstall:
		return "install: installs packages, may modify system"
	case CatDangerous:
		return "DANGEROUS: may damage system"
	default:
		return "unknown: manual review recommended"
	}
}
