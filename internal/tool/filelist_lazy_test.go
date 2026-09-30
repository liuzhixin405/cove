package tool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

// countLstat counts the Lstat calls the listing and confinement make.
func countLstat(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	orig := lstat
	lstat = func(name string) (os.FileInfo, error) { n.Add(1); return orig(name) }
	t.Cleanup(func() { lstat = orig })
	return &n
}

// grep's built-in search used to stat every listed file before searching any
// (seconds per call in a large repository on Windows), though it stops after
// grepMaxLines matches. Files are now checked as they are reached.
func TestBuiltinGrepConfinesFilesLazily(t *testing.T) {
	for _, git := range []bool{false, true} {
		t.Run(fmt.Sprint("git=", git), func(t *testing.T) {
			dir := t.TempDir()
			if git {
				gitInit(t, dir)
			}
			files := map[string]string{}
			for i := 0; i < 600; i++ {
				files[fmt.Sprintf("d%02d/f%04d.txt", i%20, i)] = "needle\n"
			}
			writeTree(t, dir, files)
			withoutRipgrep(t)
			n := countLstat(t)
			res := grep(t, dir, Input{"pattern": "needle"})
			if res.IsError || !strings.Contains(res.Data, "needle") {
				t.Fatalf("grep = %q", res.Data)
			}
			if got := n.Load(); got > grepMaxLines+50 {
				t.Errorf("grep made %d Lstat calls for a result capped at %d lines", got, grepMaxLines)
			}

			n.Store(0)
			gres, err := NewGlobTool().Call(context.Background(), Input{"pattern": "**/*.txt"}, Context{Cwd: dir})
			if err != nil || gres.IsError {
				t.Fatalf("glob = %q, %v", gres.Data, err)
			}
			if got := strings.Count(gres.Data, "\n"); got != 200 {
				t.Errorf("glob listed %d lines, want 200 + overflow note", got)
			}
			if !strings.Contains(gres.Data, "up to 400 more files") {
				t.Errorf("glob overflow note: %q", gres.Data[strings.LastIndex(gres.Data, "\n"):])
			}
			if got := n.Load(); got > 250 {
				t.Errorf("glob made %d Lstat calls for 200 shown entries", got)
			}
		})
	}
}

// git ls-files --cached still lists files deleted from the work tree; they
// stay out of results now that the existence check is made lazily too.
func TestGlobSkipsGitFilesDeletedFromWorkTree(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	writeTree(t, dir, map[string]string{"keep.txt": "x", "gone.txt": "x"})
	add := []string{"add", "keep.txt", "gone.txt"}
	if out, err := runGit(dir, add...); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	if err := os.Remove(dir + "/gone.txt"); err != nil {
		t.Fatal(err)
	}
	res, _ := NewGlobTool().Call(context.Background(), Input{"pattern": "*.txt"}, Context{Cwd: dir})
	if strings.Contains(res.Data, "gone.txt") || !strings.Contains(res.Data, "keep.txt") {
		t.Fatalf("glob = %q", res.Data)
	}
	withoutRipgrep(t)
	if g := grep(t, dir, Input{"pattern": "x", "files_only": true}); strings.Contains(g.Data, "gone.txt") {
		t.Fatalf("grep = %q", g.Data)
	}
}

func runGit(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	return cmd.CombinedOutput()
}
