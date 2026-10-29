//! Repository map: definitions per source file, ranked by PageRank over cross-file references.

use std::{
    collections::{HashMap, HashSet},
    path::{Path, PathBuf},
    sync::{
        Arc, LazyLock, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::SystemTime,
};

use regex::Regex;
use tokio_util::sync::CancellationToken;

use crate::domain::{Error, Result};

const FILE_LIMIT: u64 = 1024 * 1024;
const MAX_FILES: usize = 5000;
/// Names defined in more files than this are too generic to link files (new, main, run).
const COMMON_NAME_FILES: usize = 8;
const SYMBOLS_PER_FILE: usize = 30;

pub const LANGUAGES: &str = "Rust, Go, Python, TypeScript/JavaScript, C#, Java (.rs .go .py .ts .tsx .js .jsx .mjs .cjs .cs .java)";

#[derive(Clone, Copy, PartialEq, Eq)]
enum Language {
    Rust,
    Go,
    Python,
    Script,
    CSharp,
    Java,
}

fn language(path: &Path) -> Option<Language> {
    Some(match path.extension()?.to_str()? {
        "rs" => Language::Rust,
        "go" => Language::Go,
        "py" => Language::Python,
        "ts" | "tsx" | "js" | "jsx" | "mjs" | "cjs" => Language::Script,
        "cs" => Language::CSharp,
        "java" => Language::Java,
        _ => return None,
    })
}

pub struct Symbol {
    pub name: String,
    pub kind: &'static str,
    pub line: usize,
    pub signature: String,
}

struct Rule {
    regex: Regex,
    kind: &'static str,
    /// Capture group holding the name.
    group: usize,
}

fn rule(pattern: &str, kind: &'static str, group: usize) -> Rule {
    Rule {
        regex: Regex::new(pattern).unwrap(),
        kind,
        group,
    }
}

static RUST: LazyLock<Vec<Rule>> = LazyLock::new(|| {
    let vis = r"^\s*(?:pub(?:\([^)]*\))?\s+)?";
    vec![
        rule(
            &format!(r#"{vis}(?:(?:const|async|unsafe|extern\s+"[^"]*")\s+)*fn\s+([A-Za-z_]\w*)"#),
            "fn",
            1,
        ),
        rule(&format!(r"{vis}struct\s+([A-Za-z_]\w*)"), "struct", 1),
        rule(&format!(r"{vis}enum\s+([A-Za-z_]\w*)"), "enum", 1),
        rule(
            &format!(r"{vis}(?:unsafe\s+)?trait\s+([A-Za-z_]\w*)"),
            "trait",
            1,
        ),
        rule(&format!(r"{vis}type\s+([A-Za-z_]\w*)"), "type", 1),
        rule(&format!(r"{vis}mod\s+([A-Za-z_]\w*)\s*\{{"), "mod", 1),
        rule(
            &format!(r"{vis}(?:const|static)\s+([A-Z_][A-Z0-9_]*)\s*:"),
            "const",
            1,
        ),
        rule(r"^\s*macro_rules!\s*([A-Za-z_]\w*)", "macro", 1),
        rule(
            r"^\s*(?:unsafe\s+)?impl\b(?:<[^>]*>)?\s+(?:[\w:<>, ]+\s+for\s+)?([A-Za-z_]\w*)",
            "impl",
            1,
        ),
    ]
});

static GO: LazyLock<Vec<Rule>> = LazyLock::new(|| {
    vec![
        rule(r"^func\s+\([^)]*\)\s*([A-Za-z_]\w*)", "method", 1),
        rule(r"^func\s+([A-Za-z_]\w*)", "func", 1),
        rule(r"^type\s+([A-Za-z_]\w*)\s+interface\b", "interface", 1),
        rule(r"^type\s+([A-Za-z_]\w*)\s+struct\b", "struct", 1),
        rule(r"^type\s+([A-Za-z_]\w*)\b", "type", 1),
    ]
});

static PYTHON: LazyLock<Vec<Rule>> = LazyLock::new(|| {
    vec![
        rule(r"^\s*class\s+([A-Za-z_]\w*)", "class", 1),
        rule(r"^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)", "def", 1),
    ]
});

static SCRIPT: LazyLock<Vec<Rule>> = LazyLock::new(|| {
    vec![
        rule(
            r"^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)",
            "function",
            1,
        ),
        rule(
            r"^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)",
            "class",
            1,
        ),
        rule(
            r"^\s*(?:export\s+)?(?:declare\s+)?interface\s+([A-Za-z_$][\w$]*)",
            "interface",
            1,
        ),
        rule(
            r"^\s*(?:export\s+)?(?:declare\s+)?(?:const\s+)?enum\s+([A-Za-z_$][\w$]*)",
            "enum",
            1,
        ),
        rule(
            r"^\s*(?:export\s+)?type\s+([A-Za-z_$][\w$]*)\s*(?:<[^=]*>)?\s*=",
            "type",
            1,
        ),
        rule(
            r"^\s*export\s+(?:const|let|var)\s+([A-Za-z_$][\w$]*)",
            "const",
            1,
        ),
        rule(
            r"^\s+(?:(?:public|private|protected|static|async|readonly|override|get|set)\s+)*([A-Za-z_$][\w$]*)\s*(?:<[^>]*>)?\([^)]*\)\s*(?::\s*[^={]+)?\{\s*$",
            "method",
            1,
        ),
    ]
});

static C_FAMILY: LazyLock<Vec<Rule>> = LazyLock::new(|| {
    vec![
        rule(
            r"^\s*(?:(?:public|private|protected|internal|static|abstract|sealed|partial|final|readonly)\s+)*(class|interface|struct|enum|record)\s+([A-Za-z_]\w*)",
            "type",
            2,
        ),
        rule(
            r"^\s*(?:(?:public|private|protected|internal|static|virtual|override|abstract|async|final|sealed|synchronized|extern|unsafe|new)\s+)+[\w<>\[\],.?]+(?:\s*<[^>]*>)?\s+([A-Za-z_]\w*)\s*(?:<[^>]*>)?\([^;]*$",
            "method",
            1,
        ),
    ]
});

const KEYWORDS: &[&str] = &[
    "if",
    "for",
    "while",
    "switch",
    "catch",
    "return",
    "function",
    "constructor",
    "else",
    "do",
    "try",
    "with",
    "new",
    "typeof",
];

fn rules(language: Language) -> &'static [Rule] {
    match language {
        Language::Rust => &RUST,
        Language::Go => &GO,
        Language::Python => &PYTHON,
        Language::Script => &SCRIPT,
        Language::CSharp | Language::Java => &C_FAMILY,
    }
}

pub fn symbols(path: &Path, text: &str) -> Vec<Symbol> {
    let Some(language) = language(path) else {
        return Vec::new();
    };
    let mut out = Vec::new();
    for (index, line) in text.lines().enumerate() {
        if line.len() > 400 {
            continue;
        }
        for rule in rules(language) {
            let Some(caps) = rule.regex.captures(line) else {
                continue;
            };
            let name = &caps[rule.group];
            if KEYWORDS.contains(&name) {
                break;
            }
            let kind = if rule.kind == "type" && language != Language::Rust {
                caps.get(1)
                    .filter(|_| rule.group == 2)
                    .map_or(rule.kind, |kind| match kind.as_str() {
                        "class" => "class",
                        "interface" => "interface",
                        "struct" => "struct",
                        "enum" => "enum",
                        _ => "record",
                    })
            } else {
                rule.kind
            };
            let head = line.find('{').map_or(line, |brace| &line[..brace]);
            let mut signature = head.trim().to_owned();
            if signature.len() > 120 {
                let mut end = 117;
                while !signature.is_char_boundary(end) {
                    end -= 1;
                }
                signature.truncate(end);
                signature.push_str("...");
            }
            out.push(Symbol {
                name: name.to_owned(),
                kind,
                line: index + 1,
                signature,
            });
            break;
        }
    }
    out
}

struct File {
    path: PathBuf,
    parsed: Arc<Parsed>,
}

/// What one source file contributes: its definitions and the names it uses.
struct Parsed {
    symbols: Vec<Symbol>,
    identifiers: HashSet<String>,
}

/// Parsed files by absolute path, valid while modification time and length are
/// unchanged: a repeated `repo_map` reads only the files that changed.
static PARSED: LazyLock<Mutex<HashMap<PathBuf, Cached>>> =
    LazyLock::new(|| Mutex::new(HashMap::new()));
/// (modification time, length, parse) of one cached file.
type Cached = (Option<SystemTime>, u64, Arc<Parsed>);
/// Cached files beyond this start the cache over.
const CACHE_FILES: usize = 50_000;

fn parse(relative: &Path, absolute: &Path) -> Option<Parsed> {
    let text = std::fs::read_to_string(absolute).ok()?;
    Some(Parsed {
        symbols: symbols(relative, &text),
        identifiers: IDENTIFIER
            .find_iter(&text)
            .map(|found| found.as_str().to_owned())
            .collect(),
    })
}

/// Parses the files the cache cannot answer on scoped threads: reading is
/// mostly waiting on the file system (and on scanners hooked into it), so the
/// waits overlap; parsing spreads over the cores.
fn parse_all(
    files: &[(PathBuf, PathBuf, Option<SystemTime>, u64)],
    cancel: &CancellationToken,
) -> Result<Vec<Option<Arc<Parsed>>>> {
    let mut parsed: Vec<Option<Arc<Parsed>>> = vec![None; files.len()];
    let mut missing = Vec::new();
    {
        let cache = PARSED.lock().unwrap_or_else(|poison| poison.into_inner());
        for (index, (_, absolute, modified, len)) in files.iter().enumerate() {
            match cache.get(absolute) {
                Some((when, size, hit)) if when == modified && size == len => {
                    parsed[index] = Some(hit.clone());
                }
                _ => missing.push(index),
            }
        }
    }
    if missing.is_empty() {
        return Ok(parsed);
    }
    let workers = std::thread::available_parallelism()
        .map_or(4, |count| count.get())
        .clamp(1, 16)
        .min(missing.len());
    let next = AtomicUsize::new(0);
    let found: Vec<(usize, Arc<Parsed>)> = std::thread::scope(|scope| {
        let handles: Vec<_> = (0..workers)
            .map(|_| {
                scope.spawn(|| {
                    let mut done = Vec::new();
                    loop {
                        let at = next.fetch_add(1, Ordering::Relaxed);
                        let Some(&index) = missing.get(at) else {
                            break;
                        };
                        if cancel.is_cancelled() {
                            break;
                        }
                        let (relative, absolute, _, _) = &files[index];
                        if let Some(file) = parse(relative, absolute) {
                            done.push((index, Arc::new(file)));
                        }
                    }
                    done
                })
            })
            .collect();
        handles
            .into_iter()
            .flat_map(|handle| handle.join().unwrap_or_default())
            .collect()
    });
    if cancel.is_cancelled() {
        return Err(Error::Cancelled);
    }
    let mut cache = PARSED.lock().unwrap_or_else(|poison| poison.into_inner());
    if cache.len() + found.len() > CACHE_FILES {
        cache.clear();
    }
    for (index, file) in found {
        let (_, absolute, modified, len) = &files[index];
        cache.insert(absolute.clone(), (*modified, *len, file.clone()));
        parsed[index] = Some(file);
    }
    Ok(parsed)
}

static IDENTIFIER: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"[A-Za-z_][A-Za-z0-9_]{2,}").unwrap());

/// Files are workspace-relative; `walk` yields (relative, absolute) paths already confined.
pub fn query(
    files: impl Iterator<Item = (PathBuf, PathBuf)>,
    terms: &[String],
    max_bytes: usize,
    cancel: &CancellationToken,
) -> Result<String> {
    let mut candidates = Vec::new();
    let mut skipped = false;
    for (relative, absolute) in files {
        if cancel.is_cancelled() {
            return Err(Error::Cancelled);
        }
        if language(&relative).is_none() {
            continue;
        }
        if candidates.len() >= MAX_FILES {
            skipped = true;
            break;
        }
        let Ok(meta) = std::fs::metadata(&absolute) else {
            continue;
        };
        if meta.len() <= FILE_LIMIT {
            candidates.push((relative, absolute, meta.modified().ok(), meta.len()));
        }
    }
    let parsed = parse_all(&candidates, cancel)?;
    let scanned: Vec<File> = candidates
        .into_iter()
        .zip(parsed)
        .filter_map(|((path, ..), parsed)| parsed.map(|parsed| File { path, parsed }))
        .collect();
    let ranks = rank(&scanned);
    let terms: Vec<String> = terms
        .iter()
        .map(|term| term.trim().to_lowercase().replace('\\', "/"))
        .filter(|term| !term.is_empty())
        .collect();
    let mut order: Vec<usize> = (0..scanned.len()).collect();
    order.sort_by(|a, b| {
        ranks[*b]
            .total_cmp(&ranks[*a])
            .then(scanned[*a].path.cmp(&scanned[*b].path))
    });
    let mut matched_symbols: HashMap<usize, HashSet<usize>> = HashMap::new();
    if !terms.is_empty() {
        for (index, file) in scanned.iter().enumerate() {
            let path = file
                .path
                .to_string_lossy()
                .replace('\\', "/")
                .to_lowercase();
            let path_hit = terms.iter().any(|term| path.contains(term.as_str()));
            let hits: HashSet<usize> = file
                .parsed
                .symbols
                .iter()
                .enumerate()
                .filter(|(_, symbol)| {
                    let name = symbol.name.to_lowercase();
                    terms.iter().any(|term| name.contains(term.as_str()))
                })
                .map(|(position, _)| position)
                .collect();
            if path_hit || !hits.is_empty() {
                matched_symbols.insert(index, hits);
            }
        }
        if matched_symbols.is_empty() {
            return Ok(String::new());
        }
        // Matches first, then the files most connected to them (callers and callees).
        let matched: HashSet<usize> = matched_symbols.keys().copied().collect();
        let defined: HashMap<&str, Vec<usize>> = definitions(&scanned);
        let mut related: HashMap<usize, f64> = HashMap::new();
        for &index in &matched {
            for name in &scanned[index].parsed.identifiers {
                for &other in defined.get(name.as_str()).into_iter().flatten() {
                    if !matched.contains(&other) {
                        *related.entry(other).or_default() += ranks[other];
                    }
                }
            }
            let own: HashSet<&str> = scanned[index]
                .parsed
                .symbols
                .iter()
                .map(|symbol| symbol.name.as_str())
                .collect();
            for (other, file) in scanned.iter().enumerate() {
                if !matched.contains(&other)
                    && own
                        .iter()
                        .any(|name| file.parsed.identifiers.contains(*name))
                {
                    *related.entry(other).or_default() += ranks[other];
                }
            }
        }
        let mut related: Vec<(usize, f64)> = related.into_iter().collect();
        related.sort_by(|a, b| b.1.total_cmp(&a.1));
        let mut first: Vec<usize> = order
            .iter()
            .copied()
            .filter(|index| matched.contains(index))
            .collect();
        first.extend(related.into_iter().take(10).map(|(index, _)| index));
        order = first;
    }
    let mut out = String::new();
    let mut shown = 0;
    for (position, &index) in order.iter().enumerate() {
        let file = &scanned[index];
        let mut block = format!("{}\n", file.path.to_string_lossy().replace('\\', "/"));
        let hits = matched_symbols.get(&index);
        let focus: Vec<&Symbol> = match hits {
            Some(hits) if !hits.is_empty() => file
                .parsed
                .symbols
                .iter()
                .enumerate()
                .filter(|(position, _)| hits.contains(position))
                .map(|(_, symbol)| symbol)
                .collect(),
            _ => file.parsed.symbols.iter().collect(),
        };
        for symbol in focus.iter().take(SYMBOLS_PER_FILE) {
            block.push_str(&format!("  {:>5}: {}\n", symbol.line, symbol.signature));
        }
        if focus.len() > SYMBOLS_PER_FILE {
            block.push_str(&format!("  ... {} more\n", focus.len() - SYMBOLS_PER_FILE));
        }
        if out.len() + block.len() > max_bytes {
            out.push_str(&format!(
                "[{} more files not shown; narrow with query or path]\n",
                order.len() - position
            ));
            break;
        }
        out.push_str(&block);
        shown += 1;
    }
    if skipped {
        out.push_str(&format!("[scan stopped at {MAX_FILES} source files]\n"));
    }
    if shown == 0 && out.is_empty() {
        return Ok(String::new());
    }
    Ok(out)
}

fn definitions(files: &[File]) -> HashMap<&str, Vec<usize>> {
    let mut defined: HashMap<&str, Vec<usize>> = HashMap::new();
    for (index, file) in files.iter().enumerate() {
        for symbol in &file.parsed.symbols {
            let entry = defined.entry(symbol.name.as_str()).or_default();
            if entry.last() != Some(&index) {
                entry.push(index);
            }
        }
    }
    defined.retain(|name, files| name.len() > 2 && files.len() <= COMMON_NAME_FILES);
    defined
}

/// PageRank over "file uses a name defined in another file" edges.
fn rank(files: &[File]) -> Vec<f64> {
    let count = files.len();
    if count == 0 {
        return Vec::new();
    }
    let defined = definitions(files);
    let mut edges: Vec<Vec<usize>> = vec![Vec::new(); count];
    for (index, file) in files.iter().enumerate() {
        let mut targets = HashSet::new();
        for name in &file.parsed.identifiers {
            for &target in defined.get(name.as_str()).into_iter().flatten() {
                if target != index {
                    targets.insert(target);
                }
            }
        }
        edges[index] = targets.into_iter().collect();
    }
    let damping = 0.85;
    let mut ranks = vec![1.0 / count as f64; count];
    for _ in 0..30 {
        let dangling: f64 = (0..count)
            .filter(|index| edges[*index].is_empty())
            .map(|index| ranks[index])
            .sum();
        let base = (1.0 - damping) / count as f64 + damping * dangling / count as f64;
        let mut next = vec![base; count];
        for (index, targets) in edges.iter().enumerate() {
            if targets.is_empty() {
                continue;
            }
            let share = damping * ranks[index] / targets.len() as f64;
            for &target in targets {
                next[target] += share;
            }
        }
        ranks = next;
    }
    ranks
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn extracts_definitions_per_language() {
        let names = |path: &str, text: &str| {
            symbols(Path::new(path), text)
                .into_iter()
                .map(|symbol| format!("{}:{}:{}", symbol.kind, symbol.name, symbol.line))
                .collect::<Vec<_>>()
        };
        assert_eq!(
            names(
                "a.rs",
                "pub struct Agent {\n}\nimpl<T> Display for Agent {\n    pub async fn run(&self) {}\n}\nconst LIMIT: usize = 1;\n"
            ),
            [
                "struct:Agent:1",
                "impl:Agent:3",
                "fn:run:4",
                "const:LIMIT:6"
            ]
        );
        assert_eq!(
            names(
                "a.go",
                "func (s *Server) Start() error {\ntype Server struct {\nfunc main() {"
            ),
            ["method:Start:1", "struct:Server:2", "func:main:3"]
        );
        assert_eq!(
            names("a.py", "class Store:\n    async def load(self):\n"),
            ["class:Store:1", "def:load:2"]
        );
        assert_eq!(
            names(
                "a.ts",
                "export class Api {\n  async fetch(id: string): Promise<void> {\n    if (x) {\n}\nexport type Id = string;\n"
            ),
            ["class:Api:1", "method:fetch:2", "type:Id:5"]
        );
        assert_eq!(
            names(
                "A.cs",
                "public sealed class Repo\n{\n    public async Task<int> Count(string name)\n    {\n"
            ),
            ["class:Repo:1", "method:Count:3"]
        );
    }

    fn file(path: &str, defines: &[&str], uses: &[&str]) -> File {
        File {
            path: path.into(),
            parsed: Arc::new(Parsed {
                symbols: defines
                    .iter()
                    .map(|name| Symbol {
                        name: (*name).into(),
                        kind: "fn",
                        line: 1,
                        signature: format!("fn {name}()"),
                    })
                    .collect(),
                identifiers: uses
                    .iter()
                    .chain(defines)
                    .map(|name| (*name).to_owned())
                    .collect(),
            }),
        }
    }

    #[test]
    fn referenced_files_rank_higher() {
        let files = [
            file("core.rs", &["shared_core"], &[]),
            file("a.rs", &["alpha"], &["shared_core"]),
            file("b.rs", &["beta"], &["shared_core", "alpha"]),
        ];
        let ranks = rank(&files);
        assert!(ranks[0] > ranks[1] && ranks[1] > ranks[2], "{ranks:?}");
    }

    #[test]
    fn query_lists_matches_then_related_files() {
        let dir = tempfile::tempdir().unwrap();
        let write = |name: &str, text: &str| {
            std::fs::write(dir.path().join(name), text).unwrap();
            (PathBuf::from(name), dir.path().join(name))
        };
        let files = vec![
            write("store.rs", "pub fn load_session() {}\npub fn other() {}\n"),
            write("app.rs", "fn start() { load_session(); }\n"),
            write("unrelated.rs", "fn lonely() {}\n"),
            write("notes.txt", "load_session"),
        ];
        let out = query(
            files.into_iter(),
            &["load_session".into()],
            12 * 1024,
            &CancellationToken::new(),
        )
        .unwrap();
        assert_eq!(
            out,
            "store.rs\n      1: pub fn load_session()\napp.rs\n      1: fn start()\n"
        );
        let none = query(
            std::iter::empty(),
            &["x".into()],
            1024,
            &CancellationToken::new(),
        )
        .unwrap();
        assert!(none.is_empty());
    }
}
