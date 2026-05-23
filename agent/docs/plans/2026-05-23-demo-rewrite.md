# demo 全量重写实施计划

> For Hermes: use subagent-driven-development skill to implement this plan task-by-task.

Goal: 把 demo 从“可编译但大量占位实现”的状态，重写成“命令、配置、权限、插件、MCP、会话、关键工具都能真实工作”的 CLI 代理，并统一品牌、配置目录、构建入口与发布产物为 agentgo。

Architecture: 保留现有包边界（cmd/internal/*），但把运行时依赖显式注入到 command 层，并把权限判定从“仅 bash 分类器”升级为“所有工具统一判定 + 规则引擎 + 模式控制”。对仍需外部系统支持的能力（如 LSP/远程消息）改成真实降级行为，而不是误导性的假成功文案。

Tech Stack: Go 1.25, stdlib, x/term, git, MCP JSON-RPC, 本地 JSON 配置/会话/插件清单。

---

### Task 1: 保存实施计划

Objective: 把本次重写计划落盘，后续修改都以此为准。

Files:
- Create: `docs/plans/2026-05-23-demo-rewrite.md`

Step 1: 写入计划文件
Step 2: 校验文件存在且内容完整

Verification:
- `docs/plans/2026-05-23-demo-rewrite.md` 存在
- 文件包含 commands/core/tools/verify 四大阶段

### Task 2: 为重写补上失败测试骨架

Objective: 先用测试锁定当前缺失能力，避免继续在假实现上迭代。

Files:
- Create: `internal/command/commands_test.go`
- Create: `internal/plugin/plugin_test.go`
- Create: `internal/tool/extra_tools_test.go`
- Modify: `internal/tool/advanced_tools_test.go`

Step 1: 为 `/config`、`/plugin`、`/skills`、`/export` 写失败测试
Step 2: 为 plugin install/enable/disable/uninstall 的持久化行为写失败测试
Step 3: 为 websearch 的真实结果解析/失败降级写失败测试
Step 4: 为 team_delete / cron / brief 等当前断言错误或占位行为补测试

Verification:
- `go test ./internal/command ./internal/plugin ./internal/tool` 先红后绿

### Task 3: 重写 command 输入上下文与真实命令执行

Objective: 让 command 包拿到 engine/config/state/permission/plugin/skills/mcp 等依赖，替代当前“只能打印提示文本”的假命令。

Files:
- Modify: `internal/command/command.go`
- Rewrite: `internal/command/commands.go`
- Modify: `cmd/agentgo/main.go`

Step 1: 扩展 `command.Input`，注入运行时依赖
Step 2: 将 `/config` 改为真实读取/写入配置
Step 3: 将 `/memory` 改为真实 list/add/remove
Step 4: 将 `/resume` 改为真实列出会话、按 id 恢复到 engine
Step 5: 将 `/mcp` 改为真实 list/connect/disconnect/reload
Step 6: 将 `/plugin` 改为真实 list/install/enable/disable/uninstall
Step 7: 将 `/skills` 改为真实 list/show/search
Step 8: 将 `/export` 改为真实导出 conversation markdown/json
Step 9: 将 `/system`、`/context`、`/status`、`/stats` 改为读取真实运行时
Step 10: 清理 `main.go` 中重复/旁路逻辑，只保留 CLI 参数入口和 REPL 分发

Verification:
- `go test ./internal/command`
- `go build ./cmd/agentgo`
- 手动验证 `/config` `/plugin list` `/skills` `/export tmp.md`

### Task 4: 接通统一权限系统

Objective: 让所有工具都经过统一权限决策，而不是只对 bash 做半套控制。

Files:
- Modify: `internal/permission/permission.go`
- Modify: `internal/tool/tool.go`
- Modify: `internal/engine/engine.go`
- Modify: `internal/tool/mcp_tool.go`
- Modify: `internal/tool/write.go`
- Modify: `internal/tool/edit.go`
- Modify: `internal/tool/advanced_tools.go`

Step 1: 给 `permission.Manager` 增加 rule 增删查
Step 2: 在 `engine.executeTool` 中统一执行 Validate + Tool 默认权限 + Policy 覆盖
Step 3: 对 default/plan/auto/bypass 的行为做一致化
Step 4: MCP 工具也纳入统一权限
Step 5: 修复 baseTool 默认 “not implemented” 导致的误导行为

Verification:
- `go test ./internal/permission ./internal/tool`
- 手动验证 plan 模式禁止写工具、auto 模式允许安全工具

### Task 5: 重写 plugin 持久化与状态管理

Objective: 修复当前 plugin manager 的伪安装/伪卸载/不持久化状态。

Files:
- Modify: `internal/plugin/plugin.go`
- Create: `internal/plugin/plugin_test.go`

Step 1: install 时写入 `manifest.json`
Step 2: disable/enable 通过 marker 或 state file 持久化
Step 3: uninstall 真正删除插件目录
Step 4: list 时展示 state / error / manifest 信息

Verification:
- `go test ./internal/plugin`
- 临时目录中 install/disable/enable/uninstall 全流程通过

### Task 6: 重写 skills / memory / session 的 CLI 可用性

Objective: 让 skills、memory、session 在 REPL 中能真实浏览和恢复，而不是只打印硬编码文本。

Files:
- Modify: `internal/skills/skills.go`
- Modify: `internal/memory/store.go`
- Modify: `internal/session/store.go`
- Modify: `internal/engine/engine.go`

Step 1: skills list 排序、show 内容、search 过滤
Step 2: memory 保存/删除/list 对外暴露稳定接口
Step 3: session store 增加辅助方法（标题推导、删除/路径/摘要）
Step 4: engine resume/export/status 所需 accessor 补齐

Verification:
- `go test ./internal/skills ./internal/session`
- 手动验证 `/resume` `/skills <name>` `/memory list`

### Task 7: 补全关键工具真实行为

Objective: 把最明显的高级工具假实现替换成真实可用或真实降级的实现。

Files:
- Modify: `internal/tool/extra_tools.go`
- Modify: `internal/tool/advanced_tools.go`
- Create: `internal/tool/extra_tools_test.go`
- Modify: `internal/tool/advanced_tools_test.go`

Step 1: websearch 改成免 key 的真实搜索（带结果解析与失败降级）
Step 2: brief 改成基于 runtime/session 的真实摘要，而不是固定字符串
Step 3: cron / send_message / lsp / team_* 统一改成“真实本地 runtime 行为 + 明确边界”，避免虚假成功文案
Step 4: task 系列工具输出结构化且可追踪

Verification:
- `go test ./internal/tool`
- 手动调用工具路径确认输出不再是占位字符串

### Task 8: 修复 MCP / context / REPL 细节一致性

Objective: 清掉文档与运行时不一致、Windows shell 误报、MCP 信息不完整等问题。

Files:
- Modify: `internal/context/context.go`
- Modify: `internal/mcp/pool.go`
- Modify: `internal/mcp/client.go`
- Modify: `cmd/agentgo/main.go`
- Modify: `README.md`

Step 1: shell 探测与当前运行环境对齐
Step 2: MCP list 输出 server type / tools / resources / error
Step 3: CLI help、banner、README 与实际命令数/能力对齐
Step 4: 清理误导性提示文案
Step 5: 统一品牌、配置目录、构建入口、release 产物和绿色版说明为 agentgo

Verification:
- `go build ./cmd/agentgo`
- `go test ./...`
- README 中命令/工具/架构描述与代码一致

### Task 9: 全量验证

Objective: 确认重写版可构建、可测试、关键交互可运行。

Files:
- Modify: `README.md`

Step 1: `go test ./...`
Step 2: `go build -o agentgo.exe ./cmd/agentgo`
Step 3: 用非交互方式验证 `--doctor` `--config` `--list-sessions`
Step 4: 用脚本喂给 REPL 验证 `/help` `/config` `/skills` `/plugin list`
Step 5: 更新 README 的命令、工具、限制说明
Step 6: 验证 release 包、checksums 与 portable/绿色版说明一致
Step 7: 验证无 API key 时 REPL 会给出 /api-key、环境变量和 /config 检查指引
Step 8: 验证 Windows 终端中文输入与 UTF-8 提示说明一致

Verification:
- 全量测试通过
- 构建成功
- README 与实际行为一致
- 无明显旧品牌命名残留
