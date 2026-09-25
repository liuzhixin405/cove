# cove 第四轮：任务循环借鉴项实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把《开源 Agent 任务循环调研》中建议的 10 条借鉴项落地，让弱模型（DeepSeek）在 cove 里"说停就真做完、停下有交代、卡住有出路"。

**Architecture:** 两批并行。J 批改引擎循环（`internal/engine/engine.go`、`turn.go`、`turn_limits.go`、`loopdetect.go`、`verify_gate.go`）与 CLI 的循环提示；K 批改遮蔽器去重（`internal/engine/masker.go` 独立文件）与子代理结构化结果（`internal/delegate`、`internal/plan`、`internal/tool` 的 agent 工具）。K 不碰 engine.go。文档最后统一更新。

**Tech Stack:** Go 1.25；本地 git 仓库；门禁 build/vet/gofmt/test/golangci-lint。

**Spec:** `docs/superpowers/research/2026-09-25-agent-loop-survey.md` 第三节（借鉴项 1–10）。

## Global Constraints

- 不新增第三方依赖；不执行 git 写操作（控制者提交）；每项 TDD；改动前把原文件备份到 `%SCRATCHPAD%/orig/<相对路径>`（已存在则跳过）。
- 所有新增的"注入提示"（nudge）都必须有每轮次数上限，且上限值集中放在 `internal/engine/nudges.go` 的常量表里，便于文档引用。
- 注入给模型的合成消息统一用现有 `newSyntheticUserMsg`，文案英文（与现有 `[system: …]` 风格一致），用户可见的终端文案中文。
- 新增的模型调用（收尾总结、目标自检）必须走 `e.fallback` 并计费，且在 `-p`/预算耗尽/ctx 取消时跳过。
- golangci-lint 0 issues；文档只在任务 L 统一更新，各批写 `%SCRATCHPAD%/doc-notes-<批>.md`（工作区 `.superpowers/sdd/2026-09-25-round4-loop-borrowing/`）。

## Review Focus

1. 收尾总结调用不得携带工具定义，也不得再触发工具执行或循环检测。→ J2 测试。
2. "宣告下一步却停下"检测对以问句结尾、或明确说"已完成"的回复不得误触发。→ J1 测试。
3. 目标自检每轮最多一次，且模型回答"已满足"时不得再进入验证门禁重跑。→ J3 测试。
4. 结果去重存根不得替换本轮刚产生的结果，只替换更早的重复项。→ K1 测试。
5. 中断标记只写一条，`/continue` 续跑后不得重复累积。→ J5 测试。

---

## 第一波（并行）

### Task J1: 停后自检——宣告未做、退化结尾、空回复

**Files:** Create `internal/engine/nudges.go`（常量：`maxContinuationNudgesPerTurn = 2`、`maxEmptyResponseRetries = 2`；函数 `announcedNextStepWithoutAction(content string) bool`、`degenerateEnding(content string, usedToolsThisTurn bool) bool`、`emptyOrThinkOnly(resp *api.ChatResponse) bool`）；Modify `internal/engine/engine.go`（在 `!hasToolCalls(resp)` 分支、验证门禁之前插入检查）；Test `internal/engine/nudges_test.go`、`internal/engine/nudge_loop_test.go`

**Interfaces:** `turnLimits`（turn_limits.go）新增字段 `continuationNudges int`、`emptyRetries int`，随回合重置。

- [ ] 失败测试：
  - `announcedNextStepWithoutAction`：真 → "接下来我将修改 main.go。"、"Let me now update the config."、"Next, I'll run the tests"、"下一步：实现解析函数"（作为最后一段）；假 → "已完成全部修改。"、"需要我继续吗？"、"The task is done."、"你希望用哪种方案？"。
  - `degenerateEnding`：本轮用过工具且最终文本 < 40 字符且不含"完成/done/finished" → 真；未用过工具 → 假。
  - `emptyOrThinkOnly`：Content 为空且只有 ThinkingBlocks/ReasoningContent → 真。
  - 循环测试（假模型）：第一次返回"接下来我将修改 X"无工具 → 引擎注入 nudge 并再次调用模型；连续 3 次仍如此 → 第 3 次接受为最终回复（上限 2）。空回复重试同理。
- [ ] 实现：nudge 文案 `[system: You announced a next step but did not perform it. Continue now by calling the required tools, or state clearly that the task is complete.]`；退化结尾 `[system: Your last message is too brief to be a final answer after doing work. Summarize what you changed and what remains, or continue.]`；空回复 `[system: Your response was empty. Provide the answer or call a tool.]`。
- [ ] 门禁：`go test ./internal/engine/ -run 'Nudge|Announced|Degenerate|Empty'`

### Task J2: 停止或到硬上限时的无工具收尾总结

**Files:** Modify `internal/engine/turn_limits.go`（新增 `wrapUpSummary(ctx, reason) (string, error)`：用当前 routed 模型、`Tools: nil`、`MaxTokens: 1024`，提示 `[system: The run is stopping (<reason>). Without calling tools, summarize in the user's language: what was completed, what remains, and the recommended next step.]`）；Modify `internal/engine/engine.go`（`askLimit` 返回 Stop、`-p` 到硬上限、guardrail 硬停、循环检测 Fatal 四处调用；结果作为 assistant 消息追加进历史并通过 `onDelta`/engineOutput 打印；失败或 ctx 取消时静默跳过）；Test `internal/engine/wrapup_test.go`

- [ ] 失败测试：假模型记录请求 → 收尾请求 `Tools` 为空、`MaxTokens<=1024`；上限 Stop 后历史最后一条是收尾总结；预算耗尽时不调用；`-p` 硬上限时输出含总结且退出错误仍为 LimitError。
- [ ] 门禁同上。

### Task J3: 完成前目标自检一次（弱模型）

**Files:** Modify `internal/engine/nudges.go`（常量 `doneCheckOncePerTurn`）、`internal/config/config.go`（`done_check: "auto"|"on"|"off"`，默认 `auto` = 仅当 routed 模型为 fast/mid 档或 provider 非 anthropic 时启用；档位判断复用 `isFastModelName` 与 `api/model_context.go` 已有信息）、`internal/engine/engine.go`（`!hasToolCalls` 分支：本轮改过文件且未自检过 → 注入 `[system: Before finishing, check whether the user's request has been fully met. If anything remains, continue working now; if everything is done, reply with the final answer.]`，置 `limits.doneChecked=true`，continue）；Test `internal/engine/done_check_test.go`

- [ ] 失败测试：改过文件 + auto + fast 模型 → 注入一次；第二次无工具回复直接接受；未改文件 → 不注入；`off` → 不注入；`on` + anthropic 主模型 → 注入。
- [ ] 与 J1 的顺序：先 J1（空/宣告/退化），再 J3 自检，再验证门禁。

### Task J4: 验证门禁跳过本轮已通过的命令

**Files:** Modify `internal/engine/verify_gate.go`（`Run` 增加参数 `alreadyPassed func(cmd string) bool`）、`internal/engine/engine.go`（从本轮 tool 消息里找 bash/powershell 命令规范化后等于门禁命令且结果不含 `[exit code:`/`Error` 的记录）；Test `internal/engine/verify_skip_test.go`

- [ ] 失败测试：本轮工具历史含 `go build ./...` 成功 → 门禁不再执行该命令（用计数假执行器）；含失败记录 → 仍执行；命令带多余空格/`&&` 组合 → 只匹配单命令精确等价。

### Task J5: 中断标记写入历史；循环检测第二次询问；预算 80% 提醒；权限拒绝文案

**Files:** Modify `internal/engine/turn.go`（`interrupt` 追加一条合成用户消息 `[system: The previous turn was interrupted by the user (<reason>). Commands may have partially executed and files may be half-edited; re-check state before repeating work.]`，同一中断只写一条，`/continue` 续跑成功后下次中断再写）；Modify `internal/engine/engine.go`（循环检测：非 Fatal 的第 2 次命中即通过 `IterationLimitPrompt` 询问，Reason 为 `LimitReasonLoop`，选项文案由 CLI 渲染为 `[c] 本轮禁用循环检测并继续 / [s] 停止`，Continue → `loopDetector.DisableForTurn()`；`-p` 保持现有行为）；Modify `internal/engine/loopdetect.go`（`DisableForTurn()`、`ResetTurn()`）；Modify `internal/engine/turn_limits.go`（迭代或时间窗口用到 80% 时，向最新一条 tool 消息追加一次 `[budget: about N model calls / M minutes remain in this window. Prioritize converging on a result; do not stop solely because of this notice.]`，每窗口一次）；Modify `internal/engine/engine.go:2010/2051`（拒绝文案改为 `Error: permission denied for <tool> (<reason>). Do not call this tool again with the same input; explain the situation to the user or choose a different approach.`）；Modify `cli/cove/limit_prompt.go`（渲染 `LimitReasonLoop` 的选项文案）；Test `internal/engine/interrupt_marker_test.go`、`loop_prompt_test.go`、`budget_notice_test.go`、`cli/cove/limit_prompt_test.go`

- [ ] 失败测试：中断两次只写一条标记，续跑后再中断写第二条；循环第 2 次命中触发回调且 Reason==Loop，Continue 后本轮不再检测；80% 提醒只出现一次且附在 tool 消息末尾；拒绝文案含 "Do not call this tool again"。

### Task K1: 相同工具结果去重为存根

**Files:** Modify `internal/engine/masker.go`（`Mask` 之前新增 `dedupeRepeatedResults(history)`: 对角色为 tool、长度 ≥512 字符、内容 sha256 相同的第 2 次及以后出现项（且不在最近 `protectRecentResults=4` 条 tool 消息内），替换为 `[identical to an earlier tool result (#<index>, <n> bytes); content omitted]`；不改变消息数量与 tool_call_id）；Test `internal/engine/masker_dedupe_test.go`

- [ ] 失败测试：三次 `cat` 同一大文件 → 第 2、3 次被替换，第 1 次保留；最近 4 条内的重复不替换；<512 字符不替换；`MaskingResult.TokensSaved` 计入。

### Task K2: 子代理结果结构化

**Files:** Modify `internal/delegate/delegate.go`（`Result` 增加 `ExitReason string`（`completed|max_iterations|interrupted|error|loop`）、`Truncated bool`；`CapReached` 保留为兼容别名并在 `ExitReason=="max_iterations"` 时为 true）、`internal/plan/plan.go`（`FormatResult` 按 ExitReason 分类，如"3 个任务完成，1 个到达上限（含部分结果），1 个失败"）、`internal/tool/advanced_tools_agent_skill.go`（agent 工具输出首行 `[exit: <reason>, steps: N, truncated: yes/no]`）；Test 对应包

- [ ] 失败测试：四种退出原因各一；`FormatResult` 汇总文案；agent 工具首行格式。

---

## 第二波

### Task L: 文档

`docs/USER_MANUAL.md`（新增"完成判定与收尾"一节：三类 nudge 及次数、收尾总结、目标自检与 `done_check`、验证门禁跳过、循环第 2 次询问、80% 提醒、中断标记；子代理结果字段）、`CHANGELOG.md`（新增 `## [11.3.0] - 2026-09-25`）、`README.md` 若有配置表则补 `done_check`。验收：`go test ./cli/cove/ -run 'Docs|Help|Consistency'` 通过。

---

## 追加：Task M — dream 改为"对话结束即整理"，降低后台功能门槛

**用户诉求：** 没有人一天 24 小时干活；dream 应在一次对话结束时就跑，而不是等 24/12 小时和 N 个会话。其他后台功能同理，门槛不能高到成鸡肋。

**Files:** Modify `internal/dream/config.go`（新增 `trigger: "session_end"|"threshold"`，默认 `session_end`；`min_turns` 默认 2）、`internal/dream/dream.go`、Create `internal/dream/worker.go`（分离进程工作者）、Modify `cli/cove/main.go`（隐藏参数 `--dream-worker <sessions-dir>`：进入工作者模式，只做一次整理后退出，日志写 `~/.cove/dream.log`）、`cli/cove/session_end.go`（退出路径：满足条件则启动工作者）、`internal/dream/status.go`（`Status()` 增加 `Trigger`、`LastWorkerStartedAt`、`LastWorkerResult`）、Test 对应包。**不改 internal/engine/engine.go**（J 批在改）；若 `runTurnEndPipeline` 需要在 session_end 模式下跳过每回合的 `ExecuteAutoDream`，用 `dream.SuppressAuto` 的现有机制或在 `ExecuteAutoDream` 内按 trigger 早退。

**行为：**
- `session_end` 模式：每回合的自动检查不再触发整理（`ExecuteAutoDream` 按 trigger 早退）。进程退出时（交互 `/exit`、Ctrl+D、`-p` 结束、headless 结束）若本会话助手回合数 ≥ `min_turns` 且距上次整理后有新会话（含本会话），则**以分离子进程**启动 `cove --dream-worker`：Windows 用 `CREATE_NEW_PROCESS_GROUP|DETACHED_PROCESS`（`syscall.SysProcAttr.CreationFlags`），Unix 用 `Setsid`；父进程打印一行"已在后台启动记忆整理（约 1–3 分钟，结果见 /dream）"后立即退出。工作者内部：`TryAcquireConsolidationLock` → 复用 `runDream` → 写 `LastRun*` 到 `~/.cove/dream-last.json` → 退出；失败回滚锁。
- 启动子进程失败时回退为内联执行，时间预算 60 秒，超时取消并回滚锁，终端显示进度点。
- `threshold` 模式保持现有 12h/3 门槛。
- `/dream` 显示当前触发模式与上次工作者结果；`/dream run` 不变。
- 同时审计其他后台功能的门槛并写入 doc-notes-M（不改 engine.go）：记忆提取 2 分钟节流是否合理；`backgroundReview`（技能自动创建）的触发条件；`AutoPrune` 10 分钟节流。给出建议值，能改的（非 engine.go 文件）直接改。

- [ ] 失败测试：`session_end` 模式下 `ExecuteAutoDream` 不启动任务；退出路径在回合数 ≥2 时调用工作者启动函数（注入 spawn 函数记录参数），回合数 1 时不调用；工作者模式在假 provider 下完成一次整理并写 last-run 文件；spawn 失败时内联路径在 60 秒预算内返回（注入短预算）。

---

## 追加：Task N — 后台功能门槛清单落地（用户授权直接实施）

来源：`.superpowers/sdd/2026-09-25-round4-loop-borrowing/inventory-report.md`（功能门槛清单）。两个子批并行，文件集互不相交；N2 产出函数，N1 负责 engine.go 接线。均在 J、M 完成后开始。

### Task N1（引擎侧）

**Files:** `internal/engine/engine.go`、`turn.go`、`review.go`、`compressor.go`、`context_budget.go`、`internal/notes/**`、`cli/cove/repl_session_commands.go`、`cli/cove/repl_config_commands.go`、`cli/cove/repl_help.go`、`internal/engine/task_decomposition_prompt.go`、`internal/guardrail/**`、`internal/context/subdir_hints.go`

1. **本会话记忆即时生效**：`extractRunner` 每次成功提取后把新条目登记到 `e.newMemories`；下一轮构造用户消息时以 turn note 追加"本会话新学到的记忆：…"（≤2KB），不重建系统提示词。压缩后 `injectedSkills`、subdir hints 的 `seen` 一并重置。
2. **backgroundReview 合并**：删除 MEMORY 分支（与 extraction 重复）；SKILL 分支写盘到 `~/.cove/skills/auto-<slug>/SKILL.md`（含 description、生成时间、来源会话 ID），并在回合结束摘要行提示"新增技能 X"；`Save` 错误 `log.Warnf`。
3. **`/compact` 真压缩**：阈值传 0 强制第二层摘要；消息门槛从 12 降到 4；打印"压缩前 X tokens → 压缩后 Y tokens"；不满足条件时如实说明。
4. **压缩摘要保留原始需求**：首条非合成用户消息保留 2000 字符，其余 user 600、assistant 250、tool 100；压缩完成时终端提示一行。
5. **verify gate 覆盖更多生态**（`turn.go` 的 `detectVerifyCommands`）：`*.sln`/`*.csproj` → `dotnet build --nologo -v q`；`package.json` 且有 build 脚本 → `npm run build --if-present`；`pyproject.toml`/`setup.py` → `python -m compileall -q .`；启动时打印"完成校验命令：…"（有则显示）。
6. **checkpoint 可见性**：回合末尾本轮有 write/edit 且检查点创建成功 → 摘要行加"已建检查点，/undo 可回退"。
7. **session notes 重做**：去掉 `File: x` 与工具错误条目；决策/发现正则加中文（用/改用/采用/决定/发现/原因是）；按内容去重；存储路径改为 `~/.cove/projects/<hash>/session_notes.md`（旧位置存在则迁移并删除项目内 `.cove/` 下的文件，目录为空则删）；加载时保留原时间戳。
8. **预算预警**：费用达到 `max_budget_usd` 的 80% 时提示一行（一次）；`/budget <n>` 只改本会话，新增 `/budget save` 写配置；`/budget off` 取消上限。
9. **guardrail 按工具名分开 30 秒熔断**（`guardrail/guardrail.go`）。
10. **任务分解提示按字符数**（`utf8.RuneCountInString` ≥300）。
11. **子目录 AGENTS.md 提示**：单文件上限 12KB；grep/glob 的 `path` 参数也触发；压缩后重置 `seen`。
12. `/help` 移除 `/history clean`（保留命令本身）。

### Task N2（存储、工具、路由侧）

**Files:** `internal/memory/**`、`internal/extract/**`（M 完成后）、`internal/context/context.go`、`internal/checkpoint/**`、`internal/api/router.go`、`internal/tool/**`（question、browser、execute_plan、mcp_tool、websearch）、`internal/mcp/**`、`internal/config/config.go`（新增键）、`internal/session/store.go`、`cli/cove/registry.go`、`cli/cove/app_bootstrap.go`、`internal/command/**`（/hooks、/dream 费用展示）

1. **根目录 AGENTS.md 等指令文件加载**：从 cwd 向上到 git 根，按 `CLAUDE.md`、`.claude/CLAUDE.md`、`AGENTS.md`、`.cove.md` 顺序加载，去重，合并注入（同一预算）；截断时提示一行。
2. **记忆注入 top-K**：全量阈值 8KB → 24KB；超过时用现有 `Search`（BM25）按当前用户消息取 top-8 全文 + 其余索引；导出 `Store.PromptFor(query string) string` 供 N1 接线（N1 在构造用户消息处调用）。
3. **extraction 追加截断方向**：超过 10KB 时滚动到 `name-2.md`（不丢新内容）；写入统一经过 `Store.Save` 的总量检查，总量上限 100KB → 300KB。
4. **记忆按项目分目录**：`~/.cove/projects/<hash>/memory` + 全局 `~/.cove/memory`；加载时两者合并（项目优先）；`/memory` 显示来源；旧全局目录不迁移。
5. **checkpoint 保留策略**：每项目保留 50 条，`Create` 后超出即删最旧 ref；每 20 次创建跑一次 `git gc --prune=now --quiet`（后台）。
6. **模型路由**：长度按字符数；阈值 0.40 → 0.35；导出 `RoutedModelLabel()` 供 CLI 在回合开始的状态行显示"模型：xxx"（仅当 fast≠main）。
7. **工具面精简**：非交互模式不注册 `question`；chromedp 未编译时不注册 `browser`；`execute_plan` 接上 `max_agents`（1–8）；`task_*`、`team_*`、`send_message`、`brief`、`sleep` 归入 `experimental_tools`（默认 false，配置开启才注册）。
8. **websearch 配置**：`web_search.provider`（tavily|brave|duckduckgo）与 `api_key`，环境变量仍可用；`/diagnose` 提示未配置。
9. **MCP**：首次断线自动重连一次（间隔 2s）；工具 schema 超长时先去掉 `description` 字段再判断，不在 JSON 中间截断；处理 `notifications/tools/list_changed` 刷新工具列表。
10. **session 清理按项目计数**（每项目 200）。
11. **masker 落盘目录清理**：`~/.cove/tool-outputs` 超过 7 天的文件在启动时删除。
12. **`/hooks` 命令**列出已加载的钩子；`/dream` 显示上次整理的 token 与费用。
13. **`/init`**：改为让模型读仓库后生成 CLAUDE.md（复用 engine 一次 `-p` 式调用，走验证：生成后展示 diff 让用户确认）；已有 AGENTS.md 时提示"已检测到 AGENTS.md，将同时加载"而非"缺少"。

### Task O — 文档
在 Task L 的基础上覆盖 N1/N2 全部变化；CHANGELOG 11.3.0。

---

## 追加：Task Q — repo_map 移出系统提示词，聊天回合轻量化

**用户诉求：** 纯聊天也发 1.2 万+ token 不合理。实测本仓库系统提示词 51.7KB，其中 repo_map 38.6KB（74%）。

**Files:** `internal/engine/engine.go`（系统提示词构建处 ~1000–1050：去掉 `<repo_map>` 全量层，改为 ≤4KB 的"项目轮廓"层）、`internal/repomap/**`（新增 `Outline(root) string`：顶层目录、语言统计、入口文件、构建/测试命令；新增 `Query(root, terms []string, budget int) string`：按路径/符号关键词筛选子图）、`internal/tool/repomap_tool.go`（新工具 `repo_map`，参数 `query`（可空）与 `path`，只读，并发安全，输出 ≤12KB）、`internal/engine/turn.go`（turn note：首个"像任务"的回合自动注入 `Query` 结果 ≤12KB；判定 `looksLikeTask(msg)`：含文件路径/函数名/反引号代码/关键词（修复|实现|重构|报错|测试|fix|implement|refactor|error|bug|test）或长度 ≥200 字符；每会话只自动注入一次，之后靠工具）、`cli/cove/registry.go`（注册 `repo_map`）、`internal/engine/context_budget.go`（记忆索引上限 4KB）、Test 对应包。

- [ ] 失败测试：系统提示词不含 `<repo_map>`，含 `<project_outline>` 且 ≤4KB；`looksLikeTask("你好")==false`、`looksLikeTask("修复 internal/engine/engine.go 里的 panic")==true`；首个任务回合 turn note 含 `<repo_map_excerpt>` 且 ≤12KB，第二个任务回合不再自动注入；`repo_map` 工具按 query 返回相关文件；聊天回合的系统提示词字节数比改前减少 ≥60%（用本仓库快照断言 < 15KB）。
- [ ] 文档：USER_MANUAL「上下文与提示词」一节说明轮廓层、`repo_map` 工具与自动注入规则；CHANGELOG 11.3.0 追加。
