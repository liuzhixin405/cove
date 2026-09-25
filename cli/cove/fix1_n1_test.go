package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/mcp"
	"github.com/liuzhixin405/cove/internal/repl"
	"github.com/liuzhixin405/cove/internal/termui"
	"github.com/liuzhixin405/cove/internal/tool"
)

// New skills and the engine's extra lines (the checkpoint hint) are in the
// summary line.
func TestBackgroundSummaryLineSkillsAndExtra(t *testing.T) {
	got := backgroundSummaryLine(engine.BackgroundSummary{
		SessionSaved: true, NewSkills: []string{"发布流程", "deploy"}, Extra: []string{"已建检查点，/undo 可回退"},
	})
	for _, want := range []string{"新增技能 发布流程、deploy", "已建检查点，/undo 可回退"} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q lacks %q", got, want)
		}
	}
}

// The question tool asks through the REPL's answer relay once the REPL
// installs Runtime.AskUser (it was never assigned, so the tool always failed).
func TestQuestionToolUsesInstalledAskUser(t *testing.T) {
	eng := newTestEngine(t)
	var buf bytes.Buffer
	termui.SetWriter(&buf)
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh(); termui.SetWriter(nil) })

	installQuestionPrompt(eng)
	if eng.Runtime() == nil || eng.Runtime().AskUser == nil {
		t.Fatal("AskUser not installed")
	}
	done := make(chan tool.Result, 1)
	go func() {
		res, _ := tool.NewQuestionTool().Call(context.Background(), tool.Input{"questions": []any{
			map[string]any{"header": "格式", "question": "用哪种缩进？", "options": []any{
				map[string]any{"label": "tabs"}, map[string]any{"label": "spaces"}}},
		}}, tool.Context{Runtime: eng.Runtime()})
		done <- res
	}()
	waitForPermInputCh(t) <- "2"
	select {
	case res := <-done:
		if res.IsError || !strings.Contains(res.Data, "Answer: spaces") {
			t.Fatalf("result = %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("question tool did not return after the answer")
	}
	if !strings.Contains(buf.String(), "用哪种缩进？") {
		t.Fatalf("question not shown: %q", buf.String())
	}
}

// Without an interactive REPL the question gets an empty answer at once.
func TestAskUserQuestionNotInteractive(t *testing.T) {
	oldInteractive := replInteractive
	replInteractive = false
	t.Cleanup(func() { replInteractive = oldInteractive })
	if got := askUserQuestion("q?"); got != "" {
		t.Fatalf("answer = %q", got)
	}
}

// The adapter forwards GenerateOnce, so /init can draft with the model.
func TestReplEngineAdapterIsGuideGenerator(t *testing.T) {
	var a any = replEngineAdapter{}
	if _, ok := a.(interface {
		GenerateOnce(ctx context.Context, system, prompt string) (string, error)
	}); !ok {
		t.Fatal("replEngineAdapter does not implement GenerateOnce")
	}
}

// The turn's model is shown in one dim line when routing picks between two.
func TestTurnModelLine(t *testing.T) {
	if got := turnModelLine("deepseek-flash"); !strings.Contains(got, "模型：deepseek-flash") {
		t.Fatalf("line = %q", got)
	}
}

func TestWireToolDefsVersionNilSafe(t *testing.T) {
	wireToolDefsVersion(nil, nil)
	eng := newTestEngine(t)
	wireToolDefsVersion(eng, nil)
	wireToolDefsVersion(eng, mcp.NewPool())
}
