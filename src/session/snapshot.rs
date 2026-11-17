//! Whole-tree Git snapshots that also cover Shell effects. One private bare repository
//! under `<data_dir>/checkpoints/git` holds every project's snapshots; each project has
//! its own ref (`refs/cove/<hash>`) and its own index file, so neither the project's
//! repository nor other projects are ever touched. Snapshots are built with
//! `add`/`write-tree`/`commit-tree` through the `git` CLI (no libgit2/gix dependency,
//! and the user's installed Git understands the project's `.gitignore` exactly).

use std::{
    collections::HashSet,
    path::{Component, Path, PathBuf},
    process::Stdio,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};

use futures_util::future::BoxFuture;
use serde_json::Value;
use sha2::{Digest, Sha256};
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Result, ToolDefinition},
    engine::turn::TurnHook,
    session::Session,
    tools::{ToolExecutor, ToolOutput},
};

/// Snapshots kept per project; trimming starts at `KEEP + SLACK`.
pub const KEEP: usize = 50;
const SLACK: usize = 10;
const GIT_TIMEOUT: Duration = Duration::from_secs(120);
/// How long a tool call waits for the snapshot before it; a slower one is skipped for that
/// call (the staging finishes in the background, so the next one is fast again).
pub const SNAPSHOT_BUDGET: Duration = Duration::from_secs(10);
/// Slow snapshots after which automatic snapshots stop for the session.
const SLOW_LIMIT: u32 = 2;
/// How long worked-out exclusions are reused (each costs two runs of the project's Git).
const EXCLUSIONS_TTL: Duration = Duration::from_secs(30);
/// Untracked files larger than this are tool or data files (databases, dumps, models), not
/// work worth undoing; they are left out of snapshots (OpenCode uses the same 2 MB).
const LARGE_FILE: u64 = 2 * 1024 * 1024;
/// An untracked directory with more files than this is content dropped in wholesale (an
/// unpacked SDK, a dataset), not work in progress: staging it would exceed the snapshot
/// budget on its own (about 7 ms per new file on Windows).
const BULK_FILES: usize = 2000;
/// A walk of a non-Git work tree stops after this many entries.
const WALK_LIMIT: usize = 200_000;
/// Extra project rules, in `.gitignore` syntax, at the work tree root.
pub const COVEIGNORE: &str = ".coveignore";
const DIFF_LIMIT: usize = 256 * 1024;

/// Never captured (in addition to the project's own `.gitignore`).
const EXCLUDES: &[&str] = &[
    ".git",
    "target/",
    "node_modules/",
    "dist/",
    "build/",
    ".venv/",
    "__pycache__/",
    ".next/",
    "*.exe",
    "*.dll",
    "*.so",
    "*.dylib",
];

/// The snapshot store's own empty global config: the user's global and system Git config
/// (`core.autocrlf`, `commit.gpgSign`, `core.fsmonitor`, hooks templates and the like) never
/// reach snapshot Git, as in Gemini CLI. Everything snapshots need is pinned in [`CONFIG`].
const ISOLATED_CONFIG: &str = "isolated.gitconfig";

/// Environment for every Git run on the snapshot store.
fn isolate<'a>(
    command: &'a mut tokio::process::Command,
    git_dir: &Path,
) -> &'a mut tokio::process::Command {
    command
        .env("GIT_CONFIG_GLOBAL", git_dir.join(ISOLATED_CONFIG))
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .env_remove("GIT_CONFIG_PARAMETERS")
        .env_remove("GIT_CONFIG_COUNT")
}

/// Byte-exact storage regardless of the project's attributes.
const ATTRIBUTES: &str = "* -text -filter !diff -merge -ident -working-tree-encoding\n";

/// Pinned so user config never reshapes stored bytes or parsed output.
const CONFIG: &[&str] = &[
    // With the global config isolated, a work tree owned by another account (other drives
    // on Windows) would otherwise be refused as "dubious ownership".
    "-c",
    "safe.directory=*",
    "-c",
    "core.autocrlf=false",
    "-c",
    "core.eol=lf",
    "-c",
    "core.safecrlf=false",
    "-c",
    "core.quotePath=false",
    "-c",
    "core.precomposeUnicode=true",
    "-c",
    "core.longpaths=true",
    "-c",
    "filter.lfs.smudge=",
    "-c",
    "filter.lfs.clean=",
    "-c",
    "filter.lfs.process=",
    "-c",
    "filter.lfs.required=false",
    "-c",
    "advice.addEmbeddedRepo=false",
    "-c",
    "gc.auto=0",
    "-c",
    "color.ui=false",
    "-c",
    "diff.renames=false",
    "-c",
    "user.name=cove",
    "-c",
    "user.email=cove@localhost",
];

#[derive(Clone, Debug, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct SnapshotInfo {
    pub hash: String,
    pub time: u64,
    pub label: String,
}

impl SnapshotInfo {
    pub fn short(&self) -> &str {
        &self.hash[..self.hash.len().min(10)]
    }
}

/// A planned change of a restore: what happens to `path` when the snapshot is applied.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Planned {
    /// "restore" (modified since), "recreate" (deleted since) or "remove" (created since).
    pub action: &'static str,
    pub path: String,
}

#[derive(Default)]
struct State {
    task_first: Option<String>,
    created: u32,
    /// Snapshots that missed [`SNAPSHOT_BUDGET`]; at [`SLOW_LIMIT`] they stop.
    slow: u32,
    /// What the last staging left out, for restores, and when it was worked out.
    excluded: Exclusions,
    excluded_at: Option<std::time::Instant>,
}

/// What a snapshot leaves out, derived from the project itself rather than a fixed list
/// of names:
/// 1. everything the project's own Git ignores (`.gitignore`, `.git/info/exclude` and the
///    global excludes file; a snapshot uses a separate repository, which by itself only
///    sees `.gitignore`);
/// 2. untracked hidden (dot) files and directories: tool state such as indexes, caches
///    and IDE settings (`.github/` and other dot paths the project tracks stay);
/// 3. untracked files larger than [`LARGE_FILE`];
/// 4. untracked directories holding more than [`BULK_FILES`] files;
/// 5. outside a Git project: every hidden path and files larger than [`LARGE_FILE`];
/// 6. the patterns in the project's [`COVEIGNORE`] file;
///
/// plus cove's fixed [`EXCLUDES`]. Restores never touch a path these rules cover.
#[derive(Clone, Debug, Default)]
struct Exclusions {
    /// The rules as gitignore patterns, anchored at the work tree.
    patterns: String,
    matcher: Option<ignore::gitignore::Gitignore>,
}

impl Exclusions {
    fn new(work: &Path, patterns: String) -> Self {
        let mut builder = ignore::gitignore::GitignoreBuilder::new(work);
        for line in EXCLUDES.iter().copied().chain(patterns.lines()) {
            let _ = builder.add_line(None, line);
        }
        Self {
            matcher: builder.build().ok(),
            patterns,
        }
    }

    /// Whether `path` (relative to the work tree, `/`-separated) is left out.
    fn covers(&self, path: &str) -> bool {
        self.matcher
            .as_ref()
            .is_some_and(|matcher| matcher.matched_path_or_any_parents(path, false).is_ignore())
    }
}

/// `path` as a gitignore pattern matching exactly it, anchored at the work tree.
fn anchored(path: &str) -> String {
    let mut out = String::from("/");
    for c in path.chars() {
        if matches!(c, '\\' | '*' | '?' | '[' | '!' | '#') {
            out.push('\\');
        }
        out.push(c);
    }
    out
}

/// Files larger than [`LARGE_FILE`] in a work tree without Git, skipping hidden paths,
/// ignore files' entries and cove's fixed excludes; stops after [`WALK_LIMIT`] entries.
fn large_files(work: &Path) -> Vec<String> {
    let skipped: Vec<&str> = EXCLUDES
        .iter()
        .filter_map(|pattern| pattern.strip_suffix('/'))
        .collect();
    let walker = ignore::WalkBuilder::new(work)
        .hidden(true)
        .git_ignore(true)
        .git_global(true)
        .git_exclude(true)
        .require_git(false)
        .parents(false)
        .follow_links(false)
        .filter_entry(move |entry| {
            !skipped
                .iter()
                .any(|name| entry.file_name().to_str() == Some(*name))
        })
        .build();
    let mut out = Vec::new();
    for entry in walker.flatten().take(WALK_LIMIT) {
        if entry.file_type().is_some_and(|kind| kind.is_file())
            && entry.metadata().is_ok_and(|meta| meta.len() > LARGE_FILE)
            && let Ok(relative) = entry.path().strip_prefix(work)
        {
            out.push(relative.to_string_lossy().replace('\\', "/"));
        }
    }
    out
}

/// The path up to and including its first hidden component (`a/.cache/x` → `a/.cache/`).
fn hidden_prefix(path: &str) -> Option<String> {
    let parts: Vec<&str> = path.split('/').collect();
    let index = parts.iter().position(|part| part.starts_with('.'))?;
    let prefix = parts[..=index].join("/");
    Some(if index + 1 < parts.len() {
        format!("{prefix}/")
    } else {
        prefix
    })
}

/// What [`Snapshots::before_call`] did.
#[derive(Debug, PartialEq, Eq)]
pub enum Before {
    Taken,
    /// Missed the budget; `disabled` when this turned automatic snapshots off.
    Slow {
        disabled: bool,
    },
    /// An earlier, slow snapshot is still staging in the background; skipped without
    /// counting as slow (a first snapshot of a large project takes a while once).
    Busy,
    /// Automatic snapshots are off for this session.
    Off,
    Failed(String),
}

pub struct Snapshots {
    git_dir: PathBuf,
    work: PathBuf,
    reference: String,
    index: PathBuf,
    /// This project's generated excludes (see [`Exclusions`]).
    excludes: PathBuf,
    /// A snapshot that missed its budget is still staging in the background.
    warming: Arc<AtomicBool>,
    keep: usize,
    lock: tokio::sync::Mutex<()>,
    state: std::sync::Mutex<State>,
}

fn invalid(message: impl Into<String>) -> Error {
    Error::Workspace(message.into())
}

impl Snapshots {
    /// Opens (creating when needed) the shared store; fails when Git is unavailable.
    pub async fn open(data_dir: &Path, work: &Path) -> Result<Arc<Self>> {
        Self::open_keeping(data_dir, work, KEEP).await
    }

    async fn open_keeping(data_dir: &Path, work: &Path, keep: usize) -> Result<Arc<Self>> {
        std::fs::create_dir_all(data_dir)?;
        let git_dir = plain(&data_dir.canonicalize()?)
            .join("checkpoints")
            .join("git");
        let work = plain(&work.canonicalize()?);
        let key = format!("{:x}", Sha256::digest(work.as_os_str().as_encoded_bytes()));
        let key = &key[..16];
        let snapshots = Self {
            index: git_dir.join(format!("index-{key}")),
            excludes: git_dir.join(format!("exclude-{key}")),
            warming: Arc::new(AtomicBool::new(false)),
            reference: format!("refs/cove/{key}"),
            git_dir,
            work,
            keep,
            lock: tokio::sync::Mutex::new(()),
            state: std::sync::Mutex::new(State::default()),
        };
        std::fs::create_dir_all(&snapshots.git_dir)?;
        let isolated = snapshots.git_dir.join(ISOLATED_CONFIG);
        if !isolated.is_file() {
            std::fs::write(&isolated, "")?;
        }
        if !snapshots.git_dir.join("HEAD").is_file() {
            let output = isolate(&mut tokio::process::Command::new("git"), &snapshots.git_dir)
                .arg("init")
                .arg("--bare")
                .arg("--quiet")
                .arg(&snapshots.git_dir)
                .env_remove("GIT_DIR")
                .env_remove("GIT_WORK_TREE")
                .stdin(Stdio::null())
                .kill_on_drop(true)
                .output()
                .await
                .map_err(|err| invalid(format!("找不到 git（快照需要 git）：{err}")))?;
            if !output.status.success() {
                return Err(invalid(format!(
                    "git init failed: {}",
                    String::from_utf8_lossy(&output.stderr).trim()
                )));
            }
        }
        let info = snapshots.git_dir.join("info");
        std::fs::create_dir_all(&info)?;
        let mut exclude = String::from("# Managed by cove; regenerated on startup.\n");
        for pattern in EXCLUDES {
            exclude.push_str(pattern);
            exclude.push('\n');
        }
        // cove's own data directory, when the work tree contains it.
        for dir in crate::safety::protected::below(&snapshots.work) {
            exclude.push_str(&format!("/{dir}/\n"));
        }
        std::fs::write(info.join("exclude"), exclude)?;
        std::fs::write(info.join("attributes"), ATTRIBUTES)?;
        Ok(Arc::new(snapshots))
    }

    async fn git_bytes(&self, args: &[&str]) -> Result<Vec<u8>> {
        let mut command = tokio::process::Command::new("git");
        isolate(&mut command, &self.git_dir)
            .args(CONFIG)
            .arg("-c")
            .arg(format!(
                "core.excludesFile={}",
                self.excludes.to_string_lossy().replace('\\', "/")
            ))
            .args(args)
            .current_dir(&self.work)
            .env("GIT_DIR", &self.git_dir)
            .env("GIT_WORK_TREE", &self.work)
            .env("GIT_INDEX_FILE", &self.index)
            .env("GIT_TERMINAL_PROMPT", "0")
            .env_remove("GIT_OBJECT_DIRECTORY")
            .env_remove("GIT_ALTERNATE_OBJECT_DIRECTORIES")
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .kill_on_drop(true);
        let output = tokio::time::timeout(GIT_TIMEOUT, command.output())
            .await
            .map_err(|_| invalid(format!("git {} timed out", args.first().unwrap_or(&""))))?
            .map_err(|err| invalid(format!("找不到 git（快照需要 git）：{err}")))?;
        if !output.status.success() {
            return Err(invalid(format!(
                "git {} failed: {}",
                args.first().unwrap_or(&""),
                String::from_utf8_lossy(&output.stderr).trim()
            )));
        }
        Ok(output.stdout)
    }

    async fn git(&self, args: &[&str]) -> Result<String> {
        Ok(String::from_utf8_lossy(&self.git_bytes(args).await?)
            .trim()
            .to_owned())
    }

    async fn head(&self) -> Option<String> {
        self.git(&["rev-parse", "--verify", "--quiet", &self.reference])
            .await
            .ok()
            .filter(|hash| !hash.is_empty())
    }

    async fn tree_of(&self, commit: &str) -> Result<String> {
        self.git(&["rev-parse", &format!("{commit}^{{tree}}")])
            .await
    }

    /// Stages the work tree into this project's private index and returns its tree.
    /// Nested repositories without a commit are left out, as Git cannot store them.
    async fn stage(&self) -> Result<String> {
        self.clear_stale_lock();
        let fresh = self.state.lock().is_ok_and(|state| {
            state
                .excluded_at
                .is_some_and(|at| at.elapsed() < EXCLUSIONS_TTL)
        });
        if !fresh {
            let exclusions = self.exclusions().await;
            std::fs::write(&self.excludes, &exclusions.patterns)?;
            self.forget_excluded().await?;
            if let Ok(mut state) = self.state.lock() {
                state.excluded = exclusions;
                state.excluded_at = Some(std::time::Instant::now());
            }
        }
        // File contents go into one pack per staging (bulk check-in) instead of a loose file
        // per object, which costs several milliseconds each on Windows: the first snapshot
        // of a large project takes about half as long. Only for `add`: the same setting
        // would make `diff` treat every file as binary.
        let staging = |rest: &[&str]| -> Vec<String> {
            ["-c", "core.bigFileThreshold=1", "add", "--all"]
                .iter()
                .chain(rest)
                .map(|part| (*part).to_owned())
                .collect()
        };
        let mut args: Vec<String> = staging(&[]);
        let mut excluded = Vec::new();
        for attempt in 0.. {
            let borrowed: Vec<&str> = args.iter().map(String::as_str).collect();
            match self.git(&borrowed).await {
                Ok(_) => break,
                Err(err) => {
                    let text = err.to_string();
                    let nested: Vec<String> = text
                        .split("error: '")
                        .skip(1)
                        .filter_map(|part| part.split_once("' does not have a commit checked out"))
                        .map(|(path, _)| path.to_owned())
                        .collect();
                    if nested.is_empty() || attempt >= 8 {
                        return Err(err);
                    }
                    excluded.extend(nested.into_iter().map(|path| format!(":(exclude){path}")));
                    args = staging(&["--", "."]);
                    args.extend(excluded.iter().cloned());
                }
            }
        }
        self.git(&["write-tree"]).await
    }

    /// Removes this project's index lock when it outlived any Git run (cove exited while a
    /// background staging was still going): otherwise every later snapshot would fail.
    /// Only called under `lock`, so no Git of ours is using the index right now.
    fn clear_stale_lock(&self) {
        let mut lock = self.index.clone().into_os_string();
        lock.push(".lock");
        let lock = PathBuf::from(lock);
        let stale = std::fs::metadata(&lock)
            .and_then(|meta| meta.modified())
            .ok()
            .and_then(|time| time.elapsed().ok())
            .is_some_and(|age| age > GIT_TIMEOUT * 2);
        if stale {
            let _ = std::fs::remove_file(&lock);
        }
    }

    /// The project's own Git, run in the work tree; `None` when there is none or it fails.
    async fn project_git(&self, args: &[&str]) -> Option<String> {
        let output = tokio::time::timeout(
            GIT_TIMEOUT,
            tokio::process::Command::new("git")
                .args(["-c", "core.quotePath=false"])
                .args(args)
                .current_dir(&self.work)
                .env_remove("GIT_DIR")
                .env_remove("GIT_WORK_TREE")
                .env_remove("GIT_INDEX_FILE")
                .env("GIT_TERMINAL_PROMPT", "0")
                .stdin(Stdio::null())
                .kill_on_drop(true)
                .output(),
        )
        .await
        .ok()?
        .ok()?;
        output
            .status
            .success()
            .then(|| String::from_utf8_lossy(&output.stdout).into_owned())
    }

    /// See [`Exclusions`].
    async fn exclusions(&self) -> Exclusions {
        let mut patterns =
            String::from("# Generated by cove for this project; rewritten before each snapshot.\n");
        let inside = self
            .project_git(&["rev-parse", "--is-inside-work-tree"])
            .await;
        if inside.as_deref().map(str::trim) == Some("true") {
            for path in self.project_left_out().await {
                patterns.push_str(&anchored(&path));
                patterns.push('\n');
            }
        } else {
            patterns.push_str(".*\n");
            let work = self.work.clone();
            let large = tokio::task::spawn_blocking(move || large_files(&work))
                .await
                .unwrap_or_default();
            for path in large {
                patterns.push_str(&anchored(&path));
                patterns.push('\n');
            }
        }
        if let Ok(extra) = std::fs::read_to_string(self.work.join(COVEIGNORE)) {
            patterns.push_str("# From the project's .coveignore\n");
            patterns.push_str(&extra[..extra.floor_char_boundary(extra.len().min(64 * 1024))]);
            patterns.push('\n');
        }
        Exclusions::new(&self.work, patterns)
    }

    /// Rules 1-4 of [`Exclusions`], asked of the project's own Git: work-tree-relative
    /// paths, directories ending with `/`.
    async fn project_left_out(&self) -> Vec<String> {
        let list = |args: &'static [&'static str]| async move {
            self.project_git(args)
                .await
                .unwrap_or_default()
                .split('\0')
                .filter(|path| !path.is_empty())
                .map(str::to_owned)
                .collect::<Vec<_>>()
        };
        let mut out = Vec::new();
        let mut seen = HashSet::new();
        let mut keep = |path: String, out: &mut Vec<String>| {
            if seen.insert(path.clone()) {
                out.push(path);
            }
        };
        for path in list(&[
            "ls-files",
            "-z",
            "--others",
            "--ignored",
            "--exclude-standard",
            "--directory",
        ])
        .await
        {
            keep(path, &mut out);
        }
        let untracked = list(&["ls-files", "-z", "--others", "--exclude-standard"]).await;
        for path in &untracked {
            let left_out = hidden_prefix(path).or_else(|| {
                std::fs::metadata(self.work.join(path))
                    .is_ok_and(|meta| meta.len() > LARGE_FILE)
                    .then(|| path.clone())
            });
            if let Some(left_out) = left_out {
                keep(left_out, &mut out);
            }
        }
        // Untracked directories (no tracked file inside), by the number of files in them.
        for dir in list(&[
            "ls-files",
            "-z",
            "--others",
            "--exclude-standard",
            "--directory",
        ])
        .await
        .into_iter()
        .filter(|dir| dir.ends_with('/') && hidden_prefix(dir).is_none())
        {
            let files = untracked
                .iter()
                .filter(|path| path.starts_with(&dir))
                .count();
            if files > BULK_FILES {
                keep(dir, &mut out);
            }
        }
        out
    }

    /// Drops index entries that are left out now but were staged before (snapshots taken
    /// before these rules), so they stop riding along in every later snapshot.
    async fn forget_excluded(&self) -> Result<()> {
        let listed = self
            .git_bytes(&[
                "ls-files",
                "-z",
                "--cached",
                "--ignored",
                "--exclude-standard",
            ])
            .await?;
        if listed.is_empty() {
            return Ok(());
        }
        let list = self.excludes.with_extension("forget");
        std::fs::write(&list, &listed)?;
        let file = format!("--pathspec-from-file={}", list.to_string_lossy());
        let result = self
            .git(&[
                "--literal-pathspecs",
                "rm",
                "--cached",
                "-r",
                "-f",
                "-q",
                "--ignore-unmatch",
                &file,
                "--pathspec-file-nul",
            ])
            .await;
        let _ = std::fs::remove_file(&list);
        result.map(|_| ())
    }

    fn excluded(&self) -> Exclusions {
        self.state
            .lock()
            .map(|state| state.excluded.clone())
            .unwrap_or_default()
    }

    /// The snapshot before a tool call, waiting at most `budget`. A slower one is skipped
    /// for this call: its staging still finishes in the background (so the next snapshot
    /// is fast), but it records no snapshot, since the call may already be changing files.
    /// After [`SLOW_LIMIT`] slow ones automatic snapshots stop for the session.
    pub async fn before_call(self: &Arc<Self>, label: String, budget: Duration) -> Before {
        if self
            .state
            .lock()
            .is_ok_and(|state| state.slow >= SLOW_LIMIT)
        {
            return Before::Off;
        }
        if self.warming.load(Ordering::SeqCst) {
            return Before::Busy;
        }
        let abandoned = Arc::new(AtomicBool::new(false));
        let (this, flag) = (self.clone(), abandoned.clone());
        let mut task = tokio::spawn(async move {
            let result = this.snapshot_unless(&label, &flag).await;
            if flag.load(Ordering::SeqCst) {
                this.warming.store(false, Ordering::SeqCst);
            }
            result
        });
        match tokio::time::timeout(budget, &mut task).await {
            Ok(Ok(Ok(_))) => Before::Taken,
            Ok(Ok(Err(err))) => Before::Failed(err.to_string()),
            Ok(Err(err)) => Before::Failed(err.to_string()),
            Err(_) => {
                abandoned.store(true, Ordering::SeqCst);
                self.warming.store(true, Ordering::SeqCst);
                // It may have finished between the timeout and the flags.
                if task.is_finished() {
                    self.warming.store(false, Ordering::SeqCst);
                }
                let slow = self.state.lock().map_or(SLOW_LIMIT, |mut state| {
                    state.slow += 1;
                    state.slow
                });
                Before::Slow {
                    disabled: slow >= SLOW_LIMIT,
                }
            }
        }
    }

    /// Records the work tree; returns the snapshot and whether a new one was needed.
    pub async fn snapshot(&self, label: &str) -> Result<(String, bool)> {
        self.snapshot_unless(label, &AtomicBool::new(false)).await
    }

    /// [`snapshot`](Self::snapshot), but only stages once `abandoned` is set.
    async fn snapshot_unless(&self, label: &str, abandoned: &AtomicBool) -> Result<(String, bool)> {
        let _guard = self.lock.lock().await;
        if abandoned.load(Ordering::SeqCst) {
            self.stage().await?;
            return Ok((String::new(), false));
        }
        let tree = self.stage().await?;
        if abandoned.load(Ordering::SeqCst) {
            return Ok((tree, false));
        }
        let (hash, created) = self.commit_staged(label, tree).await?;
        if let Ok(mut state) = self.state.lock() {
            if state.task_first.is_none() {
                state.task_first = Some(hash.clone());
            }
            state.created += u32::from(created);
        }
        if created {
            self.trim_locked().await;
        }
        Ok((hash, created))
    }

    async fn snapshot_locked(&self, label: &str) -> Result<(String, bool)> {
        let tree = self.stage().await?;
        self.commit_staged(label, tree).await
    }

    /// Commits the staged `tree` unless it equals the latest snapshot.
    async fn commit_staged(&self, label: &str, tree: String) -> Result<(String, bool)> {
        let parent = self.head().await;
        if let Some(parent) = &parent
            && self.tree_of(parent).await.ok().as_deref() == Some(tree.as_str())
        {
            return Ok((parent.clone(), false));
        }
        let label: String = label
            .chars()
            .map(|c| if c.is_control() { ' ' } else { c })
            .take(160)
            .collect();
        let message = format!("[cove] {label}");
        let mut args = vec!["commit-tree", tree.as_str(), "-m", message.as_str()];
        if let Some(parent) = &parent {
            args.extend(["-p", parent.as_str()]);
        }
        let hash = self.git(&args).await?;
        let mut update = vec!["update-ref", self.reference.as_str(), hash.as_str()];
        if let Some(parent) = &parent {
            update.push(parent.as_str());
        }
        self.git(&update).await?;
        Ok((hash, true))
    }

    /// Keeps the newest `KEEP` snapshots once there are `KEEP + SLACK`; the kept commits
    /// are re-created with their dates, and unreachable objects are collected in the
    /// background (only objects older than an hour, so concurrent writers are safe).
    async fn trim_locked(&self) {
        let Ok(list) = self.git(&["rev-list", &self.reference]).await else {
            return;
        };
        let hashes: Vec<&str> = list.lines().collect();
        if hashes.len() <= self.keep + SLACK {
            return;
        }
        let mut parent: Option<String> = None;
        for hash in hashes[..self.keep].iter().rev() {
            let Ok(meta) = self.git(&["log", "-1", "--format=%ct%x1f%B", hash]).await else {
                return;
            };
            let Some((time, message)) = meta.split_once('\u{1f}') else {
                return;
            };
            let Ok(tree) = self.tree_of(hash).await else {
                return;
            };
            let mut command = tokio::process::Command::new("git");
            isolate(&mut command, &self.git_dir)
                .args(CONFIG)
                .arg("commit-tree")
                .arg(&tree)
                .arg("-m")
                .arg(message.trim())
                .current_dir(&self.work)
                .env("GIT_DIR", &self.git_dir)
                .env("GIT_INDEX_FILE", &self.index)
                .env("GIT_AUTHOR_DATE", format!("@{time} +0000"))
                .env("GIT_COMMITTER_DATE", format!("@{time} +0000"))
                .stdin(Stdio::null())
                .kill_on_drop(true);
            if let Some(parent) = &parent {
                command.arg("-p").arg(parent);
            }
            let Ok(Ok(output)) = tokio::time::timeout(GIT_TIMEOUT, command.output()).await else {
                return;
            };
            if !output.status.success() {
                return;
            }
            parent = Some(String::from_utf8_lossy(&output.stdout).trim().to_owned());
        }
        if let Some(head) = parent
            && self
                .git(&["update-ref", &self.reference, &head, hashes[0]])
                .await
                .is_ok()
        {
            let git_dir = self.git_dir.clone();
            tokio::spawn(async move {
                let _ = tokio::time::timeout(
                    Duration::from_secs(300),
                    isolate(&mut tokio::process::Command::new("git"), &git_dir)
                        // One thread and no delta search on large blobs: never saturate
                        // the machine the user is working on.
                        .args([
                            "-c",
                            "gc.auto=0",
                            "-c",
                            "pack.threads=1",
                            "-c",
                            "core.bigFileThreshold=1m",
                            "gc",
                            "--quiet",
                            "--prune=1.hour.ago",
                        ])
                        .env("GIT_DIR", &git_dir)
                        .stdin(Stdio::null())
                        .stdout(Stdio::null())
                        .stderr(Stdio::null())
                        .kill_on_drop(true)
                        .status(),
                )
                .await;
            });
        }
    }

    /// Newest first.
    pub async fn list(&self, limit: usize) -> Result<Vec<SnapshotInfo>> {
        if self.head().await.is_none() {
            return Ok(Vec::new());
        }
        let text = self
            .git(&[
                "log",
                &format!("-{limit}"),
                "--format=%H%x1f%ct%x1f%s",
                &self.reference,
            ])
            .await?;
        Ok(text
            .lines()
            .filter_map(|line| {
                let mut parts = line.splitn(3, '\u{1f}');
                Some(SnapshotInfo {
                    hash: parts.next()?.to_owned(),
                    time: parts.next()?.parse().ok()?,
                    label: parts
                        .next()
                        .unwrap_or_default()
                        .trim_start_matches("[cove] ")
                        .to_owned(),
                })
            })
            .collect())
    }

    /// The first snapshot of the current task, when one was taken.
    pub fn task_first(&self) -> Option<String> {
        self.state
            .lock()
            .ok()
            .and_then(|state| state.task_first.clone())
    }

    pub fn reset_task(&self) {
        if let Ok(mut state) = self.state.lock() {
            state.task_first = None;
        }
    }

    /// `#n` (from `/undo list`, 1 = newest) or a hash prefix of this project's snapshots.
    pub async fn resolve(&self, reference: &str) -> Result<SnapshotInfo> {
        let list = self.list(self.keep + SLACK).await?;
        if let Some(number) = reference.strip_prefix('#') {
            let index: usize = number
                .parse()
                .map_err(|_| invalid("快照编号的格式应为 #3"))?;
            return list
                .get(index.wrapping_sub(1))
                .cloned()
                .ok_or_else(|| invalid("没有这个编号的快照；用 /undo list 查看"));
        }
        if reference.len() < 4 || !reference.bytes().all(|b| b.is_ascii_hexdigit()) {
            return Err(invalid("未知快照；请使用 /undo list 中的 #n 或哈希前缀"));
        }
        let lower = reference.to_ascii_lowercase();
        let matches: Vec<_> = list
            .into_iter()
            .filter(|info| info.hash.starts_with(&lower))
            .collect();
        match matches.as_slice() {
            [one] => Ok(one.clone()),
            [] => Err(invalid(
                "本项目的快照中找不到它（可能已被清理）；用 /undo list 查看现有快照",
            )),
            _ => Err(invalid("快照前缀有歧义；请多输入几位哈希")),
        }
    }

    /// What applying `target` (limited to `paths` when given) would change, and a token
    /// binding the preview to the current work tree.
    pub async fn plan(&self, target: &str, paths: &[String]) -> Result<(Vec<Planned>, String)> {
        let _guard = self.lock.lock().await;
        let tree = self.stage().await?;
        let mut args = vec![
            "diff",
            "--cached",
            "--name-status",
            "--no-renames",
            "-z",
            target,
            "--",
        ];
        if paths.is_empty() {
            args.push(".");
        } else {
            args.extend(paths.iter().map(String::as_str));
        }
        let raw = self.git_bytes(&args).await?;
        let raw = String::from_utf8_lossy(&raw);
        let mut fields = raw.split('\0').filter(|field| !field.is_empty());
        let mut planned = Vec::new();
        let excluded = self.excluded();
        while let (Some(status), Some(path)) = (fields.next(), fields.next()) {
            // Left out now (it may sit in an older snapshot): a restore never touches it.
            if excluded.covers(path) {
                continue;
            }
            planned.push(Planned {
                action: match status.chars().next() {
                    Some('A') => "remove",
                    Some('D') => "recreate",
                    _ => "restore",
                },
                path: path.to_owned(),
            });
        }
        let mut hasher = Sha256::new();
        for part in [target, tree.as_str()]
            .into_iter()
            .chain(paths.iter().map(String::as_str))
        {
            hasher.update(part.as_bytes());
            hasher.update([0]);
        }
        let token = format!("{:x}", hasher.finalize())[..8].to_owned();
        Ok((planned, token))
    }

    /// Restores `target` (the whole tree, or only `paths`) after snapshotting the
    /// current state, which is returned so the restore itself can be undone.
    pub async fn restore(&self, target: &SnapshotInfo, paths: &[String]) -> Result<String> {
        for path in paths {
            safe_relative(path)?;
        }
        let _guard = self.lock.lock().await;
        let (backup, _) = self
            .snapshot_locked(&format!("before restore to {}", target.short()))
            .await?;
        let hash = target.hash.as_str();
        let mut diff = vec![
            "diff",
            "--raw",
            "-z",
            "--no-renames",
            "--diff-filter=A",
            hash,
            backup.as_str(),
            "--",
        ];
        if paths.is_empty() {
            diff.push(".");
        } else {
            diff.extend(paths.iter().map(String::as_str));
        }
        let raw = self.git_bytes(&diff).await?;
        let raw = String::from_utf8_lossy(&raw).into_owned();
        let excluded = self.excluded();
        let mut created = Vec::new();
        let mut fields = raw.split('\0').filter(|field| !field.is_empty());
        while let (Some(meta), Some(path)) = (fields.next(), fields.next()) {
            let mode = meta
                .trim_start_matches(':')
                .split_whitespace()
                .nth(1)
                .unwrap_or_default();
            if !excluded.covers(path) {
                created.push((mode == "160000", path.to_owned()));
            }
        }
        let existing: Vec<String> = if paths.is_empty() {
            vec![".".into()]
        } else {
            let mut list = Vec::new();
            for path in paths.iter().filter(|path| !excluded.covers(path)) {
                if self
                    .git(&["cat-file", "-e", &format!("{hash}:{path}")])
                    .await
                    .is_ok()
                {
                    list.push(path.clone());
                }
            }
            list
        };
        let tree_files = self.git(&["ls-tree", "-r", "--name-only", hash]).await?;
        let mut errors = Vec::new();
        // Older snapshots may hold paths left out now: restore only the files still covered.
        let restored: Vec<&str> = if paths.is_empty() {
            tree_files
                .lines()
                .filter(|path| !path.is_empty() && !excluded.covers(path))
                .collect()
        } else {
            existing.iter().map(String::as_str).collect()
        };
        if !restored.is_empty() {
            let list = self.excludes.with_extension("restore");
            let mut names = restored.join("\0");
            names.push('\0');
            std::fs::write(&list, names)?;
            let file = format!("--pathspec-from-file={}", list.to_string_lossy());
            let result = self
                .git(&[
                    "--literal-pathspecs",
                    "checkout",
                    hash,
                    &file,
                    "--pathspec-file-nul",
                ])
                .await;
            let _ = std::fs::remove_file(&list);
            if let Err(err) = result {
                return Err(invalid(format!(
                    "{err}；恢复前的状态已保存为快照 {}",
                    &backup[..10]
                )));
            }
        }
        let mut nested = Vec::new();
        for (gitlink, path) in created {
            if gitlink {
                nested.push(path);
                continue;
            }
            let Ok(relative) = safe_relative(&path) else {
                continue;
            };
            let full = self.work.join(relative);
            match std::fs::remove_file(&full) {
                Ok(()) => prune_empty(&self.work, full.parent()),
                Err(err) if err.kind() == std::io::ErrorKind::NotFound => {}
                Err(err) => errors.push(format!("remove {path}: {err}")),
            }
        }
        if !nested.is_empty() {
            errors.push(format!(
                "嵌套的 Git 仓库不在快照范围内，保持原样：{}",
                nested.join(", ")
            ));
        }
        if errors.is_empty() {
            Ok(backup)
        } else {
            Err(invalid(format!(
                "{}；恢复前的状态已保存为快照 {}",
                errors.join("; "),
                &backup[..10]
            )))
        }
    }

    /// Patch from `target` to the current work tree (bounded).
    pub async fn diff(&self, target: &str) -> Result<String> {
        let _guard = self.lock.lock().await;
        self.stage().await?;
        let stat = self
            .git(&[
                "diff",
                "--cached",
                "--stat",
                "--no-renames",
                target,
                "--",
                ".",
            ])
            .await?;
        let patch = self
            .git(&[
                "diff",
                "--cached",
                "--no-ext-diff",
                "--no-textconv",
                "--no-color",
                "--no-renames",
                "--src-prefix=a/",
                "--dst-prefix=b/",
                target,
                "--",
                ".",
            ])
            .await?;
        let mut out = format!("{stat}\n{patch}\n");
        if out.len() > DIFF_LIMIT {
            out.truncate(out.floor_char_boundary(DIFF_LIMIT));
            out.push_str("\n[diff truncated]\n");
        }
        Ok(out)
    }
}

/// Strips the Windows verbatim prefix (`\\?\C:\...`), which Git does not accept.
fn plain(path: &Path) -> PathBuf {
    let text = path.to_string_lossy();
    match text.strip_prefix(r"\\?\") {
        Some(rest) if rest.as_bytes().get(1) == Some(&b':') => PathBuf::from(rest),
        _ => path.to_path_buf(),
    }
}

fn safe_relative(path: &str) -> Result<PathBuf> {
    let relative = Path::new(path);
    if path.is_empty()
        || relative.is_absolute()
        || relative
            .components()
            .any(|part| !matches!(part, Component::Normal(_)))
        || relative.components().any(|part| {
            part.as_os_str()
                .to_string_lossy()
                .eq_ignore_ascii_case(".git")
        })
    {
        return Err(invalid(format!(
            "无效路径 {path:?}；请使用相对于项目目录的路径"
        )));
    }
    Ok(relative.to_path_buf())
}

fn prune_empty(root: &Path, mut dir: Option<&Path>) {
    while let Some(current) = dir {
        if current == root || !current.starts_with(root) || std::fs::remove_dir(current).is_err() {
            return;
        }
        dir = current.parent();
    }
}

/// Shell commands that only read; anything else (or anything unclear) is mutating.
/// Same tokenizer and classifier as the permission check, so both agree.
pub fn read_only_shell(command: &str) -> bool {
    crate::safety::permission::classify::read_only(
        command,
        crate::safety::permission::shell::Dialect::for_command(command),
    )
}

/// Tools whose effects a snapshot must precede.
pub fn mutating(name: &str, input: &Value) -> bool {
    match name {
        "write" | "edit" | "execute_plan" => true,
        // Sub-agents default to read mode; write mode or a writing agent type mutates.
        "agent" => {
            input.get("mode").and_then(Value::as_str) == Some("write")
                || input
                    .get("type")
                    .and_then(Value::as_str)
                    .is_some_and(|kind| !matches!(kind, "read" | "explore" | "plan" | "review"))
        }
        "shell" => input
            .get("command")
            .and_then(Value::as_str)
            .is_none_or(|command| !read_only_shell(command)),
        _ => false,
    }
}

fn label(name: &str, input: &Value) -> String {
    let detail = ["path", "command", "task", "prompt"]
        .iter()
        .find_map(|key| input.get(*key).and_then(Value::as_str))
        .unwrap_or_default();
    let detail: String = detail.chars().take(80).collect();
    format!("before {name}: {detail}")
}

/// Takes a snapshot before every mutating tool call (unchanged trees add nothing).
pub struct Snapshotting {
    pub inner: Arc<dyn ToolExecutor>,
    pub snapshots: Arc<Snapshots>,
}

impl ToolExecutor for Snapshotting {
    fn definitions(&self) -> Vec<ToolDefinition> {
        self.inner.definitions()
    }

    fn metered(&self, name: &str) -> bool {
        self.inner.metered(name)
    }

    fn concurrent(&self, name: &str) -> bool {
        self.inner.concurrent(name)
    }

    fn execute(
        &self,
        name: String,
        input: Value,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ToolOutput>> {
        Box::pin(async move {
            let mut warning = None;
            if mutating(&name, &input) && !cancel.is_cancelled() {
                let label = label(&name, &input);
                let before = self.snapshots.before_call(label, SNAPSHOT_BUDGET);
                tokio::select! {
                    before = before => warning = match before {
                        Before::Taken | Before::Off => None,
                        Before::Busy => Some("[checkpoint] The first snapshot of this workspace is still being prepared in the background, so this call ran without one: /undo cannot restore its effects.".to_owned()),
                        Before::Failed(err) => Some(format!("[checkpoint] Git snapshot before this call failed ({err}); /undo cannot restore its effects.")),
                        Before::Slow { disabled: false } => Some(format!("[checkpoint] The workspace snapshot took longer than {}s, so this call ran without one: /undo cannot restore its effects.", SNAPSHOT_BUDGET.as_secs())),
                        Before::Slow { disabled: true } => Some(format!("[checkpoint] The workspace snapshot took longer than {}s again, so automatic snapshots are off for the rest of this session: /undo cannot restore the effects of this or later calls.", SNAPSHOT_BUDGET.as_secs())),
                    },
                    _ = cancel.cancelled() => return Err(Error::Cancelled),
                }
            }
            let mut output = self.inner.execute(name, input, cancel).await?;
            if let Some(warning) = warning {
                output.content.push('\n');
                output.content.push_str(&warning);
            }
            Ok(output)
        })
    }
}

/// Starts a new "first snapshot of the task" for `/diff` at each new task.
impl TurnHook for Snapshots {
    fn task_note<'a>(&'a self, _session: &'a Session) -> BoxFuture<'a, Option<String>> {
        self.reset_task();
        Box::pin(async { None })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn git_available() -> bool {
        std::process::Command::new("git")
            .arg("--version")
            .output()
            .is_ok_and(|output| output.status.success())
    }

    #[test]
    fn shell_classification_is_conservative() {
        assert!(read_only_shell("git status && ls -la"));
        assert!(read_only_shell("Get-Content a.txt | Select-String foo"));
        assert!(read_only_shell("rg needle src"));
        assert!(!read_only_shell("echo hi > file"));
        assert!(!read_only_shell("cargo build"));
        assert!(!read_only_shell("git commit -m x"));
        assert!(!read_only_shell("find . -delete"));
        assert!(!read_only_shell("ls; rm -rf x"));
        assert!(!read_only_shell("cat $(rm x)"));
        assert!(mutating("write", &serde_json::json!({})));
        assert!(!mutating("read", &serde_json::json!({})));
        assert!(!mutating(
            "shell",
            &serde_json::json!({"command":"git diff"})
        ));
        assert!(mutating(
            "shell",
            &serde_json::json!({"command":"python x.py"})
        ));
        assert!(!mutating("agent", &serde_json::json!({"mode":"read"})));
        assert!(!mutating("agent", &serde_json::json!({"prompt":"look"})));
        assert!(mutating("agent", &serde_json::json!({"type":"general"})));
        assert!(mutating("agent", &serde_json::json!({"mode":"write"})));
    }

    #[tokio::test]
    async fn snapshots_capture_shell_effects_and_restores_are_undoable() {
        if !git_available() {
            return;
        }
        let data = tempfile::tempdir().unwrap();
        let work = tempfile::tempdir().unwrap();
        std::fs::write(work.path().join("a.txt"), "one\r\n").unwrap();
        std::fs::create_dir_all(work.path().join("target")).unwrap();
        std::fs::write(work.path().join("target/big.bin"), "ignored").unwrap();
        let snapshots = Snapshots::open(data.path(), work.path()).await.unwrap();
        let (first, created) = snapshots.snapshot("first").await.unwrap();
        assert!(created);
        assert_eq!(snapshots.task_first().as_deref(), Some(first.as_str()));
        let (same, created) = snapshots.snapshot("again").await.unwrap();
        assert!(!created);
        assert_eq!(same, first);
        // A "shell" effect: modify, create and delete files outside the file tools.
        std::fs::write(work.path().join("a.txt"), "two\n").unwrap();
        std::fs::create_dir_all(work.path().join("new/dir")).unwrap();
        std::fs::write(work.path().join("new/dir/b.txt"), "b").unwrap();
        snapshots.snapshot("second").await.unwrap();
        let list = snapshots.list(10).await.unwrap();
        assert_eq!(list.len(), 2);
        assert_eq!(list[1].label, "first");
        let target = snapshots.resolve("#2").await.unwrap();
        assert_eq!(target.hash, first);
        let (planned, token) = snapshots.plan(&target.hash, &[]).await.unwrap();
        assert!(planned.contains(&Planned {
            action: "remove",
            path: "new/dir/b.txt".into()
        }));
        assert!(
            planned
                .iter()
                .any(|p| p.action == "restore" && p.path == "a.txt")
        );
        assert!(!planned.iter().any(|p| p.path.starts_with("target")));
        let backup = snapshots.restore(&target, &[]).await.unwrap();
        assert_eq!(
            std::fs::read_to_string(work.path().join("a.txt")).unwrap(),
            "one\r\n"
        );
        assert!(!work.path().join("new").exists());
        assert!(work.path().join("target/big.bin").exists());
        // The work tree changed, so the same preview token no longer matches.
        let (_, again) = snapshots.plan(&target.hash, &[]).await.unwrap();
        assert_ne!(again, token);
        // Undo the undo, then restore only one file.
        let backup = snapshots.resolve(&backup[..8]).await.unwrap();
        snapshots.restore(&backup, &[]).await.unwrap();
        assert_eq!(
            std::fs::read_to_string(work.path().join("new/dir/b.txt")).unwrap(),
            "b"
        );
        snapshots
            .restore(&target, &["a.txt".into(), "new/dir/b.txt".into()])
            .await
            .unwrap();
        assert_eq!(
            std::fs::read_to_string(work.path().join("a.txt")).unwrap(),
            "one\r\n"
        );
        assert!(!work.path().join("new/dir/b.txt").exists());
        assert!(snapshots.restore(&target, &["../x".into()]).await.is_err());
        let diff = snapshots.diff(&first).await.unwrap();
        assert!(diff.trim().is_empty() || !diff.contains("a.txt"), "{diff}");
    }

    #[tokio::test]
    async fn retention_trims_to_the_newest_snapshots() {
        if !git_available() {
            return;
        }
        let data = tempfile::tempdir().unwrap();
        let work = tempfile::tempdir().unwrap();
        let snapshots = Snapshots::open_keeping(data.path(), work.path(), 3)
            .await
            .unwrap();
        for index in 0..=(3 + SLACK) {
            std::fs::write(work.path().join("f"), index.to_string()).unwrap();
            snapshots.snapshot(&format!("n{index}")).await.unwrap();
        }
        let list = snapshots.list(200).await.unwrap();
        assert_eq!(list.len(), 3);
        assert_eq!(list[0].label, format!("n{}", 3 + SLACK));
        assert_eq!(list[2].label, format!("n{}", 1 + SLACK));
    }

    fn project_git(work: &Path, args: &[&str]) {
        let output = std::process::Command::new("git")
            .args(["-c", "user.name=t", "-c", "user.email=t@t"])
            .args(args)
            .current_dir(work)
            .env_remove("GIT_DIR")
            .env_remove("GIT_WORK_TREE")
            .env_remove("GIT_INDEX_FILE")
            .output()
            .unwrap();
        assert!(output.status.success(), "{output:?}");
    }

    async fn tree_paths(snapshots: &Snapshots, commit: &str) -> Vec<String> {
        let mut paths: Vec<String> = snapshots
            .git(&["ls-tree", "-r", "--name-only", commit])
            .await
            .unwrap()
            .lines()
            .map(str::to_owned)
            .collect();
        paths.sort();
        paths
    }

    #[tokio::test]
    async fn snapshots_follow_the_projects_own_ignores_and_leave_out_tool_state() {
        if !git_available() {
            return;
        }
        let data = tempfile::tempdir().unwrap();
        let work = tempfile::tempdir().unwrap();
        let root = work.path();
        project_git(root, &["init", "-q"]);
        for (path, text) in [
            ("a.txt", "a"),
            (".github/ci.yml", "ci"),
            (".gitignore", "/bin/\n"),
        ] {
            std::fs::create_dir_all(root.join(path).parent().unwrap()).unwrap();
            std::fs::write(root.join(path), text).unwrap();
        }
        project_git(root, &["add", "."]);
        project_git(root, &["commit", "-qm", "init"]);
        // Ignored only through the project's `.git/info/exclude`.
        std::fs::write(root.join(".git/info/exclude"), "/notes/\n").unwrap();
        for (path, text) in [
            ("notes/n.md", "private"),
            ("bin/out.dll", "build"),
            (".codeatlas/index.db", "index"),
            ("src/.cache/c", "cache"),
            (".env", "SECRET=1"),
            ("new.txt", "new work"),
        ] {
            std::fs::create_dir_all(root.join(path).parent().unwrap()).unwrap();
            std::fs::write(root.join(path), text).unwrap();
        }
        std::fs::write(root.join("dump.bin"), vec![0u8; LARGE_FILE as usize + 1]).unwrap();
        let snapshots = Snapshots::open(data.path(), root).await.unwrap();
        let (first, _) = snapshots.snapshot("first").await.unwrap();
        assert_eq!(
            tree_paths(&snapshots, &first).await,
            [".github/ci.yml", ".gitignore", "a.txt", "new.txt"]
        );
    }

    #[tokio::test]
    async fn stale_entries_leave_the_index_and_restores_never_touch_left_out_paths() {
        if !git_available() {
            return;
        }
        let data = tempfile::tempdir().unwrap();
        let work = tempfile::tempdir().unwrap();
        let root = work.path();
        project_git(root, &["init", "-q"]);
        std::fs::write(root.join("a.txt"), "one").unwrap();
        project_git(root, &["add", "."]);
        project_git(root, &["commit", "-qm", "init"]);
        std::fs::create_dir_all(root.join(".codeatlas")).unwrap();
        std::fs::write(root.join(".codeatlas/index.db"), "old index").unwrap();
        let snapshots = Snapshots::open(data.path(), root).await.unwrap();
        // An older release staged tool state: put it in the index and in a snapshot.
        snapshots
            .git(&["add", "-f", ".codeatlas/index.db", "a.txt"])
            .await
            .unwrap();
        let tree = snapshots.git(&["write-tree"]).await.unwrap();
        let (old, _) = snapshots.commit_staged("old release", tree).await.unwrap();
        assert!(
            tree_paths(&snapshots, &old)
                .await
                .contains(&".codeatlas/index.db".into())
        );
        std::fs::write(root.join(".codeatlas/index.db"), "live index").unwrap();
        std::fs::write(root.join("a.txt"), "two").unwrap();
        let (now, _) = snapshots.snapshot("now").await.unwrap();
        assert_eq!(tree_paths(&snapshots, &now).await, ["a.txt"]);
        let (planned, _) = snapshots.plan(&old, &[]).await.unwrap();
        assert_eq!(
            planned,
            [Planned {
                action: "restore",
                path: "a.txt".into()
            }]
        );
        let target = snapshots.resolve(&old[..10]).await.unwrap();
        snapshots.restore(&target, &[]).await.unwrap();
        assert_eq!(std::fs::read_to_string(root.join("a.txt")).unwrap(), "one");
        assert_eq!(
            std::fs::read_to_string(root.join(".codeatlas/index.db")).unwrap(),
            "live index"
        );
    }

    #[tokio::test]
    async fn outside_a_git_project_hidden_paths_are_left_out() {
        if !git_available() {
            return;
        }
        let data = tempfile::tempdir().unwrap();
        let work = tempfile::tempdir().unwrap();
        for path in ["doc.md", ".cache/x", "sub/.state/y", "sub/z"] {
            std::fs::create_dir_all(work.path().join(path).parent().unwrap()).unwrap();
            std::fs::write(work.path().join(path), path).unwrap();
        }
        let snapshots = Snapshots::open(data.path(), work.path()).await.unwrap();
        let (first, _) = snapshots.snapshot("first").await.unwrap();
        assert_eq!(tree_paths(&snapshots, &first).await, ["doc.md", "sub/z"]);
    }

    #[tokio::test]
    async fn slow_snapshots_are_skipped_without_recording_and_then_turned_off() {
        if !git_available() {
            return;
        }
        let data = tempfile::tempdir().unwrap();
        let work = tempfile::tempdir().unwrap();
        std::fs::write(work.path().join("a.txt"), "a").unwrap();
        let snapshots = Snapshots::open(data.path(), work.path()).await.unwrap();
        assert_eq!(
            snapshots
                .before_call("fast".into(), Duration::from_secs(60))
                .await,
            Before::Taken
        );
        std::fs::write(work.path().join("a.txt"), "b").unwrap();
        assert_eq!(
            snapshots.before_call("slow".into(), Duration::ZERO).await,
            Before::Slow { disabled: false }
        );
        // While that one is still staging, calls skip without counting as slow.
        snapshots.warming.store(true, Ordering::SeqCst);
        assert_eq!(
            snapshots
                .before_call("busy".into(), Duration::from_secs(60))
                .await,
            Before::Busy
        );
        snapshots.warming.store(false, Ordering::SeqCst);
        assert_eq!(
            snapshots.before_call("slow".into(), Duration::ZERO).await,
            Before::Slow { disabled: true }
        );
        assert_eq!(
            snapshots
                .before_call("off".into(), Duration::from_secs(60))
                .await,
            Before::Off
        );
        // The abandoned runs still finish staging, but record nothing.
        let _guard = snapshots.lock.lock().await;
        drop(_guard);
        tokio::time::sleep(Duration::from_millis(200)).await;
        let _guard = snapshots.lock.lock().await;
        drop(_guard);
        let labels: Vec<String> = snapshots
            .list(10)
            .await
            .unwrap()
            .into_iter()
            .map(|info| info.label)
            .collect();
        assert_eq!(labels, ["fast"]);
    }

    #[tokio::test]
    async fn a_lock_left_by_an_earlier_process_does_not_block_snapshots_forever() {
        if !git_available() {
            return;
        }
        let data = tempfile::tempdir().unwrap();
        let work = tempfile::tempdir().unwrap();
        std::fs::write(work.path().join("a.txt"), "a").unwrap();
        let snapshots = Snapshots::open(data.path(), work.path()).await.unwrap();
        let mut lock = snapshots.index.clone().into_os_string();
        lock.push(".lock");
        let lock = PathBuf::from(lock);
        std::fs::write(&lock, "").unwrap();
        // A fresh lock may belong to a live Git: left alone.
        assert!(snapshots.snapshot("fresh lock").await.is_err());
        let old = std::time::SystemTime::now() - GIT_TIMEOUT * 3;
        std::fs::File::options()
            .write(true)
            .open(&lock)
            .unwrap()
            .set_modified(old)
            .unwrap();
        assert!(snapshots.snapshot("stale lock").await.unwrap().1);
        assert!(!lock.exists());
    }

    #[tokio::test]
    async fn bulk_untracked_directories_coveignore_and_packed_contents() {
        if !git_available() {
            return;
        }
        let data = tempfile::tempdir().unwrap();
        let work = tempfile::tempdir().unwrap();
        let root = work.path();
        project_git(root, &["init", "-q"]);
        std::fs::create_dir_all(root.join("src")).unwrap();
        for index in 0..40 {
            std::fs::write(
                root.join(format!("src/f{index}.rs")),
                format!("fn f{index}() {{}}\n"),
            )
            .unwrap();
        }
        project_git(root, &["add", "."]);
        project_git(root, &["commit", "-qm", "init"]);
        // An unpacked SDK, a small new directory, and project-specific rules.
        std::fs::create_dir_all(root.join("sdk/lib")).unwrap();
        for index in 0..=BULK_FILES {
            std::fs::write(root.join(format!("sdk/lib/{index}.h")), "x").unwrap();
        }
        std::fs::create_dir_all(root.join("new")).unwrap();
        std::fs::write(root.join("new/a.rs"), "a").unwrap();
        std::fs::write(root.join("run.log"), "log").unwrap();
        std::fs::create_dir_all(root.join("data")).unwrap();
        std::fs::write(root.join("data/rows.csv"), "1,2").unwrap();
        std::fs::write(root.join(COVEIGNORE), "*.log\n/data/\n").unwrap();
        let snapshots = Snapshots::open(data.path(), root).await.unwrap();
        let (first, _) = snapshots.snapshot("first").await.unwrap();
        let paths = tree_paths(&snapshots, &first).await;
        assert!(paths.contains(&"new/a.rs".to_owned()) && paths.contains(&"src/f7.rs".to_owned()));
        // Untracked and hidden itself: tool configuration, like the rest (rule 2).
        assert!(!paths.contains(&COVEIGNORE.to_owned()));
        assert!(
            !paths
                .iter()
                .any(|path| path.starts_with("sdk/") || path.starts_with("data/"))
        );
        assert!(!paths.contains(&"run.log".to_owned()));
        // File contents went into a pack, not one loose file each (only trees and the
        // commit are loose); the project's own objects are never borrowed.
        let loose = walkdir_count(&snapshots.git_dir.join("objects"));
        assert!(loose < 20, "{loose} loose objects");
        assert!(!snapshots.git_dir.join("objects/info/alternates").exists());
    }

    fn walkdir_count(objects: &Path) -> usize {
        std::fs::read_dir(objects)
            .unwrap()
            .flatten()
            .filter(|entry| entry.file_name().len() == 2)
            .map(|entry| std::fs::read_dir(entry.path()).unwrap().count())
            .sum()
    }

    #[tokio::test]
    async fn outside_git_large_files_are_left_out_too() {
        if !git_available() {
            return;
        }
        let data = tempfile::tempdir().unwrap();
        let work = tempfile::tempdir().unwrap();
        std::fs::write(work.path().join("notes.md"), "n").unwrap();
        std::fs::write(
            work.path().join("model.bin"),
            vec![0u8; LARGE_FILE as usize + 1],
        )
        .unwrap();
        let snapshots = Snapshots::open(data.path(), work.path()).await.unwrap();
        let (first, _) = snapshots.snapshot("first").await.unwrap();
        assert_eq!(tree_paths(&snapshots, &first).await, ["notes.md"]);
    }

    #[tokio::test]
    async fn snapshot_git_reads_no_user_or_system_config() {
        if !git_available() {
            return;
        }
        let data = tempfile::tempdir().unwrap();
        let work = tempfile::tempdir().unwrap();
        let snapshots = Snapshots::open(data.path(), work.path()).await.unwrap();
        let origins = snapshots
            .git(&["config", "--list", "--show-origin"])
            .await
            .unwrap();
        // Git quotes the path and escapes its backslashes.
        let plain = |text: &str| text.replace("\\\\", "/").replace('\\', "/");
        let own = plain(&snapshots.git_dir.to_string_lossy());
        for line in origins.lines() {
            let origin = line.split('\t').next().unwrap_or_default();
            assert!(
                origin == "command line:" || plain(origin).contains(&own),
                "{line}"
            );
        }
        assert!(origins.contains("core.autocrlf=false"));
    }
}
