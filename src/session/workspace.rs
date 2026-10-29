use std::{
    borrow::Cow,
    io::Write,
    path::{Component, Path, PathBuf},
    sync::Mutex,
};

use serde::{Deserialize, Serialize};
use similar::TextDiff;

use crate::{
    domain::{Error, Result},
    session::{Store, atomic_json, new_id, read_json},
};

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
struct Image {
    text: String,
    /// Base64 of content that is not UTF-8 (a GBK file); `text` is then empty.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    raw: Option<String>,
    readonly: bool,
    mode: Option<u32>,
}

/// UTF-8 content as text, anything else as base64.
fn payload(bytes: &[u8]) -> (String, Option<String>) {
    use base64::Engine;
    match std::str::from_utf8(bytes) {
        Ok(text) => (text.to_owned(), None),
        Err(_) => (
            String::new(),
            Some(base64::engine::general_purpose::STANDARD.encode(bytes)),
        ),
    }
}

fn unpayload<'a>(text: &'a str, raw: Option<&str>) -> Cow<'a, [u8]> {
    use base64::Engine;
    match raw {
        Some(raw) => Cow::Owned(
            base64::engine::general_purpose::STANDARD
                .decode(raw)
                .unwrap_or_default(),
        ),
        None => Cow::Borrowed(text.as_bytes()),
    }
}

impl Image {
    fn bytes(&self) -> Cow<'_, [u8]> {
        unpayload(&self.text, self.raw.as_deref())
    }

    /// GBK content as text; `None` for bytes that are no text encoding we read.
    fn decoded(&self) -> Option<String> {
        crate::tools::text_encoding::decode_gbk(&self.bytes()).map(|(text, _)| text)
    }

    /// Text for diffs: GBK content decoded, other bytes lossily.
    fn display(&self) -> Cow<'_, str> {
        if self.raw.is_none() {
            return Cow::Borrowed(&self.text);
        }
        let bytes = self.bytes();
        Cow::Owned(match crate::tools::text_encoding::decode_gbk(&bytes) {
            Some((text, _)) => text,
            None => String::from_utf8_lossy(&bytes).into_owned(),
        })
    }
}

/// UTF-8 text is checkpointed up to 1 MiB; other bytes (GBK text, generated PNGs) up to 4 MiB.
const TEXT_LIMIT: usize = 1024 * 1024;
const BINARY_LIMIT: usize = 4 * 1024 * 1024;

fn within_limit(bytes: &[u8]) -> bool {
    bytes.len() <= TEXT_LIMIT
        || (bytes.len() <= BINARY_LIMIT && std::str::from_utf8(bytes).is_err())
}

fn image(path: &Path) -> Result<Option<Image>> {
    let metadata = match std::fs::symlink_metadata(path) {
        Ok(metadata) => metadata,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(err) => return Err(err.into()),
    };
    if !metadata.is_file()
        || metadata.file_type().is_symlink()
        || metadata.len() > BINARY_LIMIT as u64
    {
        return Err(Error::Workspace(
            "snapshots require regular files (UTF-8 <= 1 MiB, other <= 4 MiB)".into(),
        ));
    }
    #[cfg(unix)]
    let mode = {
        use std::os::unix::fs::PermissionsExt;
        Some(metadata.permissions().mode())
    };
    #[cfg(not(unix))]
    let mode = None;
    let bytes = std::fs::read(path)?;
    if !within_limit(&bytes) {
        return Err(Error::Workspace(
            "snapshots require UTF-8 files <= 1 MiB".into(),
        ));
    }
    let (text, raw) = payload(&bytes);
    Ok(Some(Image {
        text,
        raw,
        readonly: metadata.permissions().readonly(),
        mode,
    }))
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
enum ChangeState {
    Intent,
    Applied,
    Reverted,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
struct Change {
    path: PathBuf,
    before: Option<Image>,
    after: Option<Image>,
    state: ChangeState,
    #[serde(default)]
    intended: Option<String>,
    /// Base64 of an intended content that is not UTF-8.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    intended_raw: Option<String>,
}

impl Change {
    fn set_intended(&mut self, data: Option<&[u8]>) {
        match data {
            Some(data) => {
                let (text, raw) = payload(data);
                self.intended = Some(text);
                self.intended_raw = raw;
            }
            None => {
                self.intended = None;
                self.intended_raw = None;
            }
        }
    }

    fn intended_bytes(&self) -> Option<Cow<'_, [u8]>> {
        self.intended
            .as_deref()
            .map(|text| unpayload(text, self.intended_raw.as_deref()))
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
struct Checkpoint {
    version: u32,
    id: String,
    root: PathBuf,
    changes: Vec<Change>,
    shell_effects: bool,
    undone: bool,
}

#[derive(Serialize, Deserialize)]
struct SwapJournal {
    version: u32,
    root: PathBuf,
    files: Vec<SwapFile>,
}

#[derive(Serialize, Deserialize)]
struct SwapFile {
    path: PathBuf,
    current: String,
    baseline: String,
}

pub struct Workspace {
    root: PathBuf,
    store: Store,
    current: Mutex<Option<Checkpoint>>,
    /// Lines added and removed by edits since the last `take_line_changes`.
    lines: Mutex<(u64, u64)>,
    /// Files temporarily holding baseline content during regression verification.
    swapped: Mutex<std::collections::HashSet<PathBuf>>,
}

impl Workspace {
    pub fn new(root: PathBuf, store: Store) -> Result<Self> {
        Ok(Self {
            root: root.canonicalize()?,
            store,
            current: Mutex::new(None),
            lines: Mutex::default(),
            swapped: Mutex::default(),
        })
    }

    /// Lines added and removed by edits since the previous call.
    pub fn take_line_changes(&self) -> (u64, u64) {
        self.lines
            .lock()
            .map(|mut lines| std::mem::take(&mut *lines))
            .unwrap_or_default()
    }

    pub fn begin(&self) -> Result<String> {
        let checkpoint = Checkpoint {
            version: 1,
            id: new_id(),
            root: self.root.clone(),
            changes: Vec::new(),
            shell_effects: false,
            undone: false,
        };
        let id = checkpoint.id.clone();
        self.persist(&checkpoint)?;
        *self
            .current
            .lock()
            .map_err(|_| Error::Workspace("checkpoint lock poisoned".into()))? = Some(checkpoint);
        Ok(id)
    }

    pub fn activate(&self, id: Option<&str>) -> Result<()> {
        let checkpoint = match id {
            Some(id) => {
                let cp: Checkpoint = read_json(&self.store.checkpoint_path(id)?)?;
                if cp.version != 1 || cp.id != id || cp.root != self.root {
                    return Err(Error::Workspace(
                        "checkpoint identity or workspace mismatch".into(),
                    ));
                }
                for change in &cp.changes {
                    self.resolve(&change.path)?;
                }
                Some(cp)
            }
            None => None,
        };
        *self
            .current
            .lock()
            .map_err(|_| Error::Workspace("checkpoint lock poisoned".into()))? = checkpoint;
        Ok(())
    }

    fn persist(&self, checkpoint: &Checkpoint) -> Result<()> {
        atomic_json(&self.store.checkpoint_path(&checkpoint.id)?, checkpoint)
    }

    fn resolve(&self, relative: &Path) -> Result<PathBuf> {
        if relative.as_os_str().is_empty()
            || relative
                .components()
                .any(|part| !matches!(part, Component::Normal(_)))
        {
            return Err(Error::Workspace("invalid checkpoint path".into()));
        }
        #[cfg(windows)]
        if relative
            .components()
            .any(|part| part.as_os_str().to_string_lossy().contains(':'))
        {
            return Err(Error::Workspace(
                "alternate data stream in checkpoint".into(),
            ));
        }
        if relative.components().any(|part| {
            part.as_os_str()
                .to_string_lossy()
                .eq_ignore_ascii_case(".git")
        }) {
            return Err(Error::Workspace("Git metadata in checkpoint".into()));
        }
        let candidate = self.root.join(relative);
        let parent = candidate
            .parent()
            .ok_or_else(|| Error::Workspace("missing parent".into()))?
            .canonicalize()?;
        if !parent.starts_with(&self.root) {
            return Err(Error::Workspace(
                "checkpoint path escaped through a link".into(),
            ));
        }
        let path = parent.join(
            candidate
                .file_name()
                .ok_or_else(|| Error::Workspace("missing filename".into()))?,
        );
        if path != candidate {
            return Err(Error::Workspace(
                "checkpoint parent was replaced by a link".into(),
            ));
        }
        if let Ok(metadata) = std::fs::symlink_metadata(&path)
            && !metadata.is_file()
        {
            return Err(Error::Workspace(
                "checkpoint target is not a regular file".into(),
            ));
        }
        Ok(path)
    }

    pub fn replace(&self, path: &Path, expected: Option<&str>, text: &str) -> Result<()> {
        self.replace_bytes(path, expected.map(str::as_bytes), text.as_bytes())
    }

    /// Journaled replacement of a file's bytes (any encoding); `expected` is the current content.
    pub fn replace_bytes(&self, path: &Path, expected: Option<&[u8]>, text: &[u8]) -> Result<()> {
        if !within_limit(text) {
            return Err(Error::Workspace(
                "new content exceeds 1 MiB (4 MiB for non-UTF-8)".into(),
            ));
        }
        let relative = path
            .strip_prefix(&self.root)
            .map_err(|_| Error::Workspace("edit outside workspace".into()))?
            .to_path_buf();
        let resolved = self.resolve(&relative)?;
        if self
            .swapped
            .lock()
            .map_err(|_| Error::Workspace("swap lock poisoned".into()))?
            .contains(&relative)
        {
            return Err(Error::Workspace(
                "file temporarily holds its baseline for regression verification; retry after it finishes".into(),
            ));
        }
        let before = image(&resolved)?;
        if before.as_ref().map(|image| image.bytes()).as_deref() != expected {
            return Err(Error::Workspace("file changed before snapshot".into()));
        }
        let mut guard = self
            .current
            .lock()
            .map_err(|_| Error::Workspace("checkpoint lock poisoned".into()))?;
        let cp = guard
            .as_mut()
            .ok_or_else(|| Error::Workspace("no active checkpoint".into()))?;
        if cp.undone {
            return Err(Error::Workspace(
                "cannot write into a reverted checkpoint; start a new task".into(),
            ));
        }
        let index = cp.changes.iter().position(|change| change.path == relative);
        let previous = index.map(|index| cp.changes[index].clone());
        if let Some(index) = index {
            if cp.changes[index].state != ChangeState::Applied || cp.changes[index].after != before
            {
                return Err(Error::Workspace(
                    "file drifted since its previous edit".into(),
                ));
            }
        } else {
            if cp.changes.len() >= 100 {
                return Err(Error::Workspace("checkpoint exceeds 100 files".into()));
            }
            cp.changes.push(Change {
                path: relative,
                before: before.clone(),
                after: before.clone(),
                state: ChangeState::Intent,
                intended: None,
                intended_raw: None,
            });
        }
        let index = index.unwrap_or(cp.changes.len() - 1);
        cp.changes[index].state = ChangeState::Intent;
        cp.changes[index].set_intended(Some(text));
        self.persist(cp).map_err(|err| {
            Error::Uncertain(format!(
                "could not persist edit intent; no write dispatched: {err}"
            ))
        })?;
        if let Err(err) = replace_file_bytes(&resolved, expected, Some(text)) {
            let actual = image(&resolved).map_err(|read| {
                Error::Uncertain(format!(
                    "edit failed: {err}; cannot inspect effects: {read}"
                ))
            })?;
            if actual != before {
                return Err(Error::Uncertain(format!(
                    "edit failed and file changed; inspect intent before resolving: {err}"
                )));
            }
            if let Some(previous) = previous {
                cp.changes[index] = previous;
            } else {
                cp.changes.remove(index);
            }
            self.persist(cp).map_err(|save| {
                Error::Uncertain(format!(
                    "edit failed: {err}; cannot persist unchanged checkpoint: {save}"
                ))
            })?;
            return Err(err);
        }
        let after = image(&resolved).map_err(|err| {
            Error::Uncertain(format!("file replaced but result snapshot failed: {err}"))
        })?;
        if after.as_ref().map(|image| image.bytes()).as_deref() != Some(text) {
            return Err(Error::Uncertain(
                "file changed before result snapshot; inspect it manually".into(),
            ));
        }
        cp.changes[index].after = after;
        cp.changes[index].state = ChangeState::Applied;
        cp.changes[index].set_intended(None);
        let (added, removed) = line_changes(expected.unwrap_or_default(), text);
        if let Ok(mut lines) = self.lines.lock() {
            lines.0 = lines.0.saturating_add(added);
            lines.1 = lines.1.saturating_add(removed);
        }
        self.persist(cp).map_err(|err| {
            Error::Uncertain(format!("file replaced but result journal failed: {err}"))
        })
    }

    pub fn resolve_edit(&self, path: &Path, executed: bool) -> Result<()> {
        let candidate = self.root.join(path);
        let parent = candidate
            .parent()
            .ok_or_else(|| Error::Workspace("missing parent".into()))?
            .canonicalize()?;
        let target = parent.join(
            candidate
                .file_name()
                .ok_or_else(|| Error::Workspace("missing filename".into()))?,
        );
        let relative = target
            .strip_prefix(&self.root)
            .map_err(|_| Error::Workspace("resolution outside workspace".into()))?;
        let resolved = self.resolve(relative)?;
        let actual = image(&resolved)?;
        let mut guard = self
            .current
            .lock()
            .map_err(|_| Error::Workspace("checkpoint lock poisoned".into()))?;
        let cp = guard
            .as_mut()
            .ok_or_else(|| Error::Workspace("no active checkpoint".into()))?;
        let Some(index) = cp.changes.iter().position(|change| change.path == relative) else {
            return Ok(());
        };
        let change = &mut cp.changes[index];
        if change.state == ChangeState::Intent {
            if executed {
                let intended = change.intended_bytes().map(Cow::into_owned);
                if intended.is_none()
                    || actual.as_ref().map(|image| image.bytes().into_owned()) != intended
                {
                    return Err(Error::Workspace(
                        "current file does not match the recorded edit intent; inspect manually"
                            .into(),
                    ));
                }
                change.after = actual;
                change.state = ChangeState::Applied;
                change.set_intended(None);
            } else {
                if actual != change.after {
                    return Err(Error::Workspace(
                        "cannot declare not-run: file differs from the pre-execution snapshot"
                            .into(),
                    ));
                }
                if change.before == change.after {
                    cp.changes.remove(index);
                } else {
                    change.state = ChangeState::Applied;
                    change.set_intended(None);
                }
            }
        } else if !executed || change.state != ChangeState::Applied || actual != change.after {
            return Err(Error::Workspace(
                "checkpoint contradicts this resolution; inspect manually".into(),
            ));
        }
        self.persist(cp)
    }

    /// Whether an interrupted edit of `path` happened, as far as the checkpoint can
    /// tell, and settles its entry: `Some(true)` when the file holds the recorded
    /// intent, `Some(false)` when it still holds the earlier content, `None` without a
    /// recorded intent or when the file holds neither.
    pub fn settle_edit(&self, path: &Path) -> Option<bool> {
        let relative = path.strip_prefix(&self.root).unwrap_or(path);
        let intent = {
            let guard = self.current.lock().ok()?;
            guard
                .as_ref()?
                .changes
                .iter()
                .any(|change| change.path == relative && change.state == ChangeState::Intent)
        };
        if !intent {
            return None;
        }
        if self.resolve_edit(path, true).is_ok() {
            Some(true)
        } else if self.resolve_edit(path, false).is_ok() {
            Some(false)
        } else {
            None
        }
    }

    /// Identifies the current task's file-tool results; `None` when nothing changed.
    pub fn fingerprint(&self) -> Result<Option<u64>> {
        use std::hash::{Hash, Hasher};
        let guard = self
            .current
            .lock()
            .map_err(|_| Error::Workspace("checkpoint lock poisoned".into()))?;
        let Some(cp) = guard.as_ref() else {
            return Ok(None);
        };
        let mut hasher = std::collections::hash_map::DefaultHasher::new();
        let mut changed = false;
        for change in cp
            .changes
            .iter()
            .filter(|change| change.before != change.after)
        {
            changed = true;
            change.path.hash(&mut hasher);
            change
                .after
                .as_ref()
                .map(|image| image.bytes())
                .hash(&mut hasher);
        }
        Ok(changed.then(|| hasher.finish()))
    }

    /// Workspace-relative paths the current task's file tools changed.
    pub fn changed_paths(&self) -> Result<Vec<PathBuf>> {
        let guard = self
            .current
            .lock()
            .map_err(|_| Error::Workspace("checkpoint lock poisoned".into()))?;
        Ok(guard.as_ref().map_or_else(Vec::new, |cp| {
            cp.changes
                .iter()
                .filter(|change| change.before != change.after)
                .map(|change| change.path.clone())
                .collect()
        }))
    }

    pub fn note_shell(&self) -> Result<()> {
        let mut guard = self
            .current
            .lock()
            .map_err(|_| Error::Workspace("checkpoint lock poisoned".into()))?;
        let cp = guard
            .as_mut()
            .ok_or_else(|| Error::Workspace("no active checkpoint".into()))?;
        cp.shell_effects = true;
        self.persist(cp)
    }

    pub fn diff(&self) -> Result<String> {
        let guard = self
            .current
            .lock()
            .map_err(|_| Error::Workspace("checkpoint lock poisoned".into()))?;
        let Some(cp) = guard.as_ref() else {
            return Ok("本任务还没有检查点（文件工具未改动任何文件）。\n".into());
        };
        let mut result = String::from("文件工具记录的改动（不是完整的 Git/工作区 diff）：\n");
        for change in &cp.changes {
            let path = self.resolve(&change.path)?;
            let current = image(&path)?;
            // Bytes that are not text in any encoding we read (PNG output) get a summary line.
            let binary = |image: &Option<Image>| {
                image
                    .as_ref()
                    .is_some_and(|image| image.raw.is_some() && image.decoded().is_none())
            };
            if binary(&change.before) || binary(&current) {
                let size = |image: &Option<Image>| {
                    image.as_ref().map_or("absent".to_owned(), |image| {
                        format!("{} bytes", image.bytes().len())
                    })
                };
                result.push_str(&format!(
                    "Binary file {} changed: {} -> {}\n",
                    change.path.display(),
                    size(&change.before),
                    size(&current)
                ));
                continue;
            }
            let before = change
                .before
                .as_ref()
                .map_or(Cow::Borrowed(""), |image| image.display());
            let after = current
                .as_ref()
                .map_or(Cow::Borrowed(""), |image| image.display());
            result.push_str(
                &TextDiff::from_lines(before.as_ref(), after.as_ref())
                    .unified_diff()
                    .header(
                        &format!("before/{}", change.path.display()),
                        &format!("current/{}", change.path.display()),
                    )
                    .to_string(),
            );
            if result.len() > 256 * 1024 {
                result.truncate(result.floor_char_boundary(256 * 1024));
                result.push_str("\n[diff truncated]\n");
                break;
            }
        }
        if cp.changes.is_empty() {
            result.push_str("没有记录到改动。\n");
        }
        if cp.shell_effects {
            result.push_str("警告：Shell 命令的效果没有完整记录，也不会回滚。请另外检查 git diff 和未跟踪文件。\n");
        }
        Ok(result)
    }

    fn preflight(&self, cp: &Checkpoint) -> Result<()> {
        if cp.undone {
            return Err(Error::Workspace(
                "这个检查点已经撤销过了；/undo list 可查看更早的 Git 快照".into(),
            ));
        }
        for change in &cp.changes {
            if change.state == ChangeState::Intent {
                return Err(Error::Workspace(
                    "检查点里有结果不确定的编辑，请手动检查文件；/undo list 可改用 Git 快照恢复"
                        .into(),
                ));
            }
            if change.state == ChangeState::Applied
                && image(&self.resolve(&change.path)?)? != change.after
            {
                return Err(Error::Workspace(format!(
                    "拒绝回滚：{} 在 agent 编辑后又被改动过；/undo list 可改用 Git 快照恢复",
                    change.path.display()
                )));
            }
        }
        Ok(())
    }

    pub fn undo_preview(&self) -> Result<String> {
        let guard = self
            .current
            .lock()
            .map_err(|_| Error::Workspace("checkpoint lock poisoned".into()))?;
        let cp = guard
            .as_ref()
            .ok_or_else(|| Error::Workspace("本任务没有可撤销的文件工具改动".into()))?;
        self.preflight(cp)?;
        let mut output = format!("回滚检查点 {}：\n", cp.id);
        for change in &cp.changes {
            if change.state == ChangeState::Applied {
                output.push_str(&format!(
                    "  {} {}\n",
                    if change.before.is_some() {
                        "恢复"
                    } else {
                        "删除新建的文件"
                    },
                    change.path.display()
                ));
            }
        }
        if cp.shell_effects {
            output.push_str("警告：Shell 命令的效果不会恢复，这里只回滚文件工具的改动。\n");
        }
        output.push_str("输入 /undo confirm 执行回滚（执行前会再次核对当前文件内容）。\n");
        Ok(output)
    }

    pub fn root(&self) -> &Path {
        &self.root
    }

    /// Content of `relative` before the current task first changed it: `Ok(None)` when the
    /// task created it; an error when the checkpoint does not track it.
    pub fn baseline(&self, relative: &Path) -> Result<Option<String>> {
        let guard = self
            .current
            .lock()
            .map_err(|_| Error::Workspace("checkpoint lock poisoned".into()))?;
        let cp = guard
            .as_ref()
            .ok_or_else(|| Error::Workspace("no active checkpoint".into()))?;
        let change = cp
            .changes
            .iter()
            .find(|change| change.path == relative)
            .ok_or_else(|| {
                Error::Workspace(format!(
                    "{} was not changed by file tools in this task; no baseline",
                    relative.display()
                ))
            })?;
        Ok(change.before.as_ref().map(|image| image.text.clone()))
    }

    fn swap_path(&self) -> PathBuf {
        self.store.root().join("checkpoints").join(format!(
            "swap-{}.json",
            crate::agents::lifecycle::project_key(&self.root)
        ))
    }

    /// Files a crashed regression verification left holding their baseline, if any.
    pub fn pending_swap(&self) -> Option<Vec<PathBuf>> {
        let journal: SwapJournal = read_json(&self.swap_path()).ok()?;
        Some(journal.files.into_iter().map(|file| file.path).collect())
    }

    /// Temporarily replaces each `(relative path, baseline)` with its baseline. The current
    /// content is journaled first, so `swap_restore` can put it back even after a crash.
    pub fn swap_in(&self, files: Vec<(PathBuf, String)>) -> Result<()> {
        if self.swap_path().exists() {
            return Err(Error::Workspace(
                "an earlier regression swap was not restored; run /resolve or /status agents repair".into(),
            ));
        }
        let mut journal = SwapJournal {
            version: 1,
            root: self.root.clone(),
            files: Vec::new(),
        };
        for (path, baseline) in files {
            let current = image(&self.resolve(&path)?)?
                .ok_or_else(|| Error::Workspace(format!("{} does not exist", path.display())))?
                .text;
            journal.files.push(SwapFile {
                path,
                current,
                baseline,
            });
        }
        {
            let mut swapped = self
                .swapped
                .lock()
                .map_err(|_| Error::Workspace("swap lock poisoned".into()))?;
            swapped.extend(journal.files.iter().map(|file| file.path.clone()));
        }
        if let Err(err) = atomic_json(&self.swap_path(), &journal) {
            self.swapped
                .lock()
                .map_err(|_| Error::Workspace("swap lock poisoned".into()))?
                .clear();
            return Err(err);
        }
        for file in &journal.files {
            let path = self.resolve(&file.path)?;
            if let Err(err) = replace_file(&path, Some(&file.current), Some(&file.baseline)) {
                return match self.swap_restore() {
                    Ok(_) => Err(err),
                    Err(restore) => Err(Error::Uncertain(format!(
                        "baseline swap failed ({err}) and restore failed: {restore}"
                    ))),
                };
            }
        }
        Ok(())
    }

    /// Restores every journaled file whose content is still the baseline written by
    /// `swap_in`. Content changed by anyone else is left alone and reported as uncertain.
    pub fn swap_restore(&self) -> Result<String> {
        let path = self.swap_path();
        let journal: SwapJournal = match read_json(&path) {
            Ok(journal) => journal,
            Err(Error::Io(err)) if err.kind() == std::io::ErrorKind::NotFound => {
                return Ok("no regression swap pending".into());
            }
            Err(err) => return Err(err),
        };
        if journal.version != 1 || journal.root != self.root {
            return Err(Error::Workspace("swap journal identity mismatch".into()));
        }
        let mut drifted = Vec::new();
        let mut restored = 0;
        for file in &journal.files {
            let resolved = self.resolve(&file.path)?;
            let actual = image(&resolved)?.map(|image| image.text);
            if actual.as_deref() == Some(file.current.as_str()) {
                continue;
            }
            if actual.as_deref() == Some(file.baseline.as_str()) {
                replace_file(&resolved, Some(&file.baseline), Some(&file.current))?;
                restored += 1;
            } else {
                drifted.push(file.path.display().to_string());
            }
        }
        if !drifted.is_empty() {
            return Err(Error::Uncertain(format!(
                "regression swap: {} changed while it held its baseline; restore manually (journal kept at {})",
                drifted.join(", "),
                path.display()
            )));
        }
        std::fs::remove_file(&path)?;
        self.swapped
            .lock()
            .map_err(|_| Error::Workspace("swap lock poisoned".into()))?
            .clear();
        Ok(format!(
            "regression swap restored ({restored} of {} files needed it)",
            journal.files.len()
        ))
    }

    pub fn undo(&self) -> Result<()> {
        let mut guard = self
            .current
            .lock()
            .map_err(|_| Error::Workspace("checkpoint lock poisoned".into()))?;
        let cp = guard
            .as_mut()
            .ok_or_else(|| Error::Workspace("本任务没有可撤销的文件工具改动".into()))?;
        self.preflight(cp)?;
        for index in (0..cp.changes.len()).rev() {
            let change = &cp.changes[index];
            if change.state == ChangeState::Reverted {
                continue;
            }
            let path = self.resolve(&change.path)?;
            if image(&path)? != change.after {
                return Err(Error::Workspace(
                    "回滚过程中文件被改动；已完成的恢复都有记录，请检查后重试 /undo".into(),
                ));
            }
            replace_file_bytes(
                &path,
                change.after.as_ref().map(|image| image.bytes()).as_deref(),
                change.before.as_ref().map(|image| image.bytes()).as_deref(),
            )?;
            if let Some(before) = &change.before {
                let mut permissions = std::fs::metadata(&path)?.permissions();
                #[cfg(unix)]
                {
                    use std::os::unix::fs::PermissionsExt;
                    if let Some(mode) = before.mode {
                        permissions.set_mode(mode);
                    }
                }
                #[cfg(not(unix))]
                permissions.set_readonly(before.readonly);
                std::fs::set_permissions(&path, permissions)?;
            }
            cp.changes[index].state = ChangeState::Reverted;
            self.persist(cp)?;
        }
        cp.undone = true;
        self.persist(cp)
    }
}

/// Lines added and removed from `before` to `after`; binary content counts nothing.
fn line_changes(before: &[u8], after: &[u8]) -> (u64, u64) {
    let (Ok(before), Ok(after)) = (std::str::from_utf8(before), std::str::from_utf8(after)) else {
        return (0, 0);
    };
    let mut changes = (0, 0);
    for change in similar::TextDiff::from_lines(before, after).iter_all_changes() {
        match change.tag() {
            similar::ChangeTag::Insert => changes.0 += 1,
            similar::ChangeTag::Delete => changes.1 += 1,
            similar::ChangeTag::Equal => {}
        }
    }
    changes
}

pub fn replace_file(path: &Path, expected: Option<&str>, text: Option<&str>) -> Result<()> {
    replace_file_bytes(path, expected.map(str::as_bytes), text.map(str::as_bytes))
}

/// Atomic replacement of `path` (or removal for `None`) when it still holds `expected`.
pub fn replace_file_bytes(path: &Path, expected: Option<&[u8]>, text: Option<&[u8]>) -> Result<()> {
    if text.is_some_and(|text| !within_limit(text)) {
        return Err(Error::Workspace("new content exceeds 1 MiB".into()));
    }
    let current = image(path)?;
    if current.as_ref().map(|image| image.bytes()).as_deref() != expected {
        return Err(Error::Workspace("file changed before replacement".into()));
    }
    let Some(text) = text else {
        if current.is_some() {
            std::fs::remove_file(path)?;
        }
        return Ok(());
    };
    let mut temporary = tempfile::NamedTempFile::new_in(
        path.parent()
            .ok_or_else(|| Error::Workspace("missing parent".into()))?,
    )?;
    if current.is_some() {
        temporary
            .as_file()
            .set_permissions(std::fs::metadata(path)?.permissions())?;
    }
    temporary.write_all(text)?;
    temporary.as_file().sync_all()?;
    if image(path)? != current {
        return Err(Error::Workspace("file changed during replacement".into()));
    }
    if current.is_some() {
        crate::session::replace_with(temporary, path)?;
    } else {
        temporary
            .persist_noclobber(path)
            .map_err(|err| Error::Io(err.error))?;
    }
    Ok(())
}
