//! Project context for the system prompt and per-task notes.

use std::{
    collections::{BTreeMap, HashSet},
    path::{Path, PathBuf},
    process::{Command, Stdio},
    sync::Mutex,
};

use sha2::{Digest, Sha256};

/// Instruction file names, read in this order from every directory between the git root and cwd.
const INSTRUCTION_FILES: &[&str] = &["CLAUDE.md", ".claude/CLAUDE.md", "AGENTS.md", ".cove.md"];
const INSTRUCTIONS_LIMIT: usize = 32 * 1024;
const OUTLINE_LIMIT: usize = 4 * 1024;
const SKILLS_LIMIT: usize = 8 * 1024;

/// Runs a read-only git query; `None` when git is missing or the directory is not a repository.
pub fn git(cwd: &Path, args: &[&str]) -> Option<String> {
    let output = Command::new("git")
        .args(args)
        .current_dir(cwd)
        .env("GIT_OPTIONAL_LOCKS", "0")
        .env("GIT_TERMINAL_PROMPT", "0")
        .stdin(Stdio::null())
        .stderr(Stdio::null())
        .output()
        .ok()?;
    output.status.success().then(|| {
        String::from_utf8_lossy(&output.stdout)
            .trim_end()
            .to_owned()
    })
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
            let room = INSTRUCTIONS_LIMIT.saturating_sub(total);
            if room == 0 {
                clipped = true;
                break;
            }
            let text = if text.len() > room {
                clipped = true;
                let mut end = room;
                while !text.is_char_boundary(end) {
                    end -= 1;
                }
                format!("{}\n[clipped at 32 KiB of instruction files]", &text[..end])
            } else {
                text
            };
            total += text.len();
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
        cwd.display(),
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

/// System prompt sections describing the project; stable for the whole process.
pub fn system_context(cwd: &Path, skills: &[(String, String)]) -> String {
    let mut out = format!("\n\n# Environment\n{}\n", environment(cwd));
    let (files, _) = instruction_files(cwd);
    if !files.is_empty() {
        out.push_str(
            "\n# Project instructions\nFrom instruction files in this project. Follow them for this project unless they conflict with the user's request or the safety rules above.\n",
        );
        for (path, text) in files {
            let shown = path
                .strip_prefix(cwd)
                .unwrap_or(&path)
                .display()
                .to_string();
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

/// Text added to a new task's message; `None` when there is nothing new to say.
pub trait TurnNote: Send + Sync {
    fn note(&self) -> Option<String>;
}

/// Git branch, short status and recent commits, repeated only when they change.
pub struct GitNote {
    cwd: PathBuf,
    last: Mutex<Option<String>>,
}

impl GitNote {
    pub fn new(cwd: PathBuf) -> Self {
        Self {
            cwd,
            last: Mutex::new(None),
        }
    }

    fn state(&self) -> Option<String> {
        let branch = git_branch(&self.cwd)?;
        let status = git(&self.cwd, &["status", "--porcelain"]).unwrap_or_default();
        let lines: Vec<&str> = status.lines().collect();
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
        if let Some(log) = git(&self.cwd, &["log", "--oneline", "-5"]).filter(|log| !log.is_empty())
        {
            out.push_str("Recent commits:\n");
            for line in log.lines() {
                out.push_str(&format!("  {line}\n"));
            }
        }
        Some(out)
    }
}

impl TurnNote for GitNote {
    fn note(&self) -> Option<String> {
        let state = self.state()?;
        let mut last = self.last.lock().ok()?;
        if last.as_deref() == Some(state.as_str()) {
            return None;
        }
        *last = Some(state.clone());
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
        assert!(note.note().unwrap().contains("?? new.txt"));
    }
}
