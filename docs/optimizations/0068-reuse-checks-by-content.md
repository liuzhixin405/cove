# 0068 检查结果按内容复用：同一份改动跨任务提交不再重跑全量测试

- 状态：已完成
- 日期：2026-11-28（创建）；2026-11-28（完成）
- 关联：0062（收尾验证复用模型跑过的结果）；0065 遗留（收尾总是跑全量）

## 背景

cove 自身的全量 `cargo test` 约 150 秒（`~/.cove/verify_ledger.jsonl`，2026-11-28 在 0.8.2 上的最近 5 次是 149-158 秒）。常见的流程是：任务 A 改代码，收尾验证通过；任务 B 里说"提交"，提交前检查又跑一遍全量。两次测的是同一份内容。

## 现状与问题

1. **"已通过"记录每个任务开始时清空。**
   - `EvidenceHook::task_note` 调 `Evidence::reset`（`src/safety/gates.rs:360-366`、`198-199`），`passed` 集合跟着清空。
   - 提交前检查（`src/safety/verify.rs:863-884`）和收尾验证共用 `run`，复用只看这个集合（`verify.rs:907-922`）。所以跨任务时必然重跑。
2. **提交前检查自己的缓存只认判过的暂存 tree**（`src/safety/gates/commit.rs:222-228`），不认收尾验证测过什么。
3. **单 crate 项目收尾总跑全量。** 根目录 `Cargo.toml` 没有 `[workspace]` 时，`whole` 恒为真（`verify.rs:172`、`196-197`）。这一条不在本文档改，见"范围"。
4. **AGENTS.md 三.5 要求模型收尾前自己跑全量**，再加上收尾验证、跨任务提交，同一份改动最多会跑三遍。

## 目标

- 同一份内容测过一次，之后的收尾验证和提交前检查都直接复用，不管中间隔了几个任务。
- 内容变了（包括两个任务之间用户手动改的文件、新增的未跟踪文件），就重跑。不能误复用。

## 范围

- 做：问题 1、2；同步修改 AGENTS.md 三.5（问题 4）。
- 不做：
  - 问题 3，按模块缩小测试范围（0065 遗留的 B2）。cove 是单 crate，改一个文件，测试二进制就要整体重编，模块过滤省不了编译时间。按"验证"一节的计时，看编译占多少再决定，结果写进"遗留"。
  - 模型自己跑的测试按内容记录。工具调用完成后才通知 `Evidence::observe`，拿不到运行前的内容。这类结果仍按原规则只在本任务有效；收尾验证复用它时，按当时的内容记一笔（见方案 3）。

## 调研与参考

- **用快照的 tree（`Snapshots::tree_now`）：不可行。** 快照逐字节存储（`src/session/snapshot.rs:85` 的 `-text`，`94` 的 `core.autocrlf=false`），并额外排除了 `build/`、`dist/`、`*.exe` 等（`60-77`）。项目仓库按自己的 `autocrlf` 和 attributes 暂存，本机全局 `core.autocrlf=true`。同样的内容，两边算出的 tree 哈希不同，没法和提交前检查的 `git write-tree` 比较。
- **HEAD + `git diff HEAD` 的哈希 + 未跟踪文件列表：不够。** 未跟踪文件只有名字没有内容，内容改了也认不出来。
- **采用：在项目自己的仓库里，用一份临时 index 算工作区的 tree。** 把真实 index 复制到临时文件（保留 stat 缓存，所以很快），设 `GIT_INDEX_FILE` 后执行 `git add --all` 和 `git write-tree`。规则和用户真正执行 `git add -A` 完全相同：遵守 `.gitignore`、`autocrlf` 和 attributes，包含未跟踪文件的内容。算出的值可以和提交前检查已经算好的暂存 tree 直接比较。
  - 副作用：`add` 会把改动过的文件写成对象，存进项目的 `.git/objects`，和普通的 `git add` 一样。没有被引用的对象，git gc 会清理。
  - 部分暂存时，暂存 tree 和测过的工作区 tree 不同，不复用，照常跑。这是安全的方向。

## 方案

1. **`work_tree_hash(root)`**，放在 `src/safety/verify.rs`，用 `process::git::Git` 实现：
   - 用 `git rev-parse --git-path index` 找到 index 文件，复制到系统临时目录下的唯一文件名（真实 index 不存在时从空 index 开始）。
   - 带 `GIT_INDEX_FILE` 执行 `add --all` 和 `write-tree`，用完删除临时文件。
   - 不在 git 仓库里或执行失败时返回 `None`，这时不按内容复用，行为和现在相同。
2. **`Evidence` 增加跨任务保留的记录** `by_tree`：`tree 哈希 → 通过的命令`。它放在 `EvidenceState` 之外，`reset` 不清空；最多保留 16 个 tree，超过时淘汰最早的。
   - 新增方法：`record_tree_pass(tree, command)`、`passed_at(tree, command)`。
3. **`CommandVerifier::run` 增加参数 `judged: Option<String>`**，表示这次检查判定的内容：
   - 开始时算一次当前工作区的 tree `before`（只在有 `Evidence` 时算）。
   - 查找键：`judged`（提交前检查传暂存 tree），没有就用 `before`（收尾验证）。
   - 一条命令满足"本任务已通过"（原规则），或者"查找键下已通过"，就复用。
   - 记录：
     - 命令实际运行并通过后再算一次 tree，和 `before` 相同才记到 `before` 名下。运行期间内容变了就不记。
     - 按本任务规则复用的命令，也记到 `before` 名下。
     - 按基线判定"失败都是改动前就有的"那种通过，不记。
4. **`verify_commit` 增加参数 `tree: Option<&str>`。** `commit.rs` 传入它已经算好的 `write-tree` 结果。
5. **AGENTS.md 三.5**：不用在收尾前自己再跑全量。收尾验证会跑；之后跨任务提交，只要内容相同就复用。

## 影响范围

- 改 `src/safety/gates.rs`（`Evidence`）、`src/safety/verify.rs`（`run`、`verify_commit`、新函数）、`src/safety/gates/commit.rs`（传 tree），以及 `AGENTS.md`。
- 每次运行验证时，多执行 1~2 次 `git add --all` + `write-tree`（用临时 index，有 stat 缓存）。只在验证运行时发生，验证本身要几十秒以上。
- 数据格式：记录只在内存里，不持久化。cove 重启后重新开始记。

## 验证

- `verify.rs` 新增测试（临时 git 仓库）：
  - 工作区 tree 的哈希等于 `git add -A && git write-tree` 的结果；改一个未跟踪文件的内容后哈希会变。
  - 第一个任务验证通过。`Evidence::reset` 之后（模拟新任务），对同样内容的 `verify_commit` 直接复用（状态 `reused`）。改了文件之后再提交检查，会重跑。
- `cargo clippy --release --all-targets` 不新增警告。
- 计时，留作 B2 的决策依据：改一个文件后，分别看 `cargo test --no-run`（编译）和 `cargo test`（之后只剩运行）各用多少时间。
- `cargo test` 全量通过。

## 验证结果

- `src/safety/verify.rs` 新增两个测试，都通过：
  - `the_work_tree_hash_is_what_staging_everything_would_give`：哈希等于 `git add --all && git write-tree` 的结果；真实暂存区不受影响；被忽略的文件改了，哈希不变；未跟踪文件的内容改了，哈希会变；不在 git 仓库里时返回 `None`。
  - `a_commit_of_content_tested_in_an_earlier_task_reuses_the_pass`：第一次提交检查实际运行；`Evidence::reset` 之后（相当于新任务），同样内容的提交检查直接复用，摘要是 `already passed on this exact content`，运行次数仍是 1；改了文件再提交，重新运行。
- 原有的 `verify_commit` 和 `run` 测试按新签名补上参数（传 `None`），行为不变，都通过。
- `cargo clippy --release --all-targets`：lib 5 条，`tests/all/cli.rs:209` 1 条，都是原有的，没有新增。
- 全量 `cargo test`：lib 619 passed。`tests/all` 两次分别失败 9 个、6 个，都是超时和时序类断言（钩子超时、子进程清理、进程 5 秒内退出、MCP 子进程清理），单独重跑全部通过。
  - 对照：在改动前的 HEAD（`7f5c00c`）上开一个 git worktree，跑同样的 `cargo test --test all`，也失败 4 个（`background_agent_interrupt_restart_resume_notify_and_merge`、`all_hook_events_run_with_scrubbed_environment_and_session_end_once`、`cancelled_shell_kills_its_spawned_child_before_late_writes`、`cancelled_and_timed_out_mcp_operations_kill_owned_descendants`），用时 231 秒，本次是 235 秒。所以这些失败是本机全量并发时原有的问题，用时也没有被本次改动拉长。
  - 本次多出来的两个（`task_deadline_cancels_a_pending_http_stream_without_hanging`、`hook_contract_and_session_end_are_bounded_and_exactly_once_per_visit`），单独连跑 3 次都通过。
- B2 计时（2026-11-28，本机，改了 10 个源文件后）：`cargo test --no-run` 编译 106 秒；运行时 lib 测试 122-127 秒，`tests/all` 231-238 秒。**运行占了大部分时间，不是编译。** 这推翻了 0065 里"多半是编译"的推测：`cargo test --lib <模块>` 能省掉 `tests/all` 的运行时间，以及 lib 里无关模块的测试。

## 遗留

- **B2（收尾按模块缩小测试范围）有可观的收益，但没有做。**
  - 要做的取舍：收尾只跑模块测试，就会漏掉 `tests/all` 能发现的回归，这部分要留到提交前的全量检查。而全量没有在这份内容上通过过，提交时一定会跑一次。所以只有"改了几轮、中间不提交"时才省得下来。
  - 前面提到的"全量通过覆盖子集"的规则也要一起补上。
  - 要做的话另开文档，先定好"哪些改动必须全量"。
- **本机全量并发时有 4~9 个集成测试偶发超时**，见上面的对照，HEAD 上一样会出现。会挡住提交前检查，值得另开文档：给这些测试放宽时限，或者让它们串行执行。
- 模型自己跑的测试，仍然只在本任务里有效（见"范围"）。
