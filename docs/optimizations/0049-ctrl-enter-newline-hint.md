# 0049 换行提示改为 Ctrl+Enter

- 状态：已完成
- 日期：2026-11-25（创建）；2026-11-25（完成）
- 关联：无

## 问题

输入框为空时的提示是 `(/ 查看命令，@路径 或 Alt+V 添加附件，Ctrl+J 换行)`（`src/app/console.rs` 的 `placeholder`），README 的操作表也写"Ctrl+J、Shift+Enter 或 Alt+Enter"。Ctrl+Enter 在 Windows 上本来就能换行（`console.rs` 按键处理里 `KeyCode::Enter if alt || control || shift`），提示却要用户去记 Ctrl+J。

特殊情况：cove 没有开启终端的键盘增强协议，macOS/Linux 的大多数终端里 Ctrl+Enter 和 Enter 发出的是同一个字符，分不出来，那里只能用 Ctrl+J。

## 改法

- Windows 上提示 `Ctrl+Enter 换行`；其他平台仍提示 `Ctrl+J 换行`。
- README 操作表和 `docs/reference.md` 的"多行输入"把 Ctrl+Enter 写在前面，并注明 Ctrl+J 用在分不出 Ctrl+Enter 的终端。
- 按键处理不变，Ctrl+J 仍然可用。
- 测试：`console.rs` 的多行输入测试加上 Ctrl+Enter。

## 验证

- `cargo test --release --lib app::console`：通过（见下）。
- release 版在 Windows Terminal 里确认 Ctrl+Enter 换行、提示文字正确：未做，待下次使用时确认。

## 验证结果

见"验证"。
