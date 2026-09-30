package main

// Flow F4 (test design 5.3): the provider failing under a turn — rate
// limits with the three kinds of reset header, 5xx and overload (the
// per-turn fallback to the other configured model), a stream cut short,
// authentication, the spend budget and a window too small for the history.
// One home and one fake model are shared; each subtest scripts the model
// afresh and starts its own REPL.
//
// The provider's retry schedule (internal/api defaultRetry: 3 retries, base
// 1 s) and the REPL's reconnect pause (1.2 s) have no configuration knob, so
// the waits here are real but kept to a few seconds: Retry-After values of
// 2–5 s, and one subtest that sits through the full 5xx backoff.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func f4RateLimited(headers map[string]string) fakeStep {
	return fakeStep{Status: 429, Headers: headers, Body: `{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`}
}

func f4ServerError(status int) fakeStep {
	return fakeStep{Status: status, Body: `{"error":{"message":"upstream overloaded","type":"server_error"}}`}
}

func TestFlowF4_ProviderFailures(t *testing.T) {
	model := newFakeModel(t)
	// model_fast is the other model the overload fallback moves a turn to.
	home, _ := e2eHomeWith(t, model, e2eHomeOptions{Config: map[string]any{"model_fast": "qwen-fast"}})
	cfgDir := filepath.Join(home, ".cove")
	draftPath := filepath.Join(cfgDir, "interrupted.json")

	start := func(t *testing.T, steps ...fakeStep) (*e2eSession, flow) {
		t.Helper()
		model.Reset(steps...)
		_ = os.Remove(draftPath)
		s := startREPL(t)
		return s, flow{t: t, s: s, model: model}
	}
	// retryAt waits for the model's first request and its retry, and
	// returns when (by polling, within ~10 ms) each was seen.
	retryAt := func(t *testing.T) (first, retry time.Time) {
		t.Helper()
		model.WaitRequests(t, 1, e2eTimeout)
		first = time.Now()
		model.WaitRequests(t, 2, e2eTimeout)
		return first, time.Now()
	}
	// slack covers the polling of retryAt and whole-millisecond rounding.
	const slack = 60 * time.Millisecond
	// rateLimit is one 429 → wait → success subtest: the 429 carries
	// limited, the retry must not come before notBefore(when the 429's
	// request was seen), the success carries reply headers, and /ratelimit
	// afterwards must match wantRate.
	rateLimit := func(t *testing.T, limited, reply map[string]string, notBefore func(first time.Time) time.Time, wantRate *regexp.Regexp) *e2eSession {
		t.Helper()
		answer := "限流过去之后重试成功，这是完整的回答。"
		s, f := start(t, f4RateLimited(limited), fakeStep{Content: answer, Headers: reply})
		s.Type("回答一个会遇到限流的问题")
		first, retry := retryAt(t)
		if want := notBefore(first).Add(-slack); retry.Before(want) {
			f.Fatalf("retried %v after the 429, %v before the time the server asked for", retry.Sub(first), want.Sub(retry))
		}
		s.WaitFor(answer, e2eTimeout)
		s.WaitIdle(e2eTimeout)
		f.Absent("请求失败", "[E2003]")
		reqs := f.Requests(2)
		if requestTexts(reqs[0])[0] != requestTexts(reqs[1])[0] || len(reqs[0].Messages) != len(reqs[1].Messages) {
			f.Fatalf("the retry is not the same request:\n%q\n%q", requestTexts(reqs[0]), requestTexts(reqs[1]))
		}
		mark := s.Mark()
		s.Type("/ratelimit")
		s.WaitForSince(mark, "=== Rate Limit ===", e2eTimeout)
		s.WaitForSince(mark, "Updated:", e2eTimeout)
		if got := s.OutputSince(mark); !wantRate.MatchString(got) {
			f.Fatalf("/ratelimit does not show %s:\n%s", wantRate, got)
		}
		rec := capturedFromRecord(sessionFile(t, home, s.app.eng.SessionID()))
		if countMessages(rec, "assistant", answer) != 1 {
			f.Fatalf("the answer is not saved once: %q", requestTexts(rec))
		}
		return s
	}

	// ---------- 429 ----------

	t.Run("429带Retry-After秒数等待后重试成功且/ratelimit显示倒计时", func(t *testing.T) {
		if testing.Short() {
			t.Skip("slow (>3s): skipped under -short")
		}
		s := rateLimit(t,
			map[string]string{"Retry-After": "5"},
			map[string]string{"x-ratelimit-limit-requests": "100", "x-ratelimit-remaining-requests": "99", "x-ratelimit-reset-requests": "20s"},
			func(first time.Time) time.Time { return first.Add(5 * time.Second) },
			regexp.MustCompile(`Requests: 99 / 100 \(reset in 20s\)`))
		// A wait this long is announced (5 s and up).
		s.WaitFor("请求被限流（429），5 秒后自动重试（第 1/3 次）", e2eTimeout)
	})

	t.Run("429带Retry-After HTTP日期等待后重试成功且/ratelimit显示倒计时", func(t *testing.T) {
		if testing.Short() {
			t.Skip("slow (>3s): skipped under -short")
		}
		// Whole seconds: 3–4 s from now, well past the REPL's start-up.
		at := time.Now().Add(4 * time.Second).UTC().Truncate(time.Second)
		rateLimit(t,
			map[string]string{"Retry-After": at.Format(http.TimeFormat)},
			map[string]string{"x-ratelimit-limit-tokens": "80000", "x-ratelimit-remaining-tokens": "79000", "x-ratelimit-reset-tokens": "1m30s"},
			func(time.Time) time.Time { return at },
			regexp.MustCompile(`Tokens: 79000 / 80000 \(reset in 1m30s\)`))
	})

	t.Run("429带RFC3339重置头等待后重试成功且/ratelimit显示倒计时", func(t *testing.T) {
		if testing.Short() {
			t.Skip("slow (>3s): skipped under -short")
		}
		reset := time.Now().Add(3 * time.Second).UTC()
		later := time.Now().Add(45 * time.Second).UTC().Format(time.RFC3339Nano)
		rateLimit(t,
			map[string]string{"anthropic-ratelimit-requests-remaining": "0", "anthropic-ratelimit-requests-reset": reset.Format(time.RFC3339Nano)},
			map[string]string{"anthropic-ratelimit-requests-limit": "50", "anthropic-ratelimit-requests-remaining": "49", "anthropic-ratelimit-requests-reset": later},
			func(time.Time) time.Time { return reset },
			regexp.MustCompile(`Requests: 49 / 50 \(reset in (3\d|4[0-5])s\)`))
	})

	// ---------- 5xx and overload ----------

	var fallbackLine *regexp.Regexp = regexp.MustCompile(`模型 (\S+) 暂时不可用，本轮改用 (\S+) 继续`)
	var fallbackFrom, fallbackTo string

	t.Run("5xx三次后本轮回退到备用模型且下一轮回到主模型", func(t *testing.T) {
		if testing.Short() {
			t.Skip("sits through the provider's 5xx backoff (1+2+4 s ±50%)")
		}
		s, f := start(t,
			f4ServerError(503), f4ServerError(502), f4ServerError(529), f4ServerError(529),
			textReply("备用模型接手完成了重构，本轮处理完成。"),
			textReply("第二轮由主模型回答，重构说明补充完成。"))
		const ask = "重构 internal/api/retry.go 里的 retryDelay 函数，拆出 jitter 计算"
		s.Type(ask)
		s.WaitFor("本轮处理完成。", 60*time.Second)
		s.WaitIdle(e2eTimeout)
		m := fallbackLine.FindStringSubmatch(s.Output())
		if m == nil {
			f.Fatalf("no fallback notice")
		}
		fallbackFrom, fallbackTo = m[1], m[2]
		reqs := f.Requests(5)
		primary := reqs[0].Model
		for i := 1; i < 4; i++ {
			if reqs[i].Model != primary {
				f.Fatalf("retry %d went to %s, want the same model %s", i, reqs[i].Model, primary)
			}
		}
		if fallbackFrom != primary || reqs[4].Model != fallbackTo || fallbackTo == primary {
			f.Fatalf("fallback %s → %s, requests went %s → %s", fallbackFrom, fallbackTo, primary, reqs[4].Model)
		}
		f.Absent("请求失败")

		mark := s.Mark()
		s.Type("再" + ask)
		s.WaitForSince(mark, "重构说明补充完成。", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		reqs = f.Requests(6)
		if reqs[5].Model != primary {
			f.Fatalf("the next turn went to %s, want the primary %s again", reqs[5].Model, primary)
		}
		if strings.Contains(s.OutputSince(mark), "本轮改用") {
			f.Fatalf("the next turn fell back again")
		}
		rec := capturedFromRecord(sessionFile(t, home, s.app.eng.SessionID()))
		if countMessages(rec, "assistant", "备用模型接手完成了重构") != 1 || countMessages(rec, "assistant", "重构说明补充完成") != 1 {
			f.Fatalf("the session does not hold both answers: %q", requestTexts(rec))
		}
	})

	t.Run("流中断后透明重试", func(t *testing.T) {
		s, f := start(t,
			fakeStep{Content: "回答的前半段", StreamCutAfter: 1},
			textReply("这次是完整的回答，前面断掉的部分已经补齐，回答完成。"))
		s.Type("回答一个会遇到断流的问题")
		s.WaitFor("网络波动", e2eTimeout)
		s.WaitFor("前面断掉的部分已经补齐", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		f.Absent("请求失败")
		reqs := f.Requests(2)
		if countMessages(reqs[1], "user", "回答一个会遇到断流的问题") != 1 {
			f.Fatalf("the retry does not carry the request once: %q", requestTexts(reqs[1]))
		}
		if countMessages(reqs[1], "assistant", "回答的前半段") != 0 {
			f.Fatalf("the cut half answer was sent back as a finished reply: %q", requestTexts(reqs[1]))
		}
		rec := capturedFromRecord(sessionFile(t, home, s.app.eng.SessionID()))
		if countMessages(rec, "assistant", "") != 1 || countMessages(rec, "assistant", "前面断掉的部分已经补齐") != 1 {
			f.Fatalf("the session does not hold exactly the whole answer: %q", requestTexts(rec))
		}
	})

	t.Run("流内overloaded_error与HTTP 529相同回退", func(t *testing.T) {
		s, f := start(t,
			fakeStep{StreamError: &fakeStreamError{Type: "overloaded_error", Message: "Overloaded"}},
			textReply("过载之后由备用模型接手，本轮处理完成。"))
		s.Type("重构 internal/api/retry.go 里的 retryDelay 函数，拆出 jitter 计算")
		s.WaitFor("本轮处理完成。", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		m := fallbackLine.FindStringSubmatch(s.Output())
		if m == nil {
			f.Fatalf("no fallback notice after the in-stream overloaded_error")
		}
		reqs := f.Requests(2)
		if m[1] != reqs[0].Model || m[2] != reqs[1].Model || m[1] == m[2] {
			f.Fatalf("fallback %s → %s, requests went %s → %s", m[1], m[2], reqs[0].Model, reqs[1].Model)
		}
		// The same move as the HTTP 5xx/529 subtest's (when it ran).
		if fallbackFrom != "" && (m[1] != fallbackFrom || m[2] != fallbackTo) {
			f.Fatalf("in-stream overload fell back %s → %s, HTTP 529 %s → %s", m[1], m[2], fallbackFrom, fallbackTo)
		}
		f.Absent("请求失败")
		rec := capturedFromRecord(sessionFile(t, home, s.app.eng.SessionID()))
		if countMessages(rec, "assistant", "过载之后由备用模型接手") != 1 {
			f.Fatalf("the answer is not saved: %q", requestTexts(rec))
		}
	})

	// ---------- authentication ----------

	t.Run("401单key立即停止并提示", func(t *testing.T) {
		s, f := start(t, fakeStep{Status: 401, Body: `{"error":{"message":"invalid api key","type":"authentication_error"}}`})
		s.Type("回答一个会遇到认证失败的问题")
		s.WaitFor("请求失败", e2eTimeout)
		s.WaitFor("API Key 无效或已过期，请检查 api_key 配置", e2eTimeout)
		s.WaitFor("[E2004]", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		f.Absent("网络波动", "自动重试")
		f.Requests(1)
		d, err := os.ReadFile(draftPath)
		if err != nil || !strings.Contains(string(d), "回答一个会遇到认证失败的问题") {
			f.Fatalf("the failed request is not kept as a draft: %v %s", err, d)
		}
	})

	t.Run("401多key时轮换到下一个key", func(t *testing.T) {
		// provider.APIKeys (internal/config) is `json:"-"` and nothing fills
		// it from config.json or the environment, so a person cannot give
		// cove more than one key; the rotation (internal/api transport.send)
		// is exercised by internal/api's key_rotation_test.go only.
		t.Skip("not configurable: no config key or env var reaches ProviderConfig.APIKeys")
	})

	// ---------- budget ----------

	t.Run("预算80%提示一次、到上限暂停提示/budget、/budget off后继续", func(t *testing.T) {
		cfgPath := filepath.Join(cfgDir, "config.json")
		cfgBefore, _ := os.ReadFile(cfgPath)
		t.Cleanup(func() { _ = os.WriteFile(cfgPath, cfgBefore, 0o600) })
		// 50 000 completion tokens at the default rate ($0.87/M) make each
		// call ~$0.0435: two calls reach 87% of $0.10, the third passes it.
		costly := func(st fakeStep) fakeStep { st.CompletionTokens = 50000; return st }
		s, f := start(t,
			costly(toolReply(bashCall("echo budget_step1"))),
			costly(textReply("第一轮：命令输出了 budget_step1，第一轮处理完成。")),
			costly(toolReply(bashCall("echo budget_step2"))),
			textReply("预算放开后继续，第二步也完成了，第二轮处理完成。"))
		mark := s.Mark()
		s.Type("/budget 0.10")
		s.WaitForSince(mark, "本会话预算: $0.10", e2eTimeout)
		s.Type("第一轮：跑一条命令")
		s.WaitFor("第一轮处理完成。", e2eTimeout)
		s.WaitFor("费用已达预算的 80%", e2eTimeout)
		s.WaitIdle(e2eTimeout)

		s.Type("第二轮：再跑一条命令")
		s.WaitFor("预算已超限，继续重试不会成功", e2eTimeout)
		s.WaitFor("/budget", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		f.Requests(3)
		if n := strings.Count(s.Output(), "费用已达预算的 80%"); n != 1 {
			f.Fatalf("the 80%% notice was shown %d times, want once", n)
		}
		// A new message is refused before it reaches the model.
		mark = s.Mark()
		s.Type("第三轮：这一条不该发出")
		s.WaitForSince(mark, "预算已超限，继续重试不会成功", e2eTimeout)
		f.Requests(3)

		mark = s.Mark()
		s.Type("/budget off")
		s.WaitForSince(mark, "已取消本会话的预算上限", e2eTimeout)
		if got, _ := os.ReadFile(cfgPath); string(got) != string(cfgBefore) {
			f.Fatalf("/budget <n> and /budget off changed config.json:\n%s", got)
		}
		s.Type("继续")
		s.WaitFor("第二轮处理完成。", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		reqs := f.Requests(4)
		if n := countMessages(reqs[3], "user", "第二轮：再跑一条命令"); n != 1 {
			f.Fatalf("the resumed request carries the paused one %d times, want once: %q", n, requestTexts(reqs[3]))
		}
		if countMessages(reqs[3], "tool", "budget_step2") != 1 {
			f.Fatalf("the step done before the pause is not kept: %q", requestTexts(reqs[3]))
		}
		if countMessages(reqs[3], "", "第三轮") != 0 {
			f.Fatalf("the refused message reached the model: %q", requestTexts(reqs[3]))
		}

		// /budget save is what writes config.json.
		mark = s.Mark()
		s.Type("/budget 3")
		s.WaitForSince(mark, "本会话预算: $3.00", e2eTimeout)
		s.Type("/budget save")
		s.WaitForSince(mark, "已把预算 $3.00 写入配置", e2eTimeout)
		var cfg map[string]any
		raw, _ := os.ReadFile(cfgPath)
		if err := json.Unmarshal(raw, &cfg); err != nil || cfg["max_budget_usd"] != 3.0 {
			f.Fatalf("/budget save did not write max_budget_usd 3: %v\n%s", err, raw)
		}
	})

	// ---------- window ----------

	t.Run("小窗口溢出后压缩并继续且输出解释", func(t *testing.T) {
		// The remedy writes the learned window into config.json.
		cfgPath := filepath.Join(cfgDir, "config.json")
		cfgBefore, _ := os.ReadFile(cfgPath)
		t.Cleanup(func() { _ = os.WriteFile(cfgPath, cfgBefore, 0o600) })
		// Three tool turns: 12 messages, the least the automatic
		// compaction summarises.
		var steps []fakeStep
		for i := 1; i <= 3; i++ {
			steps = append(steps,
				toolReply(bashCall(fmt.Sprintf("echo window_step%d", i))),
				textReply(fmt.Sprintf("第%d轮：命令输出了 window_step%d，本轮介绍完成。", i, i)))
		}
		steps = append(steps,
			fakeStep{Status: 400, Body: overflowBody},
			textReply("之前的对话摘要：用户让助手依次运行了三条 echo 命令（window_step1、window_step2、window_step3），每条都成功输出了对应的标记，没有修改任何文件，也没有未完成的事项。"),
			textReply("第四轮：压缩之后继续回答，cli 负责终端交互，介绍完成。"))
		s, f := start(t, steps...)
		for i := 1; i <= 3; i++ {
			mark := s.Mark()
			s.Type(fmt.Sprintf("第%d轮：跑一条 echo 命令", i))
			s.WaitForSince(mark, "本轮介绍完成。", e2eTimeout)
			s.WaitIdle(e2eTimeout)
		}
		s.Type("第四轮：cli 是做什么的")
		s.WaitFor("第四轮：压缩之后继续回答", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		s.WaitFor("已按服务端返回的 16384 token 调整模型", e2eTimeout)
		s.WaitFor("上下文超出模型上限，已压缩对话历史后重试", e2eTimeout)
		f.Absent("请求失败", "对话历史已无法再压缩")
		reqs := f.Requests(9)
		overflowed, retried := reqs[6], reqs[8]
		if countMessages(reqs[7], "", "window_step1") == 0 {
			f.Fatalf("request 8 is not the compaction summary: %q", requestTexts(reqs[7]))
		}
		if len(retried.Messages) >= len(overflowed.Messages) {
			f.Fatalf("the retry sent %d messages, the overflowed request %d: nothing was compacted", len(retried.Messages), len(overflowed.Messages))
		}
		if last := lastMessage(retried); last.Role != "user" || !strings.HasPrefix(last.Text(), "第四轮：cli 是做什么的") {
			f.Fatalf("the retry does not end with the question: %s %q", last.Role, last.Text())
		}
		rec := capturedFromRecord(sessionFile(t, home, s.app.eng.SessionID()))
		if countMessages(rec, "assistant", "压缩之后继续回答") != 1 || len(rec.Messages) >= len(overflowed.Messages) {
			f.Fatalf("the saved session is not the compacted one with the answer: %q", requestTexts(rec))
		}
	})
}
