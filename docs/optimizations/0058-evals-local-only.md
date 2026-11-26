# 0058 评测工具留在本机，不进仓库

- 状态：已完成
- 日期：2026-11-26（创建）；2026-11-26（完成）
- 关联：0037、[0045](0045-expand-public-benchmarks.md)（公开题扩充，文档里提到 `evals/`）

## 背景

`evals/` 下是 55 个用例（26 个本机场景 + 29 道公开题）和评测脚本，样例仓库合计约 4 MB，其中还有 SWE-bench 题按 base_commit 从 GitHub 拉取的缓存说明。它只在开发机上用真实模型跑，会产生费用，不在 `cargo test` 和 CI 里运行，对克隆仓库、构建和发布都没有用处。提交它还会让 pre-commit 门禁去审查 500 多个与代码无关的文件，而门禁的“暂存内容与工作区一致”检查会把全部暂存路径拼进一条 `git diff --name-only -- <路径>`（`src/safety/gates/commit.rs:180-182`）：整批 664 条路径合计 33,002 字节，加上 `git.exe` 路径和参数后超过 Windows 32,767 字节的命令行上限，`CreateProcess` 报 `os error 206`（门禁把它显示成“找不到 git”），提交直接被拒绝。

## 方案

- 整目录忽略：`.gitignore` 里 `/evals/results/` 换成 `/evals/`；`__pycache__/` 一行去掉（仓库里除 `evals/` 外没有 Python 文件）。
- 文档改为说明它在本机、不在仓库：`README.md` 末尾「## 文档」列表里那一项从链接改成纯文本，`docs/reference.md` 的评测一节写明“本机的 `evals/`（不在仓库里，见本机 README）”，性能与测试口径不变。
- 代价（可接受）：被忽略后 cove 自己的 `grep`/`glob` 默认不再搜 `evals/`（要看那里的东西得传 `include_ignored: true`），快照和 `/undo` 也不再覆盖它——快照的排除规则取自项目自己的 git 忽略规则（`src/session/snapshot.rs:584` 的 `project_left_out()` 用 `git ls-files --others --ignored --exclude-standard` 收进来），`auto` 模式下删除那里的文件要走审批（正是 0034 的行为）。评测脚本在终端里直接跑，不受影响。

## 验证

- `git ls-files evals` 为空，`git log --all -- evals` 无历史，`HEAD^{tree}` 里没有 `evals` 条目：仓库从来没有跟踪过这些文件，`.gitignore` 只对未跟踪文件生效这点不影响结论。
- `git status --short` 里 `evals/` 只剩一条 `??`（忽略后不再出现）。
- 评测脚本在本机照旧可用（`python -X utf8 evals/run.py --self-test`）——`evals/` 不在仓库里，这条在 clone 下来的仓库中无法复现，只是本地状态。
- 代码未改动，本批代码在 0.8.1 的版本行上跑过 `cargo test --release`：库 610 通过 / 7 忽略，集成 269 通过，doc-tests 0；`cargo clippy --release --all-targets` 仍是原有的 5 条警告。这条提交只改 `.gitignore`、README 和文档。

## 遗留

- 0037、0045 等历史文档里的 `evals/...` 路径保持原样；只有 0029 里那处 Markdown 链接改成行内代码，避免 clone 后是死链。
- 门禁自身的两个问题这次刻意不修（属于安全闸门的行为改动，另开文档单独做）：`src/safety/gates/commit.rs:180-182` 把全部暂存路径放进一条 `git diff --name-only -- <路径>` 的 argv，大批量暂存（不只 `evals/`，公开题样例仓库、批量重排文件都会）在 Windows 上就是 `os error 206`，应改成不带路径跑 `git diff --name-only -z` 再和暂存集合求交集；`src/process/git.rs:211` 把所有 spawn 失败都说成"找不到 git"，把命令过长、权限被拒（火绒拦截）这类原因掩盖成"请安装 git"，应只对 `ErrorKind::NotFound` 那样提示。修好后就不必再为大批量暂存把提交拆小。
