# 0059 任务运行时提示 Alt+M 查看子代理

- 状态：已完成
- 日期：2026-11-25（创建）；2026-11-25（完成）
- 关联：0049（输入框提示）；0060 把提示改为"Alt+M 查看任务"，面板加上主任务简介

## 问题

Alt+M 可以随时打开 Agent 面板，看主任务和子代理、后台 Agent 的状态（`src/app/console.rs` 按键处理和 `agent_rows`）。但界面上只有面板打开后的标题写着"Alt+M 或 Esc 关闭"，任务运行时没有任何地方提示怎么打开它；README 的操作表也没写，只有 `docs/reference.md` 提到。用户不知道这个按键，等于没有这个功能。

## 改法

- 任务运行时的状态行（`status`）在"按两次 Esc 中断"后面加上 `· Alt+M 查看子代理`。放在 Esc 后面、引导和排队计数前面：状态行按终端宽度截断，靠前才不容易被截掉。
- 面板已经打开时不加这条提示（面板标题里已经有）。
- README 操作表加一行"看子代理在做什么"。
- 按键处理不变。
- 测试：`console.rs` 加一个测试，运行中状态行带提示，面板打开后不带。

## 验证

- `cargo test --release --lib`：611 通过，含新测试 `busy_status_mentions_the_agent_panel_until_it_is_open`。
- `cargo test --release --test all`：首次 267 通过、2 失败，原因都不在本次改动：
  - `agents::a_long_answer_is_capped_in_the_parent_and_saved_whole`：本机 `TMP=C:\WINDOWS\TEMP`，cove 输出的是 canonicalize 后的 `C:\Windows\Temp`，测试拿没 canonicalize 的路径比较（AGENTS.md 一、第二条）。已改测试，先 canonicalize 再比。
  - `safety::files_changed_by_shell_are_progress_and_unchanged_ones_are_not`：全量并行跑时偶发失败，单独跑 3 次都通过。
- 修完后 `cargo test`（debug）：lib 611 通过，集成测试 269 通过，0 失败。
- `cargo clippy --release --all-targets`：5 条警告，都是原有的，没有新增。
- `rustfmt --check src/app/console.rs`：通过。
- release 版实际运行一次带子代理的任务，确认状态行显示提示、Alt+M 能打开面板：未做，待下次使用时确认。

## 验证结果

见"验证"。
