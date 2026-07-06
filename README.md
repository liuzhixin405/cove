<div align="center">

# 馃 cove

**Go-powered AI Coding Assistant for the Terminal**

[![CI](https://github.com/liuzhixin405/cove/actions/workflows/ci.yml/badge.svg)](https://github.com/liuzhixin405/cove/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/cove/cove?include_prereleases)](https://github.com/liuzhixin405/cove/releases)
[![Go Version](https://img.shields.io/github/go-mod/go-version/cove/cove)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](CONTRIBUTING.md)

[English](#english) | [涓枃](#chinese)

</div>

---

<a name="english"></a>

## English

cove is a pure CLI AI programming assistant, implemented as a single-file Go binary. It runs in your terminal, supports multiple AI providers, and is designed for local development, scripting, and portable distribution.

### 鉁?Features

- 馃幆 **Single Binary** 鈥?Zero dependencies, just download and run
- 馃寪 **Multi-Provider** 鈥?Anthropic, OpenAI, DeepSeek + 10+ OpenAI-compatible endpoints
- 馃枼锔?**Cross-Platform** 鈥?Windows, macOS (Intel & Apple Silicon), Linux
- 馃帹 **Interactive REPL** 鈥?25+ slash commands, async task queue, session management
- 馃敡 **Agent Tools** 鈥?File ops, shell, grep, glob, web fetch/search, headless browser, PowerShell
- 馃 **Self-Learning** 鈥?Auto memory extraction, skill creation, cross-session consolidation (Dream)
- 馃搵 **Plan Executor** 鈥?Declarative multi-step task plans with dependency DAG + parallel sub-agent execution
- 馃懃 **Multi-Agent & Teams** 鈥?Spawn sub-agents, create teams with message passing, cron scheduling
- 馃摎 **Skill System** 鈥?23 built-in skills + custom skills, conditional auto-loading by file type
- 馃幁 **Permission Modes** 鈥?default | plan | auto | bypass with intelligent classifier
- 馃洝锔?**Guardrails** 鈥?Tool loop detection, rapid-failure circuit breaker, idempotent result detection
- 馃挵 **Cost Tracking** 鈥?Real-time token counting, cost estimation, budget caps, rate-limit awareness
- 馃攧 **Checkpoints** 鈥?Auto Git snapshots before write/edit, undo support
- 馃┖ **Diagnostic System** 鈥?30+ error codes, startup checks, hot-fixable without restart
- 馃尪 **TUI Theme System** 鈥?5 built-in themes (Catppuccin, Dracula, Gruvbox, OneDark, TokyoNight), hot-switchable
- 馃摫 **CovePhone — Android mobile app with native Go AI engine

### 馃摜 Installation

#### Download Pre-built Binary

Go to [Releases](https://github.com/liuzhixin405/cove/releases) and download the archive for your platform:


| Platform              | File                          |
| --------------------- | ----------------------------- |
| Windows (amd64)       | `cove-v*-windows-amd64.zip`   |
| macOS (Intel)         | `cove-v*-darwin-amd64.tar.gz` |
| macOS (Apple Silicon) | `cove-v*-darwin-arm64.tar.gz` |
| Linux (amd64)         | `cove-v*-linux-amd64.tar.gz`  |

Extract and run:

```bash
# macOS / Linux
tar -xzf cove-v*-linux-amd64.tar.gz
./cove

# Windows (PowerShell)
Expand-Archive cove-v*-windows-amd64.zip -DestinationPath .
.\cove.exe
```

Optionally, add to your `PATH` for global access.

#### Build from Source

```bash
git clone https://github.com/liuzhixin405/cove.git
cd cove/agent
go build -o cove ./cmd/cove
./cove --version
```

Requires Go 1.24+.

#### Local Release Build

```bash
python scripts/release_build.py v2.0.0
```

Artifacts are output to `dist/v2.0.0/`.

### 馃摫 CovePhone (Android)

CovePhone is an **Android companion app** for cove, bringing AI assistant capabilities to your mobile device.

- 馃 **Native Go Engine** 鈥?Real AI engine (not mock) powered by `cove-core.aar`, a Go module compiled via `gomobile`
- 馃挰 **Full Chat UI** 鈥?Message list with thinking display, smooth scrolling, batch-rendered thinking blocks
- 鈿欙笍 **Settings & Config** 鈥?API key, model selection, provider choice, persistent via SharedPreferences
- 馃攲 **DeepSeek API** 鈥?Connects to DeepSeek (or other compatible providers) directly from your phone

**Download:** [covephone-v4.0.5.apk](dist/v4.0.5/covephone-v4.0.5.apk) (Android, ~47MB)

**Source:** [`mobile/`](mobile/) 鈥?Lightweight Go engine for mobile.

### 馃殌 Quick Start

```bash
# Interactive REPL
cove

# One-shot query
cove -p "Create a snake game in HTML"

# View version
cove --version

# System diagnostics
cove --doctor
```

On first run, cove will guide you through API key setup. You can also set it directly:

```bash
# In REPL
/api-key sk-your-key-here

# Or via environment variable
export DEEPSEEK_API_KEY="sk-..."
export ANTHROPIC_API_KEY="sk-ant-..."
export OPENAI_API_KEY="sk-..."
```

### 馃實 Supported Providers


| Provider            | Type       | Environment Variable                 |
| ------------------- | ---------- | ------------------------------------ |
| **Anthropic**       | Native     | `ANTHROPIC_API_KEY`                  |
| **OpenAI**          | Native     | `OPENAI_API_KEY`                     |
| **DeepSeek**        | Native     | `DEEPSEEK_API_KEY`                   |
| **GLM (鏅鸿氨)**      | Compatible | `GLM_API_KEY` / `ZHIPU_API_KEY`      |
| **Kimi (鏈堜箣鏆楅潰)** | Compatible | `KIMI_API_KEY` / `MOONSHOT_API_KEY`  |
| **Qwen (閫氫箟鍗冮棶)** | Compatible | `QWEN_API_KEY` / `DASHSCOPE_API_KEY` |
| **Doubao (璞嗗寘)**   | Compatible | `DOUBAO_API_KEY` / `ARK_API_KEY`     |
| **OpenRouter**      | Compatible | `OPENROUTER_API_KEY`                 |
| **SiliconFlow**     | Compatible | `SILICONFLOW_API_KEY`                |
| **Groq**            | Compatible | `GROQ_API_KEY`                       |
| **Together**        | Compatible | `TOGETHER_API_KEY`                   |
| **Fireworks**       | Compatible | `FIREWORKS_API_KEY`                  |
| **xAI (Grok)**      | Compatible | `XAI_API_KEY`                        |
| **Mistral**         | Compatible | `MISTRAL_API_KEY`                    |
| **Custom**          | Compatible | `LLM_API_KEY` + `LLM_BASE_URL`       |

### 鈱笍 REPL Commands


| Command              | Description                                                |
| -------------------- | ---------------------------------------------------------- |
| `/model <name>`      | Switch AI model                                            |
| `/provider <name>`   | Switch provider                                            |
| `/api-key <key>`     | Set API key                                                |
| `/base-url <url>`    | Custom API endpoint                                        |
| `/mode <mode>`       | Permission mode:`default|plan|auto|bypass`                 |
| `/budget <amount>`   | Set session budget cap ($);`auto` for smart adjustment     |
| `/cost`              | View token usage & cost (session + 24h + 7d + all-time)    |
| `/ratelimit`         | View API rate limit status                                 |
| `/config`            | View full configuration                                    |
| `/system <prompt>`   | Custom system prompt                                       |
| `/attach <file...>`  | Attach images/files (`list`/`remove`/`clear` sub-commands) |
| `/cd <path>`         | Change working directory                                   |
| `/context`           | View current context                                       |
| `/compact`           | Compress conversation history                              |
| `/undo`              | Revert to previous checkpoint                              |
| `/checkpoints`       | List all checkpoints                                       |
| `/history`           | View and resume past sessions                              |
| `/resume [id]`       | List or resume saved sessions                              |
| `/export`            | Export conversation to Markdown                            |
| `/memory [add|list]` | Manage persistent memory                                   |
| `/dream`             | Trigger memory consolidation                               |
| `/tasks`             | View running/queued background tasks                       |
| `/stop` / `/cancel`  | Cancel current running task                                |
| `/commit [msg]`      | Git add + commit                                           |
| `/review`            | Review working changes                                     |
| `/diff`              | Show git diff                                              |
| `/doctor`            | System diagnostics (`full`/`quick`/`codes`)                |
| `/mcp`               | MCP server management                                      |
| `/plugin`            | Plugin management                                          |
| `/skills`            | Skill listing                                              |
| `/help`              | Show help                                                  |
| `/exit`              | Exit REPL                                                  |

### 鈿欙笍 Configuration

Configuration is read from three tiers (lowest to highest priority):

1. **Environment Variables** 鈥?`LLM_API_KEY`, `LLM_BASE_URL`, provider-specific keys
2. **User Config** 鈥?`~/.cove/config.json`
3. **Project Config** 鈥?`.cove.json` in project root

#### Model Routing (Dual-Model)

cove supports intelligent dual-model routing. When you send a message, the system evaluates its complexity:

- **Complex tasks** (containing keywords like `refactor`, `architecture`, `debug`, `閲嶆瀯`, `鏋舵瀯` etc., or messages longer than 500 chars) 鈫?uses the **primary model** (`model`)
- **Simple/short tasks** 鈫?uses the **fast model** (`model_fast`) for speed and cost savings

If `model` is empty or `"auto"`, it auto-resolves to the provider's default premium model. If `model_fast` is empty or `"auto"`, it reuses the main model (safe no-op).

#### Model Fallback (Automatic)

When the primary provider is unavailable (rate limited, down, or auth error), cove automatically falls back to other configured providers (set via environment variables).

#### Policy Engine (Persistent Permissions)

Permission decisions can be persisted as rules in `~/.cove/policy.json`:

- `always_allow` 鈥?auto-approve matching tools
- `always_deny` 鈥?auto-reject matching tools
- `ask` 鈥?always prompt user

Supports wildcard tool patterns (e.g. `"mcp_*_*"`), parameter conditions, and optional expiration.

#### Other Built-in Safeguards

- **3-Layer Loop Detection** 鈥?fingerprint match, output hash match, stagnation detection (60 rounds no file changes)
- **AI Context Compression** 鈥?auto-triggers at 50% token limit, writes AI summaries of old messages
- **Tool Output Masking** 鈥?trims large old tool outputs to save tokens
- **Safety Filters** 鈥?blocks dangerous commands (`rm -rf`, `dd`, `mkfs`), path traversal, and API key leakage

Example `~/.cove/config.json`:

```json
{
  "debug": false,
  "max_budget_usd": 10,
  "mcp_servers": {
    "fs": {
      "args": [
        "-y",
        "@modelcontextprotocol/server-filesystem",
        "/tmp"
      ],
      "command": "npx"
    }
  },
  "model": "deepseek-v4-pro",
  "model_fast": "deepseek-v4-flash",
  "permission_mode": "default",
  "provider": {
    "api_key": "sk-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
    "base_url": "https://api.deepseek.com/v1",
    "name": "deepseek"
  },
  "system_prompt": "浣犳槸 Cove锛屼竴涓?AI 缂栫▼鍔╂墜銆傛牳蹇冭亴璐ｏ細浣跨敤宸ュ叿瀹屾垚浠诲姟锛岃€岄潪鎻忚堪浠诲姟銆傛墍鏈夋搷浣滃繀椤婚€氳繃宸ュ叿瀹屾垚銆傚崟姝ョ洿鎺ュ仛锛屽姝ョ敤 todowrite 鍒嗚В璺熻釜銆? 浠€涔堝彨 鍋氬畬浜?1. 浜や粯鐗?= 鐪熷疄宸ュ叿杈撳嚭锛屼笉鏄弿杩般€備唬鐮佸繀椤诲疄闄呰繍琛?缂栬瘧閫氳繃鎵嶇畻瀹屾垚銆?. 缁濅笉瑕佺紪閫犵粨鏋溿€傚伐鍏峰け璐ュ氨濡傚疄鎶ュ憡锛屽皾璇曟浛浠ｆ柟妗堬紝缁濅笉浼€犺緭鍑恒€?. 瀹屾垚鍚庤嚜鏌ワ細鏂囦欢鐪熸敼浜嗭紵浠ｇ爜鐪熻窇浜嗭紵鏈夋病鏈夋湭澶勭悊鐨勯敊璇紵# 宸ュ叿浣跨敤- 鏂囦欢 鈫?write(鏂板缓/閲嶅啓) / read / edit(淇敼)- 鍛戒护/鏋勫缓/娴嬭瘯/git 鈫?bash- 鎼滅储 鈫?grep / glob- 缃戠粶 鈫?webfetch / websearch- 浠诲姟绠＄悊 鈫?todowrite + execute_plan锛?姝ヤ互涓婏級- 瀛愪唬鐞?鈫?agent锛堢嫭绔嬪瓙浠诲姟锛夊苟鍙戣鍒欙細鐙珛璇诲彇鍙苟琛岋紝鍐欐枃浠跺繀椤讳覆琛岋紝鏈変緷璧栧厛璇诲悗鏀广€傛枃浠惰鑼冿細鏂板缓鐢?write 涓€娆℃€у啓鍏ュ畬鏁村唴瀹癸紝涓嶈鐢ㄥ涓皬 edit 鎷煎噾銆? 閿欒闃叉姢- 杩炵画3娆″け璐?鈫?鎹㈢瓥鐣? 琚埅鏂?鈫?绛夌户缁寚浠? 妫€娴嬪埌寰幆 鈫?绔嬪埢鎹㈡柟娉? 鏈€澶?200 杞? 椋庢牸绠€娲併€佽鍔ㄣ€侀€忔槑銆備腑鏂囩敤鎴风敤涓枃鍥炲銆?,
  "telemetry": true,
  "thinking_tokens": 16000,
  "verbose": false
}


### 馃 Agent Tools

| Tool | Description | Read-Only |
|------|-------------|-----------|
| `read` | Read files or list directories | 鉁?|
| `write` | Create or overwrite files | |
| `edit` | Exact string replacements in files | |
| `glob` | Find files by glob pattern | 鉁?|
| `grep` | Regex search in files | 鉁?|
| `bash` | Execute bash commands | |
| `powershell` | Execute PowerShell commands | |
| `webfetch` | HTTP fetch 鈫?Markdown | 鉁?|
| `websearch` | DuckDuckGo web search | 鉁?|
| `browser` | Headless Chrome: navigate + screenshot | 鉁?|
| `todowrite` | Structured task list management | |
| `execute_plan` | Execute task plans with sub-agents | |
| `plan_mode` | Enter read-only plan mode | 鉁?|
| `task` / `task_list` / `task_update` | Background task CRUD | 鉁?|
| `agent` | Spawn sub-agent for complex tasks | |
| `team_create` / `team_delete` | Manage agent teams | |
| `send_message` | Message passing between tasks/teams | |
| `cron` | Schedule recurring tasks | |
| `question` | Ask user multiple-choice questions | 鉁?|
| `skill` / `skill_view` / `skills_list` | Skill discovery & execution | 鉁?|
| `mcp` / `mcp_resources` | MCP tool & resource access | varies |
| `sleep` | Pause execution | 鉁?|
| `brief` | Generate session summary | 鉁?|
| `worktree` / `exit_worktree` | Git worktree management | |

### 馃搨 Project Structure

```text
cove/
鈹溾攢鈹€ .github/            # GitHub CI/CD, templates
鈹?  鈹斺攢鈹€ workflows/      # CI & Release workflows
鈹溾攢鈹€ agent/              # Go source code
鈹?  鈹溾攢鈹€ cmd/cove/    # Entry point
鈹?  鈹斺攢鈹€ internal/       # 25+ internal packages
鈹溾攢鈹€ mobile/             # Go engine for Android (CovePhone)
鈹溾攢鈹€ dist/               # Release artifacts
鈹溾攢鈹€ scripts/            # Build & test scripts
鈹溾攢鈹€ CHANGELOG.md        # Release history
鈹溾攢鈹€ CONTRIBUTING.md     # Contribution guide
鈹溾攢鈹€ LICENSE             # MIT License
鈹斺攢鈹€ README.md           # This file
```

### 馃 Contributing

Contributions are welcome! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

### 馃搫 License

MIT 鈥?see [LICENSE](LICENSE) for details.

---

<a name="chinese"></a>

## 涓枃

cove 鏄竴涓函 CLI 鐨?AI 缂栫▼鍔╂墜锛屼互鍗曟枃浠?Go 浜岃繘鍒跺舰寮忓彂甯冦€傚畠杩愯鍦ㄧ粓绔腑锛屾敮鎸佸绉?AI 鎻愪緵鍟嗭紝涓撲负鏈湴寮€鍙戙€佽剼鏈皟鐢ㄥ拰渚挎惡鍒嗗彂鑰岃璁°€?
### 鉁?鐗规€?
- 馃幆 **鍗曟枃浠朵簩杩涘埗** 鈥?闆朵緷璧栵紝涓嬭浇鍗崇敤
- 馃寪 **澶氭彁渚涘晢** 鈥?Anthropic銆丱penAI銆丏eepSeek 鍙?10+ 涓吋瀹规帴鍙?- 馃枼锔?**璺ㄥ钩鍙?* 鈥?Windows銆乵acOS (Intel & Apple Silicon)銆丩inux
- 馃帹 **浜や簰寮?REPL** 鈥?25+ 涓枩鏉犲懡浠わ紝寮傛浠诲姟闃熷垪锛屼細璇濈鐞?- 馃敡 **鐏垫椿宸ュ叿闆?* 鈥?鏂囦欢鎿嶄綔銆乻hell/PowerShell銆佷唬鐮佹悳绱€佺綉椤垫姄鍙?鎼滅储銆乭eadless 娴忚鍣?- 馃 **鑷涔犵郴缁?* 鈥?鑷姩璁板繂鎻愬彇銆佹妧鑳藉垱寤恒€佽法浼氳瘽鏁村悎 (Dream)
- 馃搵 **璁″垝鎵ц鍣?* 鈥?澹版槑寮忓姝ラ浠诲姟璁″垝锛屼緷璧?DAG + 骞惰瀛愭櫤鑳戒綋鎵ц
- 馃懃 **澶氭櫤鑳戒綋涓庡洟闃?* 鈥?瀛愭櫤鑳戒綋鐢熸垚銆佸洟闃熷垱寤轰笌娑堟伅浼犻€掋€乧ron 瀹氭椂璋冨害
- 馃攲 **MCP 鏀寔** 鈥?Model Context Protocol 鏈嶅姟鍣ㄩ泦鎴?(stdio + SSE + Streamable HTTP)
- 馃幁 **鏉冮檺妯″紡** 鈥?default | plan | auto | bypass锛屾櫤鑳藉垎绫诲櫒
- 馃洝锔?**鎶ゆ爮淇濇姢** 鈥?宸ュ叿寰幆妫€娴嬨€佸揩閫熷け璐ユ柇璺櫒銆佸箓绛夌粨鏋滄娴?- 馃攧 **妫€鏌ョ偣** 鈥?鍐欏叆鍓嶈嚜鍔?Git 蹇収锛屾敮鎸佹挙娑堝洖閫€
- 馃┖ **璇婃柇绯荤粺** 鈥?30+ 閿欒鐮侊紝鍚姩妫€鏌ワ紝鐑慨澶嶆棤闇€閲嶅惎
- 馃摝 **鎻掍欢涓庢妧鑳?* 鈥?鍙墿灞曟灦鏋勶紝鍐呯疆 23+ 鎶€鑳斤紝鏀寔鑷畾涔?- 馃挵 **璐圭敤杩借釜** 鈥?瀹炴椂 token 璁℃暟銆佹垚鏈及绠椼€侀绠椾笂闄愩€侀€熺巼闄愬埗鎰熺煡
- 馃摫 **CovePhone** 鈥?Android 鎵嬫満 AI 鍔╂墜搴旂敤
- 馃尪 **TUI 主题系统** 鈥?5 套内置主题（Catppuccin、Dracula、Gruvbox、OneDark、TokyoNight），支持热切换

### 馃摜 瀹夎

#### 涓嬭浇棰勭紪璇戜簩杩涘埗

鍓嶅線 [Releases](https://github.com/liuzhixin405/cove/releases) 涓嬭浇瀵瑰簲骞冲彴鐨勫帇缂╁寘锛?

| 骞冲彴                  | 鏂囦欢                          |
| --------------------- | ----------------------------- |
| Windows (amd64)       | `cove-v*-windows-amd64.zip`   |
| macOS (Intel)         | `cove-v*-darwin-amd64.tar.gz` |
| macOS (Apple Silicon) | `cove-v*-darwin-arm64.tar.gz` |
| Linux (amd64)         | `cove-v*-linux-amd64.tar.gz`  |

瑙ｅ帇杩愯锛?
```bash
# macOS / Linux
tar -xzf cove-v*-linux-amd64.tar.gz
./cove

# Windows (PowerShell)
Expand-Archive cove-v*-windows-amd64.zip -DestinationPath .
.\cove.exe
```

寤鸿灏嗙▼搴忕洰褰曟坊鍔犲埌 `PATH` 浠ヤ究鍏ㄥ眬浣跨敤銆?
#### 浠庢簮鐮佹瀯寤?
```bash
git clone https://github.com/liuzhixin405/cove.git
cd cove/agent
go build -o cove ./cmd/cove
./cove --version
```

闇€瑕?Go 1.24+銆?
### 馃摫 CovePhone (Android)

CovePhone 鏄?cove 鐨?**Android 鎵嬫満浼翠荆搴旂敤**锛屽皢 AI 鍔╂墜鑳藉姏甯﹀埌浣犵殑鎵嬫満涓娿€?
- 馃 **鍘熺敓 Go 寮曟搸** 鈥?鍩轰簬 `cove-core.aar`锛堥€氳繃 `gomobile` 缂栬瘧鐨?Go 妯″潡锛夌殑鐪熷疄 AI 寮曟搸
- 馃挰 **瀹屾暣鑱婂ぉ鐣岄潰** 鈥?娑堟伅鍒楄〃甯︽€濊€冭繃绋嬫樉绀猴紝骞虫粦婊氬姩锛屾壒閲忔覆鏌撶殑 thinking 鍧?- 鈿欙笍 **璁剧疆涓庨厤缃?* 鈥?API key銆佹ā鍨嬮€夋嫨銆佹彁渚涘晢閫夋嫨锛岄€氳繃 SharedPreferences 鎸佷箙鍖?- 馃攲 **DeepSeek API** 鈥?鐩存帴浠庢墜鏈鸿繛鎺?DeepSeek锛堟垨鍏朵粬鍏煎鎻愪緵鍟嗭級

**涓嬭浇:** [covephone-v4.0.5.apk](dist/v4.0.5/covephone-v4.0.5.apk) (Android, ~47MB)

**婧愮爜:** [`mobile/`](mobile/) 鈥?绉诲姩绔交閲?Go 寮曟搸銆?
### 馃殌 蹇€熷紑濮?
```bash
# 浜や簰寮?REPL
cove

# 鍗曟鏌ヨ
cove -p "鍒涘缓涓€涓椽鍚冭泧 HTML 娓告垙"

# 鏌ョ湅鐗堟湰
cove --version

# 绯荤粺璇婃柇
cove --doctor
```

棣栨杩愯鏃讹紝cove 浼氬紩瀵间綘閰嶇疆 API key銆備篃鍙互鐩存帴璁剧疆锛?
```bash
# 鍦?REPL 涓?/api-key sk-your-key-here

# 鎴栭€氳繃鐜鍙橀噺
export DEEPSEEK_API_KEY="sk-..."
```

### 馃搫 璁稿彲璇?
MIT 鈥?璇﹁ [LICENSE](LICENSE)銆?
### 猸?Star History

濡傛灉杩欎釜椤圭洰瀵逛綘鏈夊府鍔╋紝璇风粰鎴戜滑涓€涓?Star 猸愶紒

[![Star History Chart](https://api.star-history.com/svg?repos=cove/cove&type=Date)](https://star-history.com/#cove/cove&Date)

