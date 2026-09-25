## [11.3.0] - 2026-09-25

> 即将发布的版本（第四轮：任务循环借鉴项与后台功能门槛）；源码中的 `Version` 发布时再同步。

> **升级注意**：`/budget <金额>`、`/budget auto` 不再写 `config.json`，只改本会话，写配置改用 `/budget save`（`/config budget <n>` 仍直接写配置）。实验性协作工具（`task*`、`team_*`、`send_message`、`brief`、`sleep`）默认不再注册，需要时配置 `"experimental_tools": true`。检查点每项目只保留最近 50 个，裁剪会改写被保留检查点的哈希。新记忆写入 `~/.cove/projects/<hash>/memory`，旧的 `~/.cove/memory` 仍作为全局记忆只读合并。

### Added
- **停后自检**（`internal/engine/nudges.go` 集中定义上限）：模型不调工具准备结束时，依次检查空回复（注入 `[system: Your response was empty. Provide the answer or call a tool.]`，每轮 2 次）、宣告下一步却没做（`[system: You announced a next step but did not perform it. …]`）、做完工作后只回一句话（少于 40 字符，含中文 20 字：`[system: Your last message is too brief to be a final answer after doing work. …]`），后两者每轮合计 2 次。宣告识别只看最后一段的句首/行首，问句、第二人称、建议、否定和完成声明都不触发；退化结尾只在运行过非只读工具后判定。已取消、预算用尽或下一次调用会撞上限时不注入。
- **完成前目标自检 `done_check`**（`auto`|`on`|`off`，默认 `auto` = 快速档模型或非 anthropic provider）：本轮改过文件时，第一次准备结束前注入一次 `[system: Before finishing, check whether the user's request has been fully met. …]`。
- **收尾总结**：迭代/时间上限选停止、`-p`/headless 撞硬上限、停滞或循环询问选停止、循环硬停时，再做一次不带工具的调用（`max_tokens` 1024），总结已完成、剩余与下一步；写入历史，`-p` 先打到 stdout，退出码不变。
- **中断标记**：回合被中断时在历史里写一条 `[system: The previous turn was interrupted (<reason>). …]`，同一次中断只写一条。
- **循环检测第二次询问**（仅交互）：一轮内第 2 次命中 L1/L2 时询问 `[c] 本轮禁用循环检测并继续 / [s] 停止`。
- **预算提醒**：迭代或时间窗口用掉 80% 时向模型追加一次 `[budget: about N model calls / M minutes remain in this window. …]`；会话费用到 `max_budget_usd` 的 80% 时终端提示一次。
- **子智能体结构化结果**：`exit_reason`（`completed|max_iterations|interrupted|error|loop`）与 `truncated`；`agent` 工具首行 `[exit: <reason>, steps: N, truncated: yes/no]`；`execute_plan` 汇总新增“汇总：N 个任务完成，…”一行。
- **相同工具结果去重**：≥512 字节、完全相同的第 2 份及以后的工具结果替换为 `[identical to earlier tool result for call <id> (<n> bytes); content omitted]`（最近 4 条不动；只在落盘遮蔽本就改写历史或能省 ≥2000 token 时执行）。
- **dream 对话结束即整理**：`dream.json` 新增 `trigger`（默认 `session_end`，`threshold` 为旧行为）与 `min_turns`（默认 2，按本进程成功完成的回合计）；退出时以分离的后台进程整理（日志 `dream.log`、结果 `dream-last.json`），启动失败时内联执行最多 60 秒；`/dream` 显示触发方式、上次会话结束整理结果与 token/费用；项目记忆目录一并整理。
- **本会话记忆即时生效**：新提取的记忆在下一回合以 `<session_memories>`（≤2KB）附在用户消息后；记忆超过 24KB 时每回合附 BM25 检索的 `<relevant_memories>`（≤4KB），合并为 `<turn_memories>`（≤6KB）。
- **自动技能落盘**：后台回顾学到的技能写到 `~/.cove/skills/auto-<slug>-<hash>/SKILL.md`，不覆盖、不遮蔽用户/项目/插件/内置技能；摘要行显示“新增技能 X”“更新技能 X”。
- **指令文件**：从 git 根到当前目录逐层加载 `CLAUDE.md`、`.claude/CLAUDE.md`、`AGENTS.md`、`.cove.md`，去重合并，上限 32KB。
- **按项目分目录的记忆**：`~/.cove/projects/<hash>/memory` + 全局 `~/.cove/memory`（只读合并，项目优先）；`/memory list` 标注 `(项目)`/`(全局)`/`(指令文件)`。
- **`/budget off`、`/budget save`**；**`/hooks`** 列出已加载的钩子；**`/init`** 由模型起草 CLAUDE.md，以 diff 展示，`/init apply` / `/init discard`。
- **新配置**：`done_check`、`experimental_tools`、`web_search`（`provider` = tavily|brave|duckduckgo、`api_key`；`/diagnose` 提示未配置）。
- **完成校验命令推断**：新增 `dotnet build --nologo -v q`（sln/csproj）、`npm run build --if-present`（有 build 脚本）、`python -m compileall -q -x … .`；首轮开始时打印“完成校验命令：…”。
- **模型状态行**：`model_fast` 与 `model` 不同时，交互回合开始显示“模型：xxx”。
- **摘要行**新增“已建检查点，/undo 可回退”、新增/更新技能。
- **MCP**：首次意外断线 2 秒后自动重连一次；处理 `notifications/tools/list_changed`；工具定义缓存随 MCP 连接变化失效。
- **`repo_map` 工具**：按 `query`（路径片段/标识符）与 `path` 查询代码大纲（类型/函数/方法签名与行号），输出 ≤12KB，只读、所有模式注册；支持 Go/Python/TypeScript/JavaScript，其他语言提示用 grep/glob。
- **首个任务回合的代码摘录**：每会话一次，在第一个带路径、标识符或反引号代码的任务回合附带 ≤12KB 的 `<repo_map_excerpt>`（不进系统提示词）；纯中文闲聊或提取不出检索词时不注入、不占名额。
- **指令文件截断提示**：CLAUDE.md/AGENTS.md/.cove.md 合计超过 32KB 被截断时，终端提示一行（每会话一次）。
- **`question` 工具在 REPL 可用**：问题显示在输入行上方，下一行输入即回答；Ctrl+C 立即取消且不吞掉后续输入。
- **`dream.json` `min_interval_minutes`**（默认 0 = 不限）：`session_end` 模式下两次整理的最短间隔；退出时的整理提示注明“至多约 30 次后台模型调用”。

### Changed
- **系统提示词瘦身**：`<repo_map>` 全量层、`Repository Micro-Map` 与文件树移出系统提示词，改为 ≤4KB 的 `<project_outline>`（语言、目录、入口、构建/测试命令；排序稳定、无时间戳，不破坏 prompt 缓存）；记忆索引上限 4KB。基线：本仓库系统提示词 51.7KB → 13.1KB（−74.6%）。
- **`/context`**：启动时不再扫描文件树与代码地图；`/context` 首次使用时后台生成项目结构并缓存，最多等 5 秒，超时提示稍后再看。
- **记忆提取每回合都跑**：去掉 2 分钟节流，保留“进行中跳过”“无新消息跳过”“少于 4 条消息不提取”。
- **后台回顾只生成技能**，不再写记忆；`cove -p` 不等回顾。
- **记忆注入**：全文注入阈值 8KB → 24KB，超过时索引 + BM25 top-K；记忆总量上限 100KB → 300KB；自动追加超过 10KB 时滚动写入 `name-2.md`，不再截断新内容，与末尾相同的行不重复写。
- **`/compact`** 强制摘要（4 条消息即可），打印“已压缩：压缩前 X tokens → 压缩后 Y tokens。”，部分压缩或未压缩时如实说明；自动压缩时终端提示一行。
- **压缩摘要保留原始需求**：首条真实用户消息保留 2000 字符（其余用户 600、助手 250、工具 100）。
- **验证门禁**跳过本轮已由模型成功运行、且之后没有改动文件的同一命令。
- **权限拒绝文案**统一追加 `Do not call this tool again with the same input; explain the situation to the user or choose a different approach.`
- **模型路由**：长度按字符数计（原按字节），阈值 0.40 → 0.35。
- **工具面**：`question` 只在交互式界面注册；默认构建（无 chromedp）不注册 `browser`；`execute_plan` 的 `max_agents`（1–8）生效；实验性协作工具归入 `experimental_tools`（默认关闭）。
- **子目录 AGENTS.md 等提示**：单文件 2000 字节 → 12KB，一次调用最多 24KB；`grep`/`glob` 的 `path` 也会触发；压缩后重置。
- **检查点保留**：每项目最近 50 个（超过 60 个时后台裁剪，保留的检查点哈希会变）；每 20 次创建后台 `git gc --quiet`（不带 `--prune=now`，2 分钟超时）。
- **会话清理 `max_sessions`** 改为按项目（git 根）分别计数。
- **会话笔记**移到 `~/.cove/projects/<hash>/session_notes.md`（自动迁移）；不再记录 `File:` 与工具错误；中文决策/发现识别收紧并去重。
- **guardrail 30 秒熔断**按工具名分别计数。
- **任务分解提示**长度门槛按字符数（300 字符）。
- **MCP 工具 schema** 超过 4KB 时依次去掉 description → 只留类型 → 只列属性名，不再在 JSON 中间截断。
- **`dream.json`** 跟随 `COVE_CONFIG_DIR`。
- `/help` 不再列出 `/history clean`（命令仍可用）。

### Fixed
- 快速会话的最后几个回合因记忆提取节流而丢失。
- 中文消息按字节计长度被路由到高级模型。
- `/compact` 不满足阈值时实际没有压缩却提示“已压缩”。
- 后台回顾的 MEMORY 分支与记忆提取重复写入；`Save` 错误被吞掉。
- 检查点 `git gc --prune=now` 可能删掉并发写入中的对象；gc 持锁无超时。
- 追加记忆时遮蔽同名全局记忆（现在以全局内容为底写入项目目录）。
- 整理工作者 panic 或被强杀时整理锁不回滚。
- `~/.cove/tool-outputs` 无限增长：启动时删除 7 天前的文件。
- 提示（nudge）、完成前自检或验证门禁让回合继续时，第一段回复与第二段回复在流式输出里粘在一起：现在中间空一行，自检时另显示暗色“（自检中…）”。`-p` 只输出最终回答，不变。
- `cove -p` 仍会发出技能回顾请求并在退出时丢弃（白花一次调用）：`-p` 不再回顾；交互模式下回顾改为距上次至少 3 个回合、且本回合用过非只读工具时才触发。
- 交互退出（`/exit`、Ctrl+D）不等最后一回合的记忆提取：现在最多等 10 秒，仍在提取时 stderr 显示“正在保存本轮记忆…”。
- 验证门禁把 120 秒超时当成构建失败（打回重试并升级模型）：超时现在只提示、不算失败、不重试不升级；`dotnet`/`npm` 命令默认 300 秒，新增 `done_verify_timeout_seconds`。
- TypeScript 项目的自动校验同时跑 `tsc --noEmit` 和 `npm run build`：检测到 tsc 时不再追加 `npm run build`。

## [11.2.0] - 2026-09-25

> 即将发布的版本（第三轮遗留项）；源码中的 `Version` 发布时再同步。

> **升级注意**：新增的 `max_sessions` 默认 200 且默认开启——升级后第一个回合结束时，超过最近 200 个的旧会话会被直接删除（当前会话除外；回合结束摘要行显示“已清理 N 个旧会话（max_sessions=200）”，日志记一条警告）。要保留全部历史，升级前在 `config.json`（或 `.cove.json` / profile）中设置 `"max_sessions": -1`。

### Added
- **单轮上限改为可询问的软上限**：新配置 `max_iterations`（默认 200）、`max_turn_minutes`（交互模式默认 60，`0` 关闭；`-p`/headless 仅显式配置时生效）、`subagent_max_iterations`（默认 60，原硬编码 30）。交互模式到达迭代上限时暂停并显示已调用次数、用时、本轮费用与最近 5 个工具，询问 `[c] 继续 N 次 / [s] 停止`；时间上限同样询问（`[c] 继续 N 分钟`）；连续 60 次迭代没有文件读写时询问一次，之后本轮不再问。回答 `c`/`继续`/`y` 继续同样大小的窗口，其他输入、Ctrl+C 或 15 分钟无应答视为停止（超时提示“等待超时，已按停止处理”）。停止后只显示一行“本轮已停止：已达到单轮最大迭代次数 N。输入 /continue 可继续；可通过配置 max_iterations 调整”（不带 “Request failed” 前缀，也不再重复 /continue 提示）。
- **`--max-turns <N>`**（仅与 `-p` 连用）：`-p` 的迭代上限为硬上限，`--max-turns` 覆盖 `max_iterations`，`0` 不限制；到达上限时 stderr 输出 `Error: 已达到单轮最大迭代次数 N（可用 --max-turns 调整）`，退出码 1。`-p` 与 headless 默认不施加时间上限，只有配置（`config.json`、项目 `.cove.json` 或所选 profile）显式写了 `max_turn_minutes` 时才生效；停滞检测在 `-p` / headless 下只记日志。
- **`/continue`**：上一轮因上限、Ctrl+C、API 错误等中断后，从中断处续跑，已完成的工具步骤及结果保留、不重做；没有可继续的回合时给出提示。Ctrl+C 中断提示改为“[已中断] 正在停止当前任务…输入 /continue 可继续”。headless 模式不支持。
- **回合结束摘要行**：交互模式下，后台工作有值得说的事（本轮提取到记忆、dream 门槛变化、`max_sessions` 清理了旧会话、会话保存失败）时打印一行淡色摘要，如 `已提取 2 条记忆 · dream 还差 2 个会话`；回合之间到达立即显示，回合进行中到达则在该轮输出结束后显示，不插入回答中间；dream 状态在首个回合只记基线；`-p`、headless 与非终端不输出。
- **`/dream status` / `/dream run`**：`status`（或无参数）显示是否启用、距上次整理、时间与会话门槛还差多少、正在整理或上次运行结果；`run` 忽略门槛立即在后台整理（仍需启用且拿到整理锁）。
- **`/memory list` / `/memory stats` 增强**：list 显示“共 N 条”及每条的大小与首行摘要；stats 新增“上次提取: 时间，保存 N 条”（记录在 `~/.cove/memory/.last-extraction.json`）。
- **`/doctor` 与 `/diagnose` 新增两项**：“后台学习”（dream 摘要、记忆条数/大小、上次提取；上次整理失败时警告 E5006）与“权限规则文件”（`policies.json` 无法读取或解析时报 E3004）。均读取当前会话的实际状态。
- **`max_sessions`**（默认 200，负数关闭）：回合结束时自动删除更早的会话，当前会话永不删除，每进程最多每 10 分钟清理一次；有删除时摘要行显示“已清理 N 个旧会话（max_sessions=200）”，进程内首次删除另记一条警告日志。
- **hooks `SessionEnd` 生效**：退出时（`/exit`、Ctrl+D、headless 读完输入、`-p` 结束）触发一次并等待完成（含 async，总上限 30 秒），配置了该 hook 时 stderr 提示“正在运行 SessionEnd hook…”；hook stdin JSON 新增 `session_id`、`cwd`。
- **子智能体部分结果**：子智能体到达上限时返回已完成步骤列表（工具名 + 主要参数，失败标注）与最后一次模型输出，前缀 `Sub-agent did not finish: 已达上限（N 次模型调用），以下为部分结果`。`execute_plan` 中到达上限的任务标记失败但保留部分结果（计划汇总显示“部分结果:”），不再整体重试。

### Changed
- **权限判定顺序**：deny → plan → bypass → **ask → allow** → 模式默认。`ask` 规则不再被同一命令的 `allow`（`[a]`/`[p]`、整工具允许、`policies.json` allow）盖过；`policies.json` 同优先级 deny > ask > allow，与书写顺序无关；`bypass` 下 `ask` 仍不生效。
- **deny / ask 规则按归一化命令匹配**：程序名去目录与 `.exe`、不区分大小写；剥掉 `sudo`/`doas`/`env`/`nohup`/`time`/`command`/`nice`/`exec`/`busybox`/`timeout`（含选项）与 `VAR=value`；git/docker/kubectl/helm/go/npm 等跳过子命令前的全局选项。deny `git push` 现在命中 `git -C . push`、`/usr/bin/git push`、`sudo git push`、`command git push`。allow 规则仍严格逐词比较。
- **cmd.exe 回退不自动放行**：`bash` 工具回退到 cmd 时，`default` 与 `auto` 模式都不再自动放行任何命令（`dir`、`git status`、`go test` 也会询问）；Git Bash / PowerShell 不变。
- **`[p]` 规则随项目切换**：写入 `policies.json` 的规则作为磁盘规则装入本会话，`/cd` 到其他项目时一起卸下并按新项目重新加载；写入失败时退化为会话规则并提示“未能写入，仅本次会话有效”。`/cd` 后新项目的 `policies.json` 无法解析时给出警告，沿用切换前的规则。
- **dream 默认门槛**：12 小时 / 3 个会话（原 24 小时 / 5 个）；整理改在记忆提取之后进行；恢复会话后正在使用的会话不计入门槛；退出时正在运行的整理被取消并回滚锁。
- **`-p` 收尾**：回答后最多等待 20 秒让记忆提取完成；`-p` 进程内不再自动 dream。
- **会话文件首行元数据**：标题/模型/目录变化时随下一次保存重写；tokens/费用最多每 16 次保存刷新一次。
- **edit 模糊匹配缩进单位**：从整个文件推断（相邻行缩进增量的众数）；块内多种深度时用块自身单位，单一深度时仅当整文件单位能映射到与 oldString 相同层级数才使用。
- **grep 溢出提示**：装有 ripgrep 时为 `... at least N more matches|files|lines not shown (...)`，某文件达到单文件上限时注明 `some files hit the per-file cap`。
- **shell 实时进度**：Windows（非 PowerShell）下按行解码 GBK 再推送；无换行的长行超过 4KB 也推送且不切断字符；输出超 2MB 截断时按行边界切。
- **git 子进程环境**：通过 `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n` 设置 `core.fsmonitor=false`（已有序号时顺延），`GIT_PAGER=cat` 保持。
- **会话中 `git init` 后**下一轮即可获得 git 状态（原需重启）。

### Fixed
- 旧 `/dream` 只把锁文件时间戳改为“刚整理过”，实际不整理，反而推迟下一次自动整理。
- 工具在串行路径（单个调用、批内非并发安全调用、同文件延迟写）中 panic 会使整个进程崩溃；现在变为该调用的 `Error: tool panicked: …` 结果。
- “项目内写入”判断会解析符号链接与 Windows junction；项目内指向项目外的链接、超过 40 跳或成环的链接链一律视为项目外（原超限时 fail-open）。
- `build.bat` 版本号只取 `Version = "..."` 引号内内容；`.golangci.yml` 去掉与 standard 重复的 `unused`。

### Known limitations
- 仓库自身 git 配置（如 `diff.external`、`.gitattributes` 的 `diff.<driver>.command` / `textconv`）指定的外部程序仍可能随自动放行的只读 git 命令（`git diff`/`git show`/`git log -p`）执行；清空这些配置会破坏普通 `git diff`，因此未加固。
- `deny`/`ask` 前缀规则不展开 `bash -c "…"`、`xargs` 等内联脚本中的命令。

## [11.1.0] - 2026-09-25

> 即将发布的版本；源码中的 `Version` 仍为 11.0.0，发布时再同步。

### Added
- **授权提示 `[p] 永久允许（本项目）`**：选项变为 `[y] 允许 / [a] 本次会话总是允许 / [p] 永久允许 / [n] 拒绝`。`[p]` 在本次会话生效的同时把允许规则写入 `~/.cove/policies.json`（设置了 `COVE_CONFIG_DIR` 时位于该目录下，确认提示显示实际路径），规则带 `scope` = 项目根，只在该项目生效；写入为原子操作，文件损坏时不会被覆盖。
- **`policies.json` 规则字段**：`command_prefix`（shell 命令前缀）、`input_equals`（参数精确匹配，用于 MCP 服务器 + 工具名）、`scope`（项目根，空 = 所有项目）。
- **用户级 hooks**：读取 `~/.cove/hooks.json`（不读项目级），支持 `BeforeTool`/`AfterTool`/`SessionStart`/`SessionEnd`（及别名 `PreToolUse`/`PostToolUse`），`matcher` 匹配完整工具名，`command` 用 bash 工具同款 shell 执行，`BeforeTool` 可通过 `{"continue": false}` 阻止调用，支持 `timeout`、`async`。
- **交互模式 Markdown 渐进渲染**：标题、粗体、行内代码、围栏代码块、列表与引用边流式边渲染；不支持 Unicode 的控制台自动降级为 ASCII（`COVE_TUI_ASCII=1` 可强制）。`-p`/headless 输出不受影响。
- **grep 参数**：`ignore_case`、`context`（0–10）、`files_only`；输出路径统一相对当前工作目录。
- **edit 反馈**：成功消息附带改动区域及前后 2 行；多处命中时列出所有行号；模糊匹配时按文件缩进重排 newString。
- **会话存储 `Store.Prune(keep, protect...)`**（暂无调用方）与统一的会话文件辅助函数；`cove -r` 接受 `<id>`、`<id>.jsonl`、`<id>.json`。
- **CI / 发布**：golangci-lint 固定 v2.1.6 并启用 `gosec`、`revive`、`errorlint`；Build Check 增加 `-tags chromedp` 的 vet 与请求拦截测试；发布矩阵新增 `windows/arm64`、`linux/arm64`；`build.bat` 版本号默认读取 `cli/cove/main.go`。

### Changed
- **权限模式分层**：`default` 自动放行只读工具和整行只读的 shell 命令；`auto` 另外放行构建/测试命令与项目内的 write/edit，git 写操作、安装、网络、未知命令、项目外写入、MCP 等仍需确认；`bypass` 全部放行。无规则命中时任何模式下都是“询问”（旧版 `auto` 在无规则命中时直接放行一切）。`-p` 运行无法回答询问，完全无人值守需 `bypass` 或持久化的允许规则。
- **命令前缀规则**：Git Bash/sh 与 PowerShell 下，引号内的参数可包含 `; & | ( ) < >`（cmd 下保持严格）；heredoc / here-string 内容视为 stdin 数据；`$(`、反引号、`${`、`<(`、`>(` 仍不放行。
- **会话存储格式**：`~/.cove/sessions/<id>.jsonl`（首行元数据 + 每行一条消息）+ `index.json` 索引；保存只追加新消息，列会话只读索引；旧 `<id>.json` 仍可加载，下次保存时自动迁移。
- **上下文计数与压缩阈值**：以服务商返回的 `input_tokens` 为锚计算上下文大小（含 system prompt 与工具定义）；压缩预算 = 窗口 × 0.85，触发点 = min(0.75 × 压缩预算, 窗口 − 回复预留 − 8K 安全余量)，回复预留为窗口的 1/4（4K–64K）；200K 模型约 127.5K 触发（原约 5.95 万 token），64K 模型 40K，1M 模型 637.5K。请求的 max_tokens 改为 min(64K, 模型输出上限, 窗口/4)、不低于 4K，不再固定为 64K。masker 的 token 计算同样改用 `token.Estimate`。每回合只刷新 git 信息，不再重扫文件树与 repo map。
- **工具调度（屏障语义）**：同一批调用按原顺序分段执行，并发安全的调用与写入不同文件的 write/edit 并行；遇到 bash 等非并发安全调用时，先等它之前的所有调用完成再单独执行，然后才继续后面的调用；同一文件的重复写入放在整批最后。
- **重试与计价**：退避加入 `[0.5, 1.5)` 抖动；支持 `retry-after-ms` 与 HTTP-date 形式的 `Retry-After`；`anthropic-ratelimit-*-reset` 只在 429 时读取（只看剩余为 0 的限额），5xx 不再读取、按正常退避；Anthropic 非流式超时保持 300s，请求超时不重试，仅连接阶段等传输错误（拒绝、重置）重试；prompt cache 写入按输入价 1.25 倍计费。
- **bash / powershell 工具**：默认超时 120s、上限 10 分钟；超时/取消返回已有输出并标注；工具描述写明实际解释器；关闭子进程彩色输出；Windows 下 Git Bash/cmd 输出尝试 GBK 解码；每流捕获上限 2MB。
- **read 工具**：超长行按字符截断并标注；按行截断时末行为固定格式 `[next: offset=N]`，引擎再截断时仍保留且位于最后一行。工具错误结果同样按输出上限截断。
- **`policies.json` 位置**：与 `config.json` 同目录（遵循 `COVE_CONFIG_DIR`），原先固定为 `~/.cove`。
- **工具块宽度**：旧 OnEngineOutput 前端的工具块按实时终端宽度减 1 列排版，避免 Windows 控制台在最后一列自动折行（原固定 120 列）。
- **测试耗时与隔离**：`go test ./...` 全量约 57–89s（受机器负载影响，此前实测 134s），未删除任何断言；engine 测试通过 TestMain 使用临时 HOME 与 `COVE_CONFIG_DIR`，不再读写开发者真实的 `policies.json` 与会话目录。

### Fixed
- 旧版 `auto` 模式因策略引擎“无规则命中即放行”而实际等同 `bypass`。
- `policies.json` 中的 `deny` / `ask` 规则此前只有 `allow` 被载入会话权限管理器：`deny` 在 `bypass` 模式与前缀放行下不生效、`ask` 不能让只读命令询问；现在三种规则都生效，同优先级下 `deny` 优先于 `allow`。
- `/cd` 切换项目后仍沿用旧项目 `scope` 的持久化规则；现在按新项目根重新加载 `policies.json`。
- 灾难命令拦截补齐：`bash -c`/`sh -c`/`cmd /c`/`powershell -Command`/`pwsh -c` 包装及嵌套、`powershell -EncodedCommand`、`xargs rm -rf`、`find / -delete`、`find ~ -exec rm`、喂给 shell 的 heredoc；引号内参数与喂给 `cat` 的 heredoc 不误拦。
- 上下文按字节 /4 估算导致中文低估约 3 倍且不计 system/工具的问题。
- 串行工具调用与仍在运行的并行调用重叠执行。
- hooks `matcher` 部分匹配（`bash` 误匹配 `bash_output`）。
- 浏览器 SSRF：`safeurl` 新增拒绝 `0.0.0.0/8`、`224.0.0.0/4`、`240.0.0.0/4`、`ff00::/8`、`64:ff9b::/96`；`198.18.0.0/15` 只拒绝直接写 IP（兼容 fake-IP DNS）；无头 Chrome 对页面所有请求做 CDP Fetch 拦截。
- 交互模式每次新请求/重试时重置 Markdown 渲染状态；保存会话时 `index.json` 更新失败不再让保存报错；Windows 下原子重命名遇共享冲突时重试。
- `build.bat` 版本号写死 5.0.0、构建时间非 UTC。

### Removed
- `lsp` 工具（未接入任何 LSP runner）与 `cron` 工具（记录后从不触发）；非 Windows 平台不再注册 `powershell` 工具。
- `internal/telemetry` 包与配置字段 `telemetry`（旧配置中的该键仍可加载，不再生效）。
- 折叠块的 `/x <id>` 展开提示与 `render.Expanded`；goldmark 依赖。
- 死代码：`api.ProviderWithStatus.Model`、`ModelFallback.CurrentModel()`、`engine.MessageNode`/`BuildMessageGraph`、`api/adapter` 的 `Message`/`MergeReasoning`/`HasParseError`、`tool.Context` 的 `AlwaysAllowRules`/`AlwaysDenyRules`。
- 文档：`docs/bubbletea-migration-plan.md`（描述的全屏界面已不存在）；手册中不存在的主题系统与旧 `policy.json` 规则类型说明。

## [11.0.0] - 2026-09-25

### Added
- **Shell 抽象层** (`internal/shell`)：统一命令解释器的选择（Windows 上 cmd/PowerShell，类 Unix 上 sh/bash），使 bash 工具、done-verify 门禁与系统提示词中的环境行保持一致。
- **终端控制序列净化** (`internal/render/sanitize`)：模型回复、推理内容、命令文本、文件/网页摘要等一切非 cove 自身产生的输出在渲染前统一剥离 ANSI 转义序列。
- **灾难性命令检测** (`internal/safety/command.go`)：识别会破坏项目之外数据的 shell 命令（清空文件系统/家目录/系统目录、写裸盘、关机、执行网络拉取的代码）并给出简短原因。
- **读后写保护** (`internal/tool/file_tracker.go`)：记录本会话中模型已读取的文件版本，write/edit 拒绝覆盖模型未曾读取过的文件。
- **旧版编码与行尾支持** (`internal/tool/gbk.go`、`internal/browser/charset.go`、`internal/tool/line_endings.go`)：GBK/GB18030 文件的解码与回写、UTF-8 BOM 处理、CRLF 行尾保留。
- **原子文件替换** (`internal/tool/replace_file*.go`)：write/edit 通过临时文件 + rename 落盘，避免写入中途损坏目标文件。
- **Shell 工具权限作用域** (`internal/permission/command_prefix.go`)：“总是允许”对 shell 类工具按命令前缀授权，而非整工具放行。
- **SSE 解析健壮性** (`internal/api/sse.go`、`internal/mcp/sseparse.go`)：容忍 `data:` 后缺省空格、注释行等非标准但常见的 SSE 写法。
- **API 错误分类** (`internal/api/errors.go`) 与**调用计量 Provider** (`internal/api/metered.go`)：统一的错误判定/重试语义与用量回调。
- **会话项目规范化** (`internal/session/project.go`)：统一项目目录的规范形式，避免同一项目因路径写法不同被拆成多个。
- **Windows 进程树终止** (`internal/mcp/proctree_*.go`)：确保 MCP 子进程连同其子进程一并清理。
- **自定义/插件命令参数展开** (`internal/command/arguments.go`)：`$ARGUMENTS` 占位符填充与追加逻辑。
- **CLI 参数解析独立成文件** (`cli/cove/cli_args.go`) 与 **stdin PTY 检测** (`cli/cove/stdin_pty_*.go`)：支持 `--print`/`--resume` 等参数与管道输入的可靠判定。

### Changed
- **技能系统** (`internal/skills`)：每个技能记录来源，加载优先级为 项目 > 用户 > 插件 > 内置；内置技能收敛为 12 个按需加载技能。
- **大规模重构**：engine、MCP 客户端与连接池、delegate、diagnostic、config、cost 等模块的职责拆分与错误处理整理。
- **工具加固**：grep、read、glob、mcp_tool、webfetch、bash/powershell、browser 等工具的输出处理、编码与路径安全增强。
- **文档**：README 与 `docs/USER_MANUAL.md` 更新（技能数量、命令与配置说明）。

### Fixed
- 引入依赖 `golang.org/x/text` 以支持 GBK/GB18030 解码。

### Tests
- 新增 113 个测试文件，覆盖 api、engine、tool、mcp、permission、config、session、delegate 等模块的边界与回归场景。

## [Unreleased]

### Added
- **配置档案 (Profiles)**：新增 /profile 命令（list/switch/save/delete/show）和 --profile 启动参数，支持切换命名配置切片（model/provider/budget 等）。
- **会话录制与回放 (Record/Replay)**：新增 /record 命令（status/start/stop）、--record 和 --replay 启动参数。录制输出至 events.jsonl，回放时不调用真实 API。
- **Provider Adapter 基础层**：internal/api/adapter/ 包提供 Message、StreamAccumulator、MergeReasoning、ToolCallsFromResponse 等归一化工具（为后续 Provider 重构铺路）。
- **L1b 目录多样性检测**：循环检测 Layer 1b 新增目录指纹分析，同一工具模式在 ≥3 个不同目录中视为探索性工作，不再误判为循环（解决顺序 git 操作误触发问题）。

### Changed
- **压缩语义标签**：压缩注入的上下文提示添加 <compress summary="..."> 标签，给模型清晰信号。
- **Engine 重构**：抽取 stream_handler.go、	ool_runner.go、message_processor.go 分担 engine.go 职责；新增 uildMessageGraph() 为后续拓扑感知压缩做准备。
- **循环检测改进**：resetFingerprintHistory() 新增目录状态清理；hasToolCalls() 零值安全性提升。

### Fixed
- 循环检测 L1b 在频繁使用 shell 命令进行 git 操作时误触发自动中止。
- Config.Load() 在 profile 为空时的 nil map 赋值防护。

## [8.0.0] - 2026-07-18

### Added

#### TUI 主题系统 (Theme System)
- 全新主题系统 `internal/tui/theme/`，支持 5 套内置主题：
  - **Catppuccin** (Mocha) — 暖色调舒适主题
  - **Dracula** — 经典暗色高对比主题
  - **Gruvbox** — 复古暖色主题
  - **OneDark** — Atom 编辑器经典主题
  - **TokyoNight** — 夜间蓝紫色调主题
- 主题接口：`theme.go` 定义 `Theme` 接口，包含 20+ 语义化颜色令牌（text、accent、success、warning、error、border 等）
- 按需自由切换：TUI 内通过快捷键或命令切换主题

#### MCP 客户端重构 (Client Refactor)
- `internal/mcp/client.go` 全面重构：连接生命周期管理、超时控制、停止信号通道
- 改进的 goroutine 安全管理：`stopCh` 机制确保 `Receive()` 调用可被取消
- 更健壮的错误处理和重连逻辑

#### 循环检测增强 (LoopDetector)
- Layer 1b 模糊匹配增强：区分工具参数变化，减少误报
- Layer 2 输出循环检测修复：从仅日志升级为主动注入引导+硬中止
- Layer 3 停滞检测激活：`RecordIteration()` 和 `RecordFileActivity()` 现在被正确调用
- 新增 Layer 1b 工具输出进度检测：相同模式下产出 ≥4 种不同输出时重置循环计数

#### 引擎改进 (Engine)
- `OnToolStart` 回调：工具执行前通知，配合 `OnToolProgress` 提供完整生命周期回调
- 流式输出 ANSI 清理：TUI 显示工具输出时自动去除 ANSI 转义码
- CompressHistory 中文乱码修复

### Changed
- **TUI 视图层重构**：tui.go 重写 640+ 行，视图布局全面优化
- **styles.go**：重构为基于主题令牌的样式系统（253 行 → 256 行，+256/-57）
- 引擎中的循环检测配置同步更新（fpWindow = 10）

### Fixed
- 修复 compressHistory() 中文乱码（锟斤拷问题）
- 修复 MCP client 上下文取消未正确处理的问题
- 修复 TUI tool start 时缺少回调通知

### Chore
- `internal/tui/tui_smoke_test.go` 测试覆盖扩展（+267 行）

## [7.1.1] - 2026-07-04

### Chore

- **文档整理**: 将根目录大型开发指南移入 docs/guide/，清理临时文件 (session_notes.md, testout.txt)
- **文档归档**: 将未使用的优化文档移入 docs/archive/（.gitignore 忽略）
- **测试脚本迁移**: test_e2e_steer.py 移入 scripts/
- **.gitignore 优化**: 移除 blanket *.md 规则，改用精确忽略

## [7.0.6] - 2026-07-17

### Added
- **TUI F6 复制模式切换**：按下 F6 可在 TUI 中选择/复制文本（原生拖拽选择）
- Shift+Wheel 支持：按住 Shift 滚动鼠标滚轮时正常滚动视口

## [7.0.5] - 2026-07-04

### Fixed
- **中文乱码修复**：修复多文件中的锟斤拷（mojibake）显示问题
- 清理旧的回退兼容代码（repl_history.go）

### Changed
- Activity 提示从中文改为英文
- Review 输出改为纯英文

## [7.0.4] - 2026-07-03

### Changed
- **文档重组**：将 COVE_COMPLETE_GUIDE.md、DEVELOPMENT_DESIGN.md、DEVELOPMENT_GUIDE.md 移入 docs/guide/
- 清理根目录临时文件（session_notes.md, testout.txt）

### Fixed
- 修复 test_e2e_steer.py 路径问题，移至 scripts/

## [7.0.3] - 2026-07-01

### Changed
- **精简系统提示词**：精简 system_prompt 长度，将参考配置写入 config.example.json
- 减少不必要的上下文占用

## [7.0.2] - 2026-06-30

### Added
- **极致系统提示词优化**：任务完成标准、防编造规则、工具调用规范、双语支持
- 更严格的完成任务验证要求

## [7.0.1] - 2026-06-28

### Chore
- **文档归档**：将未使用的优化文档移入 docs/archive/
- 添加 .gitignore 规则忽略 archive 目录

## [7.0.0] - 2026-06-25

### Added
- **P0-P2 共 14 项完整优化实现**，基于设计文档 (DEVELOPMENT_DESIGN.md)
- 循环检测（3-Layer LoopDetector）
- 模型故障转移（ModelFallback）
- 对话压缩（ChatCompressor）
- 工具输出掩码（ToolOutputMasker）
- NextSpeaker、PolicyEngine、MCP Streamable HTTP
- SessionDiff、Telemetry、Safety、Enhanced RepoMap

### Changed
- Engine 重构：集成 ModelFallback、ModelRouter、LoopDetector 统一编排
- Config 扩展：新增 ModelFast、循环检测相关配置

### Documentation
- 详细设计文档 DEVELOPMENT_DESIGN.md（1208 行）
- 开发指南 DEVELOPMENT_GUIDE.md（1584 行）
- 完整开发手册 COVE_COMPLETE_GUIDE.md（1780 行）
- 配置模板 config.example.json
## [6.3.1] - 2026-06-20

### Added

#### 模型路由 (ModelRouter)
- 双模型自动切换：根据任务复杂度在主模型与 model_fast 之间切换。
- 策略链：override -> complexity classifier -> default，命中即生效。
- 支持运行期切换：/model 命令与 SetModels() 可动态更新。

#### 故障转移 (ModelFallback)
- Provider 链式故障转移：主 Provider 失败时自动切换备用 Provider。
- 三态健康状态：ProviderOK、ProviderDegraded、ProviderUnavailable。
- 冷却恢复机制：429/5xx/超时触发冷却，冷却后自动恢复探测。
- 永久错误检测：401/403 等认证错误标记为不可用，需人工修复。
- TryChat/TryChatStream：统一的非流式/流式故障转移调用入口。
- UI 状态指示：在 /status 中展示健康、降级、不可用状态。

#### 三层循环检测 (3-Layer LoopDetector)
- Layer 1a（精确指纹）：窗口内重复相同 tool-call 指纹触发检测。
- Layer 1b（模糊模式）：窗口内重复相同工具名模式触发检测。
- Layer 2（输出哈希）：窗口内重复输出哈希触发检测。
- Layer 3（停滞检测）：长时间无文件创建/修改时触发停滞提示。
- 只读工具豁免：read、grep、glob、lsp、webfetch、browser、task_list 等不触发循环报警。
- 分级响应：先注入引导，超过阈值后硬中止。

#### 对话压缩 (ChatCompressor)
- 双层压缩：先做轻量截断，再做结构化 AI 摘要。
- 智能触发：接近上下文预算上限时自动执行压缩。
- 安全切分：避免压缩后产生非法消息序列。
- 失败降级：摘要失败时自动回退到保守策略。

#### 工具输出掩码 (ToolOutputMasker)
- 反向扫描 FIFO，优先保留最近上下文。
- 支持磁盘卸载历史工具输出到 ~/.cove/tool-outputs/。
- 交互类工具默认豁免，不参与掩码。
- 避免重复掩码，降低无效噪声。

#### 其他新增
- NextSpeaker：上下文感知的继续/停止决策。
- PolicyEngine：声明式权限策略（allow/deny/ask）。
- MCP Streamable HTTP：支持新的流式传输协议。
- SessionDiff：会话变更追踪与摘要。
- Telemetry：本地遥测记录与容量保护。
- Safety：敏感命令、路径遍历、密钥泄漏检测。
- Enhanced RepoMap：基于引用与增量缓存的仓库映射增强。

### Changed
- Engine 重构：引入 ModelFallback、ModelRouter、LoopDetector 统一编排。
- Config 扩展：新增 ModelFast、循环检测相关配置项。
- Permission 升级：集成 PolicyEngine 作为权限决策入口。
- Hooks 增强：PreToolUse/PostToolUse 支持异步处理。
- MCP 客户端：支持 Streamable HTTP（兼容既有 SSE）。
- Session Store：集成 SessionDiff 追踪。
- State 扩展：新增 ModelFast 字段。

### Fixed
- 修复压缩后相邻 user 消息导致 API 400 的问题。
- 修复只读工具被误判为循环的误报问题。
- 修复 Provider 锁竞争导致的阻塞读问题。
### Documentation
- README.md：补充双模型路由章节与配置示例。
- docs/USER_MANUAL.md：补充模型切换策略、配置字段与 MCP 传输说明。
- CHANGELOG.md：记录本次更新。

## [5.1.2] - 2026-06-11

### Added
- **Plan Executor (execute_plan)**: Declarative multi-step task plans with dependency DAG, topological sort, and parallel sub-agent execution (up to 4 concurrent)
- **Multi-Agent Teams (team_create/team_delete)**: Create agent teams with member tasks and inter-agent message passing (send_message)
- **Cron Scheduler (cron)**: Schedule recurring background tasks via cron expressions
- **Background Task Queue**: Async REPL task execution with queue, merge detection, retry support, and interrupt drafts
- **Checkpoint System**: Auto Git snapshots before write/edit operations with `/undo` and `/checkpoints` commands
- **Headless Browser (browser)**: Chrome-based JS rendering and screenshot capture (chromedp build tag)
- **Web Search (websearch)**: DuckDuckGo-based live web search tool
- **Attachments (/attach)**: File and image attachment in REPL with `@path` inline syntax
- **Session History (/history)**: Browse and resume past conversation sessions with detail view
- **Rate Limit Tracking (/ratelimit)**: API rate limit status monitoring
- **Budget Auto-Mode (/budget auto)**: Smart budget suggestion based on historical usage
- **Git Worktree (worktree/exit_worktree)**: Isolated git worktree creation and cleanup
- **User Manual**: Comprehensive Chinese user manual covering all features, commands, and tools

### Changed
- **README**: Added Agent Tools table, Plan Executor, Teams, Guardrails, Checkpoints, and Diagnostic System
- **docs/**: Reorganized with index, fixed corrupted docs/README.md
- **REPL UI**: Async task execution prevents input blocking; streaming-safe cursor handling

### Fixed
- **Permission prompt hang**: Replaced fmt.Scanln with bufio.Scanner - empty input defaults to deny
- **Tool goroutine panic crash**: Added defer recover() in parallel tool execution goroutines
- **Engine loop after Ctrl+C**: Added ctx.Err() check at iteration start
- **WalkingIndicator race condition**: Synchronized Stop() with doneCh channel

## [4.0.5] - 2026-06-07

### Added
- **CovePhone (Android App)**: First mobile companion app for cove
  - Native Go AI engine (mobile/cove.go) compiled via gomobile into cove-core.aar
  - Full chat UI with thinking display, batch-rendered thinking blocks
  - Settings screen for API key, model, and provider configuration
  - Persistent configuration via SharedPreferences (ViewModel-backed)
  - DeepSeek API integration (real AI, not simulated responses)
- **Mobile Go Engine**: Lightweight standalone engine in mobile/cove.go for Android use
- **Release artifact**: covephone-v4.0.5.apk available in dist/v4.0.5/

### Changed
- **Documentation**: README updated with CovePhone sections (English & Chinese)

## [3.0.3] - 2026-06-06

### Fixed
- GitHub Actions release pipeline paths.

## [1.0.0] - 2026-06-03

### Added
- **Self-Learning Pipeline**: Automatic memory extraction, background skill/memory review, and periodic cross-session memory consolidation (dream)
- **Skill Tools for Agent**: `skills_list` and `skill_view` tools - agents can discover and load skills autonomously
- **Conditional Skill Loading**: Skills with `paths` frontmatter auto-inject when agent opens matching files
- **23 Built-in Skills as Disk Files**: User-editable SKILL.md files in `~/.cove/skills/`
- **Guardrail Time-Window Circuit Breaker**: 6+ consecutive failures in 30s triggers immediate block
- **Session Notes Decision/Discovery Tracking**: Regex-based auto-detection of user decisions
- **Memory Deduplication**: >80% similarity 鈫?merge instead of duplicate
- **Hooks Event System**: `PreToolUse`, `PostToolUse`, `SessionStart` events
- **Checkpoint Auto-Trigger**: Git snapshots before write/edit operations
- **`/dream` Command**: Manual memory consolidation trigger

### Changed
- **Skill System**: Hardcoded skill bundles 鈫?disk-based SKILL.md files
- **`backgroundReview` Throttled**: Minimum 4 new messages between reviews

### Removed
- **Buddy System**: Virtual pet companion removed
- **Suggest System**: Follow-up suggestion generation removed
- **Hardcoded Skill Bundles**: Replaced with disk files

## [1.0.2] - 2025-05-24

### Added
- Cross-platform release artifacts for Windows, Linux, macOS (amd64/arm64)
- Automated release build script with checksum generation

### Fixed
- Various stream processing fixes

## [1.0.1] - 2025-05-XX

### Added
- Initial public release
- Multi-provider AI backend (Anthropic, OpenAI, DeepSeek + 9 compat)
- Interactive REPL with slash commands
- Git integration (commit, review, diff)
- Permission system (default, plan, auto, bypass)
- Config management with env vars, user config, project-level overrides
- Token tracking and cost estimation
- Session save/resume, Memory persistence
- MCP (Model Context Protocol) server support
- Plugin system, Skills system


