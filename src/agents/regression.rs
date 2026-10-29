//! `regression` tool: proves a test catches the defect a change fixed.
//!
//! The listed production files are temporarily restored to their baseline (the task
//! checkpoint's before-image, or a git revision), the selected tests run and must really
//! fail; then the current content is restored and the same tests must pass. Build errors,
//! skipped or absent tests and timeouts are `unverified`, never proof. The swap is journaled
//! through [`Workspace::swap_in`] before any file changes, so a crash leaves a journal that
//! `/resolve` (or `/status agents repair`) uses to put the current content back.

use std::{
    collections::BTreeMap,
    path::{Component, Path, PathBuf},
    sync::{Arc, Mutex},
    time::Duration,
};

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Progress, Result, ToolDefinition},
    process,
    safety::permission::{Authorizer, Effect},
    session::workspace::Workspace,
    tools::ToolOutput,
};

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum EvidenceStatus {
    Passed,
    Failed,
    Unverified,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct RunSummary {
    pub exit_code: Option<i32>,
    pub timed_out: bool,
    pub failed: Vec<String>,
    pub passed: Vec<String>,
    pub output_tail: String,
}

/// Regression evidence. This is the struct an acceptance report consumes
/// (`AgentHub::evidence()` returns every record of the session).
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct RegressionEvidence {
    pub status: EvidenceStatus,
    pub reason: String,
    /// `cargo`, `go` or `command`.
    pub runner: String,
    pub selector: String,
    pub files: Vec<String>,
    /// `checkpoint` or `git:<rev>`.
    pub baseline: String,
    /// Tests that failed on the baseline and pass on the current code.
    pub tests: Vec<String>,
    pub before: RunSummary,
    pub after: RunSummary,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "lowercase")]
enum Runner {
    Cargo,
    Go,
    Command,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Args {
    files: Vec<String>,
    runner: Runner,
    filter: String,
    package: Option<String>,
    baseline: Option<String>,
    timeout_seconds: Option<u64>,
}

pub struct Regression {
    pub root: PathBuf,
    pub workspace: Option<Arc<Workspace>>,
    pub authorizer: Arc<dyn Authorizer>,
    pub progress: Option<Arc<Progress>>,
    /// Configured `regression_command`, with `{filter}` where the selector goes.
    pub command: Option<String>,
    pub evidence: Arc<Mutex<Vec<RegressionEvidence>>>,
}

pub fn definition() -> ToolDefinition {
    ToolDefinition {
        name: "regression".into(),
        description: "Regression verification (requires approval; runs project test code twice, not sandboxed). Temporarily restores the listed \
            production files to their baseline (default: content before this task changed them; or baseline=\"git:<rev>\"), requires the \
            selected tests to really fail there, restores the current files and requires the same tests to pass. runner=cargo runs \
            `cargo test <filter>`, runner=go runs `go test -json -run <filter> <package>`, runner=command runs the configured \
            regression_command. Tests must live outside the listed files. Build errors, skips and timeouts are unverified, not proof."
            .into(),
        schema: json!({"type":"object","properties":{
            "files":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":32,"description":"Project-relative production files changed by the fix"},
            "runner":{"enum":["cargo","go","command"]},
            "filter":{"type":"string","description":"Test name filter (cargo) or -run regex (go)"},
            "package":{"type":"string","description":"Go package, e.g. ./internal/foo (default ./...)"},
            "baseline":{"type":"string","description":"checkpoint (default) or git:<rev>"},
            "timeout_seconds":{"type":"integer","minimum":10,"maximum":1800}
        },"required":["files","runner","filter"],"additionalProperties":false}),
    }
}

fn is_test_file(path: &str) -> bool {
    let path = path.replace('\\', "/");
    let name = path.rsplit('/').next().unwrap_or_default();
    path.starts_with("tests/")
        || path.contains("/tests/")
        || name.ends_with("_test.go")
        || name.ends_with("_test.rs")
        || name.ends_with("_tests.rs")
        || name == "tests.rs"
}

fn relative(path: &str) -> Result<PathBuf> {
    let path = PathBuf::from(path);
    if path.as_os_str().is_empty()
        || !path
            .components()
            .all(|part| matches!(part, Component::Normal(_)))
        || path.to_string_lossy().contains(':')
    {
        return Err(Error::Tool(format!(
            "{} must be a project-relative path without '..'",
            path.display()
        )));
    }
    Ok(path)
}

/// Single-quoted argument that both PowerShell and sh take literally.
fn quoted(text: &str) -> Result<String> {
    if text.is_empty() || text.len() > 300 || text.chars().any(|c| c == '\'' || c.is_control()) {
        return Err(Error::Tool(
            "selector must be 1..300 characters without quotes or control characters".into(),
        ));
    }
    Ok(format!("'{text}'"))
}

fn tail(text: &str, limit: usize) -> String {
    if text.len() <= limit {
        return text.to_owned();
    }
    let mut start = text.len() - limit;
    while !text.is_char_boundary(start) {
        start += 1;
    }
    text[start..].to_owned()
}

/// `test <name> ... ok|FAILED` lines of the libtest harness.
pub fn cargo_outcomes(stdout: &str) -> BTreeMap<String, bool> {
    stdout
        .lines()
        .filter_map(|line| line.trim_end().strip_prefix("test "))
        .filter_map(|rest| rest.rsplit_once(" ... "))
        .filter_map(|(name, result)| match result.trim() {
            "ok" => Some((name.trim().to_owned(), true)),
            "FAILED" => Some((name.trim().to_owned(), false)),
            _ => None,
        })
        .collect()
}

/// pass/fail events of `go test -json`; a test timeout panic is never evidence.
pub fn go_outcomes(stdout: &str) -> std::result::Result<BTreeMap<String, bool>, String> {
    let mut outcomes = BTreeMap::new();
    for line in stdout.lines().filter(|line| line.starts_with('{')) {
        let event: Value =
            serde_json::from_str(line).map_err(|err| format!("invalid go test JSON: {err}"))?;
        let field = |name: &str| event.get(name).and_then(Value::as_str).unwrap_or_default();
        if field("Action") == "output" && field("Output").contains("panic: test timed out after") {
            return Err("go test timed out; no completed evidence".into());
        }
        if !field("Test").is_empty() && matches!(field("Action"), "pass" | "fail") {
            outcomes.insert(
                format!("{}/{}", field("Package"), field("Test")),
                field("Action") == "pass",
            );
        }
    }
    Ok(outcomes)
}

struct Run {
    output: process::CommandOutput,
    outcomes: std::result::Result<BTreeMap<String, bool>, String>,
}

fn build_failed(runner: Runner, output: &process::CommandOutput) -> bool {
    match runner {
        Runner::Cargo => {
            output.stderr.contains("error[E") || output.stderr.contains("could not compile")
        }
        Runner::Go => {
            output.stdout.contains("[build failed]") || output.stderr.contains("[setup failed]")
        }
        Runner::Command => false,
    }
}

fn summary(run: &Run) -> RunSummary {
    let outcomes = run.outcomes.as_ref().ok();
    let names = |passed: bool| {
        outcomes
            .map(|map| {
                map.iter()
                    .filter(|(_, ok)| **ok == passed)
                    .map(|(name, _)| name.clone())
                    .collect()
            })
            .unwrap_or_default()
    };
    RunSummary {
        exit_code: run.output.code,
        timed_out: run.output.timed_out,
        failed: names(false),
        passed: names(true),
        output_tail: tail(
            format!("{}\n{}", run.output.stdout, run.output.stderr).trim(),
            1500,
        ),
    }
}

/// Status and reason for a baseline run and a current run of the same selector.
fn compare(runner: Runner, before: &Run, after: &Run) -> (EvidenceStatus, String, Vec<String>) {
    use EvidenceStatus::*;
    for (label, run) in [("baseline", before), ("current", after)] {
        if run.output.timed_out {
            return (Unverified, format!("{label} run timed out"), Vec::new());
        }
        if build_failed(runner, &run.output) {
            return (
                Unverified,
                format!("{label} run failed to build; no test ran"),
                Vec::new(),
            );
        }
        if let Err(err) = &run.outcomes {
            return (Unverified, err.clone(), Vec::new());
        }
    }
    if after.output.code != Some(0) {
        return (
            Failed,
            "tests fail on the current implementation".into(),
            Vec::new(),
        );
    }
    if before.output.code == Some(0) {
        return (
            Failed,
            "tests also pass on the baseline; they do not catch the defect".into(),
            Vec::new(),
        );
    }
    if runner == Runner::Command {
        return (
            Passed,
            "configured command failed on the baseline and passes now (exit codes only; a build failure cannot be told apart)".into(),
            Vec::new(),
        );
    }
    let (Ok(old), Ok(new)) = (&before.outcomes, &after.outcomes) else {
        unreachable!("checked above");
    };
    let mut tests = Vec::new();
    for (name, _) in old.iter().filter(|(_, ok)| !**ok) {
        if new.get(name) != Some(&true) {
            return (
                Unverified,
                format!("{name} failed on the baseline but did not pass on the current code"),
                Vec::new(),
            );
        }
        tests.push(name.clone());
    }
    if tests.is_empty() {
        return (
            Unverified,
            "no test actually failed on the baseline (absent, skipped or not selected)".into(),
            Vec::new(),
        );
    }
    (
        Passed,
        "the same tests fail on the baseline and pass on the current implementation".into(),
        tests,
    )
}

impl Regression {
    fn command(&self, args: &Args) -> Result<String> {
        Ok(match args.runner {
            Runner::Cargo => format!(
                "cargo test --no-fail-fast {} -- --color never",
                quoted(&args.filter)?
            ),
            Runner::Go => {
                let package = args.package.as_deref().unwrap_or("./...");
                if !(package == "." || package.starts_with("./"))
                    || package
                        .split('/')
                        .any(|part| part == ".." || part == ".git")
                {
                    return Err(Error::Tool("package must be project-relative".into()));
                }
                format!(
                    "go test -json -count=1 -run {} {}",
                    quoted(&args.filter)?,
                    quoted(package)?
                )
            }
            Runner::Command => {
                let template = self.command.as_deref().ok_or_else(|| {
                    Error::Tool("runner=command needs regression_command in the config".into())
                })?;
                template.replace("{filter}", &quoted(&args.filter)?)
            }
        })
    }

    async fn baselines(
        &self,
        args: &Args,
        files: &[PathBuf],
        cancel: &CancellationToken,
    ) -> Result<Vec<(PathBuf, String)>> {
        let mut out = Vec::new();
        match args.baseline.as_deref().unwrap_or("checkpoint") {
            "checkpoint" => {
                let workspace = self.workspace.as_ref().ok_or_else(|| {
                    Error::Tool("no task checkpoint here; pass baseline=\"git:<rev>\"".into())
                })?;
                for file in files {
                    let baseline = workspace.baseline(file)?.ok_or_else(|| {
                        Error::Tool(format!(
                            "{} was created by this task; there is no earlier version to test against",
                            file.display()
                        ))
                    })?;
                    out.push((file.clone(), baseline));
                }
            }
            other => {
                let rev = other.strip_prefix("git:").ok_or_else(|| {
                    Error::Tool("baseline must be checkpoint or git:<rev>".into())
                })?;
                if rev.is_empty()
                    || rev.starts_with('-')
                    || !rev.chars().all(|c| {
                        c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '/' | '~' | '^' | '-')
                    })
                {
                    return Err(Error::Tool("invalid git revision".into()));
                }
                for file in files {
                    let spec = format!("{rev}:{}", file.to_string_lossy().replace('\\', "/"));
                    let output =
                        crate::agents::worktree::git(&self.root, &["show", &spec], cancel).await?;
                    if !output.ok {
                        return Err(Error::Tool(format!(
                            "git show {spec} failed: {}",
                            output.text()
                        )));
                    }
                    out.push((file.clone(), output.stdout));
                }
            }
        }
        Ok(out)
    }

    async fn run(
        &self,
        runner: Runner,
        command: &str,
        label: &str,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<Run> {
        if let Some(progress) = &self.progress {
            progress
                .line(format!("[regression:{label}] $ {command}"), false)
                .await;
        }
        let output = process::shell_live(
            &self.root,
            command,
            timeout,
            cancel.clone(),
            self.progress.clone(),
        )
        .await?;
        let outcomes = match runner {
            Runner::Cargo => Ok(cargo_outcomes(&output.stdout)),
            Runner::Go => go_outcomes(&output.stdout),
            Runner::Command => Ok(BTreeMap::new()),
        };
        Ok(Run { output, outcomes })
    }

    pub async fn execute(&self, input: Value, cancel: CancellationToken) -> Result<ToolOutput> {
        let failed = |content: String| {
            Ok(ToolOutput {
                content,
                failed: true,
                ..Default::default()
            })
        };
        let args: Args = match serde_json::from_value(input) {
            Ok(args) => args,
            Err(err) => return failed(err.to_string()),
        };
        let prepared = async {
            if args.files.is_empty() || args.files.len() > 32 {
                return Err(Error::Tool("list 1..32 production files".into()));
            }
            let mut files = Vec::new();
            for file in &args.files {
                if is_test_file(file) {
                    return Err(Error::Tool(format!(
                        "{file} is a test file; list only the production files the fix changed (tests must stay current)"
                    )));
                }
                files.push(relative(file)?);
            }
            let command = self.command(&args)?;
            let baselines = self.baselines(&args, &files, &cancel).await?;
            Ok((command, baselines))
        }
        .await;
        let (command, baselines) = match prepared {
            Ok(prepared) => prepared,
            Err(err @ (Error::Cancelled | Error::Uncertain(_))) => return Err(err),
            Err(err) => return failed(err.to_string()),
        };
        self.authorizer
            .authorize(
                Effect::Shell(format!(
                    "regression verification: {command} (twice; temporarily restores {})",
                    args.files.join(", ")
                )),
                cancel.clone(),
            )
            .await?;
        let Some(workspace) = self.workspace.clone() else {
            return failed("regression verification needs a workspace journal here".into());
        };
        let _ = workspace.note_shell();
        let timeout = Duration::from_secs(args.timeout_seconds.unwrap_or(300).clamp(10, 1800));
        if let Err(err) = workspace.swap_in(baselines) {
            return match err {
                Error::Uncertain(_) => Err(err),
                err => failed(format!("could not restore the baseline: {err}")),
            };
        }
        let before = self
            .run(args.runner, &command, "baseline", timeout, &cancel)
            .await;
        // Always put the current content back before anything else, cancellation included.
        let restored = workspace.swap_restore();
        if let Err(err) = restored {
            return Err(Error::Uncertain(format!(
                "regression verification could not restore current files: {err}"
            )));
        }
        let before = before?;
        let after = self
            .run(args.runner, &command, "current", timeout, &cancel)
            .await?;
        let (status, reason, tests) = compare(args.runner, &before, &after);
        let evidence = RegressionEvidence {
            status,
            reason,
            runner: format!("{:?}", args.runner).to_lowercase(),
            selector: args.filter.clone(),
            files: args.files.clone(),
            baseline: args.baseline.clone().unwrap_or_else(|| "checkpoint".into()),
            tests,
            before: summary(&before),
            after: summary(&after),
        };
        if let Ok(mut all) = self.evidence.lock() {
            all.push(evidence.clone());
            let excess = all.len().saturating_sub(64);
            all.drain(..excess);
        }
        Ok(ToolOutput {
            content: format!(
                "regression evidence: {}\n{}",
                format!("{status:?}").to_lowercase(),
                serde_json::to_string_pretty(&evidence)?
            ),
            failed: status != EvidenceStatus::Passed,
            ..Default::default()
        })
    }
}

/// Repairs a regression swap a crash left behind; used by `/resolve` and `/status agents repair`.
pub fn repair(workspace: &Workspace) -> Result<Option<String>> {
    if workspace.pending_swap().is_none() {
        return Ok(None);
    }
    workspace.swap_restore().map(Some)
}

pub fn pending_files(workspace: &Workspace) -> Option<String> {
    workspace.pending_swap().map(|files| {
        files
            .iter()
            .map(|file| Path::new(file).display().to_string())
            .collect::<Vec<_>>()
            .join(", ")
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn output(code: i32, stdout: &str, stderr: &str) -> process::CommandOutput {
        process::CommandOutput {
            code: Some(code),
            stdout: stdout.into(),
            stderr: stderr.into(),
            truncated: false,
            timed_out: false,
            encoding_warning: false,
        }
    }

    fn cargo(code: i32, stdout: &str, stderr: &str) -> Run {
        Run {
            outcomes: Ok(cargo_outcomes(stdout)),
            output: output(code, stdout, stderr),
        }
    }

    #[test]
    fn cargo_evidence_requires_real_failures_then_passes() {
        let before = cargo(
            101,
            "test a::fixes_bug ... FAILED\ntest a::other ... ok\n",
            "",
        );
        let after = cargo(0, "test a::fixes_bug ... ok\ntest a::other ... ok\n", "");
        let (status, _, tests) = compare(Runner::Cargo, &before, &after);
        assert_eq!(status, EvidenceStatus::Passed);
        assert_eq!(tests, ["a::fixes_bug"]);

        let build = cargo(101, "", "error[E0425]: cannot find value\n");
        assert_eq!(
            compare(Runner::Cargo, &build, &after).0,
            EvidenceStatus::Unverified
        );
        let nothing = cargo(101, "running 0 tests\n", "");
        assert_eq!(
            compare(Runner::Cargo, &nothing, &after).0,
            EvidenceStatus::Unverified
        );
        let also_pass = cargo(0, "test a::fixes_bug ... ok\n", "");
        assert_eq!(
            compare(Runner::Cargo, &also_pass, &after).0,
            EvidenceStatus::Failed
        );
        let still_fails = cargo(101, "test a::fixes_bug ... FAILED\n", "");
        assert_eq!(
            compare(Runner::Cargo, &before, &still_fails).0,
            EvidenceStatus::Failed
        );
        let mut timeout = cargo(101, "test a::fixes_bug ... FAILED\n", "");
        timeout.output.timed_out = true;
        assert_eq!(
            compare(Runner::Cargo, &timeout, &after).0,
            EvidenceStatus::Unverified
        );
        let ignored = cargo(0, "test a::fixes_bug ... ignored\n", "");
        assert!(cargo_outcomes(&ignored.output.stdout).is_empty());
    }

    #[test]
    fn go_events_and_timeouts() {
        let json = "{\"Action\":\"fail\",\"Package\":\"p\",\"Test\":\"TestX\"}\n{\"Action\":\"skip\",\"Package\":\"p\",\"Test\":\"TestY\"}\n";
        assert_eq!(go_outcomes(json).unwrap().get("p/TestX"), Some(&false));
        assert!(!go_outcomes(json).unwrap().contains_key("p/TestY"));
        let timeout = "{\"Action\":\"output\",\"Output\":\"panic: test timed out after 1m\"}\n";
        assert!(go_outcomes(timeout).is_err());
    }

    #[test]
    fn selectors_and_files_are_validated() {
        assert!(quoted("it's").is_err());
        assert_eq!(quoted("a::b|c").unwrap(), "'a::b|c'");
        assert!(is_test_file("tests/agents.rs"));
        assert!(is_test_file("pkg/x_test.go"));
        assert!(is_test_file("src/foo/tests.rs"));
        assert!(!is_test_file("src/lib.rs"));
        assert!(relative("../x.rs").is_err());
        assert!(relative("src/lib.rs").is_ok());
    }

    #[test]
    fn swap_is_journaled_and_restored_after_a_crash() {
        let data = tempfile::tempdir().unwrap();
        let root = tempfile::tempdir().unwrap();
        let store = crate::session::Store::open(data.path()).unwrap();
        std::fs::write(root.path().join("lib.rs"), "old\n").unwrap();
        let workspace = Workspace::new(root.path().into(), store.clone()).unwrap();
        workspace.begin().unwrap();
        workspace
            .replace(
                &root.path().canonicalize().unwrap().join("lib.rs"),
                Some("old\n"),
                "new\n",
            )
            .unwrap();
        let baseline = workspace.baseline(Path::new("lib.rs")).unwrap().unwrap();
        assert_eq!(baseline, "old\n");
        workspace
            .swap_in(vec![(PathBuf::from("lib.rs"), baseline)])
            .unwrap();
        assert_eq!(
            std::fs::read_to_string(root.path().join("lib.rs")).unwrap(),
            "old\n"
        );
        // Edits are refused while the file holds its baseline.
        assert!(
            workspace
                .replace(
                    &root.path().canonicalize().unwrap().join("lib.rs"),
                    Some("old\n"),
                    "x"
                )
                .is_err()
        );
        // "Crash": a fresh Workspace sees the journal and repairs it.
        drop(workspace);
        let reopened = Workspace::new(root.path().into(), store).unwrap();
        assert!(pending_files(&reopened).unwrap().contains("lib.rs"));
        assert!(repair(&reopened).unwrap().is_some());
        assert_eq!(
            std::fs::read_to_string(root.path().join("lib.rs")).unwrap(),
            "new\n"
        );
        assert!(reopened.pending_swap().is_none());
        assert!(repair(&reopened).unwrap().is_none());
    }

    #[test]
    fn restore_refuses_to_overwrite_foreign_changes() {
        let data = tempfile::tempdir().unwrap();
        let root = tempfile::tempdir().unwrap();
        let store = crate::session::Store::open(data.path()).unwrap();
        std::fs::write(root.path().join("a.rs"), "new\n").unwrap();
        let workspace = Workspace::new(root.path().into(), store).unwrap();
        workspace
            .swap_in(vec![(PathBuf::from("a.rs"), "old\n".into())])
            .unwrap();
        std::fs::write(root.path().join("a.rs"), "someone else\n").unwrap();
        assert!(matches!(workspace.swap_restore(), Err(Error::Uncertain(_))));
        assert!(workspace.pending_swap().is_some());
    }
}
