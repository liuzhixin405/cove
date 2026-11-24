# 0041 自动验证区分改动前就失败的测试

- 状态：实施中
- 日期：2026-11-24（创建）
- 关联：0039（遗留第 2 条）、0037（发现这个问题的评测题 `pub-swe-sympy-24562`）、`regression` 工具（`src/agents/regression.rs`，同一套换回改前内容的机制）

## 背景

任务结束时，自动验证运行和改动相关的测试，有失败就判"未通过"，把失败输出交回模型去修。测试在改动前就失败（环境问题、依赖版本、本来就坏的用例）时，这个判断是错的：失败和本次改动无关，模型却被要求修它，于是去改任务以外的代码。

## 现状与问题

1. **现象**（`pub-swe-sympy-24562`，构建 `0.7.0+27947ada177d`，`--keep` 保留的现场 `%TEMP%\cove-eval-kt7664_z\pub-swe-sympy-24562-1`）：
   - 第 1 次验证：`python -m pytest -q sympy/core/tests/test_numbers.py sympy/polys/numberfields/tests/test_numbers.py` 失败，会话里交给模型的失败用例只有 `sympy/core/tests/test_numbers.py::test_numpy_to_float - ValueError`；
   - 模型随后读并改了和任务无关的 `sympy/core/sympify.py`（`_convert_numpy_types`，让 NumPy 2 下不再出现 `np.float64(...)`）；
   - 第 2 次验证多带上了 `test_sympify.py`，失败用例是 `test_sympify_factorial`、`test_sympify_numpy`；模型想用 `git stash` 在改动前的代码上跑一遍对比（被拒绝），之后又改了 `sympy/parsing/sympy_parser.py`；
   - 最终改动 5 个文件，其中 3 个和任务无关。
2. **在当前环境复现（2026-11-24）**：用 `git archive HEAD` 取出这次评测的基准提交（未改动的 sympy），本机 numpy 2.3.3、Python 3.13.7、pytest 9.2.0：
   - `python -m pytest -q sympy/core/tests/test_numbers.py sympy/polys/numberfields/tests/test_numbers.py`：`FAILED sympy/core/tests/test_numbers.py::test_numpy_to_float`，`1 failed, 109 passed, 1 xfailed in 6.77s`；
   - `python -m pytest -q sympy/core/tests/test_sympify.py`：`test_sympify_factorial`、`test_sympify_numpy`、`test_issue_14706` 失败，`3 failed, 47 passed`。
   - 也就是说，两次"验证未通过"里的失败用例全部在改动前就失败。
3. **代码路径**（当前版本）：`CommandVerifier::run`（`src/safety/verify.rs:767-900`）只看退出码，非 0 就是 `Outcome::Failed`，详情是输出末尾 12 KB；没有任何和改动前对比的步骤。`src/engine/agent.rs:2361-2383` 把这份详情原样交给模型，并要求"Fix the cause"。
4. **已有的基础**：`regression` 工具已经实现"把本任务改过的文件临时换回改前内容、跑测试、再换回来"：`Workspace::swap_in` 先写日志再换文件，`swap_restore` 只恢复仍是改前内容的文件，进程崩溃时 `/resolve` 能按日志放回（`src/session/workspace.rs:747-858`）。改前内容来自本任务的回滚点。

## 目标

- 测试命令失败时，用同一条命令在改动前的代码上再跑一次，按用例比较：只有改动后**新出现**的失败才算"验证未通过"。
- 失败的用例全都在改动前就失败时，判"通过"，在验证进度行里写明"N 个用例改动前就失败，与本次改动无关"，不要求模型修。
- 有新失败时仍判"未通过"，交给模型的详情里分开列出"新出现的失败"和"改动前就失败、不需要修"的用例。
- 拿不到用例级结果时，退回现在的行为（只看退出码），不因为这项功能多判"通过"。

## 范围

- 做：
  - 任务结束时的自动验证（`CommandVerifier::verify`）；
  - pytest 和 `go test` 的用例级比较；
  - 文档（`docs/reference.md` 的 `done_verify_tests`）。
- 不做：
  - 提交门禁（`/commit`、`git commit` 前的验证）：它检查的是暂存内容，可能包含本任务以外的改动，"本任务改动前"不是它的基准；
  - `cargo test`：默认在第一个有失败的测试二进制处停下，后面的二进制没有运行，拿到的失败集合不完整；判"全是旧失败"会漏掉没跑的部分。要支持得改成 `--no-fail-fast`，这会改变推断出的命令、和模型自己跑的命令的复用匹配，另行考虑；
  - `dotnet test`、jest/npm、Maven/Gradle：输出格式多（VSTest 和 Microsoft.Testing.Platform 不同，jest 要 `--json` 才稳定），这次只按退出码，见"遗留"；
  - 构建、编译检查（`cargo check`、`py_compile`、`dotnet build`）：编译失败没有"用例"，改动前能编译就说明是新问题。

## 调研与参考

- **基准什么时候跑**：
  - 任务第一次改文件前跑：那时还不知道会改哪些文件、选哪些测试；而且每个任务都多一次测试，编辑工具要等它跑完；
  - 失败后，用回滚点里的改前内容换回去再跑：只在失败时花时间；`regression` 工具已经这么做，崩溃恢复现成。选这个。
- **跑什么**：
  - 只跑失败的用例（pytest 的节点 ID）：更快，但 Go 的 `-run` 要拼正则，两种框架各写一套；
  - 跑同一条命令：和当前这次的范围完全一致，比较简单；成本是同一范围再跑一次，sympy 这次是 4 秒（本机冷启动 22 秒，pytest 报告 6.77 秒）。选这个，并按"命令 + 改动文件列表"在本任务内缓存基准结果，同一范围只跑一次。
- **各框架能否拿到用例级结果**（都按当前推断出的命令格式，不改命令）：
  - pytest：`-q` 末尾的 short test summary 默认列出 `FAILED <节点 ID> - …` 和 `ERROR <节点 ID> - …`（默认 `-rfE`），最后一行有 `N failed`、`M error(s)` 合计，可以用来核对是否解析全了。退出码 1 表示"有用例失败"，2 是收集阶段出错被中断、4 是用法错误、5 是没收集到用例。
  - `go test`：不加 `-v` 时，失败的用例打印 `--- FAIL: TestX (…)`（子测试缩进），随后 `FAIL\t<包>\t…`；通过的包打印 `ok  \t<包>`。编译失败是 `[build failed]` / `[setup failed]`，超时是 `panic: test timed out`。会跑完所有包。
  - `cargo test`：`test <名字> ... FAILED`（`regression.rs` 的 `cargo_outcomes` 已能解析），但见"范围"，默认 fail-fast。
  - `dotnet test`：VSTest 打印 `Failed <名字> [时间]`，MTP 格式不同；jest 文本输出是 `●` 开头的块，稳定格式要 `--json`。
- **本任务新建的文件**：回滚点里改前内容为空。`swap_in` 只能换内容，不能临时删掉文件，所以新建的文件在基准运行时仍然在。新建的测试文件在基准里也会失败（被测代码还是旧的），不能据此认为它"改动前就失败"。

## 方案

1. **`Workspace::task_baseline()`**（`src/session/workspace.rs`）：返回本任务改过的文件的改前内容（`(路径, 文本)`）和本任务新建、现在仍存在的文件列表。下面任一情况返回 `None`（不做基准）：有结果不确定的编辑；改前或当前内容不是 UTF-8（`swap_in` 只按文本换，GBK 文件会被写成空内容）；项目原有、本任务改过的文件现在不存在（`swap_in` 不能重建）；文件读不了。新建后又删掉的临时文件跳过。
2. **识别框架**（`src/safety/verify.rs`）：命令是 `pytest …`、`python -m pytest …`（`python`/`python3`/`py`）或 `go test …`，且不含 `&&`、`;`、`|`。
3. **解析失败用例**：
   - 严格解析（用于改动后这次）：pytest 要求退出码 1、`FAILED`/`ERROR` 行数等于末行合计且大于 0；Go 要求退出码 1、每个 `FAIL\t<包>` 都能对上至少一条 `--- FAIL`、没有 `[build failed]`/`[setup failed]`/超时 panic。用例名 pytest 用节点 ID，Go 用 `<包>/<用例>`。任一条件不满足就不比较。
   - 宽松解析（用于基准）：能解析出多少算多少，没解析出的用例视为"改动前没有失败"。这只会让更多失败被算作新失败，不会多判"通过"。
4. **不做基准的情况**：本任务新建了 `conftest.py`（pytest）或 `_test.go`（Go）；改动后这次运行超过 120 秒（同样的时间再跑一次代价太大）；`task_baseline()` 为 `None`；已有未恢复的换回日志。
5. **跑基准**：`swap_in` 换回改前内容 → 用同一条命令、同样的时限运行 → 无论结果、取消与否都先 `swap_restore`（另有 drop 守卫兜底；恢复失败报 `Uncertain`）。进度行 `[验证] 改动前基准 $ …`、`[验证] 改动前基准 … → N 个用例失败（Ns）`。基准超时就不比较。
6. **比较**：改动后失败的用例里，不在基准失败集合里的是"新失败"；pytest 节点 ID 的文件是本任务新建的，一律算新失败。
   - 没有新失败：这条命令算通过，进度行 `[验证] … → 通过（N 个失败的用例改动前就失败，与本次改动无关）`，账本状态记 `preexisting`，继续下一条命令；
   - 有新失败：照旧判失败，交给模型的详情开头先列新失败，再列改动前就失败、不需要修的用例，之后是原来的输出末尾。
7. **回滚点与"改动前"的含义**：基准是"本任务用文件工具改过的文件换回改前内容"。shell 改的文件不在回滚点里，基准里保持改动后的样子。

## 影响范围

- `src/session/workspace.rs`：新增 `TaskBaseline`、`task_baseline()`；
- `src/safety/verify.rs`：`run` 增加是否做基准的参数（任务结束验证开，提交门禁关）；新增框架识别、失败解析、基准运行和缓存；
- `docs/reference.md`：`done_verify_tests` 一行补充这条规则。
- 行为变化：只在测试命令失败时多跑一次；全是旧失败时结果由"未通过"变为"通过"。验证账本多一种状态 `preexisting`。换回日志沿用 `regression` 的文件，崩溃后 `/resolve` 同样能恢复。

## 验证

- 单元测试：
  - pytest、Go 输出的严格/宽松解析（合计对不上、退出码不对、编译失败时不比较）；
  - 比较规则（新建测试文件里的失败算新失败）；
  - `task_baseline()`：新建、删除、改动的文件分类；
  - 端到端：临时目录里建一个 pytest 项目，一个用例改动前就失败，任务改了另一个文件；验证判"通过"、文件内容恢复成改动后；再让任务改坏一个原本通过的用例，判"未通过"，详情分开列出两类失败。
- `cargo test --release`、`cargo clippy --release --all-targets`（不新增警告）。
- 真实模型评测：`pub-swe-sympy-24562` 跑 3 次 + `--keep` 1 次，确认 stderr 里出现"改动前基准"行，`test_numpy_to_float` 被标为改动前就失败，模型不再改 `sympify.py` 等无关文件。

## 实施计划

- [x] 阶段 1：`task_baseline()` 和解析、比较函数，带单元测试
- [x] 阶段 2：接入 `CommandVerifier::verify`，端到端测试
- [x] 阶段 3：文档
- [ ] 阶段 4：真实模型评测确认

## 待决策

无（下列各点已定，见"决策记录"）。

## 决策记录

- 2026-11-24：基准在测试失败后再跑，用回滚点里的改前内容和 `swap_in`/`swap_restore`，不在任务开始前跑：开始前不知道要跑哪些测试，而且成功的任务不该多花时间。
- 2026-11-24：基准跑同一条命令，不只跑失败用例；按"命令 + 改动文件列表"缓存，本任务内同一范围只跑一次。改动后这次运行超过 120 秒时不跑基准。
- 2026-11-24：这次只支持 pytest 和 `go test`。`cargo test` 默认在第一个失败的测试二进制处停下，失败集合不完整，不能据此判"通过"；dotnet、jest 等输出不稳定。它们都退回只看退出码。
- 2026-11-24：失败全是旧失败时判"通过"，不是"未运行"：这些用例在改动前后结果一样，本次改动没有让任何测试变坏。进度行写明，账本记 `preexisting`，不记为"已通过"可复用的检查。
- 2026-11-24：本任务新建的测试文件里的失败一律算新失败；新建了 `conftest.py` 或 `_test.go` 时不做基准。
- 2026-11-24：提交门禁不做基准：它验证的是暂存内容，不一定是本任务的改动。
- 2026-11-24：交给模型的详情用英文（和外层 `[automatic verification failed …]` 一致），进度行和汇总行用中文。
- 2026-11-24（实施中补充）：pytest 的基准运行带 `PYTHONDONTWRITEBYTECODE=1` 和指向空目录的 `PYTHONPYCACHEPREFIX`。Python 和 pytest 的字节码缓存按源文件的修改时间（秒）和大小判断是否过期，换回的文件和改动后的文件大小相同、又在同一秒内写入时，会跑到旧的字节码；基准既不读也不写 `__pycache__`，换回后模型自己的运行也不会读到基准留下的字节码。Go 的缓存按内容，不需要处理。
- 2026-11-24（实施中补充）：除了显式恢复，换回期间另有 drop 守卫调用 `swap_restore`，验证被中途丢弃时也会放回改动后的内容；进程被杀时靠换回日志和 `/resolve`。

## 验证结果

- 实现：
  - `src/session/workspace.rs`：新增 `TaskBaseline` 和 `Workspace::task_baseline()`（`workspace.rs:761`）；
  - `src/safety/verify.rs`：`run` 增加 `baseline` 参数（`verify` 传 `true`，`verify_commit` 传 `false`）；新增 `judge`、`run_baseline`（`verify.rs:1004`、`1058`），`Runner`、`failing_tests`/`pytest_failures`/`go_failures`、`Judged`、换回守卫 `Swapped`；
  - `docs/reference.md`：`done_verify_tests` 一行补充这条规则。
- 单元测试（`src/safety/verify.rs`）：
  - `test_level_failures_are_read_only_from_complete_runs`：用 sympy 这次的 short test summary 原文解析出 `sympy/core/tests/test_numbers.py::test_numpy_to_float`；合计对不上、收集中断（退出码 2）、超时时严格解析返回 `None`，宽松解析仍给出 ID；Go 的子测试、`[build failed]`、找不到 `--- FAIL` 的包失败；`cargo test`、`npm test`、带 `&&` 的命令、`dotnet test` 不识别；
  - `failures_in_test_files_the_task_created_are_always_new`：新建测试文件里的失败即使基准里也失败，仍算新失败；新建 `conftest.py`/`_test.go` 时不做基准；
  - `the_task_baseline_holds_before_texts_and_created_files`：改过的文件给出改前文本，新建的列为 created，新建又删掉的跳过；项目原有、改过又被删掉的文件使结果为 `None`；
  - `failures_that_predate_the_task_do_not_fail_verification`（本机有 pytest 时运行）：临时 pytest 项目里 `test_legacy` 改动前就失败，任务改了 `calc.py` 但没弄坏 `add`：判"通过"，汇总写明改动前就失败的 `test_legacy`，`calc.py` 是改动后的内容，没有遗留换回日志；同一命令走提交门禁（不做基准）判"失败"；再把 `add` 改坏：判"失败"，详情分开列出新失败 `tests/test_calc.py::test_add` 和改动前就失败的 `tests/test_calc.py::test_legacy`，文件仍是改动后的内容。
- `cargo test --release --no-fail-fast`：库 524 通过、2 失败、6 忽略；集成测试 240 通过、2 失败。4 个失败是 `session::snapshot::tests::a_task_takes_one_snapshot_plus_one_per_changing_shell_command`、`session::snapshot::tests::auto_deletes_only_files_the_snapshot_before_the_call_holds`、`integrations::cancelled_and_timed_out_mcp_operations_kill_owned_descendants`、`safety::files_changed_by_shell_are_progress_and_unchanged_ones_are_not`，都和快照、进程计时有关，不经过本文改动的代码；单独重跑 4 个全部通过（本机同时有其他编译在跑，负载高时这几项超时）。前一次全量运行里另有 `engine::progress::tests::the_same_error_from_different_commands_is_one_error` 失败，当时 `src/engine/progress.rs` 正被另一项改动修改，之后单独运行通过。
- `cargo clippy --release --all-targets`：5 条警告，都是原有的（`app/console.rs` 2 条、`process.rs`、`safety/permission/policy.rs`、`tests/all/cli.rs`），没有新增。
- 真实模型评测（待做）：见"验证"一节。

## 遗留

- `cargo test` 没有用例级比较：默认 fail-fast，失败集合不完整。改成推断 `cargo test --no-fail-fast` 后可以复用 `regression.rs` 的 `cargo_outcomes`，要另开文档，连同命令复用匹配一起考虑。
- dotnet（VSTest 的 `Failed <名字>`、MTP）、jest（`--json`）、Maven/Gradle（surefire 报告）没有用例级比较，仍只看退出码。
- 基准里 shell 改过的文件保持改动后的样子（回滚点只记文件工具的改动）；本任务新建的非测试文件在基准运行时仍然存在。前者可能让基准不纯，后者只会让更多失败算新失败。
- 换回期间，同一工作区里的其他读者（并行的只读子 Agent、编辑器）会读到改前内容；对被换回文件的编辑会被拒绝（`swap_in` 的既有行为），和 `regression` 工具相同。
- 判"通过"时模型不会收到改动前就失败的用例列表，它的最终回复里可能仍写"测试全部通过"；用户能在进度行看到说明。
