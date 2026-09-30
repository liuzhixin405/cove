package main

// Flow F1 (test design 5.3): one request's life — input, permission, tool
// run, the end of the turn, saving and leaving. One home, one fake model and
// one git project are shared; each subtest scripts the model afresh, starts
// its own REPL and checks the terminal, the disk and what the model received.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/session"
)

// f1DenyRm is the pre-seeded policies.json: rm is denied in every project.
const f1DenyRm = `[
  {"id": "deny-bash-rm", "tool_pattern": "bash", "action": "deny", "enabled": true, "command_prefix": "rm"}
]`

func bashCall(cmd string) fakeToolCall {
	b, _ := json.Marshal(map[string]any{"command": cmd})
	return fakeToolCall{Name: "bash", Args: string(b)}
}

func bashCallTimeout(cmd string, ms int) fakeToolCall {
	b, _ := json.Marshal(map[string]any{"command": cmd, "timeout": ms})
	return fakeToolCall{Name: "bash", Args: string(b)}
}

func toolReply(calls ...fakeToolCall) fakeStep { return fakeStep{ToolCalls: calls} }

func textReply(s string) fakeStep { return fakeStep{Content: s} }

func TestFlowF1_RequestLifecycle(t *testing.T) {
	model := newFakeModel(t)
	home, project := e2eHomeWith(t, model, e2eHomeOptions{Policies: f1DenyRm, GitInit: true})
	cfgDir := filepath.Join(home, ".cove")
	writeTestFile(t, filepath.Join(project, "notes.txt"), "第一条要点：保持接口稳定\n")
	policiesPath := filepath.Join(cfgDir, "policies.json")

	// start scripts the model and starts a REPL for one subtest.
	start := func(t *testing.T, steps ...fakeStep) (*e2eSession, flow) {
		t.Helper()
		model.Reset(steps...)
		s := startREPL(t)
		return s, flow{t: t, s: s, model: model}
	}
	// toolResult is the text of the tool message in req (the first one).
	toolResult := func(req capturedRequest) string {
		for _, m := range req.Messages {
			if m.Role == "tool" {
				return m.Text()
			}
		}
		return ""
	}

	// ---------- 输入 ----------

	t.Run("输入普通一行原样发给模型", func(t *testing.T) {
		s, f := start(t, textReply("这个项目是一个示例工程，目前只有说明文件和笔记，介绍完成。"))
		s.Type("用一句话介绍这个项目")
		s.WaitFor("介绍完成。", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		reqs := f.Requests(1)
		// The engine appends its <environment> note (git state) to the text.
		if last := lastMessage(reqs[0]); last.Role != "user" || !strings.HasPrefix(last.Text(), "用一句话介绍这个项目") {
			f.Fatalf("last message = %s %q, want the typed line as the user message", last.Role, last.Text())
		}
		rec := sessionFile(t, home, s.app.eng.SessionID())
		if countMessages(capturedFromRecord(rec), "user", "用一句话介绍这个项目") != 1 ||
			countMessages(capturedFromRecord(rec), "assistant", "介绍完成。") != 1 {
			f.Fatalf("session file lacks the exchange: %+v", rec.Messages)
		}
	})

	t.Run("输入@附件存在时内容随消息发送", func(t *testing.T) {
		s, f := start(t, textReply("笔记里的要点是保持接口稳定，总结完成。"))
		s.Type("总结 @notes.txt 的要点")
		s.WaitFor("总结完成。", e2eTimeout)
		f.Absent("不是已存在的文件")
		req := f.Requests(1)[0]
		if countMessages(req, "user", "附件 notes.txt") != 1 || countMessages(req, "user", "保持接口稳定") != 1 {
			f.Fatalf("the attachment did not reach the model once: %q", requestTexts(req))
		}
		if countMessages(req, "user", "@notes.txt") != 0 {
			f.Fatalf("the @token stayed in the text: %q", requestTexts(req))
		}
	})

	t.Run("输入@附件不存在时警告且仍发送", func(t *testing.T) {
		s, f := start(t, textReply("没有找到这个日志文件，请确认路径，检查完成。"))
		s.Type("看看 @missing/app.log 里写了什么")
		s.WaitFor("⚠ @missing/app.log 不是已存在的文件", e2eTimeout)
		s.WaitFor("检查完成。", e2eTimeout)
		last := lastMessage(f.Requests(1)[0])
		if last.Role != "user" || !strings.Contains(last.Text(), "@missing/app.log") {
			f.Fatalf("the message was not sent as text: %s %q", last.Role, last.Text())
		}
	})

	t.Run("输入多行粘贴作为一条消息", func(t *testing.T) {
		// The harness drives the plain reader (stdin is a pipe, not a
		// console), which reads one line per Enter by design; the paste
		// heuristics live in the raw-mode editor and are covered by
		// internal/repl (TestBracketedPasteIsOneMessage,
		// TestUnbracketedPasteIsOneMessage).
		t.Skip("harness: the plain reader has no paste mode; see internal/repl paste tests")
	})

	// ---------- 权限 ----------

	t.Run("权限只读命令自动放行", func(t *testing.T) {
		s, f := start(t, toolReply(bashCall("cat notes.txt")), textReply("读到了笔记，第一条要点是保持接口稳定，读取完成。"))
		s.Type("读一下笔记文件")
		s.WaitFor("读取完成。", e2eTimeout)
		f.Absent("需要授权")
		if got := toolResult(f.Requests(2)[1]); !strings.Contains(got, "保持接口稳定") {
			f.Fatalf("tool result lacks the file: %q", got)
		}
	})

	t.Run("权限y只允许这一次", func(t *testing.T) {
		before, _ := os.ReadFile(policiesPath)
		s, f := start(t,
			toolReply(bashCall("mkdir y_once")),
			toolReply(bashCall("mkdir y_twice")),
			textReply("两个目录 y_once 和 y_twice 都已创建完成。"))
		s.Type("建两个目录，每次都问我")
		s.WaitFor("需要授权", e2eTimeout)
		s.Type("y")
		s.WaitForCount("需要授权", 2, e2eTimeout)
		s.Type("y")
		s.WaitFor("都已创建完成。", e2eTimeout)
		for _, d := range []string{"y_once", "y_twice"} {
			if _, err := os.Stat(filepath.Join(project, d)); err != nil {
				f.Errorf("%s not created: %v", d, err)
			}
		}
		if after, _ := os.ReadFile(policiesPath); string(after) != string(before) {
			f.Fatalf("[y] changed policies.json:\n%s", after)
		}
		f.Requests(3)
	})

	t.Run("权限a本会话内不再询问且退出后失效", func(t *testing.T) {
		if testing.Short() {
			t.Skip("slow (>3s): skipped under -short")
		}
		before, _ := os.ReadFile(policiesPath)
		s, f := start(t,
			toolReply(bashCall("touch a_1.txt")),
			toolReply(bashCall("touch a_2.txt")),
			textReply("a_1.txt 和 a_2.txt 两个文件都已创建完成。"))
		s.Type("建两个空文件")
		s.WaitFor("需要授权", e2eTimeout)
		s.Type("a")
		s.WaitFor("已记住 bash 中", e2eTimeout)
		s.WaitFor("都已创建完成。", e2eTimeout)
		if n := strings.Count(s.Output(), "需要授权"); n != 1 {
			f.Fatalf("asked %d times, want once after [a]", n)
		}
		if after, _ := os.ReadFile(policiesPath); string(after) != string(before) {
			f.Fatalf("[a] wrote policies.json:\n%s", after)
		}
		s.Exit()

		model.Reset(toolReply(bashCall("touch a_3.txt")), textReply("a_3.txt 已创建完成。"))
		s2 := startREPL(t)
		f2 := flow{t: t, s: s2, model: model}
		s2.Type("再建一个空文件")
		s2.WaitFor("需要授权", e2eTimeout)
		s2.Type("y")
		s2.WaitFor("a_3.txt 已创建完成。", e2eTimeout)
		f2.Requests(2)
	})

	t.Run("权限p写入policies.json且重启后仍生效", func(t *testing.T) {
		if testing.Short() {
			t.Skip("slow (>3s): skipped under -short")
		}
		s, f := start(t, toolReply(bashCall("cp notes.txt p_1.txt")), textReply("已复制为 p_1.txt，复制完成。"))
		s.Type("复制一份笔记")
		s.WaitFor("需要授权", e2eTimeout)
		s.Type("p")
		s.WaitFor("已写入", e2eTimeout)
		s.WaitFor("复制完成。", e2eTimeout)
		raw, err := os.ReadFile(policiesPath)
		if err != nil {
			f.Fatalf("policies.json: %v", err)
		}
		var rules []map[string]any
		if err := json.Unmarshal(raw, &rules); err != nil {
			f.Fatalf("policies.json is not a rule array: %v\n%s", err, raw)
		}
		var sawCp, sawDeny bool
		for _, r := range rules {
			sawCp = sawCp || (r["command_prefix"] == "cp" && r["action"] == "allow")
			sawDeny = sawDeny || r["id"] == "deny-bash-rm"
		}
		if !sawCp || !sawDeny {
			f.Fatalf("policies.json lacks the cp allow (%v) or lost the seeded deny (%v):\n%s", sawCp, sawDeny, raw)
		}
		s.Exit()

		model.Reset(toolReply(bashCall("cp notes.txt p_2.txt")), textReply("已复制为 p_2.txt，复制完成。"))
		s2 := startREPL(t)
		f2 := flow{t: t, s: s2, model: model}
		s2.Type("再复制一份笔记")
		s2.WaitFor("p_2.txt，复制完成。", e2eTimeout)
		f2.Absent("需要授权")
		if _, err := os.Stat(filepath.Join(project, "p_2.txt")); err != nil {
			f2.Fatalf("p_2.txt not created: %v", err)
		}
		f2.Requests(2)
	})

	t.Run("权限n拒绝后模型收到拒绝文本", func(t *testing.T) {
		s, f := start(t, toolReply(bashCall("mkdir n_dir")), textReply("好的，不建 n_dir 目录，改为只列出方案，处理完成。"))
		s.Type("建一个 n_dir 目录")
		s.WaitFor("需要授权", e2eTimeout)
		s.Type("n")
		s.WaitFor("已拒绝 bash", e2eTimeout)
		s.WaitFor("处理完成。", e2eTimeout)
		if _, err := os.Stat(filepath.Join(project, "n_dir")); err == nil {
			f.Fatalf("n_dir was created after [n]")
		}
		if got := toolResult(f.Requests(2)[1]); !strings.Contains(got, "user rejected") {
			f.Fatalf("the model was not told the call was refused: %q", got)
		}
	})

	t.Run("权限deny规则命中直接拒绝不询问", func(t *testing.T) {
		s, f := start(t, toolReply(bashCall("rm -f notes.txt")), textReply("删除被策略拒绝了，笔记文件保留，处理完成。"))
		s.Type("删掉笔记文件")
		s.WaitFor("处理完成。", e2eTimeout)
		f.Absent("需要授权")
		if _, err := os.Stat(filepath.Join(project, "notes.txt")); err != nil {
			f.Fatalf("notes.txt removed despite the deny rule: %v", err)
		}
		if got := toolResult(f.Requests(2)[1]); !strings.Contains(got, "denied by policy for bash") {
			f.Fatalf("tool result is not the policy denial: %q", got)
		}
	})

	t.Run("权限plan模式下写工具被拒且不询问", func(t *testing.T) {
		s, f := start(t,
			toolReply(fakeToolCall{Name: "write", Args: `{"filePath":"plan.txt","content":"x\n"}`}),
			textReply("计划模式下不能写文件，我先给出计划，规划完成。"))
		s.Type("/mode plan")
		s.WaitFor("plan", e2eTimeout)
		t.Cleanup(func() {
			// /mode writes config.json; later subtests run in default mode.
			s3 := startREPL(t)
			s3.Type("/mode default")
			s3.WaitFor("default", e2eTimeout)
			s3.Exit()
		})
		s.Type("写一个 plan.txt")
		s.WaitFor("规划完成。", e2eTimeout)
		f.Absent("需要授权")
		if _, err := os.Stat(filepath.Join(project, "plan.txt")); err == nil {
			f.Fatalf("plan.txt written in plan mode")
		}
		if got := toolResult(f.Requests(2)[1]); !strings.Contains(got, "plan mode") {
			f.Fatalf("tool result does not name plan mode: %q", got)
		}
		raw, _ := os.ReadFile(filepath.Join(cfgDir, "config.json"))
		if !strings.Contains(string(raw), `"permission_mode": "plan"`) {
			f.Fatalf("/mode plan not saved to config.json:\n%s", raw)
		}
	})

	// ---------- 工具 ----------

	t.Run("工具成功结果回到模型", func(t *testing.T) {
		s, f := start(t, toolReply(bashCall("echo tool_ok_marker")), textReply("命令输出了 tool_ok_marker，执行完成。"))
		s.Type("跑一条 echo")
		s.WaitFor("执行完成。", e2eTimeout)
		got := toolResult(f.Requests(2)[1])
		if !strings.Contains(got, "tool_ok_marker") || strings.Contains(got, "exit code") {
			f.Fatalf("tool result = %q, want the output and no failure", got)
		}
	})

	t.Run("工具非零退出码标为错误", func(t *testing.T) {
		s, f := start(t, toolReply(bashCall("ls no_such_dir_f1")), textReply("目录不存在，命令失败了，检查完成。"))
		s.Type("列出一个不存在的目录")
		s.WaitFor("检查完成。", e2eTimeout)
		if got := toolResult(f.Requests(2)[1]); !strings.Contains(got, "exit code") {
			f.Fatalf("tool result does not report the exit code: %q", got)
		}
	})

	t.Run("工具超时后进程被杀并说明", func(t *testing.T) {
		if testing.Short() {
			t.Skip("slow (>3s): skipped under -short")
		}
		s, f := start(t, toolReply(bashCallTimeout("echo started_f1; sleep 20", 800)), textReply("命令超时被终止了，检查完成。"))
		began := time.Now()
		s.Type("跑一条很慢的命令")
		s.WaitFor("需要授权", e2eTimeout)
		s.Type("y")
		s.WaitFor("检查完成。", e2eTimeout)
		if el := time.Since(began); el > 15*time.Second {
			f.Fatalf("the turn took %v: the timed-out process was not killed", el)
		}
		got := toolResult(f.Requests(2)[1])
		if !strings.Contains(got, "[timed out after 0.8s]") || !strings.Contains(got, "started_f1") {
			f.Fatalf("tool result = %q, want the output and the timeout marker", got)
		}
	})

	t.Run("工具后台进程不阻塞收尾", func(t *testing.T) {
		if testing.Short() {
			t.Skip("slow (>3s): skipped under -short")
		}
		// The background sleep runs from / so it holds no handle on the
		// temporary project directory after the test. Under Git Bash the
		// child still inherits the output pipe despite the redirections, so
		// the tool waits its 5 s WaitDelay and says so ("a background process
		// it started kept the output open"); elsewhere it returns at once.
		// Either way the turn ends long before the 20 s sleep does.
		s, f := start(t,
			toolReply(bashCall("(cd / && sleep 20 </dev/null >/dev/null 2>&1 &) ; echo bg_started")),
			textReply("后台进程已经启动，本轮结束，启动完成。"))
		began := time.Now()
		s.Type("在后台起一个进程")
		s.WaitFor("需要授权", e2eTimeout)
		s.Type("y")
		s.WaitFor("启动完成。", e2eTimeout)
		if el := time.Since(began); el > 12*time.Second {
			f.Fatalf("the turn waited %v for the background process", el)
		}
		if got := toolResult(f.Requests(2)[1]); !strings.Contains(got, "bg_started") {
			f.Fatalf("tool result = %q", got)
		}
	})

	// ---------- 收尾 ----------

	t.Run("收尾最终文本过短提醒一次", func(t *testing.T) {
		s, f := start(t,
			toolReply(bashCall("mkdir short_dir")),
			textReply("好了"),
			textReply("已完成：创建了 short_dir 目录，没有其他改动，也没有遗留事项。"))
		s.Type("建一个 short_dir 目录")
		s.WaitFor("需要授权", e2eTimeout)
		s.Type("y")
		s.WaitFor("没有遗留事项。", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		reqs := f.Requests(3)
		if last := lastMessage(reqs[2]); last.Role != "user" || !strings.Contains(last.Text(), "too brief") {
			f.Fatalf("third request does not carry the brief-ending reminder: %s %q", last.Role, last.Text())
		}
		if n := countMessages(reqs[2], "", "too brief"); n != 1 {
			f.Fatalf("reminder sent %d times, want once", n)
		}
	})

	t.Run("收尾todo未完成提示不覆盖已有答复", func(t *testing.T) {
		todos := func(status string) fakeToolCall {
			return fakeToolCall{Name: "todowrite", Args: `{"todos":[{"content":"整理目录","status":"` + status + `","priority":"high"},{"content":"补充说明","status":"` + status + `","priority":"low"}]}`}
		}
		s, f := start(t,
			toolReply(todos("pending")),
			textReply("第一版报告：目录已整理，说明也已补充，两项工作都完成了。"),
			toolReply(todos("completed")),
			textReply("最终报告：两项都已完成并在列表里勾掉，结论与第一版一致。"))
		s.Type("整理目录并补充说明")
		s.WaitFor("最终报告：", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		out := s.Output()
		if !strings.Contains(out, "第一版报告：") {
			f.Fatalf("the first answer is gone from the terminal")
		}
		reqs := f.Requests(4)
		if last := lastMessage(reqs[2]); !strings.Contains(last.Text(), "still open") {
			f.Fatalf("third request lacks the open-todo reminder: %q", last.Text())
		}
		if n := countMessages(reqs[3], "", "still open"); n != 1 {
			f.Fatalf("open-todo reminder sent %d times, want once", n)
		}
		rec := capturedFromRecord(sessionFile(t, home, s.app.eng.SessionID()))
		if countMessages(rec, "assistant", "第一版报告：") != 1 || countMessages(rec, "assistant", "最终报告：") != 1 {
			f.Fatalf("session file does not keep both answers")
		}
	})

	t.Run("收尾改文件后出现git状态行只读回合不出现", func(t *testing.T) {
		s, f := start(t,
			toolReply(fakeToolCall{Name: "write", Args: `{"filePath":"changed.txt","content":"x\n"}`}),
			textReply("已创建 changed.txt 文件，写入完成，尚未提交。"))
		s.Type("写一个 changed.txt")
		s.WaitFor("需要授权", e2eTimeout)
		s.Type("y")
		s.WaitFor("尚未提交。", e2eTimeout)
		s.WaitFor("git：", e2eTimeout)
		s.WaitFor("未提交的改动", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		f.Requests(2)

		model.Reset(toolReply(bashCall("cat README.md")), textReply("README 只有一个标题，阅读完成。"))
		mark := s.Mark()
		s.Type("看看 README 写了什么")
		s.WaitForSince(mark, "阅读完成。", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		if strings.Contains(s.OutputSince(mark), "git：") {
			f.Fatalf("a read-only turn printed the git status line:\n%s", s.OutputSince(mark))
		}
	})

	// ---------- 保存与退出 ----------

	t.Run("保存/exit后会话文件完整可-r恢复", func(t *testing.T) {
		s, f := start(t, textReply("恢复测试的第一轮回答，关于配置加载，回答完成。"))
		s.Type("讲讲配置加载的流程")
		s.WaitFor("回答完成。", e2eTimeout)
		id := s.app.eng.SessionID()
		s.Type("/exit")
		s.WaitExited(e2eTimeout)
		s.WaitFor("再见！", e2eTimeout)
		rec := capturedFromRecord(sessionFile(t, home, id))
		if countMessages(rec, "user", "讲讲配置加载的流程") != 1 || countMessages(rec, "assistant", "关于配置加载") != 1 {
			f.Fatalf("session file incomplete after /exit")
		}
		s.Exit()

		model.Reset(textReply("接着上次的配置加载话题继续，补充完成。"))
		s2 := startREPLResumed(t, id)
		f2 := flow{t: t, s: s2, model: model}
		s2.Type("那缓存呢")
		s2.WaitFor("补充完成。", e2eTimeout)
		req := f2.Requests(1)[0]
		if countMessages(req, "user", "讲讲配置加载的流程") != 1 || countMessages(req, "assistant", "关于配置加载") != 1 {
			f2.Fatalf("the resumed request does not carry the old conversation")
		}
		if s2.app.eng.SessionID() != id {
			f2.Fatalf("-r continued session %s, want %s", s2.app.eng.SessionID(), id)
		}
	})

	t.Run("保存Ctrl+D等同/exit", func(t *testing.T) {
		s, f := start(t, textReply("关于 Ctrl+D 的这一轮回答，回答完成。"))
		s.Type("随便问一个关于退出的问题")
		s.WaitFor("回答完成。", e2eTimeout)
		id := s.app.eng.SessionID()
		s.CloseStdin()
		s.WaitExited(e2eTimeout)
		s.WaitFor("再见！", e2eTimeout)
		rec := capturedFromRecord(sessionFile(t, home, id))
		if countMessages(rec, "user", "随便问一个关于退出的问题") != 1 || countMessages(rec, "assistant", "关于 Ctrl+D") != 1 {
			f.Fatalf("session file incomplete after Ctrl+D")
		}
		f.Requests(1)
	})

	t.Run("保存/restart保存后新进程接续", func(t *testing.T) {
		s, f := start(t, textReply("重启前的回答，讲的是日志模块，回答完成。"))
		s.Type("讲讲日志模块")
		s.WaitFor("回答完成。", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		id := s.app.eng.SessionID()
		s.Type("/restart")
		s.WaitExited(e2eTimeout)
		s.WaitFor("正在重启 cove", e2eTimeout)
		if !s.Restarted() {
			f.Fatalf("/restart did not ask main for a restart")
		}
		if got := restartArgs([]string{"--no-auto"}, id); strings.Join(got, " ") != "--no-auto -r "+id {
			f.Fatalf("restart args = %q", got)
		}
		rec := capturedFromRecord(sessionFile(t, home, id))
		if countMessages(rec, "assistant", "日志模块") != 1 {
			f.Fatalf("session not saved before the restart")
		}
		s.Exit()

		// The new process is started with -r <id> (restartArgs).
		model.Reset(textReply("重启后接着日志模块继续，补充完成。"))
		s2 := startREPLResumed(t, id)
		f2 := flow{t: t, s: s2, model: model}
		s2.Type("日志怎么轮转")
		s2.WaitFor("补充完成。", e2eTimeout)
		if req := f2.Requests(1)[0]; countMessages(req, "user", "讲讲日志模块") != 1 {
			f2.Fatalf("the restarted session does not carry the conversation")
		}
	})

	t.Run("保存任务panic后可继续", func(t *testing.T) {
		s, f := start(t,
			fakeStep{Delay: 800 * time.Millisecond, ToolCalls: []fakeToolCall{bashCall("echo before_panic")}},
			textReply("从内部异常中恢复，任务已经继续完成。"))
		// A panic on the task goroutine: the hook the engine calls when it
		// hands steered guidance to the model. The REPL's recover in
		// replTaskRunner.run is what is under test. runREPL installed the
		// hook before the first prompt; the typed line below orders this
		// write before the task reads it.
		s.WaitFor("❯", e2eTimeout)
		orig := s.app.eng.OnSteerConsumed
		panicked := false
		s.app.eng.OnSteerConsumed = func() {
			if !panicked {
				panicked = true
				panic("injected by the flow test")
			}
			if orig != nil {
				orig()
			}
		}
		s.Type("做一件会出异常的事")
		model.WaitRequests(t, 1, e2eTimeout)
		s.Type("顺便看看日志")
		s.WaitFor("[已插入]", e2eTimeout)
		s.WaitFor("任务执行出现内部异常", e2eTimeout)
		draft, err := os.ReadFile(filepath.Join(cfgDir, "interrupted.json"))
		if err != nil || !strings.Contains(string(draft), "做一件会出异常的事") {
			f.Fatalf("no interrupted draft after the panic: %v %s", err, draft)
		}
		s.Type("继续")
		s.WaitFor("任务已经继续完成。", e2eTimeout)
		reqs := f.Requests(2)
		if n := countMessages(reqs[1], "user", "做一件会出异常的事"); n != 1 {
			f.Fatalf("the resumed request carries the original request %d times, want once", n)
		}
	})
}

// capturedFromRecord turns a saved session's messages into a
// capturedRequest, so countMessages works on the disk state too.
func capturedFromRecord(rec *session.Record) capturedRequest {
	var req capturedRequest
	for _, m := range rec.Messages {
		b, _ := json.Marshal(m.Content)
		req.Messages = append(req.Messages, capturedMessage{Role: m.Role, Content: b, ToolCallID: m.ToolCallID})
	}
	return req
}
