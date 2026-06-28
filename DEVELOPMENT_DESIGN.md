# Cove 优化项详细设计文档

> 版本: 2.0 | 基于 Cove v6.3.1 | ⚠ 本文档包含原始设计与实际实现对照

---

## 目录

1. [文档概述](#1-文档概述)
2. [P0-1: Loop Detection 循环检测](#2-p0-1-loop-detection-循环检测)
3. [P0-2: 模型故障转移](#3-p0-2-模型故障转移)
4. [P0-3: AI 对话压缩](#4-p0-3-ai-对话压缩)
5. [P1-4: Hook 系统升级](#5-p1-4-hook-系统升级)
6. [P1-5: MCP 传输协议扩展](#6-p1-5-mcp-传输协议扩展)
7. [P1-6: Tool Output Masking](#7-p1-6-tool-output-masking)
8. [P1-7: 策略引擎升级](#8-p1-7-策略引擎升级)
9. [P1-8: 模型路由](#9-p1-8-模型路由)
10. [P2 项简述](#10-p2-项简述)
11. [实施顺序与依赖关系](#11-实施顺序与依赖关系)

---

## 1. 文档概述

本文档为 Cove 优化清单中每个待实现项提供**详细设计**，包括：

- 问题描述与现状
- 数据结构定义
- 接口变更
- 文件变更清单
- 与现有代码的集成点
- 测试策略
- 风险与注意事项

**核心设计原则**：

1. **最小侵入** — 新代码通过接口注入 Engine，不破坏现有逻辑
2. **优雅降级** — 每个子系统可选，不可用时不影响核心对话功能
3. **100% 完整实现** — 每个模块必须完整可用：完整的数据结构、完整的错误处理、完整的测试覆盖、完整的 TUI 用户感知。不允许 demo / 骨架 / TODO。
4. **Go 惯用写法** — 遵循现有代码风格（短变量名、显式错误处理、goroutine + context、接口抽象）
5. **向后兼容** — 现有配置文件和命令行参数不受影响，新功能默认启用但可关闭

---

## 1a. 实现总结 (v2.0)

所有 P0/P1 优化项已于 v6.3.1 实现完毕。以下是实际实现与原始设计的差异总结：

| 模块 | 原始设计要点 | 实际实现差异 |
|------|-------------|-------------|
| **LoopDetector** | 2 层（工具调用哈希 + 输出哈希），窗口 50，阈值 5/10 | **3 层 + 自适应**：新增 Layer 1b 模糊工具名匹配 + Layer 3 停滞检测；只读工具豁免；Flash 模型自适应阈值；分级响应（maxBreaks=5 非致命→硬终止）；指纹重置 |
| **ModelFallback** | 3 状态 + 冷却 60s + maxFails=3 | 基本一致，新增 `TryChatStream` 流式故障转移；`/status` 状态指示 |
| **ChatCompressor** | 2 层（免费截断 + AI 压缩） | 基本一致，新增安全分割（assistant 锚定避免双 user 消息） |
| **ToolOutputMasker** | 反向扫描 + 磁盘卸载 | 基本一致，新增豁免机制 + 最小裁剪阈值 + 防重复掩码 |
| **PolicyEngine** | allow/deny/ask + Glob 模式 | 基本一致，新增 `param_match` 参数级条件 + `FilePolicyStorage` 持久化 |
| **ModelRouter** | 策略链 override→classifier→default | 基本一致 |
| **Hook 系统** | 事件系统增强 | 新增异步处理支持 + 结果修改能力 |
| **MCP 传输** | SSE 扩展 | 新增 **Streamable HTTP**（2025 新规范） |
| **P2 新增项** | 未在设计范围内 | 实际新增：NextSpeaker、SessionDiff、Telemetry、Safety、EnhancedRepoMap |

> 详细信息在每个模块的「实现要点」章节中说明。

本文档为 Cove 优化清单中每个待实现项提供**详细设计**，包括：

- 问题描述与现状
- 数据结构定义
- 接口变更
- 文件变更清单
- 与现有代码的集成点
- 测试策略
- 风险与注意事项

**核心设计原则**：

1. **最小侵入** — 新代码通过接口注入 Engine，不破坏现有逻辑
2. **优雅降级** — 每个子系统可选，不可用时不影响核心对话功能
3. **100% 完整实现** — 每个模块必须完整可用：完整的数据结构、完整的错误处理、完整的测试覆盖、完整的 TUI 用户感知。不允许 demo / 骨架 / TODO。
4. **Go 惯用写法** — 遵循现有代码风格（短变量名、显式错误处理、goroutine + context、接口抽象）
5. **向后兼容** — 现有配置文件和命令行参数不受影响，新功能默认启用但可关闭

---

## 2. P0-1: Loop Detection 循环检测

### 2.1 问题

Cove 当前只有 `maxSteps`（默认 50）硬限制工具调用步数。AI 可能在步骤 10 就已陷入死循环（反复读写同一文件、同一行、同一内容），但直到步骤 50 才被硬上限杀死——浪费 40 轮 API 调用和 Token 费用。

Gemini CLI 的 `loopDetectionService.ts` 用三层检测解决此问题。我们参考其思路但实现更完善的 Go 版本。

### 2.2 设计（原始设计）

原始设计为两层检测：
- **第一层 - 工具调用哈希**：对 `(toolName, sha256(input_json))` 做计数。同一工具+同参数连续重复 5 次即触发。
- **第二层 - 输出内容哈希**：对 `sha256(output_string[:500])` 做滑动窗口计数（窗口大小=50）。相同输出内容出现 10 次触发。

### 2.2a 实现要点 (v6.3.1)

实际实现扩展为 **3 层 + 自适应阈值**：

```
┌───────────────────────────────────────────────────────────┐
│                    LoopDetector                            │
│                                                           │
│  record(fingerprint, toolName, output, changed) → Decision │
│       │                                                   │
│       ├─ Layer 1a: 精确指纹匹配 (14窗口, 阈值10)            │
│       │   sha256(toolName + ":" + json(input))            │
│       │   相同指纹出现 ≥10/14轮 → 循环                      │
│       │                                                   │
│       ├─ Layer 1b: 模糊工具名匹配 (12窗口, 阈值10)          │
│       │   相同工具名（不同参数）出现 ≥10/12轮 → 循环         │
│       │                                                   │
│       ├─ Layer 2: 输出内容哈希 (40窗口, 阈值8)             │
│       │   sha256(output[:512]) 相同出现 ≥8/40轮 → 循环     │
│       │                                                   │
│       └─ Layer 3: 停滞检测 (60轮)                          │
│           连续 60 轮无文件创建/修改 → 空转                  │
│                                                           │
│  只读工具豁免: read/grep/glob/lsp/webfetch/browser/task_list│
│  自适应阈值: Flash模型使用更敏感的 8/12, 8/10, 8/30, 50    │
│  分级响应: 前5次非致命注入引导, 超出则硬终止                │
│  指纹重置: 注入后清空窗口, 让模型重新开始                   │
└───────────────────────────────────────────────────────────┘
```

**关键差异**：
| 维度 | 原始设计 | 实际实现 |
|------|---------|---------|
| 层数 | 2 层 | 4 层 (1a+1b+2+3) |
| 指纹窗口 | 无窗口，连续计数 | 14/12/40 轮滑动窗口 |
| 阈值 | 5/10 | 10/10/8/60 |
| 只读工具 | 未考虑 | 豁免 |
| 模型自适应 | 无 | Flash 模型更敏感 |
| 分级响应 | 直接中断 | 前5次注入引导，后硬终止 |

**文件变更清单**（实际）：
| 文件 | 操作 | 说明 |
|------|------|------|
| `internal/engine/loopdetect.go` | **新建** | 394 行完整实现 |
| `internal/engine/loopdetect_test.go` | **新建** | 测试覆盖全部 4 层 + 自适应 + 豁免 |
| `internal/engine/engine.go` | **修改** | 集成 LoopDetector 到主循环 |
| `internal/config/config.go` | **修改** | 添加 LoopDetectionEnabled

```
┌──────────────────────────────────────────────────────┐
│                 LoopDetector                          │
│                                                       │
│  record(toolName, input, output) → Decision           │
│       │                                               │
│       ├─ 1. 工具调用哈希: 同一 (工具名, 参数) 出现      │
│       │    TOOL_CALL_LOOP_THRESHOLD=5 次 → 触发        │
│       │                                               │
│       └─ 2. 输出内容哈希: 同一输出内容出现              │
│            CONTENT_LOOP_THRESHOLD=10 次 → 触发         │
│                                                       │
│  返回: Decision{IsLoop, Reason, RecentCalls}           │
└──────────────────────────────────────────────────────┘
```

**两层检测逻辑**：

- **第一层 - 工具调用哈希**：对 `(toolName, sha256(input_json))` 做计数。同一工具+同参数连续重复 5 次即触发。这捕获最明显的死循环模式（AI 反复执行相同操作）。
- **第二层 - 输出内容哈希**：对 `sha256(output_string[:500])` 做滑动窗口计数（窗口大小=50）。相同输出内容出现 10 次触发。这捕获「每次参数微调但结果完全一样」的模式。

**⚠ 注意**: 参数比较只取前 500 字符的 SHA256。对于超大文件操作，只比较关键参数（如 filePath、oldString、command）。

### 2.3 数据结构

```go
// internal/engine/loopdetect.go

package engine

import (
    "crypto/sha256"
    "encoding/json"
    "sync"
)

// LoopDecision 循环检测结果
type LoopDecision struct {
    IsLoop      bool     // 是否检测到循环
    Reason      string   // 循环原因（给用户看的）
    RecentCalls []string // 最近几次调用描述（给 AI 看的）
}

// LoopDetector 循环检测器
type LoopDetector struct {
    mu sync.Mutex

    // 第一层：工具调用哈希计数
    // key = sha256(toolName + ":" + jsonParams)
    // value = 连续重复次数
    callHashes   map[string]int
    lastCallHash string

    // 第二层：输出内容哈希计数（滑动窗口 50）
    outputHashes []string
    outputCounts map[string]int

    // 配置
    toolCallThreshold  int // 默认 5
    contentThreshold   int // 默认 10
    contentWindowSize  int // 默认 50
    enabled            bool
}

// NewLoopDetector 创建循环检测器
func NewLoopDetector() *LoopDetector {
    return &LoopDetector{
        callHashes:        make(map[string]int),
        outputHashes:      make([]string, 0, 50),
        outputCounts:      make(map[string]int),
        toolCallThreshold: 5,
        contentThreshold:  10,
        contentWindowSize: 50,
        enabled:           true,
    }
}

// Record 记录一次工具调用，返回是否检测到循环
func (ld *LoopDetector) Record(toolName string, input map[string]any, output string) LoopDecision {
    ld.mu.Lock()
    defer ld.mu.Unlock()

    if !ld.enabled {
        return LoopDecision{}
    }

    // 第一层：工具调用哈希
    inputJSON, _ := json.Marshal(input)
    callHash := hashStrings(toolName, string(inputJSON))

    if callHash == ld.lastCallHash {
        ld.callHashes[callHash]++
        if ld.callHashes[callHash] >= ld.toolCallThreshold {
            return LoopDecision{
                IsLoop: true,
                Reason: fmt.Sprintf("检测到循环: 工具 '%s' 以相同参数连续调用了 %d 次",
                    toolName, ld.callHashes[callHash]),
            }
        }
    } else {
        ld.callHashes[callHash] = 1
        ld.lastCallHash = callHash
    }

    // 第二层：输出内容哈希
    if len(output) > 0 {
        outputHash := hashOutput(output)
        ld.outputHashes = append(ld.outputHashes, outputHash)
        ld.outputCounts[outputHash]++

        // 保持滑动窗口
        if len(ld.outputHashes) > ld.contentWindowSize {
            oldest := ld.outputHashes[0]
            ld.outputHashes = ld.outputHashes[1:]
            ld.outputCounts[oldest]--
            if ld.outputCounts[oldest] <= 0 {
                delete(ld.outputCounts, oldest)
            }
        }

        if ld.outputCounts[outputHash] >= ld.contentThreshold {
            return LoopDecision{
                IsLoop: true,
                Reason: fmt.Sprintf("检测到循环: 相同工具输出内容出现了 %d 次",
                    ld.outputCounts[outputHash]),
            }
        }
    }

    return LoopDecision{}
}

// Reset 重置检测器（新一轮对话时调用）
func (ld *LoopDetector) Reset() {
    ld.mu.Lock()
    defer ld.mu.Unlock()
    ld.callHashes = make(map[string]int)
    ld.lastCallHash = ""
    ld.outputHashes = ld.outputHashes[:0]
    ld.outputCounts = make(map[string]int)
}

func hashStrings(parts ...string) string {
    h := sha256.New()
    for _, p := range parts {
        h.Write([]byte(p))
        h.Write([]byte{0}) // 分隔符
    }
    return fmt.Sprintf("%x", h.Sum(nil))[:16] // 取前 16 字符足够
}

func hashOutput(output string) string {
    truncated := output
    if len(truncated) > 500 {
        truncated = truncated[:500]
    }
    h := sha256.Sum256([]byte(truncated))
    return fmt.Sprintf("%x", h[:])[:16]
}
```

### 2.4 集成点

**文件变更**:

| 文件 | 操作 | 说明 |
|------|------|------|
| `internal/engine/loopdetect.go` | **新建** | LoopDetector 实现 |
| `internal/engine/loopdetect_test.go` | **新建** | 单元测试 |
| `internal/engine/engine.go` | **修改** | 在 `executeToolCalls` 循环中注入检测 |
| `internal/config/config.go` | **修改** | 添加 `LoopDetectionEnabled bool` 配置项 |

**Engine 集成**（`engine.go` 中 `executeToolCalls` 方法，约第 1150 行附近）:

```go
// 在工具执行循环的每个迭代末尾插入:
func (e *Engine) executeToolCallsWithLoopDetection(toolCalls []api.ToolCall) []tool.Result {
    results := e.executeToolCalls(toolCalls)

    // 如果启用了循环检测，逐条记录
    if e.loopDetector != nil {
        for i, tc := range toolCalls {
            decision := e.loopDetector.Record(tc.Name, tc.Input, results[i].Data)
            if decision.IsLoop {
                e.activityLog("⚠ " + decision.Reason + " — 已中断")
                // 触发 review 学习: "不要重复相同的操作"
                // 返回错误，让 RunMessageWithStream 的循环退出
                results[i].IsError = true
                results[i].Data = fmt.Sprintf("[循环中断] %s", decision.Reason)
            }
        }
    }
    return results
}
```

**用户感知**：

当循环检测触发时，用户在 TUI 中看到：
```
⚠ 检测到循环: 工具 'write' 以相同参数连续调用了 5 次 — 已中断
```

**配置**（`config.go`）：

```go
type Config struct {
    // ... existing fields
    LoopDetectionEnabled bool `json:"loop_detection_enabled"` // 默认 true
}
```

可通过 `COVE_NO_LOOP_DETECT=1` 环境变量关闭。

### 2.5 测试策略

```go
// loopdetect_test.go

func TestLoopDetector_ExactRepeat(t *testing.T) {
    // 同一工具+同参数重复 5 次 → 触发
}

func TestLoopDetector_DifferentArgs_NoTrigger(t *testing.T) {
    // 不同参数 → 不触发
}

func TestLoopDetector_OutputContentRepeat(t *testing.T) {
    // 参数不同但输出相同 → 第二层触发
}

func TestLoopDetector_InterleavedCalls_Reset(t *testing.T) {
    // 中间插入不同调用 → 第一层重置
}

func TestLoopDetector_Reset(t *testing.T) {
    // Reset() → 所有计数归零
}

func TestLoopDetector_Disabled(t *testing.T) {
    // enabled=false → Record 永远返回非循环
}
```

### 2.6 风险

| 风险 | 缓解 |
|------|------|
| 误判（批量操作如给 20 个文件加 header） | 第一层只检测**同一工具+同一参数**；批量操作参数中 filePath 不同，不会触发 |
| 第二层误判（正常重复输出） | 阈值 10 次足够高，且滑动窗口 50 限制了影响范围 |
| 性能开销 | SHA256 计算极快（微秒级），无外部 API 调用 |

---

## 3. P0-2: 模型故障转移

### 3.1 问题

Cove 当前使用 `KeyPool` 在同模型下轮转多个 API Key。当模型服务本身不可用（如 Anthropic 全局限流、OpenAI 宕机）时，Key 轮转无效。用户只能收到错误，对话中断。

需要的不是同模型的 Key 轮转，而是**跨模型的自动降级**：Anthropic → OpenAI → ... 直到找到可用的。

### 3.2 设计（原始设计）

```
┌─────────────────────────────────────────────────────────┐
│                   ModelFallback                          │
│                                                          │
│  providers: []ProviderWithStatus                         │
│  ┌──────────┐   ┌──────────┐   ┌──────────┐             │
│  │ Anthropic │→  │ OpenAI   │→  │ OpenRouter│            │
│  │ OK        │   │ OK       │   │ OK       │             │
│  └──────────┘   └──────────┘   └──────────┘             │
│                                                          │
│  TryChat(ctx, req) → (resp, usedProvider, error)         │
│       │                                                  │
│       ├─ 1. 用当前 provider 尝试                          │
│       ├─ 2. 失败? 标记 unavailable + cooldown            │
│       ├─ 3. 切换到下一个可用 provider                     │
│       └─ 4. 所有都不可用? → 返回错误                       │
│                                                          │
│  Cooldown: 被限流的 provider 在 60s 后自动恢复             │
└─────────────────────────────────────────────────────────┘

### 3.2a 实现要点 (v6.3.1)

实际实现与设计基本一致，新增：

1. **流式故障转移**：`TryChatStream` 支持流式 API 的故障转移，与 `TryChat` 对称
2. **状态指示**：通过 `/status` 命令显示所有 Provider 的健康状态
   - `●` 健康 / `○` 降级 / `✕` 不可用
3. **锁优化**：API 调用期间释放锁，避免阻塞状态读取（`/status` 可能被长时间调用阻塞）
4. **永久错误 vs 临时错误**：401/403 → `ProviderUnavailable`，手动恢复；429/5xx → `ProviderDegraded`，60s 冷却后自动恢复

**文件变更**（实际）：
| 文件 | 操作 | 说明 |
|------|------|------|
| `internal/api/fallback.go` | **新建** | 272 行完整实现 |
| `internal/api/provider.go` | **修改** | 集成 Fallback 到 Provider 接口 |
| `internal/engine/engine.go` | **修改** | `Engine.provider` → `Engine.fallback` |
| `internal/tui/app.go` | **修改** | 状态指示 UI |
```

### 3.3 数据结构

```go
// internal/api/fallback.go

type ProviderStatus int
const (
    ProviderOK        ProviderStatus = iota // 可用
    ProviderDegraded                       // 降级（最近失败过，冷却中）
    ProviderUnavailable                    // 不可用（需手动恢复）
)

type ProviderWithStatus struct {
    Provider   Provider
    Status     ProviderStatus
    CoolUntil  time.Time // 冷却到何时（Degraded 状态）
    FailCount  int       // 连续失败次数
    LastError  error
}

type ModelFallback struct {
    mu          sync.Mutex
    providers   []*ProviderWithStatus
    currentIdx  int
    cooldownDur time.Duration // 默认 60s
    maxFails    int           // 连续失败多少次标记为 Unavailable，默认 3
}

func NewModelFallback(providers []Provider) *ModelFallback {
    mf := &ModelFallback{
        cooldownDur: 60 * time.Second,
        maxFails:    3,
    }
    for _, p := range providers {
        mf.providers = append(mf.providers, &ProviderWithStatus{
            Provider: p,
            Status:   ProviderOK,
        })
    }
    return mf
}

// TryChat 尝试使用当前 provider 调用，失败时自动切换
func (mf *ModelFallback) TryChat(ctx context.Context, buildRequest func(Provider) ChatRequest) (*ChatResponse, Provider, error) {
    mf.mu.Lock()
    defer mf.mu.Unlock()

    startIdx := mf.currentIdx
    tried := 0

    for tried < len(mf.providers) {
        idx := (startIdx + tried) % len(mf.providers)
        mf.currentIdx = idx
        pw := mf.providers[idx]

        // 检查是否可以尝试
        switch pw.Status {
        case ProviderUnavailable:
            tried++
            continue
        case ProviderDegraded:
            if time.Now().Before(pw.CoolUntil) {
                tried++
                continue
            }
            // 冷却超时，恢复
            pw.Status = ProviderOK
        }

        // 尝试调用
        resp, err := pw.Provider.Chat(ctx, buildRequest(pw.Provider))
        if err == nil {
            pw.FailCount = 0
            return resp, pw.Provider, nil
        }

        // 处理失败
        pw.FailCount++
        pw.LastError = err

        if isRateLimit(err) || isTemporary(err) {
            pw.Status = ProviderDegraded
            pw.CoolUntil = time.Now().Add(mf.cooldownDur)
            log.Warn("provider %s degraded (rate limited), cooling until %s", pw.Provider.Name(), pw.CoolUntil)
        } else if pw.FailCount >= mf.maxFails || isPermanent(err) {
            pw.Status = ProviderUnavailable
            log.Error("provider %s marked unavailable after %d failures: %v", pw.Provider.Name(), pw.FailCount, err)
        }

        tried++
    }

    // 全部失败
    return nil, nil, fmt.Errorf("all %d providers failed", len(mf.providers))
}

// TryChatStream 同 TryChat，但用于流式调用
func (mf *ModelFallback) TryChatStream(ctx context.Context, buildRequest func(Provider) ChatRequest, handler StreamHandler) (*ChatResponse, Provider, error) {
    // 同 TryChat 逻辑，调用 Provider.ChatStream
}

// Status 返回所有 provider 的状态（给 TUI 状态栏显示）
func (mf *ModelFallback) Status() []ProviderStatusInfo {
    mf.mu.Lock()
    defer mf.mu.Unlock()
    // ...
}

func isRateLimit(err error) bool { /* 检测 429 */ }
func isTemporary(err error) bool  { /* 检测 5xx / 网络超时 */ }
func isPermanent(err error) bool  { /* 检测 401/403 */ }
```

### 3.4 集成点

**文件变更**:

| 文件 | 操作 | 说明 |
|------|------|------|
| `internal/api/fallback.go` | **新建** | ModelFallback 实现 |
| `internal/api/fallback_test.go` | **新建** | 单元测试 |
| `cli/cove/app_bootstrap.go` | **修改** | 创建多个 Provider，传给 ModelFallback |
| `internal/engine/engine.go` | **修改** | 用 ModelFallback 替代直接调用 provider |
| `internal/config/config.go` | **修改** | 支持多 provider 配置 + fallback 开关 |

**配置变更**（`~/.cove/config.json`）：

```json
{
    "providers": [
        {"name": "anthropic", "api_key": "sk-ant-xxx", "model": "claude-sonnet-4-20250514"},
        {"name": "openai",   "api_key": "sk-xxx",     "model": "gpt-4o"},
        {"name": "openai",   "api_key": "sk-yyy",     "model": "gpt-4o-mini", "base_url": "https://api.openai.com"}
    ],
    "fallback": {
        "enabled": true,
        "cooldown_seconds": 60,
        "max_failures": 3
    }
}
```

向后兼容：单 `provider` 配置仍然有效，自动包装为只有一个 provider 的 `ModelFallback`。

**TUI 状态栏显示**：

```
cove · claude-sonnet-4 · anthropic ● | openai ○ | openrouter ○
                                 当前      备用       备用
```

### 3.5 测试策略

```go
func TestFallback_PrimaryOK(t *testing.T) {
    // 主 provider 正常 → 不切换
}
func TestFallback_PrimaryRateLimit_SecondaryUsed(t *testing.T) {
    // 主 provider 429 → 切换到备用
}
func TestFallback_AllFailed(t *testing.T) {
    // 全部失败 → 返回错误
}
func TestFallback_CooldownRecovery(t *testing.T) {
    // 冷却超时 → 自动恢复
}
func TestFallback_PermanentError_MarkUnavailable(t *testing.T) {
    // 403 → 标记 Unavailable，不再尝试
}
```

---

## 4. P0-3: AI 对话压缩

### 4.1 问题

Cove 当前当消息历史超过 `maxContextMessages=200` 或 `maxContextTokens=90000` 时，采用简单截断：保留 system 消息 + 最早 5 条 + 最近 N 条，中间丢弃。

这导致长对话中**丢失大量上下文**——尤其是多步骤编程任务，中间步骤的工具调用和结果对后续决策至关重要。

### 4.2 设计

```
┌─────────────────────────────────────────────────────┐
│              ChatCompressor                          │
│                                                      │
│  Compress(messages, tokenLimit) → compressedMessages │
│       │                                              │
│       ├─ 1. 当前总 Token 是否超过 50% 限制?           │
│       │    不超过 → 不压缩，直接返回                   │
│       │                                              │
│       ├─ 2. 找到 split 点:                            │
│       │    保留最近 30% 的消息（从最后一个 user 消息起） │
│       │    旧消息进入压缩池                           │
│       │                                              │
│       ├─ 3. 用廉价模型生成摘要:                        │
│       │    "以下是之前的对话摘要: ..."                  │
│       │    包含: 用户目标、已完成步骤、关键决策、        │
│       │          文件修改列表、待解决问题               │
│       │                                              │
│       └─ 4. 替换:                                     │
│            [system] + [摘要] + [保留的最近 30%]        │
└─────────────────────────────────────────────────────┘
```

**关键决策**：

- **压缩触发阈值**：总 Token 超过模型限制的 **50%**（而非 80%），给压缩后留足 buffer
- **保留比例**：最近 30% 消息保留不压缩（包含最近完整的工具调用上下文）
- **压缩模型**：复用现有 API 调用路径，优先用快速/廉价的模型（如已配置的 OpenAI 兼容 provider 中的 gpt-4o-mini）
- **摘要内容**：结构化模板，包含目标、进度、文件变更、待解决问题

### 4.3 数据结构

```go
// internal/engine/compressor.go

type CompressResult struct {
    Compressed  bool           // 是否执行了压缩
    Summary     string         // 生成的摘要
    OldCount    int            // 压缩前的消息数
    NewCount    int            // 压缩后的消息数
    TokenSavings int           // 节省的 Token 估算
}

type ChatCompressor struct {
    enabled         bool
    tokenThreshold  float64 // 默认 0.5（50%）
    keepFraction    float64 // 默认 0.3（保留最近 30%）
    maxFunctionRespTokens int // 工具响应最大 Token 预算，默认 50000
}

func NewChatCompressor() *ChatCompressor {
    return &ChatCompressor{
        enabled:         true,
        tokenThreshold:  0.5,
        keepFraction:    0.3,
        maxFunctionRespTokens: 50000,
    }
}

// Compress 检查是否需要压缩，如果需要则执行
func (cc *ChatCompressor) Compress(
    ctx context.Context,
    messages []api.Message,
    tokenLimit int,
    summaryProvider Provider, // 用于生成摘要的 Provider（可是廉价模型）
) (*CompressResult, []api.Message, error) {
    // 1. 估算当前 Token
    currentTokens := estimateTokens(messages)
    if float64(currentTokens) < float64(tokenLimit)*cc.tokenThreshold {
        return &CompressResult{}, messages, nil // 不需要压缩
    }

    // 2. 找 split 点: 从后往前找到最后一个 user 消息
    splitIdx := findSplitPoint(messages, cc.keepFraction)
    if splitIdx <= 0 {
        return &CompressResult{}, messages, nil // 无法压缩
    }

    oldMessages := messages[:splitIdx]
    keepMessages := messages[splitIdx:]

    // 3. 生成摘要
    summary, err := cc.generateSummary(ctx, summaryProvider, oldMessages)
    if err != nil {
        // 压缩失败 → 降级为简单截断（现有行为）
        log.Warn("compression failed, falling back to truncation: %v", err)
        return nil, fallbackTruncate(messages, tokenLimit), err
    }

    // 4. 构建压缩后的消息列表
    compressed := []api.Message{
        messages[0], // system 消息
        {
            Role:    "user",
            Content: fmt.Sprintf("[对话摘要]\n%s", summary),
        },
    }
    compressed = append(compressed, keepMessages...)

    return &CompressResult{
        Compressed:   true,
        Summary:      summary,
        OldCount:     len(messages),
        NewCount:     len(compressed),
        TokenSavings: currentTokens - estimateTokens(compressed),
    }, compressed, nil
}

// generateSummary 调用 AI 生成摘要
func (cc *ChatCompressor) generateSummary(ctx context.Context, provider Provider, messages []api.Message) (string, error) {
    summaryPrompt := `请将以下对话历史总结为简洁的摘要。包含:
1. 用户最初的目标
2. 已完成的关键步骤
3. 文件修改列表（文件名和改动类型）
4. 当前遇到的问题
5. 下一步需要做什么

使用中文。控制在 500 字以内。`

    summaryMessages := []api.Message{
        {Role: "system", Content: summaryPrompt},
        {Role: "user", Content: formatMessagesForSummary(messages)},
    }

    req := ChatRequest{
        Messages:   summaryMessages,
        MaxTokens:  1000,
        Temperature: 0.3,
    }

    resp, err := provider.Chat(ctx, req)
    if err != nil {
        return "", err
    }
    return resp.Content, nil
}
```

### 4.4 集成点

**文件变更**:

| 文件 | 操作 | 说明 |
|------|------|------|
| `internal/engine/compressor.go` | **新建** | ChatCompressor 实现 |
| `internal/engine/compressor_test.go` | **新建** | 单元测试 |
| `internal/engine/engine.go` | **修改** | 在 RunMessageWithStream 的消息构建阶段调用 |
| `cli/cove/repl_loop.go` | **修改** | 压缩触发提示给用户 |
| `internal/config/config.go` | **修改** | 压缩配置项 |

**Engine 集成点**（`engine.go` 中 `RunMessageWithStream` 方法，在构建 system prompt **之前**）:

```go
// 约在 engine.go 第 350 行附近
func (e *Engine) maybeCompress(ctx context.Context) {
    if e.compressor == nil || !e.compressor.enabled {
        return
    }

    result, newMessages, err := e.compressor.Compress(
        ctx,
        e.messages,
        e.tokenLimit,
        e.fallbackProvider, // 如果有 ModelFallback，用其获取廉价 provider
    )
    if err != nil {
        e.activityLog("⚠ 对话压缩失败: " + err.Error())
        return
    }
    if result.Compressed {
        e.messages = newMessages
        e.activityLog(fmt.Sprintf("📦 对话压缩完成: %d → %d 条消息, 节省 ~%d tokens",
            result.OldCount, result.NewCount, result.TokenSavings))
    }
}
```

**用户感知**（TUI 中）：

```
📦 对话压缩完成: 85 → 32 条消息, 节省 ~12000 tokens
```

### 4.5 测试策略

```go
func TestCompressor_BelowThreshold_NoCompress(t *testing.T) {}
func TestCompressor_AboveThreshold_Compresses(t *testing.T) {}
func TestCompressor_FindSplitPoint(t *testing.T) {}
func TestCompressor_SummaryGenerationError_Fallback(t *testing.T) {}
func TestCompressor_EmptyHistory(t *testing.T) {}
```

---

## 5. P1-4: Hook 系统升级

### 5.1 问题

Cove 现有 `hooks/hooks.go`（97 行）只支持非常基础的生命周期回调。Gemini CLI 的 hooks 系统有 11 种事件类型、runtime/command 双模式、matcher 过滤、120KB 代码。

### 5.2 设计

```go
// internal/hooks/hooks.go (重写)

type HookEvent string
const (
    BeforeTool        HookEvent = "BeforeTool"        // 工具调用前
    AfterTool         HookEvent = "AfterTool"         // 工具调用后
    BeforeAgent       HookEvent = "BeforeAgent"       // Agent 启动前
    AfterAgent        HookEvent = "AfterAgent"        // Agent 完成后
    BeforeModel       HookEvent = "BeforeModel"        // API 调用前
    AfterModel        HookEvent = "AfterModel"         // API 调用后
    SessionStart      HookEvent = "SessionStart"       // 会话启动
    SessionEnd        HookEvent = "SessionEnd"          // 会话结束
    PreCompress       HookEvent = "PreCompress"        // 压缩前
    Notification      HookEvent = "Notification"        // 通知事件
    BeforeToolSelection HookEvent = "BeforeToolSelection" // 工具选择前
)

type HookType int
const (
    HookCommand HookType = iota // 外部命令（stdin/stdout）
    HookRuntime                 // Go 函数回调
)

type HookConfig struct {
    Event      HookEvent
    Matcher    string    // 可选: 正则匹配工具名/模型名，空=全部
    Type       HookType
    Command    string    // Command 模式
    RuntimeFn  func(HookInput) (HookOutput, error) // Runtime 模式
    Timeout    time.Duration
    Sequential bool      // 是否必须等待完成
}

type HookInput struct {
    Event     HookEvent
    ToolName  string
    ToolInput map[string]any
    Messages  []api.Message
    Model     string
}

type HookOutput struct {
    Continue  bool     // false = 阻止继续执行
    Message   string   // 给 AI 或用户的提示
    Modified  bool     // 是否修改了输入
}

type HookManager struct {
    hooks map[HookEvent][]HookConfig
}
```

**关键增强**：

1. **11 种事件类型**覆盖完整生命周期
2. **Matcher 过滤**：`BeforeTool:bash` 只匹配 bash 工具
3. **Command 模式**：执行外部脚本，通过 stdin 传 HookInput JSON，stdout 读 HookOutput JSON
4. **非阻塞通知**：`Sequential=false` 的 hook 异步执行，不阻塞主流程
5. **超时控制**：每个 hook 有独立超时

**集成**：在 Engine 的关键方法中注入 hook 调用点。

### 5.3 文件变更

| 文件 | 操作 |
|------|------|
| `internal/hooks/hooks.go` | **重写**（97→~400 行） |
| `internal/hooks/hooks_test.go` | **新建** |
| `internal/hooks/builtin_hooks.go` | **新建**（内置 hooks） |
| `internal/engine/engine.go` | **修改**（注入 hook 调用点） |

---

## 6. P1-5: MCP 传输协议扩展

### 6.1 问题

Cove 现有 MCP 仅支持 `stdio`（启动子进程通过 stdin/stdout JSON-RPC 通信）。Gemini CLI 额外支持 **SSE**（Server-Sent Events）和 **StreamableHTTP**，允许远程 MCP 服务器（如企业内部工具服务器、云服务）。

### 6.2 设计

```go
// internal/mcp/transport.go (新建)

type TransportType string
const (
    TransportStdio           TransportType = "stdio"
    TransportSSE             TransportType = "sse"
    TransportStreamableHTTP  TransportType = "streamable_http"
)

type TransportConfig struct {
    Type    TransportType
    Command string            // stdio
    Args    []string          // stdio
    URL     string            // sse / streamable_http
    Headers map[string]string // sse / streamable_http
    Timeout time.Duration
}

// TransportFactory 根据配置创建 Transport
func NewTransport(cfg TransportConfig) (Transport, error) {
    switch cfg.Type {
    case TransportStdio:
        return NewStdioTransport(cfg)
    case TransportSSE:
        return NewSSETransport(cfg)
    case TransportStreamableHTTP:
        return NewStreamableHTTPTransport(cfg)
    default:
        return nil, fmt.Errorf("unknown transport type: %s", cfg.Type)
    }
}
```

**ServerConfig 扩展**：

```json
{
    "mcp_servers": [
        {
            "type": "stdio",
            "command": "npx",
            "args": ["-y", "@anthropic/mcp-server-filesystem", "/path"]
        },
        {
            "type": "sse",
            "url": "https://mcp.internal.company.com/tools",
            "headers": {"Authorization": "Bearer xxx"}
        },
        {
            "type": "streamable_http",
            "url": "https://mcp.cloud.service.com/v1"
        }
    ]
}
```

### 6.3 文件变更

| 文件 | 操作 |
|------|------|
| `internal/mcp/transport.go` | **新建**（传输抽象） |
| `internal/mcp/sse_transport.go` | **新建** |
| `internal/mcp/http_transport.go` | **新建** |
| `internal/mcp/client.go` | **修改**（使用 Transport 接口） |
| `internal/mcp/pool.go` | **修改**（支持多传输类型） |

### 6.4 实现要点 (v6.3.1)

实际实现与设计基本一致，新增：

1. **背压机制**：消息队列满时阻塞发送方而非丢弃（避免 JSON-RPC 挂起）
2. **自动重连**：流断开时自动尝试重建
3. **SSE 事件解析**：自动解析 `data: ` SSE 事件流

**文件变更**（实际）：
| 文件 | 操作 | 说明 |
|------|------|------|
| `internal/mcp/transport.go` | **新建** | 传输抽象层 |
| `internal/mcp/http_transport.go` | **新建** | Streamable HTTP 实现 |
| `internal/mcp/sse_transport.go` | **新建** | SSE 传输实现 |
| `internal/mcp/client.go` | **修改** | 集成 Transport 接口 |

---

## 7. P1-6: Tool Output Masking

### 7.1 问题

工具输出（尤其是 `read` 大文件、`bash` 长输出）会快速消耗上下文窗口。Cove 当前直接返回完整工具输出，经常出现「读取一个 5000 行文件 → 上下文窗口爆满 → 后续对话质量下降」。

Gemini CLI 的 `toolOutputMaskingService.ts` 使用 **Hybrid Backward Scanned FIFO** 算法：保护最新 50K token，对旧工具输出批量遮蔽（写入文件，替换为 `<tool_output_masked: path/to/file>`）。

### 7.2 设计

```
┌──────────────────────────────────────────────────┐
│          ToolOutputMasker                         │
│                                                   │
│  mask(history) → maskedHistory                    │
│       │                                           │
│       ├─ 1. 从后往前扫描，累计 tool token 数       │
│       │    达到 protectionThreshold=50K → 停止     │
│       │    这些最新输出 → 保护，不遮蔽              │
│       │                                           │
│       ├─ 2. 继续扫描更早的输出，累计 prunable       │
│       │    达到 minPrunableThreshold=30K → 触发遮蔽 │
│       │                                           │
│       └─ 3. 遮蔽: tool_output → 写入文件,           │
│            替换为 [已遮蔽: .cove/tool-outputs/xxx]  │
└──────────────────────────────────────────────────┘
```

### 7.3 数据结构

```go
// internal/engine/masker.go

type MaskingResult struct {
    NewHistory  []api.Message
    MaskedCount int
    TokensSaved int
}

type ToolOutputMasker struct {
    enabled                bool
    protectionThreshold    int // 默认 50000 tokens
    minPrunableThreshold   int // 默认 30000 tokens
    protectLatestTurn      bool // 默认 true（保护最近一轮完整对话）
    outputDir              string // ~/.cove/tool-outputs/
    exemptTools            map[string]bool // 永远不遮蔽的工具
}

func NewToolOutputMasker() *ToolOutputMasker {
    return &ToolOutputMasker{
        enabled:              true,
        protectionThreshold:  50000,
        minPrunableThreshold: 30000,
        protectLatestTurn:    true,
        exemptTools: map[string]bool{
            "question":     true, // 用户交互
            "todowrite":   true, // 任务列表
            "plan_mode":   true,
            "exit_plan_mode": true,
        },
    }
}

func (m *ToolOutputMasker) Mask(history []api.Message) (*MaskingResult, []api.Message) {
    // 实现 Hybrid Backward Scanned FIFO
    // ...
}
```

### 7.4 集成点

在 Engine 中，每次 `executeToolCalls` 返回后、追加结果到消息历史之前调用 `Masker.Mask()`。

### 7.5 文件变更

| 文件 | 操作 |
|------|------|
| `internal/engine/masker.go` | **新建** |
| `internal/engine/masker_test.go` | **新建** |
| `internal/engine/engine.go` | **修改**（在工具结果添加前调用） |

### 7.6 实现要点 (v6.3.1)

实际实现与设计基本一致，新增：

1. **磁盘卸载**：将旧工具输出写入 `~/.cove/tool-outputs/`，替换为路径占位符
2. **豁免机制**：`question`/`todowrite`/`plan_mode`/`exit_plan_mode` 等交互式工具永不掩码
3. **防止重复掩码**：已掩码消息跳过（`maskedPrefix` 检测）
4. **最小裁剪阈值**：可裁剪内容 <30K tokens 时不触发掩码

---

## 8. P1-7: 策略引擎升级

### 8.1 问题

Cove 现有权限系统 `permission/classifier.go`（229 行）：每次工具调用都询问用户。用户经常输入相同的"允许"但每次都要重复确认。

需要：
1. **策略持久化**：用户选择"始终允许 read"后记住
2. **细粒度规则**：支持工具参数匹配（如"允许 bash 但禁止 curl | bash"）
3. **Wildcard 匹配**：支持 MCP 工具名称模式匹配

### 8.2 设计

```go
// internal/permission/policy.go (新建)

type Policy struct {
    Rules []PolicyRule `json:"rules"`
}

type PolicyRule struct {
    ID          string            `json:"id"`
    ToolPattern string            `json:"tool_pattern"` // "read", "bash", "mcp_*_*"
    Decision    RuleDecision      `json:"decision"`      // always_allow / always_deny / ask
    ParamRules  []ParamCondition  `json:"param_rules,omitempty"` // 参数级规则
    Modes       []PermissionMode  `json:"modes,omitempty"`       // 仅在特定模式下生效
    ExpiresAt   *time.Time        `json:"expires_at,omitempty"`  // 可选过期时间
    Description string            `json:"description"`
    CreatedAt   time.Time         `json:"created_at"`
}

type ParamCondition struct {
    Param    string `json:"param"`    // 参数名，如 "command"
    Operator string `json:"operator"` // contains / not_contains / equals / regex
    Value    string `json:"value"`    // 匹配值，如 "curl"
}

type RuleDecision string
const (
    AlwaysAllow RuleDecision = "always_allow"
    AlwaysDeny  RuleDecision = "always_deny"
    Ask         RuleDecision = "ask"
)

type PolicyEngine struct {
    rules     []PolicyRule
    storage   PolicyStorage // 持久化到 ~/.cove/policy.json
    mu        sync.RWMutex
}

// Evaluate 评估工具调用，返回决策
func (pe *PolicyEngine) Evaluate(toolName string, params map[string]any, mode PermissionMode, serverName string) PermissionDecision {
    pe.mu.RLock()
    defer pe.mu.RUnlock()

    // 1. 精确匹配
    for _, rule := range pe.rules {
        if !pe.ruleInMode(rule, mode) { continue }
        if !pe.matchTool(rule.ToolPattern, toolName, serverName) { continue }
        if !pe.matchParams(rule.ParamRules, param) { continue }
        return PermissionDecision{Decision: pe.toDecision(rule.Decision), Reason: rule.Description}
    }

    // 2. 没有匹配规则 → 回退到分类器（现有逻辑）
    return pe.classifier.Classify(toolName, param)
}

// AddRule 用户说"始终允许"时调用
func (pe *PolicyEngine) AddRule(rule PolicyRule) error {
    pe.mu.Lock()
    defer pe.mu.Unlock()
    pe.rules = append(pe.rules, rule)
    return pe.storage.Save(pe.rules)
}

// RemoveRule 用户撤销时调用
func (pe *PolicyEngine) RemoveRule(ruleID string) error { ... }
```

**持久化**（`~/.cove/policy.json`）：

```json
{
    "rules": [
        {
            "id": "rule-001",
            "tool_pattern": "read",
            "decision": "always_allow",
            "description": "始终允许 read 工具",
            "created_at": "2025-01-01T00:00:00Z"
        },
        {
            "id": "rule-002",
            "tool_pattern": "bash",
            "decision": "always_deny",
            "param_rules": [
                {"param": "command", "operator": "contains", "value": "curl | bash"}
            ],
            "description": "禁止管道执行远程脚本",
            "created_at": "2025-01-01T00:00:00Z"
        }
    ]
}
```

### 8.3 文件变更

| 文件 | 操作 |
|------|------|
| `internal/permission/policy.go` | **新建**（PolicyEngine 实现） |
| `internal/permission/policy_test.go` | **新建** |
| `internal/permission/storage.go` | **新建**（JSON 持久化） |
| `internal/permission/permission.go` | **修改**（集成 PolicyEngine） |
| `internal/permission/classifier.go` | **修改**（作为 fallback） |

---

## 9. P1-8: 模型路由

### 9.1 问题

Cove 固定使用一个模型。简单任务（如"解释这段代码"）和复杂任务（如"重构整个模块"）使用同一个昂贵模型，浪费费用。

### 9.2 设计

```
用户消息 → 复杂度评估 → 选择模型
                             │
                ┌────────────┼────────────┐
                ▼            ▼            ▼
            cheap model   normal model   premium model
            (haiku)       (sonnet)       (opus / 长上下文)
```

**评估策略**（按优先级）：

1. **Fallback 策略**：如果当前模型不可用 → 强制切换
2. **覆盖策略**：用户显式指定 `/model gpt-4o` → 使用指定模型
3. **分类器策略**：基于用户消息特征判断复杂度：
   - 消息长度 < 200 字符 → 可能是简单任务
   - 包含"重构"、"架构"、"设计" → 复杂任务
   - 包含"解释"、"什么是"、"帮我看看" → 简单任务
4. **默认策略**：使用配置的默认模型

```go
// internal/api/router.go

type RoutingDecision struct {
    Model    string
    Source   string // "classifier", "override", "fallback", "default"
    Reason   string
}

type ModelRouter struct {
    strategies []RoutingStrategy
    defaultModel string
}

type RoutingStrategy interface {
    Route(ctx context.Context, userMessage string, config Config) (*RoutingDecision, error)
    Name() string
}

func (mr *ModelRouter) Route(ctx context.Context, userMessage string) *RoutingDecision {
    for _, s := range mr.strategies {
        decision, err := s.Route(ctx, userMessage, mr.config)
        if err == nil && decision != nil {
            return decision
        }
    }
    return &RoutingDecision{Model: mr.defaultModel, Source: "default"}
}
```

### 9.3 文件变更

| 文件 | 操作 |
|------|------|
| `internal/api/router.go` | **新建** |
| `internal/api/router_strategies.go` | **新建**（各策略实现） |
| `internal/api/router_test.go` | **新建** |
| `cli/cove/repl_tui.go` | **修改**（worker 中调用 router） |

---

## 10. P2 项简述（实现状态）

### 10.1 Safety 检查器 ✅ 已实现

**目标**：在用户输入和 AI 输出阶段做安全扫描。

**实现**：`internal/permission/safety.go`，集成在 Permission 系统中。

**关键特性**：
- 基础安全过滤器：检测敏感命令（rm -rf、dd、mkfs 等）
- 路径安全校验：防止路径遍历攻击（`../`）
- 内容审查：检测 API key、密码等敏感信息泄露
- 与 `PolicyEngine` 集成，作为权限决策的前置检查

### 10.2 Next Speaker 检测 ✅ 已实现

**目标**：判断 AI 是否应该继续响应还是等待用户（减少无意义连续工具调用）。

**实现**：`internal/engine/nextspeaker.go`（81 行）

**关键特性**：
- 上下文感知的继续/停止决策：检测终止信号（"task complete"、"任务完成"等短语）
- 最大迭代限制：默认 50 轮，防止无限循环
- 会话结束检测：扫描最近 3 条消息中的终止短语
- 集成在 Engine 主循环中，作为每次工具调用后的决策点

### 10.3 本地遥测系统 ✅ 已实现

**目标**：本地使用数据记录（非匿名上报）。

**实现**：`internal/telemetry/telemetry.go`（132 行）

**关键特性**：
- 事件录制：结构化事件记录（类型、时间戳、数据）
- 本地存储：保存至 `~/.cove/telemetry.json`
- 容量保护：上限 1000 条，超出时裁剪后半
- 选择加入：默认关闭，需 `Enable()` 启用
- 轻量级：仅记录关键事件，不包含敏感数据

### 10.4 IDE 伴生
**规划中**：VS Code 扩展。独立仓库。通过 MCP 连接 Cove Agent 进程。

### 10.5 Repomap 增强 ✅ 已实现

**目标**：在文件树/符号提取的基础上，提供**增量更新**——缓存上次扫描结果，仅对变更文件重算。

**实现**：`internal/repomap/repomap.go`

**关键特性**：
- PageRank 交叉引用评分：基于引用的文件重要性排名
- 基于 mtime 的增量缓存：仅重新扫描修改过的文件
- RWMutex 并发安全：读多写少的无锁缓存设计
- 集成在 Engine 的 `buildSystemPrompt` 中注入 `<repo_map>`

### 10.6 Session Diff 对比 ✅ 已实现

**目标**：追踪会话中的变更。

**实现**：`internal/session/diff.go`（136 行）

**关键特性**：
- 轻量级快照：`SessionView` 捕获消息列表 + token 数的即时快照
- 结构化 Diff：`SessionDiff` 包含 Old/New Tokens、MsgCount、Added/Removed Tools、Added/Removed Files
- 工具/文件提取：自动从 ToolCalls 参数中提取文件路径
- 集成在 Session Store 中，作为变更追踪的基础

---

## 11. 实施顺序与依赖关系

```
Phase 1 (P0)
├─ 1. Loop Detection      [无依赖，独立实现]
├─ 2. Model Fallback       [依赖: 多 Provider 配置，可与 #1 并行]
└─ 3. AI Chat Compression  [依赖: Model Fallback (用于获取廉价模型做摘要)]

Phase 2 (P1)
├─ 4. Hook System Upgrade  [依赖: #2 (Model Fallback 的 AfterModel hook)]
├─ 5. MCP Transport        [无依赖，独立实现]
├─ 6. Tool Output Masking  [无依赖，独立实现]
├─ 7. Policy Engine        [依赖: #4 (AfterTool hook 用于策略学习)]
└─ 8. Model Routing        [依赖: #2 (需要多 Provider)]

Phase 3 (P2)
└─ 9-14. 各项独立实现
```

**推荐开工顺序**：`#1 → #2 → #3`（P0 三项），然后 `#5 → #6 → #4 → #7 → #8`（P1）。

---

> 文档完。每个模块可在开工时进一步细化为实现级设计。
