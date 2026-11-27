# cove 技术参考

本文是完整的功能与实现细节参考；日常使用请先看 [使用说明](../README.md)。

cove 是聚焦编码任务的终端 Agent，用 Rust 编写，项目名和可执行文件名为 **cove**。当前版本 **0.8.2**：单个可执行文件，常驻交互控制台加单次任务模式，用户命令集中在少量入口下。架构与设计原则见 [架构设计](design.md)，扩展配置见 [扩展使用说明](extensions.md)。

0.8.2 让界面说清楚当下在做什么，并减少了不必要的打扰：Alt+M 面板改名为任务面板，显示主任务的状态、用时和当前动作、本次请求、`todo` 计划进度，子代理只逐行列出还在进行的，已结束的合并成一行，状态和步骤改为中文，任务运行时状态行提示“Alt+M 查看任务”；改窗口宽度后状态行不再残留。默认权限模式改为 `bypass`（除拒绝规则和灾难命令外不询问），要审批就用 `--permission default`、`/mode default` 或配置 `permission_mode`。项目未信任、MCP 工具不在允许列表里时，这次调用作为失败结果交给模型，任务不再中断；`plugins/` 下没有清单的目录跳过并提示怎么消除提示。恢复会话时标出“这一轮没做完”，`/restart` 自动恢复后不再提示下次输入“继续”。测试统一用 `cargo test`（debug），收尾自动验证复用模型跑过的同一条命令。README 重写为更短的上手路径。

## 已实现

- OpenAI Chat Completions 兼容协议、Anthropic Messages 协议的 SSE 流式调用。
- 文件工具：`read` 读文件（默认 2000 行，可用 `offset`/`limit` 续读）或列目录，支持 GBK/GB18030；`write` 自动建父目录，覆盖前必须读过当前版本；`edit` 支持 `replace_all`、空白容错匹配和缩进映射；`grep`（有 ripgrep 时优先用 rg）和 `glob`（支持 `**`）。详见下文“文件、输出与失败语义”。
- Shell 自动选择解释器：Windows 上优先 Git Bash，没有时用 PowerShell；工具描述和系统提示的环境行会写明实际方言。命令明显是给另一种已安装的 Shell 写的（PowerShell cmdlet、cmd 的 `%VAR%`、bash 循环）时改用那个 Shell 运行，审批也按同一方言判断。
- `question` 交互式提问：一次可问多个问题，每个问题可带编号选项，按编号或自由文本回答（仅交互模式）；对带选项的问题用自己的话回答时，问卷就此结束，剩下的问题不会拿这段话去作答。
- `todo` 任务计划清单：多步任务中维护待办项，支持 `priority`、`cancelled`、`id` 和 `depends` 依赖，以及给 `execute_plan` 用的 `writes`（写集合）和 `estimate_minutes`（预估分钟数），见“计划执行”；同一时间至多一项进行中。
- 连续的只读工具调用（read/glob/grep/repo_map/web_fetch/web_search）并发执行（最多 8 个），结果顺序和日志顺序保持确定；写入和 Shell 仍逐个执行。
- `repo_map` 仓库结构索引：列出源文件中的类型、函数和方法（签名和行号），按跨文件引用做 PageRank 排序；可按路径片段或标识符查询，并带出调用方和被调用方所在的文件。支持 Rust、Go、Python、TS/JS、C#、Java。解析结果按文件的修改时间和大小缓存，再次查询只解析变化的文件，未命中缓存的文件在多个线程上并行解析。
- Shell 实时输出：命令运行时逐行显示 stdout 和 stderr（交互控制台里是 `│` 开头的暗色行，stderr 为红色；`-p` 和管道输出里分别以 `|`、`!` 开头），验证命令同样实时显示；结束后只再打印一行状态，不重复正文。
- 完成前验证门禁：本轮通过文件工具改过文件时，模型宣布完成后自动运行检查命令；失败输出回给模型继续修，最多 3 轮。
- `web_fetch` / `web_search`：抓取公网 https 页面（拒绝内网和本机地址），以及通过 Tavily、Brave 或 DuckDuckGo 搜索网页。
- 子 Agent：六种内置类型（general/explore/plan/review/test/verify）加配置里的自定义类型（所有子 Agent 都被要求最终答复先给结论、控制在约 400 词内，因为答复会留在主 Agent 的上下文里；超过 6 KB 的部分存到磁盘；`explore` 默认用快速模型，同一次回复里的多个只读子 Agent 并行）；`execute_plan` 按 todo 依赖图并行执行计划（`isolation: "overlay"` 时各条目在私有覆盖层里改，最后统一合并）；worktree 工具；回归验证工具；可持久化、可中断/暂存/唤醒的后台 Agent（各自独立的 git worktree）。详见下文“子 Agent、计划、Worktree 与回归验证”。
- 集中权限检查：`default`、`plan`、`auto`、`bypass` 四种模式（`ask`/`readonly` 仍可用作别名），交互中用 `/mode` 即时切换；按命令实际运行的 Shell 语法切分、给出“会改动什么”原因的风险判断；任何模式都拦截的灾难命令；`policies.json` 允许/拒绝/询问规则，审批时可“本项目总是允许”。
- 任务守卫与完成闸门：重复失败熔断、循环检测、按有没有进展止损、收尾催促（空回复、只说不做、todo 未完成、完成自检）、读代码问答的引用核实、改动影响说明（改动符号的调用方和没有测试引用的调用方）、计划模式与 review/once 审阅流程、提交前验证、自审子 Agent、项目审查命令（审查有问题交回模型修，最多 3 轮）、每个任务的验收报告。
- 有界模型调用次数、输出、上下文、请求空闲时间、任务与命令超时。
- Ctrl+C 取消、子进程树回收；Windows 使用 Job Object，在加入 Job 后才发送用户脚本。
- 工具调用 ID 配对、完整参数校验、reasoning/Anthropic thinking signature 回传。
- 统计并显示 token 和费用估算，不设花费上限；单次费用未知时按零累计，实际费用可能更高。
- 模型文本写 stdout；工具结果、退出码和诊断写 stderr。
- 常驻交互控制台：任务运行时输入行留在输出下方，可随时插话引导、排队命令或中断；审批和提问在输入行上作答；命令、技能和 `@路径` 补全，输入历史与搜索，多行输入，粘贴图片。不同内容分开显示：你的输入在灰底色块里，模型回复渲染 Markdown（标题、粗体、代码、列表、引用、表格），工具调用是一行标题加结果摘要，状态行首段是本任务的目标卡（请求原文第一行），随后是进度、耗时和 token。启动时对回环地址的本地服务（llama.cpp 等）自动探测真实上下文窗口并采用，显式配置 `context_window`/`context_bytes` 时不探测。详见下文“交互控制台”。
- 简体中文界面：提示、错误、帮助和命令输出都是中文，错误会说明下一步怎么做；发给模型的系统提示和工具结果保持英文。
- 会话保存/恢复、显式继续；会话以完整快照加追加日志落盘，已经完成的工具调用不重放。
- 手动/自动上下文压缩；保留完整历史归档、当前需求和最近完整工具交换。
- 文件工具变更的 unified diff、检查点预览与确认回退；发现用户后续修改时拒绝覆盖。
- 会话历史（标题、列表、详情、删除、清理、修复、Markdown 导出、`max_sessions` 自动清理）；按月费用账本与 `/status cost`；覆盖 Shell 改动、可再撤销的 Git 快照 `/undo`；BM25 记忆检索与注入、自动提取和 dream 整理。
- MCP 客户端：stdio、Streamable HTTP 与旧版 HTTP+SSE 三种传输，支持 `extensions.mcp` 和顶层 `mcp_servers` 两种写法；连接后工具清单写进 `mcp` 工具描述，处理 `tools/list_changed`，断线自动重连一次，从不重试失败调用。
- Skills：内置 13 个技能（`commit-messages`、`executing-plans`、`github-code-review`、`github-pr-workflow`、`karpathy-guidelines`、`performance-optimization`、`plan`、`requesting-code-review`、`safe-refactoring`、`spike`、`systematic-debugging`、`task-split`、`test-driven-development`；用户目录里的同名技能会覆盖内置版本，见“发现顺序”），多目录分层发现（项目覆盖用户、用户覆盖插件和内置），运行中新增或修改的技能无需重启即可使用，`paths` 按文件类型提示，`/<技能名>` 直接调用；自带技能市场（内置于程序、可离线使用，也可指向自建市场）；项目/全局带来源记忆的检索和增删改。
- 同步生命周期 Hooks 与兼容 Claude Code 格式的 `hooks.json`（`PreToolUse` 可阻止工具调用）；插件（`manifest.json` 或 `.claude-plugin/plugin.json`，`commands/*.md` 斜杠命令、git 安装、市场搜索）和本地插件清单。
- 项目信任：`.cove.json` 未信任时只应用安全字段，`/config trust` 后才应用 MCP、系统提示、校验命令等敏感字段。
- `draw_image` 画 PNG、`browser` 用无头 Chrome/Edge 渲染网页或截图；浏览器实操验收、一个需求两个方案（`/race`）、统一变更集（`/changes`）和远程监督（`/remote`），见[外围工作流](#外围工作流racechangesremote)。

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

配置是一个 JSON 文件：顶层用 `active_profile` 选择默认配置，并集中存放公共设置；`profiles` 中的每套配置指定模型、供应商及其他需要覆盖的设置。加载顺序如下：

1. 传了 `--config <文件>`（或 `COVE_CONFIG`），就读这个文件；
2. 否则读 `<数据目录>/config.json`，默认是 `%USERPROFILE%\.cove\config.json`，文件存在才读；
3. 工作区里的项目配置（如 `.cove.json`）**永远不会自动加载**。

完整示例见 [config.example.json](config.example.json)：

```json
{
  "active_profile": "deepseek",
  "mcp_servers": {},
  "profiles": {
    "deepseek": {
      "model": "deepseek-v4-pro",
      "provider": { "name": "deepseek" }
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
| `commit_gate` | 提交前验证门禁（默认 on）；`"off"` 关闭后只保留任务收尾验证 | - |
| `commit_review` | 提交前 AI 审查（默认 on）：用快速模型按真实缺陷和项目规则审查暂存内容；`"auto"` 只审改动不少于 40 行的提交，`"off"` 关闭。配置了 `review_commands` 时不运行（见“完成闸门”的项目审查） | `/commit --no-review` 单次跳过 |
| `thinking` | Anthropic 扩展思考：`adaptive` 或 `disabled`；不写则不发送该字段 | 无 |
| `effort` | Anthropic `output_config.effort`：`low`/`medium`/`high`/`xhigh`/`max` | 无 |
| `show_reasoning` | `true` 时实时显示模型的推理/思考文本（stderr，终端里为暗色斜体，以 `✻ 思考过程` 开头）；默认隐藏，交互控制台只留一行 `✻ 思考了 Ns` | 无 |
| `model_context_windows` | 兼容旧配置中的模型窗口记录；通常不需要设置 | 无 |
| `permission_mode` | `default`、`plan`、`auto`、`bypass`；旧写法 `ask`=default、`readonly`=plan | `--permission` |
| `max_iterations` | 每个任务最多调用模型的次数，默认 200；用完时交互会话询问是否再给一轮，否则总结后停下（见“完成闸门”的止损） | `--max-turns` |
| `stall_rounds` | 连续多少轮没有新的改动或新信息就停下，默认 12，一半时先提醒模型；`0` 关闭。子 Agent 同样适用（见“完成闸门”） | 无 |
| `max_turn_minutes` | 任务总时限（分钟，必须为正数） | `--timeout`（秒） |
| `system_prompt` | 可选的自定义指令，附加在内置规则之后；默认指导已内置，无需配置 | 无 |
| `max_output_tokens`、`context_window`、`context_bytes`、`input_price`、`output_price` | 含义同命令行参数 | 同名参数 |
| `subagent_max_iterations` | 每个子 Agent 最多调用模型的次数，默认 60（`explore` 最多 30） | 无 |
| `explore_model` | `explore` 子 Agent 用哪个模型：`fast`（默认，用 `model_fast`）或 `main`（主模型）；命令行 `--explore-model` | 无 |
| `agent_types` | 自定义子 Agent 类型：`{"名字": {"description", "instructions", "tools": [...], "readonly": bool, "model": "main"\|"fast"\|"inherit"\|模型名}}`，不能覆盖内置类型；`model` 默认 `inherit`（跟随当前任务） | 无 |
| `max_background_agents` | 同时运行的后台 Agent 上限，1–16，默认 4 | 无 |
| `regression_command` | `regression` 工具 `runner=command` 使用的命令，必须含 `{filter}` 占位符 | 无 |
| `experimental_tools` | 默认 `false`；为 `true` 时启用 task/team/send_message/brief/sleep 等实验工具 | 无 |
| `split_suggest_minutes` | 默认 5：`todo` 计划有 2 条以上未完成的条目、校准后的预估总时长超过这个分钟数、并且至少两条可以并行时，`todo` 的结果末尾提示模型按 `task-split` 技能先把拆分计划交给你确认，再 `execute_plan parallel=true isolation="overlay"`；同一个任务只提示一次，同一个会话里同一份计划只提示一次，条目都申报了 `writes` 的计划不提示，`execute_plan` 里的子 Agent 不提示；`0` 关闭。见“计划执行” | 无 |
| `overlay_check` | 覆盖层计划的共享检查槽命令（只编译不测试），例如 `"cargo check"`、`"dotnet build --no-restore"`；不配（或写空字符串）则关闭。开启后 `isolation="overlay"` 的条目多一个 `check` 工具，见“覆盖层拆分”。项目配置里的需要信任 | 无 |
| `mergiraf_path` | 合并时结构化合并用的 `mergiraf` 程序的绝对路径；不配时用 PATH 里的 `mergiraf`，都没有就跳过这一级。只在用户配置或已信任的项目配置里生效 | 无 |
| `done_verify_commands` | 完成前检查命令列表；配置后替代自动推断，写空数组 `[]` 表示关闭门禁 | 无 |
| `done_verify_auto` | 默认 `true`：按项目文件自动推断检查命令。`Cargo.toml` → `cargo check --all-targets`，`go.mod` → `go build ./...`，`tsconfig.json` 且已安装 typescript → `npx tsc --noEmit`，`package.json` 有 build 脚本且已安装依赖 → `npm run build --if-present`，`pyproject.toml`/`setup.py` → `python -m compileall -q .`，`*.sln`/`*.slnx`/`*.csproj` → `dotnet build` | 无 |
| `done_verify_tests` | 默认 `true`：自动推断时再加上项目测试，并按本轮改动收窄范围：Cargo workspace 只测改动所在的包（`cargo test -p a -p b`），Go 项目（`go test`）只测改动的包目录，另有 `dotnet test`（只测引用了改动项目的测试项目，没有测试覆盖时改为构建改动的项目；此时不再单独运行推断出的 `dotnet build`，测试会先构建。测试项目按 csproj 里的测试包或 `IsTestProject` 认定，也认最近一级 `Directory.Build.props` 统一加测试框架、项目名以 Test/Tests 结尾的写法；`global.json` 选了 `Microsoft.Testing.Platform` 时写成 `dotnet test --project <路径>`）、Maven/Gradle（`mvn test -pl <模块> -am`、`gradle :<模块>:test`，有 wrapper 时用 wrapper）、`npm test`（有真实 test 脚本时）、`python -m pytest -q`（有 pytest 配置或 tests 目录时；只跑和改动模块同名的 `test_<名字>.py`/`<名字>_test.py`，同名的有多个时只取目录最近的：和改动文件共同的上级目录最深的优先，再比去掉 `tests`/`test` 后末尾相同的目录层数，一样近的都跑）。pytest 和 `go test` 失败时，用同一条命令在本任务改动前的代码上再跑一次（把本任务用文件工具改过的文件临时换回改前内容，跑完立刻换回；崩溃时 `/resolve` 恢复），只有改动后新出现的失败用例才算验证未通过；全是改动前就失败的用例时算通过，进度行写明“N 个失败的用例改动前就失败，与本次改动无关”，账本状态记 `preexisting`；有新失败时交给模型的详情里分开列出两类。拿不到完整的用例级结果（输出被截断、合计对不上、收集中断、编译失败）、失败的那次运行超过 120 秒、本任务新建了 `conftest.py`/`_test.go`、改过的文件不是 UTF-8 或已被删除时，不做这一步，只看退出码；本任务新建的测试文件里的失败一律算新失败。`cargo test`（默认遇到第一个失败的测试程序就停）、dotnet、npm、Maven/Gradle 只看退出码；提交门禁也只看退出码。本任务用文件工具新建、之后又删掉的文件（比如用 `rm` 删掉的临时脚本）不参与范围选择，项目原有、被删掉的文件仍然参与；改动只剩这类文件时不验证。同样不参与的还有被 git 忽略的文件（如 `.local/` 下的草稿）和文档（`.md`、`.markdown`、`.rst`、`.adoc`），任务收尾验证和提交门禁都一样：只改了这些文件时不运行检查，和代码一起改时也不会把范围扩大到全量测试；这条只针对自动推断的检查，`done_verify_commands` 配置的命令照常运行。回复结束后的自动验证会先打印一行“[验证] 回复已结束，正在自动运行检查，不需要你操作”，运行期间状态栏显示“回复已结束，自动验证中（无需操作） · <命令>”（提交门禁里是“提交前验证中 · <命令>”），不再停在“回复中”，和等待你回复的状态区分开。同一任务中已通过的检查（模型自己运行的或提交门禁运行的）在之后没有文件改动时直接复用；`git add/commit/tag/push/fetch` 不使其失效 | 无 |
| `done_check` | `on`/`off`/`auto`（默认）：改过文件后，模型收尾前追问一次“需求是否全部完成”。`auto` 只在交互会话且非 Anthropic 供应商时开启 | 无 |
| `cite_check` | `on`/`off`/`auto`（默认，等于 on）：没改文件的回答结束前，在本地核实回复里的 `path:line` 引用（文件在、行号不越界、旁边提到的符号在那一行附近），对不上时催模型更正一次，见“完成闸门” | 无 |
| `done_self_review` | `off`（默认）/`on`/`auto`：自动验证通过后，让只读子 Agent 审阅本轮 diff（最多 40 KB），`auto` 只在改动不少于 40 行时启用。配置了 `review_commands` 时不运行 | 无 |
| `change_impact` | `auto`（默认）/`on`/`off`：用文件工具改了源码的任务收尾时，列出改动符号在其他文件的使用和有没有测试引用；删除或改了签名的符号有没被测试引用的调用方时，交给模型核对一次。见“完成闸门”的改动影响 | 无 |
| `review_commands` | 项目自己的审查命令列表，按顺序运行、结果合并；收尾（和提交前）按输出内容判定有没有问题，有问题交回模型修，见“完成闸门”的项目审查。写在 `.cove.json` 里需要信任，`[]` 关闭 | 无 |
| `review_when` | 项目审查在哪里运行：`done`（默认，任务收尾）、`commit`（提交前）、`both` | 无 |
| `review_block` | 达到这个级别的问题才交回模型：`critical`、`high`（默认）、`medium`、`low`、`any`（等于 `low`）；更低的级别只列给你看 | 无 |
| `review_max_rounds` | 收尾审查最多跑几轮（含第一轮），默认 3，范围 1–10 | 无 |
| `review_timeout_seconds` | 每条审查命令的总时限，默认 900 秒，最多 1800；运行中 180 秒没有任何活动（见 `done_verify_idle_seconds`）也会中止，审查记为跳过并写明原因 | 无 |
| `review_instructions` | 项目审查说明文件（相对工作区，如 `eng/ai-review/SKILL.md`）：内置审查（`done_self_review`、`commit_review`）把它当作额外的审查标准，最多读 16 KB；输出格式仍按 cove 的要求 | 无 |
| `workflow` | `direct`（默认）/`review`/`once`，含义见下文“计划模式与审阅流程” | 无 |
| `done_verify_timeout_seconds` | 每条检查命令的总时限，默认 3600 秒，范围 1–86400。检查只要还在干活就不会因为时长被砍，这个值只用来兜住一直耗 CPU 的死循环 | 无 |
| `done_verify_idle_seconds` | 检查命令完全没有活动多久就判定卡住并中止，默认 180 秒，范围 10–3600。活动指：有新输出、整个进程树的 CPU 时间在增加（每 5 秒超过 0.05 秒）、有读写、有进程启动或退出。Windows（Job 对象）和 Linux（`/proc`）支持；其他平台只按总时限。中止时写明运行了多久、多久没有活动、最后一次输出在多久前、仍在运行的进程（名称、pid、CPU 时间）和按工具给出的排查建议（如 `dotnet test` 的 `--blame-hang-timeout`），账本状态记 `stalled`，结果算“未验证”。验证期间状态栏显示已运行时长和“活跃”/“已 N 秒无活动” | 无 |
| `shell` | Shell 工具的解释器：`auto`（默认；Windows 上 Git Bash 优先，否则 PowerShell；其他平台 bash 或 sh）、`bash`、`powershell`、`cmd`（仅 Windows），或解释器的完整路径。也可用环境变量 `COVE_SHELL` | `auto` |
| `web_search` | `{"provider": "tavily"\|"brave"\|"duckduckgo", "api_key": "..."}`。未配置时依次看 `TAVILY_API_KEY`、`BRAVE_API_KEY`/`BRAVE_SEARCH_API_KEY`，都没有就用 DuckDuckGo | 无 |
| `skills_registry` | 技能市场来源：`registry.json` 的 https 地址，或本地路径（`registry.json` 文件或包含它的目录）；不写时用程序内置的市场。见下文“技能市场” | 无 |

- **优先级**：命令行/环境变量 > 选中的 profile > 顶层字段 > 内置默认值。profile 由 `--profile <名>` / `COVE_PROFILE` 选择，没有指定时用 `active_profile`；profile 名不存在时报错。
- **公共配置与差异**：通用设置写在顶层，profile 只写各自的模型、供应商和其他差异；profile 字段优先于顶层。`/model profile save` 会保留当前 profile 的设置，但省略与顶层相同的字段，以便继续继承公共值。
- **provider 块整体替换**：profile 里写了 `provider`，就整体替换顶层的 `provider`（不逐字段合并）；命令行传 `--provider` 时，配置里的整个 provider 块（包括 key 和地址）都不再使用，避免把一家的 key 发给另一家。
- **支持的供应商**：`openai`、`openai-compatible`、`anthropic`、`deepseek`、`glm`、`kimi`、`qwen`、`doubao`、`openrouter`、`siliconflow`、`groq`、`together`、`fireworks`、`xai`、`mistral`、`bedrock`，也接受别名 `claude`、`moonshot`、`zhipu`、`dashscope` 等。其中 `anthropic` 和 `bedrock` 走 Anthropic Messages 协议，其余走 OpenAI 兼容协议。不认识的名字按 OpenAI 兼容协议处理，但必须写 `base_url`。Anthropic 协议的根地址缺少 `/v1` 时会自动补上。`bedrock` 只支持 Bedrock API key（`AWS_BEARER_TOKEN_BEDROCK` 或 `provider.api_key`），不支持 SigV4 签名。
- **Bedrock**：默认走 AWS 原生的 Bedrock Runtime 接口（`https://bedrock-runtime.<区域>.amazonaws.com`，区域取 `AWS_REGION`/`AWS_DEFAULT_REGION`，默认 `us-east-1`），与 Claude Code 的 `CLAUDE_CODE_USE_BEDROCK` 相同：按 AWS 账号授权，不按调用方 IP 所在地区限制。也可以把 `base_url` 设为 Anthropic 兼容端点 `bedrock-mantle`，但该端点会拒绝不受支持地区的 IP（报 “unsupported countries”）。模型可写短名（`anthropic.claude-haiku-4-5`），首次请求时查询账号可用的推理配置，解析为完整 ID（`us.anthropic.claude-haiku-4-5-20251001-v1:0`），优先与区域对应的 `us.`/`eu.`/`apac.`，其次 `global.`；Claude Code 的 `[1m]` 后缀会开启 1M 上下文。
- **网络代理**：默认跟随系统代理（Windows“Internet 选项”/macOS 网络设置，VPN 客户端开启“系统代理”模式时写入）以及 `HTTPS_PROXY`、`ALL_PROXY`、`NO_PROXY` 环境变量；本机地址始终直连。也可用配置 `"proxy": "http://127.0.0.1:7890"`（支持 `http`、`https`、`socks5`、`socks5h`）、命令行 `--proxy` 或环境变量 `COVE_PROXY` 指定，`none` 表示全部直连。`/status` 显示当前代理。`web_fetch` 为防止访问内网，只使用显式指定的代理。
- **调试与输出**：`debug`（或 `-d`/`--debug`）把每次模型调用、工具调用和压缩的轨迹记录（不含正文）实时打印到 stderr；`verbose`（或 `--verbose`）完整显示工具结果，不只显示摘要（交互控制台）或前 12 行（`-p` 和管道输出）；`live_output`（默认 true）控制命令输出是否逐行直播，`false` 时只在结束时显示摘要和结尾几行（失败时更多）。已废弃的 `telemetry`、`thinking_tokens` 会被静默忽略。
- **API key 查找顺序**：`COVE_API_KEY` > `provider.api_key`/`provider.api_keys` > 供应商对应的环境变量（例如 `DEEPSEEK_API_KEY`、`KIMI_API_KEY`/`MOONSHOT_API_KEY`）。每一处都可以用逗号分隔写多个 key。带 `****` 的已掩码 key 会被忽略。key 写进配置文件时，文件必须保持私有；Unix 下如果其他用户可读，启动时会给出警告。
- **未知字段**：不认识的字段只给出警告（“ignoring keys not supported by cove”）并忽略，不会报错。`mcp_servers`、`disabled_skills` 见下文“扩展”。顶层 `base_url` 会报错，请写到 `provider.base_url`。
- `/status` 会显示当前生效的供应商、协议、模型、地址、profile 和配置文件路径，最后一行是构建标识（见“项目命令、诊断与命令行”里的“构建标识”）。

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

配置 `model_fast` 后，同一个供应商上有两个模型：`model`（高级模型）和 `model_fast`（快速模型）。每个新任务开始时打一次分，决定整个任务用哪个模型；选用快速模型时显示一行"简单任务，交给快速模型 <模型>"，使用主模型时不提示；`/model next` 指定时显示"本任务用 <模型>（/model next 指定）"。打分规则：

- 复杂度关键词（refactor、architecture、debug、optimize… 及对应中文“重构、架构、调试、优化、性能、安全审计”等）+0.45；消息长度最多 +0.20（1000 字符封顶）；提到的文件路径最多 +0.15；近期快速模型失败率最多 +0.10。费用不影响路由。
- 总分 ≥ 0.35 用高级模型；消息不少于 2000 字符一律用高级模型。
- 上一个任务跑在高级模型上时，40 字以内的简短追问（如“继续”）留在高级模型，避免中途换模型丢掉提示缓存。
- **升级**：快速模型上验证门禁失败，或连续 3 批工具调用全部失败，本任务剩余部分改用高级模型。
- **回退**：重试后仍是 5xx/429 时，本任务换到另一个模型一次；快速模型返回 404（模型不存在）时改用主模型。
- 上下文压缩等后台调用使用快速模型；其他模块通过同一个入口（`background_provider`）做摘要、记忆提取等廉价调用。
- 费用按实际应答的模型计价，`/model` 列出每个模型的调用次数、token（含缓存读写）和费用。快速模型不在价格表里时按主模型价格估算（宁可高估）。

### 供应商特性

- **Anthropic**：`thinking`、`effort` 写入请求；thinking 块（含签名）原样回传，压缩后只保留当前工具循环和最后一条助手消息里的 thinking。系统提示和最后 3 条消息带 `cache_control: ephemeral` 提示缓存，并发送 prompt-caching beta 头（Bedrock 不发）。主任务和子 Agent 的对话请求在支持的模型上（Opus 4.5、Sonnet 4.6、Haiku 4.5 及之后的所有 Claude 模型）全部使用 1 小时缓存（`ttl: "1h"`），任务之间停 5–60 分钟仍能命中；压缩、记忆提取、标题等一次性的后台调用保持默认的 5 分钟。
- **缓存计费**：解析 Anthropic 的 `cache_read_input_tokens`/`cache_creation_input_tokens`、OpenAI 的 `cached_tokens`、DeepSeek 的 `prompt_cache_hit_tokens`。缓存读取按内置价格表里各模型的缓存价（未知模型按输入价的 0.1 倍），缓存写入按输入价的 1.25 倍；Anthropic 报告的 1 小时缓存写入（`cache_creation.ephemeral_1h_input_tokens`）按输入价的 2 倍。
- **OpenAI 兼容**：o1/o3/o4/gpt-5 系列用 `max_completion_tokens`；`deepseek-reasoner`/`deepseek-r1` 不发送工具定义；`reasoning_content` 只在带工具调用的助手消息上回传。
- **多 key 轮换**：`provider.api_keys`（或逗号分隔的环境变量）轮流使用。429 让该 key 休息（按 `Retry-After`，没有则 60 秒），有其他 key 时立即换用；401/403 把该 key 标为失效并立即用下一个重试。
- **限流信息**：记录响应里的 `x-ratelimit-*` / `anthropic-ratelimit-*` 头，`/status limits` 显示剩余请求数、token 数和重置倒计时。
- **上下文超长**：服务端报上下文超长时，自动压缩并重试一次；错误里写明窗口大小时，缩小本会话的上下文预算，并按供应商、接口和模型写入独立运行缓存，下次启动继续使用，不改写用户配置。

### 附件与图片

- 消息里的 `@路径`（可加引号：`@"带 空格.png"`）指向已存在的文件时作为附件发送，并从文字中去掉；不是文件的 `@xxx`（邮箱、`@Override` 等）原样保留。
- 消息中写 `@路径` 即附加该文件（状态行可见，Alt+Backspace 移除）；附件随下一条消息发送一次后清空。
- PNG/JPEG/GIF/WebP 按文件头识别，作为图片发送：Anthropic 用 base64 source，OpenAI 兼容协议用 data URL 的 `image_url`。长边超过 1568 像素或超过 5 MB 时自动缩放、重新编码。UTF-8 文本文件作为文字附加（最多 200 KB）；其他二进制文件拒绝附加。
- `read` 工具读到图片时返回图片本身（源文件不超过 5 MiB、边长不超过 4096 像素），模型可以直接“看”截图。
- 当前模型不支持视觉时，如果 `model`/`model_fast` 中有支持视觉的模型，带图片的请求自动改用它；都不支持时，图片换成一行说明文字再发送。

### 运行中切换

| 命令 | 行为 |
| --- | --- |
| `/model` | 当前供应商、协议、地址、主模型/快速模型/本任务模型、key 数量和各模型用量 |
| `/model <名>` | 切换主模型；`auto` 表示供应商默认模型，`deepseek/deepseek-chat` 这类带本供应商前缀的名字会去掉前缀（OpenRouter 等网关保留） |
| `/model fast <名\|off>` | 设置或关闭快速模型 |
| `/model next <main\|fast\|auto\|名>` | 指定下一个任务用的模型（仅一次，之后恢复自动路由） |
| `/model provider <名> [模型]` | 切换供应商；原来的 key 和地址不会带给新供应商。原模型是旧供应商的默认模型时跟着换成新供应商的默认模型；缺 key 时在终端里提示输入（不回显） |
| `/model profile list\|switch\|save\|delete\|show <名>` | 管理 profile；`show` 中的 key 已打码 |
| `/config model\|provider\|base_url\|system\|max_output_tokens [值]` | 不带值显示当前值，带值修改 |
| `/config api_key [key]` | 不带值时在终端里不回显地输入；显示时只显示打码的 key |
| `/config effort [low\|medium\|high\|xhigh\|max\|default]` | 立即生效并写入当前配置档；越低响应越快、输出越少，复杂任务效果可能下降；`default` 删除设置交给服务端。只对 Anthropic 协议（含 Bedrock）生效 |
| `/config setup` | 重新运行设置向导（含验证请求） |
| `/status limits` | 最近一次响应的限流信息 |

修改立即生效，并原子写入用户配置文件：只改相关字段，其他字段保持不变；当前有 profile 时通常写入该 profile。`/config system` 更新原有作用域：公共提示词写回顶层，profile 独立提示词写回该 profile。新配置先完整加载校验，失败时不写文件、不生效。启动时用 `--model`/`--provider`/`--base-url` 指定的值会在第一次修改时一并写入，配置文件始终与正在运行的设置一致；环境变量里的 key 不写入文件。

会话不再绑定模型和供应商：恢复的会话可以换模型或供应商继续，每次请求按实际模型记账；工作区必须一致。

## 扩展：Skills、插件、Hooks、MCP 与项目信任

所有扩展都遵循同一条信任线：项目里带来的内容可以作为**文本**进入提示（技能、插件命令），但任何**执行**（MCP 进程/连接、Hooks、git 克隆）都需要 `--trust-extensions` 或 `/config trust`，并且每次动作仍走当前权限模式的审批。下文的 `<data_dir>` 默认是 `~/.cove`。

### Skills

- **发现顺序**（后者覆盖同名前者）：内置技能（编译进二进制，随版本更新）→ 插件的 `skills/` → `~/.claude/skills` → `<data_dir>/skills` → 配置的 `extensions.skills` → 项目内从仓库根到 cwd 每一级的 `.claude/skills`、`.cove/skills`（越内层越优先）。cwd 以下的子目录不扫描，避免依赖包里的技能混进来。
- **内置技能**（13 个）：`commit-messages`、`executing-plans`、`github-code-review`、`github-pr-workflow`、`karpathy-guidelines`、`performance-optimization`、`plan`、`requesting-code-review`、`safe-refactoring`、`spike`、`systematic-debugging`、`task-split`（预计超过 5 分钟、能拆出并行部分的任务：先分析影响范围，拆成写集合互不重叠的单元交给你确认，再用 `todo` 的 `writes`/`depends`/`estimate_minutes` 和 `execute_plan parallel=true isolation="overlay"` 并行执行、统一合并，最后统一构建、测试和审查一次）、`test-driven-development`。按上面的发现顺序，用户目录（如 `~/.claude/skills/task-split`）或项目里的同名技能会覆盖内置版本，这是预期行为；想用内置版本就删掉或改名同名的技能。
- **运行中生效**：交互模式下每次读取输入前（至多每秒一次）检查这些目录和技能文件的修改时间，有新增、修改或删除就重新加载，补全列表和 `/<技能名>` 立即可用，不需要重启。重新加载失败时保留原来的技能并给出警告。
- **文件格式**：`<目录>/<名字>/SKILL.md` 或 `<目录>/*.md`，可选 YAML frontmatter：`name`、`description`（展示时截到 120 字符）、`paths`（如 `"*.rs, src/**/*.ts"`）、`allowed_tools`（也接受 `allowed-tools`）、`steps`。没有 frontmatter 时名字取目录/文件名，描述取正文第一行。
- **安全**：项目技能文件如果是指向项目外的链接，拒绝加载并警告；技能正文始终作为不可信数据交给模型，`allowed_tools` 只是收窄工具选择的提示，不授予任何权限。配置 `disabled_skills: ["名字"]` 可整体移除技能。
- **调用**：`/<技能名> [参数]` 把技能（编号步骤清单、工具限制说明、正文和你的参数）作为一次新任务发给模型。内置命令同名时内置命令优先；输错命令会提示“你是不是想输入 /…”。模型也可以用 `skill` 工具按需加载。
- **按文件类型提示**：带 `paths` 的技能在 read/write/edit 触及匹配文件时，于该次工具结果后附 `<relevant_skills>`，每个会话每个技能只提示一次。
- **不再自动学习技能**：早期版本会在任务结束后由后台模型复盘并学习技能，这个功能已移除（见 `docs/optimizations/0003`）。可复用的经验由自动记忆和 dream 记忆整理负责；技能只来自内置、插件、用户目录、配置和项目目录。
- **改进建议**：`/suggestions analyze` 显式分析当前会话，`/suggestions list` 查看，`/suggestions accept <完整哈希>`、`reject <完整哈希>` 和 `revoke <完整哈希> <原因>` 记录人工决定。仅在显式分析时扫描最近 256 条消息，每条最多 32 个内容块；目前只识别白名单工具相同输入至少两次失败，重复调用 ID 或结果存在歧义时不作为证据。接受前重新核对当前会话证据，历史压缩或修改可能使证据位置失效；拒绝及撤销仍可关闭过期建议。接受仅记录决定，不执行建议、不改动提示词、技能或权限，也不发起模型请求。
- 建议记录保存在 `<data_dir>/suggestions/<会话ID>.json`，每个会话最多 128 条，不自动清理；记录工具名、输入摘要和消息位置，不复制工具输入或输出。摘要不是匿名化手段，低熵输入可能被猜出；撤销原因由用户填写并持久保存。记录仅做一致性校验，不能防止能修改本地文件的人伪造记录。
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

- 插件放在 `<data_dir>/plugins/<名字>/`，清单为 `manifest.json`（`name`、`version`、`description`、`author`、`commands`、`skills`），也接受 Claude Code 插件的 `.claude-plugin/plugin.json`。没有清单的子目录（例如只放了一个命令行工具）不当插件加载，启动时提示"不是 cove 插件，已跳过"；不想看到提示，就把它移出 `plugins/`，或把目录名改成以 `.disabled` 结尾。`skills/` 下的技能参与发现；`commands/*.md`（frontmatter `description` + 正文，`$ARGUMENTS` 替换为参数，没有占位符时参数附在末尾）成为 `/<命令名>`。多个插件同名命令时，按插件名排序靠前的生效。
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
- 项目未信任时，模型调用 `mcp`（列工具、调用、读资源）不会启动服务器，这次调用作为失败结果交给模型，结果里写明先 `/config trust` 再 `/restart`；工具不在 `extensions.mcp` 的 `tools` 白名单里时同样作为失败结果。任务不会因此中断，模型可以继续做别的事，或在回答里告诉你怎么放行。只列配置的 `servers` 不需要信任。
- **需要登录的 HTTP 服务（OAuth）**：例如 Atlassian（Jira/Confluence），配置 `"mcp_servers": {"atlassian": {"type": "http", "url": "https://mcp.atlassian.com/v2/mcp"}}`，然后运行 `/mcp login atlassian`：按 MCP 授权流程自动发现授权服务器、动态注册客户端，在浏览器里登录授权（PKCE，回调到本机 `127.0.0.1` 临时端口，5 分钟内完成）。令牌保存在 `<数据目录>/mcp-oauth/`（工具无法访问），过期前自动刷新；服务拒绝令牌时先刷新一次，仍失败则提示重新 `login`。`logout` 删除保存的令牌。

### 项目信任与 `.cove.json`

- 启动时读取 cwd 下的 `.cove.json`（必须是不超过 1 MiB 的普通文件，不跟随链接；格式错误只警告）。它合并到用户配置的顶层，选中的 profile 仍覆盖其上。
- **无需信任**即生效的安全字段：`model`、`model_fast`、`permission_mode` 为 `default`/`plan`、只做减法的 `done_verify_auto`/`done_verify_tests`（设为 `false`）、`commit_gate`/`commit_review`（设为 `off`）、`review_commands` 设为 `[]`、`overlay_check` 设为 `""`、不比已生效值更松的 `review_block`（级别相同或更低）和 `review_max_rounds`（不多于已生效值，默认 3）、`review_when`、`review_timeout_seconds`、`review_instructions`、`done_verify_timeout_seconds`、`done_verify_idle_seconds`、`done_self_review`、`done_check`、`cite_check`、`change_impact`、`thinking`、`effort`、`show_reasoning`、`disabled_skills`、`split_suggest_minutes`、`max_iterations`、`stall_rounds` 等限额。
- **需要信任**的敏感字段：`auto`/`bypass` 权限、`system_prompt`、`mcp_servers`、`done_verify_commands`、`review_commands`（会运行仓库里的代码）、`overlay_check`（设为空字符串关闭时无需信任）、`mergiraf_path`、放松的 `review_block`/`review_max_rounds`、开启校验、`web_search`、`provider`、`memory_embedding`、`skills_registry`、单价等。未信任时启动会列出被忽略的字段；不支持的字段（如 `profiles`、`extensions`）单独列出。
- `/config trust` 把项目根目录（git 根，否则 cwd）记为可信，并按**本次启动读到的内容**记录 `.cove.json` 的 sha256，写入 `<data_dir>/trusted_projects.json`；文件之后再改动就需要重新信任。重启后生效。可信项目等同于加了 `--trust-extensions`，自动验证门禁也通过 `cove::trust::is_trusted(cwd)` 查询同一份记录。按内容信任过的 `.cove.json` 里的 `review_commands` 运行时不再逐次审批（等于你批准了这条命令；否则 `-p`、`auto` 模式下 `bash x.sh` 这类命令会因为没人可问而不运行），用户配置里写的同样不审批。

## 会话、恢复与交互

不带提示词启动 REPL；`-p` 或位置提示词为单次任务。交互控制台的启动信息里显示会话 ID 前 8 位；`-p`、管道和简单模式启动时打印一行 `[session] <完整 ID>`。

| 命令 | 行为 |
| --- | --- |
| `/help`、`/status` | 帮助、状态、上下文与累计用量 |
| `/new` | 保存当前会话并新建；不回退文件 |
| `/handoff [重点]` | 换话题时交接到新会话。模型在原会话的上下文上写一份交接摘要（之前做了什么、用户定下的要求、没做完和待确认的事、下一步可能用到的命令和路径），请求的系统提示、工具定义和消息与上一轮完全相同，只在最后多一条要求，所以前缀命中提示缓存；用的是上一个任务的模型。cove 再附上自己收集的事实：本会话用 write/edit 改过的文件、执行过的 `git commit`/`git push` 命令、工作区未提交的改动（`git status --short`），这部分不经过模型。新会话的第一条消息就是这份交接，之后输入的第一个任务算第一轮。原会话的消息、状态原样保留，记下 `handoff_to`；新会话记下 `handoff_from`，`/resume` 列表里两个都能看到（新会话要有了第一个任务才列出）。摘要和交接调用的费用计入原会话。`重点` 会作为写摘要时的要求。摘要超过 8000 tokens 时截断并标注；摘要为空、被截断或模型要调用工具时不交接，留在原会话。不运行 `SessionEnd` 钩子、不断开 MCP。交接后打印 `[session] <新 ID>` 和交接前后的上下文估算 |
| `/back` | 在 `/handoff` 交接出来的会话里，回到原会话（同 `/resume <原会话 ID>`），原会话的完整上下文都在；1 小时内提示缓存通常还在 |
| `/resume [list\|all]`、`/resume <n\|id>` | 列出当前工作区（`all`：所有工作区）的其他会话，显示标题、对话轮数（包括压缩前的轮次）和本地时间（从未提问的空会话和当前会话不列出）；按编号或 ID（前缀）切换。列表后直接输入数字即可恢复该条。交互控制台里只打开历史面板（不再另外打印文本列表），会话按更新时间分组为“今天”“昨天”“最近 7 天”“更早”：输入文字筛选标题和提问，↑↓ 选择，数字键 1–9 直接选，Enter 恢复，Tab 打开阅读器，Ctrl+A 切换本工作区 / 全部工作区，Ctrl+R 改名，Esc 关闭。选中其他工作区的会话时，保存当前会话后在那个目录重启并恢复（同 `/cd`） |
| `/resume search <关键词>` | 按标题和用户请求检索所有工作区的会话（BM25，支持中文） |
| `/resume detail\|delete <n\|id>` | 查看详情 / 删除会话（含归档和文件检查点）；不能删除当前会话 |
| `/resume rename <n\|id> <名字>`、`/rename <名字>` | 给会话改名；改过名的会话不再被 AI 生成的标题覆盖。会话标题默认由模型在第 1、2、3 轮完成后各生成一次，之后每 5 轮更新一次（本地模型、`--no-auto`、`-p` 时不生成） |
| `/resume clear [all] [confirm]`、`/resume clean`、`/resume export [file]` | 批量清理（先预览，加 `confirm` 才执行）/ 修复历史（先备份）/ 导出 Markdown 记录 |
| `/continue`、`/continue <n|id>` | 裸命令列出当前工作区的会话（当前会话有未完成任务时排在第一位，标“当前”），交互终端打开历史面板；指定编号或 ID 直接恢复。未完成任务会继续运行，已完成任务恢复上下文后等待新消息。结果不确定的工具调用会作为失败结果交回 Agent 检查，不会自动重放。 |
| `/turns [编号]` | 查看当前会话每一轮的提问和结论（不含中间的工具调用），包括压缩前的轮次。交互终端打开全屏阅读器：上方是你的输入，↑↓ 或单击切换，下方显示这一轮的结论；滚轮、PageUp/PageDown 滚动；拖动鼠标选中即复制（可跨屏），`c` 复制整段 Markdown 原文，`o` 在编辑器中打开，`/` 筛选，Esc 返回。普通终端用 `/turns <编号>` 查看指定轮。 |
| `/status usage`（简写 `/usage`） | 当前会话的账户与用量：认证方式（供应商及 key 来源）、估算总费用、等待模型响应的累计时间（API 耗时）、会话创建至今的时间、文件工具新增/删除的行数、模型调用次数，以及每个模型的输入/输出/缓存读写 token、调用次数和费用（含子 Agent 的花费） |
| `/status cost [period] [export [file]]`（简写 `/cost`） | 按供应商/模型统计用量与估算费用，可导出 CSV |
| `/compact` | 付费摘要压缩，原始上下文先归档 |
| `/diff` | 有 Git 快照时显示本任务首个快照以来的整树变更（含 Shell 改动）；否则显示文件工具跟踪的变更 |
| `/undo` | 列出回滚点（最新 15 个；每个任务开始改动前一个，会改东西的 Shell 和子代理前各一个） |
| `/undo <#n\|hash>`、`/undo files <#n\|hash> <文件…>`、`/undo confirm` | 预览整个工作区或指定文件的恢复，再 `/undo confirm` 执行（含 Shell 改动，恢复本身可再撤销）。没有 Git 快照时，`/undo`、`/undo confirm` 退回上一个任务的文件工具回滚 |
| `/resolve <call-id> executed` | 用户确认不确定调用已执行，禁止重放；结果标为未验证失败 |
| `/resolve <call-id> not-run` | 检查证据确认未执行后允许重试 |
| `/config` | Skills、MCP、Hooks、插件及记忆管理；`/mcp`、`/plugins`、`/hooks`、`/memory` 是 `/config mcp` 等的简写 |
| `/memory`（同 `/config memory`）`[list\|add [global] <文本>\|remove <id>\|search <查询>\|stats\|source <id>\|dream [status\|run\|backups\|show <备份>\|preview <备份> <id>\|restore <token>]]` | 记忆管理；`/config memory <JSON>` 仍按 memory 工具参数执行 |
| `/status agents [show\|stop\|park\|resume\|send\|merge\|discard <id\|main> …]` | 子 Agent 与主任务的生命周期：列表、查看完整报告、中断、暂存、唤醒、发消息、合并/丢弃后台 Agent 的分支；`/status agents repair` 修复中断后遗留的回归验证临时还原 |
| `/exit` | 保存并退出；EOF（Ctrl+D）同样保存 |

未知斜杠命令报错，不发送给模型。

### 交互控制台

终端支持控制序列时，REPL 由一个常驻控制台接管键盘：输出显示在上方，输入行始终留在下方，任务运行时也一样。任务运行时，输入行上方的状态行显示转圈动画、当前在做什么、已用时间和 token 数，例如 `⠹ 运行 cargo test… 12s · ↑18.2k ↓1.1k tokens · 按两次 Esc 中断`。

有转到后台或 `run_in_background` 启动的命令在跑时，状态行多一段 `后台命令 N 个`，空闲时也显示；本轮结束前等待后台命令时，状态行显示 `等待后台命令 · <命令> · <已运行时长> · 活跃`。

状态行下面常驻一行用量，空闲和运行时都显示：`上下文 52.1k/200k（26%）· 本会话 1.2M tokens · $3.20`。
- **上下文**：模型调用后取服务商报告的本次输入 token（含缓存读写），即模型实际读到的对话长度；启动、压缩、`/new`、`/resume` 之后历史变了，改用估算值（消息、系统提示和工具定义之和）。
- **窗口**：当前模型的上下文窗口。
- **本会话**：会话账本里的输入和输出 token 之和（输入已包含缓存读写），以及估算花费（价格未知时不显示）。

上下文达到自动压缩线的 80% 时，这一行变成黄色，显示自动压缩线占窗口的百分比，并提示可先 `/compact`。还没到这一步、但上下文已达到 15 万 tokens（压缩线较低的模型按压缩线的 3/8，低于 3.2 万时不提示）时，行尾提示 `· 换话题可 /handoff`。自动压缩线与 `/status` 显示的一致，按对话消息估算；状态栏用量还包含系统提示和工具定义，两者口径不同。窗口未知时不显示这一行。

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

- **显示规则**：模型回复的 Markdown 边接收边渲染：标题加粗（一、二级加下划线），`**粗体**`，反引号里的代码名、路径和命令为浅橙色，列表符号换成 `•`，引用以 `│` 开头，分隔线画成横线，代码块保留原文（围栏行为暗色）便于复制。表格收齐后按终端宽度对齐，长单元格在列内换行并分隔多行记录；列太多时改为带列名的纵向展示。工具调用一行写明动作和关键参数（读取的文件、搜索的内容、运行的命令），下面用 `└` 给出摘要（行数、匹配数、退出码等），`#n` 可用 `/expand n` 看完整输出。同时运行的多个工具，各自的标题和结果放在一起。`-p`、管道和简单模式保持原来的机器可读行（`[tool]`、`[ok]`、`[summary]`），供脚本和测试解析。

- **任务运行中输入**：普通文字作为插话，默认在工具结束或本轮回复完成时自动送入；以 `!` 开头则优先插话并打断生成。被中断的请求可能照常计费，但未返回的费用按零累计。插话标注为任务运行时输入的指令，最多排 16 条；任务结束时还没送达的插话成为下一条消息。斜杠命令和带附件的消息排队，等当前任务结束后依次执行。
- **中断**：任务运行中连按两次 **Esc**（2 秒内）或按 **Ctrl+C** 中断任务，`/continue` 可以继续。空闲时在空行上按 Ctrl+C 只提示用 `/exit` 或 Ctrl+D 退出；**Ctrl+D** 在空行上保存并退出。
- **审批与提问**：在同一输入行上作答。审批显示在黄色边框里：第一行是“需要批准”，接着是命令类型（只读、构建/测试、未识别等）或需要审批的原因（如“删除文件（rm）”“推送到远端（git push）”），然后是要执行的操作和红色的风险警告，接着是编号选项（参照 Claude Code 和 Codex）：`1. 允许这一次 (y)`、`2. 本次会话不再询问 (s)`、`3. 本项目以后都不再询问 (a)`、`4. 拒绝 (n)`，“不再询问”两项后面写明这条规则会放行什么（如“git 常规操作（push、pull、merge 等；--force、reset、clean 仍会询问）”“以 `npm run` 开头的命令”“本项目内的文件修改”）。会话规则只在内存里，退出即失效；项目规则写入 policies.json；没有 policies.json 时不提供第 3 项，生成不了规则的操作（如扩展进程）只有 1 和拒绝。按 `y`/`s`/`a` 立即提交，上下键在选项间切换后回车；大写的 `N` 是默认值，直接回车即拒绝。输入 `n <原因>` 或任何不是选项的文字，都当作拒绝理由交给模型。也接受 `yes`/`always`/`session`/`no` 和中文“是、允许、本次会话、总是、否、拒绝”。Esc 或 Ctrl+C 取消当前提问。
- **拒绝后任务继续**：拒绝一次调用不会结束任务。模型收到“用户拒绝了”和你的理由，被要求不要重试、也不要换个等价命令绕过，而是换别的办法或询问你；屏幕上这一步显示为 `└ 已拒绝：<理由>（已告诉模型）`。拒绝不算工具失败。后台 Agent 和远程审批的拒绝同样处理。
- **补全**：Tab 补全斜杠命令、子命令（由 `/help` 的用法行自动生成，新增子命令无需另外登记）、技能名、插件命令和 `@路径`；候选列在输入行下方，上下键选择。
- **历史**：提交的输入保存在 `<data_dir>/input_history.jsonl`（最多 1000 条），上下键翻阅，**Ctrl+R** 反向搜索（再按 Ctrl+R 找更早的匹配，Enter 采用，Esc 取消）。含 API key、token、密码等疑似密钥的输入（包括 `/config api_key …`、带账号的 base_url）不写入历史。
- **多行输入**：Ctrl+Enter、Shift+Enter、Alt+Enter 插入换行（Windows 上输入框提示 Ctrl+Enter）；macOS/Linux 的多数终端分不出 Ctrl+Enter 和 Enter，那里用 Ctrl+J（提示也写 Ctrl+J），Ctrl+J 在所有平台都可用；行尾输入 `\` 再回车也会换行；粘贴的多行文本保持原样，不会逐行提交。多行时上下键在行间移动。
- **编辑键**：Ctrl+A/Ctrl+E 或 Home/End 到行首/行尾，Alt+B/Alt+F 或 Ctrl/Alt+←/→ 按词移动，Ctrl+W 或 Alt/Ctrl+Backspace 删前一个词，Ctrl+K 删到行尾，Ctrl+U 删到行首；Esc 清空输入，紧接着 Ctrl+Z 恢复；Ctrl+L 清屏。
- **任务面板**：Alt+M 打开或关闭输入行下方的任务面板（Esc 也可关闭），最多 8 行：主任务的状态、用时和当前动作，本次请求，`todo` 计划的进度和当前项（"完成 1/3 · 当前：…"），然后是子 Agent / 后台 Agent（没有时显示"子代理：无"）。子代理只逐行列出还在进行的（运行中、暂停、被中断），一行一个，用中文写状态和当前一步；已完成、失败的合并成一行"已结束 N 个（/status agents 查看）"，`/status agents` 仍列全部。后台 Agent 需要你确认时，面板里还会出现"待确认：…"行。任务运行时状态行提示"Alt+M 查看任务"，面板打开后不再提示。
- **图片**：粘贴或拖入图片文件路径（PNG/JPEG/GIF/WebP，可多个、可带引号）会作为附件挂在下一条消息上；Windows 上 Alt+V 附加剪贴板里的图片（保存为数据目录 `clipboard/` 下的 PNG，图片发送时已写进会话，这些文件在下次粘贴时清理 7 天前的）；空行上 Alt+Backspace 移除最后一个附件。只有附件、没有文字时，按 Enter 让模型分析附件。
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

- `sessions/<id>.json` 始终是一份完整的会话快照（原子替换写入，单独读取它也能得到一个有效的会话）；其中 `builds` 列出在这个会话里跑过任务（新任务或 `/continue`）的构建标识，按首次出现的顺序、不重复，只留最近 20 个；旧会话没有这个字段，按空处理；
- 任务进行中的每次保存只向 `journals/<id>.log` 追加一条记录（自上次以来新增的消息和其余字段），写完即同步到磁盘，比每次重写整个文件快得多；
- 任务结束、退出、历史被改写（压缩、替换）或日志过长（8 MiB）时写一份新快照并清空日志；
- 读取时先读快照，再重放快照未包含的日志记录；重放是幂等的，崩溃时写了一半的最后一条记录会被忽略。

同一数据根中的同一工作区只允许一个 CLI 写者；使用不同数据根不能相互保护。执行意图先保存，结果先持久化再展示。重启后，未记录结果的在途调用按能确定的情况处理：没有副作用的调用（读取、搜索、提问、todo、网页读取）在继续时直接重新运行；文件写入/编辑在检查点记有意图、且文件内容正好是预期结果或原内容时，自动判定为已完成或未执行；其余调用标记为不确定，不自动重试，`/continue` 会列出它们的 ID 和两种处理方式：检查副作用后 `/resolve`，或者直接发一条新消息。发新消息时，旧批次里没运行的调用记为未执行，结果不明的记为不确定，什么都不重放，对话照常继续。`executed` 不会伪造原始输出、退出码或测试通过证据；文件调用还会对照检查点意图核验内容。遇到无法核对的外部改动，保守拒绝处理。

## 项目命令、诊断与命令行

| 命令 | 行为 |
| --- | --- |
| `/help`、`/help <命令>` | 按组列出全部命令（含各功能区的命令）/ 单个命令的用法；未知命令给出相近命令建议，参数不对时显示该命令用法，**都不发送给模型** |
| `/init`、`/init apply`、`/init discard` | 一次有界模型调用（最多 3 分钟，计费）阅读仓库摘要（两级目录、README 与清单文件开头、项目概要），起草 `CLAUDE.md`；仓库采用 `AGENTS.md` 约定时起草 `AGENTS.md`。模型不可用、失败或 Ctrl+C 时用模板。草稿存于 `<数据目录>/projects/<hash>/init-draft.md`，以 diff 展示；`apply` 经授权后走工作区检查点写入（`/diff`、`/undo` 可见可退），**从不覆盖已有文件**；`discard` 删除草稿 |
| `/diff git`、`/diff git --stat` | 分两节显示 `git diff --cached` 与 `git diff`；`--stat` 列出已暂存、未暂存和未跟踪文件，用于提交前审阅。`/diff` 仍是当前任务的文件工具变更 |
| `/commit [--preview] [--no-review] [--all \| --only <路径>... --] [消息]` | 默认只提交**已暂存**内容；`--all`/`--only` 通过从 HEAD 建立的临时 `GIT_INDEX_FILE` 选择文件，不改动你原来的暂存区（提交后只同步已提交文件）。先显示 stat、执行 `git diff --cached --check`，default 模式下请求批准，再运行验证命令（`done_verify_commands` 或自动检测）；验证前后检查暂存树哈希和工作区内容均未改变，否则不提交。验证通过后做提交前 AI 审查（见“完成闸门”），发现问题就不提交并列出问题；`--no-review` 跳过审查。默认消息 `auto-commit`；**从不推送** |
| `/rules [add [global] <文字>\|remove <n>\|remove g<n>\|promote <n>\|pending\|local]` | 全局偏好和项目规则（见“项目规则”“全局偏好”）：不带参数先列全局偏好（`g1`、`g2`…）再列项目规则（`1`、`2`…）和两份文件的位置；`add` 添加一条项目规则，`add global` 添加一条全局偏好；`remove <n>` 删项目规则，`remove g<n>` 删全局偏好；`promote <n>` 把第 n 条项目规则提升为全局偏好（先写全局，成功后才从项目删掉）；`pending` 列出等待确认的偏好候选，有终端时逐条询问；`local` 把规则文件加进 `.git/info/exclude`，只留在本机 |
| `/cd <路径>` | 保存会话后以新目录重启本程序（工作区、租约、检查点、工具和项目上下文都绑定 cwd，重启是最安全的切换方式）；原会话留在原目录，可在那里 `--resume` |
| `/restart` | 保存后用原参数加 `--resume <id>` 重新启动本程序（Unix 为 exec，Windows 启动子进程并等待其退出码） |
| `/expand [n] [all]` | 工具结果在终端只显示摘要（交互控制台）或前 12 行（`-p`、管道），并带 `#n` 标记；本命令显示第 n 个（默认最近一个）的完整输出，默认最多 200 行，`all` 显示全部。内存中只保留最近 50 个 |
| `/clear` | 清屏并清除回滚缓冲区；输出不是终端时拒绝 |
| `/doctor [quick\|errors\|trace [N]\|codes\|report]` | 环境诊断、错误记录、交互轨迹及本地报告，见下文 |

诊断 `/doctor` 检查：配置文件能否加载及警告、占位或掩码的 API Key（`sk-xxxx`、`****`）、API Key 是否存在（只显示首尾各 4 位）、provider/模型/base_url、网络可达性（对 base_url 发 HEAD，任何 HTTP 响应都算可达；`quick` 跳过）、git 与 ripgrep、shell（Windows `powershell.exe`，其他 `sh`）、数据目录可写与权限、`policies.json` 与扩展（Hooks/MCP/插件）配置加载错误，以及最近的错误记录。失败项带诊断码和处置建议；`/doctor codes` 列出全部诊断码。命令行 `--doctor` 在**没有可用配置或 API Key 时也能运行**（这正是它要诊断的情况），有失败项时退出码为 1。

- **错误日志** `<数据目录>/errors.log`：模型调用失败、任务中断原因、命令错误和启动失败各记一行 JSON（时间、来源、诊断码、最多 400 字节的消息，API Key 被替换为 `****`）；超过 1 MiB 轮换为 `errors.log.1`。`/doctor errors` 按诊断码和消息分组显示次数、最近时间和建议。
- **交互轨迹** `<数据目录>/trace.jsonl`：每次模型调用、工具调用、上下文压缩和任务结束各记一行，主要记录大小、数量、耗时和结果（每一行都有 `build`，是写这一行的构建，错误日志的每一行也有；2026-11-24 之前的记录没有这个字段；模型行的 `model` 是实际作答的模型，子 Agent 的调用另有 `agent` 字段记它的 id，2026-11-24 之前的记录 `model` 一律是配置的主模型名；任务行的 `model_calls` 是会话累计的调用次数，`task_calls` 是这一次运行的调用次数，含压缩和子 Agent；`idle_max` 是最长连续没有进展的轮数，`stalled` 表示因此停下；模型行的 `cache_read` 是其中从提示缓存读取的 token；`/handoff` 记一行 `kind=handoff`：是否成功、交接前后的上下文估算、收集到的文件和提交数，不含正文）；失败工具的首行输出及模型错误摘要也可能被记录。已知 API Key 会被替换，但无法保证日志不含其他敏感信息，**不要直接外发原始日志**。超过 4 MiB 轮换一次为 `trace.jsonl.1`；`/doctor trace [N]` 显示最近 N 条（默认 30）。错误日志同样可能包含敏感信息，大小轮转不是按时间过期，当前没有自动按时间清理。
- **支持报告**：`/doctor report` 本地预览版本（带构建标识和构建时间）、系统类型、最近最多 2000 条错误的已知诊断码汇总及最近最多 2000 条轨迹中的压缩截断次数；只使用固定字段和内置建议，不复制原始错误、路径、会话或工具输出。报告是有限窗口的统计，不代表完整历史。`/doctor report export <文件>` 经写入授权后生成文件，不覆盖已有文件；导出时重新生成，请检查导出的内容并按需编辑。报告不会自动外发，当前没有邮件发送功能或预置邮箱。
- **可选模型分析**：预览会显示供应商、模型、端点主机及确认摘要；核对后运行 `/doctor report analyze confirm <完整摘要>`，才会按网络权限向指定模型发送报告，产生一次可能计费的调用并记入会话用量。摘要绑定报告、供应商、完整端点和模型；变化后需重新预览。模型回答仅作未验证建议，不使用工具、不自动执行。无确认时不发请求；不要把原始日志自行拼入报告后发送，且分享前仍应人工检查。

命令行：

- `-p` 会把**管道传入的 stdin** 附加到提示词后（`cat app.log | cove -p "解释"`，也可只用管道 `cove -p < prompt.txt`）：最多 8 MiB，去掉 PowerShell 写入的 UTF-8 BOM，截断时不切断多字节字符并给出警告。stdin 是普通文件或 stderr 是终端时一直等待数据；否则（IDE、CI、其他 Agent 启动且从不写入的 stdin）最多等 3 秒首字节，超时警告并忽略。
- `--file <路径>`（可重复）把不超过 256 KiB 的 UTF-8 文本文件作为数据附加到 `-p` 提示词；`--image <路径>`（可重复）附加 PNG/JPEG/GIF/WebP 图片，处理方式与 `@路径` 附件相同（缩放到 ≤1568 px、≤5 MB）。
- `-r` 是 `--resume` 的简写；`--dump-system-prompt` 打印按当前目录生成的完整系统提示词后退出（恢复的会话沿用自己记下的项目上下文，见“历史”一节）。
- **构建标识**：`cove --version`、`/status` 的最后一行和 `/doctor report` 的“版本”显示 `<版本号>+3fa9c1d2e4b7（构建于 2026-11-24 10:00 本地时间）`。`+` 后面是构建时对 `src/`、`assets/`、`marketplace/` 下的全部文件和 `Cargo.toml`、`Cargo.lock` 按路径排序后算出的 sha256 前 12 位：这些文件的内容不变，哈希就不变（只更新修改时间不算改动）；`docs/`、`tests/` 不参与。版本号不随每次改动变化，区分两次构建看哈希。trace、错误日志、验收报告里的 `build` 和会话里的 `builds` 记的是 `<版本号>+<哈希>` 这一段，分析本机数据时可以按它筛选；没有这个字段的旧记录来自 2026-11-24（0033）之前的版本。
- 退出码：`0` 成功，`1` 失败，`2` 参数错误（含 `-p` 缺少提示词），`130` 被 Ctrl+C 中断。

### 历史

- 标题取第一条真实用户请求（跳过系统通知和“继续”这类短输入），最多 60 个字符，保存在会话文件里，压缩后也不丢失。
- **接着上次做**：启动时会提示当前工作区最近有实际对话的会话及其最近任务；在新会话输入“继续”会恢复同一会话。`/continue` 列出当前工作区的会话并允许选择其他会话（包括较早的未完成任务）；`/continue <编号或 ID>` 直接选择。未完成任务会续跑，已完成任务恢复完整上下文后等待新消息。结果不确定的工具调用会作为失败结果交回 Agent 检查，不会自动重放。新会话的第一条消息与某个**未完成**任务的原始请求相同时，也会续跑那个任务，而不是重新开始。
- **恢复后能看到之前的对话**：`/resume`、`--resume`、列表选择、`继续` 恢复会话时，在终端输出最近 3 轮对话；模型仍保有完整上下文。已压缩的早期内容不在当前上下文中。最后一轮没做完（被中断，或有未确认的工具调用）时，回显末尾注明"这一轮没做完"：输入 `继续` 接着做它，要做别的事先 `/new`。`/restart` 会自动恢复会话，所以退出时不再提示"下次在这里输入'继续'"。
- **恢复后沿用原来的系统提示**：会话在第一个任务开始时记下系统提示里的项目上下文（环境行、用户偏好、项目说明文件、项目概要、技能列表）。之后在新进程里恢复它（`--resume`、`/resume`、`/restart`、`继续`）时，请求沿用记下的那一份，系统提示和上一个进程逐字节相同，对话历史的提示缓存还能读到；期间新增了文件、切换了分支，项目概要和分支信息不更新（模型可以用 `repo_map`、`git` 查到最新情况）。用户偏好或项目说明文件（AGENTS.md、CLAUDE.md、`.cove/rules.md` 等）变了时，下一个任务的消息里附一段 `[project context]` 说明，写出变化后的内容（项目说明合计超过 16 KiB 时只列文件名，让模型自己读），同样的变化只说一次。压缩之后、`/new` 和 `/handoff` 开出的新会话改用当前算出的项目上下文。`/trace` 的任务记录带 `system_reused`：这次运行是否沿用了与本进程不同的旧上下文。
- 交互终端使用普通屏幕和终端自身的滚动、选择与复制功能，不再接管鼠标或折叠对话。选择和粘贴的快捷键取决于所用终端的设置。管道和重定向仍使用普通文本输出。
- 启动信息显示版本、模型与供应商、权限模式、工作目录、git 分支和会话 ID 前 8 位；退出时提示下次如何继续（输入 `继续`，或 `cove --resume <id>`）。
- `/resume` 默认只列当前工作区，显示更新时间（本地时间）、状态、对话轮数、标题和 ID 前 8 位；`/resume all` 列出的其他工作区会话，选中后自动在那个目录重启并恢复。
- `max_sessions`（配置根键，默认 200，≤0 关闭）：启动时按更新时间保留最新的会话，至多每 10 分钟清理一次，从不删除当前会话或其他进程正在使用的会话；同时清除没有对应会话、超过 1 小时的归档、文件检查点和锁文件。有会话文件无法读取时不清理检查点。
- `/resume clean` 把无法读取的会话文件移到 `<数据目录>/backups/clean-<时间>/`（不删除），并为缺标题的旧会话补标题（改写前先备份原文件）。正在被其他进程使用的会话跳过。
- `/resume export [file]` 导出 Markdown：会话信息、每条消息、工具调用参数和（截取 8 KiB 的）工具结果。默认写到 `<数据目录>/exports/<id>.md`；指定的相对路径按工作区解析，写入需授权。

### 费用账本

- 模型调用（任务、压缩、子 Agent、后台记忆提取、dream）在 Provider 边界记录到 `<数据目录>/usage/YYYY-MM.jsonl`。账本记录时间、供应商、**实际作答的模型**（后台调用是快速模型）、会话、来源（`session` 主任务、`agent` 子 Agent、`background` 后台调用；2026-11-24 之前子 Agent 记为 `session`）和供应商报告的 token，`/status cost` 在有多个来源时多打一行“按来源”；费用与会话统计用同一规则：供应商报告了费用就用报告值，否则按该模型的价格分别计算普通输入、缓存读取和缓存写入（2026-11-20 之前的记录把缓存读取按全价计算、后台调用按主模型计价，费用明显偏高，未回改）；按 UTC 月份分文件，保留 13 个月。具体断电持久性取决于操作系统和文件系统。
- `/status cost` 默认统计本月；也可用 `YYYY-MM`、`30d`、`24h`、`today`、`all`。价格未知的调用显示为 `unknown`，合计里标成 `>=`，不当作零。`export [file]` 写 CSV，默认 `<数据目录>/usage/export-<时段>.csv`。
- 是估算，不是供应商账单。

### Git 快照与 `/undo`

- 每个任务第一次改动之前（标签是本次请求），以及每次非只读 Shell、写模式子 Agent、`execute_plan` 之前（标签写明命令和所属任务），把整个工作区快照到私有的 Git 仓库 `<数据目录>/checkpoints/git`；同一任务里之后的 `write`、`edit`、`draw_image` 不再快照，它们之间的回退由文件工具检查点负责。快照的位置：每个项目一个 ref、一个独立 index，不碰项目自己的 `.git`；工作区没变化时不新增快照。排除规则由项目本身推导，每次快照前更新（结果复用 30 秒）：
  - 项目自己的 Git 忽略的一切：`.gitignore`、`.git/info/exclude` 和全局 `core.excludesFile`（快照用的是独立仓库，单靠它只能看到 `.gitignore`，所以改为直接询问项目的 Git）；
  - 项目 Git 未跟踪的隐藏（`.` 开头）文件和目录，如索引、缓存、IDE 设置（`.codeatlas/`、`.vscode/`、`.env`）；项目跟踪的 `.github/` 等照常快照；
  - 项目 Git 未跟踪且大于 2 MB 的文件（数据库、数据转储、模型等；与 OpenCode 相同）；
  - 项目 Git 未跟踪、且其中超过 2000 个文件的目录（整批放进来的 SDK、数据集等）；
  - 不在 Git 项目里时，所有隐藏文件和目录，以及大于 2 MB 的文件；
  - 项目根目录 `.coveignore` 里的补充规则（语法同 `.gitignore`），需要时自己加，无需改代码；
  - 另有固定排除：`.git`、`target/`、`node_modules/`、`dist/`、`build/`、`.venv`、`__pycache__` 等。
  以前快照进去、现在被排除的条目会从快照索引中移除；还原旧快照时也不会触碰这些路径。只读权限模式下不做快照。需要本机安装 `git`；没有时只剩文件工具回退。
- 工具调用最多等快照 10 秒：超时则本次调用不做快照照常执行，结果里注明 `/undo` 无法还原这次的效果；已开始的暂存在后台继续完成（不记录快照），之后的快照就快了。后台暂存期间的调用同样跳过快照、不计为超时。同一会话两次超时后，本会话不再自动快照。大项目第一次快照可能需要几十秒（之后通常 1 秒左右）。
- 快照按原始字节存储（`* -text`、`core.autocrlf=false`），还原时一字不差，项目的 `.gitattributes` 和换行设置不会影响它；快照用的 Git 不读用户的全局和系统配置（`core.autocrlf`、`commit.gpgSign`、钩子模板等都不会带进来）。暂存时文件内容写进一个打包文件而不是逐个小文件，大项目第一次快照约快一半。
- 每个项目保留最新 50 个、且不超过 30 天的快照：超过 60 个，或最旧的超过 33 天时修剪，后台 `git gc` 只回收 1 小时前的对象（单线程、不对大文件做增量压缩，避免占满机器）。任何项目修剪时，顺带删除最新快照也超过 30 天的其他项目（不再打开的项目不会自己修剪）；打开快照库时，若有超过 1 小时、尚未回收的 cruft pack，就在后台跑一次 `git gc`。`/init apply` 写入前也会拍一个快照。cove 退出时遗留的过期 index 锁会在下次快照前清除。
- 快照还原**包括 Shell 改动**（被忽略和排除的目录除外）。没有 Git 快照时（未安装 Git、工作区是主目录或根目录、快照超时被关闭），`/undo` 退回文件工具回滚：只回退上一个任务文件工具的修改，**不含 Shell 改动**。
- 转到后台的 Shell 命令（`timeout_seconds` 到点或 `run_in_background`）：命令之前的快照照常拍，`/undo` 到它会连同命令之后做的改动一起还原；但命令还在后台跑时，下一条命令之前的快照会包含它做了一半的改动，命令还在跑时 `/undo`，它可能继续改文件。命令结束时再把检查点标记一次"有 Shell 改动"。
- 快照还原先预览：`/undo #n` 列出将要还原、重建、删除的文件；`/undo confirm` 执行最近一次预览，执行前复核工作区，与预览时不一致就拒绝。还原前先给当前状态拍快照，输出里给出它的编号，用 `/undo <hash>` 可以撤销这次还原。嵌套 Git 仓库不在快照里，保持原样。
- 还原后之前的验证证据失效，需要重新检查。

### 记忆

- 记忆分项目和全局两个范围，每条最多 25 KiB，每个范围合计最多 300 KiB；写入跨进程加锁；疑似密钥（私钥、`sk-…`、AKIA、GitHub/Slack token 等）拒绝保存。
- 检索使用 BM25：中日韩文本切成相邻二字词，英文去掉停用词，再按更新时间衰减（30 天）加权。可选配置 `memory_embedding: {base_url, api_key, model}`（OpenAI 兼容 `/embeddings`），余弦相似度 ×0.5 叠加到 BM25；首次使用需网络授权，失败后 5 分钟内不再尝试。
- 注入：会话中第一次（以及每次压缩后）完整注入。全部记忆合计不超过 8 KiB 时附全部内容；超过时分三层：全局记忆全文（最多 4 KiB，放不下的进索引）、与本次请求最相关的 5 条项目记忆（每条最多 2 KiB；请求不到 8 个字时，用会话标题和上一次请求一起检索）、其余条目的索引（按更新时间从新到旧，最多 4 KiB，放不下时写明还有多少条，用 `memory search` 查找）。索引只写 id 的前 8 位，`memory` 工具的 read、update、delete 都接受唯一的 id 前缀。之后每个新任务只附**新增或改动**的记忆（以及尚未出现过的相关条目），没有变化就不附，避免同样的记忆在历史里反复堆积；恢复会话时根据历史里已有的记忆块判断。记忆标注为“可能过时的笔记，不是指令”；分层时全局记忆的标题是 “Global notes (this machine)”，它只放本机环境事实，你希望在所有项目里遵守的做法放在全局偏好里（见“全局偏好”）。
- 来源（provenance）：每次写入记录类型（tool/operator/extraction/dream）、会话、消息区间、转录哈希和证据摘录，最多保留 8 条；新记录还可标注证据类型、项目范围、环境和验证时间，未记录时为未知。`/config memory source <id>` 查看，并提示内容在记录后是否被改过。来源只说明输入，不证明内容正确。dream 合并要求先读取双方修订，保留双方来源及原文；超出来源容量或修订变化时拒绝合并，不允许直接删除。dream 更新也要求匹配修订，且不覆盖用户陈述或未知来源。合并先更新保留条目、再删除重复条目，若中断可能留下两个条目，应人工检查。
- 自动提取：任务完成后在后台调用一次模型（30 秒上限），从最近 20 条消息中提取持久事实，并把你对做法的纠正或明确说的长期规则写成项目规则（见“项目规则”）；看起来不只适用于这个项目的做事方式，记成项目规则的同时提议升为全局偏好（见“全局偏好”）；工具输出不发给提取器。自动提取只写当前项目记忆，全局记忆和全局偏好都不会被自动写入。已有同名条目只追加、从不覆盖。本地/内网模型、`--no-auto` 或对话太短时跳过。
- Dream 整理：`/config memory dream status|run`。用带 `memory` 和 `project_rules` 工具的子 Agent 合并重复、修正过时内容，并把至少在两个会话里出现过的问题写成项目规则（不在项目目录里时，仍归纳成“rules and patterns”记忆），只能写当前项目记忆，全局记忆只读，5 分钟上限；运行前把记忆备份到 `<数据目录>/memory/backups/`（所有项目合计保留 5 份）。`dream backups` 列出有当前项目归属的新备份，`dream show <备份>` 查看条目；`dream preview <备份> <完整id>` 展示当前值和备份值，再在同一进程中使用 `dream restore <token>` 授权恢复单条项目记忆。备份或当前条目在预览后改变时拒绝恢复，不删除其他记忆；令牌一次有效，重启或新预览后失效。旧备份没有项目归属清单，不能通过此命令恢复；全局记忆不能通过此命令恢复。
- 恢复意图和结果记录在 `<数据目录>/memory/restore-audit/`：`started` 表示中断后结果未知，`unknown` 表示写入结果不确定，需先检查当前记忆；不会自动重试。这些审计文件目前不自动清理。备份记录输入来源，不保证其内容正确。
- Dream 配置 `<数据目录>/dream.json`，默认 `enabled: true`、`trigger: session_end`、`min_turns: 2`，`threshold` 模式 12 小时且 3 个会话；本实现在会话达到 `min_turns` 后的某次任务结束时在后台运行一次，进程退出时取消。文件格式错误时关闭自动运行，避免按错误配置执行。

### 项目规则

- 规则文件是 `<git 根目录>/.cove/rules.md`，不在 git 里时用工作区目录；工作区是家目录或盘符根时不启用。文件跟着仓库走：提交后，换电脑、换人都能用；不想提交时用 `/rules local`。
- 每条规则以 `- ` 开头，一句话写明不要做什么、改为怎么做，末尾带日期。可以直接编辑这个文件；cove 只认 `- ` 开头的行，保留你写的其他内容。每条最多 300 字，最多 60 条；疑似密钥拒绝保存。
- 来源：
  - 你对做法的纠正，或明确说的长期规则（自动提取，一次就记）；
  - 至少在两个会话里出现过的“问题 → 尝试 → 失败 → 解决”（dream）；
  - `/rules add`。
  新规则写入后立即生效，不需要审批；后台记下的规则会在下一个输入提示前显示一行。
- 生效：规则文件是项目说明文件之一，和 AGENTS.md 一起完整放进系统提示，不按相关性挑选，安全上也同等对待（是项目方的指令，不能越过安全规则）。会话中途新增的规则，在下一个任务开始时告诉模型。提交前审查和自审每次都读取最新的规则文件。

### 全局偏好

- 文件是 `<数据目录>/preferences.md`（默认 `~/.cove/preferences.md`，跟随 `--data-dir`），格式和项目规则一样：每条以 `- ` 开头，末尾带日期，可以直接编辑。每条最多 200 字、最多 20 条，超出时拒绝写入并提示整理（不截断）；疑似密钥拒绝保存。
- 生效：每个项目的系统提示里，在项目说明之前有一节 “User preferences”，写明项目说明（AGENTS.md、CLAUDE.md、`.cove/rules.md`）和你当前的要求优先于它。优先级从高到低：当前请求 > 项目说明 > 全局偏好 > 记忆。超过 8 KiB 时截断并标注。子 Agent 带同一节；会话中途新加的偏好在下一个任务开始时告诉模型。提交前审查和自审**不**按全局偏好检查。
- 只有你能写入：`/rules add global`、`/rules promote <n>`，或者确认 cove 的提议。dream 只读，提示词里列出已有的偏好，避免再写成项目规则。
- 提议：自动提取认为你说的是和具体项目无关的做事方式（工作流程、改动方式、沟通、文档写法、核实方式），而且没有提到这个项目的目录名、远端仓库名或绝对路径时：
  - 立刻记成当前项目的规则（当前项目马上生效，不需要确认）；不在项目里时只排队；
  - 同时排进 `<数据目录>/preferences.pending.json`，在交互会话下一个输入提示前问一句：`[偏好] 这条要求看起来不只适用于这个项目 … y 记成全局偏好 · n 只留在本项目 · 回车 以后再说`。`y` 写进全局偏好并删掉项目里的同一条；`n` 留在项目规则里，以后不再问同一条；回车下次启动再问；输入的是别的文字时，这一条同样下次再问，这句话照常作为任务执行（显示 `[偏好] 以后再问（/rules pending 可以处理）；刚才输入的内容作为任务执行。`）。
  - 只在任务结束、等待输入、没有插话排队时问；两次任务之间最多 1 条（回答后按回车或执行斜杠命令都不会接着问下一条），每次启动最多 3 条；`/rules pending` 主动查看时逐条问，输入别的文字时剩下的不再问、这句话作为任务执行；同一条问题在一个窗口里问着时，其他窗口不问（退出时放回队列，异常退出 30 分钟后放回）；30 天没处理的候选清掉（它一直是项目规则）。
  - `-p`、管道输入这类没有终端的运行不问，结束时如果新排了候选，在标准错误打印 `[偏好] 有 N 条候选等待确认，下次交互运行 cove 时会询问；或用 /rules pending 查看`。
- 迁移（0030）：启动时把全局记忆里标题是语言偏好的条目（如“用户语言偏好”）移进全局偏好：先把原条目复制到 `<数据目录>/backups/0030-global-preferences/<id>.json`，再写入偏好，最后删除记忆条目，并显示一行说明。每个数据目录只做一次（该目录下的 `migrated` 文件记录了移动了什么）。内容超过 200 字的不移动，提示用 `/rules add global` 自己写。

## 上下文与回退边界

- 上下文按估算 token 计算（ASCII 约 3 字节 1 个 token，其他字符 1 个）。交互启动时可探测支持 `/props` 的本地服务；否则使用已学习的窗口、旧配置记录或内置模型表。服务端超限错误报告窗口时会自动学习并缓存，但并非所有服务都会提供该信息；可用 `context_window`（或 `--context-window`）显式指定。上下文预算最多为窗口的 85%，同时为回复和安全余量留出空间。估算值按服务端上一次报告的实际输入 token 数校准（比例限制在 0.5–3 倍）。超过预算的 90% 或 400K tokens（取较小者；实测超过约 400K 后每次响应明显变慢）时自动压缩，摘要同样收费并占模型调用额度。上一次对话请求还在提示词缓存有效期内（5 分钟；模型支持 1 小时缓存且开启时为 1 小时）、加上摘要请求仍在预算内、模型也没换时，摘要在原对话上续写：系统提示、工具定义和全部消息原样发送，末尾追加一条“总结前面若干条”的请求，用上一个任务的模型，输入几乎都按缓存读计费；回复里有工具调用或为空就算失败，不改上下文，自动压缩时改用独立请求重试一次，`/compact` 直接报错。不满足这些条件（或服务端刚报过超长）时用独立请求：把旧消息作为数据交给 `model_fast`（未配置时用主模型），这个请求不打缓存标记。完成提示里写明读到的缓存 tokens，`/trace` 的压缩记录带 `mode`（`continuation` 或 `standalone`）；压缩后保留的最近消息不超过阈值的一半。离阈值还有 60K tokens（小阈值时为阈值的 1/6）时，摘要先在后台写；到达阈值时，如果被摘要的那段消息没有变化，就直接用它，不用停下来等，否则照常现场压缩。后台摘要的费用在使用或放弃时计入会话；进程在它完成前退出时，这次调用的费用无法计入。旧的 `--context-bytes` 仍可用。
- 保留当前需求原文、近期完整消息/工具交换和相应 thinking signature；更早历史归档后摘要化。不会保留所有历史决策原文，必要时检查归档。压缩摘要的输出上限为配置的 `max_output_tokens` 与 8192 中较小者；实际输出和思考用量由供应商计费，不会自动重试。摘要失败、不完整或不能缩小时，保留原上下文并明确停止；截断时显示实际输出上限，手动压缩可用 `/compact` 重试，中断任务可用 `/continue`。
- 最新不可拆分交换本身过大时明确失败，不静默截断需求。会话超过 `max_sessions` 时自动清理最旧的会话及其归档。
- 单个工具结果按模型窗口限长：read/grep 约 6000 token，shell 和网络工具约 3000，其他约 4000；窗口大于 128K 时按比例放大（最多 4 倍，且不超过窗口的 1/8）。被截断的 read 结果末尾给出正确的 `[next: offset=N]`。
- 旧工具输出外置：最近约 50K token（不随窗口放大）之外的旧工具输出累计超过 30K token 时，移到数据目录的 `tool-outputs/`，历史中只留一行占位说明文件位置（可用 Shell 查看）；`question`、`todo` 等计划类结果不外置。重复出现的相同结果只保留最新一份，较早的改成指向它的说明。外置文件 7 天后自动清理。
- 工具访问到工作区子目录时，会把该目录及其上级（最多 5 层，不超过工作区根）的 `AGENTS.md`、`CLAUDE.md`、`.cursorrules`、`.cove.md` 附在结果后，标注 `[Context from <路径>]`；每个文件只附一次（压缩后的新任务重新附），单文件 12 KB、单次 24 KB 封顶。Shell 命令中的路径也会识别。
- 每个新任务建立一个检查点，最多跟踪 100 个文本文件；重复编辑保留本轮首次修改前的原件。回退前检查全部文件，再逐文件复核并记录进度。
- 文件工具回滚（没有 Git 快照时的 `/undo confirm`）只还原 **文件工具**产生的变化，保留已有未提交改动和用户后续修改，**不回滚 Shell、Git、网络或外部进程副作用**；工作区内的 Shell 文件改动可用 Git 快照还原（见上文），工作区外的副作用和网络请求都无法撤销。回退后原验证证据失效，需要重新检查。
- 外部进程并发改文件不受锁控制；预检不能保证整批原子事务。回退过程中发生漂移或崩溃时保留明确记录，可能需要人工核对，不盲目覆盖。

## 权限与限额

| 参数 | 行为 |
| --- | --- |
| `--permission default` | 需要审批时手动选用（旧名 `ask`，对齐 Copilot 默认姿态）；`read`/`grep`/`glob`/`repo_map` 不需要审批；Shell 命令按“会改动什么”判断，什么都改不了的直接运行（读取、构建、测试、项目脚本、本地 git 操作、参数普通的陌生工具，可与只读命令串联），会改文件或删除、推远端、装软件、改系统设置和服务、结束进程、执行读不到内容的代码（`eval`、`python -c`、`bash -c '...'`、`find -exec`、程序名来自变量等）才审批，并写明原因；项目内的文件修改直接放行；`web_fetch`/`web_search` 只读取，不需要审批；项目外写入和扩展进程逐次审批，没有终端则明确失败 |
| `--permission plan` | 只读（旧名 `readonly`）；允许阅读、搜索和只读 Shell 命令，其他操作拒绝，模型收到拒绝说明后可以继续 |
| `--permission auto` | 在 default 的 Shell 判断之外，另外自动放行网络工具和已信任的扩展进程，以及能用 `/undo` 恢复的删除：整行只有 `rm`/`unlink`（PowerShell 为 `Remove-Item`/`del`/`rm`）删除字面量写出的文件路径（可用 `&&`、`;` 连接，也可以带上 `ls`、`git status` 这类只读命令，但不能有 `cd` 等换目录的命令；只允许 `-f`、`--`、`-Force`、`-Path`/`-LiteralPath`），每个文件都是工作区内、`.git` 之外的普通文件（不是目录或链接），并且都在这次调用之前刚打成的 Git 快照里，结果末尾注明“可用 /undo 恢复”。递归删除目录、通配符、变量、`~`、命令替换、被快照排除的文件（Git 忽略的、未跟踪的隐藏文件、大文件、`.coveignore` 里的）、快照没打成或已关闭时，照常询问。其他仍然需要审批的 Shell 命令（改文件、推远端、装软件等）照常询问；无终端时不执行这次调用，并告诉模型 |
| `--permission bypass` | **默认**（没有命令行参数、配置里也没写 `permission_mode` 时）；除拒绝规则和灾难命令外全部允许，不询问 |
| `--max-turns 20` | 每个任务/继续窗口最多 20 次模型请求，含自动压缩，不是用户对话数 |
| `--timeout 300` | 任务总时限，秒；达到后取消并有界清理 |
| `--max-output-tokens 4096` | 每次模型响应的输出 token 上限 |
| `--input-price` / `--output-price` | 每百万 token 的美元价格 |

`-p` 永不读取审批输入，也不会注册提问工具；不要通过管道传入 `y` 来绕过审批。没有终端可以询问时（`-p`、管道输入、后台 agent），需要审批的调用**不执行**，模型收到"没人能批准、不要重试同类命令，换不需要审批的做法，或在最终回复里写明你要手动做什么"的说明后继续；stderr 上每个这样的调用显示一行"未执行（没有终端可以询问）"。它不计为工具失败，退出码由任务本身的结果决定。`auto` 只放行能用 `/undo` 恢复的删除（见上表；`git rm` 等其他删除被拒绝时，说明里会附上哪种删除可以直接执行），仍会询问其他改文件、执行代码等命令，要让这类命令在无人值守时运行，用 `--permission bypass` 或在 `policies.json` 加允许规则。

被拒之后不能用脚本绕过（[0038](optimizations/0038-no-script-bypass-after-refusal.md)）：`default` 和 `auto` 下，一个任务里有调用因为没有终端而没执行，或者你在审批里选了拒绝（含远程审批拒绝），这个任务后面运行本任务文件工具（`write`、`edit` 等）新建或修改过的文件就要审批，无终端时同样不执行。认得的写法：`python`/`python3`/`py`、`node`、`deno run`、`bun`、`ruby`、`perl`、`php`、`bash`/`sh`/`zsh`、`source`/`.` 后面的脚本，`pwsh -File a.ps1`、`pwsh a.ps1`、`cmd /c x.bat`，以及按路径直接运行的 `./x.sh`、`.\x.ps1`；`bash -c`、`cmd /c`、`pwsh -Command` 里面的命令和它前面字面写出的 `cd` 也算在内。`python -m` 的模块、`-c`/`-e` 的内联代码不算运行文件（它们按下一段的规则判断）。项目里原有、本任务没改过的脚本照常运行；`policies.json` 或“本次会话不再询问”的允许规则仍然放行；新任务开始时重新计算。这类拒绝的说明里会写明“运行本任务新建或修改的脚本、python -c 这类内联代码同样需要批准”。

内联代码（[0043](optimizations/0043-python-c-in-print-mode.md)）：`auto` 下，代码完整写在命令行里的调用按本任务写的脚本对待，本任务没有调用被拒过时直接运行，被拒过之后要审批（无终端时不执行）。包括 `python -c`、`python - <<'PY'`、`python <<< '…'`、`node -e`/`-p`、`bun -e`、`ruby -e`、`perl -e`、`php -r`、`deno eval`、`Rscript -e`、`lua -e`，以及从 here-document 读代码的这些解释器；PowerShell 里的 `python -c '…'` 同样适用。仍然要审批的：同一行还有别的需要审批的原因（重定向写文件、`git push` 等）；代码来自命令行以外（`curl … | python -`、`cat a.py | python -`、`python -c "$(…)"`）；代码里有变量展开（`python -c "…$X…"`、定界符不带引号且正文有 `$` 的 here-document）；`perl -i`（原地改文件）；`osascript -e`；PowerShell here-string（`@'…'@ | python -`）。`default` 下内联代码仍然要审批。另外，`default` 和 `auto` 下定界符不带引号的 here-document 正文里有 `$(…)` 或反引号时都要审批（shell 会先执行它们）；`bash <<'EOF'` 这类喂给 shell 的 here-document 按 `bash -c` 一样判断里面的命令。后台 Agent 和竞速方案按各自的一次运行计算。

cove 统计并显示费用，但不设花费上限，也不会因费用停止任务。OpenRouter 官方端点优先使用响应中的 `usage.cost`；自带上游密钥时，上游单独收取的费用可能不包含在内。其他情况用报告的 token 和已知单价估算。缺少费用、请求失败、中断或崩溃时，未能确定的单次费用按零计入，已知金额不丢失，因此累计可能低估，实际费用以供应商账单为准。旧会话历史费用若已未知则仍显示未知。

文件工具限制在解析后的工作区内，拒绝父目录穿越、外部链接目标、直接访问 Git 元数据和 Windows ADS；这不是对恶意并发文件系统变化的沙箱。Shell 具有宿主权限，可能修改工作区外的数据、访问网络或启动程序。授权前务必检查命令。

### 权限模式与规则

判定顺序：灾难命令拦截 → 拒绝规则 → plan 模式（只放行只读操作）→ bypass → 允许/询问规则（按优先级，同级时询问优先）→ 模式默认。

- **命令分类**：Shell 命令先按它将要运行的 Shell 语法切分（命令明显是给另一种已安装的 Shell 写的就按那种：PowerShell cmdlet、cmd 的 `%VAR%`、bash 循环；否则 Windows 上按 bash/PowerShell，其他平台按 sh），处理引号、转义、管道、`&&`/`||`/`;`、重定向和 here-doc，再逐条给出“会改动什么”的原因：写入或删除文件、改远端、装软件、改系统设置或服务、结束进程、执行读不到内容的代码、提权，整行合并去重；原因列表为空就直接运行，非空则需要审批并显示这些原因。Windows `certutil -hashfile` 和常见摘要工具按只读命令处理。命令替换 `$(...)`、反引号、PowerShell 脚本块 `{...}`、调用运算符 `&`、写文件的重定向、隐藏字符等无法静态判断的写法一律需要审批。
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

- `tool_pattern` 是工具名 glob：`shell`（也认 `bash`、`powershell`）、`write`/`edit`、`web_fetch`/`web_search`、`mcp`/`hook`。`command_prefix` 按词匹配；`command_group` 是常规操作组（`git` 含 add/commit/push/pull/switch 等但不含 `--force`、reset、clean、checkout；另有 `cargo`、`npm`、`go`、`dotnet`）；`command_exact` 要求 Shell 命令整行完全相同；`command_directory` 将确认范围限制为指定目录下程序的 `--help` / `--version` 只读查询；`command_executable` 将确认范围限制为指定原生程序路径；`command_endpoint` / `command_output` 将确认范围限制为本地模型推理端点和同一个输出文件；`param_match` 按字段 glob（`command`、`path`、`url`）；`input_equals` 要求字段完全相等；`scope` 限定项目根目录；`enabled: false` 停用。允许规则必须覆盖整行的每一条命令才生效，拒绝规则只要命中其中一条（含 `bash -c` 内层文本）就生效。
- **记住本项目 / 本次会话**：选 `a` 会向 policies.json 追加一条限定本项目的允许规则，选 `s` 生成同样的规则但只保存在本进程内存里（常规命令记成组或“程序 + 子命令”前缀；只读帮助/版本查询可记住同一目录；其他复合命令只记住整行；本地模型完成请求可变更 JSON 提示内容，但限制在同一 loopback 端点与同一结果文件；明确路径的原生程序可记住同一个程序路径，后续参数可变，但不覆盖管道或重定向）。含动态变量或解析不确定的命令不会生成命令族规则；无疑似密钥且长度不超过 8 KiB 时，可以选择只记住完全相同的命令行。这种"完全相同的命令行"规则在 policies.json 里最多保留最近允许的 100 条，更早的自动删除（几乎都是一次性的命令）；再次允许已有的同一条时，它算作最新。
- 拒绝规则和灾难命令被拦截时，模型收到拒绝说明并继续；你在审批里拒绝时同样如此（见“交互控制台”）。需要询问但没有终端时（如 `-p`）也一样：这次调用不执行，模型收到说明并继续（见上文 `-p` 一段）。

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
- **循环检测**：同一修改类调用在最近 14 次里出现 10 次、同一工具换参数在最近 12 次里出现 10 次（输出或目录在变化时不算）、同一输出在最近 40 次结果里出现 8 次，都会拒绝该调用（同一输出只附提醒）并给出换思路的提示。读类工具不参与循环检测。熔断和循环检测只管拦下这一次调用，被拦下的调用算作“没有进展”，任务要不要停由下面的止损决定。
- **Shell 结果后的提示**：Shell 命令在做文件工具该做的事（`sed -i`、`cat >`/`tee` 写工作区文件、`sed -n` 读行区间）时，结果后附一条改用 edit/write/read 的提示；下载模型（`huggingface-cli`/`hf download`、`modelscope download`、`ollama pull`、`git lfs pull`，或 `curl`/`wget`/`aria2c` 下载 `.gguf`、`.safetensors` 等模型文件）时附下载规则（确认文件和大小、单个可续传传输、校验后改名、网络反复失败就停）；构建或测试命令的输出接到 `| tail`、`| head` 时提示不要截断实时输出。每类每个任务只提示一次。这些场景很少出现，所以不写在每次请求都发的系统提示里（见 [0044](optimizations/0044-slim-system-prompt.md)）。
- **按有没有进展止损**（`stall_rounds`，默认 12；见 [0032](optimizations/0032-stuck-detection.md)）：一轮是一次模型回复加它的全部工具调用；只回文字、被收尾催促送回的回复，自动验证失败后交回模型的那次，和审查问题交回模型的那次，也各算一轮。一轮里只要有一项就算有进展：`edit`/`write` 写出了本任务没写过的内容（来回改成以前的样子不算）；成功的调用拿到了没见过的结果（按工具、主参数和结果判断，结果里的数字、时间、7 位以上的十六进制串忽略不计，所以只差耗时或端口的输出算同一个）；失败的调用出现了没见过的错误（按工具、错误的首末行和退出码判断，同一个错误换着命令出现不算）；自动验证的失败详情变了；交回的审查问题（按级别和文件）和上一次交回的不同；用户插话、回答了 `ask_user`、在止损询问里输入了指示。被熔断、循环检测、计划模式、拒绝规则拦下的调用，用户拒绝或无人值守时没执行的调用，以及只调 `todo`、`memory`、`skill` 的轮次，都不算进展。连续 6 轮（`stall_rounds` 的一半）没有进展时在下一次请求里提醒模型说明阻碍、换做法，缺外部条件就直接说明并结束；连续 12 轮时先对照工作区快照确认（shell 或子 Agent 改了文件就算有进展，重新计数；快照不可用时跳过确认）。确认后：交互会话显示反复出现的错误（次数只数标题所说的最近这几轮；被熔断、循环检测拦下的调用按机制归为一类，显示为“被拦下（guardrail）”“被拦下（loop detection）”）和改过的文件，询问 `1` 停下并总结（默认，直接回车）、`2` 换个思路再试（把试过的做法告诉模型，重新计数）、`3` 照原样继续（这个任务的阈值翻倍）或直接输入新的指示，三个选项各显示一行确认；`-p`、没有终端时和子 Agent 直接停下。停下时和用完调用次数一样，做一次不带工具的总结（附上 cove 本地记下的调用次数、反复出现的错误和改过的文件），任务记为中断，`/continue` 可以接着做、计数从零开始；交互会话在结束行 `■ 已停下 · 因连续 12 轮没有进展提前停止 · …` 之后只再打印一行怎么继续（不显示为错误，用完调用次数时也一样）；`-p` 照样打印 `[summary]`，末尾写“因连续 12 轮没有进展提前停止，任务没有完成”，退出码非零，诊断码 E5008。切换模型（快速模型升级到主模型）时重新计数。
- **收尾催促**（临近调用次数上限时跳过）：空回复（最多 2 次）；最新 todo 清单仍有未完成项（1 次）；改过文件后的完成自检（`done_check`，1 次）。“宣布了下一步却没做”（中英文启发式）和“做了事却只回一句话”（共最多 2 次）只在交互会话启用。
- **引用核实**（`cite_check`，默认开，交互会话和 `-p` 都生效）：本任务没改文件、回复去掉代码块后不少于 200 字符、而且讲的是本仓库的代码（至少一条引用能对上工作区文件，或反引号里至少 2 个词在工作区搜得到）时，结束前不调模型地核实回复里的引用。认识 `path:line`、`path:12-30`、`path#L12`、markdown 链接、`` `a.rs` 第 12 行 ``、`a.rs（第 12 行）`、`a.rs line 12`，以及跟在文件后面的“第 30 行”（同一段落、有小标题时同一小标题下）；代码块里的、URL 和 `host:port` 不算。路径可以是简写（`Distributor/Program.cs`）或工作区内的绝对路径，按忽略规则列出的文件查找。每条引用核对文件在不在、行号是否越界、同一分句里反引号中的符号是否在那一行前后 30 行内（忽略大小写、驼峰和下划线写法；分句说“没有”“未”“不在”“not”等否定时不查符号；另一条行号引用如 `:117`、不含字母的片段不当符号）。另外：回复说某个仓库路径里有某符号、但整个工作区都搜不到（说明“没有”的不算）。回复提到了仓库文件、本任务却一个读类工具都没调用、上下文的工具结果和你的消息里也没出现过这些文件时，记为“没读就讲代码”，只写进验收报告，不催模型。只是行号偏了时不调模型，显示 `[引用核实] a.rs:12 → 实际在第 19 行`（最多 3 条）；文件不存在、行号越界、符号对不上、仓库里搜不到时催模型一次，要求重新回答、不比原来长、只改错的地方；第二次仍对不上就显示 `[引用核实] 以下引用没能核实：…` 并正常结束。系统提示也要求问答的关键结论带 `path:line` 引用，行为和配置值以实际运行的代码为准。
- **验证门禁补充**：自动推断的检查命令在项目受信任或 `auto`/`bypass` 模式下直接运行，否则照常审批（项目信任由项目设置提供，当前默认不信任）。模型在最后一次修改之后自己跑通过的同一条命令直接记为通过，不再重复运行。检查超时记为“未验证”，不当作失败打回。每条检查结果追加到 `<data_dir>/verify_ledger.jsonl`。
- **提交前验证**：开启门禁时，模型通过 Shell 执行 `git commit` 前会先运行检查，并核对暂存内容与工作区一致、检查期间暂存区没变；要求先单独 `git add`，再单独 `git commit -m …`（不支持 `-a` 等改变暂存范围的参数）。不通过则不提交，结果交回模型。每份暂存内容（按 `git write-tree` 的树哈希）只检查一次：通过后，同样的内容再提交不重跑检查；失败或你拒绝了检查，同样的内容再提交时直接说明“已经检查过且未通过”，不再重跑也不再询问，并提示模型改动内容或向你说明情况，而不是反复重试。系统提示也告诉模型提交要单独一次调用，避免被门禁退回。
- **每个审查点只有一个审查者**：收尾和提交前各有一个审查点。配置了 `review_commands` 时由项目审查命令审（`review_when` 决定在哪个点运行），内置的 `done_self_review` 和 `commit_review` 都不再运行；`review_when` 为 `done` 时提交前不再审查（收尾已经审过）。没有配置时按原来的开关用内置审查。
- **提交前审查**：`commit_review`（默认 on）开启时，模型执行 `git commit` 和你执行 `/commit` 之前，一个只读子 Agent（快速模型，未配置时用主模型）阅读 `git diff --cached`（最多 40 KiB），查真实缺陷，并对照项目说明文件（AGENTS.md、CLAUDE.md、.cove.md）、最新的项目规则和 `review_instructions`：违反规则就算问题，并引用那条规则；没有规则覆盖的风格不提，全局偏好也不算审查标准。有问题时提交被拦下：模型的提交把问题交回模型，`/commit` 列出问题，可用 `--no-review` 跳过。同一份暂存内容只审一次。审查没跑完（模型报错、超时）时不拦截，只提示已跳过。没有配置验证命令时，门禁也会为审查生效。一次审查通常约 $0.01。`review_when` 含 `commit` 时，这里改为运行项目审查命令（`COVE_REVIEW_POINT=commit`），按 `review_block` 判定；收尾审查已经通过、工作区（文件工具的改动）没再变时直接算通过，不重跑；审查命令依赖的文件被改动时拦下提交；命令没跑成或输出无法识别时不拦截，只提示已跳过。
- **自审**：`done_self_review` 开启时，模型要结束、自动验证通过（或没有验证命令）之后，一个只读子 Agent 阅读本轮 diff，按与提交前审查相同的标准（真实缺陷 + 项目规则 + `review_instructions`）回答 `NO_ISSUES` 或按 `[HIGH] 文件:行 - 问题 - 原因` 列出问题；问题全部交回模型处理一次，不再重审。自审的调用和费用计入当前会话。
- **项目审查**（`review_commands`，见 [0028](optimizations/0028-review-loop.md)）：模型要结束、自动验证通过（或没有验证命令）之后运行项目自己的审查命令（经 Shell 运行，清掉 cove 的 API key，可取消，每条时限 `review_timeout_seconds`）。命令能读到 `COVE_REVIEW_POINT`（`done`/`commit`）、`COVE_REVIEW_ROUND`（第几轮，从 1 开始）、`COVE_REVIEW_PREVIOUS`（上一轮输出所在文件，第一轮没有）、`COVE_REVIEW_REQUEST`（当前需求所在文件）。
  - **判定看输出，不看退出码**，按顺序认三种写法：整个输出、最后一行或最后一个 ```` ```json ```` 代码块是 `{"findings": [{"severity", "file", "line", "title", "detail"}], "summary"}`（`severity` 认 `critical/high/medium/low`，也认 SARIF 的 `error/warning/note`，缺失按 `medium`；`findings` 为空表示无问题）；以 `[CRITICAL]`/`[HIGH]`/`[MEDIUM]`/`[LOW]`（可带 `**`、列表符号）开头的条目，输出里有 `Findings`/`问题` 标题时只看这一节，没有时跳过 `Previous review`/`上一轮` 一节，所以复述上一轮的标记不算；首个非空行是 `NO_ISSUES`。都不符合记为“无法判定”。退出码非 0、超时、命令无法启动都记为“未运行”。
  - **循环**：本任务没用文件工具改过文件不审；达到 `review_block` 的问题交回模型（阻断问题全文、其余只列标题，最多 12 KB），模型修完再审，最多 `review_max_rounds` 轮。阻断问题按（级别, 文件）连续两轮相同、交回后模型没有再改动文件（它认为不成立）、跑满轮数时停下，记为未通过，模型的回复里应说明没修的原因。没有阻断问题时通过，更低级别的问题只列在结论里。“无法判定”时第一轮把输出交给模型判断一次，之后不算通过；“未运行”不交回模型、不算通过。快用完模型调用次数时不审。
  - **结论**：任务结束时显示一行，如 `[审查] 第 2 轮通过（另有 2 条 LOW，未要求处理：…）`、`[审查] 3 轮后仍有 1 条 HIGH：…；没修的原因见上面的回复`、`[审查] 未运行：bash eng/review.sh 退出码 1`。每次运行追加一行到 `<data_dir>/verify_ledger.jsonl`（`kind: "review"`，命令、审查点、轮次、结论、各级别条数、退出码、耗时）。外部审查的花费 cove 看不到，不计入费用。
  - **防篡改**：启动时记下审查命令里指向工作区文件的参数（如 `bash eng/review.sh`、`python tools/review.py`、`./x.sh`）、存在的 `.cove.json` 和 `review_instructions` 指向的文件的内容；模型用 `write`/`edit` 改它们会被拒绝；每次审查前核对内容，被改动或删除（包括 shell 改的）时这次审查作废、记为“未运行”，提交前审查则拦下提交。你自己要改这些文件时，改完重启 cove。
- **改动影响**（`change_impact`，默认 `auto`，见 [0031](optimizations/0031-change-impact.md)）：模型要结束、本任务用文件工具改过 Rust、Go、Python、TS/JS、C#、Java 源码时（在收尾催促之后、自动验证和收尾审查之前），不调用模型地算一次：用 `repo_map` 的索引（按行正则抽定义）比较每个改动文件改前改后的定义，分出**删除**、**签名变化**（定义那一行变了）、**函数体变化**（改动行落在它的起始行到下一个定义之间）；新增的定义、测试代码不算。再按名字找用到它的其他文件（函数只认 `名字(` 形式的调用，类型和常量认整个单词，注释行都不算；自己也定义了这个名字的文件不算；不足 4 个字符、在 8 个以上文件里都有定义的名字和 `main`/`init` 这类名字不查，计为“名字太通用”），每处记最多 3 个行号和它所在的定义。调用方分三级：**有测试引用**（某个测试文件用到了调用方所在的函数）；**仅同包有测试**（Go 同目录有 `_test.go`、Rust 同 crate 有测试、其他语言有按文件名对应的测试文件，如 `test_x.py`、`x.spec.ts`、`XTests.cs`）；**没有测试引用**。测试文件按 `tests/`、`test/`、`__tests__/`、`*.Tests/` 目录和 `*_test.go`、`test_*.py`、`*_test.py`、`*.test.*`、`*.spec.*`、`*Test(s).java`、`*Test(s).cs` 认定，Rust 文件 `#[cfg(test)]` 之后的部分也算。上限：20 个改动符号、每个 10 处、共 30 处，2 秒；超出时如实写“另有 N 处未列出”“只查了一部分”。`auto` 在有跨文件调用方、并且有删除或签名变化（或改动不少于 20 行）时显示一段 `[影响]`（最多 12 行），`on` 只要有改动符号就显示，`-p` 下和其他通知一样写在 stderr。删除或改了签名的符号有**没有测试引用、本任务也没改过**的调用方时，把它们（最多 10 处）作为通知交给模型一次，要求逐个读、需要时在任务范围内修或补测试、不要扩大重构，并在最终回复里说明；快用完调用次数时不交。开了 `done_self_review` 时，自审子 Agent 的任务里附上这份调用方列表（`<callers>`）。按名字匹配会误报（同名的不同函数）也会漏报（接口成员和属性抽不到、函数被当作值传递、路由路径和配置键这类字符串耦合），所以输出总写“按名字匹配，可能有误报或漏报”，不给“没有风险”的结论。shell 改的文件不在文件工具的记录里，不计入。
- **验收报告**：每个任务结束后写 `<data_dir>/acceptance/<会话>.json`：运行过的检查及证据、验证结论、Git 状态（HEAD、未提交文件数、未推送提交数、本任务里的 commit/push 是否成功），做过引用核实的任务还有 `citations`（核实条数、通过、行号偏移、没能核实、是否催过），做过收尾审查的任务还有 `review`（审查者、结论 passed/failed/not_run/unparsed、轮数、没处理的问题），每份报告都有 `build`（写报告的构建标识；2026-11-24 之前的报告没有），算过改动影响的任务还有 `impact`（改动符号数和删除、签名变化、函数体变化各几个，其他文件几处使用，没有测试引用的调用方（文件、行、符号，最多 30 条），是否只查了一部分、是否交给过模型；不存代码）。`/status acceptance` 显示最近一次，引用一行形如“引用：6 条，5 条核实，1 条行号偏移”，审查一行形如“审查（项目审查命令）：通过，2 轮”，影响一行形如“影响：改了 1 个符号（删除 0、签名变化 1、函数体变化 0），其他文件 2 处使用，1 处没有测试引用（已交给模型核对）”。报告只记录发生了什么，不代表模型声称完成。

## 文件、输出与失败语义

- **read**：返回 `File: 路径 (N lines total)`（CRLF 或混合换行的文件在括号里注明，例如 `CRLF line endings`；显示的行不带 `\r`） 加带行号的内容，默认从 `offset`（1 起）读 2000 行，`limit` 可改；旧参数 `start_line`/`lines` 仍可用。单行超过 2000 字符截断并标注 `…[+N chars]`。还有剩余时末尾给出 `[next: offset=X]`。传入目录时列出目录项，子目录以 `/` 结尾。文件最大 32 MiB。
- **编码**：含 NUL 的二进制文件和 UTF-16 文件拒绝读取（UTF-16 会给出转换建议）；非 UTF-8 文件如果能按 GBK/GB18030 解码且能逐字节还原，就解码显示，并注明编码；其他旧编码照原样显示并提示乱码，`edit`/`write` 会拒绝这类文件。图片文件暂不读取。
- **write**：自动创建缺失的父目录（目录本身不随 `/undo` 删除）。覆盖已有文件前，本会话必须读过它的当前版本（按大小和 SHA-256 判断，部分读取也算；本工具自己写过的文件自动记为已读；只读子 Agent 读过也算）；文件之后被用户或命令改过，要先重新读。`overwrite` 参数仍接受，但已不需要。覆盖时保留原文件的 UTF-8 BOM 和 GBK 编码，换行按行保留：没改的行原样不动，改动的行沿用它所替换那一行的换行，新增的行用文件的多数换行（不会因为一次覆盖把整个文件的换行统一掉）。新建文件的换行按项目约定：`.editorconfig` 的 `end_of_line` → 项目 Git 检出该路径时的换行（`eol`/`text` 属性、`core.autocrlf`、`core.eol`）→ LF；内容里自带 `\r` 时视为有意指定，原样写入。内容中有 GBK 无法表示的字符时拒绝写入。参数名也接受 `file_path`/`filePath`、`text`。
- **edit**：`old` 必须唯一匹配，多处匹配时报告所在行号，可用 `replace_all=true` 全部替换。换行按行处理：匹配在去掉 `\r` 的视图上进行（混合换行的文件也能精确匹配），写回时没改的行原样不动，改动的行沿用它所替换那一行的换行，新增的行用文件的多数换行；模型发来的 `old`/`new` 里的 `\r` 会先去掉，不会把 LF 文件写成混合换行。`old` 和 `new` 只差 `\r` 时视为有意修改换行，按字节原样替换。GBK 文件编辑后仍写回 GBK。没有精确匹配时，按“忽略空白差异”逐行比较：只有唯一匹配才替换，并把 `new` 的缩进换成文件的缩进单位；缩进对应关系不一致（混用 Tab/空格或层级不齐）时拒绝多行替换；超过 20000 行的文件不做这种匹配。仍找不到时给出最相近的候选位置和相似度。成功结果显示最多 3 处改动及前后各 2 行；精确匹配且改动片段超过 400 字节时，只报告改动所在的行号（模型刚发出的内容不再原样回显），按空白差异匹配的替换始终显示改动后的内容。`edit` 不要求先读，但修改都经过检查点，可以 `/undo`。
- 文件改动上限 1 MiB（非 UTF-8 内容 4 MiB，例如 `draw_image`/`browser` 截图写出的 PNG）；`write`/`edit` 都经过检查点日志和漂移检查。
- 临时文件原子替换不是与外部编辑器的原子 CAS；检查点也不是宿主沙箱，建议先在干净分支或临时副本上试用。
- **搜索**遵守 ignore 文件（没有 Git 仓库时也遵守 `.gitignore`），不跟随目录链接，不检索 Git 元数据。`grep` 有 ripgrep 时调用 `rg`，否则用内置搜索，结果格式相同（`路径:行号:内容`，上下文行用 `-`，组之间用 `--`）；参数 `include`（文件名 glob）、`path`（文件或目录）、`ignore_case`、`context`（0–10）、`files_only`；最多 100 行，超出时提示“at least N more”。`glob` 的模式不含 `/` 时匹配文件名，含 `/` 时匹配相对 `path` 的路径，`**` 可跨目录；最多 200 条，超出时提示 `... and N more`。路径一律相对工作区、用 `/` 分隔。
  - **被忽略的文件**：`glob` 和 `grep` 都有参数 `include_ignored`（默认 `false`）。设为 `true` 时不再应用 `.gitignore`、`.ignore`、`.git/info/exclude` 和全局 excludes，可以搜 `node_modules`、`bin`、`obj` 这类构建输出；`.git` 和 cove 数据目录照样跳过，范围仍限于工作区。这时搜索最多运行 20 秒。`glob` 和内置搜索的 `grep`（没装 ripgrep 时）每次最多遍历 50,000 个条目（默认模式也是）；用 ripgrep 的 `grep` 不数条目，只有 `include_ignored` 时的 20 秒和匹配行数上限。提前停止时结果末尾写明 `... search stopped after …; results are incomplete, narrow path or pattern`。`path` 直接指向被忽略的目录（如 `node_modules/foo`）时不需要这个参数，目录照搜，但其中再匹配别的 ignore 规则的文件（如 `*.log`）仍被跳过。默认模式结果为空、且搜索路径或它的上级目录里有 `.gitignore`/`.ignore`（或 `.git/info/exclude` 有规则）时，结果写成 `No files found (files excluded by .gitignore were skipped; include_ignored=true searches them)`，grep 同理；不会为此再遍历一次被忽略的目录。要列出被 `.gitignore` 排除了哪些内容（比如找可以清理的构建输出），`glob` 的说明建议先用 `git status --short --ignored` 或 `git clean -ndX`。界面上带这个参数的调用标题后面写“含被忽略的文件”。
- **Shell 解释器**：Windows 上优先使用 Git for Windows 的 bash（按 git 所在目录和 Program Files 查找，跳过 WSL 的 bash 启动器），没有时用 PowerShell；其他平台用 bash，没有时用 sh。可用配置 `shell` 或环境变量 `COVE_SHELL` 指定（`auto`、`bash`、`powershell`、`cmd` 或解释器路径）。命令明显是给另一种已安装的 Shell 写的就改用那个 Shell 运行，审批按同一方言判断；在原 Shell 里失败且错误表明方言不对时，用能只读解释它的另一种 Shell 重跑一次，并在结果开头注明。工具描述和系统提示中的环境行都写明实际解释器。Hooks 和验证门禁命令仍按原方言运行（Windows 上为 PowerShell）。bash 和 PowerShell 在进程加入 Job Object 之后才从 stdin 收到脚本；cmd 用 `/d /s /c` 传入单行脚本（多行以 `&` 连接，`NoDefaultCurrentDirectoryInExePath` 避免先运行工作区里的同名 `.bat`）。
- **非交互环境**：命令运行时设置 `GIT_TERMINAL_PROMPT=0`、`GIT_EDITOR=true`、`GIT_PAGER=cat`、`PAGER=cat`、`NO_COLOR=1`、`TERM=dumb` 等，不会等待编辑器、分页器或凭据输入；通过 `GIT_CONFIG_*` 关闭仓库配置的 `core.fsmonitor` 并设 `safe.bareRepository=explicit`（保留用户已有的 `GIT_CONFIG_COUNT` 条目）。
- **Shell 时限**：`timeout_seconds` 是前台最多等多久，默认 120 秒，最多 3600 秒（也接受毫秒单位的 `timeout`）；可选 `description` 说明命令用途。到点时命令**不会被杀**，而是转到后台继续跑：工具立刻返回到目前为止的输出（同样按头尾截取），开头一行 `[background #N] Still running after 120s, …` 写明任务编号和完整输出文件；屏幕上打印 `命令运行超过 2m00s，已转到后台继续（#N）；完成后会自动交给模型`。所有命令（不只是构建和测试）只要还在干活就继续跑：180 秒没有任何活动（判断方式同 `done_verify_idle_seconds`）才中止，结果开头写明 `[stalled: …]` 和原因；`timeout_seconds` 大于 180 时，空闲上限取 `timeout_seconds`（`ssh` 远程任务、`sleep` 这类长时间没有动静的命令，给一个足够长的 `timeout_seconds`）；输出里出现"在等另一个进程的锁"（cargo、yarn、bazel 等，见锁等待）之后不再按空闲判卡住，只受总时限；总时长最多 3600 秒。
- **后台命令**：转到后台的命令结束（正常退出、失败、卡住被中止、到总时限、被停止）后，结果在下一次模型调用前作为一条通知交给模型（命令、结果、耗时、完整输出文件、输出最后约 3 KB），屏幕上显示 `[后台] #N 已结束：退出码 0，用时 3m12s · <命令>`。模型准备结束本轮时，如果还有转到后台的命令在跑，任务先等它们结束再把结果交给模型继续（`-p` 也一样），收尾验证在这之后才运行；后台命令的结果和前台命令一样算作验证证据（成功结束的检查命令记为通过，收尾验证不再重跑；失败或被停止就不算；可能改文件的命令照样作废之前的通过记录），`shell_task` 既不作废证据也不算"做了改动"；等待时状态栏显示 `等待后台命令 · <命令> · <已运行时长> · 活跃`（或 `· 已 N 无活动`），输入文字会打断等待（命令继续跑，模型先回应插话，再次结束时再等），连按两次 Esc 停止等待、取消任务并结束这些命令的整个进程树。任务被取消、出错或因上限、没有进展停下时，仍在跑的转后台命令被结束，结果在下一个任务开头交付。完整输出边跑边写到 `<数据目录>/tool-outputs/shell/<会话 id>/<N>.log`（前台跑完的命令不写文件），每个文件最多 64 MiB，超出部分不再写入，文件末尾写明 `[cove: output past 64 MiB is not written here; …]`，结果里的文件路径后注明 `(cut at 64 MiB)`，最新输出仍可用 `shell_task output` 看；7 天后清理，会话结束时不删（之后还可能要看）；内存里只保留头尾和最近 16 KiB。编号在会话内跨 cove 进程连续：恢复会话时从目录里已有的最大编号往后排，已有的日志不会被覆盖。cove 重启后不会恢复后台命令。后台命令占着锁（例如后台的 `cargo test` 占着构建目录锁）、新命令在等它时，锁等待提示写明 `占用者是本会话的后台命令 #N`，不提议结束它。
- **`run_in_background`**：`shell` 传 `run_in_background: true` 时直接在后台启动，最多等 5 秒（出现首批输出后再等 0.5 秒；提前结束则按普通结果返回），返回 `[background #N] Started in the background` 和初始输出，用于开发服务器、watch 这类一直运行的命令。这类命令不受空闲上限和总时限限制，本轮结束前不等它们，任务被取消时也不停止；它们自己结束时结果在下一次模型调用前交给模型。审批和普通 shell 命令相同。
- **`shell_task`**：查看或停止后台命令。`action` 为 `output`（状态、完整输出文件和最后约 4 KB 输出）、`stop`（结束命令和它的子进程，等它退出）或 `list`（全部后台命令），`output`/`stop` 需要 `id`。模型看过已结束命令的 `output`/`stop` 结果后不再重复通知。不需要审批，只作用于本会话启动的命令。
- **后台命令的生命周期**：后台命令属于启动它的会话或子 Agent。会话结束（`/new`、`/resume`、挂起主任务、退出 cove）时全部结束，并打印 `[后台] 会话结束，已停止 N 个后台命令`；子 Agent 和后台 Agent 结束时它启动的后台命令全部结束，结果末尾写 `[background commands stopped when this sub-agent ended: …]`（后台 Agent 写 `…when this agent ended: …`）。`/handoff` 不把后台命令交给新会话：交接时结束它们，打印 `[后台] 交接到新会话，已停止 N 个后台命令`。cove 正常退出（包括 Ctrl+C）时逐个结束整棵进程树。Windows 上即使 cove 被强制结束，Job 对象随句柄关闭也会结束整棵进程树；Unix 上 cove 被 `kill -9` 或收到 SIGHUP 后来不及清理时，后台命令的进程组不会被结束，可能留下运行中的服务。有后台命令在跑时，状态栏多一段 `后台命令 N 个`（空闲时也显示）。
- **输出截取**：stdout 最多保留约 30 KB，stderr 约 10 KB，超出时保留开头 40% 和结尾 60%（在行边界切开），中间注明省略的字节数；管道始终排空，避免堵塞。Windows 上非 UTF-8 的输出行（控制台代码页 GBK）按 GBK 解码；仍无法解码时标记 `encoding_warning`，不假装无损。
- Shell 结果包含退出码、stdout、stderr、超时和截断标记。运行中逐行实时显示，每个流最多实时显示 256 KiB，超出部分只保留在结果里。这是按行刷新的输出，不是 TUI 面板，进度条这类原地刷新的输出会显示成多行。
- 写入或命令已经产生的副作用不会因取消或网络错误自动撤销。
- 流式响应缺终止事件或中途断开（连接中断、空闲超时、服务端过载）时，不执行该响应内的工具；断开的部分没有生效，自动重新发起这次请求，最多 3 次（间隔 2、4、8 秒），仍失败才结束任务。请求本身有错（如工具 schema 不合法）不重试，错误信息带上服务端给的说明。输出截断或工具参数不完整时，同样不执行该响应内的工具，回复作废并让模型重做。
- 工具失败会作为错误结果交给模型；任务结束时按整次任务统计历史失败，不能据此判断问题是否已解决。交互模式显示提醒，可用 `/doctor trace` 查看失败记录或用 `/expand <n>` 查看工具输出；单次执行的退出码保守地保持非零，即使模型声称已恢复。用户拒绝的调用不算失败。
- 模型正常结束不等于通过独立验收；没有实际测试证据时不能声称验证通过。
- **验证门禁的结果**写在任务结束的汇总行里（交互控制台是 `✓ 完成 · …`，`-p` 和管道是 `[summary] …`）：`验证通过`、`验证未通过`、`验证未运行`（审批被拒、plan 模式下无法运行，或检查超时），或 `无自动验证`（无检查命令，或本轮没有文件改动；交互控制台不显示这一项）。3 轮后仍失败，退出码为非零。门禁只看文件工具产生的改动；单纯通过 Shell 修改文件不会触发它。没有运行就不算通过。
- **网络工具**：`web_fetch` 只访问 https（http 自动升级）。每次请求和每次重定向前，都会解析目标地址，拒绝本机、私网、链路本地和 CGNAT 地址，并固定解析结果，防止 DNS rebinding；TUN 模式（“fake-IP”）代理返回的 `198.18.0.0/15`、`2001:db8::/32` 地址视为公网，因为局域网不会用到它们。不走系统代理。响应正文最多读 2 MiB，每次返回 12 KiB 文本，可以用 `offset` 翻页。带 `prompt`（要从页面里找什么）且页面超过 4 KiB 时，由后台模型（`model_fast`，未配置时用当前模型）只根据页面内容回答，返回简短答案而不是整页；需要原文时不带 `prompt`。提取失败时退回返回页面本身。网络访问被拒绝时，工具返回失败结果，模型可以不联网继续，任务不会被中断。搜索词会发送给第三方服务。

## 子 Agent、计划、Worktree 与回归验证

### 多个子任务怎么分发

| 方式 | 适合 | 并行 | 改动落在哪里 |
| --- | --- | --- | --- |
| `execute_plan`（见下文“计划执行”） | 一个需求拆成几块，有先后依赖 | 按依赖图并行，最多 `max_agents` 个（1–8，默认 4） | 主工作区，进入当前任务的检查点（`/diff`、`/undo`） |
| 后台 Agent `agent {background: true}`（见下文“后台 Agent 与生命周期”） | 彼此独立、耗时长、想各自出一个分支的任务 | 同时最多 `max_background_agents` 个（1–16，默认 4），不阻塞主任务 | 各自的 git worktree 和分支，由你 `/status agents merge` 合并 |
| `team_create`（实验工具，需 `experimental_tools: true`） | 一次建一组成员 | 复用计划执行器 | 同 `execute_plan` |
| `execute_plan {isolation: "overlay"}`（见下文“覆盖层拆分”） | 预计超过 5 分钟、能拆成几块的改动，几块可能改同一个文件；子任务里不需要构建、测试或跑生成文件的命令 | 写集合不重叠的、同一文件不同 `::符号` 的并行，其余自动排成先后；最多 `max_agents` 个 | 每个条目先进自己的覆盖层，全部结束后统一合并进主工作区，作为当前任务的一个检查点（`/diff`、`/undo`） |

模型在同一次回复里直接调用多个前台 `agent` 时，只读类型（`explore`、`plan`、`review` 和只读的自定义类型；带 `regression` 的 `verify` 除外）**同时**运行，最多 4 个；可编辑类型（`general`、`test`）依次运行。要并行改代码，用上面的方式。不需要记工具名，直接说明意图即可，例如：

```text
把这个需求拆成几个独立子任务并行执行：A 改 dispatcher，B 改 margin，C 补测试（C 依赖 A 和 B）
用后台 Agent 分别在独立 worktree 里做这三个修复，做完告诉我
```

子 Agent 的费用计入主会话；审批规则与主任务相同（并行时审批排队，一次只问一个）；子 Agent 看不到主对话，不能再委派，也不能向你提问。

### 子 Agent 类型

`agent` 工具把一个自包含的子任务交给子 Agent。子 Agent 看不到父对话，只按 `prompt` 工作，不能提问，也不能再委派（没有 `agent`、`ask_user`、`memory`、`mcp`）。系统提示里带有和父任务相同的项目说明。每个子 Agent 最多调用模型 `subagent_max_iterations` 次（默认 60；`explore` 另有上限 30，取两者中较小的）。它的模型调用、token 和费用都记入父会话。

| 类型 | 能做什么 | 用哪个模型 |
| --- | --- | --- |
| `general`（默认） | 读写文件、运行 Shell、使用 todo/skill/网络工具，也能运行 `regression` | 跟随当前任务 |
| `explore` | 只读调查，可以联网；答复固定三段：结论、证据（`路径:行`）、没查清 | 快速模型（`model_fast`；没配或与主模型相同时用主模型；`explore_model: "main"` 改回主模型） |
| `plan` | 只读，产出有序的实现计划，可以联网 | 主模型 |
| `review` | 只读代码审查，不能联网 | 主模型 |
| `test` | 编写和运行测试，工具与 general 相同 | 跟随当前任务 |
| `verify` | 只读、不能联网，只多一个 `regression` 工具。必须用它拿到“基线失败、当前通过”的证据，否则这次调用就是失败的工具结果（`exit: unverified`） | 主模型 |

- “主模型”指 `model`，即使这个任务被判成简单任务、路由到了快速模型；“跟随当前任务”指这个任务正在用的模型。见 [0029](optimizations/0029-explore-delegation.md)。
- 子 Agent 按**实际作答的模型**的单价记账：快速模型的调用按快速模型的价格计入会话，和用量账本一致；上下文窗口和输出上限也按这个模型取（服务端报告过的窗口优先，否则按内置模型表），不再沿用主模型的。
- 旧参数 `mode=read`/`mode=write` 仍然可用，分别等于 `explore`/`general`。
- 自定义类型写在配置的 `agent_types` 里。只读类型只能列 read/glob/grep/repo_map、web 工具和 regression。`model` 可写 `main`、`fast`、`inherit`（默认）或具体的模型名。
- 结果第一行是 `[sub-agent <id> (<类型>): <描述>] [exit: <原因>, steps: N, truncated: yes/no]`，最后一行是费用、实际作答的模型、步数和失败工具数（如 `[cost: $0.0040, model: deepseek-v4-flash, steps: 6, failed tools: 0]`）。`exit` 的取值：`completed`、`max_iterations`、`stalled`（连续 `stall_rounds` 轮没有进展，总结后停下）、`interrupted`、`approval_denied`、`unverified`、`error`。
- **答复上限 6 KB**（中文约 2000 字）：超出的部分不进主上下文，全文存到 `<数据目录>/tool-outputs/`（和外置的旧工具输出同一目录，7 天后清理），结尾写明路径，需要时用 `read` 读。父模型在 prompt 里要求写更长也一样。
- **证据核对**（只读类型）：子 Agent 结束后，cove 在本地检查答复里每个 `路径:行` 是否存在、行号是否在文件范围内（不调用模型，只查位置，不查内容）。结果第二行是 `citations: N ok, M invalid`；不合格的那一行末尾标 `[未核实：文件不存在]`、`[未核实：超出文件行数 N]` 等。`explore` 的答复既没有证据、也没写“没查清”时标 `citations: none`，提示主模型把它当线索而不是事实。
- **什么时候用 explore**（系统提示和工具说明里的准则）：回答“X 在哪里、怎么流转、谁调用了 Y、改 Z 会影响哪些地方”，预计要看 5 个以上文件或几个目录，只需要结论时委派；几块不相关的区域在同一次回复里发多个 explore，并行执行。知道要看哪一两个文件、接下来要改的文件、一步步缩小范围的调试、一次 grep 就能回答的，主任务自己读。主任务在一段连续的只读调用里读到第 6 个不同的文件、本任务还没委派过时，工具结果后附一次提示；任务本身就在快速模型上、或没配快速模型时不提示。
- 可编辑类型的写入进入父任务的检查点，`/diff` 能看到、`/undo` 能回退，也会触发验证门禁。审批规则与父任务相同；多个子 Agent 并行时，审批提示会排队，一次只问一个。
- 子 Agent 内部的单次工具调用不进入父会话的调用日志。委派过程中崩溃时，整个 `agent` 调用标记为不确定，需要检查改动后 `/resolve`。Hooks 只对 `agent` 这一次调用触发。
- 子 Agent 运行时的动作会实时显示为带 id 前缀的行（如 `[agent-1] -> edit`、`[todo-2] [ok] shell: ...`）。

### 计划执行 `execute_plan`

`execute_plan` 读取本会话最近一次成功的 `todo` 调用，把其中 `pending` 的条目当作子 Agent 任务执行。

- **依赖**：用条目的 `id`/`depends` 声明，或者在内容前写前缀 `depends:todo-1,todo-2 标题`。依赖必须在本计划内，或者已经 `completed`；未知依赖和循环依赖直接报错。
- **调度**：按依赖图调度，依赖一完成就启动，不按层等待。`parallel=true` 时最多同时跑 `max_agents` 个（1–8，默认 4）；否则串行。
- **重试**：失败的条目带着失败原因重试一次。到达调用上限或被取消的不重试，保留部分结果。依赖失败的条目会跳过，并逐级传递。
- **文件冲突**：并行条目通过文件写入认领表协调。同一个文件不会被两个正在运行的条目同时写：后来者等先认领的条目结束后再写；如果出现互相等待，后来者的写入直接失败，并说明冲突。Shell 命令改的文件不在认领范围内。
- **结果**：返回每个条目的结果、exit、尝试次数和步数。已由 execute_plan 完成的条目不会重复执行；模型应据此更新 todo。
- **写集合 `writes`**（[0054](optimizations/0054-task-split-skill.md)）：条目可以申报 `writes`（字符串数组：工作区相对路径或 glob；目录（结尾带不带 `/` 都行，比如 `src/agents`）表示它下面的所有文件；可以写成 `文件::符号`，强制时只看文件，符号只用来说明），也可以在内容前写前缀 `writes:a.cs,b.cs 标题`（逗号后不留空格，可以和 `depends:` 前缀前后连用）。只对申报了 `writes` 的条目生效，没申报的条目行为不变：
  - **开始前的重叠检查**：`parallel=true` 时，两个互相没有依赖路径的条目都申报了写集合、而且有交集（同一个文件；glob 匹配到对方的文件或同一个已存在的文件；两个 glob 字面相同），整个计划在任何条目运行前被拒绝，结果写明是哪两个条目、哪些文件，提示加 `depends` 或合并成一个条目。两个不同的 glob 只能按已存在的文件判断，将来才新建的同一个文件看不出来。串行执行不做这项检查。
  - **写入约束**：条目的子 Agent 用 `write`、`edit`（以及 `draw_image`）写写集合以外的文件时直接拒绝，结果是 `<path> is outside this plan item's writes (...); stop and report why it needs changing instead of editing it`。
  - **命令约束**：条目里的 shell 命令是包安装、代码生成、数据库迁移、格式化或自动修复（`npm/pnpm/yarn install|ci|add`、`pip install`、`dotnet restore|ef|format|tool restore`、`cargo fmt|fix|add`、`go generate|get|mod tidy`、`protoc`、`buf generate`、`prisma generate|migrate`、`alembic upgrade`、`prettier --write`、`eslint --fix`、`black`、`ruff format`、npm 脚本名和 make/gradle/mvn/just/task 目标名里含 gen、generate、codegen、format、fmt、fix、migrate、install、proto、restore 等整词的（名字按 `:`、`-`、`_`、`.`、`/` 和大小写边界切成词后整词比较，连写的也认，比如 `reformat`、`lintfix`、`dbmigrate`、`mockgen`、`genproto`；`test:agents`、`prefix`、`prototype` 不算。格式化类的词紧挨着 check/verify（`format:check`、`check-fmt`、`fmtcheck`），或者参数带 `--check`/`--dry-run` 时，只算检查；其他生成类词带了 check 也照样拒绝，比如 `generate-and-check`；只看真正的脚本名和目标名，`-C dir`、`-pl module`、`--tests pattern` 这类选项值不看）等，按 shell 方言解析，`npx`/`pnpm exec` 等按它们运行的工具判断）时直接拒绝，说明这类步骤放到所有条目完成之后。列表外的生成命令靠下面的事后报告兜底。`run_in_background` 的 shell 和 `regression` 工具也直接拒绝：前者在这次调用返回之后还在改文件，查不到；后者会把文件换回基线再跑测试，同时运行的条目会看到。
  - **shell 越界改动**：条目的 shell 命令前后各取一次工作区的 `git status`（只看未提交的改动和未跟踪的文件，按大小和修改时间比较），命令改了写集合以外的文件时不回滚，在这次 shell 的结果里提醒子 Agent，并记进条目的结果。同一时间别的条目用文件工具写的文件不算在这个条目上。命令运行期间没有别的条目在跑 shell、也没有没申报写集合的条目在并行时，改动都记在这个条目上，改到别的条目申报的文件时写明是谁的（`(changed by shell; in the writes of todo-2)`）；否则归属说不准，记为“不确定”（`(changed during its shell command; maybe by another item running then)`），不提醒子 Agent。不是 git 仓库时不做这项检查；`timeout_seconds` 到点转入后台的 shell 命令只检查到转入后台为止。
  - **结果**：每个条目多两行 `written:`（实际写入的文件，含 shell 改的写集合内文件）和 `outside writes:`（被拒绝的写入和命令、shell 改的越界文件）；汇总后面列出所有越界。因越界写入或命令被拒绝而失败的条目不再重试（重试会被同样拒绝），需要改计划。
- **预估和校准**：条目可以带 `estimate_minutes`（非负数，小数四舍五入到整分钟）。每次 `execute_plan` 结束时，取这次完成的、带预估的条目，把“预估之和、这些条目各自最后一次运行的耗时之和（并行时也按串行工作量相加）、条目数、是否并行”追加到 `<数据目录>/estimates.jsonl`（保留最近 500 条）；只做了规划、没有执行计划的任务不记录。校准系数是最近 20 条这类记录的“实际/预估”中位数，限制在 0.5–4，没有记录时为 1；旧版本按“从 todo 到任务结束”记下的记录不参与校准。校准后的预估超过 `split_suggest_minutes`（默认 5）、并且至少两条可以并行（互相没有依赖；都申报了写集合的，写集合不重叠）时，`todo` 结果末尾提示模型按 `task-split` 技能先把拆分计划交给用户确认，再执行；同一个任务只提示一次，同一个会话里同一份计划（按未完成条目的内容判断，不管顺序）只提示一次，所有未完成条目都申报了 `writes` 的计划（已经按技能拆好）不提示，`execute_plan` 里的子 Agent 不提示。提示推荐的执行方式是 `execute_plan parallel=true isolation="overlay"`。

### 覆盖层拆分 `isolation: "overlay"`

`execute_plan` 的 `isolation` 参数：`"main"`（默认，上面描述的行为，完全不变）或 `"overlay"`（[0053](optimizations/0053-overlay-split.md)）。

- **前提**：每个条目都要申报 `writes`，否则直接拒绝，结果列出缺的条目。
- **确认**：开始前在终端里列出条目、各自的写集合、依赖和自动加的依赖，串行和并行的校准预估（按 `estimates.jsonl` 校准），预计额外的 token 和费用（每个子 Agent 约 1 万 token），然后问：`1 确认`、`2 改为普通方式（isolation=main）`、`3 取消`。没有终端时（`-p`）：`auto` 和 `bypass` 权限模式直接执行，其他模式改按 `isolation="main"` 执行，结果开头注明原因。
- **调度**：写集合在文件级不重叠的条目并行；同一个文件、双方都写了 `文件::符号` 而且符号不同的，也并行，最后合并；其他重叠（同一个符号、没写符号、glob）不拒绝，而是自动让后一个条目（按计划顺序）依赖前一个，后一个在前一个的层上改，结果的 `Implicit dependencies` 一段写明。显式 `depends` 同样叠层。并发上限沿用 `max_agents`。
- **覆盖层**：每个条目一个私有层 `<数据目录>/overlays/<计划>/<条目>/`；它的读取依次看自己的层、依赖条目的层（按依赖顺序）、主工作区，写入只进自己的层，第一次写某个文件时保存这个文件当时的内容作为合并基线（只保存改过的文件）。条目运行期间主工作区不变。两个并行条目都改了同一个文件、第三个条目同时依赖它们时，第三个条目开始前先用合并引擎把两者的版本合成一个只读的组合层（`<条目>-combined/`），放在依赖层之上、自己的层之下，它和它的 `check` 都看到两边的改动；合不成时第三个条目不运行（`skipped: deferred: ...`，依赖它的条目一并跳过），留在待办里，等这次计划合并完再调用一次 `execute_plan`，它就在合并后的主工作区上运行。覆盖层条目之间不用文件认领：同一个文件的条目真正并行，冲突交给合并。
- **条目里的限制**：`write`/`edit` 只能改写集合里的文件（0054 的约束照样生效）；shell 只放行只读命令，在主工作区运行，看不到层里的改动；构建、测试、重定向写文件、格式化、包安装、删除一律拒绝；`run_in_background` 和 `shell_task` 关闭。
- **共享检查槽**：配置了 `overlay_check` 时，条目多一个 `check` 工具：排队（同一时刻只跑一个，几个 cove 窗口之间也排队），把“主工作区 + 依赖条目的层 + 自己的层”同步进 `<数据目录>/check-slots/<项目哈希>/tree/`，在那里运行配置的命令（按 0048 的规则判断卡住，总时限 1 小时），返回退出码和输出。同步的文件是 `git ls-files --cached --others --exclude-standard` 列出的（不是 git 仓库时按忽略规则遍历），之后每次只重写变化的文件、删掉不再存在的文件；命令留在槽里的构建产物不动，下一次检查是热缓存。编译失败不算失败的工具调用，条目可以改完再检查。
- **合并**：所有条目结束后，按依赖顺序把完成的条目逐层合进主工作区；每个改过的文件调用一次三方合并（基线 = 这一层开始改它时看到的内容，当前 = 主工作区现在的内容，可能已有前面合进来的层和你中途的改动，本方 = 层里的结果），依次尝试直接写入、`git merge-file` 行级合并、锚点重放（`edit` 意图；只在行级合并有冲突时用，并且重放结果对当前内容的改动必须正好是这一层对基线的改动，改到当前内容里别人改过的行、或在别人插入的同一处再插一遍时不采用）、`mergiraf` 结构化合并（`mergiraf_path` 或 PATH 里有时）。写入都经过检查点，整体是当前任务的一个检查点，`/diff`、`/undo` 可用。合并结果是删除文件的，不执行，按冲突处理。
- **冲突重做**：一个条目有冲突的文件时，它以主工作区的当前内容为基线**重做一次**：新的层，写集合只限冲突的文件，提示词里带原目标、冲突说明、当前内容里冲突附近的行和它原来的编辑；重做完再合并（`merged after redo`）。仍冲突、重做失败或重做没改这个文件的，**不写入**，带冲突标记的内容（没有标记时是条目的版本）存到 `<数据目录>/overlays/<计划>/conflicts/<条目>/<路径>`，结果里列出，交给主任务处理；这个条目不算完成，依赖它、也改了同一文件的后续条目的这个文件一并不写入。
- **合并报告**：工具结果在条目报告之后列出每个文件一行：`direct write`（直接写入）、`anchor replay`（锚点重放）、`line merge`（行级合并）、`structural merge`（结构化合并）、`merged after redo`（重做后合并）、`conflict unresolved`（冲突未解决，附原因和副本位置）；`Outside declared writes`（层里超出申报的改动，以及被拒绝的写入和命令）；`Implicit dependencies`；没完成的条目。界面上同时显示一行摘要，例如 `[合并] 覆盖层合并进工作区：写入 3 个文件（直接写入 2，锚点重放 1，…）`；Agent 面板里每个条目的最后一步写着层里改过的文件数。
- **中途停止**：只有层完整合并进去的条目才算完成，后续 `execute_plan` 只把这些当作已完成的依赖。条目运行中、重做中或 mergiraf 运行中按 Esc（或出错、到时限）时，工具结果仍然记录下来：已经写入的文件（同一个检查点，`/undo` 可撤销）、正在合并的条目、没合并的条目及其层的位置；没合并的条目留在待办里。合并时为新文件新建的目录也记在检查点里，`/undo` 后为空的会删掉。
- **清理**：全部合并成功时删除这个计划的覆盖层目录；有失败或冲突的条目时目录保留到当前任务结束，结果里写明位置；计划中途停止、有已完成但没合并的条目时目录**不随任务结束删除**，供恢复：每个条目目录里的 `overlay.json` 列出改过的路径和内容对应的 `blobs/` 文件，编辑记录在 `intents.jsonl`；cove 启动时删除 7 天没动过的计划目录（和 `tool-outputs/` 同一时机、同一期限）。条目开始前合成组合层时按 Esc，条目记为取消，不是 deferred。
- **之后**：合并完成后，主任务在主工作区跑一次生成类步骤（如果有）、构建、测试和审查（收尾验证照常进行），不自动提交。
- mergiraf 每次结构化合并会在它的缓存目录（`merges/`）里留一份供 `mergiraf review` 用的记录；cove 从输出里读出记录 id，合并后删掉这份记录（Linux 上还把它的缓存目录指到临时目录）。

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
- 编译错误、跳过或不存在的测试、超时、卡住（180 秒没有任何活动）都记为 `unverified`，不算证据。`timeout_seconds` 是每次运行的总时限，默认 3600 秒，范围 10–3600。测试本身必须写在没列出的文件里（如 `tests/`）。
- 临时还原前，先通过 Workspace 记下当前内容。还原期间，文件工具拒绝修改这些文件。进程崩溃后，`/resolve` 或 `/status agents repair` 会把仍是基线内容的文件恢复成当前内容；被别人改过的文件不覆盖，标记为不确定。
- 证据是 `RegressionEvidence` 结构（status/reason/runner/selector/files/baseline/tests/before/after），可以通过 `AgentHub::evidence()` 提供给验收报告。

### 后台 Agent 与生命周期

`agent {background: true}` 立即返回 Agent id，子 Agent 作为后台任务运行，不阻塞父任务。

- **隔离**：可编辑类型必须隔离（`isolation="worktree"`，默认）。每个 Agent 在 `<数据目录>/worktrees/<项目哈希>/<id>` 有自己的 worktree，分支为 `cove/agent-<id8>`，从当前 HEAD 创建。只读类型可以 `isolation="none"`，在主工作区只读运行，此时不提供 regression。
- **持久化**：每个后台 Agent 的会话（消息、待执行调用、费用）都按会话存储的规则落盘：原子快照，执行前先记日志，不确定的调用从不重放。状态记录在 `<数据目录>/agents/<项目哈希>/`。
- **状态机**：`running → paused / interrupted / finished / failed → discarded`，所有转换都经过类型化的状态机校验。进程重启后，原来 `running` 的 Agent 显示为 `interrupted`。
- **通知**：Agent 结束时，父 Agent 会收到带最终报告的通知：任务进行中，在下一次工具结果里送达；任务之间，在下一个任务开头送达。没有改动的 worktree 会标为“worktree has no changes”。屏幕上：报告已经交给模型的，只打印一行 `后台代理 <id>（<类型>）已完成，报告已交给主任务；查看全文：/status agents show <id>`；还没交给模型的（任务之间结束），打印状态（原因、失败的工具调用、分支和提交）和完整报告。
- **费用**：后台 Agent 的费用经 Ledger 记入父会话。
- **审批**：后台 Agent 使用同一权限模式。default 模式下，审批请求排队，在任务之间到终端上逐个询问（REPL 空闲时会提示按 Enter 处理）；没有终端时直接拒绝，绝不静默批准。
- **单次模式**：`-p` 单次模式结束前会等后台 Agent 跑完，最长等 `--timeout`，超时后中断它们（可恢复）。REPL `/exit` 会中断仍在运行的后台 Agent。

用户命令统一放在 `/status agents` 下，`main` 指前台任务：

| 命令 | 行为 |
| --- | --- |
| `/status agents` | 列出 id、类型、状态、原因、模型、时长、费用、任务、最后一步、分支及其是否有未提交改动 |
| `show <id>` | 显示后台 Agent 的状态和完整报告 |
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

## 外围工作流（/race、/changes、/remote）

所有功能默认关闭，只在显式命令下运行。结果放在数据目录（`<data-dir>`），**除 `/changes apply … confirm` 外不写入项目工作区**。子进程（验证命令、浏览器、git）都在同一套进程树管理下运行（Windows Job Object / Unix 进程组），取消或超时时整棵树回收。

### 命令一览

| 命令 | 作用 |
| --- | --- |
| `/race <需求>`、`/race` | 一个需求两个方案（见下文），前台运行，Ctrl+C 取消；不带参数查看最近一次 |
| `/changes` | 列出所有变更集：竞速方案 `race:<run>:<a\|b>`、当前任务 `turn` |
| `/changes show\|diff <id>` | 查看元数据（基线、补丁哈希/字节、检查结果、审阅状态）或补丁 |
| `/changes export <id> <out>` | 把补丁写到项目外的新文件，不覆盖已有文件 |
| `/changes apply <id>` | 只做预检；加 `confirm` 才真正应用 |
| `/changes review <id> accepted\|rejected` | 记录审阅决定，不应用 |
| `/changes impact [turn\|<git 区间>]` | 改动影响：改了哪些符号、其他文件里谁在用、有没有测试引用。不带参数等于 `turn`（当前任务用文件工具做的改动）；区间认 `A...B`（从共同祖先算起，审阅 MR 用 `origin/main...HEAD`）、`A..B` 和单个提交 `X`（这个提交自己的改动，即 `X^..X`）。调用方按当前工作区查找；只读，不调用模型，最多列 50 处，连标题和用时最多输出 30 行 |
| `/changes verify-browser <workflow.json> [--allow-local]` / `results` | 浏览器实操验收 / 查看历史报告 |
| `/remote start [--bind 127.0.0.1:0] [--token-file <路径>] [--allow-lan --tls-cert <pem> --tls-key <pem>]`、`status`、`stop` | 远程监督 |

### 统一变更集与应用

竞速方案和当前任务都用同一个模型表示：基线 `{root, commit, branch}`、补丁 SHA-256 与字节数、验证记录（只看退出码，按最后一次尝试判断）、审阅状态（pending/accepted/rejected/applied）。`apply … confirm` 依次检查：可应用、项目根/分支/提交与基线一致且工作区干净（无已跟踪改动、无未跟踪文件）、最后一次检查全部通过、补丁大小与哈希一致；再在项目级文件锁（最多等 10 秒）下执行 `git apply --check` 和 `git apply`。最后的 `git apply` 不响应取消，避免半途中断。应用后不提交、不合并、不推送，并在会话中记一条说明，之前的验证证据视为过期。当前任务的变更集 `turn` 只读，撤销用 `/undo`。

干净检查**允许被忽略的文件**（例如 `target/`、`node_modules/`）：补丁不含它们，且 `git apply` 不会覆盖已存在文件，所以不要求先清掉构建产物。

### 一个需求两个方案（`/race`）

要求项目根是本地 Git 仓库、处于分支上且工作区干净。`/race <需求>` 同时做两个方案：

- 方案 A 用主模型；方案 B 用快速模型（`model_fast`）。没有配置快速模型或与主模型相同时，B 也用主模型，并被要求采用与最直接的做法不同的思路。
- 每个方案是一个可编辑的子 Agent（`general` 类型，调用上限同 `subagent_max_iterations`），在 `<data-dir>/race/<run>/wt-a|wt-b` 的分离 worktree（从 HEAD 创建，禁用 Git hooks）里运行，和主任务在同一进程、用同样的工具（包括 MCP、Hooks）。权限是 `auto`，判定范围是自己的 worktree；仍需审批的命令排进后台审批队列，没有终端时拒绝。
- 费用记入当前会话和费用账本。
- 两个方案完成后，在各自 worktree 运行同一组验证命令（不经 Shell，只看退出码）：配置了 `done_verify_commands` 时用它，否则按自动验证门禁的规则推断构建和测试命令（`cargo check`/`cargo test`、`go build`/`go test`、`npm run build`/测试脚本、`dotnet build`、Python 的 `compileall`/`pytest` 等）。推断不出时结果标为"未验证"。
- 清理被忽略文件后捕获二进制补丁（符号链接、子模块、`.git` 路径会使方案失败），报告与补丁存到 `<data-dir>/race/<run>/`，worktree 随后删除。
- 总时限 15 分钟，约 2/3 留给生成。结果列出每个方案的模型、验证结果、改动的文件数与增删行数、费用和 Agent 的总结。
- 模型自评不能决定通过：只有通过验证的方案可以 `/changes apply race:<run>:a confirm`；**未验证的方案不能直接应用**（可 `/changes export` 后自行检查、手动 `git apply`）。一次运行只能应用一个方案。

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

Git worktree 不是操作系统、网络或进程沙箱：竞速方案的 Agent 和验证命令拥有宿主权限，Git 管理数据共享，只对可信项目、提示和命令使用。补丁、输出和截图可能含敏感信息，不要同步数据目录。费用只是估算，cove 不限制支出。宿主崩溃可能遗留 worktree（位于数据目录下），需要人工清理。

## 尚未实现与已知限制

- 界面是滚动输出加常驻输入行的命令行控制台，不是全屏 TUI：输出按行追加，进度条这类原地刷新的输出会显示成多行；鼠标只在历史阅读器（`/resume`、`/turns`）里可用（滚动、拖选复制）。
- 剪贴板图片（Alt+V）只支持 Windows；其他平台可以粘贴或拖入图片路径。
- 不支持 MCP 的采样、交互 elicitation 和服务端后台任务（OAuth 登录见上文 `/mcp login`）；插件不加载原生动态库或可执行代码；`bedrock` 不支持 SigV4 签名（只支持 Bedrock API key）。
- 写入模式的子 Agent 和后台 Agent 使用与主任务相同的权限判定，但不经过主任务的计划模式工具和提交前验证包装。
- 后台 Shell 命令不能接收输入（没有 `write`），也不跨 cove 重启保留：cove 退出时随进程树结束；重启后，此前转到后台的命令的结果不会再交给模型。

自动化测试只用模拟模型协议和临时项目中的真实文件/进程操作，不调用真实供应商的付费 API，也未完成各模型的兼容认证。交互控制台的显示在 Windows 伪终端里按字符核对过颜色和排版；其他终端（特别是旧版 conhost 字体）对 `●`、`│`、`└` 等符号的显示可能不同，转圈动画在无法确认支持盲文点字符的终端里改用 `- \ | /`。

## 验证

[持久化测试](../tests/all/persistence.rs)覆盖会话租约、状态损坏、结果不确定时禁止重放、继续时保留失败、完整归档与压缩、累计计费、文件漂移和回退边界。[CLI 测试](../tests/all/cli.rs)另外覆盖管道 REPL、多轮历史、跨进程继续、`/handoff` 交接（请求前缀与上一轮逐字节相同、新会话只带交接消息、原会话不变）与 `/back`、恢复后的 diff/回退，以及未知命令与身份不匹配。测试使用隔离临时数据目录，不写入用户默认会话目录。

[项目命令测试](../tests/all/project.rs)覆盖分组帮助与未知命令建议、`-p` 管道 stdin 与 `--file`、退出码、`--dump-system-prompt`、无凭据的 `--doctor`、轨迹与错误日志不含正文、`/init` 起草与不覆盖写入、`/commit` 的暂存/`--all`/`--only` 与验证、`/expand`，以及 `/cd`、`/restart` 的重启。

[扩展集成测试](../tests/all/integrations.rs)覆盖官方 MCP SDK 对真实本地 stdio 服务的握手、分页、调用、资源、断线和进程树取消；Skills、隔离记忆、插件组合，以及 Hook 拒绝、中断和防重放。MCP 测试夹具需要 Python 3（Windows 为 `python`，其他平台为 `python3`）；运行时使用 MCP 本身不依赖 Python，只有所选服务需要其自身运行时。CI 显式准备 Python。

[Agent 测试](../tests/all/agents.rs)在临时 git 仓库中用模拟模型覆盖以下场景：并行 `execute_plan` 的依赖顺序和结果传递；没有回归证据的 verify 记为失败；`regression` 先在基线上失败、再在当前代码上通过，并恢复文件；worktree 内的子 Agent 及合并回主工作区；实验工具开关和自定义类型；后台 Agent 的完整流程（启动 → 进程退出被中断 → 新进程中显示 interrupted → 带上下文恢复 → 通知 → 合并）；父任务在下一轮工具调用时收到完成通知；主任务暂存（park）后在新进程中带消息唤醒；同一批两个 explore 并行、general 在它们之后执行，explore 走快速模型并按快速模型的价格记账（会话和用量账本一致），trace 记实际模型和 agent id；超过 6 KB 的答复在父会话里被截断、全文存盘，`--explore-model main` 让 explore 改用主模型。覆盖层计划（`isolation: "overlay"`）：两个条目并行改同一个文件的不同函数、合并前主工作区不变、合并结果正确，验证在合并之后才跑；改到同一处时重做后合并，重做没解决时不写入并列出副本；同一处的条目自动叠层；条目里的构建和写盘 shell 被拒；没申报 `writes` 时拒绝；`-p` 下非 auto 模式退回 `main`；共享检查槽排队运行并看到各自的层。

```powershell
cargo fmt --check
cargo clippy --all-targets --locked -- -D warnings
cargo test --locked
cargo build --release --locked
```

[安全与闸门测试](../tests/all/safety.rs)用模拟 SSE 服务覆盖各权限模式、灾难命令拦截、拒绝规则、计划模式、熔断、没有进展时止损（shell 改了文件不算没进展）、调用上限也打印 `[summary]`、复用模型自己的检查结果、验收报告、引用核实（错的引用催一次、只有行号偏移时只出通知）、密钥提示和 `/mode save`。[核心测试](../tests/all/core.rs)覆盖权限、精确编辑、路径限制、搜索、费用阈值、进程输出/超时/取消及子进程树清理，以及审批被拒绝后理由交回模型、任务继续且不计为失败。[渲染单元测试](../src/app/render.rs)覆盖流式 Markdown 在任意分片下的样式（包括跨分片延续）、表格对齐、工具标题与结果摘要。[CLI 测试](../tests/all/cli.rs)用本地模拟 SSE 服务验证两类协议、真实编辑和命令检查、默认模式放行项目内写入、断流以及任务 deadline。测试不需要真实 API key，不访问真实供应商。

上面的测试只检查机制有没有坏，不衡量真实模型配合当前提示词做得好不好。效果用本机的 `evals/`（不在仓库里，见本机 README）里的真实模型评测衡量：每个用例是一个小样例仓库加一项任务，cove 以 `-p` 在临时目录里运行（独立数据目录，不读你的记忆和规则），结束后由用例自己的检查判定，检查内容包括隐藏测试、文件状态、git 状态和回复里的关键事实，不采信 cove 自己说的"完成"。超时、崩溃或检查出错记为"未知"，不算通过。评测要用真实 key、会产生费用，不在 `cargo test` 和 CI 里运行：

```powershell
python evals/run.py --self-test                 # 不调用模型，确认每个用例的判定可靠
python evals/run.py --profile deepseek --repeat 3
python evals/run.py --baseline evals/results/<上次结果>.json   # 和基准对比
```
