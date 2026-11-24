# 0039 自动验证不因删掉的临时脚本退回全量测试

- 状态：已完成
- 日期：2026-11-24（创建）；2026-11-24（完成）
- 关联：0025（自动验证）、0037（发现这个问题的评测）

## 背景

任务结束时，cove 按改过的文件挑出相关的测试来运行（自动验证）。在大仓库里，如果挑不出来，就退回运行整个测试套件。在 sympy 这种规模的仓库里，全量测试跑不完，会占满验证的时限，验证结果只能是"未验证"。

## 现状与问题

1. **现象**：`evals/results/20261124-164305-deepseek.json` 的 `pub-swe-sympy-24562` 第 2 次（构建 `0.7.0+39e48059b2d4`）：
   - 最终只改了 `sympy/core/numbers.py` 和 `sympy/core/tests/test_numbers.py`，自动验证却执行了 `python -m pytest -q`，也就是整个 sympy 套件；
   - 同一时间段 `py_compile` 只编译了这两个文件，说明编译检查拿到的范围是对的。
2. **推断的原因（待实现时复现确认）**：
   - 这个任务里，模型用文件工具建过 `_check_rational.py`、`_env_check.py`、`_install_pytest.py`，之后又用 `rm` 删掉了；
   - `pytest_targets`（`src/safety/verify.rs:383-421`）对每个改过的非测试 `.py` 文件，去找 `test_<名字>.py`，有一个找不到就返回 `None`，`None` 的结果是退回全量测试（`verify.rs:247-250`）；
   - `workspace.changed_paths()`（`verify.rs:943`）很可能仍包含这几个已删除的临时脚本。
3. **后果**：在大仓库里，验证要跑到超时（300 秒），结论是"未验证"。这既浪费时间，也没有起到验证的作用。
4. **复现确认（2026-11-24，当前代码）**：
   - 这次评测留下的检查点（`data/checkpoints/037e7c6b….json`）记了 6 个改动：`sympy\core\numbers.py`、`sympy\core\tests\test_numbers.py` 的 `before` 有内容；`_check_rational.py`、`_env_check.py`、`_install_pytest.py`、`_env_check2.py` 的 `before` 为空（本任务新建）、`after` 有内容，而工作区里这 4 个文件已经不存在。shell 删除文件不会更新检查点，`changed_paths()` 只比较 `before != after`，所以仍然返回它们。
   - 验证账本里第一次全量 `python -m pytest -q` 在 134 ms 内以退出码 1 结束（当时环境里还没装 pytest）；模型装上 pytest 后，第二次全量运行开始后 stderr 再无输出，账本里也没有它的记录（运行没有结束就被评测终止）。
   - 单元测试 `deleted_scratch_files_do_not_widen_pytest_to_the_whole_suite` 按这组文件在临时目录重建，修复前得到 `["python -m pytest -q"]`，推断成立。

## 目标

- 已经不存在的文件，不参与测试范围的选择。
- 改过的文件都找不到对应测试时，行为保持现状（退回全量测试）。这次只修"已删除文件"这一种情况。
- 用 sympy 这次的文件组合做回归测试：改了 `numbers.py`、`tests/test_numbers.py`，另有三个已删除的临时脚本，期望只跑 `test_numbers.py`。

## 范围

- 做：
  - 在 `turn_commands` 或 `pytest_targets` 里跳过已经不存在的路径；
  - 检查 node、dotnet、java 的范围选择是否有同样的问题，有就一起修；
  - 加测试。
- 不做：
  - 修改全量测试的退回策略（比如大仓库里改成不跑）：需要另外设计；
  - 修改验证超时。

## 调研与参考

- `scope_compile` 的范围是对的，`py_compile` 只编译了两个文件。可以对照它是怎么处理不存在的文件的。

## 方案

1. 先写一个测试复现：临时目录里放 `pytest.ini`、`pkg/numbers.py`、`pkg/tests/test_numbers.py`，`changed` 传入这两个文件，再加上 `_tmp.py`（不存在）。断言当前代码返回全量 `pytest -q`，确认推断成立。
2. `turn_commands` 入口处过滤：只保留 `root.join(path)` 仍然存在的路径，再交给 `test_commands`。
   - 删除文件本身也可能影响测试，比如删了一个模块。但这类情况现在的逻辑也处理不了，因为找不到对应测试时会退回全量。
   - 所以过滤只针对本任务新建、之后又删掉的文件：项目里原有、被删掉的文件仍然保留在列表里。
   - 实现时确认 `workspace` 能不能区分"本任务新建"和"原有"两种文件；不能区分时，就全部按"不存在即跳过"处理，并在遗留里写明。
3. node、dotnet、java 的范围函数，各补一个"列表里有已删除临时文件"的测试。

## 影响范围

- `src/safety/verify.rs`；
- 视情况涉及 `workspace` 提供"本任务新建"信息的接口。

## 验证

- 单元测试：
  - 复现测试：修复前失败、修复后通过；
  - node、dotnet、java 各加一个测试。
- `pub-swe-sympy-24562` 跑 3 次，看 stderr 里 `[验证]` 那几行执行的是 `test_numbers.py`，不是全量。
- `cargo test --release`、`cargo clippy --release --all-targets`（不新增警告）。

## 实施计划

- [x] 阶段 1：复现测试
- [x] 阶段 2：过滤和其他语言的检查
- [x] 阶段 3：评测确认

## 待决策

无。

## 决策记录

- 2026-11-24：先用测试复现再改，推断不成立就重新定位原因，并更新本文档。
- 2026-11-24：复现成立。检查点里每个改动都记了改前内容，`before` 为空就是本任务新建的文件，所以只跳过"本任务新建、现在不存在"的路径，项目原有、被删掉的文件仍参与范围选择。
- 2026-11-24：过滤放在 `CommandVerifier::changed`（`verify` 取改动列表的地方），而不是 `turn_commands` 里按 `root.join(path)` 是否存在判断：要不要跳过取决于检查点里的改前内容，只有 `Workspace` 知道。`turn_commands`、`test_commands` 的输入输出不变。
- 2026-11-24：过滤后改动为空（本任务只建过又删掉了临时文件）时不验证，和"没有文件改动"一致；否则空列表会让各语言退回全量测试。
- 2026-11-24：node、dotnet、java 的范围函数本身不改：过滤在它们之前完成，它们拿不到已删除的临时文件。三种语言都确认有同样的问题：node 遇到非源码文件、dotnet 遇到项目外的文件、Maven 遇到根模块下的文件，都会退回全量；node 遇到 `.js` 临时文件还会把不存在的路径交给 `jest --findRelatedTests`。

## 验证结果

- 实现：
  - `src/session/workspace.rs`：新增 `Workspace::deleted_scratch_paths`，返回检查点里 `before` 为空、`after` 有内容、文件现在不存在的路径；
  - `src/safety/verify.rs`：新增 `CommandVerifier::changed`，从 `changed_paths()` 去掉上面这些路径；`verify` 改用它，过滤后为空时返回"不验证"；
  - `docs/reference.md`：`done_verify_tests` 一行补充这条规则。
- 单元测试（`src/safety/verify.rs`，都经真实 `Workspace` 用文件工具新建临时文件、再用 `std::fs::remove_file` 删除）：
  - `deleted_scratch_files_do_not_widen_pytest_to_the_whole_suite`：sympy 这组文件（`pkg/numbers.py`、`pkg/tests/test_numbers.py`，加 3 个删掉的临时脚本），修复前得到 `python -m pytest -q`，修复后得到 `python -m pytest -q pkg/tests/test_numbers.py`；再删掉项目原有的 `pkg/numbers.py`，它仍在改动列表里；
  - `deleted_scratch_files_do_not_widen_node_dotnet_or_java_tests`：node（`npm test -- --findRelatedTests src/a.ts`，临时文件为 `_probe.py`、`_probe.js`）、dotnet（`dotnet test tests/Lib.Tests/Lib.Tests.csproj`，临时文件在项目外）、Maven（`mvn test -q -pl core -am`，临时文件在根模块）；修复前 node 一项得到 `npm test`；
  - `a_task_that_only_left_deleted_scratch_files_is_not_verified`：只建过又删掉临时文件时 `verify` 返回 `None`，修复前会运行验证。
- `cargo test --release`：库 515 通过、6 忽略，集成测试 241 通过，0 失败。
- `cargo clippy --release --all-targets`：5 条警告，都是原有的（`app/console.rs` 2 条、`process.rs`、`safety/permission/policy.rs`、`tests/all/cli.rs`），没有新增。
- 评测确认（待做）：`pub-swe-sympy-24562` 跑 3 次，stderr 里 `[验证]` 那几行执行的应是 `python -m pytest -q sympy/core/tests/test_numbers.py`，不是 `python -m pytest -q`。

### 评测（Bedrock Opus 5.5，`--set model_fast=off`，构建 `0.7.0+27947ada177d`）

- `pub-swe-sympy-24562` 跑 3 次：3/3 通过，平均 13.3 次调用、131 秒，费用 $1.03。结果在 `evals/results/20261124-190008-bedrock.json`。
- 再用 `--keep` 跑 1 次保留现场（`20261124-191051-bedrock.json`，1/1）。stderr 里的 `[验证]` 行只跑了相关的测试文件，没有退回全量，用时 4 秒：
  ```
  python -m pytest -q sympy/core/tests/test_numbers.py sympy/polys/numberfields/tests/test_numbers.py
  ```
- 同一构建的全量评测 `20261124-190241-bedrock.json`：29/29，费用 $2.44。这是纯 Opus 的第一份全量基线。

## 遗留

- **同名测试多选**：按文件名找测试，会把同名但不相关的 `sympy/polys/numberfields/tests/test_numbers.py` 也带上。可以优先选同目录或同包下的测试，见 0042。
- **原本就失败的测试会误导模型**：本机 numpy 2.3 让 `test_numpy_to_float` 报异常，这条测试在改动前就失败。自动验证把它当成"验证未通过"通知模型，模型就去改了和任务无关的 `sympy/core/sympify.py`（保留的现场里，第二轮验证编译了这个文件）。自动验证需要区分"这次改坏的"和"改动前就失败的"，比如改动前先跑一次做基准。见 0041。
- 模型用 shell（`echo > x.py`、脚本生成）建的文件不在检查点里，本来就不参与范围选择，不受这次改动影响。
- 只修了"已删除的临时文件"。改动里仍存在的临时文件（模型建了没删）、找不到对应测试的模块，照旧退回全量测试；大仓库里全量测试跑不完的问题按"范围"一节，需要另外设计。
- 收尾审查、收尾催促、改动影响（`gates/review.rs`、`gates/nudge.rs`、`gates/impact.rs`）也用 `changed_paths()`，没有跟着过滤：它们对已删除文件的处理不在本文档范围内，未检查是否有同类问题。
