//! Sessions and recovery: the versioned session store, file checkpoints, git snapshots, the cost ledger and session notes.

pub mod history;
mod journal;
pub mod notes;
pub mod snapshot;
pub mod usage;
pub mod workspace;

use std::{
    fs::{File, OpenOptions},
    io::{Read, Write},
    path::{Path, PathBuf},
    time::{SystemTime, UNIX_EPOCH},
};

use fs2::FileExt;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

pub use history::History;

use crate::{
    domain::{Block, Error, Message, Result, Usage},
    tools::ToolOutput,
};

const STATE_LIMIT: u64 = 32 * 1024 * 1024;

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
        self.cost = self.cost.zip(other.cost).map(|(a, b)| a + b);
    }
}

impl Accounting {
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
    pub state: State,
    pub pending: Vec<PendingCall>,
    pub accounting: Accounting,
    pub prices: Option<(f64, f64)>,
    pub checkpoints: Vec<String>,
    pub current_checkpoint: Option<String>,
    pub compactions: u32,
    #[serde(default)]
    pub task_failures: u32,
    /// First real user request (at most 60 characters), for history lists.
    #[serde(default)]
    pub title: Option<String>,
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
            state: State::Idle,
            pending: Vec::new(),
            accounting: Accounting {
                cost: prices.map(|_| 0.0),
                ..Default::default()
            },
            prices,
            checkpoints: Vec::new(),
            current_checkpoint: None,
            compactions: 0,
            task_failures: 0,
            title: None,
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
        let file = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .open(&lock)
            .map_err(|err| io_context("open lock", &lock, err))?;
        file.try_lock_exclusive().map_err(|err| {
            Error::Session(format!(
                "这个会话正被另一个 cove 窗口使用（或无法加锁：{err}）；请关闭那个窗口，或输入 /new 开启新会话"
            ))
        })?;
        Ok(Lease { _file: file })
    }

    pub fn acquire_workspace(&self, root: &Path) -> Result<Lease> {
        let root = root.canonicalize()?;
        let key = format!("{:x}", Sha256::digest(root.as_os_str().as_encoded_bytes()));
        let file = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .open(
                self.root
                    .join("locks")
                    .join(format!("workspace-{key}.lock")),
            )
            .map_err(|err| {
                io_context(
                    "open lock",
                    &self
                        .root
                        .join("locks")
                        .join(format!("workspace-{key}.lock")),
                    err,
                )
            })?;
        file.try_lock_exclusive().map_err(|err| {
            Error::Workspace(format!(
                "另一个 cove 进程（同一数据目录）正在使用这个项目目录（{err}）；请关闭那个窗口后重试（或在那个窗口里继续工作）"
            ))
        })?;
        Ok(Lease { _file: file })
    }

    pub fn load(&self, id: &str) -> Result<Session> {
        let mut session = journal::load(&self.path("sessions", id)?, &self.journal_path(id)?)?;
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
        Ok(session)
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

pub fn read_json<T: serde::de::DeserializeOwned>(path: &Path) -> Result<T> {
    let mut data = Vec::new();
    File::open(path)
        .map_err(|err| io_context("open", path, err))?
        .take(STATE_LIMIT + 1)
        .read_to_end(&mut data)
        .map_err(|err| io_context("read", path, err))?;
    if data.len() as u64 > STATE_LIMIT {
        return Err(Error::Session("state file exceeds 32 MiB".into()));
    }
    Ok(serde_json::from_slice(&data)?)
}

/// Processes that have `path` open, as the Windows Restart Manager reports them
/// ("name (pid)", comma separated); empty when none or unknown.
#[cfg(windows)]
pub fn file_holders(path: &Path) -> String {
    use windows_sys::Win32::System::RestartManager::{
        RM_PROCESS_INFO, RmEndSession, RmGetList, RmRegisterResources, RmStartSession,
    };
    let display = path.to_string_lossy();
    let plain = display.strip_prefix(r"\\?\").unwrap_or(&display);
    let wide: Vec<u16> = plain.encode_utf16().chain(Some(0)).collect();
    let mut key = [0_u16; 64];
    let mut session = 0_u32;
    // All pointers refer to live local buffers of the declared sizes.
    unsafe {
        if RmStartSession(&mut session, 0, key.as_mut_ptr()) != 0 {
            return String::new();
        }
        let files = [wide.as_ptr()];
        let mut names = Vec::new();
        if RmRegisterResources(
            session,
            1,
            files.as_ptr(),
            0,
            std::ptr::null(),
            0,
            std::ptr::null(),
        ) == 0
        {
            let mut needed = 0_u32;
            let mut count = 16_u32;
            let mut reasons = 0_u32;
            let mut infos: Vec<RM_PROCESS_INFO> = vec![std::mem::zeroed(); 16];
            if RmGetList(
                session,
                &mut needed,
                &mut count,
                infos.as_mut_ptr(),
                &mut reasons,
            ) == 0
            {
                for info in &infos[..count as usize] {
                    let end = info.strAppName.iter().position(|c| *c == 0).unwrap_or(256);
                    names.push(format!(
                        "{} ({})",
                        String::from_utf16_lossy(&info.strAppName[..end]),
                        info.Process.dwProcessId
                    ));
                }
            }
        }
        RmEndSession(session);
        names.join(", ")
    }
}

#[cfg(not(windows))]
pub fn file_holders(_: &Path) -> String {
    String::new()
}

/// Moves a written temporary file over `path`. `NamedTempFile::persist` uses
/// `MoveFileExW`, which on Windows fails with "access denied" while any process (a
/// reader, an editor, a virus scanner, the search indexer) has the target open, even
/// with delete sharing; `std::fs::rename` uses POSIX rename semantics there and
/// replaces the file anyway, as it does on Unix.
pub fn replace_with(file: tempfile::NamedTempFile, path: &Path) -> std::io::Result<()> {
    let temporary = file.into_temp_path();
    std::fs::rename(&temporary, path)?;
    let _ = temporary.keep();
    Ok(())
}

/// An I/O error that names the operation and the file, keeping its kind.
pub fn io_context(op: &str, path: &Path, err: std::io::Error) -> Error {
    Error::Io(std::io::Error::new(
        err.kind(),
        format!("{op} {}: {err}", path.display()),
    ))
}

pub fn atomic_json(path: &Path, value: &impl Serialize) -> Result<()> {
    let data = serde_json::to_vec(value)?;
    if data.len() as u64 > STATE_LIMIT {
        return Err(Error::Session(
            "state exceeds 32 MiB; no data was overwritten".into(),
        ));
    }
    let parent = path
        .parent()
        .ok_or_else(|| Error::Session("state path has no parent".into()))?;
    let mut file = tempfile::NamedTempFile::new_in(parent)
        .map_err(|err| io_context("create temporary file in", parent, err))?;
    file.write_all(&data)
        .map_err(|err| io_context("write", file.path(), err))?;
    file.as_file()
        .sync_all()
        .map_err(|err| io_context("sync", file.path(), err))?;
    replace_with(file, path).map_err(|err| {
        let holders = file_holders(path);
        let op = if holders.is_empty() {
            "replace".to_owned()
        } else {
            format!("replace (in use by {holders})")
        };
        io_context(&op, path, err)
    })?;
    #[cfg(unix)]
    File::open(parent)?.sync_all()?;
    Ok(())
}

/// Characters of a session title.
pub const TITLE_CHARS: usize = 60;
/// Default `max_sessions`.
pub const MAX_SESSIONS: i64 = 200;
const PRUNE_INTERVAL: u64 = 600;
const ORPHAN_AGE: u64 = 3600;

/// Notices the program itself adds as user messages; never a title.
fn synthetic(text: &str) -> bool {
    let text = text.trim();
    text.is_empty()
        || text.starts_with('[')
        || text.starts_with("Earlier conversation summary")
        || text.starts_with("Operator ")
        || text.starts_with("File-tool rollback")
        || text.starts_with("Workspace restored")
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
        self.derived_title().unwrap_or_else(|| "(untitled)".into())
    }

    pub fn derived_title(&self) -> Option<String> {
        self.messages
            .iter()
            .filter(|message| message.role == crate::domain::Role::User)
            .filter_map(|message| match message.blocks.first() {
                Some(Block::Text(text)) => title_from(text),
                _ => None,
            })
            .next()
            .or_else(|| title_from(&self.current_request))
    }

    /// Real (non-synthetic) user turns.
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
        self.messages
            .iter()
            .find_map(|message| match message.blocks.first() {
                Some(Block::Text(text))
                    if message.role == crate::domain::Role::User && !synthetic(text) =>
                {
                    Some(text.as_str())
                }
                _ => None,
            })
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

    /// The last `turns` user turns as the terminal showed them: requests after `> `,
    /// the assistant's text, tool calls by name and the first line of each result.
    pub fn transcript(&self, turns: usize) -> String {
        let real_turn = |message: &Message| {
            message.role == crate::domain::Role::User
                && matches!(message.blocks.first(), Some(Block::Text(text)) if !synthetic(text))
        };
        let starts: Vec<usize> = self
            .messages
            .iter()
            .enumerate()
            .filter(|(_, message)| real_turn(message))
            .map(|(index, _)| index)
            .collect();
        let Some(&start) = starts
            .len()
            .checked_sub(turns.max(1))
            .and_then(|at| starts.get(at))
            .or(starts.first())
        else {
            return String::new();
        };
        let clip = |text: &str, limit: usize| {
            let text = text.trim();
            if text.chars().count() <= limit {
                text.to_owned()
            } else {
                let cut: String = text.chars().take(limit).collect();
                format!("{cut}…")
            }
        };
        let mut out = String::new();
        for message in &self.messages[start..] {
            for block in &message.blocks {
                match (message.role, block) {
                    (crate::domain::Role::User, Block::Text(text)) if !synthetic(text) => {
                        out.push_str(&format!("\n> {}\n", clip(text, 800)));
                    }
                    (crate::domain::Role::Assistant, Block::Text(text))
                        if !text.trim().is_empty() =>
                    {
                        out.push_str(&format!("{}\n", clip(text, 2000)));
                    }
                    (_, Block::ToolUse { name, input, .. }) => {
                        let detail = ["path", "command", "pattern", "query", "url", "description"]
                            .iter()
                            .find_map(|key| input.get(*key).and_then(serde_json::Value::as_str))
                            .map(|value| format!(" {}", clip(value, 100)))
                            .unwrap_or_default();
                        out.push_str(&format!("[tool] {name}{detail}\n"));
                    }
                    (
                        _,
                        Block::ToolResult {
                            content, is_error, ..
                        },
                    ) => {
                        let first = content
                            .lines()
                            .find(|line| !line.trim().is_empty())
                            .unwrap_or_default();
                        out.push_str(&format!(
                            "  [{}] {}\n",
                            if *is_error { "failed" } else { "ok" },
                            clip(first, 160)
                        ));
                    }
                    _ => {}
                }
            }
        }
        out
    }

    pub fn user_turns(&self) -> usize {
        self.messages
            .iter()
            .filter(|message| {
                message.role == crate::domain::Role::User
                    && matches!(message.blocks.first(), Some(Block::Text(text)) if !synthetic(text))
            })
            .count()
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
            match self.load(&id) {
                Ok(session) => sessions.push(session),
                Err(err) => broken.push((path, err.to_string())),
            }
        }
        sessions.sort_by_key(|session| std::cmp::Reverse((session.updated, session.created)));
        (sessions, broken)
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

/// One history line per session, numbered from 1.
pub fn history_lines(sessions: &[Session], current: Option<&str>, show_cwd: bool) -> String {
    let mut out = String::new();
    for (index, session) in sessions.iter().enumerate() {
        let state = match &session.state {
            State::Idle => "空闲",
            State::Running => "运行中",
            State::Interrupted(_) => "已中断",
            State::Completed => "已完成",
        };
        out.push_str(&format!(
            "{:>3}. {}  {}{} {:>3} 轮  {}  [{}]{}{}\n",
            index + 1,
            crate::session::usage::timestamp(session.updated),
            state,
            // CJK characters take two columns; pad to the widest state word.
            " ".repeat(6 - 2 * state.chars().count()),
            session.user_turns(),
            session.display_title(),
            &session.id[..8],
            if Some(session.id.as_str()) == current {
                " (current)"
            } else {
                ""
            },
            if show_cwd {
                format!("  {}", session.cwd.display())
            } else {
                String::new()
            }
        ));
    }
    if out.is_empty() {
        out.push_str("没有保存的会话。\n");
    }
    out
}

/// `--list-sessions [all]`, without model credentials.
pub fn list_sessions(
    data_dir: &std::path::Path,
    cwd: &std::path::Path,
    all: bool,
) -> Result<String> {
    let store = Store::open(data_dir)?;
    let cwd = cwd.canonicalize()?;
    let (sessions, broken) = store.list_lenient();
    let sessions: Vec<Session> = sessions
        .into_iter()
        .filter(|session| all || session.cwd == cwd)
        .collect();
    let mut out = history_lines(&sessions, None, all);
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

    #[cfg(windows)]
    #[test]
    fn file_holders_names_the_process_that_has_a_file_open() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("held.json");
        std::fs::write(&path, "{}").unwrap();
        let held = File::open(&path).unwrap();
        let holders = file_holders(&path.canonicalize().unwrap());
        assert!(
            holders.contains(&format!("({})", std::process::id())),
            "{holders}"
        );
        drop(held);
        assert!(file_holders(&path).is_empty());
    }

    #[test]
    fn atomic_json_replaces_a_file_that_a_reader_has_open() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("state.json");
        atomic_json(&path, &1).unwrap();
        let reader = File::open(&path).unwrap();
        atomic_json(&path, &2).unwrap();
        drop(reader);
        assert_eq!(std::fs::read_to_string(&path).unwrap(), "2");
        assert_eq!(std::fs::read_dir(dir.path()).unwrap().count(), 1);
    }

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
