# 0066 快照测试在并发负载下不再因 10 秒预算偶发失败

- 状态：已完成
- 日期：2026-11-28（创建）；2026-11-28（完成）
- 关联：0065（提交前检查被本问题拦下）

## 问题

全量 `cargo test` 时，`session::snapshot` 的两个测试偶发失败：

- `a_task_takes_one_snapshot_plus_one_per_changing_shell_command`：快照列表为空（`src/session/snapshot.rs:1751`）。
- `auto_deletes_only_files_the_snapshot_before_the_call_holds`：返回 `Unattended`（`snapshot.rs:1840`）。

复现：这两个测试单独跑通过（约 13 秒）；`session::snapshot` 的 16 个测试并发跑失败（两次分别失败 2 个、1 个）；`--test-threads=1` 全部通过（约 126 秒）。

原因：工具调用前拍快照的预算是 `SNAPSHOT_BUDGET = 10s`（`snapshot.rs:41`，在 `1428` 使用）。并发时十几个测试同时起 git，第一次快照超过 10 秒，`before_call` 返回 `Slow`，并把 `warming` 置为 true（`713-730`）。后台 staging 结束前，之后的调用一律返回 `Busy`（`701-703`）。只有 `Taken` 才记 `task_taken`，所以测试里后续的调用都落进 `Busy`，列表为空；没有快照兜底时，auto 模式不放行 `rm`（`1789-1791`）。

线上行为（慢就跳过并警告）是设计如此。问题在于测试依赖"10 秒内一定拍完"。

## 改法

`SNAPSHOT_BUDGET` 在测试构建下放宽到 120 秒：`Duration::from_secs(if cfg!(test) { 120 } else { 10 })`。这只是上限，正常情况下不会等这么久。

- 用到它的只有 `snapshot.rs` 自己（`1428`、`1439-1440` 的提示文字、`1484` 的 `tree_now`），`tests/` 下没有引用，所以 `cfg!(test)` 能覆盖所有调用。生产构建仍是 10 秒。
- 不选的方案：给 `Snapshotting` 加 `budget` 字段（要改结构体和生产构造处 `src/commands/sessions.rs:185`，收益相同）；快照测试共用一把锁串行跑（挡不住其他模块的测试同时跑 git 带来的负载）。

## 验证

- `cargo test --lib session::snapshot` 并发连续跑 3 次，全部通过。
- 如果仍失败，说明是 git 报错（`Before::Failed`）那条路，另行排查。

## 验证结果

- `cargo test --lib session::snapshot` 并发连跑 3 次：16 passed ×3（84s、79s、59s）。
- 全量 `cargo test` 的 lib 部分两次都是 619 passed。
- 生产构建仍是 10 秒：`cfg!(test)` 只在 lib 的单元测试构建里为真。
