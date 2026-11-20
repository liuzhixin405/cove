# 0004 统一执行外部命令的环境变量与 git 调用

- 状态：已完成
- 日期：2026-11-19（创建）；2026-11-19（完成）
- 关联：同类功能统一分析（2026-11-19 对话记录，第一组）

## 背景

做同类功能统一分析时发现：执行外部命令这件事，在代码里有好几套实现，安全措施各不相同。用户同意按建议的顺序统一，这是第一组。

## 现状与问题

**P1 cove 正在使用的 API Key 会被模型执行的命令读到。**
- shell 工具、自动验证、regression、`/init` 的验证命令都通过 `process::run`（`src/process.rs`）执行。只有钩子命令走白名单环境（`clean_environment`），其他命令继承 cove 的全部环境变量，**什么都不移除**。
- 对比：改动集的检查命令（`workflows/changes.rs` 的 `run_checks`）会移除 `COVE_API_KEY`；`cove -p` 子进程（`workflows/worker.rs`）会移除 `COVE_*` 后再显式传入密钥；MCP、浏览器、钩子用白名单重建环境。同一件事，三种策略。
- 影响：如果 API Key 是通过 `COVE_API_KEY` 或服务商环境变量（比如 `DEEPSEEK_API_KEY`）设置的，模型执行一条 `echo $COVE_API_KEY` 就能读到它，然后可能写进文件、日志或回复里。

**P2 git 辅助函数有 8 份，安全措施各不相同。**

| 位置 | 问题 |
|---|---|
| `safety/gates/acceptance.rs` `git` | 继承标准输入，没有设置 `GIT_TERMINAL_PROMPT=0`，可能卡在等待输入上 |
| `safety/gates/commit.rs` `git` | 同上 |
| `engine/prompt.rs` `git` | 同步阻塞调用，**没有超时**；它在构建系统提示时运行，git 一旦卡住，cove 就无法启动 |
| `workflows/gitops.rs` `git_in` | 没有超时，也不能取消 |
| `extensions/plugins.rs` `git` | 没有关闭 fsmonitor（仓库配置可以借 fsmonitor 运行任意程序） |
| `agents/worktree.rs` `git` | 自己写了一份限量读取输出的逻辑 |
| `session/snapshot.rs`（5 处） | 需要隔离配置和私有索引，属于合理的特殊需求；但输出不设上限、不能取消 |
| `workflows/changes.rs` `git_raw` | **唯一做全的一份**：清理 `GIT_*`、超时后杀掉整个进程树、限制输出大小 |

另外，只有部分实现会清理 `GIT_DIR`、`GIT_WORK_TREE` 这类变量。如果 cove 是从 git 钩子里启动的（比如 husky），这些变量可能让 git 操作到错误的仓库。

**P3 限量读取输出的循环写了 3 份**（`worktree.rs` 的 `read_capped`、`process/argv.rs` 的 `capture_limited`、`tools/search.rs` 的 stderr 读取）。**按字节截取末尾的函数写了 3 份**（`agents/regression.rs`、`safety/verify.rs`、`workflows/changes.rs` 的 `tail`）。

## 目标

1. cove 正在使用的密钥（`COVE_API_KEY`，以及密钥实际来源的那个服务商变量）不会出现在模型执行的任何命令的环境里。
2. 所有 git 调用都经过同一个入口，默认带上：非交互（`GIT_TERMINAL_PROMPT=0`、`GCM_INTERACTIVE=never`、标准输入为空）、关闭 fsmonitor、清理会把 git 引到错误仓库的变量、超时、超时或取消时杀掉整个进程树、限制输出大小。特殊需求（快照隔离、临时索引、清理全部 `GIT_*`）通过选项开启。
3. 构建系统提示时的 git 查询有超时。
4. 限量读取和截取末尾各只保留一份。

## 范围

- 做：
  - 新增 `src/process/git.rs`：基于 `run_argv(...).direct()` 的 `Git` 构造器，以及供构建系统提示使用的阻塞版本；
  - 新增 `src/process/secrets.rs`（或并入 `process` 模块）：要保护的环境变量列表，启动时登记 cove 实际使用的密钥来源；`process::run`（非钩子分支）和 `run_argv` 默认移除这些变量；
  - 8 个 git 调用点迁移到 `Git`；
  - `tail` 合并成 `process` 模块里的一个函数；`worktree.rs` 的 `read_capped` 删除（改用 `Git`）；`tools/search.rs` 的 stderr 读取改用公共的限量读取。
- 不做：
  - **不移除所有服务商的环境变量**：只移除 cove 当前实际在用的那一个。理由：用户自己的脚本或测试可能要用到其他服务商的密钥（比如项目里调用 OpenAI 的集成测试），全部移除会把它们搞坏。
  - 不改 MCP、浏览器、钩子的白名单环境（它们已经是最严格的做法）。
  - 不改 shell 工具执行命令的方式（路由、Job Object、输出的头尾截取）。
  - `tools/search.rs` 启动 rg 的方式（没有 Job Object）这次不改，只统一 stderr 的读取；改进程树处理属于另一个话题。

## 方案

**环境变量**
- `process::secrets::protect(names)`：启动时登记 `COVE_API_KEY`，以及 `Settings.key_source` 里的服务商变量（来源是配置文件时就不登记）。用 `OnceLock` 保存。
- `process::run`：不是钩子时（钩子已经走白名单），对每个受保护的变量调用 `env_remove`。
- `run_argv`：默认移除受保护的变量；需要密钥的调用方（`cove -p` 子进程）本来就是在移除之后再显式设置，不受影响。

**git**

```rust
pub struct Git { /* argv, scrub mode */ }
impl Git {
    pub fn new(dir: &Path) -> Self;            // 默认：-c core.fsmonitor=false、非交互、清理定位类变量、60 秒、4 MiB
    pub fn arg(self, ..) / args(self, ..);
    pub fn env(self, key, value);              // 显式设置，不会被清理
    pub fn timeout(self, Duration);
    pub fn limit(self, usize);                 // 输出超过上限时报错，绝不静默截断
    pub fn stdin(self, Vec<u8>);
    pub fn isolated(self, git_dir: &Path);     // 快照：空的全局配置、不读系统配置、去掉 GIT_CONFIG_PARAMETERS/COUNT
    pub fn scrub_all(self);                    // 改动集：清理全部 GIT_*
    pub async fn output(self, &CancellationToken) -> Result<GitOutput>; // 不把失败当错误
    pub async fn bytes(self, &CancellationToken) -> Result<Vec<u8>>;    // 失败或超限即错误，错误信息带 stderr
    pub async fn text(self, &CancellationToken) -> Result<String>;
    pub async fn query(self, &CancellationToken) -> Option<String>;     // 只读查询：失败返回 None
    pub fn query_blocking(self) -> Option<String>;                      // 构建系统提示用，带超时
}
```

- "定位类变量"指会让 git 操作到别的仓库或改变配置的变量：`GIT_DIR`、`GIT_WORK_TREE`、`GIT_INDEX_FILE`、`GIT_OBJECT_DIRECTORY`、`GIT_ALTERNATE_OBJECT_DIRECTORIES`、`GIT_COMMON_DIR`、`GIT_NAMESPACE`、`GIT_CONFIG_PARAMETERS`、`GIT_CONFIG_COUNT`。网络相关的变量（`GIT_SSH_COMMAND`、`GIT_ASKPASS`、代理）**保留**，插件安装要从远程拉取，需要用户自己的 SSH 和凭据配置。
- 各调用点保持原来的超时：快照 120 秒（垃圾回收 300 秒）、插件 120 秒、提交门 30 秒、验收门 10 秒、构建系统提示 10 秒、其余 60 秒。
- 快照的输出上限设为 1 GiB（原来不设上限），超出时报错。理由：快照必须拿到完整输出，静默截断会损坏数据；1 GiB 只是给内存设一个上界。
- 错误信息统一为 `git <子命令> 失败：<stderr 截取前 2000 字>`；找不到 git 时为 `找不到 git（<原因>）；请安装 git 并加入 PATH`。

**输出读取**：`process::tail_bytes(text, limit) -> &str`（在字符边界上截取），需要"[...]"前缀的调用方自己加。

## 影响范围

- 新增 `src/process/git.rs`、`src/process/secrets.rs`；修改 `src/process.rs`、`src/process/argv.rs`、`src/agents/worktree.rs`、`src/engine/prompt.rs`、`src/extensions/plugins.rs`、`src/safety/gates/acceptance.rs`、`src/safety/gates/commit.rs`、`src/session/snapshot.rs`、`src/workflows/changes.rs`、`src/workflows/gitops.rs`、`src/agents/regression.rs`、`src/safety/verify.rs`、`src/tools/search.rs`、`src/app/mod.rs`（启动时登记密钥）。
- **行为变化**：模型执行的命令里读不到 cove 正在使用的密钥；如果用户的脚本依赖 `COVE_API_KEY`，会读不到（这正是要修的问题）。验收门、提交门、`/diff git`、`/commit` 的 git 调用不再继承标准输入，也不会弹出凭据输入提示。
- 没有数据格式变化。

## 验证

- 单元测试：
  - 受保护变量的计算：`COVE_API_KEY` 一定在列表里；`key_source` 是服务商变量时加入，是 `config` 时不加入；
  - `Git` 的环境：定位类变量被移除，显式设置的变量保留，`scrub_all` 会移除全部 `GIT_*`；
  - `query_blocking` 在超时后返回 `None`，不会挂起；
  - `tail_bytes` 在多字节字符上截取正确。
- 集成测试：在设置了受保护变量的情况下运行一条 shell 命令，确认输出里没有它的值。
- 全量 `cargo test` 通过，重点关注快照、`/undo`、提交门、改动集、插件、worktree 相关的测试。
- 只对改动过的文件，通过标准输入运行 `rustfmt`。

## 实施计划

- [x] 受保护的环境变量：登记、`process::run` 和 `run_argv` 移除
- [x] `process::git`：构造器、阻塞版本、测试
- [x] 8 个 git 调用点迁移
- [x] `tail` 和限量读取合并
- [x] 测试、格式化，回填验证结果

## 待决策

无。

## 决策记录

- 2026-11-19：按"同类功能统一分析"的建议顺序实施，这是第一组。（用户："按照建议做"）
- 2026-11-19：只移除 cove 当前实际使用的密钥变量，不移除所有服务商变量；网络相关的 git 变量保留。理由见"范围"。（实施方）
- 2026-11-19：实施时发现第 9 处直接调用 git 的代码：`tools/line_endings.rs`（write/edit 工具每次写文件都会运行它来判断换行符；同步调用，没有超时，也没有关闭 fsmonitor）。一并迁移到 `Git::query_blocking`，超时 10 秒。（实施方）
- 2026-11-19：`/diff git` 和 `/commit` 的 git 输出上限设为 64 MiB（显示时本来就会再截取到 256 KiB）；快照设为 1 GiB；其余使用默认的 4 MiB。（实施方）
- 2026-11-20 收尾核对（[0014](0014-audit-fixes.md)）：
  - worktree 的 git 超时是 120 秒，不是"其余 60 秒"；
  - 保护函数名是 `protect_key_source`；
  - `/diff git`、`/commit` 只有超时，不能中途取消；
  - 计划中的"`scrub_all` 去掉所有 `GIT_*`"测试没有写；
  - 核对时发现 `Git::query_blocking` 没有去掉受保护的变量、输出超长时静默截断，已在 0014 修正并补测试。

## 验证结果

- **新增测试**：
  - `process::secrets`：`COVE_API_KEY` 一定在受保护列表里；密钥来源是服务商变量时加入该变量，是 `config` 时不加入；
  - `process::git`：定位类变量被移除，显式设置的保留，网络变量（`GIT_SSH_COMMAND`）不动；子命令名能跳过 `-c` 的值；Windows 带盘符的路径去掉 `\\?\` 前缀，UNC 路径保留；真实运行 git 时，查询失败返回 `None`，错误信息为"git rev-parse 失败"，阻塞版本能正常拿到结果；
  - `process::tail_bytes`：在多字节字符上截取正确；
  - **端到端**：`shell_commands_the_model_runs_never_see_the_api_key`。用 mock 模型让 cove 执行 `echo "[$COVE_TEST_VISIBLE][$COVE_API_KEY]"`，返回给模型的工具结果是 `[yes][]`：普通变量能读到（证明 shell 确实在展开变量），密钥读不到。**反向验证**：临时去掉 `process::run` 里移除密钥的代码，这个测试会失败；恢复后重新通过。
- `cargo test` 全部通过：单元测试 376 个（含 4 个忽略），集成测试 223 个。
- 除测试代码外，`src/` 里只剩 `process/git.rs` 一处直接启动 git（`rg 'Command::new("git")'`）。
- 只对改动过的文件，通过标准输入运行了 rustfmt；`cargo build --release` 完成。
- **顺带修掉的潜在 bug**：`agents/worktree.rs` 原来会把 git 输出截断到 64 KiB，并在末尾加上"[truncated]"。regression 工具用它执行 `git show <版本>:<文件>` 来读取修复前的文件，所以超过 64 KiB 的文件会被换成截断后的内容。现在输出超过上限会直接报错，不会再静默截断。

## 遗留

- `tools/search.rs` 启动 rg 的方式（没有进程树处理）这次没改，按范围约定另行处理；这次只统一了 stderr 的限量读取。
- 测试代码里还有几处直接调用 git，用来搭建临时仓库，不涉及用户环境，保持原样。
