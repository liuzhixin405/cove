package session

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
)

// Pruning keeps `keep` sessions per project directory, so a busy project no
// longer pushes every other project's history out.
func TestPruneCountsPerProject(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	projA := filepath.Join(t.TempDir(), "a")
	projB := filepath.Join(t.TempDir(), "b")
	n := 0
	for _, p := range []struct {
		dir   string
		count int
	}{{projA, 4}, {projB, 2}, {"", 3}} {
		for i := 0; i < p.count; i++ {
			id := fmt.Sprintf("x%02d", n)
			r := &Record{ID: id, Model: "m", Cwd: p.dir, Messages: []api.Message{{Role: "user", Content: id}}}
			if err := s.Save(r); err != nil {
				t.Fatal(err)
			}
			s.setIndexUpdatedAt(t, id, base.Add(time.Duration(n)*time.Hour))
			n++
		}
	}
	// Upper-case spelling of project A on Windows counts as the same project.
	if runtime.GOOS == "windows" {
		r := &Record{ID: "x99", Model: "m", Cwd: strings.ToUpper(projA), Messages: []api.Message{{Role: "user", Content: "u"}}}
		if err := s.Save(r); err != nil {
			t.Fatal(err)
		}
		s.setIndexUpdatedAt(t, "x99", base.Add(-time.Hour)) // oldest
	}

	removed, err := s.Prune(2)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"x00": false, "x01": false, "x02": true, "x03": true, // project A: newest 2
		"x04": true, "x05": true, // project B: both
		"x06": false, "x07": true, "x08": true, // no project: newest 2
	}
	wantRemoved := 3
	if runtime.GOOS == "windows" {
		want["x99"] = false
		wantRemoved = 4
	}
	if removed != wantRemoved {
		t.Errorf("removed %d, want %d", removed, wantRemoved)
	}
	for id, keep := range want {
		_, err := os.Stat(filepath.Join(s.dir, id+".jsonl"))
		if exists := err == nil; exists != keep {
			t.Errorf("%s exists=%v, want %v", id, exists, keep)
		}
	}
}

// Sessions started in subdirectories of one repository count as one project,
// like the per-project memory directory.
func TestPruneGroupsByRepositoryRoot(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, cwd := range []string{repo, filepath.Join(repo, "a"), filepath.Join(repo, "b")} {
		id := fmt.Sprintf("r%d", i)
		if err := s.Save(&Record{ID: id, Model: "m", Cwd: cwd, Messages: []api.Message{{Role: "user", Content: id}}}); err != nil {
			t.Fatal(err)
		}
		s.setIndexUpdatedAt(t, id, base.Add(time.Duration(i)*time.Hour))
	}
	removed, err := s.Prune(2)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d, want 1 (one repository, keep 2)", removed)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "r0.jsonl")); !os.IsNotExist(err) {
		t.Fatal("oldest session of the repository kept")
	}
}
