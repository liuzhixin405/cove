# 0046 全局偏好提问和"没有进展"询问的界面问题

- 状态：已完成
- 日期：2026-11-24（创建）；2026-11-24（完成）
- 关联：0030（全局偏好）、0032（按有没有进展止损）；`src/app/mod.rs`、`src/memory/preferences.rs`、`src/commands/project.rs`、`src/engine/progress.rs`、`src/engine/agent.rs`

## 背景

0030 和 0032 完成后，用 release 版在伪终端里实际跑了一遍交互界面（两份文档"验证结果"末尾的"交互界面验证（2026-11-24）"），流程和设计一致，但发现 6 个界面问题：2 个在全局偏好提问上，4 个在"没有进展"询问和停下后的显示上。都是现有路径上的小问题，一起改。

## 现状与问题

证据来自 0030、0032 的交互界面验证（release 版 `cove.exe`，pywinpty 140×45，假的 OpenAI 兼容服务只监听 127.0.0.1，临时数据目录和工作区）。下面的代码位置是改动前的当前代码，每条都读代码确认了路径还在。

**P1 提问时输入的任务被吞掉。** `Application::answer_preference`（`src/app/mod.rs`）把 `preferences::answer` 认不出的回答（不是 y/n 的任何文字）一律当成"以后再说"：`preferences.later(candidate)`，显示 `[偏好] 以后再问；也可以用 /rules pending 处理。`，然后回到循环开头读下一行，输入的文字被丢掉。复现：队列里有候选时启动交互模式，提问出现后输入"运行一下 echo"并回车，界面显示"以后再问"，假服务没有收到含这句话的请求（0030 交互验证第 3 步）。提问出现在输入提示的位置，没看到提问就开始打字时，这句任务就丢了。

**P2 回答完一条马上问下一条。** `repl_lines` 每次读输入前都调 `ask_preference`；空行直接 `continue` 回到循环开头，又会问一次。提问末尾的 `[y/n]` 让控制台把 `y`、`n` 当单键回答，按完 `y` 习惯性再按的回车就成了下一个输入提示里的空行。回答完第一条后按一次回车，第二条马上出现，用户没执行任何任务就连着被问两次（0030 交互验证第 2、3 步）。唯一的限制是每次启动最多 3 条（`ASKS_PER_RUN`）。

**P3 "没有进展"询问里选 2 没有确认行。** `settle_round`（`src/engine/agent.rs`）里 `StallChoice::KeepGoing` 发一条 `Event::Notice`（"照原样继续；再连续 N 轮没有进展时再问"），回车停下时 `stall_stop` 也发一条，`StallChoice::Retry` 只把说明附给模型，界面上什么都没有（0032 交互验证第 2 步）。

**P4 "反复出现 ×N"是整个任务的累计次数。** `Progress::count_error`（`src/engine/progress.rs`）按签名累加整个任务的次数，`repeated` 直接返回累计值；询问标题却是"最近 12 轮没有新的改动或新信息"。实测三次询问里同一条 guardrail 错误显示 ×8 → ×20 → ×44，第二次询问时标题是"最近 12 轮"，读起来像这 12 轮里出现了 20 次。发给模型的提醒（`warning`）、换思路说明（`retry_note`）和收尾事实（`facts`）用的是同一个数。

**P5 被 guardrail 拦下的那一类，标签固定是第一次的原文。** 被拒绝的调用签名是 `hash(("refused", 工具, normalize(首行)))`，数字已经归一化；标签却是第一次出现时的原文 `[guardrail] shell failed 5 times within 30 s; the…`。之后每次拦下时实际次数在变（guardrail 的文字里带 30 秒内的失败次数），界面一直显示"failed 5 times"。

**P6 停下后同一件事说两遍，还标成"错误"。** 任务被 cove 停下时 `Agent::run_in` 返回 `Error::Stopped`；`execute` 先打印结束行 `■ 已停下 · 因连续 24 轮没有进展提前停止 · …`（`print_report` → `render::report`），再把错误返回给 `repl_lines`，后者 `print_error` 打一行 `错误：没有进展，已停下：连续 24 轮没有新的改动或新信息；用 /continue …`（`Stop::message`）。同一件事说了两遍，而停下是 cove 有意为之的结果，不是错误。用完调用次数（`Stop::Limit`）走同一条路径，同样重复。

## 目标

- 提问时输入的文字照常作为任务执行，这条候选照"以后再说"处理。
- 两次任务之间最多问一条偏好；`/rules pending` 主动查看时仍逐条问。
- "没有进展"询问的三个选项都有一行确认。
- "反复出现 ×N"只数询问标题所说的那几轮；guardrail 和循环检测拦下的调用不再显示第一次的原文。
- cove 停下任务后，交互界面只在结束行说一次停下的原因，后面跟一行怎么继续的提示，不出现"错误："。

## 范围

- 做：P1–P6。
- 不做：
  - 单键回答：提问末尾的 `[y/n]` 让控制台在输入框为空时把 `y`、`n` 当单键回答（`console.rs` 的 `answer_keys`），所以以 y/n 开头的任务文字的第一个字母会被当成回答。这是所有 `[y/N]` 类询问共用的输入机制，改它影响审批询问，不在本文处理，记入遗留。
  - `-p` 下停下时的输出：`[summary] … 因连续 12 轮没有进展提前停止，任务没有完成` 之后，`main.rs` 在标准错误打印 `错误：…` 并以非零退出。这是 `-p` 的退出约定（脚本靠退出码和标准错误判断），不改。
  - 子 Agent 的停下（`exit: stalled`）：结果交给父模型，不经过交互界面。

## 调研与参考

- 偏好提问的时机：0030 的设计是"任务之间、读输入前、每次启动最多 3 条"。P2 的根因是"读输入前"不等于"任务之间"：空行、斜杠命令也会回到读输入前。改成以"跑过一个任务"为界，比加时间间隔简单、可测。
- 输入被吞：同类的提问（审批询问、`ask_user`）都是任务进行中的阻塞询问，用户知道自己在回答；偏好提问是 cove 主动插在输入提示位置的，用户可能没看到就开始输入任务，所以不是 y/n 的文字更可能是任务，按任务执行。
- 累计还是窗口：两种都能消除误读。标注"累计"要改三处文字、标题和次数仍然对不上；只数最近这几轮，和标题说的是同一件事，也更贴近"这几轮在原地打转"的判断依据。选窗口。
- guardrail 标签：两个办法是"每次更新为最新原文"和"不显示原文"。更新为最新原文仍然是一句英文的内部提示，而且 guardrail 同一类里有几种不同文字（30 秒内失败 N 次、连续失败 8 次），会被拆成几条签名相同含义却不同的行。改为只显示拦下它的机制名，签名也按机制名归类。

## 方案

**P1、P2（`src/memory/preferences.rs`、`src/app/mod.rs`、`src/commands/project.rs`）**

- `preferences::answer` 改为返回 `Reply`：`Yes`、`No`、`Later`（空回答）、`Task(String)`（其他文字，原样保留）。
- `Application::answer_preference` 返回 `Result<Option<String>>`：`Task` 时照旧 `later`，显示 `[偏好] 以后再问（/rules pending 可以处理）；刚才输入的内容作为任务执行。`，并把这句话交回调用方。
- `Preferences` 增加"本次任务间隔已经问过"的标记：`next_question` 问出一条后置位，置位期间不再问；新方法 `task_done` 清除。`repl_lines` 在一行内容作为任务执行后（不是斜杠命令）调 `task_done`。`claim`（`/rules pending`）不受这个标记限制。
- `repl_lines`：`ask_preference` 交回的文字直接当作这一轮的输入行，不再读输入。
- `/rules pending`：逐条问；遇到不是 y/n 的文字时，这一条照"以后再说"处理、剩下的不再问，这句话由 `Application` 的 `typed` 字段交给 `repl_lines` 作为下一行执行。

**P3（`src/engine/agent.rs`）** `StallChoice::Retry` 时发一条 `Event::Notice`：`换个思路再试；再连续 N 轮没有进展时再问`。

**P4（`src/engine/progress.rs`）** `Progress` 记录已结算的轮数 `rounds`；每个错误签名记下出现在哪几轮（不再只记总数）。`repeated` 只数最近 `idle` 轮（即标题所说的那几轮）里的出现次数，少于 2 次的不列。提醒、询问、换思路说明、收尾事实都用这个数。

**P5（`src/engine/progress.rs`）** 被拒绝的调用，首行以 `[guardrail]` 或 `[loop detection]` 开头时，签名和标签都只用机制名：标签为 ``shell `ls x` → 被拦下（guardrail）``。其他拒绝（用户拒绝、拒绝规则、计划模式、无人值守）的原文由用户或规则决定、不随次数变化，保持原样。

**P6（`src/engine/agent.rs`、`src/app/mod.rs`）** `Stop` 增加 `hint()`（停止后怎么继续），`message()` 由原因和 `hint()` 拼成，文字不变。`repl_lines` 遇到 `Error::Stopped` 时不再 `print_error`，只打印一行 `hint()`（诊断色，不带"错误："）；结束行仍是唯一说明停下原因的地方。`-p` 和其他错误不变。

## 影响范围

- `src/memory/preferences.rs`：`answer` → `Reply`；`Preferences` 新增标记和 `task_done`。
- `src/app/mod.rs`：`Application` 新增 `typed` 字段；`repl_lines`、`ask_preference`、`answer_preference`。
- `src/commands/project.rs`：`/rules pending` 的循环。
- `src/engine/progress.rs`：错误记录从总数改为出现的轮次；被拦下调用的签名和标签。
- `src/engine/agent.rs`：`Stop::hint`、Retry 的确认行（局部编辑，不碰系统提示文本）。
- `docs/reference.md`：偏好提问的时机和其他输入的处理、止损询问里"反复出现"的含义。
- 数据格式：无变化（`preferences.pending.json` 不变）。
- 行为变化：偏好提问比原来少（每个任务间隔最多一条）；"反复出现"的数字比原来小；被拦下的调用标签变短；交互界面停下后不再显示"错误："一行。

## 验证

- 单元测试：
  - `preferences.rs`：`Reply` 的四种结果；问过一条后 `next_question` 返回空、`task_done` 后再问；`claim` 不受影响。
  - `progress.rs`：同一错误在 `keep_going` 前后，`repeated` 的次数只数当前窗口；guardrail 拦下的调用文字里次数在变时归为一类，标签不含原文。修改原有的 `×7` 断言（窗口内是 ×6）和"被拒绝"断言。
- 集成测试 `tests/all/core.rs` 的 `a_stalled_task_asks_and_each_choice_does_what_it_says`：收集事件，断言选 2 后有"换个思路再试"一行；第二次询问的次数是窗口内的 ×4（累计是 ×7）。
- `cargo test --release` 全过；`cargo clippy --release --all-targets` 不新增警告。
- release 版交互实测（同 0030/0032 的环境：假服务、pywinpty、临时数据目录和工作区），改动前用 0030 验证时的二进制、改动后用新二进制跑同一组步骤，屏幕记录存 `%TEMP%\ui0046\`：
  - 偏好：3 条候选；启动后问第一条 → `y` → 空回车（应当不再问）→ 输入任务（执行）→ 任务结束后问第二条 → 直接输入任务（以后再问，并执行）→ 任务结束后问第三条 → `n` → `/rules pending`（应当问放回去的第二条）→ 回车。
  - 止损：同 0032 的步骤（2 → 3 → 回车），看确认行、次数、标签和停下后的显示。

## 实施计划

- [x] P1、P2：`Reply`、提问标记、`repl_lines`、`/rules pending`
- [x] P3、P6：`agent.rs`、`repl_lines`
- [x] P4、P5：`progress.rs`
- [x] 测试、文档、交互实测

## 待决策

无。

## 决策记录

- 2026-11-24：提问时输入的不是 y/n 的文字，照旧算"以后再说"（候选放回队列），同时作为任务执行。理由：提问插在输入提示的位置，这样的文字更可能是任务。
- 2026-11-24：每两次任务之间最多问一条，剩下的留到下一个任务结束后；`/rules pending` 主动查看时仍逐条问。理由：用户主动查看时知道自己在回答。
- 2026-11-24："反复出现 ×N"改为只统计最近这一窗口（询问标题所说的那几轮）内的次数，不改成标注"累计"。理由：和标题说的是同一件事，判断"是否在原地打转"也只看这几轮。
- 2026-11-24：guardrail 和循环检测拦下的调用，按机制名归类、显示时不带原文。理由：原文是给模型看的英文提示，次数会变，同一机制还有几种文字。
- 2026-11-24：交互界面上停下的原因只在结束行说一次，之后只打印怎么继续，不当成错误；`-p` 不变。

## 验证结果

- 新增单元测试：`preferences.rs` 的 `at_most_one_question_between_two_tasks`（问过一条后 `next_question` 为空，`claim` 照常给出，`task_done` 后再问下一条），`questions_wait_for_an_idle_terminal_and_stop_after_three` 补了 `Reply` 的四种结果；`progress.rs` 的 `repeats_are_counted_in_the_current_idle_rounds`（`keep_going` 后清零，第二次停下时窗口内 ×8，整个任务累计 13 次）和 `refusals_of_a_repeat_detector_are_one_kind_named_by_it`（文字里次数从 5 变到 16 的 12 次 guardrail 拒绝归为一类，标签 ``shell `ls missing` → 被拦下（guardrail）``）。原有断言按新的口径改了三处：`×7` → `×6`、`Connection refused (x13)` → `(x12)`（第一次是新信息，不在窗口内）、"被拒绝" → "被拦下（guardrail）"。
- 集成测试 `a_stalled_task_asks_and_each_choice_does_what_it_says`：选 3、选 2、回车各有一行确认；第二次询问显示 ×4（整个任务累计 7 次）。
- `cargo test --release --no-fail-fast`：单元 524 通过、2 失败；集成 237 通过、5 失败。7 个失败都和本文无关，单独重跑全部通过：
  - `session::snapshot` 的 2 个（`rm a.txt && …` 被判成需要审批，返回 `Unattended`）：同一时间 `src/safety/permission/risk.rs` 正在被另外修改；前一轮全量测试里单元测试 526 个全过，这一轮之后单独跑 `snapshot::tests` 16 个全过。
  - 集成测试的 5 个（`background_agent_interrupt_restart_resume_notify_and_merge`、`task_deadline_cancels_a_pending_http_stream_without_hanging`、`all_hook_events_run_with_scrubbed_environment_and_session_end_once`、`cancelled_and_timed_out_mcp_operations_kill_owned_descendants`、`files_changed_by_shell_are_progress_and_unchanged_ones_are_not`）：都是超时（30 秒任务时限、MCP 超时、`Elapsed`），本机同时在编译别的构建；`--test-threads=1` 单独重跑 5 个全过。
- `cargo clippy --release --all-targets`：5 条警告（`console.rs` 2 条、`process.rs`、`policy.rs`、`tests/all/cli.rs` 各 1 条），和改动前相同，本文改动的文件没有警告。
- rustfmt（`--edition 2024 --config max_width=100`）只对改过的 6 个 .rs 文件运行，差异都在本次改动的行里，已采用。
- release 版交互实测（假服务只监听 127.0.0.1，pywinpty 140×45，每次新建数据目录和 Git 工作区；改动前用 0030 验证时的二进制，改动后用本次构建）。屏幕记录在 `%TEMP%\ui0046\`：`before-pref.txt`、`before-pref-ws2-swallowed.txt`、`before-stall.txt`、`after-pref.txt`、`after-stall.txt`，假服务收到的请求在同名 `.requests.jsonl`。
  - 偏好，改动前：回答 `y` 后的回车直接带出第二条提问（P2）；接着输入的"运行一下 echo"显示"以后再问"，假服务一个请求都没收到（P1）。在另一个工作区用同一个数据目录启动时，任务文字"STUCK 看看 …"同样被提问吞掉。
  - 偏好，改动后：`y` 后的回车不再提问；输入任务正常执行，结束后才问第二条；在第二条提问处输入"再运行一下 echo"，显示 `[偏好] 以后再问（/rules pending 可以处理）；刚才输入的内容作为任务执行。`，任务执行（假服务收到的 6 个请求里都有这句话），结束后问第三条；`n` 后的回车不再提问；`/rules pending` 问到放回去的第二条，回车后"以后再问"。
  - 止损，改动前：三次询问 guardrail 一行为 `被拒绝：[guardrail] shell failed 5 times within 30 s; the… ×8` → `×20` → `×44`；选 2 无确认行；停下后结束行之外还有 `错误：没有进展，已停下：…`。
  - 止损，改动后：三次询问为 `被拦下（guardrail） ×8`（另有 `ls: cannot access … ×4`）→ `×12` → `×24`，和标题"最近 12/12/24 轮"一致；选 2 显示 `! 换个思路再试；再连续 12 轮没有进展时再问`；停下后是 `■ 已停下 · 因连续 24 轮没有进展提前停止 · 50 次模型调用 · …`，下一行只有 `用 /continue 换个思路继续，或补上缺的外部条件（服务、权限、依赖）`，没有"错误："。假服务两次都收到 49 个带工具的请求和 1 个收尾请求，轮数不变；换思路说明里给模型的也是窗口内次数（`被拦下（guardrail） (x8)`）。

## 遗留

- 单键回答：输入框为空时 `y`、`n` 作为单键回答（`console.rs` 的 `answer_keys`），以 y/n 开头的任务文字在偏好提问处会被当成回答。审批询问共用这套机制，没有改；要处理需另开文档。
- 停下的显示只改了交互界面；`-p` 仍在 `[summary]` 之后由 `main.rs` 打印 `错误：…`，这是退出约定，保留。
- 被拒绝调用的标签里，命令参数仍是这一类第一次出现时的参数（签名不含参数，同一错误换着命令出现算一类），和 P5 无关，没有改。
