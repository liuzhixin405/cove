package checkpoint

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCreateKeepsOnlyNewestCheckpoints(t *testing.T) {
	isolatedGit(t)
	oldKeep, oldSlack := keepCheckpoints, pruneSlack
	keepCheckpoints, pruneSlack = 3, 0
	t.Cleanup(func() { keepCheckpoints, pruneSlack = oldKeep, oldSlack })

	dir := t.TempDir()
	mgr, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for i := 0; i < 6; i++ {
		writeFile(t, filepath.Join(dir, "a.txt"), fmt.Sprintf("v%d", i))
		if last, err = mgr.Create(fmt.Sprintf("cp%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	mgr.WaitMaintenance() // trimming runs in the background
	out, err := mgr.gitOutput(mgr.env(), "rev-list", mgr.refName)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Fields(out)); n != 3 {
		t.Fatalf("chain has %d checkpoints, want 3", n)
	}
	head := mgr.getRef(mgr.env(), mgr.refName)
	if mgr.treeOf(head) != mgr.treeOf(last) {
		t.Fatal("newest checkpoint content lost by trimming")
	}
	list := mgr.List()
	if len(list) != 3 || !strings.Contains(list[0], "cp5") || !strings.Contains(list[2], "cp3") {
		t.Fatalf("List = %q", list)
	}
	// The newest hash returned by Create must still be restorable.
	writeFile(t, filepath.Join(dir, "a.txt"), "changed")
	if _, err := mgr.Restore(head); err != nil {
		t.Fatalf("restore newest after trim: %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "a.txt")); got != "v5" {
		t.Fatalf("restored %q", got)
	}
}

func TestCreateRunsGCPeriodically(t *testing.T) {
	isolatedGit(t)
	oldEvery, oldRun := gcEvery, runGC
	var calls atomic.Int32
	gcEvery = 2
	runGC = func(storeDir string) { calls.Add(1) }
	t.Cleanup(func() { gcEvery, runGC = oldEvery, oldRun })

	dir := t.TempDir()
	mgr, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		writeFile(t, filepath.Join(dir, "a.txt"), fmt.Sprintf("g%d", i))
		if _, err := mgr.Create(""); err != nil {
			t.Fatal(err)
		}
	}
	mgr.WaitMaintenance()
	if got := calls.Load(); got != 2 {
		t.Fatalf("gc ran %d times over 5 creations, want 2", got)
	}
}
