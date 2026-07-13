# Cove Agent 完全开发手册

> **目标读者**: 零基础新手。读完本文档即可从零开发整个 Cove Agent 项目。
> **项目**: Cove — 一个 Go 语言编写的 AI 编程助手 (Coding Agent CLI)
> **版本**: v6.3.1+

---

## 目录

1. [项目是什么](#1-项目是什么)
2. [30 秒快速跑起来](#2-30-秒快速跑起来)
3. [项目文件结构一览](#3-项目文件结构一览)
4. [从 main 到 Engine：启动全流程](#4-从-main-到-engine启动全流程)
5. [核心模块逐层拆解](#5-核心模块逐层拆解)
   - [5.1 配置系统 (config)](#51-配置系统-config)
   - [5.2 API 层 (api)](#52-api-层-api)
   - [5.3 引擎层 (engine) — 项目心脏](#53-引擎层-engine--项目心脏)
   - [5.4 工具系统 (tool)](#54-工具系统-tool)
   - [5.5 权限系统 (permission)](#55-权限系统-permission)
   - [5.6 会话持久化 (session)](#56-会话持久化-session)
   - [5.7 记忆系统 (memory)](#57-记忆系统-memory)
   - [5.8 技能系统 (skills)](#58-技能系统-skills)
   - [5.9 钩子系统 (hooks)](#59-钩子系统-hooks)
   - [5.10 MCP 协议集成 (mcp)](#510-mcp-协议集成-mcp)
   - [5.11 循环检测 (LoopDetector)](#511-循环检测-loopdetector)
   - [5.12 上下文压缩 (ChatCompressor)](#512-上下文压缩-chatcompressor)
   - [5.12a 模型故障转移 (ModelFallback)](#512a-模型故障转移-modelfallback)
   - [5.12b 工具输出掩码 (ToolOutputMasker)](#512b-工具输出掩码-tooloutputmasker)
   - [5.12c 模型路由 (ModelRouter)](#512c-模型路由-modelrouter)
   - [5.12d 策略引擎 (PolicyEngine)](#512d-策略引擎-policyengine)
   - [5.12e 发言人预测 (NextSpeaker)](#512e-发言人预测-nextspeaker)
   - [5.12f 会话 Diff (SessionDiff)](#512f-会话-diff-sessiondiff)
   - [5.12g 本地遥测 (Telemetry)](#512g-本地遥测-telemetry)
   - [5.12h 安全检查 (Safety)](#512h-安全检查-safety)
   - [5.13 TUI 界面层 (tui)](#513-tui-界面层-tui)
   - [5.14 移动端引擎 (mobile)](#514-移动端引擎-mobile)
   - [5.15 其他辅助模块](#515-其他辅助模块)
   - [5.16 后台回顾系统 (review)](#516-后台回顾系统-review)
   - [5.17 计划执行系统 (plan)](#517-计划执行系统-plan)
   - [5.18 斜杠命令系统 (command)](#518-斜杠命令系统-command)
6. [核心数据流全追踪](#6-核心数据流全追踪)
7. [如何添加新功能（实操教程）](#7-如何添加新功能实操教程)
   - [7.1 添加一个新工具](#71-添加一个新工具)
   - [7.2 添加一个新的斜杠命令](#72-添加一个新的斜杠命令)
   - [7.3 添加一个新的 API Provider](#73-添加一个新的-api-provider)
   - [7.4 添加一个配置项](#74-添加一个配置项)
   - [7.5 添加一个新的引擎子系统](#75-添加一个新的引擎子系统)
8. [测试策略](#8-测试策略)
9. [调试技巧](#9-调试技巧)
10. [常见问题 FAQ](#10-常见问题-faq)

---

## 1. 项目是什么

Cove 是一个**在终端中运行的 AI 编程助手**。你输入自然语言（"帮我写一个 HTTP 服务器"），它调用大模型（Claude/GPT）理解你的意图，然后：

1. **读取你的代码** (`read` 工具)
2. **搜索代码库** (`grep`, `glob` 工具)
3. **创建/修改文件** (`write`, `edit` 工具)
4. **执行命令** (`bash`, `powershell` 工具)
5. **管理任务** (`todowrite` → `execute_plan` 工作流)
6. **多轮迭代**：模型可能连续调用 10-30 轮工具，逐步完成任务

**三大交互模式**：
- **TUI 模式**（默认）：全屏 Bubble Tea 界面，分屏显示对话和工具进度
- **Headless 模式**（非交互）：无 UI，逐行读取 stdin，适合管道/重定向/脚本
- **Mobile 模式**：Android 手机控制引擎，通过 gomobile 绑定

---

## 2. 30 秒快速跑起来

```bash
# 1. 克隆
git clone https://github.com/liuzhixin405/cove
cd cove

# 2. 配置 API Key（二选一）
# Anthropic:
mkdir -p ~/.cove && echo '{"model":"claude-sonnet-4-20250514","provider":{"name":"anthropic","api_key":"sk-ant-你的key"}}' > ~/.cove/config.json
# OpenAI 兼容:
mkdir -p ~/.cove && echo '{"model":"gpt-4o","provider":{"name":"openai","api_key":"sk-你的key"}}' > ~/.cove/config.json

# 3. 构建并运行
go build -o cove ./cli/cove/
./cove                    # TUI 模式（默认）
./cove --no-tui           # headless 无 UI 模式
echo "hello" | ./cove     # 管道模式(自动 headless)

# 4. 运行所有测试
go test ./...
```

**开发依赖**：
- Go 1.22+（go.mod 写 1.25.0，实际 1.22 即可编译）
- Git（工作树隔离、检查点功能需要）
- ripgrep（`rg`，grep 工具底层调用，可选但建议安装）

---

## 3. 项目文件结构一览

```
cove/
│
├── go.mod                          # Go 模块定义（依赖极少，主要是 Bubble Tea + chromedp）
├── go.sum
│
├── cli/cove/                       # ★ 命令行入口（程序起点）
│   ├── main.go                     #   入口 + 参数解析 + 主流程编排 (501行)
│   ├── app_bootstrap.go            #   启动引导：创建所有子系统 (178行)
│   ├── registry.go                 #   工具注册表：注册所有内置工具 (87行)
│   ├── headless.go                 #   非交互前端（管道/重定向/--no-tui）
│   ├── repl_tui.go                 #   TUI 交互桥接（默认模式）(785行)
│   └── chat_interaction.go         #   单次对话处理 (139行)
│
├── internal/                       # ★ 核心实现（所有 .go 文件都在这里）
│   │
│   ├── api/                        # AI 模型 API 抽象层
│   │   ├── provider.go             #   Provider 接口定义 + ChatRequest/Response 类型
│   │   ├── provider_catalog.go     #   内置 Provider 目录（模型→Provider 映射）
│   │   ├── anthropic.go            #   Anthropic Messages API 实现 (433行)
│   │   ├── openai_compat.go        #   OpenAI Chat Completions 兼容实现 (616行)
│   │   ├── keypool.go              #   API Key 池（多 Key 轮转 + 故障标记）
│   │   ├── ratelimit.go            #   速率限制追踪（解析响应头）
│   │   ├── retry.go                #   指数退避重试 (69行)
│   │   ├── fallback.go             #   模型故障转移 (272行)
│   │   ├── router.go               #   模型路由（多策略链式决策）
│   │   └── prompt_cache.go         #   Anthropic prompt cache 断点注入
│   │
│   ├── engine/                     # ★ 核心引擎（项目灵魂）
│   │   ├── engine.go               #   引擎主逻辑 (1576行)：RunMessageWithStream、executeTool
│   │   ├── activity.go             #   活动追踪 + 卡顿检测
│   │   ├── review.go               #   后台对话回顾 + 自动学习
│   │   ├── masker.go               #   工具输出遮蔽（敏感信息替换）
│   │   ├── nextspeaker.go          #   下一说话人预测（多 Agent 协作）
│   │   ├── loopdetect.go           #   循环检测器 (145行)
│   │   ├── loopdetect_test.go      #   循环检测器测试
│   │   ├── compressor.go           #   上下文压缩器 (208行)
│   │   └── engine_test.go          #   引擎测试 (931行)
│   │
│   ├── tool/                       # 工具系统
│   │   ├── tool.go                 #   Tool 接口定义 + Runtime/Context 类型
│   │   ├── registry.go             #   工具注册表
│   │   ├── bash.go                 #   执行 Shell 命令
│   │   ├── powershell.go           #   执行 PowerShell（Windows）
│   │   ├── read.go                 #   读取文件
│   │   ├── write.go                #   写入文件（覆盖）
│   │   ├── edit.go                 #   精确字符串替换
│   │   ├── grep.go                 #   正则搜索（底层 ripgrep）
│   │   ├── glob.go                 #   文件名匹配搜索
│   │   ├── webfetch.go             #   网页抓取 + headless 浏览器
│   │   ├── advanced_tools_task_core.go    # task/sleep/send_message 等高级工具
│   │   ├── advanced_tools_agent_skill.go  # agent/skill 子代理工具
│   │   └── advanced_tools_plan_worktree.go # plan_mode/execute_plan/worktree
│   │
│   ├── permission/                 # 权限系统
│   │   ├── permission.go           #   权限模式 + 决策引擎
│   │   └── classifier.go           #   命令危险等级分类器
│   │
│   ├── plan/                       # 计划系统
│   │   ├── plan.go                 #   计划数据结构
│   │   └── executor.go             #   计划执行器
│   │
│   ├── session/                    # 会话持久化
│   │   ├── store.go                #   文件存储（~/.cove/sessions/*.json）
│   │   └── store_test.go
│   │
│   ├── memory/                     # 记忆系统
│   │   ├── store.go                #   BM25 + 向量存储
│   │   ├── bm25.go                 #   BM25 关键词检索
│   │   └── embed.go                #   伪嵌入生成
│   │
│   ├── skills/                     # 技能系统
│   │   ├── skills.go               #   技能加载 + 条件注入
│   │   └── skills_test.go
│   │
│   ├── hooks/                      # 生命周期钩子
│   │   └── hooks.go                #   PreToolUse/PostToolUse/SessionStart 等
│   │
│   ├── mcp/                        # MCP 协议集成
│   │   ├── pool.go                 #   连接池管理
│   │   └── client.go               #   MCP 客户端（stdio + HTTP）
│   │
│   ├── tui/                        # 全屏 TUI 界面
│   │   ├── tui.go                  #   Bubble Tea Model (757行)
│   │   ├── app.go                  #   程序包装器
│   │   └── styles.go               #   样式 + 布局渲染 (313行)
│   │
│   ├── context/                    # 项目上下文
│   │   └── context.go              #   文件树、Git 状态、Repo Map 收集
│   │
│   ├── config/                     # 配置管理
│   │   └── config.go               #   配置加载 + 迁移 + 验证 (229行)
│   │
│   ├── cost/                       # 成本追踪
│   │   └── tracker.go              #   Token → 美元费用计算
│   │
│   ├── token/                      # Token 估算
│   │   └── token.go                #   简单字符/词数估算
│   │
│   ├── checkpoint/                 # 文件快照
│   │   └── checkpoint.go           #   Git 自动 checkpoint（write/edit 前）
│   │
│   ├── guardrail/                  # 安全护栏
│   │   └── guardrail.go            #   工具调用前后的安全检查
│   │
│   ├── diagnostic/                 # 系统诊断
│   │   ├── checker.go              #   环境健康检查
│   │   ├── recorder.go             #   运行时事件记录
│   │   └── errors.go               #   错误定义
│   │
│   ├── log/                        # 日志
│   │   └── logger.go               #   分级日志（Debug/Warn/Error）
│   │
│   ├── termui/                     # 终端输出样式组件
│   │   ├── style.go                #   颜色与样式
│   │   ├── io.go                   #   输出打印封装
│   │   └── indicator.go            #   状态指示器
│   │
│   ├── command/                    # 斜杠命令系统
│   │   └── （/help, /exit, /doctor, /compact, /cost...）
│   │
│   ├── notes/                      # 会话笔记
│   │   └── notes.go                #   自动记录决策 + 错误
│   │
│   ├── extract/                    # 内容提取
│   │   └── extract.go              #   从对话中提取结构化信息
│   │
│   ├── dream/                      # 反思系统
│   │   └── dream.go                #   定期让 AI 反思并巩固记忆
│   │
│   ├── plugin/                     # 插件系统
│   │   └── plugin.go               #   外部插件加载
│   │
│   ├── repomap/                    # 仓库地图
│   │   └── repomap.go              #   AST 级别代码结构分析
│   │
│   ├── delegate/                   # 任务委托
│   │   └── delegate.go             #   跨代理任务分发
│   │
│   ├── onboarding/                 # 新手引导
│   │   └── onboarding.go           #   首次使用配置向导
│   │
│   ├── state/                      # 应用状态
│   │   └── state.go                #   状态枚举 + 持久化
│   │
│   └── browser/                    # 浏览器工具
│       └── browser.go              #   chromedp headless 浏览器
│
├── mobile/                         # ★ 移动端引擎（Android）
│   ├── cove.go                     #   MobileEngine (203行)
│   └── mobileapi/
│       ├── mobileapi.go            #   自包含 API 层（不依赖 internal）(356行)
│       └── mobileapi_test.go
│
├── testdata/                       # 测试数据
├── scripts/                        # 辅助脚本
├── docs/                           # 文档
├── README.md
├── DEVELOPMENT_GUIDE.md            # 现有开发指南
├── DEVELOPMENT_DESIGN.md           # 现有优化设计文档
└── build.bat                       # Windows 构建脚本
```

---

## 4. 从 main 到 Engine：启动全流程

这是理解整个项目的关键。以下是程序启动的完整调用链：

```
main()                                     // cli/cove/main.go:75
  │
  ├─ 1. 解析命令行参数                      // --version, --help, --no-tui, --debug...
  │
  ├─ 2. bootstrapApp(debugMode)            // app_bootstrap.go:40
  │     │
  │     ├─ config.Load()                   // 加载 ~/.cove/config.json + .cove.json
  │     ├─ ctxt.Collect()                  // 收集项目上下文（Git状态、文件树、Repo Map）
  │     ├─ permission.NewManager()         // 权限管理器
  │     ├─ hooks.NewManager()              // 钩子管理器
  │     ├─ skills.NewManager()             // 技能管理器
  │     ├─ memory.NewStore()               // 记忆存储
  │     ├─ mcp.NewPool()                   // MCP 连接池
  │     ├─ registerAllTools(mcpPool)       // 注册所有工具 → tool.Registry
  │     └─ engine.New(engine.Config{...})  // ★ 创建引擎
  │           │
  │           ├─ tool.NewRegistry()        // 工具注册表
  │           ├─ api.DetectProvider()      // 检测并创建 API Provider
  │           ├─ session.NewStore()        // 会话存储
  │           ├─ cost.NewTracker()         // 成本追踪
  │           ├─ notes.New()               // 会话笔记
  │           ├─ guardrail.New()           // 安全护栏
  │           ├─ checkpoint.New()          // Git 检查点
  │           └─ 返回 *Engine
  │
  ├─ 3. eng.SetProjectContext(projCtx)     // 注入项目上下文
  ├─ 4. eng.WirePlanExecutor()             // 连接计划执行器
  │
  ├─ 5. useTUI() 判断交互模式
  │     │
  │     ├─ true  → runTUI()                // repl_tui.go（默认）
  │     │           │
  │     │           ├─ 创建 TUI 程序（Bubble Tea）
  │     │           ├─ 创建任务队列（串行处理用户消息）
  │     │           ├─ 启动 goroutine: 从队列取消息 → eng.RunMessageWithStream()
  │     │           └─ 引擎回调 → App.Send*() → TUI 更新
  │     │
    │     └─ false → runHeadless()           // headless.go（管道/--no-tui）
  │                 │
  │                 ├─ 无限循环：显示提示符 → 读用户输入
  │                 ├─ 调用 eng.RunWithStream(userInput, onDelta)
  │                 └─ 引擎输出通过 onDelta 回调直接打印
  │
  └─ 6. 退出：保存会话、清理资源
```

**关键理解**：
- `Engine` 是整个程序的核心——它拥有所有子系统的引用
- TUI/REPL 只是 UI 壳，真正的逻辑全部在 Engine 里
- `RunMessageWithStream()` 是 Engine 最重要的方法，处理"一条用户消息 → 多轮 AI 调用 → 工具执行 → 最终响应"的完整循环

---

## 5. 核心模块逐层拆解

### 5.1 配置系统 (config)

**文件**: `internal/config/config.go` (229行)

```go
type Config struct {
    Model          string                     // AI 模型名称
    Provider       ProviderConfig             // Provider 配置
    PermissionMode string                     // 权限模式: default/auto/strict/yolo
    MaxBudgetUsd   float64                    // 最大预算（美元）
    ThinkingTokens int                        // 思考预算
    Debug          bool                       // 调试模式
    Verbose        bool                       // 详细输出
    SystemPrompt   string                     // 自定义系统提示
    MCPServers     map[string]MCPServerConfig  // MCP 服务器配置
}
```

**加载优先级**（后者覆盖前者）：
1. 默认值 (`DefaultConfig()`)
2. `~/.cove/config.json`（全局配置）
3. `./.cove.json`（项目级覆盖）
4. 环境变量（如 `COVE_TUI=0`）
5. 命令行参数（如 `--model gpt-4o`）

**关键代码**：
```go
func Load() (*Config, error) {
    cfg := DefaultConfig()
    // 1. 读全局配置
    data, _ := os.ReadFile("~/.cove/config.json")
    json.Unmarshal(data, cfg)
    // 2. 读项目配置
    data, _ = os.ReadFile("./.cove.json")
    json.Unmarshal(data, &override)
    // 3. 合并 + 默认值回填
    applyDefaults(cfg)
    return cfg, nil
}
```

**Provider 配置的子结构**：
```go
type ProviderConfig struct {
    Name    string   // "anthropic" | "openai" | "deepseek" | ...
    APIKey  string   // 单个 Key
    APIKeys []string // 多个 Key（KeyPool 轮转）
    BaseURL string   // 自定义端点（如本地 LLM）
}
```

---

### 5.2 API 层 (api)

**核心接口**：`Provider` — 整个项目对任何 AI 模型的唯一抽象。

```go
// internal/api/provider.go
type Provider interface {
    Name() string                                                    // 内部名称
    DisplayName() string                                             // 显示名称
    Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)       // 非流式
    ChatStream(ctx context.Context, req ChatRequest, handler StreamHandler) (*ChatResponse, error) // 流式
    Validate() error                                                 // 验证配置
}
```

**两个实现**：

| 文件 | 实现 | API 端点 | 认证方式 |
|------|------|----------|----------|
| `anthropic.go` | Anthropic Messages API | `https://api.anthropic.com/v1/messages` | `x-api-key` header |
| `openai_compat.go` | OpenAI Chat Completions | `{base_url}/v1/chat/completions` | `Authorization: Bearer` |

**消息类型**：
```go
type Message struct {
    Role             string      // "system" | "user" | "assistant" | "tool"
    Content          string      // 文本内容
    Parts            []MessagePart // 多模态：图片、文件
    ReasoningContent string      // 模型思维链（Claude extended thinking）
    ToolCalls        []ToolCall  // AI 请求的工具调用
    ToolCallID       string      // 工具消息的关联 ID
    Name             string      // 工具名称
    CacheControl     string      // Anthropic prompt caching 标记
}

type ToolCall struct {
    ID    string         // 唯一 ID
    Name  string         // 工具名
    Input map[string]any // 参数
}
```

**请求/响应流**：
```
Engine 构建 ChatRequest → Provider.ChatStream() → 逐块返回 StreamEvent
                                                          │
                              ┌───────────────────────────┘
                              ▼
              StreamEvent{Type:"delta", Delta:"..."}    → onDelta 回调
              StreamEvent{Type:"reasoning", ...}         → onReasoning 回调
              StreamEvent{Type:"tool_call", ToolCall:...} → 累积到 ChatResponse.ToolCalls
```

**KeyPool** (`keypool.go`): 管理多个 API Key，自动轮转跳过被限流的 Key：
```
KeyOK → KeyExhausted（429 后冷却 60s）→ KeyOK（冷却恢复）
KeyOK → KeyDead（401/403 永久失效）
```

**重试** (`retry.go`): 指数退避，1s→2s→4s→8s，最多 3 次。可重试：网络超时、5xx、429。不可重试：401、403。

**速率限制追踪** (`ratelimit.go`): 解析 API 响应头，在 TUI 状态栏显示：
```
请求: 150/200(75%) 重置:1m30s | Token: 45K/100K(45%)
```

---

### 5.3 引擎层 (engine) — 项目心脏

**文件**: `internal/engine/engine.go` (1576行)

Engine 是所有子系统的**编排中心**。它拥有所有其他模块的引用，协调它们一起工作。

```go
type Engine struct {
    // === 核心依赖 ===
    provider     api.Provider        // AI 模型接口
    registry     *tool.Registry      // 工具注册表
    config       Config              // 引擎配置
    messages     []api.Message       // ★ 当前对话历史（最重要的状态）

    // === 子系统 ===
    perm         *permission.Manager // 权限管理
    costTracker  *cost.Tracker      // 成本追踪
    memStore     *memory.Store      // 记忆存储
    skillMgr     *skills.Manager    // 技能管理
    hookMgr      *hooks.Manager     // 钩子管理
    guardrails   *guardrail.Tracker // 安全护栏
    sessionNotes *notes.SessionNotes // 会话笔记
    extractRunner *extract.Runner   // 自动记忆提取
    dreamRunner  *dream.Runner      // 记忆巩固
    cpMgr        *checkpoint.Manager // Git 检查点
    rateLimits   *api.RateLimitTracker // 限流追踪

    // === 循环检测 ===
    loopDetector *LoopDetector      // 增强循环检测器（P0）
    loopHistory  []string           // 旧循环检测历史
    iterCount    int                // 当前迭代数

    // === 其他 ===
    projCtx      *ctxt.ProjectContext // 项目上下文
    fileHistory  map[string]bool     // 文件修改记录
    pendingSteer string              // 待注入的用户引导
    consecutiveErrors int            // 连续错误计数

    // === 回调（UI 注入） ===
    OnEngineOutput    func(line string)                                      // 引擎日志
    PermissionPrompt  func(toolName string, input map[string]any, reason string) bool // 权限询问
    OnPermissionPause func()                                                 // 权限暂停
    OnPermissionDone  func()                                                 // 权限恢复
    OnToolProgress    func(toolName, chunk string)                           // 工具实时输出
}
```

#### 5.3.1 核心方法：RunMessageWithStream

这是理解整个项目最重要的方法（约 300 行逻辑）。它处理**一条用户消息的完整生命周期**：

```go
func (e *Engine) RunMessageWithStream(
    ctx context.Context,
    userMessage api.Message,
    onDelta func(delta string),          // 流式文本回调
    onReasoning func(reasoning string),   // 思维链回调
) (string, error) {
```

**完整执行流程图**：

```
用户消息 "帮我写一个 HTTP 服务器"
  │
  ▼
┌───────────────────────────────────────────────────────┐
│ 1. 保存旧消息（用于失败回滚）                            │
│    prevMessages = e.messages                           │
│    e.messages = append(e.messages, userMessage)        │
│    e.saveSession()                                     │
├───────────────────────────────────────────────────────┤
│ 2. 重建系统提示（含项目上下文、记忆、技能）               │
│    sp = e.SystemPrompt()                               │
│    toolDefs = e.buildAPIToolDefs()                     │
├───────────────────────────────────────────────────────┤
│ 3. 重置循环检测器                                       │
│    e.loopDetector.Reset()                              │
├───────────────────────────────────────────────────────┤
│ 4. ★ 主循环（最多 200 轮迭代）                          │
│    for iter := 0; iter < MaxIterations; iter++ {       │
│                                                         │
│      a. 处理待注入的用户引导 (Steer)                      │
│         if steer := e.drainPendingSteer(); steer != "" { │
│             注入到最后一条 tool 消息末尾                    │
│         }                                               │
│                                                         │
│      b. 压缩过长的对话历史                                │
│         e.compressHistory()                             │
│                                                         │
│      c. ★ 调用 AI 模型                                  │
│         req = ChatRequest{                              │
│             Model: e.currentModel,                      │
│             Messages: e.messages,                       │
│             SystemBase: sp,                             │
│             Tools: toolDefs,                            │
│         }                                               │
│         resp = e.provider.ChatStream(ctx, req, onDelta) │
│                                                         │
│      d. 更新成本追踪                                     │
│         e.costTracker.Add(resp.InputTokens, ...)        │
│                                                         │
│      e. 判断响应类型                                     │
│         ├─ 没有工具调用 → 返回文本响应 ✓                  │
│         │   触发 turn-end pipeline（review, extract）    │
│         │   return resp.Content                         │
│         │                                               │
│         └─ 有工具调用 → 继续                              │
│                                                         │
│      f. 添加 assistant 消息                              │
│         e.messages = append(e.messages, assistantMsg)   │
│                                                         │
│      g. ★ 循环检测（工具调用指纹）                        │
│         loopFp = fingerprintToolCalls(resp.ToolCalls)   │
│         如果同一模式 ≥3 次/5 轮 → 注入引导                 │
│                                                         │
│      h. ★ 执行所有工具（并行）                            │
│         for each toolCall:                              │
│           ├─ 权限检查                                    │
│           ├─ 安全护栏检查                                │
│           ├─ Git checkpoint（write/edit 前）             │
│           ├─ 钩子 PreToolUse/PostToolUse                │
│           ├─ 实际执行 t.Call(ctx, input, tctx)          │
│           ├─ 结果截断（自适应 Token 预算）                │
│           ├─ 技能条件注入                                │
│           └─ 循环检测（输出内容哈希）                     │
│                                                         │
│      i. 添加 tool 消息到对话历史                          │
│         e.messages = append(e.messages, toolResultMsg)  │
│                                                         │
│      j. 断路器检查（连续工具失败 ≥3 → 引导）               │
│                                                         │
│      k. 检查是否需要压缩                                 │
│         if totalTokens > 64000 → e.Compact(ctx)         │
│    }                                                    │
├───────────────────────────────────────────────────────┤
│ 5. 超过最大迭代次数 → 返回错误                            │
└───────────────────────────────────────────────────────┘
```

#### 5.3.2 工具执行：executeTool

每个工具调用都走这个方法：

```
executeTool(tc ToolCall) → string
  │
  ├─ 1. 查找工具: e.registry.Find(tc.Name)
  ├─ 2. 记录活动（用于卡顿检测）
  ├─ 3. 钩子: PreToolUse
  ├─ 4. 护栏: BeforeCall（可能 Block 或 Warn）
  ├─ 5. Git checkpoint（write/edit）
  ├─ 6. 构建 tool.Context（Cwd, PermissionMode, Runtime...）
  ├─ 7. 高危命令分类（bash + classifier）
  ├─ 8. 参数验证: t.Validate(input)
  ├─ 9. 权限检查: t.CheckPermissions → e.perm.Check
  │     └─ DAsk → 调用 PermissionPrompt 回调（询问用户）
  ├─ 10. 执行: t.Call(ctx, input, tctx)
  │     └─ 错误 → 重试一次（瞬时错误）
  ├─ 11. 护栏: AfterCall
  ├─ 12. 钩子: PostToolUse
  ├─ 13. 自适应截断（bash=3K, read=6K, webfetch=3K）
  ├─ 14. 技能条件注入（根据文件类型匹配技能提示）
  └─ 15. 返回结果字符串
```

#### 5.3.3 并发工具执行

Engine 支持并行执行多个工具调用：

```go
// 分两组：
// - IsConcurrencySafe → goroutine + WaitGroup（最多 8 个并发）
// - 非并发安全的 → 串行
// 特殊：write/edit 作用于不同文件 → 也可以并行
```

#### 5.3.4 系统提示构建 (SystemPrompt)

每次引擎运行前，动态构建系统提示：

```
你是 Cove，一个 AI 编程助手。必须使用工具完成任务。
可用工具: [bash, read, write, edit, grep, glob, webfetch, ...]
项目上下文: CWD + Git 分支 + 文件树 + Repo Map
技能提示: [已加载技能列表]
记忆: [相关记忆]
会话笔记: [当前对话的决策和错误]
```

#### 5.3.5 Steer 机制

允许用户在 Agent 运行时**注入实时引导**（不中断正在执行的工具）：

```go
eng.Steer("用更简单的方法")
// → 给下一条 tool 消息添加 "[用户指引] 用更简单的方法"
// → AI 在下一次迭代中看到这个引导
```

#### 5.3.6 模型故障转移 (Model Fallback)

当主模型失败时自动切换到备用模型：

```go
// 在 RunMessageWithStream 中
if err != nil && e.fallback != nil {
    // 尝试备用模型
    e.currentModel = fallbackModel
    e.fallbackActive = true
    continue  // 重试本轮
}
```

---

### 5.4 工具系统 (tool)

**Tool 接口**（`internal/tool/tool.go`）：

```go
type Tool interface {
    Def() Def                                      // 工具元数据
    Validate(input Input) string                   // 参数验证（返回空=通过）
    CheckPermissions(input Input, tctx Context) PermissionDecision // 权限决策
    Call(ctx context.Context, input Input, tctx Context) (Result, error) // 实际执行
}

type Def struct {
    Name              string           // "bash", "read", "write"...
    Aliases           []string         // 别名
    Description       string           // 给 AI 看的描述
    Prompt            string           // 使用提示
    InputSchema       json.RawMessage  // JSON Schema 参数定义
    IsReadOnly        bool             // 只读？
    IsConcurrencySafe bool             // 可并行？
    UserFacingName    string           // 显示名称
}
```

**内置工具完整列表**（30+ 工具）：

| 工具 | 文件 | 功能 | 只读 | 并发安全 |
|------|------|------|------|----------|
| `read` | read.go | 读取文件（支持行号范围） | ✅ | ✅ |
| `write` | write.go | 创建/覆盖文件 | ❌ | 不同文件可 |
| `edit` | edit.go | 精确字符串替换 | ❌ | 不同文件可 |
| `bash` | bash.go | 执行 Shell 命令 | ❌ | ❌ |
| `powershell` | powershell.go | 执行 PowerShell | ❌ | ❌ |
| `grep` | grep.go | 正则搜索（底层 rg） | ✅ | ✅ |
| `glob` | glob.go | 文件名 glob 搜索 | ✅ | ✅ |
| `webfetch` | webfetch.go | 抓取网页 | ✅ | ✅ |
| `browser` | webfetch.go | headless 浏览器 | ✅ | ❌ |
| `websearch` | webfetch.go | 网络搜索 | ✅ | ✅ |
| `question` | adv_*.go | 询问用户 | ✅ | ✅ |
| `todowrite` | adv_*.go | 创建任务列表 | ✅ | ✅ |
| `execute_plan` | adv_*.go | 执行计划 | ❌ | ❌ |
| `plan_mode` | adv_*.go | 进入计划模式 | ✅ | ✅ |
| `exit_plan_mode` | adv_*.go | 退出计划模式 | ✅ | ✅ |
| `worktree` | adv_*.go | 创建 Git 工作树 | ❌ | ❌ |
| `exit_worktree` | adv_*.go | 退出工作树 | ❌ | ❌ |
| `task` | adv_*.go | 创建后台任务 | ❌ | ✅ |
| `task_list` | adv_*.go | 列出任务 | ✅ | ✅ |
| `task_update` | adv_*.go | 更新任务 | ✅ | ✅ |
| `task_stop` | adv_*.go | 停止任务 | ❌ | ✅ |
| `task_get` | adv_*.go | 查看任务详情 | ✅ | ✅ |
| `task_output` | adv_*.go | 获取任务输出 | ✅ | ✅ |
| `agent` | adv_*.go | 创建子 Agent | ❌ | ✅ |
| `team_create` | adv_*.go | 创建 Agent 团队 | ❌ | ✅ |
| `team_delete` | adv_*.go | 删除团队 | ❌ | ✅ |
| `skill` | adv_*.go | 调用技能 | ✅ | ✅ |
| `skills_list` | adv_*.go | 列出技能 | ✅ | ✅ |
| `skill_view` | adv_*.go | 查看技能详情 | ✅ | ✅ |
| `sleep` | adv_*.go | 暂停等待 | ✅ | ✅ |
| `brief` | adv_*.go | 生成摘要 | ✅ | ✅ |
| `send_message` | adv_*.go | 发送消息 | ✅ | ✅ |
| `lsp` | adv_*.go | LSP 调用 | ✅ | ✅ |
| `cron` | adv_*.go | 定时任务 | ❌ | ✅ |
| `mcp` | 动态 | 调用 MCP 工具 | 取决于工具 | 取决于工具 |

**工具注册**(`registry.go`)：
```go
type Registry struct {
    tools    map[string]Tool
    toolList []Tool         // 保持插入顺序
}
func (r *Registry) Register(t Tool)
func (r *Registry) Find(name string) (Tool, bool)
func (r *Registry) All() []Tool
```

**工具执行上下文 (tool.Context)**：
```go
type Context struct {
    Cwd              string           // 当前工作目录
    ToolUseID        string           // 工具调用 ID
    SessionID        string           // 会话 ID
    PermissionMode   string           // 权限模式
    AlwaysAllowRules map[string][]string
    AlwaysDenyRules  map[string][]string
    IsNonInteractive bool
    Debug            bool
    Runtime          *Runtime         // 运行时状态（任务、团队、计划等）
    OnProgress       func(chunk string) // 实时输出回调
}
```

**Runtime** — 工具间共享的运行时状态：
```go
type Runtime struct {
    PlanMode      bool
    WorktreeDir   string
    Tasks         map[string]*TaskRecord
    Teams         map[string]*TeamRecord
    CronSchedules map[string]*CronRecord
    Messages      []MessageRecord
    SkillPrompts  map[string]string
    AskUser       func(prompt string) string
    PlanExecuteFunc func(parallel bool) (string, error)
}
```

---

### 5.5 权限系统 (permission)

**四种模式**：
```
Default  — 写操作需确认（默认）
Auto     — 全部允许（自动）
Strict   — 全部需确认
Yolo     — 仅危险操作需确认
Bypass   — 引擎内部使用
Plan     — 计划执行期间使用
```

**权限决策链**：
```
1. tool.CheckPermissions() → Allow / Deny / Ask
2. e.perm.Check(toolName, input, toolDecision)
3. 如果是 Ask → 调用 PermissionPrompt 回调
4. 用户决定 → Allow / Deny
```

**命令分类器** (`classifier.go`)：分析 bash 命令的危险等级
```
CatSafe      — 读文件、查看状态
CatDangerous — rm -rf、curl|bash、修改系统文件
```

---

### 5.6 会话持久化 (session)

**存储位置**：`~/.cove/sessions/{session-id}.json`

```go
type Record struct {
    ID        string        // "session-1234567890"
    Title     string        // "New session"
    Model     string        // 使用的模型
    Messages  []api.Message // 完整对话历史
    TokensIn  int
    TokensOut int
    Cost      float64
}
```

**操作**：
```go
store.Save(record)           // 持久化到文件
store.Load(id)               // 从文件加载
store.List()                  // 列出所有会话
```

---

### 5.7 记忆系统 (memory)

**核心数据结构**：
```go
type Store struct {
    entries    []MemoryEntry  // 所有记忆条目
    bm25       *BM25Index     // BM25 关键词索引
    vectors    map[string][]float64 // 伪向量嵌入
}
```

**记忆来源**：
1. **自动提取** — 引擎每轮对话后，`extract.Runner` 调用 AI 分析对话，提取 `MEMORY: xxx` 标注的内容
2. **Dream 巩固** — 定期让 AI 回顾历史对话，合并相关记忆
3. **手动添加** — 用户可以通过 `question` 工具让 AI 记住某些信息

**检索**：BM25 关键词匹配 + 向量相似度 → 排序 → 注入到系统提示中

---

### 5.8 技能系统 (skills)

技能是预定义的、可复用的工作流。存储在 `.cove/skills/` 目录下的 Markdown 文件中。

```go
type Skill struct {
    Name        string
    Description string
    Prompt      string   // AI 看到的技能提示
    FilePattern string   // 触发条件（文件匹配）
}
```

**两个技能工具**：
- `skill` — AI 调用特定技能
- `skills_list` / `skill_view` — 列出/查看可用技能

**条件注入**：当工具操作的文件匹配某个技能的 `FilePattern` 时，自动在工具输出末尾附加该技能的提示。

---

### 5.9 钩子系统 (hooks)

**文件**: `internal/hooks/hooks.go`

支持的生命周期事件：
```go
SessionStart   // 会话开始
PreToolUse     // 工具调用前（可修改输入）
PostToolUse    // 工具调用后（可修改输出）
```

每个钩子可以是：
- **内置 Go 函数** — 直接注册
- **外部脚本** — 配置在 `~/.cove/hooks/` 下

---

### 5.10 MCP 协议集成 (mcp)

MCP (Model Context Protocol) 允许外部进程暴露工具给 Cove。

**架构**：
```
Cove Engine
  └─ mcp.Pool
       ├─ Server "filesystem" → stdio 连接 → npx @anthropic/mcp-filesystem
       ├─ Server "database"   → HTTP 连接 → localhost:3000
       └─ Server "memory"     → stdio 连接 → python memory_server.py
```

**工具暴露**：MCP 服务器的工具通过 `mcp` 工具代理暴露给 AI：
```
AI: tool_call("mcp", {serverName: "filesystem", toolName: "read_file", arguments: {...}})
→ Engine: mcpTool.Call() → Pool.Dispatch() → MCP Client → MCP Server
```

---

### 5.11 循环检测 (LoopDetector) — 实际实现

**文件**: `internal/engine/loopdetect.go` (394行)

**架构**: 3 层检测 + 自适应阈值 + 分级响应 + 只读豁免

```
┌─────────────────────────────────────────────────────────────┐
│  record(fingerprint, toolName, output, filesChanged)        │
│       → LoopResult (None / Warning / Fatal)                 │
│                                                             │
│  Layer 1a: 精确指纹匹配 (14轮滑动窗口, 阈值10)               │
│  ──────────────────────────────────────────                  │
│  sha256(toolName + ":" + json(input))                       │
│  相同指纹出现 ≥10/14轮 → 循环                                │
│                                                             │
│  Layer 1b: 模糊工具名匹配 (12轮滑动窗口, 阈值10)              │
│  ──────────────────────────────────────────                  │
│  相同工具名（不同参数）出现 ≥10/12轮 → 循环                   │
│                                                             │
│  Layer 2: 输出内容哈希 (40轮滑动窗口, 阈值8)                 │
│  ──────────────────────────────────────────                  │
│  sha256(output[:512]) 相同出现 ≥8/40轮 → 循环                │
│                                                             │
│  Layer 3: 停滞检测 (连续60轮)                                │
│  ──────────────────────────────────────────                  │
│  连续 60 轮无任何文件创建/修改 → 空转检测                     │
│                                                             │
│  只读工具豁免: read/grep/glob/lsp/webfetch/browser/task_list │
│  自适应阈值: Flash模型使用更敏感的 8/12, 8/10, 8/30, 50      │
│  分级响应: 前5次非致命注入引导, 超出则硬终止                   │
│  指纹重置: 注入引导后自动清空窗口                              │
└─────────────────────────────────────────────────────────────┘
```

**数据结构**：
```go
type LoopDetector struct {
    fpHistory       []string       // 工具调用指纹滑动窗口
    fpThresh        int            // 指纹阈值（默认 10，Flash 8）
    nameHistory     []string       // 工具名滑动窗口
    nameThresh      int            // 工具名阈值（默认 10，Flash 8）
    outHashes       []string       // 输出哈希滑动窗口
    outCounts       map[string]int // 哈希计数
    outThresh       int            // 输出阈值（默认 8，Flash 8）
    stagnationCount int            // 停滞轮数
    stagnationLimit int            // 停滞限制（默认 60，Flash 50）
    breakCount      int            // 已触发次数
    maxBreaks       int            // 最大允许次数（默认 5）
    windowSize      int            // 窗口基数（默认 14，Flash 12）
    isFlashModel    bool           // 是否使用自适应阈值
}

func (ld *LoopDetector) Record(fingerprint string, toolName string, output string, filesChanged bool) LoopResult
func (ld *LoopDetector) Reset()
```

**引擎集成**：在 `engine.go` 主循环的 `executeToolCalls` 后调用 `loopDetector.Record()` 检查。旧 `fingerprintToolCalls()` 已被新 LoopDetector 完全替代。

---

### 5.12 上下文压缩 (ChatCompressor)

**文件**: `internal/engine/compressor.go` (258行)

**双层架构**：

**Layer 1 — 轻量修剪**（免费，无 API 调用）：
```
遍历消息历史中较旧的 tool 结果
→ 超过 300 字符的截断为 1 行摘要
→ 保留 filePath/command 等关键信息
→ 替换原内容为简短摘要
```

**Layer 2 — AI 摘要**（有 API 调用）：
```
触发条件: 当前 Token 使用量 > 模型上限的 50%
执行:
  1. 找到分割点（以 assistant 消息锚定，保留最近 30% 消息）
  2. 调用模型生成旧消息的结构化摘要
  3. 摘要包含: Key Decisions / Files / Task Status / Errors
  4. 替换旧消息为 "[对话摘要] ..."
  5. 返回压缩后的消息列表
```

**安全机制**：
- **安全分割**：始终以 assistant 消息锚定，避免连续两条 user 消息（API 400 错误）
- **优雅降级**：AI 摘要失败 → 回退到简单截断
- **触发阈值**：50% 而非 80%，给压缩后留 buffer

**代码核心**：
```go
type ChatCompressor struct {
    enabled         bool
    tokenThreshold  float64   // 默认 0.5（50%）
    keepFraction    float64   // 默认 0.3（保留最近 30%）
    maxFunctionRespTokens int // 默认 50000
}

func (cc *ChatCompressor) Compress(ctx, messages, tokenLimit, provider) → (result, compressedMessages, error)
```

**用户感知**：压缩触发时显示 `📦 对话压缩完成: 85 → 32 条消息`

---

### 5.12a 模型故障转移 (ModelFallback)

**文件**: `internal/api/fallback.go` (272行)

**问题**：单模型不可用（如 Anthropic 全局限流）导致对话中断。

**方案**：跨模型自动降级（Anthropic → OpenAI → OpenRouter）。

```
TryChat(ctx, req) → (resp, usedProvider, error)
  ├─ 1. 用当前 provider 尝试
  ├─ 2. 失败? 标记 unavailable + cooldown
  ├─ 3. 切换到下一个可用 provider
  └─ 4. 所有都不可用? → 返回错误

Cooldown: 被限流的 provider 在 60s 后自动恢复
```

**状态管理**：
| 状态 | 含义 | 恢复方式 |
|------|------|----------|
| `●` ProviderOK | 可用 | — |
| `○` ProviderDegraded | 降级（429/5xx） | 60s 冷却后自动 |
| `✕` ProviderUnavailable | 不可用（401/403） | 手动恢复 |

**关键设计**：API 调用期间释放锁，避免阻塞 `/status` 命令读取。

---

### 5.12b 工具输出掩码 (ToolOutputMasker)

**文件**: `internal/engine/masker.go` (≈200行)

**问题**：大型工具输出（如编译日志、搜索大量结果）膨胀上下文，浪费 Token。

**方案**：Hybrid Backward Scanned FIFO — 从后向前扫描，遮蔽旧的大工具输出。

```
Mask(history) → (result, newHistory)
  ├─ 1. 计算当前 Token 总量
  ├─ 2. 未超 protectionThreshold(50000) → 跳过
  ├─ 3. 从后向前扫描工具结果消息
  ├─ 4. 超 minPrunableThreshold(30000) 的旧结果 → 替换为占位符
  └─ 5. 保护最新一轮完整对话

豁免工具: question, todowrite, plan_mode, exit_plan_mode
```

---

### 5.12c 模型路由 (ModelRouter)

**文件**: `internal/api/router.go` (≈150行)

**问题**：简单任务（"解释这段代码"）和复杂任务（"重构整个模块"）使用同一个昂贵模型，浪费费用。

**方案**：根据任务复杂度自动选择模型。

```
用户消息 → 复杂度评估 → 选择模型
                  │
     ┌────────────┼────────────┐
     ▼            ▼            ▼
 cheap model   normal model  premium model
 (haiku)       (sonnet)      (opus / 长上下文)
```

**路由策略**（按优先级）：
1. **覆盖策略**：用户 `/model gpt-4o` → 使用指定模型
2. **分类器策略**：基于消息长度 + 关键词判断复杂度
3. **Fallback 策略**：当前模型不可用 → 强制切换
4. **默认策略**：使用配置的默认模型

---

### 5.12d 策略引擎 (PolicyEngine)

**文件**: `internal/permission/policy.go` (≈250行), `internal/permission/storage.go`

**问题**：每次工具调用都询问用户确认。

**方案**：策略持久化 + 细粒度规则匹配。

**规则模型**：
```go
type PolicyRule struct {
    ToolPattern string           // "read", "bash", "mcp_*_*"
    Decision    RuleDecision     // always_allow / always_deny / ask
    ParamRules  []ParamCondition // 参数级条件
    ExpiresAt   *time.Time       // 可选过期
}
```

**匹配优先级**：精确匹配 → 通配符匹配 → 回退到分类器

**持久化**：`~/.cove/policy.json`

---

### 5.12e 发言人预测 (NextSpeaker)

**文件**: `internal/engine/nextspeaker.go` (81行)

**问题**：AI 在完成任务后继续无意义调用工具。

**方案**：上下文感知的继续/停止决策。

```
判断逻辑:
  ├─ 检测终止信号: "task complete", "任务完成" 等短语
  ├─ 最大迭代限制: 默认 50 轮
  ├─ 扫描最近 3 条消息中的终止短语
  └─ 返回 shouldStop: true/false
```

**集成**：在 Engine 主循环中，每次工具调用后作为决策点。

---

### 5.12f 会话 Diff (SessionDiff)

**文件**: `internal/session/diff.go` (136行)

**问题**：需要追踪会话中的变更（工具调用、文件修改、Token 消耗）。

**方案**：轻量级快照对比系统。

```go
type SessionDiff struct {
    OldTokens    int      // 之前的 Token 数
    NewTokens    int      // 新的 Token 数
    MsgCount     int      // 消息数变化
    AddedTools   []string // 新增工具
    RemovedTools []string // 移除工具
    AddedFiles   []string // 新增文件
    RemovedFiles []string // 移除文件
}
```

**文件提取**：自动从 ToolCalls 参数中提取文件路径。

---

### 5.12g 本地遥测 (Telemetry)

**文件**: `internal/telemetry/telemetry.go` (132行)

**问题**：需要了解使用情况以优化产品。

**方案**：本地事件记录（非匿名上报）。

**特性**：
- 结构化事件（类型、时间戳、数据）
- 本地存储 `~/.cove/telemetry.json`
- 上限 1000 条，超出裁剪
- 选择加入（默认关闭）
- 轻量级，不包含敏感数据

---

### 5.12h 安全检查 (Safety)

**文件**: `internal/permission/safety.go` (集成在 Permission 中)

**特性**：
- 敏感命令检测（rm -rf, dd, mkfs 等）
- 路径安全校验（防止 `../` 遍历）
- 敏感信息泄露检测（API key、密码）

**集成**：作为权限决策的前置检查，与 PolicyEngine 联动。

---

### 5.13 TUI 界面层 (tui)

**文件**: `internal/tui/tui.go` (757行)

基于 **Bubble Tea** (Elm Architecture) 的全屏终端 UI。

**布局**：
```
┌────────────────────────────────────────────┐
│  Cove v6.3.1 · claude-sonnet-4 · main*    │ ← 顶部状态栏
├────────────────────────────────────────────┤
│                                            │
│  用户: 帮我写一个 HTTP 服务器               │
│                                            │
│  Cove: 好的，我来创建...                    │ ← 对话流（可滚动）
│                                            │
│  ⏳ [write] server.go                      │ ← 工具进度
│  ✓ [write] server.go (1.2 KB)              │
│                                            │
├────────────────────────────────────────────┤
│  > 用户输入区...                            │ ← 底部输入框
├────────────────────────────────────────────┤
│  📊 in:1.2K out:3.4K | 💰 0.02/10.00 USD  │ ← 底部状态栏
└────────────────────────────────────────────┘
```

**数据流（TUI ↔ Engine）**：
```
User submits text in TUI
  → App.Submit(text) 回调
  → 推入 tuiJobQueue
  → Worker goroutine: eng.RunMessageWithStream(ctx, text, ...)
       │
       ├─ onDelta → App.SendDelta(tea.Msg)
       ├─ onReasoning → App.SendReasoning(tea.Msg)
       ├─ OnEngineOutput → App.SendOutput(tea.Msg)
       ├─ OnToolProgress → App.SendProgress(tea.Msg)
       └─ return → App.SendDone(tea.Msg)
```

---

### 5.14 移动端引擎 (mobile)

**文件**: `mobile/cove.go` + `mobile/mobileapi/`

为 Android 设计的轻量引擎。通过 **gomobile** 编译为 AAR，在 Kotlin 中调用。

**关键设计**：
- `mobileapi` 包**完全自包含**，不依赖 `internal/` 任何包
- 工具执行通过 `StreamCallback.OnToolCall()` 委托给 Kotlin 层
- 简化版 API 层（anthropic/openai 直接在 mobileapi 内实现）

```go
type MobileEngine struct {
    provider mobileapi.Provider
    model    string
    messages []mobileapi.Message
    toolDefs []ToolDef
}

type StreamCallback interface {
    OnDelta(delta string)
    OnToolCall(toolName string, inputJSON string) string
    OnDone(response string)
    OnReasoning(reasoning string)
    OnError(err string)
}
```

---

### 5.15 其他辅助模块

| 模块 | 文件 | 作用 |
|------|------|------|
| **cost** | `cost/tracker.go` | Token 费用计算（Anthropic $15/M, OpenAI $2.5-10/M） |
| **token** | `token/token.go` | 简单 Token 估算 + 结果截断 |
| **diagnostic** | `diagnostic/` | 系统健康检查 + 运行时错误记录 |
| **checkpoint** | `checkpoint/checkpoint.go` | write/edit 前自动 `git add . && git commit` |
| **guardrail** | `guardrail/guardrail.go` | 工具调用前的安全检查 |
| **notes** | `notes/notes.go` | 会话笔记（记录决策、错误、文件变更） |
| **extract** | `extract/extract.go` | 从对话中提取 MEMORY/SKILL 标注 |
| **dream** | `dream/dream.go` | 定期让 AI 反思，巩固长期记忆 |
| **termui** | `termui/` | ANSI 样式 + 输出/指示器封装 |
| **command** | `command/` | 斜杠命令（/help, /exit, /cost...） |
| **context** | `context/context.go` | 项目上下文收集（Git + 文件树 + RepoMap） |
| **repomap** | `repomap/repomap.go` | 代码库 AST 级别结构图 + 增量缓存 |
| **plugin** | `plugin/plugin.go` | 外部插件加载 |
| **log** | `log/logger.go` | 分级日志 |
| **browser** | `browser/browser.go` | chromedp headless 浏览器 |
| **state** | `state/state.go` | 应用状态持久化 |
| **onboarding** | `onboarding/onboarding.go` | 首次使用引导 |
| **delegate** | `delegate/delegate.go` | 任务委托 |
| **plan** | `plan/` | 计划依赖 DAG 构建 + 执行编排 |
| **fallback** | `api/fallback.go` | 跨模型故障转移（Anthropic → OpenAI → OpenRouter） |
| **router** | `api/router.go` | 双模型路由（简单/复杂任务智能分流） |
| **masker** | `engine/masker.go` | 工具输出掩码（Hybrid FIFO，节省 Token） |
| **compressor** | `engine/compressor.go` | 对话上下文压缩（AI 摘要 + 轻量修剪） |
| **loopdetect** | `engine/loopdetect.go` | 3 层循环检测 + 自适应阈值 + 停滞检测 |
| **nextspeaker** | `engine/nextspeaker.go` | 发言人预测（继续/停止决策） |
| **sessiondiff** | `session/diff.go` | 会话变更对比（工具/文件/Token） |
| **telemetry** | `telemetry/telemetry.go` | 本地事件记录（选择加入） |
| **safety** | `permission/safety.go` | 安全检查（敏感命令/路径/密钥） |
| **policy** | `permission/policy.go` | 策略引擎（持久化权限规则） |

### 5.16 后台回顾系统 (review)

**文件**: `internal/engine/review.go` (146行)

每轮对话结束后，引擎启动一个 **30 秒超时**的后台 goroutine：
1. 生成对话快照（最近消息摘要）
2. 调用 AI 分析："这段对话有没有值得记住的？"
3. AI 输出 `MEMORY: xxx` → 自动存入记忆库
4. AI 输出 `SKILL: xxx | steps...` → 自动存入技能库
5. 输出 `NONE` → 跳过

**节流策略**：至少新增 4 条消息才触发一次回顾（避免频繁调用 AI）。

### 5.17 计划执行系统 (plan)

**文件**: `internal/plan/plan.go` (219行)

当 AI 调用 `todowrite` 创建任务列表后，再调用 `execute_plan` 时：

```
execute_plan 工具
  ├─ plan.FromRuntime() → 扫描 Runtime.Tasks 中 pending 的任务
  ├─ 解析 "depends:task-1,task-2" 声明 → 构建依赖 DAG
  ├─ 循环检测（防止循环依赖）
  ├─ 拓扑排序 → 计算执行深度
  └─ 逐层执行：
       ├─ 深度 0（无依赖）→ 并行启动子 Agent（若 parallel=true）
       ├─ 子 Agent 完成 → 更新状态（done/failed）
       ├─ 深度 1（依赖已满足）→ 继续执行
       └─ 全部完成 → 返回汇总
```

**依赖声明格式**（在 todowrite 的 content 中）：
```
"depends:task-1,task-2 编写认证测试"
→ 该任务依赖 task-1 和 task-2 完成
```

### 5.18 斜杠命令系统 (command)

**文件**: `internal/command/` (12个文件)

命令通过接口注册，在 REPL 中通过 `/` 触发：

```go
type EngineView interface {
    Messages() []api.Message
    LoadMessages([]api.Message)
    SetSystemOverride(prompt string)
    SystemPrompt() string
    CostTracker() CostTrackerView
}
```

**内置命令列表**：
| 命令 | 文件 | 功能 |
|------|------|------|
| `/help` | commands_helpers.go | 显示帮助 |
| `/exit`, `/quit` | commands_session.go | 退出程序 |
| `/doctor` | diagnose.go | 系统诊断 |
| `/compact` | commands_session.go | 手动压缩上下文 |
| `/cost` | commands_session_misc.go | 显示费用统计 |
| `/memory` | commands_memory.go | 管理记忆 |
| `/skills` | commands_session_misc.go | 列出技能 |
| `/git` | commands_git.go | Git 操作 |
| `/resume` | resume_appstate_test.go | 恢复会话 |
| `/plugin` | commands_integrations.go | 管理插件 |
| `/model` | commands_session.go | 切换模型 |
| `/project` | commands_project.go | 项目管理 |

---

## 6. 核心数据流全追踪

以下追踪"用户输入 '在 server.go 中添加一个 /health 端点' → 最终完成"的完整数据流：

```
用户输入: "在 server.go 中添加一个 /health 端点"
  │
  ▼
[CLI] headless.go / repl_tui.go
  │  调用 eng.RunMessageWithStream(ctx, "在 server.go 中添加...", onDelta, nil)
  │
  ▼
[Engine] RunMessageWithStream()
  │
  ├─ 1. e.messages = append(e.messages, {role:"user", content:"在 server.go 中添加..."})
  │
  ├─ 2. 构建系统提示 SystemPrompt()
  │       = "你是 Cove AI 编程助手...\n"
  │       + "可用工具: bash, read, write, edit, grep...\n"
  │       + "项目: main.go, server.go, handlers/...\n"
  │       + "Git: main (clean)"
  │
  ├─ 3. 构建 API 请求
  │       ChatRequest{
  │           Model: "claude-sonnet-4-20250514",
  │           Messages: [system, user:"在 server.go 中添加..."],
  │           Tools: [{name:"read",...}, {name:"write",...}, {name:"edit",...}, ...]
  │       }
  │
  ├─ 4. ★ 第 1 次 API 调用
  │       provider.ChatStream(ctx, req, onDelta)
  │       → AI 响应: "我先看看 server.go 的内容"
  │       → ToolCalls: [{id:"tc1", name:"read", input:{filePath:"server.go"}}]
  │
  ├─ 5. 循环检测: fingerprintToolCalls([{read, server.go}]) → "read:server.go"
  │       历史上第 1 次出现 → 不触发
  │
  ├─ 6. 添加 assistant 消息
  │       e.messages = append(e.messages, {role:"assistant", tool_calls:[...]})
  │
  ├─ 7. ★ 执行工具 read
  │       executeTool({id:"tc1", name:"read", input:{filePath:"server.go"}})
  │       ├─ 权限: 只读 → Allow
  │       ├─ readTool.Call() → 读取文件内容
  │       ├─ 结果截断: 6000 tokens
  │       └─ 返回: "package main\n\nimport (...)\n\nfunc main() {..."
  │
  ├─ 8. 循环检测: loopDetector.RecordOutput("package main\n\nimport...")
  │       输出哈希: abc123 → 第 1 次 → 不触发
  │
  ├─ 9. 添加 tool 消息
  │       e.messages = append(e.messages, {role:"tool", tool_call_id:"tc1", content:"package main..."})
  │
  ├─ 10. ★ 第 2 次 API 调用（有了文件内容后）
  │       → AI 响应: "我看到 server.go 的结构了，我来添加 /health 端点"
  │       → ToolCalls: [{id:"tc2", name:"edit", input:{filePath:"server.go", oldString:"...", newString:"..."}}]
  │
  ├─ 11. 循环检测: fingerprintToolCalls([{edit, server.go, ...}]) → "edit:server.go:abc"
  │        历史上第 1 次 → 不触发
  │
  ├─ 12. 添加 assistant 消息
  │
  ├─ 13. ★ 执行工具 edit
  │       executeTool({id:"tc2", name:"edit", input:{...}})
  │       ├─ 权限: 写操作 → 检查模式 → Default → Ask
  │       ├─ e.perm.Check() → DAsk
  │       ├─ 调用 PermissionPrompt("edit", {filePath:"server.go"}, "will modify file")
  │       │     → TUI 显示: "⚠ 允许执行 edit: server.go? [y/N]"
  │       │     → 用户输入 y → 返回 true
  │       ├─ Git checkpoint（自动 git commit）
  │       ├─ editTool.Call() → 精确替换
  │       └─ 返回: "✓ File edited: server.go"
  │
  ├─ 14. 添加 tool 消息
  │
  ├─ 15. ★ 第 3 次 API 调用
  │       → AI 响应: "已添加 /health 端点！让我验证一下代码能否编译"
  │       → ToolCalls: [{id:"tc3", name:"bash", input:{command:"go build ./..."}}]
  │
  ├─ 16. 执行工具 bash
  │       executeTool({id:"tc3", name:"bash", input:{command:"go build ./..."}})
  │       ├─ 命令分类: go build → CatSafe → 允许
  │       ├─ bashTool.Call() → 执行 → "✓ Build successful"
  │       └─ 返回: "✓ Build successful"
  │
  ├─ 17. ★ 第 4 次 API 调用
  │       → AI 响应: "编译成功！/health 端点已添加完成。"
  │       → ToolCalls: [] (空)
  │
  ├─ 18. 没有工具调用 → 准备返回
  │       ├─ runTurnEndPipeline()
  │       │     ├─ extract.Runner: 分析对话 → 提取记忆
  │       │     └─ 如果没有新记忆 → 跳过
  │       ├─ recordSignals() → 记录决策
  │       ├─ saveSession() → 持久化
  │       └─ return "编译成功！/health 端点已添加完成。"
  │
  ▼
[CLI] onDelta 回调累积的完整响应文本
  │
  ▼
用户看到: "编译成功！/health 端点已添加完成。"
```

---

## 7. 如何添加新功能（实操教程）

### 7.1 添加一个新工具

假设我们要添加一个 `timer` 工具，让 AI 可以设置计时器。

**步骤**：

#### 第 1 步：创建工具文件

`internal/tool/timer.go`:
```go
package tool

import (
    "context"
    "fmt"
    "time"
)

type timerTool struct{ baseTool }

func NewTimerTool() Tool {
    return &timerTool{baseTool: *NewTool(Def{
        Name:        "timer",
        Description: "Set a timer. After the specified duration, the result will indicate the timer has elapsed.",
        InputSchema: json.RawMessage(`{
            "type": "object",
            "properties": {
                "seconds": {
                    "type": "integer",
                    "description": "Number of seconds to wait"
                }
            },
            "required": ["seconds"]
        }`),
        IsReadOnly:        true,  // 不修改文件
        IsConcurrencySafe: true,  // 可并行
        UserFacingName:    "Timer",
    })}
}

func (t *timerTool) Validate(input Input) string {
    secs, ok := input["seconds"].(float64)
    if !ok || secs <= 0 || secs > 300 {
        return "seconds must be 1-300"
    }
    return "" // 空字符串 = 通过
}

func (t *timerTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
    return Allowed("timer is safe") // 总是允许
}

func (t *timerTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
    secs := int(input["seconds"].(float64))

    select {
    case <-time.After(time.Duration(secs) * time.Second):
        return Result{Data: fmt.Sprintf("✓ Timer: %d seconds elapsed", secs)}, nil
    case <-ctx.Done():
        return Result{Data: "Timer cancelled", IsError: true}, ctx.Err()
    }
}
```

#### 第 2 步：注册工具

`cli/cove/registry.go`，在 `registerAllTools()` 中添加：
```go
r.Register(tool.NewTimerTool())  // ← 加这一行
```

#### 第 3 步：（可选）添加测试

`internal/tool/timer_test.go`:
```go
func TestTimerTool(t *testing.T) {
    tr := NewTimerTool()

    // 测试验证
    if msg := tr.Validate(map[string]any{"seconds": float64(0)}); msg == "" {
        t.Fatal("expected validation error for 0 seconds")
    }
    if msg := tr.Validate(map[string]any{"seconds": float64(500)}); msg == "" {
        t.Fatal("expected validation error for >300 seconds")
    }

    // 测试执行
    ctx := context.Background()
    res, err := tr.Call(ctx, map[string]any{"seconds": float64(1)}, Context{})
    if err != nil {
        t.Fatal(err)
    }
    if !strings.Contains(res.Data, "elapsed") {
        t.Fatalf("unexpected result: %s", res.Data)
    }
}
```

#### 第 4 步：构建并测试

```bash
go build ./cli/cove/
# 在新会话中测试: "请设置一个 3 秒计时器"
```

**关键点**：
- `Def.Name` 必须是唯一的，这是 AI 引用工具的名字
- `Def.Description` 是 AI 决定是否调用此工具的依据，要清晰描述
- `InputSchema` 必须是合法的 JSON Schema
- `Validate()` 返回非空 = 参数错误，工具不会被执行
- `CheckPermissions()` 决定权限策略
- `Call()` 中的 context 要正确传递，支持取消

---

### 7.2 添加一个新的斜杠命令

假设要添加 `/stats` 命令显示详细统计。

**步骤**：

#### 第 1 步：创建命令文件

`internal/command/stats.go`:
```go
package command

import (
    "fmt"
    "github.com/liuzhixin405/cove/internal/engine"
)

type statsCmd struct{}

func NewStatsCmd() Command {
    return &statsCmd{}
}

func (c *statsCmd) Name() string        { return "stats" }
func (c *statsCmd) Aliases() []string    { return []string{"stat"} }
func (c *statsCmd) Description() string  { return "Show detailed statistics" }
func (c *statsCmd) Help() string         { return "/stats — Show session statistics" }

func (c *statsCmd) Execute(eng *engine.Engine, args []string) (string, error) {
    tracker := eng.CostTracker()
    return fmt.Sprintf(
        "📊 Session Stats\n"+
        "  Model:   %s\n"+
        "  Iter:    %d\n"+
        "  Tokens:  %d in / %d out\n"+
        "  Cost:    $%.4f\n"+
        "  Budget:  $%.2f",
        eng.Session().Model,
        eng.IterCount(),
        // ... 从 tracker 获取更多数据
    ), nil
}
```

#### 第 2 步：注册命令

在 `cli/cove/registry.go` 的 `registerAllCommands()` 中添加：
```go
r.Register(command.NewStatsCmd())
```

#### 第 3 步：在 REPL 中处理

在 `cli/cove/main.go` 中通过命令注册表统一分发命令；交互端（TUI/headless）共享同一套命令实现。

---

### 7.3 添加一个新的 API Provider

假设要添加对 "Groq" API 的支持。

#### 第 1 步：创建 Provider 实现

`internal/api/groq.go`:
```go
package api

import (
    "context"
    "fmt"
)

type groqProvider struct {
    apiKey  string
    baseURL string
    client  *http.Client
}

func newGroqProvider(cfg ProviderConfig) *groqProvider {
    return &groqProvider{
        apiKey:  cfg.APIKey,
        baseURL: orDefault(cfg.BaseURL, "https://api.groq.com/openai/v1"),
        client:  &http.Client{Transport: defaultHTTPTransport()},
    }
}

func (p *groqProvider) Name() string        { return "groq" }
func (p *groqProvider) DisplayName() string  { return "Groq" }
func (p *groqProvider) Validate() error {
    if p.apiKey == "" {
        return fmt.Errorf("groq: api_key required")
    }
    return nil
}

// Groq 使用 OpenAI 兼容 API，所以 Chat 和 ChatStream 可以复用 OpenAI 的实现
// 只需设置正确的 BaseURL 即可
func (p *groqProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
    return openAICompatChat(ctx, p.apiKey, p.baseURL, p.client, req)
}

func (p *groqProvider) ChatStream(ctx context.Context, req ChatRequest, handler StreamHandler) (*ChatResponse, error) {
    return openAICompatChatStream(ctx, p.apiKey, p.baseURL, p.client, req, handler)
}
```

#### 第 2 步：注册到 Provider 工厂

在 `internal/api/provider.go` 的 `NewProvider()` 和 `DetectProvider()` 中加入 Groq 的分支。

#### 第 3 步：添加到 Provider 目录

在 `internal/api/provider_catalog.go` 中添加 Groq 的模型列表。

---

### 7.4 添加一个配置项

假设要添加 `max_iterations` 配置项。

#### 第 1 步：修改 Config 结构

`internal/config/config.go`:
```go
type Config struct {
    // ... 现有字段
    MaxIterations int `json:"max_iterations"` // 新增
}
```

#### 第 2 步：在 DefaultConfig 中设置默认值

```go
func DefaultConfig() *Config {
    return &Config{
        // ...
        MaxIterations: 200, // 默认 200
    }
}
```

#### 第 3 步：在 applyDefaults 中处理

```go
func applyDefaults(cfg *Config) {
    if cfg.MaxIterations <= 0 {
        cfg.MaxIterations = 200
    }
    // ...
}
```

#### 第 4 步：在 Engine 中使用

`internal/engine/engine.go`:
```go
// 原来: for iter := 0; iter < MaxIterations; iter++ {
// 改为:
maxIter := e.config.MaxIterations
if maxIter <= 0 {
    maxIter = MaxIterations
}
for iter := 0; iter < maxIter; iter++ {
```

---

### 7.5 添加一个新的引擎子系统

假设要添加一个 "操作审计" 系统，记录所有写操作。

#### 第 1 步：创建子系统包

`internal/audit/audit.go`:
```go
package audit

import (
    "sync"
    "time"
)

type Entry struct {
    Time     time.Time
    ToolName string
    FilePath string
    Action   string
}

type Auditor struct {
    mu      sync.Mutex
    entries []Entry
}

func New() *Auditor {
    return &Auditor{entries: make([]Entry, 0)}
}

func (a *Auditor) Record(toolName, filePath, action string) {
    a.mu.Lock()
    defer a.mu.Unlock()
    a.entries = append(a.entries, Entry{
        Time:     time.Now(),
        ToolName: toolName,
        FilePath: filePath,
        Action:   action,
    })
}

func (a *Auditor) All() []Entry {
    a.mu.Lock()
    defer a.mu.Unlock()
    result := make([]Entry, len(a.entries))
    copy(result, a.entries)
    return result
}
```

#### 第 2 步：注入到 Engine

`internal/engine/engine.go`:
```go
import "github.com/liuzhixin405/cove/internal/audit"

type Engine struct {
    // ... 现有字段
    auditor *audit.Auditor  // 新增
}

func New(config Config) (*Engine, error) {
    // ...
    e := &Engine{
        // ...
        auditor: audit.New(),  // 新增
    }
}
```

#### 第 3 步：在合适的钩子点调用

在 `executeTool()` 中，write/edit 成功后：
```go
if tc.Name == "write" || tc.Name == "edit" {
    e.auditor.Record(tc.Name,
        tc.Input["filePath"].(string),
        "modified")
}
```

#### 第 4 步：暴露给外部

添加 getter 方法或通过 Engine 的方法暴露。

---

## 8. 测试策略

### 测试结构

```
每个包都有自己的 _test.go 文件
go test ./... 运行所有测试
go test ./internal/engine/ -v -run TestLoopDetector  运行特定测试
```

### 测试类型

| 类型 | 示例 | 策略 |
|------|------|------|
| **单元测试** | `TestLoopDetector_ToolCallRepeat` | 隔离测试单个函数/类型 |
| **集成测试** | `engine_test.go` | Mock Provider，测试引擎完整流程 |
| **工具测试** | 各工具的 `_test.go` | 实际调用工具，验证输入输出 |

### 如何写一个好的测试

```go
func TestYourFeature(t *testing.T) {
    // 1. 准备（Arrange）
    obj := NewYourType()

    // 2. 执行（Act）
    result := obj.DoSomething("input")

    // 3. 断言（Assert）
    if result != "expected" {
        t.Fatalf("got %q, want %q", result, "expected")
    }

    // 4. 边界情况
    // 空输入、极限值、并发访问...
}
```

### 运行测试

```bash
go test ./...                      # 所有测试
go test -v ./...                   # 详细输出
go test -race ./...                # 竞态检测
go test -cover ./...               # 覆盖率
go test -run TestLoop ./...        # 运行匹配的测试
```

---

## 9. 调试技巧

### 启用调试日志

```bash
./cove --debug
# 或
COVE_DEBUG=1 ./cove
```

日志输出到 stderr，包含：
- 每个迭代的消息数、Token 数、工具数
- API 请求/响应的摘要
- 循环检测触发信息
- 工具执行结果

### 在代码中加日志

```go
import "github.com/liuzhixin405/cove/internal/log"

log.Debugf("variable = %v", value)
log.Warnf("something suspicious: %v", err)
```

### 查看原始 API 调用

在 engine.go 中，设置断点或添加临时代码：
```go
log.Debugf("req: %+v", req)
log.Debugf("resp: %+v", resp)
```

### 使用 delve 调试器

```bash
go install github.com/go-delve/delve/cmd/dlv@latest
dlv debug ./cli/cove/
```

### 常见调试场景

**"为什么 AI 不调用我的新工具？"**
→ 检查 `Def.Description` 是否清晰描述工具用途
→ 检查系统提示中是否包含了新工具

**"为什么工具调用失败？"**
→ 在 `executeTool()` 中添加日志
→ 检查 `Validate()` 返回值
→ 检查权限是否被拒绝

**"为什么 API 调用失败？"**
→ 检查 `~/.cove/config.json` 中的 API Key
→ 使用 `--debug` 查看完整错误
→ 运行 `/doctor` 命令检查环境

---

## 10. 常见问题 FAQ

**Q: 怎么修改系统提示？**
A: 编辑 `engine.go` 的 `SystemPrompt()` 方法，或在 `~/.cove/config.json` 中设置 `"system_prompt"` 字段。

**Q: 怎么添加对新的 AI 模型的支持？**
A: 如果模型兼容 OpenAI API，只需在 `config.json` 中设置 `provider.base_url`。如果完全不兼容，参考 7.3 节添加新 Provider。

**Q: 为什么移动端引擎是独立的包？**
A: gomobile 对依赖有限制。`mobile/mobileapi/` 自包含，不引用 `internal/` 包，确保 Android 编译成功。

**Q: 循环检测会不会误判？**
A: Layer 1 只检测完全相同的工具+参数组合（如反复写同一个文件）。批量操作中每次参数不同，不会触发。Layer 2 的阈值 10/50 足够高，正常重复不会触发。

**Q: 内存使用是否可控？**
A: 对话历史在 Token 超过 64000 或超过 16 条消息时自动压缩。会话文件在磁盘上可能较大（包含完整历史），但内存中总是压缩后的版本。

**Q: 怎么回滚 Agent 的误操作？**
A: write/edit 操作前会自动执行 `git commit`（checkpoint 系统）。使用 `git diff HEAD~1` 查看变更，`git reset --hard HEAD~1` 回滚。

**Q: 什么是 Plan Mode？**
A: 当任务复杂时，AI 可以调用 `plan_mode` 进入只读规划模式，分析代码后制定计划，再调用 `execute_plan` 执行。Worktree 提供了隔离的文件修改环境。

---

## 附录：快速参考卡

### 关键常量

| 常量 | 值 | 位置 |
|------|-----|------|
| `MaxIterations` | 200 | engine.go |
| `CompactTokenThreshold` | 64000 | engine.go |
| `maxParallelTools` | 8 | engine.go |
| `streamIdleTimeout` | 180s | api/provider.go |
| `fpWindow` (循环检测) | 10 | loopdetect.go |
| `fpThresh` (循环检测) | 5 | loopdetect.go |
| `outWindow` (输出检测) | 50 | loopdetect.go |
| `outThresh` (输出检测) | 10 | loopdetect.go |
| `maxBreaks` (循环中断) | 2 | loopdetect.go |
| `maxFails` (模型降级) | 3 | api/fallback.go |
| `cooldownDur` (Key 冷却) | 60s | api/keypool.go |

### 关键文件路径速查

| 想看什么 | 文件 |
|----------|------|
| 程序入口 + 参数 | `cli/cove/main.go` |
| 启动初始化 | `cli/cove/app_bootstrap.go` |
| 所有工具注册 | `cli/cove/registry.go` |
| Engine 核心 | `internal/engine/engine.go` |
| Tool 接口 | `internal/tool/tool.go` |
| API Provider 接口 | `internal/api/provider.go` |
| 配置加载 | `internal/config/config.go` |
| 循环检测 | `internal/engine/loopdetect.go` |
| TUI 界面 | `internal/tui/tui.go` |
| 会话存储 | `internal/session/store.go` |
| 权限管理 | `internal/permission/permission.go` |
| 移动端引擎 | `mobile/cove.go` |

---

> **最后**：这个项目的设计哲学是 **"最小依赖 + 接口抽象 + 关注分离"**。每个 `internal/` 子包只做一件事，Engine 负责把所有东西串起来。理解了 `RunMessageWithStream()` 的执行流程，你就理解了整个项目。
