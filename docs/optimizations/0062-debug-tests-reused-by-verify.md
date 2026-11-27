# 0062 测试统一用 debug 构建，收尾验证复用模型跑过的结果

- 状态：已完成
- 日期：2026-11-27（创建）；2026-11-27（完成）
- 关联：0061（触发本文的任务）；0047、0048（收尾验证）

## 背景

0061 只改了一行默认值，任务却用了 8m44s（trace `ms=523853`）。当时按 trace 和账本拆开看，同一套测试跑了两遍：模型按 AGENTS.md 手动跑一次 `cargo test --release`，收尾的自动验证又跑一次 `cargo test`。

| 环节 | 耗时 |
|---|---|
| 模型按 AGENTS.md 跑 `cargo test --release` | 约 259 秒 |
| clippy、rustfmt | 约 45 秒 |
| 收尾自动验证跑 `cargo test`（`verify_ledger.jsonl`，`duration_ms=181735`） | 约 182 秒 |
| 6 次模型调用和编辑 | 约 1 分钟 |

各段都是取整估算、彼此有重叠，合计比 524 秒的总耗时多 20 余秒。数据来自本机 2026-11-25 至 2026-11-26 的 `~/.cove/trace.jsonl` 和 `verify_ledger.jsonl`（0061 实施那次任务）；当时没有抄录 `build` 字段，现在这两条记录已随文件滚动查不到，这张表只能当量级参考。

## 现状与问题

1. **命令原文不同，不复用。** 收尾验证判断"模型已经跑过"时，比较的是规整空白后的整条命令原文（`src/safety/gates.rs` 的 `normalize_command`、`Evidence::passed`、`observe`）。`cargo test --release` 和自动推断出的 `cargo test`（`src/safety/verify.rs` 的 `test_commands`）不是同一条，不复用。
2. **两个 profile 的产物不共用。** debug 和 release 各自编译，第二遍要重新编译。
3. **之后改文件也会作废。** 非只读的工具调用一律清掉已通过的记录（`observe` 里 `!READ_ONLY_TOOLS.contains(&name)`，`write`/`edit` 都算），可能改文件的 shell 命令也一样（`may_change_files`）。所以测试之后再格式化、拷回，或者回填文档，同样让结果作废。
4. **规则本身有冲突。** `.cove/rules.md` 允许小改动只跑针对性测试，而"跑测试只用 `cargo test`、不加参数"如果写成绝对规则，两者没法同时遵守；带过滤参数的运行也匹配不上整条 `cargo test`（`gates.rs` 的 `normalize_command`）。
5. **后台命令的记录来得太晚。** shell 默认 120 秒后把命令转到后台（`src/tools/mod.rs:1124` 的 `SHELL_TIMEOUT`），结果要等下一次模型调用前才由 `src/engine/agent.rs` 的 `background_finished`（`src/safety/gates.rs:374`）交给 `observe`；而成功的 shell 命令是无条件写进已通过的（`gates.rs:302-310`），不检查命令开始之后有没有改过文件。文档自己测到的收尾测试是 182 秒，超过 120 秒，所以"先跑测试、再回填文档"很可能得到一条**过期**的通过记录：回填的 `write` 先清了记录，后台结果随后又把它记回去。

## 目标

- 模型和收尾验证用同一条命令，全量测试只编译一次；回填文档（不动源码）之后收尾验证可能再跑一次测试，但不用重新编译。
- 规则不互相矛盾：小改动可以只跑针对性测试，收尾那一次固定是整条 `cargo test`。
- 不让过期的通过记录留下来：收尾那次测试给足 `timeout_seconds`，让它在前台跑完再动文件。
- 说清残留成本：回填文档仍然会让收尾验证再跑一遍测试，去向写在"遗留"里。

## 范围

- 做：改 `AGENTS.md` 三、5 的验证步骤和顺序；同步 `.cove/rules.md` 里两条与测试有关的规则；写清 `cargo test` 之后改文件的代价。
- 不做：改 `Evidence` 的复用判断（认不同写法、不同 profile，或把纯文档改动排除在外）。这要动 `src/safety/gates.rs` 的核心逻辑，留到有明确收益时另开文档。

## 调研与参考

- 选 debug 而不是让收尾验证也跑 release：debug 有增量编译，小改动重编更快；开着 `debug_assertions` 和溢出检查，能多发现问题。
- 让收尾验证跑 release 要配置 `done_verify_commands`，它会整体替代自动推断，影响所有项目，不做。

## 方案

只改规则，不改代码：

- `AGENTS.md` 三、5：顺序改为格式化 → `cargo clippy --release --all-targets` → 改界面的用 release 版实际跑一遍 → 最后 `cargo test`；写明小改动可以先只跑受影响的测试，全量留给收尾验证；写明收尾那次要给足 `timeout_seconds`（超过 120 秒会转后台，记录来得太晚），并写明 `cargo test` 之后再用 `write`/`edit` 或命令改文件（回填文档、跑 `target/release/cove`、格式化后拷回）会让记录作废，收尾验证会重跑测试；回填文档和跑 release 版不动源码，不用重编，格式化后拷回要重编。
- `.cove/rules.md`：把"跑测试只用 `cargo test`"限定为收尾那一次完整测试，说明针对性测试可以带过滤参数，但复用的只是整条命令。
- 历史优化文档里记录的 `cargo test --release` 是当时的验证记录，不改。

## 影响范围

- 只影响开发流程的三处文档，不改代码，不影响 cove 的用户行为。
- `.cove/rules.md` 不进仓库（`.gitignore` 忽略 `.cove/`），只影响本机这个项目的会话。
- 历史优化文档里的 `cargo test --release` 保持原样，不影响它们的结论。

## 验证

- 没有代码改动，不需要新增测试或构建。
- 复核代码路径：`src/safety/gates.rs` 的 `normalize_command`（`gates.rs:114`）、`Evidence::passed`（`gates.rs:202`）、`observe`（`gates.rs:274`，清掉记录的分支在 `gates.rs:289-323`、非只读工具在 `gates.rs:320`、只读工具表在 `gates.rs:178-191`），`src/safety/verify.rs` 的 `test_commands`（`verify.rs:166`），以及转后台的路径（`src/tools/mod.rs:1124`、`src/safety/gates.rs:374`）；确认了复用条件是"命令原文相同、之后没有再调非只读工具、也没有跑分类上可能改文件的命令"，以及后台结果来得晚、不检查这期间改没改文件。
- 人工确认：按新规则跑一次改代码的任务，`verify_ledger.jsonl` 里该任务不应出现第二条 `cargo test`（见"遗留"）。

## 实施计划

- [x] 改 `AGENTS.md` 三、5
- [x] 改 `.cove/rules.md` 两条测试规则
- [x] 在本文件记录证据和残留成本

## 待决策

无。

## 决策记录

- 2026-11-27：收尾验证统一用 debug 的 `cargo test`，不再用 `cargo test --release`。
- 2026-11-27：这一版不动 `Evidence` 的清空条件，接受"回填文档后再跑一遍测试（不重编）"的残留成本；把纯文档改动排除在外留到以后。
- 2026-11-27：后台命令的记录与"开始之后有没有改文件"脱钩这件事只写进规则（给足 `timeout_seconds`、等结果再改文件），不改 `observe`，因为改它要区分"这次调用开始后有没有改过文件"，属于新的状态，另开文档。

## 验证结果

- 实际跑过的命令：无。这次只改文档和规则，没有代码可测。
- 人工确认：按 `gates.rs`、`verify.rs` 的代码核对复用条件，未实跑。

## 遗留

- 下一个改 Rust 代码的任务里确认收尾验证真的复用了模型跑过的 `cargo test`：`verify_ledger.jsonl` 里那次任务不应出现第二条 `cargo test`，也不应再有一次全量编译。
- 把纯文档改动（`write`/`edit` 只碰 `docs/`、`README.md` 这类文件）排除在"作废"之外，回填验证结果后就不用再跑一遍测试。去向：以后另开文档改 `src/safety/gates.rs` 的 `observe`。
- 后台命令的结果要带上"命令开始之后有没有改过文件"的判断，否则转后台的 `cargo test` 在回填文档之后仍会被记成通过。去向：以后另开文档改 `observe` 和 `background_finished`。
