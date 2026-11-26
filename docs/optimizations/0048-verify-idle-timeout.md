# 0048 测试和构建命令按"是否还在干活"判断卡住，不按固定时长砍掉

- 状态：已完成
- 日期：2026-11-25（创建）；2026-11-25（完成）
- 关联：0047（同一个 .NET 仓库里发现）

## 背景

一个 .NET 仓库的集成测试单个项目约 270 秒（验证账本：`QuoteHub.Admin.IntegrationTest` 271630 ms、`QuoteHub.Console.IntegrationTest` 269527 ms），全量 `dotnet test` 必然超过 300 秒。自动验证两次都在 300 秒时被杀（账本 `status: timed_out`、`duration_ms: 300081`），结果是"验证未运行"。测试当时在正常运行，只是需要更久；用户等了 5 分钟，得到的是一条不能说明任何问题的超时。

## 现状与问题

1. **固定时长，不看进度。** `CommandVerifier::timeout`（`src/safety/verify.rs:771`）：测试/构建类命令 300 秒，其余 120 秒；`done_verify_timeout_seconds` 只能把这个固定值改成另一个固定值（1..3600）。`process::run`（`src/process.rs:574`、`583`）到点就 `timed_out = true` 并杀掉整个进程树，不管它还在不在干活。
2. **模型自己跑测试也一样。** shell 工具默认 120 秒，构建/测试 300 秒，最多 600 秒（`src/tools/mod.rs:1082`、`SHELL_MAX_TIMEOUT`）。慢的测试套件模型自己也跑不完。
3. **超时原因说不清。** 结果只有 `not run dotnet test (timed out after 300s; not verified)`。看不出是卡在等网络或数据库、死锁，还是正常跑但慢；也不知道当时还剩哪个进程、最后输出了什么。
4. **运行期间看不出是否还活着。** 验证不逐行输出（`run` 里的 "No per-line streaming"），`dotnet test` 本身也要等一个测试项目跑完才打印。界面上只能看到在转圈。

## 目标

- 测试或构建只要还在干活（有输出、或者进程树在消耗 CPU、在读写），就不因为时长被杀。
- 完全不动超过一段时间（默认见"待决策"）才中止，并写明实际原因：多久没有动静、各项信号的最后变化时间、还在运行的进程（名称、pid、各自的 CPU 时间）、最后几行输出，以及按工具给出的排查建议。
- 运行期间状态栏显示已运行时长和是否活跃，例如 `回复已结束，自动验证中（无需操作） · dotnet test · 6m12s · 活跃`；没动静时显示 `· 已 45s 无活动`。

## 范围

- 做：`process::run` 支持"空闲上限"；自动验证（任务收尾和提交门禁）的测试/构建命令改用它；中止原因的收集和展示；状态栏显示时长和活跃状态。
- 待决策：模型调用的 shell 工具、回归验证子 Agent（`agents/regression.rs`）、`review_commands` 是否同样改用空闲上限。
- 不做：
  - 判断死循环。一直在消耗 CPU 的死循环从外部看和正常计算一样，无法区分；靠总时长上限（待决策）或用户按 Esc 中止。
  - 进程树以外的活动。比如 Testcontainers 的容器、远程数据库里的工作，算力消耗不在 cove 起的进程里；只能靠测试进程自己的输出和 I/O 判断，见"风险"。

## 调研与参考

- 同类做法：
  - 只按总时长：Claude Code 的 Bash 工具（默认 2 分钟、最多 10 分钟，长任务让模型放后台）、Codex CLI 的命令超时，和 cove 现状一样。
  - 按"无输出"：Travis CI 的 "No output has been received in the last 10m0s"、Jenkins build-timeout 插件的 "no activity" 策略。只看输出不够：`dotnet test` 一个测试项目跑完前可能几分钟没有输出，单靠输出会误杀，而 0047 的现场正是这种情况。
  - 测试框架自己的卡住检测：VSTest 的 `--blame-hang --blame-hang-timeout <时长>` 在单个测试超时后打印正在运行的测试名并生成 dump；pytest-timeout 插件；`go test -timeout` 超时会打印所有 goroutine 的栈。它们能说出"卡在哪个测试"，但不是每个项目都装了，也不是每种工具都有。
- 可用的活动信号（cove 已有的进程树）：
  - Windows：每条命令已经在一个 Job 对象里（`ProcessTree`，`src/process.rs:632`）。`QueryInformationJobObject(JobObjectBasicAndIoAccountingInformation)` 给出整个进程树的累计用户态/内核态 CPU 时间、读写次数和字节数、存活进程数；`JobObjectBasicProcessIdList` 给出进程列表。每次查询是一次系统调用，每 5 秒一次可以忽略不计。
  - Linux：命令有自己的进程组（`ProcessTree(pgid)`）；扫描 `/proc/*/stat` 找同一进程组的进程，汇总 `utime+stime`，读 `/proc/<pid>/io` 的 `rchar/wchar`。
  - macOS：没有 `/proc`；可以用 `libc::proc_listpids` + `proc_pidinfo(PROC_PIDTASKINFO)` 读 CPU 时间。先只用输出和直接子进程的 CPU 时间，实现成本高的部分放到后面。

## 方案

### 1. 活动判断（`process.rs`）

- `run` 的 `timeout: Duration` 换成 `Limit { idle: Option<Duration>, total: Option<Duration> }`。`Limit::total(d)` 等价于现在的行为，没有改用空闲上限的调用方行为不变。
- 每 5 秒取一次快照：输出字节数（两个 reader 共享一个计数）、进程树 CPU 时间、I/O 次数、进程 id 集合。和上一次比，满足任一条就算"有活动"并刷新最后活动时间：
  - 有新输出；
  - CPU 时间增加超过 50 ms（5 秒内平均 1% 单核，过滤掉空等时的零星调度）；
  - 读写次数增加；
  - 有进程启动或退出。
- 距最后活动超过 `idle`，就判定卡住：先收集原因，再杀掉进程树。`CommandOutput` 新增 `stalled: Option<Stall>`（`timed_out` 仍表示超过总时长）。

### 2. 中止原因（`Stall`）

```text
dotnet test 已运行 9m40s，最近 180s 没有任何活动，已中止：
- 输出：最后一次在 3m05s 前（"Starting test execution, please wait..."）
- CPU：整个进程树最近 180s 增加 0.02s
- 读写：最近 180s 没有
- 仍在运行：testhost.exe（pid 12345，CPU 共 41.2s）、dotnet.exe（pid 12001，CPU 共 6.3s）
可能原因：测试在等网络、数据库或锁，或者在等输入。
建议：dotnet test 加 --blame-hang --blame-hang-timeout 2m 可以打印卡住的测试名。
```

- 进程名取自 Job 的进程列表（Windows）或 `/proc/<pid>/comm`（Linux）。
- 建议按命令给出：`dotnet test`（VSTest）→ `--blame-hang-timeout`；pytest → `pytest-timeout`；`go test` → `-timeout` 会打印栈；其他只写通用原因。
- 验证结果：账本 `status: "stalled"`；汇总写 `not run  dotnet test（卡住：最近 180s 没有活动；not verified）`；交给模型的详情是上面整段加上输出末尾。

### 3. 验证接入（`verify.rs`）

- 测试/构建类命令（`slow_command`）用 `Limit { idle: 空闲上限, total: 总上限 }`；其他命令保持 120 秒总时长。
- `done_verify_timeout_seconds` 的含义变为总上限（见"待决策"）；新增 `done_verify_idle_seconds`。
- 运行期间每 5 秒刷新状态栏：`<阶段> · <命令> · <已运行时长> · 活跃` / `· 已 <n>s 无活动`（复用 0047 的 `Event::Activity`）。

### 4. 文档

`docs/reference.md` 的 `done_verify_timeout_seconds`、`done_verify_tests` 两行，以及 shell 工具的超时说明（如果待决策 2 选了一起改）。

## 决策记录

- 2026-11-25：保留总时长上限，默认 60 分钟，只用来兜住一直消耗 CPU 的死循环；`done_verify_timeout_seconds` 改为设置这个总上限（范围 1..86400）。原因：完全不限时，死循环只能靠用户发现后按 Esc。
- 2026-11-25：范围包括自动验证（任务收尾、提交门禁）、模型调用的 shell 工具、回归验证子 Agent 和 `review_commands`。shell 工具里，测试/构建类命令用空闲上限加总上限，模型给的 `timeout_seconds` 作为总上限（上限从 600 秒放宽到 3600 秒）；其他命令保持原来的固定时长。原因：只改验证的话，模型自己跑全量测试仍会被 600 秒截断。
- 2026-11-25：空闲上限默认 180 秒，可用 `done_verify_idle_seconds` 修改。原因：要兼顾等容器启动、拉镜像的集成测试。
- 2026-11-25：这次不自动给 `dotnet test` 加 `--blame-hang-timeout`，只在中止原因里给出建议。原因：只对 VSTest 有效，会改变用户看到的命令，还会生成 dump 文件。

## 影响范围

| 文件 | 改动 |
|---|---|
| `src/process.rs` | `Limit`、活动快照（Windows Job 统计、Linux `/proc`）、`Stall`、`run` 的等待循环 |
| `src/safety/verify.rs` | 超时规则、`stalled` 的汇总和账本、状态栏刷新 |
| `src/config/mod.rs` | `done_verify_idle_seconds`，`done_verify_timeout_seconds` 含义调整 |
| `src/tools/mod.rs`、`src/agents/regression.rs` | 取决于待决策 2 |
| `docs/reference.md` | 对应配置和工具说明 |

## 风险

- 进程树之外的活动看不到。Testcontainers、远程服务里的工作不算在内；测试进程在等它们时如果没有输出也没有读写，超过空闲上限会被中止。中止原因会写明"在等网络、数据库或容器"，用户可以调大 `done_verify_idle_seconds`。
- Windows 的 I/O 计数是否包含套接字读写，需要实测确认；不包含的话，只在等网络的测试只能靠 CPU 和输出判断。

## 验证

- 单元测试（`process.rs`）：
  - 一直输出但总时长超过空闲上限的命令，不会被中止；
  - 不输出、但持续消耗 CPU 的命令（忙循环 N 秒后退出），不会被中止；
  - 不输出、不消耗 CPU 的命令（`sleep`），超过空闲上限被中止，`Stall` 里有进程名和各项信号；
  - `Limit::total` 的行为和现在一致（现有超时测试照常通过）。
- 验证接入：用一个假测试命令（先输出、再忙 N 秒、再退出）确认结果是"通过"；用 `sleep` 确认账本状态是 `stalled`，汇总里有原因。
- 真实仓库：在那个 .NET 仓库跑一次全量 `dotnet test` 的自动验证，确认 10 分钟左右的全量测试能跑完并判"通过"，状态栏一直显示"活跃"。
- `cargo test --release`、`cargo clippy --release --all-targets` 不新增警告。

## 验证结果

### 实现

- `src/process.rs`：
  - 新增 `Limit { total, idle, status }`（`From<Duration>` 保持原行为）、`Limit::watched`、`IDLE_LIMIT`（180 秒）、`WATCHED_TOTAL`（3600 秒）、`clock`；`CommandOutput` 新增 `stalled: Option<String>`。
  - `Watch` 判断活动：新输出、CPU 增量超过 50 ms、读写次数增加、进程集合变化，满足任一就刷新；正式版每 5 秒采样一次（单元测试里 200 ms）。
  - `ProcessTree::sample`/`processes`：Windows 用 `QueryInformationJobObject`（`JobObjectBasicAndIoAccountingInformation`、`JobObjectBasicProcessIdList`），以及每个进程的 `QueryFullProcessImageNameW`、`GetProcessTimes`；Linux 扫 `/proc/*/stat` 里同一进程组的进程，读 `/proc/<pid>/io` 的 `syscr`/`syscw`；其他 Unix 返回 `None`，那里只按总时限。
  - `run` 的等待循环加上采样分支；判定卡住后，先在进程树还在的时候列出仍在运行的进程，再杀进程树。
  - 公开的 `shell`、`shell_live`、`shell_with_env`、`shell_configured`、`run_shell` 改为接受 `impl Into<Limit>`，hooks 等原有调用方照旧传 `Duration`，行为不变。
- `src/safety/verify.rs`：所有检查命令用 `Limit::watched(done_verify_timeout_seconds 或 3600, done_verify_idle_seconds 或 180)`，状态栏显示 `<阶段> · <命令> · <已运行时长> · 活跃/已 N 无活动`；卡住时汇总写 `not run  <命令>（卡住，已中止；not verified）` 加原因全文，账本状态 `stalled`，结果算"未验证"；改动前基准运行用同样的限制；`failing_tests` 对卡住的运行不给用例级结果。新增 `with_idle`。
- `src/config/mod.rs`、`src/config/trust.rs`、`src/safety/gates.rs`：新增 `done_verify_idle_seconds`（10..3600），`done_verify_timeout_seconds` 改为总时限（1..86400）。
- `src/commands/project.rs`（`/commit` 的验证）：同样的限制，卡住时报错并带原因。
- `src/tools/mod.rs`：构建/测试类命令（`slow_command`）用空闲上限，`timeout_seconds` 是总时限，默认和上限都改为 3600；卡住时结果开头是 `[stalled: stopped for lack of any activity] <原因>`，记为失败；工具描述同步。
- `src/agents/regression.rs`：每次运行用空闲上限，`timeout_seconds` 默认 3600、范围 10..3600；卡住记为 `unverified`。
- `src/safety/gates/review_command.rs`：审查命令加空闲上限，总时限仍是 `review_timeout_seconds`；卡住时审查记为跳过并写原因，账本状态 `stalled`。
- `docs/reference.md`：`review_timeout_seconds`、`done_verify_timeout_seconds`、新增 `done_verify_idle_seconds`、可信项目字段列表、Shell 时限、regression。

### 测试

- 新增单元测试：
  - `process::tests::idleness_counts_from_the_last_sign_of_work`：用构造的样本确认输出、CPU、读写、进程启动都会刷新计时，50 ms 以内的 CPU 增量不算；原因文字里有运行时长、空闲时长、CPU 增量、最后输出时间、进程名和 `--blame-hang-timeout` 建议。
  - `process::tests::a_command_that_does_nothing_is_stopped_with_the_reason`（Windows/Linux）：`Start-Sleep 30`/`sleep 30` 在空闲上限 2 秒时被中止，原因里有 `powershell`/`sleep`。
  - `process::tests::quiet_work_and_steady_output_are_not_stopped`（Windows/Linux）：没有输出、只占 CPU 4 秒，以及每 0.5 秒输出一行、共 4 秒，空闲上限 1–1.5 秒，都没被中止。
  - `safety::verify::tests::a_stuck_check_is_stopped_and_says_why`：卡住的检查结果是"未验证"，汇总里有原因，账本是 `stalled`。
  - `process::tests::watch_command`（`#[ignore]`，开发用）：用默认限制运行 `COVE_WATCH_COMMAND`，打印结果。
- `tests/all/tools.rs`：shell 时限上限的断言改为 `1..3600`。
- 合成验证（`watch_command`，默认限制；单元测试构建，采样间隔 200 ms）：
  - bash 忙循环 320 秒、不输出、不起子进程：运行 5m19s，退出码 0，没有卡住、没有超时。原来的规则会在 300 秒杀掉它。
  - `Start-Sleep -Seconds 600`：在 3m30s 时中止，原因如下。比 180 秒多出的 30 秒是 PowerShell 启动后的读写（整个进程只用了 0.5 秒 CPU）：

```text
已运行 3m30s，最近 3m00s 没有任何活动，已中止：没有输出，整个进程树的 CPU 时间只增加了 0.03s，没有读写，没有进程启动或退出。
启动以来没有新的输出。
仍在运行：powershell.exe（pid 28056，CPU 共 0.5s）。
可能原因：在等网络、数据库、容器、锁或输入，或者死锁。
```

- `cargo test --release --no-fail-fast`：lib 530 通过、1 失败（`session::snapshot::tests::auto_deletes_only_files_the_snapshot_before_the_call_holds`，之前就出现过，单独重跑通过）；集成测试 237 通过、5 失败（`task_deadline_cancels…`、`all_hook_events…`、`cancelled_shell_kills…`、`cancelled_and_timed_out_mcp…`、`files_changed_by_shell…`），用 `--test-threads=1` 重跑 5 个全部通过。这几个就是 0041/0044 记录过的、并行负载下的超时类偶发失败。修完 clippy 后 lib 再跑一次：531 通过。
- `cargo clippy --release --all-targets`：5 条，都是原有的（`console.rs` 2 条、`process.rs:332` 即原来的 138 行、`policy.rs`、`tests/all/cli.rs`）；新代码里的 `unnecessary_sort_by` 已改掉。
- 格式：只对改过的文件跑 rustfmt；`process.rs` 的差异都在新代码里，已采用；`regression.rs` 唯一的差异在原有代码，没有采用；其他文件没有差异。

## 遗留

- 没有在 .NET 仓库里实际跑全量 `dotnet test`。那个仓库的集成测试可能会读写共享的外部资源，要不要跑由仓库负责人决定。命令：在仓库根目录设 `COVE_WATCH_DIR=<仓库>`、`COVE_WATCH_COMMAND="dotnet test"`，运行 `cargo test --release --lib watch_command -- --ignored --nocapture`。
- Linux 的 `/proc` 采样代码只在 Windows 上编译过（本机只装了 `x86_64-pc-windows-gnu` 目标），没有在 Linux 上编译或运行；要靠 CI 的 Linux 检查确认。
- macOS 没有采样：只按总时限（默认 3600 秒）。
- Windows 的读写计数是否包含套接字收发还没有实测；如果不包含，只在等网络的测试只能靠 CPU 和输出判断，可能被判为卡住（已在原因里写出"在等网络"）。
- shell 工具里，`npm start` 这类一直在跑的开发服务器属于"慢命令"：只要有 CPU 或读写就会一直跑到 `timeout_seconds`（默认 3600 秒），原来是 300 秒。工具描述写明了不支持后台任务；需要观察模型会不会这样用。
- 验证期间插话不能打断验证（同 0047）。
