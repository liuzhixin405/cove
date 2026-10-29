//! Instruction files of subdirectories (AGENTS.md, CLAUDE.md, ...), added to a tool result
//! the first time a tool touches a path below them. The workspace root's own files are
//! already in the system prompt.

use std::{collections::HashSet, path::PathBuf, sync::Mutex};

use futures_util::future::BoxFuture;
use serde_json::Value;

use crate::{engine::turn::TurnHook, session::Session, tools::ToolOutput};

const HINT_FILES: &[&str] = &["AGENTS.md", "CLAUDE.md", ".cursorrules", ".cove.md"];
const FILE_LIMIT: usize = 12 * 1024;
const CALL_LIMIT: usize = 24 * 1024;
const LEVELS: usize = 5;

#[derive(Default)]
struct State {
    /// Directories whose files were all shown.
    seen: HashSet<PathBuf>,
    shown: HashSet<PathBuf>,
    compactions: Option<u32>,
}

pub struct SubdirHints {
    root: PathBuf,
    state: Mutex<State>,
}

impl SubdirHints {
    pub fn new(root: PathBuf) -> Self {
        Self {
            root: root.canonicalize().unwrap_or(root),
            state: Mutex::new(State::default()),
        }
    }

    /// Hints for the directory of `path` and its parents below the workspace root.
    fn check(&self, state: &mut State, path: &str, budget: &mut usize) -> Vec<String> {
        let path = path.trim().trim_matches(['"', '\'']);
        if path.is_empty() {
            return Vec::new();
        }
        let candidate = self.root.join(path);
        let dir = if candidate.is_dir() {
            candidate
        } else {
            match candidate.parent() {
                Some(parent) => parent.to_path_buf(),
                None => return Vec::new(),
            }
        };
        let Ok(mut current) = dir.canonicalize() else {
            return Vec::new();
        };
        let mut hints = Vec::new();
        for _ in 0..LEVELS {
            if current == self.root || !current.starts_with(&self.root) {
                break;
            }
            if !state.seen.contains(&current) {
                let mut skipped = false;
                for name in HINT_FILES {
                    let file = current.join(name);
                    if state.shown.contains(&file) {
                        continue;
                    }
                    let Ok(text) = std::fs::read_to_string(&file) else {
                        continue;
                    };
                    let text = text.trim();
                    if text.is_empty() {
                        continue;
                    }
                    let relative = crate::tools::display_path(&self.root, &file);
                    let hint = format!(
                        "[Context from {relative}]\n{}",
                        crate::tools::text_encoding::clip_bytes(
                            text,
                            FILE_LIMIT,
                            "\n[...truncated]"
                        )
                    );
                    if hint.len() > *budget {
                        skipped = true;
                        continue;
                    }
                    *budget -= hint.len();
                    state.shown.insert(file);
                    hints.push(hint);
                }
                if skipped {
                    break;
                }
                state.seen.insert(current.clone());
            }
            if !current.pop() {
                break;
            }
        }
        hints
    }
}

/// Path arguments of file tools, and path-like words of shell commands.
fn paths(name: &str, input: &Value) -> Vec<String> {
    match name {
        "shell" => input
            .get("command")
            .and_then(Value::as_str)
            .map(|command| {
                command
                    .split(|c: char| c.is_whitespace() || matches!(c, ';' | '|' | '&' | '(' | ')'))
                    .filter(|word| {
                        !word.starts_with('-') && (word.contains('/') || word.contains('\\'))
                    })
                    .map(str::to_owned)
                    .collect()
            })
            .unwrap_or_default(),
        _ => ["path", "file_path", "filePath"]
            .iter()
            .filter_map(|key| input.get(*key).and_then(Value::as_str))
            .map(str::to_owned)
            .collect(),
    }
}

impl TurnHook for SubdirHints {
    fn task_note<'a>(&'a self, session: &'a Session) -> BoxFuture<'a, Option<String>> {
        Box::pin(async move {
            // Compaction may have summarized earlier hints away: show them again.
            if let Ok(mut state) = self.state.lock()
                && state.compactions != Some(session.compactions)
            {
                *state = State {
                    compactions: Some(session.compactions),
                    ..State::default()
                };
            }
            None
        })
    }

    fn tool_note(&self, name: &str, input: &Value, output: &ToolOutput) -> Option<String> {
        if output.failed {
            return None;
        }
        let paths = paths(name, input);
        if paths.is_empty() {
            return None;
        }
        let mut state = self.state.lock().ok()?;
        let mut budget = CALL_LIMIT;
        let hints: Vec<String> = paths
            .iter()
            .flat_map(|path| self.check(&mut state, path, &mut budget))
            .collect();
        (!hints.is_empty()).then(|| hints.join("\n\n"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn ok() -> ToolOutput {
        ToolOutput {
            content: String::new(),
            failed: false,
            ..Default::default()
        }
    }

    #[test]
    fn hints_appear_once_per_file_and_never_above_the_workspace() {
        let root = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(root.path().join("pkg/sub")).unwrap();
        std::fs::write(root.path().join("AGENTS.md"), "root rules").unwrap();
        std::fs::write(root.path().join("pkg/AGENTS.md"), "pkg rules").unwrap();
        std::fs::write(root.path().join("pkg/sub/.cursorrules"), "sub rules").unwrap();
        std::fs::write(root.path().join("pkg/sub/a.rs"), "").unwrap();
        let hints = SubdirHints::new(root.path().into());
        let note = hints
            .tool_note("read", &json!({"path":"pkg/sub/a.rs"}), &ok())
            .unwrap();
        assert!(
            note.contains("[Context from pkg/sub/.cursorrules]\nsub rules"),
            "{note}"
        );
        assert!(
            note.contains("[Context from pkg/AGENTS.md]\npkg rules"),
            "{note}"
        );
        assert!(!note.contains("root rules"), "{note}");
        assert!(
            hints
                .tool_note("edit", &json!({"path":"pkg/sub/a.rs"}), &ok())
                .is_none()
        );
        let shell = SubdirHints::new(root.path().into());
        let note = shell
            .tool_note(
                "shell",
                &json!({"command":"cat pkg/sub/a.rs | wc -l"}),
                &ok(),
            )
            .unwrap();
        assert!(note.contains("sub rules"), "{note}");
    }
}
