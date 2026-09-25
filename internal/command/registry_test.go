package command

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// allCommandConstructors mirrors the production wiring in cli/cove/registry.go.
// It is keyed by constructor name so TestRegistryCoversEveryConstructor can
// prove, by parsing this package's own sources, that no command escapes the
// table-driven tests below. Adding a New*Cmd constructor without adding it here
// fails that test.
var allCommandConstructors = map[string]func() Command{
	"NewCommitCmd":      NewCommitCmd,
	"NewReviewCmd":      NewReviewCmd,
	"NewDiffCmd":        NewDiffCmd,
	"NewDoctorCmd":      NewDoctorCmd,
	"NewConfigCmd":      NewConfigCmd,
	"NewDiagnoseCmd":    NewDiagnoseCmd,
	"NewCompactCmd":     NewCompactCmd,
	"NewCostCmd":        NewCostCmd,
	"NewRateLimitCmd":   NewRateLimitCmd,
	"NewUndoCmd":        NewUndoCmd,
	"NewCheckpointsCmd": NewCheckpointsCmd,
	"NewMemoryCmd":      NewMemoryCmd,
	"NewResumeCmd":      NewResumeCmd,
	"NewHistoryCmd":     NewHistoryCmd,
	"NewExportCmd":      NewExportCmd,
	"NewSystemCmd":      NewSystemCmd,
	"NewStatusCmd":      NewStatusCmd,
	"NewStatsCmd":       NewStatsCmd,
	"NewCdCmd":          NewCdCmd,
	"NewContextCmd":     NewContextCmd,
	"NewPermissionsCmd": NewPermissionsCmd,
	"NewInitCmd":        NewInitCmd,
	"NewDreamCmd":       NewDreamCmd,
	"NewSkillsCmd":      NewSkillsCmd,
	"NewPluginCmd":      NewPluginCmd,
	"NewMcpCmd":         NewMcpCmd,
	"NewHooksCmd":       NewHooksCmd,
}

// testRegistry builds a registry holding every command, in a stable order.
func testRegistry(t *testing.T) *Registry {
	t.Helper()
	names := make([]string, 0, len(allCommandConstructors))
	for name := range allCommandConstructors {
		names = append(names, name)
	}
	sort.Strings(names)

	r := NewRegistry()
	for _, name := range names {
		r.Register(allCommandConstructors[name]())
	}
	return r
}

// declaredConstructors parses this package's sources and returns every
// exported `func New...Cmd() Command` it declares.
//
// Files are walked with parser.ParseFile rather than the deprecated
// parser.ParseDir, skipping _test.go sources by name.
func declaredConstructors(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	var found []string
	for _, entry := range entries {
		fileName := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(fileName, ".go") || strings.HasSuffix(fileName, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, fileName, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", fileName, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name == nil {
				continue
			}
			name := fn.Name.Name
			if !strings.HasPrefix(name, "New") || !strings.HasSuffix(name, "Cmd") {
				continue
			}
			if fn.Type.Params != nil && len(fn.Type.Params.List) != 0 {
				continue // a constructor taking collaborators is wired elsewhere
			}
			found = append(found, name)
		}
	}
	sort.Strings(found)
	return found
}

// TestRegistryCoversEveryConstructor is the tripwire that keeps every other
// test in this file honest: a newly added slash command must be listed in
// allCommandConstructors, otherwise the robustness table silently stops
// covering it.
func TestRegistryCoversEveryConstructor(t *testing.T) {
	declared := declaredConstructors(t)
	if len(declared) == 0 {
		t.Fatal("found no New*Cmd constructors: the source scan is broken, so it can no longer detect missing coverage")
	}
	for _, name := range declared {
		if _, ok := allCommandConstructors[name]; !ok {
			t.Errorf("%s() is declared in this package but missing from allCommandConstructors; add it so the command-wide tests cover it", name)
		}
	}
	declaredSet := map[string]bool{}
	for _, name := range declared {
		declaredSet[name] = true
	}
	for name := range allCommandConstructors {
		if !declaredSet[name] {
			t.Errorf("allCommandConstructors lists %s(), which this package no longer declares", name)
		}
	}
}

// TestRegistryFindResolvesNamesAndAliases covers the lookup the REPL performs
// on every "/x" line.
func TestRegistryFindResolvesNamesAndAliases(t *testing.T) {
	r := testRegistry(t)

	for _, c := range r.All() {
		got, ok := r.Find(c.Name())
		if !ok {
			t.Errorf("Find(%q) reported unknown for a registered command", c.Name())
			continue
		}
		if got != c {
			t.Errorf("Find(%q) returned %T, want %T", c.Name(), got, c)
		}
		for _, alias := range c.Aliases() {
			got, ok := r.Find(alias)
			if !ok {
				t.Errorf("Find(%q) reported unknown for an alias of /%s", alias, c.Name())
				continue
			}
			if got != c {
				t.Errorf("alias %q resolved to %T, want %T (/%s)", alias, got, c, c.Name())
			}
		}
	}

	// Aliases that exist today, spelled out so a silent removal is caught.
	for alias, wantName := range map[string]string{"skill": "skills", "diag": "diagnose"} {
		c, ok := r.Find(alias)
		if !ok {
			t.Errorf("alias %q is no longer registered", alias)
			continue
		}
		if c.Name() != wantName {
			t.Errorf("alias %q resolves to /%s, want /%s", alias, c.Name(), wantName)
		}
	}

	for _, unknown := range []string{"", "nope", "stat", "commit ", "/commit"} {
		if c, ok := r.Find(unknown); ok {
			t.Errorf("Find(%q) matched /%s, want known=false", unknown, c.Name())
		}
	}

	// Lookup is exact: callers hand Find the name the user typed verbatim
	// (strings.TrimPrefix(parts[0], "/") in cli/cove), so "/Status" is unknown
	// rather than being silently normalized.
	if _, ok := r.Find("Status"); ok {
		t.Error("Find is case-insensitive; callers pass the raw typed name and rely on exact matching")
	}
}

// TestRegistryNamesAndAliasesAreUnique is a registry-wide invariant: Register
// writes names and aliases into one map, so a collision silently shadows a
// command and makes it unreachable.
func TestRegistryNamesAndAliasesAreUnique(t *testing.T) {
	r := testRegistry(t)
	owner := map[string]string{}
	for _, c := range r.All() {
		keys := append([]string{c.Name()}, c.Aliases()...)
		for _, k := range keys {
			if k == "" {
				t.Errorf("/%s registers an empty name or alias", c.Name())
				continue
			}
			if prev, dup := owner[k]; dup {
				t.Errorf("%q is claimed by both /%s and /%s; one of them is unreachable", k, prev, c.Name())
				continue
			}
			owner[k] = c.Name()
		}
	}
}

// TestRegistryAllIsDeduplicatedAndOrdered: All() drives /help, so an aliased
// command must appear exactly once, in registration order.
func TestRegistryAllIsDeduplicatedAndOrdered(t *testing.T) {
	r := NewRegistry()
	r.Register(NewSkillsCmd()) // has the "skill" alias
	r.Register(NewMcpCmd())
	r.Register(NewDiagnoseCmd()) // has the "diag" alias

	got := []string{}
	for _, c := range r.All() {
		got = append(got, c.Name())
	}
	want := []string{"skills", "mcp", "diagnose"}
	if len(got) != len(want) {
		t.Fatalf("All() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("All() = %v, want %v", got, want)
		}
	}

	full := testRegistry(t)
	if len(full.All()) != len(allCommandConstructors) {
		t.Errorf("All() returned %d commands, want %d registered", len(full.All()), len(allCommandConstructors))
	}
}

// TestCommandMetadataIsPresent: /help renders Name/Description/Help for every
// command, and an empty one leaves a blank row. Help text should also describe
// the command it belongs to.
func TestCommandMetadataIsPresent(t *testing.T) {
	for _, c := range testRegistry(t).All() {
		if strings.TrimSpace(c.Name()) == "" {
			t.Errorf("%T has an empty Name()", c)
		}
		if strings.ContainsAny(c.Name(), " /") {
			t.Errorf("%T Name() = %q; a name with a space or slash can never be matched by Find", c, c.Name())
		}
		if strings.TrimSpace(c.Description()) == "" {
			t.Errorf("/%s has an empty Description(); /help would show a blank row", c.Name())
		}
		help := c.Help()
		if strings.TrimSpace(help) == "" {
			t.Errorf("/%s has an empty Help()", c.Name())
			continue
		}
		if !strings.Contains(help, "/"+c.Name()) {
			t.Errorf("/%s Help() = %q, want it to mention /%s", c.Name(), help, c.Name())
		}
	}
}
