# cove 第三轮：遗留项清零 + 迭代上限改为询问

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把第二轮台账里全部遗留项做完；单轮 200 次模型调用的硬上限改为"到点暂停询问"，并补齐后台功能的可见性。

**Architecture:** 三批。F 权限遗留（permission / safety / shell / cli 权限提示）与 G 后台可见性（dream / memory / session / diagnostic / command / render / tool 小修）第一波并行，二者都不改 `internal/engine/engine.go`；E 引擎循环第二波单独进行，负责迭代上限、时间上限、停滞询问、`/continue`，并把 G 提供的状态函数接进回合结束流程。文档最后统一更新。

**Tech Stack:** Go 1.25；本地 git 仓库（基线 3117c9b）；门禁 `go build ./... && go vet ./... && go test ./...` + golangci-lint 0 issues。

**Spec:** 用户指令："未处理的一起修了吧，特别是 200 次上限，最好改成询问而不是 200 次后直接结束。" 遗留项清单见 `.superpowers/sdd/2026-09-25-optimization-round2/progress.md` 的 Minor（deferred）与"残留（记录）"行。

## Global Constraints

- 不新增第三方依赖；不执行 git 写操作（控制者提交）；每任务 TDD；改动前把原文件备份到 `%SCRATCHPAD%/orig/<相对路径>`（已存在则跳过）。
- 中文 Windows 为主环境，涉及 shell 的改动要考虑 Git Bash / PowerShell 5.1 / cmd。
- 用户可见文案：中文提示，英文 `Error:` 前缀。
- golangci-lint（.golangci.yml 现有配置）必须保持 0 issues。
- 文档只在任务 H 统一更新；各批把要点写到 `%SCRATCHPAD%/doc-notes-<批>.md`（工作区 `.superpowers/sdd/2026-09-25-round3-leftovers/`）。

## Review Focus

1. 交互模式到达迭代上限时，用户选"继续"后必须从中断处续跑而不是重发消息或丢失已完成的工具结果。→ E1 测试。
2. `-p` 模式没有提示处理器，到达上限必须以错误退出，退出码非 0，且错误里写明 `--max-turns`。→ E1 测试。
3. deny 规则对 `git -C . push`、`/usr/bin/git push`、`command git push` 必须命中；allow 前缀规则对这些写法仍**不**放行。→ F2 测试。
4. cmd 作为 shell 时 default 模式不得自动放行任何命令。→ F4 测试。
5. 回合结束的后台摘要行不得在 `-p` 模式或非 TTY 下输出到 stdout（会污染管道输出）。→ E4 测试。

---

## 第一波

### Task F1: `[p]` 的会话副本随 `/cd` 一起换掉

**Files:** Modify `cli/cove/permission_prompt.go`、`internal/engine/engine.go`（仅 `PersistPermissionRules` 与 `diskRules` 登记，E 批不动这两处）、Test `cli/cove/permission_prompt_test.go`、`internal/engine/permission_persist_test.go`

- [ ] 失败测试：`[p]` 应答后 `SetWorkingDir` 到另一项目，同一命令重新询问。
- [ ] 实现：`PersistPermissionRules` 成功后把规则登记进 `e.diskRules`（decision=allow）并注入 `perm`，CLI 不再额外 `AddPermissionRule` 会话副本；持久化失败时退化为会话规则并提示"未能写入，仅本次会话有效"。

### Task F2: deny / ask 前缀匹配做归一化

**Files:** Modify `internal/permission/command_prefix.go`（`anyCommandHasPrefix` 改为 `anyCommandHasPrefixNormalized`）、Test `internal/permission/command_prefix_test.go`

- [ ] 失败测试：deny `git push` 命中 `git -C . push`、`/usr/bin/git push`、`git.exe push`、`command git push`、`sudo git push`、`env X=1 git push`、`echo a && git push`；不命中 `git pushx`、`gitk push`。allow 规则对上述变体仍不放行（现有 `commandCovered` 不变）。
- [ ] 实现：对每条简单命令，先用 `safety` 的 `commandWords` 逻辑剥掉 runner 与 `VAR=value`，程序名去目录与 `.exe`、小写；git/docker/kubectl 等子命令工具跳过前置全局选项（`-C dir`、`-c k=v`、`--no-pager`、`-P`、`--git-dir=…`、`--work-tree=…`）再取子命令。仅用于 deny/ask 匹配。

### Task F3: ask 规则优先于 allow；PolicyLoadError 接入 `/doctor` 与 `/cd`

**Files:** Modify `internal/permission/permission.go`（`Check` 中 ask 列表移到 allow 之前，bypass 分支之后）、`internal/diagnostic/checker.go`（新增"权限规则文件"检查项，读取 engine 暴露的 `PolicyLoadError`，需通过一个可注入函数变量 `diagnostic.PolicyLoadErrorFn`）、`cli/cove/*`（`/cd` 命令执行后若 `PolicyLoadError()!=nil` 打一行警告；`/doctor` 输出含该项）、Test 对应包

- [ ] 失败测试：ask `git status` + 会话 allow `git status` → 仍询问；bypass 下 ask 不生效；`/doctor` 文本含"权限规则文件损坏"。

### Task F4: cmd 回退时不自动放行；git 环境加固；PathInside 跟随符号链接

**Files:** Modify `internal/permission/classifier.go`（`ShellKind==ShellCmd` 时 `IsReadOnlyLineFor` 恒 false）、`internal/shell/shell.go`（`Env` 追加 `GIT_PAGER=cat`、`GIT_CONFIG_COUNT=1`、`GIT_CONFIG_KEY_0=core.fsmonitor`、`GIT_CONFIG_VALUE_0=false`；若环境已有 `GIT_CONFIG_COUNT` 则在其后追加序号）、`internal/permission/project.go`（`PathInside` 对存在的路径先 `filepath.EvalSymlinks`）、Test 对应包

- [ ] 失败测试：cmd 下 `dir` 不为只读放行；`Env` 含上述四项且不破坏已有 `GIT_CONFIG_COUNT`；Windows 上用 junction（`mklink /J`，创建失败则 t.Skip）指向项目外目录时 `PathInside` 为 false。

### Task F5: 拆分过长文件与重复分词

**Files:** Create `internal/safety/heredoc.go`（把 parseShell 的 heredoc 逻辑迁出）、`internal/permission/classifier_git.go`、`classifier_pkg.go`（按工具族拆出）；Modify `internal/permission/classifier.go`（`IsReadOnlyLineFor` / `ClassifyLineFor` 只分词一次，内部共享 `[]SimpleCommand`）。行为不变：现有测试全绿即为验收；新增一个基准测试记录分词次数（用计数钩子）。

### Task G1: 后台功能状态查询与手动命令

**Files:** Modify `internal/dream/dream.go`（新增 `func (r *Runner) Status() Status`：Enabled、LastConsolidatedAt、HoursSinceLast、SessionsSinceLast、MinHours、MinSessions、Running、LastRunFilesTouched、LastRunErr；新增 `func (r *Runner) RunNow(ctx) error` 忽略时间与会话门槛、仍需锁）、`internal/dream/config.go`（默认 `MinHours` 12、`MinSessions` 3）、`internal/memory/store.go`（`Stats()` 补 `LastExtractedAt`、`LastExtractedCount`；新增 `RecordExtraction(n int)`）、`internal/command/`（新增 `/dream`：无参数显示 Status，`run` 立即执行；`/memory`：`list` 列出条目摘要，`stats` 显示统计）、`cli/cove/registry*.go` 注册命令、Test 对应包

**Interfaces（供 E 批消费）:** `dream.Status` 结构体与 `Runner.Status()`；`memory.Stats{Count, TotalBytes, LastExtractedAt, LastExtractedCount}`；`extract.Runner` 在完成一次提取后调用 `memStore.RecordExtraction(n)`。

- [ ] 失败测试：`Status()` 在无历史时 `HoursSinceLast` 为 +Inf 或 -1（选一，文档写明）且 `SessionsSinceLast` 正确计数；`/dream` 输出含"距上次整理"与"还差 N 个会话"；`/memory list` 输出条目数与 `Stats().Count` 一致。

### Task G2: `/doctor` 展示后台状态；`-p` 模式同步收尾；dream 会话 ID 跟随恢复

**Files:** Modify `internal/diagnostic/checker.go`（`Run` 报告新增"后台学习"段：dream 状态、记忆统计、上次提取时间，通过可注入函数变量获取，避免依赖 engine）、`cli/cove/headless.go`（`-p` 结束前若启用自学习，等待记忆提取最多 20 秒后再退出；dream 在 `-p` 下跳过并在 debug 日志说明）、`internal/dream/dream.go`（`SetCurrentSession(id string)`）、Test

**Interfaces（供 E 批消费）:** `dream.Runner.SetCurrentSession(id)`；`engine` 恢复会话时调用它（E 批负责调用点）。`headless` 等待用的是 engine 暴露的 `WaitBackground(ctx) `（E 批实现；G 批先以接口 `interface{ WaitBackground(context.Context) }` 断言并在 nil 时跳过）。

### Task G3: 会话自动清理、SessionEnd hook、元数据刷新、指纹前提测试

**Files:** Modify `internal/config/config.go`（`max_sessions` 默认 200）、`internal/session/store.go`（`Prune` 由 `Save` 每第 N 次调用触发，或提供 `AutoPrune(keep, protect)` 由 E 批在回合结束调用——选后者；首行元数据在标题/费用变化时随下一次追加一并重写首行：实现为"追加后若元数据脏则整写"）、`internal/hooks/hooks.go`（`SessionEnd` 事件可 Fire；`cli/cove` 在退出路径触发）、Test：`store_jsonl_test.go` 新增"消息字段原地修改不会被指纹察觉"的前提固定测试（断言当前行为并注明）

### Task G4: 工具与渲染小修

**Files:** Modify `internal/tool/edit.go`（`spaceUnit` 改为扫描整个文件推断缩进单位，块内不一致才回退）、`internal/tool/grep.go`（rg 路径保留 `... N more matches` 计数）、`internal/tool/stream_exec.go`（`OnProgress` 分块经 `decodeShellOutput` 后再回调；GBK 溢出裁剪按行边界）、`cli/cove/chat_interaction.go`（`beginAttempt`/`stop` 先 `switchToLocked(outText)`）、`.golangci.yml`（去掉重复启用的 `unused`）、`build.bat`（版本行只取引号内内容）、Test 对应包

---

## 第二波（F、G 完成后）

### Task E1: 迭代上限改为软上限 + 询问；`-p` 可配置硬上限

**Files:** Modify `internal/engine/engine.go`（`MaxIterations` 常量改为 `Config.MaxIterations`，默认 200；新增回调 `IterationLimitPrompt func(stats LimitStats) LimitDecision`，`LimitStats{Iterations, Elapsed, Cost, RecentSteps []string, Reason string}`，`LimitDecision` 为 `Continue|Stop`；到达上限时若回调非 nil 则询问，Continue 则把上限加一个窗口继续循环，Stop 或回调为 nil 则按现有路径中断；中断错误文案改为"已达到单轮最大迭代次数 N（可用 --max-turns 或配置 max_iterations 调整）"）、`internal/config/config.go`（`max_iterations`）、`cli/cove/cli_args.go`（`--max-turns N`，`0` 表示不限制；仅 `-p` 有效）、`cli/cove/permission_prompt.go` 旁新建 `cli/cove/limit_prompt.go`（渲染：已调用模型 N 次、用时、费用、最近 5 步工具名；`[c] 继续 N 次  [s] 停止`；复用 `repl.SetPermInputCh` 机制与 15 分钟超时，超时=停止）、Test `internal/engine/iteration_limit_test.go`、`cli/cove/limit_prompt_test.go`

- [ ] 失败测试：上限 3、假工具每轮都调用 → 回调被调用一次且 `Iterations==3`；回调返回 Continue → 再跑 3 次后再次询问；返回 Stop → 中断且已完成的工具结果保留在 `e.messages`，重发同一消息续跑；回调 nil → 错误信息含 `--max-turns`；`--max-turns 0` → 不中断（用 20 次上限的假模型自行结束验证）。

### Task E2: 单轮时间上限与停滞询问走同一条路径

**Files:** Modify `internal/engine/engine.go`（`Config.MaxTurnMinutes` 默认 60，0 关闭；每次迭代检查 `time.Since(turnStart)`，超过则以 `Reason="time"` 调用同一回调；L3 停滞检测在交互模式下以 `Reason="stagnation"` 询问一次，之后本轮不再问）、`internal/config/config.go`、Test

### Task E3: 子代理上限可配置并返回部分结果；串行路径 recover；git init 感知

**Files:** Modify `internal/delegate/delegate.go`（`MaxIter` 从 `Config.SubagentMaxIterations` 读，默认 60；到达上限时 `Result.Output` 含已完成步骤摘要与最后一次模型文本，`Error` 说明"已达上限，以下为部分结果"）、`internal/engine/engine.go`（串行内联与 deferred 路径包 `recover`，与 goroutine 路径一致）、`internal/context/context.go`（`RefreshGitAll` 在 `GitRoot==""` 时重新探测 `findGitRoot`）、Test

### Task E4: 回合结束摘要与 G 批接线

**Files:** Modify `internal/engine/engine.go`（`runTurnEndPipeline` 末尾：若 `e.OnBackgroundSummary != nil` 且非 headless，调用它传入 `BackgroundSummary{MemoriesExtracted int, DreamStatus dream.Status, SessionSaved bool}`；恢复会话时调用 `dreamRunner.SetCurrentSession`；新增 `WaitBackground(ctx)` 等待记忆提取 goroutine；回合结束调用 `store.AutoPrune(cfg.MaxSessions, e.session.ID)`）、`cli/cove/chat_interaction.go`（渲染一行淡色摘要，如"已提取 2 条记忆 · dream 还差 2 个会话"，`-p` 与非 TTY 不输出）、Test

### Task E5: `/continue` 命令

**Files:** Modify `cli/cove/repl_loop.go`（`/continue`：若上一轮因上限/取消中断且存在未完成回合，则重发同一条用户消息触发续跑；否则提示"没有可继续的回合"）；中断提示文案末尾加"输入 /continue 可继续"；Test

### Task H: 文档

**Files:** `docs/USER_MANUAL.md`（迭代/时间上限与询问、`--max-turns`、`/continue`、`/dream`、`/memory`、`/doctor` 新段、ask/deny 规则语义与归一化、cmd 回退不自动放行、`max_sessions`、hooks `SessionEnd`）、`CHANGELOG.md` 11.1.0 追加、`README.md` 命令表。验收：`rg -n "200 次|MaxIterations" docs README.md` 只剩说明默认值的位置。
