//! Overlay workspaces (docs/optimizations/0053): a sub-task's file changes kept in a
//! private layer instead of the main workspace, so several sub-tasks can change the same
//! files in parallel and be merged afterwards.
//!
//! An [`Overlay`] holds one sub-task's layer: workspace-relative path -> new content or a
//! deletion mark, the content hash each path had before the layer first wrote it, and the
//! ordered edit intents the merge replays. It lives in a directory of its own (callers use
//! `<data dir>/overlays/<task>/<sub-task>/`) and is restored from there by [`Overlay::open`].
//!
//! An [`OverlayStack`] reads the main workspace (never written through the stack) under
//! ordered layers: lower layers are read-only (sub-tasks this one depends on), the top
//! layer takes every write. File tools use it through `tools::view::FileView`.

use std::{
    collections::{BTreeMap, HashSet},
    path::{Component, Path, PathBuf},
    sync::{Arc, Mutex, MutexGuard},
};

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::{
    domain::{Error, Result},
    persist,
};

const MANIFEST: &str = "overlay.json";
/// Append-only log of the intents, one JSON line each (version 2).
const INTENTS: &str = "intents.jsonl";
const BLOBS: &str = "blobs";
/// 1: the intents inside the manifest (still read); 2: in [`INTENTS`].
const VERSION: u32 = 2;

/// Content hash used by overlays and the merge: SHA-256 of the raw bytes, lowercase hex.
/// (Checkpoints keep whole contents and snapshots use git's blob ids, which need git to
/// compute; SHA-256 is what the rest of cove hashes content with.)
pub fn content_hash(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

/// Comparison key of a workspace-relative path: `/` separators, case-folded on Windows.
pub fn key(relative: &Path) -> String {
    crate::paths::key(relative)
}

/// `relative` rebuilt from its components: only plain names, no `.git`, no Windows
/// alternate data streams. The empty path is the workspace root.
fn normalize(relative: &Path) -> Result<PathBuf> {
    let mut out = PathBuf::new();
    for part in relative.components() {
        match part {
            Component::Normal(name) => {
                let text = name.to_string_lossy();
                if text.eq_ignore_ascii_case(".git") || (cfg!(windows) && text.contains(':')) {
                    return Err(Error::Workspace(format!(
                        "invalid overlay path {}",
                        relative.to_string_lossy()
                    )));
                }
                out.push(name);
            }
            Component::CurDir => {}
            _ => {
                return Err(Error::Workspace(format!(
                    "overlay paths are workspace-relative: {}",
                    relative.to_string_lossy()
                )));
            }
        }
    }
    Ok(out)
}

fn slashed(relative: &Path) -> String {
    relative.to_string_lossy().replace('\\', "/")
}

/// What a layer holds for one path.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Content {
    File(Arc<Vec<u8>>),
    Deleted,
}

/// One change a sub-task made, in order. `base_hash` is the [`content_hash`] of what the
/// sub-task saw just before this change (from a lower layer or the main workspace);
/// `Write` has `None` when the file did not exist. `Edit` keeps `old`/`new` exactly as the
/// model sent them; [`crate::tools::replay_edit`] applies them the way `edit` does.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Intent {
    Edit {
        path: PathBuf,
        base_hash: String,
        old: String,
        new: String,
        replace_all: bool,
    },
    Write {
        path: PathBuf,
        base_hash: Option<String>,
        content: Vec<u8>,
    },
    Delete {
        path: PathBuf,
        base_hash: String,
    },
}

impl Intent {
    pub fn path(&self) -> &Path {
        match self {
            Self::Edit { path, .. } | Self::Write { path, .. } | Self::Delete { path, .. } => path,
        }
    }
}

/// The text replacement of an `edit` call, recorded as its intent.
#[derive(Clone, Debug)]
pub struct EditSpec {
    pub old: String,
    pub new: String,
    pub replace_all: bool,
}

/// One path a layer changed: its content now, and the hash it had before the layer first
/// wrote it (`None`: it did not exist).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Change {
    pub path: PathBuf,
    pub content: Content,
    pub base_hash: Option<String>,
}

#[derive(Default)]
struct State {
    entries: BTreeMap<String, Change>,
    intents: Vec<Intent>,
    /// The manifest's form of `entries` (blob hashes computed once), so a write stores
    /// only its own content instead of hashing the whole layer again.
    stored_entries: BTreeMap<String, StoredEntry>,
    /// Bytes of [`INTENTS`] the manifest vouches for; anything after them (a write the
    /// manifest never confirmed) is cut off before the next append.
    log_bytes: u64,
    /// Intents of a version 1 manifest, moved into the log with the next write.
    unlogged: Vec<StoredIntent>,
}

#[derive(Serialize, Deserialize)]
struct Manifest {
    version: u32,
    entries: Vec<StoredEntry>,
    /// Version 1 only.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    intents: Vec<StoredIntent>,
    /// Version 2: valid length of the intent log.
    #[serde(default)]
    intents_bytes: u64,
}

/// [`Manifest`] borrowed from a [`State`], for writing.
#[derive(Serialize)]
struct ManifestRef<'a> {
    version: u32,
    entries: Vec<&'a StoredEntry>,
    intents_bytes: u64,
}

#[derive(Clone, Serialize, Deserialize)]
struct StoredEntry {
    path: String,
    /// Blob hash of the content; `None` marks a deletion.
    content: Option<String>,
    base_hash: Option<String>,
}

#[derive(Clone, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
enum StoredIntent {
    Edit {
        path: String,
        base_hash: String,
        old: String,
        new: String,
        replace_all: bool,
    },
    Write {
        path: String,
        base_hash: Option<String>,
        /// Blob hash of the content.
        content: String,
    },
    Delete {
        path: String,
        base_hash: String,
    },
}

/// One sub-task's private layer, persisted in its directory: `overlay.json` (entries and
/// intents) and `blobs/<sha256>` (contents, written before the manifest names them).
pub struct Overlay {
    dir: PathBuf,
    state: Mutex<State>,
}

impl Overlay {
    /// The layer stored in `dir`, or a new empty one (the directory is created).
    pub fn open(dir: PathBuf) -> Result<Self> {
        std::fs::create_dir_all(&dir).map_err(|err| persist::io_context("create", &dir, err))?;
        let manifest = dir.join(MANIFEST);
        let state = if manifest.is_file() {
            Self::load(&dir, persist::read_json(&manifest)?)?
        } else {
            State::default()
        };
        Ok(Self {
            dir,
            state: Mutex::new(state),
        })
    }

    fn load(dir: &Path, manifest: Manifest) -> Result<State> {
        if !(1..=VERSION).contains(&manifest.version) {
            return Err(Error::Workspace(format!(
                "overlay {} has version {}",
                crate::paths::display(dir),
                manifest.version
            )));
        }
        let blob = |hash: &str| -> Result<Vec<u8>> {
            let path = dir.join(BLOBS).join(hash);
            let bytes =
                std::fs::read(&path).map_err(|err| persist::io_context("read", &path, err))?;
            if content_hash(&bytes) != hash {
                return Err(Error::Workspace(format!(
                    "overlay blob {} is damaged",
                    crate::paths::display(&path)
                )));
            }
            Ok(bytes)
        };
        let mut state = State::default();
        for entry in manifest.entries {
            let path = normalize(Path::new(&entry.path))?;
            let content = match &entry.content {
                Some(hash) => Content::File(Arc::new(blob(hash)?)),
                None => Content::Deleted,
            };
            state.stored_entries.insert(key(&path), entry.clone());
            state.entries.insert(
                key(&path),
                Change {
                    path,
                    content,
                    base_hash: entry.base_hash,
                },
            );
        }
        let stored = if manifest.version == 1 {
            state.unlogged.clone_from(&manifest.intents);
            manifest.intents
        } else {
            state.log_bytes = manifest.intents_bytes;
            Self::read_log(dir, manifest.intents_bytes)?
        };
        for intent in stored {
            state.intents.push(match intent {
                StoredIntent::Edit {
                    path,
                    base_hash,
                    old,
                    new,
                    replace_all,
                } => Intent::Edit {
                    path: normalize(Path::new(&path))?,
                    base_hash,
                    old,
                    new,
                    replace_all,
                },
                StoredIntent::Write {
                    path,
                    base_hash,
                    content,
                } => Intent::Write {
                    path: normalize(Path::new(&path))?,
                    base_hash,
                    content: blob(&content)?,
                },
                StoredIntent::Delete { path, base_hash } => Intent::Delete {
                    path: normalize(Path::new(&path))?,
                    base_hash,
                },
            });
        }
        Ok(state)
    }

    /// The first `bytes` of the intent log, one intent per line.
    fn read_log(dir: &Path, bytes: u64) -> Result<Vec<StoredIntent>> {
        if bytes == 0 {
            return Ok(Vec::new());
        }
        let path = dir.join(INTENTS);
        let mut data =
            std::fs::read(&path).map_err(|err| persist::io_context("read", &path, err))?;
        if (data.len() as u64) < bytes {
            return Err(Error::Workspace(format!(
                "overlay intent log {} is shorter than its manifest says",
                crate::paths::display(&path)
            )));
        }
        data.truncate(bytes as usize);
        data.split(|byte| *byte == b'\n')
            .filter(|line| !line.is_empty())
            .map(|line| serde_json::from_slice(line).map_err(Error::from))
            .collect()
    }

    pub fn dir(&self) -> &Path {
        &self.dir
    }

    fn state(&self) -> Result<MutexGuard<'_, State>> {
        self.state
            .lock()
            .map_err(|_| Error::Workspace("overlay lock poisoned".into()))
    }

    /// What this layer holds for `relative`, if it changed it.
    pub fn get(&self, relative: &Path) -> Option<Content> {
        let state = self.state().ok()?;
        state
            .entries
            .get(&key(relative))
            .map(|change| change.content.clone())
    }

    /// Every path this layer changed, ordered by key.
    pub fn changes(&self) -> Vec<Change> {
        self.state()
            .map(|state| state.entries.values().cloned().collect())
            .unwrap_or_default()
    }

    /// Paths this layer wrote or deleted, ordered by key.
    pub fn changed_paths(&self) -> Vec<PathBuf> {
        self.changes()
            .into_iter()
            .map(|change| change.path)
            .collect()
    }

    /// The layer's changes in the order they were made.
    pub fn intents(&self) -> Vec<Intent> {
        self.state()
            .map(|state| state.intents.clone())
            .unwrap_or_default()
    }

    pub fn is_empty(&self) -> bool {
        self.state().map_or(true, |state| state.entries.is_empty())
    }

    /// The content `relative` had before this layer first wrote it (what the sub-task
    /// started from, from a lower layer or the main workspace): `Ok(None)` when it did not
    /// exist then, an error when the layer never changed it or the saved copy is damaged.
    /// Only the paths a layer changes get such a copy, saved on their first write.
    pub fn base(&self, relative: &Path) -> Result<Option<Vec<u8>>> {
        let base_hash = {
            let state = self.state()?;
            let change = state.entries.get(&key(relative)).ok_or_else(|| {
                Error::Workspace(format!(
                    "the overlay did not change {}",
                    relative.to_string_lossy()
                ))
            })?;
            change.base_hash.clone()
        };
        let Some(hash) = base_hash else {
            return Ok(None);
        };
        let path = self.dir.join(BLOBS).join(&hash);
        let bytes = std::fs::read(&path).map_err(|err| persist::io_context("read", &path, err))?;
        if content_hash(&bytes) != hash {
            return Err(Error::Workspace(format!(
                "overlay blob {} is damaged",
                crate::paths::display(&path)
            )));
        }
        Ok(Some(bytes))
    }

    /// Stores `content` for `path` with its intent. `seen` is the content the path had
    /// before this change; on the layer's first write of the path its hash becomes the
    /// entry's base and a copy is saved ([`Overlay::base`]).
    fn record(
        &self,
        path: PathBuf,
        content: Content,
        seen: Option<&[u8]>,
        intent: Intent,
    ) -> Result<()> {
        let mut state = self.state()?;
        let entry_key = key(&path);
        let base_hash = match state.entries.get(&entry_key) {
            Some(change) => change.base_hash.clone(),
            None => seen.map(|bytes| self.blob(bytes)).transpose()?,
        };
        // Only this change's blobs are stored and hashed; the rest are in the manifest.
        let stored = StoredEntry {
            path: slashed(&path),
            content: match &content {
                Content::File(bytes) => Some(self.blob(bytes)?),
                Content::Deleted => None,
            },
            base_hash: base_hash.clone(),
        };
        // The intent goes to the end of the log (a version 1 layer's intents first),
        // then the manifest confirms the log's new length.
        let mut lines = Vec::new();
        for pending in state
            .unlogged
            .iter()
            .cloned()
            .chain([self.stored_intent(&intent)?])
        {
            serde_json::to_writer(&mut lines, &pending)?;
            lines.push(b'\n');
        }
        let log_bytes = persist::append_at(&self.dir.join(INTENTS), state.log_bytes, &lines)?;
        let previous = state.stored_entries.insert(entry_key.clone(), stored);
        if let Err(err) = self.persist(&state, log_bytes) {
            match previous {
                Some(previous) => state.stored_entries.insert(entry_key, previous),
                None => state.stored_entries.remove(&entry_key),
            };
            return Err(err);
        }
        state.log_bytes = log_bytes;
        state.unlogged.clear();
        state.entries.insert(
            entry_key,
            Change {
                path,
                content,
                base_hash,
            },
        );
        state.intents.push(intent);
        Ok(())
    }

    fn blob(&self, bytes: &[u8]) -> Result<String> {
        let hash = content_hash(bytes);
        let path = self.dir.join(BLOBS).join(&hash);
        if !path.is_file() {
            persist::write_atomic(&path, bytes, false)?;
        }
        Ok(hash)
    }

    fn persist(&self, state: &State, intents_bytes: u64) -> Result<()> {
        persist::atomic_json(
            &self.dir.join(MANIFEST),
            &ManifestRef {
                version: VERSION,
                entries: state.stored_entries.values().collect(),
                intents_bytes,
            },
        )
    }

    /// The manifest form of `intent` (a `Write` stores its content as a blob).
    fn stored_intent(&self, intent: &Intent) -> Result<StoredIntent> {
        Ok(match intent {
            Intent::Edit {
                path,
                base_hash,
                old,
                new,
                replace_all,
            } => StoredIntent::Edit {
                path: slashed(path),
                base_hash: base_hash.clone(),
                old: old.clone(),
                new: new.clone(),
                replace_all: *replace_all,
            },
            Intent::Write {
                path,
                base_hash,
                content,
            } => StoredIntent::Write {
                path: slashed(path),
                base_hash: base_hash.clone(),
                content: self.blob(content)?,
            },
            Intent::Delete { path, base_hash } => StoredIntent::Delete {
                path: slashed(path),
                base_hash: base_hash.clone(),
            },
        })
    }
}

/// A file as the layers hold it: workspace-relative path and content.
pub type LayeredFile = (PathBuf, Arc<Vec<u8>>);

/// What a path is in a stack's view.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Kind {
    File(u64),
    Dir,
}

/// The main workspace under ordered layers. Reads look through the layers top-down, then
/// at the disk; writes go only into the top layer. Safe to share between concurrent tool
/// calls: a write compares and stores under one lock.
pub struct OverlayStack {
    root: PathBuf,
    /// Bottom first; the last one is writable.
    layers: Vec<Arc<Overlay>>,
    write: Mutex<()>,
}

impl OverlayStack {
    /// `root` is the main workspace; `layers` are bottom first and the last one takes the
    /// writes (B depending on A: `[a, b]`).
    pub fn new(root: &Path, layers: Vec<Arc<Overlay>>) -> Result<Self> {
        if layers.is_empty() {
            return Err(Error::Workspace("an overlay stack needs a layer".into()));
        }
        Ok(Self {
            root: root.canonicalize()?,
            layers,
            write: Mutex::new(()),
        })
    }

    /// The main workspace, canonical.
    pub fn root(&self) -> &Path {
        &self.root
    }

    pub fn layers(&self) -> &[Arc<Overlay>] {
        &self.layers
    }

    /// The writable layer.
    pub fn top(&self) -> &Arc<Overlay> {
        self.layers.last().expect("checked in new")
    }

    /// What the layers hold for `relative`, the topmost layer that has it deciding.
    pub fn lookup(&self, relative: &Path) -> Option<Content> {
        self.layers
            .iter()
            .rev()
            .find_map(|layer| layer.get(relative))
    }

    /// Current bytes of `relative`: from the layers, else from the main workspace.
    pub fn read(&self, relative: &Path) -> std::io::Result<Vec<u8>> {
        match self.lookup(relative) {
            Some(Content::File(bytes)) => Ok(bytes.as_ref().clone()),
            Some(Content::Deleted) => Err(std::io::ErrorKind::NotFound.into()),
            None => std::fs::read(self.root.join(relative)),
        }
    }

    /// Files the layers hold below `relative` (a directory nobody created on disk).
    fn implied_dir(&self, relative: &Path) -> bool {
        let prefix = key(relative);
        self.files()
            .iter()
            .any(|(path, _)| under(&prefix, &key(path)))
    }

    /// Whether `relative` is a file (with its length) or a directory in this view.
    pub fn stat(&self, relative: &Path) -> std::io::Result<Kind> {
        match self.lookup(relative) {
            Some(Content::File(bytes)) => return Ok(Kind::File(bytes.len() as u64)),
            Some(Content::Deleted) if self.implied_dir(relative) => return Ok(Kind::Dir),
            Some(Content::Deleted) => return Err(std::io::ErrorKind::NotFound.into()),
            None => {}
        }
        match std::fs::metadata(self.root.join(relative)) {
            Ok(meta) if meta.is_dir() => Ok(Kind::Dir),
            Ok(meta) => Ok(Kind::File(meta.len())),
            Err(_) if self.implied_dir(relative) => Ok(Kind::Dir),
            Err(err) => Err(err),
        }
    }

    /// Entries directly in the directory `relative` (name, is a directory): the disk's,
    /// without files deleted in the layers, plus what the layers created.
    pub fn list(&self, relative: &Path) -> std::io::Result<Vec<(String, bool)>> {
        let mut names: BTreeMap<String, (String, bool)> = BTreeMap::new();
        let disk = std::fs::read_dir(self.root.join(relative));
        let implied = self.implied_dir(relative);
        match disk {
            Ok(entries) => {
                for entry in entries.flatten() {
                    let name = entry.file_name().to_string_lossy().into_owned();
                    let dir = entry.file_type().is_ok_and(|kind| kind.is_dir());
                    let path = relative.join(&name);
                    if !dir && self.lookup(&path) == Some(Content::Deleted) {
                        continue;
                    }
                    names.insert(key(Path::new(&name)), (name, dir));
                }
            }
            Err(err) if !implied => return Err(err),
            Err(_) => {}
        }
        let prefix = key(relative);
        for (path, _) in self.files() {
            let full = key(&path);
            if !under(&prefix, &full) {
                continue;
            }
            let Ok(rest) = path.strip_prefix(relative) else {
                continue;
            };
            let mut parts = rest.components();
            let Some(first) = parts.next() else { continue };
            let name = first.as_os_str().to_string_lossy().into_owned();
            let dir = parts.next().is_some();
            names
                .entry(key(Path::new(&name)))
                .and_modify(|held| held.1 |= dir)
                .or_insert((name, dir));
        }
        Ok(names.into_values().collect())
    }

    /// Current content of `relative`, `None` when absent.
    fn current(&self, relative: &Path) -> Result<Option<Vec<u8>>> {
        match self.read(relative) {
            Ok(bytes) => Ok(Some(bytes)),
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(err) => Err(err.into()),
        }
    }

    /// Writes `data` to `relative` in the top layer when its content is still `expected`
    /// (`None`: absent). `edit` records the change as an `Edit` intent (the file must
    /// exist), otherwise it is a `Write`.
    pub fn write(
        &self,
        relative: &Path,
        expected: Option<&[u8]>,
        data: &[u8],
        edit: Option<EditSpec>,
    ) -> Result<()> {
        let relative = normalize(relative)?;
        if relative.as_os_str().is_empty() {
            return Err(Error::Workspace("cannot write the workspace root".into()));
        }
        if !crate::session::workspace::within_limit(data) {
            return Err(Error::Workspace(
                "new content exceeds 1 MiB (4 MiB for non-UTF-8)".into(),
            ));
        }
        let _guard = self
            .write
            .lock()
            .map_err(|_| Error::Workspace("overlay lock poisoned".into()))?;
        if self.current(&relative)?.as_deref() != expected {
            return Err(Error::Workspace("file changed before replacement".into()));
        }
        let seen = expected.map(content_hash);
        let intent = match edit {
            Some(edit) => Intent::Edit {
                path: relative.clone(),
                base_hash: seen
                    .clone()
                    .ok_or_else(|| Error::Workspace("edit needs an existing file".into()))?,
                old: edit.old,
                new: edit.new,
                replace_all: edit.replace_all,
            },
            None => Intent::Write {
                path: relative.clone(),
                base_hash: seen.clone(),
                content: data.to_vec(),
            },
        };
        self.top().record(
            relative,
            Content::File(Arc::new(data.to_vec())),
            expected,
            intent,
        )
    }

    /// Marks `relative` deleted in the top layer when its content is still `expected`.
    pub fn delete(&self, relative: &Path, expected: &[u8]) -> Result<()> {
        let relative = normalize(relative)?;
        let _guard = self
            .write
            .lock()
            .map_err(|_| Error::Workspace("overlay lock poisoned".into()))?;
        if self.current(&relative)?.as_deref() != Some(expected) {
            return Err(Error::Workspace("file changed before deletion".into()));
        }
        let seen = content_hash(expected);
        let intent = Intent::Delete {
            path: relative.clone(),
            base_hash: seen.clone(),
        };
        self.top()
            .record(relative, Content::Deleted, Some(expected), intent)
    }

    /// Paths the top layer wrote or deleted.
    pub fn changed_paths(&self) -> Vec<PathBuf> {
        self.top().changed_paths()
    }

    /// Every path any layer wrote or deleted: their disk content does not count.
    pub fn shadowed(&self) -> Vec<PathBuf> {
        let mut seen = HashSet::new();
        let mut out = Vec::new();
        for layer in self.layers.iter().rev() {
            for path in layer.changed_paths() {
                if seen.insert(key(&path)) {
                    out.push(path);
                }
            }
        }
        out.sort_by_key(|path| key(path));
        out
    }

    /// Files as the layers hold them now (deleted ones left out), ordered by key.
    pub fn files(&self) -> Vec<LayeredFile> {
        let mut decided: BTreeMap<String, Option<LayeredFile>> = BTreeMap::new();
        for layer in self.layers.iter().rev() {
            for change in layer.changes() {
                decided
                    .entry(key(&change.path))
                    .or_insert(match change.content {
                        Content::File(bytes) => Some((change.path, bytes)),
                        Content::Deleted => None,
                    });
            }
        }
        decided.into_values().flatten().collect()
    }

    /// Workspace-relative entries of a disk listing merged with the layers: `base` without
    /// what the layers deleted, then the files the layers created (ordered by key).
    pub fn merge_entries(&self, base: impl IntoIterator<Item = PathBuf>) -> Vec<PathBuf> {
        let mut seen = HashSet::new();
        let mut out = Vec::new();
        for path in base {
            if self.lookup(&path) == Some(Content::Deleted) || !seen.insert(key(&path)) {
                continue;
            }
            out.push(path);
        }
        for (path, _) in self.files() {
            if seen.insert(key(&path)) {
                out.push(path);
            }
        }
        out
    }
}

/// Whether the key `path` lies below the directory key `prefix` (`""`: the root).
fn under(prefix: &str, path: &str) -> bool {
    prefix.is_empty()
        || path
            .strip_prefix(prefix)
            .is_some_and(|rest| rest.starts_with('/'))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    fn stack(root: &Path, layers: &[&Path]) -> OverlayStack {
        let layers = layers
            .iter()
            .map(|dir| Arc::new(Overlay::open(dir.to_path_buf()).unwrap()))
            .collect();
        OverlayStack::new(root, layers).unwrap()
    }

    #[test]
    fn writes_stay_in_the_top_layer_and_reads_fall_through_to_the_disk() {
        let work = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        std::fs::write(work.path().join("a.txt"), "one\n").unwrap();
        let stack = stack(work.path(), &[&data.path().join("t1")]);
        assert_eq!(stack.read(Path::new("a.txt")).unwrap(), b"one\n");
        stack
            .write(Path::new("a.txt"), Some(b"one\n"), b"two\n", None)
            .unwrap();
        assert_eq!(stack.read(Path::new("a.txt")).unwrap(), b"two\n");
        assert_eq!(std::fs::read(work.path().join("a.txt")).unwrap(), b"one\n");
        // A stale expectation is refused, like a file changed on disk.
        assert!(
            stack
                .write(Path::new("a.txt"), Some(b"one\n"), b"x", None)
                .is_err()
        );
        stack
            .write(Path::new("new/b.txt"), None, b"b\n", None)
            .unwrap();
        assert_eq!(stack.stat(Path::new("new")).unwrap(), Kind::Dir);
        assert_eq!(stack.stat(Path::new("new/b.txt")).unwrap(), Kind::File(2));
        assert!(!work.path().join("new").exists());
        assert_eq!(
            stack.changed_paths(),
            [PathBuf::from("a.txt"), PathBuf::from("new").join("b.txt")]
        );
        let changes = stack.top().changes();
        assert_eq!(changes[0].base_hash, Some(content_hash(b"one\n")));
        assert_eq!(changes[1].base_hash, None);
    }

    #[test]
    fn deletions_hide_disk_files_and_listings_merge_the_layers() {
        let work = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        std::fs::create_dir(work.path().join("src")).unwrap();
        std::fs::write(work.path().join("src/a.rs"), "a").unwrap();
        std::fs::write(work.path().join("src/b.rs"), "b").unwrap();
        let stack = stack(work.path(), &[&data.path().join("t1")]);
        stack.delete(Path::new("src/a.rs"), b"a").unwrap();
        stack
            .write(Path::new("src/c/d.rs"), None, b"d", None)
            .unwrap();
        assert!(stack.read(Path::new("src/a.rs")).is_err());
        assert!(stack.stat(Path::new("src/a.rs")).is_err());
        assert_eq!(
            stack.list(Path::new("src")).unwrap(),
            [("b.rs".to_owned(), false), ("c".to_owned(), true)]
        );
        let merged = stack.merge_entries([PathBuf::from("src/a.rs"), PathBuf::from("src/b.rs")]);
        assert_eq!(
            merged,
            [
                PathBuf::from("src/b.rs"),
                PathBuf::from("src").join("c").join("d.rs")
            ]
        );
        assert!(matches!(
            stack.top().intents()[0],
            Intent::Delete { ref base_hash, .. } if *base_hash == content_hash(b"a")
        ));
        assert!(work.path().join("src/a.rs").exists());
    }

    #[test]
    fn a_dependent_layer_reads_through_the_one_below_and_writes_only_its_own() {
        let work = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        std::fs::write(work.path().join("a.txt"), "base").unwrap();
        let a = stack(work.path(), &[&data.path().join("a")]);
        a.write(Path::new("a.txt"), Some(b"base"), b"from a", None)
            .unwrap();
        let b = stack(
            work.path(),
            &[&data.path().join("a"), &data.path().join("b")],
        );
        assert_eq!(b.read(Path::new("a.txt")).unwrap(), b"from a");
        b.write(
            Path::new("a.txt"),
            Some(b"from a"),
            b"from b",
            Some(EditSpec {
                old: "a".into(),
                new: "b".into(),
                replace_all: false,
            }),
        )
        .unwrap();
        assert_eq!(b.read(Path::new("a.txt")).unwrap(), b"from b");
        assert_eq!(
            b.layers()[0].get(Path::new("a.txt")),
            a.lookup(Path::new("a.txt"))
        );
        assert!(matches!(
            &b.top().intents()[0],
            Intent::Edit { base_hash, old, .. } if *base_hash == content_hash(b"from a") && old == "a"
        ));
        assert_eq!(
            b.top().changes()[0].base_hash,
            Some(content_hash(b"from a"))
        );
    }

    #[test]
    fn a_layer_is_restored_from_its_directory() {
        let work = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        std::fs::write(work.path().join("a.txt"), "1").unwrap();
        std::fs::write(work.path().join("gone.txt"), "g").unwrap();
        let dir = data.path().join("task").join("sub");
        {
            let stack = stack(work.path(), &[&dir]);
            stack
                .write(Path::new("a.txt"), Some(b"1"), b"2", None)
                .unwrap();
            stack
                .write(
                    Path::new("a.txt"),
                    Some(b"2"),
                    b"3",
                    Some(EditSpec {
                        old: "2".into(),
                        new: "3".into(),
                        replace_all: true,
                    }),
                )
                .unwrap();
            stack.delete(Path::new("gone.txt"), b"g").unwrap();
        }
        let layer = Overlay::open(dir.clone()).unwrap();
        assert_eq!(
            layer.get(Path::new("a.txt")),
            Some(Content::File(Arc::new(b"3".to_vec())))
        );
        assert_eq!(layer.get(Path::new("gone.txt")), Some(Content::Deleted));
        let intents = layer.intents();
        assert_eq!(intents.len(), 3);
        assert_eq!(
            intents[0],
            Intent::Write {
                path: "a.txt".into(),
                base_hash: Some(content_hash(b"1")),
                content: b"2".to_vec()
            }
        );
        assert!(matches!(
            &intents[1],
            Intent::Edit {
                replace_all: true,
                ..
            }
        ));
        // The first write's base stays the entry's base.
        assert_eq!(layer.changes()[0].base_hash, Some(content_hash(b"1")));
        // A damaged blob is reported, not read as content.
        let blob = dir.join(BLOBS).join(content_hash(b"3"));
        std::fs::write(&blob, "x").unwrap();
        assert!(Overlay::open(dir).is_err());
    }

    #[test]
    fn paths_leaving_the_workspace_or_naming_git_metadata_are_refused() {
        let work = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        let stack = stack(work.path(), &[&data.path().join("t")]);
        for bad in ["../x", ".git/config", "a/.GIT/x"] {
            assert!(
                stack.write(Path::new(bad), None, b"x", None).is_err(),
                "{bad}"
            );
        }
        assert!(stack.write(Path::new(""), None, b"x", None).is_err());
    }

    #[test]
    fn concurrent_writes_to_one_file_keep_every_change() {
        let work = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        std::fs::write(work.path().join("n.txt"), "0").unwrap();
        let stack = Arc::new(stack(work.path(), &[&data.path().join("t")]));
        let threads: Vec<_> = (0..4)
            .map(|_| {
                let stack = stack.clone();
                std::thread::spawn(move || {
                    let mut done = 0;
                    while done < 5 {
                        let now = stack.read(Path::new("n.txt")).unwrap();
                        let next: u32 = String::from_utf8_lossy(&now).parse::<u32>().unwrap() + 1;
                        if stack
                            .write(
                                Path::new("n.txt"),
                                Some(&now),
                                next.to_string().as_bytes(),
                                None,
                            )
                            .is_ok()
                        {
                            done += 1;
                        }
                    }
                })
            })
            .collect();
        for thread in threads {
            thread.join().unwrap();
        }
        assert_eq!(stack.read(Path::new("n.txt")).unwrap(), b"20");
        assert_eq!(stack.top().intents().len(), 20);
    }

    #[test]
    fn the_first_write_of_a_path_keeps_a_copy_of_its_base() {
        let work = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        std::fs::write(work.path().join("a.txt"), "base\n").unwrap();
        std::fs::write(work.path().join("b.txt"), "other\n").unwrap();
        let lower = stack(work.path(), &[&data.path().join("a")]);
        lower
            .write(Path::new("a.txt"), Some(b"base\n"), b"one\n", None)
            .unwrap();
        lower
            .write(Path::new("a.txt"), Some(b"one\n"), b"two\n", None)
            .unwrap();
        lower.write(Path::new("new.txt"), None, b"n", None).unwrap();
        let top = lower.top();
        // The base is what the path held before the first write, not the one before.
        assert_eq!(top.base(Path::new("a.txt")).unwrap().unwrap(), b"base\n");
        assert_eq!(top.base(Path::new("new.txt")).unwrap(), None);
        // Unchanged paths have no base copy: only changed files are saved.
        assert!(top.base(Path::new("b.txt")).is_err());
        let saved = std::fs::read_dir(data.path().join("a").join(BLOBS))
            .unwrap()
            .count();
        // base, one, two and n: the base copy adds one blob for the changed file.
        assert_eq!(saved, 4);
        // A dependent layer's base is what it saw: the lower layer's content.
        let upper = stack(
            work.path(),
            &[&data.path().join("a"), &data.path().join("b")],
        );
        upper
            .write(Path::new("a.txt"), Some(b"two\n"), b"three\n", None)
            .unwrap();
        assert_eq!(
            upper.top().base(Path::new("a.txt")).unwrap().unwrap(),
            b"two\n"
        );
        // The copy survives a restore and is checked like the other blobs.
        let restored = Overlay::open(data.path().join("a")).unwrap();
        assert_eq!(
            restored.base(Path::new("a.txt")).unwrap().unwrap(),
            b"base\n"
        );
        std::fs::write(
            data.path()
                .join("a")
                .join(BLOBS)
                .join(content_hash(b"base\n")),
            "x",
        )
        .unwrap();
        assert!(restored.base(Path::new("a.txt")).is_err());
    }

    #[test]
    fn repeated_writes_are_stored_one_change_at_a_time_and_restore_in_full() {
        let work = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        std::fs::write(work.path().join("a.txt"), "0").unwrap();
        let dir = data.path().join("t");
        let stack = stack(work.path(), &[&dir]);
        let mut current = b"0".to_vec();
        for round in 1..=40 {
            let next = round.to_string().into_bytes();
            stack
                .write(Path::new("a.txt"), Some(&current), &next, None)
                .unwrap();
            current = next;
        }
        stack.write(Path::new("b.txt"), None, b"b", None).unwrap();
        stack.delete(Path::new("b.txt"), b"b").unwrap();
        let restored = Overlay::open(dir).unwrap();
        assert_eq!(restored.changes(), stack.top().changes());
        assert_eq!(restored.intents(), stack.top().intents());
        assert_eq!(restored.intents().len(), 42);
        assert_eq!(
            restored.get(Path::new("a.txt")),
            Some(Content::File(Arc::new(b"40".to_vec())))
        );
        assert_eq!(restored.get(Path::new("b.txt")), Some(Content::Deleted));
        assert_eq!(
            restored.base(Path::new("a.txt")).unwrap(),
            Some(b"0".to_vec())
        );
    }

    #[test]
    fn intents_are_appended_to_a_log_and_an_unconfirmed_tail_is_dropped() {
        let work = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        std::fs::write(work.path().join("a.txt"), "one\n").unwrap();
        let dir = data.path().join("t");
        let stack = stack(work.path(), &[&dir]);
        let edit = |old: &str, new: &str| EditSpec {
            old: old.into(),
            new: new.into(),
            replace_all: false,
        };
        stack
            .write(
                Path::new("a.txt"),
                Some(b"one\n"),
                b"two\n",
                Some(edit("one", "two")),
            )
            .unwrap();
        let manifest = std::fs::read_to_string(dir.join(MANIFEST)).unwrap();
        assert!(!manifest.contains("\"old\""), "{manifest}");
        let log = std::fs::read(dir.join(INTENTS)).unwrap();
        // A write that crashed before its manifest: the log has a tail nobody confirmed.
        std::fs::OpenOptions::new()
            .append(true)
            .open(dir.join(INTENTS))
            .unwrap()
            .write_all(b"{\"kind\":\"edit\",\"pa")
            .unwrap();
        let restored = Overlay::open(dir.clone()).unwrap();
        assert_eq!(restored.intents(), stack.top().intents());
        drop(restored);
        stack
            .write(
                Path::new("a.txt"),
                Some(b"two\n"),
                b"three\n",
                Some(edit("two", "three")),
            )
            .unwrap();
        let grown = std::fs::read(dir.join(INTENTS)).unwrap();
        assert!(grown.starts_with(&log) && !grown.windows(4).any(|w| w == b"\"pa{"));
        let restored = Overlay::open(dir).unwrap();
        assert_eq!(restored.intents(), stack.top().intents());
        assert_eq!(restored.intents().len(), 2);
        assert_eq!(restored.changes(), stack.top().changes());
    }

    #[test]
    fn a_version_1_layer_is_read_and_moves_its_intents_to_the_log() {
        let work = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        std::fs::write(work.path().join("a.txt"), "one\n").unwrap();
        let dir = data.path().join("t");
        std::fs::create_dir_all(dir.join(BLOBS)).unwrap();
        for bytes in [&b"one\n"[..], b"two\n"] {
            std::fs::write(dir.join(BLOBS).join(content_hash(bytes)), bytes).unwrap();
        }
        let manifest = serde_json::json!({
            "version": 1,
            "entries": [{"path":"a.txt","content":content_hash(b"two\n"),"base_hash":content_hash(b"one\n")}],
            "intents": [{"kind":"edit","path":"a.txt","base_hash":content_hash(b"one\n"),"old":"one","new":"two","replace_all":false}]
        });
        std::fs::write(dir.join(MANIFEST), manifest.to_string()).unwrap();
        let layer = Arc::new(Overlay::open(dir.clone()).unwrap());
        assert_eq!(layer.intents().len(), 1);
        assert_eq!(
            layer.base(Path::new("a.txt")).unwrap(),
            Some(b"one\n".to_vec())
        );
        let stack = OverlayStack::new(work.path(), vec![layer.clone()]).unwrap();
        stack.write(Path::new("b.txt"), None, b"b\n", None).unwrap();
        let restored = Overlay::open(dir.clone()).unwrap();
        assert_eq!(restored.intents(), layer.intents());
        assert_eq!(restored.intents().len(), 2);
        assert_eq!(restored.changes(), layer.changes());
        let manifest = std::fs::read_to_string(dir.join(MANIFEST)).unwrap();
        assert!(
            manifest.contains("\"version\":2") && !manifest.contains("\"old\""),
            "{manifest}"
        );
    }
}
