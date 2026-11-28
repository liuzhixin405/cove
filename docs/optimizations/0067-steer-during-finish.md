# 0067 收尾验证和评审期间的输入留在这一轮；用过的插话唤醒不再打断下一次生成

- 状态：已完成
- 日期：2026-11-28（创建）；2026-11-28（完成）
- 关联：0065（让模型用 question 工具提问）；0050（等后台命令时接住输入）

## 背景

模型给出最终回复后，引擎还要跑收尾验证和评审，cove 上一次全量测试约 150 秒。这段时间里用户输入的内容（常见的是对回复里问题的回答），任务结束后由 app 层当成新消息补发，并带上 `[sent while the previous task was running]` 前缀。这一轮的上下文就断了。0065 让模型改用 `question` 工具提问，可以减少这种情况，但不能消除。

## 现状与问题

1. **收尾验证、评审到完成之间不读插话。**
   - 收尾验证在 `src/engine/agent.rs:2382-2446`，评审在 `2452-2498`。评审结果是 Quiet 或 Concluded 时，直接 `State::Completed`（`2499`）。
   - 这段时间输入的文字留在 console 队列里，由 `src/app/mod.rs:564-567` 当作新任务发出。
   - 其他阶段都会接住插话：生成中被打断后重答（`2164-2191`）、工具结果后的 `tool_note` 和 `finish_nudge`（`src/app/console.rs:580-592`）、等后台命令时（`2319-2327`、`2610-2636`）。
   - console 的 `ConsoleSteer::take` 和 `SteerHook::take` 用的是同一个队列（`console.rs:535`）。
2. **已经送达的 `!` 插话，会让下一次生成一开始就被打断。**
   - `!` 插话会调用 `STEER_WAKE.notify_one()`（`console.rs:1567`）。远程插话的唤醒方式相同。
   - 如果插话发生在工具运行期间，没有人在等，`Notify` 会留下一个 permit。文字随后被 `tool_note` 取走，permit 却还在。
   - 下一次生成时，`steer_waiting` 立即返回，取不到文字，于是丢弃已经发出的请求、记一次调用、重发（`agent.rs:2176-2197`）。白占一次调用名额，还多发了一次请求。

## 目标

- 收尾验证或评审期间输入的内容，在同一轮里交给模型，不再变成新消息。
- 用过的唤醒不再打断请求：一次插话只引发一次重答。

## 范围

- 做：上面两个问题。
- 不做：
  - 任务标成完成之后、回到输入循环之前的极短窗口。这时 app 层补发是正确的兜底。
  - 收尾验证进行中被输入打断。验证跑到一半就中止，会让验证结果失效；输入在验证跑完后处理即可。

## 方案

1. 在 `Agent` 上加一个 helper `typed_meanwhile()`，从所有插话来源取出文字（和 `2629-2635` 相同）。在两个位置调用：
   - 收尾验证结束、评审之前。取到就不做评审：回复要按新的输入改，现在评审是白花钱。
   - 评审结束、`State::Completed` 之前。
   - 取到时：`progress.user_input()`，推入 `Message::steer`，保存，提示"[steer] 收尾检查期间收到你的输入，在这一轮接着处理"，然后 `continue`。模型下一次回复后照常收尾；没改文件时，验证会按指纹跳过（`src/safety/verify.rs:1462`）。
2. 生成时的 `select` 改成循环。被唤醒却取不到文字时，接着等原来的请求（`stream`），不再丢弃重发。之后输入的新插话仍能打断它。去掉"空唤醒也计一次调用"的保护：permit 用掉就没了，不会空转。

## 影响范围

- 只改 `src/engine/agent.rs`。
- 行为变化：收尾期间输入的内容会多触发一次模型回复（原来是作为新任务发出，花费相同）。

## 验证

- 新增测试：模型最终回复后，在验证器运行期间往插话来源推一条文字，断言模型被再调用一次、会话里有这条 steer、任务只有一个。
- 新增测试：插话来源先留一个 permit、再被取空，然后生成。断言只发一次请求，`model_calls` 为 1。
- `cargo test` 全量通过。

## 验证结果

- 新增 `tests/all/core.rs`：
  - `text_typed_during_the_final_checks_stays_in_the_task`：验证器运行时推入插话，断言同一任务里再请求一次模型，第二次请求带着这段文字，队列被取空。通过。
  - `a_wake_whose_text_was_already_delivered_keeps_the_reply`：先留一个空的唤醒信号，再生成，断言 `model_calls == 1`，只发一次请求。通过。
- 改写 `steering_empty_wakeup_counts_interrupted_request`。原测试断言的正是"空唤醒打断请求、计一次调用"这个被去掉的行为，改名为 `steering_an_empty_wakeup_keeps_the_call_and_typed_text_still_interrupts`：空唤醒之后请求继续；之后真正输入的文字仍能打断它，并带进重答的请求。通过。
- `cargo test --test all -- steer`，以及上面这些测试：全部通过。
- 方案之外加了一层保护：同一次请求里，连续 8 次被唤醒却取不到文字，就不再等插话、只等请求结束，防止唤醒信号有问题的来源造成空转（`EMPTY_WAKES`）。
- `await_jobs` 里取插话的那段代码改用同一个 helper `typed_meanwhile`，行为不变。
