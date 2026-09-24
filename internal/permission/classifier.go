package permission

import (
	"strings"

	"github.com/liuzhixin405/cove/internal/safety"
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

// isDangerous is the hard-block test the engine applies in every permission
// mode, so it only covers damage outside the project (see
// safety.CatastrophicCommand). Everything else that writes or deletes is
// CatUnknown and goes through the normal approval prompt.
func (c *Classifier) isDangerous(cmd string) bool {
	_, ok := safety.CatastrophicCommand(cmd)
	return ok
}

func (c *Classifier) hasShellControlOperator(cmd string) bool {
	// "${" is included alongside the classic "$(" / backtick substitution
	// markers: brace parameter expansion can also be used to construct or
	// hide command content that a keyword scan wouldn't recognize (beyond
	// the specific $IFS case already escalated to CatDangerous above), so
	// it's treated the same as other substitution syntax — forced to
	// CatUnknown for manual review rather than silently classified.
	operators := []string{"&&", "||", ";", "|", "`", "$(", "${", ">", "<"}
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
		"blame", "grep", "shortlog", "reflog", "whatchanged", "cherry",
		"worktree list", "submodule status",
		"fetch", "merge-base", "for-each-ref", "cat-file", "count-objects", "fsck --name-objects",
		"gc --dry-run", "notes show", "range-diff",
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

// classifyNetworking rates curl/wget invocations.
//
// The default is CatUnknown (ask the user), not CatSafe. The old default
// auto-approved anything it did not specifically recognize, which covered the
// two cases that matter most: `curl -X POST -d @secrets.json <url>` exfiltrates
// data, and `wget <url>` (no -O) writes a file into the working directory —
// both ran without a prompt. Only genuinely read-only fetches are auto-approved.
func (c *Classifier) classifyNetworking(cmd string) CmdCategory {
	// Short flags are matched CASE-SENSITIVELY, on tokenized arguments.
	//
	// Case matters: for curl, -F is a multipart form upload while -f is
	// --fail, and -T is an upload while -t is unrelated. Lowercasing the
	// command first (as an earlier version of this function did) conflated
	// them, so an ordinary read-only `curl -f <url>` was escalated to a
	// confirmation prompt. Tokenizing matters too: a substring scan for " -d "
	// also fires on a URL or a quoted body that happens to contain it.
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return CatUnknown
	}
	args := fields[1:]

	hasArg := func(want ...string) bool {
		for _, a := range args {
			// Split "--data=x" / "--output=x" at the "=" so both spellings match.
			name := a
			if i := strings.IndexByte(name, '='); i > 0 {
				name = name[:i]
			}
			for _, w := range want {
				if name == w {
					return true
				}
			}
		}
		return false
	}
	hasArgPrefix := func(prefix string) bool {
		for _, a := range args {
			if strings.HasPrefix(a, prefix) {
				return true
			}
		}
		return false
	}
	// method returns the value of -X/--request, uppercased ("" when absent).
	method := ""
	for i, a := range args {
		if (a == "-X" || a == "--request") && i+1 < len(args) {
			method = strings.ToUpper(args[i+1])
			break
		}
		if strings.HasPrefix(a, "--request=") {
			method = strings.ToUpper(strings.TrimPrefix(a, "--request="))
			break
		}
	}

	// Any explicit non-read method needs confirmation.
	switch method {
	case "POST", "PUT", "PATCH", "DELETE":
		return CatUnknown
	}

	// Request-body and upload flags mean data is being sent outward, whatever
	// the method ends up being (curl infers POST from -d/-F/--data-*).
	if hasArg("-d", "--data", "-F", "--form", "-T", "--upload-file",
		"--post-data", "--post-file", "--data-binary", "--data-raw",
		"--data-urlencode", "--form-string") || hasArgPrefix("--data") {
		return CatUnknown
	}
	// Disabling certificate verification turns a fetch into an untrusted one.
	if hasArg("-k", "--insecure", "--no-check-certificate") {
		return CatUnknown
	}

	// Writing the response to disk needs confirmation. "-o -" / "-O -" targets
	// stdout, which is still a read.
	outputToStdout := false
	for i, a := range args {
		if (a == "-o" || a == "-O" || a == "--output") && i+1 < len(args) && args[i+1] == "-" {
			outputToStdout = true
			break
		}
	}
	if !outputToStdout {
		if hasArg("-o", "-O", "--output", "--remote-name", "--output-dir") {
			return CatUnknown
		}
		// wget writes a file by default, so without an explicit stdout target
		// it is never a pure read.
		if base := strings.ToLower(fields[0]); strings.HasSuffix(base, "wget") ||
			strings.HasSuffix(base, "wget.exe") {
			return CatUnknown
		}
	}

	switch method {
	case "GET", "HEAD", "OPTIONS":
		return CatSafe
	}
	// A plain `curl <url>` prints to stdout and is a read.
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
