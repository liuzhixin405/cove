## [Unreleased]

### Added
- **配置档案 (Profiles)**：新增 /profile 命令（list/switch/save/delete/show）和 --profile 启动参数，支持切换命名配置切片（model/provider/budget 等）。
- **会话录制与回放 (Record/Replay)**：新增 /record 命令（status/start/stop）、--record 和 --replay 启动参数。录制输出至 events.jsonl，回放时不调用真实 API。
- **Provider Adapter 基础层**：internal/api/adapter/ 包提供 Message、StreamAccumulator、MergeReasoning、ToolCallsFromResponse 等归一化工具（为后续 Provider 重构铺路）。
- **L1b 目录多样性检测**：循环检测 Layer 1b 新增目录指纹分析，同一工具模式在 ≥3 个不同目录中视为探索性工作，不再误判为循环（解决顺序 git 操作误触发问题）。

### Changed
- **压缩语义标签**：压缩注入的上下文提示添加 <compress summary="..."> 标签，给模型清晰信号。
- **Engine 重构**：抽取 stream_handler.go、	ool_runner.go、message_processor.go 分担 engine.go 职责；新增 uildMessageGraph() 为后续拓扑感知压缩做准备。
- **循环检测改进**：esetFingerprintHistory() 新增目录状态清理；hasToolCalls() 零值安全性提升。

### Fixed
- 循环检测 L1b 在频繁使用 shell 命令进行 git 操作时误触发自动中止。
- Config.Load() 在 profile 为空时的 nil map 赋值防护。

## [8.0.0] - 2026-07-18

### Added

#### TUI 主题系统 (Theme System)
- 全新主题系统 `internal/tui/theme/`，支持 5 套内置主题：
  - **Catppuccin** (Mocha) — 暖色调舒适主题
  - **Dracula** — 经典暗色高对比主题
  - **Gruvbox** — 复古暖色主题
  - **OneDark** — Atom 编辑器经典主题
  - **TokyoNight** — 夜间蓝紫色调主题
- 主题接口：`theme.go` 定义 `Theme` 接口，包含 20+ 语义化颜色令牌（text、accent、success、warning、error、border 等）
- 按需自由切换：TUI 内通过快捷键或命令切换主题

#### MCP 客户端重构 (Client Refactor)
- `internal/mcp/client.go` 全面重构：连接生命周期管理、超时控制、停止信号通道
- 改进的 goroutine 安全管理：`stopCh` 机制确保 `Receive()` 调用可被取消
- 更健壮的错误处理和重连逻辑

#### 循环检测增强 (LoopDetector)
- Layer 1b 模糊匹配增强：区分工具参数变化，减少误报
- Layer 2 输出循环检测修复：从仅日志升级为主动注入引导+硬中止
- Layer 3 停滞检测激活：`RecordIteration()` 和 `RecordFileActivity()` 现在被正确调用
- 新增 Layer 1b 工具输出进度检测：相同模式下产出 ≥4 种不同输出时重置循环计数

#### 引擎改进 (Engine)
- `OnToolStart` 回调：工具执行前通知，配合 `OnToolProgress` 提供完整生命周期回调
- 流式输出 ANSI 清理：TUI 显示工具输出时自动去除 ANSI 转义码
- CompressHistory 中文乱码修复

### Changed
- **TUI 视图层重构**：tui.go 重写 640+ 行，视图布局全面优化
- **styles.go**：重构为基于主题令牌的样式系统（253 行 → 256 行，+256/-57）
- 引擎中的循环检测配置同步更新（fpWindow = 10）

### Fixed
- 修复 compressHistory() 中文乱码（锟斤拷问题）
- 修复 MCP client 上下文取消未正确处理的问题
- 修复 TUI tool start 时缺少回调通知

### Chore
- `internal/tui/tui_smoke_test.go` 测试覆盖扩展（+267 行）

## [7.1.1] - 2026-07-04

### Chore

- **文档整理**: 将根目录大型开发指南移入 docs/guide/，清理临时文件 (session_notes.md, testout.txt)
- **文档归档**: 将未使用的优化文档移入 docs/archive/（.gitignore 忽略）
- **测试脚本迁移**: test_e2e_steer.py 移入 scripts/
- **.gitignore 优化**: 移除 blanket *.md 规则，改用精确忽略

## [7.0.6] - 2026-07-17

### Added
- **TUI F6 复制模式切换**：按下 F6 可在 TUI 中选择/复制文本（原生拖拽选择）
- Shift+Wheel 支持：按住 Shift 滚动鼠标滚轮时正常滚动视口

## [7.0.5] - 2026-07-04

### Fixed
- **中文乱码修复**：修复多文件中的锟斤拷（mojibake）显示问题
- 清理旧的回退兼容代码（repl_history.go）

### Changed
- Activity 提示从中文改为英文
- Review 输出改为纯英文

## [7.0.4] - 2026-07-03

### Changed
- **文档重组**：将 COVE_COMPLETE_GUIDE.md、DEVELOPMENT_DESIGN.md、DEVELOPMENT_GUIDE.md 移入 docs/guide/
- 清理根目录临时文件（session_notes.md, testout.txt）

### Fixed
- 修复 test_e2e_steer.py 路径问题，移至 scripts/

## [7.0.3] - 2026-07-01

### Changed
- **精简系统提示词**：精简 system_prompt 长度，将参考配置写入 config.example.json
- 减少不必要的上下文占用

## [7.0.2] - 2026-06-30

### Added
- **极致系统提示词优化**：任务完成标准、防编造规则、工具调用规范、双语支持
- 更严格的完成任务验证要求

## [7.0.1] - 2026-06-28

### Chore
- **文档归档**：将未使用的优化文档移入 docs/archive/
- 添加 .gitignore 规则忽略 archive 目录

## [7.0.0] - 2026-06-25

### Added
- **P0-P2 共 14 项完整优化实现**，基于设计文档 (DEVELOPMENT_DESIGN.md)
- 循环检测（3-Layer LoopDetector）
- 模型故障转移（ModelFallback）
- 对话压缩（ChatCompressor）
- 工具输出掩码（ToolOutputMasker）
- NextSpeaker、PolicyEngine、MCP Streamable HTTP
- SessionDiff、Telemetry、Safety、Enhanced RepoMap

### Changed
- Engine 重构：集成 ModelFallback、ModelRouter、LoopDetector 统一编排
- Config 扩展：新增 ModelFast、循环检测相关配置

### Documentation
- 详细设计文档 DEVELOPMENT_DESIGN.md（1208 行）
- 开发指南 DEVELOPMENT_GUIDE.md（1584 行）
- 完整开发手册 COVE_COMPLETE_GUIDE.md（1780 行）
- 配置模板 config.example.json
## [6.3.1] - 2026-06-20

### Added

#### 模型路由 (ModelRouter)
- 双模型自动切换：根据任务复杂度在主模型与 model_fast 之间切换。
- 策略链：override -> complexity classifier -> default，命中即生效。
- 支持运行期切换：/model 命令与 SetModels() 可动态更新。

#### 故障转移 (ModelFallback)
- Provider 链式故障转移：主 Provider 失败时自动切换备用 Provider。
- 三态健康状态：ProviderOK、ProviderDegraded、ProviderUnavailable。
- 冷却恢复机制：429/5xx/超时触发冷却，冷却后自动恢复探测。
- 永久错误检测：401/403 等认证错误标记为不可用，需人工修复。
- TryChat/TryChatStream：统一的非流式/流式故障转移调用入口。
- UI 状态指示：在 /status 中展示健康、降级、不可用状态。

#### 三层循环检测 (3-Layer LoopDetector)
- Layer 1a（精确指纹）：窗口内重复相同 tool-call 指纹触发检测。
- Layer 1b（模糊模式）：窗口内重复相同工具名模式触发检测。
- Layer 2（输出哈希）：窗口内重复输出哈希触发检测。
- Layer 3（停滞检测）：长时间无文件创建/修改时触发停滞提示。
- 只读工具豁免：read、grep、glob、lsp、webfetch、browser、task_list 等不触发循环报警。
- 分级响应：先注入引导，超过阈值后硬中止。

#### 对话压缩 (ChatCompressor)
- 双层压缩：先做轻量截断，再做结构化 AI 摘要。
- 智能触发：接近上下文预算上限时自动执行压缩。
- 安全切分：避免压缩后产生非法消息序列。
- 失败降级：摘要失败时自动回退到保守策略。

#### 工具输出掩码 (ToolOutputMasker)
- 反向扫描 FIFO，优先保留最近上下文。
- 支持磁盘卸载历史工具输出到 ~/.cove/tool-outputs/。
- 交互类工具默认豁免，不参与掩码。
- 避免重复掩码，降低无效噪声。

#### 其他新增
- NextSpeaker：上下文感知的继续/停止决策。
- PolicyEngine：声明式权限策略（allow/deny/ask）。
- MCP Streamable HTTP：支持新的流式传输协议。
- SessionDiff：会话变更追踪与摘要。
- Telemetry：本地遥测记录与容量保护。
- Safety：敏感命令、路径遍历、密钥泄漏检测。
- Enhanced RepoMap：基于引用与增量缓存的仓库映射增强。

### Changed
- Engine 重构：引入 ModelFallback、ModelRouter、LoopDetector 统一编排。
- Config 扩展：新增 ModelFast、循环检测相关配置项。
- Permission 升级：集成 PolicyEngine 作为权限决策入口。
- Hooks 增强：PreToolUse/PostToolUse 支持异步处理。
- MCP 客户端：支持 Streamable HTTP（兼容既有 SSE）。
- Session Store：集成 SessionDiff 追踪。
- State 扩展：新增 ModelFast 字段。

### Fixed
- 修复压缩后相邻 user 消息导致 API 400 的问题。
- 修复只读工具被误判为循环的误报问题。
- 修复 Provider 锁竞争导致的阻塞读问题。
### Documentation
- README.md：补充双模型路由章节与配置示例。
- docs/USER_MANUAL.md：补充模型切换策略、配置字段与 MCP 传输说明。
- CHANGELOG.md：记录本次更新。

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
- **Permission prompt hang**: Replaced fmt.Scanln with bufio.Scanner - empty input defaults to deny
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
- **Skill Tools for Agent**: `skills_list` and `skill_view` tools - agents can discover and load skills autonomously
- **Conditional Skill Loading**: Skills with `paths` frontmatter auto-inject when agent opens matching files
- **23 Built-in Skills as Disk Files**: User-editable SKILL.md files in `~/.cove/skills/`
- **Guardrail Time-Window Circuit Breaker**: 6+ consecutive failures in 30s triggers immediate block
- **Session Notes Decision/Discovery Tracking**: Regex-based auto-detection of user decisions
- **Memory Deduplication**: >80% similarity 鈫?merge instead of duplicate
- **Hooks Event System**: `PreToolUse`, `PostToolUse`, `SessionStart` events
- **Checkpoint Auto-Trigger**: Git snapshots before write/edit operations
- **`/dream` Command**: Manual memory consolidation trigger

### Changed
- **Skill System**: Hardcoded skill bundles 鈫?disk-based SKILL.md files
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


