# 交互层收敛改造复盘（已完成）

> 状态：已完成。交互统一为 Bubble Tea TUI；非交互统一为 headless 无 UI 通道。

## 1. 最终形态

- 交互式终端：统一走 Bubble Tea TUI。
- 非交互场景（管道、重定向、`--no-tui`、`COVE_TUI=0`）：统一走 headless。
- 任务队列：统一为 `internal/session/task_runner.go`。
- 终端输出能力：统一收敛到 `internal/termui/`。

## 2. 已完成项

- 移除旧行模式交互栈（输入器、终端能力适配、旧控制台兼容层）。
- 删除旧的 Windows 控制台兼容文件与对应测试。
- 将历史的终端打印与状态指示能力迁移到 `internal/termui/`。
- TUI 侧改用共享任务队列，消除本地重复实现。
- CLI 帮助与行为语义统一到 TUI + headless。

## 3. 当前架构

```text
Session Core
  ├─ TaskRunner（唯一队列）
  ├─ 命令与会话逻辑
  └─ Engine 编排

Frontends
  ├─ Bubble Tea TUI（交互式 TTY）
  └─ Headless（非交互 stdin/stdout）
```

## 4. 验证结果

- 全量测试：`go test ./...` 通过。
- 关键行为：
  - `./cove` 启动 TUI
  - `./cove --no-tui` 走 headless
  - `echo "hello" | ./cove` 走 headless

## 5. 维护建议

- 新增交互能力优先放在 TUI 与 headless 共享逻辑层。
- 避免重新引入与 UI 耦合的输入/打印实现。
- 文档新增示例统一使用 TUI + headless 口径。