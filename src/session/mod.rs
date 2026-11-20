//! Sessions and recovery: the versioned session store, file checkpoints, git snapshots, and the cost ledger.

pub mod history;
mod journal;
pub mod snapshot;
pub mod suggestions;
pub mod summary;
pub mod time;
pub mod titles;
pub mod turns;
pub mod usage;
pub mod workspace;

use std::{
    fs::File,
    path::{Path, PathBuf},
    time::{SystemTime, UNIX_EPOCH},
};

use fs2::FileExt;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

pub use history::History;
pub use summary::{SessionScope, SessionSummary, history_lines};
pub use turns::{TurnRecord, is_steer};

use crate::{
    domain::{Block, Error, Message, Result, Usage},
    persist::{atomic_json, read_json},
    tools::ToolOutput,
};

pub fn new_id() -> String {
    uuid::Uuid::new_v4().simple().to_string()
}

pub fn valid_id(id: &str) -> bool {
    id.len() == 32 && id.bytes().all(|byte| byte.is_ascii_hexdigit())
}

pub fn now() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_secs())
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub enum State {
    Idle,
    Running,
    Interrupted(String),
    Completed,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub enum CallState {
    Planned,
    Executing,
    Uncertain,
    Finished,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct PendingCall {
    pub id: String,
    pub name: String,
    pub input: serde_json::Value,
    pub state: CallState,
    pub result: Option<ToolOutput>,
}

#[derive(Clone, Debug, Serialize, Deserialize, Default)]
pub struct Accounting {
    pub usage: Usage,
    pub model_calls: u64,
    pub cost: Option<f64>,
    /// Spend per model that actually served the requests (a session may switch models).
    #[serde(default, skip_serializing_if = "std::collections::BTreeMap::is_empty")]
    pub by_model: std::collections::BTreeMap<String, ModelSpend>,
    /// Time spent waiting on model responses, in milliseconds.
    #[serde(default)]
    pub api_ms: u64,
    /// Lines the file tools added and removed in this session's workspace.
    #[serde(default)]
    pub lines_added: u64,
    #[serde(default)]
    pub lines_removed: u64,
}

#[derive(Clone, Debug, Serialize, Deserialize, Default, PartialEq)]
pub struct ModelSpend {
    pub calls: u64,
    pub usage: Usage,
    /// `None` once any response of this model had unknown prices or usage.
    pub cost: Option<f64>,
}

impl ModelSpend {
    pub fn add(&mut self, other: &ModelSpend) {
        self.calls = self.calls.saturating_add(other.calls);
        self.usage.input = self.usage.input.saturating_add(other.usage.input);
        self.usage.output = self.usage.output.saturating_add(other.usage.output);
        self.usage.cache_read = self.usage.cache_read.saturating_add(other.usage.cache_read);
        self.usage.cache_write = self
            .usage
            .cache_write
            .saturating_add(other.usage.cache_write);
        self.usage.cache_write_1h = self
            .usage
            .cache_write_1h
            .saturating_add(other.usage.cache_write_1h);
        self.cost = self.cost.zip(other.cost).map(|(a, b)| a + b);
    }
}

impl Accounting {
    /// Adds one model response: its usage, its cost (`None` makes the total unknown) and
    /// the per-model spend of the model that answered.
    pub fn add_response(&mut self, response: &crate::domain::ModelResponse, cost: Option<f64>) {
        let usage = &mut self.usage;
        usage.input = usage.input.saturating_add(response.usage.input);
        usage.output = usage.output.saturating_add(response.usage.output);
        usage.cache_read = usage.cache_read.saturating_add(response.usage.cache_read);
        usage.cache_write = usage.cache_write.saturating_add(response.usage.cache_write);
        usage.cache_write_1h = usage
            .cache_write_1h
            .saturating_add(response.usage.cache_write_1h);
        self.cost = self.cost.zip(cost).map(|(a, b)| a + b);
        if let Some(model) = response.model.as_ref().filter(|model| !model.is_empty()) {
            self.by_model
                .entry(model.clone())
                .or_insert_with(|| ModelSpend {
                    cost: Some(0.0),
                    ..ModelSpend::default()
                })
                .add(&ModelSpend {
                    calls: 1,
                    usage: response.usage,
                    cost,
                });
        }
    }

    /// Adds the per-model records of `other` (a sub-agent's spend).
    pub fn merge_models(&mut self, other: &Accounting) {
        for (model, spend) in &other.by_model {
            self.by_model
                .entry(model.clone())
                .or_insert_with(|| ModelSpend {
                    cost: Some(0.0),
                    ..ModelSpend::default()
                })
                .add(spend);
        }
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Session {
    #[serde(default)]
    pub hook_runs: Vec<crate::extensions::hooks::HookRun>,
    pub version: u32,
    pub id: String,
    pub cwd: PathBuf,
    pub protocol: String,
    pub model: String,
    pub created: u64,
    pub updated: u64,
    /// Absent in journal records, which carry their messages separately.
    #[serde(default)]
    pub messages: History,
    pub current_request: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub task_id: Option<String>,
    pub state: State,
    pub pending: Vec<PendingCall>,
    pub accounting: Accounting,
    pub prices: Option<(f64, f64)>,
    pub checkpoints: Vec<String>,
    pub current_checkpoint: Option<String>,
    pub compactions: u32,
    #[serde(default)]
    pub task_failures: u32,
    /// The session's name in history lists (at most 60 characters): the first real
    /// request, then a title the model gave it, or the name the user chose.
    #[serde(default)]
    pub title: Option<String>,
    #[serde(default, skip_serializing_if = "TitleSource::is_prompt")]
    pub title_source: TitleSource,
}

/// Where a session's title came from. A model's title is replaced as the
/// conversation grows; a name the user gave never is.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TitleSource {
    #[default]
    Prompt,
    Ai,
    User,
}

impl TitleSource {
    pub fn is_prompt(&self) -> bool {
        *self == Self::Prompt
    }
}

impl Session {
    pub fn new(cwd: PathBuf, protocol: String, model: String, prices: Option<(f64, f64)>) -> Self {
        Self {
            hook_runs: Vec::new(),
            version: 1,
            id: new_id(),
            cwd,
            protocol,
            model,
            created: now(),
            updated: now(),
            messages: History::new(),
            current_request: String::new(),
            task_id: None,
            state: State::Idle,
            pending: Vec::new(),
            accounting: Accounting {
                cost: Some(0.0),
                ..Default::default()
            },
            prices,
            checkpoints: Vec::new(),
            current_checkpoint: None,
            compactions: 0,
            task_failures: 0,
            title: None,
            title_source: TitleSource::Prompt,
        }
    }

    pub fn interrupted(&mut self, reason: String) {
        self.state = State::Interrupted(reason);
        for call in &mut self.pending {
            if call.state == CallState::Executing {
                call.state = CallState::Uncertain;
            }
        }
    }

    pub fn validate(&self) -> Result<()> {
        if self.version != 1 || !valid_id(&self.id) {
            return Err(Error::Session(
                "unsupported session version or invalid ID".into(),
            ));
        }
        if self.task_id.as_ref().is_some_and(|id| !valid_id(id)) {
            return Err(Error::Session("invalid task ID".into()));
        }
        if self
            .accounting
            .cost
            .is_some_and(|cost| !cost.is_finite() || cost < 0.0)
            || self.prices.is_some_and(|(input, output)| {
                !input.is_finite() || !output.is_finite() || input < 0.0 || output < 0.0
            })
        {
            return Err(Error::Session("invalid stored accounting".into()));
        }
        if self.checkpoints.iter().any(|id| !valid_id(id))
            || self
                .current_checkpoint
                .as_ref()
                .is_some_and(|id| !self.checkpoints.contains(id))
        {
            return Err(Error::Session("invalid checkpoint reference".into()));
        }
        let mut ids = std::collections::HashSet::new();
        for call in &self.pending {
            if call.id.is_empty()
                || !ids.insert(&call.id)
                || (call.state == CallState::Finished) != call.result.is_some()
            {
                return Err(Error::Session("invalid pending tool state".into()));
            }
        }
        let mut open = std::collections::HashMap::new();
        let mut seen = std::collections::HashSet::new();
        for message in &self.messages {
            if !open.is_empty()
                && !message
                    .blocks
                    .iter()
                    .all(|block| matches!(block, Block::ToolResult { .. } | Block::Image { .. }))
            {
                return Err(Error::Session(
                    "tool batch was interrupted by unrelated history".into(),
                ));
            }
            for block in &message.blocks {
                match block {
                    Block::ToolUse {
                        id, name, input, ..
                    } => {
                        if message.role != crate::domain::Role::Assistant
                            || id.is_empty()
                            || !seen.insert(id.clone())
                            || !input.is_object()
                        {
                            return Err(Error::Session("invalid tool call in history".into()));
                        }
                        open.insert(id.clone(), (name, input));
                    }
                    Block::ToolResult { id, .. }
                        if message.role != crate::domain::Role::User
                            || open.remove(id).is_none() =>
                    {
                        return Err(Error::Session("orphan tool result in history".into()));
                    }
                    _ => {}
                }
            }
        }
        if open.len() != self.pending.len()
            || (!self.pending.is_empty()
                && (!matches!(self.state, State::Running | State::Interrupted(_))
                    || self
                        .messages
                        .last()
                        .is_none_or(|message| message.role != crate::domain::Role::Assistant)))
            || self.pending.iter().any(|call| {
                open.get(&call.id)
                    .is_none_or(|(name, input)| **name != call.name || **input != call.input)
            })
        {
            return Err(Error::Session(
                "pending tool journal does not match conversation".into(),
            ));
        }
        Ok(())
    }

    pub fn finish_batch(&mut self) -> Result<()> {
        if self.pending.is_empty() {
            return Ok(());
        }
        if self
            .pending
            .iter()
            .any(|call| call.state != CallState::Finished || call.result.is_none())
        {
            return Err(Error::Session("tool batch is not finished".into()));
        }
        // Images follow every tool result: providers require the results first.
        let mut images = Vec::new();
        let mut blocks: Vec<Block> = self
            .pending
            .drain(..)
            .map(|call| {
                let result = call.result.expect("validated finished call");
                images.extend(result.images.into_iter().map(Block::from));
                Block::ToolResult {
                    id: call.id,
                    content: result.content,
                    is_error: result.failed,
                }
            })
            .collect();
        blocks.extend(images);
        self.messages.push(Message {
            role: crate::domain::Role::User,
            blocks,
            origin: crate::domain::Origin::Unknown,
        });
        Ok(())
    }
}

pub trait SessionPersistence: Send + Sync {
    fn save(&self, session: &Session) -> Result<()>;
    fn archive(&self, session: &Session) -> Result<()>;
}

pub struct Ephemeral;

impl SessionPersistence for Ephemeral {
    fn save(&self, _: &Session) -> Result<()> {
        Ok(())
    }
    fn archive(&self, _: &Session) -> Result<()> {
        Ok(())
    }
}

#[derive(Clone)]
pub struct Store {
    root: PathBuf,
}

pub struct Lease {
    _file: File,
}

impl Store {
    pub fn open(root: &Path) -> Result<Self> {
        std::fs::create_dir_all(root)?;
        let root = root.canonicalize()?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&root, std::fs::Permissions::from_mode(0o700))?;
        }
        for name in ["sessions", "journals", "checkpoints", "locks", "archives"] {
            std::fs::create_dir_all(root.join(name))?;
        }
        Ok(Self { root })
    }

    pub fn root(&self) -> &Path {
        &self.root
    }

    pub fn checkpoint_path(&self, id: &str) -> Result<PathBuf> {
        self.path("checkpoints", id)
    }

    /// The journal of a session's snapshot (see `journal`); a directory of its
    /// own, so `sessions/` keeps holding only complete sessions.
    fn journal_path(&self, id: &str) -> Result<PathBuf> {
        Ok(self.path("journals", id)?.with_extension("log"))
    }

    /// Writes the whole session as its snapshot and clears its journal: at task
    /// ends and exits, so files at rest are complete for every reader.
    pub fn flush(&self, session: &Session) -> Result<()> {
        session.validate()?;
        journal::flush(
            &self.path("sessions", &session.id)?,
            &self.journal_path(&session.id)?,
            session,
        )
    }

    fn path(&self, directory: &str, id: &str) -> Result<PathBuf> {
        if !valid_id(id) {
            return Err(Error::Session(
                "invalid ID (expected 32 hexadecimal characters)".into(),
            ));
        }
        Ok(self.root.join(directory).join(format!("{id}.json")))
    }

    pub fn acquire(&self, id: &str) -> Result<Lease> {
        if !valid_id(id) {
            return Err(Error::Session("invalid session ID".into()));
        }
        let lock = self.root.join("locks").join(format!("{id}.lock"));
        let file = crate::persist::try_lock(&lock, std::time::Duration::ZERO, |err| {
            Error::Session(format!(
                "这个会话正被另一个 cove 窗口使用（或无法加锁：{err}）；请关闭那个窗口，或输入 /new 开启新会话"
            ))
        })?;
        Ok(Lease { _file: file })
    }

    pub fn acquire_workspace(&self, root: &Path) -> Result<Lease> {
        let root = root.canonicalize()?;
        let key = format!("{:x}", Sha256::digest(root.as_os_str().as_encoded_bytes()));
        let lock = self
            .root
            .join("locks")
            .join(format!("workspace-{key}.lock"));
        let file = crate::persist::try_lock(&lock, std::time::Duration::ZERO, |err| {
            Error::Workspace(format!(
                "另一个 cove 进程（同一数据目录）正在使用这个项目目录（{err}）；请关闭那个窗口后重试（或在那个窗口里继续工作）"
            ))
        })?;
        Ok(Lease { _file: file })
    }

    pub fn load(&self, id: &str) -> Result<Session> {
        self.load_with_sequence(id).map(|(session, _)| session)
    }

    fn load_with_sequence(&self, id: &str) -> Result<(Session, u64)> {
        let (mut session, sequence) =
            journal::load(&self.path("sessions", id)?, &self.journal_path(id)?)?;
        session.validate()?;
        if session.id != id {
            return Err(Error::Session("session ID does not match its file".into()));
        }
        for run in &mut session.hook_runs {
            if run.state == crate::extensions::hooks::HookState::Running {
                run.state = crate::extensions::hooks::HookState::Uncertain;
                run.outcome = "上次进程在 hook 运行时停止；请检查其效果（不会自动重放），再用 /config hooks resolve 确认".into();
            }
        }
        if session.state == State::Running
            || session
                .pending
                .iter()
                .any(|call| call.state == CallState::Executing)
        {
            session.interrupted("上次进程在任务中途停止；继续前请检查结果不确定的工具".into());
        }
        Ok((session, sequence))
    }

    pub fn list(&self) -> Result<Vec<Session>> {
        let mut sessions = Vec::new();
        for entry in std::fs::read_dir(self.root.join("sessions"))? {
            let path = entry?.path();
            if path
                .extension()
                .is_some_and(|extension| extension == "json")
            {
                let id = path
                    .file_stem()
                    .and_then(|id| id.to_str())
                    .ok_or_else(|| Error::Session("invalid session filename".into()))?;
                sessions.push(self.load(id)?);
            }
        }
        sessions.sort_by_key(|session| std::cmp::Reverse(session.updated));
        Ok(sessions)
    }
}

impl SessionPersistence for Store {
    fn save(&self, session: &Session) -> Result<()> {
        session.validate()?;
        journal::save(
            &self.path("sessions", &session.id)?,
            &self.journal_path(&session.id)?,
            session,
        )
    }

    fn archive(&self, session: &Session) -> Result<()> {
        atomic_json(
            &self
                .root
                .join("archives")
                .join(format!("{}-{}.json", session.id, new_id())),
            session,
        )
    }
}

/// Characters of a session title.
pub const TITLE_CHARS: usize = 60;
/// Default `max_sessions`.
pub const MAX_SESSIONS: i64 = 200;
const PRUNE_INTERVAL: u64 = 600;
const ORPHAN_AGE: u64 = 3600;

/// Notices the program itself adds as user messages; never a title.
fn synthetic(text: &str) -> bool {
    turns::is_notice(text)
}

/// Inputs too short to describe a session ("continue", "继续", "ok").
fn low_signal(text: &str) -> bool {
    let lower = text.trim().to_lowercase();
    lower.chars().count() < 2
        || matches!(
            lower.as_str(),
            "continue"
                | "go on"
                | "go"
                | "ok"
                | "okay"
                | "yes"
                | "y"
                | "hi"
                | "hello"
                | "继续"
                | "继续吧"
                | "好"
                | "好的"
                | "是"
                | "嗯"
                | "你好"
        )
}

/// One-line title of at most `TITLE_CHARS` characters, or `None` for low-signal text.
pub fn title_from(text: &str) -> Option<String> {
    if synthetic(text) || low_signal(text) {
        return None;
    }
    let line: String = text.split_whitespace().collect::<Vec<_>>().join(" ");
    let mut title: String = line.chars().take(TITLE_CHARS).collect();
    if line.chars().count() > TITLE_CHARS {
        title.pop();
        title.push('…');
    }
    Some(title)
}

impl Session {
    /// The stored title, else one derived from the first real user message.
    pub fn display_title(&self) -> String {
        if let Some(title) = self.title.as_ref().filter(|title| !title.trim().is_empty()) {
            return title.clone();
        }
        // Only "继续"/"好的"-like requests: the start of the first conclusion says more.
        self.derived_title()
            .or_else(|| {
                self.turns().iter().find_map(|turn| {
                    turn.reply()
                        .and_then(|reply| reply.lines().find(|line| !line.trim().is_empty()))
                        .map(|line| {
                            let line: String = line.trim().chars().take(TITLE_CHARS - 2).collect();
                            format!("（{line}）")
                        })
                })
            })
            .unwrap_or_else(|| "(untitled)".into())
    }

    pub fn derived_title(&self) -> Option<String> {
        self.turns()
            .iter()
            .find_map(|turn| title_from(turn.input))
            .or_else(|| title_from(&self.current_request))
    }

    /// Whether a real user request was ever made in this session.
    pub fn has_conversation(&self) -> bool {
        self.user_turns() > 0
    }

    /// Whether a task stopped before finishing (killed, interrupted, uncertain tools).
    pub fn unfinished(&self) -> bool {
        !self.pending.is_empty() || matches!(self.state, State::Running | State::Interrupted(_))
    }

    /// The first real user request, for matching a retyped request.
    pub fn first_request(&self) -> Option<&str> {
        self.turns().first().map(|turn| turn.input)
    }

    /// How worth resuming this session is: a real conversation with tool work scores
    /// high, an unfinished one highest; an empty or trivial one negative.
    pub fn resume_score(&self) -> i32 {
        if !self.has_conversation() {
            return -100;
        }
        let mut score = (self.messages.len() as i32).min(4);
        let tools = self.messages.iter().any(|message| {
            message
                .blocks
                .iter()
                .any(|block| matches!(block, Block::ToolUse { .. }))
        });
        if tools {
            score += 4;
        }
        if self
            .messages
            .iter()
            .filter(|m| m.role == crate::domain::Role::Assistant)
            .count()
            > 1
        {
            score += 2;
        }
        match self.first_request() {
            Some(text) if low_signal(text) => score -= 6,
            Some(text) => {
                score += 3;
                if text.chars().count() >= 20 {
                    score += 1;
                }
            }
            None => {}
        }
        if self.unfinished() {
            score += 10;
        }
        score
    }

    /// Turns of the live session (see [`turns`]); compaction may have removed earlier
    /// ones, which [`Store::history`] restores from the archives.
    pub fn user_turns(&self) -> usize {
        self.turns().len()
    }
}

#[derive(Debug, Default, PartialEq, Eq)]
pub struct PruneReport {
    pub sessions: usize,
    pub archives: usize,
    pub checkpoints: usize,
    pub locks: usize,
}

#[derive(Debug, Default)]
pub struct CleanReport {
    pub scanned: usize,
    pub titles_fixed: usize,
    pub quarantined: Vec<String>,
    pub in_use: usize,
    pub backup: Option<PathBuf>,
}

fn age(path: &Path) -> u64 {
    std::fs::metadata(path)
        .and_then(|meta| meta.modified())
        .ok()
        .and_then(|time| SystemTime::now().duration_since(time).ok())
        .map_or(0, |age| age.as_secs())
}

impl Store {
    /// Every turn of `session`, also those a compaction removed from it: the
    /// archives taken before each compaction are joined with the live session.
    pub fn history(&self, session: &Session) -> Vec<TurnRecord> {
        let live: Vec<TurnRecord> = session.turns().iter().map(turns::Turn::record).collect();
        if session.compactions == 0 {
            return live;
        }
        let prefix = format!("{}-", session.id);
        let mut snapshots: Vec<Session> = std::fs::read_dir(self.root.join("archives"))
            .into_iter()
            .flatten()
            .flatten()
            .filter(|entry| entry.file_name().to_string_lossy().starts_with(&prefix))
            .filter_map(|entry| read_json::<Session>(&entry.path()).ok())
            .filter(|snapshot| snapshot.compactions < session.compactions)
            .collect();
        // One snapshot per compaction: the fullest one taken before it.
        snapshots.sort_by_key(|snapshot| (snapshot.compactions, snapshot.messages.len()));
        let mut levels: Vec<(u32, Vec<TurnRecord>)> = Vec::new();
        for snapshot in &snapshots {
            let turns = snapshot.turns().iter().map(turns::Turn::record).collect();
            match levels.last_mut() {
                Some((level, kept)) if *level == snapshot.compactions => *kept = turns,
                _ => levels.push((snapshot.compactions, turns)),
            }
        }
        let mut levels: Vec<Vec<TurnRecord>> = levels.into_iter().map(|(_, turns)| turns).collect();
        levels.push(live);
        turns::stitch(levels)
    }

    fn session_files(&self) -> Result<Vec<(String, PathBuf)>> {
        let mut files = Vec::new();
        for entry in std::fs::read_dir(self.root.join("sessions"))? {
            let path = entry?.path();
            if path
                .extension()
                .is_some_and(|extension| extension == "json")
                && let Some(id) = path.file_stem().and_then(|id| id.to_str())
                && valid_id(id)
            {
                files.push((id.to_owned(), path));
            }
        }
        Ok(files)
    }

    /// Every loadable session (newest first) and the files that failed to load.
    pub fn list_lenient(&self) -> (Vec<Session>, Vec<(PathBuf, String)>) {
        let mut sessions = Vec::new();
        let mut broken = Vec::new();
        for (id, path) in self.session_files().unwrap_or_default() {
            match self.load_with_sequence(&id) {
                Ok(session) => sessions.push(session),
                Err(err) => broken.push((path, err.to_string())),
            }
        }
        sessions.sort_by_key(|(session, sequence)| {
            std::cmp::Reverse((session.updated, *sequence, session.created))
        });
        (
            sessions.into_iter().map(|(session, _)| session).collect(),
            broken,
        )
    }

    /// Deletes a session with its archives and file checkpoints. The caller holds its
    /// lease (so it is not in use) and never passes the current session.
    pub fn remove(&self, session: &Session) -> Result<()> {
        let log = self.journal_path(&session.id)?;
        journal::forget(&log);
        let _ = std::fs::remove_file(log);
        std::fs::remove_file(self.path("sessions", &session.id)?)?;
        for entry in std::fs::read_dir(self.root.join("archives"))?.flatten() {
            if entry
                .file_name()
                .to_string_lossy()
                .starts_with(&format!("{}-", session.id))
            {
                let _ = std::fs::remove_file(entry.path());
            }
        }
        for checkpoint in &session.checkpoints {
            let _ = std::fs::remove_file(self.checkpoint_path(checkpoint)?);
        }
        Ok(())
    }

    /// Keeps the newest `max` sessions (all projects; `max <= 0` disables), at most once
    /// every ten minutes, never touching `current` or a session in use; also removes
    /// orphaned archives, file checkpoints and lock files.
    pub fn prune(&self, max: i64, current: &str) -> Result<Option<PruneReport>> {
        if max <= 0 {
            return Ok(None);
        }
        let stamp = self.root.join("locks").join("prune.stamp");
        if stamp.exists() && age(&stamp) < PRUNE_INTERVAL {
            return Ok(None);
        }
        std::fs::write(&stamp, now().to_string())?;
        let mut report = PruneReport::default();
        let (sessions, broken) = self.list_lenient();
        let mut kept = Vec::new();
        for (index, session) in sessions.into_iter().enumerate() {
            if index as i64 >= max
                && session.id != current
                && let Ok(lease) = self.acquire(&session.id)
            {
                self.remove(&session)?;
                drop(lease);
                report.sessions += 1;
            } else {
                kept.push(session);
            }
        }
        let known: std::collections::HashSet<String> = self
            .session_files()?
            .into_iter()
            .map(|(id, _)| id)
            .collect();
        for entry in std::fs::read_dir(self.root.join("archives"))?.flatten() {
            let name = entry.file_name().to_string_lossy().into_owned();
            if name.len() > 32
                && valid_id(&name[..32])
                && !known.contains(&name[..32])
                && age(&entry.path()) >= ORPHAN_AGE
                && std::fs::remove_file(entry.path()).is_ok()
            {
                report.archives += 1;
            }
        }
        // Unknown references (a broken session file) make every checkpoint suspect.
        if broken.is_empty() {
            let referenced: std::collections::HashSet<&String> = kept
                .iter()
                .flat_map(|session| &session.checkpoints)
                .collect();
            for entry in std::fs::read_dir(self.root.join("checkpoints"))?.flatten() {
                let path = entry.path();
                let Some(id) = path
                    .file_stem()
                    .and_then(|stem| stem.to_str())
                    .filter(|_| path.extension().is_some_and(|ext| ext == "json"))
                else {
                    continue;
                };
                if valid_id(id)
                    && !referenced.contains(&id.to_owned())
                    && age(&path) >= ORPHAN_AGE
                    && std::fs::remove_file(&path).is_ok()
                {
                    report.checkpoints += 1;
                }
            }
        }
        for entry in std::fs::read_dir(self.root.join("locks"))?.flatten() {
            let name = entry.file_name().to_string_lossy().into_owned();
            let Some(id) = name.strip_suffix(".lock").filter(|id| valid_id(id)) else {
                continue;
            };
            if known.contains(id) {
                continue;
            }
            let free = File::options()
                .read(true)
                .write(true)
                .open(entry.path())
                .is_ok_and(|file| file.try_lock_exclusive().is_ok());
            if free && std::fs::remove_file(entry.path()).is_ok() {
                report.locks += 1;
            }
        }
        Ok(Some(report))
    }

    /// Repairs history: unreadable session files are moved (not deleted) to
    /// `backups/clean-<time>/`, and sessions without a title get one derived from their
    /// first real request (the original file is copied to the backup first).
    pub fn clean(&self, current: &str) -> Result<CleanReport> {
        let mut report = CleanReport::default();
        let backup = self.root.join("backups").join(format!("clean-{}", now()));
        let ensure = |report: &mut CleanReport| -> Result<()> {
            if report.backup.is_none() {
                std::fs::create_dir_all(&backup)?;
                report.backup = Some(backup.clone());
            }
            Ok(())
        };
        for (id, path) in self.session_files()? {
            report.scanned += 1;
            let lease = if id == current {
                None
            } else {
                match self.acquire(&id) {
                    Ok(lease) => Some(lease),
                    Err(_) => {
                        report.in_use += 1;
                        continue;
                    }
                }
            };
            match self.load(&id) {
                Err(err) => {
                    if id == current {
                        continue;
                    }
                    ensure(&mut report)?;
                    let log = self.journal_path(&id)?;
                    journal::forget(&log);
                    if log.exists() {
                        std::fs::rename(&log, backup.join(format!("{id}.log")))?;
                    }
                    std::fs::rename(&path, backup.join(format!("{id}.json")))?;
                    report.quarantined.push(format!("{id}: {err}"));
                }
                Ok(mut session) if session.title.is_none() && id != current => {
                    if let Some(title) = session.derived_title() {
                        ensure(&mut report)?;
                        std::fs::copy(&path, backup.join(format!("{id}.json")))?;
                        session.title = Some(title);
                        self.flush(&session)?;
                        report.titles_fixed += 1;
                    }
                }
                Ok(_) => {}
            }
            drop(lease);
        }
        Ok(report)
    }
}

/// `--list-sessions [all]`, without model credentials.
pub fn list_sessions(
    data_dir: &std::path::Path,
    cwd: &std::path::Path,
    all: bool,
) -> Result<String> {
    let store = Store::open(data_dir)?;
    let cwd = cwd.canonicalize()?;
    let scope = if all {
        SessionScope::everywhere()
    } else {
        SessionScope::workspace(&cwd)
    };
    let mut out = history_lines(&store.summaries(&scope), None, all);
    let broken = store.list_lenient().1;
    if !broken.is_empty() {
        out.push_str(&format!(
            "有 {} 个会话文件无法读取；输入 /resume clean 清理。\n",
            broken.len()
        ));
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::Message;

    fn session(store: &Store, cwd: &str, request: Option<&str>) -> Session {
        let mut session = Session::new(cwd.into(), "p".into(), "m".into(), None);
        if let Some(request) = request {
            session.messages.push(Message::user(request));
        }
        store.save(&session).unwrap();
        session
    }

    #[test]
    fn titles_skip_synthetic_and_low_signal_messages() {
        assert_eq!(title_from("继续"), None);
        assert_eq!(title_from("[system notice]"), None);
        assert_eq!(
            title_from("Fix the\nlogin   bug").as_deref(),
            Some("Fix the login bug")
        );
        let long = "x".repeat(100);
        assert_eq!(title_from(&long).unwrap().chars().count(), TITLE_CHARS);
        let mut session = Session::new(PathBuf::new(), String::new(), String::new(), None);
        session.messages.push(Message::user("continue"));
        session.messages.push(Message::user("重构配置加载"));
        assert_eq!(session.display_title(), "重构配置加载");
        session.title = Some("stored".into());
        assert_eq!(session.display_title(), "stored");
    }

    #[test]
    fn turns_keep_tool_results_and_system_notices_with_their_request() {
        let mut session = Session::new(PathBuf::new(), String::new(), String::new(), None);
        session.messages.push(Message::user("first request"));
        session.messages.push(Message {
            role: crate::domain::Role::Assistant,
            blocks: vec![Block::ToolUse {
                id: "call".into(),
                name: "read".into(),
                input: serde_json::json!({"path":"file"}),
                extra: None,
            }],
            origin: crate::domain::Origin::Unknown,
        });
        session.messages.push(Message {
            role: crate::domain::Role::User,
            blocks: vec![Block::ToolResult {
                id: "call".into(),
                content: "result".into(),
                is_error: false,
            }],
            origin: crate::domain::Origin::Unknown,
        });
        session
            .messages
            .push(Message::user("[system notice] retry"));
        session.messages.push(Message::user("second request"));
        session.messages.push(Message {
            role: crate::domain::Role::Assistant,
            blocks: vec![Block::Text("answer".into())],
            origin: crate::domain::Origin::Unknown,
        });
        let turns = session.turns();
        assert_eq!(turns.len(), 2);
        assert_eq!(turns[0].messages.len(), 4);
        assert_eq!(turns[1].messages.len(), 2);
        assert!(
            matches!(&turns[0].messages[2].blocks[0], Block::ToolResult { content, .. } if content == "result")
        );
        assert!(matches!(&turns[1].messages[1].blocks[0], Block::Text(text) if text == "answer"));
    }

    #[test]
    fn turns_keep_steering_as_a_user_turn() {
        let mut session = Session::new(PathBuf::new(), String::new(), String::new(), None);
        session.messages.push(Message::user("first request"));
        session.messages.push(Message::user("[the user typed this while the task ran; treat it as an instruction from the user]\nuse the new API"));
        session.messages.push(Message::user(
            "[sent while the previous task was running]\nverify again",
        ));
        session
            .messages
            .push(Message::user("[system notice] retry"));
        session.messages.push(Message::user("[bug] inspect this"));
        let turns = session.turns();
        assert_eq!(turns.len(), 4);
        assert_eq!(turns[2].messages.len(), 2);
        assert_eq!(turns[1].input, "use the new API");
        assert!(turns[1].steer);
        assert_eq!(turns[2].input, "verify again");
        assert_eq!(turns[3].input, "[bug] inspect this");
    }

    #[test]
    fn prune_keeps_newest_and_current_and_removes_orphans() {
        let data = tempfile::tempdir().unwrap();
        let store = Store::open(data.path()).unwrap();
        let current = session(&store, "/a", Some("current"));
        let mut old = session(&store, "/a", Some("old"));
        old.checkpoints.push(new_id());
        std::fs::write(store.checkpoint_path(&old.checkpoints[0]).unwrap(), "{}").unwrap();
        store.save(&old).unwrap();
        store.archive(&old).unwrap();
        std::thread::sleep(std::time::Duration::from_millis(1100));
        let newest = session(&store, "/b", Some("newest"));
        let _busy = store.acquire(&newest.id).unwrap();
        assert_eq!(store.prune(0, &current.id).unwrap(), None);
        let report = store.prune(1, &current.id).unwrap().unwrap();
        assert_eq!(report.sessions, 1);
        let (left, broken) = store.list_lenient();
        assert!(broken.is_empty());
        let ids: Vec<_> = left.iter().map(|s| s.id.clone()).collect();
        assert!(ids.contains(&current.id) && ids.contains(&newest.id) && !ids.contains(&old.id));
        assert!(!store.checkpoint_path(&old.checkpoints[0]).unwrap().exists());
        assert_eq!(
            std::fs::read_dir(store.root().join("archives"))
                .unwrap()
                .count(),
            0
        );
        // Rate limited: a second prune within ten minutes does nothing.
        assert_eq!(store.prune(1, &current.id).unwrap(), None);
    }

    #[test]
    fn clean_quarantines_broken_files_and_fixes_titles_with_backups() {
        let data = tempfile::tempdir().unwrap();
        let store = Store::open(data.path()).unwrap();
        let current = session(&store, "/a", Some("current"));
        let untitled = session(&store, "/a", Some("Add retries to the HTTP client"));
        let broken = new_id();
        std::fs::write(
            store.root().join("sessions").join(format!("{broken}.json")),
            "{",
        )
        .unwrap();
        let report = store.clean(&current.id).unwrap();
        assert_eq!(report.scanned, 3);
        assert_eq!(report.titles_fixed, 1);
        assert_eq!(report.quarantined.len(), 1);
        let backup = report.backup.unwrap();
        assert!(backup.join(format!("{broken}.json")).exists());
        assert!(backup.join(format!("{}.json", untitled.id)).exists());
        assert_eq!(
            store.load(&untitled.id).unwrap().title.as_deref(),
            Some("Add retries to the HTTP client")
        );
        assert!(store.load(&current.id).unwrap().title.is_none());
        assert!(store.list_lenient().1.is_empty());
    }
}
