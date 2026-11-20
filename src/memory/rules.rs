//! Project rules: short standing rules learned in a project (the user's corrections,
//! problems that came back in more than one session), kept in `<project>/.cove/rules.md`
//! so they travel with the repository. The file is one of the project instruction files
//! (`engine::prompt`), so every task follows it, and the reviews check changes against it.
//! The user may edit it by hand: every line starting with `- ` is a rule.

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

const HEADER: &str = "# 项目规则\n\n\
cove 在这个项目里记下的规则：你纠正过的做法，或者反复出现过的问题。\
每条以 `- ` 开头，可以直接编辑或删除。cove 每个任务都会遵守，提交前审查也会按它检查。\n\n";

/// One project's rules file and the rules this process added to it.
pub struct ProjectRules {
    path: PathBuf,
    /// Rules (normalized) the model already knows: those in its system prompt, and
    /// those announced since.
    known: Mutex<HashSet<String>>,
    /// Added in this process and not yet shown to the user.
    unseen: Mutex<Vec<String>>,
}

fn normalize(rule: &str) -> String {
    crate::text::one_line(rule).to_lowercase()
}

/// The rules in the text of a rules file.
pub fn parse(text: &str) -> Vec<String> {
    text.lines()
        .filter_map(|line| line.trim_start().strip_prefix("- "))
        .map(|rule| rule.trim().to_owned())
        .filter(|rule| !rule.is_empty())
        .collect()
}

impl ProjectRules {
    /// The rules of the project around `cwd`: its git root, else `cwd` itself; `None`
    /// for a directory too wide to be one project (home, a drive root).
    pub fn for_workspace(cwd: &Path) -> Option<Arc<Self>> {
        if crate::safety::protected::too_wide(cwd).is_some() {
            return None;
        }
        let root = crate::engine::prompt::git_root(cwd).unwrap_or_else(|| cwd.to_path_buf());
        Some(Arc::new(Self::at(root.join(FILE))))
    }

    pub fn at(path: PathBuf) -> Self {
        let known = Self::read(&path)
            .unwrap_or_default()
            .iter()
            .map(|rule| normalize(rule))
            .collect();
        Self {
            path,
            known: Mutex::new(known),
            unseen: Mutex::default(),
        }
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

    /// Rewrites the file with `rules`, keeping the user's text above the first rule.
    fn write(&self, rules: &[String]) -> Result<()> {
        let existing = std::fs::read_to_string(&self.path).unwrap_or_default();
        let head = existing
            .lines()
            .take_while(|line| !line.trim_start().starts_with("- "))
            .map(|line| format!("{line}\n"))
            .collect::<String>();
        let head = if head.trim().is_empty() {
            HEADER.to_owned()
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
        self.insert(rule, true)
    }

    /// [`add`](Self::add) for a rule the user gave: the command says so itself.
    pub fn add_by_user(&self, rule: &str) -> Result<bool> {
        self.insert(rule, false)
    }

    fn insert(&self, rule: &str, notify: bool) -> Result<bool> {
        let rule = crate::text::one_line(rule);
        if rule.is_empty() {
            return Err(Error::Tool("规则不能为空".into()));
        }
        crate::memory::store::screen(&rule)?;
        let rule = crate::text::clip_chars(&rule, MAX_CHARS);
        let key = normalize(&rule);
        crate::persist::with_lock(&self.path, || {
            let mut rules = Self::read(&self.path)?;
            if rules.iter().any(|existing| {
                let existing = normalize(existing);
                existing == key || existing.starts_with(&format!("{key}（"))
            }) {
                return Ok(false);
            }
            if rules.len() >= MAX_RULES {
                return Err(Error::Tool(format!(
                    "项目规则已有 {MAX_RULES} 条；先用 /rules remove 删掉过时的，或手动整理 {}",
                    crate::paths::display(&self.path)
                )));
            }
            let dated = format!("{rule}（{}）", chrono::Local::now().format("%Y-%m-%d"));
            rules.push(dated.clone());
            self.write(&rules)?;
            if notify && let Ok(mut unseen) = self.unseen.lock() {
                unseen.push(dated);
            }
            Ok(true)
        })
    }

    /// Removes rule `number` (1-based, as `/rules` lists them) and returns it.
    pub fn remove(&self, number: usize) -> Result<String> {
        crate::persist::with_lock(&self.path, || {
            let mut rules = Self::read(&self.path)?;
            if number == 0 || number > rules.len() {
                return Err(Error::Tool(format!(
                    "没有第 {number} 条规则；/rules 列出全部"
                )));
            }
            let removed = rules.remove(number - 1);
            self.write(&rules)?;
            Ok(removed)
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

/// Rules added after the system prompt was built reach the model with the next task.
impl TurnHook for ProjectRules {
    fn task_note<'a>(&'a self, _session: &'a Session) -> BoxFuture<'a, Option<String>> {
        Box::pin(async move {
            let rules = self.list().ok()?;
            let mut known = self.known.lock().ok()?;
            let new: Vec<&String> = rules
                .iter()
                .filter(|rule| known.insert(normalize(rule)))
                .collect();
            (!new.is_empty()).then(|| {
                format!(
                    "[project rules] Rules added to {FILE} since this session started; follow them like the other project instructions:\n{}",
                    new.iter()
                        .map(|rule| format!("- {rule}\n"))
                        .collect::<String>()
                )
            })
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rules_are_added_once_listed_removed_and_kept_by_hand_edits() {
        let dir = tempfile::tempdir().unwrap();
        let rules = ProjectRules::at(dir.path().join(FILE));
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
    }

    #[test]
    fn secrets_long_rules_and_a_full_file_are_refused_or_cut() {
        let dir = tempfile::tempdir().unwrap();
        let rules = ProjectRules::at(dir.path().join(FILE));
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

    #[tokio::test]
    async fn the_model_hears_of_rules_added_after_its_prompt_was_built() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(FILE);
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(&path, "- already in the prompt\n").unwrap();
        let rules = ProjectRules::at(path);
        let session = Session::new(dir.path().to_path_buf(), "Openai".into(), "m".into(), None);
        assert!(rules.task_note(&session).await.is_none());
        rules.add("new rule").unwrap();
        let note = rules.task_note(&session).await.unwrap();
        assert!(
            note.contains("new rule") && !note.contains("already"),
            "{note}"
        );
        assert!(rules.task_note(&session).await.is_none());
    }

    #[test]
    fn a_home_directory_has_no_project_rules() {
        let home = crate::extensions::skills::home_dir().unwrap();
        assert!(ProjectRules::for_workspace(&home).is_none());
    }
}
