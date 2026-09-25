package permission

import (
	"strings"

	"github.com/liuzhixin405/cove/internal/safety"
)

// simpleCommands is the tokenizer the classifier uses; a variable so a test
// can count how often a line is split.
var simpleCommands = safety.SimpleCommands

type CmdCategory int

const (
	CatUnknown   CmdCategory = iota
	CatSafe                  // 只读，永远安全
	CatGit                   // git 操作，需区分读/写
	CatBuild                 // 构建/测试，需看具体命令
	CatInstall               // 包管理器安装
	CatDangerous             // rm -rf, fork bomb, etc.
)

func (c CmdCategory) String() string {
	switch c {
	case CatSafe:
		return "CatSafe"
	case CatGit:
		return "CatGit"
	case CatBuild:
		return "CatBuild"
	case CatInstall:
		return "CatInstall"
	case CatDangerous:
		return "CatDangerous"
	default:
		return "CatUnknown"
	}
}

// riskRank orders categories from least to most risky, so a compound line
// takes the category of its riskiest command.
func riskRank(c CmdCategory) int {
	switch c {
	case CatSafe:
		return 0
	case CatBuild:
		return 1
	case CatGit:
		return 2
	case CatInstall:
		return 3
	case CatDangerous:
		return 5
	default: // CatUnknown
		return 4
	}
}

type Classifier struct{}

func NewClassifier() *Classifier { return &Classifier{} }

// Classify rates a single simple command. A line holding more than one
// command, a redirect or a substitution is CatUnknown here; ClassifyLine is
// the entry point that looks at every command of a compound line.
func (c *Classifier) Classify(cmd string) CmdCategory {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return CatSafe
	}
	if c.isDangerous(cmd) {
		return CatDangerous
	}
	if c.hasShellControlOperator(cmd) {
		return CatUnknown
	}
	simple := simpleCommands(cmd)
	if len(simple) == 0 {
		return CatUnknown
	}
	return c.classifyWords(simple[0].Words)
}

// ClassifyLine rates a whole command line: every simple command the shell
// would start is classified on its own tokenized words and the riskiest
// category wins. Command or process substitution anywhere in the line, ${...}
// expansion, and any output redirect to a real file make the line CatUnknown.
// Quotes are not trusted (cmd.exe rules); ClassifyLineFor takes the shell.
func (c *Classifier) ClassifyLine(command string) CmdCategory {
	return c.ClassifyLineFor(command, "")
}

// ClassifyLineFor is ClassifyLine for a line run by a shell of the given
// kind. A command with a word the tokenizer may have misread (an operator
// character outside trusted quotes, an unquoted $ under PowerShell; see
// literalWords) is CatUnknown.
func (c *Classifier) ClassifyLineFor(command string, kind ShellKind) CmdCategory {
	cat, _ := c.classifyLine(command, kind)
	return cat
}

// classifyLine is ClassifyLineFor that also returns the simple commands it
// tokenized (nil when it decided before tokenizing), so IsReadOnlyLineFor
// can look at them without splitting the line a second time.
func (c *Classifier) classifyLine(command string, kind ShellKind) (CmdCategory, []safety.SimpleCommand) {
	command = strings.TrimSpace(command)
	if command == "" {
		return CatUnknown, nil
	}
	if c.isDangerous(command) {
		return CatDangerous, nil
	}
	if hasSubstitution(command) || maybePowerShell(kind) && (hasUnquotedBrace(command) || hasTypographicQuote(command)) {
		return CatUnknown, nil
	}
	simple := simpleCommands(command)
	if len(simple) == 0 {
		return CatUnknown, nil
	}
	trustQuotes := quotingTrusted(command, kind)
	worst := CatSafe
	for _, sc := range simple {
		cat := CatUnknown
		if len(sc.Words) > 0 && !writesFile(sc.Redirects) && literalWords(sc, kind, trustQuotes) &&
			(!maybePowerShell(kind) || !powerShellIterator(sc.Words[0])) {
			cat = c.classifyWords(sc.Words)
		}
		if riskRank(cat) > riskRank(worst) {
			worst = cat
		}
	}
	return worst, simple
}

// IsReadOnlyLine reports whether every command in the line is read-only, so
// default mode may run it without asking. Network fetches (curl/wget) are
// CatSafe for the classifier but still egress, so they are excluded here and
// keep asking in default mode.
func (c *Classifier) IsReadOnlyLine(command string) bool {
	return c.IsReadOnlyLineFor(command, "")
}

// IsReadOnlyLineFor is IsReadOnlyLine for a line run by a shell of kind.
// Under ShellCmd (the bash tool fell back to cmd.exe) it is always false: the
// tokenizer does not model cmd's ^ escapes, %VAR% expansion or its quoting,
// so no line is trusted to be read-only there and every command asks. The
// zero ShellKind keeps the classification (strict quoting) for callers that
// only want to know what a line does.
func (c *Classifier) IsReadOnlyLineFor(command string, kind ShellKind) bool {
	if kind == ShellCmd {
		return false
	}
	cat, simple := c.classifyLine(command, kind)
	if cat != CatSafe {
		return false
	}
	for _, sc := range simple {
		if len(sc.Words) > 0 {
			switch programName(sc.Words[0]) {
			case "curl", "wget":
				return false
			}
		}
	}
	return true
}

// AutoApproveLine is the auto-mode test: every command in the line is
// read-only or a build/test command.
func (c *Classifier) AutoApproveLine(command string) bool {
	return c.AutoApproveLineFor(command, "")
}

// AutoApproveLineFor is AutoApproveLine for a line run by a shell of kind.
// Like IsReadOnlyLineFor it is always false under ShellCmd: auto mode does
// not run anything unasked through a cmd.exe fallback.
func (c *Classifier) AutoApproveLineFor(command string, kind ShellKind) bool {
	if kind == ShellCmd {
		return false
	}
	cat := c.ClassifyLineFor(command, kind)
	return cat == CatSafe || cat == CatBuild
}

func hasSubstitution(command string) bool {
	for _, op := range []string{"`", "$(", "<(", ">(", "${"} {
		if strings.Contains(command, op) {
			return true
		}
	}
	return false
}

// maybePowerShell reports whether a line of this kind may be run by
// PowerShell: PowerShell itself, or an unknown shell (treated strictly).
func maybePowerShell(kind ShellKind) bool {
	return kind == ShellPowerShell || kind == ""
}

// powerShellIterator names the PowerShell cmdlets and aliases that run a
// scriptblock or member for every pipeline item (where { rm $_ }, % Delete).
func powerShellIterator(word string) bool {
	switch programName(word) {
	case "where", "where-object", "?", "foreach", "foreach-object", "%":
		return true
	}
	return false
}

// hasTypographicQuote reports U+2018–U+201F (‘ ’ ‚ ‛ “ ” „ ‟). PowerShell
// treats them as ' and ", the tokenizer does not, so under PowerShell any
// line containing one is never trusted: quote-dependent checks (scriptblock
// braces, ; | & inside quotes) would be reading a different line.
func hasTypographicQuote(command string) bool {
	for _, r := range command {
		if r >= '\u2018' && r <= '\u201f' {
			return true
		}
	}
	return false
}

// hasUnquotedBrace reports a { or } outside '...' and "..." — a PowerShell
// scriptblock, which runs code wherever it is passed.
func hasUnquotedBrace(command string) bool {
	var q rune
	for _, r := range command {
		switch {
		case q != 0:
			if r == q {
				q = 0
			}
		case r == '\'' || r == '"':
			q = r
		case r == '{' || r == '}':
			return true
		}
	}
	return false
}

func writesFile(redirects []string) bool {
	for _, r := range redirects {
		if !discardTarget(r) {
			return true
		}
	}
	return false
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

// isUNCPath reports whether a word (or the value of a --flag=value word)
// names a UNC / SMB path such as \\host\share or //host/share. Opening one
// contacts another host, and on Windows sends the user's credentials to it,
// so a "read" of it is not a local read. A leading separator pair followed by
// a space or another separator (a "// TODO" grep pattern) is not a host name.
func isUNCPath(word string) bool {
	check := func(w string) bool {
		w = strings.Trim(w, `"'`)
		if len(w) < 3 || !strings.HasPrefix(w, `\\`) && !strings.HasPrefix(w, "//") {
			return false
		}
		switch w[2] {
		case '/', '\\', ' ', '\t':
			return false
		}
		return true
	}
	if check(word) {
		return true
	}
	if i := strings.IndexByte(word, '='); i >= 0 && check(word[i+1:]) {
		return true
	}
	return false
}

// classifyWords rates one simple command from its words exactly as written.
// Anything run through a wrapper (env, sudo, xargs, time, nohup), started via
// a path, or preceded by VAR=value is CatUnknown: the classifier only vouches
// for commands it can name.
func (c *Classifier) classifyWords(words []string) CmdCategory {
	if len(words) == 0 {
		return CatUnknown
	}
	exe := words[0]
	if strings.ContainsAny(exe, `/\$=*?[`) || strings.HasPrefix(exe, "-") {
		return CatUnknown
	}
	name := programName(exe)
	args := words[1:]
	for _, a := range args {
		if isUNCPath(a) {
			return CatUnknown
		}
	}
	switch name {
	case "git":
		return c.classifyGit(args)
	case "ls", "dir", "pwd", "echo", "cat", "head", "tail", "wc", "du", "df", "printenv",
		"which", "where", "whoami", "uname", "uptime", "id", "groups", "grep", "egrep", "fgrep",
		"locate", "stat", "type", "realpath", "basename", "dirname", "true",
		"get-childitem", "gci", "get-content", "gc", "get-location", "gl", "select-string", "sls",
		"test-path", "get-item", "gi", "resolve-path", "get-command", "gcm":
		return CatSafe
	case "date":
		for _, a := range args {
			if a == "-s" || strings.HasPrefix(a, "--set") {
				return CatUnknown
			}
		}
		return CatSafe
	case "hostname":
		if len(args) == 0 {
			return CatSafe
		}
		return CatUnknown
	case "ag":
		for _, a := range args {
			if a == "--pager" || strings.HasPrefix(a, "--pager=") {
				return CatUnknown
			}
		}
		return CatSafe
	case "rg":
		for _, a := range args {
			if a == "--pre" || strings.HasPrefix(a, "--pre=") || strings.HasPrefix(a, "--pre-glob") {
				return CatUnknown
			}
		}
		return CatSafe
	case "tree":
		// -o writes a file, -H emits HTML, -R writes 00Tree.html per directory;
		// short options combine (-aR, -ao out.txt, -fH .).
		for _, a := range args {
			if strings.HasPrefix(a, "--output") {
				return CatUnknown
			}
			if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsAny(a[1:], "oHR") {
				return CatUnknown
			}
		}
		return CatSafe
	case "file":
		for _, a := range args {
			if a == "-C" || a == "--compile" {
				return CatUnknown
			}
		}
		return CatSafe
	case "find":
		return classifyFind(args)
	case "go", "cargo", "rustc", "javac", "tsc", "make", "cmake", "ninja", "bazel", "meson":
		return c.classifyBuild(name, args)
	case "npm", "yarn", "pnpm", "pip", "pip3", "gem", "composer", "nuget", "apt", "apt-get",
		"yum", "dnf", "brew", "choco", "winget", "pacman", "zypper", "snap", "flatpak":
		return c.classifyPackageManager(name, args)
	case "docker", "podman", "nerdctl":
		return c.classifyDocker(args)
	case "curl", "wget":
		return c.classifyNetworkingWords(words)
	default:
		return CatUnknown
	}
}

// classifyFind allows find only without its actions that delete, run
// commands or write files.
func classifyFind(args []string) CmdCategory {
	for _, a := range args {
		switch {
		case a == "-delete", a == "-exec", a == "-execdir", a == "-ok", a == "-okdir",
			strings.HasPrefix(a, "-fprint"), a == "-fls":
			return CatUnknown
		}
	}
	return CatSafe
}

func hasAny(args []string, want ...string) bool {
	for _, a := range args {
		for _, w := range want {
			if a == w {
				return true
			}
		}
	}
	return false
}

// onlyFlagsFrom reports whether every arg is one of allowed.
func onlyFlagsFrom(args []string, allowed ...string) bool {
	for _, a := range args {
		if !hasAny([]string{a}, allowed...) {
			return false
		}
	}
	return true
}

// classifyNetworking rates curl/wget invocations.
//
// The default is CatUnknown (ask the user), not CatSafe. The old default
// auto-approved anything it did not specifically recognize, which covered the
// two cases that matter most: `curl -X POST -d @secrets.json <url>` exfiltrates
// data, and `wget <url>` (no -O) writes a file into the working directory —
// both ran without a prompt. Only genuinely read-only fetches are auto-approved.
func (c *Classifier) classifyNetworking(cmd string) CmdCategory {
	return c.classifyNetworkingWords(strings.Fields(cmd))
}

func (c *Classifier) classifyNetworkingWords(fields []string) CmdCategory {
	// Short flags are matched CASE-SENSITIVELY, on tokenized arguments.
	//
	// Case matters: for curl, -F is a multipart form upload while -f is
	// --fail, and -T is an upload while -t is unrelated. Lowercasing the
	// command first (as an earlier version of this function did) conflated
	// them, so an ordinary read-only `curl -f <url>` was escalated to a
	// confirmation prompt. Tokenizing matters too: a substring scan for " -d "
	// also fires on a URL or a quoted body that happens to contain it.
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
