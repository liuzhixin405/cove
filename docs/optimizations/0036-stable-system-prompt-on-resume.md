# 0036 恢复会话时沿用原来的项目上下文，让缓存继续命中

- 状态：已完成
- 日期：2026-11-24（创建）；2026-11-24（完成）
- 关联：0013（1 小时缓存）、0035（压缩请求续写）

## 背景

退出后用 `--resume`、`/restart` 或 `/cd` 回到同一个会话，cove 会在新进程里重新生成系统提示词。项目里新增一个文件，或者切换一次分支，系统提示词就和上一个进程不一样了。系统提示词在请求的前部，它一变，后面整段对话历史的缓存都读不到了，恢复后的第一个请求按全价重新写一遍缓存。会话越长，这一次越贵。

## 现状与问题

1. **项目上下文每个进程现算一次。**
   - `src/app/mod.rs:203-212` 在启动时调用 `prompt::system_context`。
   - 生成的内容在进程内不变（`prompt.rs` 注释："stable for the whole process"），但没有存进会话。
   - 跨进程恢复时重新计算。
2. **这段内容会随工作区变化。** `system_context` 包含四部分：
   - `environment`（`prompt.rs:126-141`）：工作目录、平台、shell、**git 分支**、git 用户；
   - 用户偏好（`~/.cove/preferences.md`）；
   - 项目说明文件（`AGENTS.md` 等）；
   - `outline`（`prompt.rs:200-290`）：**各语言的文件数**、顶层目录列表、入口文件、构建命令。

   新增、删除一个源码文件，或者顶层多一个文件，outline 就变；切分支，environment 就变。
3. **在当前版本上复现**：临时 git 仓库里有 `Cargo.toml` 和 `src/main.rs`，用 `cove --dump-system-prompt` 打印一次；新建 `src/lib.rs` 后再打印一次。两次输出有 3 行不同：
   ```
   < Languages: Rust (1 files)
   > Languages: Rust (2 files)
   < Entry points: src/main.rs
   > Entry points: src/main.rs, src/lib.rs
   ```
   另有顶层目录一行不同。编程任务几乎都会新建文件，所以"恢复后系统提示词变了"是常态，不是特例。
4. **缓存失效的范围**：
   - Anthropic（Bedrock）：缓存按前缀匹配，顺序是 tools、system、messages，system 一变，后面全部 messages 都读不到缓存。
   - DeepSeek：缓存按前缀自动匹配，变化点之后的内容同样读不到。

   两家都是从变化点往后整段重算。

## 目标

- 恢复一个已有对话的会话时，系统提示词和上一个进程最后一次请求的系统提示词逐字节相同。恢复后的第一个请求能读到缓存（用量里 `cache_read` 占输入的大部分）。
- 项目说明文件或用户偏好在这期间被改了，模型仍然能知道：变化作为一条消息追加到对话里，不改系统提示词。
- 新会话、`/new`、压缩之后，用当前算出的项目上下文。压缩已经重写了消息，这时更新系统提示词不会额外损失缓存。

## 范围

- 做：
  - 会话里保存项目上下文；
  - 恢复时沿用；
  - 说明文件和偏好变化时追加一条提示；
  - 压缩后、`/new` 时更新。
- 不做：
  - cove 升级后系统提示词的基础部分改了：不可避免，也不该沿用旧版本的提示词。
  - `max_turns` 这类配置改了导致的变化：很少发生。
  - outline 和 git 分支的变化不提示模型：outline 只是给模型的方向感，模型随时可以用 `repo_map`、`git` 查到最新的情况。
  - 子 agent（explore、review）：它们每次都是新的对话，没有可以沿用的缓存。

## 调研与参考

- 同类工具常见做法：环境信息在会话开始时生成一次，之后的变化以消息形式告知模型（未逐一核实各工具的实现，不作为依据）。
- cove 内部已有类似做法：`/rules` 新增的规则"announced to the model as they are added"（`src/app/mod.rs:200-201` 注释），通过消息告诉模型，不改系统提示词。
- 取舍：沿用旧的 outline，模型可能看到稍旧的文件数。这只影响方向感，不影响正确性；换来的是长会话恢复时不再全价重算。说明文件和偏好会影响模型行为，所以变化必须告诉模型。

## 方案

1. **保存**：`Session` 新增可选字段 `system_context: Option<String>`，记下第一次发请求时用的项目上下文。旧会话没有这个字段，按"没有可沿用的"处理。压缩、`/new` 之后，把这个字段更新为当前进程算出的值。
2. **恢复时沿用**：
   - 加载的会话有对话历史，并且有 `system_context` 时，这个会话的请求使用保存的内容，不用本进程新算的。
   - 主 agent 和 `write_handoff`、续写压缩（0035）都会读这个值，需要确认它们取到的是同一个值。
3. **变化提示**：恢复时把新算的上下文和保存的对比，只比较"项目说明文件"和"用户偏好"两节。有变化时，在下一条用户消息前追加一段说明："本会话开始后，以下内容有变化：<文件名>（新内容附后）……"。
   - 内容超过上限时，只给文件名和"请用 read 查看"。
   - 说明只追加一次；之后把保存的 `system_context` 中的这两节视为已经告知，具体做法是记录一个哈希。
4. **观察**：trace 的 `kind=model` 记录已经有 `in` 和 cache 字段。另外在 `kind=task` 记录里加 `system_reused: true|false`，便于以后统计。

## 影响范围

- `src/session/mod.rs`：会话格式新增可选字段，向后兼容。旧版本 cove 读新会话时会忽略这个字段，需要确认 serde 没有设置 `deny_unknown_fields`。
- `src/app/mod.rs`：启动和恢复时选择用哪一份项目上下文。
- `src/engine/agent.rs`：`project_context` 从会话里取。
- `src/engine/prompt.rs`：提供"只比较说明和偏好两节"的函数。
- 行为变化：恢复的会话里，模型看到的 outline 和分支信息可能是旧的。

## 验证

- 单元测试：
  - 恢复的会话沿用保存的上下文，`system_prompt()` 和保存时逐字节相同；
  - 只有 outline 变化时不追加提示；
  - AGENTS.md 变了时追加一次，之后不再追加；
  - 压缩以后用新的上下文；
  - 旧会话没有这个字段时用新算的。
- 集成：临时仓库里跑 `cove -p` 一次，新建一个文件，再用 `--resume <id> -p` 跑一次。用假提供方断言两次请求的 system 相同。
- 实际运行（Bedrock Opus 5.5）：
  - 同样的操作用真实模型做一次，第一个任务让会话积累几千 tokens；
  - 看账本里恢复后第一次请求的 `cache_read`；
  - 和改动前的版本对比，改动前用 `%TEMP%\cove-snap\0036` 构建，或者在验证结果里写明改动前的同一组数字。
- `cargo test --release`、`cargo clippy --release --all-targets`（不新增警告）。

## 实施计划

- [x] 阶段 1：会话字段、恢复时沿用、压缩和 `/new` 时更新
- [x] 阶段 2：说明和偏好的变化提示
- [x] 阶段 3：trace 字段、测试、实际运行对比

## 待决策

1. **git 分支变了要不要提示？** 建议不提示。分支信息只影响方向感，模型要做 git 操作时会先查状态。
2. **变化提示放在用户消息前，还是作为单独一条消息？** 建议和 `/rules` 新增规则的提示用同一种方式，实现时照那里的做法。

## 决策记录

- 2026-11-24：按两条建议执行。理由：分支和 outline 只影响方向感；提示方式和已有的规则提示保持一致，不新造一种。

## 验证结果

### 实现要点（与方案不同或方案没写清的地方）

1. **字段**：`Session.system_context: Option<String>`，另加 `Session.system_told: Option<String>` 记录已告知的哈希（`prompt::announced_mark`：偏好和各说明文件按名字、内容算 sha256，取前 16 位十六进制）。两个字段都是 `serde(default)`、为空时不写出；`Session` 没有 `deny_unknown_fields`（只有 `agents/`、`config`、`hooks` 里的几个结构体有），旧版本读新会话会忽略它们。
2. **记录时机**：在 `run_with` 开始时（新任务把用户消息放进历史之前，`/continue` 时同样检查）记录，而不是第一个请求发出之后。两者之间不会有别的请求，效果相同；这样同一次运行的第一个请求就用记下的值。
3. **什么时候沿用**（`Agent::context_for`）：会话有对话（`has_conversation`，即至少一轮用户请求）、有 `system_context`、本代理配置了项目上下文时，用记下的；否则用本进程算的。会话有对话但没有这个字段（0036 之前保存的）时，第一个任务开始时记下本进程的值，从这以后沿用。
4. **三处取同一个值**：任务循环、`write_handoff`（0026）、续写压缩（0035 的 `continuation`）都改为 `system_prompt_for(session)`；状态栏的上下文估算（`Application::context_estimate`）也改用它。`system_prompt()` 只剩 `--dump-system-prompt` 使用，打印按当前目录算出的值。子 Agent 不动：它们每次都是新会话，第一个任务记下的就是自己的上下文。
5. **压缩后更新**：后台预压缩被采用（`apply_precompute`）和现场压缩（`compact_as`）成功后，`system_context` 换成本进程的值，`system_told` 清空；保存失败时和消息一起回退。任务循环里的 `system` 原来在循环外只算一次，改为在自动压缩和超长恢复之后重新取，压缩后的下一个请求就用新值。
6. **`/new`、`/handoff`**：新会话本来就没有这个字段，第一个任务开始时记下当前值，不需要额外处理。
7. **会话文件**：journal 的每条追加记录都带完整的会话头，项目上下文最大约 100 KiB（说明 32 KiB、规则 64 KiB、偏好 8 KiB、概要 4 KiB）。为不在每次保存时重复写它，追加记录不写 `system_context`，回放时保留快照里的值；`system_context` 与快照时不同时，这次保存改写快照（`journal.rs` 的 `Journal.context`）。只在记录和压缩时变化，次数很少。
8. **变化提示**：
   - 比较的是两份上下文里的"User preferences"一节和每个 `<instructions file=…>` 块（`prompt::announced_parts`，从上下文文本解析；说明区以最后一个 `# Project outline`/`# Available skills` 标题结束，这两节是 cove 自己生成的）。环境行、概要、技能列表不比较。
   - 提示和 `/rules` 新增规则一样，作为任务附注加在新任务的用户消息里（`RulesFile::task_note` 的做法），开头是 `[project context]`，用英文，与规则、偏好的附注一致（发给模型的文字保持英文）。
   - 写出变化后的内容：改了或新增的说明文件给出新内容，删除的写 `removed`；偏好变了给出新的偏好列表（最多 8 KiB，偏好文件在工作区之外，read 工具打不开，所以总是附上）。改动的说明文件合计超过 16 KiB 时只列文件名，让模型自己读。
   - 只告知一次：说明之后记下当前值的哈希；之后同样的变化不再说，再改一次才再说。`/continue` 没有新的用户消息，不附说明，留到下一个新任务。
9. **trace**：`kind=task` 记录增加 `system_reused`：这次运行用的是会话记下的、且与本进程算出的**不同**的上下文。两者相同（同一进程里的后续任务、项目没变）时为 `false`，所以这个字段统计的是"沿用确实避免了一次系统提示变化"的次数。
10. **`/init` 的提示**：原来写"/restart 后或在新会话中，系统提示会读取它"，现在恢复的会话不会重读系统提示，改为"新会话的系统提示会读取它；/restart 后继续当前会话时，下一个任务开始时告诉模型"。
11. **文档**：`docs/reference.md` 的"历史"一节增加"恢复后沿用原来的系统提示"，`--dump-system-prompt` 一条注明打印的是按当前目录生成的值。

### 测试

- 单元测试：
  - `engine::prompt::tests::only_preference_and_instruction_changes_are_told`：新增源码文件、换技能列表时上下文变了，但 `context_changes` 为 `None`、哈希不变；改 AGENTS.md、新增 CLAUDE.md 时说明里有两个文件的新内容，不含偏好和概要；删除文件和清空偏好写 `removed`；偏好变化给出新列表；说明超过 16 KiB 时只列文件名。
  - `session::journal::tests::the_project_context_is_snapshotted_once_and_kept_by_records`：上下文变化时写快照、journal 为空；之后的追加记录不含上下文，回放后仍是快照里的值，`system_told` 随记录保存；上下文再变时再写快照。
- 集成测试（`tests/all/persistence.rs`，假提供方，每个"进程"是一个新 `Agent`，会话从 `Store` 读出）：
  - `a_resumed_session_keeps_its_project_context_and_hears_of_instruction_changes_once`：第一个任务记下上下文；新增 `src/lib.rs` 后下一个进程的请求 system 与第一次逐字节相同、没有附注；改 AGENTS.md 后 system 仍相同，用户消息里有 `[project context]` 和新内容；再下一个进程不再附注。每次都断言 `system_prompt_for(session)` 等于实际发出的 system。
  - `compaction_and_older_sessions_use_the_current_project_context`：没有该字段、有对话的旧会话用本进程的值并记下，不附注；压缩后字段更新为当前值，下一个请求的 system 含当前值，且写进了会话文件。
- 集成测试（`tests/all/cli.rs`，模拟 OpenAI 兼容服务，真实的 `cove` 进程）：`a_resumed_session_sends_the_same_system_prompt_after_the_project_changed`：`-p` 跑一次，新建 `src/lib.rs`，`--resume <id> -p` 再跑一次；改 AGENTS.md 后再跑两次。四次请求的 system 消息相同（第一次含 `Entry points: src/main.rs` 且没有 `src/lib.rs`）；只有第三次的用户消息带 `[project context]` 和新内容；trace 里四条 `kind=task` 的 `system_reused` 依次是 `false, true, true, true`。
- `cargo test --release`：单元 512 通过（6 忽略），集成 241 通过。第一次全量运行时 `integrations::cancelled_and_timed_out_mcp_operations_kill_owned_descendants` 失败，单独重跑通过，第二次全量通过；0035 记录过同一个用例在负载下偶发失败，与本改动无关。
- `cargo clippy --release --all-targets`：5 条警告，都是原有的，没有新增。

### 实际运行（Bedrock Opus 5.5）

操作步骤：
1. 在临时 git 仓库里放 `Cargo.toml`、`src/main.rs`、`AGENTS.md` 和 4 个约 16 KB 的 `part*.txt`；
2. `cove -p --profile bedrock` 读完这 4 个文件；
3. 新建 `src/lib.rs`，这时项目概况里的 Languages、Entry points 两行会变；
4. `--resume <id> -p` 问 part2 的暗语。

两次用的是同一个脚本，分别在两个新仓库里跑：改动前用构建 `0.7.0+112252d6ec16`，改动后用 `0.7.0+0ccbee20d514`。数字取自 `~/.cove/usage/2026-11.jsonl` 里这个会话恢复后的第一次主请求：

| 版本 | 输入 | 读缓存 | 写缓存 | 这次请求的费用 |
|---|---|---|---|---|
| 改动前 | 40586 | 0 | 40582 | $0.1628 |
| 改动后 | 40593 | 40400（99.5%） | 189 | $0.0093 |

两次的回答都正确（`spread-2222`），都没有重新读文件。

说明：这个问题很简单，cove 把它路由给了 bedrock profile 的快速模型 `anthropic.claude-sonnet-5-5`，第一个任务也是在快速模型上跑的。两个版本走的是同一个模型，所以对比有效；Opus 5.5 上没有单独测。

## 遗留

1. 实际运行只测了快速模型（Sonnet 5.5），见上一节的说明。
2. **规则和偏好可能被告知两次**：会话进行中用 `/rules` 加的规则、新加的偏好，当时由 `RulesFile` 的附注告诉模型；之后在新进程里恢复，说明文件（`.cove/rules.md`）或偏好和记下的上下文不同，会再告知一次（内容是整份文件）。只多一次，不影响正确性，暂不处理。
3. **恢复的会话看不到新装的技能**：技能列表也在记下的上下文里，恢复后沿用旧列表，直到压缩或开新会话。`skill action=list` 仍能列出全部技能。用得多再考虑把技能变化也加入提示。
4. **同一进程里改了说明文件不提示**：和改动前一样，项目上下文在进程内只算一次，进程运行中改 AGENTS.md 要到下次恢复或新会话才生效。不在本文档范围。

