# 0060 Alt+M 面板显示主任务简介

- 状态：已完成
- 日期：2026-11-27（创建）；2026-11-27（完成）
- 关联：0059（状态行提示 Alt+M）

## 问题

1. **只有主 agent 时面板没内容。** Alt+M 面板（`src/app/console.rs` 的 `agent_rows`）对主任务只有一行"主任务：运行中/空闲"，其余都是子代理和后台 Agent（`AgentHub::status_text`）。只有主 agent 在跑时，面板只剩：

   ```
   主任务：运行中
   No sub-agent activity.
   ```

   看不到主任务在做什么、做到哪一步；第二行还是 `status_text` 给模型和 `/status agents` 用的英文原文。0059 的提示写成"Alt+M 查看子代理"，也让人以为这个面板和主任务无关。
2. **改窗口宽度后状态行残留（用户在本任务运行时报告）。** 全屏和非全屏之间切换后，会多出一行运行中的状态行。原因：`erase` 按画的时候记下的光标行号 `drawn` 往上移再清屏；终端变窄时（Windows Terminal、新版 conhost 都会这样）会按新宽度把旧行重新折行，画满一行的状态行折成两行，光标上方的行数变多，`erase` 移得不够，状态行的前半截留在屏幕上。

## 改法

### 面板

- 标题改为"任务 | Alt+M 或 Esc 关闭"，状态行提示改为"Alt+M 查看任务"。
- 主任务部分（运行中时）：
  - `主任务：运行中 1m02s · <当前动作>`：时长和动作与状态行同源（`started`、`activity`）。
  - `请求：<本次输入>`：`app::mod` 在 `set_busy(true)` 之后调用 `set_task` 记下本次输入，任务结束时清空。
  - `计划：完成 1/3 · 当前：<进行中的一项>`：没有进行中的项就显示"下一项"，都做完了就显示"全部完成"，已取消的不计。数据有两个来源：
    - 任务开始时取会话里最近一次 `todo` 的计划（`tools::latest_todos`）；
    - 任务进行中，主 agent 的 `todo` 调用成功后（`ui::rich_events` 的 `ToolFinished`），按它的输出更新。
    
    子代理的工具事件在 `agents::subagent` 里被转成了输出行，不会走到这里，所以不会混进子代理的计划。
- 空闲时显示 `主任务：空闲`；有计划时仍显示计划这一行。
- 子代理部分前加一行"子代理："，没有时显示"子代理：无"。`status_text` 的"No sub-agent activity."提成常量 `agents::background::NO_ACTIVITY`，面板拿到它就当作没有子代理。`/status agents` 和 `agent_control` 的输出不变。面板最多 8 行，剩下的行数留给子代理。

### 改宽度后的残留

- `draw` 记下画的时候的宽度 `drawn_columns`，以及从块顶到光标每个逻辑行（以 `\r\n` 分隔）的显示宽度 `drawn_lines`，最后一项是光标在本行里的位置（`line_widths`）。
- `erase` 发现宽度变了，就按新宽度重新算光标到块顶的行数（`rows_above_cursor`）：光标之前的每一行算 `ceil(宽度/新列数)`，至少 1 行；光标所在行算 `光标位置/新列数`。宽度没变时和原来完全一样。
- 前提是终端在改宽度时会重新折行。Windows Terminal、conhost、iTerm2、macOS 终端、kitty 都会这样。xterm 这类不重新折行的终端，窗口变窄时会多往上清掉几行旧输出（只影响显示，不丢数据），代价比残留小。

### 顺带修的测试

- `safety::files_changed_by_shell_are_progress_and_unchanged_ones_are_not` 在全量测试中偶发失败。trace 显示不是进展判断出错，而是第 9 轮时撞上了 `cli()` 写死的 `--timeout 30`："任务超过时限（30.0s）"：全量测试并行跑时，9 次启动 Python 会超过 30 秒。现在这个测试改用 `cli_with(..., "120")`，其他测试不变。

### 测试

`console.rs` 加了下面几个测试：

- 只有主任务时面板的内容：时长和动作、请求、计划进度和当前项、"子代理：无"，以及最多 8 行；
- 计划摘要里的"下一项"和"全部完成"；
- 窗口变窄后，`rows_above_cursor` 能找到折行后的块顶，包括输入行在旧宽度下就已折行的情况。

## 暂缓

- 状态行的"目标"取的是 `session.current_request` 的第一行。用 skill 启动的任务，第一行是"[Skill: task-split]"，目标就显示成这个，没有信息量。这次不改，留待以后。

## 验证

- `cargo test`：lib 614 通过、7 忽略；集成测试 269 通过；0 失败。
- `cargo clippy --release --all-targets`：5 条警告，都是原有的（lib 4 条，测试 1 条），没有新增。
- rustfmt：只用 stdin 模式检查了改过的 6 个文件，都没有差异。
- release 版实际运行，只有主任务时按 Alt+M、切换全屏：未做，需要用户实际使用时确认。

## 验证结果

见"验证"。
