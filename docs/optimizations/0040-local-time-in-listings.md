# 0040 /undo、/memory、dream 显示的时间改成本地时间

- 状态：已完成
- 日期：2026-11-24（创建）、2026-11-24（完成）
- 关联：0014 B13（`/resume detail`、导出、回放曾显示 UTC）、AGENTS.md"给人看的时间用 `chrono::Local`"

## 问题

`crate::session::usage::timestamp`（`src/session/usage.rs:80-89`）按 UTC 格式化，注释写的就是 "in UTC"。它被下面这些给人看的地方调用：

- `/undo` 的回滚点列表和预览：`src/commands/sessions.rs:1754`、`:1791`；
- 记忆列表和详情的创建、更新时间：`sessions.rs:1959`、`:2088-2089`；
- 记忆来源：`sessions.rs:2099`；
- dream 状态的"上次运行"：`src/memory/dream.rs:657`；
- dream 交给模型的会话摘要：`dream.rs:747`。

复现：在本机（UTC+8）当前构建下，16:20 打的回滚点在 `/undo` 里显示为 08:20。0034 的集成验证里看到了这个现象。

## 改法

- 新增 `usage::local_timestamp(secs)`：用 `chrono::Local` 格式化成 `YYYY-MM-DD HH:MM`。上面列出的 7 处都改用它。
- `timestamp` 本身保留：账本按 UTC 存储和计算是有意的（AGENTS.md）。改完后搜一遍它剩下的调用方，确认都不是给人看的。没有剩下的调用方就删掉它，不留空壳。
- `dream.rs:747` 是给模型看的摘要，同样改成本地时间，和用户在界面上看到的时间一致。

## 验证

- 单元测试：`local_timestamp` 和 `chrono::Local` 直接格式化的结果一致。
- release 版实际运行 `/undo`，看列表里的时间和本机时钟一致。
- `cargo test --release`、`cargo clippy --release --all-targets`（不新增警告）。

## 验证结果

**实施与方案的差异。**

- 没有新增 `usage::local_timestamp`：`session::time::stamp` 已经是"`chrono::Local` 格式化成 `YYYY-MM-DD HH:MM`"（`/resume detail`、导出、回放在用），再加一个就是重复实现。所有改动处都改用 `crate::session::time::stamp`。
- 实际改了 10 处，比"问题"里列的 7 处多 3 处：`timestamp` 的全部调用方里，另有 `sessions.rs` 记忆出处的"核实时间"（`verified_at.map_or("未知".into(), timestamp)`，按函数值传入，搜 `timestamp(` 搜不到，编译时发现）、`src/memory/extract.rs` 自动提取状态的"上次提取"、`dream.rs` 交给模型的提示里的 `Today:`，都是给人或模型看的，一并改成本地时间。
- `usage::timestamp` 剩下唯一的调用方是 `usage::csv`：导出 CSV 的列名就是 `time_utc`，按 UTC 是有意的，保留。它改成模块私有（去掉 `pub`），注释写明只用于这一列，防止再被拿去给人显示。`session/time.rs` 的模块注释同步改成"只有用量 CSV 导出保持 UTC"。
- `docs/reference.md` 里 `/resume` "显示更新时间（UTC）"是过时描述（`/resume` 早就按 `time::relative` 显示本地时间），改成"本地时间"。

**测试。**

- 新增 `session::time::tests::stamp_is_local_time`：`0`、`1_700_000_000`、`1_795_000_000` 和当前时间，`stamp` 与 `chrono::Local` 直接格式化的结果一致。
- `tests/all/sessions.rs` 的 `git_snapshots_restore_shell_effects_and_the_restore_is_undoable`（真实 release 二进制 + 假 OpenAI 服务器）加断言：`/undo` 列表里 `#1` 的时间落在任务前后两次 `chrono::Local::now()` 的分钟之间。改动前的代码按 UTC 显示，在非 UTC 时区的机器上这条断言不成立（按代码推断，没有用旧代码实跑这条测试）；在 UTC 时区的机器上这条断言区分不出两者。
- `cargo test --release`：库 516 通过、6 忽略；集成 241 通过；0 失败。
- `cargo clippy --release --all-targets`：5 条警告，与改动前相同（`app/console.rs` 2 条、`process.rs`、`safety/permission/policy.rs`、`tests/all/cli.rs`），都不在本次改动的文件里。

**实际运行。** 临时 git 仓库（`a.txt` 已提交），用 Python 写的假 OpenAI 服务器（第一次返回 `shell` 调用 `echo changed> a.txt`，第二次返回结束），不调用真实模型：

1. `cove.exe --provider openai --base-url http://127.0.0.1:18740/v1 --permission bypass -p "改 a.txt"`：任务前打一个回滚点，`a.txt` 被改。
2. 管道 REPL（`COVE_PLAIN_REPL=1`）输入 `/undo`，输出：`#1  2026-11-24 19:06  任务：改 a.txt`。
3. 同一时刻本机时间 `2026-11-24 19:06`，UTC `2026-11-24 11:06`：列表显示的是本地时间。

`/memory` 列表和详情、记忆出处、dream 状态和摘要、自动提取状态没有单独实际运行，改动都是同一个函数的替换，由编译和上面的单元测试覆盖。
