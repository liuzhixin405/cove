# 0005 统一一次性模型调用，修正费用账本

- 状态：已完成
- 日期：2026-11-20（创建）；2026-11-20（完成）
- 关联：同类功能统一分析（第二组）；[0004](0004-unified-process-env-and-git.md)

## 背景

同类功能统一分析的第二组。"调用一次模型、拿到一段文字"这件事，在代码里写了 9 处；费用的计算也有两套。核查时发现，账本记录的费用存在系统性错误。

## 现状与问题

**P1 账本把缓存读取按全价计费，费用高估约 6 倍。**
- 证据（`~/.cove/usage/2026-11.jsonl` 中的一条 session 记录）：`in` 100065，其中 `cache_read` 98816、`cache_write` 1247，`out` 955，记录的 `cost` 为 $0.41936，正好等于 100065 × $4 + 955 × $20（每百万 token）。缓存读取按约 10% 计价、缓存写入按 1.25 倍计价，正确的费用约为 $0.065。
- 原因：账本记录器 `session/usage.rs` 的 `Metered` 用 `bill(prices, input, output)`，只认两种价格；会话自己的费用统计 `Agent::account`（`engine/agent.rs`）用 `Prices::cost`，会区分三种输入。**同一笔开销两套算法**：状态行显示的会话费用是对的，`/usage` 报告里的账本合计（$2723）严重偏高。

**P2 后台调用记成了主模型。**
- 后台调用实际走的是快速模型（`router.background()`，即 `model_fast`），但 `Metered` 记录的是固定的 `settings.model` 和主模型价格，没有使用响应里填好的实际模型（`response.model`，`model/provider.rs:1119`）。
- 证据：账本里 527 条 `background` 记录全部是 `claude-opus-5-5`；用户的 bedrock profile 里 `model_fast` 是 `claude-sonnet-5-5`。（已删除的技能复盘自己单独记账，同样的快速模型调用被正确记成了 sonnet，共 43 条。）

**P3 `/init` 的模型调用没有记进账本。** `commands/project.rs` 的 `draft_with_model` 用的是 `app.background_provider()`，返回的是没有经过记账和跟踪包装的 `router.background()`。它的会话费用是手工算的（主模型价格、忽略缓存）。

**P4 一次性调用的样板代码写了 9 处。** 记忆提取、标题、网页摘要、`/init`、配置向导的连通测试、调用次数用完时的总结、`/doctor` 诊断分析、上下文压缩：
- 构造 `ModelRequest { system, messages: vec![Message::user(..)], tools: vec![], .. }`：7 处；
- 丢弃流式事件 `tokio::spawn(async move { while receiver.recv().await.is_some() {} })`：7 处，而且收尾方式不同（中止、等待、放任不管）；
- 从响应里拼出文字：7 处；
- "响应不完整或带工具调用就算失败"的检查：3 处；
- 后台任务的"认领 → 启动 → 记下句柄 → 退出时限时等待"：记忆提取和标题各写了一遍。
- 由此产生的不一致：**标题生成不检查响应是否完整**，回复被截断时也会拿去当标题。

## 目标

1. 账本的费用和会话统计用同一个计价函数：按实际使用的模型查价格，区分普通输入、缓存读取、缓存写入。
2. 账本记录实际使用的模型。
3. 所有模型调用都经过记账和跟踪包装，包括 `/init`。
4. 一次性调用的样板只有一份：构造请求、带超时调用、取出文字并检查完整性、后台任务的认领和收尾。

## 范围

- 做：
  - 新增 `src/model/oneshot.rs`；
  - `Metered` 改为按 `response.model` 和路由器的价格计价，计价函数和 `Agent::account` 共用；
  - `Application` 保存经过包装的后台 provider，`background_provider()` 返回它；`/init` 改用共享的计价函数；
  - 9 个调用点改用 `oneshot`；记忆提取和标题的后台任务管理改用公共的 `Background`。
- 不做：
  - **不改写历史账本**：已有记录里，后台调用的真实模型无法可靠还原（profile 后来改过），重新计价反而会制造新的错误数据。在 `reference.md` 里注明"2026-11-20 之前的账本费用偏高"。
  - dream 不改：它走的是完整的 `Agent::run_detached`，不属于一次性调用。
  - 后台调用在账本里的来源仍然统一记为 `background`，不细分成提取、标题等：要细分，就得让请求带上来源字段，改动面大，收益小。顺手修正注释里"extraction / dream"的过时说法。
  - 预算相关的代码（`budget_notice`、`_budget` 参数）留给 0006 处理。

## 方案

**计价**（`model/models.rs`）：

```rust
/// What a response cost: the provider's reported cost, else the usage at `prices`
/// (cache reads and writes at their own rates); `None` when neither is known.
pub fn charge(response: &ModelResponse, prices: Option<Prices>) -> Option<f64>;
```

- `Agent::account` 和 `Metered` 都调用它；`Metered` 用 `response.model`（为空时退回配置的主模型）向路由器查价格（`RoutingPolicy::prices`），通过一个价格查询闭包传进来。
- `bill()` 删除。

**一次性调用**（`model/oneshot.rs`）：

```rust
pub fn text_request(system: impl Into<String>, user: impl Into<String>, max_output_tokens: u32) -> ModelRequest;
pub fn response_text(message: &Message) -> String;
pub enum Incomplete { Truncated, ToolUse, Empty }
pub fn require_text(response: &ModelResponse) -> Result<String, Incomplete>;
/// 丢弃流式事件、带超时；超时或取消时取消请求。
pub async fn call(provider: &dyn Provider, request: ModelRequest, timeout: Duration, cancel: CancellationToken) -> Result<ModelResponse>;
/// 后台任务：认领（去重）、启动、记下句柄、退出时限时等待、最近一次结果。
pub struct Background<O> { .. }
```

- 每个调用点保留自己的超时、`max_output_tokens` 和失败处理；只替换样板。
- 标题生成改用 `require_text`，响应不完整时不再拿来当标题。

## 影响范围

- 文件：新增 `src/model/oneshot.rs`；修改 `src/model/models.rs`、`src/model/mod.rs`、`src/session/usage.rs`、`src/app/mod.rs`、`src/engine/agent.rs`、`src/memory/extract.rs`、`src/session/titles.rs`、`src/tools/web.rs`、`src/commands/project.rs`、`src/app/setup.rs`、`docs/reference.md`。
- 行为变化：从此以后，账本里的费用会明显变低（这才是对的），后台调用记录的是实际的快速模型；`/init` 的调用出现在账本里。

## 验证

- 单元测试：`charge` 区分三种输入（用 P1 里那条真实记录的数字作为测试数据，期望约 $0.065）；`Metered` 记录 `response.model` 和对应的价格；`require_text` 的三种失败情况；`Background` 的认领去重和限时等待；标题生成在响应被截断时不产生标题。
- 全量 `cargo test` 通过。
- 只对改动过的文件，通过标准输入运行 `rustfmt`。

## 实施计划

- [x] `charge` 和 `Metered` 计价修正；`/init` 改用包装后的 provider
- [x] `oneshot` 模块和测试
- [x] 9 个调用点迁移
- [x] 文档、测试、格式化，回填验证结果

## 待决策

无。

## 决策记录

- 2026-11-20：按同类功能统一分析的建议顺序实施，这是第二组。（用户："按照建议做"，"继续"）
- 2026-11-20：不改写历史账本，只在文档里注明。理由见"范围"。（实施方）
- 2026-11-20：会话的费用累加（用量、费用、按模型分账）抽成 `Accounting::add_response`，由 `Agent::account` 和 `/init` 共用。`/init` 原来手工计价，还漏掉了缓存 token 和按模型分账。（实施方）
- 2026-11-20：配置向导的连通测试（`app/setup.rs`）只改用 `text_request`，超时和错误分类保留原样：它要把"超时"和"服务器接受了密钥、只是回复异常"区分开，给出不同的提示。`/doctor report` 的调用方和 dream 走的是 `Agent` 的完整流程，不属于一次性调用，不改。（实施方）
- 2026-11-20 收尾核对（[0014](0014-audit-fixes.md)）：
  - 记忆提取没有改用 `Background`，保留了自己的后台任务管理：它要求同一时间只跑一个提取，而 `Background` 是"每个键只跑一次"，语义不同；
  - `Background::last()` 没有调用方，已在 0014 删除；
  - "回复被截断时不产生标题"和"`Background` 限时等待"两个计划中的测试，前者没有写，后者已在 0014 补上。

## 验证结果

- **新增测试**：
  - `charge`：用 P1 里那条真实账本记录的数字（输入 100065，其中缓存读取 98816、缓存写入 1247，输出 955，价格 $4/$20）计算，结果在 $0.06–0.07 之间，原来记的是 $0.419；provider 报告了费用时用报告值；没有报告用量时返回 `None`；
  - 集成测试 `the_ledger_records_the_model_that_answered_at_its_cache_aware_prices`：响应里的模型是 `fast-model`、配置的主模型是 `main-model` 时，账本记录 `fast-model`，并按它的价格、区分缓存计费；
  - `oneshot`：`require_text` 的三种失败（被截断、带工具调用、空白）；`call` 超时后返回超时错误并取消请求；`Background` 同一个键只认领一次，并保留最近一次结果。
- **调用点**：记忆提取、标题、网页摘要、`/init` 全部改用 `call`、`text_request`、`require_text` 或 `response_text`；调用次数用完时的总结、诊断分析、上下文压缩改用 `require_text`，各自的错误提示不变；另外两处"取最后一条回复的文字"（`engine/agent.rs`、`safety/gates.rs`）改用 `response_text`。**标题生成现在会拒绝被截断的回复。**
- `/init` 改用经过记账和跟踪包装的后台 provider（`Application::background`），费用按实际模型和缓存计价，并记入按模型分账。
- `cargo test` 全部通过：单元测试 376 个（另有 4 个忽略），集成测试 224 个。第一次全量运行时那个已知的 hook 测试偶发失败，单独运行连续两次都通过；第二次全量运行全部通过。
- 只对改动过的文件，通过标准输入运行了 rustfmt；`cargo build --release` 完成。
- `reference.md` 注明：2026-11-20 之前的账本费用偏高，没有回改。

## 遗留

- 预算相关的代码（`budget_notice`、`budget_message`、`Extractor`/`Dreamer` 里被忽略的 `_budget` 参数）留给 0006 处理。
- `session/notes.rs` 里还有一处自己拼接回复文字，会话笔记在第四组会整体并入记忆，届时一起处理。
