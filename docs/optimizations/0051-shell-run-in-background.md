# 0051 shell 增加 `run_in_background`：直接在后台启动一直运行的命令

- 状态：已完成
- 日期：2026-11-25（创建）；2026-11-25（完成）
- 关联：0050（到时长转后台、后台任务登记表、`shell_task`）、0048（空闲上限）

## 背景

开发服务器（`npm run dev`、`dotnet watch`、`python -m http.server`）、文件监视这类命令不会自己结束。0048 的遗留里记着：`npm start` 属于"慢命令"，只要有 CPU 或读写就会一直跑到 `timeout_seconds`，期间整个任务卡在这次工具调用上。0050 之后它们会在 120 秒时转后台，但在那之前模型白等 120 秒；转后台后还会被当作"本轮工作的一部分"，本轮结束前一直等它（而它永远不会结束），或者 180 秒没有请求时被当成卡住杀掉。

## 现状与问题

1. **没有办法声明"这个命令要一直跑"。** shell 工具只有 `timeout_seconds`；模型想启动服务器再用 `curl` 测接口，只能先等满前台时长。
2. **0050 的规则不适合服务器。** 服务器没有请求时既不输出也不耗 CPU，180 秒空闲上限会杀掉它；3600 秒总上限也不合理；本轮结束前等待会一直等下去（只能 Esc）。

## 目标

- `run_in_background: true` 直接在后台启动命令：等最多 5 秒（出现首批输出后再等 0.5 秒就返回；命令提前结束则直接返回结果），返回任务编号和初始输出。
- 这类任务不受空闲上限和总上限限制，只在模型（`shell_task stop`）或用户停止、会话结束、cove 退出、子 Agent 结束时结束；本轮结束前不等它们；它们自己结束（比如服务器崩溃）时，结果在下一次模型调用前交给模型。
- 审批和普通 shell 一样。

## 范围

- 做：`shell` 的 `run_in_background` 参数；登记表的 `Detached` 种类；工具描述；文档。
- 不做：
  - 端口就绪检测（等到 `localhost:3000` 能连上再返回）。各服务器的就绪信号不同，模型可以自己用 `curl` 重试；固定的短暂等待和首批输出已经能看到启动错误。
  - 向服务器进程写入输入。同 0050。
  - 跨 cove 重启保留服务器进程。同 0050。

## 调研与参考

- **Claude Code**：`run_in_background: true` 立即返回任务 ID，输出写文件，用 `BashOutput` 查看、`KillShell` 停止；后台任务跑完自动通知模型。`-p` 模式退出前会等后台命令结束。
- **VS Code Copilot**：`mode: async` 立即返回终端 ID；"When an async command finishes ... you will be automatically notified on your next turn"。
- **Copilot CLI**：`mode: async` 加 `initial_wait`（默认等一会儿收集初始输出）；"By default, async processes are TERMINATED when the session shuts down"。
- **取舍**：
  - 初始等待：Copilot CLI 的 `initial_wait` 能看到启动失败（端口占用、语法错误），cove 固定最多 5 秒、有输出后再等 0.5 秒，不另开参数（0044 精简工具描述）。
  - `-p` 结束时：Claude Code 等后台命令；但服务器永远不会结束，等它等于挂住。cove 不等 `run_in_background` 的命令，进程随会话结束（和 Copilot CLI 一样）。需要命令结果的模型应该用普通 shell（会在超时后转后台并被等待）。
  - 空闲和总上限：服务器空闲是正常的，不加；泄漏风险由"会话结束就停止"兜住。

## 方案

- `ShellArgs` 新增 `run_in_background: bool`（默认 `false`）。
- `run_shell`：`run_in_background` 时 `Limit { total: Duration::MAX, idle: None }`，登记为 `Detached`，前台等待最多 5 秒；`tap` 有输出后再等 0.5 秒；命令在这期间结束时按普通结果返回（结果开头注明它已经结束）；否则 `persist`、`mute`，返回：

```text
[background #2] Started in the background. Full output: <路径>
It runs until stopped (shell_task stop id=2) or the session ends; it is not waited for at the end of your turn, and if it exits on its own the result is delivered to you. Do not poll or sleep.
exit_code=None timed_out=false truncated=false encoding_warning=false
stdout:
……
```

  同时给用户一行：`已在后台启动（#2）：npm run dev；停止前一直运行，会话结束时自动停止`。
- 方言重试（`dialect_mismatch`）只在命令前台跑完时做，和原来一样。
- 登记表：`Detached` 任务不参与"本轮结束前等待"，任务取消/出错时不停止；会话切换、退出、子 Agent 结束、登记表丢弃时停止。子 Agent 结束时停止的任务列在子 Agent 结果末尾：`[background commands stopped when this sub-agent ended: #1 npm run dev]`。
- 工具描述：`run_in_background: true starts a command that keeps running (dev server, watcher) and returns after a few seconds with its id and first output; it runs until stopped or the session ends.`
- 权限：`Effect::Shell(command)`，和普通 shell 走同一次审批；`run_in_background` 不改变风险判断。

## 影响范围

| 文件 | 改动 |
|---|---|
| `src/tools/mod.rs` | `ShellArgs.run_in_background`、前台短暂等待、描述 |
| `src/tools/jobs.rs` | `Detached` 种类的生命周期 |
| `src/agents/subagent.rs` | 子 Agent 结束时停止并说明 |
| `docs/reference.md` | Shell 参数说明 |

## 验证

- 单元测试：`run_in_background` 启动一个持续运行的命令（每秒输出一行），几秒内返回 `[background #1]` 和首批输出；它不在"需要等待"的列表里；登记表丢弃或 `close` 后进程被杀（之后要写的文件没有出现）。
- 单元测试：`run_in_background` 的命令不受空闲上限限制（测试里空闲上限缩短，`sleep` 超过它仍在运行）。
- 集成测试：模型用 `run_in_background` 启动长命令后结束回复，任务立即完成（不等待）。
- `cargo test --release`、`cargo clippy --release --all-targets`。

## 实施计划

- [x] 阶段 1：参数和前台短暂等待
- [x] 阶段 2：生命周期、子 Agent 说明
- [x] 阶段 3：测试、文档

## 待决策

无（见决策记录）。

## 决策记录

- 2026-11-25：初始等待最多 5 秒，出现首批输出后再等 0.5 秒返回，不开放参数。原因：能看到启动错误，又不让模型白等；少一个参数。
- 2026-11-25：本轮结束前和 `-p` 退出前都不等 `run_in_background` 的命令；会话结束时停止。原因：服务器不会自己结束，等它就是挂住；和 Copilot CLI 一致。
- 2026-11-25：不受空闲上限和总上限限制。原因：服务器空闲是正常的；进程泄漏由会话结束时停止兜住。
- 2026-11-25：任务取消（Esc）时不停止 `run_in_background` 的命令。原因：用户中断的是模型的这一轮，不一定是要关掉刚起的服务器；需要时 `shell_task stop` 或结束会话。

## 验证结果

### 实现

- `src/tools/mod.rs`：`ShellArgs.run_in_background`；为真时登记为 `Detached`，前台最多等 5 秒（`jobs::DETACHED_WAIT`），出现首批输出后再等 0.5 秒返回；命令在这期间结束则按普通结果返回，开头注明 `[note: the command ended within 5s, so it is not running in the background]`；否则返回 `[background #N] Started in the background. …` 和初始输出，给用户一行 `已在后台启动（#N）：<命令>；停止前一直运行，会话结束时自动停止`。审批仍是 `Effect::Shell(command)`。
- `src/tools/jobs.rs`：`Detached` 用 `Limit::from(Duration::MAX)`（无总时限、无空闲上限）；不参与 `awaited`/`waiting_status`；`stop_moved`（任务结束、取消）不停止它；`close`（会话结束、子 Agent 结束）和 `Drop` 停止它。
- 工具描述和 `docs/reference.md` 同步。

### 测试

- 单元测试 `background_starts_return_early_are_not_awaited_and_end_with_the_registry`：先输出 `ready` 再两次各停 3 秒的命令，在 5 秒内返回且初始输出里有 `ready`；不在等待列表里；空闲上限设为 1 秒时第一段 3 秒的停顿之后仍在运行（空闲上限不适用）；登记表丢弃后，命令之后要写的文件没有出现。
- 单元测试 `closing_stops_every_background_command_and_names_it`：`stop_moved` 不停止它；`close` 停止它并返回 `#1 …`；计数回调最后收到 0；关闭后不再通知。
- 集成测试 `a_background_start_is_not_awaited_and_shell_task_stops_it`：模型用 `run_in_background` 启动长命令后结束回复，任务两次模型调用后立即完成（不等待）；之后命令仍在运行，`shell_task list` 显示 `#1 [run_in_background] running for …`，`shell_task stop` 结束它，`output` 一个不存在的编号返回失败。
- 全量测试和 clippy 结果同 0050。

## 遗留

- 没有端口就绪检测（见"不做"）。
- `-p` 模式退出时直接停止 `run_in_background` 的命令，不像 Claude Code 那样等它们；需要结果的命令应该不用 `run_in_background`。
- 同 0050：不跨 cove 重启保留；Linux/macOS 未实际运行。
- Unix 上 cove 被 `kill -9` 或收到 SIGHUP、来不及执行清理时，`run_in_background` 启动的服务所在的进程组不会被结束，可能留在后台继续运行（Windows 的 Job 对象随句柄关闭结束整棵进程树，没有这个问题）。要覆盖这种情况需要在 Linux 上用 `PR_SET_PDEATHSIG` 或独立的看守进程，暂未做；`docs/reference.md` 已如实说明。
- 审查后的修正（2026-11-25）：后台 Agent 结束、`/handoff` 时同样停止 `run_in_background` 的命令；输出文件 64 MiB 上限、日志编号跨进程续接同样适用（见 0050 决策记录）。测试和 clippy 结果见 0050。
