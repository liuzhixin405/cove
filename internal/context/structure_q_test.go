package context

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countBuilders swaps the file-tree and repo-map builders for counting ones.
func countBuilders(t *testing.T, delay chan struct{}) (trees, maps *int32) {
	t.Helper()
	trees, maps = new(int32), new(int32)
	origTree, origMap := buildFileTree, buildRepoMap
	buildFileTree = func(root string) string { atomic.AddInt32(trees, 1); return "tree of " + filepath.Base(root) + "\n" }
	buildRepoMap = func(root string) string {
		atomic.AddInt32(maps, 1)
		if delay != nil {
			<-delay
		}
		return "map.go:\n  - func X()\n"
	}
	t.Cleanup(func() { buildFileTree, buildRepoMap = origTree, origMap })
	return trees, maps
}

// The system prompt no longer carries the file tree or repo map, so startup
// must not build them: Collect used to walk the whole repository before cove
// could start.
func TestCollectDoesNotBuildTreeOrRepoMap(t *testing.T) {
	trees, maps := countBuilders(t, nil)
	pc := Collect()
	if n := atomic.LoadInt32(trees) + atomic.LoadInt32(maps); n != 0 {
		t.Fatalf("Collect built the tree/map %d times, want 0", n)
	}
	if pc.FileTree != "" || pc.RepoMap != "" {
		t.Fatalf("Collect filled FileTree/RepoMap: %q / %q", pc.FileTree, pc.RepoMap)
	}
}

func TestStructureIsLazyAndCached(t *testing.T) {
	trees, maps := countBuilders(t, nil)
	pc := &ProjectContext{Cwd: t.TempDir()}
	for i := 0; i < 2; i++ {
		tree, rm, ok := pc.Structure(5 * time.Second)
		if !ok || !strings.Contains(tree, "tree of") || !strings.Contains(rm, "func X") {
			t.Fatalf("Structure = %q, %q, %v", tree, rm, ok)
		}
	}
	if atomic.LoadInt32(trees) != 1 || atomic.LoadInt32(maps) != 1 {
		t.Fatalf("built tree %d / map %d times, want once each", *trees, *maps)
	}
}

func TestStructureTimesOutWithoutBlocking(t *testing.T) {
	release := make(chan struct{})
	countBuilders(t, release)
	pc := &ProjectContext{Cwd: t.TempDir()}
	start := time.Now()
	if _, _, ok := pc.Structure(50 * time.Millisecond); ok {
		t.Fatal("Structure reported done while the repo map was still building")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Structure blocked past its timeout")
	}
	close(release)
	if _, rm, ok := pc.Structure(5 * time.Second); !ok || rm == "" {
		t.Fatalf("after the build finished: map %q ok %v", rm, ok)
	}
}

func TestStructureRealBuilders(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc Hello() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree, rm, ok := (&ProjectContext{Cwd: dir}).Structure(5 * time.Second)
	if !ok || !strings.Contains(tree, "a.go") || !strings.Contains(rm, "Hello") {
		t.Fatalf("Structure = %q, %q, %v", tree, rm, ok)
	}
}
