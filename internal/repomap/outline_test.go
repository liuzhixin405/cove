package repomap

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func syntheticRepo(t *testing.T) string {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod":                       "module example.com/x\n\ngo 1.25\n",
		"Makefile":                     "build:\n\tgo build ./...\ntest:\n\tgo test ./...\nlint:\n\tgolangci-lint run\n",
		"README.md":                    "# x\n",
		"cmd/app/main.go":              "package main\n\nfunc main() {}\n",
		"internal/engine/engine.go":    "package engine\n\ntype Engine struct{}\n\nfunc (e *Engine) Run(msg string) {}\n\nfunc New() *Engine { return nil }\n",
		"internal/engine/turn.go":      "package engine\n\nfunc turnContextNote(q string) string { return q }\n",
		"internal/engine/turn_test.go": "package engine\n\nimport \"testing\"\n\nfunc TestTurn(t *testing.T) {}\n",
		"internal/store/store.go":      "package store\n\ntype Store struct{}\n\nfunc (s *Store) Save(key string) {}\n",
		"web/index.ts":                 "export function render(x) {}\n",
		".hidden/secret.go":            "package hidden\n\nfunc Hidden() {}\n",
		"node_modules/dep/index.js":    "function dep() {}\n",
	})
	return root
}

func TestOutlineSummarisesProject(t *testing.T) {
	root := syntheticRepo(t)
	out := Outline(root)
	if !strings.HasPrefix(out, "<project_outline>\n") || !strings.HasSuffix(out, "</project_outline>\n") {
		t.Fatalf("outline not wrapped:\n%s", out)
	}
	for _, want := range []string{"Go", "cmd/", "internal/", "internal/engine", "cmd/app/main.go", "go build ./...", "go test ./...", "make", "repo_map"} {
		if !strings.Contains(out, want) {
			t.Errorf("outline lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{".hidden", "node_modules", "Engine struct", "turnContextNote"} {
		if strings.Contains(out, bad) {
			t.Errorf("outline contains %q (symbols and ignored dirs belong to repo_map queries):\n%s", bad, out)
		}
	}
	if len(out) > OutlineMaxBytes {
		t.Fatalf("outline is %d bytes, cap %d", len(out), OutlineMaxBytes)
	}
}

func TestOutlineIsStable(t *testing.T) {
	root := syntheticRepo(t)
	a, b := Outline(root), Outline(root)
	if a != b {
		t.Fatalf("outline changed between calls:\n%s\n---\n%s", a, b)
	}
}

func TestOutlineCapsLargeTrees(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{"go.mod": "module m\n"}
	// Few files, long names: the outline must still be capped.
	long := strings.Repeat("a_rather_long_directory_name_", 3)
	for i := 0; i < 80; i++ {
		files["pkg"+itoa(i%5)+"/"+long+itoa(i)+"/deeper/f.go"] = "package p\n"
		files[long+"top"+itoa(i)+"/main.go"] = "package main\n\nfunc main() {}\n"
	}
	writeTree(t, root, files)
	out := Outline(root)
	if len(out) > OutlineMaxBytes {
		t.Fatalf("outline is %d bytes, cap %d", len(out), OutlineMaxBytes)
	}
	if !strings.HasSuffix(out, "</project_outline>\n") {
		t.Fatalf("capped outline lost its closing tag:\n%s", out[len(out)-200:])
	}
}

func TestOutlineOfMissingDirIsEmpty(t *testing.T) {
	if out := Outline(filepath.Join(t.TempDir(), "nope")); out != "" {
		t.Fatalf("outline of a missing dir = %q, want empty", out)
	}
}

// The outline of this repository stays inside its cap (the snapshot the
// prompt-size test in internal/engine measures).
func TestOutlineOfThisRepo(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	out := Outline(root)
	if out == "" || len(out) > OutlineMaxBytes {
		t.Fatalf("outline of this repo is %d bytes (cap %d)", len(out), OutlineMaxBytes)
	}
	if !strings.Contains(out, "internal/engine") {
		t.Fatalf("outline of this repo lacks internal/engine:\n%s", out)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestOutlineHintFirst(t *testing.T) {
	out := Outline(syntheticRepo(t))
	lines := strings.Split(out, "\n")
	if len(lines) < 2 || !strings.Contains(lines[1], "repo_map") {
		t.Fatalf("the repo_map hint is not the first line:\n%s", out)
	}
}

func TestOutlineWithoutMappedLanguages(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"src/Main.java": "class Main {}\n", "pom.xml": "<project/>\n"})
	out := Outline(root)
	if strings.Contains(out, "repo_map tool") || !strings.Contains(out, "grep") {
		t.Fatalf("outline of a Java repo should point at grep/glob, not repo_map:\n%s", out)
	}
}
