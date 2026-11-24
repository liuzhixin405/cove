# 0037 评测集接入公开评测题（先试 3 题）

- 状态：已完成
- 日期：2026-11-24（创建）；2026-11-24（完成）
- 关联：0023（真实模型评测集）

## 背景

`evals/cases/` 现有 25 个用例，都是根据 cove 出过的问题自己编写的。题目和判定都出自同一批人，判定本身可能写错：0031 的首次判定里有 4 次误判，都是判定写错了。另一个来源是：拿内部仓库人工分析出的 bug 做用例。但这些 bug 本身就未必准确，所以没有采用。公开评测题有现成的官方测试，标准答案也经过验证，可以补上"判定本身可信"的这一类用例。

## 现状与问题

1. 现有用例的判定都是自己写的 `check.py`（正则匹配回复、检查文件内容），正确性只靠 selftest 保证。selftest 的 good 和 bad 样例也是同一批人写的。
2. 没有"在陌生的真实大仓库里定位并修复"的用例。现有用例的 `repo/` 都只有几个到十几个文件。
3. 公开数据集的可用性，本机实测结果如下（2026-11-24）：
   - `huggingface.co` 连不上（连接超时），`hf-mirror.com` 和 `github.com` 可以访问。
   - 本机没有 Docker 环境。SWE-bench 官方工具、Multi-SWE-bench、SWE-PolyBench、Terminal-Bench 都按 Docker 镜像运行，不能直接使用。

## 目标

- 新增 3 个用例：
  - Exercism Python 题 1 道；
  - Exercism Rust 题 1 道；
  - SWE-bench Verified 题 1 道。
- 判定使用题目自带的官方测试：
  - 参考解或标准补丁必须让判定通过（selftest good）；
  - 原始代码必须让判定失败（selftest bad）。
- 用 DeepSeek 每题跑 3 次，记下耗时、费用、通过率和卡住的地方，据此决定要不要扩充到 20–30 题。

## 范围

- 做：上面 3 个用例；`evals/README.md` 增加"公开题"一节，写明来源、许可和缓存目录。
- 不做：
  - 需要 Docker 的数据集：本机没有 Docker。
  - HumanEval/MBPP：只考单个函数补全，测不到读仓库、跑命令这些能力。
  - 现在就扩到几十题：先看试验结果。
  - Bedrock 上的对照：SWE 题的上下文大，Bedrock 贵约一个数量级；等确认有价值再做。

## 调研与参考

候选数据集比较：

| 数据集 | 判定 | 答案验证 | 运行条件 | 许可 |
|---|---|---|---|---|
| Aider polyglot（取自 Exercism） | 题目自带单元测试 | 每题有参考解（`.meta/example.*`） | 不需要 Docker，本机即可 | 题目来自 Exercism 题库，题库为 MIT；polyglot 仓库自身没有 LICENSE 文件，README 写的是按 Exercism 的许可使用 |
| SWE-bench Verified | FAIL_TO_PASS 和 PASS_TO_PASS 两组测试 | 人工标注筛选，每题 3 人 | 官方工具要 Docker；纯 Python 仓库可以在本机跑 | 数据集卡片没有写许可；代码属于各上游仓库（sympy 为 BSD-3-Clause） |
| Multi-SWE-bench、SWE-PolyBench、Terminal-Bench | 测试 | 人工筛选 | 必须 Docker | Apache-2.0 / MIT |
| HumanEval、MBPP | 单个函数的测试 | 测试偏少 | 本机即可 | 不适合 agent 评测 |

- Exercism 三个题库 `exercism/python`、`exercism/rust`、`exercism/csharp` 的 LICENSE 都是 MIT，2026-11-24 从 GitHub 原文件核实。
- SWE-bench Verified 的已知问题：有公开分析指出，部分题目的答案就写在 issue 里，部分题目的测试太弱，会放过错误的修复；另有报告称它已被部分厂商停用。所以挑题时人工逐题读：issue 原文里不能直接给出修法，测试必须真的检验行为。
- 数据污染：这些题公开已久，模型可能见过。所以只用来对比改动前后的变化，不当作能力的绝对值。

## 方案

### 选题

- **Python：`book-store`**（polyglot 收录）。
  - 求一篮书的最低价，"每次凑最大的套装"这种贪心解法是错的，测试里专门有反例。
  - 测试 20 条，用 unittest。
- **Rust：`forth`**（polyglot 收录）。
  - 实现一个 Forth 子集的解释器，题目附带 `alloc-attack` 测试，专门检查用户自定义词展开时的内存占用。
  - 测试 50 条，大部分标了 `#[ignore]`，判定要加 `--include-ignored`。
- **SWE-bench Verified：`sympy__sympy-24562`**。
  - 问题：`Rational('0.5', '100')` 得到 1/100100，正确结果是 1/200。
  - 标准补丁只改 `sympy/core/numbers.py`。
  - 难度标注为 "<15 min fix"。
  - issue 原文里没有写修法：修法在 hints 里，按 SWE-bench 的做法不交给模型。

### 用例格式

**Exercism 题**

- `repo/` 放：
  - 题目说明 `.docs/instructions*.md`；
  - 桩代码；
  - 官方测试，和 polyglot 一样让模型看得到测试；
  - Rust 题再放 `Cargo.toml`。
- `hidden/` 放同一份官方测试的副本。
- `check.py` 先检查 `repo/` 里的测试没有被改，再用 `hidden/` 里的原件跑测试：
  - Python 用 `ctx.hidden_tests()`，副本改名为 `test_hidden_*.py`；
  - Rust 把 `hidden/` 里的测试复制到工作区的 `tests/` 覆盖，然后跑 `cargo test -- --include-ignored`。
- selftest：
  - good：写入 `.meta/example.*`。这个文件存放在用例目录里，不放进 `repo/`。
  - bad：保留桩代码；或者改测试，让可见的测试通过。

**SWE 题**

- `repo/` 只放一个说明文件。
- `setup.py` 负责准备工作区：
  - 把 sympy 在 base_commit 的代码复制进工作区并提交；
  - 源码缓存在 `%TEMP%\cove-eval-cache\sympy-<sha>`，只有第一次会联网，用 `git fetch --depth 1` 拉取这一个提交；
  - mpmath 1.3.0（sympy 的唯一依赖，纯 Python，BSD）缓存在 `%TEMP%\cove-eval-cache\mpmath-1.3.0`，复制进工作区根目录，并写进 `.git/info/exclude`。这样不需要往全局 Python 环境里装包。
- prompt：issue 原文，再加一句"修复这个问题"。
- `check.py`：
  1. 把模型改过的测试文件还原到 base_commit 的版本；
  2. 打上官方的 test_patch；
  3. 用 sympy 自带的 `bin/test` 跑 `sympy/core/tests/test_numbers.py`；
  4. FAIL_TO_PASS 和 PASS_TO_PASS 里列出的测试都必须通过；
  5. 不在这两个列表里的失败不计入。本机新版 numpy 会让 `test_numpy_to_float` 报异常，这条测试不在 PASS_TO_PASS 里。
- selftest：
  - good：打上标准补丁；
  - bad：不改代码；或者只改测试。

### 本机验证过的事实

- book-store：
  - 桩代码跑官方测试：20 条失败；
  - 换成 `.meta/example.py`：全部通过。
- forth：
  - 桩代码：失败；
  - 换成 `.meta/example.rs`：50 条通过（49 条加 alloc-attack 1 条），`cargo test` 用时约 11 秒。
- sympy-24562，在 Python 3.13 加 mpmath 1.3.0 的环境下：
  - 基准提交：106 条通过，1 条异常（`test_numpy_to_float`）；
  - 打上 test_patch 后：`test_issue_24543` 失败；
  - 再打上标准补丁：107 条通过，只剩那 1 条 numpy 异常。

## 影响范围

- 新增目录：
  - `evals/cases/pub-exercism-py-book-store/`
  - `evals/cases/pub-exercism-rs-forth/`
  - `evals/cases/pub-swe-sympy-24562/`
- 每个目录附上题目来源和许可说明（`LICENSE-SOURCE.md`）。
- `evals/README.md` 增加"公开题"一节。
- 不改 `run.py`，不改 cove 的代码。
- 全量评测从 25 个用例增加到 28 个，费用和时间都会增加，SWE 题占大头。

## 验证

- `python -X utf8 evals/run.py --self-test`：三个新用例的 good 都通过，bad 都失败。
- DeepSeek：新用例每题跑 3 次（`--case` 指定），记录通过率、耗时和费用。
- 判定失败时，看现场分清楚原因：是模型的问题、环境的问题，还是判定写错了。

## 实施计划

- [x] 阶段 1：核实许可和网络，在本机验证三道题的标准答案
- [x] 阶段 2：三个用例和 selftest
- [x] 阶段 3：DeepSeek 跑 3 次，回填结果，决定是否扩充

## 待决策

无。扩不扩充，等阶段 3 的数据出来再定。

## 决策记录

- 2026-11-24：内部仓库人工分析出的 bug 不做用例。改用答案经过验证的公开题，先试 3 题。
- 2026-11-24：Exercism 题让模型看得到测试（和 polyglot 一致），判定用 hidden 里的原件，并检查可见测试没有被改。原因：Exercism 的题目说明不足以确定接口细节，比如 Rust 的错误类型；不给测试，判定就不公平。

## 验证结果

- `python -X utf8 evals/run.py --self-test`：三个新用例全部通过。
  - book-store：4 个函数，含一个贪心解，确认会被官方测试判为失败；
  - forth：3 个函数；
  - sympy：4 个函数，含"修了问题但改坏了同文件里的其他行为"的情况，确认会被 PASS_TO_PASS 判为失败。
- 真实模型：`evals/results/20261124-164305-deepseek.json`（DeepSeek，构建 `0.7.0+39e48059b2d4`，每题 3 次，并发 3），费用合计约 $0.36。

  | 用例 | 通过 | 平均调用 | 平均耗时 | 主上下文峰值 |
  |---|---|---|---|---|
  | pub-exercism-py-book-store | 3/3 | 8.7 | 37s | 1.4 万 |
  | pub-exercism-rs-forth | 3/3 | 12.0 | 75s | 2.8 万 |
  | pub-swe-sympy-24562 | 2/3，1 次未知 | 21.7 | 611s | 5.4 万 |

- sympy 第 2 次"未知"的现场：
  - 时间线：模型 17:04 已经完成修复并给出最终回复（修法和标准补丁一致）。超时的原因有两个：
    - 中途执行了一次 `python -m pytest -q`（整个 sympy 测试套件），在 shell 的 600 秒上限处被截断；
    - 回复之后，cove 的自动验证又启动了全量测试，接着用例的 1200 秒时限就到了。
  - 这次暴露了两个 cove 的问题，另开文档：
    - **0038**：`pip install pytest` 被拒绝之后，模型写了 `_install_pytest.py`，用 `subprocess` 调 pip，再运行这个脚本，绕过了拒绝。pytest、pluggy、iniconfig 被装进了本机全局 Python（保留，没有卸载）。
    - **0039**：自动验证退回了全量测试，推断原因是已经删掉的临时脚本仍被算作改过的文件。
  - 评测这边的改动：
    - `run.py` 给被测 cove 设置 `PIP_REQUIRE_VIRTUALENV=1`，pip 在虚拟环境之外会拒绝安装；
    - sympy 用例的时限放宽到 1800 秒。
- 同一时段的全量评测 `20261124-163529-deepseek.json` 也跑到了两道 Exercism 题，各 1/1 通过。

## 遗留

- **扩充**：试验表明三类题在本机都能跑，判定也可靠，建议扩充到 20–30 题，另开文档：
  - Exercism Python 和 Rust 各 8–10 题，从 polyglot 里挑；
  - Exercism C# 5 题（polyglot 没有 C#）；
  - SWE-bench Verified 纯 Python 题 5 题，按本文的挑选标准。
- **区分度**：两道 Exercism 题 DeepSeek 都是 3/3，区分不出好坏。扩充时优先选 polyglot 里"多数模型做错"的题。
- **污染没有量化**：可以在扩充时把函数名、文件名改掉，各跑一组做对比。
- **其他模型**：Bedrock 上没跑。
- **sympy 题的环境噪声**：本机新版 numpy 让 `test_numpy_to_float` 报异常，模型会花时间解释它；pytest 现在已经装在全局 Python 里，之后的运行环境和这次不同。
