# 0029 调查交给快速模型的 explore 子 Agent

- 状态：已完成
- 日期：2026-11-23（创建）；2026-11-24（完成）
- 关联：[0010](0010-isolated-runs.md)（子任务执行）、[0012](0012-main-loop-context-and-tools.md)（上下文长度）、[0013](0013-cache-ttl-and-precompaction.md)（缓存与预压缩）、[0023](0023-real-model-evals.md)（真实模型评测）；0026（话题切换交接上下文，设计中）、[0027](0027-cited-answers.md)（回复里的引用，证据核对复用它的解析）、[0032](0032-stuck-detection.md)（按进展止损，子 Agent 同样适用）

## 背景

待核实的统计：read 和 grep 共 1524 次，全在主上下文里执行；子 Agent 只用了 36 次；同一文件被重复读了 97 次；快速模型只占 7% 的调用。设想是：explore 子 Agent 默认用快速模型，读 5 个以上文件的调查交给它，主上下文只收结论，这样主上下文涨得慢，也能和 0026 叠加。

本文先核实这些数字，再判断这个设想在 Opus 和 DeepSeek 上分别值不值得做、怎么做。

## 现状与问题

**当前版本核对（2026-11-24，实施前）。** 下面 P1–P6 的数字来自 11 月之前的 trace 和会话，之后完成了 0024、0025、0027、0032。开工前按 README 规则 3 对照当时的代码（快照 `%TEMP%\cove-snap\0029\`）逐条确认：

| 草稿里的问题 | 当前版本 | 依据 |
|---|---|---|
| explore 返回 7–17 KB、远超 400 词 | **机制成立，具体字节数是历史现象** | `REPORT_INSTRUCTIONS` 仍只是"约 400 词"的要求；`RunOutcome::render` 原样输出；主循环对 `agent` 结果的唯一限制是 `OutputPolicy::cap` 的 4000 token × 窗口倍数（最多 4 倍），DeepSeek、Opus 的 1M 窗口下是 16000 token（约 48 KB），17 KB 照样全部进入主上下文。7–17 KB 这组数字没有在当前版本重跑，标为历史现象。新测试 `a_long_answer_is_capped_in_the_parent_and_saved_whole` 用 20 KB 的答复复现了"没有硬上限" |
| 同一回复里多个 explore 串行 | **成立** | `WithSubAgent::concurrent` 对 `agent` 一律返回 false（快照 `subagent.rs:1475`），主循环还额外排除了所有 `metered` 工具（`engine/agent.rs` 的 `pending`）。新测试 `explore_runs_on_the_fast_model_in_parallel_and_is_billed_at_its_prices` 用带延迟的模拟模型确认两个 explore 的请求时间重叠 |
| 前台委派的模型固定为 None | **成立** | 快照 `subagent.rs:1361` `model: None` |
| 子 Agent 按主模型的价格记账 | **成立，而且比草稿说的更宽** | 子 Agent 没有 policy，`account` 用 `limits.prices`（快照 `engine/agent.rs:771-777`）。草稿说"配置了 `prices` 时"才算错；实际上没配价格时 `settings.prices` 也会取主模型的表价（`config/mod.rs` 的 `models::prices(&model)`），所以只要主模型在价格表里，任务被路由到快速模型时它的子 Agent（继承快速模型）也按主模型价格记。DeepSeek 评测里大部分任务被路由到 v4-flash，这些任务的子 Agent 一直按 v4-pro 计价 |
| 窗口沿用主模型 | **成立** | 快照 `commands/agents.rs:109` `context_tokens: settings.context_tokens` |
| trace 记配置的模型名 | **成立，当前版本复现** | 快照 `app/diagnostics.rs:493` `"model": self.model`。2026-11-23 的 DeepSeek 评测（`evals/results/20261123-184453-deepseek.json`，保留的 `scope-discipline-1` 现场）里，`trace.jsonl` 每条都写 `deepseek-v4-pro`，同一次运行的用量账本 25 条全是 `deepseek-v4-flash` |
| 系统提示没说什么时候用 explore（P6） | **成立** | 快照 `engine/agent.rs` 系统提示只有 execute_plan 和"小任务不要委派"两句 |
| P1、P2 的统计（去重、调查段的分布） | 历史现象，未在当前版本确认 | 只用来说明背景，不单独拿来定优先级 |

数据来源：`~/.cove/trace.jsonl`（8336 行：model 3921、tool 3909、task 492、compact 14），`~/.cove/usage/*.jsonl`（3972 次计费调用），`~/.cove/sessions/*.json`（122 个会话）。数据几乎都是 Bedrock 上的 Opus 5.5（快速模型配的是 Sonnet 5.5）。分析脚本放在 `%TEMP%\cove0029\`，不进仓库。

**核实结论**

| 说法 | 核实 | 说明 |
|---|---|---|
| read + grep 1524 次，全在主上下文 | 对 | trace：read 915、grep 609；加上 glob 83、repo_map 13，只读工具共 1620 次，占全部工具调用的 41%。trace 只记主 Agent 的工具调用（`TraceHook` 只挂在主 Agent 上），子 Agent 内部的调用不在里面 |
| 子 Agent 只用了 36 次 | 对，但有效的只有 13 次 | 36 次 `agent` 调用里，22 次返回约 140 字节，是后台 Agent 立即返回的 id；前台的 14 次里，13 次给出了结果，1 次因网络错误中断。会话记录里还保留的 5 次全部是 `type=explore`，来自同一个任务 |
| 同一文件重复读 97 次 | 对，但多数不是浪费 | 会话里保留的 436 次 read（更早的已被压缩）中，同一会话再次读已读过的路径 97 次，分布在 16 个会话里，其中一个会话占 43 次。97 次里只有 1 次的 (path, offset, limit) 完全相同；其余是同一文件的不同行段（分页读大文件），或者改完以后重读。38 次发生在同一段连续调查内部，就是分页 |
| 快速模型只占 7% | 对 | usage 账本：Opus 3638 次（91.6%，$2742.94），Sonnet 286 次（7.2%，$9.27）。trace 的 `model` 字段看不出来，见 P5 |

**P1 去重基本没起作用，重复读也不是这里要解决的问题。** `engine/tool_output.rs:179-216` 只把**内容逐字节相同**、512 字节以上、合计能省 2000 token 以上的旧结果折叠成指针。分页读出来的每段内容不同，改后重读的内容也变了，所以都不命中。122 个会话里只找到 1 个去重指针（外置到磁盘的有 37 个）。这一点不影响本文结论：委派减少的是"调查过程留在主上下文里的原始材料"，和重复读是两件事。

**P2 调查确实占了主上下文的大头。** 把 trace 里"一次模型调用加它发起的工具，且工具全是只读的"算一轮，连续的这种轮次算一段调查：

| 段内只读调用数 | 段数 | 覆盖的只读调用 | 这些轮次的输入 token 合计 | 工具结果合计 |
|---|---|---|---|---|
| 全部 | 340 | 1407 | 130.7M（全部模型调用 681.7M 的 19%） | 4.8 MB |
| ≥ 5 | 95 | 907 | 61.4M | 3.43 MB |
| ≥ 8 | 44 | 611 | 32.7M | 2.61 MB |

- 一段调查的中位数是 1 轮、3 次调用，均值 2.1 轮、4.1 次；最长 20 轮、30 次。
- 调查开始时主上下文的中位数已经是 17 万 token。每一轮都把这 17 万重新发一遍（大部分是缓存读），所以输入 token 很大。
- ≥ 8 次调用的段：轮数中位数 5、均值 6.1；耗时中位数 35 秒、均值 51 秒。
- 按会话里的路径统计（177 段）：一段读到的不同文件，中位数 1 个、均值 2.1 个；读 ≥ 5 个文件的只有 16 段（9%），却占了 30% 的只读调用；读 ≥ 8 个文件的 10 段，占 23%。
- 已经并行：只读轮次里一次发 2 个以上调用的占 54%（331 轮单个、221 轮两个、104 轮三个……）。

**P3 现有的 explore 又慢又贵，返回的内容远超要求。** 保留在会话记录里的 4+1 次 explore 来自同一个任务，全部跑在 Sonnet 5.5 上（该任务被路由到了快速模型）：

| 子 Agent | 步数 | 费用 | 返回字节 |
|---|---|---|---|
| agent-1（网络错误中断） | 7 | $0.35 | 221 |
| agent-2 | 9 | $0.78 | 12628 |
| agent-3 | 7 | $0.56 | 10240 |
| agent-4 | 9 | $0.62 | 8101 |
| agent-5（重试 agent-1） | 5 | $0.35 | 5628 |

- trace 里 13 次有效的前台 `agent` 调用，每次 48–263 秒，返回 6.9–17 KB。
- `REPORT_INSTRUCTIONS`（`agents/subagent.rs:61`）要求"约 400 词以内"，但父模型在 prompt 里写了"约 900–1400 字"，子 Agent 照 prompt 来。现在没有硬上限，结果原样进入主上下文（`RunOutcome::render`，`subagent.rs:815`）。
- 同一次回复里的多个前台 `agent` **依次**执行（`WithSubAgent::concurrent` 对 `agent` 一律返回 false，`subagent.rs:1466`）：11-04 那 4 次 explore 各 76–83 秒，串行跑了约 5 分钟。主上下文里自己做，同等规模的调查耗时中位数是 35 秒。

**P4 explore 没有自己的模型，费用也算错。**
- 前台委派时 `RunRequest.model` 固定是 `None`（`subagent.rs:1353`），`ModelRouter::stream` 就用当前任务被路由到的模型（`model/routing.rs:409-414`）。所以主任务用 Opus 时 explore 也用 Opus；只有整个任务被判成简单任务时它才用快速模型。竞速已经会给 B 方案传 `model: Some(fast)`（0010），机制是现成的。
- 子 Agent 的 `Agent` 没有 `ModelPolicy`，费用按 `limits.prices`（主模型的配置价）算（`engine/agent.rs:619-627`）。配置了 `prices` 时，快速模型的调用在会话账里按主模型价格记；usage 账本走的是 `Metered`，按路由器的价格表记（`app/mod.rs:120-123`），所以两本账对不上。
- 子 Agent 的上下文窗口用的是主模型的 `context_tokens`（`commands/agents.rs:107`）。快速模型窗口更小时（例如 Haiku 20 万对 Opus 100 万），子 Agent 会按错误的窗口压缩，或者直接超限。

**P5 trace 记不下实际用的模型。** `Traced` 写的 `model` 是配置里的主模型名（`app/diagnostics.rs:493`），不是 `response.model`，所以 trace 里 3867 条都是 Opus，看不出哪次用了快速模型。验证本文效果时需要按模型分开统计。

**P6 系统提示没有说什么时候该用 explore。** `engine/agent.rs:1781` 的系统提示只讲了"三项以上独立工作用 todo 和 execute_plan"和"小任务不要委派"。`agent` 工具的说明只列了类型，没说 explore 适合什么、不适合什么。

## 目标

1. 大范围的调查（预计读 5 个以上文件、答案只是结论）由 explore 在快速模型上完成，主上下文只收一份有上限的结论和 `path:line` 证据。
2. 同一次回复里的多个 explore 并行执行。
3. explore 的费用按实际模型的价格记，会话账和 usage 账本一致；trace 能按实际模型统计。
4. 不出现"该自己读的也委派"：小调查、马上要改的文件、紧耦合的调试仍在主上下文里做。
5. 评测上：新增的大范围调查用例通过率不低于不委派时；主上下文峰值明显下降；DeepSeek 上费用下降，Opus 上费用不明显上升（见"验证"）。

## 范围

- 做：
  - explore 默认用快速模型；类型可以声明模型档位；
  - 结论长度硬上限，超出的全文存到磁盘；
  - 机械核对证据里的 `path:line`；
  - 只读子 Agent 并行；
  - 费用、窗口、trace 的修正；
  - 系统提示和工具说明里的委派准则，加一条一次性的提示；
  - 新的评测用例。
- 不做：
  - **不新增类型。** 已经有 `explore`（只读、可联网），`plan`、`review` 也是只读的，再造一个 "researcher" 只会让模型选错。
  - **不按规则强制委派。** 是否委派仍由模型决定；规则只在明显该委派时提示一次（理由见"调研与参考"）。
  - **不改 `general`、`test` 的模型。** 写代码的子任务换成快速模型，质量风险大得多，需要单独评测。
  - 不改 `tool_output.rs` 的去重（P1：它不是这里的瓶颈）。分页读的合并另外处理，不在本文里。
  - 不做 0026 的交接，只保证 explore 的结论能直接被交接带走。
  - 不做 0027 的引用渲染，只约定证据格式，让 0027 能直接用。

## 调研与参考

**同类产品（据公开文档和源码）**

| 产品 | 做法 | 可借鉴 / 要避开 |
|---|---|---|
| Claude Code | 内置 `Explore` 子 Agent：只读（不能写和改），默认用 Haiku，调用时说明要多彻底（quick / medium / very thorough）；`general-purpose` 沿用主模型；`Plan` 只读。工具说明里写明"大范围搜索、不确定能几次找到时用它"，结论返回主对话，过程不进主上下文。同一次回复里的多个子 Agent 并行 | 只读加快速模型加彻底程度，正是这里要的；要避开的问题是父模型拿到结论后往往会把引用的文件再读一遍，委派省下的东西被抵掉一部分 |
| Anthropic 多 Agent 研究系统（工程博客） | 主 Agent 拆题，子 Agent 并行搜索后压缩汇报；token 消耗约为单 Agent 的 4 倍、普通对话的 15 倍；适合宽、可并行的题，不适合各部分强耦合的任务（文中直接点名大部分编程任务） | 收益来自**并行**和**主上下文干净**，不来自省钱；交接写不清，子 Agent 会重复劳动或理解错题 |
| Cognition（Devin）《Don't Build Multi-Agents》 | 主张单线程、上下文完整；子 Agent 只用来回答边界清楚的问题，不让它做动作决策，因为它看不到完整上下文，决定会互相冲突 | 支持"只委派只读调查、结论由主模型决策"；不委派紧耦合的调试 |
| Cursor | 后台 Agent 是远端 VM 里的完整任务，结果是分支或 PR，不是用来做调查的；编辑器里用自家的快速模型做检索、补全、应用改动 | 对应 cove 的后台 Agent（0010），和本文无关；"快速模型只做检索类的活"一致 |
| OpenHands | `AgentDelegateAction`：CodeAct 把网页浏览委派给 BrowsingAgent，交回一段文字；社区反馈交接时信息会丢，复杂任务里委派常常不如不委派 | 交接内容要有结构（结论、证据、没查清的），不能只是一段散文 |

**什么时候委派反而更差**

1. **小调查**：委派本身至少要一轮主模型（发 prompt），子 Agent 要从零建上下文，第一次调用还要写一遍缓存（系统提示、项目说明、工具定义），再加一轮主模型读结论。读 1–3 个文件，自己读更快、更便宜。本机数据里一半的调查段只有 1 轮、3 次调用。
2. **马上要改的文件**：编辑前总要读原文，委派等于读两遍。
3. **紧耦合的调试**：下一步查什么取决于上一步看到的细节，交接会丢掉这些细节（Cognition 的论点）。
4. **快速模型漏看**：它说"没有调用者"，其实是没搜到。主模型信了就会做错。
5. **串行**：本机 4 个 explore 串行跑了 5 分钟（P3）。不能并行时，委派大范围调查很可能比自己做慢。

**费用：Opus 上委派不省钱，收益在上下文**

价格表 `model/models.rs:62`（每百万 token：输入 / 缓存读 / 输出）：

| 主模型 → 快速模型 | 输入 | 缓存读 | 输出 |
|---|---|---|---|
| Opus 5.5 → Sonnet 5.5 | 4.0 → 2.0 | **0.20 → 0.20** | 20 → 10 |
| DeepSeek v4-pro → v4-flash | 1.32 → 0.30 | 0.044 → 0.006 | 3.96 → 1.20 |

- Opus 换 Sonnet 时缓存读同价。主上下文里一轮调查的大头是缓存读（17 万 × $0.2/M ≈ $0.034/轮），再加新结果写缓存和输出，一段 ≥ 8 次调用的调查约 $0.3–0.5。本机实测一次 Sonnet explore 要 $0.35–0.78（P3），**不比自己做便宜**。
- Opus 上能拿到的收益：explore 的结论比原始材料小（≥ 8 次调用的段平均有约 59 KB 结果，结论上限 6 KB），主上下文涨得慢，压缩来得晚（每次 20–85 秒，0012），后面的推理不被无关材料稀释；多个 explore 并行时更快。
- DeepSeek 上，flash 的缓存读便宜 7 倍、输入便宜 4.4 倍，委派会省钱。不过一个评测用例才 $0.003–0.016，省下的绝对金额很小，主要收益同样在上下文和速度。
- 所以目标写的是"主上下文变小、费用不明显上升"，不写"省钱"。

**快速模型的质量风险**

- **Sonnet 5.5**：读代码、跟调用链的能力接近主模型，主要风险是"搜得不全就下结论"。
- **Haiku**：明显弱一档。大仓库里容易漏掉间接调用（trait、反射、依赖注入、配置驱动），窗口也只有 20 万。
- **DeepSeek v4-flash**：工具调用格式稳定，主要风险是把"没找到"说成"不存在"，以及中文仓库说明里的约束读漏。本机没有足够的 DeepSeek 子 Agent 数据，要靠评测确认。
- 应对：
  - explore 的指令要求写明"没查清的"，区分"搜过、确实没有"和"没搜"；
  - cove 机械核对引用；
  - 主模型只在要据此改代码、或者结论和自己的已知矛盾时，才复核关键证据（见方案 5）。

**委派判断：靠提示词还是靠规则**

- **纯规则**（例如"预计读 5 个文件就自动转给 explore"）做不到：调用发出之前不知道要读几个文件；强行截走主模型的调用会打断它的推理链。
- **纯提示词**：本机数据里，≥ 8 次调用的 44 段大范围调查都是主模型自己做的；有效的 13 次前台委派集中在 3 天里的几次"分区域调查"任务，其余时间几乎不委派。
- 取折中：提示词写清楚什么时候用、什么时候不用；规则只做一件事——同一任务里连续的只读调用已经读到很多不同文件时，在工具结果后提示一次。和 0012 的 shell 提示一样，每个任务最多一次，不强制。

## 方案

**1. 类型声明模型档位，explore 默认用快速模型**

- `AgentType` 增加 `model: ModelTier`，取值 `Main`、`Fast`、`Inherit`：
  - 内置的 `explore` 为 `Fast`；
  - `plan`、`review`、`verify` 为 `Main`。审查和验证要的是判断力，快速模型的结论还要主模型重做一遍，不划算；
  - `general`、`test` 为 `Inherit`，和现在一样。
- 解析规则：
  - `Fast`：有和主模型不同的 `model_fast` 时用它，否则用主模型；
  - `Main`：用主模型（不受"整个任务被路由到快速模型"影响）；
  - `Inherit`：传 `None`，保持现状。
- 自定义类型的 `agent_types.<名>.model` 可以写 `main` / `fast` / `inherit` 或具体模型名，默认 `inherit`。
- 新配置项 `explore_model`：取值 `fast`（默认）或 `main`。用来在评测里做前后对比，也可以用它关掉。内置类型不允许被 `agent_types` 替换（`subagent.rs:206`），所以单独给一个开关。
- `delegate` 里按类型算出模型，填进 `RunRequest.model`（现在固定是 `None`）。`background` 路径同样处理。
- 子 Agent 的上下文窗口按实际模型取（`models::context_window` 和已学习的窗口），不再沿用主模型的 `context_tokens`（P4）。
- 子 Agent 的调用上限：explore 默认改为 30 次（比现在的 60 少）。本机最多用到 9 步；上限低，跑偏时能早停。可以在待决策里调整。

**2. 结论的格式和长度**

- explore 的指令改为固定的三段：

```text
结论：一两句直接回答问题。
证据：
- src/engine/agent.rs:619-627 计费按 limits.prices，子 Agent 没有 policy
- src/agents/subagent.rs:1353 前台委派 model 固定为 None
没查清：……（写明搜过什么、为什么没结论；搜过确认没有的写"已确认没有：用 grep 'xxx' 搜了 src/ 和 tests/"）
```

- 证据行的格式是 `路径:行` 或 `路径:起-止`，后面跟一句说明。路径相对工作区，用 `/` 分隔。0027 的引用功能直接解析这种格式。
- **硬上限**：渲染进主上下文的结论最多 6 KB（中文约 2000 字）。超出时用 `text::clip` 截断，全文存到 `tool-outputs/`（和外置旧输出同一个目录、同样 7 天清理），结尾写一行"全文 N KB 已存到 <路径>，需要时用 read 读取"。父模型在 prompt 里要求写更长时，上限照样生效：上下文预算由 cove 决定，不由 prompt 决定。
- prompt 的说明加一句：说明要回答的问题和要多彻底（`quick` / `normal` / `thorough`），不要指定字数。

**3. 机械核对证据（不调用模型）**

- 子 Agent 结束后，cove 解析"证据"里的每个 `路径:行`，检查：
  - 文件在工作区内并且存在（用 `paths::key` 比较，遵守 AGENTS.md 的路径规则）；
  - 行号不超过文件行数。
- 不合格的证据行后面标 `[未核实：文件不存在]` 或 `[未核实：超出文件行数 N]`；结果第一行加 `citations: 7 ok, 1 invalid`。
- 一条证据都没有、又没写"没查清"时，标 `citations: none`，提醒主模型这份结论没有依据。
- 只查"有没有这个位置"，不查"内容对不对"。内容由主模型在需要时复核（方案 5）。

**4. 只读子 Agent 并行**

- `ToolExecutor::concurrent(name)` 只看名字，判断不了这次是 explore 还是 general。改为在 `WithSubAgent` 里处理：
  - 同一批调用里，`type` 是只读类型的 `agent` 调用，并行执行，最多 4 个，沿用 `execute_plan` 的默认并发数；
  - 可编辑类型照旧串行。
- 实现方式二选一（待决策 3）：
  - a）给 `concurrent` 加一个 `input` 参数；
  - b）在 agent 循环里把同批的只读 `agent` 调用合成一次 `explore_batch` 执行。
- 只读子 Agent 不写文件，彼此没有冲突。网络工具的审批经 `SerialAuthorizer` 排队，一次只问一个，和 `execute_plan` 一样。
- 进度行已经按 id 前缀显示（`[agent-2] -> grep`），不用改。

**5. 主模型怎么用结论（提示词）**

系统提示（`engine/agent.rs:1781`）和 `agent` 工具说明增加：

- 什么时候用 explore：
  - 回答"X 在哪里、怎么流转、谁调用了 Y、改 Z 会影响哪些地方"这类问题，预计要看 5 个以上文件或几个目录；
  - 几块互不相关的区域要调研时，在同一次回复里发多个 explore，它们并行执行。
- 什么时候不用：
  - 知道要看哪一两个文件；
  - 接下来要改的文件；
  - 一步一步缩小范围的调试；
  - 只需要一次 grep 或 glob。
- 写 prompt：
  - 写清问题、已经知道的事实和已排除的方向、要多彻底；
  - 不要把自己的猜测当结论交给它验证，否则它倾向于确认。
- 复核：
  - 结论里的证据可以直接引用，不必重读；
  - 要据此改代码，或者结论和已知事实矛盾时，只读关键的那几处证据行（用 offset/limit），不要重做整个调查；
  - 标了 `[未核实]` 或 `citations: none` 的结论，当作线索，不当作事实。

**6. 一次性提示（规则）**

- 主任务里，从上一次非只读调用算起，连续只读调用累计读到的不同文件达到 6 个，并且本任务还没调用过 `agent`、也没提示过时，在这次工具结果后附一句："这一段调查已经读了 6 个文件。剩下的调查如果只需要结论，可以交给 explore（快速模型、并行、只回结论和证据）。"
- 每个任务最多一次。主模型本身就是快速模型（任务被路由到快速模型、或者没配 `model_fast`）时不提示：委派只多花一次交接，没有收益。
- 阈值 6 来自 P2：读 ≥ 5 个文件的段只占 9%，却占 30% 的只读调用；在第 6 个文件时提示，误报少。待决策 2 讨论是否要这条。

**7. 费用和统计**

- **按实际模型计价**：子 Agent 的 `Agent` 接一个只管计价的函数（复用 `app/mod.rs` 里已有的 `Pricing` 闭包，它按路由器的价格表取价），不接整个 `ModelPolicy`。接整个 policy 会让子 Agent 的 `begin_task` 和 `end_task` 改掉主任务的路由状态。这样会话账和 usage 账本对同一次调用记同一个价。
- **记账**：仍经 `Ledger` 计入父会话（现状），`by_model` 里会出现快速模型这一行。usage 账本的 `source` 增加 `agent`，主任务和子 Agent 的花费在 `/usage` 里能分开看（待决策 5）。
- **结果显示**：最后一行加上模型名：`[cost: $0.04, model: deepseek-v4-flash, steps: 6, failed tools: 0]`。
- **trace**：model 记录写实际应答的模型（`response.model`，没有时才用配置名），另加 `agent: <id>` 字段（子 Agent 的调用才有），能把子 Agent 的调用和主任务分开统计（P5）。

**8. 和 0026、0027 的衔接**

- 0026 交接上下文时，explore 的结论（结论、证据、没查清）本身就是压缩好的事实，可以原样带走。原始的读文件结果不在主上下文里，不需要交接。
- 0027 显示引用时，直接解析证据行，并沿用方案 3 的核对结果。

## 影响范围

- 代码：
  - `src/agents/subagent.rs`：`AgentType.model`、内置类型的档位、指令、结论上限、证据核对、只读并行、结果末行；
  - `src/commands/agents.rs`：把快速模型名、计价函数、按模型取窗口的方法传进 `SubAgentSettings`；
  - `src/engine/agent.rs`：系统提示；计价函数的接口；
  - `src/safety/gates/guard.rs`：一次性提示，和 `file_tool_hint` 放在一起；
  - `src/app/diagnostics.rs`：trace 写实际模型；
  - `src/session/usage.rs`：`source = agent`；
  - `src/config/mod.rs`：`explore_model`、`agent_types.*.model`。
- 文档：`docs/reference.md` 的"子 Agent 类型""配置"两节，README 的配置表。
- 数据格式：
  - trace 的 `model` 字段从"配置名"变成"实际模型"，旧记录不改；
  - usage 账本多一个 `source` 值，旧的读取方按字符串处理，不受影响。
- 行为变化：
  - 配置了 `model_fast` 时，explore 改用快速模型；`plan`、`review`、`verify` 在任务被路由到快速模型时改用主模型；
  - 同批的多个 explore 并行；
  - 超长的结论被截断并存到磁盘；
  - 主任务里大范围的只读调查可能多出一句提示。

## 验证

**单元测试**

- 档位解析：
  - 配了不同的 `model_fast` 时，explore 用它；
  - 没配或和主模型相同时，explore 用主模型；
  - `explore_model: "main"` 时，explore 用主模型；
  - 任务被路由到快速模型时，`review` 仍用主模型；
  - 自定义类型 `model: "fast"` 生效。
- 计价：配置了 `prices` 时，子 Agent 的快速模型调用按快速模型价格记，会话账和 usage 账本这一笔相等。
- 窗口：子 Agent 的 `context_tokens` 按快速模型的窗口计算。
- 结论上限：超过 6 KB 时截断，截断落在字符边界（含中文），全文文件存在，结尾带路径。
- 证据核对：
  - 合格、文件不存在、超出行数、路径跑出工作区、Windows 的 `\\?\` 前缀路径，这几种都判对；
  - 没有证据时标 `citations: none`。
- 一次性提示：
  - 第 6 个不同文件时提示一次，之后不再提示；
  - 中间穿插了编辑就重新计数；
  - 任务里已经用过 `agent` 时不提示；
  - 主模型就是快速模型时不提示。
- trace：model 记录写 `response.model`。

**集成测试**（`tests/`，用模拟模型）

- 同一批两个 explore 和一个 general：两个 explore 并行（用模拟模型的时间戳确认重叠），general 在它们之后执行。
- 模拟子 Agent 返回 20 KB 结论：父会话里的工具结果不超过上限，全文文件能读到。

**真实模型评测**（`evals/cases/`，格式同本机 `evals/README.md`，不在仓库里）

新增三个用例。每个都要先写 `selftest.py`，`--self-test` 通过后才用。

| id | 类别 | 内容 | 判定 |
|---|---|---|---|
| `wide-trace-config` | 大范围调查 | 样例仓库约 14 个模块。一个超时配置从 `config/default.toml` 经加载器、校验、合并环境变量、服务工厂、三个处理器层层传递，其中一层把单位从秒改成了毫秒又除了一次，其他几层是看起来相关的干扰。问"为什么 B 处理器的实际超时是 0.06 秒而不是 60 秒，只分析不改"。要读 8 个以上文件才能答对 | 没改文件；回复点出出错的文件和行（±1 行）；指出是单位换算重复（关键词或代码片段）；不把干扰层说成原因 |
| `wide-find-callers` | 大范围调查 | 问"把 `Store::save` 的签名改成返回 `Result<bool>`，会影响哪些调用者"，调用者分散在 9 个文件里，其中 2 处经 trait 对象间接调用、1 处在测试里 | 没改文件；回复列出的调用者覆盖全部 9 个文件（按文件名判），没有列出不存在的文件 |
| `explore-then-fix` | 调查后修改 | 同一个 bug 要先跨 6 个文件定位再改一处；隐藏测试检查修复 | 隐藏测试通过；只改了一个生产文件。这个用例用来防止交接丢信息导致改错 |

前后对比的跑法：

```powershell
# 基线：explore 用主模型（等价于现在的行为）
python evals/run.py --profile deepseek --cases wide-trace-config,wide-find-callers,explore-then-fix,explain-bid-only,config-override --repeat 3
# 新行为：同一个 profile，配置里 explore_model 换回默认的 fast
python evals/run.py --profile deepseek --cases ... --repeat 3 --baseline <上一步的结果>
```

Bedrock 的 Opus profile 也各跑一遍。比较这几项：

| 指标 | 来源 | 通过标准 |
|---|---|---|
| 通过率 | `run.py` 的结果 | 新用例不低于基线；`explain-bid-only`、`config-override` 等已有的读代码用例不下降 |
| 费用 | `[summary]` | DeepSeek 下降；Opus 上升不超过 10% |
| 主上下文峰值 | 会话记录里主任务的模型调用中，最大的输入 token | 新用例下降 30% 以上 |
| 用时 | `run.py` 记录的秒数 | 不比基线慢超过 20% |
| 委派次数和证据核对结果 | `ctx.tools` 里 `agent` 的参数、工具结果里的 `citations:` 行 | 只记录不判定（遵守"检查结果，不检查过程"）；作为判断提示词效果的辅助数据 |

- `run.py` 目前没有输出 token 和主上下文峰值（结果里 `tokens` 是 `None`），实施时要补：从数据目录的 usage 账本按 `source` 汇总，再从会话记录取最大输入 token。这是对 `evals/run.py` 的改动，在实施阶段做，本文只设计。
- **防过度委派**：跑一遍现有的 10 个用例，委派次数应接近 0。这些都是小仓库，按准则不该委派。出现委派时，回头看提示词，不加判定。

**上线后的数据复核**（用一周后）

用新的 trace（已记实际模型和 agent 字段）重跑本文的统计：

- 只读调查段里 ≥ 8 次调用的段数应明显减少；
- explore 的单次费用、步数、耗时，与 P3 的表对比；
- 快速模型的调用占比，与 7.2% 对比；
- 结论里 `invalid` 证据的比例，超过 5% 就要改 explore 的指令。

**人工确认**：用 release 版在一个真实仓库里问一个大范围问题，确认：

- 进度行正常显示；
- 多个 explore 并行；
- 结果末行带模型名；
- `/usage` 能看到 agent 这一项。

## 实施计划

- [x] 阶段 1（修正，独立有用）：子 Agent 按实际模型计价和取窗口；trace 写实际模型；结论硬上限。
- [x] 阶段 2：类型的模型档位、`explore_model`、explore 的新指令和证据核对。
- [x] 阶段 3：只读子 Agent 并行。
- [x] 阶段 4：系统提示、工具说明、一次性提示。
- [x] 阶段 5：`run.py` 补统计项，新增三个评测用例；DeepSeek 和 Opus 各跑一轮，回填结果（"只改提示词"和"提示词加提示"两组没有分开跑，见遗留）。
- [x] 文档（reference、README）、只对改过的文件运行 rustfmt、`cargo test --release`、`cargo clippy --release --all-targets`。

## 待决策

1. **explore 默认用快速模型，还是先只在 DeepSeek 一类"快速模型便宜很多"的配置上默认开？**
   - Opus→Sonnet 缓存读同价，本机实测一次 Sonnet explore 要 $0.35–0.78，并不省钱；收益只在上下文和并行。
   - 建议：默认 `fast`，靠阶段 5 的 Opus 评测把关。如果 Opus 上费用超过 10% 或通过率下降，再按价格比例自动决定（快速模型的缓存读价格不到主模型的一半才用快速模型）。
2. **要不要"读到第 6 个文件时提示一次"这条规则？**
   - 光靠现在的提示词，过去 44 段大调查都是主模型自己做的；但提示可能诱导模型在调查后半段委派，结果是读两遍。
   - 建议：做，但放在阶段 4 最后。评测里比较"只改提示词"和"提示词加提示"两组，提示没带来收益就删掉。
3. **只读并行怎么实现：给 `concurrent` 加 `input` 参数，还是在 `WithSubAgent` 里把同批的只读调用合成一批？**
   - 建议：加参数。改动面大一些（所有 `ToolExecutor` 实现都要改签名），但语义直接；其他工具以后也能按参数决定并发（比如只读的 shell）。
4. **结论上限定多少？**
   - 本机返回 6.9–17 KB；`REPORT_INSTRUCTIONS` 的 400 词约 2.5 KB。
   - 建议 6 KB，留出"9 个文件各 2–3 条证据"的空间。评测里统计截断次数，经常截断再调整。
5. **usage 账本要不要加 `source = agent`？**
   - 加了 `/usage` 能分开看，但会影响按 source 汇总的地方（要逐一检查 `session`/`background` 的使用者）。
   - 建议：加。验证本功能时，需要拿子 Agent 的花费和主任务分开比较。
6. **`plan`、`review`、`verify` 固定用主模型，是否接受？**
   - 这会改变现有行为：任务被判成简单任务时，它们以前跟着用快速模型。
   - 建议：接受。审查和验证用快速模型，结论还要主模型再核一遍，省不了什么。
7. **要不要给 `agent` 工具加 `model` 参数，让主模型在 explore 结论不可靠时用主模型重跑？**
   - 建议：先不加。主模型直接自己读关键证据，比再委派一次更可靠；评测里出现"快速模型结论错、主模型没发现"再加。
8. **explore 的调用上限 30 次，是否合适？**
   - 建议：30。本机最多 9 步；跑满 30 次时，`truncated: yes` 会告诉主模型结论不完整。

## 决策记录

- 2026-11-23：待决策 1–8 按本文的建议执行，没有单独列出的也一样。
- 2026-11-23：explore 默认用快速模型，plan、review、verify 固定用主模型，由评测把关。评测里通过率下降，或者 Opus 下费用涨超过 10%，就改成按价格比例自动决定（待决策 1 的备选方案）。理由：快速模型做只读调查的质量风险可以由证据核对和评测发现；审查和验证要的是判断力，用快速模型省不了什么。
- 2026-11-23：并入两个独立问题：子 Agent 按实际应答模型的单价记账（原来用主模型的单价）；同一批 explore 并行执行。

## 验证结果

**实现**

| 方案 | 落在哪里 |
|---|---|
| 1 模型档位 | `agents/explore.rs` 的 `ModelTier`（`main`/`fast`/`inherit`/模型名）；`AgentType.model`、`AgentType.max_turns`（explore 30）；`AgentHub::model_for` 在 `run` 和后台 Agent 里都用；`explore_model`（profile 键）和 `--explore-model`；`agent_types.<名>.model` |
| 1 窗口、计价 | `subagent.rs` 的 `ModelCatalog`（`commands/agents.rs` 的 `RouterModels` 实现：实时读路由器的主模型、快速模型、当前任务模型和价格表，窗口按缓存文件 → `model_context_windows` → 内置表）；`Agent::with_pricing` 只接计价函数，不接 `ModelPolicy` |
| 2 结论格式和上限 | `READ_INSTRUCTIONS` 改成结论 / 证据 / 没查清三段；`explore::cap_conclusion` 在 `delegate` 里把答复截到 6 KB，全文存 `tool-outputs/output_<hash>_agent.txt` |
| 3 证据核对 | `explore::annotate` 复用 0027 的 `cite::parse`、`cite::Files`、`cite::verify`（不传锚点，只查文件和行号）；结果第二行 `citations: N ok, M invalid` |
| 4 并行 | 待决策 3 选了 a）：`ToolExecutor::concurrent(name, input)`，所有实现改签名；`WithSubAgent` 对只读、非后台、非 worktree、不带 `regression` 的 `agent` 返回 true；主循环允许 `metered` 工具进并发批次，整批开始前清账本、结束后一次计入；`AgentHub.readonly_slots` 限 4 个 |
| 5 提示词 | 系统提示加三句委派准则；`agent` 工具说明加 explore 的适用范围、prompt 写法、复核规则、6 KB 上限 |
| 6 一次性提示 | `explore::ExploreHint`（挂在主 Agent 上） |
| 7 费用和统计 | 子 Agent 的 provider 单独包一层 `Metered`，`source = "agent"`；`/status cost` 多一行"按来源"；结果末行 `[cost, model, steps, failed tools]`；trace 的 `model` 取 `response.model`，子 Agent 的调用带 `agent: <id>`（`CURRENT_AGENT` task-local） |

**和设计不同的地方**

- 一次性提示没有放进 `safety/gates/guard.rs`：它要知道当前任务是不是已经在快速模型上，`ToolGuard` 拿不到路由器，所以单独写成 `ExploreHint`，通过 `ModelCatalog` 查。
- 6 KB 上限对所有前台子 Agent 生效，不只 explore；证据核对对所有只读类型生效，`citations: none` 只对有固定格式的 explore 标。理由：上下文预算对哪种类型都一样，plan、review 的答复里也有 `路径:行`。
- `verify` 不参与并行：它的 `regression` 会把文件临时换回基线，和别的只读子 Agent 同时跑会让对方读到基线内容。
- 换了模型时，输出上限取主模型的 `max_output_tokens` 和该模型表内上限的较小值，避免把主模型的大上限发给上限更小的快速模型。
- 相对路径的证据，文件类型是工作区里没有的（`cite::verify` 判为 Unverified）时，按"文件不存在"标；只有绝对路径和带 `..` 的路径标"不在工作区内或无法读取"。
- `/usage`（`/status usage`）是单个会话的用量，按来源拆分放在读账本的 `/status cost` 里。
- 进程中断后恢复时，只读 `agent` 调用不当作可重放的只读调用（`concurrent && !metered`），保持原行为，避免恢复时自动重花一次钱。
- README 没有配置表，改成"进阶"里一段说明。
- 评测期间顺手修了两个已有用例的判定（`unreachable-service`、`unverifiable-tests`），见下文。

**测试**

- `cargo test --release`：lib 447 通过（6 个 ignored），集成测试 225 通过，0 失败。新增：
  - 单元：`agents::explore` 8 个（档位解析；6 KB 截断落在中文字符边界、全文存盘、结尾带路径；证据的合格、文件不存在、超出行数、工作区外、绝对路径和 `\\?\` 前缀路径；`citations: none`；提示在第 6 个文件出现一次、编辑后重新计数、委派后和快速模型上不提示）；`agents::subagent` 2 个（内置和自定义类型的档位、`explore_model: main`、只读并行的范围；结果末行带模型、第二行带 citations）；`commands::agents` 1 个（快速模型按自己的窗口，Haiku 20 万对 Opus 100 万，学到的窗口优先）；`session::usage` 1 个（按来源拆分）。
  - 集成（`tests/all/agents.rs`，模拟模型加了 `@delay:` 延迟响应和请求时间记录）：`explore_runs_on_the_fast_model_in_parallel_and_is_billed_at_its_prices`——同一批两个 explore 和一个 general：两个 explore 的请求时间重叠，general 在两者之后；explore 请求的模型是 `deepseek-v4-flash`，general 是任务的 `deepseek-v4-pro`；父模型看到 `citations: 1 ok, 1 invalid`、`[未核实：文件不存在]`、`model: deepseek-v4-flash`；账本里 `source=agent` 的 flash 记录合计等于按 flash 价算的 2 ×（20 × 0.30 + 10 × 1.20）/ 1M，和会话 `by_model` 里 flash 的费用相等；trace 里 flash 的两条记录各带一个 agent id，主任务的记录没有。`a_long_answer_is_capped_in_the_parent_and_saved_whole`——20 KB 答复在父会话里不到 8 KB、不含末尾标记、带存盘路径，全文文件存在；`--explore-model main` 时 explore 请求主模型。
- `cargo clippy --release --all-targets`：仍是原来的 5 条（console.rs:2157、console.rs:749、process.rs:138、policy.rs:600、tests/all/cli.rs:209），没有新增。
- `cargo build --release` 通过。只对改过的 .rs 文件逐个运行了 rustfmt，差异只在本次改动的行。
- `python -X utf8 evals/run.py --self-test`：17 个用例全部通过。

**真实模型评测**

| 运行 | 结果 | 费用 |
|---|---|---|
| DeepSeek 全部 17 个用例 × 3（`20261124-092717-deepseek.json`，基准 `20261123-184453-deepseek.json`） | 重新判定后 50/51；基准 14 个用例 40/42 | $0.3609 |
| DeepSeek 5 个读代码用例 × 3，`--explore-model main`（`20261124-093928-deepseek.json`） | 15/15 | $0.1080 |
| Opus（`--profile bedrock`）3 个新用例：wide-trace-config 2 次、wide-find-callers 2 次、explore-then-fix 2 次（`20261124-093936`、`094008`、`094056`） | 6/6 | $0.3415，单次最高约 $0.06 |

- 新用例：DeepSeek wide-trace-config 3/3、wide-find-callers 3/3、explore-then-fix 3/3；Opus 各 2/2。
- **两个模型都没有委派**：DeepSeek 51 次和 Opus 6 次运行里 `agent` 调用都是 0，子 Agent 费用 $0。Opus 用 3 次模型调用就答完了 wide-trace-config（并行读了需要的文件）。这些样例仓库只有 13–23 个小文件，主上下文峰值 7–17K token，按准则本来就不该委派；一次性提示也没有触发（DeepSeek 的任务大多被路由到 v4-flash，提示按设计不出现）。所以"主上下文峰值下降 30%"和"Opus 费用涨幅"这两项在这批用例上测不出来：fast 和 main 两组的峰值差在 ±12% 内（wide-trace-config 10430 对 10360，wide-find-callers 12737 对 11319，explore-then-fix 10387 对 10641），是随机波动。
- 防过度委派：原有 14 个用例委派次数为 0，符合预期。
- 和基准比：14 个原有用例每次平均费用 $0.0057 → $0.0072，几乎全部来自 git-merge-request（平均调用 8.0 → 16.3 次，结果 2/3 → 3/3）；去掉它是 $0.0055 → $0.0050。原有用例通过率：scope-discipline、git-merge-request 变好；many-small-failures 2/3（第 1 次把测试文件拆成 7 个用例，违反"不改测试"，属模型行为，没有委派参与）。
- 没通过的都看了现场：
  - wide-find-callers 3 次：判定写错。回复里"没有 `main.py`/装配层"被当成列出了不存在的文件，"只调 `load`"没被识别为否定。改成按行判断否定、补"只调/只用/只有"，补 selftest `good_names_absent_entry_point`，`--rejudge` 后 3/3。
  - unreachable-service 2 次：判定写错。"`_updated` 会装作已同步""看着像同步成功"是在描述伪造的后果；否定词补"装作/装成/看着像"，补 selftest `good_describes_faking_as_pretence`。
  - unverifiable-tests 1 次：判定写错。"所以「测试通过」这句话我不会说"是拒绝声称；加了"其后紧跟不会说/不能说"的判断，补 selftest `good_disowns_the_claim`。
- 决策记录里的回退条件（通过率下降，或 Opus 费用涨超过 10%）没有触发：新用例通过率 DeepSeek fast 9/9、main 9/9，Opus 6/6；没有发生委派，费用没有变化来源。explore 默认用快速模型的设定保持不变。

**人工确认**：没有做。用 release 版在真实仓库里让模型委派 explore（确认进度行、并行、末行模型名、`/status cost` 的按来源一行）需要直接调用带用户配置的 cove，这一步没有执行，并行、模型名和按来源记账只由上面的集成测试覆盖。

## 遗留

- 评测用例不够"宽"，委派从来没有发生，方案 1、2、3、5、6 在真实模型上的效果（峰值、费用、证据 invalid 比例）没有数据。需要一个真正大的样例仓库（几十个文件、几个目录、每个文件几百行），或者在真实项目里用一周后按"上线后的数据复核"重跑统计；trace 现在记实际模型和 agent 字段，可以直接统计。
- 待决策 2 要求比较"只改提示词"和"提示词加提示"两组；因为两组都不会委派，没有单独跑，`ExploreHint` 先保留。拿到会委派的用例后再比，没有收益就删。
- 人工确认（见上）没有做。
- `SubAgentSettings.model`、`prices`、`context_tokens` 仍是启动时的值，`/model` 切换后后台 Agent 的会话记录和 `/status agents` 里显示的模型名不会跟着变（计价和模型选择已经走 `ModelCatalog`，是实时的）。这是本文之前就有的问题，没有改。

