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
    /// Project root the skill belongs to; `None` is global (every project).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub scope: Option<String>,
    /// How often the approved skill was invoked, and when last (for retirement).
    #[serde(default, skip_serializing_if = "is_zero")]
    pub uses: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_used: Option<u64>,
}

fn is_zero(value: &u64) -> bool {
    *value == 0
}

impl Candidate {
    /// Loaded in the project `root` (`None`: outside any project).
    pub fn visible_in(&self, root: Option<&str>) -> bool {
        self.scope.is_none() || self.scope.as_deref() == root
    }

    /// When an approved skill last proved useful: its last invocation, or when it was
    /// approved or last updated.
    fn last_active(&self) -> u64 {
        let approved = self
            .history
            .first()
            .map_or(self.created_at, |decision| decision.at);
        self.last_used.map_or(approved, |used| used.max(approved))
    }

    /// Ready to go live without a person: seen in two sessions, or taught by a task that
    /// completed and passed verification.
    fn proven(&self) -> bool {
        self.sessions.len() >= ACTIVATE_SESSIONS
            || self.sources.iter().any(|source| {
                source.completed == Some(true) && source.verification == ValidationState::Passed
            })
    }
}

/// Actor recorded for decisions the skill review makes on its own.
pub const REVIEW_ACTOR: &str = "skill_review";
/// Pending candidates seen in only one session are dropped after this long.
pub const STALE_SECS: u64 = 30 * 24 * 3600;
/// Approved learned skills nobody invoked for this long are retired.
pub const UNUSED_SECS: u64 = 60 * 24 * 3600;
/// Sessions a candidate must appear in before it goes live on its own.
const ACTIVATE_SESSIONS: usize = 2;
/// Most approved learned skills per project, and globally.
const MAX_ACTIVE_PROJECT: usize = 20;
const MAX_ACTIVE_GLOBAL: usize = 30;
/// Longest learned instruction text; a revision past this is refused, not appended.
pub const MAX_LEARNED_BODY: usize = 2048;

/// One workflow the review proposed.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Proposal {
    pub kind: ProposalKind,
    pub name: String,
    pub description: String,
    pub body: String,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ProposalKind {
    /// A new workflow for the current project only.
    Project,
    /// A new workflow useful in any project.
    Global,
    /// A revised version of an existing learned skill (pending or approved).
    Update,
}

/// What [`CandidateStore::learn`] did.
#[derive(Debug)]
pub enum Learned {
    /// A new pending candidate.
    New(Candidate),
    /// Seen again: an existing candidate gained this session as a source.
    Recurred(Candidate),
    /// A pending candidate's content was replaced.
    Revised(Candidate),
    /// An approved skill's content was replaced and stays approved.
    Updated(Candidate),
    /// A candidate went live on its own (it recurred, or its task passed verification).
    Activated(Candidate),
    /// Nothing stored (already covered, rejected, or a repeat of this session).
    Skipped,
}

fn name_tokens(name: &str) -> HashSet<&str> {
    name.split(['-', '_'])
        .filter(|token| !token.is_empty())
        .collect()
}

/// Same name, or names sharing at least two words and half of all words
/// (`diagnose-exchange-api-key-rejection` and `diagnose-exchange-api-auth-rejection`).
pub fn similar_names(a: &str, b: &str) -> bool {
    if a == b {
        return true;
    }
    let (a, b) = (name_tokens(a), name_tokens(b));
    let shared = a.intersection(&b).count();
    shared >= 2 && shared * 2 >= a.union(&b).count()
}

/// How [`CandidateStore::learn`] may act without a person.
pub struct Rules<'a> {
    /// Activate proven candidates, update approved skills and retire unused ones
    /// (config `skill_review_auto`). Off: proposals only become candidates.
    pub automatic: bool,
    /// Names that belong to another skill (built-in, plugin, user or project files).
    pub taken: &'a dyn Fn(&str) -> bool,
}

/// What one review changed in the store.
#[derive(Debug)]
pub struct LearnOutcome {
    pub learned: Learned,
    /// Approved skills retired because they went unused or exceeded the per-scope cap.
    pub retired: Vec<Candidate>,
}

fn retire(candidate: &mut Candidate, at: u64, reason: &str) {
    candidate.state = CandidateState::Revoked;
    candidate.history.push(Decision {
        state: CandidateState::Revoked,
        actor: REVIEW_ACTOR.into(),
        at,
        reason: reason.into(),
    });
}

/// Retires approved skills unused for [`UNUSED_SECS`], then the least recently active ones
/// in any scope holding more than its cap. Returns the retired skills.
fn maintain(records: &mut Records, now: u64) -> Vec<Candidate> {
    let mut retired = Vec::new();
    for candidate in &mut records.candidates {
        if candidate.state == CandidateState::Approved
            && now.saturating_sub(candidate.last_active()) >= UNUSED_SECS
        {
            retire(candidate, now, "unused for 60 days");
            retired.push(candidate.clone());
        }
    }
    let scopes: HashSet<Option<String>> = records
        .candidates
        .iter()
        .filter(|candidate| candidate.state == CandidateState::Approved)
        .map(|candidate| candidate.scope.clone())
        .collect();
    for scope in scopes {
        let cap = if scope.is_some() {
            MAX_ACTIVE_PROJECT
        } else {
            MAX_ACTIVE_GLOBAL
        };
        let mut active: Vec<usize> = (0..records.candidates.len())
            .filter(|&index| {
                let candidate = &records.candidates[index];
                candidate.state == CandidateState::Approved && candidate.scope == scope
            })
            .collect();
        if active.len() <= cap {
            continue;
        }
        active.sort_by_key(|&index| records.candidates[index].last_active());
        for &index in &active[..active.len() - cap] {
            let candidate = &mut records.candidates[index];
            retire(candidate, now, "least used beyond the per-scope limit");
            retired.push(candidate.clone());
        }
    }
    retired
}

/// Removes pending candidates seen in one session and older than `max_age` seconds;
/// `max_age == 0` removes every pending candidate.
fn prune_records(records: &mut Records, now: u64, max_age: u64) -> usize {
    let before = records.candidates.len();
    records.candidates.retain(|candidate| {
        candidate.state != CandidateState::Candidate
            || (max_age > 0
                && (candidate.sessions.len() > 1
                    || now.saturating_sub(candidate.created_at) < max_age))
    });
    before - records.candidates.len()
}

/// Frees a slot for a new candidate: stale pending ones go first, then the least-recurring,
/// oldest pending, rejected or automatically retired one. Approvals and user revocations are
/// kept, so their names keep blocking re-proposals.
fn make_room(records: &mut Records) -> Result<()> {
    prune_records(records, now(), STALE_SECS);
    if records.candidates.len() < MAX_CANDIDATES {
        return Ok(());
    }
    let victim = records
        .candidates
        .iter()
        .enumerate()
        .filter(|(_, candidate)| {
            matches!(
                candidate.state,
                CandidateState::Candidate | CandidateState::Rejected
            ) || (candidate.state == CandidateState::Revoked
                && candidate
                    .history
                    .last()
                    .is_some_and(|decision| decision.actor == REVIEW_ACTOR))
        })
        .min_by_key(|(_, candidate)| (candidate.sessions.len(), candidate.created_at))
        .map(|(index, _)| index)
        .ok_or_else(|| Error::Tool("candidate store is full".into()))?;
    records.candidates.remove(victim);
    Ok(())
}

/// Appends without failing when full: the oldest entries give way.
fn push_capped<T: PartialEq + Clone>(list: &mut Vec<T>, items: &[T], cap: usize) -> bool {
    let mut changed = false;
    for item in items {
        if !list.contains(item) {
            if list.len() >= cap {
                list.remove(0);
            }
            list.push(item.clone());
            changed = true;
        }
    }
    changed
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

/// The global form hashes exactly what older releases hashed, so their ids stay valid.
fn content_id(name: &str, description: &str, body: &str, scope: Option<&str>) -> String {
    let mut digest = Sha256::new();
    for text in [name, description, body].into_iter().chain(scope) {
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
            if candidate.id
                != content_id(
                    &candidate.name,
                    &candidate.description,
                    &candidate.body,
                    candidate.scope.as_deref(),
                )
                || !ids.insert(candidate.id.as_str())
                || (candidate.state == CandidateState::Approved
                    && !approved_names
                        .insert((candidate.scope.as_deref(), candidate.name.as_str())))
                || candidate
                    .scope
                    .as_ref()
                    .is_some_and(|scope| scope.is_empty() || scope.len() > 1024)
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
        let id = content_id(name, description, body, None);
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
        make_room(&mut records)?;
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
            scope: None,
            uses: 0,
            last_used: None,
        };
        records.candidates.push(candidate.clone());
        atomic_json(&self.dir.join("records.json"), &records)?;
        Ok(candidate)
    }

    /// Stores what a skill review proposed in the project `root` (`None`: no project), with
    /// no person in the loop.
    ///
    /// - `Project`/`Global`: a name similar to a rejected or user-revoked candidate is
    ///   dropped; similar to a live (pending or approved) one only records the recurrence.
    /// - `Update`: replaces the content of the named live candidate, keeping its name,
    ///   scope, sources and state, so an approved skill stays approved. An unknown name is
    ///   stored as a new candidate.
    /// - With [`Rules::automatic`] only: a pending candidate that is now
    ///   [proven](Candidate::proven) is approved unless its name is
    ///   [taken](Rules::taken); updates reach approved skills; unused and over-cap approved
    ///   skills are retired ([`maintain`]). Without it an update of an approved skill is
    ///   dropped.
    ///
    /// Waits briefly for the lock: the review runs beside an interactive session.
    pub(crate) fn learn(
        &self,
        proposal: &Proposal,
        root: Option<&str>,
        session: &str,
        sources: &[TaskSource],
        rules: &Rules,
    ) -> Result<LearnOutcome> {
        let Proposal {
            name,
            description,
            body,
            ..
        } = proposal;
        if !identifier(name)
            || description.len() > 1024
            || body.trim().is_empty()
            || session.is_empty()
            || session.len() > 128
            || root.is_some_and(|root| root.is_empty() || root.len() > 1024)
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
        if body.len() > MAX_LEARNED_BODY {
            return Err(Error::Tool(format!(
                "steps longer than {MAX_LEARNED_BODY} bytes; not saved"
            )));
        }
        let _lock = self.lock_with_wait(Duration::from_millis(250))?;
        let mut records = self.read()?;
        let at = now();
        let (mut learned, index) = Self::apply_proposal(
            &mut records,
            proposal,
            root,
            session,
            sources,
            rules.automatic,
        )?;
        if let (Some(index), Learned::New(_) | Learned::Recurred(_) | Learned::Revised(_)) =
            (index, &learned)
        {
            let candidate = &records.candidates[index];
            let duplicate = records.candidates.iter().any(|other| {
                other.id != candidate.id
                    && other.state == CandidateState::Approved
                    && other.name == candidate.name
                    && other.scope == candidate.scope
            });
            if rules.automatic
                && candidate.state == CandidateState::Candidate
                && candidate.proven()
                && !duplicate
                && !(rules.taken)(&candidate.name)
            {
                let candidate = &mut records.candidates[index];
                candidate.state = CandidateState::Approved;
                candidate.approved_hash = Some(candidate.id.clone());
                candidate.history = vec![Decision {
                    state: CandidateState::Approved,
                    actor: REVIEW_ACTOR.into(),
                    at,
                    reason: if candidate.sessions.len() >= ACTIVATE_SESSIONS {
                        format!("seen in {} sessions", candidate.sessions.len())
                    } else {
                        "taught by a verified task".into()
                    },
                }];
                learned = Learned::Activated(candidate.clone());
            }
        }
        let retired = if rules.automatic {
            maintain(&mut records, at)
        } else {
            Vec::new()
        };
        // A skill retired in the same pass is reported once, as retired.
        if let Learned::Activated(candidate) | Learned::Updated(candidate) = &learned
            && retired.iter().any(|gone| gone.id == candidate.id)
        {
            learned = Learned::Skipped;
        }
        if index.is_some() || !retired.is_empty() {
            atomic_json(&self.dir.join("records.json"), &records)?;
        }
        Ok(LearnOutcome { learned, retired })
    }

    /// The store change for one proposal, and the index it touched (`None`: nothing changed).
    fn apply_proposal(
        records: &mut Records,
        proposal: &Proposal,
        root: Option<&str>,
        session: &str,
        sources: &[TaskSource],
        automatic: bool,
    ) -> Result<(Learned, Option<usize>)> {
        let Proposal {
            kind,
            name,
            description,
            body,
        } = proposal;
        let live = |candidate: &Candidate| {
            matches!(
                candidate.state,
                CandidateState::Candidate | CandidateState::Approved
            ) && candidate.visible_in(root)
        };
        // Approved first (one per scope and name), then the newest pending version.
        let find = |records: &Records, matches: &dyn Fn(&Candidate) -> bool| {
            records
                .candidates
                .iter()
                .enumerate()
                .filter(|(_, candidate)| live(candidate) && matches(candidate))
                .max_by_key(|(_, candidate)| {
                    (
                        candidate.state == CandidateState::Approved,
                        candidate.created_at,
                    )
                })
                .map(|(index, _)| index)
        };
        let target = match kind {
            ProposalKind::Update => find(records, &|candidate| candidate.name == *name),
            ProposalKind::Project | ProposalKind::Global => None,
        };
        let Some(index) = target else {
            let refused = records.candidates.iter().any(|candidate| {
                candidate.visible_in(root)
                    && similar_names(&candidate.name, name)
                    && (candidate.state == CandidateState::Rejected
                        || (candidate.state == CandidateState::Revoked
                            && candidate
                                .history
                                .last()
                                .is_some_and(|decision| decision.actor != REVIEW_ACTOR)))
            });
            if refused {
                return Ok((Learned::Skipped, None));
            }
            if let Some(index) = find(records, &|candidate| similar_names(&candidate.name, name)) {
                // Another session, or another task of this one (which may prove it).
                let candidate = &mut records.candidates[index];
                let changed =
                    push_capped(&mut candidate.sessions, &[session.to_owned()], MAX_SESSIONS)
                        | push_capped(&mut candidate.sources, sources, MAX_SOURCES);
                return Ok(if changed {
                    (Learned::Recurred(candidate.clone()), Some(index))
                } else {
                    (Learned::Skipped, None)
                });
            }
            let scope = match kind {
                ProposalKind::Global => None,
                _ => root.map(str::to_owned),
            };
            let id = content_id(name, description, body, scope.as_deref());
            if records
                .candidates
                .iter()
                .any(|candidate| candidate.id == id)
            {
                return Ok((Learned::Skipped, None));
            }
            make_room(records)?;
            let candidate = Candidate {
                id,
                name: name.clone(),
                description: description.clone(),
                body: body.clone(),
                sessions: vec![session.to_owned()],
                sources: sources
                    .iter()
                    .rev()
                    .take(MAX_SOURCES)
                    .rev()
                    .cloned()
                    .collect(),
                legacy_sources: Vec::new(),
                created_at: now(),
                state: CandidateState::Candidate,
                validation: ValidationState::Unknown,
                approved_hash: None,
                history: Vec::new(),
                scope,
                uses: 0,
                last_used: None,
            };
            records.candidates.push(candidate.clone());
            return Ok((Learned::New(candidate), Some(records.candidates.len() - 1)));
        };
        let old = records.candidates[index].clone();
        if old.state == CandidateState::Approved && !automatic {
            // Approved skills change only by hand unless updates are automatic.
            return Ok((Learned::Skipped, None));
        }
        let id = content_id(name, description, body, old.scope.as_deref());
        if id == old.id
            || records
                .candidates
                .iter()
                .any(|candidate| candidate.id == id)
        {
            // Unchanged, or the revised text is already stored as another version.
            return Ok((Learned::Skipped, None));
        }
        let mut revised = old.clone();
        revised.id = id.clone();
        revised.description = description.clone();
        revised.body = body.clone();
        push_capped(&mut revised.sessions, &[session.to_owned()], MAX_SESSIONS);
        push_capped(&mut revised.sources, sources, MAX_SOURCES);
        revised.validation = ValidationState::Unknown;
        let approved = old.state == CandidateState::Approved;
        if approved {
            revised.approved_hash = Some(id);
            revised.history = vec![Decision {
                state: CandidateState::Approved,
                actor: REVIEW_ACTOR.into(),
                at: now(),
                reason: format!("auto-update of {}", &old.id[..12]),
            }];
        }
        records.candidates[index] = revised.clone();
        Ok((
            if approved {
                Learned::Updated(revised)
            } else {
                Learned::Revised(revised)
            },
            Some(index),
        ))
    }

    /// Counts an invocation of the approved skill `id`; best effort (a busy store is skipped).
    pub fn touch(&self, id: &str) -> Result<()> {
        let _lock = self.lock()?;
        let mut records = self.read()?;
        if let Some(candidate) = records
            .candidates
            .iter_mut()
            .find(|candidate| candidate.id == id && candidate.state == CandidateState::Approved)
        {
            candidate.uses = candidate.uses.saturating_add(1);
            candidate.last_used = Some(now());
            atomic_json(&self.dir.join("records.json"), &records)?;
        }
        Ok(())
    }

    /// Drops pending candidates seen in one session and older than `max_age` seconds;
    /// `max_age == 0` drops every pending candidate. Returns how many were removed.
    pub fn prune(&self, max_age: u64) -> Result<usize> {
        let _lock = self.lock()?;
        let mut records = self.read()?;
        let removed = prune_records(&mut records, now(), max_age);
        if removed > 0 {
            atomic_json(&self.dir.join("records.json"), &records)?;
        }
        Ok(removed)
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
            let target = records
                .candidates
                .iter()
                .find(|candidate| candidate.id == id)
                .ok_or_else(|| Error::Tool("candidate not found".into()))?;
            let (name, scope) = (target.name.clone(), target.scope.clone());
            if records.candidates.iter().any(|candidate| {
                candidate.id != id
                    && candidate.name == name
                    && candidate.scope == scope
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

    impl CandidateStore {
        /// `learn` with no taken names, returning what it did to the proposal.
        fn learn_test(
            &self,
            proposal: &Proposal,
            root: Option<&str>,
            session: &str,
            sources: &[TaskSource],
        ) -> Result<Learned> {
            self.learn(proposal, root, session, sources, &AUTO)
                .map(|outcome| outcome.learned)
        }
    }

    fn proposal(kind: ProposalKind, name: &str, body: &str) -> Proposal {
        Proposal {
            kind,
            name: name.into(),
            description: format!("When {name}"),
            body: body.into(),
        }
    }

    fn project(name: &str, body: &str) -> Proposal {
        proposal(ProposalKind::Project, name, body)
    }

    const ROOT: Option<&str> = Some("D:/work/quote-hub");
    const AUTO: Rules<'static> = Rules {
        automatic: true,
        taken: &|_| false,
    };

    #[test]
    fn similar_names_match_reworded_workflows_only() {
        assert!(similar_names(
            "diagnose-exchange-api-key-rejection",
            "diagnose-exchange-api-auth-rejection"
        ));
        assert!(similar_names("deploy", "deploy"));
        assert!(!similar_names(
            "glab-auth-diagnose-windows",
            "eks-connection-error-diagnosis"
        ));
        assert!(!similar_names(
            "run-tests",
            "run-integration-tests-against-local-services"
        ));
    }

    #[test]
    fn learn_scopes_new_candidates_and_global_ids_match_older_releases() {
        let data = tempfile::tempdir().unwrap();
        let store = CandidateStore::open(data.path()).unwrap();
        let Learned::New(local) = store
            .learn_test(&project("orderbook", "a"), ROOT, "s1", &[])
            .unwrap()
        else {
            panic!()
        };
        assert_eq!(local.scope.as_deref(), ROOT);
        assert!(local.visible_in(ROOT) && !local.visible_in(Some("D:/other")));
        let Learned::New(global) = store
            .learn_test(
                &proposal(ProposalKind::Global, "aws-sso", "b"),
                ROOT,
                "s1",
                &[],
            )
            .unwrap()
        else {
            panic!()
        };
        assert_eq!(global.scope, None);
        assert!(global.visible_in(Some("D:/other")));
        assert_eq!(global.id, content_id("aws-sso", "When aws-sso", "b", None));
        // Project scope without a project falls back to global.
        let Learned::New(rootless) = store
            .learn_test(&project("x-y", "c"), None, "s1", &[])
            .unwrap()
        else {
            panic!()
        };
        assert_eq!(rootless.scope, None);
        assert_eq!(store.list().unwrap().len(), 3);
    }

    #[test]
    fn learn_folds_similar_names_into_one_candidate_and_counts_sessions() {
        let data = tempfile::tempdir().unwrap();
        let store = CandidateStore::open(data.path()).unwrap();
        store
            .learn_test(
                &project("diagnose-exchange-api-key-rejection", "a"),
                ROOT,
                "s1",
                &[],
            )
            .unwrap();
        assert!(matches!(
            store
                .learn_test(
                    &project("diagnose-exchange-api-auth-rejection", "b"),
                    ROOT,
                    "s1",
                    &[]
                )
                .unwrap(),
            Learned::Skipped
        ));
        // The second session proves it: it goes live without a person.
        let Learned::Activated(again) = store
            .learn_test(
                &project("diagnose-exchange-api-auth-rejection", "b"),
                ROOT,
                "s2",
                &[],
            )
            .unwrap()
        else {
            panic!()
        };
        assert_eq!(again.name, "diagnose-exchange-api-key-rejection");
        assert_eq!(again.body, "a");
        assert_eq!(again.sessions, ["s1", "s2"]);
        assert_eq!(again.state, CandidateState::Approved);
        assert_eq!(again.history[0].actor, REVIEW_ACTOR);
        // Another project does not see it, so it gets its own candidate.
        assert!(matches!(
            store
                .learn_test(
                    &project("diagnose-exchange-api-key-rejection", "a"),
                    Some("D:/other"),
                    "s3",
                    &[]
                )
                .unwrap(),
            Learned::New(_)
        ));
        assert_eq!(store.list().unwrap().len(), 2);
    }

    #[test]
    fn learn_drops_names_the_user_rejected_or_revoked() {
        let data = tempfile::tempdir().unwrap();
        let store = CandidateStore::open(data.path()).unwrap();
        let Learned::New(first) = store
            .learn_test(&project("flaky-clock-fix", "a"), ROOT, "s1", &[])
            .unwrap()
        else {
            panic!()
        };
        store
            .decide(
                &first.id,
                CandidateState::Candidate,
                CandidateState::Rejected,
                "user",
                "",
            )
            .unwrap();
        assert!(matches!(
            store
                .learn_test(&project("flaky-clock-fix", "b"), ROOT, "s2", &[])
                .unwrap(),
            Learned::Skipped
        ));
        assert!(matches!(
            store
                .learn_test(
                    &proposal(ProposalKind::Update, "flaky-clock-fix", "c"),
                    ROOT,
                    "s2",
                    &[]
                )
                .unwrap(),
            Learned::Skipped
        ));
        assert_eq!(store.list().unwrap().len(), 1);
    }

    #[test]
    fn learn_updates_pending_and_approved_skills_in_place() {
        let data = tempfile::tempdir().unwrap();
        let store = CandidateStore::open(data.path()).unwrap();
        let Learned::New(pending) = store
            .learn_test(&project("deploy", "v1"), ROOT, "s1", &[])
            .unwrap()
        else {
            panic!()
        };
        // Same session, unverified: revised but still pending.
        let Learned::Revised(revised) = store
            .learn_test(
                &proposal(ProposalKind::Update, "deploy", "v2"),
                ROOT,
                "s1",
                &[],
            )
            .unwrap()
        else {
            panic!()
        };
        assert_ne!(revised.id, pending.id);
        assert_eq!(revised.state, CandidateState::Candidate);
        assert_eq!(revised.sessions, ["s1"]);
        assert_eq!(revised.scope.as_deref(), ROOT);
        // A verified task proves it even within one session.
        let verified = TaskSource {
            session: "s1".into(),
            task_id: None,
            message_start: None,
            message_end: 4,
            completed: Some(true),
            verification: ValidationState::Passed,
        };
        let Learned::Activated(active) = store
            .learn_test(
                &proposal(ProposalKind::Update, "deploy", "v2b"),
                ROOT,
                "s1",
                &[verified],
            )
            .unwrap()
        else {
            panic!()
        };
        assert_eq!(active.history[0].reason, "taught by a verified task");
        let Learned::Updated(updated) = store
            .learn_test(
                &proposal(ProposalKind::Update, "deploy", "v3"),
                ROOT,
                "s3",
                &[],
            )
            .unwrap()
        else {
            panic!()
        };
        assert_eq!(updated.state, CandidateState::Approved);
        assert_eq!(updated.approved_hash.as_deref(), Some(updated.id.as_str()));
        assert_eq!(updated.history[0].actor, REVIEW_ACTOR);
        let records = store.list().unwrap();
        assert_eq!(records.len(), 1);
        assert_eq!(records[0].body, "v3");
        // An auto-update is not a user revocation: the name stays learnable.
        assert!(matches!(
            store
                .learn_test(&project("deploy", "v4"), ROOT, "s4", &[])
                .unwrap(),
            Learned::Recurred(_)
        ));
        // Updating an unknown name stores a new candidate.
        assert!(matches!(
            store
                .learn_test(
                    &proposal(ProposalKind::Update, "brand-new", "x"),
                    ROOT,
                    "s4",
                    &[]
                )
                .unwrap(),
            Learned::New(_)
        ));
    }

    #[test]
    fn stale_single_session_candidates_are_pruned_and_full_store_evicts() {
        let data = tempfile::tempdir().unwrap();
        let store = CandidateStore::open(data.path()).unwrap();
        for (name, sessions) in [("old-once", 1), ("old-twice", 2), ("fresh", 1)] {
            for session in 0..sessions {
                store
                    .learn_test(&project(name, "body"), ROOT, &format!("s{session}"), &[])
                    .unwrap();
            }
        }
        let path = data.path().join("skill-candidates/records.json");
        let mut records: Records = read_json(&path).unwrap();
        for candidate in &mut records.candidates {
            if candidate.name.starts_with("old") {
                candidate.created_at -= STALE_SECS + 1;
            }
        }
        atomic_json(&path, &records).unwrap();
        assert_eq!(store.prune(STALE_SECS).unwrap(), 1);
        let names: Vec<_> = store.list().unwrap().into_iter().map(|c| c.name).collect();
        assert_eq!(names, ["old-twice", "fresh"]);
        // "old-twice" went live in its second session; only "fresh" was still pending.
        assert_eq!(store.prune(0).unwrap(), 1);

        let mut records: Records = read_json(&path).unwrap();
        let existing = records.candidates.len();
        for index in 0..MAX_CANDIDATES - existing {
            let name = format!("flow{index}");
            records.candidates.push(Candidate {
                id: content_id(&name, "d", "b", None),
                name,
                description: "d".into(),
                body: "b".into(),
                sessions: vec!["s".into()],
                sources: Vec::new(),
                legacy_sources: Vec::new(),
                created_at: now() + index as u64,
                state: CandidateState::Candidate,
                validation: ValidationState::Unknown,
                approved_hash: None,
                history: Vec::new(),
                scope: None,
                uses: 0,
                last_used: None,
            });
        }
        atomic_json(&path, &records).unwrap();
        assert!(matches!(
            store
                .learn_test(&project("newest", "b"), ROOT, "s", &[])
                .unwrap(),
            Learned::New(_)
        ));
        let records = store.list().unwrap();
        assert_eq!(records.len(), MAX_CANDIDATES);
        assert!(!records.iter().any(|candidate| candidate.name == "flow0"));
    }

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
            let result = store.learn_test(&project("deploy", "build"), None, "session-1", &[]);
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

    #[test]
    fn unused_and_over_cap_skills_retire_without_blocking_relearning() {
        let data = tempfile::tempdir().unwrap();
        let store = CandidateStore::open(data.path()).unwrap();
        for index in 0..MAX_ACTIVE_PROJECT + 1 {
            let name = format!("flow{index}");
            for session in ["s1", "s2"] {
                store
                    .learn_test(&project(&name, "b"), ROOT, session, &[])
                    .unwrap();
            }
        }
        let records = store.list().unwrap();
        let active = |records: &[Candidate]| {
            records
                .iter()
                .filter(|c| c.state == CandidateState::Approved)
                .count()
        };
        assert_eq!(active(&records), MAX_ACTIVE_PROJECT);
        let over = records
            .iter()
            .find(|c| c.state == CandidateState::Revoked)
            .unwrap();
        assert_eq!(over.history.last().unwrap().actor, REVIEW_ACTOR);
        // A used skill survives the idle sweep; the others retire.
        let used = records
            .iter()
            .find(|c| c.state == CandidateState::Approved)
            .unwrap()
            .id
            .clone();
        let path = data.path().join("skill-candidates/records.json");
        let mut stored: Records = read_json(&path).unwrap();
        for candidate in &mut stored.candidates {
            for decision in &mut candidate.history {
                decision.at -= UNUSED_SECS + 1;
            }
        }
        atomic_json(&path, &stored).unwrap();
        store.touch(&used).unwrap();
        let outcome = store
            .learn(&project("other", "b"), ROOT, "s9", &[], &AUTO)
            .unwrap();
        assert_eq!(outcome.retired.len(), MAX_ACTIVE_PROJECT - 1);
        let records = store.list().unwrap();
        assert_eq!(active(&records), 1);
        let kept = records.iter().find(|c| c.id == used).unwrap();
        assert_eq!(kept.state, CandidateState::Approved);
        assert_eq!(kept.uses, 1);
        // Retired by the review, not the user: the name can come back.
        let gone = &outcome.retired[0].name;
        for session in ["s10", "s11"] {
            store
                .learn_test(&project(gone, "new"), ROOT, session, &[])
                .unwrap();
        }
        assert!(
            store
                .list()
                .unwrap()
                .iter()
                .any(|c| c.name == *gone && c.state == CandidateState::Approved)
        );
    }

    #[test]
    fn activation_skips_names_taken_by_other_skills_and_oversized_steps() {
        let data = tempfile::tempdir().unwrap();
        let store = CandidateStore::open(data.path()).unwrap();
        for session in ["s1", "s2"] {
            store
                .learn(
                    &project("plan", "b"),
                    ROOT,
                    session,
                    &[],
                    &Rules {
                        taken: &|name| name == "plan",
                        ..AUTO
                    },
                )
                .unwrap();
        }
        assert_eq!(store.list().unwrap()[0].state, CandidateState::Candidate);
        assert!(
            store
                .learn_test(
                    &project("big", &"x".repeat(MAX_LEARNED_BODY + 1)),
                    ROOT,
                    "s1",
                    &[]
                )
                .is_err()
        );
    }

    #[test]
    fn without_automatic_rules_nothing_goes_live_updates_or_retires() {
        let data = tempfile::tempdir().unwrap();
        let store = CandidateStore::open(data.path()).unwrap();
        let manual = Rules {
            automatic: false,
            ..AUTO
        };
        for session in ["s1", "s2", "s3"] {
            store
                .learn(&project("deploy", "v1"), ROOT, session, &[], &manual)
                .unwrap();
        }
        let candidate = store.list().unwrap().remove(0);
        assert_eq!(candidate.state, CandidateState::Candidate);
        assert_eq!(candidate.sessions.len(), 3);
        store
            .decide(
                &candidate.id,
                CandidateState::Candidate,
                CandidateState::Approved,
                "user",
                "",
            )
            .unwrap();
        let outcome = store
            .learn(
                &proposal(ProposalKind::Update, "deploy", "v2"),
                ROOT,
                "s4",
                &[],
                &manual,
            )
            .unwrap();
        assert!(matches!(outcome.learned, Learned::Skipped));
        let path = data.path().join("skill-candidates/records.json");
        let mut stored: Records = read_json(&path).unwrap();
        stored.candidates[0].history[0].at -= UNUSED_SECS + 1;
        atomic_json(&path, &stored).unwrap();
        let outcome = store
            .learn(&project("other", "x"), ROOT, "s5", &[], &manual)
            .unwrap();
        assert!(outcome.retired.is_empty());
        let records = store.list().unwrap();
        assert_eq!(records[0].body, "v1");
        assert_eq!(records[0].state, CandidateState::Approved);
    }
}
