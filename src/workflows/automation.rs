//! Opt-in automations: project-keyed job specs that run a child agent in a fresh
//! detached worktree from HEAD and drop the resulting patch into an inbox.
//!
//! State lives in `<data>/automations/<project-key>/` (`state.json`, `patches/`,
//! a state lock and a job lock). Adding a spec never starts anything; runs happen
//! only on `run`, `tick` (one due scan, for OS schedulers) or `event`. Results are
//! never applied automatically; `/changes apply inbox:<id> confirm` does that after
//! the shared baseline/clean/check/hash preconditions.

use std::{
    fs::OpenOptions,
    path::{Path, PathBuf},
    time::Duration,
};

use fs2::FileExt;
use serde::{Deserialize, Serialize};
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Result},
    session::{atomic_json, new_id, now, read_json},
    workflows::changes::{
        self, Baseline, Changeset, Check, CheckStatus, Decision, Kind, capture_patch, run_checks,
        sha256_hex,
    },
    workflows::worker::{self, Job, RunStatus, WorkerConfig},
};

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Spec {
    pub id: String,
    pub prompt: String,
    #[serde(default = "enabled")]
    pub enabled: bool,
    /// Schedule interval for `tick`; 0 disables scheduling (manual/event still run).
    #[serde(default)]
    pub every_seconds: u64,
    /// Event name accepted by `event <name> <key>`.
    #[serde(default)]
    pub event: Option<String>,
    #[serde(default = "default_timeout")]
    pub timeout_seconds: u64,
    /// Total estimated USD, split evenly across attempts.
    #[serde(default)]
    pub budget_usd: Option<f64>,
    #[serde(default = "default_turns")]
    pub max_turns: u32,
    /// Extra attempts after a completed failure (0..=2).
    #[serde(default)]
    pub retries: u32,
    #[serde(default)]
    pub verify: Vec<Vec<String>>,
}

fn enabled() -> bool {
    true
}
fn default_timeout() -> u64 {
    600
}
fn default_turns() -> u32 {
    12
}

/// The file format `{"version":1,"spec":{...}}`, or a bare spec.
#[derive(Deserialize)]
#[serde(untagged)]
enum SpecFile {
    Wrapped {
        #[serde(default)]
        version: Option<u32>,
        spec: Spec,
    },
    Bare(Spec),
}

impl Spec {
    pub fn read(path: &Path) -> Result<Self> {
        let bytes = std::fs::read(path)?;
        if bytes.len() > 128 * 1024 {
            return Err(Error::Tool("automation spec exceeds 128 KiB".into()));
        }
        let bytes = bytes.strip_prefix(b"\xEF\xBB\xBF").unwrap_or(&bytes);
        let spec = match serde_json::from_slice::<SpecFile>(bytes)
            .map_err(|err| Error::Tool(format!("自动化任务配置无效：{err}")))?
        {
            SpecFile::Wrapped { version, spec } => {
                if version.is_some_and(|version| version != 1) {
                    return Err(Error::Tool("unsupported automation spec version".into()));
                }
                spec
            }
            SpecFile::Bare(spec) => spec,
        };
        spec.validate()?;
        Ok(spec)
    }

    pub fn validate(&self) -> Result<()> {
        let id_ok = !self.id.is_empty()
            && self.id.len() <= 64
            && self
                .id
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
            && self.id.as_bytes()[0].is_ascii_alphanumeric();
        if !id_ok {
            return Err(Error::Tool(
                "automation id must be 1..64 of [A-Za-z0-9_-], starting alphanumeric".into(),
            ));
        }
        if self.prompt.trim().is_empty() || self.prompt.len() > worker::PROMPT_LIMIT {
            return Err(Error::Tool("prompt must be 1..24576 bytes".into()));
        }
        if self.every_seconds != 0 && self.every_seconds < 60 {
            return Err(Error::Tool("every_seconds must be 0 or at least 60".into()));
        }
        if !(1..=3600).contains(&self.timeout_seconds) {
            return Err(Error::Tool("timeout_seconds must be 1..3600".into()));
        }
        if !(1..=200).contains(&self.max_turns) {
            return Err(Error::Tool("max_turns must be 1..200".into()));
        }
        if self.retries > 2 {
            return Err(Error::Tool("retries must be 0..2".into()));
        }
        if let Some(budget) = self.budget_usd
            && !(budget.is_finite() && budget > 0.0)
        {
            return Err(Error::Tool("budget_usd must be positive".into()));
        }
        if self.event.as_ref().is_some_and(|event| {
            event.is_empty() || event.len() > 64 || event.contains(char::is_whitespace)
        }) {
            return Err(Error::Tool(
                "event must be 1..64 non-space characters".into(),
            ));
        }
        changes::validate_argvs(&self.verify)
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RunState {
    Running,
    Succeeded,
    Failed,
    Interrupted,
    /// The process stopped mid-run; effects unknown. Blocks further runs until reviewed.
    Uncertain,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct InboxResult {
    pub id: String,
    pub task: String,
    pub trigger: String,
    pub state: RunState,
    pub review: Decision,
    pub started: u64,
    #[serde(default)]
    pub finished: Option<u64>,
    #[serde(default)]
    pub reviewed_at: Option<u64>,
    pub attempts: u32,
    /// Root, HEAD and branch when the job started (branch empty when detached).
    #[serde(default)]
    pub baseline: Option<Baseline>,
    #[serde(default)]
    pub checks: Vec<Check>,
    #[serde(default)]
    pub patch_sha256: String,
    #[serde(default)]
    pub patch_bytes: usize,
    #[serde(default)]
    pub cost_usd: Option<f64>,
    #[serde(default)]
    pub error: Option<String>,
}

#[derive(Default, Serialize, Deserialize)]
struct State {
    version: u32,
    project: PathBuf,
    specs: Vec<Spec>,
    #[serde(default)]
    last_run: std::collections::BTreeMap<String, u64>,
    /// `<task>\n<event>\n<key>` occurrences already consumed (also when they failed).
    #[serde(default)]
    consumed: std::collections::BTreeSet<String>,
    results: Vec<InboxResult>,
}

pub struct Store {
    dir: PathBuf,
    project: PathBuf,
}

const MAX_RESULTS: usize = 200;

impl Store {
    pub fn open(data_dir: &Path, project: &Path) -> Result<Self> {
        let project = project.canonicalize()?;
        let dir = data_dir
            .join("automations")
            .join(changes::project_key(&project));
        std::fs::create_dir_all(dir.join("patches"))?;
        Ok(Self { dir, project })
    }

    pub fn project(&self) -> &Path {
        &self.project
    }

    fn lock(&self, name: &str) -> Result<std::fs::File> {
        Ok(OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .open(self.dir.join(name))?)
    }

    /// Read-modify-write of `state.json` under the state lock (shared with OS-scheduled runs).
    fn change<T>(&self, update: impl FnOnce(&mut State) -> Result<T>) -> Result<T> {
        let lock = self.lock("state.lock")?;
        lock.lock_exclusive()?;
        let path = self.dir.join("state.json");
        let mut state = if path.exists() {
            read_json::<State>(&path)?
        } else {
            State {
                version: 1,
                project: self.project.clone(),
                ..State::default()
            }
        };
        let value = update(&mut state)?;
        atomic_json(&path, &state)?;
        Ok(value)
    }

    fn read(&self) -> Result<State> {
        self.change(|_| Ok(()))?;
        read_json(&self.dir.join("state.json"))
    }

    pub fn specs(&self) -> Result<Vec<Spec>> {
        Ok(self.read()?.specs)
    }

    pub fn add(&self, spec: Spec) -> Result<()> {
        spec.validate()?;
        self.change(|state| {
            if state.specs.iter().any(|s| s.id == spec.id) {
                return Err(Error::Tool(
                    "an automation with this id exists; remove it first".into(),
                ));
            }
            if state.specs.len() >= 32 {
                return Err(Error::Tool("at most 32 automations per project".into()));
            }
            state.specs.push(spec);
            Ok(())
        })
    }

    pub fn remove(&self, id: &str) -> Result<()> {
        self.change(|state| {
            let before = state.specs.len();
            state.specs.retain(|spec| spec.id != id);
            if before == state.specs.len() {
                return Err(Error::Tool("未知的自动化任务；用 list 查看现有任务".into()));
            }
            Ok(())
        })
    }

    pub fn results(&self) -> Result<Vec<InboxResult>> {
        self.recover()?;
        let mut results = self.read()?.results;
        results.sort_by_key(|result| std::cmp::Reverse(result.started));
        Ok(results)
    }

    pub fn result(&self, id: &str) -> Result<InboxResult> {
        self.results()?
            .into_iter()
            .find(|result| result.id == id)
            .ok_or_else(|| Error::Tool("收件箱里没有这个结果；用 inbox list 查看".into()))
    }

    pub fn patch_path(&self, id: &str) -> Result<PathBuf> {
        if !crate::session::valid_id(id) {
            return Err(Error::Tool("invalid inbox result ID".into()));
        }
        Ok(self.dir.join("patches").join(format!("{id}.patch")))
    }

    pub fn review(&self, id: &str, decision: Decision) -> Result<()> {
        self.change(|state| {
            let result = state
                .results
                .iter_mut()
                .find(|result| result.id == id)
                .ok_or_else(|| Error::Tool("收件箱里没有这个结果；用 inbox list 查看".into()))?;
            if result.state == RunState::Running {
                return Err(Error::Tool("这个结果还在运行中；请等它结束".into()));
            }
            if result.review == Decision::Applied && decision != Decision::Applied {
                return Err(Error::Tool("这个结果已经应用过了".into()));
            }
            result.review = decision;
            result.reviewed_at = Some(now());
            Ok(())
        })
    }

    /// Marks results left `running` by a stopped process as uncertain. Only
    /// possible while nobody holds the job lock.
    pub fn recover(&self) -> Result<()> {
        let job = self.lock("job.lock")?;
        if job.try_lock_exclusive().is_err() {
            return Ok(());
        }
        self.change(|state| {
            for result in &mut state.results {
                if result.state == RunState::Running {
                    result.state = RunState::Uncertain;
                    result.error = Some(
                        "the process stopped during this job; inspect before running again".into(),
                    );
                }
            }
            Ok(())
        })
    }

    pub fn changesets(&self) -> Result<Vec<Changeset>> {
        let mut sets = Vec::new();
        for result in self.results()? {
            let not_applicable = match (&result.baseline, result.state) {
                (_, state) if state != RunState::Succeeded => Some(format!("job {state:?}")),
                (None, _) => Some("没有记录基线".to_owned()),
                (Some(baseline), _) if baseline.branch.is_empty() => {
                    Some("任务运行在 detached HEAD 上；只能导出".to_owned())
                }
                _ if result.patch_bytes == 0 => Some("没有补丁".to_owned()),
                _ => None,
            };
            sets.push(Changeset {
                id: format!("inbox:{}", result.id),
                kind: Kind::Inbox,
                title: format!("{} ({})", result.task, result.trigger),
                baseline: result.baseline.clone(),
                patch_sha256: result.patch_sha256.clone(),
                patch_bytes: result.patch_bytes,
                checks: result.checks.clone(),
                review: result.review,
                not_applicable,
                created: result.started,
                patch_path: Some(self.patch_path(&result.id)?),
            });
        }
        Ok(sets)
    }
}

#[derive(Clone, Debug)]
pub enum Trigger {
    Manual,
    Schedule,
    Event { name: String, key: String },
}

impl Trigger {
    fn label(&self) -> String {
        match self {
            Self::Manual => "manual".into(),
            Self::Schedule => "schedule".into(),
            Self::Event { name, key } => format!("event:{name}:{key}"),
        }
    }
}

/// Runs one job now: claims the job lock, records a running result, runs up to
/// `1 + retries` attempts in fresh worktrees and stores the final patch.
pub async fn run_job(
    store: &Store,
    spec: &Spec,
    trigger: Trigger,
    config: &WorkerConfig,
    cancel: &CancellationToken,
) -> Result<InboxResult> {
    changes::refuse_nested()?;
    if !spec.enabled {
        return Err(Error::Tool(format!("自动化任务 {} 已停用", spec.id)));
    }
    let job_lock = store.lock("job.lock")?;
    job_lock
        .try_lock_exclusive()
        .map_err(|_| Error::Tool("another automation job is running for this project".into()))?;
    // The lock is held: anything still `running` belongs to a stopped process.
    store.change(|state| {
        for result in &mut state.results {
            if result.state == RunState::Running {
                result.state = RunState::Uncertain;
            }
        }
        if let Some(blocked) = state
            .results
            .iter()
            .find(|r| r.state == RunState::Uncertain && r.review == Decision::Pending)
        {
            return Err(Error::Tool(format!(
                "inbox result {} is uncertain; review it (accepted/rejected) before running again",
                blocked.id
            )));
        }
        Ok(())
    })?;
    let baseline = Baseline::capture(store.project(), false, cancel).await?;
    let mut result = InboxResult {
        id: new_id(),
        task: spec.id.clone(),
        trigger: trigger.label(),
        state: RunState::Running,
        review: Decision::Pending,
        started: now(),
        finished: None,
        reviewed_at: None,
        attempts: 0,
        baseline: Some(baseline.clone()),
        checks: Vec::new(),
        patch_sha256: String::new(),
        patch_bytes: 0,
        cost_usd: None,
        error: None,
    };
    store.change(|state| {
        if let Trigger::Event { name, key } = &trigger {
            let occurrence = format!("{}\n{name}\n{key}", spec.id);
            if !state.consumed.insert(occurrence) {
                return Err(Error::Tool(
                    "this event occurrence was already handled".into(),
                ));
            }
        }
        state.last_run.insert(spec.id.clone(), now());
        state.results.push(result.clone());
        if state.results.len() > MAX_RESULTS {
            let excess = state.results.len() - MAX_RESULTS;
            state.results.drain(..excess);
        }
        Ok(())
    })?;
    let deadline = tokio::time::Instant::now() + Duration::from_secs(spec.timeout_seconds);
    let attempts = spec.retries + 1;
    let share = match (spec.budget_usd, config.prices) {
        (Some(budget), Some(_)) => Some(budget / f64::from(attempts)),
        _ => None,
    };
    let scratch = store.dir.join("worktrees");
    std::fs::create_dir_all(&scratch)?;
    let mut patch = Vec::new();
    let mut outcome = RunState::Failed;
    for attempt in 1..=attempts {
        result.attempts = attempt;
        let tree = scratch.join(format!("{}-{attempt}", result.id));
        let attempt_result: Result<(RunState, Vec<u8>)> = async {
            baseline.add_worktree(&tree, cancel).await?;
            let remaining = deadline.saturating_duration_since(tokio::time::Instant::now());
            if remaining.is_zero() {
                return Ok((RunState::Interrupted, Vec::new()));
            }
            let execution = worker::run(
                config,
                Job {
                    worktree: &tree,
                    prompt: &spec.prompt,
                    budget: share,
                    max_turns: spec.max_turns,
                    timeout: (remaining * 3 / 4).max(Duration::from_secs(1)),
                },
                cancel,
            )
            .await;
            result.cost_usd = match (result.cost_usd, execution.cost_usd) {
                (Some(a), Some(b)) => Some(a + b),
                (a, b) => a.or(b),
            };
            result.checks.push(Check {
                argv: vec!["cove".into(), "-p".into(), "<prompt>".into()],
                exit_code: execution.exit_code,
                status: if execution.status == RunStatus::Completed {
                    CheckStatus::Passed
                } else {
                    CheckStatus::Failed
                },
                attempt,
                duration_ms: execution.duration_ms,
                output: execution.output.clone(),
            });
            match execution.status {
                RunStatus::Cancelled | RunStatus::Timeout => {
                    return Ok((RunState::Interrupted, Vec::new()));
                }
                RunStatus::Completed => {}
                _ => return Ok((RunState::Failed, Vec::new())),
            }
            let checks = run_checks(&tree, &spec.verify, attempt, deadline, cancel).await;
            let passed = checks
                .iter()
                .all(|check| check.status == CheckStatus::Passed);
            let interrupted = checks
                .iter()
                .any(|check| check.status == CheckStatus::Unverified);
            result.checks.extend(checks);
            let patch = capture_patch(&tree, &baseline.commit, cancel).await?;
            Ok(if passed {
                (RunState::Succeeded, patch)
            } else if interrupted
                && (cancel.is_cancelled() || tokio::time::Instant::now() >= deadline)
            {
                (RunState::Interrupted, patch)
            } else {
                (RunState::Failed, patch)
            })
        }
        .await;
        if let Some(left) = baseline.remove_worktree(&tree).await {
            result.error = Some(left);
        }
        match attempt_result {
            Ok((state, bytes)) => {
                outcome = state;
                patch = bytes;
            }
            Err(Error::Cancelled) => outcome = RunState::Interrupted,
            Err(err) => {
                outcome = RunState::Failed;
                result.error = Some(err.to_string());
            }
        }
        // Only completed failures are retried; interruptions never are.
        if outcome != RunState::Failed || cancel.is_cancelled() {
            break;
        }
    }
    if outcome == RunState::Succeeded && spec.verify.is_empty() {
        // The child's exit status is the only evidence; still a reviewable result.
        result
            .error
            .get_or_insert_with(|| "no independent verification configured".into());
    }
    if !patch.is_empty() {
        let path = store.patch_path(&result.id)?;
        std::fs::write(&path, &patch)?;
        result.patch_sha256 = sha256_hex(&patch);
        result.patch_bytes = patch.len();
    }
    result.state = outcome;
    result.finished = Some(now());
    let saved = result.clone();
    store.change(|state| {
        if let Some(slot) = state.results.iter_mut().find(|r| r.id == saved.id) {
            *slot = saved;
        }
        Ok(())
    })?;
    drop(job_lock);
    Ok(result)
}

/// Runs every enabled, scheduled spec that is due, sequentially, once each.
pub async fn tick(
    store: &Store,
    config: &WorkerConfig,
    cancel: &CancellationToken,
) -> Result<Vec<InboxResult>> {
    let state = store.read()?;
    let mut results = Vec::new();
    for spec in state
        .specs
        .iter()
        .filter(|s| s.enabled && s.every_seconds > 0)
    {
        let last = state.last_run.get(&spec.id).copied().unwrap_or(0);
        if now().saturating_sub(last) < spec.every_seconds {
            continue;
        }
        results.push(run_job(store, spec, Trigger::Schedule, config, cancel).await?);
    }
    Ok(results)
}

/// Runs the specs listening to `name` once for occurrence `key` (deduplicated).
pub async fn event(
    store: &Store,
    name: &str,
    key: &str,
    config: &WorkerConfig,
    cancel: &CancellationToken,
) -> Result<Vec<InboxResult>> {
    if key.is_empty() || key.len() > 128 || key.contains(char::is_whitespace) {
        return Err(Error::Tool(
            "event key must be 1..128 non-space characters".into(),
        ));
    }
    let specs: Vec<Spec> = store
        .specs()?
        .into_iter()
        .filter(|spec| spec.enabled && spec.event.as_deref() == Some(name))
        .collect();
    if specs.is_empty() {
        return Err(Error::Tool(format!(
            "no enabled automation listens to event {name}"
        )));
    }
    let mut results = Vec::new();
    for spec in specs {
        let trigger = Trigger::Event {
            name: name.into(),
            key: key.into(),
        };
        match run_job(store, &spec, trigger, config, cancel).await {
            Ok(result) => results.push(result),
            Err(Error::Tool(message)) if message.contains("already handled") => {}
            Err(err) => return Err(err),
        }
    }
    Ok(results)
}

pub fn render_result(result: &InboxResult) -> String {
    format!(
        "{} {:<12} {:?}/{:?} attempts={} patch={} bytes cost={} {}{}",
        result.id,
        result.task,
        result.state,
        result.review,
        result.attempts,
        result.patch_bytes,
        result
            .cost_usd
            .map_or("unverified".into(), |cost| format!("${cost:.4}")),
        result.trigger,
        result
            .error
            .as_ref()
            .map_or(String::new(), |err| format!(" note={err}"))
    )
}

/// Shared text front end for `/auto …` and `cove --automation …`. `config` is
/// only built for commands that start a worker.
pub async fn command(
    data_dir: &Path,
    project: &Path,
    args: &[&str],
    config: impl FnOnce() -> Result<WorkerConfig>,
    cancel: &CancellationToken,
) -> Result<String> {
    let store = Store::open(data_dir, project)?;
    let lines = |results: Vec<InboxResult>| {
        if results.is_empty() {
            "没有运行任何任务。\n".to_owned()
        } else {
            results
                .iter()
                .map(|result| format!("{}\n", render_result(result)))
                .collect()
        }
    };
    Ok(match args {
        [] | ["list"] => {
            let specs = store.specs()?;
            if specs.is_empty() {
                "还没有自动化任务。用 /auto add <spec.json> 添加。\n".into()
            } else {
                specs
                    .iter()
                    .map(|spec| {
                        format!(
                            "{} enabled={} every={}s event={} turns={} retries={} verify={}\n",
                            spec.id,
                            spec.enabled,
                            spec.every_seconds,
                            spec.event.as_deref().unwrap_or("-"),
                            spec.max_turns,
                            spec.retries,
                            spec.verify.len()
                        )
                    })
                    .collect()
            }
        }
        ["add", path] => {
            let spec = Spec::read(Path::new(path.trim_matches('"')))?;
            let id = spec.id.clone();
            store.add(spec)?;
            format!("已添加自动化任务 {id}；尚未启动任何运行。\n")
        }
        ["remove", id] => {
            store.remove(id)?;
            format!("已删除自动化任务 {id}；其收件箱结果仍保留。\n")
        }
        ["run", id] => {
            let spec = store
                .specs()?
                .into_iter()
                .find(|spec| spec.id == *id)
                .ok_or_else(|| Error::Tool("未知的自动化任务；用 list 查看现有任务".into()))?;
            lines(vec![
                run_job(&store, &spec, Trigger::Manual, &config()?, cancel).await?,
            ])
        }
        ["tick"] => lines(tick(&store, &config()?, cancel).await?),
        ["event", name, key] => lines(event(&store, name, key, &config()?, cancel).await?),
        ["inbox"] | ["inbox", "list"] => {
            let results = store.results()?;
            if results.is_empty() {
                "收件箱是空的。\n".into()
            } else {
                lines(results)
            }
        }
        ["inbox", "show", id] => {
            let result = store.result(id)?;
            format!("{}\n", serde_json::to_string_pretty(&result)?)
        }
        ["inbox", "patch", id, out] => {
            let result = store.result(id)?;
            if result.patch_bytes == 0 {
                return Err(Error::Tool("这个结果没有补丁".into()));
            }
            let bytes = std::fs::read(store.patch_path(id)?)?;
            if sha256_hex(&bytes) != result.patch_sha256 {
                return Err(Error::Tool("保存的补丁哈希不匹配".into()));
            }
            let target = changes::new_outside_file(Path::new(out.trim_matches('"')), project)?;
            changes::write_new(&target, &bytes)?;
            format!("已写入 {} 字节到 {}\n", bytes.len(), target.display())
        }
        ["inbox", "review", id, decision] => {
            store.review(id, Decision::parse(decision)?)?;
            format!("已为 {id} 记录 {decision}；没有应用任何改动。\n")
        }
        _ => return Err(Error::Tool(USAGE.into())),
    })
}

pub const USAGE: &str = "usage: list | add <spec.json> | remove <id> | run <id> | tick | event <name> <key> | inbox [list|show <id>|patch <id> <out>|review <id> accepted|rejected]";

/// `cove --automation <list|tick|add|run|event|remove|inbox> <projectDir> …` for OS schedulers.
/// Returns the process exit code.
pub async fn cli(args: Vec<String>) -> i32 {
    let usage = "usage: cove --automation <list|tick|add <spec>|run <id>|event <name> <key>|remove <id>|inbox …> <projectDir>";
    let (Some(verb), Some(project)) = (args.first(), args.get(1)) else {
        eprintln!("{usage}");
        return 2;
    };
    let mut rest: Vec<&str> = vec![verb.as_str()];
    rest.extend(args[2..].iter().map(String::as_str));
    let project = PathBuf::from(project);
    let cli = <crate::config::Cli as clap::Parser>::parse_from([
        "cove".as_ref(),
        "--cwd".as_ref(),
        project.as_os_str(),
    ]);
    let result = async {
        let data = crate::config::data_dir(&cli).map_err(|err| Error::Tool(err.to_string()))?;
        let store = crate::session::Store::open(&data)?;
        let cwd = project.canonicalize()?;
        if store.root().starts_with(&cwd) {
            return Err(Error::Session(
                "数据目录必须在项目目录之外；请用 --data-dir 指定其他位置".into(),
            ));
        }
        let cancel = CancellationToken::new();
        let config = || {
            crate::config::Settings::load(cli.clone())
                .map(|settings| WorkerConfig::from_settings(&settings))
                .map_err(|err| Error::Tool(format!("{err:#}")))
        };
        let run = command(store.root(), &cwd, &rest, config, &cancel);
        tokio::pin!(run);
        tokio::select! {
            result = &mut run => result,
            _ = tokio::signal::ctrl_c() => {
                cancel.cancel();
                tokio::time::timeout(Duration::from_secs(30), &mut run)
                    .await
                    .unwrap_or_else(|_| Err(Error::Limit("清理超时".into())))
            }
        }
    }
    .await;
    match result {
        Ok(text) => {
            print!("{}", crate::app::ui::visible(&text));
            0
        }
        Err(err) => {
            eprintln!("错误：{}", crate::app::ui::visible(&err.to_string()));
            1
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn spec(id: &str) -> Spec {
        serde_json::from_value(serde_json::json!({"id":id,"prompt":"fix it","every_seconds":3600,"event":"tests-failed","verify":[["git","status"]]})).unwrap()
    }

    #[test]
    fn go_and_bare_spec_files_validate() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("spec.json");
        std::fs::write(&path, r#"{"version":1,"spec":{"id":"maintenance","prompt":"Fix one small failing test","enabled":true,"every_seconds":3600,"event":"tests-failed","timeout_seconds":300,"budget_usd":1,"max_turns":8,"retries":1,"verify":[["go","test","./..."]]}}"#).unwrap();
        assert_eq!(Spec::read(&path).unwrap().max_turns, 8);
        std::fs::write(&path, r#"{"id":"x","prompt":"p"}"#).unwrap();
        assert!(Spec::read(&path).unwrap().enabled);
        for bad in [
            r#"{"id":"../x","prompt":"p"}"#,
            r#"{"id":"x","prompt":"p","every_seconds":5}"#,
            r#"{"id":"x","prompt":"p","retries":3}"#,
            r#"{"id":"x","prompt":"p","unknown":1}"#,
        ] {
            std::fs::write(&path, bad).unwrap();
            assert!(Spec::read(&path).is_err(), "{bad}");
        }
    }

    #[test]
    fn store_adds_lists_reviews_and_recovers_stopped_jobs() {
        let data = tempfile::tempdir().unwrap();
        let project = tempfile::tempdir().unwrap();
        let store = Store::open(data.path(), project.path()).unwrap();
        store.add(spec("a")).unwrap();
        assert!(store.add(spec("a")).is_err());
        assert_eq!(store.specs().unwrap().len(), 1);
        let id = new_id();
        store
            .change(|state| {
                state.results.push(InboxResult {
                    id: id.clone(),
                    task: "a".into(),
                    trigger: "manual".into(),
                    state: RunState::Running,
                    review: Decision::Pending,
                    started: now(),
                    finished: None,
                    reviewed_at: None,
                    attempts: 1,
                    baseline: None,
                    checks: vec![],
                    patch_sha256: String::new(),
                    patch_bytes: 0,
                    cost_usd: None,
                    error: None,
                });
                Ok(())
            })
            .unwrap();
        // Nobody holds the job lock, so a running result is from a stopped process.
        assert_eq!(store.result(&id).unwrap().state, RunState::Uncertain);
        let sets = store.changesets().unwrap();
        assert!(sets[0].not_applicable.is_some());
        store.review(&id, Decision::Rejected).unwrap();
        assert_eq!(store.result(&id).unwrap().review, Decision::Rejected);
        store.remove("a").unwrap();
        assert!(store.remove("a").is_err());
        assert_eq!(store.results().unwrap().len(), 1);
    }

    #[tokio::test]
    async fn non_git_projects_are_refused_before_a_worker_starts() {
        let data = tempfile::tempdir().unwrap();
        let project = tempfile::tempdir().unwrap();
        let store = Store::open(data.path(), project.path()).unwrap();
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
        let cancel = CancellationToken::new();
        assert!(
            run_job(&store, &spec("a"), Trigger::Manual, &config, &cancel)
                .await
                .is_err()
        );
        assert!(store.results().unwrap().is_empty());
    }
}
