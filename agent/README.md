# agentgo

纯 CLI 版 AI 编程助手。当前实现为单文件 Go 二进制，面向本地终端、脚本化调用与便携分发。

## 安装

### 方式 1：一键安装

```powershell
cd <repo-root>
.\\install.bat
```

默认安装到：`%USERPROFILE%\.agentgo\agentgo.exe`

### 方式 2：手动构建并安装

```powershell
cd demo
go build -o $env:USERPROFILE\.agentgo\agentgo.exe ./cmd/agentgo
```

然后把 `%USERPROFILE%\.agentgo` 加入 PATH。

## 绿色版 / Portable

绿色版支持，且推荐用于分发：

1. 运行 release 构建脚本生成压缩包
2. 解压到任意目录
3. 直接运行 `agentgo.exe`
4. 如需全局命令，再手动把解压目录加入 PATH

Windows 绿色版不依赖安装程序；解压即用。
如果 Windows 终端里中文显示仍乱码，优先使用 Windows Terminal，并确认终端使用 UTF-8；在 cmd 中可先执行 `chcp 65001` 后再启动 `agentgo.exe`。
新版本会在启动时尽量自动把 Windows 控制台输入/输出代码页切到 UTF-8；如果启动后仍看到控制台代码页提醒，请先在同一个窗口执行 `chcp 65001`，再重新启动。

### 本地生成 release

```bash
python scripts/release_build.py v1.0.1
```

产物输出到：
- `dist/v1.0.1/agentgo-v1.0.1-windows-amd64.zip`
- `dist/v1.0.1/agentgo-v1.0.1-linux-amd64.tar.gz`
- `dist/v1.0.1/agentgo-v1.0.1-darwin-amd64.tar.gz`
- `dist/v1.0.1/agentgo-v1.0.1-darwin-arm64.tar.gz`
- `dist/v1.0.1/checksums.txt`

### 校验发布包

```bash
certutil -hashfile dist\v1.0.1\agentgo-v1.0.1-windows-amd64.zip SHA256
```

或对照 `checksums.txt` 进行校验。

## 使用

```powershell
agentgo                     # 交互 REPL
agentgo -p "创建贪吃蛇 HTML"  # 单次执行
agentgo --version           # 查看版本
agentgo --doctor            # 诊断
agentgo --config            # 查看配置
agentgo --debug             # 调试模式
```

首次进入 REPL 时，若未配置 API key，程序会按“当前厂商 -> 对应环境变量 -> 常见兼容厂商示例”的顺序给出提示。

最简单的配置方式：
- 在当前 REPL 直接执行 `/api-key <key>`
- 或在启动前设置环境变量，然后用 `/config` 确认 `api_key_set: true`

内置 provider：
- 原生：`anthropic`、`deepseek`、`openai`
- 兼容 OpenAI：`openai-compatible`、`glm`、`kimi`、`qwen`、`doubao`、`openrouter`、`siliconflow`、`groq`、`together`、`fireworks`、`xai`、`mistral`

常见环境变量示例：
- `ANTHROPIC_API_KEY`
- `DEEPSEEK_API_KEY`
- `OPENAI_API_KEY`
- `GLM_API_KEY` / `ZHIPU_API_KEY`
- `KIMI_API_KEY` / `MOONSHOT_API_KEY`
- `QWEN_API_KEY` / `DASHSCOPE_API_KEY`
- `DOUBAO_API_KEY` / `ARK_API_KEY`
- `OPENROUTER_API_KEY`
- `SILICONFLOW_API_KEY`
- 通用回退：`LLM_API_KEY`
- 自定义兼容接口地址：`LLM_BASE_URL`

## 常用 REPL 命令

| 命令 | 说明 |
| --- | --- |
| `/model <name>` | 切换模型 |
| `/provider <name>` | 切换 Provider |
| `/api-key <key>` | 保存 API Key 到配置；无 key 时 REPL 也会提示此命令 |
| `/base-url <url>` | 自定义 API 端点 |
| `/mode <mode>` | 权限模式：`default\|plan\|auto\|bypass` |
| `/budget <amount>` | 设置会话预算上限 |
| `/cost` | 查看 token 用量和费用 |
| `/config` | 查看完整配置 |
| `/system <prompt>` | 自定义系统提示词 |
| `/cd <path>` | 切换工作目录 |
| `/context` | 查看当前上下文 |
| `/compact` | 压缩对话历史 |
| `/resume [id]` | 列出/恢复已保存会话 |
| `/memory [add|list]` | 管理持久内存 |
| `/commit [msg]` | git add + commit |
| `/review` | 查看工作区变化 |
| `/diff` | git diff |
| `/doctor` | 系统诊断 |
| `/mcp` | MCP 服务器管理 |
| `/plugin` | 插件管理 |
| `/skills` | 技能列表 |
| `/export` | 导出对话 |
| `/help` | 帮助 |
| `/exit` | 退出 |

## 配置

### 环境变量

```powershell
$env:LLM_API_KEY = "sk-xxx"
$env:LLM_BASE_URL = "https://..."
$env:ANTHROPIC_API_KEY = "sk-ant-..."
$env:DEEPSEEK_API_KEY = "sk-..."
$env:OPENAI_API_KEY = "sk-..."
```

### 用户级配置文件

路径：`~/.agentgo/config.json`

```json
{
  "model": "deepseek-chat",
  "provider": {
    "name": "deepseek",
    "api_key": "sk-xxx",
    "base_url": ""
  },
  "permission_mode": "default",
  "max_budget_usd": 10,
  "thinking_tokens": 16000,
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

### 项目级覆盖

项目根目录可创建 `.agentgo.json`，覆盖 model、permission_mode、budget、system_prompt、mcp_servers。

## 目录结构

```text
demo/
├── cmd/
│   └── agentgo/main.go
├── internal/
│   ├── agent/
│   ├── api/
│   ├── command/
│   ├── config/
│   ├── context/
│   ├── cost/
│   ├── engine/
│   ├── hooks/
│   ├── mcp/
│   ├── memory/
│   ├── permission/
│   ├── plugin/
│   ├── session/
│   ├── skills/
│   ├── state/
│   └── tool/
├── docs/
├── go.mod
└── README.md
```

## 构建与验证

```bash
cd demo
go test ./...
go build -o agentgo.exe ./cmd/agentgo/
./agentgo.exe --version
printf '/plugin list\n/exit\n' | ./agentgo.exe
```

## 发布

GitHub Actions 发布入口：`.github/workflows/release.yml`

支持：
- 推送 tag：`v*`
- 手动触发 workflow_dispatch

## 说明

本目录是一个独立的 Go 实现与交付链路实验区，当前品牌、配置目录、构建入口、发布产物均统一为 `agentgo`。
