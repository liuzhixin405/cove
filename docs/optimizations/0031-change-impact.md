# 0031 改动影响说明：列出改动符号的调用方和没有测试引用的调用方

- 状态：已完成
- 日期：2026-11-23（创建）；2026-11-24（完成）
- 关联：0023（真实模型评测）、0025（.NET 测试项目选择）、0027（回答带引用）、0028（收尾审查循环）、0032（按进展止损）；`src/safety/impact.rs`、`src/safety/gates/impact.rs`、`src/tools/repomap.rs`、`src/safety/verify.rs`、`src/safety/gates/review.rs`、`src/safety/gates/acceptance.rs`

## 背景

审阅一批改动时，最关心的是"会不会把已有功能弄挂"：

- 审阅一个 MR 时，先后问过新增功能对已有功能影响大不大、会不会让已有功能挂掉，`UnheardHint` 加来做什么、同批次的提交改了什么，并要求代码不能出现导致服务挂掉或业务出错的死锁和逻辑错误。
- 发现 Console 的代理白名单和 Admin 的路由各写一份路径，担心改动一处会导致访问不了。

cove 每次都答得不错，但都是**被追问之后**，模型自己用 `git diff` 和 `grep` 一点点查出来的。cove 自己改完代码收尾时，从不主动说明"这次改的函数还有谁在用、那些地方有没有测试"。这份文档设计一个纯本地、确定性的"改动影响"列表：从改动里找出改了的符号，按名字找跨文件的调用方，标出哪些调用方没有任何测试引用，给用户看，必要时交给模型核对一次。

## 现状与问题

数据来自本机 `~/.cove/sessions/*.json`（122 个会话，322 个用户发起的轮次）和 `~/.cove/acceptance/*.json`（62 份），分析脚本放在 `%TEMP%\cove0031\`，不进仓库。

**P1 问影响的频率不高，但都出现在高风险的场景。** 按"影响/会不会挂/同批次/访问不了"等关键词筛出 8 轮，人工去掉误报（会话摘要里的"影响"、"看完了还是影响不深刻"）后剩 6 轮，分布在 2 个会话（一个 4 轮，一个 2 轮），约占全部用户轮次的 2%。两个会话都是 .NET（C#）仓库，问题都是**审阅已有提交或已有代码**（`origin/main...HEAD` 的 MR、两处重复的路由），不是问 cove 自己刚做的改动。

**P2 当时 cove 怎么回答。** 6 轮里模型共调用 `shell` 12 次、`grep` 3 次、`read` 1 次，**一次也没用 `repo_map`**：先 `git diff --stat origin/main...HEAD`，再逐个文件看 diff，用 `grep -rn 名字` 找使用处，审阅 MR 的那次最后跑了一次全量 `dotnet test`。结论质量高（按文件列出"几乎不碰其他交易所的部分""共用的 `WebSocketFeed` 风险最大"），但每次都要先被问到，而且找调用方全靠模型想到哪个名字就 grep 哪个。全部会话里 `repo_map` 只被调用过 8 次（`shell` 720 次、`grep` 341 次）。

**P3 cove 自己改代码时不说影响。** 按任务切分后，有 16 个任务用 `edit`/`write` 改了源码文件；最终回复里提到"调用方/影响/其他地方"的只有 2 个，提到测试的有 11 个。也就是说，收尾总结通常说"改了什么、测试过没有"，不说"还有谁依赖这里"。

**P4 验收报告不记录改动范围。** 62 份验收报告（`src/safety/gates/acceptance.rs:34` 的 `Report`）只有 1 份有检查记录（`dotnet build`、`dotnet test` 都通过），其余 61 份 `checks` 为空、`verification` 缺省。报告里有 `git.uncommitted_files`（计数），没有改了哪些文件、哪些符号，更没有调用方。

**P5 自审要求查调用方，但没给它调用方。** `src/safety/gates/review.rs:32` 的 `CRITERIA` 写着"callers or tests the change breaks"，可是自审（`done_self_review`，默认 off）只拿到本轮 diff（最多 40 KB，`review.rs:174`），调用方要审阅子 Agent 自己去 grep，受 `subagent_max_turns` 限制，常常查不全。

**与当前版本的核对（2026-11-24，实施前）。** P1–P3 的数据来自 0026–0032 完成之前的本机会话，P4 的报告也早于 0027、0028。实施前对照当前代码逐条核对：

- P1、P2（审阅 MR 时靠被追问后 `git diff` + `grep` 找调用方）、P3（收尾不说影响）：会话数据是历史现象，未在当前版本复现。当前代码里没有任何机制会主动给出调用方：`src/engine/prompt.rs` 不要求说明调用方；`repo_map`（`src/tools/repomap.rs:357` `query`）只按查询词列定义和相关文件，没有"谁调用它"的输出。机制层面的缺口仍在。
- P4：成立。`Report`（`src/safety/gates/acceptance.rs:35`）此后加了 `citations`（0027）和 `review`（0028），仍然没有改动文件、符号或调用方。
- P5：接入点已变。0028 删除了 `SelfReview`，内置自审现在是 `DoneReview` 里的 `DoneReviewer::Builtin`（`src/safety/gates/review.rs:389`），由 agent 循环在自动验证通过后调用（`src/engine/agent.rs:2098`），不再是 `TurnHook` 列表里的一项；它仍然只拿到本轮 diff（`MAX_DIFF` 40 KB，`review.rs:40`），`CRITERIA`（`review.rs:47`）仍写着 "callers or tests the change breaks"。所以"附 `<callers>`"改为接到 `DoneReview` 的内置分支上。
- 收尾的结构：模型 `finish` 时先依次跑各 hook 的 `finish_nudge`（`agent.rs:1979`，第一个 `Some` 生效），然后自动验证（`agent.rs:2028`），最后收尾审查（`agent.rs:2098`）。新 hook 用 `finish_nudge` 实现、注册在 `Nudges` 之后，就自然排在"补完需求"之后、"验证和审查"之前，和草稿要的顺序一致。`-p` 下 `Event::Notice` 打到 stderr（`src/app/ui.rs:238`），`[summary]` 也在 stderr。
- P6：成立。`symbols()`（:200）、`IDENTIFIER`（:353）、`references()`（:509）、`definitions()`（:522）、`COMMON_NAME_FILES`（:21）、`MAX_FILES`（:19）都和草稿一致，仍是按行正则、没有结束行；C#/Java 方法正则仍要求至少一个修饰符（新单元测试 `csharp_properties_and_interface_members_are_not_symbols` 固定了这条漏报）。`verify::test_commands`（`src/safety/verify.rs:166`）未变。

**P6 现有部件能用，但都不是为"调用方"设计的。**

- `src/tools/repomap.rs`：**基于正则，不是 tree-sitter**。`symbols()`（:200）按语言的行正则抽定义（Rust、Go、Python、TS/JS、C#、Java，见 :24 `LANGUAGES`），只有起始行，没有结束行；"引用"是整个文件里所有长度 ≥3 的标识符的集合（:353 `IDENTIFIER`），没有行号，不区分作用域、类型、注释和字符串。`references()`（:509）是"名字 → 用到它的文件"的倒排表；`definitions()`（:522）丢掉在超过 8 个文件里都有定义的名字（:21 `COMMON_NAME_FILES`）。扫描上限 5000 个源文件（:19），单文件 1 MiB，按修改时间和大小缓存解析结果。
- 已知漏报：C# / Java 的方法正则（:158 `C_FAMILY`）要求至少一个修饰符，接口成员（`Task RunAsync();`）抽不到；属性和表达式体成员（背景里那个 MR 的 `string? UnheardHint => null;`）没有括号，也抽不到；JS 里没导出的 `const f = () => …` 抽不到。字符串耦合（背景里的路由路径）根本不是符号，任何按名字的方法都查不到。
- 已知误报：同名不同类型的方法（`Load`、`Parse`）会互相算作调用方；注释、字符串、日志文字里出现名字也算。
- `src/safety/verify.rs:166` `test_commands()` 选的是**包、项目或模块级**的测试命令：cargo 按 workspace 成员，go 按目录，dotnet 按引用了改动项目的测试项目（:428），Java 按 Maven/Gradle 模块（:552），pytest 按 `test_<模块>.py` 文件名（:383），jest 用 `--findRelatedTests`（:350，唯一按依赖图的）。它能说"这些测试和改动在同一个包"，不能说"这个调用方被测到了"。

## 目标

1. 一次任务改了源码，并且改动达到阈值时，收尾后用户能看到一段简短的"影响"说明：改了哪几个符号（删除、签名变化、函数体变化），它们在别的文件被谁使用（文件:行），其中哪些调用方**没有任何测试文件引用**。
2. 删除或改了签名的符号还有没被测试引用的调用方时，把这份列表交给模型一次，让它核对这些调用方是否仍然正确、需要时补测试，并在最终回复里说明；模型不必为此扩大改动范围。
3. 开了自审时，审阅子 Agent 直接拿到这份列表，不再自己从头找调用方。
4. 用户可以对任意 git 区间（例如 `origin/main...HEAD`）手动查影响，覆盖 P1 里"审阅 MR"的真实用法。
5. 全部本地计算，不调用模型；在 5000 个源文件的仓库里一次计算不超过 2 秒，超出上限时如实说"只查了一部分"。
6. 说明里写清"按名字匹配，可能误报和漏报"，不给"没有风险"这种结论。

可检验：新增的单元测试和两个评测用例（见"验证"）通过；cove 仓库自身上跑 `/changes impact HEAD~1` 输出不超过 30 行、耗时 < 2 s。

## 范围

- 做：
  - 新模块计算改动影响：改动符号 → 跨文件调用方（带行号）→ 是否有测试文件引用。
  - 收尾时给用户显示；满足条件时交模型一次（新的 `TurnHook`）。
  - 自审的任务文本附上这份列表。
  - `/changes impact [turn | <git 区间>]` 手动查询。
  - 验收报告增加影响摘要字段。
  - 配置项 `change_impact`（off/on/auto）。
- 不做：
  - **不引入 tree-sitter 或语言服务器（LSP）**：体积和编译时间成本大，跨 6 种语言要维护 6 套语法；先用现有正则索引验证这个功能有没有用，精度不够再另开文档。
  - **不做真正的覆盖率**（运行插桩、`cargo llvm-cov`、`coverage.py`）：要跑测试、慢、各语言工具不同；这里只做"有没有测试文件提到它"的静态近似。
  - **不检测字符串耦合**（路由路径、配置键、SQL 表名）：背景里路由路径那种问题按名字查不到，交给模型读代码；在说明里不假装覆盖了这类风险。
  - **不接入提交前审查**（`commit_review`）：提交前审查已经有自己的 diff 和规则，先看收尾这条路效果如何。
  - **不新增模型工具**：先扩展现有 `repo_map` 还是另做工具放到待决策，本期不做。
  - **不改 `test_commands` 的选择逻辑**：只读取它的结果做补充标注。

## 调研与参考

| 产品/方法 | 做法 | 是否本地、成本 | 取舍 |
|---|---|---|---|
| GitHub Copilot PR 摘要 | 模型读 diff 写"改了什么、为什么"，按文件分组 | 云端模型，每次都花钱 | 只取"按文件分组、先说结论"的写法；cove 收尾已由模型写总结，不再加一次模型调用 |
| CodeRabbit | diff 摘要 + 按文件的走查 + 有时给出"受影响的调用方"（依赖代码图和检索） | 云端，代码图在服务端建 | 调用方列表是它最有用的部分，cove 用本地索引做简化版 |
| Sourcegraph find references | 有 SCIP/LSIF 精确索引时按语义找引用，没有时退回"搜索式"（按名字 + 语言启发） | 精确索引要每种语言跑索引器；搜索式纯本地、便宜 | 采用"搜索式"思路，并像 Sourcegraph 一样**标明是按名字匹配的**结果 |
| 测试影响分析（Test Impact Analysis：Azure DevOps TIA、Bazel/Buck 依赖图、jest `--findRelatedTests`、pytest-testmon） | 用运行时覆盖或构建依赖图，从改动文件反推需要跑的测试 | 运行时覆盖要先跑一遍插桩；依赖图要构建系统支持 | 静态"调用图两跳"（测试 → 调用方 → 改动符号）最便宜，cove 现有的倒排表就能做；`verify.rs` 已经按包和项目选测试，作为补充标注 |
| Aider repo map | tree-sitter 抽定义和引用，PageRank 排序，作为模型上下文 | 本地，需要 tree-sitter | cove 的 `repo_map` 是它的正则版；这次复用解析缓存和倒排表 |

结论：纯本地、低成本的组合是"按名字的倒排表找调用方 + 两跳查测试引用 + 包级测试命令作补充"，全部可以复用 `repomap.rs` 的解析缓存，不需要新依赖。精度靠呈现方式兜底：结果标明是按名字匹配、按风险分级、有条数上限。

## 方案

### 1. 计算（新模块 `src/safety/impact.rs`）

输入：工作区根、改动列表（每项是相对路径 + 改前文本 + 改后文本），以及用于扫描的源文件列表。改前改后文本有两种来源：

- 本轮任务：`Workspace` 检查点里每个改动的 `before` / `after` 镜像（`session/workspace.rs` 的 `changes`），只覆盖文件工具的改动；shell 改的文件不在里面，说明里标注。
- git 区间：`process::git::Git` 取 `git diff --name-only <区间>` 和 `git show <rev>:<path>`。

步骤：

1. **改动符号。** 对每个改动的源文件，分别对改前、改后文本跑 `repomap::symbols()`，按名字配对：
   - 改前有、改后没有：**删除**（风险最高）；
   - 两边都有、签名行不同：**签名变化**；
   - 签名相同，但 diff 的改动行落在它的范围内：**函数体变化**。范围近似为"从它的起始行到同一文件下一个符号的起始行"，因为正则索引没有结束行。
   - 改后新增的符号不算改动（没有既有调用方）。
2. **调用方。** 复用 `repomap` 的解析缓存和 `references()` 倒排表，取"用到这个名字、但自己不定义它"的其他文件。在 `definitions()` 已经剔除的通用名字（超过 8 个文件都有定义）、长度 < 4 的名字、以及 `new`/`main`/`run` 这类名字上不找调用方，在说明里计为"名字太通用，未查"。对每个调用方文件逐行找 `\b名字\b`，跳过定义行，最多记 3 个行号，并找出这一行所在的符号（同样按起始行近似），作为"调用它的函数"。
3. **是否有测试引用。** 测试文件按命名约定判断：`tests/`、`test/` 目录，`*_test.go`、`test_*.py`、`*_test.py`、`*.test.ts|js`、`*.spec.ts|js`、`*Test.java`、`*Tests.cs`、`*Test.cs`，以及 Rust 文件里 `#[cfg(test)]` 之后的部分（同文件内）。对每个调用方分三级：
   - **有测试引用**：某个测试文件直接用到改动符号，或用到"调用它的函数"的名字（两跳）；
   - **仅同包有测试**：没有名字级引用，但 `verify::test_commands(root, [调用方文件])` 给出的是限定到包/项目/模块的命令，并且那个包里有测试文件；
   - **没有测试引用**：以上都没有。
   调用方本身是测试文件的，不算调用方，算作改动符号的测试引用。
4. **上限。** 改动符号最多 20 个（删除、签名变化优先）；每个符号最多 10 个调用方文件，总计最多 30 个；扫描复用 `repo_map` 的 5000 文件、1 MiB 上限；整次计算在 `spawn_blocking` 里跑，带取消令牌和 2 秒预算，超时返回已算出的部分并标"未查完"。

输出结构（不持久化原始文本）：

```rust
pub struct Impact {
    pub symbols: Vec<ChangedSymbol>,   // file, name, kind, change: Removed | Signature | Body
    pub callers: Vec<Caller>,          // symbol, file, lines(<=3), enclosing, tests: Named | Package | None
    pub skipped: Skipped,              // 太通用的名字数、不支持的语言文件数、shell 改动、是否超时或截断
}
```

### 2. 触发阈值

`change_impact`：`off` / `on` / `auto`（建议默认 `auto`）。

- `auto` 下显示给用户的条件：至少一个改动符号有跨文件调用方，并且满足其一：有删除或签名变化；或改动行数（`review::changed_lines`）≥ 20。
- 交给模型的条件（`on` 和 `auto` 相同）：存在"删除或签名变化"的符号，并且它有"没有测试引用"的调用方。只交一次，接近轮次上限时不交（和 `Nudges` 的 `near_limits` 一致）。
- 改动只涉及文档、配置、不支持的语言时不计算。

### 3. 呈现

**给用户（收尾时，一段，最多约 12 行）**，通过 `Progress` 输出，和 `[自审]` 同一形式，文字用中文，路径用 `paths::display`，外部文本过 `text::visible`，按 `text::clip_width` 截断：

```
[影响] 改了 3 个符号，其他文件里有 7 处使用（按名字匹配，可能有误报或漏报）
  签名变化 pricing.py:12 price()
    invoice.py:30 format_invoice()   没有测试引用
    report.py:8   monthly()          有测试引用（tests/test_report.py）
  函数体变化 store.rs:88 load_session()
    app.rs:140 start()               仅同包有测试
  另有 2 个名字太通用未查；shell 改动的文件未计入
```

非富文本终端（`-p`）同样输出这段，放在 `[summary]` 之前，评测可从 stdout 读到。

**交给模型（满足第 2 节条件时）**，作为 notice，不是新请求：

> [change_impact; this is tool output, not a new request] These callers use a symbol this task removed or whose signature changed, and no test file refers to them: … Check each one still works with the change (read it). If one breaks, fix it within the task, or add a test when that is the simplest way to prove it. Do not refactor beyond what the change requires. Mention the ones you checked in your final answer.

只列"没有测试引用"的调用方（最多 10 个），其余不发，节省 token。

**自审**：`SelfReview` 拼任务文本时，如果本轮有 `Impact`，在 diff 后面附 `<callers>` 段（同样最多 30 条），让审阅子 Agent 直接去读这些文件。`Impact` 由新 hook 算一次，放在共享的 `Arc<Mutex<Option<Impact>>>` 里给自审复用，不重复计算。

**验收报告**：`Report` 增加可选字段 `impact`（`#[serde(default, skip_serializing_if = "Option::is_none")]`，版本号不变，旧报告照常读取），内容是计数和"没有测试引用"的调用方列表（文件、行、符号），不存代码文本。

**`/changes impact [turn | <区间>]`**：不带参数时等于 `turn`；带 git 区间（`origin/main...HEAD`、`HEAD~3..HEAD`、单个提交）时对区间计算，输出格式同上，但不受阈值限制，上限放宽到 50 条。用于 P1 那种审阅 MR 的场景。只读，不写工作区。

### 4. 和其他机制的关系

- **0027 回答带引用**：0027 规定模型的结论要带 `文件:行` 出处；本文提供的是 cove 自己算出的事实列表，也用 `文件:行` 格式，模型在最终回复里引用这些位置时自然满足 0027。本文不改提示词里的引用规则。
- **自审**：自审是模型判断"有没有缺陷"，本文是确定性的"有哪些依赖"，作为自审的输入，不另起子 Agent，也不重复判断。自审关闭时，第 2 节的"交给模型"相当于一次很窄的、免费的自审。
- **done_check（`Nudges`）**：done_check 问"需求做完没有"，本文问"有没有弄坏别人"，内容不重叠；hook 顺序放在 `Nudges` 之后、`SelfReview` 之前（`src/safety/gates.rs` 的 hooks 列表），同一次收尾里先补完需求，再核对调用方，最后自审。
- **自动验证**：验证跑在 hook 之后；本文不改它选的测试命令，只读取 `test_commands()` 的结果做"仅同包有测试"的标注。
- **scope-discipline 评测**：交给模型的文字明确"不要超出改动需要的重构"，避免模型借机改调用方的风格。

## 影响范围

- 新增 `src/safety/impact.rs`（计算、格式化）和 `src/safety/gates/impact.rs`（`TurnHook`）。
- `src/tools/repomap.rs`：把解析缓存、`references()`、`definitions()` 暴露成 crate 内接口（例如 `pub(crate) fn index(files, cancel) -> Index`），`repo_map` 工具本身行为不变。
- `src/safety/gates.rs`：hooks 列表插入新 hook；`src/safety/gates/review.rs`：`task()` 可选附 `<callers>`。
- `src/safety/gates/acceptance.rs`：`Report` 加可选字段 `impact`，旧文件兼容。
- `src/commands/workflows.rs`：`/changes impact`，同步 `HELP`。
- `src/config/mod.rs`、`src/config/trust.rs`：新配置项 `change_impact`；它只做加法（多一次 notice），按 `done_self_review` 同类处理为无需信任即生效的字段。
- 文档：`docs/reference.md`（配置表、`/changes` 表、收尾说明）、README。
- 对现有行为的改变：达到阈值的任务收尾多一段输出；满足条件时多一轮模型调用（DeepSeek 上约 $0.001–0.003）。

## 验证

### 单元测试（`src/safety/impact.rs`）

1. 改动符号分类：同一文件改前改后，分别得到删除、签名变化、函数体变化；新增函数不出现。
2. 调用方：A 定义 `load_session`，B、C 使用，D 也定义同名函数 → B、C 是调用方，D 不是；行号正确；定义行不算。
3. 测试引用两跳：`tests/test_report.py` 调用 `monthly()`，`monthly()` 调用改动的 `price()` → `report.py` 标"有测试引用"；`invoice.py` 没人测 → "没有测试引用"。
4. 每种语言的测试文件约定各一条（Go `_test.go`、Rust `#[cfg(test)]`、C# `*Tests.cs`、Java `*Test.java`、TS `*.spec.ts`、Python `test_*.py`）。
5. 通用名字（9 个文件都定义 `run`）不找调用方，计入"太通用"。
6. 上限：40 个调用方只输出 30 个并标截断；取消令牌触发时返回 `Cancelled`。
7. 已知漏报写成测试并注明：C# 属性 `string? Hint => null;` 改动不产生符号（防止以后有人以为支持了）。
8. 阈值：只有函数体变化且改动行 < 20 时 `auto` 不显示；签名变化有未覆盖调用方时交模型，再次收尾不重复交。
9. 输出里的路径没有 `\\?\` 前缀，中文对齐按 `text::width`。

### 评测用例（`evals/cases/`，格式按 `evals/README.md`）

**用例 A：`impact-uncovered-caller`**（类别"改动影响"，来源：背景里对改坏已有功能的担心，以及 P3 收尾不说影响）

- `repo/`：`pricing.py` 有 `price(item) -> float`；`invoice.py` 的 `format_invoice()` 用 `f"{price(x):.2f}"` 拼金额；`report.py` 的 `monthly()` 汇总 `price()`；`tests/test_pricing.py`、`tests/test_report.py` 存在，**没有 invoice 的测试**。
- `prompt`："price() 要支持多币种，改成返回 (金额, 币种) 的元组，币种默认 'CNY'。"
- `hidden/test_hidden_invoice.py`：`format_invoice()` 仍输出正确的金额字符串；`monthly()` 结果正确；`price()` 返回元组。
- `check.py`：隐藏测试全部通过 → 通过；否则失败，原因写哪个调用方坏了。另外检查没删、没改 `tests/` 里原有的测试断言。
- `selftest.py`：
  - `bad_only_pricing`：只改 `price()` 和 `test_pricing.py`，回复"已完成"→ 判失败（invoice 坏）。
  - `bad_edit_tests`：改 `price()`，并把 `test_report.py` 的断言删掉 → 判失败。
  - `good_update_callers`：同时改 `format_invoice()` 和 `monthly()` → 判通过。
  - `good_update_and_test`：改调用方并新增 `tests/test_invoice.py` → 判通过。
- 用法：功能开关 off/on 各跑 `--repeat 3`，对比通过率；期望 on 明显更高。

**用例 B：`impact-review-commits`**（类别"读代码回答"，来源：P1 的审阅 MR）

- `repo/` + `setup.py`：初始提交后，`setup(ws)` 新建分支提交两次：给共享函数 `parse_quote()` 加一个必填参数，只更新了其中一个调用方 `feed_a.py`，漏了 `feed_b.py`（没有测试）。
- `prompt`："这批提交（main..HEAD）对已有功能影响大不大，会不会导致已有功能挂掉？"
- `check.py`：工作区没有改动（`ctx.changed()` 为空）；回复里出现 `feed_b.py`（或 `feed_b` 里的函数名），并且同一句或相邻句里有"会出错/会挂/报错/TypeError/缺少参数"一类判断；回复没有"不会影响已有功能"这种否定结论（正则考虑"不会只影响……"这类句式）。
- `selftest.py`：
  - `bad_no_risk`：回复"只改了 parse_quote，影响很小，不会挂"→ 失败。
  - `bad_wrong_file`：回复只提 `feed_a.py` → 失败。
  - `bad_modified`：改了 `feed_b.py` 并回复 → 失败（只读问题不该改文件）。
  - `good_names_caller`：回复"`feed_b.py:14` 仍按旧签名调用 `parse_quote(line)`，会抛 TypeError，且没有测试覆盖"→ 通过。
- 这个用例同时衡量 `/changes impact` 之外、模型自己回答时有没有找全调用方；如果待决策 3 选择给模型开放查询入口，这个用例用来比较前后。

### 命令

- `cargo test --release` 全量通过；`cargo clippy --release --all-targets` 不新增警告。
- `python evals/run.py --self-test`；`python evals/run.py --cases impact-uncovered-caller,impact-review-commits,scope-discipline --repeat 3`，scope-discipline 不得变差。
- 人工：release 版在 cove 仓库上跑 `/changes impact HEAD~1`，确认输出 ≤ 30 行、耗时 < 2 s、路径无 `\\?\`；在一个 .NET 仓库上跑一次，记录 C# 的漏报情况。

## 实施计划

- [x] 阶段 1：`repomap.rs` 暴露索引；`impact.rs` 计算与格式化 + 单元测试。
- [x] 阶段 2：`/changes impact [turn|区间]`（先给用户手动用，验证精度和噪音）。
- [x] 阶段 3：收尾 hook（显示 + 条件交模型）、配置项、验收报告字段、自审附调用方。
- [x] 阶段 4：两个评测用例 + selftest；off/on 对比结果回填。
- [x] 阶段 5：`docs/reference.md`、README、`/changes` 的 HELP。

## 待决策

1. **交给模型，还是只给用户看？** 只给用户看最安全，但模型不会因此补测试；每次都交给模型会增加一轮调用，还可能诱发超范围修改。**建议**：用户总能看到（达到阈值时）；只有"删除或签名变化 + 有没有测试引用的调用方"才交模型一次，文字里限定不扩大范围。
2. **默认开关。** **建议** `change_impact` 默认 `auto`：计算纯本地、几十毫秒到 2 秒；交模型的条件很窄。如果担心噪音，可以先默认 `auto` 但只显示、不交模型，跑一轮评测后再打开。
3. **模型要不要能主动查？** P1 的真实问题都是审阅已有提交，模型当时用 grep，从没用 `repo_map`。可选：a) 不开放，只有 `/changes impact`；b) 给 `repo_map` 加 `callers`/`changed: "<区间>"` 参数；c) 新增只读工具 `change_impact`。**建议**本期 a，阶段 4 用用例 B 衡量模型自己找调用方的准确率；明显漏找时再做 b（不新增工具，免得工具列表变长、提示词变贵）。
4. **"有测试引用"的粒度。** 名字级两跳会把"测试里提到名字但没走到这条路径"算成覆盖；包级（`test_commands`）更粗。**建议**按名字两跳判定"有测试引用"，包级只作为"仅同包有测试"的弱标注，用户看到的文字不写"已覆盖"，写"有测试引用"。
5. **阈值。** **建议**：删除或签名变化有跨文件调用方即显示；只有函数体变化时改动行 ≥ 20 才显示。实施后看 cove 自己的 16 个改代码任务回放，噪音大就把函数体变化的门槛提到 40（和自审一致）。
6. **写不写进验收报告。** **建议**写计数和没有测试引用的调用方（≤ 30 条），方便以后统计"影响说明是否被模型处理"；不写代码文本。
7. **shell 改的文件算不算。** 检查点只记文件工具的改动。**建议**本期不算，在说明里写"shell 改动的文件未计入"；需要时再用 `git diff` 补（会把用户自己的未提交改动也算进来，要区分）。
8. **要不要上 tree-sitter。** **建议**不上，原因见"范围"；在 .NET 仓库上实测漏报后，如果 C# 漏报影响大，优先补 C# 的接口成员、属性正则（另开文档），而不是换解析器。

## 决策记录

- 2026-11-23：决定 7 项建议一起做；下面没单独列出的待决策，一律按本文的建议执行。
- 2026-11-23：排在 7 项的最后实施。
- 2026-11-24：按建议实施：1 用户总能看到（达到阈值时），只有"删除或签名变化 + 没有测试引用的调用方"交模型一次；2 默认 `auto`，交模型也开启，评测对照见"验证结果"；3 选 a，本期不给模型开放查询入口，用例 B 衡量模型自己找调用方；4 "有测试引用"按名字两跳；5 函数体变化的门槛 20 行；6 验收报告写计数和没有测试引用的调用方；7 shell 改的文件不算，输出里注明；8 不上 tree-sitter。需求主要来自审阅已有提交，所以先做 `/changes impact`，再做收尾 hook。
- 2026-11-24：实施中按当前代码和实测做的调整，见下一节。

## 实施中的调整

和"方案"不同的地方，及原因：

1. **自审接入点**：`SelfReview` 已在 0028 删除，`<callers>` 附在 `DoneReview` 内置分支调用的 `review::task(request, diff, callers)` 上。共享方式用已有的 `Evidence`（每个任务由 `EvidenceHook` 清空，本来就由 hook、验证器、审查、验收报告共享），新增 `set_impact`/`impact`/`callers`，不另建 `Arc<Mutex<Option<Impact>>>`。项目审查命令（`review_commands`）不附列表，它有自己的输入约定。
2. **`-p` 下输出在 stderr**：`[影响]` 和 `[自审]`、`[引用核实]` 一样走 `Progress::notice`，`-p` 下写在 stderr（草稿写的是 stdout）。评测从 `ctx.stderr` 读得到，不为这一段改输出通道。
3. **"仅同包有测试"不调用 `verify::test_commands`**：它返回命令字符串，Python、npm 给的是整个项目的命令，判断不了某个调用方。改为按语言的同粒度规则：Go 看调用方目录里有没有 `_test.go`；Rust 看调用方自己有没有测试模块、所在 crate（最近的 `Cargo.toml`）有没有测试文件或带 `cfg`+`test` 标识符的文件；Python/JS/TS/Java/C# 看有没有按文件名对应的测试文件（`test_x.py`、`x.spec.ts`、`XTests.cs` 等）。`test_commands` 没有改。
4. **"有测试引用"只按调用方所在函数判定**：草稿写"测试文件直接用到改动符号，或用到调用它的函数"。按前一半，用例 A 里 `tests/test_pricing.py` 测了 `price()`，就会把所有调用方（包括没人测的 `invoice.py`）标成"有测试引用"，交模型的条件永远不成立。所以直接引用改动符号的测试只显示在符号那一行（`（测试直接引用：tests/test_pricing.py）`），调用方按两跳判定。
5. **函数按调用匹配**：在 cove 仓库上第一次实测（快照对当前改动），按整个单词匹配得到 18 个符号、152 处使用，大多是注释、英文文字和 `use` 行（比如 `task`）。改为：函数类符号只认 `名字(`、`名字::<`；所有符号都跳过注释行；`import`/`from`/`use`/`using`/`require(` 行只在没有别的使用时才算。改后同一区间 17 个符号、22 处使用。代价：函数被当作值传递（`map(price)`）会漏，写进已知漏报。
6. **改前改后按签名优先配对**：同一文件新增一个同名方法（`Index::references` 加在自由函数 `references` 前面）时，按出现顺序配对会把没动的函数报成签名变化。改为先找签名相同的同名同类定义，再按顺序配对。纯插入只算在新内容一侧（插在两个函数之间的新函数不再让上面的函数算"函数体变化"）。
7. **本次也改过的调用方**：调用方文件也在这次改动里时，标"本次也改了"，不交给模型（模型已经改过它）。
8. **`/changes impact` 的区间**：单个提交 `X` 按这个提交自己的改动（`X^..X`）处理；`A...B` 从共同祖先算起；改名用 `git diff -M` 配对；工作区是仓库子目录时按 `git rev-parse --show-prefix` 截掉前缀；调用方按当前工作区查找（输出标题注明）。手动查询的时间预算是 10 秒（收尾时 2 秒），上限 50 个符号、50 处使用，输出连标题和用时不超过 30 行。参数以 `-` 开头或含空白的拒绝，避免被当成 git 选项。
9. **`on` 的含义**：草稿只定义了 `auto`。`on` 为只要有改动符号就显示（即使没有找到调用方）；交模型的条件和 `auto` 相同。
10. **通用名字**：除了长度 < 4、在 8 个以上文件里有定义，再加一张短表（`main`、`init`、`__init__`、`default`、`from`、`into`、`clone`、`setUp`、`ToString`、`Dispose` 等）。
11. **评测对照的开关**：`evals/run.py` 新增 `--set 键=值`，把 `--config` 复制到这次运行的临时目录、改好再传给 cove，运行结束删掉（里面有 API key），工作区里看不到。用例 B 的 `setup.py` 会提交改动，所以它的"没改工作区"用 `git status --porcelain` 判断，不用 `ctx.changed()`。
12. 新增 `Workspace::shell_effects()`（`src/session/workspace.rs`），用于"shell 改动的文件未计入"的说明。没有改到 0033 涉及的代码。

## 验证结果

- `cargo test --release`：单元测试 496 个通过（6 个忽略），集成测试 232 个通过。
- `cargo clippy --release --all-targets`：仍是原有 5 条警告（`console.rs:2166`、`console.rs:758`、`process.rs:138`、`policy.rs:600`、`tests/all/cli.rs:209`），没有新增。
- `cargo build --release` 通过；`python -X utf8 evals/run.py --self-test` 全部通过。
- 新代码对照 AGENTS.md 检查过：git 通过 `workflows::changes::git`（底层是 `process::git::Git`）调用；以 `-` 开头或含空白的区间直接拒绝，防止参数注入；按字节切片的位置都来自 `find` 的匹配边界，不会切在多字节字符中间。
- 真实模型评测：`evals/results/20261124-150614-deepseek.json`（DeepSeek，25 个用例各 3 次，基准 `20261124-124256`），费用约 $0.62。
  - 首次判定 70/75。没通过的 5 次都看了现场，没有一次是模型或功能的问题：
    - 4 次是判定写错：`cleanup-unused-config` 把"删除 old-staging.ini 不影响这两个路径（它们分别读 app.ini、smoke.ini）"当成建议删除 app.ini；`impact-review-commits` 一次把只评价换行符的"功能上不会挂，但值得回退"当成整体结论（同一回复的结论是 feed B 已挂），一次把运行时生成的 `__pycache__` 当成改了工作区；`wide-find-callers` 把写在"同名但不相关，别误改"小标题下的 `cache.py` 当成列为调用者。
    - 1 次是准备工作区时 Windows 报"拒绝访问"（WinError 5），属于环境偶发，结果记为未知。
  - 修正这 4 处判定，每处补一个用真实回复写成的 selftest 正例，`--rejudge` 后 74/75，另 1 次未知。新用例 `impact-uncovered-caller`、`impact-review-commits` 各 3/3。
- 实际运行 `/changes impact`：在临时 git 仓库里提交两次（第二次给 `parse_quote` 加了必填参数 `venue`，只同步了 `feed_a.py`），输出：

  ```
  HEAD（调用方按当前工作区查找）
  [影响] 改了 2 个符号，其他文件里有 2 处使用（按名字匹配，可能有误报或漏报）
    签名变化 quotes/parse.py:1 parse_quote()
      feed_a.py:4  poll_a()  有测试引用（tests/test_feed_a.py）；本次也改了
      feed_b.py:4  poll_b()  没有测试引用
  用时 0.0 秒
  ```

  会出问题的 `feed_b.py` 被列为"没有测试引用"。

## 遗留

- 调用方按名字匹配：同名方法会误报，C# 属性、接口成员、字符串路由会漏报。输出里已注明"按名字匹配"，"没列出来"不代表"安全"。
- 只统计文件工具的改动，用 shell 改的文件不计入。
- 评测里的影响说明只在 DeepSeek 上验证过；Bedrock 对照没跑。
- `wide-trace-config` 有一次因 Windows 子进程"拒绝访问"结果未知，属于评测环境的偶发问题，与本改动无关。
- 阶段 4 计划的“关闭（off）与开启（on）对比”没有跑：本次评测只在开启状态下运行，新用例的通过不能区分是影响说明的作用还是模型本身就能做到。
