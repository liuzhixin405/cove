//! Cross-project preferences: extraction proposes them, the user confirms them.
//!
//! A proposal is recorded at once as a project rule (so it applies in this project
//! whatever the user answers) and queued in `<data dir>/preferences.pending.json`; the
//! REPL asks about it before reading the next line, and only a "yes" writes it to the
//! preferences file (`memory::rules`, `Kind::Preferences`), which every project's system
//! prompt includes. Without a terminal (`-p`) nothing is asked; the queue waits.

use std::{
    path::{Path, PathBuf},
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
};

use serde::{Deserialize, Serialize};

use crate::{
    domain::Result,
    memory::rules::{Kind, MAX_PREFERENCE_CHARS, RulesFile, normalize, undated},
    memory::store::{Memory, Scope},
};

/// The queue of proposals, relative to the data directory.
pub const PENDING_FILE: &str = "preferences.pending.json";
/// Where the one-time move of global memories into the preferences keeps the originals.
pub const MIGRATION_BACKUP: &str = "backups/0030-global-preferences";
/// A proposal nobody answered for this long is dropped; it stays a project rule.
const EXPIRY_SECS: u64 = 30 * 24 * 3600;
/// A question being asked holds its proposal this long; a process that exits without an
/// answer gives it back at once, a crashed one after this.
const ASKING_LEASE_SECS: u64 = 30 * 60;
/// At most this many questions per process, one between two tasks.
pub const ASKS_PER_RUN: usize = 3;
const DECLINED_KEPT: usize = 200;

#[derive(Clone, Debug, Deserialize, PartialEq, Eq, Serialize)]
pub struct Asking {
    /// The asking process's token.
    pub by: String,
    pub until: u64,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq, Serialize)]
pub struct Candidate {
    pub id: String,
    pub text: String,
    /// The project it was said in, for display.
    #[serde(default)]
    pub project: String,
    /// That project's rules file, where it was recorded; `None` outside a project.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub rules: Option<String>,
    /// The project rule as stored there (dated).
    #[serde(default)]
    pub rule: String,
    #[serde(default)]
    pub session: String,
    pub at: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub asking: Option<Asking>,
}

#[derive(Default, Deserialize, Serialize)]
struct Queue {
    #[serde(default)]
    version: u32,
    #[serde(default)]
    candidates: Vec<Candidate>,
    /// Normalized texts the user answered "no" to: never asked again.
    #[serde(default)]
    declined: Vec<String>,
}

/// The queue file; every change is a locked read-modify-write.
pub struct Pending {
    path: PathBuf,
}

impl Pending {
    pub fn at(path: PathBuf) -> Self {
        Self { path }
    }

    pub fn path(&self) -> &Path {
        &self.path
    }

    fn load(&self) -> Result<Queue> {
        if !self.path.is_file() {
            return Ok(Queue::default());
        }
        let mut queue: Queue = crate::persist::read_json(&self.path)?;
        let cutoff = crate::session::now().saturating_sub(EXPIRY_SECS);
        queue.candidates.retain(|candidate| candidate.at >= cutoff);
        Ok(queue)
    }

    fn change<R>(&self, change: impl FnOnce(&mut Queue) -> R) -> Result<R> {
        crate::persist::with_lock(&self.path, || {
            let mut queue = self.load()?;
            let result = change(&mut queue);
            queue.version = 1;
            if let Some(dir) = self.path.parent() {
                std::fs::create_dir_all(dir)
                    .map_err(|err| crate::persist::io_context("create", dir, err))?;
            }
            crate::persist::atomic_json(&self.path, &queue)?;
            Ok(result)
        })
    }

    /// Queues `candidate`; `false` when the same text is queued or was declined.
    pub fn push(&self, candidate: Candidate) -> Result<bool> {
        let key = normalize(&candidate.text);
        self.change(|queue| {
            if queue.declined.contains(&key)
                || queue
                    .candidates
                    .iter()
                    .any(|queued| normalize(&queued.text) == key)
            {
                return false;
            }
            queue.candidates.push(candidate);
            true
        })
    }

    pub fn is_declined(&self, text: &str) -> bool {
        let key = normalize(undated(text));
        self.load().is_ok_and(|queue| queue.declined.contains(&key))
    }

    /// The proposals waiting, oldest first (expired ones dropped).
    pub fn list(&self) -> Result<Vec<Candidate>> {
        Ok(self.load()?.candidates)
    }

    /// The oldest proposal nobody is asking about and not in `skip`, marked as asked by
    /// `by` (so another window does not ask the same); `None` when there is none.
    pub fn claim(&self, by: &str, now: u64, skip: &[String]) -> Result<Option<Candidate>> {
        self.change(|queue| {
            let free = queue.candidates.iter_mut().find(|candidate| {
                !skip.contains(&candidate.id)
                    && candidate
                        .asking
                        .as_ref()
                        .is_none_or(|asking| asking.until < now)
            })?;
            free.asking = Some(Asking {
                by: by.to_owned(),
                until: now + ASKING_LEASE_SECS,
            });
            Some(free.clone())
        })
    }

    /// Makes proposal `id` askable again ("later", Esc).
    pub fn release(&self, id: &str) -> Result<()> {
        self.change(|queue| {
            for candidate in &mut queue.candidates {
                if candidate.id == id {
                    candidate.asking = None;
                }
            }
        })
    }

    /// Gives back every proposal `by` was asking about (the process is exiting).
    pub fn release_all(&self, by: &str) -> Result<()> {
        if !self.path.is_file() {
            return Ok(());
        }
        self.change(|queue| {
            for candidate in &mut queue.candidates {
                if candidate
                    .asking
                    .as_ref()
                    .is_some_and(|asking| asking.by == by)
                {
                    candidate.asking = None;
                }
            }
        })
    }

    /// Takes proposal `id` out of the queue (accepted).
    pub fn resolve(&self, id: &str) -> Result<Option<Candidate>> {
        self.change(|queue| {
            let index = queue.candidates.iter().position(|c| c.id == id)?;
            Some(queue.candidates.remove(index))
        })
    }

    /// Takes proposal `id` out and remembers its text as declined.
    pub fn decline(&self, id: &str) -> Result<Option<Candidate>> {
        self.change(|queue| {
            let index = queue.candidates.iter().position(|c| c.id == id)?;
            let candidate = queue.candidates.remove(index);
            let key = normalize(&candidate.text);
            if !queue.declined.contains(&key) {
                queue.declined.push(key);
            }
            let excess = queue.declined.len().saturating_sub(DECLINED_KEPT);
            queue.declined.drain(..excess);
            Some(candidate)
        })
    }
}

/// What became of one proposal from an extraction.
#[derive(Debug, PartialEq, Eq)]
pub enum Proposal {
    /// Already a preference (or empty, or refused by the secret screen).
    Dropped,
    /// Recorded as a project rule only: names this project, too long, or declined before.
    ProjectRule,
    /// Recorded as a project rule (when in a project) and queued for the question.
    Queued,
}

/// The user's preferences and the proposals waiting for an answer.
pub struct Preferences {
    pub file: Arc<RulesFile>,
    pub pending: Pending,
    /// This process, in the queue's "being asked" marks.
    token: String,
    asked: AtomicUsize,
    /// A question was asked since the last task ran: no other before the next one.
    asked_since_task: AtomicBool,
    queued: AtomicUsize,
    /// Asked in this run and answered "later": not asked again before the next run.
    postponed: Mutex<Vec<String>>,
}

impl Preferences {
    pub fn open(data: &Path) -> Arc<Self> {
        Arc::new(Self {
            file: Arc::new(RulesFile::preferences(data)),
            pending: Pending::at(data.join(PENDING_FILE)),
            token: crate::session::new_id(),
            asked: AtomicUsize::new(0),
            asked_since_task: AtomicBool::new(false),
            queued: AtomicUsize::new(0),
            postponed: Mutex::default(),
        })
    }

    /// Handles one proposed preference `text` said in the project of `rules` (`None`
    /// outside a project). `markers`: names that tie a text to this project
    /// ([`workspace_markers`]).
    pub fn propose(
        &self,
        text: &str,
        rules: Option<&RulesFile>,
        markers: &[String],
        session: &str,
        project: &str,
    ) -> Proposal {
        let text = crate::text::one_line(undated(text));
        if text.is_empty()
            || crate::memory::store::screen(&text).is_err()
            || self.file.contains(&text)
        {
            return Proposal::Dropped;
        }
        if project_specific(&text, markers)
            || text.chars().count() > MAX_PREFERENCE_CHARS
            || self.pending.is_declined(&text)
        {
            // An ordinary project rule, announced like the others.
            return match rules.map(|rules| rules.add(&text)) {
                Some(Ok(_)) => Proposal::ProjectRule,
                _ => Proposal::Dropped,
            };
        }
        // Quiet: the question tells the user it is a project rule already.
        let rule = rules
            .and_then(|rules| rules.add_quietly(&text).ok())
            .flatten()
            .unwrap_or_else(|| text.clone());
        let candidate = Candidate {
            id: crate::session::new_id(),
            text,
            project: project.to_owned(),
            rules: rules.map(|rules| rules.path().to_string_lossy().into_owned()),
            rule,
            session: session.to_owned(),
            at: crate::session::now(),
            asking: None,
        };
        match self.pending.push(candidate) {
            Ok(true) => {
                self.queued.fetch_add(1, Ordering::Relaxed);
                Proposal::Queued
            }
            Ok(false) => Proposal::Dropped,
            Err(_) => Proposal::Dropped,
        }
    }

    /// The next proposal to ask about before a prompt, if this is a moment to ask: at
    /// most one between two tasks ([`Self::task_done`]).
    pub fn next_question(&self, terminal: bool, typing: bool) -> Option<Candidate> {
        if !should_ask(terminal, false, typing, self.asked.load(Ordering::Relaxed))
            || self.asked_since_task.load(Ordering::Relaxed)
        {
            return None;
        }
        let postponed = self
            .postponed
            .lock()
            .map(|ids| ids.clone())
            .unwrap_or_default();
        let candidate = self.claim(&postponed)?;
        self.asked.fetch_add(1, Ordering::Relaxed);
        self.asked_since_task.store(true, Ordering::Relaxed);
        Some(candidate)
    }

    /// A task ran: the next prompt may ask again.
    pub fn task_done(&self) {
        self.asked_since_task.store(false, Ordering::Relaxed);
    }

    /// The next waiting proposal not in `skip`, regardless of the per-run limit
    /// (`/rules pending`).
    pub fn claim(&self, skip: &[String]) -> Option<Candidate> {
        self.pending
            .claim(&self.token, crate::session::now(), skip)
            .ok()
            .flatten()
    }

    /// "Yes": the preference is written, the proposal leaves the queue and the project
    /// rule it was recorded as is removed (the preference covers it). A full
    /// preferences file leaves everything as it was.
    pub fn accept(&self, candidate: &Candidate) -> Result<String> {
        if let Err(err) = self.file.add_by_user(&candidate.text) {
            self.later(candidate);
            return Err(err);
        }
        self.pending.resolve(&candidate.id)?;
        let removed = match &candidate.rules {
            Some(path) => RulesFile::at(Kind::Project, PathBuf::from(path))
                .remove_matching(&candidate.rule)
                .unwrap_or(false),
            None => false,
        };
        let mut text = format!(
            "已记成全局偏好（{}），所有项目都会遵守",
            crate::paths::display(self.file.path())
        );
        if removed {
            text.push_str("；项目规则里的同一条已删掉");
        }
        text.push_str("。/rules 查看或删除。\n");
        Ok(text)
    }

    /// "No": it stays a project rule and is never asked again.
    pub fn decline(&self, candidate: &Candidate) -> Result<()> {
        self.pending.decline(&candidate.id).map(|_| ())
    }

    /// No answer: asked again in a later run (or with `/rules pending`).
    pub fn later(&self, candidate: &Candidate) {
        if let Ok(mut postponed) = self.postponed.lock() {
            postponed.push(candidate.id.clone());
        }
        let _ = self.pending.release(&candidate.id);
    }

    /// Proposals this process queued.
    pub fn queued(&self) -> usize {
        self.queued.load(Ordering::Relaxed)
    }

    /// Gives back the questions this process did not get answered.
    pub fn shutdown(&self) {
        let _ = self.pending.release_all(&self.token);
    }
}

/// Whether to ask a preference question now: in an interactive terminal, between
/// tasks, before the user started typing, at most [`ASKS_PER_RUN`] times a run.
pub fn should_ask(terminal: bool, task_running: bool, typing: bool, asked: usize) -> bool {
    terminal && !task_running && !typing && asked < ASKS_PER_RUN
}

/// The question for `candidate`, asked in the project shown as `here`; the trailing
/// `[y/n]` makes the console take `y` and `n` as single keys.
pub fn question(candidate: &Candidate, here: &str) -> String {
    let from = if candidate.project.is_empty() || candidate.project == here {
        String::new()
    } else {
        format!(
            "（在 {} 里说的）",
            crate::text::visible_row(&candidate.project)
        )
    };
    format!(
        "[偏好] 这条要求看起来不只适用于这个项目{from}：\n  {}\n在所有项目里都这样做吗？ y 记成全局偏好 · n 只留在本项目 · 回车 以后再说 [y/n]",
        crate::text::visible_row(&candidate.text)
    )
}

/// An answer to [`question`].
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Reply {
    Yes,
    No,
    /// Enter: ask again in a later run.
    Later,
    /// Other text, most likely a task typed without seeing the question: later for
    /// the question, and the text runs as the next task.
    Task(String),
}

/// What `text`, typed at [`question`], means.
pub fn answer(text: &str) -> Reply {
    let text = text.trim();
    match text.to_lowercase().as_str() {
        "y" | "yes" | "是" | "好" | "要" => Reply::Yes,
        "n" | "no" | "否" | "不" | "不要" => Reply::No,
        "" => Reply::Later,
        _ => Reply::Task(text.to_owned()),
    }
}

/// The line `-p` prints at exit when it queued proposals.
pub fn unattended_notice(queued: usize) -> Option<String> {
    (queued > 0).then(|| {
        format!(
            "[偏好] 有 {queued} 条候选等待确认，下次交互运行 cove 时会询问；或用 /rules pending 查看\n"
        )
    })
}

/// Names that tie a text to the project at `cwd`: the directory names of `cwd` and its
/// git root, and the repository name of the `origin` remote. Short names (under three
/// characters) are left out; they would match ordinary words.
pub fn workspace_markers(cwd: &Path) -> Vec<String> {
    let mut names = Vec::new();
    let mut add = |name: &str| {
        let name = name.trim().to_lowercase();
        if name.chars().count() >= 3 && !names.contains(&name) {
            names.push(name);
        }
    };
    if let Some(name) = cwd.file_name().and_then(|name| name.to_str()) {
        add(name);
    }
    if let Some(root) = crate::engine::prompt::git_root(cwd)
        && let Some(name) = root.file_name().and_then(|name| name.to_str())
    {
        add(name);
    }
    if let Some(url) = crate::engine::prompt::git(cwd, &["remote", "get-url", "origin"]) {
        let last = url
            .trim()
            .trim_end_matches('/')
            .rsplit(['/', ':', '\\'])
            .next()
            .unwrap_or_default();
        add(last.strip_suffix(".git").unwrap_or(last));
    }
    names
}

/// Whether `text` names this project (one of `markers`) or an absolute path.
pub fn project_specific(text: &str, markers: &[String]) -> bool {
    static PATH: std::sync::OnceLock<regex::Regex> = std::sync::OnceLock::new();
    let path = PATH.get_or_init(|| {
        regex::Regex::new(r#"\b[A-Za-z]:[\\/]|(?:^|[\s(`"'=])(?:~|/[\w.-]+)/[\w.-]+"#)
            .expect("valid path pattern")
    });
    let lower = text.to_lowercase();
    markers.iter().any(|marker| lower.contains(marker.as_str())) || path.is_match(text)
}

/// Whether a global memory's title says it is the user's language preference.
fn is_language_preference(title: &str) -> bool {
    let title = title.to_lowercase();
    let language = ["语言", "language", "中文", "chinese", "英文", "english"];
    let preference = [
        "偏好",
        "preference",
        "prefer",
        "习惯",
        "回复",
        "reply",
        "respon",
        "交流",
        "communicat",
    ];
    language.iter().any(|word| title.contains(word))
        && preference.iter().any(|word| title.contains(word))
}

/// The one-time move of the language preference from global memory into the
/// preferences file: each matching entry is copied to `<data>/backups/0030-global-
/// preferences/` first, then written as a preference, then deleted. Runs once per data
/// directory (a `migrated` file in the backup directory marks it). Returns a notice
/// for the user when something was moved or left behind.
pub fn migrate_language(
    data: &Path,
    memory: &Memory,
    preferences: &RulesFile,
) -> Result<Option<String>> {
    let backup = data.join(MIGRATION_BACKUP);
    let marker = backup.join("migrated");
    if marker.exists() {
        return Ok(None);
    }
    let entries = memory.entries(Scope::Global)?;
    let mut moved = Vec::new();
    let mut kept = Vec::new();
    for entry in entries
        .iter()
        .filter(|entry| is_language_preference(&entry.title))
    {
        let text = crate::text::one_line(&entry.content);
        if text.is_empty() || text.chars().count() > MAX_PREFERENCE_CHARS {
            kept.push(entry.title.clone());
            continue;
        }
        let source = memory
            .directory(Scope::Global)
            .join(format!("{}.json", entry.id));
        let original = std::fs::read(&source)
            .map_err(|err| crate::persist::io_context("read", &source, err))?;
        std::fs::create_dir_all(&backup)
            .map_err(|err| crate::persist::io_context("create", &backup, err))?;
        crate::persist::write_atomic(&backup.join(format!("{}.json", entry.id)), &original, true)?;
        if let Err(err) = preferences.add_by_user(&text) {
            kept.push(format!("{}（{err}）", entry.title));
            continue;
        }
        memory.remove(Scope::Global, &entry.id)?;
        moved.push(entry.title.clone());
    }
    std::fs::create_dir_all(&backup)
        .map_err(|err| crate::persist::io_context("create", &backup, err))?;
    let record = format!(
        "0030: global memories moved into {}: {}; left in place: {}\n",
        crate::memory::rules::PREFERENCES_FILE,
        if moved.is_empty() {
            "none".to_owned()
        } else {
            moved.join(", ")
        },
        if kept.is_empty() {
            "none".to_owned()
        } else {
            kept.join(", ")
        },
    );
    crate::persist::write_atomic(&marker, record.as_bytes(), false)?;
    let mut notice = String::new();
    if !moved.is_empty() {
        notice.push_str(&format!(
            "[偏好] 已把全局记忆里的语言偏好（{}）移到全局偏好 {}，原条目备份在 {}；/rules 查看。\n",
            crate::text::visible_row(&moved.join("、")),
            crate::paths::display(preferences.path()),
            crate::paths::display(&backup)
        ));
    }
    if !kept.is_empty() {
        notice.push_str(&format!(
            "[偏好] 全局记忆 {} 太长或写不进全局偏好，没有移动；需要时用 /rules add global <文字> 自己写一条。\n",
            crate::text::visible_row(&kept.join("、"))
        ));
    }
    Ok((!notice.is_empty()).then_some(notice))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::safety::permission::{LocalAuthorizer, Mode};

    fn candidate(text: &str, at: u64) -> Candidate {
        Candidate {
            id: crate::session::new_id(),
            text: text.into(),
            project: "p".into(),
            rules: None,
            rule: text.into(),
            session: "s".into(),
            at,
            asking: None,
        }
    }

    #[test]
    fn the_queue_keeps_dedupes_expires_and_remembers_declines() {
        let data = tempfile::tempdir().unwrap();
        let pending = Pending::at(data.path().join(PENDING_FILE));
        let now = crate::session::now();
        assert!(pending.list().unwrap().is_empty());
        assert!(pending.push(candidate("改动尽量小", now)).unwrap());
        assert!(!pending.push(candidate("  改动尽量小 ", now)).unwrap());
        assert!(
            pending
                .push(candidate("old one", now - EXPIRY_SECS - 10))
                .unwrap()
        );
        // Another process sees the same queue; the expired one is gone.
        let other = Pending::at(data.path().join(PENDING_FILE));
        let listed = other.list().unwrap();
        assert_eq!(listed.len(), 1, "{listed:?}");
        let first = other.claim("b", now, &[]).unwrap().unwrap();
        assert_eq!(first.text, "改动尽量小");
        other.decline(&first.id).unwrap();
        assert!(pending.is_declined("改动尽量小（2026-01-01）"));
        assert!(!pending.push(candidate("改动尽量小", now)).unwrap());
        assert!(pending.list().unwrap().is_empty());
        assert!(pending.push(candidate("先审查再推送", now)).unwrap());
        let taken = pending.claim("a", now, &[]).unwrap().unwrap();
        assert!(pending.resolve(&taken.id).unwrap().is_some());
        assert!(pending.list().unwrap().is_empty());
    }

    #[test]
    fn a_question_being_asked_is_not_asked_twice_and_comes_back_after_exit() {
        let data = tempfile::tempdir().unwrap();
        let pending = Pending::at(data.path().join(PENDING_FILE));
        let now = crate::session::now();
        pending
            .push(candidate("查官方文档，不要推断", now))
            .unwrap();
        let asked = pending.claim("a", now, &[]).unwrap().unwrap();
        assert_eq!(asked.asking.as_ref().unwrap().by, "a");
        assert!(pending.claim("b", now, &[]).unwrap().is_none());
        // Process a exits without an answer.
        pending.release_all("a").unwrap();
        let again = pending.claim("b", now, &[]).unwrap().unwrap();
        assert_eq!(again.id, asked.id);
        // A process that crashed while asking: its mark lapses.
        assert!(pending.claim("c", now + 1, &[]).unwrap().is_none());
        assert!(
            pending
                .claim("c", now + ASKING_LEASE_SECS + 1, &[])
                .unwrap()
                .is_some()
        );
        pending.release(&asked.id).unwrap();
        assert!(pending.claim("d", now, &[]).unwrap().is_some());
    }

    #[test]
    fn questions_wait_for_an_idle_terminal_and_stop_after_three() {
        assert!(should_ask(true, false, false, 0));
        assert!(should_ask(true, false, false, ASKS_PER_RUN - 1));
        assert!(!should_ask(true, false, false, ASKS_PER_RUN));
        assert!(!should_ask(false, false, false, 0), "no terminal");
        assert!(!should_ask(true, true, false, 0), "a task is running");
        assert!(!should_ask(true, false, true, 0), "the user is typing");
        assert_eq!(answer(" Y "), Reply::Yes);
        assert_eq!(answer("n"), Reply::No);
        assert_eq!(answer("  "), Reply::Later);
        assert_eq!(
            answer(" 运行一下 echo "),
            Reply::Task("运行一下 echo".into())
        );
        assert!(unattended_notice(0).is_none());
        assert!(
            unattended_notice(1)
                .unwrap()
                .contains("有 1 条候选等待确认")
        );
    }

    #[test]
    fn proposals_become_project_rules_and_wait_for_a_yes() {
        let data = tempfile::tempdir().unwrap();
        let root = tempfile::tempdir().unwrap();
        let preferences = Preferences::open(data.path());
        let rules = RulesFile::at(Kind::Project, root.path().join(crate::memory::rules::FILE));
        let markers = vec!["quotehub".to_owned()];
        assert_eq!(
            preferences.propose("改动尽量小，功能要完整", Some(&rules), &markers, "s", "p"),
            Proposal::Queued
        );
        // Recorded in the project at once, without a separate notice.
        assert_eq!(rules.list().unwrap().len(), 1);
        assert!(rules.take_unseen().is_empty());
        // Names the project or a path: a project rule, announced, not queued.
        assert_eq!(
            preferences.propose(
                "QuoteHub 的文档放 .local/",
                Some(&rules),
                &markers,
                "s",
                "p"
            ),
            Proposal::ProjectRule
        );
        assert_eq!(
            preferences.propose("日志写到 D:\\logs\\cove", Some(&rules), &markers, "s", "p"),
            Proposal::ProjectRule
        );
        assert_eq!(rules.take_unseen().len(), 2);
        // Outside a project: only queued.
        assert_eq!(
            preferences.propose("先 AI 审查，没问题再提交推送", None, &markers, "s", "p"),
            Proposal::Queued
        );
        assert_eq!(rules.list().unwrap().len(), 3);
        assert_eq!(preferences.queued(), 2);
        assert_eq!(preferences.pending.list().unwrap().len(), 2);

        let first = preferences.next_question(true, false).unwrap();
        assert_eq!(first.text, "改动尽量小，功能要完整");
        assert!(question(&first, "p").contains("改动尽量小，功能要完整"));
        assert!(question(&first, "other").contains("在 p 里说的"));
        let said = preferences.accept(&first).unwrap();
        assert!(said.contains("项目规则里的同一条已删掉"), "{said}");
        assert!(preferences.file.contains("改动尽量小，功能要完整"));
        assert_eq!(rules.list().unwrap().len(), 2);
        // Already a preference: dropped.
        assert_eq!(
            preferences.propose("改动尽量小，功能要完整", Some(&rules), &markers, "s", "p"),
            Proposal::Dropped
        );
        preferences.task_done();
        let second = preferences.next_question(true, false).unwrap();
        preferences.decline(&second).unwrap();
        // Declined: next time it is a plain project rule.
        assert_eq!(
            preferences.propose(
                "先 AI 审查，没问题再提交推送",
                Some(&rules),
                &markers,
                "s",
                "p"
            ),
            Proposal::ProjectRule
        );
        preferences.task_done();
        assert!(preferences.next_question(true, false).is_none());
        assert!(preferences.next_question(false, false).is_none());
    }

    #[test]
    fn at_most_one_question_between_two_tasks() {
        let data = tempfile::tempdir().unwrap();
        let preferences = Preferences::open(data.path());
        for text in ["先给方案再动手", "改动尽量小，功能要完整", "先查官方文档"]
        {
            assert_eq!(
                preferences.propose(text, None, &[], "s", "p"),
                Proposal::Queued
            );
        }
        let first = preferences.next_question(true, false).unwrap();
        preferences.accept(&first).unwrap();
        // Answered, then Enter or a command: not asked again before a task ran.
        assert!(preferences.next_question(true, false).is_none());
        // `/rules pending` still goes through all of them.
        let shown = preferences.claim(&[]).unwrap();
        preferences.later(&shown);
        preferences.task_done();
        let second = preferences.next_question(true, false).unwrap();
        assert_ne!(second.id, first.id);
        assert!(preferences.next_question(true, false).is_none());
    }

    #[test]
    fn a_question_put_off_waits_for_the_next_run() {
        let data = tempfile::tempdir().unwrap();
        let preferences = Preferences::open(data.path());
        preferences.propose("先给方案再动手", None, &[], "s", "p");
        let asked = preferences.next_question(true, false).unwrap();
        preferences.later(&asked);
        assert!(preferences.next_question(true, false).is_none());
        // `/rules pending` may still ask it; so does the next run.
        assert_eq!(preferences.claim(&[]).unwrap().id, asked.id);
        preferences.shutdown();
        let next_run = Preferences::open(data.path());
        assert_eq!(next_run.next_question(true, false).unwrap().id, asked.id);
    }

    #[test]
    fn project_names_and_absolute_paths_tie_a_text_to_the_project() {
        let markers = vec!["cove-main".to_owned()];
        assert!(project_specific("cove-main 里不要用 cargo fmt", &markers));
        assert!(project_specific("输出放到 /tmp/out 下", &[]));
        assert!(project_specific("用 ~/bin/tool 运行", &[]));
        assert!(project_specific("看 C:\\Users 下的配置", &[]));
        assert!(!project_specific("提交/推送前先 AI 审查", &[]));
        assert!(!project_specific("改动尽量小，功能要完整", &markers));
        let root = tempfile::tempdir().unwrap();
        let dir = root.path().join("my-service");
        std::fs::create_dir_all(&dir).unwrap();
        assert!(workspace_markers(&dir).contains(&"my-service".to_owned()));
    }

    #[test]
    fn the_language_preference_moves_once_with_a_backup() {
        let data = tempfile::tempdir().unwrap();
        let root = tempfile::tempdir().unwrap();
        let memory = Memory::new(
            data.path(),
            root.path(),
            Arc::new(LocalAuthorizer {
                mode: Mode::Auto,
                input: None,
            }),
        )
        .unwrap();
        let add = |title: &str, content: &str| {
            memory
                .save(
                    Scope::Global,
                    None,
                    title.into(),
                    content.into(),
                    "test".into(),
                    crate::memory::store::ProvenanceSource::default(),
                )
                .unwrap();
        };
        add("用户语言偏好", "回复一律使用简体中文");
        add("TMP 环境变量", "TMP 指向系统临时目录");
        let language = memory.entries(Scope::Global).unwrap();
        let language = language
            .iter()
            .find(|entry| entry.title == "用户语言偏好")
            .unwrap()
            .clone();
        let preferences = RulesFile::preferences(data.path());
        let notice = migrate_language(data.path(), &memory, &preferences)
            .unwrap()
            .unwrap();
        assert!(notice.contains("已把全局记忆里的语言偏好"), "{notice}");
        let listed = preferences.list().unwrap();
        assert_eq!(listed.len(), 1);
        assert!(
            listed[0].starts_with("回复一律使用简体中文（"),
            "{listed:?}"
        );
        let left: Vec<String> = memory
            .entries(Scope::Global)
            .unwrap()
            .into_iter()
            .map(|entry| entry.title)
            .collect();
        assert_eq!(left, ["TMP 环境变量"]);
        let backup = data
            .path()
            .join(MIGRATION_BACKUP)
            .join(format!("{}.json", language.id));
        let saved: crate::memory::store::Entry =
            serde_json::from_slice(&std::fs::read(backup).unwrap()).unwrap();
        assert_eq!(saved.content, "回复一律使用简体中文");
        // Once only: a language memory added later stays a memory.
        add("Language preference", "Reply in English");
        assert!(
            migrate_language(data.path(), &memory, &preferences)
                .unwrap()
                .is_none()
        );
        assert_eq!(memory.entries(Scope::Global).unwrap().len(), 2);
        assert!(is_language_preference("User language preference"));
        assert!(is_language_preference("回复语言"));
        assert!(!is_language_preference("cove 可执行文件位置"));
    }

    #[test]
    fn a_data_directory_without_a_language_memory_is_only_marked() {
        let data = tempfile::tempdir().unwrap();
        let root = tempfile::tempdir().unwrap();
        let memory = Memory::new(
            data.path(),
            root.path(),
            Arc::new(LocalAuthorizer {
                mode: Mode::Auto,
                input: None,
            }),
        )
        .unwrap();
        let preferences = RulesFile::preferences(data.path());
        assert!(
            migrate_language(data.path(), &memory, &preferences)
                .unwrap()
                .is_none()
        );
        assert!(
            data.path()
                .join(MIGRATION_BACKUP)
                .join("migrated")
                .is_file()
        );
        assert!(!preferences.path().exists());
    }
}
