<div align="center">

# 🤖 cove

**Go-powered AI Coding Assistant for the Terminal**

[![CI](https://github.com/liuzhixin405/cove/actions/workflows/ci.yml/badge.svg)](https://github.com/liuzhixin405/cove/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/liuzhixin405/cove?include_prereleases)](https://github.com/liuzhixin405/cove/releases)
[![Go Version](https://img.shields.io/github/go-mod/go-version/liuzhixin405/cove)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](CONTRIBUTING.md)

[English](#english) | [中文](#chinese)

</div>

---

<a name="english"></a>
## English

cove is a pure CLI AI programming assistant, implemented as a single-file Go binary. It runs in your terminal, supports multiple AI providers, and is designed for local development, scripting, and portable distribution.

### ✨ Features

- 🎯 **Single Binary** — Zero dependencies, just download and run
- 🌐 **Multi-Provider** — Anthropic, OpenAI, DeepSeek + 10+ OpenAI-compatible endpoints
- 🖥️ **Cross-Platform** — Windows, macOS (Intel & Apple Silicon), Linux
- 🎨 **Interactive REPL** — 25+ slash commands, async task queue, session management
- 🔧 **Agent Tools** — File ops, shell, grep, glob, web fetch/search, headless browser, PowerShell
- 🧠 **Self-Learning** — Auto memory extraction, skill creation, cross-session consolidation (Dream)
- 📋 **Plan Executor** — Declarative multi-step task plans with dependency DAG + parallel sub-agent execution
- 👥 **Multi-Agent & Teams** — Spawn sub-agents, create teams with message passing
- 📚 **Skill System** — 12 built-in skills loaded on demand + custom skills; project > user > plugin > built-in precedence
- 🎭 **Tiered Permission Modes** — `default` (read-only tools and read-only shell lines auto-allowed) · `auto` (+ build/test commands and in-project write/edit) · `bypass` (everything) · `plan` (read-only); per-project permanent allow rules via `[p]` → `~/.cove/policies.json`
- 🛡️ **Guardrails** — Tool loop detection, rapid-failure circuit breaker, idempotent result detection
- 💰 **Cost Tracking** — Real-time token counting, cost estimation, budget caps, rate-limit awareness
- 🔄 **Checkpoints** — Auto Git snapshots before write/edit, undo support
- 🩺 **Diagnostic System** — 30+ error codes, startup checks, hot-fixable without restart
- 📱 **CovePhone** — Android mobile app with native Go AI engine

### 📥 Installation

#### Download Pre-built Binary

Go to [Releases](https://github.com/liuzhixin405/cove/releases) and download the archive for your platform:

| Platform | File |
|----------|------|
| Windows (amd64) | `cove-v*-windows-amd64.zip` |
| macOS (Intel) | `cove-v*-darwin-amd64.tar.gz` |
| macOS (Apple Silicon) | `cove-v*-darwin-arm64.tar.gz` |
| Linux (amd64) | `cove-v*-linux-amd64.tar.gz` |
| Windows (arm64) | `cove-v*-windows-arm64.zip` |
| Linux (arm64) | `cove-v*-linux-arm64.tar.gz` |

Extract and run:

```bash
# macOS / Linux
tar -xzf cove-v*-linux-amd64.tar.gz
./cove

# Windows (PowerShell)
Expand-Archive cove-v*-windows-amd64.zip -DestinationPath .
.\cove.exe
```

Optionally, add to your `PATH` for global access.

#### Build from Source

```bash
git clone https://github.com/liuzhixin405/cove.git
cd cove
go build -o cove ./cli/cove
./cove --version
```

Requires the Go version declared in `go.mod`.

#### Local Release Build

```bash
python scripts/release_build.py v2.0.0
```

Artifacts are output to `dist/v2.0.0/`.

### 📱 CovePhone (Android)

CovePhone is an **Android companion app** for cove, bringing AI assistant capabilities to your mobile device.

- 🧠 **Native Go Engine** — Real AI engine (not mock) powered by `cove-core.aar`, a Go module compiled via `gomobile`
- 💬 **Full Chat UI** — Message list with thinking display, smooth scrolling, batch-rendered thinking blocks
- ⚙️ **Settings & Config** — API key, model selection, provider choice, persistent via SharedPreferences
- 🔌 **DeepSeek API** — Connects to DeepSeek (or other compatible providers) directly from your phone

**Download:** [covephone-v4.0.5.apk](dist/v4.0.5/covephone-v4.0.5.apk) (Android, ~47MB)

**Source:** [`mobile/`](mobile/) — Lightweight Go engine for mobile.

### 🚀 Quick Start

```bash
# Interactive REPL
cove

# One-shot query (cannot answer permission prompts: use bypass mode or
# persisted allow rules for fully unattended runs)
cove -p "Create a snake game in HTML"

# Cap the one-shot turn at 50 model calls (0 = no limit; default max_iterations = 200)
cove -p "Fix the failing tests" --max-turns 50

# View version
cove --version

# System diagnostics
cove --doctor
```

On first run, cove will guide you through API key setup. You can also set it directly:

```bash
# In REPL
/api-key sk-your-key-here

# Or via environment variable
export DEEPSEEK_API_KEY="sk-..."
export ANTHROPIC_API_KEY="sk-ant-..."
export OPENAI_API_KEY="sk-..."
```

### 🌍 Supported Providers

| Provider | Type | Environment Variable |
|----------|------|---------------------|
| **Anthropic** | Native | `ANTHROPIC_API_KEY` |
| **OpenAI** | Native | `OPENAI_API_KEY` |
| **DeepSeek** | Native | `DEEPSEEK_API_KEY` |
| **GLM (智谱)** | Compatible | `GLM_API_KEY` / `ZHIPU_API_KEY` |
| **Kimi (月之暗面)** | Compatible | `KIMI_API_KEY` / `MOONSHOT_API_KEY` |
| **Qwen (通义千问)** | Compatible | `QWEN_API_KEY` / `DASHSCOPE_API_KEY` |
| **Doubao (豆包)** | Compatible | `DOUBAO_API_KEY` / `ARK_API_KEY` |
| **OpenRouter** | Compatible | `OPENROUTER_API_KEY` |
| **SiliconFlow** | Compatible | `SILICONFLOW_API_KEY` |
| **Groq** | Compatible | `GROQ_API_KEY` |
| **Together** | Compatible | `TOGETHER_API_KEY` |
| **Fireworks** | Compatible | `FIREWORKS_API_KEY` |
| **xAI (Grok)** | Compatible | `XAI_API_KEY` |
| **Mistral** | Compatible | `MISTRAL_API_KEY` |
| **Custom** | Compatible | `LLM_API_KEY` + `LLM_BASE_URL` |

### ⌨️ REPL Commands

| Command | Description |
|---------|-------------|
| `/model <name>` | Switch AI model |
| `/provider <name>` | Switch provider |
| `/api-key <key>` | Set API key |
| `/base-url <url>` | Custom API endpoint |
| `/mode <mode>` | Permission mode: `default\|plan\|auto\|bypass` |
| `/budget <amount\|auto\|off\|save>` | Session-only budget cap ($): `<amount>`/`auto` change this session, `off` removes the cap, `save` writes the current cap to `config.json`; a one-time notice at 80% of the cap |
| `/cost` | View token usage & cost (session + 24h + 7d + all-time) |
| `/ratelimit` | View API rate limit status |
| `/config` | View full configuration |
| `/system <prompt>` | Custom system prompt |
| `/attach <file...>` | Attach images/files (`list`/`remove`/`clear` sub-commands) |
| `/cd <path>` | Change working directory; reloads the project's `policies.json` rules (warns if the file cannot be parsed) |
| `/context` | View current context |
| `/compact` | Compress conversation history now (forced summary, 4+ messages), printing tokens before → after |
| `/undo` | Revert to previous checkpoint |
| `/checkpoints` | List all checkpoints |
| `/history` | View and resume past sessions |
| `/resume [id]` | List or resume saved sessions |
| `/continue` | Resume the interrupted turn from where it stopped (after a limit stop, Ctrl+C or an API error) |
| `/export` | Export conversation to Markdown |
| `/memory [list\|add <name> <content>\|remove\|search\|stats]` | Manage persistent memory |
| `/dream [status\|run]` | Show memory consolidation (dream) trigger, last result and its token cost; `run` consolidates now in the background |
| `/hooks` | List hooks loaded from the user-level `hooks.json` (and entries that failed to load) |
| `/tasks` | View running/queued tasks in the interactive REPL; headless reports sync execution state |
| `/stop` / `/cancel` | Cancel current task in the interactive REPL; no background queue in headless |
| `/commit [msg]` | Git add + commit |
| `/review` | Review working changes |
| `/diff` | Show git diff |
| `/doctor` | Quick check (git/ripgrep/provider/API key) plus background learning and permission-rules file |
| `/diagnose [quick\|errors\|archive\|codes]` | Full diagnostics and runtime error analysis |
| `/status` | View agent state and session info |
| `/stats` | View message and cost stats |
| `/permissions` | View current permission mode |
| `/init [apply\|discard]` | Let the model draft CLAUDE.md from the repo and show it as a diff; `apply` writes it (never overwrites), `discard` drops it |
| `/mcp` | MCP server management |
| `/plugin` | Plugin management |
| `/skills` | Skill listing |
| `/help` | Show help |
| `/exit` | Exit REPL |

### ⚙️ Configuration

Configuration is read from three tiers (lowest to highest priority):

1. **Environment Variables** — `LLM_API_KEY`, `LLM_BASE_URL`, provider-specific keys
2. **User Config** — `~/.cove/config.json`
3. **Project Config** — `.cove.json` in project root

#### Model Routing (Dual-Model)

cove supports intelligent dual-model routing. When you send a message, the system evaluates its complexity:

- Each message is scored: a complexity keyword (`refactor`, `architecture`, `debug`, `重构`, `架构`, `调试` …) adds 0.45, length adds up to 0.20 (counted in **characters**, full at 1000; 2000+ always premium), named files up to 0.15, recent fast-model failures up to 0.10, a tight budget subtracts up to 0.10
- **Score ≥ 0.35** → **primary model** (`model`); otherwise → **fast model** (`model_fast`)
- When `model_fast` differs from `model`, each interactive turn starts with a dim `模型：<model>` line

If `model` is empty or `"auto"`, it auto-resolves to the provider's default premium model. If `model_fast` is empty or `"auto"`, it reuses the primary model.

Example `~/.cove/config.json`:

```json
{
  "model": "deepseek-v4-pro",
  "model_fast": "deepseek-v4-flash",
  "provider": {
    "name": "deepseek",
    "api_key": "sk-***"
  },
  "permission_mode": "default",
  "max_budget_usd": 10,
  "thinking_tokens": 16000,
  "debug": false,
  "verbose": false,
  "system_prompt": "",
  "mcp_servers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/path/to/allowed"],
      "type": "stdio"
    }
  }
}
```

Selected settings (full list in [docs/USER_MANUAL.md](docs/USER_MANUAL.md#配置字段说明)):

| Key | Default | Description |
|-----|---------|-------------|
| `max_budget_usd` | 10 | Session budget cap; `/budget` changes only the current session, `/budget save` persists it |
| `max_iterations` / `max_turn_minutes` | 200 / 60 | Per-turn soft limits (interactive asks `[c]/[s]`; `-p` hard limit) |
| `max_sessions` | 200 | Sessions kept **per project**; negative disables cleanup |
| `done_verify_commands` / `done_verify_auto` | — / `true` | Commands that must pass before a turn may end (auto: Go, Cargo, tsc, dotnet, npm build, Python compileall) |
| `done_check` | `auto` | One "is the request fully met?" self-check per turn after editing files; `auto` = fast-tier model or non-Anthropic provider; `on` / `off` |
| `experimental_tools` | `false` | Register `task*`, `team_*`, `send_message`, `brief`, `sleep` |
| `web_search` | — | `{"provider": "tavily\|brave\|duckduckgo", "api_key": "..."}`; env vars still work |

Background memory consolidation is configured in `~/.cove/dream.json` (`enabled`, `trigger`: `session_end` (default) \| `threshold`, `min_turns`: 2, `min_hours`, `min_sessions`).

### 🤖 Agent Tools

| Tool | Description | Read-Only |
|------|-------------|-----------|
| `read` | Read files or list directories | ✓ |
| `write` | Create or overwrite files | |
| `edit` | Exact string replacements in files | |
| `glob` | Find files by glob pattern | ✓ |
| `grep` | Regex search in files | ✓ |
| `repo_map` | Query the code outline (type/func/method signatures with line numbers) by path fragment or identifier; Go/Python/TS/JS, ≤12KB | ✓ |
| `bash` | Execute shell commands (Git Bash on Windows, falling back to PowerShell/cmd) | |
| `powershell` | Execute PowerShell commands (registered on Windows only) | |
| `webfetch` | HTTP fetch → Markdown | ✓ |
| `websearch` | Web search via Tavily / Brave (`web_search` config or env vars), DuckDuckGo fallback | ✓ |
| `browser` | Headless Chrome: navigate + screenshot (only in `-tags chromedp` builds) | ✓ |
| `todowrite` | Structured task list management | |
| `execute_plan` | Execute task plans with sub-agents (`max_agents` 1–8, default 4) | |
| `plan_mode` | Enter read-only plan mode | ✓ |
| `task` / `task_list` / `task_update` | Record and track to-do tasks (recorded only, not executed; `experimental_tools`) | |
| `agent` | Spawn sub-agent for complex tasks | |
| `team_create` / `team_delete` | Manage agent teams (`experimental_tools`) | |
| `send_message` | Message passing between tasks/teams (`experimental_tools`) | |
| `question` | Ask user multiple-choice questions (interactive REPL only) | ✓ |
| `skill` / `skill_view` / `skills_list` | Skill discovery & execution | ✓ |
| `mcp` / `mcp_resources` | MCP tool & resource access | varies |
| `sleep` | Pause execution (`experimental_tools`) | ✓ |
| `brief` | Generate session summary (`experimental_tools`) | ✓ |
| `worktree` / `exit_worktree` | Git worktree management | |

### 📂 Project Structure

```text
cove/
├── .github/            # GitHub CI/CD, templates
│   └── workflows/      # CI & Release workflows
├── cli/cove/           # CLI entry point
├── internal/           # Internal Go packages
├── mobile/             # Go engine for Android (CovePhone)
├── dist/               # Release artifacts
├── scripts/            # Build & test scripts
├── CHANGELOG.md        # Release history
├── CONTRIBUTING.md     # Contribution guide
├── LICENSE             # MIT License
└── README.md           # This file
```

### 🤝 Contributing

Contributions are welcome! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

### 📄 License

MIT — see [LICENSE](LICENSE) for details.

---

<a name="chinese"></a>
## 中文

cove 是一个纯 CLI 的 AI 编程助手，以单文件 Go 二进制形式发布。它运行在终端中，支持多种 AI 提供商，专为本地开发、脚本调用和便携分发而设计。

### ✨ 特性

- 🎯 **单文件二进制** — 零依赖，下载即用
- 🌐 **多提供商** — Anthropic、OpenAI、DeepSeek 及 10+ 个兼容接口
- 🖥️ **跨平台** — Windows、macOS (Intel & Apple Silicon)、Linux
- 🎨 **交互式 REPL** — 25+ 个斜杠命令，异步任务队列，会话管理
- 🔧 **灵活工具集** — 文件操作、shell/PowerShell、代码搜索、网页抓取/搜索、headless 浏览器
- 🧠 **自学习系统** — 自动记忆提取、技能创建、跨会话整合 (Dream)
- 📋 **计划执行器** — 声明式多步骤任务计划，依赖 DAG + 并行子智能体执行
- 👥 **多智能体与团队** — 子智能体生成、团队创建与消息传递
- 🔌 **MCP 支持** — Model Context Protocol 服务器集成 (stdio + SSE + Streamable HTTP)
- 🎭 **分层权限模式** — `default`（只读工具与只读命令自动放行）· `auto`（另放行构建/测试命令与项目内写入）· `bypass`（全部放行）· `plan`（只读）；`[p]` 永久允许按项目写入 `~/.cove/policies.json`
- 🛡️ **护栏保护** — 工具循环检测、快速失败断路器、幂等结果检测
- 🔄 **检查点** — 写入前自动 Git 快照，支持撤消回退
- 🩺 **诊断系统** — 30+ 错误码，启动检查，热修复无需重启
- 📦 **插件与技能** — 可扩展架构，内置 12 个技能（按需加载），支持自定义和覆盖
- 💰 **费用追踪** — 实时 token 计数、成本估算、预算上限、速率限制感知
- 📱 **CovePhone** — Android 手机 AI 助手应用

### 📥 安装

#### 下载预编译二进制

前往 [Releases](https://github.com/liuzhixin405/cove/releases) 下载对应平台的压缩包：

| 平台 | 文件 |
|------|------|
| Windows (amd64) | `cove-v*-windows-amd64.zip` |
| macOS (Intel) | `cove-v*-darwin-amd64.tar.gz` |
| macOS (Apple Silicon) | `cove-v*-darwin-arm64.tar.gz` |
| Linux (amd64) | `cove-v*-linux-amd64.tar.gz` |
| Windows (arm64) | `cove-v*-windows-arm64.zip` |
| Linux (arm64) | `cove-v*-linux-arm64.tar.gz` |

解压运行：

```bash
# macOS / Linux
tar -xzf cove-v*-linux-amd64.tar.gz
./cove

# Windows (PowerShell)
Expand-Archive cove-v*-windows-amd64.zip -DestinationPath .
.\cove.exe
```

建议将程序目录添加到 `PATH` 以便全局使用。

#### 从源码构建

```bash
git clone https://github.com/liuzhixin405/cove.git
cd cove
go build -o cove ./cli/cove
./cove --version
```

需要 `go.mod` 中声明的 Go 版本。

### 📱 CovePhone (Android)

CovePhone 是 cove 的 **Android 手机伴侣应用**，将 AI 助手能力带到你的手机上。

- 🧠 **原生 Go 引擎** — 基于 `cove-core.aar`（通过 `gomobile` 编译的 Go 模块）的真实 AI 引擎
- 💬 **完整聊天界面** — 消息列表带思考过程显示，平滑滚动，批量渲染的 thinking 块
- ⚙️ **设置与配置** — API key、模型选择、提供商选择，通过 SharedPreferences 持久化
- 🔌 **DeepSeek API** — 直接从手机连接 DeepSeek（或其他兼容提供商）

**下载:** [covephone-v4.0.5.apk](dist/v4.0.5/covephone-v4.0.5.apk) (Android, ~47MB)

**源码:** [`mobile/`](mobile/) — 移动端轻量 Go 引擎。

### 🚀 快速开始

```bash
# 交互式 REPL
cove

# 单次查询（无法回答授权询问：完全无人值守需 bypass 模式或已持久化的允许规则）
cove -p "创建一个贪吃蛇 HTML 游戏"

# 单次查询最多调用模型 50 次（0 不限制；默认取配置 max_iterations = 200）
cove -p "修复失败的测试" --max-turns 50

# 查看版本
cove --version

# 系统诊断
cove --doctor
```

首次运行时，cove 会引导你配置 API key。也可以直接设置：

```bash
# 在 REPL 中
/api-key sk-your-key-here

# 或通过环境变量
export DEEPSEEK_API_KEY="sk-..."
```

### 📄 许可证

MIT — 详见 [LICENSE](LICENSE)。

### ⭐ Star History

如果这个项目对你有帮助，请给我们一个 Star ⭐！

[![Star History Chart](https://api.star-history.com/svg?repos=liuzhixin405/cove&type=Date)](https://star-history.com/#liuzhixin405/cove&Date)

