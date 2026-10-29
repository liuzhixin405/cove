use std::{
    collections::HashSet,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::Duration,
};

use futures_util::future::BoxFuture;
use regex::Regex;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Result},
    extensions::{HookConfig, HookEvent, Registry, Sourced, bounded},
    process,
    safety::permission::{Authorizer, Effect},
    session::workspace::Workspace,
    session::{Session, SessionPersistence, new_id},
};

#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
pub enum HookState {
    Running,
    Completed,
    Uncertain,
    Acknowledged,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct HookRun {
    pub id: String,
    pub hook: String,
    pub event: HookEvent,
    pub state: HookState,
    pub outcome: String,
    #[serde(default)]
    pub tool_call: Option<String>,
    /// A `hooks.json` PreToolUse hook refused this tool call; the reason the model sees.
    /// Journaled, so a resumed task never runs a call its hook blocked.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub blocked: Option<String>,
}

/// The reason a `BeforeTool` hook blocked tool call `call_id`, if one did.
pub fn blocked(session: &Session, call_id: &str) -> Option<String> {
    session
        .hook_runs
        .iter()
        .filter(|run| {
            run.event == HookEvent::BeforeTool && run.tool_call.as_deref() == Some(call_id)
        })
        .find_map(|run| run.blocked.clone())
}

pub trait Lifecycle: Send + Sync {
    fn run<'a>(
        &'a self,
        event: HookEvent,
        session: &'a mut Session,
        persistence: &'a dyn SessionPersistence,
        detail: Value,
        cancel: CancellationToken,
    ) -> BoxFuture<'a, Result<()>>;
}

pub struct Hooks {
    pub configs: Vec<Sourced<HookConfig>>,
    registry: Arc<Registry>,
    authorizer: Arc<dyn Authorizer>,
    workspace: Arc<Workspace>,
    root: PathBuf,
    trusted: bool,
    started: Mutex<HashSet<String>>,
}

impl Hooks {
    pub fn new(
        configs: Vec<Sourced<HookConfig>>,
        registry: Arc<Registry>,
        authorizer: Arc<dyn Authorizer>,
        workspace: Arc<Workspace>,
        root: PathBuf,
        trusted: bool,
    ) -> Self {
        Self {
            configs,
            registry,
            authorizer,
            workspace,
            root,
            trusted,
            started: Mutex::new(HashSet::new()),
        }
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct HookOutput {
    decision: Decision,
    #[serde(default)]
    message: String,
}
#[derive(Deserialize)]
#[serde(rename_all = "snake_case")]
enum Decision {
    Allow,
    Deny,
}

/// Default timeout of a `hooks.json` hook that sets none.
pub const CLAUDE_TIMEOUT: u64 = 60;
const CLAUDE_INPUT_LIMIT: usize = 30 * 1024;

/// Event names accepted in `hooks.json`, with the Claude Code spellings.
fn json_event(name: &str) -> Option<HookEvent> {
    Some(match name {
        "PreToolUse" | "BeforeTool" => HookEvent::BeforeTool,
        "PostToolUse" | "AfterTool" => HookEvent::AfterTool,
        "SessionStart" => HookEvent::SessionStart,
        "SessionEnd" => HookEvent::SessionEnd,
        _ => return None,
    })
}

fn hooks_json_event(event: HookEvent) -> (&'static str, &'static str) {
    match event {
        HookEvent::BeforeTool => ("BeforeTool", "PreToolUse"),
        HookEvent::AfterTool => ("AfterTool", "PostToolUse"),
        HookEvent::SessionStart => ("SessionStart", "SessionStart"),
        HookEvent::SessionEnd => ("SessionEnd", "SessionEnd"),
        HookEvent::TaskStart => ("TaskStart", "UserPromptSubmit"),
        HookEvent::TaskEnd => ("TaskEnd", "Stop"),
    }
}

/// A matcher names whole tools: `bash` must not also match `bash_output`. `^`/`$` are
/// taken as written; empty or `*` matches every tool.
pub fn matcher_regex(matcher: &str) -> std::result::Result<Option<Regex>, regex::Error> {
    let matcher = matcher.trim();
    if matcher.is_empty() || matcher == "*" {
        return Ok(None);
    }
    let pattern = if matcher.starts_with('^') || matcher.ends_with('$') {
        format!("(?i){matcher}")
    } else {
        format!("(?i)^(?:{matcher})$")
    };
    Regex::new(&pattern).map(Some)
}

/// Claude Code names of a cove tool, so `Bash` or `Edit|MultiEdit` matchers work.
fn aliases(tool: &str) -> &'static [&'static str] {
    match tool {
        "shell" => &["Bash", "bash", "PowerShell"],
        "edit" => &["Edit", "MultiEdit"],
        "write" => &["Write"],
        "read" => &["Read"],
        "grep" => &["Grep"],
        "glob" => &["Glob"],
        "web_fetch" => &["WebFetch"],
        "web_search" => &["WebSearch"],
        "todo" => &["TodoWrite"],
        "agent" => &["Task", "Agent"],
        "ask_user" => &["AskUserQuestion"],
        "skill" => &["Skill"],
        _ => &[],
    }
}

pub fn matches(matcher: Option<&str>, tool: &str) -> bool {
    match matcher.map(matcher_regex) {
        None | Some(Ok(None)) => true,
        Some(Ok(Some(regex))) => {
            regex.is_match(tool) || aliases(tool).iter().any(|alias| regex.is_match(alias))
        }
        Some(Err(_)) => false,
    }
}

#[derive(Deserialize)]
struct JsonHook {
    #[serde(default)]
    matcher: String,
    command: String,
    #[serde(default)]
    timeout: u64,
    #[serde(default, rename = "async")]
    run_async: bool,
}

/// Loads `<data_dir>/hooks.json`: `{"hooks": {"PreToolUse": [{matcher, command, timeout, async}]}}`.
/// Invalid entries become warnings; the valid ones still load. Only the user's own data
/// directory is read: a project hooks file would let a clone run commands.
pub fn load_hooks_json(data_dir: &Path) -> Result<(Vec<Sourced<HookConfig>>, Vec<String>)> {
    let path = data_dir.join("hooks.json");
    if !path.try_exists()? {
        return Ok((Vec::new(), Vec::new()));
    }
    let data = crate::extensions::skills::read_text(&path, 256 * 1024)?;
    #[derive(Deserialize)]
    struct File {
        #[serde(default)]
        hooks: std::collections::BTreeMap<String, Vec<Value>>,
    }
    let file: File = serde_json::from_str(data.trim_start_matches('\u{feff}'))
        .map_err(|err| Error::Tool(format!("parse {}: {err}", path.display())))?;
    let (mut hooks, mut warnings) = (Vec::new(), Vec::new());
    for (event_name, entries) in file.hooks {
        let Some(event) = json_event(&event_name) else {
            warnings.push(format!("hooks.json：未知的 hook 事件 {event_name:?}"));
            continue;
        };
        for (index, entry) in entries.into_iter().enumerate() {
            let id = format!("hooks.json/{event_name}-{index}");
            let hook: JsonHook = match serde_json::from_value(entry) {
                Ok(hook) => hook,
                Err(err) => {
                    warnings.push(format!("{id}: {err}"));
                    continue;
                }
            };
            if hook.command.trim().is_empty() || hook.command.len() > 64 * 1024 {
                warnings.push(format!("{id}：命令为空或过长"));
                continue;
            }
            if let Err(err) = matcher_regex(&hook.matcher) {
                warnings.push(format!("{id}：matcher 无效：{err}"));
                continue;
            }
            hooks.push(Sourced {
                id: id.clone(),
                plugin: None,
                value: HookConfig {
                    name: id,
                    event,
                    command: hook.command,
                    timeout_seconds: if hook.timeout == 0 {
                        CLAUDE_TIMEOUT
                    } else {
                        hook.timeout.min(600)
                    },
                    matcher: Some(hook.matcher).filter(|m| !m.trim().is_empty()),
                    run_async: hook.run_async,
                    claude: true,
                },
            });
            if hooks.len() > 64 {
                return Err(Error::Tool("hooks.json: at most 64 hooks".into()));
            }
        }
    }
    Ok((hooks, warnings))
}

/// Feeds the JSON input to the command's stdin. The scripted shell already reads the script
/// from stdin on Windows, so the input arrives through `COVE_HOOK_INPUT` and a pipeline.
fn stdin_script(command: &str) -> String {
    if cfg!(windows) {
        format!("$env:COVE_HOOK_INPUT | {command}")
    } else {
        format!("printf '%s' \"$COVE_HOOK_INPUT\" | {{\n{command}\n}}")
    }
}

/// Keeps the input under the environment-variable limit: long strings are clipped first.
fn bounded_input(mut input: Value) -> String {
    let text = input.to_string();
    if text.len() <= CLAUDE_INPUT_LIMIT {
        return text;
    }
    if let Some(Value::Object(tool_input)) = input.get_mut("tool_input") {
        for value in tool_input.values_mut() {
            if let Value::String(text) = value
                && text.len() > 2048
            {
                *text = format!("{}…[truncated]", &text[..text.floor_char_boundary(2048)]);
            }
        }
    }
    let text = input.to_string();
    if text.len() <= CLAUDE_INPUT_LIMIT {
        return text;
    }
    input["tool_input"] = json!({"truncated": true});
    input.to_string()
}

enum ClaudeResult {
    Done(String),
    Block(String),
}

/// `{"continue": false, "message"}` (any key case) or `{"decision": "block", "reason"}`
/// (Claude Code) blocks; other stdout is a message.
fn interpret(stdout: &str) -> ClaudeResult {
    let Ok(Value::Object(object)) = serde_json::from_str::<Value>(stdout.trim()) else {
        return ClaudeResult::Done(stdout.trim().to_owned());
    };
    let get = |keys: &[&str]| {
        object
            .iter()
            .find(|(key, _)| keys.iter().any(|k| key.eq_ignore_ascii_case(k)))
            .map(|(_, value)| value.clone())
    };
    let message = get(&["message", "reason", "stopReason"])
        .and_then(|v| v.as_str().map(str::to_owned))
        .unwrap_or_default();
    let stop = get(&["continue"]) == Some(Value::Bool(false))
        || get(&["decision"])
            .and_then(|v| v.as_str().map(str::to_ascii_lowercase))
            .is_some_and(|d| d == "block" || d == "deny");
    if stop {
        ClaudeResult::Block(if message.is_empty() {
            "blocked by hook".into()
        } else {
            message
        })
    } else {
        ClaudeResult::Done(message)
    }
}

impl Hooks {
    /// A `hooks.json` hook. Unlike native hooks its failures never leave the task waiting on
    /// `/config hooks resolve`: a failing PreToolUse hook (error, timeout, refusal) blocks the
    /// tool call (fail closed), any other failing hook is recorded and
    /// ignored. The journal entry is written before the command starts.
    async fn run_claude(
        &self,
        config: &Sourced<HookConfig>,
        event: HookEvent,
        session: &Session,
        detail: &Value,
        cancel: &CancellationToken,
    ) -> Result<ClaudeResult> {
        let failure = |reason: String| {
            Ok(if event == HookEvent::BeforeTool {
                ClaudeResult::Block(format!("{reason}; the tool call was not run"))
            } else {
                ClaudeResult::Done(format!("failed (ignored): {reason}"))
            })
        };
        if !self.trusted {
            return failure(format!(
                "hook {} requires --trust-extensions or /config trust",
                config.id
            ));
        }
        match self
            .authorizer
            .authorize(
                Effect::Process(format!("hook {}: {}", config.id, config.value.command)),
                cancel.clone(),
            )
            .await
        {
            Ok(()) => {}
            Err(Error::Approval(reason) | Error::Denied(reason)) => {
                return failure(format!("hook {} not authorized: {reason}", config.id));
            }
            Err(err) => return Err(err),
        }
        if session.current_checkpoint.is_some() {
            self.workspace.note_shell()?;
        }
        let (go_name, claude_name) = hooks_json_event(event);
        let tool = detail.get("name").and_then(Value::as_str);
        let tool_input = detail.get("input").cloned().or_else(|| {
            let id = detail.get("id").and_then(Value::as_str)?;
            session
                .pending
                .iter()
                .find(|call| call.id == id)
                .map(|call| call.input.clone())
        });
        let input = bounded_input(json!({"event":go_name,"hook_event_name":claude_name,
            "tool_name":tool,"tool_input":tool_input,"model":session.model,
            "session_id":session.id,"cwd":self.root}));
        let script = stdin_script(&config.value.command);
        let timeout = Duration::from_secs(config.value.timeout_seconds);
        if config.value.run_async {
            let root = self.root.clone();
            let id = config.id.clone();
            tokio::spawn(async move {
                let result = process::shell_configured(
                    &root,
                    &script,
                    timeout,
                    CancellationToken::new(),
                    Some(&input),
                )
                .await;
                if let Ok(output) = &result
                    && output.code != Some(0)
                {
                    let _ = crate::app::ui::print_text(
                        &format!(
                            "[hook {id}] 异步 hook 退出码={:?}：{}\n",
                            output.code,
                            output.stderr.trim()
                        ),
                        true,
                    );
                }
            });
            return Ok(ClaudeResult::Done(
                "started asynchronously; cannot block".into(),
            ));
        }
        let output = match process::shell_configured(
            &self.root,
            &script,
            timeout,
            cancel.clone(),
            Some(&input),
        )
        .await
        {
            Ok(output) => output,
            Err(Error::Cancelled) => return Err(Error::Cancelled),
            Err(err) => return failure(format!("hook {} could not run: {err}", config.id)),
        };
        if output.timed_out {
            return failure(format!(
                "hook {} timed out after {}s",
                config.id, config.value.timeout_seconds
            ));
        }
        match output.code {
            Some(0) => Ok(match interpret(&output.stdout) {
                ClaudeResult::Block(reason) if event != HookEvent::BeforeTool => {
                    ClaudeResult::Done(format!(
                        "asked to stop (only PreToolUse can block): {reason}"
                    ))
                }
                result => result,
            }),
            Some(2) if event == HookEvent::BeforeTool => {
                let reason = Some(output.stderr.trim().to_owned())
                    .filter(|r| !r.is_empty())
                    .or_else(|| match interpret(&output.stdout) {
                        ClaudeResult::Block(r) | ClaudeResult::Done(r) => {
                            Some(r).filter(|r| !r.is_empty())
                        }
                    })
                    .unwrap_or_else(|| "hook refused this operation with exit code 2".into());
                Ok(ClaudeResult::Block(reason))
            }
            code => match interpret(&output.stdout) {
                ClaudeResult::Block(reason) if event == HookEvent::BeforeTool => {
                    Ok(ClaudeResult::Block(reason))
                }
                _ => failure(format!(
                    "hook {} exited with {code:?}: {}",
                    config.id,
                    output.stderr.trim()
                )),
            },
        }
    }
}

impl Lifecycle for Hooks {
    fn run<'a>(
        &'a self,
        event: HookEvent,
        session: &'a mut Session,
        persistence: &'a dyn SessionPersistence,
        detail: Value,
        cancel: CancellationToken,
    ) -> BoxFuture<'a, Result<()>> {
        Box::pin(async move {
            {
                let mut started = self
                    .started
                    .lock()
                    .map_err(|_| Error::Session("hook lifecycle lock poisoned".into()))?;
                if event == HookEvent::SessionStart && !started.insert(session.id.clone()) {
                    return Ok(());
                }
                if event == HookEvent::SessionEnd && !started.remove(&session.id) {
                    return Ok(());
                }
            }
            for config in &self.configs {
                if config.value.event != event
                    || !self
                        .registry
                        .active("hook", &config.id, config.plugin.as_deref())?
                {
                    continue;
                }
                if matches!(event, HookEvent::BeforeTool | HookEvent::AfterTool)
                    && !matches(
                        config.value.matcher.as_deref(),
                        detail
                            .get("name")
                            .and_then(Value::as_str)
                            .unwrap_or_default(),
                    )
                {
                    continue;
                }
                let tool_call = detail.get("id").and_then(Value::as_str).map(str::to_owned);
                if tool_call.is_some()
                    && session.hook_runs.iter().any(|run| {
                        run.hook == config.id
                            && run.event == event
                            && run.tool_call == tool_call
                            && matches!(run.state, HookState::Completed | HookState::Acknowledged)
                    })
                {
                    continue;
                }
                if session.hook_runs.len() >= 128 {
                    let Some(index) = session.hook_runs.iter().position(|run| {
                        matches!(run.state, HookState::Completed | HookState::Acknowledged)
                            && run.tool_call.as_ref().is_none_or(|id| {
                                !session.pending.iter().any(|call| {
                                    call.id == *id
                                        && call.state != crate::session::CallState::Finished
                                })
                            })
                    }) else {
                        return Err(Error::Session(
                            "未确认的 hook 执行太多；请用 /config hooks 查看，并用 /config hooks resolve <run-id> 确认".into(),
                        ));
                    };
                    session.hook_runs.remove(index);
                }
                let index = session.hook_runs.len();
                session.hook_runs.push(HookRun {
                    id: new_id(),
                    hook: config.id.clone(),
                    event,
                    state: HookState::Running,
                    outcome: String::new(),
                    tool_call,
                    blocked: None,
                });
                persistence.save(session)?;
                if config.value.claude {
                    let result = self
                        .run_claude(config, event, session, &detail, &cancel)
                        .await;
                    let run = &mut session.hook_runs[index];
                    match &result {
                        Ok(ClaudeResult::Done(message)) => {
                            run.state = HookState::Completed;
                            run.outcome = bounded(message.clone());
                            if message.starts_with("failed (ignored)") {
                                crate::app::ui::print_text(
                                    &format!("[hook {}] {message}\n", config.id),
                                    true,
                                )?;
                            }
                        }
                        Ok(ClaudeResult::Block(reason)) => {
                            run.state = HookState::Completed;
                            run.outcome = bounded(format!("blocked: {reason}"));
                            run.blocked =
                                Some(bounded(format!("Blocked by hook {}: {reason}", config.id)));
                        }
                        // Cancelled mid-run: effects unknown, like a native hook.
                        Err(err) => {
                            run.state = HookState::Uncertain;
                            run.outcome = bounded(err.to_string());
                        }
                    }
                    persistence.save(session)?;
                    result?;
                    if session.hook_runs[index].blocked.is_some() {
                        // One veto is enough; later hooks for this call are not run.
                        return Ok(());
                    }
                    continue;
                }
                let result = async {
                    if !self.trusted { return Err(Error::Approval("hook 需要 --trust-extensions（或先用 /config trust 信任此项目并重启）".into())); }
                    self.authorizer.authorize(Effect::Process(format!("hook {}: {}",config.id,config.value.command)),cancel.clone()).await?;
                    if session.current_checkpoint.is_some() { self.workspace.note_shell()?; }
                    let input = serde_json::to_string(&json!({"version":1,"event":event,"session_id":session.id,
                        "request":session.current_request,"detail":detail}))?;
                    if input.len() > 64 * 1024 { return Err(Error::Tool("hook input exceeds 64 KiB".into())); }
                    let output = process::shell_configured(&self.root, &config.value.command,
                        Duration::from_secs(config.value.timeout_seconds), cancel.clone(), Some(&input)).await?;
                    if output.code != Some(0) || output.timed_out || output.truncated || output.encoding_warning {
                        return Err(Error::Tool(format!("hook {} failed: exit={:?}, timeout={}, truncated={}, encoding_warning={}, stderr={}",
                            config.id,output.code,output.timed_out,output.truncated,output.encoding_warning,output.stderr)));
                    }
                    if output.stdout.trim().is_empty() { return Ok(String::new()); }
                    let result: HookOutput = serde_json::from_str(output.stdout.trim())?;
                    if matches!(result.decision, Decision::Deny) { return Err(Error::Approval(format!("hook {} denied: {}",config.id,result.message))); }
                    Ok(result.message)
                }.await;
                match &result {
                    Ok(message) => {
                        session.hook_runs[index].state = HookState::Completed;
                        session.hook_runs[index].outcome = bounded(message.clone());
                    }
                    Err(err) => {
                        session.hook_runs[index].state = HookState::Uncertain;
                        session.hook_runs[index].outcome = bounded(err.to_string());
                    }
                }
                persistence.save(session).map_err(|save| {
                    Error::Uncertain(format!(
                        "hook {} 结果 {:?}；保存失败：{save}",
                        config.id,
                        result.as_ref().err()
                    ))
                })?;
                result.map_err(|err| Error::Uncertain(format!("hook {} 已停止；请检查其效果，再用 /config hooks resolve {} 确认后继续：{err}",config.id,session.hook_runs[index].id)))?;
            }
            Ok(())
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn matchers_are_anchored_case_insensitive_and_know_claude_names() {
        assert!(matches(Some("Bash"), "shell"));
        assert!(matches(Some("bash"), "shell"));
        assert!(!matches(Some("bash"), "bash_output"));
        assert!(matches(Some("Edit|Write"), "edit"));
        assert!(matches(Some("MultiEdit"), "edit"));
        assert!(!matches(Some("read|write"), "readme"));
        assert!(matches(Some("^re"), "read"));
        assert!(matches(Some("*"), "anything") && matches(None, "x"));
        assert!(matcher_regex("(").is_err());
    }

    #[test]
    fn outputs_block_in_both_json_shapes() {
        assert!(
            matches!(interpret(r#"{"Continue": false, "Message": "no"}"#), ClaudeResult::Block(m) if m == "no")
        );
        assert!(
            matches!(interpret(r#"{"decision":"block","reason":"r"}"#), ClaudeResult::Block(m) if m == "r")
        );
        assert!(matches!(
            interpret(r#"{"continue": true}"#),
            ClaudeResult::Done(_)
        ));
        assert!(matches!(interpret("plain text"), ClaudeResult::Done(m) if m == "plain text"));
    }

    #[test]
    fn hooks_json_loads_with_warnings_for_bad_entries() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(
            dir.path().join("hooks.json"),
            r#"{"hooks":{"PreToolUse":[{"matcher":"Bash","command":"echo hi"},{"matcher":"(","command":"x"}],
                "Nope":[{"command":"x"}],"PostToolUse":[{"command":"y","timeout":5,"async":true}]}}"#,
        )
        .unwrap();
        let (hooks, warnings) = load_hooks_json(dir.path()).unwrap();
        assert_eq!(hooks.len(), 2);
        assert_eq!(warnings.len(), 2);
        let pre = hooks
            .iter()
            .find(|h| h.value.event == HookEvent::BeforeTool)
            .unwrap();
        assert_eq!(pre.value.timeout_seconds, CLAUDE_TIMEOUT);
        assert!(pre.value.claude);
        assert!(
            hooks
                .iter()
                .any(|h| h.value.run_async && h.value.timeout_seconds == 5)
        );
    }

    #[test]
    fn oversized_input_is_clipped() {
        let input = bounded_input(json!({"tool_input":{"content":"x".repeat(100_000),"path":"a"}}));
        assert!(input.len() <= CLAUDE_INPUT_LIMIT && input.contains("\"path\":\"a\""));
    }
}
