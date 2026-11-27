# 0063 MCP 未信任时不再中断整轮；重启恢复后说清"继续"接的是哪一轮

- 状态：已完成
- 日期：2026-11-27（创建）；2026-11-27（完成）
- 关联：0024（没有终端可询问时拒绝调用、任务继续，本文沿用同一思路）；0026（换话题时手动交接上下文）

## 背景

会话里有两个不相关的请求，先后提出：第一个已经回答完，第二个需要调用 MCP。项目还没被信任，第二个请求在第一次调用 MCP 时整轮报错结束，没有任何回答。用户执行 `/config trust`、`/restart` 后输入"继续"。cove 按规则接着做了第二个请求，这一点符合代码设计。但从界面上看不出"继续"会接哪一轮，而重启时打印的扩展警告里，恰好有第一个请求要找的答案（某个插件目录加载失败）。模型看不到这条警告，第一个请求的错误结论也就一直没有更正。

问题不在某一个函数，而在这条流程：一个只能靠改配置解决的问题被当成"等待批准"中断了整轮，留下一个未完成的轮次。重启后，这个轮次会自动占住"继续"，界面却没有标出来；启动时的警告也只给人看，不给模型看。

## 现状与问题

1. **MCP 未信任被当成"等待批准"，整轮中断。**
   - `src/extensions/mcp.rs:459-462`：项目未信任时，`checked` 返回 `Error::Approval("MCP 服务器需要 --trust-extensions…，并且需要授权")`。工具不在允许列表里时也走同一条路（`mcp.rs:535-538`）。
   - `src/tools/extension_tools.rs:156-157`：`Approval` 原样往上抛，不转成工具失败。
   - `src/engine/agent.rs:1881-1883`：遇到 `Approval` 就把调用标回 `CallState::Planned`，整轮 `return Err`。
   - 结果：模型拿不到任何结果，用户只看到一行"错误：需要批准或已被拒绝：…"。会话里留下一个 `pending` 非空的轮次。
   - 这两种情况都不是用户当场批准就能解决的：前者要改信任设置并重启，后者要改配置。按 0024 的思路，应该作为失败的工具结果交给模型，任务继续。
   - 文案也有误导：bypass 模式下仍然写"并且需要授权"；统一前缀"需要批准或已被拒绝"（`src/domain.rs:21-22`）区分不了是哪种情况。
2. **"继续"会接上那个被中断的轮次，界面却不标出来。**
   - 会话里只要 `pending` 非空，或状态是 `Running` / `Interrupted`，就算未完成（`src/session/mod.rs:684-686`）。这时输入"继续"等词（`src/commands/sessions.rs:830`），会直接 `execute(None)` 接着做（`sessions.rs:404-407`）。同一会话里只可能有一个未完成的轮次，就是最后一轮。
   - 重启后的对话回显（`sessions.rs:995-1024`）只列出请求和回答。被中断的那一轮只有请求、没有回答，但界面没有写"这一轮没做完，输入'继续'会接着做它"。如果前面还有答完但没解决的请求，用户分不清"继续"会接哪一个。
3. **`/restart` 退出时提示"下次在这里输入'继续'"。**
   - `/restart` 先保存会话，再带着 `--resume <id>` 重启自己（`src/commands/project.rs:906-916`）。
   - 退出 REPL 时，只要会话里有对话，就打印"会话已保存。下次在这里输入'继续'，或用 cove --resume … 启动"（`src/app/mod.rs:526-533`）。重启是自动恢复的，这句话在重启场景下多余，还会让人以为"继续"是在恢复会话本身。
4. **启动时的扩展警告模型看不到。**
   - 插件扫描时，缺清单的目录记一条警告：`插件 <目录>：tool: no manifest.json or .claude-plugin/plugin.json`（`src/extensions/plugins.rs:79-81`、`141`）。这条警告只在启动时经 `warn` 打到终端（`src/commands/extensions.rs:59-61`、`101`），不进会话，也不进系统提示。
   - 文案中英混杂，还带着 `Error::Tool` 的 `tool: ` 前缀（`src/domain.rs:32-33`），不符合 AGENTS.md"给人看的文字用中文"。
   - 本机实例：`~/.cove/plugins/docref/` 下只有 `bin/`（`docref.exe`、`docref.dll`、PdfPig 等 DLL）。它不是 cove 插件，是本地技能 `~/.claude/skills/dotnet-kb/SKILL.md` 第 11、51 行用全路径调用的命令行工具。cove 把 `plugins/` 下的每个子目录都当成插件扫描，所以每次启动都会报这条警告。

## 目标

- MCP 未信任或工具未被允许时，这次调用作为失败结果交给模型，结果里写明原因和放行办法（`/config trust` 后 `/restart`，或改允许列表）。模型可以继续做别的事，或在回答里告诉用户怎么做，整轮正常结束，不留下未完成的轮次。
- 恢复会话时，如果最后一轮没做完，回显里明确写出来，并说明输入"继续"会接着做它。
- `/restart` 不再打印"下次在这里输入'继续'"。
- 插件警告改成中文，去掉 `tool: ` 前缀。没有清单的目录说明"不是 cove 插件，已跳过"，并告诉用户怎么消掉这条提示。

## 范围

- 做：上面 4 个问题。
- 不做：
  - 启动警告写进模型上下文。理由：每轮都带上会占缓存前缀，而且大多数警告和任务无关。先让警告文案自己说清楚（见"决策记录"）。
  - 同一会话里换话题的自动识别和提示。0026 已经提供了手动的 `/handoff`；自动判断话题是否相关容易误判，另开文档。
  - 本次一起发现的其他缺陷另开文档，见"遗留"。

## 调研与参考

- 0024 把"没有终端可询问"从中断整轮改成"拒绝这一次调用、告诉模型、任务继续"（`Error::Unattended`，`src/engine/agent.rs:1877-1880`）。MCP 未信任的性质相同：当场没人能批准，本文沿用这个做法，不新增机制。
- 同一个函数里，`Error::Denied`（`agent.rs:1876`）也是交给模型后继续，说明"整轮中断"只应该留给真正等待用户回答的情况。
- docref 来源核对：GitHub 仓库 `liuzhixin405/doc-ref` 里没有 `manifest.json`、`.claude-plugin/plugin.json`。发布页 `v0.1.0` 有 `docref-win-x64.zip`（sha256 `ad9eee1b…6f3fbd`），里面是 `docref.exe`、`docref.dll` 和 PdfPig 等 12 个文件，没有清单。它的安装脚本 `dotnet-kb/sync-skills.py --cove` 只往 `~/.cove/plugins/docref/skills/dotnet-kb-cove/` 写一个 `SKILL.md`，注释里引用的是 Go 版 cove 的 `internal/skills/skills.go`。当前 Rust 版要求插件目录里有清单（`plugins.rs:69-81`），所以无论是下载发布包，还是按这个脚本安装，得到的目录照样会报缺清单。重新下载能让别的机器用上 docref 工具，但解决不了问题 4。

## 方案

1. **MCP 未信任和工具未被允许改为工具失败。**
   - `mcp.rs:460`、`mcp.rs:536` 改为返回 `Error::Tool`。文案分别写成："项目未信任，MCP 服务器不会启动：先执行 /config trust，再 /restart。这次调用没有执行。" 和 "MCP 工具 <名> 不在允许列表中，这次调用没有执行。"
   - `extension_tools.rs:159-164` 已经会把 `Error::Tool` 转成 `failed: true` 的工具结果，不需要改。
   - 其他调用方（`connect_now`、`/config mcp` 命令）拿到的也是 `Error::Tool`，命令行照样报错，只是错误类别变了。实施时用 `grep` 核对调用方，确认没有依赖 `Approval` 类型。
2. **回显标出未完成的轮次。** `show_conversation` 接收会话的 `unfinished()` 状态。最后一轮没做完时，在它后面加一行："（这一轮没做完；输入"继续"接着做，或直接输入新请求）"。新请求的提示要和现状一致：`agent.rs:1557-1564` 在有未完成轮次时会拒绝新请求，提示用户 `/continue` 或 `/new`。实施时以这段代码为准，写对文案。
3. **`/restart` 不打印"下次在这里输入'继续'"。** `restart` 设置一个标记，或 `repl_loop` 根据 `request_restart` 是否已触发，跳过 `app/mod.rs:527-533` 这句。
4. **插件警告文案。**
   - `read_manifest` 的缺清单错误改为中文："不是 cove 插件（没有 manifest.json 或 .claude-plugin/plugin.json），已跳过；不想看到这条提示，就把它移出 plugins 目录，或把目录名改成以 .disabled 结尾"。`.disabled` 后缀的跳过规则在 `plugins.rs:105-109`。
   - `plugins.rs:141` 拼接时不用 `Error` 的 `Display`，避免带上 `tool: ` 前缀。

## 影响范围

- `src/extensions/mcp.rs`、`src/extensions/plugins.rs`、`src/commands/sessions.rs`、`src/app/mod.rs`、`src/commands/project.rs`（或 `src/app/launch.rs`）。
- 行为变化：项目未信任时，模型调用 MCP 不再中断整轮，而是收到失败结果。原来那条"保留 Planned 调用、信任后用'继续'自动重试"的路径没有了：信任并重启后，模型要重新发起调用。由于不信任就无法当场批准，这条旧路径只在"信任、重启、再继续"时有用，现在改由模型在回答里提示用户。
- 数据格式不变，旧会话里已经停在 `Planned` 的调用照常用"继续"恢复。
- 文档：`docs/reference.md` 的 MCP 信任和会话恢复相关段落、README（如有提及）同步修改。

## 验证

- 新增测试：
  - 项目未信任时，`mcp` 工具调用返回 `failed: true` 的结果，内容包含 `/config trust`，不返回 `Error::Approval`。
  - 工具不在允许列表时同上。
  - 回显：会话 `unfinished()` 为真时，输出包含"这一轮没做完"；已完成的会话不包含。
  - 插件扫描：缺清单的目录，警告是中文，不含 `tool: `，含 `.disabled`。现有断言见 `plugins.rs:493`。
- 搜 `src/`、`tests/` 里对原文案"MCP 服务器需要 --trust-extensions"和"no manifest.json"的断言，一并更新。
- 按 AGENTS.md：只格式化改过的文件，跑 `cargo clippy --release --all-targets`（不新增警告），最后跑 `cargo test`。
- 人工：release 版在未信任的项目里让模型调用一次 MCP，确认这一轮有回答；执行 `/restart`，确认没有"下次在这里输入'继续'"；故意中断一轮再重启，确认回显标出未完成。

## 实施计划

- [x] 阶段 1：方案 1（MCP 改为工具失败）及测试
- [x] 阶段 2：方案 2、3（回显标注、`/restart` 提示）及测试
- [x] 阶段 3：方案 4（插件警告文案）及测试
- [x] 阶段 4：同步 `docs/reference.md`，回填验证结果（README 没有相关段落，未改）

## 待决策

无。

## 决策记录

- 2026-11-27：**启动警告不进模型上下文，只改文案。** 理由：每轮都带上会占缓存前缀，而且大多数警告和任务无关。以后如果再出现"警告里有答案、模型看不到"的情况，再考虑在 `/doctor` 或技能列表的工具结果里附上加载警告，不放进每轮的系统提示。
- 2026-11-27：**`plugins/` 下没有清单的目录继续提示，不静默跳过。** 文案说明"不是 cove 插件，已跳过"，并给出消除办法（移出 plugins 目录，或把目录名改成以 `.disabled` 结尾）。理由：静默跳过会让清单写错的真插件也查不出原因。

## 验证结果

实际改动：

- `src/extensions/mcp.rs`：未信任、工具不在白名单两处由 `Error::Approval` 改为 `Error::Tool`，文案保留 `--trust-extensions`（`/config mcp connect` 的现有测试断言它）。
- `src/commands/sessions.rs`：`show_conversation` 在 `session.unfinished()` 时加一行"这一轮没做完：输入'继续'接着做；要做别的事，先用 /new 开新会话"。
- `src/app/launch.rs` 新增 `restart_requested()`；`src/app/mod.rs` 在已请求重启时不打印"下次在这里输入'继续'"。
- `src/extensions/plugins.rs`：扫描时先判断有没有清单，没有就提示"<目录> 不是 cove 插件……已跳过"和消除办法；解析失败的文案改中文，拼接时去掉 `tool: ` 前缀。和方案略有出入：缺清单的提示放在 `scan` 里，`read_manifest` 只把缺清单的错误改成中文，因为它也被 `install` 用，`.disabled` 的建议对安装不适用。
- `docs/reference.md`：MCP 一节、插件一节、"恢复后能看到之前的对话"一条。

测试与检查：

- 新增测试：`extensions::untrusted_mcp_call_fails_the_call_and_the_turn_finishes`（模型被调用两次，第二次请求里带 `/config trust`，stderr 里没有"需要批准"）、`sessions::resuming_an_unfinished_session_marks_the_unfinished_turn`、`project::restart_does_not_tell_the_user_to_continue_next_time`（重启前后两次运行只出现一次该提示）、`extensions::plugins::tests::a_directory_without_a_manifest_is_skipped_with_a_hint`；`continue_in_a_new_session_goes_back_to_the_last_task_and_shows_it` 增加断言：已完成的会话不显示"这一轮没做完"。白名单那条路径没有单独新增测试，现有的 `integrations::mcp_readonly_and_allowlist_rejections_do_not_start_a_process` 通过。
- 未信任时 `-p` 仍以非零退出：这一轮有一个工具失败，按现有规则提示"最终结果仍需核对"。这是预期行为，测试按"模型拿到失败结果并作答"断言。
- rustfmt（stdin 模式，只对改过的 8 个文件）：只有 `tests/all/project.rs` 一处换行，已手动改。
- `cargo clippy --release --all-targets`：5 条警告，都是原有的（`console.rs` ×2、`process.rs`、`policy.rs`、`tests/all/cli.rs`），没有新增。
- `cargo test`：lib 622 个全部通过；集成 272 个中 271 个通过。`cli::all_hook_events_run_with_scrubbed_environment_and_session_end_once` 失败（task_end hook 在全量并发负载下被取消），单独重跑通过，和本次改动无关。
- 人工验证（release 版实际操作）：未做。

## 遗留

- 人工验证未做：release 版在未信任项目里调用 MCP、`/restart`、中断后重启这三项，需要在交互终端里实际操作一遍。
- `cli::all_hook_events_run_with_scrubbed_environment_and_session_end_once` 在全量运行时偶发失败（hook 超时被取消），单独运行通过。
- 未信任时 `-p` 收尾报"错误：tool: 任务过程中有 1 个工具调用失败……"，同样带 `tool: ` 前缀，归入下面"其他 `Error::Tool` 文案"一条。

本次一起发现、不在本文范围的问题，各自另开文档：

- 模型路由：`src/model/routing.rs:47-66` 的关键词表没有"排查""为什么""查一下"这类调查用词，一个需要几十次工具调用的调查请求被判成简单任务。
- MCP 工具行只显示 `mcp call`，看不出服务器、工具和参数；失败时预览的是结果 JSON 的末尾（`"isError": true`），看不到错误原因。
- MCP 结果里 `content` 文本和 `structuredContent` 是同一份数据，两份都进了上下文，token 翻倍。这是根据输出观察到的，代码路径未核实。
- `agent` 工具说明没写子代理不能用 MCP（子代理的工具定义里过滤掉了它，`src/agents/subagent.rs:610-613`、`635-640`）。后台子代理即使是只读类型，也要求 git 仓库（`src/agents/background.rs:460-465`）。
- 其他 `Error::Tool` 的英文文案和 `tool: ` 前缀（如 `background.rs:463`、工作区外路径的报错）。
