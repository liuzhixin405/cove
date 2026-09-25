# cove 第二轮优化实施计划（授权体验 + 18 项修复）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把一次 git 提交所需的授权次数从十几次降到 0–2 次，并完成 2026-09-25 调研清单中除"钥匙串"外的 18 项修复。

**Architecture:** 分四个批次、两波执行。第一波三批并行且文件集互不相交：A 权限/安全（含 engine.go 授权区段）、C 工具层、D 会话/渲染/浏览器/CI/死代码；第二波 B 引擎/API 与测试提速，在 A 完成对 engine.go 的改动后开始。每项修复先写失败测试再改代码。

**Tech Stack:** Go 1.25，标准库 testing；无 git 仓库，用 `go build ./... && go vet ./... && go test ./...` 作为门禁。

**Spec:** 本文件"根因分析"与"任务"两节即规格；调研原文见会话 2026-09-25。

## Global Constraints

- 项目目录 `D:\github\cove-main` 不是 git 仓库，任何任务都不执行 git 命令；改动前把原文件复制到 `%SCRATCHPAD%/orig/<相对路径>` 以便导出补丁。
- 每个任务结束时 `go build ./... && go vet ./...` 必须通过，所属包 `go test ./internal/<pkg>/...` 必须通过。
- 不新增第三方依赖；goldmark 已在 go.mod 中，可用于任务 D3。
- 中文 Windows 是主要运行环境：所有涉及 shell 的改动必须在 Git Bash、PowerShell 5.1、cmd 三种解释器下考虑。
- 用户可见文案与现有风格一致（中文提示、英文错误前缀 `Error:`）。
- 文档只在第二波末尾统一更新（任务 B7），各批次把需要写进文档的变更记到 `%SCRATCHPAD%/doc-notes-<批次>.md`。

## Review Focus

1. 含括号、分号、`&` 的提交信息（`git commit -m "fix(api): a; b"`）在 Git Bash 下应被已记住的 `git commit` 前缀放行，在 cmd 下应照常询问。→ 任务 A3 测试。
2. `git status && rm -rf build` 不能因为 `git status` 只读而整条放行。→ 任务 A1 测试。
3. `bash -c "rm -rf ~"` 与 `cmd /c rd /s /q C:\` 必须被灾难命令扫描拦住。→ 任务 A2 测试。
4. 出错的工具结果（如 5 万行编译日志）进入上下文前必须被截断到与成功结果同一上限。→ 任务 B3 测试。
5. edit 模糊命中且缩进不一致时不得静默写入。→ 任务 C1 测试。

---

## 根因分析：一次 git 提交为什么要授权十几次

调用链：`executeTool` → 分类器（只在 auto 模式生效，`engine.go:1501-1509`）→ `bashTool.CheckPermissions`（default 模式一律 Ask，`bash.go:91-98`）→ `Manager.Check`（前缀规则，`permission.go:98-150`）→ `PolicyEngine.Evaluate` → 交互提示（`permission_prompt.go:50-113`）。

| # | 根因 | 证据 |
|---|---|---|
| 1 | default 模式下只读 shell 命令也询问。手册承诺"读取操作自动允许"，但分类器只在 auto 模式被调用，`git status/diff/log`、`ls`、`cat` 每条都弹窗 | `engine.go:1507`、`bash.go:98`、`USER_MANUAL.md:405` |
| 2 | "总是允许"按 `git <子命令>` 记忆，`status/diff/log/add/commit/push` 各需一次 `a` | `command_prefix.go:79-95` |
| 3 | 提交信息里只要有 `( ) ; & \| < >` 中任一字符，即便在引号内，前缀规则也拒绝放行，于是 `git commit -m "fix(api): ..."` 每次都问 | `command_prefix.go:165-168` |
| 4 | 规则只存内存，重启即忘。`policies.json` 的读写代码存在，但交互提示从不写入，且 `PolicyRule` 没有命令前缀字段 | `permission_prompt.go:97-101`、`policy.go:19-27`，全仓库无 `policyEngine.AddRule` 生产调用 |
| 5 | 分类器用子串匹配，`find -delete`、`env rm`、`git -c core.fsmonitor=x status`、`npm publish -v` 判为安全；同时 `-d` 子串又让 `git diff --stat` 之类误判 | `classifier.go:42-46,110-122,170` |

修复策略（任务 A1–A4）：默认模式自动放行"整行全部为只读简单命令、无文件重定向、无替换"的 shell 命令；前缀规则在 bash/pwsh 下接受引号内的运算符字符；新增 `[p]` 永久允许并落盘；分类器改为按分词后的命令名与参数判断。

---

## 第一波

### Task A1: 分类器重写为分词判定，默认模式放行只读命令

**Files:**
- Modify: `internal/permission/classifier.go`
- Modify: `internal/engine/engine.go:1501-1510`（分类器调用）与 `toolPermissionMode`（`engine.go:1617-1624`）
- Test: `internal/permission/classifier_test.go`（新增表驱动用例）、`internal/engine/permission_default_readonly_test.go`（新建）

**Interfaces:**
- Produces: `func (c *Classifier) ClassifyLine(command string) CmdCategory` —— 对整行调用 `safety.SimpleCommands`，逐条分类后取最危险类别；任一条带文件输出重定向即 `CatUnknown`；`$(`、反引号、`<(`、`>(` 出现即 `CatUnknown`。
- Produces: `func (c *Classifier) IsReadOnlyLine(command string) bool` —— `ClassifyLine == CatSafe`。
- 保留 `Classify(cmd)` 作为单条命令入口，内部改为对 `safety.SimpleCommands` 首条的 words 判定。

- [ ] **Step 1: 写失败测试** —— 在 `classifier_test.go` 加表驱动用例：
  - 应为 `CatSafe`：`git status`、`git diff --stat`、`git log --oneline -5`、`git branch --show-current`、`ls -la`、`cat go.mod`、`grep -rn foo .`、`find . -name "*.go"`、`git status && git diff`、`go env GOPATH`。
  - 不得为 `CatSafe`：`find . -delete`、`find . -exec rm {} +`、`env rm -rf x`、`git -c core.fsmonitor=evil status`、`git branch newname`、`git diff --output=f`、`npm publish -v`、`docker run img:version`、`git status && rm -rf build`、`cat a > b`、`echo $(rm x)`、`go run main.go`、`go generate ./...`。
  - `git commit -m "x"` 为 `CatGit`；`go test ./...` 为 `CatBuild`。
- [ ] **Step 2: 运行确认失败** `go test ./internal/permission/ -run TestClassify -v`
- [ ] **Step 3: 实现** —— 用 `safety.SimpleCommands` 分词；git 判定按 `words[1]`（跳过 `-C dir`、`--no-pager` 这类前置选项，遇到 `-c` 直接 `CatUnknown`）；只读子命令白名单精确匹配，并检查 `--output`、`-o` 等写选项；`find` 出现 `-delete/-exec/-execdir/-ok/-okdir/-fprint*` 为 `CatUnknown`；`env/xargs/time/nohup/sudo` 为 `CatUnknown`；包管理器改为按 `words[1]` 判定，删除 ` -v`、`-h`、`clean`、`prune`；docker 只读子命令精确匹配；`classifyBuild` 中 `run`、`generate`、`install` 归 `CatUnknown`。
- [ ] **Step 4: 引擎接入** —— `engine.go` 分类器调用改为对 bash **和** powershell 生效；在 **default** 模式下 `IsReadOnlyLine(cmd)` 为真则 `tctx.PermissionMode = "auto"`（复用工具现有分支）并记录原因 "read-only command"；auto 模式沿用 `ShouldAutoApprove`。
- [ ] **Step 5: 引擎测试** —— `permission_default_readonly_test.go`：构造 Engine（参考 `patterns_test.go` 的构造方式），`PermissionPrompt` 设为计数器；default 模式下执行 `git status` 不触发提示，执行 `git commit -m x` 触发一次。
- [ ] **Step 6: 全量门禁** `go build ./... && go vet ./... && go test ./internal/permission/ ./internal/engine/`

### Task A2: 灾难命令扫描展开嵌套 shell

**Files:**
- Modify: `internal/safety/command.go`（`commandWords`、`catastrophicSimple`）
- Test: `internal/safety/command_test.go`

- [ ] **Step 1: 失败测试** —— 以下必须返回 ok=true：`bash -c "rm -rf ~"`、`sh -c 'rm -rf /'`、`cmd /c rd /s /q C:\`、`powershell -Command "Remove-Item -Recurse -Force C:\"`、`pwsh -c "rm -r -fo ~"`、`echo ~ | xargs rm -rf`、`find / -delete`。以下必须 ok=false：`bash -c "go test ./..."`、`git commit -m "rm -rf /"`（引号内是参数，不是命令）。
- [ ] **Step 2: 确认失败**
- [ ] **Step 3: 实现** —— 在 `catastrophicSimple` 前增加 `unwrapShell(words) (inner string, ok)`：命令名为 `sh/bash/zsh/dash/cmd/pwsh/powershell` 且带 `-c`/`/c`/`-Command`/`-c`/`-EncodedCommand`（后者直接返回危险，理由 "encoded command"）时，取其后的参数字符串递归调用 `CatastrophicCommand`；`xargs` 把其余 words 视为命令并把管道左侧的输出当作目标参数（若左侧含 `~`、`/`、盘符根等 `criticalPath` 命中则危险）；`find <path> -delete|-exec rm` 且 `<path>` 为 `criticalPath` 时危险。注意 `git commit -m "..."` 的 `-m` 参数不是命令，不得递归。
- [ ] **Step 4: 通过并门禁**

### Task A3: 前缀规则容忍引号内运算符、支持 heredoc

**Files:**
- Modify: `internal/safety/command.go`（`SimpleCommand` 增加 `Quoted []bool`，`parseShell` 记录每个词是否整词处于引号内；`<<`/`<<-`/`<<<` 识别为 here-doc/here-string，不当作输入文件，其内容不进入 words）
- Modify: `internal/permission/command_prefix.go`（`coverableCommands` 增加 shell 参数）
- Modify: `internal/permission/permission.go`（`Check` 传入当前 shell 类型）
- Test: `internal/permission/command_prefix_test.go`、`internal/safety/command_test.go`

**Interfaces:**
- Produces: `permission.ShellKind` 枚举 `ShellPOSIX | ShellPowerShell | ShellCmd`；`func ShellKindOf(s *shell.Shell) ShellKind`（放在 `internal/permission/shellkind.go`，读取 `shell.Default()` 的 `Kind`/`Path` 判断）。
- Produces: `Manager.SetShellKind(k ShellKind)`，engine 在 `New` 中调用一次。

- [ ] **Step 1: 失败测试** —— 记住 `git commit` 前缀后，POSIX 下应放行：`git commit -m "fix(api): handle 429; retry"`、`git commit -m 'a && b'`、`git commit -m "line1" -m "line2"`、`git commit -F- <<'EOF'\nmsg\nEOF`；不得放行：`git commit -m "x" && rm -rf build`、`git commit -m "x" > out.txt`、`git commit -m "$(rm x)"`、`git commit -m x; rm y`。cmd 下 `git commit -m 'a;b'` 不放行（单引号在 cmd 里不是引号）。
- [ ] **Step 2: 确认失败**
- [ ] **Step 3: 实现** —— `coverableCommands` 对 `Quoted[i]==true` 且 shell 为 POSIX/PowerShell 的词跳过运算符检查；heredoc 体不参与判定。
- [ ] **Step 4: 通过并门禁**

### Task A4: 授权提示增加"永久允许"并落盘，模式语义分层

**Files:**
- Modify: `internal/permission/policy.go`（`PolicyRule` 增加 `CommandPrefix string json:"command_prefix,omitempty"`、`InputEquals map[string]string json:"input_equals,omitempty"`、`Scope string json:"scope,omitempty"`；新增 `func (r PolicyRule) ToRule() (Rule, bool)`）
- Modify: `internal/permission/storage.go`（`Save` 改用 `fsatomic.WriteFile`）
- Modify: `internal/permission/policy.go:103-106`（无规则命中时一律返回 `ActionAsk`，去掉 auto 特判；对应修正 `policy_test.go:37`）
- Modify: `internal/engine/engine.go:317-325`（加载持久规则后，把 allow 类 `PolicyRule` 通过 `ToRule` 注入 `e.perm`；scope 非空且不等于当前项目根时跳过）；新增 `func (e *Engine) PersistPermissionRule(rule permission.Rule, scope string) error`
- Modify: `internal/engine/engine.go:1617-1624`（`toolPermissionMode`：auto 模式下 write/edit 目标在项目内则返回 "auto"）
- Modify: `cli/cove/permission_prompt.go`（选项 `[y] 允许 [a] 本次会话总是允许 … [p] 永久允许 … [n] 拒绝`；`p` 同时加内存规则和持久化）
- Test: `internal/permission/policy_test.go`、`cli/cove/permission_prompt_test.go`、`internal/engine/permission_persist_test.go`（新建）

**Interfaces:**
- Consumes: A3 的 `ShellKind`。
- Produces: 持久文件格式 `~/.cove/policies.json` 数组元素示例 `{"id":"allow-bash-git commit","tool_pattern":"bash","action":"allow","enabled":true,"command_prefix":"git commit","scope":""}`。

- [ ] **Step 1: 失败测试** —— (a) `PolicyRule{CommandPrefix:"git commit"}.ToRule()` 得到 `Rule{ToolPattern:"bash",CommandPrefix:"git commit"}`；(b) 引擎用临时 HOME 启动，`PersistPermissionRule` 后新建引擎能免询问执行 `git commit -m x`；(c) `permissionAnswerDecision("p")` 返回 allow=true, persist=true；(d) `Evaluate(..., "auto")` 无规则时返回 `ActionAsk`。
- [ ] **Step 2: 确认失败**
- [ ] **Step 3: 实现**（含把 `permissionAnswerDecision` 返回值扩为 `(allow, always, persist bool)`）
- [ ] **Step 4: 模式语义** —— default：只读工具与只读命令自动，其余询问；auto：另加 `CatBuild` 命令与项目内 write/edit 自动，`CatGit/CatInstall/CatUnknown` 询问；bypass：全部。写入 `doc-notes-A.md`。
- [ ] **Step 5: 通过并门禁** `go test ./internal/permission/ ./internal/engine/ ./cli/cove/`

### Task C1: edit 模糊命中的缩进对齐与显式反馈

**Files:**
- Modify: `internal/tool/edit.go:175-257`
- Test: `internal/tool/edit_fuzzy_test.go`（新建）

- [ ] **Step 1: 失败测试** —— (a) 文件用 4 空格缩进、`oldString` 用 tab，模糊唯一命中：结果消息包含 `fuzzy whitespace match`、行号范围，且写入内容已按文件缩进重排；(b) 命中块内各行缩进宽度不一致且 `newString` 多行：返回错误，提示重新 read；(c) 精确匹配 3 处且未开 replaceAll：错误信息列出三个行号；(d) 成功后消息附带改动后前后各 2 行片段。
- [ ] **Step 2: 确认失败** → **Step 3: 实现** → **Step 4: 通过** `go test ./internal/tool/ -run Edit`

### Task C2: bash/powershell 超时保留输出、编码、上限、颜色

**Files:**
- Modify: `internal/tool/bash.go`、`internal/tool/powershell.go`、`internal/tool/stream_exec.go`、`internal/shell/shell.go`（`Env` 增加 `NO_COLOR=1`、`TERM=dumb`、`CLICOLOR=0`、`FORCE_COLOR=0`）
- Test: `internal/tool/bash_timeout_test.go`（新建）、`internal/shell/env_test.go`

- [ ] **Step 1: 失败测试** —— (a) 超时命令（`sleep 5` / `Start-Sleep 5`，timeout=300ms）返回内容包含已产生的 stdout 与 `[timed out after 0.3s]`，`IsError=true`；(b) `timeout` 大于 10 分钟被夹到 10 分钟；(c) `shell.Env` 含 `NO_COLOR=1`；(d) Windows 下 bash 输出为 GBK 字节时被解码为 UTF-8（构造 GBK 字节直接调用输出处理函数）；(e) 捕获缓冲上限 2MB，超出保留首尾。
- [ ] **Step 2–4** 实现并通过。stdout/stderr 缓冲改为有界 `textutil` 首尾保留；Windows 且非 PowerShell 时对非 UTF-8 输出尝试 `decodeGBK`（复用 `internal/tool/gbk.go`）；cmd 前缀 `chcp 65001>nul &`。
- [ ] **Step 5: 描述更新** —— bash/powershell 的 Description 写明默认超时 120s、上限 10min、cwd 不保留、当前 shell 类型（从 `shell.Default().Describe()` 动态拼）。

### Task C3: read 与截断协同、超长行、grep 参数

**Files:**
- Modify: `internal/tool/read.go`（单行超过 2000 字符截断并标注；末尾续读提示改为固定格式 `[next: offset=N]`）
- Modify: `internal/tool/grep.go`（新增 `-i`、`context`、`files_only` 参数；输出相对路径；rg 加 `--max-count`；内置实现到上限即停）
- Test: `internal/tool/read_longline_test.go`、`internal/tool/grep_params_test.go`

- [ ] **Step 1: 失败测试** —— read：一行 5000 字符被截为 2000 并附 `…[+3000 chars]`；输出末尾含 `[next: offset=2001]`。grep：`ignore_case=true` 匹配大小写不同；`context=1` 输出含上下文行；`files_only=true` 只输出路径；路径为相对 cwd。
- [ ] **Step 2–4** 实现并通过。engine 侧配套（截断时保留最后一行的 `[next: offset=N]`）留给任务 B3。

### Task C4: 精简工具面

**Files:**
- Modify: `cli/cove/registry.go`（非 Windows 不注册 powershell；`lsp`、`cron` 不再注册；`task` 描述改为"记录一个待办任务，不会执行"）
- Modify: `internal/tool/extra_tools.go`（todowrite 的 status/priority 加 enum）、`internal/tool/advanced_tools_task_core.go`（task_update status 加 enum）、`internal/tool/advanced_tools_agent_skill.go`（agent type 加 enum）
- Modify: `internal/tool/tool.go:36-37`（删除 `AlwaysAllowRules`/`AlwaysDenyRules`）
- Test: `cli/cove/registry_test.go`

- [ ] **Step 1: 失败测试** —— 注册表不含 `lsp`、`cron`；`runtime.GOOS!="windows"` 时不含 `powershell`（用 build tag 或注入 GOOS 参数）；todowrite schema 含 `"enum":["pending","in_progress","completed"]`。
- [ ] **Step 2–4** 实现并通过；`go vet ./...` 确认删除字段后无引用。

### Task D1: 会话存储改为 JSONL 追加 + 索引

**Files:**
- Modify: `internal/session/store.go`（新格式：`<id>.jsonl` 每行一条消息，首行为元数据；`index.json` 存 id/title/cwd/turns/preview/updated_at；`Save` 只追加新消息并更新索引；`Load` 兼容旧 `.json`）
- Modify: `internal/engine/engine.go:1941-1961` 调用处 **仅允许改函数名/参数**（与 A 批协调：此处改动由 D 批在 A 批完成后用 Edit 精确替换）
- Test: `internal/session/store_jsonl_test.go`

- [ ] **Step 1: 失败测试** —— 追加 100 条后文件行数为 101；`List()` 不解码消息体（用 1MB 消息验证耗时 < 50ms）；旧 `.json` 会话可 `Load` 并在下次 `Save` 时迁移为 `.jsonl`；`Prune(keep=50)` 删除最旧的。
- [ ] **Step 2–4** 实现并通过。

### Task D2: 死依赖与死代码清理

**Files:**
- Modify: `go.mod`/`go.sum`（`go mod tidy`）
- Delete: `internal/telemetry/`；Modify: `internal/config/config.go`（删除 `Telemetry` 字段，`Load` 忽略未知字段不报错）、`docs/config.example.json`
- Modify: `internal/hooks/`（新增 `LoadUserConfig(home string) ([]HookDef, error)` 读取 `~/.cove/hooks.json`，格式 `{"hooks":{"BeforeTool":[{"matcher":"bash","command":"..."}]}}`；`cli/cove/app_bootstrap.go` 在构造 hookMgr 后调用注册；不读项目级配置）
- Modify: `internal/render/render.go:394-400`（删除 `ExpandHint` 与 `/x` 文案）、删除 `render.Expanded` 若无调用方
- Test: `internal/hooks/config_test.go`、`internal/config/telemetry_removed_test.go`

- [ ] **Step 1: 失败测试** —— hooks 配置文件被加载后 `Fire(BeforeTool,"bash")` 执行了配置的命令（用写临时文件的命令验证）；含 `"telemetry":{}` 的旧配置仍能 Load。
- [ ] **Step 2–4** 实现并通过；`go mod tidy` 后 `go build ./...`。

### Task D3: 交互模式 Markdown 渐进渲染

**Files:**
- Create: `internal/render/markdown.go`（基于 goldmark 的行级渲染器：标题加粗、`**` 粗体、行内代码反色、围栏代码块缩进+dim 边框、列表符号 `•`；输入按行流式，代码块状态跨行保持）
- Modify: `cli/cove/chat_interaction.go:225-260`（增量文本经渲染器后再 `StreamPrint`）
- Modify: `internal/render/blocks.go:53`（宽度改为 `termui.Width()` 实时值，回退 120）
- Test: `internal/render/markdown_test.go`

- [ ] **Step 1: 失败测试** —— 输入 "## 标题\n正文 **粗** `code`\n```go\nfmt.Println()\n```\n- 项" 分三次喂入，拼接输出含 ANSI 粗体的"标题"、代码块行带缩进、列表行以 `•` 开头；`textmode.PreferASCII()` 时不含非 ASCII 符号。
- [ ] **Step 2–4** 实现并通过。

### Task D4: 浏览器 SSRF 与 safeurl 地址段

**Files:**
- Modify: `internal/browser/chrome_enabled.go`（启用 `fetch.Enable`，每个请求经 `safeurl.IsPrivateURL` 判定，私网则 `fetch.FailRequest`）、`internal/safeurl/safeurl.go:29-38`（补 `0.0.0.0/8`、`64:ff9b::/96`、`198.18.0.0/15`、`224.0.0.0/4`、`240.0.0.0/4`、`ff00::/8`）
- Test: `internal/safeurl/safeurl_test.go`、`internal/browser/chrome_intercept_test.go`（`-tags chromedp` 下才编译；无标签时只测判定函数）

- [ ] **Step 1: 失败测试** —— `IsPrivateURL("http://0.0.0.0/")`、`http://[64:ff9b::a00:1]/`、`http://198.18.0.1/`、`http://224.0.0.1/` 为真；抽出的 `shouldBlockRequest(url string) bool` 对上述为真。
- [ ] **Step 2–4** 实现并通过。

### Task D5: CI 与构建脚本

**Files:**
- Modify: `.github/workflows/ci.yml`（golangci-lint 固定版本 `v2.1.6`；删除 TEMPORARY 步骤）、`.golangci.yml`（启用 `unused`、`gosec`、`revive`、`errorlint`）、`.github/workflows/release.yml`（矩阵加 `linux/arm64`、`windows/arm64`）、`build.bat`（版本号从 `cli/cove/main.go` 的 `Version` 常量读取或从参数传入）
- [ ] **Step 1:** 本地运行 `golangci-lint run ./...`（若未安装则用 `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.1.6 run`）记录基线问题数到 `doc-notes-D.md`；`unused` 报告的死代码逐项删除或加注释说明。
- [ ] **Step 2:** `build.bat` 在 PowerShell 中执行一次，`cove.exe --version` 输出与 `main.go` 一致。

---

## 第二波（A 批完成后开始）

### Task B1: 每回合只刷新 git 信息，不重扫仓库

**Files:**
- Modify: `internal/context/context.go`（新增 `RefreshGitAll()`：branch/status/log 三项）、`internal/engine/engine.go:826-830`
- Test: `internal/engine/collect_per_turn_test.go`

- [ ] **Step 1: 失败测试** —— 注入计数的 `collectContext`，连续两回合后计数为 1（首次），`RefreshGit` 被调用 2 次。
- [ ] **Step 2–4** 实现并通过。

### Task B2: 用真实 token 数驱动压缩，统一估算器

**Files:**
- Modify: `internal/engine/engine.go`（`countTokens` 改为：`lastUsage.InputTokens + token.Estimate(自上次响应后新增消息)`，无 usage 时回退 `token.Estimate` 全量；计入 system/tools/reasoning）、`internal/engine/compressor.go:37`（阈值改为 `EffectiveCompactionBudget` 的 0.8）、删除按字节 /4 的实现
- Test: `internal/engine/token_count_test.go`

- [ ] **Step 1: 失败测试** —— 200K 窗口、上一次响应 `InputTokens=100000`、新增 1000 字中文时不触发压缩；`InputTokens=170000` 时触发。
- [ ] **Step 2–4** 实现并通过。

### Task B3: 错误结果也截断；保留 read 续读提示

**Files:**
- Modify: `internal/engine/engine.go:1588-1595`、`internal/token/token.go`（新增 `TruncateKeepTail(s string, limit int, tailLines int)`）
- Test: `internal/engine/truncate_error_test.go`

- [ ] **Step 1: 失败测试** —— 10 万字符错误结果被截到 `toolOutputLimit` 内且仍以 `Error` 开头；read 结果被截时输出仍以 `[next: offset=N]` 结尾（N 为实际保留到的行号 +1，需从内容反推：read 每行以 `N\t` 开头）。
- [ ] **Step 2–4** 实现并通过。

### Task B4: 并行调度先等安全组再跑串行组

**Files:**
- Modify: `internal/engine/engine.go:1196-1246`
- Test: `internal/engine/parallel_order_test.go`

- [ ] **Step 1: 失败测试** —— 一批 `[read(慢 200ms), bash]`，记录开始/结束时间，bash 开始时间必须晚于 read 结束时间；`[edit a, edit b]` 仍并行（总耗时 < 两者之和）。
- [ ] **Step 2–4** 实现：先启动所有 safe 项，`wg.Wait()`，再顺序执行非 safe 项与 deferred。

### Task B5: 重试 jitter、Retry-After 全格式、缓存计价

**Files:**
- Modify: `internal/api/retry.go`（延迟乘 `0.5+rand.Float64()`）、`internal/api/errors.go:105-118` 与 `internal/api/keypool.go:157-166`（解析 HTTP-date、`retry-after-ms`、`anthropic-ratelimit-*-reset`）、`internal/api/anthropic.go:44`（非流式超时降为 120s 且传输超时不重试生成）、`internal/cost/tracker.go:158`（cache_creation 按 1.25 倍）、删除 `api/fallback.go` 中从未赋值的 `ProviderWithStatus.Model`、`engine/message_processor.go` 的 `BuildMessageGraph`、`api/adapter` 中无生产调用的 `Message`/`MergeReasoning`/`HasParseError`
- Test: `internal/api/retry_test.go`、`internal/cost/cache_price_test.go`

- [ ] **Step 1: 失败测试** —— 100 次采样的退避延迟落在 `[0.5d,1.5d]` 且不全相等；`Retry-After: Wed, 21 Oct 2026 07:28:00 GMT` 解析为正时长；`retry-after-ms: 1500` 为 1.5s；1000 个 cache_creation token 的费用 = 1250 个 input token 的费用。
- [ ] **Step 2–4** 实现并通过。

### Task B6: 测试提速

**Files:**
- Modify: 各包中耗时最长的测试（先用 `go test -json ./... | 统计 elapsed` 找出前 20 个）
- 目标：全仓库 `go test ./...` 从 79s 降到 30s 以内；不删除断言，只替换真实 sleep/网络/git 进程为可注入的时钟或假实现；无法加速的加 `testing.Short()` 跳过并保留在 CI 全量运行。
- [ ] **Step 1:** 记录基线 → **Step 2:** 逐个改造 → **Step 3:** `go test -count=1 ./...` 计时并记录到 `doc-notes-B.md`。

### Task B7: 文档统一更新

**Files:**
- Modify: `docs/USER_MANUAL.md`（权限模式表、提示选项 `[p]`、持久化文件名改为 `policies.json`、bash 工具说明、hooks 配置格式、删除 TUI 描述）、`README.md`、`CHANGELOG.md`（新增 11.1.0 条目）、删除 `docs/bubbletea-migration-plan.md`
- 输入：四份 `doc-notes-*.md`。
- [ ] **Step 1:** 合并笔记 → **Step 2:** 更新 → **Step 3:** `rg -n "Bubble Tea|bubbletea|TUI" docs README.md` 只剩历史 CHANGELOG 条目。
