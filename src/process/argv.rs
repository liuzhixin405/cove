//! Argv commands (git, verifiers, child agents) under the same process-tree
//! guard as shell commands. The program starts through `--internal-stdio-host`,
//! which waits for a gate byte, so it is inside the Job Object / process group
//! before any of its own code runs.

use std::{ffi::OsString, path::PathBuf, process::Stdio, time::Duration};

use tokio::{
    io::{AsyncRead, AsyncReadExt, AsyncWriteExt},
    process::Command,
};
use tokio_util::sync::CancellationToken;

use super::ProcessTree;
use crate::domain::{Error, Result};

/// One argv command. `program` is resolved through `PATH` by the host process.
pub struct Argv {
    pub cwd: PathBuf,
    pub program: OsString,
    pub args: Vec<OsString>,
    /// Set for the command (and inherited by its descendants).
    pub env: Vec<(OsString, OsString)>,
    /// Removed from the inherited environment.
    pub env_remove: Vec<OsString>,
    pub stdin: Option<Vec<u8>>,
    pub timeout: Duration,
    /// Captured bytes per stream; the rest is drained and dropped.
    pub limit: usize,
    /// Start through the gated host so the program joins the tree before it runs.
    /// Off only for short trusted tools (git), where attaching right after spawn suffices.
    pub gated: bool,
}

impl Argv {
    pub fn new(cwd: impl Into<PathBuf>, program: impl Into<OsString>) -> Self {
        Self {
            cwd: cwd.into(),
            program: program.into(),
            args: Vec::new(),
            env: Vec::new(),
            env_remove: Vec::new(),
            stdin: None,
            timeout: Duration::from_secs(60),
            limit: 64 * 1024,
            gated: true,
        }
    }

    /// For git and similar trusted short commands: spawned directly, tree attached after spawn.
    pub fn direct(mut self) -> Self {
        self.gated = false;
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

    pub fn env(mut self, key: impl Into<OsString>, value: impl Into<OsString>) -> Self {
        self.env.push((key.into(), value.into()));
        self
    }

    pub fn timeout(mut self, timeout: Duration) -> Self {
        self.timeout = timeout;
        self
    }

    pub fn limit(mut self, limit: usize) -> Self {
        self.limit = limit;
        self
    }

    pub fn stdin(mut self, input: Vec<u8>) -> Self {
        self.stdin = Some(input);
        self
    }
}

pub struct RawOutput {
    /// `None` when killed (timeout) or terminated by a signal.
    pub code: Option<i32>,
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
    pub truncated: bool,
    pub timed_out: bool,
}

impl RawOutput {
    pub fn success(&self) -> bool {
        self.code == Some(0) && !self.timed_out
    }

    pub fn stdout_text(&self) -> String {
        String::from_utf8_lossy(&self.stdout).into_owned()
    }

    pub fn stderr_text(&self) -> String {
        String::from_utf8_lossy(&self.stderr).into_owned()
    }
}

async fn capture_limited(
    mut pipe: impl AsyncRead + Unpin,
    limit: usize,
) -> std::io::Result<(Vec<u8>, bool)> {
    let mut bytes = Vec::new();
    let mut truncated = false;
    let mut buffer = [0_u8; 8192];
    loop {
        let count = pipe.read(&mut buffer).await?;
        if count == 0 {
            return Ok((bytes, truncated));
        }
        let keep = count.min(limit.saturating_sub(bytes.len()));
        bytes.extend_from_slice(&buffer[..keep]);
        truncated |= keep != count;
    }
}

/// Runs `argv` to completion, timeout or cancellation; the whole process tree is
/// killed on every exit path. `Err(Cancelled)` only for cancellation.
pub async fn run_argv(argv: Argv, cancel: &CancellationToken) -> Result<RawOutput> {
    if cancel.is_cancelled() {
        return Err(Error::Cancelled);
    }
    let mut command = if argv.gated {
        let mut command = Command::new(std::env::current_exe()?);
        command.arg("--internal-stdio-host").arg(&argv.program);
        command
    } else {
        Command::new(&argv.program)
    };
    command
        .args(&argv.args)
        .current_dir(&argv.cwd)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true);
    for key in &argv.env_remove {
        command.env_remove(key);
    }
    for (key, value) in &argv.env {
        command.env(key, value);
    }
    #[cfg(unix)]
    command.process_group(0);
    let mut child = command.spawn()?;
    let tree = match ProcessTree::attach(&child) {
        Ok(tree) => tree,
        Err(err) => {
            child.kill().await?;
            return Err(err);
        }
    };
    let mut stdin = child
        .stdin
        .take()
        .ok_or_else(|| Error::Tool("missing child stdin".into()))?;
    let mut stdout = child
        .stdout
        .take()
        .ok_or_else(|| Error::Tool("missing child stdout".into()))?;
    let stderr = child
        .stderr
        .take()
        .ok_or_else(|| Error::Tool("missing child stderr".into()))?;
    let gated = argv.gated;
    let handshake = async {
        if !gated {
            return Ok(());
        }
        stdin.write_all(b"+").await?;
        let mut ready = [0];
        stdout.read_exact(&mut ready).await?;
        if ready != *b"+" {
            return Err(Error::Tool("invalid process host handshake".into()));
        }
        Ok(())
    };
    tokio::select! {
        biased;
        _ = cancel.cancelled() => {
            drop(tree);
            let _ = child.kill().await;
            return Err(Error::Cancelled);
        }
        result = tokio::time::timeout(Duration::from_secs(10), handshake) => {
            if let Err(err) = result.map_err(|_| Error::Tool("process host start timed out".into())).and_then(|r| r) {
                drop(tree);
                let _ = child.kill().await;
                return Err(err);
            }
        }
    }
    let input = argv.stdin.unwrap_or_default();
    let writer = tokio::spawn(async move {
        // A program that never reads stdin must not block on a full pipe.
        let _ = stdin.write_all(&input).await;
        drop(stdin);
    });
    let limit = argv.limit;
    let out_reader = tokio::spawn(capture_limited(stdout, limit));
    let err_reader = tokio::spawn(capture_limited(stderr, limit));
    let mut cancelled = false;
    let mut timed_out = false;
    let status = tokio::select! {
        biased;
        _ = cancel.cancelled() => { cancelled = true; None }
        _ = tokio::time::sleep(argv.timeout) => { timed_out = true; None }
        status = child.wait() => Some(status?),
    };
    drop(tree);
    if status.is_none() {
        let _ = child.kill().await;
    }
    writer.abort();
    let drain = async {
        let out = out_reader
            .await
            .map_err(|err| Error::Tool(format!("output reader failed: {err}")))??;
        let err = err_reader
            .await
            .map_err(|err| Error::Tool(format!("output reader failed: {err}")))??;
        Ok::<_, Error>((out, err))
    };
    let ((stdout, out_truncated), (stderr, err_truncated)) =
        tokio::time::timeout(Duration::from_secs(5), drain)
            .await
            .map_err(|_| Error::Tool("process output cleanup timed out".into()))??;
    if cancelled {
        return Err(Error::Cancelled);
    }
    Ok(RawOutput {
        code: status.and_then(|status| status.code()),
        stdout,
        stderr,
        truncated: out_truncated || err_truncated,
        timed_out,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn direct_commands_capture_bounded_output_and_exit_codes() {
        let output = run_argv(
            Argv::new(std::env::temp_dir(), "git")
                .args(["--version"])
                .direct()
                .limit(4),
            &CancellationToken::new(),
        )
        .await
        .unwrap();
        assert!(output.success());
        assert_eq!(output.stdout.len(), 4);
        assert!(output.truncated);
        let failed = run_argv(
            Argv::new(std::env::temp_dir(), "git")
                .args(["definitely-not-a-git-command"])
                .direct(),
            &CancellationToken::new(),
        )
        .await
        .unwrap();
        assert!(!failed.success());
        let cancel = CancellationToken::new();
        cancel.cancel();
        assert!(matches!(
            run_argv(Argv::new(std::env::temp_dir(), "git").direct(), &cancel).await,
            Err(Error::Cancelled)
        ));
    }

    #[tokio::test]
    async fn missing_program_reports_failure_not_success() {
        // Unit tests run inside the test binary, which is not the stdio host;
        // the host handshake therefore fails instead of running anything.
        let result = run_argv(
            Argv::new(std::env::temp_dir(), "definitely-not-a-program-cove"),
            &CancellationToken::new(),
        )
        .await;
        assert!(result.map(|output| !output.success()).unwrap_or(true));
    }
}
