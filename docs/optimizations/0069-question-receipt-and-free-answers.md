# 0069 提问回执显示答案；用自己的话回答后，其余问题照常提问

- 状态：已完成
- 日期：2026-11-28（创建）；2026-11-28（完成）
- 关联：0065（需要用户回答时用 question 工具）

## 背景

0065 之后，`question` 工具会用得更多。两处问题跟着变得显眼：回执看不出用户答了什么；一次提了几个问题时，第一个问题用自己的话回答，其余问题就被丢掉。

## 现状与问题

1. **回执只显示问题，不显示答案。**
   - 选完后，终端显示 `● 提问 └ [Q1] 接下来怎么走？ … 共 2 行`，看起来像又被问了一次。
   - 原因：折叠行的标题固定为"提问"，没有说明文字（`src/app/render.rs:91`）。摘要走通用分支，只显示输出的第一行（`render.rs:455-461`）。工具输出是 `[Qn] 问题\nAnswer: 选择`（`src/tools/question.rs:135-139`），答案在第二行，被折叠掉了。
2. **用自己的话回答后，其余问题被丢弃。**
   - 带选项的问题，用户没选编号、写了自己的话，而且后面还有问题时，工具记下"Q2..Qn were not asked"，然后 `break`（`src/tools/mod.rs:1076-1083`）。
   - 判定条件（`question.rs:95-107`）是：有选项，回答非空，并且不全是合法编号。随手补一句话也会让后面的问题全部作废。
   - 原设计的理由是：自由回答可能改变后面该问什么。
3. **单选题的输入提示是英文**（`question.rs:85`，"Answer with a number or your own text"）。多选题的提示是中文，界面其他地方也是中文。

## 目标

- 回执一眼能看出答了什么。
- 其余问题由用户决定答不答，不再被工具替用户丢掉。

## 范围

- 做：问题 1~3。
- 不做：提问界面的交互方式（比如一键跳过剩下所有问题）。回车跳过单个问题已经可以做到，够用了。

## 方案

1. **回执**：`render.rs` 的摘要 `match name` 加一个 `"question" | "ask_user"` 分支，取出所有以 `Answer: ` 开头的行。
   - 只有 1 个答案：显示"答：X"。
   - 多个答案：显示"N 个问题，答：X；Y"。
   - 用 `clip` 限制在 160 列以内。没有答案行时，退回通用分支。
2. **自由回答**：去掉 `break`，问完所有问题。
   - 用户用自己的话回答之后，后面每个问题的提示前加一行"（上一题是你自己写的回答；这题不再适用可直接回车跳过）"。空回答本来就记成 `(no answer)`。
   - 工具输出末尾附一句给模型的说明："The user answered Q{n} in their own words; read the later answers in its light, and treat (no answer) as skipped."
   - 工具说明补一句："Put questions together only when they do not depend on each other's answers."
3. 单选题提示改成"输入编号，或直接输入你的回答"。

## 影响范围

- 改 `src/app/render.rs`、`src/tools/mod.rs`、`src/tools/question.rs`。
- 模型看到的工具输出有变化：不再有"were not asked"，改为附一句说明。

## 验证

- `render.rs` 新增测试：单个问题的回执是"答：2. B"；两个问题的回执是"2 个问题，答：…；…"。
- `src/tools` 新增或修改测试：第一个问题用自己的话回答，第二个问题仍然被问到，提示里有"可直接回车跳过"，输出里有两条 `Answer:`。
- `cargo test` 全量通过。

## 验证结果

- `src/app/render.rs` 新增 `question_receipts_show_the_answers`：单个问题显示"答：3. 先修测试"；两个问题显示"2 个问题，答：先修测试；(no answer)"。通过。
- `tests/all/tools.rs` 新增 `an_answer_in_own_words_still_asks_the_rest_and_lets_them_be_skipped`：第一题用自己的话回答，后两题照常提问，提示以"（上一题是你自己写的回答"开头，输出里有 3 条 `Answer:` 和给模型的说明。通过。
- 原有的 `question_accepts_numbers_or_free_text_and_todo_shows_the_plan` 不受影响，通过。
- `docs/reference.md` 里 `question` 一条已同步。
