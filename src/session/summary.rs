//! The one way history lists see sessions: which sessions are listed ([`SessionScope`]),
//! and what is shown about each ([`SessionSummary`]). `/continue`, `/resume`,
//! `--list-sessions` and the console's history panel all go through here.

use std::path::{Path, PathBuf};

use super::{Session, State, Store, TurnRecord, time};

/// Which sessions a list shows.
#[derive(Clone, Debug)]
pub struct SessionScope {
    /// Only sessions of this workspace; `None` lists every workspace.
    pub workspace: Option<PathBuf>,
    /// Left out, usually the session in use.
    pub exclude: Option<String>,
    /// Also sessions in which nothing was ever asked.
    pub include_empty: bool,
}

impl SessionScope {
    pub fn workspace(cwd: &Path) -> Self {
        Self {
            workspace: Some(cwd.to_path_buf()),
            exclude: None,
            include_empty: false,
        }
    }

    pub fn everywhere() -> Self {
        Self {
            workspace: None,
            exclude: None,
            include_empty: false,
        }
    }

    pub fn excluding(mut self, id: &str) -> Self {
        self.exclude = Some(id.to_owned());
        self
    }

    pub fn with_empty(mut self) -> Self {
        self.include_empty = true;
        self
    }

    fn admits(&self, session: &Session) -> bool {
        self.workspace
            .as_ref()
            .is_none_or(|cwd| session.cwd == *cwd)
            && self.exclude.as_deref() != Some(session.id.as_str())
            && (self.include_empty || session.has_conversation())
    }
}

/// What a history list shows about one session.
#[derive(Clone, Debug)]
pub struct SessionSummary {
    pub id: String,
    pub title: String,
    pub cwd: PathBuf,
    pub updated: u64,
    /// A task stopped before finishing (pending calls, interrupted or still running).
    pub unfinished: bool,
    pub interrupted: bool,
    /// Every turn, also those a compaction removed (from the archives).
    pub turns: Vec<TurnRecord>,
    pub model: String,
    pub cost: Option<f64>,
    pub lines_added: u64,
    pub lines_removed: u64,
    pub compacted: bool,
}

impl SessionSummary {
    /// The summary of the live session alone (no archives are read).
    pub fn live(session: &Session) -> Self {
        Self {
            id: session.id.clone(),
            title: session.display_title(),
            cwd: session.cwd.clone(),
            updated: session.updated,
            unfinished: session.unfinished(),
            interrupted: matches!(session.state, State::Interrupted(_)),
            turns: session.turns().iter().map(|turn| turn.record()).collect(),
            model: session.model.clone(),
            cost: session.accounting.cost,
            lines_added: session.accounting.lines_added,
            lines_removed: session.accounting.lines_removed,
            compacted: session.compactions > 0,
        }
    }

    /// The workspace's directory name, for lists that span workspaces.
    pub fn workspace_name(&self) -> String {
        let shown = crate::paths::display(&self.cwd);
        shown
            .trim_end_matches(['\\', '/'])
            .rsplit(['\\', '/'])
            .next()
            .unwrap_or(&shown)
            .to_owned()
    }
}

impl Store {
    /// The sessions `scope` admits, newest first.
    pub fn sessions(&self, scope: &SessionScope) -> Vec<Session> {
        self.list_lenient()
            .0
            .into_iter()
            .filter(|session| scope.admits(session))
            .collect()
    }

    /// The summary with every turn, also those compaction moved to the archives.
    pub fn summary(&self, session: &Session) -> SessionSummary {
        SessionSummary {
            turns: self.history(session),
            ..SessionSummary::live(session)
        }
    }

    /// Summaries of the sessions `scope` admits, newest first.
    pub fn summaries(&self, scope: &SessionScope) -> Vec<SessionSummary> {
        self.sessions(scope)
            .iter()
            .map(|session| self.summary(session))
            .collect()
    }
}

/// One text line per session, numbered from 1 (plain output: pipes, `-p`,
/// `--list-sessions`).
pub fn history_lines(sessions: &[SessionSummary], current: Option<&str>, show_cwd: bool) -> String {
    let mut out = String::new();
    for (index, session) in sessions.iter().enumerate() {
        let state = if session.interrupted {
            "已中断"
        } else if session.unfinished {
            "未完成"
        } else {
            "已完成"
        };
        out.push_str(&format!(
            "{:>3}. {}  {}{} {:>3} 轮  {}  [{}]{}{}\n",
            index + 1,
            time::stamp(session.updated),
            state,
            // CJK characters take two columns; pad to the widest state word.
            " ".repeat(6 - 2 * state.chars().count()),
            session.turns.len(),
            session.title,
            &session.id[..session.id.len().min(8)],
            if Some(session.id.as_str()) == current {
                " (current)"
            } else {
                ""
            },
            if show_cwd {
                format!("  {}", crate::paths::display(&session.cwd))
            } else {
                String::new()
            }
        ));
    }
    if out.is_empty() {
        out.push_str("没有保存的会话。\n");
    }
    out
}
