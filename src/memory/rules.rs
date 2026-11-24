//! Standing rules in a Markdown file, one per `- ` line, in two places:
//!
//! - project rules (`<project>/.cove/rules.md`): short rules learned in a project (the
//!   user's corrections, problems that came back in more than one session); they travel
//!   with the repository, are one of the project instruction files (`engine::prompt`) and
//!   the reviews check changes against them;
//! - user preferences (`<data dir>/preferences.md`): how the user wants work done in every
//!   project, written only by the user (`/rules add global`, or confirming a proposal,
//!   `memory::preferences`); the system prompt puts them below the project instructions.
//!
//! The user may edit either file by hand: every line starting with `- ` is a rule.

use std::{
    collections::HashSet,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
};

use futures_util::future::BoxFuture;

use crate::{
    domain::{Error, Result},
    engine::turn::TurnHook,
    session::Session,
};

/// The rules file, relative to the project root.
pub const FILE: &str = ".cove/rules.md";
pub const MAX_RULES: usize = 60;
pub const MAX_CHARS: usize = 300;
/// The preferences file, relative to the data directory.
pub const PREFERENCES_FILE: &str = "preferences.md";
/// Preferences apply everywhere, so there are fewer and shorter ones than rules; a
/// longer one is refused rather than cut.
pub const MAX_PREFERENCES: usize = 20;
pub const MAX_PREFERENCE_CHARS: usize = 200;

const HEADER: &str = "# 项目规则\n\n\
cove 在这个项目里记下的规则：你纠正过的做法，或者反复出现过的问题。\
每条以 `- ` 开头，可以直接编辑或删除。cove 每个任务都会遵守，提交前审查也会按它检查。\n\n";

const PREFERENCES_HEADER: &str = "# 全局偏好\n\n\
你希望 cove 在所有项目里都遵守的做事方式。每条以 `- ` 开头，可以直接编辑或删除。\
项目规则、项目说明和你当前的要求优先于这里。\n\n";

/// Which of the two files.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Kind {
    Project,
    Preferences,
}

impl Kind {
    fn header(self) -> &'static str {
        match self {
            Self::Project => HEADER,
            Self::Preferences => PREFERENCES_HEADER,
        }
    }

    pub fn max_rules(self) -> usize {
        match self {
            Self::Project => MAX_RULES,
            Self::Preferences => MAX_PREFERENCES,
        }
    }

    pub fn max_chars(self) -> usize {
        match self {
            Self::Project => MAX_CHARS,
            Self::Preferences => MAX_PREFERENCE_CHARS,
        }
    }

    /// What one rule is called in messages.
    pub fn name(self) -> &'static str {
        match self {
            Self::Project => "项目规则",
            Self::Preferences => "全局偏好",
        }
    }

    fn remove_command(self) -> &'static str {
        match self {
            Self::Project => "/rules remove <n>",
            Self::Preferences => "/rules remove g<n>",
        }
    }
}

/// One rules file and the rules this process added to it.
pub struct RulesFile {
    kind: Kind,
    path: PathBuf,
    /// Rules (normalized) the model already knows: those in its system prompt, and
    /// those announced since.
    known: Mutex<HashSet<String>>,
    /// Added in this process and not yet shown to the user.
    unseen: Mutex<Vec<String>>,
}

pub(crate) fn normalize(rule: &str) -> String {
    crate::text::one_line(rule).to_lowercase()
}

/// `rule` without the `（YYYY-MM-DD）` date cove appends.
pub fn undated(rule: &str) -> &str {
    let trimmed = rule.trim_end();
    if let Some(head) = trimmed.strip_suffix('）')
        && let Some((text, date)) = head.rsplit_once('（')
        && date.len() == 10
        && date.chars().enumerate().all(|(index, c)| {
            if index == 4 || index == 7 {
                c == '-'
            } else {
                c.is_ascii_digit()
            }
        })
    {
        return text.trim_end();
    }
    trimmed
}

/// Whether the stored rule `existing` is the rule whose normalized, undated text is `key`.
fn same(existing: &str, key: &str) -> bool {
    normalize(undated(existing)) == key
}

/// The rules in the text of a rules file.
pub fn parse(text: &str) -> Vec<String> {
    text.lines()
        .filter_map(|line| line.trim_start().strip_prefix("- "))
        .map(|rule| rule.trim().to_owned())
        .filter(|rule| !rule.is_empty())
        .collect()
}

impl RulesFile {
    /// The rules of the project around `cwd`: its git root, else `cwd` itself; `None`
    /// for a directory too wide to be one project (home, a drive root).
    pub fn for_workspace(cwd: &Path) -> Option<Arc<Self>> {
        if crate::safety::protected::too_wide(cwd).is_some() {
            return None;
        }
        let root = crate::engine::prompt::git_root(cwd).unwrap_or_else(|| cwd.to_path_buf());
        Some(Arc::new(Self::at(Kind::Project, root.join(FILE))))
    }

    /// The user's preferences in the data directory `data`.
    pub fn preferences(data: &Path) -> Self {
        Self::at(Kind::Preferences, data.join(PREFERENCES_FILE))
    }

    pub fn at(kind: Kind, path: PathBuf) -> Self {
        let known = Self::read(&path)
            .unwrap_or_default()
            .iter()
            .map(|rule| normalize(rule))
            .collect();
        Self {
            kind,
            path,
            known: Mutex::new(known),
            unseen: Mutex::default(),
        }
    }

    pub fn kind(&self) -> Kind {
        self.kind
    }

    pub fn path(&self) -> &Path {
        &self.path
    }

    fn read(path: &Path) -> Result<Vec<String>> {
        match std::fs::read_to_string(path) {
            Ok(text) => Ok(parse(&text)),
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(Vec::new()),
            Err(err) => Err(crate::persist::io_context("read", path, err)),
        }
    }

    pub fn list(&self) -> Result<Vec<String>> {
        Self::read(&self.path)
    }

    /// The file as it is now, for a review; `None` when there are no rules.
    pub fn current(&self) -> Option<String> {
        let rules = self.list().ok()?;
        (!rules.is_empty()).then(|| {
            rules
                .iter()
                .map(|rule| format!("- {rule}\n"))
                .collect::<String>()
        })
    }

    /// Whether `rule` (dated or not) is already in the file.
    pub fn contains(&self, rule: &str) -> bool {
        let key = normalize(undated(rule));
        self.list()
            .is_ok_and(|rules| rules.iter().any(|existing| same(existing, &key)))
    }

    /// Rewrites the file with `rules`, keeping the user's text above the first rule.
    fn write(&self, rules: &[String]) -> Result<()> {
        let existing = std::fs::read_to_string(&self.path).unwrap_or_default();
        let head = existing
            .lines()
            .take_while(|line| !line.trim_start().starts_with("- "))
            .map(|line| format!("{line}\n"))
            .collect::<String>();
        let head = if head.trim().is_empty() {
            self.kind.header().to_owned()
        } else {
            head
        };
        let body: String = rules.iter().map(|rule| format!("- {rule}\n")).collect();
        if let Some(dir) = self.path.parent() {
            std::fs::create_dir_all(dir)
                .map_err(|err| crate::persist::io_context("create", dir, err))?;
        }
        crate::persist::write_atomic(&self.path, format!("{head}{body}").as_bytes(), false)
    }

    /// Adds a learned `rule` (extraction, dream), dated today, and queues a notice for
    /// the user; `Ok(false)` when the same rule is already there.
    pub fn add(&self, rule: &str) -> Result<bool> {
        self.insert(rule, true).map(|added| added.is_some())
    }

    /// [`add`](Self::add) for a rule the user gave, or one the caller tells the user
    /// about itself: no notice is queued. The rule as stored, when it was added.
    pub fn add_quietly(&self, rule: &str) -> Result<Option<String>> {
        self.insert(rule, false)
    }

    /// [`add_quietly`](Self::add_quietly) for a rule the user typed.
    pub fn add_by_user(&self, rule: &str) -> Result<bool> {
        self.insert(rule, false).map(|added| added.is_some())
    }

    fn insert(&self, rule: &str, notify: bool) -> Result<Option<String>> {
        let rule = crate::text::one_line(undated(rule));
        let name = self.kind.name();
        if rule.is_empty() {
            return Err(Error::Tool(format!("{name}不能为空")));
        }
        crate::memory::store::screen(&rule)?;
        let max = self.kind.max_chars();
        let rule = match self.kind {
            Kind::Project => crate::text::clip_chars(&rule, max),
            Kind::Preferences if rule.chars().count() > max => {
                return Err(Error::Tool(format!(
                    "一条{name}最多 {max} 字，这条有 {} 字；请写短一些",
                    rule.chars().count()
                )));
            }
            Kind::Preferences => rule,
        };
        let key = normalize(&rule);
        crate::persist::with_lock(&self.path, || {
            let mut rules = Self::read(&self.path)?;
            if rules.iter().any(|existing| same(existing, &key)) {
                return Ok(None);
            }
            let limit = self.kind.max_rules();
            if rules.len() >= limit {
                return Err(Error::Tool(format!(
                    "{name}已有 {limit} 条；先用 {} 删掉过时的，或手动整理 {}",
                    self.kind.remove_command(),
                    crate::paths::display(&self.path)
                )));
            }
            let dated = format!("{rule}（{}）", chrono::Local::now().format("%Y-%m-%d"));
            rules.push(dated.clone());
            self.write(&rules)?;
            if notify && let Ok(mut unseen) = self.unseen.lock() {
                unseen.push(dated.clone());
            }
            Ok(Some(dated))
        })
    }

    /// Removes rule `number` (1-based, as `/rules` lists them) and returns it.
    pub fn remove(&self, number: usize) -> Result<String> {
        crate::persist::with_lock(&self.path, || {
            let mut rules = Self::read(&self.path)?;
            if number == 0 || number > rules.len() {
                return Err(Error::Tool(format!(
                    "没有第 {number} 条{}；/rules 列出全部",
                    self.kind.name()
                )));
            }
            let removed = rules.remove(number - 1);
            self.write(&rules)?;
            Ok(removed)
        })
    }

    /// Removes the rule whose text is `rule` (dated or not); `Ok(false)` when it is
    /// not there (removed or edited by hand since).
    pub fn remove_matching(&self, rule: &str) -> Result<bool> {
        let key = normalize(undated(rule));
        crate::persist::with_lock(&self.path, || {
            let mut rules = Self::read(&self.path)?;
            let before = rules.len();
            rules.retain(|existing| !same(existing, &key));
            if rules.len() == before {
                return Ok(false);
            }
            self.write(&rules)?;
            Ok(true)
        })
    }

    /// Rules added in this process since the last call, for a one-line notice each.
    pub fn take_unseen(&self) -> Vec<String> {
        self.unseen
            .lock()
            .map(|mut unseen| std::mem::take(&mut *unseen))
            .unwrap_or_default()
    }
}

/// Moves project rule `number` into the preferences: written there first, removed from
/// the project only when that worked (a full preferences file leaves the project as it
/// was). Returns the rule.
pub fn promote(project: &RulesFile, preferences: &RulesFile, number: usize) -> Result<String> {
    crate::persist::with_lock(&project.path, || {
        let rules = RulesFile::read(&project.path)?;
        if number == 0 || number > rules.len() {
            return Err(Error::Tool(format!(
                "没有第 {number} 条项目规则；/rules 列出全部"
            )));
        }
        let rule = rules[number - 1].clone();
        preferences.add_by_user(&rule)?;
        let mut rules = rules;
        rules.remove(number - 1);
        project.write(&rules)?;
        Ok(rule)
    })
}

/// Rules added after the system prompt was built reach the model with the next task.
impl TurnHook for RulesFile {
    fn task_note<'a>(&'a self, _session: &'a Session) -> BoxFuture<'a, Option<String>> {
        Box::pin(async move {
            let rules = self.list().ok()?;
            let mut known = self.known.lock().ok()?;
            let new: Vec<&String> = rules
                .iter()
                .filter(|rule| known.insert(normalize(rule)))
                .collect();
            if new.is_empty() {
                return None;
            }
            let lead = match self.kind {
                Kind::Project => format!(
                    "[project rules] Rules added to {FILE} since this session started; follow them like the other project instructions:"
                ),
                Kind::Preferences => "[user preferences] Preferences the user added since this session started; they apply to every project, like the other user preferences (the project instructions and the user's current request win over them):".to_owned(),
            };
            Some(format!(
                "{lead}\n{}",
                new.iter()
                    .map(|rule| format!("- {rule}\n"))
                    .collect::<String>()
            ))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn project(dir: &Path) -> RulesFile {
        RulesFile::at(Kind::Project, dir.join(FILE))
    }

    #[test]
    fn rules_are_added_once_listed_removed_and_kept_by_hand_edits() {
        let dir = tempfile::tempdir().unwrap();
        let rules = project(dir.path());
        assert!(rules.list().unwrap().is_empty());
        assert!(rules.current().is_none());
        assert!(rules.add("不要直接推 staging，改为提 MR").unwrap());
        assert!(!rules.add("  不要直接推   staging，改为提 MR ").unwrap());
        assert!(rules.add("Run the tests with --release").unwrap());
        let listed = rules.list().unwrap();
        assert_eq!(listed.len(), 2);
        assert!(
            listed[0].starts_with("不要直接推 staging，改为提 MR（"),
            "{listed:?}"
        );
        assert!(rules.contains(&listed[0]) && rules.contains("run the tests with --release"));
        assert_eq!(rules.take_unseen().len(), 2);
        assert!(rules.take_unseen().is_empty());
        assert!(rules.add_by_user("a rule the user typed").unwrap());
        assert!(rules.take_unseen().is_empty());
        assert_eq!(
            rules.remove(3).unwrap().split('（').next(),
            Some("a rule the user typed")
        );

        // A hand-edited file: the user's own header and an extra rule are kept.
        let path = dir.path().join(FILE);
        let text = std::fs::read_to_string(&path).unwrap();
        assert!(text.starts_with("# 项目规则"), "{text}");
        std::fs::write(&path, "# Our rules\n\n- keep commits small\n").unwrap();
        assert!(rules.add("use LF line endings").unwrap());
        let text = std::fs::read_to_string(&path).unwrap();
        assert!(
            text.starts_with("# Our rules\n\n- keep commits small\n- use LF"),
            "{text}"
        );

        assert_eq!(rules.remove(1).unwrap(), "keep commits small");
        assert!(rules.remove(9).is_err());
        assert_eq!(rules.list().unwrap().len(), 1);
        assert!(
            rules
                .remove_matching("Use LF line endings（2020-01-01）")
                .unwrap()
        );
        assert!(!rules.remove_matching("use LF line endings").unwrap());
        assert!(rules.list().unwrap().is_empty());
    }

    #[test]
    fn secrets_long_rules_and_a_full_file_are_refused_or_cut() {
        let dir = tempfile::tempdir().unwrap();
        let rules = project(dir.path());
        assert!(
            rules
                .add("use key sk-abcdefghijklmnopqrstuvwxyz123456")
                .is_err()
        );
        assert!(rules.add("   ").is_err());
        rules.add(&"长".repeat(1000)).unwrap();
        let first = &rules.list().unwrap()[0];
        assert!(first.chars().count() <= MAX_CHARS + 14, "{}", first.len());
        for n in 1..MAX_RULES {
            rules.add(&format!("rule {n}")).unwrap();
        }
        assert!(rules.add("one too many").is_err());
    }

    #[test]
    fn preferences_have_their_own_header_and_stricter_limits() {
        let data = tempfile::tempdir().unwrap();
        let preferences = RulesFile::preferences(data.path());
        assert_eq!(preferences.path(), data.path().join(PREFERENCES_FILE));
        assert!(preferences.add_by_user("改动尽量小，功能要完整").unwrap());
        assert!(
            !preferences
                .add_by_user("改动尽量小，功能要完整（2026-01-02）")
                .unwrap()
        );
        let text = std::fs::read_to_string(preferences.path()).unwrap();
        assert!(text.starts_with("# 全局偏好"), "{text}");
        assert!(
            preferences
                .add_by_user("token ghp_abcdefghijklmnopqrstuvwxyz0123456789")
                .is_err()
        );
        // Too long is refused, not cut.
        let long = "长".repeat(MAX_PREFERENCE_CHARS + 1);
        let refused = preferences.add_by_user(&long).unwrap_err().to_string();
        assert!(refused.contains("最多 200 字"), "{refused}");
        assert!(
            preferences
                .add_by_user(&"长".repeat(MAX_PREFERENCE_CHARS))
                .unwrap()
        );
        for n in 3..=MAX_PREFERENCES {
            preferences.add_by_user(&format!("preference {n}")).unwrap();
        }
        let full = preferences
            .add_by_user("one too many")
            .unwrap_err()
            .to_string();
        assert!(full.contains("/rules remove g<n>"), "{full}");
        // A hand-written header is kept.
        std::fs::write(preferences.path(), "# mine\n\n- a\n").unwrap();
        preferences.add_by_user("b").unwrap();
        let text = std::fs::read_to_string(preferences.path()).unwrap();
        assert!(text.starts_with("# mine\n\n- a\n- b（"), "{text}");
    }

    #[test]
    fn promoting_writes_the_preference_before_removing_the_rule() {
        let dir = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        let rules = project(dir.path());
        let preferences = RulesFile::preferences(data.path());
        rules.add_by_user("先审查再推送").unwrap();
        rules.add_by_user("只在本项目用 tabs").unwrap();
        assert!(promote(&rules, &preferences, 3).is_err());
        let promoted = promote(&rules, &preferences, 1).unwrap();
        assert!(promoted.starts_with("先审查再推送"));
        assert_eq!(rules.list().unwrap().len(), 1);
        let listed = preferences.list().unwrap();
        assert_eq!(listed.len(), 1);
        assert!(listed[0].starts_with("先审查再推送（"), "{listed:?}");
        // A full preferences file: the project rule stays.
        for n in 1..MAX_PREFERENCES {
            preferences.add_by_user(&format!("preference {n}")).unwrap();
        }
        assert!(promote(&rules, &preferences, 1).is_err());
        assert_eq!(rules.list().unwrap().len(), 1);
    }

    #[test]
    fn dates_are_recognized_only_in_cove_s_own_form() {
        assert_eq!(undated("a（2026-11-24）"), "a");
        assert_eq!(undated("a （2026-11-24） "), "a");
        assert_eq!(undated("a（见 README）"), "a（见 README）");
        assert_eq!(undated("a"), "a");
    }

    #[tokio::test]
    async fn the_model_hears_of_rules_added_after_its_prompt_was_built() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(FILE);
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(&path, "- already in the prompt\n").unwrap();
        let rules = RulesFile::at(Kind::Project, path);
        let session = Session::new(dir.path().to_path_buf(), "Openai".into(), "m".into(), None);
        assert!(rules.task_note(&session).await.is_none());
        rules.add("new rule").unwrap();
        let note = rules.task_note(&session).await.unwrap();
        assert!(
            note.contains("new rule") && !note.contains("already"),
            "{note}"
        );
        assert!(rules.task_note(&session).await.is_none());
        let preferences = RulesFile::preferences(dir.path());
        preferences.add_by_user("reply in Chinese").unwrap();
        let note = preferences.task_note(&session).await.unwrap();
        assert!(
            note.starts_with("[user preferences]") && note.contains("reply in Chinese"),
            "{note}"
        );
    }

    #[test]
    fn a_home_directory_has_no_project_rules() {
        let home = crate::extensions::skills::home_dir().unwrap();
        assert!(RulesFile::for_workspace(&home).is_none());
    }
}
