package command

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/diagnostic"
	"github.com/liuzhixin405/cove/internal/dream"
	"github.com/liuzhixin405/cove/internal/memory"
)

// Fix round 1 (item 2): the start notice used to count sessions after the
// lock was stamped, so it always said "回顾 0 个会话".
func TestDreamRunReportsSessionCount(t *testing.T) {
	home := backgroundHome(t)
	writeSessions(t, home, "a", "b", "c")
	dream.NewRunner(doneProvider{}, "m", "")

	out, err := NewDreamCmd().Execute(context.Background(), Input{Args: []string{"run"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Message, "3 个会话") {
		t.Fatalf("/dream run output = %q, want 3 sessions", out.Message)
	}
	deadline := time.Now().Add(10 * time.Second)
	for dream.ActiveTask() != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

// Fix round 1 (item 1): /doctor shows the background-learning and policy
// items too, not only /diagnose.
func TestDoctorShowsBackgroundAndPolicyItems(t *testing.T) {
	backgroundHome(t)
	diagnostic.BackgroundStatusFn = func() diagnostic.BackgroundStatus {
		return diagnostic.BackgroundStatus{
			Dream:  dream.Status{Enabled: true, HoursSinceLast: -1, MinHours: 12, MinSessions: 3},
			Memory: memory.Stats{FileCount: 7},
		}
	}
	diagnostic.PolicyLoadErrorFn = func() error { return errors.New("load x/policies.json: bad") }
	t.Cleanup(func() { diagnostic.BackgroundStatusFn, diagnostic.PolicyLoadErrorFn = nil, nil })

	out, err := NewDoctorCmd().Execute(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"后台学习", "7 条", "权限规则文件", "policies.json"} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("/doctor lacks %q:\n%s", want, out.Message)
		}
	}
}
