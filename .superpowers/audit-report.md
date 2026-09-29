# cove 主动审计报告（2026-09-27）

审计方式：只读逐文件阅读 + 针对可疑点写临时 `_test.go` 验证后删除。审计期间仓库在被并发修改（`repl_loop.go`/`repl_tasks.go` 14:39 改为 steer 语义、`activity.go` 14:28、`pinned.go` 14:38 等），下文行号以 **14:39–14:45 读取的版本**为准；`cli/cove/repl_*.go` 已按新版重读。结束时 `gofmt -l .` 无输出、`go build ./...` 通过，无遗留文件改动。注意：`internal/engine` 的**测试包**当前编不过（`review_fixes_test.go:105` 以三参数调用 `reportStall`，与 `activity.go` 的双参数签名不一致）——这是并发编辑造成的，不是本次审计改动，但意味着 engine 的测试当前形同未跑。

审计口径与用户反馈一致：找「happy path 成立、真实环境（本地模型 / Windows 终端 / 任务与输入并发）不成立」的逻辑。

---

## 1. 总览

| 模块 | 一句话结论 |
|------|-----------|
| `cli/cove/repl_loop.go` / `repl_tasks.go` | 输入分发层与引擎之间没有任何互斥：主循环上的 `/compact`、`/cd`、`/model`、`/history N`、`exit` 都会在任务 goroutine 正在写 `e.messages` 时同时改引擎状态；新加的 steer 在任务失败后会「抢跑」并吃掉 `/continue` 的恢复点。 |
| `cli/cove/chat_interaction.go` | turnPrinter 本身的交错处理合理；REPL 层对 "timeout" 的 3 次重试与手册「超时不重试」相悖，退避 sleep 不可取消。 |
| `internal/repl/readline.go` / `pinned.go` | 授权提示与打字预输入共用同一条 relay，任何非答案行（包括空回车、`exit`、slash 命令、正在打的下一条指令）都会被当成「拒绝」并丢弃；窗口缩放时钉住行未重新定位流光标。 |
| `cli/cove/permission_prompt.go` + `internal/permission/` | 前缀/引号/替换检查扎实；**git 常规操作组**是漏洞集中区：`checkout <无点路径>`/`checkout *` 直接丢工作区改动，`push origin +ref` 是强推，长选项用精确匹配、git 接受唯一前缀缩写（`--force-w`/`--del`/`--disc`），`branch --delete` 长写法未拒。 |
| `internal/engine/engine.go` 等 | `/model` 切换后路由器仍持旧模型名，实际请求发的是旧模型；小窗口（≤24K）本地模型的压缩触发点低于固定开销，进入「每 3 轮摘要一次」；上下文超长第一次报错不学习窗口；`max_tokens` 续写循环无上限；stall 监控对慢速本地模型、`-p` 非流式、`question` 工具等待用户都会误报。 |
| `internal/api/` | 错误归类以状态码优先、文本兜底，方向正确；`fallback` 单供应商时会把连续 3 次上下文超长（400）记成 E2009「供应商不可用」。 |
| `internal/diagnostic/` | 结构化记录/处置器并发安全（ring buffer 加锁、追加写单次 write）；处置器只在**第二次**超长时才有机会运行；`-p` 下 `Notify` 写到 stdout 污染答案。 |
| `internal/termui/` | Spinner/StreamPrint 的锁序正确、可重启；`⚡` 按 1 列计宽（实际 2 列）。 |
| 文档 vs 代码（第 4 节） | 手册多处描述的行为没有实现或已换语义：`/model` 的「强制指定」从未接到路由器；`/new`、`/clear` 命令不存在却被文案引用；`verbose` 配置无任何行为；`uiout.Sink`/`SetOutput`、`OnToolStart` 等回调无人赋值；headless/-p 完全没有接引擎输出。 |

---

## 2. 问题列表

严重度定义：**Critical** = 数据丢失 / 误放行危险命令 / 死锁或崩溃 / 功能根本不生效；**Important** = 用户会明确遇到的错误行为或误导性文案；**Minor** = 边缘。

### Critical

#### C1. `/model` 与 `/provider` 切换后，实际请求仍发送旧模型名
- **位置**：`internal/engine/engine.go:1257-1263`（`routedModel = decision.Model`，只要 `modelRouter != nil` 就无条件覆盖 `e.config.Model`）、`engine.go:620-636`（`ReloadProvider` 只改 `e.config.Model`，不碰 `modelRouter`）、`internal/api/router.go:157-172`（`Route` 返回路由器自己保存的 `defaultModel`/`fastModel`）、`router.go:113-137`（`SetModels`/`SetOverride`/`ClearOverride` 在非测试代码里**零调用**）。请求体用 `req.Model`：`openai_compat.go:161,468,661`、`anthropic.go:178,714`。
- **触发条件**：以 `model: A` 启动 → `/model B`（或 `/provider` 切到另一家）→ 发一条消息。
- **现象**：请求里的 `model` 仍是 A；费用按 A 计价；`/provider` 换家后把旧家的模型名发给新家 → 404/400「模型不存在」。手册 §模型切换策略 第 214 行「用户手动 /model xxx 指定 — 强制使用指定模型」对应的 `SetOverride` 从未被调用。
- **根因**：路由器在 `engine.New` 里用启动配置构造一次，之后没有任何更新路径。
- **修法**：`ReloadProvider`（或 `SetModel`）里调用 `e.modelRouter.SetModels(model, e.config.ModelFast)`；`/model` 手动指定时用 `SetOverride`，`/model auto` 用 `ClearOverride`。
- **测试**：无（`cli/cove/main_test.go` 用 stub reloader，不检查请求；`internal/engine/live_setters_test.go` 不涉及模型）。我写的临时测试因 engine 测试包编不过未能运行，结论由上述代码链路确认。

#### C2. git 常规操作组放行 `git checkout <路径>`，直接丢弃工作区修改
- **位置**：`internal/permission/routine.go:97-104`（只把含 `.` 或 `\` 的位置参数视为路径）。
- **触发条件**：用户对某次 git 写操作答过 `[a]`/`[p]`（组规则 `command_group: git`）后，模型执行 `git checkout internal/repl`、`git checkout src/main`、`git checkout Makefile`、`git checkout *`、`git checkout main src/utils`。已用临时测试验证以上全部 `lineCovered == true`。
- **现象**：不再询问，直接运行；对目录/无扩展名文件是恢复到 HEAD，等于删掉未提交改动；`*` 由 shell 展开为全部文件。手册 §授权提示 承诺「`checkout <路径>`（参数含 `.` 或 `\` 视为路径）」，但含 `/` 的目录路径和无点文件名不在启发式内。
- **根因**：用「有没有点」猜路径 vs 分支。
- **修法**：`checkout` 的位置参数若在 cwd 下 `os.Stat` 存在、或含 `/`、`*`、`?`、`[`，或位置参数多于 1 个，即不算常规；更稳的做法是把 `checkout` 整体移出组（推荐模型用 `switch`）。
- **测试**：`routine_test.go:121-157` 只覆盖 `-- .`、`-f`、`-B`、含点路径；无目录/通配符用例。

#### C3. git 常规操作组对强推、缩写选项、长选项覆盖不全
- **位置**：`routine.go:32-50`（拒绝表为精确字符串）、`:76-80`（`optionName` 精确匹配）、`:89-96`（push 只拒 `:branch`）。
- **触发条件 / 现象**（均已临时测试验证为放行）：
  - `git push origin +main` / `git push origin +feature:main` —— `+` refspec 即强推，未拒。
  - `git push --force-w origin main`、`git tag --del v1.0`、`git switch --disc main`、`git checkout --forc main` —— git 接受长选项唯一前缀缩写，拒绝表精确匹配不到。
  - `git branch --delete main` —— 只拒了 `-D`/`-M`/`-C` 短写与 `--force`，`--delete`/`--move`/`--copy` 长写法未列入 `gitRefusedLong["branch"]`。
  - `git push --receive-pack=<cmd> <本地路径>`、`git fetch/pull --upload-pack=<cmd>` —— 对本地/文件远端会执行任意程序。
  - 裸 `git commit`（无 `-m`/`-F`）—— 与 `-e` 同样打开编辑器挂死非交互 shell，未拒。
- **修法**：refspec 以 `+` 开头即拒；长选项改为「按前缀匹配拒绝表」（`strings.HasPrefix(refused, name)` 且 `len(name) >= 4`），或改为白名单；补 `branch: --delete/--move/--copy`、`*: --receive-pack/--upload-pack/--exec`；`commit` 无 `-m/-F/-C/--message/--file/--no-edit` 视为交互。
- **测试**：`routine_test.go:145` 只有 `--force-with-lease` 全称。

#### C4. 任务运行中，主循环上的内置命令直接改引擎状态（无任何锁）
- **位置**：`cli/cove/repl_session_commands.go:56-58`（`/compact` → `eng.Compact` 重写 `e.messages`）、`repl_history.go:78,578,765`（`/history N`、`/resume` → `ResumeSession/LoadMessages`）、`internal/command/commands_session.go`（`/cd` → `SetWorkingDir`：替换 cpMgr/verifyGate/sessionNotes/projCtx、`loadPersistedPolicies` 增删规则）、`repl_config_commands.go:29-58`（`/model`/`/provider` → `ReloadProvider`）、`repl_loop.go:241-247`（`exit` 等 3 秒后直接 `autoSaveSession`）。任务 goroutine 同时在 `engine.go:1214,1276,1312,1514,1732,2438` 写 `e.messages`。slash 命令不走 `SubmitWithFeedback`，所以 steer 不会拦住它们。`grep IsRunning` 全 cli 只有 `repl_loop.go` 的 `/continue`/`继续`/`/stop` 有守卫。
- **触发条件**：任务运行中输入 `/compact`（最常见：看到「上下文超出」提示后手动压缩）、`/cd`、`/history 3`、`/model x`；或任务卡在授权/工具执行时输入 `exit`（`WaitIdleUntil` 3s 超时后保存会话）。
- **现象**：`e.messages` 并发读写（数据竞争；slice header 撕裂可 panic）；`/compact` 把一批工具调用切成一半 → 下次请求 `tool_result` 无对应 `tool_use` → 400；`/cd` 途中换掉 `e.cpMgr`/`e.perm` 规则；`exit` 保存到一半的历史。
- **修法**：所有会改引擎状态的命令加 `tasks.IsRunning()` 守卫并提示「任务运行中不可执行」（与 `/continue` 一致）；或把它们排进任务队列串行执行。
- **测试**：无。

### Important

#### I1. 预输入的下一条指令会被当成授权 / 上限 / 提问的答案而丢弃
- **位置**：`repl_loop.go:205-211`（先 `TakePermInputCh`，再判空、再分发）、`permission_prompt.go:92-95, 211-221`（「anything else denies」）、`limit_prompt.go:36-59, 96-103`、`question_prompt.go:38-48`。
- **触发**：任务运行、用户正在钉住行打下一条指令；模型此刻发起需授权的工具调用（`SetPermInputCh` 早于回车被处理）。
- **现象**：用户的整行被当成答案 → 不是 y/a/p → 拒绝该工具；那行文字消失，没有任何提示说它被消费了；模型收到「user rejected」。同理 `exit`/`/stop`/任何 slash 命令在提示期间输入都等于「拒绝」且命令不执行。
- **修法**：relay 只接受 `permissionAnswerDecision`/`limitAnswerDecision` 认得的答案（含 `n`/`s`），其余行重新显示选项并把该行交回正常分发（或 steer）。
- **测试**：`permission_prompt_test.go` 只测答案映射，无「非答案行」用例。

#### I2. 授权提示下空回车 = 静默拒绝
- **位置**：`repl_loop.go:205-217`（perm 路由在 `input == ""` 判断之前）、`permission_prompt.go:211-221`。
- **触发**：授权框出现时按一下回车（用户今天已表现出「任务中空回车」习惯）。
- **现象**：工具被拒，屏幕只回显一行空提示符；选项行 `[y] 允许 [a] … [n] 拒绝` 没写「回车=拒绝」，也没有「已拒绝」反馈行（拒绝结果只作为工具错误进入模型上下文）。
- **修法**：空行忽略并重画选项；拒绝时打印一行「已拒绝 <tool>」。

#### I3. 小窗口（≤24K）本地模型：压缩触发点低于固定开销，进入持续摘要
- **位置**：`internal/api/model_context.go:233-243`（`room = window − window/4 − 8000`，8000 是与窗口无关的常数）、`engine.go:2381-2385`、`compressor.go:70-75, 98-100`。
- **数字**（临时测试实测）：窗口 16384 → `CompactionTrigger = 4288`，8192 → 2000（floor）；而每次请求的固定开销 ≈ 工具定义 3.3K（25 个工具，用 cove 自己的估算器）+ 基础提示词 ~0.8K + `StaticContextBudget`（16K 窗口 4177、32K 窗口 8160）≈ 8–12K。
- **触发**：`context_window: 16384`，或 E2008 处置器从 llama.cpp 报错学到 16384（`remedy.go:47-59`）。
- **现象**：`NeedsCompression` 恒真；消息数一到 12 就摘要（多一次模型调用），历史缩到「摘要 + 最近 6 条」，之后每约 3 个工具回合重复一次；用户看到反复的「已压缩上下文」并且模型不断失忆。处置器「学到真实窗口」反而把情况变差。默认 32K 猜测值也只留 ~4K 给对话历史。
- **修法**：用 `total − requestOverhead` 与触发点比较；安全余量改为 `min(8000, window/8)`；当 `requestOverhead >= trigger` 时给出明确提示（工具太多/窗口太小）而不是循环摘要。
- **测试**：`model_context_test.go` 无小窗口用例；`compact_n1_test.go` 不覆盖「开销 ≥ 触发点」。

#### I4. 上下文超长恢复：第一次报错不上报、不学习窗口；压缩目标与真实窗口无关
- **位置**：`engine.go:1389-1404`（首次超长直接 `compact(ctx, totalTokens/2)` 后 `continue`，不经 `diagnostic.ReportError`）、`:1411`（只有重试再失败才上报）、`remedy.go:47-59`。
- **触发**：llama.cpp `-c 16384`，cove 按名字猜 32K，历史 25K 时报 `exceeds the available context size`。
- **现象**：压到 12.5K 仍超 → 第二次失败才记录 E2008、处置器才把窗口改成 16384 → 本轮以错误结束，用户必须 `/continue`。手册 §自动处置 说「配合超长即压缩重试，/continue 即可续跑」，实际是必然先失败一次。
- **修法**：第一次超长先 `ReportError`（让处置器学到窗口），再按 `CompactionTrigger(routedModel)` 为目标压缩。
- **测试**：`context_length_retry_test.go` 两个用例都没有断言处置/学习顺序。

#### I5. stall 监控在三种正常情形下报「可能卡住」并写 E5007
- **位置**：`internal/engine/activity.go:13`（固定 30s）、`:73-116`；`engine.go:1361-1378`（只有流式事件调 `progressActivity`）、`:1891-1892, 2005`（只在授权提示期间 `pauseActivity`）。
- **触发 / 现象**：
  1. 本地模型 CPU 处理 13K prompt 首 token >30s → 每 30s 一行「仍在「call model」阶段，已 Ns 无进展（可能卡住）」+ errors.log 一条 E5007。
  2. `-p`/headless：`RunMessageWithStream(ctx, msg, nil, nil)` 走非流式 `TryChat`，整轮无 progress → 任何 >30s 的回复都被记为 E5007（界面无输出，但 `/diagnose errors` 会显示一堆「模型调用无进展」）。
  3. `question` 工具等待用户回答：tool activity 未 pause → 每 30s「仍在「run tool question」阶段…（可能卡住，按 Ctrl+C 可中断）」——用户正被提问却被告知卡住。
- **修法**：`AskUser` 期间 pause 该 tool activity；非流式调用视为进行中（或用 provider 首字节）；阈值按 provider（本地/自定义 base_url 放宽或只在第二次提醒）。
- **测试**：`diagnostic_wiring_test.go` 只测上报路径。

#### I6. `max_tokens` 截断续写循环没有上限；推理模型会原地重发同一请求
- **位置**：`engine.go:1447-1453`。
- **触发**：思考型模型（deepseek-reasoner、本地 Qwen3 thinking、llama.cpp `-n` 较小）推理耗尽 `MaxTokens`（`deepseek-reasoner` 为 16000、未知本地模型 8000）→ `finish_reason = length` 且 `Content == ""`、无 ThinkingBlocks。
- **现象**：只追加一条 `[system: your previous response was truncated…]` 就 `continue`，请求内容与上次几乎相同 → 再次截断 → 直到 200 次迭代上限；每次都是满额输出计费；交互模式最终弹「达到单轮上限」，用户看不到任何正文。
- **修法**：连续截断计数 ≥3 即中断并说明「回复被输出上限截断，请调大 max_tokens/窗口」；空内容截断直接不重试。
- **测试**：无。

#### I7. 任务失败后「回收」的 steer 抢跑，吃掉 `/continue` 的恢复点
- **位置**：`repl_tasks.go:377-400`（先打印 `/continue` 提示，再 `reclaimSteerLocked` 把指引放队首并 `startNextLocked`）；`engine.go:1198-1214`（新请求到来时 `e.interrupted = nil` 并写「上一轮任务被中断」标记）。
- **触发**：任务 A 运行中输入一句指引（如「别改测试文件」）→ A 因 API 错误/超长失败。
- **现象**：屏幕先出现「输入 /continue 可从中断处继续刚才的任务」，紧接着「[已排队] 刚插入的指引将作为新任务执行」，指引「别改测试文件」作为一条**独立请求**立刻开跑（模型看不到语境）；随后 `/continue` 回答「没有可继续的回合」；输入「继续」会把 A 当作**全新请求**从头执行（`sameRequest` 对不上已清空的 `interrupted`）。
- **修法**：`pendingFailedMsg != nil` 时不回收为新任务，改为保留 pending steer（下次 `/continue` 一并送入），或把指引拼进 `pendingFailedMsg`。
- **测试**：无。

#### I8. `/stop` 文案与行为不一致
- **位置**：`repl_loop.go:377-386`（立刻打印「[已取消] 当前任务已终止」，但 `CancelRunning` 只是 cancel ctx）、`:255-257, 366`。
- **现象**：紧接着输入 `/continue` 或「继续」得到「当前有任务正在运行，请等待其结束后再重试」；Ctrl+C 路径文案是准确的「等待任务停止...」。
- **修法**：`/stop` 也用「正在停止…」，或 `WaitIdleUntil` 短暂等待后再宣布已终止。

#### I9. `-p`/headless 完全没有接引擎输出；处置器提示写进 stdout
- **位置**：`cli/cove/headless.go`、`main.go`（无 `OnEngineOutput`/`SetOutput` 赋值，仅 REPL 的 `chat_interaction.go:69` 设）；`engine.go:2916-2924`（未接线即静默）；`diagnostic_runtime.go:19-21`（`Notify` → `termui.PrintAbove`），cli 中没有任何 `termui.SetWriter` 调用，headless/-p 下 termui 直写 `os.Stdout`。
- **现象**：`-p`/headless 下「上下文超出模型上限，已压缩…」「! blocked: …」「verify_gate rejected」「费用已达预算的 80%」「本轮后续改用 <模型>」「仍在…阶段」全部消失；而 E2008/E4009 处置器的「⚙ 已按服务端返回的 N token 调整…」却混进 stdout —— 与手册 §启动参数「只有最终答案写到 stdout，提示、警告和错误都写到 stderr」相反。
- **修法**：headless/-p 在启动时 `eng.OnEngineOutput = func(l){ fmt.Fprintln(os.Stderr, render.StripControls(l)) }`，并 `termui.SetWriter(os.Stderr)`（答案由 `outln` 单独走 stdout）。
- **测试**：`headless` 测试不断言这些行。

#### I10. 钉住输入行时窗口缩放，流光标落入保留行（未实测，代码推断）
- **位置**：`internal/repl/pinned.go:310-335`（`h != pinRows` 时只重设滚动区并清旧保留行，`ESC7…ESC8` 仍把光标还回缩放前的行号）；没有任何地方在流输出时检测尺寸变化，只有按键/计数更新才 `drawPinnedLocked`。
- **触发**：Windows Terminal 中任务流式输出时把窗口拉矮几行。
- **现象**：终端缩放后原光标行可能 ≥ h−1，`ESC8` 把流光标放回保留行 → 后续流文本盖在分隔线/输入行上，滚动区不含该行故不滚动，直到本轮结束 `unpin`。
- **修法**：尺寸变化时重新 `queryCursorPos` 并走 `pinSequence`（把光标压回滚动区末行），而不是只改边距；或流输出每 N 行检查一次 `GetSize`。
- **测试**：`pinned_test.go:151-155` 只覆盖固定尺寸。

### Minor

- **M1** `readline.go:353-366, 444-455`：Ctrl+C / Ctrl+D 在 `eraseLineLocked` 之后于**锁外** `fmt.Print("\r\n")`；流式进行中 `eraseLineLocked` 是空操作，这个 CRLF 直接插进模型输出流，且可能与并发 `StreamPrint` 交错。
- **M2** `repl_tasks.go:79-119`：`canMergeQueuedTask` 用子串包含判相似，`mergeQueuedTask` 再次 `Contains` 则原样返回 —— 实测队列里有「run pytest and fix the failures」时输入 `y`/`run` 被「合并」并**丢弃**，反馈却说「已补充」。steer 语义后只剩附件/插件命令/继续路径会入队，影响缩小，但逻辑仍在。
- **M3** `repl_tasks.go:403-409`：`finishLocked` 置 `cancel = nil` 却不调用 `cancel()`，每个任务泄漏一个 context（`go vet lostcancel` 因存在字段中而不报）。
- **M4** `repl_loop.go:115-131` + `permission_prompt.go:97-108`：cooked 模式下（内置命令执行中）Ctrl+C 走 `taskSigCh` 只 cancel 任务、不调 `denyPendingPermissionPrompt`；授权提示不看 ctx，继续等 15 分钟，下一行输入被吃掉——正是 `:591-598` 注释描述的 bug 在另一条路径上的残留。
- **M5** `main.go:237-267`：`withInterrupt` 同时 `Notify(SIGINT, SIGTERM)`：内置命令运行期间外部 `kill` 被吞；Ctrl+C 想中止慢命令（如 `/compact`）时 `taskSigCh` 也把后台任务一起取消。
- **M6** `readline.go:506-530`：`⚡`（U+26A1，EAW=Wide）按 1 列计，`PromptRunning()` 宽度少 1；`:828` 的 `maxVis = w − promptWidth − 1` 与 `:841` 的 hint 余量、`pinned.go:328` 的注释位置都可能触到最后一列产生软换行/鬼影行（未实测）。
- **M7** `fallback.go:186-195`：单供应商连续 3 次上下文超长（HTTP 400）→ `ProviderUnavailable` → E2009「供应商已被标记不可用」，把用户侧问题记成供应商故障；`classify.go:83-85` 还会让后续同类错误优先归到 E2009 而非 E2008。
- **M8** `recorder.go:157-158`：`LoadRuntimeLog` 单行缓冲 1MB，超长行使 scanner 停止，其后事件静默丢失（无 `sc.Err()` 处理）。
- **M9** `internal/tool/mcp_tool.go:59-61`：`t.pool == nil` 判断被 typed-nil `*mcp.Pool` 绕过 → `registerToolsWith(nil, …)` 直接 panic（生产总是 `mcp.NewPool()`，只影响测试/工具复用）。
- **M10** `diagnostic_runtime.go:27-29`：`context_window` 只作用于 `model`，`model_fast` 为本地模型时无法配置。
- **M11** `chat_interaction.go:19-35, 107-116`：REPL 层把含 "timeout"/"deadline exceeded"/"eof" 的错误重试 3 次（重发同一消息触发引擎续跑），与手册 §请求重试「请求超时后不再重试（可能已计费）」矛盾；退避 `time.Sleep` 不可取消，Ctrl+C 最多等 3.6s。
- **M12** `repl_loop.go:475`：REPL 用 `pc.APIKey == ""` 拦截，headless 用 `runNeedsAPIKey(key, replaying)`；`--replay` 交互模式无 key 时不能发消息，两条路径口径不一。
- **M13** `repl_loop.go:155-169`：授权提示期间 Ctrl+C = 拒绝 **并** 取消整个任务，选项行只写了 `[n] 拒绝`，未说明 Ctrl+C 会终止任务。
- **M14** 手册 §任务合并（第 852 行）仍描述「排队任务与新输入相似即合并」，steer 语义后普通输入不再入队，段落已过时。

---

## 3. 建议补的测试

| 目标分支 | 建议用例 |
|---------|---------|
| `Engine.ReloadProvider` → 请求模型 | mock provider 记录 `req.Model`；`/model B` 后断言请求用 B；`/provider` 切换后断言模型名随之变化。 |
| `permission/routine.go` | `git checkout <目录>`、`git checkout *`、`git checkout main <path>`、`git push origin +main`、`--force-w`/`--del`/`--disc`/`--forc` 缩写、`git branch --delete`、`--receive-pack=`、裸 `git commit` 均应 **不**在组内。 |
| `repl_loop` 分发 | 任务运行中输入 `/compact`、`/cd`、`/history 1`、`/model` 应被拒绝或排队；用 `-race` 跑一个「任务写消息 + 主循环 /compact」的用例。 |
| 授权 relay | 非答案行不应被当成拒绝并丢失；空回车重新显示选项；`exit` 在提示期间的行为。 |
| `api.CompactionTrigger` / `checkAndCompress` | 窗口 8192/16384/24000 时 `trigger > requestOverhead`；开销 ≥ 触发点时不应每轮摘要。 |
| 上下文超长恢复 | 第一次超长即 `ReportError`（处置器学到窗口）；压缩目标 ≤ `CompactionTrigger(learned)`。 |
| stall 监控 | `question` 工具等待期间不报 stall；非流式调用 >30s 不报；本地 provider 阈值。 |
| 截断续写 | `finish_reason=length` 且空内容连续 3 次应中断并给出说明，而不是跑到迭代上限。 |
| steer 回收 | 任务失败时 pending steer 不得启动新任务；`/continue` 仍可恢复；`/tasks` 显示待生效指引。 |
| `/stop` | 取消后立即 `/continue` 的文案一致性。 |
| headless/-p 输出 | `OnEngineOutput` 行进入 stderr；`diagnostic.Notify` 不进 stdout。 |
| pinned 缩放 | `drawPinnedLocked` 在 `h` 变小时输出的序列应把流光标放回滚动区内（对 `pinSequence` 做纯函数测试）。 |
| `canMergeQueuedTask` | 短输入（≤3 字符 / 单词）不得因子串包含被合并丢弃。 |
| `recorder.LoadRuntimeLog` | 超 1MB 行后仍能读出后续事件。 |
| `mcpToolProxy.Def` | typed-nil pool 不 panic。 |

---

## 4. 有代码无接线

方法：以 `docs/USER_MANUAL.md` 目录与配置表为清单，对每项 grep 到运行期调用点；对 `internal/{engine,diagnostic,repl,tool,permission,memory,dream,hooks,skills,plugin,mcp,api,termui,uiout}` 导出符号做「非测试引用数 ≤ 声明数（去注释）」扫描后逐条人工核对。今日已确认的三例（Steer、matchKnownCode、AutoFixable）状态：`Steer` 已于 14:39 由 `repl_tasks.go:265-275` 接入（本次审计中途完成）；`errors.go` 已无 `AutoFixable` 字段和标题子串匹配（14:23 版本）。

### 4.1 手册功能 / 配置键 → 运行期调用点

| 功能 / 符号 | 文档位置 | 代码位置 | 结论 | 接线成本 |
|---|---|---|---|---|
| `/model xxx` 强制使用指定模型 | USER_MANUAL §自动切换规则 L214；§REPL 命令 `/model` | `api/router.go:113-137` `SetModels/SetOverride/ClearOverride`；`engine.go:620-636` | **未接线**（见 C1；路由器永不更新） | 小 |
| 引擎输出 Sink `Engine.SetOutput` / `internal/uiout`（`NewWriter/NewFuncs/NewCapture/Activities`） | 无（engine.go:144-158 注释自述「neither calls SetOutput yet」） | `engine.go:2950-2960`，`internal/uiout/uiout.go` | **未接线**（仅测试） | 中 |
| headless / `-p` 的引擎提示行 | §启动参数「提示、警告和错误都写到 stderr」 | `headless.go`、`main.go` 无 `OnEngineOutput`/`SetOutput`；`termui.SetWriter` 全 cli 零调用 | **未接线**（见 I9） | 小 |
| `Engine.OnToolStart` 回调 | 无 | `engine.go:174-175`，被 `:1611,1659,1678,1703` 调用 | 从未赋值 | 小（或删） |
| `verbose` 配置 | §配置字段说明「详细输出；可在 profile 中单独设置」 | 只在 `repl_config_commands.go` 的 profile show/save 读取；无任何行为分支 | **配置无行为** | 中（定义语义） |
| `context_window` 对 `model_fast` | §配置字段说明 | `diagnostic_runtime.go:27-29` 只设 `cfg.Model` | 部分接线 | 小 |
| `/new` 命令 | `diagnostic/errors.go:216` E6001 建议「使用 /new 开始新会话」 | 命令注册表无 `new`、无 `clear` | **仅文案** | 小 |
| `/clear` 命令 | §记忆提取 L1117「压缩、`/clear` 使历史变短」 | 无注册 | **仅文档** | 小 |
| 「渐进式多轮色彩还原：重绘最近 4 轮」 | §会话恢复 L1399-1405 | `repl_history.go` 只找到「已自动恢复最近有效任务 #N」一行，未找到重绘最近 N 轮的实现 | 仅文档（**待核实**：未通读全文件） | 中 |
| 「历史记录智能降噪：丢弃通用命令类标题」 | §会话恢复 L1396-1397 | `engine.go:2680-2691 pickSessionTitle` 只跳过 synthetic 消息 | 仅文档（**待核实** session store 侧） | 小 |
| 「本地文件热发现与 mtime 动态防抖缓存…无感通知大模型刷新」 | §高级技巧 10 L1684-1688 | `internal/repomap/enhanced.go`、`notes.go` 仅用 `ModTime` 做缓存失效；`engine/filetouch.go` 只是 Layer-3 路径记录；没有监听线程、没有「通知模型」 | 仅文档（夸大） | 大 |
| 「双搜索引擎核查屏障 / 并联激活 / RAG 清洗」 | §高级技巧 11 | `tool/extra_tools.go` 按配置选一个 provider（tavily/brave/duckduckgo） | 仅文档（夸大，**待核实**是否有多引擎合并） | 大 |
| 「任务合并」 | §后台任务 L852-854 | `repl_tasks.go:298-308` 仅当运行中且队列非空（附件/插件/继续路径）才合并 | 语义已变，文档过时 | 小 |
| E2008 处置「配合超长即压缩重试，/continue 即可续跑」 | §自动处置 L1528 | `engine.go:1389-1411` 首次超长不上报 | 部分接线（见 I4） | 小 |
| `IterationLimitPrompt`、`OnBackgroundSummary`、`OnTurnModel`、`OnSteerConsumed`、`PermissionPrompt`、`Runtime.AskUser` | 手册各节 | `limit_prompt.go:27`、`chat_interaction.go:523`、`:70`、`repl_tasks.go:183`、`permission_prompt.go:33`、`question_prompt.go` | 已接线（REPL）；headless/-p 均未接（按设计拒绝/停止） | — |
| hooks（`BeforeTool/AfterTool/SessionStart/SessionEnd`） | §Hooks | `app_bootstrap.go:77-…` `hooks.LoadConfigDir`，`engine.go:528,1898,2040`，`session_end.go:95` | 已接线 | — |
| `done_check` / `done_verify_auto` / `done_verify_timeout_seconds` | §配置 | `app_bootstrap.go:146-149` 经 `cfg.DoneCheckMode()`/`VerifyAutoEnabled()` 传入 | 已接线 | — |
| `experimental_tools`、`web_search`、`memory_embedding`、`show_reasoning`、`disabled_skills`、`max_*`、`subagent_max_iterations`、`thinking`/`effort` | §配置 | `registry.go:30-33`、`app_bootstrap.go:102-166` | 已接线 | — |
| `thinking_tokens` | §配置「已不再生效」 | 只在展示/profile 读取 | 与文档一致（无行为） | — |
| `lsp` / `cron` 工具 | §工具注册条件 L71「已移除」 | `tool.NewLSPTool/NewCronTool` 仍在，仅测试引用 | 与文档一致；死代码 | 小（删） |

### 4.2 导出符号在非测试代码里零调用（已去除注释干扰，人工核对）

| 符号 | 位置 | 结论 |
|---|---|---|
| `(*Engine).IterCount` | `engine/engine.go:1112` | 零调用（连测试也没有） |
| `(*Engine).SessionNotes` | `engine/engine.go:2895` | 零调用 |
| `(*Engine).HasInterruptedTurn` | `engine/turn.go:56` | 只在测试里用（`/continue` 用 `InterruptedTurn`） |
| `(*Engine).PersistPermissionRule`（单数） | `engine/engine.go:797` | 只在测试里用；生产走复数版 |
| `(*Engine).SetProvider` | `engine/engine.go:770` | 只在测试里用（注释已说明） |
| `(*ModelRouter).SetModels/SetOverride/ClearOverride` | `api/router.go:113-137` | 零调用 → 导致 C1 |
| `(*Classifier).Explain`、`ShouldAutoApprove`、`AutoApproveLine`、`IsReadOnlyLine`（非 `…For` 版） | `permission/classifier.go:136-203, 556-577` | `Explain` 零调用；其余只在测试里用，已被 `…For(kind)` 版本取代 |
| `CommandPrefixes`（旧入口） | `permission/command_prefix.go:30` | 只在测试里用（生产用 `ShellRememberRules`） |
| `(*PolicyEngine).SetStorage/ListRules/RemoveRule/AddRule` | `permission/policy.go:144-235` | 零/仅测试调用；`policy.go:126-128` 声称的「规则变更持久化」路径未接（实际持久化走 `persist.go AppendAllowRules`） |
| `AppendAllowRule`（单数） | `permission/persist.go` | 只在测试里用 |
| `(*Store).AddDir`、`TruncateEntry`、`PromptFor`、`RelevantMemoriesFor` | `memory/store.go` | `AddDir/TruncateEntry` 零调用；`PromptFor/RelevantMemoriesFor` 仅测试（每轮检索实际走 `Search`，`engine/session_memory.go:185`） |
| `(*Manager).AddDirectory` | `skills/skills.go` | 零调用 |
| `(*Manager).EnabledCommands/EnabledTools`、`(*Marketplace).AddSource/RemoveSource/InstallFromGit` | `plugin/plugin.go`、`plugin/marketplace.go:474` | 零调用（`/plugin install` 走 `Manager.Install/MarketplaceInstall`；「从 git URL 安装」「管理源」无入口） |
| `dream.LastFinishedTask`、`RecordConsolidation` | `dream/task.go`、`dream/lock.go` | 零/仅测试调用 |
| `hooks.LoadUserConfig` | `hooks/config.go` | 只在测试里用（生产用 `LoadConfigDir`） |
| `diagnostic.Lookup`、`HasProblems` | `diagnostic/errors.go:227`、`checker.go` | 仅测试 |
| `diagnostic.Context.Attempt` | `diagnostic/report.go:16` | 字段从未被赋值 |
| `mcp.ParseToolName` | `mcp/client.go` | 仅测试 |
| `repl.HasActiveInput`、`repl.SectionHeader` | `repl/readline.go:280`、`repl/color.go:277` | 零调用 |
| `tool.IsPlanMode`、`tool.NewCronTool`、`tool.NewLSPTool`、`tool.NewWebSearchTool` | `tool/tool.go`、`tool/advanced_tools_collab.go`、`tool/extra_tools.go` | 零/仅测试调用（websearch 走带设置的构造器） |
| `api.ClearModelContextWindows`、`diagnostic.ResetForTest` | — | 测试辅助，合理 |

### 4.3 实现了但默认关闭且文档未说 / 说法不符

| 功能 | 现状 |
|---|---|
| 引擎 Sink（`uiout`）与 `Activity` 状态行 | 整套实现存在，两个前端都未启用；手册无描述。 |
| `-p`/headless 的引擎提示 | 实现存在（`engineOutput`），前端未接 → 事实上关闭；手册反而声称写 stderr。 |
| `verbose` | 可配置、可存 profile，无任何效果；手册写「详细输出」。 |
| 路由器 `Override` | 实现存在，`/model` 未调用；手册写「强制使用指定模型」。 |
| `PolicyEngine` 的 `SetStorage` 持久化 | 实现存在，未接；真实持久化是另一条路径，`policy.go:126-128` 注释误导。 |
| 插件「从 git URL 安装 / 源管理」 | `Marketplace.InstallFromGit/AddSource/RemoveSource` 实现存在，无命令入口；手册 §插件 只写 `/plugin install`。 |
