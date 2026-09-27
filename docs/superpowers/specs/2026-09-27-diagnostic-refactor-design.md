# 诊断系统重构设计（方案 A：结构化归类 + 自动处置）

日期：2026-09-27　状态：待评审　范围：`internal/diagnostic`、`internal/api`、`internal/engine`（记录处）、`internal/command/diagnose.go`、`internal/config`（一个新键）、文档

## 1. 背景与现状

诊断系统由三部分组成，只有第一部分名副其实：

| 部分 | 现状 | 结论 |
|---|---|---|
| 静态检查器（`/doctor`、`/diagnose`，13 项） | 检查配置、密钥、网络、shell、git、磁盘、会话、policies、后台学习；测试完整 | 保留，不动 |
| 运行时记录（`errors.log`、`/diagnose errors`） | 错误以自由文本记录（`RecordRuntime(sev, cat, message)`）；配诊断码的方法是拿诊断码的**中文标题**在错误文本里找子串（`recorder.go` `matchKnownCode`），任何真实 API 报错都匹配不上，因此从未给出过修复建议；`log.Warnf/Errorf` 全部经 sink 落进同一日志，与引擎主动记录的重复 | 重做 |
| 「自修复」 | 30 个码里 4 个标 `AutoFixable`，其中只有会话完整性检查真的修（清理损坏的会话文件，`NewFixed`）；其余三个标志名不副实，运行期记录的错误没有任何自动动作；文档称「所有修复都是 HotFixable」 | 删除 `AutoFixable`/`HotFixable` 标志（静态检查器的 `DiagError.Fixed` 保留），运行期错误换成真实处置器 |

今天一次会话里出现的三类运行期问题（上下文超出本地模型窗口、工具参数非法 JSON、模型调用卡住）都被记录了，都没有诊断码，都没有建议。

## 2. 目标与非目标

目标：
1. 运行期错误在**产生处按类型归类**，得到稳定的诊断码，而不是靠文本猜。
2. 每个诊断码带一条对 exe 用户可执行的建议；部分码带**处置器**，能在运行期自动改运行策略或运行时配置，并且只在真的执行后才声称「已处置」。
3. `/diagnose errors` 按码聚合，显示次数、最近发生时间、建议、已执行的处置。
4. 文档与命令输出不再出现名不副实的「自修复」「热修复」。

非目标：
- 不改静态检查器的 13 项检查及其输出格式。
- 不引入事件总线、结构化追踪、面板等可观测性基础设施。
- 处置器不修改代码、不自动改写 `config.json`（除本设计明确列出的项）。

## 3. 架构

```
产生处（engine / api / tool）
   │  diagnostic.Report(err, Context{...})      ← 结构化入口
   ▼
Classify(err, ctx) → (Code, Detail)             ← 按错误类型归类，纯函数
   │
   ▼
RuntimeEvent{Code, Detail, Model, Provider, Tool, Time, Severity}
   │  追加到环形缓冲 + errors.log（JSON 行）
   ▼
Remedy（若该码注册了处置器）→ Apply(ctx) → 成功则再记一条 SevRecovered 事件
   │
   ▼
/diagnose errors：按 Code 聚合 → 次数、最近时间、Recovery、已处置记录
启动：上次日志里有未处置的 ERROR/FATAL 且有建议 → 提示一行
```

### 3.1 结构化入口

新增：

```go
// Context 是产生处能提供的事实；字段可空。
type Context struct {
    Provider, Model, Tool string
    Attempt               int
}

// Report 归类并记录 err。返回记录到的事件，便于调用方在同一行日志里引用诊断码。
func Report(err error, c Context) RuntimeEvent
```

`RecordRuntime(sev, cat, message)` 保留为**低层**入口（日志 sink 与无 error 值的场景），但不再做标题子串匹配；`Code` 留空。

### 3.2 归类（Classify）

纯函数，按顺序判定，首个命中者决定：

| 判定（已有能力） | 码 | 类别 | 严重度 |
|---|---|---|---|
| `errors.Is(err, context.Canceled)` | E5002 操作被中断 | engine | info |
| `engine.LimitError` | E5001 达到单轮上限 | engine | warning |
| `api.IsContextLengthError(err)` | **E2008 上下文超出模型窗口**（新增） | api | error |
| `statusOf == 429` 或 `isRateLimit` | E2003 触发速率限制 | api | warning |
| `statusOf ∈ {401,403}` | E2004 认证失败 | api | fatal |
| `statusOf == 400` | E2005 请求参数错误 | api | error |
| `statusOf >= 500` | E2006 服务端错误 | api | warning |
| 传输错误（`isTemporary`：timeout/EOF/reset） | E2002 / E2007 | network | warning |
| `api.ToolArgsInvalid`（新增哨兵，见 3.4） | **E4009 工具参数非法 JSON**（新增） | tool | warning |
| 其他 | 无码，按原文记录 | 调用方给的类别 | 调用方给的严重度 |

`isRateLimit`/`isTemporary`/`isPermanent`/`statusOf` 目前是 `internal/api` 的私有函数，导出为 `api.Classify(err) api.ErrorKind`（枚举：Canceled、ContextLength、RateLimit、Auth、BadRequest、ServerError、Transport、Unknown），诊断层只依赖这个枚举，不重复写匹配逻辑。

### 3.3 目录（Catalog）

`ErrorDef` 去掉 `AutoFixable`、`HotFixable`，新增 `Remedy RemedyFunc`（可空）。新增诊断码：

| 码 | 标题 | 建议（Recovery） | 处置器 |
|---|---|---|---|
| E2008 | 上下文超出模型窗口 | 输入 /continue 会先压缩历史再重试；仍失败需调大模型上下文（llama-server `-c`、LM Studio Context Length、Ollama `num_ctx`）；cove 固定开销约 13K token | 有：学习窗口大小（3.5.1） |
| E2009 | 供应商已被标记不可用 | 连续 3 次失败后本会话不再优先尝试该供应商；只有一个供应商时仍会继续尝试它，修好原因（如调大模型上下文）后成功一次即自动复位；有多个供应商时可用 `/provider <名称>` 切换 | 无（建议） |
| E4009 | 工具参数非法 JSON | 模型输出的工具参数无法解析；同一模型反复出现说明它不适合工具调用，考虑换模型或降低 temperature | 有：计数提示（3.5.2） |
| E5007 | 模型调用无进展 | 已 N 秒无响应，可 Ctrl+C 中断；本地模型常见于上下文接近上限或显存不足 | 无 |

现有 E2005「请求参数错误」的 Recovery 文案「已自动调整」改为「检查模型名与请求格式；若是本地服务，确认其兼容 OpenAI 接口」。

### 3.4 产生处改造

| 位置 | 现在 | 改为 |
|---|---|---|
| `engine.go` 模型调用失败处 | `RecordRuntime(SevError, CatAPI, "模型调用失败: "+err)` | `diagnostic.Report(err, Context{Provider, Model, Attempt})`；用户可见的失败行末尾附 `[E2008]` 之类的码 |
| `engine.go` 工具失败处 | `RecordRuntime(SevWarning, CatTool, "工具 X 失败: …")` | 工具结果以 `Error:` 开头且属于参数修复失败时（`api/tool_repair.go` 新增哨兵错误 `ToolArgsInvalid` 并让结果携带它），`Report(ToolArgsInvalid, Context{Tool, Model})`；其余工具错误保持无码记录 |
| `activity.go` 卡住告警 | `RecordRuntime(SevWarning, CatEngine, "stage … stalled")` | `Report(StallError{Stage, Idle}, …)` → E5007 |
| `api/fallback.go` 标记不可用 | `log.Errorf` | 额外 `Report(ProviderUnavailableError{Name, Fails, Cause}, …)` → E2009 |
| `AttachToLogger` | 每条 Warn/Error 都进日志 | 保留，但 sink 记录的事件标记 `Source: "log"`；`Report` 已记录过的错误不再由 sink 重复（引擎记录后用 `log.Debugf` 而不是 `Errorf` 打印同一错误） |

### 3.5 处置器（Remedy）

```go
type RemedyFunc func(ev RuntimeEvent, rt Runtime) (applied string, ok bool)

// Runtime 是处置器能触碰的运行期能力，由 cli 在启动时注入；测试可用桩。
type Runtime interface {
    SetModelContextWindow(model string, tokens int)   // api 层会话级覆盖
    Notify(line string)                              // 向用户打印一行
}
```

处置器成功返回 `ok=true` 时，记录一条 `SevRecovered` 事件（`Code` 同源、`Detail=applied`），`/diagnose errors` 显示为「已处置：…」。失败或不适用返回 `ok=false`，不留痕。

#### 3.5.1 E2008：学习模型窗口

从报错文本解析真实窗口：匹配 `n_ctx"?\s*:\s*(\d+)`、`context size \((\d+) tokens\)`、`maximum context length is (\d+)`、`(\d+) maximum`。解析成功且与 `api.ContextWindowForModel(model)` 不同时：
- 调用 `SetModelContextWindow(model, n)`（`internal/api` 新增会话级覆盖表，`ContextWindowForModel` 先查它）；
- `Notify("已按服务端返回的 16384 token 调整本会话的上下文预算；要固定下来，在 config.json 加 \"context_window\": 16384")`；
- 返回 `applied="窗口 32000 → 16384"`。

此后压缩触发点按真实窗口计算，同一会话不再撞墙。配合引擎已有的「超长即压缩重试」，用户输入 `/continue` 即可续跑。

#### 3.5.2 E4009：计数提示

同一 `Model` 在本会话累计 ≥ 3 次时 `Notify` 一次「模型 X 已 3 次输出非法工具参数，建议换模型或降低 temperature」，并 `applied="已提示"`；之后每再累计 5 次提示一次。

### 3.6 配置

`config.json` 新增可选键 `context_window`（int）：当前 `model` 的上下文窗口。启动时若设置，`api.SetModelContextWindow(cfg.Model, n)`。这是 3.5.1 提示用户「固定下来」的落点；处置器本身不写配置文件。

### 3.7 报告

`/diagnose errors`：
```
运行时问题记录（共 9 条，按严重程度排序）
 [ERROR] E2008 上下文超出模型窗口 ×3   最近 13:17
    模型 qwen3.6-27b · 服务端窗口 16384
    💡 输入 /continue 会先压缩历史再重试；仍失败需调大模型上下文 …
    ✓ 已处置 13:18：窗口 32000 → 16384
 [WARN]  E4009 工具参数非法 JSON ×2   最近 13:16
    …
 [WARN]  （无码）loop detected (layer 2): … ×1
```
聚合键：有码按 `Code+Model`，无码按去掉数字与路径后的消息文本（`\d+`、`[A-Za-z]:\\\S+`、`/\S+` → `#`），避免同一错误因 token 数不同被拆成多条。

`/diagnose codes`：去掉「可自动修复/即时生效」标注，改为「有处置器」。

启动提示：`startupDiagnosticsText` 在静态检查之外，读取上次日志：存在 ERROR/FATAL 且其码有 Recovery、且之后没有同码的 `SevRecovered` → 附一行「上次运行有 N 个未处理的问题，/diagnose errors 查看」。

### 3.8 文档

`USER_MANUAL.md` 诊断系统一节：删除「所有修复都是 HotFixable」；新增「自动处置」小节，列出处置器及其边界（只改运行策略和会话级参数，不改代码，不写配置文件）；诊断码表补 E2008/E2009/E4009/E5007；配置表补 `context_window`。`CHANGELOG.md` 记录。

## 4. 错误处理

- `Report` 永不 panic、永不阻塞：归类失败退化为无码记录；处置器内 `recover`，失败视为 `ok=false`。
- 日志文件写失败静默（现有行为）。
- `Runtime` 未注入（headless、测试）时处置器一律 `ok=false`，只留建议。

## 5. 测试

- `Classify`：表驱动，覆盖 3.2 每一行，包括 llama.cpp 报错原文、`fallback` 组合错误、包装过的错误。
- 处置器：桩 `Runtime`；E2008 解析四种文案；窗口一致时不处置；`Runtime` 为空时不处置；`SevRecovered` 事件写入。
- 聚合：同码不同 token 数合并；无码消息去数字合并；`SevRecovered` 正确挂到源码。
- 产生处：引擎测试断言模型调用失败行携带 `[E2008]`；`activity` 卡住告警产生 E5007。
- 现有静态检查器测试全部不变。

## 6. 实施顺序（供计划拆分）

1. `internal/api`：导出 `Classify`/`ErrorKind`；会话级窗口覆盖；`ToolArgsInvalid` 哨兵。
2. `internal/diagnostic`：`Context`/`Report`/`Classify`；目录改造（删假标志、加新码、加 `Remedy`）；聚合重写；处置器与 `Runtime`。
3. 产生处接线：engine、activity、fallback、tool_repair。
4. `/diagnose` 输出；启动提示；`config.context_window`；cli 注入 `Runtime`。
5. 文档与 CHANGELOG。

每步独立可测、可编译，`go test ./...` 在每步末尾通过（`internal/config` 的品牌测试为既有失败，不在范围内）。
