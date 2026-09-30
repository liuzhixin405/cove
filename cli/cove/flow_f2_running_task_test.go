package main

// Flow F2 (test design 5.3): typing while a task runs — guidance, a queued
// request, Ctrl+C and "继续", a permission prompt that keeps waiting, and the
// commands refused or allowed mid-task. One home and one fake model are
// shared; each subtest scripts the model afresh and starts its own REPL.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFlowF2_RunningTask(t *testing.T) {
	model := newFakeModel(t)
	home, project := e2eHome(t, model)
	cfgDir := filepath.Join(home, ".cove")
	draftPath := filepath.Join(cfgDir, "interrupted.json")
	writeTestFile(t, filepath.Join(project, "notes.txt"), "笔记：发布前要跑全部测试\n")

	start := func(t *testing.T, steps ...fakeStep) (*e2eSession, flow) {
		t.Helper()
		model.Reset(steps...)
		// A draft left by an earlier subtest would be offered at start-up
		// and taken by a "继续" typed here.
		_ = os.Remove(draftPath)
		s := startREPL(t)
		return s, flow{t: t, s: s, model: model}
	}
	// slow holds the model's answer long enough to type into the running
	// task; the subtest ends it with Interrupt or waits it out.
	const slow = 1500 * time.Millisecond

	t.Run("任务中普通文本成为指引且/tasks显示并送达模型", func(t *testing.T) {
		s, f := start(t,
			fakeStep{Delay: slow, ToolCalls: []fakeToolCall{bashCall("echo step1")}},
			textReply("第一步已经完成，指引也收到了，之后只用小写命名，处理完成。"))
		s.Type("做一件分两步的活")
		model.WaitRequests(t, 1, e2eTimeout)
		s.Type("只用小写命名")
		s.WaitFor("[已插入]", e2eTimeout)
		s.Type("/tasks")
		s.WaitFor("待生效指引: 只用小写命名", e2eTimeout)
		s.WaitFor("处理完成。", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		reqs := f.Requests(2)
		if n := countMessages(reqs[1], "user", "[用户指引] 只用小写命名"); n != 1 {
			f.Fatalf("the second request carries the guidance %d times, want once", n)
		}
		if n := countMessages(reqs[0], "", "只用小写命名"); n != 0 {
			f.Fatalf("the guidance reached the request already in flight")
		}
		rec := capturedFromRecord(sessionFile(t, home, s.app.eng.SessionID()))
		if countMessages(rec, "user", "只用小写命名") != 1 {
			f.Fatalf("the saved session lacks the guidance: %q", requestTexts(rec))
		}
		// Not queued as a task of its own.
		f.Absent("[已排队]")
	})

	t.Run("任务中第二条请求排队并在前一条完成后自动开始", func(t *testing.T) {
		s, f := start(t,
			fakeStep{Delay: slow, Content: "第一件事：目录结构已经整理完成。"},
			textReply("第二件事：笔记要求发布前跑全部测试，阅读完成。"))
		s.Type("第一件事：整理目录结构")
		model.WaitRequests(t, 1, e2eTimeout)
		// A message with an attachment cannot travel as guidance; it queues.
		s.Type("第二件事：看看 @notes.txt")
		s.WaitFor("[已排队] 当前任务结束后执行", e2eTimeout)
		s.Type("/tasks")
		s.WaitFor("排队中 (1)", e2eTimeout)
		s.WaitFor("第二件事：笔记要求", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		out := s.Output()
		if strings.Index(out, "第一件事：目录结构已经整理完成") > strings.Index(out, "第二件事：笔记要求") {
			f.Fatalf("the queued request ran before the first one finished")
		}
		reqs := f.Requests(2)
		if countMessages(reqs[1], "user", "附件 notes.txt") != 1 {
			f.Fatalf("the queued request did not carry its attachment: %q", requestTexts(reqs[1]))
		}
		if countMessages(reqs[1], "assistant", "第一件事：目录结构已经整理完成") != 1 {
			f.Fatalf("the queued request does not follow the first exchange: %q", requestTexts(reqs[1]))
		}
		if countMessages(reqs[0], "user", "附件 notes.txt") != 0 {
			f.Fatalf("the queued message leaked into the running request")
		}
	})

	t.Run("Ctrl+C停止任务并保存草稿继续恢复同一回合且请求不重复", func(t *testing.T) {
		s, f := start(t,
			toolReply(bashCall("echo ctrlc_step1")),
			fakeStep{Delay: 30 * time.Second, Content: "这条回答不会出现"},
			textReply("从中断处继续，第二步也完成了，处理完成。"))
		t.Cleanup(func() { _ = os.Remove(draftPath) })
		s.Type("做一件会被打断的活")
		model.WaitRequests(t, 2, e2eTimeout)
		began := time.Now()
		s.Interrupt()
		s.WaitFor(interruptNote, e2eTimeout)
		s.WaitIdle(e2eTimeout)
		if el := time.Since(began); el > 10*time.Second {
			f.Fatalf("the task took %v to stop", el)
		}
		draft, err := os.ReadFile(draftPath)
		if err != nil || !strings.Contains(string(draft), "做一件会被打断的活") {
			f.Fatalf("no interrupted draft after Ctrl+C: %v %s", err, draft)
		}
		f.Absent("这条回答不会出现")

		s.Type("继续")
		s.WaitFor("第二步也完成了", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		reqs := f.Requests(3)
		last := reqs[2]
		if n := countMessages(last, "user", "做一件会被打断的活"); n != 1 {
			f.Fatalf("the resumed request carries the original request %d times, want once: %q", n, requestTexts(last))
		}
		if n := countMessages(last, "tool", "ctrlc_step1"); n != 1 {
			f.Fatalf("the finished tool step appears %d times in the resumed request, want once (not redone)", n)
		}
		if n := strings.Count(s.Output(), "bash echo ctrlc_step1  实时输出"); n > 1 {
			f.Fatalf("the finished tool step ran %d times", n)
		}
		if _, err := os.Stat(draftPath); err == nil {
			f.Fatalf("the draft is still there after the turn completed")
		}
	})

	t.Run("授权提示中打字不被吞掉作为指引且提示仍等待", func(t *testing.T) {
		s, f := start(t,
			toolReply(bashCall("mkdir f2_out")),
			textReply("输出目录 f2_out 已经建好，并按指引使用小写，创建完成。"))
		s.Type("建一个输出目录")
		s.WaitFor("需要授权", e2eTimeout)
		s.Type("目录名用小写")
		s.WaitFor("提示仍在等待回答", e2eTimeout)
		s.WaitFor("[已插入]", e2eTimeout)
		if _, err := os.Stat(filepath.Join(project, "f2_out")); err == nil {
			f.Fatalf("the typed line was taken as the approval")
		}
		f.Absent("已拒绝 bash")
		if n := len(model.Requests()); n != 1 {
			f.Fatalf("model called %d times while the prompt waited", n)
		}
		s.Type("y")
		s.WaitFor("创建完成。", e2eTimeout)
		if _, err := os.Stat(filepath.Join(project, "f2_out")); err != nil {
			f.Fatalf("f2_out not created after y: %v", err)
		}
		reqs := f.Requests(2)
		if countMessages(reqs[1], "tool", "") != 1 || countMessages(reqs[1], "user", "[用户指引] 目录名用小写") != 1 {
			f.Fatalf("second request lacks the tool result or the guidance: %q", requestTexts(reqs[1]))
		}
		if n := strings.Count(s.Output(), "需要授权"); n != 1 {
			f.Fatalf("asked %d times, want once", n)
		}
	})

	t.Run("任务中/history编号、/compact、/base-url、/restart被拒并提示", func(t *testing.T) {
		s, f := start(t, fakeStep{Delay: 30 * time.Second, Content: "这条回答不会出现"})
		t.Cleanup(func() { _ = os.Remove(draftPath) })
		cfgBefore, _ := os.ReadFile(filepath.Join(cfgDir, "config.json"))
		id := s.app.eng.SessionID()
		s.Type("做一件很慢的活")
		model.WaitRequests(t, 1, e2eTimeout)
		for _, cmd := range []string{"/history 1", "/compact", "/base-url http://127.0.0.1:1/v1", "/restart"} {
			name := strings.Fields(cmd)[0]
			s.Type(cmd)
			s.WaitFor("[提示] 任务运行中不能执行 "+name, e2eTimeout)
		}
		select {
		case <-s.done:
			f.Fatalf("/restart ended the REPL during the task")
		default:
		}
		if cfgAfter, _ := os.ReadFile(filepath.Join(cfgDir, "config.json")); string(cfgAfter) != string(cfgBefore) {
			f.Fatalf("/base-url changed config.json during the task:\n%s", cfgAfter)
		}
		if got := s.app.eng.SessionID(); got != id {
			f.Fatalf("the session changed during the task: %s -> %s", id, got)
		}
		s.Interrupt()
		s.WaitFor(interruptNote, e2eTimeout)
		s.WaitIdle(e2eTimeout)
		f.Requests(1)
	})

	t.Run("任务中/tasks、/cost和/history列表允许", func(t *testing.T) {
		s, f := start(t, fakeStep{Delay: 30 * time.Second, Content: "这条回答不会出现"})
		t.Cleanup(func() { _ = os.Remove(draftPath) })
		s.Type("再做一件很慢的活")
		model.WaitRequests(t, 1, e2eTimeout)
		mark := s.Mark()
		s.Type("/tasks")
		s.WaitForSince(mark, "当前任务 (已运行", e2eTimeout)
		s.WaitForSince(mark, "再做一件很慢的活", e2eTimeout)
		mark = s.Mark()
		s.Type("/cost")
		s.WaitForSince(mark, "$", e2eTimeout)
		mark = s.Mark()
		// The listing only reads; picking a number resumes and is refused
		// (previous subtest).
		s.Type("/history")
		s.WaitForSince(mark, "历史记录", e2eTimeout)
		if strings.Contains(s.Output(), "任务运行中不能执行") {
			f.Fatalf("an allowed command was refused during the task")
		}
		s.Interrupt()
		s.WaitFor(interruptNote, e2eTimeout)
		s.WaitIdle(e2eTimeout)
		f.Requests(1)
	})
}
