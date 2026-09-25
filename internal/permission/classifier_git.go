package permission

import "strings"

// Rating of git invocations for the classifier (see classifyWords).

// gitReadOnly lists subcommands that only read the repository. Options that
// make one of them write are checked separately in classifyGit.
var gitReadOnly = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "ls-files": true, "ls-tree": true,
	"rev-parse": true, "rev-list": true, "describe": true, "blame": true, "grep": true,
	"shortlog": true, "whatchanged": true, "cherry": true, "merge-base": true,
	"for-each-ref": true, "cat-file": true, "count-objects": true, "range-diff": true,
	"show-ref": true, "name-rev": true, "check-ignore": true, "check-attr": true,
	"version": true, "show-branch": true,
}

// classifyGit rates a git invocation by its subcommand. Only -C <dir>,
// --no-pager, -P and --no-optional-locks may come before it; -c (and any
// other global option) can set config such as core.fsmonitor or core.pager
// that runs arbitrary programs, so it is CatUnknown.
func (c *Classifier) classifyGit(args []string) CmdCategory {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "-C":
			if len(args) < 2 {
				return CatUnknown
			}
			args = args[2:]
		case "--no-pager", "-P", "--no-optional-locks":
			args = args[1:]
		default:
			return CatUnknown
		}
	}
	if len(args) == 0 {
		return CatUnknown
	}
	sub, rest := args[0], args[1:]
	for _, a := range rest {
		if a == "--output" || strings.HasPrefix(a, "--output=") {
			return CatUnknown
		}
	}
	switch sub {
	case "grep":
		return gitGrepCategory(rest)
	case "branch":
		return gitBranchCategory(rest)
	case "tag":
		if len(rest) == 0 || hasAny(rest, "-l", "--list") &&
			!hasAny(rest, "-d", "--delete", "-a", "--annotate", "-s", "--sign", "-f", "--force", "-m", "-F", "-u") {
			return CatSafe
		}
		return CatGit
	case "remote":
		if len(rest) == 0 || onlyFlagsFrom(rest, "-v", "--verbose") {
			return CatSafe
		}
		if rest[0] == "get-url" {
			return CatSafe
		}
		return CatGit
	case "stash":
		if len(rest) > 0 && (rest[0] == "list" || rest[0] == "show") {
			return CatSafe
		}
		return CatGit
	case "config":
		if hasAny(rest, "--get", "--get-all", "--get-regexp", "--list", "-l") &&
			!hasAny(rest, "--unset", "--unset-all", "--add", "--replace-all", "--rename-section", "--remove-section", "-e", "--edit") {
			return CatSafe
		}
		return CatGit
	case "worktree":
		if len(rest) > 0 && rest[0] == "list" {
			return CatSafe
		}
		return CatGit
	case "submodule":
		if len(rest) > 0 && rest[0] == "status" {
			return CatSafe
		}
		return CatGit
	case "reflog":
		if len(rest) == 0 || rest[0] == "show" || strings.HasPrefix(rest[0], "-") {
			return CatSafe
		}
		return CatGit
	case "notes":
		if len(rest) > 0 && (rest[0] == "show" || rest[0] == "list") {
			return CatSafe
		}
		return CatGit
	}
	if gitReadOnly[sub] {
		return CatSafe
	}
	return CatGit
}

// gitGrepShortFlags are git grep's single-letter options that only change
// what is printed. O (--open-files-in-pager, which runs a program) is not
// among them, so a combined group such as -nO is refused.
const gitGrepShortFlags = "nilLcwEFPGhHvazqoIW"

// gitGrepLongFlags are the long options git grep may take and stay a read.
// Anything else — --open-files-in-pager and every abbreviation of it
// (--open, --open-files), --no-index, unknown or abbreviated options — is
// CatUnknown.
var gitGrepLongFlags = map[string]bool{
	"--line-number": true, "--ignore-case": true, "--name-only": true, "--files-with-matches": true,
	"--files-without-match": true, "--count": true, "--word-regexp": true, "--extended-regexp": true,
	"--fixed-strings": true, "--perl-regexp": true, "--basic-regexp": true, "--invert-match": true,
	"--text": true, "--null": true, "--quiet": true, "--only-matching": true, "--cached": true,
	"--untracked": true, "--heading": true, "--break": true, "--show-function": true,
	"--function-context": true, "--column": true, "--full-name": true, "--recurse-submodules": true,
	"--and": true, "--or": true, "--not": true, "--all-match": true, "--color": true, "--no-color": true,
	"--max-depth": true, "--context": true, "--after-context": true, "--before-context": true,
	"--max-count": true, "--threads": true,
}

// gitGrepCategory allows git grep only with whitelisted options.
func gitGrepCategory(args []string) CmdCategory {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return CatSafe // the rest are paths
		case strings.HasPrefix(a, "--"):
			name := a
			if j := strings.IndexByte(name, '='); j > 0 {
				name = name[:j]
			}
			if !gitGrepLongFlags[name] {
				return CatUnknown
			}
		case a == "-e" || a == "-f" || a == "-A" || a == "-B" || a == "-C" || a == "-m":
			i++ // takes a value
		case strings.HasPrefix(a, "-") && len(a) > 1:
			group := a[1:]
			if strings.ContainsAny(group[:1], "ABCm") {
				if strings.Trim(group[1:], "0123456789") != "" {
					return CatUnknown
				}
				continue
			}
			if strings.Trim(group, "0123456789") == "" {
				continue // -3 is --context=3
			}
			for _, r := range group {
				if !strings.ContainsRune(gitGrepShortFlags, r) {
					return CatUnknown
				}
			}
		}
	}
	return CatSafe
}

// gitBranchCategory: listing branches is read-only; naming a branch without
// --list, or any create/delete/move/upstream option, changes the repository.
func gitBranchCategory(args []string) CmdCategory {
	listing := hasAny(args, "--list", "-l")
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			if !listing {
				return CatGit
			}
			continue
		}
		name := a
		if i := strings.IndexByte(name, '='); i > 0 {
			name = name[:i]
		}
		switch name {
		case "-d", "-D", "-m", "-M", "-c", "-C", "-f", "-u", "-t",
			"--delete", "--move", "--copy", "--force", "--set-upstream-to", "--set-upstream",
			"--unset-upstream", "--edit-description", "--track", "--no-track":
			return CatGit
		}
	}
	return CatSafe
}
