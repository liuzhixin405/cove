//! Write sets of plan items: the files a `todo` item may change.
//!
//! An entry is a workspace-relative path or glob, optionally with a `::symbol` suffix that
//! is shown to the model but not enforced: enforcement is per file. `execute_plan` rejects
//! a plan whose independent items may write the same file ([`overlaps`]); while an item
//! with a write set runs, its tools refuse writes outside it and file-generating commands,
//! and the files its shell commands changed outside it are recorded ([`ItemScope`]).
//! Items without a write set are not constrained.

use std::{
    collections::{BTreeMap, BTreeSet, HashMap},
    path::{Component, Path, PathBuf},
    sync::{
        Arc, Mutex, OnceLock,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    time::{Duration, Instant, SystemTime},
};

use globset::{GlobBuilder, GlobMatcher};
use serde_json::Value;
use tokio_util::sync::CancellationToken;

use crate::domain::{Error, Result};

/// Entries one item may declare.
pub const MAX_ENTRIES: usize = 64;
/// Files listed when two globs are compared against the workspace.
const MAX_LISTED: usize = 50_000;
/// Bound of one `git status` around a shell command.
const STATUS_TIMEOUT: Duration = Duration::from_secs(15);
/// Shell command spans kept to tell which commands ran at the same time.
const MAX_SHELLS: usize = 256;

/// The file part of an entry: `src/a.rs::Foo::bar` is `src/a.rs`.
pub fn file_part(entry: &str) -> &str {
    entry
        .split_once("::")
        .map_or(entry, |(file, _)| file)
        .trim()
}

/// `/` separators, no leading `./`.
fn normalize(path: &str) -> String {
    let mut text = path.trim().replace('\\', "/");
    while let Some(rest) = text.strip_prefix("./") {
        text = rest.to_owned();
    }
    while text.contains("//") {
        text = text.replace("//", "/");
    }
    text
}

/// The comparable form of a normalized relative path: lower case on Windows.
fn key(path: &str) -> String {
    if cfg!(windows) {
        path.to_lowercase()
    } else {
        path.to_owned()
    }
}

fn is_glob(text: &str) -> bool {
    text.contains(['*', '?', '[', '{'])
}

#[derive(Clone, Debug)]
enum Pattern {
    /// The key of one file, or of a directory written without a trailing `/`: it covers
    /// the files below it too (nothing can be below a file).
    File(String),
    /// The key of the glob as written, and its matcher.
    Glob(String, GlobMatcher),
}

impl Pattern {
    fn matches(&self, file_key: &str) -> bool {
        match self {
            Self::File(key) => {
                key == file_key
                    || file_key
                        .strip_prefix(key.as_str())
                        .is_some_and(|rest| rest.starts_with('/'))
            }
            Self::Glob(_, matcher) => matcher.is_match(file_key),
        }
    }
}

/// One item's declared write set.
#[derive(Clone, Debug, Default)]
pub struct WriteSet {
    declared: Vec<String>,
    patterns: Vec<Pattern>,
}

impl PartialEq for WriteSet {
    fn eq(&self, other: &Self) -> bool {
        self.declared == other.declared
    }
}

impl WriteSet {
    /// Checks and compiles declared entries: relative, inside the workspace, valid globs.
    /// A directory, with or without a trailing `/`, means everything below it.
    pub fn parse(entries: &[String]) -> Result<Self> {
        if entries.len() > MAX_ENTRIES {
            return Err(Error::Tool(format!(
                "at most {MAX_ENTRIES} writes entries per item"
            )));
        }
        let mut set = Self::default();
        for entry in entries {
            let file = normalize(file_part(entry));
            let absolute = file.starts_with('/')
                || Path::new(&file).is_absolute()
                || file.as_bytes().get(1) == Some(&b':');
            if file.is_empty() || absolute || file.split('/').any(|part| part == "..") {
                return Err(Error::Tool(format!(
                    "writes entry {entry:?} must be a workspace-relative path or glob without .."
                )));
            }
            let file = match file.strip_suffix('/') {
                Some(dir) => format!("{dir}/**"),
                None => file,
            };
            let file_key = key(&file);
            let pattern = if is_glob(&file) {
                let glob = GlobBuilder::new(&file_key)
                    .literal_separator(true)
                    .build()
                    .map_err(|err| {
                        Error::Tool(format!("writes entry {entry:?} is not a valid glob: {err}"))
                    })?;
                Pattern::Glob(file_key, glob.compile_matcher())
            } else {
                Pattern::File(file_key)
            };
            set.declared.push(entry.trim().to_owned());
            set.patterns.push(pattern);
        }
        Ok(set)
    }

    /// The entries as declared, `::symbol` suffixes included.
    pub fn declared(&self) -> &[String] {
        &self.declared
    }

    pub fn is_empty(&self) -> bool {
        self.declared.is_empty()
    }

    /// The file parts of the entries (paths and globs), without repeats.
    pub fn files(&self) -> Vec<String> {
        let mut seen = BTreeSet::new();
        self.declared
            .iter()
            .map(|entry| normalize(file_part(entry)))
            .filter(|file| seen.insert(file.clone()))
            .collect()
    }

    fn has_globs(&self) -> bool {
        self.patterns
            .iter()
            .any(|pattern| matches!(pattern, Pattern::Glob(..)))
    }

    /// Whether `relative` (workspace-relative, either separator) is in the set.
    pub fn covers(&self, relative: &str) -> bool {
        let file = key(&normalize(relative));
        self.patterns.iter().any(|pattern| pattern.matches(&file))
    }

    /// The files both sets may write: a file of one the other covers, two identical globs,
    /// or an existing file (`existing`, workspace-relative) both globs match. Files that
    /// two different globs could match only once created are not seen.
    pub fn overlap<'f>(&self, other: &Self, existing: &dyn Fn() -> &'f [String]) -> Vec<String> {
        let mut shared = BTreeSet::new();
        for (mine, theirs) in [(self, other), (other, self)] {
            for (pattern, entry) in mine.patterns.iter().zip(&mine.declared) {
                if let Pattern::File(file) = pattern
                    && theirs.patterns.iter().any(|other| other.matches(file))
                {
                    shared.insert(normalize(file_part(entry)));
                }
            }
        }
        let mut files = None;
        // A file or directory entry against a glob: the existing files both cover.
        for (mine, theirs) in [(self, other), (other, self)] {
            for pattern in &mine.patterns {
                let Pattern::File(_) = pattern else {
                    continue;
                };
                for glob in &theirs.patterns {
                    let Pattern::Glob(_, matcher) = glob else {
                        continue;
                    };
                    for file in files.get_or_insert_with(existing).iter() {
                        let file_key = key(file);
                        if pattern.matches(&file_key) && matcher.is_match(&file_key) {
                            shared.insert(file.clone());
                        }
                    }
                }
            }
        }
        if self.has_globs() && other.has_globs() {
            for (pattern, entry) in self.patterns.iter().zip(&self.declared) {
                let Pattern::Glob(literal, mine) = pattern else {
                    continue;
                };
                for theirs in &other.patterns {
                    let Pattern::Glob(other_literal, theirs) = theirs else {
                        continue;
                    };
                    if literal == other_literal {
                        shared.insert(normalize(file_part(entry)));
                        continue;
                    }
                    for file in files.get_or_insert_with(existing).iter() {
                        let file_key = key(file);
                        if mine.is_match(&file_key) && theirs.is_match(&file_key) {
                            shared.insert(file.clone());
                        }
                    }
                }
            }
        }
        shared.into_iter().collect()
    }

    /// The `::symbol`s this set names in `file` (workspace-relative); `None` when it may
    /// change the whole file: an entry for it without a symbol, or a glob covering it.
    pub fn symbols_for(&self, file: &str) -> Option<BTreeSet<String>> {
        let file_key = key(&normalize(file));
        let mut symbols = BTreeSet::new();
        for (pattern, entry) in self.patterns.iter().zip(&self.declared) {
            if !pattern.matches(&file_key) {
                continue;
            }
            match (pattern, entry.split_once("::")) {
                (Pattern::File(_), Some((_, symbol))) if !symbol.trim().is_empty() => {
                    symbols.insert(symbol.trim().to_owned());
                }
                _ => return None,
            }
        }
        (!symbols.is_empty()).then_some(symbols)
    }

    /// `a.rs, b.rs::f` for messages, at most `max` entries.
    pub fn summary(&self, max: usize) -> String {
        let mut text = self
            .declared
            .iter()
            .take(max)
            .cloned()
            .collect::<Vec<_>>()
            .join(", ");
        if self.declared.len() > max {
            text.push_str(&format!(" (+{} more)", self.declared.len() - max));
        }
        text
    }
}

/// The files of the workspace at `root` (git-ignored ones left out), workspace-relative.
pub fn workspace_files(root: &Path) -> Vec<String> {
    let mut walk = ignore::WalkBuilder::new(root);
    walk.hidden(false).follow_links(false).require_git(false);
    walk.filter_entry(|entry| entry.file_name() != ".git");
    walk.build()
        .flatten()
        .filter(|entry| entry.file_type().is_some_and(|kind| kind.is_file()))
        .take(MAX_LISTED)
        .filter_map(|entry| {
            let relative = entry.path().strip_prefix(root).ok()?;
            Some(slashed(relative))
        })
        .collect()
}

fn slashed(relative: &Path) -> String {
    relative
        .components()
        .map(|part| part.as_os_str().to_string_lossy().into_owned())
        .collect::<Vec<_>>()
        .join("/")
}

/// One unit of a plan for the overlap check.
pub struct Unit<'a> {
    pub id: &'a str,
    pub depends: &'a [String],
    pub writes: Option<&'a WriteSet>,
}

/// Two items that may run at the same time and write the same files.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Overlap {
    pub a: String,
    pub b: String,
    pub files: Vec<String>,
}

/// For each unit, the units it (transitively) depends on, by index.
fn ancestors(units: &[Unit]) -> Vec<BTreeSet<usize>> {
    let index: HashMap<&str, usize> = units
        .iter()
        .enumerate()
        .map(|(position, unit)| (unit.id, position))
        .collect();
    units
        .iter()
        .map(|unit| {
            let mut found = BTreeSet::new();
            let mut stack: Vec<&str> = unit.depends.iter().map(String::as_str).collect();
            while let Some(id) = stack.pop() {
                if let Some(&position) = index.get(id)
                    && found.insert(position)
                {
                    stack.extend(units[position].depends.iter().map(String::as_str));
                }
            }
            found
        })
        .collect()
}

/// Index pairs `(a, b)`, `a < b`, of units with no dependency path between them.
pub fn independent_pairs(units: &[Unit]) -> Vec<(usize, usize)> {
    let ancestors = ancestors(units);
    let mut pairs = Vec::new();
    for a in 0..units.len() {
        for b in a + 1..units.len() {
            if !ancestors[a].contains(&b) && !ancestors[b].contains(&a) {
                pairs.push((a, b));
            }
        }
    }
    pairs
}

/// Independent units that both declared writes and share files. Globs are compared
/// against the files existing under `root`, listed once and only when needed.
pub fn overlaps(units: &[Unit], root: &Path) -> Vec<Overlap> {
    let listed: OnceLock<Vec<String>> = OnceLock::new();
    let existing = || listed.get_or_init(|| workspace_files(root)).as_slice();
    independent_pairs(units)
        .into_iter()
        .filter_map(|(a, b)| {
            let files = units[a].writes?.overlap(units[b].writes?, &existing);
            (!files.is_empty()).then(|| Overlap {
                a: units[a].id.to_owned(),
                b: units[b].id.to_owned(),
                files,
            })
        })
        .collect()
}

/// A dependency the overlay schedule adds: `later` builds on `earlier`'s layer
/// because both write `files` and the files are not split between them by `::symbol`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Implicit {
    pub later: usize,
    pub earlier: usize,
    pub files: Vec<String>,
}

/// Whether two sets share `file` only through distinct `::symbol`s of it.
fn split_by_symbols(a: &WriteSet, b: &WriteSet, file: &str) -> bool {
    if is_glob(file) {
        return false;
    }
    match (a.symbols_for(file), b.symbols_for(file)) {
        (Some(mine), Some(theirs)) => mine.is_disjoint(&theirs),
        _ => false,
    }
}

/// The overlay schedule's extra dependencies: independent units that share files run
/// side by side only when every shared file is split between them by distinct
/// `::symbol`s (their layers are merged afterwards); any other overlap makes the later
/// unit (plan order) depend on the earlier one, so its layer stacks on that one's.
/// Dependencies added earlier count for the pairs after them; the graph stays acyclic
/// because an edge is only added between units with no path between them.
pub fn implicit_depends(units: &[Unit], root: &Path) -> Vec<Implicit> {
    let listed: OnceLock<Vec<String>> = OnceLock::new();
    let existing = || listed.get_or_init(|| workspace_files(root)).as_slice();
    let mut depends: Vec<Vec<String>> = units.iter().map(|unit| unit.depends.to_vec()).collect();
    let mut found = Vec::new();
    for later in 0..units.len() {
        for earlier in 0..later {
            let (Some(a), Some(b)) = (units[earlier].writes, units[later].writes) else {
                continue;
            };
            let current: Vec<Unit> = units
                .iter()
                .zip(&depends)
                .map(|(unit, depends)| Unit {
                    id: unit.id,
                    depends,
                    writes: unit.writes,
                })
                .collect();
            let ancestors = ancestors(&current);
            if ancestors[later].contains(&earlier) || ancestors[earlier].contains(&later) {
                continue;
            }
            let files: Vec<String> = a
                .overlap(b, &existing)
                .into_iter()
                .filter(|file| !split_by_symbols(a, b, file))
                .collect();
            if files.is_empty() {
                continue;
            }
            depends[later].push(units[earlier].id.to_owned());
            found.push(Implicit {
                later,
                earlier,
                files,
            });
        }
    }
    found
}

/// `path` as a tool received it, relative to the canonical `root` with `/` separators;
/// `None` outside the workspace or with `..` (the tool itself refuses those).
pub fn relative(root: &Path, path: &str) -> Option<String> {
    let given = Path::new(path.trim());
    if path.trim().is_empty()
        || given
            .components()
            .any(|part| matches!(part, Component::ParentDir))
    {
        return None;
    }
    let joined = if given.is_absolute() {
        given.to_path_buf()
    } else {
        root.join(given)
    };
    let mut missing = Vec::new();
    let mut existing = joined.as_path();
    let mut full = loop {
        match existing.canonicalize() {
            Ok(found) => break found,
            Err(_) => {
                missing.push(existing.file_name()?.to_owned());
                existing = existing.parent()?;
            }
        }
    };
    for name in missing.iter().rev() {
        full.push(name);
    }
    let rest = full.strip_prefix(root).ok()?;
    let text = slashed(rest);
    (!text.is_empty()).then_some(text)
}

/// The path argument of a tool that writes one workspace file.
pub fn write_target<'a>(name: &str, input: &'a Value) -> Option<&'a str> {
    let keys: &[&str] = match name {
        // The same aliases the write/edit tools accept (tools/mod.rs).
        "write" | "edit" => &["path", "file_path", "filePath"],
        "draw_image" => &["outputPath"],
        _ => return None,
    };
    keys.iter()
        .find_map(|key| input.get(*key).and_then(Value::as_str))
}

/// Size and modification time of each file `git status` lists as changed or untracked
/// (`None`: listed but gone). Two of them differ where files changed in between.
pub type Tree = HashMap<String, Option<(u64, SystemTime)>>;

/// One shell command in [`PlanScopes`]: number, item, start, end (`None` while it runs).
type ShellSpan = (u64, String, Instant, Option<Instant>);

/// What the write-scoped items of one plan run share.
pub struct PlanScopes {
    root: PathBuf,
    /// Each scoped item's id and write set.
    items: Mutex<Vec<(String, WriteSet)>>,
    /// Files written with the file tools: key, item, when.
    written: Mutex<Vec<(String, String, Instant)>>,
    /// Shell commands of the scoped items ([`ShellRun`]): number, item, start, end
    /// (`None` while it runs).
    shells: Mutex<Vec<ShellSpan>>,
    next_shell: AtomicU64,
    /// Items without writes may run at the same time as the scoped ones; what changes
    /// during a shell command may then be theirs.
    unscoped_parallel: AtomicBool,
    /// The workspace below the repository top (`git rev-parse --show-prefix`);
    /// `None` outside a git repository, where shell changes are not checked.
    prefix: tokio::sync::OnceCell<Option<String>>,
}

impl PlanScopes {
    pub fn new(root: &Path) -> Arc<Self> {
        Arc::new(Self {
            root: root.canonicalize().unwrap_or_else(|_| root.to_path_buf()),
            items: Mutex::default(),
            written: Mutex::default(),
            shells: Mutex::default(),
            next_shell: AtomicU64::new(0),
            unscoped_parallel: AtomicBool::new(false),
            prefix: tokio::sync::OnceCell::new(),
        })
    }

    /// Says whether items without writes may run at the same time as the scoped ones.
    pub fn with_unscoped_parallel(self: Arc<Self>, parallel: bool) -> Arc<Self> {
        self.unscoped_parallel.store(parallel, Ordering::Relaxed);
        self
    }

    fn end_shell(&self, number: u64) {
        if let Ok(mut shells) = self.shells.lock()
            && let Some(shell) = shells.iter_mut().find(|shell| shell.0 == number)
        {
            shell.3.get_or_insert_with(Instant::now);
        }
    }

    /// Whether a shell command of another item than `run`'s ran at some point between
    /// `run`'s start and now.
    fn overlapped(&self, run: &ShellRun) -> bool {
        let now = Instant::now();
        let Ok(shells) = self.shells.lock() else {
            return true;
        };
        shells.iter().any(|(_, item, start, end)| {
            *item != run.item && *start <= now && end.is_none_or(|end| end >= run.started)
        })
    }

    /// The scope of item `id`, which may write `writes`.
    pub fn item(self: &Arc<Self>, id: &str, writes: WriteSet) -> Arc<ItemScope> {
        if let Ok(mut items) = self.items.lock() {
            items.push((id.to_owned(), writes.clone()));
        }
        Arc::new(ItemScope {
            id: id.to_owned(),
            writes,
            plan: self.clone(),
            record: Mutex::default(),
        })
    }

    async fn prefix(&self, cancel: &CancellationToken) -> Option<String> {
        self.prefix
            .get_or_init(|| async {
                let output = crate::process::git::Git::new(&self.root)
                    .args(["rev-parse", "--show-prefix"])
                    .timeout(STATUS_TIMEOUT)
                    .output(cancel)
                    .await
                    .ok()?;
                output
                    .ok
                    .then(|| output.stdout_text().trim().replace('\\', "/"))
            })
            .await
            .clone()
    }

    /// The changed and untracked files under the workspace now; `None` outside git or
    /// when git fails.
    async fn tree(&self, cancel: &CancellationToken) -> Option<Tree> {
        let prefix = self.prefix(cancel).await?;
        let output = crate::process::git::Git::new(&self.root)
            .args([
                "status",
                "--porcelain=v1",
                "-z",
                "--untracked-files=all",
                "--no-renames",
                "--ignore-submodules=all",
                "--",
                ".",
            ])
            .timeout(STATUS_TIMEOUT)
            .output(cancel)
            .await
            .ok()?;
        if !output.ok {
            return None;
        }
        let root = self.root.clone();
        let stdout = output.stdout;
        tokio::task::spawn_blocking(move || {
            let mut tree = Tree::new();
            for entry in stdout.split(|byte| *byte == 0) {
                // `XY path`: two status letters and a space.
                let Some(path) = entry.get(3..).filter(|path| !path.is_empty()) else {
                    continue;
                };
                let path = String::from_utf8_lossy(path);
                let Some(relative) = path.strip_prefix(prefix.as_str()) else {
                    continue;
                };
                let meta = std::fs::metadata(root.join(relative))
                    .ok()
                    .and_then(|meta| Some((meta.len(), meta.modified().ok()?)));
                tree.insert(relative.to_owned(), meta);
            }
            tree
        })
        .await
        .ok()
    }
}

/// What one write-scoped item did, for its result in the plan report.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Record {
    /// Files it changed inside its writes (file tools and shell commands).
    pub written: BTreeSet<String>,
    /// Writes outside its writes that were refused.
    pub refused: BTreeSet<String>,
    /// Files outside its writes its shell commands changed (kept, only reported).
    pub shell_outside: BTreeSet<String>,
    /// Of those, the ones other items declared: file -> their ids.
    pub shell_others: BTreeMap<String, String>,
    /// Files that changed while one of its shell commands ran but may have been changed
    /// by another item running at the same time.
    pub shell_uncertain: BTreeSet<String>,
    /// File-generating commands that were refused.
    pub commands: Vec<String>,
}

impl Record {
    /// Anything outside the declared writes, attempted or done.
    pub fn outside(&self) -> bool {
        !self.refused.is_empty()
            || !self.shell_outside.is_empty()
            || !self.shell_uncertain.is_empty()
            || !self.commands.is_empty()
    }

    /// `c.rs (write refused); d.rs (changed by shell); npm install (command refused)`.
    pub fn outside_text(&self) -> String {
        let mut parts: Vec<String> = Vec::new();
        parts.extend(
            self.refused
                .iter()
                .map(|file| format!("{file} (write refused)")),
        );
        parts.extend(
            self.shell_outside
                .iter()
                .map(|file| match self.shell_others.get(file) {
                    Some(owners) => format!("{file} (changed by shell; in the writes of {owners})"),
                    None => format!("{file} (changed by shell)"),
                }),
        );
        parts.extend(self.shell_uncertain.iter().map(|file| {
            format!("{file} (changed during its shell command; maybe by another item running then)")
        }));
        parts.extend(
            self.commands
                .iter()
                .map(|command| format!("{} (command refused)", crate::text::short(command, 80))),
        );
        parts.join("; ")
    }
}

/// One shell command of a write-scoped item, from just before it runs until this is
/// dropped, however the call ends (done, failed, cancelled); [`ShellRun::void`] takes it
/// back when the command never ran (refused).
pub struct ShellRun {
    plan: Arc<PlanScopes>,
    number: u64,
    item: String,
    pub started: Instant,
}

impl ShellRun {
    /// The command did not run: other items' changes meanwhile are not in doubt.
    pub fn void(&self) {
        if let Ok(mut shells) = self.plan.shells.lock() {
            shells.retain(|shell| shell.0 != self.number);
        }
    }
}

impl Drop for ShellRun {
    fn drop(&mut self) {
        self.plan.end_shell(self.number);
    }
}

/// The write constraint of one running plan item, checked by its sub-agent's tools.
pub struct ItemScope {
    pub id: String,
    pub writes: WriteSet,
    plan: Arc<PlanScopes>,
    record: Mutex<Record>,
}

impl ItemScope {
    pub fn record(&self) -> Record {
        self.record
            .lock()
            .map(|record| record.clone())
            .unwrap_or_default()
    }

    fn update(&self, change: impl FnOnce(&mut Record)) {
        if let Ok(mut record) = self.record.lock() {
            change(&mut record);
        }
    }

    /// The reason a call may not run for this item, before it runs: a file write outside
    /// its writes, or a command that installs, generates, migrates or formats.
    pub fn refuse(&self, name: &str, input: &Value) -> Option<String> {
        if let Some(path) = write_target(name, input) {
            let relative = relative(&self.plan.root, path)?;
            if self.writes.covers(&relative) {
                return None;
            }
            self.update(|record| {
                record.refused.insert(relative.clone());
            });
            return Some(format!(
                "{relative} is outside this plan item's writes ({}); stop and report why it needs changing instead of editing it",
                self.writes.summary(8)
            ));
        }
        if name == "regression" {
            self.update(|record| record.commands.push("regression".into()));
            return Some(
                "regression is off for plan items that declared writes: it swaps files back to their baseline and runs tests, \
                 which items running at the same time would see. Say in your report which tests should prove the change; \
                 they run after all items finish."
                    .into(),
            );
        }
        if name == "shell" {
            let command = input.get("command").and_then(Value::as_str)?;
            if input.get("run_in_background").and_then(Value::as_bool) == Some(true) {
                self.update(|record| {
                    record
                        .commands
                        .push(format!("{command} (run_in_background)"))
                });
                return Some(
                    "run_in_background is off for plan items that declared writes: what such a command changes after this call \
                     returns cannot be checked against them. Run it in the foreground, or say in your report that it is needed."
                        .into(),
                );
            }
            let dialect = crate::safety::permission::shell::Dialect::for_command(command);
            if let Some(found) =
                crate::safety::permission::classify::generates_files(command, dialect)
            {
                self.update(|record| record.commands.push(command.to_owned()));
                return Some(format!(
                    "`{}` installs packages, generates code, migrates a database or formats/autofixes files; \
                     this plan item declared writes, so such steps run after all items finish. \
                     Do not run it; say in your report that it is needed.",
                    crate::text::short(&found, 120)
                ));
            }
        }
        None
    }

    /// After a successful call: the file a file tool wrote.
    pub fn wrote(&self, name: &str, input: &Value) {
        let Some(relative) =
            write_target(name, input).and_then(|path| relative(&self.plan.root, path))
        else {
            return;
        };
        if let Ok(mut written) = self.plan.written.lock() {
            written.push((key(&relative), self.id.clone(), Instant::now()));
        }
        self.update(|record| {
            record.written.insert(relative);
        });
    }

    /// Marks the start of a shell command that is about to run; keep the result until
    /// the command has ended.
    pub fn shell_run(&self) -> ShellRun {
        let number = self.plan.next_shell.fetch_add(1, Ordering::Relaxed);
        let started = Instant::now();
        if let Ok(mut shells) = self.plan.shells.lock() {
            shells.push((number, self.id.clone(), started, None));
            let excess = shells.len().saturating_sub(MAX_SHELLS);
            shells.drain(..excess);
        }
        ShellRun {
            plan: self.plan.clone(),
            number,
            item: self.id.clone(),
            started,
        }
    }

    /// The changed files of the workspace now, around a shell command.
    pub async fn tree(&self, cancel: &CancellationToken) -> Option<Tree> {
        if cancel.is_cancelled() {
            return None;
        }
        self.plan.tree(cancel).await
    }

    /// Records what the shell command `run` changed between `before`
    /// and `after`; returns the files outside this item's writes it changed (another
    /// item's declared files among them, with their ids). Files another item's file tools
    /// wrote meanwhile are left to that item; while another item's shell command, or an
    /// item without writes, may have run at the same time, the other changes are recorded
    /// as uncertain and not returned.
    pub fn shell_changed(&self, before: &Tree, after: &Tree, run: &ShellRun) -> Vec<String> {
        self.plan.end_shell(run.number);
        let started = run.started;
        let mut changed: BTreeSet<&String> = BTreeSet::new();
        for (path, meta) in before {
            if after.get(path) != Some(meta) {
                changed.insert(path);
            }
        }
        for (path, meta) in after {
            if before.get(path) != Some(meta) {
                changed.insert(path);
            }
        }
        let by_files: BTreeSet<String> = self.plan.written.lock().map_or_else(
            |_| BTreeSet::new(),
            |written| {
                written
                    .iter()
                    .filter(|(_, _, when)| *when >= started)
                    .map(|(file, _, _)| file.clone())
                    .collect()
            },
        );
        let others: Vec<(String, WriteSet)> = self.plan.items.lock().map_or_else(
            |_| Vec::new(),
            |items| {
                items
                    .iter()
                    .filter(|(id, _)| *id != self.id)
                    .cloned()
                    .collect()
            },
        );
        let shared =
            self.plan.overlapped(run) || self.plan.unscoped_parallel.load(Ordering::Relaxed);
        let mut outside = Vec::new();
        let mut owned = BTreeMap::new();
        let mut uncertain = Vec::new();
        for path in changed {
            if self.writes.covers(path) {
                self.update(|record| {
                    record.written.insert(path.clone());
                });
                continue;
            }
            if by_files.contains(&key(path)) {
                continue;
            }
            if shared {
                uncertain.push(path.clone());
                continue;
            }
            let owners: Vec<&str> = others
                .iter()
                .filter(|(_, writes)| writes.covers(path))
                .map(|(id, _)| id.as_str())
                .collect();
            if !owners.is_empty() {
                owned.insert(path.clone(), owners.join(", "));
            }
            outside.push(path.clone());
        }
        self.update(|record| {
            record.shell_outside.extend(outside.iter().cloned());
            record.shell_others.extend(owned.clone());
            record.shell_uncertain.extend(uncertain);
        });
        outside
            .into_iter()
            .map(|path| match owned.get(&path) {
                Some(owners) => format!("{path} (in the writes of {owners})"),
                None => path,
            })
            .collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn set(entries: &[&str]) -> WriteSet {
        WriteSet::parse(&entries.iter().map(|e| (*e).to_owned()).collect::<Vec<_>>()).unwrap()
    }

    #[test]
    fn entries_are_files_or_globs_and_symbols_are_not_enforced() {
        let writes = set(&[
            "src/a.rs::Foo::bar",
            "./tests\\b.rs",
            "docs/*.md",
            "assets/",
        ]);
        assert!(writes.covers("src/a.rs"));
        assert!(writes.covers("tests/b.rs"));
        assert!(writes.covers("docs/x.md"));
        assert!(!writes.covers("docs/sub/x.md"), "* stays in one directory");
        assert!(writes.covers("assets/img/x.png"));
        assert!(!writes.covers("src/b.rs"));
        assert_eq!(
            writes.files(),
            ["src/a.rs", "tests/b.rs", "docs/*.md", "assets/"]
        );
        for bad in ["", "/etc/passwd", "C:/x", "../x", "a/../../b", "src/[a"] {
            assert!(WriteSet::parse(&[bad.to_owned()]).is_err(), "{bad}");
        }
    }

    #[test]
    fn the_overlay_schedule_runs_distinct_symbols_side_by_side_and_stacks_the_rest() {
        let root = tempfile::tempdir().unwrap();
        let sets = [
            set(&["src/lib.rs::one"]),
            set(&["src/lib.rs::two", "src/b.rs"]),
            set(&["src/lib.rs::one"]),
            set(&["src/lib.rs"]),
            set(&["docs/x.md"]),
        ];
        let none: Vec<String> = Vec::new();
        let units: Vec<Unit> = sets
            .iter()
            .enumerate()
            .map(|(index, writes)| Unit {
                id: ["a", "b", "c", "d", "e"][index],
                depends: &none,
                writes: Some(writes),
            })
            .collect();
        let found = implicit_depends(&units, root.path());
        let pairs: Vec<(usize, usize)> = found
            .iter()
            .map(|implicit| (implicit.later, implicit.earlier))
            .collect();
        // a and b: distinct symbols, parallel. c repeats a's symbol: after a. d has no
        // symbol: after each of the earlier ones. e shares nothing.
        assert_eq!(pairs, [(2, 0), (3, 0), (3, 1), (3, 2)]);
        assert_eq!(found[0].files, ["src/lib.rs"]);
        assert_eq!(
            set(&["src/lib.rs::one", "src/lib.rs::two"])
                .symbols_for("./src\\lib.rs")
                .unwrap()
                .len(),
            2
        );
        assert_eq!(set(&["src/*.rs"]).symbols_for("src/lib.rs"), None);
        assert_eq!(
            set(&["src/lib.rs::f", "src/lib.rs"]).symbols_for("src/lib.rs"),
            None
        );
        assert_eq!(set(&["src/lib.rs::f"]).symbols_for("src/other.rs"), None);
        // An existing dependency is respected: no implicit edge is added on top of it.
        let after_a = vec!["a".to_owned()];
        let chained = [
            Unit {
                id: "a",
                depends: &none,
                writes: Some(&sets[0]),
            },
            Unit {
                id: "c",
                depends: &after_a,
                writes: Some(&sets[2]),
            },
        ];
        assert!(implicit_depends(&chained, root.path()).is_empty());
        // The same glob on both sides is never split by symbols.
        let globs = [set(&["src/*.rs"]), set(&["src/*.rs"])];
        let glob_units = [
            Unit {
                id: "x",
                depends: &none,
                writes: Some(&globs[0]),
            },
            Unit {
                id: "y",
                depends: &none,
                writes: Some(&globs[1]),
            },
        ];
        assert_eq!(
            implicit_depends(&glob_units, root.path())[0].files,
            ["src/*.rs"]
        );
    }

    #[test]
    fn overlap_needs_a_shared_file_a_matching_existing_file_or_the_same_glob() {
        let none = || -> &[String] { &[] };
        let listed = vec!["src/a.rs".to_owned(), "src/b.rs".to_owned()];
        let files = || listed.as_slice();
        assert_eq!(
            set(&["src/a.rs::f"]).overlap(&set(&["src/a.rs::g"]), &none),
            ["src/a.rs"]
        );
        assert_eq!(
            set(&["src/*.rs"]).overlap(&set(&["src/b.rs"]), &none),
            ["src/b.rs"]
        );
        assert_eq!(
            set(&["src/*.rs"]).overlap(&set(&["src/*.rs"]), &none),
            ["src/*.rs"]
        );
        assert_eq!(
            set(&["src/*.rs"]).overlap(&set(&["**/b.rs"]), &files),
            ["src/b.rs"]
        );
        assert!(
            set(&["src/*.rs"])
                .overlap(&set(&["**/c.rs"]), &files)
                .is_empty()
        );
        assert!(
            set(&["src/a.rs"])
                .overlap(&set(&["src/b.rs"]), &none)
                .is_empty()
        );
    }

    #[test]
    fn only_independent_items_with_writes_are_checked() {
        let root = tempfile::tempdir().unwrap();
        let (a, b, c) = (set(&["x.rs"]), set(&["x.rs"]), set(&["x.rs"]));
        let after_a = vec!["a".to_owned()];
        let after_b = vec!["b".to_owned()];
        let units = [
            Unit {
                id: "a",
                depends: &[],
                writes: Some(&a),
            },
            Unit {
                id: "b",
                depends: &after_a,
                writes: Some(&b),
            },
            Unit {
                id: "c",
                depends: &after_b,
                writes: Some(&c),
            },
            Unit {
                id: "d",
                depends: &[],
                writes: None,
            },
        ];
        assert!(
            overlaps(&units, root.path()).is_empty(),
            "a chain is serial"
        );
        let units = [
            Unit {
                id: "a",
                depends: &[],
                writes: Some(&a),
            },
            Unit {
                id: "b",
                depends: &[],
                writes: Some(&b),
            },
        ];
        assert_eq!(
            overlaps(&units, root.path()),
            [Overlap {
                a: "a".into(),
                b: "b".into(),
                files: vec!["x.rs".into()]
            }]
        );
    }

    #[test]
    fn relative_paths_resolve_inside_the_workspace_only() {
        let root = tempfile::tempdir().unwrap();
        let root = root.path().canonicalize().unwrap();
        std::fs::create_dir(root.join("src")).unwrap();
        assert_eq!(relative(&root, "src/new.rs").as_deref(), Some("src/new.rs"));
        assert_eq!(
            relative(&root, &root.join("src").join("x.rs").to_string_lossy()).as_deref(),
            Some("src/x.rs")
        );
        assert_eq!(relative(&root, "../x"), None);
        let outside = tempfile::tempdir().unwrap();
        assert_eq!(
            relative(&root, &outside.path().join("x").to_string_lossy()),
            None
        );
    }

    #[test]
    fn shell_changes_outside_are_recorded_with_the_items_that_declared_them() {
        let root = tempfile::tempdir().unwrap();
        let plan = PlanScopes::new(root.path());
        let mine = plan.item("a", set(&["a.rs"]));
        let _theirs = plan.item("b", set(&["b.rs"]));
        let time = SystemTime::UNIX_EPOCH;
        let before: Tree = [("x.rs".to_owned(), Some((1, time)))].into();
        let after: Tree = [
            ("x.rs".to_owned(), Some((2, time))),
            ("a.rs".to_owned(), Some((1, time))),
            ("b.rs".to_owned(), Some((1, time))),
            ("gen.rs".to_owned(), Some((1, time))),
        ]
        .into();
        let run = mine.shell_run();
        // No other command ran meanwhile: everything changed is this item's doing,
        // b's declared file most of all.
        let outside = mine.shell_changed(&before, &after, &run);
        assert_eq!(outside, ["b.rs (in the writes of b)", "gen.rs", "x.rs"]);
        let record = mine.record();
        assert!(record.written.contains("a.rs"));
        assert!(record.outside());
        assert_eq!(
            record.outside_text(),
            "b.rs (changed by shell; in the writes of b); gen.rs (changed by shell); x.rs (changed by shell)"
        );
    }

    fn changed_tree() -> (Tree, Tree) {
        let time = SystemTime::UNIX_EPOCH;
        let after: Tree = [
            ("a.rs".to_owned(), Some((1, time))),
            ("b.rs".to_owned(), Some((1, time))),
            ("x.rs".to_owned(), Some((1, time))),
        ]
        .into();
        (Tree::new(), after)
    }

    #[test]
    fn changes_while_another_item_may_be_writing_are_uncertain() {
        let (before, after) = changed_tree();
        // Another scoped item's shell command ran at the same time.
        let root = tempfile::tempdir().unwrap();
        let plan = PlanScopes::new(root.path());
        let mine = plan.item("a", set(&["a.rs"]));
        let theirs = plan.item("b", set(&["b.rs"]));
        let run = mine.shell_run();
        let other = theirs.shell_run();
        assert!(mine.shell_changed(&before, &after, &run).is_empty());
        let record = mine.record();
        assert!(record.written.contains("a.rs"));
        assert!(record.shell_outside.is_empty());
        assert_eq!(
            record.shell_uncertain.iter().collect::<Vec<_>>(),
            ["b.rs", "x.rs"]
        );
        assert!(
            record
                .outside_text()
                .contains("x.rs (changed during its shell command; maybe by another item")
        );
        // A command of the other item that ended before this one started does not count.
        drop(other);
        std::thread::sleep(Duration::from_millis(5));
        let run = mine.shell_run();
        assert_eq!(
            mine.shell_changed(&before, &after, &run),
            ["b.rs (in the writes of b)", "x.rs"]
        );
        // Items without writes running in parallel: nothing is certain.
        let plan = PlanScopes::new(root.path()).with_unscoped_parallel(true);
        let alone = plan.item("a", set(&["a.rs"]));
        let run = alone.shell_run();
        assert!(alone.shell_changed(&before, &after, &run).is_empty());
        assert_eq!(alone.record().shell_uncertain.len(), 2);
    }

    /// A command that never got its result checked (refused, failed, cancelled) does not
    /// leave a run open that turns later changes of other items uncertain; passing
    /// `refuse` alone does not start one.
    #[test]
    fn shell_runs_end_on_every_way_out() {
        let (before, after) = changed_tree();
        let root = tempfile::tempdir().unwrap();
        let plan = PlanScopes::new(root.path());
        let mine = plan.item("a", set(&["a.rs"]));
        let theirs = plan.item("b", set(&["b.rs"]));
        // Allowed by the scope, then refused by the permission check: never ran.
        assert!(
            theirs
                .refuse("shell", &serde_json::json!({"command":"make stuff"}))
                .is_none()
        );
        let refused = theirs.shell_run();
        refused.void();
        drop(refused);
        // Started, then the call failed or was cancelled: ended when dropped.
        drop(theirs.shell_run());
        std::thread::sleep(Duration::from_millis(5));
        let run = mine.shell_run();
        assert_eq!(
            mine.shell_changed(&before, &after, &run),
            ["b.rs (in the writes of b)", "x.rs"]
        );
        assert!(mine.record().shell_uncertain.is_empty());
        // A voided run is gone even while the other one runs.
        let refused = theirs.shell_run();
        refused.void();
        let run = mine.shell_run();
        assert_eq!(mine.shell_changed(&before, &after, &run).len(), 2);
        drop(refused);
    }

    #[test]
    fn background_shells_and_regression_are_refused_in_scoped_items() {
        let root = tempfile::tempdir().unwrap();
        let plan = PlanScopes::new(root.path());
        let item = plan.item("a", set(&["a.rs"]));
        let background = serde_json::json!({"command":"npm run dev","run_in_background":true});
        assert!(
            item.refuse("shell", &background)
                .unwrap()
                .contains("run_in_background is off")
        );
        assert!(
            item.refuse("regression", &serde_json::json!({}))
                .unwrap()
                .contains("regression is off")
        );
        let foreground = serde_json::json!({"command":"cargo test","run_in_background":false});
        assert!(item.refuse("shell", &foreground).is_none());
        assert_eq!(
            item.record().commands,
            ["npm run dev (run_in_background)", "regression"]
        );
    }

    #[test]
    fn a_directory_without_a_slash_covers_the_files_below_it() {
        let writes = set(&["src/agents"]);
        assert!(writes.covers("src/agents"));
        assert!(writes.covers("src/agents/plan.rs"));
        assert!(writes.covers(r"src\agents\sub\x.rs"));
        assert!(!writes.covers("src/agents2/x.rs"));
        assert!(!writes.covers("src/agent"));
        let none = || -> &[String] { &[] };
        assert_eq!(
            set(&["src/agents/plan.rs"]).overlap(&writes, &none),
            ["src/agents/plan.rs"]
        );
        let listed = vec!["src/agents/plan.rs".to_owned(), "src/lib.rs".to_owned()];
        let files = || listed.as_slice();
        assert_eq!(
            writes.overlap(&set(&["src/**/*.rs"]), &files),
            ["src/agents/plan.rs"]
        );
        assert!(writes.overlap(&set(&["src/*.rs"]), &files).is_empty());
    }
}
