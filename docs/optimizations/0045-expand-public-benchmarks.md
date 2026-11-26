# 0045 公开评测题扩充到 20–30 题

- 状态：已完成
- 日期：2026-11-24（创建）
- 关联：0037（公开评测题试点）、0023（真实模型评测集）

## 背景

0037 接入了 3 道公开题（Exercism Python `book-store`、Rust `forth`，SWE-bench Verified `sympy__sympy-24562`），本机都能跑，判定用官方测试，selftest 证明判定可靠。0037 的遗留建议扩充到 20–30 题，并指出两个问题：两道 Exercism 题 DeepSeek 都是 3/3，区分不出好坏；只有 3 题时，一题的随机波动就能左右结论。

## 现状与问题

1. **题太少。** `evals/cases/pub-*` 只有 3 个，每类各 1 题，单题结果的随机性大于改动带来的差别。
2. **区分度不够。** `evals/results/20261124-164305-deepseek.json` 里 book-store、forth 都是 3/3（0037 验证结果）。
3. **没有 C# 题。** polyglot 不含 C#；cove 的使用场景里有 .NET 项目，现有用例全部是 Python 和 Rust。
4. **SWE 题只有 sympy 的一个模块（core/numbers）。** 换一个子模块，定位难度就不同。

## 目标

- 公开题合计 20–30 题：Exercism Python 8–10 题、Rust 8–10 题、C# 5 题，SWE-bench Verified 5 题（均含 0037 已有的题）。
- 每题的标准答案在本机验证过：参考解、标准补丁让判定通过；桩代码、不修改让判定失败。
- 每题的 selftest 除了 good（参考解、标准补丁）和 bad（桩代码、不修改），再加至少一种常见错法：改测试过关，或者贪心、漏掉一条规则的部分实现。
- 给出真实模型的首轮运行命令；结果出来后回填。

## 范围

- 做：
  - 新增 26 个 `pub-*` 用例目录，每个目录附 `LICENSE-SOURCE.md`；
  - `evals/README.md` 的"用例"表和"公开题"一节补上新题。
- 不做：
  - 不改 `run.py`、不改 cove 代码、不改已有用例。现有 `ctx.hidden_tests()`、`setup.py` 机制已经够用。
  - 不跑真实模型：本文只负责题目和判定，首轮结果另行回填。
  - 依赖第三方 crate 的 Rust 题（alphametics、decimal、poker、grep 等）：参考解要 `itertools`、`num-bigint` 等 crate，第一次构建要联网拉 crates.io，环境不稳定。
  - pytest、requests 的 SWE 题：requests 的测试依赖 httpbin 服务和 `pytest-httpbin`；pytest 题要在 3.13 上从源码跑老版本 pytest，还要处理全局已装的 pytest 插件；收益不抵环境成本，这次只用 sympy。
  - 函数名、文件名改写后的"去污染"对照：另开文档。

## 调研与参考

### polyglot 有没有逐题结果

- Aider 的博客（https://aider.chat/2024/12/21/polyglot.html）只给出选题方法：从 Exercism 697 题里取"7 个模型中至多 3 个做对"的 225 题。没有逐题的通过数据。
- 所以 polyglot 里的题本来就偏难；在其中再按题型挑：状态机、解析器、组合搜索、带规则陷阱（贪心是错的、有容易漏掉的禁止状态、错误信息有固定要求）的题，以及 Rust 里考类型系统（生命周期、泛型约束、`unsafe`、宏）的题。

### C# 能不能在本机跑

- 本机 .NET SDK：8.0.413、10.0.102、10.0.402、10.0.404。exercism/csharp（MIT，2026-11-24 克隆，提交 `f4e24112`）的练习题目标框架是 `net10.0`，测试用 xunit.v3。
- 实测：`change` 题换上 `.meta/Example.cs`、去掉测试上的 `Skip` 后，`dotnet test` 第一次还原 NuGet 包用时 13 秒，整次 47 秒，13 条测试全部通过。之后走本机缓存。
- exercism/csharp 的测试除第一条外都标了 `[Fact(Skip = "Remove this Skip property to run this test")]`，学员逐条去掉。用例里统一去掉，相当于 Rust 题的 `--include-ignored`。
- 部分 C# 题依赖第三方包（`forth`、`sgf-parsing` 用 Sprache，`react` 用 FakeItEasy），`ledger`、`markdown` 是重构题（桩代码已经能通过测试，做不了 bad）；都不选。

### SWE-bench Verified 选题

按 0037 的标准从 `verified.parquet` 筛：sympy、单文件补丁、改动不超过 20 行、难度 "<15 min fix" 或 "15 min - 1 hour"，得到 54 题。再人工逐题读 issue：

- 排除 issue 原文直接写了修法的：`23824`（"the insertion loop is just backward"）、`21612`（issue 里附了 diff）。
- 排除 1.0–1.4 的老版本：在 Python 3.13 上会碰到 `collections.Mapping` 一类已经删掉的接口。
- 排除测试太弱的：`21379` 的 FAIL_TO_PASS 只检查"不抛异常"，一个 `try/except` 就能过。
- 排除含义有争议的：`20801`（issue 自己也不确定期望哪个结果）。
- 本机验证不通过的换掉：`23262` 的 PASS_TO_PASS 里 `test_integral`、`test_double_integral` 在基准提交和标准补丁下都报错（`NameError: name 'Integral' is not defined`，本机全局 numpy 2.3.3 下 lambdify 的模块选择不同），good 无法通过，换成 `22714`。

## 方案

### 选题（新增 26 题，合计 29 题）

**Exercism Python（新增 8 题，合计 9 题）**，均为 polyglot 收录：

| 题 | 为什么选 | selftest 里的常见错法 |
|---|---|---|
| bowling | 计分规则多、状态多，补投和各种非法投球都要报错，错误信息有固定要求 | 第十轮补投两球之和超过 10 不报错 |
| dominoes | 组合搜索：要排成首尾相接的环，只排成一条链是错的 | 不检查首尾闭合 |
| poker | 牌型比较规则多，A 可以当 1（A-2-3-4-5 顺子） | 不认 A 当 1 的顺子 |
| pov | 树的换根和找路径，错误信息有固定要求 | 错误信息和说明不一致 |
| react | 响应式单元格：回调只在值真正变化时触发一次，菱形依赖下不能重复触发 | 不等输入全部更新就重算，回调重复触发 |
| sgf-parsing | 手写解析器，转义、换行、空白规则细碎 | 转义时把反斜杠也留在值里 |
| two-bucket | 状态空间搜索，有一条容易漏掉的禁止状态规则 | 漏掉禁止状态 |
| wordy | 解析自然语言算式，"不认识的运算"和"语法错误"要分开 | 两种错误混用 |

**Exercism Rust（新增 8 题，合计 9 题）**，均为 polyglot 收录、不依赖第三方 crate：

| 题 | 为什么选 | selftest 里的常见错法 |
|---|---|---|
| bowling | 计分状态机，第十轮补投的合法性规则细 | 全中后两次补投之和超过 10 也接受 |
| doubly-linked-list | 用 `unsafe` 写双向链表和游标，有内存泄漏测试；`advanced` 特性检查协变和 Send/Sync | Drop 不释放节点 |
| fizzy | 泛型加闭包，同一套规则要用在 u8、u64、f64 上 | 只取第一条命中的规则 |
| macros | 写 `macro_rules!`，还有一组"这些写法必须编译失败"的测试 | 不支持末尾逗号 |
| react | 响应式单元格加回调，借用和生命周期 | 值没变也触发回调 |
| two-bucket | 状态空间搜索，有禁止状态 | 漏掉禁止状态 |
| wordy | 解析算式；`exponentials` 特性要求乘方 | 没做乘方（不带特性跑测试时看不出来） |
| xorcism | 桩代码不能编译，要自己设计泛型约束和生命周期；`io` 特性要求 Read/Write 适配器 | 密钥循环边界差一 |

**Exercism C#（新增 5 题）**，取自 exercism/csharp，不依赖第三方包：

| 题 | 为什么选 | selftest 里的常见错法 |
|---|---|---|
| change | 最少硬币数，每次拿最大面值的贪心是错的，测试有反例 | 贪心解 |
| go-counting | 洪水填充找连通空地再判归属 | 被两色包围的空地也判给一方 |
| rest-api | 按 JSON 接口记账，借贷要和已有欠款抵消，输出要和测试逐字一致 | 不抵消已有欠款 |
| state-of-tic-tac-toe | 判断井字棋局面，要识别各种不可能的非法局面 | X 赢了但双方步数相同没判为非法 |
| zipper | 不可变二叉树的 zipper，移动、修改后还原整棵树 | 往左走时面包屑顺序反了 |

**SWE-bench Verified（新增 5 题，合计 6 题）**，都是 sympy，每题一个子模块：

| 实例 | 版本 / 难度 | 模块 | 问题 | selftest 里的常见错法 |
|---|---|---|---|---|
| sympy__sympy-19954 | 1.7 / <15 min | combinatorics | `DihedralGroup(18).sylow_subgroup(2)` 抛 IndexError：循环里边遍历边删列表元素 | 只删掉 `del` 避开异常 |
| sympy__sympy-20590 | 1.7 / 15 min–1 h | core | 1.7 起 `Symbol` 有了 `__dict__`：要找出是哪个基类漏了 `__slots__` | 只有改测试过关这一种 |
| sympy__sympy-21847 | 1.9 / <15 min | polys | `itermonomials` 带 `min_degrees` 时漏掉混合单项式 | 只修了可交换变量的分支 |
| sympy__sympy-22714 | 1.10 / <15 min | geometry | `evaluate(False)` 下 `Point2D(1, 2)` 报"虚数坐标" | 直接删掉虚数坐标检查 |
| sympy__sympy-24066 | 1.12 / 15 min–1 h | physics.units | 量纲为 1 的表达式套 `exp` 后量纲推导报错 | 所有函数一律当无量纲 |

每题的 selftest 都另有 `bad_only_tests`：在测试文件末尾把 FAIL_TO_PASS 里的测试重新定义成空函数（改测试过关）。

### 用例格式

沿用 0037，不改 `run.py`：

- **Python**：`repo/` 放 `.docs/`（instructions、instructions.append、introduction）、桩代码和官方测试；`hidden/test_hidden_<模块>_test.py` 是测试原件；`check.py` 先比对可见测试的字节，再用 `ctx.hidden_tests()` 跑原件。
- **Rust**：`repo/` 放 `.docs/`、`Cargo.toml`、`src/`（含 doubly-linked-list 的 `pre_implemented.rs`）、`tests/`（含 macros 的 `tests/invalid/`），加 `.gitignore`（`/target`、`Cargo.lock`）。`hidden/` 是 `tests/` 的原件。`check.py` 比对 `tests/` 下每个文件，用原件覆盖后运行 `cargo test --test <各目标> [--features …] -- --include-ignored`，超时 600 秒。判定命令写进 prompt：特性后面的测试默认不跑，不告诉模型就不公平。xorcism 的 prompt 改成"签名只是起点，可以补上泛型约束和生命周期"：这题的桩代码本来就不能编译（exercism 的 `allowed-to-not-compile`）。
- **C#**：`repo/` 放 `.docs/`、桩代码、`.csproj`、`packages.lock.json`、`.editorconfig`、去掉 `Skip` 的测试，加 `.gitignore`（`bin/`、`obj/`）。`check.py` 比对测试文件和 `.csproj` 的字节（改项目文件可以把测试排除掉），用原件覆盖后运行 `dotnet test`（`DOTNET_CLI_UI_LANGUAGE=en`），要求退出码为 0 且输出含 `Passed!`。
- **SWE**：`setup.py`、`check.py` 和 sympy-24562 的逐字节相同，它们只读 `hidden/task.json`；源码缓存在 `%TEMP%\cove-eval-cache\sympy-<sha12>`，mpmath 1.3.0 共用一份。prompt 是 issue 原文，不含 hints。`task.json` 多记了 `version`、`difficulty`、`gold_file`。
- **常见错法**：Exercism 题写成对参考解的字符串替换（`PARTIAL`），每处替换在参考解里恰好出现一次，否则 selftest 直接报错；C# change 的贪心解是整段代码。SWE 题是对基准文件的替换。

生成脚本（不提交）：从本机的 polyglot、exercism/csharp 克隆和 `verified.parquet` 复制文件、转 LF、写 `case.json`/`check.py`/`selftest.py`/`LICENSE-SOURCE.md`。

## 影响范围

- 新增 26 个目录：
  - `evals/cases/pub-exercism-py-{bowling,dominoes,poker,pov,react,sgf-parsing,two-bucket,wordy}/`
  - `evals/cases/pub-exercism-rs-{bowling,doubly-linked-list,fizzy,macros,react,two-bucket,wordy,xorcism}/`
  - `evals/cases/pub-exercism-cs-{change,go-counting,rest-api,state-of-tic-tac-toe,zipper}/`
  - `evals/cases/pub-swe-sympy-{19954,20590,21847,22714,24066}/`
- `evals/README.md`："用例"表加 4 行（按语言合并），"公开题"一节补 C# 和 Rust 特性的说明。
- 全量评测从 28 个用例增加到 54 个。时间和费用主要在 6 道 SWE 题（每题时限 1800 秒）；C# 每次判定要构建一次，约 10–50 秒；Rust 约 5–15 秒。
- C# 题需要本机有 .NET 10 SDK，第一次要联网还原 NuGet 包。没有 SDK 的机器上这 5 题判为失败，不是 cove 的问题。

## 验证

- `python -X utf8 evals/run.py --self-test --cases <新增 26 题>`：每题的 good 都通过，bad 都失败。
- 对每个部分实现的 bad，确认失败原因是官方测试没过（而不是测试被改、编译环境出错）。
- SWE 题另外逐题确认：基准提交打上 test_patch 后 FAIL_TO_PASS 失败、PASS_TO_PASS 全过；再打上标准补丁后两组全过。
- 真实模型首轮：每题 1 次摸底，看通过率、耗时、费用、有没有环境问题；之后对区分度好的题跑 3 次。

## 实施计划

- [x] 阶段 1：挑题；核实 C# 环境；逐题在本机验证标准答案
- [x] 阶段 2：26 个用例和 selftest；`evals/README.md`
- [x] 阶段 3：真实模型首轮（`--repeat 1`），回填结果；按结果剔除 3/3 区分不出的题或补更难的题

## 待决策

- 首轮结果出来后，哪些题保留进全量评测，哪些只在需要时单独跑：建议 SWE 题和 C# 题因为耗时，默认只在改动涉及读仓库、定位能力时单独跑。

## 决策记录

- 2026-11-24：Rust 题的判定打开 Cargo.toml 里原有的特性（`advanced`、`exponentials`、`io`），并把判定命令写进 prompt。原因：特性后面的测试正是这几题的难点；不说明，模型照常 `cargo test` 看不到这些测试，判定就不公平。
- 2026-11-24：C# 题去掉测试的 `Skip`，`repo/` 和 `hidden/` 是同一份。原因：和 Rust 的 `--include-ignored` 一致，模型看到的就是判定要跑的全部测试。
- 2026-11-24：SWE 题这次只用 sympy。原因：requests 测试要 httpbin，pytest 题要在 3.13 上跑老版本 pytest，环境成本高；sympy 不同子模块的定位难度差别已经足够。
- 2026-11-24：`sympy__sympy-23262` 换成 `22714`。原因：本机 numpy 2.3.3 下 PASS_TO_PASS 里有 2 条在标准补丁上也报错，good 判不过。

## 验证结果

- 用例目录：26 个新用例曾为了让 0044 的全量评测和 29 题基线可比，暂存在 `%TEMP%\cove-0045-parked`；2026-11-25 移回 `evals/cases/`，现在共 55 个用例。
- 自检（`--self-test --cases <新增 26 题>`）：第一次 102 项符合、1 项不符合：`pub-exercism-cs-zipper` 的 `bad_crumb_order` 判成了通过。原因是这个错误实现只弄反了向左走时面包屑的顺序，而官方测试从不连续向左走两层，抓不到。改为左右两个方向都按错误顺序追加（"先左后右再 `Up`"会出错）后重跑，该题 4 项全部符合；26 题自检全部通过。
- 真实模型首轮（2026-11-25，构建 `0.8.0+555f0cebcafa`，Bedrock Opus `model_fast=off`，`--repeat 1 --keep`，结果 `evals/results/20261125-175833-bedrock.json`）：25/26 通过，1 次结果未知，费用 $2.03。

| 类别 | 题数 | 通过 | 费用 | 平均调用 |
|---|---|---|---|---|
| exercism C# | 5 | 5 | $0.42 | 5.0 |
| exercism Python | 8 | 8 | $0.53 | 4.5 |
| exercism Rust | 8 | 8 | $0.77 | 5.4 |
| SWE sympy | 5 | 4（1 次未知） | $0.31 | — |

- `pub-swe-sympy-20590` 的"未知"：模型改了 `sympy/core/_print_helpers.py`，约 2 分钟就写完了回复；之后的自动验证找不到同名测试 `test__print_helpers.py`，按 0042 的规则退回运行全量 `python -m pytest -q`（整个 sympy 测试套件）。全量测试一直在正常运行，按 0048 的规则不会因为时长被砍，于是先撞上了评测脚本每题 1800 秒的上限。按 0048 之前的规则，它会在 300 秒被砍，记为"验证未运行"。评测脚本杀掉 cove 后，没有残留的 pytest 进程。
- 环境问题：没有发现（C#、Rust、Python、sympy 题都能正常构建、跑测试）。

## 遗留

- 区分度：首轮 Opus 在 exercism 21 题上全部通过，单次结果区分不出难度。剔除"3/3 都过"的题需要每题跑 3 次（预计约 $6），这次没跑；要不要跑、剔除后补哪些更难的题，另开文档决定。
- `pub-swe-sympy-20590` 暴露的验证范围问题：pytest 找不到同名测试时退回全量测试，在 sympy 这种大仓库里要跑几十分钟。可选的改法是退回到改动模块所在包的 `tests/` 目录（这里是 `sympy/core/tests/`），而不是全量；这是验证规则的改动，另开文档。修好之前，这题会一直是"未知"或者很慢。
