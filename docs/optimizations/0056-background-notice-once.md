# 0056 后台代理结束时，用户看到的通知不重复、用中文；可单独查看完整报告

- 状态：已完成
- 日期：2026-11-26（创建）；2026-11-26（完成）
- 关联：0055（同一现象中的另外两个问题）

## 背景

主任务开了一个后台代理，回合进行中代理结束。它的报告在下一轮工具调用时交给了主模型，主模型在最终回答里转述了结论。回合结束、显示"✓ 完成"之后，同一份报告又带着英文抬头 `[background agent ba24277d (plan) finished: completed; failed tools: 0; no worktree]` 原样打印了一遍，约 3000 字，屏幕上同样的内容出现了两次。

## 现状与问题

**P1** [background.rs](../../src/agents/background.rs) 的 `Notice` 分别用 `model_seen`、`user_seen` 记录模型和用户是否看过，两边拿到的是同一段文字。`NoticeHook::tool_note` 在回合中把报告交给模型；回合结束后，`commands/agents.rs` 的 `print_notices` 调 `notices_for_user`，不管模型有没有看过都打印全文。

**P2** 打给用户的抬头是英文，不符合 AGENTS.md "给人看的文字用中文"。

## 目标

- 模型已拿到报告时，用户只看到一行：`后台代理 ba24277d（plan）已完成，报告已交给主任务；查看全文：/status agents show ba24277d`。
- 模型还没拿到时（任务之间代理结束），用户看到中文的状态行和全文，下一个任务开始时模型照旧拿到英文的那份。
- `/status agents show <id>` 显示某个后台代理的状态和完整报告。

## 范围

- 做：P1、P2；新增 `/status agents show <id>`。给模型的通知文字不变。
- 不做：`/status agents` 列表（和 `agent_control list` 共用一份英文输出）的中文化，涉及给模型的文字，另开文档。

## 调研与参考

`push_notice` 只有一个调用点（代理结束时）。`notices_for_user` 有两个调用点：REPL 的 `print_notices`，以及非交互模式退出前的 `wait_all`。两处都按同一规则处理。

## 方案

- `Notice` 分三份文字：`text` 给模型（原样不变），`user` 是给用户的中文全文，`brief` 是给用户的一行摘要。`notices_for_user` 在 `model_seen` 为真时返回 `brief`，否则返回 `user`。通知的读写挪到 `BackgroundState`，`AgentHub` 只转调用，便于单测。
- 中文全文：`后台代理 {id}（{类型}）{状态}：{原因}；{n} 次工具调用失败；{工作区改动}`，后面接报告。状态取 已完成/失败/已中断；原因按退出类型翻译，正常完成和中断不写；没有独立工作区（主工作树里只读运行）时不写工作区一项。
- `change_summary` 内部改为先得到 `Changes`，英文输出逐字不变，另有中文输出。
- `/status agents show <id>`：`AgentHub::report_text`；对主任务报错说明没有后台报告。

## 影响范围

| 文件 | 改动 |
|---|---|
| `src/agents/background.rs` | `Notice`、`Changes`、通知读写、`report_text`；测试 |
| `src/commands/agents.rs` | `show` 子命令、`HELP` |
| `docs/reference.md` | 命令表、通知说明 |
| `docs/optimizations/README.md` | 索引 |

## 验证

- 新测试：先交给模型再给用户时，用户拿到的是摘要；先给用户时，用户拿到全文，模型照旧拿到全文；原因、工作区改动的中文和英文输出。
- `cargo test --release` 全量通过；`cargo clippy --release --all-targets` 不新增警告。

## 验证结果

- 新测试 `notices_show_the_user_one_line_once_the_model_has_the_report`、`finish_reasons_read_in_chinese` 通过；`tests/all/agents.rs` 两处断言改为中文通知文字后通过（全文路径：任务之间结束时打印 `后台代理 <id>（general）已完成` 和报告）。
- `cargo test --release -- --test-threads=4`：lib 610 通过（7 忽略），集成 269 通过。默认并行度下 `safety::files_changed_by_shell_are_progress_and_unchanged_ones_are_not` 偶发失败（负载高时 9 次启动 Python 超过该测试自设的 30 秒任务时限），单独跑和降低并行度都通过，与本次改动无关。
- `cargo clippy --release --all-targets`：仍是原有 5 条。

## 遗留

`/status agents` 列表仍是英文，见"范围"。
