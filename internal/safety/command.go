package safety

import (
	"regexp"
	"strings"
)

// CatastrophicCommand reports whether a shell command would do irreversible
// damage outside the project: wipe the filesystem, the home directory or a
// system directory, write to a raw disk, take the machine down, or run code
// fetched from the network. It returns a short reason when it does.
//
// The command is split into words and simple commands first, so "git add ."
// does not match "dd " and "rm -rf /tmp/x" does not match "rm -rf /". Earlier
// substring lists hard-blocked everyday commands like those while a command
// with an extra space in it slipped past.
func CatastrophicCommand(command string) (string, bool) {
	return catastrophicCommand(command, 0)
}

// maxShellNesting bounds how deep bash -c "sh -c '...'" is unwrapped. Deeper
// nesting has no legitimate use and is treated as catastrophic.
const maxShellNesting = 8

func catastrophicCommand(command string, depth int) (string, bool) {
	if depth > maxShellNesting {
		return "shell nesting too deep", true
	}
	command = ifsPattern.ReplaceAllString(command, " ")
	// PowerShell accepts en dash, em dash and minus sign as the parameter
	// dash (Remove-Item –Recurse). Normalizing them for every shell only
	// makes the scan stricter.
	command = dashNormalizer.Replace(command)
	if name, ok := forkBomb(command); ok {
		return "fork bomb " + name + "()", true
	}
	for _, pipeline := range parseShell(command) {
		if why, ok := catastrophicPipeline(pipeline); ok {
			return why, true
		}
		if why, ok := catastrophicXargs(pipeline); ok {
			return why, true
		}
		for _, c := range pipeline {
			if inner, encoded, ok := unwrapShell(c.words); ok {
				if encoded {
					return "encoded command", true
				}
				if why, ok := catastrophicCommand(inner, depth+1); ok {
					return why, true
				}
			}
			if len(c.heredocs) > 0 {
				if name, _ := commandWords(c.words); stdinShells[name] {
					for _, h := range c.heredocs {
						if why, ok := catastrophicCommand(h.body, depth+1); ok {
							return why, true
						}
					}
				}
			}
			if why, ok := catastrophicSimple(c); ok {
				return why, true
			}
		}
	}
	return "", false
}

// stdinShells run a here-document or here-string fed to them as a script.
var stdinShells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
	"cmd": true, "pwsh": true, "powershell": true,
}

// unwrapShell recognizes a shell started with an inline command — sh/bash/
// zsh/dash/ksh -c "...", cmd /c ..., pwsh/powershell -Command ... — and
// returns that command string so it is scanned like a top-level one. encoded
// is true for PowerShell's -EncodedCommand, whose payload cannot be read.
// Arguments of other programs (git commit -m "rm -rf /") are never unwrapped.
func unwrapShell(words []string) (inner string, encoded, ok bool) {
	name, args := commandWords(words)
	switch name {
	case "sh", "bash", "zsh", "dash", "ksh", "ash":
		for i := 0; i < len(args); i++ {
			a := args[i]
			switch {
			case a == "--" || !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+"):
				// The first operand is a script file, not an inline command.
				return "", false, false
			case strings.HasPrefix(a, "--"):
				continue
			case a == "-o" || a == "+o" || a == "-O" || a == "+O":
				i++ // the option name that follows
			case strings.HasPrefix(a, "-") && strings.Contains(a[1:], "c"):
				// -c, -lc, -ec ...: the next non-option word is the command.
				for _, b := range args[i+1:] {
					if !strings.HasPrefix(b, "-") && !strings.HasPrefix(b, "+") {
						return b, false, true
					}
				}
				return "", false, false
			}
		}
	case "cmd":
		for i, a := range args {
			la := strings.ToLower(a)
			if la == "/c" || la == "/k" || la == "/r" {
				return strings.Join(args[i+1:], " "), false, true
			}
		}
	case "pwsh", "powershell":
		for i, a := range args {
			la := strings.ToLower(a)
			if !strings.HasPrefix(la, "-") && !strings.HasPrefix(la, "/") {
				continue
			}
			flag := "-" + strings.TrimLeft(la, "-/")
			switch {
			case flag == "-ec" || len(flag) >= 2 && strings.HasPrefix("-encodedcommand", flag):
				// -e, -ec, -enc ... -EncodedCommand: a base64 payload.
				return "", true, true
			case len(flag) >= 2 && strings.HasPrefix("-command", flag):
				return strings.Join(args[i+1:], " "), false, true
			}
		}
	}
	return "", false, false
}

// xargsValueFlags are xargs options that take a separate value.
var xargsValueFlags = map[string]bool{
	"-n": true, "-I": true, "-L": true, "-P": true, "-d": true, "-s": true, "-E": true, "-a": true,
	"--max-args": true, "--max-lines": true, "--max-procs": true, "--delimiter": true,
	"--max-chars": true, "--eof": true, "--arg-file": true,
}

// catastrophicXargs catches `echo ~ | xargs rm -rf`: xargs runs its words as
// a command with the output of the commands before it appended. The output is
// approximated by those commands' arguments; each one is tried as the target.
func catastrophicXargs(p []simpleCmd) (string, bool) {
	for i, c := range p {
		name, args := commandWords(c.words)
		if name != "xargs" || i == 0 {
			continue
		}
		for len(args) > 0 && strings.HasPrefix(args[0], "-") {
			if xargsValueFlags[args[0]] && len(args) > 1 {
				args = args[1:]
			}
			args = args[1:]
		}
		if len(args) == 0 {
			continue
		}
		for _, prev := range p[:i] {
			_, prevArgs := commandWords(prev.words)
			for _, target := range prevArgs {
				if !criticalPath(target) {
					continue
				}
				words := append(append([]string(nil), args...), target)
				if why, ok := catastrophicSimple(simpleCmd{words: words}); ok {
					return "xargs: " + why, true
				}
			}
		}
	}
	return "", false
}

// catastrophicFind catches find <critical path> -delete / -exec rm.
func catastrophicFind(args []string) (string, bool) {
	var paths []string
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") || a == "(" || a == "!" {
			break
		}
		paths = append(paths, a)
	}
	critical := ""
	for _, p := range paths {
		if criticalPath(p) {
			critical = p
			break
		}
	}
	if critical == "" {
		return "", false
	}
	for j := i; j < len(args); j++ {
		switch args[j] {
		case "-delete":
			return "find " + critical + " -delete", true
		case "-exec", "-execdir", "-ok", "-okdir":
			if j+1 < len(args) {
				switch name, _ := commandWords(args[j+1:]); name {
				case "rm", "rmdir", "shred", "unlink", "del", "remove-item":
					return "find " + critical + " " + args[j] + " " + name, true
				}
			}
		}
	}
	return "", false
}

var ifsPattern = regexp.MustCompile(`\$\{?IFS\}?`)

// dashNormalizer maps PowerShell's other dashes (SpecialCharacters.IsDash:
// en dash, em dash, horizontal bar) and the minus sign to "-".
var dashNormalizer = strings.NewReplacer("\u2013", "-", "\u2014", "-", "\u2015", "-", "\u2212", "-")

var funcDef = regexp.MustCompile(`([\w:.]+)\s*\(\)\s*\{([^}]*)\}`)

// forkBomb finds a function that pipes into itself, e.g. :(){ :|:& };:
func forkBomb(command string) (string, bool) {
	for _, m := range funcDef.FindAllStringSubmatch(command, -1) {
		body := strings.Join(strings.Fields(m[2]), "")
		if strings.Contains(body, m[1]+"|"+m[1]) {
			return m[1], true
		}
	}
	return "", false
}

// simpleCmd is one command of a pipeline: its words and output redirect targets.
type simpleCmd struct {
	words     []string
	quoted    []bool // quoted[i]: every character of words[i] came from inside quotes
	redirects []string
	// heredocs are the bodies of << / <<- here-documents and <<< here-strings
	// fed to this command's stdin. They are data, not commands of this line.
	heredocs []*heredoc
}

// parseShell splits a command line into pipelines of simple commands. It knows
// quotes, ; && || & | and newlines, redirects, here-documents, subshells and
// command substitution — enough to find each command's name and arguments.
// Backslash is kept literally: the same string may be a PowerShell or cmd
// command, where it is a path separator.
func parseShell(s string) [][]simpleCmd {
	var (
		pipelines [][]simpleCmd
		pipeline  []simpleCmd
		cur       simpleCmd
		word      strings.Builder
		inWord    bool
		hadQuote  bool // the current word contains a quoted part
		allQuoted = true
		quote     rune
		redirect  bool // the next word is an output redirect target
		skipWord  bool // the next word is an input redirect source
		hereWord  bool // the next word is a here-string (<<<)
		pending   []*heredoc
		// parenDepth and lineComment keep << from being read as a heredoc
		// where bash would not start one — (( x<<2 )) arithmetic, a subshell,
		// or after a # comment — since skipping the "body" would hide
		// commands bash runs.
		parenDepth  int
		lineComment bool
	)
	endWord := func() {
		if !inWord {
			return
		}
		w := word.String()
		switch {
		case redirect:
			cur.redirects = append(cur.redirects, w)
			redirect = false
		case hereWord:
			cur.heredocs = append(cur.heredocs, &heredoc{body: w})
			hereWord = false
		case skipWord:
			skipWord = false
		default:
			cur.words = append(cur.words, w)
			cur.quoted = append(cur.quoted, hadQuote && allQuoted)
		}
		word.Reset()
		inWord = false
		hadQuote = false
		allQuoted = true
	}
	writeUnquoted := func(ch rune) {
		word.WriteRune(ch)
		inWord = true
		allQuoted = false
	}
	endCmd := func() {
		endWord()
		if len(cur.words) > 0 || len(cur.redirects) > 0 || len(cur.heredocs) > 0 {
			pipeline = append(pipeline, cur)
		}
		cur = simpleCmd{}
	}
	endPipeline := func() {
		endCmd()
		if len(pipeline) > 0 {
			pipelines = append(pipelines, pipeline)
		}
		pipeline = nil
	}

	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		ch := rs[i]
		next := rune(0)
		if i+1 < len(rs) {
			next = rs[i+1]
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				word.WriteRune(ch)
			}
			continue
		}
		switch {
		case ch == '\'' || ch == '"':
			quote = ch
			inWord = true
			hadQuote = true
		case ch == ' ' || ch == '\t' || ch == '\r':
			endWord()
		case ch == '\n':
			endPipeline()
			lineComment = false
			if len(pending) > 0 {
				i = readHeredocBodies(rs, i, pending)
				pending = nil
			}
		case ch == '(':
			parenDepth++
			endPipeline()
		case ch == ')':
			if parenDepth > 0 {
				parenDepth--
			}
			endPipeline()
		case ch == ';' || ch == '`':
			endPipeline()
		case ch == '{' || ch == '}':
			if ch == '{' && inWord && strings.HasSuffix(word.String(), "$") {
				// ${VAR}: keep the whole expansion in the word.
				for ; i < len(rs) && rs[i] != '}'; i++ {
					word.WriteRune(rs[i])
				}
				writeUnquoted('}')
				continue
			}
			endPipeline()
		case ch == '$' && next == '(':
			parenDepth++
			endPipeline()
			i++
		case ch == '|':
			if next == '|' {
				endPipeline()
				i++
			} else {
				endCmd()
			}
		case ch == '&':
			if next == '&' {
				i++
			}
			endPipeline()
		case ch == '>':
			// A file descriptor number directly before > belongs to the redirect.
			if inWord && isDigits(word.String()) {
				word.Reset()
				inWord = false
			}
			endWord()
			if next == '>' || next == '|' {
				i++ // >> appends, >| overrides noclobber: both write a file
			}
			if i+1 < len(rs) && rs[i+1] == '&' {
				i++
				// >&WORD copies or closes a descriptor only when WORD is all
				// digits, exactly "-", or digits followed by "-" (2>&1, >&-,
				// >&1-). Any other WORD (>&1b, >&file) is a file bash writes.
				j := i + 1
				for j < len(rs) && !strings.ContainsRune(" \t\r\n;|&<>()'\"`", rs[j]) {
					j++
				}
				// bash removes quotes before judging WORD, so a quote glued to
				// it (>&1'b', 2>&1"b") makes it a file name; treat any glued
				// quote or backtick as a file (conservative for >&1'').
				glued := j < len(rs) && (rs[j] == '\'' || rs[j] == '"' || rs[j] == '`')
				if w := string(rs[i+1 : j]); !glued && isDescriptorWord(w) {
					i = j - 1
					continue
				}
			}
			redirect = true
		case ch == '<':
			// \< is a literal < for bash, so \<<EOF is no heredoc.
			escaped := i > 0 && rs[i-1] == '\\'
			if inWord && isDigits(word.String()) {
				word.Reset()
				inWord = false
			}
			endWord()
			switch {
			case next == '<' && i+2 < len(rs) && rs[i+2] == '<':
				// <<< here-string: the next word is stdin data.
				i += 2
				hereWord = true
			case next == '<' && (escaped || parenDepth > 0 || lineComment):
				// Not a heredoc bash would start: keep reading the next lines
				// as commands (the safe direction) and treat << as input.
				i++
				skipWord = true
			case next == '<':
				// << or <<- here-document: the body starts on the next line.
				i++
				dash := false
				if i+1 < len(rs) && rs[i+1] == '-' {
					dash = true
					i++
				}
				delim, last := readHeredocDelimiter(rs, i+1)
				i = last
				if delim != "" {
					h := &heredoc{delim: delim, dash: dash}
					cur.heredocs = append(cur.heredocs, h)
					pending = append(pending, h)
				}
			default:
				skipWord = true
			}
		default:
			if ch == '#' && !inWord {
				lineComment = true
			}
			writeUnquoted(ch)
		}
	}
	endPipeline()
	return pipelines
}

// SimpleCommand is one command the shell would start: its words exactly as
// written (quotes removed, nothing like sudo or VAR=value stripped) and the
// files its output is redirected to. Quoted[i] reports whether Words[i] was
// written entirely inside quotes ("a;b", 'x && y'), so an operator character
// in it is an argument for a shell that honours those quotes. Here-document
// and here-string bodies are stdin data and appear in neither list.
type SimpleCommand struct {
	Words     []string
	Quoted    []bool
	Redirects []string
}

// SimpleCommands lists every simple command in a command line, in order, with
// the same tokenizer CatastrophicCommand uses. Pipelines, && ; & and newlines
// are flattened, and the bodies of $(...), backticks and subshells come out as
// commands of their own, so a caller that vets each entry also vets what a
// substitution would run.
func SimpleCommands(command string) []SimpleCommand {
	var out []SimpleCommand
	for _, pipeline := range parseShell(command) {
		for _, c := range pipeline {
			if len(c.words) == 0 && len(c.redirects) == 0 {
				continue // only here-document data
			}
			out = append(out, SimpleCommand{Words: c.words, Quoted: c.quoted, Redirects: c.redirects})
		}
	}
	return out
}

// isDescriptorWord reports whether w, the word after >&, names a descriptor
// to copy or close: all digits, "-", or digits followed by "-".
func isDescriptorWord(w string) bool {
	if w == "-" {
		return true
	}
	return isDigits(strings.TrimSuffix(w, "-"))
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// commandWords drops what runs in front of the real command — sudo, env,
// VAR=value assignments — and returns the lowercased command name and its args.
func commandWords(words []string) (string, []string) {
	words = StripCommandRunners(words)
	if len(words) == 0 {
		return "", nil
	}
	return ProgramName(words[0]), words[1:]
}

// ProgramName normalizes an executable word the way the shell resolves it:
// lowercased (Windows is case-insensitive), directory and .exe dropped, so
// "/usr/bin/git", `C:\Git\git.exe` and "GIT" all name "git".
func ProgramName(exe string) string {
	name := strings.ToLower(exe)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSuffix(name, ".exe")
}

// runnerArgOptions lists, per command runner, the options that take their
// value as a separate word, so StripCommandRunners can step over it.
var runnerArgOptions = map[string]map[string]bool{
	"sudo":    {"-u": true, "-g": true, "-C": true, "-h": true, "-p": true, "-r": true, "-t": true, "-U": true, "-D": true},
	"doas":    {"-u": true, "-C": true},
	"env":     {"-u": true, "-C": true, "-S": true},
	"time":    {"-f": true, "-o": true},
	"nice":    {"-n": true},
	"exec":    {"-a": true},
	"timeout": {"-s": true, "-k": true},
	"nohup":   {},
	"command": {},
	"busybox": {},
}

// StripCommandRunners drops what runs in front of the real command — sudo,
// doas, env, nohup, time, command, nice, exec, busybox, timeout (with their
// own options, and timeout's duration) and VAR=value assignments — and
// returns the remaining words, the program as written first. Runner names are
// compared with ProgramName, so "/usr/bin/sudo" counts too.
func StripCommandRunners(words []string) []string {
	for len(words) > 0 {
		w := words[0]
		name := ProgramName(w)
		if argOpts, ok := runnerArgOptions[name]; ok {
			words = words[1:]
			for len(words) > 0 && strings.HasPrefix(words[0], "-") && words[0] != "-" {
				opt := words[0]
				words = words[1:]
				if opt == "--" {
					break
				}
				if argOpts[opt] && len(words) > 0 {
					words = words[1:]
				}
			}
			if name == "timeout" && len(words) > 0 {
				words = words[1:] // the duration
			}
			continue
		}
		if strings.Contains(w, "=") && !strings.HasPrefix(w, "=") && !strings.HasPrefix(w, "-") {
			words = words[1:]
			continue
		}
		return words
	}
	return nil
}

var (
	driveRoot  = regexp.MustCompile(`^[a-z]:[\\/]*\*?$`)
	systemDirs = map[string]bool{
		"/bin": true, "/boot": true, "/dev": true, "/etc": true, "/home": true,
		"/lib": true, "/lib64": true, "/opt": true, "/proc": true, "/root": true,
		"/sbin": true, "/sys": true, "/usr": true, "/var": true,
		"/system": true, "/users": true, "/library": true, "/applications": true,
		`c:\windows`: true, `c:\program files`: true, `c:\program files (x86)`: true,
		`c:\users`: true, `c:\programdata`: true,
	}
	homeRefs = map[string]bool{
		"~": true, "$home": true, "${home}": true, "%userprofile%": true,
		"$env:userprofile": true, "%systemroot%": true, "$env:systemroot": true,
		"%windir%": true, "$env:windir": true,
	}
)

// criticalPath reports whether deleting (or chmod-ing) target recursively
// would destroy the system or the user's home rather than a project directory.
func criticalPath(target string) bool {
	// PowerShell treats typographic quotes (U+2018–U+201F) as quotes; the
	// tokenizer keeps them, so strip them before judging the target.
	t := strings.ToLower(strings.TrimSpace(strings.Trim(target, "‘’‚‛“”„‟")))
	if t == "" || t == "*" || t == "." || t == "./" {
		// The working directory: destructive, but project-scoped (a warning).
		return false
	}
	t = strings.TrimSuffix(t, "*")
	if t == "/" || t == "" {
		return true
	}
	if driveRoot.MatchString(t) {
		return true
	}
	trimmed := strings.TrimRight(t, `/\`)
	if trimmed == "" {
		return true
	}
	return homeRefs[trimmed] || systemDirs[trimmed]
}

func catastrophicSimple(c simpleCmd) (string, bool) {
	for _, r := range c.redirects {
		if rawDisk(r) {
			return "write to raw disk " + r, true
		}
	}
	name, args := commandWords(c.words)
	switch name {
	case "rm", "rmdir", "remove-item", "ri", "del", "erase", "rd":
		recursive := false
		for _, a := range args {
			la := strings.ToLower(a)
			if la == "--no-preserve-root" {
				return "rm --no-preserve-root", true
			}
			// PowerShell's -Name:value switch form (-Recurse:$true) names the
			// same switch; judged by name alone, so -Recurse:$false is treated
			// as recursive too (stricter).
			if strings.HasPrefix(la, "-") && !strings.HasPrefix(la, "--") {
				if i := strings.IndexByte(la, ':'); i > 0 {
					la = la[:i]
				}
			}
			recursive = recursive || recursiveFlag(la)
		}
		if !recursive {
			return "", false
		}
		for _, a := range args {
			if strings.HasPrefix(a, "-") || cmdSwitch.MatchString(a) {
				continue
			}
			if criticalPath(a) {
				return "recursive delete of " + a, true
			}
		}
	case "chmod", "chown", "chgrp":
		for _, a := range args {
			if !strings.HasPrefix(a, "-") && criticalPath(a) {
				return name + " on " + a, true
			}
		}
	case "mkfs", "mke2fs", "fdisk", "sfdisk", "parted", "wipefs", "format-volume", "clear-disk", "initialize-disk", "diskpart":
		return name + " rewrites a disk", true
	case "format":
		for _, a := range args {
			if driveArg.MatchString(strings.ToLower(a)) {
				return "format " + a, true
			}
		}
	case "dd":
		for _, a := range args {
			if strings.HasPrefix(a, "of=") && rawDisk(strings.TrimPrefix(a, "of=")) {
				return "dd to raw disk " + a, true
			}
		}
	case "shutdown", "reboot", "halt", "poweroff", "stop-computer", "restart-computer":
		return name + " takes the machine down", true
	case "find":
		return catastrophicFind(args)
	}
	if strings.HasPrefix(name, "mkfs.") {
		return name + " rewrites a disk", true
	}
	return "", false
}

// recursiveFlag recognizes -r/-R/--recursive, combined short flags (-rf, -fR),
// PowerShell's -Recurse and any prefix of it, and cmd's /s.
func recursiveFlag(la string) bool {
	switch {
	case la == "--recursive" || la == "/s":
		return true
	case strings.HasPrefix(la, "--"):
		return false
	case len(la) >= 2 && strings.HasPrefix("-recurse", la):
		return true
	case shortFlags.MatchString(la):
		return strings.Contains(la, "r")
	}
	return false
}

var (
	shortFlags = regexp.MustCompile(`^-[rfivd]{1,4}$`)
	cmdSwitch  = regexp.MustCompile(`^/[a-z]$`)
	driveArg   = regexp.MustCompile(`^[a-z]:$`)
)

func rawDisk(path string) bool {
	p := strings.ToLower(path)
	for _, prefix := range []string{"/dev/sd", "/dev/hd", "/dev/nvme", "/dev/vd", "/dev/xvd", "/dev/disk", "/dev/mmcblk", `\\.\physicaldrive`} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

var (
	remoteSources = map[string]bool{
		"curl": true, "wget": true, "iwr": true, "irm": true,
		"invoke-webrequest": true, "invoke-restmethod": true,
		"base64": true, "xxd": true,
	}
	interpreters = map[string]bool{
		"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
		"python": true, "python3": true, "perl": true, "ruby": true, "node": true, "php": true,
		"pwsh": true, "powershell": true, "iex": true, "invoke-expression": true,
	}
)

// catastrophicPipeline catches downloaded or decoded text being run as code:
// curl ... | sh, irm ... | iex, base64 -d | bash. An interpreter that is given
// a script or a module (python3 -m json.tool, python3 parse.py) is only
// reading data and is left alone.
func catastrophicPipeline(p []simpleCmd) (string, bool) {
	source := ""
	for _, c := range p {
		name, args := commandWords(c.words)
		if source != "" && interpreters[name] && readsCodeFromStdin(args) {
			return source + " piped into " + name, true
		}
		if remoteSources[name] {
			source = name
		}
	}
	return "", false
}

func readsCodeFromStdin(args []string) bool {
	for _, a := range args {
		la := strings.ToLower(a)
		if !strings.HasPrefix(la, "-") || la == "-c" || la == "-m" || la == "-e" ||
			la == "-command" || la == "-file" {
			return false
		}
	}
	return true
}
