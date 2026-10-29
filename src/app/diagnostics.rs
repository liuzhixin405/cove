//! Diagnostics: the error log (`errors.log`), the interaction trace (`trace.jsonl`),
//! the error-code catalogue and the `/doctor` checks.
//!
//! Both logs are JSON lines under the data directory, size-capped and rotated once
//! (`<name>.1`). The trace records sizes, counts, durations and outcomes only, never
//! message text; the error log records a bounded, key-redacted error message.

use std::{
    io::Write,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use futures_util::future::BoxFuture;
use serde_json::{Value, json};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::{
    config::Settings,
    domain::{Block, Event, ModelRequest, ModelResponse, Provider, Result},
    engine::turn::TurnHook,
    session::{Session, State},
    tools::ToolOutput,
};

pub const TRACE_FILE: &str = "trace.jsonl";
pub const ERROR_FILE: &str = "errors.log";
const TRACE_LIMIT: u64 = 4 * 1024 * 1024;
const ERROR_LIMIT: u64 = 1024 * 1024;
/// Longest error text kept in the error log; trace error heads are shorter.
const MESSAGE_LIMIT: usize = 400;
const HEAD_LIMIT: usize = 160;

/// Append-only JSON-lines file; rotated to `<path>.1` once it would exceed `limit`.
/// Writing never fails loudly: a diagnostic that cannot be written is dropped.
pub struct JsonLog {
    path: PathBuf,
    limit: u64,
    lock: Mutex<()>,
    /// `debug`: each record is also printed to stderr.
    echo: std::sync::atomic::AtomicBool,
}

impl JsonLog {
    pub fn new(path: PathBuf, limit: u64) -> Self {
        Self {
            path,
            limit,
            lock: Mutex::new(()),
            echo: std::sync::atomic::AtomicBool::new(false),
        }
    }

    pub fn path(&self) -> &Path {
        &self.path
    }

    pub fn set_echo(&self, echo: bool) {
        self.echo.store(echo, std::sync::atomic::Ordering::Relaxed);
    }

    /// Appends `{"t": <local RFC 3339>, "kind": kind, ...fields}`.
    pub fn write(&self, kind: &str, fields: Value) {
        let mut line = match fields {
            Value::Object(map) => map,
            _ => serde_json::Map::new(),
        };
        line.insert(
            "t".into(),
            Value::String(
                chrono::Local::now().to_rfc3339_opts(chrono::SecondsFormat::Millis, false),
            ),
        );
        line.insert("kind".into(), Value::String(kind.into()));
        let Ok(mut data) = serde_json::to_vec(&Value::Object(line)) else {
            return;
        };
        if self.echo.load(std::sync::atomic::Ordering::Relaxed) {
            let _ = crate::app::ui::print_text(
                &format!("[debug] {}\n", String::from_utf8_lossy(&data)),
                true,
            );
        }
        data.push(b'\n');
        let Ok(_guard) = self.lock.lock() else {
            return;
        };
        if let Some(parent) = self.path.parent() {
            let _ = std::fs::create_dir_all(parent);
        }
        if std::fs::metadata(&self.path)
            .is_ok_and(|meta| meta.len() + data.len() as u64 > self.limit)
        {
            let rotated = rotated(&self.path);
            let _ = std::fs::remove_file(&rotated);
            let _ = std::fs::rename(&self.path, &rotated);
        }
        let mut options = std::fs::OpenOptions::new();
        options.create(true).append(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        if let Ok(mut file) = options.open(&self.path) {
            let _ = file.write_all(&data);
        }
    }

    /// The last `n` entries, oldest first, from the rotated and the current file.
    pub fn tail(&self, n: usize) -> Vec<Value> {
        let _guard = self.lock.lock();
        let mut entries = read_lines(&self.path);
        if entries.len() < n {
            let mut older = read_lines(&rotated(&self.path));
            older.append(&mut entries);
            entries = older;
        }
        let skip = entries.len().saturating_sub(n);
        entries.split_off(skip)
    }
}

fn rotated(path: &Path) -> PathBuf {
    let mut name = path.as_os_str().to_owned();
    name.push(".1");
    PathBuf::from(name)
}

fn read_lines(path: &Path) -> Vec<Value> {
    std::fs::read(path)
        .map(|data| {
            data.split(|byte| *byte == b'\n')
                .filter(|line| !line.is_empty())
                .filter_map(|line| serde_json::from_slice(line).ok())
                .collect()
        })
        .unwrap_or_default()
}

/// Cuts `text` to at most `limit` bytes on a character boundary.
pub fn clip(text: &str, limit: usize) -> String {
    if text.len() <= limit {
        return text.to_owned();
    }
    let mut end = limit;
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}…", &text[..end])
}

/// A key for display: the first and last four characters at most.
pub fn redact_key(key: &str) -> String {
    let chars: Vec<char> = key.chars().collect();
    if chars.len() <= 12 {
        return "****".into();
    }
    let head: String = chars[..4].iter().collect();
    let tail: String = chars[chars.len() - 4..].iter().collect();
    format!("{head}****{tail}")
}

/// The "sk-xxxx" style keys example configs and old setup tools wrote.
pub fn placeholder_key(key: &str) -> bool {
    let key = key.trim().to_ascii_lowercase();
    if key.contains("****") || key.starts_with('<') || key.starts_with("your") {
        return true;
    }
    if matches!(
        key.as_str(),
        "changeme" | "todo" | "xxx" | "api_key" | "apikey"
    ) {
        return true;
    }
    key.strip_prefix("sk-")
        .is_some_and(|rest| !rest.is_empty() && rest.chars().all(|c| c == 'x' || c == '.'))
}

/// One entry of the diagnostic code catalogue.
pub struct Code {
    pub code: &'static str,
    pub category: &'static str,
    pub message: &'static str,
    pub recovery: &'static str,
}

const fn code(
    code: &'static str,
    category: &'static str,
    message: &'static str,
    recovery: &'static str,
) -> Code {
    Code {
        code,
        category,
        message,
        recovery,
    }
}

/// Stable numbers: a code keeps its meaning across releases.
pub const CODES: &[Code] = &[
    code(
        "E1001",
        "config",
        "配置文件不存在",
        "可只用环境变量运行；需要持久配置时创建 <data-dir>/config.json",
    ),
    code(
        "E1002",
        "config",
        "配置文件无效",
        "按错误提示修正 JSON 或取值；修好前 cove 拒绝启动",
    ),
    code(
        "E1003",
        "config",
        "未设置模型",
        "设置 --model、COVE_MODEL 或配置里的 \"model\"",
    ),
    code(
        "E1005",
        "config",
        "API Key 未设置",
        "设置 COVE_API_KEY、供应商环境变量或 provider.api_key",
    ),
    code(
        "E1006",
        "config",
        "权限模式无效",
        "有效值: default, plan, auto, bypass（或 --permission ask|readonly|auto）",
    ),
    code(
        "E1007",
        "config",
        "API Key 是占位符",
        "把示例值换成真实密钥，或删除该字段改用环境变量",
    ),
    code(
        "E2001",
        "network",
        "无法连接 API",
        "检查 base_url、代理、证书与网络；/doctor 会测试连通性",
    ),
    code(
        "E2002",
        "network",
        "API 请求超时",
        "稍后重试；网络慢时检查代理，或换更近的端点",
    ),
    code(
        "E2003",
        "api",
        "触发限流 (HTTP 429)",
        "稍候再试，或降低并发/换用额度更高的密钥",
    ),
    code(
        "E2004",
        "api",
        "认证失败 (HTTP 401/403)",
        "检查 API Key 是否属于当前 provider/base_url，是否过期",
    ),
    code(
        "E2005",
        "api",
        "请求被拒绝 (HTTP 400/404/422)",
        "检查模型名与 base_url；协议不匹配时换用正确的 provider",
    ),
    code(
        "E2006",
        "api",
        "服务端错误 (HTTP 5xx)",
        "服务端故障，稍后重试或换用其他 provider",
    ),
    code(
        "E2007",
        "api",
        "响应流中断或不完整",
        "重试；/continue 不会重放已完成的工具调用",
    ),
    code(
        "E2008",
        "api",
        "超出模型上下文长度",
        "执行 /compact，或用 --context-window 设置正确的窗口",
    ),
    code(
        "E3001",
        "permission",
        "操作未获授权",
        "在询问时批准，或用 /mode 选择合适的权限模式",
    ),
    code(
        "E3002",
        "permission",
        "非交互模式无法询问授权",
        "无人值守时显式使用 --permission auto（会执行写入与 shell）",
    ),
    code(
        "E3004",
        "permission",
        "策略/扩展配置加载失败",
        "按错误修正 policies、hooks 或 MCP 配置",
    ),
    code("E4002", "tool", "工具执行超时", "缩小命令范围或调高超时"),
    code("E4004", "tool", "工具执行失败", "查看 /expand 中的完整输出"),
    code(
        "E4005",
        "tool",
        "找不到 shell",
        "Windows 需要 powershell.exe，其他系统需要 sh",
    ),
    code(
        "E4008",
        "tool",
        "找不到 git",
        "安装 git 并加入 PATH；/commit、/diff git 需要它",
    ),
    code(
        "E5001",
        "engine",
        "达到模型调用上限",
        "/continue 继续；或调高 --max-turns / max_iterations",
    ),
    code(
        "E5002",
        "engine",
        "任务被取消",
        "已完成的文件或命令效果没有回滚；用 /diff、/undo 检查",
    ),
    code(
        "E5003",
        "engine",
        "上下文压缩失败",
        "原上下文保留；/new 开新会话或稍后再 /compact",
    ),
    code(
        "E5004",
        "engine",
        "预算用尽",
        "调高 --budget / max_budget_usd，或开新会话",
    ),
    code(
        "E5007",
        "engine",
        "任务超过时限",
        "调高 --timeout / max_turn_minutes，或把任务拆小",
    ),
    code(
        "E6001",
        "session",
        "会话或检查点无效",
        "会话文件损坏或与当前工作区/模型不匹配；/new 开新会话",
    ),
    code(
        "E6002",
        "session",
        "会话保存失败",
        "检查数据目录权限与磁盘空间",
    ),
    code(
        "E6003",
        "filesystem",
        "文件权限不足",
        "检查数据目录与工作区的读写权限",
    ),
    code("E6004", "filesystem", "磁盘空间不足", "清理磁盘后重试"),
];

pub fn lookup(code: &str) -> Option<&'static Code> {
    CODES.iter().find(|entry| entry.code == code)
}

/// Best-effort code for an error message; "" when none fits.
pub fn classify(message: &str) -> &'static str {
    let text = message.to_ascii_lowercase();
    let has = |needles: &[&str]| needles.iter().any(|needle| text.contains(needle));
    if has(&["http 401", "http 403", "unauthorized", "invalid api key"]) {
        "E2004"
    } else if has(&["http 429", "rate limit"]) {
        "E2003"
    } else if has(&[
        "context length",
        "context_length",
        "maximum context",
        "too many tokens",
    ]) {
        "E2008"
    } else if has(&["http 5"]) {
        "E2006"
    } else if has(&["http 400", "http 404", "http 422"]) {
        "E2005"
    } else if has(&["http request failed", "connection failed", "connect"]) {
        "E2001"
    } else if has(&[
        "stream idle timeout",
        "truncated",
        "stream without",
        "incomplete",
    ]) {
        "E2007"
    } else if has(&["task deadline exceeded", "任务超过时限"]) {
        "E5007"
    } else if has(&["model calls used", "model call window", "模型调用"]) {
        "E5001"
    } else if has(&["budget", "预算"]) {
        "E5004"
    } else if has(&["compact", "summary", "压缩", "摘要"]) {
        "E5003"
    } else if has(&[
        "non-interactive mode cannot ask",
        "non-interactive",
        "无法询问",
    ]) {
        "E3002"
    } else if has(&["approval required or denied", "需要批准或已被拒绝"]) {
        "E3001"
    } else if has(&["cancelled", "已取消"]) {
        "E5002"
    } else if has(&["timed out", "timeout"]) {
        "E2002"
    } else if has(&["no space left", "disk full"]) {
        "E6004"
    } else if has(&["permission denied", "access is denied"]) {
        "E6003"
    } else if has(&[
        "do not match this invocation",
        "属于另一个工作区",
        "invalid id",
        "checkpoint identity",
    ]) {
        "E6001"
    } else if has(&["session:", "会话："]) {
        "E6002"
    } else {
        ""
    }
}

/// Shared handle on both logs.
pub struct Diagnostics {
    pub trace: JsonLog,
    pub errors: JsonLog,
    secrets: Vec<String>,
}

impl Diagnostics {
    pub fn new(data_dir: &Path) -> Self {
        Self {
            trace: JsonLog::new(data_dir.join(TRACE_FILE), TRACE_LIMIT),
            errors: JsonLog::new(data_dir.join(ERROR_FILE), ERROR_LIMIT),
            secrets: Vec::new(),
        }
    }

    /// Strings never written to either log (the API key).
    pub fn with_secret(mut self, secret: &str) -> Self {
        if secret.len() >= 4 {
            self.secrets.push(secret.to_owned());
        }
        self
    }

    fn scrub(&self, text: &str, limit: usize) -> String {
        let mut text = text.to_owned();
        for secret in &self.secrets {
            text = text.replace(secret.as_str(), "****");
        }
        clip(&text, limit)
    }

    /// Records a failure in `errors.log` with its diagnostic code.
    pub fn error(&self, source: &str, message: &str) {
        let code = classify(message);
        self.errors.write(
            "error",
            json!({"source": source, "code": code, "message": self.scrub(message, MESSAGE_LIMIT)}),
        );
    }
}

/// Provider decorator writing one trace line per model call (and per compaction).
pub struct Traced {
    pub inner: Arc<dyn Provider>,
    pub model: String,
    pub diagnostics: Arc<Diagnostics>,
}

impl Provider for Traced {
    fn stream(
        &self,
        request: ModelRequest,
        events: mpsc::Sender<Event>,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ModelResponse>> {
        Box::pin(async move {
            let compaction = request.tools.is_empty()
                && request
                    .system
                    .starts_with(crate::engine::agent::COMPACT_SYSTEM_PREFIX);
            let messages = request.messages.len();
            let est_tokens = crate::engine::context::tokens(&request.messages);
            let tools = request.tools.len();
            let started = Instant::now();
            let result = self.inner.stream(request, events, cancel.clone()).await;
            let ms = started.elapsed().as_millis() as u64;
            let mut fields = json!({
                "model": self.model, "messages": messages, "est_tokens": est_tokens,
                "tools": tools, "ms": ms,
            });
            match &result {
                Ok(response) => {
                    let (calls, text) =
                        response
                            .message
                            .blocks
                            .iter()
                            .fold((0, 0), |(calls, text), block| match block {
                                Block::ToolUse { .. } => (calls + 1, text),
                                Block::Text(body) => (calls, text + body.len()),
                                _ => (calls, text),
                            });
                    let extra = json!({
                        "complete": response.complete, "in": response.usage.input,
                        "out": response.usage.output, "tool_calls": calls, "content_bytes": text,
                    });
                    merge(&mut fields, extra);
                }
                Err(err) => {
                    let message = err.to_string();
                    let extra = json!({
                        "error": self.diagnostics.scrub(&message, HEAD_LIMIT),
                        "error_kind": classify(&message),
                        "cancelled": cancel.is_cancelled(),
                    });
                    merge(&mut fields, extra);
                    if !cancel.is_cancelled() {
                        self.diagnostics.error("model", &message);
                    }
                }
            }
            let kind = if compaction { "compact" } else { "model" };
            self.diagnostics.trace.write(kind, fields);
            result
        })
    }
}

fn merge(target: &mut Value, extra: Value) {
    if let (Value::Object(target), Value::Object(extra)) = (target, extra) {
        target.extend(extra);
    }
}

/// Turn hook writing tool and task trace lines and recording failed tasks.
pub struct TraceHook {
    diagnostics: Arc<Diagnostics>,
    tool_started: Mutex<Option<Instant>>,
    task_started: Mutex<Option<Instant>>,
}

impl TraceHook {
    pub fn new(diagnostics: Arc<Diagnostics>) -> Self {
        Self {
            diagnostics,
            tool_started: Mutex::new(None),
            task_started: Mutex::new(None),
        }
    }

    fn elapsed(slot: &Mutex<Option<Instant>>) -> Option<u64> {
        slot.lock()
            .ok()?
            .take()
            .map(|started| started.elapsed().as_millis() as u64)
    }
}

impl TurnHook for TraceHook {
    fn task_note<'a>(&'a self, _session: &'a Session) -> BoxFuture<'a, Option<String>> {
        if let Ok(mut slot) = self.task_started.lock() {
            *slot = Some(Instant::now());
        }
        Box::pin(async { None })
    }

    fn before_tool(&self, _name: &str, _input: &Value) -> Option<String> {
        if let Ok(mut slot) = self.tool_started.lock() {
            *slot = Some(Instant::now());
        }
        None
    }

    fn tool_note(&self, name: &str, _input: &Value, output: &ToolOutput) -> Option<String> {
        let mut fields = json!({
            "name": name,
            "ms": Self::elapsed(&self.tool_started).unwrap_or(0),
            "result_bytes": output.content.len(),
            "error": output.failed,
        });
        if output.failed {
            let head = output.content.lines().next().unwrap_or_default();
            merge(
                &mut fields,
                json!({"head": self.diagnostics.scrub(head, HEAD_LIMIT)}),
            );
        }
        self.diagnostics.trace.write("tool", fields);
        None
    }

    fn task_end<'a>(&'a self, session: &'a Session, completed: bool) -> BoxFuture<'a, ()> {
        Box::pin(async move {
            let reason = match &session.state {
                State::Interrupted(reason) if !completed => Some(reason.clone()),
                _ => None,
            };
            let mut fields = json!({
                "completed": completed,
                "messages": session.messages.len(),
                "est_tokens": crate::engine::context::tokens(&session.messages),
                "failures": session.task_failures,
                "model_calls": session.accounting.model_calls,
            });
            if let Some(ms) = Self::elapsed(&self.task_started) {
                merge(&mut fields, json!({"ms": ms}));
            }
            if let Some(reason) = &reason {
                merge(&mut fields, json!({"error_kind": classify(reason)}));
                self.diagnostics.error("task", reason);
            }
            self.diagnostics.trace.write("task", fields);
        })
    }
}

fn number(value: &Value, key: &str) -> u64 {
    value.get(key).and_then(Value::as_u64).unwrap_or(0)
}

fn text<'a>(value: &'a Value, key: &str) -> &'a str {
    value.get(key).and_then(Value::as_str).unwrap_or_default()
}

fn time_of(value: &Value) -> String {
    let stamp = text(value, "t");
    chrono::DateTime::parse_from_rfc3339(stamp)
        .map(|time| time.format("%m-%d %H:%M:%S").to_string())
        .unwrap_or_else(|_| stamp.to_owned())
}

pub fn duration(ms: u64) -> String {
    match ms {
        60_000.. => format!("{}m{:02}s", ms / 60_000, (ms % 60_000) / 1000),
        1000.. => format!("{:.1}s", ms as f64 / 1000.0),
        _ => format!("{ms}ms"),
    }
}

pub fn bytes(n: u64) -> String {
    match n {
        1_048_576.. => format!("{:.1} MB", n as f64 / 1_048_576.0),
        1024.. => format!("{:.1} KB", n as f64 / 1024.0),
        _ => format!("{n} B"),
    }
}

/// One trace entry as a compact display line.
pub fn format_trace(entry: &Value) -> String {
    let time = time_of(entry);
    let ms = duration(number(entry, "ms"));
    match text(entry, "kind") {
        kind @ ("model" | "compact") => {
            let line = format!(
                "{time} {kind:<7} {}  {} 条消息 ~{} tok  {ms}",
                text(entry, "model"),
                number(entry, "messages"),
                number(entry, "est_tokens")
            );
            if entry.get("error").is_some() {
                let kind = if entry.get("cancelled") == Some(&Value::Bool(true)) {
                    "已取消"
                } else {
                    text(entry, "error_kind")
                };
                format!("{line}  失败 {kind} {}", text(entry, "error"))
            } else {
                format!(
                    "{line}  -> 输入 {} 输出 {}  {} 次工具调用  文本 {}{}",
                    number(entry, "in"),
                    number(entry, "out"),
                    number(entry, "tool_calls"),
                    bytes(number(entry, "content_bytes")),
                    if entry.get("complete") == Some(&Value::Bool(false)) {
                        "  (被截断)"
                    } else {
                        ""
                    }
                )
            }
        }
        "tool" => {
            let line = format!(
                "{time} tool    {}  {ms}  {}",
                text(entry, "name"),
                bytes(number(entry, "result_bytes"))
            );
            if entry.get("error") == Some(&Value::Bool(true)) {
                format!("{line}  失败 {}", text(entry, "head"))
            } else {
                line
            }
        }
        "task" => format!(
            "{time} task    {}  {} 条消息 ~{} tok  {} 个工具失败{}",
            if entry.get("completed") == Some(&Value::Bool(true)) {
                "完成"
            } else {
                "中止"
            },
            number(entry, "messages"),
            number(entry, "est_tokens"),
            number(entry, "failures"),
            entry
                .get("ms")
                .and_then(Value::as_u64)
                .map(|ms| format!("  {}", duration(ms)))
                .unwrap_or_default()
        ),
        other => format!("{time} {other} {entry}"),
    }
}

/// `/doctor trace [N]`.
pub fn trace_report(diagnostics: &Diagnostics, n: usize) -> String {
    let entries = diagnostics.trace.tail(n);
    if entries.is_empty() {
        return format!(
            "还没有跟踪记录（{}）；运行一个任务后再查看 /doctor trace。\n",
            diagnostics.trace.path().display()
        );
    }
    let mut out = format!(
        "跟踪记录：最近 {} 条（{}）；只记录大小与耗时，不含消息内容。\n",
        entries.len(),
        diagnostics.trace.path().display()
    );
    for entry in &entries {
        out.push_str(&format_trace(entry));
        out.push('\n');
    }
    out
}

/// `/doctor errors`: recorded failures grouped by code and message.
pub fn errors_report(diagnostics: &Diagnostics) -> String {
    let entries = diagnostics.errors.tail(2000);
    if entries.is_empty() {
        return format!(
            "没有记录到运行错误（{}）。\n",
            diagnostics.errors.path().display()
        );
    }
    // (code, message) -> (count, last time, source)
    let mut groups: Vec<(String, String, usize, String, String)> = Vec::new();
    for entry in &entries {
        let code = text(entry, "code").to_owned();
        let message = text(entry, "message").to_owned();
        let time = time_of(entry);
        let source = text(entry, "source").to_owned();
        match groups
            .iter_mut()
            .find(|group| group.0 == code && group.1 == message)
        {
            Some(group) => {
                group.2 += 1;
                group.3 = time;
            }
            None => groups.push((code, message, 1, time, source)),
        }
    }
    groups.sort_by(|a, b| b.3.cmp(&a.3));
    let mut out = format!(
        "运行错误：共 {} 条，{} 类，最新的在前（{}）。\n",
        entries.len(),
        groups.len(),
        diagnostics.errors.path().display()
    );
    for (code, message, count, time, source) in groups.iter().take(30) {
        out.push_str(&format!(
            "  {time} [{source}] {}{}{}\n",
            if code.is_empty() {
                String::new()
            } else {
                format!("{code} ")
            },
            message,
            if *count > 1 {
                format!("  x{count}")
            } else {
                String::new()
            }
        ));
        if let Some(entry) = lookup(code) {
            out.push_str(&format!("      {}: {}\n", entry.message, entry.recovery));
        }
    }
    out
}

/// `/doctor codes`.
pub fn codes_report() -> String {
    let mut out = String::from("诊断代码：\n");
    let mut category = "";
    for entry in CODES {
        if entry.category != category {
            category = entry.category;
            out.push_str(&format!("  [{category}]\n"));
        }
        out.push_str(&format!(
            "    {}  {} - {}\n",
            entry.code, entry.message, entry.recovery
        ));
    }
    out
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Status {
    Ok,
    Warn,
    Fail,
    Skip,
}

pub struct Check {
    pub status: Status,
    pub name: &'static str,
    pub detail: String,
    pub code: &'static str,
}

fn check(status: Status, name: &'static str, detail: impl Into<String>) -> Check {
    Check {
        status,
        name,
        detail: detail.into(),
        code: "",
    }
}

impl Check {
    fn code(mut self, code: &'static str) -> Self {
        self.code = code;
        self
    }
}

/// What `/doctor` and `--doctor` know about the run.
pub struct DoctorInput<'a> {
    pub data_dir: &'a Path,
    pub config_path: Option<PathBuf>,
    pub settings: Option<&'a Settings>,
    /// Why the settings could not be loaded (`--doctor` only).
    pub load_error: Option<String>,
    /// Extension catalogue summary or its load error.
    pub extensions: std::result::Result<String, String>,
    pub quick: bool,
}

/// Finds an executable on PATH (with Windows extensions).
pub fn which(name: &str) -> Option<PathBuf> {
    let path = std::env::var_os("PATH")?;
    let extensions: Vec<String> = if cfg!(windows) {
        std::env::var("PATHEXT")
            .unwrap_or_else(|_| ".EXE;.CMD;.BAT;.COM".into())
            .split(';')
            .map(str::to_owned)
            .chain([String::new()])
            .collect()
    } else {
        vec![String::new()]
    };
    std::env::split_paths(&path).find_map(|dir| {
        extensions.iter().find_map(|extension| {
            let candidate = dir.join(format!("{name}{extension}"));
            candidate.is_file().then_some(candidate)
        })
    })
}

fn version(program: &str) -> Option<String> {
    let output = std::process::Command::new(program)
        .arg("--version")
        .stdin(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .output()
        .ok()?;
    output.status.success().then(|| {
        String::from_utf8_lossy(&output.stdout)
            .lines()
            .next()
            .unwrap_or_default()
            .trim()
            .to_owned()
    })
}

fn config_checks(input: &DoctorInput<'_>, checks: &mut Vec<Check>) {
    match (&input.config_path, &input.load_error) {
        (_, Some(error)) => {
            let code = match classify_setup(error) {
                "" => "E1002",
                code => code,
            };
            checks.push(check(Status::Fail, "config", clip(error, MESSAGE_LIMIT)).code(code));
        }
        (Some(path), None) => checks.push(check(
            Status::Ok,
            "config",
            format!("已加载 {}", path.display()),
        )),
        (None, None) => checks.push(
            check(
                Status::Warn,
                "config",
                format!(
                    "没有配置文件（{}）；当前使用命令行参数和环境变量",
                    input.data_dir.join("config.json").display()
                ),
            )
            .code("E1001"),
        ),
    }
    if let Some(settings) = input.settings {
        for warning in &settings.warnings {
            checks.push(check(Status::Warn, "config", warning.clone()));
        }
    }
    // Placeholder keys in the file, also when loading failed.
    let raw: Option<Value> = match input.settings {
        Some(settings) => Some(settings.file.clone()),
        None => input
            .config_path
            .as_ref()
            .and_then(|path| std::fs::read(path).ok())
            .and_then(|data| {
                let data = data.strip_prefix(b"\xEF\xBB\xBF").unwrap_or(&data).to_vec();
                serde_json::from_slice(&data).ok()
            }),
    };
    if let Some(raw) = raw {
        let mut keys = vec![raw.pointer("/provider/api_key").cloned()];
        if let Some(profiles) = raw.get("profiles").and_then(Value::as_object) {
            keys.extend(
                profiles
                    .values()
                    .map(|profile| profile.pointer("/provider/api_key").cloned()),
            );
        }
        if keys
            .iter()
            .flatten()
            .filter_map(Value::as_str)
            .any(placeholder_key)
        {
            checks.push(
                check(
                    Status::Warn,
                    "api_key",
                    "配置里的 provider.api_key 是占位符或打码值（不会发送）",
                )
                .code("E1007"),
            );
        }
    }
}

fn classify_setup(error: &str) -> &'static str {
    let text = error.to_ascii_lowercase();
    if text.contains("api_key") || text.contains("api key") {
        "E1005"
    } else if text.contains("--model") {
        "E1003"
    } else if text.contains("permission_mode") {
        "E1006"
    } else {
        ""
    }
}

async fn network_check(settings: &Settings) -> Check {
    let client = match crate::model::net::apply(reqwest::Client::builder())
        .redirect(reqwest::redirect::Policy::none())
        .connect_timeout(Duration::from_secs(5))
        .timeout(Duration::from_secs(8))
        .build()
    {
        Ok(client) => client,
        Err(err) => return check(Status::Fail, "network", err.to_string()),
    };
    let started = Instant::now();
    match client.head(&settings.base_url).send().await {
        // Any HTTP answer (even 401/404) proves the endpoint is reachable.
        Ok(response) => check(
            Status::Ok,
            "network",
            format!(
                "{} 已响应 HTTP {}，耗时 {}",
                settings.base_url,
                response.status().as_u16(),
                duration(started.elapsed().as_millis() as u64)
            ),
        ),
        Err(err) if err.is_timeout() => check(
            Status::Fail,
            "network",
            format!("{} 连接超时", settings.base_url),
        )
        .code("E2002"),
        Err(_) => check(
            Status::Fail,
            "network",
            format!(
                "无法连接 {}（DNS、代理、证书或网络问题）",
                settings.base_url
            ),
        )
        .code("E2001"),
    }
}

fn data_dir_check(data_dir: &Path, workspace: Option<&Path>) -> Check {
    if !data_dir.is_dir() {
        return check(
            Status::Warn,
            "data_dir",
            format!("{} 尚不存在（首次启动时会自动创建）", data_dir.display()),
        );
    }
    let probe = tempfile::NamedTempFile::new_in(data_dir);
    if probe.is_err() {
        return check(
            Status::Fail,
            "data_dir",
            format!("{} 不可写", data_dir.display()),
        )
        .code("E6003");
    }
    if let (Ok(data), Some(workspace)) = (data_dir.canonicalize(), workspace)
        && data.starts_with(workspace)
    {
        return check(
            Status::Fail,
            "data_dir",
            "数据目录位于工作区内；请用 --data-dir 指定工作区之外的目录",
        );
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        if std::fs::metadata(data_dir).is_ok_and(|meta| meta.permissions().mode() & 0o077 != 0) {
            return check(
                Status::Warn,
                "data_dir",
                format!("{} 其他用户可以访问；请执行 chmod 700", data_dir.display()),
            );
        }
    }
    check(
        Status::Ok,
        "data_dir",
        format!("{} 可写", data_dir.display()),
    )
}

fn policy_check(data_dir: &Path) -> Option<Check> {
    let path = data_dir.join("policies.json");
    let data = std::fs::read(&path).ok()?;
    Some(match serde_json::from_slice::<Value>(&data) {
        Ok(_) => check(
            Status::Ok,
            "policies",
            format!("{} 解析正常", path.display()),
        ),
        Err(err) => check(
            Status::Fail,
            "policies",
            format!(
                "{}：{err}；其中的规则未生效，修正 JSON 后重启",
                path.display()
            ),
        )
        .code("E3004"),
    })
}

/// Runs the checks; the network check is skipped when `quick`.
pub async fn doctor(input: DoctorInput<'_>) -> Vec<Check> {
    let mut checks = Vec::new();
    config_checks(&input, &mut checks);
    match input.settings {
        Some(settings) => {
            checks.push(if placeholder_key(&settings.api_key) {
                check(
                    Status::Fail,
                    "api_key",
                    "当前使用的 Key 是占位符，请求会被拒绝",
                )
                .code("E1007")
            } else {
                check(
                    Status::Ok,
                    "api_key",
                    format!("已设置（{}）", redact_key(&settings.api_key)),
                )
            });
            checks.push(check(
                Status::Ok,
                "model",
                format!(
                    "provider={} protocol={:?} model={} base_url={} window={} profile={} permission={:?}",
                    settings.provider,
                    settings.protocol,
                    settings.model,
                    settings.base_url,
                    settings.context_window,
                    settings.profile.as_deref().unwrap_or("-"),
                    settings.permission
                ),
            ));
            if input.quick {
                checks.push(check(Status::Skip, "network", "已跳过（快速模式）"));
            } else {
                checks.push(network_check(settings).await);
            }
        }
        None => checks.push(check(
            Status::Skip,
            "model",
            "设置未能加载；未检查模型、Key 和网络",
        )),
    }
    checks.push(match version("git") {
        Some(version) => check(Status::Ok, "git", version),
        None => check(
            Status::Warn,
            "git",
            "找不到 git；/commit、/diff git 和 git 上下文不可用",
        )
        .code("E4008"),
    });
    checks.push(match which("rg") {
        Some(path) => check(Status::Ok, "ripgrep", path.display().to_string()),
        None => check(
            Status::Warn,
            "ripgrep",
            "PATH 中没有 rg（内置搜索仍可用；调用 rg 的 shell 命令会失败）",
        ),
    });
    let shell = if cfg!(windows) {
        "powershell.exe"
    } else {
        "sh"
    };
    checks.push(match which(shell.trim_end_matches(".exe")) {
        Some(path) => check(Status::Ok, "shell", format!("{shell}：{}", path.display())),
        None => check(Status::Fail, "shell", format!("找不到 {shell}")).code("E4005"),
    });
    checks.push(data_dir_check(
        input.data_dir,
        input.settings.map(|settings| settings.cwd.as_path()),
    ));
    checks.extend(policy_check(input.data_dir));
    checks.push(match &input.extensions {
        Ok(summary) => check(Status::Ok, "extensions", summary.clone()),
        Err(error) => check(Status::Fail, "extensions", error.clone()).code("E3004"),
    });
    checks
}

pub fn catalog_summary(catalog: &crate::extensions::Catalog, trust: bool) -> String {
    format!(
        "已加载 {} 个技能目录、{} 个 MCP 服务器、{} 个钩子、{} 个插件；trust_extensions={trust}",
        catalog.skills.len(),
        catalog.servers.len(),
        catalog.hooks.len(),
        catalog.plugins.len()
    )
}

/// `--doctor`: runs without a working configuration (that is what it diagnoses).
/// Returns false when a check failed.
pub async fn cli_doctor(cli: crate::config::Cli) -> anyhow::Result<bool> {
    let data_dir = crate::config::data_dir(&cli)?;
    let config_path = cli
        .config
        .clone()
        .or_else(|| Some(data_dir.join("config.json")).filter(|path| path.is_file()));
    let (settings, load_error) = match Settings::load(cli) {
        Ok(settings) => (Some(settings), None),
        Err(err) => (None, Some(format!("{err:#}"))),
    };
    let extensions = match &settings {
        Some(settings) => crate::extensions::Registry::open(&data_dir)
            .and_then(|registry| {
                crate::extensions::Catalog::load(
                    settings.extensions.clone(),
                    &settings.extension_base,
                    &registry,
                )
            })
            .map(|catalog| catalog_summary(&catalog, settings.trust_extensions))
            .map_err(|err| err.to_string()),
        None => Ok("未检查（设置未能加载）".into()),
    };
    let diagnostics = Diagnostics::new(&data_dir);
    let checks = doctor(DoctorInput {
        data_dir: &data_dir,
        config_path,
        settings: settings.as_ref(),
        load_error,
        extensions,
        quick: false,
    })
    .await;
    print!(
        "{}",
        crate::app::ui::visible(&format_checks(&checks, Some(&diagnostics)))
    );
    Ok(checks.iter().all(|item| item.status != Status::Fail))
}

/// Plain-text report: one line per check, hints for failures, the last errors.
pub fn format_checks(checks: &[Check], diagnostics: Option<&Diagnostics>) -> String {
    let mut out = String::from("cove 诊断\n");
    for item in checks {
        let tag = match item.status {
            Status::Ok => "ok",
            Status::Warn => "warn",
            Status::Fail => "FAIL",
            Status::Skip => "skip",
        };
        out.push_str(&format!(
            "  [{tag:<4}] {:<10} {}{}\n",
            item.name,
            if item.code.is_empty() {
                String::new()
            } else {
                format!("{} ", item.code)
            },
            item.detail
        ));
        if matches!(item.status, Status::Warn | Status::Fail)
            && let Some(entry) = lookup(item.code)
        {
            out.push_str(&format!("               建议：{}\n", entry.recovery));
        }
    }
    let failed = checks
        .iter()
        .filter(|item| item.status == Status::Fail)
        .count();
    out.push_str(&format!("共 {} 项检查，{failed} 项失败。\n", checks.len()));
    if let Some(diagnostics) = diagnostics {
        let recent = diagnostics.errors.tail(5);
        if !recent.is_empty() {
            out.push_str("最近的错误（/doctor errors 查看全部）：\n");
            for entry in &recent {
                out.push_str(&format!(
                    "  {} {} {}\n",
                    time_of(entry),
                    text(entry, "code"),
                    text(entry, "message")
                ));
            }
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn logs_rotate_once_and_tail_spans_both_files() {
        let dir = tempfile::tempdir().unwrap();
        let log = JsonLog::new(dir.path().join("trace.jsonl"), 200);
        for index in 0..10 {
            log.write("tool", json!({"name": format!("t{index}")}));
        }
        assert!(dir.path().join("trace.jsonl.1").is_file());
        let all = std::fs::metadata(dir.path().join("trace.jsonl"))
            .unwrap()
            .len();
        assert!(all <= 200);
        let tail = log.tail(3);
        assert_eq!(tail.len(), 3);
        assert_eq!(tail[2]["name"], "t9");
        assert_eq!(tail[0]["name"], "t7");
        assert!(log.tail(100).len() < 10, "only one rotation is kept");
    }

    #[test]
    fn classification_and_placeholders() {
        assert_eq!(
            classify("provider: HTTP 401 Unauthorized: bad key"),
            "E2004"
        );
        assert_eq!(classify("provider: HTTP 429 Too Many Requests"), "E2003");
        assert_eq!(classify("provider: HTTP 503 Service Unavailable"), "E2006");
        assert_eq!(
            classify("provider: HTTP request failed (check endpoint, proxy)"),
            "E2001"
        );
        assert_eq!(classify("limit reached: task deadline exceeded"), "E5007");
        assert_eq!(classify("something odd"), "");
        assert!(placeholder_key("sk-xxxxxxxx"));
        assert!(placeholder_key("sk-1****abcd"));
        assert!(!placeholder_key("sk-real1234567890"));
        assert_eq!(redact_key("sk-abcdefghijklmnop"), "sk-a****mnop");
        assert_eq!(redact_key("short"), "****");
        assert!(CODES.iter().all(|entry| lookup(entry.code).is_some()));
    }

    #[test]
    fn errors_are_redacted_bounded_and_grouped() {
        let dir = tempfile::tempdir().unwrap();
        let diagnostics = Diagnostics::new(dir.path()).with_secret("secret-key-123");
        diagnostics.error("model", "HTTP 401 Unauthorized: secret-key-123 rejected");
        diagnostics.error("model", "HTTP 401 Unauthorized: secret-key-123 rejected");
        diagnostics.error("task", &"x".repeat(2000));
        let raw = std::fs::read_to_string(dir.path().join(ERROR_FILE)).unwrap();
        assert!(!raw.contains("secret-key-123"));
        assert!(raw.lines().all(|line| line.len() < 1000));
        let report = errors_report(&diagnostics);
        assert!(
            report.contains("E2004") && report.contains("x2"),
            "{report}"
        );
        assert!(report.contains("认证失败"));
    }

    #[test]
    fn trace_lines_format_without_text() {
        let line = format_trace(&json!({"t":"2026-10-27T10:00:00.000+08:00","kind":"model",
            "model":"m","messages":3,"est_tokens":900,"ms":1500,"in":10,"out":5,"tool_calls":1,"content_bytes":20}));
        assert!(line.contains("model") && line.contains("1.5s") && line.contains("1 次工具调用"));
        let line = format_trace(
            &json!({"kind":"tool","name":"shell","ms":5,"result_bytes":2048,"error":true,"head":"exit 1"}),
        );
        assert!(line.contains("2.0 KB") && line.contains("失败 exit 1"));
    }
}
