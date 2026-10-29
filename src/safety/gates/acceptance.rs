//! Acceptance report per task: the checks that ran with their evidence, the verification
//! outcome and the git work state, saved as `<data_dir>/acceptance/<session>.json`.
//! It records what happened; it is not the model's claim of completion.

use std::{
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::Duration,
};

use futures_util::future::BoxFuture;
use serde::{Deserialize, Serialize};

use super::{CheckRecord, Evidence, GitEvent};
use crate::{engine::turn::TurnHook, session::Session};

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct GitState {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub head: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub upstream: Option<String>,
    /// Files with uncommitted changes (`git status --porcelain`).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub uncommitted_files: Option<usize>,
    /// Commits not on the upstream branch.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub unpushed_commits: Option<u64>,
    #[serde(default)]
    pub events: Vec<GitEvent>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Report {
    pub version: u32,
    pub session_id: String,
    pub cwd: PathBuf,
    pub request: String,
    pub started_at: u64,
    pub finished_at: u64,
    /// The task ended normally (not an error, limit or cancellation).
    pub completed: bool,
    /// passed, failed, not_run, or absent when no automatic verification applied.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub verification: Option<String>,
    pub checks: Vec<CheckRecord>,
    pub git: GitState,
}

pub fn path(data_dir: &Path, session_id: &str) -> PathBuf {
    data_dir
        .join("acceptance")
        .join(format!("{session_id}.json"))
}

pub fn load(data_dir: &Path, session_id: &str) -> Option<Report> {
    crate::session::read_json(&path(data_dir, session_id)).ok()
}

pub fn summary(report: &Report) -> String {
    let mut out = format!(
        "Acceptance report (session {}):\nrequest: {}\ntask: {}\nverification: {}\n",
        report.session_id,
        report
            .request
            .lines()
            .next()
            .unwrap_or_default()
            .chars()
            .take(200)
            .collect::<String>(),
        if report.completed {
            "ended normally"
        } else {
            "stopped (error, limit or cancellation)"
        },
        report.verification.as_deref().unwrap_or("none applied")
    );
    if report.checks.is_empty() {
        out.push_str("checks: none ran\n");
    }
    for check in &report.checks {
        out.push_str(&format!(
            "  {:<9} {}{}\n",
            check.status,
            check.command,
            check
                .exit_code
                .map(|code| format!(" (exit {code})"))
                .unwrap_or_default()
        ));
    }
    let git = &report.git;
    match &git.head {
        None => out.push_str("git: not a repository or git unavailable\n"),
        Some(head) => {
            out.push_str(&format!(
                "git: HEAD {} | uncommitted files: {} | unpushed commits: {}\n",
                &head[..head.len().min(12)],
                git.uncommitted_files.map_or("?".into(), |n| n.to_string()),
                match (&git.upstream, git.unpushed_commits) {
                    (Some(upstream), Some(n)) => format!("{n} (vs {upstream})"),
                    _ => "no upstream".into(),
                }
            ));
        }
    }
    for event in &git.events {
        out.push_str(&format!(
            "  {} {}: {}\n",
            event.action,
            if event.succeeded {
                "succeeded"
            } else {
                "FAILED"
            },
            event.command
        ));
    }
    out
}

async fn git(root: &Path, args: &[&str]) -> Option<String> {
    let output = tokio::time::timeout(
        Duration::from_secs(10),
        tokio::process::Command::new("git")
            .args(args)
            .current_dir(root)
            .kill_on_drop(true)
            .output(),
    )
    .await
    .ok()?
    .ok()?;
    output
        .status
        .success()
        .then(|| String::from_utf8_lossy(&output.stdout).trim().to_owned())
}

pub async fn git_state(root: &Path) -> GitState {
    let head = git(root, &["rev-parse", "HEAD"]).await;
    if head.is_none() {
        return GitState::default();
    }
    let uncommitted_files = git(root, &["status", "--porcelain"])
        .await
        .map(|s| s.lines().count());
    let upstream = git(
        root,
        &["rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"],
    )
    .await;
    let unpushed_commits = match upstream {
        Some(_) => git(root, &["rev-list", "--count", "@{u}..HEAD"])
            .await
            .and_then(|n| n.parse().ok()),
        None => None,
    };
    GitState {
        head,
        upstream,
        uncommitted_files,
        unpushed_commits,
        events: Vec::new(),
    }
}

pub struct Acceptance {
    data_dir: PathBuf,
    root: PathBuf,
    evidence: Arc<Evidence>,
    started: Mutex<Option<u64>>,
}

impl Acceptance {
    pub fn new(data_dir: PathBuf, root: PathBuf, evidence: Arc<Evidence>) -> Self {
        Self {
            data_dir,
            root,
            evidence,
            started: Mutex::new(None),
        }
    }
}

impl TurnHook for Acceptance {
    fn task_note<'a>(&'a self, _session: &'a Session) -> BoxFuture<'a, Option<String>> {
        if let Ok(mut started) = self.started.lock() {
            *started = Some(crate::session::now());
        }
        Box::pin(async { None })
    }

    fn task_end<'a>(&'a self, session: &'a Session, completed: bool) -> BoxFuture<'a, ()> {
        Box::pin(async move {
            let (checks, events, verification) = self.evidence.snapshot();
            let started_at = self
                .started
                .lock()
                .ok()
                .and_then(|s| *s)
                .unwrap_or_else(crate::session::now);
            let mut git = git_state(&self.root).await;
            git.events = events;
            let report = Report {
                version: 1,
                session_id: session.id.clone(),
                cwd: self.root.clone(),
                request: session.current_request.clone(),
                started_at,
                finished_at: crate::session::now(),
                completed,
                verification,
                checks,
                git,
            };
            let target = path(&self.data_dir, &session.id);
            if let Some(parent) = target.parent() {
                let _ = std::fs::create_dir_all(parent);
            }
            // A failed report must not fail the task it describes.
            let _ = crate::session::atomic_json(&target, &report);
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn report_is_saved_and_summarized() {
        let data = tempfile::tempdir().unwrap();
        let root = tempfile::tempdir().unwrap();
        let evidence = Arc::new(Evidence::default());
        evidence.record_check(CheckRecord {
            command: "cargo test".into(),
            status: "passed".into(),
            exit_code: Some(0),
            duration_ms: 5,
            evidence: String::new(),
        });
        evidence.set_outcome("passed");
        let hook = Acceptance::new(data.path().into(), root.path().into(), evidence);
        let mut session = Session::new(root.path().into(), String::new(), String::new(), None);
        session.current_request = "fix the bug".into();
        hook.task_note(&session).await;
        hook.task_end(&session, true).await;
        let report = load(data.path(), &session.id).unwrap();
        assert_eq!(report.verification.as_deref(), Some("passed"));
        let text = summary(&report);
        assert!(
            text.contains("cargo test") && text.contains("fix the bug"),
            "{text}"
        );
    }
}
