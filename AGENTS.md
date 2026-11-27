# 改 cove 代码前必读

给改这个仓库的人和 AI 看。下面每一条都踩过坑，出处写在后面。**不确定时按这里的做，不要照抄旁边的旧代码。**

## 一、禁止的写法

| 禁止 | 改用 | 为什么 / 出处 |
|---|---|---|
| `Path::display()`、`PathBuf::display()` | 给人或模型看：`paths::display(&p)`；比较：`paths::key(&p)`；交给 git 或外部程序：`paths::plain(&p)`；必须保持原样的字符串（持久化的键、测试里拼命令）：`p.to_string_lossy()` | 工作区和数据目录都经过 `canonicalize`，Windows 上是 `\\?\D:\…`，直接 `.display()` 会把前缀带进输出。一次漏了 15 处（0007 P3、0019） |
| 拿 `Path::starts_with`、`==` 比较一个 canonicalize 过的路径和一个没 canonicalize 的路径 | 两边都 canonicalize，或都转成 `paths::key` 再比 | Windows 上 `\\?\D:` 和 `D:` 是不同的前缀分量，比较永远不成立（0019） |
| 用 `contains` 判断路径在不在某个目录下 | 按路径分量判断：`starts_with`，或 `paths::key` 后按 `/` 边界比较 | `src/tmpl/x` 被当成临时目录 `tmp/`（0014 B9） |
| 直接按字节切字符串：`&s[..n]`、`&s[n..]`（可能切在汉字中间） | 前缀用 `text::head`，后缀用 `text::tail`，带标记用 `text::clip` / `text::clip_chars` / `text::short`；`String` 原地截断用 `s.truncate(text::head(&s, n).len())` | 中文落在字节中间会 panic；同一件事曾有 12 个函数、5 种后缀（0007 P1） |
| 终端上按字符数截断或对齐：`chars().count()`、`chars().take(n)`、`{:<20}` | `text::width`、`text::clip_width`、`text::fit_width` | 一个汉字占两列，按字符数算会超宽折行（0007 P1、0014 B15） |
| 把外部文本（模型输出、文件内容、命令输出）直接打到终端 | 先过 `text::visible` / `text::visible_row`；自己生成的 ANSI 样式在这之后再拼 | 控制字符会弄乱终端；以前 4 个函数规则各不相同（0007 P2） |
| 直接 `std::process::Command` / `tokio::process::Command` 启动子进程 | 普通程序用 `process::argv::Argv` + `run_argv`，git 用 `process::git::Git`。现有例外只有：打开浏览器和编辑器、重启自身、shell 工具本身、ripgrep、`/doctor` 探测程序版本；测试代码不受限 | 直接起的进程会继承 API key，没有超时、不能取消，还会读仓库的 fsmonitor 配置（0004 P1/P2、0014 B7） |
| 直接 `std::fs::write` 写配置、状态、会话这类 cove 自己的文件 | `persist::write_atomic`、`persist::atomic_json`；读出、修改、写回用 `persist::with_lock` 包住 | 写一半断电会写坏文件；两个窗口同时写会互相覆盖，丢规则（0007 P5/P6）。写用户工作区的文件不受这条限制 |
| 自己打开锁文件并加锁 | `persist::lock`、`persist::try_lock`、`persist::try_lock_async` | 以前 11 处各写一遍，等待和报忙的方式都不一样（0007 P4） |
| 给人看的时间用 UTC | `chrono::Local`；存储和账本按 UTC 是有意的，不要改 | `/resume detail`、导出、回放曾显示 UTC（0014 B13） |
| 自己过滤会话列表 | `Store::sessions(SessionScope::…)` | dream 自己过滤，把空会话也算了进去（0014 B14） |
| 对一个正在跑的、会花钱或会改状态的任务套 `tokio::time::timeout` 后直接丢弃 | `tokio::pin!` + `select!`：超时时先取消，再 await 它结束 | 直接丢弃后花费没记账，状态一直停在"运行中"（0014 B1） |
| 结构体里起了后台任务，却不在它销毁时停掉 | 保存 `JoinHandle`，在 `Drop` 里 `abort()` | 子代理销毁后，它的预压缩还在后台花钱（0014 B5） |

## 二、改动时容易漏的地方

- **新增用量字段**（`Usage` 里加一项）：所有累加、计价、记账的地方都要加。`add_spend` 曾漏掉 `cache_write_1h`（0014 B2）。先 `grep` 一个已有字段，看它出现在哪些地方。
- **删除或改名命令、参数**：同时搜 `src/`、`tests/`、`docs/` 里的旧名字。各模块的 `HELP` 字符串同时驱动 `/help`、补全和"你是不是想输入"，留着旧行，补全就会给出已经删掉的命令（0014 B10、B11）。
- **改了行为就改文档**：同一次改动里同步 `docs/reference.md` 和 README。文档和代码对不上的情况出过好几次：memory 的 id 冲突（0014 B4）、1 小时缓存的适用范围（B6）、权限模式写成"改文件前先问"（0015）。
- **新增给人看的文字**：用中文，和界面其他地方一致。

## 三、流程

1. **先写优化文档，再改代码。** 写在 `docs/optimizations/NNNN-*.md`，按那里 README 的模板写，并加进索引；改完回填验证结果和状态。拿本机数据（`~/.cove` 下的会话、trace、账本）当证据时，先确认问题在当前版本上还存在：数据来自旧版本，之后可能已经修了（README 规则 3）。
2. **禁止 `cargo fmt`**：它会重排 120 多个无关文件。只格式化自己改过的文件：
   `rustfmt --edition 2024 --config max_width=100 --emit stdout < 文件 > 临时文件`，确认后再拷回。
   **文件列表要用自己改过的文件，不要用 `grep -l` 搜出来的列表**：搜出来的会带上没改过的文件，曾因此误格式化 `safety/permission/risk.rs`。
3. **这个目录是 git 仓库，改错了用 `git restore` / `git checkout` 回退。** 批量改多个文件的脚本，执行前先提交，或把涉及的文件复制一份到临时目录。
4. **删功能要删干净**，不留转发函数和兼容空壳；确实要保留的，写明到什么时候删。
5. **验证**：先格式化、跑 `cargo clippy --release --all-targets`（不新增警告，现有 5 条），改界面的接着用 release 版实际跑一遍，最后跑 `cargo test` 全量通过；改动很小、只碰到少数文件时，可以先只跑受影响的测试，全量留给收尾的自动验证。收尾那一次用 debug 构建，命令就写 `cargo test`，不加 `--release`、管道或别的参数，并且给足 `timeout_seconds`：超过 120 秒它会转到后台，结果要等下一次模型调用前才记进"通过"，那条记录不检查这期间改没改文件，别拿它当"最后一次改动之后的通过"。收尾自动验证跑的就是 `cargo test`，只有命令原文相同、并且之后没有再调非只读的工具（`write`、`edit`、`agent`、`execute_plan` 等）、没有再跑分类上可能改文件的命令才复用结果（0062）——判据不是"改没改文件"，而是"这条调用按分类会不会改动"；`cargo build --release` 这类构建命令和 git 提交不算，`target/release/cove` 这类带路径的程序算。格式化后拷回也照样作废（改的是源码，要重编）。三、1 的回填写在它之后也允许，代价是收尾验证会再跑一遍测试（回填文档不动源码，不用重编）；想省这一遍就先写文档、把 `cargo test` 放到最后。

## 四、本机工具的坑（Windows + Git Bash）

- **heredoc、`sed`、`python -c` 会吞掉反斜杠**：正则（`\w`、`\b`）、Windows 路径都会被改坏，`\b` 甚至变成退格字符。含反斜杠的内容，用编辑工具写进文件，再执行文件。
- **Python 打印中文乱码**：是控制台按 GBK 输出，不是数据坏了。用 `python -X utf8`。
- **`python -I` 会屏蔽用户目录下装的包**（比如 pywinpty），看起来像没装。
- **`grep` 搜 `\\?\` 这类带反斜杠的字面量**：用 `grep -F`。
- **`ls | head`、`| tail` 会截断输出**：判断"文件不存在"之前，先看完整列表。
- **`rm -rf` 删目录会被安全检查拦下**：清理演示项目时用 `git checkout` / `git clean`。
- **分析本机数据时不要读 `~/.cove/config.json`**：`profiles.*.provider.api_key` 是嵌套字段，只过滤顶层的 key 会把它原样打印出来。要知道有哪些 profile、模型是什么，用 `cove --dump-system-prompt --profile <名>` 或 `/status`；trace、会话、账本里没有 key。（出处：2026-11-23 两次分析数据时，都因为只过滤了顶层字段，把 API key 打印到了命令输出里）

## 维护这份文档

- 同一个坑踩了第二次，或者踩一次就出了严重问题（丢数据、泄露密钥、花钱），就加进来。
- 每条写三样：禁止什么、改用什么、出处（优化文档编号、bug 编号，或当时的现象）。
- 统一入口改名或删除时，同步改这里。
