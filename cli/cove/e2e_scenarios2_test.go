package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A turn that failed leaves a draft; the next start says so and "继续"
// resumes it in the same session.
func TestE2E_InterruptedDraftIsOfferedAndResumedAfterRestart(t *testing.T) {
	model := newFakeModel(t, fakeStep{Status: 400, Body: overflowBody})
	e2eHome(t, model)

	first := startREPL(t)
	first.Type("把 README 翻译成英文")
	first.WaitFor("[E2008]", e2eTimeout)
	first.Exit()

	model.Append(fakeStep{Content: "README 已经翻译完成，英文版放在同一目录下。"})
	second := startREPL(t)
	second.WaitFor("未完成", e2eTimeout)
	second.Type("继续")
	second.WaitFor("README 已经翻译完成", e2eTimeout)
	reqs := model.Requests()
	last := reqs[len(reqs)-1]
	var sawOriginal bool
	for _, m := range last.Messages {
		if m.Role == "user" && strings.Contains(m.Text(), "把 README 翻译成英文") {
			sawOriginal = true
		}
	}
	if !sawOriginal {
		t.Fatalf("the resumed request does not carry the original task:\n%+v", last.Messages)
	}
}

// The question tool: the person picks an option by number and the model
// gets the label back.
func TestE2E_QuestionToolRelaysTheAnswer(t *testing.T) {
	model := newFakeModel(t,
		fakeStep{ToolCalls: []fakeToolCall{{Name: "question", Args: `{"questions":[{"header":"框架","question":"用哪个 Web 框架？","options":[{"label":"Minimal API","description":"轻量"},{"label":"MVC","description":"传统"}]}]}`}}},
		fakeStep{Content: "好，按 MVC 来搭建，下面开始创建控制器和视图的目录结构。"},
	)
	e2eHome(t, model)
	s := startREPL(t)

	s.Type("帮我选一个框架")
	s.WaitFor("用哪个 Web 框架", e2eTimeout)
	s.Type("2")
	s.WaitFor("按 MVC 来搭建", e2eTimeout)
	reqs := model.Requests()
	var sawAnswer bool
	for _, m := range reqs[len(reqs)-1].Messages {
		if m.Role == "tool" && strings.Contains(m.Text(), "MVC") {
			sawAnswer = true
		}
	}
	if !sawAnswer {
		t.Fatalf("the model did not receive the chosen option:\n%+v", reqs[len(reqs)-1].Messages)
	}
}

// A 429 that outlives the provider's retries ends the turn with a coded
// hint; /continue picks it up once the server answers.
func TestE2E_RateLimitIsCodedAndContinues(t *testing.T) {
	if testing.Short() {
		t.Skip("slow (>3s): skipped under -short")
	}
	steps := []fakeStep{}
	for i := 0; i < 8; i++ {
		steps = append(steps, fakeStep{Status: 429, Body: `{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`})
	}
	steps = append(steps, fakeStep{Content: "限流过去了，这是完整的回答。"})
	model := newFakeModel(t, steps...)
	e2eHome(t, model)
	s := startREPL(t)

	s.Type("回答一个问题")
	s.WaitFor("[E2003]", 60*time.Second)
	// Drain whatever 429s are left so the continue reaches the reply.
	for i := 0; i < 8; i++ {
		if len(model.Requests()) >= 8 {
			break
		}
		s.Type("/continue")
		s.WaitForCount("[E2003]", i+2, 60*time.Second)
	}
	s.Type("/continue")
	s.WaitFor("限流过去了", 60*time.Second)
}

// "p" writes policies.json with the group and /permissions lists it.
func TestE2E_PersistedRuleIsVisibleInPermissionsAndOnDisk(t *testing.T) {
	if testing.Short() {
		t.Skip("slow (>3s): skipped under -short")
	}
	model := newFakeModel(t,
		fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"git init -q"}`}}},
		fakeStep{Content: "仓库初始化完成，可以开始添加文件并进行第一次提交了。"},
	)
	home, _ := e2eHome(t, model)
	s := startREPL(t)

	s.Type("初始化仓库")
	s.WaitFor("需要授权", e2eTimeout)
	s.Type("p")
	s.WaitFor("已写入", e2eTimeout)
	s.WaitFor("仓库初始化完成", e2eTimeout)

	raw, err := os.ReadFile(filepath.Join(home, ".cove", "policies.json"))
	if err != nil {
		t.Fatalf("policies.json not written: %v", err)
	}
	var rules []map[string]any
	if err := json.Unmarshal(raw, &rules); err != nil {
		// Some layouts wrap the list; accept either shape.
		var wrapped map[string]any
		if err2 := json.Unmarshal(raw, &wrapped); err2 != nil {
			t.Fatalf("policies.json is not JSON: %v\n%s", err, raw)
		}
	}
	if !strings.Contains(string(raw), `"command_group": "git"`) && !strings.Contains(string(raw), `"command_group":"git"`) {
		t.Fatalf("policies.json lacks the git group rule:\n%s", raw)
	}

	s.Type("/permissions")
	s.WaitFor("git", e2eTimeout)
	if !strings.Contains(s.Output(), "allow") && !strings.Contains(s.Output(), "允许") {
		t.Fatalf("/permissions does not show the persisted allow rule:\n%s", s.Output())
	}
}

// A finished session resumed with /history N continues with its history.
func TestE2E_HistoryResumeCarriesTheOldConversation(t *testing.T) {
	model := newFakeModel(t, fakeStep{Content: "这是第一次对话的回答，内容是关于解析器结构的说明。"})
	e2eHome(t, model)

	first := startREPL(t)
	first.Type("讲讲解析器的结构")
	first.WaitFor("第一次对话的回答", e2eTimeout)
	first.Exit()

	model.Append(fakeStep{Content: "接着上次的解析器话题，这次说说错误恢复的部分。"})
	second := startREPL(t)
	second.Type("/history")
	second.WaitFor("讲讲解析器的结构", e2eTimeout)
	second.Type("/history 1")
	second.WaitFor("恢复", e2eTimeout)
	second.Type("那错误恢复呢")
	second.WaitFor("错误恢复的部分", e2eTimeout)
	reqs := model.Requests()
	last := reqs[len(reqs)-1]
	var sawOld bool
	for _, m := range last.Messages {
		if strings.Contains(m.Text(), "讲讲解析器的结构") {
			sawOld = true
		}
	}
	if !sawOld {
		t.Fatalf("resumed session did not send the old conversation:\n%+v", last.Messages)
	}
}

// Refusing a tool prints the refusal and the model is told; the turn goes on.
func TestE2E_RefusedToolIsReportedToBoth(t *testing.T) {
	model := newFakeModel(t,
		fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"rm -rf build"}`}}},
		fakeStep{Content: "好的，不删除 build 目录，我改用清理单个文件的方式继续处理。"},
	)
	e2eHome(t, model)
	s := startREPL(t)

	s.Type("清理构建产物")
	s.WaitFor("需要授权", e2eTimeout)
	s.Type("n")
	s.WaitFor("已拒绝 bash", e2eTimeout)
	s.WaitFor("不删除 build 目录", e2eTimeout)
	reqs := model.Requests()
	var toldModel bool
	for _, m := range reqs[len(reqs)-1].Messages {
		if m.Role == "tool" && strings.Contains(strings.ToLower(m.Text()), "denied") {
			toldModel = true
		}
	}
	if !toldModel {
		t.Fatalf("the model was not told the call was refused:\n%+v", reqs[len(reqs)-1].Messages)
	}
}

// /tasks shows the running task and the guidance waiting for it; once the
// model consumed the guidance the note is gone.
func TestE2E_TasksShowsRunningTaskAndPendingGuidance(t *testing.T) {
	if testing.Short() {
		t.Skip("slow (>3s): skipped under -short")
	}
	model := newFakeModel(t,
		fakeStep{Delay: 1500 * time.Millisecond, ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"echo step1"}`}}},
		fakeStep{Content: "第一步已经完成，指引也收到了，接下来按你的要求只用小写命名。"},
	)
	e2eHome(t, model)
	s := startREPL(t)

	s.Type("做一件慢活")
	time.Sleep(300 * time.Millisecond)
	s.Type("只用小写命名")
	s.WaitFor("[已插入]", e2eTimeout)
	s.Type("/tasks")
	s.WaitFor("待生效指引", e2eTimeout)
	deadline := time.Now().Add(e2eTimeout)
	for !strings.Contains(s.Output(), "第一步已经完成") {
		if time.Now().After(deadline) {
			var summary []string
			for i, r := range model.Requests() {
				var roles []string
				for _, m := range r.Messages {
					roles = append(roles, m.Role+":"+strings.TrimSpace(m.Text())[:min(len(strings.TrimSpace(m.Text())), 40)])
				}
				summary = append(summary, "req"+string(rune('0'+i))+": "+strings.Join(roles, " | "))
			}
			t.Fatalf("reply never shown; %d requests:\n%s\noutput:\n%s", len(model.Requests()), strings.Join(summary, "\n"), s.Output())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The reply is on screen a few hundred milliseconds before the task
	// goroutine has finished its bookkeeping (session save), so /tasks may
	// still say "running" once; ask until it says otherwise.
	idle := false
	for i := 0; i < 20 && !idle; i++ {
		s.Type("/tasks")
		time.Sleep(300 * time.Millisecond)
		idle = strings.Contains(s.Output(), "当前没有运行中的任务")
	}
	if !idle {
		t.Fatalf("task never went idle after its reply:\n%s", s.Output())
	}
	out := s.Output()
	idx := strings.LastIndex(out, "当前没有运行中的任务")
	if strings.Contains(out[idx:], "待生效指引") {
		t.Fatalf("guidance still listed after the model consumed it:\n%s", out[idx:])
	}
	reqs := model.Requests()
	var sawSteer bool
	for _, m := range reqs[len(reqs)-1].Messages {
		if strings.Contains(m.Text(), "[用户指引]") && strings.Contains(m.Text(), "只用小写命名") {
			sawSteer = true
		}
	}
	if !sawSteer {
		t.Fatalf("guidance never reached the model:\n%+v", reqs[len(reqs)-1].Messages)
	}
}
