# 0033 trace、会话和验收报告记下是哪一次构建的 cove 产生的

- 状态：已完成
- 日期：2026-11-24（创建）；2026-11-24（完成）
- 关联：README 规则 3"证据要对得上当前版本"；0026、0027、0032（历史数据被误读）

## 背景

优化文档引用的本机数据（会话、trace、账本、验收报告）是过去不同版本的 cove 留下的。0026、0027、0032 都出现过同一种情况：数据里的问题其实在更早的某次改动里已经修复，或者字段含义已经变了，但记录本身看不出来自哪个版本，只能对照文件时间和优化文档的完成日期去推断（见下文问题 5）。
README 规则 3 要求引用数据前确认问题在当前版本上还存在，但这只能靠人对照日期；数据本身记下"是哪一次构建产生的"，才能直接筛出"修复之后"的数据。

## 现状与问题

1. **版本号一直没变。** `Cargo.toml` 是 `0.7.0`。0001 到 0032 这 30 多次改动都没有改它，所以 `env!("CARGO_PKG_VERSION")` 区分不了任何一次修复前后。
2. **trace 每行只有时间和类型。** `src/app/diagnostics.rs:65` 的 `JsonLog::write` 只写 `t`、`kind` 和各自的字段，没有构建信息（`errors.log` 也用它写）。
3. **会话和验收报告也没有。** 会话 JSON 的 `version` 字段是存储格式的版本（`src/session/mod.rs:163`，固定为 1）；验收报告（`src/safety/gates/acceptance.rs:36`，`Report` 结构在第 35 行）的 `version` 也是格式版本。
4. **这个目录不是 git 仓库**，构建时拿不到提交哈希。
5. 被历史数据误导的实例：
   - 0026：0005 之前的账本把缓存按全价算，费用被高估约 4.4 倍；
   - 0032：trace 的 `model_calls` 是会话累计值，"单任务 586 次"就是这么来的；
   - 0027：回放时有 3 次核实失败，判断不了当时对不对。
   这三次都只能靠人对照日期才发现。
6. **版本号显示的地方。** `cove --version` 来自 clap 的 `#[command(version)]`（`src/config/mod.rs:28`），即 `CARGO_PKG_VERSION`；启动横幅（`src/app/mod.rs:517`、`:526`）；诊断报告的"版本"一行（`src/app/diagnostics.rs:804`–`805`）；另有 MCP、`web_fetch` 的 User-Agent 和 MCP `clientInfo`。`/status`（`src/app/mod.rs:1410`）不显示版本。

以上于 2026-11-24 开工前对照当前代码核对过：版本号、行号与草稿一致，只有验收报告的 `version` 在第 36 行（草稿写的 34），诊断报告的版本行在 804–805 行（草稿写的 803）。

## 目标

- 每条 trace 记录、每份验收报告、每个会话里，都能看出它是哪一次构建的 cove 产生的。
- 分析脚本可以直接按构建筛选，比如"只看 0032 之后的数据"。
- 不需要 git，不增加明显的启动开销。

## 范围

- 做：构建标识（build.rs 生成）；trace 每行加 `build`；验收报告加 `build`；会话记录用过它的构建列表；`/status`、`--version`、诊断报告显示构建标识；文档。
- 不做：
  - 不自动改 `Cargo.toml` 的版本号：发版节奏是另一件事。
  - 不回填历史数据：旧数据没有这个字段，按缺失处理，分析时算作"未知构建"。

## 调研与参考

- 常见做法：`git describe` 或提交哈希加构建时间（vergen 等 crate）。这里没有 git，所以用"源码内容哈希"代替提交哈希：源码不变，哈希就不变；改了任何一个源文件，哈希就变。
- 构建时间单独不可靠：同一份源码重新编译，时间会变，但行为不变。
- 对比"最后一个完成的优化文档编号"：语义最直观，但要求每次都手动维护，容易漏，可以作为附加信息。

| 方案 | 能区分修复前后 | 维护成本 |
|---|---|---|
| 只用 `CARGO_PKG_VERSION` | 不能（一直是 0.7.0） | 无 |
| 构建时间 | 能，但同一份源码会算成不同版本 | 无 |
| **源码哈希（build.rs 计算）+ 构建时间** | 能，而且同一份源码哈希一样 | 无 |
| 最后一个完成的优化编号 | 能，语义直观 | 需要人维护 |

## 方案

1. **build.rs**：对 `src/**/*.rs`、`Cargo.toml`、`Cargo.lock`、`assets/`、`marketplace/` 按路径排序后计算 sha256，取前 12 位，作为 `COVE_SOURCE`；构建时间（UTC）作为 `COVE_BUILT`。用 `cargo:rerun-if-changed` 声明这些路径。
2. **常量**：在 `src/lib.rs` 或新的小模块里定义 `BUILD = "<版本>+<源码哈希>"`，例如 `0.7.0+3fa9c1d2e4b7`。
3. **trace**：`Diagnostics::write` 每行加 `"build"`（约 25 字节）。
4. **验收报告**：`Report` 加可选字段 `build`。
5. **会话**：加 `builds: Vec<String>`，记录参与过这个会话的构建，按首次出现的顺序，去重，最多保留 20 个。一个会话可能跨越多次构建，记一个字段不够。
6. **显示**：`cove --version` 和 `/status` 显示 `0.7.0+3fa9c1d2e4b7（构建于 2026-11-24 10:00 本地时间）`；诊断报告（`diagnostics.rs:804`）的"版本"一行也一样。
7. **评测**：`evals/run.py` 的结果 JSON 记录被测 cove 的 `--version` 输出，`--baseline` 对比时显示两边的构建。
8. **分析约定**：写进 README 规则 3。以后引用本机数据时，按 `build` 字段说明数据来自哪些构建；没有 `build` 的旧数据，一律视为 0033 之前的版本。

## 影响范围

- 新增 build.rs，改动 diagnostics.rs、acceptance.rs、session（字段）、app（版本显示）、evals/run.py、docs/reference.md、README 规则 3。
- 数据格式：trace、验收报告、会话各多一个可选字段，旧数据照常读取。
- 编译：每次改源码，build.rs 都会重新运行，计算哈希大约几十毫秒。

## 验证

- 单元测试：
  - `BUILD` 的格式是 `版本+12 位十六进制`；
  - 会话的 `builds` 去重、最多 20 个；
  - 读取没有 `build`/`builds` 字段的旧 trace、报告、会话不报错。
- 集成测试：`-p` 跑一个任务后，trace 每行、验收报告、会话里都有当前的 `build`。
- 手动：改一行源码重新构建，`--version` 的哈希改变；不改源码只 `touch` 文件，哈希不变。

## 实施计划

- [x] 阶段 1：build.rs 和 `BUILD` 常量
- [x] 阶段 2：trace、验收报告、会话
- [x] 阶段 3：版本显示、评测记录、文档

## 待决策

无：三项都已决定，见"决策记录"。

## 决策记录

- 2026-11-24：排入计划，排在 0031 之后、`auto` 删除问题之前。
- 2026-11-24：哈希范围取 `src/`、`assets/`、`marketplace/` 下的全部文件和 `Cargo.toml`、`Cargo.lock`，不含 `docs/`、`tests/`。理由：只有这些文件决定二进制的运行时行为（内置技能编译进二进制）；改文档、改测试不该让数据看起来来自"另一个版本"。`src/` 取全部文件而不只是 `*.rs`，因为 `include_str!` 之类引入的非 Rust 文件同样影响行为（目前 `src/` 下没有这类文件）。
- 2026-11-24：trace 每行都写 `build`。理由：每行只多约 30 字节，分析时不用回溯到任务开始的那一行去关联；同一个 `JsonLog` 写的 `errors.log` 也一并带上，成本相同。
- 2026-11-24：不附带"最后一个完成的优化编号"。理由：要人手动维护，容易漏改、改错；需要时用 `--version` 里的构建时间对照优化文档的完成日期即可。

## 实现说明

和"方案"不同或方案没写到的地方：

- 常量放在新模块 `src/build_id.rs`（不叫 `build.rs`，免得和根目录的构建脚本混淆）：`BUILD`（`版本+源码哈希`）和 `label()`（带本地构建时间的显示文字）。`COVE_BUILT` 存 Unix 秒（UTC），显示时转成 `chrono::Local`。
- build.rs 用 sha2 计算哈希，已在 `Cargo.toml` 加 `[build-dependencies] sha2 = "0.10"`（和普通依赖同一版本，没有新增 crate，`Cargo.lock` 不变）。每个文件按"相对路径（`/` 分隔）、0 字节、长度、内容"依次喂给哈希，路径排序后计算，和平台的路径分隔符、目录遍历顺序无关。
- `rerun-if-changed` 声明的是目录，cargo 按修改时间判断；只更新修改时间时 build.rs 会重跑、crate 会重新编译，构建时间变了，但哈希不变（见手动验证）。
- 会话的 `builds` 在 `Agent::run` 里记录：新任务和 `/continue` 都算，只是打开、列出会话不算。满 20 个时丢掉最早的，保留最近的构建。`Head`（日志记录的会话头）同步加了这个字段，日志记录和快照都带它。
- `/status` 在最后一行加"构建 …"；启动横幅仍只显示 `0.7.0`（一行里信息已经很多，需要时看 `/status`）；User-Agent 和 MCP `clientInfo` 保持 `CARGO_PKG_VERSION`，不对外暴露源码哈希。
- 验收报告的文字摘要（`/status acceptance`）不显示构建，`build` 只在 JSON 里。
- README.md 没有提到版本显示、trace 或报告格式，没有改。

## 验证结果

- `cargo test --release`：全部通过（单元测试 500 个、集成测试 233 个，6 个按原样 ignored）。中间一轮全量运行里 `integrations::cancelled_and_timed_out_mcp_operations_kill_owned_descendants` 失败一次（tests/all/integrations.rs:992，Python MCP 测试服务器在 2 秒超时内没有回应第一次 `echo`），这个测试不经过本次改动的代码；单独重跑 3 次都通过，随后再跑全量也全部通过。新增：`build_id::tests::build_is_version_plus_twelve_hex_digits`、`label_shows_local_build_time`（格式为 `版本+12 位小写十六进制`；构建时间按本地时间显示）；`session::tests::builds_are_unique_capped_and_absent_in_old_sessions`（去重、最多 20 个、日志记录和快照都保存、没有该字段的旧会话照常读取、空列表不写出）；`app::diagnostics::tests::every_line_names_its_build_and_old_lines_still_read`；`acceptance::tests::report_is_saved_and_summarized` 增加 `build` 和"旧报告没有 `build` 也能读"的断言；集成测试 `project::trace_acceptance_and_session_name_the_build`（`-p` 跑一个读文件的任务后，trace 每一行的 `build`、验收报告的 `build`、会话的 `builds` 都等于当前 `BUILD`，`--version` 以 `cove <BUILD>（构建于 ` 开头）。
- `cargo clippy --release --all-targets`：仍是原有的 5 条警告（console.rs:2166、console.rs:758、process.rs:138、policy.rs:600、tests/all/cli.rs:209），没有新增。
- `cargo build --release` 后 `target/release/cove.exe --version`：

  ```
  cove 0.7.0+e12abbb516b8（构建于 2026-11-24 15:51 本地时间）
  ```

- 手动验证（`src/lib.rs`）：当前源码 `e12abbb516b8` → 末尾加一行注释重新构建 `425059d99b3a` → 改回原文件重新构建 `e12abbb516b8` → 只用 `os.utime` 更新修改时间重新构建，crate 重新编译、构建时间变为 15:51，哈希仍是 `e12abbb516b8`。
- `python -X utf8 evals/run.py --self-test`：全部通过。
- 评测（不影响模型行为，只跑 3 个用例）：`python -X utf8 evals/run.py --cases fix-failing-test,explain-bid-only,config-override --repeat 1 --jobs 3 --keep`，3/3 通过，费用 $0.0184。开跑时打印"被测 cove：cove 0.7.0+e12abbb516b8（构建于 2026-11-24 15:51 本地时间）"，结果 JSON（`evals/results/20261124-155304-deepseek.json`）的 `cove_version` 是同一行。逐个检查保留的独立数据目录：`trace.jsonl` 分别 21、13、16 行，每行的 `build` 都是 `0.7.0+e12abbb516b8`；验收报告的 `build`、会话的 `builds`（`["0.7.0+e12abbb516b8"]`）也一样。拿这份结果对 0033 之前的一份结果调用 `report()`，末尾显示"构建：基准 未记录（0033 之前的结果） → 本次 cove 0.7.0+e12abbb516b8（…）"；两边源码哈希相同、构建时间不同时标"（同一份源码）"。

## 遗留

- 0033 之前的 trace、会话、验收报告和评测结果没有 `build`，按方案不回填，分析时算作"0033 之前的版本"。
- 用量账本（`usage/*.jsonl`）和 `verify_ledger.jsonl` 没有加 `build`，不在本次范围；按会话 id 可以从会话的 `builds` 关联到构建。需要时另开文档。
