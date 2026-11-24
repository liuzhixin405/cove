# 0027 读代码问答带 path:line 引用，并在本地自动核实

- 状态：已完成
- 日期：2026-11-23（创建）；2026-11-23（完成）
- 关联：0021（项目规则）、0023（真实模型评测）、`src/safety/gates/nudge.rs`（收尾催促）、`evals/cases/explain-bid-only`

## 背景

现有的完成闸门几乎都只在**改了文件**之后起作用：自动验证、`done_check`、自审、提交前审查。不改文件的回答，也就是读代码问答、解释报错、运维排查，结束前没有任何"拿出依据"的约束，模型凭名称、凭记忆或凭文档下结论，用户只能自己去核对。

本机的纠正记录和项目规则都指向同一个问题：

- quote-hub 的 `.cove/rules.md` 第 5 条（2026-11-23 自动记下）："不要凭名称搜索命中或略读就下结论（集成是否已存在、算法细节、文档中的端口和行为）；先在代码里找到真实的调用点并逐条核对，再作答或保存。"
- 曾出现：回答里推断字段含义，被纠正为应去官方文档查真实数据。
- 曾出现：问的是代码里的实现，回答的却是和 redis 的协议对比。

## 现状与问题

数据来自本机 `~/.cove/sessions/*.json`（122 个）和 `~/.cove/archives/*.json`（14 个压缩前的存档），按"用户发出一条消息到下一条消息"切成轮次，按（会话、请求、最终回复）去重，共 **440 轮**。分析脚本只读，没有改动任何仓库。模型分布：426 轮是 Bedrock 上的 Claude Opus，其余 14 轮是本地小模型；本机会话里**没有 DeepSeek 的轮次**，DeepSeek 的数据只有 `evals/results/` 里的两次评测。

1. **不改文件的轮次占多数，但没有核实机制。**

   | 这一轮做了什么 | 轮次 | 占比 |
   |---|---|---|
   | 调用了 `edit`/`write` | 70 | 16% |
   | 没调 `edit`/`write`，但 shell 命令写了文件（重定向、`rm`、`cp`、`mkdir` 等，多数是临时脚本） | 83 | 19% |
   | 只有 git 状态变化（`pull`、`checkout`、`commit` 等） | 13 | 3% |
   | 只用了只读工具 | 193 | 44% |
   | 没调用任何工具 | 81 | 18% |

   前期调研说的"约 89% 不改文件"复现不出这个数：只看 `edit`/`write` 是 **84%**；把 shell 写文件和 git 状态变化也算作改动是 **62%**。不管用哪个口径，多数轮次都落在现有闸门之外。按"本轮没改文件、调用过工具、最终回复不少于 200 字"粗略算作读代码问答，有 **165 轮，占 37.5%**。

2. **问答回复里带行号引用的只有三分之一。** 这 165 轮里：

   | 回复里的依据 | 轮次 | 占比 |
   |---|---|---|
   | 有 `path:line` 引用（`file.cs:12`、`file.md` 第 20 行这类） | 48 | 29% |
   | 有行号引用（含只写"第 30 行"、没有紧挨着文件名的） | 59 | 36% |
   | 只提到文件路径，没有行号 | 69 | 42% |
   | 文件和行号都没有 | 37 | 22% |

   另有 69 轮**一个工具都没调**、回复却不少于 200 字，其中 47 轮在 git 仓库里、提到两个以上代码符号，也就是凭上下文或记忆在讲代码。这 47 轮里有 7 轮带了行号。

   引用格式实际出现的有：`path:line`、`path:line-line`、`` `file` 第 N 行 ``、`第 N–M 行`（文件写在小标题里）、表格里"`SourceEndpoints.cs:37`"、"（第 88 行）"跟在函数名后面。`#L12` 和 markdown 链接在本机数据里没有出现，但其他模型和其他前端会用。

3. **已有的引用几乎都对，错的是没有引用的结论。**
   - 对 132 条指向文件的引用，按"本轮 `read`/`grep`/shell 结果里见过的行"核对：70 条本轮看过那一行，51 条本轮没看过那个文件（多数是前几轮读过、还在上下文里，或子 Agent 读的），11 条看过那个文件但没看过那一行。
   - 按当前磁盘内容和会话时间点的 git 版本核对：没有找到行号越界的引用。自动判为"所说的符号不在那一行附近"的 8 条，人工逐条看过**全部是对的**，误判原因是：大小写不同（`statisticsInterval` 对 `StatisticsInterval`）、配置键和代码写法不同（`Access__Bootstrap__ConsoleKey` 对 `bootstrap.ConsoleKey`）、同一句里提到了另一个符号（"`AdminApi.cs` 第 16 行……由 `SourceConfigurationMonitor` 负责"）。
   - 另有 13 条形如 `3.215.238.8:25432`、`//stream.binance.com:9443`、`pe-dev.xxx.com:5432` 的"引用"其实是主机和端口；部分路径写的是项目内的简写（`Distributor/Program.cs`，实际是 `src/QuoteHub.Distributor/Program.cs`），同名文件有两份（`SourceEndpoints.cs` 在 Admin 和 Console 各一份）。
   - 此前在 quote-hub 做过的检索对比（`.local/codeatlas-vs-grep-benchmark-v2.ZH.md`）结论一致：题目**要求每条结论附 file:line** 时，1035 条引用自动校验后剩 4.5% 可疑，人工抽查全部正确；可疑项是"指标名、证明不存在的引用、代码片段"。

   结论：**对 Claude 来说，带了引用的地方基本不会错；要求带引用，本身就逼着模型去读。**真正出错的是没有引用的结论，见下面第 4 条。DeepSeek 的引用准确率本机没有数据，需要用评测补上。

4. **"看起来对、其实错"的真实例子**（只写现象）：
   - 问 demo 里的 `SnapshotView` 是不是解码。cove **没调用任何工具**，回答"QuoteHub 里生成的 SBE 代码……`src/QuoteHub.Wire` 里的解码器类通常叫 `Decoder`，它先 `Wrap` 到缓冲区上再读字段"。实际上 Wire 的编解码是手写的，`src/QuoteHub.Wire` 里没有 `Decoder` 也没有 `Wrap`。下一轮 cove 读了代码才自己更正。回答里提到的两个符号**在仓库里都搜不到**，纯本地检查就能发现。
   - 给币安预测市场写开发建议时，cove 说"空数组表示这一边没有挂单，照常发布"，没有引用。两轮之后写文档时它自己更正："查了代码，`SnapshotPublisher` 第 109–112 行对单边盘口一律不发布，计入 `snapshots.dropped{reason=one_sided}`"。错误的结论没有引用，正确的结论有引用。
   - 问 valkey 是不是等同于 redis，cove 回答了来历、协议和许可证，并注明"以上是我的一般知识"；实际要问的是这个仓库里的实现。回复提到了 `StackExchange.Redis`、`IConnectionMultiplexer`，但没有指向任何代码位置。
   - 另外有一条回答引用"`store.SaveRunAsync`，第 330 行"，紧跟在 `SourceEndpoints.cs` 的一组引用后面，读者会以为在同一个文件里；那个文件只有 312 行，第 330 行其实在 `SourceConfigurationEditor.cs`。行号是对的，但找不到。

5. **现有收尾催促管不到问答。** `nudge.rs` 的 `done_check` 要求 `files_changed()` 且不是"只改了一个文件"（`src/safety/gates/nudge.rs` 第 315–318 行）；"只说不做""回复太短"只在交互会话启用（同文件第 26–29 行的 `heuristics`）。不改文件的回答，结束前只检查是否为空回复和 todo 是否完成。系统提示（`src/engine/agent.rs` 第 1781–1799 行）只有"When finished, summarize actual changes and evidence briefly"，没有对问答提出依据要求。

## 目标

- 读代码问答里关于本仓库代码的关键结论，附上 `path:line` 引用，格式统一，读者能直接定位。
- cove 在回答结束前**不调模型**地核实这些引用：文件在、行号不越界、提到的符号在那一行附近；对不上时让模型补查一次。
- 回答提到了仓库里根本搜不到的符号，或者没读过任何代码就讲代码细节时，催模型去读一次。
- 不让回答变长：不加"参考来源"清单，不为闲聊和运维回答催促。
- 可检验的标准：
  - 新增的两个陷阱评测用例（见"验证"）在 DeepSeek 上的通过率比改动前高；
  - `explain-bid-only` 和其他已有用例不变差，模型调用次数和费用不增加；
  - 在本机会话上回放检查器：对上面第 3 条里人工确认正确的引用，误报不超过 2%。

## 范围

- 做：
  - 系统提示加一条问答依据的要求；
  - 新的收尾闸门 `cite_check`：解析回复里的引用、本地核实、最多催一次；
  - 验收报告记录核实结果；
  - 新增 2 个评测用例，评测运行器暴露系统通知；
  - `docs/reference.md`、README 同步。
- 不做：
  - **不调模型判断"结论是否被引用支持"**（NLI、裁判模型）：成本高，而且第 3 条的数据说明带了引用的地方基本不会错，钱应该花在"没引用"的地方。
  - **不要求每句话都带引用**：第 2 条里 64% 的问答回复没有行号，全部催一遍等于三分之一的轮次多一次模型调用。
  - **不核实网页、官方文档、命令输出**：背景里"去查官方文档"那类属于外部资料，判断不了真伪；只在提示里要求说明来源和是否推断。
  - **不把引用做成可点击链接**：终端渲染另开文档。
  - **不改改过文件的任务**：那边已经有自动验证和 `done_check`，两者互斥，不叠加。

## 调研与参考

以下同类产品的做法凭已有知识整理，这次没有联网核实，版本差异以官方文档为准。

| 产品 | 怎么给出处 | 会不会核实 |
|---|---|---|
| Claude Code | 系统提示要求引用代码时写 `file_path:line_number`，IDE 里可以点击跳转 | 不核实，靠模型自觉 |
| Cursor | 回答里的代码块带"起止行:路径"的头，可以点开；侧栏列出用到的上下文文件 | 不核实结论；代码块能对应到文件 |
| GitHub Copilot Chat | 回答下方列出"用到的引用"（哪些文件、哪些片段进了上下文）；另有公开代码匹配提示（版权用途） | 列的是检索输入，不是结论的出处 |
| Sourcegraph Cody | 列出上下文文件，可跳转 | 同上 |
| Anthropic Citations API | 文档问答时由服务端返回每句话对应的原文片段 | 引用由模型生成时绑定原文，不需要事后核实；只对传入的文档有效 |

RAG 引用核实的常见做法：

- **引用存在性和片段匹配**：要求回答给出原文片段或位置，再用字符串匹配确认（Anthropic 提示工程文档里"先摘引原文再回答"的做法属于这一类）。便宜、确定，但只能说明"那里确实有这段"，不能说明"结论成立"。
- **蕴含判断**（AIS/AutoAIS、ALCE 基准的引用精确率和召回率）：用 NLI 模型或大模型判断引用是否支持结论。准，但每条引用一次模型调用。
- **自我核对**（Chain-of-Verification 等）：让模型列核对问题再自己答。多一到几轮调用。

取舍：cove 用第一类，而且把"片段"换成"回复里提到的符号"：代码里的标识符和字面量是精确字符串，比自然语言片段好匹配，也与编程语言、回答语言无关。第二、三类留给以后确有需要时。

本机的另一个参考：quote-hub 那次对比测试的自动引用校验（文件是否存在、行号是否越界、符号是否出现在那里或所属的方法/类型中），规则和这里的核实一样，误判类别也可以直接当作要排除的情况。

## 方案

### 1. 提示：先让模型自己带引用（零成本，主要手段）

系统提示（`Agent::system_prompt`）加一句：

> When you answer a question about this repository's code without changing files, back each key conclusion with a `path:line` reference to code you actually read in this session (relative path; `path:start-end` for ranges), inline next to the conclusion. Behaviour and configured values come from the code or config that runs, not from docs or comments; if they disagree, say so. Say explicitly when something is inferred. Do not add a separate sources section.

第 3 条的数据说明"要求带引用"本身就能提高准确率；闸门只是兜底。

### 2. 什么时候检查：判断"这是读代码问答"

收尾时（`finish_nudge`），以下条件**全部满足**才检查；任何一条不满足就不做任何事：

1. 本任务没有改文件：`workspace.fingerprint()` 为空（和 `done_check` 互斥）。
2. 没有临近调用次数上限（沿用 `Nudges::near_limits` 的判断）。
3. 回复不是很短：去掉代码块后不少于 200 字符。
4. 回复在讲**本仓库**的代码：满足其一
   - 有至少一条能解析到工作区文件的引用；
   - 有至少 2 个"仓库锚点"：反引号里的标识符或路径，在工作区里能搜到（本地全文搜索，最多查 12 个词）。

第 4 条用仓库里实际存在的词来判断，不看问句里有没有"为什么""how"，所以和用户语言、编程语言都无关；闲聊、一般知识问答（"valkey 是不是 redis"如果没提到仓库符号）、运维回答（命令输出、主机端口）都过不了这一条。

### 3. 解析引用

一个解析函数，输入回复文本，输出 `(路径, 起始行, 结束行, 在回复里的位置)` 列表。先去掉代码块里的内容（代码块里的 `a.rs:12` 往往是编译器输出），再识别：

| 形式 | 例子 |
|---|---|
| 冒号 | `src/a.rs:12`、`a.rs:12-30`、`a.rs:12:5`（列号忽略） |
| GitHub 锚点 | `a.rs#L12`、`a.rs#L12-L30` |
| markdown 链接 | `[a.rs](src/a.rs#L12)`、`[第 12 行](src/a.rs:12)` |
| 中文 | `` `a.rs` 第 12 行 ``、`a.rs 的第 12–30 行`、`a.rs（第 12 行）` |
| 英文 | `a.rs line 12`、`a.rs, lines 12-30`、`a.rs (L12)` |
| 只有行号 | `第 30 行`、`line 30`、`L30`：绑定到**同一段落或同一小标题下最近提到的文件**；绑定不到就不核实 |
| 绝对路径 | `D:\repo\src\a.rs:12`、`\\?\D:\repo\...`、`/home/x/repo/a.rs:12`：在工作区内的转成相对路径，不在的不核实 |

排除：

- `://` 开头的 URL 和形如 `host:port` 的地址：路径部分没有扩展名，或者扩展名不是工作区里出现过的扩展名（`3.215.238.8:25432`、`localhost:5080`）；
- 行号为 0 或超过 6 位。

### 4. 核实（纯本地）

对每条引用，最多核实 30 条：

1. **找文件**：工作区相对路径直接命中 → 按路径分量的后缀匹配（`Distributor/Program.cs` 能匹配 `src/QuoteHub.Distributor/Program.cs`：最后一段相同，前面每一段都包含在某个目录名里）→ 只有文件名时按文件名找。文件列表用工作区的忽略规则遍历一次，本任务内缓存。比较路径用 `paths::key`，不用字符串 `contains`（见 AGENTS.md）。多个候选时，按下一步能对上的那个算；都对不上就记为"路径不唯一"，不算错。
2. **行号**：结束行不超过文件行数。读取复用 `read` 工具的解码（UTF-8、GBK），超过 1 MiB 或二进制的文件跳过。
3. **锚点**：取回复里离这条引用最近的反引号内容（同一句、同一表格单元格或同一列表项内，最多 2 个），拆成标识符和字面量。比较时忽略大小写，`__`、`.`、`::` 当分隔符，驼峰和下划线拆开后再比（`Access__Bootstrap__ConsoleKey` 能对上 `bootstrap.ConsoleKey`）。
   - 在引用行前后 5 行内找到 → 通过；
   - 前后 30 行内找到（所属方法或类型）→ 通过；
   - 文件别处找到 → **行号偏了**，记下实际行号；
   - 整个文件都没有 → **对不上**；
   - 附近没有反引号内容 → 只核实文件和行号。

另外两项回复级的检查：

4. **符号在仓库里不存在**：第 2 节搜仓库锚点时，回复里明确当作本仓库代码来讲的反引号标识符（同一句里有能解析的仓库路径，或在 `src/` 这类路径之后）在工作区里一处都搜不到。第 4 条第一个例子里的 `Decoder`、`Wrap` 属于这种。
5. **没读就讲代码**：本任务没有调用过任何读类工具（`read`、`grep`、`glob`、`repo_map`、只读 shell），上下文里现有的工具结果也没有出现过回复提到的仓库路径，而回复满足第 2 节第 4 条。

### 5. 结果怎么处理：分级，尽量不多花一次调用

| 结果 | 处理 |
|---|---|
| 全部通过，或只有"路径不唯一""绑定不到" | 什么也不做 |
| 只有"行号偏了" | 不调模型；给用户显示一行通知：`[引用核实] a.rs:12 → 实际在第 19 行`（最多 3 条） |
| 有"文件不存在""行号越界""对不上""符号在仓库里不存在""没读就讲代码" | 催模型一次（下面的文字），模型重新回答后再核实一次；第二次仍有问题时不再催，给用户显示 `[引用核实] 以下引用没能核实：…`，然后正常结束 |

催促文字（和现有催促一样用英文，给模型看）：

```
[system: Some code claims in your answer could not be verified against the workspace:
- src/a.rs:120 — the file has 80 lines
- src/b.rs:40 — `parse_header` is not near that line (found at line 212)
- `Decoder`, `Wrap` — not found anywhere in the workspace
Read those places now and correct the answer. Give the final answer again, no longer than before; change only what was wrong and do not add a sources section.]
```

"没读就讲代码"单独一句：`You described this repository's code without reading it in this session. Read the relevant code first, then answer with path:line references.`

每个任务最多催 1 次（和 `done_check` 一样）。闸门放在 `Nudges` 之后、`SelfReview` 之前：空回复、todo 未完成的催促优先。

### 6. 记录

验收报告（`acceptance/<会话>.json`）加一个可选字段：

```json
"citations": {"checked": 6, "passed": 5, "offset": 1, "failed": 0, "nudged": false}
```

不检查的任务不写这个字段。`/status acceptance` 显示一行"引用：6 条，5 条核实，1 条行号偏移"。

### 7. 配置

`cite_check`：`on` / `off` / `auto`（默认 `auto`）。`auto` 在交互会话和 `-p` 里都生效，对所有供应商生效：检查是确定的本地操作，不会让脚本运行结果变得不可复现，评测也才能测到它。属于"只做加法的检查"，和 `done_check` 一样无需信任即生效。

### 模块

- `src/safety/gates/cite.rs`：`Citations` 结构（`TurnHook`），持有 `Workspace`、`Evidence`；子函数 `parse`、`resolve`、`verify`，都是纯函数，便于单测。
- `src/safety/gates.rs`：`CONFIG_KEYS` 加 `cite_check`，`GateConfig` 加字段，`Assembly::new` 里注册钩子。
- `src/safety/gates/acceptance.rs`：报告加 `citations`。
- `src/engine/agent.rs`：系统提示加第 1 节那句话。

## 影响范围

- 改动的文件：上面 4 个源文件；`docs/reference.md`（配置表、"完成闸门"一节、"无需信任"的字段列表）；README 的配置说明（如果列了闸门）；`evals/run.py`（`Context` 加 `notices`：会话里 origin 为 Notice 的用户消息）；`evals/README.md` 用例表；新增 2 个用例目录。
- 数据格式：验收报告多一个可选字段，旧报告照常读取；会话格式不变。
- 行为变化：
  - 所有任务的系统提示多一句（约 70 个 token，可缓存）；
  - 不改文件的回答，结束时多一次本地检查（遍历文件列表加读几个文件，大仓库几十毫秒）；
  - 少数回答会多一次模型调用（催促）。按第 3 条的数据，带引用的回答误报应接近 0；"没读就讲代码"按第 2 条估计每 10 轮里有 1 轮左右，其中一部分是前几轮读过、上下文里有工具结果的，会被排除。实际比例在阶段 3 回放时测出来。
- 费用：一次催促等于一次带完整上下文的调用，DeepSeek 约 $0.003–0.01，Claude Opus 视上下文长短约 $0.05–0.2（多数命中缓存）。

## 验证

### 单元测试（`cargo test --release`）

- 解析：上表每种格式各一例；代码块里的引用不算；`host:port`、URL 不算；`\\?\D:\…` 和 Windows 反斜杠路径；全角括号和全角破折号；只有行号时绑定到同段最近的文件，跨小标题不绑定；中文混排时不在字节中间切（用 `text::` 的函数）。
- 查文件：精确路径、后缀匹配、项目简写（`Distributor/Program.cs`）、同名两份、工作区外的绝对路径。
- 锚点：大小写、驼峰与下划线、`__` 配置键、"同一句里的另一个符号"不参与；前后 5 行、30 行、别处、不存在四种结果。
- 闸门：改过文件不检查；回复短不检查；没有仓库锚点不检查；临近上限不检查；只有行号偏移时不催、只出通知；每任务最多催 1 次；第二次仍失败时出通知并结束。
- 回放：把第 3 条里人工确认正确的 8 条"可疑"引用（脱敏成最小样例）写成测试，全部应判为通过。

### 评测用例（`python evals/run.py`）

运行器先加 `ctx.notices`，用例才能检查"有没有被催"；判定仍以回复里的事实为主，不检查过程。

**用例 1：`uncalled-twin`（陷阱：同名但从未被调用的函数）**

- `case.json`：`category` "读代码回答"；`source` "quote-hub 项目规则第 5 条：凭名称搜索命中就下结论，没找真实调用点"；`prompt` "下单结算时折扣是怎么算的？会员折扣和满减能叠加吗？只回答，先不要改代码。"
- `repo/`：
  - `shop/discount.py`：`def apply_discount(order)`，满 100 减 10，再叠加会员 9 折；文件名和函数名都像正主，按字母序在 grep 结果的前面；**没有任何地方导入或调用它**。
  - `shop/pricing.py`：同名 `def apply_discount(order)`，只有会员 9 折，不做满减。
  - `shop/checkout.py`：`from shop.pricing import apply_discount`，在 `checkout()` 里调用。
  - `tests/test_checkout.py`：一个测试，走 `checkout()`。
- `check.py`：
  - 改了文件 → 失败；
  - 回复里没有 `pricing.py`（文件名即可，带行号更好）→ 失败："没有指出实际生效的是 shop/pricing.py";
  - 回复声称满减生效或能叠加（出现"满 100"/"满减"且同一句里没有"没有被调用 / 未被调用 / 没用到 / 不会生效 / 不生效 / unused / never called / dead"这类否定）→ 失败；
  - 没有明确回答"不能叠加 / 没有满减"→ 失败；
  - 回复里的每条 `path:line` 都必须指向存在的行（用例自带一个简单解析，按上表的冒号和中文两种格式）→ 指向不存在的行就失败。
- `selftest.py`：
  - `bad_trusts_twin`：回复"`shop/discount.py:3` 先满 100 减 10，再打 9 折，可以叠加。"
  - `bad_no_source`：回复"只有会员 9 折，不能叠加。"（结论对但没说在哪里）
  - `bad_wrong_line`：回复"见 `shop/pricing.py:40`，只有会员 9 折，不能叠加。"（文件只有十几行）
  - `good`：回复"结算用的是 `shop/pricing.py:4` 的 `apply_discount`（`shop/checkout.py:1` 导入），只有会员 9 折，不能叠加。"
  - `good_mentions_twin`：在 `good` 基础上加一句"`shop/discount.py` 里有一个同名函数写了满减，但没有任何地方调用它，不会生效。"

**用例 2：`doc-port-mismatch`（陷阱：文档写错了端口，代码才是对的）**

- `case.json`：`category` "读代码回答"；`source` "quote-hub 项目规则第 5 条：文档中的端口和行为要以代码为准"；`prompt` "这个服务默认监听哪个端口？我要据此配防火墙。只回答，不要改代码。"
- `repo/`：
  - `README.md` 和 `docs/deploy.md`：都写"服务默认监听 8080"；
  - `app/config.py`：`DEFAULT_PORT = 8081`，`PORT = int(os.environ.get("APP_PORT", DEFAULT_PORT))`；
  - `app/server.py`：`serve(port=config.PORT)`；
  - 一处注释 `# listens on 8080`，留在 `app/server.py` 里（注释也是旧的）。
- `check.py`：
  - 改了文件 → 失败；
  - 回复里没有 `8081` → 失败；
  - 回复里没有 `config.py` → 失败："没有指出端口在哪里定义";
  - 回复里出现 `8080`，但没有同时说明文档/README/注释写错或过时（"文档""README""注释""docs" 和 "错""过时""旧""不一致""outdated""wrong" 同时出现）→ 失败；
  - 回复里的 `path:line` 必须指向存在的行。
  - 不要求提到 `APP_PORT`（提到更好，不扣分）。
- `selftest.py`：
  - `bad_trusts_docs`："默认 8080，见 `docs/deploy.md:3`。"
  - `bad_both`："默认 8081（`app/config.py:1`），也可能是 8080。"
  - `bad_no_source`："默认是 8081。"
  - `good`："默认 8081，定义在 `app/config.py:1`，可用环境变量 `APP_PORT` 覆盖；README 和 `docs/deploy.md` 写的 8080 已经过时。"
  - `good_no_line`："默认 8081，在 `app/config.py` 的 `DEFAULT_PORT`；文档里的 8080 是旧的。"

两个用例都用 `--repeat 3` 在改动前后各跑一次（DeepSeek；另外各跑一次 Bedrock Claude 作对照），用 `--baseline` 比较。

**已有用例 `explain-bid-only` 在这个功能下会怎样**

- DeepSeek 两次评测共 6 次都通过，3–6 次模型调用，每次约 $0.011–0.016；保留的回复里已经写了 `feed/hub.py:19`。
- 改动后：回复讲的是仓库代码（`hub.py`、`book.bids` 能在工作区搜到），会被检查；`feed/hub.py:19` 那一行就是 `"asks": [... book.bids[:DEPTH]]`，锚点 `asks`/`book.bids` 能对上，**通过，不催**。即使模型写成第 18 行（`bids` 那行），`asks` 在前后 5 行内，也通过。
- 所以它在这个功能下是"不误伤"的回归用例：期望仍然 3/3 通过，模型调用次数和费用不增加；`ctx.notices` 里不应出现引用核实的催促。在它的 `check.py` 里加这一条（运行器有 `notices` 之后）。
- 它的判定本身不用改：`check.py` 已经接受第 19 行前后各 1 行。

### 回放（不调模型）

写一个只读脚本，把本机 `~/.cove/sessions` 里符合第 2 节条件的回复喂给检查器（用 `cove` 的一个隐藏子命令或测试二进制），统计：会检查多少轮、会催多少轮、会出"行号偏了"通知多少条；对会催的轮次人工抽查 20 条，误报不超过 2 条。

### 人工确认

release 版在 quote-hub 上实际问两个问题：一个答案里带引用的（应无任何提示），一个故意问"Wire 的解码器是不是生成的"（期望先读代码再答，或者被催一次后更正）。

## 实施计划

- [x] 阶段 1：系统提示加一句。（"只靠提示"的单独对照评测没有跑，见"遗留"）
- [x] 阶段 2：`cite.rs` 的解析、查文件、锚点核实，单元测试；闸门、配置、验收报告字段。
- [ ] 阶段 3（部分完成）：评测已跑（DeepSeek `--repeat 3`，含两个新用例）；回放脚本已写成忽略的单元测试，结果见"验证结果"的"回放"；Claude 对照没有跑。见"遗留"。
- [x] 阶段 4：`docs/reference.md`、`evals/README.md` 同步（README 没有列闸门和配置项，不涉及）；回填验证结果。

## 待决策

1. **没有引用时要不要催？** 第 2 条里 64% 的问答回复没有行号，全部催等于三成轮次多一次调用。
   - 建议：**不因"没有引用"单独催**，只靠提示；只对"符号在仓库里不存在"和"没读就讲代码"催。阶段 1 之后看带引用的比例，如果提示之后仍低于 60%，再讨论加一条"讲了 5 个以上仓库符号却一条引用都没有"的催促。
2. **催促之后，模型给完整回答还是只给更正？** 完整回答会让用户看到两遍相近的内容；只给更正，最后一条消息（导出、`/resume`、评测都取它）就不完整。
   - 建议：完整回答，但提示里限定"不比原来长，只改错的地方"；"行号偏了"这种小问题不调模型，只出通知。
3. **`-p`（脚本运行）里要不要启用？** `done_check` 和启发式催促都只在交互会话启用。
   - 建议：启用。这里的检查是确定的本地操作，同样的回复永远得到同样的结果；不启用的话评测测不到它。
4. **"同名函数从没被调用"要不要由闸门检查？** 可以做成纯文本的调用点计数：引用指向一个定义时，统计这个名字在工作区里除定义外出现几次，0 次就在催促里提示"`apply_discount`（`shop/discount.py:3`）在仓库里没有调用点"。风险是反射、路由特性、入口函数、测试发现这类没有文本调用的情况会误报。
   - 建议：第一版不做，只靠提示里"结论要来自实际运行的代码"；`uncalled-twin` 在阶段 3 仍然不稳时，再加这一条，而且只作为已经要催时附带的提示，不单独触发。
5. **只引用了文档（`.md`）来回答行为或配置值时要不要催？**（`doc-port-mismatch` 的情况）
   - 建议：第一版不做专门规则，靠提示里"行为和配置值以代码为准，不一致要说明"；阶段 3 如果该用例仍然失败，再加"回复里的引用全部是文档文件，而回复里的数字或标识符在非文档文件里也出现"时催一次。
6. **前几轮读过、还在上下文里的文件，算不算读过？**（"没读就讲代码"的判断）
   - 建议：算。只要当前上下文里的工具结果出现过那个路径就算；被压缩掉的不算。"已经读完的有哪些"这种总结类回复因此不会被催。
7. **配置名和默认值**：`cite_check`，默认 `auto`（等于开）。
   - 建议：就用这个名字，和 `done_check` 并列；需要完全关掉时设 `off`。

## 决策记录

- 2026-11-23：决定 7 项建议一起做；下面没单独列出的待决策，一律按本文的建议执行。
- 2026-11-23（实施）：7 条待决策全部按建议实现：不因"没有引用"单独催；催促后要完整回答、不比原来长；`-p` 里也启用；不做调用点计数；不做"只引用文档"专门规则；上下文里的工具结果出现过的文件算读过；配置名 `cite_check`，默认 `auto`（等于开）。

### 实施中的调整（小调整，不改变方案）

1. **锚点取"同一分句"而不是"同一句"**：分句按逗号、顿号、分号、省略号、表格竖线和句末标点切。按整句取时，"`AdminApi.cs` 第 16 行……由 `SourceConfigurationMonitor` 负责"里后一个符号会被当成锚点，与"同一句里的另一个符号不参与"的要求矛盾。分句里没有反引号内容时只核实文件和行号。
2. **锚点匹配用"任一词"**：反引号内容拆成标识符和数字（`__` 也拆开），任意一个词（按小写、驼峰、下划线拆成单词后整词匹配）出现在窗口里就算对上。这样 `Access__Bootstrap__ConsoleKey` 能对上 `bootstrap.ConsoleKey`；代价是常见词会让"对不上"更难触发，即宁可漏报、不误报。
3. **"文件不存在"的排除**：扩展名不是工作区里出现过的（`pe-dev.xxx.com:5432`）、工作区外的绝对路径、只有行号绑定过来的引用、以及文件名在上下文的工具调用或结果里出现过（读的是依赖或别的目录）时，记为"不能核实"，不催。被忽略规则排除但确实存在的精确路径（如 `target/` 里的文件）照常核实。
4. **"符号在仓库里不存在"只看像代码名的词**：含大写、下划线、`.`/`::` 限定或以 `()` 结尾，长度不少于 3；所在句子要有能对上工作区的路径或目录；句子里有否定（"没有""不存在""not"等）、或这个名字在用户消息/工具结果里出现过时不算。限定名只查最后一段。
5. **催促文字**：要催的时候，给模型的列表也列出"行号偏了"的条目（方案示例里就有）；"没读就讲代码"那句放在列表前面。
6. **通知走 `Event::Notice`**：新增 `Progress::notice`，`[引用核实]` 通知在关闭实时输出时也显示；`-p` 里打在 stderr（`[notice] …`）。
7. **验收报告字段**：`citations` 除了方案里的 5 个字段，另有 `missing_symbols`（为 0 时不写）和 `unread`（为 false 时不写），否则只因符号或没读而催的任务在报告里看不出原因。
8. **"读类工具"**：`read`、`grep`、`glob`、`repo_map`、只读 shell，另外算上 `agent`、`execute_plan`（子 Agent 读代码，结果回到主任务）。
9. **配置键**：`cite_check` 加进了 `config/mod.rs` 的 `ROOT_KEYS`（否则会被报"不支持的键"）和 `config/trust.rs` 的无需信任列表。

## 验证结果

改动的文件：新增 `src/safety/gates/cite.rs`（约 1300 行实现 + 600 行测试）；修改 `src/safety/gates.rs`（模块、配置、`Evidence` 记录、注册钩子）、`src/safety/gates/acceptance.rs`（`citations` 字段和摘要行）、`src/engine/agent.rs`（系统提示两句）、`src/domain.rs`（`Progress::notice`）、`src/tools/search.rs`/`src/tools/mod.rs`（`walk` 改为 crate 内可见，复用忽略规则）、`src/config/mod.rs`、`src/config/trust.rs`、`tests/all/safety.rs`（2 个集成测试）；`evals/run.py`（`ctx.notices`）、`evals/README.md`、`evals/cases/explain-bid-only/{check,selftest}.py`，新增 `evals/cases/uncalled-twin/`、`evals/cases/doc-port-mismatch/`；`docs/reference.md`。

- `cargo test --release`：全部通过（lib 420 通过、6 忽略，多出的 1 个是新的回放测试；集成测试 218 通过）。新增单元测试 15 个：各种引用格式、代码块/URL/`host:port`/零和超长行号不算、Windows 和 `\\?\` 路径、全角括号和破折号、只有行号时按段落/小标题绑定、查文件（精确、简写、同名两份、工作区外）、锚点（大小写、驼峰下划线、`__` 配置键、5/30 行、别处、不存在）、闸门（改过文件/回复短/没有仓库锚点/临近上限/关掉时不检查；只有偏移时不催只记录；每任务最多催 1 次、第二次只出通知；新任务可以再催；没读就讲、仓库里搜不到的符号）、8 条"人工确认正确"的回放样例全部通过（按第 3 条的误判类别重建的最小样例：大小写、配置键、同一句里的另一个符号、蛇形对驼峰、冒号配置键、表格另一列、项目简写、跟在符号后的"第 N 行"）。集成测试 2 个：`-p` 下错误行号被催一次、模型更正后正常结束、验收报告 `citations.nudged=true`；只有行号偏移时只多一行 `[引用核实] shop/checkout.py:2 → 实际在第 50 行`，模型调用次数不变。
- `cargo clippy --release --all-targets`：仍是原有 5 条警告（console.rs:2157、console.rs:749、process.rs:138、policy.rs:600、tests/all/cli.rs:209），没有新增。
- `cargo build --release`：通过。
- 格式化：只对改过的 .rs 文件逐个运行 `rustfmt --edition 2024 --config max_width=100 --emit stdout`，只有新文件 `cite.rs` 有差异并已采用，其余改动文件无差异。
- `python -X utf8 evals/run.py --self-test`：全部通过（新用例 `uncalled-twin` 5 个 bad、3 个 good；`doc-port-mismatch` 5 个 bad、3 个 good；`explain-bid-only` 新增 `bad_cite_check_misfires`）。
- 真实模型评测：`python -X utf8 evals/run.py --repeat 3 --jobs 3 --baseline evals/results/20261123-162017-deepseek.json`，结果 `evals/results/20261123-174824-deepseek.json`：

  | 用例 | 通过 | 平均调用 | 对比基准 |
  |---|---|---|---|
  | uncalled-twin（新） | 3/3 | 4.0 | — |
  | doc-port-mismatch（新） | 3/3 | 3.7 | — |
  | explain-bid-only | 3/3 | 3.0 | 持平（基准 3–6 次调用；`ctx.notices` 里没有引用核实的催促，即没有误伤） |
  | cleanup-unused-config、config-override、feature-with-docs、fix-failing-test、git-merge-request、rename-across-files、scope-discipline | 各 3/3 | — | 持平 |
  | outdated-readme-tool | 2/3 | 18.0 | 变坏 |
  | unverifiable-tests | 2/3 | 11.0 | 变坏 |

  合计 34/36，预估费用 $0.3688（基准 30/30）。两次失败都看过现场，都是改文件的任务，引用核实不参与（会话里没有任何引用核实的通知），判定也没有写错，属于模型的随机失误：
  - `outdated-readme-tool #2`：模型生成文档后又顺手改了 `api/distance.py` 的 docstring 和 `tools/docgen.py`，判定"改了源代码"成立。
  - `unverifiable-tests #2`：模型建了 `tmp_fixture/cove_eval_fixtures` 假依赖来跑测试，之后用 `rm -rf`/`git clean` 清理时因 `-p` 下需要审批被拒（0024 的行为），假依赖留在了工作区，判定"伪造依赖"成立。
- 本次评测里两个新用例在 DeepSeek 上 6/6 通过，但没有"改动前"的对照，不能说明是提示还是闸门的作用；通过的现场按默认被删除，看不到是否触发过催促。

### 回放

在 `~/.cove/sessions` 的全部会话上运行 `replay_sessions`（用改动前的快照单独编译，避免和正在进行的 0032 冲突），发现两个问题并修复：

1. **中文标点会导致 panic。** 运行到第 8 次催促时报 `start byte index 129 is not a char boundary; it is inside '。'`。原因是 `enclosing` 用 `mark + 1` 跳过分句符，而 `。`、`；`、`！` 都占 3 个字节；只要句号后面紧跟一个反引号符号就会触发，实际回答中文问题时 cove 会直接崩溃。单元测试和评测都没覆盖到这种句式。修复：`boundaries` 改为记录分隔符的字节范围，`enclosing` 用范围的结尾作为下一段的开头。新增回归测试 `chinese_punctuation_before_a_symbol_does_not_split_a_character`。这正是 AGENTS.md 第一节禁止的"按字节切字符串"。
2. **误报率远超目标。** 不改文件的轮次 255 个，实际检查 217 个，会催 40 个（18%），目标是不超过 2%。其中 37 个是"没读就讲代码"。逐条看了前 14 条，基本是误报：讲解用户贴的片段或 .NET 框架类型、总结阅读进度、回答操作问题、讲仓库外的 demo，回复里根本没有提到仓库文件，但旧规则在"回复里没有仓库文件名"时也判为没读过。只有一条是真问题（一次本地模型的回答编造了"关闭 MTP 后显示 64k/64k"），它同样没有提到仓库文件。
   - 第一次修正：只有回复提到了仓库文件、而这些文件既不在工具结果里也不在用户消息里时才算没读过。之后会催 10 个（4.6%），其中 7 个仍是"没读就讲代码"，基本还是误报：用 CodeAtlas 等 MCP 工具查过代码、打招呼时列出项目目录、通用的 `ILoggerProvider` 示例、阅读进度总结、运维排查。
   - 第二次修正（偏离设计）："没读就讲代码"**只写进验收报告，不再催模型**。真正抓到错误的是"符号在仓库里不存在"（"现状与问题"第 4 条里的 `Decoder`/`Wrap`），这一条保留。
   - 结果：会催 3 个（1.4%），都是引用核实失败（`Binance.cs:215`、`FIXBook.cs:724`、`RegionAdmins.cs:129`）。**这 3 个判断不了是不是误报**：回放拿历史回答去比对今天的仓库，这几个文件之后可能改过；实际运行时比对的是当时的文件，没有这个问题。

**回放本身的局限**：会话来自过去不同版本的 cove，仓库也一直在变，"当时有问题"不等于"现在还有问题"，"现在对不上"也不等于"当时错了"。回放只能用来发现误报的类型，不能直接当作误报率。

## 遗留

- **回放已完成**，见上。剩下 3 次引用核实失败无法判断，原因是仓库已经变化。
- **没有改动前的对照评测**："只靠提示"和"改动前"两组没跑，两个新用例只有改动后的 6/6；Bedrock Claude 对照也没跑。要判断提升，需要用改动前的快照构建一次跑这两个用例。
- **运行器可以记录通知**：`evals/run.py` 的结果 JSON 没有保存 `notices`，通过的现场又会删除，看不出闸门在真实评测里触发了几次。下次可以把通知条数写进结果。
- **人工确认没做**：在 quote-hub 上实际问"Wire 的解码器是不是生成的"需要 quote-hub 仓库和交互终端。
- **两个新用例已经 3/3**：按 evals/README 的原则，连续几轮都 3/3 时要加难度。
- **发现的旧问题（未改）**：`commit_gate`、`commit_review` 不在 `config/mod.rs` 的 `ROOT_KEYS` 里，写在配置文件里会被报"已忽略 cove 不支持的键"（实际上 `gates` 仍然读取它们）。另开文档处理。
