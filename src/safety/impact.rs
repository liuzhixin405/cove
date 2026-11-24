//! Change impact (docs/optimizations/0031): the symbols a change removed or modified,
//! the other files that use them by name, and which of those callers no test file
//! refers to. Local and deterministic: the `repo_map` index (regex definitions, the
//! identifiers of each file), no model call. Name matching has false positives (two
//! functions of the same name) and misses (members the regexes do not see, string
//! coupling such as route paths); the text says so.

use std::{
    collections::{HashMap, HashSet},
    path::{Path, PathBuf},
    time::{Duration, Instant},
};

use serde::{Deserialize, Serialize};
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Result},
    tools::repomap::{self, Index, Symbol},
};

/// The time one computation may take at the end of a task; past it the result is
/// marked partial.
pub const BUDGET: Duration = Duration::from_secs(2);
/// `/changes impact`, asked for: a cold cache on a large repository gets longer.
pub const ASKED_BUDGET: Duration = Duration::from_secs(10);
/// Names shorter than this, or defined in more files than [`COMMON_NAME_FILES`], are
/// too generic to find callers by name.
const MIN_NAME: usize = 4;
const COMMON_NAME_FILES: usize = 8;
/// Names that mean nothing on their own (constructors, entry points, trait methods).
const GENERIC: &[&str] = &[
    "main",
    "init",
    "__init__",
    "default",
    "from",
    "into",
    "drop",
    "clone",
    "call",
    "next",
    "test",
    "setup",
    "setUp",
    "tearDown",
    "Main",
    "Dispose",
    "ToString",
    "Equals",
    "GetHashCode",
    "toString",
    "equals",
    "hashCode",
    "constructor",
];
const LINES_PER_CALLER: usize = 3;
/// Changed files beyond this are left out (a mass rename, a vendored tree).
const MAX_CHANGED_FILES: usize = 200;
/// Rust files read to find a test module naming a caller's function.
const MAX_TEST_READS: usize = 20;

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Change {
    Removed,
    Signature,
    Body,
}

impl Change {
    fn label(self) -> &'static str {
        match self {
            Self::Removed => "删除",
            Self::Signature => "签名变化",
            Self::Body => "函数体变化",
        }
    }

    fn english(self) -> &'static str {
        match self {
            Self::Removed => "removed",
            Self::Signature => "signature changed",
            Self::Body => "body changed",
        }
    }

    /// A removal or a signature change can break a caller that was not touched.
    pub fn risky(self) -> bool {
        matches!(self, Self::Removed | Self::Signature)
    }
}

/// Whether a test file refers to a caller.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Tested {
    /// A test file uses the function the call is in (the file named).
    Named(String),
    /// No test names it, but its package or module has tests.
    Package,
    None,
}

#[derive(Clone, Debug)]
pub struct ChangedSymbol {
    /// Workspace-relative, `/`-separated.
    pub file: String,
    pub name: String,
    pub kind: &'static str,
    /// Line before the change for a removed symbol, after it otherwise.
    pub line: usize,
    pub change: Change,
    /// Test files that use the symbol itself (at most 3).
    pub tests: Vec<String>,
    /// The name is too generic; callers were not looked for.
    pub generic: bool,
}

#[derive(Clone, Debug)]
pub struct Caller {
    /// Position in [`Impact::symbols`].
    pub symbol: usize,
    pub file: String,
    /// Lines that use the name (at most 3).
    pub lines: Vec<usize>,
    /// The definition the first use is in (by start line: the regex index has no ends).
    pub enclosing: Option<String>,
    /// Its kind (`fn`, `class`, ...), empty without one.
    pub enclosing_kind: &'static str,
    pub tests: Tested,
    /// The same change also edited this file.
    pub changed: bool,
}

#[derive(Clone, Debug, Default)]
pub struct Skipped {
    /// Changed symbols whose names are too generic.
    pub generic: usize,
    /// Changed files in languages the index does not read.
    pub unsupported: usize,
    /// Changed symbols or callers past the limits.
    pub symbols_cut: usize,
    pub callers_cut: usize,
    /// Shell commands changed files too (not in the file tools' record).
    pub shell: bool,
    /// The time budget ran out.
    pub timed_out: bool,
    /// The scan stopped at the index's file limit.
    pub scan_limit: bool,
}

#[derive(Clone, Debug, Default)]
pub struct Impact {
    pub symbols: Vec<ChangedSymbol>,
    pub callers: Vec<Caller>,
    pub skipped: Skipped,
}

/// One changed file: workspace-relative `/` path, text before and after (`None`:
/// created or deleted).
#[derive(Clone, Debug)]
pub struct FileChange {
    pub path: String,
    pub before: Option<String>,
    pub after: Option<String>,
}

#[derive(Clone, Copy, Debug)]
pub struct Limits {
    pub symbols: usize,
    pub per_symbol: usize,
    pub callers: usize,
}

impl Limits {
    /// The end of a task.
    pub const TASK: Self = Self {
        symbols: 20,
        per_symbol: 10,
        callers: 30,
    };
    /// `/changes impact`: asked for, so more.
    pub const ASKED: Self = Self {
        symbols: 50,
        per_symbol: 10,
        callers: 50,
    };
}

/// Whether `path` (`/`-separated, workspace-relative) is a test file by the usual
/// conventions of the languages the index reads.
pub fn is_test_path(path: &str) -> bool {
    let mut parts: Vec<&str> = path.split('/').filter(|part| !part.is_empty()).collect();
    let Some(name) = parts.pop() else {
        return false;
    };
    if parts.iter().any(|dir| {
        let lower = dir.to_ascii_lowercase();
        matches!(lower.as_str(), "tests" | "test" | "__tests__")
            || lower.ends_with(".tests")
            || lower.ends_with(".test")
    }) {
        return true;
    }
    let script = |name: &str| {
        [".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"]
            .iter()
            .any(|ext| name.ends_with(ext))
    };
    name.ends_with("_test.go")
        || (name.starts_with("test_") && name.ends_with(".py"))
        || name.ends_with("_test.py")
        || (script(name) && (name.contains(".test.") || name.contains(".spec.")))
        || name.ends_with("Test.java")
        || name.ends_with("Tests.java")
        || name.ends_with("Tests.cs")
        || name.ends_with("Test.cs")
}

/// First line (1-based) of a Rust file's test module (`#[cfg(test)]`); the rest of
/// the file counts as test code.
fn test_start(path: &str, text: &str) -> Option<usize> {
    if !path.ends_with(".rs") {
        return None;
    }
    text.lines()
        .position(|line| line.trim_start().starts_with("#[cfg(test)]"))
        .map(|index| index + 1)
}

fn ident_char(c: char) -> bool {
    c.is_alphanumeric() || c == '_' || c == '$'
}

/// Whether `line` has `name` as a whole word.
fn has_word(line: &str, name: &str) -> bool {
    let mut from = 0;
    while let Some(found) = line[from..].find(name) {
        let start = from + found;
        let end = start + name.len();
        let before = line[..start].chars().next_back();
        let after = line[end..].chars().next();
        if !before.is_some_and(ident_char) && !after.is_some_and(ident_char) {
            return true;
        }
        from = end;
    }
    false
}

/// A line that is only a comment (`//`, `/*`, `*`, `#` but not `#[`).
fn comment_line(line: &str) -> bool {
    let line = line.trim_start();
    line.starts_with("//")
        || line.starts_with("/*")
        || line.starts_with('*')
        || (line.starts_with('#') && !line.starts_with("#[") && !line.starts_with("#!"))
}

/// Whether `line` calls `name`: the whole word followed by `(` or `::<`, outside a
/// comment line. A function passed as a value (`map(price)`) is missed.
fn has_call(line: &str, name: &str) -> bool {
    let mut from = 0;
    while let Some(found) = line[from..].find(name) {
        let start = from + found;
        let end = start + name.len();
        let before = line[..start].chars().next_back();
        let rest = &line[end..];
        if !before.is_some_and(ident_char) && (rest.starts_with('(') || rest.starts_with("::<")) {
            return true;
        }
        from = end;
    }
    false
}

/// 1-based lines of `text` that use `name`, comment lines left out: calls for a
/// function, the whole word for anything else (types, constants).
fn use_lines(text: &str, name: &str, call: bool) -> Vec<usize> {
    text.lines()
        .enumerate()
        .filter(|(_, line)| {
            line.len() <= 2000
                && !comment_line(line)
                && if call {
                    has_call(line, name)
                } else {
                    has_word(line, name)
                }
        })
        .map(|(index, _)| index + 1)
        .collect()
}

/// An import or `use` line: it names a symbol without calling it.
fn import_line(line: &str) -> bool {
    let line = line.trim_start();
    [
        "import ", "from ", "use ", "pub use ", "using ", "export {", "export *", "#include",
    ]
    .iter()
    .any(|prefix| line.starts_with(prefix))
        || line.contains("require(")
}

/// Whether a name is too generic to look callers up by.
fn generic(name: &str, defined_in: usize) -> bool {
    name.len() < MIN_NAME || GENERIC.contains(&name) || defined_in > COMMON_NAME_FILES
}

/// Lines (1-based) on each side that a line diff of `before` and `after` touches.
fn touched_lines(before: &str, after: &str) -> (HashSet<usize>, HashSet<usize>) {
    let mut old = HashSet::new();
    let mut new = HashSet::new();
    for op in similar::TextDiff::from_lines(before, after).ops() {
        if op.tag() == similar::DiffTag::Equal {
            continue;
        }
        old.extend(op.old_range().map(|index| index + 1));
        new.extend(op.new_range().map(|index| index + 1));
        // An insertion shows on the new side only, a deletion on the old side: the
        // lines themselves say which symbol they are in.
    }
    (old, new)
}

/// Symbols that end the range of the one before them; `impl` blocks bound ranges but
/// are not changed symbols themselves (their methods are).
/// `last` ends the last range (the start of a Rust test module).
fn ranges(symbols: &[Symbol], last: usize) -> Vec<(usize, usize)> {
    symbols
        .iter()
        .enumerate()
        .map(|(index, symbol)| {
            let end = symbols
                .get(index + 1)
                .map_or(last, |next| next.line.max(symbol.line + 1));
            (symbol.line, end)
        })
        .collect()
}

/// The symbols of one file that a change removed or modified (test code left out).
pub fn changed_symbols(change: &FileChange) -> Vec<ChangedSymbol> {
    let path = Path::new(&change.path);
    if is_test_path(&change.path) {
        return Vec::new();
    }
    let Some(before) = change.before.as_deref() else {
        // A new file: nothing used it before.
        return Vec::new();
    };
    let after = change.after.as_deref().unwrap_or_default();
    let production = |text: &str| -> (Vec<Symbol>, usize) {
        let end = test_start(&change.path, text).unwrap_or(usize::MAX);
        let symbols = repomap::symbols(path, text)
            .into_iter()
            .filter(|symbol| symbol.line < end)
            .collect();
        (symbols, end)
    };
    let (old, old_end) = production(before);
    let (new, new_end) = production(after);
    let (old_touched, new_touched) = touched_lines(before, after);
    let old_ranges = ranges(&old, old_end);
    let new_ranges = ranges(&new, new_end);
    // Pair by (name, kind): the one with the same signature first, then in order, so
    // two `fn new` in two impls, or a method added before a same-named function,
    // stay apart.
    let mut taken = vec![false; new.len()];
    let mut pairs: Vec<Option<usize>> = vec![None; old.len()];
    for (index, symbol) in old.iter().enumerate() {
        let same = |other: &Symbol| other.name == symbol.name && other.kind == symbol.kind;
        let found = (0..new.len())
            .find(|at| !taken[*at] && same(&new[*at]) && new[*at].signature == symbol.signature);
        if let Some(at) = found {
            taken[at] = true;
            pairs[index] = Some(at);
        }
    }
    for (index, symbol) in old.iter().enumerate() {
        if pairs[index].is_some() {
            continue;
        }
        let found = (0..new.len())
            .find(|at| !taken[*at] && new[*at].name == symbol.name && new[*at].kind == symbol.kind);
        if let Some(at) = found {
            taken[at] = true;
            pairs[index] = Some(at);
        }
    }
    let mut out = Vec::new();
    for (index, symbol) in old.iter().enumerate() {
        if symbol.kind == "impl" || symbol.kind == "mod" {
            continue;
        }
        let (change_kind, line) = match pairs[index].as_ref() {
            None => (Change::Removed, symbol.line),
            Some(&at) => {
                let now = &new[at];
                if now.signature != symbol.signature {
                    (Change::Signature, now.line)
                } else {
                    let (start, end) = old_ranges[index];
                    let (new_start, new_end) = new_ranges[at];
                    let body = old_touched.iter().any(|line| (start..end).contains(line))
                        || new_touched
                            .iter()
                            .any(|line| (new_start..new_end).contains(line));
                    if !body {
                        continue;
                    }
                    (Change::Body, now.line)
                }
            }
        };
        out.push(ChangedSymbol {
            file: change.path.clone(),
            name: symbol.name.clone(),
            kind: symbol.kind,
            line,
            change: change_kind,
            tests: Vec::new(),
            generic: false,
        });
    }
    out
}

/// What the index knows about test files, for "is this caller tested" lookups.
struct Tests<'a> {
    paths: Vec<&'a str>,
    is_test: Vec<bool>,
    /// Lower-case file names of the test files.
    names: HashSet<String>,
    /// Rust files that seem to have a test module (`cfg` and `test` among their
    /// identifiers): read them all would cost too much for a weak label.
    rust_tests: Vec<bool>,
}

impl Tests<'_> {
    /// A test file named after the caller's file (`test_x.py`, `x.spec.ts`,
    /// `XTests.cs`), or tests in its Go directory or Rust crate.
    fn package(&self, root: &Path, file: &str, own_tests: bool) -> bool {
        let (dir, name) = file.rsplit_once('/').unwrap_or(("", file));
        let (stem, ext) = name.rsplit_once('.').unwrap_or((name, ""));
        let under = |prefix: &str| {
            self.paths
                .iter()
                .zip(&self.is_test)
                .zip(&self.rust_tests)
                .any(|((path, test), rust)| {
                    (*test || *rust) && (prefix.is_empty() || path.starts_with(prefix))
                })
        };
        let named = |candidates: &[String]| {
            candidates
                .iter()
                .any(|candidate| self.names.contains(&candidate.to_lowercase()))
        };
        match ext {
            "go" => self.paths.iter().zip(&self.is_test).any(|(path, test)| {
                *test
                    && path.ends_with("_test.go")
                    && path.rsplit_once('/').map_or("", |(dir, _)| dir) == dir
            }),
            "rs" => {
                if own_tests {
                    return true;
                }
                // The nearest crate: its tests/ directory, or any file of it with a
                // test module (the index's identifiers stand in for reading them).
                let mut crate_dir = Some(dir);
                while let Some(current) = crate_dir {
                    if root.join(current).join("Cargo.toml").is_file() {
                        break;
                    }
                    crate_dir = current
                        .rsplit_once('/')
                        .map(|(up, _)| up)
                        .or(if current.is_empty() { None } else { Some("") });
                }
                let Some(crate_dir) = crate_dir else {
                    return false;
                };
                let prefix = if crate_dir.is_empty() {
                    String::new()
                } else {
                    format!("{crate_dir}/")
                };
                under(&prefix)
            }
            "py" => named(&[format!("test_{stem}.py"), format!("{stem}_test.py")]),
            "ts" | "tsx" | "js" | "jsx" | "mjs" | "cjs" => ["ts", "tsx", "js", "jsx"]
                .iter()
                .any(|ext| named(&[format!("{stem}.test.{ext}"), format!("{stem}.spec.{ext}")])),
            "java" => named(&[format!("{stem}Test.java"), format!("{stem}Tests.java")]),
            "cs" => named(&[format!("{stem}Tests.cs"), format!("{stem}Test.cs")]),
            _ => false,
        }
    }
}

/// The definition a line is in: the last one starting above it.
fn enclosing(symbols: &[Symbol], line: usize) -> Option<&Symbol> {
    symbols
        .iter()
        .filter(|symbol| symbol.line < line)
        .max_by_key(|symbol| symbol.line)
}

/// Callers of the changed symbols in `index` (the workspace as it is now).
pub(crate) fn compute(
    root: &Path,
    changes: &[FileChange],
    index: Option<&Index>,
    limits: Limits,
    deadline: Instant,
) -> Impact {
    let mut impact = Impact::default();
    let mut symbols = Vec::new();
    for change in changes.iter().take(MAX_CHANGED_FILES) {
        if !repomap::supported(Path::new(&change.path)) {
            impact.skipped.unsupported += 1;
            continue;
        }
        symbols.extend(changed_symbols(change));
    }
    symbols.sort_by(|a, b| (a.change, &a.file, a.line).cmp(&(b.change, &b.file, b.line)));
    if symbols.len() > limits.symbols {
        impact.skipped.symbols_cut = symbols.len() - limits.symbols;
        symbols.truncate(limits.symbols);
    }
    impact.symbols = symbols;
    let Some(index) = index else {
        return impact;
    };
    impact.skipped.scan_limit = index.truncated;
    let files: Vec<(&Path, &[Symbol], &HashSet<String>)> = index.files().collect();
    let paths: Vec<String> = files
        .iter()
        .map(|(path, ..)| path.to_string_lossy().replace('\\', "/"))
        .collect();
    let tests = Tests {
        paths: paths.iter().map(String::as_str).collect(),
        is_test: paths.iter().map(|path| is_test_path(path)).collect(),
        names: paths
            .iter()
            .filter(|path| is_test_path(path))
            .map(|path| path.rsplit('/').next().unwrap_or(path).to_lowercase())
            .collect(),
        rust_tests: files
            .iter()
            .zip(&paths)
            .map(|((_, _, used), path)| {
                path.ends_with(".rs") && used.contains("cfg") && used.contains("test")
            })
            .collect(),
    };
    let mut defined: HashMap<&str, Vec<usize>> = HashMap::new();
    for (position, (_, symbols, _)) in files.iter().enumerate() {
        for symbol in symbols.iter() {
            let entry = defined.entry(symbol.name.as_str()).or_default();
            if entry.last() != Some(&position) {
                entry.push(position);
            }
        }
    }
    let used = index.references();
    let changed: HashSet<&str> = changes.iter().map(|change| change.path.as_str()).collect();
    let is_generic = |name: &str| generic(name, defined.get(name).map_or(0, Vec::len));
    let mut texts: HashMap<usize, Option<String>> = HashMap::new();
    let read = |position: usize, texts: &mut HashMap<usize, Option<String>>| {
        texts
            .entry(position)
            .or_insert_with(|| std::fs::read_to_string(root.join(&paths[position])).ok())
            .clone()
    };
    let mut test_reads = 0;
    'symbols: for at in 0..impact.symbols.len() {
        if Instant::now() > deadline {
            impact.skipped.timed_out = true;
            break;
        }
        let name = impact.symbols[at].name.clone();
        let file = impact.symbols[at].file.clone();
        let call = function_like(impact.symbols[at].kind);
        if is_generic(&name) {
            impact.symbols[at].generic = true;
            impact.skipped.generic += 1;
            continue;
        }
        let definers = defined.get(name.as_str()).cloned().unwrap_or_default();
        let mut candidates: Vec<usize> = used
            .get(name.as_str())
            .into_iter()
            .flatten()
            .copied()
            .filter(|position| paths[*position] != file && !definers.contains(position))
            .collect();
        candidates.sort_by(|a, b| paths[*a].cmp(&paths[*b]));
        let mut listed = 0;
        for position in candidates {
            if Instant::now() > deadline {
                impact.skipped.timed_out = true;
                break 'symbols;
            }
            let Some(text) = read(position, &mut texts) else {
                continue;
            };
            let path = paths[position].as_str();
            let symbols = files[position].1;
            let definition_lines: HashSet<usize> = symbols
                .iter()
                .filter(|symbol| symbol.name == name)
                .map(|symbol| symbol.line)
                .collect();
            let lines: Vec<usize> = use_lines(&text, &name, call)
                .into_iter()
                .filter(|line| !definition_lines.contains(line))
                .collect();
            if lines.is_empty() {
                continue;
            }
            let own_tests = test_start(path, &text);
            let split = own_tests.unwrap_or(usize::MAX);
            let tested_here = tests.is_test[position] || lines.iter().any(|line| *line >= split);
            let symbol = &mut impact.symbols[at];
            if tested_here && symbol.tests.len() < 3 && !symbol.tests.iter().any(|t| t == path) {
                symbol.tests.push(path.to_owned());
            }
            if tests.is_test[position] {
                continue;
            }
            let production: Vec<usize> = lines.into_iter().filter(|line| *line < split).collect();
            if production.is_empty() {
                continue;
            }
            // Calls first: an import names the symbol without calling it.
            let rows: Vec<&str> = text.lines().collect();
            let calls: Vec<usize> = production
                .iter()
                .copied()
                .filter(|line| !rows.get(line - 1).is_some_and(|row| import_line(row)))
                .collect();
            let production = if calls.is_empty() { production } else { calls };
            if listed >= limits.per_symbol || impact.callers.len() >= limits.callers {
                impact.skipped.callers_cut += 1;
                continue;
            }
            listed += 1;
            let outer_symbol = enclosing(symbols, production[0]);
            let around = outer_symbol.map(|symbol| symbol.name.clone());
            let around_kind = outer_symbol.map_or("", |symbol| symbol.kind);
            // Two hops: a test that uses the function this call is in.
            let mut tested = Tested::None;
            if let Some(outer) = around.as_deref().filter(|outer| !is_generic(outer)) {
                let users: Vec<usize> = used.get(outer).cloned().unwrap_or_default();
                let mut named: Vec<&str> = users
                    .iter()
                    .filter(|user| tests.is_test[**user] && paths[**user] != path)
                    .map(|user| paths[*user].as_str())
                    .collect();
                named.sort();
                if let Some(first) = named.first() {
                    tested = Tested::Named((*first).to_owned());
                } else if own_tests.is_some_and(|start| {
                    text.lines()
                        .skip(start - 1)
                        .any(|line| has_word(line, outer))
                }) {
                    tested = Tested::Named(path.to_owned());
                } else {
                    for user in users {
                        let other = paths[user].as_str();
                        if other == path || !other.ends_with(".rs") || tests.is_test[user] {
                            continue;
                        }
                        if test_reads >= MAX_TEST_READS {
                            break;
                        }
                        test_reads += 1;
                        let Some(other_text) = read(user, &mut texts) else {
                            continue;
                        };
                        if let Some(start) = test_start(other, &other_text)
                            && other_text
                                .lines()
                                .skip(start - 1)
                                .any(|line| has_word(line, outer))
                        {
                            tested = Tested::Named(other.to_owned());
                            break;
                        }
                    }
                }
            }
            if tested == Tested::None && tests.package(root, path, own_tests.is_some()) {
                tested = Tested::Package;
            }
            impact.callers.push(Caller {
                symbol: at,
                file: path.to_owned(),
                lines: production.into_iter().take(LINES_PER_CALLER).collect(),
                enclosing: around,
                enclosing_kind: around_kind,
                tests: tested,
                changed: changed.contains(path),
            });
        }
    }
    impact
}

/// Builds the index of `root` and computes, within `budget`: past it, or when the
/// index cannot be built in time, the result is what was found and says so.
pub async fn run(
    root: PathBuf,
    changes: Vec<FileChange>,
    limits: Limits,
    budget: Duration,
    cancel: &CancellationToken,
) -> Result<Impact> {
    let deadline = Instant::now() + budget;
    let stop = cancel.child_token();
    let timer = {
        let stop = stop.clone();
        tokio::spawn(async move {
            tokio::time::sleep(budget).await;
            stop.cancel();
        })
    };
    let worker_stop = stop.clone();
    let result = tokio::task::spawn_blocking(move || {
        let files = crate::tools::search::walk(&root).filter_map(|entry| {
            let entry = entry.ok()?;
            if !entry.file_type()?.is_file() {
                return None;
            }
            let relative = entry.path().strip_prefix(&root).ok()?.to_path_buf();
            Some((relative, entry.path().to_path_buf()))
        });
        let index = match repomap::index(files, &worker_stop) {
            Ok(index) => Some(index),
            Err(Error::Cancelled) => None,
            Err(err) => return Err(err),
        };
        let mut impact = compute(&root, &changes, index.as_ref(), limits, deadline);
        if index.is_none() {
            impact.skipped.timed_out = true;
        }
        Ok(impact)
    })
    .await;
    timer.abort();
    if cancel.is_cancelled() {
        return Err(Error::Cancelled);
    }
    result.map_err(|err| Error::Tool(format!("改动影响计算失败：{err}")))?
}

fn function_like(kind: &str) -> bool {
    matches!(kind, "fn" | "def" | "function" | "method" | "func")
}

impl ChangedSymbol {
    fn title(&self) -> String {
        format!(
            "{}{}",
            self.name,
            if function_like(self.kind) { "()" } else { "" }
        )
    }
}

impl Caller {
    /// `monthly()` for a function, `Report` for a type, empty without one.
    fn around(&self) -> String {
        match &self.enclosing {
            Some(name) if function_like(self.enclosing_kind) => format!("{name}()"),
            Some(name) => name.clone(),
            None => String::new(),
        }
    }

    /// ` in monthly()` for the model, empty without one.
    fn inside(&self) -> String {
        let around = self.around();
        if around.is_empty() {
            around
        } else {
            format!(" in {around}")
        }
    }

    fn location(&self) -> String {
        let lines = self
            .lines
            .iter()
            .map(usize::to_string)
            .collect::<Vec<_>>()
            .join(",");
        format!("{}:{lines}", self.file)
    }

    fn tests_label(&self) -> String {
        let mut text = match &self.tests {
            Tested::Named(file) => format!("有测试引用（{file}）"),
            Tested::Package => "仅同包有测试".into(),
            Tested::None => "没有测试引用".into(),
        };
        if self.changed {
            text.push_str("；本次也改了");
        }
        text
    }
}

impl Impact {
    /// At least one changed symbol is used in another file.
    pub fn has_callers(&self) -> bool {
        !self.callers.is_empty()
    }

    /// A removed symbol or a changed signature among the changed symbols.
    pub fn risky(&self) -> bool {
        self.symbols.iter().any(|symbol| symbol.change.risky())
    }

    /// Callers of removed or re-signed symbols that no test refers to and that the
    /// change itself did not touch: what goes to the model.
    pub fn exposed(&self) -> Vec<&Caller> {
        self.callers
            .iter()
            .filter(|caller| {
                caller.tests == Tested::None
                    && !caller.changed
                    && self.symbols[caller.symbol].change.risky()
            })
            .collect()
    }

    fn notes(&self) -> Vec<String> {
        let skipped = &self.skipped;
        let mut notes = Vec::new();
        if skipped.callers_cut > 0 {
            notes.push(format!("另有 {} 处使用未列出", skipped.callers_cut));
        }
        if skipped.symbols_cut > 0 {
            notes.push(format!("另有 {} 个改动符号未查", skipped.symbols_cut));
        }
        if skipped.generic > 0 {
            notes.push(format!("{} 个名字太通用未查", skipped.generic));
        }
        if skipped.unsupported > 0 {
            notes.push(format!(
                "{} 个改动文件的语言不支持（只认 {}）",
                skipped.unsupported,
                repomap::LANGUAGES.split(" (").next().unwrap_or_default()
            ));
        }
        if skipped.shell {
            notes.push("shell 改动的文件未计入".into());
        }
        if skipped.timed_out {
            notes.push("时间用完，只查了一部分".into());
        }
        if skipped.scan_limit {
            notes.push("源文件太多，只扫描了一部分".into());
        }
        notes
    }

    /// The `[影响]` block for the user, at most `max_lines` lines.
    pub fn render(&self, max_lines: usize) -> String {
        let mut lines = Vec::new();
        let header = if self.callers.is_empty() {
            format!(
                "[影响] 改了 {} 个符号，其他文件里没有找到使用（按名字匹配，可能漏报）",
                self.symbols.len()
            )
        } else {
            format!(
                "[影响] 改了 {} 个符号，其他文件里有 {} 处使用（按名字匹配，可能有误报或漏报）",
                self.symbols.len(),
                self.callers.len() + self.skipped.callers_cut
            )
        };
        lines.push(header);
        let notes = self.notes();
        let budget = max_lines.saturating_sub(1 + usize::from(!notes.is_empty()));
        let mut body = Vec::new();
        let mut hidden = 0;
        for (at, symbol) in self.symbols.iter().enumerate() {
            let callers: Vec<&Caller> = self
                .callers
                .iter()
                .filter(|caller| caller.symbol == at)
                .collect();
            if callers.is_empty() {
                continue;
            }
            if body.len() + 2 > budget {
                hidden += callers.len();
                continue;
            }
            let mut head = format!(
                "  {} {}:{} {}",
                symbol.change.label(),
                symbol.file,
                symbol.line,
                symbol.title()
            );
            if !symbol.tests.is_empty() {
                head.push_str(&format!("（测试直接引用：{}）", symbol.tests.join("、")));
            }
            body.push(head);
            let rows: Vec<(String, String, String)> = callers
                .iter()
                .map(|caller| (caller.location(), caller.around(), caller.tests_label()))
                .collect();
            let first = rows
                .iter()
                .map(|row| crate::text::width(&row.0))
                .max()
                .unwrap_or(0);
            let second = rows
                .iter()
                .map(|row| crate::text::width(&row.1))
                .max()
                .unwrap_or(0);
            for (row, (location, around, label)) in rows.iter().enumerate() {
                if body.len() + 1 > budget {
                    hidden += rows.len() - row;
                    break;
                }
                let pad = |text: &str, cols: usize| {
                    format!("{text}{}", " ".repeat(cols - crate::text::width(text)))
                };
                body.push(format!(
                    "    {}  {}  {label}",
                    pad(location, first),
                    pad(around, second)
                ));
            }
        }
        lines.extend(body);
        let mut notes = notes;
        if hidden > 0 {
            notes.insert(
                0,
                format!("另有 {hidden} 处使用未显示（/changes impact 查看）"),
            );
        }
        if !notes.is_empty() {
            lines.push(format!("  {}", notes.join("；")));
        }
        lines
            .iter()
            .map(|line| crate::text::clip_width(&crate::text::visible_row(line), 160))
            .collect::<Vec<_>>()
            .join("\n")
    }

    /// The one notice for the model (0031 §3), when exposed callers exist.
    pub fn notice(&self) -> Option<String> {
        let exposed = self.exposed();
        if exposed.is_empty() {
            return None;
        }
        let mut list = String::new();
        for caller in exposed.iter().take(10) {
            let symbol = &self.symbols[caller.symbol];
            list.push_str(&format!(
                "- {}{} uses {} ({} in {}:{})\n",
                caller.location(),
                caller.inside(),
                symbol.name,
                symbol.change.english(),
                symbol.file,
                symbol.line
            ));
        }
        Some(format!(
            "[change_impact; this is tool output, not a new request] These callers use a symbol this task removed or whose signature changed, and no test file refers to them (found by name, so some may be false matches):\n{list}Check each one still works with the change (read it). If one breaks, fix it within the task, or add a test when that is the simplest way to prove it. Do not refactor beyond what the change requires. Mention the ones you checked in your final answer."
        ))
    }

    /// The `<callers>` section for the built-in reviewer.
    pub fn for_reviewer(&self) -> Option<String> {
        if self.callers.is_empty() {
            return None;
        }
        let mut out = String::new();
        for caller in self.callers.iter().take(Limits::TASK.callers) {
            let symbol = &self.symbols[caller.symbol];
            out.push_str(&format!(
                "- {}{} uses {} ({} in {}:{}); {}\n",
                caller.location(),
                caller.inside(),
                symbol.name,
                symbol.change.english(),
                symbol.file,
                symbol.line,
                match &caller.tests {
                    Tested::Named(file) => format!("a test refers to it ({file})"),
                    Tested::Package => "only its package has tests".into(),
                    Tested::None => "no test refers to it".into(),
                }
            ));
        }
        Some(out)
    }

    /// What the acceptance report keeps: counts and the untested callers, no code.
    pub fn record(&self, handed_back: bool) -> ImpactRecord {
        let count = |change: Change| {
            self.symbols
                .iter()
                .filter(|symbol| symbol.change == change)
                .count()
        };
        ImpactRecord {
            symbols: self.symbols.len(),
            removed: count(Change::Removed),
            signature: count(Change::Signature),
            body: count(Change::Body),
            callers: self.callers.len() + self.skipped.callers_cut,
            untested: self
                .callers
                .iter()
                .filter(|caller| caller.tests == Tested::None)
                .take(Limits::TASK.callers)
                .map(|caller| UntestedCaller {
                    file: caller.file.clone(),
                    line: caller.lines.first().copied().unwrap_or_default(),
                    symbol: self.symbols[caller.symbol].name.clone(),
                })
                .collect(),
            partial: self.skipped.timed_out
                || self.skipped.scan_limit
                || self.skipped.callers_cut > 0
                || self.skipped.symbols_cut > 0,
            handed_back,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct UntestedCaller {
    pub file: String,
    pub line: usize,
    pub symbol: String,
}

/// The change impact in the acceptance report (0031).
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct ImpactRecord {
    pub symbols: usize,
    pub removed: usize,
    pub signature: usize,
    pub body: usize,
    /// Uses in other files, listed or not.
    pub callers: usize,
    /// Callers no test file refers to (at most 30).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub untested: Vec<UntestedCaller>,
    /// Limits or the time budget cut the search.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub partial: bool,
    /// The untested callers went to the model once.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub handed_back: bool,
}

impl ImpactRecord {
    /// `影响：改了 3 个符号，其他文件 7 处使用，2 处没有测试引用（已交给模型核对）`.
    pub fn summary(&self) -> String {
        let mut text = format!(
            "影响：改了 {} 个符号（删除 {}、签名变化 {}、函数体变化 {}），其他文件 {} 处使用，{} 处没有测试引用",
            self.symbols,
            self.removed,
            self.signature,
            self.body,
            self.callers,
            self.untested.len()
        );
        if self.partial {
            text.push_str("，只查了一部分");
        }
        if self.handed_back {
            text.push_str("（已交给模型核对）");
        }
        text
    }
}

/// A git range as `/changes impact` takes it: `A...B` (from their merge base),
/// `A..B`, or one commit `X` (its own change, `X^..X`).
pub async fn git_changes(
    root: &Path,
    range: &str,
    cancel: &CancellationToken,
) -> Result<Vec<FileChange>> {
    use crate::workflows::changes::git;
    let range = range.trim();
    if range.is_empty() || range.starts_with('-') || range.contains(char::is_whitespace) {
        return Err(Error::Tool(format!("无效的 git 区间：{range}")));
    }
    let text = |bytes: Vec<u8>| String::from_utf8_lossy(&bytes).trim().to_owned();
    let commit = |rev: String| async move {
        git(
            root,
            &[
                "rev-parse",
                "--verify",
                "--quiet",
                &format!("{rev}^{{commit}}"),
            ],
            cancel,
        )
        .await
        .map(text)
        .map_err(|_| Error::Tool(format!("找不到提交：{rev}")))
    };
    let (base, tip) = if let Some((a, b)) = range.split_once("...") {
        let (a, b) = (commit(or_head(a)).await?, commit(or_head(b)).await?);
        let base = git(root, &["merge-base", &a, &b], cancel)
            .await
            .map(text)
            .map_err(|_| Error::Tool(format!("{range} 没有共同祖先")))?;
        (base, b)
    } else if let Some((a, b)) = range.split_once("..") {
        (commit(or_head(a)).await?, commit(or_head(b)).await?)
    } else {
        let tip = commit(range.to_owned()).await?;
        let base = commit(format!("{tip}^"))
            .await
            .map_err(|_| Error::Tool(format!("{range} 是第一个提交，没有可比较的父提交")))?;
        (base, tip)
    };
    // Paths in git output are relative to the repository top; the workspace may be a
    // subdirectory of it.
    let prefix = text(git(root, &["rev-parse", "--show-prefix"], cancel).await?);
    let listing = git(
        root,
        &[
            "-c",
            "core.quotepath=false",
            "diff",
            "--name-status",
            "-z",
            "-M",
            "--no-ext-diff",
            &base,
            &tip,
            "--",
        ],
        cancel,
    )
    .await?;
    let fields: Vec<String> = listing
        .split(|byte| *byte == 0)
        .map(|field| String::from_utf8_lossy(field).into_owned())
        .collect();
    let mut entries = Vec::new();
    let mut at = 0;
    while at < fields.len() {
        let status = fields[at].as_str();
        if status.is_empty() {
            at += 1;
            continue;
        }
        let (old, new, step) = if status.starts_with('R') || status.starts_with('C') {
            (fields.get(at + 1).cloned(), fields.get(at + 2).cloned(), 3)
        } else {
            let path = fields.get(at + 1).cloned();
            match status.chars().next() {
                Some('A') => (None, path, 2),
                Some('D') => (path, None, 2),
                _ => (path.clone(), path, 2),
            }
        };
        at += step;
        entries.push((old, new));
    }
    let show = |rev: String, path: String| async move {
        let bytes = git(root, &["show", &format!("{rev}:{path}")], cancel)
            .await
            .ok()?;
        (bytes.len() <= 1024 * 1024)
            .then(|| String::from_utf8(bytes).ok())
            .flatten()
    };
    let mut out = Vec::new();
    for (old, new) in entries.into_iter().take(MAX_CHANGED_FILES) {
        let Some(shown) = new.as_ref().or(old.as_ref()) else {
            continue;
        };
        let Some(relative) = shown.strip_prefix(prefix.as_str()) else {
            continue;
        };
        let relative = relative.to_owned();
        if !repomap::supported(Path::new(&relative)) {
            out.push(FileChange {
                path: relative,
                before: None,
                after: None,
            });
            continue;
        }
        let before = match old {
            Some(old) => show(base.clone(), old).await,
            None => None,
        };
        let after = match new {
            Some(new) => show(tip.clone(), new).await,
            None => None,
        };
        out.push(FileChange {
            path: relative,
            before,
            after,
        });
    }
    Ok(out)
}

fn or_head(rev: &str) -> String {
    if rev.is_empty() {
        "HEAD".into()
    } else {
        rev.to_owned()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn write(root: &Path, rel: &str, text: &str) {
        let path = root.join(rel);
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(path, text).unwrap();
    }

    fn change(path: &str, before: &str, after: &str) -> FileChange {
        FileChange {
            path: path.into(),
            before: Some(before.into()),
            after: Some(after.into()),
        }
    }

    fn index(root: &Path) -> Index {
        let files = crate::tools::search::walk(root).filter_map(|entry| {
            let entry = entry.ok()?;
            entry.file_type()?.is_file().then(|| {
                (
                    entry.path().strip_prefix(root).unwrap().to_path_buf(),
                    entry.path().to_path_buf(),
                )
            })
        });
        repomap::index(files, &CancellationToken::new()).unwrap()
    }

    fn later() -> Instant {
        Instant::now() + Duration::from_secs(30)
    }

    /// Writes the files (the state after the change) and computes.
    fn impact(files: &[(&str, &str)], changes: &[FileChange], limits: Limits) -> Impact {
        let dir = tempfile::tempdir().unwrap();
        for (rel, text) in files {
            write(dir.path(), rel, text);
        }
        let index = index(dir.path());
        compute(dir.path(), changes, Some(&index), limits, later())
    }

    #[test]
    fn classifies_removed_signature_and_body_changes_and_ignores_new_ones() {
        let before = "def keep():\n    return 1\n\ndef gone():\n    return 2\n\ndef price(item):\n    return item\n\ndef body():\n    x = 1\n    return x\n";
        let after = "def keep():\n    return 1\n\ndef price(item, currency='CNY'):\n    return item\n\ndef body():\n    x = 2\n    return x\n\ndef fresh():\n    return 3\n";
        let symbols = changed_symbols(&change("pricing.py", before, after));
        let found: Vec<(&str, Change)> = symbols
            .iter()
            .map(|symbol| (symbol.name.as_str(), symbol.change))
            .collect();
        assert_eq!(
            found,
            [
                ("gone", Change::Removed),
                ("price", Change::Signature),
                ("body", Change::Body)
            ]
        );
        // A deleted file removes everything; a new file changes nothing that existed.
        let deleted = FileChange {
            path: "a.py".into(),
            before: Some("def one():\n    pass\n".into()),
            after: None,
        };
        assert_eq!(changed_symbols(&deleted)[0].change, Change::Removed);
        let created = FileChange {
            path: "a.py".into(),
            before: None,
            after: Some("def one():\n    pass\n".into()),
        };
        assert!(changed_symbols(&created).is_empty());
        // Test code is not a changed symbol.
        let rust = changed_symbols(&change(
            "src/a.rs",
            "pub fn run_all() {}\n#[cfg(test)]\nmod tests {\n    fn case_one() { run_all(); }\n}\n",
            "pub fn run_all() {}\n#[cfg(test)]\nmod tests {\n    fn case_one() { run_all(); run_all(); }\n}\n",
        ));
        assert!(rust.is_empty(), "{rust:?}");
        // A same-named method added above an unchanged function: nothing changed.
        let added = changed_symbols(&change(
            "src/b.rs",
            "fn other() {}\nfn references(files: &[File]) -> usize {\n    0\n}\n",
            "impl Index {\n    pub fn references(&self) -> usize {\n        1\n    }\n}\nfn other() {}\nfn references(files: &[File]) -> usize {\n    0\n}\n",
        ));
        assert!(added.is_empty(), "{added:?}");
        // Calls only, outside comments, and not prose with a space before `(`.
        assert!(has_call("    let x = price(item);", "price"));
        assert!(has_call("feed::parse_quote::<T>(l)", "parse_quote"));
        assert!(!has_call("a task (a new task)", "task"));
        assert!(!comment_line("#[test]") && comment_line("    // price(x)"));
        assert_eq!(use_lines("// price(x)\nprice(y)\n", "price", true), [2]);
        assert_eq!(
            use_lines("/// the Report\nlet r: Report;\n", "Report", false),
            [2]
        );
    }

    #[test]
    fn callers_are_other_files_that_use_the_name_without_defining_it() {
        let impact = impact(
            &[
                ("store.rs", "pub fn load_session(id: u32) {}\n"),
                ("app.rs", "fn start() {\n    load_session(1);\n}\n"),
                (
                    "cli.rs",
                    "// load_session is called below\nfn open() {\n    let s = load_session(2);\n}\n",
                ),
                (
                    "other.rs",
                    "fn load_session() {}\nfn x() { load_session(); }\n",
                ),
            ],
            &[change(
                "store.rs",
                "pub fn load_session() {}\n",
                "pub fn load_session(id: u32) {}\n",
            )],
            Limits::TASK,
        );
        assert_eq!(impact.symbols.len(), 1);
        assert_eq!(impact.symbols[0].change, Change::Signature);
        let callers: Vec<(&str, Vec<usize>, Option<&str>)> = impact
            .callers
            .iter()
            .map(|caller| {
                (
                    caller.file.as_str(),
                    caller.lines.clone(),
                    caller.enclosing.as_deref(),
                )
            })
            .collect();
        assert_eq!(
            callers,
            [
                ("app.rs", vec![2], Some("start")),
                ("cli.rs", vec![3], Some("open")),
            ]
        );
        // A function counts where it is called: the comment naming it is not a use.
    }

    const PRICING_BEFORE: &str = "def price(item):\n    return item['cost']\n";
    const PRICING_AFTER: &str =
        "def price(item, currency='CNY'):\n    return (item['cost'], currency)\n";

    fn pricing() -> Impact {
        impact(
            &[
                ("pricing.py", PRICING_AFTER),
                (
                    "invoice.py",
                    "from pricing import price\n\ndef format_invoice(x):\n    return f\"{price(x):.2f}\"\n",
                ),
                (
                    "report.py",
                    "from pricing import price\n\ndef monthly(items):\n    return sum(price(i) for i in items)\n",
                ),
                (
                    "tests/test_report.py",
                    "from report import monthly\n\ndef test_monthly():\n    assert monthly([]) == 0\n",
                ),
                (
                    "tests/test_pricing.py",
                    "from pricing import price\n\ndef test_price():\n    assert price({'cost': 1})\n",
                ),
            ],
            &[change("pricing.py", PRICING_BEFORE, PRICING_AFTER)],
            Limits::TASK,
        )
    }

    #[test]
    fn a_test_of_the_calling_function_marks_the_caller_tested() {
        let impact = pricing();
        let tested: Vec<(&str, &Tested)> = impact
            .callers
            .iter()
            .map(|caller| (caller.file.as_str(), &caller.tests))
            .collect();
        assert_eq!(
            tested,
            [
                ("invoice.py", &Tested::None),
                ("report.py", &Tested::Named("tests/test_report.py".into())),
            ]
        );
        assert_eq!(impact.symbols[0].tests, ["tests/test_pricing.py"]);
        let exposed = impact.exposed();
        assert_eq!(exposed.len(), 1);
        let notice = impact.notice().unwrap();
        assert!(
            notice.contains(
                "- invoice.py:4 in format_invoice() uses price (signature changed in pricing.py:1)"
            ),
            "{notice}"
        );
        assert!(!notice.contains("report.py"), "{notice}");
        let text = impact.render(12);
        assert!(text.starts_with("[影响] 改了 1 个符号，其他文件里有 2 处使用（按名字匹配，可能有误报或漏报）\n  签名变化 pricing.py:1 price()（测试直接引用：tests/test_pricing.py）\n"), "{text}");
        assert!(
            text.contains("    invoice.py:4  format_invoice()  没有测试引用"),
            "{text}"
        );
        assert!(
            text.contains("    report.py:4   monthly()         有测试引用（tests/test_report.py）"),
            "{text}"
        );
        let record = impact.record(true);
        assert_eq!(
            (record.symbols, record.signature, record.callers),
            (1, 1, 2)
        );
        assert_eq!(record.untested[0].file, "invoice.py");
        assert!(record.summary().contains("已交给模型核对"));
        // A caller the change also edited is not handed to the model again.
        let mut edited = impact.clone();
        edited.callers[0].changed = true;
        assert!(edited.notice().is_none());
        assert!(edited.render(12).contains("没有测试引用；本次也改了"));
    }

    #[test]
    fn test_file_conventions_per_language() {
        for path in [
            "pkg/server_test.go",
            "tests/all/cli.rs",
            "Api.Tests/RepoTests.cs",
            "src/RepoTest.cs",
            "src/test/java/a/StoreTest.java",
            "web/api.spec.ts",
            "web/__tests__/api.ts",
            "test_store.py",
            "pkg/store_test.py",
        ] {
            assert!(is_test_path(path), "{path}");
        }
        for path in [
            "src/store.rs",
            "src/tmpl/x.py",
            "src/testing_utils.go",
            "latest.py",
        ] {
            assert!(!is_test_path(path), "{path}");
        }
        // Rust: the test module of the same file.
        let impact = impact(
            &[
                ("Cargo.toml", "[package]\nname = \"x\"\n"),
                ("src/store.rs", "pub fn load_session(id: u32) {}\n"),
                (
                    "src/app.rs",
                    "pub fn start_app() {\n    load_session(1);\n}\n#[cfg(test)]\nmod tests {\n    #[test]\n    fn starts() { super::start_app(); }\n}\n",
                ),
                (
                    "src/cli.rs",
                    "pub fn open_cli() {\n    load_session(2);\n}\n",
                ),
            ],
            &[change(
                "src/store.rs",
                "pub fn load_session() {}\n",
                "pub fn load_session(id: u32) {}\n",
            )],
            Limits::TASK,
        );
        let tested: Vec<(&str, &Tested)> = impact
            .callers
            .iter()
            .map(|caller| (caller.file.as_str(), &caller.tests))
            .collect();
        assert_eq!(
            tested,
            [
                ("src/app.rs", &Tested::Named("src/app.rs".into())),
                // Another file of the crate has a test module.
                ("src/cli.rs", &Tested::Package),
            ]
        );
        // Go: a _test.go in the caller's directory is package-level.
        let impact = impact_go();
        assert_eq!(impact.callers[0].tests, Tested::Package);
    }

    fn impact_go() -> Impact {
        impact(
            &[
                (
                    "quote/parse.go",
                    "package quote\n\nfunc ParseQuote(line string, venue string) Quote {\n}\n",
                ),
                (
                    "feed/feed.go",
                    "package feed\n\nfunc Consume() {\n\tquote.ParseQuote(l, v)\n}\n",
                ),
                (
                    "feed/other_test.go",
                    "package feed\n\nfunc TestOther(t *testing.T) {}\n",
                ),
            ],
            &[change(
                "quote/parse.go",
                "package quote\n\nfunc ParseQuote(line string) Quote {\n}\n",
                "package quote\n\nfunc ParseQuote(line string, venue string) Quote {\n}\n",
            )],
            Limits::TASK,
        )
    }

    #[test]
    fn generic_names_are_not_looked_up() {
        let mut files: Vec<(String, String)> = (0..9)
            .map(|n| {
                (
                    format!("m{n}.py"),
                    "def handle_it():\n    pass\n".to_owned(),
                )
            })
            .collect();
        files.push((
            "use.py".into(),
            "from m0 import handle_it\nhandle_it()\n".into(),
        ));
        let files: Vec<(&str, &str)> = files
            .iter()
            .map(|(a, b)| (a.as_str(), b.as_str()))
            .collect();
        let impact = impact(
            &files,
            &[
                change(
                    "m0.py",
                    "def handle_it(x):\n    pass\n",
                    "def handle_it():\n    pass\n",
                ),
                change(
                    "x.py",
                    "def main(a):\n    pass\n",
                    "def main():\n    pass\n",
                ),
            ],
            Limits::TASK,
        );
        assert_eq!(impact.skipped.generic, 2);
        assert!(impact.callers.is_empty());
        assert!(impact.render(12).contains("2 个名字太通用未查"));
    }

    #[test]
    fn limits_cut_and_say_so() {
        let mut files = vec![(
            "core.py".to_owned(),
            "def compute_total(a):\n    pass\n".to_owned(),
        )];
        for n in 0..40 {
            files.push((
                format!("dir{}/use{n}.py", n % 5),
                format!("def caller_{n}():\n    compute_total(1)\n"),
            ));
        }
        let files: Vec<(&str, &str)> = files
            .iter()
            .map(|(a, b)| (a.as_str(), b.as_str()))
            .collect();
        let impact = impact(
            &files,
            &[change(
                "core.py",
                "def compute_total():\n    pass\n",
                "def compute_total(a):\n    pass\n",
            )],
            Limits::TASK,
        );
        assert_eq!(impact.callers.len(), 10);
        assert_eq!(impact.skipped.callers_cut, 30);
        let text = impact.render(12);
        assert!(text.lines().count() <= 12, "{text}");
        assert!(text.contains("其他文件里有 40 处使用"), "{text}");
        assert!(text.contains("另有 30 处使用未列出"), "{text}");
        assert!(impact.record(false).partial);
        // Past the deadline nothing more is looked up, and it says so.
        let dir = tempfile::tempdir().unwrap();
        for (rel, text) in &files {
            write(dir.path(), rel, text);
        }
        let index = index(dir.path());
        let late = compute(
            dir.path(),
            &[change(
                "core.py",
                "def compute_total():\n    pass\n",
                "def compute_total(a):\n    pass\n",
            )],
            Some(&index),
            Limits::TASK,
            Instant::now() - Duration::from_millis(1),
        );
        assert!(late.skipped.timed_out && late.callers.is_empty());
        assert!(late.render(12).contains("只查了一部分"));
    }

    #[tokio::test]
    async fn run_is_cancellable_and_bounded() {
        let dir = tempfile::tempdir().unwrap();
        write(dir.path(), "a.py", "def price_it(a):\n    pass\n");
        write(dir.path(), "b.py", "def use_it():\n    price_it(1)\n");
        let changes = vec![change(
            "a.py",
            "def price_it():\n    pass\n",
            "def price_it(a):\n    pass\n",
        )];
        let cancel = CancellationToken::new();
        cancel.cancel();
        let result = run(
            dir.path().to_path_buf(),
            changes.clone(),
            Limits::TASK,
            BUDGET,
            &cancel,
        )
        .await;
        assert!(matches!(result, Err(Error::Cancelled)));
        let impact = run(
            dir.path().to_path_buf(),
            changes,
            Limits::TASK,
            BUDGET,
            &CancellationToken::new(),
        )
        .await
        .unwrap();
        assert_eq!(impact.callers.len(), 1);
    }

    /// Known misses, written down so nobody takes them for supported (0031 P6).
    #[test]
    fn csharp_properties_and_interface_members_are_not_symbols() {
        let symbols = changed_symbols(&change(
            "Feed.cs",
            "public class Feed\n{\n    public string? UnheardHint => null;\n    Task RunAsync();\n}\n",
            "public class Feed\n{\n    public string? UnheardHint => \"x\";\n    Task RunAsync(int n);\n}\n",
        ));
        // Only the class (its body changed); the property and interface member are missed.
        let names: Vec<&str> = symbols.iter().map(|symbol| symbol.name.as_str()).collect();
        assert_eq!(names, ["Feed"]);
    }

    #[test]
    fn paths_have_no_verbatim_prefix_and_columns_follow_display_width() {
        let impact = impact(
            &[
                ("定价/pricing.py", PRICING_AFTER),
                (
                    "发票/invoice.py",
                    "from pricing import price\n\ndef format_invoice(x):\n    return price(x)\n",
                ),
                (
                    "r.py",
                    "from pricing import price\n\ndef monthly(items):\n    return price(items)\n",
                ),
            ],
            &[change("定价/pricing.py", PRICING_BEFORE, PRICING_AFTER)],
            Limits::TASK,
        );
        let text = impact.render(12);
        assert!(!text.contains(r"\\?\"), "{text}");
        let rows: Vec<&str> = text
            .lines()
            .filter(|line| line.starts_with("    "))
            .collect();
        assert_eq!(rows.len(), 2, "{text}");
        // The label column starts at the same display column on both rows.
        let column = |row: &str| crate::text::width(&row[..row.find("没有").unwrap()]);
        assert_eq!(column(rows[0]), column(rows[1]), "{text}");
    }

    #[tokio::test]
    async fn git_ranges_give_before_and_after_text() {
        use crate::workflows::changes::tests::git_sync;
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        git_sync(&root, &["init", "-q", "-b", "main"]);
        write(
            &root,
            "quote.py",
            "def parse_quote(line):\n    return line\n",
        );
        write(
            &root,
            "feed_a.py",
            "def feed_a(l):\n    return parse_quote(l)\n",
        );
        write(&root, "notes.md", "x\n");
        git_sync(&root, &["add", "-A"]);
        git_sync(&root, &["commit", "-q", "-m", "init"]);
        git_sync(&root, &["checkout", "-q", "-b", "topic"]);
        write(
            &root,
            "quote.py",
            "def parse_quote(line, venue):\n    return line\n",
        );
        write(&root, "notes.md", "y\n");
        git_sync(&root, &["commit", "-q", "-am", "venue"]);
        let cancel = CancellationToken::new();
        for range in ["main..topic", "main...HEAD", "HEAD"] {
            let changes = git_changes(&root, range, &cancel).await.unwrap();
            let quote = changes.iter().find(|c| c.path == "quote.py").unwrap();
            assert!(
                quote.before.as_deref().unwrap().contains("(line)"),
                "{range}"
            );
            assert!(quote.after.as_deref().unwrap().contains("venue"), "{range}");
            assert!(changes.iter().any(|c| c.path == "notes.md"), "{range}");
        }
        assert!(git_changes(&root, "--output=x", &cancel).await.is_err());
        assert!(git_changes(&root, "nope..HEAD", &cancel).await.is_err());
        let first = git_changes(&root, "main", &cancel).await;
        assert!(first.is_err(), "the first commit has no parent");
    }
}
