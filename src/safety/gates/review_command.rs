//! The project's own review command (`review_commands`, docs/optimizations/0028): run
//! it, read its findings from the output (JSON, `[HIGH]`-style markers or
//! `NO_ISSUES`), and keep the model from passing it by editing the command.
//!
//! The verdict comes from the output, never the exit code: a non-zero exit, a timeout
//! or a missing program means the review did not run, which is not a pass.

use std::{
    collections::BTreeSet,
    io::Write as _,
    path::{Path, PathBuf},
    sync::{Mutex, OnceLock},
    time::{Duration, Instant},
};

use regex::Regex;
use serde_json::Value;
use sha2::{Digest, Sha256};
use tokio_util::sync::CancellationToken;

use super::review::{NO_ISSUES, Verdict};
use crate::{
    domain::{Error, Progress, Result},
    safety::permission::shell::{self, Dialect},
};

pub const DEFAULT_ROUNDS: u32 = 3;
pub const DEFAULT_TIMEOUT: u64 = 900;
pub const MAX_TIMEOUT: u64 = 1800;
/// What goes back to the model of one review (the verification gate's `DETAIL_LIMIT`).
pub const DETAIL_LIMIT: usize = 12 * 1024;

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum Severity {
    Low,
    Medium,
    High,
    Critical,
}

impl Severity {
    /// The review levels, and SARIF's `error`/`warning`/`note`.
    pub fn parse(text: &str) -> Option<Self> {
        match text.trim().to_ascii_lowercase().as_str() {
            "critical" | "blocker" => Some(Self::Critical),
            "high" | "error" | "major" => Some(Self::High),
            "medium" | "warning" | "moderate" => Some(Self::Medium),
            "low" | "note" | "minor" | "info" => Some(Self::Low),
            _ => None,
        }
    }

    pub fn name(self) -> &'static str {
        match self {
            Self::Critical => "CRITICAL",
            Self::High => "HIGH",
            Self::Medium => "MEDIUM",
            Self::Low => "LOW",
        }
    }

    /// `review_block`: a level, or `any` (= low).
    pub fn parse_block(text: &str) -> Option<Self> {
        match text.trim().to_ascii_lowercase().as_str() {
            "any" => Some(Self::Low),
            other => Self::parse(other),
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Finding {
    pub severity: Severity,
    pub file: Option<String>,
    pub line: Option<u32>,
    pub title: String,
    pub detail: String,
}

impl Finding {
    pub fn location(&self) -> Option<String> {
        let file = self.file.as_ref()?;
        Some(match self.line {
            Some(line) => format!("{file}:{line}"),
            None => file.clone(),
        })
    }

    /// One line: `[HIGH] src/a.py:12 title`.
    pub fn headline(&self) -> String {
        let mut text = format!("[{}]", self.severity.name());
        if let Some(location) = self.location() {
            text.push(' ');
            text.push_str(&location);
        }
        text.push(' ');
        text.push_str(&self.title);
        text
    }

    /// The headline with the detail after it.
    pub fn full(&self) -> String {
        if self.detail.trim().is_empty() {
            self.headline()
        } else {
            format!("{} — {}", self.headline(), self.detail.trim())
        }
    }
}

/// The blocking findings' identity across rounds: wording and line numbers change
/// from round to round, the level and the file do not (0028 P4).
pub fn identity(findings: &[Finding]) -> BTreeSet<(Severity, Option<String>)> {
    findings
        .iter()
        .map(|finding| {
            (
                finding.severity,
                finding.file.as_ref().map(|file| file.replace('\\', "/")),
            )
        })
        .collect()
}

/// Counts per level, highest first, for the ledger and the user: `1 HIGH, 2 LOW`.
pub fn counts(findings: &[Finding]) -> String {
    let mut parts = Vec::new();
    for severity in [
        Severity::Critical,
        Severity::High,
        Severity::Medium,
        Severity::Low,
    ] {
        let n = findings.iter().filter(|f| f.severity == severity).count();
        if n > 0 {
            parts.push(format!("{n} 条 {}", severity.name()));
        }
    }
    parts.join("、")
}

// ---------------------------------------------------------------- parsing

/// The findings in a review's output; `Some(vec![])` means it found nothing, `None`
/// that the output follows none of the conventions. Tried in order: JSON (the whole
/// output, its last line, or the last ```json block), level markers (within the
/// Findings section when there is one), `NO_ISSUES` as the first line.
pub fn parse(output: &str) -> Option<Vec<Finding>> {
    if let Some(findings) = parse_json(output) {
        return Some(findings);
    }
    if let Some(findings) = parse_markers(output) {
        return Some(findings);
    }
    no_issues(output).then(Vec::new)
}

/// The first non-empty line, without whitespace and backticks, starts with `NO_ISSUES`.
pub fn no_issues(output: &str) -> bool {
    output
        .lines()
        .map(|line| line.trim_matches(|c: char| c.is_whitespace() || c == '`'))
        .find(|line| !line.is_empty())
        .is_some_and(|line| line.starts_with(NO_ISSUES))
}

fn parse_json(output: &str) -> Option<Vec<Finding>> {
    let trimmed = output.trim();
    let mut candidates: Vec<&str> = vec![trimmed];
    if let Some(last) = trimmed.lines().last().map(str::trim)
        && last.starts_with('{')
    {
        candidates.push(last);
    }
    if let Some(block) = last_json_block(trimmed) {
        candidates.push(block);
    }
    candidates.into_iter().find_map(|text| {
        serde_json::from_str::<Value>(text)
            .ok()
            .and_then(json_findings)
    })
}

/// The contents of the last ```json fenced block.
fn last_json_block(text: &str) -> Option<&str> {
    let lower = text.to_ascii_lowercase();
    let start = lower.rfind("```json")?;
    let body = &text[start + "```json".len()..];
    let end = body.find("```")?;
    Some(body[..end].trim())
}

fn json_findings(value: Value) -> Option<Vec<Finding>> {
    let items = value.get("findings")?.as_array()?;
    let text = |item: &Value, keys: &[&str]| {
        keys.iter()
            .find_map(|key| item.get(*key).and_then(Value::as_str))
            .map(|text| text.trim().to_owned())
            .filter(|text| !text.is_empty())
    };
    let mut out = Vec::new();
    for item in items {
        let severity = item
            .get("severity")
            .or_else(|| item.get("level"))
            .and_then(Value::as_str)
            .and_then(Severity::parse)
            .unwrap_or(Severity::Medium);
        let line = item.get("line").and_then(|line| match line {
            Value::Number(n) => n.as_u64().and_then(|n| u32::try_from(n).ok()),
            Value::String(s) => s.trim().parse().ok(),
            _ => None,
        });
        let detail = text(item, &["detail", "description", "body"]).unwrap_or_default();
        let title = text(item, &["title", "message", "summary"])
            .unwrap_or_else(|| crate::text::short(&detail, 120));
        out.push(Finding {
            severity,
            file: text(item, &["file", "path"]),
            line,
            title,
            detail,
        });
    }
    Some(out)
}

fn marker_re() -> &'static Regex {
    static RE: OnceLock<Regex> = OnceLock::new();
    RE.get_or_init(|| {
        Regex::new(
            r"(?i)^\s*(?:[-*+]\s+|\d+[.)]\s+)?(?:#{1,6}\s+)?(?:\*\*|__)?\s*\[(critical|high|medium|low)\]\s*(.*)$",
        )
        .expect("marker regex")
    })
}

fn heading_re() -> &'static Regex {
    static RE: OnceLock<Regex> = OnceLock::new();
    RE.get_or_init(|| Regex::new(r"^(#{1,6})\s+(.*?)\s*#*\s*$").expect("heading regex"))
}

fn location_re() -> &'static Regex {
    static RE: OnceLock<Regex> = OnceLock::new();
    RE.get_or_init(|| {
        Regex::new(r"([A-Za-z0-9_.~/\\-]*[A-Za-z0-9_-]\.[A-Za-z0-9]+):(\d+)")
            .expect("location regex")
    })
}

/// A heading's title, without numbering and emphasis.
fn heading_title(text: &str) -> String {
    text.trim_matches(|c: char| c == '*' || c == '_' || c.is_whitespace())
        .trim_start_matches(|c: char| c.is_ascii_digit() || c == '.' || c == ')')
        .trim()
        .to_lowercase()
}

fn is_findings_heading(title: &str) -> bool {
    ["findings", "问题", "发现的问题", "审查发现"]
        .iter()
        .any(|name| title.starts_with(name))
}

/// A section that repeats an earlier round ("Previous review"): its markers are history.
fn is_previous_heading(title: &str) -> bool {
    ["previous", "上一轮", "上次"]
        .iter()
        .any(|name| title.starts_with(name))
}

/// The lines to look for markers in: the Findings section up to the next heading of
/// its level or above when there is one, else everything but "Previous review"
/// sections; `true` when it was a Findings section.
fn marker_lines(output: &str) -> (Vec<&str>, bool) {
    let lines: Vec<&str> = output.lines().collect();
    let headings: Vec<(usize, usize, String)> = lines
        .iter()
        .enumerate()
        .filter_map(|(index, line)| {
            let captures = heading_re().captures(line)?;
            Some((index, captures[1].len(), heading_title(&captures[2])))
        })
        .collect();
    let section_end = |position: usize| {
        let (start, level, _) = &headings[position];
        headings
            .iter()
            .find(|(index, other, _)| index > start && other <= level)
            .map_or(lines.len(), |(index, _, _)| *index)
    };
    if let Some(position) = headings
        .iter()
        .position(|(_, _, title)| is_findings_heading(title))
    {
        let start = headings[position].0 + 1;
        return (lines[start..section_end(position)].to_vec(), true);
    }
    let mut skip = vec![false; lines.len()];
    for (position, (start, _, title)) in headings.iter().enumerate() {
        if is_previous_heading(title) {
            for flag in &mut skip[*start..section_end(position)] {
                *flag = true;
            }
        }
    }
    let kept = lines
        .iter()
        .zip(skip)
        .filter(|(_, skip)| !skip)
        .map(|(line, _)| *line)
        .collect();
    (kept, false)
}

/// `[HIGH] title`-style findings; `None` when there are none and no Findings section.
fn parse_markers(output: &str) -> Option<Vec<Finding>> {
    let (lines, section) = marker_lines(output);
    let mut out: Vec<Finding> = Vec::new();
    let mut body: Vec<&str> = Vec::new();
    let finish = |finding: Option<&mut Finding>, body: &mut Vec<&str>| {
        if let Some(finding) = finding {
            let rest = body.join("\n");
            let rest = rest.trim();
            if !rest.is_empty() {
                if !finding.detail.is_empty() {
                    finding.detail.push('\n');
                }
                finding.detail.push_str(rest);
            }
            if finding.file.is_none()
                && let Some(captures) = location_re().captures(&finding.detail)
            {
                finding.file = Some(captures[1].to_owned());
                finding.line = captures[2].parse().ok();
            }
        }
        body.clear();
    };
    for line in lines {
        if let Some(captures) = marker_re().captures(line) {
            finish(out.last_mut(), &mut body);
            let severity = Severity::parse(&captures[1]).unwrap_or(Severity::Medium);
            out.push(marker_finding(severity, &captures[2]));
        } else if !out.is_empty() {
            if heading_re().is_match(line) {
                // A heading ends the last finding's text.
                finish(out.last_mut(), &mut body);
                continue;
            }
            body.push(line);
        }
    }
    finish(out.last_mut(), &mut body);
    (section || !out.is_empty()).then_some(out)
}

/// One marker line: `**[HIGH] title**: `a.py:12` detail` or `[HIGH] a.py:12 - title - why`.
fn marker_finding(severity: Severity, rest: &str) -> Finding {
    let mut file = None;
    let mut line = None;
    if let Some(captures) = location_re().captures(rest) {
        file = Some(captures[1].to_owned());
        line = captures[2].parse().ok();
    }
    let rest = rest.trim();
    let (title, detail) = match rest.find("**").or_else(|| rest.find("__")) {
        Some(end) => (&rest[..end], &rest[end + 2..]),
        None => (rest, ""),
    };
    let title = without_location(title);
    let detail = without_location(detail);
    Finding {
        severity,
        file,
        line,
        // `**[HIGH]** a.py:3` alone: the location was all of it.
        title: if title.is_empty() {
            crate::text::short(&detail, 120)
        } else {
            title
        },
        detail,
    }
}

/// `text` without separators around it and without a location it starts with
/// (`a.py:12 - problem`, `` `a.py:12` the detail ``): the finding keeps that apart.
fn without_location(text: &str) -> String {
    let separators = |c: char| c.is_whitespace() || matches!(c, ':' | '：' | '-' | '—' | '`');
    let text = text.trim_matches(separators);
    if let Some(found) = location_re().find(text)
        && text[..found.start()]
            .trim_matches(|c: char| c == '`' || c.is_whitespace())
            .is_empty()
    {
        return text[found.end()..].trim_matches(separators).to_owned();
    }
    text.to_owned()
}

// ---------------------------------------------------------------- config

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Point {
    Done,
    Commit,
}

impl Point {
    pub fn name(self) -> &'static str {
        match self {
            Self::Done => "done",
            Self::Commit => "commit",
        }
    }
}

/// The `review_*` settings.
#[derive(Clone, Debug)]
pub struct ReviewConfig {
    /// Empty: no project review.
    pub commands: Vec<String>,
    pub at_done: bool,
    pub at_commit: bool,
    /// Findings at or above this level go back to the model.
    pub block: Severity,
    /// Reviews at the end of a task, the first included.
    pub max_rounds: u32,
    pub timeout: Duration,
    /// `review_instructions`: a file of the project's review criteria for the built-in reviewer.
    pub instructions: Option<String>,
}

impl ReviewConfig {
    pub fn from_json(file: &Value) -> Self {
        let commands = file
            .get("review_commands")
            .and_then(Value::as_array)
            .map(|commands| {
                commands
                    .iter()
                    .filter_map(Value::as_str)
                    .map(str::trim)
                    .filter(|command| !command.is_empty())
                    .map(str::to_owned)
                    .collect()
            })
            .unwrap_or_default();
        let (at_done, at_commit) = match file
            .get("review_when")
            .and_then(Value::as_str)
            .map(|when| when.trim().to_ascii_lowercase())
            .as_deref()
        {
            Some("commit") => (false, true),
            Some("both") => (true, true),
            _ => (true, false),
        };
        Self {
            commands,
            at_done,
            at_commit,
            block: file
                .get("review_block")
                .and_then(Value::as_str)
                .and_then(Severity::parse_block)
                .unwrap_or(Severity::High),
            max_rounds: file
                .get("review_max_rounds")
                .and_then(Value::as_u64)
                .map_or(DEFAULT_ROUNDS, |rounds| rounds.clamp(1, 10) as u32),
            timeout: Duration::from_secs(
                file.get("review_timeout_seconds")
                    .and_then(Value::as_u64)
                    .map_or(DEFAULT_TIMEOUT, |seconds| seconds.clamp(1, MAX_TIMEOUT)),
            ),
            instructions: file
                .get("review_instructions")
                .and_then(Value::as_str)
                .map(str::trim)
                .filter(|path| !path.is_empty())
                .map(str::to_owned),
        }
    }
}

// ---------------------------------------------------------------- protection

/// Files the review depends on, with their content when cove started.
struct Protected {
    path: PathBuf,
    shown: String,
    hash: Option<Vec<u8>>,
}

fn file_hash(path: &Path) -> Option<Vec<u8>> {
    std::fs::read(path)
        .ok()
        .map(|data| Sha256::digest(&data).to_vec())
}

/// `path` without `.` and with `..` applied, for files that may not exist.
fn lexical(path: &Path) -> PathBuf {
    let mut out = PathBuf::new();
    for component in path.components() {
        match component {
            std::path::Component::CurDir => {}
            std::path::Component::ParentDir => {
                out.pop();
            }
            other => out.push(other),
        }
    }
    out
}

fn resolve(root: &Path, path: &str) -> PathBuf {
    let path = Path::new(path.trim());
    let joined = if path.is_absolute() {
        path.to_path_buf()
    } else {
        root.join(path)
    };
    joined.canonicalize().unwrap_or_else(|_| lexical(&joined))
}

/// The workspace files a review command runs: words of it that name an existing
/// file below `root` (`bash eng/review.sh`, `python tools/review.py`, `./x.sh --all`).
pub fn command_files(root: &Path, command: &str) -> Vec<PathBuf> {
    let root_key = crate::paths::key(root);
    let mut out = Vec::new();
    let line = shell::parse(command, Dialect::for_command(command));
    for simple in line.commands() {
        for word in simple.argv() {
            let word = word.trim_matches(|c| c == '"' || c == '\'');
            if word.is_empty() || word.starts_with('-') {
                continue;
            }
            let path = resolve(root, word);
            let key = crate::paths::key(&path);
            if path.is_file() && key.starts_with(&format!("{root_key}/")) && !out.contains(&path) {
                out.push(path);
            }
        }
    }
    out
}

// ---------------------------------------------------------------- running

/// The project review command(s), shared by the end-of-task loop and the pre-commit gate.
pub struct CommandReview {
    root: PathBuf,
    pub config: ReviewConfig,
    protected: Vec<Protected>,
    workspace: std::sync::Arc<crate::session::workspace::Workspace>,
    ledger: Option<PathBuf>,
    progress: Option<std::sync::Arc<Progress>>,
    /// Where `COVE_REVIEW_PREVIOUS` and `COVE_REVIEW_REQUEST` point; removed with it.
    files: Mutex<Option<tempfile::TempDir>>,
    /// The workspace fingerprint of the last end-of-task review that passed: a commit
    /// of the same content needs no second run.
    passed: Mutex<Option<u64>>,
}

/// One run of the review commands.
pub struct Run {
    pub verdict: Verdict,
    /// What the commands printed, for the next round's `COVE_REVIEW_PREVIOUS`.
    pub output: String,
}

impl CommandReview {
    pub fn new(
        root: PathBuf,
        config: ReviewConfig,
        workspace: std::sync::Arc<crate::session::workspace::Workspace>,
    ) -> Self {
        let mut paths: Vec<PathBuf> = config
            .commands
            .iter()
            .flat_map(|command| command_files(&root, command))
            .collect();
        for extra in [
            Some(crate::config::trust::PROJECT_CONFIG.to_owned()),
            config.instructions.clone(),
        ]
        .into_iter()
        .flatten()
        {
            let path = resolve(&root, &extra);
            if path.is_file() && !paths.contains(&path) {
                paths.push(path);
            }
        }
        let root_key = crate::paths::key(&root);
        let protected = paths
            .into_iter()
            .map(|path| {
                let key = crate::paths::key(&path);
                let shown = key
                    .strip_prefix(&format!("{root_key}/"))
                    .map(str::to_owned)
                    .unwrap_or_else(|| crate::paths::display(&path));
                Protected {
                    hash: file_hash(&path),
                    shown,
                    path,
                }
            })
            .collect();
        Self {
            root,
            config,
            protected,
            workspace,
            ledger: None,
            progress: None,
            files: Mutex::new(None),
            passed: Mutex::new(None),
        }
    }

    pub fn with_ledger(mut self, path: PathBuf) -> Self {
        self.ledger = Some(path);
        self
    }

    pub fn with_progress(mut self, progress: std::sync::Arc<Progress>) -> Self {
        self.progress = Some(progress);
        self
    }

    pub fn workspace(&self) -> &crate::session::workspace::Workspace {
        &self.workspace
    }

    /// The protected files, relative to the workspace.
    pub fn protected(&self) -> Vec<String> {
        self.protected.iter().map(|p| p.shown.clone()).collect()
    }

    /// Whether a write to `path` (as a tool got it) would change the review.
    pub fn protects(&self, path: &str) -> bool {
        let key = crate::paths::key(&resolve(&self.root, path));
        self.protected
            .iter()
            .any(|protected| crate::paths::key(&protected.path) == key)
    }

    /// The protected files changed or removed since cove started, if any.
    pub fn tampered(&self) -> Vec<String> {
        self.protected
            .iter()
            .filter(|protected| file_hash(&protected.path) != protected.hash)
            .map(|protected| protected.shown.clone())
            .collect()
    }

    pub fn note_passed(&self, fingerprint: Option<u64>) {
        if let Ok(mut passed) = self.passed.lock() {
            *passed = fingerprint;
        }
    }

    /// The end-of-task review already passed this exact content.
    pub fn passed_now(&self) -> bool {
        let Ok(Some(now)) = self.workspace.fingerprint() else {
            return false;
        };
        self.passed.lock().is_ok_and(|passed| *passed == Some(now))
    }

    async fn say(&self, text: String) {
        if let Some(progress) = &self.progress {
            progress.line(text, false).await;
        }
    }

    /// A file in this review's private directory with `text`.
    fn file(&self, name: &str, text: &str) -> Option<PathBuf> {
        let mut files = self.files.lock().ok()?;
        if files.is_none() {
            *files = tempfile::Builder::new()
                .prefix("cove-review-")
                .tempdir()
                .ok();
        }
        let path = files.as_ref()?.path().join(name);
        std::fs::write(&path, text).ok()?;
        Some(path)
    }

    #[allow(clippy::too_many_arguments)]
    fn record(
        &self,
        command: &str,
        point: Point,
        round: u32,
        verdict: &str,
        findings: &[Finding],
        exit_code: Option<i32>,
        duration_ms: u128,
    ) {
        let Some(path) = &self.ledger else {
            return;
        };
        let level = |severity| findings.iter().filter(|f| f.severity == severity).count();
        let line = serde_json::json!({
            "time": crate::session::now(),
            "kind": "review",
            "cwd": self.root,
            "command": command,
            "point": point.name(),
            "round": round,
            "status": verdict,
            "critical": level(Severity::Critical),
            "high": level(Severity::High),
            "medium": level(Severity::Medium),
            "low": level(Severity::Low),
            "exit_code": exit_code,
            "duration_ms": duration_ms,
        });
        let _ = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(path)
            .and_then(|mut file| writeln!(file, "{line}"));
    }

    /// Runs every command once; `previous` is the last round's output.
    pub async fn run(
        &self,
        point: Point,
        round: u32,
        previous: Option<&str>,
        request: &str,
        cancel: CancellationToken,
    ) -> Result<Run> {
        let tampered = self.tampered();
        if !tampered.is_empty() {
            let reason = format!(
                "审查依赖的文件在 cove 启动后被改动或删除：{}；本次审查作废。确认改动无误后重启 cove",
                tampered.join("、")
            );
            for command in &self.config.commands {
                self.record(command, point, round, "tampered", &[], None, 0);
            }
            return Ok(Run {
                verdict: Verdict::Skipped(reason),
                output: String::new(),
            });
        }
        let mut env = vec![
            ("COVE_REVIEW_POINT", point.name().to_owned()),
            ("COVE_REVIEW_ROUND", round.to_string()),
        ];
        if let Some(path) = self.file("request.txt", request) {
            env.push((
                "COVE_REVIEW_REQUEST",
                crate::paths::plain(&path).to_string_lossy().into_owned(),
            ));
        }
        if let Some(previous) = previous
            && let Some(path) = self.file("previous.txt", previous)
        {
            env.push((
                "COVE_REVIEW_PREVIOUS",
                crate::paths::plain(&path).to_string_lossy().into_owned(),
            ));
        }
        let mut findings = Vec::new();
        let mut output = String::new();
        for command in &self.config.commands {
            self.say(format!("[审查] 第 {round} 轮：$ {command}")).await;
            let started = Instant::now();
            // Review commands are often test suites: they run while they keep working.
            let result = crate::process::shell_with_env(
                &self.root,
                command,
                crate::process::Limit::watched(self.config.timeout, crate::process::IDLE_LIMIT),
                cancel.clone(),
                &env,
            )
            .await;
            let duration_ms = started.elapsed().as_millis();
            let seconds = duration_ms as f64 / 1000.0;
            let ran = match result {
                Ok(ran) => ran,
                Err(Error::Cancelled) => return Err(Error::Cancelled),
                Err(err) => {
                    self.record(command, point, round, "not_run", &[], None, duration_ms);
                    return Ok(Run {
                        verdict: Verdict::Skipped(format!("{command} 无法运行：{err}")),
                        output,
                    });
                }
            };
            if let Some(reason) = &ran.stalled {
                self.record(command, point, round, "stalled", &[], None, duration_ms);
                return Ok(Run {
                    verdict: Verdict::Skipped(format!("{command} 卡住，已中止：{reason}")),
                    output,
                });
            }
            if ran.timed_out {
                self.record(command, point, round, "timed_out", &[], None, duration_ms);
                return Ok(Run {
                    verdict: Verdict::Skipped(format!(
                        "{command} 超过 {} 秒没有结束",
                        self.config.timeout.as_secs()
                    )),
                    output,
                });
            }
            if ran.code != Some(0) {
                self.record(command, point, round, "not_run", &[], ran.code, duration_ms);
                let tail = crate::text::visible(&crate::text::short(
                    crate::process::tail_bytes(ran.stderr.trim(), 300),
                    300,
                ));
                let code = ran
                    .code
                    .map_or_else(|| "被终止".to_owned(), |code| format!("退出码 {code}"));
                let reason = if tail.is_empty() {
                    format!("{command} {code}")
                } else {
                    format!("{command} {code}：{tail}")
                };
                return Ok(Run {
                    verdict: Verdict::Skipped(reason),
                    output,
                });
            }
            if !output.is_empty() {
                output.push('\n');
            }
            output.push_str(&ran.stdout);
            match parse(&ran.stdout) {
                Some(found) => {
                    let status = if found.is_empty() { "passed" } else { "issues" };
                    self.record(command, point, round, status, &found, Some(0), duration_ms);
                    self.say(format!(
                        "[审查] {command} → {}（{seconds:.0}s）",
                        if found.is_empty() {
                            "无问题".to_owned()
                        } else {
                            counts(&found)
                        }
                    ))
                    .await;
                    findings.extend(found);
                }
                None => {
                    self.record(command, point, round, "unparsed", &[], Some(0), duration_ms);
                    self.say(format!(
                        "[审查] {command} → 输出格式无法识别（{seconds:.0}s）"
                    ))
                    .await;
                    return Ok(Run {
                        verdict: Verdict::Unparsed(ran.stdout),
                        output,
                    });
                }
            }
        }
        let verdict = if findings.is_empty() {
            Verdict::Passed
        } else {
            Verdict::Issues(findings)
        };
        Ok(Run { verdict, output })
    }

    /// The findings that go back to the model under `review_block`.
    pub fn blocking(&self, findings: &[Finding]) -> (Vec<Finding>, Vec<Finding>) {
        findings
            .iter()
            .cloned()
            .partition(|finding| finding.severity >= self.config.block)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The shape of a real review result (Overview / Findings / Previous review /
    /// Summary), with the content made up: no issue left, the previous round's LOW
    /// findings repeated under Previous review.
    const MARKDOWN_CLEAN: &str = "## Overview\n\nReviewed the uncommitted change to the pricing service.\n\n### Findings\n\nNo blocking or non-blocking issues found in this change.\n\n### Previous review\n\n- **[LOW] Log message wording**: `src/Pricing/Service.cs:40` — fixed.\n- **[LOW] Missing XML doc**: `src/Pricing/Api.cs:12` — fixed.\n\n### Summary\n\nReady to merge.\n";

    const MARKDOWN_ISSUES: &str = "## Overview\n\nThe change adds weighted pricing.\n\n### Findings\n\n1. **[HIGH] Division by zero on empty input**: `src/Pricing/Service.cs:88`\n   `Average` divides by the item count without checking it.\n   Suggest returning 0.\n2. **[MEDIUM] MR description and code disagree**: the description mentions caching.\n3. **[LOW] Naming**: `src/Pricing/Api.cs:5` uses an abbreviation.\n\n### Previous review\n\n- **[HIGH] Old finding**: `src/Old.cs:1` — fixed.\n\n### Summary\n\nOne blocking issue.\n";

    #[test]
    fn markdown_findings_come_from_the_findings_section_only() {
        assert_eq!(parse(MARKDOWN_CLEAN), Some(vec![]));
        let findings = parse(MARKDOWN_ISSUES).unwrap();
        assert_eq!(findings.len(), 3, "{findings:?}");
        assert_eq!(findings[0].severity, Severity::High);
        assert_eq!(findings[0].title, "Division by zero on empty input");
        assert_eq!(findings[0].file.as_deref(), Some("src/Pricing/Service.cs"));
        assert_eq!(findings[0].line, Some(88));
        assert!(
            findings[0].detail.contains("Suggest returning 0"),
            "{findings:?}"
        );
        assert_eq!(findings[1].severity, Severity::Medium);
        assert_eq!(findings[1].file, None);
        assert_eq!(findings[2].severity, Severity::Low);
        assert_eq!(findings[2].line, Some(5));
        // The HIGH under Previous review is history, not a finding.
        assert!(findings.iter().all(|f| f.title != "Old finding"));
    }

    #[test]
    fn markers_without_a_findings_section_skip_previous_review() {
        let text = "[HIGH] src/a.py:12 - divides by zero - empty list\n[low] b.py:3 - name\n\n## Previous review\n**[HIGH] gone**: `c.py:1`\n";
        let findings = parse(text).unwrap();
        assert_eq!(findings.len(), 2, "{findings:?}");
        assert_eq!(findings[0].title, "divides by zero - empty list");
        assert_eq!(findings[0].file.as_deref(), Some("src/a.py"));
        assert_eq!(findings[1].severity, Severity::Low);
        // Chinese headings.
        let text = "## 概览\n改了一处。\n\n## 问题\n\n- **[CRITICAL] 注入**：`app/db.py:7`\n\n## 上一轮\n- **[LOW] 旧的**\n";
        let findings = parse(text).unwrap();
        assert_eq!(findings.len(), 1);
        assert_eq!(findings[0].severity, Severity::Critical);
        assert_eq!(findings[0].title, "注入");
        assert_eq!(findings[0].location().as_deref(), Some("app/db.py:7"));
    }

    #[test]
    fn json_findings_whole_last_line_or_last_block() {
        let whole = r#"{"findings":[{"severity":"high","file":"src/a.py","line":12,"title":"空列表时除零","detail":"……"}],"summary":"x"}"#;
        let findings = parse(whole).unwrap();
        assert_eq!(findings[0].severity, Severity::High);
        assert_eq!(findings[0].headline(), "[HIGH] src/a.py:12 空列表时除零");
        // SARIF levels, a missing level, a string line number.
        let sarif = r#"{"findings":[{"level":"error","title":"a"},{"severity":"warning","title":"b"},{"severity":"note","title":"c"},{"title":"d","line":"7","path":"x.rs"}]}"#;
        let levels: Vec<_> = parse(sarif).unwrap().iter().map(|f| f.severity).collect();
        assert_eq!(
            levels,
            [
                Severity::High,
                Severity::Medium,
                Severity::Low,
                Severity::Medium
            ]
        );
        assert_eq!(parse(sarif).unwrap()[3].line, Some(7));
        assert_eq!(
            parse(r#"{"findings": [], "summary": "无问题"}"#),
            Some(vec![])
        );
        // Progress lines first, the JSON on the last line.
        assert_eq!(parse("reviewing...\n{\"findings\": []}\n"), Some(vec![]));
        // The last ```json block wins over an earlier example.
        let fenced = "Example:\n```json\n{\"findings\":[{\"severity\":\"low\",\"title\":\"example\"}]}\n```\nResult:\n```json\n{\"findings\":[{\"severity\":\"critical\",\"title\":\"real\"}]}\n```\n";
        let findings = parse(fenced).unwrap();
        assert_eq!(findings.len(), 1);
        assert_eq!(findings[0].title, "real");
    }

    #[test]
    fn no_issues_and_unparsed_output() {
        assert_eq!(parse("NO_ISSUES"), Some(vec![]));
        assert_eq!(parse("```\nNO_ISSUES\n```\n"), Some(vec![]));
        assert_eq!(parse("  `NO_ISSUES` nothing to report\n"), Some(vec![]));
        assert_eq!(parse("Looks mostly fine, a few nits."), None);
        assert_eq!(parse(""), None);
        // A NO_ISSUES that is not the first line is not a verdict.
        assert_eq!(parse("Some text\nNO_ISSUES"), None);
    }

    #[test]
    fn identity_ignores_wording_and_lines() {
        let a = parse("[HIGH] src/a.py:12 - divides by zero").unwrap();
        let b = parse("**[HIGH] Zero total quantity**: `src/a.py:14`").unwrap();
        assert_eq!(identity(&a), identity(&b));
        let c = parse("[HIGH] src/b.py:1 - other").unwrap();
        assert_ne!(identity(&a), identity(&c));
        assert_eq!(
            counts(&parse(MARKDOWN_ISSUES).unwrap()),
            "1 条 HIGH、1 条 MEDIUM、1 条 LOW"
        );
    }

    #[test]
    fn config_defaults_and_limits() {
        let config = ReviewConfig::from_json(&serde_json::json!({}));
        assert!(config.commands.is_empty());
        assert!(config.at_done && !config.at_commit);
        assert_eq!(config.block, Severity::High);
        assert_eq!(config.max_rounds, DEFAULT_ROUNDS);
        assert_eq!(config.timeout, Duration::from_secs(DEFAULT_TIMEOUT));
        let config = ReviewConfig::from_json(&serde_json::json!({
            "review_commands": ["python tools/review.py", " "],
            "review_when": "both",
            "review_block": "any",
            "review_max_rounds": 0,
            "review_timeout_seconds": 99999,
            "review_instructions": "eng/SKILL.md"
        }));
        assert_eq!(config.commands, ["python tools/review.py"]);
        assert!(config.at_done && config.at_commit);
        assert_eq!(config.block, Severity::Low);
        assert_eq!(config.max_rounds, 1);
        assert_eq!(config.timeout, Duration::from_secs(MAX_TIMEOUT));
        assert_eq!(config.instructions.as_deref(), Some("eng/SKILL.md"));
        let config = ReviewConfig::from_json(&serde_json::json!({"review_when": "commit"}));
        assert!(!config.at_done && config.at_commit);
    }

    #[test]
    fn review_scripts_need_approval_in_auto_mode() {
        // Why trusted review commands run without asking: auto mode would refuse them
        // with nobody to ask (`-p`), and the loop would never run.
        use crate::safety::permission::classify::auto_approvable;
        for command in [
            "bash eng/ai-review-local.sh --uncommitted",
            "python tools/review.py",
            "./tools/review.sh",
        ] {
            assert!(
                !auto_approvable(command, Dialect::for_command(command)),
                "{command}"
            );
        }
    }

    #[test]
    fn the_files_a_command_runs_are_protected() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        std::fs::create_dir_all(root.join("tools")).unwrap();
        std::fs::create_dir_all(root.join("eng")).unwrap();
        std::fs::write(root.join("tools/review.py"), "print('NO_ISSUES')").unwrap();
        std::fs::write(root.join("eng/ai-review-local.sh"), "echo NO_ISSUES").unwrap();
        std::fs::write(root.join("x.sh"), "echo NO_ISSUES").unwrap();
        let names = |command: &str| -> Vec<String> {
            command_files(&root, command)
                .iter()
                .map(|p| {
                    crate::paths::key(p)
                        .strip_prefix(&format!("{}/", crate::paths::key(&root)))
                        .unwrap()
                        .to_owned()
                })
                .collect()
        };
        assert_eq!(names("python tools/review.py"), ["tools/review.py"]);
        assert_eq!(
            names("bash eng/ai-review-local.sh --uncommitted"),
            ["eng/ai-review-local.sh"]
        );
        assert_eq!(names("./x.sh --all"), ["x.sh"]);
        assert!(names("python -m reviewer").is_empty());

        std::fs::write(root.join(".cove.json"), "{}").unwrap();
        let data = tempfile::tempdir().unwrap();
        let store = crate::session::Store::open(data.path()).unwrap();
        let workspace = std::sync::Arc::new(
            crate::session::workspace::Workspace::new(root.clone(), store).unwrap(),
        );
        let review = CommandReview::new(
            root.clone(),
            ReviewConfig::from_json(
                &serde_json::json!({"review_commands": ["python tools/review.py"]}),
            ),
            workspace,
        );
        assert_eq!(review.protected(), ["tools/review.py", ".cove.json"]);
        assert!(review.protects("tools/review.py"));
        assert!(review.protects("./tools/../tools/review.py"));
        assert!(review.protects(&root.join(".cove.json").to_string_lossy()));
        assert!(!review.protects("pricing.py"));
        assert!(review.tampered().is_empty());
        std::fs::remove_file(root.join("tools/review.py")).unwrap();
        assert_eq!(review.tampered(), ["tools/review.py"]);
    }
}
