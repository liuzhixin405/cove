package main

import (
	"os"
	"testing"

	"github.com/liuzhixin405/cove/internal/dream"
	"github.com/liuzhixin405/cove/internal/hooks"
	"github.com/liuzhixin405/cove/internal/memory"
)

func TestParseDreamProjectFlag(t *testing.T) {
	opts, err := parseCLIArgs([]string{"--profile", "p", dream.WorkerProjectFlag, "/repo", dream.WorkerFlag, "/tmp/s"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.action != actionDreamWorker || opts.dreamWorkerDir != "/tmp/s" || opts.dreamProjectRoot != "/repo" {
		t.Fatalf("opts = %+v", opts)
	}
	if _, err := parseCLIArgs([]string{dream.WorkerProjectFlag}); err == nil {
		t.Fatal("--dream-project without a value parsed")
	}
}

// The exit path passes the current project root to the worker.
func TestSessionEndPassesProjectRootToWorker(t *testing.T) {
	eng := newTestEngine(t)
	resetSessionEnd(t, hooks.NewManager())
	rec := stubDreamSpawn(t, nil)
	eng.LoadMessages(twoTurnMessages())
	noteTurnCompleted()
	noteTurnCompleted()

	finishSession(eng, nil)

	if len(rec.calls) != 1 {
		t.Fatalf("spawn called %d times", len(rec.calls))
	}
	cwd, _ := os.Getwd()
	want := memory.ProjectRoot(cwd)
	args := rec.calls[0]
	found := false
	for i := 0; i+1 < len(args); i++ {
		if args[i] == dream.WorkerProjectFlag && args[i+1] == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("spawn args %q lack %s %s", args, dream.WorkerProjectFlag, want)
	}
}
