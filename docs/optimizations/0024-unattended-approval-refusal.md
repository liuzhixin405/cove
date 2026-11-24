# 0024 没有终端可询问时，拒绝这一次调用并告诉模型，不再终止任务

- 状态：已完成
- 日期：2026-11-23（创建）；2026-11-23（完成）
- 关联：0023（评测里发现）

## 背景

0023 用 DeepSeek 跑基准（`--permission auto`，10 个用例各 3 次），21/30 通过。9 次失败里有 7 次不是模型做错：模型调用了需要审批的命令，`-p` 没法询问，整个任务直接终止，最终回复都没有。这是产品行为的问题，应修产品，让评测在 `auto` 下测到真实行为。

## 现状与问题

1. **一条命令要审批，整个任务就终止。** `src/safety/permission.rs:349` 的 `Permissions::ask` 在没有输入通道时返回 `Error::Approval`；`src/engine/agent.rs:1266`、`:1327` 遇到 `Approval` 就把调用标回 `Planned` 并结束任务。评测现场：
   - `cleanup-unused-config` 3/3：模型已经查清只有 `old-staging.ini` 没人用（连 `.github/workflows/ci.yml` 对 `ci.ini` 的引用都找到了），执行 `git rm config/old-staging.ini` 时任务被终止。
   - `unverifiable-tests` 3/3：模型想用 `python -c "import cove_eval_fixtures"` 确认依赖缺失，任务被终止，功能都没开始改。
   - `git-merge-request` 2/3、`scope-discipline` 1/3：`python -c` 验证或 `cd … && git -C …` 查询被终止。

   被终止的这几次，`[summary]` 都没打出来，模型调用次数统计为 0。
2. **报错信息误导。** 已经是 `--permission auto` 时，报错仍然写"请明确指定 --permission auto（允许项目内的构建/测试命令和编辑）"（`permission.rs:351`、`:602`）；诊断码 E3002 的建议也是"无人值守时显式使用 --permission auto（会执行写入与 shell）"（`src/app/diagnostics.rs:288`）。实际上 `auto` 对这类命令照样询问（reference"权限与限额"一节）。
3. **文档前后矛盾。** `docs/reference.md` 的权限表写 `auto`"无终端时拒绝"，同一文件"安全拦截"前一段写"需要询问但没有终端时（如 `-p`），任务停止并保留状态，换模式后可 `/continue`"。
4. **同类产品的做法相反。** Claude Code 的 `-p` 和 Codex CLI 的 `exec`：没被批准的调用作为"被拒绝"交回模型，模型换做法或在结尾说明，任务不终止；结束时列出被拒绝的调用。
5. **后台 agent 和管道输入的 REPL 同样会终止**：`src/agents/background.rs:127`（后台 agent 无交互终端）、`src/app/ui.rs:49`（stdin 不是终端时 `answer` 返回 `Approval`）。

## 目标

- 没有终端可以询问时，这一次调用不执行，模型收到明确说明（没人能批准、不要重试同类命令、换不需要审批的做法，或在最终回复里写明用户要手动做什么），任务继续。
- 用户在 stderr 上看到每一个因此没执行的操作，以及放行的办法。
- 报错和诊断建议不再推荐一个不起作用的模式。
- 文档前后一致。

## 范围

- 做：`Permissions::ask`、`LocalAuthorizer`、后台 agent 的无终端分支、管道 REPL 的审批；主循环里对新错误的处理；界面摘要；诊断建议；相关测试和文档。
- 不做：
  - 不改风险判断本身（比如 `python -c` 要不要审批）。那是"哪些命令需要问"的问题，另开文档调研。
  - 不改 `ask` 提问工具在无终端时的行为（`src/tools/mod.rs:830`）：`-p` 本来就不注册它。
  - 不改策略拒绝、灾难命令的处理：它们已经是"拒绝并继续"（`tests/all/safety.rs` 有覆盖）。

## 调研与参考

- Claude Code `-p`：未授权的工具调用返回权限拒绝，模型继续；`--output-format json` 的结果里有 `permission_denials` 列表。
- Codex CLI `exec`：非交互时审批策略为 never，需要审批的命令直接失败并把结果交给模型。
- cove 现有的"用户拒绝"路径（`Error::Denied` → `denied()`，`refused: true`，不计为失败）已经是"拒绝并继续"，只是措辞是"用户拒绝了"。

方案对比：

| 方案 | 优点 | 缺点 |
|---|---|---|
| 复用 `Error::Denied` | 改动最少 | 模型和界面都会说"用户拒绝了"，与事实不符；区分只能靠匹配字符串 |
| **新增 `Error::Unattended`** | 语义准确；模型提示、界面摘要、诊断码可以各自对应 | 多一个错误类型；其他模块把它当普通错误处理（转成失败输出给模型），行为正确 |
| 保持终止，只改报错信息 | 不改行为 | 评测和 CI 里 agent 一遇到要审批的命令就停，问题 1 不解决 |

取第二种。

放弃的东西：以前终止时会保留状态，换模式后可以 `--continue` 接着跑。改后模型会自己换做法或结束；要放行这类命令，用 `--permission bypass` 或 `policies.json` 的 allow 规则重跑。`--continue` 本身不受影响（中断、出错后的继续照常）。

## 方案

1. `src/domain.rs`：新增 `Error::Unattended(String)`，显示为"需要批准，但没有终端可以询问，未执行：{0}"（保留"无法询问"字样，诊断码仍归到 E3002）。
2. 产生它的地方：
   - `Permissions::ask`：没有输入通道时返回 `Unattended(描述)`；输入通道返回 `Approval`（管道 REPL，stdin 不是终端）时也转成 `Unattended`。
   - `LocalAuthorizer`（子 agent、测试）：没有输入通道时同上。
   - `background.rs` 后台 agent 无交互终端：同上，描述里注明是后台 agent。
3. `src/engine/agent.rs`：`run_call` 和 `run_concurrent` 把 `Unattended` 转成工具结果 `unattended(&reason)`（`failed: true`、`refused: true`，不计入失败），同时发一条 `Event::Notice`："未执行（没有终端可以询问）：<描述>。要放行，用 --permission bypass 或在 policies.json 加 allow 规则"。给模型的文字：

   > This call needs approval, but nobody can be asked: cove is running without a terminal (-p, a pipe or a background agent), so it was not run (…). Do not retry it or an equivalent command. Use a way that needs no approval (for example the write and edit tools for file changes), or finish without it and say in your final reply what the user has to run or allow.

4. `src/app/render.rs`：识别上面这段，摘要显示"未执行：没有终端可以询问（已告诉模型）"。
5. `src/app/diagnostics.rs`：E3002 的建议改成"无人值守时用 --permission bypass，或在 policies.json 加 allow 规则；auto 仍会询问改文件、执行代码等命令"。
6. 删掉旧的两段报错文字（`permission.rs:351`、`:602`），不留兼容。

## 影响范围

- 代码：`src/domain.rs`、`src/safety/permission.rs`、`src/agents/background.rs`、`src/engine/agent.rs`、`src/app/render.rs`、`src/app/diagnostics.rs`。
- 行为：`-p`、管道、后台 agent 中需要审批的调用不再终止任务，退出码由任务本身的结果决定。依赖"遇到审批就以非零退出"的脚本会受影响，在 reference 里写明。
- 数据格式：无变化。会话里该调用记为已完成、结果为拒绝说明。

## 验证

- 修改 `tests/all/safety.rs::auto_mode_allows_project_edits_but_asks_for_unknown_shell`：模拟模型先写文件、再执行需要审批的 shell、最后回复文字。断言：
  - 第二次请求里的工具结果含"nobody can be asked"；
  - `b.txt` 不存在；
  - 第三次请求发生（任务继续）；
  - 退出码为 0；
  - stderr 含"未执行"和"--permission bypass"。
- 修改 `tests/all/core.rs::noninteractive_default_allows_workspace_writes_but_denies_shell`：期望 `Err(Error::Unattended(_))`。
- `src/app/render.rs` 加单元测试：识别新提示并显示"未执行"。
- `cargo test --release` 全部通过；`cargo clippy --release --all-targets` 不新增警告。
- 重新编译 release，用 `python evals/run.py --repeat 3 --jobs 3`（`auto`）重跑 0023 的基准，对比上一次。

## 实施计划

- [x] 阶段 1：错误类型与产生处
- [x] 阶段 2：主循环、界面、诊断
- [x] 阶段 3：测试
- [x] 阶段 4：文档（reference 三处）
- [x] 阶段 5：全量测试、clippy、评测重跑

## 待决策

无。

## 决策记录

- 2026-11-23：采用方案 A：修复产品行为，而不是让评测改用 bypass 或加允许规则，原因是评测要测到 `auto` 下的真实行为，而 `-p`、管道、后台 agent 遇到审批就终止本身就是产品问题。
- 2026-11-23：新增 `Error::Unattended`，不复用 `Denied`，理由见"调研与参考"。

## 验证结果

- `cargo test --release`：单元测试 404 通过（4 ignored），集成测试 216 通过，0 失败。
  - 改写后的 `safety::auto_mode_without_a_terminal_refuses_the_call_and_the_task_goes_on` 通过：拒绝说明交到了模型手里，任务继续到第 3 次请求，退出码 0，stderr 有"未执行"和"--permission bypass"。
  - `core_tests::noninteractive_default_allows_workspace_writes_but_denies_shell` 改为期望 `Unattended`，通过。
  - 策略拒绝、灾难命令、plan 模式三个测试不受影响。
- `cargo clippy --release --all-targets`：仍是原有 5 条警告（`console.rs` ×2、`process.rs`、`policy.rs`、`tests/all/cli.rs`），没有新增。
- 改过的 8 个 Rust 文件逐个用 `rustfmt --edition 2024 --config max_width=100 --emit stdout` 对比，没有差异。
- 评测（0023，`auto`，DeepSeek，10 个用例 × 3）：修复前 21/30，另有 9 次被终止、结果未知；修复后 30/30。终止的 9 次全部恢复。现场里模型的表现符合预期：删除被拒后没有绕过，回复写明"文件还在"并给出 `git rm config/old-staging.ini`；`python -c` 被拒后改用文件工具完成修改，并如实说明"没有任何测试通过的证据"。

## 遗留

- **`auto` 下不能删除项目内的文件。** 文件工具只有 write/edit，删除只能走 shell，而 shell 删除要审批。这和"`auto` 放行项目内的修改"不一致，评测 cleanup-unused-config 3 次都卡在这里。另开文档：给文件工具加删除，或把项目内的 `rm`/`git rm` 归为项目内修改。
- **`python -c` 等"读不到内容的代码"一律要审批**，是写代码时最常用的验证手段，交互使用时摩擦大。属于风险判断的调整，需要单独调研。
- 后台 agent 队列被关闭时（`background.rs` 的 `Err(_) => Approval`）仍会终止该 agent，这和"无人可问"不是一回事，没改。
