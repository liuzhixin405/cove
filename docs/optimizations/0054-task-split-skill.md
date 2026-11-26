# 0054 内置 task-split 技能，计划执行按写集合约束单元

- 状态：已完成
- 日期：2026-11-26（创建）
- 关联：0053（覆盖层拆分：本文的 `writes`、`estimate_minutes` 是它调度用的写集合和预估）、`execute_plan`（`src/agents/plan.rs`）、`todo`（`src/tools/todo.rs`）、文件认领表（`src/agents/subagent.rs` 的 `Claims`）

## 背景

0053 决定先用一个通用 skill 解决"长任务等太久，要拆细并行、最后统一验证"：只要并行单元的写集合互不重叠，它们就能在同一个工作目录里跑，不需要隔离和合并。skill 已经写在 `~/.claude/skills/task-split/SKILL.md`，Claude Code、Copilot CLI 和 cove 都能读到。

但 skill 只能提醒模型，有三件事 cove 能在代码里保证，而且改动很小：

## 现状与问题

1. P1 **cove 不带这个技能**：它只在本机的 `~/.claude/skills` 里，别的机器上的 cove 没有。内置技能在 `assets/skills/`，由 `src/extensions/skills.rs` 用 `include_str!` 编进程序。
2. P2 **单元的写集合没有约束**：`todo` 条目只有 `id`、`depends`、`priority`、`status`。`execute_plan` 并行时，文件认领表只保证同一个文件不被两个正在运行的条目同时写（后来者等待），不知道每个单元"应该"写哪些文件；越界改动、并行单元写集合重叠，都要到最后才发现。shell 改的文件也不在认领范围内。
3. P3 **单元会跑生成文件的命令**：包安装、代码生成、格式化工具会改别的单元的文件，或者抢锁。
4. P4 **没有触发**：本机 `trace.jsonl` 里 `execute_plan` 用了 0 次；模型不会主动去拆。

## 目标

- 内置 `task-split` 技能，`/task-split` 可用。
- `todo` 条目可以申报 `writes`（写集合）和 `estimate_minutes`（预估）。申报了 `writes` 的单元，在 `execute_plan` 里**只能写申报的文件**；并行单元的写集合重叠时，计划在开始前就被拒绝。没申报的条目行为不变。
- 申报了 `writes` 的单元不能跑包安装、代码生成、格式化工具这类命令。
- 计划的校准预估超过 5 分钟、且有可以并行的条目时，提示模型用 `task-split`：先把拆分计划交给用户确认，再执行。
- 预估和实际耗时记下来，用于校准。

## 范围

- 做：内置技能；`todo` 的 `writes`、`estimate_minutes`；`execute_plan` 的重叠检查、越界写入拒绝、生成命令拒绝、shell 越界改动的报告；预估校准和提示；文档。
- 不做：
  - 覆盖层、合并引擎：在 0053 里做。本文的约束针对现有的主工作区并行；0053 完成后，技能的 cove 一节改为优先用 `isolation: "overlay"`。
  - 按符号（`文件::符号`）强制：只按文件强制，`::符号` 部分只用来展示和让模型自己遵守。
  - 自动拆分：拆分仍由模型按技能来做，cove 只做约束和提示。
  - 改动 `execute_plan` 在没有 `writes` 时的现有行为。

## 方案

### 1. 内置技能

- 新增 `assets/skills/task-split/SKILL.md`，内容以 `~/.claude/skills/task-split/SKILL.md` 为准；把其中"cove"一节改成用 `writes`、`estimate_minutes` 申报，并说明 cove 会强制约束。
- 在 `src/extensions/skills.rs` 注册；`docs/reference.md` 和 README 里内置技能数量随之更新。
- 同名覆盖：用户的 `~/.claude/skills/task-split` 会覆盖内置版本（现有的发现顺序）。这是预期行为，在 reference 里写明。

### 2. `todo` 的新字段

- `writes`：字符串数组，每项是工作区相对路径或 glob，可以带 `::符号` 后缀（强制时去掉后缀，只按文件）。也接受内容前缀 `writes:a.cs,b.cs `，规则同现有的 `depends:` 前缀。
- `estimate_minutes`：非负整数。
- 都是可选字段。`todo` 的展示里带上（例如 `[写 2 个文件，约 6 分钟]`），保持简短。

### 3. `execute_plan` 的约束（只对申报了 `writes` 的条目生效）

- **开始前的重叠检查**：两个条目之间没有依赖路径（互不为祖先）、而写集合有交集（同一个文件，或者 glob 能匹配到同一个已存在的文件、或者两个 glob 字面相同），整个计划拒绝执行，结果写明是哪两个条目、哪些文件，并提示"加 depends 或合并成一个条目"。
- **写入约束**：这个条目的子 Agent 调用 `write`、`edit`（以及其他会写工作区文件的工具），目标不在写集合里时拒绝，工具结果说明：`<path> is outside this plan item's writes (<列表>); stop and report why it needs changing instead of editing it`。
- **命令约束**：shell 拒绝包安装、代码生成、数据库迁移、格式化或自动修复类命令（复用 `classify::may_rewrite_sources`，再补一份列表：`npm/pnpm/yarn install|ci|add`、`dotnet restore|ef|tool restore`、`cargo fmt`、`go generate`、`protoc`、`prisma generate` 等，按 shell 方言解析），说明"这类步骤放到所有条目完成之后"。只读命令和普通检查命令照常。
- **shell 越界改动**：shell 命令执行后，如果检测到工作区里被它改了、又不在写集合里的受控文件（复用现有"shell 改了哪些文件"的检测，见快照/进度判断相关代码），不回滚，但记进这个条目的结果。
- **结果**：每个条目的结果加上"实际写入的文件"和"越界"两项；`execute_plan` 的汇总里列出所有越界。

### 4. 预估、校准和提示

- **记录**：任务结束时，如果本任务的 `todo` 里有 `estimate_minutes`，把"预估总分钟数、实际耗时、条目数、是否用了 execute_plan 并行"追加到 `<数据目录>/estimates.jsonl`（用 `persist` 的追加或原子写，按 AGENTS.md）。
- **校准系数**：最近 20 条的"实际/预估"中位数，限制在 0.5–4，没有记录时为 1。
- **提示**：`todo` 调用成功后，如果条目数 ≥ 2、校准后总预估 > `split_suggest_minutes`（配置，默认 5，0 表示关闭），并且至少有两个条目可以并行（互相没有依赖；申报了 `writes` 的，写集合不重叠），工具结果末尾加一段（英文，给模型）：校准后的预估、能并行的条目数，建议按 `task-split` 技能把拆分计划交给用户确认，再用 `execute_plan parallel=true`。同一个任务只提示一次；已经在 `execute_plan` 里的子 Agent 不提示。
- 配置 `split_suggest_minutes` 写进 `docs/reference.md` 的配置表。

### 5. 文档

`docs/reference.md`：内置技能列表和数量、`todo` 的新字段、`execute_plan` 的约束、`split_suggest_minutes`、`estimates.jsonl`；README 如有相关段落同步。

## 决策记录

- 2026-11-26（审查修复）：生成类命令里，脚本名和目标名按整词匹配：按 `:`、`-`、`_`、`.`、`/` 和小写到大写的边界切成词，和一张词表（gen、generate、codegen、format、fmt、fix、migrate、install、proto、restore、bootstrap、prettier 等）整词比较；名字里有 check/verify 一词，或脚本后的参数带 `--check`/`--dry-run`/`--list-different` 的，算只读检查。make/gradle/mvn/just/task 只看真正的目标：跳过选项、带值选项的值（`-C dir`、`-f file`、`-pl module`、`--tests pattern`、`-x task` 等）和 `NAME=value`。原因：子串匹配把 `agent`、`legend`、`prefix`、`prototype` 都当成生成类，误拒的条目算工具失败而且不重试。
- 2026-11-26（审查修复）：`regression` 在申报了 `writes` 的条目里**明确拒绝**（不按生成类命令处理成"放到之后再跑"之外的别的方式）：它会把文件换回基线再跑测试，同时运行的条目会看到被换掉的文件，事后也无法按写集合归属；条目报告里说明需要哪些测试，在全部完成后统一跑。`run_in_background` 的 shell 同样拒绝：调用返回后它还在写文件，前后两次 `git status` 查不到。
- 2026-11-26（审查修复）：shell 越界的归属：命令运行期间没有别的条目的 shell 在跑、也没有没申报写集合的条目在并行时，改动全部算这个条目的（改到别的条目申报的文件也报，写明是谁的）；否则记为"不确定"，进报告但不提醒子 Agent。原来把别的条目申报的文件一律放过，恰好漏掉了最该报的情况。
- 2026-11-26（审查修复）：校准只记真正执行过的计划：以 `execute_plan` 为单位，取这次完成的、带预估的条目，预估相加、各自最后一次运行的耗时相加（并行时也按串行工作量）。只做规划就结束的任务不记。旧记录没有 `executed` 字段，不参与校准。原来从 `todo` 计时到任务结束：出提示的任务把计划交给用户就结束了（实际零点几分钟），真正执行的任务不再调 `todo` 什么也不记，中位数被系统性压到 0.5。下面"校准只用正常结束的任务的记录……"一条作废。
- 2026-11-26（审查修复）：拆分提示除了"同一任务一次"，再加两条：所有未完成条目都申报了 `writes` 的计划不提示（已经按技能拆好，用户确认后的那一轮就是这样）；同一个会话里同一份计划（未完成条目的标题集合，不管顺序）只提示一次。
- 2026-11-26（审查修复）：`estimate_minutes` 接受小数，四舍五入到整分钟（schema 改为 `number`）；求和用 `u64`。不带 `/` 的目录写法（`src/agents`）表示它下面的所有文件：文件条目同时按路径前缀匹配（文件下面不可能有别的文件，所以不需要先判断它是不是目录）；目录条目和 glob 比较重叠时，按已存在的文件判断。
- 2026-11-26：写集合只按文件强制，不按符号。原因：按符号强制要解析每次编辑落在哪个符号里，误判风险高；文件级已经能挡住主要的越界和重叠。
- 2026-11-26：越界的 `write`/`edit` 直接拒绝，shell 造成的越界只报告不回滚。原因：文件工具能事前拦截；shell 只能事后发现，回滚可能丢掉有用的改动。
- 2026-11-26：只对申报了 `writes` 的条目生效。原因：不改变 `execute_plan` 现有用法的行为。
- 2026-11-26：触发做成"提示模型"，不由 cove 自动拆分。原因：拆分的质量取决于对代码的理解，交给模型按技能来做；cove 负责在合适的时机提醒和事后约束。
- 2026-11-26（实施）：重叠检查只在 `parallel=true` 时做。原因：串行执行的条目不会同时写同一个文件，拒绝串行计划只会添麻烦。
- 2026-11-26（实施）：因越界写入或生成类命令被拒绝而失败的条目不重试。原因：重试时同样会被拒绝，浪费一次运行；需要的是改计划。
- 2026-11-26（实施）：shell 越界改动用命令前后两次 `git status --porcelain -z --untracked-files=all`（加上每个文件的大小和修改时间）比较。原因：现有的快照检测（`Snapshots::tree_now`）只给出整棵树的哈希，看不出是哪些文件，而且子 Agent 的工具层拿不到快照对象；同一时间别的条目用文件工具写的文件、别的条目申报的文件不记在这个条目上。不是 git 仓库时不检查。
- 2026-11-26（实施）：校准只用正常结束的任务的记录；记录的预估取本任务第一次带预估的 `todo` 里未完成条目之和，实际耗时从那次 `todo` 算到任务结束。原因：中断的任务的耗时不代表做完要多久；从计划时刻起算才对应这份预估。
- 2026-11-26（实施）：写入约束放在子 Agent 的工具包装层（`ScopedTools`），不改 `write`/`edit` 本身。原因：只约束 `execute_plan` 的条目，其他调用方不受影响，也和 0053 对文件工具的改动互不冲突。

## 影响范围

| 文件 | 改动 |
|---|---|
| `assets/skills/task-split/SKILL.md`（新）、`src/extensions/skills.rs` | 内置技能 |
| `src/tools/todo.rs` | `writes`、`estimate_minutes`、前缀解析、展示、提示 |
| `src/agents/plan.rs`、`src/agents/subagent.rs` | 重叠检查、写入约束、命令约束、结果字段 |
| `src/engine/`（任务结束处） | 预估记录、校准 |
| `src/config/mod.rs`、`src/config/trust.rs` | `split_suggest_minutes` |
| `docs/reference.md`、`README.md` | 同步 |

## 风险

- glob 重叠只能近似判断：两个不同的 glob 可能匹配到将来才新建的同一个文件。只按"已存在文件的交集 + 字面相同"判断，在文档里写明。
- 命令约束靠列表，列不全：列表外的生成命令，靠"shell 越界改动"的事后报告兜底。
- 预估由模型给，早期没有历史时不准：用校准系数逐步修正，并在提示里写明是"校准后的预估"。

## 验证

- 单元测试：`writes`/`estimate_minutes` 的解析（字段和前缀）；重叠检查（同文件、glob、有依赖时允许）；校准系数（无记录、上下限、中位数）；提示的触发条件。
- 集成测试（假 provider）：
  - 申报了 `writes` 的条目写越界文件被拒绝、写申报内的文件成功；
  - 两个并行条目写集合重叠时计划被拒绝，加了依赖后通过；
  - 条目里跑 `npm install` 被拒绝；
  - shell 越界改动出现在条目结果里；
  - 没申报 `writes` 的计划行为不变（现有 execute_plan 测试照常通过）；
  - 预估超过阈值时 `todo` 结果带提示，低于阈值或 `split_suggest_minutes=0` 时不带；
  - `/task-split` 能列出和调用。
- `cargo test --release` 全量通过；`cargo clippy --release --all-targets` 不新增警告。

## 验证结果

- 实现：`src/agents/writes.rs`（新：`WriteSet` 写集合的解析、匹配和重叠判断，`overlaps`/`independent_pairs` 按依赖关系找能同时运行的条目，`PlanScopes`/`ItemScope` 是运行时的约束和记录）；`src/engine/estimate.rs`（新：`estimates.jsonl` 的记录、校准系数、拆分提示的 `Estimates` 钩子，只挂在主 Agent 上）；`src/agents/plan.rs`（`PlanItem.writes`/`estimate_minutes`、`writes:` 前缀、开始前的重叠检查、条目提示词、结果里的 `written:`/`outside writes:` 和汇总）；`src/agents/subagent.rs`（`ScopedTools.scope`：写入和命令在执行前拒绝，shell 前后比较）；`src/safety/permission/classify.rs`（`generates_files`）；`src/tools/todo.rs`、`src/tools/mod.rs`（只改了 `todo` 的 schema 和说明）；`split_suggest_minutes` 在 `src/config/mod.rs`、`src/config/trust.rs`（无需信任）和 `src/app/mod.rs`；内置技能 `assets/skills/task-split/SKILL.md` 只改了第六步的 cove 一段，在 `src/extensions/skills.rs` 注册。
- 新增单元测试（`cargo test --release --lib -- writes:: estimate:: todo:: plan:: generating`，20 个通过）：写集合的路径/glob/目录/`::符号` 和非法写法；重叠（同一文件不同符号、glob 对文件、字面相同的 glob、两个 glob 匹配到同一个已存在的文件；有依赖链时不算）；工作区相对路径的解析；shell 改动的归属（写集合内记为写入、别的条目申报的不算、其余记为越界）；`todo` 和 `execute_plan` 两边的字段、`writes:` 前缀（和 `depends:` 前后连用）、展示 `[writes 2, ~6 min]`、负数预估被拒；校准系数（无记录为 1、上下限 0.5/4、中位数、只取最近 20 条、中断的任务不算）；记录的追加和读回；提示条件（阈值、`0` 关闭、校准后降到阈值以下、依赖链、同文件、单条）；钩子同一任务只提示一次、新任务重新提示、任务结束写记录；`generates_files` 在三种方言下的正例和反例（`npm run dev`、`cargo test`、`black --check` 等不拒绝），`may_rewrite_sources` 判定的命令都被包含。
- 新增集成测试（假 provider，`cargo test --release --test all -- writes overlapping task_split execute_plan`，13 个通过，含原有的 `execute_plan_runs_the_todo_dag_in_parallel_and_passes_results_on`）：
  - `a_plan_item_with_writes_is_kept_to_them_and_reports_what_it_changed`：越界的 `write c.txt` 被拒、`a.txt` 写入成功、`echo x > gen.txt` 执行后在结果里提示越界、`npm install left-pad` 被拒；汇总列出三项越界；用前缀申报的第二条正常完成；失败的条目不重试。
  - `overlapping_writes_of_parallel_items_reject_the_plan_until_they_depend`：`src/x.rs::one` 和 `src/*.rs` 并行时计划被拒、没有条目运行；加 `depends` 后两条都完成，汇总为 `Outside writes: none.`。
  - `a_long_parallel_plan_suggests_task_split_once_and_records_its_estimate`：4+4 分钟带提示且只提示一次，2+2 分钟和 `split_suggest_minutes: 0` 不带；三种情况都写了 `estimates.jsonl`。
  - `the_builtin_task_split_skill_is_listed_and_runs_unless_the_user_has_one`：`/skills` 列出、`/task-split` 调用；`~/.claude/skills/task-split` 覆盖内置版本。
- `cargo test --release --no-fail-fast` 全量：lib 556 通过、1 失败、7 忽略；集成测试 246 通过、4 失败。失败的 5 个都是已知的偶发问题（`session::snapshot::tests::auto_deletes_only_files_the_snapshot_before_the_call_holds`、`cli::all_hook_events_run_with_scrubbed_environment_and_session_end_once`、`integrations::cancelled_and_timed_out_mcp_operations_kill_owned_descendants`、`core_tests::cancelled_shell_kills_its_spawned_child_before_late_writes`、`safety::files_changed_by_shell_are_progress_and_unchanged_ones_are_not`），`--test-threads=1` 重跑全部通过（`cli::all_hook_events…` 在四个一起串行时又失败一次，单独重跑通过）。没申报 `writes` 的计划走原来的路径，原有 execute_plan/team 测试不变并通过。
- `cargo clippy --release --all-targets`：只有原有的 5 条（`src/app/console.rs` 2 条、`src/process.rs`、`src/safety/permission/policy.rs`、`tests/all/cli.rs`）。
- 只对改过的文件跑 `rustfmt --edition 2024 --config max_width=100`，只采用落在改动行里的差异（`classify.rs` 里原有的未格式化数组没动）。

## 审查修复

### 问题和修复

1. **生成类命令按子串误判**（`src/safety/permission/classify.rs`）：`generating_name` 用 `contains`，"gen" ⊂ "agent"/"legend"、"fix" ⊂ "prefix"、"proto" ⊂ "prototype"；make/gradle/mvn 分支把所有不以 `-` 开头的参数都当目标，`-pl agent-core`、`--tests '*AgentTest*'`、`-C agents` 的值都被拿去匹配；`npm run format:check` 这种只检查的也被拒。被拒算工具失败，plan.rs 有拒绝记录时不重试，条目直接失败。修复：`name_words` 切词后整词匹配 `GENERATING_WORDS`，有 check/verify 一词或带 `--check`/`--dry-run`/`--list-different` 参数的算只读；`build_targets` 按各工具的带值选项跳过选项值，只看真正的目标；`npm run` 跳过 `--silent` 之类的选项找脚本名。
2. **确认后的那一轮又提示**（`src/engine/estimate.rs`）：每个新任务清空"已提示"，技能第六步用户确认后，新任务里带 writes/estimate 重写 todo 时又提示，形成循环确认。修复：所有未完成条目都有 `writes` 时不提示；同一个会话（按会话 id）里同一份计划（未完成条目标题的集合）只提示一次；同一任务一次的规则保留。
3. **校准系数被压低**：见决策记录。`estimate.rs` 提供 `plan_record`（纯函数）和 `record_plan`（从活动面板取每个完成条目最后一次运行的开始和结束时间，写进数据目录）；`src/agents/plan.rs` 里只在两处 `finish` 之前各加一行调用。主 Agent 的钩子不再记录。
4. **shell 越界检测漏报和误报**（`src/agents/writes.rs`）：`PlanScopes` 记下每个条目 shell 命令的起止时间（第二轮复审改为由 `ShellRun` 记录，见下），`with_unscoped_parallel` 标出并行计划里有没有没申报写集合的条目（plan.rs 建 `PlanScopes` 时传入）。归属规则见决策记录；报告里的写法：`b.rs (changed by shell; in the writes of todo-2)`、`x.rs (changed during its shell command; maybe by another item running then)`。`refuse` 里拒绝 `run_in_background` 和 `regression`，记进条目的被拒命令。
5. **低优先级**：`estimate_minutes` 求和改 `u64`（提示和记录两处）；`todo` 里的小数四舍五入（`engine::estimate::minutes` 和 `deserialize_minutes`，`todo` 工具和 `plan::parse` 共用）；目录写法按前缀匹配，`todo` 工具说明和 `docs/reference.md` 写明；mergiraf 的两处见 0053 的"审查修复（F2：合并引擎）"。

### 测试

- `safety::permission::classify::tests::generating_commands_are_found_in_every_dialect` 加了一组正反例：拒绝 `npm run generate`、`npm run gen:types`、`npm run --silent db:migrate`、`pnpm format`、`yarn lint:fix`、`make gen`、`make -C web fmt`、`make -j 4 proto`、`gradle :app:generateProto`、`./gradlew spotless flywayMigrate`、`mvn -pl core spotless:apply install`、`just codegen`、`npm install`、`go generate ./...`、`dotnet ef migrations add X`、`cargo fmt`、`prettier --write .`；放行 `mvn -pl agent-core test`、`mvn -Dtest=AgentTest -pl agents test`、`gradle test --tests '*AgentTest*'`、`./gradlew :legend:test`、`make -C agents test`、`make -f gen.mk test`、`make test PREFIX=/tmp`、`npm run test:agents`、`npm run prototype`、`npm run prefix-check`、`npm run format:check`、`npm run fmt -- --check`、`pnpm check-format`、`yarn legend`、`just test agents`；`name_words` 的切词。
- `engine::estimate::tests`：`the_hook_suggests_once_per_plan_and_session_and_records_nothing_itself`（同一任务一次；下一任务里同一份计划换了顺序不提示；条目都有 writes 不提示；另一份计划提示；新会话重新提示；钩子不写记录）；`an_executed_plan_records_its_estimated_items_as_serial_work`（4+6 分钟的条目跑了 5+9 分钟记为 10/14、没预估的条目不算、`u32::MAX` 相加不溢出）；`records_of_planning_only_tasks_from_before_do_not_calibrate`（旧格式记录不参与校准）。
- `agents::writes::tests`：`shell_changes_outside_are_recorded_with_the_items_that_declared_them`（改了别的条目申报的文件要报、标明是谁的）；`changes_while_another_item_may_be_writing_are_uncertain`（别的条目的 shell 同时在跑、或有没申报写集合的并行条目时记为不确定；对方的命令在本命令开始前已结束则不影响）；`background_shells_and_regression_are_refused_in_scoped_items`；`a_directory_without_a_slash_covers_the_files_below_it`。
- `tools::todo::tests`：`2.5` 显示为 `~3 min`，负数仍被拒。
- 集成测试 `a_long_parallel_plan_suggests_task_split_once_and_records_its_estimate` 改为：只规划的两种情况不写 `estimates.jsonl`，执行了 `execute_plan` 的情况写一条 `estimate_minutes` 为 8、`executed: true`、`parallel: true` 的记录。

### 第二轮复审

1. **连写的名字漏报**：整词匹配后，`reformat`、`autoformat`、`lintfix`、`dbmigrate`、`gentypes`、`mockgen`、`genproto`、`HTMLGen` 都不再被拒。另外名字里只要有 check/verify 就整体放行，`format-and-check`、`generate-and-check` 也放过了。修复（`classify.rs`）：整词没命中时，再按词干的前缀或后缀匹配（`STEMS`：format、fmt、gen、generate、migrate、migration、install、proto 可前可后，fix 只作后缀），每个词干带一张常见词的例外表（prefix、suffix、general、generic、oxygen、prototype、protocol 等）。check/verify 只对格式化类的词（含 format、fmt、prettier）起作用，而且必须紧挨着它或在同一个词里（`format:check`、`check-fmt`、`fmtcheck`）；`--check` 参数同样只对格式化类的词起作用。其他生成类词不受 check 影响（`gen:check`、`generate-and-check`），中间隔了别的词的也不算（`format-and-check`）。新增正例：`npm run reformat`、`npm run autoformat`、`yarn lintfix`、`npm run dbmigrate`、`npm run gentypes`、`make mockgen`、`make genproto`、`./gradlew HTMLGen`、`npm run preinstall`、`npm run format-and-check`、`npm run generate-and-check`、`npm run gen:check`。新增反例：`npm run fmtcheck`、`npm run check:prettier`、`npm run prefix`、`npm run general`、`npm run protocol-test`、`npm run test:AgentTest`。上一轮的反例全部保留。
2. **shell 运行记录没有结束时间**：原来在 `refuse()` 通过时就登记开始，命令之后被权限拒绝，或者结果没走到 `shell_changed`（出错、取消、第二次 `git status` 失败），记录就一直开着，其他条目之后的改动都被降为"不确定"。修复（`writes.rs` 和 `src/agents/subagent.rs` 的 `ScopedTools::scoped`）：在第一次 `git status` 之后、命令执行之前，用 `ItemScope::shell_run()` 登记，得到一个 `ShellRun`，它被丢弃时结束（覆盖完成、失败、`?` 提前返回、取消等所有路径）。用户拒绝的调用（`output.refused`）用 `ShellRun::void()` 撤掉这条记录，不再检查这次的改动。`refuse()` 不再登记。测试 `shell_runs_end_on_every_way_out`：通过 `refuse` 但随后被拒绝的命令、启动后失败的命令，都不会让后来的改动变成"不确定"；撤掉的记录即使在对方运行期间也不算。

### 验证结果

- 第二轮复审后再跑一次全量：lib 600 通过、0 失败、7 忽略；集成测试 261 通过、0 失败；clippy 仍只有原有 5 条；mergiraf 记录目录无残留。
- `PATH` 里带 mergiraf 跑 `cargo test --release --no-fail-fast` 全量：lib 597 通过、0 失败、7 忽略；集成测试 261 通过、0 失败（这次已知的偶发用例也都通过）。跑完 mergiraf 的 `merges/` 目录没有新增记录。
- `cargo clippy --release --all-targets`：只有原有 5 条（`src/app/console.rs` 2 条、`src/process.rs`、`src/safety/permission/policy.rs`、`tests/all/cli.rs`）。
- 只对改过的文件跑 `rustfmt --edition 2024 --config max_width=100`，只采用落在改动行里的差异（`classify.rs` 里原有的未格式化数组没动）。

### 遗留

- `timeout_seconds` 到点转入后台的前台 shell 仍然只检查到转入后台为止（转后台发生在 shell 工具内部，写集合层拦不住）。
- 条目耗时取自活动面板，只算最后一次运行；失败重试的那次耗时不计入，校准会略微偏低。
- 拒绝 `run_in_background` / `regression` 和拒绝生成类命令一样算条目的工具失败，条目不重试；子 Agent 按提示改用前台命令后，这次失败仍会让条目记为失败（和原有的拒绝规则一致）。

## 遗留

- 没有用真实模型跑过一次完整的 task-split 流程（拆分、确认、并行执行、收尾），提示的措辞和触发频率要在日常任务里观察；预估校准要攒够记录才有意义。
- 重叠检查只看已存在的文件：两个不同的 glob 将来新建的同一个文件看不出来。
- shell 越界检测依赖 git：不是 git 仓库时不检查；超时转后台的命令只检查到转入后台为止（`run_in_background` 已在审查修复里拒绝）；被 git 忽略的文件（如构建产物）不检查。
- 生成类命令靠列表，列表外的命令（比如自定义脚本里调用代码生成）只能靠事后报告；`npm run`/`make` 的脚本按名字猜（审查修复后按整词：名字里有 gen/format/fix/migrate/install 等词）。
- 自定义子 Agent 类型如果带了 MCP 之类会写文件的工具，这些工具不受写集合约束（只认 `write`、`edit`、`draw_image` 和 `shell`）。
- 0053 完成后，技能的 cove 一段要改为优先用 `isolation: "overlay"`；0053 可以直接用 `PlanItem::write_set()`、`writes::overlaps`/`independent_pairs` 和 `engine::estimate::factor` 做调度和触发。
