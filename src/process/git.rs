//! The one way cove runs git. Every call is non-interactive (no terminal or
//! credential-manager prompts, empty stdin), disables `core.fsmonitor` (a repository's
//! config could otherwise run a program through a read-only query), drops the variables
//! that would point git at another repository, is bounded in time and output, and has
//! its process tree killed on timeout or cancellation (`run_argv`). Callers with special
//! needs say so: the snapshot store isolates git's config, the changeset code drops every
//! `GIT_*` variable.

use std::{
    ffi::OsString,
    io::Read,
    path::{Path, PathBuf},
    process::Stdio,
    time::{Duration, Instant},
};

use tokio_util::sync::CancellationToken;

use super::{Argv, RawOutput, run_argv};
use crate::domain::{Error, Result};

pub const DEFAULT_TIMEOUT: Duration = Duration::from_secs(60);
/// Output kept per stream by default; more is an error, never a silent cut.
pub const DEFAULT_LIMIT: usize = 4 * 1024 * 1024;

/// Variables that point git at another repository, index or config. A cove started
/// from a git hook (husky and the like) inherits them; they must not leak into its git.
const LOCATION: &[&str] = &[
    "GIT_DIR",
    "GIT_WORK_TREE",
    "GIT_INDEX_FILE",
    "GIT_OBJECT_DIRECTORY",
    "GIT_ALTERNATE_OBJECT_DIRECTORIES",
    "GIT_COMMON_DIR",
    "GIT_NAMESPACE",
    "GIT_CONFIG_PARAMETERS",
    "GIT_CONFIG_COUNT",
];

/// Fixed for every call; explicit settings of the same names win.
const NON_INTERACTIVE: &[(&str, &str)] = &[
    ("GIT_TERMINAL_PROMPT", "0"),
    ("GCM_INTERACTIVE", "never"),
    ("GIT_OPTIONAL_LOCKS", "0"),
];

#[derive(Clone, Copy, PartialEq, Eq)]
enum Scrub {
    /// Only [`LOCATION`]; network settings (`GIT_SSH_COMMAND`, `GIT_ASKPASS`, proxies)
    /// stay, so fetching plugins keeps the user's credentials.
    Location,
    /// Every `GIT_*` variable that is not set explicitly.
    All,
}

pub struct Git {
    dir: PathBuf,
    args: Vec<OsString>,
    env: Vec<(OsString, OsString)>,
    remove: Vec<OsString>,
    stdin: Option<Vec<u8>>,
    timeout: Duration,
    limit: usize,
    scrub: Scrub,
}

/// What git printed and how it ended; a failure is not an error here.
pub struct GitOutput {
    pub ok: bool,
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
}

impl GitOutput {
    pub fn stdout_text(&self) -> String {
        String::from_utf8_lossy(&self.stdout).into_owned()
    }

    pub fn stderr_text(&self) -> String {
        String::from_utf8_lossy(&self.stderr).into_owned()
    }
}

impl Git {
    pub fn new(dir: &Path) -> Self {
        Self {
            dir: crate::paths::plain(dir),
            args: vec!["-c".into(), "core.fsmonitor=false".into()],
            env: Vec::new(),
            remove: Vec::new(),
            stdin: None,
            timeout: DEFAULT_TIMEOUT,
            limit: DEFAULT_LIMIT,
            scrub: Scrub::Location,
        }
    }

    pub fn arg(mut self, arg: impl Into<OsString>) -> Self {
        self.args.push(arg.into());
        self
    }

    pub fn args<I, S>(mut self, args: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<OsString>,
    {
        self.args.extend(args.into_iter().map(Into::into));
        self
    }

    /// Set for this call; never removed by the scrub.
    pub fn env(mut self, key: impl Into<OsString>, value: impl Into<OsString>) -> Self {
        self.env.push((key.into(), value.into()));
        self
    }

    pub fn env_remove(mut self, key: impl Into<OsString>) -> Self {
        self.remove.push(key.into());
        self
    }

    pub fn timeout(mut self, timeout: Duration) -> Self {
        self.timeout = timeout;
        self
    }

    /// Bytes kept per stream; output beyond it makes `bytes`/`text` fail.
    pub fn limit(mut self, limit: usize) -> Self {
        self.limit = limit;
        self
    }

    pub fn stdin(mut self, input: Vec<u8>) -> Self {
        self.stdin = Some(input);
        self
    }

    /// The snapshot store's git: its own empty global config, no system config and no
    /// `-c` settings inherited through the environment.
    pub fn isolated(self, global_config: &Path) -> Self {
        self.env("GIT_CONFIG_GLOBAL", global_config)
            .env("GIT_CONFIG_NOSYSTEM", "1")
    }

    /// Drops every inherited `GIT_*` variable (changesets apply patches and must see
    /// nothing of the user's git environment).
    pub fn scrub_all(mut self) -> Self {
        self.scrub = Scrub::All;
        self
    }

    /// The subcommand, for messages: the first argument that is neither an option nor
    /// the value of `-c`/`-C`.
    fn label(&self) -> String {
        let mut args = self.args.iter().map(|arg| arg.to_string_lossy());
        while let Some(arg) = args.next() {
            if arg == "-c" || arg == "-C" {
                args.next();
            } else if !arg.starts_with('-') {
                return arg.into_owned();
            }
        }
        String::new()
    }

    /// The variables removed from the inherited environment.
    fn removed(&self) -> Vec<OsString> {
        let explicit = |key: &OsString| {
            self.env.iter().any(|(set, _)| set == key)
                || NON_INTERACTIVE.iter().any(|(set, _)| key == set)
        };
        let mut removed: Vec<OsString> = match self.scrub {
            Scrub::Location => LOCATION.iter().map(OsString::from).collect(),
            Scrub::All => std::env::vars_os()
                .map(|(key, _)| key)
                .filter(|key| {
                    key.to_string_lossy()
                        .to_ascii_uppercase()
                        .starts_with("GIT_")
                })
                .collect(),
        };
        removed.extend(self.remove.iter().cloned());
        removed.retain(|key| !explicit(key));
        removed
    }

    fn environment(&self) -> Vec<(OsString, OsString)> {
        let mut env: Vec<(OsString, OsString)> = NON_INTERACTIVE
            .iter()
            .filter(|(key, _)| !self.env.iter().any(|(set, _)| set == key))
            .map(|(key, value)| (OsString::from(key), OsString::from(value)))
            .collect();
        env.extend(self.env.iter().cloned());
        env
    }

    async fn run(self, cancel: &CancellationToken) -> Result<(String, RawOutput)> {
        let label = self.label();
        let mut argv = Argv::new(&self.dir, "git")
            .args(self.args.iter().cloned())
            .timeout(self.timeout)
            .limit(self.limit)
            .direct();
        argv.env_remove = self.removed();
        argv.env = self.environment();
        argv.stdin = self.stdin.clone();
        let output = run_argv(argv, cancel).await.map_err(|err| match err {
            Error::Cancelled => Error::Cancelled,
            Error::Io(err) => Error::Tool(format!("找不到 git（{err}）；请安装 git 并加入 PATH")),
            other => other,
        })?;
        if output.timed_out {
            return Err(Error::Tool(format!(
                "git {label} 超时（{} 秒）",
                self.timeout.as_secs()
            )));
        }
        Ok((label, output))
    }

    /// The run as `run_argv` reports it (exit code, truncation, timeout), for callers
    /// that judge these themselves; only failing to start or cancellation is an error.
    pub async fn raw(self, cancel: &CancellationToken) -> Result<RawOutput> {
        let mut argv = Argv::new(&self.dir, "git")
            .args(self.args.iter().cloned())
            .timeout(self.timeout)
            .limit(self.limit)
            .direct();
        argv.env_remove = self.removed();
        argv.env = self.environment();
        argv.stdin = self.stdin.clone();
        run_argv(argv, cancel).await
    }

    /// Runs to the end; a non-zero exit is reported in `ok`, not as an error.
    /// Output beyond the limit is an error.
    pub async fn output(self, cancel: &CancellationToken) -> Result<GitOutput> {
        let limit = self.limit;
        let (label, output) = self.run(cancel).await?;
        if output.truncated {
            return Err(Error::Tool(format!("git {label} 输出超过 {} 字节", limit)));
        }
        Ok(GitOutput {
            ok: output.success(),
            stdout: output.stdout,
            stderr: output.stderr,
        })
    }

    /// Stdout of a successful run; a failure carries git's own message.
    pub async fn bytes(self, cancel: &CancellationToken) -> Result<Vec<u8>> {
        let label = self.label();
        let output = self.output(cancel).await?;
        if !output.ok {
            let stderr = output.stderr_text();
            let detail = if stderr.trim().is_empty() {
                output.stdout_text()
            } else {
                stderr
            };
            return Err(Error::Tool(format!(
                "git {label} 失败：{}",
                crate::text::clip(detail.trim(), 2000, "…")
            )));
        }
        Ok(output.stdout)
    }

    /// [`bytes`](Self::bytes) as text (not trimmed).
    pub async fn text(self, cancel: &CancellationToken) -> Result<String> {
        Ok(String::from_utf8_lossy(&self.bytes(cancel).await?).into_owned())
    }

    /// A read-only query: trimmed stdout, or `None` on any failure.
    pub async fn query(self, cancel: &CancellationToken) -> Option<String> {
        self.text(cancel)
            .await
            .ok()
            .map(|text| text.trim_end().to_owned())
    }

    /// [`query`](Self::query) for synchronous callers (building the system prompt):
    /// polled to the timeout, then killed. Like every other run it never sees the
    /// protected variables, and output past the limit is a failure, not a cut.
    pub fn query_blocking(self) -> Option<String> {
        let mut command = std::process::Command::new("git");
        command
            .args(&self.args)
            .current_dir(&self.dir)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::null());
        for key in self.removed() {
            command.env_remove(key);
        }
        for key in crate::process::secrets::protected() {
            command.env_remove(key);
        }
        command.envs(self.environment());
        let mut child = command.spawn().ok()?;
        let mut stdout = child.stdout.take()?;
        let limit = self.limit;
        let reader = std::thread::spawn(move || {
            let mut kept = Vec::new();
            let mut buffer = [0_u8; 8192];
            loop {
                match stdout.read(&mut buffer) {
                    Ok(0) | Err(_) => return Some(kept),
                    Ok(count) if kept.len() + count > limit => return None,
                    Ok(count) => kept.extend_from_slice(&buffer[..count]),
                }
            }
        });
        let started = Instant::now();
        let status = loop {
            match child.try_wait() {
                Ok(Some(status)) => break status,
                Ok(None) if started.elapsed() < self.timeout => {
                    std::thread::sleep(Duration::from_millis(10));
                }
                _ => {
                    let _ = child.kill();
                    let _ = child.wait();
                    return None;
                }
            }
        };
        let stdout = reader.join().ok()??;
        status
            .success()
            .then(|| String::from_utf8_lossy(&stdout).trim_end().to_owned())
    }
}

/// A git error's own message, for callers that wrap it in another error kind.
pub fn error_text(err: &Error) -> String {
    match err {
        Error::Tool(message) => message.clone(),
        other => other.to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn names(list: &[OsString]) -> Vec<String> {
        list.iter()
            .map(|key| key.to_string_lossy().into_owned())
            .collect()
    }

    #[test]
    fn location_variables_go_explicit_ones_stay() {
        let git = Git::new(Path::new(".")).env("GIT_INDEX_FILE", "/tmp/index");
        let removed = names(&git.removed());
        assert!(removed.contains(&"GIT_DIR".to_owned()));
        assert!(!removed.contains(&"GIT_INDEX_FILE".to_owned()));
        assert!(!removed.contains(&"GIT_SSH_COMMAND".to_owned()));
        let env: Vec<String> = git
            .environment()
            .iter()
            .map(|(key, value)| format!("{}={}", key.to_string_lossy(), value.to_string_lossy()))
            .collect();
        assert!(env.contains(&"GIT_TERMINAL_PROMPT=0".to_owned()));
        assert!(env.contains(&"GIT_INDEX_FILE=/tmp/index".to_owned()));
    }

    #[test]
    fn blocking_queries_fail_rather_than_cut_their_output() {
        if Git::new(Path::new("."))
            .arg("--version")
            .query_blocking()
            .is_none()
        {
            return; // no git here
        }
        assert!(
            Git::new(Path::new("."))
                .arg("--version")
                .limit(4)
                .query_blocking()
                .is_none()
        );
    }

    #[test]
    fn the_label_is_the_subcommand() {
        let git = Git::new(Path::new(".")).args(["-c", "x=y", "status", "--porcelain"]);
        assert_eq!(git.label(), "status");
    }

    #[tokio::test]
    async fn queries_run_and_failures_are_none() {
        let dir = tempfile::tempdir().unwrap();
        let cancel = CancellationToken::new();
        let version = Git::new(dir.path()).arg("--version").query(&cancel).await;
        assert!(version.is_some_and(|text| text.starts_with("git version")));
        assert!(
            Git::new(dir.path())
                .args(["rev-parse", "HEAD"])
                .query(&cancel)
                .await
                .is_none()
        );
        let error = Git::new(dir.path())
            .args(["rev-parse", "HEAD"])
            .text(&cancel)
            .await
            .unwrap_err()
            .to_string();
        assert!(error.contains("git rev-parse 失败"), "{error}");
        assert!(
            Git::new(dir.path())
                .arg("--version")
                .query_blocking()
                .is_some_and(|text| text.starts_with("git version"))
        );
    }
}
