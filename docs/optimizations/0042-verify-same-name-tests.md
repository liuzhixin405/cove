# 0042 自动验证按目录就近选测试，不带上同名的无关测试

- 状态：已完成
- 日期：2026-11-24（创建）；2026-11-25（完成）
- 关联：0039（遗留第 1 条）、0041（同一题里暴露的另一个验证问题）

## 背景

Python 项目里，自动验证按改动文件的文件名找测试：改了 `x.py` 就跑所有叫 `test_x.py` / `x_test.py` 的文件。大仓库里同名测试文件很常见，按名字匹配会把其他子包的同名测试也带上。这些测试和改动无关，多花时间；它们失败时（比如环境问题）还会被当成本次改动的验证失败交给模型。

## 现状与问题

1. **现象**（`pub-swe-sympy-24562`，构建 `0.7.0+27947ada177d`，`%TEMP%\cove-eval-kt7664_z\pub-swe-sympy-24562-1\stderr.txt`）：改了 `sympy/core/numbers.py`，验证执行的是
   `python -m pytest -q sympy/core/tests/test_numbers.py sympy/polys/numberfields/tests/test_numbers.py`。后者测的是 `sympy/polys/numberfields/` 下的代码，和 `sympy/core/numbers.py` 无关。
2. **在当前版本复现（2026-11-24）**：对这次评测的基准提交（未改动的 sympy）运行
   `COVE_VERIFY_ROOT=<sympy> COVE_VERIFY_CHANGED=sympy/core/numbers.py cargo test --release --lib print_checks -- --ignored --nocapture`，
   输出 `tests: ["python -m pytest -q sympy/core/tests/test_numbers.py sympy/polys/numberfields/tests/test_numbers.py"]`，问题仍在。
3. **代码位置**：`pytest_targets`（`src/safety/verify.rs:383-424`）对每个改动的模块，取全仓库里文件名等于 `test_<名字>.py` 或 `<名字>_test.py` 的所有测试文件，不看目录。
4. **规模**：这个 sympy 提交有 566 个 `test_*.py`，其中 34 个文件名出现不止一次（`test_util.py`、`test_printing.py`、`test_functions.py` 各 4 个，`test_solvers.py`、`test_core.py` 各 3 个）。

## 目标

- 同名测试有远有近时，只选最近的：同目录、同包下的 `tests/` 优先；
- 一样近（都不在改动文件所在的子树里）时保持现状，全部选上，不因此退回全量测试；
- sympy 这个例子只选 `sympy/core/tests/test_numbers.py`。

## 范围

- 做：`pytest_targets` 的候选筛选，加测试，`docs/reference.md` 补一句。
- 不做：
  - 找不到同名测试时退回全量测试的策略：0039 已说明要另外设计；
  - `change_impact` 的"仅同包有测试"判断（`src/safety/impact.rs:493` 也按文件名匹配）：它只影响提示里的分级，不决定跑哪些测试，另行检查；
  - 其他语言：Go 按目录、Cargo 按包、dotnet 按项目引用、jest 用 `--findRelatedTests`，都不按文件名匹配。

## 调研与参考

Python 项目常见的测试布局，以及"近"应该怎么算：

| 布局 | 改动文件 | 对应测试 |
|---|---|---|
| 同目录 | `pkg/mod.py` | `pkg/test_mod.py` |
| 包内 `tests/`（sympy） | `pkg/sub/mod.py` | `pkg/sub/tests/test_mod.py` |
| 根目录 `tests/` | `pkg/mod.py` | `tests/test_mod.py` |
| 镜像目录 | `src/pkg/a/mod.py` | `tests/a/test_mod.py` 或 `tests/pkg/a/test_mod.py` |

- **只按共同祖先目录的深度**：sympy 例子可以区分（`sympy/core` 深 2 层，`sympy/polys/numberfields` 只共享 `sympy`，1 层）；镜像目录布局下两个候选和改动文件的共同祖先都是根目录，分不出来。
- **再看末尾目录**：把测试所在目录里的 `tests`、`test` 去掉后，和改动文件所在目录从末尾比，相同的层数越多越近。镜像布局 `src/pkg/a/mod.py` 对 `tests/a/test_mod.py` 末尾共享 `a`，对 `tests/b/test_mod.py` 是 0。
- 两者都相同（比如 sympy 里改 `sympy/sets/handlers/functions.py`，4 个 `test_functions.py` 都只和它共享 `sympy`）时全部保留，和现在一样：去掉它们会退回全量测试，在大仓库里跑不完。

## 方案

`pytest_targets` 里，对每个改动的非测试模块，先按文件名找出候选（不变），再按下面的"距离"只保留最近的一组：

1. `common`：改动文件所在目录和测试文件所在目录（去掉 `tests`/`test` 这一层之前）的共同前缀层数；
2. `tail`：测试文件所在目录去掉 `tests`/`test` 分量后，和改动文件所在目录从末尾起相同的层数；
3. 按 `(common, tail)` 取最大值，等于最大值的候选全部保留。

只有一个候选时不变；改动的是测试文件本身时不变。

## 影响范围

- `src/safety/verify.rs`：`pytest_targets` 和一个新的辅助函数；
- `docs/reference.md`：`done_verify_tests` 一行；
- 行为变化：同名测试文件不止一个时，可能少跑远处的；不会让原来能选出测试的情况退回全量。

## 验证

- 单元测试：sympy 布局（`pkg/core/numbers.py`，`pkg/core/tests/test_numbers.py` 和 `pkg/polys/nf/tests/test_numbers.py`）只选前者；镜像布局选末尾目录相同的那个；一样远时都选；根目录 `tests/` 布局不受影响（已有测试）。
- 再对 sympy 基准提交跑一次 `print_checks`，确认只剩 `sympy/core/tests/test_numbers.py`。
- `cargo test --release`、`cargo clippy --release --all-targets`（不新增警告）。
- 真实模型评测：`pub-swe-sympy-24562` 的 stderr 里 `[验证]` 行只有 `sympy/core/tests/test_numbers.py`（模型改了别的模块时另加对应的测试）。

## 实施计划

- [x] 阶段 1：筛选和单元测试
- [x] 阶段 2：对 sympy 跑 `print_checks`，文档
- [x] 阶段 3：真实模型评测确认

## 待决策

无（见"决策记录"）。

## 决策记录

- 2026-11-24：按 `(共同前缀层数, 去掉 tests 后末尾相同的层数)` 选最近的一组，一样近时全部保留：只用前者分不出镜像布局，全部去掉会退回全量测试。
- 2026-11-24：只去掉 `tests`、`test` 两种目录名，不去掉 `testing`：sympy 里 `sympy/testing` 是正常的子包。
- 2026-11-24：`change_impact` 里同样的文件名匹配不在这次改：它不决定跑哪些测试。

## 验证结果

- 实现：`src/safety/verify.rs` 新增 `nearest`（`verify.rs:431`），`pytest_targets` 对按文件名找到的候选调用它；`docs/reference.md` 的 `done_verify_tests` 一行补充了就近规则。
- 单元测试 `pytest_prefers_the_nearest_test_file_of_the_same_name`：
  - sympy 布局：`pkg/core/numbers.py` 只选 `pkg/core/tests/test_numbers.py`（修改前还会带上 `pkg/polys/nf/tests/test_numbers.py`）；
  - 一样远：`pkg/sets/handlers/functions.py` 两个 `test_functions.py` 都保留；
  - 镜像布局：`src/app/a/util.py` 只选 `tests/a/test_util.py`。
  - 原有的 `pytest_runs_the_test_files_of_the_changed_modules`（根目录 `tests/`）、`deleted_scratch_files_do_not_widen_pytest_to_the_whole_suite` 不变，照常通过。
- 对 sympy 基准提交（`git archive` 取出的未改动代码）运行 `print_checks`：
  - `COVE_VERIFY_CHANGED=sympy/core/numbers.py` → `python -m pytest -q sympy/core/tests/test_numbers.py`（修改前两个文件）；
  - `COVE_VERIFY_CHANGED=sympy/sets/handlers/functions.py` → 4 个 `test_functions.py`（都只和它共享 `sympy`，保持现状）。
- `cargo test --release`、`cargo clippy --release --all-targets`：见 0041 的验证结果（同一次运行）。
- 真实模型评测：`evals/results/20261125-164807-bedrock.json`（全量 29 题，含 sympy 1 次，`--keep`）和 `evals/results/20261125-165758-bedrock.json`（sympy 3 次，`--keep`），构建 `0.8.0+250359b4ace5`，Bedrock Opus（`model_fast=off`）。`pub-swe-sympy-24562` 共 4 次，4/4 通过；4 次的 `[验证]` 行都只有 `python -m py_compile sympy/core/numbers.py` 和 `python -m pytest -q sympy/core/tests/test_numbers.py`，没有再出现 `sympy/polys/numberfields/tests/test_numbers.py`。

## 遗留

- 同名测试一样远时仍然全部选上（sympy 的 `sets/handlers/functions.py` 会跑 4 个无关的 `test_functions.py`）。这时没有更好的依据，去掉它们会退回全量测试；要改需要按导入关系找测试，另开文档。
- `change_impact` 的"仅同包有测试"（`src/safety/impact.rs:493`）仍按文件名匹配，可能把远处同名的测试当成"同包有测试"，未检查。
