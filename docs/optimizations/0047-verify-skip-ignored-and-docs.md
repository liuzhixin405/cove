# 0047 自动验证不因被忽略的文件和文档跑全量测试；验证期间状态栏写"验证中"

- 状态：已完成
- 日期：2026-11-25（创建）；2026-11-25（完成）
- 关联：0042（自动验证按目录就近选测试）

## 背景

在一个 .NET 仓库（含集成测试，单个测试项目约 270 秒）里，一轮任务只用文件工具写了 `.local/commit-msg-docs.txt`（被 git 忽略的提交说明草稿），再用它 `git commit --amend -F`。回复正文生成完以后，任务又停了 5 分多钟，状态栏一直显示"回复中"，界面上只有一行 `[验证] $ dotnet test`，最后报 `dotnet test → 超时（300s）`、"验证未运行"。期间输入的插话只能等任务结束才送出。

数据来自构建 `0.8.0+6dc75068e3d5`（当前版本）：trace 里最后一次模型调用 16:16:10 结束（`tool_calls: 0`），之后没有 `task` 记录，直到验证超时。

## 现状与问题

1. **被忽略的文件、文档也会触发验证，并且把范围扩大到全量。** `CommandVerifier::changed`（`src/safety/verify.rs`）取本任务文件工具改过的全部路径，只去掉"新建后又删掉"的临时文件。`dotnet_tests` 对不属于任何项目的路径直接返回全量 `dotnet test`（"Solution files, shared props, anything outside a project: test everything"）；Cargo、Maven/Gradle 也有同样的退回。于是 `.local/` 下的草稿、`docs/*.md` 这类不参与构建的文件会触发全量测试。提交门禁 `verify_commit` 也一样：只提交文档也会跑全量测试。
2. **验证期间看不出在做什么。** 状态栏只在模型输出和工具开始/结束时更新。回复后的验证不是工具调用，所以状态栏停在"回复中"。验证命令又有意不逐行输出（`run` 里 "No per-line streaming"），界面上看起来就是一直在转圈却没有内容。回复以提问结尾（如"要我现在开始做第 2 个提交吗？"）时，分不清是在等用户回答还是还在做别的事。

## 目标

- 本任务只改了被 git 忽略的文件或纯文档时，自动推断的检查不运行；有代码改动时，这些文件也不会把测试范围扩大到全量。
- 验证命令运行期间，界面明确写出"回复已结束、正在自动验证、不需要操作"：回复结束后的验证先打印一行 `[验证] 回复已结束，正在自动运行检查，不需要你操作`，状态栏显示 `回复已结束，自动验证中（无需操作） · <命令>`；提交门禁里显示 `提交前验证中 · <命令>`。回复以提问结尾、后面又在跑验证时，不会被误认为在等用户回答。

## 范围

- 做：自动推断（`detected`）模式下，选择检查命令前去掉被 git 忽略的路径和文档（`.md`、`.markdown`、`.rst`、`.adoc`）；任务收尾验证和提交门禁都按这个规则。新增 `Event::Activity`，验证时更新状态栏。
- 不做：
  - 用户自己配置了 `done_verify_commands` 时不过滤。命令是用户指定的（可能就是文档检查），有改动就照常运行。
  - `.txt`、`.json` 等不当成文档：它们常是测试数据或配置。
  - 验证期间插话不打断验证。插话打断只针对模型调用；要不要中断正在跑的测试是另一个问题，另开文档。
  - Rust crate 用 `include_str!` 把 README 作为 doctest 的情况：README 改动不再触发验证。这种写法少见，模型自己跑测试或提交前 `cargo test` 仍会覆盖。

## 调研与参考

- `git check-ignore -z --stdin`：按仓库的 `.gitignore`、`info/exclude`、全局 excludes 判断，已跟踪的文件不算忽略；没有匹配时退出码 1，不是 git 仓库时 128，两种情况都按"没有被忽略的"处理。
- 过滤放在选命令之前（`affecting`），而不是改每种生态的 `*_tests`：所有生态一起生效，`changed` 的测试也不受影响。

## 方案

1. `verify.rs` 新增 `CommandVerifier::affecting(paths, cancel)`：非 `detected` 模式原样返回；否则去掉文档扩展名的路径，再用 `process::git::Git` 跑 `check-ignore` 去掉被忽略的路径。
2. `verify()`：`changed()` 之后过 `affecting`，为空就返回 `Ok(None)`（不验证）。
3. `verify_commit()`：过 `affecting`，为空就直接返回 `Passed`，不运行检查。
4. `domain::Event` 新增 `Activity(String)`，`Progress::activity` 发送它；`run()` 新增参数 `after_reply`（收尾验证为真、提交门禁为假）；收尾验证在第一条实际运行的命令前打印一行说明，每条命令开始前按阶段发状态栏文字，全部结束后发 `思考中`。富终端界面更新状态栏；纯文本输出和子代理转发忽略它。

## 影响范围

| 文件 | 改动 |
|---|---|
| `src/safety/verify.rs` | `affecting`、`verify`、`verify_commit`、`run` 的状态更新、测试 |
| `src/domain.rs` | `Event::Activity`、`Progress::activity` |
| `src/app/ui.rs` | 两处事件处理 |
| `docs/reference.md` | `done_verify_tests` 说明补充过滤规则 |

## 验证

- 单元测试：detected 模式下只改了被忽略的文件或 `.md` 时 `verify` 返回 `None`；代码文件加 `.md` 时只按代码文件选命令；配置命令模式不过滤；`verify_commit` 只有文档时返回 `Passed` 且不运行命令。
- `cargo test --release` 全量通过；`cargo clippy --release --all-targets` 不新增警告。

## 验证结果

- `cargo test --release --lib safety::verify`：19 通过（含新增的 `ignored_and_documentation_files_neither_start_nor_widen_detected_checks`）。
- `cargo test --release` 全量：lib 527 通过；集成测试 241 通过，`safety::files_changed_by_shell_are_progress_and_unchanged_ones_are_not` 失败 1 次，单独重跑通过；lib 的 `session::snapshot::tests::auto_deletes_only_files_the_snapshot_before_the_call_holds` 在第一次全量里失败，单独重跑通过。两者都不涉及本次改动的代码，属于并行运行时的偶发失败。
- `cargo clippy --release --all-targets`：5 条警告，均为原有（console.rs、process.rs、policy.rs、tests/all/cli.rs），本次没有新增。
- 界面未用 release 版实际跑一遍：需要一个带慢测试的 .NET 仓库，留给下一次在该仓库使用时确认。

## 遗留

- 验证期间插话不能打断验证（见"不做"）。
- 界面文字的实际效果待在 .NET 仓库里用 release 版确认。
