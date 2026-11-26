# 0055 引用核对误报否定句；表格单元格在单词中间折行

- 状态：已完成
- 日期：2026-11-26（创建）；2026-11-26（完成）
- 关联：0027（引用核对）

## 背景

一次只读代码审查（167 次模型调用，回答约 3900 字，含一张 6 行表格）结束时出现两处异常：

1. 最终回答给出后，引用核对认为一处引用不对，要求模型重答；模型补查一次后把整份结论（表格和三节内容）又输出了一遍，屏幕上同一份结论出现两次。被判错的那处引用其实是对的。
2. 表格第三列在英文单词中间断行：`positio` / `nal`、`ObjectDisposedExce` / `ption`、`AdminApi.cs:36-41` / `）`。

## 现状与问题

**P1 否定句里的引用被当成"声称这里有这个符号"。** 被判错的原文是：

> `AdminApi.cs:36-41` 也没开 `RespectNullableAnnotations`。

这句话说的是那几行**没有**这个设置，`src/` 里也确实搜不到它。核对时，[cite.rs](../../src/safety/gates/cite.rs) 的 `analyze` 把引用所在分句里最近的两个反引号词当锚点，到引用行附近找；找不到就判 `Mismatch`，`needs_fix()` 为真，触发重答。同一个函数里，"符号在仓库里不存在"这一支在 `NEGATIONS` 命中时会跳过，"引用行附近找锚点"这一支没有这个判断。另外，`NEGATIONS` 的匹配对象包括反引号里的代码，`NoneType` 这类名字会命中 `none`。

**P2 表格单元格按字素硬切。** [render.rs](../../src/app/render.rs) 的 `wrap_cell` 遇到超宽就在当前字素处断行，不找空格。英文、标识符、路径都会被切开。`wrap_cell` 还用于历史记录的折行（`app/history.rs`）。

## 目标

- 引用所在分句含否定词时，只核对文件存在和行号范围，不再拿分句里的符号当锚点；肯定句的核对不变。
- 表格单元格和历史记录优先在空格、CJK 字符前后断行；单个词比列宽还长时才硬切。

## 范围

- 做：P1、P2。
- 不做：
  - 后台代理报告在回合结束后被原样再打印一次（报告已交给模型并被转述）。要决定给用户显示什么、在哪里看完整报告，另开文档。
  - 表格列宽分配（现在是各列均分剩余宽度）。没有实际的错位，只是不够紧凑。
  - 行首禁则（`）`、`，` 不出现在行首）。

## 调研与参考

- `missing_symbols` 已经用同一份 `NEGATIONS` 跳过否定句（`symbols_missing_from_the_repository` 测试覆盖），P1 复用这份列表，并补上中文常见的"未""不在"。
- 否定分句里不拿锚点，只会少查，不会多报，代价是否定句里的错误行号仍能查出（行号越界、文件不存在照旧报）。

## 方案

**P1**：新增 `negated(prose, range, spans)`：取 `range` 内去掉反引号片段后的文字，小写后判断是否含 `NEGATIONS` 中的任一项。`analyze` 里：
- 引用：分句 `negated` 时锚点为空，`locate` 只查行号范围。
- 缺失符号：改用 `negated` 判断句子（去掉代码片段后再匹配）。

`NEGATIONS` 加 "未"、"不在"。

**P2**：`wrap_cell` 记录最近一个断点（空格之后；宽字符前后）在 `line` 中的字节位置、当时的宽度和当时生效的样式。超宽时，如果断点在行内且不在行首，就在断点处切开：前半段推入结果（样式未结束时补 `RESET`），后半段以断点处的样式开头接到下一行；没有断点时照旧硬切。

## 影响范围

| 文件 | 改动 |
|---|---|
| `src/safety/gates/cite.rs` | `negated`、`NEGATIONS`、`analyze`；测试 |
| `src/app/render.rs` | `wrap_cell`；测试 |
| `docs/optimizations/README.md` | 索引 |

## 验证

- 新测试：否定句里的引用不 `needs_fix`；同一引用写成肯定句仍判 `Mismatch`。先在旧代码上确认失败。
- 新测试：表格单元格 `positional record` 在列宽不足时在空格处换行；超长标识符仍硬切；样式跨行保留。
- `cargo test --release` 全量通过；`cargo clippy --release --all-targets` 不新增警告。

## 验证结果

- 新测试 `negated_references_are_not_checked_for_their_symbols`、`cells_wrap_between_words` 在旧代码上失败（表格一项输出 `["is a positio", "nal record"]`，与现象一致），改后通过。
- `COVE_REPLAY=~/.cove/sessions cargo test --release --lib -- --ignored replay_sessions`：改动前后都是 4 次重答提示，内容相同；没有放过原来能查出的问题。触发本文的那一轮不在回放里，因为会话保存的是改过后的回答。
- `cargo test --release`：lib 607 通过（7 忽略），集成 269 通过。
- `cargo clippy --release --all-targets`：仍是原有 5 条，都不在本次改过的文件里。

## 遗留

- 后台代理报告重复打印：0056 已处理。
- 回放里有一次提示是 `PublishingLog.cs:111 — `:117` is not in that file`：裸行号 `:117` 被当成了锚点，是另一种误报：0057 已处理。
