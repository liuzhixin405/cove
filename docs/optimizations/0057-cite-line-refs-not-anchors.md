# 0057 引用核对不把另一条行号引用当成锚点

- 状态：已完成
- 日期：2026-11-26（创建）；2026-11-26（完成）
- 关联：0027（引用核对）、0055（遗留项）

## 背景

用本机会话回放引用核对（`replay_sessions`）时，4 次重答提示里有 1 次是误报：

```
- src/QuoteHub.Infrastructure/Configuration/PublishingLog.cs:111 — `:117` is not in that file
```

原文是：`src/QuoteHub.Infrastructure/Configuration/PublishingLog.cs:111` 和 `:117`。`:117` 是紧跟着的另一条行号引用，不是代码符号。

## 现状与问题

**P1** [cite.rs](../../src/safety/gates/cite.rs) 的 `analyze` 为每条引用挑锚点时，只排除和**本条**引用重叠的反引号片段，以及像路径的片段。`:117` 自己是一条引用，但和 `:111` 那条不重叠，于是被选成锚点，`needles` 得到 `117`，在第 111 行前后找不到就判 `Mismatch`，触发一次重答。

## 目标

和任何一条引用重叠的片段，以及不含字母的片段（`:117`、`42`、`3-5`），都不作锚点。

## 范围

- 做：P1。
- 不做：调整锚点的其他取法。回放里其余 3 次提示都是真错：行号越界 2 次，符号不在文件里 1 次。

## 调研与参考

回放数据：`~/.cove/sessions`，2026-11-26 的本机全部会话，316 轮没有改文件的回答。

## 方案

`analyze` 里挑锚点的过滤条件增加两项：片段不与任何 `citations[*].span` 重叠；片段文字里至少有一个字母。

## 影响范围

| 文件 | 改动 |
|---|---|
| `src/safety/gates/cite.rs` | `analyze` 锚点过滤；测试 |
| `docs/optimizations/README.md` | 索引 |

## 验证

- 新测试：“`src/AdminApi.cs:16` 和 `:18`”不 `needs_fix`。先在旧代码上确认失败。
- `replay_sessions`：重答提示从 4 次降到 3 次，减少的就是上面那一次。
- `cargo test --release` 全量通过；`cargo clippy --release --all-targets` 不新增警告。

## 验证结果

- 新测试 `line_references_are_not_anchors` 在旧代码上失败，改后通过。
- `replay_sessions`：重答提示 4 → 3，去掉的正是 `PublishingLog.cs:111 — `:117`` 那一次，其余 3 次不变。
- `cargo test --release -- --test-threads=4`：lib 610 通过（7 忽略），集成 269 通过。默认并行度下 `safety::files_changed_by_shell_are_progress_and_unchanged_ones_are_not` 偶发失败（负载高时 9 次启动 Python 超过该测试自设的 30 秒任务时限），单独跑和降低并行度都通过，与本次改动无关。
- `cargo clippy --release --all-targets`：仍是原有 5 条。

## 遗留

无。
