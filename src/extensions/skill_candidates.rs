use std::{
    collections::HashSet,
    fs::{File, OpenOptions},
    path::{Path, PathBuf},
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

use fs2::FileExt;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::{
    domain::{Error, Result},
    extensions::identifier,
    session::{atomic_json, read_json},
};

const VERSION: u32 = 1;
const MAX_CANDIDATES: usize = 256;
const MAX_BODY_BYTES: usize = 16 * 1024;
const MAX_SESSIONS: usize = 64;
const MAX_SOURCES: usize = 64;

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum CandidateState {
    Candidate,
    Approved,
    Rejected,
    Revoked,
}

#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ValidationState {
    Passed,
    Failed,
    NotRun,
    #[default]
    Unknown,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Candidate {
    pub id: String,
    pub name: String,
    pub description: String,
    pub body: String,
    pub sessions: Vec<String>,
    #[serde(default)]
    pub sources: Vec<TaskSource>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub legacy_sources: Vec<LegacySource>,
    pub created_at: u64,
    pub state: CandidateState,
    pub validation: ValidationState,
    #[serde(default)]
    pub approved_hash: Option<String>,
    #[serde(default)]
    pub history: Vec<Decision>,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct LegacySource {
    pub name: String,
    pub content_hash: String,
    pub session: String,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct TaskSource {
    pub session: String,
    pub task_id: Option<String>,
    pub message_start: Option<usize>,
    pub message_end: usize,
    pub completed: Option<bool>,
    #[serde(default)]
    pub verification: ValidationState,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Decision {
    pub state: CandidateState,
    pub actor: String,
    pub at: u64,
    pub reason: String,
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct Records {
    version: u32,
    candidates: Vec<Candidate>,
}

pub struct CandidateStore {
    dir: PathBuf,
}

fn content_id(name: &str, description: &str, body: &str) -> String {
    let mut digest = Sha256::new();
    for text in [name, description, body] {
        digest.update((text.len() as u64).to_be_bytes());
        digest.update(text.as_bytes());
    }
    format!("{:x}", digest.finalize())
}

fn now() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |time| time.as_secs())
}

fn reject_symlink(path: &Path) -> Result<()> {
    match std::fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() => Err(Error::Tool(format!(
            "candidate storage path is a symbolic link: {}",
            path.display()
        ))),
        Ok(_) => Ok(()),
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(err) => Err(err.into()),
    }
}

impl CandidateStore {
    pub fn open(data: &Path) -> Result<Self> {
        let data = data.canonicalize()?;
        let dir = data.join("skill-candidates");
        reject_symlink(&dir)?;
        std::fs::create_dir_all(&dir)?;
        if dir.canonicalize()? != dir {
            return Err(Error::Tool(
                "candidate directory escaped data directory".into(),
            ));
        }
        Ok(Self { dir })
    }

    fn lock(&self) -> Result<File> {
        self.lock_with_wait(Duration::ZERO)
    }

    fn lock_with_wait(&self, wait: Duration) -> Result<File> {
        let path = self.dir.join("records.lock");
        reject_symlink(&path)?;
        let file = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .open(path)?;
        let started = Instant::now();
        loop {
            match file.try_lock_exclusive() {
                Ok(()) => return Ok(file),
                Err(err)
                    if err.kind() == fs2::lock_contended_error().kind()
                        && started.elapsed() < wait =>
                {
                    std::thread::sleep(
                        Duration::from_millis(10).min(wait.saturating_sub(started.elapsed())),
                    );
                }
                Err(err) => return Err(Error::Tool(format!("candidate store is busy: {err}"))),
            }
        }
    }

    fn read(&self) -> Result<Records> {
        let path = self.dir.join("records.json");
        reject_symlink(&path)?;
        if !path.try_exists()? {
            return Ok(Records {
                version: VERSION,
                candidates: Vec::new(),
            });
        }
        let records: Records = read_json(&path)?;
        if records.version != VERSION || records.candidates.len() > MAX_CANDIDATES {
            return Err(Error::Tool(
                "unsupported or oversized candidate store".into(),
            ));
        }
        let mut ids = HashSet::with_capacity(records.candidates.len());
        let mut approved_names = HashSet::new();
        for candidate in &records.candidates {
            if candidate.id != content_id(&candidate.name, &candidate.description, &candidate.body)
                || !ids.insert(candidate.id.as_str())
                || (candidate.state == CandidateState::Approved
                    && !approved_names.insert(candidate.name.as_str()))
                || !identifier(&candidate.name)
                || candidate.description.len() > 1024
                || candidate.body.len() > MAX_BODY_BYTES
                || candidate.sessions.len() > MAX_SESSIONS
                || candidate.sources.len() > MAX_SOURCES
                || candidate.legacy_sources.len() > MAX_SOURCES
                || candidate.legacy_sources.iter().any(|source| {
                    !identifier(&source.name)
                        || source.content_hash.len() != 64
                        || !source
                            .content_hash
                            .bytes()
                            .all(|byte| byte.is_ascii_hexdigit())
                        || !candidate.sessions.contains(&source.session)
                })
                || candidate.sources.iter().any(|source| {
                    source.session.is_empty()
                        || source.session.len() > 128
                        || source
                            .task_id
                            .as_ref()
                            .is_some_and(|id| id.is_empty() || id.len() > 128)
                        || source
                            .message_start
                            .is_some_and(|start| start > source.message_end)
                        || !candidate.sessions.contains(&source.session)
                })
                || candidate
                    .sessions
                    .iter()
                    .any(|session| session.is_empty() || session.len() > 128)
                || candidate.history.len() > 2
                || candidate.history.iter().any(|decision| {
                    decision.actor.is_empty()
                        || decision.actor.len() > 128
                        || decision.reason.len() > 1024
                })
                || match candidate.state {
                    CandidateState::Candidate => {
                        !candidate.history.is_empty() || candidate.approved_hash.is_some()
                    }
                    CandidateState::Approved => {
                        candidate.history.len() != 1
                            || candidate.history[0].state != CandidateState::Approved
                            || candidate.approved_hash.as_deref() != Some(&candidate.id)
                    }
                    CandidateState::Rejected => {
                        candidate.history.len() != 1
                            || candidate.history[0].state != CandidateState::Rejected
                            || candidate.approved_hash.is_some()
                    }
                    CandidateState::Revoked => {
                        candidate.history.len() != 2
                            || candidate.history[0].state != CandidateState::Approved
                            || candidate.history[1].state != CandidateState::Revoked
                            || candidate.approved_hash.as_deref() != Some(&candidate.id)
                    }
                }
            {
                return Err(Error::Tool("candidate data changed or is invalid".into()));
            }
        }
        Ok(records)
    }

    pub fn list(&self) -> Result<Vec<Candidate>> {
        let _lock = self.lock()?;
        Ok(self.read()?.candidates)
    }

    pub fn submit(
        &self,
        name: &str,
        description: &str,
        body: &str,
        session: &str,
    ) -> Result<Candidate> {
        self.submit_with_sources(name, description, body, session, &[])
    }

    pub fn submit_with_sources(
        &self,
        name: &str,
        description: &str,
        body: &str,
        session: &str,
        sources: &[TaskSource],
    ) -> Result<Candidate> {
        self.submit_with_wait(
            name,
            description,
            body,
            session,
            (sources, None),
            Duration::ZERO,
        )
    }

    pub(crate) fn submit_legacy(
        &self,
        name: &str,
        description: &str,
        body: &str,
        session: &str,
        source: LegacySource,
    ) -> Result<Candidate> {
        self.submit_with_wait(
            name,
            description,
            body,
            session,
            (&[], Some(&source)),
            Duration::ZERO,
        )
    }

    pub(crate) fn submit_review_with_sources(
        &self,
        name: &str,
        description: &str,
        body: &str,
        session: &str,
        sources: &[TaskSource],
    ) -> Result<Candidate> {
        self.submit_with_wait(
            name,
            description,
            body,
            session,
            (sources, None),
            Duration::from_millis(250),
        )
    }

    fn submit_with_wait(
        &self,
        name: &str,
        description: &str,
        body: &str,
        session: &str,
        provenance: (&[TaskSource], Option<&LegacySource>),
        wait: Duration,
    ) -> Result<Candidate> {
        let (sources, legacy) = provenance;
        if !identifier(name)
            || description.len() > 1024
            || body.is_empty()
            || body.len() > MAX_BODY_BYTES
            || session.is_empty()
            || session.len() > 128
            || sources.len() > MAX_SOURCES
            || legacy.is_some_and(|source| {
                !identifier(&source.name)
                    || source.session != session
                    || source.content_hash.len() != 64
                    || !source
                        .content_hash
                        .bytes()
                        .all(|byte| byte.is_ascii_hexdigit())
            })
            || sources.iter().any(|source| {
                source.session != session
                    || source
                        .message_start
                        .is_some_and(|start| start > source.message_end)
                    || source
                        .task_id
                        .as_ref()
                        .is_some_and(|id| id.is_empty() || id.len() > 128)
            })
        {
            return Err(Error::Tool("invalid candidate data".into()));
        }
        let id = content_id(name, description, body);
        let _lock = self.lock_with_wait(wait)?;
        let mut records = self.read()?;
        if let Some(candidate) = records
            .candidates
            .iter_mut()
            .find(|candidate| candidate.id == id)
        {
            let mut changed = false;
            if !candidate.sessions.iter().any(|source| source == session) {
                if candidate.sessions.len() >= MAX_SESSIONS {
                    return Err(Error::Tool("candidate has too many sources".into()));
                }
                candidate.sessions.push(session.to_owned());
                changed = true;
            }
            for source in sources {
                if !candidate.sources.contains(source) {
                    if candidate.sources.len() >= MAX_SOURCES {
                        return Err(Error::Tool("candidate has too many task sources".into()));
                    }
                    candidate.sources.push(source.clone());
                    changed = true;
                }
            }
            if let Some(source) = legacy
                && !candidate.legacy_sources.contains(source)
            {
                if candidate.legacy_sources.len() >= MAX_SOURCES {
                    return Err(Error::Tool("candidate has too many legacy sources".into()));
                }
                candidate.legacy_sources.push(source.clone());
                changed = true;
            }
            if changed {
                let result = candidate.clone();
                atomic_json(&self.dir.join("records.json"), &records)?;
                return Ok(result);
            }
            return Ok(candidate.clone());
        }
        if records.candidates.len() >= MAX_CANDIDATES {
            return Err(Error::Tool("candidate store is full".into()));
        }
        let candidate = Candidate {
            id,
            name: name.to_owned(),
            description: description.to_owned(),
            body: body.to_owned(),
            sessions: vec![session.to_owned()],
            sources: sources.to_vec(),
            legacy_sources: legacy.into_iter().cloned().collect(),
            created_at: now(),
            state: CandidateState::Candidate,
            validation: ValidationState::Unknown,
            approved_hash: None,
            history: Vec::new(),
        };
        records.candidates.push(candidate.clone());
        atomic_json(&self.dir.join("records.json"), &records)?;
        Ok(candidate)
    }

    pub fn decide(
        &self,
        id: &str,
        expected: CandidateState,
        next: CandidateState,
        actor: &str,
        reason: &str,
    ) -> Result<Candidate> {
        if !matches!(
            (expected, next),
            (
                CandidateState::Candidate,
                CandidateState::Approved | CandidateState::Rejected
            ) | (CandidateState::Approved, CandidateState::Revoked)
        ) || actor.trim().is_empty()
            || actor.len() > 128
            || reason.len() > 1024
            || (next == CandidateState::Revoked && reason.trim().is_empty())
        {
            return Err(Error::Tool("invalid candidate decision".into()));
        }
        let _lock = self.lock()?;
        let mut records = self.read()?;
        if next == CandidateState::Approved {
            let name = records
                .candidates
                .iter()
                .find(|candidate| candidate.id == id)
                .ok_or_else(|| Error::Tool("candidate not found".into()))?
                .name
                .clone();
            if records.candidates.iter().any(|candidate| {
                candidate.id != id
                    && candidate.name == name
                    && candidate.state == CandidateState::Approved
            }) {
                return Err(Error::Tool(
                    "another version of this skill is already approved".into(),
                ));
            }
        }
        let candidate = records
            .candidates
            .iter_mut()
            .find(|candidate| candidate.id == id)
            .ok_or_else(|| Error::Tool("candidate not found".into()))?;
        if candidate.state != expected {
            return Err(Error::Tool(
                "candidate state changed; review it again".into(),
            ));
        }
        candidate.state = next;
        if next == CandidateState::Approved {
            candidate.approved_hash = Some(id.to_owned());
        }
        candidate.history.push(Decision {
            state: next,
            actor: actor.to_owned(),
            at: now(),
            reason: reason.to_owned(),
        });
        let result = candidate.clone();
        atomic_json(&self.dir.join("records.json"), &records)?;
        Ok(result)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn skill_candidates_survive_restart_and_duplicate_submission() {
        let data = tempfile::tempdir().unwrap();
        let store = CandidateStore::open(data.path()).unwrap();
        let first = store
            .submit("deploy", "When deploying", "build, ship", "session-1")
            .unwrap();
        let repeated = store
            .submit("deploy", "When deploying", "build, ship", "session-1")
            .unwrap();
        assert_eq!(first.id, repeated.id);
        let reopened = CandidateStore::open(data.path()).unwrap();
        let records = reopened.list().unwrap();
        assert_eq!(records.len(), 1);
        assert_eq!(records[0].id, first.id);
        assert_eq!(records[0].state, CandidateState::Candidate);
        assert_eq!(records[0].validation, ValidationState::Unknown);
        assert_eq!(records[0].body, "build, ship");
        assert_eq!(records[0].sessions, vec!["session-1"]);
    }

    #[test]
    fn skill_candidates_keep_versions_and_reject_tampering() {
        let data = tempfile::tempdir().unwrap();
        let store = CandidateStore::open(data.path()).unwrap();
        let first = store
            .submit("deploy", "Deploy", "build", "session-1")
            .unwrap();
        let second = store
            .submit("deploy", "Deploy", "test", "session-2")
            .unwrap();
        assert_ne!(first.id, second.id);
        assert_eq!(store.list().unwrap().len(), 2);
        let path = data.path().join("skill-candidates/records.json");
        let mut records: serde_json::Value = read_json(&path).unwrap();
        records["candidates"][0]["body"] = "changed".into();
        atomic_json(&path, &records).unwrap();
        assert!(store.list().is_err());
        assert!(store.submit("new", "New", "body", "session-3").is_err());
        let unchanged: serde_json::Value = read_json(&path).unwrap();
        assert_eq!(unchanged, records);
    }

    #[test]
    fn skill_candidates_reject_duplicate_ids_and_duplicate_approved_names() {
        let data = tempfile::tempdir().unwrap();
        let store = CandidateStore::open(data.path()).unwrap();
        let first = store
            .submit("deploy", "Deploy", "build", "session-1")
            .unwrap();
        let second = store
            .submit("deploy", "Deploy", "test", "session-2")
            .unwrap();
        store
            .decide(
                &first.id,
                CandidateState::Candidate,
                CandidateState::Approved,
                "user",
                "",
            )
            .unwrap();
        let path = data.path().join("skill-candidates/records.json");
        let mut records: Records = read_json(&path).unwrap();
        records.candidates.push(records.candidates[0].clone());
        atomic_json(&path, &records).unwrap();
        assert!(store.list().is_err());
        assert!(
            store
                .decide(
                    &first.id,
                    CandidateState::Approved,
                    CandidateState::Revoked,
                    "user",
                    "unsafe"
                )
                .is_err()
        );
        records.candidates.pop();
        records.candidates[1].state = CandidateState::Approved;
        records.candidates[1].approved_hash = Some(second.id);
        records.candidates[1].history = records.candidates[0].history.clone();
        atomic_json(&path, &records).unwrap();
        assert!(store.list().is_err());
    }

    #[test]
    fn skill_candidates_concurrent_same_name_approvals_do_not_both_succeed() {
        let data = tempfile::tempdir().unwrap();
        let store = CandidateStore::open(data.path()).unwrap();
        let first = store
            .submit("deploy", "Deploy", "build", "session-1")
            .unwrap();
        let second = store
            .submit("deploy", "Deploy", "test", "session-2")
            .unwrap();
        let barrier = std::sync::Arc::new(std::sync::Barrier::new(2));
        let handles: Vec<_> = [first.id, second.id]
            .into_iter()
            .map(|id| {
                let path = data.path().to_path_buf();
                let barrier = barrier.clone();
                std::thread::spawn(move || {
                    let store = CandidateStore::open(&path).unwrap();
                    barrier.wait();
                    store.decide(
                        &id,
                        CandidateState::Candidate,
                        CandidateState::Approved,
                        "user",
                        "",
                    )
                })
            })
            .collect();
        let successes = handles
            .into_iter()
            .map(|handle| handle.join().unwrap())
            .filter(Result::is_ok)
            .count();
        assert_eq!(successes, 1);
        let records = store.list().unwrap();
        assert_eq!(
            records
                .iter()
                .filter(|record| record.state == CandidateState::Approved)
                .count(),
            1
        );
        assert_eq!(
            records
                .iter()
                .filter(|record| record.state == CandidateState::Candidate)
                .count(),
            1
        );
    }

    #[test]
    fn skill_candidates_process_lock_worker() {
        let Some(path) = std::env::var_os("COVE_CANDIDATE_LOCK_TEST_DIR") else {
            return;
        };
        let store = CandidateStore::open(Path::new(&path)).unwrap();
        let result = store.submit("process-flow", "Deploy", "build", "session-1");
        assert_eq!(
            result.is_ok(),
            std::env::var("COVE_CANDIDATE_LOCK_EXPECT_SUCCESS").unwrap() == "1"
        );
    }

    #[test]
    fn skill_candidates_exclusive_lock_works_across_processes() {
        let data = tempfile::tempdir().unwrap();
        let store = CandidateStore::open(data.path()).unwrap();
        let lock = store.lock().unwrap();
        let run = |success: bool| {
            std::process::Command::new(std::env::current_exe().unwrap())
                .args([
                    "--exact",
                    "extensions::skill_candidates::tests::skill_candidates_process_lock_worker",
                ])
                .env("COVE_CANDIDATE_LOCK_TEST_DIR", data.path())
                .env(
                    "COVE_CANDIDATE_LOCK_EXPECT_SUCCESS",
                    if success { "1" } else { "0" },
                )
                .output()
                .unwrap()
        };
        let blocked = run(false);
        assert!(
            blocked.status.success(),
            "{}",
            String::from_utf8_lossy(&blocked.stdout)
        );
        drop(lock);
        assert!(store.list().unwrap().is_empty());
        let permitted = run(true);
        assert!(
            permitted.status.success(),
            "{}",
            String::from_utf8_lossy(&permitted.stdout)
        );
        assert_eq!(store.list().unwrap()[0].name, "process-flow");
    }

    #[cfg(windows)]
    #[test]
    fn skill_candidates_failed_replace_does_not_partially_approve() {
        use std::os::windows::fs::OpenOptionsExt;
        let data = tempfile::tempdir().unwrap();
        let store = CandidateStore::open(data.path()).unwrap();
        let candidate = store
            .submit("deploy", "Deploy", "build", "session-1")
            .unwrap();
        let path = data.path().join("skill-candidates/records.json");
        let before = std::fs::read(&path).unwrap();
        let locked = OpenOptions::new()
            .read(true)
            .share_mode(1)
            .open(&path)
            .unwrap();
        assert!(
            store
                .decide(
                    &candidate.id,
                    CandidateState::Candidate,
                    CandidateState::Approved,
                    "user",
                    ""
                )
                .is_err()
        );
        drop(locked);
        assert_eq!(std::fs::read(&path).unwrap(), before);
        assert_eq!(store.list().unwrap()[0].state, CandidateState::Candidate);
    }

    #[test]
    fn skill_candidates_refuse_conflicting_writers_and_invalid_input() {
        let data = tempfile::tempdir().unwrap();
        let store = CandidateStore::open(data.path()).unwrap();
        let other = CandidateStore::open(data.path()).unwrap();
        let lock = store.lock().unwrap();
        assert!(
            other
                .submit("deploy", "Deploy", "build", "session-1")
                .is_err()
        );
        drop(lock);
        assert!(
            other
                .submit("../deploy", "Deploy", "build", "session-1")
                .is_err()
        );
        assert!(
            other
                .submit(
                    "deploy",
                    "Deploy",
                    &"x".repeat(MAX_BODY_BYTES + 1),
                    "session-1"
                )
                .is_err()
        );
        assert!(other.list().unwrap().is_empty());
        other
            .submit("deploy", "Deploy", "build", "session-1")
            .unwrap();
        other
            .submit("deploy", "Deploy", "build", "session-2")
            .unwrap();
        assert_eq!(
            other.list().unwrap()[0].sessions,
            ["session-1", "session-2"]
        );
    }

    #[test]
    fn skill_review_candidate_wait_is_bounded_when_store_remains_locked() {
        let data = tempfile::tempdir().unwrap();
        let store = CandidateStore::open(data.path()).unwrap();
        let lock = store.lock().unwrap();
        let path = data.path().to_path_buf();
        let (sender, receiver) = std::sync::mpsc::channel();
        let worker = std::thread::spawn(move || {
            let store = CandidateStore::open(&path).unwrap();
            let result =
                store.submit_review_with_sources("deploy", "Deploy", "build", "session-1", &[]);
            sender.send(result.is_err()).unwrap();
        });
        let result = receiver.recv_timeout(Duration::from_secs(3));
        drop(lock);
        worker.join().unwrap();
        assert!(result.unwrap());
        assert!(store.list().unwrap().is_empty());
    }

    #[test]
    fn skill_candidates_decisions_bind_hash_and_reject_stale_transitions() {
        let data = tempfile::tempdir().unwrap();
        let store = CandidateStore::open(data.path()).unwrap();
        let candidate = store
            .submit("deploy", "Deploy", "build", "session-1")
            .unwrap();
        assert!(
            store
                .decide(
                    &candidate.id,
                    CandidateState::Approved,
                    CandidateState::Revoked,
                    "user",
                    ""
                )
                .is_err()
        );
        let approved = store
            .decide(
                &candidate.id,
                CandidateState::Candidate,
                CandidateState::Approved,
                "user",
                "",
            )
            .unwrap();
        assert_eq!(
            approved.approved_hash.as_deref(),
            Some(candidate.id.as_str())
        );
        assert!(
            store
                .decide(
                    &candidate.id,
                    CandidateState::Candidate,
                    CandidateState::Rejected,
                    "user",
                    ""
                )
                .is_err()
        );
        let revoked = store
            .decide(
                &candidate.id,
                CandidateState::Approved,
                CandidateState::Revoked,
                "user",
                "outdated",
            )
            .unwrap();
        assert_eq!(revoked.state, CandidateState::Revoked);
        assert_eq!(revoked.history.len(), 2);
        assert_eq!(store.list().unwrap()[0].history.len(), 2);
    }
}
