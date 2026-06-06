# cove 完整使用手册

> **版本**: 2.1.0
> **最后更�?*: 2026-05-30

---

## 目录

1. [快速开始](#1-快速开�?
2. [安装与配置](#2-安装与配�?
3. [启动参数](#3-启动参数)
4. [REPL 交互](#4-repl-交互)
5. [所有命令详解](#5-所有命令详�?
6. [LLM 工具清单](#6-llm-工具清单)
7. [权限系统](#7-权限系统)
8. [配置文件](#8-配置文件)
9. [环境变量](#9-环境变量)
10. [供应商支持](#10-供应商支�?
11. [Multi-Key 密钥池](#11-multi-key-密钥�?
12. [插件系统�?Marketplace](#12-插件系统�?marketplace)
13. [MCP 集成](#13-mcp-集成)
14. [Skills 技能系统](#14-skills-技能系�?
15. [检查点与回退](#15-检查点与回退)
16. [上下文压缩](#16-上下文压�?
17. [护栏防循环](#17-护栏防循�?
18. [子代理系统](#18-子代理系�?
19. [记忆系统](#19-记忆系统)
21. [Dream 记忆整理](#21-dream-记忆整理)
22. [渐进式引导](#22-渐进式引�?
23. [提示缓存与速率限制](#23-提示缓存与速率限制)
24. [文件与目录结构](#24-文件与目录结�?
25. [高级用法与技巧](#25-高级用法与技�?
26. [诊断系统](#26-诊断系统)
27. [稳定性保障](#27-稳定性保�?

---

## 1. 快速开�?
```bash
# 设置 API Key（二选一�?export ANTHROPIC_API_KEY="sk-ant-..."
# �?cove --api-key "sk-ant-..."

# 启动交互�?REPL
cove

# 单次查询模式（不进入 REPL�?cove -p "�?Go 写一�?HTTP server"

# 恢复上次会话
cove --resume <session-id>
```

首次启动时会自动检测项目类型，建议运行 `/init` 生成项目指南文件（`CLAUDE.md`），�?AI 更好地理解你的项目�?
---

## 2. 安装与配�?
### 2.1 安装

```bash
# 从源码构�?cd agent/
go build -o cove ./cli/cove/
```

### 2.2 首次配置

启动后使用以下命令快速配置：

```
/provider anthropic          # 选择供应�?/api-key sk-ant-xxx...       # 设置 API Key
/model claude-sonnet-4-20250514  # 选择模型
/budget 5                    # 设置预算上限 ($5)
```

配置会自动保存到 `~/.cove/config.json`�?
---

## 3. 启动参数

| 参数 | 简�?| 说明 |
|------|------|------|
| `--version` | `-v` | 显示版本�?|
| `--help` | `-h` | 显示帮助信息 |
| `--debug` | `-d` | 启用调试日志（显�?API 请求/响应详情�?|
| `--print <prompt>` | `-p <prompt>` | 单次查询模式，执行完毕后退�?|
| `--resume <id>` | `-r <id>` | 恢复指定的已保存会话 |
| `--list-sessions` | �?| 列出所有已保存的历史会�?|
| `--doctor` | �?| 运行系统诊断（检�?git、ripgrep、配置） |
| `--config` | �?| �?JSON 格式输出当前配置 |
| `--dump-system-prompt` | �?| 输出完整系统提示词后退�?|
| `--no-auto` | �?| 禁用后台自动提取（extract/suggest�?|

### 示例

```bash
# 调试模式，查看所�?API 交互
cove -d

# 单次执行，适合脚本集成
cove -p "列出当前目录所�?Go 文件" 2>/dev/null

# 诊断环境问题
cove --doctor
```

---

## 4. REPL 交互

### 4.1 按键绑定

| 按键 | 功能 |
|------|------|
| `Enter` | 提交输入 |
| `Ctrl+C` | 中断当前操作（工具执�?API 调用�?|
| `Ctrl+D` | 退出程序（输入缓冲为空时） |
| `←` / `→` | 移动光标 |
| `↑` / `↓` | 浏览历史输入 |
| `Home` | 光标移到行首 |
| `End` | 光标移到行尾 |
| `Delete` | 删除光标后字�?|
| `Backspace` | 删除光标前字�?|
| `Tab` | 自动补全 |

### 4.2 Tab 补全

- **命令补全**: 输入 `/com` �?Tab �?提示 `/commit`、`/compact`
- **参数补全**: 输入 `/mode ` �?Tab �?提示 `default`、`plan`、`auto`、`bypass`
- **供应商补�?*: 输入 `/provider ` �?Tab �?列出所有支持的供应�?- **技能补�?*: 输入 `/skill ` �?Tab �?列出可用技能名

### 4.3 输入模式

- **普通输�?*: 直接输入自然语言问题或指�?- **命令模式**: �?`/` 开头触发命令（实时显示补全建议�?- **多行输入**: 暂不支持，每�?Enter 即提�?
### 4.4 输出格式

- **Markdown 渲染**: 代码块自动语法高�?- **工具调用进度**: 显示工具名称和简要参�?- **成本追踪**: 每次交互后显�?token 用量
- **Walking Indicator**: 多轮迭代时显�?思考中..."动画

---

## 5. 所有命令详�?
核心运行时包含：Agent 工具链、自我学习管道（extract/backgroundReview/dream）、技能系统、护栏、检查点、会话笔记�?
### 5.1 配置类命�?
| 命令 | 说明 | 用法 |
|------|------|------|
| `/model <名称>` | 切换 LLM 模型 | `/model gpt-4o` |
| `/provider <名称>` | 切换 AI 供应�?| `/provider deepseek` |
| `/api-key <密钥>` | 设置 API 密钥 | `/api-key sk-xxx` |
| `/base-url <URL>` | 设置自定�?API 地址 | `/base-url https://my-proxy.com/v1` |
| `/mode <模式>` | 切换权限模式 | `/mode auto` |
| `/budget <金额>` | 设置每会话预算上�?USD) | `/budget 10` |
| `/config` | 查看完整配置 | `/config` |
| `/config <�? <�?` | 修改配置字段 | `/config thinking_tokens 32000` |

### 5.2 会话管理

| 命令 | 说明 | 用法 |
|------|------|------|
| `/cost` | 查看费用（当�?24h+7d+总计�?| `/cost` |
| `/compact` | 手动压缩对话历史 | `/compact` |
| `/history` | 列出历史会话 | `/history` |
| `/history <编号>` | 恢复指定历史会话 | `/history 3` |
| `/resume [id]` | 恢复已保存的会话 | `/resume abc123` |
| `/export [文件]` | 导出对话�?markdown | `/export chat.md` |
| `/context` | 查看会话上下文信�?| `/context` |
| `/status` | 查看代理状�?| `/status` |
| `/stats` | 查看会话统计 | `/stats` |

### 5.3 项目操作

| 命令 | 说明 | 用法 |
|------|------|------|
| `/commit [消息]` | 暂存所有更改并 git 提交 | `/commit fix: 修复空指针` |
| `/diff` | 显示当前 git diff | `/diff` |
| `/review` | AI 审查工作区更�?| `/review` |
| `/cd <路径>` | 切换工作目录 | `/cd src/` |
| `/init` | 初始化项目（生成 CLAUDE.md�?| `/init` |

### 5.4 记忆

| 命令 | 说明 | 用法 |
|------|------|------|
| `/memory` | 列出所有记忆条�?| `/memory` |
| `/memory add <内容>` | 手动添加记忆 | `/memory add 用户偏好 tab=4` |
| `/memory remove <名称>` | 删除记忆条目 | `/memory remove auto` |

### 5.5 其他

| 命令 | 说明 | 用法 |
|------|------|------|
| `/help` | 显示帮助 | `/help` |
| `/exit` | 退出程�?| `/exit` |
| `/debug` | 切换调试模式 | `/debug` |
| `/system [提示词]` | 查看/设置自定义系统提示词 | `/system 你是一�?Go 专家` |
| `/permissions` | 查看当前权限设置 | `/permissions` |
| `/doctor` | 系统诊断 | `/doctor` |
| `/diagnose [模式]` | 结构化诊�? full/quick/codes | `/diagnose full` |

---

## 6. LLM 工具清单

以下�?AI 可以在对话中自动调用的工具（共 12 个）：

### 6.1 文件系统

| 工具 | 只读 | 说明 | 参数 |
|------|:----:|------|------|
| `bash` | �?| 执行 shell 命令 | `command`(必填), `description`, `timeout`(ms, 默认120000) |
| `powershell` | �?| 执行 PowerShell 命令(Windows) | `command`(必填), `description`, `timeout`(ms) |
| `read` | �?| 读取文件内容/列出目录 | `filePath`(必填), `offset`(起始�?, `limit`(行数) |
| `write` | �?| 写入/创建文件(完整覆盖) | `filePath`(必填), `content`(必填) |
| `edit` | �?| 精确字符串替�?| `filePath`(必填), `oldString`(必填), `newString`(必填), `replaceAll`(bool) |
| `grep` | �?| 正则搜索文件内容(基于 ripgrep) | `pattern`(必填), `include`(glob过滤), `path`(搜索路径) |
| `glob` | �?| 按通配符模式查找文�?| `pattern`(必填), `path`(搜索根目�? |

### 6.2 网络

| 工具 | 只读 | 说明 | 参数 |
|------|:----:|------|------|
| `webfetch` | �?| 获取 URL 内容(自动 HTTP→HTTPS 升级) | `url`(必填), `format`(text/html/json) |
| `websearch` | �?| 网页搜索(DuckDuckGo) | `query`(必填) |

### 6.3 交互

| 工具 | 说明 | 参数 |
|------|------|------|
| `question` | 向用户提问以获取信息 | `questions`(数组, 每个�?header/question/options/multiple) |
| `todowrite` | 创建/更新结构化任务列�?| `todos`(数组, 每个�?content/status/priority) |
| `plan_mode` | 进入计划模式(只规划不执行) | `reason` |
| `exit_plan_mode` | 退出计划模式恢复执�?| `summary` |
| `brief` | 生成上下文摘�?| `what`(描述需要什么摘�? |
| `sleep` | 暂停执行等待(最�?00�? | `seconds`(必填) |

### 6.4 Git Worktree

| 工具 | 说明 | 参数 |
|------|------|------|
| `worktree` | 创建 git worktree 分支隔离工作 | `branch`(必填) |
| `exit_worktree` | 退�?worktree 并可选合�?| `merge`(bool) |

### 6.5 任务管理

| 工具 | 说明 | 参数 |
|------|------|------|
| `task` | 创建后台任务 | `title`(必填), `description`(必填) |
| `task_list` | 列出所有任�?| (无参�? |
| `task_update` | 更新任务状�?| `taskId`(必填), `status`(必填), `output` |
| `task_stop` | 停止运行中的任务 | `taskId`(必填) |
| `task_get` | 获取任务详情 | `taskId`(必填) |
| `task_output` | 获取任务完整输出 | `taskId`(必填) |

### 6.6 团队协作

| 工具 | 说明 | 参数 |
|------|------|------|
| `agent` | 生成子代理处理复杂任�?| `type`(必填: general/explore/plan/review/test), `prompt`(必填) |
| `team_create` | 创建并行代理团队 | `name`(必填), `members`(数组: [{agent, task}]) |
| `team_delete` | 解散代理团队 | `name`(必填) |
| `cron` | 调度定期任务(cron 表达�? | `schedule`(必填), `task`(必填) |
| `send_message` | 给代�?用户发送消�?| `to`(必填), `message`(必填) |

### 6.7 开发工�?
| 工具 | 说明 | 参数 |
|------|------|------|
| `lsp` | 语言服务器操�?诊断/hover/引用/定义) | `action`(必填), `filePath`(必填) |
| `skill` | 调用预定义技能工作流 | `name`(必填), `args` |

### 6.8 MCP 工具

| 工具 | 说明 | 参数 |
|------|------|------|
| `mcp` | 调用 MCP 服务器工�?| `serverName`(必填), `toolName`(必填), `arguments`(对象) |
| `mcp_resources` | 列出 MCP 服务器资�?| `server`(可选过�? |
| `mcp_read_resource` | 读取 MCP 资源内容 | `serverName`(必填), `uri`(必填) |

---

## 7. 权限系统

### 7.1 四种权限模式

| 模式 | 说明 | 适用场景 |
|------|------|----------|
| `default` | 每次写入/执行操作需用户确认 | 日常使用，安全第一 |
| `plan` | 只允许只读操作，所有写入被拒绝 | 纯分�?规划，不修改任何文件 |
| `auto` | 安全操作自动批准，危险操作仍需确认 | 信任 AI 做常规操�?|
| `bypass` | 所有操作自动批准，完全不确�?| 完全信任（脚�?批处理场景） |

### 7.2 权限交互

当模式为 `default` �?`auto` 时，危险操作会触发确认提示：

```
�?bash: rm -rf node_modules && npm install
  允许执行此命令？ (y/n/a)
```

- **y** �?本次允许
- **n** �?本次拒绝
- **a** �?始终允许此类操作（添加永久规则）

### 7.3 危险命令检�?
内置分类器自动识别危险命令：
- 删除操作: `rm -rf`, `del /s`, `rmdir`
- Git 危险操作: `push --force`, `reset --hard`, `rebase`
- 系统修改: `chmod 777`, `chown`, `shutdown`
- 网络操作: `curl | sh`, `wget | bash`

### 7.4 切换模式

```
/mode default    # 恢复默认（每次确认）
/mode auto       # 自动模式
/mode plan       # 只读模式
/mode bypass     # 跳过所有确�?```

---

## 8. 配置文件

### 8.1 全局配置

路径: `~/.cove/config.json`

```json
{
  "model": "claude-sonnet-4-20250514",
  "provider": {
    "name": "anthropic",
    "api_key": "sk-ant-...",
    "api_keys": [],
    "base_url": ""
  },
  "permission_mode": "default",
  "max_budget_usd": 10,
  "thinking_tokens": 16000,
  "debug": false,
  "verbose": false,
  "system_prompt": "",
  "mcp_servers": {}
}
```

### 8.2 字段说明

| 字段 | 类型 | 默认�?| 说明 |
|------|------|--------|------|
| `model` | string | `"claude-sonnet-4-20250514"` | 使用�?LLM 模型名称 |
| `provider.name` | string | `"anthropic"` | AI 供应商名�?|
| `provider.api_key` | string | `""` | 单个 API 密钥 |
| `provider.api_keys` | []string | `[]` | 多密钥轮询池（优先于 api_key�?|
| `provider.base_url` | string | `""` | 自定�?API 端点 |
| `permission_mode` | string | `"default"` | 权限模式 |
| `max_budget_usd` | float64 | `10` | 每会话最大预算（美元�?|
| `thinking_tokens` | int | `16000` | 思�?token 配额（最�?1024�?|
| `debug` | bool | `false` | 调试模式 |
| `verbose` | bool | `false` | 详细输出 |
| `system_prompt` | string | `""` | 自定义系统提示词（追加到默认提示词后�?|
| `mcp_servers` | map | `{}` | MCP 服务器配�?|

### 8.3 项目级配�?
路径: `<项目根目�?/.cove.json`

项目级配置会覆盖全局配置中的以下字段�?- `model`
- `permission_mode`
- `max_budget_usd`
- `system_prompt`
- `mcp_servers`

```json
{
  "model": "deepseek-chat",
  "permission_mode": "auto",
  "max_budget_usd": 2,
  "system_prompt": "这是一�?Go 微服务项目，使用 gin 框架",
  "mcp_servers": {
    "database": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-postgres"],
      "env": {"POSTGRES_URL": "postgres://localhost/mydb"}
    }
  }
}
```

---

## 9. 环境变量

### 9.1 通用

| 环境变量 | 说明 |
|----------|------|
| `LLM_API_KEY` | 通用 API 密钥（任意供应商通用�?|
| `LLM_BASE_URL` | 通用 API 地址（所有供应商�?fallback�?|

### 9.2 按供应商

| 供应�?| 环境变量 |
|--------|----------|
| Anthropic | `ANTHROPIC_API_KEY` |
| OpenAI | `OPENAI_API_KEY` |
| DeepSeek | `DEEPSEEK_API_KEY` |
| 智谱 GLM | `GLM_API_KEY` / `ZHIPU_API_KEY` / `BIGMODEL_API_KEY` |
| Kimi | `KIMI_API_KEY` / `MOONSHOT_API_KEY` |
| 通义千问 | `QWEN_API_KEY` / `DASHSCOPE_API_KEY` |
| 豆包 | `DOUBAO_API_KEY` / `ARK_API_KEY` / `VOLCENGINE_API_KEY` |
| OpenRouter | `OPENROUTER_API_KEY` |
| 硅基流动 | `SILICONFLOW_API_KEY` |
| Groq | `GROQ_API_KEY` |
| Together | `TOGETHER_API_KEY` |
| Fireworks | `FIREWORKS_API_KEY` |
| xAI | `XAI_API_KEY` / `GROK_API_KEY` |
| Mistral | `MISTRAL_API_KEY` |

### 9.3 优先�?
1. `config.json` 中的 `provider.api_key`
2. 供应商专用环境变量（�?`ANTHROPIC_API_KEY`�?3. `LLM_API_KEY`（通用 fallback�?
---

## 10. 供应商支�?
### 10.1 支持的供应商列表

| 供应�?| 协议 | 默认 Base URL | 备注 |
|--------|------|---------------|------|
| `anthropic` | Anthropic Messages API | `https://api.anthropic.com/v1` | 默认供应商，支持提示缓存 |
| `openai` | OpenAI Chat Completions | `https://api.openai.com/v1` | |
| `deepseek` | OpenAI 兼容 | `https://api.deepseek.com` | |
| `glm` | OpenAI 兼容 | `https://open.bigmodel.cn/api/paas/v4` | 智谱 |
| `kimi` | OpenAI 兼容 | `https://api.moonshot.cn/v1` | |
| `qwen` | OpenAI 兼容 | `https://dashscope.aliyuncs.com/compatible-mode/v1` | 通义千问 |
| `doubao` | OpenAI 兼容 | `https://ark.cn-beijing.volces.com/api/v3` | 豆包/火山方舟 |
| `openrouter` | OpenAI 兼容 | `https://openrouter.ai/api/v1` | 多模型网�?|
| `siliconflow` | OpenAI 兼容 | `https://api.siliconflow.cn/v1` | 硅基流动 |
| `groq` | OpenAI 兼容 | `https://api.groq.com/openai/v1` | 超快推理 |
| `together` | OpenAI 兼容 | `https://api.together.xyz/v1` | |
| `fireworks` | OpenAI 兼容 | `https://api.fireworks.ai/inference/v1` | |
| `xai` | OpenAI 兼容 | `https://api.x.ai/v1` | Grok |
| `mistral` | OpenAI 兼容 | `https://api.mistral.ai/v1` | |
| `openai-compatible` | OpenAI 兼容 | (需要手动设�?base_url) | 任意兼容 API |

### 10.2 切换供应�?
```
/provider deepseek
/model deepseek-chat
/api-key sk-xxx
```

### 10.3 使用自定�?自建模型

```
/provider openai-compatible
/base-url http://localhost:11434/v1
/model llama3:70b
/api-key ollama
```

---

## 11. Multi-Key 密钥�?
当你有多�?API Key（比如多�?Anthropic 账号）时，可以配置密钥池实现自动轮转和故障转移�?
### 11.1 配置

```json
{
  "provider": {
    "name": "anthropic",
    "api_keys": [
      "sk-ant-key1...",
      "sk-ant-key2...",
      "sk-ant-key3..."
    ]
  }
}
```

### 11.2 行为

- **Round-Robin 轮询**: 每次 API 调用使用下一�?key
- **限流自动跳过**: �?429 �?key 自动冷却，到期后恢复
- **永久失败标记**: 认证失败(401/403)�?key 标记�?Dead
- **容错降级**: 所�?key 不可用时，选择冷却最快恢复的 key 继续尝试

### 11.3 注意事项

- `api_keys` 数组优先�?`api_key` 单�?- 如果只配�?`api_key`，行为与之前完全一�?- 建议至少配置 2-3 �?key 以获得更好的可用�?
---

## 12. 插件系统�?Marketplace

### 12.1 概述

插件可以�?cove 添加额外的命令、工具、技能和钩子�?
### 12.2 安装插件

```bash
# �?Marketplace 安装（推荐）
/plugin refresh                # 首次使用需刷新索引
/plugin search formatter       # 搜索可用插件
/plugin install code-formatter # 安装

# �?Git URL 直接安装
/plugin install https://github.com/user/cove-plugin-xxx.git

# 查看已安�?/plugin list
```

### 12.3 管理插件

```bash
/plugin disable my-plugin    # 禁用（保留文件，不加载）
/plugin enable my-plugin     # 重新启用
/plugin update my-plugin     # 更新到最新版�?/plugin update               # 更新所有插�?/plugin uninstall my-plugin  # 彻底删除
```

### 12.4 Marketplace 来源

默认使用官方 registry。可以添加自定义来源（企业私�?registry）：

```json
// ~/.cove/marketplace/sources.json
[
  {"name": "official", "type": "git", "url": "https://github.com/cove-plugins/registry.git", "enabled": true},
  {"name": "company", "type": "git", "url": "https://git.company.com/cove-plugins.git", "enabled": true},
  {"name": "local", "type": "directory", "url": "/path/to/local/plugins", "enabled": true}
]
```

### 12.5 开发插�?
插件目录结构�?
```
my-plugin/
├── manifest.json    # 必须
├── tools/           # 自定义工�?├── skills/          # 自定义技�?└── hooks/           # 生命周期钩子
```

`manifest.json` 格式�?
```json
{
  "name": "my-plugin",
  "version": "1.0.0",
  "description": "我的自定义插�?,
  "author": "YourName",
  "commands": ["my-cmd"],
  "tools": ["my-tool"],
  "hooks": ["on-start"],
  "skills": ["my-workflow"]
}
```

### 12.6 版本锁定

安装信息保存�?`~/.cove/marketplace/lock.json`�?
```json
{
  "plugins": {
    "code-formatter": {
      "source": "https://github.com/user/code-formatter.git",
      "version": "1.2.0",
      "commit_sha": "abc1234...",
      "installed_at": "2026-05-29T10:00:00Z",
      "auto_update": true
    }
  }
}
```

---

## 13. MCP 集成

### 13.1 什么是 MCP

MCP (Model Context Protocol) 是一个开放协议，允许 AI 代理通过标准化接口调用外部工具和访问资源�?
### 13.2 配置 MCP 服务�?
�?`config.json` 或项�?`.cove.json` 中：

```json
{
  "mcp_servers": {
    "puppeteer": {
      "command": "npx",
      "args": ["-y", "@anthropic/mcp-puppeteer"],
      "type": "stdio"
    },
    "postgres": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-postgres"],
      "env": {
        "POSTGRES_URL": "postgres://localhost:5432/mydb"
      },
      "type": "stdio"
    },
    "remote-api": {
      "type": "sse",
      "url": "http://localhost:8080/mcp/sse"
    }
  }
}
```

### 13.3 传输类型

| 类型 | 说明 |
|------|------|
| `stdio` | 启动本地子进程，通过标准输入/输出通信（默认） |
| `sse` | 通过 Server-Sent Events 连接远程服务�?|
| `http` | 通过 HTTP 请求连接远程服务�?|

### 13.4 使用 MCP

启动时自动连接配置中的所�?MCP 服务器（30秒超时）�?
```bash
/mcp list                    # 查看已连接服务器及其工具/资源
/mcp connect puppeteer       # 手动连接
/mcp disconnect puppeteer    # 断开
/mcp read postgres file:///schema.sql  # 读取资源
```

AI 可以直接调用 MCP 服务器提供的工具，就像使用内置工具一样�?
---

## 14. Skills 技能系�?
### 14.1 概述

技能是预定义的工作流程/提示词，�?AI 按照最佳实践执行特定任务�?
### 14.2 内置技�?
| 技能名 | 说明 |
|--------|------|
| `batch` | 并行工作编排（拆分任务并行执行） |
| `debug` | 系统化调试（复现→定位→修复→验证） |
| `refactor` | 安全重构（小步骤+测试验证�?|
| `review` | 代码审查（正确�?风格/性能/安全�?|
| `test` | 编写测试（单�?集成/边界案例�?|
| `verify` | 验证变更（构�?测试+lint�?|
| `simplify` | 代码精简（去除冗余，保持功能�?|
| `commit` | 生成规范�?git commit message |
| `perf` | 性能分析与优�?|
| `init` | 项目初始�?|
| `remember` | 整理记忆文件 |
| `update-config` | 修改项目配置 |
| `loop` | 循环执行任务 |
| `schedule` | 后台调度任务 |
| `skillify` | 将工作模式保存为可复用技�?|
| `stuck` | 诊断卡死的会�?|
| `claude-api` | Claude API 使用指南 |
| `claude-in-chrome` | 浏览器验证工作流 |
| `keybindings-help` | 快捷键配置帮�?|
| `lorem-ipsum` | 生成占位文本 |

### 14.3 使用技�?
```bash
/skills              # 列出所有可用技�?/skills debug        # 查看 debug 技能详�?/debug               # 直接使用技能名作为命令激�?
# 在对话中 AI 也可以自己调�?skill 工具
```

### 14.4 技能加载路径（按优先级�?
1. 内置 bundles
2. `~/.cove/skills/` �?全局用户技�?3. `~/.claude/skills/` �?兼容目录
4. `<项目>/.claude/skills/` �?项目级技�?5. `<项目>/.cove/skills/` �?项目级技�?6. 父目�?`.claude/skills/`（递归向上查找�?
### 14.5 自定义技�?
创建文件 `~/.cove/skills/my-skill/SKILL.md`�?
```markdown
---
name: my-skill
description: 我的自定义工作流
paths: src/, lib/
allowed_tools: read, grep, bash, write
---

# 我的工作�?
按照以下步骤操作�?1. 搜索相关代码
2. 分析上下�?3. 生成方案
4. 实施变更
5. 验证结果
```

YAML frontmatter 字段�?- `name` �?技能名�?- `description` �?简要描�?- `paths` �?适用的文件路径模�?- `allowed_tools` �?允许使用的工具白名单

---

## 15. 检查点与回退

### 15.1 原理

cove 使用 Git bare repository 作为影子存储，在每次文件修改前自动创建快照�?
### 15.2 自动行为

- AI 执行 `write` �?`edit` 工具前自动创建检查点
- 标签格式：`工具�? 文件路径`（如 `write: src/main.go`�?- 存储位置：`~/.cove/checkpoints/store/`（共�?bare repo�?
### 15.3 手动操作

```bash
/undo          # 回退到上一个检查点（恢复所有文件）
/checkpoints   # 列出所有检查点（最新在前，最�?0条）
```

### 15.4 排除列表

以下文件/目录不会被纳入检查点�?- `node_modules/`
- `.git/`
- `.venv/`
- `__pycache__/`
- `*.exe`, `*.dll`, `*.so`, `*.dylib`
- `target/`, `dist/`, `build/`, `.next/`

### 15.5 隔离�?
每个项目目录使用独立�?git ref（`refs/cove/<sha256(workdir)[:8]>`），互不干扰�?
---

## 16. 上下文压�?
### 16.1 自动触发

当对�?token 数超�?**64,000** 且迭代次�?> 5 且消息数 > 16 时自动触发�?
### 16.2 两层压缩策略

**Layer 1: 工具结果裁剪（无 API 开销�?*
- 将最�?6 条之前的 tool 消息结果压缩�?1 行摘�?- 格式：`[工具名] 首行内容... (N chars原始输出已压�?`
- 保留 �?00 字符的短结果

**Layer 2: LLM 摘要（需要一�?API 调用�?*
- 如果 Layer 1 后仍超阈值，调用 LLM 总结中间对话
- 保留首条用户消息 + 最�?6 条消息完�?- 中间部分替换为结构化摘要（决�?文件/状�?错误/上下文）

### 16.3 手动触发

```bash
/compact    # 立即执行压缩
```

---

## 17. 护栏防循�?
### 17.1 三级检�?
| 级别 | 条件 | 阈�?Warn) | 阈�?Block) |
|------|------|-----------|-------------|
| 精确重复失败 | 相同工具+相同参数连续失败 | 2 �?| 5 �?|
| 同工具失�?| 同一工具名连续失�?| 3 �?| 8 �?|
| 幂等无进�?| 只读工具返回完全相同结果 | 2 �?| 5 �?|

### 17.2 幂等工具

以下工具被识别为幂等（结果会被跟踪去重）�?- `read`
- `grep`
- `glob`
- `webfetch`

### 17.3 重置机制

- 成功调用会重置该工具/参数的失败计�?- 每个新用户输入会完全重置所有计数器

---

## 18. 子代理系�?
### 18.1 概述

AI 可以自动生成独立的子代理来处理复杂的并行任务。子代理有自己独立的工具集和上下文�?
### 18.2 子代理类�?
| 类型 | 说明 | 系统提示 |
|------|------|----------|
| `general` | 通用多步骤任�?| "完成任务并返回清晰结�? |
| `explore` | 代码探索 | "搜索代码库并报告发现" |
| `plan` | 方案设计 | "创建详细结构化计�? |
| `review` | 代码审查 | "检查正确�?风格/性能/安全" |
| `test` | 编写测试 | "编写并运行全面测�? |

### 18.3 子代理特�?
- **隔离环境**: 独立 tool registry，不能递归生成子代�?- **自动批准**: 子代理内部工具调用不需要用户确�?- **超时保护**: 5 分钟超时限制
- **最大迭�?*: 30 轮对�?- **结果截断**: 工具输出限制 4000 字符

### 18.4 内部委托系统

Engine 内部�?`Delegator` 管理子代理生命周期：
- 可以同时运行多个子代�?- 支持取消正在运行的子代理
- 任务完成后自动清理资�?
---

## 19. 记忆系统

### 19.1 自动学习

每轮对话结束后，后台异步分析对话内容�?- 识别**用户偏好**（编码风格、工具偏好、工作习惯）�?保存�?MEMORY
- 识别**可复用工作流**（解决特定问题的步骤）→ 保存�?SKILL

### 19.2 手动管理

```bash
/memory              # 列出所有记忆条�?/memory add <内容>   # 手动添加记忆
/memory remove <名称> # 删除记忆
```

### 19.3 记忆在对话中的作�?
- 记忆内容被注入到系统提示词中
- AI 可以参考记忆来更好地适应用户偏好
- 自动学习的记忆以 `[auto]` 标签存储

---


## 21. 自我学习系统 (Self-Learning)

cove 在每轮对话后自动运行三级自我学习管道：

### 21.1 记忆提取 (Extract)

每轮对话后，后台自动分析最近消息，提取可持久的事实：
- 用户偏好、工作习惯、项目约定
- 自动去重：新记忆与已有记忆相似度 >80% 时合并而非重复
- 存储位置：`~/.cove/memory/`

### 21.2 背景回顾 (Background Review)

每 4 条以上新消息触发一次，自动识别：
- 可复用的工作流程 → 创建新技能
- 重要的用户偏好 → 保存到记忆

### 21.3 梦境整合 (Dream)

周期性跨会话记忆整合（默认每 24 小时、5 个以上会话触发）：

1. **Orient** — 浏览已有记忆，读 INDEX.md
2. **Gather** — 搜索近期会话转录，提取信号
3. **Consolidate** — 合并、更新、删除记忆，去重，时间戳绝对化
4. **Prune** — 更新索引，删除过期条目

### 21.4 会话笔记 (Session Notes)

每轮对话自动追踪：
- **Decisions**: 用户的关键决策（"我们用 PostgreSQL"、"选 React 而不是 Vue"）
- **Discoveries**: 代码发现（"找到了 bug 的根源"、"发现了遗留的 API 模式"）
- **Errors**: 错误与解决方案
- **Tasks**: 文件操作记录

笔记持久化到 `.cove/session_notes.md`，上下文压缩后仍然可读。

### 21.5 护栏系统 (Guardrails)

防止 agent 陷入工具调用死循环：
- **精准签名追踪**: 相同工具+参数连续失败 5 次 → 阻断
- **同工具连续失败**: 8 次 → 阻断
- **幂等无进展检测**: 只读工具返回相同结果 5 次 → 阻断
- **时间窗口断路器**: 30 秒内 6 次以上失败 → 立即阻断，成功自动清零
- **检查点快照**: `write`/`edit` 操作前自动创建 git 快照，可回退

### 21.6 手动命令

```
/dream               手动触发记忆整合
/checkpoints         查看检查点历史
/skill list          查看已有技能
/memory              查看记忆
```
## 22. 渐进式引�?
### 22.1 概述

首次使用各功能时显示一次性提示，帮助用户了解功能�?
### 22.2 提示列表

| 触发场景 | 提示内容 |
|----------|----------|
| 首次 Ctrl+C | "�?Ctrl+C 可以中断当前操作。用 /stop 停止工具执行�?exit 退出程序�? |
| 首次工具执行 | "工具执行时会显示进度。你可以�?/mode auto 自动批准安全操作�? |
| 首次上下文压�?| "对话过长时会自动压缩上下文。用 /compact 可手动触发�? |
| 接近预算上限 | "接近预算上限。用 /budget <金额> 调整，或 /cost 查看详情�? |
| 首次权限询问 | "权限模式可选：default/auto/plan。用 /mode 切换�? |
| 长会�?| "我会自动记住重要的偏好和工作模式。用 /memory 查看已学习的内容�? |
| 首次文件修改 | "文件修改前会自动创建检查点。用 /undo 可回退到修改前的状态�? |

### 22.3 状态持久化

提示显示状态保存在 `~/.cove/onboarding.json`，不会重复显示�?
---

## 23. 提示缓存与速率限制

### 23.1 Anthropic 提示缓存

针对 Anthropic 供应商，自动在最�?3 条非系统消息上添�?`cache_control: ephemeral` 标记，实现跨请求的提示缓存�?
效果�?- 减少重复 token 计费
- 降低延迟（命中缓存时跳过编码�?- 对其他供应商自动跳过，无副作�?
### 23.2 速率限制监控

```bash
/ratelimit    # 查看当前速率限制状�?```

输出示例�?```
速率限制: 请求:45/60(75%) 重置:15s | Token:85.0K/100.0K(85%) 重置:1m
```

### 23.3 自动警告

�?token 剩余额度低于 **20%** 时，每次交互后自动显示黄色警告�?
### 23.4 支持�?Header

自动解析�?- 标准: `x-ratelimit-limit-requests`, `x-ratelimit-remaining-tokens` �?- Anthropic: `anthropic-ratelimit-requests-limit`, `anthropic-ratelimit-tokens-remaining` �?
---

## 24. 文件与目录结�?
### 24.1 全局目录 (`~/.cove/`)

```
~/.cove/
├── config.json              # 全局配置
├── onboarding.json          # 引导状�?├── dream.json               # 梦境配置
├── sessions/                # 会话存储
�?  └── <session-id>.json
├── plugins/                 # 插件目录
�?  └── <plugin-name>/
�?      └── manifest.json
├── skills/                  # 用户自定义技�?�?  └── <skill-name>/
�?      └── SKILL.md
├── marketplace/             # 插件市场
�?  ├── sources.json         # 市场来源配置
�?  ├── index.json           # 缓存的插件索�?�?  ├── lock.json            # 安装版本锁定
�?  └── cache/               # 市场仓库缓存
├── checkpoints/
�?  └── store/               # 检查点 git bare repo
└── cost/                    # 费用历史记录
```

### 24.2 项目级文�?
```
<project>/
├── .cove.json            # 项目配置覆盖
├── CLAUDE.md                # 项目指南（AI 上下文）
├── AGENTS.md                # 子目录级 AI 指南
├── .cove.md              # �?CLAUDE.md（备选名�?├── .cursorrules             # Cursor 兼容规则文件
└── .cove/
    └── skills/              # 项目级技�?```

### 24.3 子目录上下文发现

�?AI 操作涉及子目录中的文件时，会自动向上（最�?5 级）搜索以下文件�?- `AGENTS.md`
- `CLAUDE.md`
- `.cursorrules`
- `.cove.md`

找到后，内容（最�?2000 字符）会自动注入到工具结果中，帮�?AI 理解该子目录的特殊规则�?
---

## 25. 高级用法与技�?
### 25.1 批处�?脚本集成

```bash
# 非交互模式执行任�?cove -p "找出所�?TODO 注释并生成报�? > report.md

# 管道输入
echo "解释这段代码" | cove -p -
```

### 25.2 快速切换模�?
```bash
# 对话中随时切�?/model deepseek-chat         # 切到便宜的模型做简单任�?/model claude-sonnet-4-20250514  # 切回高质量模型做复杂任务
```

### 25.3 预算控制

```bash
/budget 1        # 限制�?$1 以内（适合试验�?/budget 50       # 长时间深度工�?/cost            # 随时查看已用额度
```

### 25.4 高效使用 Plan 模式

```bash
/mode plan       # 先让 AI 只做分析和规�?# AI 会读取文件、搜索代码，但不会修改任何内�?# 确认方案后：
/mode auto       # 切换到自动模式执�?```

### 25.5 自定义系统提示词

```bash
/system 你是一位资�?Go 工程师。遵循以下规范：
- 使用 Go 1.22+ 特�?- 错误处理使用 errors.Is/As
- 测试使用 table-driven 风格
- 不使用全局变量
```

### 25.6 MCP 服务器链

配置多个 MCP 服务器实现能力叠加：

```json
{
  "mcp_servers": {
    "browser": {"command": "npx", "args": ["-y", "@anthropic/mcp-puppeteer"]},
    "database": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-postgres"]},
    "filesystem": {"command": "npx", "args": ["-y", "@anthropic/mcp-filesystem"], "env": {"ALLOWED_DIRS": "/tmp"}}
  }
}
```

### 25.7 技能开发最佳实�?
1. �?YAML frontmatter 中明�?`allowed_tools`，限制技能可用工具范�?2. �?`paths` 限制技能作用的文件范围
3. 提示词中使用清晰的步骤编�?4. �?`/skillify` 技能将当前工作模式自动保存为可复用技�?
### 25.8 性能优化建议

- 大项目使�?`.cove.json` 指定 `system_prompt`，减�?AI 探索时间
- 配置 `thinking_tokens` �?32000+ 对复杂推理任务效果更�?- �?key 配置避免�?key 限流影响工作�?- `/compact` 在长会话中定期手动触发，保持响应速度

### 25.9 Engine 参数参�?
| 参数 | �?|
|------|------|
| 最大迭代轮�?| 200 |
| 压缩 Token 阈�?| 64,000 |
| Max API 输出 tokens | 64,000 |
| API 重试次数 | 3（指数退避） |
| Bash 默认超时 | 120 �?|
| PowerShell 默认超时 | 120 �?|
| MCP 连接超时 | 30 �?|
| WebFetch 超时 | 30 �?|
| WebSearch 超时 | 15 �?|
| 子代理超�?| 5 分钟 |
| 子代理最大迭�?| 30 |

---

## 附录 A: 故障排查

### 一键诊�?
```bash
/diagnose full    # 运行全部检查，自动修复可修复的问题
```

### API Key 无效

```bash
cove --doctor    # 检查配置和连通�?```

### 工具执行超时

增加 bash 超时：AI 会自动在 `timeout` 参数中指定更长的时间。如果系统性超时，检查网络连接�?
### 上下文窗口溢�?
```bash
/compact    # 手动压缩
/stats      # 查看当前 token 使用�?```

### 恢复误操�?
```bash
/undo           # 回退上一步文件修�?/checkpoints    # 查看可回退的检查点列表
```

### 插件安装失败

```bash
/plugin refresh  # 刷新索引
# 确认 git 可用�?git --version
```

---

## 附录 B: 快速参考卡

```
配置:  /model /provider /api-key /base-url /mode /budget /config
会话:  /cost /ratelimit /compact /history /resume /export /context /status /stats
项目:  /commit /diff /review /cd /init
检�?  /undo /checkpoints
记忆:  /memory /dream /skills
插件:  /plugin list|install|search|refresh|update|enable|disable|uninstall
MCP:   /mcp list|connect|disconnect|read
诊断:  /doctor /diagnose full|quick|codes
其他:  /help /exit /debug /system /permissions
```

---

## 26. 诊断系统

### 26.1 概述

cove 内置了结构化诊断系统，可以自动检测和修复常见问题。诊断系统使�?30+ 个错误码，覆盖配置、API、网络、模型、Shell 和数据目录等六大类问题�?
### 26.2 启动自检

程序启动时自动运�?`QuickCheck`，快速检测关键问题：
- 配置文件是否存在且可�?- API Key 是否配置
- 模型名称是否合法

发现问题时会以结构化格式输出错误码和修复建议�?*不会阻断启动流程**�?
### 26.3 使用方式

```bash
# �?REPL �?/diagnose           # 等同�?/diagnose full
/diagnose full      # 运行全部 9 项检�?/diagnose quick     # 仅运行关键检查（更快�?/diagnose codes     # 列出所有已知错误码和修复方�?
# 命令�?cove --doctor    # 等同�?/diagnose full
```

### 26.4 错误码分�?
| 范围 | 类别 | 说明 |
|------|------|------|
| E1001-E1099 | CONFIG | 配置文件问题（不存在、解析失败、权限错误） |
| E2001-E2099 | API | API 密钥问题（未配置、格式错误、过期） |
| E3001-E3099 | NETWORK | 网络连通性问题（超时、DNS、代理） |
| E4001-E4099 | MODEL | 模型相关问题（不存在、不支持、名称错误） |
| E5001-E5099 | SHELL | Shell 工具问题（git/rg 未安装、权限不足） |
| E6001-E6099 | DATA | 数据目录问题（磁盘满、路径不存在、写入失败） |

### 26.5 输出格式

```
[E2001] API_KEY_MISSING (严重)
  API 密钥未配�?  详情: provider "deepseek" 未找到有效的 API Key
  修复: 使用 /api-key 命令设置，或设置环境变量 DEEPSEEK_API_KEY
  �?可自动修复，立即生效
```

每个诊断条目包含�?- **错误�?*: 唯一标识符，方便搜索和引�?- **严重级别**: `critical`（阻断）| `warning`（影响体验）| `info`（建议）
- **修复建议**: 具体的操作步�?- **热修复标�?*: 标记�?可自动修�?的问题会尝试自动修复，修复后**立即生效**，无需重启程序

### 26.6 9 项检查清�?
| 序号 | 检查项 | 说明 |
|------|--------|------|
| 1 | 配置文件存在�?| `~/.cove/config.json` 是否存在且可解析 |
| 2 | API Key 有效�?| 当前 provider 是否配置了密�?|
| 3 | 模型名称校验 | 模型名是否在已知列表中（或为 "auto"�?|
| 4 | 网络连通�?| 能否访问当前 provider �?API 端点 |
| 5 | Shell 工具 | git、ripgrep 是否已安�?|
| 6 | 数据目录 | `~/.cove/` 目录是否可写 |
| 7 | 磁盘空间 | 数据目录所在分区是否有足够空间 |
| 8 | 会话完整�?| 历史会话文件是否可正常加�?|
| 9 | 提供商配�?| base_url 格式是否正确 |

### 26.7 热修�?(HotFix)

所有标记为 `HotFixable` 的问题在修复�?*立即在当前进程中生效**，不需要重�?exe�?
可自动修复的场景�?- 配置文件不存�?�?自动创建默认配置
- 数据目录不存�?�?自动创建
- 模型名设�?"auto" �?自动根据 provider 选择默认模型

---

## 27. 稳定性保�?
### 27.1 概述

cove 针对以下关键场景做了防护，确保不会出现卡死、崩溃等问题�?
### 27.2 已防护的场景

| 场景 | 问题描述 | 防护措施 |
|------|----------|----------|
| 权限提示卡死 | `fmt.Scanln` 在某些终端下永久阻塞 | 改用 `bufio.Scanner`，空输入默认拒绝 |
| 工具 panic 崩溃 | 单个工具�?panic 导致整个进程退�?| 所有工�?goroutine 包裹 `recover()`，panic 转为错误返回 |
| Ctrl+C 无响�?| 引擎循环不检�?context 取消 | 每轮迭代开头检�?`ctx.Err()`，立即退�?|
| 动画指示器泄�?| `WalkingIndicator.Stop()` 异步导致 goroutine 泄漏 | 使用 `doneCh` 同步等待 goroutine 完成 |
| 后台任务 panic | `runTurnEndPipeline` 中的 goroutine panic | 所�?`go` 调用包裹 `defer recover()` |
| PermissionPrompt 未设�?| 权限回调�?nil 时调用导�?nil panic | nil 检查，默认拒绝 |

### 27.3 测试覆盖

引擎核心流程�?16 个集成测试用例，覆盖所有关键路径：

```bash
go test -v ./internal/engine/ -run "TestEngine"
```

测试矩阵�?
| 测试 | 验证内容 |
|------|----------|
| BasicMessageFlow | 正常消息收发流程 |
| ContextCancellation | 超时/Ctrl+C 中断 |
| ToolExecution | 工具调用完整流程 |
| PermissionDenied | 用户拒绝权限 |
| PermissionPromptNil | 权限回调未设置不卡死 |
| ToolPanicRecovery | 工具 panic 不崩�?|
| APIError | API 错误正常返回 |
| MultipleIterations | 多轮工具调用 |
| CancelMidIteration | 工具执行中途取�?|
| EmptyToolCalls | 空工具调用列�?|
| PermissionAllowed | 用户批准权限 |
| StreamDeltas | 流式输出回调 |
| UnknownToolName | 不存在的工具�?|
| ParallelToolExecution | 并行工具执行 |
| AutoPermissionMode | auto 模式跳过确认 |

### 27.4 错误恢复策略

| 错误类型 | 恢复策略 |
|----------|----------|
| API 超时 | 指数退避重�?3 �?|
| API 429 限流 | Multi-Key 自动轮转下一�?key |
| 工具连续失败 | 护栏系统: 2次警告，5次阻�?|
| 磁盘写入失败 | 降级到内存模式继续运�?|
| MCP 连接失败 | 30 秒超时，跳过该服务器继续 |
| 解析响应失败 | 返回原始文本，不崩溃 |

### 27.5 验证方式

确认程序稳定性的推荐步骤�?
```bash
cd agent/

# 1. 运行全量单元测试
go test ./...

# 2. 运行引擎集成测试（覆盖所有卡流程场景�?go test -v -timeout 60s ./internal/engine/ -run "TestEngine"

# 3. 静态分�?go vet ./...

# 4. 启动诊断
cove --doctor
```
