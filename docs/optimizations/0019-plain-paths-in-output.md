# 0019 输出里的路径去掉 `\\?\` 前缀

- 状态：已完成
- 日期：2026-11-20（创建）；2026-11-20（完成）
- 关联：[0018](0018-demo-polish.md)（实测 `/cost` 时发现，见其"遗留"）

## 背景

实测 `/cost` 时，最后一行显示 `账本：\\?\C:\Users\Lenovo\.cove\usage`。用户要求修掉。

## 现状与问题

- 数据目录（`session/mod.rs` 的 `Store::open`）和工作区（`config/mod.rs` 的 `Settings`）都做了 `canonicalize`。在 Windows 上，它返回带 `\\?\` 前缀的长路径形式。
- 项目里已经有 `paths::display`，专门把这种路径转回用户熟悉的写法，标题栏用的就是它。但很多输出点直接用了 `Path::display()`，前缀就漏了出来。
- release 版实测，用真实数据目录跑一遍会打印路径的只读命令，带前缀的输出有：
  - `/cost`：`账本：\\?\C:\…\usage`；
  - `/doctor`：`data_dir \\?\C:\Users\Lenovo\.cove 可写`、`policies \\?\…\policies.json 解析正常`；
  - `/status`：`工作区 \\?\D:\cove-demo\order-service`；
  - `/skill <名字>`、`/config skills list`：`"source": "\\\\?\\C:\\…\\SKILL.md"`；
  - `/replay`：`会话回放已生成：\\?\…`；`/resume export`：`对话记录已导出到 \\?\…`。
- 代码里同类的还有：`/cd`、导出 CSV、dream 备份、后台 Agent 和 worktree 的路径等。

## 目标

所有给人看的路径都不带 `\\?\` 前缀。

## 方案

- 把给人看的 `.display()`（`format!`、`say`、诊断项、错误信息、交给模型的工具结果、技能列表的 JSON）换成 `crate::paths::display(&path)`。它对普通路径原样返回，所以换上不会改坏输出。
- 不改内部用 canonicalize 的做法。`app/mod.rs:56`、`commands/project.rs:553`、`commands/sessions.rs:914` 用 `Path::starts_with` 拿数据目录和 canonicalize 过的路径比较；Windows 上 `\\?\D:` 和 `D:` 是不同的前缀分量，源头去掉前缀会让这些比较失效，"数据目录在工作区里时保护它"这条逻辑就会漏掉。
- 交给 git 或拼进命令行的路径不动，那些已经按需用 `paths::plain`。

## 范围

不做：统一内部路径形式（源头去掉前缀）。要做得先审一遍所有路径比较，收益只是少写几次 `paths::display`。

## 影响范围

只改输出文字。涉及 `src/` 下各命令、诊断、技能、后台 Agent 等模块中给人看的 `.display()`。

## 验证

- 全量 `cargo test`。
- release 版重跑上面那组命令，输出里搜不到 `\\?\`。

## 实施计划

- [x] 逐个检查 `.display()`，换掉给人看的
- [x] 测试、格式化、实测

## 待决策

无。

## 决策记录

- 2026-11-20：用户要求修。（用户）
- 2026-11-20：在输出处修，不在源头修，原因见"方案"。（实施方）

## 验证结果

- 共 132 处 `.display()`，换掉 107 处，另把技能和插件列表 JSON 里直接序列化的 `source` 路径（5 处）也换成 `paths::display`。
- 没换的 25 处：
  - 持久化的键：`config/trust.rs` 的 `key`、`policy.rs:364` 的规则作用域，改了会让已信任的项目和已有规则失效；
  - 规则匹配：`policy.rs:85`；
  - git 参数和审批里的命令文本：`commands/extensions.rs` 的 clone/pull；
  - 相对路径（不会带前缀）：`session/workspace.rs` 的改动列表、`onboarding.rs`、`regression.rs:517`、`engine/prompt.rs:282`、`guard.rs:106`、`paste.rs`；
  - `main.rs`：命令行传入的路径，没有经过 canonicalize；
  - `plugins.rs` 的一个测试：它把本地路径当 git URL 传。
- 系统提示里的 `Working directory` 也换成了普通路径，模型看到的是 `D:\cove-demo\order-service`。
- 全量 `cargo test --release` 通过（393 + 216）；clippy 仍是原有的 5 条。替换时产生的 42 处多余 `&` 和 1 处多余闭包已改掉。
- release 版用真实数据目录重跑 `/cost`、`/status usage`、`/doctor`、`/skills`、`/skill commit`、`/status`、`/cd`、`/undo`、`/replay`、`/resume export`、`/config skills list`、`/plugins`，输出里 `\\?\` 出现 0 次（修改前 15 次）。

## 遗留

- 以后新加的输出如果直接用 `.display()`，还会漏出前缀。没有加 lint 规则：clippy 的 `disallowed_methods` 会连内部合法的用法一起拦下。代码评审时注意即可。
- 内部路径形式没有统一（见"范围"）。
