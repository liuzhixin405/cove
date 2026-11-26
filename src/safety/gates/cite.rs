//! Citation check for answers that change no files (0027): the `path:line` references of
//! the final reply are verified against the workspace without calling the model (the file
//! exists, the line is in range, the symbols named next to it are near that line), and the
//! model is asked once to correct what does not hold. Repository symbols that cannot be
//! found anywhere, and code described without reading any, are handed back the same way.

use std::{
    collections::{HashMap, HashSet},
    ops::Range,
    path::{Path, PathBuf},
    sync::{Arc, LazyLock, Mutex},
};

use futures_util::future::BoxFuture;
use regex::Regex;
use serde::{Deserialize, Serialize};
use serde_json::Value;

use super::{Evidence, last_reply};
use crate::{
    domain::{Block, Origin, Progress, Role},
    engine::turn::TurnHook,
    safety::permission::{classify, shell::Dialect},
    session::{Session, workspace::Workspace},
    tools::ToolOutput,
};

/// Replies shorter than this (code blocks removed) are not checked.
const MIN_CHARS: usize = 200;
const MAX_CITATIONS: usize = 30;
/// Backtick words searched in the workspace per reply.
const MAX_ANCHOR_WORDS: usize = 12;
const MAX_FILES: usize = 20_000;
const MAX_FILE_BYTES: u64 = 1 << 20;
const MAX_SCAN_BYTES: u64 = 64 << 20;
/// A symbol this close to the cited line confirms it.
const NEAR: usize = 5;
/// Within the enclosing method or type.
const ENCLOSING: usize = 30;
const MAX_OFFSET_NOTES: usize = 3;
const MAX_CONTEXT_BYTES: usize = 4 << 20;
/// Tools that count as reading code this task.
const READ_TOOLS: &[&str] = &["read", "grep", "glob", "repo_map", "agent", "execute_plan"];

// ---------------------------------------------------------------- parsing

static PATH: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(
        r"(?:\\\\\?\\)?(?:[A-Za-z]:[\\/]|\./|/)?(?:[A-Za-z0-9_.@+$\-]+[\\/])*[A-Za-z0-9_@+$\-][A-Za-z0-9_.@+$\-]*\.[A-Za-z][A-Za-z0-9_+\-]*",
    )
    .expect("path pattern")
});
static COLON: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^`?:([0-9]+)(?:[-–—~]([0-9]+))?(?::[0-9]+)?").unwrap());
static GITHUB: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^#L([0-9]+)(?:-L?([0-9]+))?").unwrap());
static ZH_AFTER: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(
        r"^`?[ \t\x{3000}]*(?:中的|里的|的|中|里)?[ \t\x{3000}]*[（(]?[ \t\x{3000}]*第[ \t\x{3000}]*([0-9]+)[ \t\x{3000}]*(?:[-–—~～至到][ \t\x{3000}]*([0-9]+)[ \t\x{3000}]*)?行",
    )
    .unwrap()
});
static EN_AFTER: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(
        r"^`?,?[ \t]*[（(]?[ \t]*(?:(?i:lines?)[ \t]*|L)([0-9]+)(?:[ \t]*(?:[-–—~]|to)[ \t]*L?([0-9]+))?",
    )
    .unwrap()
});
static ZH_LINE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(
        r"第[ \t\x{3000}]*([0-9]+)[ \t\x{3000}]*(?:[-–—~～至到][ \t\x{3000}]*([0-9]+)[ \t\x{3000}]*)?行",
    )
    .unwrap()
});
static EN_LINE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"(?i)\blines?[ \t]+([0-9]+)(?:[ \t]*(?:[-–—~]|to)[ \t]*([0-9]+))?").unwrap()
});
static L_LINE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"\bL([0-9]+)(?:-L?([0-9]+))?\b").unwrap());
static OF_PATH: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^(?i)[ \t]+(?:of|in)[ \t]+`?").unwrap());
static URL: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r#"[A-Za-z][A-Za-z0-9+.\-]*://[^\s)>\]`'"]*|(?:^|[\s(])//[^\s)>\]`'"]+"#).unwrap()
});
static LINK_TEXT: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"\[([^\]\n]*)\]\([^)\s]+\)").unwrap());
static BACKTICK: LazyLock<Regex> = LazyLock::new(|| Regex::new(r"`([^`\n]+)`").unwrap());
static TOKEN: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"[A-Za-z_][A-Za-z0-9_]*|[0-9]+").unwrap());
static SYMBOL: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"^[A-Za-z_$][A-Za-z0-9_$]*(?:(?:\.|::|->|#)[A-Za-z_$][A-Za-z0-9_$]*)*(?:\(\))?$")
        .unwrap()
});

/// One reference to a place in a file.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Citation {
    /// The path as written, with `/` separators and without a `\\?\` prefix.
    pub path: String,
    pub start: usize,
    pub end: usize,
    /// Byte range of the reference in the prose text.
    pub span: Range<usize>,
    /// A bare line number ("第 30 行") bound to the nearest file mentioned before it.
    pub bound: bool,
}

impl Citation {
    pub fn label(&self) -> String {
        let path = crate::text::clip_chars(&self.path, 80);
        if self.start == self.end {
            format!("{path}:{}", self.start)
        } else {
            format!("{path}:{}-{}", self.start, self.end)
        }
    }
}

fn blank(text: &str) -> String {
    text.chars()
        .map(|c| match c {
            '\n' => "\n".to_owned(),
            c => " ".repeat(c.len_utf8()),
        })
        .collect()
}

fn blank_range(text: &mut String, range: Range<usize>) {
    let spaces = blank(&text[range.clone()]);
    text.replace_range(range, &spaces);
}

/// The reply with code blocks and URLs blanked out, byte offsets unchanged: a `a.rs:12`
/// in a code block is usually compiler output, and `host:port` in a URL no reference.
pub fn prose(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut fenced = false;
    for line in text.split_inclusive('\n') {
        let trimmed = line.trim_start();
        let fence = trimmed.starts_with("```") || trimmed.starts_with("~~~");
        if fenced || fence {
            out.push_str(&blank(line));
        } else {
            out.push_str(line);
        }
        if fence {
            fenced = !fenced;
        }
    }
    let urls: Vec<Range<usize>> = URL.find_iter(&out).map(|m| m.range()).collect();
    for range in urls {
        blank_range(&mut out, range);
    }
    out
}

fn normalize(path: &str) -> String {
    let path = path.replace('\\', "/");
    path.strip_prefix("//?/").unwrap_or(&path).to_owned()
}

fn extension(path: &str) -> Option<String> {
    let name = path.rsplit('/').next()?;
    let (_, ext) = name.rsplit_once('.')?;
    (!ext.is_empty()).then(|| ext.to_ascii_lowercase())
}

fn line_number(text: &str) -> Option<usize> {
    if text.len() > 6 {
        return None;
    }
    text.parse().ok().filter(|n| *n > 0)
}

/// Start and end lines from the captured numbers; a second number that does not follow
/// on directly ("a.rs:12-3") is ignored.
fn lines_of(caps: &regex::Captures, rest: &str) -> Option<(usize, usize, usize)> {
    let whole = caps.get(0)?;
    if rest[whole.end()..].starts_with(|c: char| c.is_ascii_digit()) {
        return None;
    }
    let start = line_number(caps.get(1)?.as_str())?;
    let end = caps
        .get(2)
        .and_then(|m| line_number(m.as_str()))
        .filter(|end| *end >= start)
        .unwrap_or(start);
    Some((start, end, whole.end()))
}

/// The line reference written right after a path: `:12`, `#L12`, ` 第 12 行`, ` line 12`.
fn suffix(rest: &str) -> Option<(usize, usize, usize)> {
    [&COLON, &GITHUB, &ZH_AFTER, &EN_AFTER]
        .iter()
        .find_map(|re| re.captures(rest).and_then(|caps| lines_of(&caps, rest)))
}

/// Sections for binding bare line numbers: headings when the reply has any, else
/// paragraphs.
struct Scopes {
    marks: Vec<usize>,
}

impl Scopes {
    fn new(prose: &str) -> Self {
        let mut lines = Vec::new();
        let mut offset = 0;
        for line in prose.split_inclusive('\n') {
            lines.push((offset, line));
            offset += line.len();
        }
        let heading = |line: &str| {
            let hashes = line.len() - line.trim_start_matches('#').len();
            (1..=6).contains(&hashes) && line[hashes..].starts_with([' ', '\t'])
        };
        let headings: Vec<usize> = lines
            .iter()
            .filter(|(_, line)| heading(line.trim_start()))
            .map(|(offset, _)| *offset)
            .collect();
        let marks = if headings.is_empty() {
            lines
                .iter()
                .filter(|(_, line)| line.trim().is_empty())
                .map(|(offset, _)| *offset)
                .collect()
        } else {
            headings
        };
        Self { marks }
    }

    fn same(&self, before: usize, after: usize) -> bool {
        !self
            .marks
            .iter()
            .any(|mark| before < *mark && *mark <= after)
    }
}

fn overlaps(a: &Range<usize>, b: &Range<usize>) -> bool {
    a.start < b.end && b.start < a.end
}

/// The references in `prose` (see [`prose`]). `exts` are the file extensions of the
/// workspace: a bare line number binds only to a mention of such a file.
pub fn parse(prose: &str, exts: &HashSet<String>) -> Vec<Citation> {
    let mut out = Vec::new();
    let mut taken: Vec<Range<usize>> = LINK_TEXT
        .captures_iter(prose)
        .filter_map(|caps| caps.get(1).map(|m| m.range()))
        .collect();
    let mut mentions: Vec<(Range<usize>, String)> = Vec::new();
    for found in PATH.find_iter(prose) {
        let path = normalize(found.as_str());
        let rest = &prose[found.end()..];
        if let Some((start, end, len)) = suffix(rest) {
            let span = found.start()..found.end() + len;
            taken.push(span.clone());
            out.push(Citation {
                path: path.clone(),
                start,
                end,
                span,
                bound: false,
            });
        }
        if extension(&path).is_some_and(|ext| exts.contains(&ext)) {
            mentions.push((found.range(), path));
        }
    }
    let scopes = Scopes::new(prose);
    let mut bare = Vec::new();
    for re in [&ZH_LINE, &EN_LINE, &L_LINE] {
        for caps in re.captures_iter(prose) {
            let Some(whole) = caps.get(0) else { continue };
            if taken.iter().any(|range| overlaps(range, &whole.range())) {
                continue;
            }
            let Some((start, end, _)) = lines_of(&caps, prose) else {
                continue;
            };
            let after = &prose[whole.end()..];
            // "line 12 of a.rs" names its file after the number.
            let named = OF_PATH.find(after).and_then(|of| {
                PATH.find(&after[of.end()..])
                    .filter(|path| path.start() == 0)
                    .map(|path| normalize(path.as_str()))
            });
            let path = named.or_else(|| {
                mentions
                    .iter()
                    .rev()
                    .find(|(range, _)| {
                        range.end <= whole.start() && scopes.same(range.start, whole.start())
                    })
                    .map(|(_, path)| path.clone())
            });
            if let Some(path) = path {
                taken.push(whole.range());
                bare.push(Citation {
                    path,
                    start,
                    end,
                    span: whole.range(),
                    bound: true,
                });
            }
        }
    }
    out.extend(bare);
    out.sort_by_key(|citation| citation.span.start);
    let mut seen = HashSet::new();
    out.retain(|citation| {
        let name = citation
            .path
            .rsplit('/')
            .next()
            .unwrap_or_default()
            .to_lowercase();
        seen.insert((name, citation.start, citation.end))
    });
    out.truncate(MAX_CITATIONS);
    out
}

// ---------------------------------------------------------------- words

/// Lower-case words of identifiers: `parseHeader`, `parse_header` and `ParseHeader` all
/// give `parse header`; `__`, `.` and `::` separate like any other punctuation.
pub fn words(text: &str) -> Vec<String> {
    let chars: Vec<char> = text.chars().collect();
    let mut out = Vec::new();
    let mut current = String::new();
    for (index, c) in chars.iter().copied().enumerate() {
        if !c.is_ascii_alphanumeric() {
            if !current.is_empty() {
                out.push(std::mem::take(&mut current));
            }
            continue;
        }
        if !current.is_empty() {
            let prev = chars[index - 1];
            let next = chars.get(index + 1).copied();
            let boundary = c.is_ascii_uppercase()
                && (prev.is_ascii_lowercase()
                    || prev.is_ascii_digit()
                    || (prev.is_ascii_uppercase() && next.is_some_and(|n| n.is_ascii_lowercase())));
            if boundary {
                out.push(std::mem::take(&mut current));
            }
        }
        current.push(c.to_ascii_lowercase());
    }
    if !current.is_empty() {
        out.push(current);
    }
    out
}

/// ` w1 w2 ` for whole-word searching with `contains`.
fn joined(text: &str) -> String {
    let words = words(text);
    if words.is_empty() {
        return " ".into();
    }
    format!(" {} ", words.join(" "))
}

/// The search needles of a symbol written in backticks: each identifier and number in it
/// (`book.bids[:DEPTH]` gives book, bids and depth). Any one of them confirms a place.
pub fn needles(anchor: &str) -> Vec<String> {
    let mut out = Vec::new();
    for token in TOKEN.find_iter(anchor) {
        for part in token.as_str().split("__") {
            let needle = joined(part);
            if needle.trim().len() >= 2 && !out.contains(&needle) {
                out.push(needle);
            }
        }
    }
    out
}

// ---------------------------------------------------------------- workspace files

/// One file's lines as [`joined`] words.
pub struct FileText {
    lines: Vec<String>,
}

struct Entry {
    rel: String,
    key: String,
}

/// The workspace's files (ignore rules applied), listed once per task.
pub struct Files {
    root: PathBuf,
    entries: Vec<Entry>,
    dirs: Vec<String>,
    pub exts: HashSet<String>,
    texts: Mutex<HashMap<String, Option<Arc<FileText>>>>,
}

fn read_text(path: &Path) -> Option<String> {
    use crate::tools::text_encoding as encoding;
    if std::fs::metadata(path).ok()?.len() > MAX_FILE_BYTES {
        return None;
    }
    let bytes = std::fs::read(path).ok()?;
    let text = match encoding::sniff(&bytes) {
        encoding::TextKind::Utf8 => String::from_utf8_lossy(&bytes).into_owned(),
        encoding::TextKind::Legacy => encoding::decode_gbk(&bytes)?.0,
        encoding::TextKind::Binary | encoding::TextKind::Utf16 => return None,
    };
    Some(
        text.strip_prefix(encoding::UTF8_BOM)
            .map(str::to_owned)
            .unwrap_or(text),
    )
}

fn in_order(wanted: &[&str], dirs: &[&str]) -> bool {
    let mut dirs = dirs.iter();
    wanted
        .iter()
        .all(|segment| dirs.any(|dir| dir.contains(segment)))
}

impl Files {
    pub fn scan(root: &Path) -> Self {
        let mut entries = Vec::new();
        let mut dirs = HashSet::new();
        let mut exts = HashSet::new();
        for entry in crate::tools::search::walk(root).flatten() {
            if entries.len() >= MAX_FILES {
                break;
            }
            if !entry.file_type().is_some_and(|kind| kind.is_file()) {
                continue;
            }
            let Ok(relative) = entry.path().strip_prefix(root) else {
                continue;
            };
            let rel = relative.to_string_lossy().replace('\\', "/");
            let key = crate::paths::key(Path::new(&rel));
            if let Some(ext) = extension(&rel) {
                exts.insert(ext);
            }
            let mut parent = key.as_str();
            while let Some((dir, _)) = parent.rsplit_once('/') {
                dirs.insert(dir.to_owned());
                parent = dir;
            }
            entries.push(Entry { rel, key });
        }
        Self {
            root: root.to_path_buf(),
            entries,
            dirs: dirs.into_iter().collect(),
            exts,
            texts: Mutex::default(),
        }
    }

    /// The comparable workspace-relative form of a cited path; `None` for an absolute
    /// path outside the workspace.
    fn relative_key(&self, cited: &str) -> Option<String> {
        let text = normalize(cited);
        let bytes = text.as_bytes();
        let absolute = text.starts_with('/')
            || (bytes.len() > 2
                && bytes[0].is_ascii_alphabetic()
                && bytes[1] == b':'
                && bytes[2] == b'/');
        let key = crate::paths::key(Path::new(&text));
        let key = if absolute {
            let root = crate::paths::key(&self.root);
            key.strip_prefix(&root)?.strip_prefix('/')?.to_owned()
        } else {
            key
        };
        let mut key = key.as_str();
        while let Some(rest) = key.strip_prefix("./") {
            key = rest;
        }
        Some(key.to_owned())
    }

    /// Workspace files a cited path can mean: the exact path, else files with the same
    /// name whose directories contain the cited ones in order (`Distributor/Program.cs`
    /// is `src/QuoteHub.Distributor/Program.cs`), else the name alone.
    pub fn resolve(&self, cited: &str) -> Vec<String> {
        let Some(key) = self.relative_key(cited) else {
            return Vec::new();
        };
        if let Some(entry) = self.entries.iter().find(|entry| entry.key == key) {
            return vec![entry.rel.clone()];
        }
        let segments: Vec<&str> = key
            .split('/')
            .filter(|s| !s.is_empty() && *s != "." && *s != "..")
            .collect();
        let Some((name, wanted)) = segments.split_last() else {
            return Vec::new();
        };
        let found: Vec<String> = self
            .entries
            .iter()
            .filter(|entry| {
                let mut parts: Vec<&str> = entry.key.split('/').collect();
                parts.pop() == Some(*name) && in_order(wanted, &parts)
            })
            .map(|entry| entry.rel.clone())
            .collect();
        if found.is_empty() && !key.split('/').any(|s| s == "..") && self.root.join(&key).is_file()
        {
            // Ignored (build output, dependencies) but there.
            return vec![key];
        }
        found
    }

    /// Whether a cited directory is in the workspace.
    pub fn resolve_dir(&self, cited: &str) -> bool {
        let Some(key) = self.relative_key(cited) else {
            return false;
        };
        let key = key.trim_end_matches('/');
        if key.is_empty() {
            return false;
        }
        if self.dirs.iter().any(|dir| dir == key) {
            return true;
        }
        let segments: Vec<&str> = key.split('/').filter(|s| !s.is_empty()).collect();
        let Some((name, wanted)) = segments.split_last() else {
            return false;
        };
        self.dirs.iter().any(|dir| {
            let mut parts: Vec<&str> = dir.split('/').collect();
            parts.pop() == Some(*name) && in_order(wanted, &parts)
        })
    }

    fn text(&self, rel: &str) -> Option<Arc<FileText>> {
        if let Some(cached) = self.texts.lock().ok()?.get(rel) {
            return cached.clone();
        }
        let text = read_text(&self.root.join(rel)).map(|text| {
            Arc::new(FileText {
                lines: text.lines().map(joined).collect(),
            })
        });
        if let Ok(mut texts) = self.texts.lock() {
            texts.insert(rel.to_owned(), text.clone());
        }
        text
    }

    /// For each query (a list of needles), whether any needle occurs in any file.
    fn search(&self, queries: &[Vec<String>]) -> Vec<bool> {
        let mut found: Vec<bool> = queries.iter().map(|q| q.is_empty()).collect();
        let mut budget = MAX_SCAN_BYTES;
        for entry in &self.entries {
            if found.iter().all(|f| *f) {
                break;
            }
            let path = self.root.join(&entry.rel);
            let Some(size) = std::fs::metadata(&path).ok().map(|m| m.len()) else {
                continue;
            };
            if size > MAX_FILE_BYTES || size > budget {
                continue;
            }
            budget -= size;
            let Some(text) = read_text(&path) else {
                continue;
            };
            let text = joined(&text);
            for (query, hit) in queries.iter().zip(found.iter_mut()) {
                if !*hit && query.iter().any(|needle| text.contains(needle.as_str())) {
                    *hit = true;
                }
            }
        }
        found
    }
}

// ---------------------------------------------------------------- verifying

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Verdict {
    /// The file and line exist and the named symbol is near the line.
    Passed,
    /// The named symbol is elsewhere in the file.
    Offset {
        anchor: String,
        line: usize,
    },
    Missing,
    OutOfRange {
        lines: usize,
    },
    /// The named symbol is nowhere in the file.
    Mismatch {
        anchor: String,
    },
    /// Several files match the path and none confirms the reference.
    Ambiguous,
    /// Not checkable (outside the workspace, unreadable, an unknown kind of file).
    Unverified,
}

impl Verdict {
    pub fn failed(&self) -> bool {
        matches!(
            self,
            Self::Missing | Self::OutOfRange { .. } | Self::Mismatch { .. }
        )
    }
}

/// A symbol written in backticks next to a reference, with its search needles.
#[derive(Clone, Debug)]
pub struct Anchor {
    pub text: String,
    pub needles: Vec<String>,
}

impl Anchor {
    pub fn new(text: &str) -> Self {
        Self {
            text: text.to_owned(),
            needles: needles(text),
        }
    }
}

fn locate(text: &FileText, anchors: &[Anchor], start: usize, end: usize) -> Verdict {
    let count = text.lines.len();
    if end > count {
        return Verdict::OutOfRange { lines: count };
    }
    let anchors: Vec<&Anchor> = anchors.iter().filter(|a| !a.needles.is_empty()).collect();
    if anchors.is_empty() {
        return Verdict::Passed;
    }
    let hit = |line: usize| {
        let text = &text.lines[line - 1];
        anchors
            .iter()
            .any(|anchor| anchor.needles.iter().any(|n| text.contains(n.as_str())))
    };
    let window = |reach: usize| {
        let low = start.saturating_sub(reach).max(1);
        let high = (end + reach).min(count);
        (low..=high).any(hit)
    };
    if window(NEAR) || window(ENCLOSING) {
        return Verdict::Passed;
    }
    let nearest = (1..=count)
        .filter(|line| hit(*line))
        .min_by_key(|line| line.abs_diff(start));
    match nearest {
        Some(line) => Verdict::Offset {
            anchor: anchors[0].text.clone(),
            line,
        },
        None => Verdict::Mismatch {
            anchor: anchors[0].text.clone(),
        },
    }
}

/// Checks one reference. `seen` is lower-case text of the tool calls and results in
/// context: a file that is not in the workspace but was read there (a dependency, another
/// checkout) is not reported missing.
pub fn verify(files: &Files, citation: &Citation, anchors: &[Anchor], seen: &str) -> Verdict {
    let candidates = files.resolve(&citation.path);
    if candidates.is_empty() {
        let name = citation
            .path
            .rsplit('/')
            .next()
            .unwrap_or_default()
            .to_lowercase();
        let known = extension(&citation.path).is_some_and(|ext| files.exts.contains(&ext));
        let outside = files.relative_key(&citation.path).is_none();
        return if citation.bound || !known || outside || seen.contains(&name) {
            Verdict::Unverified
        } else {
            Verdict::Missing
        };
    }
    let verdicts: Vec<Verdict> = candidates
        .iter()
        .map(|rel| match files.text(rel) {
            Some(text) => locate(&text, anchors, citation.start, citation.end),
            None => Verdict::Unverified,
        })
        .collect();
    if verdicts.contains(&Verdict::Passed) {
        return Verdict::Passed;
    }
    match verdicts.as_slice() {
        [only] => only.clone(),
        _ => Verdict::Ambiguous,
    }
}

// ---------------------------------------------------------------- the reply

/// Text positions that end a clause (anchors are taken from the citation's clause) or
/// a sentence (symbols are tied to repository paths in the same sentence).
/// Each mark is the boundary character's byte range: `。` and `；` are three bytes long.
fn boundaries(prose: &str, spans: &[Range<usize>], clause: bool) -> Vec<Range<usize>> {
    let mut out = Vec::new();
    let mut chars = prose.char_indices().peekable();
    while let Some((index, c)) = chars.next() {
        if spans.iter().any(|span| span.contains(&index)) {
            continue;
        }
        let next = chars.peek().map(|(_, c)| *c);
        let end = match c {
            '\n' | '。' | '！' | '？' | '!' | '?' | '；' | ';' => true,
            '.' => next.is_none_or(char::is_whitespace),
            '，' | ',' | '|' | '…' | '、' => clause,
            _ => false,
        };
        if end {
            out.push(index..index + c.len_utf8());
        }
    }
    out
}

/// The text between the boundary marks around `span`; both ends are char boundaries.
fn enclosing(marks: &[Range<usize>], span: &Range<usize>) -> Range<usize> {
    let start = marks
        .iter()
        .rev()
        .find(|mark| mark.start < span.start)
        .map_or(0, |mark| mark.end);
    let end = marks
        .iter()
        .find(|mark| mark.start >= span.end)
        .map_or(usize::MAX, |mark| mark.start);
    start..end
}

const NEGATIONS: &[&str] = &[
    "没有",
    "没",
    "不存在",
    "找不到",
    "搜不到",
    "并非",
    "不是",
    "不叫",
    "不在",
    "未",
    "not ",
    "no ",
    "n't",
    "never",
    "none",
    "without",
];

/// `range` of the prose says something is not there: a negation outside the code spans
/// (`NoneType` in backticks does not count as "none").
fn negated(prose: &str, range: &Range<usize>, spans: &[Range<usize>]) -> bool {
    let end = range.end.min(prose.len());
    let mut said = String::new();
    let mut at = range.start;
    let mut inside: Vec<&Range<usize>> = spans
        .iter()
        .filter(|span| span.start < end && span.end > range.start)
        .collect();
    inside.sort_by_key(|span| span.start);
    for span in inside {
        if span.start > at {
            said.push_str(&prose[at..span.start]);
        }
        said.push(' ');
        at = at.max(span.end);
    }
    if at < end {
        said.push_str(&prose[at..end]);
    }
    let said = said.to_lowercase();
    NEGATIONS.iter().any(|negation| said.contains(negation))
}

/// What the check is given besides the workspace.
pub struct Input<'a> {
    pub reply: &'a str,
    /// Lower-case text of the tool calls and results in context.
    pub tools: &'a str,
    /// [`joined`] words of the tool results and the user's messages in context.
    pub context: &'a str,
    /// A read-class tool ran this task.
    pub read: bool,
}

#[derive(Debug, Default)]
pub struct Analysis {
    /// The reply talks about this repository's code, so it was checked.
    pub applies: bool,
    pub results: Vec<(Citation, Verdict)>,
    /// Code symbols tied to a repository path that occur nowhere in the workspace.
    pub missing_symbols: Vec<String>,
    /// Repository code described without reading any of it.
    pub unread: bool,
}

impl Analysis {
    /// Something the model should correct (line offsets alone are only reported).
    /// Wrong references and symbols the repository does not have. `unread` is recorded
    /// but never nudges: replaying the real sessions it was nearly always a correct reply
    /// that needed no reading (an MCP code tool, the project outline, a generic example).
    pub fn needs_fix(&self) -> bool {
        !self.missing_symbols.is_empty() || self.results.iter().any(|(_, verdict)| verdict.failed())
    }

    pub fn stats(&self, nudged: bool) -> CitationStats {
        let count = |f: fn(&Verdict) -> bool| self.results.iter().filter(|(_, v)| f(v)).count();
        CitationStats {
            checked: self.results.len(),
            passed: count(|v| *v == Verdict::Passed),
            offset: count(|v| matches!(v, Verdict::Offset { .. })),
            failed: count(Verdict::failed),
            missing_symbols: self.missing_symbols.len(),
            unread: self.unread,
            nudged,
        }
    }
}

fn path_like(text: &str, exts: &HashSet<String>) -> bool {
    text.contains('/')
        || text.contains('\\')
        || PATH
            .find(text)
            .is_some_and(|m| extension(m.as_str()).is_some_and(|ext| exts.contains(&ext)))
}

/// Looks like a name in code rather than a word: `Decoder`, `parse_header`, `a.b`, `f()`.
fn symbol_like(text: &str) -> bool {
    text.len() >= 3
        && SYMBOL.is_match(text)
        && (text.chars().any(|c| c.is_ascii_uppercase())
            || text.contains('_')
            || text.contains('.')
            || text.contains("::")
            || text.ends_with("()"))
}

/// The last name of a qualified symbol: `QuoteHub.Wire.Decoder` → `Decoder`.
fn last_name(text: &str) -> &str {
    let text = text.trim_end_matches("()");
    text.rsplit(['.', ':', '#', '>'])
        .find(|s| !s.is_empty())
        .unwrap_or(text)
}

pub fn analyze(files: &Files, input: &Input) -> Analysis {
    let prose = prose(input.reply);
    if prose.trim().chars().count() < MIN_CHARS {
        return Analysis::default();
    }
    let citations = parse(&prose, &files.exts);
    let spans: Vec<(Range<usize>, String)> = BACKTICK
        .captures_iter(&prose)
        .filter_map(|caps| {
            let whole = caps.get(0)?;
            Some((whole.range(), caps.get(1)?.as_str().trim().to_owned()))
        })
        .collect();
    let ranges: Vec<Range<usize>> = spans.iter().map(|(range, _)| range.clone()).collect();

    // Repository anchors: backtick words that name something in this workspace.
    let mut words_seen = HashSet::new();
    let mut paths = Vec::new();
    let mut symbols = Vec::new();
    for (range, text) in &spans {
        if words_seen.len() >= MAX_ANCHOR_WORDS || !words_seen.insert(text.clone()) {
            continue;
        }
        if path_like(text, &files.exts) {
            let path = PATH
                .find(text)
                .filter(|m| extension(m.as_str()).is_some_and(|ext| files.exts.contains(&ext)))
                .map_or(text.as_str(), |m| m.as_str());
            let found = !files.resolve(path).is_empty() || files.resolve_dir(text);
            paths.push((range.clone(), text.clone(), found));
        } else if !needles(text).is_empty() {
            symbols.push((range.clone(), text.clone()));
        }
    }
    let mut queries: Vec<Vec<String>> = symbols.iter().map(|(_, text)| needles(text)).collect();
    queries.extend(symbols.iter().map(|(_, text)| needles(last_name(text))));
    let hits = files.search(&queries);
    let (any_hit, last_hit) = hits.split_at(symbols.len());
    let found_anchors = paths.iter().filter(|(_, _, found)| *found).count()
        + any_hit.iter().filter(|hit| **hit).count();
    let resolved: Vec<&Citation> = citations
        .iter()
        .filter(|citation| !files.resolve(&citation.path).is_empty())
        .collect();
    if resolved.is_empty() && found_anchors < 2 {
        return Analysis::default();
    }

    let clauses = boundaries(&prose, &ranges, true);
    let results = citations
        .iter()
        .map(|citation| {
            let clause = enclosing(&clauses, &citation.span);
            // "`a.cs:3` does not set `X`": only the file and its lines are checked.
            if negated(&prose, &clause, &ranges) {
                let verdict = verify(files, citation, &[], input.tools);
                return (citation.clone(), verdict);
            }
            let mut near: Vec<&(Range<usize>, String)> = spans
                .iter()
                .filter(|(range, text)| {
                    range.start >= clause.start
                        && range.end <= clause.end
                        // Another line reference (`:117`) or a number is not a symbol.
                        && !citations.iter().any(|other| overlaps(range, &other.span))
                        && text.chars().any(char::is_alphabetic)
                        && !path_like(text, &files.exts)
                })
                .collect();
            near.sort_by_key(|(range, _)| {
                if range.end <= citation.span.start {
                    citation.span.start - range.end
                } else {
                    range.start.saturating_sub(citation.span.end)
                }
            });
            let anchors: Vec<Anchor> = near
                .iter()
                .take(2)
                .map(|(_, text)| Anchor::new(text))
                .collect();
            let verdict = verify(files, citation, &anchors, input.tools);
            (citation.clone(), verdict)
        })
        .collect();

    // Code symbols said to be in a repository path that exist nowhere in it.
    let sentences = boundaries(&prose, &ranges, false);
    let mut mentions: Vec<Range<usize>> = paths
        .iter()
        .filter(|(_, _, found)| *found)
        .map(|(range, _, _)| range.clone())
        .collect();
    mentions.extend(
        PATH.find_iter(&prose)
            .filter(|m| !files.resolve(m.as_str()).is_empty())
            .map(|m| m.range()),
    );
    let mut missing_symbols = Vec::new();
    for ((range, text), found) in symbols.iter().zip(last_hit) {
        if *found || !symbol_like(text) {
            continue;
        }
        let name = needles(last_name(text));
        if name
            .iter()
            .any(|needle| input.context.contains(needle.as_str()))
        {
            continue;
        }
        let sentence = enclosing(&sentences, range);
        let end = sentence.end.min(prose.len());
        if negated(&prose, &sentence, &ranges) {
            continue;
        }
        if mentions
            .iter()
            .any(|mention| mention.start >= sentence.start && mention.end <= end)
        {
            missing_symbols.push(text.clone());
        }
    }

    // Described without reading: no read this task, the reply names repository files, and
    // neither a tool result nor the user's own messages in context name any of them. A
    // reply naming no repository file (explaining a pasted snippet, a framework type, the
    // work so far) is not about code it should have read; replaying the real sessions,
    // that case made up most of the nudges.
    let unread = !input.read && {
        let names: Vec<String> = resolved
            .iter()
            .map(|citation| citation.path.as_str())
            .chain(paths.iter().filter(|p| p.2).map(|p| p.1.as_str()))
            .filter_map(|path| {
                path.replace('\\', "/")
                    .rsplit('/')
                    .next()
                    .map(str::to_lowercase)
            })
            .filter(|name| !name.is_empty())
            .collect();
        let context = input.context.to_lowercase();
        !names.is_empty()
            && !names
                .iter()
                .any(|name| input.tools.contains(name.as_str()) || context.contains(name.as_str()))
    };
    Analysis {
        applies: true,
        results,
        missing_symbols,
        unread,
    }
}

/// The model-facing request to correct the answer.
pub fn nudge(analysis: &Analysis) -> String {
    let mut items = Vec::new();
    for (citation, verdict) in &analysis.results {
        let label = citation.label();
        match verdict {
            Verdict::Missing => items.push(format!("- {label} — no such file in the workspace")),
            Verdict::OutOfRange { lines } => {
                items.push(format!("- {label} — the file has {lines} lines"))
            }
            Verdict::Mismatch { anchor } => {
                items.push(format!("- {label} — `{anchor}` is not in that file"))
            }
            Verdict::Offset { anchor, line } => items.push(format!(
                "- {label} — `{anchor}` is not near that line (found at line {line})"
            )),
            _ => {}
        }
    }
    if !analysis.missing_symbols.is_empty() {
        let names: Vec<String> = analysis
            .missing_symbols
            .iter()
            .map(|name| format!("`{name}`"))
            .collect();
        items.push(format!(
            "- {} — not found anywhere in the workspace",
            names.join(", ")
        ));
    }
    let mut out = String::from("[system: ");
    if !items.is_empty() {
        out.push_str(
            "Some code claims in your answer could not be verified against the workspace:\n",
        );
        out.push_str(&items.join("\n"));
        out.push_str("\nRead those places now and correct the answer.");
    }
    out.push_str(" Give the final answer again, no longer than before; change only what was wrong and do not add a sources section.]");
    out
}

/// The user-facing notice for line offsets (no model call is spent on them).
pub fn offset_notice(analysis: &Analysis) -> Option<String> {
    let lines: Vec<String> = analysis
        .results
        .iter()
        .filter_map(|(citation, verdict)| match verdict {
            Verdict::Offset { line, .. } => Some(format!(
                "[引用核实] {} → 实际在第 {line} 行",
                citation.label()
            )),
            _ => None,
        })
        .take(MAX_OFFSET_NOTES)
        .collect();
    (!lines.is_empty()).then(|| lines.join("\n"))
}

/// The user-facing notice when a corrected answer still does not check out.
pub fn failure_notice(analysis: &Analysis) -> String {
    let mut items: Vec<String> = analysis
        .results
        .iter()
        .filter_map(|(citation, verdict)| {
            let label = citation.label();
            match verdict {
                Verdict::Missing => Some(format!("{label}（文件不存在）")),
                Verdict::OutOfRange { lines } => Some(format!("{label}（文件只有 {lines} 行）")),
                Verdict::Mismatch { anchor } => Some(format!(
                    "{label}（文件里没有 `{}`）",
                    crate::text::clip_chars(anchor, 60)
                )),
                Verdict::Offset { line, .. } => Some(format!("{label}（实际在第 {line} 行）")),
                _ => None,
            }
        })
        .collect();
    if !analysis.missing_symbols.is_empty() {
        let names: Vec<String> = analysis
            .missing_symbols
            .iter()
            .map(|name| format!("`{}`", crate::text::clip_chars(name, 60)))
            .collect();
        items.push(format!("{}（仓库里搜不到）", names.join("、")));
    }
    if analysis.unread {
        items.push("回答讲了仓库代码，但本轮没有读过代码".into());
    }
    format!("[引用核实] 以下引用没能核实：{}", items.join("；"))
}

// ---------------------------------------------------------------- the gate

/// What the acceptance report records about the check.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct CitationStats {
    pub checked: usize,
    pub passed: usize,
    pub offset: usize,
    pub failed: usize,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub missing_symbols: usize,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub unread: bool,
    pub nudged: bool,
}

fn is_zero(n: &usize) -> bool {
    *n == 0
}

impl CitationStats {
    /// One line for `/status acceptance`.
    pub fn summary(&self) -> String {
        let mut out = format!(
            "引用：{} 条，{} 条核实，{} 条行号偏移",
            self.checked, self.passed, self.offset
        );
        if self.failed > 0 {
            out.push_str(&format!("，{} 条没能核实", self.failed));
        }
        if self.missing_symbols > 0 {
            out.push_str(&format!("，{} 个符号仓库里搜不到", self.missing_symbols));
        }
        if self.unread {
            out.push_str("，没读代码就作答");
        }
        if self.nudged {
            out.push_str("（已催模型更正一次）");
        }
        out
    }
}

#[derive(Default)]
struct State {
    start_calls: u64,
    nudged: bool,
    read: bool,
    last: Option<u64>,
    files: Option<Arc<Files>>,
}

/// The `cite_check` gate.
pub struct Citations {
    enabled: bool,
    max_turns: u32,
    workspace: Arc<Workspace>,
    evidence: Arc<Evidence>,
    progress: Option<Arc<Progress>>,
    state: Mutex<State>,
}

impl Citations {
    pub fn new(
        enabled: bool,
        max_turns: u32,
        workspace: Arc<Workspace>,
        evidence: Arc<Evidence>,
        progress: Option<Arc<Progress>>,
    ) -> Self {
        Self {
            enabled,
            max_turns,
            workspace,
            evidence,
            progress,
            state: Mutex::default(),
        }
    }

    async fn notice(&self, text: String) {
        if let Some(progress) = &self.progress {
            progress.notice(text).await;
        }
    }
}

/// Lower-case text of the tool calls and results in context, and the [`joined`] words of
/// the tool results and the user's own messages (program notices left out, so a nudge
/// does not count as a source). Newest first, bounded.
fn context(session: &Session) -> (String, String) {
    let mut tools = String::new();
    let mut said = String::new();
    for message in session.messages.iter().rev() {
        if tools.len() + said.len() > MAX_CONTEXT_BYTES {
            break;
        }
        for block in &message.blocks {
            match block {
                Block::ToolUse { input, .. } => {
                    tools.push_str(&input.to_string().to_lowercase());
                    tools.push('\n');
                }
                Block::ToolResult { content, .. } => {
                    tools.push_str(&content.to_lowercase());
                    tools.push('\n');
                    said.push_str(content);
                    said.push('\n');
                }
                Block::Text(text)
                    if message.role == Role::User && message.origin != Origin::Notice =>
                {
                    said.push_str(text);
                    said.push('\n');
                }
                _ => {}
            }
        }
    }
    (tools, joined(&said))
}

fn hash(text: &str) -> u64 {
    use std::hash::{Hash, Hasher};
    let mut hasher = std::collections::hash_map::DefaultHasher::new();
    text.hash(&mut hasher);
    hasher.finish()
}

impl TurnHook for Citations {
    fn task_note<'a>(&'a self, session: &'a Session) -> BoxFuture<'a, Option<String>> {
        if let Ok(mut state) = self.state.lock() {
            *state = State {
                start_calls: session.accounting.model_calls,
                ..State::default()
            };
        }
        Box::pin(async { None })
    }

    fn tool_note(&self, name: &str, input: &Value, output: &ToolOutput) -> Option<String> {
        if output.failed {
            return None;
        }
        let read = READ_TOOLS.contains(&name)
            || (name == "shell"
                && input
                    .get("command")
                    .and_then(Value::as_str)
                    .is_some_and(|command| {
                        classify::read_only(command, Dialect::for_command(command))
                    }));
        if read && let Ok(mut state) = self.state.lock() {
            state.read = true;
        }
        None
    }

    fn finish_nudge<'a>(&'a self, session: &'a Session) -> BoxFuture<'a, Option<String>> {
        Box::pin(async move {
            if !self.enabled {
                return None;
            }
            let reply = last_reply(session);
            if reply.trim().is_empty() {
                return None;
            }
            let (files, read, nudged) = {
                let mut state = self.state.lock().ok()?;
                let used = session
                    .accounting
                    .model_calls
                    .saturating_sub(state.start_calls);
                if used + 1 >= u64::from(self.max_turns) {
                    return None;
                }
                // Changed files have their own gates (verification, done_check).
                if self.workspace.fingerprint().ok().flatten().is_some() {
                    return None;
                }
                let digest = hash(&reply);
                if state.last == Some(digest) {
                    return None;
                }
                state.last = Some(digest);
                (state.files.clone(), state.read, state.nudged)
            };
            let (tools, said) = context(session);
            let root = self.workspace.root().to_path_buf();
            let (files, analysis) = tokio::task::spawn_blocking(move || {
                let files = files.unwrap_or_else(|| Arc::new(Files::scan(&root)));
                let analysis = analyze(
                    &files,
                    &Input {
                        reply: &reply,
                        tools: &tools,
                        context: &said,
                        read,
                    },
                );
                (files, analysis)
            })
            .await
            .ok()?;
            if let Ok(mut state) = self.state.lock() {
                state.files = Some(files);
            }
            if !analysis.applies {
                return None;
            }
            if analysis.needs_fix() {
                if !nudged {
                    if let Ok(mut state) = self.state.lock() {
                        state.nudged = true;
                    }
                    self.evidence.set_citations(analysis.stats(true));
                    return Some(nudge(&analysis));
                }
                self.evidence.set_citations(analysis.stats(true));
                self.notice(failure_notice(&analysis)).await;
                return None;
            }
            self.evidence.set_citations(analysis.stats(nudged));
            if let Some(text) = offset_notice(&analysis) {
                self.notice(text).await;
            }
            None
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn exts() -> HashSet<String> {
        ["rs", "py", "cs", "md", "json"]
            .iter()
            .map(|e| (*e).to_owned())
            .collect()
    }

    fn cites(text: &str) -> Vec<(String, usize, usize)> {
        parse(&prose(text), &exts())
            .into_iter()
            .map(|c| (c.path, c.start, c.end))
            .collect()
    }

    fn one(path: &str, start: usize, end: usize) -> Vec<(String, usize, usize)> {
        vec![(path.to_owned(), start, end)]
    }

    #[test]
    fn parses_every_reference_form() {
        assert_eq!(cites("见 src/a.rs:12。"), one("src/a.rs", 12, 12));
        assert_eq!(cites("a.rs:12-30"), one("a.rs", 12, 30));
        assert_eq!(cites("`a.rs:12:5` 报错"), one("a.rs", 12, 12));
        assert_eq!(cites("`a.rs`:12"), one("a.rs", 12, 12));
        assert_eq!(cites("a.rs#L12"), one("a.rs", 12, 12));
        assert_eq!(cites("a.rs#L12-L30"), one("a.rs", 12, 30));
        assert_eq!(cites("[a.rs](src/a.rs#L12)"), one("src/a.rs", 12, 12));
        assert_eq!(cites("[第 12 行](src/a.rs:12)"), one("src/a.rs", 12, 12));
        assert_eq!(cites("`a.rs` 第 12 行"), one("a.rs", 12, 12));
        assert_eq!(cites("a.rs 的第 12–30 行"), one("a.rs", 12, 30));
        assert_eq!(cites("a.rs（第 12 行）"), one("a.rs", 12, 12));
        assert_eq!(cites("a.rs（第 12—30 行）"), one("a.rs", 12, 30));
        assert_eq!(cites("a.rs 第 12～30 行"), one("a.rs", 12, 30));
        assert_eq!(cites("a.rs line 12"), one("a.rs", 12, 12));
        assert_eq!(cites("a.rs, lines 12-30"), one("a.rs", 12, 30));
        assert_eq!(cites("a.rs (L12)"), one("a.rs", 12, 12));
        assert_eq!(cites("see line 7 of `src/a.rs`"), one("src/a.rs", 7, 7));
        assert_eq!(
            cites("文件shop/pricing.py第4行说明"),
            one("shop/pricing.py", 4, 4)
        );
    }

    #[test]
    fn parses_windows_paths() {
        assert_eq!(
            cites(r"D:\repo\src\a.rs:12"),
            one("D:/repo/src/a.rs", 12, 12)
        );
        assert_eq!(
            cites(r"\\?\D:\repo\src\a.rs:12"),
            one("D:/repo/src/a.rs", 12, 12)
        );
        assert_eq!(cites("/home/x/repo/a.rs:3"), one("/home/x/repo/a.rs", 3, 3));
    }

    #[test]
    fn skips_code_blocks_hosts_urls_and_bad_numbers() {
        assert!(cites("```\nerror at src/a.rs:12\n```\n").is_empty());
        assert!(cites("~~~text\nsrc/a.rs:12\n~~~").is_empty());
        assert!(
            cites(
                "连接 3.215.238.8:25432 和 localhost:5080，见 https://github.com/x/y/blob/main/a.rs#L12 以及 //stream.binance.com:9443"
            )
            .is_empty()
        );
        assert!(cites("a.rs:0 和 a.rs:1234567").is_empty());
        // Mixed Chinese text never panics on a byte boundary.
        let _ = cites("中文：好的a.rs：第行，`中文`，第 行 L x.rs#L");
    }

    #[test]
    fn bare_line_numbers_bind_within_their_section() {
        let found = parse(&prose("`src/a.rs` 里解析头部，见第 30 行。"), &exts());
        assert_eq!(found.len(), 1);
        assert!(found[0].bound);
        assert_eq!((found[0].path.as_str(), found[0].start), ("src/a.rs", 30));
        // Paragraphs bound a reply without headings.
        assert!(cites("讲的是 `src/a.rs`。\n\n另外第 30 行是别的东西。").is_empty());
        // Headings bound a reply that has them, blank lines inside a section do not.
        assert_eq!(
            cites("## src/a.rs\n\n解析在第 30 行。\n\n## 其他\n\n第 40 行也是。"),
            one("src/a.rs", 30, 30)
        );
        // A file of a kind the workspace does not have is no binding target.
        assert!(cites("连 `pe-dev.xxx.com` 时看第 3 行").is_empty());
    }

    #[test]
    fn identifier_words() {
        assert_eq!(words("parseHeader"), ["parse", "header"]);
        assert_eq!(words("parse_header"), ["parse", "header"]);
        assert_eq!(
            words("HTTPServer utf8Decoder"),
            ["http", "server", "utf8", "decoder"]
        );
        assert_eq!(
            needles("Access__Bootstrap__ConsoleKey"),
            [" access ", " bootstrap ", " console key "]
        );
        assert_eq!(
            needles("book.bids[:DEPTH]"),
            [" book ", " bids ", " depth "]
        );
    }

    fn write(root: &Path, rel: &str, text: &str) {
        let path = root.join(rel);
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(path, text).unwrap();
    }

    fn fixture() -> (tempfile::TempDir, PathBuf) {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        let mut long = String::new();
        for n in 1..=100 {
            long.push_str(&match n {
                10 => "pub fn parse_header() {}".to_owned(),
                60 => "pub fn far_away() {}".to_owned(),
                _ => format!("// filler {n}"),
            });
            long.push('\n');
        }
        write(&root, "src/long.rs", &long);
        write(&root, "src/a.rs", "fn main() {}\n");
        write(
            &root,
            "shop/discount.py",
            "\"\"\"Old rules.\"\"\"\n\ndef apply_discount(order):\n    total = order.total\n    if total >= 100:\n        total -= 10\n    return total\n",
        );
        write(
            &root,
            "shop/pricing.py",
            "MEMBER_RATE = 0.9\n\n\ndef apply_discount(order):\n    if order.member:\n        return order.total * MEMBER_RATE\n    return order.total\n",
        );
        write(
            &root,
            "shop/checkout.py",
            "from shop.pricing import apply_discount\n\n\ndef checkout(order):\n    return apply_discount(order)\n",
        );
        write(
            &root,
            "src/QuoteHub.Distributor/Program.cs",
            "var builder = WebApplication.CreateBuilder(args);\n\n\n\nbuilder.WebHost.UseKestrel();\n\n\nvoid ConfigureServices() {}\n",
        );
        write(
            &root,
            "admin/SourceEndpoints.cs",
            "class A {\n  void MapPost() {}\n}\n",
        );
        write(
            &root,
            "console/SourceEndpoints.cs",
            "class B {\n  void Other() {}\n}\n",
        );
        write(
            &root,
            "src/Boot.cs",
            "class Boot {\n  void Run() {\n    var key = bootstrap.ConsoleKey;\n  }\n}\n",
        );
        write(
            &root,
            "src/Stats.cs",
            "class Stats {\n\n\n\n  public int StatisticsInterval { get; set; }\n  public int MaxRetries { get; set; }\n  string c = options.Redis.ConnectionString;\n}\n",
        );
        let mut admin = String::new();
        for n in 1..=20 {
            admin.push_str(if n == 16 {
                "app.MapGet(\"/sources\", List);\n"
            } else {
                "\n"
            });
        }
        write(&root, "src/AdminApi.cs", &admin);
        write(
            &root,
            "src/QuoteHub.Wire/Codec.cs",
            "static class Codec {\n  public static int ReadInt(byte[] b) => b[0];\n}\n",
        );
        write(
            &root,
            "src/Monitor.cs",
            "class SourceConfigurationMonitor {}\n",
        );
        write(&root, "docs/deploy.md", "# Deploy\n\nlisten on 8080\n");
        (dir, root)
    }

    #[test]
    fn resolves_paths_like_people_write_them() {
        let (_dir, root) = fixture();
        let files = Files::scan(&root);
        assert_eq!(files.resolve("src/a.rs"), ["src/a.rs"]);
        assert_eq!(files.resolve("./src/a.rs"), ["src/a.rs"]);
        assert_eq!(files.resolve("a.rs"), ["src/a.rs"]);
        assert_eq!(
            files.resolve("Distributor/Program.cs"),
            ["src/QuoteHub.Distributor/Program.cs"]
        );
        let mut twins = files.resolve("SourceEndpoints.cs");
        twins.sort();
        assert_eq!(
            twins,
            ["admin/SourceEndpoints.cs", "console/SourceEndpoints.cs"]
        );
        let absolute = root.join("src").join("a.rs");
        assert_eq!(files.resolve(&absolute.to_string_lossy()), ["src/a.rs"]);
        assert_eq!(
            files.resolve(&crate::paths::display(&absolute)),
            ["src/a.rs"]
        );
        assert!(files.resolve("/elsewhere/repo/src/a.rs").is_empty());
        assert!(files.resolve("nope.rs").is_empty());
        assert!(files.resolve_dir("src/QuoteHub.Wire"));
        assert!(files.resolve_dir("QuoteHub.Wire/"));
        assert!(!files.resolve_dir("src/QuoteHub.Missing"));
    }

    fn at(path: &str, start: usize) -> Citation {
        Citation {
            path: path.into(),
            start,
            end: start,
            span: 0..0,
            bound: false,
        }
    }

    #[test]
    fn verifies_lines_and_nearby_symbols() {
        let (_dir, root) = fixture();
        let files = Files::scan(&root);
        let anchor = |text: &str| vec![Anchor::new(text)];
        let check = |path: &str, line: usize, anchors: &[Anchor]| {
            verify(&files, &at(path, line), anchors, "")
        };
        assert_eq!(
            check("src/long.rs", 12, &anchor("parse_header")),
            Verdict::Passed
        );
        assert_eq!(
            check("src/long.rs", 35, &anchor("parse_header")),
            Verdict::Passed
        );
        assert_eq!(
            check("src/long.rs", 100, &anchor("parse_header")),
            Verdict::Offset {
                anchor: "parse_header".into(),
                line: 10
            }
        );
        assert_eq!(
            check("src/long.rs", 10, &anchor("Decoder")),
            Verdict::Mismatch {
                anchor: "Decoder".into()
            }
        );
        assert_eq!(
            check("src/long.rs", 120, &[]),
            Verdict::OutOfRange { lines: 100 }
        );
        assert_eq!(check("src/long.rs", 50, &[]), Verdict::Passed);
        assert_eq!(check("src/nope.rs", 3, &[]), Verdict::Missing);
        // Read elsewhere (a dependency), a host:port, a path outside: not checkable.
        assert_eq!(
            verify(
                &files,
                &at("src/nope.rs", 3),
                &[],
                "{\"path\":\"../dep/src/nope.rs\"}"
            ),
            Verdict::Unverified
        );
        assert_eq!(check("pe-dev.xxx.com", 5432, &[]), Verdict::Unverified);
        assert_eq!(check("/elsewhere/a.rs", 1, &[]), Verdict::Unverified);
        // Case, camelCase and config-key spellings.
        assert_eq!(
            check("src/Stats.cs", 5, &anchor("statisticsInterval")),
            Verdict::Passed
        );
        assert_eq!(
            check("src/Boot.cs", 3, &anchor("Access__Bootstrap__ConsoleKey")),
            Verdict::Passed
        );
        // Two files of the same name: the one that confirms wins, none is not an error.
        assert_eq!(
            check("SourceEndpoints.cs", 2, &anchor("MapPost")),
            Verdict::Passed
        );
        assert_eq!(
            check("SourceEndpoints.cs", 2, &anchor("NotThere")),
            Verdict::Ambiguous
        );
    }

    fn pad() -> String {
        "\n\n补充说明：这一段只是为了让回答足够长，不涉及任何具体的代码位置，也不提到任何文件或者符号的名字，读者可以忽略这一段文字。"
            .repeat(4)
    }

    fn analysis(root: &Path, reply: &str, read: bool) -> Analysis {
        let files = Files::scan(root);
        analyze(
            &files,
            &Input {
                reply: &format!("{reply}{}", pad()),
                tools: "",
                context: " ",
                read,
            },
        )
    }

    /// The suspicious references that people confirmed correct (0027, problem 3),
    /// reduced to minimal samples: none may be reported.
    #[test]
    fn replayed_correct_references_pass() {
        let (_dir, root) = fixture();
        for reply in [
            "统计周期由 `statisticsInterval` 控制（`src/Stats.cs:5`）。",
            "控制台密钥来自环境变量 `Access__Bootstrap__ConsoleKey`，读取在 `src/Boot.cs:3`。",
            "`src/AdminApi.cs` 第 16 行注册了路由……刷新由 `SourceConfigurationMonitor` 负责。",
            "重试次数是 `max_retries`，见 `src/Stats.cs:6`。",
            "连接串配置在 `Quotes:Redis:ConnectionString`（`src/Stats.cs:7`）。",
            "| `SourceEndpoints.cs:2` | `MapPost` |\n|---|---|",
            "监听在 `Distributor/Program.cs:5` 的 `UseKestrel`。",
            "服务注册在 `src/QuoteHub.Distributor/Program.cs`，`ConfigureServices`（第 8 行）。",
        ] {
            let found = analysis(&root, reply, true);
            assert!(found.applies, "{reply}");
            assert!(!found.results.is_empty(), "{reply}");
            assert!(!found.needs_fix(), "{reply}: {:?}", found.results);
            assert!(
                found
                    .results
                    .iter()
                    .all(|(_, v)| matches!(v, Verdict::Passed | Verdict::Ambiguous)),
                "{reply}: {:?}",
                found.results
            );
        }
    }

    #[test]
    fn symbols_missing_from_the_repository() {
        let (_dir, root) = fixture();
        let found = analysis(
            &root,
            "`src/QuoteHub.Wire` 里的解码器类通常叫 `Decoder`，它先 `Wrap` 到缓冲区上再读字段，`Codec` 负责整数。",
            true,
        );
        assert!(found.applies);
        assert_eq!(found.missing_symbols, ["Decoder", "Wrap"]);
        // Said not to exist: a correct answer.
        let found = analysis(
            &root,
            "`src/QuoteHub.Wire` 里没有 `Decoder` 也没有 `Wrap`，编解码是手写的，`Codec` 负责整数。",
            true,
        );
        assert!(
            found.missing_symbols.is_empty(),
            "{:?}",
            found.missing_symbols
        );
    }

    /// A reference said NOT to contain a symbol is not checked for that symbol (0055 P1);
    /// the same reference stated positively still is.
    #[test]
    fn negated_references_are_not_checked_for_their_symbols() {
        let (_dir, root) = fixture();
        for reply in [
            "`src/AdminApi.cs:16` 也没开 `RespectNullableAnnotations`。",
            "`src/AdminApi.cs:16` 未设置 `RespectNullableAnnotations`。",
            "`src/AdminApi.cs:16` does not set `RespectNullableAnnotations`.",
        ] {
            let found = analysis(&root, reply, true);
            assert!(found.applies, "{reply}");
            assert!(!found.needs_fix(), "{reply}: {:?}", found.results);
        }
        let found = analysis(
            &root,
            "`src/AdminApi.cs:16` 开了 `RespectNullableAnnotations`。",
            true,
        );
        assert!(
            found
                .results
                .iter()
                .any(|(_, v)| matches!(v, Verdict::Mismatch { .. })),
            "{:?}",
            found.results
        );
        // A negation word inside code does not make the sentence negated.
        let found = analysis(
            &root,
            "`src/QuoteHub.Wire` 里的 `NoneDecoder` 先 `Wrap` 到缓冲区，`Codec` 负责整数。",
            true,
        );
        assert_eq!(found.missing_symbols, ["NoneDecoder", "Wrap"]);
        // Line ranges are still checked in a negated clause.
        let found = analysis(&root, "`src/AdminApi.cs:99` 没有 `MapGet`。", true);
        assert!(found.needs_fix(), "{:?}", found.results);
    }

    /// Another line reference next to a citation is not a symbol to look for (0057).
    #[test]
    fn line_references_are_not_anchors() {
        let (_dir, root) = fixture();
        for reply in [
            "路由在 `src/AdminApi.cs:16` 和 `:18`。",
            "路由在 `src/AdminApi.cs:16`，参数是 `42`。",
        ] {
            let found = analysis(&root, reply, true);
            assert!(found.applies, "{reply}");
            assert!(!found.needs_fix(), "{reply}: {:?}", found.results);
        }
    }

    /// Replaying the real sessions panicked here: a clause or sentence starting right
    /// after `。` or `；` (three bytes each) was sliced one byte past the mark.
    #[test]
    fn chinese_punctuation_before_a_symbol_does_not_split_a_character() {
        let (_dir, root) = fixture();
        for reply in [
            "结论见 `src/Stats.cs:5`。`Decoder` 在 `src/QuoteHub.Wire` 里没有出现。",
            "先看 `src/Boot.cs:3`；`Wrap` 是 `src/QuoteHub.Wire` 里的方法。",
            "路由在 `src/AdminApi.cs` 第 16 行！`Codec`、`Wrap` 都在 `src/QuoteHub.Wire`？",
        ] {
            let found = analysis(&root, reply, true);
            assert!(found.applies, "{reply}");
        }
    }

    #[test]
    fn unread_and_unrelated_replies() {
        let (_dir, root) = fixture();
        let found = analysis(
            &root,
            "结算用的是 `shop/pricing.py` 的 `apply_discount`。",
            false,
        );
        assert!(found.applies && found.unread);
        let found = analysis(
            &root,
            "结算用的是 `shop/pricing.py` 的 `apply_discount`。",
            true,
        );
        assert!(found.applies && !found.unread);
        // General knowledge naming nothing in the workspace is not checked.
        let found = analysis(
            &root,
            "`valkey` 是 `redis` 的分支，协议兼容，许可证不同，`StackExchangeRedis` 可以直接连。",
            false,
        );
        assert!(!found.applies);
    }

    fn assistant(text: &str) -> crate::domain::Message {
        crate::domain::Message {
            role: Role::Assistant,
            blocks: vec![Block::Text(text.into())],
            origin: Origin::Unknown,
        }
    }

    struct Gate {
        _data: tempfile::TempDir,
        _dir: tempfile::TempDir,
        root: PathBuf,
        workspace: Arc<Workspace>,
        evidence: Arc<Evidence>,
        hook: Citations,
        session: Session,
    }

    fn gate(max_turns: u32) -> Gate {
        let (dir, root) = fixture();
        let data = tempfile::tempdir().unwrap();
        let store = crate::session::Store::open(data.path()).unwrap();
        let workspace = Arc::new(Workspace::new(root.clone(), store).unwrap());
        let evidence = Arc::new(Evidence::default());
        let hook = Citations::new(true, max_turns, workspace.clone(), evidence.clone(), None);
        let session = Session::new(root.clone(), String::new(), String::new(), None);
        Gate {
            _data: data,
            _dir: dir,
            root,
            workspace,
            evidence,
            hook,
            session,
        }
    }

    impl Gate {
        async fn finish(&mut self, reply: &str) -> Option<String> {
            self.session
                .messages
                .push(assistant(&format!("{reply}{}", pad())));
            self.hook.finish_nudge(&self.session).await
        }

        async fn start(&self, read: bool) {
            self.hook.task_note(&self.session).await;
            if read {
                self.hook.tool_note(
                    "read",
                    &serde_json::json!({"path": "shop/pricing.py"}),
                    &ToolOutput::default(),
                );
            }
        }
    }

    const GOOD: &str = "结算用的是 `shop/pricing.py:4` 的 `apply_discount`（`shop/checkout.py:1` 导入），只有会员 9 折。";

    #[tokio::test]
    async fn gate_passes_good_answers_and_records_them() {
        let mut gate = gate(50);
        gate.start(true).await;
        assert_eq!(gate.finish(GOOD).await, None);
        let stats = gate.evidence.citations().unwrap();
        assert_eq!((stats.checked, stats.passed, stats.nudged), (2, 2, false));
        assert!(
            stats.summary().contains("引用：2 条，2 条核实"),
            "{}",
            stats.summary()
        );
    }

    #[tokio::test]
    async fn gate_skips_what_it_does_not_cover() {
        // Short replies.
        let mut short = gate(50);
        short.start(true).await;
        short
            .session
            .messages
            .push(assistant("见 `shop/pricing.py:40`。"));
        assert_eq!(short.hook.finish_nudge(&short.session).await, None);
        assert!(short.evidence.citations().is_none());
        // Nothing of this repository named.
        let mut unrelated = gate(50);
        unrelated.start(false).await;
        assert_eq!(
            unrelated
                .finish("`valkey` 和 `redis` 协议兼容，`RESP3` 一样能用。")
                .await,
            None
        );
        assert!(unrelated.evidence.citations().is_none());
        // Near the call limit.
        let mut limited = gate(2);
        limited.start(true).await;
        limited.session.accounting.model_calls = 1;
        assert_eq!(limited.finish("见 `shop/pricing.py:40`。").await, None);
        // Files changed: the other gates own the task.
        let mut changed = gate(50);
        changed.start(true).await;
        changed.workspace.begin().unwrap();
        changed
            .workspace
            .replace(
                &changed.root.join("src/a.rs"),
                Some("fn main() {}\n"),
                "fn main() { }\n",
            )
            .unwrap();
        assert_eq!(changed.finish("见 `shop/pricing.py:40`。").await, None);
        assert!(changed.evidence.citations().is_none());
        // Switched off.
        let mut off = gate(50);
        off.hook.enabled = false;
        off.start(true).await;
        assert_eq!(off.finish("见 `shop/pricing.py:40`。").await, None);
    }

    #[tokio::test]
    async fn gate_reports_offsets_without_a_model_call() {
        let mut gate = gate(50);
        gate.start(true).await;
        assert_eq!(
            gate.finish("头部解析在 `parse_header`（`src/long.rs:90`）。")
                .await,
            None
        );
        let stats = gate.evidence.citations().unwrap();
        assert_eq!((stats.offset, stats.failed, stats.nudged), (1, 0, false));
    }

    #[tokio::test]
    async fn gate_nudges_once_then_reports() {
        let mut gate = gate(50);
        gate.start(true).await;
        let nudge = gate
            .finish("只有会员 9 折，见 `shop/pricing.py:40` 的 `apply_discount`。")
            .await
            .unwrap();
        assert!(
            nudge.contains("shop/pricing.py:40 — the file has 7 lines"),
            "{nudge}"
        );
        assert!(nudge.contains("no longer than before"), "{nudge}");
        // Still wrong after the nudge: reported, not nudged again.
        assert_eq!(
            gate.finish("只有会员 9 折，见 `shop/pricing.py:41` 的 `apply_discount`。")
                .await,
            None
        );
        let stats = gate.evidence.citations().unwrap();
        assert_eq!((stats.failed, stats.nudged), (1, true));
        assert_eq!(
            gate.finish("只有会员 9 折，见 `shop/pricing.py:42` 的 `apply_discount`。")
                .await,
            None
        );
        // A new task may be nudged again.
        gate.start(true).await;
        assert!(
            gate.finish("只有会员 9 折，见 `shop/pricing.py:43` 的 `apply_discount`。")
                .await
                .is_some()
        );
    }

    #[tokio::test]
    async fn gate_records_unread_and_nudges_missing_symbols() {
        // Unread is recorded, not nudged (0027, the replay of the real sessions).
        let mut unread = gate(50);
        unread.start(false).await;
        assert_eq!(unread.finish(GOOD).await, None);
        assert!(unread.evidence.citations().unwrap().unread);
        let mut missing = gate(50);
        missing.start(true).await;
        let nudge = missing
            .finish("`src/QuoteHub.Wire` 里的解码器类叫 `Decoder`，它先 `Wrap` 到缓冲区上，`Codec` 负责整数。")
            .await
            .unwrap();
        assert!(
            nudge.contains("`Decoder`, `Wrap` — not found anywhere in the workspace"),
            "{nudge}"
        );
        // The corrected answer passes; the nudge text itself is no source.
        missing
            .session
            .messages
            .push(crate::domain::Message::notice(nudge));
        assert_eq!(
            missing
                .finish("`src/QuoteHub.Wire` 的编解码是手写的，`Codec`（`src/QuoteHub.Wire/Codec.cs:2`）读整数。")
                .await,
            None
        );
        let stats = missing.evidence.citations().unwrap();
        assert_eq!((stats.missing_symbols, stats.nudged), (0, true));
    }

    /// Replays the final answers of saved sessions through the check, without calling a
    /// model (0027 "回放"); the workspaces are read as they are now:
    /// `COVE_REPLAY=<sessions dir> cargo test --release --lib replay_sessions -- --ignored --nocapture`
    #[test]
    #[ignore]
    fn replay_sessions() {
        let Ok(dir) = std::env::var("COVE_REPLAY") else {
            return;
        };
        let mut scans: HashMap<String, Option<Arc<Files>>> = HashMap::new();
        let (mut turns, mut checked, mut nudged, mut offsets, mut cited) = (0, 0, 0, 0, 0);
        for entry in std::fs::read_dir(dir).unwrap().flatten() {
            let Ok(text) = std::fs::read_to_string(entry.path()) else {
                continue;
            };
            let Ok(session) = serde_json::from_str::<Value>(&text) else {
                continue;
            };
            let cwd = session["cwd"].as_str().unwrap_or_default().to_owned();
            let files = scans
                .entry(cwd.clone())
                .or_insert_with(|| {
                    let path = Path::new(&cwd);
                    path.is_dir()
                        .then(|| Arc::new(Files::scan(&path.canonicalize().unwrap())))
                })
                .clone();
            let Some(files) = files else { continue };
            let id: String = session["id"]
                .as_str()
                .unwrap_or_default()
                .chars()
                .take(8)
                .collect();
            let empty = Vec::new();
            let messages = session["messages"].as_array().unwrap_or(&empty);
            let (mut tools, mut said) = (String::new(), String::new());
            let (mut used, mut reply) = (Vec::<String>::new(), String::new());
            let mut evaluate = |used: &[String], reply: &str, tools: &str, said: &str| {
                if reply.is_empty() || used.iter().any(|n| n == "edit" || n == "write") {
                    return;
                }
                turns += 1;
                let read = used
                    .iter()
                    .any(|n| READ_TOOLS.contains(&n.as_str()) || n == "shell");
                let analysis = analyze(
                    &files,
                    &Input {
                        reply,
                        tools,
                        context: said,
                        read,
                    },
                );
                if !analysis.applies {
                    return;
                }
                checked += 1;
                cited += analysis.results.len();
                offsets += analysis
                    .results
                    .iter()
                    .filter(|(_, v)| matches!(v, Verdict::Offset { .. }))
                    .count();
                if analysis.needs_fix() {
                    nudged += 1;
                    println!("--- {id} {}\n{}\n", cwd, nudge(&analysis));
                }
            };
            for message in messages {
                let role = message["role"].as_str().unwrap_or_default();
                let origin = message["origin"].as_str().unwrap_or("unknown");
                let blocks = message["blocks"].as_array().unwrap_or(&empty);
                let has_result = blocks.iter().any(|b| b.get("ToolResult").is_some());
                if role == "User" && (origin == "human" || (origin == "unknown" && !has_result)) {
                    evaluate(&used, &reply, &tools, &joined(&said));
                    used.clear();
                    reply.clear();
                }
                for block in blocks {
                    if let Some(text) = block.get("Text").and_then(Value::as_str) {
                        if role == "Assistant" {
                            reply = text.to_owned();
                        } else if origin != "notice" {
                            said.push_str(text);
                            said.push('\n');
                        }
                    } else if let Some(tool) = block.get("ToolUse") {
                        used.push(tool["name"].as_str().unwrap_or_default().to_owned());
                        tools.push_str(&tool["input"].to_string().to_lowercase());
                        tools.push('\n');
                    } else if let Some(result) = block.get("ToolResult") {
                        let content = result["content"].as_str().unwrap_or_default();
                        tools.push_str(&content.to_lowercase());
                        tools.push('\n');
                        said.push_str(content);
                        said.push('\n');
                    }
                }
            }
            evaluate(&used, &reply, &tools, &joined(&said));
        }
        println!(
            "turns without edits: {turns}; checked: {checked}; references: {cited}; would nudge: {nudged}; offsets: {offsets}"
        );
    }
}
