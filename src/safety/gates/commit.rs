//! Pre-commit gate: a shell `git commit` first runs the verification checks (when there
//! are any) and the AI review of the staged change (`commit_review`), and requires the
//! staged content to match the worktree, before and after the checks.

use std::{
    path::{Path, PathBuf},
    sync::Arc,
    time::Duration,
};

use futures_util::future::BoxFuture;
use serde_json::Value;
use tokio_util::sync::CancellationToken;

use super::{
    review::{CommitReview, Verdict},
    tool_output,
};
use crate::{
    domain::{Progress, Result, ToolDefinition},
    safety::permission::shell::{self, Dialect},
    safety::verify::{CommandVerifier, Outcome},
    tools::{ToolExecutor, ToolOutput},
};

pub struct WithCommitGate {
    pub inner: Arc<dyn ToolExecutor>,
    /// The checks; `None` when there are none or `commit_gate` is off.
    pub verifier: Option<Arc<CommandVerifier>>,
    /// The AI review; `None` when `commit_review` is off.
    pub review: Option<Arc<CommitReview>>,
    pub progress: Option<Arc<Progress>>,
    pub root: PathBuf,
    /// Staged trees already judged (`git write-tree` → passed, or the failure): a retried
    /// commit of the same content does not run, or ask for, the checks again.
    pub judged:
        std::sync::Mutex<std::collections::HashMap<String, std::result::Result<(), String>>>,
}

impl WithCommitGate {
    pub fn new(
        inner: Arc<dyn ToolExecutor>,
        verifier: Option<Arc<CommandVerifier>>,
        review: Option<Arc<CommitReview>>,
        root: PathBuf,
    ) -> Self {
        Self {
            inner,
            verifier: verifier.filter(|verifier| verifier.enabled()),
            review,
            progress: None,
            root,
            judged: Default::default(),
        }
    }

    pub fn with_progress(mut self, progress: Arc<Progress>) -> Self {
        self.progress = Some(progress);
        self
    }

    async fn say(&self, text: &str) {
        if let Some(progress) = &self.progress {
            progress.line(text.to_owned(), false).await;
        }
    }

    /// The review's verdict on the staged change; a review that cannot run blocks nothing.
    async fn review(&self, review: &CommitReview, cancel: CancellationToken) -> Verdict {
        let Ok(diff) = git(
            &self.root,
            &["diff", "--cached", "--no-color", "--no-ext-diff"],
        )
        .await
        else {
            self.say("[pre-commit review] could not read the staged diff; skipped")
                .await;
            return Verdict::Skipped;
        };
        if !review.wanted(&diff) {
            return Verdict::Passed;
        }
        self.say("[pre-commit review] reviewing the staged change…")
            .await;
        let verdict = review.review("", &diff, cancel).await;
        if verdict == Verdict::Skipped {
            self.say("[pre-commit review] did not finish; skipped")
                .await;
        }
        verdict
    }
}

/// Whether any command of the line is `git ... commit`.
pub fn is_commit(command: &str) -> bool {
    super::git_actions(command).contains(&"commit")
}

const VALUE_OPTIONS: &[&str] = &[
    "-m",
    "--message",
    "-F",
    "--file",
    "--author",
    "--date",
    "--cleanup",
];
const FLAG_OPTIONS: &[&str] = &[
    "--amend",
    "--no-edit",
    "--allow-empty",
    "--allow-empty-message",
    "-s",
    "--signoff",
    "-S",
    "--gpg-sign",
    "--no-gpg-sign",
    "-v",
    "--verbose",
    "-q",
    "--quiet",
    "-n",
    "--no-verify",
];

/// A commit the gate can judge: exactly `git commit` with options that keep the staged set.
fn check_shape(command: &str) -> std::result::Result<(), String> {
    let line = shell::parse(command, Dialect::for_command(command));
    let commands: Vec<_> = line.commands().collect();
    let split = "the pre-commit gate needs the commit on its own: stage with a separate `git add`, then run `git commit -m ...` alone (no cd, chaining or push in the same command)";
    if !line.opaque.is_empty() || commands.len() != 1 || !commands[0].redirects.is_empty() {
        return Err(split.into());
    }
    let words = commands[0].argv();
    if words.len() < 2 || shell::program_name(words[0]) != "git" || words[1] != "commit" {
        return Err(split.into());
    }
    let mut index = 2;
    while index < words.len() {
        let word = words[index];
        let option = word.split('=').next().unwrap_or(word);
        if VALUE_OPTIONS.contains(&option) {
            if !word.contains('=') {
                index += 1;
                if index >= words.len() {
                    return Err(format!("commit option {word} needs a value"));
                }
            }
        } else if !FLAG_OPTIONS.contains(&word)
            && !word.starts_with("-m")
            && !word.starts_with("-F")
        {
            return Err(format!(
                "stage files with a separate `git add` first; the gate only accepts commit options that keep the staged set (not {word})"
            ));
        }
        index += 1;
    }
    Ok(())
}

async fn git(root: &Path, args: &[&str]) -> std::result::Result<String, String> {
    crate::process::git::Git::new(root)
        .args(args.iter().copied())
        .timeout(Duration::from_secs(30))
        .text(&tokio_util::sync::CancellationToken::new())
        .await
        .map_err(|err| crate::process::git::error_text(&err))
}

impl WithCommitGate {
    async fn staged_matches(&self, paths: &[String]) -> std::result::Result<(), String> {
        if paths.is_empty() {
            return Ok(());
        }
        let mut args = vec!["--literal-pathspecs", "diff", "--name-only", "--"];
        args.extend(paths.iter().map(String::as_str));
        let changed = git(&self.root, &args).await?;
        if !changed.trim().is_empty() {
            return Err(format!(
                "the staged snapshot differs from the worktree; stage again (git add) so the checked content is what gets committed: {}",
                changed.split_whitespace().collect::<Vec<_>>().join(", ")
            ));
        }
        Ok(())
    }

    async fn gate(
        &self,
        command: &str,
        cancel: CancellationToken,
    ) -> Result<std::result::Result<(), String>> {
        if self.verifier.is_none() && self.review.is_none() {
            return Ok(Ok(()));
        }
        if let Err(err) = check_shape(command) {
            return Ok(Err(err));
        }
        let staged = match git(&self.root, &["diff", "--cached", "--name-only", "-z"]).await {
            Ok(staged) => staged,
            Err(err) => return Ok(Err(err)),
        };
        let paths: Vec<String> = staged
            .split('\0')
            .filter(|p| !p.is_empty())
            .map(str::to_owned)
            .collect();
        if paths.is_empty() {
            return Ok(Ok(()));
        }
        let tree = match git(&self.root, &["write-tree"]).await {
            Ok(tree) => tree,
            Err(err) => return Ok(Err(err)),
        };
        if let Err(err) = self.staged_matches(&paths).await {
            return Ok(Err(err));
        }
        let known = self
            .judged
            .lock()
            .ok()
            .and_then(|judged| judged.get(&tree).cloned());
        match known {
            Some(Ok(())) => return Ok(Ok(())),
            Some(Err(failure)) => {
                return Ok(Err(format!(
                    "this exact staged content already failed, or the user declined, the pre-commit checks (not run or asked again); nothing was committed. Change the staged content, or tell the user the checks fail and ask how to proceed instead of retrying.\n{failure}"
                )));
            }
            None => {}
        }
        let mut verdict = Ok(());
        if let Some(verifier) = &self.verifier {
            let changed: Vec<PathBuf> = paths.iter().map(PathBuf::from).collect();
            let verification = verifier.verify_commit(&changed, cancel.clone()).await?;
            if verification.outcome != Outcome::Passed {
                verdict = Err(format!(
                    "pre-commit verification did not pass; nothing was committed.\n{}{}",
                    verification.summary, verification.detail
                ));
            }
        }
        if verdict.is_ok()
            && let Some(review) = &self.review
            && let Verdict::Issues(findings) = self.review(review, cancel).await
        {
            verdict = Err(format!(
                "the pre-commit review found issues; nothing was committed. Fix the real ones, stage and commit again. If a finding is wrong, do not commit around it: tell the user why and let them decide.\n{findings}"
            ));
        }
        // Declined checks count too: asking again on a retry is what made commits drag.
        if let Ok(mut judged) = self.judged.lock() {
            judged.insert(tree.clone(), verdict.clone());
        }
        if let Err(failure) = verdict {
            return Ok(Err(failure));
        }
        if let Err(err) = self.staged_matches(&paths).await {
            return Ok(Err(err));
        }
        match git(&self.root, &["write-tree"]).await {
            Ok(current) if current == tree => Ok(Ok(())),
            Ok(_) => Ok(Err(
                "the index changed during verification; nothing was committed".into(),
            )),
            Err(err) => Ok(Err(err)),
        }
    }
}

impl ToolExecutor for WithCommitGate {
    fn definitions(&self) -> Vec<ToolDefinition> {
        self.inner.definitions()
    }

    fn metered(&self, name: &str) -> bool {
        self.inner.metered(name)
    }

    fn concurrent(&self, name: &str) -> bool {
        self.inner.concurrent(name)
    }

    fn execute(
        &self,
        name: String,
        input: Value,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ToolOutput>> {
        Box::pin(async move {
            let command = input
                .get("command")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_owned();
            if name == "shell"
                && is_commit(&command)
                && let Err(reason) = self.gate(&command, cancel.clone()).await?
            {
                return Ok(tool_output(format!("[pre-commit gate] {reason}"), true));
            }
            self.inner.execute(name, input, cancel).await
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn the_same_staged_content_is_checked_once() {
        use crate::safety::permission::{Authorizer, LocalAuthorizer, Mode};
        let data = tempfile::tempdir().unwrap();
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        let git = |args: &[&str]| {
            std::process::Command::new("git")
                .args(args)
                .current_dir(&root)
                .output()
                .unwrap()
        };
        git(&["init", "-q"]);
        std::fs::write(root.join("a.txt"), "x\n").unwrap();
        git(&["add", "a.txt"]);
        let store = crate::session::Store::open(data.path()).unwrap();
        let workspace =
            Arc::new(crate::session::workspace::Workspace::new(root.clone(), store).unwrap());
        let authorizer: Arc<dyn Authorizer> = Arc::new(LocalAuthorizer {
            mode: Mode::Bypass,
            input: None,
        });
        // A failing check that leaves a line per run.
        let verifier = Arc::new(CommandVerifier::new(
            root.clone(),
            vec!["echo run >> runs.log; exit 1".into()],
            None,
            workspace,
            authorizer.clone(),
        ));
        let tools = crate::tools::LocalTools::new(root.clone(), authorizer).unwrap();
        let gate = WithCommitGate::new(Arc::new(tools), Some(verifier), None, root.clone());
        let mut outputs = Vec::new();
        for _ in 0..2 {
            let output = gate
                .execute(
                    "shell".into(),
                    serde_json::json!({"command": "git commit -m x"}),
                    CancellationToken::new(),
                )
                .await
                .unwrap();
            assert!(output.failed, "{}", output.content);
            outputs.push(output.content);
        }
        let runs = std::fs::read(root.join("runs.log")).unwrap();
        let runs = String::from_utf8_lossy(&runs).replace('\0', "");
        assert_eq!(runs.matches("run").count(), 1, "{runs}");
        assert!(outputs[1].contains("already failed"), "{}", outputs[1]);
    }

    /// A reviewer model that always gives `reply`, counting its calls.
    struct Reviewing {
        reply: &'static str,
        calls: std::sync::atomic::AtomicUsize,
    }

    impl crate::domain::Provider for Reviewing {
        fn stream(
            &self,
            request: crate::domain::ModelRequest,
            _events: tokio::sync::mpsc::Sender<crate::domain::Event>,
            _cancel: CancellationToken,
        ) -> BoxFuture<'_, Result<crate::domain::ModelResponse>> {
            use crate::domain::{Block, Message, ModelResponse, Origin, Role, Usage};
            self.calls.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            let asked = request.messages.iter().any(|message| {
                message.blocks.iter().any(
                    |block| matches!(block, Block::Text(text) if text.contains("+uses display")),
                )
            });
            let reply = self.reply.to_owned();
            Box::pin(async move {
                assert!(asked, "the staged diff is what gets reviewed");
                Ok(ModelResponse {
                    message: Message {
                        role: Role::Assistant,
                        blocks: vec![Block::Text(reply)],
                        origin: Origin::Unknown,
                    },
                    usage: Usage::default(),
                    complete: true,
                    usage_reported: true,
                    reported_cost: None,
                    model: None,
                })
            })
        }
    }

    #[tokio::test]
    async fn the_review_alone_gates_a_commit_and_judges_each_content_once() {
        use crate::safety::gates::review::{CommitReview, ReviewSettings, Reviewer};
        use crate::safety::permission::{Authorizer, LocalAuthorizer, Mode};
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        let git = |args: &[&str]| {
            std::process::Command::new("git")
                .args(args)
                .current_dir(&root)
                .output()
                .unwrap()
        };
        git(&["init", "-q"]);
        std::fs::write(root.join("a.txt"), "uses display\n").unwrap();
        git(&["add", "a.txt"]);
        let authorizer: Arc<dyn Authorizer> = Arc::new(LocalAuthorizer {
            mode: Mode::Bypass,
            input: None,
        });
        let tools: Arc<dyn ToolExecutor> =
            Arc::new(crate::tools::LocalTools::new(root.clone(), authorizer).unwrap());
        let gate_with = |reply: &'static str| {
            let model = Arc::new(Reviewing {
                reply,
                calls: Default::default(),
            });
            let review = Arc::new(CommitReview {
                mode: crate::safety::gates::Switch::On,
                reviewer: Reviewer {
                    provider: model.clone(),
                    tools: tools.clone(),
                    settings: ReviewSettings {
                        max_turns: 3,
                        max_output_tokens: 1000,
                        prices: None,
                        context_tokens: 100_000,
                        project_context: None,
                    },
                    ledger: Default::default(),
                    rules: None,
                },
            });
            let gate = WithCommitGate::new(tools.clone(), None, Some(review), root.clone());
            (gate, model)
        };
        async fn commit(gate: &WithCommitGate) -> ToolOutput {
            gate.execute(
                "shell".into(),
                serde_json::json!({"command": "git commit -m x"}),
                CancellationToken::new(),
            )
            .await
            .unwrap()
        }

        // No verification commands: the review still gates the commit.
        let (gate, model) = gate_with("a.txt:1 - uses display - breaks rule 1");
        let first = commit(&gate).await;
        assert!(first.failed, "{}", first.content);
        assert!(
            first.content.contains("review found issues"),
            "{}",
            first.content
        );
        assert!(first.content.contains("breaks rule 1"), "{}", first.content);
        let second = commit(&gate).await;
        assert!(
            second.content.contains("already failed"),
            "{}",
            second.content
        );
        assert_eq!(model.calls.load(std::sync::atomic::Ordering::SeqCst), 1);

        // A clean review lets the commit through to git.
        let (gate, model) = gate_with(crate::safety::gates::review::NO_ISSUES);
        let passed = commit(&gate).await;
        assert!(
            !passed.content.contains("[pre-commit gate]"),
            "{}",
            passed.content
        );
        assert_eq!(model.calls.load(std::sync::atomic::Ordering::SeqCst), 1);
    }

    #[test]
    fn commit_shapes() {
        assert!(is_commit("git add . && git commit -m x"));
        assert!(!is_commit("git status"));
        assert!(check_shape("git commit -m 'fix: x' --signoff").is_ok());
        assert!(check_shape("git commit -am x").is_err());
        assert!(check_shape("git add . && git commit -m x").is_err());
        assert!(check_shape("git commit -m x > log").is_err());
        assert!(check_shape("git commit -m").is_err());
    }
}
