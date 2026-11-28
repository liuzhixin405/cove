//! The shared check slot of overlay plans: one persistent
//! copy of the project's source files under `<data dir>/check-slots/<project hash>/`, where
//! `overlay_check` (a compile check such as `cargo check`) runs on the main workspace plus
//! an overlay item's layers. Only one check runs at a time, in this process and across
//! cove processes; each run syncs only the files that changed since the last one, and the
//! build output the command leaves in the slot stays there, so later checks are warm.

use std::{
    collections::{BTreeMap, BTreeSet},
    path::{Component, Path, PathBuf},
    sync::Arc,
    time::{SystemTime, UNIX_EPOCH},
};

use serde::{Deserialize, Serialize};
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Progress, Result, ToolDefinition},
    persist,
    safety::permission::{Authorizer, Effect},
    session::overlay::{Content, OverlayStack, content_hash},
    tools::ToolOutput,
};

const MANIFEST: &str = "manifest.json";
const TREE: &str = "tree";
const LOCK: &str = "slot.lock";
const VERSION: u32 = 1;
/// Output of a check kept in its result.
const OUTPUT_LIMIT: usize = 12 * 1024;

/// One synced file: the hash of what the slot holds, and the size and modification time
/// of the workspace file it was copied from (`None`: it came from a layer).
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
struct SlotFile {
    hash: String,
    #[serde(default)]
    source: Option<(u64, u128)>,
}

#[derive(Default, Serialize, Deserialize)]
struct Manifest {
    version: u32,
    files: BTreeMap<String, SlotFile>,
}

/// What one sync changed in the slot.
#[derive(Debug, Default, PartialEq, Eq)]
pub struct Synced {
    pub written: usize,
    pub removed: usize,
    pub files: usize,
}

pub struct CheckSlot {
    root: PathBuf,
    dir: PathBuf,
    command: String,
    queue: tokio::sync::Mutex<()>,
}

/// The tool definition offered to overlay items while a check slot is configured.
pub fn definition(command: &str) -> ToolDefinition {
    ToolDefinition {
        name: "check".into(),
        description: format!(
            "Run the project's compile check (`{command}`) on the main workspace plus your overlay changes and those of the items you depend on, \
             in a shared check slot (one check at a time; a call waits for its turn). It only compiles: tests run once after all items are merged. \
             Returns the exit code and the output; fix compile errors in your own writes and check again."
        ),
        schema: serde_json::json!({"type":"object","properties":{},"additionalProperties":false}),
    }
}

/// Size and modification time (nanoseconds since the epoch) of a file.
fn stamp(path: &Path) -> Option<(u64, u128)> {
    let meta = std::fs::metadata(path).ok()?;
    let modified = meta.modified().ok()?;
    let nanos = modified
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    Some((meta.len(), nanos))
}

/// A listed path as a plain relative path: no `..`, root, prefix or `.git` part.
fn plain_relative(path: &str) -> Option<PathBuf> {
    let candidate = Path::new(path);
    let mut out = PathBuf::new();
    for part in candidate.components() {
        match part {
            Component::Normal(name) if !name.eq_ignore_ascii_case(".git") => out.push(name),
            Component::CurDir => {}
            _ => return None,
        }
    }
    (!out.as_os_str().is_empty()).then_some(out)
}

impl CheckSlot {
    /// The slot of the workspace `root` (canonical) with `command`.
    pub fn new(data_dir: &Path, root: &Path, command: String) -> Self {
        Self {
            root: root.to_path_buf(),
            dir: data_dir
                .join("check-slots")
                .join(crate::agents::lifecycle::project_key(root)),
            command,
            queue: tokio::sync::Mutex::new(()),
        }
    }

    pub fn command(&self) -> &str {
        &self.command
    }

    /// The directory the command runs in.
    pub fn tree(&self) -> PathBuf {
        self.dir.join(TREE)
    }

    /// The source files of the workspace: what git tracks plus untracked files it does
    /// not ignore (`git ls-files --cached --others --exclude-standard`); outside a git
    /// repository, the files the ignore rules let through.
    async fn sources(&self, cancel: &CancellationToken) -> Result<Vec<String>> {
        let listed = crate::process::git::Git::new(&self.root)
            .args([
                "ls-files",
                "-z",
                "--cached",
                "--others",
                "--exclude-standard",
            ])
            .limit(64 * 1024 * 1024)
            .output(cancel)
            .await;
        match listed {
            Ok(output) if output.ok => {
                let mut seen = BTreeSet::new();
                Ok(output
                    .stdout
                    .split(|byte| *byte == 0)
                    .filter(|entry| !entry.is_empty())
                    .map(|entry| String::from_utf8_lossy(entry).into_owned())
                    .filter(|entry| seen.insert(entry.clone()))
                    .collect())
            }
            Err(Error::Cancelled) => Err(Error::Cancelled),
            _ => {
                let root = self.root.clone();
                Ok(tokio::task::spawn_blocking(move || {
                    crate::agents::writes::workspace_files(&root)
                })
                .await
                .unwrap_or_default())
            }
        }
    }

    /// Makes the slot's tree hold `stack`'s view of `sources` plus the files its layers
    /// created: changed files are rewritten, files no longer there are removed.
    pub fn sync(&self, stack: &OverlayStack, sources: &[String]) -> Result<Synced> {
        sync_into(&self.root, &self.dir, stack, sources)
    }

    /// Waits for the slot, syncs `stack` into it and runs the command there.
    pub async fn check(
        &self,
        stack: &Arc<OverlayStack>,
        authorizer: &Arc<dyn Authorizer>,
        progress: Option<Arc<Progress>>,
        cancel: CancellationToken,
    ) -> Result<ToolOutput> {
        authorizer
            .authorize(
                Effect::Shell(format!("overlay check: {}", self.command)),
                cancel.clone(),
            )
            .await?;
        let waited = SystemTime::now();
        let _turn = tokio::select! {
            _ = cancel.cancelled() => return Err(Error::Cancelled),
            turn = self.queue.lock() => turn,
        };
        // Another cove process may be checking the same project.
        let lock_path = self.dir.join(LOCK);
        let _lock = tokio::select! {
            _ = cancel.cancelled() => return Err(Error::Cancelled),
            lock = persist::try_lock_async(&lock_path, crate::process::WATCHED_TOTAL, |err| {
                Error::Tool(format!("the check slot stayed busy: {err}"))
            }) => lock?,
        };
        let queued = waited.elapsed().unwrap_or_default();
        let sources = self.sources(&cancel).await?;
        let synced = {
            let (root, dir, stack) = (self.root.clone(), self.dir.clone(), stack.clone());
            tokio::task::spawn_blocking(move || sync_into(&root, &dir, &stack, &sources))
                .await
                .map_err(|err| Error::Tool(format!("check slot sync panicked: {err}")))??
        };
        if let Some(progress) = &progress {
            progress
                .line(format!("[check] $ {}", self.command), false)
                .await;
        }
        let output = crate::process::shell_live(
            &self.tree(),
            &self.command,
            crate::process::Limit::watched(
                crate::process::WATCHED_TOTAL,
                crate::process::IDLE_LIMIT,
            ),
            cancel,
            progress,
        )
        .await?;
        let status = if let Some(reason) = &output.stalled {
            format!("stopped, no sign of work: {reason}")
        } else if output.timed_out {
            "timed out".to_owned()
        } else {
            match output.code {
                Some(0) => "exit 0 (passed)".to_owned(),
                Some(code) => {
                    format!("exit {code} (failed: fix the errors in your writes and check again)")
                }
                None => "ended without an exit code".to_owned(),
            }
        };
        let mut text = format!(
            "check `{}` in the shared check slot (main workspace + the layers this item sees; {} of {} files updated, {} removed",
            self.command, synced.written, synced.files, synced.removed
        );
        if queued.as_secs() >= 1 {
            text.push_str(&format!("; waited {} s for the slot", queued.as_secs()));
        }
        text.push_str(&format!("): {status}"));
        let body = format!("{}\n{}", output.stdout.trim_end(), output.stderr.trim_end());
        let body = body.trim();
        if !body.is_empty() {
            text.push('\n');
            text.push_str(&crate::text::visible(crate::text::tail(body, OUTPUT_LIMIT)));
        }
        Ok(ToolOutput {
            content: text,
            ..Default::default()
        })
    }
}

/// [`CheckSlot::sync`] of the workspace `root` into the slot directory `dir`.
fn sync_into(root: &Path, dir: &Path, stack: &OverlayStack, sources: &[String]) -> Result<Synced> {
    let tree = dir.join(TREE);

    std::fs::create_dir_all(&tree).map_err(|err| persist::io_context("create", &tree, err))?;
    let manifest_path = dir.join(MANIFEST);
    let mut manifest: Manifest = if manifest_path.is_file() {
        persist::read_json(&manifest_path).unwrap_or_default()
    } else {
        Manifest::default()
    };
    if manifest.version != VERSION {
        manifest = Manifest {
            version: VERSION,
            files: BTreeMap::new(),
        };
    }
    let mut wanted: BTreeMap<String, PathBuf> = BTreeMap::new();
    for source in sources {
        if let Some(relative) = plain_relative(source) {
            wanted.insert(relative.to_string_lossy().replace('\\', "/"), relative);
        }
    }
    for (path, _) in stack.files() {
        wanted.insert(path.to_string_lossy().replace('\\', "/"), path);
    }
    let mut synced = Synced::default();
    let mut next = BTreeMap::new();
    for (name, relative) in wanted {
        let target = tree.join(&relative);
        let layered = stack.lookup(&relative);
        if layered == Some(Content::Deleted) {
            continue;
        }
        let source = match layered {
            Some(_) => None,
            None => stamp(&root.join(&relative)),
        };
        if let (Some(known), Some(stamp)) = (manifest.files.get(&name), source)
            && known.source == Some(stamp)
            && target.is_file()
        {
            next.insert(name, known.clone());
            continue;
        }
        let Ok(bytes) = stack.read(&relative) else {
            // Listed but gone (deleted and not yet staged), or not a regular file.
            continue;
        };
        let hash = content_hash(&bytes);
        let current = manifest.files.get(&name).map(|known| known.hash.as_str());
        if current != Some(hash.as_str()) || !target.is_file() {
            if let Some(parent) = target.parent() {
                std::fs::create_dir_all(parent)
                    .map_err(|err| persist::io_context("create", parent, err))?;
            }
            persist::write_atomic(&target, &bytes, false)?;
            synced.written += 1;
        }
        next.insert(name, SlotFile { hash, source });
    }
    for name in manifest.files.keys() {
        if next.contains_key(name) {
            continue;
        }
        if let Some(relative) = plain_relative(name)
            && std::fs::remove_file(tree.join(relative)).is_ok()
        {
            synced.removed += 1;
        }
    }
    synced.files = next.len();
    persist::atomic_json(
        &manifest_path,
        &Manifest {
            version: VERSION,
            files: next,
        },
    )?;
    Ok(synced)
}

/// The `check` tool of one overlay item: its stack in the hub's slot.
pub struct CheckRun {
    pub slot: Arc<CheckSlot>,
    pub stack: Arc<OverlayStack>,
    pub authorizer: Arc<dyn Authorizer>,
    pub progress: Arc<Progress>,
}

impl CheckRun {
    pub async fn execute(&self, cancel: CancellationToken) -> Result<ToolOutput> {
        match self
            .slot
            .check(
                &self.stack,
                &self.authorizer,
                Some(self.progress.clone()),
                cancel,
            )
            .await
        {
            Ok(output) => Ok(output),
            Err(err @ (Error::Approval(_) | Error::Cancelled | Error::Uncertain(_))) => Err(err),
            Err(err) => Ok(ToolOutput {
                content: format!("check could not run: {err}"),
                failed: true,
                ..Default::default()
            }),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::session::overlay::Overlay;

    #[test]
    fn a_sync_copies_sources_and_layers_then_only_what_changed() {
        let work = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        let root = work.path().canonicalize().unwrap();
        std::fs::create_dir(root.join("src")).unwrap();
        std::fs::write(root.join("src/a.rs"), "a").unwrap();
        std::fs::write(root.join("src/b.rs"), "b").unwrap();
        std::fs::write(root.join("gone.rs"), "g").unwrap();
        let layer = Arc::new(Overlay::open(data.path().join("layer")).unwrap());
        let stack = OverlayStack::new(&root, vec![layer]).unwrap();
        let slot = CheckSlot::new(data.path(), &root, "cargo check".into());
        let sources = vec![
            "src/a.rs".to_owned(),
            "src/b.rs".to_owned(),
            "gone.rs".to_owned(),
            "../x".to_owned(),
        ];
        let first = slot.sync(&stack, &sources).unwrap();
        assert_eq!((first.written, first.files), (3, 3));
        assert_eq!(std::fs::read(slot.tree().join("src/a.rs")).unwrap(), b"a");
        // Build output in the slot stays; nothing unchanged is rewritten.
        std::fs::create_dir(slot.tree().join("target")).unwrap();
        std::fs::write(slot.tree().join("target/cache"), "warm").unwrap();
        let again = slot.sync(&stack, &sources).unwrap();
        assert_eq!((again.written, again.removed), (0, 0));
        // The layer's edit, its new file and its deletion reach the slot; the disk does not change.
        stack
            .write(Path::new("src/a.rs"), Some(b"a"), b"A", None)
            .unwrap();
        stack
            .write(Path::new("src/new.rs"), None, b"n", None)
            .unwrap();
        stack.delete(Path::new("src/b.rs"), b"b").unwrap();
        let layered = slot.sync(&stack, &sources).unwrap();
        assert_eq!((layered.written, layered.removed), (2, 1));
        assert_eq!(std::fs::read(slot.tree().join("src/a.rs")).unwrap(), b"A");
        assert_eq!(std::fs::read(slot.tree().join("src/new.rs")).unwrap(), b"n");
        assert!(!slot.tree().join("src/b.rs").exists());
        assert_eq!(std::fs::read(root.join("src/a.rs")).unwrap(), b"a");
        // A file that left the sources is removed; the build output is kept.
        let fewer = vec!["src/a.rs".to_owned(), "src/b.rs".to_owned()];
        let pruned = slot.sync(&stack, &fewer).unwrap();
        assert_eq!(pruned.removed, 1);
        assert!(!slot.tree().join("gone.rs").exists());
        assert_eq!(
            std::fs::read(slot.tree().join("target/cache")).unwrap(),
            b"warm"
        );
    }
}
