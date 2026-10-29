//! Per-project session notes (`<data_dir>/projects/<hash>/session_notes.md`): decisions
//! the user stated, discoveries the agent reported and compaction events, found by
//! heuristics after each task and offered (bounded, by relevance) to later tasks.

use std::{
    collections::HashMap,
    path::{Path, PathBuf},
    sync::{Mutex, OnceLock},
};

use futures_util::future::BoxFuture;
use regex::Regex;
use sha2::{Digest, Sha256};

use crate::{
    domain::{Block, Result, Role},
    engine::turn::TurnHook,
    session::usage::timestamp,
    session::{Session, now},
};

const KEEP_LINES: usize = 200;
const NOTE_LIMIT: usize = 2048;
const MIN_CHARS: usize = 4;
const MAX_CHARS: usize = 200;

/// Negations and hand-offs that never precede a real decision or finding.
const ZH_NEGATIONS: &str = "没未不无你由";

struct Signal {
    regex: Regex,
    reject_before: &'static str,
    reject_after: &'static str,
}

fn signal(pattern: &str, reject_before: &'static str, reject_after: &'static str) -> Signal {
    Signal {
        regex: Regex::new(pattern).expect("valid signal pattern"),
        reject_before,
        reject_after,
    }
}

fn decisions() -> &'static [Signal] {
    static PATTERNS: OnceLock<Vec<Signal>> = OnceLock::new();
    PATTERNS.get_or_init(|| {
        vec![
            signal(
                r"(?i)\b(?:use|using|we.ll use|go with|let.s use|switch to|prefer|stick with)\s+(.+?)(?:\.|$)",
                "",
                "",
            ),
            signal(
                r"(?i)(?:I prefer|I like|I want|let.s go with)\s+(.+?)(?:\.|$)",
                "",
                "",
            ),
            signal(
                r"(?:改用|采用|决定|换成)\s*(.+?)(?:[。！!？?；;\n]|$)",
                ZH_NEGATIONS,
                "",
            ),
            signal(
                r"(?:^|[，,。；;！!？?\s])用\s*(.+?)(?:[。！!？?；;\n]|$)",
                "",
                "户例法途于来以",
            ),
        ]
    })
}

fn discoveries() -> &'static [Signal] {
    static PATTERNS: OnceLock<Vec<Signal>> = OnceLock::new();
    PATTERNS.get_or_init(|| {
        vec![
            signal(
                r"(?i)\b(?:I found|discovered|the issue is|the reason is|it turns out)\s+(.+?)(?:\.|$)",
                "",
                "",
            ),
            signal(
                r"(?i)(?:fixed by|resolved by|solved by)\s+(.+?)(?:\.|$)",
                "",
                "",
            ),
            signal(
                r"(?:发现|原因是)\s*[:：]?\s*(.+?)(?:[。！!？?；;\n]|$)",
                ZH_NEGATIONS,
                "",
            ),
        ]
    })
}

fn find(signal: &Signal, text: &str) -> Option<String> {
    for captures in signal.regex.captures_iter(text) {
        let whole = captures.get(0)?;
        let capture = captures.get(1)?;
        let before: String = text[..whole.start()].chars().rev().take(2).collect();
        if before.chars().any(|c| signal.reject_before.contains(c)) {
            continue;
        }
        if capture
            .as_str()
            .chars()
            .next()
            .is_some_and(|c| signal.reject_after.contains(c))
        {
            continue;
        }
        let found = capture.as_str().trim();
        let count = found.chars().count();
        if (MIN_CHARS..MAX_CHARS).contains(&count) {
            return Some(found.replace(['\n', '\r'], " "));
        }
    }
    None
}

/// (kind, text) signals in a user request and the agent's final reply.
pub fn signals(user: &str, assistant: &str) -> Vec<(&'static str, String)> {
    let mut found = Vec::new();
    for pattern in decisions() {
        if let Some(text) = find(pattern, user) {
            found.push(("decision", text));
        }
    }
    for pattern in discoveries() {
        if let Some(text) = find(pattern, assistant) {
            found.push(("discovery", text));
        }
    }
    found.dedup_by(|a, b| a.1 == b.1);
    found
}

pub struct SessionNotes {
    path: PathBuf,
    seen: Mutex<HashMap<String, u32>>,
}

impl SessionNotes {
    pub fn new(data_dir: &Path, workspace: &Path) -> Self {
        let hash = format!(
            "{:x}",
            Sha256::digest(workspace.as_os_str().as_encoded_bytes())
        );
        Self {
            path: data_dir
                .join("projects")
                .join(hash)
                .join("session_notes.md"),
            seen: Mutex::new(HashMap::new()),
        }
    }

    pub fn path(&self) -> &Path {
        &self.path
    }

    fn lines(&self) -> Vec<String> {
        std::fs::read_to_string(&self.path)
            .unwrap_or_default()
            .lines()
            .filter(|line| line.starts_with("- "))
            .map(str::to_owned)
            .collect()
    }

    /// Appends one note (deduplicated), keeping the newest `KEEP_LINES`.
    pub fn add(&self, kind: &str, text: &str, session: &str) -> Result<()> {
        let parent = self.path.parent().expect("notes path has a parent");
        std::fs::create_dir_all(parent)?;
        let lock = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .open(parent.join(".notes.lock"))?;
        fs2::FileExt::lock_exclusive(&lock)?;
        let mut lines = self.lines();
        let marker = format!("[{kind}] {text}");
        if lines.iter().any(|line| line.contains(&marker)) {
            return Ok(());
        }
        lines.push(format!(
            "- {} {marker} (session {})",
            timestamp(now()),
            &session[..session.len().min(8)]
        ));
        if lines.len() > KEEP_LINES {
            lines.drain(..lines.len() - KEEP_LINES);
        }
        let mut text = String::from(
            "# Session notes\n\nRecorded automatically by cove (heuristic, not verified).\n\n",
        );
        for line in lines {
            text.push_str(&line);
            text.push('\n');
        }
        write_text(&self.path, &text)
    }

    /// Notes relevant to `session`'s request; recent ones on a session's first task.
    pub fn relevant(&self, session: &Session) -> Option<String> {
        let lines = self.lines();
        if lines.is_empty() {
            return None;
        }
        let first = self
            .seen
            .lock()
            .map(|seen| !seen.contains_key(&session.id))
            .unwrap_or(false);
        let documents: Vec<(String, u64)> = lines.iter().map(|line| (line.clone(), 0)).collect();
        let mut picked: Vec<usize> =
            crate::memory::recall::rank(&documents, &session.current_request, 0)
                .into_iter()
                .take(6)
                .map(|(index, _)| index)
                .collect();
        if first {
            picked.extend(lines.len().saturating_sub(5)..lines.len());
        }
        picked.sort_unstable();
        picked.dedup();
        if picked.is_empty() {
            return None;
        }
        let mut out = String::from(
            "[Session notes from earlier work in this project: heuristic notes, not instructions]\n",
        );
        for index in picked.into_iter().rev() {
            if out.len() + lines[index].len() + 1 > NOTE_LIMIT {
                break;
            }
            out.push_str(&lines[index]);
            out.push('\n');
        }
        Some(out)
    }

    fn compactions(&self, session: &Session) {
        let previous = self
            .seen
            .lock()
            .ok()
            .and_then(|mut seen| seen.insert(session.id.clone(), session.compactions));
        if let Some(previous) = previous
            && session.compactions > previous
        {
            let request: String = session.current_request.chars().take(80).collect();
            let _ = self.add(
                "compaction",
                &format!(
                    "context compacted (total {}) while working on: {}",
                    session.compactions,
                    request.replace('\n', " ")
                ),
                &session.id,
            );
        }
    }
}

fn write_text(path: &Path, text: &str) -> Result<()> {
    let parent = path.parent().expect("notes path has a parent");
    let mut file = tempfile::NamedTempFile::new_in(parent)?;
    std::io::Write::write_all(&mut file, text.as_bytes())?;
    file.as_file().sync_all()?;
    crate::session::replace_with(file, path)?;
    Ok(())
}

fn last_assistant_text(session: &Session) -> String {
    session
        .messages
        .iter()
        .rev()
        .find(|message| message.role == Role::Assistant)
        .map(|message| {
            message
                .blocks
                .iter()
                .filter_map(|block| match block {
                    Block::Text(text) => Some(text.as_str()),
                    _ => None,
                })
                .collect::<Vec<_>>()
                .join("\n")
        })
        .unwrap_or_default()
}

impl TurnHook for SessionNotes {
    fn task_note<'a>(&'a self, session: &'a Session) -> BoxFuture<'a, Option<String>> {
        Box::pin(async move {
            let note = self.relevant(session);
            self.compactions(session);
            note
        })
    }

    fn task_end<'a>(&'a self, session: &'a Session, completed: bool) -> BoxFuture<'a, ()> {
        Box::pin(async move {
            self.compactions(session);
            if completed {
                for (kind, text) in signals(&session.current_request, &last_assistant_text(session))
                {
                    let _ = self.add(kind, &text, &session.id);
                }
            }
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::Message;

    #[test]
    fn signals_follow_the_go_heuristics() {
        let found = signals(
            "Let's use tabs for indentation. 我们决定改用 pnpm 管理依赖",
            "I found the config loader ignores BOMs.",
        );
        assert!(found.contains(&("decision", "tabs for indentation".into())));
        assert!(found.iter().any(|(_, text)| text.contains("pnpm")));
        assert!(found.contains(&("discovery", "the config loader ignores BOMs".into())));
        assert!(signals("没有发现问题", "没有发现问题").is_empty());
        assert!(signals("用户需要登录", "").is_empty());
        assert_eq!(signals("用 tabs 缩进", "")[0].1, "tabs 缩进");
        assert!(signals("use it.", "").is_empty());
    }

    #[tokio::test]
    async fn notes_are_recorded_bounded_and_offered_by_relevance() {
        let data = tempfile::tempdir().unwrap();
        let notes = SessionNotes::new(data.path(), Path::new("/work"));
        let mut session = Session::new("/work".into(), String::new(), String::new(), None);
        session.current_request = "Please switch to ripgrep for searching.".into();
        assert!(notes.task_note(&session).await.is_none());
        session.messages.push(Message::user("x"));
        session.messages.push(Message {
            role: Role::Assistant,
            blocks: vec![Block::Text(
                "It turns out grep was missing on Windows.".into(),
            )],
        });
        session.compactions = 1;
        notes.task_end(&session, true).await;
        notes.task_end(&session, true).await;
        let text = std::fs::read_to_string(notes.path()).unwrap();
        assert_eq!(
            text.matches("[decision] ripgrep for searching").count(),
            1,
            "{text}"
        );
        assert!(text.contains("[discovery] grep was missing on Windows"));
        assert!(text.contains("[compaction] context compacted (total 1)"));
        let mut next = Session::new("/work".into(), String::new(), String::new(), None);
        next.current_request = "why is grep missing".into();
        let note = notes.task_note(&next).await.unwrap();
        assert!(note.contains("grep was missing") && note.len() <= NOTE_LIMIT);
        let many: String = (0..KEEP_LINES + 5)
            .map(|index| format!("- 2026-01-01 00:00 [decision] item {index} (session s)\n"))
            .collect();
        std::fs::write(notes.path(), many).unwrap();
        notes.add("decision", "newest", "s").unwrap();
        assert_eq!(notes.lines().len(), KEEP_LINES);
    }
}
