//! Typed agent lifecycle shared by background sub-agents and the parked main task.
//!
//! ```text
//!            resume                 finish / fail
//!   paused ─────────┐   ┌──────────────────────────► finished / failed
//!     ▲             ▼   │                                  │ resume (follow-up)
//!     │ park     running ◄─────────────────────────────────┘
//!     └──────────── │  ▲
//!                   │  │ resume
//!       interrupt   ▼  │
//!               interrupted ── park ──► paused
//!   any state but running ── discard / merge ──► discarded
//! ```
//! Every change goes through [`Lifecycle::apply`], so an invalid transition is an error
//! instead of a silently overwritten field. Records persist as atomic JSON snapshots.

use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use crate::{
    domain::{Error, Result},
    session::{Accounting, atomic_json, now, read_json, valid_id},
};

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Lifecycle {
    Running,
    /// Parked on purpose: full state kept, slot freed, survives restarts.
    Paused,
    /// Stopped by the user, a crash or a limit; resumable.
    Interrupted,
    Finished,
    Failed,
    Discarded,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Transition {
    /// Start or wake (optionally with a follow-up message).
    Resume,
    Interrupt,
    Park,
    Finish,
    Fail,
    /// Remove the work (discard, or after a successful merge).
    Discard,
}

impl Lifecycle {
    pub fn apply(self, transition: Transition) -> Result<Self> {
        use Lifecycle::*;
        use Transition as T;
        let next = match (self, transition) {
            (Paused | Interrupted | Finished | Failed, T::Resume) => Running,
            (Running, T::Interrupt) => Interrupted,
            (Running | Interrupted | Finished | Failed | Paused, T::Park) => Paused,
            (Running, T::Finish) => Finished,
            (Running, T::Fail) => Failed,
            (Paused | Interrupted | Finished | Failed, T::Discard) => Discarded,
            (state, transition) => {
                return Err(Error::Session(format!(
                    "cannot {} an agent that is {}",
                    format!("{transition:?}").to_lowercase(),
                    state.label()
                )));
            }
        };
        Ok(next)
    }

    pub fn label(self) -> &'static str {
        match self {
            Self::Running => "running",
            Self::Paused => "paused",
            Self::Interrupted => "interrupted",
            Self::Finished => "finished",
            Self::Failed => "failed",
            Self::Discarded => "discarded",
        }
    }

    pub fn resumable(self) -> bool {
        matches!(
            self,
            Self::Paused | Self::Interrupted | Self::Finished | Self::Failed
        )
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Kind {
    /// A durable sub-agent with its own session (and usually its own worktree).
    Background,
    /// A parked foreground session; `session_id` addresses it.
    Main,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct AgentRecord {
    pub version: u32,
    pub id: String,
    pub kind: Kind,
    pub agent_type: String,
    pub description: String,
    pub task: String,
    pub state: Lifecycle,
    /// Why the last transition happened (exit reason, user action).
    pub reason: String,
    pub session_id: String,
    pub project: PathBuf,
    pub worktree: Option<PathBuf>,
    pub branch: Option<String>,
    /// Commit the worktree branch started from.
    pub base: Option<String>,
    pub created: u64,
    pub updated: u64,
    pub report: String,
    pub last_step: String,
    /// Spend already forwarded to the parent session's ledger.
    pub reported: Accounting,
    #[serde(default)]
    pub merged: bool,
}

impl AgentRecord {
    pub fn short(&self) -> &str {
        &self.id[..8.min(self.id.len())]
    }

    pub fn transition(&mut self, transition: Transition, reason: impl Into<String>) -> Result<()> {
        self.state = self.state.apply(transition)?;
        self.reason = reason.into();
        self.updated = now();
        Ok(())
    }
}

/// Durable agent records of one project: `<data>/agents/<project-hash>/<id>.json`.
#[derive(Clone)]
pub struct AgentStore {
    dir: PathBuf,
}

pub fn project_key(project: &Path) -> String {
    use sha2::{Digest, Sha256};
    let digest = Sha256::digest(project.as_os_str().as_encoded_bytes());
    digest[..8]
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect()
}

impl AgentStore {
    pub fn open(data_root: &Path, project: &Path) -> Result<Self> {
        let dir = data_root.join("agents").join(project_key(project));
        std::fs::create_dir_all(&dir)?;
        Ok(Self { dir })
    }

    fn path(&self, id: &str) -> Result<PathBuf> {
        if !valid_id(id) {
            return Err(Error::Session("invalid agent ID".into()));
        }
        Ok(self.dir.join(format!("{id}.json")))
    }

    pub fn save(&self, record: &AgentRecord) -> Result<()> {
        atomic_json(&self.path(&record.id)?, record)
    }

    pub fn load(&self, id: &str) -> Result<AgentRecord> {
        let record: AgentRecord = read_json(&self.path(id)?)?;
        if record.version != 1 || record.id != id {
            return Err(Error::Session("agent record identity mismatch".into()));
        }
        Ok(record)
    }

    /// All records, oldest first. A record left `running` by a stopped process is
    /// reported (and persisted) as interrupted: its run cannot still be in flight.
    pub fn list(&self, live: &dyn Fn(&str) -> bool) -> Result<Vec<AgentRecord>> {
        let mut records = Vec::new();
        for entry in std::fs::read_dir(&self.dir)? {
            let path = entry?.path();
            let Some(id) = path
                .file_stem()
                .and_then(|stem| stem.to_str())
                .filter(|_| path.extension().is_some_and(|ext| ext == "json"))
            else {
                continue;
            };
            let Ok(mut record) = self.load(id) else {
                continue;
            };
            if record.state == Lifecycle::Running && !live(&record.id) {
                record.transition(
                    Transition::Interrupt,
                    "previous process stopped while it ran; resume to continue from its transcript",
                )?;
                self.save(&record)?;
            }
            records.push(record);
        }
        records.sort_by(|a, b| a.created.cmp(&b.created).then(a.id.cmp(&b.id)));
        Ok(records)
    }

    /// Resolves a full ID or a unique prefix (at least 4 characters).
    pub fn find(&self, prefix: &str, live: &dyn Fn(&str) -> bool) -> Result<AgentRecord> {
        let prefix = prefix.trim();
        if prefix.len() < 4 {
            return Err(Error::Session(
                "agent ID prefix needs at least 4 characters".into(),
            ));
        }
        let mut matches: Vec<_> = self
            .list(live)?
            .into_iter()
            .filter(|record| record.id.starts_with(prefix) || record.session_id == prefix)
            .collect();
        match matches.len() {
            1 => Ok(matches.remove(0)),
            0 => Err(Error::Session(format!("no agent {prefix}"))),
            _ => Err(Error::Session(format!("agent ID {prefix} is ambiguous"))),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn transitions_follow_the_state_machine() {
        use Lifecycle::*;
        use Transition as T;
        assert_eq!(Running.apply(T::Interrupt).unwrap(), Interrupted);
        assert_eq!(Interrupted.apply(T::Resume).unwrap(), Running);
        assert_eq!(Running.apply(T::Park).unwrap(), Paused);
        assert_eq!(Paused.apply(T::Resume).unwrap(), Running);
        assert_eq!(Finished.apply(T::Resume).unwrap(), Running);
        assert_eq!(Failed.apply(T::Discard).unwrap(), Discarded);
        assert!(Running.apply(T::Discard).is_err());
        assert!(Running.apply(T::Resume).is_err());
        assert!(Discarded.apply(T::Resume).is_err());
        assert!(Paused.apply(T::Finish).is_err());
        assert!(Discarded.apply(T::Park).is_err());
    }

    fn record(id: &str) -> AgentRecord {
        AgentRecord {
            version: 1,
            id: id.into(),
            kind: Kind::Background,
            agent_type: "general".into(),
            description: "d".into(),
            task: "t".into(),
            state: Lifecycle::Running,
            reason: String::new(),
            session_id: id.into(),
            project: PathBuf::from("."),
            worktree: None,
            branch: None,
            base: None,
            created: 1,
            updated: 1,
            report: String::new(),
            last_step: String::new(),
            reported: Accounting::default(),
            merged: false,
        }
    }

    #[test]
    fn a_reload_marks_orphaned_running_records_interrupted() {
        let data = tempfile::tempdir().unwrap();
        let store = AgentStore::open(data.path(), Path::new("project")).unwrap();
        let id = crate::session::new_id();
        store.save(&record(&id)).unwrap();
        // Still running in this process: untouched.
        assert_eq!(store.list(&|_| true).unwrap()[0].state, Lifecycle::Running);
        // A new process sees no live run: interrupted, and persisted as such.
        assert_eq!(
            store.list(&|_| false).unwrap()[0].state,
            Lifecycle::Interrupted
        );
        assert_eq!(store.load(&id).unwrap().state, Lifecycle::Interrupted);
        assert_eq!(store.find(&id[..6], &|_| false).unwrap().id, id);
        assert!(store.find("zz", &|_| false).is_err());
    }
}
