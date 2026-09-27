# 诊断系统重构 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 运行期错误按类型归类得到稳定诊断码，带可执行建议和真实的自动处置，`/diagnose errors` 按码聚合展示。

**Architecture:** `internal/api` 导出错误归类枚举与会话级上下文窗口覆盖；`internal/diagnostic` 新增结构化入口 `Report`、处置器接口 `Remedy` 与按码聚合；引擎、卡住监控、故障切换层改为调用 `Report`；`/diagnose` 与启动提示消费新的聚合结果。静态检查器不动。

**Tech Stack:** Go 1.25，标准库；`go test ./...`。

**Spec:** `docs/superpowers/specs/2026-09-27-diagnostic-refactor-design.md`

## Global Constraints

- 目录 `D:\github\cove-main` 不是 git 仓库：每个任务末尾不提交，改为运行 `gofmt -l`、`go vet` 与该包测试，并记录结果。
- 静态检查器（`checker.go` 13 项、`background.go`）及其测试不改。
- 处置器只改运行策略与会话级参数，不修改 `config.json`，不修改代码。
- 用户可见文案用简体中文；错误码沿用 `E<类别><序号>` 四位数字。
- `internal/config` 的 `TestCoveBranding_NoLegacyClaudeNamesInDemoTree` 为既有失败，不在范围内。
- `internal/diagnostic` 不能被 `internal/api`、`internal/engine` 以外的方向反向依赖：`api` 不得 import `diagnostic`（`diagnostic` 已 import `api`），`diagnostic` 不得 import `engine`。

## Review Focus

1. **同一错误被记两次**：`log.Errorf` 经 sink 落日志，同一错误又经 `Report` 落日志，`/diagnose errors` 显示两条。Task 4 把故障切换层的 `log.Errorf` 改为 `log.Debugf`，Task 2 的聚合测试断言 `Source: "log"` 与 `Source: "report"` 的同文本事件不合并、且 Task 4 引擎测试断言只出现一条 E2008。
2. **处置器在无 Runtime 时被调用**：headless、`-p`、测试没有注入 `Runtime`，处置器必须返回 `ok=false` 且不 panic。Task 3 测试覆盖 `SetRuntime(nil)`。
3. **窗口解析出荒谬值**：报错里解析到 0、负数、或大于当前估算（服务端窗口比 cove 猜的还大）时不得缩小或放大到错误值。Task 3 只在 `0 < n < 当前估算` 时处置。
4. **报错文本里有多个数字**：`request (17964 tokens) exceeds the available context size (16384 tokens)` 必须取 16384 而不是 17964。Task 3 的表驱动测试用原文。
5. **旧 `errors.log` 里的事件缺新字段**：`Model`/`Source` 为空的旧 JSON 行必须仍能加载、聚合、显示。Task 5 用一段旧格式 JSON 行测试 `LoadRuntimeLog` 后的聚合。

---

### Task 1: `internal/api` — 错误归类枚举、窗口覆盖、两个哨兵错误、不可用回调

**Files:**
- Create: `internal/api/classify.go`
- Create: `internal/api/classify_test.go`
- Modify: `internal/api/model_context.go:54-70`
- Modify: `internal/api/model_context_test.go`（追加）
- Modify: `internal/api/tool_repair.go`（追加类型）
- Modify: `internal/api/fallback.go:57-81, 162-177`
- Modify: `internal/api/fallback_test.go` 或新建 `internal/api/fallback_unavailable_test.go`

**Interfaces:**
- Produces:
  - `type ErrorKind int` 与常量 `KindUnknown, KindCanceled, KindContextLength, KindRateLimit, KindAuth, KindBadRequest, KindServerError, KindTransport, KindToolArgs, KindProviderUnavailable`
  - `func Classify(err error) ErrorKind`
  - `type ToolArgsInvalidError struct{ Tool string }`（实现 `error`）
  - `type ProviderUnavailableError struct{ Provider string; Fails int; Cause error }`（实现 `error`，`Unwrap() error`）
  - `func (mf *ModelFallback) SetOnUnavailable(fn func(provider string, fails int, cause error))`
  - `func SetModelContextWindow(model string, tokens int)`、`func ClearModelContextWindows()`；`ContextWindowForModel` 先查覆盖

- [ ] **Step 1: 写失败测试 `internal/api/classify_test.go`**

```go
package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestClassifyRecognisesEveryKind(t *testing.T) {
	llama := &StatusError{Status: 400, Msg: `{"error":{"code":400,"message":"request (17964 tokens) exceeds the available context size (16384 tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":17964,"n_ctx":16384}}`}
	cases := []struct {
		name string
		err  error
		want ErrorKind
	}{
		{"nil", nil, KindUnknown},
		{"canceled", context.Canceled, KindCanceled},
		{"wrapped canceled", fmt.Errorf("api: %w", context.Canceled), KindCanceled},
		{"deadline", context.DeadlineExceeded, KindTransport},
		{"llama.cpp context", llama, KindContextLength},
		{"wrapped context", fmt.Errorf("api: %w", llama), KindContextLength},
		{"openai context", &StatusError{Status: 400, Msg: "This model's maximum context length is 65536 tokens"}, KindContextLength},
		{"413", &StatusError{Status: 413, Msg: "request_too_large"}, KindContextLength},
		{"429", &StatusError{Status: 429, Msg: "slow down"}, KindRateLimit},
		{"retryable 429", &RetryableError{Status: 429, Msg: "x"}, KindRateLimit},
		{"401", &StatusError{Status: 401, Msg: "bad key"}, KindAuth},
		{"403", &StatusError{Status: 403, Msg: "forbidden"}, KindAuth},
		{"400 other", &StatusError{Status: 400, Msg: "max_tokens must be at most 8192"}, KindBadRequest},
		{"503", &StatusError{Status: 503, Msg: "busy"}, KindServerError},
		{"retryable transport", &RetryableError{Status: 0, Msg: "connection reset by peer"}, KindTransport},
		{"plain transport", errors.New("dial tcp: connection refused"), KindTransport},
		{"tool args", &ToolArgsInvalidError{Tool: "bash"}, KindToolArgs},
		{"provider unavailable wraps context", &ProviderUnavailableError{Provider: "openai-compatible", Fails: 3, Cause: llama}, KindProviderUnavailable},
		{"unknown", errors.New("boom"), KindUnknown},
	}
	for _, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Errorf("%s: Classify = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestErrorKindString(t *testing.T) {
	if KindContextLength.String() != "context_length" || KindUnknown.String() != "unknown" {
		t.Errorf("String() = %q / %q", KindContextLength, KindUnknown)
	}
}

func TestProviderUnavailableErrorUnwraps(t *testing.T) {
	cause := &StatusError{Status: 400, Msg: "context size"}
	err := &ProviderUnavailableError{Provider: "p", Fails: 3, Cause: cause}
	if !errors.Is(err, cause) {
		t.Fatal("Unwrap does not reach the cause")
	}
	if !IsContextLengthError(err) {
		t.Fatal("the cause's context-length nature is hidden by the wrapper")
	}
}
```

- [ ] **Step 2: 运行确认编译失败**

Run: `cd /d/github/cove-main && go test ./internal/api/ -run 'TestClassify|TestErrorKind|TestProviderUnavailable' 2>&1 | head -5`
Expected: `undefined: ErrorKind` 等编译错误。

- [ ] **Step 3: 新建 `internal/api/classify.go`**

```go
package api

import (
	"context"
	"errors"
	"net/http"
)

// ErrorKind is the coarse nature of a failed model call, for the diagnostic
// layer and the UI. It replaces matching error text in those layers: the
// status carried by StatusError/RetryableError and the typed errors below
// decide, and the textual fallbacks live here only.
type ErrorKind int

const (
	KindUnknown ErrorKind = iota
	KindCanceled
	KindContextLength
	KindRateLimit
	KindAuth
	KindBadRequest
	KindServerError
	KindTransport
	KindToolArgs
	KindProviderUnavailable
)

func (k ErrorKind) String() string {
	switch k {
	case KindCanceled:
		return "canceled"
	case KindContextLength:
		return "context_length"
	case KindRateLimit:
		return "rate_limit"
	case KindAuth:
		return "auth"
	case KindBadRequest:
		return "bad_request"
	case KindServerError:
		return "server_error"
	case KindTransport:
		return "transport"
	case KindToolArgs:
		return "tool_args"
	case KindProviderUnavailable:
		return "provider_unavailable"
	default:
		return "unknown"
	}
}

// ToolArgsInvalidError says a model's tool call carried arguments that were
// not JSON even after RepairToolArguments; the engine reports it so repeated
// occurrences with one model can be counted.
type ToolArgsInvalidError struct{ Tool string }

func (e *ToolArgsInvalidError) Error() string {
	return "tool call arguments for " + e.Tool + " were not valid JSON"
}

// ProviderUnavailableError says the fallback chain marked a provider
// unavailable after repeated failures; Cause is the failure that tipped it.
type ProviderUnavailableError struct {
	Provider string
	Fails    int
	Cause    error
}

func (e *ProviderUnavailableError) Error() string {
	return "provider " + e.Provider + " marked unavailable: " + e.Cause.Error()
}

func (e *ProviderUnavailableError) Unwrap() error { return e.Cause }

// Classify rates err. The typed errors come first, so a provider marked
// unavailable because of a context overflow is KindProviderUnavailable, and
// a cancellation is never mistaken for a transport error.
func Classify(err error) ErrorKind {
	if err == nil {
		return KindUnknown
	}
	var pu *ProviderUnavailableError
	if errors.As(err, &pu) {
		return KindProviderUnavailable
	}
	var ta *ToolArgsInvalidError
	if errors.As(err, &ta) {
		return KindToolArgs
	}
	if errors.Is(err, context.Canceled) {
		return KindCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return KindTransport
	}
	if IsContextLengthError(err) {
		return KindContextLength
	}
	switch st := statusOf(err); {
	case st == http.StatusTooManyRequests:
		return KindRateLimit
	case st == http.StatusUnauthorized || st == http.StatusForbidden:
		return KindAuth
	case st == http.StatusBadRequest:
		return KindBadRequest
	case st >= 500:
		return KindServerError
	case st != 0:
		return KindUnknown
	}
	switch {
	case isRateLimit(err):
		return KindRateLimit
	case isPermanent(err):
		return KindAuth
	case isTemporary(err):
		return KindTransport
	}
	return KindUnknown
}
```

- [ ] **Step 4: 运行归类测试**

Run: `go test ./internal/api/ -run 'TestClassify|TestErrorKind|TestProviderUnavailable' -v 2>&1 | tail -8`
Expected: PASS。

- [ ] **Step 5: 写窗口覆盖的失败测试（追加到 `internal/api/model_context_test.go`）**

```go
// A local server's real window (learned from its error) overrides the
// name-based guess for the rest of the session, case-insensitively.
func TestModelContextWindowOverride(t *testing.T) {
	t.Cleanup(ClearModelContextWindows)
	if w := ContextWindowForModel("qwen3.6-27b"); w != 32000 {
		t.Fatalf("baseline guess = %d, want 32000", w)
	}
	SetModelContextWindow("Qwen3.6-27B", 16384)
	if w := ContextWindowForModel("qwen3.6-27b"); w != 16384 {
		t.Errorf("override ignored: %d", w)
	}
	if got := CompactionTrigger("qwen3.6-27b"); got >= 16384 {
		t.Errorf("compaction trigger %d does not fit a 16384 window", got)
	}
	SetModelContextWindow("qwen3.6-27b", 0)
	if w := ContextWindowForModel("qwen3.6-27b"); w != 32000 {
		t.Errorf("zero did not clear the override: %d", w)
	}
}
```

- [ ] **Step 6: 实现覆盖表（`internal/api/model_context.go`，替换 `ContextWindowForModel`）**

```go
// contextOverrides holds windows learned or configured for a model this
// session (SetModelContextWindow), keyed by lower-cased name. They beat the
// name-based guesses: a local server answering with its n_ctx knows better
// than a substring table.
var (
	contextOverridesMu sync.RWMutex
	contextOverrides   = map[string]int{}
)

// SetModelContextWindow records model's real context window for this
// session; tokens <= 0 removes the record.
func SetModelContextWindow(model string, tokens int) {
	key := strings.ToLower(strings.TrimSpace(model))
	contextOverridesMu.Lock()
	defer contextOverridesMu.Unlock()
	if tokens <= 0 {
		delete(contextOverrides, key)
		return
	}
	contextOverrides[key] = tokens
}

// ClearModelContextWindows forgets every SetModelContextWindow record.
func ClearModelContextWindows() {
	contextOverridesMu.Lock()
	defer contextOverridesMu.Unlock()
	contextOverrides = map[string]int{}
}

// ContextWindowForModel returns an approximate context window size (in
// tokens) for the given model name: a window set for it this session, else
// a substring match against known model families, else defaultContextWindow.
func ContextWindowForModel(model string) int {
	lower := strings.ToLower(strings.TrimSpace(model))
	contextOverridesMu.RLock()
	w, ok := contextOverrides[lower]
	contextOverridesMu.RUnlock()
	if ok {
		return w
	}
	for _, p := range contextWindowPatterns {
		if strings.Contains(lower, p.pattern) {
			return p.window
		}
	}
	return defaultContextWindow
}
```

`import` 增加 `"sync"`。

- [ ] **Step 7: 运行窗口测试**

Run: `go test ./internal/api/ -run 'ContextWindow|Compaction' 2>&1 | tail -4`
Expected: PASS。

- [ ] **Step 8: 写不可用回调的失败测试（新建 `internal/api/fallback_unavailable_test.go`）**

```go
package api

import (
	"context"
	"errors"
	"testing"
)

// When the chain marks a provider unavailable it tells the registered
// callback once, with the failure count and the error that tipped it, so the
// diagnostic layer can record E2009 with its cause.
func TestFallbackReportsProviderUnavailableOnce(t *testing.T) {
	cause := &StatusError{Status: 400, Msg: "request (17964 tokens) exceeds the available context size (16384 tokens)"}
	p := &flakyProvider{errs: []error{cause, cause, cause, cause}}
	mf := NewModelFallback([]Provider{p})
	var got []*ProviderUnavailableError
	mf.SetOnUnavailable(func(provider string, fails int, err error) {
		got = append(got, &ProviderUnavailableError{Provider: provider, Fails: fails, Cause: err})
	})
	for i := 0; i < 4; i++ {
		_, _, _ = mf.TryChat(context.Background(), func(Provider) ChatRequest { return ChatRequest{} })
	}
	if len(got) != 1 {
		t.Fatalf("callback called %d times, want once", len(got))
	}
	if got[0].Fails != 3 || !errors.Is(got[0], cause) {
		t.Errorf("event = %+v", got[0])
	}
}
```

（`flakyProvider` 已在 `internal/api` 的测试文件中定义；若其 `errs` 用尽后返回成功，把切片补足 4 个错误即可。）

- [ ] **Step 9: 实现回调（`internal/api/fallback.go`）**

在 `ModelFallback` 结构体增加字段：

```go
	// onUnavailable, when set, is told once each time a provider is marked
	// unavailable (SetOnUnavailable); the engine routes it to diagnostics.
	onUnavailable func(provider string, fails int, cause error)
```

新增方法：

```go
// SetOnUnavailable registers the callback told when a provider is marked
// unavailable after repeated failures. It runs under the chain's lock, so it
// must not call back into the chain.
func (mf *ModelFallback) SetOnUnavailable(fn func(provider string, fails int, cause error)) {
	mf.mu.Lock()
	defer mf.mu.Unlock()
	mf.onUnavailable = fn
}
```

把 `try` 中的分支

```go
		} else if pw.FailCount >= mf.maxFails || isPermanent(err) {
			pw.Status = ProviderUnavailable
			log.Errorf("provider %s marked unavailable after %d failures: %v", pw.Provider.Name(), pw.FailCount, err)
		}
```

改为

```go
		} else if pw.FailCount >= mf.maxFails || isPermanent(err) {
			wasUnavailable := pw.Status == ProviderUnavailable
			pw.Status = ProviderUnavailable
			// Debug, not Error: the diagnostic layer records this through
			// onUnavailable with a code and a hint; an Errorf would land in
			// the same log a second time, uncoded, via the log sink.
			log.Debugf("provider %s marked unavailable after %d failures: %v", pw.Provider.Name(), pw.FailCount, err)
			if mf.onUnavailable != nil && !wasUnavailable {
				mf.onUnavailable(pw.Provider.Name(), pw.FailCount, err)
			}
		}
```

- [ ] **Step 10: 运行整个 api 包**

Run: `gofmt -l ./internal/api; go vet ./internal/api/ && go test ./internal/api/ 2>&1 | tail -4`
Expected: 无 gofmt 输出，PASS。

---

### Task 2: `internal/diagnostic` — 结构化入口、目录改造、聚合重写

**Files:**
- Create: `internal/diagnostic/report.go`
- Create: `internal/diagnostic/report_test.go`
- Modify: `internal/diagnostic/errors.go:80-89, 101-122, 133-142, 160-198`
- Modify: `internal/diagnostic/recorder.go:17-27, 78-85, 112-144, 179-225`
- Modify: `internal/command/diagnose.go:166-175`（去掉 `AutoFixable` 引用，见 Step 8，先让编译通过；显示改造在 Task 5）

**Interfaces:**
- Consumes: Task 1 的 `api.Classify`、`api.ErrorKind`、`*api.ToolArgsInvalidError`、`*api.ProviderUnavailableError`
- Produces:
  - `type Context struct{ Provider, Model, Tool string; Attempt int }`
  - `type Stall struct{ Stage string; Idle time.Duration }`（实现 `error`）
  - `func Report(err error, c Context) RuntimeEvent`
  - `func Classify(err error, c Context) (ErrorCode, string)`
  - `type Runtime interface{ SetModelContextWindow(model string, tokens int); Notify(line string) }`
  - `func SetRuntime(rt Runtime)`
  - `type RemedyFunc func(ev RuntimeEvent, rt Runtime) (applied string, ok bool)`；`ErrorDef.Remedy RemedyFunc`
  - `RuntimeEvent` 新增 `Model, Provider, Tool, Source string`
  - `type RuntimeSummary struct{ Code ErrorCode; Message, Model string; Count int; Last time.Time; Severity Severity; Recovery string; Applied []string; Detail string; HasRemedy bool }`
  - 新码 `ErrAPIContextLength = "E2008"`, `ErrAPIProviderUnavailable = "E2009"`, `ErrToolArgsInvalid = "E4009"`, `ErrEngineStall = "E5007"`

- [ ] **Step 1: 写失败测试 `internal/diagnostic/report_test.go`**

```go
package diagnostic

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
)

const llamaOverflow = `{"error":{"code":400,"message":"request (17964 tokens) exceeds the available context size (16384 tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":17964,"n_ctx":16384}}`

func resetRuntimeEvents(t *testing.T) {
	t.Helper()
	runtimeMu.Lock()
	runtimeEvents = nil
	runtimeMu.Unlock()
	SetRuntime(nil)
	t.Cleanup(func() {
		runtimeMu.Lock()
		runtimeEvents = nil
		runtimeMu.Unlock()
		SetRuntime(nil)
	})
}

// Errors are coded by their type, never by matching Chinese titles against
// the text: the local server's overflow, the chain marking a provider
// unavailable, a tool call with unparsable arguments and a stalled stage all
// get their code.
func TestClassifyGivesEachErrorItsCode(t *testing.T) {
	overflow := &api.StatusError{Status: 400, Msg: llamaOverflow}
	cases := []struct {
		err  error
		want ErrorCode
	}{
		{fmt.Errorf("api: %w", overflow), ErrAPIContextLength},
		{&api.ProviderUnavailableError{Provider: "openai-compatible", Fails: 3, Cause: overflow}, ErrAPIProviderUnavailable},
		{&api.ToolArgsInvalidError{Tool: "bash"}, ErrToolArgsInvalid},
		{&Stall{Stage: "call model qwen3.6-27b", Idle: 33 * time.Second}, ErrEngineStall},
		{&api.StatusError{Status: 429, Msg: "x"}, ErrAPIRateLimit},
		{&api.StatusError{Status: 401, Msg: "x"}, ErrAPIAuth},
		{&api.StatusError{Status: 400, Msg: "max_tokens too large"}, ErrAPIBadRequest},
		{&api.StatusError{Status: 502, Msg: "x"}, ErrAPIServerError},
		{errors.New("read tcp: connection reset by peer"), ErrAPIStreamBroken},
		{errors.New("something odd"), ""},
	}
	for _, c := range cases {
		if got, _ := Classify(c.err, Context{}); got != c.want {
			t.Errorf("Classify(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	if code, _ := Classify(nil, Context{}); code != "" {
		t.Errorf("nil error coded %q", code)
	}
}

// Report records one event carrying the code, the call's context and the
// error text, at the code's severity.
func TestReportRecordsACodedEvent(t *testing.T) {
	resetRuntimeEvents(t)
	err := fmt.Errorf("api: %w", &api.StatusError{Status: 400, Msg: llamaOverflow})
	ev := Report(err, Context{Provider: "openai-compatible", Model: "qwen3.6-27b"})
	if ev.Code != ErrAPIContextLength || ev.Model != "qwen3.6-27b" || ev.Provider != "openai-compatible" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Severity != SevError || ev.Category != CatAPI || ev.Source != "report" {
		t.Errorf("severity/category/source = %v/%v/%q", ev.Severity, ev.Category, ev.Source)
	}
	if !strings.Contains(ev.Message, "16384") {
		t.Errorf("message lost the error text: %q", ev.Message)
	}
	got := RecentRuntime()
	if len(got) != 1 || got[0].Code != ErrAPIContextLength {
		t.Fatalf("recorded = %+v", got)
	}
}

// An error of no known kind is still recorded, uncoded, as before.
func TestReportRecordsUnknownErrorsUncoded(t *testing.T) {
	resetRuntimeEvents(t)
	ev := Report(errors.New("something odd"), Context{Tool: "bash"})
	if ev.Code != "" || ev.Category != CatTool || ev.Severity != SevError {
		t.Errorf("event = %+v", ev)
	}
	if Report(nil, Context{}) != (RuntimeEvent{}) {
		t.Error("nil error produced an event")
	}
}

// The log sink still records Warn/Error lines, uncoded and marked as coming
// from the log, so they can be told apart from structured reports.
func TestRecordRuntimeNoLongerGuessesCodesFromText(t *testing.T) {
	resetRuntimeEvents(t)
	RecordRuntime(SevError, CatAPI, "模型调用失败: 请求参数错误 400")
	got := RecentRuntime()
	if len(got) != 1 || got[0].Code != "" {
		t.Fatalf("free text was coded: %+v", got)
	}
	if got[0].Source != "log" {
		t.Errorf("source = %q, want log", got[0].Source)
	}
}

// Aggregation: coded events group by code and model whatever their token
// counts say; uncoded ones by their text with numbers and paths blanked;
// recovered events attach to the coded entry they fixed.
func TestSummarizeRuntimeGroupsByCodeAndModel(t *testing.T) {
	resetRuntimeEvents(t)
	base := time.Date(2026, 9, 27, 13, 17, 0, 0, time.Local)
	events := []RuntimeEvent{
		{Time: base, Severity: SevError, Category: CatAPI, Code: ErrAPIContextLength, Model: "qwen", Message: "request (16569 tokens) exceeds", Source: "report"},
		{Time: base.Add(time.Minute), Severity: SevError, Category: CatAPI, Code: ErrAPIContextLength, Model: "qwen", Message: "request (17964 tokens) exceeds", Source: "report"},
		{Time: base.Add(2 * time.Minute), Severity: SevRecovered, Category: CatAPI, Code: ErrAPIContextLength, Model: "qwen", Message: "窗口 32000 → 16384", Source: "remedy"},
		{Time: base, Severity: SevError, Category: CatAPI, Code: ErrAPIContextLength, Model: "other", Message: "x", Source: "report"},
		{Time: base, Severity: SevWarning, Category: CatEngine, Message: "loop detected (layer 2): read D:\\a\\b.go 3 times", Source: "log"},
		{Time: base, Severity: SevWarning, Category: CatEngine, Message: "loop detected (layer 2): read D:\\c\\d.go 7 times", Source: "log"},
	}
	sums := SummarizeRuntime(events)
	if len(sums) != 3 {
		t.Fatalf("got %d summaries: %+v", len(sums), sums)
	}
	first := sums[0]
	if first.Code != ErrAPIContextLength || first.Model != "qwen" || first.Count != 2 {
		t.Errorf("first summary = %+v", first)
	}
	if !first.Last.Equal(base.Add(time.Minute)) {
		t.Errorf("Last = %v", first.Last)
	}
	if len(first.Applied) != 1 || first.Applied[0] != "窗口 32000 → 16384" {
		t.Errorf("Applied = %v", first.Applied)
	}
	if first.Recovery == "" || first.Message != Lookup(ErrAPIContextLength).Message {
		t.Errorf("summary lacks the catalogue's title/recovery: %+v", first)
	}
	var loops *RuntimeSummary
	for i := range sums {
		if sums[i].Code == "" {
			loops = &sums[i]
		}
	}
	if loops == nil || loops.Count != 2 {
		t.Fatalf("uncoded events not merged by normalised text: %+v", sums)
	}
	if strings.Contains(loops.Message, "D:\\") || strings.Contains(loops.Message, "7") {
		t.Errorf("normalised message still carries a path or number: %q", loops.Message)
	}
}

// The catalogue no longer claims fixes it cannot make.
func TestCatalogueHasNoAutoFixFlags(t *testing.T) {
	for code, def := range AllErrors() {
		if strings.Contains(def.Recovery, "已自动调整") || strings.Contains(def.Recovery, "已自动切换") {
			t.Errorf("%s still promises an automatic fix: %q", code, def.Recovery)
		}
	}
	for _, code := range []ErrorCode{ErrAPIContextLength, ErrAPIProviderUnavailable, ErrToolArgsInvalid, ErrEngineStall} {
		if Lookup(code) == nil || Lookup(code).Recovery == "" {
			t.Errorf("%s missing or without recovery hint", code)
		}
	}
}
```

- [ ] **Step 2: 运行确认编译失败**

Run: `go test ./internal/diagnostic/ -run 'TestClassifyGives|TestReport|TestRecordRuntimeNoLonger|TestSummarizeRuntimeGroups|TestCatalogue' 2>&1 | head -5`
Expected: `undefined: Report`、`unknown field Model` 等。

- [ ] **Step 3: 目录改造（`internal/diagnostic/errors.go`）**

在 E2xxx 常量块追加：

```go
	// ErrAPIContextLength: the request did not fit the model's context
	// window, even after the engine compacted and retried.
	ErrAPIContextLength ErrorCode = "E2008"
	// ErrAPIProviderUnavailable: the fallback chain stopped preferring a
	// provider after repeated failures.
	ErrAPIProviderUnavailable ErrorCode = "E2009"
```

在 E4xxx 块追加：

```go
	// ErrToolArgsInvalid: a model's tool call carried arguments that were
	// not JSON even after repair.
	ErrToolArgsInvalid ErrorCode = "E4009"
```

在 E5xxx 块追加：

```go
	// ErrEngineStall: a stage made no progress for stallThreshold.
	ErrEngineStall ErrorCode = "E5007"
```

`ErrorDef` 改为：

```go
// ErrorDef defines a known error type with its metadata and recovery info.
type ErrorDef struct {
	Code     ErrorCode
	Category Category
	Severity Severity
	Message  string // User-facing summary (Chinese)
	Detail   string // Technical detail template
	Recovery string // What the user can do
	// Remedy, when set, is an action the diagnostic layer may take at run
	// time when an event with this code is reported (see report.go). It
	// changes run-time strategy or session-level parameters only, never
	// code or config files, and says what it did.
	Remedy RemedyFunc
}
```

`init()` 里每条 `register(&ErrorDef{code, cat, sev, msg, detail, recovery, a, b})` 去掉末尾两个 bool（共 30 条，机械修改），并把 E2005 的 Recovery 改为 `"检查模型名与请求格式；若是本地服务，确认其兼容 OpenAI 接口"`，E3002 的 Recovery 改为 `"非交互模式下无法请求权限确认，请用 bypass 模式或事先在 policies.json 写好 allow 规则"`。追加四条：

```go
	register(&ErrorDef{ErrAPIContextLength, CatAPI, SevError, "上下文超出模型窗口", "%s",
		"输入 /continue 会先压缩对话历史再重试；若仍失败，需调大模型的上下文长度（llama-server 加 -c 65536、LM Studio 的 Context Length、Ollama 的 num_ctx），cove 的系统提示词和工具定义本身约占 13K token", nil})
	register(&ErrorDef{ErrAPIProviderUnavailable, CatAPI, SevError, "供应商已被标记不可用", "%s",
		"连续 3 次失败后本会话不再优先尝试该供应商；只有一个供应商时仍会继续尝试它，修好原因（如调大模型上下文）后成功一次即自动复位；有多个供应商时可用 /provider <名称> 切换", nil})
	register(&ErrorDef{ErrToolArgsInvalid, CatTool, SevWarning, "工具参数非法 JSON", "%s",
		"模型输出的工具参数无法解析；同一模型反复出现说明它不适合工具调用，考虑换模型或降低 temperature", nil})
	register(&ErrorDef{ErrEngineStall, CatEngine, SevWarning, "模型调用无进展", "%s",
		"长时间无响应可按 Ctrl+C 中断；本地模型常见于上下文接近上限或显存不足", nil})
```

（`Remedy` 在 Task 3 里由 `remedy.go` 的 `init()` 赋值，这里先 `nil`。）

- [ ] **Step 4: 事件与记录（`internal/diagnostic/recorder.go`）**

`RuntimeEvent` 改为：

```go
type RuntimeEvent struct {
	Time     time.Time `json:"time"`
	Severity Severity  `json:"severity"`
	Category Category  `json:"category"`
	Message  string    `json:"message"`
	Code     ErrorCode `json:"code,omitempty"` // set when classified
	// Model, Provider and Tool are the call's context when known.
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	Tool     string `json:"tool,omitempty"`
	// Source says how the event was made: "report" (a classified error),
	// "remedy" (what a remedy did), "log" (a Warn/Error log line).
	Source string `json:"source,omitempty"`
}
```

`RecordRuntime` 改为：

```go
// RecordRuntime captures a free-form runtime problem (a Warn/Error log line)
// in memory and in the persistent log. It is uncoded: codes come from
// Report, which classifies the error itself instead of guessing from text.
func RecordRuntime(sev Severity, cat Category, message string) {
	record(RuntimeEvent{Time: time.Now(), Severity: sev, Category: cat, Message: message, Source: "log"})
}

// record appends ev to the ring buffer and the persistent log.
func record(ev RuntimeEvent) {
	runtimeMu.Lock()
	runtimeEvents = append(runtimeEvents, ev)
	if len(runtimeEvents) > maxRuntimeEvents {
		runtimeEvents = runtimeEvents[len(runtimeEvents)-maxRuntimeEvents:]
	}
	runtimeMu.Unlock()

	if p := runtimeLogPath(); p != "-" {
		// （原有的轮转与追加逻辑，原样搬入，不变）
		...
	}
}
```

删除 `matchKnownCode`。`RuntimeSummary` 与 `SummarizeRuntime` 替换为：

```go
// RuntimeSummary is one line of /diagnose errors: a coded problem for one
// model (or an uncoded message), how often and how recently it happened,
// the catalogue's hint, and what a remedy did about it.
type RuntimeSummary struct {
	Code      ErrorCode
	Message   string // the catalogue title when coded, else the normalised text
	Model     string
	Count     int
	Last      time.Time
	Severity  Severity
	Recovery  string
	Applied   []string // remedy results, oldest first
	Detail    string   // the latest event's own text (coded entries)
	HasRemedy bool
}

// SummarizeRuntime aggregates events for display: coded events by code and
// model, whatever their numbers say; uncoded ones by their text with
// numbers and paths blanked, so one problem is one line. Recovered events
// attach to the entry of the same code and model. Ordered by severity then
// frequency.
func SummarizeRuntime(events []RuntimeEvent) []RuntimeSummary {
	byKey := map[string]*RuntimeSummary{}
	var order []string
	get := func(key string, ev RuntimeEvent) *RuntimeSummary {
		s, ok := byKey[key]
		if !ok {
			s = &RuntimeSummary{Code: ev.Code, Model: ev.Model, Severity: ev.Severity}
			if def := registry[ev.Code]; ev.Code != "" && def != nil {
				s.Message, s.Recovery, s.HasRemedy = def.Message, def.Recovery, def.Remedy != nil
			} else {
				s.Message = normaliseMessage(ev.Message)
			}
			byKey[key] = s
			order = append(order, key)
		}
		return s
	}
	for _, ev := range events {
		key := string(ev.Code) + "|" + ev.Model
		if ev.Code == "" {
			key = "|" + normaliseMessage(ev.Message)
		}
		s := get(key, ev)
		if ev.Severity == SevRecovered {
			s.Applied = append(s.Applied, ev.Message)
			continue
		}
		s.Count++
		if ev.Time.After(s.Last) {
			s.Last, s.Detail = ev.Time, ev.Message
		}
		if ev.Severity > s.Severity {
			s.Severity = ev.Severity
		}
	}
	out := make([]RuntimeSummary, 0, len(order))
	for _, k := range order {
		if s := byKey[k]; s.Count > 0 || len(s.Applied) > 0 {
			out = append(out, *s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity > out[j].Severity
		}
		return out[i].Count > out[j].Count
	})
	return out
}

var (
	numberRe = regexp.MustCompile(`\d+`)
	pathRe   = regexp.MustCompile(`(?:[A-Za-z]:\\|/)[^\s"'，。；:]+`)
)

// normaliseMessage blanks the parts of a free-form message that differ
// between occurrences of one problem: paths, then numbers.
func normaliseMessage(s string) string {
	s = pathRe.ReplaceAllString(s, "#")
	s = numberRe.ReplaceAllString(s, "#")
	return strings.Join(strings.Fields(s), " ")
}
```

`import` 增加 `"regexp"`。注意：一个只有 `SevRecovered` 事件的分组也要显示（`len(s.Applied) > 0`），且其 `Severity` 初值会是 `SevRecovered`，排序时排最前，这是期望的（用户先看到「已处置」）。

- [ ] **Step 5: 新建 `internal/diagnostic/report.go`**

```go
package diagnostic

import (
	"fmt"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/log"
)

// Context is what the place that saw an error knows about the call; every
// field may be empty.
type Context struct {
	Provider, Model, Tool string
	Attempt               int
}

// Stall is the error the engine reports when a stage has made no progress
// for its threshold. It lives here so the engine can report it without the
// diagnostic layer importing the engine.
type Stall struct {
	Stage string
	Idle  time.Duration
}

func (s *Stall) Error() string {
	return fmt.Sprintf("「%s」阶段已 %s 无进展", s.Stage, s.Idle.Round(time.Second))
}

// Runtime is what a remedy may touch at run time. The CLI injects an
// implementation; headless runs and tests leave it nil, and every remedy
// then declines.
type Runtime interface {
	// SetModelContextWindow records model's real context window for this
	// session (api.SetModelContextWindow behind it).
	SetModelContextWindow(model string, tokens int)
	// Notify shows the user one line.
	Notify(line string)
}

// RemedyFunc is an action bound to an error code. It returns what it did
// and true when it changed something, false when it did not apply.
type RemedyFunc func(ev RuntimeEvent, rt Runtime) (applied string, ok bool)

var (
	runtimeRtMu sync.RWMutex
	runtimeRt   Runtime
)

// SetRuntime installs the Runtime remedies act through; nil disables them.
func SetRuntime(rt Runtime) {
	runtimeRtMu.Lock()
	defer runtimeRtMu.Unlock()
	runtimeRt = rt
}

func currentRuntime() Runtime {
	runtimeRtMu.RLock()
	defer runtimeRtMu.RUnlock()
	return runtimeRt
}

// Classify gives err the code of its kind, and the detail to record. It
// decides by the error's type (api.Classify, Stall), never by matching
// text; "" means no known kind.
func Classify(err error, c Context) (ErrorCode, string) {
	if err == nil {
		return "", ""
	}
	if st, ok := err.(*Stall); ok {
		return ErrEngineStall, st.Error()
	}
	switch api.Classify(err) {
	case api.KindContextLength:
		return ErrAPIContextLength, err.Error()
	case api.KindProviderUnavailable:
		return ErrAPIProviderUnavailable, err.Error()
	case api.KindToolArgs:
		return ErrToolArgsInvalid, err.Error()
	case api.KindRateLimit:
		return ErrAPIRateLimit, err.Error()
	case api.KindAuth:
		return ErrAPIAuth, err.Error()
	case api.KindBadRequest:
		return ErrAPIBadRequest, err.Error()
	case api.KindServerError:
		return ErrAPIServerError, err.Error()
	case api.KindTransport:
		return ErrAPIStreamBroken, err.Error()
	case api.KindCanceled:
		return ErrEngineCtxCancel, err.Error()
	}
	return "", err.Error()
}

// Report classifies and records err, runs the code's remedy when there is
// one and a Runtime is installed, and returns the recorded event so the
// caller can quote its code. A nil err records nothing. It never panics and
// never blocks on anything but the log file.
func Report(err error, c Context) RuntimeEvent {
	if err == nil {
		return RuntimeEvent{}
	}
	code, detail := Classify(err, c)
	ev := RuntimeEvent{
		Time: time.Now(), Message: detail, Code: code,
		Model: c.Model, Provider: c.Provider, Tool: c.Tool, Source: "report",
		Severity: SevError, Category: CatEngine,
	}
	if c.Tool != "" {
		ev.Category = CatTool
	}
	def := registry[code]
	if code != "" && def != nil {
		ev.Severity, ev.Category = def.Severity, def.Category
	}
	record(ev)
	if def != nil && def.Remedy != nil {
		if rt := currentRuntime(); rt != nil {
			if applied, ok := applyRemedy(def.Remedy, ev, rt); ok {
				record(RuntimeEvent{
					Time: time.Now(), Severity: SevRecovered, Category: ev.Category, Code: code,
					Message: applied, Model: c.Model, Provider: c.Provider, Tool: c.Tool, Source: "remedy",
				})
			}
		}
	}
	return ev
}

// applyRemedy runs fn and turns a panic into "did not apply".
func applyRemedy(fn RemedyFunc, ev RuntimeEvent, rt Runtime) (applied string, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Warnf("diagnostic remedy for %s panicked: %v", ev.Code, r)
			applied, ok = "", false
		}
	}()
	return fn(ev, rt)
}
```

- [ ] **Step 6: 让 `internal/command/diagnose.go` 编译（临时最小改动）**

把 `listCodes` 中

```go
			fixable := ""
			if def.AutoFixable && def.HotFixable {
				fixable = " \x1b[32m(自动修复+即时生效)\x1b[0m"
			} else if def.AutoFixable {
				fixable = " \x1b[32m(可自动修复)\x1b[0m"
			}
```

改为

```go
			fixable := ""
			if def.Remedy != nil {
				fixable = " \x1b[32m(有处置器)\x1b[0m"
			}
```

- [ ] **Step 7: 运行 diagnostic 与 command 包**

Run: `gofmt -l ./internal/diagnostic ./internal/command; go vet ./internal/diagnostic/ ./internal/command/ && go test ./internal/diagnostic/ ./internal/command/ 2>&1 | tail -6`
Expected: 无 gofmt 输出，两包 PASS（`TestSummarizeRuntimeGroupsByCodeAndModel` 中 `first.Recovery` 非空依赖 Step 3 的目录条目）。

- [ ] **Step 8: 编译全仓**

Run: `go build ./... 2>&1 | head`
Expected: 无输出。若 `cli/cove` 或其他处引用了 `AutoFixable`（`grep -rn AutoFixable --include=*.go .`），按 Step 6 同法改掉。

---

### Task 3: 处置器 — 学习模型窗口、非法工具参数计数

**Files:**
- Create: `internal/diagnostic/remedy.go`
- Create: `internal/diagnostic/remedy_test.go`

**Interfaces:**
- Consumes: Task 2 的 `RemedyFunc`、`Runtime`、`RuntimeEvent`、`registry`
- Produces: `func parseContextWindow(msg string) int`（包内）；`init()` 把 `contextWindowRemedy`、`toolArgsRemedy` 挂到 `ErrAPIContextLength`、`ErrToolArgsInvalid`；`func ResetRemedyState()`（测试与 `/diagnose archive` 用）

- [ ] **Step 1: 写失败测试 `internal/diagnostic/remedy_test.go`**

```go
package diagnostic

import (
	"fmt"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

type stubRuntime struct {
	windows map[string]int
	notes   []string
}

func newStubRuntime() *stubRuntime { return &stubRuntime{windows: map[string]int{}} }

func (s *stubRuntime) SetModelContextWindow(model string, tokens int) { s.windows[model] = tokens }
func (s *stubRuntime) Notify(line string)                            { s.notes = append(s.notes, line) }

// The real window is the one the server names, not the request's size and
// not the first number in the text.
func TestParseContextWindow(t *testing.T) {
	cases := map[string]int{
		llamaOverflow: 16384,
		`request (17964 tokens) exceeds the available context size (16384 tokens), try increasing it`: 16384,
		`This model's maximum context length is 65536 tokens. However, you requested 70000 tokens`:  65536,
		`prompt is too long: 210000 tokens > 200000 maximum`:                                       200000,
		`"n_ctx": 8192`: 8192,
		`max_tokens must be at most 8192`: 0,
		``: 0,
	}
	for msg, want := range cases {
		if got := parseContextWindow(msg); got != want {
			t.Errorf("parseContextWindow(%q) = %d, want %d", msg, got, want)
		}
	}
}

// Reporting an overflow with a Runtime installed shrinks the model's window
// to what the server said, tells the user, and records what it did.
func TestContextWindowRemedyLearnsTheServersWindow(t *testing.T) {
	resetRuntimeEvents(t)
	ResetRemedyState()
	t.Cleanup(api.ClearModelContextWindows)
	rt := newStubRuntime()
	SetRuntime(rt)

	err := fmt.Errorf("api: %w", &api.StatusError{Status: 400, Msg: llamaOverflow})
	Report(err, Context{Provider: "openai-compatible", Model: "qwen3.6-27b"})

	if rt.windows["qwen3.6-27b"] != 16384 {
		t.Fatalf("window not set: %v", rt.windows)
	}
	if len(rt.notes) != 1 || !strings.Contains(rt.notes[0], "16384") || !strings.Contains(rt.notes[0], "context_window") {
		t.Errorf("user not told, or not told how to persist: %v", rt.notes)
	}
	events := RecentRuntime()
	if len(events) != 2 || events[1].Severity != SevRecovered || events[1].Code != ErrAPIContextLength {
		t.Fatalf("recovered event missing: %+v", events)
	}
	if !strings.Contains(events[1].Message, "32000") || !strings.Contains(events[1].Message, "16384") {
		t.Errorf("applied text = %q", events[1].Message)
	}
}

// Nothing happens when there is no Runtime, when the text names no window,
// when the named window is not smaller than the current estimate, or when
// the model is unknown.
func TestContextWindowRemedyDeclines(t *testing.T) {
	resetRuntimeEvents(t)
	ResetRemedyState()
	t.Cleanup(api.ClearModelContextWindows)

	overflow := func(msg string) error {
		return fmt.Errorf("api: %w", &api.StatusError{Status: 400, Msg: msg})
	}
	// No runtime.
	Report(overflow(llamaOverflow), Context{Model: "qwen3.6-27b"})
	if n := len(RecentRuntime()); n != 1 {
		t.Fatalf("without a Runtime %d events were recorded, want 1", n)
	}
	rt := newStubRuntime()
	SetRuntime(rt)
	// No window in the text.
	Report(overflow("input is too long"), Context{Model: "qwen3.6-27b"})
	// Window not smaller than the estimate (32000 for an unknown name).
	Report(overflow(`request exceeds the available context size (65536 tokens)`), Context{Model: "qwen3.6-27b"})
	// No model.
	Report(overflow(llamaOverflow), Context{})
	if len(rt.windows) != 0 || len(rt.notes) != 0 {
		t.Errorf("remedy applied when it should have declined: %v %v", rt.windows, rt.notes)
	}
	for _, ev := range RecentRuntime() {
		if ev.Severity == SevRecovered {
			t.Errorf("recovered event recorded without a remedy: %+v", ev)
		}
	}
}

// Invalid tool arguments are counted per model; the user hears about it at
// the third occurrence and then every fifth.
func TestToolArgsRemedyCountsPerModel(t *testing.T) {
	resetRuntimeEvents(t)
	ResetRemedyState()
	rt := newStubRuntime()
	SetRuntime(rt)
	for i := 0; i < 8; i++ {
		Report(&api.ToolArgsInvalidError{Tool: "bash"}, Context{Model: "qwen3.6-27b", Tool: "bash"})
	}
	Report(&api.ToolArgsInvalidError{Tool: "edit"}, Context{Model: "other", Tool: "edit"})
	if len(rt.notes) != 2 {
		t.Fatalf("notes = %v, want one at 3 and one at 8", rt.notes)
	}
	if !strings.Contains(rt.notes[0], "qwen3.6-27b") || !strings.Contains(rt.notes[0], "3 次") {
		t.Errorf("first note = %q", rt.notes[0])
	}
	recovered := 0
	for _, ev := range RecentRuntime() {
		if ev.Severity == SevRecovered {
			recovered++
		}
	}
	if recovered != 2 {
		t.Errorf("%d recovered events, want 2", recovered)
	}
}

// A remedy that panics is a remedy that did not apply.
func TestPanickingRemedyIsContained(t *testing.T) {
	resetRuntimeEvents(t)
	def := registry[ErrAPIContextLength]
	saved := def.Remedy
	def.Remedy = func(RuntimeEvent, Runtime) (string, bool) { panic("boom") }
	t.Cleanup(func() { def.Remedy = saved })
	SetRuntime(newStubRuntime())
	ev := Report(fmt.Errorf("api: %w", &api.StatusError{Status: 400, Msg: llamaOverflow}), Context{Model: "m"})
	if ev.Code != ErrAPIContextLength || len(RecentRuntime()) != 1 {
		t.Fatalf("panic leaked or event lost: %+v", RecentRuntime())
	}
}
```

- [ ] **Step 2: 运行确认编译失败**

Run: `go test ./internal/diagnostic/ -run 'TestParseContextWindow|TestContextWindowRemedy|TestToolArgsRemedy|TestPanickingRemedy' 2>&1 | head -5`
Expected: `undefined: parseContextWindow`、`undefined: ResetRemedyState`。

- [ ] **Step 3: 新建 `internal/diagnostic/remedy.go`**

```go
package diagnostic

import (
	"fmt"
	"regexp"
	"strconv"
	"sync"

	"github.com/liuzhixin405/cove/internal/api"
)

// Remedies: the actions the diagnostic layer takes itself when an error is
// reported. They change run-time strategy or session-level parameters and
// tell the user; they never touch code or config files. Each is bound to its
// code in init.

func init() {
	registry[ErrAPIContextLength].Remedy = contextWindowRemedy
	registry[ErrToolArgsInvalid].Remedy = toolArgsRemedy
}

// contextWindowPatterns are the ways servers name their window in an
// overflow error, most specific first; the capture is the window.
var contextWindowRemedyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`n_ctx"?\s*:\s*(\d+)`),
	regexp.MustCompile(`context size \((\d+) tokens\)`),
	regexp.MustCompile(`maximum context length is (\d+)`),
	regexp.MustCompile(`(\d+) maximum`),
}

// parseContextWindow returns the window an overflow error names, or 0.
func parseContextWindow(msg string) int {
	for _, re := range contextWindowRemedyPatterns {
		if m := re.FindStringSubmatch(msg); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// contextWindowRemedy learns a model's real window from the server's error:
// when the error names a window smaller than cove's estimate for the model,
// the estimate is replaced for this session, so compaction triggers where it
// should and the next /continue fits. The user is told how to make it stick.
func contextWindowRemedy(ev RuntimeEvent, rt Runtime) (string, bool) {
	if ev.Model == "" {
		return "", false
	}
	n := parseContextWindow(ev.Message)
	current := api.ContextWindowForModel(ev.Model)
	if n <= 0 || n >= current {
		return "", false
	}
	rt.SetModelContextWindow(ev.Model, n)
	rt.Notify(fmt.Sprintf("已按服务端返回的 %d token 调整模型 %s 本会话的上下文预算；要固定下来，在 config.json 加 \"context_window\": %d", n, ev.Model, n))
	return fmt.Sprintf("窗口 %d → %d", current, n), true
}

var (
	toolArgsMu     sync.Mutex
	toolArgsCounts = map[string]int{}
)

// toolArgsRemedy counts invalid tool arguments per model and tells the user
// at the third occurrence, then every fifth: one bad call is noise, a
// pattern says the model is not up to tool calling.
func toolArgsRemedy(ev RuntimeEvent, rt Runtime) (string, bool) {
	if ev.Model == "" {
		return "", false
	}
	toolArgsMu.Lock()
	toolArgsCounts[ev.Model]++
	n := toolArgsCounts[ev.Model]
	toolArgsMu.Unlock()
	if n < 3 || (n > 3 && (n-3)%5 != 0) {
		return "", false
	}
	rt.Notify(fmt.Sprintf("模型 %s 已 %d 次输出非法的工具参数，建议换一个模型或降低 temperature", ev.Model, n))
	return fmt.Sprintf("已提示（第 %d 次）", n), true
}

// ResetRemedyState forgets the remedies' counters, for tests and for
// /diagnose archive, which starts a new cycle.
func ResetRemedyState() {
	toolArgsMu.Lock()
	toolArgsCounts = map[string]int{}
	toolArgsMu.Unlock()
}
```

- [ ] **Step 4: 运行 diagnostic 包**

Run: `gofmt -l ./internal/diagnostic; go vet ./internal/diagnostic/ && go test ./internal/diagnostic/ 2>&1 | tail -4`
Expected: PASS。若 `TestToolArgsRemedyCountsPerModel` 因第 8 次未提示失败，检查条件：n=3 提示，n=8 提示（(8-3)%5==0），n=13 提示。

---

### Task 4: 接线 — 引擎、卡住监控、故障切换、配置键、CLI 注入

**Files:**
- Modify: `internal/engine/engine.go:381-383`（构造处）、`:1335-1362`（失败处）、`:1813-1819`（ParseError 处）
- Modify: `internal/engine/activity.go:101-111`
- Modify: `internal/engine/context_length_retry_test.go`（追加断言）或新建 `internal/engine/diagnostic_wiring_test.go`
- Modify: `internal/config/config.go:81-90`
- Modify: `cli/cove/app_bootstrap.go:52-58`
- Create: `cli/cove/diagnostic_runtime.go`

**Interfaces:**
- Consumes: Task 1 `api.SetModelContextWindow`、`api.ToolArgsInvalidError`、`api.ProviderUnavailableError`、`(*api.ModelFallback).SetOnUnavailable`；Task 2 `diagnostic.Report`、`diagnostic.Context`、`diagnostic.Stall`、`diagnostic.Runtime`、`diagnostic.SetRuntime`
- Produces: `config.Config.ContextWindow int`（`json:"context_window,omitempty"`）；`cli/cove` 的 `diagRuntime` 类型

- [ ] **Step 1: 写引擎侧失败测试（新建 `internal/engine/diagnostic_wiring_test.go`）**

先看 `internal/engine/context_length_retry_test.go` 里构造「返回上下文超长错误的 provider」的方式（`newTestEngine` 与一个总是返回 `&api.StatusError{Status: 400, Msg: …}` 的 mock provider），复用同一套件：

```go
package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/diagnostic"
)

// A turn that dies of context overflow leaves exactly one coded E2008 event
// (no uncoded duplicate from a log line) and names the code in the failure
// reason the user sees.
func TestContextOverflowIsReportedOnceWithItsCode(t *testing.T) {
	clearDiagnostics(t)
	const msg = `{"error":{"code":400,"message":"request (17964 tokens) exceeds the available context size (16384 tokens), try increasing it","type":"exceed_context_size_error","n_ctx":16384}}`
	prov := &mockProvider{err: &api.StatusError{Status: 400, Msg: msg}} // 按 context_length_retry_test.go 中的写法构造总是失败的 provider
	eng := newTestEngine(prov)
	eng.config.Model = "qwen3.6-27b"
	var shown []string
	eng.OnEngineOutput = func(line string) { shown = append(shown, line) }

	_, err := eng.RunWithStream(context.Background(), "hi", func(string) {})
	if err == nil {
		t.Fatal("want the overflow error back")
	}
	coded := 0
	for _, ev := range diagnostic.RecentRuntime() {
		if ev.Code == diagnostic.ErrAPIContextLength {
			coded++
			if ev.Model != "qwen3.6-27b" {
				t.Errorf("event lacks the model: %+v", ev)
			}
		}
	}
	if coded != 1 {
		t.Fatalf("E2008 recorded %d times, want 1: %+v", coded, diagnostic.RecentRuntime())
	}
	if !strings.Contains(strings.Join(shown, "\n"), "[E2008]") {
		t.Errorf("failure line does not name the code:\n%s", strings.Join(shown, "\n"))
	}
}

// A tool call whose arguments were not JSON is reported as E4009 with the
// tool and model.
func TestUnparsableToolArgsAreReported(t *testing.T) {
	clearDiagnostics(t)
	eng := newTestEngine(&mockProvider{})
	eng.config.Model = "qwen3.6-27b"
	out := eng.executeTool(context.Background(), api.ToolCall{ID: "1", Name: "bash", ParseError: true,
		Input: map[string]any{"_cove_parse_error": "tool call arguments were not valid JSON"}})
	if !strings.HasPrefix(out, "Error:") {
		t.Fatalf("result = %q", out)
	}
	evs := diagnostic.RecentRuntime()
	if len(evs) != 1 || evs[0].Code != diagnostic.ErrToolArgsInvalid || evs[0].Tool != "bash" || evs[0].Model != "qwen3.6-27b" {
		t.Fatalf("events = %+v", evs)
	}
}

// The provider chain marking its provider unavailable is E2009, with the
// overflow that caused it wrapped inside.
func TestProviderUnavailableIsReported(t *testing.T) {
	clearDiagnostics(t)
	const msg = `request (17964 tokens) exceeds the available context size (16384 tokens)`
	eng := newTestEngine(&mockProvider{err: &api.StatusError{Status: 400, Msg: msg}})
	for i := 0; i < 4; i++ {
		_, _ = eng.RunWithStream(context.Background(), "hi", func(string) {})
	}
	seen := 0
	for _, ev := range diagnostic.RecentRuntime() {
		if ev.Code == diagnostic.ErrAPIProviderUnavailable {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("E2009 recorded %d times, want 1", seen)
	}
}

// clearDiagnostics empties the recorded events and disables remedies.
func clearDiagnostics(t *testing.T) {
	t.Helper()
	diagnostic.ResetForTest()
	t.Cleanup(diagnostic.ResetForTest)
}
```

`diagnostic.ResetForTest` 需在 `internal/diagnostic/report.go` 末尾新增（导出，供其他包的测试用）：

```go
// ResetForTest empties the recorded events, the remedy counters and the
// Runtime. Tests in other packages use it; production code never does.
func ResetForTest() {
	runtimeMu.Lock()
	runtimeEvents = nil
	runtimeMu.Unlock()
	ResetRemedyState()
	SetRuntime(nil)
}
```

`mockProvider` 的字段名（`err`）以 `internal/engine/engine_test.go` 中的实际定义为准；若它不支持「每次都返回同一个错误」，用 `context_length_retry_test.go` 里那个 provider 类型。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/engine/ -run 'TestContextOverflowIsReported|TestUnparsableToolArgs|TestProviderUnavailableIsReported' 2>&1 | head -8`
Expected: `undefined: diagnostic.ResetForTest` 或断言失败（E2008 为 0）。

- [ ] **Step 3: 引擎失败处（`internal/engine/engine.go` 约 1347-1362）**

把

```go
		if err != nil {
			e.recordEvent(ctx, "llm_error", map[string]any{"error": err.Error()})
			// Keep the completed tool rounds: their side effects already
			// happened, and re-sending this message resumes from here.
			e.interrupt(userMessage, routedModel, "模型调用失败: "+textutil.ClipRunes(err.Error(), 120))
			diagnostic.RecordRuntime(diagnostic.SevError, diagnostic.CatAPI,
				fmt.Sprintf("模型调用失败: %s", err.Error()))
			return "", fmt.Errorf("api: %w", err)
		}
```

改为

```go
		if err != nil {
			e.recordEvent(ctx, "llm_error", map[string]any{"error": err.Error()})
			// Classified and recorded with its code (and remedied when the
			// diagnostic layer can), so /diagnose errors shows one line with
			// a hint instead of the raw text; the code is quoted in the
			// failure line the user sees.
			ev := diagnostic.Report(err, e.diagContext(modelName, ""))
			reason := "模型调用失败: " + textutil.ClipRunes(err.Error(), 120)
			if ev.Code != "" {
				reason += " [" + string(ev.Code) + "]"
			}
			// Keep the completed tool rounds: their side effects already
			// happened, and re-sending this message resumes from here.
			e.interrupt(userMessage, routedModel, reason)
			return "", fmt.Errorf("api: %w", err)
		}
```

在 `engine.go` 的 `ProviderName` 附近新增：

```go
// diagContext is what the engine tells the diagnostic layer about a call.
func (e *Engine) diagContext(model, tool string) diagnostic.Context {
	c := diagnostic.Context{Model: model, Tool: tool}
	if model == "" && e.config != nil {
		c.Model = e.config.Model
	}
	if e.fallback != nil {
		c.Provider = e.fallback.Current().Name()
	}
	return c
}
```

`interrupt` 打印的失败行是否经 `OnEngineOutput` 输出，以 `turn.go:31` 的实现为准；测试断言 `[E2008]` 出现在 `OnEngineOutput` 收到的行里，若 `interrupt` 只写历史不打印，则改为断言 `eng.messages` 末尾的中断标记含 `[E2008]`。

- [ ] **Step 4: ParseError 处（`internal/engine/engine.go` 约 1813）**

```go
	if tc.ParseError {
		msg, _ := tc.Input["_cove_parse_error"].(string)
		if msg == "" {
			msg = "tool call arguments could not be parsed as JSON"
		}
		diagnostic.Report(&api.ToolArgsInvalidError{Tool: tc.Name}, e.diagContext("", tc.Name))
		return fmt.Sprintf("Error: %s. Please resend this tool call with valid JSON arguments (check quote escaping, and avoid truncating long string fields).", msg)
	}
```

- [ ] **Step 5: 构造处挂不可用回调（`internal/engine/engine.go` 约 381-390 之后，`e` 构造完成处）**

在 `e := &Engine{...}` 之后加：

```go
	e.fallback.SetOnUnavailable(func(provider string, fails int, cause error) {
		diagnostic.Report(&api.ProviderUnavailableError{Provider: provider, Fails: fails, Cause: cause},
			diagnostic.Context{Provider: provider, Model: e.config.Model})
	})
```

- [ ] **Step 6: 卡住监控（`internal/engine/activity.go:101-111`）**

```go
			for _, s := range stuckList {
				e.engineOutput(fmt.Sprintf(
					"\r\x1b[K\x1b[33m! 仍在「%s」阶段，已 %s 无进展（可能卡住，按 Ctrl+C 可中断）\x1b[0m\n",
					s.label, s.idle.Round(time.Second)))
				// Recorded with its code (E5007); not also logged, which would
				// put the same line in errors.log a second time via the sink.
				diagnostic.Report(&diagnostic.Stall{Stage: s.label, Idle: s.idle}, e.diagContext("", ""))
			}
```

- [ ] **Step 7: 运行引擎测试**

Run: `gofmt -l ./internal/engine; go vet ./internal/engine/ && go test ./internal/engine/ -run 'TestContextOverflowIsReported|TestUnparsableToolArgs|TestProviderUnavailableIsReported|ContextLength' 2>&1 | tail -6`
Expected: PASS。

- [ ] **Step 8: 配置键（`internal/config/config.go` `Config` 结构体）**

在 `ThinkingTokens int` 之后加：

```go
	// ContextWindow is the context window of Model in tokens, for servers
	// cove cannot recognise by model name (a local llama.cpp with -c 16384).
	// It sizes compaction; 0 keeps the name-based estimate. The E2008 remedy
	// suggests the value to put here.
	ContextWindow int `json:"context_window,omitempty"`
```

- [ ] **Step 9: CLI 注入（新建 `cli/cove/diagnostic_runtime.go`）**

```go
package main

import (
	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/config"
	"github.com/liuzhixin405/cove/internal/diagnostic"
	"github.com/liuzhixin405/cove/internal/termui"
)

// diagRuntime is what the diagnostic layer's remedies may do in the
// interactive shell: adjust a model's context budget for this session and
// tell the user a line.
type diagRuntime struct{}

func (diagRuntime) SetModelContextWindow(model string, tokens int) {
	api.SetModelContextWindow(model, tokens)
}

func (diagRuntime) Notify(line string) {
	termui.PrintAbove("  " + termui.Styled(termui.Yellow, "⚙ "+line) + "\n")
}

// installDiagnostics wires the remedies' Runtime and applies the configured
// context window before any model call.
func installDiagnostics(cfg *config.Config) {
	diagnostic.SetRuntime(diagRuntime{})
	if cfg != nil && cfg.ContextWindow > 0 && cfg.Model != "" {
		api.SetModelContextWindow(cfg.Model, cfg.ContextWindow)
	}
}
```

在 `cli/cove/app_bootstrap.go` 的 `diagnostic.AttachToLogger()` 之后加一行 `installDiagnostics(cfg)`。

- [ ] **Step 10: 写 CLI 侧测试（新建 `cli/cove/diagnostic_runtime_test.go`）**

```go
package main

import (
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/config"
)

// context_window in config.json sizes compaction for a model cove cannot
// recognise by name.
func TestConfiguredContextWindowApplies(t *testing.T) {
	t.Cleanup(api.ClearModelContextWindows)
	cfg := &config.Config{Model: "qwen3.6-27b", ContextWindow: 16384}
	installDiagnostics(cfg)
	if w := api.ContextWindowForModel("qwen3.6-27b"); w != 16384 {
		t.Fatalf("window = %d, want the configured 16384", w)
	}
}
```

- [ ] **Step 11: 全量编译与相关包测试**

Run: `gofmt -l ./internal ./cli; go build ./... && go vet ./... && go test ./internal/api/ ./internal/diagnostic/ ./internal/engine/ ./internal/config/ ./cli/cove/ 2>&1 | grep -v '^ok' | tail -10`
Expected: 只剩既有的 `TestCoveBranding_NoLegacyClaudeNamesInDemoTree`。

---

### Task 5: `/diagnose` 输出、启动提示、文档

**Files:**
- Modify: `internal/command/diagnose.go:62-132`
- Modify: `internal/command/diagnose_audit_test.go`（追加）
- Modify: `internal/diagnostic/recorder.go`（追加 `UnresolvedFromLog`）
- Modify: `internal/diagnostic/report_test.go`（追加）
- Modify: `cli/cove/app_bootstrap.go:202-207`
- Modify: `docs/USER_MANUAL.md`（诊断系统一节、配置表）
- Modify: `CHANGELOG.md`

**Interfaces:**
- Consumes: Task 2 `RuntimeSummary`、`SummarizeRuntime`、`LoadRuntimeLog`
- Produces: `func UnresolvedFromLog() int`

- [ ] **Step 1: 写 `/diagnose errors` 的失败测试（追加到 `internal/command/diagnose_audit_test.go`）**

```go
// /diagnose errors shows one line per coded problem and model with its
// count, latest time, hint and what a remedy did — not one raw line per
// occurrence, and never without a hint for a coded problem.
func TestDiagnoseErrorsGroupsAndExplains(t *testing.T) {
	diagnostic.ResetForTest()
	t.Cleanup(diagnostic.ResetForTest)
	overflow := fmt.Errorf("api: %w", &api.StatusError{Status: 400,
		Msg: `request (17964 tokens) exceeds the available context size (16384 tokens)`})
	diagnostic.Report(overflow, diagnostic.Context{Model: "qwen3.6-27b", Provider: "openai-compatible"})
	diagnostic.Report(fmt.Errorf("api: %w", &api.StatusError{Status: 400,
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
```

`import` 增加 `"encoding/json"`, `"fmt"`, `"github.com/liuzhixin405/cove/internal/api"`, `"github.com/liuzhixin405/cove/internal/diagnostic"`。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/command/ -run 'TestDiagnoseErrors' 2>&1 | head -8`
Expected: 输出格式断言失败（缺 `×2` 或 `/continue`）。

- [ ] **Step 3: 重写 `showRuntimeErrors` 与 `runtimeReminder`（`internal/command/diagnose.go`）**

```go
// showRuntimeErrors lists problems recorded while the agent was running,
// merged with persisted events from previous runs: one line per coded
// problem and model (or per uncoded message), with count, latest time, the
// catalogue's hint and what a remedy did.
func (c *diagnoseCmd) showRuntimeErrors() (Output, error) {
	events := diagnostic.RecentRuntime()
	if len(events) == 0 {
		events = diagnostic.LoadRuntimeLog()
	}
	if len(events) == 0 {
		return Output{Message: "\x1b[32m✓ 没有记录到运行时错误或卡顿。\x1b[0m\n"}, nil
	}
	summaries := diagnostic.SummarizeRuntime(events)
	const reset = "\x1b[0m"
	var sb strings.Builder
	fmt.Fprintf(&sb, "\x1b[1m运行时问题记录\x1b[0m (共 %d 条，按严重程度排序)\n\n", len(events))
	for _, s := range summaries {
		count := ""
		if s.Count > 1 {
			count = fmt.Sprintf(" \x1b[2m×%d\x1b[0m", s.Count)
		}
		last := ""
		if !s.Last.IsZero() {
			last = fmt.Sprintf("   \x1b[2m最近 %s\x1b[0m", s.Last.Format("01-02 15:04"))
		}
		code := ""
		if s.Code != "" {
			code = string(s.Code) + " "
		}
		fmt.Fprintf(&sb, " %s[%s]%s %s%s%s%s\n", s.Severity.Color(), s.Severity.String(), reset, code, s.Message, count, last)
		if s.Code != "" {
			ctx := ""
			if s.Model != "" {
				ctx = "模型 " + s.Model
			}
			if s.Detail != "" {
				if ctx != "" {
					ctx += " · "
				}
				ctx += textutil.ClipRunes(s.Detail, 100)
			}
			if ctx != "" {
				fmt.Fprintf(&sb, "    \x1b[2m%s\x1b[0m\n", ctx)
			}
		}
		if s.Recovery != "" {
			fmt.Fprintf(&sb, "    \x1b[33m💡 %s\x1b[0m\n", s.Recovery)
		}
		for _, a := range s.Applied {
			fmt.Fprintf(&sb, "    \x1b[32m✓ 已处置：%s\x1b[0m\n", a)
		}
	}
	sb.WriteString("\n\x1b[2m日志文件: ~/.cove/errors.log · 处理完可用 /diagnose archive 归档\x1b[0m\n")
	return Output{Message: sb.String()}, nil
}
```

`runtimeReminder` 的每行改为同样带 `s.Code` 前缀（`code + s.Message`）；其余不变。`import` 增加 `"github.com/liuzhixin405/cove/internal/textutil"`。`archiveRuntimeLog` 在归档成功后调用 `diagnostic.ResetRemedyState()`。

- [ ] **Step 4: 运行 command 包**

Run: `go test ./internal/command/ 2>&1 | tail -4`
Expected: PASS。

- [ ] **Step 5: 启动提示的失败测试（追加到 `internal/diagnostic/report_test.go`）**

```go
// The start-up hint counts coded ERROR+ problems with a hint that no remedy
// resolved afterwards; a resolved code, an uncoded line and a warning do
// not count.
func TestUnresolvedCountsCodedProblemsWithoutARemedy(t *testing.T) {
	events := []RuntimeEvent{
		{Severity: SevError, Code: ErrAPIContextLength, Model: "a", Source: "report"},
		{Severity: SevRecovered, Code: ErrAPIContextLength, Model: "a", Source: "remedy"},
		{Severity: SevError, Code: ErrAPIProviderUnavailable, Model: "a", Source: "report"},
		{Severity: SevError, Code: ErrAPIProviderUnavailable, Model: "a", Source: "report"},
		{Severity: SevWarning, Code: ErrToolArgsInvalid, Model: "a", Source: "report"},
		{Severity: SevError, Message: "loop detected", Source: "log"},
	}
	if got := unresolvedIn(events); got != 1 {
		t.Errorf("unresolved = %d, want 1 (E2009 only)", got)
	}
}
```

- [ ] **Step 6: 实现（追加到 `internal/diagnostic/recorder.go`）**

```go
// UnresolvedFromLog counts, in the persisted log, the coded problems of
// severity ERROR or worse that have a hint and were not resolved by a remedy
// afterwards; the start-up hint points at /diagnose errors when it is not 0.
func UnresolvedFromLog() int { return unresolvedIn(LoadRuntimeLog()) }

func unresolvedIn(events []RuntimeEvent) int {
	type key struct {
		code  ErrorCode
		model string
	}
	open := map[key]bool{}
	for _, ev := range events {
		if ev.Code == "" {
			continue
		}
		k := key{ev.Code, ev.Model}
		switch {
		case ev.Severity == SevRecovered:
			delete(open, k)
		case ev.Severity >= SevError && ev.Severity != SevRecovered:
			if def := registry[ev.Code]; def != nil && def.Recovery != "" {
				open[k] = true
			}
		}
	}
	return len(open)
}
```

注意 `SevRecovered` 的枚举值大于 `SevFatal`，所以 `ev.Severity >= SevError` 的分支必须排除它（上面已排除）。

- [ ] **Step 7: 启动提示（`cli/cove/app_bootstrap.go` `startupDiagnosticsText`）**

```go
func startupDiagnosticsText(cfg *config.Config, debugMode bool) string {
	if issues := diagnostic.QuickCheck(cfg); len(issues) > 0 {
		return "\n  \x1b[90m⚠️  系统检测到潜在环境或配置异常，建议输入 \x1b[36m/diagnose\x1b[90m 查看。\x1b[0m\n"
	}
	if n := diagnostic.UnresolvedFromLog(); n > 0 {
		return fmt.Sprintf("\n  \x1b[90m⚠️  错误日志里有 %d 类未处理的问题，输入 \x1b[36m/diagnose errors\x1b[90m 查看建议，处理完可用 /diagnose archive 归档。\x1b[0m\n", n)
	}
	return ""
}
```

（去掉原文案里的「一键修复」：静态检查器只有会话清理是自动的，其余是建议。）

- [ ] **Step 8: 运行 diagnostic 与 cli 包**

Run: `gofmt -l ./internal ./cli; go vet ./... && go test ./internal/diagnostic/ ./cli/cove/ 2>&1 | tail -4`
Expected: PASS。

- [ ] **Step 9: 文档**

`docs/USER_MANUAL.md` 诊断系统一节：
- 「诊断码体系」表格改为按实际类别：E1 配置、E2 API/网络（含 E2008 上下文超出模型窗口、E2009 供应商已被标记不可用）、E3 权限、E4 工具（含 E4009 工具参数非法 JSON）、E5 引擎（含 E5007 模型调用无进展）、E6 会话/文件系统。
- 删除「所有修复都是 **HotFixable**，无需重启即可应用。」。
- 新增小节「自动处置」：

  > 运行期错误按类型归类得到诊断码并记入 `~/.cove/errors.log`，`/diagnose errors` 按码和模型聚合，显示次数、最近时间、建议和已执行的处置。部分诊断码带处置器，在错误发生时自动执行并提示，只改本会话的运行策略与参数，不改代码、不改 `config.json`：
  > - **E2008 上下文超出模型窗口**：从服务端报错解析真实窗口（llama.cpp 的 `n_ctx`、OpenAI 的 `maximum context length is N` 等），小于 cove 的估算时当场改为该值，后续压缩按真实窗口触发，并提示在 `config.json` 加 `context_window` 固定。
  > - **E4009 工具参数非法 JSON**：同一模型第 3 次出现时提示换模型或降低 temperature，之后每 5 次提示一次。
  >
  > 静态检查（`/diagnose`）里只有「会话完整性」会自动清理损坏文件，其余检查给出建议。启动时若错误日志里有未处置的错误，提示一行。
- 配置表加一行：`| \`context_window\` | number | 当前 \`model\` 的上下文窗口（token）。用于 cove 认不出的模型（本地 llama.cpp 等），决定压缩触发点；0 或不设按模型名估算。E2008 的处置提示会给出应填的值 |`。
- `/diagnose codes` 说明改为「列出所有诊断码，带处置器的标注 (有处置器)」。

`CHANGELOG.md` 在 `### Added` 顶部加：

> - **诊断系统重构**：运行期错误在产生处按类型归类（状态码、上下文超长、限流、传输、工具参数非法、卡住、供应商不可用），不再靠中文标题在错误文本里找子串；新增诊断码 E2008/E2009/E4009/E5007；`/diagnose errors` 按码和模型聚合，显示次数、最近时间、建议与已处置记录；新增「自动处置」：E2008 从服务端报错学习真实上下文窗口并调整本会话压缩预算（提示写入新配置键 `context_window`），E4009 累计到 3 次提示换模型。删除名不副实的 `AutoFixable`/`HotFixable` 标志与「所有修复已热加载」提示。启动时若错误日志有未处置的错误提示一行。

- [ ] **Step 10: 最终验证**

Run: `gofmt -l ./internal ./cli; go build ./... && go vet ./... && go test ./... 2>&1 | grep -v '^ok' | grep -v 'no test files' | tail -10`
Expected: 只剩 `internal/config` 的既有品牌测试失败。

---

## Self-Review

**Spec coverage**：3.1 结构化入口 → Task 2；3.2 归类表 → Task 1/2（E5001 `LimitError` 不经 `Report` 产生，spec 表中该行按实际情况不实现，已在 Task 2 的 `Classify` 中省略，spec 第 3.2 节的这一行应视为「保持现状」）；3.3 目录 → Task 2 Step 3；3.4 产生处 → Task 4；3.5 处置器 → Task 3；3.6 配置 → Task 4 Step 8-9；3.7 报告与启动提示 → Task 5；3.8 文档 → Task 5 Step 9；4 错误处理 → Task 2 `applyRemedy`、Task 3 `Declines` 测试；5 测试 → 各任务 Step 1。

**Placeholder scan**：Task 2 Step 4 `record` 中「原有的轮转与追加逻辑，原样搬入」指的是 `recorder.go:94-109` 现有代码块，执行者直接剪切粘贴；Task 4 Step 1 对 `mockProvider` 字段名的说明是对现有测试夹具的引用，不是待定项。

**Type consistency**：`RuntimeSummary` 字段在 Task 2 定义、Task 5 使用一致；`diagnostic.Context` 字段 `Provider, Model, Tool, Attempt`；`Runtime` 两个方法在 Task 2 定义、Task 3 使用、Task 4 实现；`ResetForTest` 在 Task 4 Step 1 定义，Task 5 Step 1 使用。

**Review Focus 覆盖**：1 → Task 4 `TestContextOverflowIsReportedOnceWithItsCode`、Task 2 `Source` 断言；2 → Task 3 `TestContextWindowRemedyDeclines` 首段；3、4 → Task 3 `TestParseContextWindow` 与 `Declines`；5 → Task 5 `TestDiagnoseErrorsAcceptsOldLogLines`。
