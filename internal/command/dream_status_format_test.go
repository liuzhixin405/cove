package command

import (
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/dream"
)

// /dream shows the trigger mode and the last session-end worker's result;
// in session_end mode the threshold gates are not presented as pending.
func TestFormatDreamStatusShowsTriggerAndWorker(t *testing.T) {
	st := dream.Status{
		Enabled: true, Trigger: dream.TriggerSessionEnd, MinTurns: 2, MinHours: 12, MinSessions: 3,
		HoursSinceLast: 1, LastConsolidatedAt: time.Now().Add(-time.Hour),
		LastWorkerStartedAt: time.Now(), LastWorkerResult: "后台整理完成：回顾 1 个会话，写入 2 个文件",
	}
	out := FormatDreamStatus(st)
	for _, want := range []string{"触发方式: 对话结束时", "至少 2 个助手回合", "上次会话结束整理", "写入 2 个文件"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "还差") {
		t.Errorf("session_end mode shows pending gates:\n%s", out)
	}
	st.Trigger = dream.TriggerThreshold
	if out := FormatDreamStatus(st); !strings.Contains(out, "触发方式: 门槛") || !strings.Contains(out, "时间门槛") {
		t.Errorf("threshold output:\n%s", out)
	}
}
