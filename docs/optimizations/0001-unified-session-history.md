# 0001 统一历史会话的数据、标题与展示

- 状态：已完成
- 日期：2026-11-18（创建）；2026-11-18（完成）
- 关联：demo `d:\github\cove-history-demo`（交互原型，已经用户确认）

## 背景

用户在使用 `/continue` 时发现列表重复显示了两次。往下排查时又发现：历史会话的列表、轮数、标题、时间和展示，在 cove 里分散在很多地方，**每处各自实现、规则互不一致**。同一个会话在不同地方显示的轮数不同，标题不同，时间一处是 UTC、一处是本地时间。用户的原话是"历史列表或者历史记录有好多不同的地方使用到，都各自实现没有统一"。

交互方面已经用 demo 打磨过几轮，用户确认的最终样子是：会话列表面板，加一个全屏阅读器（上方是你的输入列表，下方只显示结论，支持拖选复制）。

## 现状与问题

**P1 同一画面重复。** `/continue` 先 `say()` 打印一份文本列表，再弹出 Choice 选择器，两份内容一样（`src/commands/sessions.rs` 的 `continue_sessions`）；`/resume` 和 `/resume search` 也一样（`history` → `offer_picker`）。

**P2 "什么算用户的一轮"有 6 套规则：**

| 规则 | 使用方 | 偏差 |
|---|---|---|
| `synthetic()`：以 `[` 开头一律排除 | `user_turns`、`has_conversation`、`first_request`、`transcript`、`derived_title`、`resume_score` | 运行中插话不算；用户粘贴的 `[Range(...)]` 代码也不算 |
| `conversation_input()` | `conversation_turns`（`/turns`）、`latest_task_title` | 与上一行口径不同，同一会话的轮数对不上 |
| `!starts_with('[')` | `replay_html` | 把 `Operator …`、`Workspace restored…`、压缩摘要当成用户的一轮 |
| 完全不过滤 | `markdown`（`/resume export`） | 把注入的 `[Saved memories…]`、`[Session notes…]` 当作用户发言导出 |
| 每条用户消息的第一个块 | `search_text`（`/resume search`） | 会搜到系统注入的提示 |
| 每条 AI 消息的第一段文字 | `memory/dream.rs` 的会话摘要 | 它的"final reply"其实是最后一条 AI 消息的第一段，不一定是结论 |

证据：用真实数据（`~/.cove/sessions`，55 个有对话的会话）对比，**11 个会话的轮数在两种口径下不一样**；`[` 开头的用户输入里，有 2 条其实是用户粘贴的 C# 代码。

**P3 根源：来源靠事后猜。** 用户真实输入、插话、注入的记忆和笔记、压缩摘要、系统通知，都存成普通的 `Block::Text`，读取时再靠文字前缀判断（`src/domain.rs` 的 `Message` 只有 `role` 和 `blocks` 两个字段）。

**P4 标题有 4 种取法，而且质量差。** `display_title`、`latest_task_title`、`/turns` 的 `title_from`/截取前 60 字、dream 用的 `title` 或 `current_request`。标题本质上是第一条输入的前 60 个字，长问题会被截断，"继续""好的"这类输入还会得到"(untitled)"。

**P5 压缩后看不到以前的对话。** 压缩会替换会话里的旧消息，但 `src/engine/agent.rs:701` 在压缩前已经把完整快照存进了 `~/.cove/archives`，只是没有任何地方去读它。

**P6 时间口径混乱。** 列表、详情、导出用 `usage::timestamp`（UTC，截图里的 `05:47` 实际是北京时间 13:47），启动提示用本地相对时间。

**P7 会话范围各自过滤。** `/continue` 取前 5 个而且包含当前会话；`/resume` 的选择器排除当前会话；`pick` 按 id 前缀查找时包含空会话；dream 也包含空会话。

**P8 展示有 6 处各写各的。** `history_lines`、两个 Choice 选择器、`/turns` 的 TurnBrowser、`transcript`（恢复会话时打印的"之前的对话"）、`/resume detail`、启动提示。

## 目标

1. 判断"一轮"、取标题、确定会话范围、格式化时间，**各只有一份实现**；所有使用方都调用它，同一会话在任何地方显示的轮数和标题都一致。
2. 新会话的消息在写入时就标明来源；旧会话自动按统一后的规则兜底，不需要迁移。
3. 会话标题由 AI 生成，随对话推进更新；用户可以改名，改过名的不再被 AI 覆盖。
4. 交互界面和 demo 一致：列表只显示一份；阅读器能看到压缩之前的轮次。
5. 现有的非交互输出（管道、`-p`、`--list-sessions`）保持文本格式，测试继续通过。

## 范围

- **做：** 第 0 到第 3 层（见"方案"）；AI 标题；`/rename`；选择器的数字键 1–9 和 `Ctrl+A`、`Ctrl+R`；把 `/turns` 换成阅读器；dream 的 final reply 改为取结论。
- **不做：**
  - `/undo` 和检查点的列表改造：属于另一个功能。但第 1 层的轮次接口要能支撑它，以后另开文档。
  - 会话保留期和清理策略：现有的 `MAX_SESSIONS` 修剪规则不变。
  - 把会话存储改成只追加（Claude Code 的做法）：改动太大；用读取归档来达到"能看到压缩前的对话"的效果。
  - 阅读器展开工具调用：用户明确表示工具调用没有意义。

## 调研与参考

**Claude Code**（本机 `~/.claude/projects` 下 99 个会话文件，以及 code.claude.com 官方文档）：
- 每条用户记录写入时就带 `origin.kind`（`human`/`peer`/`task-notification`/`scheduled`）和 `promptSource`；注入的内容是独立的 `attachment` 记录；压缩用 `compact_boundary` 和 `isCompactSummary` 标记。旧版本的记录没有 `origin`，用 `isMeta` 兜底。→ 采纳为**第 0 层**。
- 会话文件只追加，压缩后之前的对话仍然可见。→ cove 改为**读取已有的归档**来达到同样效果。
- 标题由小模型在后台生成（`ai-title` 记录，随对话多次更新）；可以用 `/rename` 或在选择器里按 `Ctrl+R` 改名，改过的名字优先。→ 采纳。
- 选择器支持 ↑↓、Enter、数字键 1–9、`/` 筛选、`Ctrl+A` 切换到所有项目、Space 预览、`Ctrl+R` 改名。→ 采纳数字键、`Ctrl+A`、`Ctrl+R`；预览由阅读器承担（Tab）。
- `/rewind` 只列出用户的提问。→ 与阅读器的输入列表概念一致，以后做 `/undo` 时复用。
- 不采纳：恢复会话时打印全部历史（cove 是内联终端，会刷屏）；默认折叠、按 Ctrl+O 展开工具调用（用户不需要工具调用）。

**demo**（`d:\github\cove-history-demo`）：交互已和用户确认，包括列表分组、预览字段、阅读器布局、按键、拖选复制、`c`/`o`。真实数据核对过两点：只显示最后一段回复不会漏掉结论（65 轮多段回复里没有反例）；30% 的轮次没有中间过程。

## 方案

### 第 0 层：消息来源（`src/domain.rs`）

`Message` 新增一个字段：

```rust
#[serde(default, skip_serializing_if = "Origin::is_unknown")]
pub origin: Origin,

pub enum Origin { Unknown, Human, Steer, Injected, Summary, Notice }
```

- `Unknown`：旧文件读进来时的默认值，由第 1 层的文字规则兜底；
- `Human`：用户输入的请求；`Steer`：任务运行中的插话，以及排队后送达的消息；
- `Injected`：注入的记忆、笔记、环境信息等；`Summary`：压缩摘要；`Notice`：程序自己加的提示（验证失败、Operator、工作区恢复等）。
- 所有写入用户消息的地方，改用 `Message::human` / `Message::steer` / `Message::notice` 等构造函数显式标明。`Message::user` 保留，产生 `Unknown`，只在测试和确实无法归类的地方使用。
- 发给模型的请求不带这个字段（provider 转换时忽略），对模型行为没有影响。

### 第 1 层：轮次（`src/session/mod.rs`）

- 一个判断函数 `turn_start(message) -> Option<TurnStart>`：先看 `origin`；是 `Unknown` 时用统一的文字规则（就是 demo 里验证过的规则：插话去掉前缀后算一轮；压缩摘要里保留的那条请求算一轮；只排除已知的注入和通知前缀；其余都算用户输入）。
- `Session::turns() -> Vec<Turn>`：每一轮包含提问、结论（最后一段文字回复）、图片数、是否插话，以及这一轮的消息范围。
- `Store::full_turns(&Session)`：把 `archives` 里这个会话的快照按压缩次数排序后拼接，再接上当前会话；保留的那条请求在前后两份里重复出现时去重，并在压缩的位置做标记。
- `user_turns`、`has_conversation`、`first_request`、`derived_title`、`conversation_turns`、`transcript`、`search_text`、`markdown`、`replay_html`、`detail`、dream 的摘要，**全部改为基于 `turns()`**。删除 `synthetic()` 里"`[` 开头一律排除"这条规则。

### 第 2 层：会话摘要与 AI 标题

- `SessionSummary`：id、标题、工作区、更新时间、状态（未完成 / 已中断）、轮次（提问和结论）、模型、花费、改动行数、是否压缩过。
- 统一的列表入口 `Store::summaries(Scope { all_workspaces, exclude: Option<id>, include_empty })`。`/continue`、`/resume`、`--list-sessions`、`pick`、`most_relevant`、dream 都通过它取会话。
- 时间：新增 `session::time::{local, relative, group}`（今天 / 昨天 / 最近 7 天 / 更早），列表、详情、导出都改用本地时间；`usage::timestamp` 只留给日志等机器读取的地方。
- **标题：** `Session` 新增 `title_source: TitleSource { Prompt, Ai, User }`（默认值为 `Prompt`）。
  - **AI 标题**：仿照 `memory/extract.rs` 的 `Extractor`，新建 `session/titles.rs` 的 `Titler`（实现 `TurnHook::task_end`）。任务完成后，在后台调用一次当前 provider（30 秒超时）：输入最近几轮的提问和结论，输出一个不超过 20 个字的中文标题。
  - **更新时机**：第 1、2、3 轮完成后各生成一次，之后每 5 轮更新一次。对话前几轮主题还没稳定，所以前几轮更新得勤一些。
  - **何时不生成**：`title_source == User`（用户改过名）、`-p` 单次运行、以及本地或私有网络上的模型（沿用 `extract::is_local` 的规则）。
  - **写回**：生成的标题先放进一个共享槽位；`Application` 在两次任务之间把它写进当前会话并保存。如果会话已经切换走了，就加锁后写回存储里的那个会话；拿不到锁就放弃这一次。
  - **改名**：`/rename <名字>`，以及在选择器里按 `Ctrl+R`，会设置 `title_source = User`。
  - 失败时保留原来的标题，不报错，只在 `/status` 里显示最近一次生成的情况。

### 第 3 层：展示

- **交互**：新建 `src/app/history.rs`，包含会话列表面板和阅读器（行为与 demo 一致），`console.rs` 只负责接线：
  - 列表面板挂在输入框下方，输入框里的文字用作筛选条件；支持 ↑↓、PgUp/PgDn、Home/End、数字键 1–9、Enter 选定、Tab 打开阅读器、`Ctrl+A` 切换范围、`Ctrl+R` 改名、Esc 关闭；
  - 阅读器使用备用屏幕，并开启鼠标捕获：↑↓ 和单击切换对话，滚轮和 PgUp/PgDn/空格滚动回答，拖选即复制（可跨屏，拖到边缘自动滚动），`c` 复制整段回答，`o` 在编辑器里打开，`/` 筛选，Esc 逐层返回；
  - 阅读器打开期间，其他输出先缓存，退出后再写出；有审批或提问时自动退出阅读器。
- `/continue`、`/resume`、`/resume all`、`/resume search` 都用这个面板，删除两个 Choice 选择器（`offer_picker` 和 `continue_sessions` 里的那段）。`/continue` 改为列出本工作区的全部会话，并且排除当前会话。
- `/turns` 用阅读器打开当前会话，删除 `TurnBrowser`。
- 恢复会话时打印的"之前的对话"：只显示最近 3 轮的提问和结论。
- **非交互**：只保留一个 `history_lines`（改用本地时间，轮数用新口径）。
- `render::wrap_cell` 改为 `pub(crate)`，供阅读器折行使用。

## 影响范围

- 文件：`src/domain.rs`、`src/session/mod.rs`、新增 `src/session/time.rs`、新增 `src/session/titles.rs`、`src/engine/agent.rs` 和 `src/engine/context.rs`（写入消息时标明 origin）、`src/commands/sessions.rs`、`src/commands/workflows.rs`、`src/app/console.rs`、新增 `src/app/history.rs`、`src/app/render.rs`、`src/app/mod.rs`（挂接 Titler、写回标题）、`src/memory/dream.rs`。
- **数据格式**：会话 JSON 新增 `origin`（消息上）和 `title_source`（会话上），两个字段都带默认值。旧文件可以正常读取。旧版本的 cove 读到新文件时会忽略这两个字段，不影响使用。
- **行为变化**：轮数会变化（插话算一轮，压缩前的轮次也算进去）；列表时间改为本地时间；标题会被 AI 更新；`/continue` 不再列出当前会话。
- **费用**：每个会话在第 1/2/3/5/10/… 轮完成后各多一次短的模型调用（用户确认不考虑费用）。

## 验证

- **单元测试**：`turn_start` 的分类（每种 origin、每种旧前缀、粘贴 `[` 开头的代码）；`turns()` 处理压缩摘要和插话；归档拼接时去重；`SessionSummary` 的标题优先级（User > Ai > Prompt）；`Titler` 的触发时机和跳过条件（使用模拟 provider）；`history.rs` 的列表和阅读器渲染、按键、鼠标选择与 `selected_text`（把 demo 里的自检场景改写成测试）。
- **回归**：`cargo test` 全部通过；`tests/all/cli.rs`、`tests/all/sessions.rs` 依赖的非交互输出保持不变。
- **真实数据**：用 `~/.cove/sessions` 跑一次对比，确认列表、`/turns`、`detail`、导出、回放显示的轮数一致；压缩过的会话（比如 `1b2326eb`）能在阅读器里看到压缩前的轮次。
- **人工确认**（由用户在终端里做）：列表只显示一份；阅读器的按键和拖选复制；新会话完成第 1 轮后标题变成 AI 生成的。
- 格式化：只对改动过的文件运行 `rustfmt --edition 2024 --config max_width=100`。

## 实施计划

- [x] 阶段 0：`Origin` 字段和构造函数；所有写入用户消息的地方标明来源
- [x] 阶段 1：`turn_start`、`turns()`、归档拼接；替换各个使用方；删除旧规则
- [x] 阶段 2：`SessionSummary`、统一的列表入口、本地时间；`title_source`、`Titler`、`/rename`
- [x] 阶段 3：`app/history.rs` 和 console 接线；替换 `/continue`、`/resume`、`/turns`；恢复会话时的显示
- [x] 回填验证结果
- [x] 用户人工确认交互界面后，状态改为"已完成"

## 待决策

无。

## 决策记录

- 2026-11-18：交互以 demo 为准：列表加全屏阅读器，阅读器只显示结论、不显示工具调用，拖选复制必须可用。（用户在 demo 迭代中逐项确认）
- 2026-11-18：采纳本文"方案"一节列出的全部建议：`/turns` 改用阅读器；恢复会话时只显示最近 3 轮的提问和结论；导出和回放保留完整过程，但轮次改用统一规则；`/continue` 列出全部会话并排除当前会话。（用户："按照你建议的改"）
- 2026-11-18：做 AI 标题，不考虑费用；其余部分按简单的方式实现。（用户）
- 2026-11-18：AI 标题的更新时机定为第 1、2、3 轮之后各一次，之后每 5 轮一次。理由：前几轮主题还没稳定；Claude Code 也是随对话更新标题。（实施方提出，作为默认值；用户如有意见可调整）
- 2026-11-18：修正"`/continue` 排除当前会话"这一条：当前会话有未完成的任务时，把它排在第一位并标"当前"；只有当前任务已经完成时才排除它。`/continue <编号>` 在没有先列出列表时，也按这份列表计数。`/resume` 仍然排除当前会话。理由：集成测试 `continue_lists_recent_sessions_and_selection_does_not_replay_uncertain_calls` 和 `failed_request_can_continue_without_resetting_budget` 表明，`/continue` 最主要的用途就是继续当前被中断的任务，原来的规则会把这个场景搞坏。（实施方，阶段 2 发现）
- 2026-11-18：`SteerHook::tool_note` 会把插话追加在工具结果的内容里，而不是作为单独的消息写入。统一规则会识别这种插话并算作一轮；发给模型的格式不变。（实施方，阶段 0 发现）
- 2026-11-18：`/resume` 原来计算"另有 N 个其他工作区的会话"时，把空会话也算了进去；改用统一入口计数后修正。（实施方，阶段 2 发现）

## 验证结果

**自动测试**（2026-11-18）：`cargo test` 全部通过，单元测试 405 个、集成测试 224 个。新增的测试有：
- `session::turns`：按 origin 分类，以及旧文本兜底（插话、通知、`[` 开头的用户代码、压缩摘要）；轮次切分和结论选取（包括写在工具结果里的插话）；归档拼接时去重；
- `session::titles`：生成时机（1、2、3、5、10、15 轮）、模型回复的清洗（引号、句号、前缀）、提示内容（第一轮加最近几轮）；
- `session::time`：相对时间和分组；
- `app::history`：列表的筛选、数字键、提交、`Ctrl+R` 预填；阅读器切换轮次、只显示结论、Enter 和 Esc；拖选跨行取词，按光标下的字计算；滚轮只滚动、不切换轮次；
- `app::console`：面板用输入框筛选并提交选择；有提问时关闭历史视图；任务运行时历史视图不挡住插话、Esc 和 Ctrl+C。

**按新行为修改的已有测试**：`history_lists_titles_resumes_by_number_and_exports_markdown`（`/resume` 不再列出当前会话）；`turns_list_each_request_and_show_its_conclusion`（原名 `turns_lists_each_conversation_and_shows_the_complete_tool_result`，`/turns` 不再显示工具调用和结果）。

**真实数据**（`~/.cove`，用 `cove --list-sessions all` 只读查看）：
- 时间为本地时间（`845fee4e` 显示 15:03，以前显示 UTC 07:03）；
- 压缩过的会话拼接了归档里的轮次：`07b2f81a` 从 4 轮变为 40 轮，`510273bb` 从 2 轮变为 23 轮，`1b2326eb` 从 17 轮变为 30 轮。逐条核对了 `07b2f81a` 的 40 轮提问：35 轮来自归档、5 轮来自当前会话，衔接连续，没有重复；
- "继续/hi"这类会话不再显示"(无标题)"（`bceebb4c` 改用结论开头作为标题）；
- 路径不再带 Windows 的 `\\?\` 前缀。

**格式化**：只对改动过的文件，通过标准输入运行 `rustfmt --edition 2024 --config max_width=100`（不会顺着 `mod` 声明去格式化没改动的子模块）。

**人工确认**（2026-11-18，用户确认通过）：`/continue`、`/resume` 只显示一个面板；阅读器的按键、滚轮、拖选复制、`c` 和 `o`；新会话完成第一轮后，标题变成 AI 生成的。

## 遗留

- AI 标题还没有在真实模型上跑过：自动测试用的 mock 服务器在 localhost 上，按规则不会生成标题。需要用户用远程模型实际对话一轮后确认，`/status` 里会显示最近一次生成的结果。
- `/undo` 和检查点列表还没有改用统一的轮次接口（本次范围外，以后另开文档）。
- 阅读器每次重画都会重新渲染当前这一轮的 Markdown；回答特别长时，如果出现卡顿，可以加缓存。
- 集成测试 `all_hook_events_run_with_scrubbed_environment_and_session_end_once` 在全量并行运行时偶发超时失败，单独运行稳定通过。它用的是 `-p` 模式，和本次改动无关，是原来就有的时序问题，以后另开文档处理。
- Choice 选择器（`console.choose`）还保留着，其他命令（比如模型选择）还在用它；会话历史已经不再使用。
