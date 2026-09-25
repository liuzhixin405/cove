package command

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/dream"
)

// Final fix (Minor 6): the lock a completed run leaves behind is this
// process's own and stays for an hour, so "/dream run" right after a
// consolidation hits it; the message must cover that case too.
func TestDreamRunLockHeldMentionsRecentRunInThisProcess(t *testing.T) {
	backgroundHome(t)
	dream.NewRunner(doneProvider{}, "m", "")
	if _, ok, err := dream.TryAcquireConsolidationLock(); err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	out, err := NewDreamCmd().Execute(context.Background(), Input{Args: []string{"run"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"整理锁被占用", "本进程", "1 小时内"} {
		if !strings.Contains(out.Message, want) {
			t.Fatalf("/dream run output = %q, want %q", out.Message, want)
		}
	}
}
