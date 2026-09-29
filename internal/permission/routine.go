package permission

import (
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/safety"
)

// Command groups: one "always allow" answer covers a tool's everyday
// subcommands instead of one prefix per subcommand, so the add → commit →
// push cycle, or new → build → test, is one question. Each group names the
// subcommands that are routine and the options that take a command out of
// the group: anything that forces, deletes, publishes, installs system-wide,
// runs another program or opens an editor keeps asking. Read-only commands
// are not in any group; they run unasked.
const (
	// GroupGitRoutine: staging, committing, pushing, pulling and fetching,
	// switching branches, merging and rebasing, stashes and tags. Not in it:
	// forced pushes ("+refspec" included), deleting remote branches or tags,
	// reset, clean, checkout (its path form discards working-tree changes and
	// a branch name cannot be told from a path), restore, stash drop/clear,
	// interactive rebase, a commit or annotated tag without a message.
	GroupGitRoutine = "git"
	// GroupDotnetRoutine: new, sln, add, restore, build, test, run, format,
	// clean, pack. Not in it: publish, tool, nuget, watch, --force.
	GroupDotnetRoutine = "dotnet"
	// GroupNpmRoutine (npm, pnpm, yarn): install, ci, run, test, init,
	// uninstall, build, ls, outdated, audit. Not in it: publish, login,
	// exec/npx, global installs.
	GroupNpmRoutine = "npm"
	// GroupGoRoutine: build, test, vet, run, fmt, mod, get, generate, list,
	// doc. Not in it: install, clean, env -w, tool, work.
	GroupGoRoutine = "go"
	// GroupCargoRoutine: build, test, check, run, fmt, clippy, add, update,
	// doc, bench. Not in it: install, publish, clean, yank.
	GroupCargoRoutine = "cargo"
)

// routineGroup describes one command group.
type routineGroup struct {
	programs    []string        // program names the group applies to
	subcommands map[string]bool // routine subcommands
	// refusedLong lists, per subcommand ("*" = every one), the long options
	// that take a command out of the group; a given option is matched as a
	// prefix of these names (git and .NET accept unambiguous abbreviations).
	refusedLong map[string][]string
	// refusedShort lists the single-letter options that do the same; letters
	// are checked inside grouped flags too ("-fu").
	refusedShort map[string]string
	// split separates the subcommand from the rest; nil means the first
	// word that is not an option.
	split func(args []string) (sub string, rest []string, ok bool)
	// check adds per-group rules on the parsed invocation.
	check func(sub string, positional []string, shortFlags string, hasLong func(string) bool) bool
	label string
}

var routineGroups = map[string]*routineGroup{
	GroupGitRoutine: {
		programs: []string{"git"},
		subcommands: map[string]bool{
			"init": true, "add": true, "commit": true, "push": true, "pull": true, "fetch": true,
			"switch": true, "merge": true, "rebase": true,
			"stash": true, "branch": true, "tag": true, "cherry-pick": true, "mv": true,
		},
		refusedLong: map[string][]string{
			"*":      {"--interactive", "--patch", "--edit", "--force", "--receive-pack", "--upload-pack", "--exec", "--edit-todo"},
			"push":   {"--force-with-lease", "--force-if-includes", "--delete", "--mirror", "--prune"},
			"tag":    {"--delete"},
			"switch": {"--discard-changes"},
		},
		refusedShort: map[string]string{
			"*": "ipe", "push": "d", "branch": "DMC", "switch": "C", "tag": "d", "rebase": "x",
			"commit": "c", // -c <commit> reuses a message and opens the editor
		},
		split: gitSubcommand,
		check: gitRoutineCheck,
		label: "git 常规操作（add/commit/push/pull/switch 等，不含 --force、reset、clean、checkout）",
	},
	GroupDotnetRoutine: {
		programs: []string{"dotnet"},
		subcommands: map[string]bool{
			"new": true, "sln": true, "add": true, "remove": true, "restore": true, "build": true,
			"test": true, "run": true, "format": true, "clean": true, "pack": true, "list": true,
		},
		refusedLong:  map[string][]string{"*": {"--force"}},
		refusedShort: map[string]string{},
		label:        "dotnet 常规操作（new/sln/add/restore/build/test/run 等，不含 publish、tool、--force）",
	},
	GroupNpmRoutine: {
		programs: []string{"npm", "pnpm", "yarn"},
		subcommands: map[string]bool{
			"install": true, "i": true, "ci": true, "run": true, "test": true, "init": true,
			"uninstall": true, "remove": true, "build": true, "ls": true, "list": true, "outdated": true, "audit": true,
		},
		refusedLong:  map[string][]string{"*": {"--force", "--global"}},
		refusedShort: map[string]string{"*": "g"},
		label:        "npm/pnpm/yarn 常规操作（install/run/test/build 等，不含 publish、全局安装、exec）",
	},
	GroupGoRoutine: {
		programs: []string{"go"},
		subcommands: map[string]bool{
			"build": true, "test": true, "vet": true, "run": true, "fmt": true, "mod": true,
			"get": true, "generate": true, "list": true, "doc": true, "version": true,
		},
		refusedLong:  map[string][]string{},
		refusedShort: map[string]string{},
		label:        "go 常规操作（build/test/vet/run/mod/get 等，不含 install、clean、env -w）",
	},
	GroupCargoRoutine: {
		programs: []string{"cargo"},
		subcommands: map[string]bool{
			"build": true, "test": true, "check": true, "run": true, "fmt": true, "clippy": true,
			"add": true, "update": true, "doc": true, "bench": true, "tree": true,
		},
		refusedLong:  map[string][]string{"*": {"--force"}},
		refusedShort: map[string]string{},
		label:        "cargo 常规操作（build/test/check/run/fmt/add 等，不含 install、publish、clean）",
	},
}

// KnownGroup reports whether name is a command group a rule may refer to.
func KnownGroup(name string) bool { _, ok := routineGroups[name]; return ok }

// GroupLabel names a group for the person, e.g. in the prompt's 记住范围.
func GroupLabel(name string) string {
	if g := routineGroups[name]; g != nil {
		return g.label
	}
	return name + " 常规操作"
}

// groupForProgram returns the group whose programs include exe (spelled
// plainly), or "".
func groupForProgram(exe string) string {
	name := programName(exe)
	for id, g := range routineGroups {
		for _, p := range g.programs {
			if p == name {
				return id
			}
		}
	}
	return ""
}

// minLongAbbrev is the shortest long-option spelling that counts as an
// abbreviation of a refused option: "--f" is too short to be anyone's intent,
// "--forc" is --force.
const minLongAbbrev = len("--xx")

// longRefused reports whether the long option name (without "=value") is
// one of, or an abbreviation of, the group's refused options for sub or for
// every subcommand.
func (g *routineGroup) longRefused(sub, name string) bool {
	if len(name) < minLongAbbrev {
		return false
	}
	for _, list := range [][]string{g.refusedLong["*"], g.refusedLong[sub]} {
		for _, r := range list {
			if strings.HasPrefix(r, name) {
				return true
			}
		}
	}
	return false
}

// firstWordSubcommand is the default split: the first word that is not an
// option is the subcommand; a program with options only has none.
func firstWordSubcommand(args []string) (string, []string, bool) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a, append(append([]string(nil), args[:i]...), args[i+1:]...), true
		}
	}
	return "", nil, false
}

// routine reports whether an invocation of one of the group's programs (its
// arguments, without the program) is a routine write.
func (g *routineGroup) routine(args []string) bool {
	split := g.split
	if split == nil {
		split = firstWordSubcommand
	}
	sub, rest, ok := split(args)
	if !ok || !g.subcommands[sub] {
		return false
	}
	refusedShort := g.refusedShort["*"] + g.refusedShort[sub]
	var positional []string
	var shortFlags string
	var longNames []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--":
			positional = append(positional, rest[i+1:]...)
			i = len(rest)
		case strings.HasPrefix(a, "--"):
			name := optionName(a)
			if g.longRefused(sub, name) {
				return false
			}
			longNames = append(longNames, a)
		case strings.HasPrefix(a, "-") && len(a) > 1:
			if strings.ContainsAny(a[1:], refusedShort) {
				return false
			}
			shortFlags += a[1:]
		default:
			positional = append(positional, a)
		}
	}
	hasLong := func(want string) bool {
		for _, a := range longNames {
			n := optionName(a)
			if len(n) >= minLongAbbrev && strings.HasPrefix(want, n) {
				return true
			}
		}
		return false
	}
	if g.check != nil && !g.check(sub, positional, shortFlags, hasLongWithValues(longNames, hasLong)) {
		return false
	}
	return true
}

// hasLongWithValues wraps hasLong so a check can also ask about "--opt=value"
// forms through the same function ("--rebase=interactive").
func hasLongWithValues(longNames []string, hasLong func(string) bool) func(string) bool {
	return func(want string) bool {
		if strings.Contains(want, "=") {
			wantName, wantValue := optionName(want), optionValue(want)
			for _, a := range longNames {
				n := optionName(a)
				if len(n) >= minLongAbbrev && strings.HasPrefix(wantName, n) && optionValue(a) == wantValue {
					return true
				}
			}
			return false
		}
		return hasLong(want)
	}
}

// gitRoutineCheck holds git's rules beyond the option tables.
func gitRoutineCheck(sub string, positional []string, shortFlags string, hasLong func(string) bool) bool {
	if sub != "add" && strings.ContainsRune(shortFlags, 'f') {
		// git add -f only includes ignored files; every other -f forces
		// something over what is there.
		return false
	}
	switch sub {
	case "push":
		for _, p := range positional {
			// "git push origin :branch" deletes the remote branch; a "+"
			// refspec is a forced push.
			if strings.HasPrefix(p, ":") || strings.HasPrefix(p, "+") {
				return false
			}
		}
	case "pull":
		if hasLong("--rebase=interactive") || hasLong("--rebase=i") {
			return false
		}
	case "stash":
		if len(positional) > 0 && (positional[0] == "drop" || positional[0] == "clear") {
			return false
		}
	case "commit":
		// Without a message source git opens the editor, which hangs a
		// non-interactive shell: -m/-F/-C, --message/--file/--reuse-message,
		// --no-edit (amend) or --allow-empty-message.
		if !strings.ContainsAny(shortFlags, "mFC") && !hasLong("--message") && !hasLong("--file") &&
			!hasLong("--reuse-message") && !hasLong("--no-edit") && !hasLong("--allow-empty-message") {
			return false
		}
	case "tag":
		// An annotated or signed tag without a message opens the editor.
		if (strings.ContainsAny(shortFlags, "asu") || hasLong("--annotate") || hasLong("--sign") || hasLong("--local-user")) &&
			!strings.ContainsAny(shortFlags, "mF") && !hasLong("--message") && !hasLong("--file") {
			return false
		}
	}
	return true
}

// optionName is a long option without its "=value" part.
func optionName(a string) string {
	if i := strings.IndexByte(a, '='); i > 0 {
		return a[:i]
	}
	return a
}

// optionValue is the "=value" part of a long option, or "".
func optionValue(a string) string {
	if i := strings.IndexByte(a, '='); i > 0 {
		return a[i+1:]
	}
	return ""
}

// plainProgram reports whether exe is spelled plainly: no path, no
// variable, no runner in front (the allow reading, like an allow prefix).
func plainProgram(exe string) bool {
	return !strings.ContainsAny(exe, ` /\$*?[=`) && !strings.HasPrefix(exe, "-")
}

// groupCovers is the allow reading of a group rule for one simple command
// given as its words as written.
func groupCovers(group string, words []string) bool {
	g := routineGroups[group]
	if g == nil || len(words) == 0 || !plainProgram(words[0]) || groupForProgram(words[0]) != group {
		return false
	}
	return g.routine(words[1:])
}

// routineGroupOf returns the group that covers words as a routine write, or
// "" (the program is in no group, or the invocation is not routine).
func routineGroupOf(words []string) string {
	if len(words) == 0 || !plainProgram(words[0]) {
		return ""
	}
	if id := groupForProgram(words[0]); id != "" && routineGroups[id].routine(words[1:]) {
		return id
	}
	return ""
}

// groupMatchesAny is the deny/ask reading of a group rule: some command of
// the line, seen through runners, paths and letter case, is in the group.
func groupMatchesAny(group, command string) bool {
	g := routineGroups[group]
	if g == nil {
		return false
	}
	for _, c := range safety.SimpleCommands(command) {
		for _, words := range [][]string{c.Words, safety.StripCommandRunners(c.Words)} {
			if len(words) > 0 && groupForProgram(words[0]) == group && g.routine(words[1:]) {
				return true
			}
		}
	}
	return false
}
