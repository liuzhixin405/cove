package main

import (
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/session"
)

// A local llama.cpp with -c 16384 rejects the first request. The person
// sees a coded hint, the remedy learns the real window, and /continue
// finishes the task; /diagnose errors shows the incident once, remedied.
func TestE2E_SmallWindowOverflowIsExplainedAndContinues(t *testing.T) {
	model := newFakeModel(t,
		fakeStep{Status: 400, Body: overflowBody},
		fakeStep{Content: "项目骨架已经创建完成。"},
	)
	e2eHome(t, model)
	s := startREPL(t)

	s.Type("在当前目录写一个 netcore 的 agent 框架项目")
	s.WaitFor("[E2008]", e2eTimeout)
	s.WaitFor("已按服务端返回的 16384", e2eTimeout)
	if w := api.ContextWindowForModel("qwen-test"); w != 16384 {
		t.Fatalf("window not learned from the server's error: %d", w)
	}

	s.Type("/continue")
	s.WaitFor("项目骨架已经创建完成。", e2eTimeout)

	s.Type("/diagnose errors")
	s.WaitFor("上下文超出模型窗口", e2eTimeout)
	s.WaitFor("已处置：窗口", e2eTimeout)
	if n := strings.Count(s.Output(), "E2008 上下文超出模型窗口"); n != 1 {
		t.Fatalf("E2008 listed %d times, want once:\n%s", n, s.Output())
	}
	if reqs := model.Requests(); len(reqs) != 2 {
		t.Fatalf("model called %d times, want 2 (the overflow, then the continue)", len(reqs))
	}
}

// While a permission prompt waits, a typed instruction is not the prompt's
// answer: it is steered into the task, the prompt keeps waiting, and "y"
// then runs the tool. The next model request carries both the tool result
// and the guidance.
func TestE2E_TypeAheadDuringPermissionPromptIsSteeredNotSwallowed(t *testing.T) {
	model := newFakeModel(t,
		fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"mkdir e2e_out"}`}}},
		fakeStep{Content: "输出目录已经建好，后续生成的文件都会放在这个目录里，需要的话我再补充说明。"},
	)
	e2eHome(t, model)
	s := startREPL(t)

	s.Type("建一个输出目录")
	s.WaitFor("需要授权", e2eTimeout)
	s.WaitFor("mkdir e2e_out", e2eTimeout)

	s.Type("目录名用小写")
	s.WaitFor("提示仍在等待回答", e2eTimeout)
	s.WaitFor("[已插入]", e2eTimeout)

	s.Type("y")
	s.WaitFor("输出目录已经建好", e2eTimeout)

	reqs := model.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model called %d times, want 2:\n%s", len(reqs), s.Output())
	}
	var sawTool, sawSteer bool
	for _, m := range reqs[1].Messages {
		if m.Role == "tool" {
			sawTool = true
		}
		if m.Role == "user" && strings.Contains(m.Text(), "[用户指引]") && strings.Contains(m.Text(), "目录名用小写") {
			sawSteer = true
		}
	}
	if !sawTool || !sawSteer {
		t.Fatalf("second request lacks the tool result (%v) or the guidance (%v): %+v", sawTool, sawSteer, reqs[1].Messages)
	}
	if strings.Contains(s.Output(), "已拒绝 bash") {
		t.Fatalf("the typed instruction was taken as a refusal:\n%s", s.Output())
	}
}

// /stop while the model is slow, then /continue: the task is reported
// stopped only once it is, and the resumed turn reaches the model.
func TestE2E_StopThenContinueResumesTheTurn(t *testing.T) {
	model := newFakeModel(t,
		fakeStep{Delay: 10 * time.Second, Content: "太慢了"},
		fakeStep{Content: "从中断处继续完成。"},
	)
	e2eHome(t, model)
	s := startREPL(t)

	s.Type("做一件慢活")
	// Give the task time to reach the model before stopping it.
	time.Sleep(700 * time.Millisecond)
	s.Type("/stop")
	s.WaitFor("已终止", e2eTimeout)

	s.Type("/continue")
	s.WaitFor("从中断处继续完成。", e2eTimeout)
	if reqs := model.Requests(); len(reqs) != 2 {
		t.Fatalf("model called %d times, want 2", len(reqs))
	}
}

// The same request typed again after a restart continues the unfinished
// session instead of adding a copy to /history.
func TestE2E_RestartAndResendContinuesTheSameSession(t *testing.T) {
	model := newFakeModel(t, fakeStep{Status: 400, Body: overflowBody})
	e2eHome(t, model)
	const req = "在该目录写一个 netcore 的 agent 框架的项目，使用 ai 本地模型"

	first := startREPL(t)
	first.Type(req)
	first.WaitFor("[E2008]", e2eTimeout)
	first.Exit()

	model.Append(fakeStep{Content: "接着上次的进度继续。"})
	second := startREPL(t)
	second.Type(req)
	second.WaitFor("[已恢复] 这条请求与会话 #1", e2eTimeout)
	second.WaitFor("接着上次的进度继续。", e2eTimeout)
	second.Type("exit")
	second.Exit()

	all, err := second.app.eng.Store().List()
	if err != nil {
		t.Fatal(err)
	}
	mine := session.FilterByProject(all, currentProjectDir())
	if len(mine) != 1 {
		t.Fatalf("project has %d sessions, want the one continued: %+v", len(mine), mine)
	}
}

// A reply cut off by max_tokens with no content does not loop until the
// iteration cap; the person is told why the turn stopped.
func TestE2E_EmptyTruncatedRepliesStopWithAReason(t *testing.T) {
	model := newFakeModel(t,
		fakeStep{StopReason: "length"}, fakeStep{StopReason: "length"}, fakeStep{StopReason: "length"},
		fakeStep{StopReason: "length"}, fakeStep{StopReason: "length"},
	)
	e2eHome(t, model)
	s := startREPL(t)

	s.Type("想一个很长的问题")
	s.WaitFor("截断", e2eTimeout)
	if n := len(model.Requests()); n > 3 {
		t.Fatalf("model called %d times for empty truncated replies", n)
	}
}

// A tool call whose arguments are not JSON is one E4009 in /diagnose
// errors, with the tool and model, and the turn goes on.
func TestE2E_InvalidToolArgumentsAreDiagnosedOnce(t *testing.T) {
	model := newFakeModel(t,
		fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command": "echo hi`}}},
		fakeStep{Content: "参数格式有问题，我已经重新整理好并完成了这一步的检查工作。"},
	)
	e2eHome(t, model)
	s := startREPL(t)

	s.Type("随便执行一条命令")
	s.WaitFor("参数格式有问题", e2eTimeout)
	s.Type("/diagnose errors")
	s.WaitFor("工具参数非法 JSON", e2eTimeout)
	out := s.Output()
	if n := strings.Count(out, "E4009 工具参数非法 JSON"); n != 1 {
		t.Fatalf("E4009 listed %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out, "模型 qwen-test") {
		t.Fatalf("the diagnostic does not name the model:\n%s", out)
	}
}

// /compact while a task runs is refused with a reason instead of rewriting
// the history the task is appending to.
func TestE2E_CompactIsRefusedWhileATaskRuns(t *testing.T) {
	model := newFakeModel(t, fakeStep{Delay: 2 * time.Second, Content: "慢慢地把这件事做完了，结果都在上面。"})
	e2eHome(t, model)
	s := startREPL(t)

	s.Type("做一件慢活")
	time.Sleep(500 * time.Millisecond)
	s.Type("/compact")
	s.WaitFor("任务运行中不能执行 /compact", e2eTimeout)
	s.WaitFor("慢慢地把这件事做完了", e2eTimeout)
}

// "a" remembers the command prefix for the session: the next line with the
// same prefix (plus read-only companions) runs without a second prompt.
func TestE2E_SessionRuleStopsRepeatPrompts(t *testing.T) {
	model := newFakeModel(t,
		fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"mkdir d1"}`}}},
		fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"cd d1 && mkdir sub && echo \"created: sub\" && ls"}`}}},
		fakeStep{Content: "两个目录都建好了，结构和你要求的一致，后面可以直接往里放代码。"},
	)
	e2eHome(t, model)
	s := startREPL(t)

	s.Type("建两个目录")
	s.WaitFor("需要授权", e2eTimeout)
	s.Type("a")
	s.WaitFor("已记住 bash 中", e2eTimeout)
	s.WaitFor("两个目录都建好了", e2eTimeout)
	if n := strings.Count(s.Output(), "需要授权"); n != 1 {
		t.Fatalf("prompted %d times, want once after [a]:\n%s", n, s.Output())
	}
}

// "p" persists the rule for this project: after a restart in the same
// directory the command runs without asking.
func TestE2E_ProjectRuleSurvivesRestart(t *testing.T) {
	model := newFakeModel(t,
		fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"mkdir p1"}`}}},
		fakeStep{Content: "第一个目录建好了，接下来的目录会按同样的方式创建，不再重复询问。"},
	)
	e2eHome(t, model)

	first := startREPL(t)
	first.Type("建一个目录")
	first.WaitFor("需要授权", e2eTimeout)
	first.Type("p")
	first.WaitFor("已记住 bash 中", e2eTimeout)
	first.WaitFor("第一个目录建好了", e2eTimeout)
	first.Exit()

	model.Append(
		fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"mkdir -p p2/inner"}`}}},
		fakeStep{Content: "第二个目录也建好了，规则是上次记住的，所以这次没有再问你。"},
	)
	second := startREPL(t)
	second.Type("再建一个目录")
	second.WaitFor("第二个目录也建好了", e2eTimeout)
	if strings.Contains(second.Output(), "需要授权") {
		t.Fatalf("the persisted rule did not apply after the restart:\n%s", second.Output())
	}
}

// The git routine group remembered once covers the rest of the cycle, quoted
// commit messages with operators included.
func TestE2E_GitRoutineGroupCoversTheWholeCycle(t *testing.T) {
	model := newFakeModel(t,
		fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"git init -q && git add . && git commit -q -m \"feat: scaffold; first cut\" --allow-empty"}`}}},
		fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"git commit -q --allow-empty -m \"fix: a && b\" && git log --oneline -1"}`}}},
		fakeStep{Content: "两次提交都完成了，历史里可以看到 scaffold 和 fix 两条记录。"},
	)
	e2eHome(t, model)
	s := startREPL(t)

	s.Type("初始化仓库并提交两次")
	s.WaitFor("需要授权", e2eTimeout)
	s.WaitFor("git 常规操作", e2eTimeout)
	s.Type("a")
	s.WaitFor("两次提交都完成了", e2eTimeout)
	if n := strings.Count(s.Output(), "需要授权"); n != 1 {
		t.Fatalf("prompted %d times, want once for the whole git cycle:\n%s", n, s.Output())
	}
}

// A second prompt after "a" is not the rule failing: the line runs other
// programs, so the "mkdir" rule is not named — a rule for an unrelated
// program used to be, which read as if the command had something to do with
// it (a "sed … > s2.json" line was said to fall outside a docref.exe rule).
func TestE2E_SecondPromptDoesNotNameUnrelatedRules(t *testing.T) {
	model := newFakeModel(t,
		fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"mkdir agent"}`}}},
		fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"cd agent && dotnet --version"}`}}},
		fakeStep{Content: "目录和 SDK 都确认过了，接下来按这个结构开始创建解决方案和项目文件。"},
	)
	e2eHome(t, model)
	s := startREPL(t)

	s.Type("初始化一个 dotnet 项目")
	s.WaitFor("需要授权", e2eTimeout)
	s.Type("a")
	s.WaitFor("已记住 bash 中", e2eTimeout)
	s.WaitForCount("需要授权", 2, e2eTimeout)
	s.WaitFor("cd agent && dotnet --version", e2eTimeout)
	s.WaitForCount("按键即答", 2, e2eTimeout)
	out := s.Output()
	second := out[strings.LastIndex(out, "需要授权"):]
	if !strings.Contains(second, "dotnet --version") {
		t.Fatalf("second prompt does not show the command:\n%s", out)
	}
	if strings.Contains(second, "未覆盖这一行") || strings.Contains(second, `"mkdir"`) {
		t.Fatalf("second prompt names the unrelated mkdir rule:\n%s", second)
	}
	s.Type("y")
	s.WaitFor("目录和 SDK 都确认过了", e2eTimeout)
}
