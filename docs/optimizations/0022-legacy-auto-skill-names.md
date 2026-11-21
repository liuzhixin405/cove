# 0022 旧式 auto 技能的非 ASCII 名字按生成名加载

- 状态：已完成
- 日期：2026-11-20（创建）；2026-11-20（完成）
- 关联：[0003](0003-remove-skill-auto-learning.md)（去掉技能自动学习）、[0006](0006-remove-budget.md)（去掉预算）

## 背景

用户贴出启动日志，要求把下面的警告清理掉：

```text
警告：config：已忽略 cove 不支持的键：profiles.bedrock.max_budget_usd
[extensions] skills: C:\Users\victor\.cove\skills\auto-b5ee20\SKILL.md not loaded: tool: invalid skill name "编译冲突排查"
[extensions] skills: C:\Users\victor\.cove\skills\auto-cove-28f6e5\SKILL.md not loaded: tool: invalid skill name "Cove 提交门禁排查"
…（共 7 条 auto-* 技能）
```

第一条是用户配置里残留的 `max_budget_usd`（0006 已删除预算，配置里的旧键应直接清理），按 0006 处理：备份后删掉该键，`cove --doctor` 确认配置正常加载。剩下的是技能加载的警告，是本次要改的代码问题。

## 现状与问题

**0003 删掉了自动学习，也删掉了 `auto-` 前缀的专门处理，但旧数据还在。**

- 0003 决定把 `~/.cove/skills/auto-*` 旧式自动技能移到备份目录，并删除 `Origin::Auto` 与 `auto-` 前缀的专门处理（`docs/optimizations/0003-remove-skill-auto-learning.md:41、:56`）。本机随后又恢复了这些文件（`~/.cove/skills/` 下现有 8 个 `auto-*` 目录，其中 7 个的 frontmatter `name` 是非 ASCII 中文）。
- `load_file` 对每个技能先取 `parsed.name`，再用 `identifier` 校验（`src/extensions/mod.rs:23` 只允许 ASCII 字母数字、`-`、`_`）；非 ASCII 名字直接返回 `invalid skill name` 并丢弃技能，于是每次启动都刷警告。

**要修的是加载时的名字校验，不是把技能删掉。** 这些文件是旧版本生成的技能，仍有用户写的说明内容；删除用户数据比按规则加载风险更大。项目规则 `.cove/rules.md` 也写明：非 ASCII 名字的 `auto-*` 技能按目录名加载，严格校验只针对非 auto 技能。

## 目标

- 启动时不再出现 `skills: … not loaded: tool: invalid skill name …` 这类警告；
- 旧式 `auto-*` 技能按生成名（`auto-*/SKILL.md` 用目录名，平铺 `auto-*.md` 用文件主干名）加载，内容仍可用；
- 非 `auto-*` 技能继续走严格校验，一个都不能放宽。

## 范围

- 做：
  - `src/extensions/skills.rs` 的 `load_file`：当 frontmatter 名字缺失或不是合法标识符，且生成名以 `auto-` 开头、本身是合法标识符时，用生成名加载；
  - 新增单元测试，覆盖 `auto-*/SKILL.md` 和平铺 `auto-*.md` 两种布局。
- 不做：
  - 不恢复自动学习、`Origin::Auto`、候选库等 0003 已删的功能；
  - 不删除磁盘上的旧技能文件；
  - 不放宽非 `auto-*` 技能的名字校验；
  - 配置清理（删 `max_budget_usd`）不写进代码，按 0006 直接改用户配置。

## 方案

`load_file`（`src/extensions/skills.rs:633-655`）里，`fallback` 本来就是"生成名"：

- `SKILL.md` → 父目录名；
- 平铺 `*.md` → 文件主干名。

所以 `fallback` 同时覆盖了两种旧布局。校验改成：

1. `name = parsed.name`，缺省时用 `fallback`；
2. 若 `name` 不是合法标识符：
   - 若 `fallback.starts_with("auto-")` 且 `fallback` 是合法标识符，用 `fallback` 作为名字；
   - 否则维持原样：返回 `invalid skill name`，丢弃技能。

这样不改任何数据结构，也不引入新常量；唯一的行为变化是把"旧 auto 技能的非 ASCII 名字"从"报错丢弃"改成"按生成名加载"。

## 影响范围

- 文件：`src/extensions/skills.rs`（`load_file` 与一个测试）；`docs/optimizations/0003-remove-skill-auto-learning.md`（把"不再特殊处理"的表述改成"只保留名字回退"）；`docs/optimizations/README.md` 索引。
- 行为：7 个非 ASCII 名字的旧 auto 技能重新出现在技能索引里（按 `auto-*` 生成名），系统提示会多出约 600–700 token，和 0003 之前一样；非 auto 技能行为不变。
- 兼容：没有新增配置键，没有数据格式变化。

## 验证

- 新增单元测试：`legacy_auto_skills_with_unicode_names_load_under_their_generated_name`，分别写 `auto-compile/SKILL.md`（目录布局）和 `auto-deploy.md`（平铺布局），断言两个技能按生成名加载、`warnings` 为空、正文可渲染。
- `cargo test --lib` 全部通过。
- `cargo clippy --all-targets` 不新增警告。
- release 版在独立工作目录跑 `--dump-system-prompt`，确认没有 `not loaded` / `invalid skill name` 输出，且 auto 技能按生成名出现在技能列表。

## 实施计划

- [x] 改 `load_file` 的名字回退
- [x] 新增覆盖两种布局的单元测试
- [x] 写本文档并回填 0003 的表述
- [x] 测试、clippy、release 版实测

## 待决策

无。

## 决策记录

- 2026-11-20：按生成名加载，而不是删磁盘文件。理由：项目规则 `.cove/rules.md` 明确要求按目录名加载；这些文件仍有用户写的说明内容，删除是范围外且有风险。（实施方）
- 2026-11-20：回退只认 `fallback.starts_with("auto-")`，覆盖目录和平铺两种旧布局；不用 `file.parent()`，因为平铺文件的父目录是 `skills`，会漏判。（实施方，提交前审查指出）

## 验证结果

- `cargo test --lib`：404 passed / 0 failed / 4 ignored。
- `cargo clippy --all-targets`：仍是原有 5 条警告（4 条 lib + 1 条 `tests/all/cli.rs`），`src/extensions/skills.rs` 没有新增警告。
- `rustfmt --edition 2024 --config max_width=100 --check src/extensions/skills.rs`：通过。
- release 版（`cargo build --release --target-dir target/alias`）在独立目录 `C:\Users\victor\AppData\Local\Temp\cove-verify` 跑 `--dump-system-prompt`：退出码 0，输出里没有 `not loaded` / `invalid skill name`；技能索引里出现 `auto-b5ee20`、`auto-cove-28f6e5` 等生成名。
- 用户配置：`profiles.bedrock.max_budget_usd` 已删除，`--doctor` 显示 `[ok] config 已加载 C:\Users\victor\.cove\config.json`；两个含密钥的 `config.json.bak-*` 已删除。

## 遗留

- 本机 `~/.cove/skills/` 下仍保留这些旧 auto 技能文件（本次特意不删）；如果用户不再需要，可手动删除目录，删除后对应技能自然从索引消失。
