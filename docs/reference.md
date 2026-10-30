# cove 技术参考

本文是完整的功能与实现细节参考；日常使用请先看 [使用说明](../README.md)。

cove 是聚焦编码任务的终端 Agent，用 Rust 编写，项目名和可执行文件名为 **cove**。当前版本 **0.3.0**：单个可执行文件，常驻交互控制台加单次任务模式，用户命令集中在少量入口下。架构与设计原则见 [架构设计](design.md)，扩展配置见 [扩展使用说明](extensions.md)。

## 已实现

- OpenAI Chat Completions 兼容协议、Anthropic Messages 协议的 SSE 流式调用。
- 文件工具：`read` 读文件（默认 2000 行，可用 `offset`/`limit` 续读）或列目录，支持 GBK/GB18030；`write` 自动建父目录，覆盖前必须读过当前版本；`edit` 支持 `replace_all`、空白容错匹配和缩进映射；`grep`（有 ripgrep 时优先用 rg）和 `glob`（支持 `**`）。详见下文“文件、输出与失败语义”。
- Shell 自动选择解释器：Windows 上优先 Git Bash，没有时用 PowerShell；工具描述和系统提示会写明实际方言。
- `question` 交互式提问：一次可问多个问题，每个问题可带编号选项，按编号或自由文本回答（仅交互模式）；对带选项的问题用自己的话回答时，问卷就此结束，剩下的问题不会拿这段话去作答。
- `todo` 任务计划清单：多步任务中维护待办项，支持 `priority`、`cancelled`、`id` 和 `depends` 依赖；同一时间至多一项进行中。
- 连续的只读工具调用（read/glob/grep/repo_map/web_fetch/web_search）并发执行（最多 8 个），结果顺序和日志顺序保持确定；写入和 Shell 仍逐个执行。
- `repo_map` 仓库结构索引：列出源文件中的类型、函数和方法（签名和行号），按跨文件引用做 PageRank 排序；可按路径片段或标识符查询，并带出调用方和被调用方所在的文件。支持 Rust、Go、Python、TS/JS、C#、Java。解析结果按文件的修改时间和大小缓存，再次查询只解析变化的文件，未命中缓存的文件在多个线程上并行解析。
- Shell 实时输出：命令运行时逐行显示 stdout 和 stderr（交互控制台里是 `│` 开头的暗色行，stderr 为红色；`-p` 和管道输出里分别以 `|`、`!` 开头），验证命令同样实时显示；结束后只再打印一行状态，不重复正文。
- 完成前验证门禁：本轮通过文件工具改过文件时，模型宣布完成后自动运行检查命令；失败输出回给模型继续修，最多 3 轮。
- `web_fetch` / `web_search`：抓取公网 https 页面（拒绝内网和本机地址），以及通过 Tavily、Brave 或 DuckDuckGo 搜索网页。
- 子 Agent：六种内置类型（general/explore/plan/review/test/verify）加配置里的自定义类型；`execute_plan` 按 todo 依赖图并行执行计划；worktree 工具；回归验证工具；可持久化、可中断/暂存/唤醒的后台 Agent（各自独立的 git worktree）。详见下文“子 Agent、计划、Worktree 与回归验证”。
- 集中权限检查：`default`、`plan`、`auto`、`bypass` 四种模式（`ask`/`readonly` 仍可用作别名），交互中用 `/mode` 即时切换；按 sh/PowerShell 语法切分命令的风险分类器；任何模式都拦截的灾难命令；`policies.json` 允许/拒绝/询问规则，审批时可“本项目总是允许”。
- 任务守卫与完成闸门：重复失败熔断、循环检测、收尾催促（空回复、只说不做、todo 未完成、完成自检）、计划模式与 review/once 审阅流程、提交前验证、自审子 Agent、每个任务的验收报告。
- 有界模型调用次数、输出、上下文、请求空闲时间、任务与命令超时。
- Ctrl+C 取消、子进程树回收；Windows 使用 Job Object，在加入 Job 后才发送用户脚本。
- 工具调用 ID 配对、完整参数校验、reasoning/Anthropic thinking signature 回传。
- 可选费用估算和预算；费用未知不作为零费用。
- 模型文本写 stdout；工具结果、退出码和诊断写 stderr。
- 常驻交互控制台：任务运行时输入行留在输出下方，可随时插话引导、排队命令或中断；审批和提问在输入行上作答；命令、技能和 `@路径` 补全，输入历史与搜索，多行输入，粘贴图片。不同内容分开显示：你的输入在灰底色块里，模型回复渲染 Markdown（标题、粗体、代码、列表、引用、表格），工具调用是一行标题加结果摘要，状态行显示进度、耗时和 token。详见下文“交互控制台”。
- 简体中文界面：提示、错误、帮助和命令输出都是中文，错误会说明下一步怎么做；发给模型的系统提示和工具结果保持英文。
- 会话保存/恢复、显式继续；会话以完整快照加追加日志落盘，已经完成的工具调用不重放。
- 手动/自动上下文压缩；保留完整历史归档、当前需求和最近完整工具交换。
- 文件工具变更的 unified diff、检查点预览与确认回退；发现用户后续修改时拒绝覆盖。
- 会话历史（标题、列表、详情、删除、清理、修复、Markdown 导出、`max_sessions` 自动清理）；按月费用账本与 `/status cost`；覆盖 Shell 改动、可再撤销的 Git 快照 `/undo`；BM25 记忆检索与注入、自动提取、dream 整理和会话笔记。
- MCP 客户端：stdio、Streamable HTTP 与旧版 HTTP+SSE 三种传输，支持 `extensions.mcp` 和顶层 `mcp_servers` 两种写法；连接后工具清单写进 `mcp` 工具描述，处理 `tools/list_changed`，断线自动重连一次，从不重试失败调用。
- Skills：内置 12 个技能，多目录分层发现（项目覆盖用户、用户覆盖插件和内置），运行中新增或修改的技能无需重启即可使用，`paths` 按文件类型提示，`/<技能名>` 直接调用；自带技能市场（内置于程序、可离线使用，也可指向自建市场）；后台复盘自动沉淀技能；项目/全局带来源记忆的检索和增删改。
- 同步生命周期 Hooks 与兼容 Claude Code 格式的 `hooks.json`（`PreToolUse` 可阻止工具调用）；插件（`manifest.json` 或 `.claude-plugin/plugin.json`，`commands/*.md` 斜杠命令、git 安装、市场搜索）和本地插件清单。
- 项目信任：`.cove.json` 未信任时只应用安全字段，`/config trust` 后才应用 MCP、系统提示、校验命令等敏感字段。
- `draw_image` 画 PNG、`browser` 用无头 Chrome/Edge 渲染网页或截图；浏览器实操验收、双方案竞跑、自动化任务与收件箱、统一变更集（`/changes`）和远程监督（`/remote`），见[外围工作流](#外围工作流changesautoremote)。

架构以 [执行内核](../src/engine/agent.rs)为中心，依赖 Provider、ToolExecutor、UserInput、SessionPersistence 和 Lifecycle 小接口。协议适配在 [provider](../src/model/provider.rs)，工具调度在 [tools](../src/tools/mod.rs)，权限在 [permission](../src/safety/permission.rs)，进程在 [process](../src/process.rs)，会话存储在 [session](../src/session/mod.rs)，压缩在 [context](../src/engine/context.rs)，文件检查点在 [workspace](../src/session/workspace.rs)，终端交互在 [ui](../src/app/ui.rs)，由 [app](../src/app/mod.rs)组装。扩展工具通过 [组合工具适配器](../src/tools/extension_tools.rs)复用同一条持久化执行路径，不引入另一套 Agent 引擎或通用插件框架。

## 构建

使用 Rust stable；当前本地验证版本为 **Rust 1.99.1，Windows x86_64 GNU**。CI 固定同版本，并配置 Windows/macOS/Linux 检查；配置 CI 不代表这些平台已经实际运行通过。

从仓库根目录运行：

```powershell
Set-Location D:\github\cove
cargo build --release --locked
.\target\release\cove.exe --help
```

Windows MSVC 工具链需要 Visual Studio C++ Build Tools；GNU 工具链需要对应 MinGW 链接工具。Linux/macOS 使用各自平台的构建工具。发布产物的跨平台分发仍需后续验证。

## 配置与运行

### 首次运行

直接运行 `cove.exe`（在终端里运行或双击都可以）。如果还没有配置模型或 API key，会自动进入设置向导：

1. 选择供应商；
2. 确认 API 地址；
3. 确认模型（deepseek、openai、anthropic、bedrock 有默认模型，其他供应商需要填写）；
4. 输入 API key，输入时显示 `*`；环境变量里已有对应 key 时直接回车即可，不写入文件。

设置保存到 `%USERPROFILE%\.cove\config.json`，以后启动不用再配。双击启动时会先询问项目目录，否则 agent 会把 exe 所在的 `target\release` 当成项目；出错时窗口会停住等你按回车，不会一闪就关。向导只在交互模式下出现：`-p`、位置参数任务、`--continue`、非终端输入或设置了 `COVE_NO_SETUP=1` 时直接报错。保存前向导会发一个极短的验证请求（“reply ok”，最多等 20 秒）；失败时可以选择重试、重新输入 key、换供应商，或不验证直接保存。运行中可用 `/config setup` 重新走一遍向导，结果立即生效。

cove 只读取自己数据目录（默认 `~/.cove`）里的配置和数据。工作目录下的 `.cove.json` 按[项目信任](#项目信任与-covejson)规则部分应用。凭据来自环境变量，或用户自己的私有配置文件中的 `provider.api_key`；不接受命令行 key 参数，也不保存到会话。

PowerShell 示例：

```powershell
$env:COVE_API_KEY = "<你的 API key>"
$env:COVE_MODEL = "<供应商支持的模型 ID>"
$env:COVE_BASE_URL = "https://api.deepseek.com/v1"

# 只读任务：不能执行 Shell，也不能修改文件
.\target\release\cove.exe -p "阅读代码并说明这个项目的入口" --cwd D:\project --permission readonly

# 常驻交互 + 本地逐次审批，需要真实终端
.\target\release\cove.exe --cwd D:\project

# 也可传入一个任务，任务结束后退出
.\target\release\cove.exe "修复测试失败，并运行相关测试" --cwd D:\project

# 无交互写入/执行：显式允许文件修改及宿主 Shell，务必只用于可信任务
.\target\release\cove.exe -p "修复测试失败，并运行相关测试" --cwd D:\project --permission auto
```

Anthropic：

```powershell
$env:ANTHROPIC_API_KEY = "<你的 API key>"
Remove-Item Env:COVE_API_KEY -ErrorAction SilentlyContinue
.\target\release\cove.exe --provider anthropic --model "<模型 ID>" --base-url https://api.anthropic.com/v1 -p "分析代码" --cwd D:\project
```

`--provider` 接受下文“配置文件”一节列出的供应商名；`openai` 表示 OpenAI 兼容协议，不要求必须使用 OpenAI 服务。API 根地址包含 `/v1` 等供应商要求的前缀，不包含最终的 `chat/completions` 或 `messages`。HTTPS 为默认要求，显式 loopback 地址可用 HTTP 做本地测试。

### 配置文件

配置是一个 JSON 文件：顶层写通用设置，`profiles` 里按名字写多套模型配置（各自的 provider 块、权限与限额字段），`active_profile` 选默认的一套。加载顺序如下：

1. 传了 `--config <文件>`（或 `COVE_CONFIG`），就读这个文件；
2. 否则读 `<数据目录>/config.json`，默认是 `%USERPROFILE%\.cove\config.json`，文件存在才读；
3. 工作区里的项目配置（如 `.cove.json`）**永远不会自动加载**。

完整示例见 [config.example.json](config.example.json)：

```json
{
  "active_profile": "deepseek",
  "permission_mode": "default",
  "profiles": {
    "deepseek": {
      "model": "deepseek-v4-pro",
      "provider": { "name": "deepseek", "base_url": "https://api.deepseek.com/v1" },
      "max_output_tokens": 8192,
      "input_price": 0.27,
      "output_price": 1.1,
      "max_budget_usd": 10
    },
    "local": {
      "model": "qwen3.6-27b",
      "provider": { "name": "openai-compatible", "api_key": "local", "base_url": "http://127.0.0.1:8080/v1" }
    }
  }
}
```

| 字段（顶层或 profile 内） | 含义 | 对应命令行 |
| --- | --- | --- |
| `model` | 模型 ID | `--model` / `COVE_MODEL` |
| `provider.name` | 供应商名，决定协议、默认地址和 key 环境变量 | `--provider` / `COVE_PROVIDER` |
| `provider.base_url` | API 根地址；粘贴完整的 `/chat/completions`、`/messages` 也能识别 | `--base-url` / `COVE_BASE_URL` |
| `provider.api_key` | 可选，写入配置文件的 key；逗号分隔可写多个 | 无（只能用环境变量） |
| `provider.api_keys` | 多个 key 的数组，轮流使用（见下文“模型、供应商与切换”） | 无 |
| `model_fast` | 快速模型：简单任务和后台调用（压缩等）用它；`"off"` 关闭 | `/model fast <名>` |
| `thinking` | Anthropic 扩展思考：`adaptive` 或 `disabled`；不写则不发送该字段 | 无 |
| `effort` | Anthropic `output_config.effort`：`low`/`medium`/`high`/`xhigh`/`max` | 无 |
| `show_reasoning` | `true` 时实时显示模型的推理/思考文本（stderr，终端里为暗色斜体，以 `✻ 思考过程` 开头）；默认隐藏，交互控制台只留一行 `✻ 思考了 Ns` | 无 |
| `model_context_windows`（仅顶层） | 按模型名（小写）记录的上下文窗口；服务端报“上下文超长”时自动写入 | 无 |
| `permission_mode` | `default`、`plan`、`auto`、`bypass`；旧写法 `ask`=default、`readonly`=plan | `--permission` |
| `max_iterations` | 每个任务最多调用模型的次数 | `--max-turns` |
| `max_turn_minutes` | 任务总时限（分钟，必须为正数） | `--timeout`（秒） |
| `max_budget_usd` | 预算；必须同时配置两个单价，否则忽略并给出警告 | `--budget` |
| `system_prompt` | 附加在内置安全规则之后的自定义指令，不替换内置规则 | 无 |
| `max_output_tokens`、`context_window`、`context_bytes`、`input_price`、`output_price` | 含义同命令行参数 | 同名参数 |
| `subagent_max_iterations` | 每个子 Agent 最多调用模型的次数，默认 60 | 无 |
| `agent_types`（仅顶层） | 自定义子 Agent 类型：`{"名字": {"description", "instructions", "tools": [...], "readonly": bool}}`，不能覆盖内置类型 | 无 |
| `max_background_agents`（仅顶层） | 同时运行的后台 Agent 上限，1–16，默认 4 | 无 |
| `regression_command`（仅顶层） | `regression` 工具 `runner=command` 使用的命令，必须含 `{filter}` 占位符 | 无 |
| `experimental_tools`（仅顶层） | 默认 `false`；为 `true` 时启用 task/team/send_message/brief/sleep 等实验工具 | 无 |
| `done_verify_commands`（仅顶层） | 完成前检查命令列表；配置后替代自动推断，写空数组 `[]` 表示关闭门禁 | 无 |
| `done_verify_auto`（仅顶层） | 默认 `true`：按项目文件自动推断检查命令。`Cargo.toml` → `cargo check --all-targets`，`go.mod` → `go build ./...`，`tsconfig.json` 且已安装 typescript → `npx tsc --noEmit`，`package.json` 有 build 脚本且已安装依赖 → `npm run build --if-present`，`pyproject.toml`/`setup.py` → `python -m compileall -q .`，`*.sln`/`*.csproj` → `dotnet build` | 无 |
| `done_verify_tests`（仅顶层） | 默认 `true`：自动推断时再加上项目测试，并按本轮改动收窄范围：Cargo workspace 只测改动所在的包（`cargo test -p a -p b`），Go 项目（`go test`）只测改动的包目录，另有 `dotnet test`、`npm test`（有真实 test 脚本时）、`python -m pytest -q`（有 pytest 配置或 tests 目录时） | 无 |
| `done_check`（仅顶层） | `on`/`off`/`auto`（默认）：改过文件后，模型收尾前追问一次“需求是否全部完成”。`auto` 只在交互会话且非 Anthropic 供应商时开启 | 无 |
| `done_self_review`（仅顶层） | `off`（默认）/`on`/`auto`：收尾前让只读子 Agent 审阅本轮 diff（最多 40 KB），`auto` 只在改动不少于 40 行时启用 | 无 |
| `workflow`（仅顶层） | `direct`（默认）/`review`/`once`，含义见下文“计划模式与审阅流程” | 无 |
| `done_verify_timeout_seconds`（仅顶层） | 每条检查命令的时限；默认 120 秒，dotnet/npm/npx 为 300 秒 | 无 |
| `shell`（仅顶层） | Shell 工具的解释器：`auto`（默认；Windows 上 Git Bash 优先，否则 PowerShell；其他平台 bash 或 sh）、`bash`、`powershell`，或解释器的完整路径。也可用环境变量 `COVE_SHELL` | `auto` |
| `web_search`（仅顶层） | `{"provider": "tavily"\|"brave"\|"duckduckgo", "api_key": "..."}`。未配置时依次看 `TAVILY_API_KEY`、`BRAVE_API_KEY`/`BRAVE_SEARCH_API_KEY`，都没有就用 DuckDuckGo | 无 |
| `skills_registry`（仅顶层） | 技能市场来源：`registry.json` 的 https 地址，或本地路径（`registry.json` 文件或包含它的目录）；不写时用程序内置的市场。见下文“技能市场” | 无 |

- **优先级**：命令行/环境变量 > 选中的 profile > 顶层字段 > 内置默认值。profile 由 `--profile <名>` / `COVE_PROFILE` 选择，没有指定时用 `active_profile`；profile 名不存在时报错。
- **provider 块整体替换**：profile 里写了 `provider`，就整体替换顶层的 `provider`（不逐字段合并）；命令行传 `--provider` 时，配置里的整个 provider 块（包括 key 和地址）都不再使用，避免把一家的 key 发给另一家。
- **支持的供应商**：`openai`、`openai-compatible`、`anthropic`、`deepseek`、`glm`、`kimi`、`qwen`、`doubao`、`openrouter`、`siliconflow`、`groq`、`together`、`fireworks`、`xai`、`mistral`、`bedrock`，也接受别名 `claude`、`moonshot`、`zhipu`、`dashscope` 等。其中 `anthropic` 和 `bedrock` 走 Anthropic Messages 协议，其余走 OpenAI 兼容协议。不认识的名字按 OpenAI 兼容协议处理，但必须写 `base_url`。Anthropic 协议的根地址缺少 `/v1` 时会自动补上。`bedrock` 只支持 Bedrock API key（`AWS_BEARER_TOKEN_BEDROCK` 或 `provider.api_key`），不支持 SigV4 签名。
- **Bedrock**：默认走 AWS 原生的 Bedrock Runtime 接口（`https://bedrock-runtime.<区域>.amazonaws.com`，区域取 `AWS_REGION`/`AWS_DEFAULT_REGION`，默认 `us-east-1`），与 Claude Code 的 `CLAUDE_CODE_USE_BEDROCK` 相同：按 AWS 账号授权，不按调用方 IP 所在地区限制。也可以把 `base_url` 设为 Anthropic 兼容端点 `bedrock-mantle`，但该端点会拒绝不受支持地区的 IP（报 “unsupported countries”）。模型可写短名（`anthropic.claude-haiku-4-5`），首次请求时查询账号可用的推理配置，解析为完整 ID（`us.anthropic.claude-haiku-4-5-20251001-v1:0`），优先与区域对应的 `us.`/`eu.`/`apac.`，其次 `global.`；Claude Code 的 `[1m]` 后缀会开启 1M 上下文。
- **网络代理**：默认跟随系统代理（Windows“Internet 选项”/macOS 网络设置，VPN 客户端开启“系统代理”模式时写入）以及 `HTTPS_PROXY`、`ALL_PROXY`、`NO_PROXY` 环境变量；本机地址始终直连。也可用配置 `"proxy": "http://127.0.0.1:7890"`（支持 `http`、`https`、`socks5`、`socks5h`）、命令行 `--proxy` 或环境变量 `COVE_PROXY` 指定，`none` 表示全部直连。`/status` 显示当前代理。`web_fetch` 为防止访问内网，只使用显式指定的代理。
- **调试与输出**：`debug`（或 `-d`/`--debug`）把每次模型调用、工具调用和压缩的轨迹记录（不含正文）实时打印到 stderr；`verbose`（或 `--verbose`）完整显示工具结果，不只显示摘要（交互控制台）或前 12 行（`-p` 和管道输出）。已废弃的 `telemetry`、`thinking_tokens` 会被静默忽略。
- **API key 查找顺序**：`COVE_API_KEY` > `provider.api_key`/`provider.api_keys` > 供应商对应的环境变量（例如 `DEEPSEEK_API_KEY`、`KIMI_API_KEY`/`MOONSHOT_API_KEY`）。每一处都可以用逗号分隔写多个 key。带 `****` 的已掩码 key 会被忽略。key 写进配置文件时，文件必须保持私有；Unix 下如果其他用户可读，启动时会给出警告。
- **未知字段**：不认识的字段只给出警告（“ignoring keys not supported by cove”）并忽略，不会报错。顶层的 `mcp_servers`、`disabled_skills`、`skill_review` 见下文“扩展”。顶层 `base_url` 会报错，请写到 `provider.base_url`。
- `/status` 会显示当前生效的供应商、协议、模型、地址、profile 和配置文件路径。

配置文件还支持 `extensions` 对象。最小例子：

```json
{
  "extensions": {
    "skills": ["./skills"],
    "mcp": {
      "example": {
        "command": "your-mcp-server",
        "args": [],
        "env": ["EXAMPLE_SERVICE_TOKEN"],
        "tools": ["search"]
      }
    },
    "hooks": [],
    "plugins": []
  }
}
```

路径相对配置文件目录解析。MCP 不在启动时运行；实际启动或调用既需要 `--trust-extensions`，也需要当前权限模式批准进程操作。`readonly` 不能运行 MCP 或 Hooks，不信任服务端的只读声明。凭据只指定环境变量名，不写入 JSON 值。完整配置、命令、Hooks 协议和限制见 [扩展使用说明](extensions.md)。

## 模型、供应商与切换

### 快速模型与路由

配置 `model_fast` 后，同一个供应商上有两个模型：`model`（高级模型）和 `model_fast`（快速模型）。每个新任务开始时打一次分，决定整个任务用哪个模型，并在 stderr 显示 `[model] <模型> (score ...)`：

- 复杂度关键词（refactor、architecture、debug、optimize… 及对应中文“重构、架构、调试、优化、性能、安全审计”等）+0.45；消息长度最多 +0.20（1000 字符封顶）；提到的文件路径最多 +0.15；近期快速模型失败率最多 +0.10；预算越紧扣分越多（最多 −0.10）。
- 总分 ≥ 0.35 用高级模型；消息不少于 2000 字符一律用高级模型。
- 上一个任务跑在高级模型上时，40 字以内的简短追问（如“继续”）留在高级模型，避免中途换模型丢掉提示缓存。
- **升级**：快速模型上验证门禁失败，或连续 3 批工具调用全部失败，本任务剩余部分改用高级模型。
- **回退**：重试后仍是 5xx/429 时，本任务换到另一个模型一次；快速模型返回 404（模型不存在）时改用主模型。
- 上下文压缩等后台调用使用快速模型；其他模块通过同一个入口（`background_provider`）做摘要、记忆提取等廉价调用。
- 费用按实际应答的模型计价，`/model` 列出每个模型的调用次数、token（含缓存读写）和费用。快速模型不在价格表里时按主模型价格计，预算仍是上限。

### 供应商特性

- **Anthropic**：`thinking`、`effort` 写入请求；thinking 块（含签名）原样回传，压缩后只保留当前工具循环和最后一条助手消息里的 thinking。系统提示和最后 3 条消息带 `cache_control: ephemeral` 提示缓存，并发送 prompt-caching beta 头（Bedrock 不发）。
- **缓存计费**：解析 Anthropic 的 `cache_read_input_tokens`/`cache_creation_input_tokens`、OpenAI 的 `cached_tokens`、DeepSeek 的 `prompt_cache_hit_tokens`。缓存读取按内置价格表里各模型的缓存价（未知模型按输入价的 0.1 倍），缓存写入按输入价的 1.25 倍。
- **OpenAI 兼容**：o1/o3/o4/gpt-5 系列用 `max_completion_tokens`；`deepseek-reasoner`/`deepseek-r1` 不发送工具定义；`reasoning_content` 只在带工具调用的助手消息上回传。
- **多 key 轮换**：`provider.api_keys`（或逗号分隔的环境变量）轮流使用。429 让该 key 休息（按 `Retry-After`，没有则 60 秒），有其他 key 时立即换用；401/403 把该 key 标为失效并立即用下一个重试。
- **限流信息**：记录响应里的 `x-ratelimit-*` / `anthropic-ratelimit-*` 头，`/status limits` 显示剩余请求数、token 数和重置倒计时。
- **上下文超长**：服务端报上下文超长时，自动压缩并重试一次；错误里写明了窗口大小时，按它缩小本会话的上下文预算，并写入用户配置的 `model_context_windows`，下次启动直接生效。

### 附件与图片

- 消息里的 `@路径`（可加引号：`@"带 空格.png"`）指向已存在的文件时作为附件发送，并从文字中去掉；不是文件的 `@xxx`（邮箱、`@Override` 等）原样保留。
- `/attach [add] <文件…>` 预先挂载，`/attach list`、`/attach remove <序号>`、`/attach clear` 管理；挂载的文件随下一条消息发送一次后清空。
- PNG/JPEG/GIF/WebP 按文件头识别，作为图片发送：Anthropic 用 base64 source，OpenAI 兼容协议用 data URL 的 `image_url`。长边超过 1568 像素或超过 5 MB 时自动缩放、重新编码。UTF-8 文本文件作为文字附加（最多 200 KB）；其他二进制文件拒绝附加。
- `read` 工具读到图片时返回图片本身（源文件不超过 5 MiB、边长不超过 4096 像素），模型可以直接“看”截图。
- 当前模型不支持视觉时，如果 `model`/`model_fast` 中有支持视觉的模型，带图片的请求自动改用它；都不支持时，图片换成一行说明文字再发送。

### 运行中切换

| 命令 | 行为 |
| --- | --- |
| `/model` | 当前供应商、协议、地址、主模型/快速模型/本任务模型、key 数量和各模型用量 |
| `/model <名>` | 切换主模型；`auto` 表示供应商默认模型，`deepseek/deepseek-chat` 这类带本供应商前缀的名字会去掉前缀（OpenRouter 等网关保留） |
| `/model fast <名\|off>` | 设置或关闭快速模型 |
| `/model provider <名> [模型]` | 切换供应商；原来的 key 和地址不会带给新供应商。原模型是旧供应商的默认模型时跟着换成新供应商的默认模型；缺 key 时在终端里提示输入（不回显） |
| `/model profile list\|switch\|save\|delete\|show <名>` | 管理 profile；`show` 中的 key 已打码 |
| `/config model\|provider\|base_url\|system\|max_output_tokens [值]` | 不带值显示当前值，带值修改 |
| `/config api_key [key]` | 不带值时在终端里不回显地输入；显示时只显示打码的 key |
| `/config budget [金额\|auto\|off\|save]` | 金额、`auto`（已花费 + 10 美元）、`off` 只改本会话；`save` 把当前预算写入配置 |
| `/config setup` | 重新运行设置向导（含验证请求） |
| `/status limits` | 最近一次响应的限流信息 |

修改立即生效，并原子写入用户配置文件：只改相关字段，其他字段保持不变；当前有 profile 时写入该 profile。新配置先完整加载校验，失败时不写文件、不生效。启动时用 `--model`/`--provider`/`--base-url` 指定的值会在第一次修改时一并写入，配置文件始终与正在运行的设置一致；环境变量里的 key 不写入文件。

会话不再绑定模型和供应商：恢复的会话可以换模型或供应商继续，每次请求按实际模型记账；工作区必须一致。

## 扩展：Skills、插件、Hooks、MCP 与项目信任

所有扩展都遵循同一条信任线：项目里带来的内容可以作为**文本**进入提示（技能、插件命令），但任何**执行**（MCP 进程/连接、Hooks、git 克隆）都需要 `--trust-extensions` 或 `/config trust`，并且每次动作仍走当前权限模式的审批。下文的 `<data_dir>` 默认是 `~/.cove`。

### Skills

- **发现顺序**（后者覆盖同名前者）：内置技能（编译进二进制，随版本更新）→ 插件的 `skills/` → `~/.claude/skills` → `<data_dir>/skills` → 配置的 `extensions.skills` → 项目内从仓库根到 cwd 每一级的 `.claude/skills`、`.cove/skills`（越内层越优先）。cwd 以下的子目录不扫描，避免依赖包里的技能混进来。
- **运行中生效**：交互模式下每次读取输入前（至多每秒一次）检查这些目录和技能文件的修改时间，有新增、修改或删除就重新加载，补全列表和 `/<技能名>` 立即可用，不需要重启。重新加载失败时保留原来的技能并给出警告。
- **文件格式**：`<目录>/<名字>/SKILL.md` 或 `<目录>/*.md`，可选 YAML frontmatter：`name`、`description`（展示时截到 120 字符）、`paths`（如 `"*.rs, src/**/*.ts"`）、`allowed_tools`（也接受 `allowed-tools`）、`steps`。没有 frontmatter 时名字取目录/文件名，描述取正文第一行。
- **安全**：项目技能文件如果是指向项目外的链接，拒绝加载并警告；技能正文始终作为不可信数据交给模型，`allowed_tools` 只是收窄工具选择的提示，不授予任何权限。配置 `disabled_skills: ["名字"]` 可整体移除技能。
- **调用**：`/<技能名> [参数]` 把技能（编号步骤清单、工具限制说明、正文和你的参数）作为一次新任务发给模型。内置命令同名时内置命令优先；输错命令会提示“你是不是想输入 /…”。模型也可以用 `skill` 工具按需加载。
- **按文件类型提示**：带 `paths` 的技能在 read/write/edit 触及匹配文件时，于该次工具结果后附 `<relevant_skills>`，每个会话每个技能只提示一次。
- **自动技能**：交互会话中，每完成至少 3 个任务、新增至少 4 条消息且期间用过非只读工具时，用后台模型复盘一次，输出 `SKILL: 名字 | 何时使用 | 步骤` 的会存为 `<data_dir>/skills/auto-<slug>/SKILL.md`。含注入指令或疑似密钥的提议直接丢弃，永不覆盖已有的非自动技能。`"skill_review": false` 关闭；`-p` 单次任务不复盘。
- **管理**：`/skills` 列出技能（名字、来源、描述），`/skill <名>` 查看正文；`/config skills [list]`、`show <名>`、`create <名>`（生成模板）、`install <名|https URL>`、`marketplace [关键词]`。新建和安装的技能写到 `<data_dir>/skills/<名字>/SKILL.md`，写入前需要授权；已存在的同名技能不会被覆盖。

### 技能市场

- **内置市场**：仓库里的 [`marketplace/`](../marketplace/) 目录就是 cove 的技能市场，`marketplace/registry.json` 是清单，`marketplace/skills/<名字>/SKILL.md` 是技能正文，首批有 `security-audit`、`api-design`、`dockerize`、`i18n`、`ci-cd`。整个目录编译进程序，所以 `/config skills marketplace [关键词]` 和 `/config skills install <名字>` 默认**离线**可用，不访问网络。
- **换用其他市场**：在用户配置顶层设置 `skills_registry`：
  - `https://…/registry.json`：从网络读取清单（每次读取需网络授权，经 SSRF 防护的 HTTP 客户端，最多 1 MiB）；
  - 本地路径：一个 `registry.json` 文件，或包含它的目录（相对路径按该配置文件所在目录解析，未使用配置文件时按数据目录）。

  配置的市场读取失败或格式错误时，给出警告并改用内置市场。项目 `.cove.json` 里的 `skills_registry` 属于敏感字段，项目经 `/config trust` 信任后才生效。
- **清单格式**：`registry.json` 是数组，每项 `{"name", "description", "url", "author"}`。`name` 只能用字母、数字、`-`、`_`（最长 64）；`url` 可以是完整的 `https://` 地址，也可以是相对 `registry.json` 所在位置的路径（如 `skills/dockerize/SKILL.md`，不允许 `..` 越出市场目录）。
- **直接安装**：`/config skills install https://…/SKILL.md` 不经过清单，直接下载单个技能文件（只接受 https，最多 256 KiB）。
- 安装前检查 frontmatter，含可疑注入内容的技能拒绝安装。技能正文始终是不可信的指令数据，安装后可用 `/config skills show <名>` 审阅。
- **发布自己的市场**：按下面的结构建一个目录，放到任意静态文件服务（例如 Git 仓库的 raw 地址），让使用者把 `skills_registry` 指向其中的 `registry.json`；团队内部也可以直接共享这个目录，配置成本地路径。

  ```text
  my-market/
    registry.json                 [{"name": "deploy", "description": "一句话说明", "url": "skills/deploy/SKILL.md", "author": "team"}]
    skills/deploy/SKILL.md        YAML frontmatter（name、description）+ 正文
  ```

### 插件

- 插件放在 `<data_dir>/plugins/<名字>/`，清单为 `manifest.json`（`name`、`version`、`description`、`author`、`commands`、`skills`），也接受 Claude Code 插件的 `.claude-plugin/plugin.json`。`skills/` 下的技能参与发现；`commands/*.md`（frontmatter `description` + 正文，`$ARGUMENTS` 替换为参数，没有占位符时参数附在末尾）成为 `/<命令名>`。多个插件同名命令时，按插件名排序靠前的生效。
- `/config plugins [list]`、`search [关键词]`（Claude 官方插件市场索引）、`install <名字|git URL>`、`update [名字]`（`git pull --ff-only`）、`enable|disable|uninstall <名字>`。git 安装只接受 `https://`、`ssh://`、`git@host:path`，以 `git clone --depth=1 --config core.symlinks=false` 克隆到暂存目录，校验名字后再改名就位，失败不留残余；git 运行时禁用终端和凭据管理器弹窗。另有组合 Skills、MCP 和 Hooks 的本地清单插件（`/config plugins install <清单路径>`），格式见[扩展使用说明](extensions.md#6-本地清单插件)。
- cove 不执行插件里的任何代码：命令是提示文本，技能是数据。

### Hooks（`hooks.json`）

除 `extensions.hooks` 外，还读取 `<data_dir>/hooks.json`（只读用户自己的数据目录，不读项目目录），格式兼容 Claude Code：

```json
{"hooks": {"PreToolUse": [{"matcher": "Bash|Write", "command": "python check.py", "timeout": 30}],
           "PostToolUse": [{"command": "notify-send done", "async": true}]}}
```

- 事件：`PreToolUse`/`BeforeTool`、`PostToolUse`/`AfterTool`、`SessionStart`、`SessionEnd`。`matcher` 是整词匹配、不区分大小写的正则，空或 `*` 匹配全部；可以写 Claude Code 的工具名（`Bash`→shell，`Edit`/`MultiEdit`→edit，`Write`、`Read`、`Grep`、`Glob`、`WebFetch` 等）。默认超时 60 秒。
- 标准输入是 JSON：`event`、`hook_event_name`、`tool_name`、`tool_input`、`model`、`session_id`、`cwd`（过长的字段会截断）。环境变量会清理，只保留 PATH 等系统变量，不含 API key。Windows 上通过管道把输入交给命令的第一个程序。
- 阻止：`PreToolUse` 输出 `{"continue": false, "message": "原因"}`（或 Claude 的 `{"decision": "block", "reason": ...}`），或以退出码 2 结束（stderr 为原因）。被阻止的调用不执行，模型收到失败结果和原因，任务继续。阻止结果写入会话日志，恢复任务时不会再执行被阻止的调用。
- 出错处理：`PreToolUse` 钩子出错或超时时**不放行**该工具调用（钩子是安全闸门时，出错放行等于没有闸门）；其余事件出错只记录警告。与 `extensions.hooks` 不同，`hooks.json` 的失败不会让任务停下等 `/config hooks resolve`。`async: true` 的钩子后台运行、不能阻止。
- 需要 `--trust-extensions` 或项目信任，否则启动时提示并不加载。

### MCP

- 除 `extensions.mcp` 外，也可以写顶层 `mcp_servers`：`{"名字": {"command", "args", "env": {"K": "V"}, "type": "stdio"|"sse"|"http"|"streamable_http", "url"}}`。只写 `url` 时按 Streamable HTTP 处理。`mcp_servers` 的服务开放全部工具（每次调用仍需审批）；`extensions.mcp` 仍用显式 `tools` 白名单。两处同名会报错。
- `env` 里的值视为密钥：只传给子进程，不显示、不写入会话；列表里只显示变量名，URL 只显示协议、主机和路径（去掉查询串和账号）。
- 传输：stdio 经内置进程宿主启动；HTTP 与 SSE 用项目自带的 reqwest 实现（不引入第二个 reqwest），支持 `Mcp-Session-Id`、JSON 与 SSE 响应，关闭时发送 DELETE。
- 连接后，服务的工具清单（描述截到 2 KB、参数 schema 压缩到 4 KB 内）会出现在 `mcp` 工具描述中，下一次模型请求即可见；收到 `tools/list_changed` 后下次操作前刷新。连接断开时自动重连一次，之后需要 `/mcp connect`。
- `/mcp`（同 `/config mcp`）`[list]`、`connect <名字>`、`disconnect <名字|all>`、`login <名字>`、`logout <名字>`、`read <服务> <uri>`。`/mcp` 列出每个服务器的传输方式、地址、是否已连接、是否已登录，没有配置时说明怎么添加，需要登录时给出要运行的命令。
- **需要登录的 HTTP 服务（OAuth）**：例如 Atlassian（Jira/Confluence），配置 `"mcp_servers": {"atlassian": {"type": "http", "url": "https://mcp.atlassian.com/v2/mcp"}}`，然后运行 `/mcp login atlassian`：按 MCP 授权流程自动发现授权服务器、动态注册客户端，在浏览器里登录授权（PKCE，回调到本机 `127.0.0.1` 临时端口，5 分钟内完成）。令牌保存在 `<数据目录>/mcp-oauth/`（工具无法访问），过期前自动刷新；服务拒绝令牌时先刷新一次，仍失败则提示重新 `login`。`logout` 删除保存的令牌。

### 项目信任与 `.cove.json`

- 启动时读取 cwd 下的 `.cove.json`（必须是不超过 1 MiB 的普通文件，不跟随链接；格式错误只警告）。它合并到用户配置的顶层，选中的 profile 仍覆盖其上。
- **无需信任**即生效的安全字段：`model`、`model_fast`、`permission_mode` 为 `default`/`plan`、调低的 `max_budget_usd`、只做减法的 `done_verify_auto`/`done_verify_tests`/`skill_review`（设为 `false`）、`done_verify_timeout_seconds`、`done_self_review`、`done_check`、`thinking`、`effort`、`show_reasoning`、`disabled_skills`、`max_iterations` 等限额。
- **需要信任**的敏感字段：`auto`/`bypass` 权限、调高预算、`system_prompt`、`mcp_servers`、`done_verify_commands`、开启校验、`web_search`、`provider`、`memory_embedding`、`skills_registry`、单价等。未信任时启动会列出被忽略的字段；不支持的字段（如 `profiles`、`extensions`）单独列出。
- `/config trust` 把项目根目录（git 根，否则 cwd）记为可信，并按**本次启动读到的内容**记录 `.cove.json` 的 sha256，写入 `<data_dir>/trusted_projects.json`；文件之后再改动就需要重新信任。重启后生效。可信项目等同于加了 `--trust-extensions`，自动验证门禁也通过 `cove::trust::is_trusted(cwd)` 查询同一份记录。

## 会话、恢复与交互

不带提示词启动 REPL；`-p` 或位置提示词为单次任务。交互控制台的启动信息里显示会话 ID 前 8 位；`-p`、管道和简单模式启动时打印一行 `[session] <完整 ID>`。

| 命令 | 行为 |
| --- | --- |
| `/help`、`/status` | 帮助、状态、上下文与累计用量 |
| `/new` | 保存当前会话并新建；不回退文件 |
| `/resume [list\|all]`、`/resume <n\|id>`（也可写 `/history`） | 列出当前工作区（`all`：所有工作区）会话，显示标题和对话轮数（从未提问的空会话不列出）；按编号或 ID（前缀）切换。列表后直接输入数字即可恢复该条；在交互控制台里列表同时以选择器打开，输入文字过滤，上下键选择，Enter 恢复。选中其他工作区的会话时，保存当前会话后在那个目录重启并恢复（同 `/cd`） |
| `/resume search <关键词>` | 按标题和用户请求检索所有工作区的会话（BM25，支持中文） |
| `/resume detail\|delete <n\|id>` | 查看详情 / 删除会话（含归档和文件检查点）；不能删除当前会话 |
| `/resume clear [all] [confirm]`、`/resume clean`、`/resume export [file]` | 批量清理（先预览，加 `confirm` 才执行）/ 修复历史（先备份）/ 导出 Markdown 记录 |
| `/continue`，或直接输入 `继续` / `continue` | 继续中断任务，授予新的模型调用窗口，不清空累计费用；当前会话没有未完成任务时（例如刚启动），回到本工作区上次的任务 |
| `/status usage`（或 `/usage`） | 当前会话的账户与用量：认证方式（供应商及 key 来源）、估算总费用、等待模型响应的累计时间（API 耗时）、会话创建至今的时间、文件工具新增/删除的行数、模型调用次数，以及每个模型的输入/输出/缓存读写 token、调用次数和费用（含子 Agent 的花费） |
| `/status cost [period] [export [file]]` | 按供应商/模型统计用量与估算费用，可导出 CSV |
| `/compact` | 付费摘要压缩，原始上下文先归档 |
| `/diff` | 有 Git 快照时显示本任务首个快照以来的整树变更（含 Shell 改动）；否则显示文件工具跟踪的变更 |
| `/undo`、`/undo confirm` | 预览 / 授权后回退当前任务的文件工具检查点（不含 Shell 改动），并列出最近的 Git 快照 |
| `/undo list`、`/undo <#n\|hash>`、`/undo files <#n\|hash> <文件…>`、`/undo confirm <token>` | Git 快照：列表 / 预览整树或单个文件的还原，凭预览给出的 token 确认执行（含 Shell 改动，还原本身可再撤销） |
| `/resolve <call-id> executed` | 用户确认不确定调用已执行，禁止重放；结果标为未验证失败 |
| `/resolve <call-id> not-run` | 检查证据确认未执行后允许重试 |
| `/config` | Skills、MCP、Hooks、插件及记忆管理；`/mcp`、`/plugins`、`/hooks`、`/memory` 是 `/config mcp` 等的简写 |
| `/memory`（同 `/config memory`）`[list\|add [global] <文本>\|remove <id>\|search <查询>\|stats\|source <id>\|dream [status\|run]]` | 记忆管理；`/config memory <JSON>` 仍按 memory 工具参数执行 |
| `/status agents [stop\|park\|resume\|send\|merge\|discard <id\|main> …]` | 子 Agent 与主任务的生命周期：列表、中断、暂存、唤醒、发消息、合并/丢弃后台 Agent 的分支；`/status agents repair` 修复中断后遗留的回归验证临时还原 |
| `/exit` | 保存并退出；EOF（Ctrl+D）同样保存 |

未知斜杠命令报错，不发送给模型。

### 交互控制台

终端支持控制序列时，REPL 由一个常驻控制台接管键盘：输出显示在上方，输入行始终留在下方，任务运行时也一样。任务运行时，输入行上方的状态行显示转圈动画、当前在做什么、已用时间和 token 数，例如 `⠹ 运行 cargo test… 12s · ↑18.2k ↓1.1k tokens · 按两次 Esc 中断`。

状态行下面常驻一行用量，空闲和运行时都显示：`上下文 52.1k/200k（26%）· 本会话 1.2M tokens · $3.20`。
- **上下文**：模型调用后取服务商报告的本次输入 token（含缓存读写），即模型实际读到的对话长度；启动、压缩、`/new`、`/resume` 之后历史变了，改用估算值（消息、系统提示和工具定义之和）。
- **窗口**：当前模型的上下文窗口。
- **本会话**：会话账本里的输入、输出和缓存读写 token 之和，以及估算花费（价格未知时不显示）。

上下文达到自动压缩线（上下文预算的 75%，与 `/status` 显示的一致）的 80% 时，这一行变成黄色，并提示可先 `/compact`。窗口未知时不显示这一行。

一次任务在屏幕上是这样的：

```text
>  介绍一下这个项目                        ← 你的输入（灰底色块）

✻ 思考了 1s                               ← 隐藏的思考过程

● 我先看看代码。                           ← 模型回复（白色 ●）

● 读取 Cargo.toml                          ← 工具调用（青色 ●）及结果摘要
  └ 5 行 #1
● 搜索 "name"
  └ 1 处匹配，1 个文件 #2
● 运行 cargo test
  │ test result: ok. 214 passed            ← 实时输出
  └ ✓ 退出码 0 #3

● 结论                                     ← Markdown：标题、列表、代码、表格
• 入口在 src/main.rs

✓ 完成 · 2 次模型调用 · 3 个工具 · 2.6k tokens · $0.02 · 48s
```

- **显示规则**：模型回复的 Markdown 边接收边渲染：标题加粗（一、二级加下划线），`**粗体**`，反引号里的代码名、路径和命令为浅橙色，列表符号换成 `•`，引用以 `│` 开头，分隔线画成横线，代码块保留原文（围栏行为暗色）便于复制，表格收齐后按列对齐。工具调用一行写明动作和关键参数（读取的文件、搜索的内容、运行的命令），下面用 `└` 给出摘要（行数、匹配数、退出码等），`#n` 可用 `/expand n` 看完整输出。同时运行的多个工具，各自的标题和结果放在一起。`-p`、管道和简单模式保持原来的机器可读行（`[tool]`、`[ok]`、`[summary]`），供脚本和 worker 解析。

- **任务运行中输入**：普通文字作为插话，在任务的下一次模型调用时交给模型（标注为用户在任务运行时输入的指令），最多排 16 条；任务结束时还没送达的插话成为下一条消息。斜杠命令和带附件的消息排队，等当前任务结束后依次执行。
- **中断**：任务运行中连按两次 **Esc**（2 秒内）或按 **Ctrl+C** 中断任务，`/continue` 可以继续。空闲时在空行上按 Ctrl+C 只提示用 `/exit` 或 Ctrl+D 退出；**Ctrl+D** 在空行上保存并退出。
- **审批与提问**：在同一输入行上作答。审批显示在黄色边框里：第一行是“需要批准”和命令类型（只读、构建/测试、未识别等），然后是要执行的操作和红色的风险警告，接着是编号选项（参照 Claude Code 和 Codex）：`1. 允许这一次 (y)`、`2. 本次会话不再询问 (s)`、`3. 本项目以后都不再询问 (a)`、`4. 拒绝 (n)`，“不再询问”两项后面写明这条规则会放行什么（如“git 常规操作（push、pull、merge 等；--force、reset、clean 仍会询问）”“以 `npm run` 开头的命令”“本项目内的文件修改”）。会话规则只在内存里，退出即失效；项目规则写入 policies.json；没有 policies.json 时不提供第 3 项，生成不了规则的操作（如扩展进程）只有 1 和拒绝。按 `y`/`s`/`a` 立即提交，上下键在选项间切换后回车；大写的 `N` 是默认值，直接回车即拒绝。输入 `n <原因>` 或任何不是选项的文字，都当作拒绝理由交给模型。也接受 `yes`/`always`/`session`/`no` 和中文“是、允许、本次会话、总是、否、拒绝”。Esc 或 Ctrl+C 取消当前提问。
- **拒绝后任务继续**：拒绝一次调用不会结束任务。模型收到“用户拒绝了”和你的理由，被要求不要重试、也不要换个等价命令绕过，而是换别的办法或询问你；屏幕上这一步显示为 `└ 已拒绝：<理由>（已告诉模型）`。拒绝不算工具失败。后台 Agent 和远程审批的拒绝同样处理。
- **补全**：Tab 补全斜杠命令、子命令（由 `/help` 的用法行自动生成，新增子命令无需另外登记）、技能名、插件命令和 `@路径`；候选列在输入行下方，上下键选择。
- **历史**：提交的输入保存在 `<data_dir>/input_history.jsonl`（最多 1000 条），上下键翻阅，**Ctrl+R** 反向搜索（再按 Ctrl+R 找更早的匹配，Enter 采用，Esc 取消）。含 API key、token、密码等疑似密钥的输入（包括 `/config api_key …`、带账号的 base_url）不写入历史。
- **多行输入**：Ctrl+J、Alt+Enter、Shift+Enter、Ctrl+Enter 插入换行；行尾输入 `\` 再回车也会换行；粘贴的多行文本保持原样，不会逐行提交。多行时上下键在行间移动。
- **编辑键**：Ctrl+A/Ctrl+E 或 Home/End 到行首/行尾，Alt+B/Alt+F 或 Ctrl/Alt+←/→ 按词移动，Ctrl+W 或 Alt/Ctrl+Backspace 删前一个词，Ctrl+K 删到行尾，Ctrl+U 删到行首；Esc 清空输入，紧接着 Ctrl+Z 恢复；Ctrl+L 清屏。
- **Agent 面板**：Alt+M 打开或关闭输入行下方的子 Agent / 后台 Agent 面板（Esc 也可关闭）。
- **图片**：粘贴或拖入图片文件路径（PNG/JPEG/GIF/WebP，可多个、可带引号）会作为附件挂在下一条消息上；Windows 上 Alt+V 附加剪贴板里的图片；空行上 Alt+Backspace 移除最后一个附件。只有附件、没有文字时，按 Enter 让模型分析附件。
- **环境变量**：设置 `NO_COLOR` 去掉颜色（光标控制保留）；`COVE_PLAIN_REPL=1` 改用简单的逐行读取（无常驻输入行、补全和任务中输入，命令在任务之间执行，用 Ctrl+C 取消任务）；`COVE_ESC_INTERRUPT=0` 让单独的 Esc 不再中断任务。stdin 或 stderr 不是终端时也自动使用简单模式。

```powershell
# 恢复后进入交互；cwd 须与原会话一致，模型和供应商可以不同
.\target\release\cove.exe --cwd D:\project --resume "<session-id>"

# 无交互继续；需要修改文件或执行 Shell 时显式授权
.\target\release\cove.exe --cwd D:\project --resume "<session-id>" --continue --permission auto
```

默认数据目录为 Windows `%USERPROFILE%\.cove`，其他平台 `$HOME/.cove`。可用 `--data-dir` 或 `COVE_DATA_DIR` 覆盖。数据目录在工作区之内时（例如直接在家目录里运行 `cove`）照常启动，但它受保护：文件工具拒绝访问其中的路径，搜索、ripgrep 和 Git 快照都跳过它。工作区是家目录或磁盘根目录时会给出警告，不做自动 Git 快照（否则会暂存整个磁盘），也不把其中的 `.cove`、`.claude` 当作项目技能目录。会话、压缩归档和检查点含源代码/工具输出，**未加密**，请放在私有目录，不要提交或共享；Unix 新建目录限制为当前用户，Windows 继承父目录 ACL。凭据不写入状态，但工具输出若含敏感信息会进入会话。除 `--list-sessions` 外，启动仍要求模型及 API 凭据已配置。

`--list-sessions [all]` 不需要模型凭据，列出会话后退出。`--no-auto` 关闭后台记忆提取和自动 dream 整理。

存储使用版本化 JSON 快照加追加日志、完整压缩归档和文件租约，不引入数据库或事件溯源框架：

- `sessions/<id>.json` 始终是一份完整的会话快照（原子替换写入，单独读取它也能得到一个有效的会话）；
- 任务进行中的每次保存只向 `journals/<id>.log` 追加一条记录（自上次以来新增的消息和其余字段），写完即同步到磁盘，比每次重写整个文件快得多；
- 任务结束、退出、历史被改写（压缩、替换）或日志过长（8 MiB）时写一份新快照并清空日志；
- 读取时先读快照，再重放快照未包含的日志记录；重放是幂等的，崩溃时写了一半的最后一条记录会被忽略。

同一数据根中的同一工作区只允许一个 CLI 写者；使用不同数据根不能相互保护。执行意图先保存，结果先持久化再展示。重启后，未记录结果的在途调用按能确定的情况处理：没有副作用的调用（读取、搜索、提问、todo、网页读取）在继续时直接重新运行；文件写入/编辑在检查点记有意图、且文件内容正好是预期结果或原内容时，自动判定为已完成或未执行；其余调用标记为不确定，不自动重试，`/continue` 会列出它们的 ID 和两种处理方式：检查副作用后 `/resolve`，或者直接发一条新消息。发新消息时，旧批次里没运行的调用记为未执行，结果不明的记为不确定，什么都不重放，对话照常继续。`executed` 不会伪造原始输出、退出码或测试通过证据；文件调用还会对照检查点意图核验内容。遇到无法核对的外部改动，保守拒绝处理。

## 项目命令、诊断与命令行

| 命令 | 行为 |
| --- | --- |
| `/help`、`/help <命令>` | 按组列出全部命令（含各功能区的命令）/ 单个命令的用法；未知命令给出相近命令建议，参数不对时显示该命令用法，**都不发送给模型** |
| `/init`、`/init apply`、`/init discard` | 一次有界模型调用（最多 3 分钟，计费，预算用尽时跳过）阅读仓库摘要（两级目录、README 与清单文件开头、项目概要），起草 `CLAUDE.md`；仓库采用 `AGENTS.md` 约定时起草 `AGENTS.md`。模型不可用、失败或 Ctrl+C 时用模板。草稿存于 `<数据目录>/projects/<hash>/init-draft.md`，以 diff 展示；`apply` 经授权后走工作区检查点写入（`/diff`、`/undo` 可见可退），**从不覆盖已有文件**；`discard` 删除草稿 |
| `/diff git`、`/diff git --stat` | 分两节显示 `git diff --cached` 与 `git diff`；`--stat` 列出已暂存、未暂存和未跟踪文件，用于提交前审阅。`/diff` 仍是当前任务的文件工具变更 |
| `/commit [--preview] [--all \| --only <路径>... --] [消息]` | 默认只提交**已暂存**内容；`--all`/`--only` 通过从 HEAD 建立的临时 `GIT_INDEX_FILE` 选择文件，不改动你原来的暂存区（提交后只同步已提交文件）。先显示 stat、执行 `git diff --cached --check`，default 模式下请求批准，再运行验证命令（`done_verify_commands` 或自动检测）；验证前后检查暂存树哈希和工作区内容均未改变，否则不提交。默认消息 `auto-commit`；**从不推送** |
| `/cd <路径>` | 保存会话后以新目录重启本程序（工作区、租约、检查点、工具和项目上下文都绑定 cwd，重启是最安全的切换方式）；原会话留在原目录，可在那里 `--resume` |
| `/restart` | 保存后用原参数加 `--resume <id>` 重新启动本程序（Unix 为 exec，Windows 启动子进程并等待其退出码） |
| `/expand [n] [all]` | 工具结果在终端只显示摘要（交互控制台）或前 12 行（`-p`、管道），并带 `#n` 标记；本命令显示第 n 个（默认最近一个）的完整输出，默认最多 200 行，`all` 显示全部。内存中只保留最近 50 个 |
| `/clear` | 清屏并清除回滚缓冲区；输出不是终端时拒绝 |
| `/doctor [quick\|errors\|trace [N]\|codes]` | 环境诊断、错误记录与交互轨迹，见下文 |

诊断 `/doctor` 检查：配置文件能否加载及警告、占位或掩码的 API Key（`sk-xxxx`、`****`）、API Key 是否存在（只显示首尾各 4 位）、provider/模型/base_url、网络可达性（对 base_url 发 HEAD，任何 HTTP 响应都算可达；`quick` 跳过）、git 与 ripgrep、shell（Windows `powershell.exe`，其他 `sh`）、数据目录可写与权限、`policies.json` 与扩展（Hooks/MCP/插件）配置加载错误，以及最近的错误记录。失败项带诊断码和处置建议；`/doctor codes` 列出全部诊断码。命令行 `--doctor` 在**没有可用配置或 API Key 时也能运行**（这正是它要诊断的情况），有失败项时退出码为 1。

- **错误日志** `<数据目录>/errors.log`：模型调用失败、任务中断原因、命令错误和启动失败各记一行 JSON（时间、来源、诊断码、最多 400 字节的消息，API Key 被替换为 `****`）；超过 1 MiB 轮换为 `errors.log.1`。`/doctor errors` 按诊断码和消息分组显示次数、最近时间和建议。
- **交互轨迹** `<数据目录>/trace.jsonl`：每次模型调用、工具调用、上下文压缩和任务结束各记一行，只含大小、数量、耗时和结果（如消息数、估算 token、输入/输出 token、工具调用数、结果字节数、失败类型），**从不记录消息正文**；超过 4 MiB 轮换一次为 `trace.jsonl.1`。`/doctor trace [N]` 显示最近 N 条（默认 30）。实现为 Provider 装饰器和 `TurnHook`，不改执行内核流程。

命令行：

- `-p` 会把**管道传入的 stdin** 附加到提示词后（`cat app.log | cove -p "解释"`，也可只用管道 `cove -p < prompt.txt`）：最多 8 MiB，去掉 PowerShell 写入的 UTF-8 BOM，截断时不切断多字节字符并给出警告。stdin 是普通文件或 stderr 是终端时一直等待数据；否则（IDE、CI、其他 Agent 启动且从不写入的 stdin）最多等 3 秒首字节，超时警告并忽略。
- `--file <路径>`（可重复）把不超过 256 KiB 的 UTF-8 文本文件作为数据附加到 `-p` 提示词；`--image <路径>`（可重复）附加 PNG/JPEG/GIF/WebP 图片，处理方式与 `/attach` 相同（缩放到 ≤1568 px、≤5 MB）。
- `-r` 是 `--resume` 的简写；`--dump-system-prompt` 打印本次调用的完整系统提示词后退出。
- 退出码：`0` 成功，`1` 失败，`2` 参数错误（含 `-p` 缺少提示词），`130` 被 Ctrl+C 中断。

### 历史

- 标题取第一条真实用户请求（跳过系统通知和“继续”这类短输入），最多 60 个字符，保存在会话文件里，压缩后也不丢失。
- **接着上次做**：启动时如果当前工作区有值得继续的任务（优先未完成的，其次最近有实际内容的），会提示“上次在这里的任务：#n 标题（轮数，状态，多久之前）。输入 继续 或 /continue 接着做”。在新会话里输入 `继续`（或 `continue`、`接着做`、`/continue`）即恢复该任务：未完成的直接续跑，已完成的带着完整上下文继续对话。新会话的第一条消息与某个**未完成**任务的原始请求相同时，也会续跑那个任务，而不是重新开始。
- **恢复后能看到之前的对话**：`/resume`、`--resume`、列表选择、`继续` 恢复会话后，屏幕上会重新显示最近 3 轮对话（你的请求、模型回复、调用了哪些工具及结果首行）；模型拿到的是完整上下文。更早的内容用 `/resume detail` 查看。
- 启动信息显示版本、模型与供应商、权限模式、工作目录、git 分支和会话 ID 前 8 位；退出时提示下次如何继续（输入 `继续`，或 `cove --resume <id>`）。
- `/resume` 默认只列当前工作区，显示更新时间（UTC）、状态、对话轮数、标题和 ID 前 8 位；`/resume all` 列出的其他工作区会话，选中后自动在那个目录重启并恢复。
- `max_sessions`（配置根键，默认 200，≤0 关闭）：启动时按更新时间保留最新的会话，至多每 10 分钟清理一次，从不删除当前会话或其他进程正在使用的会话；同时清除没有对应会话、超过 1 小时的归档、文件检查点和锁文件。有会话文件无法读取时不清理检查点。
- `/resume clean` 把无法读取的会话文件移到 `<数据目录>/backups/clean-<时间>/`（不删除），并为缺标题的旧会话补标题（改写前先备份原文件）。正在被其他进程使用的会话跳过。
- `/resume export [file]` 导出 Markdown：会话信息、每条消息、工具调用参数和（截取 8 KiB 的）工具结果。默认写到 `<数据目录>/exports/<id>.md`；指定的相对路径按工作区解析，写入需授权。

### 费用账本

- 每次模型调用（任务、压缩、子 Agent、后台记忆提取、dream）都在 Provider 边界由装饰器记一行 JSON 到 `<数据目录>/usage/YYYY-MM.jsonl`：时间、供应商、模型、会话、来源、输入/输出 token、缓存读写 token（供应商报告时）和估算费用。按 UTC 月份分文件，保留 13 个月。
- `/status cost` 默认统计本月；也可用 `YYYY-MM`、`30d`、`24h`、`today`、`all`。价格未知的调用显示为 `unknown`，合计里标成 `>=`，不当作零。`export [file]` 写 CSV，默认 `<数据目录>/usage/export-<时段>.csv`。
- 估算方法与会话预算一致（报告的 input/output token × 配置单价），不是供应商账单。

### Git 快照与 `/undo`

- 每个会产生副作用的工具调用（`write`、`edit`、非只读 Shell、写模式子 Agent、`execute_plan`）执行前，把整个工作区快照到私有的 Git 仓库 `<数据目录>/checkpoints/git`：每个项目一个 ref、一个独立 index，不碰项目自己的 `.git`；工作区没变化时不新增快照。遵守项目 `.gitignore`，并排除 `.git`、`target/`、`node_modules/`、`dist/`、`build/`、`.venv`、`__pycache__` 等。只读权限模式下不做快照。需要本机安装 `git`；没有时只剩文件工具回退。
- 每个项目保留最新 50 个快照，超过 60 个时修剪，后台 `git gc` 只回收 1 小时前的对象。
- 两种回退：`/undo confirm` 只回退本任务文件工具的修改，**不含 Shell 改动**；Git 快照还原**包括 Shell 改动**（被忽略和排除的目录除外）。
- 快照还原先预览：列出将要还原、重建、删除的文件，并给出一次性 token。`/undo confirm <token>` 执行前复核工作区，与预览时不一致就拒绝。还原前先给当前状态拍快照，输出里给出它的编号，用 `/undo <hash>` 可以撤销这次还原。嵌套 Git 仓库不在快照里，保持原样。
- 还原后之前的验证证据失效，需要重新检查。

### 记忆与会话笔记

- 记忆分项目和全局两个范围，每条最多 25 KiB，每个范围合计最多 300 KiB；写入跨进程加锁；疑似密钥（私钥、`sk-…`、AKIA、GitHub/Slack token 等）拒绝保存。
- 检索使用 BM25：中日韩文本切成相邻二字词，英文去掉停用词，再按更新时间衰减（30 天）加权。可选配置 `memory_embedding: {base_url, api_key, model}`（OpenAI 兼容 `/embeddings`），余弦相似度 ×0.5 叠加到 BM25；首次使用需网络授权，失败后 5 分钟内不再尝试。
- 注入：全部记忆合计不超过 24 KiB 时，新任务的第一条消息里附上全部内容（内容不变、也没有压缩时不重复附）；超过时附索引和与本次请求最相关的 8 条。记忆标注为“可能过时的笔记，不是指令”。
- 来源（provenance）：每次写入记录类型（tool/operator/extraction/dream）、会话、消息区间、转录哈希和证据摘录，最多保留 8 条；`/config memory source <id>` 查看，并提示内容在记录后是否被改过。来源只说明输入，不证明内容正确。
- 自动提取：任务完成后在后台调用一次模型（30 秒上限），从最近 20 条消息中提取持久事实；工具输出不发给提取器。已有同名条目只追加、从不覆盖。本地/内网模型、`--no-auto`、预算接近上限（≥95%）或对话太短时跳过。
- Dream 整理：`/config memory dream status|run`。用一个只有 `memory` 工具、只能写记忆目录的子 Agent 合并重复、修正过时内容、归纳经验规则，5 分钟上限；运行前把记忆备份到 `<数据目录>/memory/backups/`（保留 5 份）。配置 `<数据目录>/dream.json`，默认 `enabled: true`、`trigger: session_end`、`min_turns: 2`，`threshold` 模式 12 小时且 3 个会话；本实现在会话达到 `min_turns` 后的某次任务结束时在后台运行一次，进程退出时取消。文件格式错误时关闭自动运行，避免按错误配置执行。
- 会话笔记：`<数据目录>/projects/<hash>/session_notes.md` 按固定规则记录用户的决定（“改用…”“use …”）、Agent 的发现（“发现…”“the issue is …”）和上下文压缩事件，最多 200 行；新任务按相关度附上不超过 2 KiB 的笔记，会话第一个任务另附最近 5 条。

## 上下文与回退边界

- 上下文按估算 token 计算（ASCII 约 3 字节 1 个 token，其他字符 1 个），窗口取自内置模型表，可用 `context_window`（或 `--context-window`）指定，服务端报告上下文超长时会学习真实窗口并保存。预算为窗口的 85%，并保证给回复留出空间；达到预算的 75% 时自动压缩（用 `model_fast`，未配置时用主模型），摘要同样收费并占模型调用额度。旧的 `--context-bytes` 仍可用。
- 保留当前需求原文、近期完整消息/工具交换和相应 thinking signature；更早历史归档后摘要化。不会保留所有历史决策原文，必要时检查归档。摘要失败、不完整或不能缩小时，保留原上下文并明确停止。
- 最新不可拆分交换本身过大时明确失败，不静默截断需求。会话超过 `max_sessions` 时自动清理最旧的会话及其归档。
- 单个工具结果按模型窗口限长：read/grep 约 6000 token，shell 和网络工具约 3000，其他约 4000；窗口大于 128K 时按比例放大（最多 4 倍，且不超过窗口的 1/8）。被截断的 read 结果末尾给出正确的 `[next: offset=N]`。
- 旧工具输出外置：最近约 50K token（随窗口放大）之外的旧工具输出累计超过 30K token 时，移到数据目录的 `tool-outputs/`，历史中只留一行占位说明文件位置（可用 Shell 查看）；`question`、`todo` 等计划类结果不外置。重复出现的相同结果只保留最新一份，较早的改成指向它的说明。外置文件 7 天后自动清理。
- 工具访问到工作区子目录时，会把该目录及其上级（最多 5 层，不超过工作区根）的 `AGENTS.md`、`CLAUDE.md`、`.cursorrules`、`.cove.md` 附在结果后，标注 `[Context from <路径>]`；每个文件只附一次（压缩后的新任务重新附），单文件 12 KB、单次 24 KB 封顶。Shell 命令中的路径也会识别。
- 每个新任务建立一个检查点，最多跟踪 100 个文本文件；重复编辑保留本轮首次修改前的原件。回退前检查全部文件，再逐文件复核并记录进度。
- `/undo confirm` 只还原 **文件工具**产生的变化，保留已有未提交改动和用户后续修改，**不回滚 Shell、Git、网络或外部进程副作用**；工作区内的 Shell 文件改动可用 Git 快照还原（见上文），工作区外的副作用和网络请求都无法撤销。回退后原验证证据失效，需要重新检查。
- 外部进程并发改文件不受锁控制；预检不能保证整批原子事务。回退过程中发生漂移或崩溃时保留明确记录，可能需要人工核对，不盲目覆盖。

## 权限与限额

| 参数 | 行为 |
| --- | --- |
| `--permission default` | 默认（旧名 `ask`）；`read`/`grep`/`glob`/`repo_map` 不需要审批，只读 Shell 命令（`ls`、`git status`、`rg`、只打印的 `sed -n`、`timeout` 包裹的只读命令、`strings`、`cygpath` 等）直接运行，本地 git 操作（`git add`、`git commit`、`git tag`，可与只读命令串联；`-f`/`--force`、`-d`、`-p`、`$(...)`、写文件的重定向除外）也直接运行；写文件、`git push` 等其他 Shell、网络访问和扩展进程逐次审批，没有终端则明确失败。`auto` 模式同样免审本地 git 操作 |
| `--permission plan` | 只读（旧名 `readonly`）；允许阅读、搜索和只读 Shell 命令，其他操作拒绝，模型收到拒绝说明后可以继续 |
| `--permission auto` | 另外自动允许构建/测试命令（`cargo test`、`go vet`、`npm run build` 等）、项目内文件修改、网络工具和已信任的扩展进程；其余 Shell 仍需审批，无终端时拒绝 |
| `--permission bypass` | 除拒绝规则和灾难命令外全部允许；只用于可信任务 |
| `--max-turns 20` | 每个任务/继续窗口最多 20 次模型请求，含自动压缩，不是用户对话数 |
| `--timeout 300` | 任务总时限，秒；达到后取消并有界清理 |
| `--max-output-tokens 4096` | 每次模型响应的输出 token 上限 |
| `--budget <USD>` | 估算预算，必须同时指定两类 token 价格 |
| `--input-price` / `--output-price` | 每百万 token 的美元价格 |

`-p` 永不读取审批输入，也不会注册提问工具；不要通过管道传入 `y` 来绕过审批。非交互运行中需要审批的操作会报错并停止，错误里写明可选的 `--permission auto`、`bypass` 或允许规则；需要无交互执行时必须显式选择。

预算按会话累计，在请求前及响应后检查，进行中的请求可能超额。计费使用供应商报告的 input/output token 和用户提供的单价，不精确建模缓存、分层或特殊推理计费，不是供应商账单。每次请求发送前先落盘“费用未知”，收到 usage 后再更新，避免进程崩溃漏记可能收费的请求。缺少 usage 或请求失败/断流时，有预算的会话不能继续自动执行；无预算仍可显式继续，不能把费用未知当作零。

文件工具限制在解析后的工作区内，拒绝父目录穿越、外部链接目标、直接访问 Git 元数据和 Windows ADS；这不是对恶意并发文件系统变化的沙箱。Shell 具有宿主权限，可能修改工作区外的数据、访问网络或启动程序。授权前务必检查命令。

### 权限模式与规则

判定顺序：灾难命令拦截 → 拒绝规则 → plan 模式（只放行只读操作）→ bypass → 允许/询问规则（按优先级，同级时询问优先）→ 模式默认。

- **命令分类**：Shell 命令先按实际 Shell 语法切分（Windows 上按 PowerShell，其他平台按 sh），处理引号、转义、管道、`&&`/`||`/`;`、重定向和 here-doc，再逐条判断类别：只读、构建/测试、Git、安装、未知、危险，整行取最高风险。命令替换 `$(...)`、反引号、PowerShell 脚本块 `{...}`、调用运算符 `&`、写文件的重定向、隐藏字符等无法静态判断的写法一律算“未知”，需要审批。
- **交互中切换**：`/mode` 显示当前模式、审阅流程和规则；`/mode default|plan|auto|bypass` 立即生效，不需要重启；加 `save` 写入配置文件（`permission_mode`）。
- **policies.json**：放在数据目录（`<data_dir>/policies.json`），是一个规则数组：

```json
[
  {"id": "no-push", "tool_pattern": "shell", "action": "deny", "command_prefix": "git push"},
  {"id": "make", "tool_pattern": "shell", "action": "allow", "command_prefix": "make", "scope": "D:\\project"},
  {"id": "git", "action": "allow", "command_group": "git", "priority": 1},
  {"id": "lock", "tool_pattern": "write", "action": "deny", "param_match": {"path": "*.lock"}}
]
```

  `tool_pattern` 是工具名 glob：`shell`（也认 `bash`、`powershell`）、`write`/`edit`、`web_fetch`/`web_search`、`mcp`/`hook`。`command_prefix` 按词匹配；`command_group` 是常规操作组（`git` 含 add/commit/push/pull/switch 等但不含 `--force`、reset、clean、checkout；另有 `cargo`、`npm`、`go`、`dotnet`）；`param_match` 按字段 glob（`command`、`path`、`url`）；`input_equals` 要求字段完全相等；`scope` 限定项目根目录；`enabled: false` 停用。允许规则必须覆盖整行的每一条命令才生效，拒绝规则只要命中其中一条（含 `bash -c` 内层文本）就生效。
- **记住本项目 / 本次会话**：选 `a` 会向 policies.json 追加一条限定本项目的允许规则，选 `s` 生成同样的规则但只保存在本进程内存里（Git 等常规操作记成命令组，其他记成“程序 + 子命令”前缀）。
- 拒绝规则和灾难命令被拦截时，模型收到拒绝说明并继续；你在审批里拒绝时同样如此（见“交互控制台”）。需要询问但没有终端时（如 `-p`），任务停止并保留状态，换模式后可 `/continue`。

### 安全拦截

- 任何模式（包括 bypass）都拒绝：对根目录、盘符、家目录或系统目录的递归删除（含 `rm -rf $UNSET/` 这类变量为空时的情况）、`--no-preserve-root`、对系统目录 chmod/chown、写裸磁盘（`dd of=/dev/sda`、重定向到磁盘设备）、mkfs/format/diskpart、关机重启、fork 炸弹、下载内容直接执行（`curl … | sh`、`irm … | iex`、`iex (irm …)`、`bash -c "$(curl …)"`、下载后立即运行同一文件）、`base64 -d | bash`、PowerShell `-EncodedCommand`、嵌套超过 8 层的 `sh -c`，以及藏在 `bash -c`/`cmd /c`/`eval`/`iex`/git 别名/xargs/`find -exec`/here-doc 里的同类命令。命令同时按 sh 和 PowerShell 两种读法检查，零宽字符会先去掉再检查。
- 只警告不拦截：`git push --force`、`git reset --hard`、`git clean -fdx`、`git checkout -- .`、递归删除当前目录内容。警告会显示在审批提示里，并附在工具结果后提醒模型。
- 用户输入中疑似密钥（`sk-…`、`AKIA…`、`ghp_…`、`github_pat_…` 等）只提示风险，内容照常发送；提示里的密钥已打码。
- 这些规则是防误操作的护栏，不是沙箱：Shell 仍以宿主权限运行。

### 计划模式与审阅流程

- 交互会话中模型可以调用 `plan_mode {reason}` 进入计划模式，只能做只读操作；调研完成后调用 `exit_plan_mode {summary, files[], decisions[], checks[]}` 提交计划，用户输入 `y` 批准后恢复原模式，输入其他文字则作为修改意见退回。计划保存在 `<data_dir>/plans/<会话>.json`。
- 用户用 `/mode plan` 或 `--permission plan` 选的计划模式，模型无法自行退出，只能由用户 `/mode default` 等解除。
- 计划模式下的写入、非只读 Shell、写模式子 Agent 等调用会被拒绝并提示模型先提交计划，任务不会因此中断。
- `/mode review`：之后每个任务都先进入计划模式，计划经批准才能修改；`/mode once`：只对下一个任务这样做；`/mode direct` 关闭。也可写入配置 `workflow`。计划列出的文件在提交时做快照，用户批准前这些文件若有变化，批准作废，需要重新提交。审阅流程需要交互终端。

### 完成闸门

- **重复失败熔断**：同一工具 30 秒内失败 3 次警告、5 次阻止；同一调用（同参数）失败 2 次警告、5 次阻止；同一工具连续失败 3 次警告、8 次阻止；读类工具连续返回相同结果 2 次警告、5 次阻止。
- **循环检测**：同一修改类调用在最近 14 次里出现 10 次、同一工具换参数在最近 12 次里出现 10 次（输出或目录在变化时不算）、同一输出在最近 40 次结果里出现 8 次，都会拒绝该调用并给出换思路的提示；第 2 次检测到时在交互会话里询问是否继续，第 5 次停止工具调用，让模型直接总结。60 次调用没有读写文件只提醒。读类工具不参与循环检测。
- **收尾催促**（临近调用次数或预算上限时跳过）：空回复（最多 2 次）；最新 todo 清单仍有未完成项（1 次）；改过文件后的完成自检（`done_check`，1 次）。“宣布了下一步却没做”（中英文启发式）和“做了事却只回一句话”（共最多 2 次）只在交互会话启用。
- **验证门禁补充**：自动推断的检查命令在项目受信任或 `auto`/`bypass` 模式下直接运行，否则照常审批（项目信任由项目设置提供，当前默认不信任）。模型在最后一次修改之后自己跑通过的同一条命令直接记为通过，不再重复运行。检查超时记为“未验证”，不当作失败打回。每条检查结果追加到 `<data_dir>/verify_ledger.jsonl`。
- **提交前验证**：开启门禁时，模型通过 Shell 执行 `git commit` 前会先运行检查，并核对暂存内容与工作区一致、检查期间暂存区没变；要求先单独 `git add`，再单独 `git commit -m …`（不支持 `-a` 等改变暂存范围的参数）。不通过则不提交，结果交回模型。每份暂存内容（按 `git write-tree` 的树哈希）只检查一次：通过后，同样的内容再提交不重跑检查；失败或你拒绝了检查，同样的内容再提交时直接说明“已经检查过且未通过”，不再重跑也不再询问，并提示模型改动内容或向你说明情况，而不是反复重试。系统提示也告诉模型提交要单独一次调用，避免被门禁退回。
- **自审**：`done_self_review` 开启时，收尾前一个只读子 Agent 阅读本轮 diff，回答 `NO_ISSUES` 或列出问题；问题交回模型处理一次。自审的调用和费用计入当前会话。
- **验收报告**：每个任务结束后写 `<data_dir>/acceptance/<会话>.json`：运行过的检查及证据、验证结论、Git 状态（HEAD、未提交文件数、未推送提交数、本任务里的 commit/push 是否成功）。`/status acceptance` 显示最近一次。报告只记录发生了什么，不代表模型声称完成。

## 文件、输出与失败语义

- **read**：返回 `File: 路径 (N lines total)` 加带行号的内容，默认从 `offset`（1 起）读 2000 行，`limit` 可改；旧参数 `start_line`/`lines` 仍可用。单行超过 2000 字符截断并标注 `…[+N chars]`。还有剩余时末尾给出 `[next: offset=X]`。传入目录时列出目录项，子目录以 `/` 结尾。文件最大 32 MiB。
- **编码**：含 NUL 的二进制文件和 UTF-16 文件拒绝读取（UTF-16 会给出转换建议）；非 UTF-8 文件如果能按 GBK/GB18030 解码且能逐字节还原，就解码显示，并注明编码；其他旧编码照原样显示并提示乱码，`edit`/`write` 会拒绝这类文件。图片文件暂不读取。
- **write**：自动创建缺失的父目录（目录本身不随 `/undo` 删除）。覆盖已有文件前，本会话必须读过它的当前版本（按大小和 SHA-256 判断，部分读取也算；本工具自己写过的文件自动记为已读；只读子 Agent 读过也算）；文件之后被用户或命令改过，要先重新读。`overwrite` 参数仍接受，但已不需要。覆盖时保留原文件的 CRLF、UTF-8 BOM 和 GBK 编码；内容中有 GBK 无法表示的字符时拒绝写入。参数名也接受 `file_path`/`filePath`、`text`。
- **edit**：`old` 必须唯一匹配，多处匹配时报告所在行号，可用 `replace_all=true` 全部替换。CRLF 文件中，`old`/`new` 的换行自动转成 CRLF；GBK 文件编辑后仍写回 GBK。没有精确匹配时，按“忽略空白差异”逐行比较：只有唯一匹配才替换，并把 `new` 的缩进换成文件的缩进单位；缩进对应关系不一致（混用 Tab/空格或层级不齐）时拒绝多行替换；超过 20000 行的文件不做这种匹配。仍找不到时给出最相近的候选位置和相似度。成功结果显示最多 3 处改动及前后各 2 行。`edit` 不要求先读，但修改都经过检查点，可以 `/undo`。
- 文件改动上限 1 MiB（非 UTF-8 内容 4 MiB，例如 `draw_image`/`browser` 截图写出的 PNG）；`write`/`edit` 都经过检查点日志和漂移检查。
- 临时文件原子替换不是与外部编辑器的原子 CAS；检查点也不是宿主沙箱，建议先在干净分支或临时副本上试用。
- **搜索**遵守 ignore 文件（没有 Git 仓库时也遵守 `.gitignore`），不跟随目录链接，不检索 Git 元数据。`grep` 有 ripgrep 时调用 `rg`，否则用内置搜索，结果格式相同（`路径:行号:内容`，上下文行用 `-`，组之间用 `--`）；参数 `include`（文件名 glob）、`path`（文件或目录）、`ignore_case`、`context`（0–10）、`files_only`；最多 100 行，超出时提示“at least N more”。`glob` 的模式不含 `/` 时匹配文件名，含 `/` 时匹配相对 `path` 的路径，`**` 可跨目录；最多 200 条，超出时提示 `... and N more`。路径一律相对工作区、用 `/` 分隔。
- **Shell 解释器**：Windows 上优先使用 Git for Windows 的 bash（按 git 所在目录和 Program Files 查找，跳过 WSL 的 bash 启动器），没有时用 PowerShell；其他平台用 bash，没有时用 sh。可用配置 `shell` 或环境变量 `COVE_SHELL` 指定。工具描述和系统提示中的环境行都写明实际解释器。Hooks 和验证门禁命令仍按原方言运行（Windows 上为 PowerShell）。两种解释器都在进程加入 Job Object 之后才从 stdin 收到脚本。
- **非交互环境**：命令运行时设置 `GIT_TERMINAL_PROMPT=0`、`GIT_EDITOR=true`、`GIT_PAGER=cat`、`PAGER=cat`、`NO_COLOR=1`、`TERM=dumb` 等，不会等待编辑器、分页器或凭据输入；通过 `GIT_CONFIG_*` 关闭仓库配置的 `core.fsmonitor` 并设 `safe.bareRepository=explicit`（保留用户已有的 `GIT_CONFIG_COUNT` 条目）。
- **Shell 时限**：`timeout_seconds` 默认 120 秒，最多 600 秒（也接受毫秒单位的 `timeout`）；可选 `description` 说明命令用途。
- **输出截取**：stdout 最多保留约 30 KB，stderr 约 10 KB，超出时保留开头 40% 和结尾 60%（在行边界切开），中间注明省略的字节数；管道始终排空，避免堵塞。Windows 上非 UTF-8 的输出行（控制台代码页 GBK）按 GBK 解码；仍无法解码时标记 `encoding_warning`，不假装无损。
- Shell 结果包含退出码、stdout、stderr、超时和截断标记。运行中逐行实时显示，每个流最多实时显示 256 KiB，超出部分只保留在结果里。这是按行刷新的输出，不是 TUI 面板，进度条这类原地刷新的输出会显示成多行。
- 写入或命令已经产生的副作用不会因取消或网络错误自动撤销。
- 流式响应缺终止事件、参数不完整或输出截断时，不执行该响应内的工具；不自动重试并重放副作用。
- 工具失败会作为错误结果交给模型；任务若出现过失败工具，最终退出码保守地保持非零，即使模型声称已恢复。用户拒绝的调用不算失败。
- 模型正常结束不等于通过独立验收；没有实际测试证据时不能声称验证通过。
- **验证门禁的结果**写在任务结束的汇总行里（交互控制台是 `✓ 完成 · …`，`-p` 和管道是 `[summary] …`）：`验证通过`、`验证未通过`、`验证未运行`（审批被拒、plan 模式下无法运行，或检查超时），或 `无自动验证`（无检查命令，或本轮没有文件改动；交互控制台不显示这一项）。3 轮后仍失败，退出码为非零。门禁只看文件工具产生的改动；单纯通过 Shell 修改文件不会触发它。没有运行就不算通过。
- **网络工具**：`web_fetch` 只访问 https（http 自动升级）。每次请求和每次重定向前，都会解析目标地址，拒绝本机、私网、链路本地和 CGNAT 地址，并固定解析结果，防止 DNS rebinding；TUN 模式（“fake-IP”）代理返回的 `198.18.0.0/15`、`2001:db8::/32` 地址视为公网，因为局域网不会用到它们。不走系统代理。响应正文最多读 2 MiB，每次返回 32 KiB 文本，可以用 `offset` 翻页。网络访问被拒绝时，工具返回失败结果，模型可以不联网继续，任务不会被中断。搜索词会发送给第三方服务。

## 子 Agent、计划、Worktree 与回归验证

### 多个子任务怎么分发

| 方式 | 适合 | 并行 | 改动落在哪里 |
| --- | --- | --- | --- |
| `execute_plan`（见下文“计划执行”） | 一个需求拆成几块，有先后依赖 | 按依赖图并行，最多 `max_agents` 个（1–8，默认 4） | 主工作区，进入当前任务的检查点（`/diff`、`/undo`） |
| 后台 Agent `agent {background: true}`（见下文“后台 Agent 与生命周期”） | 彼此独立、耗时长、想各自出一个分支的任务 | 同时最多 `max_background_agents` 个（1–16，默认 4），不阻塞主任务 | 各自的 git worktree 和分支，由你 `/status agents merge` 合并 |
| `team_create`（实验工具，需 `experimental_tools: true`） | 一次建一组成员 | 复用计划执行器 | 同 `execute_plan` |

模型在同一次回复里直接调用多个前台 `agent` 时，这些子 Agent **依次**运行，不并行；要并行，用上面的方式。不需要记工具名，直接说明意图即可，例如：

```text
把这个需求拆成几个独立子任务并行执行：A 改 dispatcher，B 改 margin，C 补测试（C 依赖 A 和 B）
用后台 Agent 分别在独立 worktree 里做这三个修复，做完告诉我
```

所有子 Agent 共用主会话的预算，审批规则与主任务相同（并行时审批排队，一次只问一个）；子 Agent 看不到主对话，不能再委派，也不能向你提问。

### 子 Agent 类型

`agent` 工具把一个自包含的子任务交给子 Agent。子 Agent 看不到父对话，只按 `prompt` 工作，不能提问，也不能再委派（没有 `agent`、`ask_user`、`memory`、`mcp`）。系统提示里带有和父任务相同的项目说明。每个子 Agent 最多调用模型 `subagent_max_iterations` 次（默认 60）。它的模型调用、token 和费用都记入父会话，并受剩余预算约束。

| 类型 | 能做什么 |
| --- | --- |
| `general`（默认） | 读写文件、运行 Shell、使用 todo/skill/网络工具，也能运行 `regression` |
| `explore` | 只读调查，可以联网 |
| `plan` | 只读，产出有序的实现计划，可以联网 |
| `review` | 只读代码审查，不能联网 |
| `test` | 编写和运行测试，工具与 general 相同 |
| `verify` | 只读、不能联网，只多一个 `regression` 工具。必须用它拿到“基线失败、当前通过”的证据，否则这次调用就是失败的工具结果（`exit: unverified`） |

- 旧参数 `mode=read`/`mode=write` 仍然可用，分别等于 `explore`/`general`。
- 自定义类型写在配置的 `agent_types` 里。只读类型只能列 read/glob/grep/repo_map、web 工具和 regression。
- 结果第一行是 `[sub-agent <id> (<类型>): <描述>] [exit: <原因>, steps: N, truncated: yes/no]`，最后一行是费用、步数和失败工具数。`exit` 的取值：`completed`、`max_iterations`、`budget`、`interrupted`、`approval_denied`、`unverified`、`error`。
- 可编辑类型的写入进入父任务的检查点，`/diff` 能看到、`/undo` 能回退，也会触发验证门禁。审批规则与父任务相同；多个子 Agent 并行时，审批提示会排队，一次只问一个。
- 子 Agent 内部的单次工具调用不进入父会话的调用日志。委派过程中崩溃时，整个 `agent` 调用标记为不确定，需要检查改动后 `/resolve`。Hooks 只对 `agent` 这一次调用触发。
- 子 Agent 运行时的动作会实时显示为带 id 前缀的行（如 `[agent-1] -> edit`、`[todo-2] [ok] shell: ...`）。

### 计划执行 `execute_plan`

`execute_plan` 读取本会话最近一次成功的 `todo` 调用，把其中 `pending` 的条目当作子 Agent 任务执行。

- **依赖**：用条目的 `id`/`depends` 声明，或者在内容前写前缀 `depends:todo-1,todo-2 标题`。依赖必须在本计划内，或者已经 `completed`；未知依赖和循环依赖直接报错。
- **调度**：按依赖图调度，依赖一完成就启动，不按层等待。`parallel=true` 时最多同时跑 `max_agents` 个（1–8，默认 4）；否则串行。
- **重试**：失败的条目带着失败原因重试一次。到达调用上限、预算耗尽或被取消的不重试，保留部分结果。依赖失败的条目会跳过，并逐级传递。
- **文件冲突**：并行条目通过文件写入认领表协调。同一个文件不会被两个正在运行的条目同时写：后来者等先认领的条目结束后再写；如果出现互相等待，后来者的写入直接失败，并说明冲突。Shell 命令改的文件不在认领范围内。
- **结果**：返回每个条目的结果、exit、尝试次数和步数。已由 execute_plan 完成的条目不会重复执行；模型应据此更新 todo。

### Worktree

- `worktree {branch}` 用 `git worktree add ../<branch> -b <branch>` 创建工作树。分支名会校验，拒绝 `..`、前导 `-` 等。
- `exit_worktree {branch, merge, discard}` 删除工作树：
  - `merge=true` 先把已提交的工作合并回主工作区，要求两边都干净。有冲突时中止合并并列出冲突文件，不会自动解决。
  - 有未提交改动时，必须传 `discard=true` 才会删除。
  - 分支保留，只有合并后才删除。
- 这两个工具都需要审批。
- **设计选择**：父 Agent 自己的工具不切换根目录。父任务的检查点、验证门禁、`/undo` 和项目上下文都对应主工作区，中途换根会把一个任务拆到两棵树上。所以在 worktree 里做事，要用 `agent(worktree=<branch>)` 运行一个根目录在该 worktree 的子 Agent。它有自己的检查点日志；`/undo` 不覆盖它，回退单位是分支。

### 回归验证 `regression`

`regression {files, runner, filter, package?, baseline?, timeout_seconds?}` 证明某个测试确实能抓住被修复的缺陷，需要审批，会运行项目测试代码（不是沙箱）。

1. 把列出的生产文件临时恢复成基线：默认是本任务检查点里改动前的内容，也可以用 `baseline="git:<rev>"`。
2. 运行所选测试，要求它真实失败。
3. 恢复当前内容，再运行同一组测试，要求通过。

- `runner=cargo` 运行 `cargo test <filter>`，按 libtest 的 `test … ok/FAILED` 行判断。
- `runner=go` 运行 `go test -json -run <filter> <package>`。
- `runner=command` 运行配置里的 `regression_command`。这种方式只看退出码，无法区分编译失败，结果里会写明。
- 编译错误、跳过或不存在的测试、超时都记为 `unverified`，不算证据。测试本身必须写在没列出的文件里（如 `tests/`）。
- 临时还原前，先通过 Workspace 记下当前内容。还原期间，文件工具拒绝修改这些文件。进程崩溃后，`/resolve` 或 `/status agents repair` 会把仍是基线内容的文件恢复成当前内容；被别人改过的文件不覆盖，标记为不确定。
- 证据是 `RegressionEvidence` 结构（status/reason/runner/selector/files/baseline/tests/before/after），可以通过 `AgentHub::evidence()` 提供给验收报告。

### 后台 Agent 与生命周期

`agent {background: true}` 立即返回 Agent id，子 Agent 作为后台任务运行，不阻塞父任务。

- **隔离**：可编辑类型必须隔离（`isolation="worktree"`，默认）。每个 Agent 在 `<数据目录>/worktrees/<项目哈希>/<id>` 有自己的 worktree，分支为 `cove/agent-<id8>`，从当前 HEAD 创建。只读类型可以 `isolation="none"`，在主工作区只读运行，此时不提供 regression。
- **持久化**：每个后台 Agent 的会话（消息、待执行调用、费用）都按会话存储的规则落盘：原子快照，执行前先记日志，不确定的调用从不重放。状态记录在 `<数据目录>/agents/<项目哈希>/`。
- **状态机**：`running → paused / interrupted / finished / failed → discarded`，所有转换都经过类型化的状态机校验。进程重启后，原来 `running` 的 Agent 显示为 `interrupted`。
- **通知**：Agent 结束时，父 Agent 会收到带最终报告的通知：任务进行中，在下一次工具结果里送达；任务之间，在 REPL 里打印一行。没有改动的 worktree 会标为“worktree has no changes”。
- **费用**：后台 Agent 的费用经 Ledger 记入父会话，并占用共享预算。
- **审批**：后台 Agent 使用同一权限模式。default 模式下，审批请求排队，在任务之间到终端上逐个询问（REPL 空闲时会提示按 Enter 处理）；没有终端时直接拒绝，绝不静默批准。
- **单次模式**：`-p` 单次模式结束前会等后台 Agent 跑完，最长等 `--timeout`，超时后中断它们（可恢复）。REPL `/exit` 会中断仍在运行的后台 Agent。

用户命令统一放在 `/status agents` 下，`main` 指前台任务：

| 命令 | 行为 |
| --- | --- |
| `/status agents` | 列出 id、类型、状态、原因、模型、时长、费用、任务、最后一步、分支及其是否有未提交改动 |
| `stop <id>` | 中断：取消当前模型请求；进行中的工具调用按日志规则结束或标为不确定 |
| `park <id>` | 暂存：先中断，再把 worktree 里未提交的改动提交到其分支，并释放名额 |
| `park main` | 暂存前台会话（会话和检查点都保留，文件不回退），然后新建会话，可以开始别的任务 |
| `resume <id> [confirm] [消息]` | 从记录继续，带完整上下文，重启进程后同样可用。有结果不确定的调用时，必须先检查，再加 `confirm`，这些调用会记为失败、不重放。唤醒暂存的主任务时，会切回它的会话并 `/continue` |
| `send <id> <消息>` | 运行中的 Agent 在下一轮工具调用时收到消息；已停止的 Agent 带着消息恢复 |
| `merge <id>` | 只在用户明确要求时执行：要求主工作区干净；基线未变时快进合并，变了则生成合并提交；冲突会报告并中止。之后删除 worktree 和分支。`/undo` 不覆盖合并结果 |
| `discard <id>` | 删除 worktree 和分支 |

模型可以通过 `agent_control {action: list|stop|park|resume|send}` 做同样的操作，但不能合并或丢弃，只能请用户执行。

### 实验工具（`experimental_tools: true`）

- `task`、`task_list`、`task_get`、`task_output`、`task_update` 只在内存看板上记录状态，结果里会注明“只记录，不执行”。
- `task_stop` 对后台 Agent id 真正中断它；对看板条目只是标记取消。
- `team_create` 立即把成员当作并行子 Agent 执行，复用计划执行器；`team_delete` 删除看板记录。
- `send_message` 的接收方可以是：后台 Agent（排队或唤醒）、尚未执行的计划条目（加入其提示）或 `user`（打印到终端）。
- `brief` 汇总看板、子 Agent 活动和最新计划。
- `sleep` 等待 1–300 秒，可以取消。

## 外围工作流（/changes、/auto、/remote）

所有功能默认关闭，只在显式命令下运行。结果放在数据目录（`<data-dir>`），**除 `/changes apply … confirm` 外不写入项目工作区**。子进程（worker、验证命令、浏览器、git）都在同一套进程树管理下运行（Windows Job Object / Unix 进程组），取消或超时时整棵树回收。

### 命令一览

| 命令 | 作用 |
| --- | --- |
| `/changes` | 列出所有变更集：竞跑候选 `race:<run>:<a\|b>`、收件箱结果 `inbox:<id>`、当前任务 `turn` |
| `/changes show\|diff <id>` | 查看元数据（基线、补丁哈希/字节、检查结果、审阅状态）或补丁 |
| `/changes export <id> <out>` | 把补丁写到项目外的新文件，不覆盖已有文件 |
| `/changes apply <id>` | 只做预检；加 `confirm` 才真正应用 |
| `/changes review <id> accepted\|rejected` | 记录审阅决定，不应用 |
| `/changes race <spec.json>` | 前台运行双方案竞跑，Ctrl+C 取消 |
| `/changes verify-browser <workflow.json> [--allow-local]` / `results` | 浏览器实操验收 / 查看历史报告 |
| `/auto list\|add <spec.json>\|remove <id>\|run <id>\|tick\|event <name> <key>` | 自动化任务 |
| `/auto inbox [list\|show <id>\|patch <id> <out>\|review <id> accepted\|rejected]` | 收件箱 |
| `/remote start [--bind 127.0.0.1:0] [--token-file <路径>] [--allow-lan --tls-cert <pem> --tls-key <pem>]`、`status`、`stop` | 远程监督 |

### 统一变更集与应用

竞跑候选、收件箱结果和当前任务都用同一个模型表示：基线 `{root, commit, branch}`、补丁 SHA-256 与字节数、验证记录（只看退出码，按最后一次尝试判断）、审阅状态（pending/accepted/rejected/applied）。`apply … confirm` 依次检查：可应用、项目根/分支/提交与基线一致且工作区干净（无已跟踪改动、无未跟踪文件）、最后一次检查全部通过、补丁大小与哈希一致；再在项目级文件锁（最多等 10 秒）下执行 `git apply --check` 和 `git apply`。最后的 `git apply` 不响应取消，避免半途中断。应用后不提交、不合并、不推送，并在会话中记一条说明，之前的验证证据视为过期。当前任务的变更集 `turn` 只读，撤销用 `/undo`。

干净检查**允许被忽略的文件**（例如 `target/`、`node_modules/`）：补丁不含它们，且 `git apply` 不会覆盖已存在文件，所以不要求先清掉构建产物。

### 双方案竞跑

要求项目根是本地 Git 仓库、处于分支上且工作区干净。spec 建议放在项目外：

```json
{"prompts":["用方案 A 实现","用方案 B 实现"],"verify":[["cargo","test"]],"budget":2,"timeout":900}
```

（`budget`、`timeout` 也可写作别名 `total_budget_usd`、`total_timeout_seconds`。）两个候选各自在 `<data-dir>/race/<run>/wt-a|wt-b` 的分离 worktree（从 HEAD 创建，禁用 Git hooks）里运行子进程 `cove -p <prompt> --permission auto --max-turns 12`：私有临时数据目录与最小配置（只有 provider、模型、单价、预算份额；无 profile、扩展、MCP、Hooks 和自动验证门禁），API key 只通过子进程环境变量传递，不落盘。预算均分；不知道单价时不能设预算。随后在各自 worktree 运行同一组验证 argv（不经 Shell），清理被忽略文件后捕获二进制补丁（符号链接、子模块、`.git` 路径会使候选失败），报告与补丁存到 `<data-dir>/race/<run>/`，worktree 随后删除。总时限内约 2/3 留给生成。模型自评不能决定通过；只有通过验证的候选可以 `/changes apply race:<run>:a confirm`，一次运行只能应用一个候选。成本取子进程 `[summary]` 的估算，没有则为 unverified。

### 自动化任务与收件箱

spec 存在 `<data-dir>/automations/<项目键>/`，格式如下：

```json
{"version":1,"spec":{"id":"nightly","prompt":"修复一个失败的小测试","enabled":true,"every_seconds":3600,"event":"tests-failed","timeout_seconds":300,"budget_usd":1,"max_turns":8,"retries":1,"verify":[["cargo","test"]]}}
```

`add` 不启动任何东西。`run` 立即运行；`tick` 对到期任务各运行一次（错过的周期合并为一次），需由操作系统计划任务调用；`event <name> <key>` 运行监听该事件的任务，同一 key 只处理一次（失败或崩溃也不重放）。每次尝试使用从 HEAD 新建的分离 worktree（未提交和未跟踪文件不参与）、隔离配置、`auto` 模式、无 MCP，嵌套运行通过环境变量 `COVE_NESTED_WORKER` 拒绝。只有“已完成但失败”的尝试会重试（最多 2 次，预算按次均分）；取消和超时记为 interrupted，不重试。每个项目同时只运行一个任务（跨进程任务锁）；进程中途退出留下的 running 结果下次会标为 uncertain，审阅前阻止再次运行。结果连同基线、检查、补丁进入收件箱，**从不自动应用**；在分离 HEAD 上记录的结果只能导出。

供计划任务使用的独立入口（`--automation` 必须是第一个参数，provider 等取自默认配置和 `COVE_*` 环境变量）：

```text
cove --automation tick D:\project
cove --automation event D:\project tests-failed build-42
cove --automation add D:\project C:\Temp\nightly.json
cove --automation inbox D:\project list
```

### draw_image 与 browser 工具

- `draw_image {outputPath, width, height, shapes}`：白色画布，形状有 `fill`、`rect`（x,y,w,h）、`circle`（圆心 x,y 与 r）、`line`（x,y 到 x2,y2，`strokeWidth` ≤ 64），颜色 `#RRGGBB`、`#RRGGBBAA` 或 `rgba(r,g,b,a)`（a 为 0–1 或 0–255），支持半透明叠加。每边 ≤ 16384 px、总像素 ≤ 64M，PNG ≤ 4 MiB。只能写工作区内的 `.png`；已有文件不是 PNG 时拒绝覆盖。写入经过检查点，`/diff` 显示为二进制变化，`/undo` 可按字节恢复。
- `browser {action: navigate|screenshot, url, format: text|markdown|html, output}`：自动查找已安装的 Chrome/Edge（或配置 `browser_path`、环境变量 `COVE_BROWSER`），用精简的 CDP 客户端驱动无头模式，每次调用使用一次性 profile。页面、子框架和 worker 的**每个请求**都经 CDP Fetch 拦截检查：只放行公网地址，拒绝本机、私网、链路本地和 `file:`/`chrome:` 等协议，WebSocket 一律阻断，禁止下载。截图存到工作区（默认 `browser-screenshot.png`，经检查点）并返回路径。找不到浏览器时 `navigate` 退回 `web_fetch`，`screenshot` 报错。残余风险：地址检查与浏览器自身的 DNS 解析之间仍有 rebinding 窗口。
- 两个工具始终提供：固定的工具列表有利于供应商的提示缓存，比只在提到图片时才提供节省的约 300 token 更划算。

### 浏览器实操验收

```json
{"url":"http://127.0.0.1:3000","fixtures":{"reference":"fixture-42"},"steps":[{"action":"fill","selector":"#name","fixture":"reference"},{"action":"click","selector":"#submit"},{"action":"assert_text","selector":"#result","text":"Confirmed fixture-42"},{"action":"assert_visible","selector":"#result"}]}
```

支持 `navigate`、`click`、`fill`、`assert_text`、`assert_visible`、`assert_url`，至多 32 步且必须有断言；不接受 JavaScript，不填写密码框。桌面 1280×800 与移动 390×844 各在独立浏览器上下文中重放，报告 JSON 与 PNG 证据（尺寸、SHA-256）存在 `<data-dir>/browser-verification/<run>/`。断言不成立或元素不存在为 **fail**；安全拒绝、网络不可用、取消、无效工作流或找不到浏览器为 **unverified**，不会假绿。本地服务需操作员先启动，并显式加 `--allow-local`（只放开 loopback，不放开私网；在 default 模式下需审批）。报告不记录 fixture 值、选择器、URL 或页面文字，但截图可能包含页面内容。

### 远程监督

`/remote start` 默认监听随机的 loopback 端口，显示 URL 和凭据文件路径（不显示 token；Unix 上权限 0600，Windows 位于用户目录）。私网地址需要 `--allow-lan` 加 TLS 证书和私钥；通配和公网地址一律拒绝。所有端点要求 `Authorization: Bearer <token>`，严格校验 Host 与 Origin（防 DNS rebinding 和跨站请求），POST 必须是 JSON。

| 端点 | 说明 |
| --- | --- |
| `GET /v1/status` | 会话、项目、是否运行、是否暂停、排队的引导消息、待审批项 `{id, summary, expires_at}` |
| `POST /v1/actions` | `{"id":"<请求ID>","kind":"steer\|cancel\|pause\|resume\|approve\|deny","text":"…","approval_id":"…"}` |
| `GET /v1/actions/{id}` | 查询某个请求的结果 |

请求 ID 防重放：同 ID 同内容返回原结果，同 ID 不同内容拒绝。`steer` 的文字作为操作员指令附在下一个工具结果上（或在模型准备结束时续接）；`cancel` 取消当前任务（不撤销已发生的副作用）；`pause` 让任务停在下一次工具调用前，直到 `resume` 或取消；`approve`/`deny` 只回答当前这一个待审批提示，一次有效、5 分钟过期。远程和本地终端同时可答，先答者生效；过期后本地提示继续等待。远程批准不保存规则、不改变权限模式。`/remote stop` 与退出时撤回远程审批并删除凭据文件。

### 安全边界

Git worktree 不是操作系统、网络或进程沙箱：worker 和验证命令拥有宿主权限，Git 管理数据共享，只对可信项目、提示和命令使用。补丁、输出和截图可能含敏感信息，不要同步数据目录。预算是估算上限，进行中的请求可能超出。宿主崩溃可能遗留 worktree（位于数据目录下）或临时配置，需要人工清理。

## 尚未实现与已知限制

- 界面是滚动输出加常驻输入行的命令行控制台，不是全屏 TUI：输出按行追加，进度条这类原地刷新的输出会显示成多行；不支持鼠标操作。
- 剪贴板图片（Alt+V）只支持 Windows；其他平台可以粘贴或拖入图片路径。
- 不支持 MCP 的采样、交互 elicitation 和服务端后台任务（OAuth 登录见上文 `/mcp login`）；插件不加载原生动态库或可执行代码；`bedrock` 不支持 SigV4 签名（只支持 Bedrock API key）。
- 写入模式的子 Agent 和后台 Agent 使用与主任务相同的权限判定，但不经过主任务的计划模式工具和提交前验证包装。
- 后台调用（记忆提取、技能复盘、自审）按主模型单价估算费用。

自动化测试只用模拟模型协议和临时项目中的真实文件/进程操作，不调用真实供应商的付费 API，也未完成各模型的兼容认证。交互控制台的显示在 Windows 伪终端里按字符核对过颜色和排版；其他终端（特别是旧版 conhost 字体）对 `●`、`│`、`└` 等符号的显示可能不同，转圈动画在无法确认支持盲文点字符的终端里改用 `- \ | /`。

## 验证

[持久化测试](../tests/persistence.rs)覆盖会话租约、状态损坏、结果不确定时禁止重放、继续时保留失败、完整归档与压缩、累计计费、文件漂移和回退边界。[CLI 测试](../tests/cli.rs)另外覆盖管道 REPL、多轮历史、跨进程继续、恢复后的 diff/回退，以及未知命令与身份不匹配。测试使用隔离临时数据目录，不写入用户默认会话目录。

[项目命令测试](../tests/project.rs)覆盖分组帮助与未知命令建议、`-p` 管道 stdin 与 `--file`、退出码、`--dump-system-prompt`、无凭据的 `--doctor`、轨迹与错误日志不含正文、`/init` 起草与不覆盖写入、`/commit` 的暂存/`--all`/`--only` 与验证、`/expand`，以及 `/cd`、`/restart` 的重启。

[扩展集成测试](../tests/integrations.rs)覆盖官方 MCP SDK 对真实本地 stdio 服务的握手、分页、调用、资源、断线和进程树取消；Skills、隔离记忆、插件组合，以及 Hook 拒绝、中断和防重放。MCP 测试夹具需要 Python 3（Windows 为 `python`，其他平台为 `python3`）；运行时使用 MCP 本身不依赖 Python，只有所选服务需要其自身运行时。CI 显式准备 Python。

[Agent 测试](../tests/agents.rs)在临时 git 仓库中用模拟模型覆盖以下场景：并行 `execute_plan` 的依赖顺序和结果传递；没有回归证据的 verify 记为失败；`regression` 先在基线上失败、再在当前代码上通过，并恢复文件；worktree 内的子 Agent 及合并回主工作区；实验工具开关和自定义类型；后台 Agent 的完整流程（启动 → 进程退出被中断 → 新进程中显示 interrupted → 带上下文恢复 → 通知 → 合并）；父任务在下一轮工具调用时收到完成通知；主任务暂存（park）后在新进程中带消息唤醒。

```powershell
cargo fmt --check
cargo clippy --all-targets --locked -- -D warnings
cargo test --locked
cargo build --release --locked
```

[安全与闸门测试](../tests/safety.rs)用模拟 SSE 服务覆盖各权限模式、灾难命令拦截、拒绝规则、计划模式、熔断、复用模型自己的检查结果、验收报告、密钥提示和 `/mode save`。[核心测试](../tests/core.rs)覆盖权限、精确编辑、路径限制、搜索、费用阈值、进程输出/超时/取消及子进程树清理，以及审批被拒绝后理由交回模型、任务继续且不计为失败。[渲染单元测试](../src/app/render.rs)覆盖流式 Markdown 在任意分片下的样式（包括跨分片延续）、表格对齐、工具标题与结果摘要。[CLI 测试](../tests/cli.rs)用本地模拟 SSE 服务验证两类协议、真实编辑和命令检查、默认拒绝写入、断流以及任务 deadline。测试不需要真实 API key，不访问真实供应商。
