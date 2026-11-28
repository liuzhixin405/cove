//! Model-facing tools: files, search, shell, questions, todo, repo map, web, drawing and browser, composed into one executor.

pub mod browser;
pub mod draw;
pub mod extension_tools;
pub mod external;
pub mod jobs;
pub mod line_endings;
pub mod repomap;
pub mod text_encoding;
pub mod view;
pub mod web;
pub mod workflow_tools;

use std::{
    path::{Component, Path, PathBuf},
    sync::Arc,
    time::Duration,
};

use futures_util::future::BoxFuture;
use serde::Deserialize;
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Progress, Result, ToolDefinition, UserInput},
    process::{self, Shell},
    safety::permission::{Authorizer, Effect},
    session::{
        overlay::{EditSpec, Kind, OverlayStack},
        workspace::{Workspace, replace_file_bytes},
    },
    tools::text_encoding::{self as text, TextKind},
};

pub(crate) mod edit;
mod question;
pub(crate) mod search;
mod todo;
mod tracker;

pub use todo::{Priority, Todo, TodoStatus, latest_todos, render as render_todos};
pub use tracker::FileTracker;

/// Largest file write and edit change (the checkpoint snapshot limit).
const FILE_LIMIT: u64 = 1024 * 1024;
/// Largest file read shows; bigger files need grep or the shell.
const READ_LIMIT: u64 = 32 * 1024 * 1024;
const READ_WINDOW: usize = 2000;
const READ_LINE_CHARS: usize = 2000;
/// Read stops collecting lines here; the kernel's per-result cap may cut further.
const READ_OUTPUT: usize = 256 * 1024;
const DIRECTORY_LIMIT: usize = 1000;
const SHELL_TIMEOUT: u64 = 120;
/// Edit results show the changed lines up to this size; past it, only their line numbers
/// (unless a whitespace-tolerant match applied the edit, which the model should see).
const SHORT_SNIPPET: usize = 400;
/// Longest foreground wait a call may ask for; the command runs on in the background
/// after it either way.
const SHELL_MAX_TIMEOUT: u64 = 3600;
/// Most overlay paths grep hands ripgrep as exclusions; more fall back to the built-in search.
const RG_EXCLUDES: usize = 200;
/// Why an overlay sub-task's shell refused a command.
pub const OVERLAY_SHELL_REFUSED: &str =
    "overlay sub-tasks cannot change files on disk; put steps that generate files after the merge";
/// Appended to an overlay sub-task's shell results.
const OVERLAY_SHELL_NOTE: &str = "[note: this ran in the main workspace, which does not have this sub-task's overlay changes; use read, grep or glob to see your own changes]";
const IMAGE_EXTENSIONS: &[&str] = &["png", "jpg", "jpeg", "gif", "webp"];

/// A JSON value as tool-result text, pretty-printed and bounded.
pub fn json_output(value: Value) -> Result<String> {
    Ok(crate::extensions::bounded(serde_json::to_string_pretty(
        &value,
    )?))
}

/// Why `shell` could not run a command written for another shell, read from its errors
/// (English and Chinese Windows messages).
fn dialect_mismatch(shell: &Shell, output: &process::CommandOutput) -> Option<&'static str> {
    let text = format!("{}\n{}", output.stderr, output.stdout).to_lowercase();
    let signs: &[(&str, &'static str)] = match shell.kind {
        process::ShellKind::PowerShell => &[
            ("is not a valid statement separator", "syntax error"),
            ("不是此版本中的有效语句分隔符", "syntax error"),
            ("parsererror", "syntax error"),
            ("unexpected token", "syntax error"),
            ("意外的标记", "syntax error"),
            (
                "is not recognized as the name of a cmdlet",
                "command not found",
            ),
            ("识别为 cmdlet", "command not found"),
        ],
        process::ShellKind::Posix => &[
            ("syntax error near unexpected token", "syntax error"),
            ("command not found", "command not found"),
            ("未找到命令", "command not found"),
        ],
        process::ShellKind::Cmd => &[
            (
                "is not recognized as an internal or external command",
                "command not found",
            ),
            ("不是内部或外部命令", "command not found"),
            ("was unexpected at this time", "syntax error"),
            ("此时不应有", "syntax error"),
        ],
    };
    signs
        .iter()
        .find(|(sign, _)| text.contains(sign))
        .map(|(_, why)| *why)
}

/// Whether an overlay sub-task may run `command`: auto-approvable
/// and read-only, so no redirect into a file, file-changing or build command, network
/// fetch, formatter or deletion; the overlay's changes would never see its effects.
fn overlay_shell_allowed(command: &str) -> bool {
    use crate::safety::permission::{classify, shell::Dialect};
    let dialect = Dialect::for_command(command);
    classify::auto_approvable(command, dialect)
        && classify::classify(command, dialect) == classify::Category::Safe
        && !classify::may_rewrite_sources(command, dialect)
        && !classify::deletes_files(command, dialect)
}

/// Another installed shell that reads `command` as read-only, so it may run again there
/// without a new approval: the shell its syntax names first, then the others.
fn retry_shell(shell: &Shell, command: &str, _why: &str) -> Option<&'static Shell> {
    use crate::safety::permission::shell::Dialect;
    let mut order: Vec<process::ShellKind> = Vec::new();
    if let Some(dialect) = crate::safety::permission::shell::detect(command) {
        order.push(match dialect {
            Dialect::Posix => process::ShellKind::Posix,
            Dialect::PowerShell => process::ShellKind::PowerShell,
            Dialect::Cmd => process::ShellKind::Cmd,
        });
    }
    order.extend([
        process::ShellKind::Posix,
        process::ShellKind::PowerShell,
        process::ShellKind::Cmd,
    ]);
    order
        .into_iter()
        .filter(|kind| *kind != shell.kind)
        .filter(|kind| crate::safety::permission::classify::read_only(command, Dialect::of(*kind)))
        .find_map(Shell::available)
}

#[derive(Clone, Debug, Default, serde::Serialize, serde::Deserialize)]
pub struct ToolOutput {
    pub content: String,
    pub failed: bool,
    /// Images the model should see with this result (the `read` tool on an image).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub images: Vec<crate::domain::ImagePart>,
    /// The user refused the call: the model sees an error, but the task did not fail.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub refused: bool,
}

pub trait ToolExecutor: Send + Sync {
    fn definitions(&self) -> Vec<ToolDefinition>;
    /// The shell commands these tools run in the background, if any:
    /// the task loop delivers their results and waits for them. Required, so a wrapper
    /// cannot hide them.
    fn jobs(&self) -> Option<Arc<jobs::Jobs>>;
    /// Tools that make model requests themselves and report spend through a `Ledger`.
    /// Required, like `concurrent`: a wrapper that forgot to forward these silently
    /// lost parallel reads, so every implementation now says what it does.
    fn metered(&self, name: &str) -> bool;
    /// Read-only tools without approval prompts or side effects that the kernel may run
    /// concurrently with neighbouring calls of the same kind. `input` decides for tools
    /// whose arguments matter (`agent` with a read-only type).
    fn concurrent(&self, name: &str, input: &Value) -> bool;
    fn execute(
        &self,
        name: String,
        input: Value,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ToolOutput>>;
}

#[derive(Clone)]
pub struct LocalTools {
    root: PathBuf,
    authorizer: Arc<dyn Authorizer>,
    user_input: Option<Arc<dyn UserInput>>,
    workspace: Option<Arc<Workspace>>,
    progress: Option<Arc<Progress>>,
    files: Arc<FileTracker>,
    shell: Shell,
    ripgrep: bool,
    jobs: Arc<jobs::Jobs>,
    /// Where file tools read and write: the disk, or an overlay sub-task's layers.
    view: view::FileView,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ReadArgs {
    #[serde(alias = "file_path", alias = "filePath")]
    path: PathBuf,
    /// First line, 1-based.
    #[serde(default, alias = "start_line")]
    offset: Option<usize>,
    #[serde(default, alias = "lines")]
    limit: Option<usize>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct WriteArgs {
    #[serde(alias = "file_path", alias = "filePath")]
    path: PathBuf,
    #[serde(alias = "text")]
    content: String,
    /// Accepted for compatibility; replacing a file requires having read it instead.
    #[serde(default)]
    #[allow(dead_code)]
    overwrite: bool,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct EditArgs {
    #[serde(alias = "file_path", alias = "filePath")]
    path: PathBuf,
    #[serde(alias = "old_string", alias = "oldString")]
    old: String,
    #[serde(alias = "new_string", alias = "newString")]
    new: String,
    #[serde(default, alias = "replaceAll")]
    replace_all: bool,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct GlobArgs {
    pattern: String,
    #[serde(default)]
    path: Option<PathBuf>,
    #[serde(default)]
    include_ignored: bool,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct GrepArgs {
    pattern: String,
    #[serde(default)]
    include: Option<String>,
    #[serde(default)]
    path: Option<PathBuf>,
    #[serde(default, alias = "-i")]
    ignore_case: bool,
    #[serde(default)]
    context: Option<usize>,
    #[serde(default)]
    files_only: bool,
    #[serde(default)]
    include_ignored: bool,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RepoMapArgs {
    #[serde(default)]
    query: String,
    path: Option<PathBuf>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct AskArgs {
    question: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ShellArgs {
    command: String,
    #[serde(default)]
    timeout_seconds: Option<u64>,
    /// Milliseconds.
    #[serde(default)]
    timeout: Option<u64>,
    #[serde(default)]
    #[allow(dead_code)]
    description: Option<String>,
    /// Start in the background and return after a few seconds.
    #[serde(default)]
    run_in_background: bool,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ShellTaskArgs {
    action: String,
    #[serde(default)]
    id: Option<Value>,
}

impl ShellTaskArgs {
    /// `3`, `"3"` or `"#3"`.
    fn id(&self) -> Option<u32> {
        match self.id.as_ref()? {
            Value::Number(number) => number.as_u64().and_then(|id| u32::try_from(id).ok()),
            Value::String(text) => text.trim().trim_start_matches('#').parse().ok(),
            _ => None,
        }
    }
}

enum Call {
    Read(ReadArgs),
    Write(WriteArgs),
    Edit(EditArgs),
    Glob(GlobArgs),
    Grep(GrepArgs),
    RepoMap(RepoMapArgs),
    Shell(ShellArgs),
    ShellTask(ShellTaskArgs),
    Ask(AskArgs),
    Question(question::QuestionArgs),
    Todo(todo::TodoArgs),
}

impl Call {
    fn parse(name: &str, input: Value) -> Result<Self> {
        Ok(match name {
            "read" => Self::Read(serde_json::from_value(input)?),
            "write" => Self::Write(serde_json::from_value(input)?),
            "edit" => Self::Edit(serde_json::from_value(input)?),
            "glob" => Self::Glob(serde_json::from_value(input)?),
            "grep" => Self::Grep(serde_json::from_value(input)?),
            "repo_map" => Self::RepoMap(serde_json::from_value(input)?),
            "shell" => Self::Shell(serde_json::from_value(input)?),
            "shell_task" => Self::ShellTask(serde_json::from_value(input)?),
            // Older sessions may still call the single-question tool.
            "ask_user" => Self::Ask(serde_json::from_value(input)?),
            "question" => Self::Question(serde_json::from_value(input)?),
            "todo" => Self::Todo(serde_json::from_value(input)?),
            _ => return Err(Error::Tool(format!("unknown tool {name}"))),
        })
    }
}

/// `path` relative to `root` with forward slashes, as tool results show paths.
pub fn display_path(root: &Path, path: &Path) -> String {
    search::display(root, path)
}

/// What `edit` makes of a file's bytes (`shown` names it in errors): the new bytes and
/// how `old` matched. Line endings, BOM and GBK encoding are kept as `edit` keeps them.
fn edited(
    existing: &[u8],
    shown: &str,
    old: &str,
    new: &str,
    replace_all: bool,
) -> Result<(Vec<u8>, edit::Applied)> {
    let decoded = text::decode_editable(existing, shown).map_err(Error::Tool)?;
    // read shows lines without `\r`, so the model works in LF: match the file's LF view
    // and give every line back its own ending (see `line_endings`).
    let (lf_old, lf_new) = (line_endings::to_lf(old), line_endings::to_lf(new));
    let (applied, written) = if lf_old != lf_new {
        let applied = edit::apply(
            &line_endings::to_lf(&decoded.text),
            &lf_old,
            &lf_new,
            replace_all,
        )
        .map_err(Error::Tool)?;
        let written = line_endings::restore(&decoded.text, &applied.result);
        (applied, written)
    } else if old != new {
        // Only `\r` differs: the model changes line endings on purpose, byte for byte.
        let mut applied = edit::apply(&decoded.text, old, new, replace_all)
            .map_err(|err| {
                Error::Tool(format!(
                    "{err} (old and new differ only in line endings, so old must match the file's bytes, \\r included)"
                ))
            })?;
        applied.note.push_str(" (line endings changed as written)");
        let written = applied.result.clone();
        (applied, written)
    } else {
        return Err(Error::Tool("old and new must differ".into()));
    };
    let data = match decoded.legacy {
        Some(legacy) => legacy.encode(&written).map_err(|err| {
            Error::Tool(format!(
                "{shown} is {0}-encoded, and {err} in new. The file was left unchanged.",
                legacy.name()
            ))
        })?,
        None => written.into_bytes(),
    };
    Ok((data, applied))
}

/// Applies an overlay `Edit` intent to `existing` exactly as the `edit` tool would (the
/// merge replays intents with it); the error says why it does
/// not apply (no match, several matches, encoding).
pub fn replay_edit(
    existing: &[u8],
    shown: &str,
    old: &str,
    new: &str,
    replace_all: bool,
) -> std::result::Result<Vec<u8>, String> {
    if old.is_empty() {
        return Err("old must not be empty".into());
    }
    edited(existing, shown, old, new, replace_all)
        .map(|(data, _)| data)
        .map_err(|err| match err {
            Error::Tool(text) => text,
            other => other.to_string(),
        })
}

fn failed(content: String) -> ToolOutput {
    ToolOutput {
        content,
        failed: true,
        ..Default::default()
    }
}

impl LocalTools {
    pub fn new(root: PathBuf, authorizer: Arc<dyn Authorizer>) -> Result<Self> {
        let root = root.canonicalize()?;
        if !root.is_dir() {
            return Err(Error::Tool("workspace is not a directory".into()));
        }
        Ok(Self {
            root,
            authorizer,
            user_input: None,
            workspace: None,
            progress: None,
            files: Arc::new(FileTracker::default()),
            shell: Shell::current().clone(),
            ripgrep: true,
            jobs: Arc::new(jobs::Jobs::new(None)),
            view: view::FileView::Disk,
        })
    }

    /// The tools of an overlay sub-task: `read`, `glob`, `grep`
    /// and `repo_map` see `stack` (its layers above the main workspace), `write` and `edit`
    /// go only into its top layer and record intents there. Nothing reaches the disk or a
    /// checkpoint: the workspace is dropped, `stack.changed_paths()` lists the writes.
    /// Boundaries, protected paths, ignore rules and approvals stay as they are; `shell`
    /// runs read-only commands only, in the main workspace, and background commands
    /// (`run_in_background`, `shell_task`) are off. The read tracker and the job registry
    /// are new, so the sub-task shares neither with the tools it was made from.
    /// `stack.root()` must be this workspace.
    pub fn with_overlay(mut self, stack: Arc<OverlayStack>) -> Result<Self> {
        if !view::same_root(stack.root(), &self.root) {
            return Err(Error::Tool(
                "the overlay belongs to another workspace".into(),
            ));
        }
        self.workspace = None;
        self.files = Arc::new(FileTracker::default());
        self.jobs = Arc::new(jobs::Jobs::new(Some(self.jobs.dir())));
        self.view = view::FileView::Overlay(stack);
        Ok(self)
    }

    /// The overlay stack these tools write into, if they belong to an overlay sub-task.
    pub fn overlay(&self) -> Option<&Arc<OverlayStack>> {
        self.view.overlay()
    }

    /// Output files of background shell commands go under `dir` (`<data dir>/tool-outputs/shell`).
    pub fn with_job_dir(mut self, dir: PathBuf) -> Self {
        self.jobs = Arc::new(jobs::Jobs::new(Some(dir)));
        self
    }

    /// Another registry for background shell commands (tests use shorter limits).
    pub fn with_jobs(mut self, jobs: Arc<jobs::Jobs>) -> Self {
        self.jobs = jobs;
        self
    }

    /// The background shell commands of these tools.
    pub fn background_jobs(&self) -> Arc<jobs::Jobs> {
        self.jobs.clone()
    }

    /// Shell output is forwarded here line by line while the command runs.
    pub fn with_progress(mut self, progress: Arc<Progress>) -> Self {
        self.progress = Some(progress);
        self
    }

    pub fn with_user_input(mut self, input: Option<Arc<dyn UserInput>>) -> Self {
        self.user_input = input;
        self
    }

    pub fn with_workspace(mut self, workspace: Arc<Workspace>) -> Self {
        self.workspace = Some(workspace);
        self
    }

    /// The interpreter of the `shell` tool; defaults to `Shell::current()`.
    pub fn with_shell(mut self, shell: Shell) -> Self {
        self.shell = shell;
        self
    }

    /// `false` keeps grep on the built-in search even when ripgrep is installed.
    pub fn with_ripgrep(mut self, enabled: bool) -> Self {
        self.ripgrep = enabled;
        self
    }

    /// Same workspace and read tracker under another authorizer, without questions,
    /// checkpoint or live output (read-only sub-agents).
    pub fn restricted(&self, authorizer: Arc<dyn Authorizer>) -> Self {
        Self {
            authorizer,
            user_input: None,
            workspace: None,
            progress: None,
            jobs: Arc::new(jobs::Jobs::new(Some(self.jobs.dir()))),
            ..self.clone()
        }
    }

    pub fn shell(&self) -> &Shell {
        &self.shell
    }

    pub fn file_tracker(&self) -> Arc<FileTracker> {
        self.files.clone()
    }

    /// Canonical path inside the workspace. With `new`, missing parent directories are
    /// allowed; the deepest existing ancestor must still lie inside the workspace.
    /// In an overlay, a file only its layers hold resolves like a new one.
    fn resolve(&self, path: &Path, new: bool) -> Result<PathBuf> {
        match self.resolve_on_disk(path, new) {
            Err(Error::Io(err))
                if !new
                    && err.kind() == std::io::ErrorKind::NotFound
                    && self.overlay().is_some() =>
            {
                match self.resolve_on_disk(path, true) {
                    Ok(resolved) if self.view.stat(&resolved).is_ok() => Ok(resolved),
                    _ => Err(Error::Io(err)),
                }
            }
            resolved => resolved,
        }
    }

    fn resolve_on_disk(&self, path: &Path, new: bool) -> Result<PathBuf> {
        if path.as_os_str().is_empty()
            || path.components().any(|c| matches!(c, Component::ParentDir))
        {
            return Err(Error::Tool(
                "empty paths and parent traversal are not allowed".into(),
            ));
        }
        #[cfg(windows)]
        if path
            .components()
            .any(|c| matches!(c, Component::Normal(name) if name.to_string_lossy().contains(':')))
        {
            return Err(Error::Tool(
                "Windows alternate data streams are not allowed".into(),
            ));
        }
        let candidate = self.root.join(path);
        let resolved = if new && !candidate.exists() {
            let mut missing = Vec::new();
            let mut existing = candidate.as_path();
            while !existing.exists() {
                missing.push(
                    existing
                        .file_name()
                        .ok_or_else(|| Error::Tool("missing filename".into()))?
                        .to_owned(),
                );
                existing = existing
                    .parent()
                    .ok_or_else(|| Error::Tool("missing parent".into()))?;
            }
            let mut resolved = existing.canonicalize()?;
            if !resolved.is_dir() {
                return Err(Error::Tool(format!(
                    "{} is not a directory",
                    search::display(&self.root, &resolved)
                )));
            }
            for name in missing.iter().rev() {
                resolved.push(name);
            }
            resolved
        } else {
            candidate.canonicalize()?
        };
        if !resolved.starts_with(&self.root) {
            return Err(Error::Tool(
                "path is outside the workspace (including link targets)".into(),
            ));
        }
        if crate::safety::protected::is_protected(&resolved) {
            return Err(Error::Tool(
                "path is inside cove's own data directory (configuration, keys, sessions); tools cannot access it".into(),
            ));
        }
        let relative = resolved
            .strip_prefix(&self.root)
            .map_err(|_| Error::Tool("invalid root".into()))?;
        if relative
            .components()
            .any(|c| c.as_os_str().to_string_lossy().eq_ignore_ascii_case(".git"))
        {
            return Err(Error::Tool(
                "direct access to Git metadata is not allowed".into(),
            ));
        }
        Ok(resolved)
    }

    fn shown(&self, path: &Path) -> String {
        search::display(&self.root, path)
    }

    /// Current bytes of a file a tool is about to change, bounded by the snapshot limit.
    fn read_bounded(&self, path: &Path) -> Result<Vec<u8>> {
        let len = match self.view.stat(path)? {
            Kind::File(len) => len,
            // As reading a directory fails on disk.
            Kind::Dir => return Ok(self.view.read(path)?),
        };
        if len > FILE_LIMIT {
            return Err(Error::Tool(format!(
                "{} exceeds 1 MiB; file tools change files up to 1 MiB",
                self.shown(path)
            )));
        }
        Ok(self.view.read(path)?)
    }

    /// Replaces `path` when it still holds `expected`: on disk through the checkpoint, or
    /// in the overlay's top layer, where `edit` becomes the recorded intent.
    fn replace(
        &self,
        path: &Path,
        expected: Option<&[u8]>,
        data: &[u8],
        edit: Option<EditSpec>,
    ) -> Result<()> {
        if let Some(stack) = self.overlay() {
            let relative = path
                .strip_prefix(stack.root())
                .map_err(|_| Error::Tool("path is outside the workspace".into()))?;
            return stack.write(relative, expected, data, edit);
        }
        if let Some(workspace) = &self.workspace {
            workspace.replace_bytes(path, expected, data)
        } else {
            replace_file_bytes(path, expected, Some(data))
        }
    }

    fn files(&self) -> ignore::Walk {
        let mut walk = ignore::WalkBuilder::new(&self.root);
        walk.hidden(false).follow_links(false).require_git(false);
        walk.filter_entry(|entry| {
            entry.file_name() != ".git" && !crate::safety::protected::is_protected(entry.path())
        });
        walk.build()
    }

    fn list_directory(&self, path: &Path) -> Result<String> {
        let mut entries: Vec<String> = self
            .view
            .list(path)?
            .into_iter()
            .filter(|(name, _)| !name.eq_ignore_ascii_case(".git"))
            .map(|(name, dir)| if dir { format!("{name}/") } else { name })
            .collect();
        entries.sort();
        let total = entries.len();
        let mut out = format!("Directory: {}/ ({total} entries)\n\n", self.shown(path));
        if total == 0 {
            out.push_str("(empty)");
        }
        out.push_str(&entries[..total.min(DIRECTORY_LIMIT)].join("\n"));
        if total > DIRECTORY_LIMIT {
            out.push_str(&format!(
                "\n... and {} more (use glob to narrow)",
                total - DIRECTORY_LIMIT
            ));
        }
        Ok(out)
    }

    fn read(&self, args: ReadArgs) -> Result<String> {
        if args.offset == Some(0) || args.limit == Some(0) {
            return Err(Error::Tool("offset and limit must be >= 1".into()));
        }
        let path = self.resolve(&args.path, false)?;
        let shown = self.shown(&path);
        let len = match self.view.stat(&path)? {
            Kind::Dir => return self.list_directory(&path),
            Kind::File(len) => len,
        };
        if path
            .extension()
            .and_then(|extension| extension.to_str())
            .is_some_and(|extension| {
                IMAGE_EXTENSIONS.contains(&extension.to_ascii_lowercase().as_str())
            })
        {
            // Real images were returned before this point (`images::read_output`).
            return Err(Error::Tool(format!(
                "{shown} has an image extension but is not a readable PNG/JPEG/GIF/WebP within the size limits"
            )));
        }
        if len > READ_LIMIT {
            return Err(Error::Tool(format!(
                "{shown} exceeds 32 MiB; search it with grep or inspect parts with the shell"
            )));
        }
        let bytes = self.view.read(&path)?;
        let mut note = None;
        let content = match text::sniff(&bytes) {
            TextKind::Binary => {
                return Err(Error::Tool(format!(
                    "{shown} is a binary file; read only shows text. Use the shell (file, xxd, or the format's own tool) to inspect it."
                )));
            }
            TextKind::Utf16 => {
                // Recorded, so converting it with write afterwards is not refused as unread.
                self.files.record(&path, &bytes);
                return Err(Error::Tool(text::utf16_advice(&shown)));
            }
            TextKind::Utf8 => String::from_utf8_lossy(&bytes).into_owned(),
            TextKind::Legacy => match text::decode_gbk(&bytes) {
                Some((decoded, legacy)) => {
                    note = Some(format!(
                        "[note: file is {0}-encoded; shown decoded here, and edit/write keep it in {0}]",
                        legacy.name()
                    ));
                    decoded
                }
                None => {
                    note = Some("[note: file is not valid UTF-8, and not GBK either (probably another ANSI code page); non-ASCII text below is garbled, and edit/write will refuse this file]".into());
                    String::from_utf8_lossy(&bytes).into_owned()
                }
            },
        };
        self.files.record(&path, &bytes);
        let content = content.strip_prefix(text::UTF8_BOM).unwrap_or(&content);
        let total = content.lines().count();
        let offset = args.offset.unwrap_or(1);
        let limit = args.limit.unwrap_or(READ_WINDOW);
        // Lines are shown without `\r`; say so when the file has them, so shell edits
        // (sed, echo, cat <<EOF write LF) do not mix endings unknowingly.
        let endings =
            line_endings::describe(content).map_or(String::new(), |note| format!("; {note}"));
        let mut out = format!("File: {shown} ({total} lines total{endings})\n");
        if let Some(note) = note {
            out.push_str(&note);
            out.push('\n');
        }
        out.push('\n');
        let mut last = offset - 1;
        for (index, line) in content.lines().enumerate().skip(offset - 1).take(limit) {
            let row = format!(
                "{}: {}\n",
                index + 1,
                crate::text::clip_counted(line, READ_LINE_CHARS)
            );
            if out.len() + row.len() > READ_OUTPUT && last >= offset {
                break;
            }
            out.push_str(&row);
            last = index + 1;
        }
        if last < offset {
            out.push_str("(no lines in range)\n");
        } else if last < total {
            out.push_str(&format!(
                "... [showing lines {offset}-{last} of {total}]\n[next: offset={}]",
                last + 1
            ));
        }
        Ok(out.trim_end().to_owned())
    }

    fn write(&self, args: WriteArgs) -> Result<String> {
        let path = self.resolve(&args.path, true)?;
        let shown = self.shown(&path);
        // An overlay holds new directories implicitly; nothing is created on disk.
        if let Some(parent) = path.parent()
            && self.overlay().is_none()
            && !parent.exists()
        {
            std::fs::create_dir_all(parent)?;
            // A link swapped in while creating directories must not redirect the write.
            if !parent.canonicalize()?.starts_with(&self.root) {
                return Err(Error::Tool("parent directory left the workspace".into()));
            }
        }
        let lines = args.content.lines().count();
        // read_bounded checks the 1 MiB limit before reading; a plain read first would
        // allocate an oversize file in full, then read it again.
        let existing = match self.read_bounded(&path) {
            Ok(bytes) => Some(bytes),
            Err(Error::Io(err)) if err.kind() == std::io::ErrorKind::NotFound => None,
            Err(err) => return Err(err),
        };
        let Some(existing) = existing else {
            let content = line_endings::for_new_file(&self.root, &path, &args.content);
            let data = content.as_bytes();
            self.replace(&path, None, data, None)?;
            self.files.record(&path, data);
            return Ok(format!(
                "Created {shown} ({} bytes, {lines} lines)",
                data.len()
            ));
        };
        self.files
            .check(&path, &shown, &existing)
            .map_err(Error::Tool)?;
        let decoded = text::decode_editable(&existing, &shown).map_err(Error::Tool)?;
        let content = text::match_existing(&args.content, &decoded.text);
        let data = match decoded.legacy {
            Some(legacy) => legacy.encode(&content).map_err(|err| {
                Error::Tool(format!(
                    "{shown} is {0}-encoded, and {err} in content. The file was left unchanged; use characters {0} has, or convert the file to UTF-8 first.",
                    legacy.name()
                ))
            })?,
            None => content.into_bytes(),
        };
        self.replace(&path, Some(&existing), &data, None)?;
        self.files.record(&path, &data);
        Ok(format!(
            "Overwrote {shown} ({} bytes, {lines} lines{})",
            data.len(),
            decoded
                .legacy
                .map_or(String::new(), |legacy| format!(", kept {}", legacy.name()))
        ))
    }

    fn edit(&self, args: EditArgs) -> Result<String> {
        if args.old.is_empty() {
            return Err(Error::Tool("old must not be empty".into()));
        }
        let path = self.resolve(&args.path, false)?;
        let shown = self.shown(&path);
        let existing = self.read_bounded(&path)?;
        let (data, applied) = edited(&existing, &shown, &args.old, &args.new, args.replace_all)?;
        let edit = EditSpec {
            old: args.old,
            new: args.new,
            replace_all: args.replace_all,
        };
        self.replace(&path, Some(&existing), &data, Some(edit))?;
        self.files.record(&path, &data);
        let mut out = format!(
            "Edited {shown}: {} replacement(s){}",
            applied.replacements, applied.note
        );
        let snippets = edit::snippets(&applied.result, &applied.spots, applied.inserted_len);
        if applied.note.is_empty() && snippets.len() > SHORT_SNIPPET {
            // An exact replacement wrote what the model just sent: where it landed is enough.
            out.push_str(&format!(
                " at {}",
                edit::regions(&applied.result, &applied.spots, applied.inserted_len)
            ));
            if applied.replacements > applied.spots.len() {
                out.push_str(&format!(" (first {} listed)", applied.spots.len()));
            }
        } else if !snippets.is_empty() {
            out.push('\n');
            out.push_str(&snippets);
            if applied.replacements > applied.spots.len() {
                out.push_str(&format!(
                    "\n(first {} of {} replacements shown)",
                    applied.spots.len(),
                    applied.replacements
                ));
            }
        }
        Ok(out)
    }

    fn search_base(&self, path: Option<&Path>) -> Result<PathBuf> {
        match path {
            Some(path) if !path.as_os_str().is_empty() && path != Path::new(".") => {
                self.resolve(path, false)
            }
            _ => Ok(self.root.clone()),
        }
    }

    fn grep_options(args: &GrepArgs) -> search::GrepOptions {
        search::GrepOptions {
            pattern: args.pattern.clone(),
            include: args
                .include
                .clone()
                .filter(|include| !include.trim().is_empty()),
            ignore_case: args.ignore_case,
            context: args.context.unwrap_or(0).min(search::GREP_MAX_CONTEXT),
            files_only: args.files_only,
            include_ignored: args.include_ignored,
        }
    }

    fn local(&self, call: Call, cancel: &CancellationToken) -> Result<String> {
        if cancel.is_cancelled() {
            return Err(Error::Cancelled);
        }
        match call {
            Call::Read(args) => self.read(args),
            Call::Write(args) => self.write(args),
            Call::Edit(args) => self.edit(args),
            Call::Glob(args) => {
                let base = self.search_base(args.path.as_deref())?;
                if !matches!(self.view.stat(&base), Ok(Kind::Dir)) {
                    return Err(Error::Tool("glob path must be a directory".into()));
                }
                let shadow = self.view.shadow();
                search::glob(
                    &self.root,
                    &base,
                    &args.pattern,
                    args.include_ignored,
                    shadow.as_ref(),
                    cancel,
                    &|path| self.resolve(path, false).ok(),
                )
            }
            Call::Grep(args) => {
                let base = self.search_base(args.path.as_deref())?;
                let options = Self::grep_options(&args);
                let shadow = self.view.shadow();
                let found = search::builtin_grep(
                    &self.root,
                    &base,
                    &options,
                    shadow.as_ref(),
                    cancel,
                    &|path| self.resolve(path, false).ok(),
                )?;
                Ok(search::grep_result(
                    found, false, &options, &self.root, &base,
                ))
            }
            Call::Todo(args) => todo::execute(args),
            Call::RepoMap(args) => {
                let prefix = match &args.path {
                    Some(path) if !path.as_os_str().is_empty() && path != Path::new(".") => {
                        let resolved = self.resolve(path, false)?;
                        if !matches!(self.view.stat(&resolved), Ok(Kind::Dir)) {
                            return Err(Error::Tool("path must be a directory".into()));
                        }
                        resolved
                    }
                    _ => self.root.clone(),
                };
                let shadow = self.view.shadow();
                let mut files = Vec::new();
                // An overlay's files are mapped from their layered content, and come first:
                // past the scan's file limit they are what the sub-task is working on.
                let mut contents = std::collections::HashMap::new();
                let prefix_key = crate::paths::key(&prefix);
                for (path, bytes) in shadow.iter().flat_map(|shadow| &shadow.files) {
                    if !search::under(&prefix_key, path) || search::ignored(&self.root, path) {
                        continue;
                    }
                    let Ok(full) = self.resolve(path, false) else {
                        continue;
                    };
                    if let Ok(relative) = full.strip_prefix(&self.root) {
                        contents.insert(relative.to_path_buf(), bytes.clone());
                        files.push((relative.to_path_buf(), full.clone()));
                    }
                }
                for entry in self.files() {
                    if cancel.is_cancelled() {
                        return Err(Error::Cancelled);
                    }
                    let Ok(entry) = entry else { continue };
                    if !entry.file_type().is_some_and(|kind| kind.is_file())
                        || shadow
                            .as_ref()
                            .is_some_and(|shadow| shadow.hides(&self.root, entry.path()))
                    {
                        continue;
                    }
                    let Ok(full) = self.resolve(entry.path(), false) else {
                        continue;
                    };
                    if !full.starts_with(&prefix) {
                        continue;
                    }
                    if let Ok(relative) = full.strip_prefix(&self.root) {
                        files.push((relative.to_path_buf(), full.clone()));
                    }
                    if files.len() >= 20_000 {
                        break;
                    }
                }
                let terms: Vec<String> = args
                    .query
                    .split(|c: char| c.is_whitespace() || c == ',' || c == '|')
                    .map(str::to_owned)
                    .collect();
                let map =
                    repomap::query_with(files.into_iter(), &contents, &terms, 12 * 1024, cancel)?;
                Ok(if map.is_empty() {
                    format!(
                        "No files match (only {} source files are mapped); try other terms, grep or glob.",
                        repomap::LANGUAGES
                    )
                } else {
                    map
                })
            }
            Call::Shell(_) | Call::ShellTask(_) | Call::Ask(_) | Call::Question(_) => {
                unreachable!()
            }
        }
    }

    async fn ask(&self, call: Call, cancel: CancellationToken) -> Result<ToolOutput> {
        let Some(input) = &self.user_input else {
            return Err(Error::Approval(
                "没有交互终端，无法向你提问；请在交互模式下运行，或在任务描述中写明所需信息".into(),
            ));
        };
        match call {
            Call::Ask(args) => {
                if args.question.trim().is_empty() || args.question.len() > 4096 {
                    return Ok(failed("question must be nonempty and <= 4096 bytes".into()));
                }
                let content = input
                    .answer(format!("{}\n> ", args.question), cancel)
                    .await?;
                Ok(ToolOutput {
                    content,
                    failed: false,
                    ..Default::default()
                })
            }
            Call::Question(args) => {
                if let Err(err) = args.validate() {
                    return Ok(failed(err.to_string()));
                }
                let mut answers = Vec::new();
                // The first question answered in the user's own words: later ones may no
                // longer apply, so the user may skip them; the user decides, not the tool.
                let mut own_words = None;
                for index in 0..args.len() {
                    if cancel.is_cancelled() {
                        return Err(Error::Cancelled);
                    }
                    let mut prompt = args.prompt(index);
                    if own_words.is_some() {
                        prompt.insert_str(
                            0,
                            "（上一题是你自己写的回答；这题不再适用可直接回车跳过）\n",
                        );
                    }
                    let raw = input.answer(prompt, cancel.clone()).await?;
                    answers.push(args.answer(index, &raw));
                    if own_words.is_none() && args.own_words(index, &raw) && index + 1 < args.len()
                    {
                        own_words = Some(index + 1);
                    }
                }
                if let Some(number) = own_words {
                    answers.push(format!(
                        "The user answered Q{number} in their own words; read the later answers in its light, and treat (no answer) as skipped."
                    ));
                }
                Ok(ToolOutput {
                    content: answers.join("\n\n"),
                    failed: false,
                    ..Default::default()
                })
            }
            _ => unreachable!(),
        }
    }

    /// Lines stripped from shell results for the model (the terminal still shows
    /// everything live): git's per-file CRLF warnings, repeated on every git command,
    /// and cargo's progress boilerplate.
    fn quiet(text: &str) -> String {
        const CARGO_PROGRESS: &[&str] = &[
            "Compiling ",
            "Finished ",
            "Downloading ",
            "Downloaded ",
            "Updating ",
            "Locking ",
        ];
        text.lines()
            .filter(|line| {
                let trimmed = line.trim_start();
                !trimmed.starts_with("warning: in the working copy of '")
                    && !trimmed.starts_with("warning: LF will be replaced by CRLF")
                    && !trimmed.starts_with("warning: CRLF will be replaced by LF")
                    && !CARGO_PROGRESS.iter().any(|verb| trimmed.starts_with(verb))
            })
            .collect::<Vec<_>>()
            .join("\n")
    }

    async fn run_shell(&self, args: ShellArgs, cancel: CancellationToken) -> Result<ToolOutput> {
        let seconds = match (args.timeout_seconds, args.timeout) {
            (Some(seconds), _) => seconds,
            (None, Some(millis)) => millis.div_ceil(1000),
            (None, None) => SHELL_TIMEOUT,
        };
        if args.command.trim().is_empty()
            || args.command.len() > 64 * 1024
            || !(1..=SHELL_MAX_TIMEOUT).contains(&seconds)
        {
            return Ok(failed(format!(
                "shell requires a nonempty command <= 64 KiB and timeout 1..{SHELL_MAX_TIMEOUT} seconds"
            )));
        }
        if let Some(workspace) = &self.workspace {
            workspace.note_shell()?;
        }
        // Routed like the permission check judged it, unless a shell was set explicitly.
        let shell = if self.shell == *Shell::current() {
            Shell::route(&args.command)
        } else {
            &self.shell
        };
        // Every command runs while it keeps working (output, CPU or I/O); `seconds` is
        // how long this call waits before it goes on in the background.
        let (kind, wait) = if args.run_in_background {
            (jobs::Kind::Detached, jobs::DETACHED_WAIT)
        } else {
            (jobs::Kind::Moved, Duration::from_secs(seconds))
        };
        let (id, ran) = self
            .shell_job(shell, &args.command, kind, wait, &cancel)
            .await?;
        let mut output = match ran {
            jobs::Foreground::Done(output) => output,
            jobs::Foreground::Moved => {
                return Ok(self.backgrounded(id, kind, wait, &args.command, "").await);
            }
        };
        let mut note = String::new();
        if args.run_in_background {
            note.push_str(&format!(
                "[note: the command ended within {}s, so it is not running in the background]\n",
                wait.as_secs()
            ));
        }
        if (output.code != Some(0) && !output.timed_out && output.stalled.is_none())
            && let Some(mismatch) = dialect_mismatch(shell, &output)
        {
            match retry_shell(shell, &args.command, mismatch) {
                Some(other) => {
                    let retried = format!(
                        "[note: {} could not run this ({mismatch}); it was run read-only in {} instead. Write {} syntax next time.]\n",
                        shell.dialect(),
                        other.dialect(),
                        other.dialect()
                    );
                    let (id, ran) = self
                        .shell_job(other, &args.command, kind, wait, &cancel)
                        .await?;
                    output = match ran {
                        jobs::Foreground::Done(output) => output,
                        jobs::Foreground::Moved => {
                            return Ok(self
                                .backgrounded(id, kind, wait, &args.command, &retried)
                                .await);
                        }
                    };
                    note.push_str(&retried);
                }
                None => {
                    note.push_str(&format!(
                        "[hint: {} could not run this ({mismatch}). The shell tool runs {}; rewrite the command in that syntax.]\n",
                        shell.dialect(),
                        shell.describe()
                    ));
                }
            }
        }
        if let Some(reason) = &output.stalled {
            note.push_str(&format!(
                "[stalled: stopped for lack of any activity] {reason}\n"
            ));
        }
        Ok(ToolOutput {
            failed: output.code != Some(0) || output.timed_out || output.stalled.is_some(),
            content: format!(
                "{note}exit_code={:?} timed_out={} truncated={} encoding_warning={}\nstdout:\n{}\nstderr:\n{}",
                output.code,
                output.timed_out,
                output.truncated,
                output.encoding_warning,
                Self::quiet(&output.stdout),
                Self::quiet(&output.stderr)
            ),
            ..Default::default()
        })
    }

    /// Starts `command` in the job registry and waits for it in the foreground.
    async fn shell_job(
        &self,
        shell: &Shell,
        command: &str,
        kind: jobs::Kind,
        wait: Duration,
        cancel: &CancellationToken,
    ) -> Result<(u32, jobs::Foreground)> {
        let id = self.jobs.start(
            shell.clone(),
            self.root.clone(),
            command.to_owned(),
            kind,
            // A longer wait than the idle limit says the command may stay quiet that long.
            (kind == jobs::Kind::Moved).then_some(wait),
            self.progress.clone(),
            self.workspace.clone(),
        );
        let ran = self
            .jobs
            .wait_foreground(id, wait, kind == jobs::Kind::Detached, cancel)
            .await?;
        Ok((id, ran))
    }

    /// The result of a call whose command runs on in the background: what it printed
    /// so far, and how its result reaches the model.
    async fn backgrounded(
        &self,
        id: u32,
        kind: jobs::Kind,
        wait: Duration,
        command: &str,
        note: &str,
    ) -> ToolOutput {
        let (stdout, stderr, truncated, file) = self.jobs.snapshot(id).unwrap_or_default();
        let (total, idle) = self.jobs.limits();
        let idle = idle.max(wait);
        let (head, user) = match kind {
            jobs::Kind::Moved => (
                format!(
                    "[background #{id}] Still running after {}s, so it was moved to the background (not stopped). Full output: {file}\n\
                     It runs on until it exits, shows no activity for {}s, or reaches {}s in total; its result is delivered to you automatically when it ends. \
                     Do not poll or sleep: continue with other work, or end your reply and the task waits for it. Check or stop it with shell_task (id {id}).",
                    wait.as_secs(),
                    idle.as_secs(),
                    total.as_secs()
                ),
                format!(
                    "命令运行超过 {}，已转到后台继续（#{id}）；完成后会自动交给模型",
                    process::clock(wait)
                ),
            ),
            jobs::Kind::Detached => (
                format!(
                    "[background #{id}] Started in the background. Full output: {file}\n\
                     It runs until stopped (shell_task stop, id {id}) or the session ends; it is not waited for when your turn ends, and if it exits on its own its result is delivered to you. Do not poll or sleep."
                ),
                format!(
                    "已在后台启动（#{id}）：{}；停止前一直运行，会话结束时自动停止",
                    crate::text::short(command, 60)
                ),
            ),
        };
        if let Some(progress) = &self.progress {
            progress.notice(user).await;
        }
        ToolOutput {
            content: format!(
                "{head}\n{note}exit_code=None timed_out=false truncated={truncated} encoding_warning=false\nstdout:\n{}\nstderr:\n{}",
                Self::quiet(&stdout),
                Self::quiet(&stderr)
            ),
            failed: false,
            ..Default::default()
        }
    }

    async fn shell_task(&self, args: ShellTaskArgs) -> ToolOutput {
        let missing = |id: u32| {
            failed(format!(
                "no background shell command #{id}; shell_task list shows them"
            ))
        };
        let reply = |content: String| ToolOutput {
            content,
            ..Default::default()
        };
        match (args.action.as_str(), args.id()) {
            ("list", _) => reply(self.jobs.list()),
            ("output", Some(id)) => self.jobs.output(id).map_or_else(|| missing(id), reply),
            ("stop", Some(id)) => self.jobs.stop(id).await.map_or_else(|| missing(id), reply),
            ("output" | "stop", None) => failed(format!("shell_task {} needs id", args.action)),
            (action, _) => failed(format!(
                "unknown shell_task action {action}; use output, stop or list"
            )),
        }
    }

    async fn grep(&self, args: GrepArgs, cancel: CancellationToken) -> Result<String> {
        let shadow = self.view.shadow();
        // Past this many changed paths the exclusions would not fit a command line.
        let rg = search::ripgrep().filter(|_| {
            self.ripgrep
                && shadow
                    .as_ref()
                    .is_none_or(|shadow| shadow.hidden.len() <= RG_EXCLUDES)
        });
        let Some(rg) = rg else {
            let tools = self.clone();
            return tokio::task::spawn_blocking(move || tools.local(Call::Grep(args), &cancel))
                .await
                .map_err(|err| Error::Tool(format!("grep task failed: {err}")))?;
        };
        let base = self.search_base(args.path.as_deref())?;
        let options = Self::grep_options(&args);
        let target = self.shown(&base);
        let Some(shadow) = shadow else {
            let found =
                search::ripgrep_search(rg, &self.root, &target, &options, &[], &cancel).await?;
            return Ok(search::grep_result(
                found, true, &options, &self.root, &base,
            ));
        };
        // rg searches the disk without what the layers changed; those are searched here.
        // A base only the layers hold (a new directory) has nothing on disk for rg.
        let mut found = if shadow.hides(&self.root, &base) || !base.exists() {
            search::Found::default()
        } else {
            search::ripgrep_search(rg, &self.root, &target, &options, &shadow.hidden, &cancel)
                .await?
        };
        let printed = !found.lines.is_empty();
        let tools = self.clone();
        let (root, layered_base) = (self.root.clone(), base.clone());
        let layered = tokio::task::spawn_blocking(move || {
            let options = Self::grep_options(&args);
            search::grep_layers(&root, &layered_base, &options, &shadow, printed, &|path| {
                tools.resolve(path, false).ok()
            })
        })
        .await
        .map_err(|err| Error::Tool(format!("grep task failed: {err}")))??;
        found.lines.extend(layered);
        Ok(search::grep_result(
            found, true, &options, &self.root, &base,
        ))
    }
}

impl ToolExecutor for LocalTools {
    fn definitions(&self) -> Vec<ToolDefinition> {
        fn definition(
            name: &str,
            description: &str,
            properties: Value,
            required: &[&str],
        ) -> ToolDefinition {
            ToolDefinition {
                name: name.into(),
                description: description.into(),
                schema: json!({"type":"object","properties":properties,"required":required,"additionalProperties":false}),
            }
        }
        const IGNORED_PARAM: &str = "also search files .gitignore excludes (node_modules, build output; stops after 20 s); use only when needed";
        let mut definitions = vec![
            definition(
                "read",
                "Read a workspace file as numbered lines, or list a directory (subdirectories end with /). \
                 Shows 2000 lines from offset (1-based) unless limit is given; lines over 2000 characters are clipped. \
                 When more remains the result ends with [next: offset=N]. GBK/GB18030 files are decoded; binary and UTF-16 files are refused.",
                json!({
                    "path":{"type":"string"},
                    "offset":{"type":"integer","minimum":1,"description":"first line, 1-based"},
                    "limit":{"type":"integer","minimum":1,"description":"number of lines"}
                }),
                &["path"],
            ),
            definition(
                "write",
                "Create a file (parent directories are created) or replace a whole file. Replacing an existing file requires having read its current version in this session. \
                 An existing file keeps its line endings (CRLF), UTF-8 BOM and GBK encoding. Prefer edit for partial changes.",
                json!({
                    "path":{"type":"string"},"content":{"type":"string"}
                }),
                &["path", "content"],
            ),
            definition(
                "edit",
                "Replace exact text in an existing file; old must match exactly once unless replace_all=true (a multiple match lists its line numbers). \
                 Write old/new with LF; CRLF files and GBK encoding are kept. If old differs only in whitespace from one unique block, that block is replaced and new is re-indented to the file. \
                 The result shows the changed lines with context.",
                json!({
                    "path":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"},
                    "replace_all":{"type":"boolean"}
                }),
                &["path", "old", "new"],
            ),
            definition(
                "glob",
                "Find workspace files by glob; respects ignore files. A pattern without / matches file names anywhere (*.rs); with / it matches the path relative to path, and ** crosses directories (src/**/*.ts). At most 200 results. \
                 To list what .gitignore excludes (e.g. build output to clean up), run git status --short --ignored or git clean -ndX first.",
                json!({
                    "pattern":{"type":"string"},
                    "path":{"type":"string","description":"directory to search (default: workspace)"},
                    "include_ignored":{"type":"boolean","description":IGNORED_PARAM}
                }),
                &["pattern"],
            ),
            definition(
                "grep",
                "Regex search in workspace files (ripgrep when installed, else built in); respects ignore files. Output lines are path:line:text (context lines use -). At most 100 lines.",
                json!({
                    "pattern":{"type":"string"},
                    "include":{"type":"string","description":"file glob, e.g. *.rs"},
                    "path":{"type":"string","description":"file or directory to search"},
                    "ignore_case":{"type":"boolean"},
                    "context":{"type":"integer","minimum":0,"maximum":10},
                    "files_only":{"type":"boolean","description":"list matching files only"},
                    "include_ignored":{"type":"boolean","description":IGNORED_PARAM}
                }),
                &["pattern"],
            ),
            definition(
                "repo_map",
                &format!(
                    "Repository map: source files with their types, functions and methods (signature and line), most referenced first. \
                     query: space-separated path fragments or identifiers (case-insensitive), plus the files most connected to the matches; \
                     path: only files under this directory; neither: the most referenced files. Capped at 12 KiB. Maps {}.",
                    repomap::LANGUAGES
                ),
                json!({"query":{"type":"string"},"path":{"type":"string"}}),
                &[],
            ),
            definition(
                "shell",
                &format!(
                    "Run a command in {} in the workspace directory; write that shell's syntax. A command plainly written for another installed shell (PowerShell cmdlets, cmd %VAR%) runs in that shell. Non-interactive: no pager, editor or credential prompt, colours off. \
                     Waits up to timeout_seconds (default 120); a command still running then moves to the background, is not stopped, and its result is delivered to you when it ends. \
                     Any command is stopped after 180 s without activity (output, CPU or I/O), or after timeout_seconds when that is longer (give ssh, sleep and other long-silent commands a long timeout_seconds), and at 3600 s in total. \
                     run_in_background: true starts a command that keeps running (dev server, watcher) and returns after a few seconds with its id and first output; it runs until stopped or the session ends. \
                     Long output keeps its head and tail (stdout ~30 KB, stderr ~10 KB). Host access is NOT sandboxed. \
                     Not for reading, searching or editing files.",
                    self.shell.describe()
                ),
                json!({
                    "command":{"type":"string"},
                    "description":{"type":"string","description":"what the command does, in a few words"},
                    "timeout_seconds":{"type":"integer","minimum":1,"maximum":SHELL_MAX_TIMEOUT},
                    "run_in_background":{"type":"boolean"}
                }),
                &["command"],
            ),
            definition(
                "shell_task",
                "Check or stop shell commands running in the background (moved there at timeout_seconds, or started with run_in_background). \
                 Each one's result is delivered to you automatically when it ends: do not poll, sleep or wait for it. \
                 output: its state and latest output; stop: end it and its child processes; list: all of them.",
                json!({
                    "action":{"enum":["output","stop","list"]},
                    "id":{"type":"integer","minimum":1}
                }),
                &["action"],
            ),
            definition(
                "todo",
                "Replace the task plan for multi-step work. Send the full list each time; at most one item in_progress. Mark items completed only after doing them, cancelled when dropped. \
                 Items get ids todo-1, todo-2, ... unless given; depends lists ids that must finish first. \
                 For a plan you will run with execute_plan: writes lists the only files the item may change (workspace-relative files, directories or globs, optionally file::symbol), \
                 which execute_plan enforces per file; estimate_minutes is your estimate for the item.",
                json!({"todos":{"type":"array","maxItems":50,"items":{"type":"object","properties":{
                    "content":{"type":"string"},
                    "status":{"enum":["pending","in_progress","completed","cancelled"]},
                    "priority":{"enum":["high","medium","low"]},
                    "id":{"type":"string"},
                    "depends":{"type":"array","items":{"type":"string"}},
                    "writes":{"type":"array","maxItems":64,"items":{"type":"string"}},
                    "estimate_minutes":{"type":"number","minimum":0}
                },"required":["content","status"],"additionalProperties":false}}}),
                &["todos"],
            ),
        ];
        if self.overlay().is_some() {
            definitions.retain(|definition| definition.name != "shell_task");
            if let Some(shell) = definitions.iter_mut().find(|d| d.name == "shell") {
                if let Some(properties) = shell.schema["properties"].as_object_mut() {
                    properties.remove("run_in_background");
                }
                shell.description.push_str(
                    " In this overlay sub-task only read-only commands run (no redirects into files, file-changing commands, builds, generators or formatters), in the main workspace without your overlay changes, and nothing runs in the background.",
                );
            }
        }
        if self.user_input.is_some() {
            definitions.push(definition(
                "question",
                "Ask the user one or more questions when requirements need clarification or a choice. The task waits for the answer and continues. Options are shown numbered; the user answers by number(s) or with free text. Put questions together only when they do not depend on each other's answers.",
                json!({"questions":{"type":"array","minItems":1,"maxItems":8,"items":{"type":"object","properties":{
                    "question":{"type":"string"},
                    "header":{"type":"string","description":"short topic label"},
                    "options":{"type":"array","maxItems":12,"items":{"type":"object","properties":{
                        "label":{"type":"string"},"description":{"type":"string"}
                    },"required":["label"],"additionalProperties":false}},
                    "multiple":{"type":"boolean","description":"allow several options"}
                },"required":["question"],"additionalProperties":false}}}),
                &["questions"],
            ));
        }
        definitions
    }

    fn concurrent(&self, name: &str, _input: &Value) -> bool {
        matches!(name, "read" | "glob" | "grep" | "repo_map")
    }

    fn jobs(&self) -> Option<Arc<jobs::Jobs>> {
        Some(self.jobs.clone())
    }

    fn metered(&self, _name: &str) -> bool {
        false
    }

    fn execute(
        &self,
        name: String,
        input: Value,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ToolOutput>> {
        Box::pin(async move {
            let call = match Call::parse(&name, input) {
                Ok(call) => call,
                Err(err) => return Ok(failed(err.to_string())),
            };
            if matches!(call, Call::Ask(_) | Call::Question(_)) {
                return self.ask(call, cancel).await;
            }
            if self.overlay().is_some() {
                match &call {
                    Call::Shell(args) if args.run_in_background => {
                        return Ok(failed(format!(
                            "{OVERLAY_SHELL_REFUSED} (run_in_background is off in overlay sub-tasks)"
                        )));
                    }
                    Call::Shell(args) if !overlay_shell_allowed(&args.command) => {
                        return Ok(failed(format!(
                            "{OVERLAY_SHELL_REFUSED} (only read-only commands run here: no redirects into files, file-changing commands, builds, generators or formatters)"
                        )));
                    }
                    Call::ShellTask(_) => {
                        return Ok(failed(
                            "shell_task is off in overlay sub-tasks: they run no background commands".into(),
                        ));
                    }
                    _ => {}
                }
            }
            let effect = match &call {
                Call::Write(args) => self.resolve(&args.path, true).map(Effect::Write),
                Call::Edit(args) => self.resolve(&args.path, false).map(Effect::Write),
                Call::Shell(args) => Ok(Effect::Shell(args.command.clone())),
                _ => Ok(Effect::Read),
            };
            let effect = match effect {
                Ok(effect) => effect,
                Err(err) => return Ok(failed(err.to_string())),
            };
            let approval = self.authorizer.authorize(effect, cancel.clone());
            match (&call, &self.workspace) {
                // After a refusal, a script this task's file tools wrote asks.
                (Call::Shell(_), Some(workspace)) => {
                    let changed = workspace.changed_paths().unwrap_or_default();
                    crate::safety::permission::with_written(
                        &self.root,
                        workspace.root(),
                        &changed,
                        approval,
                    )
                    .await?
                }
                _ => approval.await?,
            }
            // Images (sniffed by content) go to the model as native image parts.
            // Decoding/resizing/base64 runs off the async workers.
            if let Call::Read(args) = &call {
                // A file an overlay changed is read from its layer, never as a disk image.
                let path = self
                    .resolve(&args.path, false)
                    .ok()
                    .filter(|path| !self.view.shadows(path));
                let image = tokio::task::spawn_blocking(move || {
                    crate::model::images::read_output(path.as_deref())
                })
                .await
                .map_err(|err| Error::Tool(format!("image read task failed: {err}")))?;
                if let Some(image) = image {
                    return Ok(image);
                }
            }
            let result = match call {
                Call::Shell(args) => {
                    let mut output = self.run_shell(args, cancel).await?;
                    if self.overlay().is_some() {
                        output.content.push('\n');
                        output.content.push_str(OVERLAY_SHELL_NOTE);
                    }
                    return Ok(output);
                }
                Call::ShellTask(args) => return Ok(self.shell_task(args).await),
                Call::Grep(args) => self.grep(args, cancel).await,
                call => {
                    let tools = self.clone();
                    tokio::task::spawn_blocking(move || tools.local(call, &cancel))
                        .await
                        .map_err(|err| Error::Tool(format!("local tool task failed: {err}")))?
                }
            };
            match result {
                Ok(content) => Ok(ToolOutput {
                    content,
                    failed: false,
                    ..Default::default()
                }),
                Err(err @ (Error::Cancelled | Error::Uncertain(_))) => Err(err),
                Err(err) => Ok(failed(err.to_string())),
            }
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn output(stderr: &str) -> process::CommandOutput {
        process::CommandOutput {
            code: Some(1),
            stdout: String::new(),
            stderr: stderr.into(),
            truncated: false,
            timed_out: false,
            stalled: None,
            encoding_warning: false,
        }
    }

    #[test]
    fn dialect_mismatches_are_read_from_english_and_chinese_errors() {
        let shell = |kind| Shell {
            kind,
            path: "x".into(),
        };
        let ps = shell(process::ShellKind::PowerShell);
        assert_eq!(
            dialect_mismatch(&ps, &output("标记“&&”不是此版本中的有效语句分隔符。")),
            Some("syntax error")
        );
        assert_eq!(
            dialect_mismatch(
                &ps,
                &output("无法将“grep”项识别为 cmdlet、函数、脚本文件或可运行程序的名称。")
            ),
            Some("command not found")
        );
        let bash = shell(process::ShellKind::Posix);
        assert_eq!(
            dialect_mismatch(&bash, &output("bash: Get-ChildItem: command not found")),
            Some("command not found")
        );
        let cmd = shell(process::ShellKind::Cmd);
        assert_eq!(
            dialect_mismatch(
                &cmd,
                &output("'grep' 不是内部或外部命令，也不是可运行的程序")
            ),
            Some("command not found")
        );
        assert_eq!(dialect_mismatch(&bash, &output("error: test failed")), None);
    }
}
