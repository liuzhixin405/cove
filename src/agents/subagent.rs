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
    agents::check_slot::{CheckRun, CheckSlot},
    agents::explore::{self, ModelTier},
    agents::lifecycle::{AgentStore, Lifecycle},
    agents::regression::{Regression, RegressionEvidence},
    agents::worktree::Worktrees,
    agents::writes::ItemScope,
    domain::{Error, Event, Progress, Provider, Result, ToolDefinition, UserInput},
    engine::agent::{Agent, Ledger, Limits},
    engine::turn::TurnHook,
    safety::permission::{Authorizer, Effect},
    session::overlay::OverlayStack,
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
    "shell_task",
    "todo",
    "skill",
    "web_fetch",
    "web_search",
    "regression",
];

/// Every sub-agent's final answer lands in the parent's context and stays there.
pub const REPORT_INSTRUCTIONS: &str = "Your final answer is read by the parent agent and stays in its context for the rest of the task, so keep it short: \
    lead with the conclusion, then only the facts that support it (file:line references instead of quoted code, exact commands and results). \
    Aim for at most about 400 words, whatever length the task asks for: cove cuts answers longer than 6 KB; do not restate the task, list what you looked at, or repeat yourself.";

const READ_INSTRUCTIONS: &str = "You are a delegated read-only sub-agent (explore). You cannot edit files, run shell commands or ask the user. \
    Investigate only the assigned question with read/glob/grep/repo_map (and web tools when sources outside the repository matter); put independent reads and searches in one response. \
    Answer in exactly three parts, in the language of the task:\n\
    Conclusion: one or two sentences that answer the question directly.\n\
    Evidence: one line per fact, `path:line` or `path:start-end` (workspace-relative, `/` separators) followed by what is there, \
    for example `- src/engine/agent.rs:619-627 prices come from limits.prices`. Cite only lines you actually read; cove checks that every cited place exists.\n\
    Not determined: what you could not settle and why. Tell \"searched and confirmed absent\" (name the search, e.g. grep 'save(' in src/ and tests/) apart from \"not searched\"; \
    never turn \"not found\" into \"does not exist\" without saying what you searched. Do not guess.";

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
    /// The model it runs on (explore: fast; plan, review, verify: main).
    pub model: ModelTier,
    /// Its own cap on model calls, below `subagent_max_iterations` (explore: 30).
    pub max_turns: Option<u32>,
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
        model: match name {
            "explore" => ModelTier::Fast,
            "plan" | "review" | "verify" => ModelTier::Main,
            _ => ModelTier::Inherit,
        },
        max_turns: (name == "explore").then_some(explore::EXPLORE_MAX_TURNS),
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
    /// main, fast, inherit (default) or a model name.
    #[serde(default)]
    model: Option<String>,
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
            let model = match spec.model.as_deref() {
                None => ModelTier::Inherit,
                Some(text) => ModelTier::parse(text)
                    .map_err(|err| Error::Session(format!("agent_types.{name}: {err}")))?,
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
                    model,
                    max_turns: None,
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
    /// An overlay item's layers (0053): its file tools read through them and write only
    /// into the top one; nothing reaches the disk or the checkpoint.
    pub overlay: Option<Arc<OverlayStack>>,
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
                task: crate::text::short(task, 120),
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
            entry.last_step = crate::text::short(step, 120);
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
    /// The declared writes of a plan item (0054): writes outside them and file-generating
    /// commands are refused, files its shell commands change outside them recorded.
    pub scope: Option<Arc<ItemScope>>,
    /// The `check` tool of an overlay item while `overlay_check` is configured (0053).
    pub check: Option<CheckRun>,
    /// An overlay item: its shell commands only read, so they are not compared around.
    pub overlay: bool,
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

    fn concurrent(&self, name: &str, input: &Value) -> bool {
        self.allowed.contains(&name) && self.inner.concurrent(name, input)
    }

    fn metered(&self, name: &str) -> bool {
        self.allowed.contains(&name) && self.inner.metered(name)
    }

    fn jobs(&self) -> Option<std::sync::Arc<crate::tools::jobs::Jobs>> {
        self.inner.jobs()
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
            // Same aliases the write/edit tools accept (tools/mod.rs), or a claim
            // silently misses a parallel write spelled with file_path/filePath.
            "write" | "edit" => ["path", "file_path", "filePath"]
                .iter()
                .find_map(|key| input.get(key).and_then(Value::as_str))
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

impl ScopedTools {
    /// Runs a call of a write-scoped plan item: records the file a file tool wrote, and
    /// what a shell command changed (outside the item's writes: kept, but reported).
    async fn scoped(
        &self,
        scope: &ItemScope,
        name: String,
        input: Value,
        cancel: CancellationToken,
    ) -> Result<ToolOutput> {
        let before = if name == "shell" && !self.overlay {
            // The run ends when it is dropped, on every way out of this call.
            let tree = scope.tree(&cancel).await;
            tree.map(|tree| (tree, scope.shell_run()))
        } else {
            None
        };
        let mut output = self
            .base
            .execute(name.clone(), input.clone(), cancel.clone())
            .await?;
        if !output.failed {
            scope.wrote(&name, &input);
        }
        if output.refused
            && let Some((_, run)) = &before
        {
            run.void();
        }
        if let Some((before, run)) = before
            && !output.refused
            && let Some(after) = scope.tree(&cancel).await
        {
            let outside = scope.shell_changed(&before, &after, &run);
            if !outside.is_empty() {
                output.content.push_str(&format!(
                    "\n[writes] This command changed files outside this plan item's writes: {}. They were kept and are reported to the parent; do not change more files outside your writes.",
                    outside.join(", ")
                ));
            }
        }
        Ok(output)
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
        if let Some(check) = &self.check {
            definitions.push(crate::agents::check_slot::definition(check.slot.command()));
        }
        definitions
    }

    fn metered(&self, name: &str) -> bool {
        self.allowed.contains(&name.to_owned()) && self.base.metered(name)
    }

    fn jobs(&self) -> Option<std::sync::Arc<crate::tools::jobs::Jobs>> {
        self.base.jobs()
    }

    /// Reads claim no paths, so they keep the base tools' concurrency.
    fn concurrent(&self, name: &str, input: &Value) -> bool {
        self.allowed.contains(&name.to_owned()) && self.base.concurrent(name, input)
    }

    fn execute(
        &self,
        name: String,
        input: Value,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ToolOutput>> {
        Box::pin(async move {
            if name == "check"
                && let Some(check) = &self.check
            {
                return check.execute(cancel).await;
            }
            if !self.allowed.contains(&name) {
                return Ok(ToolOutput {
                    content: format!("{name} is not available to this sub-agent"),
                    failed: true,
                    ..Default::default()
                });
            }
            if let Some(refusal) = self
                .scope
                .as_ref()
                .and_then(|scope| scope.refuse(&name, &input))
            {
                return Ok(ToolOutput {
                    content: refusal,
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
            match &self.scope {
                Some(scope) => self.scoped(scope, name, input, cancel).await,
                None => self.base.execute(name, input, cancel).await,
            }
        })
    }
}

/// Why a sub-agent run ended.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Exit {
    Completed,
    MaxIterations,
    /// Stopped after rounds without new changes or information.
    Stalled,
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
            Self::Stalled => "stalled",
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
            Err(Error::Stopped(stopped)) => match stopped.stop {
                crate::engine::agent::Stop::Limit(_) => Self::MaxIterations,
                crate::engine::agent::Stop::Stalled(_) => Self::Stalled,
            },
            Err(Error::Cancelled) => Self::Interrupted,
            Err(Error::Approval(text)) => Self::ApprovalDenied(text.clone()),
            Err(err) => Self::Error(err.to_string()),
        }
    }

    /// Lifecycle state after a run with this exit.
    pub fn lifecycle(&self) -> Lifecycle {
        match self {
            Self::Completed => Lifecycle::Finished,
            Self::MaxIterations | Self::Stalled | Self::Interrupted | Self::ApprovalDenied(_) => {
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
    /// The model(s) that answered.
    pub model: String,
    /// `citations: N ok, M invalid` of a read-only run's evidence (0029).
    pub citations: Option<String>,
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
            crate::text::short(&self.description, 80),
            self.exit.label(),
            self.steps,
            if self.truncated { "yes" } else { "no" }
        )
    }

    pub fn footer(&self) -> String {
        format!(
            "[cost: {}, model: {}, steps: {}, failed tools: {}]",
            self.spent
                .cost
                .map_or("unknown".into(), |cost| format!("${cost:.4}")),
            if self.model.is_empty() {
                "unknown"
            } else {
                &self.model
            },
            self.steps,
            self.tools_failed
        )
    }

    pub fn render(&self) -> String {
        let mut text = self.header();
        if let Some(citations) = &self.citations {
            text.push('\n');
            text.push_str(citations);
        }
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

/// The configured models as sub-agents see them, live across `/model` switches (the
/// router; `commands/agents.rs`). A sub-agent has no `ModelPolicy`: its runs must not
/// change the main task's routing.
pub trait ModelCatalog: Send + Sync {
    fn main_model(&self) -> String;
    /// The fast model, only when one different from the main model is configured.
    fn fast_model(&self) -> Option<String>;
    /// The model the current (or last) task runs on.
    fn task_model(&self) -> String;
    /// Prices of the model that answered; `None` when unknown.
    fn prices(&self, model: &str) -> Option<crate::model::models::Prices>;
    /// Context budget (estimated tokens) and output cap for `model`.
    fn budget(&self, model: &str) -> (usize, u32);
}

/// Sub-agent spend is billed at the prices of the model that answered.
fn pricing(catalog: &Arc<dyn ModelCatalog>) -> crate::session::usage::Pricing {
    let catalog = catalog.clone();
    Arc::new(move |model: &str| catalog.prices(model))
}

tokio::task_local! {
    /// The id of the sub-agent whose model calls run in this future (trace `agent`).
    pub static CURRENT_AGENT: String;
}

/// The models that answered a run, most calls first; `requested` when none reported.
fn served_by(spent: &Accounting, requested: &str) -> String {
    let mut models: Vec<_> = spent.by_model.iter().collect();
    models.sort_by(|a, b| b.1.calls.cmp(&a.1.calls).then(a.0.cmp(b.0)));
    if models.is_empty() {
        return requested.to_owned();
    }
    models
        .into_iter()
        .map(|(model, _)| model.as_str())
        .collect::<Vec<_>>()
        .join(" + ")
}

/// The tier of `kind`; `explore_model: "main"` moves explore to the main model.
fn tier_for(kind: &AgentType, explore_fast: bool) -> ModelTier {
    match &kind.model {
        ModelTier::Fast if kind.name == "explore" && !explore_fast => ModelTier::Main,
        tier => tier.clone(),
    }
}

/// Read-only and without `regression` (which swaps files to their baseline while it runs).
fn parallel_kind(kind: &AgentType) -> bool {
    kind.readonly && !kind.tools.iter().any(|tool| tool == "regression")
}

/// Read-only foreground agents of one batch running at once (as `execute_plan`).
pub const PARALLEL_READONLY: usize = 4;

pub struct SubAgentSettings {
    pub max_turns: u32,
    /// Rounds without progress that stop a sub-agent (`stall_rounds`; 0 = off).
    pub stall_rounds: u32,
    pub max_output_tokens: u32,
    pub prices: Option<(f64, f64)>,
    pub context_tokens: usize,
    pub project_context: Option<String>,
    pub model: String,
    pub protocol: String,
    pub max_background: usize,
    pub experimental: bool,
    pub regression_command: Option<String>,
    pub catalog: Arc<dyn ModelCatalog>,
    /// `explore_model`: explore on the fast model (default) or the main one.
    pub explore_fast: bool,
    /// `overlay_check`: the compile check of overlay items' shared check slot (0053).
    pub overlay_check: Option<String>,
    /// `mergiraf_path` (trusted config only), else `mergiraf` on PATH; `None`: no
    /// structural merge.
    pub mergiraf: Option<PathBuf>,
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
    /// The terminal, for questions such as the overlay plan confirmation (0053).
    pub input: Option<Arc<dyn UserInput>>,
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
    /// The terminal; `None` without one (`-p`).
    pub input: Option<Arc<dyn UserInput>>,
    /// The shared check slot of overlay plans, when `overlay_check` is set.
    pub check_slot: Option<Arc<CheckSlot>>,
    /// Overlay directories kept for the current task (failed or conflicting items);
    /// removed when it ends.
    pub overlay_dirs: Mutex<Vec<PathBuf>>,
    /// Slots of read-only foreground agents running in parallel.
    readonly_slots: tokio::sync::Semaphore,
    seq: AtomicU64,
}

/// What to run as a sub-agent.
pub struct RunRequest {
    pub id: String,
    pub kind: &'static str,
    pub agent_type: AgentType,
    pub description: String,
    pub prompt: String,
    /// The model of every request; `None` is the hub's default.
    pub model: Option<String>,
    /// Approvals for this run instead of the parent's (a race lane's own worktree).
    pub authorizer: Option<Arc<dyn Authorizer>>,
    /// Started outside a tool call (`/race`): the spend waits in the ledger's
    /// background slot, which the session absorbs between tasks.
    pub detached: bool,
    /// Worktree root and its journal, instead of the main tree.
    pub worktree: Option<(PathBuf, Arc<Workspace>)>,
    pub claims: Option<(Arc<Claims>, String)>,
    /// The write constraint of a plan item that declared writes.
    pub scope: Option<Arc<ItemScope>>,
    /// The layers of an overlay plan item (0053).
    pub overlay: Option<Arc<OverlayStack>>,
}

impl AgentHub {
    pub fn new(parts: HubParts) -> Result<Arc<Self>> {
        let records = AgentStore::open(parts.store.root(), &parts.root)?;
        let background = crate::agents::background::BackgroundState::default();
        let check_slot = parts
            .settings
            .overlay_check
            .clone()
            .map(|command| Arc::new(CheckSlot::new(parts.store.root(), &parts.root, command)));
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
            input: parts.input,
            check_slot,
            overlay_dirs: Mutex::default(),
            readonly_slots: tokio::sync::Semaphore::new(PARALLEL_READONLY),
            seq: AtomicU64::new(0),
        }))
    }

    /// Whether explore runs on a fast model different from the main one.
    pub fn explore_on_fast(&self) -> bool {
        let fast = self.settings.catalog.fast_model();
        self.types
            .get("explore")
            .ok()
            .and_then(|kind| self.model_for(kind))
            .is_some_and(|model| fast.as_deref() == Some(model.as_str()))
    }

    /// The model a run of `kind` requests; `None` keeps the current task's model.
    pub fn model_for(&self, kind: &AgentType) -> Option<String> {
        tier_for(kind, self.settings.explore_fast).resolve(&*self.settings.catalog)
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
        scope: Option<Arc<ItemScope>>,
        overlay: Option<Arc<OverlayStack>>,
        activity_id: &str,
    ) -> Result<Arc<ScopedTools>> {
        let base = (self.factory)(ToolScope {
            root: root.to_path_buf(),
            workspace: (!kind.readonly).then(|| workspace.clone()),
            progress: progress.clone(),
            authorizer: authorizer.clone(),
            readonly: kind.readonly,
            overlay: overlay.clone(),
        })?;
        let check = match (&self.check_slot, &overlay) {
            (Some(slot), Some(stack)) if !kind.readonly => Some(CheckRun {
                slot: slot.clone(),
                stack: stack.clone(),
                authorizer: authorizer.clone(),
                progress: progress.clone(),
            }),
            _ => None,
        };
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
        // A type that may run commands may also check and stop its background ones.
        let mut allowed = kind.tools.clone();
        if allowed.iter().any(|tool| tool == "shell")
            && !allowed.iter().any(|tool| tool == "shell_task")
        {
            allowed.push("shell_task".into());
        }
        Ok(Arc::new(ScopedTools {
            base,
            allowed,
            regression,
            claims,
            scope,
            check,
            overlay: overlay.is_some(),
            activity: Some((self.activity.clone(), activity_id.into())),
            regression_passed: AtomicBool::new(false),
        }))
    }

    /// An agent of `kind` whose requests go to `model` (`None`: the task's model), with
    /// that model's window, output cap and prices.
    pub fn build_agent(
        &self,
        kind: &AgentType,
        tools: Arc<dyn ToolExecutor>,
        extra: Option<String>,
        inbox_key: &str,
        model: Option<String>,
    ) -> Result<Agent> {
        let mut instructions = kind.instructions.clone();
        instructions.push_str("\n\n");
        instructions.push_str(REPORT_INSTRUCTIONS);
        if let Some(extra) = extra {
            instructions.push_str("\n\n");
            instructions.push_str(&extra);
        }
        let catalog = &self.settings.catalog;
        let serving = model.clone().unwrap_or_else(|| catalog.task_model());
        let (context_tokens, max_output) = if serving == catalog.main_model() {
            (
                self.settings.context_tokens,
                self.settings.max_output_tokens,
            )
        } else {
            let (tokens, output) = catalog.budget(&serving);
            (tokens, output.min(self.settings.max_output_tokens))
        };
        let max_turns = kind.max_turns.map_or(self.settings.max_turns, |cap| {
            cap.min(self.settings.max_turns)
        });
        Ok(Agent::new(
            self.provider.clone(),
            tools,
            Limits {
                max_turns,
                max_output_tokens: max_output.max(1),
                prices: self.settings.prices,
            },
        )?
        .with_model(model)
        .with_pricing(pricing(catalog))
        .with_stall_rounds(self.settings.stall_rounds)
        .with_context_tokens(context_tokens)?
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
                        let first =
                            crate::text::short(output.lines().next().unwrap_or_default(), 160);
                        let status = if failed { "failed" } else { "ok" };
                        hub.activity.step(&id, &format!("{name} {status}: {first}"));
                        hub.progress
                            .line(format!("[{id}] [{status}] {name}: {first}"), failed)
                            .await;
                    }
                    Event::Notice(text) => {
                        let first =
                            crate::text::short(text.lines().next().unwrap_or_default(), 160);
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
        let model = request.model.clone().or_else(|| self.model_for(&kind));
        let mut outcome = RunOutcome {
            id: request.id.clone(),
            agent_type: kind.name.clone(),
            description: request.description.clone(),
            exit: Exit::Error(String::new()),
            steps: 0,
            truncated: false,
            output: String::new(),
            spent: Accounting {
                cost: Some(0.0),
                ..Accounting::default()
            },
            tools_failed: 0,
            readonly: kind.readonly,
            model: String::new(),
            citations: None,
            fatal: None,
        };
        let requested = model
            .clone()
            .unwrap_or_else(|| self.settings.catalog.task_model());
        self.activity.start(
            &request.id,
            request.kind,
            &kind.name,
            &requested,
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
                    crate::paths::display(root)
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
                request
                    .authorizer
                    .clone()
                    .unwrap_or_else(|| self.authorizer.clone()),
                request.claims.clone(),
                request.scope.clone(),
                request.overlay.clone(),
                &request.id,
            )
            .and_then(|tools| {
                let agent =
                    self.build_agent(&kind, tools.clone(), extra, &request.id, model.clone())?;
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
        let (result, spent, text) = CURRENT_AGENT
            .scope(
                request.id.clone(),
                agent.run_detached_events(request.prompt.clone(), cancel, events),
            )
            .await;
        // Its background shell commands end with it; the parent hears which (0050, 0051).
        let stopped = match tools.jobs() {
            Some(jobs) => jobs.close("stopped: the sub-agent ended").await,
            None => Vec::new(),
        };
        progress.attach(None);
        let _ = forward.await;
        outcome.model = served_by(&spent, &requested);
        let (text, citations) = if kind.readonly && !text.trim().is_empty() {
            let root = root.clone();
            let required = kind.name == "explore";
            let original = text.clone();
            tokio::task::spawn_blocking(move || explore::annotate(&root, &text, required))
                .await
                .unwrap_or((original, None))
        } else {
            (text, None)
        };
        outcome.citations = citations;
        if request.detached {
            self.ledger.record_background(&spent);
        } else {
            self.ledger.record(&spent);
        }
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
        if !stopped.is_empty() {
            outcome.output.push_str(&format!(
                "\n[background commands stopped when this sub-agent ended: {}]",
                stopped.join(", ")
            ));
        }
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
    /// A foreground `agent` call of a read-only type: it writes nothing, so a batch of
    /// them can run at once.
    fn parallel_readonly(&self, input: &Value) -> bool {
        let Ok(args) = serde_json::from_value::<AgentArgs>(input.clone()) else {
            return false;
        };
        !args.background
            && args.worktree.is_none()
            && args.isolation.is_none()
            && self
                .resolve_type(&args)
                .is_ok_and(|kind| parallel_kind(&kind))
    }

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
                .spawn_background(kind, args.description, args.prompt, isolated, &cancel)
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
        // Read-only agents of one batch run side by side (`concurrent`), a few at a time.
        let _slot = if parallel_kind(&kind) {
            tokio::select! {
                _ = cancel.cancelled() => return Err(Error::Cancelled),
                slot = self.hub.readonly_slots.acquire() => slot.ok(),
            }
        } else {
            None
        };
        let mut outcome = self
            .hub
            .run(
                RunRequest {
                    id: self.hub.next_id("agent"),
                    kind: "agent",
                    agent_type: kind,
                    description: args.description,
                    prompt: args.prompt,
                    model: None,
                    authorizer: None,
                    detached: false,
                    worktree,
                    claims: None,
                    scope: None,
                    overlay: None,
                },
                cancel,
            )
            .await;
        if let Some(err) = outcome.fatal.take() {
            return Err(err);
        }
        // The parent's context budget is cove's, not the prompt's.
        let dir = self.hub.store.root().join("tool-outputs");
        outcome.output = explore::cap_conclusion(outcome.output.trim(), Some(&dir));
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
            "resume" => self.hub.resume_agent(id, args.message.clone(), false).await,
            "send" => match args.message.as_deref() {
                Some(message) if !message.trim().is_empty() => {
                    self.hub.send_agent(id, message.into()).await
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
                "Delegate a self-contained subtask to a sub-agent (at most {} model calls). Types: {}. \
                 Editing types share your approvals and checkpoint; read-only types never edit. It sees nothing of this conversation: give full context in prompt. \
                 worktree=<branch>: an editing agent works in a worktree made with the worktree tool. background=true returns an id at once \
                 (editing types get their own git worktree; you are notified when it ends; see agent_control). \
                 Result header: [exit, steps, truncated]; answers over 6 KB are cut (full text saved, path given). \
                 explore ({} model, at most {} calls) answers wide read-only questions: where X lives, how it flows, who calls Y, what a change to Z affects \
                 (when to delegate: see the system prompt); foreground read-only agents in one response run in parallel, so split unrelated areas. \
                 Its prompt: the question (not your guess to confirm), facts you have, directions ruled out, and thoroughness (quick, normal or thorough). \
                 It answers with Conclusion, Evidence (path:line) and Not determined: cite the evidence without re-reading it; read only the key lines \
                 before changing code on it or when it contradicts what you know. [未核实] or citations: none means a lead, not a fact.",
                self.hub.settings.max_turns,
                self.hub.types.summary(),
                if self.hub.explore_on_fast() { "fast" } else { "main" },
                explore::EXPLORE_MAX_TURNS.min(self.hub.settings.max_turns)
            ),
            schema: json!({"type":"object","properties":{
                "description":{"type":"string","description":"3-8 word label"},
                "prompt":{"type":"string","description":"Complete task with all needed context; for explore the question, known facts, ruled-out directions and thoroughness (quick/normal/thorough)"},
                "type":{"type":"string","description":"Agent type (default general)"},
                "mode":{"enum":["read","write"],"description":"Legacy alias: read=explore, write=general"},
                "worktree":{"type":"string","description":"Branch of an existing worktree to work in"},
                "background":{"type":"boolean"},
                "isolation":{"enum":["worktree","none"]}
            },"required":["description","prompt"],"additionalProperties":false}),
        });
        definitions.push(ToolDefinition {
            name: "agent_control".into(),
            description: "Manage background agents: list; stop <id> (interrupt, resumable); park <id> (suspend, commit its worktree, free its slot); \
                          resume <id> [message]; send <id> message (delivered at its next tool round; resumes a stopped one). \
                          Only the user merges or discards an agent's work (/status agents merge|discard <id>)."
                .into(),
            schema: json!({"type":"object","properties":{
                "action":{"enum":["list","stop","park","resume","send"]},
                "id":{"type":"string"},"message":{"type":"string"}
            },"required":["action"],"additionalProperties":false}),
        });
        definitions.push(ToolDefinition {
            name: "execute_plan".into(),
            description: "Run the pending items of your latest todo list as sub-agents in dependency order (todo depends). \
                          parallel=true runs independent items concurrently (max_agents 1-8, default 4), never two writing the same file at once. \
                          A failed item is retried once. Afterwards update the todo list from the per-item report. \
                          Items with todo writes may change only those files: a parallel plan whose independent items share files is rejected before it starts, \
                          writes outside them and installing/generating/formatting commands are refused, and each result lists the files written and anything outside. \
                          isolation=\"overlay\" (every item must declare writes): each item works in a private layer and nothing reaches the main workspace until all items end; \
                          items sharing a file run side by side when they name distinct `file::symbol`s, otherwise the later one is stacked on the earlier one (reported). \
                          The layers are then merged into the workspace as one checkpoint (direct, line merge, verified anchor replay, structural merge); a conflicting item redoes \
                          its part once on the merged content, and what still conflicts is left out and listed. Items cannot run builds, tests or file-writing commands \
                          (a `check` tool compiles in a shared slot when configured): build, test and review once after the merge. The user confirms an overlay plan first."
                .into(),
            schema: json!({"type":"object","properties":{
                "parallel":{"type":"boolean"},
                "isolation":{"enum":["main","overlay"],"description":"main (default): items change the main workspace directly; overlay: private layers merged at the end"},
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

    fn jobs(&self) -> Option<std::sync::Arc<crate::tools::jobs::Jobs>> {
        self.inner.jobs()
    }

    /// Foreground agents of read-only types run side by side; editing ones stay serial.
    fn concurrent(&self, name: &str, input: &Value) -> bool {
        if name == "agent" {
            return self.parallel_readonly(input);
        }
        self.inner.concurrent(name, input)
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
    fn types_declare_their_model_tier() {
        let registry = TypeRegistry::from_config(&json!({"agent_types":{
            "scout":{"description":"x","instructions":"y","readonly":true,"model":"fast"},
            "pinned":{"description":"x","instructions":"y","model":"claude-haiku-4-5"},
            "plain":{"description":"x","instructions":"y"}
        }}))
        .unwrap();
        let tier = |name: &str, explore_fast| tier_for(registry.get(name).unwrap(), explore_fast);
        assert_eq!(tier("explore", true), ModelTier::Fast);
        assert_eq!(tier("explore", false), ModelTier::Main);
        for name in ["plan", "review", "verify"] {
            assert_eq!(tier(name, true), ModelTier::Main, "{name}");
        }
        assert_eq!(tier("general", true), ModelTier::Inherit);
        assert_eq!(tier("test", true), ModelTier::Inherit);
        assert_eq!(tier("scout", true), ModelTier::Fast);
        // explore_model only concerns explore.
        assert_eq!(tier("scout", false), ModelTier::Fast);
        assert_eq!(
            tier("pinned", true),
            ModelTier::Named("claude-haiku-4-5".into())
        );
        assert_eq!(tier("plain", true), ModelTier::Inherit);
        assert_eq!(
            registry.get("explore").unwrap().max_turns,
            Some(explore::EXPLORE_MAX_TURNS)
        );
        assert!(
            TypeRegistry::from_config(&json!({"agent_types":{
                "bad":{"description":"x","instructions":"y","model":"two words"}
            }}))
            .is_err()
        );
        // Only read-only types without regression run side by side.
        assert!(parallel_kind(registry.get("explore").unwrap()));
        assert!(parallel_kind(registry.get("review").unwrap()));
        assert!(!parallel_kind(registry.get("verify").unwrap()));
        assert!(!parallel_kind(registry.get("general").unwrap()));
    }

    #[test]
    fn the_result_names_the_model_and_the_evidence_check() {
        let mut spent = Accounting::default();
        assert_eq!(served_by(&spent, "fast-model"), "fast-model");
        spent.by_model.insert(
            "b-model".into(),
            crate::session::ModelSpend {
                calls: 1,
                ..Default::default()
            },
        );
        spent.by_model.insert(
            "a-model".into(),
            crate::session::ModelSpend {
                calls: 3,
                ..Default::default()
            },
        );
        assert_eq!(served_by(&spent, "x"), "a-model + b-model");
        let outcome = RunOutcome {
            id: "agent-1".into(),
            agent_type: "explore".into(),
            description: "find callers".into(),
            exit: Exit::Completed,
            steps: 4,
            truncated: false,
            output: "Conclusion: two callers.".into(),
            spent: Accounting {
                cost: Some(0.04),
                ..Accounting::default()
            },
            tools_failed: 0,
            readonly: true,
            model: "deepseek-v4-flash".into(),
            citations: Some("citations: 2 ok, 0 invalid".into()),
            fatal: None,
        };
        let text = outcome.render();
        let lines: Vec<&str> = text.lines().collect();
        assert!(
            lines[0].starts_with("[sub-agent agent-1 (explore)"),
            "{text}"
        );
        assert_eq!(lines[1], "citations: 2 ok, 0 invalid");
        assert_eq!(
            *lines.last().unwrap(),
            "[cost: $0.0400, model: deepseek-v4-flash, steps: 4, failed tools: 0]"
        );
    }

    #[test]
    fn exits_map_to_lifecycle_states() {
        assert_eq!(Exit::Completed.lifecycle(), Lifecycle::Finished);
        assert_eq!(Exit::MaxIterations.lifecycle(), Lifecycle::Interrupted);
        assert_eq!(Exit::Unverified.lifecycle(), Lifecycle::Failed);
        assert_eq!(Exit::Stalled.lifecycle(), Lifecycle::Interrupted);
        let stopped = |stop| {
            Err(Error::Stopped(Box::new(crate::engine::agent::Stopped {
                stop,
                report: crate::engine::agent::TaskReport::default(),
            })))
        };
        assert_eq!(
            Exit::from_result(&stopped(crate::engine::agent::Stop::Limit(20))),
            Exit::MaxIterations
        );
        assert_eq!(
            Exit::from_result(&stopped(crate::engine::agent::Stop::Stalled(12))),
            Exit::Stalled
        );
        // Other limits (the task deadline) are errors, not a used-up quota.
        assert!(matches!(
            Exit::from_result(&Err(Error::Limit("任务超过时限".into()))),
            Exit::Error(_)
        ));
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
