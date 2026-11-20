# 0014 收尾核对：修正 0001–0013 的改漏和改错

- 状态：已完成
- 日期：2026-11-20（创建）；2026-11-20（完成）
- 关联：[0001](0001-unified-session-history.md)–[0013](0013-cache-ttl-and-precompaction.md)

## 背景

用户要求："对着 docs 中的文档做一个收尾，看看是不是都完成了，有没有改漏，或者改错代码。"

核对的做法：
- 3 个独立的子代理按文档分组（0001–0004、0005–0009、0010–0013）逐条对照代码；
- 另做了机械检查：索引和各文档的状态、已删功能的残留、clippy、没人调用的公共函数。

13 份文档的状态和索引一致，都是"已完成"。以下是核实后确认的问题；所有条目都已对照代码确认，子代理报告里有误或不需要处理的已剔除。

## 现状与问题

### 代码错误

| # | 问题 | 位置 | 出自 |
|---|---|---|---|
| B1 | 竞速方案超时后，用 `timeout` 直接丢弃了 `hub.run` 的执行。花费没记入会话，活动状态一直停在"运行中"；`lane_cancel.cancel()` 在丢弃之后才调用，不起作用 | `workflows/race.rs` | 0010 |
| B2 | `add_spend` 没有累加 `cache_write_1h`。子代理、竞速、后台调用的 1 小时写入不计入会话总量 | `engine/agent.rs` | 0013 |
| B3 | 精确命令规则的上限把所有带 `command_exact` 的规则都当作可淘汰，包括用户手写的 deny/ask 规则 | `safety/permission/policy.rs` | 0011 |
| B4 | memory 的 id 在两个范围都存在时，文档说要报错，代码却默默用了 project，删除时可能删错 | `tools/extension_tools.rs` | 0012 |
| B5 | 子代理的 `Agent` 用完即丢，它启动的后台预压缩不会被取消，会继续花钱且不计入会话 | `engine/agent.rs` | 0013 |
| B6 | 自审和 dream 也走主循环，按 1 小时缓存的 2 倍价写入；文档只给主任务和子代理用 | `safety/gates/review.rs`、`memory/dream.rs` | 0013 |
| B7 | `Git::query_blocking` 直接 spawn git：没有去掉受保护的环境变量（API key 会传给 git），输出超过上限时静默截断 | `process/git.rs` | 0004 |
| B8 | 压缩保留部分的上限按原始估算算，触发阈值却按校准后的值判断；校准比例大于 2 时，压缩后下一轮可能马上又压缩 | `engine/agent.rs` | 0012 |
| B9 | shell 提示判断临时路径时用 `contains`，`src/tmpl/x`、`web/dev/x` 这类工作区路径被当成临时目录，不提示 | `safety/gates/guard.rs` | 0012 |
| B10 | `/undo` 的帮助行还是旧的 `[list|…|confirm [token]]`。这一行同时用于 `/help` 和补全，所以补全会给出已删除的 `/undo list` | `commands/sessions.rs` | 0009 |
| B11 | 快照的报错还在提示用户运行已删除的 `/undo list`（3 处） | `session/snapshot.rs` | 0009 |
| B12 | AI 标题生成完成后，总是按"当前会话"更新窗口标题；生成期间 `/resume` 换了会话或 `/rename` 改了名，窗口会显示错 | `session/titles.rs`、`app/console.rs` | 0002 |
| B13 | `/resume detail`、Markdown 导出、回放 HTML 还在用 UTC；0001 要求统一为本地时间，`reference.md` 也写着"时间统一为本地时间" | `commands/sessions.rs` | 0001 |
| B14 | dream 挑选最近会话时绕开了统一入口，自己过滤，还会把空会话算进去 | `memory/dream.rs` | 0001 |
| B15 | 终端的技能列表仍按字符数截断、对齐，中文会超宽；0007 说已改为按显示宽度 | `commands/extensions.rs` | 0007 |
| B16 | 竞速结果总是提示"应用方案 A"，即使只有 B 通过了验证 | `workflows/race.rs` | 0010 |

### 死代码和残留

- `Skills::scope_key` 没有调用方（0003 删掉的自动技能用的）；
- 技能列表里判断 `verification == "legacy-unverified"` 的分支永远不会成立（同上）；
- `oneshot::Background` 的 `last()` 除了测试没人用；
- `persist::file_holders` 只在模块内使用，却是公共函数；
- `safety/verify.rs` 里有一个空测试 `tail_keeps_char_boundaries`；
- 注释残留：
  - `workflows/changes.rs` 开头残缺、`tail` 上方重复；
  - `session/mod.rs` 有一行孤立注释；
  - `session/usage.rs` 留着已删除的 `bill()` 的注释；
  - `app/mod.rs` 对后台调用的说明过时；
  - `commands/sessions.rs`、`session/snapshot.rs` 里提到 `/undo list` 和确认码；
- 0007、0008、0013 引入的 13 条 clippy 风格警告。

### 文档和代码对不上

- `docs/reference.md`：
  - 还写着已删除的"技能复盘"和"后台调用按主模型单价估算"（0003、0005）；
  - `[summary]` 行"供 worker 解析"（0010 已删除 worker）；
  - "不支持鼠标操作"（0001 的阅读器已支持鼠标）；
  - 没写剪贴板清理、闲置项目快照过期、打开时回收 cruft pack，快照说明漏了 `draw_image`（0009）。
- `docs/design.md`：还写着"技能复盘"（0003）。
- 各优化文档里与实际实现不一致的描述（见方案第 3 项）。

## 方案

**1. 修代码（B1–B16）。**
- B1：超时后先取消方案的运行，再等它收尾；这样花费照常记入会话，活动状态正常结束，结果记为 `timeout`。
- B2：`add_spend` 累加 `cache_write_1h`。
- B3：只淘汰 cove 自动生成的 `allow-shell-exact-*` 规则。
- B4：两个范围都有这个 id 时报错，要求写明 `scope`。
- B5：`Agent` 被丢弃时取消它的预压缩。
- B6：`Agent` 增加 `with_long_cache(false)`，自审和 dream 使用 5 分钟缓存。
- B7：去掉受保护的环境变量；输出超过上限时返回失败，不截断。
- B8：保留上限按校准比例换算成原始估算。
- B9：只认路径开头是临时目录（`/tmp`、`$TMP`、`$TEMP`、`/dev/`），或路径里含 `AppData/Local/Temp`。
- B10、B11：帮助行和报错改为新命令。
- B12：控制台记住窗口标题属于哪个会话，以及是不是用户改的名；AI 标题只在会话一致、且没有被用户改名时更新窗口。
- B13：改用本地时间。
- B14：改用 `Store::sessions`（统一入口，默认排除空会话）。
- B15：按显示宽度截断和对齐。
- B16：提示第一个可以应用的方案；都不能应用时说明原因。

**2. 删除死代码和残留**（上一节列出的全部）。

**3. 更正文档**：
- `reference.md`、`design.md` 按上一节更正。
- 各优化文档在决策记录里补一条"2026-11-20 收尾核对"，写明实际做法：
  - 0001：`Origin::Injected` 没有实现，注入内容作为同一条用户消息里的后续块；标题状态在 `/config memory stats` 里，不在 `/status`；
  - 0004：worktree 的超时是 120 秒；函数名是 `protect_key_source`；`/diff git`、`/commit` 只有超时，不能中途取消；计划中的两个测试没有写；
  - 0005：记忆提取保留了自己的后台任务管理，因为它要求同一时间只跑一个提取，而 `Background` 是"每个键只跑一次"；
  - 0008：前缀有歧义时报错的测试没有写；
  - 0009：清理仍保留 10 个 / 3 天的缓冲，以免每次拍快照都重写；`/undo` 列出 15 个；剪贴板在粘贴图片时清理，不是启动时；
  - 0010：方案使用 `general` 类型；查看补丁用 `/changes diff`；
  - 0013："摘要依据的消息变了就回退"由单元测试覆盖，没有集成测试。

## 影响范围

上面列出的文件，以及 `docs/reference.md`、`docs/design.md` 和 0001–0013 的文档。

## 验证

- 新增单元测试：
  - B3：用户手写的带 `command_exact` 的 deny 规则不会被淘汰；
  - B4：同一 id 在两个范围都存在时报错；
  - B7：超过上限时返回失败；
  - B9：`src/tmpl/x.html` 会提示；
  - B12：别的会话的标题不会改动窗口，用户改名后 AI 标题不覆盖；
  - B16：只有 B 能应用时提示 B。
- 全量 `cargo test`、`cargo clippy`。

## 实施计划

- [x] B1–B16
- [x] 死代码和残留
- [x] 文档更正
- [x] 测试、格式化，回填验证结果

## 待决策

无。

## 决策记录

- 2026-11-20：收尾核对，修正改漏和改错。（用户）
- 2026-11-20：记忆提取不迁移到 `Background`，只更正 0005 的描述，理由见方案 3。（实施方）
- 2026-11-20：clippy 剩下的 5 条警告（`console.rs` 两处、`process.rs`、`policy.rs`、`tests/all/cli.rs`）在 0001 之前的原有代码里，与这些优化无关，不在本次处理。（实施方）
- 2026-11-20：子代理报告里以下几条核对后不处理（实施方）：
  - `Titler` 用的是只看当前会话的轮数，压缩后和列表里拼接的轮数不同。这只影响何时重新生成标题，不影响显示；
  - git 错误信息有几处仍是英文；
  - rg 子进程没有进程树管理（0004 已记入遗留）；
  - 预压缩在 Ctrl+C 时被放弃、它已完成的费用不计入。属于少见情况，已在 0013 的遗留里说明。

## 验证结果

- **B1–B16 全部修正**。
- **新增测试**：
  - 用户手写的带 `command_exact` 的 deny 规则不会被淘汰；
  - 同一 id 在两个范围都存在时，memory 报错；
  - `query_blocking` 输出超过上限时返回失败；
  - `src/tmpl/page.html`、`web/dev/app.js` 会得到写文件提示；
  - 别的会话的 AI 标题不改窗口，用户改过名后也不改；
  - 只有 B 能应用时提示 B，都不能应用时说明原因；
  - `Background` 限时等待（替换原来依赖 `last()` 的测试）。
- **删除**：`Skills::scope_key`、`legacy-unverified` 分支、`Background::last()` 和它保存的结果、`verify.rs` 的空测试；`persist::file_holders` 改为私有。新增 `usage::Pricing` 类型别名，代替重复的闭包类型。
- **clippy**：从 20 条降到 5 条，剩下的都在原有代码里（见决策记录）。
- **文档**：
  - `reference.md`：删掉"技能复盘""供 worker 解析""不支持鼠标"；补上剪贴板清理、闲置项目快照过期、cruft pack 回收、`draw_image` 和 `/init apply` 的快照；
  - `design.md`：删掉"技能复盘"那一条；
  - 0001–0005、0007–0013 的决策记录各补一条"收尾核对"。
- `cargo test` 全部通过：单元测试 392 个（另有 4 个忽略），集成测试 216 个。
- 只对改动过的 25 个文件，通过标准输入运行了 rustfmt；`cargo build --release` 完成。

## 遗留

- 各文档里原有的遗留仍然有效，主要是：
  - 用新数据验证 0012、0013 的效果；
  - 偶发失败的 hook 测试；
  - 真实模型跑一次 `/race`；
  - QuoteHub 记忆交给 dream 整理；
  - 两份"会不会改东西"的判断（0009）。
