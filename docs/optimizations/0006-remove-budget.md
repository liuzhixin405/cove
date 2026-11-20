# 0006 去掉预算：只统计和显示 token 与费用

- 状态：已完成
- 日期：2026-11-20（创建）；2026-11-20（完成）
- 关联：[0005](0005-unified-oneshot-model-calls-and-billing.md)（费用计算统一）

## 背景

用户要求："砍掉设置金额、超过金额不让使用的功能，展示 token 和费用是没问题，但是不去做限制。"用户此前也说过"费用不用考虑"。

## 现状与问题

**P1 拦截已经没有了，但"预算"这个概念还完整地留在代码里。**
- 主流程里，预算只会提醒：`engine/agent.rs` 的 `budget_message` 生成"[预算提醒]"和"[预算超限] …任务继续"两种通知，`budget_notice` 负责发出去，没有任何调用点会因此返回错误。`/doctor` 的错误码说明也写着"新版预算仅作费用提醒，不会因超额停止"。
- 子代理里有一个按预算退出的分支（`agents/subagent.rs` 的 `Exit::Budget`），触发条件是收到包含"预算"字样的 `Error::Limit`。现在只有测试代码会构造这种错误，属于旧版本留下的代码。
- 用户在输入历史里输过十几次 `/config budget off`，应该是旧版本还会按预算停止任务时留下的。

**P2 预算概念分散在 30 个文件里**：
- 设置：命令行 `--budget`，配置项 `max_budget_usd`（以及它在项目配置信任规则里的分类），运行时的 `/config budget auto|off|<金额>|save`；
- 会话：`Accounting.renewed_budget`（`BudgetSpend`）、`budget_cost()`、`reset_budget_spend()`，也就是"从设置预算那一刻起重新计数"的预算周期；
- 提醒：`budget_message`、`budget_notice`、`print_budget_notice`，以及控制台给这两种通知上色的逻辑；
- 子代理：会话账本里的 `remaining_budget`，后台子代理恢复或发消息时带上的剩余预算，`Exit::Budget`；
- 竞速、自动化、`cove -p` 子进程：任务描述里的 `budget`/`budget_usd`/`total_budget_usd`，"预算平分给两个候选"，子进程的 `--budget` 参数；
- 提示文字："预算提醒可能低估实际费用"等；
- 测试：76 处引用，十几个测试专门检验预算行为。

## 目标

1. 不再有任何"预算"：不能设置，不提醒，不按预算拦截或退出。
2. token 和费用的统计与显示保持不变：状态行的用量行、`/status`、`/usage` 账本报告、会话的按模型分账。
3. 删干净，不留废弃的兼容项（见长期记忆"删功能不留空壳兼容"）。

## 范围

- 做：删除上面 P2 列出的全部内容；与费用准确性有关、但措辞挂在预算上的提示，比如"模型价格未知，缺失费用按零计入"，保留成"费用可能低估"的说法。
- 不做：
  - 不改费用计算（0005 刚统一过）；
  - 不改"按模型路由时，未知价格的快速模型按主模型价格计费"这条规则。它原来的注释写的是"为了让预算仍是上限"，改成"费用宁可高估、不低估"。

## 调研与参考

- 核查用户数据：`~/.cove/config.json` 的根级没有 `max_budget_usd`，**但 4 个 profile（bedrock 500、deepseek 10、gemini 10、local 500）都写了**（创建文档时只查了根级，实施时发现，已更正）；各项目的 `.cove.json` 里没有；`~/.cove/automations/*/state.json` 里没有预算字段，也没有竞速任务文件。
- 删除字段后，配置里残留的 `max_budget_usd` 会在启动时报"未知字段"，所以要同时清理用户配置（先备份）。
- 会话的 `Accounting` 没有设置 `deny_unknown_fields`，旧会话文件里的 `renewed_budget` 会被忽略，可以正常读取。

## 方案

1. 删除会话层的预算周期：`BudgetSpend`、`renewed_budget`、`budget_cost`、`reset_budget_spend`，以及 `Accounting::add_response` 和 `add_spend` 里更新它的代码。
2. 删除设置层的预算：`Settings.budget`、`Limits.budget`、`--budget`、`max_budget_usd`，以及它的信任分类。
3. 删除提醒：`budget_message`、`budget_notice` 及其全部调用点、`print_budget_notice`、控制台上色。
4. 删除 `/config budget` 子命令、帮助文字和补全。
5. 删除子代理的剩余预算传递和 `Exit::Budget`；竞速、自动化、`cove -p` 子进程的预算字段和参数。
6. 文档：`reference.md`、`README.md` 里与预算相关的段落；`/doctor` 错误码说明里"旧版预算限制"那一条。
7. 测试：专门检验预算行为的测试删除；只是顺带设置了预算的测试，去掉预算参数后保留。

## 影响范围

- 文件：`src/session/mod.rs`、`src/config/mod.rs`、`src/config/trust.rs`、`src/engine/agent.rs`、`src/commands/model.rs`、`src/commands/project.rs`、`src/commands/agents.rs`、`src/commands/sessions.rs`、`src/agents/subagent.rs`、`src/agents/background.rs`、`src/agents/plan.rs`、`src/agents/collab.rs`、`src/workflows/race.rs`、`src/workflows/automation.rs`、`src/workflows/worker.rs`、`src/model/routing.rs`、`src/memory/extract.rs`、`src/memory/dream.rs`、`src/app/ui.rs`、`src/app/completion.rs`、`src/app/diagnostics.rs`、`src/app/mod.rs`，以及相关测试和文档。
- 行为变化：`--budget`、`/config budget` 不再存在（命令行会报未知参数，`/config budget` 会提示用法）；配置里如果写了 `max_budget_usd`，会提示"未知字段"；不再出现"[预算提醒]""[预算超限]"。
- 数据：会话文件不再写入 `renewed_budget`；旧文件里的这个字段会被忽略。

## 验证

- `rg -i "budget|预算" src` 只剩与预算无关的用法（上下文 token 预算 `context_budget` 等）。
- `cargo test` 全部通过；删掉的测试逐一列在验证结果里。
- 只对改动过的文件，通过标准输入运行 `rustfmt`。

## 实施计划

- [x] 会话层、设置层、提醒
- [x] 命令、子代理、竞速、自动化、子进程
- [x] 文档
- [x] 测试、格式化，回填验证结果

## 待决策

无。

## 决策记录

- 2026-11-20：去掉预算，只保留 token 和费用的统计与显示。（用户）
- 2026-11-20：把预算概念整个删掉，而不是只关掉提醒：拦截早就不存在了，剩下的都是不再起作用的设置和代码；按"删功能不留空壳兼容"的原则处理。（实施方）
- 2026-11-20：用户配置里 4 个 profile 的 `max_budget_usd` 直接删除，原文件备份到 `~/.cove/backups/0006-remove-budget/config.json`；不在代码里保留"忽略旧字段"的兼容。（实施方）
- 2026-11-20：顺带删掉的同类残留：模型路由打分里的 `budget_left`（调用方一直传 `None`，从未生效）、`/doctor` 错误码 E5004"旧版预算限制"、子代理 `Exit::Budget`。（实施方）
- 2026-11-20：启动时和切换模型时"价格未知，预算提醒可能低估"的警告，启动时那条删除（没有预算就没有"低估"的问题，费用会直接显示为未知）；切换模型时改成"新模型价格未知；费用统计可能不完整"。（实施方）
- 2026-11-20：`~/.cove/backups/0006-*` 下的config.json 的备份已删除。（用户："一起删了"）

## 验证结果

- `rg -i "budget|预算" src` 只剩上下文预算（`context_budget`、"上下文预算必须在 …"）、快照耗时预算、工具输出截断预算等无关用法。
- 删除的测试（专门检验预算行为）：
  - 单元测试 2 个：`unknown_price_keeps_budget_and_warns`、`reported_cost_provider_keeps_configured_budget_without_static_prices`；另删去 `exits_map_to_lifecycle_states` 里 `Exit::Budget` 那条断言，`budget_raise_needs_trust_and_bad_files_warn` 去掉预算部分后改名 `bad_project_files_warn`。
  - 集成测试 9 个：`plan_runs_after_parent_exceeds_budget`、`background_spend_warns_without_blocking`、`steering_with_budget_continues_when_spent`、`budget_threshold_warns_and_allows_write`、`missing_usage_counts_as_zero_when_budgeted`、`legacy_unknown_cost_resumes_with_a_new_budget_counter`、`renewed_budget_counts_future_spend_without_claiming_historical_cost`、`renewed_budget_counts_missing_usage_as_zero`、`renewed_budget_counts_unknown_subagent_spend_as_zero`。
- 改写后保留的测试：去掉 `--budget`/`limits.budget`/"[预算超限]"断言；改名的有 `steering_with_known_prices_does_not_wait_for_usage`、`reported_cost_is_used_without_static_prices`、`failed_request_can_continue_and_keeps_its_cost`（原来用 `/config budget` 看已花费，改用 `/status usage` 的"总花费"）、`init_uses_model_after_a_task`、`classifier_weighs_keywords_length_files_and_failures`、`reported_cost_provider_tracks_cost_without_prices`；补全测试的样例从 `/config budget` 换成 `/config effort`。
- `cargo test` 全部通过：单元测试 374 个（另有 4 个忽略），集成测试 215 个。
- 只对改动过的文件，通过标准输入运行了 rustfmt；`cargo build --release` 完成。
- 用真实的 `~/.cove/config.json` 启动 release 版本：没有任何配置警告；`--budget 5` 报"unexpected argument"。

## 遗留

- 0005 遗留里提到的 `budget_notice`、`budget_message`、`Extractor`/`Dreamer` 的 `_budget` 参数，已在本次全部删除。
- 旧会话文件里的 `renewed_budget` 字段读取时被忽略，下次保存时自然消失，不需要迁移。
