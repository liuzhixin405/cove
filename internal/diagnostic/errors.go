package diagnostic

import "fmt"

// Severity indicates how critical an error is.
type Severity int

const (
	SevInfo      Severity = iota // Informational, no action needed
	SevWarning                   // Degraded functionality, can continue
	SevError                     // Significant problem, feature unavailable
	SevFatal                     // Cannot continue, must fix before proceeding
	SevRecovered                 // Was an error but has been auto-fixed
)

func (s Severity) String() string {
	switch s {
	case SevInfo:
		return "INFO"
	case SevWarning:
		return "WARN"
	case SevError:
		return "ERROR"
	case SevFatal:
		return "FATAL"
	case SevRecovered:
		return "FIXED"
	default:
		return "UNKNOWN"
	}
}

func (s Severity) Color() string {
	switch s {
	case SevInfo:
		return "\x1b[36m" // cyan
	case SevWarning:
		return "\x1b[33m" // yellow
	case SevError:
		return "\x1b[31m" // red
	case SevFatal:
		return "\x1b[1;31m" // bold red
	case SevRecovered:
		return "\x1b[32m" // green
	default:
		return ""
	}
}

// Category groups errors by subsystem.
type Category string

const (
	CatConfig     Category = "config"
	CatNetwork    Category = "network"
	CatAPI        Category = "api"
	CatPermission Category = "permission"
	CatTool       Category = "tool"
	CatEngine     Category = "engine"
	CatSession    Category = "session"
	CatFileSystem Category = "filesystem"
)

// ErrorCode is a unique identifier for each known error type.
type ErrorCode string

// Config errors (E1xxx)
const (
	ErrConfigMissing       ErrorCode = "E1001"
	ErrConfigInvalid       ErrorCode = "E1002"
	ErrConfigModelInvalid  ErrorCode = "E1003"
	ErrConfigProviderEmpty ErrorCode = "E1004"
	ErrConfigAPIKeyMissing ErrorCode = "E1005"
	ErrConfigPermMode      ErrorCode = "E1006"
	// ErrConfigAPIKeyPlaceholder is the "sk-xxxx…" key older versions of the
	// missing-config auto-fix wrote into config.json.
	ErrConfigAPIKeyPlaceholder ErrorCode = "E1007"
)

// Network/API errors (E2xxx)
const (
	ErrAPIUnreachable  ErrorCode = "E2001"
	ErrAPITimeout      ErrorCode = "E2002"
	ErrAPIRateLimit    ErrorCode = "E2003"
	ErrAPIAuth         ErrorCode = "E2004"
	ErrAPIBadRequest   ErrorCode = "E2005"
	ErrAPIServerError  ErrorCode = "E2006"
	ErrAPIStreamBroken ErrorCode = "E2007"
	// ErrAPIContextLength: the request did not fit the model's context
	// window, even after the engine compacted and retried.
	ErrAPIContextLength ErrorCode = "E2008"
	// ErrAPIProviderUnavailable is no longer produced (the provider fallback
	// chain is gone); it stays registered so old errors.log entries render.
	// It was: the fallback chain stopped preferring a
	// provider after repeated failures.
	ErrAPIProviderUnavailable ErrorCode = "E2009"
)

// Permission errors (E3xxx)
const (
	ErrPermDenied     ErrorCode = "E3001"
	ErrPermNoPrompt   ErrorCode = "E3002"
	ErrPermFileAccess ErrorCode = "E3003"
	// ErrPermPolicyLoad: policies.json exists but could not be read or
	// parsed, so its persisted rules (deny rules included) are not applied.
	ErrPermPolicyLoad ErrorCode = "E3004"
)

// Tool errors (E4xxx)
const (
	ErrToolNotFound   ErrorCode = "E4001"
	ErrToolTimeout    ErrorCode = "E4002"
	ErrToolPanic      ErrorCode = "E4003"
	ErrToolExecFailed ErrorCode = "E4004"
	ErrToolShellMiss  ErrorCode = "E4005"
	ErrToolNoGitBash  ErrorCode = "E4006"
	ErrToolShellWSL   ErrorCode = "E4007"
	ErrToolGitMissing ErrorCode = "E4008"
	// ErrToolArgsInvalid: a model's tool call carried arguments that were
	// not JSON even after repair.
	ErrToolArgsInvalid ErrorCode = "E4009"
)

// Engine errors (E5xxx)
const (
	ErrEngineMaxIter    ErrorCode = "E5001"
	ErrEngineCtxCancel  ErrorCode = "E5002"
	ErrEngineCompact    ErrorCode = "E5003"
	ErrEnginePanic      ErrorCode = "E5004"
	ErrEngineNoProvider ErrorCode = "E5005"
	// ErrEngineDreamFailed: the last background memory consolidation failed.
	ErrEngineDreamFailed ErrorCode = "E5006"
	// ErrEngineStall: a stage made no progress for stallThreshold.
	ErrEngineStall ErrorCode = "E5007"
)

// Session/FS errors (E6xxx)
const (
	ErrSessionCorrupt ErrorCode = "E6001"
	ErrSessionSave    ErrorCode = "E6002"
	ErrFSPermission   ErrorCode = "E6003"
	ErrFSDiskFull     ErrorCode = "E6004"
)

// ErrorDef defines a known error type with its metadata and recovery info.
type ErrorDef struct {
	Code     ErrorCode
	Category Category
	Severity Severity
	Message  string // User-facing summary (Chinese)
	Detail   string // Technical detail template
	Recovery string // What the user can do
	// Remedy, when set, is an action the diagnostic layer may take at run
	// time when an event with this code is reported (see report.go). It
	// changes run-time strategy or session-level parameters only, never
	// code or config files, and says what it did.
	Remedy RemedyFunc
}

// registry holds all known error definitions.
var registry = map[ErrorCode]*ErrorDef{}

func init() {
	// Config errors — all config fixes are hot-reloadable (take effect immediately)
	// ErrConfigMissing is a warning, not fatal: cove runs on defaults plus an
	// API key from the environment. The old hint sent users to /init, which
	// writes CLAUDE.md, and the old "auto-fix" wrote a placeholder API key.
	register(&ErrorDef{ErrConfigMissing, CatConfig, SevWarning, "配置文件不存在", "找不到配置文件: %s", "设置对应的 API Key 环境变量（如 DEEPSEEK_API_KEY），或用 /provider <名称> 和 /api-key <密钥> 保存到配置文件", nil})
	register(&ErrorDef{ErrConfigInvalid, CatConfig, SevError, "配置文件格式错误", "JSON解析失败: %s", "按提示的位置修正 JSON 语法；在修好之前该文件中的设置不会生效，/model 等命令也不会覆盖它", nil})
	register(&ErrorDef{ErrConfigAPIKeyPlaceholder, CatConfig, SevFatal, "API Key 是占位符", "provider.api_key 仍是示例值 %s（旧版诊断自动写入）", "用 /api-key <密钥> 设置真实密钥，或删除该字段改用环境变量", nil})
	register(&ErrorDef{ErrConfigModelInvalid, CatConfig, SevError, "模型名无效", "模型 '%s' 不被当前 provider 支持", "使用 /model 命令切换模型，或在配置中设置有效模型名", nil})
	register(&ErrorDef{ErrConfigProviderEmpty, CatConfig, SevFatal, "未配置 Provider", "provider.name 为空", "在配置中设置 provider.name (如 deepseek, openai, anthropic)", nil})
	register(&ErrorDef{ErrConfigAPIKeyMissing, CatConfig, SevFatal, "API Key 未设置", "provider '%s' 需要 API Key", "设置环境变量 LLM_API_KEY 或在配置中设置 provider.api_key", nil})
	register(&ErrorDef{ErrConfigPermMode, CatConfig, SevWarning, "权限模式无效", "permission_mode '%s' 不是有效值", "有效值: default, plan, auto, bypass。当前按 default 模式运行", nil})

	// Network/API errors
	register(&ErrorDef{ErrAPIUnreachable, CatNetwork, SevError, "API 服务不可达", "无法连接到 %s", "检查网络连接和代理设置，确认 base_url 正确", nil})
	register(&ErrorDef{ErrAPITimeout, CatNetwork, SevWarning, "API 请求超时", "请求超过 %s 未响应", "服务端在超时前没有回应：本地模型常见于模型加载或显存不足，云端服务检查网络与代理；稍后重试或 /continue", nil})
	register(&ErrorDef{ErrAPIRateLimit, CatAPI, SevWarning, "触发速率限制", "API 返回 429: %s", "等待片刻后重试或 /continue；如频繁触发，考虑降低请求频率或升级 API 套餐", nil})
	register(&ErrorDef{ErrAPIAuth, CatAPI, SevFatal, "认证失败", "API 返回 401/403: %s", "检查 API Key 是否正确且未过期", nil})
	register(&ErrorDef{ErrAPIBadRequest, CatAPI, SevError, "请求参数错误", "API 返回 400: %s", "检查模型名与请求格式；若是本地服务，确认其兼容 OpenAI 接口", nil})
	register(&ErrorDef{ErrAPIContextLength, CatAPI, SevError, "上下文超出模型窗口", "%s",
		"输入 /continue 会先压缩对话历史再重试；若仍失败，需调大模型的上下文长度（llama-server 加 -c 65536、LM Studio 的 Context Length、Ollama 的 num_ctx），cove 的系统提示词和工具定义本身约占 13K token", nil})
	register(&ErrorDef{ErrAPIProviderUnavailable, CatAPI, SevError, "供应商已被标记不可用", "%s",
		"连续 3 次失败后本会话不再优先尝试该供应商；只有一个供应商时仍会继续尝试它，修好原因（如调大模型上下文）后成功一次即自动复位；有多个供应商时可用 /provider <名称> 切换", nil})
	register(&ErrorDef{ErrAPIServerError, CatAPI, SevWarning, "API 服务端错误", "API 返回 5xx: %s", "服务端临时问题，稍后重试或 /continue；持续出现时检查服务状态", nil})
	register(&ErrorDef{ErrAPIStreamBroken, CatNetwork, SevWarning, "流式连接中断", "SSE 流读取失败: %s", "连接在回复中途断开，输入 /continue 从中断处继续；持续出现时检查网络与代理", nil})

	// Permission errors
	register(&ErrorDef{ErrPermDenied, CatPermission, SevInfo, "操作被拒绝", "用户拒绝了 %s 的执行", "这是正常的安全行为，Agent 会尝试替代方案", nil})
	register(&ErrorDef{ErrPermNoPrompt, CatPermission, SevError, "无法显示权限提示", "PermissionPrompt 回调未设置", "非交互模式下无法请求权限确认，请用 bypass 模式或事先在 policies.json 写好 allow 规则", nil})
	register(&ErrorDef{ErrPermPolicyLoad, CatPermission, SevError, "权限规则文件无法加载", "%s", "修正 policies.json 的 JSON 语法（或删除该文件）后重启 cove；在修好之前其中的规则（包括 deny 规则）都不生效", nil})
	register(&ErrorDef{ErrPermFileAccess, CatFileSystem, SevError, "文件访问被拒", "无法访问 %s: 权限不足", "检查文件权限，或以管理员身份运行", nil})

	// Tool errors
	register(&ErrorDef{ErrToolNotFound, CatTool, SevWarning, "工具未注册", "找不到工具: %s", "可能是 Agent 请求了不存在的工具名，会自动重试", nil})
	register(&ErrorDef{ErrToolTimeout, CatTool, SevWarning, "工具执行超时", "%s 执行超过 %s", "命令可能挂起，已被终止。可以设置更长的 timeout 参数", nil})
	register(&ErrorDef{ErrToolPanic, CatTool, SevError, "工具执行崩溃", "%s 发生了内部错误: %v", "这是一个 Bug，请反馈到项目 Issue", nil})
	register(&ErrorDef{ErrToolExecFailed, CatTool, SevWarning, "命令执行失败", "%s 退出码 %d", "命令返回了错误，Agent 会分析输出并调整", nil})
	register(&ErrorDef{ErrToolShellMiss, CatTool, SevError, "Shell 不可用", "找不到 %s", "确保系统 PATH 中有可用的 shell (bash/powershell)", nil})
	register(&ErrorDef{ErrToolNoGitBash, CatTool, SevWarning, "未找到 Git Bash", "命令将由 %s 执行，模型写出的 bash 语法可能失败", "安装 Git for Windows (https://git-scm.com/download/win)，重启 cove 后会自动使用其中的 bash", nil})
	register(&ErrorDef{ErrToolShellWSL, CatTool, SevError, "Shell 是 WSL 启动器", "%s 会把命令交给 WSL 发行版执行，而不是 Windows 工具链", "安装 Git for Windows，或把 Git 的 bin 目录放到 PATH 中 System32 之前", nil})
	register(&ErrorDef{ErrToolGitMissing, CatTool, SevWarning, "未找到 git", "PATH 中没有 git", "安装 git 后重启 cove；检查点 (/rewind) 和工作树等功能依赖 git", nil})
	register(&ErrorDef{ErrToolArgsInvalid, CatTool, SevWarning, "工具参数非法 JSON", "%s",
		"模型输出的工具参数无法解析；同一模型反复出现说明它不适合工具调用，考虑换模型或降低 temperature", nil})

	// Engine errors
	register(&ErrorDef{ErrEngineMaxIter, CatEngine, SevWarning, "达到最大迭代次数", "Agent 执行了 %d 次迭代未完成", "任务可能过于复杂，尝试拆分为更小的子任务", nil})
	register(&ErrorDef{ErrEngineCtxCancel, CatEngine, SevInfo, "操作被中断", "用户取消了当前操作", "可以重新输入继续，之前的上下文保留", nil})
	register(&ErrorDef{ErrEngineCompact, CatEngine, SevInfo, "上下文已压缩", "对话超过 %d tokens，已自动压缩", "这是正常行为，较早的细节可能丢失", nil})
	register(&ErrorDef{ErrEnginePanic, CatEngine, SevFatal, "引擎内部崩溃", "未捕获的异常: %v", "引擎已自动恢复，当前对话可继续使用", nil})
	register(&ErrorDef{ErrEngineDreamFailed, CatEngine, SevWarning, "后台记忆整理失败", "%s", "通常是临时的 API 错误，下次满足门槛时会自动重试；也可以用 /dream run 立即重试", nil})
	register(&ErrorDef{ErrEngineNoProvider, CatEngine, SevFatal, "未初始化 Provider", "engine 缺少 provider 实例", "配置错误，请使用 /config provider.name xxx 设置后立即生效", nil})
	register(&ErrorDef{ErrEngineStall, CatEngine, SevWarning, "模型调用无进展", "%s",
		"长时间无响应可按 Ctrl+C 中断；本地模型常见于上下文接近上限或显存不足", nil})

	// Session/FS errors
	register(&ErrorDef{ErrSessionCorrupt, CatSession, SevWarning, "会话数据损坏", "无法加载会话 %s", "损坏的会话已跳过；重启 cove 即开始新会话，/history 可查看其余可恢复的会话", nil})
	register(&ErrorDef{ErrSessionSave, CatSession, SevWarning, "会话保存失败", "写入失败: %s", "可能是磁盘空间不足或权限问题", nil})
	register(&ErrorDef{ErrFSPermission, CatFileSystem, SevError, "文件系统权限错误", "无法写入 %s", "检查目录权限，或尝试其他路径", nil})
	register(&ErrorDef{ErrFSDiskFull, CatFileSystem, SevFatal, "磁盘空间不足", "写入失败，可用空间: %s", "请清理磁盘空间，清理后可继续使用无需重启", nil})
}

func register(def *ErrorDef) {
	registry[def.Code] = def
}

// Lookup returns the error definition for a given code, or nil.
func Lookup(code ErrorCode) *ErrorDef {
	return registry[code]
}

// AllErrors returns all registered error definitions.
func AllErrors() map[ErrorCode]*ErrorDef {
	return registry
}

// DiagError represents a concrete error instance with context.
type DiagError struct {
	Def    *ErrorDef
	Detail string // Formatted detail message
	Fixed  bool   // Whether it was auto-fixed
}

func (e *DiagError) Error() string {
	return fmt.Sprintf("[%s] %s: %s", e.Def.Code, e.Def.Message, e.Detail)
}

// Format returns a colored, user-friendly string for terminal display.
func (e *DiagError) Format() string {
	const reset = "\x1b[0m"
	sev := e.Def.Severity
	if e.Fixed {
		sev = SevRecovered
	}
	color := sev.Color()

	line := fmt.Sprintf("%s[%s %s]%s %s", color, e.Def.Code, sev.String(), reset, e.Def.Message)
	if e.Detail != "" {
		line += fmt.Sprintf("\n  %s详情:%s %s", "\x1b[2m", reset, e.Detail)
	}
	if e.Fixed {
		line += fmt.Sprintf("\n  %s✓ 已自动修复，立即生效%s", "\x1b[32m", reset)
	} else if e.Def.Recovery != "" {
		line += fmt.Sprintf("\n  %s💡 %s%s", "\x1b[33m", e.Def.Recovery, reset)
	}
	return line
}

// New creates a DiagError from a code with formatted detail arguments.
func New(code ErrorCode, args ...any) *DiagError {
	def := registry[code]
	if def == nil {
		return &DiagError{
			Def:    &ErrorDef{Code: code, Category: "unknown", Severity: SevError, Message: "未知错误"},
			Detail: fmt.Sprint(args...),
		}
	}
	detail := ""
	if def.Detail != "" && len(args) > 0 {
		detail = fmt.Sprintf(def.Detail, args...)
	} else if len(args) > 0 {
		detail = fmt.Sprint(args...)
	}
	return &DiagError{Def: def, Detail: detail}
}

// NewFixed creates a DiagError that has been auto-resolved.
func NewFixed(code ErrorCode, args ...any) *DiagError {
	e := New(code, args...)
	e.Fixed = true
	return e
}
