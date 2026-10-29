//! Model-facing tools: files, search, shell, questions, todo, repo map, web, drawing and browser, composed into one executor.

pub mod browser;
pub mod draw;
pub mod extension_tools;
pub mod external;
pub mod repomap;
pub mod text_encoding;
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
    session::workspace::{Workspace, replace_file_bytes},
    tools::text_encoding::{self as text, TextKind},
};

mod edit;
mod question;
mod search;
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
const SHELL_MAX_TIMEOUT: u64 = 600;
const IMAGE_EXTENSIONS: &[&str] = &["png", "jpg", "jpeg", "gif", "webp"];

/// A JSON value as tool-result text, pretty-printed and bounded.
pub fn json_output(value: Value) -> Result<String> {
    Ok(crate::extensions::bounded(serde_json::to_string_pretty(
        &value,
    )?))
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
    /// Tools that make model requests themselves and report spend through a `Ledger`.
    /// Required, like `concurrent`: a wrapper that forgot to forward these silently
    /// lost parallel reads, so every implementation now says what it does.
    fn metered(&self, name: &str) -> bool;
    /// Read-only tools without approval prompts or side effects that the kernel may run
    /// concurrently with neighbouring calls of the same kind.
    fn concurrent(&self, name: &str) -> bool;
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
}

enum Call {
    Read(ReadArgs),
    Write(WriteArgs),
    Edit(EditArgs),
    Glob(GlobArgs),
    Grep(GrepArgs),
    RepoMap(RepoMapArgs),
    Shell(ShellArgs),
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
        })
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
    fn resolve(&self, path: &Path, new: bool) -> Result<PathBuf> {
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
        if std::fs::metadata(path)?.len() > FILE_LIMIT {
            return Err(Error::Tool(format!(
                "{} exceeds 1 MiB; file tools change files up to 1 MiB",
                self.shown(path)
            )));
        }
        Ok(std::fs::read(path)?)
    }

    fn replace(&self, path: &Path, expected: Option<&[u8]>, data: &[u8]) -> Result<()> {
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
        let mut entries: Vec<String> = std::fs::read_dir(path)?
            .flatten()
            .filter(|entry| !entry.file_name().eq_ignore_ascii_case(".git"))
            .map(|entry| {
                let name = entry.file_name().to_string_lossy().into_owned();
                if entry.file_type().is_ok_and(|kind| kind.is_dir()) {
                    format!("{name}/")
                } else {
                    name
                }
            })
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
        let metadata = std::fs::metadata(&path)?;
        if metadata.is_dir() {
            return self.list_directory(&path);
        }
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
        if metadata.len() > READ_LIMIT {
            return Err(Error::Tool(format!(
                "{shown} exceeds 32 MiB; search it with grep or inspect parts with the shell"
            )));
        }
        let bytes = std::fs::read(&path)?;
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
        let mut out = format!("File: {shown} ({total} lines total)\n");
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
                text::clip_chars(line, READ_LINE_CHARS)
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
        if let Some(parent) = path.parent()
            && !parent.exists()
        {
            std::fs::create_dir_all(parent)?;
            // A link swapped in while creating directories must not redirect the write.
            if !parent.canonicalize()?.starts_with(&self.root) {
                return Err(Error::Tool("parent directory left the workspace".into()));
            }
        }
        let lines = args.content.lines().count();
        let existing = match std::fs::read(&path) {
            Ok(_) => Some(self.read_bounded(&path)?),
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => None,
            Err(err) => return Err(err.into()),
        };
        let Some(existing) = existing else {
            let data = args.content.as_bytes();
            self.replace(&path, None, data)?;
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
        self.replace(&path, Some(&existing), &data)?;
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
        let decoded = text::decode_editable(&existing, &shown).map_err(Error::Tool)?;
        // read shows lines without `\r`, so the model writes LF; match in the file's style.
        let (old, new) = if text::uses_crlf(&decoded.text) {
            (text::to_crlf(&args.old), text::to_crlf(&args.new))
        } else {
            (args.old, args.new)
        };
        if old == new {
            return Err(Error::Tool("old and new must differ".into()));
        }
        let applied =
            edit::apply(&decoded.text, &old, &new, args.replace_all).map_err(Error::Tool)?;
        let data = match decoded.legacy {
            Some(legacy) => legacy.encode(&applied.result).map_err(|err| {
                Error::Tool(format!(
                    "{shown} is {0}-encoded, and {err} in new. The file was left unchanged.",
                    legacy.name()
                ))
            })?,
            None => applied.result.clone().into_bytes(),
        };
        self.replace(&path, Some(&existing), &data)?;
        self.files.record(&path, &data);
        let mut out = format!(
            "Edited {shown}: {} replacement(s){}",
            applied.replacements, applied.note
        );
        let snippets = edit::snippets(&applied.result, &applied.spots, applied.inserted_len);
        if !snippets.is_empty() {
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
                if !base.is_dir() {
                    return Err(Error::Tool("glob path must be a directory".into()));
                }
                search::glob(&self.root, &base, &args.pattern, cancel, &|path| {
                    self.resolve(path, false).ok()
                })
            }
            Call::Grep(args) => {
                let base = self.search_base(args.path.as_deref())?;
                let options = Self::grep_options(&args);
                let lines = search::builtin_grep(&self.root, &base, &options, cancel, &|path| {
                    self.resolve(path, false).ok()
                })?;
                Ok(search::grep_result(lines, false, &options))
            }
            Call::Todo(args) => todo::execute(args),
            Call::RepoMap(args) => {
                let prefix = match &args.path {
                    Some(path) if !path.as_os_str().is_empty() && path != Path::new(".") => {
                        let resolved = self.resolve(path, false)?;
                        if !resolved.is_dir() {
                            return Err(Error::Tool("path must be a directory".into()));
                        }
                        resolved
                    }
                    _ => self.root.clone(),
                };
                let mut files = Vec::new();
                for entry in self.files() {
                    if cancel.is_cancelled() {
                        return Err(Error::Cancelled);
                    }
                    let Ok(entry) = entry else { continue };
                    if !entry.file_type().is_some_and(|kind| kind.is_file()) {
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
                let map = repomap::query(files.into_iter(), &terms, 12 * 1024, cancel)?;
                Ok(if map.is_empty() {
                    format!(
                        "No files match (only {} source files are mapped); try other terms, grep or glob.",
                        repomap::LANGUAGES
                    )
                } else {
                    map
                })
            }
            Call::Shell(_) | Call::Ask(_) | Call::Question(_) => unreachable!(),
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
                for index in 0..args.len() {
                    if cancel.is_cancelled() {
                        return Err(Error::Cancelled);
                    }
                    let raw = input.answer(args.prompt(index), cancel.clone()).await?;
                    answers.push(args.answer(index, &raw));
                    if args.own_words(index, &raw) && index + 1 < args.len() {
                        answers.push(format!(
                            "The user answered Q{} in their own words instead of choosing, so Q{}..Q{} were not asked. Act on that reply first; ask the remaining questions again only if they still apply.",
                            index + 1,
                            index + 2,
                            args.len()
                        ));
                        break;
                    }
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
        let output = process::run_shell(
            &self.shell,
            &self.root,
            &args.command,
            Duration::from_secs(seconds),
            cancel,
            self.progress.clone(),
        )
        .await?;
        Ok(ToolOutput {
            failed: output.code != Some(0) || output.timed_out,
            content: format!(
                "exit_code={:?} timed_out={} truncated={} encoding_warning={}\nstdout:\n{}\nstderr:\n{}",
                output.code,
                output.timed_out,
                output.truncated,
                output.encoding_warning,
                output.stdout,
                output.stderr
            ),
            ..Default::default()
        })
    }

    async fn grep(&self, args: GrepArgs, cancel: CancellationToken) -> Result<String> {
        let rg = search::ripgrep().filter(|_| self.ripgrep);
        let Some(rg) = rg else {
            let tools = self.clone();
            return tokio::task::spawn_blocking(move || tools.local(Call::Grep(args), &cancel))
                .await
                .map_err(|err| Error::Tool(format!("grep task failed: {err}")))?;
        };
        let base = self.search_base(args.path.as_deref())?;
        let options = Self::grep_options(&args);
        let target = self.shown(&base);
        let lines = search::ripgrep_search(rg, &self.root, &target, &options, &cancel).await?;
        Ok(search::grep_result(lines, true, &options))
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
        let mut definitions = vec![
            definition(
                "read",
                "Read a workspace file as numbered lines, or list a directory (subdirectories end with /). \
                 Shows 2000 lines from offset (1-based) unless limit is given; lines over 2000 characters are clipped. \
                 When more remains the result ends with [next: offset=N]. GBK/GB18030 files are decoded; binary and UTF-16 files are refused. \
                 Reading a file is required before replacing it with write.",
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
                "Find workspace files by glob; respects ignore files. A pattern without / matches file names anywhere (*.rs); with / it matches the path relative to path, and ** crosses directories (src/**/*.ts). At most 200 results.",
                json!({"pattern":{"type":"string"},"path":{"type":"string","description":"directory to search (default: workspace)"}}),
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
                    "files_only":{"type":"boolean","description":"list matching files only"}
                }),
                &["pattern"],
            ),
            definition(
                "repo_map",
                &format!(
                    "Repository map: source files with their types, functions and methods (signature and line), ranked by cross-file references (PageRank). \
                     query: space-separated path fragments or identifiers, case-insensitive; also lists files most connected to the matches. \
                     path: only files under this directory. Both optional; with neither, lists the most referenced files. Capped at 12 KiB. Maps {}.",
                    repomap::LANGUAGES
                ),
                json!({"query":{"type":"string"},"path":{"type":"string"}}),
                &[],
            ),
            definition(
                "shell",
                &format!(
                    "Run a command in {} in the workspace directory; write that shell's syntax. Non-interactive: no pager, editor or credential prompt, colours off. \
                     Default timeout 120 s, at most 600 (timeout_seconds). Long output keeps its head and tail (stdout ~30 KB, stderr ~10 KB). No background jobs. Host access is NOT sandboxed. \
                     Not for reading or searching files: read, grep and glob are faster, need no approval and skip ignored directories.",
                    self.shell.describe()
                ),
                json!({
                    "command":{"type":"string"},
                    "description":{"type":"string","description":"what the command does, in a few words"},
                    "timeout_seconds":{"type":"integer","minimum":1,"maximum":SHELL_MAX_TIMEOUT}
                }),
                &["command"],
            ),
            definition(
                "todo",
                "Replace the task plan for multi-step work. Send the full list each time; at most one item in_progress. Mark items completed only after doing them, cancelled when dropped. \
                 Items get ids todo-1, todo-2, ... unless given; depends lists ids that must finish first.",
                json!({"todos":{"type":"array","maxItems":50,"items":{"type":"object","properties":{
                    "content":{"type":"string"},
                    "status":{"enum":["pending","in_progress","completed","cancelled"]},
                    "priority":{"enum":["high","medium","low"]},
                    "id":{"type":"string"},
                    "depends":{"type":"array","items":{"type":"string"}}
                },"required":["content","status"],"additionalProperties":false}}}),
                &["todos"],
            ),
        ];
        if self.user_input.is_some() {
            definitions.push(definition(
                "question",
                "Ask the user one or more questions when requirements need clarification or a choice. Options are shown numbered; the user answers by number(s) or with free text.",
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

    fn concurrent(&self, name: &str) -> bool {
        matches!(name, "read" | "glob" | "grep" | "repo_map")
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
            self.authorizer.authorize(effect, cancel.clone()).await?;
            // Images (sniffed by content) go to the model as native image parts.
            if let Call::Read(args) = &call
                && let Some(image) = crate::model::images::read_output(
                    self.resolve(&args.path, false).ok().as_deref(),
                )
            {
                return Ok(image);
            }
            let result = match call {
                Call::Shell(args) => return self.run_shell(args, cancel).await,
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
