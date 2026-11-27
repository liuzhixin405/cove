# 0064 任务面板只列正在运行的子代理，已结束的合并成一行，状态和步骤改中文

- 状态：已完成
- 日期：2026-11-27
- 关联：0060（Alt+M 任务面板）；0063（本文是审查那次会话时发现的）

## 背景

在一次会话里，用户先让一个子代理排查 Grafana 问题，它因为拿不到 MCP 当场结束。之后用户让主任务去做一件无关的事（跑 `cargo test`），打开 Alt+M 面板，看到：

```text
主任务：运行中 19m21s · 运行 cargo test
  请求：开始
子代理：
agent-1 agent:explore finished model=anthropic.claude-sonnet-5-5 7s task="排查 Grafana 缺 staging 资源指标" last="model responding"
```

用户的判断是：这个子代理和当前任务无关，不该出现在这里。

## 现状与问题

1. **已结束的子代理一直留在面板里，和当前任务混在一起。**
   - `ActivityBoard` 只在超过 64 条时才清掉已结束的记录（`src/agents/subagent.rs:378-387`），没有"属于哪一轮任务"的信息。
   - 面板的内容来自 `status_text(None)`（`src/app/mod.rs:497`）。`status_text` 把后台记录和活动板里所有条目都列出来，包括已结束的（`src/agents/background.rs:1315-1336`）。
   - 面板标题是"任务"，主任务一行写的是当前任务，下面再接一个早已结束的子代理，读起来就是它属于当前任务。
2. **已结束的条目还显示结束前的最后一步。** `finish` 只改状态和结束时间（`subagent.rs:398-405`），`last_step` 保持 `model responding`，这是子代理被中断时的瞬间，不是它的结论。
3. **整行是英文内部格式，不是给人看的。** 状态 `finished`、类型 `agent:explore`、`model=…`、`task=…`、`last=…`，步骤 `starting`、`model responding`、`running <工具>`、`<工具> ok: …`、`overlay layer: N files changed`、`N pending tool calls`，都直接显示在面板里。AGENTS.md 要求给人看的文字用中文。
4. `/status agents` 和模型调用的 `agent_control list` 也走 `status_text`，那里要看到完整的历史记录，不能因为面板的需要把已结束的条目删掉。

## 目标

- 面板的"子代理"部分只列还在进行的（运行中、暂停、被中断、待恢复），一行一个，中文。
- 已结束的（完成、失败）合并成一行："已结束 N 个（/status agents 查看）"。已丢弃的不计。
- 步骤文字改成中文；状态用中文名。
- `/status agents` 和 `agent_control list` 的内容、格式不变（模型靠它读状态，不做不必要的变动），只有其中的步骤文字跟着改成中文。

## 范围

- 做：面板的内容生成；步骤文字改中文；状态增加中文名。
- 不做：
  - 按"任务轮次"给子代理打标记再过滤。用"是否还在进行"判断已经能解决这个现象，改动小；真有需要再做。
  - 结束时改写 `last_step`。已结束的条目面板里不再逐行显示，`/status agents` 里保留结束前最后一步，对排查有用。
  - `/status agents` 的整体格式中文化：它是命令行输出，同时被模型读取，另开文档。

## 方案

1. `src/agents/lifecycle.rs`：给 `Lifecycle` 加 `zh()`，返回"运行中/已暂停/已中断/已完成/失败/已丢弃"。
2. `src/agents/background.rs`：新增 `panel_lines(&self) -> Vec<String>`。
   - 后台记录：跳过已丢弃；状态为运行中、暂停、被中断的逐行列出；已完成、失败的计入"已结束"。
   - 活动板里不属于后台记录的条目（前台子代理）：`ended` 为空的逐行列出，其余计入"已结束"。
   - 行格式：`<短 id> <类型> <中文状态> <已用秒数>秒 · <任务描述，截 40 字符宽> · <最后一步，截 40 字符宽>`；有待确认的审批时加一行"待确认：…"；用 `text::clip_width` 之类按宽度截断。
   - 没有任何内容时返回空列表（面板显示"子代理：无"）。
   - `NO_ACTIVITY` 的特判（`app/mod.rs:498-503`）不再需要，删掉；`status_text` 里的 `NO_ACTIVITY` 保留，`/status agents` 仍用它。
3. `src/app/mod.rs`：面板刷新改调 `panel_lines()`。
4. 步骤文字改中文：`subagent.rs:376`（启动中）、`1348`（运行 `<工具>`）、`1359`（`<工具> 成功/失败：…`）、`1371`（模型回答中）；`plan.rs:583`（覆盖层：改了 N 个文件）；`background.rs:519`（启动中）、`1174`（N 个工具调用待确认）。

## 影响范围

- `src/agents/lifecycle.rs`、`src/agents/background.rs`、`src/agents/subagent.rs`、`src/agents/plan.rs`、`src/app/mod.rs`。
- 步骤文字也会出现在 `/status agents` 和模型读到的 `agent_control list` 里，变成中文；模型能读懂，不影响判断。
- 搜 `src/`、`tests/`、`docs/` 里对原步骤文字的断言和描述，一并更新（`grep` 已核对：`src/` 里没有测试断言这些文字，`docs/` 待实施时再搜一遍）。
- 文档：`docs/reference.md` 里描述 Alt+M 面板的段落同步。

## 验证

- 单元测试（`background.rs`）：
  - 一个运行中、一个已完成的前台子代理 → `panel_lines()` 只有运行中那一行，加一行"已结束 1 个"。
  - 全部已结束 → 只有"已结束 N 个"一行。
  - 没有任何条目 → 空列表。
  - 行里不含 `agent:`、`model=`、`finished`、`last=` 这类内部格式。
- 现有 `the_task_panel_sums_up_the_main_task_without_subagents`（`console.rs:3287`）继续通过。
- 按 AGENTS.md：只格式化改过的文件，`cargo clippy --release --all-targets`（不新增警告），最后 `cargo test`。
- 人工：release 版里让一个子代理结束，再开始另一个任务，打开 Alt+M，确认面板里不再列出已结束的子代理。

## 实施计划

- [x] 阶段 1：`Lifecycle::zh()` 和 `panel_lines()` 及测试
- [x] 阶段 2：步骤文字改中文，面板改调 `panel_lines()`
- [x] 阶段 3：同步 `docs/reference.md`，回填验证结果

## 待决策

无。

## 决策记录

- 2026-11-27：**用"是否还在进行"过滤，不给子代理打任务轮次标记。** 理由：改动小，能解决已结束子代理混进当前任务的问题；以后需要"只看本轮子代理"再单独做。
- 2026-11-27：**`/status agents` 不改格式。** 理由：它同时给模型读，格式变动要连同模型的用法一起评估。

## 验证结果

实际改动：

- `src/agents/lifecycle.rs`：`Lifecycle::zh()`。
- `src/agents/background.rs`：`panel_lines()`（取数据）和纯函数 `panel_rows()`（可单测）；`NO_ACTIVITY` 的注释改了，常量保留给 `/status agents`。和方案不同的一点：没有给面板加"类型 + 英文 kind"，行里类型只写 agent 类型（如 `explore`），去掉了 `agent:`、`bg:` 前缀。
- `src/app/mod.rs`：面板刷新改调 `panel_lines()`，删掉 `NO_ACTIVITY` 特判。
- 步骤文字中文化：`subagent.rs`（启动中、运行 <工具>、<工具> 成功/失败：…、模型回答中）、`plan.rs`（覆盖层：改了 N 个文件）、`background.rs`（启动中、N 个工具调用待确认）。
- `docs/reference.md`：任务面板一条。

检查：

- 新增测试 `agents::background::tests::the_panel_lists_running_agents_and_folds_the_finished_ones`：运行中的一行、待确认一行、已结束的合并计数、已丢弃的不计、全部结束只剩计数、空列表。
- rustfmt（stdin 模式，5 个改过的文件）：只有 `plan.rs` 一处换行，已手动改。
- `cargo clippy --release --all-targets`：5 条，都是原有的，没有新增。
- `cargo test`：lib 623、集成 272，全部通过。
- 搜 `src/`、`tests/`、`docs/` 里旧步骤文字：只剩本文档里的问题描述。
- 人工验证（release 版里让子代理结束后再开始别的任务，打开 Alt+M）：未做。

## 遗留

- `/status agents` 的整体格式仍是英文内部格式（`状态 model=… task=… last=…`）。
