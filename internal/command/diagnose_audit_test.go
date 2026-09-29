package command

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/diagnostic"
)

// /diagnose codes grouped the codes by category but listed each group in Go's
// random map order, so E1003 could come before E1001 and the list changed
// every time it was shown.
func TestDiagnoseCodesAreListedInOrder(t *testing.T) {
	out, err := NewDiagnoseCmd().Execute(context.Background(), Input{Args: []string{"codes"}})
	if err != nil {
		t.Fatal(err)
	}
	// Categories are printed in a fixed order under "[category]" headers;
	// within each section the codes must be sorted.
	header := regexp.MustCompile(`\[([a-z]+)\]`)
	code := regexp.MustCompile(`E\d{4}`)
	sections := map[string][]string{}
	current, total := "", 0
	for _, line := range strings.Split(out.Message, "\n") {
		if m := header.FindStringSubmatch(line); m != nil {
			current = m[1]
			continue
		}
		if c := code.FindString(line); c != "" {
			sections[current] = append(sections[current], c)
			total++
		}
	}
	if total < 10 {
		t.Fatalf("only %d codes listed", total)
	}
	for cat, list := range sections {
		if !sort.StringsAreSorted(list) {
			t.Errorf("[%s] codes out of order: %v", cat, list)
		}
	}
}

// /diagnose errors shows one line per coded problem and model with its
// count, latest time, hint and what a remedy did — not one raw line per
// occurrence, and never without a hint for a coded problem.
func TestDiagnoseErrorsGroupsAndExplains(t *testing.T) {
	diagnostic.ResetForTest()
	t.Cleanup(diagnostic.ResetForTest)
	overflow := fmt.Errorf("api: %w", &api.StatusError{Status: 400,
		Msg: `request (17964 tokens) exceeds the available context size (16384 tokens)`})
	diagnostic.ReportError(overflow, diagnostic.Context{Model: "qwen3.6-27b", Provider: "openai-compatible"})
	diagnostic.ReportError(fmt.Errorf("api: %w", &api.StatusError{Status: 400,
		Msg: `request (16569 tokens) exceeds the available context size (16384 tokens)`}),
		diagnostic.Context{Model: "qwen3.6-27b", Provider: "openai-compatible"})

	out, err := NewDiagnoseCmd().Execute(context.Background(), Input{Args: []string{"errors"}})
	if err != nil {
		t.Fatal(err)
	}
	msg := out.Message
	if strings.Count(msg, "E2008") != 1 {
		t.Errorf("E2008 listed %d times, want once (grouped):\n%s", strings.Count(msg, "E2008"), msg)
	}
	for _, want := range []string{"×2", "qwen3.6-27b", "/continue", "上下文超出模型窗口"} {
		if !strings.Contains(msg, want) {
			t.Errorf("output lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "可自动修复") {
		t.Errorf("output still promises automatic fixes:\n%s", msg)
	}
}

// A line from an old errors.log (no model, no source) still loads and shows.
func TestDiagnoseErrorsAcceptsOldLogLines(t *testing.T) {
	var ev diagnostic.RuntimeEvent
	old := `{"time":"2026-09-20T10:00:00+08:00","severity":2,"category":"api","message":"模型调用失败: API error 400: something"}`
	if err := json.Unmarshal([]byte(old), &ev); err != nil {
		t.Fatal(err)
	}
	sums := diagnostic.SummarizeRuntime([]diagnostic.RuntimeEvent{ev, ev})
	if len(sums) != 1 || sums[0].Count != 2 || sums[0].Code != "" {
		t.Fatalf("old events not aggregated: %+v", sums)
	}
}
