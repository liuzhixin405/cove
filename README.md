# cove

**终端里的中文 AI 编程助手：读代码、改代码、跑命令和测试，改坏了整体撤销，花费逐笔可查。**

一个可执行文件，不用装运行时。支持 DeepSeek、OpenAI、Anthropic、AWS Bedrock、通义、Kimi、智谱、豆包、OpenRouter，以及任何 OpenAI 兼容接口（包括本地模型）。

![cove 演示：先找出并修好一个失败的测试，再加一个会员折扣需求并补上测试](docs/assets/demo.gif)

<sub>真实录制，没有剪辑：先修好一个边界条件 bug，再加会员折扣需求、补 4 个测试，共 6 个测试通过，花费 $0.01。</sub>

## 和同类工具的不同

- **撤销包括 Shell 的改动。** 每个任务动手前，cove 把工作区快照到一个私有 Git 仓库（不碰你的 `.git`）；脚本改的文件也能 `/undo`，恢复本身也能再撤销。
- **改完自己验证。** 改过文件后自动推断并运行构建和测试，失败的输出交回模型修，最多 3 轮；pytest 和 `go test` 会排除改动前就失败的用例。
- **原地打转会停。** 连续 12 轮既没有新改动、也没有拿到新信息，cove 停下来，列出反复出现的错误，问你停下、换思路还是继续。
- **记住你的纠正。** 你纠正过的做法写进 `.cove/rules.md`，跟着仓库走；之后每个任务都遵守，提交前的 AI 审查也按它检查。
- **花费算得准。** 按实际作答的模型计价，缓存读写分开算；输入行上方的状态行一直显示上下文长度和本会话花费。

## 安装

**预编译版**：在 Releases 页面下载对应平台的文件（Windows 是 `cove-x86_64-pc-windows-msvc.exe`），改名为 `cove.exe`，放进 `PATH` 里的一个目录。

**从源码构建**（Rust stable）：

```powershell
cargo build --release --locked
# 产物：target\release\cove.exe
```

主要在 Windows 上开发和测试。Windows MSVC 工具链需要 Visual Studio C++ Build Tools，见[构建](docs/reference.md#构建)。

## 60 秒上手

1. 进入项目目录，启动：

   ```powershell
   cd D:\my-project
   cove
   ```

2. 第一次运行会进入设置向导：选服务商、确认地址和模型、输入 API key。配置存到 `%USERPROFILE%\.cove\config.json`，之后 `/config setup` 可以重来。
3. 直接说要做什么：

   ```text
   > 这个项目的入口在哪里，整体结构是怎样的
   > 修复 OrderServiceTests 里失败的测试，并跑一遍相关测试
   > 给 /api/orders 加分页参数，照着现有接口的写法来
   ```

每一步都显示在屏幕上：读了哪个文件、改了哪几行、命令的退出码；结束时列出模型调用次数、token 和花费。连不上或报错时，运行 `cove --doctor` 检查配置、密钥和网络。

## 常用操作

输入 `/` 列出所有命令，`/help <命令>` 看某一条的用法。

| 想做什么 | 怎么做 |
| --- | --- |
| 附上文件或截图 | 消息里写 `@路径`（Tab 补全）；把图片拖进终端；Windows 上 Alt+V 粘贴剪贴板图片 |
| 换行不发送 | Ctrl+Enter、Shift+Enter 或 Alt+Enter；终端分不出 Ctrl+Enter 时用 Ctrl+J |
| 运行中补充要求 | 直接输入，下一步交给模型；开头加 `!` 立即打断当前回答 |
| 停下当前任务 | 连按两次 Esc，或 Ctrl+C |
| 看任务进度和子代理 | Alt+M 打开或关闭任务面板 |
| 接着上次做 | 输入 `继续`，或 `/continue` |
| 看改了什么 | `/diff` |
| 撤销 | `/undo` 列出回滚点，`/undo #n` 预览，`/undo confirm` 执行 |
| 提交 | `/commit 消息`：先验证、再 AI 审查，有问题就不提交；`--no-review` 跳过审查；从不推送 |
| 管理规则和偏好 | `/rules` 查看；`/rules add <规则>`、`/rules add global <偏好>` |
| 找回旧会话 | `/resume`：打字筛选，Enter 恢复，Tab 预览，Ctrl+R 改名 |
| 换个话题 | `/handoff`：写一份交接摘要，在新会话里接着做；`/back` 回原会话，`/new` 从空白开始 |
| 回看问答 | `/turns`：只看每轮的提问和结论 |
| 看完整工具输出 | `/expand`，或 `/expand 3` 看 `#3` |
| 看花费 | `/usage`（本会话），`/cost`（按月，可导出 CSV） |
| 换模型 | `/model <名称>`；`/model fast <名称>` 设快速模型 |
| 退出 | `/exit` 或 Ctrl+D，会话自动保存 |

**不进交互界面**：`-p` 运行一次就退出，不会询问你。

```powershell
cove -p "解释这个项目的入口"
Get-Content error.log | cove -p "这个报错是什么原因"    # 管道内容附在提示词后面
cove -p "分析这段代码的问题" --permission plan          # 只读
```

## 权限与安全

**默认是 `bypass` 模式**：除了拒绝规则和灾难命令，什么都直接做，不询问。想先审批再执行，用 `--permission default` 启动，或在会话里 `/mode default save`（写入配置的 `permission_mode`）。

| 模式 | 效果 |
| --- | --- |
| `bypass`（默认） | 全部直接做，只拦拒绝规则和灾难命令 |
| `default` | 读代码、改项目内的文件、构建、测试、本地 git 提交直接做；删改文件的命令、推送远端、装软件、改系统设置、写项目外的文件先问你 |
| `auto` | 在 `default` 基础上，网络工具、已信任的扩展进程，以及能用 `/undo` 恢复的文件删除也直接做 |
| `plan` | 只读，适合先分析、出方案 |

需要审批时，提示是这样的：

```text
┌ 需要批准 · 原因：推送到远端（git push）
│ 运行 shell 命令（无沙箱）：git push origin main
  1. 允许这一次 (y)
  2. 本次会话不再询问 (s) - git 常规操作（push、pull、merge 等；--force、reset、clean 仍会询问）
  3. 本项目以后都不再询问 (a) - git 常规操作（push、pull、merge 等；--force、reset、clean 仍会询问）
  4. 拒绝 (n) - 或直接输入理由，告诉模型该怎么做
└ 按 y/s/a 直接选，或上下键选择后回车；直接回车 = 拒绝
```

不想用 y/n 时可以直接打字，比如"先别推送，等我看完"：这句话作为理由交给模型，任务继续。

使用前请知道：

- 任何模式都拦截灾难命令，比如删除整个磁盘或家目录、格式化、下载后直接执行脚本。但这只是护栏，不是沙箱：Shell 命令拥有你的系统权限。重要项目先提交，或在新分支上做。
- `/undo` 只恢复工作区里的文件，撤销不了已经推送的提交、已经发出的请求。
- 会话、记忆和快照存在 `%USERPROFILE%\.cove`，含代码片段、没有加密，不要共享这个目录。
- 费用是按单价估算的，以服务商账单为准。

规则细节、`policies.json` 和被拒后的脚本限制见[权限与限额](docs/reference.md#权限与限额)。

## 配置

- **用户配置**：`%USERPROFILE%\.cove\config.json`，可以写多套 `profiles` 用 `--profile` 切换。完整示例见 [config.example.json](docs/config.example.json)，字段表见[配置文件](docs/reference.md#配置文件)。
- **界面里直接改**：`/model`、`/config api_key`、`/config effort high`，立即生效并写回配置。
- **API key**：也可以用环境变量，如 `COVE_API_KEY`、`DEEPSEEK_API_KEY`、`ANTHROPIC_API_KEY`。
- **项目配置**：项目根目录的 `.cove.json`。会执行命令或花钱的字段，要先 `/config trust` 信任这个项目才生效，见[项目信任](docs/reference.md#项目信任与-covejson)。

## 进阶功能

| 功能 | 一句话 | 详细说明 |
| --- | --- | --- |
| 快速模型分工 | 配了 `model_fast` 后，简单任务和压缩、标题等后台调用走快速模型 | [快速模型与路由](docs/reference.md#快速模型与路由) |
| 拆成子任务并行 | 说"拆成几个子任务并行做"；大任务先给你拆分计划确认，各自在覆盖层里改，最后统一合并 | [多个子任务怎么分发](docs/reference.md#多个子任务怎么分发) |
| 大范围调查 | 要看很多文件的问题交给只读的 explore 子代理，主上下文只收结论和 `路径:行` 证据 | [子 Agent 类型](docs/reference.md#子-agent-类型) |
| 后台 Agent | 在各自的 git worktree 里干活，`/status agents` 看进度、决定合并哪个 | [后台 Agent 与生命周期](docs/reference.md#后台-agent-与生命周期) |
| 一个需求两个方案 | `/race 需求`：两个模型各做一版，同一组命令验证，选一个应用 | [/race](docs/reference.md#一个需求两个方案race) |
| 项目自己的审查 | `.cove.json` 里写 `review_commands`，改完自动跑，有问题交回模型修 | [完成闸门](docs/reference.md#完成闸门) |
| 改动影响 | 收尾时列出改动的函数被谁调用、有没有测试；`/changes impact origin/main...HEAD` 审 MR | [完成闸门](docs/reference.md#完成闸门) |
| 先出方案再改 | `/mode review` 让每个任务先提交计划，批准后才改 | [计划模式与审阅流程](docs/reference.md#计划模式与审阅流程) |
| 长会话 | 旧工具输出外置，上下文到 40 万 token 前自动压缩；`/compact` 手动压缩 | [上下文与回退边界](docs/reference.md#上下文与回退边界) |
| 记忆与偏好 | 项目约定自动记下；所有项目都遵守的做法放进全局偏好 | [记忆](docs/reference.md#记忆)、[全局偏好](docs/reference.md#全局偏好) |
| MCP | 在配置里加 `mcp_servers`，需要登录的用 `/mcp login <名字>` | [MCP](docs/reference.md#mcp) |
| 技能 | `/skills` 列出，`/技能名` 直接用，`/config skills marketplace` 浏览市场 | [Skills](docs/reference.md#skills) |
| Hooks 与插件 | 兼容 Claude Code 格式的 `hooks.json` 和插件 | [扩展](docs/extensions.md) |

## 文档

- [技术参考](docs/reference.md)：所有命令、配置项和行为细节
- [扩展](docs/extensions.md)：MCP、技能、Hooks、插件
- [架构设计](docs/design.md)：模块划分、实现原理和各模块的设计取舍
- [性能说明](docs/performance.md)：提速机制与测量口径
- [开发须知](AGENTS.md)：改代码前必读

MIT License
