use super::{ProcessTree, capture};
use crate::domain::{Error, Result};
use std::{path::Path, process::Stdio, time::Duration};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    process::Command,
};
use tokio_util::sync::CancellationToken;

pub(super) fn clean_environment(command: &mut Command) -> Result<()> {
    command.env_clear();
    for name in [
        "PATH",
        "SystemRoot",
        "WINDIR",
        "TEMP",
        "TMP",
        "HOME",
        "USERPROFILE",
        "APPDATA",
        "LOCALAPPDATA",
        "LANG",
        "LC_ALL",
        // Windows: without these, PowerShell cannot resolve `cmd`/`python` on PATH and
        // native programs miss their system directories. None of them carries secrets.
        "PATHEXT",
        "ComSpec",
        "SystemDrive",
        "ProgramData",
        "ProgramFiles",
        "ProgramFiles(x86)",
        "CommonProgramFiles",
        "NUMBER_OF_PROCESSORS",
        "PROCESSOR_ARCHITECTURE",
        "OS",
    ] {
        if let Some(value) = std::env::var_os(name) {
            command.env(name, value);
        }
    }
    Ok(())
}

type StderrReader = tokio::task::JoinHandle<std::io::Result<(Vec<u8>, bool)>>;
pub struct ManagedChild {
    child: tokio::process::Child,
    tree: Option<ProcessTree>,
    pub stdin: Option<tokio::process::ChildStdin>,
    pub stdout: Option<tokio::process::ChildStdout>,
    stderr: Option<StderrReader>,
}
impl ManagedChild {
    pub async fn spawn(
        host: &Path,
        cwd: &Path,
        program: &str,
        args: &[String],
        env: &[String],
        cancel: &CancellationToken,
    ) -> Result<Self> {
        Self::spawn_with(host, cwd, program, args, env, &[], cancel).await
    }

    /// `spawn` plus explicit variables (configured MCP `env` maps; never logged or saved).
    pub async fn spawn_with(
        host: &Path,
        cwd: &Path,
        program: &str,
        args: &[String],
        env: &[String],
        values: &[(String, String)],
        cancel: &CancellationToken,
    ) -> Result<Self> {
        let mut command = Command::new(host);
        command
            .arg("--internal-stdio-host")
            .arg(program)
            .args(args)
            .current_dir(cwd)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .kill_on_drop(true);
        clean_environment(&mut command)?;
        for name in env {
            command.env(
                name,
                std::env::var_os(name).ok_or_else(|| {
                    Error::Tool(format!("missing MCP environment variable {name}"))
                })?,
            );
        }
        for (name, value) in values {
            command.env(name, value);
        }
        #[cfg(unix)]
        command.process_group(0);
        let mut child = command.spawn()?;
        let tree = ProcessTree::attach(&child)?;
        let stdin = child.stdin.take();
        let stdout = child.stdout.take();
        let stderr = child
            .stderr
            .take()
            .ok_or_else(|| Error::Tool("missing MCP stderr".into()))?;
        let mut owned = Self {
            child,
            tree: Some(tree),
            stdin,
            stdout,
            stderr: Some(tokio::spawn(capture(stderr, None, super::STDERR_LIMIT))),
        };
        let start = async {
            owned
                .stdin
                .as_mut()
                .ok_or_else(|| Error::Tool("missing MCP stdin".into()))?
                .write_all(b"+")
                .await?;
            let mut ready = [0];
            owned
                .stdout
                .as_mut()
                .ok_or_else(|| Error::Tool("missing MCP stdout".into()))?
                .read_exact(&mut ready)
                .await?;
            if ready != *b"+" {
                return Err(Error::Tool("invalid stdio host handshake".into()));
            }
            Ok(())
        };
        tokio::select! {
            biased;
            _ = cancel.cancelled() => return Err(Error::Cancelled),
            result = tokio::time::timeout(Duration::from_secs(5), start) => {
                result.map_err(|_| Error::Tool("stdio host start timed out".into()))??;
            }
        }
        Ok(owned)
    }
    pub async fn stop(&mut self) -> Result<String> {
        self.tree.take();
        tokio::time::timeout(Duration::from_secs(3), self.child.wait())
            .await
            .map_err(|_| Error::Tool("MCP process cleanup timed out".into()))??;
        let Some(reader) = self.stderr.take() else {
            return Ok(String::new());
        };
        let (bytes, truncated) = tokio::time::timeout(Duration::from_secs(2), reader)
            .await
            .map_err(|_| Error::Tool("MCP stderr cleanup timed out".into()))?
            .map_err(|err| Error::Tool(format!("MCP stderr reader failed: {err}")))??;
        let mut text = String::from_utf8_lossy(&bytes).into_owned();
        if truncated {
            text.push_str("\n[stderr truncated]");
        }
        Ok(text)
    }
}
impl Drop for ManagedChild {
    fn drop(&mut self) {
        self.tree.take();
        if let Some(reader) = self.stderr.take() {
            reader.abort();
        }
    }
}
pub fn stdio_host() -> Result<i32> {
    use std::io::{Read, Write};
    // Only the gate byte is available until this acknowledgement, so stdin cannot
    // buffer MCP bytes that must subsequently be inherited by the real server.
    let mut gate = [0];
    std::io::stdin().read_exact(&mut gate)?;
    if gate != *b"+" {
        return Err(Error::Tool("invalid stdio host gate".into()));
    }
    std::io::stdout().write_all(b"+")?;
    std::io::stdout().flush()?;
    let mut args = std::env::args_os().skip(2);
    let program = args
        .next()
        .ok_or_else(|| Error::Tool("missing stdio command".into()))?;
    let status = std::process::Command::new(program)
        .args(args)
        .stdin(Stdio::inherit())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit())
        .status()?;
    Ok(status.code().unwrap_or(1))
}
