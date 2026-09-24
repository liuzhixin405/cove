# Cove 使用手册

> **cove** — Go 语言 AI 编程助手，单文件二进制，零依赖，终端即用。

## 目录

- [快速开始](#快速开始)
- [安装与启动](#安装与启动)
- [提供商与模型配置](#提供商与模型配置)
- [REPL 命令参考](#repl-命令参考)
- [Agent 工具参考](#agent-工具参考)
- [权限模式](#权限模式)
- [配置系统](#配置系统)
- [技能系统](#技能系统)
- [MCP 协议支持](#mcp-协议支持)
- [插件系统](#插件系统)
- [后台任务与异步执行](#后台任务与异步执行)
- [计划执行器 (Plan Executor)](#计划执行器-plan-executor)
- [子智能体与团队协作](#子智能体与团队协作)
- [自学习系统](#自学习系统)
- [护栏与安全](#护栏与安全)
- [检查点与回退](#检查点与回退)
- [会话管理](#会话管理)
- [记忆系统](#记忆系统)
- [费用追踪](#费用追踪)
- [诊断系统](#诊断系统)
- [附件功能](#附件功能)
- [Git 集成](#git-集成)
- [CovePhone (Android)](#covephone-android)
- [高级技巧](#高级技巧)

---

## 快速开始

```bash
# 交互式 REPL
cove

# 单次查询
cove -p "创建一个贪吃蛇 HTML 游戏"

# 带附件查询
cove -p "分析这张图片" --image screenshot.png
cove -p "审查这个文件" --file config.json

# 查看版本
cove --version

# 系统诊断
cove --doctor

# 查看当前配置
cove --config

# 调试模式
cove -d
```

---

## 安装与启动

### 预编译二进制

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

### 从源码构建

需要 Go 1.25+：
```bash
git clone https://github.com/liuzhixin405/cove.git
cd cove
go build -o cove ./cli/cove
./cove --version
```

### 启动参数

| 参数 | 说明 |
|------|------|
| `-p, --print <prompt>` | 单次查询模式，非交互（见下方说明） |
| `--image <path>` | 附加图片（可多次指定） |
| `--file <path>` | 附加文件（可多次指定） |
| `-v, --version` | 显示版本信息 |
| `-d, --debug` | 调试模式 |
| `--doctor` | 系统诊断（git、ripgrep、供应商与 API key 是否设置） |
| `--config` | 查看配置（不显示 API key，只显示 `api_key_set`） |
| `-r, --resume <id>` | 按 ID 恢复会话后启动；可与 `-p` 连用继续该会话。会话属于其他项目目录时给出提示；ID 不存在时报错退出 |
| `--list-sessions [all]` | 列出当前项目的会话；加 `all` 列出所有项目的会话 |
| `--dump-system-prompt` | 打印系统提示词 |
| `--no-auto` | 禁用后台自学习功能 |
| `--no-tui` | 使用 headless 模式（按行读 stdin，答案写 stdout，提示写 stderr） |
| `--tui` | 即使 stdin/stdout 不是终端也强制使用交互界面 |
| `--profile <name>` | 使用指定 profile 启动 |
| `--record <dir>` | 录制本次会话的请求与响应到目录 |
| `--replay <dir>` | 用录制数据回放，不调用真实 API |
| `-h, --help` | 帮助信息 |

未知参数（例如拼错的 `--no-tiu`）和没有 `-p` 的多余文字会报错退出（退出码 2），不再被静默忽略。

**`-p` 单次查询说明：**

- 只有最终答案写到 stdout，提示、警告和错误都写到 stderr，因此 `cove -p "..." > out.txt` 只得到答案。
- 管道输入会附在提示后面一起发送：`cat app.log | cove -p "解释这段日志"`；只有管道输入时它就是提示本身。管道输入上限 8MB，超出部分截断并在 stderr 提示。
- 提示可以不加引号：`cove -p 解释 这段 代码` 等同于 `cove -p "解释 这段 代码"`。
- 退出码：`0` 成功，`1` 失败（API 错误、附件读取失败等），`2` 参数错误，`130` 被 Ctrl+C 中断。
- 没有人能回答授权询问：需要询问的工具调用（写文件、执行非只读命令）一律拒绝，不会卡住等待，模型会收到拒绝原因。要非交互地放行这些操作，用 `auto` 或 `bypass` 权限模式（配置 `permission_mode`，或用一个设置了该模式的 `--profile`）。注意 `/mode` 会写入配置文件，之后的交互会话也会沿用。

---

## 提供商与模型配置

### 支持的原生提供商

| 提供商 | 类型 | 环境变量 |
|--------|------|---------|
| **Anthropic** | 原生 | `ANTHROPIC_API_KEY` |
| **OpenAI** | 原生 | `OPENAI_API_KEY` |
| **DeepSeek** | 原生 | `DEEPSEEK_API_KEY` |

### 支持的兼容提供商 (OpenAI 兼容接口)

| 提供商 | 环境变量 |
|--------|---------|
| GLM (智谱) | `GLM_API_KEY` / `ZHIPU_API_KEY` |
| Kimi (月之暗面) | `KIMI_API_KEY` / `MOONSHOT_API_KEY` |
| Qwen (通义千问) | `QWEN_API_KEY` / `DASHSCOPE_API_KEY` |
| Doubao (豆包) | `DOUBAO_API_KEY` / `ARK_API_KEY` |
| OpenRouter | `OPENROUTER_API_KEY` |
| SiliconFlow (硅基流动) | `SILICONFLOW_API_KEY` |
| Groq | `GROQ_API_KEY` |
| Together | `TOGETHER_API_KEY` |
| Fireworks | `FIREWORKS_API_KEY` |
| xAI (Grok) | `XAI_API_KEY` |
| Mistral | `MISTRAL_API_KEY` |
| 自定义 | `LLM_API_KEY` + `LLM_BASE_URL` |

### 在 REPL 中切换

```
/provider deepseek        # 切换到 DeepSeek
/model deepseek-v4-pro    # 切换模型
/api-key sk-xxx           # 设置 API Key
/base-url https://...     # 设置自定义接口地址
```

### 模型切换策略（简单任务 vs 复杂任务）

系统支持在 `config.json` 中配置**两个模型**，根据任务复杂度**自动切换**，无需手动干预。

#### 配置方式

```json
{
  "model": "deepseek-v4-pro",        // ← 高级模型：用于复杂任务
  "model_fast": "deepseek-v4-flash", // ← 快速模型：用于简单任务
  "provider": {
    "name": "deepseek"
  }
}
```

| 配置字段 | 用途 | 推荐值 |
|---------|------|-------|
| `model` | **复杂任务模型**（高级、昂贵、能力强） | `deepseek-v4-pro`, `claude-sonnet-4-20250514`, `gpt-4o` |
| `model_fast` | **简单任务模型**（快速、便宜、够用） | `deepseek-flash`（旧名 `deepseek-v4-flash` 仍可用）, `gpt-4o-mini`, `claude-haiku-4-5` |

> 如果只配置 `model`，不配置 `model_fast`，则 `model_fast` 与 `model` 相同，即不做模型切换。
>
> 快速模型和高级模型使用同一套提示词和行为规则，不会因为走了快速模型就额外加限制。复杂任务进行中的简短跟进（如"继续"）会留在当前模型上；快速模型在一轮中连续失败时，该轮后续会自动改用高级模型。

#### 自动切换规则

系统分析用户每条消息的内容，**自动选择**合适的模型：

| 触发条件 | 使用的模型 | 示例场景 |
|---------|-----------|---------|
| 包含关键词：`refactor`、`重构`、`架构`、`设计`、`迁移`、`重写`、`debug`、`optimize`、`security audit` | ✅ `model`（高级模型） | "帮我重构这个模块"、"设计系统架构" |
| 消息长度 > 500 字符 | ✅ `model`（高级模型） | 长篇幅的需求描述 |
| 其他简单任务 | ✅ `model_fast`（快速模型） | "读取这个文件"、"搜索日志"、"简单问答" |
| 用户手动 `/model xxx` 指定 | ✅ 强制使用指定模型 | 临时需要切换模型 |

#### 代码实现

- **配置层**: `internal/config/config.go` — `Config.Model` + `Config.ModelFast`
- **路由层**: `internal/api/router.go` — `ModelRouter` 使用策略链自动决策
- **引擎层**: `internal/engine/engine.go` — 每次用户消息前调用 `Route()` 获取目标模型

#### 默认值

未指定模型时，系统按提供商自动填充默认值：

```json
{
  "model": "deepseek-v4-pro",        // DeepSeek 提供商的默认高级模型
  "model_fast": "deepseek-v4-flash"  // 默认快速模型
}
```

#### 视觉模型自动切换

当检测到图片附件时，系统自动切换到支持视觉的模型：
| 提供商 | 视觉模型 |
|--------|---------|
| DeepSeek | `deepseek-v4-flash` |
| OpenAI | `gpt-4o` |
| Anthropic | `claude-sonnet-4-20250514` |

### 配置优先级（从低到高）

1. **环境变量** — `LLM_API_KEY`, `LLM_BASE_URL`, 各提供商专用变量
2. **用户配置** — `~/.cove/config.json`
3. **项目配置** — 当前目录下的 `.cove.json`

---

## REPL 命令参考

### 供应商与模型

| 命令 | 说明 |
|------|------|
| `/model <名称>` | 切换 AI 模型 |
| `/provider <名称>` | 切换提供商（anthropic/deepseek/openai/openai-compatible/glm/kimi/qwen/doubao/openrouter/siliconflow/groq/together/fireworks/xai/mistral） |
| `/api-key <密钥>` | 保存 API 密钥 |
| `/base-url <地址>` | 设置自定义接口地址 |
| `/mode <模式>` | 设置权限模式 |
| `/budget <金额\|auto>` | 设置会话预算上限（$），`auto` 为一键智能调整 |
| `/cost` | 查看用量和费用 |
| `/ratelimit` | 查看 API 速率限制状态 |
| `/attach <文件...>` | 挂载图片或文件（支持 `list`/`remove`/`clear` 子命令） |
| `/config` | 查看完整配置 |

### 会话

| 命令 | 说明 |
|------|------|
| `/compact` | 压缩对话历史（当上下文接近 token 限制时） |
| `/undo` | 回退到上一个检查点 |
| `/checkpoints` | 列出所有检查点 |
| `/history` | 查看和恢复历史会话 |
| `/history detail <id>` | 查看某次会话详情 |
| `/resume [id]` | 恢复已保存的会话 |
| `/export` | 导出当前对话 |

### 记忆

| 命令 | 说明 |
|------|------|
| `/memory add <名称> <内容>` | 添加持久记忆 |
| `/memory list` | 列出所有记忆 |

### 后台任务

| 命令 | 说明 |
|------|------|
| `/tasks` | 查看运行中/排队任务（TUI）；headless 显示同步执行状态 |
| `/stop` 或 `/cancel` | 取消当前任务（TUI）；headless 无后台任务可取消 |

### Git 集成

| 命令 | 说明 |
|------|------|
| `/commit [msg]` | Git add + commit |
| `/review` | 审查工作区变更 |
| `/diff` | 显示 git diff |

### 系统

| 命令 | 说明 |
|------|------|
| `/mcp` | MCP 服务器管理 |
| `/plugin` | 插件管理 |
| `/skills` | 列出可用技能 |
| `/doctor` | 环境快速检查（Go/git/ripgrep） |
| `/diagnose [quick\|errors\|archive\|codes]` | 完整系统诊断与错误分析 |
| `/status` | 查看代理状态与会话信息 |
| `/stats` | 查看消息数与费用统计 |
| `/permissions` | 查看当前权限模式 |
| `/init` | 检测项目结构并初始化 CLAUDE.md |
| `/cd <路径>` | 切换工作目录 |
| `/context` | 查看当前上下文 |
| `/system <提示词>` | 设置自定义系统提示词 |
| `/dream` | 手动触发记忆整合 |
| `/help` | 显示帮助 |
| `/exit` | 退出 REPL |

---

## Agent 工具参考

Agent（AI）在对话中可以调用以下工具。每个工具有其权限要求（R=只读安全，W=可能需要确认）。

### 文件操作

| 工具 | 说明 | 权限 |
|------|------|------|
| `read` | 读取文件或目录内容 | R |
| `write` | 写入文件（创建或覆盖） | W |
| `edit` | 精确字符串替换编辑文件 | W |
| `glob` | 文件模式匹配查找 | R |
| `grep` | 正则表达式搜索文件内容 | R |

### 终端执行

| 工具 | 说明 | 权限 |
|------|------|------|
| `bash` | 执行 Bash 命令（macOS/Linux） | W |
| `powershell` | 执行 PowerShell 命令（Windows） | W |

### 网络与浏览器

| 工具 | 说明 | 权限 |
|------|------|------|
| `webfetch` | HTTP 获取网页内容并转为文本/Markdown | R |
| `websearch` | 通过 DuckDuckGo 搜索网络 | R |
| `browser` | 控制 headless Chrome 浏览器（渲染 JS 页面/截图） | R/W |

> **browser 工具说明**：
> - `navigate`：渲染 JS 页面并返回文本/Markdown/HTML
> - `screenshot`：截图保存为 PNG
> - 需要 Chrome 浏览器支持（`chromedp` 构建标签）
> - 无 Chrome 时自动降级为 HTTP fetch

### 计划与任务管理

| 工具 | 说明 | 权限 |
|------|------|------|
| `todowrite` | 创建和管理结构化任务列表 | W(本地) |
| `plan_mode` | 进入计划模式（只读操作） | R |
| `exit_plan_mode` | 退出计划模式 | W |
| `execute_plan` | 执行计划中的任务（通过子智能体） | W |
| `task` | 创建后台任务 | W |
| `task_list` | 列出所有后台任务 | R |
| `task_update` | 更新任务状态或输出 | W |
| `brief` | 生成会话或上下文摘要 | R |

### 智能体与团队

| 工具 | 说明 | 权限 |
|------|------|------|
| `agent` | 生成子智能体处理复杂多步骤任务 | W |
| `team_create` | 创建智能体团队并行工作 | W |
| `team_delete` | 删除智能体团队 | W |
| `send_message` | 向任务/团队发送消息 | W |

### 定时与协作

| 工具 | 说明 | 权限 |
|------|------|------|
| `cron` | 创建定时任务 | W |
| `sleep` | 暂停执行指定秒数（最多 300 秒） | R |
| `question` | 向用户提问（多选题） | R |
| `skill` | 执行预定义技能 | W |

### MCP 与插件

| 工具 | 说明 | 权限 |
|------|------|------|
| `mcp` | 调用 MCP 服务器工具 | 取决于 MCP 工具 |
| `mcp_resources` | 列出 MCP 资源 | R |
| `mcp_read_resource` | 读取 MCP 资源 | R |

### 技能

| 工具 | 说明 | 权限 |
|------|------|------|
| `skills_list` | 列出可用技能 | R |
| `skill_view` | 加载并查看技能内容 | R |

### Git 工作树

| 工具 | 说明 | 权限 |
|------|------|------|
| `worktree` | 创建 Git 工作树用于隔离开发 | W |
| `exit_worktree` | 退出工作树并清理 | W |

---

## 权限模式

四种权限模式，控制 Agent 在执行写入操作时是否需要确认：

| 模式 | 说明 |
|------|------|
| `default` | 智能分类：高风险操作（写文件、执行命令）弹出确认，读取操作自动允许 |
| `plan` | 计划模式：只能执行只读操作，写入请求被拒绝（之前选过的"本次会话总是允许"在此模式下不生效） |
| `auto` | 自动模式：所有操作自动批准（适合信任的场景） |
| `bypass` | 绕过模式：完全跳过权限检查 |

切换方式：
```
/mode auto
```

权限提示交互：
- `y` — 确认本次操作
- `n` — 拒绝
- `a` — 始终允许此类操作（当前会话，不持久化）。对 `bash`/`powershell` 只记住命令前缀，提示里会写明，例如 `[a] 本次会话总是允许 "go test" 开头的命令`：
  - 前缀取法：`git`、`go`、`npm`、`docker`、`kubectl`、`dotnet`、`cargo`、`pip` 等带子命令的工具取"程序 + 子命令"（`git status`、`go test`、`npm run`、`docker compose`），其他程序只取程序名（`ls`、`cat`）。复合命令会为其中每条命令各记一个前缀（`cd src && go test ./...` 记住 `cd` 和 `go test`）
  - 之后一行命令里的**每一条**命令（`&&`、`||`、`;`、`&`、管道、换行、子 shell 分隔的都算）都必须以已允许的前缀开头才免询问，按词比较：允许 `go test` 后，`go test ./... && rm -rf x`、`go test ./... | tee out.txt`（除非也允许了 `tee`）、`sudo go test`、`FOO=1 go test`、`go vet` 仍会询问
  - 含命令替换或进程替换（`$(...)`、反引号、`<(...)`、`>(...)`，引号内也算）、输出重定向到文件（`/dev/null`、`NUL`、`$null` 除外）、或引号内出现 `; & | < > ( )` 的命令行不会被前缀规则放行，照常询问
  - `sudo`、`env`、`xargs`、`bash -c`、`VAR=值` 开头，或 `git -C dir …` 这类取不到子命令的命令无法安全地记住前缀，提示中不提供 `[a]`；此时输入 `a` 只允许本次
  - 其他工具（`write`、`edit` 等）选 `a` 仍对整个工具生效
  - plan 模式下这些规则不起作用，非只读工具照样被拒绝

无论哪种模式，以下命令都会被直接拦截：递归删除根目录/家目录/系统目录或盘符根（如 `rm -rf /`、`rm -rf ~`、`Remove-Item -Recurse C:\`）、格式化磁盘或写裸设备（`mkfs`、`dd of=/dev/sda`、`format c:`）、关机重启、fork bomb、把下载或解码的内容直接交给解释器执行（`curl … | sh`、`irm … | iex`、`base64 -d | bash`）。删除项目内的文件或目录（如 `rm -rf build`）不会被拦截，按当前模式正常确认。

#### 命令运行在哪个 shell

`bash` 工具在 Windows 上优先使用 Git for Windows 的 bash（通过 `git.exe` 的位置找到），不会使用 `C:\Windows\System32\bash.exe`（WSL 启动器）；找不到时依次回退到 PowerShell、cmd。实际使用的 shell 会写进系统提示词告诉模型。命令以非交互方式运行：`git commit` 不带 `-m` 会直接失败而不是打开编辑器，git 不会在终端里等待输入密码。

#### 持久化权限规则

系统支持将权限决策持久化为规则（保存至 `~/.cove/policy.json`）：

| 规则类型 | 说明 |
|---------|------|
| `always_allow` | 始终允许匹配的工具调用 |
| `always_deny` | 始终拒绝匹配的工具调用 |
| `ask` | 每次询问用户 |

规则支持：
- **通配符匹配**：`"read"`、`"mcp_*_*"`、`"bash"` 等
- **参数条件**：仅在特定参数值时触发
- **过期时间**：可设置规则到期自动失效

---

## 配置系统

### 配置文件位置

- 用户配置：`~/.cove/config.json`
- 项目配置：项目根目录的 `.cove.json`

### config.json 示例

```json
{
  "model": "deepseek-v4-pro",
  "provider": {
    "name": "deepseek",
    "api_key": "sk-***",
    "base_url": ""
  },
  "permission_mode": "default",
  "max_budget_usd": 10,
  "thinking_tokens": 16000,
  "debug": false,
  "mcp_servers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/path/to/allowed"],
      "type": "stdio"
    },
    "atlassian": {
      "url": "https://mcp.atlassian.com/v1/mcp",
      "type": "sse"
    }
  }
}
```

### 配置字段说明

| 字段 | 类型 | 说明 |
|------|------|------|
| `model` | string | **复杂任务模型**，如 `deepseek-v4-pro`（高级）、`claude-sonnet-4-20250514`、`gpt-4o` |
| `model_fast` | string | **简单任务模型**（快速/便宜），如 `deepseek-flash`（旧名 `deepseek-v4-flash`）、`gpt-4o-mini`、`claude-haiku-4-5`。不配置则与 `model` 相同，即不做模型切换 |
| `provider.name` | string | 提供商名称（anthropic/deepseek/openai/glm/kimi/qwen/doubao/...）|
| `provider.api_key` | string | API 密钥。留空时读取提供商对应的环境变量（如 `DEEPSEEK_API_KEY`、`LLM_API_KEY`）；配置文件里有值时优先于环境变量 |
| `provider.base_url` | string | 自定义 API 端点（留空则自动匹配提供商默认地址） |
| `permission_mode` | string | 默认权限模式（default/plan/auto/bypass）。拼错的值按 `default` 运行，`/diagnose` 会提示 |
| `max_budget_usd` | number | 会话预算上限（美元）。每次模型调用前都会检查，超过时暂停；主循环、子智能体、记忆提取、上下文压缩等所有模型调用都计入 |
| `thinking` | string | 支持该能力的提供商（Anthropic）的思考模式：`adaptive`（由模型决定是否思考、思考多少，推理摘要会显示出来）或 `disabled`；留空则使用模型默认值 |
| `effort` | string | 推理深度：`low` / `medium` / `high` / `xhigh` / `max`；留空则使用模型默认值 |
| `done_verify_commands` | string[] | 模型声称完成后必须通过的校验命令（如 `go build ./...`），不通过则打回继续修改 |
| `done_verify_auto` | boolean | 未配置 `done_verify_commands` 时，按项目自动推断校验命令（`go.mod` → `go build ./...`，`Cargo.toml` → `cargo check`，本地安装了 TypeScript → `tsc --noEmit`），且只在本轮改过文件时执行。默认开启，设为 `false` 关闭 |
| `show_reasoning` | boolean | 是否把思考型模型（如 DeepSeek V4）的完整推理过程实时输出到对话区。默认关闭：推理进度只显示在状态行（"思考中… 已推理 N 字"） |
| `disabled_skills` | string[] | 不加载的技能名称列表（内置或自定义均可），如 `["spike", "plan"]` |
| `system_prompt` | string | 你自己的长期指令（如"提交信息用英文"），会**追加**到内置系统提示词末尾，不会替换内置规则 |
| `thinking_tokens` | number | 已不再生效：新版 Claude 模型不接受固定的思考 token 预算，请改用 `thinking` + `effort` |
| `debug` | boolean | 调试模式（开启详细日志） |
| `verbose` | boolean | 详细输出；可在 profile 中单独设置 |
| `mcp_servers` | object | MCP 服务器配置（支持 stdio/SSE/Streamable HTTP 传输） |
| `profiles` | object | 具名配置组，可覆盖 `model`、`model_fast`、`provider`、`permission_mode`、`max_budget_usd`、`thinking_tokens`、`debug`、`verbose`、`system_prompt`；用 `/profile save/switch` 管理 |
| `active_profile` | string | 启动时应用的 profile 名称（`--profile` 参数优先）；名称不存在时会给出警告并使用基础配置 |
| `memory_embedding` | object | 可选：`{"base_url", "api_key", "model"}`，为记忆检索启用远程语义向量；留空的字段沿用主 provider 的值。不配置则只用关键词检索，不产生额外请求 |
| `telemetry` | boolean | 目前不生效（当前版本没有读取该字段的代码） |

### 配置迁移

配置系统支持自动迁移，升级版本时无需手动修改 config.json。

---

## 技能系统

Cove 内置 **12 个技能**，编译在二进制里，随 cove 版本一起更新。

### 技能加载机制

- **按需加载**：所有技能只把名称和一句话描述列在系统提示词里，模型判断任务需要时再用 `skill` 工具加载全文。内置技能都是工作流（写计划、TDD、调试等），不会因为读写了某类文件就被自动塞进对话
- **按文件类型注入（仅自定义技能）**：自己写的技能如果在 `paths` 里声明了 glob 模式，操作匹配文件时会自动注入，每个会话只注入一次
- **禁用**：在配置里写 `"disabled_skills": ["spike", "plan"]`，就不会加载这些技能（内置或自定义都可以）

### 技能来源与优先级

同名技能以更"近"的定义为准：**项目 > 用户 > 插件 > 内置**。

| 来源 | 位置 |
|------|------|
| 内置 | 编译在 cove 二进制中 |
| 插件 | `~/.cove/plugins/<插件名>/skills/` |
| 用户 | `~/.claude/skills/`，然后 `~/.cove/skills/`（后者优先） |
| 项目 | 从 git 仓库根目录到当前目录，每一级的 `.claude/skills/` 和 `.cove/skills/`，越靠近当前目录越优先 |

- 不读取仓库根目录以上的目录；不在 git 仓库里时，只读当前目录
- 不扫描当前目录下的子目录：克隆或 vendor 进来的第三方代码即使带着 `.claude/skills`，也不会被加载
- `/skills list` 会标注每个技能的来源（`[内置]` `[插件]` `[用户]` `[项目]`）

### 修改内置技能

`/skills export <名称>` 会把内置技能复制到 `~/.cove/skills/<名称>/SKILL.md`，修改后重启即生效。注意：导出的副本会一直覆盖内置版本，**不再随 cove 升级更新**；删除这个文件即可恢复内置版本。

### 内置技能

| 技能 | 说明 |
|------|------|
| `commit-messages` | 编写 Conventional Commits 提交信息 |
| `executing-plans` | 按已有的实现计划分步执行，设置检查点 |
| `github-code-review` | 在 GitHub 上审查 PR：读 diff、行内评论、批准或要求修改 |
| `github-pr-workflow` | GitHub PR 生命周期：建分支、提交、开 PR、盯 CI、合并 |
| `karpathy-guidelines` | 编码准则：先想后写、简单优先、改动精准、目标驱动 |
| `performance-optimization` | 基于测量的性能优化：先 profile，修真正的瓶颈，再验证效果 |
| `plan` | 实现前先写可执行的计划：小任务、精确路径、完整代码 |
| `requesting-code-review` | 提交前自检：安全扫描、质量门禁、自动修复 |
| `safe-refactoring` | 不改变行为的重构：小步走，每步测试保持通过 |
| `spike` | 用一次性实验先验证想法再动手 |
| `systematic-debugging` | 四阶段根因调试：先弄清原因再修，不靠猜 |
| `test-driven-development` | TDD：红-绿-重构，先写测试再写代码 |

### 技能文件格式

```markdown
---
name: my-skill
description: 我的自定义技能
paths: "*.go,*.py"
---

# 技能内容

技能的具体指令和提示词...
```

---

## MCP 协议支持

Cove 支持 **Model Context Protocol (MCP)**，可连接外部工具服务器。

### 传输类型

- **stdio**：本地子进程通信
- **SSE (Server-Sent Events)**：远程 HTTP 流
- **Streamable HTTP**：新版 HTTP 传输协议

### 配置示例

```json
{
  "mcp_servers": {
    "filesystem": {
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/path/to/allowed"]
    },
    "atlassian": {
      "type": "sse",
      "url": "https://mcp.atlassian.com/v1/mcp"
    }
  }
}
```

### REPL 管理命令

```
/mcp list          # 列出已连接的 MCP 服务器
/mcp connect ...   # 连接新服务器
/mcp disconnect    # 断开连接
```

> **注意**：MCP 服务器仅在 Cove **启动时**从配置加载。修改 `mcp_servers` 后需重启 Cove。

---

## 插件系统

插件提供可扩展的命令和工具，内置插件市场。

### REPL 命令

```
/plugin list       # 列出已安装插件
/plugin install    # 安装插件
/plugin remove     # 卸载插件
```

---

## 后台任务与异步执行

### 异步任务架构

Cove 的 REPL 支持异步任务执行：

- **主 REPL** 循环中，用户输入被转换为任务放入队列
- 后台 goroutine 取出任务异步执行
- 用户可以在当前任务执行时继续输入（新输入排队）
- `/tasks` 查看运行中和排队任务（仅 TUI 维护队列）
- `/stop` 取消当前任务（仅 TUI；headless 为同步执行）

### 任务合并

当排队任务与新输入的内容相似或重叠时，系统会自动合并任务，避免重复执行。

### 失败重试

任务执行失败后，用户可以输入 `继续` 或 `continue` 来重试。

### 中断草稿保存

任务因异常中断时，输入会自动保存为中断草稿，重启后可以恢复。

---

## 计划执行器 (Plan Executor)

Plan Executor 是 Cove 的核心高级功能之一，支持**声明式多步骤任务执行**。

### 工作流程

1. Agent 使用 `todowrite` 工具创建结构化任务列表
2. 每个任务可声明依赖（`depends:task-1,task-2` 前缀）
3. Agent 调用 `execute_plan` 工具执行计划
4. Plan Executor 分析依赖关系，生成拓扑排序的执行级别
5. **同级别的独立任务并行执行**（最多 4 个并发子智能体）
6. 依赖任务失败时，下游任务自动标记为「跳过」
7. 失败的任务自动重试 1 次

### 依赖声明语法

在 `todowrite` 的 `content` 字段中使用 `depends:` 前缀：

```
depends:task-1,task-2 实现用户登录功能
```

### 并行执行

`execute_plan` 工具的 `parallel` 参数控制是否并行执行：
- `true`：同级别任务并发执行
- `false`：所有任务串行执行

---

## 子智能体与团队协作

### 子智能体 (Sub-Agent)

通过 `agent` 工具，Agent 可以生成子智能体处理独立的子任务：

- 支持的类型：`general`（通用）、`explore`（探索代码）、`plan`（计划）、`review`（审查）、`test`（测试）
- 子智能体拥有受限的工具集
- 子智能体沿用当前会话的权限模式，每次工具调用都经过与主会话相同的授权检查
- 最多 30 次迭代，超时 5 分钟
- 通过 `delegate.Delegator` 管理生命周期

### 团队 (Team)

通过 `team_create` 创建智能体团队并行工作：

- 定义团队成员及其各自的任务
- 通过 `send_message` 在任务/团队间发送消息
- 消息支持定向投递（任务 ID / 团队 ID / 广播）

### Cron 定时任务

通过 `cron` 工具创建定时任务，支持 cron 表达式调度。

---

## 自学习系统

Cove 内置多阶段自学习流水线，在对话过程中自动提取和整合知识。

### 提取 (Extract)

每轮对话后，系统自动分析对话内容，提取可持久化的事实存入记忆文件。

### 技能审查 (Background Review)

后台自动分析对话模式，创建新技能和记忆：
- 至少 4 条新消息触发一次审查
- 自动节流，避免过度消耗 API

### 记忆整合 (Dream)

周期性的 4 阶段记忆整合：
1. **Orient** — 收集所有记忆文件
2. **Gather** — 分析关联和冗余
3. **Consolidate** — 合并和重组
4. **Prune** — 清理过时记忆

可通过 `/dream` 手动触发。

### 记忆去重

新记忆与现有记忆相似度 >80% 时自动合并。

### 会话笔记 (Session Notes)

基于正则的决策和发现自动检测，保存到 `session_notes.md`。

---

## 护栏与安全

### 工具循环检测

系统内置**三层循环检测**机制，防止 AI 陷入无限循环浪费 Token：

| 层级 | 检测方式 | 窗口 | 阈值 | 说明 |
|------|---------|------|------|------|
| Layer 1a | 精确工具指纹匹配（工具名+参数哈希） | 14 轮 | 10 次 | 检测完全相同工具调用 |
| Layer 1b | 模糊工具名匹配 | 12 轮 | 10 次 | 检测同一工具不同参数 |
| Layer 2 | 输出内容哈希 | 40 轮 | 8 次 | 检测相同输出重复 |
| Layer 3 | 停滞检测（无文件修改） | 60 轮 | — | 检测空转无进展 |

**响应机制**：
- 前 5 次检测到循环 → 注入引导消息，要求 AI 换思路，自动清空检测窗口
- 超出 5 次 → 硬终止当前回合，返回错误
- 只读工具（`read`/`grep`/`glob`/`lsp`/`webfetch`/`browser`）豁免检测
- Flash 模型使用更敏感的阈值（8/12, 8/10, 8/30, 50）

### 幂等结果检测

检测重复的相同工具输出，防止无限循环。

### 并行执行保护

- 并行工具调用上限：8 个
- 每个工具 goroutine 有 `defer recover()` 防止 panic 崩溃
- 并行子智能体上限：4 个

### 路径安全

文件操作受路径安全检查，禁止访问系统敏感路径。

### URL 安全

`browser` 和 `webfetch` 工具会检查 URL 安全性，阻止访问私有/内部地址。

### 速率限制

内置 API 速率限制追踪（`/ratelimit` 查看状态）。

---

## 检查点与回退

### 自动检查点

在执行 `write` 或 `edit` 操作前（整批工具调用开始之前），系统自动创建 Git 快照作为检查点。快照存放在 `~/.cove/checkpoints/store`，每个项目有独立的历史，不会写入项目自己的 Git 仓库；项目的 `.gitignore` 会被遵守。内容与上一个检查点相同时不会重复创建。

### 手动操作

```
/checkpoints       # 列出当前项目最近的检查点
/undo              # 回退到上一个与当前状态不同的检查点；连续执行会一步步往前回退
/undo <commit>     # 回退到指定检查点（只接受当前项目的检查点）
```

回退会恢复检查点里的文件内容，并删除检查点之后新建的文件。回退前的状态会先自动备份，输出里会给出撤销这次回退的命令（`/undo <备份>`）。

`bash`/`powershell` 命令执行前也会创建检查点，所以 `rm`、`sed -i`、代码生成器造成的改动同样可以回退；明确只读的命令（`ls`、`cat`、`git status`、`git diff` 等）不创建。

---

## 会话管理

### 会话保存

会话自动保存到 `~/.cove/sessions/`，每个会话会记录启动 cove 时所在的项目目录（会话文件中的 `cwd` 字段）。

### 按项目区分的历史

`/history`、`/resume`、`cove --list-sessions` 以及输入“继续”时自动恢复最近任务，默认**只列出当前目录（项目）的会话**，避免把其他代码库的对话恢复到当前项目、让模型混淆文件路径。目录比较前会规范化为绝对路径；在 Windows 上不区分大小写（`D:\Proj` 与 `d:\proj` 视为同一项目）。

需要查看所有项目的会话时加上 `all`：

```
/history all               # 列出所有项目的会话（每行标注所属目录）
/history all <编号>        # 按 all 列表的编号恢复
/history all detail <编号> # 按 all 列表的编号查看详情
/resume all                # 列出所有项目的会话 ID
cove --list-sessions all   # 命令行列出所有项目的会话
```

执行 `/history all` 后直接输入编号，按的是 all 列表的编号；执行 `/history` 后则按当前项目列表的编号。

旧版本 cove 保存的会话没有记录目录，无法判断属于哪个项目，因此不出现在按项目的列表中（否则每个项目都会看到它们），但文件不会被删除，仍可在 `all` 视图中看到（标注为“旧版会话，未记录目录”）并恢复。列表中有被隐藏的会话时，会提示隐藏的数量和 `all` 用法。

按会话 ID 恢复（`/resume <id>`、`/history <id>`）不受项目限制；如果该会话属于其他目录，恢复时会给出提示，显示会话目录和当前目录。

### 会话恢复

```
/resume            # 列出当前项目可恢复的会话
/resume <id>       # 恢复指定会话（可跨项目，会提示）
/history           # 查看当前项目的历史会话
/history <编号|id> # 恢复历史会话并美化显式
```

#### 🛡️ 历史记录智能降噪
Cove 的会话管理具备低信噪比排除算法。当会自动为您保存的会话生成标题和摘要预览时，任何诸如单独的通用命令行启动指令（例如：`write`、`read file`、`grep`、`cd`、`git commit`等），都会被自动判定为“低信息噪音标题”而丢弃。系统会自动向后寻检并精确蒸馏首句真实的 User 提问语义作为替代标题，确保历史菜单一目了然。

#### 🎨 渐进式多轮色彩还原
在 REPL 或全键 TUI 交互页面输入并加载历史会话时，系统不再以一两行简单的“已恢复”来掩盖状态。控制台会**无感温和重绘最近的 4 轮交互历史**：
- **用户（User）指令**：以高饱和彩色、富有留白的层次显示。在系统内置微调时生成的 `[system:` 前缀底层通知则自动低亮隐藏。
- **助手（Assistant）**：完美梳理出的逻辑行文直接打印。
- **核心工具（Tool）调用链**：树状追溯所有调用工具（如 `edit`、`bash`）时传入的具体参数与经过剪裁压缩处理的返回结果（拒绝直接刷屏 200 行日志，精准截断）。

极大地唤醒了开发者的短期记忆，确保从上次中断的地方无缝衔接。

### 会话导出

```
/export            # 导出当前对话为 Markdown
```

### 上下文压缩

当对话 token 超过 64000 时，系统会提示压缩。也可手动触发：

```
/compact           # 压缩对话历史
```

---

## 记忆系统

### 持久记忆

记忆存储在 `~/.cove/memories/` 目录。

```
/memory add <名称> <内容>   # 添加记忆
/memory list         # 列出所有记忆
```

### 记忆特性

- BM25 检索增强（用于上下文注入）
- 嵌入向量存储
- 自动提取和去重
- 跨会话持久化

---

## 费用追踪

### 实时追踪

- 每次 API 调用的 token 使用和费用实时计算
- 不同模型的计费标准不同
- 达到预算上限时自动暂停并提示

### 查看费用

```
/cost               # 查看本次会话费用
```

显示信息包括：
- 本次会话 token 数和费用
- 近 24 小时总费用
- 近 7 天总费用
- 历史总会话数和总费用

### 预算管理

```
/budget 5           # 设置预算为 $5
/budget auto        # 智能调整预算（基于历史使用）
```

---

## 诊断系统

### 诊断码体系

30+ 诊断码，覆盖 6 大类：

| 类别 | 码段 | 范围 |
|------|------|------|
| E1xxx | 配置 | API Key、配置文件 |
| E2xxx | API | 认证、响应格式 |
| E3xxx | 网络 | 连接、超时 |
| E4xxx | 模型 | 不支持功能、速率限制 |
| E5xxx | Shell | 命令执行 |
| E6xxx | 数据目录 | 权限、空间 |

### 使用

```
/doctor             # 快速诊断
/doctor full        # 完整诊断（9 项检查）
/doctor quick       # 快速检查
/doctor codes       # 列出所有诊断码
```

所有修复都是 **HotFixable**，无需重启即可应用。

### 启动时诊断

`diagnostic.QuickCheck()` 在启动时自动运行，检测常见问题。

---

## 附件功能

### 在 REPL 中

```
/attach image.png           # 挂载图片
/attach config.json         # 挂载文件
/attach list                # 列出附件
/attach remove image.png    # 移除附件
/attach clear               # 清除所有附件
```

### 在 -p 模式

```bash
cove -p "分析这张图" --image screenshot.png
cove -p "审查配置" --file config.json
```

### 内联 @ 语法

在 REPL 或 `-p` 消息中使用 `@路径` 自动挂载：

```
解释这张图 @assets/screen.png
审查这个文件 @src/main.go
```

---

## Git 集成

### 提交

```
/commit "feat: add login feature"    # git add + commit
/commit                              # 自动生成 Conventional Commit 消息
```

### 审查

```
/review             # 审查未暂存的变更
/diff               # 显示 git diff
```

### 工作树

Agent 可通过 `worktree` 工具创建隔离的 Git 工作树，适合大规模重构。

---

## CovePhone (Android)

CovePhone 是 Cove 的 Android 手机伴侣应用。

### 要求

- Android 8.0 (API 26) 或更高
- 网络连接
- 支持的提供商 API Key（如 DeepSeek）

### 安装

1. 从 [Releases](https://github.com/liuzhixin405/cove/releases) 下载 APK
2. 允许安装未知来源应用
3. 打开 APK 完成安装

### 设置

1. 启动 CovePhone
2. 进入设置（齿轮图标）
3. 输入 API Key
4. 选择模型和提供商
5. 返回聊天界面开始使用

### 特性

- **原生 Go 引擎**：与桌面版使用相同的 Go 引擎，通过 `gomobile` 编译为 `cove-core.aar`
- **Thinking 显示**：AI 思考过程带平滑滚动显示
- **持久化设置**：API Key、模型、提供商自动保存
- **多轮对话**：会话内完整聊天历史

### 问题排查

如果应用返回重复响应：
1. 检查 API Key 是否正确配置
2. 确保网络连接正常
3. 尝试切换模型
4. 重启应用

### 技术支持

- GitHub Issues: https://github.com/liuzhixin405/cove/issues
- 邮箱: 164910441@qq.com

---

## TUI 主题系统

Cove 的 TUI 界面内置了一套**主题系统**，提供多种视觉风格供您选择。

### 内置主题

| 主题 | 风格描述 |
|------|----------|
| **Catppuccin** (Mocha) | 暖色调舒适主题，柔和的粉紫配色 |
| **Dracula** | 经典暗色高对比主题，紫色为主色调 |
| **Gruvbox** | 复古暖色主题，米黄背景+深色文字 |
| **OneDark** | Atom 编辑器经典主题，深蓝背景 |
| **TokyoNight** | 夜间蓝紫色调主题，深邃星空风格 |

### 切换主题

在 TUI 模式下，您可以通过以下方式切换主题：

- **快捷键**：按下 F5 或配置的快捷键循环切换主题
- 切换即时生效，无需重启

### 配置默认主题

在 config.json 中设置默认主题：

`json
{
  "tui": {
    "theme": "catppuccin"
  }
}
`

可选值：catppuccin、dracula、gruvbox、onedark、	okyonight


## 高级技巧

### 1. 利用计划模式

对于复杂变更，先输入要求进入计划模式 (`plan_mode`)，让 Agent 只读取和分析代码，生成完整计划后再执行。

### 2. 批量任务提高效率

利用 `todowrite` 一次性定义多个任务，然后 `execute_plan` 并行执行无依赖的任务。

### 3. 自定义技能

在 `~/.cove/skills/` 创建符合工作流的技能文件，让 Agent 在操作特定类型文件时自动加载。

### 4. 记忆管理

定期使用 `/memory list` 查看积累的记忆，用 `/dream` 触发整合去重。

### 5. 预算控制

设置合理的 `max_budget_usd`，或使用 `/budget auto` 让系统根据历史使用智能调整。

### 6. 附件而非复制

对于大型代码审查，使用 `--file` 参数或 `/attach` 命令而不是直接复制代码到对话中。

### 7. 浏览器工具

对于 JS 渲染的页面（如 Jira、Confluence），使用 `browser` 工具而非 `webfetch`；需要截图确认时使用 `screenshot` 动作。

### 8. Chrome Headless 模式

使用 `chromedp` 标签构建 Cove 可获得完整的 headless Chrome 支持：
```bash
go build -tags chromedp -o cove ./cli/cove
```

### 9. 🌲 符号级 AST 代码库大地图 (Repository Map)

在处理大中型、多级目录、高内聚耦合项目时，全量注入代码是不切实际且极其高昂的。Cove 拥有专有的、免 CGO 且零外部二进制依赖的并行 AST 定义地图库（[internal/repomap/](internal/repomap/)）：
- **智能提取**：自动、并行解析整个代码库中的所有声明、结构体、契约定义以及接收器（Receiver/Methods）。对周边 TS & Python 采用轻状态过滤。
- **PageRank 式关系排序**：基于类似 PageRank 的关系依赖和交叉引用出现频次，自动计算全局符号的热度，由高到低剪裁出 Top 50 的全局最相关大骨架地图树。
- **上下文自动剪枝**：无需开发者指示，AI 便可在极低的 Token 开销下拥有整个项目级的“巨型上帝视角（Bird's-Eye View）”。

### 10. ⚡ 本地文件改变热发现与 $mtime$ 动态防抖缓存

当外部（如 IDE、Git checkout 或编译器生成）或者 Cove 的辅助工具更改了工作区源代码时：
- Cove 将基于高并发 `RWMutex` 锁，自动追踪所有文件的绝对路径及最新修改时间戳（$mtime$）。
- 数据改变时感知线程无缝触发增量失效；在极短的时间窗口内对同一目标的连续改动做高效率增量防抖，无感通知大模型智能刷新或废弃过时提示词上下文。这极大节约了 API 资费。

### 11. 🔎 全网事实核查搜索引擎 Grounding 保护

Cove 具有双搜索引擎核查屏障：
- **搜索引擎联动检测**：当您设置了环境变量 `TAVILY_API_KEY` 或 `BRAVE_API_KEY` 时，Cove 的网络搜索功能将并联激活对应 API，结合高保真 RAG 结果清洗，杜绝任何 SEO 引流垃圾数据。
- **动态兜底**：若缺失高级 API 密钥，系统将自动使用轻量且经过编码重构的 DuckDuckGo 作为防灾兜底抓取，始终向 AI 输送最干净、真实的联网第三方库与 API 信息，全时抗击大模型知识幻觉和死板记忆。
