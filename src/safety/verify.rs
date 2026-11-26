use std::{
    collections::{BTreeSet, HashMap, HashSet},
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
            name.ends_with(".sln") || name.ends_with(".slnx") || name.ends_with(".csproj")
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

/// Prose no build or test reads. `.txt` and data formats are left out: tests read them.
fn documentation(path: &Path) -> bool {
    path.extension()
        .and_then(|extension| extension.to_str())
        .is_some_and(|extension| {
            ["md", "markdown", "rst", "adoc"]
                .iter()
                .any(|doc| extension.eq_ignore_ascii_case(doc))
        })
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
                Some(
                    "bin" | "obj" | "node_modules" | "target" | "venv" | ".venv" | "build" | "dist"
                )
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
                matches!(
                    e,
                    "js" | "jsx" | "ts" | "tsx" | "mjs" | "cjs" | "mts" | "cts"
                )
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
            let found = tests
                .iter()
                .filter(|test| {
                    test.file_name()
                        .is_some_and(|file| wanted.iter().any(|w| file.to_string_lossy() == *w))
                })
                .collect();
            nearest(root, path, found)
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

/// The test files among `found` (all named after `module`) nearest to it: most leading
/// directories shared, then most trailing ones once `tests`/`test` are dropped (the
/// `src/pkg/a/mod.py` and `tests/a/test_mod.py` layout). Equally near ones all stay, so a
/// module whose namesakes are all far still gets them rather than the whole suite.
fn nearest<'a>(root: &Path, module: &Path, found: Vec<&'a PathBuf>) -> Vec<&'a PathBuf> {
    let dirs = |path: &Path| -> Vec<String> {
        path.parent()
            .into_iter()
            .flat_map(Path::components)
            .filter_map(|part| match part {
                std::path::Component::Normal(name) => Some(name.to_string_lossy().into_owned()),
                _ => None,
            })
            .collect()
    };
    let module = dirs(module);
    let nearness = |test: &Path| {
        let test = dirs(test.strip_prefix(root).unwrap_or(test));
        let leading = module.iter().zip(&test).take_while(|(a, b)| a == b).count();
        let stripped: Vec<&String> = test
            .iter()
            .filter(|name| !matches!(name.as_str(), "tests" | "test"))
            .collect();
        let trailing = module
            .iter()
            .rev()
            .zip(stripped.iter().rev())
            .take_while(|(a, b)| a == *b)
            .count();
        (leading, trailing)
    };
    let Some(best) = found.iter().map(|test| nearness(test)).max() else {
        return found;
    };
    found
        .into_iter()
        .filter(|test| nearness(test) == best)
        .collect()
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
        if is_test_project(root, project, &text) {
            tests.push(normalize(project));
        }
    }
    // Microsoft.Testing.Platform's `dotnet test` takes no positional path.
    let test = if mtp(root) {
        "dotnet test --project"
    } else {
        "dotnet test"
    };
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
        .filter(|project| covers(project))
        .map(|project| format!("{test} {}", shell_path(root, project)))
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

/// Text marking test projects: VSTest's SDK package, the MSBuild property, and the
/// Microsoft.Testing.Platform frameworks, which no longer reference Microsoft.NET.Test.Sdk.
const TEST_MARKERS: &[&str] = &[
    "Microsoft.NET.Test.Sdk",
    "<IsTestProject>true",
    "Microsoft.Testing.Platform",
    "xunit.v3",
    "MSTest.Sdk",
    "\"MSTest\"",
    "\"NUnit\"",
    "\"TUnit\"",
];

/// A test project by its own text, or by the nearest `Directory.Build.props` (which MSBuild
/// imports) naming a test framework for a project whose name ends in Test or Tests. The
/// props usually decide by a condition on the name; this approximates it without running
/// MSBuild, and keeps helpers such as `X.TestSupport` out.
fn is_test_project(root: &Path, project: &Path, text: &str) -> bool {
    let marked = |text: &str| TEST_MARKERS.iter().any(|marker| text.contains(marker));
    if marked(text) {
        return true;
    }
    let named = project.file_stem().is_some_and(|stem| {
        let stem = stem.to_string_lossy().to_lowercase();
        stem.ends_with("test") || stem.ends_with("tests")
    });
    if !named {
        return false;
    }
    let relative = project.strip_prefix(root).unwrap_or(project);
    owner(root, relative, |dir| {
        dir.join("Directory.Build.props").is_file()
    })
    .and_then(|dir| std::fs::read_to_string(dir.join("Directory.Build.props")).ok())
    .is_some_and(|props| marked(&props) || props.contains("<IsTestProject"))
}

/// `global.json` selects Microsoft.Testing.Platform for `dotnet test`.
fn mtp(root: &Path) -> bool {
    std::fs::read_to_string(root.join("global.json"))
        .ok()
        .and_then(|text| serde_json::from_str::<serde_json::Value>(&text).ok())
        .and_then(|json| {
            json.pointer("/test/runner")
                .and_then(serde_json::Value::as_str)
                .map(|runner| runner.eq_ignore_ascii_case("Microsoft.Testing.Platform"))
        })
        .unwrap_or(false)
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
            if maven {
                has(dir, &["pom.xml"])
            } else {
                has(dir, GRADLE)
            }
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
    /// Idle limit of the checks; `None` is [`process::IDLE_LIMIT`].
    idle: Option<Duration>,
    workspace: Arc<Workspace>,
    authorizer: Arc<dyn Authorizer>,
    verified: Mutex<Option<u64>>,
    progress: Option<Arc<Progress>>,
    /// Commands were detected, not configured: tests may join and pre-approval applies.
    detected: Option<(bool, Preapproved)>,
    evidence: Option<Arc<Evidence>>,
    ledger: Option<PathBuf>,
    /// Tests failing before the task's changes, per command and changed-file set;
    /// `None` when the baseline run gave no answer.
    baselines: Mutex<HashMap<String, Option<BTreeSet<String>>>>,
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
            idle: None,
            workspace,
            authorizer,
            verified: Mutex::new(None),
            progress: None,
            detected: None,
            evidence: None,
            ledger: None,
            baselines: Mutex::default(),
        }
    }

    /// Stops a check after this long without any sign of work (default 180 s).
    pub fn with_idle(mut self, idle: Option<Duration>) -> Self {
        self.idle = idle;
        self
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

    /// The task's changed files that scope the checks: scratch files it created and then
    /// deleted have no tests and would widen the run to the whole suite.
    fn changed(&self) -> Result<Vec<PathBuf>> {
        let scratch = self.workspace.deleted_scratch_paths()?;
        let mut changed = self.workspace.changed_paths()?;
        changed.retain(|path| !scratch.contains(path));
        Ok(changed)
    }

    /// The changed paths detected checks are chosen by: git-ignored files (scratch notes,
    /// build output) and documentation are left out, so they neither start a run nor
    /// widen it to the whole suite. Configured commands see every change.
    async fn affecting(&self, paths: Vec<PathBuf>, cancel: &CancellationToken) -> Vec<PathBuf> {
        if self.detected.is_none() {
            return paths;
        }
        let paths: Vec<PathBuf> = paths
            .into_iter()
            .filter(|path| !documentation(path))
            .collect();
        if paths.is_empty() {
            return paths;
        }
        let slash = |path: &Path| path.to_string_lossy().replace('\\', "/");
        let mut input = Vec::new();
        for path in &paths {
            input.extend_from_slice(slash(path).as_bytes());
            input.push(0);
        }
        // Exit 1 (nothing ignored) and 128 (not a repository) both leave stdout empty.
        let ignored: HashSet<String> = match process::git::Git::new(&self.root)
            .args(["check-ignore", "-z", "--stdin"])
            .stdin(input)
            .output(cancel)
            .await
        {
            Ok(output) => output
                .stdout_text()
                .split('\0')
                .filter(|path| !path.is_empty())
                .map(str::to_owned)
                .collect(),
            Err(_) => HashSet::new(),
        };
        paths
            .into_iter()
            .filter(|path| !ignored.contains(&slash(path)))
            .collect()
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

    /// Checks run as long as they keep working: stopped after the idle limit without any
    /// sign of work, or at the total limit (configured, else an hour).
    fn limit(&self) -> process::Limit {
        process::Limit::watched(
            self.timeout.unwrap_or(process::WATCHED_TOTAL),
            self.idle.unwrap_or(process::IDLE_LIMIT),
        )
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
        let paths = self.affecting(paths.to_vec(), &cancel).await;
        // Only documentation or ignored files staged: nothing for the checks to judge.
        if paths.is_empty() && self.detected.is_some() {
            return Ok(Verification {
                outcome: Outcome::Passed,
                summary: String::new(),
                detail: String::new(),
            });
        }
        let commands = self.turn_commands(&paths);
        // Staged content may hold more than this task: the task's before is no baseline.
        let verification = self.run(&commands, false, false, cancel).await?;
        if let Some(evidence) = &self.evidence {
            evidence.set_outcome(verification.outcome.name());
        }
        Ok(verification)
    }

    /// Runs `commands` in order. With `baseline`, a test command whose failing tests all
    /// failed before the task's changes too counts as passed (see [`Self::judge`]).
    /// `after_reply`: the checks run once the reply is written, so the screen says the
    /// task is still busy and needs nothing from the user, not that it waits for an answer.
    async fn run(
        &self,
        commands: &[String],
        baseline: bool,
        after_reply: bool,
        cancel: CancellationToken,
    ) -> Result<Verification> {
        let stage = if after_reply {
            "回复已结束，自动验证中（无需操作）"
        } else {
            "提交前验证中"
        };
        let mut announced = false;
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
                if after_reply && !std::mem::replace(&mut announced, true) {
                    progress
                        .line(
                            "[验证] 回复已结束，正在自动运行检查，不需要你操作".into(),
                            false,
                        )
                        .await;
                }
                progress.activity(format!("{stage} · {command}")).await;
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
            let limit = self.limit();
            let timeout = limit.total;
            let started = Instant::now();
            // No per-line streaming: the terminal stays quiet during checks; the finish
            // line and, on failure, the detail tail carry the information. The status row
            // says how long it has run and whether it still works.
            let watched = match &self.progress {
                Some(progress) => limit
                    .clone()
                    .with_status(progress.clone(), format!("{stage} · {command}")),
                None => limit.clone(),
            };
            let output =
                process::shell_live(&self.root, command, watched, cancel.clone(), None).await?;
            let duration_ms = started.elapsed().as_millis();
            if let Some(progress) = &self.progress {
                let state = if output.stalled.is_some() {
                    "卡住，已中止"
                } else if output.timed_out {
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
            if let Some(reason) = &output.stalled {
                // Stuck says nothing certain about the change either: not verified, and
                // the reason names what was still running and since when nothing moved.
                summary.push_str(&format!(
                    "not run  {command}（卡住，已中止；not verified）\n{reason}\n"
                ));
                self.record(CheckRecord {
                    command: command.clone(),
                    status: "stalled".into(),
                    exit_code: None,
                    duration_ms,
                    evidence: format!("{reason}\n{evidence}"),
                });
                if outcome == Outcome::Passed {
                    outcome = Outcome::NotRun;
                }
                continue;
            }
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
            let judged = if baseline {
                self.judge(command, &output, duration_ms, &limit, &cancel)
                    .await?
            } else {
                None
            };
            if let Some(judged) = &judged
                && judged.new.is_empty()
            {
                let note = format!(
                    "{} 个失败的用例改动前就失败，与本次改动无关",
                    judged.old.len()
                );
                if let Some(progress) = &self.progress {
                    progress
                        .line(format!("[验证] {command} → 通过（{note}）"), false)
                        .await;
                }
                summary.push_str(&format!(
                    "passed   {command}（{note}：{}）\n",
                    list(&judged.old, 5)
                ));
                self.record(CheckRecord {
                    command: command.clone(),
                    status: "preexisting".into(),
                    exit_code: output.code,
                    duration_ms,
                    evidence,
                });
                continue;
            }
            let status = format!("exit_code={:?}", output.code);
            let judged_text = judged.as_ref().map(Judged::detail).unwrap_or_default();
            match &judged {
                Some(judged) if !judged.old.is_empty() => summary.push_str(&format!(
                    "failed   {command} ({status}; 新失败 {} 个，另有 {} 个改动前就失败)\n",
                    judged.new.len(),
                    judged.old.len()
                )),
                _ => summary.push_str(&format!("failed   {command} ({status})\n")),
            }
            detail.push_str(&format!(
                "$ {command}\n{status}\n{judged_text}{}\n",
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
        if let Some(progress) = &self.progress {
            progress.activity("思考中".into()).await;
        }
        Ok(Verification {
            outcome,
            summary,
            detail,
        })
    }
}

const DETAIL_LIMIT: usize = 12 * 1024;

/// A failing run slower than this gets no baseline run: it would cost as much again.
const BASELINE_MAX: Duration = Duration::from_secs(120);

impl CommandVerifier {
    /// Splits the failing tests of a failed test command into new ones and ones that fail
    /// on the code before the task's changes as well. `None` when that cannot be told:
    /// not pytest or `go test`, no test-level result, or a baseline that cannot be rebuilt.
    async fn judge(
        &self,
        command: &str,
        output: &process::CommandOutput,
        duration_ms: u128,
        limit: &process::Limit,
        cancel: &CancellationToken,
    ) -> Result<Option<Judged>> {
        let Some(runner) = Runner::of(command) else {
            return Ok(None);
        };
        if duration_ms > BASELINE_MAX.as_millis() {
            return Ok(None);
        }
        let Some(failing) = failing_tests(runner, output, true) else {
            return Ok(None);
        };
        let Some(task) = self.workspace.task_baseline()? else {
            return Ok(None);
        };
        let slash = |path: &PathBuf| path.to_string_lossy().replace('\\', "/");
        let created: Vec<String> = task.created.iter().map(slash).collect();
        if !runner.baseline_possible(&created) {
            return Ok(None);
        }
        let mut paths: Vec<String> = task
            .files
            .iter()
            .map(|(path, _)| slash(path))
            .chain(created.iter().cloned())
            .collect();
        paths.sort();
        let key = format!("{command}\n{}", paths.join("\n"));
        let lock = || {
            self.baselines
                .lock()
                .map_err(|_| Error::Tool("verification baseline lock poisoned".into()))
        };
        let cached = lock()?.get(&key).cloned();
        let before = match cached {
            Some(before) => before,
            None => {
                let before = self
                    .run_baseline(runner, command, task.files, limit.clone(), cancel)
                    .await?;
                lock()?.insert(key, before.clone());
                before
            }
        };
        Ok(before.map(|before| Judged::split(runner, failing, &before, &created)))
    }

    /// Runs `command` with `files` back at their content before the task; the failing tests,
    /// or `None` when the files could not be swapped or the run timed out.
    async fn run_baseline(
        &self,
        runner: Runner,
        command: &str,
        files: Vec<(PathBuf, String)>,
        limit: process::Limit,
        cancel: &CancellationToken,
    ) -> Result<Option<BTreeSet<String>>> {
        if let Some(progress) = &self.progress {
            progress
                .line(format!("[验证] 改动前基准 $ {command}"), false)
                .await;
        }
        match self.workspace.swap_in(files) {
            Ok(()) => {}
            Err(err @ Error::Uncertain(_)) => return Err(err),
            // An earlier swap still pending, a file that changed meanwhile: no baseline.
            Err(_) => return Ok(None),
        }
        let swap = Swapped {
            workspace: &self.workspace,
            restored: false,
        };
        let started = Instant::now();
        // Bytecode caches go by mtime and size: a swapped-in file written in the same second
        // as a same-sized edit would run stale code. Read none and write none.
        let env: Vec<(&str, String)> = match runner {
            Runner::Pytest => vec![
                ("PYTHONDONTWRITEBYTECODE", "1".into()),
                (
                    "PYTHONPYCACHEPREFIX",
                    std::env::temp_dir()
                        .join("cove-verify-baseline-nopyc")
                        .to_string_lossy()
                        .into_owned(),
                ),
            ],
            // Go caches by content.
            Runner::Go => Vec::new(),
        };
        let output =
            process::shell_with_env(&self.root, command, limit, cancel.clone(), &env).await;
        // The current content goes back first, whatever the run did (cancellation too).
        swap.restore()?;
        let output = output?;
        let failing =
            (!output.timed_out).then(|| failing_tests(runner, &output, false).unwrap_or_default());
        if let Some(progress) = &self.progress {
            let seconds = started.elapsed().as_secs_f64();
            let result = match &failing {
                Some(failing) => format!("{} 个用例失败（{seconds:.0}s）", failing.len()),
                None => format!("超时，不作比较（{seconds:.0}s）"),
            };
            progress
                .line(format!("[验证] 改动前基准 {command} → {result}"), false)
                .await;
        }
        Ok(failing.map(|failing| failing.into_iter().collect()))
    }
}

/// Puts the swapped files back when dropped, should the verification future be dropped
/// mid-run; `restore` does it explicitly and reports failure.
struct Swapped<'a> {
    workspace: &'a Workspace,
    restored: bool,
}

impl Swapped<'_> {
    fn restore(mut self) -> Result<()> {
        self.restored = true;
        self.workspace.swap_restore().map(|_| ()).map_err(|err| {
            Error::Uncertain(format!(
                "自动验证跑完改动前基准后没能放回改动后的文件：{err}；用 /resolve 恢复"
            ))
        })
    }
}

impl Drop for Swapped<'_> {
    fn drop(&mut self) {
        if !self.restored {
            let _ = self.workspace.swap_restore();
        }
    }
}

/// Test runners whose output names each failing test.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Runner {
    Pytest,
    Go,
}

impl Runner {
    fn of(command: &str) -> Option<Self> {
        if command.contains(['&', ';', '|', '\n']) {
            return None;
        }
        let words: Vec<&str> = command.split_whitespace().collect();
        match words.as_slice() {
            ["pytest", ..] => Some(Self::Pytest),
            [python, "-m", "pytest", ..] if python.starts_with("python") || *python == "py" => {
                Some(Self::Pytest)
            }
            ["go", "test", ..] => Some(Self::Go),
            _ => None,
        }
    }

    /// A created `conftest.py` or `_test.go` would act in the baseline run too (a swap
    /// cannot remove it), and Go failures cannot be traced to a file.
    fn baseline_possible(self, created: &[String]) -> bool {
        !created.iter().any(|path| match self {
            Self::Pytest => path.rsplit('/').next() == Some("conftest.py"),
            Self::Go => path.ends_with("_test.go"),
        })
    }
}

/// Failing test ids in a run's output. `strict` (the run under judgment) demands a complete
/// list, `None` otherwise; a baseline run gives whatever it names.
fn failing_tests(
    runner: Runner,
    output: &process::CommandOutput,
    strict: bool,
) -> Option<Vec<String>> {
    // A run stopped early, stuck or at the limit, lists only part of its failures.
    if output.timed_out || output.stalled.is_some() {
        return None;
    }
    let text = format!("{}\n{}", output.stdout, output.stderr);
    match runner {
        Runner::Pytest => pytest_failures(&text, output.code, strict),
        Runner::Go => go_failures(&text, output.code, output.truncated, strict),
    }
}

/// Node ids of the short test summary's `FAILED`/`ERROR` lines; strict mode needs exit code 1
/// (tests failed, as opposed to an interrupted collection or a usage error) and as many lines
/// as the final totals count.
fn pytest_failures(text: &str, code: Option<i32>, strict: bool) -> Option<Vec<String>> {
    static TOTALS: std::sync::LazyLock<regex::Regex> = std::sync::LazyLock::new(|| {
        regex::Regex::new(r"\b\d+ (passed|failed|errors?|skipped|xfailed|xpassed)\b").unwrap()
    });
    static FAILED: std::sync::LazyLock<regex::Regex> =
        std::sync::LazyLock::new(|| regex::Regex::new(r"\b(\d+) (failed|errors?)\b").unwrap());
    let ids: Vec<String> = text
        .lines()
        .filter_map(|line| {
            line.strip_prefix("FAILED ")
                .or_else(|| line.strip_prefix("ERROR "))
        })
        .map(|rest| rest.split(" - ").next().unwrap_or(rest).trim().to_owned())
        .filter(|id| !id.is_empty())
        .collect();
    if !strict {
        return Some(ids);
    }
    if code != Some(1) {
        return None;
    }
    let totals = text
        .lines()
        .rev()
        .find(|line| line.contains(" in ") && TOTALS.is_match(line))?;
    let expected: usize = FAILED
        .captures_iter(totals)
        .filter_map(|captures| captures[1].parse::<usize>().ok())
        .sum();
    (expected > 0 && expected == ids.len()).then_some(ids)
}

/// `<package>/<test>` for each `--- FAIL: <test>` line, assigned to the `FAIL\t<package>`
/// line after it. Strict mode needs exit code 1, a complete output, no build failure or
/// timeout, and every failed package traced to its tests.
fn go_failures(
    text: &str,
    code: Option<i32>,
    truncated: bool,
    strict: bool,
) -> Option<Vec<String>> {
    let mut pending: Vec<String> = Vec::new();
    let mut ids = Vec::new();
    let mut untraced = false;
    for line in text.lines() {
        if let Some(rest) = line.trim_start().strip_prefix("--- FAIL: ") {
            pending.extend(rest.split_whitespace().next().map(str::to_owned));
        } else if let Some(rest) = line.strip_prefix("FAIL\t") {
            let package = rest.split_whitespace().next().unwrap_or_default();
            untraced |= pending.is_empty();
            ids.extend(pending.drain(..).map(|test| format!("{package}/{test}")));
        } else if line.starts_with("ok  \t") {
            pending.clear();
        }
    }
    if !strict {
        return Some(ids);
    }
    let broken = ["[build failed]", "[setup failed]", "panic: test timed out"]
        .iter()
        .any(|marker| text.contains(marker));
    (code == Some(1) && !truncated && !broken && !untraced && pending.is_empty() && !ids.is_empty())
        .then_some(ids)
}

/// A failed test command's failing tests, split by whether they fail before the change too.
#[derive(Debug, PartialEq, Eq)]
struct Judged {
    new: Vec<String>,
    old: Vec<String>,
}

impl Judged {
    /// Failures in a test file the task created are new whatever the baseline says: the
    /// swap leaves created files in place, so the baseline ran them against the old code.
    fn split(
        runner: Runner,
        failing: Vec<String>,
        before: &BTreeSet<String>,
        created: &[String],
    ) -> Self {
        let created_file = |id: &str| {
            runner == Runner::Pytest
                && created
                    .iter()
                    .any(|path| id.split("::").next() == Some(path.as_str()))
        };
        let failing: BTreeSet<String> = failing.into_iter().collect();
        let (old, new) = failing
            .into_iter()
            .partition(|id| before.contains(id) && !created_file(id));
        Self { new, old }
    }

    /// The lines ahead of the output in the detail handed to the model.
    fn detail(&self) -> String {
        if self.old.is_empty() {
            return "Baseline: none of these failures occur on the code before this task's changes; this task caused them.\n".into();
        }
        format!(
            "New failures (they pass before this task's changes; fix these): {}\nAlready failing before this task's changes (unrelated to it; do not fix them or change other code for them): {}\n",
            list(&self.new, 20),
            list(&self.old, 20)
        )
    }
}

/// Up to `limit` names, then how many more.
fn list(names: &[String], limit: usize) -> String {
    let mut text = names
        .iter()
        .take(limit)
        .cloned()
        .collect::<Vec<_>>()
        .join(", ");
    if names.len() > limit {
        text.push_str(&format!(" (+{})", names.len() - limit));
    }
    text
}

use crate::process::tail_bytes as tail;

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
            let changed = self.affecting(self.changed()?, &cancel).await;
            // Only scratch, ignored or documentation files changed: nothing to check.
            if changed.is_empty() {
                return Ok(None);
            }
            let commands = self.turn_commands(&changed);
            if commands.is_empty() || lock(&self.verified)? == Some(fingerprint) {
                return Ok(None);
            }
            let verification = self.run(&commands, true, true, cancel).await?;
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
                .run(&checks, true, false, CancellationToken::new())
                .await
                .unwrap();
            assert_eq!(verification.outcome, Outcome::Passed);
        }
        let runs = std::fs::read(root.join("runs.log")).unwrap();
        let runs = String::from_utf8_lossy(&runs).replace('\0', "");
        assert_eq!(runs.matches("run").count(), 1, "{runs}");
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
            test_commands(
                root,
                &[PathBuf::from("src/a.ts"), PathBuf::from("jest.config.js")]
            )
            .len(),
            1
        );
        assert_eq!(
            test_commands(root, &[PathBuf::from("README.md")]),
            ["pnpm test"]
        );
        write(
            root,
            "package.json",
            r#"{"scripts":{"test":"vitest run && eslint ."}}"#,
        );
        assert_eq!(
            test_commands(root, &[PathBuf::from("src/a.ts")]),
            ["pnpm test"]
        );
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
    fn pytest_prefers_the_nearest_test_file_of_the_same_name() {
        let Some(python) = python_program() else {
            return;
        };
        let pytest = |root: &Path, changed: &str| test_commands(root, &[PathBuf::from(changed)]);
        // sympy: core/numbers.py and polys/numberfields/tests/test_numbers.py share a name.
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        write(root, "pytest.ini", "");
        write(root, "pkg/core/numbers.py", "");
        write(root, "pkg/core/tests/test_numbers.py", "");
        write(root, "pkg/polys/nf/tests/test_numbers.py", "");
        write(root, "pkg/sets/handlers/functions.py", "");
        write(root, "pkg/vector/tests/test_functions.py", "");
        write(root, "pkg/tensor/tests/test_functions.py", "");
        assert_eq!(
            pytest(root, "pkg/core/numbers.py"),
            [format!(
                "{python} -m pytest -q pkg/core/tests/test_numbers.py"
            )]
        );
        // Equally far namesakes all stay rather than widening to the whole suite.
        assert_eq!(
            pytest(root, "pkg/sets/handlers/functions.py"),
            [format!(
                "{python} -m pytest -q pkg/tensor/tests/test_functions.py pkg/vector/tests/test_functions.py"
            )]
        );
        // A tests/ tree mirroring src/: the matching subdirectory wins.
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        write(root, "pytest.ini", "");
        write(root, "src/app/a/util.py", "");
        write(root, "tests/a/test_util.py", "");
        write(root, "tests/b/test_util.py", "");
        assert_eq!(
            pytest(root, "src/app/a/util.py"),
            [format!("{python} -m pytest -q tests/a/test_util.py")]
        );
    }

    /// A verifier over a real workspace whose task created `scratch` with the file tools and
    /// then deleted it outside them (`rm` in the shell), after editing `edited`.
    fn verifier_with_deleted_scratch(
        root: &Path,
        data: &Path,
        edited: &[&str],
        scratch: &[&str],
    ) -> CommandVerifier {
        let store = crate::session::Store::open(data).unwrap();
        let workspace = Arc::new(Workspace::new(root.to_path_buf(), store).unwrap());
        workspace.begin().unwrap();
        for path in edited {
            let path = root.join(path);
            let before = std::fs::read_to_string(&path).unwrap();
            workspace
                .replace(&path, Some(&before), &format!("{before}\n# edited\n"))
                .unwrap();
        }
        for path in scratch {
            workspace
                .replace(&root.join(path), None, "print(1)\n")
                .unwrap();
            std::fs::remove_file(root.join(path)).unwrap();
        }
        CommandVerifier::new(
            root.to_path_buf(),
            Vec::new(),
            None,
            workspace,
            Arc::new(crate::safety::permission::LocalAuthorizer {
                mode: crate::safety::permission::Mode::Bypass,
                input: None,
            }),
        )
        .with_detection(true, Arc::new(|_| true))
    }

    #[test]
    fn deleted_scratch_files_do_not_widen_pytest_to_the_whole_suite() {
        let Some(python) = python_program() else {
            return;
        };
        // The files of pub-swe-sympy-24562: one module and its test edited, scratch scripts
        // created with the write tool and removed with `rm`.
        let data = tempfile::tempdir().unwrap();
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        write(&root, "pytest.ini", "");
        write(&root, "pkg/numbers.py", "x = 1\n");
        write(
            &root,
            "pkg/tests/test_numbers.py",
            "def test_x():\n    pass\n",
        );
        let verifier = verifier_with_deleted_scratch(
            &root,
            data.path(),
            &["pkg/numbers.py", "pkg/tests/test_numbers.py"],
            &["_check_rational.py", "_env_check.py", "_install_pytest.py"],
        );
        assert_eq!(
            verifier.turn_commands(&verifier.changed().unwrap()),
            [format!("{python} -m pytest -q pkg/tests/test_numbers.py")]
        );
        // A project file the task deleted still scopes the run: its tests may now fail.
        std::fs::remove_file(root.join("pkg/numbers.py")).unwrap();
        assert_eq!(
            verifier.changed().unwrap(),
            [
                PathBuf::from("pkg/numbers.py"),
                PathBuf::from("pkg/tests/test_numbers.py")
            ]
        );
    }

    #[tokio::test]
    async fn a_task_that_only_left_deleted_scratch_files_is_not_verified() {
        let data = tempfile::tempdir().unwrap();
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        write(&root, "pytest.ini", "");
        let verifier = verifier_with_deleted_scratch(&root, data.path(), &[], &["_probe.py"]);
        assert!(verifier.workspace.fingerprint().unwrap().is_some());
        assert!(
            verifier
                .verify(CancellationToken::new())
                .await
                .unwrap()
                .is_none()
        );
    }

    #[test]
    fn deleted_scratch_files_do_not_widen_node_dotnet_or_java_tests() {
        let checks = |root: &Path, edited: &[&str], scratch: &[&str]| {
            let data = tempfile::tempdir().unwrap();
            let verifier = verifier_with_deleted_scratch(root, data.path(), edited, scratch);
            verifier.turn_commands(&verifier.changed().unwrap())
        };
        // Node: a non-source scratch file would run the whole suite, a source one would be
        // handed to jest as a missing path.
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        write(&root, "package.json", r#"{"scripts":{"test":"jest"}}"#);
        std::fs::create_dir(root.join("node_modules")).unwrap();
        write(&root, "src/a.ts", "export const a = 1;\n");
        assert_eq!(
            checks(&root, &["src/a.ts"], &["_probe.py", "_probe.js"]),
            ["npm test -- --findRelatedTests src/a.ts"]
        );
        // .NET: a scratch file outside every project would test the whole solution.
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        write(&root, "app.sln", "");
        write(&root, "src/Lib/Lib.csproj", "<Project/>");
        write(&root, "src/Lib/A.cs", "class A {}\n");
        write(
            &root,
            "tests/Lib.Tests/Lib.Tests.csproj",
            r#"<PackageReference Include="Microsoft.NET.Test.Sdk" />
               <ProjectReference Include="..\..\src\Lib\Lib.csproj" />"#,
        );
        assert_eq!(
            checks(&root, &["src/Lib/A.cs"], &["_probe.py"]),
            ["dotnet test tests/Lib.Tests/Lib.Tests.csproj"]
        );
        // Maven: a scratch file in the root module would test every module.
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        write(&root, "pom.xml", "");
        write(&root, "core/pom.xml", "");
        write(&root, "core/src/A.java", "class A {}\n");
        assert_eq!(
            checks(&root, &["core/src/A.java"], &["Probe.java"]),
            ["mvn test -q -pl core -am"]
        );
    }

    #[tokio::test]
    async fn ignored_and_documentation_files_neither_start_nor_widen_detected_checks() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        let initialized = std::process::Command::new("git")
            .arg("init")
            .arg("-q")
            .current_dir(&root)
            .status()
            .is_ok_and(|status| status.success());
        if !initialized {
            return;
        }
        write(&root, ".gitignore", ".local/\n");
        write(&root, "app.sln", "");
        write(&root, "src/Lib/Lib.csproj", "<Project/>");
        write(&root, "src/Lib/A.cs", "class A {}\n");
        write(
            &root,
            "tests/Lib.Tests/Lib.Tests.csproj",
            r#"<PackageReference Include="Microsoft.NET.Test.Sdk" />
               <ProjectReference Include="..\..\src\Lib\Lib.csproj" />"#,
        );
        write(&root, ".local/commit-msg.txt", "draft\n");
        write(&root, "docs/notes.md", "# notes\n");
        let cancel = CancellationToken::new();
        // An ignored scratch note or a document alone: nothing to verify, where both used
        // to run the whole solution's tests.
        for edited in [".local/commit-msg.txt", "docs/notes.md"] {
            let data = tempfile::tempdir().unwrap();
            let verifier = verifier_with_deleted_scratch(&root, data.path(), &[edited], &[]);
            assert_eq!(
                verifier.turn_commands(&verifier.changed().unwrap()),
                ["dotnet test"]
            );
            assert!(
                verifier.verify(cancel.clone()).await.unwrap().is_none(),
                "{edited}"
            );
        }
        // Next to a code change they do not widen the run to the whole suite.
        let data = tempfile::tempdir().unwrap();
        let verifier = verifier_with_deleted_scratch(
            &root,
            data.path(),
            &["src/Lib/A.cs", "docs/notes.md", ".local/commit-msg.txt"],
            &[],
        );
        let changed = verifier
            .affecting(verifier.changed().unwrap(), &cancel)
            .await;
        assert_eq!(changed, [PathBuf::from("src/Lib/A.cs")]);
        assert_eq!(
            verifier.turn_commands(&changed),
            ["dotnet test tests/Lib.Tests/Lib.Tests.csproj"]
        );
        // A commit of documentation only runs no checks.
        let verification = verifier
            .verify_commit(&[PathBuf::from("docs/notes.md")], cancel.clone())
            .await
            .unwrap();
        assert_eq!(verification.outcome, Outcome::Passed);
        assert!(verification.summary.is_empty());
        // Configured commands are the user's: they see every change.
        let configured = CommandVerifier::new(
            root.clone(),
            vec!["markdownlint docs".into()],
            None,
            verifier.workspace.clone(),
            Arc::new(crate::safety::permission::LocalAuthorizer {
                mode: crate::safety::permission::Mode::Bypass,
                input: None,
            }),
        );
        let paths = vec![
            PathBuf::from("docs/notes.md"),
            PathBuf::from(".local/commit-msg.txt"),
        ];
        assert_eq!(configured.affecting(paths.clone(), &cancel).await, paths);
    }

    #[cfg(any(windows, target_os = "linux"))]
    #[tokio::test]
    async fn a_stuck_check_is_stopped_and_says_why() {
        let data = tempfile::tempdir().unwrap();
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        let store = crate::session::Store::open(data.path()).unwrap();
        let workspace = Arc::new(Workspace::new(root.clone(), store).unwrap());
        let ledger = data.path().join("ledger.jsonl");
        let command = if cfg!(windows) {
            "Start-Sleep -Seconds 60"
        } else {
            "sleep 60"
        };
        let verifier = CommandVerifier::new(
            root.clone(),
            vec![command.into()],
            None,
            workspace,
            Arc::new(crate::safety::permission::LocalAuthorizer {
                mode: crate::safety::permission::Mode::Bypass,
                input: None,
            }),
        )
        .with_idle(Some(Duration::from_secs(2)))
        .with_ledger(ledger.clone());
        let started = Instant::now();
        let verification = verifier
            .verify_commit(&[PathBuf::from("a.txt")], CancellationToken::new())
            .await
            .unwrap();
        assert!(started.elapsed() < Duration::from_secs(30));
        assert_eq!(verification.outcome, Outcome::NotRun);
        assert!(
            verification.summary.contains("卡住，已中止")
                && verification.summary.contains("没有任何活动"),
            "{}",
            verification.summary
        );
        let line = std::fs::read_to_string(&ledger).unwrap();
        assert!(line.contains("\"status\":\"stalled\""), "{line}");
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
    fn slnx_and_microsoft_testing_platform_projects_are_verified() {
        // The layout of a .NET 10 repository: a .slnx, MTP selected in global.json, and
        // the test framework added by tests/Directory.Build.props, not by each csproj.
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        write(root, "App.slnx", "<Solution/>");
        assert_eq!(detect(root), ["dotnet build"]);
        write(
            root,
            "global.json",
            r#"{"sdk":{"version":"10.0.100"},"test":{"runner":"Microsoft.Testing.Platform"}}"#,
        );
        write(root, "src/Lib/Lib.csproj", "<Project/>");
        write(root, "src/Other/Other.csproj", "<Project/>");
        write(
            root,
            "tests/Directory.Build.props",
            r#"<IsTestProject Condition="$(MSBuildProjectName.EndsWith('UnitTest'))">true</IsTestProject>
               <PackageReference Include="xunit.v3.mtp-v2" />"#,
        );
        write(
            root,
            "tests/Lib.UnitTest/Lib.UnitTest.csproj",
            r#"<ProjectReference Include="..\..\src\Lib\Lib.csproj" />
               <ProjectReference Include="..\Lib.TestSupport\Lib.TestSupport.csproj" />"#,
        );
        // A helper library under tests/ is no test project.
        write(
            root,
            "tests/Lib.TestSupport/Lib.TestSupport.csproj",
            r#"<ProjectReference Include="..\..\src\Lib\Lib.csproj" />"#,
        );
        assert_eq!(
            test_commands(root, &[PathBuf::from("src/Lib/A.cs")]),
            ["dotnet test --project tests/Lib.UnitTest/Lib.UnitTest.csproj"]
        );
        assert_eq!(
            test_commands(root, &[PathBuf::from("src/Other/B.cs")]),
            ["dotnet build src/Other/Other.csproj"]
        );
        assert_eq!(
            test_commands(root, &[PathBuf::from("global.json")]),
            ["dotnet test"]
        );
    }

    /// Prints the checks chosen for a real repository: `COVE_VERIFY_ROOT=<dir>
    /// COVE_VERIFY_CHANGED=a.cs,b.cs cargo test --release --lib print_checks -- --ignored --nocapture`.
    #[test]
    #[ignore]
    fn print_checks_for_a_real_repository() {
        let root = PathBuf::from(std::env::var("COVE_VERIFY_ROOT").unwrap());
        let changed: Vec<PathBuf> = std::env::var("COVE_VERIFY_CHANGED")
            .unwrap_or_default()
            .split(',')
            .filter(|path| !path.is_empty())
            .map(PathBuf::from)
            .collect();
        println!("detect: {:?}", detect(&root));
        println!("tests:  {:?}", test_commands(&root, &changed));
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
        assert_eq!(
            test_commands(root, &[PathBuf::from("pom.xml")]),
            ["mvn test -q"]
        );
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

    fn output(code: i32, stdout: &str) -> process::CommandOutput {
        process::CommandOutput {
            code: Some(code),
            stdout: stdout.into(),
            stderr: String::new(),
            truncated: false,
            timed_out: false,
            stalled: None,
            encoding_warning: false,
        }
    }

    #[test]
    fn test_level_failures_are_read_only_from_complete_runs() {
        assert_eq!(
            Runner::of("python -m pytest -q a/test_x.py"),
            Some(Runner::Pytest)
        );
        assert_eq!(Runner::of("go test ./pkg/a"), Some(Runner::Go));
        for other in [
            "cargo test",
            "npm test",
            "python -m pytest -q && echo",
            "dotnet test",
        ] {
            assert_eq!(Runner::of(other), None, "{other}");
        }
        // The summary of pub-swe-sympy-24562's first check.
        let pytest = ".....F....\n=========================== short test summary info ===========================\n\
            FAILED sympy/core/tests/test_numbers.py::test_numpy_to_float - ValueError: co...\n\
            1 failed, 109 passed, 1 xfailed in 6.77s\n";
        let run = output(1, pytest);
        assert_eq!(
            failing_tests(Runner::Pytest, &run, true).unwrap(),
            ["sympy/core/tests/test_numbers.py::test_numpy_to_float"]
        );
        // Totals that do not match the lines (a summary cut off, `-rN`), an interrupted
        // collection or a timeout: no test-level answer.
        let short = "FAILED a.py::t - x\n2 failed, 3 passed in 1.0s\n";
        assert_eq!(failing_tests(Runner::Pytest, &output(1, short), true), None);
        let collection = "ERROR a/test_x.py - ImportError\n1 error in 0.2s\n";
        assert_eq!(
            failing_tests(Runner::Pytest, &output(2, collection), true),
            None
        );
        assert_eq!(
            failing_tests(Runner::Pytest, &output(2, collection), false).unwrap(),
            ["a/test_x.py"]
        );
        let mut slow = output(1, pytest);
        slow.timed_out = true;
        assert_eq!(failing_tests(Runner::Pytest, &slow, true), None);

        let go = "--- FAIL: TestA (0.00s)\n    a_test.go:9: boom\n    --- FAIL: TestA/sub (0.00s)\nFAIL\nFAIL\texample.com/m/a\t0.01s\nok  \texample.com/m/b\t0.01s\nFAIL\n";
        assert_eq!(
            failing_tests(Runner::Go, &output(1, go), true).unwrap(),
            ["example.com/m/a/TestA", "example.com/m/a/TestA/sub"]
        );
        let build = "FAIL\texample.com/m/a [build failed]\n";
        assert_eq!(failing_tests(Runner::Go, &output(1, build), true), None);
        let untraced = "panic: boom\nFAIL\texample.com/m/a\t0.01s\n";
        assert_eq!(failing_tests(Runner::Go, &output(1, untraced), true), None);
    }

    #[test]
    fn failures_in_test_files_the_task_created_are_always_new() {
        let before: BTreeSet<String> = ["t/test_a.py::test_old", "t/test_new.py::test_x"]
            .map(String::from)
            .into();
        let failing = [
            "t/test_a.py::test_old",
            "t/test_a.py::test_broke",
            "t/test_new.py::test_x",
        ]
        .map(String::from)
        .to_vec();
        let judged = Judged::split(
            Runner::Pytest,
            failing,
            &before,
            &["t/test_new.py".to_owned()],
        );
        assert_eq!(judged.old, ["t/test_a.py::test_old"]);
        assert_eq!(
            judged.new,
            ["t/test_a.py::test_broke", "t/test_new.py::test_x"]
        );
        assert!(judged.detail().contains("do not fix"));
        assert!(Runner::Pytest.baseline_possible(&["t/test_new.py".to_owned()]));
        assert!(!Runner::Pytest.baseline_possible(&["t/conftest.py".to_owned()]));
        assert!(!Runner::Go.baseline_possible(&["a/x_test.go".to_owned()]));
    }

    #[test]
    fn the_task_baseline_holds_before_texts_and_created_files() {
        let data = tempfile::tempdir().unwrap();
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        write(&root, "a.py", "old\n");
        write(&root, "gone.py", "x\n");
        let store = crate::session::Store::open(data.path()).unwrap();
        let workspace = Workspace::new(root.clone(), store).unwrap();
        workspace.begin().unwrap();
        workspace
            .replace(&root.join("a.py"), Some("old\n"), "new\n")
            .unwrap();
        workspace.replace(&root.join("b.py"), None, "b\n").unwrap();
        workspace
            .replace(&root.join("_tmp.py"), None, "t\n")
            .unwrap();
        std::fs::remove_file(root.join("_tmp.py")).unwrap();
        let baseline = workspace.task_baseline().unwrap().unwrap();
        assert_eq!(
            baseline.files,
            [(PathBuf::from("a.py"), "old\n".to_owned())]
        );
        assert_eq!(baseline.created, [PathBuf::from("b.py")]);
        // A project file the task changed and that is gone cannot be swapped back.
        workspace
            .replace(&root.join("gone.py"), Some("x\n"), "y\n")
            .unwrap();
        std::fs::remove_file(root.join("gone.py")).unwrap();
        assert!(workspace.task_baseline().unwrap().is_none());
    }

    #[tokio::test]
    async fn failures_that_predate_the_task_do_not_fail_verification() {
        let Some(python) = python_program() else {
            return;
        };
        let probe = std::process::Command::new(python)
            .args(["-m", "pytest", "--version"])
            .output();
        if !probe.is_ok_and(|probe| probe.status.success()) {
            return;
        }
        let data = tempfile::tempdir().unwrap();
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        write(&root, "pytest.ini", "");
        write(&root, "conftest.py", "");
        let original = "def add(a, b):\n    return a + b\n\n\ndef legacy():\n    return 1\n";
        write(&root, "calc.py", original);
        write(
            &root,
            "tests/test_calc.py",
            "from calc import add, legacy\n\n\ndef test_add():\n    assert add(1, 2) == 3\n\n\ndef test_legacy():\n    assert legacy() == 2\n",
        );
        let store = crate::session::Store::open(data.path()).unwrap();
        let workspace = Arc::new(Workspace::new(root.clone(), store).unwrap());
        workspace.begin().unwrap();
        let verifier = CommandVerifier::new(
            root.clone(),
            Vec::new(),
            None,
            workspace.clone(),
            Arc::new(crate::safety::permission::LocalAuthorizer {
                mode: crate::safety::permission::Mode::Bypass,
                input: None,
            }),
        )
        .with_detection(true, Arc::new(|_| true));
        let checks = [format!("{python} -m pytest -q tests/test_calc.py")];
        // The change keeps add working; test_legacy failed before it.
        let kept = format!("{original}\n\ndef unused():\n    return 0\n");
        workspace
            .replace(&root.join("calc.py"), Some(original), &kept)
            .unwrap();
        let verification = verifier
            .run(&checks, true, false, CancellationToken::new())
            .await
            .unwrap();
        assert_eq!(
            verification.outcome,
            Outcome::Passed,
            "{}",
            verification.summary
        );
        assert!(
            verification.summary.contains("改动前就失败")
                && verification.summary.contains("test_legacy"),
            "{}",
            verification.summary
        );
        assert_eq!(std::fs::read_to_string(root.join("calc.py")).unwrap(), kept);
        assert!(workspace.pending_swap().is_none());
        // The commit gate does not compare.
        let commit = verifier
            .run(&checks, false, false, CancellationToken::new())
            .await
            .unwrap();
        assert_eq!(commit.outcome, Outcome::Failed);
        // Breaking add is a new failure; test_legacy is listed as unrelated.
        let broken = kept.replace("return a + b", "return a - b - 0");
        workspace
            .replace(&root.join("calc.py"), Some(&kept), &broken)
            .unwrap();
        let verification = verifier
            .run(&checks, true, false, CancellationToken::new())
            .await
            .unwrap();
        assert_eq!(verification.outcome, Outcome::Failed);
        let detail = &verification.detail;
        assert!(
            detail.contains("New failures (they pass before this task's changes; fix these): tests/test_calc.py::test_add\n")
                && detail.contains("do not fix them or change other code for them): tests/test_calc.py::test_legacy\n"),
            "{detail}"
        );
        assert_eq!(
            std::fs::read_to_string(root.join("calc.py")).unwrap(),
            broken
        );
    }
}
