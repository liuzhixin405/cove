package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/delegate"
)

func TestFormatResultSummarisesByExitReason(t *testing.T) {
	out := FormatResult(&ExecutionResult{
		PlanID: "p",
		Tasks: []*Task{
			{ID: "a", Title: "A", Status: "done", ExitReason: delegate.ExitCompleted},
			{ID: "b", Title: "B", Status: "done", ExitReason: delegate.ExitCompleted},
			{ID: "c", Title: "C", Status: "done"},
			{ID: "d", Title: "D", Status: "failed", ExitReason: delegate.ExitMaxIterations, Error: "已达上限", Output: "已完成 2 个步骤"},
			{ID: "e", Title: "E", Status: "failed", ExitReason: delegate.ExitError, Error: "upstream 500"},
		},
	})
	want := "汇总：3 个任务完成，1 个到达上限（含部分结果），1 个失败"
	if !strings.Contains(out, want) {
		t.Fatalf("summary missing %q:\n%s", want, out)
	}
	if !strings.Contains(out, "[exit: max_iterations]") {
		t.Fatalf("per-task exit reason missing:\n%s", out)
	}
}

func TestFormatResultCountsLoopInterruptedAndSkipped(t *testing.T) {
	out := FormatResult(&ExecutionResult{
		PlanID: "p",
		Tasks: []*Task{
			{ID: "a", Status: "failed", ExitReason: delegate.ExitLoop},
			{ID: "b", Status: "cancelled", Error: "cancelled by user"},
			{ID: "c", Status: "failed", ExitReason: delegate.ExitInterrupted, Error: "timed out"},
			{ID: "d", Status: "skipped"},
		},
	})
	want := "汇总：1 个陷入循环，2 个已中断，1 个跳过"
	if !strings.Contains(out, want) {
		t.Fatalf("summary missing %q:\n%s", want, out)
	}
}

func TestExecuteRecordsTheSubAgentExitReason(t *testing.T) {
	p := &busyProvider{}
	pe := cappedExecutor(t, p)
	plan := &Plan{ID: "p", Tasks: []*Task{{ID: "a", Title: "A", Description: "A", Status: "pending"}}}
	res := pe.Execute(context.Background(), plan)
	if got := res.Tasks[0].ExitReason; got != delegate.ExitMaxIterations {
		t.Fatalf("ExitReason = %q, want max_iterations", got)
	}
	if !strings.Contains(FormatResult(res), "1 个到达上限（含部分结果）") {
		t.Fatalf("summary:\n%s", FormatResult(res))
	}
}
