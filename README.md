<div align="center">

# 🤖 cove

**Go-powered AI Coding Assistant for the Terminal**

[![CI](https://github.com/liuzhixin405/cove/actions/workflows/ci.yml/badge.svg)](https://github.com/liuzhixin405/cove/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/cove/cove?include_prereleases)](https://github.com/liuzhixin405/cove/releases)
[![Go Version](https://img.shields.io/github/go-mod/go-version/cove/cove)](https://go.dev/)
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
- 🎨 **Interactive REPL** — Focused terminal UI with a small core command set
- 🔧 **Agent Tools** — File ops, shell, grep, glob, web fetch, skill_view, skills_list
- 🧠 **Self-Learning** — Auto memory extraction, skill creation, cross-session consolidation
- 📚 **Skill System** — 23 built-in skills + custom skills, conditional auto-loading by file type
- 🎭 **Permission Modes** — default | plan | auto | bypass with intelligent classifier
- 🛡️ **Guardrails** — Tool loop detection, rapid-failure circuit breaker, idempotent result detection
- 💰 **Cost Tracking** — Real-time token counting and cost estimation

### 📥 Installation

#### Download Pre-built Binary

Go to [Releases](https://github.com/liuzhixin405/cove/releases) and download the archive for your platform:

| Platform | File |
|----------|------|
| Windows (amd64) | `cove-v*-windows-amd64.zip` |
| macOS (Intel) | `cove-v*-darwin-amd64.tar.gz` |
| macOS (Apple Silicon) | `cove-v*-darwin-arm64.tar.gz` |
| Linux (amd64) | `cove-v*-linux-amd64.tar.gz` |

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
cd cove/agent
go build -o cove ./cmd/cove
./cove --version
```

Requires Go 1.24+.

#### Local Release Build

```bash
python scripts/release_build.py v2.0.0
```

Artifacts are output to `dist/v2.0.0/`.

### 🚀 Quick Start

```bash
# Interactive REPL
cove

# One-shot query
cove -p "Create a snake game in HTML"

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
| `/budget <amount>` | Set session budget cap ($) |
| `/cost` | View token usage & cost |
| `/config` | View full configuration |
| `/system <prompt>` | Custom system prompt |
| `/cd <path>` | Change working directory |
| `/context` | View current context |
| `/compact` | Compress conversation history |
| `/resume [id]` | List or resume saved sessions |
| `/memory [add\|list]` | Manage persistent memory |
| `/commit [msg]` | Git add + commit |
| `/review` | Review working changes |
| `/diff` | Show git diff |
| `/doctor` | System diagnostics |
| `/mcp` | MCP server management |
| `/plugin` | Plugin management |
| `/skills` | Skill listing |
| `/export` | Export conversation |
| `/help` | Show help |
| `/exit` | Exit REPL |

### ⚙️ Configuration

Configuration is read from three tiers (lowest to highest priority):

1. **Environment Variables** — `LLM_API_KEY`, `LLM_BASE_URL`, provider-specific keys
2. **User Config** — `~/.cove/config.json`
3. **Project Config** — `.cove.json` in project root

Example `~/.cove/config.json`:

```json
{
  "model": "deepseek-v4-pro",
  "provider": {
    "name": "deepseek",
    "api_key": "sk-***"
  },
  "permission_mode": "default",
  "max_budget_usd": 10,
  "thinking_tokens": 16000,
  "mcp_servers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/path/to/allowed"],
      "type": "stdio"
    }
  }
}
```

### 📂 Project Structure

```text
cove/
├── .github/            # GitHub CI/CD, templates
│   └── workflows/      # CI & Release workflows
├── agent/              # Go source code
│   ├── cmd/cove/    # Entry point
│   └── internal/       # 25+ internal packages
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
- 🎨 **交互式 REPL** — 丰富的终端界面，25+ 个斜杠命令
- 🔧 **灵活工具集** — 文件操作、shell 命令、Git 集成、网页抓取
- 🧠 **记忆与会话** — 持久化记忆、会话保存/恢复/导出
- 🔌 **MCP 支持** — Model Context Protocol 服务器集成 (stdio + SSE)
- 🎭 **权限模式** — default | plan | auto | bypass
- 📦 **插件与技能** — 可扩展架构，内置市场
- 💰 **费用追踪** — 实时 token 计数和成本估算
- 🐾 **伙伴系统** — 带情绪引擎的交互式伙伴角色

### 📥 安装

#### 下载预编译二进制

前往 [Releases](https://github.com/liuzhixin405/cove/releases) 下载对应平台的压缩包：

| 平台 | 文件 |
|------|------|
| Windows (amd64) | `cove-v*-windows-amd64.zip` |
| macOS (Intel) | `cove-v*-darwin-amd64.tar.gz` |
| macOS (Apple Silicon) | `cove-v*-darwin-arm64.tar.gz` |
| Linux (amd64) | `cove-v*-linux-amd64.tar.gz` |

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
cd cove/agent
go build -o cove ./cmd/cove
./cove --version
```

需要 Go 1.24+。

### 🚀 快速开始

```bash
# 交互式 REPL
cove

# 单次查询
cove -p "创建一个贪吃蛇 HTML 游戏"

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

[![Star History Chart](https://api.star-history.com/svg?repos=cove/cove&type=Date)](https://star-history.com/#cove/cove&Date)
