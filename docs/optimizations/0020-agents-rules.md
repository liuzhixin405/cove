# 0020 开发禁忌文档（AGENTS.md）

- 状态：已完成
- 日期：2026-11-20（创建）；2026-11-20（完成）
- 关联：[0004](0004-unified-process-env-and-git.md)、[0007](0007-text-paths-persist.md)、[0014](0014-audit-fixes.md)、[0019](0019-plain-paths-in-output.md)

## 背景

用户原话："禁止写 .display()，我觉得有必要维护一个文档，对于可能引起问题的或者多次踩坑出错的都彻底禁掉。"确认是指"给改代码的人和 AI 看的规则文档"。

## 现状与问题

- 仓库里没有任何开发规则文档。cove 会读根目录的 `CLAUDE.md`、`AGENTS.md`、`.cove.md`（`app/onboarding.rs`），Claude Code 读 `CLAUDE.md`，但这些文件一个都没有。
- 同类问题反复出现。0004、0007 已经把子进程、git、截断、控制字符、路径前缀、文件锁、原子写入收拢成统一入口；之后的 0014 审计里，仍有 B7（直接 spawn git）、B14（自己过滤会话）、B15（按字符数对齐）绕开了这些入口。0019 又漏出 15 处 `\\?\`。
- 规则只记在协作者的个人记忆里（禁止 `cargo fmt`、先写优化文档等），换一个人或工具就看不到。
- 0019 之后代码里还有 37 处 `.display()`，用于持久化的键、git 参数、相对路径和测试。只要它们还在，照抄旁边的代码就会再写出 `.display()`。

## 目标

- 一份所有改代码的人和 AI 都会自动读到的规则文档，每条都有出处；
- 清单里的禁令在现有代码里先落实：直接按字节切片的路径已全部改用 `text::head` / `text::tail`；`String::truncate` 配合已确认边界的原地截断保留，不留可以照抄的切片写法。

## 方案

- 根目录新建 `AGENTS.md`，`CLAUDE.md` 只写 `@AGENTS.md` 引用它。内容分四部分：
  - 禁止的写法：禁止什么、改用什么、出处；
  - 改动时容易漏的地方；
  - 流程；
  - 本机工具的坑。
- 剩下的 `.display()` 换成 `to_string_lossy()`。对合法 UTF-8 路径，两者结果逐字节相同，所以信任表、权限规则这类持久化的键不变。能给人看的那一处（regression 的待恢复文件列表）改用 `paths::display`。
- `session/workspace.rs` 里 `Image::display`（取 diff 文本，和路径无关）改名为 `diff_text`，免得和禁令混淆。

## 范围

不做：用测试或 clippy 机器拦截。用户选的是规则文档；clippy 不在 CI 里跑，`cargo test` 扫源码可以做，但没有被要求。

## 影响范围

- `AGENTS.md`、`CLAUDE.md`（新增）；
- 20 个源文件里的 `.display()`，`session/workspace.rs` 的方法改名；
- `agents/regression.rs` 删了一个不再使用的 `Path` 导入。

## 验证

- `grep -rn '\.display()' src tests` 只剩 `src/paths.rs`；
- 全量 `cargo test --release`；clippy 不新增警告。

## 实施计划

- [x] 替换剩余的 `.display()`
- [x] 写 AGENTS.md、CLAUDE.md
- [x] 测试、clippy

## 待决策

无。

## 决策记录

- 2026-11-20：做成给改代码的人和 AI 看的规则文档，而不是 cove 运行时的拦截。（用户）
- 2026-11-20：放在根目录的 `AGENTS.md`，cove、Codex 等会直接读到；`CLAUDE.md` 用 `@AGENTS.md` 引用，只维护一份。（实施方）
- 2026-11-20：只收有出处的条目，不把一般性的编码建议写进去。（实施方）

## 验证结果

- `.display()` 在 `src/`、`tests/` 中为 0 处（`src/paths.rs` 内部只用 `to_string_lossy`）。
- 全量 `cargo test --release` 通过（393 + 216）；clippy 仍是原有的 5 条。

## 遗留

- 格式化时误把没改过的 `src/safety/permission/risk.rs` 也跑了一遍 rustfmt：文件列表是用 `grep -l` 搜出来的，带上了它。rustfmt 只改排版，测试通过；旧排版可以用 `git show HEAD:src/safety/permission/risk.rs` 找回。这个教训已写进 AGENTS.md 的"流程"第 2、3 条。
- 禁令目前只靠文档和评审执行，没有机器检查。
