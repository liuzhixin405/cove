//! Sub-agents: typed agents, their tool scopes, live activity and the `agent` tool family.
//!
//! `AgentHub` owns everything sub-agents share: the provider, a factory that builds file and
//! shell tools for a root (the main tree or a worktree), the type registry, the activity
//! board, message inboxes, regression evidence and the durable background agents
//! (`background.rs`). `WithSubAgent` exposes it to the parent model as tools. The parent
//! kernel still runs tools one at a time; concurrency (plans, teams, background agents)
//! lives inside these tools.

use std::{
    collections::{BTreeMap, HashMap},
    path::{Path, PathBuf},
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    time::Instant,
};

use futures_util::future::BoxFuture;
use serde::Deserialize;
use serde_json::{Value, json};
use tokio::sync::{Notify, mpsc};
use tokio_util::sync::CancellationToken;

use crate::{
    agents::lifecycle::{AgentStore, Lifecycle},
    agents::regression::{Regression, RegressionEvidence},
    agents::worktree::Worktrees,
    domain::{Error, Event, Progress, Provider, Result, ToolDefinition},
    engine::agent::{Agent, Ledger, Limits},
    engine::turn::TurnHook,
    safety::permission::{Authorizer, Effect},
    session::workspace::Workspace,
    session::{Accounting, Session, Store},
    tools::{ToolExecutor, ToolOutput},
};

/// Read-only investigation tools.
pub const READ_TOOLS: &[&str] = &["read", "glob", "grep", "repo_map"];
pub const WEB_TOOLS: &[&str] = &["web_fetch", "web_search"];

/// Tools of editing types: the parent's file, shell and web tools under the parent's
/// approvals. Never `agent` (no recursion), `ask_user`, `memory` or `mcp`.
pub const WRITE_TOOLS: &[&str] = &[
    "read",
    "glob",
    "grep",
    "repo_map",
    "write",
    "edit",
    "shell",
    "todo",
    "skill",
    "web_fetch",
    "web_search",
    "regression",
];

const READ_INSTRUCTIONS: &str = "You are a delegated read-only sub-agent (explore). You cannot edit files, run shell commands or ask the user. \
    Investigate only the assigned question with read/glob/grep/repo_map (and web tools when sources outside the repository matter), then answer concisely with concrete file paths, line numbers and findings. \
    State what you could not determine; do not guess.";

const PLAN_INSTRUCTIONS: &str = "You are a delegated read-only planning sub-agent. Study the relevant code and produce a concrete, ordered implementation plan: \
    files to change, the change in each, risks, and the checks that prove it works. Do not modify anything and do not ask the user.";

const REVIEW_INSTRUCTIONS: &str = "You are a delegated read-only review sub-agent. Examine the specified code for bugs, risks and inconsistencies with read/glob/grep/repo_map only. \
    Report each finding with file:line, why it is a problem and how confident you are. Do not modify anything; say so when you found nothing.";

const WRITE_INSTRUCTIONS: &str = "You are a delegated sub-agent with write access to the same workspace as the parent agent; you cannot ask the user or delegate further. \
    Make only the changes the assigned task needs, inspect files before editing, and run the relevant checks. \
    Finish with: files changed, checks run with their actual results, and anything left undone. Never claim a check passed without its successful output.";

const TEST_INSTRUCTIONS: &str = "You are a delegated testing sub-agent. Write or run tests for the specified behaviour; change production code only when the task says so. \
    Report exactly which tests ran, the commands used and their actual results. Never weaken an assertion to make a test pass.";

const VERIFY_INSTRUCTIONS: &str = "You are an independent regression verifier, not the author of the fix. Inspect the request, the production code and the tests. \
    Use the regression tool with the production files the fix changed and a focused test selector: it must observe the same tests failing on the baseline \
    and passing on the current code. Build errors, skips, absent tests and your own reasoning are not proof. Do not write files or weaken assertions. \
    If no meaningful regression test exists, report unverified. Cite the tested scope and the tool evidence; one passing regression does not prove the whole request.";

#[derive(Clone, Debug)]
pub struct AgentType {
    pub name: String,
    pub description: String,
    pub instructions: String,
    pub readonly: bool,
    pub tools: Vec<String>,
    /// A run without passing regression evidence fails (verify).
    pub requires_regression: bool,
}

fn names(list: &[&[&str]]) -> Vec<String> {
    list.iter()
        .flat_map(|names| names.iter())
        .map(|name| (*name).to_owned())
        .collect()
}

pub fn builtin_types() -> Vec<AgentType> {
    let make = |name: &str, description: &str, instructions: &str, readonly, tools| AgentType {
        name: name.into(),
        description: description.into(),
        instructions: instructions.into(),
        readonly,
        tools,
        requires_regression: name == "verify",
    };
    vec![
        make(
            "general",
            "can edit files and run shell (default)",
            WRITE_INSTRUCTIONS,
            false,
            names(&[WRITE_TOOLS]),
        ),
        make(
            "explore",
            "read-only investigation, web allowed",
            READ_INSTRUCTIONS,
            true,
            names(&[READ_TOOLS, WEB_TOOLS]),
        ),
        make(
            "plan",
            "read-only implementation plan, web allowed",
            PLAN_INSTRUCTIONS,
            true,
            names(&[READ_TOOLS, WEB_TOOLS]),
        ),
        make(
            "review",
            "read-only code review, no web",
            REVIEW_INSTRUCTIONS,
            true,
            names(&[READ_TOOLS]),
        ),
        make(
            "test",
            "writes and runs tests",
            TEST_INSTRUCTIONS,
            false,
            names(&[WRITE_TOOLS]),
        ),
        make(
            "verify",
            "read-only, no web; must produce passing regression evidence",
            VERIFY_INSTRUCTIONS,
            true,
            names(&[READ_TOOLS, &["regression"]]),
        ),
    ]
}

/// Built-in types plus `agent_types` from the config.
#[derive(Clone)]
pub struct TypeRegistry {
    types: BTreeMap<String, AgentType>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct CustomType {
    description: String,
    instructions: String,
    #[serde(default)]
    tools: Option<Vec<String>>,
    #[serde(default)]
    readonly: bool,
}

impl TypeRegistry {
    pub fn builtin() -> Self {
        Self {
            types: builtin_types()
                .into_iter()
                .map(|kind| (kind.name.clone(), kind))
                .collect(),
        }
    }

    /// `agent_types: {name: {description, instructions, tools[], readonly}}`. Read-only
    /// types may only list read, web and regression tools; built-ins cannot be replaced.
    pub fn from_config(file: &Value) -> Result<Self> {
        let mut registry = Self::builtin();
        let Some(custom) = file.get("agent_types") else {
            return Ok(registry);
        };
        let custom: BTreeMap<String, CustomType> = serde_json::from_value(custom.clone())
            .map_err(|err| Error::Session(format!("agent_types: {err}")))?;
        for (name, spec) in custom {
            if name.is_empty()
                || name.len() > 32
                || !name
                    .chars()
                    .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || matches!(c, '-' | '_'))
            {
                return Err(Error::Session(format!(
                    "agent_types: invalid name {name:?} (lowercase letters, digits, '-', '_')"
                )));
            }
            if registry.types.contains_key(&name) {
                return Err(Error::Session(format!(
                    "agent_types: {name} is built in and cannot be replaced"
                )));
            }
            if spec.instructions.trim().is_empty() || spec.instructions.len() > 16 * 1024 {
                return Err(Error::Session(format!(
                    "agent_types.{name}: instructions must be 1..16384 bytes"
                )));
            }
            let universe = if spec.readonly {
                names(&[READ_TOOLS, WEB_TOOLS, &["regression"]])
            } else {
                names(&[WRITE_TOOLS])
            };
            let tools = match spec.tools {
                None => universe
                    .iter()
                    .filter(|tool| *tool != "regression" || !spec.readonly)
                    .cloned()
                    .collect(),
                Some(tools) => {
                    if let Some(bad) = tools.iter().find(|tool| !universe.contains(tool)) {
                        return Err(Error::Session(format!(
                            "agent_types.{name}: tool {bad} is not available to {} sub-agents",
                            if spec.readonly {
                                "read-only"
                            } else {
                                "editing"
                            }
                        )));
                    }
                    tools
                }
            };
            registry.types.insert(
                name.clone(),
                AgentType {
                    name,
                    description: spec.description,
                    instructions: spec.instructions,
                    readonly: spec.readonly,
                    tools,
                    requires_regression: false,
                },
            );
        }
        Ok(registry)
    }

    pub fn get(&self, name: &str) -> Result<&AgentType> {
        self.types
            .get(&name.trim().to_ascii_lowercase())
            .ok_or_else(|| {
                Error::Tool(format!(
                    "unknown agent type {name}; available: {}",
                    self.types.keys().cloned().collect::<Vec<_>>().join(", ")
                ))
            })
    }

    pub fn summary(&self) -> String {
        self.types
            .values()
            .map(|kind| format!("{} ({})", kind.name, kind.description))
            .collect::<Vec<_>>()
            .join("; ")
    }
}

/// Where a sub-agent's file and shell tools act.
pub struct ToolScope {
    pub root: PathBuf,
    /// Checkpoint journal for edits; `None` only for read-only scopes.
    pub workspace: Option<Arc<Workspace>>,
    pub progress: Arc<Progress>,
    pub authorizer: Arc<dyn Authorizer>,
    pub readonly: bool,
}

/// Builds the file/shell/web/skill tools for a scope (assembled in `app.rs`).
pub type ToolFactory = Arc<dyn Fn(ToolScope) -> Result<Arc<dyn ToolExecutor>> + Send + Sync>;

/// Serializes approval prompts of concurrently running sub-agents on one terminal.
pub struct SerialAuthorizer {
    pub inner: Arc<dyn Authorizer>,
    pub lock: tokio::sync::Mutex<()>,
}

impl Authorizer for SerialAuthorizer {
    fn authorize(&self, effect: Effect, cancel: CancellationToken) -> BoxFuture<'_, Result<()>> {
        Box::pin(async move {
            if matches!(effect, Effect::Read) {
                return Ok(());
            }
            let _turn = tokio::select! {
                _ = cancel.cancelled() => return Err(Error::Cancelled),
                guard = self.lock.lock() => guard,
            };
            self.inner.authorize(effect, cancel).await
        })
    }
}

/// One line of `/status agents`.
#[derive(Clone, Debug)]
pub struct Activity {
    pub id: String,
    pub kind: &'static str,
    pub agent_type: String,
    pub state: Lifecycle,
    pub model: String,
    pub started: Instant,
    pub ended: Option<Instant>,
    pub task: String,
    pub last_step: String,
}

#[derive(Default)]
pub struct ActivityBoard {
    entries: Mutex<Vec<Activity>>,
}

pub fn clip(text: &str, limit: usize) -> String {
    let text = text.split_whitespace().collect::<Vec<_>>().join(" ");
    if text.len() <= limit {
        return text;
    }
    format!("{}…", &text[..text.floor_char_boundary(limit)])
}

impl ActivityBoard {
    pub fn start(&self, id: &str, kind: &'static str, agent_type: &str, model: &str, task: &str) {
        if let Ok(mut entries) = self.entries.lock() {
            entries.retain(|entry| entry.id != id);
            entries.push(Activity {
                id: id.into(),
                kind,
                agent_type: agent_type.into(),
                state: Lifecycle::Running,
                model: model.into(),
                started: Instant::now(),
                ended: None,
                task: clip(task, 120),
                last_step: "starting".into(),
            });
            // Keep the most recent finished entries only.
            while entries.len() > 64 {
                match entries.iter().position(|entry| entry.ended.is_some()) {
                    Some(index) => {
                        entries.remove(index);
                    }
                    None => break,
                }
            }
        }
    }

    pub fn step(&self, id: &str, step: &str) {
        if let Ok(mut entries) = self.entries.lock()
            && let Some(entry) = entries.iter_mut().find(|entry| entry.id == id)
        {
            entry.last_step = clip(step, 120);
        }
    }

    pub fn finish(&self, id: &str, state: Lifecycle) {
        if let Ok(mut entries) = self.entries.lock()
            && let Some(entry) = entries.iter_mut().find(|entry| entry.id == id)
        {
            entry.state = state;
            entry.ended = Some(Instant::now());
        }
    }

    pub fn get(&self, id: &str) -> Option<Activity> {
        self.entries
            .lock()
            .ok()?
            .iter()
            .find(|entry| entry.id == id)
            .cloned()
    }

    pub fn snapshot(&self) -> Vec<Activity> {
        self.entries
            .lock()
            .map(|entries| entries.clone())
            .unwrap_or_default()
    }
}

/// Messages queued for running agents, delivered at their next tool round.
#[derive(Default)]
pub struct Inbox {
    queues: Mutex<HashMap<String, Vec<String>>>,
}

impl Inbox {
    pub fn push(&self, key: &str, message: String) {
        if let Ok(mut queues) = self.queues.lock() {
            let queue = queues.entry(key.into()).or_default();
            if queue.len() < 32 {
                queue.push(message);
            }
        }
    }

    pub fn take(&self, key: &str) -> Vec<String> {
        self.queues
            .lock()
            .ok()
            .and_then(|mut queues| queues.remove(key))
            .unwrap_or_default()
    }

    pub fn pending(&self, key: &str) -> usize {
        self.queues
            .lock()
            .map_or(0, |queues| queues.get(key).map_or(0, Vec::len))
    }
}

fn messages_note(messages: Vec<String>) -> Option<String> {
    (!messages.is_empty()).then(|| {
        format!(
            "[message from your delegator (parent agent or user), delivered now]\n{}",
            messages.join("\n---\n")
        )
    })
}

/// Delivers inbox messages into a sub-agent's run: appended to its next tool result, or
/// sent as a follow-up when it was about to finish.
pub struct InboxHook {
    pub inbox: Arc<Inbox>,
    pub key: String,
}

impl TurnHook for InboxHook {
    fn tool_note(&self, _name: &str, _input: &Value, _output: &ToolOutput) -> Option<String> {
        messages_note(self.inbox.take(&self.key))
    }

    fn finish_nudge<'a>(&'a self, _session: &'a Session) -> BoxFuture<'a, Option<String>> {
        Box::pin(async move { messages_note(self.inbox.take(&self.key)) })
    }
}

/// Files written by the tasks of one parallel plan. Two running tasks never write the same
/// file at once: the second waits until the first task ends (serialized). A wait that would
/// deadlock (each waits for the other) fails the write instead.
#[derive(Default)]
pub struct Claims {
    state: Mutex<ClaimState>,
    notify: Notify,
}

#[derive(Default)]
struct ClaimState {
    owner: HashMap<String, String>,
    waits: HashMap<String, String>,
}

pub fn claim_key(path: &str) -> String {
    let mut key = path.replace('\\', "/");
    while let Some(rest) = key.strip_prefix("./") {
        key = rest.to_owned();
    }
    if cfg!(windows) {
        key = key.to_lowercase();
    }
    key
}

impl Claims {
    /// `Ok(Err(owner))` when waiting would deadlock with `owner`.
    pub async fn claim(
        &self,
        task: &str,
        key: &str,
        cancel: &CancellationToken,
        mut waiting: impl FnMut(&str),
    ) -> Result<std::result::Result<(), String>> {
        loop {
            let notified = self.notify.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            {
                let mut state = self
                    .state
                    .lock()
                    .map_err(|_| Error::Tool("claim lock poisoned".into()))?;
                let owner = match state.owner.get(key) {
                    None => None,
                    Some(owner) if owner == task => None,
                    Some(owner) => Some(owner.clone()),
                };
                let Some(owner) = owner else {
                    state.owner.insert(key.into(), task.into());
                    state.waits.remove(task);
                    return Ok(Ok(()));
                };
                // Deadlock check: follow who the owner waits for.
                let mut next = Some(owner.clone());
                let mut hops = 0;
                while let Some(current) = next {
                    if current == task {
                        state.waits.remove(task);
                        return Ok(Err(owner));
                    }
                    hops += 1;
                    if hops > 64 {
                        break;
                    }
                    next = state.waits.get(&current).cloned();
                }
                state.waits.insert(task.into(), owner.clone());
                waiting(&owner);
            }
            tokio::select! {
                _ = cancel.cancelled() => {
                    if let Ok(mut state) = self.state.lock() {
                        state.waits.remove(task);
                    }
                    return Err(Error::Cancelled);
                }
                _ = &mut notified => {}
            }
        }
    }

    pub fn release(&self, task: &str) {
        if let Ok(mut state) = self.state.lock() {
            state.owner.retain(|_, owner| owner != task);
            state.waits.remove(task);
        }
        self.notify.notify_waiters();
    }

    pub fn owners(&self) -> Vec<(String, String)> {
        self.state.lock().map_or(Vec::new(), |state| {
            state
                .owner
                .iter()
                .map(|(key, owner)| (key.clone(), owner.clone()))
                .collect()
        })
    }
}

/// A sub-agent's tool set: `base` limited to the type's tools, plus `regression`, with
/// optional write claims for parallel plans.
pub struct ScopedTools {
    pub base: Arc<dyn ToolExecutor>,
    pub allowed: Vec<String>,
    pub regression: Option<Arc<Regression>>,
    pub claims: Option<(Arc<Claims>, String)>,
    pub activity: Option<(Arc<ActivityBoard>, String)>,
    pub regression_passed: AtomicBool,
}

/// Compatibility shim: `inner` limited to `allowed`.
pub struct AllowedTools {
    pub inner: Arc<dyn ToolExecutor>,
    pub allowed: &'static [&'static str],
}

impl ToolExecutor for AllowedTools {
    fn definitions(&self) -> Vec<ToolDefinition> {
        self.inner
            .definitions()
            .into_iter()
            .filter(|definition| self.allowed.contains(&definition.name.as_str()))
            .collect()
    }

    fn concurrent(&self, name: &str) -> bool {
        self.allowed.contains(&name) && self.inner.concurrent(name)
    }

    fn metered(&self, name: &str) -> bool {
        self.allowed.contains(&name) && self.inner.metered(name)
    }

    fn execute(
        &self,
        name: String,
        input: Value,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ToolOutput>> {
        Box::pin(async move {
            if !self.allowed.contains(&name.as_str()) {
                return Ok(ToolOutput {
                    content: format!("{name} is not available to this sub-agent"),
                    failed: true,
                    ..Default::default()
                });
            }
            self.inner.execute(name, input, cancel).await
        })
    }
}

impl ScopedTools {
    async fn claim_paths(
        &self,
        name: &str,
        input: &Value,
        cancel: &CancellationToken,
    ) -> Result<Option<String>> {
        let Some((claims, task)) = &self.claims else {
            return Ok(None);
        };
        let paths: Vec<String> = match name {
            "write" | "edit" => input
                .get("path")
                .and_then(Value::as_str)
                .map(|path| vec![path.to_owned()])
                .unwrap_or_default(),
            "regression" => input
                .get("files")
                .and_then(Value::as_array)
                .map(|files| {
                    files
                        .iter()
                        .filter_map(Value::as_str)
                        .map(str::to_owned)
                        .collect()
                })
                .unwrap_or_default(),
            _ => return Ok(None),
        };
        for path in paths {
            let key = claim_key(&path);
            let activity = self.activity.clone();
            let outcome = claims
                .claim(task, &key, cancel, |owner| {
                    if let Some((board, id)) = &activity {
                        board.step(id, &format!("waiting for {owner} to finish with {path}"));
                    }
                })
                .await?;
            if let Err(owner) = outcome {
                return Ok(Some(format!(
                    "write conflict: {path} is claimed by plan task {owner}, which is waiting for a file you claimed; \
                     finish without touching {path} and report it, the plan will be retried"
                )));
            }
        }
        Ok(None)
    }
}

impl ToolExecutor for ScopedTools {
    fn definitions(&self) -> Vec<ToolDefinition> {
        let mut definitions: Vec<_> = self
            .base
            .definitions()
            .into_iter()
            .filter(|definition| self.allowed.contains(&definition.name))
            .collect();
        if self.regression.is_some() && self.allowed.iter().any(|tool| tool == "regression") {
            definitions.push(crate::agents::regression::definition());
        }
        definitions
    }

    fn metered(&self, name: &str) -> bool {
        self.allowed.contains(&name.to_owned()) && self.base.metered(name)
    }

    /// Reads claim no paths, so they keep the base tools' concurrency.
    fn concurrent(&self, name: &str) -> bool {
        self.allowed.contains(&name.to_owned()) && self.base.concurrent(name)
    }

    fn execute(
        &self,
        name: String,
        input: Value,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ToolOutput>> {
        Box::pin(async move {
            if !self.allowed.contains(&name) {
                return Ok(ToolOutput {
                    content: format!("{name} is not available to this sub-agent"),
                    failed: true,
                    ..Default::default()
                });
            }
            if let Some(conflict) = self.claim_paths(&name, &input, &cancel).await? {
                return Ok(ToolOutput {
                    content: conflict,
                    failed: true,
                    ..Default::default()
                });
            }
            if name == "regression" {
                let Some(regression) = &self.regression else {
                    return Ok(ToolOutput {
                        content: "regression is not available here".into(),
                        failed: true,
                        ..Default::default()
                    });
                };
                let output = regression.execute(input, cancel).await?;
                if !output.failed {
                    self.regression_passed.store(true, Ordering::SeqCst);
                }
                return Ok(output);
            }
            self.base.execute(name, input, cancel).await
        })
    }
}

/// Why a sub-agent run ended.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Exit {
    Completed,
    MaxIterations,
    Budget,
    Interrupted,
    ApprovalDenied(String),
    /// verify finished without passing regression evidence.
    Unverified,
    Error(String),
}

impl Exit {
    pub fn label(&self) -> &'static str {
        match self {
            Self::Completed => "completed",
            Self::MaxIterations => "max_iterations",
            Self::Budget => "budget",
            Self::Interrupted => "interrupted",
            Self::ApprovalDenied(_) => "approval_denied",
            Self::Unverified => "unverified",
            Self::Error(_) => "error",
        }
    }

    pub fn detail(&self) -> Option<&str> {
        match self {
            Self::ApprovalDenied(text) | Self::Error(text) => Some(text),
            _ => None,
        }
    }

    pub fn from_result(result: &Result<crate::engine::agent::TaskReport>) -> Self {
        match result {
            Ok(_) => Self::Completed,
            // Matches the engine's limit texts ("已用完 N 次模型调用", "已达到本会话的预算").
            Err(Error::Limit(text)) if text.contains("模型调用") => Self::MaxIterations,
            Err(Error::Limit(text)) if text.contains("预算") => Self::Budget,
            Err(Error::Cancelled) => Self::Interrupted,
            Err(Error::Approval(text)) => Self::ApprovalDenied(text.clone()),
            Err(err) => Self::Error(err.to_string()),
        }
    }

    /// Lifecycle state after a run with this exit.
    pub fn lifecycle(&self) -> Lifecycle {
        match self {
            Self::Completed => Lifecycle::Finished,
            Self::MaxIterations | Self::Budget | Self::Interrupted | Self::ApprovalDenied(_) => {
                Lifecycle::Interrupted
            }
            Self::Unverified | Self::Error(_) => Lifecycle::Failed,
        }
    }
}

pub struct RunOutcome {
    pub id: String,
    pub agent_type: String,
    pub description: String,
    pub exit: Exit,
    pub steps: u64,
    pub truncated: bool,
    pub output: String,
    pub spent: Accounting,
    pub tools_failed: u32,
    pub readonly: bool,
    /// Cancellation or an uncertain effect, which must reach the parent's journal.
    pub fatal: Option<Error>,
}

impl RunOutcome {
    pub fn succeeded(&self) -> bool {
        self.exit == Exit::Completed && (self.readonly || self.tools_failed == 0)
    }

    pub fn header(&self) -> String {
        format!(
            "[sub-agent {} ({}): {}] [exit: {}, steps: {}, truncated: {}]",
            self.id,
            self.agent_type,
            clip(&self.description, 80),
            self.exit.label(),
            self.steps,
            if self.truncated { "yes" } else { "no" }
        )
    }

    pub fn footer(&self) -> String {
        format!(
            "[cost: {}, steps: {}, failed tools: {}]",
            self.spent
                .cost
                .map_or("unknown".into(), |cost| format!("${cost:.4}")),
            self.steps,
            self.tools_failed
        )
    }

    pub fn render(&self) -> String {
        let mut text = self.header();
        if let Some(detail) = self.exit.detail() {
            text.push_str(&format!("\nstopped: {detail}"));
        }
        if self.exit == Exit::Unverified {
            text.push_str(
                "\nno passing regression evidence was produced; the change is NOT verified",
            );
        }
        text.push('\n');
        text.push_str(if self.output.trim().is_empty() {
            "(no answer)"
        } else {
            self.output.trim()
        });
        if !self.readonly && self.exit != Exit::Completed {
            text.push_str("\nFiles may be partially changed; inspect /diff before continuing.");
        }
        text.push('\n');
        text.push_str(&self.footer());
        text
    }
}

pub struct SubAgentSettings {
    pub max_turns: u32,
    pub max_output_tokens: u32,
    pub prices: Option<(f64, f64)>,
    pub context_tokens: usize,
    pub project_context: Option<String>,
    pub model: String,
    pub protocol: String,
    /// Session budget, for background agents resumed between tasks.
    pub budget: Option<f64>,
    pub max_background: usize,
    pub experimental: bool,
    pub regression_command: Option<String>,
}

pub struct HubParts {
    pub provider: Arc<dyn Provider>,
    pub factory: ToolFactory,
    pub types: TypeRegistry,
    pub settings: SubAgentSettings,
    pub ledger: Arc<Ledger>,
    pub progress: Arc<Progress>,
    pub root: PathBuf,
    pub workspace: Arc<Workspace>,
    pub store: Store,
    pub authorizer: Arc<dyn Authorizer>,
    /// Permission mode for background agents (their ask-mode approvals are queued).
    pub permission: crate::safety::permission::Mode,
    /// Whether a terminal can answer queued background approvals.
    pub interactive: bool,
}

pub struct AgentHub {
    pub provider: Arc<dyn Provider>,
    pub factory: ToolFactory,
    pub types: TypeRegistry,
    pub settings: SubAgentSettings,
    pub ledger: Arc<Ledger>,
    pub progress: Arc<Progress>,
    pub root: PathBuf,
    pub workspace: Arc<Workspace>,
    pub store: Store,
    pub authorizer: Arc<dyn Authorizer>,
    pub permission: crate::safety::permission::Mode,
    pub approvals: Arc<crate::agents::background::ApprovalQueue>,
    pub activity: Arc<ActivityBoard>,
    pub inbox: Arc<Inbox>,
    pub evidence: Arc<Mutex<Vec<RegressionEvidence>>>,
    pub worktrees: Worktrees,
    pub records: AgentStore,
    pub plan: crate::agents::plan::PlanState,
    pub board: crate::agents::collab::Board,
    pub background: crate::agents::background::BackgroundState,
    seq: AtomicU64,
}

/// What to run as a sub-agent.
pub struct RunRequest {
    pub id: String,
    pub kind: &'static str,
    pub agent_type: AgentType,
    pub description: String,
    pub prompt: String,
    pub budget: Option<f64>,
    /// Worktree root and its journal, instead of the main tree.
    pub worktree: Option<(PathBuf, Arc<Workspace>)>,
    pub claims: Option<(Arc<Claims>, String)>,
}

impl AgentHub {
    pub fn new(parts: HubParts) -> Result<Arc<Self>> {
        let records = AgentStore::open(parts.store.root(), &parts.root)?;
        let background = crate::agents::background::BackgroundState::default();
        let approvals = Arc::new(crate::agents::background::ApprovalQueue::new(
            parts.interactive,
            background.notify.clone(),
        ));
        Ok(Arc::new(Self {
            worktrees: Worktrees {
                project: parts.root.clone(),
                authorizer: parts.authorizer.clone(),
            },
            provider: parts.provider,
            factory: parts.factory,
            types: parts.types,
            settings: parts.settings,
            ledger: parts.ledger,
            progress: parts.progress,
            root: parts.root,
            workspace: parts.workspace,
            store: parts.store,
            authorizer: parts.authorizer,
            permission: parts.permission,
            approvals,
            activity: Arc::default(),
            inbox: Arc::default(),
            evidence: Arc::default(),
            records,
            plan: Default::default(),
            board: Default::default(),
            background,
            seq: AtomicU64::new(0),
        }))
    }

    pub fn next_id(&self, prefix: &str) -> String {
        format!("{prefix}-{}", self.seq.fetch_add(1, Ordering::SeqCst) + 1)
    }

    /// Regression evidence recorded in this process, for acceptance reports.
    pub fn evidence(&self) -> Vec<RegressionEvidence> {
        self.evidence
            .lock()
            .map(|all| all.clone())
            .unwrap_or_default()
    }

    /// The parent's own `regression` tool, rooted in the main tree.
    pub fn regression(&self, progress: Option<Arc<Progress>>) -> Regression {
        Regression {
            root: self.root.clone(),
            workspace: Some(self.workspace.clone()),
            authorizer: self.authorizer.clone(),
            progress,
            command: self.settings.regression_command.clone(),
            evidence: self.evidence.clone(),
        }
    }

    /// Tools for a sub-agent of `kind` acting in `root`.
    #[allow(clippy::too_many_arguments)]
    pub fn scoped_tools(
        &self,
        kind: &AgentType,
        root: &Path,
        workspace: Arc<Workspace>,
        progress: Arc<Progress>,
        authorizer: Arc<dyn Authorizer>,
        claims: Option<(Arc<Claims>, String)>,
        activity_id: &str,
    ) -> Result<Arc<ScopedTools>> {
        let base = (self.factory)(ToolScope {
            root: root.to_path_buf(),
            workspace: (!kind.readonly).then(|| workspace.clone()),
            progress: progress.clone(),
            authorizer: authorizer.clone(),
            readonly: kind.readonly,
        })?;
        let regression = kind.tools.iter().any(|tool| tool == "regression").then(|| {
            Arc::new(Regression {
                root: root.to_path_buf(),
                workspace: Some(workspace.clone()),
                authorizer: authorizer.clone(),
                progress: Some(progress.clone()),
                command: self.settings.regression_command.clone(),
                evidence: self.evidence.clone(),
            })
        });
        Ok(Arc::new(ScopedTools {
            base,
            allowed: kind.tools.clone(),
            regression,
            claims,
            activity: Some((self.activity.clone(), activity_id.into())),
            regression_passed: AtomicBool::new(false),
        }))
    }

    pub fn build_agent(
        &self,
        kind: &AgentType,
        tools: Arc<dyn ToolExecutor>,
        budget: Option<f64>,
        extra: Option<String>,
        inbox_key: &str,
    ) -> Result<Agent> {
        let mut instructions = kind.instructions.clone();
        if let Some(extra) = extra {
            instructions.push_str("\n\n");
            instructions.push_str(&extra);
        }
        Ok(Agent::new(
            self.provider.clone(),
            tools,
            Limits {
                max_turns: self.settings.max_turns,
                max_output_tokens: self.settings.max_output_tokens,
                budget,
                prices: self.settings.prices,
            },
        )?
        .with_context_tokens(self.settings.context_tokens)?
        .with_project_context(self.settings.project_context.clone())
        .with_instructions(Some(instructions))
        .with_turn_hook(Arc::new(InboxHook {
            inbox: self.inbox.clone(),
            key: inbox_key.into(),
        })))
    }

    /// A progress sink for sub-agent `id`: its events become `[id] ...` lines on the
    /// parent's progress stream and update its activity entry.
    pub fn forward_events(
        self: &Arc<Self>,
        id: &str,
    ) -> (
        Arc<Progress>,
        mpsc::Sender<Event>,
        tokio::task::JoinHandle<()>,
    ) {
        let (sender, mut receiver) = mpsc::channel::<Event>(64);
        let progress = Arc::new(Progress::default());
        progress.attach(Some(sender.clone()));
        let hub = self.clone();
        let id = id.to_owned();
        let forward = tokio::spawn(async move {
            let mut answering = false;
            while let Some(event) = receiver.recv().await {
                match event {
                    Event::Output { line, stderr } => {
                        hub.progress.line(format!("[{id}] {line}"), stderr).await;
                    }
                    Event::ToolStarted { name, .. } => {
                        answering = false;
                        hub.activity.step(&id, &format!("running {name}"));
                        hub.progress.line(format!("[{id}] -> {name}"), false).await;
                    }
                    Event::ToolFinished {
                        name,
                        failed,
                        output,
                    } => {
                        let first = clip(output.lines().next().unwrap_or_default(), 160);
                        let status = if failed { "failed" } else { "ok" };
                        hub.activity.step(&id, &format!("{name} {status}: {first}"));
                        hub.progress
                            .line(format!("[{id}] [{status}] {name}: {first}"), failed)
                            .await;
                    }
                    Event::Notice(text) => {
                        let first = clip(text.lines().next().unwrap_or_default(), 160);
                        hub.progress.line(format!("[{id}] {first}"), false).await;
                    }
                    Event::Text(_) | Event::Reasoning(_) if !answering => {
                        answering = true;
                        hub.activity.step(&id, "model responding");
                    }
                    _ => {}
                }
            }
        });
        (progress, sender, forward)
    }

    /// Runs one foreground sub-agent to completion and records its spend in the ledger.
    pub async fn run(
        self: &Arc<Self>,
        request: RunRequest,
        cancel: CancellationToken,
    ) -> RunOutcome {
        let kind = request.agent_type.clone();
        let mut outcome = RunOutcome {
            id: request.id.clone(),
            agent_type: kind.name.clone(),
            description: request.description.clone(),
            exit: Exit::Error(String::new()),
            steps: 0,
            truncated: false,
            output: String::new(),
            spent: Accounting {
                cost: self.settings.prices.map(|_| 0.0),
                ..Accounting::default()
            },
            tools_failed: 0,
            readonly: kind.readonly,
            fatal: None,
        };
        self.activity.start(
            &request.id,
            request.kind,
            &kind.name,
            &self.settings.model,
            &request.description,
        );
        let (progress, events, forward) = self.forward_events(&request.id);
        let (root, workspace, extra) = match &request.worktree {
            Some((root, workspace)) => (
                root.clone(),
                workspace.clone(),
                Some(format!(
                    "You work inside the git worktree {}; every path is relative to it and it is separate from the user's main tree. \
                     Commit finished steps there with git so the work survives interruption.",
                    root.display()
                )),
            ),
            None => (self.root.clone(), self.workspace.clone(), None),
        };
        let built = self
            .scoped_tools(
                &kind,
                &root,
                workspace,
                progress.clone(),
                self.authorizer.clone(),
                request.claims.clone(),
                &request.id,
            )
            .and_then(|tools| {
                let agent =
                    self.build_agent(&kind, tools.clone(), request.budget, extra, &request.id)?;
                Ok((tools, agent))
            });
        let (tools, agent) = match built {
            Ok(built) => built,
            Err(err) => {
                progress.attach(None);
                drop(events);
                let _ = forward.await;
                outcome.exit = Exit::Error(format!("sub-agent not started: {err}"));
                self.activity.finish(&request.id, Lifecycle::Failed);
                return outcome;
            }
        };
        let (result, spent, text) = agent
            .run_detached_events(request.prompt.clone(), cancel, events)
            .await;
        progress.attach(None);
        let _ = forward.await;
        self.ledger.record(&spent);
        outcome.exit = Exit::from_result(&result);
        if outcome.exit == Exit::Completed
            && kind.requires_regression
            && !tools.regression_passed.load(Ordering::SeqCst)
        {
            outcome.exit = Exit::Unverified;
        }
        outcome.truncated = outcome.exit == Exit::MaxIterations;
        outcome.steps = spent.model_calls;
        outcome.spent = spent;
        outcome.output = text;
        if let Ok(report) = &result {
            outcome.tools_failed = report.tools_failed;
        }
        if let Err(err @ (Error::Cancelled | Error::Uncertain(_))) = result {
            outcome.fatal = Some(err);
        }
        self.activity.finish(&request.id, outcome.exit.lifecycle());
        outcome
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct AgentArgs {
    description: String,
    prompt: String,
    #[serde(default, alias = "agent_type", alias = "subagent_type")]
    r#type: Option<String>,
    /// Legacy: read = explore, write = general.
    #[serde(default)]
    mode: Option<String>,
    #[serde(default)]
    worktree: Option<String>,
    #[serde(default)]
    background: bool,
    #[serde(default)]
    isolation: Option<String>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ControlArgs {
    action: String,
    #[serde(default)]
    id: Option<String>,
    #[serde(default)]
    message: Option<String>,
}

/// Adds the agent tools to `inner`.
pub struct WithSubAgent {
    pub inner: Arc<dyn ToolExecutor>,
    pub hub: Arc<AgentHub>,
}

fn failed(content: impl Into<String>) -> Result<ToolOutput> {
    Ok(ToolOutput {
        content: content.into(),
        failed: true,
        ..Default::default()
    })
}

const OWN_TOOLS: &[&str] = &[
    "agent",
    "agent_control",
    "execute_plan",
    "worktree",
    "exit_worktree",
    "regression",
];

impl WithSubAgent {
    fn resolve_type(&self, args: &AgentArgs) -> Result<AgentType> {
        let from_mode = match args.mode.as_deref() {
            None => None,
            Some("read") => Some("explore"),
            Some("write") => Some("general"),
            Some(other) => {
                return Err(Error::Tool(format!(
                    "mode must be read or write, not {other}"
                )));
            }
        };
        let name = match (args.r#type.as_deref(), from_mode) {
            (Some(kind), Some(mode)) if !kind.eq_ignore_ascii_case(mode) => {
                return Err(Error::Tool(format!(
                    "type={kind} contradicts mode={}",
                    args.mode.as_deref().unwrap_or_default()
                )));
            }
            (Some(kind), _) => kind,
            (None, Some(mode)) => mode,
            (None, None) => "general",
        };
        self.hub.types.get(name).cloned()
    }

    async fn delegate(&self, args: AgentArgs, cancel: CancellationToken) -> Result<ToolOutput> {
        if args.prompt.trim().is_empty() || args.prompt.len() > 32 * 1024 {
            return failed("prompt must be 1..32768 bytes");
        }
        let kind = match self.resolve_type(&args) {
            Ok(kind) => kind,
            Err(err) => return failed(err.to_string()),
        };
        let budget = self.hub.ledger.remaining_budget();
        if budget.is_some_and(|budget| budget <= 0.0) {
            return failed("session budget exhausted; sub-agent not started");
        }
        if args.background {
            if args.worktree.is_some() {
                return failed(
                    "background agents get their own fresh worktree; omit worktree (use isolation)",
                );
            }
            let isolated = match args.isolation.as_deref() {
                None => !kind.readonly,
                Some("worktree") => true,
                Some("none") if kind.readonly => false,
                Some("none") => {
                    return failed(format!(
                        "{} edits files; background editing agents need isolation=\"worktree\"",
                        kind.name
                    ));
                }
                Some(other) => return failed(format!("unknown isolation {other}")),
            };
            return match self
                .hub
                .spawn_background(
                    kind,
                    args.description,
                    args.prompt,
                    isolated,
                    budget,
                    &cancel,
                )
                .await
            {
                Ok(text) => Ok(ToolOutput {
                    content: text,
                    failed: false,
                    ..Default::default()
                }),
                Err(err @ (Error::Approval(_) | Error::Cancelled | Error::Uncertain(_))) => {
                    Err(err)
                }
                Err(err) => failed(err.to_string()),
            };
        }
        if args.isolation.is_some() {
            return failed(
                "isolation applies to background agents; use worktree=<branch> for a foreground one",
            );
        }
        let worktree = match &args.worktree {
            None => None,
            Some(branch) => {
                if kind.readonly {
                    return failed("worktree applies to editing agent types");
                }
                let root = match self.hub.worktrees.resolve(branch, &cancel).await {
                    Ok(root) => root,
                    Err(err) => return failed(err.to_string()),
                };
                let workspace = Arc::new(Workspace::new(root.clone(), self.hub.store.clone())?);
                workspace.begin()?;
                Some((root, workspace))
            }
        };
        let outcome = self
            .hub
            .run(
                RunRequest {
                    id: self.hub.next_id("agent"),
                    kind: "agent",
                    agent_type: kind,
                    description: args.description,
                    prompt: args.prompt,
                    budget,
                    worktree,
                    claims: None,
                },
                cancel,
            )
            .await;
        if let Some(err) = outcome.fatal {
            return Err(err);
        }
        Ok(ToolOutput {
            content: outcome.render(),
            failed: !outcome.succeeded(),
            ..Default::default()
        })
    }

    async fn control(&self, args: ControlArgs, cancel: CancellationToken) -> Result<ToolOutput> {
        let id = args.id.as_deref().unwrap_or_default();
        let result = match args.action.as_str() {
            "list" => Ok(self.hub.status_text(None).await),
            "stop" => self.hub.stop_agent(id, false).await,
            "park" => self.hub.park_agent(id, &cancel).await,
            "resume" => {
                let remaining = self.hub.ledger.remaining_budget();
                self.hub
                    .resume_agent(id, args.message.clone(), false, remaining)
                    .await
            }
            "send" => match args.message.as_deref() {
                Some(message) if !message.trim().is_empty() => {
                    let remaining = self.hub.ledger.remaining_budget();
                    self.hub.send_agent(id, message.into(), remaining).await
                }
                _ => Err(Error::Tool("send needs a message".into())),
            },
            "merge" | "discard" => Err(Error::Tool(format!(
                "{} changes the user's repository; ask the user to run /status agents {} {id}",
                args.action, args.action
            ))),
            other => Err(Error::Tool(format!(
                "unknown action {other}; use list, stop, park, resume or send"
            ))),
        };
        match result {
            Ok(content) => Ok(ToolOutput {
                content,
                failed: false,
                ..Default::default()
            }),
            Err(err @ (Error::Approval(_) | Error::Cancelled | Error::Uncertain(_))) => Err(err),
            Err(err) => failed(err.to_string()),
        }
    }
}

impl ToolExecutor for WithSubAgent {
    fn definitions(&self) -> Vec<ToolDefinition> {
        let mut definitions = self.inner.definitions();
        definitions.push(ToolDefinition {
            name: "agent".into(),
            description: format!(
                "Delegate a self-contained subtask to a sub-agent with at most {} model calls. Types: {}. \
                 Editing types share your approvals and checkpoint; read-only types never edit. It sees nothing of this conversation: \
                 give full context in prompt. worktree=<branch> runs an editing agent inside a worktree made with the worktree tool. \
                 background=true returns an agent id at once and runs it as a durable background agent (editing types in their own fresh \
                 git worktree; you are notified when it finishes; manage it with agent_control). Costs count toward this session's budget. \
                 The result header reports [exit, steps, truncated].",
                self.hub.settings.max_turns,
                self.hub.types.summary()
            ),
            schema: json!({"type":"object","properties":{
                "description":{"type":"string","description":"3-8 word label"},
                "prompt":{"type":"string","description":"Complete task with all needed context"},
                "type":{"type":"string","description":"Agent type (default general)"},
                "mode":{"enum":["read","write"],"description":"Legacy alias: read=explore, write=general"},
                "worktree":{"type":"string","description":"Branch of an existing worktree to work in"},
                "background":{"type":"boolean"},
                "isolation":{"enum":["worktree","none"]}
            },"required":["description","prompt"],"additionalProperties":false}),
        });
        definitions.push(ToolDefinition {
            name: "agent_control".into(),
            description: "Manage background agents: list; stop <id> (interrupt, resumable); park <id> (suspend, commits its worktree, frees its slot); \
                          resume <id> [message]; send <id> message (queued for a running agent at its next tool round, resumes a stopped one). \
                          Merging or discarding an agent's work is only done by the user (/status agents merge|discard <id>)."
                .into(),
            schema: json!({"type":"object","properties":{
                "action":{"enum":["list","stop","park","resume","send"]},
                "id":{"type":"string"},"message":{"type":"string"}
            },"required":["action"],"additionalProperties":false}),
        });
        definitions.push(ToolDefinition {
            name: "execute_plan".into(),
            description: "Run the pending items of your latest todo list as sub-agents in dependency order (todo id/depends, or a \
                          'depends:todo-1,todo-2 ' content prefix). parallel=true runs independent items concurrently (max_agents 1-8, \
                          default 4); two running items never write the same file at once. Each failed item is retried once. \
                          Use only for plans of 3+ substantial items; update the todo list from the per-item report afterwards."
                .into(),
            schema: json!({"type":"object","properties":{
                "parallel":{"type":"boolean"},
                "max_agents":{"type":"integer","minimum":1,"maximum":8},
                "type":{"type":"string","description":"Agent type for every item (default general)"}
            },"additionalProperties":false}),
        });
        definitions.extend(crate::agents::worktree::definitions());
        definitions.push(crate::agents::regression::definition());
        if self.hub.settings.experimental {
            definitions.extend(crate::agents::collab::definitions());
        }
        definitions
    }

    fn metered(&self, name: &str) -> bool {
        matches!(name, "agent" | "execute_plan" | "team_create") || self.inner.metered(name)
    }

    fn concurrent(&self, name: &str) -> bool {
        name != "agent" && self.inner.concurrent(name)
    }

    fn execute(
        &self,
        name: String,
        input: Value,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ToolOutput>> {
        Box::pin(async move {
            let experimental = self.hub.settings.experimental
                && crate::agents::collab::TOOLS.contains(&name.as_str());
            if !OWN_TOOLS.contains(&name.as_str()) && !experimental {
                let todo = (name == "todo").then(|| input.clone());
                let output = self.inner.execute(name, input, cancel).await?;
                if let Some(todo) = todo
                    && !output.failed
                {
                    self.hub.plan.record(todo);
                }
                return Ok(output);
            }
            match name.as_str() {
                "agent" => match serde_json::from_value(input) {
                    Ok(args) => self.delegate(args, cancel).await,
                    Err(err) => failed(err.to_string()),
                },
                "agent_control" => match serde_json::from_value(input) {
                    Ok(args) => self.control(args, cancel).await,
                    Err(err) => failed(err.to_string()),
                },
                "execute_plan" => crate::agents::plan::execute(&self.hub, input, cancel).await,
                "worktree" | "exit_worktree" => {
                    self.hub.worktrees.execute(&name, input, cancel).await
                }
                "regression" => {
                    self.hub
                        .regression(Some(self.hub.progress.clone()))
                        .execute(input, cancel)
                        .await
                }
                _ => crate::agents::collab::execute(&self.hub, &name, input, cancel).await,
            }
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn builtin_types_have_the_documented_tools() {
        let registry = TypeRegistry::builtin();
        let tools = |name: &str| registry.get(name).unwrap().tools.clone();
        assert!(tools("general").contains(&"write".to_owned()));
        assert!(tools("test").contains(&"shell".to_owned()));
        assert!(!tools("explore").contains(&"write".to_owned()));
        assert!(tools("explore").contains(&"web_fetch".to_owned()));
        assert_eq!(tools("review"), names(&[READ_TOOLS]));
        assert!(tools("verify").contains(&"regression".to_owned()));
        assert!(!tools("verify").iter().any(|tool| tool.starts_with("web")));
        assert!(registry.get("verify").unwrap().requires_regression);
        assert!(registry.get("unknown").is_err());
    }

    #[test]
    fn custom_types_are_validated() {
        let registry = TypeRegistry::from_config(&json!({"agent_types":{
            "docs":{"description":"docs writer","instructions":"Write docs.","tools":["read","write"]},
            "audit":{"description":"audit","instructions":"Audit.","readonly":true}
        }}))
        .unwrap();
        assert_eq!(registry.get("docs").unwrap().tools, ["read", "write"]);
        assert!(registry.get("audit").unwrap().readonly);
        assert!(
            !registry
                .get("audit")
                .unwrap()
                .tools
                .contains(&"write".to_owned())
        );
        for bad in [
            json!({"agent_types":{"general":{"description":"x","instructions":"y"}}}),
            json!({"agent_types":{"ro":{"description":"x","instructions":"y","readonly":true,"tools":["shell"]}}}),
            json!({"agent_types":{"Bad Name":{"description":"x","instructions":"y"}}}),
            json!({"agent_types":{"x":{"description":"x","instructions":""}}}),
        ] {
            assert!(TypeRegistry::from_config(&bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn exits_map_to_lifecycle_states() {
        assert_eq!(Exit::Completed.lifecycle(), Lifecycle::Finished);
        assert_eq!(Exit::MaxIterations.lifecycle(), Lifecycle::Interrupted);
        assert_eq!(Exit::Unverified.lifecycle(), Lifecycle::Failed);
        assert_eq!(
            Exit::from_result(&Err(Error::Limit("已用完 20 次模型调用".into()))),
            Exit::MaxIterations
        );
        assert_eq!(
            Exit::from_result(&Err(Error::Limit("已达到本会话的预算（估算）".into()))),
            Exit::Budget
        );
    }

    #[tokio::test]
    async fn claims_serialize_writers_and_detect_deadlocks() {
        let claims = Arc::new(Claims::default());
        let cancel = CancellationToken::new();
        claims
            .claim("a", "x", &cancel, |_| {})
            .await
            .unwrap()
            .unwrap();
        claims
            .claim("b", "y", &cancel, |_| {})
            .await
            .unwrap()
            .unwrap();
        // b waits for a's file until a ends.
        let waiter = {
            let claims = claims.clone();
            let cancel = cancel.clone();
            tokio::spawn(async move { claims.claim("b", "x", &cancel, |_| {}).await })
        };
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;
        assert!(!waiter.is_finished());
        // a now wants b's file while b waits for a: deadlock, refused.
        assert_eq!(
            claims.claim("a", "y", &cancel, |_| {}).await.unwrap(),
            Err("b".to_owned())
        );
        claims.release("a");
        waiter.await.unwrap().unwrap().unwrap();
        assert!(claims.owners().contains(&("x".into(), "b".into())));
        assert!(!claim_key(".\\Src\\A.rs").contains('\\'));
    }

    #[test]
    fn inbox_delivers_once() {
        let inbox = Arc::new(Inbox::default());
        inbox.push("t", "hello".into());
        let hook = InboxHook {
            inbox: inbox.clone(),
            key: "t".into(),
        };
        let output = ToolOutput {
            content: String::new(),
            failed: false,
            ..Default::default()
        };
        assert!(
            hook.tool_note("read", &json!({}), &output)
                .unwrap()
                .contains("hello")
        );
        assert!(hook.tool_note("read", &json!({}), &output).is_none());
    }
}
