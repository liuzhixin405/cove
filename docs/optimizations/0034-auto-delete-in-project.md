# 0034 auto 模式下删除项目内、可撤销的文件不再需要批准

- 状态：已完成
- 日期：2026-11-24（创建）；2026-11-24（完成）
- 关联：0009（回滚点按任务记录）、0024（没有终端时拒绝并交回模型）、评测用例 `cleanup-unused-config`

## 背景

`auto` 模式下，在项目里新建、改写文件都不用批准，删文件却每次都要批准。没有终端可以问的时候（`cove -p`、评测），按 0024 的规则，这次删除被拒绝、交回模型，"清理没用的文件"这类任务做不完，只能停在"指出哪个文件没用"。

## 现状与问题

1. **文件工具不能删除。** 工具只有 `write`、`edit`（`src/tools/mod.rs:259-270`），删文件只能用 shell 的 `rm`、`del`、`Remove-Item`。
2. **shell 删除在 `auto` 下一律要批准。** `mode_default`（`src/safety/permission.rs:108-139`）里，`auto` 只放行 `classify::auto_approvable`（Safe/Build）和 `risk::reasons` 为空的命令。`risk.rs` 把删除列为"删除文件（rm）""删除文件（remove-item）"（`risk.rs:1315`、`risk.rs:1403`），不区分删的是项目里的文件还是 `~`。写文件则按路径判断：项目内直接放行（`permission.rs:131-135`）。
3. **没有终端时任务做不完。** 评测用例 `cleanup-unused-config` 的判定只能接受"文件还在、但指出它没人用"（`evals/cases/cleanup-unused-config/check.py:22-28`，注释："Without a terminal, `auto` cannot delete"）。最近一轮 `20261124-150614-deepseek` 里这个用例 3/3 通过，但都没有真正删掉 `config/old-staging.ini`。这是在当前版本上跑出来的结果。
4. **能不能撤销，取决于回滚点。** 每次改动类调用之前都会打回滚点（`src/session/snapshot.rs:1335-1350`），删除可以用 `/undo` 恢复（"recreate"，`snapshot.rs:143`）。但有三种情况撤销不了：
   - 回滚点会排除一些文件：项目 Git 忽略的、未跟踪的隐藏文件、大文件、`.coveignore` 里的（`snapshot.rs:163-176`）。
   - 打点可能失败或超时。
   - 打点可能已经在这个会话里关掉（`Before::Failed`、`Slow`、`Off`）。

   这三种情况下删掉的文件找不回来。

## 目标

- `auto` 模式下，下面这类 shell 命令不用批准，直接执行：
  - 只删除项目内的文件；
  - 每个路径都写成字面量；
  - 每个文件都在回滚点里，并且这次调用前确实打了点。
- 在 `cove -p` 下 `cleanup-unused-config` 真正删掉 `old-staging.ini`。判定改成要求删掉，3/3 通过。
- 以下情况仍按原样要批准（没有终端时拒绝）：
  - 删项目外的文件；
  - 删整个目录（`-r`）；
  - 路径含通配符或变量；
  - 删回滚点不覆盖的文件；
  - 回滚点没打成功。
- `default` 模式不变。

## 范围

- 做：
  - `auto` 下 POSIX 的 `rm`/`unlink`，以及 PowerShell 的 `Remove-Item`/`del`/`rm`，用于删除字面量文件路径的情况。
  - 放行前确认两件事：文件在回滚点里，以及这次调用前的回滚点已经打成。
  - 修改评测判定。
- 不做：
  - 新增 `delete` 文件工具。工具定义每轮都会进入提示词，和后面要做的"精简系统提示词"方向相反；而且 shell 删除本来就存在，只需要调整权限判断。
  - 放开递归删除目录。一条命令可能删掉成百上千个文件，回滚点的覆盖情况不好逐个确认。
  - `find -delete`、`xargs rm`、`git rm`、`git clean`。路径在执行前看不到，或者属于 git 自己的语义。
  - `default` 和 `plan` 模式。

## 调研与参考

- Codex CLI：`apply_patch` 支持 `*** Delete File`，在 workspace-write 沙箱里删除项目内文件不需要批准。它靠工作区可写这一层沙箱兜底，不靠能否撤销。
- Claude Code：没有删除工具，`rm` 走 Bash 权限规则，默认要问。用户可以加 `Bash(rm:*)` 规则，但这条规则不区分路径。
- 取舍：cove 有每次调用前打的回滚点，可以做得比"一律问"更细：能撤销的删除放行，撤销不了的照旧要问。这比 Codex 的"工作区内一律可写"更保守。

## 方案

1. **识别"纯删除"命令**（`src/safety/permission/classify.rs`）。新增 `deletes(command, dialect) -> Option<Vec<String>>`：
   - 整行只由删除命令组成，可以用 `&&`、`;` 连接多条；
   - 每条都是 `rm`/`unlink`/`Remove-Item`/`del`/`rm`(PowerShell 别名)；
   - 选项只允许 `-f`、`--`、`-Force`、`-LiteralPath`/`-Path`；
   - 参数都是字面量，不含 `* ? [`、`$`、`~`、反引号、命令替换；
   - 满足这些条件时返回路径列表，否则返回 `None`。
   - 带 `-r`/`-R`/`--recursive`/`-Recurse` 的一律返回 `None`。
2. **路径检查**：每个路径相对 cwd 解析后要满足下面四条，任何一条不满足就不放行：
   - 在工作区内（按 `paths::key` 比较）；
   - 不在 `.git/` 里；
   - 是普通文件，不是目录，也不是符号链接；
   - 回滚点覆盖到它（`Exclusions::covers` 为 false）。
3. **回滚点确认**：放行的前提是这次调用前的 `before_call` 返回了 `Before::Taken`。现在打点（`snapshot.rs` 的 tool 包装）在权限判断之前还是之后，需要在实现时确认：
   - 如果在之后，就把顺序调整为先打点、再判断这一类命令。
   - 如果做不到，就退一步：只在本会话的回滚点处于开启状态，并且最近一次打点成功时放行，并把这个限制写进遗留。
4. **结果展示**：放行的删除在输出里照常显示命令，并附一行"可用 /undo 恢复"，让人知道删了什么、怎么找回。
5. **评测**：`cleanup-unused-config/check.py` 改成要求 `config/old-staging.ini` 已被删除，并且另外三个文件保持不变；selftest 增加两种情况：
   - 删掉了 `old-staging.ini`，判定通过；
   - 只指出、没删，判定失败。

   另外新增用例 `delete-ignored-file-asks`：要求删除一个被 `.gitignore` 忽略的文件。预期是没有删成，模型说明需要批准。

## 影响范围

- `src/safety/permission.rs`：`mode_default` 增加一个分支，需要传入工作区根目录和回滚点覆盖信息。现有签名里有 `roots`，回滚点信息要新增参数，或者在 `Permissions` 层判断。
- `src/safety/permission/classify.rs`：新增 `deletes`。
- `src/session/snapshot.rs`：对外提供"路径是否被回滚点覆盖"和"本次调用是否已打点"。
- `evals/cases/cleanup-unused-config/`，以及新用例。
- `docs/reference.md` 的权限模式一节、README 的权限说明。
- 行为变化只发生在 `auto` 模式：以前要问的一部分删除，现在直接执行。

## 验证

- 单元测试（classify）：下面这些返回路径列表：
  - `rm a.txt`
  - `rm -f a b`
  - `rm -- -x`
  - `Remove-Item -LiteralPath a.txt`
  - `del a.txt`

  下面这些返回 `None`：
  - `rm -rf build`
  - `rm *.log`
  - `rm $F`
  - `rm ~/a`
  - `rm a && curl x`
  - `find . -delete`
  - `Remove-Item -Recurse x`
- 单元测试（permission）：`auto` 模式下：
  - 项目内被回滚点覆盖的文件：放行；
  - 项目外的文件：要问；
  - `.gitignore` 忽略的文件：要问；
  - 目录：要问；
  - 回滚点关闭时：要问；
  - `default` 模式：同样的命令仍然要问。
- 集成：在临时 git 仓库里用 `cove -p --permission auto` 删除一个已跟踪的文件，确认文件已删除；再执行 `/undo`，确认文件恢复。
- 评测：
  - `cleanup-unused-config` 和新用例各 3 次；
  - 全量 25 个用例各 1 次，确认没有退步；
  - 只跑 DeepSeek。
- `cargo test --release`、`cargo clippy --release --all-targets`（不新增警告）。

## 实施计划

- [x] 阶段 1：确认打点与权限判断的先后顺序，定下回滚点确认的接法
- [x] 阶段 2：`classify::deletes` 和权限分支，以及单元测试
- [x] 阶段 3：输出里的"可用 /undo 恢复"，同步 reference 和 README
- [x] 阶段 4：评测判定、新用例和评测运行

## 待决策

1. **放行范围要不要包括未被回滚点覆盖、但被 git 跟踪的文件？** 回滚点的排除规则已经保证 git 跟踪的文件都在里面，所以不需要单独处理。建议：不单列。
2. **回滚点没打成时怎么办？** 建议照旧要问：撤销不了的删除不放行，没有例外。
3. **删除空目录（`rmdir`）要不要放行？** 空目录不含内容，丢了也没有损失，但回滚点不记录空目录。建议这次不做，先看使用中有没有需要。

## 决策记录

- 2026-11-24：不新增 `delete` 工具，在 shell 权限判断里放行。理由是不增加每轮都要发送的工具定义。
- 2026-11-24：只放行能撤销的删除。文件必须在回滚点里，并且这次调用前回滚点已经打成；否则照旧要问。理由是 `auto` 的前提是出错能找回。
- 2026-11-24：递归删除目录、通配符、变量、`rmdir` 都不放行。理由是执行前看不清会删掉什么，或者回滚点不记录。
- 2026-11-24（实施中）：删除命令可以和只读命令连用（`rm a && ls config`），但不能和 `cd` 等换目录的命令连用。理由：评测第一轮 3 次里有 1 次写成 `rm config/old-staging.ini && ls config` 被拒；只读命令什么都不改，换目录会让路径解析和检查时不一致。
- 2026-11-24（实施中）：`git rm` 仍不放行（它还改项目的暂存区，`/undo` 只恢复工作区文件），但 `auto` 下没有终端时被拒的删除（含 `git rm`）在拒绝说明里附一句"哪种删除不需要批准"。理由：评测第一轮 3 次里有 2 次直接用 `git rm`，被拒后按"不要重试等价命令"停下，没有删成。

## 验证结果

**阶段 1 结论。** `Snapshotting::execute`（`src/session/snapshot.rs`）先 `before_call` 打点，再调用内层工具；shell 工具在内层 `execute` 里才调 `authorize`（`src/tools/mod.rs`）。所以打点本来就在权限判断之前，按方案 3 的第一种接法实现，不需要退一步：

- `Snapshots::undoes_deletion(command)`：只在这次调用的 `before_call` 返回 `Taken` 后、对 `shell` 调用运行。用 `classify::deletes` 取出路径，逐个检查：相对工作区解析后是普通文件（`symlink_metadata`，不是目录或链接）；`canonicalize` 后按 `paths::key` 在工作区之下；不在任何 `.git` 分量里；`Exclusions::covers` 为 false；最后用 `git ls-tree -r --full-tree <最新快照> -- <路径>` 确认每个路径在快照里、并且是 `100644`/`100755` 的 blob（排除符号链接和嵌套仓库）。Windows 上 `/d/x` 这类 Git Bash 绝对路径、`C:x` 这类盘符相对路径和带 `:` 的备用数据流一律不算。
- 都满足时，内层调用在 `permission::undoable(command, …)`（tokio task-local）里运行；`mode_default` 新增分支：`auto`、命令与 task-local 里的命令逐字相同、`classify::deletes` 非空，才放行。别的调用（子 Agent 里的 shell、后台 agent、打点没打成的调用）看不到这个值，照旧询问。规则（拒绝/询问）仍然先于模式默认。
- 执行成功后结果末尾加 `[checkpoint] 删除前已打回滚点，可用 /undo 恢复。`

**测试。**

- `cargo test --release`：单元测试 504 通过、6 忽略；集成测试 233 个。第一次全量（同时在跑评测自检）有 3 个计时类测试失败（`cancelled_shell_kills_its_spawned_child_before_late_writes`、`cancelled_and_timed_out_mcp_operations_kill_owned_descendants`、`files_changed_by_shell_are_progress_and_unchanged_ones_are_not`），第二次全量只剩 MCP 超时那一个；单独跑 3 次都通过，再跑一遍 `--test all` 233/233 通过。都是超时/计时断言，和本次改动的代码路径无关。
- 新增测试：`classify::tests::deletions_of_literal_files_are_recognized`（方案里列的通过/`None` 用例，外加 `-fr`、`-i`、`unlink a b`、`sudo rm`、`cd sub && rm a`、管道、重定向、PowerShell 逗号数组和两个位置参数等）；`permission::tests::auto_deletes_only_what_the_snapshot_before_the_call_holds`（没有 task-local、命令不同、`default`、`plan`、混了 `curl`、询问规则时都不放行）；`permission::tests::a_refused_deletion_in_auto_says_which_deletions_run`；`snapshot::tests::auto_deletes_only_files_the_snapshot_before_the_call_holds`（真实 git：已跟踪文件放行并删掉、结果含 `/undo`；`.gitignore` 忽略的文件、目录、`.git/config`、不存在的文件、工作区外的绝对路径、`../` 出工作区都要问；还原快照后文件回来；自动快照关闭后要问）。
- `cargo clippy --release --all-targets`：仍是原有 5 条警告（`console.rs` 2 条、`process.rs`、`policy.rs`、`tests/all/cli.rs`），没有新增。
- `python -X utf8 evals/run.py --self-test`：全部通过。

**集成。** 临时 git 仓库（`keep.txt`、`old.txt` 已提交），`cove -p "删除 old.txt …" --permission auto`：模型执行 `rm old.txt`，没有询问，结果带"可用 /undo 恢复"，`old.txt` 已删除。再用管道 REPL（`COVE_PLAIN_REPL=1`）执行 `/undo`、`/undo #1`、`/undo confirm`：预览为 `recreate old.txt`，确认后 `old.txt` 恢复，内容不变。

**评测（DeepSeek）。**

- 第一轮 `20261124-162113-deepseek`（只有方案里的放行规则）：`cleanup-unused-config` 0/3，2 次用 `git rm`、1 次用 `rm … && ls config`，都被拒；`delete-ignored-file-asks` 3/3。据此做了上面两条实施中的决定。
- 第二轮 `20261124-162654-deepseek`：`cleanup-unused-config` 3/3（都用 `rm` 真正删掉了 `old-staging.ini`，另外三个配置没变），`delete-ignored-file-asks` 3/3（`debug.log` 还在，回复说明需要批准、它被 Git 忽略不在回滚点里，并给出让用户自己执行的命令）。
- 全量 `20261124-163529-deepseek`：28/28 通过（25 个原有用例、新用例，以及运行时目录里已有的 2 个 `pub-*` 用例）；基准 `20261124-150614-deepseek` 为 74/75，没有退步；同一批用例每次平均费用 $0.0082 → $0.0091。

## 遗留

- `git rm`、`find -delete`、`rmdir`、递归删除仍要批准（按范围不做）。`git rm` 若要放行，需要 `/undo` 同时恢复项目暂存区，另开文档。
- 放行依据是"刚打的快照里有这个文件"，执行时不再复查一次；检查和执行之间如果有别的进程把文件换成链接，仍按普通删除执行。概率低，不单独处理。
- 删除命令的路径按工作区根目录解析，与 shell 工具的运行目录一致；若以后 shell 工具支持别的工作目录，需要把那个目录传给 `undoes_deletion`。
- 新发现：`/undo` 列出的回滚点时间显示的是 UTC（本机 16:20 显示为 08:20），不符合 AGENTS.md"给人看的时间用 `chrono::Local`"，另开文档处理。
