package safety

import "strings"

// NestedCommands lists the simple commands a line runs indirectly, for deny
// and ask rules: the inline command of sh/bash/zsh -c, cmd /c and
// pwsh/powershell (-Command or the first positional argument), the text eval
// and iex/Invoke-Expression run, the command xargs and find -exec/-execdir/
// -ok/-okdir start, a here-document or here-string fed or piped into a shell,
// and the shell command of a git "-c alias.X=!cmd". Nested commands are
// listed recursively, outer first, each once, under both the literal and the
// bash reading (see readings). The line's own top-level commands are not
// included; SimpleCommands lists those.
//
// Deny and ask rules used to look at the top-level commands only, so with a
// deny on "git push", "bash -c 'git push origin main'", "eval ...", "xargs
// git push" and "find . -exec git push \;" all ran.
func NestedCommands(command string) []SimpleCommand {
	var out []SimpleCommand
	seen := map[string]bool{}
	emit := func(sc SimpleCommand) bool {
		key := strings.Join(sc.Words, "\x00")
		if seen[key] {
			return false
		}
		seen[key] = true
		out = append(out, sc)
		return true
	}
	var fromLine func(s string, depth int, top bool)
	var fromCmd func(c simpleCmd, rest []simpleCmd, depth int)
	fromLine = func(s string, depth int, top bool) {
		if depth > maxShellNesting {
			return
		}
		for _, pipeline := range readings(s) {
			for i, c := range pipeline {
				if !top {
					if len(c.words) == 0 {
						continue
					}
					if !emit(SimpleCommand{Words: c.words, Quoted: c.quoted, Redirects: c.redirects}) {
						continue
					}
				}
				fromCmd(c, pipeline[i+1:], depth)
			}
		}
	}
	fromWords := func(words []string, depth int) {
		if len(words) == 0 || depth > maxShellNesting {
			return
		}
		if emit(SimpleCommand{Words: words, Quoted: make([]bool, len(words))}) {
			fromCmd(simpleCmd{words: words}, nil, depth)
		}
	}
	fromCmd = func(c simpleCmd, rest []simpleCmd, depth int) {
		if len(c.heredocs) > 0 && (stdinShell(c) || pipesIntoShell(rest)) {
			for _, h := range c.heredocs {
				fromLine(h.body, depth+1, false)
			}
		}
		if inner, encoded, ok := unwrapShell(c.words); ok && !encoded {
			fromLine(inner, depth+1, false)
		}
		if inner, ok := evalText(c.words); ok {
			fromLine(inner, depth+1, false)
		}
		name, args := commandWords(c.words)
		switch name {
		case "xargs":
			fromWords(xargsCommand(args), depth+1)
		case "find":
			for _, words := range findExecCommands(args) {
				fromWords(words, depth+1)
			}
		case "git":
			for _, cmd := range GitShellAliases(args) {
				fromLine(cmd, depth+1, false)
			}
		}
	}
	fromLine(command, 0, true)
	return dropPrefixCommands(out)
}

// evalText returns the command line eval or iex/Invoke-Expression runs: its
// arguments joined by blanks, as eval joins them. Deny rules (NestedCommands)
// and the hard block (CatastrophicCommand) share it; only the deny side used
// to unwrap eval, so eval 'rm -rf /' passed the hard block.
func evalText(words []string) (string, bool) {
	switch name, args := commandWords(words); name {
	case "eval", "iex", "invoke-expression":
		if len(args) > 0 {
			return strings.Join(args, " "), true
		}
	}
	return "", false
}

// dropPrefixCommands drops a command whose words are a strict prefix of
// another listed command: the literal reading splits at a glued brace
// ("git push {}" becomes "git push"), and a rule matching the shorter one
// matches the longer one as well.
func dropPrefixCommands(cmds []SimpleCommand) []SimpleCommand {
	var out []SimpleCommand
	for i, c := range cmds {
		shadowed := false
		for j, d := range cmds {
			if i != j && len(d.Words) > len(c.Words) && wordsHavePrefix(d.Words, c.Words) {
				shadowed = true
				break
			}
		}
		if !shadowed {
			out = append(out, c)
		}
	}
	return out
}

func wordsHavePrefix(words, prefix []string) bool {
	if len(words) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if words[i] != p {
			return false
		}
	}
	return true
}

// xargsCommand returns the command xargs runs: its arguments after its own
// options.
func xargsCommand(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if args[0] == "--" {
			return args[1:]
		}
		if xargsValueFlags[args[0]] && len(args) > 1 {
			args = args[1:]
		}
		args = args[1:]
	}
	return args
}

// findExecCommands returns the commands of find's -exec, -execdir, -ok and
// -okdir actions, without their ";" or "+" terminator ("\;" in the literal
// reading, where the backslash stays a word of its own before the ";").
func findExecCommands(args []string) [][]string {
	var out [][]string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-exec", "-execdir", "-ok", "-okdir":
		default:
			continue
		}
		var words []string
		for i++; i < len(args); i++ {
			a := args[i]
			if a == ";" || a == `\;` || a == `\` || a == "+" && len(words) > 0 && words[len(words)-1] == "{}" {
				break
			}
			words = append(words, a)
		}
		if len(words) > 0 {
			out = append(out, words)
		}
	}
	return out
}

// GitShellAliases returns the shell commands of the aliases a git invocation
// defines on its command line (the arguments after "git"): "-c alias.p=!cmd"
// runs cmd through the shell when "git p" is invoked.
func GitShellAliases(args []string) []string {
	var out []string
	for _, v := range gitConfigValues(args) {
		key, val, ok := strings.Cut(v, "=")
		if ok && strings.HasPrefix(strings.ToLower(key), "alias.") && strings.HasPrefix(val, "!") {
			out = append(out, val[1:])
		}
	}
	return out
}

// GitDefinesAlias reports whether a git invocation (the arguments after
// "git") defines an alias on its command line (-c alias.X=..., also glued
// and through --config-env) or writes one with git config. The alias's name
// then runs a subcommand, or a shell command, a rule on the subcommand
// cannot see.
func GitDefinesAlias(args []string) bool {
	for _, v := range gitConfigValues(args) {
		if strings.HasPrefix(strings.ToLower(v), "alias.") {
			return true
		}
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if a != "config" {
			return false
		}
		break
	}
	for _, a := range args {
		if strings.HasPrefix(strings.ToLower(a), "alias.") {
			return true
		}
	}
	return false
}

// gitConfigValues returns the key=value words of git's -c and --config-env
// global options.
func gitConfigValues(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" || a == "--config-env":
			if i+1 < len(args) {
				out = append(out, args[i+1])
				i++
			}
		case strings.HasPrefix(a, "--config-env="):
			out = append(out, strings.TrimPrefix(a, "--config-env="))
		case strings.HasPrefix(a, "-c") && len(a) > 2:
			out = append(out, a[2:])
		case a == "-C" || a == "--git-dir" || a == "--work-tree" || a == "--namespace":
			i++
		case !strings.HasPrefix(a, "-"):
			return out // the subcommand
		}
	}
	return out
}
