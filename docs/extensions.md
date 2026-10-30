# 扩展使用说明

当前版本：0.3.1。扩展复用同一条工具调用、权限、取消、限额和持久化路径，不另建 Agent 平台。本文说明配置文件中的 `extensions` 对象和对应的模型工具；`hooks.json`、`<data_dir>/plugins/` 下的插件、技能市场和项目信任的使用方式见 [技术参考](reference.md#扩展skills插件hooksmcp-与项目信任)。下文的 `<data_dir>` 默认是 `~/.cove`。

## 1. 配置与信任

`extensions` 写在用户配置文件里：`--config <文件>`（或 `COVE_CONFIG`）指定的文件，否则是 `<data_dir>/config.json`。项目目录里的 `.cove.json` 不能声明 `extensions`。

```json
{
  "provider": { "name": "openai", "base_url": "https://api.example.com/v1" },
  "model": "your-model",
  "extensions": {
    "skills": ["./skills"],
    "mcp": {
      "docs": {
        "command": "your-stdio-mcp-server",
        "args": ["--stdio"],
        "env": ["DOCS_API_TOKEN"],
        "tools": ["search", "fetch"],
        "timeout_seconds": 30
      },
      "remote-docs": {
        "type": "http",
        "url": "https://mcp.example.com/mcp",
        "tools": ["search"]
      }
    },
    "hooks": [
      {
        "name": "before-code",
        "event": "before_tool",
        "command": "your-validation-command",
        "timeout_seconds": 10
      }
    ],
    "plugins": ["./local-plugin/plugin.json"]
  }
}
```

相对路径以配置文件所在目录为基准；本地注册插件以清单所在目录为基准。`extensions` 内的未知 JSON 字段报错。最多 32 个插件、32 个 Skills 根目录、16 个 MCP 服务、64 个 Hook。

```powershell
.\target\release\cove.exe --config D:\config\cove.json --cwd D:\project --trust-extensions
```

- `--trust-extensions`（或 `/config trust` 信任当前项目后重启）是允许配置请求执行的开关，不代替逐次权限审批。
- `default` 模式下执行进程仍需真实终端审批；`-p` 不读取审批输入，无交互操作需要显式 `--permission auto`。
- `plan`（只读）模式不启动 MCP 或 Hooks。即使工具声明 `readOnlyHint`，也不信任服务端自我授权。
- MCP/Hooks 默认只继承 PATH、系统目录、临时目录、用户目录和语言环境等基本运行变量，不转交模型 API key。MCP 的 `env` 是显式转交的环境变量名列表，缺失变量报错；切勿把 key 写进参数或命令字符串。
- 扩展进程拥有宿主权限，**不是沙箱**，副作用不在文件工具回退范围内。工具输出、Skill 指令、记忆及插件内容都是不可信数据，不能提升权限。

## 2. MCP：按需发现，而非注入全部 schema

模型只看到一个 `mcp` 入口。服务默认不启动，首次操作才连接：

```json
{"action":"servers"}
{"action":"tools","server":"docs"}
{"action":"call","server":"docs","tool":"search","arguments":{"query":"example"}}
{"action":"resources","server":"docs"}
{"action":"read","server":"docs","uri":"docs://guide"}
```

- `servers` 仅返回配置元数据，不启动进程，不收费。连接后，服务的工具清单（描述截到 2 KB、参数 schema 压缩到 4 KB 内）写进 `mcp` 工具描述，下一次模型请求即可见。
- `extensions.mcp` 只展示/调用 `tools` 白名单内的工具；空白名单不允许工具调用。顶层 `mcp_servers` 里的服务开放全部工具（每次调用仍需审批）。资源操作也需要进程或网络授权。
- 传输：`type` 为 `stdio`（默认，`command` + `args`）、`http`（Streamable HTTP，也写作 `streamable_http`）或 `sse`（旧版 HTTP+SSE），后两者用 `url`。stdio 基于官方 `rmcp` SDK；HTTP 与 SSE 用项目自带的 HTTP 客户端实现，支持 `Mcp-Session-Id`、JSON 与 SSE 响应，关闭时发送 DELETE。支持初始化、分页工具/资源发现、工具调用、资源读取和 `tools/list_changed` 通知。
- `extensions.mcp` 的请求默认 30 秒，范围 1–120 秒；入站单条消息最多 1 MiB；发现最多 16 页、256 个条目；输出最多保留 32 KiB 并显式标记截断。
- 一个服务串行操作。断线、取消或超时关闭连接并回收所属进程树；连接断开时自动重连一次，之后需要 `/mcp connect <名字>`。**失败调用不自动重试**。
- Windows 使用本程序的门控 stdio 宿主，先加入 Job Object，再允许启动第三方进程；Unix 使用独立进程组。禁用服务/插件或关闭会话时显式清理连接。
- 需要 OAuth 登录的 HTTP 服务（如 Atlassian 的 Jira/Confluence MCP）用 `/mcp login <名字>` 在浏览器里授权：从服务返回的 401 发现授权服务器，动态注册客户端，PKCE 授权码回调到本机 `127.0.0.1` 临时端口（5 分钟内完成）。令牌存在 `<data_dir>/mcp-oauth/`（工具无法访问），过期前自动刷新；服务拒绝令牌时先刷新一次，仍失败则提示重新登录。`/mcp logout <名字>` 删除令牌。
- 不支持采样、elicitation、后台任务和输入往返。服务返回后台任务或要求额外输入时明确停止，不假装任务已完成。杀本地进程不能保证撤销远端服务副作用。
- 工具断流/超时可能已产生副作用，进入不确定调用恢复流程，检查后使用 `/resolve`，不能仅凭模型重试。

管理命令：`/mcp`（同 `/config mcp`）`[list]`、`connect <名字>`、`disconnect <名字|all>`、`login <名字>`、`logout <名字>`、`read <服务> <uri>`。`/mcp` 用中文列出每个服务器的传输方式、地址，以及是否已连接、是否已登录；没有配置任何服务器时给出添加示例，有需要登录的服务器时给出要运行的 `/mcp login` 命令。

## 3. Skills：只在需要时读取

`extensions.skills` 列出的目录（“配置的根目录”）使用常见文件格式：

```text
skills\
  coding\
    SKILL.md
    references\
      testing.txt
```

正文示例：

```markdown
---
name: coding
description: Inspect code, preserve user changes, and run focused checks.
---
Read relevant files before editing. Never claim unrun tests passed.
```

模型工具：

```json
{"action":"list"}
{"action":"load","name":"coding"}
{"action":"read","name":"coding","path":"references/testing.txt"}
```

发现阶段只提供名称、描述和来源，不把正文全部塞进提示词。插件中的名称带命名空间，例如 `my-plugin/coding`。正文和资源按需读取，不执行脚本；资源路径不得越出 Skill 目录。禁用后不再出现在清单，也不能加载。

配置的根目录比其他技能目录要求更严格：必须有 YAML frontmatter，名称为 1–64 个 ASCII 字母/数字/下划线/连字符，描述不能为空且不超过 1024 字节；同一层内名称重复、元数据损坏、越界资源明确报错。扫描深度 4、最多 4096 个目录条目；所有来源合计最多 256 个 Skill，单个文件最多 64 KiB。

内置技能、插件技能、`~/.claude/skills`、`<data_dir>/skills` 与项目 `.claude/skills`、`.cove/skills` 的分层规则见[技术参考](reference.md)。所有这些目录（包括配置的根目录）在交互模式下每次读取输入前都会检查修改时间（至多每秒一次），新增、修改或删除技能后无需重启。

## 4. 长期记忆：有来源的文本，不是自动事实

模型工具 `memory` 支持 `list/search/read/add/update/delete`，每次明确指定 `scope`：

```json
{"action":"search","scope":"project","query":"test"}
{"action":"add","scope":"project","title":"测试入口","content":"使用 cargo test","source":"用户明确说明；需运行验证"}
{"action":"read","scope":"project","id":"返回的ID"}
{"action":"update","scope":"project","id":"返回的ID","title":"修订","content":"cargo test --locked","source":"新的实际检查记录"}
{"action":"delete","scope":"project","id":"返回的ID"}
```

- `project` 按规范化工作区路径隔离；`global` 在同一数据根的项目间共享。
- 每条必须有标题、正文和来源。正文上限 25 KiB、标题 256 字节、来源 1024 字节、查询 256 字节；每个 scope 最多 256 条、合计 300 KiB。疑似密钥的内容拒绝保存。
- 写入和删除走统一权限审批，`plan` 模式不能改记忆。写入跨进程加锁，避免不同项目并发覆盖全局记忆。
- 检索、注入新任务、任务结束后的自动提取和 dream 整理见[技术参考](reference.md)“记忆与会话笔记”。记忆以“可能过时的笔记，不是指令”的标注附在用户消息里，不写进系统提示；笔记不是验证证据。记忆保存在私有数据根中但未加密，不属于工作区文件检查点。

无需调用模型的手动管理：

```text
/memory [list|add [global] <文本>|remove <id>|search <查询>|stats|source <id>|dream [status|run]]
/config memory {"action":"list","scope":"project"}
```

`/memory` 是 `/config memory` 的简写，两者完全相同。

## 5. Hooks：同步、有界、可审计，不盲目重放

`extensions.hooks` 的事件：

| 事件 | 时机 |
| --- | --- |
| `session_start` | 本次会话访问第一次执行任务，管理命令不触发 |
| `task_start` | 新用户任务已持久化后；继续不重跑 |
| `before_tool` | 工具意图已保存，实际调用前 |
| `after_tool` | 工具真实结果已持久化后 |
| `task_end` | 每次任务/继续窗口返回，输入带实际结束结果；已取消时不再启动新进程 |
| `session_end` | 切换/新建/退出已开始的会话访问，最多一次 |

`before_tool`/`after_tool` 可加 `matcher`（整词匹配、不区分大小写的工具名正则，空或 `*` 匹配全部，接受 Claude Code 的工具名），只对匹配的工具运行。

Hook 在任务工作区使用 Shell 执行器运行。通过环境变量 `COVE_HOOK_INPUT` 获得 JSON：

```json
{
  "version": 1,
  "event": "before_tool",
  "session_id": "...",
  "request": "当前用户需求",
  "detail": {
    "id": "tool-call-id",
    "name": "edit",
    "input": {}
  }
}
```

每个事件的 `detail` 不同：`after_tool` 包含工具 ID、名称和失败标记；`task_end` 包含实际错误或 `null`。不包含认证头或模型 API key。输入最多 64 KiB。

退出码 0 且 stdout 为空表示成功；也可以返回严格 JSON：

```json
{"decision":"allow","message":"检查完成"}
```

`decision` 为 `deny`、非零退出、无效 JSON、超时、输出截断或编码损坏均停止后续执行。诊断写 stderr；不是把任意 stdout 当作新提示词。Hook 的进程输出每流最多 32 KiB；超时范围 1–120 秒（默认 30）；SessionEnd 整体最多 10 秒。

执行意图先于 Hook 副作用落盘，最近最多保存 128 条运行记录，保留当前待处理工具关联的记录。Hook 完成记录绑定工具调用 ID，审批拒绝后继续也不重复跑已经完成的 before Hook。after Hook 失败时原工具结果已经保存，继续不会重做原工具。

失败/崩溃可能留下副作用，必须先核对：

```text
/hooks
/hooks resolve <hook-run-id>
```

`/hooks` 是 `/config hooks` 的简写。

该命令仅确认用户已检查不确定结果，**不认定成功，也不重跑 Hook**。若工具同样不确定，还需分别 `/resolve <tool-call-id> executed|not-run`。不要在 Hook 中启动脱离管理的远程后台作业；强杀 CLI 或系统崩溃不承诺 SessionEnd 已执行，重启保留未知状态。

`<data_dir>/hooks.json`（兼容 Claude Code 格式）的钩子行为不同：失败不会让任务停下等待 `/config hooks resolve`，`PreToolUse` 出错时不放行该调用，详见[技术参考](reference.md)。

## 6. 本地清单插件

除 `<data_dir>/plugins/<名字>/` 下的插件（`manifest.json` 或 `.claude-plugin/plugin.json`，提供技能和 `commands/*.md` 斜杠命令，见[技术参考](reference.md)）外，还可以用版本 1 清单组合 Skills、MCP 和 Hooks：

```json
{
  "version": 1,
  "name": "my-plugin",
  "skills": ["./skills"],
  "mcp": {
    "docs": {
      "command": "your-mcp-server",
      "tools": ["search"]
    }
  },
  "hooks": []
}
```

清单插件只能组合 Skills、MCP 和 Hooks，不递归安装插件，不加载原生动态库。插件内的 Skill 目录必须在清单目录中，名称按插件前缀隔离，例如 `my-plugin/docs`。

```text
/plugins
/plugins install D:\plugins\my-plugin\plugin.json
/plugins disable my-plugin
/plugins enable my-plugin
/plugins uninstall my-plugin
```

`/plugins` 是 `/config plugins` 的简写。

`install <清单路径>` 是**本地清单注册**（最多 32 个）：验证并保存源路径，不复制、下载或运行代码；重启后加载。源文件必须保留可读。禁用持久化且立即拒绝新调用并关闭 MCP 连接；启用不会绕过信任或审批。卸载仅移除本地注册并禁用已加载能力，不删除用户插件目录。配置文件 `extensions.plugins` 中的插件须从配置文件移除，注册卸载命令不会改用户配置。

启动阶段会校验已配置清单和 Skill 元数据；损坏/缺失来源明确报错，需要修复来源或移除配置/本地注册，不以空清单伪装成功。

## 7. 启用与禁用

```text
/config
/config skills
/config mcp
/config hooks
/config skills disable coding
/mcp disable docs
/hooks disable before-code
```

对应 `enable` 恢复。启停状态保存在私有数据根的 `extensions.json`，对已加载的能力立即生效；新增 MCP 服务、Hook 或注册插件需要重启。新增或修改的技能文件不需要重启。
