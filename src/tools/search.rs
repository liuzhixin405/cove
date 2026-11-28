//! `grep` (ripgrep when installed, else built in) and `glob` over workspace files.

use std::{
    collections::HashSet,
    path::{Path, PathBuf},
    sync::{Arc, OnceLock},
    time::{Duration, Instant},
};

use tokio_util::sync::CancellationToken;

use crate::domain::{Error, Result};

/// Lines in one grep result; the rest is summarized.
pub const GREP_LINES: usize = 100;
pub const GREP_MAX_CONTEXT: usize = 10;
/// Paths in one glob result.
pub const GLOB_LIMIT: usize = 200;
/// Entries one walk visits before it stops.
const SCAN_LIMIT: usize = 50_000;
/// How long a search with `include_ignored` may run: ignored directories (node_modules,
/// build output) can be huge. VS Code Copilot's grep_search uses the same 20 s.
pub const IGNORED_TIME_LIMIT: Duration = Duration::from_secs(20);
const FILE_LIMIT: u64 = 8 * 1024 * 1024;

pub struct GrepOptions {
    pub pattern: String,
    pub include: Option<String>,
    pub ignore_case: bool,
    pub context: usize,
    pub files_only: bool,
    /// Also search what `.gitignore`, `.ignore` and git excludes leave out.
    pub include_ignored: bool,
}

/// Why a search stopped before covering everything below its path.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Stop {
    Entries,
    Time,
}

impl Stop {
    fn note(self) -> String {
        let after = match self {
            Self::Entries => format!("scanning {SCAN_LIMIT} entries"),
            Self::Time => format!("{} s", IGNORED_TIME_LIMIT.as_secs()),
        };
        format!("... search stopped after {after}; results are incomplete, narrow path or pattern")
    }
}

/// What is left of a walk: an entry count, and a deadline when ignored files are searched.
struct Budget {
    left: usize,
    deadline: Option<Instant>,
}

impl Budget {
    fn new(include_ignored: bool) -> Self {
        Self::with(SCAN_LIMIT, include_ignored.then_some(IGNORED_TIME_LIMIT))
    }

    fn with(entries: usize, time: Option<Duration>) -> Self {
        Self {
            left: entries,
            deadline: time.map(|time| Instant::now() + time),
        }
    }

    /// Counts one entry; `Some` once the walk has to stop before it.
    fn spend(&mut self) -> Option<Stop> {
        if self.left == 0 {
            return Some(Stop::Entries);
        }
        self.left -= 1;
        self.deadline
            .is_some_and(|deadline| Instant::now() >= deadline)
            .then_some(Stop::Time)
    }
}

/// Matching lines (or files) and, when the search ended early, why.
#[derive(Debug, Default)]
pub struct Found {
    pub lines: Vec<String>,
    pub stop: Option<Stop>,
}

const IGNORED_HINT: &str =
    " (files excluded by .gitignore were skipped; include_ignored=true searches them)";

/// Whether ignore rules may have hidden files from a search of `base`: an ignore file
/// in `base` or a directory above it within `root`, or entries in `.git/info/exclude`.
/// Deeper ignore files and global excludes are not looked for: this only words an
/// empty result, so it has to stay cheap.
fn ignore_rules_apply(root: &Path, base: &Path) -> bool {
    let excluded = std::fs::read_to_string(root.join(".git").join("info").join("exclude"))
        .is_ok_and(|text| {
            text.lines()
                .any(|line| !line.trim().is_empty() && !line.trim_start().starts_with('#'))
        });
    excluded
        || base
            .ancestors()
            .take_while(|dir| dir.starts_with(root))
            .any(|dir| dir.join(".gitignore").is_file() || dir.join(".ignore").is_file())
}

/// `No files found` / `No matches found`, pointing at `include_ignored` when ignore
/// rules may be the reason, and saying why the search stopped early.
fn nothing(
    what: &str,
    root: &Path,
    base: &Path,
    include_ignored: bool,
    stop: Option<Stop>,
) -> String {
    let hint = stop.is_none() && !include_ignored && ignore_rules_apply(root, base);
    let mut out = format!("{what}{}", if hint { IGNORED_HINT } else { "" });
    if let Some(stop) = stop {
        out.push('\n');
        out.push_str(&stop.note());
    }
    out
}

/// `ripgrep` on PATH, looked up once.
pub fn ripgrep() -> Option<&'static Path> {
    static RG: OnceLock<Option<PathBuf>> = OnceLock::new();
    RG.get_or_init(|| which::which("rg").ok()).as_deref()
}

/// Paths are shown relative to the workspace with forward slashes, as read/edit accept them.
pub fn display(root: &Path, path: &Path) -> String {
    let relative = path.strip_prefix(root).unwrap_or(path);
    let text = relative.to_string_lossy().replace('\\', "/");
    if text.is_empty() { ".".into() } else { text }
}

/// `path` relative to `base`, an ancestor that may differ in case (Windows), `/`-separated.
fn below(base: &Path, path: &Path) -> String {
    match path.strip_prefix(base) {
        Ok(_) => display(base, path),
        Err(_) => {
            let skip = base.components().count();
            let relative: PathBuf = path.components().skip(skip).collect();
            display(Path::new(""), &relative)
        }
    }
}

/// Matches `pattern`: without `/` against the basename, otherwise against
/// the whole relative path with `**` crossing directories.
pub struct PathPattern {
    matcher: globset::GlobMatcher,
    basename: bool,
}

impl PathPattern {
    pub fn new(pattern: &str) -> Result<Self> {
        let pattern = pattern.trim().replace('\\', "/");
        let pattern = pattern.strip_prefix("./").unwrap_or(&pattern).to_owned();
        if pattern.is_empty() {
            return Err(Error::Tool("pattern must not be empty".into()));
        }
        let matcher = globset::GlobBuilder::new(&pattern)
            .literal_separator(true)
            .backslash_escape(false)
            .build()
            .map_err(|err| Error::Tool(format!("invalid glob: {err}")))?
            .compile_matcher();
        Ok(Self {
            matcher,
            basename: !pattern.contains('/'),
        })
    }

    pub fn matches(&self, relative: &str) -> bool {
        if self.basename {
            let name = relative.rsplit('/').next().unwrap_or(relative);
            self.matcher.is_match(name)
        } else {
            self.matcher.is_match(relative)
        }
    }
}

/// What an overlay stack changes for a search: the disk
/// entries of `hidden` do not count, and `files` are searched in memory instead.
#[derive(Default)]
pub struct Shadow {
    /// Workspace-relative paths (`/`-separated, as written) any layer wrote or deleted.
    pub hidden: Vec<String>,
    keys: HashSet<String>,
    /// Files as the layers hold them (absolute path, content), ordered by path.
    pub files: Vec<(PathBuf, Arc<Vec<u8>>)>,
}

impl Shadow {
    pub fn new(hidden: Vec<String>, files: Vec<(PathBuf, Arc<Vec<u8>>)>) -> Self {
        let keys = hidden
            .iter()
            .map(|path| crate::paths::key(Path::new(path)))
            .collect();
        Self {
            hidden,
            keys,
            files,
        }
    }

    /// Whether the disk entry `path` (below `root`) is replaced by the layers.
    pub fn hides(&self, root: &Path, path: &Path) -> bool {
        !self.keys.is_empty()
            && path
                .strip_prefix(root)
                .is_ok_and(|relative| self.keys.contains(&crate::paths::key(relative)))
    }

    /// Layered files at or below `base`, without what ignore files hide unless
    /// `include_ignored`.
    fn files_under<'a>(
        &'a self,
        root: &'a Path,
        base: &'a Path,
        include_ignored: bool,
    ) -> impl Iterator<Item = &'a (PathBuf, Arc<Vec<u8>>)> + 'a {
        let base = crate::paths::key(base);
        self.files.iter().filter(move |(path, _)| {
            under(&base, path) && (include_ignored || !ignored(root, path))
        })
    }

    /// Whether `path` is one of the layered files (compared as [`crate::paths::key`]).
    fn holds(&self, path: &Path) -> bool {
        let key = crate::paths::key(path);
        self.files
            .iter()
            .any(|(file, _)| crate::paths::key(file) == key)
    }
}

/// Whether `path` is `base` (a [`crate::paths::key`]) or below it, by path components.
pub(crate) fn under(base: &str, path: &Path) -> bool {
    let key = crate::paths::key(path);
    key == base
        || base.is_empty()
        || key
            .strip_prefix(base)
            .is_some_and(|rest| rest.starts_with('/'))
}

/// Whether ignore files would leave out `path`, a file below `root` that need not exist
/// on disk: `.ignore` and `.gitignore` of its directories (nearest first), then
/// `.git/info/exclude`. Global excludes are not read.
pub(crate) fn ignored(root: &Path, path: &Path) -> bool {
    use ignore::{Match, gitignore::GitignoreBuilder};
    let Some(parent) = path.parent() else {
        return false;
    };
    let matched = |dir: &Path, files: &[PathBuf]| -> Option<bool> {
        let mut builder = GitignoreBuilder::new(dir);
        let mut any = false;
        for file in files.iter().filter(|file| file.is_file()) {
            builder.add(file);
            any = true;
        }
        let matcher = builder.build().ok().filter(|_| any)?;
        match matcher.matched_path_or_any_parents(path, false) {
            Match::Ignore(_) => Some(true),
            Match::Whitelist(_) => Some(false),
            Match::None => None,
        }
    };
    for dir in parent.ancestors().take_while(|dir| dir.starts_with(root)) {
        // Later files win within one directory: `.ignore` over `.gitignore`.
        if let Some(ignored) = matched(dir, &[dir.join(".gitignore"), dir.join(".ignore")]) {
            return ignored;
        }
    }
    matched(root, &[root.join(".git").join("info").join("exclude")]).unwrap_or(false)
}

/// A `--glob` override that excludes exactly the workspace-relative `path`.
fn exclude_glob(path: &str) -> String {
    let mut out = String::from("--glob=!/");
    for c in path.chars() {
        if matches!(c, '*' | '?' | '[' | ']' | '{' | '}' | '\\') {
            out.push('[');
            out.push(c);
            out.push(']');
        } else {
            out.push(c);
        }
    }
    out
}

/// Files under `base` (respecting ignore files, without `.git`), each confined by `confine`.
pub(crate) fn walk(base: &Path) -> ignore::Walk {
    walker(base, false)
}

/// `walk`, or with `include_ignored` every file except `.git` and protected directories.
fn walker(base: &Path, include_ignored: bool) -> ignore::Walk {
    let mut walk = ignore::WalkBuilder::new(base);
    if include_ignored {
        walk.standard_filters(false);
    }
    walk.hidden(false).follow_links(false).require_git(false);
    walk.filter_entry(|entry| {
        entry.file_name() != ".git" && !crate::safety::protected::is_protected(entry.path())
    });
    walk.build()
}

/// `glob`: relative matches from `base`, shown relative to `root`. With an overlay
/// stack's `shadow`, hidden disk entries are skipped and layered files are listed after
/// the walk's.
pub fn glob(
    root: &Path,
    base: &Path,
    pattern: &str,
    include_ignored: bool,
    shadow: Option<&Shadow>,
    cancel: &CancellationToken,
    confine: &dyn Fn(&Path) -> Option<PathBuf>,
) -> Result<String> {
    let budget = Budget::new(include_ignored);
    glob_within(
        root,
        base,
        pattern,
        include_ignored,
        shadow,
        budget,
        cancel,
        confine,
    )
}

#[allow(clippy::too_many_arguments)]
fn glob_within(
    root: &Path,
    base: &Path,
    pattern: &str,
    include_ignored: bool,
    shadow: Option<&Shadow>,
    mut budget: Budget,
    cancel: &CancellationToken,
    confine: &dyn Fn(&Path) -> Option<PathBuf>,
) -> Result<String> {
    let pattern = PathPattern::new(pattern)?;
    let mut shown = Vec::new();
    let mut more = 0;
    let mut stop = None;
    let mut take = |path: &Path, shown: &mut Vec<String>| {
        if !pattern.matches(&display(base, path)) {
            return;
        }
        if shown.len() >= GLOB_LIMIT {
            more += 1;
            return;
        }
        if let Some(full) = confine(path) {
            shown.push(display(root, &full));
        }
    };
    for entry in walker(base, include_ignored) {
        if cancel.is_cancelled() {
            return Err(Error::Cancelled);
        }
        stop = budget.spend();
        if stop.is_some() {
            break;
        }
        let Ok(entry) = entry else { continue };
        if !entry.file_type().is_some_and(|kind| kind.is_file()) {
            continue;
        }
        if shadow.is_some_and(|shadow| shadow.hides(root, entry.path())) {
            continue;
        }
        take(entry.path(), &mut shown);
    }
    if let Some(shadow) = shadow {
        for (path, _) in shadow.files_under(root, base, include_ignored) {
            take(path, &mut shown);
        }
    }
    if shown.is_empty() {
        return Ok(nothing("No files found", root, base, include_ignored, stop));
    }
    let mut out = shown.join("\n");
    if more > 0 {
        out.push_str(&format!("\n... and {more} more"));
    }
    if let Some(stop) = stop {
        out.push('\n');
        out.push_str(&stop.note());
    }
    Ok(out)
}

/// `grep` through ripgrep: returns every output line (rg stops each file at the cap).
/// `excludes` are workspace-relative files rg leaves out (an overlay's changed paths).
pub async fn ripgrep_search(
    rg: &Path,
    root: &Path,
    target: &str,
    options: &GrepOptions,
    excludes: &[String],
    cancel: &CancellationToken,
) -> Result<Found> {
    let mut command = tokio::process::Command::new(rg);
    command.args([
        "-n",
        "--no-heading",
        "--color=never",
        "--hidden",
        "--no-require-git",
        "--max-columns=500",
        "--max-columns-preview",
        "--glob=!.git",
    ]);
    if options.include_ignored {
        command.arg("--no-ignore");
    }
    // Later globs take precedence in rg, so the include goes first: an exclusion
    // after it cannot be put back by it (a layered or protected file would reappear).
    if let Some(include) = &options.include {
        command.arg(format!("--glob={include}"));
    }
    for dir in crate::safety::protected::below(root) {
        command.arg(format!("--glob=!{dir}"));
        command.arg(format!("--glob=!{dir}/**"));
    }
    for path in excludes {
        command.arg(exclude_glob(path));
    }
    command.arg(format!("--max-count={}", GREP_LINES + 1));
    if options.ignore_case {
        command.arg("-i");
    }
    if options.files_only {
        command.arg("-l");
    } else if options.context > 0 {
        command.args(["-C", &options.context.to_string()]);
    }
    command
        .args(["-e", &options.pattern, "--", target])
        .current_dir(root)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .kill_on_drop(true);
    let mut child = command.spawn()?;
    let stdout = child
        .stdout
        .take()
        .ok_or_else(|| Error::Tool("rg stdout not piped".into()))?;
    let stderr = child
        .stderr
        .take()
        .ok_or_else(|| Error::Tool("rg stderr not piped".into()))?;
    // Drain stderr alongside, keeping a bounded tail, or rg could block on a full pipe.
    let stderr_task = tokio::spawn(async move {
        crate::process::capture_limited(stderr, 64 * 1024)
            .await
            .map(|(kept, _)| kept)
    });
    // Stream matches instead of buffering all of rg's output: `--max-count` limits each
    // file, not the total, so a broad pattern in a large repository could otherwise
    // collect millions of lines. The result note is a lower bound ("at least N more"),
    // which stopping early never breaks.
    use tokio::io::AsyncBufReadExt;
    let mut lines = tokio::io::BufReader::new(stdout).lines();
    let mut collected: Vec<String> = Vec::new();
    let deadline = options
        .include_ignored
        .then(|| tokio::time::Instant::now() + IGNORED_TIME_LIMIT);
    let timer = async move {
        match deadline {
            Some(deadline) => tokio::time::sleep_until(deadline).await,
            None => std::future::pending().await,
        }
    };
    tokio::pin!(timer);
    let mut stop = None;
    let status = loop {
        let line = tokio::select! {
            _ = cancel.cancelled() => return Err(Error::Cancelled),
            _ = &mut timer => {
                stop = Some(Stop::Time);
                break None;
            }
            line = lines.next_line() => line?,
        };
        match line {
            Some(line) => {
                collected.push(line);
                if collected.len() > GREP_LINES * 10 {
                    break None;
                }
            }
            None => break Some(child.wait().await?),
        }
    };
    let status = match status {
        Some(status) => Some(status),
        None => {
            let _ = child.kill().await;
            None
        }
    };
    let stderr = stderr_task
        .await
        .map_err(|err| Error::Tool(format!("rg stderr task failed: {err}")))??;
    match status.and_then(|status| status.code()) {
        Some(0) => {}
        Some(1) => return Ok(Found::default()),
        // Stopped at the global cap or the time limit (killed above), or exit 2 with
        // output: some files were unreadable; the matches collected still answer.
        _ if !collected.is_empty() || stop.is_some() => {}
        _ => {
            return Err(Error::Tool(format!(
                "rg: {}",
                String::from_utf8_lossy(&stderr).trim()
            )));
        }
    }
    let lines = collected
        .iter()
        .map(|line| {
            let line = line.trim_end_matches('\r');
            let line = line
                .strip_prefix("./")
                .or_else(|| line.strip_prefix(".\\"))
                .unwrap_or(line);
            // rg prints native separators; show the same form glob does.
            if cfg!(windows) {
                normalize_rg_path(line)
            } else {
                line.to_owned()
            }
        })
        .collect();
    Ok(Found { lines, stop })
}

/// `src\a.rs:3:text` -> `src/a.rs:3:text` (only the path part).
fn normalize_rg_path(line: &str) -> String {
    let split = line
        .char_indices()
        .find(|&(index, c)| {
            (c == ':' || c == '-')
                && line[index + 1..]
                    .split([':', '-'])
                    .next()
                    .is_some_and(|n| !n.is_empty() && n.bytes().all(|b| b.is_ascii_digit()))
        })
        .map_or(line.len(), |(index, _)| index);
    format!("{}{}", line[..split].replace('\\', "/"), &line[split..])
}

/// The built-in search, in ripgrep's output format; stops one line past the cap. With an
/// overlay stack's `shadow`, hidden disk entries are skipped and layered files are
/// searched in memory after the walk.
pub fn builtin_grep(
    root: &Path,
    base: &Path,
    options: &GrepOptions,
    shadow: Option<&Shadow>,
    cancel: &CancellationToken,
    confine: &dyn Fn(&Path) -> Option<PathBuf>,
) -> Result<Found> {
    let budget = Budget::new(options.include_ignored);
    let mut found = grep_within(root, base, options, shadow, budget, cancel, confine)?;
    if let Some(shadow) = shadow {
        let printed = !found.lines.is_empty();
        found
            .lines
            .extend(grep_layers(root, base, options, shadow, printed, confine)?);
    }
    Ok(found)
}

/// Lines of the layered files at or below `base`, in ripgrep's format; `printed` says
/// lines came before (a context search separates the groups with `--`).
pub fn grep_layers(
    root: &Path,
    base: &Path,
    options: &GrepOptions,
    shadow: &Shadow,
    printed: bool,
    confine: &dyn Fn(&Path) -> Option<PathBuf>,
) -> Result<Vec<String>> {
    let regex = grep_regex(options)?;
    let include = grep_include(options)?;
    let mut collector = Collector {
        lines: Vec::new(),
        printed,
    };
    let single = shadow.holds(base);
    for (path, bytes) in shadow.files_under(root, base, options.include_ignored || single) {
        if collector.full() {
            break;
        }
        if !single
            && include
                .as_ref()
                .is_some_and(|include| !include.matches(&below(base, path)))
        {
            continue;
        }
        if let Some(full) = confine(path) {
            collector.bytes(root, &full, bytes, &regex, options);
        }
    }
    Ok(collector.lines)
}

fn grep_regex(options: &GrepOptions) -> Result<regex::Regex> {
    regex::RegexBuilder::new(&options.pattern)
        .case_insensitive(options.ignore_case)
        .size_limit(1024 * 1024)
        .build()
        .map_err(|err| Error::Tool(format!("invalid regex: {err}")))
}

fn grep_include(options: &GrepOptions) -> Result<Option<PathPattern>> {
    options.include.as_deref().map(PathPattern::new).transpose()
}

fn grep_within(
    root: &Path,
    base: &Path,
    options: &GrepOptions,
    shadow: Option<&Shadow>,
    mut budget: Budget,
    cancel: &CancellationToken,
    confine: &dyn Fn(&Path) -> Option<PathBuf>,
) -> Result<Found> {
    let regex = grep_regex(options)?;
    let include = grep_include(options)?;
    let mut collector = Collector {
        lines: Vec::new(),
        printed: false,
    };
    if shadow.is_some_and(|shadow| shadow.hides(root, base)) {
        // A file the layers changed or deleted: only `grep_layers` searches it.
        return Ok(Found::default());
    }
    if base.is_file() {
        collector.file(root, base, &regex, options);
        return Ok(Found {
            lines: collector.lines,
            stop: None,
        });
    }
    let mut stop = None;
    for entry in walker(base, options.include_ignored) {
        if cancel.is_cancelled() {
            return Err(Error::Cancelled);
        }
        if collector.full() {
            break;
        }
        stop = budget.spend();
        if stop.is_some() {
            break;
        }
        let Ok(entry) = entry else { continue };
        if !entry.file_type().is_some_and(|kind| kind.is_file()) {
            continue;
        }
        if shadow.is_some_and(|shadow| shadow.hides(root, entry.path())) {
            continue;
        }
        if include
            .as_ref()
            .is_some_and(|include| !include.matches(&display(base, entry.path())))
        {
            continue;
        }
        if let Some(full) = confine(entry.path()) {
            collector.file(root, &full, &regex, options);
        }
    }
    Ok(Found {
        lines: collector.lines,
        stop,
    })
}

struct Collector {
    lines: Vec<String>,
    printed: bool,
}

impl Collector {
    fn full(&self) -> bool {
        self.lines.len() > GREP_LINES
    }

    fn file(&mut self, root: &Path, path: &Path, regex: &regex::Regex, options: &GrepOptions) {
        if std::fs::metadata(path).map_or(true, |meta| meta.len() > FILE_LIMIT) {
            return;
        }
        let Ok(bytes) = std::fs::read(path) else {
            return;
        };
        self.bytes(root, path, &bytes, regex, options);
    }

    /// Searches `bytes` as the content of `path`.
    fn bytes(
        &mut self,
        root: &Path,
        path: &Path,
        bytes: &[u8],
        regex: &regex::Regex,
        options: &GrepOptions,
    ) {
        if bytes[..bytes.len().min(8192)].contains(&0) {
            return;
        }
        let text = match crate::tools::text_encoding::sniff(bytes) {
            crate::tools::text_encoding::TextKind::Utf8 => {
                String::from_utf8_lossy(bytes).into_owned()
            }
            crate::tools::text_encoding::TextKind::Legacy => {
                match crate::tools::text_encoding::decode_gbk(bytes) {
                    Some((text, _)) => text,
                    None => String::from_utf8_lossy(bytes).into_owned(),
                }
            }
            _ => return,
        };
        let shown = display(root, path);
        let context = options.context;
        let mut before: std::collections::VecDeque<(usize, &str)> = Default::default();
        let (mut last_printed, mut after_left) = (0, 0);
        for (index, line) in text.lines().enumerate() {
            let number = index + 1;
            if self.full() {
                return;
            }
            if regex.is_match(line) {
                if options.files_only {
                    self.lines.push(shown);
                    return;
                }
                if context > 0 {
                    let first = number - before.len();
                    if self.printed && (last_printed == 0 || first > last_printed + 1) {
                        self.lines.push("--".into());
                    }
                    for (n, held) in before.drain(..) {
                        self.lines.push(row(&shown, '-', n, held));
                    }
                    after_left = context;
                }
                self.lines.push(row(&shown, ':', number, line));
                last_printed = number;
                self.printed = true;
            } else if after_left > 0 {
                self.lines.push(row(&shown, '-', number, line));
                last_printed = number;
                after_left -= 1;
            } else if context > 0 {
                if before.len() == context {
                    before.pop_front();
                }
                before.push_back((number, line));
            }
        }
    }
}

fn row(path: &str, separator: char, number: usize, line: &str) -> String {
    format!(
        "{path}{separator}{number}{separator}{}",
        crate::text::clip(line.trim_end_matches('\r'), 500, " [... omitted]")
    )
}

/// Joins grep lines, cutting at `GREP_LINES`. `counted` means `lines` is everything rg
/// printed, so the note gives a lower bound. `root` and `base` word an empty result.
pub fn grep_result(
    found: Found,
    counted: bool,
    options: &GrepOptions,
    root: &Path,
    base: &Path,
) -> String {
    let Found { lines, stop } = found;
    if lines.is_empty() {
        return nothing(
            "No matches found",
            root,
            base,
            options.include_ignored,
            stop,
        );
    }
    let mut out = joined(&lines, counted, options);
    if let Some(stop) = stop {
        out.push('\n');
        out.push_str(&stop.note());
    }
    out
}

fn joined(lines: &[String], counted: bool, options: &GrepOptions) -> String {
    if lines.len() <= GREP_LINES {
        return lines.join("\n");
    }
    let hint = "narrow the pattern, or use include/path";
    let note = if counted {
        let unit = if options.files_only {
            "files"
        } else if options.context > 0 {
            "lines"
        } else {
            "matches"
        };
        format!(
            "\n... at least {} more {unit} not shown ({hint})",
            lines.len() - GREP_LINES
        )
    } else {
        format!("\n... more matches not shown ({hint})")
    };
    format!("{}{note}", lines[..GREP_LINES].join("\n"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn patterns_without_a_slash_match_basenames_and_double_star_crosses_directories() {
        let pattern = PathPattern::new("*.rs").unwrap();
        assert!(pattern.matches("src/deep/a.rs"));
        let pattern = PathPattern::new("src/**/*.ts").unwrap();
        assert!(pattern.matches("src/a.ts"));
        assert!(pattern.matches("src/a/b/c.ts"));
        assert!(!pattern.matches("lib/a.ts"));
        let pattern = PathPattern::new("src/*.ts").unwrap();
        assert!(!pattern.matches("src/a/b.ts"));
    }

    #[test]
    fn builtin_grep_prints_context_groups_like_ripgrep() {
        let dir = tempfile::tempdir().unwrap();
        let text: String = (1..=12).map(|n| format!("line {n}\n")).collect();
        std::fs::write(dir.path().join("a.txt"), text).unwrap();
        let options = GrepOptions {
            pattern: "line (2|10)$".into(),
            include: Some("*.txt".into()),
            ignore_case: false,
            context: 1,
            files_only: false,
            include_ignored: false,
        };
        let found = builtin_grep(
            dir.path(),
            dir.path(),
            &options,
            None,
            &CancellationToken::new(),
            &|path| Some(path.to_path_buf()),
        )
        .unwrap();
        assert_eq!(found.stop, None);
        assert_eq!(
            found.lines,
            [
                "a.txt-1-line 1",
                "a.txt:2:line 2",
                "a.txt-3-line 3",
                "--",
                "a.txt-9-line 9",
                "a.txt:10:line 10",
                "a.txt-11-line 11"
            ]
        );
    }

    #[test]
    fn long_results_name_how_many_more_exist() {
        let lines: Vec<String> = (0..130).map(|n| format!("f:{n}:x")).collect();
        let options = GrepOptions {
            pattern: "x".into(),
            include: None,
            ignore_case: false,
            context: 0,
            files_only: false,
            include_ignored: false,
        };
        let dir = tempfile::tempdir().unwrap();
        let found = Found { lines, stop: None };
        let out = grep_result(found, true, &options, dir.path(), dir.path());
        assert!(out.ends_with(
            "... at least 30 more matches not shown (narrow the pattern, or use include/path)"
        ));
        assert_eq!(out.lines().count(), 101);
    }

    #[test]
    fn searches_that_stop_early_say_so_and_empty_ones_point_at_include_ignored() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        for n in 0..5 {
            std::fs::write(root.join(format!("f{n}.txt")), "x\n").unwrap();
        }
        let cancel = CancellationToken::new();
        let confine = |path: &Path| Some(path.to_path_buf());
        // The walk's first entry is `root` itself.
        let budget = Budget::with(4, None);
        let out = glob_within(
            &root, &root, "*.txt", false, None, budget, &cancel, &confine,
        )
        .unwrap();
        assert_eq!(out.lines().count(), 4, "{out}");
        assert!(out.ends_with(&Stop::Entries.note()), "{out}");
        let budget = Budget::with(SCAN_LIMIT, Some(Duration::ZERO));
        let out = glob_within(&root, &root, "*.rs", true, None, budget, &cancel, &confine).unwrap();
        assert_eq!(
            out,
            "No files found\n... search stopped after 20 s; results are incomplete, narrow path or pattern"
        );

        let mut options = GrepOptions {
            pattern: "x".into(),
            include: None,
            ignore_case: false,
            context: 0,
            files_only: true,
            include_ignored: false,
        };
        let found = grep_within(
            &root,
            &root,
            &options,
            None,
            Budget::with(3, None),
            &cancel,
            &confine,
        )
        .unwrap();
        assert_eq!((found.lines.len(), found.stop), (2, Some(Stop::Entries)));
        let out = grep_result(found, false, &options, &root, &root);
        assert!(out.ends_with("narrow path or pattern"), "{out}");

        // Without ignore files an empty result is just that; with one it names the parameter.
        options.pattern = "absent".into();
        let found = builtin_grep(&root, &root, &options, None, &cancel, &confine).unwrap();
        assert_eq!(
            grep_result(found, false, &options, &root, &root),
            "No matches found"
        );
        std::fs::create_dir(root.join("sub")).unwrap();
        std::fs::write(root.join(".gitignore"), "*.log\n").unwrap();
        let sub = root.join("sub");
        let found = builtin_grep(&root, &sub, &options, None, &cancel, &confine).unwrap();
        assert_eq!(
            grep_result(found, false, &options, &root, &sub),
            format!("No matches found{IGNORED_HINT}")
        );
        options.include_ignored = true;
        let found = builtin_grep(&root, &sub, &options, None, &cancel, &confine).unwrap();
        assert_eq!(
            grep_result(found, false, &options, &root, &sub),
            "No matches found"
        );
    }

    #[test]
    fn ripgrep_paths_use_forward_slashes() {
        assert_eq!(normalize_rg_path("src\\a-b.rs:3:x-1"), "src/a-b.rs:3:x-1");
        assert_eq!(normalize_rg_path("src\\a.rs-4-ctx"), "src/a.rs-4-ctx");
        assert_eq!(normalize_rg_path("src\\only.rs"), "src/only.rs");
    }
}
