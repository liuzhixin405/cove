package main

// Flow F7 (test design 5.3): background learning next to the foreground —
// memory extraction at the end of a turn, /memory add while an extraction
// is in flight, the session-end dream worker (one process, and two cove
// processes exiting together), the skill review racing /new, and --no-auto.
//
// The fake model is a learningModel: it listens on 127.0.0.2 (the engine
// skips background learning for a local server such as 127.0.0.1) and keeps
// separate scripts for turns, extraction, review and consolidation. dream.json
// lowers min_turns to 1. The dream worker is real: a TestCoveMainHelperProcess
// child running `cove --dream-worker` (helperDreamSpawn), started by the
// in-process exit path or, in child coves, via COVE_E2E_DREAM_SPAWN=1.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/dream"
	"github.com/liuzhixin405/cove-agent/internal/hooks"
	"github.com/liuzhixin405/cove-agent/internal/memory"
)

func TestFlowF7_BackgroundLearning(t *testing.T) {
	lm := newLearningModel(t)
	home, project := e2eHomeWith(t, lm.fg, e2eHomeOptions{Config: map[string]any{"provider": lm.Provider()}})
	cfgDir := filepath.Join(home, ".cove")
	writeTestFile(t, filepath.Join(cfgDir, "dream.json"), `{"enabled": true, "min_turns": 1}`)
	dataDir, err := config.ProjectDataPath(memory.ProjectRoot(project))
	if err != nil {
		t.Fatal(err)
	}
	projMem := filepath.Join(dataDir, "memory")
	lockFile := filepath.Join(cfgDir, "memory", ".consolidate-lock")
	lastRunFile := filepath.Join(cfgDir, "dream-last.json")
	dreamLog := filepath.Join(cfgDir, "dream.log")

	// start runs a REPL with background learning on (startREPL turns it
	// off); learning is switched on before the first line is typed.
	start := func(t *testing.T, learn bool, steps ...fakeStep) (*e2eSession, flow) {
		t.Helper()
		lm.fg.Reset(steps...)
		s := startREPL(t)
		if learn {
			s.app.eng.SetAutoExtract(true)
		}
		return s, flow{t: t, s: s, model: lm.fg}
	}
	// turn types one line and waits for its answer and the idle prompt.
	turn := func(s *e2eSession, line, answer string) {
		s.t.Helper()
		s.Type(line)
		s.WaitFor(answer, e2eTimeout)
		s.WaitIdle(e2eTimeout)
	}
	readFile := func(path string) string {
		b, _ := os.ReadFile(path)
		return string(b)
	}
	lastRun := func(t *testing.T) dream.LastRun {
		t.Helper()
		var lr dream.LastRun
		if err := json.Unmarshal([]byte(readFile(lastRunFile)), &lr); err != nil {
			t.Fatalf("dream-last.json: %v\n%s", err, readFile(lastRunFile))
		}
		return lr
	}
	// workersFinished counts the dream workers that have ended (each prints
	// one of these lines to dream.log).
	workersFinished := func() int {
		log := readFile(dreamLog)
		return strings.Count(log, "dream worker done in") + strings.Count(log, "dream worker failed after")
	}
	extraction := func(entries ...[2]string) fakeStep {
		var sb strings.Builder
		for _, e := range entries {
			sb.WriteString("---MEMORY---\nFILE: " + e[0] + "\nMODE: write\nCONTENT:\n" + e[1] + "\n---END---\n")
		}
		return textReply(sb.String())
	}

	// ---------- memory extraction ----------

	t.Run("回合结束提取记忆且不覆盖已有记忆", func(t *testing.T) {
		writeTestFile(t, filepath.Join(projMem, "f7-arch.md"), "旧事实：接口保持稳定")
		lm.extract.Reset(extraction(
			[2]string{"f7-arch.md", "新事实：构建用 make"},
			[2]string{"f7-new-fact.md", "用户偏好：缩进用 tab"}))
		s, f := start(t, true,
			textReply("第一个问题的回答：接口需要保持稳定，这一点已经确认。"),
			textReply("第二个问题的回答：构建统一用 make，回答完毕。"))
		turn(s, "项目接口有什么约定", "这一点已经确认")
		turn(s, "构建用什么工具", "构建统一用 make")
		waitUntil(t, e2eTimeout, "f7-new-fact.md", func() bool {
			return strings.Contains(readFile(filepath.Join(projMem, "f7-new-fact.md")), "缩进用 tab")
		})
		s.TypeUntil("/memory stats", "，保存 2 条", e2eTimeout)
		arch := readFile(filepath.Join(projMem, "f7-arch.md"))
		if !strings.Contains(arch, "旧事实：接口保持稳定") || !strings.Contains(arch, "新事实：构建用 make") {
			f.Fatalf("f7-arch.md = %q, want the old fact kept and the new one appended", arch)
		}
		f.Requests(2)
		ex := lm.extract.Requests()
		if len(ex) != 1 {
			f.Fatalf("extraction ran %d times, want once (the first turn is too short)", len(ex))
		}
		if countMessages(ex[0], "user", "f7-arch.md") != 1 || countMessages(ex[0], "user", "构建用什么工具") != 1 {
			f.Fatalf("the extraction prompt lacks the existing memory or the conversation: %q", requestTexts(ex[0]))
		}
		s.Exit()
	})

	t.Run("提取进行中/memory add同名记忆两者都保留", func(t *testing.T) {
		lm.extract.Reset(extraction([2]string{"f7-shared.md", "提取：部署走 CI"}))
		arrived, release := lm.Hold("extract")
		defer release()
		s, f := start(t, true,
			textReply("部署的回答：统一走 CI 流水线，回答完毕。"),
			textReply("发布的回答：发布前要跑 e2e，回答完毕。"))
		turn(s, "怎么部署", "统一走 CI 流水线")
		turn(s, "发布前做什么", "发布前要跑 e2e")
		waitArrived(t, arrived, "the extraction request")
		mark := s.Mark()
		s.Type("/memory add f7-shared.md 手写：发布前跑 e2e")
		s.WaitForSince(mark, "记忆 'f7-shared.md' 已保存", e2eTimeout)
		release()
		path := filepath.Join(projMem, "f7-shared.md")
		waitUntil(t, e2eTimeout, "the extracted fact in f7-shared.md", func() bool {
			return strings.Contains(readFile(path), "提取：部署走 CI")
		})
		if got := readFile(path); !strings.Contains(got, "手写：发布前跑 e2e") {
			f.Fatalf("f7-shared.md = %q, the /memory add content is gone", got)
		}
		s.TypeUntil("/memory stats", "，保存 1 条", e2eTimeout)
		f.Requests(2)
		if n := len(lm.extract.Requests()); n != 1 {
			f.Fatalf("extraction ran %d times, want 1", n)
		}
		s.Exit()
	})

	// ---------- dream ----------

	var mu sync.Mutex
	var notices strings.Builder
	useRealDream := func(t *testing.T) {
		t.Helper()
		resetSessionEnd(t, hooks.NewManager())
		resetTurnsCompleted()
		oldSpawn, oldNotice := dreamSpawn, dreamNotice
		dreamSpawn = helperDreamSpawn
		dreamNotice = func(s string) { mu.Lock(); notices.WriteString(s); mu.Unlock() }
		t.Cleanup(func() { dreamSpawn, dreamNotice = oldSpawn, oldNotice; resetTurnsCompleted() })
	}

	t.Run("退出时启动dream后台进程", func(t *testing.T) {
		useRealDream(t)
		lm.dream.Reset(textReply("整理完成：没有需要合并或删除的记忆。"))
		s, f := start(t, false, textReply("这一轮的回答：一切正常，回答完毕。"))
		turn(s, "随便问一句 marker-f7-dream", "一切正常")
		id := s.app.eng.SessionID()
		before := workersFinished()
		s.Exit()
		mu.Lock()
		notice := notices.String()
		mu.Unlock()
		if !strings.Contains(notice, "已在后台启动记忆整理") {
			f.Fatalf("exit did not announce the dream worker: %q", notice)
		}
		waitUntil(t, e2eTimeout, "the dream worker to finish", func() bool { return workersFinished() > before })
		lr := lastRun(t)
		if lr.Mode != "worker" || lr.Result != dream.ResultCompleted || lr.SessionsReviewed < 1 {
			f.Fatalf("dream-last.json = %+v, want a completed worker run\ndream.log:\n%s", lr, readFile(dreamLog))
		}
		if _, err := os.Stat(lockFile); err != nil {
			f.Fatalf("no consolidation lock after the run: %v", err)
		}
		dr := lm.dream.Requests()
		if len(dr) != 1 || countMessages(dr[0], "user", id) != 1 {
			f.Fatalf("the consolidation made %d requests; want 1 naming session %s", len(dr), id)
		}
		f.Requests(1)

		s2, _ := start(t, false)
		mark := s2.Mark()
		s2.Type("/dream")
		s2.WaitForSince(mark, "上次会话结束整理: 后台整理完成：回顾", e2eTimeout)
		s2.Exit()
	})

	t.Run("两个cove同时退出只跑一个dream", func(t *testing.T) {
		lm.fg.Reset()
		lm.dream.Reset(textReply("整理完成：两个会话都看过了，没有改动。"))
		arrived, release := lm.Hold("dream")
		defer release()
		before := workersFinished()
		logBefore := readFile(dreamLog)
		env := []string{coveSpawnEnv + "=1"}
		var wg sync.WaitGroup
		runs := make([]coveRun, 2)
		errs := make([]error, 2)
		for i := range runs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				runs[i], errs[i] = tryRunCove(t, strings.NewReader(fmt.Sprintf("同时退出的第 %d 个 cove\n", i+1)), env, "--no-tui")
			}(i)
		}
		wg.Wait()
		f := flow{t: t, model: lm.fg}
		for i, err := range errs {
			if err != nil {
				f.Fatalf("cove #%d: %v", i+1, err)
			}
		}
		spawned := 0
		for i, r := range runs {
			if r.code != 0 {
				f.Fatalf("cove #%d: code %d, stderr:\n%s", i+1, r.code, r.stderr)
			}
			if strings.Contains(r.stderr, "已在后台启动记忆整理") {
				spawned++
			}
		}
		// Normally both exits start a worker. The later one may also find,
		// at its exit, that the first worker's lock already covers its
		// session and start none; either way one consolidation runs.
		if spawned == 0 {
			f.Fatalf("neither exit started a dream worker:\n%s\n%s", runs[0].stderr, runs[1].stderr)
		}
		waitArrived(t, arrived, "a worker's consolidation request")
		if spawned == 2 {
			// The worker whose model call is held has the lock; the other
			// finds it taken (or nothing left to review) and skips.
			waitUntil(t, e2eTimeout, "the second worker to skip", func() bool { return workersFinished() > before })
		}
		release()
		waitUntil(t, e2eTimeout, "the dream workers to finish", func() bool { return workersFinished() >= before+spawned })
		if n := len(lm.dream.Requests()); n != 1 {
			f.Fatalf("%d consolidations called the model, want 1\ndream.log:\n%s", n, readFile(dreamLog))
		}
		// The other worker skipped: silently when it found the lock taken
		// before the holder had written its "running" record.
		if n := strings.Count(readFile(dreamLog)[len(logBefore):], "[dream] worker run —"); n != 1 {
			f.Fatalf("%d workers ran a consolidation, want 1\ndream.log:\n%s", n, readFile(dreamLog))
		}
		if lr := lastRun(t); lr.Result != dream.ResultCompleted {
			f.Fatalf("dream-last.json = %+v, want the running worker's completed record", lr)
		}
		f.Requests(2)
	})

	// ---------- skill review ----------

	t.Run("技能回顾期间/new技能记在旧会话名下", func(t *testing.T) {
		lm.extract.Reset() // "done": nothing to save
		lm.review.Reset(textReply("SKILL: f7-release-check | 发布前检查时使用 | 先跑 echo 自检，再打包发布"))
		arrived, release := lm.Hold("review")
		defer release()
		s, f := start(t, true,
			textReply("第一轮的回答：先确认发布范围，回答完毕。"),
			textReply("第二轮的回答：再确认版本号，回答完毕。"),
			toolReply(bashCall("echo f7-self-check")),
			textReply("第三轮的回答：自检 echo 已通过，发布前的检查全部完成，没有遗留问题。"))
		turn(s, "发布前第一步做什么", "先确认发布范围")
		turn(s, "第二步呢", "再确认版本号")
		turn(s, "跑一下自检", "发布前的检查全部完成")
		oldID := s.app.eng.SessionID()
		waitArrived(t, arrived, "the skill review request")
		mark := s.Mark()
		s.Type("/new")
		s.WaitForSince(mark, "[新会话] 已开始新会话；上一个会话已保存（"+oldID+"）", e2eTimeout)
		newID := s.app.eng.SessionID()
		release()
		var skill string
		waitUntil(t, e2eTimeout, "the learned skill file", func() bool {
			matches, _ := filepath.Glob(filepath.Join(cfgDir, "skills", "auto-f7-release-check-*", "SKILL.md"))
			if len(matches) == 1 {
				skill = readFile(matches[0])
			}
			return strings.Contains(skill, "source_session:")
		})
		if !strings.Contains(skill, "source_session: "+oldID) || newID == oldID || strings.Contains(skill, newID) {
			f.Fatalf("the skill names the wrong session (old %s, new %s):\n%s", oldID, newID, skill)
		}
		rv := lm.review.Requests()
		if len(rv) != 1 || countMessages(rv[0], "user", "跑一下自检") != 1 {
			f.Fatalf("review requests = %d, want 1 over the old conversation", len(rv))
		}
		f.Requests(4)
		s.Exit()
		// A learned skill is loaded from the next start on.
		s2, _ := start(t, false)
		mark = s2.Mark()
		s2.Type("/skills")
		s2.WaitForSince(mark, "f7-release-check", e2eTimeout)
		s2.Exit()
	})

	// ---------- --no-auto ----------

	t.Run("--no-auto时后台学习都不发生", func(t *testing.T) {
		lm.fg.Reset(
			textReply("第一问的回答，回答完毕。"),
			textReply("第二问的回答，回答完毕。"))
		lm.extract.Reset(extraction([2]string{"f7-noauto.md", "不该保存"}))
		lm.dream.Reset()
		extractBefore, dreamBefore := len(lm.extract.Requests()), len(lm.dream.Requests())
		logBefore, lastBefore := readFile(dreamLog), readFile(lastRunFile)
		r := runCove(t, strings.NewReader("第一问 marker-f7-noauto\n第二问\n"), []string{coveSpawnEnv + "=1"}, "--no-tui", "--no-auto")
		f := flow{t: t, model: lm.fg}
		if r.code != 0 || !strings.Contains(r.stdout, "第二问的回答") {
			f.Fatalf("code %d stdout %q stderr:\n%s", r.code, r.stdout, r.stderr)
		}
		if strings.Contains(r.stderr, "记忆整理") {
			f.Fatalf("--no-auto started a consolidation:\n%s", r.stderr)
		}
		f.Requests(2)
		if n, m := len(lm.extract.Requests())-extractBefore, len(lm.dream.Requests())-dreamBefore; n != 0 || m != 0 {
			f.Fatalf("--no-auto: %d extraction and %d dream requests, want none", n, m)
		}
		if _, err := os.Stat(filepath.Join(projMem, "f7-noauto.md")); !os.IsNotExist(err) {
			f.Fatalf("--no-auto saved a memory (stat err %v)", err)
		}
		if readFile(dreamLog) != logBefore || readFile(lastRunFile) != lastBefore {
			f.Fatalf("--no-auto touched dream.log or dream-last.json")
		}
	})
}
