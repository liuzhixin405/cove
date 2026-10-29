use std::{
    io::Write as _,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use futures_util::future::BoxFuture;
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Progress, Result},
    process,
    safety::gates::{CheckRecord, Evidence},
    safety::permission::{Authorizer, Effect},
    session::workspace::Workspace,
};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Outcome {
    Passed,
    Failed,
    /// Approval denied, no shell available or a check timed out; never counted as passing.
    NotRun,
}

impl Outcome {
    pub fn name(self) -> &'static str {
        match self {
            Self::Passed => "passed",
            Self::Failed => "failed",
            Self::NotRun => "not_run",
        }
    }
}

pub struct Verification {
    pub outcome: Outcome,
    /// One line per command, for the user.
    pub summary: String,
    /// Failing command output, for the model.
    pub detail: String,
}

/// Checks a task's changes before the agent may report it finished.
pub trait Verifier: Send + Sync {
    /// `Ok(None)` when nothing changed since the last verification.
    fn verify(&self, cancel: CancellationToken) -> BoxFuture<'_, Result<Option<Verification>>>;
}

/// Whether the user trusts a project enough to run its auto-detected checks unprompted.
/// The trust store belongs to project settings; until one is wired in, nothing is trusted.
pub trait ProjectTrust: Send + Sync {
    fn trusted(&self, root: &Path) -> bool;
}

pub struct Untrusted;

impl ProjectTrust for Untrusted {
    fn trusted(&self, _root: &Path) -> bool {
        false
    }
}

fn on_path(program: &str) -> bool {
    let Some(paths) = std::env::var_os("PATH") else {
        return false;
    };
    let suffixes: &[&str] = if cfg!(windows) {
        &[".exe", ".cmd", ".bat"]
    } else {
        &[""]
    };
    std::env::split_paths(&paths).any(|dir| {
        suffixes
            .iter()
            .any(|suffix| dir.join(format!("{program}{suffix}")).is_file())
    })
}

fn package_script(root: &Path, name: &str) -> Option<String> {
    let text = std::fs::read_to_string(root.join("package.json")).ok()?;
    let value: serde_json::Value = serde_json::from_str(&text).ok()?;
    value
        .pointer(&format!("/scripts/{name}"))
        .and_then(|s| s.as_str())
        .map(str::to_owned)
}

fn python(root: &Path) -> bool {
    root.join("pyproject.toml").is_file() || root.join("setup.py").is_file()
}

fn python_program() -> Option<&'static str> {
    ["python", "python3"].into_iter().find(|p| on_path(p))
}

/// Build/test commands for the project kind found at `root`.
pub fn detect(root: &Path) -> Vec<String> {
    let has = |name: &str| root.join(name).is_file();
    let mut commands = Vec::new();
    if has("Cargo.toml") {
        commands.push("cargo check --all-targets".into());
    }
    if has("go.mod") {
        commands.push("go build ./...".into());
    }
    if has("tsconfig.json") && root.join("node_modules/typescript").is_dir() {
        commands.push("npx tsc --noEmit".into());
    }
    if package_script(root, "build").is_some() && root.join("node_modules").is_dir() {
        commands.push("npm run build --if-present".into());
    }
    if python(root)
        && let Some(python) = python_program()
    {
        commands.push(format!("{python} -m compileall -q ."));
    }
    if dotnet(root) {
        commands.push("dotnet build".into());
    }
    commands
}

fn dotnet(root: &Path) -> bool {
    std::fs::read_dir(root).is_ok_and(|entries| {
        entries.flatten().any(|entry| {
            let name = entry.file_name();
            let name = name.to_string_lossy();
            name.ends_with(".sln") || name.ends_with(".csproj")
        })
    })
}

/// `name = "..."` of the `[package]` table of a Cargo manifest.
fn cargo_package(manifest: &Path) -> Option<String> {
    let text = std::fs::read_to_string(manifest).ok()?;
    let mut in_package = false;
    for line in text.lines() {
        let line = line.trim();
        if line.starts_with('[') {
            in_package = line == "[package]";
            continue;
        }
        if in_package
            && let Some(value) = line
                .strip_prefix("name")
                .map(str::trim_start)
                .and_then(|rest| rest.strip_prefix('='))
        {
            return Some(value.trim().trim_matches('"').to_owned());
        }
    }
    None
}

/// Test commands for the project, scoped to the packages the changed files belong to.
pub fn test_commands(root: &Path, changed: &[PathBuf]) -> Vec<String> {
    let mut commands = Vec::new();
    if root.join("Cargo.toml").is_file() {
        let workspace = std::fs::read_to_string(root.join("Cargo.toml"))
            .is_ok_and(|t| t.lines().any(|l| l.trim() == "[workspace]"));
        let mut packages: Vec<String> = Vec::new();
        let mut whole = !workspace || changed.is_empty();
        for path in changed {
            let mut dir = root.join(path).parent().map(Path::to_path_buf);
            let mut found = None;
            while let Some(current) = dir {
                if !current.starts_with(root) {
                    break;
                }
                let manifest = current.join("Cargo.toml");
                if manifest.is_file() {
                    found = Some((current.clone(), cargo_package(&manifest)));
                    break;
                }
                dir = current.parent().map(Path::to_path_buf);
            }
            match found {
                Some((dir, Some(name))) if dir != root => {
                    if !packages.contains(&name) {
                        packages.push(name);
                    }
                }
                _ => whole = true,
            }
        }
        commands.push(if whole || packages.is_empty() {
            "cargo test".into()
        } else {
            format!(
                "cargo test {}",
                packages
                    .iter()
                    .map(|p| format!("-p {p}"))
                    .collect::<Vec<_>>()
                    .join(" ")
            )
        });
    }
    if root.join("go.mod").is_file() {
        let mut dirs: Vec<String> = changed
            .iter()
            .filter(|p| p.extension().is_some_and(|e| e == "go"))
            .filter_map(|p| p.parent())
            .map(|dir| {
                let dir = dir.to_string_lossy().replace('\\', "/");
                if dir.is_empty() {
                    ".".to_owned()
                } else {
                    format!("./{dir}")
                }
            })
            .collect();
        dirs.sort();
        dirs.dedup();
        commands.push(if dirs.is_empty() || dirs.len() > 20 {
            "go test ./...".into()
        } else {
            format!("go test {}", dirs.join(" "))
        });
    }
    if dotnet(root) {
        commands.push("dotnet test".into());
    }
    if let Some(script) = package_script(root, "test")
        && !script.contains("no test specified")
        && root.join("node_modules").is_dir()
    {
        commands.push("npm test".into());
    }
    let pytest = ["pytest.ini", "conftest.py", "tox.ini"]
        .iter()
        .any(|f| root.join(f).is_file())
        || std::fs::read_to_string(root.join("pyproject.toml"))
            .is_ok_and(|t| t.contains("[tool.pytest"))
        || (python(root) && root.join("tests").is_dir());
    if pytest && let Some(python) = python_program() {
        commands.push(format!("{python} -m pytest -q"));
    }
    commands
}

/// Whether auto-detected checks may run without asking (trusted project, auto/bypass mode).
pub type Preapproved = Arc<dyn Fn() -> bool + Send + Sync>;

pub struct CommandVerifier {
    root: PathBuf,
    commands: Vec<String>,
    timeout: Option<Duration>,
    workspace: Arc<Workspace>,
    authorizer: Arc<dyn Authorizer>,
    verified: Mutex<Option<u64>>,
    progress: Option<Arc<Progress>>,
    /// Commands were detected, not configured: tests may join and pre-approval applies.
    detected: Option<(bool, Preapproved)>,
    evidence: Option<Arc<Evidence>>,
    ledger: Option<PathBuf>,
}

impl CommandVerifier {
    pub fn new(
        root: PathBuf,
        commands: Vec<String>,
        timeout: Option<Duration>,
        workspace: Arc<Workspace>,
        authorizer: Arc<dyn Authorizer>,
    ) -> Self {
        Self {
            root,
            commands,
            timeout,
            workspace,
            authorizer,
            verified: Mutex::new(None),
            progress: None,
            detected: None,
            evidence: None,
            ledger: None,
        }
    }

    pub fn with_progress(mut self, progress: Arc<Progress>) -> Self {
        self.progress = Some(progress);
        self
    }

    /// Marks the commands as auto-detected: `tests` adds the project's tests for the
    /// changed files; `preapproved` decides whether they run without a prompt.
    pub fn with_detection(mut self, tests: bool, preapproved: Preapproved) -> Self {
        self.detected = Some((tests, preapproved));
        self
    }

    /// Successful runs the model made after its last edit count as passed.
    pub fn with_evidence(mut self, evidence: Arc<Evidence>) -> Self {
        self.evidence = Some(evidence);
        self
    }

    /// Appends one JSON line per check to `path`.
    pub fn with_ledger(mut self, path: PathBuf) -> Self {
        self.ledger = Some(path);
        self
    }

    pub fn commands(&self) -> &[String] {
        &self.commands
    }

    /// Whether a pre-commit gate has anything to run here.
    pub fn enabled(&self) -> bool {
        !self.commands.is_empty()
            || self
                .detected
                .as_ref()
                .is_some_and(|(tests, _)| *tests && !test_commands(&self.root, &[]).is_empty())
    }

    fn turn_commands(&self, changed: &[PathBuf]) -> Vec<String> {
        let mut commands = self.commands.clone();
        if let Some((true, _)) = &self.detected {
            for command in test_commands(&self.root, changed) {
                if !commands.contains(&command) {
                    commands.push(command);
                }
            }
        }
        commands
    }

    /// Configured limit, else 120 s and 300 s for the slower test/dotnet/npm toolchains.
    fn timeout(&self, command: &str) -> Duration {
        self.timeout.unwrap_or_else(|| {
            let command = command.trim_start();
            let slow = [
                "dotnet",
                "npm",
                "npx",
                "cargo test",
                "go test",
                "python -m pytest",
                "python3 -m pytest",
            ]
            .iter()
            .any(|tool| command.starts_with(tool));
            Duration::from_secs(if slow { 300 } else { 120 })
        })
    }

    fn record(&self, check: CheckRecord) {
        if let Some(path) = &self.ledger {
            let line = serde_json::json!({
                "time": crate::session::now(),
                "cwd": self.root,
                "command": check.command,
                "status": check.status,
                "exit_code": check.exit_code,
                "duration_ms": check.duration_ms,
            });
            let _ = std::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(path)
                .and_then(|mut file| writeln!(file, "{line}"));
        }
        if let Some(evidence) = &self.evidence {
            evidence.record_check(check);
        }
    }

    /// Runs the checks for a `git commit`, whatever the file-tool fingerprint says.
    pub async fn verify_commit(
        &self,
        paths: &[PathBuf],
        cancel: CancellationToken,
    ) -> Result<Verification> {
        let commands = self.turn_commands(paths);
        let verification = self.run(&commands, cancel).await?;
        if let Some(evidence) = &self.evidence {
            evidence.set_outcome(verification.outcome.name());
        }
        Ok(verification)
    }

    async fn run(&self, commands: &[String], cancel: CancellationToken) -> Result<Verification> {
        let mut summary = String::new();
        let mut detail = String::new();
        let mut outcome = Outcome::Passed;
        let preapproved = self
            .detected
            .as_ref()
            .is_some_and(|(_, preapproved)| preapproved());
        for command in commands {
            if self
                .evidence
                .as_ref()
                .is_some_and(|evidence| evidence.passed(command))
            {
                summary.push_str(&format!(
                    "passed   {command} (already run successfully after the last edit)\n"
                ));
                self.record(CheckRecord {
                    command: command.clone(),
                    status: "reused".into(),
                    exit_code: Some(0),
                    duration_ms: 0,
                    evidence: String::new(),
                });
                continue;
            }
            if let Some(progress) = &self.progress {
                progress.line(format!("[验证] $ {command}"), false).await;
            }
            if !preapproved {
                match self
                    .authorizer
                    .authorize(Effect::Shell(command.clone()), cancel.clone())
                    .await
                {
                    Ok(()) => {}
                    Err(Error::Approval(reason)) => {
                        summary.push_str(&format!("not run  {command} ({reason})\n"));
                        self.record(CheckRecord {
                            command: command.clone(),
                            status: "not_run".into(),
                            exit_code: None,
                            duration_ms: 0,
                            evidence: reason,
                        });
                        outcome = Outcome::NotRun;
                        break;
                    }
                    Err(err) => return Err(err),
                }
            }
            let _ = self.workspace.note_shell();
            let timeout = self.timeout(command);
            let started = Instant::now();
            let output = process::shell_live(
                &self.root,
                command,
                timeout,
                cancel.clone(),
                self.progress.clone(),
            )
            .await?;
            let duration_ms = started.elapsed().as_millis();
            if output.code == Some(0) && !output.timed_out {
                summary.push_str(&format!("passed   {command}\n"));
                self.record(CheckRecord {
                    command: command.clone(),
                    status: "passed".into(),
                    exit_code: Some(0),
                    duration_ms,
                    evidence: String::new(),
                });
                continue;
            }
            let text = format!("{}\n{}", output.stdout, output.stderr);
            let evidence = tail(text.trim(), 2048).to_owned();
            if output.timed_out {
                // A slow check says nothing about the change: not verified, not failed.
                summary.push_str(&format!(
                    "not run  {command} (timed out after {}s; not verified)\n",
                    timeout.as_secs()
                ));
                self.record(CheckRecord {
                    command: command.clone(),
                    status: "timed_out".into(),
                    exit_code: None,
                    duration_ms,
                    evidence,
                });
                if outcome == Outcome::Passed {
                    outcome = Outcome::NotRun;
                }
                continue;
            }
            let status = format!("exit_code={:?}", output.code);
            summary.push_str(&format!("failed   {command} ({status})\n"));
            detail.push_str(&format!(
                "$ {command}\n{status}\n{}\n",
                tail(text.trim(), DETAIL_LIMIT)
            ));
            self.record(CheckRecord {
                command: command.clone(),
                status: "failed".into(),
                exit_code: output.code,
                duration_ms,
                evidence,
            });
            outcome = Outcome::Failed;
            break;
        }
        Ok(Verification {
            outcome,
            summary,
            detail,
        })
    }
}

const DETAIL_LIMIT: usize = 12 * 1024;

fn tail(text: &str, limit: usize) -> &str {
    if text.len() <= limit {
        return text;
    }
    let mut start = text.len() - limit;
    while !text.is_char_boundary(start) {
        start += 1;
    }
    &text[start..]
}

impl Verifier for CommandVerifier {
    fn verify(&self, cancel: CancellationToken) -> BoxFuture<'_, Result<Option<Verification>>> {
        Box::pin(async move {
            let Some(fingerprint) = self.workspace.fingerprint()? else {
                return Ok(None);
            };
            let lock = |verified: &Mutex<Option<u64>>| {
                verified
                    .lock()
                    .map(|guard| *guard)
                    .map_err(|_| Error::Tool("verification lock poisoned".into()))
            };
            let commands = self.turn_commands(&self.workspace.changed_paths()?);
            if commands.is_empty() || lock(&self.verified)? == Some(fingerprint) {
                return Ok(None);
            }
            let verification = self.run(&commands, cancel).await?;
            if verification.outcome == Outcome::Passed {
                *self
                    .verified
                    .lock()
                    .map_err(|_| Error::Tool("verification lock poisoned".into()))? =
                    Some(fingerprint);
            }
            if let Some(evidence) = &self.evidence {
                evidence.set_outcome(verification.outcome.name());
            }
            Ok(Some(verification))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn detects_project_kinds() {
        let dir = tempfile::tempdir().unwrap();
        assert!(detect(dir.path()).is_empty());
        std::fs::write(dir.path().join("Cargo.toml"), "").unwrap();
        std::fs::write(dir.path().join("app.csproj"), "").unwrap();
        assert_eq!(
            detect(dir.path()),
            ["cargo check --all-targets", "dotnet build"]
        );
    }

    #[test]
    fn npm_build_script_needs_installed_dependencies() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(
            dir.path().join("package.json"),
            r#"{"scripts":{"build":"tsc","test":"echo \"Error: no test specified\" && exit 1"}}"#,
        )
        .unwrap();
        assert!(detect(dir.path()).is_empty());
        std::fs::create_dir(dir.path().join("node_modules")).unwrap();
        assert_eq!(detect(dir.path()), ["npm run build --if-present"]);
        assert!(test_commands(dir.path(), &[]).is_empty());
    }

    #[test]
    fn cargo_tests_are_scoped_to_changed_workspace_members() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        std::fs::write(
            root.join("Cargo.toml"),
            "[workspace]\nmembers = [\"crates/*\"]\n",
        )
        .unwrap();
        for name in ["alpha", "beta"] {
            std::fs::create_dir_all(root.join(format!("crates/{name}/src"))).unwrap();
            std::fs::write(
                root.join(format!("crates/{name}/Cargo.toml")),
                format!("[package]\nname = \"{name}\"\n"),
            )
            .unwrap();
        }
        let changed = [
            PathBuf::from("crates/alpha/src/lib.rs"),
            PathBuf::from("crates/beta/src/lib.rs"),
        ];
        assert_eq!(
            test_commands(root, &changed),
            ["cargo test -p alpha -p beta"]
        );
        assert_eq!(
            test_commands(root, &[PathBuf::from("README.md")]),
            ["cargo test"]
        );
        std::fs::write(root.join("go.mod"), "module x").unwrap();
        let changed = [PathBuf::from("pkg/a/a.go"), PathBuf::from("main.go")];
        assert_eq!(test_commands(root, &changed)[1], "go test . ./pkg/a");
    }

    #[test]
    fn tail_keeps_char_boundaries() {
        assert_eq!(tail("ab界cd", 4), "cd");
        assert_eq!(tail("abc", 10), "abc");
    }
}
