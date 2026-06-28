# Changelog

All notable changes to cove will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [6.3.1] - 2026-06-20

### Added

#### 🤖 智能模型路由 (ModelRouter)
- **双模型自动切换**：根据消息复杂度自动选择高级模型（复杂任务）或快速模型（简单任务）
  - 超过 500 字符的长消息 → 高级模型
  - 含重构/架构/设计等关键词 → 高级模型
  - 其他简单任务 → 快速模型（`model_fast`）
- **策略链架构**：override → complexity classifier → default，第一个匹配的策略胜出
- **实时模型切换**：`/model` 命令可覆盖路由，`SetModels()` 支持动态更新
- 新增 `model_fast` 配置字段，默认为空（兼容旧配置）

#### 🔁 模型故障转移 (ModelFallback)
- **Provider 链式故障转移**：主 Provider 失败时自动切换至备用 Provider
- **三级健康状态**：`ProviderOK`（健康）→ `ProviderDegraded`（冷却中）→ `ProviderUnavailable`（永久不可用）
- **智能冷却机制**：429限流/5xx超时 → 自动冷却 60s → 超时自动恢复
- **永久错误检测**：401/403 认证错误 → 标记不可用，需手动恢复
- **TryChat/TryChatStream**：统一的带故障转移的 API 调用接口
- **UI 状态指示**：`●` 健康 / `○` 降级 / `✕` 不可用，通过 `/status` 查看

#### 🔄 三层循环检测 (3-Layer LoopDetector)
- **Layer 1a（精确指纹匹配）**：14 轮滑动窗口内相同 tool-call 指纹出现 ≥10 次 → 循环
- **Layer 1b（模糊工具名匹配）**：12 轮窗口内相同工具名（不同参数）出现 ≥10 次 → 循环
- **Layer 2（输出内容哈希）**：40 轮窗口内相同输出哈希（前 512 字节）出现 ≥8 次 → 循环
- **Layer 3（停滞检测）**：连续 60 轮迭代无任何文件创建/修改 → 空转检测
- **只读工具豁免**：read/grep/glob/lsp/webfetch/browser/task_list 等只读工具不触发循环检测
- **自适应阈值**：Flash 模型（快速/便宜）使用更敏感的阈值（8/12, 8/10, 8/30, 50 轮停滞）
- **分级响应**：前 `maxBreaks=5` 次非致命循环注入引导消息，超出则硬终止
- **指纹重置**：注入引导后自动清空指纹窗口，让模型重新开始

#### 🗜️ 对话压缩 (ChatCompressor)
- **双层压缩架构**：
  - Layer 1：免费压缩 — 将旧工具结果截断为 1 行摘要（无 API 调用）
  - Layer 2：AI 压缩 — 调用模型生成中间对话的结构化摘要（含 Key Decisions/Files/Task Status/Errors）
- **智能触发**：token 使用量超过模型限制 50% 时自动触发，保留最近 30% 消息完好
- **安全分割**：始终以 assistant 消息锚定分割点，避免出现连续两条 user 消息（API 400 错误）
- **优雅降级**：AI 摘要失败时回退到简单截断

#### 🎭 工具输出掩码 (ToolOutputMasker)
- **Hybrid Backward Scanned FIFO 算法**：从末尾反向扫描，保护最近 ~50K tokens
- **磁盘卸载**：将旧工具输出写入 `~/.cove/tool-outputs/`，替换为路径占位符
- **豁免机制**：question/todowrite/plan_mode/exit_plan_mode 等交互式工具永不掩码
- **防止重复掩码**：已掩码消息跳过（`maskedPrefix` 检测）
- **最小裁剪阈值**：可裁剪内容 <30K tokens 时不触发掩码

#### 🗣️ 发言人预测 (NextSpeaker)
- **上下文感知的继续/停止决策**：检测终止信号（"task complete"、"任务完成"等）
- **最大迭代限制**：默认 50 轮，防止无限循环
- **会话结束检测**：扫描最近 3 条消息中的终止短语

#### 📋 策略引擎 (PolicyEngine)
- **声明式权限规则**：支持 `allow` / `deny` / `ask` 三种动作
- **Glob 模式匹配**：工具名支持 `bash`、`write:*`、`*` 等模式
- **参数级条件**：`param_match` 支持按参数值匹配（如限制 `command` 含 `rm`）
- **优先级排序**：高优先级规则先评估，命中即返回
- **持久化存储**：`FilePolicyStorage` 将规则保存至 JSON 文件，重启后保留
- **与 Permission 系统集成**：`Evaluate(toolName, params, mode)` 作为权限决策入口

#### 🌐 MCP Streamable HTTP 传输
- **2025 新规范支持**：单端点 POST+GET 流式传输
- **SSE 事件解析**：自动解析 `data: ` SSE 事件流
- **背压机制**：消息队列满时阻塞发送方而非丢弃（避免 JSON-RPC 挂起）
- **自动重连**：流断开时自动尝试重建

#### 📊 会话变更追踪 (SessionDiff)
- **轻量级快照**：`SessionView` 捕获消息列表 + token 数的即时快照
- **结构化 Diff**：`SessionDiff` 包含 Old/New Tokens、MsgCount、Added/Removed Tools、Added/Removed Files
- **工具/文件提取**：自动从 ToolCalls 参数中提取文件路径

#### 📈 本地遥测 (Telemetry)
- **事件录制**：结构化事件记录（类型、时间戳、数据）
- **本地存储**：保存至 `~/.cove/telemetry.json`
- **容量保护**：上限 1000 条，超出时裁剪后半
- **选择加入**：默认关闭，需 `Enable()` 启用

#### 🔒 安全检测 (Safety)
- **基础安全过滤器**：检测敏感命令（rm -rf、dd、mkfs 等）
- **路径安全校验**：防止路径遍历攻击（`../`）
- **内容审查**：检测 API key、密码等敏感信息泄露

#### 🗺️ 增强 RepoMap (Enhanced RepoMap)
- **PageRank 交叉引用评分**：基于引用的文件重要性排名
- **基于 mtime 的增量缓存**：仅重新扫描修改过的文件
- **RWMutex 并发安全**：读多写少的无锁缓存设计

### Changed
- **Engine 重构**：`Engine.provider` → `Engine.fallback *ModelFallback`，集成 `modelRouter` 和 `loopDetector`
- **Config 扩展**：新增 `ModelFast`、`LoopDetectionEnabled` 等配置字段
- **Permission 升级**：集成 `PolicyEngine` 作为权限决策的核心组件
- **Hooks 增强**：`PreToolUse`/`PostToolUse` 事件现在支持异步处理和结果修改
- **MCP 客户端**：支持 Streamable HTTP 传输协议（除原有 SSE 外）
- **Session Store**：集成 `SessionDiff` 变更追踪
- **State 扩展**：新增 `ModelFast` 字段

### Fixed
- **Compressor 双 user 消息 bug**：修复压缩后可能出现连续两条 user 消息导致 API 400 错误
- **LoopDetector 只读工具误报**：read/grep/glob 等只读工具不再触发循环检测
- **Provider 锁竞争**：API 调用期间释放锁，避免阻塞状态读取

### Documentation
- **README.md**: 新增双模型路由章节和配置示例
- **docs/USER_MANUAL.md**: 新增模型切换策略、配置字段表、MCP 传输协议说明
- **CHANGELOG.md**: 本次更新记录

## [5.1.2] - 2026-06-11

### Added
- **Plan Executor (execute_plan)**: Declarative multi-step task plans with dependency DAG, topological sort, and parallel sub-agent execution (up to 4 concurrent)
- **Multi-Agent Teams (team_create/team_delete)**: Create agent teams with member tasks and inter-agent message passing (send_message)
- **Cron Scheduler (cron)**: Schedule recurring background tasks via cron expressions
- **Background Task Queue**: Async REPL task execution with queue, merge detection, retry support, and interrupt drafts
- **Checkpoint System**: Auto Git snapshots before write/edit operations with `/undo` and `/checkpoints` commands
- **Headless Browser (browser)**: Chrome-based JS rendering and screenshot capture (chromedp build tag)
- **Web Search (websearch)**: DuckDuckGo-based live web search tool
- **Attachments (/attach)**: File and image attachment in REPL with `@path` inline syntax
- **Session History (/history)**: Browse and resume past conversation sessions with detail view
- **Rate Limit Tracking (/ratelimit)**: API rate limit status monitoring
- **Budget Auto-Mode (/budget auto)**: Smart budget suggestion based on historical usage
- **Git Worktree (worktree/exit_worktree)**: Isolated git worktree creation and cleanup
- **User Manual**: Comprehensive Chinese user manual covering all features, commands, and tools

### Changed
- **README**: Added Agent Tools table, Plan Executor, Teams, Guardrails, Checkpoints, and Diagnostic System
- **docs/**: Reorganized with index, fixed corrupted docs/README.md
- **REPL UI**: Async task execution prevents input blocking; streaming-safe cursor handling

### Fixed
- **Permission prompt hang**: Replaced fmt.Scanln with bufio.Scanner — empty input defaults to deny
- **Tool goroutine panic crash**: Added defer recover() in parallel tool execution goroutines
- **Engine loop after Ctrl+C**: Added ctx.Err() check at iteration start
- **WalkingIndicator race condition**: Synchronized Stop() with doneCh channel

## [4.0.5] - 2026-06-07

### Added
- **CovePhone (Android App)**: First mobile companion app for cove
  - Native Go AI engine (mobile/cove.go) compiled via gomobile into cove-core.aar
  - Full chat UI with thinking display, batch-rendered thinking blocks
  - Settings screen for API key, model, and provider configuration
  - Persistent configuration via SharedPreferences (ViewModel-backed)
  - DeepSeek API integration (real AI, not simulated responses)
- **Mobile Go Engine**: Lightweight standalone engine in mobile/cove.go for Android use
- **Release artifact**: covephone-v4.0.5.apk available in dist/v4.0.5/

### Changed
- **Documentation**: README updated with CovePhone sections (English & Chinese)

## [3.0.3] - 2026-06-06

### Fixed
- GitHub Actions release pipeline paths.

## [1.0.0] - 2026-06-03

### Added
- **Self-Learning Pipeline**: Automatic memory extraction, background skill/memory review, and periodic cross-session memory consolidation (dream)
- **Skill Tools for Agent**: `skills_list` and `skill_view` tools — agents can discover and load skills autonomously
- **Conditional Skill Loading**: Skills with `paths` frontmatter auto-inject when agent opens matching files
- **23 Built-in Skills as Disk Files**: User-editable SKILL.md files in `~/.cove/skills/`
- **Guardrail Time-Window Circuit Breaker**: 6+ consecutive failures in 30s triggers immediate block
- **Session Notes Decision/Discovery Tracking**: Regex-based auto-detection of user decisions
- **Memory Deduplication**: >80% similarity → merge instead of duplicate
- **Hooks Event System**: `PreToolUse`, `PostToolUse`, `SessionStart` events
- **Checkpoint Auto-Trigger**: Git snapshots before write/edit operations
- **`/dream` Command**: Manual memory consolidation trigger

### Changed
- **Skill System**: Hardcoded skill bundles → disk-based SKILL.md files
- **`backgroundReview` Throttled**: Minimum 4 new messages between reviews

### Removed
- **Buddy System**: Virtual pet companion removed
- **Suggest System**: Follow-up suggestion generation removed
- **Hardcoded Skill Bundles**: Replaced with disk files

## [1.0.2] - 2025-05-24

### Added
- Cross-platform release artifacts for Windows, Linux, macOS (amd64/arm64)
- Automated release build script with checksum generation

### Fixed
- Various stream processing fixes

## [1.0.1] - 2025-05-XX

### Added
- Initial public release
- Multi-provider AI backend (Anthropic, OpenAI, DeepSeek + 9 compat)
- Interactive REPL with slash commands
- Git integration (commit, review, diff)
- Permission system (default, plan, auto, bypass)
- Config management with env vars, user config, project-level overrides
- Token tracking and cost estimation
- Session save/resume, Memory persistence
- MCP (Model Context Protocol) server support
- Plugin system, Skills system
