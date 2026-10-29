use std::{collections::VecDeque, path::Path, process::Stdio, sync::Arc, time::Duration};

use tokio::{
    io::{AsyncRead, AsyncReadExt},
    process::Command,
};
use tokio_util::sync::CancellationToken;

use crate::domain::{Error, Progress, Result};

mod argv;
mod stdio;
pub use argv::{Argv, RawOutput, run_argv};
mod shell;
pub use shell::{Shell, ShellKind, inherited_git_config_count, noninteractive_env};
use stdio::clean_environment;
pub use stdio::{ManagedChild, stdio_host};

/// Captured stdout per command; beyond it the middle is dropped (head and tail kept).
pub const STDOUT_LIMIT: usize = 30 * 1024;
/// Captured stderr per command.
pub const STDERR_LIMIT: usize = 10 * 1024;

pub struct CommandOutput {
    pub code: Option<i32>,
    pub stdout: String,
    pub stderr: String,
    pub truncated: bool,
    pub timed_out: bool,
    pub encoding_warning: bool,
}

/// Live display stops after this much per stream; the captured result has its own cap.
const LIVE_LIMIT: usize = 256 * 1024;
const LIVE_LINE: usize = 4096;

/// Splits a stream into display lines for `Progress`.
struct Live {
    progress: Arc<Progress>,
    stderr: bool,
    pending: Vec<u8>,
    shown: usize,
}

impl Live {
    async fn feed(&mut self, bytes: &[u8]) {
        if self.shown > LIVE_LIMIT {
            return;
        }
        self.pending.extend_from_slice(bytes);
        while let Some(end) = self.pending.iter().position(|byte| *byte == b'\n').or((self
            .pending
            .len()
            >= LIVE_LINE)
            .then_some(LIVE_LINE - 1))
        {
            let line: Vec<u8> = self.pending.drain(..=end).collect();
            self.emit(&line).await;
            if self.shown > LIVE_LIMIT {
                self.pending.clear();
                let note = "[live output limit reached; the result keeps its own capture]";
                self.progress.line(note.into(), self.stderr).await;
                return;
            }
        }
    }

    async fn emit(&mut self, line: &[u8]) {
        self.shown += line.len();
        let (text, _) = crate::tools::text_encoding::decode_output_line(line);
        let text = text.trim_end_matches(['\n', '\r']);
        self.progress.line(text.to_owned(), self.stderr).await;
    }

    async fn finish(&mut self) {
        if !self.pending.is_empty() && self.shown <= LIVE_LIMIT {
            let line = std::mem::take(&mut self.pending);
            self.emit(&line).await;
        }
    }
}

/// Keeps the first 40% and the last 60% of a stream within `limit` bytes. Build and test
/// output puts the summary and the failure at the end, so the tail gets the larger share.
struct HeadTail {
    head: Vec<u8>,
    tail: VecDeque<u8>,
    head_limit: usize,
    tail_limit: usize,
    dropped: usize,
}

impl HeadTail {
    fn new(limit: usize) -> Self {
        let head_limit = limit * 2 / 5;
        Self {
            head: Vec::new(),
            tail: VecDeque::new(),
            head_limit,
            tail_limit: limit - head_limit,
            dropped: 0,
        }
    }

    fn push(&mut self, mut bytes: &[u8]) {
        let room = self.head_limit.saturating_sub(self.head.len());
        if room > 0 {
            let take = room.min(bytes.len());
            self.head.extend_from_slice(&bytes[..take]);
            bytes = &bytes[take..];
        }
        self.tail.extend(bytes);
        let over = self.tail.len().saturating_sub(self.tail_limit);
        if over > 0 {
            self.tail.drain(..over);
            self.dropped += over;
        }
    }

    /// The kept bytes; a cut is moved to line boundaries and marked.
    fn finish(self) -> (Vec<u8>, bool) {
        let mut tail: Vec<u8> = self.tail.into();
        if self.dropped == 0 {
            let mut all = self.head;
            all.extend_from_slice(&tail);
            return (all, false);
        }
        let mut head = self.head;
        let mut omitted = self.dropped;
        if let Some(end) = head.iter().rposition(|byte| *byte == b'\n') {
            omitted += head.len() - end - 1;
            head.truncate(end + 1);
        }
        if let Some(start) = tail.iter().position(|byte| *byte == b'\n') {
            omitted += start + 1;
            tail.drain(..=start);
        }
        if !head.ends_with(b"\n") {
            head.push(b'\n');
        }
        head.extend_from_slice(
            format!("[... {omitted} bytes omitted from the middle of this output ...]\n")
                .as_bytes(),
        );
        head.extend_from_slice(&tail);
        (head, true)
    }
}

async fn capture(
    mut pipe: impl AsyncRead + Unpin,
    mut live: Option<Live>,
    limit: usize,
) -> std::io::Result<(Vec<u8>, bool)> {
    let mut kept = HeadTail::new(limit);
    let mut buffer = [0_u8; 4096];
    loop {
        let count = pipe.read(&mut buffer).await?;
        if count == 0 {
            break;
        }
        if let Some(live) = &mut live {
            live.feed(&buffer[..count]).await;
        }
        kept.push(&buffer[..count]);
    }
    if let Some(live) = &mut live {
        live.finish().await;
    }
    Ok(kept.finish())
}

/// Runs `script` in the legacy shell (PowerShell on Windows, sh elsewhere).
pub async fn shell(
    cwd: &Path,
    script: &str,
    timeout: Duration,
    cancel: CancellationToken,
) -> Result<CommandOutput> {
    shell_configured(cwd, script, timeout, cancel, None).await
}

/// `shell` that also forwards output lines to `progress` while the command runs.
pub async fn shell_live(
    cwd: &Path,
    script: &str,
    timeout: Duration,
    cancel: CancellationToken,
    progress: Option<Arc<Progress>>,
) -> Result<CommandOutput> {
    run(
        &Shell::legacy(),
        cwd,
        script,
        timeout,
        cancel,
        None,
        progress,
    )
    .await
}

pub async fn shell_configured(
    cwd: &Path,
    script: &str,
    timeout: Duration,
    cancel: CancellationToken,
    input: Option<&str>,
) -> Result<CommandOutput> {
    run(&Shell::legacy(), cwd, script, timeout, cancel, input, None).await
}

/// Runs `script` in `shell`, streaming output lines to `progress`.
pub async fn run_shell(
    shell: &Shell,
    cwd: &Path,
    script: &str,
    timeout: Duration,
    cancel: CancellationToken,
    progress: Option<Arc<Progress>>,
) -> Result<CommandOutput> {
    run(shell, cwd, script, timeout, cancel, None, progress).await
}

/// PowerShell reads the script from stdin after the process has joined its Job Object.
const POWERSHELL_HOST: &str = "$ErrorActionPreference='Stop'; $utf8=New-Object System.Text.UTF8Encoding $false; [Console]::InputEncoding=$utf8; [Console]::OutputEncoding=$utf8; $OutputEncoding=$utf8; $script=[Console]::In.ReadToEnd(); try { & ([scriptblock]::Create($script)); if (-not $?) { exit 1 }; if ($null -ne $LASTEXITCODE) { exit $LASTEXITCODE } } catch { [Console]::Error.WriteLine($_.ToString()); exit 1 }";

/// bash on Windows reads the script the same way; `cat` leaves stdin at EOF for the script.
#[cfg(windows)]
const BASH_HOST: &str = "eval \"$(cat)\"";

fn command_for(shell: &Shell, script: &str) -> (Command, Option<String>) {
    let mut command = Command::new(&shell.path);
    match shell.kind {
        ShellKind::PowerShell => {
            command.args([
                "-NoLogo",
                "-NoProfile",
                "-NonInteractive",
                "-Command",
                POWERSHELL_HOST,
            ]);
            command.stdin(Stdio::piped());
            (command, Some(script.to_owned()))
        }
        ShellKind::Posix => {
            #[cfg(windows)]
            {
                command.args(["--noprofile", "--norc", "-c", BASH_HOST]);
                command.env("CHERE_INVOKING", "1");
                command.stdin(Stdio::piped());
                // A CR left by the model ends every bash line with a stray `\r`.
                (command, Some(script.replace("\r\n", "\n")))
            }
            #[cfg(unix)]
            {
                command.args(["-c", script]).process_group(0);
                command.stdin(Stdio::null());
                (command, None)
            }
        }
    }
}

async fn run(
    shell: &Shell,
    cwd: &Path,
    script: &str,
    timeout: Duration,
    cancel: CancellationToken,
    input: Option<&str>,
    progress: Option<Arc<Progress>>,
) -> Result<CommandOutput> {
    if cancel.is_cancelled() {
        return Err(Error::Cancelled);
    }
    let (mut command, stdin_script) = command_for(shell, script);
    command
        .current_dir(cwd)
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true);
    let inherited = if let Some(input) = input {
        clean_environment(&mut command)?;
        command.env("COVE_HOOK_INPUT", input);
        0
    } else {
        inherited_git_config_count()
    };
    command.envs(noninteractive_env(inherited));
    let mut child = command.spawn()?;
    let tree = match ProcessTree::attach(&child) {
        Ok(tree) => tree,
        Err(err) => {
            child.kill().await?;
            return Err(err);
        }
    };
    if let Some(script) = stdin_script {
        use tokio::io::AsyncWriteExt;
        let mut stdin = child
            .stdin
            .take()
            .ok_or_else(|| Error::Tool("missing child stdin".into()))?;
        tokio::select! {
            _ = cancel.cancelled() => {
                drop(tree);
                child.kill().await?;
                return Err(Error::Cancelled);
            }
            result = stdin.write_all(script.as_bytes()) => { result?; }
        }
        drop(stdin);
    }
    let stdout = child
        .stdout
        .take()
        .ok_or_else(|| Error::Tool("missing stdout".into()))?;
    let stderr = child
        .stderr
        .take()
        .ok_or_else(|| Error::Tool("missing stderr".into()))?;
    // Drain both pipes even beyond the display cap, or a full pipe can deadlock wait().
    let mut readers = tokio::task::JoinSet::new();
    let live = |stderr| {
        progress.clone().map(|progress| Live {
            progress,
            stderr,
            pending: Vec::new(),
            shown: 0,
        })
    };
    let (out_live, err_live) = (live(false), live(true));
    readers.spawn(async move { (true, capture(stdout, out_live, STDOUT_LIMIT).await) });
    readers.spawn(async move { (false, capture(stderr, err_live, STDERR_LIMIT).await) });
    let mut cancelled = false;
    let mut timed_out = false;
    let status = tokio::select! {
        biased;
        _ = cancel.cancelled() => { cancelled = true; None },
        _ = tokio::time::sleep(timeout) => { timed_out = true; None },
        status = child.wait() => Some(status?),
    };
    drop(tree);
    if status.is_none() {
        child.kill().await?;
    }
    let mut output = CommandOutput {
        code: status.and_then(|status| status.code()),
        stdout: String::new(),
        stderr: String::new(),
        truncated: false,
        timed_out,
        encoding_warning: false,
    };
    let drain = async {
        while let Some(joined) = readers.join_next().await {
            let (stdout, result) =
                joined.map_err(|err| Error::Tool(format!("output reader failed: {err}")))?;
            let (bytes, truncated) = result?;
            output.truncated |= truncated;
            let (text, lossy) = crate::tools::text_encoding::decode_output(&bytes);
            output.encoding_warning |= lossy;
            if stdout {
                output.stdout = text;
            } else {
                output.stderr = text;
            }
        }
        Ok::<_, Error>(())
    };
    tokio::time::timeout(Duration::from_secs(5), drain)
        .await
        .map_err(|_| Error::Tool("process output cleanup timed out".into()))??;
    if cancelled {
        return Err(Error::Cancelled);
    }
    Ok(output)
}

#[cfg(windows)]
struct ProcessTree(windows_sys::Win32::Foundation::HANDLE);

#[cfg(windows)]
// The job handle is owned exclusively by this guard and only closed on drop.
unsafe impl Send for ProcessTree {}

#[cfg(windows)]
impl ProcessTree {
    fn attach(child: &tokio::process::Child) -> Result<Self> {
        use windows_sys::Win32::{
            Foundation::CloseHandle,
            System::{
                JobObjects::{
                    AssignProcessToJobObject, CreateJobObjectW, JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
                    JOBOBJECT_EXTENDED_LIMIT_INFORMATION, JobObjectExtendedLimitInformation,
                    SetInformationJobObject,
                },
                Threading::{OpenProcess, PROCESS_SET_QUOTA, PROCESS_TERMINATE},
            },
        };
        let pid = child
            .id()
            .ok_or_else(|| Error::Tool("child already exited".into()))?;
        // All pointers refer to initialized local values; handles are checked and owned.
        unsafe {
            let job = CreateJobObjectW(std::ptr::null(), std::ptr::null());
            if job.is_null() {
                return Err(std::io::Error::last_os_error().into());
            }
            let guard = Self(job);
            let mut limits: JOBOBJECT_EXTENDED_LIMIT_INFORMATION = std::mem::zeroed();
            limits.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
            if SetInformationJobObject(
                job,
                JobObjectExtendedLimitInformation,
                (&limits as *const JOBOBJECT_EXTENDED_LIMIT_INFORMATION).cast(),
                std::mem::size_of_val(&limits) as u32,
            ) == 0
            {
                return Err(std::io::Error::last_os_error().into());
            }
            let process = OpenProcess(PROCESS_SET_QUOTA | PROCESS_TERMINATE, 0, pid);
            if process.is_null() {
                return Err(std::io::Error::last_os_error().into());
            }
            let assigned = AssignProcessToJobObject(job, process);
            let err = if assigned == 0 {
                Some(std::io::Error::last_os_error())
            } else {
                None
            };
            CloseHandle(process);
            if let Some(err) = err {
                return Err(err.into());
            }
            Ok(guard)
        }
    }
}

#[cfg(windows)]
impl Drop for ProcessTree {
    fn drop(&mut self) {
        unsafe {
            windows_sys::Win32::Foundation::CloseHandle(self.0);
        }
    }
}

#[cfg(unix)]
struct ProcessTree(i32);

#[cfg(unix)]
impl ProcessTree {
    fn attach(child: &tokio::process::Child) -> Result<Self> {
        let pid = child
            .id()
            .ok_or_else(|| Error::Tool("child already exited".into()))?;
        let pid = i32::try_from(pid).map_err(|_| Error::Tool("process id out of range".into()))?;
        Ok(Self(pid))
    }
}

#[cfg(unix)]
impl Drop for ProcessTree {
    fn drop(&mut self) {
        // Negative PID addresses only the process group created for this command.
        unsafe {
            libc::kill(-self.0, libc::SIGKILL);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn long_output_keeps_head_and_tail_at_line_boundaries() {
        let mut kept = HeadTail::new(100);
        let text: String = (0..50).map(|n| format!("line {n:02}\n")).collect();
        kept.push(text.as_bytes());
        let (bytes, truncated) = kept.finish();
        let out = String::from_utf8(bytes).unwrap();
        assert!(truncated);
        assert!(out.starts_with("line 00\n"), "{out}");
        assert!(out.ends_with("line 49\n"), "{out}");
        assert!(out.contains("bytes omitted"), "{out}");
        assert!(
            out.lines()
                .all(|line| line.starts_with("line ") || line.starts_with("[..."))
        );
        let mut small = HeadTail::new(100);
        small.push(b"short\n");
        assert_eq!(small.finish(), (b"short\n".to_vec(), false));
    }
}
