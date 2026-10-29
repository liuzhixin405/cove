//! Two solutions raced in separate worktrees, checked by the same verifier argv.
//!
//! Requires a clean repository on a branch. Each candidate runs a child
//! `cove -p` in a detached worktree from HEAD with half the budget, then the
//! shared verifiers (exit codes only), then its patch is captured. Reports and
//! patches live under `<data>/race/<run-id>/`; nothing is applied until an
//! explicit `/changes apply race:<run>:<a|b> confirm`.

use std::{
    path::{Path, PathBuf},
    time::Duration,
};

use serde::{Deserialize, Serialize};
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Result},
    session::{atomic_json, new_id, now, read_json},
    workflows::changes::{
        self, Baseline, Changeset, Check, CheckStatus, Decision, Kind, capture_patch, run_checks,
        sha256_hex,
    },
    workflows::worker::{self, Execution, Job, RunStatus, WorkerConfig},
};

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Spec {
    #[serde(default)]
    pub version: Option<u32>,
    pub prompts: Vec<String>,
    pub verify: Vec<Vec<String>>,
    /// Total estimated USD; each candidate gets half.
    #[serde(default, alias = "total_budget_usd")]
    pub budget: Option<f64>,
    /// Whole race in seconds (generation and verification) [default 900].
    #[serde(default, alias = "total_timeout_seconds")]
    pub timeout: Option<u64>,
}

impl Spec {
    pub fn read(path: &Path) -> Result<Self> {
        let bytes = std::fs::read(path)?;
        if bytes.len() > 256 * 1024 {
            return Err(Error::Tool("race 配置超过 256 KiB".into()));
        }
        let bytes = bytes.strip_prefix(b"\xEF\xBB\xBF").unwrap_or(&bytes);
        let spec: Self = serde_json::from_slice(bytes)?;
        spec.validate()?;
        Ok(spec)
    }

    pub fn validate(&self) -> Result<()> {
        if self.version.is_some_and(|version| version != 1) {
            return Err(Error::Tool(
                "不支持的 race 配置版本（version 应为 1）".into(),
            ));
        }
        let [a, b] = self.prompts.as_slice() else {
            return Err(Error::Tool("prompts 必须正好有两个".into()));
        };
        if a.trim().is_empty() || b.trim().is_empty() || a.trim() == b.trim() {
            return Err(Error::Tool("两个 prompt 必须非空且互不相同".into()));
        }
        if self.verify.is_empty() {
            return Err(Error::Tool(
                "至少需要一个共享的验证命令（verify）；模型不能给自己打分".into(),
            ));
        }
        changes::validate_argvs(&self.verify)?;
        if let Some(budget) = self.budget
            && !(budget.is_finite() && budget > 0.0)
        {
            return Err(Error::Tool("budget 必须为正数".into()));
        }
        if self.timeout.is_some_and(|t| t == 0 || t > 86_400) {
            return Err(Error::Tool("timeout 必须在 1..86400 秒之间".into()));
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Candidate {
    pub id: String,
    pub prompt: String,
    pub budget_usd: Option<f64>,
    pub generation: Option<Execution>,
    pub checks: Vec<Check>,
    pub correct: bool,
    pub patch_sha256: String,
    pub patch_bytes: usize,
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
    pub spec: Spec,
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
                title: candidate.prompt.chars().take(80).collect(),
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

// One lane of the race; the parameters are the lane's whole context.
#[allow(clippy::too_many_arguments)]
async fn candidate(
    config: &WorkerConfig,
    baseline: &Baseline,
    run_dir: &Path,
    id: &str,
    prompt: &str,
    budget: Option<f64>,
    verify: &[Vec<String>],
    deadline: tokio::time::Instant,
    cancel: &CancellationToken,
) -> Candidate {
    let mut result = Candidate {
        id: id.into(),
        prompt: prompt.into(),
        budget_usd: budget,
        generation: None,
        checks: Vec::new(),
        correct: false,
        patch_sha256: String::new(),
        patch_bytes: 0,
        error: None,
        cleanup: None,
    };
    let tree = run_dir.join(format!("wt-{id}"));
    if let Err(err) = baseline.add_worktree(&tree, cancel).await {
        result.error = Some(err.to_string());
        result.cleanup = baseline.remove_worktree(&tree).await;
        return result;
    }
    let remaining = deadline.saturating_duration_since(tokio::time::Instant::now());
    // Leave at least a third of the remaining time for verification.
    let generation = worker::run(
        config,
        Job {
            worktree: &tree,
            prompt,
            budget,
            max_turns: 12,
            timeout: (remaining * 2 / 3).max(Duration::from_secs(1)),
        },
        cancel,
    )
    .await;
    let generated = generation.status == RunStatus::Completed;
    result.generation = Some(generation);
    if generated && !cancel.is_cancelled() {
        result.checks = run_checks(&tree, verify, 1, deadline, cancel).await;
        result.correct = result
            .checks
            .iter()
            .all(|check| check.status == CheckStatus::Passed);
        match capture_patch(&tree, &baseline.commit, cancel).await {
            Ok(patch) if !patch.is_empty() => {
                let path = run_dir.join(format!("{id}.patch"));
                match std::fs::write(&path, &patch) {
                    Ok(()) => {
                        result.patch_sha256 = sha256_hex(&patch);
                        result.patch_bytes = patch.len();
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
    } else if !generated {
        result.error = Some("generation did not complete".into());
    }
    result.cleanup = baseline.remove_worktree(&tree).await;
    result
}

/// Runs a whole race in the foreground; `cancel` stops both candidates.
pub async fn run(
    data_dir: &Path,
    project: &Path,
    spec: Spec,
    config: &WorkerConfig,
    cancel: &CancellationToken,
) -> Result<Report> {
    changes::refuse_nested()?;
    spec.validate()?;
    let baseline = Baseline::capture(project, true, cancel).await?;
    if baseline.branch.is_empty() {
        return Err(Error::Tool(
            "race 需要目标分支，不能在 detached HEAD 上运行；请先 git switch 到一个分支".into(),
        ));
    }
    let started = std::time::Instant::now();
    let timeout = Duration::from_secs(spec.timeout.unwrap_or(900));
    let deadline = tokio::time::Instant::now() + timeout;
    let share = match (spec.budget, config.prices) {
        (Some(budget), Some(_)) => Some(budget / 2.0),
        (Some(_), None) => {
            return Err(Error::Tool(
                "race 的预算需要已知的 token 价格（input_price/output_price）".into(),
            ));
        }
        (None, _) => None,
    };
    let mut report = Report {
        version: 1,
        id: new_id(),
        baseline: baseline.clone(),
        spec: spec.clone(),
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
            config,
            &baseline,
            &run_dir,
            "a",
            &spec.prompts[0],
            share,
            &spec.verify,
            deadline,
            &race
        ),
        candidate(
            config,
            &baseline,
            &run_dir,
            "b",
            &spec.prompts[1],
            share,
            &spec.verify,
            deadline,
            &race
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

pub fn render(report: &Report) -> String {
    let mut text = format!(
        "race {} {:?} in {:.1}s (base {} on {})\n",
        report.id,
        report.status,
        report.duration_ms as f64 / 1000.0,
        &report.baseline.commit[..report.baseline.commit.len().min(12)],
        report.baseline.branch
    );
    for candidate in &report.candidates {
        let generation = candidate.generation.as_ref();
        text.push_str(&format!(
            "  {}: generation={:?} cost={} checks={} correct={} patch={} bytes {}{}\n",
            candidate.id,
            generation.map(|g| g.status),
            generation
                .and_then(|g| g.cost_usd)
                .map_or("unverified".into(), |cost| format!("${cost:.4}")),
            candidate
                .checks
                .iter()
                .map(|check| format!("{:?}", check.status).to_lowercase())
                .collect::<Vec<_>>()
                .join(","),
            candidate.correct,
            candidate.patch_bytes,
            &candidate.patch_sha256[..candidate.patch_sha256.len().min(12)],
            candidate
                .error
                .as_ref()
                .map_or(String::new(), |err| format!(" error={err}"))
        ));
    }
    text.push_str(&format!(
        "用 /changes apply race:{}:<a|b> confirm 应用通过验证的候选方案\n",
        report.id
    ));
    text
}

#[cfg(test)]
mod tests {
    use super::*;

    fn spec(value: serde_json::Value) -> Result<Spec> {
        let spec: Spec = serde_json::from_value(value)?;
        spec.validate()?;
        Ok(spec)
    }

    #[test]
    fn specs_need_two_distinct_prompts_and_a_verifier() {
        assert!(spec(serde_json::json!({"prompts":["a","b"],"verify":[["cargo","test"]],"budget":2,"timeout":60})).is_ok());
        assert!(spec(serde_json::json!({"prompts":["a","a"],"verify":[["x"]]})).is_err());
        assert!(spec(serde_json::json!({"prompts":["a"],"verify":[["x"]]})).is_err());
        assert!(spec(serde_json::json!({"prompts":["a","b"],"verify":[]})).is_err());
        assert!(spec(serde_json::json!({"prompts":["a","b"],"verify":[[""]]})).is_err());
        assert!(
            spec(serde_json::json!({"prompts":["a","b"],"verify":[["x"]],"budget":-1})).is_err()
        );
        // Alternative field names are accepted.
        assert!(spec(serde_json::json!({"version":1,"prompts":["a","b"],"verify":[["x"]],"total_budget_usd":1,"total_timeout_seconds":5})).is_ok());
    }

    #[tokio::test]
    async fn dirty_or_detached_projects_are_refused_before_any_worker() {
        let repo = changes::tests::repo();
        let data = tempfile::tempdir().unwrap();
        let config = WorkerConfig {
            provider: "openai".into(),
            base_url: "http://127.0.0.1:9".into(),
            model: "m".into(),
            api_key: "k".into(),
            prices: None,
            context_window: 32000,
            max_output_tokens: 1000,
            instructions: None,
        };
        let spec = Spec {
            version: None,
            prompts: vec!["a".into(), "b".into()],
            verify: vec![vec!["git".into(), "status".into()]],
            budget: None,
            timeout: Some(10),
        };
        std::fs::write(repo.path().join("untracked.txt"), "x").unwrap();
        let cancel = CancellationToken::new();
        assert!(
            run(data.path(), repo.path(), spec.clone(), &config, &cancel)
                .await
                .is_err()
        );
        std::fs::remove_file(repo.path().join("untracked.txt")).unwrap();
        changes::tests::git_sync(repo.path(), &["checkout", "-q", "--detach"]);
        assert!(
            run(data.path(), repo.path(), spec, &config, &cancel)
                .await
                .is_err()
        );
        assert!(list(data.path(), repo.path()).unwrap().is_empty());
    }
}
