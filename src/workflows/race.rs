//! One request, two solutions: each lane is an editing sub-agent in its own
//! detached worktree from HEAD (lane A on the main model, lane B on the fast
//! one, or on the main one asked for another approach), then the same
//! verification commands run in both trees (exit codes only), then each patch
//! is captured. Reports and patches live under `<data>/race/<run-id>/`; nothing
//! is applied until an explicit `/changes apply race:<run>:<a|b> confirm`.
//!
//! The lanes run in this process: their spend reaches the session and the
//! ledger, and they use the same tools as the main task. They run in `auto`
//! permission mode inside their own worktree, never the user's tree.

use std::{
    path::{Path, PathBuf},
    sync::Arc,
    time::Duration,
};

use serde::{Deserialize, Serialize};
use tokio_util::sync::CancellationToken;

use crate::{
    agents::{
        background::BackgroundAuthorizer,
        subagent::{AgentHub, Exit, RunRequest},
    },
    domain::{Error, Result},
    persist::{atomic_json, read_json},
    safety::permission::Mode,
    session::{new_id, now, workspace::Workspace},
    workflows::changes::{
        self, Baseline, Changeset, Check, CheckStatus, Decision, Kind, capture_patch, run_checks,
        sha256_hex,
    },
};

/// Whole race (generation and verification) [15 minutes].
pub const TIMEOUT: Duration = Duration::from_secs(900);
/// Asked of lane B when both lanes run on the same model.
const OTHER_APPROACH: &str = "\n\nAnother agent solves the same request in parallel with the most \
    direct approach. Take a clearly different approach (another design, algorithm or place to \
    change), and say in your final answer how it differs.";

/// One way of solving the request.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Lane {
    pub id: &'static str,
    /// For the report: "main model" or "fast model".
    pub label: &'static str,
    pub model: String,
    pub prompt: String,
}

/// Lane A on `main`; lane B on `fast`, or on `main` asked for another approach.
pub fn lanes(request: &str, main: &str, fast: Option<&str>) -> [Lane; 2] {
    let a = Lane {
        id: "a",
        label: "主模型",
        model: main.to_owned(),
        prompt: request.to_owned(),
    };
    let b = match fast.filter(|fast| !fast.is_empty() && *fast != main) {
        Some(fast) => Lane {
            id: "b",
            label: "快速模型",
            model: fast.to_owned(),
            prompt: request.to_owned(),
        },
        None => Lane {
            id: "b",
            label: "主模型·换思路",
            model: main.to_owned(),
            prompt: format!("{request}{OTHER_APPROACH}"),
        },
    };
    [a, b]
}

/// The shared verification: the configured `done_verify_commands`, else the
/// detected build and test commands (as for the automatic verification gate).
/// Commands are split on whitespace and run without a shell.
pub fn verification(root: &Path, configured: &[String]) -> Vec<Vec<String>> {
    let commands = if configured.is_empty() {
        let mut found = crate::safety::verify::detect(root);
        found.extend(crate::safety::verify::test_commands(root, &[]));
        found
    } else {
        configured.to_vec()
    };
    let mut argvs: Vec<Vec<String>> = Vec::new();
    for command in commands {
        let argv: Vec<String> = command.split_whitespace().map(str::to_owned).collect();
        if !argv.is_empty() && !argvs.contains(&argv) {
            argvs.push(argv);
        }
    }
    argvs
}

/// What a lane's agent did.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Generation {
    /// `completed`, `max_iterations`, `timeout`, `interrupted`, `error`, ...
    pub exit: String,
    pub model_calls: u64,
    pub duration_ms: u64,
    pub cost_usd: Option<f64>,
    /// The agent's final answer, shortened.
    pub summary: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Candidate {
    pub id: String,
    pub label: String,
    pub model: String,
    pub prompt: String,
    pub generation: Option<Generation>,
    pub checks: Vec<Check>,
    /// Every check passed (true without checks: the report says "unverified").
    pub correct: bool,
    pub patch_sha256: String,
    pub patch_bytes: usize,
    /// Files and added/removed lines of the patch.
    #[serde(default)]
    pub files: usize,
    #[serde(default)]
    pub added: usize,
    #[serde(default)]
    pub removed: usize,
    #[serde(default)]
    pub error: Option<String>,
    #[serde(default)]
    pub cleanup: Option<String>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Status {
    Running,
    Completed,
    Timeout,
    Cancelled,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Report {
    pub version: u32,
    pub id: String,
    pub baseline: Baseline,
    pub request: String,
    pub verify: Vec<Vec<String>>,
    pub started: u64,
    pub duration_ms: u64,
    pub status: Status,
    pub candidates: Vec<Candidate>,
    #[serde(default)]
    pub selected: Option<String>,
    #[serde(default)]
    pub reviews: std::collections::BTreeMap<String, Decision>,
}

pub fn directory(data_dir: &Path) -> PathBuf {
    data_dir.join("race")
}

fn valid_run(id: &str) -> bool {
    crate::session::valid_id(id)
}

pub fn load(data_dir: &Path, id: &str) -> Result<Report> {
    if !valid_run(id) {
        return Err(Error::Tool("invalid race run ID".into()));
    }
    let mut report: Report = read_json(&directory(data_dir).join(id).join("report.json"))?;
    if report.status == Status::Running {
        // A run is only ever driven by the foreground command that started it.
        report.status = Status::Cancelled;
    }
    Ok(report)
}

fn save(data_dir: &Path, report: &Report) -> Result<()> {
    let dir = directory(data_dir).join(&report.id);
    std::fs::create_dir_all(&dir)?;
    atomic_json(&dir.join("report.json"), report)
}

pub fn list(data_dir: &Path, project: &Path) -> Result<Vec<Report>> {
    let mut reports = Vec::new();
    let Ok(entries) = std::fs::read_dir(directory(data_dir)) else {
        return Ok(reports);
    };
    let key = changes::project_key(project);
    for entry in entries.flatten() {
        let name = entry.file_name().to_string_lossy().into_owned();
        if let Ok(report) = load(data_dir, &name)
            && changes::project_key(&report.baseline.root) == key
        {
            reports.push(report);
        }
    }
    reports.sort_by_key(|report| std::cmp::Reverse(report.started));
    Ok(reports)
}

/// Every candidate of every run of this project as a changeset (`race:<run>:<a|b>`).
pub fn changesets(data_dir: &Path, project: &Path) -> Result<Vec<Changeset>> {
    let mut sets = Vec::new();
    for report in list(data_dir, project)? {
        for candidate in &report.candidates {
            let id = format!("race:{}:{}", report.id, candidate.id);
            let review = if report.selected.as_deref() == Some(candidate.id.as_str()) {
                Decision::Applied
            } else {
                report
                    .reviews
                    .get(&candidate.id)
                    .copied()
                    .unwrap_or_default()
            };
            let not_applicable = if candidate.patch_bytes == 0 {
                Some("candidate produced no patch".to_owned())
            } else if !candidate.correct {
                Some("候选方案没有通过共享验证".to_owned())
            } else if report.selected.is_some() && review != Decision::Applied {
                Some("another candidate of this run was applied".to_owned())
            } else {
                None
            };
            sets.push(Changeset {
                id,
                kind: Kind::Race,
                title: format!(
                    "{} {}：{}",
                    candidate.label,
                    candidate.model,
                    crate::text::short(&report.request, 60)
                ),
                baseline: Some(report.baseline.clone()),
                patch_sha256: candidate.patch_sha256.clone(),
                patch_bytes: candidate.patch_bytes,
                checks: candidate.checks.clone(),
                review,
                not_applicable,
                created: report.started,
                patch_path: Some(
                    directory(data_dir)
                        .join(&report.id)
                        .join(format!("{}.patch", candidate.id)),
                ),
            });
        }
    }
    Ok(sets)
}

pub fn record(data_dir: &Path, run: &str, candidate: &str, decision: Decision) -> Result<()> {
    let mut report = load(data_dir, run)?;
    if !report.candidates.iter().any(|c| c.id == candidate) {
        return Err(Error::Tool("unknown race candidate".into()));
    }
    if decision == Decision::Applied {
        report.selected = Some(candidate.into());
    }
    report.reviews.insert(candidate.into(), decision);
    save(data_dir, &report)
}

/// Files, added and removed lines of a unified diff.
fn diff_stat(patch: &[u8]) -> (usize, usize, usize) {
    let text = String::from_utf8_lossy(patch);
    let mut stat = (0, 0, 0);
    for line in text.lines() {
        if line.starts_with("diff --git ") {
            stat.0 += 1;
        } else if line.starts_with('+') && !line.starts_with("+++") {
            stat.1 += 1;
        } else if line.starts_with('-') && !line.starts_with("---") {
            stat.2 += 1;
        }
    }
    stat
}

// One lane of the race; the parameters are the lane's whole context.
#[allow(clippy::too_many_arguments)]
async fn candidate(
    hub: &Arc<AgentHub>,
    baseline: &Baseline,
    run_dir: &Path,
    lane: &Lane,
    request: &str,
    verify: &[Vec<String>],
    deadline: tokio::time::Instant,
    cancel: &CancellationToken,
) -> Candidate {
    let mut result = Candidate {
        id: lane.id.into(),
        label: lane.label.into(),
        model: lane.model.clone(),
        prompt: lane.prompt.clone(),
        generation: None,
        checks: Vec::new(),
        correct: false,
        patch_sha256: String::new(),
        patch_bytes: 0,
        files: 0,
        added: 0,
        removed: 0,
        error: None,
        cleanup: None,
    };
    let tree = run_dir.join(format!("wt-{}", lane.id));
    if let Err(err) = baseline.add_worktree(&tree, cancel).await {
        result.error = Some(err.to_string());
        result.cleanup = baseline.remove_worktree(&tree).await;
        return result;
    }
    let started = std::time::Instant::now();
    let generation = async {
        let workspace = Arc::new(Workspace::new(tree.clone(), hub.store.clone())?);
        workspace.begin()?;
        let agent_type = hub.types.get("general")?.clone();
        let run = RunRequest {
            id: hub.next_id("race"),
            kind: "race",
            agent_type,
            description: format!("race {}: {}", lane.id, crate::text::short(request, 60)),
            prompt: lane.prompt.clone(),
            model: Some(lane.model.clone()),
            authorizer: Some(Arc::new(BackgroundAuthorizer {
                mode: Mode::Auto,
                root: tree.clone(),
                queue: hub.approvals.clone(),
                agent: format!("race {}", lane.id),
                refused: Default::default(),
            })),
            detached: true,
            worktree: Some((tree.clone(), workspace)),
            claims: None,
        };
        // Leave at least a third of the remaining time for verification.
        let remaining = deadline.saturating_duration_since(tokio::time::Instant::now());
        let lane_cancel = cancel.child_token();
        let lane = hub.run(run, lane_cancel.clone());
        tokio::pin!(lane);
        let budget = tokio::time::sleep((remaining * 2 / 3).max(Duration::from_secs(1)));
        tokio::select! {
            outcome = &mut lane => Ok::<_, Error>((outcome, false)),
            () = budget => {
                // Stopped, not dropped: the run still records its spend and ends its
                // activity entry.
                lane_cancel.cancel();
                Ok((lane.await, true))
            }
        }
    }
    .await;
    let duration_ms = started.elapsed().as_millis() as u64;
    let generated = match generation {
        Ok((outcome, timed_out)) => {
            result.generation = Some(Generation {
                exit: if timed_out {
                    "timeout".into()
                } else {
                    outcome.exit.label().into()
                },
                model_calls: outcome.spent.model_calls,
                duration_ms,
                cost_usd: outcome.spent.cost,
                summary: crate::text::short(&outcome.output, 300),
            });
            if let Some(detail) = outcome.exit.detail() {
                result.error = Some(crate::text::short(detail, 300));
            }
            !timed_out && matches!(outcome.exit, Exit::Completed | Exit::Unverified)
        }
        Err(err) => {
            result.error = Some(format!("lane not started: {err}"));
            false
        }
    };
    if generated && !cancel.is_cancelled() {
        result.checks = run_checks(&tree, verify, 1, deadline, cancel).await;
        result.correct = result
            .checks
            .iter()
            .all(|check| check.status == CheckStatus::Passed);
        match capture_patch(&tree, &baseline.commit, cancel).await {
            Ok(patch) if !patch.is_empty() => {
                let path = run_dir.join(format!("{}.patch", lane.id));
                match std::fs::write(&path, &patch) {
                    Ok(()) => {
                        result.patch_sha256 = sha256_hex(&patch);
                        result.patch_bytes = patch.len();
                        (result.files, result.added, result.removed) = diff_stat(&patch);
                    }
                    Err(err) => result.error = Some(format!("cannot store patch: {err}")),
                }
            }
            Ok(_) => result.error = Some("no changes".into()),
            Err(err) => {
                result.correct = false;
                result.error = Some(format!("patch capture failed: {err}"));
            }
        }
    } else if generated {
        result.error = Some("cancelled".into());
    } else if result.error.is_none() {
        result.error = Some("generation did not complete".into());
    }
    result.cleanup = baseline.remove_worktree(&tree).await;
    result
}

/// Runs a whole race in the foreground; `cancel` stops both lanes.
#[allow(clippy::too_many_arguments)]
pub async fn run(
    hub: &Arc<AgentHub>,
    data_dir: &Path,
    project: &Path,
    request: &str,
    lanes: &[Lane; 2],
    verify: Vec<Vec<String>>,
    timeout: Duration,
    cancel: &CancellationToken,
) -> Result<Report> {
    if request.trim().is_empty() {
        return Err(Error::Tool("用法：/race <需求>".into()));
    }
    changes::validate_argvs(&verify)?;
    let baseline = Baseline::capture(project, true, cancel).await?;
    if baseline.branch.is_empty() {
        return Err(Error::Tool(
            "race 需要目标分支，不能在 detached HEAD 上运行；请先 git switch 到一个分支".into(),
        ));
    }
    let started = std::time::Instant::now();
    let deadline = tokio::time::Instant::now() + timeout;
    let mut report = Report {
        version: 2,
        id: new_id(),
        baseline: baseline.clone(),
        request: request.to_owned(),
        verify: verify.clone(),
        started: now(),
        duration_ms: 0,
        status: Status::Running,
        candidates: Vec::new(),
        selected: None,
        reviews: Default::default(),
    };
    save(data_dir, &report)?;
    let run_dir = directory(data_dir).join(&report.id);
    let race = cancel.child_token();
    let timer = {
        let race = race.clone();
        tokio::spawn(async move {
            tokio::time::sleep_until(deadline).await;
            race.cancel();
        })
    };
    let (a, b) = tokio::join!(
        candidate(
            hub, &baseline, &run_dir, &lanes[0], request, &verify, deadline, &race
        ),
        candidate(
            hub, &baseline, &run_dir, &lanes[1], request, &verify, deadline, &race
        ),
    );
    timer.abort();
    report.candidates = vec![a, b];
    report.duration_ms = started.elapsed().as_millis() as u64;
    report.status = if cancel.is_cancelled() {
        Status::Cancelled
    } else if race.is_cancelled() {
        Status::Timeout
    } else {
        Status::Completed
    };
    save(data_dir, &report)?;
    Ok(report)
}

/// The newest race of `project`.
pub fn latest(data_dir: &Path, project: &Path) -> Result<Option<Report>> {
    Ok(list(data_dir, project)?.into_iter().next())
}

pub fn render(report: &Report) -> String {
    let status = match report.status {
        Status::Running => "进行中",
        Status::Completed => "已完成",
        Status::Timeout => "超时",
        Status::Cancelled => "已取消",
    };
    let mut text = format!(
        "竞速 {}（{status}，{:.0} 秒）：{}\n",
        &report.id[..report.id.len().min(8)],
        report.duration_ms as f64 / 1000.0,
        crate::text::short(&report.request, 80)
    );
    let verify = if report.verify.is_empty() {
        "未验证（推断不出验证命令；可配置 done_verify_commands）".to_owned()
    } else {
        report
            .verify
            .iter()
            .map(|argv| argv.join(" "))
            .collect::<Vec<_>>()
            .join("；")
    };
    text.push_str(&format!("  验证：{verify}\n"));
    for candidate in &report.candidates {
        let checks = if candidate.checks.is_empty() {
            "未验证".to_owned()
        } else if candidate.correct {
            "验证 ✓".to_owned()
        } else {
            let failed: Vec<String> = candidate
                .checks
                .iter()
                .filter(|check| check.status != CheckStatus::Passed)
                .map(|check| check.argv.join(" "))
                .collect();
            format!("验证 ✗ {}", failed.join("；"))
        };
        let generation = candidate.generation.as_ref();
        let cost = generation
            .and_then(|g| g.cost_usd)
            .map_or("费用未知".into(), |cost| format!("${cost:.2}"));
        let change = if candidate.patch_bytes == 0 {
            "无改动".to_owned()
        } else {
            format!(
                "改动 {} 个文件 +{} -{}",
                candidate.files, candidate.added, candidate.removed
            )
        };
        text.push_str(&format!(
            "  {}  {} {}  {checks}  {change}  {cost}{}\n",
            candidate.id.to_uppercase(),
            candidate.label,
            candidate.model,
            candidate
                .error
                .as_ref()
                .filter(|_| candidate.patch_bytes == 0 || !candidate.correct)
                .map_or(String::new(), |err| format!("（{err}）"))
        ));
        if let Some(generation) = generation
            && !generation.summary.is_empty()
        {
            text.push_str(&format!("     {}\n", generation.summary));
        }
    }
    text.push_str(&next_step(report));
    text
}

/// What to run next: apply the first solution that can be applied.
fn next_step(report: &Report) -> String {
    let applicable = report.candidates.iter().find(|candidate| {
        candidate.patch_bytes > 0 && candidate.correct && !candidate.checks.is_empty()
    });
    match applicable {
        Some(candidate) => format!(
            "  /changes diff race:{id}:{lane} 查看补丁；/changes apply race:{id}:{lane} confirm 应用\n",
            id = report.id,
            lane = candidate.id
        ),
        None => format!(
            "  没有可以直接应用的方案（未通过验证、未验证或没有改动）；/changes diff race:{}:<a|b> 查看补丁，/changes export 可导出\n",
            report.id
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lane_b_uses_the_fast_model_or_asks_for_another_approach() {
        let [a, b] = lanes("fix the bug", "opus", Some("sonnet"));
        assert_eq!(
            (a.model.as_str(), a.prompt.as_str()),
            ("opus", "fix the bug")
        );
        assert_eq!(
            (b.model.as_str(), b.prompt.as_str()),
            ("sonnet", "fix the bug")
        );
        for fast in [None, Some("opus"), Some("")] {
            let [_, b] = lanes("fix the bug", "opus", fast);
            assert_eq!(b.model, "opus");
            assert!(b.prompt.starts_with("fix the bug") && b.prompt.contains("different approach"));
        }
    }

    #[test]
    fn verification_prefers_configured_commands_and_splits_them() {
        let dir = tempfile::tempdir().unwrap();
        assert!(verification(dir.path(), &[]).is_empty());
        std::fs::write(
            dir.path().join("Cargo.toml"),
            "[package]\nname = \"x\"\nversion = \"0.1.0\"\n",
        )
        .unwrap();
        let detected = verification(dir.path(), &[]);
        assert!(
            detected.iter().any(|argv| argv[0] == "cargo"),
            "{detected:?}"
        );
        assert_eq!(
            verification(dir.path(), &["make  test".into(), "make test".into()]),
            [vec!["make".to_owned(), "test".to_owned()]]
        );
    }

    #[test]
    fn the_next_step_names_a_solution_that_can_be_applied() {
        let candidate = |id: &str, correct: bool| Candidate {
            id: id.into(),
            label: String::new(),
            model: String::new(),
            prompt: String::new(),
            generation: None,
            checks: vec![Check {
                argv: vec!["make".into()],
                exit_code: Some(if correct { 0 } else { 1 }),
                status: if correct {
                    CheckStatus::Passed
                } else {
                    CheckStatus::Failed
                },
                attempt: 1,
                duration_ms: 0,
                output: String::new(),
            }],
            correct,
            patch_sha256: String::new(),
            patch_bytes: 10,
            files: 1,
            added: 1,
            removed: 0,
            error: None,
            cleanup: None,
        };
        let mut report = Report {
            version: 2,
            id: "run".into(),
            baseline: Baseline {
                root: PathBuf::from("/p"),
                commit: "c".into(),
                branch: "refs/heads/main".into(),
            },
            request: String::new(),
            verify: Vec::new(),
            started: 0,
            duration_ms: 0,
            status: Status::Completed,
            candidates: vec![candidate("a", false), candidate("b", true)],
            selected: None,
            reviews: Default::default(),
        };
        assert!(next_step(&report).contains("race:run:b confirm"));
        report.candidates[1].correct = false;
        assert!(next_step(&report).contains("没有可以直接应用"));
    }

    #[test]
    fn diff_stat_counts_files_and_lines() {
        let patch = b"diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1,2 @@\n-old\n+new\n+more\ndiff --git a/y b/y\n";
        assert_eq!(diff_stat(patch), (2, 2, 1));
    }
}
