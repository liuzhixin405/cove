use std::{
    collections::VecDeque,
    path::Path,
    process::Stdio,
    sync::{Arc, OnceLock},
    time::{Duration, Instant},
};

use futures_util::future::BoxFuture;
use tokio::{
    io::{AsyncRead, AsyncReadExt},
    process::Command,
};
use tokio_util::sync::CancellationToken;

use crate::domain::{Error, Progress, Result};

mod argv;
pub mod git;
pub mod secrets;
mod stdio;
pub(crate) use argv::capture_limited;
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
    /// Stopped at the total limit.
    pub timed_out: bool,
    /// Stopped because the process tree showed no sign of work for the idle limit: why,
    /// for the user and the model (see [`Limit::idle`]).
    pub stalled: Option<String>,
    pub encoding_warning: bool,
}

/// Default idle limit of test and build commands: long enough for an integration test
/// that waits for a container or a slow service.
pub const IDLE_LIMIT: Duration = Duration::from_secs(180);
/// Default total limit of test and build commands that are watched for idleness: only
/// a run that keeps working without end (a busy loop) gets this far.
pub const WATCHED_TOTAL: Duration = Duration::from_secs(3600);
/// How often a watched run's process tree is sampled (faster in unit tests).
const SAMPLE_EVERY: Duration = if cfg!(test) {
    Duration::from_millis(200)
} else {
    Duration::from_secs(5)
};
/// CPU time per sample below this is scheduling noise of a waiting process, not work.
const CPU_NOISE: Duration = Duration::from_millis(50);

/// How long a command may run.
#[derive(Clone)]
pub struct Limit {
    /// Bounds the run whatever it does.
    pub total: Duration,
    /// Stops the run once its process tree shows no sign of work for this long: no
    /// output, no CPU time beyond noise, no reads or writes, no process starting or
    /// ending. Where the tree cannot be sampled (not Windows or Linux) only `total` applies.
    pub idle: Option<Duration>,
    /// Status-row text refreshed while the run goes on: `<label> · <elapsed> · 活跃`.
    pub status: Option<(Arc<Progress>, String)>,
}

impl From<Duration> for Limit {
    fn from(total: Duration) -> Self {
        Self {
            total,
            idle: None,
            status: None,
        }
    }
}

impl Limit {
    /// A test or build run: stopped when idle for `idle`, or at `total`.
    pub fn watched(total: Duration, idle: Duration) -> Self {
        Self {
            total,
            idle: Some(idle),
            status: None,
        }
    }

    pub fn with_status(mut self, progress: Arc<Progress>, label: String) -> Self {
        self.status = Some((progress, label));
        self
    }
}

/// `45s`, `6m12s`, `1h02m`.
pub fn clock(duration: Duration) -> String {
    let seconds = duration.as_secs();
    match seconds {
        0..60 => format!("{seconds}s"),
        60..3600 => format!("{}m{:02}s", seconds / 60, seconds % 60),
        _ => format!("{}h{:02}m", seconds / 3600, seconds % 3600 / 60),
    }
}

/// One look at a process tree.
#[derive(Clone, PartialEq, Debug)]
struct TreeSample {
    /// CPU time of the tree; on Linux only of the processes alive now.
    cpu: Duration,
    /// Read and write operations, on Linux only of the processes alive now.
    io: u64,
    /// Sorted.
    pids: Vec<u32>,
}

/// Tracks whether a watched run still works.
struct Watch {
    idle: Duration,
    previous: Option<(u64, TreeSample)>,
    /// When work was last seen, and the tree's CPU time then.
    active: (Instant, Duration),
    output: Option<Instant>,
}

impl Watch {
    fn new(idle: Duration, now: Instant) -> Self {
        Self {
            idle,
            previous: None,
            active: (now, Duration::ZERO),
            output: None,
        }
    }

    /// Records a sample; `true` once the run has been idle for the limit.
    fn observe(&mut self, now: Instant, bytes: u64, tree: TreeSample) -> bool {
        let worked = match &self.previous {
            None => true,
            Some((before, previous)) => {
                if bytes != *before {
                    self.output = Some(now);
                }
                bytes != *before
                    || tree.pids != previous.pids
                    || tree.io > previous.io
                    || tree.cpu.saturating_sub(previous.cpu) > CPU_NOISE
            }
        };
        if worked {
            self.active = (now, tree.cpu);
        }
        self.previous = Some((bytes, tree));
        now.duration_since(self.active.0) >= self.idle
    }

    fn quiet_for(&self, now: Instant) -> Duration {
        now.duration_since(self.active.0)
    }

    /// Why the run was stopped, for the user and the model.
    fn reason(
        &self,
        now: Instant,
        started: Instant,
        script: &str,
        processes: &[(u32, String, Duration)],
    ) -> String {
        let cpu = self.previous.as_ref().map_or(Duration::ZERO, |(_, tree)| {
            tree.cpu.saturating_sub(self.active.1)
        });
        let mut text = format!(
            "已运行 {}，最近 {} 没有任何活动，已中止：没有输出，整个进程树的 CPU 时间只增加了 {:.2}s，没有读写，没有进程启动或退出。\n",
            clock(now.duration_since(started)),
            clock(self.quiet_for(now)),
            cpu.as_secs_f64()
        );
        match self.output {
            Some(at) => text.push_str(&format!(
                "最后一次输出在 {} 前。\n",
                clock(now.duration_since(at))
            )),
            None => text.push_str("启动以来没有新的输出。\n"),
        }
        if !processes.is_empty() {
            let list: Vec<String> = processes
                .iter()
                .take(8)
                .map(|(pid, name, cpu)| {
                    format!("{name}（pid {pid}，CPU 共 {:.1}s）", cpu.as_secs_f64())
                })
                .collect();
            let more = processes.len().saturating_sub(8);
            text.push_str(&format!(
                "仍在运行：{}{}。\n",
                list.join("、"),
                if more > 0 {
                    format!(" 等 {} 个进程", processes.len())
                } else {
                    String::new()
                }
            ));
        }
        text.push_str("可能原因：在等网络、数据库、容器、锁或输入，或者死锁。");
        if let Some(hint) = stall_hint(script) {
            text.push_str(hint);
        }
        text
    }
}

/// How to find the stuck test with the command's own tooling.
fn stall_hint(script: &str) -> Option<&'static str> {
    let has = |tool: &str| {
        script
            .split(['&', ';', '|', '\n'])
            .any(|part| part.trim_start().starts_with(tool))
    };
    if has("dotnet test") {
        Some("dotnet test（VSTest）加 --blame-hang --blame-hang-timeout 2m 可以打印卡住的测试名。")
    } else if has("pytest") || script.contains("-m pytest") {
        Some("装 pytest-timeout 后加 --timeout=120，可以打印卡住的测试和调用栈。")
    } else if has("go test") {
        Some("go test -timeout 2m 超时时会打印所有 goroutine 的调用栈。")
    } else if has("npm test") || has("npx jest") || has("jest") {
        Some("jest 加 --detectOpenHandles 可以找出没有关闭的句柄。")
    } else {
        None
    }
}

/// Live display stops after this much per stream; the captured result has its own cap.
const LIVE_LIMIT: usize = 256 * 1024;
const LIVE_LINE: usize = 4096;
const LIVE_PROGRESS_INTERVAL: Duration = Duration::from_secs(2);

/// Told when a command blocks on a lock another process holds; `child` is the
/// command's own process, so its descendants are not the holder.
pub trait LockWait: Send + Sync {
    /// Output fragments that mean "waiting on another process's lock".
    fn patterns(&self) -> &'static [&'static str];
    /// `line` is the output line that matched.
    fn waiting(
        &self,
        child: u32,
        line: String,
        cancel: CancellationToken,
    ) -> BoxFuture<'static, ()>;
}

static LOCK_HANDLER: OnceLock<Arc<dyn LockWait>> = OnceLock::new();

/// Commands of this process that run on in the background: their own process
/// id and a label, so a lock they hold is not mistaken for someone else's.
static BACKGROUND: std::sync::Mutex<Vec<(u32, String)>> = std::sync::Mutex::new(Vec::new());

/// Records that the command with process id `pid` runs in the background as `label`.
pub fn note_background(pid: u32, label: String) {
    if let Ok(mut running) = BACKGROUND.lock() {
        running.retain(|(known, _)| *known != pid);
        running.push((pid, label));
    }
}

pub fn forget_background(pid: u32) {
    if let Ok(mut running) = BACKGROUND.lock() {
        running.retain(|(known, _)| *known != pid);
    }
}

/// The label of the background command whose own process is `pid`.
pub fn background_label(pid: u32) -> Option<String> {
    BACKGROUND
        .lock()
        .ok()?
        .iter()
        .find(|(known, _)| *known == pid)
        .map(|(_, label)| label.clone())
}

/// Installs the lock-wait handler (once per process; later calls are ignored).
pub fn set_lock_wait(handler: Arc<dyn LockWait>) {
    let _ = LOCK_HANDLER.set(handler);
}

/// Splits a stream into display lines for `Progress`.
struct Live {
    progress: Arc<Progress>,
    stderr: bool,
    pending: Vec<u8>,
    skip_lf: bool,
    latest_progress: Option<Vec<u8>>,
    last_progress: Option<Instant>,
    shown: usize,
}

impl Live {
    async fn feed(&mut self, bytes: &[u8]) {
        if self.shown > LIVE_LIMIT {
            return;
        }
        self.pending.extend_from_slice(bytes);
        loop {
            if self.skip_lf {
                if self.pending.is_empty() {
                    break;
                }
                if self.pending[0] == b'\n' {
                    self.pending.remove(0);
                }
                self.skip_lf = false;
            }
            let Some((end, progress)) = self
                .pending
                .iter()
                .position(|byte| matches!(*byte, b'\n' | b'\r'))
                .map(|end| (end, self.pending[end] == b'\r'))
                .or((self.pending.len() >= LIVE_LINE).then_some((LIVE_LINE - 1, false)))
            else {
                break;
            };
            let mut line: Vec<u8> = self.pending.drain(..=end).collect();
            line.pop();
            if progress {
                // Carriage returns redraw one progress row (curl, compilers, etc.). Keep
                // only the newest update and show it at a human-readable rate.
                self.latest_progress = Some(line);
                self.skip_lf = true;
                self.flush_progress(false).await;
            } else {
                self.flush_progress(true).await;
                self.emit(&line).await;
            }
            if self.shown > LIVE_LIMIT {
                self.pending.clear();
                self.latest_progress = None;
                let note = "[live output limit reached; the result keeps its own capture]";
                self.progress.line(note.into(), self.stderr).await;
                return;
            }
        }
    }

    async fn emit(&mut self, line: &[u8]) {
        if line.is_empty() {
            return;
        }
        self.shown += line.len();
        let (text, _) = crate::tools::text_encoding::decode_output_line(line);
        let text = text.trim_end_matches(['\n', '\r']);
        self.progress.line(text.to_owned(), self.stderr).await;
    }

    async fn flush_progress(&mut self, force: bool) {
        if force
            || self
                .last_progress
                .is_none_or(|last| last.elapsed() >= LIVE_PROGRESS_INTERVAL)
        {
            if let Some(line) = self.latest_progress.take() {
                self.emit(&line).await;
                self.last_progress = Some(Instant::now());
            }
        }
    }

    async fn finish(&mut self) {
        self.flush_progress(true).await;
        if !self.pending.is_empty() && self.shown <= LIVE_LIMIT {
            let line = std::mem::take(&mut self.pending);
            self.emit(&line).await;
        }
    }
}

/// Keeps the first 40% and the last 60% of a stream within `limit` bytes. Build and test
/// output puts the summary and the failure at the end, so the tail gets the larger share.
#[derive(Clone)]
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

/// Full output held in memory before it goes to a file.
const TAP_MEMORY: usize = 256 * 1024;
/// Combined output kept for `shell_task output` and completion notices.
const TAP_TAIL: usize = 16 * 1024;
/// Largest output file of one background command; past it the file stops growing
/// (Claude Code caps its saved outputs at 64 MiB as well). Smaller in unit tests.
pub const TAP_FILE_LIMIT: u64 = if cfg!(test) {
    1024 * 1024
} else {
    64 * 1024 * 1024
};

enum Sink {
    /// Not on disk yet: the bytes so far, and where they go once they must.
    Memory(Vec<u8>, std::path::PathBuf),
    /// Bytes written so far, and whether the limit cut the file.
    File(std::fs::File, std::path::PathBuf, u64, bool),
    /// The file could not be written: why.
    Failed(String),
}

impl Sink {
    /// Appends `bytes` up to [`TAP_FILE_LIMIT`]; at the limit, a closing note.
    fn write(&mut self, bytes: &[u8]) {
        use std::io::Write;
        let Self::File(file, path, written, capped) = self else {
            return;
        };
        if *capped {
            return;
        }
        let room = TAP_FILE_LIMIT.saturating_sub(*written);
        let take = (bytes.len() as u64).min(room) as usize;
        let mut result = file.write_all(&bytes[..take]);
        *written += take as u64;
        if take < bytes.len() {
            *capped = true;
            result = result.and_then(|()| {
                file.write_all(
                    format!(
                        "\n[cove: output past {} MiB is not written here; shell_task output shows the latest]\n",
                        TAP_FILE_LIMIT / (1024 * 1024)
                    )
                    .as_bytes(),
                )
            });
        }
        if let Err(err) = result {
            *self = Self::Failed(format!("{}: {err}", crate::paths::display(path)));
        }
    }
}

/// A copy of one run's output, for a run that may continue in the background: the
/// head and tail of each stream as a result keeps them, the latest combined output,
/// and all of it (up to [`TAP_FILE_LIMIT`]) in a file once the run is moved to the
/// background (or too large to keep in memory).
pub struct Tap {
    streams: std::sync::Mutex<(HeadTail, HeadTail)>,
    tail: std::sync::Mutex<VecDeque<u8>>,
    sink: std::sync::Mutex<Sink>,
    bytes: std::sync::atomic::AtomicU64,
    muted: std::sync::atomic::AtomicBool,
    /// How long the run had shown no sign of work at the last sample.
    quiet: std::sync::Mutex<Option<Duration>>,
    /// The command's own process, once started.
    pid: std::sync::atomic::AtomicU32,
}

impl Tap {
    /// `log` is where the full output goes once it is written to disk.
    pub fn new(log: std::path::PathBuf) -> Self {
        Self {
            streams: std::sync::Mutex::new((
                HeadTail::new(STDOUT_LIMIT),
                HeadTail::new(STDERR_LIMIT),
            )),
            tail: std::sync::Mutex::new(VecDeque::new()),
            sink: std::sync::Mutex::new(Sink::Memory(Vec::new(), log)),
            bytes: std::sync::atomic::AtomicU64::new(0),
            muted: std::sync::atomic::AtomicBool::new(false),
            quiet: std::sync::Mutex::new(None),
            pid: std::sync::atomic::AtomicU32::new(0),
        }
    }

    /// The command's own process id, once it started.
    pub fn pid(&self) -> Option<u32> {
        match self.pid.load(std::sync::atomic::Ordering::Relaxed) {
            0 => None,
            pid => Some(pid),
        }
    }

    fn push(&self, stdout: bool, bytes: &[u8]) {
        self.bytes
            .fetch_add(bytes.len() as u64, std::sync::atomic::Ordering::Relaxed);
        if let Ok(mut streams) = self.streams.lock() {
            if stdout {
                streams.0.push(bytes);
            } else {
                streams.1.push(bytes);
            }
        }
        if let Ok(mut tail) = self.tail.lock() {
            tail.extend(bytes);
            let over = tail.len().saturating_sub(TAP_TAIL);
            tail.drain(..over);
        }
        if let Ok(mut sink) = self.sink.lock() {
            let spill = match &mut *sink {
                Sink::Memory(kept, _) => {
                    kept.extend_from_slice(bytes);
                    kept.len() > TAP_MEMORY
                }
                other => {
                    other.write(bytes);
                    false
                }
            };
            if spill {
                Self::spill(&mut sink);
            }
        }
    }

    /// Opens the file without replacing one: an earlier cove process of the same
    /// session may have left a log of that name.
    fn create(path: &std::path::Path) -> std::io::Result<(std::fs::File, std::path::PathBuf)> {
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent)?;
        }
        let open = |path: &std::path::Path| {
            std::fs::OpenOptions::new()
                .write(true)
                .create_new(true)
                .open(path)
        };
        match open(path) {
            Ok(file) => Ok((file, path.to_path_buf())),
            Err(err) if err.kind() == std::io::ErrorKind::AlreadyExists => {
                let stem = path
                    .file_stem()
                    .map(|stem| stem.to_string_lossy().into_owned())
                    .unwrap_or_default();
                let other = path.with_file_name(format!("{stem}-{}.log", std::process::id()));
                open(&other).map(|file| (file, other))
            }
            Err(err) => Err(err),
        }
    }

    fn spill(sink: &mut Sink) {
        let Sink::Memory(kept, path) = sink else {
            return;
        };
        let kept = std::mem::take(kept);
        *sink = match Self::create(path) {
            Ok((file, path)) => {
                let mut file = Sink::File(file, path, 0, false);
                file.write(&kept);
                file
            }
            Err(err) => Sink::Failed(format!("{}: {err}", crate::paths::display(path))),
        };
    }

    /// Writes the output to its file from now on: the path, or why it could not.
    pub fn persist(&self) -> std::result::Result<std::path::PathBuf, String> {
        let Ok(mut sink) = self.sink.lock() else {
            return Err("output lock poisoned".into());
        };
        Self::spill(&mut sink);
        drop(sink);
        self.file()
    }

    /// The output file, or why there is none.
    pub fn file(&self) -> std::result::Result<std::path::PathBuf, String> {
        match &*self
            .sink
            .lock()
            .map_err(|_| "output lock poisoned".to_owned())?
        {
            Sink::File(_, path, ..) => Ok(path.clone()),
            Sink::Failed(why) => Err(why.clone()),
            Sink::Memory(..) => Err("not written".into()),
        }
    }

    /// Whether the output file stopped at [`TAP_FILE_LIMIT`].
    pub fn capped(&self) -> bool {
        self.sink
            .lock()
            .is_ok_and(|sink| matches!(&*sink, Sink::File(.., true)))
    }

    /// Stops forwarding output lines to the live display: the tool call that showed
    /// them is over.
    pub fn mute(&self) {
        self.muted.store(true, std::sync::atomic::Ordering::Relaxed);
    }

    fn muted(&self) -> bool {
        self.muted.load(std::sync::atomic::Ordering::Relaxed)
    }

    /// Bytes of output so far.
    pub fn bytes(&self) -> u64 {
        self.bytes.load(std::sync::atomic::Ordering::Relaxed)
    }

    /// How long the run had shown no sign of work at the last sample (watched runs).
    pub fn quiet(&self) -> Option<Duration> {
        self.quiet.lock().ok().and_then(|quiet| *quiet)
    }

    fn set_quiet(&self, quiet: Option<Duration>) {
        if let Ok(mut slot) = self.quiet.lock() {
            *slot = quiet;
        }
    }

    /// Stdout and stderr so far, cut as a finished run's are, and whether either was cut.
    pub fn snapshot(&self) -> (String, String, bool) {
        let Ok(streams) = self.streams.lock() else {
            return (String::new(), String::new(), false);
        };
        let (out, out_cut) = streams.0.clone().finish();
        let (err, err_cut) = streams.1.clone().finish();
        (
            crate::tools::text_encoding::decode_output(&out).0,
            crate::tools::text_encoding::decode_output(&err).0,
            out_cut || err_cut,
        )
    }

    /// The last `limit` bytes of stdout and stderr together, starting on a line.
    pub fn tail(&self, limit: usize) -> String {
        let mut bytes: Vec<u8> = match self.tail.lock() {
            Ok(tail) => tail.iter().copied().collect(),
            Err(_) => return String::new(),
        };
        // The kept window may begin inside a line (or a character): start after it.
        if self.bytes() > bytes.len() as u64
            && let Some(start) = bytes.iter().position(|byte| *byte == b'\n')
        {
            bytes.drain(..=start);
        }
        let (text, _) = crate::tools::text_encoding::decode_output(&bytes);
        let cut = tail_bytes(&text, limit);
        if cut.len() < text.len() {
            cut.find('\n')
                .map_or(cut, |start| &cut[start + 1..])
                .to_owned()
        } else {
            cut.to_owned()
        }
    }
}

/// Watches a stream's lines for a lock-wait message; reports the first match only.
struct LockWatch {
    patterns: &'static [&'static str],
    found: tokio::sync::mpsc::UnboundedSender<String>,
    line: Vec<u8>,
}

impl LockWatch {
    /// Feeds raw bytes; returns false once a match was reported (stop watching).
    fn feed(&mut self, bytes: &[u8]) -> bool {
        for &byte in bytes {
            if byte != b'\n' && byte != b'\r' {
                if self.line.len() < LIVE_LINE {
                    self.line.push(byte);
                }
                continue;
            }
            let (text, _) = crate::tools::text_encoding::decode_output_line(&self.line);
            let lower = text.to_lowercase();
            if self.patterns.iter().any(|pattern| lower.contains(pattern)) {
                let _ = self.found.send(text.trim().to_owned());
                return false;
            }
            self.line.clear();
        }
        true
    }
}

async fn capture(
    pipe: impl AsyncRead + Unpin,
    live: Option<Live>,
    limit: usize,
) -> std::io::Result<(Vec<u8>, bool)> {
    capture_watched(pipe, live, limit, None, None, None).await
}

/// `capture` that also reports the first line saying the command waits on a lock.
async fn capture_watched(
    mut pipe: impl AsyncRead + Unpin,
    mut live: Option<Live>,
    limit: usize,
    mut lock: Option<LockWatch>,
    seen: Option<Arc<std::sync::atomic::AtomicU64>>,
    tap: Option<(Arc<Tap>, bool)>,
) -> std::io::Result<(Vec<u8>, bool)> {
    let mut kept = HeadTail::new(limit);
    let mut buffer = [0_u8; 4096];
    let mut previous_cr = false;
    loop {
        let count = pipe.read(&mut buffer).await?;
        if count == 0 {
            break;
        }
        if let Some(seen) = &seen {
            seen.fetch_add(count as u64, std::sync::atomic::Ordering::Relaxed);
        }
        if lock
            .as_mut()
            .is_some_and(|watch| !watch.feed(&buffer[..count]))
        {
            lock = None;
        }
        if let Some(live) = &mut live
            && !tap.as_ref().is_some_and(|(tap, _)| tap.muted())
        {
            live.feed(&buffer[..count]).await;
        }
        // Normalize terminal redraws and CRLF before retaining tool output. This keeps
        // control bytes out of model context and lets truncation preserve useful lines.
        let mut normalized = 0;
        for index in 0..count {
            let byte = buffer[index];
            if previous_cr {
                previous_cr = false;
                if byte == b'\n' {
                    continue;
                }
            }
            if byte == b'\r' {
                buffer[normalized] = b'\n';
                normalized += 1;
                previous_cr = true;
            } else {
                buffer[normalized] = byte;
                normalized += 1;
            }
        }
        kept.push(&buffer[..normalized]);
        if let Some((tap, stdout)) = &tap {
            tap.push(*stdout, &buffer[..normalized]);
        }
    }
    if let Some(live) = &mut live
        && !tap.as_ref().is_some_and(|(tap, _)| tap.muted())
    {
        live.finish().await;
    }
    Ok(kept.finish())
}

/// The last `limit` bytes of `text`, starting on a character boundary (output tails for
/// reports and evidence).
pub fn tail_bytes(text: &str, limit: usize) -> &str {
    if text.len() <= limit {
        return text;
    }
    crate::text::tail(text, limit)
}

/// Runs `script` in the legacy shell (PowerShell on Windows, sh elsewhere).
pub async fn shell(
    cwd: &Path,
    script: &str,
    timeout: impl Into<Limit>,
    cancel: CancellationToken,
) -> Result<CommandOutput> {
    shell_configured(cwd, script, timeout, cancel, None).await
}

/// Runs `script` in the shell it is routed to (as permission checks judge it),
/// forwarding output lines to `progress` while it runs.
pub async fn shell_live(
    cwd: &Path,
    script: &str,
    timeout: impl Into<Limit>,
    cancel: CancellationToken,
    progress: Option<Arc<Progress>>,
) -> Result<CommandOutput> {
    run(
        Shell::route(script),
        cwd,
        script,
        timeout.into(),
        cancel,
        None,
        progress,
        &[],
        None,
    )
    .await
}

/// `shell_live` with extra environment variables for the command (the project review
/// command's `COVE_REVIEW_*`), and no live output.
pub async fn shell_with_env(
    cwd: &Path,
    script: &str,
    timeout: impl Into<Limit>,
    cancel: CancellationToken,
    env: &[(&str, String)],
) -> Result<CommandOutput> {
    run(
        Shell::route(script),
        cwd,
        script,
        timeout.into(),
        cancel,
        None,
        None,
        env,
        None,
    )
    .await
}

pub async fn shell_configured(
    cwd: &Path,
    script: &str,
    timeout: impl Into<Limit>,
    cancel: CancellationToken,
    input: Option<&str>,
) -> Result<CommandOutput> {
    run(
        &Shell::legacy(),
        cwd,
        script,
        timeout.into(),
        cancel,
        input,
        None,
        &[],
        None,
    )
    .await
}

/// Runs `script` in `shell`, streaming output lines to `progress`.
pub async fn run_shell(
    shell: &Shell,
    cwd: &Path,
    script: &str,
    timeout: impl Into<Limit>,
    cancel: CancellationToken,
    progress: Option<Arc<Progress>>,
) -> Result<CommandOutput> {
    run(
        shell,
        cwd,
        script,
        timeout.into(),
        cancel,
        None,
        progress,
        &[],
        None,
    )
    .await
}

/// `run_shell` with owned arguments, so the run can be spawned and outlive the call
/// that started it, copying its output to `tap` (background shell commands).
pub async fn run_tapped(
    shell: Shell,
    cwd: std::path::PathBuf,
    script: String,
    limit: Limit,
    cancel: CancellationToken,
    progress: Option<Arc<Progress>>,
    tap: Arc<Tap>,
) -> Result<CommandOutput> {
    run(
        &shell,
        &cwd,
        &script,
        limit,
        cancel,
        None,
        progress,
        &[],
        Some(tap),
    )
    .await
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
        ShellKind::Cmd => {
            // `/c` runs one line: join the lines as `&`-separated commands.
            let line = script
                .lines()
                .map(str::trim)
                .filter(|line| !line.is_empty())
                .collect::<Vec<_>>()
                .join(" & ");
            #[cfg(windows)]
            {
                // cmd does not follow the usual argv quoting; `/s` strips exactly the outer quotes.
                command.raw_arg(format!("/d /s /c \"{line}\""));
            }
            #[cfg(unix)]
            command.args(["/d", "/s", "/c", &line]);
            // Otherwise cmd runs `git.bat` from the workspace before the real git.
            command.env("NoDefaultCurrentDirectoryInExePath", "1");
            command.stdin(Stdio::null());
            (command, None)
        }
    }
}

#[allow(clippy::too_many_arguments)]
async fn run(
    shell: &Shell,
    cwd: &Path,
    script: &str,
    limit: Limit,
    cancel: CancellationToken,
    input: Option<&str>,
    progress: Option<Arc<Progress>>,
    env: &[(&str, String)],
    tap: Option<Arc<Tap>>,
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
        // Hooks get an allowlisted environment above; everything else the model runs
        // inherits cove's, minus the API key cove itself uses.
        for key in secrets::protected() {
            command.env_remove(key);
        }
        inherited_git_config_count()
    };
    command.envs(noninteractive_env(inherited));
    command.envs(env.iter().map(|(key, value)| (*key, value.as_str())));
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
            skip_lf: false,
            latest_progress: None,
            last_progress: None,
            shown: 0,
        })
    };
    let (out_live, err_live) = (live(false), live(true));
    let (found, mut lock) = tokio::sync::mpsc::unbounded_channel::<String>();
    let watch = || {
        LOCK_HANDLER.get().map(|handler| LockWatch {
            patterns: handler.patterns(),
            found: found.clone(),
            line: Vec::new(),
        })
    };
    let (out_watch, err_watch) = (watch(), watch());
    drop(found);
    let seen = Arc::new(std::sync::atomic::AtomicU64::new(0));
    let (out_seen, err_seen) = (seen.clone(), seen.clone());
    let (out_tap, err_tap) = (
        tap.clone().map(|tap| (tap, true)),
        tap.clone().map(|tap| (tap, false)),
    );
    readers.spawn(async move {
        (
            true,
            capture_watched(
                stdout,
                out_live,
                STDOUT_LIMIT,
                out_watch,
                Some(out_seen),
                out_tap,
            )
            .await,
        )
    });
    readers.spawn(async move {
        (
            false,
            capture_watched(
                stderr,
                err_live,
                STDERR_LIMIT,
                err_watch,
                Some(err_seen),
                err_tap,
            )
            .await,
        )
    });
    let mut cancelled = false;
    let mut timed_out = false;
    let started = Instant::now();
    // `Duration::MAX`: no total limit (a server started in the background).
    let deadline = tokio::time::Instant::now().checked_add(limit.total);
    // Idle watching needs samples of the tree; where there are none only `total` applies.
    let mut idle = limit.idle.map(|idle| Watch::new(idle, started));
    let mut stall: Option<Watch> = None;
    let mut sampling = tokio::time::interval(SAMPLE_EVERY);
    sampling.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    let child_pid = child.id();
    if let (Some(tap), Some(pid)) = (&tap, child_pid) {
        tap.pid.store(pid, std::sync::atomic::Ordering::Relaxed);
    }
    // Ends the lock question once the command is over, answered or not.
    let ask = cancel.child_token();
    let mut asked = false;
    let status = loop {
        tokio::select! {
            biased;
            _ = cancel.cancelled() => { cancelled = true; break None },
            _ = async {
                match deadline {
                    Some(deadline) => tokio::time::sleep_until(deadline).await,
                    None => std::future::pending().await,
                }
            } => { timed_out = true; break None },
            status = child.wait() => break Some(status?),
            _ = sampling.tick(), if idle.is_some() || limit.status.is_some() => {
                let now = Instant::now();
                let mut quiet = None;
                if let Some(watch) = &mut idle {
                    match tree.sample() {
                        Some(sample) => {
                            let bytes = seen.load(std::sync::atomic::Ordering::Relaxed);
                            if watch.observe(now, bytes, sample) {
                                stall = idle.take();
                                break None;
                            }
                            quiet = Some(watch.quiet_for(now));
                        }
                        None => idle = None,
                    }
                }
                if let Some(tap) = &tap {
                    tap.set_quiet(quiet);
                }
                if let Some((progress, label)) = &limit.status {
                    let state = match quiet {
                        Some(quiet) if quiet >= SAMPLE_EVERY * 2 => {
                            format!("已 {} 无活动", clock(quiet))
                        }
                        Some(_) => "活跃".to_owned(),
                        None => String::new(),
                    };
                    let elapsed = clock(now.duration_since(started));
                    let text = if state.is_empty() {
                        format!("{label} · {elapsed}")
                    } else {
                        format!("{label} · {elapsed} · {state}")
                    };
                    progress.activity(text).await;
                }
            }
            Some(line) = lock.recv(), if !asked => {
                asked = true;
                // Waiting for another process's lock is neither work nor a hang: only
                // the total limit applies from here.
                idle = None;
                if let Some(tap) = &tap {
                    tap.set_quiet(None);
                }
                if let (Some(handler), Some(pid)) = (LOCK_HANDLER.get(), child_pid) {
                    tokio::spawn(handler.waiting(pid, line, ask.clone()));
                }
            }
        }
    };
    ask.cancel();
    // Named before the tree goes: the processes still running are the stuck ones.
    let stalled =
        stall.map(|watch| watch.reason(Instant::now(), started, script, &tree.processes()));
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
        stalled,
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
impl ProcessTree {
    /// CPU time and I/O of every process the job ever held (the job keeps the totals
    /// of exited ones), and the processes alive now.
    fn sample(&self) -> Option<TreeSample> {
        use windows_sys::Win32::System::JobObjects::{
            JOBOBJECT_BASIC_AND_IO_ACCOUNTING_INFORMATION,
            JobObjectBasicAndIoAccountingInformation, QueryInformationJobObject,
        };
        // The out-pointer refers to a zeroed local of the size passed.
        let info = unsafe {
            let mut info: JOBOBJECT_BASIC_AND_IO_ACCOUNTING_INFORMATION = std::mem::zeroed();
            let ok = QueryInformationJobObject(
                self.0,
                JobObjectBasicAndIoAccountingInformation,
                (&mut info as *mut JOBOBJECT_BASIC_AND_IO_ACCOUNTING_INFORMATION).cast(),
                std::mem::size_of_val(&info) as u32,
                std::ptr::null_mut(),
            );
            (ok != 0).then_some(info)?
        };
        let ticks = (info.BasicInfo.TotalUserTime + info.BasicInfo.TotalKernelTime).max(0);
        Some(TreeSample {
            // 100-nanosecond units.
            cpu: Duration::from_nanos(ticks as u64 * 100),
            io: info.IoInfo.ReadOperationCount + info.IoInfo.WriteOperationCount,
            pids: self.pids(),
        })
    }

    fn pids(&self) -> Vec<u32> {
        use windows_sys::Win32::System::JobObjects::{
            JobObjectBasicProcessIdList, QueryInformationJobObject,
        };
        // `JOBOBJECT_BASIC_PROCESS_ID_LIST` with room for 1024 ids.
        #[repr(C)]
        struct List {
            assigned: u32,
            listed: u32,
            ids: [usize; 1024],
        }
        let mut list = Box::new(List {
            assigned: 0,
            listed: 0,
            ids: [0; 1024],
        });
        // The buffer is owned, initialized and as large as the length passed.
        let ok = unsafe {
            QueryInformationJobObject(
                self.0,
                JobObjectBasicProcessIdList,
                (&mut *list as *mut List).cast(),
                std::mem::size_of::<List>() as u32,
                std::ptr::null_mut(),
            )
        };
        if ok == 0 {
            return Vec::new();
        }
        let count = (list.listed as usize).min(list.ids.len());
        let mut pids: Vec<u32> = list.ids[..count].iter().map(|id| *id as u32).collect();
        pids.sort_unstable();
        pids
    }

    /// Name and CPU time of each process alive now, busiest first.
    fn processes(&self) -> Vec<(u32, String, Duration)> {
        use windows_sys::Win32::{
            Foundation::{CloseHandle, FILETIME},
            System::Threading::{
                GetProcessTimes, OpenProcess, PROCESS_NAME_WIN32,
                PROCESS_QUERY_LIMITED_INFORMATION, QueryFullProcessImageNameW,
            },
        };
        let mut found = Vec::new();
        for pid in self.pids() {
            // The handle is checked, used for queries into local buffers, then closed.
            unsafe {
                let process = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, 0, pid);
                if process.is_null() {
                    continue;
                }
                let mut path = [0_u16; 1024];
                let mut length = path.len() as u32;
                let name = if QueryFullProcessImageNameW(
                    process,
                    PROCESS_NAME_WIN32,
                    path.as_mut_ptr(),
                    &mut length,
                ) != 0
                {
                    let full = String::from_utf16_lossy(&path[..length as usize]);
                    full.rsplit(['\\', '/']).next().unwrap_or(&full).to_owned()
                } else {
                    format!("pid {pid}")
                };
                let zero = FILETIME {
                    dwLowDateTime: 0,
                    dwHighDateTime: 0,
                };
                let (mut created, mut exited, mut kernel, mut user) = (zero, zero, zero, zero);
                let ticks = |time: FILETIME| {
                    (u64::from(time.dwHighDateTime) << 32) | u64::from(time.dwLowDateTime)
                };
                let cpu =
                    if GetProcessTimes(process, &mut created, &mut exited, &mut kernel, &mut user)
                        != 0
                    {
                        Duration::from_nanos((ticks(kernel) + ticks(user)) * 100)
                    } else {
                        Duration::ZERO
                    };
                CloseHandle(process);
                found.push((pid, name, cpu));
            }
        }
        found.sort_by_key(|process| std::cmp::Reverse(process.2));
        found
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

/// The command's process group read from `/proc`: (pid, name, CPU time, I/O operations).
#[cfg(target_os = "linux")]
fn group_processes(group: i32) -> Option<Vec<(u32, String, Duration, u64)>> {
    // Clock ticks per second of `utime`/`stime`; a plain query.
    let hertz = match unsafe { libc::sysconf(libc::_SC_CLK_TCK) } {
        hertz if hertz > 0 => hertz as u64,
        _ => 100,
    };
    let mut found = Vec::new();
    for entry in std::fs::read_dir("/proc").ok()?.flatten() {
        let Some(pid) = entry
            .file_name()
            .to_str()
            .and_then(|name| name.parse::<u32>().ok())
        else {
            continue;
        };
        let Ok(stat) = std::fs::read_to_string(format!("/proc/{pid}/stat")) else {
            continue;
        };
        // `pid (name) state ppid pgrp …`: the name may hold spaces and parentheses.
        let (Some(open), Some(close)) = (stat.find('('), stat.rfind(')')) else {
            continue;
        };
        let fields: Vec<&str> = stat[close + 1..].split_whitespace().collect();
        if fields.get(2).and_then(|pgrp| pgrp.parse::<i32>().ok()) != Some(group) {
            continue;
        }
        let ticks = |at: usize| {
            fields
                .get(at)
                .and_then(|v| v.parse::<u64>().ok())
                .unwrap_or(0)
        };
        let cpu = Duration::from_millis((ticks(11) + ticks(12)) * 1000 / hertz);
        let io = std::fs::read_to_string(format!("/proc/{pid}/io"))
            .map(|text| {
                text.lines()
                    .filter_map(|line| {
                        let (key, value) = line.split_once(':')?;
                        matches!(key, "syscr" | "syscw")
                            .then(|| value.trim().parse::<u64>().ok())?
                    })
                    .sum()
            })
            .unwrap_or(0);
        found.push((pid, stat[open + 1..close].to_owned(), cpu, io));
    }
    Some(found)
}

#[cfg(unix)]
impl ProcessTree {
    #[cfg(target_os = "linux")]
    fn sample(&self) -> Option<TreeSample> {
        let processes = group_processes(self.0)?;
        let mut pids: Vec<u32> = processes.iter().map(|(pid, ..)| *pid).collect();
        pids.sort_unstable();
        Some(TreeSample {
            cpu: processes.iter().map(|(_, _, cpu, _)| *cpu).sum(),
            io: processes.iter().map(|(.., io)| *io).sum(),
            pids,
        })
    }

    #[cfg(target_os = "linux")]
    fn processes(&self) -> Vec<(u32, String, Duration)> {
        let mut found: Vec<_> = group_processes(self.0)
            .unwrap_or_default()
            .into_iter()
            .map(|(pid, name, cpu, _)| (pid, name, cpu))
            .collect();
        found.sort_by_key(|process| std::cmp::Reverse(process.2));
        found
    }

    /// No `/proc`: idle watching is off and only the total limit applies.
    #[cfg(not(target_os = "linux"))]
    fn sample(&self) -> Option<TreeSample> {
        None
    }

    #[cfg(not(target_os = "linux"))]
    fn processes(&self) -> Vec<(u32, String, Duration)> {
        Vec::new()
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
    fn byte_tails_start_on_a_character() {
        assert_eq!(tail_bytes("ab界cd", 4), "cd");
        assert_eq!(tail_bytes("ab界cd", 5), "界cd");
        assert_eq!(tail_bytes("abc", 10), "abc");
    }

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

    #[tokio::test]
    async fn a_lock_wait_line_is_reported_even_when_split_across_reads() {
        let (mut writer, reader) = tokio::io::duplex(64);
        let (found, mut lock) = tokio::sync::mpsc::unbounded_channel();
        let watch = LockWatch {
            patterns: &["blocking waiting for file lock"],
            found,
            line: Vec::new(),
        };
        let capture = tokio::spawn(capture_watched(reader, None, 1024, Some(watch), None, None));
        use tokio::io::AsyncWriteExt;
        writer
            .write_all(b"   Compiling a\n   Blocking waiting fo")
            .await
            .unwrap();
        writer.flush().await.unwrap();
        tokio::task::yield_now().await;
        writer
            .write_all(b"r file lock on build directory\n")
            .await
            .unwrap();
        drop(writer);
        let line = tokio::time::timeout(Duration::from_secs(5), lock.recv())
            .await
            .expect("lock wait not detected");
        assert_eq!(
            line.as_deref(),
            Some("Blocking waiting for file lock on build directory")
        );
        capture.await.unwrap().unwrap();
    }

    #[test]
    fn a_tap_never_replaces_an_earlier_log_and_stops_at_its_limit() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("1.log");
        std::fs::write(&path, "earlier process").unwrap();
        let tap = Tap::new(path.clone());
        tap.push(true, b"start\n");
        let file = tap.persist().unwrap();
        assert_ne!(file, path);
        assert_eq!(std::fs::read_to_string(&path).unwrap(), "earlier process");
        let chunk = vec![b'x'; 64 * 1024];
        for _ in 0..24 {
            tap.push(true, &chunk);
        }
        assert!(tap.capped());
        let written = std::fs::read_to_string(&file).unwrap();
        assert!(
            written.len() as u64 <= TAP_FILE_LIMIT + 200,
            "{}",
            written.len()
        );
        assert!(written.starts_with("start\n"));
        assert!(
            written
                .trim_end()
                .ends_with("shell_task output shows the latest]")
        );
        // The latest output is still kept in memory.
        assert!(tap.tail(100).ends_with('x'));
    }

    #[cfg(any(windows, target_os = "linux"))]
    struct QuietLocks;

    #[cfg(any(windows, target_os = "linux"))]
    impl LockWait for QuietLocks {
        fn patterns(&self) -> &'static [&'static str] {
            &["blocking waiting for file lock"]
        }

        fn waiting(&self, _: u32, _: String, _: CancellationToken) -> BoxFuture<'static, ()> {
            Box::pin(async {})
        }
    }

    #[cfg(any(windows, target_os = "linux"))]
    #[tokio::test]
    async fn waiting_for_a_lock_is_not_taken_for_a_hang() {
        set_lock_wait(Arc::new(QuietLocks));
        let output = watched(
            "Write-Output 'Blocking waiting for file lock on build directory'; Start-Sleep -Seconds 4; Write-Output got",
            "echo 'Blocking waiting for file lock on build directory'; sleep 4; echo got",
            Duration::from_secs(1),
        )
        .await;
        assert!(output.stalled.is_none(), "{:?}", output.stalled);
        assert!(output.stdout.contains("got"), "{}", output.stdout);
    }

    #[test]
    fn idleness_counts_from_the_last_sign_of_work() {
        let start = Instant::now();
        let at = |seconds: u64| start + Duration::from_secs(seconds);
        let tree = |cpu_ms: u64, io: u64, pids: &[u32]| TreeSample {
            cpu: Duration::from_millis(cpu_ms),
            io,
            pids: pids.to_vec(),
        };
        let mut watch = Watch::new(Duration::from_secs(10), start);
        assert!(!watch.observe(at(0), 0, tree(100, 5, &[1])));
        // Each kind of work resets the clock: output, CPU, I/O, a process starting.
        assert!(!watch.observe(at(8), 10, tree(100, 5, &[1])));
        assert!(!watch.observe(at(16), 10, tree(400, 5, &[1])));
        assert!(!watch.observe(at(24), 10, tree(400, 9, &[1])));
        assert!(!watch.observe(at(32), 10, tree(400, 9, &[1, 2])));
        // Scheduling noise of a waiting process is no work.
        assert!(!watch.observe(at(38), 10, tree(420, 9, &[1, 2])));
        assert!(watch.observe(at(42), 10, tree(430, 9, &[1, 2])));
        let reason = watch.reason(
            at(42),
            start,
            "dotnet test",
            &[(2, "testhost.exe".into(), Duration::from_secs(41))],
        );
        assert!(
            reason.contains("已运行 42s，最近 10s 没有任何活动"),
            "{reason}"
        );
        assert!(reason.contains("0.03s"), "{reason}");
        assert!(reason.contains("最后一次输出在 34s 前"), "{reason}");
        assert!(
            reason.contains("testhost.exe（pid 2，CPU 共 41.0s）"),
            "{reason}"
        );
        assert!(reason.contains("--blame-hang-timeout"), "{reason}");
        assert_eq!(clock(Duration::from_secs(372)), "6m12s");
        assert_eq!(clock(Duration::from_secs(3720)), "1h02m");
    }

    /// Runs `windows` (PowerShell) or `unix` (sh) with an idle limit of `idle`.
    #[cfg(any(windows, target_os = "linux"))]
    async fn watched(windows: &str, unix: &str, idle: Duration) -> CommandOutput {
        let dir = tempfile::tempdir().unwrap();
        let script = if cfg!(windows) { windows } else { unix };
        shell(
            dir.path(),
            script,
            Limit::watched(Duration::from_secs(60), idle),
            CancellationToken::new(),
        )
        .await
        .unwrap()
    }

    #[cfg(any(windows, target_os = "linux"))]
    #[tokio::test]
    async fn a_command_that_does_nothing_is_stopped_with_the_reason() {
        let started = Instant::now();
        let output = watched(
            "Start-Sleep -Seconds 30",
            "sleep 30",
            Duration::from_secs(2),
        )
        .await;
        let reason = output.stalled.expect("stalled");
        assert!(started.elapsed() < Duration::from_secs(20), "{reason}");
        assert!(!output.timed_out && output.code.is_none(), "{reason}");
        assert!(
            reason.contains("没有任何活动") && reason.contains("仍在运行："),
            "{reason}"
        );
        let name = if cfg!(windows) { "powershell" } else { "sleep" };
        assert!(reason.to_lowercase().contains(name), "{reason}");
    }

    #[cfg(any(windows, target_os = "linux"))]
    #[tokio::test]
    async fn quiet_work_and_steady_output_are_not_stopped() {
        // Four seconds of CPU without a byte of output, against an idle limit of one.
        let busy = watched(
            "$end = (Get-Date).AddSeconds(4); while ((Get-Date) -lt $end) {}; 'done'",
            "end=$(( $(date +%s) + 4 )); while [ \"$(date +%s)\" -lt \"$end\" ]; do :; done; echo done",
            Duration::from_secs(1),
        )
        .await;
        assert!(busy.stalled.is_none(), "{:?}", busy.stalled);
        assert_eq!(busy.code, Some(0), "{}", busy.stderr);
        assert!(busy.stdout.contains("done"));
        // A line every half second for four seconds, idle otherwise.
        let chatty = watched(
            "1..8 | ForEach-Object { Write-Output $_; Start-Sleep -Milliseconds 500 }",
            "for i in 1 2 3 4 5 6 7 8; do echo $i; sleep 0.5; done",
            Duration::from_millis(1500),
        )
        .await;
        assert!(chatty.stalled.is_none(), "{:?}", chatty.stalled);
        assert_eq!(chatty.code, Some(0), "{}", chatty.stderr);
    }

    /// Runs `COVE_WATCH_COMMAND` in `COVE_WATCH_DIR` (default: a temporary directory) the
    /// way checks run, with the default limits, and prints how it ended. For trying the
    /// idle watching on a real test suite:
    /// `COVE_WATCH_COMMAND="dotnet test" cargo test --release --lib watch_command -- --ignored --nocapture`
    #[tokio::test]
    #[ignore]
    async fn watch_command() {
        let command = std::env::var("COVE_WATCH_COMMAND").expect("COVE_WATCH_COMMAND");
        let temporary = tempfile::tempdir().unwrap();
        let dir = std::env::var("COVE_WATCH_DIR")
            .map(std::path::PathBuf::from)
            .unwrap_or_else(|_| temporary.path().to_path_buf());
        let started = Instant::now();
        let output = shell_live(
            &dir,
            &command,
            Limit::watched(WATCHED_TOTAL, IDLE_LIMIT),
            CancellationToken::new(),
            None,
        )
        .await
        .unwrap();
        println!("elapsed:   {}", clock(started.elapsed()));
        println!("exit code: {:?}", output.code);
        println!("timed out: {}", output.timed_out);
        println!("stalled:   {}", output.stalled.as_deref().unwrap_or("no"));
        println!("stdout tail:\n{}", tail_bytes(output.stdout.trim(), 600));
    }

    #[cfg(windows)]
    #[tokio::test]
    async fn cmd_runs_quoted_and_multi_line_scripts() {
        let shell = Shell::available(ShellKind::Cmd).expect("cmd.exe");
        let dir = tempfile::tempdir().unwrap();
        let output = run_shell(
            shell,
            dir.path(),
            "echo \"a & b\"\necho %OS%",
            Duration::from_secs(20),
            CancellationToken::new(),
            None,
        )
        .await
        .unwrap();
        assert_eq!(output.code, Some(0), "{}", output.stderr);
        assert!(output.stdout.contains("\"a & b\""), "{}", output.stdout);
        assert!(output.stdout.contains("Windows_NT"), "{}", output.stdout);
    }
}
