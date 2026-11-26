# 0052 glob/grep 可以包含被忽略的文件

- 状态：已完成
- 日期：2026-11-25（创建）；2026-11-25（完成）
- 关联：0044（工具描述精简）

## 背景

glob 和 grep 用 `ignore::WalkBuilder` 遍历（ripgrep 存在时 grep 调 `rg`），默认遵守 `.gitignore`、`.git/info/exclude`、全局 excludes 和 `.ignore`。模型没有参数能搜被忽略的目录（`node_modules`、`bin`、`obj`、构建输出）。需要看这些目录时，模型只能退回 shell 的 `find`/`du`；在 Windows Git Bash 下遍历几万个小文件很慢，常常超时。

## 现状与问题

1. **没有办法搜被忽略的文件。** `src/tools/search.rs` 的 `walk` 固定启用 ignore 规则；`ripgrep_search` 的参数里没有 `--no-ignore`；`GlobArgs`/`GrepArgs`（`src/tools/mod.rs`）都是 `deny_unknown_fields`，模型传别的参数会直接失败。
2. **显式路径的行为没有写明。** 实测（rg 15.1.0 和内置遍历一致）：`path` 指向被忽略的目录（如 `node_modules/foo`）时，这个目录本身会被搜索，因为遍历的起点不受 ignore 规则过滤；但目录里匹配其他规则的文件（如 `.gitignore` 里的 `*.log`）仍被跳过。
3. **结果为空时没有线索。** 默认搜索因为 ignore 规则什么都没找到时，结果只有 `No files found` / `No matches found`，模型不知道还有被忽略的文件没搜。
4. **内置 grep 达到遍历上限时不说明。** `builtin_grep` 遍历超过 `SCAN_LIMIT`（50,000 个条目）后直接停止，结果里没有任何提示，模型会以为已经搜完。glob 有 `[scan limit reached; narrow path]`。

## 目标

- glob、grep 新增可选布尔参数 `include_ignored`（默认 `false`，行为不变）。为 `true` 时不应用 `.gitignore`、`.git/info/exclude`、全局 excludes、`.ignore`，但仍跳过 `.git` 和受保护路径（`safety::protected`），仍受工作区边界约束。
- `include_ignored=true` 的遍历有时长上限；达到遍历条目上限或时长上限时，结果写明"搜索已停止、结果不完整、缩小 path 或 pattern"。
- 默认搜索结果为空、且搜索范围内有 ignore 文件时，提示可以传 `include_ignored=true`。

## 范围

- 做：
  - glob、grep（内置和 ripgrep 两条路径）支持 `include_ignored`。
  - `include_ignored=true` 时加 20 秒时长上限；遍历条目上限沿用 50,000。
  - 内置 grep 达到遍历上限时在结果里说明（默认模式也说明，修问题 4）。
  - 默认模式结果为空时的提示。
  - 工具描述、`docs/reference.md` 同步。
- 不做：
  - 不改 `walk` 的默认行为。它还被 `repo_map`（`LocalTools::files`）、`safety/gates/cite.rs`、`safety/impact.rs` 使用，这些调用方不需要被忽略的文件。
  - 不改 `@路径` 补全、explore 子 Agent 的遍历逻辑（explore 使用同一套 glob/grep 工具，自然获得这个参数，默认值不变）。
  - 不改 glob 默认遵守 `.gitignore` 的行为（Claude Code 的 Glob 默认不遵守）：默认遵守能让结果集中在源码上，改默认值会让大仓库里的宽泛模式频繁撞到上限。
  - 不做"有 N 个被忽略的匹配"的计数提示，原因见决策记录。

## 调研与参考

- VS Code Copilot `grep_search`：默认遵守 `.gitignore`、`files.exclude`、`search.exclude`；参数 `includeIgnoredFiles` 关闭这些过滤，描述原文："only set it when you want to search in ignored folders like node_modules or build outputs"。结果默认 100 条、上限 200 条，20 秒超时。
- Claude Code：Grep 遵守 `.gitignore`，要搜被忽略的文件时直接传它的路径（"To search a gitignored file, Claude passes its path directly"）；Glob 默认不遵守 `.gitignore`（设 `CLAUDE_CODE_GLOB_NO_IGNORE=false` 才遵守）。
- `ignore::WalkBuilder::standard_filters(false)` 一次关掉 hidden、parents、`.ignore`、`.gitignore`、全局 excludes、`.git/info/exclude`；`filter_entry` 仍然生效，`.git` 和受保护路径的过滤不受影响。
- ripgrep `--no-ignore` 关掉同样一组 ignore 文件（含 `.rgignore` 和父目录的 ignore 文件），`--glob=!…` 覆盖规则仍生效，`.git` 和受保护路径的排除不受影响。

## 方案

1. `search.rs`：
   - 新增 `walker(base, include_ignored)`；`walk(base)` 等于 `walker(base, false)`，其他调用方不变。`include_ignored` 时 `standard_filters(false)`。
   - 新增 `Budget`：记录条目数和截止时间（只有 `include_ignored` 时有截止时间），超出时给出 `Stop::Entries` / `Stop::Time`。glob 和内置 grep 每个条目检查一次。
   - `GrepOptions` 加 `include_ignored`。`ripgrep_search` 为 `true` 时加 `--no-ignore`，并在 20 秒到时结束 rg，保留已收到的结果。
   - `builtin_grep`、`ripgrep_search` 返回 `Found { lines, stop }`；`grep_result` 根据 `stop` 在末尾追加说明行：`... search stopped after scanning 50000 entries (or 20 s); results are incomplete, narrow path or pattern`。说明行以 `... ` 开头，和"more not shown"一致，界面摘要不把它当成匹配。
   - glob 的说明行同样改成 `... ` 开头的格式。
   - 结果为空且 `include_ignored=false`、`base` 到工作区根之间有 `.gitignore`/`.ignore`，或根目录有 `.git/info/exclude` 时，结果为 `No files found (files excluded by .gitignore were skipped; include_ignored=true searches them)`，grep 同理。
2. `tools/mod.rs`：`GlobArgs`、`GrepArgs` 加 `include_ignored`；glob 描述改为"skips .gitignore'd files unless include_ignored"，并加半句"列出被 .gitignore 排除的内容先用 `git status --short --ignored` 或 `git clean -ndX`"；两个工具的 schema 加 `include_ignored` 属性，一句话描述。
3. `app/render.rs`：grep、glob 的界面摘要识别带提示的空结果（`starts_with`），glob 摘要不把 `... ` 说明行算成文件（有说明行时写成"N+ 个文件"）；带 `include_ignored=true` 的调用，标题后面加"含被忽略的文件"，让人知道这次搜索可能较慢。
4. `docs/reference.md`：搜索一节写明参数、上限和显式路径的行为。

## 影响范围

- `src/tools/search.rs`、`src/tools/mod.rs`（只改 glob/grep 的参数、调用和定义）、`src/app/render.rs`（摘要）、`tests/all/core.rs`、`tests/all/tools.rs`、`docs/reference.md`。
- 默认行为不变，只有两处输出变化：空结果在有 ignore 文件时多一句提示；内置 grep 达到遍历上限时多一行说明，glob 的遍历上限说明换了措辞。
- 工具 schema 多一个可选参数，旧会话的调用不受影响。

## 验证

- `tests/all/core.rs` 的 `search_respects_ignore_and_rejects_invalid_arguments` 扩展为：默认跳过被忽略文件并给出提示；`include_ignored=true` 找到 `node_modules`、`build` 里的文件；`.git` 和受保护目录仍被跳过；显式传被忽略的目录照搜。内置和 ripgrep 两条路径都跑。
- `search.rs` 单元测试：`grep_result` 带 `stop` 时的说明行；`Budget` 的条目上限和时长上限；glob 达到条目上限时的说明。
- `cargo test --release` 全量；`cargo clippy --release --all-targets` 不新增警告。

## 实施计划

- [x] 阶段 1：search.rs 遍历、预算、结果格式
- [x] 阶段 2：mod.rs 参数和描述、render.rs 摘要
- [x] 阶段 3：测试、reference.md、验证

## 待决策

无。

## 决策记录

- 2026-11-25：`include_ignored=true` 的时长上限定为 20 秒（同 Copilot），遍历条目上限沿用现有的 50,000，结果条数上限不变（glob 200、grep 100）。默认模式不加时长上限，保持原行为。
- 2026-11-25：显式传入被忽略的路径时照搜（保持现状，和 rg、Claude Code"直接传路径"一致），不要求同时传 `include_ignored`；这个目录里再匹配其他 ignore 规则的文件（如 `*.log`）仍被跳过，需要时传 `include_ignored=true`。不额外判断"起点是否被忽略后自动切换"：要为此读取并匹配各级 ignore 文件，得到的效果和传参数一样。
- 2026-11-25：不做"有 N 个被忽略的匹配"的计数。计数需要在结果为空时再做一次不带 ignore 规则的遍历，开销正是要避免的那部分（遍历 `node_modules` 这类大目录），最坏要多花 20 秒。改为零开销的静态提示：只在结果为空、搜索范围的上级目录里确实有 ignore 文件时出现。更深层子目录里的 `.gitignore` 和全局 excludes 不检查，这种情况不提示。

## 验证结果

- `cargo test --release --lib search`：`tools::search` 的 5 个单元测试通过，新增 `searches_that_stop_early_say_so_and_empty_ones_point_at_include_ignored`（glob 达到条目上限、时长为 0 时的 Time 说明、内置 grep 达到条目上限、空结果在有/没有 `.gitignore` 时的措辞、`include_ignored=true` 时不提示）。
- `cargo test --release --test all -- search glob grep`：新增 `core_tests::search_can_include_ignored_files_but_never_git_or_protected_directories`，内置搜索和 ripgrep（本机 rg 15.1.0）两条路径都通过：`include_ignored=true` 的 glob/grep 找到 `ignored.txt`、`node_modules/pkg/index.js`、`node_modules/pkg/debug.log`，结果里没有 `.git/` 和受保护的 `.cove/`；默认模式不含被忽略文件，空结果带提示；`path: "node_modules/pkg"` 照搜但跳过 `*.log`，加 `include_ignored=true` 后找到。原有 `search_respects_ignore_and_rejects_invalid_arguments`、`builtin_grep_and_glob_follow_the_go_semantics`、`ripgrep_backed_grep_gives_the_same_results`、`glob_caps_results_at_two_hundred` 不变并通过。
- `app::render` 测试：带提示的空结果摘要为"没有匹配"/"没有文件"，带说明行的 glob 结果摘要为"2+ 个文件"，标题带"含被忽略的文件"。
- `cargo test --release`：第一次全量运行时 lib 里 `session::snapshot::tests::a_task_takes_one_snapshot_plus_one_per_changing_shell_command`、`auto_deletes_only_files_the_snapshot_before_the_call_holds` 失败（并行负载下的已知偶发问题，与本改动无关）；`--test-threads=1` 单独重跑 snapshot 的 16 个测试全部通过。随后 `cargo test --release --no-fail-fast` 全量：lib 532 通过、7 忽略；集成测试 243 通过；0 失败。
- `cargo clippy --release --all-targets`：只有原有的 5 条警告（`src/app/console.rs` 2 条、`src/process.rs`、`src/safety/permission/policy.rs`、`tests/all/cli.rs` 各 1 条），没有新增。
- 只对改过的 4 个 Rust 文件跑了 `rustfmt --edition 2024 --config max_width=100`，差异全部落在本次改动的行里。

## 遗留

- ripgrep 路径的 20 秒时长上限没有自动化测试（需要真的跑 20 秒，或把时长做成可注入的参数）；内置遍历的时长上限由单元测试用零时长覆盖。
- 没有在真实的大 `node_modules`（几万个文件）上实测耗时和截断提示，留待之后在实际项目里观察。
- 空结果提示不检查更深层子目录里的 `.gitignore` 和全局 excludes（见决策记录），这些情况下空结果不带提示。
- `docs/reference.md` 曾写"每次搜索最多遍历 50,000 个条目（默认模式也是）"，但 ripgrep 路径（`ripgrep_search`）不数条目，只有 `include_ignored` 时的 20 秒和匹配行数上限；0050 的审查修正把这句改成：`glob` 和内置搜索的 `grep` 有 50,000 条目上限，用 ripgrep 的 `grep` 只有时长和行数上限。
