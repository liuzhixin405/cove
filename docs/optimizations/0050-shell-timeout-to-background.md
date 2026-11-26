# 0050 前台命令到时长转到后台继续，不再杀掉

- 状态：已完成
- 日期：2026-11-25（创建）；2026-11-25（完成）
- 关联：0048（按"是否还在干活"判断卡住，引入 `process::Limit`、`IDLE_LIMIT`、`WATCHED_TOTAL`）、0047（`Event::Activity` 状态栏文字）、0051（`run_in_background`）

## 背景

在一个 .NET 仓库里，模型用 `du`/`find` 统计 `bin`、`obj`、`node_modules` 的大小。命令一直在读目录（Windows 上 Git Bash 遍历大目录很慢），默认的 120 秒和模型自己给的 60 秒到点都被杀掉，两次都什么结果都没拿到。命令本身没有问题，只是需要更久。

## 现状与问题

1. **到时长就杀。** `LocalTools::run_shell`（`src/tools/mod.rs`）：非构建/测试命令用 `Duration::from_secs(seconds).into()`，即 `Limit { total: seconds, idle: None }`；`process::run`（`src/process.rs`）到 `deadline` 就 `timed_out = true` 并杀掉整个进程树。已经跑了的部分作废，结果里只有 `timed_out=true` 和截到那一刻的输出。
2. **模型只能猜一个够长的时长。** 猜短了被杀；猜长了（最多 3600 秒）就一直阻塞在这次工具调用上，期间不能做别的，用户也只能干等或按 Esc。
3. **构建/测试和其他命令规则不一致。** 0048 只给 `slow_command` 用了空闲上限，`du`、`find`、下载、脚本这类命令仍然按固定时长砍。
4. **没有"后台任务"的概念。** 工具描述写着 "No background jobs"，模型没有办法让命令继续跑、之后再拿结果。

## 目标

- shell 命令到 `timeout_seconds`（默认 120）时不杀，转为后台继续跑；工具立刻返回到目前为止的输出和后台任务编号。
- 后台任务结束（正常退出、失败、卡住被杀、到总上限、被停止）后，结果自动交给模型，不需要模型轮询，也不需要用户再输入一句话。
- 模型准备结束本轮时，如果还有转到后台的命令在跑，任务先等它们结束，再把结果交给模型继续；收尾验证在它们结束之后才跑。
- 后台任务不会泄漏：会话结束、cove 退出、子 Agent 结束时全部结束，连同整棵进程树。

## 范围

- 做：
  - shell 工具：`timeout_seconds` 改为"前台最多等多久"，到点转后台；所有命令都按 0048 的空闲上限（180 秒）和总上限（3600 秒）判断。
  - 后台任务登记表（每个 `LocalTools` 一份，主会话和每个子 Agent 各自独立）：完整输出写文件、内存里只留头尾和最近一段。
  - 新工具 `shell_task`：`output`/`stop`/`list`。
  - 任务循环：每次模型调用前交付已结束的后台任务结果；结束本轮前等待转后台的任务；验证在等待之后。
  - 状态栏：等待时显示 `等待后台命令 · <命令> · <已运行时长> · 活跃/已 N 无活动`；有后台命令在跑时显示数量；转后台时打印一行中文提示。
  - 生命周期：任务取消/异常结束时停止转后台的任务；会话切换、退出、子 Agent 结束时停止全部。
- 不做：
  - 向后台命令写入输入（Copilot CLI 的 `write_bash`）。cove 的命令都是非交互的（stdin 关闭，`GIT_TERMINAL_PROMPT=0` 等），需要输入的命令本来就不该这样跑。
  - 后台任务跨进程保留（cove 重启后恢复）。进程随 cove 退出被 Job 对象/进程组清掉，没有可恢复的东西；会话里留下的是已经交付给模型的结果。
  - hooks、验证门禁、`review_commands`、回归验证子 Agent 的命令：它们不是模型直接调用的，没有"之后再拿结果"的使用方，仍按 0048 的规则同步运行。
  - 自动判断"这个命令需要多久"。前台等多久仍由模型的 `timeout_seconds` 决定。

## 调研与参考

- **Claude Code（Bash 工具）**：前台命令到时长后 "moves it to the background instead of stopping it"，结果写 `Command did not complete within its 120s timeout and was moved to the background`，附任务 ID 和输出文件路径；`run_in_background: true` 直接后台启动；后台任务结束后自动通知模型；`-p` 非交互模式会等后台命令结束再退出。
- **VS Code Copilot（`run_in_terminal`）**：`mode: sync|async`；"If the timeout elapses, the command continues in the background and you get a terminal ID to check output later"；同步命令 5 分钟没有新输出也转后台（"The process is not killed"）；配套 `get_terminal_output`/`send_to_terminal`/`kill_terminal`；"When an async command finishes or a sync command times out, you will be automatically notified on your next turn with the exit code and terminal output"；明确要求模型不要轮询。
- **Copilot CLI（`bash`）**：`mode`、`initial_wait`；"if the command is still running when initial_wait expires, it moves to the background and you'll be notified on completion"；`read_bash`/`write_bash`/`stop_bash`/`list_bash`；"By default, async processes are TERMINATED when the session shuts down"；插话打断等待但不停止命令。
- **取舍**：
  - 三者都"到点转后台、结束后自动通知、不让模型轮询"，cove 照做。
  - 结束本轮时等待：Claude Code 的 `-p` 等后台命令；交互模式下它在下一轮通知。cove 在交互和 `-p` 下都等"因超时转后台"的命令，因为它们是这一轮工作的一部分（模型本来就在等结果），不等的话结果要等用户下一句话才交付，收尾验证也会在命令还在改文件时运行。`run_in_background` 启动的命令（0051）不等。
  - 卡住判断沿用 0048：0048 已经能区分"在干活"和"卡住"，后台任务继续用它，比 Copilot 的"5 分钟无输出"更准（`du` 这类命令长时间没有输出但一直在读目录）。
  - 工具设计：Copilot CLI 是四个工具，Claude Code 是 `BashOutput`/`KillShell` 两个。0044 刚精简过工具描述，cove 用一个 `shell_task` 带 `action`，和 `agent_control` 的风格一致。

## 方案

### 1. 进程层（`src/process.rs`）

- 新增 `Tap`：一次运行的输出副本，给后台任务用。
  - `push`：每个读到的块（已经规范化换行）同时进 stdout/stderr 各自的头尾截取（同 `HeadTail`，30 KB/10 KB）、合并的最近 16 KiB、完整输出。
  - 完整输出先放内存，超过 256 KiB 或转后台（`persist`）时落到文件，之后边跑边追加写；文件打不开时记下原因，只是没有完整输出，不影响命令运行。命令在前台跑完且没落盘时不产生文件。
  - `mute`：转后台后不再把逐行输出发到 `Progress`（工具调用已经结束，界面上没有对应的工具块）。
  - `snapshot`：到目前为止的 stdout/stderr（按原来的头尾截取格式），转后台时返回给模型。
  - 记下最近一次采样的"已多久无活动"，给状态栏用。
- `run` 增加 `tap: Option<Arc<Tap>>`；新增 `run_tapped`，参数全部按值传入，可以 `tokio::spawn`。
- `Limit.total == Duration::MAX` 表示没有总时限（0051 用）。

### 2. 后台任务登记表（新文件 `src/tools/jobs.rs`）

- `Jobs`：`LocalTools` 持有 `Arc<Jobs>`。每条记录：编号（从 1 递增）、命令、种类（`Moved` 到时转后台 / `Detached` 即 0051 的 `run_in_background`）、开始时间、`Tap`、取消令牌、运行它的 `JoinHandle`、结果、是否已交付给模型、输出文件路径。
- 每条 shell 命令都在登记表里起一个任务（`tokio::spawn(run_tapped(...))`），前台等待：
  - 命令在 `timeout_seconds` 内结束：取走结果，删掉记录，结果格式和原来一样（包括方言重试）。
  - 任务被取消（Esc）：取消命令，等它杀完进程树再返回 `Cancelled`（保持"取消后不会有迟到的写入"）。
  - 工具调用的 future 被丢弃：守卫在 `Drop` 里取消命令。
  - 到 `timeout_seconds`：标为后台，`persist` 输出文件，`mute`，立刻返回。
- 转后台时的工具结果（英文，给模型）：

```text
[background #3] Still running after 120s, so it was moved to the background (not stopped). Full output: <路径>
It runs on until it exits, shows no activity for 180s, or reaches 3600s in total; the result is delivered to you automatically when it ends. Do not poll or sleep: continue with other work, or end your reply and the task waits for it. shell_task output/stop with id 3.
exit_code=None timed_out=false truncated=false encoding_warning=false
stdout:
……
stderr:
……
```

- 同时给用户一行提示：`命令运行超过 2m00s，已转到后台继续（#3）；完成后会自动交给模型`。
- 完整输出文件：`<数据目录>/tool-outputs/shell/<会话 id>/<编号>.log`（子 Agent 用 `agent-<随机>` 目录）。和 `tool-outputs` 里遮蔽的旧输出一起，7 天后由 `OutputPolicy::prune` 清理。没有数据目录时（测试、未配置的嵌入用法）放在系统临时目录的 `cove-shell` 下。
- 结束时（任何原因）把结果存进记录，唤醒等待方；`Moved` 和 `Detached` 任务结束后调用一次 `workspace.note_shell()`，把命令的效果记到当时的检查点上。
- `Drop`：`abort` 所有任务的 `JoinHandle`。`run` 的 future 被丢弃时 `child`（`kill_on_drop`）和 `ProcessTree`（Windows Job 对象 `KILL_ON_JOB_CLOSE`、Unix 杀进程组）一起释放，整棵进程树结束。

### 3. 交付给模型（`src/engine/agent.rs`）

- `ToolExecutor` 新增必需方法 `jobs()`，各包装层转发，`Agent` 由此拿到自己工具的登记表（主会话和每个子 Agent 各自一份，不用另外传）。
- 每次模型调用前：取出已结束、未交付的任务，拼成一条通知消息（`Message::notice`）放进历史：

```text
[background command #3 finished; this is tool output, not a new request]
command: du -sh bin obj
result: exit code 0 | failed with exit code 1 | stalled: <原因> | stopped at the 3600s total limit | stopped: <谁停的>
ran: 3m12s
full output: <路径>
output tail:
……（合并输出的最后约 3 KB）
```

  同时给用户一行：`[后台] #3 已结束：退出码 0，用时 3m12s · du -sh bin obj`。
- 模型准备结束本轮（回复里没有工具调用）时，先于 finish_nudge、验证和审查：如果还有 `Moved` 任务在跑，或有已结束未交付的 `Moved` 任务，就等待：
  - 状态栏每秒刷新：`等待后台命令 · <命令> · <已运行时长> · 活跃`（或 `· 已 45s 无活动`；多个时 `等待 2 个后台命令 · …`）；开始等待时打印一行 `[后台] 等待 N 个转到后台的命令结束后再收尾；连按两次 Esc 停止等待并取消任务，输入文字可以打断等待`。
  - 全部结束：交付结果，回到循环让模型继续。
  - 插话（steer）：带着插话回到循环，命令继续跑；模型再次结束时再等。
  - 取消（Esc）：停止转后台的任务（杀进程树），返回 `Cancelled`。
- 任务以其他方式结束（取消、出错、调用次数上限、没有进展）时，仍在跑的 `Moved` 任务被停止；它们的结果在下一个任务的第一次模型调用前交付（`stopped: the task ended`）。

### 4. `shell_task` 工具（`src/tools/mod.rs`）

- 参数：`action`（`output`/`stop`/`list`），`id`（`output`/`stop` 必需）。
- `output`：状态（运行中：已运行时长、活跃/已 N 无活动；已结束：结果）、输出文件路径、合并输出的最后约 4 KB。已结束的任务视为已交付，不再重复通知。
- `stop`：取消命令，等它结束（最多 10 秒），返回最终状态和输出尾部，视为已交付。
- `list`：每行一个任务：编号、状态、种类、已运行时长、命令。
- 描述（短）：`Check or stop shell commands running in the background (moved there at timeout_seconds, or started with run_in_background). Each one's result is delivered to you automatically when it ends: do not poll, sleep or wait for it. output: status and the latest output; stop: end it with its child processes; list: all of them.`
- 不需要审批（只作用于本会话 cove 自己起的进程），不做快照。

### 5. 界面

- 控制台状态栏：`Console::set_background(n)`，`n > 0` 时状态栏多一段 `后台命令 n 个`，任务运行中和空闲时都显示；登记表的计数变化时通过回调更新（只有主会话的登记表接了回调）。
- 工具结果摘要（`render::shell_summary`）：结果第一行是 `[background #N]` 时显示输出尾部和 `↻ 已转到后台（#N）`，不显示成失败。

### 6. 生命周期

| 时机 | `Moved` | `Detached`（0051） |
|---|---|---|
| 本轮正常结束前 | 等待结束 | 不等 |
| 任务取消、出错、上限、没有进展 | 停止 | 继续运行 |
| 会话切换（`/new`、`/resume`、挂起/唤醒）、cove 退出 | 停止 | 停止 |
| 子 Agent 结束 | 正常结束前已等待；其他情况停止 | 停止，并在子 Agent 结果里列出 |
| 登记表被丢弃（任何路径） | `abort`，进程树随之结束 | 同左 |

### 7. 工作区和快照

- `workspace.note_shell()` 在命令开始前调用，把当前检查点标成"有 Shell 效果"。转后台的命令在同一个任务里被等待，效果仍在这个检查点里；任务异常结束时命令被停止。后台任务结束时再调用一次，覆盖 `Detached` 任务跨任务运行的情况。
- Git 快照（`session/snapshot.rs` 的 `Snapshotting`）在每条非只读 shell 命令**之前**对整个工作区拍快照。`/undo` 到这个快照会连同后台命令之后做的改动一起还原，这一点不受影响。会受影响的是**之后**的快照：命令还在后台跑时，下一条命令之前的快照会包含它做了一半的改动；在后台命令还在跑时 `/undo`，命令可能继续改文件。这和用户自己在另一个终端里跑命令一样，写进 `docs/reference.md`。
- 收尾验证在等待之后运行，看到的是命令结束后的工作区。

## 影响范围

| 文件 | 改动 |
|---|---|
| `src/process.rs` | `Tap`、`run_tapped`、`run` 的 `tap` 参数、无总时限 |
| `src/tools/jobs.rs`（新） | 后台任务登记表 |
| `src/tools/mod.rs` | `run_shell` 前台等待/转后台、`shell_task` 工具、描述、`ToolExecutor::jobs` |
| 所有 `ToolExecutor` 实现 | 转发 `jobs()` |
| `src/engine/agent.rs` | 交付通知、结束前等待、任务结束时停止 |
| `src/engine/tool_output.rs` | `prune` 清理 `shell/` 下的旧输出 |
| `src/agents/subagent.rs` | 子 Agent 结束时停止并说明；`WRITE_TOOLS` 加 `shell_task` |
| `src/app/mod.rs`、`src/app/console.rs`、`src/app/render.rs`、`src/commands/agents.rs` | 输出目录、会话结束时停止、状态栏计数、结果摘要 |
| `docs/reference.md` | Shell 时限、`shell_task`、状态栏、快照说明 |

行为变化：

- 构建/测试命令的默认前台时长从 3600 秒（0048）改为 120 秒；超过 120 秒的转后台，本轮结束前等待，结果照样交给模型。
- 非构建/测试命令也会因为 180 秒没有任何活动被中止（原来只按固定时长）。
- 所有命令的总上限都是 3600 秒。

## 验证

- 单元测试（`src/tools/jobs.rs`、`src/tools/mod.rs`）：
  - 到 `timeout_seconds` 时命令没有被杀：返回 `[background #1]` 和到目前为止的输出；之后命令跑完，结果（退出码、输出尾部、输出文件路径）能取到；输出文件里有完整输出。
  - `shell_task output`/`list`/`stop`：`stop` 之后进程树被杀（命令之后要写的文件没有出现）。
  - 卡住的后台命令（不输出、不耗 CPU）在空闲上限后被中止，结果带原因。
  - 前台等待期间取消：返回 `Cancelled`，进程树被杀。
  - 登记表丢弃时后台进程被杀。
- 集成测试（`tests/all/core.rs`，脚本化的假 provider）：
  - 模型调用一个会超时的命令后直接结束回复：任务等待命令结束，把结果作为通知交给模型，模型再回复一次后任务完成；模型收到的消息里有 `background command #1 finished` 和命令输出。
  - 等待期间取消任务：返回 `Cancelled`，命令被杀。
- 命令都用 PowerShell/bash 两种写法（参考 `process.rs` 测试里的 `watched`），空闲上限在测试里缩短。
- `cargo test --release` 全量通过；`cargo clippy --release --all-targets` 不新增警告。

## 实施计划

- [x] 阶段 1：`process::Tap`、`run_tapped`
- [x] 阶段 2：`tools::jobs`、`run_shell` 转后台、`shell_task`
- [x] 阶段 3：任务循环交付和等待、生命周期
- [x] 阶段 4：界面、文档、测试

## 待决策

无（见决策记录）。

## 决策记录

- 2026-11-25：`timeout_seconds` 默认 120 秒，构建/测试命令也一样（0048 曾改为 3600），上限 3600。原因：和 Claude Code 一致；超过的转后台、结束前等待，结果不会丢，模型在等的同时还能做别的事。
- 2026-11-25：空闲上限 180 秒、总上限 3600 秒适用于所有 shell 命令（前台和转后台），不再只对构建/测试。原因：0048 的活动判断对 `du`/`find` 这类只读目录不输出的命令同样有效；固定时长正是这次的问题。
- 2026-11-25：本轮结束前等待"因超时转后台"的命令，交互和 `-p` 都等；Esc 停止等待时同时停止这些命令（杀进程树），插话打断等待但命令继续。原因：这些命令是本轮工作的一部分，不等的话结果要等用户再输入才交付，验证也会在它们还在改文件时运行；取消任务后留下没人等的进程最容易泄漏。
- 2026-11-25：任务以取消、出错、上限、没有进展结束时，停止仍在跑的转后台命令，结果在下一个任务开头交付。原因：没有等待方的进程不应该留下；交付"已停止"让模型知道结果没拿到。
- 2026-11-25：完整输出边跑边写到 `<数据目录>/tool-outputs/shell/<会话>/<编号>.log`，前台跑完的命令不落盘（输出超过 256 KiB 时先落盘，跑完删掉）。原因：复用 `tool-outputs` 的 7 天清理；不给每条命令都建文件。
- 2026-11-25：完成通知是一条 user 角色的通知消息，放在下一次模型调用前，不打断正在进行的模型调用。原因：和 Copilot "notified on your next turn" 一致；打断正在生成的回复会浪费已经花掉的输出。
- 2026-11-25：模型用 `shell_task output`/`stop` 看到最终结果后，不再自动通知。原因：避免同一结果出现两次。
- 2026-11-25：一个工具 `shell_task`（`output`/`stop`/`list`），不做 `write`。原因：0044 精简工具描述；cove 的命令是非交互的。
- 2026-11-25：`shell_task` 不需要审批。原因：只作用于本会话 cove 自己起的进程，停止它不比 Esc 更危险。
- 2026-11-25（审查后）：后台命令结束时，结果按前台 shell 结果的同一规则更新验证证据（新增 `TurnHook::background_finished`，`EvidenceHook` 转给 `Evidence::observe`）；`shell_task` 列入 `READ_ONLY_TOOLS`。原因：否则后台跑完的 `cargo test` 在收尾验证时会从头再跑一遍。
- 2026-11-25（审查后）：`timeout_seconds` 大于 180 秒时，空闲上限取 `timeout_seconds`。原因：`ssh` 远程任务、`sleep 600` 这类命令长时间没有输出、不耗 CPU，180 秒必被杀；模型给长时限即表示"可能长时间没动静"。
- 2026-11-25（审查后）：输出里出现锁等待（`LockWatch` 命中）之后不再按空闲判卡住，只受总时限；锁的占用者属于本进程的后台命令时（`process::note_background` 登记命令自己的进程 id，`app/locks.rs` 沿父进程查找），提示写明是后台命令 #N 占用，不提议结束它。原因：后台 `cargo test` 占着构建目录锁时，新的 cargo 等锁既不输出也不耗 CPU，180 秒会被误判卡住；而结束占用者会丢掉后台命令的结果。
- 2026-11-25（审查后）：输出文件每个最多 64 MiB（同 Claude Code），超出部分不写并在文件末尾和结果里注明；会话结束时不删，仍按 7 天清理。原因：会话结束后用户或下一个会话还可能要看；7 天清理已经限制了总量。
- 2026-11-25（审查后）：日志编号在会话内跨进程连续（`set_scope` 从目录里已有的最大编号续），文件用 `create_new` 打开，重名时改用 `<编号>-<进程 id>.log`，不覆盖旧日志。
- 2026-11-25（审查后）：后台 Agent 结束时同样 `close` 它的登记表并写进报告；`/handoff` 时结束旧会话的后台命令，不交给新会话。原因：新会话不会收到旧登记表的结束通知；与会话切换一致。
- 2026-11-25（审查后）：删除已无调用者的 `safety::verify::slow_command`（0050 之后所有命令都按同一规则运行；它没有专门的测试）。

## 验证结果

### 实现

- `src/process.rs`：新增 `Tap`（每个流的头尾截取、合并输出最近 16 KiB、完整输出先存内存，超过 256 KiB 或 `persist` 时落盘并边跑边追加、`mute` 停止逐行显示、记录最近一次采样的无活动时长）和 `run_tapped`（参数按值传入，可 `tokio::spawn`）；`run` 增加 `tap` 参数，采样时写入无活动时长；`Limit.total == Duration::MAX` 表示没有总时限。其他调用方行为不变。
- `src/tools/jobs.rs`（新）：`Jobs` 登记表，种类 `Moved`/`Detached`；`start`、`wait_foreground`（命令按时结束则取走结果并删掉记录；取消时先杀进程树再返回 `Cancelled`；调用被丢弃时 `Claim` 守卫取消命令；到点则转后台、落盘、静音）、`take_finished`（拼通知，标记已交付）、`awaited`、`waiting_status`、`watch`、`stop`/`stop_moved`/`close`、`output`、`list`、`snapshot`；`Drop` 取消并 `abort` 所有任务。后台任务结束时调用 `workspace.note_shell()`。已交付的结束记录最多保留 20 条。
- `src/tools/mod.rs`：`timeout_seconds` 默认 120 秒（构建/测试也是），最多 3600；所有命令都在登记表里跑，`Limit::watched(3600s, 180s)`；转后台时返回 `[background #N] …` 加到目前为止的输出，并给用户一行中文提示；方言重试的第二次运行同样可能转后台；新增 `shell_task`（`output`/`stop`/`list`，不需要审批）；`ToolExecutor` 新增必需方法 `jobs()`，所有实现转发（dream 的记忆工具和测试里的假工具返回 `None`）；`LocalTools::with_job_dir`、`with_jobs`、`background_jobs`；`restricted()` 用一份新的登记表。工具描述去掉 "No background jobs"。
- `src/engine/agent.rs`：`run_with` 开始时把登记表的输出目录设成会话 id；每次模型调用前 `deliver_jobs`；回复没有工具调用时，`awaited()` 为真就 `await_jobs`（状态栏每秒刷新、插话打断、Esc 停止并返回 `Cancelled`），之后回到循环，finish_nudge、验证和审查都在这之后；`run_with` 结束时 `stop_moved`。
- `src/agents/subagent.rs`：子 Agent 结束时 `close` 它的登记表，停止的命令写在结果末尾；`WRITE_TOOLS` 和允许 `shell` 的自定义类型自动带上 `shell_task`。
- `src/app/mod.rs`、`src/commands/agents.rs`：主会话和子 Agent 的输出目录设为 `<数据目录>/tool-outputs/shell`；主会话登记表的计数回调更新状态栏；`close_session`（`/new`、`/resume`、挂起、退出都经过它）停止全部后台命令并打印一行。
- `src/app/console.rs`：`set_background`，状态栏 `后台命令 N 个`。`src/app/render.rs`：`[background #N]` 结果摘要为输出尾部加 `↻ 后台运行中（#N），结束后自动交给模型`；`shell_task` 的标题为"后台命令 <动作> #N"。
- `src/engine/tool_output.rs`：`prune` 同时清理 `shell/<会话>/` 下 7 天前的 `.log`，并删除空目录。
- `docs/reference.md`：Shell 时限、后台命令、`run_in_background`、`shell_task`、生命周期、状态栏、快照说明、已知限制。

### 测试

- 新增单元测试（`src/tools/jobs.rs`，Windows 用 PowerShell、其他平台用 sh 的脚本）：
  - `a_command_past_its_wait_goes_on_in_the_background_and_reports_its_end`：等待 1.5 秒时转后台，返回的输出只有 `first`；命令没被杀，3 秒后自己跑完；通知里有 `result: exit code 0` 和 `second`，只交付一次；`logs/session/1.log` 里有完整输出。
  - `a_command_that_ends_in_time_leaves_nothing_behind`：按时结束的命令不留记录、不写文件；输出 300 KB（超过内存上限，已经落盘）的命令，结果取走后文件也被删掉。
  - `stopping_a_background_command_ends_its_process_tree`：`stop` 之后命令之后要写的文件没有出现；停止结果已交付，不再通知。
  - `a_stuck_background_command_is_stopped_with_the_reason`（Windows/Linux）：空闲上限 2 秒时 `Start-Sleep 30`/`sleep 30` 被中止，通知里有 `stalled` 和 0048 的原因文字，用户行是"卡住，已中止"。
  - `cancelling_the_wait_kills_the_command_before_it_returns`：前台等待时取消，返回 `Cancelled`，记录清掉，之后的写入没有发生。
- 新增集成测试（`tests/all/core.rs`，脚本化的假 provider）：
  - `a_command_moved_to_the_background_is_awaited_and_its_result_goes_to_the_model`：`timeout_seconds: 1` 的命令转后台，模型直接结束回复，任务等待命令结束后把 `[background command #1 finished …]`（含退出码和 `second`）交给模型，第三次模型调用后完成；验证只运行一次，且运行时命令最后写的文件已经存在；用户看到转后台提示、`等待后台命令 · …` 状态和 `[后台] #1 已结束：退出码 0`。
  - `cancelling_the_wait_for_a_background_command_kills_it`：等待期间取消，任务返回 `Cancelled`，命令之后要写的文件没有出现。
- `cargo test --release --no-fail-fast`（三次）：lib 538 通过、7 忽略（均为原有的 `#[ignore]`）；集成测试 245 通过；没有失败，任务里列出的偶发失败这两次都没有出现。
- `cargo clippy --release --all-targets`：5 条警告，都是原有的（`console.rs` 2 条、`process.rs` 的 `Live::flush_progress`、`policy.rs`、`tests/all/cli.rs`）；新代码里的 `type_complexity` 已用类型别名改掉。
- 格式：只对改过的文件跑 `rustfmt --edition 2024 --config max_width=100`，差异都在本次新增或改动的行里，已采用。
- 没有用 release 版接真实模型实际跑一遍界面（需要付费调用）；界面文字由集成测试里收到的事件确认。

## 遗留

- cove 重启（或崩溃）后不恢复后台命令：进程随 cove 结束，模型此前收到的"结束后会通知你"不会兑现。恢复会话时可以考虑补一条通知，说明这些命令已随上次进程结束；暂未做。
- 没有在 Linux/macOS 上编译和运行（本机只有 Windows 目标），sh 写法的测试脚本要靠 CI 确认；macOS 没有活动采样（0048），转后台的命令只受 3600 秒总时限，等待时状态栏不显示活跃/无活动。
- 状态栏计数只包括主会话的后台命令，子 Agent 的不计入（它们随子 Agent 结束）。
- hooks 的工具名匹配（`extensions/hooks.rs`）没有给 `shell_task` 配 Claude Code 风格的别名。
- 子 Agent、后台 Agent 结束时停止后台命令并写进结果，`/handoff` 时停止后台命令，这几条只有代码路径，没有专门的自动化测试。
- 锁等待只在 `LockWatch` 认得的提示行（cargo、yarn、bazel、msbuild、apt/dpkg）上关闭空闲判断；其他工具的等锁仍可能被判卡住，可以给长 `timeout_seconds`。判断占用者属于后台命令依赖进程列表里的父进程关系，父进程已退出（孤儿进程）时认不出来。
- shell 工具描述比 0044 精简后长了约两句；需要观察模型是否按描述不去轮询 `shell_task`（轮询会被 `guard.rs` 的重复调用检测拦下）。
- 0048 遗留的"开发服务器一直跑到 `timeout_seconds`"由 0051 的 `run_in_background` 解决；模型不用它时，服务器会在 120 秒后转后台、本轮结束前一直被等待（可 Esc 或插话打断）。

### 审查后的修正（2026-11-25）

- 实现：见决策记录中标"审查后"的各条。改动文件：`src/process.rs`（`Tap` 的 64 MiB 上限、`create_new`、命令进程 id、后台命令登记、锁等待后关闭空闲判断）、`src/tools/jobs.rs`（`Finished`、`observed`、编号续接、空闲上限覆盖、登记/注销后台进程）、`src/tools/mod.rs`（`timeout_seconds` 作空闲上限、描述一句）、`src/engine/turn.rs`、`src/engine/agent.rs`、`src/safety/gates.rs`、`src/app/locks.rs`、`src/app/mod.rs`（`stop_background`、`/handoff`）、`src/agents/background.rs`、`src/safety/verify.rs`（删 `slow_command`）、`docs/reference.md`。
- 新增测试：
  - `safety::gates::tests::a_background_end_is_evidence_and_shell_task_changes_nothing`：转后台时不算通过，结束（退出码 0）后算通过，`shell_task` 的 list/output/stop 不作废证据、不算做了改动，失败结束后移除。
  - `core_tests::a_command_moved_to_the_background_is_awaited_and_its_result_goes_to_the_model` 增加断言：turn hook 收到 `background_finished(命令, 成功)`。
  - `process::tests::a_tap_never_replaces_an_earlier_log_and_stops_at_its_limit`：已有的 `1.log` 内容不变、新日志另起文件名；写入超过上限后文件不超过上限加说明行，末尾是说明行，`capped()` 为真，最新输出仍在内存尾部。
  - `tools::jobs::tests::a_resumed_session_numbers_on_after_its_earlier_logs`：目录里已有 `3.log`、`5-77.log` 时新命令编号为 6，旧日志不变。
  - `tools::jobs::tests::a_long_timeout_raises_the_idle_limit`：空闲上限 1 秒、`timeout_seconds` 20 秒时，静默 3 秒的命令不被中止。
  - `process::tests::waiting_for_a_lock_is_not_taken_for_a_hang`（Windows/Linux）：输出 cargo 的锁等待行后静默 4 秒，空闲上限 1 秒，不被中止。
  - `app::locks::tests::a_lock_held_by_our_background_command_is_not_offered_for_ending`：占用者的父进程是登记过的后台命令时归为"本会话的后台命令 #2"，其他占用者照旧。
- 时间相关的测试放宽：`jobs` 的转后台测试和 `run_in_background` 测试改为轮询等首批输出出现（上限 30 秒）再断言；集成测试不再断言转后台那一刻的输出里已有首行（首行在完成通知里检查）、不再断言 `run_in_background` 返回时已有输出。
- `cargo test --release --no-fail-fast`：lib 545 通过、7 忽略（原有）；集成测试 246 通过；没有失败。
- `cargo clippy --release --all-targets`：5 条，均为原有。
- 格式：只对改过的文件跑 rustfmt，差异都在本次改动的行里，已采用。
