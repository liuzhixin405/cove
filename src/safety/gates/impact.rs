//! The change impact at the end of a task (`change_impact`, 0031): when the model
//! finishes after file-tool edits, lists the changed symbols' callers in other files
//! for the user, and once per task hands the untested callers of a removed or
//! re-signed symbol to the model. Runs after the finish nudges and before the
//! automatic verification and the end-of-task review, which reuses the list.

use std::{
    path::Path,
    sync::{Arc, Mutex},
};

use futures_util::future::BoxFuture;
use tokio_util::sync::CancellationToken;

use super::{Evidence, Switch, review};
use crate::{
    domain::Progress,
    engine::turn::TurnHook,
    safety::impact::{self, FileChange, Impact, Limits},
    session::{Session, workspace::Workspace},
};

/// `auto` shows a change of function bodies only from this many changed lines.
pub const MIN_BODY_LINES: usize = 20;
/// The block for the user at the end of a task.
const MAX_SHOWN_LINES: usize = 12;

#[derive(Default)]
struct State {
    start_calls: u64,
    /// The workspace fingerprint last computed for.
    computed: Option<u64>,
    handed: bool,
}

pub struct ChangeImpact {
    mode: Switch,
    max_turns: u32,
    workspace: Arc<Workspace>,
    evidence: Arc<Evidence>,
    progress: Option<Arc<Progress>>,
    state: Mutex<State>,
}

/// Whether the user sees the list: `on` whenever a symbol changed; `auto` when some
/// symbol has callers elsewhere and a symbol was removed or re-signed, or the change
/// has at least [`MIN_BODY_LINES`] lines.
pub fn shown(mode: Switch, impact: &Impact, changed_lines: usize) -> bool {
    match mode {
        Switch::Off => false,
        Switch::On => !impact.symbols.is_empty(),
        Switch::Auto => impact.has_callers() && (impact.risky() || changed_lines >= MIN_BODY_LINES),
    }
}

/// The task's file-tool changes as before/after text; files over the snapshot limit
/// or unreadable are left out.
pub fn task_changes(workspace: &Workspace) -> Vec<FileChange> {
    let Ok(paths) = workspace.changed_paths() else {
        return Vec::new();
    };
    paths
        .into_iter()
        .filter_map(|relative| {
            let before = workspace.baseline(&relative).ok()?;
            let after = std::fs::read_to_string(workspace.root().join(&relative)).ok();
            Some(FileChange {
                path: relative.to_string_lossy().replace('\\', "/"),
                before,
                after,
            })
        })
        .collect()
}

impl ChangeImpact {
    pub fn new(
        mode: Switch,
        max_turns: u32,
        workspace: Arc<Workspace>,
        evidence: Arc<Evidence>,
        progress: Option<Arc<Progress>>,
    ) -> Self {
        Self {
            mode,
            max_turns,
            workspace,
            evidence,
            progress,
            state: Mutex::default(),
        }
    }

    fn root(&self) -> &Path {
        self.workspace.root()
    }
}

impl TurnHook for ChangeImpact {
    fn task_note<'a>(&'a self, session: &'a Session) -> BoxFuture<'a, Option<String>> {
        if let Ok(mut state) = self.state.lock() {
            *state = State {
                start_calls: session.accounting.model_calls,
                ..State::default()
            };
        }
        Box::pin(async { None })
    }

    fn finish_nudge<'a>(&'a self, session: &'a Session) -> BoxFuture<'a, Option<String>> {
        Box::pin(async move {
            if self.mode == Switch::Off {
                return None;
            }
            let fingerprint = self.workspace.fingerprint().ok().flatten()?;
            let (near_limit, handed) = {
                let mut state = self.state.lock().ok()?;
                if state.computed == Some(fingerprint) {
                    return None;
                }
                state.computed = Some(fingerprint);
                let used = session
                    .accounting
                    .model_calls
                    .saturating_sub(state.start_calls);
                (used + 1 >= u64::from(self.max_turns), state.handed)
            };
            let changes = task_changes(&self.workspace);
            if !changes
                .iter()
                .any(|change| crate::tools::repomap::supported(Path::new(&change.path)))
            {
                return None;
            }
            let mut found = impact::run(
                self.root().to_path_buf(),
                changes,
                Limits::TASK,
                impact::BUDGET,
                &CancellationToken::new(),
            )
            .await
            .ok()?;
            if found.symbols.is_empty() {
                return None;
            }
            found.skipped.shell = self.workspace.shell_effects();
            let lines = self
                .workspace
                .diff()
                .map(|diff| review::changed_lines(&diff))
                .unwrap_or_default();
            let notice = (!handed && !near_limit).then(|| found.notice()).flatten();
            self.evidence.set_impact(
                found.record(handed || notice.is_some()),
                found.for_reviewer(),
            );
            if shown(self.mode, &found, lines)
                && let Some(progress) = &self.progress
            {
                progress.notice(found.render(MAX_SHOWN_LINES)).await;
            }
            if notice.is_some()
                && let Ok(mut state) = self.state.lock()
            {
                state.handed = true;
            }
            notice
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    struct Fixture {
        _dirs: (tempfile::TempDir, tempfile::TempDir),
        root: std::path::PathBuf,
        workspace: Arc<Workspace>,
        evidence: Arc<Evidence>,
        session: Session,
    }

    impl Fixture {
        fn new(files: &[(&str, &str)]) -> Self {
            let data = tempfile::tempdir().unwrap();
            let dir = tempfile::tempdir().unwrap();
            let root = dir.path().canonicalize().unwrap();
            for (rel, text) in files {
                let path = root.join(rel);
                std::fs::create_dir_all(path.parent().unwrap()).unwrap();
                std::fs::write(path, text).unwrap();
            }
            let store = crate::session::Store::open(data.path()).unwrap();
            let workspace = Arc::new(Workspace::new(root.clone(), store).unwrap());
            workspace.begin().unwrap();
            Self {
                _dirs: (data, dir),
                root: root.clone(),
                workspace,
                evidence: Arc::new(Evidence::default()),
                session: Session::new(root, String::new(), String::new(), None),
            }
        }

        fn edit(&self, rel: &str, text: &str) {
            let path = self.root.join(rel);
            let old = std::fs::read_to_string(&path).ok();
            self.workspace.replace(&path, old.as_deref(), text).unwrap();
        }

        fn gate(&self, mode: Switch, max_turns: u32) -> ChangeImpact {
            ChangeImpact::new(
                mode,
                max_turns,
                self.workspace.clone(),
                self.evidence.clone(),
                None,
            )
        }
    }

    const FILES: &[(&str, &str)] = &[
        (
            "pricing.py",
            "def price(item):\n    return item\n\ndef label(item):\n    return str(item)\n",
        ),
        (
            "invoice.py",
            "from pricing import price, label\n\ndef format_invoice(x):\n    return label(x) + str(price(x))\n",
        ),
    ];

    #[tokio::test]
    async fn a_signature_change_with_an_untested_caller_goes_to_the_model_once() {
        let fx = Fixture::new(FILES);
        let gate = fx.gate(Switch::Auto, 50);
        gate.task_note(&fx.session).await;
        // No change: nothing.
        assert!(gate.finish_nudge(&fx.session).await.is_none());
        fx.edit(
            "pricing.py",
            "def price(item, currency='CNY'):\n    return (item, currency)\n\ndef label(item):\n    return str(item)\n",
        );
        let notice = gate.finish_nudge(&fx.session).await.unwrap();
        assert!(notice.starts_with("[change_impact;"), "{notice}");
        assert!(
            notice.contains("invoice.py:4 in format_invoice() uses price"),
            "{notice}"
        );
        let record = fx.evidence.impact().unwrap();
        assert!(record.handed_back && record.signature == 1, "{record:?}");
        assert!(
            fx.evidence
                .callers()
                .unwrap()
                .contains("no test refers to it")
        );
        // The same content again: nothing new; a further change is listed, not handed back.
        assert!(gate.finish_nudge(&fx.session).await.is_none());
        fx.edit(
            "pricing.py",
            "def price(item, currency='USD'):\n    return (item, currency)\n\ndef label(item):\n    return str(item)\n",
        );
        assert!(gate.finish_nudge(&fx.session).await.is_none());
        assert!(fx.evidence.impact().unwrap().handed_back);
        // A new task starts over, from a new checkpoint.
        fx.workspace.begin().unwrap();
        gate.task_note(&fx.session).await;
        assert!(gate.finish_nudge(&fx.session).await.is_none());
        fx.edit(
            "pricing.py",
            "def price(item):\n    return item\n\ndef label(item):\n    return str(item)\n",
        );
        assert!(
            gate.finish_nudge(&fx.session).await.is_some(),
            "handed back again"
        );
    }

    #[tokio::test]
    async fn body_changes_below_the_threshold_are_not_shown_and_never_handed_back() {
        let fx = Fixture::new(FILES);
        let gate = fx.gate(Switch::Auto, 50);
        gate.task_note(&fx.session).await;
        fx.edit(
            "pricing.py",
            "def price(item):\n    return item * 2\n\ndef label(item):\n    return str(item)\n",
        );
        assert!(gate.finish_nudge(&fx.session).await.is_none());
        let record = fx.evidence.impact().unwrap();
        assert_eq!((record.body, record.handed_back), (1, false));
        let found = impact::compute(
            &fx.root,
            &task_changes(&fx.workspace),
            None,
            Limits::TASK,
            std::time::Instant::now(),
        );
        let mut with_callers = found.clone();
        with_callers.callers.push(impact::Caller {
            symbol: 0,
            file: "invoice.py".into(),
            lines: vec![4],
            enclosing: None,
            enclosing_kind: "",
            tests: impact::Tested::None,
            changed: false,
        });
        assert!(!shown(Switch::Auto, &with_callers, MIN_BODY_LINES - 1));
        assert!(shown(Switch::Auto, &with_callers, MIN_BODY_LINES));
        assert!(!shown(Switch::Auto, &found, 100), "no callers");
        assert!(shown(Switch::On, &found, 1));
        assert!(!shown(Switch::Off, &with_callers, 100));
    }

    #[tokio::test]
    async fn off_and_near_the_call_limit_hand_nothing_back() {
        let fx = Fixture::new(FILES);
        let changed = "def price(item, currency='CNY'):\n    return (item, currency)\n\ndef label(item):\n    return str(item)\n";
        let off = fx.gate(Switch::Off, 50);
        off.task_note(&fx.session).await;
        fx.edit("pricing.py", changed);
        assert!(off.finish_nudge(&fx.session).await.is_none());
        assert!(fx.evidence.impact().is_none());
        let near = fx.gate(Switch::On, 1);
        near.task_note(&fx.session).await;
        assert!(near.finish_nudge(&fx.session).await.is_none());
        let record = fx.evidence.impact().unwrap();
        assert!(
            !record.handed_back && record.untested.len() == 1,
            "{record:?}"
        );
    }
}
