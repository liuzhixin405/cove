package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ctxt "github.com/liuzhixin405/cove/internal/context"
)

// Collect no longer builds the file tree and repo map; /context computes them
// when asked.
func TestContextCmdShowsStructureOnDemand(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.go"), []byte("package hello\n\nfunc Greet() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pc := &ctxt.ProjectContext{Cwd: dir, Platform: "test"}
	out, err := NewContextCmd().Execute(context.Background(), Input{ProjectContext: pc})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"项目结构", "hello.go", "Repo Map", "Greet"} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("/context output lacks %q:\n%s", want, out.Message)
		}
	}
}
