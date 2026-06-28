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

需要 Go 1.24+：
```bash
git clone https://github.com/liuzhixin405/cove.git
cd cove
go build -o cove ./cli/cove
./cove --version
```

### 启动参数

| 参数 | 说明 |
|------|------|
| `-p, --print <prompt>` | 单次查询模式，非交互 |
| `--image <path>` | 附加图片（可多次指定） |
| `--file <path>` | 附加文件（可多次指定） |
| `-v, --version` | 显示版本信息 |
| `-d, --debug` | 调试模式 |
| `--doctor` | 系统诊断 |
| `--config` | 查看配置 |
| `-r, --resume <id>` | 恢复会话 |
| `--list-sessions` | 列出所有会话 |
| `--dump-system-prompt` | 打印系统提示词 |
| `--no-auto` | 禁用后台自学习功能 |
| `-h, --help` | 帮助信息 |

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
| `model_fast` | **简单任务模型**（快速、便宜、够用） | `deepseek-v4-flash`, `gpt-4o-mini`, `claude-haiku-3-5` |

> 如果只配置 `model`，不配置 `model_fast`，则默认 `model_fast = "deepseek-v4-flash"`。

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
| `/memory add <内容>` | 添加持久记忆 |
| `/memory list` | 列出所有记忆 |

### 后台任务

| 命令 | 说明 |
|------|------|
| `/tasks` | 查看运行中/排队的后台任务 |
| `/stop` 或 `/cancel` | 取消当前运行的任务 |

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
| `/doctor` | 系统诊断 |
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
| `plan` | 计划模式：只能执行只读操作，写入请求被拒绝 |
| `auto` | 自动模式：所有操作自动批准（适合信任的场景） |
| `bypass` | 绕过模式：完全跳过权限检查 |

切换方式：
```
/mode auto
```

权限提示交互：
- `y` — 确认本次操作
- `n` — 拒绝
- `a` — 始终允许此类操作（当前会话）

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
| `model_fast` | string | **简单任务模型**（快速/便宜），如 `deepseek-v4-flash`、`gpt-4o-mini`、`claude-haiku-3-5`。不配置则默认 `deepseek-v4-flash` |
| `provider.name` | string | 提供商名称（anthropic/deepseek/openai/glm/kimi/qwen/doubao/...）|
| `provider.api_key` | string | API 密钥（也支持通过环境变量设置） |
| `provider.base_url` | string | 自定义 API 端点（留空则自动匹配提供商默认地址） |
| `permission_mode` | string | 默认权限模式（default/plan/auto/bypass） |
| `max_budget_usd` | number | 会话预算上限（美元），超过时自动暂停 |
| `thinking_tokens` | number | thinking/推理 token 预算，最小值 1024（默认 16000） |
| `debug` | boolean | 调试模式（开启详细日志） |
| `mcp_servers` | object | MCP 服务器配置（支持 stdio/SSE/Streamable HTTP 传输） |

### 配置迁移

配置系统支持自动迁移，升级版本时无需手动修改 config.json。

---

## 技能系统

Cove 内置 **23+ 技能**，按文件类型自动加载。

### 技能加载机制

- **自动加载**：技能在 `paths` 中声明匹配的 glob 模式，当 Agent 操作匹配文件时自动注入
- **手动加载**：Agent 通过 `skill_view` 工具主动加载
- **用户可创建**：在 `~/.cove/skills/<name>/SKILL.md` 放入自定义技能

### 内置技能

| 技能 | 触发文件 | 说明 |
|------|---------|------|
| `commit-messages` | - | 编写 Conventional Commits 提交信息 |
| `executing-plans` | - | 执行实现计划 |
| `github-code-review` | - | GitHub PR 代码审查 |
| `github-pr-workflow` | `*.go,*.py,*.js,*.ts,*.rs,*.java` | GitHub PR 生命周期管理 |
| `performance-optimization` | - | 基于测量的性能优化 |
| `plan` | `*.go,*.py,*.js,*.ts,*.rs,*.java,*.rb` | 编写可执行的实现计划 |
| `requesting-code-review` | - | 请求代码审查 |
| `safe-refactoring` | - | 安全重构（小步、绿测试） |
| `spike` | `*.go,*.py,*.js,*.ts,*.rs,*.java` | 快速原型验证 |
| `systematic-debugging` | - | 系统化调试方法论 |
| `test-driven-development` | - | TDD 测试驱动开发 |
| `using-git-worktrees` | - | Git 工作树使用 |
| `writing-skills` | - | 编写自定义技能 |
| `commit` | - | 通用提交技能 |
| `dispatching-parallel-agents` | - | 并行智能体调度 |
| `brainstorming` | - | 创意构思 |
| `receiving-code-review` | - | 接收代码审查反馈 |
| `verification-before-completion` | - | 完成前验证 |
| `writing-plans` | - | 编写计划方法论 |

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
- `/tasks` 查看运行中和排队的任务
- `/stop` 取消当前任务

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
- 子智能体在 `auto` 权限模式下运行
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

在执行 `write` 或 `edit` 操作前，系统自动创建 Git 快照作为检查点。

### 手动操作

```
/checkpoints       # 列出所有检查点
/undo              # 回退到上一个检查点
```

---

## 会话管理

### 会话保存

会话自动保存到 `~/.cove/sessions/`。

### 会话恢复

```
/resume            # 列出可恢复的会话
/resume <id>       # 恢复指定会话
/history           # 查看历史会话
/history <id>      # 恢复历史会话并美化显式
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
/memory add <内容>   # 添加记忆
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
