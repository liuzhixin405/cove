package repomap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSyntheticRepo writes n Go files of symsPerFile types and methods each;
// every method's signature mentions a type from the next file, so the
// cross-reference pass has real work to do.
func writeSyntheticRepo(t *testing.T, n, symsPerFile int) string {
	t.Helper()
	root := t.TempDir()
	for i := 0; i < n; i++ {
		var sb strings.Builder
		sb.WriteString("package p\n")
		for j := 0; j < symsPerFile; j++ {
			fmt.Fprintf(&sb, "type T%d_%d struct{}\nfunc (t *T%d_%d) M%d(a T%d_%d, b int) {}\n", i, j, i, j, j, (i+1)%n, j)
		}
		dir := filepath.Join(root, fmt.Sprintf("d%02d", i%40))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d.go", i)), []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestBuildRankedScalesOnMidSizeRepo: the engine regenerates the map on the
// prompt-building path whenever a file changed. The cross-reference score was
// computed symbol x file x symbol with strings.Contains; an 800-file repo with
// 20 types per file took ~29s per regeneration, stalling every turn after an
// edit.
func TestBuildRankedScalesOnMidSizeRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("builds an 800-file repo")
	}
	root := writeSyntheticRepo(t, 800, 20)
	g := NewGenerator(root)
	start := time.Now()
	fms := g.BuildRanked(200)
	elapsed := time.Since(start)
	t.Logf("BuildRanked: %v", elapsed)
	if elapsed > 10*time.Second {
		t.Fatalf("BuildRanked on 800 files took %v", elapsed)
	}
	if len(fms) != 200 {
		t.Fatalf("got %d files, want 200", len(fms))
	}
}

// TestBuildRankedPrefersReferencedFiles pins what the score is for: a file
// whose types are named in other files' signatures outranks one nobody
// mentions.
func TestBuildRankedPrefersReferencedFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"core.go":   "package p\ntype Store struct{}\n",
		"a.go":      "package p\nfunc UseA(s *Store) {}\n",
		"b.go":      "package p\nfunc UseB(s Store) {}\n",
		"lonely.go": "package p\ntype Lonely struct{}\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fms := NewGenerator(root).BuildRanked(1)
	if len(fms) != 1 || fms[0].Path != "core.go" {
		t.Fatalf("top file = %+v, want core.go", fms)
	}
}

// TestBuildRankedIsDeterministicOnTies: files are parsed concurrently, so they
// arrive in a random order, and sort.Slice is not stable. When scores tie at
// the maxFiles cut, a different subset was chosen on each run, so the repo map
// in the system prompt changed without any file changing — which also throws
// away the provider's prompt cache.
func TestBuildRankedIsDeterministicOnTies(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 60; i++ {
		src := fmt.Sprintf("package p\nfunc Solo%d() {}\n", i)
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%02d.go", i)), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := NewGenerator(root)
	want := FormatFileMaps(g.BuildRanked(10))
	for i := 0; i < 20; i++ {
		if got := FormatFileMaps(g.BuildRanked(10)); got != want {
			t.Fatalf("run %d picked different files:\n%s\nvs\n%s", i, got, want)
		}
	}
	if !strings.HasPrefix(want, "f00.go") {
		t.Fatalf("ties should resolve by path, got:\n%s", want)
	}
}

// TestBuildRankedCoversTSXAndModuleJS: React (.tsx/.jsx) and ES-module
// (.mjs/.cjs) sources use the same class/function syntax the .ts/.js scanner
// already handles, but they were never scanned, so a React project got an
// empty repo map.
func TestBuildRankedCoversTSXAndModuleJS(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"App.tsx":    "export function App(props) {\n  return null\n}\n",
		"Btn.jsx":    "export class Button {\n}\n",
		"util.mjs":   "export async function load(url) {\n}\n",
		"legacy.cjs": "function helper(a, b) {\n}\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := NewGenerator(root).Generate(10)
	for _, want := range []string{"function App(props)", "class Button", "function load(url)", "function helper(a, b)"} {
		if !strings.Contains(out, want) {
			t.Errorf("repo map is missing %q:\n%s", want, out)
		}
	}
}

// TestEnhancedGeneratorNoticesModuleJSChanges: the incremental generator only
// rebuilds when a tracked file changes, so a scanned extension it does not
// track (.mjs/.cjs) kept a stale map until some other file changed.
func TestEnhancedGeneratorNoticesModuleJSChanges(t *testing.T) {
	root := t.TempDir()
	eg := NewEnhancedGenerator(root)
	_, _ = eg.GenerateIncremental(50)
	if err := os.WriteFile(filepath.Join(root, "util.mjs"), []byte("export function fresh(a) {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, _ := eg.GenerateIncremental(50); !strings.Contains(out, "function fresh(a)") {
		t.Fatalf("a new .mjs file did not refresh the map:\n%s", out)
	}
}
