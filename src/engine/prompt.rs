//! Project context for the system prompt and per-task notes.

use std::{
    collections::{BTreeMap, HashSet},
    path::{Path, PathBuf},
    sync::Mutex,
};

use sha2::{Digest, Sha256};

/// Instruction file names, read in this order from every directory between the git root and cwd.
/// The rules file comes first so it is never skipped because the shared budget ran out.
const INSTRUCTION_FILES: &[&str] = &[
    crate::memory::rules::FILE,
    "CLAUDE.md",
    ".claude/CLAUDE.md",
    "AGENTS.md",
    ".cove.md",
];
const INSTRUCTIONS_LIMIT: usize = 32 * 1024;
/// Rules get their own budget, above `MAX_RULES` × `MAX_CHARS` even for Chinese
/// text (about 54 KiB), so they are applied in full.
const RULES_LIMIT: usize = 64 * 1024;
/// The user preferences, at most 20 × 200 characters when written by cove (about
/// 12 KiB of Chinese at the very most, usually far less).
const PREFERENCES_LIMIT: usize = 8 * 1024;
const OUTLINE_LIMIT: usize = 4 * 1024;
const SKILLS_LIMIT: usize = 8 * 1024;

/// Runs a read-only git query; `None` when git is missing, the directory is not a
/// repository or git does not answer within 10 s (startup never hangs on it).
pub fn git(cwd: &Path, args: &[&str]) -> Option<String> {
    crate::process::git::Git::new(cwd)
        .args(args.iter().copied())
        .timeout(std::time::Duration::from_secs(10))
        .query_blocking()
}

/// Current branch, also before the first commit; "HEAD (detached)" when not on a branch.
pub fn git_branch(cwd: &Path) -> Option<String> {
    let branch = git(cwd, &["branch", "--show-current"])?;
    Some(if branch.is_empty() {
        "HEAD (detached)".into()
    } else {
        branch
    })
}

pub fn git_root(cwd: &Path) -> Option<PathBuf> {
    cwd.ancestors()
        .find(|dir| dir.join(".git").exists())
        .map(Path::to_path_buf)
}

/// Instruction files from the git root down to `cwd` (outer first), deduplicated, capped at 32 KiB.
pub fn instruction_files(cwd: &Path) -> (Vec<(PathBuf, String)>, bool) {
    let mut dirs: Vec<&Path> = match git_root(cwd) {
        Some(root) => cwd
            .ancestors()
            .take_while(|dir| dir.starts_with(&root))
            .collect(),
        None => vec![cwd],
    };
    dirs.reverse();
    let mut seen_paths = HashSet::new();
    let mut seen_content = HashSet::new();
    let mut files = Vec::new();
    let mut total = 0;
    let mut rules_total = 0;
    let mut clipped = false;
    for dir in dirs {
        for name in INSTRUCTION_FILES {
            let path = dir.join(name);
            let Ok(canonical) = path.canonicalize() else {
                continue;
            };
            if !canonical.is_file() || !seen_paths.insert(canonical.clone()) {
                continue;
            }
            if std::fs::metadata(&canonical).is_ok_and(|meta| meta.len() > 1024 * 1024) {
                continue;
            }
            let Ok(text) = std::fs::read_to_string(&canonical) else {
                continue;
            };
            let text = text.trim().to_owned();
            if text.is_empty() || !seen_content.insert(Sha256::digest(text.as_bytes())) {
                continue;
            }
            let is_rules = *name == crate::memory::rules::FILE;
            let limit = if is_rules { RULES_LIMIT } else { INSTRUCTIONS_LIMIT };
            let used = if is_rules { rules_total } else { total };
            let room = limit.saturating_sub(used);
            if room == 0 {
                clipped = true;
                break;
            }
            let text = if text.len() > room {
                clipped = true;
                let head = crate::text::head(&text, room);
                let note = if is_rules {
                    "rules clipped at 64 KiB"
                } else {
                    "clipped at 32 KiB of instruction files"
                };
                format!("{head}\n[{note}]")
            } else {
                text
            };
            if is_rules {
                rules_total += text.len();
            } else {
                total += text.len();
            }
            files.push((path, text));
        }
    }
    (files, clipped)
}

fn shell_name() -> String {
    crate::process::Shell::current().describe()
}

/// One static line: working directory, platform, shell, git branch and user.
pub fn environment(cwd: &Path) -> String {
    let mut line = format!(
        "Working directory: {} | Platform: {}/{} | Shell: {}",
        crate::paths::display(cwd),
        std::env::consts::OS,
        std::env::consts::ARCH,
        shell_name()
    );
    if let Some(branch) = git_branch(cwd) {
        line.push_str(&format!(" | Git branch: {branch}"));
    }
    if let Some(user) = git(cwd, &["config", "user.name"]).filter(|user| !user.is_empty()) {
        line.push_str(&format!(" | Git user: {user}"));
    }
    line
}

fn language(extension: &str) -> Option<&'static str> {
    Some(match extension {
        "rs" => "Rust",
        "go" => "Go",
        "py" => "Python",
        "ts" | "tsx" => "TypeScript",
        "js" | "jsx" | "mjs" | "cjs" => "JavaScript",
        "cs" => "C#",
        "java" => "Java",
        "kt" => "Kotlin",
        "c" | "h" => "C",
        "cpp" | "cc" | "hpp" => "C++",
        "rb" => "Ruby",
        "php" => "PHP",
        "swift" => "Swift",
        "vue" => "Vue",
        _ => return None,
    })
}

/// Build and test commands a developer would run here.
pub fn project_commands(cwd: &Path) -> Vec<String> {
    let has = |name: &str| cwd.join(name).is_file();
    let mut commands = Vec::new();
    if has("Cargo.toml") {
        commands.extend([
            "cargo build".into(),
            "cargo test".into(),
            "cargo clippy --all-targets".into(),
        ]);
    }
    if has("go.mod") {
        commands.extend(["go build ./...".into(), "go test ./...".into()]);
    }
    if let Ok(text) = std::fs::read_to_string(cwd.join("package.json"))
        && let Ok(value) = serde_json::from_str::<serde_json::Value>(&text)
        && let Some(scripts) = value.get("scripts").and_then(|scripts| scripts.as_object())
    {
        for name in ["build", "test", "lint", "typecheck"] {
            if scripts.contains_key(name) {
                commands.push(format!("npm run {name}"));
            }
        }
    }
    if has("pyproject.toml") || has("setup.py") {
        commands.push("python -m pytest".into());
    }
    if crate::safety::verify::detect(cwd)
        .iter()
        .any(|command| command == "dotnet build")
    {
        commands.extend(["dotnet build".into(), "dotnet test".into()]);
    }
    commands
}

/// Languages, top-level layout, entry points and commands; at most 4 KiB.
pub fn outline(cwd: &Path) -> String {
    let mut counts: BTreeMap<&str, usize> = BTreeMap::new();
    let mut walk = ignore::WalkBuilder::new(cwd);
    walk.hidden(false).follow_links(false).require_git(false);
    walk.filter_entry(|entry| {
        entry.file_name() != ".git" && !crate::safety::protected::is_protected(entry.path())
    });
    for entry in walk.build().flatten().take(20_000) {
        if let Some(name) = entry
            .path()
            .extension()
            .and_then(|extension| extension.to_str())
            .and_then(language)
        {
            *counts.entry(name).or_default() += 1;
        }
    }
    let mut languages: Vec<(&str, usize)> = counts.into_iter().collect();
    languages.sort_by_key(|entry| std::cmp::Reverse(entry.1));
    let mut out = String::new();
    if !languages.is_empty() {
        let list: Vec<String> = languages
            .iter()
            .take(6)
            .map(|(name, count)| format!("{name} ({count} files)"))
            .collect();
        out.push_str(&format!("Languages: {}\n", list.join(", ")));
    }
    let mut entries: Vec<String> = std::fs::read_dir(cwd)
        .into_iter()
        .flatten()
        .flatten()
        .filter_map(|entry| {
            let name = entry.file_name().to_string_lossy().into_owned();
            if name.starts_with('.') && name != ".github" {
                return None;
            }
            let dir = entry.file_type().is_ok_and(|kind| kind.is_dir());
            if dir
                && ["target", "node_modules", "dist", "build", "bin", "obj"]
                    .contains(&name.as_str())
            {
                return None;
            }
            Some(if dir { format!("{name}/") } else { name })
        })
        .collect();
    entries.sort();
    if !entries.is_empty() {
        let shown = entries.len().min(40);
        out.push_str(&format!("Top level: {}", entries[..shown].join(" ")));
        if entries.len() > shown {
            out.push_str(&format!(" (+{} more)", entries.len() - shown));
        }
        out.push('\n');
    }
    let entry_points: Vec<&str> = [
        "src/main.rs",
        "src/lib.rs",
        "main.go",
        "cmd",
        "src/index.ts",
        "src/main.ts",
        "index.js",
        "src/index.js",
        "main.py",
        "app.py",
        "manage.py",
        "Program.cs",
    ]
    .into_iter()
    .filter(|path| cwd.join(path).exists())
    .collect();
    if !entry_points.is_empty() {
        out.push_str(&format!("Entry points: {}\n", entry_points.join(", ")));
    }
    let commands = project_commands(cwd);
    if !commands.is_empty() {
        out.push_str(&format!("Build/test: {}\n", commands.join("; ")));
    }
    if !out.is_empty() {
        out.push_str("Use repo_map for files, symbols and line numbers.\n");
    }
    if out.len() > OUTLINE_LIMIT {
        let mut end = OUTLINE_LIMIT;
        while !out.is_char_boundary(end) {
            end -= 1;
        }
        out.truncate(end);
    }
    out
}

/// The "User preferences" section for `preferences` (`memory::rules`, the user's own
/// file); empty when there are none. Above [`PREFERENCES_LIMIT`] the rest is cut and
/// marked: writes keep the file far below it, only a hand edit gets there.
pub fn preferences_section(preferences: &[String]) -> String {
    if preferences.is_empty() {
        return String::new();
    }
    let mut body: String = preferences
        .iter()
        .map(|preference| format!("- {preference}\n"))
        .collect();
    if body.len() > PREFERENCES_LIMIT {
        body = format!(
            "{}\n[preferences clipped at 8 KiB]\n",
            crate::text::head(&body, PREFERENCES_LIMIT)
        );
    }
    format!(
        "\n# User preferences\nThe user's own preferences, for every project. Follow them unless the project instructions below, or the user's current request, say otherwise; those win.\n<preferences>\n{body}</preferences>\n"
    )
}

/// System prompt sections describing the project, after the user's `preferences`;
/// stable for the whole process.
pub fn system_context(cwd: &Path, skills: &[(String, String)], preferences: &[String]) -> String {
    let mut out = format!("\n\n# Environment\n{}\n", environment(cwd));
    out.push_str(&preferences_section(preferences));
    let (files, _) = instruction_files(cwd);
    if !files.is_empty() {
        out.push_str(
            "\n# Project instructions\nFrom instruction files in this project. Follow them for this project unless they conflict with the user's request or the safety rules above.\n",
        );
        for (path, text) in files {
            let shown = path
                .strip_prefix(cwd)
                .unwrap_or(&path)
                .to_string_lossy()
                .into_owned();
            out.push_str(&format!(
                "<instructions file=\"{}\">\n{text}\n</instructions>\n",
                shown.replace('\\', "/")
            ));
        }
    }
    let outline = outline(cwd);
    if !outline.is_empty() {
        out.push_str(&format!("\n# Project outline\n{outline}"));
    }
    if !skills.is_empty() {
        let mut index = String::from(
            "\n# Available skills\nLoad one with the skill tool when the task matches its description.\n",
        );
        for (name, description) in skills {
            let line = format!(
                "- {name}: {}\n",
                description.lines().next().unwrap_or_default()
            );
            if index.len() + line.len() > SKILLS_LIMIT {
                index.push_str("- ... (more: skill action=list)\n");
                break;
            }
            index.push_str(&line);
        }
        out.push_str(&index);
    }
    out
}

/// Above this many bytes of changed instruction files, a [`context_changes`] note
/// names them instead of quoting them.
const CHANGE_NOTE_LIMIT: usize = 16 * 1024;

/// The key of the user preferences among [`announced_parts`]; no file is named so.
const PREFERENCES_PART: &str = "\0preferences";

/// The parts of a [`system_context`] that steer what the model does: the user
/// preferences and each instruction file, as `(name, text)`. The environment line,
/// outline and skills are left out: they only orient the model, and it can look them
/// up again.
fn announced_parts(context: &str) -> Vec<(String, String)> {
    let mut parts = Vec::new();
    if let Some(start) = context.find("\n# User preferences\n") {
        let rest = &context[start..];
        if let Some(open) = rest.find("<preferences>\n") {
            let body = &rest[open + "<preferences>\n".len()..];
            if let Some(close) = body.find("</preferences>\n") {
                parts.push((PREFERENCES_PART.to_owned(), body[..close].to_owned()));
            }
        }
    }
    if let Some(start) = context.find("\n# Project instructions\n") {
        let rest = &context[start..];
        // The outline and the skill index are cove's own text after the files, so the
        // last heading of them ends the instructions even if a file has such a line.
        let end = rest
            .rfind("\n# Project outline\n")
            .or_else(|| rest.rfind("\n# Available skills\n"))
            .unwrap_or(rest.len());
        for block in rest[..end].split("<instructions file=\"").skip(1) {
            let Some((name, body)) = block.split_once("\">\n") else {
                continue;
            };
            let body = body.strip_suffix("\n</instructions>\n").unwrap_or(body);
            parts.push((name.to_owned(), body.to_owned()));
        }
    }
    parts
}

/// A digest of the preferences and instruction files in `context`: a resumed session
/// records it once it told the model about them, so the same change is told once.
pub fn announced_mark(context: &str) -> String {
    let mut digest = Sha256::new();
    for (name, text) in announced_parts(context) {
        digest.update(name.as_bytes());
        digest.update([0]);
        digest.update(text.as_bytes());
        digest.update([0]);
    }
    digest.finalize()[..8]
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect()
}

/// What changed in the user preferences and instruction files from the project
/// context a resumed session keeps (`saved`) to the current one: a note for the next
/// task's message, as `/rules` announces added rules; `None` when neither changed (the
/// environment, outline and skills are not compared).
pub fn context_changes(saved: &str, current: &str) -> Option<String> {
    let before = announced_parts(saved);
    let after = announced_parts(current);
    let mut changed: Vec<(&str, Option<&str>)> = after
        .iter()
        .filter(|part| !before.contains(part))
        .map(|(name, text)| (name.as_str(), Some(text.as_str())))
        .collect();
    changed.extend(
        before
            .iter()
            .filter(|(name, _)| !after.iter().any(|(other, _)| other == name))
            .map(|(name, _)| (name.as_str(), None)),
    );
    if changed.is_empty() {
        return None;
    }
    let files: usize = changed
        .iter()
        .filter(|(name, _)| *name != PREFERENCES_PART)
        .filter_map(|(_, text)| *text)
        .map(str::len)
        .sum();
    let mut note = String::from(
        "[project context] Changed since this session started; this replaces what the system prompt says about them:\n",
    );
    for (name, text) in changed {
        match (name == PREFERENCES_PART, text) {
            (true, None) => note.push_str("- User preferences: all removed\n"),
            // At most 8 KiB, and outside the workspace the read tool can open.
            (true, Some(text)) => note.push_str(&format!(
                "User preferences:\n<preferences>\n{text}</preferences>\n"
            )),
            (false, None) => note.push_str(&format!("- {name}: removed\n")),
            (false, Some(_)) if files > CHANGE_NOTE_LIMIT => note.push_str(&format!(
                "- {name}: changed; read it before relying on it\n"
            )),
            (false, Some(text)) => note.push_str(&format!(
                "<instructions file=\"{name}\">\n{text}\n</instructions>\n"
            )),
        }
    }
    Some(note)
}

/// Text added to a new task's message; `None` when there is nothing new to say.
pub trait TurnNote: Send + Sync {
    fn note(&self) -> Option<String>;
}

/// Git branch, short status and recent commits, repeated only when they change.
pub struct GitNote {
    cwd: PathBuf,
    last: Mutex<Option<GitNoteState>>,
}

struct GitNoteState {
    signature: String,
    head: String,
    recent: String,
}

impl GitNote {
    pub fn new(cwd: PathBuf) -> Self {
        Self {
            cwd,
            last: Mutex::new(None),
        }
    }

    fn state(&self) -> Option<String> {
        // Porcelain v2 includes branch and HEAD in the same query as the worktree status.
        // Avoid three git process launches for every new task, and only read the recent log
        // when HEAD changes.
        let status = git(&self.cwd, &["status", "--porcelain=v2", "--branch"])?;
        let mut branch = "HEAD (detached)";
        let mut head = String::new();
        for line in status.lines() {
            if let Some(value) = line.strip_prefix("# branch.head ") {
                if value != "(detached)" {
                    branch = value;
                }
            } else if let Some(value) = line.strip_prefix("# branch.oid ") {
                head = value.to_owned();
            }
        }
        let signature = status.clone();
        let mut last = self.last.lock().ok()?;
        if last
            .as_ref()
            .is_some_and(|previous| previous.signature == signature)
        {
            return None;
        }
        let recent = match last.as_ref().filter(|previous| previous.head == head) {
            Some(previous) => previous.recent.clone(),
            None => git(&self.cwd, &["log", "--oneline", "-5"]).unwrap_or_default(),
        };
        let lines: Vec<&str> = status
            .lines()
            .filter(|line| !line.starts_with("# "))
            .collect();
        let mut out = format!("Git branch: {branch}\n");
        if lines.is_empty() {
            out.push_str("Working tree: clean\n");
        } else {
            out.push_str("Working tree changes:\n");
            for line in lines.iter().take(15) {
                out.push_str(&format!("  {line}\n"));
            }
            if lines.len() > 15 {
                out.push_str(&format!("  ... {} more\n", lines.len() - 15));
            }
        }
        if !recent.is_empty() {
            out.push_str("Recent commits:\n");
            for line in recent.lines() {
                out.push_str(&format!("  {line}\n"));
            }
        }
        *last = Some(GitNoteState {
            signature,
            head,
            recent,
        });
        Some(out)
    }
}

impl TurnNote for GitNote {
    fn note(&self) -> Option<String> {
        let state = self.state()?;
        Some(format!("<environment>\n{state}</environment>"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn instruction_files_are_read_outer_first_and_deduplicated() {
        let root = tempfile::tempdir().unwrap();
        std::fs::create_dir(root.path().join(".git")).unwrap();
        let sub = root.path().join("crates/app");
        std::fs::create_dir_all(sub.join(".claude")).unwrap();
        std::fs::write(root.path().join("AGENTS.md"), "root rules").unwrap();
        std::fs::write(sub.join("CLAUDE.md"), "app rules").unwrap();
        std::fs::write(sub.join(".claude/CLAUDE.md"), "app rules").unwrap();
        std::fs::write(sub.join(".cove.md"), "").unwrap();
        let (files, clipped) = instruction_files(&sub);
        let texts: Vec<&str> = files.iter().map(|(_, text)| text.as_str()).collect();
        assert_eq!(texts, ["root rules", "app rules"]);
        assert!(!clipped);
    }

    #[test]
    fn rules_are_complete_even_when_the_shared_budget_is_full() {
        let root = tempfile::tempdir().unwrap();
        std::fs::create_dir(root.path().join(".git")).unwrap();
        std::fs::create_dir_all(root.path().join(".cove")).unwrap();
        std::fs::write(
            root.path().join("AGENTS.md"),
            "a".repeat(INSTRUCTIONS_LIMIT + 1024),
        )
        .unwrap();
        let rule = format!("- {}", "规".repeat(200));
        std::fs::write(root.path().join(crate::memory::rules::FILE), &rule).unwrap();
        let (files, clipped) = instruction_files(root.path());
        assert!(clipped);
        let rules = files
            .iter()
            .find(|(path, _)| path.ends_with(crate::memory::rules::FILE))
            .expect("rules file included");
        assert!(rules.1.contains(&rule), "rules were clipped");
        let agents = files
            .iter()
            .find(|(path, _)| path.ends_with("AGENTS.md"))
            .expect("AGENTS.md included");
        assert!(
            agents.1.ends_with("[clipped at 32 KiB of instruction files]"),
            "{}",
            agents.1
        );
    }

    #[test]
    fn preferences_come_before_the_project_instructions_and_say_who_wins() {
        let root = tempfile::tempdir().unwrap();
        std::fs::create_dir(root.path().join(".git")).unwrap();
        std::fs::write(root.path().join("AGENTS.md"), "README is generated").unwrap();
        let context = system_context(root.path(), &[], &["改了命令就更新 README".into()]);
        let preferences = context.find("# User preferences").expect("section");
        let project = context.find("# Project instructions").expect("project");
        assert!(preferences < project, "{context}");
        assert!(context.contains("<preferences>\n- 改了命令就更新 README\n</preferences>"));
        assert!(context.contains("project instructions below, or the user's current request"));
        let none = system_context(root.path(), &[], &[]);
        assert!(!none.contains("# User preferences"), "{none}");
        assert!(preferences_section(&[]).is_empty());
        let many: Vec<String> = (0..200)
            .map(|n| format!("{n} {}", "偏".repeat(20)))
            .collect();
        let clipped = preferences_section(&many);
        assert!(clipped.contains("[preferences clipped at 8 KiB]"));
        assert!(clipped.len() < PREFERENCES_LIMIT + 512, "{}", clipped.len());
    }

    #[test]
    fn only_preference_and_instruction_changes_are_told() {
        let root = tempfile::tempdir().unwrap();
        std::fs::create_dir(root.path().join(".git")).unwrap();
        std::fs::create_dir(root.path().join("src")).unwrap();
        std::fs::write(root.path().join("Cargo.toml"), "[package]").unwrap();
        std::fs::write(root.path().join("src/main.rs"), "fn main() {}").unwrap();
        std::fs::write(root.path().join("AGENTS.md"), "Run cargo test.").unwrap();
        let skills = [("deploy".to_owned(), "Deploy it".to_owned())];
        let saved = system_context(root.path(), &skills, &["中文回复".into()]);
        // A new file and a new skill change the context, but not what is compared.
        std::fs::write(root.path().join("src/lib.rs"), "").unwrap();
        let grown = system_context(root.path(), &[], &["中文回复".into()]);
        assert_ne!(saved, grown);
        assert_eq!(context_changes(&saved, &grown), None);
        assert_eq!(announced_mark(&saved), announced_mark(&grown));

        std::fs::write(root.path().join("AGENTS.md"), "Run cargo test --release.").unwrap();
        std::fs::write(root.path().join("CLAUDE.md"), "Keep CRLF.").unwrap();
        let edited = system_context(root.path(), &[], &["中文回复".into()]);
        let note = context_changes(&saved, &edited).expect("instructions changed");
        assert!(note.starts_with("[project context]"), "{note}");
        assert!(
            note.contains(
                "<instructions file=\"AGENTS.md\">\nRun cargo test --release.\n</instructions>"
            ) && note.contains("<instructions file=\"CLAUDE.md\">\nKeep CRLF.\n</instructions>"),
            "{note}"
        );
        assert!(
            !note.contains("preferences") && !note.contains("Languages"),
            "{note}"
        );
        assert_ne!(announced_mark(&saved), announced_mark(&edited));

        std::fs::remove_file(root.path().join("AGENTS.md")).unwrap();
        let removed = system_context(root.path(), &[], &[]);
        let note = context_changes(&edited, &removed).expect("removed");
        assert!(
            note.contains("- AGENTS.md: removed")
                && note.contains("- User preferences: all removed"),
            "{note}"
        );
        let preferred = system_context(root.path(), &[], &["先写测试".into()]);
        let note = context_changes(&removed, &preferred).expect("preferences");
        assert!(
            note.contains("<preferences>\n- 先写测试\n</preferences>"),
            "{note}"
        );

        // Too much to quote: named, to be read.
        std::fs::write(
            root.path().join("AGENTS.md"),
            "x".repeat(CHANGE_NOTE_LIMIT + 1),
        )
        .unwrap();
        let large = system_context(root.path(), &[], &["先写测试".into()]);
        let note = context_changes(&preferred, &large).expect("large");
        assert!(note.contains("- AGENTS.md: changed; read it"), "{note}");
        assert!(note.len() < 1024, "{}", note.len());
    }

    #[test]
    fn outline_names_languages_layout_and_commands() {
        let root = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(root.path().join("src")).unwrap();
        std::fs::create_dir_all(root.path().join("target/debug")).unwrap();
        std::fs::write(root.path().join("Cargo.toml"), "[package]").unwrap();
        std::fs::write(root.path().join("src/main.rs"), "fn main() {}").unwrap();
        let outline = outline(root.path());
        assert!(outline.contains("Languages: Rust (1 files)"), "{outline}");
        assert!(
            outline.contains("Top level: Cargo.toml src/\n"),
            "{outline}"
        );
        assert!(outline.contains("Entry points: src/main.rs"), "{outline}");
        assert!(
            outline.contains("Build/test: cargo build; cargo test"),
            "{outline}"
        );
    }

    #[test]
    fn git_note_repeats_only_after_a_change() {
        let root = tempfile::tempdir().unwrap();
        if git(root.path(), &["init", "-q"]).is_none() {
            return;
        }
        let note = GitNote::new(root.path().to_path_buf());
        let first = note.note().unwrap();
        assert!(first.contains("Working tree: clean"), "{first}");
        assert!(note.note().is_none());
        std::fs::write(root.path().join("new.txt"), "x").unwrap();
        assert!(note.note().unwrap().contains("? new.txt"));
    }
}
