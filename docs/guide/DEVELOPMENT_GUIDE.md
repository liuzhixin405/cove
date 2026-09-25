# Cove Agent 完整开发指南

> **适用读者**: 新手开发者。本文档将带你从零开始理解整个 Cove Agent 项目的架构、设计理念和实现细节，读完即可上手开发。

---

## 目录

1. [项目概述](#1-项目概述)
2. [技术栈与依赖](#2-技术栈与依赖)
3. [目录结构总览](#3-目录结构总览)
4. [核心架构设计](#4-核心架构设计)
5. [模块详解](#5-模块详解)
   - [5.1 CLI 入口层](#51-cli-入口层)
   - [5.2 Engine 引擎层](#52-engine-引擎层)
   - [5.3 API 层](#53-api-层)
   - [5.4 Tool 工具系统](#54-tool-工具系统)
   - [5.5 Permission 权限系统](#55-permission-权限系统)
   - [5.6 Plan 计划系统](#56-plan-计划系统)
   - [5.7 Memory 记忆系统](#57-memory-记忆系统)
   - [5.8 Skills 技能系统](#58-skills-技能系统)
   - [5.9 Session 会话管理](#59-session-会话管理)
   - [5.10 MCP 集成](#510-mcp-集成)
   - [5.11 Browser 浏览器](#511-browser-浏览器)
   - [5.12 REPL 交互层](#512-repl-交互层)
   - [5.13 Context 上下文管理](#513-context-上下文管理)
   - [5.14 辅助模块](#514-辅助模块)
6. [核心数据流](#6-核心数据流)
7. [关键设计模式](#7-关键设计模式)
8. [如何运行与构建](#8-如何运行与构建)
9. [如何扩展系统](#9-如何扩展系统)
10. [测试策略](#10-测试策略)

---

## 1. 项目概述

Cove 是一个用 **Go 语言** 编写的 **AI 编程助手（Coding Agent）**，运行在终端（CLI）中。它的核心能力是：

- 与 LLM（大语言模型）对话，接收用户的自然语言编程任务
- 调用 **工具（Tools）** 来读写文件、执行命令、搜索代码等
- 通过 **计划（Plan）** 系统将复杂任务拆解为可执行步骤
- 通过 **权限（Permission）** 系统安全地控制危险操作
- 通过 **记忆（Memory）** 和 **技能（Skills）** 系统从对话中学习
- 支持 **MCP 协议** 扩展外部工具
- 支持 **多 Agent 协作**（子代理、团队）

**核心设计理念**: 单进程、事件驱动、权限可控、渐进式学习。

---

## 2. 技术栈与依赖

```
语言:          Go 1.22+
HTTP 客户端:   net/http（标准库）
终端 UI:       行式交互 REPL（internal/repl 行编辑器 + termui）+ Headless（非交互）
数据存储:      内存 + JSON 文件持久化（未来可能扩展 SQLite）
AI API:        Anthropic API（主）/ OpenAI 兼容 API
测试:          Go 标准 testing 包
```

**核心依赖（来自 go.mod）**:
- `github.com/liuzhixin405/cove` — 模块根路径
- Go 标准库为主，外部依赖极少（设计原则：最小依赖）

---

## 3. 目录结构总览

```
cove/agent/
├── go.mod                          # Go 模块定义
├── go.sum                          # 依赖校验
├── README.md                       # 项目说明
├── CHANGELOG.md                    # 版本变更日志
├── DEVELOPMENT_GUIDE.md            # 本文档
│
├── cli/                            # 命令行入口
│   └── cove/
│       ├── main.go                 # 程序入口（949行，核心启动逻辑）
│       ├── app_bootstrap.go        # 应用启动引导（196行）
│       ├── interactive.go          # useInteractiveShell()：选择 REPL 或 headless
│       ├── repl_loop.go            # runREPL() 交互主循环
│       ├── repl_tasks.go           # REPL 任务队列（排队/取消/失败重试）
│       ├── headless.go             # runHeadless() 非交互前端
│       ├── chat_interaction.go     # 单次交互处理（151行）
│       └── registry.go             # 工具注册（102行）
│
├── internal/                       # 内部包（核心实现）
│   ├── repl/                       # ★ 行编辑器（默认交互模式的输入行、历史、补全）
│   ├── render/                     # 工具块渲染 + Markdown 渐进渲染（markdown.go）
│   │
│   ├── api/                        # AI API 抽象层
│   │   ├── provider.go             # 统一 Provider 接口
│   │   ├── provider_catalog.go     # 内置 Provider 目录
│   │   ├── anthropic.go            # Anthropic API 实现
│   │   ├── openai_compat.go        # OpenAI 兼容 API 实现
│   │   ├── keypool.go              # API Key 池（轮转、故障转移）
│   │   ├── ratelimit.go            # 速率限制追踪
│   │   ├── retry.go                # 指数退避重试
│   │   └── prompt_cache.go         # Prompt 缓存策略
│   │
│   ├── engine/                     # 核心引擎
│   │   ├── engine.go               # 引擎主逻辑（1518行）
│   │   ├── activity.go             # 活动追踪 & 卡顿检测
│   │   ├── review.go               # 后台对话回顾 & 自动学习
│   │   └── engine_test.go          # 引擎测试（1076行）
│   │
│   ├── tool/                       # 工具系统
│   │   ├── tool.go                 # 工具接口定义
│   │   ├── registry.go             # 工具注册表
│   │   ├── bash.go                 # Shell 命令执行
│   │   ├── read.go                 # 文件读取
│   │   ├── write.go                # 文件写入
│   │   ├── edit.go                 # 文件精确编辑
│   │   ├── grep.go                 # 内容搜索（ripgrep）
│   │   ├── glob.go                 # 文件名匹配搜索
│   │   ├── webfetch.go             # 网页抓取
│   │   ├── powershell.go           # PowerShell 执行（Windows）
│   │   ├── advanced_tools_task_core.go    # 高级工具：任务、Agent 子进程
│   │   ├── advanced_tools_agent_skill.go  # 高级工具：技能调用
│   │   └── advanced_tools_plan_worktree.go # 高级工具：计划、工作树
│   │
│   ├── plan/                       # 计划系统
│   │   ├── plan.go                 # 计划数据结构 & 解析
│   │   └── executor.go             # 计划执行器
│   │
│   ├── permission/                 # 权限系统
│   │   ├── permission.go           # 权限模式 & 决策
│   │   └── classifier.go           # 工具分类器（自动判断危险等级）
│   │
│   ├── session/                    # 会话持久化
│   │   ├── store.go                # 会话存储（文件 + JSON）
│   │   └── store_test.go           # 存储测试
│   │
│   ├── memory/                     # 记忆系统
│   │   ├── store.go                # 记忆存储（BM25 + 向量）
│   │   ├── bm25.go                 # BM25 关键词检索
│   │   └── embed.go                # 伪嵌入 & 向量存储
│   │
│   ├── skills/                     # 技能系统
│   │   ├── skills.go               # 技能注册 & 加载
│   │   └── skills_test.go          # 技能测试
│   │
│   ├── mcp/                        # MCP 协议集成
│   │   ├── pool.go                 # MCP 连接池
│   │   └── client.go               # MCP 客户端
│   │
│   ├── browser/                    # 网页浏览器
│   │   └── browser.go              # HTTP 抓取 + HTML→文本转换
│   │
│   ├── termui/                     # 终端样式与输出封装
│   │   ├── style.go                # ANSI 颜色与样式
│   │   ├── io.go                   # 输出打印封装
│   │   └── indicator.go            # 状态指示器
│   │
│   ├── context/                    # 项目上下文
│   │   └── context.go              # 项目文件分析
│   │
│   ├── repomap/                    # 仓库地图
│   │   └── repomap.go              # 代码库结构分析
│   │
│   ├── config/                     # 配置管理
│   │   └── config.go               # 配置加载 & 验证
│   │
│   ├── log/                        # 日志系统
│   │   └── logger.go               # 分级日志 + 错误汇流
│   │
│   ├── cost/                       # 成本追踪
│   │   └── tracker.go              # Token 费用计算
│   │
│   ├── token/                      # Token 估算
│   │   └── token.go                # 简单 Token 计数
│   │
│   ├── diagnostic/                 # 诊断系统
│   │   ├── checker.go              # 系统健康检查
│   │   ├── recorder.go             # 运行时事件记录
│   │   ├── errors.go               # 错误定义
│   │   └── diagnostic_test.go      # 诊断测试
│   │
│   ├── checkpoint/                 # 检查点系统
│   │   └── checkpoint.go           # 文件修改前自动备份
│   │
│   ├── notes/                      # 笔记系统
│   │   └── notes.go                # 会话笔记管理
│   │
│   ├── onboarding/                 # 新手引导
│   │   └── onboarding.go           # 首次使用引导流程
│   │
│   ├── state/                      # 状态定义
│   │   └── state.go                # 应用状态枚举
│   │
│   ├── plugin/                     # 插件系统
│   │   └── plugin.go               # 插件加载 & 管理
│   │
│   ├── hooks/                      # 钩子系统
│   │   └── hooks.go                # 生命周期钩子
│   │
│   ├── guardrail/                  # 安全护栏
│   │   └── guardrail.go            # 输入/输出安全检查
│   │
│   ├── delegate/                   # 代理机制
│   │   └── delegate.go             # 任务委托
│   │
│   ├── extract/                    # 内容提取
│   │   └── extract.go              # 从对话中提取结构化数据
│   │
│   └── dream/                      # 梦想（反思）系统
│       └── dream.go                # Agent 自我反思
│
├── testdata/                       # 测试数据
│   └── ...
│
└── scripts/                        # 辅助脚本
    └── ...
```

---

## 4. 核心架构设计

### 4.1 整体架构图

```
┌─────────────────────────────────────────────────────────┐
│                    CLI Entry (main.go)                    │
│  - 解析参数、加载配置、初始化所有子系统                      │
│  - 调用 useInteractiveShell() 判断交互模式                 │
└───────┬───────────────────────────────┬─────────────────┘
        │ REPL (默认)                    │ Headless (fallback)
        ▼                               ▼
┌───────────────────┐   ┌─────────────────────────────────┐
│ runREPL()         │   │ runHeadless()                   │
│ (repl_loop.go)    │   │ (headless.go)                   │
│ - 行编辑器输入    │   │ - 无 UI，逐行 stdin 处理         │
│ - 输出在输入行上方│   │ - stdout/stderr 脚本友好输出     │
│ - Markdown 渲染   │   │ - 仅管道/--no-tui/              │
│ - 任务队列        │   │   COVE_TUI=0 时触发             │
└───────┬───────────┘   └────────┬────────────────────────┘
        │                        │
        └────────┬───────────────┘
                 │
┌────────────────▼────────────────────────────────────────┐
│                  Engine (engine.go)                       │
│  - 核心编排器：管理消息历史，调用 AI，执行工具               │
│  - 通过回调 onDelta/onReasoning/onEngineOutput 推送输出    │
│  - REPL 经 termui 打印在输入行上方，headless 直接写 stdout │
└───────┬───────┬───────┬───────┬─────────┬──────────────┘
        │       │       │       │         │
   ┌────▼──┐ ┌─▼──┐ ┌──▼──┐ ┌─▼───┐ ┌───▼──────┐
   │ API   │ │Tool│ │Plan │ │Perm │ │ Memory/  │
   │ Layer │ │Sys │ │Exec │ │Check│ │ Skills   │
   └───────┘ └────┘ └─────┘ └─────┘ └──────────┘
```

### 4.2 核心设计原则

1. **单一入口**: 整个程序只有一个 `main.go`，所有初始化在 `app_bootstrap.go` 中完成
2. **接口抽象**: API Provider、Tool、Memory 等核心组件都定义了接口，便于扩展
3. **关注分离**: 每个 `internal/` 子包职责单一，通过 Engine 协调
4. **安全第一**: 权限系统对所有写操作进行拦截，默认需要用户确认
5. **优雅降级**: 当某个子系统不可用时（如 MCP 服务器未启动），不影响核心功能

---

## 5. 模块详解

### 5.1 CLI 入口层

#### 5.1.1 main.go - 程序入口

**文件**: `cli/cove/main.go`（949 行）

**核心流程**:

```go
func main() {
    // 1. 解析命令行参数
    // 2. 加载配置文件（~/.cove/config.json 或环境变量）
    // 3. 创建 API Provider（Anthropic 或 OpenAI 兼容）
    // 4. 初始化 Engine（引擎是核心编排器）
    // 5. 注册内置工具（bash, read, write, edit, grep, glob, webfetch, ...）
    // 6. 初始化 MCP 连接池（如果有配置 MCP 服务器）
    // 7. 加载 Memory、Skills、Session
    // 8. 设置权限模式
    // 9. 启动 REPL 交互循环
    // 10. 应用退出时保存状态
}
```

**关键启动步骤**:

1. **配置解析**: 从 `~/.cove/config.json` 读取，优先级：命令行参数 > 环境变量 > 配置文件 > 默认值
2. **Provider 创建**: 根据 `provider.name` 选择 Anthropic 或 OpenAI 兼容，支持多 Key 轮转
3. **Engine 初始化**: `engine.New(cfg)` 创建核心引擎实例
4. **工具注册**: 通过 `registry.go` 将所有内置工具注册到 Engine
5. **MCP 启动**: 异步连接所有配置的 MCP 服务器，将其工具注册到 Engine
6. **REPL 启动**: 进入无限循环，读用户输入 → Engine 处理 → 输出响应

#### 5.1.2 app_bootstrap.go - 启动引导

**文件**: `cli/cove/app_bootstrap.go`（196 行）

负责将 `main.go` 中的初始化逻辑模块化：

- `bootstrapConfig()`: 加载并验证配置
- `bootstrapAPI()`: 创建 API Provider（含 Key 池）
- `bootstrapTools()`: 注册所有工具
- `bootstrapMCP()`: 初始化 MCP 连接
- `bootstrapMemory()`: 加载记忆系统
- `bootstrapSkills()`: 加载技能系统

#### 5.1.3 headless.go - 非交互主循环

**文件**: `cli/cove/headless.go`

**核心职责**:
- 显示提示符 `> `
- 支持多行输入（以 `\` 结尾续行）
- 处理特殊命令（`/help`, `/exit`, `/clear`, `/undo` 等）
- 调用 `chat_interaction.go` 处理单次对话
- 管理上下文窗口（自动压缩过长历史）
- 处理 Ctrl+C 中断

**关键常量**:
```go
const maxContextMessages = 200  // 最大消息数
const maxContextTokens  = 90000 // 最大 Token 数
```

**上下文窗口管理策略**:
当消息历史超过限制时，保留 `system` 消息 + 最早的 5 条 + 最近的 N 条，中间部分压缩为摘要。

#### 5.1.4 chat_interaction.go - 单次交互

**文件**: `cli/cove/chat_interaction.go`（151 行）

封装单次用户消息的处理流程：

```go
func chatInteraction(eng *engine.Engine, userInput string, ...) {
    // 1. 构建用户消息
    // 2. 调用 eng.RunMessageWithStream() 流式获取响应
    // 3. 实时输出 delta（AI 逐字输出效果）
    // 4. 处理工具调用、权限请求
    // 5. 返回最终响应文本
}
```

---

### 5.2 Engine 引擎层

**文件**: `internal/engine/engine.go`（1518 行）

Engine 是项目最核心的模块，是所有子系统的**编排中心**。

#### 5.2.1 Engine 结构体

```go
type Engine struct {
    config    Config          // 引擎配置
    provider  api.Provider    // AI Provider 接口
    messages  []api.Message   // 当前对话历史
    tools     []tool.Tool     // 已注册工具列表
    toolReg   *tool.Registry   // 工具注册表

    // 权限
    perm      *permission.Checker
    PermissionPrompt func(toolName string, input map[string]any, reason string) bool

    // 计划系统
    plan      *plan.Plan
    planExec  *plan.Executor

    // 记忆 & 技能
    memStore  *memory.Store
    skillMgr  *skills.Manager

    // 会话
    sessionStore *session.Store

    // MCP
    mcpPool   *mcp.Pool

    // 成本追踪
    costTracker *cost.Tracker

    // 活动监控（卡顿检测）
    acts      map[uint64]*activity
    actMu     sync.Mutex
    actSeq    uint64
    // ... 更多字段
}
```

#### 5.2.2 核心方法: RunMessageWithStream

这是 Engine 最重要的方法，处理一条用户消息的完整生命周期：

```go
func (e *Engine) RunMessageWithStream(
    ctx context.Context,
    msg api.Message,
    onDelta func(string),       // 流式输出回调
    interrupt <-chan struct{},    // 中断信号
) (reply string, err error)
```

**完整处理流程**:

```
用户消息
  │
  ▼
┌─────────────────────────────────────────┐
│ 1. 添加用户消息到 e.messages              │
├─────────────────────────────────────────┤
│ 2. 注入系统提示（System Prompt）          │
│    - 角色定义 + 可用工具列表               │
│    - 记忆上下文 + 技能列表                 │
│    - 项目上下文 + 仓库地图                 │
├─────────────────────────────────────────┤
│ 3. 调用 Provider.ChatStream()            │
│    - 流式获取 AI 响应                      │
│    - 通过 onDelta 回调实时输出             │
│    - 监控 interrupt 通道                  │
├─────────────────────────────────────────┤
│ 4. 解析 AI 响应                           │
│    ├─ 纯文本响应 → 收集到 reply            │
│    ├─ 工具调用请求 → 进入工具执行循环       │
│    └─ 停止原因 → 退出循环                  │
├─────────────────────────────────────────┤
│ 5. 【工具执行循环】                        │
│    a. 权限检查（Permission）               │
│    b. 并行执行所有工具调用                  │
│    c. 收集结果，格式化后添加到消息历史       │
│    d. 再次调用 Provider（继续对话）         │
│    e. 检查预算、步数限制                   │
│    f. 循环直到 AI 不再请求工具              │
├─────────────────────────────────────────┤
│ 6. 后处理                                 │
│    - 更新成本                              │
│    - 触发后台回顾（review.go）             │
│    - 保存会话                              │
│    - 返回最终响应                          │
└─────────────────────────────────────────┘
```

#### 5.2.3 系统提示构建

Engine 在每次调用 AI 前构建系统提示（System Prompt），包含：

```
你是 Cove，一个 AI 编程助手。
你可以使用以下工具：
  - bash: 执行 Shell 命令
  - read: 读取文件
  - write: 写入文件
  - ...（工具列表）

当前项目上下文：[项目文件树摘要]
相关记忆：[从 Memory 检索的相关记忆]
可用技能：[已注册技能列表]
安全规则：[护栏规则]
```

#### 5.2.4 活动监控（activity.go）

`activity.go` 实现了**卡顿检测**机制：

- 每个操作阶段（API 调用、工具执行）都被注册为一个 `activity`
- 后台 goroutine 每 5 秒扫描所有活动
- 如果某个活动 **30 秒无进展**，输出黄色警告 `⚠ 仍在「xx」，已 xx 无新进展`
- 用户看到后可以按 Ctrl+C 中断

#### 5.2.5 后台回顾（review.go）

每次对话回合结束后自动触发（异步，30 秒超时）：

1. 截取最近 10 条消息作为快照
2. 发送给 AI 分析："是否有值得记住的内容？"
3. AI 返回 `MEMORY: xxx` → 存入 Memory
4. AI 返回 `SKILL: name | steps` → 注册新技能
5. 用户看到 `🧠 记住了: xxx` 或 `📚 学会了: xxx`

---

### 5.3 API 层

#### 5.3.1 Provider 接口（provider.go）

```go
type Provider interface {
    Name() string
    DisplayName() string
    Validate() error
    Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
    ChatStream(ctx context.Context, req ChatRequest, handler StreamHandler) (*ChatResponse, error)
}

type ChatRequest struct {
    Model      string
    SystemBase string    // 系统提示
    Messages   []Message // 对话历史
    Tools      []ToolDef // 可用工具定义
    MaxTokens  int
    Temperature float64
}

type ChatResponse struct {
    Content    string
    ToolCalls  []ToolCall
    StopReason string
    Usage      Usage
}

type Message struct {
    Role         string    // "system" | "user" | "assistant" | "tool"
    Content      string
    ToolCalls    []ToolCall
    ToolCallID   string
    CacheControl string    // Anthropic prompt cache
}
```

#### 5.3.2 Anthropic 实现（anthropic.go）

**文件**: `internal/api/anthropic.go`（473 行）

实现 Anthropic Messages API 的调用：

- **端点**: `https://api.anthropic.com/v1/messages`
- **认证**: `x-api-key` 头 + Anthropic 版本头
- **流式**: Server-Sent Events (SSE) 解析
- **工具**: 将 ToolDef 转换为 Anthropic `tool_use` 格式
- **缓存**: 支持 `ephemeral` cache_control
- **速率限制**: 解析 `anthropic-ratelimit-*` 响应头

**关键处理**:
- 消息格式转换：Cove 内部格式 ↔ Anthropic API 格式
- 流式事件解析：`content_block_start/delta/stop`
- 错误处理：区分 4xx/5xx，重试策略由 retry.go 处理

#### 5.3.3 OpenAI 兼容实现（openai_compat.go）

**文件**: `internal/api/openai_compat.go`（668 行）

支持所有 OpenAI Chat Completions API 兼容的 Provider：

- **端点**: `{base_url}/v1/chat/completions`
- **认证**: `Authorization: Bearer {key}`
- **流式**: SSE `data: [DONE]`
- **工具**: 转换为 OpenAI `function_call` 格式

支持的环境变量配置：
- `OPENAI_API_KEY`
- `OPENAI_BASE_URL`（自定义端点，如 Azure、本地模型）
- `OPENAI_MODEL`（默认 gpt-4o）

#### 5.3.4 API Key 池（keypool.go）

**文件**: `internal/api/keypool.go`（172 行）

管理多个 API Key，实现自动故障转移：

```go
type KeyPool struct {
    keys    []*PoolKey      // Key 列表
    current int             // 当前轮转位置
}

type PoolKey struct {
    Key       string         // API Key
    Status    KeyStatus      // OK / Exhausted / Dead
    CoolUntil time.Time      // 冷却到何时
}
```

**状态机**:
- `KeyOK` → 可用
- `KeyExhausted` → 被限流，冷却后可恢复
- `KeyDead` → 认证失败，永久不可用

**轮转策略**: Round-robin，跳过 Exhausted（除非全部不可用）

#### 5.3.5 重试机制（retry.go）

**文件**: `internal/api/retry.go`（69 行）

指数退避重试：

```go
func retryWithBackoff[T any](ctx context.Context, cfg retryConfig, operation func() (T, error)) (T, error) {
    for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
        result, err := operation()
        if err == nil { return result, nil }
        if attempt == cfg.MaxRetries || !isRetryable(err) { return zero, err }
        delay := time.Duration(1<<attempt) * cfg.BaseDelay  // 1s, 2s, 4s, 8s...
        // 等待 delay 或 ctx 取消
    }
}
```

**可重试错误**: 网络超时、5xx、429（Rate Limit）
**不可重试错误**: 4xx（除 429）、认证错误

#### 5.3.6 速率限制追踪（ratelimit.go）

解析 API 响应头中的速率限制信息，实时显示：

```
请求:150/200(75%) 重置:1m30s | Token:45K/100K(45%) 重置:2m
```

---

### 5.4 Tool 工具系统

#### 5.4.1 工具接口（tool.go）

```go
type Tool interface {
    Def() Def                              // 返回工具定义（名称、描述、参数 Schema）
    Validate(input Input) string           // 验证输入参数，返回错误信息或空
    CheckPermissions(input Input, tctx Context) PermissionDecision  // 权限检查
    Call(ctx context.Context, input Input, tctx Context) (Result, error)  // 执行
}

type Def struct {
    Name              string          // 工具名称（如 "bash", "read"）
    Description       string          // 描述（给 AI 看的）
    InputSchema       json.RawMessage // JSON Schema 参数定义
    IsReadOnly        bool            // 是否只读
    IsConcurrencySafe bool            // 是否并发安全
    UserFacingName    string          // 面向用户的名称
}
```

#### 5.4.2 内置工具一览

| 工具名 | 文件 | 功能 | 只读 |
|--------|------|------|------|
| `bash` | bash.go | 执行 Shell 命令（Linux/macOS） | ❌ |
| `powershell` | powershell.go | 执行 PowerShell 命令（Windows） | ❌ |
| `read` | read.go | 读取文件内容 | ✅ |
| `write` | write.go | 写入文件（覆盖） | ❌ |
| `edit` | edit.go | 精确字符串替换编辑 | ❌ |
| `grep` | grep.go | 正则搜索（底层调用 ripgrep） | ✅ |
| `glob` | glob.go | 文件名 glob 匹配搜索 | ✅ |
| `webfetch` | webfetch.go | HTTP 抓取网页内容 | ✅ |
| `browser` | webfetch.go | Headless 浏览器渲染 | ✅ |
| `question` | advanced_tools_*.go | 向用户提问 | ✅ |
| `todowrite` | advanced_tools_*.go | 创建/管理任务列表 | ✅ |
| `execute_plan` | advanced_tools_plan_worktree.go | 执行计划任务 | ❌ |
| `plan_mode` | advanced_tools_plan_worktree.go | 进入计划模式 | ✅ |
| `exit_plan_mode` | advanced_tools_plan_worktree.go | 退出计划模式 | ✅ |
| `worktree` | advanced_tools_plan_worktree.go | 创建 Git 工作树 | ❌ |
| `task` | advanced_tools_task_core.go | 创建后台任务 | ❌ |
| `agent` | advanced_tools_agent_skill.go | 启动子 Agent | ❌ |
| `skill` | advanced_tools_agent_skill.go | 调用技能 | ✅ |
| `sleep` | advanced_tools_task_core.go | 暂停等待 | ✅ |
| `send_message` | advanced_tools_task_core.go | 发送消息 | ✅ |

#### 5.4.3 工具注册表（registry.go）

```go
type Registry struct {
    tools    map[string]Tool
    toolList []Tool       // 保持插入顺序
}

func (r *Registry) Register(t Tool)      // 注册工具
func (r *Registry) Get(name string) Tool // 按名查找
func (r *Registry) All() []Tool          // 所有工具（供 AI 选择）
func (r *Registry) Defs() []Def          // 所有工具定义
```

#### 5.4.4 高级工具详解

**Task 系统** (`advanced_tools_task_core.go`):
- `task`: 创建后台独立任务（异步 goroutine）
- `task_list`: 列出所有后台任务
- `task_update`: 更新任务状态
- `task_stop`: 停止运行中的任务

**Agent/Skill 系统** (`advanced_tools_agent_skill.go`):
- `agent`: 创建子代理处理复杂多步骤任务
- `skill`: 执行预定义的技能工作流

**Plan/Worktree 系统** (`advanced_tools_plan_worktree.go`):
- `plan_mode`: 进入只读计划模式
- `execute_plan`: 执行计划中的所有待处理任务
- `worktree`: 创建 Git 工作树用于隔离修改

#### 5.4.5 工具执行机制（Engine 中）

工具调用在 Engine 中是**并行执行**的：

```go
// engine.go 中简化逻辑
func (e *Engine) executeToolCalls(toolCalls []api.ToolCall) []tool.Result {
    var wg sync.WaitGroup
    results := make([]tool.Result, len(toolCalls))

    for i, tc := range toolCalls {
        wg.Add(1)
        go func(idx int, call api.ToolCall) {
            defer wg.Done()
            defer func() { recover() }()  // panic 恢复

            // 1. 权限检查
            // 2. 参数验证
            // 3. 执行工具
            results[idx] = tool.Call(ctx, input, tctx)
        }(i, tc)
    }

    wg.Wait()
    return results
}
```

---

### 5.5 Permission 权限系统

#### 5.5.1 权限模式（permission.go）

```go
type Mode string
const (
    Default Mode = "default" // 只读工具 + 整行只读的 shell 命令自动放行，其余询问
    Plan    Mode = "plan"    // 只读工具；bash 与写入一律拒绝
    Auto    Mode = "auto"    // Default + 构建/测试命令 + 项目内 write/edit；git 写/安装/网络/未知命令仍询问
    Bypass  Mode = "bypass"  // 全部放行（灾难命令仍由引擎硬拦截）
)
```

#### 5.5.2 权限决策

```go
type PermissionDecision struct {
    Decision Decision  // Allow / Deny / Ask
    Reason   string
}
```

#### 5.5.3 工具分类器（classifier.go）

自动将工具分为三类：

1. **只读安全**（`read`, `grep`, `glob`）→ 永远允许
2. **低风险写**（`write`, `edit`）→ Default 模式需确认
3. **高风险**（`bash`, `powershell`）→ 有命令分析规则

命令分析器检查 Shell 命令是否包含危险操作：
- `rm -rf /`
- `curl ... | bash`
- 修改系统文件
- 网络监听等

#### 5.5.4 权限钩子

Engine 暴露 `PermissionPrompt` 函数指针：

```go
eng.PermissionPrompt = func(toolName string, input map[string]any, reason string) bool {
    // 显示给用户，询问是否允许
    // 返回 true = 允许，false = 拒绝
}
```

在 CLI 中，这个钩子会：
1. 格式化工具调用信息
2. 显示 `⚠ 允许执行 bash: "rm file.txt"? [y/N]`
3. 等待用户输入

---

### 5.6 Plan 计划系统

#### 5.6.1 Plan 数据结构（plan.go）

```go
type Plan struct {
    ID      string
    Goal    string
    Steps   []Step
    Status  PlanStatus
}

type Step struct {
    ID          string
    Description string
    Status      StepStatus   // pending / in_progress / done / failed
    DependsOn   []string     // 依赖的其他步骤 ID
    Result      string
}
```

#### 5.6.2 计划执行器（executor.go）

```go
type Executor struct {
    plan     *Plan
    engine   *Engine
    maxAgents int            // 最大并发数
}

func (e *Executor) Execute(ctx context.Context, parallel bool) error {
    // 1. 拓扑排序步骤（处理依赖关系）
    // 2. 按依赖分组并行执行
    // 3. 每个步骤创建一个子 Agent 或直接执行工具
    // 4. 收集结果，更新步骤状态
}
```

**并行执行策略**:
- `parallel=true`: 无依赖关系的步骤同时执行
- `parallel=false`: 顺序执行

---

### 5.7 Memory 记忆系统

#### 5.7.1 记忆存储（store.go）

```go
type Store struct {
    bm25     *BM25               // 关键词检索
    vecStore *VectorStore        // 向量检索
    embedder EmbeddingProvider   // 嵌入提供者
    entries  []MemoryEntry       // 元数据
}

type MemoryEntry struct {
    ID       int
    Name     string    // "user_preference", "auto", etc.
    Content  string
    Updated  time.Time
}
```

#### 5.7.2 检索策略

**混合检索（Hybrid Search）**:

```go
func (s *Store) Search(query string, topK int) []ScoredDoc {
    // 1. BM25 关键词检索（快速、精确匹配）
    bm25Results := s.bm25.Search(query, topK*2)

    // 2. 向量语义检索（语义相似度）
    queryVec := s.embedder.Embed(query)
    vecResults := s.vecStore.Search(queryVec, topK*2)

    // 3. 融合排序（BM25 0.7 + 向量 0.3）
    // 4. 考虑时效性衰减
    // 5. 返回 Top-K
}
```

#### 5.7.3 BM25 实现（bm25.go）

经典的 BM25 信息检索算法：

- `k1=1.2`: 词频饱和参数
- `b=0.75`: 文档长度归一化参数
- 分词：小写化 + 字母数字保留 + 停用词过滤
- IDF 计算：`log(1 + (N-df+0.5)/(df+0.5))`

#### 5.7.4 伪嵌入（embed.go）

为降低成本，使用**字符三元组哈希**生成伪嵌入向量：

```go
func pseudoEmbedding(text string, dim int) []float32 {
    // 1. 提取所有字符三元组（trigram）
    // 2. 哈希到 [0, dim) 范围
    // 3. 累加计数
    // 4. L2 归一化
}
```

这是一个 economical 的替代方案，适用于记忆条目 < 1000 的场景。

---

### 5.8 Skills 技能系统

#### 5.8.1 技能定义（skills.go）

```go
type Skill struct {
    Name        string   // 技能名称
    Description string   // 描述
    Prompt      string   // 技能提示词（给 AI 的执行指南）
    Tools       []string // 需要的工具列表
}
```

#### 5.8.2 技能管理器

```go
type Manager struct {
    skills      map[string]Skill
    builtinDir  string   // 内置技能目录
    userDir     string   // 用户自定义技能目录
    loader      *Loader  // 文件系统加载器
}

func (m *Manager) Register(skill Skill)      // 注册技能
func (m *Manager) Execute(name string, args map[string]any) // 执行技能
func (m *Manager) ListForAI() string          // 格式化给 AI 看
```

#### 5.8.3 技能目录结构

```
skills/
├── builtin/               # 内置技能
│   ├── code-review/
│   │   └── SKILL.md       # 技能定义文件
│   ├── refactor/
│   │   └── SKILL.md
│   └── ...
└── user/                  # 用户自定义
    └── my-skill/
        └── SKILL.md
```

`SKILL.md` 格式：
```markdown
# skill-name
简短描述

## 执行步骤
1. 第一步...
2. 第二步...

## 需要的工具
- read
- write
```

---

### 5.9 Session 会话管理

**文件**: `internal/session/store.go`（284 行）

```go
type Store struct {
    dir     string           // 会话文件目录
    current *Session
}

type Session struct {
    ID        string
    Created   time.Time
    Updated   time.Time
    Messages  []api.Message   // 完整对话历史
    Summary   string          // 会话摘要
}
```

**持久化策略**:
- 存储为 JSONL：`~/.cove/sessions/{id}.jsonl`（首行元数据，之后每行一条消息）+ `index.json` 列表索引；旧版 `{id}.json` 仍可加载，下次保存时迁移
- 每次对话回合后自动保存（只追加新增消息；历史被替换时整文件原子重写）
- 启动时可恢复上次会话

---

### 5.10 MCP 集成

Model Context Protocol (MCP) 是 Anthropic 提出的开放协议，允许外部工具服务器提供工具。

#### 5.10.1 MCP 连接池（pool.go）

```go
type Pool struct {
    servers []*Client         // MCP 客户端列表
    tools   []tool.Tool       // 从 MCP 服务器获取的工具
}

func NewPool(configs []ServerConfig) *Pool
func (p *Pool) Connect(ctx context.Context) error   // 连接所有服务器
func (p *Pool) Tools() []tool.Tool                   // 获取所有工具
func (p *Pool) Close() error
```

#### 5.10.2 MCP 客户端（client.go）

```go
type Client struct {
    config   ServerConfig
    conn     *stdio.Connection  // 通过 stdio 与子进程通信
    tools    []tool.Tool
}

type ServerConfig struct {
    Command string   // 启动命令（如 "npx", "uvx" 等）
    Args    []string // 命令参数
    Env     []string // 环境变量
}
```

**通信机制**:
- 启动子进程（通过 `os/exec`）
- 通过 stdin/stdout 传递 JSON-RPC 消息
- `tools/list` → 获取工具列表
- `tools/call` → 调用工具

---

### 5.11 Browser 浏览器

**文件**: `internal/browser/browser.go`（342 行）

HTTP 客户端 + HTML 转换引擎：

```go
type Browser struct {
    timeout        time.Duration
    allowLocalhost bool      // 安全：默认禁止本地地址
    maxBodySize    int64     // 默认 5MB
}
```

**安全措施**:
- SSRF 防护：禁止访问内网 IP（127.0.0.1, 10.x, 192.168.x, 172.16-31.x 等）
- 禁止访问云元数据端点（`metadata.google.internal`）
- 响应大小限制 5MB
- 输出截断 100KB

**HTML 转换**:
- `HTMLToText()`: 去除标签，保留文本
- `HTMLToMarkdown()`: 转换为 Markdown（保留标题、链接、代码块、列表）

**Headless Chrome 支持**（可选）:
- 需要编译标签 `-tags chromedp`
- 提供 `FetchRendered()` 和 `Screenshot()` 方法
- 默认不可用，优雅降级

---

### 5.12 交互式 REPL（★ 当前默认交互模式）

**文件**: `cli/cove/interactive.go`（模式选择）、`cli/cove/repl_loop.go`（`runREPL()` 主循环）、`cli/cove/repl_tasks.go`（任务队列）、`cli/cove/permission_prompt.go`（授权提示）、`internal/repl/readline.go`（行编辑器）、`internal/render/markdown.go`（Markdown 渐进渲染）

#### 5.12.1 启用逻辑（`interactive.go`: `useInteractiveShell()`）

```go
func useInteractiveShell() bool {
    if noTUI || os.Getenv("COVE_TUI") == "0" { return false }   // 显式禁用 → headless
    if tuiMode || os.Getenv("COVE_TUI") == "1" { return true }  // 显式启用 → REPL
    // 默认：stdin 和 stdout 都是终端 → REPL；管道/重定向 → headless
    return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}
```

命令行控制：`--tui` / `--no-tui`，环境变量 `COVE_TUI=0/1`（名称沿用自早期的全屏界面，该界面已移除）。

#### 5.12.2 结构

- **行编辑器**（`repl.New(completer)`）：输入行、历史、Tab 补全（`completion.go`）；`termui.SetConsole(reader)` 与 `log.SetWriter` 让所有输出打印在输入行上方，不会覆盖正在编辑的内容。
- **任务队列**（`replTaskRunner`）：普通消息入队串行执行，相似任务合并；Ctrl+C / `/stop` 取消当前任务；失败后输入“继续”重试。
- **流式输出**：`onDelta` 经 `render.MarkdownStream` 渐进渲染（标题/粗体/行内代码/围栏代码块/列表/引用），每次新请求或重试时重置状态；不支持 Unicode 的控制台降级为 ASCII（`COVE_TUI_ASCII=1` 可强制）。
- **授权提示**：`askToolPermission` 显示授权框与 `[y] 允许 / [a] 本次会话总是允许 / [p] 永久允许（本项目） / [n] 拒绝`，回答经 `repl.TakePermInputCh` 从主循环转发；15 分钟无回答视为拒绝。`-p` 模式不安装提示器，需要询问的调用直接拒绝。

### 5.13 终端输出层

#### 5.13.1 颜色工具（color.go）

**文件**: `internal/termui/style.go` + `internal/termui/io.go`（输出样式与终端打印）

> **注意**: 交互层为行式 REPL；fallback 为 headless 无 UI 模式。终端样式输出统一由 `internal/termui` 提供。

ANSI 颜色和样式定义：

```go
// 颜色常量
const (
    Reset   = "\033[0m"
    Red     = "\033[31m"
    Green   = "\033[32m"
    Yellow  = "\033[33m"
    Blue    = "\033[34m"
    Cyan    = "\033[36m"
    Gray    = "\033[90m"
    // ...
)

// 渲染函数
func Dim(s string) string      // 灰色/暗色文本
func Bold(s string) string     // 粗体
func Highlight(s string) string // 高亮（青色粗体）
func Error(s string) string    // 错误（红色）
```

#### 5.13.2 输出层（termui）

**文件**: `internal/termui/style.go`、`internal/termui/io.go`、`internal/termui/indicator.go`

终端输出层统一提供：

- **样式常量**: 颜色、加粗、弱化、推理文本样式
- **输出接口**: 安全打印、流式打印、瞬时状态行
- **指示器**: Spinner 与 WalkingIndicator

---

### 5.14 Context 上下文管理

**文件**: `internal/context/context.go`（221 行）

负责分析项目目录结构并生成给 AI 看的上下文：

```go
type ProjectContext struct {
    Root       string           // 项目根目录
    Language   string           // 检测到的编程语言
    FileTree   []FileEntry      // 文件树
    Framework  string           // 检测到的框架
}

func Analyze(dir string) (*ProjectContext, error)
func (pc *ProjectContext) Format() string  // 格式化为 AI 可读字符串
```

**检测逻辑**:
- 扫描根目录关键文件（`go.mod` → Go, `package.json` → Node.js, etc.）
- 忽略 `.gitignore` 和 `.coveignore` 中指定的文件
- 生成简洁的文件树

**RepoMap**（`internal/repomap/repomap.go`, 397 行）:
- 生成代码库的结构地图
- 包含关键类/函数/模块的定位信息
- 帮助 AI 理解项目结构

---

### 5.15 辅助模块

#### 5.15.1 日志系统（log/logger.go）

- 四级日志：Debug, Info, Warn, Error
- 输出到 stderr（与 stdout 的 AI 输出分离）
- `SetSink()` 机制：Warn/Error 自动回调（用于诊断记录）

#### 5.15.2 配置管理（config/config.go）

```go
type Config struct {
    Model          string          // AI 模型名称
    Provider       ProviderConfig  // Provider 配置
    Tools          []tool.Tool     // 工具列表
    PermissionMode string          // "default" / "plan" / "auto" / "bypass"
    MaxBudget      float64         // 最大费用预算（美元）
    MaxSteps       int             // 最大工具调用步数
    // ...
}
```

#### 5.15.3 成本追踪（cost/tracker.go）

追踪 API 调用费用：

```go
type Tracker struct {
    totalCost float64
    modelRates map[string]Rate  // 各模型价格
}

type Rate struct {
    InputPrice  float64  // 每 1K tokens 价格
    OutputPrice float64
}
```

#### 5.15.4 检查点（checkpoint/checkpoint.go）

文件修改前自动备份：

```go
func Save(filePath string) error {
    // 将文件复制到 ~/.cove/checkpoints/{timestamp}/{path}
}
```

#### 5.15.5 诊断系统（diagnostic/）

- `checker.go`: 系统健康检查（API 连通性、工具可用性）
- `recorder.go`: 运行时事件记录（用于事后调试）
- `errors.go`: 错误码定义

#### 5.15.6 安全检查（guardrail/）

```go
func CheckInput(input string) error     // 检查用户输入
func CheckOutput(output string) error   // 检查 AI 输出
```

检测潜在的安全问题（注入、敏感信息泄露等）。

#### 5.15.7 插件系统（plugin/plugin.go, 514 行）

支持外部插件扩展：

```go
type Plugin struct {
    Name    string
    Tools   []tool.Tool
    Hooks   []Hook
    Skills  []skills.Skill
}

func Load(dir string) ([]Plugin, error)
```

---

## 6. 核心数据流

### 6.1 一次完整对话的数据流

```
用户输入 "帮我读一下 main.go"
    │
    ▼
┌─ REPL Loop ──────────────────────────────────────────┐
│ 1. 读取用户输入                                        │
│ 2. 检查是否是内置命令（/help, /exit...）                │
│ 3. 构建 api.Message{Role: "user", Content: "帮我..."}  │
└───────────────────┬───────────────────────────────────┘
                    │
                    ▼
┌─ Engine.RunMessageWithStream ────────────────────────┐
│                                                       │
│ ┌─────────────────────────────────────────────┐      │
│ │ 构建 System Prompt:                           │      │
│ │ - 角色定义                                    │      │
│ │ - 工具列表 (Defs)                             │      │
│ │ - 项目上下文 (Context.Format())               │      │
│ │ - 仓库地图 (RepoMap)                          │      │
│ │ - 相关记忆 (Memory.Search())                  │      │
│ │ - 可用技能 (Skills.ListForAI())              │      │
│ └─────────────────────────────────────────────┘      │
│                       │                               │
│                       ▼                               │
│ ┌─────────────────────────────────────────────┐      │
│ │ API Call: Provider.ChatStream()              │      │
│ │ → POST https://api.anthropic.com/v1/messages │      │
│ │ → SSE Stream 返回                            │      │
│ │ → 解析: content_block_delta / tool_use       │      │
│ └─────────────────────────────────────────────┘      │
│                       │                               │
│            ┌──────────┴──────────┐                    │
│            │                     │                     │
│      返回文本             返回工具调用                  │
│            │                     │                     │
│            ▼                     ▼                     │
│   onDelta(文本)        ┌──────────────────┐           │
│   实时输出给用户         │ 权限检查           │           │
│            │            │ ↓                 │           │
│            │            │ PermissionPrompt  │           │
│            │            │ ↓                 │           │
│            │            │ 并行执行工具       │           │
│            │            │ ↓                 │           │
│            │            │ 收集结果           │           │
│            │            │ ↓                 │           │
│            │            │ 结果添加到消息历史  │           │
│            │            │ ↓                 │           │
│            │            │ 再次调用 AI       │──┐        │
│            │            └──────────────────┘  │        │
│            │                     │             │        │
│            │                     ◄─────────────┘        │
│            │              (循环直到 AI 停止调用工具)     │
│            ▼                                           │
│   最终响应文本                                          │
│                                                       │
└───────────────────┬───────────────────────────────────┘
                    │
                    ▼
    ┌───────────────────────────────┐
    │ 后处理:                       │
    │ - 更新成本 Tracker            │
    │ - 触发后台回顾 Review         │
    │ - 保存 Session                │
    │ - 更新 Memory/Skills          │
    └───────────────────────────────┘
                    │
                    ▼
              返回响应给 REPL
```

### 6.2 工具执行的数据流

```
Engine 收到 AI 的工具调用请求
    │
    ├─ 提取 tool_calls[] → [{name: "read", input: {filePath: "..."}}, ...]
    │
    ├─ 对每个 tool_call 并发执行:
    │   │
    │   ├─ toolReg.Get(name)  → 获取 Tool 实例
    │   ├─ tool.Validate(input) → 参数验证
    │   ├─ tool.CheckPermissions(input, ctx) → 权限检查
    │   │   ├─ Allow → 直接执行
    │   │   ├─ Deny  → 返回拒绝原因
    │   │   └─ Ask   → 调用 PermissionPrompt（可能阻塞等待用户输入）
    │   ├─ tool.Call(ctx, input, ctx) → 执行工具
    │   └─ 返回 Result{Data: "..."} 或 error
    │
    ├─ 收集所有结果
    │
    ├─ 构造 tool result 消息:
    │   api.Message{Role: "tool", ToolCallID: tc.ID, Content: result.Data}
    │
    ├─ 追加到 e.messages
    │
    └─ 再次调用 Provider.ChatStream()（将工具结果提交给 AI）
```

---

## 7. 关键设计模式

### 7.1 接口抽象模式

所有可替换组件都定义接口：

```go
// AI Provider 可替换
type Provider interface { Chat(...); ChatStream(...) }

// 工具可扩展
type Tool interface { Def(); Validate(); CheckPermissions(); Call() }

// 嵌入提供者可替换
type EmbeddingProvider interface { Embed(); Dim() }
```

### 7.2 注册表模式

工具通过注册表管理：

```go
reg := tool.NewRegistry()
reg.Register(&BashTool{})
reg.Register(&ReadTool{})
// ...
engine.SetRegistry(reg)
```

### 7.3 回调/钩子模式

Engine 暴露钩子供外部定制：

```go
eng.PermissionPrompt = myPermissionHandler
eng.OnDelta = myStreamHandler
```

### 7.4 优雅降级模式

```go
if e.memStore != nil {  // 记忆系统可选
    memories := e.memStore.Search(query, 5)
    // 注入到系统提示
}
```

### 7.5 Panic 恢复模式

所有工具执行都在 goroutine 中有 panic 恢复：

```go
go func() {
    defer func() {
        if r := recover(); r != nil {
            log.Errorf("tool panic: %v", r)
            results[idx] = tool.Result{IsError: true, Data: fmt.Sprint(r)}
        }
    }()
    results[idx] = tool.Call(...)
}()
```

### 7.6 上下文传播模式

所有异步操作通过 `context.Context` 传播取消信号：

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
// 传递给所有子操作
```

---

## 8. 如何运行与构建

### 8.1 环境要求

- Go 1.22+
- Git（用于 worktree 功能）
- ripgrep（`rg` 命令，用于 grep 工具）

### 8.2 配置

创建 `~/.cove/config.json`:

```json
{
    "provider": {
        "name": "anthropic",
        "api_key": "sk-ant-xxx",
        "model": "claude-sonnet-4-20250514"
    },
    "permission_mode": "default",
    "max_budget": 10.0
}
```

或使用环境变量：
```bash
export ANTHROPIC_API_KEY="sk-ant-xxx"
export COVE_MODEL="claude-sonnet-4-20250514"
```

### 8.3 构建

```bash
# 基础构建
cd G:\github\cove\agent
go build -o cove.exe ./cli/cove/

# 包含 Headless Chrome 支持（可选）
go build -tags chromedp -o cove.exe ./cli/cove/

# 运行
./cove.exe
```

### 8.4 开发模式

```bash
# 直接运行（无需构建）
go run ./cli/cove/

# 运行测试
go test ./internal/...

# 运行特定包测试
go test ./internal/engine/ -v -run TestEngineBasicMessageFlow
```

---

## 9. 如何扩展系统

### 9.1 添加新工具

```go
// 1. 创建新文件 internal/tool/my_tool.go
package tool

type MyTool struct{}

func (t *MyTool) Def() Def {
    return Def{
        Name:        "my_tool",
        Description: "我的自定义工具",
        InputSchema: json.RawMessage(`{
            "type": "object",
            "properties": {
                "input": {"type": "string", "description": "输入参数"}
            },
            "required": ["input"]
        }`),
        IsReadOnly:  true,
    }
}

func (t *MyTool) Validate(input Input) string { return "" }

func (t *MyTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
    return PermissionDecision{Decision: Allow}
}

func (t *MyTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
    // 实现逻辑
    return Result{Data: "结果"}, nil
}

// 2. 在 registry.go 中注册
// reg.Register(&MyTool{})
```

### 9.2 添加新 AI Provider

```go
// 1. 创建 internal/api/my_provider.go
type MyProvider struct { ... }

func (p *MyProvider) Name() string { return "my_provider" }
func (p *MyProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) { ... }
func (p *MyProvider) ChatStream(ctx context.Context, req ChatRequest, h StreamHandler) (*ChatResponse, error) { ... }

// 2. 在 provider_catalog.go 中添加
// catalog["my_provider"] = func(cfg) Provider { return NewMyProvider(cfg) }
```

### 9.3 添加 MCP 服务器

在配置文件中添加：

```json
{
    "mcp_servers": [
        {
            "command": "npx",
            "args": ["-y", "@anthropic/mcp-server-filesystem", "/path/to/dir"]
        }
    ]
}
```

---

## 10. 测试策略

### 10.1 单元测试

- **Engine 测试** (`engine_test.go`, 1076 行): 使用 Mock Provider 和 Mock Tool 进行集成测试
- **Skill 测试** (`skills_test.go`): 测试技能加载和注册
- **Session 测试** (`store_test.go`): 测试会话持久化
- **Diagnostic 测试** (`diagnostic_test.go`): 测试诊断功能

### 10.2 Mock 策略

```go
// Mock Provider - 模拟 AI 响应
type mockProvider struct {
    responses []mockResponse  // 预设的响应队列
}

// Mock Tool - 可控的工具行为
type mockTool struct {
    name     string
    readOnly bool
    result   string
    err      error
    panicMsg string  // 测试 panic 恢复
    delay    time.Duration  // 模拟慢速工具
}
```

### 10.3 关键测试场景

| 测试用例 | 测试内容 |
|----------|----------|
| `TestEngineBasicMessageFlow` | 基本消息流程 |
| `TestEngineToolExecution` | 工具调用执行 |
| `TestEnginePermissionDenied` | 权限拒绝 |
| `TestEngineToolPanicRecovery` | 工具 Panic 恢复 |
| `TestEngineMultipleIterations` | 多轮工具调用 |
| `TestEngineAPIError` | API 错误处理 |
| `TestEngineContextCancellation` | 上下文取消 |
| `TestEnginePermissionPromptNil` | 权限钩子未设置 |

### 10.4 运行测试

```bash
# 全部测试
go test ./...

# 指定测试
go test ./internal/engine/ -v -run "TestEngine"

# 带覆盖率
go test ./... -coverprofile=coverage.out
go tool cover -html=coverage.out
```

---

## 附录 A: 关键常量

| 常量 | 值 | 说明 |
|------|-----|------|
| `maxContextMessages` | 200 | 最大上下文消息数 |
| `maxContextTokens` | 90000 | 最大上下文 Token 数 |
| `stallThreshold` | 30s | 卡顿检测阈值 |
| `maxBodySize` | 5MB | 网页抓取最大体积 |
| `outputLimit` | 100KB | 网页输出截断 |
| `defaultDim` | 384 | 向量嵌入维度 |
| `reviewInterval` | 4 messages | 后台回顾触发间隔 |
| `reviewTimeout` | 30s | 后台回顾超时 |

## 附录 B: 环境变量

| 变量 | 说明 |
|------|------|
| `ANTHROPIC_API_KEY` | Anthropic API Key |
| `OPENAI_API_KEY` | OpenAI API Key |
| `OPENAI_BASE_URL` | OpenAI 兼容端点 |
| `OPENAI_MODEL` | OpenAI 模型名称 |
| `COVE_MODEL` | 覆盖模型选择 |
| `COVE_PROVIDER` | 覆盖 Provider 选择 |

---

> **文档版本**: 1.0
> **最后更新**: 2025
> **适用代码版本**: cove/agent (G:\github\cove\agent)
