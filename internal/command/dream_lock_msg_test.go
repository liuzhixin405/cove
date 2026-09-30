package command

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/dream"
)

// A lock held by another process is reported as such. The message used to
// also blame "this process consolidated within the hour" (a completed run
// kept its lock for an hour); a completed run now leaves a done lock that
// does not block, so that clause was wrong.
func TestDreamRunLockHeldNamesAnotherProcess(t *testing.T) {
	backgroundHome(t)
	dream.NewRunner(doneProvider{}, "m", "")
	if _, ok, err := dream.TryAcquireConsolidationLock(); err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	out, err := NewDreamCmd().Execute(context.Background(), Input{Args: []string{"run"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Message, "整理锁被占用") || strings.Contains(out.Message, "1 小时") {
		t.Fatalf("/dream run output = %q", out.Message)
	}
}
