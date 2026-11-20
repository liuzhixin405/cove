# 0003 去掉技能自动学习

- 状态：已完成
- 日期：2026-11-19（创建）；2026-11-20（完成）
- 关联：功能使用情况分析（2026-11-19 对话记录）；[0001](0001-unified-session-history.md)

## 背景

用户在功能使用分析后决定："先把一个大的给干掉吧，技能自动学习，因为有了自动记忆"。

## 现状与问题

"技能自动学习"指的是：每个交互任务完成后，后台模型复盘这个任务，提出"可复用的做事步骤"作为技能候选；候选满足条件后自动启用，长期不用再自动退役。

证据（`~/.cove`，2026-11-19）：

**P1 从来没有起过作用。**
- 候选库 `skill-candidates/records.json` 里有 17 条记录，**全部是待审批状态**（`candidate`），没有一条被批准过。而技能加载时只收已批准的候选（`extensions/skills.rs` 中加载候选的那段），所以从来没有一个学到的技能进入模型的技能列表。
- 105 个会话和 14 个归档里，`skill` 工具一共调用 7 次，加载过的只有用户自己写的 `sonarqube-new-code`、`write-adr`、`choose-code-search`，没有一次加载自动技能。
- 学到的内容都是一次性的具体事件，比如"诊断本地数据库端口冲突"，很难再遇到第二次。

**P2 一直在花钱，还占用每一次请求的提示空间。**
- 每个满足条件的任务结束后，都要多一次后台模型调用（`skill_review`）。
- 更早版本直接写成文件的 8 个旧式自动技能（`~/.cove/skills/auto-*`，`Origin::Auto`）一直被当成普通技能加载，每次请求都会出现在系统提示的"可用技能"列表里，约 600–700 token。
- `usage/intents/` 里还留着 1 个中断的技能复盘意图，账本里因此一直显示一笔"未知费用"。

**P3 与自动记忆重复。** 自动记忆提取在正常工作（memory 工具在 19 个会话里调用 100 次）；dream 记忆整理的提示里，本来就有"把反复出现的'问题 → 尝试 → 失败 → 解决'模式提炼成'规则和模式'记忆"这一步（`memory/dream.rs`），覆盖了自动技能想做的事。

**P4 代码量大，和技能系统深度耦合。** `skill_review.rs`（1304 行）和 `skill_candidates.rs`（1823 行），再加上 `skills.rs` 里的候选内容类型、加载、迁移、审批逻辑，`/skills candidates` 的 7 个子命令，两个配置项，以及费用账本里的意图机制。

## 目标

1. 不再有任何后台技能复盘调用；系统提示里不再出现自动技能。
2. 技能系统本身不受影响：用户写的技能、内置技能、`/skills`、`/skill <名字>`、`skill` 工具照常可用。
3. 代码减少约 3000 行以上，`cargo test` 全部通过。

## 范围

- 做：
  - 删除 `src/extensions/skill_review.rs`、`src/extensions/skill_candidates.rs`；
  - `skills.rs`：删除候选内容类型（`Body::Candidate`）、候选加载、`auto_learn`、学习、审批、迁移、旧式自动技能（`Origin::Auto` 和 `auto-` 前缀）的专门处理；
  - `commands/extensions.rs`：删除 `/skills candidates` 及其子命令、技能复盘钩子的注册、帮助文字；
  - 配置：彻底删除 `skill_review`、`skill_review_auto` 两项（已知字段、安全分类、文档说明）；写了它们的配置会得到"未知字段"警告（见决策记录）；
  - 费用账本：删除只为技能复盘服务的意图机制（`usage/intents`）以及 `skill_review` 这个来源；
  - `contains_secret`（输入历史的敏感信息过滤也在用）挪到 `skill_review` 以外的模块，功能不变；
  - 文档：`docs/reference.md` 中相关的各节；
  - **数据**：把下面这些**移动到备份目录** `~/.cove/backups/0003-skill-auto-learning/`（不直接删除，需要的话可以搬回去）：8 个 `skills/auto-*` 目录、`skill-candidates/`、`usage/intents/`、`usage/skill-review.lock`，以及早就没有代码写入的 `skill-usage.json`、`context-usage.json`。
- 不做：
  - 不改技能系统的其他部分（技能的格式、来源、内置技能、插件技能）；
  - 不改自动记忆和 dream。

## 方案

1. 先移动数据，并确认 `/skills` 列表里只剩用户技能和内置技能。
2. 删除两个模块，再按编译错误逐个清理引用：
   - `skills.rs`：`Body` 只保留文件类型；删除候选加载那段、`automatic()`、`candidate_store()`、`learn`、`decide_candidate`、`migrate_legacy` 及相关测试；`Origin::Auto` 删除，`auto-` 前缀不再特殊处理；
   - `commands/extensions.rs`：`turn_hooks` 只保留 `SkillNotes`；`configure` 删除 `["skills", "candidates", ..]` 各分支；子命令提示里删除 `candidates`；
   - `session/usage.rs`：删除意图的写入、读取和"未完成按未知费用显示"的逻辑；
   - `config/mod.rs` 和 `config/trust.rs`：删除这两个配置项；
   - `contains_secret` 挪到 `safety` 模块。
3. 只对改动过的文件，通过标准输入运行 `rustfmt`。

## 影响范围

- 行为：任务完成后不再出现 `[skills] 已自动启用技能…` 这类提示；`/skills candidates …` 命令不再存在（会提示未知命令）；费用账本不再有 `skill_review` 记录。
- 数据：见"范围"中的数据一项，全部移到备份目录。
- 兼容：配置里如果写了 `skill_review`、`skill_review_auto`，启动时会提示"ignoring keys not supported by cove"，不影响使用（核查时没有任何配置写了这两项）。旧账本里已有的 `skill_review` 记录仍然可以读取，并单独显示。

## 验证

- `cargo test` 全部通过；删掉的测试只限于两个模块本身，以及 `skills.rs` 里关于候选和旧式自动技能的测试。
- `rg "skill_review|skill_candidates|Body::Candidate|Origin::Auto"` 在 `src/` 里只剩账本单独显示旧记录的那一处（`session/usage.rs`）。
- 用 `cove --list-sessions` 这类不调用模型的命令确认程序能正常启动；人工确认 `/skills` 列表里只有用户技能和内置技能。

## 实施计划

- [x] 数据移到备份目录
- [x] 删除模块并清理引用
- [x] 文档
- [x] 测试、格式化，回填验证结果

## 待决策

无。

## 决策记录

- 2026-11-19：去掉技能自动学习，理由是自动记忆已经覆盖，而且它从来没有起过作用。（用户）
- 2026-11-19：数据移到备份目录，不直接删除。理由：需要时可以撤回。（实施方）
- 2026-11-19：撤销"两个配置项保留为废弃、忽略、不报警告"这个决定，改为彻底删除（`config/mod.rs` 的已知字段、`config/trust.rs` 的安全分类、`reference.md` 的说明）。理由：核查后发现，用户配置和各项目的 `.cove.json` 里都没有这两项，保留它们谁也没帮到；静默失效会让人以为设置还在起作用，而"ignoring keys not supported by cove"的警告反而如实说明了情况；以后每删一个功能都这样保留，已知字段列表就会和实际功能对不上。（用户追问"为什么有意保留"之后，实施方核查并修正）
- 2026-11-20：用户确认 0003 已完成，遗留直接删除：账本里 `skill_review` 的专门处理和备份目录。（用户）
- 2026-11-19：账本按来源单独显示旧 `skill_review` 记录的逻辑暂时保留。理由：账本里有 77 条这种记录，费用全部未知；如果并进普通行，会把真实任务的费用搅混。账本只保留 13 个月，这些记录约在 2027-11 全部过期，到时删除这段逻辑，代码旁已注明。（实施方）
- 2026-11-20 收尾核对（[0014](0014-audit-fixes.md)）：遗留的 `Skills::scope_key` 和技能列表里 `legacy-unverified` 的判断没有删干净，已在 0014 删除；`reference.md`、`design.md` 里"技能复盘"的说明也已删除。

## 验证结果

- **代码**：`src/` 从 76738 行减少到 72479 行，少了 4259 行。其中删除两个模块共 3127 行，`skills.rs` 从 2185 行减到 1324 行，`/skills candidates` 命令和费用账本的意图机制各约 100 行。测试代码随之删掉 2 个集成测试和 41 个单元测试，都只检验被删除的功能。
- `rg "skill_review|skill_candidates|Body::Candidate|Origin::Auto|AUTO_PREFIX" src`：只剩账本里按来源显示旧 `skill_review` 记录的代码（`session/usage.rs`，旁边注明了约 2027-11 可以删除）。
- `contains_secret` 挪到了 `src/safety/secrets.rs`，正则表达式原样不变，并新增了一个单元测试；输入历史的敏感信息过滤照常工作。
- `cargo test`：单元测试 366 个全部通过；集成测试 222 个。第一次全量运行时，`all_hook_events_run_with_scrubbed_environment_and_session_end_once` 偶发失败（0002 遗留里记录过），单独运行连续两次都通过；删除配置项之后再次全量运行，222 个全部通过。
- 只对改动过的文件，通过标准输入运行了 rustfmt；`cargo build --release` 完成，程序体积从 45.91MB 减到 45.48MB。
- **数据**：`~/.cove/skills/` 下只剩用户技能 `choose-code-search`、`write-adr`。8 个旧式自动技能、`skill-candidates/`、`usage/intents/`、`usage/skill-review.lock`、`skill-usage.json`、`context-usage.json` 都移到了 `~/.cove/backups/0003-skill-auto-learning/`。
- **待人工确认**：`/skills` 列表里只有用户技能和内置技能；任务完成后不再出现 `[skills]` 提示。

## 遗留

- ~~备份目录 `~/.cove/backups/0003-skill-auto-learning/`~~：2026-11-20 已删除（用户）。
- ~~约 2027-11 删除账本里 `skill_review` 的专门处理~~：2026-11-20 提前删除（用户："遗留的直接删掉"）。账本里那 77 条旧记录现在和同一模型的其他调用合并成一行显示，它们的费用是未知的，所以那一行会标"未知"，不会被当成零。
- 集成测试 `all_hook_events_run_with_scrubbed_environment_and_session_end_once` 偶发失败（已知问题，见 0002 遗留）。
