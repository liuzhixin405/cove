use std::{
    collections::{BTreeMap, HashMap, HashSet},
    fs::{File, OpenOptions},
    path::{Path, PathBuf},
};

use fs2::FileExt;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::{
    domain::{Block, Error, Result},
    session::{Session, atomic_json, read_json},
};

const MAX_MESSAGES: usize = 256;
const MAX_SUGGESTIONS: usize = 16;
const MAX_INPUT_BYTES: usize = 16 * 1024;
const MAX_RECORDS: usize = 128;
const RECOMMENDATION: &str = "检查失败原因和输入，再调整操作；不要直接重复相同调用。";

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum SuggestionState {
    Candidate,
    Accepted,
    Rejected,
    Revoked,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Decision {
    pub state: SuggestionState,
    pub at: u64,
    pub reason: String,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Evidence {
    pub call_message: usize,
    pub result_message: usize,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Suggestion {
    pub id: String,
    pub kind: String,
    pub session: String,
    pub tool: String,
    pub input_hash: String,
    pub evidence: Vec<Evidence>,
    pub recommendation: String,
    pub state: SuggestionState,
    pub history: Vec<Decision>,
}

fn suggestion_id(suggestion: &Suggestion) -> String {
    let mut digest = Sha256::new();
    for value in [
        &suggestion.session,
        &suggestion.kind,
        &suggestion.tool,
        &suggestion.input_hash,
    ] {
        digest.update((value.len() as u64).to_be_bytes());
        digest.update(value.as_bytes());
    }
    for evidence in &suggestion.evidence {
        digest.update((evidence.call_message as u64).to_be_bytes());
        digest.update((evidence.result_message as u64).to_be_bytes());
    }
    format!("{:x}", digest.finalize())
}

pub fn analyze(session: &Session) -> Vec<Suggestion> {
    let start = session.messages.len().saturating_sub(MAX_MESSAGES);
    let mut calls = HashMap::new();
    let mut call_counts = HashMap::new();
    for (index, message) in session.messages.iter().enumerate().skip(start) {
        for block in message.blocks.iter().take(32) {
            if let Block::ToolUse { id, .. } = block {
                *call_counts.entry(id.as_str()).or_insert(0_usize) += 1;
            }
            if let Block::ToolUse {
                id, name, input, ..
            } = block
                && matches!(
                    name.as_str(),
                    "read" | "write" | "edit" | "grep" | "glob" | "shell"
                )
                && id.len() <= 128
                && let Ok(encoded) = serde_json::to_vec(input)
                && encoded.len() <= MAX_INPUT_BYTES
            {
                let mut digest = Sha256::new();
                digest.update(name.as_bytes());
                digest.update([0]);
                digest.update(&encoded);
                let call = (name.as_str(), format!("{:x}", digest.finalize()), index);
                calls
                    .entry(id.as_str())
                    .and_modify(|previous| *previous = None)
                    .or_insert(Some(call));
            }
        }
    }
    let mut result_counts = HashMap::new();
    for message in session.messages.iter().skip(start) {
        for block in message.blocks.iter().take(32) {
            if let Block::ToolResult { id, .. } = block {
                *result_counts.entry(id.as_str()).or_insert(0_usize) += 1;
            }
        }
    }
    let mut failures: BTreeMap<(&str, String), Vec<Evidence>> = BTreeMap::new();
    for (index, message) in session.messages.iter().enumerate().skip(start) {
        for block in message.blocks.iter().take(32) {
            if let Block::ToolResult {
                id, is_error: true, ..
            } = block
                && result_counts.get(id.as_str()) == Some(&1)
                && call_counts.get(id.as_str()) == Some(&1)
                && let Some(Some((name, input_hash, call_message))) = calls.get(id.as_str())
                && *call_message < index
            {
                let evidence = failures.entry((*name, input_hash.clone())).or_default();
                if evidence.len() < 32 {
                    evidence.push(Evidence {
                        call_message: *call_message,
                        result_message: index,
                    });
                }
            }
        }
    }
    failures
        .into_iter()
        .filter(|(_, evidence)| evidence.len() >= 2)
        .take(MAX_SUGGESTIONS)
        .map(|((tool, input_hash), evidence)| {
            let mut suggestion = Suggestion {
                id: String::new(),
                kind: "repeated_failure".into(),
                session: session.id.clone(),
                tool: tool.into(),
                input_hash,
                evidence,
                recommendation: RECOMMENDATION.into(),
                state: SuggestionState::Candidate,
                history: Vec::new(),
            };
            suggestion.id = suggestion_id(&suggestion);
            suggestion
        })
        .collect()
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct Records {
    version: u32,
    suggestions: Vec<Suggestion>,
}

pub struct SuggestionStore {
    dir: PathBuf,
    session: String,
}

fn reject_link(path: &Path) -> Result<()> {
    match std::fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() => {
            Err(Error::Tool("suggestion path is a symbolic link".into()))
        }
        Ok(_) => Ok(()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error.into()),
    }
}

impl SuggestionStore {
    pub fn open(data: &Path, session: &str) -> Result<Self> {
        if !crate::session::valid_id(session) {
            return Err(Error::Tool("invalid suggestion session".into()));
        }
        let data = data.canonicalize()?;
        let dir = data.join("suggestions");
        reject_link(&dir)?;
        if dir.try_exists()? && dir.canonicalize()? != dir {
            return Err(Error::Tool(
                "suggestion directory escaped data directory".into(),
            ));
        }
        Ok(Self {
            dir,
            session: session.into(),
        })
    }

    fn path(&self) -> PathBuf {
        self.dir.join(format!("{}.json", self.session))
    }

    fn lock(&self) -> Result<File> {
        reject_link(&self.dir)?;
        std::fs::create_dir_all(&self.dir)?;
        if self.dir.canonicalize()? != self.dir {
            return Err(Error::Tool(
                "suggestion directory escaped data directory".into(),
            ));
        }
        let path = self.dir.join(format!("{}.lock", self.session));
        reject_link(&path)?;
        let file = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .open(path)?;
        file.try_lock_exclusive()
            .map_err(|error| Error::Tool(format!("suggestion store is busy: {error}")))?;
        Ok(file)
    }

    fn read(&self) -> Result<Records> {
        let path = self.path();
        reject_link(&path)?;
        if !path.try_exists()? {
            return Ok(Records {
                version: 1,
                suggestions: Vec::new(),
            });
        }
        let records: Records = read_json(&path)?;
        let mut ids = HashSet::new();
        if records.version != 1
            || records.suggestions.len() > MAX_RECORDS
            || records.suggestions.iter().any(|item| {
                item.session != self.session
                    || item.kind != "repeated_failure"
                    || !matches!(
                        item.tool.as_str(),
                        "read" | "write" | "edit" | "grep" | "glob" | "shell"
                    )
                    || item.input_hash.len() != 64
                    || !item.input_hash.bytes().all(|byte| byte.is_ascii_hexdigit())
                    || item.evidence.len() < 2
                    || item.evidence.len() > 32
                    || item
                        .evidence
                        .iter()
                        .any(|evidence| evidence.call_message >= evidence.result_message)
                    || item.id != suggestion_id(item)
                    || !ids.insert(item.id.clone())
                    || item.recommendation != RECOMMENDATION
                    || item
                        .history
                        .iter()
                        .any(|decision| decision.reason.len() > 512)
                    || match item.state {
                        SuggestionState::Candidate => !item.history.is_empty(),
                        SuggestionState::Accepted | SuggestionState::Rejected => {
                            item.history.len() != 1 || item.history[0].state != item.state
                        }
                        SuggestionState::Revoked => {
                            item.history.len() != 2
                                || item.history[0].state != SuggestionState::Accepted
                                || item.history[1].state != SuggestionState::Revoked
                                || item.history[1].reason.trim().is_empty()
                        }
                    }
            })
        {
            return Err(Error::Tool("suggestion data changed or is invalid".into()));
        }
        Ok(records)
    }

    pub fn list(&self) -> Result<Vec<Suggestion>> {
        if !self.dir.try_exists()? {
            return Ok(Vec::new());
        }
        let _lock = self.lock()?;
        Ok(self.read()?.suggestions)
    }

    pub fn generate(&self, session: &Session) -> Result<Vec<Suggestion>> {
        if session.id != self.session {
            return Err(Error::Tool("suggestion session mismatch".into()));
        }
        let proposed = analyze(session);
        let _lock = self.lock()?;
        let mut records = self.read()?;
        let mut changed = false;
        for candidate in proposed {
            if !records
                .suggestions
                .iter()
                .any(|item| item.id == candidate.id)
            {
                if records.suggestions.len() == MAX_RECORDS {
                    return Err(Error::Tool("suggestion store is full".into()));
                }
                records.suggestions.push(candidate);
                changed = true;
            }
        }
        if changed {
            atomic_json(&self.path(), &records)?;
        }
        Ok(records.suggestions)
    }

    pub fn decide(
        &self,
        session: &Session,
        id: &str,
        expected: SuggestionState,
        next: SuggestionState,
        reason: &str,
    ) -> Result<Suggestion> {
        if !matches!(
            (expected, next),
            (
                SuggestionState::Candidate,
                SuggestionState::Accepted | SuggestionState::Rejected
            ) | (SuggestionState::Accepted, SuggestionState::Revoked)
        ) || reason.len() > 512
            || (next == SuggestionState::Revoked && reason.trim().is_empty())
        {
            return Err(Error::Tool("invalid suggestion decision".into()));
        }
        if session.id != self.session
            || (next == SuggestionState::Accepted
                && !analyze(session)
                    .iter()
                    .any(|suggestion| suggestion.id == id))
        {
            return Err(Error::Tool(
                "suggestion evidence changed; review it again".into(),
            ));
        }
        let _lock = self.lock()?;
        let mut records = self.read()?;
        let suggestion = records
            .suggestions
            .iter_mut()
            .find(|item| item.id == id)
            .ok_or_else(|| Error::Tool("suggestion not found".into()))?;
        if suggestion.state != expected {
            return Err(Error::Tool(
                "suggestion state changed; review it again".into(),
            ));
        }
        suggestion.state = next;
        suggestion.history.push(Decision {
            state: next,
            at: crate::session::now(),
            reason: reason.into(),
        });
        let result = suggestion.clone();
        atomic_json(&self.path(), &records)?;
        Ok(result)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::{Block, Message, Role};
    use serde_json::json;

    #[test]
    fn repeated_identical_failures_produce_a_bounded_private_candidate() {
        let root = tempfile::tempdir().unwrap();
        let mut session =
            crate::session::Session::new(root.path().into(), "test".into(), "test".into(), None);
        for id in ["one", "two"] {
            session.messages.push(Message {
                role: Role::Assistant,
                blocks: vec![Block::ToolUse {
                    id: id.into(),
                    name: "shell".into(),
                    input: json!({"command":"secret-command"}),
                    extra: None,
                }],
            });
            session.messages.push(Message {
                role: Role::User,
                blocks: vec![Block::ToolResult {
                    id: id.into(),
                    content: "secret-output".into(),
                    is_error: true,
                }],
            });
        }
        let suggestions = analyze(&session);
        assert_eq!(suggestions.len(), 1);
        assert_eq!(suggestions[0].kind, "repeated_failure");
        assert_eq!(suggestions[0].evidence.len(), 2);
        let encoded = serde_json::to_string(&suggestions).unwrap();
        assert!(!encoded.contains("secret-command"));
        assert!(!encoded.contains("secret-output"));
        session.messages.push(Message {
            role: Role::Assistant,
            blocks: vec![Block::ToolUse {
                id: "two".into(),
                name: "other".into(),
                input: json!({}),
                extra: None,
            }],
        });
        assert!(analyze(&session).is_empty());
        session.messages.rewrite().pop();
        if let Block::ToolResult { is_error, .. } = &mut session.messages.rewrite()[3].blocks[0] {
            *is_error = false;
        }
        assert!(analyze(&session).is_empty());
        session.messages.push(Message {
            role: Role::User,
            blocks: vec![Block::ToolResult {
                id: "two".into(),
                content: "later failure".into(),
                is_error: true,
            }],
        });
        assert!(analyze(&session).is_empty());
    }

    #[test]
    fn suggestions_are_persisted_idempotently_and_require_explicit_decisions() {
        let root = tempfile::tempdir().unwrap();
        let mut session =
            crate::session::Session::new(root.path().into(), "test".into(), "test".into(), None);
        for id in ["one", "two"] {
            session.messages.push(Message {
                role: Role::Assistant,
                blocks: vec![Block::ToolUse {
                    id: id.into(),
                    name: "grep".into(),
                    input: json!({"pattern":"x"}),
                    extra: None,
                }],
            });
            session.messages.push(Message {
                role: Role::User,
                blocks: vec![Block::ToolResult {
                    id: id.into(),
                    content: "failed".into(),
                    is_error: true,
                }],
            });
        }
        let store = SuggestionStore::open(root.path(), &session.id).unwrap();
        let first = store.generate(&session).unwrap();
        assert_eq!(first.len(), 1);
        assert_eq!(first[0].state, SuggestionState::Candidate);
        assert_eq!(store.generate(&session).unwrap().len(), 1);
        let reopened = SuggestionStore::open(root.path(), &session.id).unwrap();
        assert_eq!(reopened.list().unwrap().len(), 1);
        let id = &first[0].id;
        reopened
            .decide(
                &session,
                id,
                SuggestionState::Candidate,
                SuggestionState::Accepted,
                "",
            )
            .unwrap();
        assert!(
            reopened
                .decide(
                    &session,
                    id,
                    SuggestionState::Candidate,
                    SuggestionState::Rejected,
                    ""
                )
                .is_err()
        );
        assert!(
            reopened
                .decide(
                    &session,
                    id,
                    SuggestionState::Accepted,
                    SuggestionState::Revoked,
                    ""
                )
                .is_err()
        );
        reopened
            .decide(
                &session,
                id,
                SuggestionState::Accepted,
                SuggestionState::Revoked,
                "outdated",
            )
            .unwrap();
        assert_eq!(reopened.list().unwrap()[0].state, SuggestionState::Revoked);
        assert_eq!(
            store.generate(&session).unwrap()[0].state,
            SuggestionState::Revoked
        );
    }

    #[test]
    fn suggestions_reject_evidence_drift_and_tampered_records() {
        let root = tempfile::tempdir().unwrap();
        let mut session =
            crate::session::Session::new(root.path().into(), "test".into(), "test".into(), None);
        for id in ["one", "two"] {
            session.messages.push(Message {
                role: Role::Assistant,
                blocks: vec![Block::ToolUse {
                    id: id.into(),
                    name: "grep".into(),
                    input: json!({"pattern":"x"}),
                    extra: None,
                }],
            });
            session.messages.push(Message {
                role: Role::User,
                blocks: vec![Block::ToolResult {
                    id: id.into(),
                    content: "failed".into(),
                    is_error: true,
                }],
            });
        }
        let store = SuggestionStore::open(root.path(), &session.id).unwrap();
        let id = store.generate(&session).unwrap()[0].id.clone();
        session.messages.rewrite().remove(0);
        assert!(
            store
                .decide(
                    &session,
                    &id,
                    SuggestionState::Candidate,
                    SuggestionState::Accepted,
                    ""
                )
                .is_err()
        );
        assert_eq!(store.list().unwrap()[0].state, SuggestionState::Candidate);
        assert_eq!(
            store
                .decide(
                    &session,
                    &id,
                    SuggestionState::Candidate,
                    SuggestionState::Rejected,
                    ""
                )
                .unwrap()
                .state,
            SuggestionState::Rejected
        );
        let path = root
            .path()
            .join("suggestions")
            .join(format!("{}.json", session.id));
        let mut records: serde_json::Value = read_json(&path).unwrap();
        records["suggestions"][0]["recommendation"] = "silently execute a command".into();
        atomic_json(&path, &records).unwrap();
        assert!(store.list().is_err());
    }

    #[test]
    fn suggestions_lock_and_session_isolation_fail_closed() {
        let root = tempfile::tempdir().unwrap();
        let session =
            crate::session::Session::new(root.path().into(), "test".into(), "test".into(), None);
        let other =
            crate::session::Session::new(root.path().into(), "test".into(), "test".into(), None);
        let store = SuggestionStore::open(root.path(), &session.id).unwrap();
        assert!(store.generate(&other).is_err());
        assert!(!root.path().join("suggestions").exists());
        let lock = store.lock().unwrap();
        assert!(store.generate(&session).is_err());
        drop(lock);
        assert!(store.generate(&session).unwrap().is_empty());
    }
}
