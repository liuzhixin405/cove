# 0030 跨项目的用户偏好：自动发现，确认后才写入全局

- 状态：已完成
- 日期：2026-11-23（创建）；2026-11-24（完成）
- 关联：[0003](0003-remove-skill-auto-learning.md)（技能自动学习：要审批，结果没人批）、[0008](0008-memory-injection-and-notes.md)（记忆分层注入，全局记忆必须完整）、[0021](0021-pre-commit-ai-review.md)（项目规则 `.cove/rules.md` 的来历）、[0023](0023-real-model-evals.md)（真实模型评测）

## 背景

本机会话和记忆里反复出现同一类要求，这些要求和具体代码库无关，换个项目也成立：

- 改动越小越好，功能要全；
- 先 AI 审查，没问题再提交、推送；
- 设计文档按 `0000xx` 编号，按好的样板写；
- 字段和接口去查官方文档，别靠推断。

这些要求现在要么被记成某个项目的记忆或规则，要么根本没记下来。换一个项目，cove 就不知道了，用户只能再说一遍。

设想：记忆提取识别出"偏好类"内容，在下一个输入提示前问一句"要不要记成全局偏好"，用户确认后才写进全局。

## 现状与问题

### 三种存储现在的分工

| | 项目记忆 | 全局记忆 | 项目规则 |
|---|---|---|---|
| 位置 | `<数据目录>/memory/<工作区路径哈希>/*.json` | `<数据目录>/memory/global/*.json` | `<git 根>/.cove/rules.md` |
| 性质 | 笔记："notes, not instructions; may be stale"（`memory/recall.rs:318`） | 同左；分层注入时标题写成 "Global (always apply unless the user says otherwise)"（`recall.rs:506`） | 指令：进系统提示的 "Project instructions"（`engine/prompt.rs:13`、`:296`） |
| 注入 | 每个会话第一个任务的用户消息里，超过 8 KiB 分层（`recall.rs:457`） | 同一段里，分层时最多 4 KiB 全文（`recall.rs:30`） | 系统提示，完整，64 KiB 预算（`prompt.rs:23`） |
| 自动写入 | 提取、dream | **没有**：提取强制写项目（`extract.rs:390`），dream 只读（`dream.rs:776`） | 提取的 `---RULE---`、dream 的 `project_rules` 工具 |
| 手动写入 | `/memory add`、`memory` 工具 | `/memory add global`、`memory` 工具（要授权） | `/rules add` |

提取提示词怎么区分 RULE 和普通记忆（`extract.rs:25`）：

- 记忆：持久事实，"still true next week"，不收临时状态和代码里看得出的东西。
- RULE：只收**用户对做法的纠正**，或者"为这个项目"明说的长期规则（"don't…""以后都…""别再…"），写成一句"不要……，改为……"。
- 提示词里写着 "Automatically extracted entries are project-scoped; global memory requires explicit user action"，但输出格式里又保留了 `SCOPE: project|global`。`parse` 会解析这个字段（`extract.rs:76`），`run` 却一律按 `Scope::Project` 写（`extract.rs:390`），测试 `automatic_global_proposals_remain_project_scoped` 专门检查这一点。**模型被要求填一个永远不起作用的字段。**

### 证据核对（对照实施前的当前代码，2026-11-24）

草稿写成之后完成了 0024、0025、0027、0029、0032，这几份都没有改记忆、规则和提取。逐条对照实施前的代码：

| 草稿里的问题 | 结论 | 依据 |
|---|---|---|
| 提取输出格式里有 `SCOPE`，解析了却不起作用 | 成立 | `extract.rs` 的 `SYSTEM` 输出格式含 `SCOPE: project\|global`，`parse` 把 `SCOPE: global` 解析成 `Scope::Global`，`run` 写入时固定用 `Scope::Project`；原测试 `automatic_global_proposals_remain_project_scoped` 用 `SCOPE: global` 的回复复现：条目进了项目记忆，全局为空 |
| 提取只写项目记忆，dream 只读全局 | 成立 | 同上；`dream.rs` 提示词 "global memory is read-only"，`MemoryOnly` 授权只放行项目目录，测试 `dream_cannot_write_global_memory` |
| 全局记忆分层注入时被写成"总是生效" | 成立 | `recall.rs` 分层时的标题是 "Global (always apply unless the user says otherwise)" |
| 偏好挂在工单标题下难以被召回 | 机制成立；具体条目是历史数据 | 召回机制没变：记忆超过 8 KiB 时只按 BM25 选 `TOP_K` = 5 条项目记忆。P2 表里的条目来自 2026-11-23 的本机数据快照，本次没有重新读取本机记忆，未在当前版本确认 |
| 没有任何地方会问"要不要在所有项目里都这样" | 成立 | 代码里没有这类提问；全局只能 `/memory add global` 或授权后的 `memory` 工具写入 |
| P1、P2、P3 的数量统计 | 历史现象，未在当前版本确认 | 来自 2026-11-23 的本机数据快照；本次未重新统计，不单独拿它们定优先级 |

### P1 全局只有 3 条，偏好只有 1 条（历史数据，2026-11-23 快照）

本机数据（`~/.cove`，2026-11-23）：

- 全局记忆 3 条：用户语言偏好（提取，旧版本写入）、`TMP` 环境变量、cove 可执行文件位置。真正属于"偏好"的只有语言这一条，另外两条是本机环境事实。
- 项目记忆 7 个项目共 201 条：QuoteHub（3a1cc4c0）181 条（提取 144、工具 29、dream 8），vfx-price-engine-mono（fae6bede）9 条，ccs-backend（01e57384）5 条，其余 3 个项目共 6 条。
- 项目规则文件只有 QuoteHub 一份，10 条。

### P2 跨项目的偏好被记在项目里（历史数据，2026-11-23 快照）

用脚本扫了全部 201 条项目记忆和 10 条规则，按内容判断"换个项目是否仍然成立"。结果：**8 类偏好，散在 17 处（15 条记忆、2 条规则），全局一条都没有**：

| 偏好 | 出现在哪 | 会话里的出处 |
|---|---|---|
| 最小改动、功能要全 | QuoteHub 2 条记忆（0c904780、223352d6），标题都挂在一个工单名下 | 1 个会话 |
| 先 AI 审查，没问题再提交推送 | QuoteHub 2 条（d0c4c81d 标题是"某工单的推送流程"、85f6c8d0） | 1 个会话，提了 2 次 |
| 设计文档按编号写、按样板写 | QuoteHub 2 条（66faf14b、b66b4acd，后者 8 条来源、来自 4 个会话） | 1 个会话（问的是怎样借鉴到其他项目） |
| 查官方文档，不要推断 | **没记成偏好**，只作为事实混进一条工单记忆（19dda1e1）；相近的有规则 5"不要凭搜索命中或略读就下结论"、记忆 9017c7d2"不要编造证据" | 1 个会话 |
| 个人文档放 `.local/`，不提交 | QuoteHub 记忆 2 条 + 规则 4；vfx-price-engine-mono 记忆 1 条；另有 1 条 vfx 的记忆被记进了 QuoteHub（d8f4cb04） | 多个会话 |
| 学习项目的方式（动手跑、调试、做小任务，而不是读更多文档） | QuoteHub 3 条标题直接写着 "User preference:"（149c1373、2ff161c3、689dd107），另有 2 条相关 | 多个会话 |
| 技能写成简短指令 | QuoteHub 1 条（afcdf612），内容是关于 cove 本身的 | — |
| 延后的优化记进文档，不改代码 | QuoteHub 1 条（13aae3f2） | — |

几个事实：

1. **提取认得出"这是用户偏好"**：3 条记忆的标题直接写着 "User preference"，但只能写进项目（见上面"三种存储现在的分工"）。
2. **偏好被埋进了一次性条目**：最小改动、审查后推送都挂在工单标题下，下次换个需求，按相关性注入（`TOP_K` 5 条）时几乎不会被选中。
3. **有的根本没记下**："查官方文档别推断"不是对做法的纠正句式，RULE 没收，记忆把它当成了某个工单的事实。
4. **只有 `.local/` 这一类在 cove 的数据里真的跨了两个项目**。其余 7 类都只在 QuoteHub 说过，因为 cove 的 122 个会话里 49 个在 QuoteHub，输入里的短句纠正 42 处有 37 处在 QuoteHub。"跨项目"的判断主要来自内容本身；另一个旁证是另一个工具（Claude Code）在 cove-main 项目里的记忆，也记了"先调研再改""改前先写文档""删功能不留空壳"这类同性质的偏好。

### P3 环境事实也重复（不属于本文范围，记下来）

`TMP` 指向系统临时目录（全局 + ccs-backend 的 "rules and patterns" + QuoteHub 2 条）、cove 安装位置（全局 + Downloads 项目 1 条）、`glab` 没登录公司 GitLab（ccs-backend + QuoteHub）、`dotnet` 输出是中文（ccs-backend）、Windows PowerShell 引号问题（QuoteHub 规则 2 + 2 条记忆）。这些是本机事实，不是偏好，见"范围"。

### P4 现有的"全局"入口都靠用户自己想起来

`/memory add global` 要用户主动输入；`memory` 工具写全局要授权，模型也很少主动这么做（全局 3 条里没有一条来自工具）。没有任何一处会在用户表达了偏好之后问一句"要不要在所有项目里都这样"。

## 目标

1. 有一份全局偏好文件，进每个项目的系统提示，换项目也生效。
2. 用户说出跨项目偏好后，cove 自动发现，在不打断任务的时机问一句；确认了才写进全局。没确认之前，这条要求至少在当前项目里照样生效。
3. 项目规则和用户当前的要求优先于全局偏好，提示词里写明。
4. 一个命令能看全、能删、能把项目规则提升为全局偏好。
5. 检验标准：评测用例里，只在数据目录预置一条全局偏好的新项目任务能遵守它；预置相反的项目规则时，项目规则胜出。

## 范围

- 做：
  - 全局偏好文件 `<数据目录>/preferences.md`，格式和 `.cove/rules.md` 一样；
  - 提取识别"跨项目偏好"，先记成项目规则，再在下一个输入提示前问是否升为全局；
  - `-p` 和无终端时只排队，下一次交互时再问；
  - `/rules` 增加全局偏好的查看、添加、删除、从项目规则提升；
  - 系统提示增加 "User preferences" 一节，写明优先级；
  - 现有全局记忆里的"用户语言偏好"迁进偏好文件；提取输出格式删掉 `SCOPE`；
  - 评测用例和评测脚本预置数据目录的能力。
- 不做：
  - **环境事实**（P3）不走这套流程。它们是"笔记"，可能过时，不该作为指令进系统提示；该进全局记忆，但涉及 dream 跨项目去重，另开文档。
  - 不自动迁移已经记在项目里的偏好（理由见"方案 6"）。
  - 不让 dream 写全局偏好：dream 只看本项目的会话，判断不了"跨项目"；全局记忆只读的规则也保持不变。
  - 不按相关性挑选偏好：数量少、要始终生效，和项目规则一样全文注入。
  - 不做偏好的过期机制，和 0021 的规则一致。

## 调研与参考

以下同类产品的情况凭已有知识整理，没有联网核实，细节以官方文档为准。

| 产品 | 全局 / 用户级 | 项目级 | 自动写入 | 需要确认 |
|---|---|---|---|---|
| Claude Code | `~/.claude/CLAUDE.md`，每个项目都加载 | 项目 `CLAUDE.md`；自动记忆按项目存 `MEMORY.md` 加主题文件 | 自动记忆只写项目，不自动写用户级 `CLAUDE.md` | 用户级说明只能用户自己改；早期的 `#` 快捷记忆会让用户选"项目还是用户级" |
| ChatGPT memory | 账号级，跨所有对话 | "项目"内可以有单独记忆 | 自动提取，界面提示"已更新记忆" | 不需要；用户事后在设置里查看、删除，或说"忘掉这条" |
| Cursor | User Rules（设置里的纯文本，所有项目） | `.cursor/rules/*.mdc`，可设"始终""按文件匹配""由模型决定" | 曾推出从对话生成的 Memories | 后台生成的 Memories 需要用户批准才生效 |
| Windsurf | `global_rules.md` | 工作区规则；Memories 按工作区 | Cascade 自动生成 Memories | 不需要 |

两种做法的取舍：

- **自动写入**（ChatGPT、Windsurf、Claude Code 自动记忆）：
  - 体验：零打扰，用户说一次就记住。
  - 风险：记错了范围或记错了内容，用户不知道；ChatGPT 的做法是"写入时提示一行 + 随时可查可删"来兜底。放在全局里风险更大：一个项目里的业务细节会带进所有项目，包括别的公司的仓库。QuoteHub 规则 10"另一个演示项目里不准提到 QuoteHub"正是这种串项目的后果。
- **需要确认**（Cursor Memories、Claude Code 的用户级说明）：
  - 体验：准确，范围由用户定。
  - 风险：确认疲劳，或者没人确认。0003 的技能候选 17 条**全部停在待审批**，一条也没生效。

本方案取两者之长，关键是**确认只决定"范围升级"，不决定"记不记"**：

- 发现偏好后，立刻按 0021 的规则记成项目规则，当前项目马上生效，和今天一样不用审批；
- 只问"要不要在所有项目里都这样"。用户不理它，损失的只是"别的项目不知道"，和今天一样，不会像 0003 那样什么都没留下；
- 全局是所有项目的指令，影响面大，必须用户点头。这和 Claude Code"用户级说明只由用户改"一致。

存储形式的对比：

| 方案 | 好处 | 坏处 |
|---|---|---|
| A. 全局记忆里加"偏好"类型 | 不新增文件 | 记忆是"笔记，不是指令"，进的是用户消息、可能被截断（0008 P1 就截掉过语言偏好）；要改记忆的数据格式、dream、注入三处 |
| **B. 独立文件 `preferences.md`（建议）** | 和项目规则完全对称：项目有"记忆 + 规则"，全局也有"记忆 + 偏好"；进系统提示，完整生效；可以手改；复用 `ProjectRules` 的代码 | 多一个文件 |

## 方案

### 1. 分工（改完以后）

| | 笔记（可能过时，仅供参考） | 指令（始终生效） |
|---|---|---|
| 项目 | 项目记忆 | 项目规则 `<git 根>/.cove/rules.md` |
| 全局 | 全局记忆（只放本机环境事实） | **全局偏好 `<数据目录>/preferences.md`** |

和现有机制的重叠，处理方式：

- **全局记忆里的偏好 → 替换**。唯一的一条"用户语言偏好"迁进 `preferences.md`，原条目删除（先备份到 `<数据目录>/backups/0030-global-preferences/`）。之后全局记忆只放环境事实。`recall.rs` 分层注入时的标题从 "Global (always apply unless the user says otherwise)" 改为 "Global notes (this machine)"，不再暗示它是指令。
- **提取输出里的 `SCOPE` → 删除**。它从来不起作用；跨项目的内容改走新的 `---PREFERENCE---` 块。`Extracted.scope` 字段、`parse` 里对 `SCOPE:` 的解析、相关测试一起删掉，不留兼容。
- **项目规则 → 合并代码**。`ProjectRules` 改成通用的 `RulesFile`，两个实例：项目规则、全局偏好。表头、路径、上限不同，读写、去重、加锁、`screen`、"本进程新增、待提示"的逻辑共用一份。

### 2. 全局偏好文件

- 位置：`<数据目录>/preferences.md`（默认 `~/.cove/preferences.md`，跟随 `--data-dir`，评测因此可以预置）。
- 格式：说明头，加每条一行 `- 偏好（YYYY-MM-DD）`，和 `.cove/rules.md` 一样，可以手改。
- 上限：**20 条，每条 200 字**；写入时超出就拒绝并提示整理。注入总量再设一个 8 KiB 的硬上限，超出时截断并标注（正常写满 20 条中文约 12 KiB，所以靠写入上限挡住，硬上限只防手改得太长）。
- 写入走 `persist::with_lock` + `write_atomic`，先过 `memory::store::screen`，和规则一样。

### 3. 注入位置和优先级

- 系统提示（`engine/prompt.rs` 的 `system_context`）在 "# Project instructions" **之前**加一节：

  ```text
  # User preferences
  The user's own preferences, for every project. Follow them unless the project
  instructions below, or the user's current request, say otherwise; those win.
  <preferences>
  - ...
  </preferences>
  ```

  放在项目说明之前，读到冲突时后出现的项目说明更具体；文字里也明说项目规则和当前请求优先。
- 优先级（从高到低）：用户当前的请求 > 项目说明（AGENTS.md、CLAUDE.md、`.cove/rules.md`）> 全局偏好 > 记忆。
- 子 Agent 的系统提示同样带上这一节（它们本来就带项目说明）。
- 会话中途新确认的偏好：系统提示在进程内不变（为了提示缓存），和项目规则一样，由 `TurnHook::task_note` 在下一个任务开始时告诉模型。
- 提交前审查和自审**不**按全局偏好检查（见待决策 7）。

### 4. 提取怎么识别跨项目偏好

提示词（`extract.rs` 的 `SYSTEM`）改成三类输出：

- `---MEMORY---`：不变，去掉 `SCOPE` 行。
- `---RULE---`：不变，用户对做法的纠正，或为这个项目说的长期规则。
- `---PREFERENCE---`：**新增**。同时满足下面三点才写，否则写成 RULE：
  1. 用户自己说的，关于"希望怎么做事"：工作流程（提交前先审查）、改动方式（最小改动）、沟通（回复语言、先给方案再动手）、文档写法、核实方式（查官方文档不推断）；
  2. 不依赖这个项目：不提这个项目的路径、文件、模块、服务、分支、工单号、业务名词；
  3. 换一个无关的项目仍然成立。用户说了"所有项目""以后都""其他项目也"时尤其如此。
  拿不准时写成 RULE：项目规则马上生效，范围小，错了代价也小。
- 提示词里列出已有的全局偏好（和已有项目规则一样），避免重复提出。

代码层再挡一道（模型判断不可靠时兜底）：

- 候选里含当前工作区目录名、git 远端仓库名、绝对路径、`screen` 拦下的内容 → 不作为全局候选，只记成项目规则；
- 和已有全局偏好归一化后相同 → 丢弃；
- 在"已拒绝"列表里 → 只记成项目规则，不再问。

### 5. 确认交互

**流程**：

1. 提取得到 PREFERENCE 候选 → 立刻写进当前项目的 `.cove/rules.md`（和 RULE 一样，当前项目马上生效）；
2. 同时写进待确认队列 `<数据目录>/preferences.pending.json`（`persist::atomic_json` + 锁），记录：文本、来源项目、会话 id、时间、对应的项目规则文本；
3. 在**交互循环读取下一行输入之前**（`app/mod.rs` 的 `repl_lines`，`reader.read` 之前、`apply_titles` 之后）检查队列，有就问：

   ```text
   [偏好] 这条要求看起来不只适用于这个项目：
     改动尽量小，功能要完整
   在所有项目里都这样做吗？ y 记成全局偏好 · n 只留在本项目 · 回车 以后再说
   ```

   - `y`：写进 `preferences.md`，从本项目 `.cove/rules.md` 删掉对应那条（全局已覆盖）；
   - `n`：留在项目规则，记入"已拒绝"，以后不再问同一条；
   - 回车或其他输入：放回队列，下次启动交互时再问。超过 30 天没处理的候选自动清掉（它在项目里一直生效，不会丢）。

**为什么不会打断任务**：

- 只在"任务结束、等待下一行输入、用户还没开始打字"那一刻问；任务运行中、用户正在输入时都不问。提取要 5–20 秒（0021 遗留），用户已经开始输入下一句时，问题会顺延到这个任务结束之后。
- 每次最多问 1 条，一次启动最多问 3 条，避免一屏问题。
- 和 `announce_rules` 那一行提示合并：同一条既记成了项目规则、又在问是否升为全局时，只显示这个问题，不再另打一行"记下一条项目规则"。

**没有终端时**（`-p`、管道输入、远程监督）：

- 不问。第 1、2 步照做：项目里生效，候选排队。
- `-p` 结束时在标准错误打印一行：`[偏好] 有 1 条候选等待确认，下次交互运行 cove 时会询问；或用 /rules pending 查看`。
- 不在项目里（家目录、盘符根，`ProjectRules::for_workspace` 返回 `None`）：只排队，不写项目规则。

**多个窗口**：取出队列中的一条时先在锁内标记"正在询问"，避免两个窗口问同一条；进程退出时没答复的恢复为待问。

### 6. 已经记在项目里的偏好要不要迁移

**不写自动迁移代码**。理由：

- P2 那 17 处大多是记忆，内容和工单、项目细节混在一起（例如"最小改动"挂在一个工单的验收标准里），靠规则拆出来容易带出业务内容，这正是要避免的串项目风险；
- 数量不大，一次性人工处理更可靠。

做法：

- 实施时由实施方从 P2 整理出 5–8 条候选文本（不含项目名和业务词），交给用户逐条确认，确认的写进 `preferences.md`，记进"决策记录"；
- 以后需要时用 `/rules promote <n>` 把某条项目规则提升为全局偏好（写入全局、从项目删掉，写全局失败时项目不动）；
- 项目里那些记忆条目不删：它们还带着项目上下文，清理是 dream 的职责。

### 7. 查看和删除

扩展 `/rules`，不新增命令（两者是同一类东西，补全和帮助也少一套）：

| 命令 | 作用 |
|---|---|
| `/rules` | 先列全局偏好（编号 `g1`、`g2`…），再列项目规则（`1`、`2`…），并显示两份文件的路径 |
| `/rules add global <文字>` | 添加一条全局偏好（与 `/memory add global` 的写法一致） |
| `/rules remove g<n>` | 删除一条全局偏好；`remove <n>` 仍删项目规则 |
| `/rules promote <n>` | 把第 n 条项目规则提升为全局偏好 |
| `/rules pending` | 列出待确认的候选，并可逐条 y/n |

`HELP` 字符串同步改，它同时驱动 `/help`、补全和"你是不是想输入"（AGENTS.md 二节）。偏好文件也可以直接编辑。

## 影响范围

- `src/memory/rules.rs`：`ProjectRules` 泛化为两类实例；新增待确认队列（可以放在同一文件，或新建 `memory/preferences.rs`，视代码量定）。
- `src/memory/extract.rs`：提示词、`---PREFERENCE---` 解析、代码兜底、写项目规则 + 排队；删除 `SCOPE`。
- `src/memory/recall.rs`：全局记忆的分层标题改字。
- `src/memory/dream.rs`：提示词里列出已有全局偏好（只读），避免把它们再写成项目规则。
- `src/engine/prompt.rs`：`system_context` 增加 "User preferences" 一节，需要能拿到数据目录。
- `src/app/mod.rs`：输入前询问、启动时询问、`-p` 结束提示；`announce_rules` 合并。
- `src/commands/project.rs`：`/rules` 子命令和 `HELP`。
- `docs/reference.md`（记忆、项目规则两节）、README。
- 数据：新增 `<数据目录>/preferences.md`、`preferences.pending.json`；全局记忆里的语言偏好迁走。
- 行为变化：
  - 每个项目的系统提示多一节（目前只有语言 1 条，约 100 字节）；
  - 提取的输出多一种块，不新增模型调用；
  - 偶尔在输入提示前多一个问题。
- 评测：`evals/run.py` 支持用例自带 `data/` 目录，运行前复制进独立数据目录。

## 验证

### 单元测试

- `memory::rules`（全局偏好实例）：
  - 添加、去重、20 条和 200 字上限、`screen` 拦密钥、手改后保留用户的表头；
  - `promote`：全局写入成功后才从项目删除；全局已满时项目规则保持不变。
- 待确认队列：
  - 写入、读出、跨进程加锁；
  - 30 天过期；
  - 拒绝过的同一条不再进队列；
  - "正在询问"的标记在进程退出后恢复。
- `memory::extract`：
  - `---PREFERENCE---` 和 `---RULE---`、`---MEMORY---` 分开解析；
  - 候选写进项目规则并进入队列；不在项目里时只进队列；
  - 候选含工作区目录名或绝对路径时只记成项目规则、不进队列；
  - 已有全局偏好的同一条被丢弃；提示词里列出已有全局偏好；
  - 输出格式里不再有 `SCOPE`（改写 `automatic_global_proposals_remain_project_scoped`）。
- `engine::prompt`：
  - 有偏好时 "# User preferences" 出现在 "# Project instructions" 之前，并带优先级说明；
  - 没有偏好时没有这一节；
  - 超过 8 KiB 时截断并标注；
  - 子 Agent 的系统提示也带这一节。
- `memory::recall`：全局记忆分层注入的标题改成"笔记"。
- 询问时机（把判断写成纯函数测）：
  - 交互、空闲时问；
  - 有任务在运行、没有终端时不问；
  - 一次启动最多问 3 条。
- `commands`：`/rules` 列出两部分；`remove g1`、`promote 2`、`add global`、`pending` 的解析和结果。

### 评测用例（真实模型，形式同 `evals/cases/`）

前提：`evals/run.py` 支持用例目录里的 `data/`，在运行前复制进 `--data-dir` 指向的独立数据目录。这样只有预置的偏好，不读用户自己的数据。

1. **`global-preference-applies`**（遵守全局偏好）
   - `repo/`：和 `feature-with-docs` 相同的 todo 小项目，**没有** AGENTS.md，README 有命令表。
   - `data/preferences.md`：`- 改了命令或行为，同一次改动里同步更新 README（2026-11-23）`。
   - prompt："给 todo.py 加一个 clear 命令，清空所有待办。"
   - 判定：隐藏测试通过；README 的命令表里有 `clear`。
   - `selftest.py`：`bad_no_readme`（只改代码）判失败；`good_with_readme` 判通过。
   - 区分度：先跑一轮**不预置偏好**的对照。如果模型本来就会顺手改 README，这个用例区分不出好坏，就换成默认不会做的偏好，例如"每次改动在 CHANGELOG.md 的 Unreleased 下加一行"，判定 CHANGELOG 新增了一行且提到 clear。
2. **`project-rule-beats-preference`**（冲突时项目规则优先）
   - 同一个仓库，`AGENTS.md` 写"README 由 `scripts/gen_readme.py` 生成，不要手改"；`data/preferences.md` 同上。
   - 判定：README 和生成脚本的输出一致（运行了脚本，或者没手改）；回复里没有声称手改了 README。
   - `selftest.py`：`bad_hand_edit`（手改 README）判失败；`good_regenerate` 判通过。

### 人工确认（release 版）

- 在演示仓库里说一句"以后改动都尽量小，功能要全"，任务结束后：
  - 下一个输入提示前出现询问，`.cove/rules.md` 里已经有这一条；
  - 选 `y`：`preferences.md` 里出现这一条，项目规则里删掉；换到另一个目录启动，`/rules` 能看到；
  - 选 `n`：留在项目规则，再说一次同样的话不再问。
- `cove -p` 跑一次同样的话：不询问，结束时标准错误有一行提示；下次交互启动时询问。
- 任务进行中、正在输入时都不出现询问。

### 全量

`cargo test --release` 全部通过；`cargo clippy --release --all-targets` 不新增警告；`python evals/run.py --self-test` 通过。

## 实施计划

- [x] 阶段 1：`RulesFile` 泛化，全局偏好文件，系统提示注入，`/rules` 的 global/remove g/promote；迁移语言偏好，改 recall 标题
- [x] 阶段 2：提取的 `---PREFERENCE---`、代码兜底、删除 `SCOPE`；待确认队列
- [x] 阶段 3：交互询问、`-p` 提示、`/rules pending`
- [x] 阶段 4：评测脚本支持 `data/`，两个评测用例
- [ ] 阶段 4（续）：从 P2 整理的候选文本交给用户逐条确认（候选见"遗留"，未写入）
- [x] 阶段 5：文档（reference.md、README、evals/README.md）、测试、格式化、回填验证结果

## 待决策

1. **存储**：独立文件 `<数据目录>/preferences.md`，还是给全局记忆加一个"偏好"类型？
   建议：独立文件。和项目规则对称，进系统提示、完整生效、可以手改，代码复用规则模块；全局记忆保留给环境事实。
2. **命令**：扩展 `/rules`，还是新建 `/prefs`？
   建议：扩展 `/rules`（`global`、`g<n>`、`promote`、`pending`）。同一类东西放一个命令，用户一次看全，帮助和补全也少一套。
3. **确认之前怎么办**：先记成项目规则，还是只挂在队列里，确认后才生效？
   建议：先记成项目规则。当前项目马上生效，用户不理也不吃亏，避开 0003"等审批、结果没人批"的问题。
4. **确认方式**：输入提示前单键询问，还是只打印一行提示、让用户自己用 `/rules promote` 处理？
   建议：单键询问，只在空闲、等待输入的时刻，每次 1 条、每次启动最多 3 条。只打印提示等于让用户记得去处理，0003 证明这条路走不通。
5. **已有的重复偏好**：写自动迁移，还是一次性人工确认？
   建议：一次性人工确认。由实施方从 P2 整理 5–8 条不含项目名的候选，用户逐条确认后写入；以后用 `/rules promote`。不写迁移代码。
6. **上限**：20 条、每条 200 字、注入硬上限 8 KiB，是否合适？
   建议：就用这个。偏好应该少而稳定，比项目规则（60 条、300 字）更严；满了提示整理，不悄悄截断。
7. **提交前审查要不要按全局偏好检查**？
   建议：不检查。全局偏好多是工作方式（先审查再推送、先写文档），不是代码缺陷；拿它拦提交，误拦的代价高于收益。项目自己要强制的，写进项目规则。
8. **环境事实（P3）是否也走"确认后升为全局"的流程**？
   建议：不在本次做，另开文档。它们是可能过时的笔记，该进全局记忆而不是系统提示；去重要跨项目看记忆，是 dream 的问题。
9. **提取的 `SCOPE` 字段**：删除，还是保留为空操作？
   建议：删除，连同 `Extracted.scope` 和测试。它从来没起作用，留着会让模型误以为能写全局（AGENTS.md"删功能要删干净"）。
10. **现有全局记忆"用户语言偏好"**：迁进偏好文件并删除记忆条目，还是两边都留？
    建议：迁移并删除（先备份）。两边都留会重复注入，而且记忆那份是"笔记"语气，和偏好的"指令"语气矛盾。

## 决策记录

- 2026-11-23：决定 7 项建议一起做；下面没单独列出的待决策，一律按本文的建议执行。
- 2026-11-23：决定：语言偏好迁入偏好文件并删除原条目（先备份）；删除 `SCOPE` 字段，同时解决它被忽略的问题。
- 2026-11-24：实施中的小调整，原因如下：
  - **评测用例 1 换成 CHANGELOG 偏好**，仓库里不放 CHANGELOG.md。不预置偏好的对照组里，"同步更新 README"3/3 照样做到，"在已有 CHANGELOG 的 Unreleased 下记一行"也是 3/3，都区分不出好坏；仓库里没有 CHANGELOG.md、偏好要求"没有就新建"时，对照组 0/3。
  - **用例 2 的判定多一条**：用 `edit`/`write` 直接改了 README.md 判失败，即使结果碰巧和脚本输出一致（项目说明禁止的是手改这个动作）。为此 selftest 的返回值支持 `tools`，伪造 shell 以外的工具调用。
  - **"以后再说"在本次运行里不再问同一条**：只放回队列会让它在下一个输入提示前又被问一次。进程内记下推迟的 id，下次启动再问；`/rules pending` 仍可主动处理。
  - **"正在询问"的标记用 30 分钟租约**：标记里写进程的随机令牌和到期时间，正常退出时放回（`Preferences::shutdown`），异常退出的在租约到期后放回，不依赖检测进程是否存活。
  - **问题末尾加 `[y/n]`**：控制台按这个格式把 `y`、`n` 识别为单键作答，回车提交空答案即"以后再说"。
  - **审查不按全局偏好检查的做法**：偏好和项目说明在同一份系统提示里，审查子 Agent 也看得到；不另拼一份不带偏好的提示，而是在审查标准里写明 "User preferences" 一节不是审查依据。
  - **迁移的识别和边界**：按标题识别语言偏好（同时含"语言/language/中文/English…"和"偏好/preference/回复/reply…"一类词）；内容超过 200 字的不移动（偏好写入不截断），提示用户自己写；每个数据目录只迁移一次，备份目录下的 `migrated` 文件记录移动了什么。
  - **太长、被拒绝过或含本项目名字的候选**只记成项目规则，照常显示"记下一条项目规则"；进入队列的候选记成项目规则时不另打这一行，由提问说明。
  - `Services::start` 多了一个参数，按 `Dreamer::new` 的做法加 `#[allow(clippy::too_many_arguments)]`，避免新增 clippy 警告。

## 验证结果

改动的文件：`src/memory/rules.rs`（`ProjectRules` 泛化为 `RulesFile`，`Kind::Project` / `Kind::Preferences`；`promote`、`remove_matching`、`undated`）、`src/memory/preferences.rs`（新增：待确认队列、提议、提问时机、迁移）、`src/memory/extract.rs`（删 `SCOPE`，加 `---PREFERENCE---`）、`src/memory/recall.rs`（标题改为 "Global notes (this machine)"）、`src/memory/dream.rs`（提示词列出已有偏好）、`src/engine/prompt.rs`（"User preferences" 一节）、`src/app/mod.rs`（迁移、提问、`-p` 结束提示）、`src/commands/project.rs`（`/rules` 子命令和 `HELP`）、`src/commands/sessions.rs`、`src/safety/gates/review.rs`、`src/safety/gates.rs`、`tests/all/project.rs`；`evals/run.py`、两个新用例；`docs/reference.md`、`README.md`、`evals/README.md`。

删掉的：提取输出格式里的 `SCOPE` 行和"Automatically extracted entries are project-scoped…"一句、`Extracted.scope` 字段、`parse` 对 `SCOPE:` 的解析、测试 `automatic_global_proposals_remain_project_scoped`（改写为 `memories_stay_in_the_project_and_preferences_are_only_proposed`，并断言 `SYSTEM` 里没有 `SCOPE`）、类型名 `ProjectRules`（没有保留别名）。

- `cargo test --release`：库 460 个通过（6 个忽略），集成 227 个通过。新增的测试：
  - `memory::rules`：偏好的表头、20 条和 200 字上限（超长拒绝不截断）、密钥、手改表头保留；`promote` 先写全局再删项目、全局满时项目不变；日期后缀识别；偏好的 `task_note`。
  - `memory::preferences`：队列写入、去重、跨实例读取、30 天过期、拒绝过的不再入队；"正在询问"互斥、退出放回、租约到期放回；提问时机纯函数（有终端、空闲、没在输入、每次最多 3 条）；"以后再说"本次运行不再问、下次启动再问；提议记成项目规则并入队，含项目名或绝对路径时只记规则，不在项目里只入队，已是偏好的丢弃；语言偏好迁移（备份、写入、删除、只做一次）；没有语言记忆时只写标记。
  - `memory::extract`：三种块分开解析；候选写进项目规则并入队、全局记忆为空；含工作区目录名、绝对路径或已有偏好的候选不入队；提示词列出已有偏好。
  - `engine::prompt`：有偏好时 "# User preferences" 在 "# Project instructions" 之前并写明优先级，没有偏好时没有这一节，超过 8 KiB 截断并标注。
  - `memory::recall`：分层标题为 "Global notes (this machine)"，不再出现 "always apply"。
  - `memory::dream`：提示词列出已有偏好。`safety::gates::review`：审查标准写明偏好不是审查依据。
  - 集成测试 `preferences_reach_the_system_prompt_and_the_language_memory_moves_there_once`（`--dump-system-prompt`：迁移、备份、注入顺序、只迁一次）、`rules_lists_adds_removes_and_promotes_preferences`（`-p` 下的 `/rules`、`add global`、`promote`、`remove g1`、`pending`、`/help rules`）。
  - 子 Agent：主任务和子 Agent 用同一份 `project_context`（`commands/agents.rs` 传入），偏好一节随之带上；没有单独的测试。
- `cargo clippy --release --all-targets`：5 条，与改动前相同（console.rs:2157、console.rs:749、process.rs:138、policy.rs:600、tests/all/cli.rs:209）。
- `cargo build --release`：通过。
- `python -X utf8 evals/run.py --self-test`：全部通过（新用例 `global-preference-applies` 3 个 bad、2 个 good；`project-rule-beats-preference` 3 个 bad、2 个 good）。
- 只对改过的 .rs 文件逐个运行 rustfmt；`engine/prompt.rs` 里 rustfmt 还会改两处原有代码，没有采用，只格式化了本次新增的部分。
- 真实模型评测（deepseek，每个用例 3 次）：
  - 对照组（`--without-data`，不预置偏好）：README 偏好版 3/3、"已有 CHANGELOG"版 3/3（都区分不出，弃用）；最终的 `global-preference-applies` 0/3（都没有 CHANGELOG.md）；`project-rule-beats-preference` 3/3。
  - 预置偏好：`global-preference-applies` 3/3、`project-rule-beats-preference` 3/3（两轮共 6/6、6/6）。
  - 全量 `--repeat 3 --jobs 3 --baseline evals/results/20261124-092717-deepseek.json`（结果 `evals/results/20261124-103013-deepseek.json`）：52/57；原有 17 个用例 46/51，基准 50/51。没通过的 5 次看了现场，判定都对：explore-then-fix、scope-discipline 各 1 次留下了自己写的临时脚本（`probe_tmp.py`、`_check_total_tmp.py`），many-small-failures、unverifiable-tests 各 1 次改了测试，wide-trace-config 1 次把干扰层说成了原因；这些用例的系统提示和本次改动前逐字节相同（没有偏好时不加这一节，提取和审查只在任务结束后或提交时运行），同样的失败在更早的结果里也出现过。用最终的二进制把这 5 个用例和两个新用例重跑一轮（`evals/results/20261124-104647-deepseek.json`）：20/21，只有 wide-trace-config 1 次（回复把 `validate.py` 的上限说成原因之一）。同一批用例每次平均费用 $0.0071 → $0.0064。全量 $0.3575，重跑 $0.1209，对照组和用例调试共约 $0.09。
- 人工确认（release 版交互）：2026-11-24 补做了，见"验证结果"末尾的"交互界面验证"。"提取产生候选"和"`-p` 结束时的提示"两段没有在界面上跑到：自动提取对本机地址的服务是关闭的。界面上发现的两个问题（提问时输入的任务被吞掉、启动时连续问多条）记在 0046。

### 交互界面验证（2026-11-24）

环境：release 版 `cove.exe`（单独的 target 目录编译），伪终端（pywinpty，140×45）驱动交互模式，假的 OpenAI 兼容服务（只监听 127.0.0.1，`--provider openai --base-url http://127.0.0.1:端口/v1`），临时 `--data-dir` 和两个临时 Git 工作区 `ws`、`ws2`，不调用真实模型。

**候选的来源**：自动提取对本机和内网地址的提供方是关闭的（`extract::is_local`，`Services::start` 里 `auto_off = "local provider"`），假服务在 127.0.0.1 上，提取不会运行。所以没有走"提取 → 入队"这一段，而是：在 `ws` 里用 `/rules add` 写入两条项目规则（"改动尽量小，功能要完整""先给方案再动手"），再按 `Preferences::propose` 写出的格式在数据目录写 `preferences.pending.json`（两条候选，`rules` 指向 `ws/.cove/rules.md`，`rule` 是带日期的规则文本）。"提取 → 入队"由单元测试覆盖；`-p` 结束时的提示依赖提取入队，这次也没有看到。

**步骤和看到的界面**（屏幕内容去掉 ANSI 后）：

1. 在 `ws` 启动交互模式。启动信息之后、第一次读输入之前出现提问：

   ```text
   [偏好] 这条要求看起来不只适用于这个项目：
     改动尽量小，功能要完整
   在所有项目里都这样做吗？ y 记成全局偏好 · n 只留在本项目 · 回车 以后再说 [y/n]
   ```

   状态栏同时显示"等待你的回答"。
2. 输入 `y`、回车：

   ```text
   已记成全局偏好（…\data\preferences.md），所有项目都会遵守；项目规则里的同一条已删掉。/rules 查看或删除。
   ```

   `preferences.md` 出现 `- 改动尽量小，功能要完整（2026-11-24）`；`ws/.cove/rules.md` 只剩"先给方案再动手"；队列里这条已移除。
3. 紧接着的下一个输入提示前问了第二条（"先给方案再动手"）。这次输入的是一句任务文字"运行一下 echo"，显示 `[偏好] 以后再问；也可以用 /rules pending 处理。`，候选放回队列（`asking` 已清除）。
4. 重新启动交互模式：启动后又问"先给方案再动手"。输入 `n`、回车：`[偏好] 只留在项目规则里，以后不再问这一条。`。队列变为 `"candidates":[]`、`"declined":["先给方案再动手"]`；`preferences.md` 不变；项目规则里这一条还在。随后 `/rules` 显示：

   ```text
   全局偏好（…\data\preferences.md，所有项目都遵守，项目规则和你当前的要求优先）：
     g1. 改动尽量小，功能要完整（2026-11-24）
   项目规则（…\ws\.cove\rules.md，每个任务都遵守，提交前按它审查）：
     1. 先给方案再动手（2026-11-24）
   ```

5. 第三次启动：没有提问；`/rules pending` 显示"没有等待确认的全局偏好候选。"。
6. 在另一个工作区 `ws2` 启动：`/rules` 列出 `g1. 改动尽量小，功能要完整（2026-11-24）`，项目规则为空；`--dump-system-prompt` 里有 "# User preferences" 一节，`<preferences>` 中是这一条。

**结论**：提问的文字、时机（启动后和任务结束后、读输入之前）、`y` 写入全局并删掉项目里的同一条、`n` 不写入全局并记入已拒绝、回车或其他输入放回队列、下次启动再问、换项目生效，在 release 版界面上都和设计一致。没有覆盖：提取产生候选（本机地址不提取）、`-p` 结束提示、任务运行中和正在输入时不提问。

**发现的问题**（未修，另开文档处理）：

- 提问时输入的不是 y/n 的文字会被当成"以后再说"吞掉，不会作为任务执行。复现：队列里有候选时启动交互模式，提问出现后直接输入一句任务并回车，界面显示"以后再问"，假服务没有收到含这句话的请求。提问出现在输入提示的位置，用户没看到提问就开始打字时，这句任务就丢了。
- 队列里有多条候选时，回答完一条后，下一个输入提示前紧接着问下一条，用户还没执行任何任务就连着被问两次（每次启动最多 3 条的限制照常生效）。是否要求两次提问之间至少隔一个任务，需要定。

## 遗留

- release 版交互界面的人工确认（"验证"一节的三项）还没做：在演示仓库里说一句跨项目的要求，看输入提示前的提问、`y` 和 `n` 的结果、`-p` 结束时的提示、任务运行中不提问。
- 本机数据目录的迁移已经验证：cove 启动时把全局记忆里的语言偏好（User language preference）移进了 `preferences.md`，原条目备份在 `~/.cove/backups/0030-global-preferences/`，启动时显示了一行说明，`/rules` 列出为 g1。
- 从 P2 整理的 7 条候选，经逐条确认后已用 `/rules add global` 写入，现在是 g2–g8（改动小而功能完整、先审查再提交推送、文档按编号和样板写、先查官方文档、个人文档放 `.local/`、熟悉项目先动手跑、暂缓的优化只记文档）。
- 环境事实（P3）进全局记忆和跨项目去重，按"范围"另开文档。
- wide-trace-config 两轮共 4/6：模型偶尔把 `validate.py` 的上限也说成原因，判定是否过严需要单独看，不属于本文。
