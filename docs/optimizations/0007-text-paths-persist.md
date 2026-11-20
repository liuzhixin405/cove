# 0007 统一底层工具：文本截断、路径前缀、文件锁与原子写入

- 状态：已完成
- 日期：2026-11-20（创建）；2026-11-20（完成）
- 关联：同类功能统一分析（第三组）；[0004](0004-unified-process-env-and-git.md)（`git::plain`）

## 背景

同类功能统一分析的第三组。"截断一段文字""去掉 Windows 路径前缀""加文件锁""原子写文件"这几件底层的事，各模块各写了一份，规则不一致，有几处已经是 bug。

## 现状与问题

**P1 截断：12 个函数，三种计量方式，五种后缀。**

| 位置 | 计量 | 后缀 | 用途 |
|---|---|---|---|
| `agents/subagent.rs` `clip` | 字节，先合并空白 | `…` | 发给模型 |
| `agents/background.rs` `clip_report` | 字节（固定 8K） | `\n[report truncated]` | 发给模型 |
| `app/diagnostics.rs` `clip` | 字节 | `…` | 诊断报告 |
| `process/git.rs` `clip` | 字节 | `…` | 错误信息 |
| `memory/recall.rs` `clip` | 字节（预留 16） | `\n[truncated]` | 发给模型 |
| `safety/gates/review.rs` `clip` | 字节 | `\n... [diff truncated; …]` | 发给模型 |
| `tools/text_encoding.rs` `clip_chars` | 字符 | `…[+N chars]` | 工具输出 |
| `extensions/skills.rs` `clip` | 字符（含 `…` 在内） | `…` | 终端列表 |
| `app/locks.rs` `clip` | 字符（`…` 另加） | `…` | **终端** |
| `app/render.rs` `clip` | 字符，只取第一行 | `…` | **终端** |
| `app/history.rs` `clip` | 显示列（含 `…`） | `…` | 终端 |
| `app/console.rs` `clip` | 显示列 | 无 | 终端布局 |

- **bug**：`render.rs`（工具调用摘要）和 `locks.rs`（文件占用提示）在终端上按字符数截断，中文一个字占两列，160 个汉字实际占 320 列，超出终端宽度就会折行。`skills.rs` 的列表也有同样的问题。
- 另有约 40 处内联的 `chars().take(n)`、`floor_char_boundary(n)`。

**P2 控制字符：4 个函数，替换规则不同。** `ui::visible` 换成 `U+FFFD`，保留换行和 Tab；`console::visible` 在此基础上还保留 ESC；`console::visible_text` 把换行和 Tab 变成空格；`history::visible` 把控制字符换成空格。于是同一段带 `\r` 的文字（Windows 换行），在历史阅读器里显示为空格，在主界面里却显示为 `�`。

**P3 去掉 `\\?\` 前缀：9 处。** `process/git.rs` 的 `plain` 只给盘符路径去前缀，UNC 路径（`\\?\UNC\server\share`）保留原样，这是对的；`session/summary.rs` 的 `display_path`、`app/mod.rs` 的欢迎行、`app/render.rs` 的 `scratch_location`、`session/mod.rs` 的 `file_holders` 一律去掉前缀，**UNC 路径会被显示成 `UNC\server\share`**，也查不到占用文件的进程。用于比较的规范化写法也有两份（`safety/permission/policy.rs` 的 `same_project`、`safety/protected.rs` 的 `key`），分隔符的处理还不一样。

**P4 文件锁：11 处**，各自写"打开锁文件、加锁、等待或报忙"：信任表、dream（2 处）、记忆存储（等 5 秒）、会话和工作区租约、会话笔记、建议、`/changes` 项目锁（异步等待）、自动化（2 处）、费用账本。

**P5 原子写入：写配置文件的有 3 份，几乎相同。**
- `config::write_json_file` 和 `app/setup.rs` 的配置写入完全一样（临时文件、Unix 下 0600、fsync、替换）；
- `commands/safety.rs`（`/permissions` 改权限模式时写 `config.json`）**没有 fsync，Unix 下也没设 0600**，而这个文件里可能有 API key；
- 会话笔记、输入历史重写也各自写了一遍"临时文件 + 替换"。

**P6 读改写不加锁。** `policies.json`（"总是允许"规则）和 `config.json`（`/model`、`/config`、`/permissions`、配置向导）都是"读出、修改、整体写回"，不加锁。两个 cove 窗口同时写，后写的会覆盖先写的，丢掉一条规则或设置。同类的信任表已经加了锁，这两个没有。

## 目标

1. 截断只有一套规则：给模型和文件的按字节，给终端的按显示列；后缀统一。
2. 控制字符只有一套规则。
3. 去 Windows 路径前缀只有一个实现，UNC 路径显示正确。
4. 文件锁、原子写入各只有一个实现；所有读改写的共享文件都加锁。

## 范围

- 做：
  - 新增 `src/text.rs`、`src/paths.rs`、`src/persist.rs`；
  - 上面列出的 12 个截断函数、4 个控制字符函数、9 处路径前缀、11 处锁、5 处"临时文件 + 替换"全部改用新模块；
  - 内联截断里"截断并加标记"这种写法的，一并迁移；
  - `policies.json`、`config.json` 的读改写加锁。
- 不做：
  - `engine/tool_output.rs` 的首尾保留截断：它按 token 估算分配首尾比例，是专门的算法，不是简单截断；
  - `session/workspace.rs` 的 `replace_file_bytes`：它是"内容没变才替换"的比较交换，还要保留原文件权限，语义不同；
  - 输出给模型的各处截断长度不变，只换实现。

## 调研与参考

- 依赖里已有 `fs2`（文件锁）、`tempfile`（临时文件）、`unicode-width`（显示宽度），不需要新依赖。`dunce` 这类库能解决 P3，但 `git::plain` 那十行已经正确，不值得为它加依赖。
- `~/.cove/errors.log` 里没有锁冲突或替换失败的记录：P6 是潜在问题，目前没有实际发生过的证据。

## 方案

**`src/text.rs`**

```rust
/// 不超过 `max` 字节的前缀，落在字符边界上。
pub fn head(text: &str, max: usize) -> &str;
/// 超过 `max` 字节时截断并加上 `marker`（标记不计入 `max`）。
pub fn clip(text: &str, max: usize, marker: &str) -> Cow<'_, str>;
/// 合并所有空白为单个空格。
pub fn one_line(text: &str) -> String;
/// 不超过 `cols` 个显示列；截断时以 `…` 结尾，`…` 计入 `cols`。
pub fn clip_width(text: &str, cols: usize) -> Cow<'_, str>;
/// 不超过 `cols` 个显示列，不加标记（布局用）。
pub fn fit_width(text: &str, cols: usize) -> &str;
pub fn width(text: &str) -> usize;
/// 终端安全：`\r\n` 变 `\n`，单独的 `\r` 去掉，其他控制字符（含 ESC）变 `U+FFFD`。
/// `Lines` 保留换行和 Tab；`Row` 把换行和 Tab 变成空格。
pub fn visible(text: &str, shape: Shape) -> String;
```

- 字节截断的后缀：发给模型的统一成 `\n[truncated]`，其中 review 的 diff 保留它原来那句提示，因为那句话告诉模型"去读文件看剩下的部分"；子代理摘要、诊断、git 错误这类单行文字用 `…`。
- `console::visible` 保留 ESC 的那种用法，调用方传入的是已经由 cove 自己生成的 ANSI 样式，不是外部文本，改为由调用方先对外部文本调用 `visible`，再拼样式，不再需要"保留 ESC"这个变体。
- `clip_chars` 的 `…[+N chars]` 是给模型看的"还剩多少"，保留为 `clip` 的一种 marker 用法，由调用方算好数量。

**`src/paths.rs`**

```rust
/// 去掉 `canonicalize` 加的 `\\?\`：盘符路径变成 `C:\…`；UNC 保持原样（git 需要）。
pub fn plain(path: &Path) -> PathBuf;
/// 给人看的：盘符同上；`\\?\UNC\server\share` 显示成 `\\server\share`。
pub fn display(path: &Path) -> String;
/// 用于比较：`display` 之后统一用 `/`、去掉末尾分隔符，Windows 上转小写。
pub fn key(path: &Path) -> String;
```

**`src/persist.rs`**（`session::atomic_json` 和 `replace_with` 从 `session/mod.rs` 移过来，`session` 里不保留转发）

```rust
pub struct Options { pub private: bool }  // private：Unix 下 0600
/// 临时文件、写入、fsync、替换（Windows 上能替换被别人打开着的文件）。
pub fn write_atomic(path: &Path, bytes: &[u8], options: Options) -> Result<()>;
pub fn atomic_json(path: &Path, value: &impl Serialize) -> Result<()>;  // 保留 32 MiB 上限

pub enum Wait { No, Up(Duration), Forever }
/// 打开（必要时创建）锁文件并加独占锁；`Wait::No` 时被占用立即返回 `Busy`。
pub fn lock(path: &Path, wait: Wait) -> std::result::Result<File, LockError>;
pub async fn lock_async(path: &Path, timeout: Duration) -> std::result::Result<File, LockError>;
/// 在 `<path>.lock` 下读出、修改、原子写回 JSON。
pub fn update_json<T>(path: &Path, options: Options, edit: impl FnOnce(&mut T) -> Result<()>) -> Result<T>;
```

- 各调用方保留自己的报错文字，例如"这个会话正被另一个 cove 窗口使用"，只把加锁的过程换掉。
- `policies.json` 的 `append`、配置的 `edit_file`、`/permissions`、配置向导、信任表都改用 `update_json`。

## 影响范围

- 新增：`src/text.rs`、`src/paths.rs`、`src/persist.rs`。
- 修改：P1–P6 列出的文件，以及使用 `session::atomic_json`、`session::replace_with`、`summary::display_path` 的地方。
- 行为变化：
  - 终端上的工具调用摘要、文件占用提示、技能列表按显示宽度截断，中文不再超宽；
  - 单独的 `\r` 不再显示成 `�`；历史阅读器里的控制字符显示为 `�`，不再是空格；
  - 发给模型的截断长度和标记都不变（实施时决定，见决策记录）；
  - UNC 路径显示为 `\\server\share`；
  - `/permissions` 写配置时加上 fsync 和 0600；
  - 两个窗口同时改 `policies.json` 或 `config.json` 不再互相覆盖；锁文件 `policies.json.lock`、`config.json.lock` 放在同一目录下。

## 验证

- 单元测试：
  - `text`：中文按两列计宽；字节截断落在字符边界上；`…` 计入列宽；`\r\n` 和单独 `\r` 的处理；
  - `paths`：盘符、UNC、普通路径三种情况的 `plain`、`display`、`key`；
  - `persist`：多个线程同时 `update_json` 往同一个文件里各加一项，最后一项都不少；`lock` 的三种等待方式；原有的"替换一个正被读取的文件"测试保留。
- 全量 `cargo test` 通过；只对改动过的文件，通过标准输入运行 `rustfmt`。

## 实施计划

- [x] `text`，迁移截断和控制字符
- [x] `paths`，迁移路径前缀
- [x] `persist`，迁移锁和原子写入；`policies.json`、`config.json` 加锁
- [x] 测试、格式化，回填验证结果

## 待决策

无。

## 决策记录

- 2026-11-20：按同类功能统一分析的建议顺序实施，这是第三组。（用户："按照建议做"，"继续"）
- 2026-11-20：调研中发现 `~/.cove/policies.json` 里有 588 条"精确命令允许"规则（534KB），几乎都是一次性的多行脚本。这是"记住规则"本身的问题，不属于本次范围，记入遗留，另开文档处理。（实施方）
- 2026-11-20：实施时对方案做了几处调整（实施方）：
  - `text`：原来按字符计数、发给模型的几处（技能和 MCP 工具描述、插件描述、网页提示等）如果改成按字节，中文能保留的内容会少三分之二。所以保留按字符计数，用 `clip_chars`（`…` 计入上限）；读文件工具的"`…[+N chars]`"用 `clip_counted`；"合并空白 + 字节截断 + `…`"的摘要用 `short`。`visible` 拆成 `visible`、`visible_row`、`visible_styled` 三个函数，不用枚举参数，调用处更短。
  - `console.rs` 的输出不重构，用 `visible_styled` 保留 ESC：它的输入是 cove 自己拼好样式的文字，改成"调用方先清理外部文本"要动十处绘制代码，风险大于收益。
  - 发给模型的截断后缀不统一改成 `\n[truncated]`，各处保留原来的标记：改标记只是换了个写法，却会让模型看到的内容和以前不一样，没有实际收益。
  - `persist`：锁不用 `Wait` 枚举，改成 `try_lock(path, 等待时长, 被占用时的报错)`（时长为 0 表示不等）、`try_lock_async`、`lock`（一直等）和 `lock_for(path)`（锁 `<path>.lock`）。读改写用 `with_lock(path, 闭包)`，不做 `update_json`：配置文件的读取规则不同（64KiB 上限、容忍 BOM），用闭包可以让各处保留自己的读取方式。
  - 费用账本锁的是数据文件本身（追加写入时加锁），不是单独的锁文件，不改。
  - `/model`、`/config` 修改配置时，配置锁在写完文件后立即释放，再应用新设置，避免应用设置时再去加同一把锁。
- 2026-11-20 收尾核对（[0014](0014-audit-fixes.md)）：终端的技能列表当时没有改成按显示宽度截断和对齐，已在 0014 修正；本文引入的几条 clippy 风格警告也已修掉。

## 验证结果

- **新模块**：`src/text.rs`（截断、宽度、控制字符）、`src/paths.rs`（`plain`、`display`、`key`）、`src/persist.rs`（`write_atomic`、`atomic_json`、`read_json`、`replace_with`、`file_holders`、`io_context`、锁）。`session/mod.rs` 里原来的这些函数直接移走，没有保留转发。
- **截断**：删除了 14 个函数。除了调研列出的 12 个，实施时又找到两处：`tools/text_encoding.rs` 的 `clip_bytes`（和 `text::clip` 完全一样），以及标题生成里的一个截断闭包。另外迁移了 3 处内联的"截断 + 标记"写法（hooks 输出、扩展输出、标题）。其余内联截断是不加标记的直接截断（`truncate(floor_char_boundary(n))`），本来就是一行，没有改。
- **终端显示**：工具调用摘要、文件占用提示、历史列表、输入框补全统一按显示宽度截断，中文不再超宽。
- **控制字符**：删除了 4 个函数，统一成 `text::visible` 等三个；`\r` 不再显示成 `�`。
- **路径**：9 处去前缀的代码全部改用 `paths`。`rg '\\\\\?\\' src` 只在 `src/paths.rs` 里有结果。UNC 路径显示为 `\\server\share`。
- **锁**：信任表、dream（2 处）、记忆存储、会话和工作区租约、会话笔记、建议、`/changes` 项目锁、自动化（3 处）改用 `persist`。
- **原子写入**：`config::write_json_file`、配置向导、`/permissions`、会话笔记、输入历史重写改用 `write_atomic`。`/permissions` 写配置时补上了 fsync 和 0600。
- **加锁的读改写**：`policies.json`（"总是允许"）、`config.json`（`/model`、`/config`、`/permissions`、配置向导）。
- **新增测试**：
  - `text` 4 个：字节截断落在字符边界上、按字符截断、中文按两列计宽、控制字符和 `\r`；
  - `paths` 2 个：盘符、UNC、普通路径，以及不同写法的比较；
  - `persist` 2 个：8 个线程同时用 `with_lock` 往同一个文件加一项，一项不少；锁被占用时按时返回"忙"，释放后可以再加锁。
- **迁移的测试**：原 `session` 里的"替换一个正被读取的文件"、"查出占用文件的进程"移到 `persist`；`git.rs` 的路径前缀测试并入 `paths`；`text_encoding` 的 `clip_chars` 测试并入 `text`。
- `cargo test`：单元测试 380 个全部通过（另有 4 个忽略）。集成测试 215 个，第一次全量运行全部通过；补上一处修正后再跑，`cli::all_hook_events_run_with_scrubbed_environment_and_session_end_once` 失败一次，单独连跑 8 次失败 1 次。失败原因是测试里的 PowerShell hook 用 `Add-Content` 追加 `hook-events.txt` 时报 "Stream was not readable"，即文件刚写完就被别的进程（多半是杀毒软件或索引服务）独占打开。这个测试以前就偶发失败，本次没有改动 hook 的执行逻辑。
- 只对改动过的文件，通过标准输入运行了 rustfmt；`cargo build --release` 完成。用真实配置运行 `cove -p "/status usage"`，输出正常。

## 遗留

- 偶发失败的 hook 测试：可以改成每个 hook 写各自的文件，或在追加失败时重试，让它不受杀毒软件扫描的影响。属于测试本身的问题，另行处理。
- ~~`policies.json` 的规则只增不减~~（已由 [0011](0011-cap-exact-command-rules.md) 处理）：588 条精确命令规则，每次"总是允许"都整体重写 534KB。需要另外决定精确命令规则要不要设上限，或者用"最近使用"淘汰旧规则。
