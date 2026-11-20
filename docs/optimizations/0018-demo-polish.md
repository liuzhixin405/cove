# 0018 路由提示改成人话、回复跟随用户语言、加 /usage

- 状态：已完成
- 日期：2026-11-20（创建）；2026-11-20（完成）
- 关联：[0015](0015-readme-rewrite.md)（录制 README 演示时发现，见其"遗留"）

## 背景

README 演示 GIF 里暴露了两处观感问题，加上 0015 遗留的 `/usage` 别名，用户回复"要"，三件一起做。

## 现状与问题

1. **路由提示像调试输出。** 每个交给快速模型的任务开头显示一行黄色：
   `! [model] anthropic.claude-sonnet-5-5 (score 0.01 [length(38)+0.01])`
   （`model/routing.rs` 的 `begin_task`）。打分明细对用户没有意义，用户只需要知道"这次用的是快速模型"。
2. **工具调用之间冒英文。** 演示里用户全程中文，模型在编辑前写了一句 "Off-by-one boundary: > should be >="。系统提示（`engine/agent.rs` 的 `system_prompt`）只有一大段英文规则，没有一句说该用什么语言回复，模型在短句里容易跟着英文提示走。
3. **`/usage` 报"未知命令"。** 用量在 `/status usage`，但其他 agent 都叫 `/usage`、`/cost`，用户顺手就打这两个。

## 目标

- 路由提示一眼看懂，不带打分细节；
- 回复（包括工具调用之间的短句）跟用户的语言一致；
- `/usage`、`/cost` 能直接用，补全里看得到。

## 方案

1. 路由提示：
   - 选了快速模型：`简单任务，交给快速模型 <模型>`；
   - `/model next` 指定：`本任务用 <模型>（/model next 指定）`。
   - `classify` 只返回是否用高级模型；打分理由字符串只给这一行用，一并删掉。打分规则本来就写在 reference.md 里。
2. 系统提示加一句：`Reply in the language the user writes in, including short notes between tool calls.`。不写死中文：用户用英文提问时照样回英文。
3. 把 `config_shortcut` 扩成通用的简写表：在原来 `/mcp`、`/plugins`、`/hooks`、`/memory` → `/config …` 的基础上，加 `/usage` → `/status usage`、`/cost …` → `/status cost …`。帮助里加两行"同 /status usage / cost"，补全就能列出来（补全读的是帮助行）。

## 范围

不做：通知行的颜色和样式不改（`!` 黄色仍用于所有 Notice，包括升级、回退这类真警告）。

## 影响范围

- `src/model/routing.rs`、`src/engine/agent.rs`、`src/commands/mod.rs`、`src/commands/sessions.rs`、`src/app/mod.rs`；
- 集成测试 `tests/all/model.rs` 里断言 `[model] fast-model` 的地方；
- `docs/reference.md` 的路由说明和命令表；README 的日常操作表。

## 验证

- 单元测试：`classify` 的判断不变；简写表把 `/usage`、`/cost 30d` 换成对应命令，`/usagex` 不匹配。
- 集成测试改为断言新提示文字。
- 全量 `cargo test`；release 版手动打 `/usage`、`/cost`。

## 实施计划

- [x] 路由提示
- [x] 系统提示
- [x] 简写表和帮助
- [x] 文档、测试、格式化

## 待决策

无。

## 决策记录

- 2026-11-20：三项都做。（用户）
- 2026-11-20：语言规则写成"跟随用户语言"而不是"用中文"，不影响英文用户。（实施方）

## 验证结果

- 全量 `cargo test --release` 通过（393 + 216）；clippy 仍是原有的 5 条警告，没有新增。
- `classify` 原有 8 条断言不变，全部通过；新增简写断言：`/usage`、`/cost 30d` 换成对应命令，`/usagex` 不匹配。
- 集成测试 `simple_tasks_route_to_the_fast_model_and_complex_ones_to_premium` 断言新提示"简单任务，交给快速模型 fast-model"。
- release 版实测：`/usage` 输出本会话账户和用量，`/cost today` 输出今天的账单；打 `/u` 时提示"你是不是想输入 /undo、/usage、/cd"。
- 2026-11-20 重录 README GIF 时全程没有出现英文短句，路由行也已换成新的中文提示（`简单任务，交给快速模型 deepseek-v4-flash`）；新 GIF 见 `docs/assets/demo.gif`。

## 遗留

- 已重录：2026-11-20 用优化后的 release 版重新录制 README 演示 GIF，新路由提示和中文回复都已生效。
- 实测时发现 `/cost` 末尾的账本路径显示为 `\\?\D:\cove-demo\data3\usage`（带 Windows 长路径前缀），属于另一个问题，已在 [0019](0019-plain-paths-in-output.md) 修复。
