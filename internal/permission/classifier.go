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
	CatFileWrite             // 写文件
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
	for _, d := range dangerous {
		if strings.Contains(strings.ToLower(cmd), d) {
			return true
		}
	}
	return false
}

func (c *Classifier) hasShellControlOperator(cmd string) bool {
	operators := []string{"&&", "||", ";", "|", "`", "$(", " >", "<"}
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
