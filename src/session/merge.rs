//! Merges one file of an overlay layer into the main workspace (0053 §4). The levels, in
//! order: a direct write when only one side changed; a line-level three-way merge by
//! `git merge-file`; when that conflicts, anchor replay of the layer's `edit` calls on the
//! current content (the `edit` tool's own matching), kept only when the replayed change is
//! exactly the layer's change; a structural merge by `mergiraf` when it is installed;
//! otherwise a conflict with diff3 markers and the conflicting lines. Everything works on
//! bytes: line endings, BOM and the encoding of untouched regions are never rewritten.

use std::{
    path::{Path, PathBuf},
    sync::{Mutex, OnceLock},
    time::{Duration, Instant},
};

use similar::{Algorithm, DiffOp, capture_diff_slices_deadline};
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Result},
    process::{Argv, git::Git, run_argv},
    tools::{
        edit, line_endings,
        text_encoding::{self, TextKind},
    },
};

const GIT_TIMEOUT: Duration = Duration::from_secs(30);
const MERGIRAF_TIMEOUT: Duration = Duration::from_secs(30);
/// Conflict-marker labels; ASCII so the markers fit a file in any encoding.
const LABELS: [&str; 3] = ["current", "base", "subtask"];
/// Conflicting regions described one by one in `reason`; the rest are counted.
const DESCRIBED: usize = 5;

/// One recorded edit of a sub-task, as replayed against newer content.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct EditIntent {
    pub old: String,
    pub new: String,
    pub replace_all: bool,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Strategy {
    /// Only one side changed the file (or both made the same change).
    Direct,
    /// The layer's edits applied in order to the current content.
    Replayed,
    /// `git merge-file` merged the lines without conflicts.
    LineMerged,
    /// `mergiraf` merged the syntax trees without conflicts.
    Structured,
}

#[derive(Clone, Debug)]
pub struct Conflict {
    /// Human-readable why, in Chinese, for the merge report and the redo prompt.
    pub reason: String,
    /// Content with conflict markers (diff3 style), if a line merge produced it.
    pub marked: Option<Vec<u8>>,
    /// 1-based line ranges in `current` that conflict, when known.
    pub regions: Vec<(usize, usize)>,
}

#[derive(Clone, Debug)]
pub enum FileMerge {
    Merged {
        content: Vec<u8>,
        strategy: Strategy,
    },
    /// The merged result is that the file does not exist (both sides deleted it, or one
    /// side deleted it and the other left it as the base had it).
    Deleted {
        strategy: Strategy,
    },
    Conflict(Conflict),
}

/// `base`: what the sub-task started from; `current`: the main workspace now (may hold
/// earlier merged layers and the user's edits); `ours`: the sub-task's final content;
/// `intents`: its edits in order (empty if it wrote the file whole).
/// `None` content means the file does not exist on that side (deleted / not created).
pub async fn merge_file(
    path: &Path,
    base: Option<&[u8]>,
    current: Option<&[u8]>,
    ours: Option<&[u8]>,
    intents: &[EditIntent],
    cancel: &CancellationToken,
) -> Result<FileMerge> {
    merge_file_using(
        which::which("mergiraf").ok().as_deref(),
        path,
        base,
        current,
        ours,
        intents,
        cancel,
    )
    .await
}

/// [`merge_file`] with the mergiraf program chosen by the caller (`mergiraf_path`, or
/// the one on PATH); `None` skips the structural level.
pub async fn merge_file_using(
    mergiraf: Option<&Path>,
    path: &Path,
    base: Option<&[u8]>,
    current: Option<&[u8]>,
    ours: Option<&[u8]>,
    intents: &[EditIntent],
    cancel: &CancellationToken,
) -> Result<FileMerge> {
    let tools = Tools {
        git: true,
        mergiraf: mergiraf.map(Path::to_path_buf),
    };
    merge_with(&tools, path, base, current, ours, intents, cancel).await
}

/// The external mergers to try; tests switch them off.
struct Tools {
    git: bool,
    mergiraf: Option<PathBuf>,
}

fn take(content: Option<&[u8]>, strategy: Strategy) -> FileMerge {
    match content {
        Some(content) => FileMerge::Merged {
            content: content.to_vec(),
            strategy,
        },
        None => FileMerge::Deleted { strategy },
    }
}

fn conflict(reason: String) -> FileMerge {
    FileMerge::Conflict(Conflict {
        reason,
        marked: None,
        regions: Vec::new(),
    })
}

async fn merge_with(
    tools: &Tools,
    path: &Path,
    base: Option<&[u8]>,
    current: Option<&[u8]>,
    ours: Option<&[u8]>,
    intents: &[EditIntent],
    cancel: &CancellationToken,
) -> Result<FileMerge> {
    if cancel.is_cancelled() {
        return Err(Error::Cancelled);
    }
    if current == base {
        return Ok(take(ours, Strategy::Direct));
    }
    if ours == base || ours == current {
        return Ok(take(current, Strategy::Direct));
    }
    let shown = crate::paths::display(path);
    let (current, ours) = match (current, ours) {
        (Some(current), Some(ours)) => (current, ours),
        (None, _) => {
            return Ok(conflict(format!(
                "{shown}：主工作区里已删除，子任务却修改了它"
            )));
        }
        (_, None) => {
            return Ok(conflict(format!(
                "{shown}：子任务删除了它，主工作区里却已修改"
            )));
        }
    };
    let Some(base) = base else {
        return Ok(conflict(format!(
            "{shown}：主工作区和子任务都新建了这个文件，内容不同"
        )));
    };
    if [base, current, ours].iter().any(|bytes| binary(bytes)) {
        return Ok(conflict(format!(
            "{shown}：二进制文件，主工作区和子任务都改了它，不能逐行合并"
        )));
    }
    let mut notes = Vec::new();
    let mixed = mixed_encodings(&[base, current, ours]);
    let mut marked = None;
    let mut reported = None;
    if mixed {
        notes.push("三方的编码不一致（UTF-8 和 GBK 混用），不做逐行合并".into());
    } else if tools.git {
        match git_merge(base, current, ours, cancel).await {
            Ok((0, merged)) => {
                return Ok(FileMerge::Merged {
                    content: merged,
                    strategy: Strategy::LineMerged,
                });
            }
            Ok((count, merged)) => {
                reported = Some(count);
                marked = Some(merged);
            }
            Err(Error::Cancelled) => return Err(Error::Cancelled),
            Err(err) => notes.push(format!(
                "git merge-file 不可用，跳过行级合并（{}）",
                crate::text::short(&err.to_string(), 200)
            )),
        }
    } else {
        notes.push("没有 git，跳过行级合并".into());
    }
    // Anchor replay only where the line merge could not settle the file, and only when the
    // replayed change is exactly the layer's own change (see `replay_checked`).
    if !intents.is_empty() {
        match replay_checked(path, base, current, ours, intents) {
            Ok(merged) => {
                return Ok(FileMerge::Merged {
                    content: merged,
                    strategy: Strategy::Replayed,
                });
            }
            Err(note) => notes.push(note),
        }
    }
    if mixed {
        return Ok(conflict(format!("{shown}：{}", notes.join("；"))));
    }
    if let Some(program) = &tools.mergiraf
        && [base, current, ours]
            .iter()
            .all(|bytes| std::str::from_utf8(bytes).is_ok())
        && mergiraf_supports(program, path, cancel).await?
    {
        match mergiraf_merge(program, path, base, current, ours, cancel).await {
            Ok(Some(merged)) => {
                return Ok(FileMerge::Merged {
                    content: merged,
                    strategy: Strategy::Structured,
                });
            }
            Ok(None) => notes.push("mergiraf 结构化合并也有冲突".into()),
            Err(Error::Cancelled) => return Err(Error::Cancelled),
            Err(err) => notes.push(format!(
                "mergiraf 失败（{}）",
                crate::text::short(&err.to_string(), 200)
            )),
        }
    }
    let hunks = conflicting_hunks(base, current, ours);
    let regions = regions(&hunks);
    let mut reason = format!("{shown}：");
    if let Some(count) = reported {
        reason.push_str(&format!("行级合并有 {count} 处冲突"));
    } else {
        reason.push_str("主工作区和子任务都改了这个文件");
    }
    if !hunks.is_empty() {
        let described: Vec<String> = hunks
            .iter()
            .take(DESCRIBED)
            .map(|hunk| describe(hunk, current, ours))
            .collect();
        reason.push_str(&format!("：{}", described.join("；")));
        if hunks.len() > DESCRIBED {
            reason.push_str(&format!("；另有 {} 处", hunks.len() - DESCRIBED));
        }
    }
    if !notes.is_empty() {
        reason.push_str(&format!("。{}", notes.join("；")));
    }
    Ok(FileMerge::Conflict(Conflict {
        reason,
        marked,
        regions,
    }))
}

/// Text the file tools refuse to edit: NUL bytes or UTF-16.
fn binary(bytes: &[u8]) -> bool {
    matches!(
        text_encoding::sniff(bytes),
        TextKind::Binary | TextKind::Utf16
    )
}

/// One side is UTF-8 with non-ASCII text and another is not UTF-8 (GBK): merging their
/// lines would mix encodings in one file. Pure ASCII fits either.
fn mixed_encodings(sides: &[&[u8]]) -> bool {
    let utf8 = sides
        .iter()
        .any(|bytes| !bytes.is_ascii() && std::str::from_utf8(bytes).is_ok());
    let legacy = sides
        .iter()
        .any(|bytes| std::str::from_utf8(bytes).is_err());
    utf8 && legacy
}

/// Applies `intents` to `bytes` the way the `edit` tool does: decode (UTF-8, BOM kept, or
/// GBK), match in the LF view and give every line back its own ending, encode back.
/// `Err` says which edit failed and why.
fn replay(
    path: &Path,
    bytes: &[u8],
    intents: &[EditIntent],
) -> std::result::Result<Vec<u8>, String> {
    let shown = crate::paths::display(path);
    let decoded = text_encoding::decode_editable(bytes, &shown)?;
    let mut text = decoded.text;
    for (index, intent) in intents.iter().enumerate() {
        let step =
            |err: String| format!("第 {} 条编辑：{}", index + 1, crate::text::short(&err, 200));
        if intent.old.is_empty() {
            return Err(step("old is empty".into()));
        }
        let (old, new) = (
            line_endings::to_lf(&intent.old),
            line_endings::to_lf(&intent.new),
        );
        text = if old != new {
            let applied = edit::apply(&line_endings::to_lf(&text), &old, &new, intent.replace_all)
                .map_err(step)?;
            line_endings::restore(&text, &applied.result)
        } else if intent.old != intent.new {
            edit::apply(&text, &intent.old, &intent.new, intent.replace_all)
                .map_err(step)?
                .result
        } else {
            return Err(step("old and new are the same".into()));
        };
    }
    match decoded.legacy {
        Some(legacy) => legacy
            .encode(&text)
            .map_err(|err| format!("写回 {}：{err}", legacy.name())),
        None => Ok(text.into_bytes()),
    }
}

/// Replays `intents` on `current` and keeps the result only when its change to `current`
/// is exactly the layer's change to `base`, moved over: every changed block lies where
/// `current` still has the base's lines, and the blocks applied to `base` instead give
/// `ours`. An anchor that still matches after another layer made the same insertion (it
/// would be inserted twice), a `replace_all` that also hits text another layer added, and
/// a whitespace-insensitive match over someone else's whitespace change all fail here.
/// `Err` is the note for the conflict reason.
fn replay_checked(
    path: &Path,
    base: &[u8],
    current: &[u8],
    ours: &[u8],
    intents: &[EditIntent],
) -> std::result::Result<Vec<u8>, String> {
    match replay(path, base, intents) {
        Ok(replayed) if replayed == ours => {}
        Ok(_) => return Err("编辑记录重放到基线上得不到子任务的结果，未做锚点重放".into()),
        Err(err) => return Err(format!("编辑记录在基线上重放失败（{err}）")),
    }
    let merged = replay(path, current, intents).map_err(|err| format!("锚点重放失败（{err}）"))?;
    // Both ways round: the result is the layer's change on `current` and the change
    // already in `current` on the layer's result.
    match (
        transplant(base, current, &merged),
        transplant(base, ours, &merged),
    ) {
        (Some(rebuilt), Some(theirs)) if rebuilt == ours && theirs == current => Ok(merged),
        _ => Err(
            "锚点重放的结果和子任务自己的改动不一致（改到了主工作区已改过的行，或在同一处重复插入），不采用"
                .into(),
        ),
    }
}

fn diff_ops<'a>(old: &[&'a [u8]], new: &[&'a [u8]]) -> Vec<DiffOp> {
    let deadline = Instant::now() + Duration::from_millis(500);
    capture_diff_slices_deadline(Algorithm::Myers, old, new, Some(deadline))
}

/// The line changes from `current` to `merged`, applied to `base` instead. `None` when one
/// of them touches lines `current` changed against `base`, or inserts at a point where
/// `current` inserted or deleted lines (the order there is unknown).
fn transplant(base: &[u8], current: &[u8], merged: &[u8]) -> Option<Vec<u8>> {
    let (base, current, merged) = (lines(base), lines(current), lines(merged));
    // Base index of every unchanged current line; base point of every gap between lines.
    let mut line_at = vec![None; current.len()];
    let mut point_at: Vec<Option<usize>> = vec![None; current.len() + 1];
    let mut blocked = vec![false; current.len() + 1];
    for op in diff_ops(&base, &current) {
        match op {
            DiffOp::Equal {
                old_index,
                new_index,
                len,
            } => {
                for k in 0..len {
                    line_at[new_index + k] = Some(old_index + k);
                }
                for k in 0..=len {
                    point_at[new_index + k] = Some(old_index + k);
                }
            }
            DiffOp::Replace {
                old_index,
                old_len,
                new_index,
                new_len,
            } => {
                point_at[new_index].get_or_insert(old_index);
                point_at[new_index + new_len].get_or_insert(old_index + old_len);
            }
            DiffOp::Insert {
                new_index, new_len, ..
            } => blocked[new_index..=new_index + new_len].fill(true),
            DiffOp::Delete { new_index, .. } => blocked[new_index] = true,
        }
    }
    let mut rebuilt: Vec<&[u8]> = Vec::new();
    let mut next = 0;
    for op in diff_ops(&current, &merged) {
        let ((start, end), (from, to)) = match op {
            DiffOp::Equal { .. } => continue,
            DiffOp::Delete {
                old_index,
                old_len,
                new_index,
            } => ((old_index, old_index + old_len), (new_index, new_index)),
            DiffOp::Insert {
                old_index,
                new_index,
                new_len,
            } => ((old_index, old_index), (new_index, new_index + new_len)),
            DiffOp::Replace {
                old_index,
                old_len,
                new_index,
                new_len,
            } => (
                (old_index, old_index + old_len),
                (new_index, new_index + new_len),
            ),
        };
        let (base_start, base_end) = if start < end {
            let first = line_at[start]?;
            let last = line_at[end - 1]?;
            // Contiguous in the base too: nothing was inserted or deleted in between.
            if last.checked_sub(first)? != end - 1 - start {
                return None;
            }
            // A block that adds or removes lines next to where `current` inserted or
            // deleted lines: the order there is unknown (Myers may fold a repeat of the
            // other side's insertion into the change of the next line).
            if to - from != end - start && (blocked[start] || blocked[end]) {
                return None;
            }
            (first, last + 1)
        } else {
            if blocked[start] {
                return None;
            }
            let point = point_at[start]?;
            (point, point)
        };
        if base_start < next {
            return None;
        }
        rebuilt.extend_from_slice(&base[next..base_start]);
        rebuilt.extend_from_slice(&merged[from..to]);
        next = base_end;
    }
    rebuilt.extend_from_slice(&base[next..]);
    Some(rebuilt.concat())
}

/// `git merge-file -p --diff3 current base subtask` on temporary copies: the conflict
/// count (0 for a clean merge) and the merged bytes, markers included.
async fn git_merge(
    base: &[u8],
    current: &[u8],
    ours: &[u8],
    cancel: &CancellationToken,
) -> Result<(usize, Vec<u8>)> {
    let dir = tempfile::tempdir()?;
    for (name, bytes) in LABELS.iter().zip([current, base, ours]) {
        std::fs::write(dir.path().join(name), bytes)?;
    }
    let limit = base.len() + current.len() + ours.len() + 64 * 1024;
    let output = Git::new(dir.path())
        .args(["merge-file", "-p", "--diff3"])
        .args(LABELS.iter().flat_map(|label| ["-L", *label]))
        .args(LABELS)
        .timeout(GIT_TIMEOUT)
        .limit(limit)
        .raw(cancel)
        .await?;
    if output.timed_out {
        return Err(Error::Tool(format!("超时（{} 秒）", GIT_TIMEOUT.as_secs())));
    }
    if output.truncated {
        return Err(Error::Tool("输出超出上限".into()));
    }
    // The exit code is the number of conflicts (at most 127); negative is an error.
    match output.code {
        Some(code) if (0..=127).contains(&code) => Ok((code as usize, output.stdout)),
        code => Err(Error::Tool(format!(
            "退出码 {}：{}",
            code.map_or("无".into(), |code| code.to_string()),
            crate::text::short(&output.stderr_text(), 200)
        ))),
    }
}

/// File-name patterns mergiraf handles, read once from `mergiraf languages --gitattributes`
/// (lines such as `*.rs merge=mergiraf`).
static MERGIRAF_PATTERNS: OnceLock<Vec<String>> = OnceLock::new();
/// When listing the languages last failed; it is not retried before [`MERGIRAF_RETRY`].
static MERGIRAF_FAILED: Mutex<Option<Instant>> = Mutex::new(None);
const MERGIRAF_RETRY: Duration = Duration::from_secs(300);

fn parse_patterns(listing: &str) -> Vec<String> {
    listing
        .split_whitespace()
        .filter(|word| !word.contains('='))
        .map(str::to_owned)
        .collect()
}

/// `*.rs` matches by suffix, a plain name (`Makefile`) by the whole file name.
fn pattern_matches(patterns: &[String], path: &Path) -> bool {
    let Some(name) = path.file_name().map(|name| name.to_string_lossy()) else {
        return false;
    };
    patterns
        .iter()
        .any(|pattern| match pattern.strip_prefix('*') {
            Some(suffix) => !suffix.contains(['*', '?', '[']) && name.ends_with(suffix),
            None => *pattern == name,
        })
}

async fn mergiraf_supports(
    program: &Path,
    path: &Path,
    cancel: &CancellationToken,
) -> Result<bool> {
    if let Some(patterns) = MERGIRAF_PATTERNS.get() {
        return Ok(pattern_matches(patterns, path));
    }
    if MERGIRAF_FAILED
        .lock()
        .ok()
        .and_then(|failed| *failed)
        .is_some_and(|at| at.elapsed() < MERGIRAF_RETRY)
    {
        return Ok(false);
    }
    let argv = Argv::new(std::env::temp_dir(), program)
        .args(["languages", "--gitattributes"])
        .timeout(MERGIRAF_TIMEOUT)
        .direct();
    match run_argv(argv, cancel).await {
        Ok(output) if output.success() && !output.timed_out => Ok(pattern_matches(
            MERGIRAF_PATTERNS.get_or_init(|| parse_patterns(&output.stdout_text())),
            path,
        )),
        Err(Error::Cancelled) => Err(Error::Cancelled),
        // A mergiraf that cannot list its languages is not used for now; a failure (a
        // timeout on a busy machine) is not cached for good, it is tried again later.
        _ => {
            if let Ok(mut failed) = MERGIRAF_FAILED.lock() {
                *failed = Some(Instant::now());
            }
            Ok(false)
        }
    }
}

/// `mergiraf merge base current subtask -o out` on temporary copies named with the file's
/// extension (mergiraf picks the language from it). `Ok(None)`: it left conflicts.
async fn mergiraf_merge(
    program: &Path,
    path: &Path,
    base: &[u8],
    current: &[u8],
    ours: &[u8],
    cancel: &CancellationToken,
) -> Result<Option<Vec<u8>>> {
    let dir = tempfile::tempdir()?;
    let name = path
        .file_name()
        .map_or_else(|| "file".into(), |name| name.to_string_lossy().into_owned());
    let file = |label: &str| format!("{label}.{name}");
    for (label, bytes) in [("base", base), ("current", current), ("subtask", ours)] {
        std::fs::write(dir.path().join(file(label)), bytes)?;
    }
    let out = file("merged");
    let argv = Argv::new(dir.path(), program)
        .args([
            "merge".to_owned(),
            file("base"),
            file("current"),
            file("subtask"),
        ])
        .args(["-o".to_owned(), out.clone()])
        .timeout(MERGIRAF_TIMEOUT)
        // A merge tool cove runs itself, like git: trusted and short-lived.
        .direct();
    // Where the XDG base directories apply, mergiraf's review record goes into the
    // temporary directory, removed with it.
    let argv = if cfg!(any(windows, target_os = "macos")) {
        argv
    } else {
        argv.env("XDG_DATA_HOME", dir.path().join("data"))
    };
    let output = run_argv(argv, cancel).await?;
    forget_review(&format!(
        "{}\n{}",
        output.stdout_text(),
        output.stderr_text()
    ));
    if output.timed_out {
        return Err(Error::Tool(format!(
            "超时（{} 秒）",
            MERGIRAF_TIMEOUT.as_secs()
        )));
    }
    if !output.success() {
        return Ok(None);
    }
    let merged = match std::fs::read(dir.path().join(&out)) {
        Ok(merged) => merged,
        Err(_) => output.stdout,
    };
    let marked = merged
        .split(|byte| *byte == b'\n')
        .any(|line| line.starts_with(b"<<<<<<<") || line.starts_with(b">>>>>>>"));
    Ok((!marked && !merged.is_empty()).then_some(merged))
}

/// The id in mergiraf's `Review with: mergiraf review <id>` line: the rest of that line
/// (it is `base.<file name>_<random>`, so it may hold spaces and any letters), when it is a
/// single path component.
fn review_id(output: &str) -> Option<&str> {
    let (_, rest) = output.split_once("mergiraf review ")?;
    let id = rest
        .lines()
        .next()?
        .trim()
        .trim_matches(|c| matches!(c, '`' | '\'' | '"'))
        .trim();
    let plain = !id.is_empty()
        && id.len() <= 255
        && id != "."
        && id != ".."
        && !id.contains(['/', '\\', ':'])
        && !id.chars().any(char::is_control);
    plain.then_some(id)
}

/// Where mergiraf keeps the merges `mergiraf review` shows: `merges/` in its data
/// directory (the `directories` crate's project data dir of "Mergiraf contributors" /
/// "Mergiraf"). On Windows that is `%APPDATA%\Mergiraf contributors\Mergiraf\data\merges`
/// (seen with mergiraf 0.20.0); the macOS and Linux paths follow the crate's rules.
fn review_roots() -> Vec<PathBuf> {
    let env = |key: &str| std::env::var_os(key).map(PathBuf::from);
    let mut roots = Vec::new();
    if cfg!(windows) {
        if let Some(roaming) = env("APPDATA") {
            roots.push(
                roaming
                    .join("Mergiraf contributors")
                    .join("Mergiraf")
                    .join("data")
                    .join("merges"),
            );
        }
    } else if cfg!(target_os = "macos") {
        // The bundle-style directory name depends on the qualifier mergiraf passes:
        // take any Application Support directory named after it.
        if let Some(home) = env("HOME") {
            let support = home.join("Library").join("Application Support");
            for entry in std::fs::read_dir(&support).into_iter().flatten().flatten() {
                if entry
                    .file_name()
                    .to_string_lossy()
                    .to_lowercase()
                    .contains("mergiraf")
                {
                    roots.push(entry.path().join("merges"));
                }
            }
        }
    } else {
        if let Some(data) = env("XDG_DATA_HOME") {
            roots.push(data.join("mergiraf").join("merges"));
        }
        if let Some(home) = env("HOME") {
            roots.push(
                home.join(".local")
                    .join("share")
                    .join("mergiraf")
                    .join("merges"),
            );
        }
    }
    roots
}

/// Removes the review record of one mergiraf run, named in its output; cove's merges
/// are reported in the merge report, and the records would pile up in the user's cache.
fn forget_review(output: &str) -> Vec<PathBuf> {
    let Some(id) = review_id(output) else {
        return Vec::new();
    };
    let mut removed = Vec::new();
    for root in review_roots() {
        let record = root.join(id);
        let gone = match std::fs::symlink_metadata(&record) {
            Ok(meta) if meta.is_dir() => std::fs::remove_dir_all(&record).is_ok(),
            Ok(_) => std::fs::remove_file(&record).is_ok(),
            Err(_) => false,
        };
        if gone {
            removed.push(record);
        }
    }
    removed
}

/// A change of `current` and one of `ours` that touch the same base lines. Ranges are
/// 0-based, end-exclusive line indices.
#[derive(Debug, PartialEq, Eq)]
struct Hunk {
    base: (usize, usize),
    current: (usize, usize),
    ours: (usize, usize),
}

fn lines(bytes: &[u8]) -> Vec<&[u8]> {
    bytes.split_inclusive(|byte| *byte == b'\n').collect()
}

/// Changed blocks of `other` against `base`: (base range, other range).
fn changes(base: &[&[u8]], other: &[&[u8]]) -> Vec<((usize, usize), (usize, usize))> {
    diff_ops(base, other)
        .into_iter()
        .filter_map(|op| match op {
            DiffOp::Equal { .. } => None,
            DiffOp::Delete {
                old_index,
                old_len,
                new_index,
            } => Some(((old_index, old_index + old_len), (new_index, new_index))),
            DiffOp::Insert {
                old_index,
                new_index,
                new_len,
            } => Some(((old_index, old_index), (new_index, new_index + new_len))),
            DiffOp::Replace {
                old_index,
                old_len,
                new_index,
                new_len,
            } => Some((
                (old_index, old_index + old_len),
                (new_index, new_index + new_len),
            )),
        })
        .collect()
}

/// Changes of both sides that overlap or touch in the base, as git treats them.
fn conflicting_hunks(base: &[u8], current: &[u8], ours: &[u8]) -> Vec<Hunk> {
    let base = lines(base);
    let ours_changes = changes(&base, &lines(ours));
    let mut hunks = Vec::new();
    for (base_range, current_range) in changes(&base, &lines(current)) {
        for (theirs_base, ours_range) in &ours_changes {
            if base_range.0 <= theirs_base.1 && theirs_base.0 <= base_range.1 {
                hunks.push(Hunk {
                    base: base_range,
                    current: current_range,
                    ours: *ours_range,
                });
                break;
            }
        }
    }
    hunks
}

/// 1-based inclusive line ranges of the hunks in `current`; a deletion is the line at
/// which it happened.
fn regions(hunks: &[Hunk]) -> Vec<(usize, usize)> {
    let mut regions: Vec<(usize, usize)> = Vec::new();
    for hunk in hunks {
        let (start, end) = hunk.current;
        let region = if end > start {
            (start + 1, end)
        } else {
            (start.max(1), start.max(1))
        };
        match regions.last_mut() {
            Some(last) if region.0 <= last.1 + 1 => last.1 = last.1.max(region.1),
            _ => regions.push(region),
        }
    }
    regions
}

fn decode(bytes: &[u8]) -> String {
    match std::str::from_utf8(bytes) {
        Ok(text) => text.to_owned(),
        Err(_) => text_encoding::decode_gbk(bytes)
            .map(|(text, _)| text)
            .unwrap_or_else(|| String::from_utf8_lossy(bytes).into_owned()),
    }
}

/// "第 3-4 行：主工作区改为「…」，子任务改为「…」".
fn describe(hunk: &Hunk, current: &[u8], ours: &[u8]) -> String {
    let side = |bytes: &[u8], (start, end): (usize, usize)| -> String {
        if end == start {
            return "删除了这里".into();
        }
        let text: Vec<u8> = lines(bytes)[start..end].concat();
        format!("改为「{}」", crate::text::short(&decode(&text), 60))
    };
    let (start, end) = regions(std::slice::from_ref(hunk))[0];
    let lines_text = if end > start {
        format!("第 {start}-{end} 行")
    } else {
        format!("第 {start} 行")
    };
    let base_text = if hunk.base.1 > hunk.base.0 {
        format!("（基线第 {}-{} 行）", hunk.base.0 + 1, hunk.base.1)
    } else {
        String::new()
    };
    format!(
        "{lines_text}{base_text}主工作区{}，子任务{}",
        side(current, hunk.current),
        side(ours, hunk.ours)
    )
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;

    use super::*;

    const GIT_ONLY: Tools = Tools {
        git: true,
        mergiraf: None,
    };
    const NOTHING: Tools = Tools {
        git: false,
        mergiraf: None,
    };

    fn intent(old: &str, new: &str) -> EditIntent {
        EditIntent {
            old: old.into(),
            new: new.into(),
            replace_all: false,
        }
    }

    async fn run(
        tools: &Tools,
        base: Option<&[u8]>,
        current: Option<&[u8]>,
        ours: Option<&[u8]>,
        intents: &[EditIntent],
    ) -> FileMerge {
        merge_with(
            tools,
            Path::new("src/lib.rs"),
            base,
            current,
            ours,
            intents,
            &CancellationToken::new(),
        )
        .await
        .unwrap()
    }

    fn merged(result: FileMerge) -> (Vec<u8>, Strategy) {
        match result {
            FileMerge::Merged { content, strategy } => (content, strategy),
            other => panic!("expected a merge, got {other:?}"),
        }
    }

    fn conflicted(result: FileMerge) -> Conflict {
        match result {
            FileMerge::Conflict(conflict) => conflict,
            other => panic!("expected a conflict, got {other:?}"),
        }
    }

    const BASE: &str = "fn one() {\n    1\n}\n\nfn two() {\n    2\n}\n\nfn three() {\n    3\n}\n";

    /// Runs only where `mergiraf` is on PATH: two members added at the same place in a
    /// C# class conflict line by line, and mergiraf keeps both.
    #[tokio::test]
    async fn mergiraf_merges_members_added_at_the_same_place() {
        let Ok(mergiraf) = which::which("mergiraf") else {
            return;
        };
        let tools = Tools {
            git: true,
            mergiraf: Some(mergiraf),
        };
        let base = "public class Quote\n{\n    public decimal Bid { get; set; }\n\n    public decimal Mid() => (Bid + Ask) / 2;\n}\n";
        let current = base.replace(
            "/ 2;\n}",
            "/ 2;\n\n    public decimal Spread() => Ask - Bid;\n}",
        );
        let ours = base.replace(
            "/ 2;\n}",
            "/ 2;\n\n    public bool IsCrossed() => Bid > Ask;\n}",
        );
        let records = || -> std::collections::BTreeSet<PathBuf> {
            review_roots()
                .iter()
                .flat_map(|root| std::fs::read_dir(root).into_iter().flatten().flatten())
                .map(|entry| entry.path())
                .filter(|path| path.to_string_lossy().contains("Quote"))
                .collect()
        };
        let before = records();
        // The review record is named after the file: a name with spaces and Chinese too.
        for name in ["src/Quote.cs", "src/报价 Quote.cs"] {
            let result = merge_with(
                &tools,
                Path::new(name),
                Some(base.as_bytes()),
                Some(current.as_bytes()),
                Some(ours.as_bytes()),
                &[],
                &CancellationToken::new(),
            )
            .await
            .unwrap();
            let (content, strategy) = merged(result);
            let text = String::from_utf8(content).unwrap();
            assert_eq!(strategy, Strategy::Structured, "{text}");
            assert!(
                text.contains("Spread()") && text.contains("IsCrossed()"),
                "{text}"
            );
            assert!(!text.contains("<<<<<<<"), "{text}");
            // The review record mergiraf wrote is gone again.
            assert_eq!(records(), before, "{name}");
        }
    }

    #[tokio::test]
    async fn one_sided_changes_are_written_directly() {
        let base = BASE.as_bytes();
        let changed = BASE.replace("    1", "    10");
        let changed = changed.as_bytes();
        // Only the layer changed it.
        assert_eq!(
            merged(run(&NOTHING, Some(base), Some(base), Some(changed), &[]).await),
            (changed.to_vec(), Strategy::Direct)
        );
        // The layer did not change it: the current content stays.
        assert_eq!(
            merged(run(&NOTHING, Some(base), Some(changed), Some(base), &[]).await),
            (changed.to_vec(), Strategy::Direct)
        );
        // Both made the same change.
        assert_eq!(
            merged(run(&NOTHING, Some(base), Some(changed), Some(changed), &[]).await),
            (changed.to_vec(), Strategy::Direct)
        );
        // A new file only the layer created, and a deletion only the layer made.
        assert_eq!(
            merged(run(&NOTHING, None, None, Some(changed), &[]).await),
            (changed.to_vec(), Strategy::Direct)
        );
        assert!(matches!(
            run(&NOTHING, Some(base), Some(base), None, &[]).await,
            FileMerge::Deleted {
                strategy: Strategy::Direct
            }
        ));
    }

    #[tokio::test]
    async fn replay_applies_the_layers_edits_over_the_users_changes() {
        let base = BASE.as_bytes();
        let intents = [intent("    1\n}", "    10\n}")];
        let ours = BASE.replace("    1\n", "    10\n");
        let current = BASE.replace("    2\n", "    20\n");
        let (content, strategy) = merged(
            run(
                &NOTHING,
                Some(base),
                Some(current.as_bytes()),
                Some(ours.as_bytes()),
                &intents,
            )
            .await,
        );
        assert_eq!(strategy, Strategy::Replayed);
        assert_eq!(
            String::from_utf8(content).unwrap(),
            BASE.replace("    1\n", "    10\n")
                .replace("    2\n", "    20\n")
        );
    }

    const USES: &str = "use a;\n\nfn f() {\n    1\n}\n\nfn g() {\n    2\n}\n";

    /// Two parallel units both added `use x;` after `use a;` and changed one function each.
    /// The second layer's anchor `use a;` still matches once on the merged content; replaying
    /// it would add `use x;` a second time.
    #[tokio::test]
    async fn the_same_insertion_by_two_layers_is_not_replayed_twice() {
        let base = USES.as_bytes();
        let current = USES
            .replace("use a;\n", "use a;\nuse x;\n")
            .replace("    2\n", "    20\n");
        let ours = USES
            .replace("use a;\n", "use a;\nuse x;\n")
            .replace("    1\n", "    10\n");
        let intents = [
            intent("use a;", "use a;\nuse x;"),
            intent("    1\n", "    10\n"),
        ];
        let (content, strategy) = merged(
            run(
                &GIT_ONLY,
                Some(base),
                Some(current.as_bytes()),
                Some(ours.as_bytes()),
                &intents,
            )
            .await,
        );
        assert_eq!(strategy, Strategy::LineMerged);
        let text = String::from_utf8(content).unwrap();
        assert_eq!(text, current.replace("    1\n", "    10\n"));
        assert_eq!(text.matches("use x;").count(), 1, "{text}");
        // Without the line merge, the replay is checked and refused.
        let conflict = conflicted(
            run(
                &NOTHING,
                Some(base),
                Some(current.as_bytes()),
                Some(ours.as_bytes()),
                &intents,
            )
            .await,
        );
        assert!(
            conflict.reason.contains("锚点重放的结果"),
            "{}",
            conflict.reason
        );
    }

    /// `replace_all` would also rename the text another layer added next to the layer's
    /// change.
    #[tokio::test]
    async fn replace_all_does_not_reach_text_another_layer_added() {
        let base = "foo(1);\nfoo(2);\nend\n";
        let ours = "bar(1);\nbar(2);\nend\n";
        // Another layer added a line right below, so the lines touch and git conflicts.
        let current = "foo(1);\nfoo(2);\nfoo(3);\nend\n";
        let intents = [EditIntent {
            old: "foo(".into(),
            new: "bar(".into(),
            replace_all: true,
        }];
        let conflict = conflicted(
            run(
                &GIT_ONLY,
                Some(base.as_bytes()),
                Some(current.as_bytes()),
                Some(ours.as_bytes()),
                &intents,
            )
            .await,
        );
        assert!(
            conflict.reason.contains("锚点重放的结果"),
            "{}",
            conflict.reason
        );
    }

    /// A whitespace-insensitive match over a line the other side only re-indented would
    /// throw that change away.
    #[tokio::test]
    async fn a_fuzzy_match_does_not_cover_the_other_sides_whitespace_change() {
        let base = "fn f() {\n  let x = 1;\n  let y = 2;\n}\n";
        let ours = "fn f() {\n  let x = 10;\n  let y = 2;\n}\n";
        let current = "fn f() {\n\tlet x = 1;\n  let y = 2;\n}\n";
        let intents = [intent("{\n  let x = 1;", "{\n  let x = 10;")];
        for tools in [&GIT_ONLY, &NOTHING] {
            let result = run(
                tools,
                Some(base.as_bytes()),
                Some(current.as_bytes()),
                Some(ours.as_bytes()),
                &intents,
            )
            .await;
            assert!(matches!(result, FileMerge::Conflict(_)), "{result:?}");
        }
    }

    /// The repeated insertion folded by the diff into the change of the next line.
    #[tokio::test]
    async fn a_repeated_insertion_next_to_a_change_is_not_replayed() {
        let base = "use a;\nuse b;\n";
        let current = "use a;\nuse x;\nuse b;\n";
        let ours = "use a;\nuse x;\nuse bb;\n";
        let intents = [
            intent("use a;", "use a;\nuse x;"),
            intent("use b;", "use bb;"),
        ];
        for tools in [&GIT_ONLY, &NOTHING] {
            let result = run(
                tools,
                Some(base.as_bytes()),
                Some(current.as_bytes()),
                Some(ours.as_bytes()),
                &intents,
            )
            .await;
            if let FileMerge::Merged { content, .. } = &result {
                let text = String::from_utf8_lossy(content);
                assert_eq!(text.matches("use x;").count(), 1, "{text}");
            }
        }
        let result = merge_file_using(
            None,
            Path::new("src/lib.rs"),
            Some(base.as_bytes()),
            Some(current.as_bytes()),
            Some(ours.as_bytes()),
            &intents,
            &CancellationToken::new(),
        )
        .await
        .unwrap();
        assert!(matches!(result, FileMerge::Conflict(_)), "{result:?}");
    }

    /// Every combination of keep / modify / delete / insert-after (the same new line on
    /// both sides) on three lines: a replay the check accepts never repeats a line and
    /// keeps every change of both sides.
    #[tokio::test]
    async fn accepted_replays_never_repeat_or_lose_a_change() {
        #[derive(Clone, Copy, PartialEq)]
        enum Op {
            Keep,
            Modify,
            Delete,
            Insert,
        }
        const OPS: [Op; 4] = [Op::Keep, Op::Modify, Op::Delete, Op::Insert];
        const LINES: usize = 3;
        let apply = |ops: &[Op], tag: &str| -> String {
            let mut text = String::new();
            for (i, op) in ops.iter().enumerate() {
                match op {
                    Op::Keep => text.push_str(&format!("l{i}\n")),
                    Op::Modify => text.push_str(&format!("{tag}{i}\n")),
                    Op::Delete => {}
                    Op::Insert => text.push_str(&format!("l{i}\nn{i}\n")),
                }
            }
            text
        };
        let combos: Vec<Vec<Op>> = (0..OPS.len().pow(LINES as u32))
            .map(|mut n| {
                (0..LINES)
                    .map(|_| {
                        let op = OPS[n % OPS.len()];
                        n /= OPS.len();
                        op
                    })
                    .collect()
            })
            .collect();
        let base = apply(&[Op::Keep; LINES], "");
        let mut accepted = 0;
        for theirs in &combos {
            let current = apply(theirs, "c");
            for mine in &combos {
                let ours = apply(mine, "m");
                let intents: Vec<EditIntent> = mine
                    .iter()
                    .enumerate()
                    .filter_map(|(i, op)| {
                        let old = format!("l{i}\n");
                        let new = match op {
                            Op::Keep => return None,
                            Op::Modify => format!("m{i}\n"),
                            Op::Delete => String::new(),
                            Op::Insert => format!("l{i}\nn{i}\n"),
                        };
                        Some(EditIntent {
                            old,
                            new,
                            replace_all: false,
                        })
                    })
                    .collect();
                let Ok(merged) = replay_checked(
                    Path::new("a.txt"),
                    base.as_bytes(),
                    current.as_bytes(),
                    ours.as_bytes(),
                    &intents,
                ) else {
                    continue;
                };
                accepted += 1;
                let text = String::from_utf8(merged).unwrap();
                let lines: Vec<&str> = text.lines().collect();
                let unique: BTreeSet<&str> = lines.iter().copied().collect();
                let case = format!("current {current:?} ours {ours:?} -> {text:?}");
                assert_eq!(unique.len(), lines.len(), "a line repeated: {case}");
                for i in 0..LINES {
                    let has = |line: String| unique.contains(line.as_str());
                    let (t, m) = (theirs[i], mine[i]);
                    assert_eq!(has(format!("c{i}")), t == Op::Modify, "{case}");
                    assert_eq!(has(format!("m{i}")), m == Op::Modify, "{case}");
                    assert_eq!(
                        has(format!("n{i}")),
                        t == Op::Insert || m == Op::Insert,
                        "{case}"
                    );
                    let kept = !matches!(t, Op::Modify | Op::Delete)
                        && !matches!(m, Op::Modify | Op::Delete);
                    assert_eq!(has(format!("l{i}")), kept, "{case}");
                }
            }
        }
        // The check is not so strict that nothing passes: at most 729 of the 4096 have
        // every anchor left in `current`; 405 pass when this was written.
        assert!(accepted > 300, "{accepted}");
    }

    /// Adjacent lines changed on both sides: git conflicts, the checked replay merges them.
    #[tokio::test]
    async fn replay_settles_adjacent_changes_the_line_merge_conflicts_on() {
        let base = "a\nb\nc\nd\n";
        let current = "a\nB\nc\nd\n";
        let ours = "a\nb\nC\nd\n";
        let (content, strategy) = merged(
            run(
                &GIT_ONLY,
                Some(base.as_bytes()),
                Some(current.as_bytes()),
                Some(ours.as_bytes()),
                &[intent("c\n", "C\n")],
            )
            .await,
        );
        assert_eq!(strategy, Strategy::Replayed);
        assert_eq!(String::from_utf8(content).unwrap(), "a\nB\nC\nd\n");
    }

    #[test]
    fn transplant_moves_changes_onto_the_base_and_refuses_shared_points() {
        let t = |base: &str, current: &str, merged: &str| {
            transplant(base.as_bytes(), current.as_bytes(), merged.as_bytes())
                .map(|bytes| String::from_utf8(bytes).unwrap())
        };
        // A change in lines current kept as in the base.
        assert_eq!(
            t("a\nb\nc\n", "A\nb\nc\n", "A\nb\nC\n").as_deref(),
            Some("a\nb\nC\n")
        );
        // An insertion where current inserted too.
        assert_eq!(t("a\nc\n", "a\nb\nc\n", "a\nb\nb\nc\n"), None);
        // A change of a line current changed.
        assert_eq!(t("a\nb\n", "a\nB\n", "a\nX\n"), None);
        // A change spanning a line current deleted.
        assert_eq!(t("a\nb\nc\n", "a\nc\n", "x\ny\n"), None);
    }

    #[tokio::test]
    async fn an_ambiguous_anchor_falls_back_to_the_line_merge() {
        let base = BASE.as_bytes();
        let intents = [intent("    1\n}", "    10\n}")];
        let ours = BASE.replace("    1\n", "    10\n");
        // The user appended a copy of function one: the anchor is no longer unique.
        let current = format!("{BASE}\nfn one_again() {{\n    1\n}}\n");
        let (content, strategy) = merged(
            run(
                &GIT_ONLY,
                Some(base),
                Some(current.as_bytes()),
                Some(ours.as_bytes()),
                &intents,
            )
            .await,
        );
        assert_eq!(strategy, Strategy::LineMerged);
        assert_eq!(
            String::from_utf8(content).unwrap(),
            format!("{}\nfn one_again() {{\n    1\n}}\n", ours)
        );
    }

    #[tokio::test]
    async fn intents_that_do_not_rebuild_the_layer_are_not_replayed() {
        let base = BASE.as_bytes();
        // The layer also rewrote function three outside the recorded edit.
        let ours = BASE
            .replace("    1\n", "    10\n")
            .replace("    3\n", "    30\n");
        let current = BASE.replace("    2\n", "    20\n");
        let (content, strategy) = merged(
            run(
                &GIT_ONLY,
                Some(base),
                Some(current.as_bytes()),
                Some(ours.as_bytes()),
                &[intent("    1\n}", "    10\n}")],
            )
            .await,
        );
        assert_eq!(strategy, Strategy::LineMerged);
        assert_eq!(
            String::from_utf8(content).unwrap(),
            current
                .replace("    1\n", "    10\n")
                .replace("    3\n", "    30\n")
        );
    }

    #[tokio::test]
    async fn the_same_line_changed_on_both_sides_is_a_conflict_with_markers() {
        let base = BASE.as_bytes();
        let ours = BASE.replace("    2\n", "    22\n");
        let current = BASE.replace("    2\n", "    200\n");
        let conflict = conflicted(
            run(
                &GIT_ONLY,
                Some(base),
                Some(current.as_bytes()),
                Some(ours.as_bytes()),
                &[intent("    2\n", "    22\n")],
            )
            .await,
        );
        assert_eq!(conflict.regions, vec![(6, 6)]);
        assert!(conflict.reason.contains("1 处冲突"), "{}", conflict.reason);
        assert!(conflict.reason.contains("第 6 行"), "{}", conflict.reason);
        assert!(conflict.reason.contains("「200」"), "{}", conflict.reason);
        assert!(conflict.reason.contains("「22」"), "{}", conflict.reason);
        assert!(
            conflict.reason.contains("锚点重放失败"),
            "{}",
            conflict.reason
        );
        let marked = String::from_utf8(conflict.marked.unwrap()).unwrap();
        assert!(
            marked.contains(
                "<<<<<<< current\n    200\n||||||| base\n    2\n=======\n    22\n>>>>>>> subtask\n"
            ),
            "{marked}"
        );
    }

    #[tokio::test]
    async fn crlf_lines_keep_their_endings_in_replay_and_line_merge() {
        let base = BASE.replace('\n', "\r\n");
        let ours = base.replace("    1\r\n", "    10\r\n");
        let current = base.replace("    3\r\n", "    30\r\n");
        let expected = base
            .replace("    1\r\n", "    10\r\n")
            .replace("    3\r\n", "    30\r\n");
        // The model's edit is in LF, as `read` shows the file.
        let replayed = merged(
            run(
                &NOTHING,
                Some(base.as_bytes()),
                Some(current.as_bytes()),
                Some(ours.as_bytes()),
                &[intent("    1\n}", "    10\n}")],
            )
            .await,
        );
        assert_eq!(
            replayed,
            (expected.clone().into_bytes(), Strategy::Replayed)
        );
        let line_merged = merged(
            run(
                &GIT_ONLY,
                Some(base.as_bytes()),
                Some(current.as_bytes()),
                Some(ours.as_bytes()),
                &[],
            )
            .await,
        );
        assert_eq!(line_merged, (expected.into_bytes(), Strategy::LineMerged));
    }

    #[tokio::test]
    async fn a_utf8_bom_survives_replay_and_line_merge() {
        let base = format!("\u{feff}{BASE}");
        let ours = base.replace("    1\n", "    10\n");
        let current = base.replace("    3\n", "    30\n");
        let expected = ours.replace("    3\n", "    30\n").into_bytes();
        for (tools, intents) in [
            (&NOTHING, vec![intent("    1\n", "    10\n")]),
            (&GIT_ONLY, Vec::new()),
        ] {
            let (content, _) = merged(
                run(
                    tools,
                    Some(base.as_bytes()),
                    Some(current.as_bytes()),
                    Some(ours.as_bytes()),
                    &intents,
                )
                .await,
            );
            assert_eq!(content, expected);
        }
    }

    #[tokio::test]
    async fn gbk_files_stay_gbk() {
        let gbk = |text: &str| encoding_rs::GBK.encode(text).0.into_owned();
        let base_text = "第一行中文\n第二行\n第三行\n第四行\n第五行中文\n";
        let base = gbk(base_text);
        let ours = gbk(&base_text.replace("第一行中文", "第一行改了"));
        let current = gbk(&base_text.replace("第五行中文", "第五行也改了"));
        let expected = gbk(&base_text
            .replace("第一行中文", "第一行改了")
            .replace("第五行中文", "第五行也改了"));
        assert!(std::str::from_utf8(&base).is_err());
        let replayed = merged(
            run(
                &NOTHING,
                Some(&base),
                Some(&current),
                Some(&ours),
                &[intent("第一行中文", "第一行改了")],
            )
            .await,
        );
        assert_eq!(replayed, (expected.clone(), Strategy::Replayed));
        let line_merged =
            merged(run(&GIT_ONLY, Some(&base), Some(&current), Some(&ours), &[]).await);
        assert_eq!(line_merged, (expected, Strategy::LineMerged));
        // Both changed line 3: the reason shows the text decoded, not mojibake.
        let ours = gbk(&base_text.replace("第三行", "子任务的第三行"));
        let current = gbk(&base_text.replace("第三行", "用户的第三行"));
        let conflict =
            conflicted(run(&GIT_ONLY, Some(&base), Some(&current), Some(&ours), &[]).await);
        assert!(
            conflict.reason.contains("「用户的第三行」"),
            "{}",
            conflict.reason
        );
        assert!(
            conflict.reason.contains("「子任务的第三行」"),
            "{}",
            conflict.reason
        );
        assert_eq!(conflict.regions, vec![(3, 3)]);
    }

    #[tokio::test]
    async fn mixed_encodings_are_not_merged_line_by_line() {
        let base = "a\nb\nc\nd\n".as_bytes();
        let ours = "中文\nb\nc\nd\n".as_bytes();
        let current = encoding_rs::GBK.encode("a\nb\nc\n中文\n").0.into_owned();
        let conflict =
            conflicted(run(&GIT_ONLY, Some(base), Some(&current), Some(ours), &[]).await);
        assert!(
            conflict.reason.contains("编码不一致"),
            "{}",
            conflict.reason
        );
        assert!(conflict.marked.is_none());
    }

    #[tokio::test]
    async fn deletions_and_creations_conflict_unless_one_side_kept_the_base() {
        let base = BASE.as_bytes();
        let changed = BASE.replace("    1", "    10");
        let changed = changed.as_bytes();
        let other = BASE.replace("    2", "    20");
        let other = other.as_bytes();
        let deleted_here = conflicted(run(&GIT_ONLY, Some(base), Some(changed), None, &[]).await);
        assert!(
            deleted_here.reason.contains("子任务删除了它"),
            "{}",
            deleted_here.reason
        );
        let deleted_there = conflicted(run(&GIT_ONLY, Some(base), None, Some(changed), &[]).await);
        assert!(
            deleted_there.reason.contains("主工作区里已删除"),
            "{}",
            deleted_there.reason
        );
        assert!(matches!(
            run(&GIT_ONLY, Some(base), None, None, &[]).await,
            FileMerge::Deleted { .. }
        ));
        // Deleted in the main workspace, left alone by the layer: stays deleted.
        assert!(matches!(
            run(&GIT_ONLY, Some(base), None, Some(base), &[]).await,
            FileMerge::Deleted { .. }
        ));
        let created = conflicted(run(&GIT_ONLY, None, Some(changed), Some(other), &[]).await);
        assert!(created.reason.contains("都新建了"), "{}", created.reason);
        assert_eq!(
            merged(run(&GIT_ONLY, None, Some(changed), Some(changed), &[]).await),
            (changed.to_vec(), Strategy::Direct)
        );
    }

    #[tokio::test]
    async fn binary_files_merge_only_directly() {
        let base = b"\x00\x01binary\n".as_slice();
        let current = b"\x00\x02binary\n".as_slice();
        let ours = b"\x00\x01binary\nmore\n".as_slice();
        assert_eq!(
            merged(run(&GIT_ONLY, Some(base), Some(base), Some(ours), &[]).await),
            (ours.to_vec(), Strategy::Direct)
        );
        let conflict = conflicted(run(&GIT_ONLY, Some(base), Some(current), Some(ours), &[]).await);
        assert!(conflict.reason.contains("二进制"), "{}", conflict.reason);
        assert!(conflict.marked.is_none());
    }

    #[tokio::test]
    async fn without_git_replay_still_works_and_the_rest_is_a_conflict() {
        let base = BASE.as_bytes();
        let ours = BASE.replace("    1\n", "    10\n");
        let current = BASE.replace("    3\n", "    30\n");
        let conflict = conflicted(
            run(
                &NOTHING,
                Some(base),
                Some(current.as_bytes()),
                Some(ours.as_bytes()),
                &[],
            )
            .await,
        );
        assert!(conflict.reason.contains("没有 git"), "{}", conflict.reason);
        assert!(conflict.marked.is_none());
        // The two changes are apart, so no region conflicts.
        assert!(conflict.regions.is_empty(), "{:?}", conflict.regions);
        let (_, strategy) = merged(
            run(
                &NOTHING,
                Some(base),
                Some(current.as_bytes()),
                Some(ours.as_bytes()),
                &[intent("    1\n", "    10\n")],
            )
            .await,
        );
        assert_eq!(strategy, Strategy::Replayed);
    }

    #[tokio::test]
    async fn the_public_entry_merges_without_mergiraf_installed() {
        let base = BASE.as_bytes();
        let ours = BASE.replace("    1\n", "    10\n");
        let current = BASE.replace("    3\n", "    30\n");
        let result = merge_file(
            Path::new("notes.txt"),
            Some(base),
            Some(current.as_bytes()),
            Some(ours.as_bytes()),
            &[],
            &CancellationToken::new(),
        )
        .await
        .unwrap();
        assert!(matches!(
            result,
            FileMerge::Merged {
                strategy: Strategy::LineMerged | Strategy::Structured,
                ..
            }
        ));
    }

    #[tokio::test]
    async fn cancellation_stops_the_merge() {
        let cancel = CancellationToken::new();
        cancel.cancel();
        let result = merge_with(
            &GIT_ONLY,
            Path::new("a.rs"),
            Some(b"a\n"),
            Some(b"b\n"),
            Some(b"c\n"),
            &[],
            &cancel,
        )
        .await;
        assert!(matches!(result, Err(Error::Cancelled)));
    }

    #[test]
    fn mergiraf_languages_are_matched_by_file_name() {
        let patterns =
            parse_patterns("*.rs merge=mergiraf\n*.py merge=mergiraf\nMakefile merge=mergiraf\n");
        assert_eq!(patterns, vec!["*.rs", "*.py", "Makefile"]);
        assert!(pattern_matches(&patterns, Path::new("src/main.rs")));
        assert!(pattern_matches(&patterns, Path::new("build/Makefile")));
        assert!(!pattern_matches(&patterns, Path::new("notes.txt")));
        assert!(!pattern_matches(&patterns, Path::new("src/main.rsx")));
    }

    #[test]
    fn regions_follow_the_current_side_and_merge_adjacent_hunks() {
        let base = b"a\nb\nc\nd\ne\nf\n";
        let current = b"a\nB\nC\nd\ne\nf\n";
        let ours = b"a\nb2\nc\nd\ne\nF\n";
        let hunks = conflicting_hunks(base, current, ours);
        assert_eq!(regions(&hunks), vec![(2, 3)]);
        // A deletion on the current side is reported at the line where it happened.
        let current = b"a\nd\ne\nf\n";
        let hunks = conflicting_hunks(base, current, ours);
        assert_eq!(regions(&hunks), vec![(1, 1)]);
        assert!(describe(&hunks[0], current, ours).contains("主工作区删除了这里"));
    }

    #[test]
    fn the_review_record_id_is_read_from_mergiraf_output() {
        let line = "INFO Solved 1 conflict. Review with: mergiraf review lib.rs_8f3a1c2e\n";
        assert_eq!(review_id(line), Some("lib.rs_8f3a1c2e"));
        assert_eq!(
            review_id("Review with: `mergiraf review a-b_c`"),
            Some("a-b_c")
        );
        // Anything that could name another directory is ignored.
        for bad in [
            "mergiraf review ../x",
            "mergiraf review a/b",
            "mergiraf review ..",
        ] {
            assert_eq!(review_id(bad), None, "{bad}");
        }
        // The record is named after the file, which may hold spaces and Chinese.
        assert_eq!(
            review_id(
                "INFO Mergiraf: Solved 1 conflict. Review with: mergiraf review base.中 文.cs_3i5IRyPc\nnext"
            ),
            Some("base.中 文.cs_3i5IRyPc")
        );
        assert_eq!(review_id("mergiraf review C:x"), None);
        assert!(forget_review("no record here").is_empty());
        assert!(forget_review("mergiraf review cove-test-record-that-does-not-exist").is_empty());
    }
}
