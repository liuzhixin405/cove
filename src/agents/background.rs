//! Durable background sub-agents.
//!
//! A background agent is a tokio task running a normal kernel loop over its own persisted
//! session (`Store`: atomic snapshots, journaled tool calls, uncertain calls never
//! replayed), usually inside its own git worktree under
//! `<data>/worktrees/<project-hash>/<id>` on branch `cove/agent-<id8>`. Its lifecycle is the
//! typed state machine in `lifecycle.rs`, persisted as an `AgentRecord`; after a restart a
//! record left `running` is `interrupted` and can be resumed from its transcript.
//!
//! Approvals: background agents use the session's permission mode. In ask mode their
//! requests are queued and asked on the terminal between tasks; without a terminal they
//! are denied. They are never approved silently.

use std::{
    collections::{HashMap, VecDeque},
    path::PathBuf,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};

use futures_util::future::BoxFuture;
use serde_json::Value;
use tokio::sync::{Notify, oneshot, watch};
use tokio_util::sync::CancellationToken;

use crate::{
    agents::lifecycle::{AgentRecord, Kind, Lifecycle, Transition, project_key},
    agents::subagent::{AgentHub, AgentType, Exit, clip},
    agents::worktree,
    domain::{Error, Result},
    engine::agent::last_answer,
    engine::turn::TurnHook,
    safety::permission::{Authorizer, Decision, Effect, Mode},
    session::workspace::Workspace,
    session::{Accounting, CallState, Session, SessionPersistence, State, new_id, now},
    tools::ToolOutput,
};

/// Background approval requests waiting for the user.
pub struct ApprovalQueue {
    pending: Mutex<VecDeque<PendingApproval>>,
    /// Whether a terminal can answer; otherwise ask-mode requests are denied at once.
    pub interactive: bool,
    pub notify: Arc<Notify>,
}

pub struct PendingApproval {
    pub agent: String,
    pub description: String,
    reply: oneshot::Sender<bool>,
}

impl PendingApproval {
    pub fn answer(self, approved: bool) {
        let _ = self.reply.send(approved);
    }
}

impl ApprovalQueue {
    pub fn new(interactive: bool, notify: Arc<Notify>) -> Self {
        Self {
            pending: Mutex::default(),
            interactive,
            notify,
        }
    }

    pub fn take_all(&self) -> Vec<PendingApproval> {
        self.pending
            .lock()
            .map(|mut pending| pending.drain(..).collect())
            .unwrap_or_default()
    }

    pub fn descriptions(&self) -> Vec<String> {
        self.pending.lock().map_or(Vec::new(), |pending| {
            pending
                .iter()
                .map(|request| format!("{}: {}", request.agent, request.description))
                .collect()
        })
    }
}

pub fn describe(effect: &Effect) -> String {
    effect.describe()
}

/// The session's permission mode for one background agent.
pub struct BackgroundAuthorizer {
    pub mode: Mode,
    /// The agent's own tree (its worktree); "inside the project" is judged against it.
    pub root: std::path::PathBuf,
    pub queue: Arc<ApprovalQueue>,
    pub agent: String,
}

impl Authorizer for BackgroundAuthorizer {
    fn authorize(&self, effect: Effect, cancel: CancellationToken) -> BoxFuture<'_, Result<()>> {
        Box::pin(async move {
            if cancel.is_cancelled() {
                return Err(Error::Cancelled);
            }
            // Same decision as the foreground task, judged in the agent's own tree.
            let roots = [self.root.clone()];
            let decision = crate::safety::permission::decide(
                self.mode,
                &effect,
                &[],
                &self.root,
                Some(&roots),
                crate::safety::permission::shell::Dialect::host(),
            );
            let description = describe(&effect);
            match decision {
                Decision::Allow => Ok(()),
                Decision::Deny(reason) => Err(Error::Approval(format!("{description}: {reason}"))),
                Decision::Ask if !self.queue.interactive => Err(Error::Approval(format!(
                    "{description}；后台 agent 没有交互终端，无法询问（可用 --permission auto 或 bypass 授权）"
                ))),
                Decision::Ask => {
                    let (reply, answer) = oneshot::channel();
                    if let Ok(mut pending) = self.queue.pending.lock() {
                        pending.push_back(PendingApproval {
                            agent: self.agent.clone(),
                            description: description.clone(),
                            reply,
                        });
                    }
                    self.queue.notify.notify_one();
                    tokio::select! {
                        _ = cancel.cancelled() => Err(Error::Cancelled),
                        approved = answer => match approved {
                            Ok(true) => Ok(()),
                            Ok(false) => Err(Error::Denied(description)),
                            Err(_) => Err(Error::Approval(description)),
                        },
                    }
                }
            }
        })
    }
}

struct Running {
    cancel: CancellationToken,
    park: Arc<AtomicBool>,
    done: watch::Receiver<bool>,
    budget: Option<f64>,
}

struct Notice {
    text: String,
    user_seen: bool,
    model_seen: bool,
}

#[derive(Default)]
pub struct BackgroundState {
    running: Mutex<HashMap<String, Running>>,
    notices: Mutex<Vec<Notice>>,
    /// Wakes the REPL for notices and approval requests.
    pub notify: Arc<Notify>,
}

/// Parent-side delivery: finished-agent notices and messages for the main task reach the
/// model at its next tool round (or at a new task's start).
pub struct NoticeHook {
    pub hub: Arc<AgentHub>,
}

pub const MAIN: &str = "main";

impl NoticeHook {
    fn note(&self) -> Option<String> {
        let mut parts = Vec::new();
        if let Some(notices) = self.hub.notices_for_model() {
            parts.push(notices);
        }
        let messages = self.hub.inbox.take(MAIN);
        if !messages.is_empty() {
            parts.push(format!(
                "[message from the user, delivered now]\n{}",
                messages.join("\n---\n")
            ));
        }
        (!parts.is_empty()).then(|| parts.join("\n\n"))
    }
}

impl TurnHook for NoticeHook {
    fn task_note<'a>(&'a self, _session: &'a Session) -> BoxFuture<'a, Option<String>> {
        Box::pin(async move { self.note() })
    }

    fn tool_note(&self, _name: &str, _input: &Value, _output: &ToolOutput) -> Option<String> {
        self.note()
    }

    fn finish_nudge<'a>(&'a self, _session: &'a Session) -> BoxFuture<'a, Option<String>> {
        Box::pin(async move {
            let messages = self.hub.inbox.take(MAIN);
            (!messages.is_empty()).then(|| {
                format!(
                    "[message from the user, delivered now]\n{}",
                    messages.join("\n---\n")
                )
            })
        })
    }
}

/// Spend in `now` that `before` did not include.
fn delta(now: &Accounting, before: &Accounting) -> Accounting {
    Accounting {
        usage: crate::domain::Usage {
            input: now.usage.input.saturating_sub(before.usage.input),
            output: now.usage.output.saturating_sub(before.usage.output),
            cache_read: now.usage.cache_read.saturating_sub(before.usage.cache_read),
            cache_write: now
                .usage
                .cache_write
                .saturating_sub(before.usage.cache_write),
        },
        model_calls: now.model_calls.saturating_sub(before.model_calls),
        cost: now
            .cost
            .zip(before.cost)
            .map(|(now, before)| (now - before).max(0.0)),
        ..Default::default()
    }
}

const BACKGROUND_NOTE: &str = "You run in the background: nobody watches you live and you cannot ask the user. \
    Work autonomously within the assigned scope and finish with a self-contained report (what changed, checks and their results, what remains).";

impl AgentHub {
    pub fn is_live(&self, id: &str) -> bool {
        self.background
            .running
            .lock()
            .is_ok_and(|running| running.contains_key(id))
    }

    pub fn running_count(&self) -> usize {
        self.background
            .running
            .lock()
            .map_or(0, |running| running.len())
    }

    pub fn records(&self) -> Result<Vec<AgentRecord>> {
        self.records.list(&|id| self.is_live(id))
    }

    pub fn find_record(&self, id: &str) -> Result<AgentRecord> {
        self.records.find(id, &|id| self.is_live(id))
    }

    fn push_notice(&self, text: String) {
        if let Ok(mut notices) = self.background.notices.lock() {
            notices.push(Notice {
                text,
                user_seen: false,
                model_seen: false,
            });
            notices.retain(|notice| !(notice.user_seen && notice.model_seen));
            let excess = notices.len().saturating_sub(32);
            notices.drain(..excess);
        }
        self.background.notify.notify_one();
    }

    pub fn notices_for_model(&self) -> Option<String> {
        let mut notices = self.background.notices.lock().ok()?;
        let text: Vec<String> = notices
            .iter_mut()
            .filter(|notice| !notice.model_seen)
            .map(|notice| {
                notice.model_seen = true;
                notice.text.clone()
            })
            .collect();
        (!text.is_empty()).then(|| text.join("\n\n"))
    }

    pub fn notices_for_user(&self) -> Vec<String> {
        self.background
            .notices
            .lock()
            .map_or(Vec::new(), |mut notices| {
                notices
                    .iter_mut()
                    .filter(|notice| !notice.user_seen)
                    .map(|notice| {
                        notice.user_seen = true;
                        notice.text.clone()
                    })
                    .collect()
            })
    }

    fn reserved(&self) -> f64 {
        self.background.running.lock().map_or(0.0, |running| {
            running.values().filter_map(|run| run.budget).sum()
        })
    }

    /// Budget for a new background run: what is left of the session, minus what running
    /// background agents may still spend.
    fn allot(&self, remaining: Option<f64>) -> Result<Option<f64>> {
        let Some(remaining) = remaining else {
            return Ok(None);
        };
        let left = remaining - self.reserved();
        if left <= 0.0 {
            return Err(Error::Limit(
                "no session budget left for another background agent".into(),
            ));
        }
        Ok(Some(left))
    }

    pub async fn spawn_background(
        self: &Arc<Self>,
        kind: AgentType,
        description: String,
        prompt: String,
        isolated: bool,
        remaining: Option<f64>,
        cancel: &CancellationToken,
    ) -> Result<String> {
        if self.running_count() >= self.settings.max_background {
            return Err(Error::Limit(format!(
                "{} background agents are already running (max_background_agents); stop, park or wait for one",
                self.settings.max_background
            )));
        }
        let budget = self.allot(remaining)?;
        let id = new_id();
        let short = id[..8].to_owned();
        let (worktree, branch, base) = if isolated {
            let base = worktree::head(&self.root, cancel).await.map_err(|err| {
                Error::Tool(format!(
                    "background isolation needs a git repository with a commit: {err}"
                ))
            })?;
            let path = self
                .store
                .root()
                .join("worktrees")
                .join(project_key(&self.root))
                .join(&id);
            std::fs::create_dir_all(path.parent().unwrap_or(&path))?;
            let branch = format!("cove/agent-{short}");
            self.authorizer
                .authorize(
                    Effect::Shell(format!(
                        "git worktree add -b {branch} {} {base:.10} (background agent)",
                        path.display()
                    )),
                    cancel.clone(),
                )
                .await?;
            worktree::add_worktree(&self.root, &path, &branch, &base, cancel).await?;
            (Some(path.canonicalize()?), Some(branch), Some(base))
        } else {
            (None, None, None)
        };
        let mut session = Session::new(
            worktree.clone().unwrap_or_else(|| self.root.clone()),
            self.settings.protocol.clone(),
            self.settings.model.clone(),
            self.settings.prices,
        );
        session.id = id.clone();
        if let Some(path) = &worktree {
            let workspace = Workspace::new(path.clone(), self.store.clone())?;
            let checkpoint = workspace.begin()?;
            session.checkpoints.push(checkpoint.clone());
            session.current_checkpoint = Some(checkpoint);
        }
        self.store.save(&session)?;
        let record = AgentRecord {
            version: 1,
            id: id.clone(),
            kind: Kind::Background,
            agent_type: kind.name.clone(),
            description: clip(&description, 120),
            task: clip(&prompt, 400),
            state: Lifecycle::Running,
            reason: "started".into(),
            session_id: id.clone(),
            project: self.root.clone(),
            worktree: worktree.clone(),
            branch: branch.clone(),
            base,
            created: now(),
            updated: now(),
            report: String::new(),
            last_step: "starting".into(),
            reported: session.accounting.clone(),
            merged: false,
        };
        self.records.save(&record)?;
        self.launch(record, Some(prompt), budget)?;
        Ok(format!(
            "Background agent {short} ({}) started{}. It runs without blocking you; you will get a notice with its report when it ends. \
             Manage it with agent_control (list/stop/park/resume/send) using id {short}. The user merges or discards its work with /status agents merge|discard {short}.",
            kind.name,
            match (&worktree, &branch) {
                (Some(path), Some(branch)) =>
                    format!(" in worktree {} on branch {branch}", path.display()),
                _ => " (read-only, main tree)".into(),
            }
        ))
    }

    fn launch(
        self: &Arc<Self>,
        record: AgentRecord,
        prompt: Option<String>,
        budget: Option<f64>,
    ) -> Result<()> {
        let cancel = CancellationToken::new();
        let park = Arc::new(AtomicBool::new(false));
        let (done_sender, done) = watch::channel(false);
        {
            let mut running = self
                .background
                .running
                .lock()
                .map_err(|_| Error::Session("background lock poisoned".into()))?;
            if running.contains_key(&record.id) {
                return Err(Error::Session("agent is already running".into()));
            }
            running.insert(
                record.id.clone(),
                Running {
                    cancel: cancel.clone(),
                    park: park.clone(),
                    done,
                    budget,
                },
            );
        }
        let hub = self.clone();
        tokio::spawn(async move {
            let id = record.id.clone();
            hub.drive(record, prompt, budget, cancel, park).await;
            if let Ok(mut running) = hub.background.running.lock() {
                running.remove(&id);
            }
            let _ = done_sender.send(true);
            hub.background.notify.notify_one();
        });
        Ok(())
    }

    async fn drive(
        self: &Arc<Self>,
        mut record: AgentRecord,
        prompt: Option<String>,
        budget: Option<f64>,
        cancel: CancellationToken,
        park: Arc<AtomicBool>,
    ) {
        let short = record.short().to_owned();
        let outcome = self.drive_inner(&mut record, prompt, budget, cancel).await;
        let (transition, reason, failed_tools) = match outcome {
            Ok((exit, failed)) => {
                let transition = match exit.lifecycle() {
                    Lifecycle::Finished => Transition::Finish,
                    Lifecycle::Failed => Transition::Fail,
                    _ if park.load(Ordering::SeqCst) => Transition::Park,
                    _ => Transition::Interrupt,
                };
                let reason = match exit.detail() {
                    Some(detail) => format!("{}: {}", exit.label(), clip(detail, 200)),
                    None => exit.label().to_owned(),
                };
                (transition, reason, failed)
            }
            Err(err) => (Transition::Fail, clip(&err.to_string(), 300), 0),
        };
        let _ = record.transition(transition, reason.clone());
        if let Some(activity) = self.activity.get(&short) {
            record.last_step = activity.last_step;
        }
        let changes = self.change_summary(&record).await;
        let _ = self.records.save(&record);
        self.activity.finish(&short, record.state);
        if record.state != Lifecycle::Paused {
            self.push_notice(format!(
                "[background agent {short} ({}) {}: {reason}; failed tools: {failed_tools}; {changes}]\n{}",
                record.agent_type,
                record.state.label(),
                if record.report.trim().is_empty() {
                    "(no report)"
                } else {
                    record.report.trim()
                }
            ));
        }
    }

    /// Runs the agent; returns its exit and failed tool count.
    async fn drive_inner(
        self: &Arc<Self>,
        record: &mut AgentRecord,
        prompt: Option<String>,
        budget: Option<f64>,
        cancel: CancellationToken,
    ) -> Result<(Exit, u32)> {
        let _lease = self.store.acquire(&record.session_id)?;
        let mut session = self.store.load(&record.session_id)?;
        let mut kind = self.types.get(&record.agent_type)?.clone();
        let short = record.short().to_owned();
        let (root, workspace) = match &record.worktree {
            Some(path) => {
                if !path.is_dir() {
                    return Err(Error::Workspace(format!(
                        "worktree {} no longer exists",
                        path.display()
                    )));
                }
                let workspace = Arc::new(Workspace::new(path.clone(), self.store.clone())?);
                workspace.activate(session.current_checkpoint.as_deref())?;
                (path.clone(), workspace)
            }
            None => {
                // Read-only in the shared main tree: no regression swaps under the user.
                kind.tools.retain(|tool| tool != "regression");
                kind.requires_regression = false;
                let workspace = Arc::new(Workspace::new(self.root.clone(), self.store.clone())?);
                (self.root.clone(), workspace)
            }
        };
        self.activity.start(
            &short,
            "background",
            &kind.name,
            &self.settings.model,
            &record.description,
        );
        let authorizer = Arc::new(BackgroundAuthorizer {
            mode: self.permission,
            root: root.clone(),
            queue: self.approvals.clone(),
            agent: format!("agent {short}"),
        });
        let (progress, events, forward) = self.forward_events(&short);
        let mut extra = BACKGROUND_NOTE.to_owned();
        if let (Some(path), Some(branch)) = (&record.worktree, &record.branch) {
            extra.push_str(&format!(
                " You work in the git worktree {} on branch {branch}, isolated from the user's tree. Commit each finished step there \
                 (git add -A; git commit -m \"...\") so your progress survives interruption; the user decides whether to merge it.",
                path.display()
            ));
        }
        let tools = self.scoped_tools(
            &kind,
            &root,
            workspace,
            progress.clone(),
            authorizer,
            None,
            &short,
        )?;
        let agent = self.build_agent(&kind, tools.clone(), budget, Some(extra), &record.id)?;
        let result = agent
            .run_in(&mut session, prompt, &self.store, events, cancel)
            .await;
        progress.attach(None);
        let _ = forward.await;
        let spent = delta(&session.accounting, &record.reported);
        self.ledger.record_background(&spent);
        record.reported = session.accounting.clone();
        record.report = clip_report(&last_answer(&session));
        let mut exit = Exit::from_result(&result);
        if exit == Exit::Completed
            && kind.requires_regression
            && !tools.regression_passed.load(Ordering::SeqCst)
        {
            exit = Exit::Unverified;
        }
        Ok((exit, session.task_failures))
    }

    /// "no changes", or the dirty state and commit count of an agent's worktree.
    pub async fn change_summary(&self, record: &AgentRecord) -> String {
        let (Some(path), Some(branch), Some(base)) =
            (&record.worktree, &record.branch, &record.base)
        else {
            return "no worktree".into();
        };
        if !path.is_dir() {
            return "worktree removed".into();
        }
        let cancel = CancellationToken::new();
        let dirty = worktree::status(path, &cancel).await;
        let commits = worktree::commits_since(path, base, branch, &cancel).await;
        match (dirty, commits) {
            (Ok(dirty), Ok(0)) if dirty.is_empty() => "worktree has no changes".into(),
            (Ok(dirty), Ok(commits)) => format!(
                "branch {branch}: {commits} commit(s), {}",
                if dirty.is_empty() {
                    "clean".to_owned()
                } else {
                    format!("{} uncommitted change(s)", dirty.lines().count())
                }
            ),
            (Err(err), _) | (_, Err(err)) => format!("worktree state unknown: {err}"),
        }
    }

    async fn wait_stopped(&self, id: &str) -> Result<()> {
        let done = self
            .background
            .running
            .lock()
            .ok()
            .and_then(|running| running.get(id).map(|run| run.done.clone()));
        if let Some(mut done) = done {
            let wait = async {
                while !*done.borrow() {
                    if done.changed().await.is_err() {
                        break;
                    }
                }
            };
            tokio::time::timeout(Duration::from_secs(30), wait)
                .await
                .map_err(|_| Error::Session("agent did not stop within 30 seconds".into()))?;
        }
        Ok(())
    }

    /// Interrupts a running agent at its next safe point: the model request is cancelled
    /// and an in-flight tool call ends journaled (finished, or uncertain).
    pub async fn stop_agent(&self, id: &str, park: bool) -> Result<String> {
        let record = self.find_record(id)?;
        let running = self.background.running.lock().ok().and_then(|running| {
            running
                .get(&record.id)
                .map(|run| (run.cancel.clone(), run.park.clone()))
        });
        let Some((cancel, park_flag)) = running else {
            return Err(Error::Session(format!(
                "agent {} is {}, not running",
                record.short(),
                record.state.label()
            )));
        };
        park_flag.store(park, Ordering::SeqCst);
        cancel.cancel();
        self.wait_stopped(&record.id).await?;
        let record = self.records.load(&record.id)?;
        Ok(format!(
            "agent {} is now {} ({}); resume it with resume {}",
            record.short(),
            record.state.label(),
            record.reason,
            record.short()
        ))
    }

    /// Parks an agent: stops it if running, commits its worktree changes to its branch and
    /// frees its slot. Parked agents survive restarts.
    pub async fn park_agent(&self, id: &str, cancel: &CancellationToken) -> Result<String> {
        let record = self.find_record(id)?;
        if record.kind == Kind::Main {
            return Err(Error::Session(
                "主任务请用 /status agents park main 挂起".into(),
            ));
        }
        if self.is_live(&record.id) {
            self.stop_agent(&record.id, true).await?;
        }
        let mut record = self.records.load(&record.id)?;
        let mut committed = false;
        if let Some(path) = record.worktree.clone().filter(|path| path.is_dir())
            && !worktree::status(&path, cancel).await?.is_empty()
        {
            self.authorizer
                .authorize(
                    Effect::Shell(format!(
                        "git commit -A in agent worktree {} (park)",
                        path.display()
                    )),
                    cancel.clone(),
                )
                .await?;
            committed = worktree::commit_all(
                &path,
                &format!("cove: park agent {}", record.short()),
                cancel,
            )
            .await?;
        }
        if record.state != Lifecycle::Paused {
            record.transition(Transition::Park, "parked")?;
        }
        self.records.save(&record)?;
        Ok(format!(
            "agent {} parked{}; resume {} wakes it",
            record.short(),
            if committed {
                " (uncommitted changes committed to its branch)"
            } else {
                ""
            },
            record.short()
        ))
    }

    /// Wakes a stopped agent from its transcript, optionally with a follow-up message.
    /// Uncertain tool calls are never replayed: `confirm` records them as failed with
    /// unknown outcome after the user inspected the effects.
    pub async fn resume_agent(
        self: &Arc<Self>,
        id: &str,
        message: Option<String>,
        confirm: bool,
        remaining: Option<f64>,
    ) -> Result<String> {
        let mut record = self.find_record(id)?;
        if record.kind == Kind::Main {
            return Err(Error::Session(
                "挂起的主任务请用 /status agents resume <id> 唤醒".into(),
            ));
        }
        if self.is_live(&record.id) {
            return Err(Error::Session(format!(
                "agent {} is running; use send to queue a message",
                record.short()
            )));
        }
        if !record.state.resumable() {
            return Err(Error::Session(format!(
                "agent {} is {} and cannot be resumed",
                record.short(),
                record.state.label()
            )));
        }
        if self.running_count() >= self.settings.max_background {
            return Err(Error::Limit(
                "max_background_agents are running; stop or park one first".into(),
            ));
        }
        if record.worktree.as_ref().is_some_and(|path| !path.is_dir()) {
            return Err(Error::Workspace(
                "the agent's worktree no longer exists".into(),
            ));
        }
        let budget = self.allot(remaining)?;
        let mut session = self.store.load(&record.session_id)?;
        let uncertain: Vec<String> = session
            .pending
            .iter()
            .filter(|call| call.state == CallState::Uncertain)
            .map(|call| format!("{} {}", call.id, call.name))
            .collect();
        if !uncertain.is_empty() {
            if !confirm {
                return Err(Error::Session(format!(
                    "agent {} has tool calls with unknown outcome ({}); inspect its worktree, then run /status agents resume {} confirm \
                     to record them as failed (they are never replayed)",
                    record.short(),
                    uncertain.join(", "),
                    record.short()
                )));
            }
            for call in &mut session.pending {
                if call.state == CallState::Uncertain {
                    call.state = CallState::Finished;
                    call.result = Some(ToolOutput {
                        content: "Interrupted with unknown outcome; the operator confirmed resuming without replay. Inspect the effects before relying on them.".into(),
                        failed: true, ..Default::default() });
                    session.task_failures += 1;
                }
            }
            self.store.save(&session)?;
        }
        let prompt = match session.state {
            State::Interrupted(_) | State::Running => {
                if let Some(message) = message {
                    self.inbox.push(&record.id, message);
                }
                None
            }
            State::Completed | State::Idle => Some(message.unwrap_or_else(|| {
                "Continue the task where you left off, then report what is done.".into()
            })),
        };
        record.transition(Transition::Resume, "resumed")?;
        self.records.save(&record)?;
        self.launch(record.clone(), prompt, budget)?;
        Ok(format!(
            "agent {} resumed from its transcript ({} messages)",
            record.short(),
            session.messages.len()
        ))
    }

    /// Queues `message` for a running agent (next tool round) or resumes a stopped one with it.
    pub async fn send_agent(
        self: &Arc<Self>,
        id: &str,
        message: String,
        remaining: Option<f64>,
    ) -> Result<String> {
        let record = self.find_record(id)?;
        if record.kind == Kind::Main {
            return Err(Error::Session(
                "给主任务发消息请用 /status agents send main <text>".into(),
            ));
        }
        if self.is_live(&record.id) {
            self.inbox.push(&record.id, message);
            return Ok(format!(
                "message queued for running agent {}; it is delivered at its next tool round ({} queued)",
                record.short(),
                self.inbox.pending(&record.id)
            ));
        }
        self.resume_agent(&record.id, Some(message), false, remaining)
            .await
    }

    /// Applies the agent branch to the user's tree (explicit user request only).
    pub async fn merge_agent(&self, id: &str, cancel: &CancellationToken) -> Result<String> {
        let mut record = self.find_record(id)?;
        let (Some(path), Some(branch), Some(base)) = (
            record.worktree.clone(),
            record.branch.clone(),
            record.base.clone(),
        ) else {
            return Err(Error::Session(
                "这个 agent 没有可合并的 worktree 分支".into(),
            ));
        };
        if self.is_live(&record.id) {
            return Err(Error::Session(
                "请先用 /status agents stop 或 park 停下这个 agent，再合并".into(),
            ));
        }
        if record.state == Lifecycle::Discarded {
            return Err(Error::Session("这个 agent 已经被丢弃或合并过了".into()));
        }
        self.authorizer
            .authorize(
                Effect::Shell(format!(
                    "merge agent branch {branch} into {} (git merge)",
                    self.root.display()
                )),
                cancel.clone(),
            )
            .await?;
        if path.is_dir() {
            worktree::commit_all(
                &path,
                &format!("cove: agent {} work before merge", record.short()),
                cancel,
            )
            .await?;
        }
        if worktree::commits_since(&self.root, &base, &branch, cancel).await? == 0 {
            return Ok(format!(
                "agent {}：worktree 没有改动，无需合并（可用 /status agents discard 清理）",
                record.short()
            ));
        }
        match worktree::merge_branch(&self.root, &branch, Some(&base), cancel).await? {
            Err(report) => Err(Error::Tool(report)),
            Ok(text) => {
                if path.is_dir() {
                    worktree::remove_worktree(&self.root, &path, false, cancel).await?;
                }
                worktree::delete_branch(&self.root, &branch, false, cancel).await?;
                record.merged = true;
                record.transition(Transition::Discard, "merged into the main tree")?;
                self.records.save(&record)?;
                Ok(format!(
                    "agent {} 已合并：{text}\n已删除 worktree 和分支。/undo 不覆盖合并进来的提交，如需撤销请用 git revert。",
                    record.short()
                ))
            }
        }
    }

    /// Removes the agent's worktree and branch (explicit user request only).
    pub async fn discard_agent(&self, id: &str, cancel: &CancellationToken) -> Result<String> {
        let mut record = self.find_record(id)?;
        if self.is_live(&record.id) {
            return Err(Error::Session(
                "请先用 /status agents stop 停下这个 agent，再丢弃".into(),
            ));
        }
        if record.kind == Kind::Main {
            return Err(Error::Session(
                "挂起的主任务不能在这里丢弃；可用 /status agents resume <id> 唤醒它，或保留不动"
                    .into(),
            ));
        }
        if record.state == Lifecycle::Discarded {
            return Err(Error::Session("这个 agent 已经丢弃过了".into()));
        }
        if let (Some(path), Some(branch)) = (record.worktree.clone(), record.branch.clone()) {
            self.authorizer
                .authorize(
                    Effect::Shell(format!(
                        "discard agent {}: git worktree remove --force {} and git branch -D {branch}",
                        record.short(),
                        path.display()
                    )),
                    cancel.clone(),
                )
                .await?;
            if path.is_dir() {
                worktree::remove_worktree(&self.root, &path, true, cancel).await?;
            }
            let _ = worktree::delete_branch(&self.root, &branch, true, cancel).await;
        }
        record.transition(Transition::Discard, "discarded by the user")?;
        self.records.save(&record)?;
        Ok(format!("agent {} 已丢弃", record.short()))
    }

    /// Records the foreground session as a parked main task.
    pub fn park_main(&self, session: &Session) -> Result<AgentRecord> {
        let state = match session.state {
            State::Running => {
                return Err(Error::Session(
                    "任务正在运行，不能挂起；请等它结束或先按 Ctrl+C 中断".into(),
                ));
            }
            State::Interrupted(_) => Lifecycle::Interrupted,
            State::Completed | State::Idle => Lifecycle::Finished,
        };
        let mut record = AgentRecord {
            version: 1,
            id: session.id.clone(),
            kind: Kind::Main,
            agent_type: "main".into(),
            description: clip(&session.current_request, 120),
            task: clip(&session.current_request, 400),
            state,
            reason: String::new(),
            session_id: session.id.clone(),
            project: self.root.clone(),
            worktree: None,
            branch: None,
            base: None,
            created: now(),
            updated: now(),
            report: clip_report(&last_answer(session)),
            last_step: format!("{} pending tool calls", session.pending.len()),
            reported: session.accounting.clone(),
            merged: false,
        };
        record.transition(Transition::Park, "parked by the user")?;
        self.records.save(&record)?;
        Ok(record)
    }

    /// Removes a parked main record once its session is the foreground session again.
    pub fn wake_main(&self, id: &str) -> Result<AgentRecord> {
        let mut record = self.find_record(id)?;
        if record.kind != Kind::Main {
            return Err(Error::Session("这不是挂起的主任务".into()));
        }
        record.transition(Transition::Resume, "woken")?;
        let path = self
            .store
            .root()
            .join("agents")
            .join(project_key(&self.root))
            .join(format!("{}.json", record.id));
        std::fs::remove_file(path)?;
        Ok(record)
    }

    /// Cancels every running background agent and waits (bounded) until their state is saved.
    pub async fn stop_all(&self) {
        let ids: Vec<String> = self
            .background
            .running
            .lock()
            .map(|running| {
                running
                    .iter()
                    .map(|(id, run)| {
                        run.cancel.cancel();
                        id.clone()
                    })
                    .collect()
            })
            .unwrap_or_default();
        for id in ids {
            let _ = self.wait_stopped(&id).await;
        }
        for approval in self.approvals.take_all() {
            approval.answer(false);
        }
    }

    /// Waits until no background agent runs, up to `limit`; then stops the rest.
    pub async fn wait_all(&self, limit: Duration, mut on_notice: impl FnMut(String)) {
        let deadline = tokio::time::Instant::now() + limit;
        loop {
            for notice in self.notices_for_user() {
                on_notice(notice);
            }
            for approval in self.approvals.take_all() {
                approval.answer(false);
            }
            if self.running_count() == 0 {
                return;
            }
            let notified = self.background.notify.notified();
            if tokio::time::timeout_at(deadline, notified).await.is_err() {
                self.stop_all().await;
                for notice in self.notices_for_user() {
                    on_notice(notice);
                }
                return;
            }
        }
    }

    /// `/status agents` and `agent_control list`.
    pub async fn status_text(&self, main: Option<String>) -> String {
        let mut text = String::new();
        if let Some(main) = main {
            text.push_str(&main);
            text.push('\n');
        }
        let records = self.records().unwrap_or_default();
        for record in &records {
            if record.state == Lifecycle::Discarded && now().saturating_sub(record.updated) > 86_400
            {
                continue;
            }
            let live = self.activity.get(record.short());
            let duration = live
                .as_ref()
                .map(|entry| {
                    entry
                        .ended
                        .unwrap_or_else(std::time::Instant::now)
                        .duration_since(entry.started)
                        .as_secs()
                })
                .map_or_else(String::new, |secs| format!(" {secs}s"));
            let last = live
                .as_ref()
                .map_or(record.last_step.clone(), |entry| entry.last_step.clone());
            let changes = if record.kind == Kind::Background && record.state != Lifecycle::Discarded
            {
                self.change_summary(record).await
            } else if record.merged {
                "merged".into()
            } else {
                String::new()
            };
            text.push_str(&format!(
                "{} {:<10} {:<11} {:<8} model={}{duration} cost={} task=\"{}\" last=\"{}\"{}{}\n",
                record.short(),
                if record.kind == Kind::Main {
                    "main(parked)".to_owned()
                } else {
                    format!("bg:{}", record.agent_type)
                },
                record.state.label(),
                clip(&record.reason, 40),
                self.settings.model,
                record
                    .reported
                    .cost
                    .map_or("unknown".into(), |cost| format!("${cost:.4}")),
                clip(&record.description, 60),
                clip(&last, 60),
                record
                    .branch
                    .as_ref()
                    .map_or(String::new(), |branch| format!(" branch={branch}")),
                if changes.is_empty() {
                    String::new()
                } else {
                    format!(" ({changes})")
                }
            ));
        }
        let background: Vec<String> = records
            .iter()
            .map(|record| record.short().to_owned())
            .collect();
        for entry in self
            .activity
            .snapshot()
            .into_iter()
            .filter(|entry| !background.contains(&entry.id))
        {
            let secs = entry
                .ended
                .unwrap_or_else(std::time::Instant::now)
                .duration_since(entry.started)
                .as_secs();
            text.push_str(&format!(
                "{} {:<10} {:<11} {:<8} model={} {secs}s task=\"{}\" last=\"{}\"\n",
                entry.id,
                format!("{}:{}", entry.kind, entry.agent_type),
                entry.state.label(),
                "",
                entry.model,
                entry.task,
                entry.last_step
            ));
        }
        let approvals = self.approvals.descriptions();
        if !approvals.is_empty() {
            text.push_str(&format!(
                "approvals waiting: {} (answered between tasks)\n",
                approvals.join("; ")
            ));
        }
        if text.is_empty() {
            text.push_str("No sub-agent activity.\n");
        }
        text
    }
}

fn clip_report(text: &str) -> String {
    let text = text.trim();
    if text.len() <= 8 * 1024 {
        return text.to_owned();
    }
    format!(
        "{}\n[report truncated]",
        &text[..text.floor_char_boundary(8 * 1024)]
    )
}

/// Where background worktrees of `project` live.
pub fn worktree_root(data_root: &std::path::Path, project: &std::path::Path) -> PathBuf {
    data_root.join("worktrees").join(project_key(project))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn ask_mode_queues_until_answered_and_never_auto_approves() {
        let queue = Arc::new(ApprovalQueue::new(true, Arc::default()));
        let authorizer = BackgroundAuthorizer {
            mode: Mode::Ask,
            root: std::env::temp_dir(),
            queue: queue.clone(),
            agent: "agent x".into(),
        };
        let cancel = CancellationToken::new();
        let pending = tokio::spawn({
            let cancel = cancel.clone();
            async move {
                authorizer
                    .authorize(Effect::Shell("cargo publish".into()), cancel)
                    .await
            }
        });
        let request = loop {
            if let Some(request) = queue.take_all().pop() {
                break request;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        };
        assert!(request.description.contains("cargo publish"));
        request.answer(false);
        assert!(matches!(pending.await.unwrap(), Err(Error::Denied(_))));

        let headless = BackgroundAuthorizer {
            mode: Mode::Ask,
            root: std::env::temp_dir(),
            queue: Arc::new(ApprovalQueue::new(false, Arc::default())),
            agent: "agent y".into(),
        };
        assert!(
            headless
                .authorize(Effect::Write("x".into()), cancel.clone())
                .await
                .is_err()
        );
        assert!(headless.authorize(Effect::Read, cancel).await.is_ok());
    }

    #[test]
    fn spend_delta_is_what_was_not_reported() {
        let before = Accounting {
            model_calls: 2,
            cost: Some(0.5),
            ..Accounting::default()
        };
        let now = Accounting {
            model_calls: 5,
            cost: Some(0.75),
            ..Accounting::default()
        };
        let spent = delta(&now, &before);
        assert_eq!(spent.model_calls, 3);
        assert_eq!(spent.cost, Some(0.25));
        assert_eq!(delta(&Accounting::default(), &before).cost, None);
    }
}
