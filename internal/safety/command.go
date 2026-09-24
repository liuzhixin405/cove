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
	command = ifsPattern.ReplaceAllString(command, " ")
	if name, ok := forkBomb(command); ok {
		return "fork bomb " + name + "()", true
	}
	for _, pipeline := range parseShell(command) {
		if why, ok := catastrophicPipeline(pipeline); ok {
			return why, true
		}
		for _, c := range pipeline {
			if why, ok := catastrophicSimple(c); ok {
				return why, true
			}
		}
	}
	return "", false
}

var ifsPattern = regexp.MustCompile(`\$\{?IFS\}?`)

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
	redirects []string
}

// parseShell splits a command line into pipelines of simple commands. It knows
// quotes, ; && || & | and newlines, redirects, subshells and command
// substitution — enough to find each command's name and arguments. Backslash
// is kept literally: the same string may be a PowerShell or cmd command, where
// it is a path separator.
func parseShell(s string) [][]simpleCmd {
	var (
		pipelines [][]simpleCmd
		pipeline  []simpleCmd
		cur       simpleCmd
		word      strings.Builder
		inWord    bool
		quote     rune
		redirect  bool // the next word is an output redirect target
		skipWord  bool // the next word is an input redirect source
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
		case skipWord:
			skipWord = false
		default:
			cur.words = append(cur.words, w)
		}
		word.Reset()
		inWord = false
	}
	endCmd := func() {
		endWord()
		if len(cur.words) > 0 || len(cur.redirects) > 0 {
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
		case ch == ' ' || ch == '\t' || ch == '\r':
			endWord()
		case ch == '\n' || ch == ';' || ch == '(' || ch == ')' || ch == '`':
			endPipeline()
		case ch == '{' || ch == '}':
			if ch == '{' && inWord && strings.HasSuffix(word.String(), "$") {
				// ${VAR}: keep the whole expansion in the word.
				for ; i < len(rs) && rs[i] != '}'; i++ {
					word.WriteRune(rs[i])
				}
				word.WriteRune('}')
				continue
			}
			endPipeline()
		case ch == '$' && next == '(':
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
			if next == '>' {
				i++
			}
			if i+1 < len(rs) && rs[i+1] == '&' {
				// 2>&1 duplicates a descriptor; there is no target file.
				i++
				for i+1 < len(rs) && rs[i+1] >= '0' && rs[i+1] <= '9' {
					i++
				}
				continue
			}
			redirect = true
		case ch == '<':
			endWord()
			skipWord = true
		default:
			word.WriteRune(ch)
			inWord = true
		}
	}
	endPipeline()
	return pipelines
}

// SimpleCommand is one command the shell would start: its words exactly as
// written (quotes removed, nothing like sudo or VAR=value stripped) and the
// files its output is redirected to.
type SimpleCommand struct {
	Words     []string
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
			out = append(out, SimpleCommand{Words: c.words, Redirects: c.redirects})
		}
	}
	return out
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
	for len(words) > 0 {
		w := words[0]
		lw := strings.ToLower(w)
		switch {
		case lw == "sudo" || lw == "doas":
			words = words[1:]
			// sudo's own flags (-u root, -E ...)
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				if words[0] == "-u" || words[0] == "-g" {
					words = words[1:]
				}
				if len(words) > 0 {
					words = words[1:]
				}
			}
		case lw == "env" || lw == "nohup" || lw == "time" || lw == "command" || lw == "nice" || lw == "exec":
			words = words[1:]
		case strings.Contains(w, "=") && !strings.HasPrefix(w, "=") && !strings.HasPrefix(w, "-"):
			words = words[1:]
		default:
			name := strings.ToLower(w)
			if i := strings.LastIndexAny(name, `/\`); i >= 0 {
				name = name[i+1:]
			}
			name = strings.TrimSuffix(name, ".exe")
			return name, words[1:]
		}
	}
	return "", nil
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
	t := strings.ToLower(strings.TrimSpace(target))
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
