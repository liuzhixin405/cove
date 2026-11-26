# 0053 第四种隔离方式：覆盖层拆分（拆细并行，最后合并、一次验证）

- 状态：已完成
- 日期：2026-11-26（创建）
- 关联：0050/0051（后台命令）、0048（按活动判断卡住）、`execute_plan`（`src/agents/plan.rs`）、后台 Agent 的 worktree 隔离（`src/agents/background.rs`、`worktree.rs`）

## 背景

长任务等得太久。本机 `~/.cove/trace.jsonl`（2026-10-27 至 2026-11-25，549 个任务）：

- 超过 10 分钟的任务 39 个，占全部任务时长的 53%；任务时长中位数 45 秒，P90 7.4 分钟，最长 60 分钟。
- 工具耗时合计 1119 分钟，模型耗时 756 分钟；工具里 shell 占 990 分钟（1328 次）。
- 现有的并行能力几乎没用：`execute_plan` 0 次、`worktree` 0 次、`agent` 39 次（多为只读调查）。

## 现状与问题

cove 现有三种分发方式（`docs/reference.md`"多个子任务怎么分发"）：

1. **`execute_plan`**：子 Agent 都在主工作区里改。同一个文件靠写入认领表互斥，后来者要等先认领的条目结束；shell 改的文件不在认领范围内。
2. **后台 Agent**（`agent {background: true}`）：每个 Agent 一个完整的 git worktree 和分支（`<数据目录>/worktrees/<项目哈希>/<id>`），结束后由用户 `/status agents merge` 合并，冲突靠 git。
3. **`team_create`**（实验）：复用计划执行器，同 1。

问题：

- P1 **worktree 太重**：每个副本都是全新检出，要重新 restore、冷构建。quote-hub 受 git 管理的源码只有 901 个文件、几 MB，但 `bin/obj` 有几百 MB、`node_modules` 172 MB，一次 `dotnet restore` 加构建就要几分钟。
- P2 **同一个文件只能互斥或靠 git 合并**：主工作区方式下同一个文件只能串行；worktree 方式下只能在分支层面合并，冲突时整个文件交给用户。
- P3 **验证重复**：每个副本各自构建、测试，还会抢构建锁、测试数据库这类共享资源。
- P4 **没有自动触发**：用户得自己说"拆开并行做"，模型也很少主动用。

## 目标

- 新增第四种方式"覆盖层拆分"（`isolation: "overlay"`）。**现有三种方式的行为、参数默认值一律不变。**
- 子任务启动不复制任何文件，不建 worktree、分支或提交；只有真正改过的文件才在子任务的私有层里有一份。
- 多个子任务可以改同一个文件的不同区域，最后自动合并；只有改到同一处才算冲突。
- 构建、测试和审查只在全部合并之后跑一次，用主工作区现有的热缓存。
- 预估超过 5 分钟、且有可以并行的部分的任务，自动提议用这种方式，先把拆分计划交给用户确认。

## 范围

- 做：覆盖层工作区；文件工具的读写视图；按申报写集合调度；三级合并；可选的共享检查槽（只编译不测试）；时长预估和触发；计划确认界面；合并报告；文档。
- 不做：
  - 子任务里跑完整的测试。验证留到最后（决策 1）。
  - 子任务里跑会生成或改写文件的 shell 命令（代码生成、`npm install`、`dotnet ef migrations`、格式化工具）。规划时把它们放进合并之后的串行步骤。
  - 需要边跑边看的调试型任务。这类任务继续用 worktree 方式。
  - 自动 `git commit`。最后只跑构建、测试和审查，提交仍由用户 `/commit`（决策 4）。
  - 文件系统层的写时复制（ReFS 块克隆、APFS clonefile、overlayfs、ProjFS）。本机 C:、D: 都是 NTFS，不支持块克隆；ProjFS 实现成本高。检查槽物化时只复制 git 管理的源码。

## 调研与参考

- **同类工具都是"每个单元一个 worktree"或者干脆不隔离**：
  - Claude Code `/batch` "decomposes the work into 5 to 30 independent units"，每个单元一个 worktree、各开一个 PR（code.claude.com/docs/en/commands）；worktree 文档原文："A worktree is a fresh checkout, so initialize your development environment"，被 gitignore 的文件要用 `.worktreeinclude` 复制进去（/worktrees）。
  - Claude Code agent teams 不做文件隔离："Two teammates editing the same file leads to overwrites. Break the work so each teammate owns a different set of files"（/agent-teams）。
  - Copilot CLI `/fleet`：用 todos/todo_deps 表排依赖，"Only serialize todos with true dependencies"，所有子代理共用同一个工作目录（本机 `runtime.node` 里的提示词）。
  - Cursor：每个并行 agent 一个 worktree，"default cap is 25 worktrees per machine"（cursor.com/docs/configuration/worktrees）。
  - Codex 云端任务：best-of-N 取值 1–4，结果用 git apply 落到本地（codex-rs/cloud-tasks）。
  - OpenHands：Docker 沙箱里用 `:ro,overlay` 挂载做写时复制。
  - **没找到在工具层做"读先查私有层、写只进私有层"的产品**（未确认）。cove 的读写都经过自己的文件工具，有条件在工具层做。
- **合并**：
  - `git merge-file` 在三个普通文件上做行级三方合并，返回冲突数，支持 `--diff3`，不需要仓库和分支（git-scm.com）。
  - Mergiraf 基于 tree-sitter 按语法结构合并："If a line-based merge is conflict-free … just returns that merge"，拿不准时"retain conflict markers"（mergiraf.org）。
  - OT/CRDT 只保证收敛，不保证合出来的代码语义正确，不采用。
- **粒度**：Claude Code 建议一个子任务大约是"a function, a test file"这么大，3–5 个成员、每人 5–6 个任务；太细时"coordination overhead exceeds the benefit"；同文件编辑和依赖多的工作"a single session or subagents are more effective"（/agent-teams）。LLMCompiler 按依赖图并行，报告"latency speedup of up to 3.7x"（arXiv 2312.04511）。

## 方案

### 1. 覆盖层工作区（`src/session/overlay.rs`，新）

- **基线**：开始时复用 `execute_plan` 前已有的检查点快照，记下主工作区每个文件的内容哈希。
- **`Overlay`**：一个子任务的私有层，存在 `<数据目录>/overlays/<任务>/<子任务>/`。
  - 内容：路径 → 新内容或删除标记；写之前记下的基线哈希；编辑意图记录。每条意图包括：路径、基线哈希、`edit` 的原文锚点和新内容，或者 `write` 的整体内容。
  - 可以叠加：子任务 B 依赖 A 时，B 的读取顺序是 B 的层、A 的层、基线。
- **读写视图**：`read`、`glob`、`grep`、`repo_map`、`edit`、`write` 通过一个视图接口访问文件。
  - 普通模式下视图直接对应磁盘，现有行为不变。
  - 覆盖层模式下：读按层次查找；写只进私有层；`glob` 合并基线和各层的条目，并去掉被删除的；`grep` 在磁盘上搜基线，排除各层改过的路径，这些路径改在内存里搜；`repo_map` 对层里改过的文件重新解析。
  - 工作区边界、受保护路径和审批规则与普通模式相同。

### 2. 子任务的 shell

- 默认只允许只读命令：权限分类为可自动批准、且不会改文件的那一类。命令里出现会写盘的东西（重定向、已知的写命令）一律拒绝，并提示"覆盖层子任务不能改磁盘文件，生成文件的步骤放到合并之后"。
- 只读命令在主工作区运行，看不到覆盖层里的改动。工具结果里注明这一点，并提示查看自己的改动要用文件工具。
- 编译型项目可以开启 **共享检查槽**（决策 1）：
  - 检查槽是数据目录里一份持久的目录，只复制 git 管理的文件；构建产物目录独立，第一次之后保持热缓存。
  - 子任务调用 `check` 动作时排队：把基线、依赖层和自己的层同步进检查槽，只跑编译检查（例如 `cargo check`、`dotnet build --no-restore`），返回结果。
  - 同一时刻只运行一个检查，避免抢锁。
  - 超时和卡住的判断用 0048 的规则。

### 3. 拆分与调度

- **规划**：在 `plan` 子 Agent 或主模型的 `todo` 上扩展字段：每个条目可写 `writes`（文件，或者 `文件::符号`，符号来自 `repo_map`）、`depends`、`estimate_minutes`。
- **粒度规则**（写进规划提示）：一个子任务是一处连贯的改动，大约一个函数到几个文件；太小的就合并。依赖很多、集中改同一个文件的，不拆。
- **调度**：
  - 写集合在文件级不重叠的，直接并行。
  - 同一文件、不同符号的，也可以并行，最后合并。
  - 同一个符号的，按依赖串行：后一个叠在前一个的层上。
  - 并发上限沿用 `max_agents`，审批仍然排队。
- **核对**：子任务结束时，对照层里实际改过的文件和申报的写集合。超出申报的，在合并报告里标出来，并重新检查和其他子任务的冲突。

### 4. 合并（`src/session/merge.rs`，新）

按依赖的拓扑顺序，把各层逐个合进主工作区。所有写入都通过 Workspace，作为本任务的一个检查点（`/diff`、`/undo` 可用）。对每个文件：

1. 主工作区当前内容的哈希等于基线，而且只有这一层改了它：直接写入。
2. 否则做三方合并，基线、主工作区当前内容、这一层各一方，主工作区里可能已有前面合进来的层和用户的改动：
   1. 先按锚点重放这一层的编辑意图：每个锚点在当前内容里仍然唯一，就依次应用。
   2. 重放失败，就用 `git merge-file`（经 `process::git::Git`，在临时文件上，`--diff3`）。
   3. 还有冲突，并且本机有 `mergiraf`，再试一次结构化合并（经 `process::argv`）。
   4. 仍有冲突：让这个子任务以主工作区的当前内容为基线，重做冲突那一处（决策 2）。重做也失败的，保留冲突，在合并报告里列出来交给主任务。
- 删除和改名：一方删除、另一方修改，算冲突。

### 5. 最后一次性验证

全部合并之后，按现有流程跑收尾验证（0047/0048）和审查（`review_commands`、`done_self_review`），失败进入正常的修复循环。不自动提交（决策 4）。

### 6. 触发：预估超过 5 分钟（决策 3）

- 模型做计划时，给每个条目写 `estimate_minutes`。
- cove 用本机历史校准：记录每次的"预估总时长"和"实际耗时"，按最近 N 次实际与预估之比的中位数修正。没有历史时，系数为 1。
- 校准后的总时长超过 `overlay_split_minutes`（默认 5），并且至少有两个条目可以并行（写集合不冲突、没有依赖关系）时，提议用覆盖层拆分。
- **确认界面**：列出子任务、各自的写集合和依赖，串行和并行的预计时长，预计费用。用户确认后执行；也可以改为普通方式，或者取消。
- 没有终端时（`-p`）：`auto` 权限模式直接用，其他模式退回原来的流程，并在结果里注明。
- 用户明确要求"拆开并行"时，不看预估，直接走这个流程。

### 7. 界面

- Agent 面板显示各子任务的状态和层里改过的文件数。
- 合并报告：每个文件是直接写入、锚点重放、行级合并、结构化合并，还是重做或冲突；超出申报的写入；最后的验证结果。

## 决策记录

- 2026-11-26（审查修复 F1）：菱形依赖时下游条目开始前，先把它的各个互不依赖、改了同一文件的上游层用合并引擎合成一个只读组合层，放在上游层之上、自己的层之下；合不成就让下游条目这一轮不跑（跳过，留在待办），合并完再 `execute_plan` 时在合并后的主工作区上跑，不改成依次叠层（叠层要求上游串行，等于放弃这两个上游的并行）。覆盖层条目不用文件认领（隔离靠层，冲突靠合并），`main` 不变。只有层完整合并进去的条目才算完成；计划在运行或合并中途停止时，部分报告照样作为工具结果记录，已完成但没合并的条目的层不随任务结束删除，留作恢复。合并新建的目录记进检查点，`/undo` 后为空的删除。细节见“审查修复（F1）”。
- 2026-11-26（审查修复 F2）：**行级合并排在锚点重放前面，重放结果要校验**。原顺序（重放 → 行级 → 结构化）下，重放不经检查就采用，两层做了同一处插入时会插两遍；改为：直接写入 → `git merge-file` → （行级冲突、没有 git 或编码混用时）校验过的锚点重放 → mergiraf → 冲突。校验标准：重放结果对当前内容的逐行改动，必须都落在当前内容和基线相同的行上（不碰别人改过的行，不在别人插入或删除的同一处插入），并且把这些改动搬回基线后正好得到这一层的结果；否则当冲突。下面"冲突处理顺序是锚点重放、行级三方合并、结构化合并"一条的顺序作废。
- 2026-11-26（审查修复 F2）：mergiraf 列语言失败或超时不再永久缓存为空列表，5 分钟内不重试，之后再试；成功的结果仍缓存到进程结束。review 记录的 id 取 `mergiraf review ` 后面的整行（形如 `base.<文件名>_<随机串>`，文件名可以有空格和中文），只要求是单个路径分量（不含 `/`、`\`、`:`，不是 `.`/`..`，不含控制字符）。
- 2026-11-26（D3 实施）：基线内容在层里第一次写入时保存，不从检查点快照取；没有终端时 `auto` 和 `bypass` 直接执行，其他模式退回 `main`；只合并完成的条目；冲突在该条目合并时立即重做一次，仍冲突的文件不写入，依赖它的条目的同一文件也不写入；合并结果为删除的不执行；检查槽同步 git 管理加未忽略的文件，编译失败不算失败的工具调用；mergiraf 的 review 记录按输出里的 id 事后删除。理由和细节见“实施记录：接入 `execute_plan`（D3）”的关键决定。
- 2026-11-26（更正）：**覆盖层和合并引擎照做**。单凭 skill 解决不了：同一个文件的单元只能排队，主工作区里并行时 shell 改的文件也约束不住。分工：skill（0054 内置为 `task-split`）负责分析和拆分，产出带 `writes`、`depends`、`estimate_minutes` 的计划；本文负责执行层：`execute_plan {isolation: "overlay"}` 用 `writes` 调度，覆盖层隔离，最后合并和一次验证。下面两条"先做 skill、暂不做代码"的记录作废。
- 2026-11-26：**先做 skill，再决定改不改代码**。skill 是 `~/.claude/skills/task-split/SKILL.md`，cove（会扫描 `~/.claude/skills`）、Claude Code 和 Copilot CLI（读 `.claude/skills/`、`~/.copilot/skills/`）都能用。原因：拆分方法是通用问题，在提示词层改起来最快；拆分方法验证好之前，先做执行引擎风险最大。
- 2026-11-26：**执行和合并也写进 skill**。只要并行单元的写集合互不重叠，它们就可以在同一个工作目录里跑（cove `execute_plan parallel=true`，有文件认领表兜底；Claude Code 子 Agent 不加 isolation；Copilot `/fleet`），不需要隔离和合并；要改同一个文件的单元排成先后依赖。只在用户想要分支、或者单元必须边跑边看时才用 worktree，合并用 `git merge-file`。因此本文的覆盖层和合并引擎先不做，cove 以后可能只需要几处小强化：`execute_plan` 核对单元实际写入是否超出申报、禁止单元跑会生成文件的命令、预估超过 5 分钟时自动提议这个 skill。
- 2026-11-26：不做历史任务回放验证，用日常真实任务验证 skill；skill 汇报里要求写出预估和实际的对照，用来校准。
- 2026-11-26：已停止的第一批实现（D1 覆盖层、D2 合并引擎）的部分代码留在 `D:/tmp/cove-d1`、`D:/tmp/cove-d2`，没有合入。以下"决策记录"中关于覆盖层的条目，在重新决定做代码改造之前不生效。
- 2026-11-26：新增第四种方式，现有三种不改。原因：覆盖层有限制（子任务不能跑完整测试），不适合所有任务。
- 2026-11-26：子任务里不跑完整测试，验证留到全部合并之后一次完成；编译型项目可以开共享检查槽，只编译不测试，排队使用。原因：长任务的时间大头在 shell，多份副本各自冷构建更慢，还会抢锁。
- 2026-11-26：冲突处理顺序是锚点重放、行级三方合并、结构化合并（本机有 mergiraf 时），最后让后一个子任务基于当前内容重做冲突那一处。原因：让知道意图的子任务重做，比让一个只看到冲突标记的合并步骤猜更稳。
- 2026-11-26：最后只跑构建、测试和审查，不自动提交。
- 2026-11-26：预估超过 5 分钟、且有可以并行的部分时提议使用；用户确认拆分计划后执行。阈值可以配置；用户明确要求时直接使用。

## 影响范围

| 文件 | 改动 |
|---|---|
| `src/session/overlay.rs`（新） | 覆盖层、叠加、意图记录、持久化 |
| `src/session/merge.rs`（新） | 锚点重放、`git merge-file`、mergiraf、冲突描述 |
| `src/tools/mod.rs`、`src/tools/search.rs` | 文件工具的读写视图；覆盖层模式下的 shell 限制 |
| `src/agents/plan.rs`、`src/agents/subagent.rs` | `isolation: "overlay"`、写集合调度、合并、重做 |
| `src/agents/overlay_plan.rs`、`src/agents/check_slot.rs`（新，D3） | 覆盖层计划的调度、确认、合并、重做、报告；共享检查槽 |
| `src/engine/agent.rs` | 预估和触发、确认、收尾验证衔接 |
| `src/engine/estimate.rs`（新） | 预估校准、历史记录 |
| `src/app/ui.rs`、`src/app/console.rs` | 确认界面、面板、合并报告 |
| `src/config/mod.rs` | `overlay_split_minutes`、`overlay_check`（检查槽开关和命令） |
| `docs/reference.md` | 新方式的说明、配置项 |

## 风险

- shell 绕过覆盖层：子任务里只允许只读命令，而且它们看不到层里的改动；写盘的命令一律拒绝。
- 行级合并没有冲突，不代表语义正确：最后的构建、测试和审查必须跑。
- 写集合申报不准：合并时按实际写入核对。
- 编译错误要到最后（或检查槽里）才暴露：靠最后的修复循环，或者开启检查槽。
- 预估不准：用历史校准，并且把预估值和实际值都记下来，事后可以对照。

## 验证

- 单元测试：
  - 覆盖层的读写、叠加、删除、glob/grep 的合并结果；
  - 锚点重放和三方合并：同一文件不同区域自动合并、同一处冲突、一方删除一方修改、用户中途改了主工作区；
  - 申报外写入的检测；预估校准。
- 集成测试（假 provider）：
  - 三个子任务里有两个改同一个文件的不同函数，并行执行，合并后文件正确，验证只跑一次；
  - 改到同一处时触发重做；
  - 子任务里的写盘 shell 被拒绝；
  - 预估超过阈值时出现确认，确认后走覆盖层，取消后走原流程；
  - 现有三种方式的测试照常通过。
- 用 release 版在一个真实仓库里跑一次需要改 3 个以上文件的任务，对比普通方式的耗时。
- `cargo test --release` 全量通过；`cargo clippy --release --all-targets` 不新增警告。

## 实施记录：覆盖层与视图（D1）

范围：方案 1（覆盖层工作区、文件工具的读写视图）和方案 2 里子任务的 shell 限制，不含检查槽、调度、合并、预估和界面。

### 接口

- `src/session/overlay.rs`（新）：
  - `content_hash(&[u8]) -> String`：SHA-256 十六进制小写。工作区检查点存的是整份内容，快照用的 git blob id 要调 git 才能算；仓库里其他内容哈希都用 SHA-256，这里保持一致。合并引擎比较基线时用同一个函数。
  - `Overlay::open(dir) -> Result<Overlay>`：打开或新建一个层，目录由调用方给（`<数据目录>/overlays/<任务>/<子任务>/`）。磁盘上是 `overlay.json`（条目和意图）加 `blobs/<sha256>`（内容，先写 blob 再写清单，都经 `persist::write_atomic` / `atomic_json`）；恢复时校验 blob 的哈希，坏了报错。
  - 层的查询：`get(path) -> Option<Content>`、`changes() -> Vec<Change>`（`path`、`content`、`base_hash`：这个层第一次写这个路径之前的哈希，`None` 表示原来不存在）、`changed_paths()`、`intents() -> Vec<Intent>`（按发生顺序）、`is_empty()`、`dir()`。
  - `Intent::{Edit, Write, Delete}`，字段同任务要求；`path()` 取路径。每条意图的 `base_hash` 是这次改动之前子任务看到的内容（下层或主工作区）的哈希，所以同一个文件连续两次 `edit`，第二条的 `base_hash` 是第一次改完的内容。`Edit` 的 `old`/`new` 原样保存模型给的文本。
  - `OverlayStack::new(root, layers: Vec<Arc<Overlay>>) -> Result<OverlayStack>`：`layers` 从下到上，最后一个可写（B 依赖 A：`[a, b]`）。提供 `root()`、`layers()`、`top()`、`lookup()`、`read()`、`stat() -> Kind`、`list()`、`write(path, expected, data, Option<EditSpec>)`、`delete(path, expected)`、`changed_paths()`（最上层）、`shadowed()`（所有层改过或删过的路径）、`files()`（各层当前的文件，删除的不算）、`merge_entries(base)`（基线条目 + 各层新增 − 各层删除）。写入在一把锁里先比对 `expected` 再落层，和磁盘上 `replace_file_bytes` 的"文件在替换前被改了"语义一样；大小限制沿用 `workspace::within_limit`。
- `src/tools/view.rs`（新）：`FileView::{Disk, Overlay(Arc<OverlayStack>)}`，`read`、`write`、`edit`、`glob`、`grep`、`repo_map` 和目录列表都经它访问文件。
- `src/tools/mod.rs`：
  - `LocalTools::with_overlay(self, stack: Arc<OverlayStack>) -> Result<LocalTools>`：给子 Agent 造覆盖层工具。去掉 Workspace（写入不进检查点），换新的读记录和后台命令表；`stack.root()` 必须是同一个工作区，否则报错。`LocalTools::overlay() -> Option<&Arc<OverlayStack>>`。
  - `replay_edit(existing, shown, old, new, replace_all) -> Result<Vec<u8>, String>`：按 `edit` 工具完全相同的规则（LF 视图匹配、空白容忍匹配、保留 CRLF/BOM/GBK）把一条 `Edit` 意图应用到给定内容上，供合并时锚点重放。`edit` 工具自己也走同一个函数。
  - `OVERLAY_SHELL_REFUSED`：shell 被拒绝时结果的开头。
- `src/tools/search.rs`：`Shadow`（各层改过的路径 + 各层的文件内容），`glob`、`builtin_grep` 多一个 `Option<&Shadow>` 参数，`grep_layers` 在内存里搜层里的文件，`ripgrep_search` 多一个排除列表参数，`ignored(root, path)` 判断一个不一定在磁盘上的文件会不会被忽略规则排除。
- `src/tools/repomap.rs`：`query_with(files, contents, ...)`，`contents` 里的文件按给定内容解析，不读磁盘、不进缓存。

### 关键决定

- 视图用枚举而不是 trait：只有两种，`LocalTools` 要 `Clone`，枚举最简单。普通模式下每个分支都是原来的磁盘调用，`edit` 的核心逻辑只是挪进 `edited()`，参数和结果格式不变。
- 路径边界照旧走 `resolve`：先按磁盘解析；覆盖层模式下磁盘上找不到时，按"新文件"规则再解析一次（最深的已存在祖先必须在工作区内），并要求层里确实有这个文件。受保护路径、`.git`、`..`、备用数据流的检查都在同一处，覆盖层不另开口子。
- 层的键用 `paths::key`（`/` 分隔，Windows 上不区分大小写），和认领表一致。
- `glob`/`grep`/`repo_map`：磁盘遍历跳过各层改过或删过的路径，层里的文件排在磁盘结果之后，同样受 `.gitignore`/`.ignore`/`.git/info/exclude` 约束（`include_ignored` 时不受）。全局排除文件不读，和 `ignore_rules_apply` 的取舍一样。
- rg 路径：把各层改过的路径作为 `--glob=!/<路径>` 排除（锚定到工作区根，通配符转成字符类），再在内存里搜层里的文件。超过 200 个改动路径时命令行放不下，改用内置搜索。
- `read` 对层里改过的文件不走图片识别（图片识别只读磁盘）。
- shell：只放行 `auto_approvable` 且分类为只读、不是格式化工具、不删文件的命令；构建类（`cargo build`）也拒绝，因为它在主工作区跑，看不到层里的改动，还会抢构建锁。拒绝结果以 "overlay sub-tasks cannot change files on disk; put steps that generate files after the merge" 开头；放行的命令结果末尾注明它在主工作区运行、看不到覆盖层改动，查看自己的改动用 read/grep/glob。`run_in_background` 和 `shell_task` 拒绝，工具定义里也去掉。拒绝发生在审批之前，不会弹确认。
- 写入用的读记录（"覆盖前要先读过"）和磁盘模式一样生效，记录的是层里的内容。

### 测试

- `session::overlay::tests`（6 个）：写入只进最上层、读穿透到磁盘、过期的 `expected` 被拒；删除和目录列表合并；依赖层叠加和 `base_hash`；从目录恢复（含 blob 损坏报错）；越界和 `.git` 路径拒绝；4 线程并发改同一文件不丢改动。
- `tests/all/overlay.rs`（6 个）：`write`/`edit`/`read`/目录列表在覆盖层里的结果、CRLF 保留、意图记录、`replay_edit` 与工具结果一致；依赖层；`glob`（含忽略规则）、`grep`（内置和 rg 两条路径）、`repo_map` 看到层里的新增、修改和删除，普通工具仍只看磁盘；工作区边界、`.git`、受保护目录、别的工作区的层；shell 限制（重定向、格式化、构建、删除、`npm install`、后台）和放行命令的说明；rg 排除只排掉改过的那个路径（同名的子目录文件、带 `[]` 的文件名）。
- 本次实测（独立副本）：`cargo test --release --no-fail-fast` 第一次全部通过（单元 551、集成 252）；格式化后再跑一次，有 5 个已知偶发用例失败（snapshot 的 auto_deletes、cli 的 hook 事件、MCP 取消、shell 取消、safety 的 shell 进度），用 `--test-threads=1` 单独重跑全部通过。`cargo clippy --release --all-targets` 只有原有 5 条。

## 实施记录：合并引擎（D2）

新模块 `src/session/merge.rs`，只做单个文件的合并，不碰磁盘上的工作区；写回、检查点、合并报告和重做由 `execute_plan` 负责。为了复用 `edit` 工具的匹配函数，`src/tools/mod.rs` 里 `mod edit` 改成 `pub(crate) mod edit`，此外没有改别的源文件，没有加依赖。

### 接口

```rust
pub struct EditIntent { pub old: String, pub new: String, pub replace_all: bool }
pub enum Strategy { Direct, Replayed, LineMerged, Structured }
pub struct Conflict { pub reason: String, pub marked: Option<Vec<u8>>, pub regions: Vec<(usize, usize)> }
pub enum FileMerge {
    Merged { content: Vec<u8>, strategy: Strategy },
    Deleted { strategy: Strategy },   // 新增：合并结果是文件不存在
    Conflict(Conflict),
}
pub async fn merge_file(path, base, current, ours, intents, cancel) -> Result<FileMerge>;
```

和设计稿相比只加了 `FileMerge::Deleted`：`Merged` 只能带内容，表达不了"双方都删了"或"一方删、另一方没动"。`Err` 只在取消（`Error::Cancelled`）时出现，外部工具出错都降级成下一级或冲突。

### 各级的实际行为

1. **直接**（`Direct`）：`current == base` 取 `ours`；`ours == base` 或 `ours == current` 取 `current`。比较的是 `Option<&[u8]>`，所以"一方删除、另一方没动"和"只有一方新建"也在这里成立，结果为 `None` 时返回 `Deleted`。
2. **删除和新建**：三方各不相同时，`current` 为 `None`（主工作区已删、子任务改了）、`ours` 为 `None`（子任务删了、主工作区改了）、`base` 为 `None`（双方新建同一路径且内容不同）都直接冲突，不做后面几级。
3. **二进制**：任一方含 NUL 或是 UTF-16（`text_encoding::sniff`），冲突。
4. **锚点重放**（`Replayed`）：和 `edit` 工具同一套步骤：`decode_editable`（UTF-8 保留 BOM，GBK/GB18030 解码），`old`/`new` 转成 LF，在去 `\r` 的视图上用 `edit::apply`（唯一匹配、`replace_all`、忽略空白差异的退路），再用 `line_endings::restore` 按行还原换行；只有 `\r` 不同的编辑按字节匹配；最后按原编码写回。先在 `base` 上重放一遍，结果必须等于 `ours`，否则说明编辑记录不完整（子任务还整体写过文件等），跳过这一级；通过后再在 `current` 上重放，任一条失败就进入下一级，失败原因记进 `reason`。
5. **编码检查**：一方是含非 ASCII 的 UTF-8、另一方不是 UTF-8（GBK），逐行合并会在一个文件里混用编码，直接冲突。纯 ASCII 算两种都兼容。
6. **行级合并**（`LineMerged`）：在临时目录里写 `current`、`base`、`subtask` 三个文件，`Git::new(临时目录)` 跑 `git merge-file -p --diff3 -L current -L base -L subtask current base subtask`，参数用相对文件名，避开 Windows 路径问题。超时 30 秒，输出上限为三方大小之和加 64 KB。退出码 0 为无冲突，1–127 为冲突数，其他（git 的负数退出码在 Windows 上表现为 255 等）、超时、输出超限、启动失败都当成"git 不可用"，跳过这一级并在 `reason` 里注明。git 按字节合并，CRLF、BOM、GBK 原样保留；冲突标记的标签用 ASCII，放进任何编码的文件都不会坏；CRLF 文件里 git 会让标记行也用 CRLF。
7. **结构化合并**（`Structured`）：`which::which("mergiraf")` 找得到、三方都是 UTF-8、文件名匹配 `mergiraf languages --gitattributes` 列出的模式（结果在进程内缓存）时，经 `process::argv`（受控进程树，30 秒超时）跑 `mergiraf merge base.<名> current.<名> subtask.<名> -o merged.<名>`，临时文件保留原文件名，让 mergiraf 按扩展名识别语言。退出码 0 且结果里没有冲突标记才算成功。本机没有 mergiraf，这一级只测了模式匹配。
8. **冲突**：`marked` 是 git 带 diff3 标记的输出（git 不可用时为 `None`）；`regions` 由 `similar` 分别算 base→current、base→ours 的改动块，两边在基线上重叠或相邻的块（和 git 判定冲突的口径一致）取 current 一侧的行号，相邻的合并；删除记在删除发生的那一行。`reason` 形如"src/a.rs：行级合并有 1 处冲突：第 6 行（基线第 6-6 行）主工作区改为「200」，子任务改为「22」。锚点重放失败（…）"，最多列 5 处，GBK 内容先解码再显示。

### 关键决定

- 重放前先在 `base` 上验证编辑记录能还原 `ours`：重放成功时结果不看 `ours`，记录不全会悄悄丢掉子任务的改动。
- 重放成功不代表语义正确：锚点仍唯一时，用户在附近的改动不会被发现；`replace_all` 会连带替换主工作区里新出现的同样文本。这和 `edit` 工具本身的语义一致，靠最后一次性验证兜底。
- 外部工具的失败一律降级，不报错：合并引擎拿不准时给冲突，由上层决定重做或交给主任务。

### 测试结果

`session::merge::tests` 共 16 个：直接写入各情况；仅重放成功（用户另改了别的函数）；锚点不唯一时退到行级合并；编辑记录还原不出 `ours` 时跳过重放；同一行冲突（标记、行号、双方内容、重放失败原因）；CRLF 的重放和行级合并；UTF-8 BOM；GBK 的重放、行级合并和冲突描述（字节完全等于 GBK 编码的期望值）；编码混用；删除/新建冲突；二进制；没有 git 时重放仍可用、其余冲突；公开入口在没有 mergiraf 的机器上可用；取消；mergiraf 语言模式匹配；冲突区域的计算。全部通过。全量 `cargo test --release --no-fail-fast`：库 561 个通过（7 个忽略），集成测试 246 个通过，无失败；`cargo clippy --release --all-targets` 只有原有 5 条警告。

## 实施记录：接入 `execute_plan`（D3）

范围：方案 2 的共享检查槽、方案 3 的调度、方案 4 的写回和重做、方案 6 的确认、方案 7 的合并报告，以及配置、技能和文档。

### 接口和改动

- `execute_plan` 新参数 `isolation`：`"main"`（默认，原有行为不变）或 `"overlay"`。`overlay` 要求每个条目都申报了 `writes`，否则拒绝并列出缺的条目。
- `src/agents/writes.rs`：`WriteSet::symbols_for(file)`（这个集合在某个文件里写的 `::符号`，没写符号或 glob 覆盖时为 `None`）；`implicit_depends(units, root) -> Vec<Implicit>`：互相没有依赖路径、写集合有交集的两个条目，交集里每个文件双方都写了不同的符号时照样并行，否则后一个（计划顺序）依赖前一个；前面加的边参与后面的判断，只在没有路径的两点之间加边，所以不会成环。
- `src/agents/overlay_plan.rs`（新）：`prepare`（调度）、`plan_text`/`choice`/`decide`（确认）、`execute`（建层、运行、合并、重做、报告）、`OverlayCleanup`（任务结束时删掉保留的目录）。
- `src/agents/check_slot.rs`（新）：`CheckSlot`（同步和运行）、`CheckRun`（一个条目的 `check` 工具）、`definition`。
- `src/agents/plan.rs`：`PlanTask.overlay`，条目提示词里加 `[overlay]` 一段（有检查槽时说明 `check`），条目结束后在 Agent 面板写“层里改过的文件数”，`PlanReport.records`（各条目的 `ItemScope::record`），结果收尾抽成 `finish`。
- `src/agents/subagent.rs`：`ToolScope.overlay`、`RunRequest.overlay`、`ScopedTools.check`/`overlay`（覆盖层条目的 shell 只读，不再前后比较 `git status`）；`AgentHub.input`（终端）、`check_slot`、`overlay_dirs`；`SubAgentSettings.overlay_check`、`mergiraf`。`src/commands/agents.rs` 的工具工厂在 `ToolScope.overlay` 有值时调用 `LocalTools::with_overlay`，并读取两项新配置。
- `src/session/overlay.rs`：`Overlay::base(path)`；`record` 在第一次写某个路径时把写之前的内容存成 blob（文件名就是 `base_hash`）。
- `src/session/merge.rs`：`merge_file_using(mergiraf, ...)`，`merge_file` 改为用 PATH 里的 mergiraf 调用它；mergiraf 运行后删除它留下的 review 记录。
- 配置：`overlay_check`、`mergiraf_path`（`src/config/mod.rs`、`src/config/trust.rs`）。
- `src/engine/estimate.rs` 的拆分提示改为推荐 `execute_plan parallel=true isolation="overlay"`；内置技能 `task-split` 第六步的 cove 一段改为优先用覆盖层。

### 关键决定

- **基线内容在第一次写入时保存**，不从检查点快照取。快照是私有 git 仓库里按 git blob id 存的整棵树，要调 git 才能按内容取；依赖层的条目，它的基线是下层的内容，快照里根本没有。层里本来就按 SHA-256 存 blob，`base_hash` 直接就是 blob 名，只多存改过的文件。
- **调度**：同一个文件只有双方都写了 `::符号`、而且符号互不相同才并行；同一个符号、没写符号、glob（包括字面相同的 glob）都叠层。叠层的方向按计划顺序（后一个依赖前一个），在确认界面和结果里都列出来。
- **叠层的读视图**：一个条目的层栈是它所有直接和间接依赖的层（按依赖顺序）加自己的层。两个并行的依赖都改了同一个文件时，它读到的是依赖顺序里后一个的版本，看不到前一个的改动；合并时它的基线就是它读到的那个版本，三方合并不会丢掉前一个的改动。没有为这种情况先合并一份视图，写进遗留。（已由 F1 改为组合层，见“审查修复（F1）”。）
- **只合并完成的条目**；失败、跳过、取消的条目的层不合并，保留到任务结束。原因：主工作区方式下失败条目的部分改动留在工作区里，要靠 `/diff` 自己检查；覆盖层能做到不写，就不写。
- **冲突在这个条目合并时立即重做**，在后面的条目合进来之前。重做的层只叠在主工作区上（基线就是合并到这一步的内容），写集合只含冲突的文件，`id` 是 `<条目>-redo`。重做失败、没改这个文件、或者合并时仍冲突，这个文件都不写入；这个条目不算完成（结果里写明，留在待办里）。依赖它、也改了同一个文件的后续条目，这个文件也不写入，避免在没合进来的改动上再叠一层。
- **合并结果是删除的，不执行**，按冲突处理：`Workspace` 只有替换接口，删除没法进检查点、`/undo` 也恢复不了；覆盖层的文件工具本来也不删文件，实际只有用户在中途删了文件时才会遇到。
- **新文件的父目录**在写入前创建（和 `write` 工具相同），目录本身不进检查点。（F1 起记进检查点，`/undo` 后为空的删除。）
- **确认**：有终端时一律询问，回答 `1`/`y`/`确认` 执行，`2`/`main` 改为普通方式，其他（包括直接回车）取消，取消时什么都不运行。没有终端时，`auto` 直接执行；`bypass` 也直接执行（它允许 `auto` 允许的一切，没理由比 `auto` 更严）；`default`、`plan` 退回 `isolation="main"`，结果开头注明原因。权限模式取 cove 启动时的模式。
- **预估和费用**：串行 = 各条目预估之和 × 校准系数；并行 = max（依赖图上的最长路径，串行 ÷ 并发数）；额外 token 按每个子 Agent 1 万（技能里的经验值），有单价时折算成输入费用。
- **共享检查槽**：
  - 同步的文件是 `git ls-files --cached --others --exclude-standard`：git 管理的文件加上没被忽略的新文件（用户新建还没 `git add` 的源文件，构建同样需要）；不是 git 仓库时按忽略规则遍历。槽里有一份清单（路径 → 内容哈希、来源文件的大小和修改时间），磁盘文件没变的不再读，内容没变的不再写，不在列表里的删掉；命令自己生成的文件不在清单里，不会被删，构建产物因此保持热缓存。
  - 排队：进程内一把异步锁，加上 `persist::try_lock_async` 的文件锁，几个 cove 窗口之间也排队。
  - 运行：经条目的审批（`Effect::Shell`，和收尾验证一样），`process::shell_live` + `Limit::watched(WATCHED_TOTAL, IDLE_LIMIT)`。
  - 编译失败不算失败的工具调用：否则条目改好之后也会因为“有工具失败”被判失败并重试。结果里写明退出码，失败时提示修完再检查。
- **mergiraf 的 review 记录**：按 mergiraf 源码里的 `AttemptsCache`（未实测：本机 `D:/software/` 下没有 `mergiraf.exe`，PATH 里也没有，`%LOCALAPPDATA%`、`~/.cache` 下也没有它留下的目录），它把三方内容和结果存进缓存目录 `merges/<id>/`（`directories` crate 的项目缓存目录，Linux 为 `$XDG_CACHE_HOME/mergiraf` 或 `~/.cache/mergiraf`，macOS 为 `~/Library/Caches/mergiraf`，Windows 为 `%LOCALAPPDATA%\mergiraf\cache`；另外两种可能的写法也列为候选），超过上限才淘汰旧的，并打印 `Review with: mergiraf review <id>`；没找到只关掉这份记录的选项或环境变量（`MERGIRAF_DISABLE` 是整个停用 mergiraf）。做法：从输出里读出 id（只接受字母、数字、`_`、`-`、`.`，不接受 `..` 和路径分隔符），在上述候选目录里删掉 `<id>`；Linux 上另外把 `XDG_CACHE_HOME` 指到这次合并的临时目录，记录随临时目录一起删除。Windows 的 Known Folder 不读环境变量，只能事后删。
- **`mergiraf_path`、`overlay_check` 需要信任**：前者是要运行的程序，后者是要运行的命令；`overlay_check` 设为空字符串（关闭）无需信任。`mergiraf_path` 必须是绝对路径。
- **合并报告给模型的部分用英文**（和其他工具结果一致），界面上的一行摘要用中文，经 `Progress::notice` 显示（交互界面和 `-p` 都显示）。
- **保留与清理**：全部合并成功时立即删除这个计划的目录；有失败或冲突时保留，冲突副本在 `<计划>/conflicts/<条目>/<路径>`，结果里写明位置，主任务在同一个任务里处理；任务结束时（`TurnHook::task_end`）删除。成功合并的条目即使计划里还有别的问题，它自己的层也立即删除。

## 验证结果

- 新增单元测试（全部通过）：
  - `agents::writes::tests::the_overlay_schedule_runs_distinct_symbols_side_by_side_and_stacks_the_rest`：不同符号并行；同一符号、没写符号的叠层，前面加的边参与后面的判断；已有依赖时不加边；字面相同的 glob 叠层；`symbols_for` 的各种写法。
  - `agents::overlay_plan::tests`（3 个）：隐式依赖、计划外的依赖视为已满足、拓扑顺序和祖先、串行/并行预估（关键路径与并发上限）、确认文字（隐式依赖、预估、token 和费用）；确认回答的三种结果；重做提示词带冲突附近的行（编号、上下各 3 行）和原来的编辑。
  - `session::overlay::tests::the_first_write_of_a_path_keeps_a_copy_of_its_base`：基线是第一次写之前的内容、新文件为 `None`、没改的文件没有副本、只多一个 blob、依赖层的基线是下层内容、恢复后可读、副本损坏时报错。
  - `agents::check_slot::tests::a_sync_copies_sources_and_layers_then_only_what_changed`：第一次全量复制、第二次不重写、层里的修改/新增/删除同步进槽而主工作区不变、不在列表里的文件删掉、构建产物保留、`..` 路径被忽略。
  - `session::merge::tests::the_review_record_id_is_read_from_mergiraf_output`；`config::trust` 里两项新配置的信任分类；`engine::estimate` 的提示文字。
- 新增集成测试（假 provider，`tests/all/agents.rs`，全部通过）：
  - `an_overlay_plan_runs_items_on_one_file_in_parallel_and_merges_them_at_the_end`：两个条目改 `src/lib.rs` 的不同函数，按请求时间确认同时运行；第三个条目依赖第一个，它的只读 shell（`git status`）看到主工作区没变，`read` 看到第一个条目的层；条目里 `cargo build` 和 `echo x > gen.txt` 被拒；合并后文件正确，报告依次是 `direct write`、`anchor replay`、新文件 `direct write`，`Implicit dependencies: none.`，层目录已删除；主任务在合并之后才跑 `git diff --stat`，看到合并结果；界面摘要 `[合并] 覆盖层合并进工作区：写入 3 个文件`。
  - `an_overlay_conflict_is_redone_once_and_what_still_conflicts_is_left_out`：两个条目申报不同符号、实际改同一行；第二个条目重做（提示词带冲突、原目标、写集合只含冲突文件），重做后 `merged after redo`；重做不改文件时冲突未解决、文件保持第一个条目的结果、报告给出 `conflicts/` 下的副本位置、条目留在待办；任务结束后覆盖层目录被清掉。
  - `overlapping_overlay_items_are_stacked_instead_of_rejected`：一个写 `src/lib.rs::one`、一个写整个 `src/lib.rs`，计划不被拒绝，自动排成先后，后一个读到前一个的改动，报告列出隐式依赖，合并结果正确。
  - `an_overlay_plan_needs_writes_and_falls_back_to_main_without_a_confirmation`：缺 `writes` 时拒绝、没有条目运行；`-p` + `default` 权限时结果注明原因、按 `main` 执行、文件直接写入。
  - `overlay_items_share_one_check_slot_in_turn_and_see_their_own_layer`：配置 `overlay_check` 为一个 Python 假检查（记录开始/结束、打印 `src/lib.rs`、睡 0.8 秒）；两个并行条目各调用一次 `check`，日志是 `start end start end`（没有重叠），各自看到自己的改动、看不到对方的，退出码 0；检查槽目录保留。
  - 原有 execute_plan、0054 写集合、task-split 的测试不变并通过（`isolation` 不传时行为不变）。
- `cargo test --release --no-fail-fast` 全量：lib 585 通过、2 失败、7 忽略；集成测试 257 通过、4 失败。失败的是 `agents::overlay_plan::tests::the_redo_prompt_shows_the_conflicting_lines_and_the_earlier_edits`（摘录用了会压成一行的 `text::short`，改成 `text::clip` 后通过）和已知偶发的 `session::snapshot::tests::auto_deletes_only_files_the_snapshot_before_the_call_holds`、`cli::all_hook_events_run_with_scrubbed_environment_and_session_end_once`、`integrations::cancelled_and_timed_out_mcp_operations_kill_owned_descendants`、`core_tests::cancelled_shell_kills_its_spawned_child_before_late_writes`、`safety::files_changed_by_shell_are_progress_and_unchanged_ones_are_not`；修复后相关的 43 个单元测试和这 5 个偶发用例单独重跑（集成的 4 个用 `--test-threads=1`）全部通过，覆盖层和计划相关的 23 个集成测试再跑一次全部通过。
- `cargo clippy --release --all-targets`：只有原有 5 条（`src/app/console.rs` 2 条、`src/process.rs`、`src/safety/permission/policy.rs`、`tests/all/cli.rs`）。
- 只对改过的文件跑 `rustfmt --edition 2024 --config max_width=100`，采用的差异都落在本次改动的行里；两个新文件整体格式化。
- 没有实测：mergiraf（本机没有）、终端里的确认界面（只测了文字和回答解析）、用 release 版在真实仓库里跑一次对比耗时。

## 审查修复（F2：合并引擎）

### 问题

1. **锚点重放把同一处改动插两遍**：两个并行单元都在 `use a;` 后加了 `use x;`，各自又改了别的函数。第一层直接写入后，第二层的意图 `old="use a;" new="use a;\nuse x;"` 在当前内容上仍然唯一匹配，重放得到两行 `use x;`（Rust E0252）。原因是重放排在 `git merge-file` 之前，而且结果不做检查；git 本来会把两边相同的改动当作一次干净合并。同类问题：`replace_all` 会改到别的层新加的同名文本；忽略空白的模糊匹配会盖掉对方只改了空白的部分。
2. mergiraf 的 review 记录 id 是 `base.<文件名>_<随机串>`，原来只接受 ASCII 字母数字，文件名有中文或空格时记录不清理（本机 mergiraf 0.20.0 实测输出 `Review with: mergiraf review base.中 文.cs_3i5IRyPc`）。
3. `MERGIRAF_PATTERNS` 第一次列语言失败或超时就缓存空列表，整个进程都不再用 mergiraf。

### 修复（`src/session/merge.rs`）

- 顺序改为：直接写入 → `git merge-file` 行级合并（干净就采用）→ 校验过的锚点重放（只在行级合并有冲突、没有 git 或三方编码混用时尝试）→ mergiraf → 冲突。编码混用时仍不做逐行合并，重放校验不过就直接按冲突返回（和原来一样不走 mergiraf）。
- `replay_checked`：先确认意图在基线上重放得到这一层的结果（原有检查），再在当前内容上重放，然后用 `transplant` 校验：逐行比较"当前 → 重放结果"，每个改动块都必须落在当前内容里和基线相同的行上（按"基线 → 当前"的行级 diff 映射回基线位置）——修改或删除的行必须是基线原样保留的、且在基线里连续；纯插入的位置不能在别人插入或删除的地方。全部满足后把这些改动块搬回基线，结果必须正好等于这一层的内容。任何一条不满足，重放不采用，原因里写"锚点重放的结果和子任务自己的改动不一致"。相邻但不重叠的改动允许（这正是重放比 git 强的地方：git 对相邻行的改动报冲突）。
- `review_id` 取 `mergiraf review ` 后面的整行，去掉首尾空白和引号，只要求是单个路径分量。
- 列语言失败或超时时记下时间，5 分钟内直接当作不支持，之后再试；成功的结果照旧缓存。

### 测试

- `the_same_insertion_by_two_layers_is_not_replayed_twice`：审查者的场景；有 git 时行级合并干净、只有一行 `use x;`，没有 git 时重放校验不过、按冲突返回。
- `replace_all_does_not_reach_text_another_layer_added`：别的层在紧挨着的下一行加了 `foo(3);`，git 冲突，`replace_all` 的重放会把它也改成 `bar(3);`，校验拒绝，按冲突返回。
- `a_fuzzy_match_does_not_cover_the_other_sides_whitespace_change`：当前内容只把一行的缩进改成了 Tab，这一层的意图靠忽略空白匹配到这一行；有没有 git 都按冲突返回。
- `replay_settles_adjacent_changes_the_line_merge_conflicts_on`：两边改相邻的两行，git 冲突，重放通过校验，结果 `Replayed`。原有的 `replay_applies_the_layers_edits_over_the_users_changes`、`without_git_replay_still_works…`、CRLF、BOM、GBK 的重放用例照旧通过。
- `transplant_moves_changes_onto_the_base_and_refuses_shared_points`：搬回基线、在别人插入处插入、改别人改过的行、跨过别人删除的行。
- `the_review_record_id_is_read_from_mergiraf_output` 加了中文和空格文件名；`mergiraf_merges_members_added_at_the_same_place`（本机有 mergiraf 时运行）加了 `src/报价 Quote.cs`，确认 review 记录被删掉。
- 集成测试 `an_overlay_plan_runs_items_on_one_file_in_parallel_and_merges_them_at_the_end` 里两个条目改同一文件里不相邻的两个函数，现在由行级合并完成，报告从 `anchor replay` 改为 `line merge`。

### 第二轮复审

- **问题**：校验放过"重复插入 + 紧邻修改"。base `use a;\nuse b;`，current 在 `use a;` 后插入 `use x;`，这一层也插入 `use x;` 并把 `use b;` 改成 `use bb;`。git 冲突后走重放，Myers 把重复的 `use x;` 和下一行的修改合成一个 `Replace` 块；原来非空块只检查行在基线里连续，没检查块边界是不是别人插入或删除的地方，结果 `Replayed`、`use x;` 出现两次。
- **修复**：一个改变行数的块（`Replace` 前后行数不同、`Delete`，以及原来就检查的纯插入）只要边界碰到 current 插入或删除的位置，就不采用。另外校验改成两个方向都要成立：把"current → 重放结果"的改动搬回基线必须得到这一层的结果，把"这一层 → 重放结果"的改动搬回基线也必须得到 current。等长的修改紧挨着别人的插入仍然允许。
- **测试**：`a_repeated_insertion_next_to_a_change_is_not_replayed`（上面的场景；有没有 git、以及公开入口 `merge_file_using` 都不会出现两行 `use x;`，公开入口按冲突返回）。`accepted_replays_never_repeat_or_lose_a_change` 是穷举测试：三行，每行取保留、修改、删除、在后面插入（两边插入的是同一行）四种之一，两边组合共 4096 种，意图按这一层的操作生成。对每个被接受的重放，断言没有重复行，两边的修改、插入都在，被任一方删除或修改的原行不在。4096 种里最多 729 种的锚点在 current 里都还在，写测试时有 405 种通过；耗时约 0.1 秒。

### 验证结果

- 第二轮复审后再跑一次全量：lib 600 通过、0 失败、7 忽略；集成测试 261 通过、0 失败；clippy 仍只有原有 5 条；mergiraf 记录目录无残留。
- `PATH` 里带 mergiraf 跑 `cargo test --release --no-fail-fast` 全量：lib 597 通过、0 失败、7 忽略；集成测试 261 通过、0 失败（这次已知的偶发用例也都通过）。跑完 mergiraf 的 `merges/` 目录没有新增记录。
- `cargo clippy --release --all-targets`：只有原有 5 条（`src/app/console.rs` 2 条、`src/process.rs`、`src/safety/permission/policy.rs`、`tests/all/cli.rs`）。
- 只对改过的文件跑 `rustfmt --edition 2024 --config max_width=100`，只采用落在改动行里的差异（`classify.rs` 里原有的未格式化数组没动）。

### 遗留

- 子 Agent 的覆盖层提示词（`src/agents/subagent.rs`，另一个修复分支在改）里列合并方式的顺序仍写着 "direct, anchor replay, line merge, structural merge"，只是说明文字，不影响行为，下次改那个文件时顺手改掉。

## 遗留

- 用 release 版在真实仓库里跑一次改 3 个以上文件的任务，对比普通方式的耗时（方案“验证”最后一条）没有做；预估校准要攒够记录。
- mergiraf 的 review 记录清理按源码推断的目录实现，没有在装了 mergiraf 的机器上验证；本机 `D:/software/mergiraf.exe` 不存在。
- 合并结果是删除文件的不执行（检查点不支持删除），按冲突列出。
- 合并写入受检查点上限约束（每个任务 100 个文件、UTF-8 1 MiB）；超出时该文件按未解决列出。
- 确认界面用的是现有的文本问答（`UserInput::answer`），不能在界面里逐条修改计划；要改计划时取消后让模型改 `todo`。没有终端时按启动时的权限模式判断，任务中途 `/mode` 切换不影响这个判断。
- 检查槽只同步工作区内 git 管理和未忽略的文件：构建需要的被忽略文件（本地配置、`.env`）不在槽里，需要的话加进 git 或改 `overlay_check` 命令；工作区是仓库子目录时只同步子目录。
- 自动“预估超过阈值时出现确认”仍然是先给模型提示（0054），确认发生在模型调用 `execute_plan isolation="overlay"` 时。

## 审查修复（F1）

独立审查在这个实现上找到的问题，逐条修复。复现程序在 `D:/tmp/review-r1/probe/src/bin/`。

### 1. ripgrep 带 `include` 时返回磁盘旧内容和已删除文件（高）

- 现象：层里改了 `a.rs`、删了 `gone.rs`，`grep {pattern, include:"*.rs"}` 在 `ripgrep=true` 时输出 `gone.rs:1:needle gone`、`a.rs:1:needle disk`。原因：rg 的 `--glob` 后出现的优先，排除层里路径的 `--glob=!/path` 写在 `--glob={include}` 之前，被 include 重新放回。受保护目录的排除也有同样的问题。
- 修复：`search::ripgrep_search` 把 `--glob={include}` 放在所有排除之前（受保护目录和层里的路径都在它之后）。
- 测试：`overlay::grep_include_does_not_bring_back_changed_or_deleted_disk_files`，内置搜索和 rg 两种，覆盖单个 include、`{*.rs,*.toml}`、`path` + include、精确文件名 include、只匹配已删除文件的 include。

### 2. 合并途中出错或取消时报告丢失、条目误记为完成、层被删（高）

- 现象：重做阶段 Esc 或 mergiraf 运行中 Esc 时，`report.done` 只去掉了冲突未解决的条目，中途 break 后没合并的条目仍记为完成，后续 `execute_plan` 把它们当已完成的依赖；`finish` 先记完成再 `return Err`，合并报告丢失；`OverlayCleanup` 在任务结束时删掉计划目录，已完成但没合并的工作也没了。
- 修复：
  - `overlay_plan::execute` 只把层完整合并进去的条目放进 `done`（`merged`），其余留在待办里。
  - `PlanReport` 加 `partial`：计划的条目跑完之后（合并前或合并中）停下的，`finish` 不再丢掉报告：取消时把报告作为失败的工具结果返回（引擎先保存结果，随后照常按取消结束任务），`Uncertain` 时把报告附在错误里，其他错误同样返回报告。
  - 报告写明：已写入的文件（一个检查点，`/diff` 可看、`/undo` 可撤销）、正在合并的条目只有列为已写入的文件进了工作区、没合并的条目及其层目录。重做被取消时，它的冲突文件按未解决列出（原因 `the redo stopped`），副本照常保存。
  - 计划停下时有已完成但没合并的条目，计划目录不登记到 `OverlayCleanup`，任务结束后仍保留，结果里写明位置；条目运行中就停下、没有完成条目的，照旧到任务结束删除。
- 测试：`agents::an_overlay_merge_cancelled_during_a_redo_reports_what_reached_the_workspace`：三个条目，前两个改同一行，第二个的重做一直不返回，任务时限（15 秒）到期取消；主工作区只有第一个条目的结果、第三个条目的文件没写；会话里记录的工具结果有 `direct write`、`conflict unresolved`、`the redo stopped`、`Merging stopped`、`/undo reverts it`、`todo-2 was being merged`、`Not merged, still pending`；任务结束后 `todo-2`、`todo-3` 的层还在。

### 3. 菱形依赖的下游只看到最上面一层（中）

- 现象：b、c 按不同 `::symbol` 并行改 `lib.rs`，d 依赖两者，d 读到的 `lib.rs` 只有 c 的改动（`OverlayStack::lookup` 取最上层），检查槽同步的也是这个视图。
- 修复：建栈时，层栈里有两个互不依赖的上游的条目，多一个组合层 `<条目>-combined/`，位置在上游层之上、自己的层之下。`PlanTask` 加 `before_start` 钩子，条目开始前（上游都已完成）调用 `combine`：对两个以上上游改过的每个文件，去掉“被另一个改过它的上游依赖着”的上游（它的改动已经在对方版本里），剩下的按依赖顺序用 `merge_file_using` 依次三方合并（基线 = 该上游的基线副本，当前 = 已合并的结果，本方 = 该上游的版本），写进组合层。条目重试时组合层已有内容，相同的跳过。读、grep、glob、repo_map 和检查槽都经 `OverlayStack`，自动看到组合层。
- 合不成（冲突、基线副本读不出、合并失败）：钩子返回原因，`run_tasks` 把条目记为跳过（`deferred: ...`），依赖它的条目随后也跳过；不改成依次叠层，因为那要求两个上游串行，放弃了它们的并行。条目留在待办里，结果里提示合并完再调用 `execute_plan`，那时上游已完成，它直接在合并后的主工作区上运行。`run_tasks` 在钩子跳过条目、又没有运行中的条目时多转一轮，让跳过沿依赖传下去。
- d 合并进主工作区时，它的基线就是组合层的内容，和主工作区（已合入 b、c）三方合并。
- 测试：`agents::an_item_on_two_parallel_layers_of_one_file_sees_both_changes`（d 的 `read`、`grep` 同时看到 b、c 的改动，3 个条目全部完成，层目录删除）；`agents::an_item_on_two_conflicting_layers_is_deferred_with_its_dependents`（b、c 改同一行，d 和依赖 d 的 e 都没运行，报告里是 `deferred: the items it builds on (todo-1, todo-2) both changed src/lib.rs` 和 `dependency todo-3 did not succeed`）。

### 4. 覆盖层模式仍在用文件认领（中）

- 现象：`run_tasks` 对所有条目都传 `Claims`，同一文件的覆盖层条目被串行；两个条目反序写两个共享文件时被判死锁，条目失败。
- 修复：`run_tasks` 只给没有覆盖层的条目传认领；`main` 不变。
- 测试：`agents::overlay_items_writing_shared_files_run_in_parallel_without_claims`：两个条目分别按 `a.rs` → `b.rs` 和 `b.rs` → `a.rs` 编辑（第二次编辑前模型延迟 0.8 秒），两次慢请求在时间上重叠，2 个条目完成、没有 `write conflict`，两个文件都是合并后的结果。去掉修复时这个测试失败。

### 5. 低优先级

- **只在层里的新目录、大小写不同的路径**：`grep {path:"newdir"}` 在 `ripgrep=true` 时 rg 报 `IO error`：`LocalTools::grep` 在搜索起点磁盘上不存在时不再调用 rg，只搜层。`Shadow::files_under` 和“搜的是不是层里的单个文件”改用 `paths::key` 按路径分量比较；include 匹配用的相对路径在大小写不同时按分量数截取。repo_map 选层里的文件也改用同样的比较。测试：`overlay::grep_finds_files_in_directories_only_the_layers_hold`（两种搜索；Windows 上另测 `NEWDIR`、`NewDir/X.RS`、`NEWDIR` + include）。
- **合并新建的目录**：`Workspace::create_dirs` 先把缺的各级目录（相对路径，父目录在前）记进当前检查点（`created_dirs`，旧检查点没有这个字段时为空）再创建；`undo` 在还原文件之后从深到浅 `remove_dir`，只删空目录，别的东西放进去的目录保留。`Merger::write` 改用它；`write` 工具没有改（它不经过 `Workspace`，行为和以前一样）。测试：`tools::directories_created_for_a_checkpoint_are_removed_by_undo_when_empty`。
- **`Overlay::persist` 平方级变慢**：状态里同时保存清单形式（`stored_entries`、`stored_intents`，blob 哈希已算好），每次写只为这一次的内容和意图写 blob、算哈希，再写一次清单；写清单失败时回退清单形式，内存状态不变。也不再整份克隆状态（里面有每个 `Write` 意图的全部内容）。清单格式不变。测试：`session::overlay::tests::repeated_writes_are_stored_one_change_at_a_time_and_restore_in_full`（同一文件写 40 次、新建再删除，重新打开后条目、意图、基线一致）。
- **repo_map 截断时层里的文件优先**：层里的文件先放进待扫描列表，再遍历磁盘，超过 `MAX_FILES`（5000）时被截掉的是磁盘文件。测试：`overlay::repo_map_keeps_the_layered_files_when_the_scan_is_truncated`（5001 个磁盘文件 + 层里一个新文件，查询能找到层里的符号，结果带 `scan stopped`）。

### 第二轮（复审）

- **保留的层不会被删**：供恢复保留的计划目录（计划在条目跑完后、合并前或合并中停下）以前永远留着。现在 `overlay_plan::prune_kept` 在启动时删掉 `<数据目录>/overlays/` 下 7 天（`KEPT_DAYS`，和 `tool-outputs/` 的期限相同）没修改过的计划目录，和 tool-outputs 的清理在同一个 `spawn_blocking` 里。正常的计划目录在任务结束时已经删了，所以只会清到保留下来的。结果里写明"任务结束时不删、7 天后自动删除"，并说明怎么手动查看（`overlay.json` 列出路径和 blob，内容在 `blobs/`）。不做恢复工具。测试：`agents::overlay_plan::tests::kept_plan_directories_go_after_their_retention`（新目录不删、过期的删、结果措辞里的天数和常量一致）。
- **取消被报成 deferred**：组合层钩子改为返回 `NotStarted::{Cancelled, Skipped(reason)}`。开始前或合并后发现已取消、或者合并返回 `Cancelled` 时返回 `Cancelled`，`run_tasks` 把条目记为 cancelled（"still pending, execute_plan again resumes it"），不再写 "deferred…merging failed"。测试：`agents::overlay_plan::tests::combining_merges_the_layers_defers_on_a_conflict_and_honours_cancellation`（能合并的写进组合层；已取消时返回 `Cancelled`、组合层为空；冲突时返回 `deferred: …`）。
- **manifest 仍整份重写，含 intents 原文**：清单升到版本 2，只记文件列表、blob 哈希，以及 `intents_bytes`（意图日志里已确认的字节数）。意图改成追加写进同目录的 `intents.jsonl`，每行一个，用新的 `persist::append_at`：先把超出已确认长度的尾巴截掉，再追加并 sync，返回新长度，然后原子写清单确认这个长度。写清单失败时，那一行没被确认，下次追加前截掉。恢复时只读已确认的字节。版本 1 的清单（意图在清单里）照样能读，下一次写时把旧意图一起写进日志，清单改成版本 2。测试：`session::overlay::tests::intents_are_appended_to_a_log_and_an_unconfirmed_tail_is_dropped`（清单里没有 `old`；日志末尾半行没确认的记录，重新打开时被忽略、下次写之前截掉，前面已有的内容不变）、`a_version_1_layer_is_read_and_moves_its_intents_to_the_log`；原有的恢复测试都通过。
- 本轮验证：`PATH` 带 mergiraf 跑全量 `cargo test --release --no-fail-fast`，lib 602 通过、7 忽略，集成测试 269 通过；clippy 只有原有 5 条；mergiraf 的 `merges/` 下没有留下记录。
- 本轮改动的文件：`src/agents/overlay_plan.rs`、`src/agents/plan.rs`、`src/session/overlay.rs`、`src/persist.rs`（`append_at`）、`src/app/mod.rs`（启动时清理）、`docs/reference.md`。

### 改动的文件

`src/tools/search.rs`、`src/tools/mod.rs`、`src/agents/plan.rs`、`src/agents/overlay_plan.rs`、`src/agents/collab.rs`（`PlanTask` 新字段）、`src/session/overlay.rs`、`src/session/workspace.rs`、`docs/reference.md`；测试在 `tests/all/agents.rs`、`tests/all/overlay.rs`、`tests/all/tools.rs`。没有改 `session/merge.rs`、`safety/permission/classify.rs`、`engine/estimate.rs`、`agents/writes.rs`、`tools/todo.rs`。

### 验证

- `PATH` 里带 mergiraf 跑 `cargo test --release --no-fail-fast` 全量：lib 588 通过、7 忽略；集成测试 269 通过，没有失败（已知偶发的几个这次也通过）。不带 mergiraf 再跑覆盖层计划相关的 8 个集成测试，全部通过。跑完 `%APPDATA%\Mergiraf contributors\Mergiraf\data\merges\` 下没有留下记录。
- 去掉修复 1、3、4 后，`grep_include_does_not_bring_back_changed_or_deleted_disk_files`、两个菱形依赖测试、`overlay_items_writing_shared_files_run_in_parallel_without_claims` 都失败，恢复后通过。
- `cargo clippy --release --all-targets`：只有原有 5 条。
- 只对改过的文件跑 `rustfmt --edition 2024 --config max_width=100`，差异都在本次改动的行里。
- 没有做：计划停下后保留的目录不会自动清理（要用户自己删）；`write` 工具新建的目录仍不进检查点。
