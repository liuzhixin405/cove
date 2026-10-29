//! `grep` (ripgrep when installed, else built in) and `glob` over workspace files.

use std::{
    path::{Path, PathBuf},
    sync::OnceLock,
};

use tokio_util::sync::CancellationToken;

use crate::domain::{Error, Result};

/// Lines in one grep result; the rest is summarized.
pub const GREP_LINES: usize = 100;
pub const GREP_MAX_CONTEXT: usize = 10;
/// Paths in one glob result.
pub const GLOB_LIMIT: usize = 200;
const SCAN_LIMIT: usize = 50_000;
const FILE_LIMIT: u64 = 8 * 1024 * 1024;

pub struct GrepOptions {
    pub pattern: String,
    pub include: Option<String>,
    pub ignore_case: bool,
    pub context: usize,
    pub files_only: bool,
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

/// Files under `base` (respecting ignore files, without `.git`), each confined by `confine`.
fn walk(base: &Path) -> ignore::Walk {
    let mut walk = ignore::WalkBuilder::new(base);
    walk.hidden(false).follow_links(false).require_git(false);
    walk.filter_entry(|entry| {
        entry.file_name() != ".git" && !crate::safety::protected::is_protected(entry.path())
    });
    walk.build()
}

/// `glob`: relative matches from `base`, shown relative to `root`.
pub fn glob(
    root: &Path,
    base: &Path,
    pattern: &str,
    cancel: &CancellationToken,
    confine: &dyn Fn(&Path) -> Option<PathBuf>,
) -> Result<String> {
    let pattern = PathPattern::new(pattern)?;
    let mut shown = Vec::new();
    let mut more = 0;
    for (index, entry) in walk(base).enumerate() {
        if cancel.is_cancelled() {
            return Err(Error::Cancelled);
        }
        if index > SCAN_LIMIT {
            shown.push("[scan limit reached; narrow path]".to_owned());
            break;
        }
        let Ok(entry) = entry else { continue };
        if !entry.file_type().is_some_and(|kind| kind.is_file()) {
            continue;
        }
        let relative = display(base, entry.path());
        if !pattern.matches(&relative) {
            continue;
        }
        if shown.len() >= GLOB_LIMIT {
            more += 1;
            continue;
        }
        if let Some(full) = confine(entry.path()) {
            shown.push(display(root, &full));
        }
    }
    if shown.is_empty() {
        return Ok("No files found".into());
    }
    let mut out = shown.join("\n");
    if more > 0 {
        out.push_str(&format!("\n... and {more} more"));
    }
    Ok(out)
}

/// `grep` through ripgrep: returns every output line (rg stops each file at the cap).
pub async fn ripgrep_search(
    rg: &Path,
    root: &Path,
    target: &str,
    options: &GrepOptions,
    cancel: &CancellationToken,
) -> Result<Vec<String>> {
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
    for dir in crate::safety::protected::below(root) {
        command.arg(format!("--glob=!{dir}"));
        command.arg(format!("--glob=!{dir}/**"));
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
    if let Some(include) = &options.include {
        command.arg(format!("--glob={include}"));
    }
    command
        .args(["-e", &options.pattern, "--", target])
        .current_dir(root)
        .stdin(std::process::Stdio::null())
        .kill_on_drop(true);
    let output = tokio::select! {
        _ = cancel.cancelled() => return Err(Error::Cancelled),
        output = command.output() => output?,
    };
    let stdout = String::from_utf8_lossy(&output.stdout);
    match output.status.code() {
        Some(0) => {}
        Some(1) => return Ok(Vec::new()),
        // Exit 2 with output: some files were unreadable; the matches still answer.
        _ if !stdout.trim().is_empty() => {}
        _ => {
            return Err(Error::Tool(format!(
                "rg: {}",
                String::from_utf8_lossy(&output.stderr).trim()
            )));
        }
    }
    Ok(stdout
        .lines()
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
        .collect())
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

/// The built-in search, in ripgrep's output format; stops one line past the cap.
pub fn builtin_grep(
    root: &Path,
    base: &Path,
    options: &GrepOptions,
    cancel: &CancellationToken,
    confine: &dyn Fn(&Path) -> Option<PathBuf>,
) -> Result<Vec<String>> {
    let regex = regex::RegexBuilder::new(&options.pattern)
        .case_insensitive(options.ignore_case)
        .size_limit(1024 * 1024)
        .build()
        .map_err(|err| Error::Tool(format!("invalid regex: {err}")))?;
    let include = options
        .include
        .as_deref()
        .map(PathPattern::new)
        .transpose()?;
    let mut collector = Collector {
        lines: Vec::new(),
        printed: false,
    };
    if base.is_file() {
        collector.file(root, base, &regex, options);
        return Ok(collector.lines);
    }
    for (index, entry) in walk(base).enumerate() {
        if cancel.is_cancelled() {
            return Err(Error::Cancelled);
        }
        if collector.full() || index > SCAN_LIMIT {
            break;
        }
        let Ok(entry) = entry else { continue };
        if !entry.file_type().is_some_and(|kind| kind.is_file()) {
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
    Ok(collector.lines)
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
        if bytes[..bytes.len().min(8192)].contains(&0) {
            return;
        }
        let text = match crate::tools::text_encoding::sniff(&bytes) {
            crate::tools::text_encoding::TextKind::Utf8 => {
                String::from_utf8_lossy(&bytes).into_owned()
            }
            crate::tools::text_encoding::TextKind::Legacy => {
                match crate::tools::text_encoding::decode_gbk(&bytes) {
                    Some((text, _)) => text,
                    None => String::from_utf8_lossy(&bytes).into_owned(),
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
        crate::tools::text_encoding::clip_bytes(line.trim_end_matches('\r'), 500, " [... omitted]")
    )
}

/// Joins grep lines, cutting at `GREP_LINES`. `counted` means `lines` is everything rg
/// printed, so the note gives a lower bound.
pub fn grep_result(lines: Vec<String>, counted: bool, options: &GrepOptions) -> String {
    if lines.is_empty() {
        return "No matches found".into();
    }
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
        };
        let lines = builtin_grep(
            dir.path(),
            dir.path(),
            &options,
            &CancellationToken::new(),
            &|path| Some(path.to_path_buf()),
        )
        .unwrap();
        assert_eq!(
            lines,
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
        };
        let out = grep_result(lines, true, &options);
        assert!(out.ends_with(
            "... at least 30 more matches not shown (narrow the pattern, or use include/path)"
        ));
        assert_eq!(out.lines().count(), 101);
    }

    #[test]
    fn ripgrep_paths_use_forward_slashes() {
        assert_eq!(normalize_rg_path("src\\a-b.rs:3:x-1"), "src/a-b.rs:3:x-1");
        assert_eq!(normalize_rg_path("src\\a.rs-4-ctx"), "src/a.rs-4-ctx");
        assert_eq!(normalize_rg_path("src\\only.rs"), "src/only.rs");
    }
}
