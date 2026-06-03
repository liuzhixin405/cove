<div align="center">

# 🤖 agentgo

**Go-powered AI Coding Assistant for the Terminal**

[![CI](https://github.com/agentgo/agentgo/actions/workflows/ci.yml/badge.svg)](https://github.com/agentgo/agentgo/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/agentgo/agentgo?include_prereleases)](https://github.com/agentgo/agentgo/releases)
[![Go Version](https://img.shields.io/github/go-mod/go-version/agentgo/agentgo?file=agent%2Fgo.mod)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](CONTRIBUTING.md)

[English](#english) | [中文](#chinese)

</div>

---

> **📖 [完整用户手册 / Full User Manual](docs/USER_MANUAL.md)** | **[文档中心 / Docs](docs/)**

---

<a name="english"></a>
## English

agentgo is a pure CLI AI programming assistant, implemented as a single-file Go binary. It runs in your terminal, supports multiple AI providers, and is designed for local development, scripting, and portable distribution.

### ✨ Features

- 🎯 **Single Binary** — Zero dependencies, just download and run
- 🌐 **Multi-Provider** — Anthropic, OpenAI, DeepSeek + 10+ OpenAI-compatible endpoints
- 🖥️ **Cross-Platform** — Windows, macOS (Intel & Apple Silicon), Linux
- 🎨 **Interactive REPL** — Focused terminal UI with a small core command set
- 🔧 **Core Tools** — File operations, shell commands, Git integration, web fetch
- 🧠 **Memory & Sessions** — Persistent memory, session save/resume/export
- 🎭 **Permission Modes** — default | plan | auto | bypass
- 💰 **Cost Tracking** — Real-time token counting and cost estimation

### 📥 Installation

#### Download Pre-built Binary

Go to [Releases](https://github.com/agentgo/agentgo/releases) and download the archive for your platform:

| Platform | File |
|----------|------|
| Windows (amd64) | `agentgo-v*-windows-amd64.zip` |
| macOS (Intel) | `agentgo-v*-darwin-amd64.tar.gz` |
| macOS (Apple Silicon) | `agentgo-v*-darwin-arm64.tar.gz` |
| Linux (amd64) | `agentgo-v*-linux-amd64.tar.gz` |

Extract and run:

```bash
# macOS / Linux
tar -xzf agentgo-v*-linux-amd64.tar.gz
./agentgo

# Windows (PowerShell)
Expand-Archive agentgo-v*-windows-amd64.zip -DestinationPath .
.\agentgo.exe
```

Optionally, add to your `PATH` for global access.

#### Build from Source

```bash
git clone https://github.com/agentgo/agentgo.git
cd agentgo/agent
go build -o agentgo ./cmd/agentgo
./agentgo --version
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
agentgo

# One-shot query
agentgo -p "Create a snake game in HTML"

# View version
agentgo --version

# System diagnostics
agentgo --doctor
```

On first run, agentgo will guide you through API key setup. You can also set it directly:

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
| `/diagnose [mode]` | Structured diagnostics: `full\|quick\|codes` |
| `/export` | Export conversation |
| `/help` | Show help |
| `/exit` | Exit REPL |

### ⚙️ Configuration

Configuration is read from three tiers (lowest to highest priority):

1. **Environment Variables** — `LLM_API_KEY`, `LLM_BASE_URL`, provider-specific keys
2. **User Config** — `~/.agentgo/config.json`
3. **Project Config** — `.agentgo.json` in project root

Example `~/.agentgo/config.json`:

```json
{
  "model": "deepseek-v4-pro",
  "provider": {
    "name": "deepseek",
    "api_key": "sk-***"
  },
  "permission_mode": "default",
  "max_budget_usd": 10,
  "thinking_tokens": 16000
}
```

### 📂 Project Structure

```text
agentgo/
├── agent/              # Go source code (single module)
│   ├── cmd/agentgo/    # CLI entry point
│   └── internal/       # 25+ internal packages
├── docs/               # User manual & documentation
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

agentgo 是一个纯 CLI 的 AI 编程助手，以单文件 Go 二进制形式发布。它运行在终端中，支持多种 AI 提供商，专为本地开发、脚本调用和便携分发而设计。

### ✨ 特性

- 🎯 **单文件二进制** — 零依赖，下载即用
- 🌐 **多提供商** — Anthropic、OpenAI、DeepSeek 及 10+ 个兼容接口
- 🖥️ **跨平台** — Windows、macOS (Intel & Apple Silicon)、Linux
- 🎨 **交互式 REPL** — 聚焦核心命令的稳定终端界面
- 🔧 **核心工具集** — 文件操作、shell 命令、Git 集成、网页抓取
- 🧠 **记忆与会话** — 持久化记忆、会话保存/恢复/导出
- 🎭 **权限模式** — default | plan | auto | bypass
- 💰 **费用追踪** — 实时 token 计数和成本估算

### 📥 安装

#### 下载预编译二进制

前往 [Releases](https://github.com/agentgo/agentgo/releases) 下载对应平台的压缩包：

| 平台 | 文件 |
|------|------|
| Windows (amd64) | `agentgo-v*-windows-amd64.zip` |
| macOS (Intel) | `agentgo-v*-darwin-amd64.tar.gz` |
| macOS (Apple Silicon) | `agentgo-v*-darwin-arm64.tar.gz` |
| Linux (amd64) | `agentgo-v*-linux-amd64.tar.gz` |

解压运行：

```bash
# macOS / Linux
tar -xzf agentgo-v*-linux-amd64.tar.gz
./agentgo

# Windows (PowerShell)
Expand-Archive agentgo-v*-windows-amd64.zip -DestinationPath .
.\agentgo.exe
```

建议将程序目录添加到 `PATH` 以便全局使用。

#### 从源码构建

```bash
git clone https://github.com/agentgo/agentgo.git
cd agentgo/agent
go build -o agentgo ./cmd/agentgo
./agentgo --version
```

需要 Go 1.24+。

### 🚀 快速开始

```bash
# 交互式 REPL
agentgo

# 单次查询
agentgo -p "创建一个贪吃蛇 HTML 游戏"

# 查看版本
agentgo --version

# 系统诊断
agentgo --doctor
```

首次运行时，agentgo 会引导你配置 API key。也可以直接设置：

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

[![Star History Chart](https://api.star-history.com/svg?repos=agentgo/agentgo&type=Date)](https://star-history.com/#agentgo/agentgo&Date)
