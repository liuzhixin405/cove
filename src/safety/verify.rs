use std::{
    collections::HashSet,
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
    if has("tsconfig.json")
        && root.join("node_modules/typescript").is_dir()
        // A `tsc` build script already type-checks.
        && !package_script(root, "build").is_some_and(|script| script.contains("tsc"))
    {
        commands.push("npx tsc --noEmit".into());
    }
    if package_script(root, "build").is_some() && root.join("node_modules").is_dir() {
        commands.push(match node_manager(root) {
            "npm" => "npm run build --if-present".into(),
            manager => format!("{manager} run build"),
        });
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
        commands.extend(dotnet_tests(root, changed));
    }
    if let Some(script) = package_script(root, "test")
        && !script.contains("no test specified")
        && root.join("node_modules").is_dir()
    {
        commands.push(node_tests(root, &script, changed));
    }
    let pytest = ["pytest.ini", "conftest.py", "tox.ini"]
        .iter()
        .any(|f| root.join(f).is_file())
        || std::fs::read_to_string(root.join("pyproject.toml"))
            .is_ok_and(|t| t.contains("[tool.pytest"))
        || (python(root) && root.join("tests").is_dir());
    if pytest && let Some(python) = python_program() {
        commands.push(match pytest_targets(root, changed) {
            Some(targets) => format!("{python} -m pytest -q {}", targets.join(" ")),
            None => format!("{python} -m pytest -q"),
        });
    }
    commands.extend(java_tests(root, changed));
    commands
}

/// `python -m compileall -q .` walks every directory, virtualenvs included: byte-compile
/// only the changed Python files, and skip it when none changed.
fn scope_compile(root: &Path, command: String, changed: &[PathBuf]) -> Option<String> {
    let Some(python) = command.strip_suffix(" -m compileall -q .") else {
        return Some(command);
    };
    if changed.is_empty() {
        return Some(command);
    }
    let files: Vec<String> = changed
        .iter()
        .filter(|path| path.extension().is_some_and(|e| e == "py") && root.join(path).is_file())
        .map(|path| shell_path(root, &root.join(path)))
        .collect();
    (!files.is_empty()).then(|| format!("{python} -m py_compile {}", files.join(" ")))
}

/// A path for a command line: relative to `root`, `/`-separated, quoted when it has spaces.
fn shell_path(root: &Path, path: &Path) -> String {
    let relative = path.strip_prefix(root).unwrap_or(path);
    let text = relative.to_string_lossy().replace('\\', "/");
    if text.contains(char::is_whitespace) {
        format!("\"{text}\"")
    } else {
        text
    }
}

/// `a/../b` → `b` without touching the file system.
fn normalize(path: &Path) -> PathBuf {
    let mut out = PathBuf::new();
    for component in path.components() {
        match component {
            std::path::Component::ParentDir => {
                out.pop();
            }
            std::path::Component::CurDir => {}
            other => out.push(other),
        }
    }
    out
}

/// Project files whose names `keep` accepts, skipping ignored, dependency and build output.
fn project_files(root: &Path, keep: impl Fn(&str) -> bool) -> Vec<PathBuf> {
    ignore::WalkBuilder::new(root)
        .require_git(false)
        .max_depth(Some(10))
        .filter_entry(|entry| {
            !matches!(
                entry.file_name().to_str(),
                Some("bin" | "obj" | "node_modules" | "target" | "venv" | ".venv" | "build" | "dist")
            )
        })
        .build()
        .flatten()
        .filter(|entry| entry.file_type().is_some_and(|kind| kind.is_file()))
        .filter(|entry| keep(&entry.file_name().to_string_lossy()))
        .take(5000)
        .map(ignore::DirEntry::into_path)
        .collect()
}

/// The nearest directory at or above `path` (inside `root`) for which `owns` holds.
fn owner(root: &Path, path: &Path, owns: impl Fn(&Path) -> bool) -> Option<PathBuf> {
    let mut dir = root.join(path).parent().map(Path::to_path_buf);
    while let Some(current) = dir {
        if !current.starts_with(root) {
            return None;
        }
        if owns(&current) {
            return Some(current);
        }
        dir = current.parent().map(Path::to_path_buf);
    }
    None
}

/// The package manager the lockfile names; `npm` without one.
fn node_manager(root: &Path) -> &'static str {
    if root.join("pnpm-lock.yaml").is_file() {
        "pnpm"
    } else if root.join("yarn.lock").is_file() {
        "yarn"
    } else if root.join("bun.lockb").is_file() || root.join("bun.lock").is_file() {
        "bun"
    } else {
        "npm"
    }
}

/// The test script, limited to tests related to the changed sources when it runs plain jest.
fn node_tests(root: &Path, script: &str, changed: &[PathBuf]) -> String {
    let manager = node_manager(root);
    let run = if manager == "bun" {
        "bun run test".to_owned()
    } else {
        format!("{manager} test")
    };
    let sources = !changed.is_empty()
        && changed.iter().all(|path| {
            path.extension().and_then(|e| e.to_str()).is_some_and(|e| {
                matches!(e, "js" | "jsx" | "ts" | "tsx" | "mjs" | "cjs" | "mts" | "cts")
            })
        });
    let jest = script.trim_start().starts_with("jest")
        && !script.contains("&&")
        && !script.contains(';')
        && !script.contains('|');
    if !(sources && jest) {
        return run;
    }
    let files: Vec<String> = changed
        .iter()
        .map(|path| shell_path(root, &root.join(path)))
        .collect();
    // `--` keeps the flags for the script rather than the package manager.
    format!("{run} -- --findRelatedTests {}", files.join(" "))
}

/// Test files for the changed Python files, or `None` to run the whole suite (a
/// non-Python change, `conftest.py`, or a module without a matching test file).
fn pytest_targets(root: &Path, changed: &[PathBuf]) -> Option<Vec<String>> {
    let python = |path: &&PathBuf| path.extension().is_some_and(|e| e == "py");
    if changed.is_empty() || !changed.iter().all(|path| python(&path)) {
        return None;
    }
    let test_name = |name: &str| {
        name.ends_with(".py") && (name.starts_with("test_") || name.ends_with("_test.py"))
    };
    let tests = project_files(root, test_name);
    let mut targets: Vec<String> = Vec::new();
    for path in changed {
        let name = path.file_name()?.to_string_lossy();
        if name == "conftest.py" {
            return None;
        }
        let found: Vec<&PathBuf> = if test_name(&name) {
            vec![]
        } else {
            let stem = path.file_stem()?.to_string_lossy();
            let wanted = [format!("test_{stem}.py"), format!("{stem}_test.py")];
            tests
                .iter()
                .filter(|test| {
                    test.file_name()
                        .is_some_and(|file| wanted.iter().any(|w| file.to_string_lossy() == *w))
                })
                .collect()
        };
        if test_name(&name) {
            if root.join(path).is_file() {
                targets.push(shell_path(root, &root.join(path)));
            }
        } else if found.is_empty() {
            return None;
        } else {
            targets.extend(found.iter().map(|test| shell_path(root, test)));
        }
    }
    targets.sort();
    targets.dedup();
    (!targets.is_empty()).then_some(targets)
}

/// `dotnet test` for the test projects that reference (directly or not) a changed
/// project; `dotnet build` of the changed projects when no test covers them.
fn dotnet_tests(root: &Path, changed: &[PathBuf]) -> Vec<String> {
    static REFERENCE: std::sync::LazyLock<regex::Regex> = std::sync::LazyLock::new(|| {
        regex::Regex::new(r#"(?i)<ProjectReference\s+Include\s*=\s*"([^"]+)""#).unwrap()
    });
    let whole = || vec!["dotnet test".to_owned()];
    if changed.is_empty() {
        return whole();
    }
    let projects = project_files(root, |name| {
        name.ends_with(".csproj") || name.ends_with(".fsproj") || name.ends_with(".vbproj")
    });
    let mut references = std::collections::HashMap::new();
    let mut tests = Vec::new();
    for project in &projects {
        let text = std::fs::read_to_string(project).unwrap_or_default();
        let dir = project.parent().unwrap_or(root);
        let refs: Vec<PathBuf> = REFERENCE
            .captures_iter(&text)
            .map(|captures| normalize(&dir.join(captures[1].replace('\\', "/"))))
            .collect();
        references.insert(normalize(project), refs);
        if text.contains("Microsoft.NET.Test.Sdk") || text.contains("<IsTestProject>true") {
            tests.push(normalize(project));
        }
    }
    let project_in = |dir: &Path| {
        projects
            .iter()
            .find(|project| project.parent() == Some(dir))
            .map(|project| normalize(project))
    };
    let mut owners = HashSet::new();
    for path in changed {
        let Some(dir) = owner(root, path, |dir| project_in(dir).is_some()) else {
            // Solution files, shared props, anything outside a project: test everything.
            return whole();
        };
        owners.extend(project_in(&dir));
    }
    let covers = |test: &PathBuf| {
        let mut seen = HashSet::new();
        let mut stack = vec![test.clone()];
        while let Some(project) = stack.pop() {
            if owners.contains(&project) {
                return true;
            }
            if seen.insert(project.clone()) {
                stack.extend(references.get(&project).into_iter().flatten().cloned());
            }
        }
        false
    };
    let mut selected: Vec<String> = tests
        .iter()
        .filter(|test| covers(test))
        .map(|test| format!("dotnet test {}", shell_path(root, test)))
        .collect();
    if selected.is_empty() {
        selected = owners
            .iter()
            .map(|project| format!("dotnet build {}", shell_path(root, project)))
            .collect();
    }
    selected.sort();
    selected
}

/// Maven/Gradle tests of the modules the changed files belong to.
fn java_tests(root: &Path, changed: &[PathBuf]) -> Option<String> {
    let has = |dir: &Path, names: &[&str]| names.iter().any(|name| dir.join(name).is_file());
    const GRADLE: &[&str] = &["build.gradle", "build.gradle.kts"];
    let (maven, gradle) = (
        has(root, &["pom.xml"]),
        has(root, GRADLE) || has(root, &["settings.gradle", "settings.gradle.kts"]),
    );
    if !maven && !gradle {
        return None;
    }
    let mut modules: Vec<String> = Vec::new();
    let mut whole = changed.is_empty();
    for path in changed {
        match owner(root, path, |dir| {
            if maven { has(dir, &["pom.xml"]) } else { has(dir, GRADLE) }
        }) {
            Some(dir) if dir != root => {
                let module = shell_path(root, &dir);
                if !modules.contains(&module) {
                    modules.push(module);
                }
            }
            _ => whole = true,
        }
    }
    let wrapper = |name: &str| {
        if cfg!(windows) && root.join(format!("{name}.cmd")).is_file() {
            Some(format!(".\\{name}.cmd"))
        } else if cfg!(windows) && root.join(format!("{name}.bat")).is_file() {
            Some(format!(".\\{name}.bat"))
        } else if !cfg!(windows) && root.join(name).is_file() {
            Some(format!("./{name}"))
        } else {
            None
        }
    };
    Some(if maven {
        let mvn = wrapper("mvnw").unwrap_or_else(|| "mvn".into());
        if whole {
            format!("{mvn} test -q")
        } else {
            // -am builds the modules they depend on; their tests are not the point here.
            format!("{mvn} test -q -pl {} -am", modules.join(","))
        }
    } else {
        let gradle = wrapper("gradlew").unwrap_or_else(|| "gradle".into());
        if whole {
            format!("{gradle} test")
        } else {
            let tasks: Vec<String> = modules
                .iter()
                .map(|module| format!(":{}:test", module.trim_matches('"').replace('/', ":")))
                .collect();
            format!("{gradle} {}", tasks.join(" "))
        }
    })
}

/// Whether a check may run without asking (trusted project or mode policy).
pub type Preapproved = Arc<dyn Fn(&str) -> bool + Send + Sync>;

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
            // A whole-project test run compiles what the build check would.
            let has = |command: &str| commands.iter().any(|c| c == command);
            let tested_dotnet = commands.iter().any(|c| c.starts_with("dotnet test"));
            let (cargo, go) = (has("cargo test"), has("go test ./..."));
            commands.retain(|command| {
                !(command == "dotnet build" && tested_dotnet
                    || command == "cargo check --all-targets" && cargo
                    || command == "go build ./..." && go)
            });
        }
        if self.detected.is_some() {
            commands = commands
                .into_iter()
                .filter_map(|command| scope_compile(&self.root, command, changed))
                .collect();
        }
        commands
    }

    /// Configured limit, else 120 s and 300 s for the slower test/dotnet/npm toolchains.
    fn timeout(&self, command: &str) -> Duration {
        self.timeout.unwrap_or_else(|| {
            Duration::from_secs(if slow_command(command) { 300 } else { 120 })
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
            let preapproved = self
                .detected
                .as_ref()
                .is_some_and(|(_, preapproved)| preapproved(command));
            if !preapproved {
                match self
                    .authorizer
                    .authorize(Effect::Shell(command.clone()), cancel.clone())
                    .await
                {
                    Ok(()) => {}
                    // Declined by the user or not askable: the check did not run.
                    Err(Error::Approval(reason) | Error::Denied(reason)) => {
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
            // No per-line streaming: the terminal stays quiet during checks; the finish
            // line and, on failure, the detail tail carry the information.
            let output =
                process::shell_live(&self.root, command, timeout, cancel.clone(), None).await?;
            let duration_ms = started.elapsed().as_millis();
            if let Some(progress) = &self.progress {
                let state = if output.timed_out {
                    "超时"
                } else if output.code == Some(0) {
                    "通过"
                } else {
                    "失败"
                };
                progress
                    .line(
                        format!(
                            "[验证] {command} → {state}（{:.0}s）",
                            duration_ms as f64 / 1000.0
                        ),
                        false,
                    )
                    .await;
            }
            if output.code == Some(0) && !output.timed_out {
                summary.push_str(&format!("passed   {command}\n"));
                if let Some(evidence) = &self.evidence {
                    evidence.record_pass(command);
                }
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

/// Builds and test suites of toolchains that often need minutes on a cold cache.
pub fn slow_command(command: &str) -> bool {
    const SLOW: &[&str] = &[
        "dotnet",
        "npm",
        "npx",
        "pnpm",
        "yarn",
        "cargo build",
        "cargo test",
        "cargo check",
        "cargo clippy",
        "cargo run",
        "cargo nextest",
        "go build",
        "go test",
        "mvn",
        "gradle",
        "./gradlew",
        "./mvnw",
        ".\\gradlew",
        ".\\mvnw",
        "bun",
        "pytest",
        "python -m pytest",
        "python3 -m pytest",
    ];
    command
        .split(['&', ';', '|', '\n'])
        .map(str::trim_start)
        .any(|part| SLOW.iter().any(|tool| part.starts_with(tool)))
}

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

    #[tokio::test]
    async fn passed_checks_are_reused_and_dotnet_test_covers_the_build() {
        use crate::safety::permission::{LocalAuthorizer, Mode};
        let data = tempfile::tempdir().unwrap();
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        std::fs::write(root.join("app.csproj"), "").unwrap();
        let store = crate::session::Store::open(data.path()).unwrap();
        let workspace = Arc::new(Workspace::new(root.clone(), store).unwrap());
        let evidence = Arc::new(Evidence::default());
        let verifier = CommandVerifier::new(
            root.clone(),
            vec!["dotnet build".into(), "echo run >> runs.log".into()],
            None,
            workspace,
            Arc::new(LocalAuthorizer {
                mode: Mode::Bypass,
                input: None,
            }),
        )
        .with_detection(true, Arc::new(|_| true))
        .with_evidence(evidence.clone());
        let commands = verifier.turn_commands(&[]);
        assert!(
            !commands.contains(&"dotnet build".to_owned()),
            "{commands:?}"
        );
        assert!(commands.contains(&"dotnet test".to_owned()), "{commands:?}");
        let checks = ["echo run >> runs.log".to_owned()];
        for _ in 0..2 {
            let verification = verifier
                .run(&checks, CancellationToken::new())
                .await
                .unwrap();
            assert_eq!(verification.outcome, Outcome::Passed);
        }
        let runs = std::fs::read(root.join("runs.log")).unwrap();
        let runs = String::from_utf8_lossy(&runs).replace('\0', "");
        assert_eq!(runs.matches("run").count(), 1, "{runs}");
    }

    #[test]
    fn tail_keeps_char_boundaries() {
        assert_eq!(tail("ab界cd", 4), "cd");
        assert_eq!(tail("abc", 10), "abc");
    }

    fn write(root: &Path, path: &str, text: &str) {
        let path = root.join(path);
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(path, text).unwrap();
    }

    #[test]
    fn node_tests_use_the_lockfiles_manager_and_jests_related_tests() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        write(root, "package.json", r#"{"scripts":{"test":"jest --ci"}}"#);
        write(root, "pnpm-lock.yaml", "");
        std::fs::create_dir(root.join("node_modules")).unwrap();
        assert_eq!(
            test_commands(root, &[PathBuf::from("src/a.ts")]),
            ["pnpm test -- --findRelatedTests src/a.ts"]
        );
        // Not only sources changed: the whole suite.
        assert_eq!(
            test_commands(root, &[PathBuf::from("src/a.ts"), PathBuf::from("jest.config.js")])
                .len(),
            1
        );
        assert_eq!(test_commands(root, &[PathBuf::from("README.md")]), ["pnpm test"]);
        write(root, "package.json", r#"{"scripts":{"test":"vitest run && eslint ."}}"#);
        assert_eq!(test_commands(root, &[PathBuf::from("src/a.ts")]), ["pnpm test"]);
    }

    #[test]
    fn pytest_runs_the_test_files_of_the_changed_modules() {
        let Some(python) = python_program() else {
            return;
        };
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        write(root, "pytest.ini", "");
        write(root, "pkg/util.py", "");
        write(root, "pkg/other.py", "");
        write(root, "tests/test_util.py", "");
        write(root, ".venv/lib/test_util.py", "");
        assert_eq!(
            test_commands(root, &[PathBuf::from("pkg/util.py")]),
            [format!("{python} -m pytest -q tests/test_util.py")]
        );
        // A module without its own test file, or conftest: everything.
        for changed in ["pkg/other.py", "conftest.py"] {
            assert_eq!(
                test_commands(root, &[PathBuf::from(changed)]),
                [format!("{python} -m pytest -q")]
            );
        }
    }

    #[test]
    fn dotnet_tests_only_the_test_projects_that_reach_the_change() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        write(root, "app.sln", "");
        write(root, "src/Lib/Lib.csproj", "<Project/>");
        write(root, "src/Other/Other.csproj", "<Project/>");
        write(
            root,
            "src/App/App.csproj",
            r#"<ProjectReference Include="..\Lib\Lib.csproj" />"#,
        );
        write(
            root,
            "tests/App.Tests/App.Tests.csproj",
            r#"<PackageReference Include="Microsoft.NET.Test.Sdk" />
               <ProjectReference Include="..\..\src\App\App.csproj" />"#,
        );
        // Lib is reached through App.
        assert_eq!(
            test_commands(root, &[PathBuf::from("src/Lib/A.cs")]),
            ["dotnet test tests/App.Tests/App.Tests.csproj"]
        );
        assert_eq!(
            test_commands(root, &[PathBuf::from("src/Other/B.cs")]),
            ["dotnet build src/Other/Other.csproj"]
        );
        assert_eq!(
            test_commands(root, &[PathBuf::from("Directory.Build.props")]),
            ["dotnet test"]
        );
    }

    #[test]
    fn java_tests_are_scoped_to_the_changed_modules() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        write(root, "settings.gradle", "");
        write(root, "app/build.gradle", "");
        write(root, "libs/core/build.gradle", "");
        let changed = [
            PathBuf::from("app/src/main/java/A.java"),
            PathBuf::from("libs/core/src/B.java"),
        ];
        assert_eq!(
            test_commands(root, &changed),
            ["gradle :app:test :libs:core:test"]
        );
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        write(root, "pom.xml", "");
        write(root, "core/pom.xml", "");
        assert_eq!(
            test_commands(root, &[PathBuf::from("core/src/A.java")]),
            ["mvn test -q -pl core -am"]
        );
        assert_eq!(test_commands(root, &[PathBuf::from("pom.xml")]), ["mvn test -q"]);
    }

    #[test]
    fn whole_test_runs_drop_the_build_check_and_python_compiles_only_changes() {
        let data = tempfile::tempdir().unwrap();
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        write(&root, "Cargo.toml", "[package]\nname = \"a\"\n");
        write(&root, "a.py", "");
        let store = crate::session::Store::open(data.path()).unwrap();
        let verifier = CommandVerifier::new(
            root.clone(),
            vec![
                "cargo check --all-targets".into(),
                "python -m compileall -q .".into(),
            ],
            None,
            Arc::new(Workspace::new(root.clone(), store).unwrap()),
            Arc::new(crate::safety::permission::LocalAuthorizer {
                mode: crate::safety::permission::Mode::Bypass,
                input: None,
            }),
        )
        .with_detection(true, Arc::new(|_| true));
        assert_eq!(
            verifier.turn_commands(&[PathBuf::from("a.py"), PathBuf::from("src/lib.rs")]),
            ["python -m py_compile a.py", "cargo test"]
        );
        assert_eq!(
            verifier.turn_commands(&[PathBuf::from("src/lib.rs")]),
            ["cargo test"]
        );
    }
}
