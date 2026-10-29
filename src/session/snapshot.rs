//! Whole-tree Git snapshots that also cover Shell effects. One private bare repository
//! under `<data_dir>/checkpoints/git` holds every project's snapshots; each project has
//! its own ref (`refs/cove/<hash>`) and its own index file, so neither the project's
//! repository nor other projects are ever touched. Snapshots are built with
//! `add`/`write-tree`/`commit-tree` through the `git` CLI (no libgit2/gix dependency,
//! and the user's installed Git understands the project's `.gitignore` exactly).

use std::{
    path::{Component, Path, PathBuf},
    process::Stdio,
    sync::Arc,
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

/// Byte-exact storage regardless of the project's attributes.
const ATTRIBUTES: &str = "* -text -filter !diff -merge -ident -working-tree-encoding\n";

/// Pinned so user config never reshapes stored bytes or parsed output.
const CONFIG: &[&str] = &[
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
}

pub struct Snapshots {
    git_dir: PathBuf,
    work: PathBuf,
    reference: String,
    index: PathBuf,
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
            reference: format!("refs/cove/{key}"),
            git_dir,
            work,
            keep,
            lock: tokio::sync::Mutex::new(()),
            state: std::sync::Mutex::new(State::default()),
        };
        if !snapshots.git_dir.join("HEAD").is_file() {
            std::fs::create_dir_all(&snapshots.git_dir)?;
            let output = tokio::process::Command::new("git")
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
        command
            .args(CONFIG)
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
        let mut args: Vec<String> = vec!["add".into(), "--all".into()];
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
                    args = vec!["add".into(), "--all".into(), "--".into(), ".".into()];
                    args.extend(excluded.iter().cloned());
                }
            }
        }
        self.git(&["write-tree"]).await
    }

    /// Records the work tree; returns the snapshot and whether a new one was needed.
    pub async fn snapshot(&self, label: &str) -> Result<(String, bool)> {
        let _guard = self.lock.lock().await;
        let (hash, created) = self.snapshot_locked(label).await?;
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
            command
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
                    tokio::process::Command::new("git")
                        .args(["-c", "gc.auto=0", "gc", "--quiet", "--prune=1.hour.ago"])
                        .env("GIT_DIR", git_dir)
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
        while let (Some(status), Some(path)) = (fields.next(), fields.next()) {
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
        let mut created = Vec::new();
        let mut fields = raw.split('\0').filter(|field| !field.is_empty());
        while let (Some(meta), Some(path)) = (fields.next(), fields.next()) {
            let mode = meta
                .trim_start_matches(':')
                .split_whitespace()
                .nth(1)
                .unwrap_or_default();
            created.push((mode == "160000", path.to_owned()));
        }
        let existing: Vec<String> = if paths.is_empty() {
            vec![".".into()]
        } else {
            let mut list = Vec::new();
            for path in paths {
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
        if !tree_files.is_empty() && !existing.is_empty() {
            let mut checkout = vec!["checkout", hash, "--"];
            checkout.extend(existing.iter().map(String::as_str));
            if let Err(err) = self.git(&checkout).await {
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
        crate::safety::permission::shell::Dialect::host(),
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
                let snapshot = self.snapshots.snapshot(&label);
                tokio::select! {
                    result = snapshot => if let Err(err) = result {
                        warning = Some(format!("[checkpoint] Git snapshot before this call failed ({err}); /undo cannot restore its effects."));
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
}
