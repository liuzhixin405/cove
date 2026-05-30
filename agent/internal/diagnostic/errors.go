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
)

// Permission errors (E3xxx)
const (
	ErrPermDenied     ErrorCode = "E3001"
	ErrPermNoPrompt   ErrorCode = "E3002"
	ErrPermFileAccess ErrorCode = "E3003"
)

// Tool errors (E4xxx)
const (
	ErrToolNotFound   ErrorCode = "E4001"
	ErrToolTimeout    ErrorCode = "E4002"
	ErrToolPanic      ErrorCode = "E4003"
	ErrToolExecFailed ErrorCode = "E4004"
	ErrToolShellMiss  ErrorCode = "E4005"
)

// Engine errors (E5xxx)
const (
	ErrEngineMaxIter    ErrorCode = "E5001"
	ErrEngineCtxCancel  ErrorCode = "E5002"
	ErrEngineCompact    ErrorCode = "E5003"
	ErrEnginePanic      ErrorCode = "E5004"
	ErrEngineNoProvider ErrorCode = "E5005"
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
	Code        ErrorCode
	Category    Category
	Severity    Severity
	Message     string // User-facing summary (Chinese)
	Detail      string // Technical detail template
	Recovery    string // What user can do
	AutoFixable bool   // Whether the diagnostic system can auto-fix this
	HotFixable  bool   // Whether fix takes effect immediately without restart
}

// registry holds all known error definitions.
var registry = map[ErrorCode]*ErrorDef{}

func init() {
	// Config errors — all config fixes are hot-reloadable (take effect immediately)
	register(&ErrorDef{ErrConfigMissing, CatConfig, SevFatal, "配置文件不存在", "找不到配置文件: %s", "运行 /init 创建默认配置，或手动创建 ~/.agentgo/config.yaml", true, true})
	register(&ErrorDef{ErrConfigInvalid, CatConfig, SevFatal, "配置文件格式错误", "YAML解析失败: %s", "检查配置文件语法，或删除后重新生成", true, true})
	register(&ErrorDef{ErrConfigModelInvalid, CatConfig, SevError, "模型名无效", "模型 '%s' 不被当前 provider 支持", "使用 /model 命令切换模型，或在配置中设置有效模型名", true, true})
	register(&ErrorDef{ErrConfigProviderEmpty, CatConfig, SevFatal, "未配置 Provider", "provider.name 为空", "在配置中设置 provider.name (如 deepseek, openai, anthropic)", false, false})
	register(&ErrorDef{ErrConfigAPIKeyMissing, CatConfig, SevFatal, "API Key 未设置", "provider '%s' 需要 API Key", "设置环境变量 LLM_API_KEY 或在配置中设置 provider.api_key", false, false})
	register(&ErrorDef{ErrConfigPermMode, CatConfig, SevWarning, "权限模式无效", "permission_mode '%s' 不是有效值", "有效值: auto, ask, bypass。已回退到 ask 模式", true, true})

	// Network/API errors
	register(&ErrorDef{ErrAPIUnreachable, CatNetwork, SevError, "API 服务不可达", "无法连接到 %s", "检查网络连接和代理设置，确认 base_url 正确", false, false})
	register(&ErrorDef{ErrAPITimeout, CatNetwork, SevWarning, "API 请求超时", "请求超过 %s 未响应", "网络可能不稳定，将自动重试。如果持续超时，检查代理设置", false, false})
	register(&ErrorDef{ErrAPIRateLimit, CatAPI, SevWarning, "触发速率限制", "API 返回 429: %s", "等待片刻后自动重试。如频繁触发，考虑降低请求频率或升级 API 套餐", false, false})
	register(&ErrorDef{ErrAPIAuth, CatAPI, SevFatal, "认证失败", "API 返回 401/403: %s", "检查 API Key 是否正确且未过期", false, false})
	register(&ErrorDef{ErrAPIBadRequest, CatAPI, SevError, "请求参数错误", "API 返回 400: %s", "可能是模型名不正确或请求格式不兼容，已自动调整", true, true})
	register(&ErrorDef{ErrAPIServerError, CatAPI, SevWarning, "API 服务端错误", "API 返回 5xx: %s", "服务端临时问题，将自动重试", false, false})
	register(&ErrorDef{ErrAPIStreamBroken, CatNetwork, SevWarning, "流式连接中断", "SSE 流读取失败: %s", "网络波动导致连接断开，将自动重试", false, false})

	// Permission errors
	register(&ErrorDef{ErrPermDenied, CatPermission, SevInfo, "操作被拒绝", "用户拒绝了 %s 的执行", "这是正常的安全行为，Agent 会尝试替代方案", false, false})
	register(&ErrorDef{ErrPermNoPrompt, CatPermission, SevError, "无法显示权限提示", "PermissionPrompt 回调未设置", "非交互模式下无法请求权限确认，已自动切换为 auto 模式", true, true})
	register(&ErrorDef{ErrPermFileAccess, CatFileSystem, SevError, "文件访问被拒", "无法访问 %s: 权限不足", "检查文件权限，或以管理员身份运行", false, false})

	// Tool errors
	register(&ErrorDef{ErrToolNotFound, CatTool, SevWarning, "工具未注册", "找不到工具: %s", "可能是 Agent 请求了不存在的工具名，会自动重试", false, false})
	register(&ErrorDef{ErrToolTimeout, CatTool, SevWarning, "工具执行超时", "%s 执行超过 %s", "命令可能挂起，已被终止。可以设置更长的 timeout 参数", false, false})
	register(&ErrorDef{ErrToolPanic, CatTool, SevError, "工具执行崩溃", "%s 发生了内部错误: %v", "这是一个 Bug，请反馈到项目 Issue", false, false})
	register(&ErrorDef{ErrToolExecFailed, CatTool, SevWarning, "命令执行失败", "%s 退出码 %d", "命令返回了错误，Agent 会分析输出并调整", false, false})
	register(&ErrorDef{ErrToolShellMiss, CatTool, SevError, "Shell 不可用", "找不到 %s", "确保系统 PATH 中有可用的 shell (bash/powershell)", false, false})

	// Engine errors
	register(&ErrorDef{ErrEngineMaxIter, CatEngine, SevWarning, "达到最大迭代次数", "Agent 执行了 %d 次迭代未完成", "任务可能过于复杂，尝试拆分为更小的子任务", false, false})
	register(&ErrorDef{ErrEngineCtxCancel, CatEngine, SevInfo, "操作被中断", "用户取消了当前操作", "可以重新输入继续，之前的上下文保留", false, false})
	register(&ErrorDef{ErrEngineCompact, CatEngine, SevInfo, "上下文已压缩", "对话超过 %d tokens，已自动压缩", "这是正常行为，较早的细节可能丢失", false, false})
	register(&ErrorDef{ErrEnginePanic, CatEngine, SevFatal, "引擎内部崩溃", "未捕获的异常: %v", "引擎已自动恢复，当前对话可继续使用", false, false})
	register(&ErrorDef{ErrEngineNoProvider, CatEngine, SevFatal, "未初始化 Provider", "engine 缺少 provider 实例", "配置错误，请使用 /config provider.name xxx 设置后立即生效", false, false})

	// Session/FS errors
	register(&ErrorDef{ErrSessionCorrupt, CatSession, SevWarning, "会话数据损坏", "无法加载会话 %s", "损坏的会话将被跳过，可以使用 /new 开始新会话", true, true})
	register(&ErrorDef{ErrSessionSave, CatSession, SevWarning, "会话保存失败", "写入失败: %s", "可能是磁盘空间不足或权限问题", false, false})
	register(&ErrorDef{ErrFSPermission, CatFileSystem, SevError, "文件系统权限错误", "无法写入 %s", "检查目录权限，或尝试其他路径", false, false})
	register(&ErrorDef{ErrFSDiskFull, CatFileSystem, SevFatal, "磁盘空间不足", "写入失败，可用空间: %s", "请清理磁盘空间，清理后可继续使用无需重启", false, false})
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
