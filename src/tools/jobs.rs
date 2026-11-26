//! Shell commands that run on after their tool call returned: moved to the background
//! at `timeout_seconds` (docs/optimizations/0050) or started there with
//! `run_in_background` (0051). Each `LocalTools` owns one registry, so the main session
//! and every sub-agent have their own; dropping it ends every command it still holds.

use std::{
    path::PathBuf,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use tokio::sync::watch;
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Progress, Result},
    process::{self, CommandOutput, Limit, Shell, Tap},
    session::workspace::Workspace,
    tools::ToolOutput,
};

/// How long a `run_in_background` start waits for the first output.
pub const DETACHED_WAIT: Duration = Duration::from_secs(5);
/// After the first output of a background start, a little more of it.
const FIRST_OUTPUT_GRACE: Duration = Duration::from_millis(500);
/// How long a stop waits for the process tree to go.
const STOP_WAIT: Duration = Duration::from_secs(10);
/// Output tail in a completion notice and in `shell_task output`.
const NOTICE_TAIL: usize = 3 * 1024;
const OUTPUT_TAIL: usize = 4 * 1024;
/// Finished commands kept for `shell_task` after their result was delivered.
const KEEP_FINISHED: usize = 20;

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Kind {
    /// Still running at `timeout_seconds`: part of the round, waited for before it ends.
    Moved,
    /// Started with `run_in_background`: runs until stopped or the session ends.
    Detached,
}

/// How a foreground wait ended.
pub enum Foreground {
    Done(CommandOutput),
    /// Running on in the background.
    Moved,
}

struct Entry {
    id: u32,
    command: String,
    kind: Kind,
    /// The tool call returned while the command went on.
    background: bool,
    /// The tool call went away without claiming the result: drop it when it comes.
    orphan: bool,
    started: Instant,
    tap: Arc<Tap>,
    cancel: CancellationToken,
    task: Option<tokio::task::JoinHandle<()>>,
    result: Option<Result<CommandOutput>>,
    elapsed: Duration,
    stopped_by: Option<&'static str>,
    delivered: bool,
    /// Its end was reported to the turn hooks (verification evidence), which happens
    /// even when the model saw the result through `shell_task` first.
    observed: bool,
}

impl Entry {
    fn running(&self) -> bool {
        self.result.is_none()
    }

    fn label(&self) -> String {
        format!("#{} {}", self.id, crate::text::short(&self.command, 60))
    }

    /// The result for the model, and in a few words for the user.
    fn outcome(&self, total: Duration) -> (String, String) {
        match &self.result {
            None => ("still running".into(), "运行中".into()),
            Some(Err(Error::Cancelled)) => (
                self.stopped_by.unwrap_or("stopped").to_owned(),
                "已停止".into(),
            ),
            Some(Err(err)) => (format!("failed to run: {err}"), "没能运行".into()),
            Some(Ok(output)) => {
                if let Some(reason) = &output.stalled {
                    (
                        format!("stalled: stopped for lack of any activity. {reason}"),
                        "卡住，已中止".into(),
                    )
                } else if output.timed_out {
                    (
                        format!("stopped at the {}s total limit", total.as_secs()),
                        "到总时限，已中止".into(),
                    )
                } else {
                    match output.code {
                        Some(0) => ("exit code 0".into(), "退出码 0".into()),
                        Some(code) => (
                            format!("failed with exit code {code}"),
                            format!("退出码 {code}"),
                        ),
                        None => ("ended without an exit code".into(), "已结束".into()),
                    }
                }
            }
        }
    }

    fn state(&self, total: Duration) -> String {
        if self.running() {
            let activity = match self.tap.quiet() {
                Some(quiet) if quiet >= Duration::from_secs(10) => {
                    format!(", no activity for {}", process::clock(quiet))
                }
                Some(_) => ", active".into(),
                None => String::new(),
            };
            format!(
                "running for {}{activity}",
                process::clock(self.started.elapsed())
            )
        } else {
            format!(
                "{} after {}",
                self.outcome(total).0,
                process::clock(self.elapsed)
            )
        }
    }

    fn file(&self) -> String {
        match self.tap.file() {
            Ok(path) if self.tap.capped() => format!(
                "{} (cut at {} MiB)",
                crate::paths::display(&path),
                process::TAP_FILE_LIMIT / (1024 * 1024)
            ),
            Ok(path) => crate::paths::display(&path),
            Err(why) => format!("(none: {why})"),
        }
    }

    /// The end as a shell result reads, for the checks that read shell results
    /// (`safety::gates::Evidence`): same header, same failure rule.
    fn as_result(&self) -> ToolOutput {
        match &self.result {
            Some(Ok(output)) => ToolOutput {
                failed: output.code != Some(0) || output.timed_out || output.stalled.is_some(),
                content: format!(
                    "exit_code={:?} timed_out={} truncated={} encoding_warning={}",
                    output.code, output.timed_out, output.truncated, output.encoding_warning
                ),
                ..Default::default()
            },
            _ => ToolOutput {
                failed: true,
                content: "exit_code=None timed_out=false truncated=false encoding_warning=false"
                    .into(),
                ..Default::default()
            },
        }
    }
}

/// What ended since the last look: the notice for the model (when it has not seen the
/// results yet), a line per command for the user, and each command with its result for
/// the turn hooks.
#[derive(Default)]
pub struct Finished {
    pub notice: Option<String>,
    pub lines: Vec<String>,
    pub results: Vec<(String, ToolOutput)>,
}

/// Told how many background commands still run.
type CountCallback = Arc<dyn Fn(usize) + Send + Sync>;

struct Shared {
    entries: Mutex<Vec<Entry>>,
    /// Bumped whenever a command ends.
    generation: watch::Sender<u64>,
    count: Mutex<Option<CountCallback>>,
}

impl Shared {
    fn lock(&self) -> std::sync::MutexGuard<'_, Vec<Entry>> {
        self.entries
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// Records the end of command `id`; whether it had gone to the background.
    fn finish(&self, id: u32, result: Result<CommandOutput>) -> bool {
        let background = {
            let mut entries = self.lock();
            let Some(at) = entries.iter().position(|entry| entry.id == id) else {
                return false;
            };
            if entries[at].orphan {
                entries.remove(at);
                return false;
            }
            let entry = &mut entries[at];
            entry.elapsed = entry.started.elapsed();
            entry.result = Some(result);
            entry.task = None;
            if let Some(pid) = entry.tap.pid() {
                process::forget_background(pid);
            }
            entry.background
        };
        self.generation.send_modify(|generation| *generation += 1);
        self.report();
        background
    }

    /// Tells the display how many background commands still run.
    fn report(&self) {
        let running = self
            .lock()
            .iter()
            .filter(|entry| entry.background && entry.running())
            .count();
        let callback = self.count.lock().ok().and_then(|count| count.clone());
        if let Some(callback) = callback {
            callback(running);
        }
    }
}

/// Ends a foreground wait the tool call left without claiming the result.
struct Claim<'a> {
    shared: &'a Shared,
    id: u32,
    armed: bool,
}

impl Drop for Claim<'_> {
    fn drop(&mut self) {
        if !self.armed {
            return;
        }
        let mut entries = self.shared.lock();
        if let Some(at) = entries.iter().position(|entry| entry.id == self.id) {
            if entries[at].running() {
                entries[at].orphan = true;
                entries[at].cancel.cancel();
            } else {
                entries.remove(at);
            }
        }
    }
}

pub struct Jobs {
    shared: Arc<Shared>,
    /// Output files go to `<dir>/<scope>/<id>.log`.
    dir: PathBuf,
    scope: Mutex<String>,
    next: std::sync::atomic::AtomicU32,
    total: Duration,
    idle: Duration,
}

impl Jobs {
    /// `dir`: where output files go (`<data dir>/tool-outputs/shell`); without one, the
    /// system's temporary directory.
    pub fn new(dir: Option<PathBuf>) -> Self {
        let dir = dir.unwrap_or_else(|| std::env::temp_dir().join("cove-shell"));
        let scope = format!("agent-{}", &crate::session::new_id()[..12]);
        Self {
            shared: Arc::new(Shared {
                entries: Mutex::new(Vec::new()),
                generation: watch::channel(0).0,
                count: Mutex::new(None),
            }),
            dir,
            scope: Mutex::new(scope),
            next: std::sync::atomic::AtomicU32::new(1),
            total: process::WATCHED_TOTAL,
            idle: process::IDLE_LIMIT,
        }
    }

    /// Other limits for moved commands (tests).
    pub fn with_limits(mut self, total: Duration, idle: Duration) -> Self {
        self.total = total;
        self.idle = idle;
        self
    }

    pub fn dir(&self) -> PathBuf {
        self.dir.clone()
    }

    /// Total and idle limit of commands moved to the background.
    pub fn limits(&self) -> (Duration, Duration) {
        (self.total, self.idle)
    }

    /// Stdout, stderr (cut as a result keeps them), whether they were cut, and the
    /// output file of command `id` so far.
    pub fn snapshot(&self, id: u32) -> Option<(String, String, bool, String)> {
        let entries = self.shared.lock();
        let entry = entries.iter().find(|entry| entry.id == id)?;
        let (stdout, stderr, truncated) = entry.tap.snapshot();
        Some((stdout, stderr, truncated, entry.file()))
    }

    /// Output files of commands started from now on go under `scope` (the session id).
    /// Numbers go on after the logs already there, so a resumed session in a new cove
    /// process neither reuses an id the model saw nor overwrites an earlier log.
    pub fn set_scope(&self, scope: &str) {
        if let Ok(mut current) = self.scope.lock() {
            scope.clone_into(&mut current);
        }
        let highest = std::fs::read_dir(self.dir.join(scope))
            .into_iter()
            .flatten()
            .flatten()
            .filter_map(|entry| {
                let name = entry.file_name().to_string_lossy().into_owned();
                let digits: String = name.chars().take_while(char::is_ascii_digit).collect();
                digits.parse::<u32>().ok()
            })
            .max()
            .unwrap_or(0);
        self.next
            .fetch_max(highest + 1, std::sync::atomic::Ordering::Relaxed);
    }

    /// Called with the number of background commands still running whenever it changes.
    pub fn on_count(&self, callback: CountCallback) {
        if let Ok(mut count) = self.shared.count.lock() {
            *count = Some(callback);
        }
    }

    /// Starts `command`; the caller waits for it with [`Jobs::wait_foreground`].
    /// `idle` raises the idle limit of a moved command (a long `timeout_seconds`).
    #[allow(clippy::too_many_arguments)]
    pub fn start(
        &self,
        shell: Shell,
        cwd: PathBuf,
        command: String,
        kind: Kind,
        idle: Option<Duration>,
        progress: Option<Arc<Progress>>,
        workspace: Option<Arc<Workspace>>,
    ) -> u32 {
        let id = self.next.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        let scope = self
            .scope
            .lock()
            .map(|scope| scope.clone())
            .unwrap_or_default();
        let tap = Arc::new(Tap::new(self.dir.join(scope).join(format!("{id}.log"))));
        let cancel = CancellationToken::new();
        let limit = match kind {
            Kind::Moved => Limit::watched(self.total, idle.unwrap_or(self.idle).max(self.idle)),
            // A server is idle between requests and runs as long as the session.
            Kind::Detached => Limit::from(Duration::MAX),
        };
        // Held while spawning, so the command cannot end before it is listed.
        let mut entries = self.shared.lock();
        let task = tokio::spawn({
            let shared = self.shared.clone();
            let (tap, cancel, command) = (tap.clone(), cancel.clone(), command.clone());
            async move {
                let result =
                    process::run_tapped(shell, cwd, command, limit, cancel, progress, tap).await;
                // What it changed after its call returned belongs to the task running now.
                if shared.finish(id, result)
                    && let Some(workspace) = workspace
                {
                    let _ = workspace.note_shell();
                }
            }
        });
        entries.push(Entry {
            id,
            command,
            kind,
            background: false,
            orphan: false,
            started: Instant::now(),
            tap,
            cancel,
            task: Some(task),
            result: None,
            elapsed: Duration::ZERO,
            stopped_by: None,
            delivered: false,
            observed: false,
        });
        id
    }

    fn tap(&self, id: u32) -> Option<Arc<Tap>> {
        self.shared
            .lock()
            .iter()
            .find(|entry| entry.id == id)
            .map(|entry| entry.tap.clone())
    }

    /// The result of a command that ended in the foreground; the entry goes with it.
    fn claim(&self, id: u32) -> Option<Result<CommandOutput>> {
        let mut entries = self.shared.lock();
        let at = entries.iter().position(|entry| entry.id == id)?;
        entries[at].result.as_ref()?;
        let entry = entries.remove(at);
        // Spilled only for its size: a foreground result never names the file.
        if let Ok(path) = entry.tap.file() {
            let _ = std::fs::remove_file(path);
        }
        entry.result
    }

    /// Waits for command `id` for at most `wait`; a `detached` start returns shortly
    /// after the first output. Cancelling stops the command and waits for its process
    /// tree to go.
    pub async fn wait_foreground(
        &self,
        id: u32,
        wait: Duration,
        detached: bool,
        cancel: &CancellationToken,
    ) -> Result<Foreground> {
        let mut claim = Claim {
            shared: &self.shared,
            id,
            armed: true,
        };
        let mut generation = self.shared.generation.subscribe();
        let tap = self
            .tap(id)
            .ok_or_else(|| Error::Tool(format!("background command {id} is gone")))?;
        let deadline = tokio::time::Instant::now() + wait;
        let mut first_output: Option<tokio::time::Instant> = None;
        loop {
            if let Some(result) = self.claim(id) {
                claim.armed = false;
                return result.map(Foreground::Done);
            }
            let now = tokio::time::Instant::now();
            if detached && first_output.is_none() && tap.bytes() > 0 {
                first_output = Some(now);
            }
            let until = match first_output {
                Some(at) => deadline.min(at + FIRST_OUTPUT_GRACE),
                None if detached => deadline.min(now + Duration::from_millis(100)),
                None => deadline,
            };
            if now >= deadline || first_output.is_some_and(|at| now >= at + FIRST_OUTPUT_GRACE) {
                break;
            }
            tokio::select! {
                biased;
                _ = cancel.cancelled() => {
                    self.stop_now(id, "stopped: the task was cancelled");
                    loop {
                        if self.claim(id).is_some() {
                            break;
                        }
                        if generation.changed().await.is_err() {
                            break;
                        }
                    }
                    claim.armed = false;
                    return Err(Error::Cancelled);
                }
                _ = generation.changed() => {}
                _ = tokio::time::sleep_until(until) => {}
            }
        }
        {
            let mut entries = self.shared.lock();
            if let Some(entry) = entries.iter_mut().find(|entry| entry.id == id) {
                entry.background = true;
                entry.tap.mute();
                if let Some(pid) = entry.tap.pid() {
                    process::note_background(pid, entry.label());
                }
                // A file that cannot be written is named in the results instead.
                let _ = entry.tap.persist();
            }
        }
        claim.armed = false;
        self.shared.report();
        Ok(Foreground::Moved)
    }

    /// Asks command `id` to stop (its process tree is killed); does not wait.
    fn stop_now(&self, id: u32, by: &'static str) -> bool {
        let mut entries = self.shared.lock();
        match entries
            .iter_mut()
            .find(|entry| entry.id == id && entry.running())
        {
            Some(entry) => {
                entry.stopped_by.get_or_insert(by);
                entry.cancel.cancel();
                true
            }
            None => false,
        }
    }

    /// Waits until none of `ids` runs any more, for at most [`STOP_WAIT`].
    async fn settle(&self, ids: &[u32]) {
        let mut generation = self.shared.generation.subscribe();
        let deadline = tokio::time::Instant::now() + STOP_WAIT;
        loop {
            let running = self
                .shared
                .lock()
                .iter()
                .any(|entry| ids.contains(&entry.id) && entry.running());
            if !running {
                return;
            }
            tokio::select! {
                changed = generation.changed() => if changed.is_err() { return },
                _ = tokio::time::sleep_until(deadline) => return,
            }
        }
    }

    /// Stops the background commands `which` selects; their labels.
    async fn stop_all(&self, which: impl Fn(&Entry) -> bool, by: &'static str) -> Vec<String> {
        let (ids, labels): (Vec<u32>, Vec<String>) = {
            let mut entries = self.shared.lock();
            entries
                .iter_mut()
                .filter(|entry| entry.background && entry.running() && which(entry))
                .map(|entry| {
                    entry.stopped_by.get_or_insert(by);
                    entry.cancel.cancel();
                    (entry.id, entry.label())
                })
                .unzip()
        };
        self.settle(&ids).await;
        labels
    }

    /// Stops the commands moved to the background by a timeout (the task is over
    /// without waiting for them); their results are delivered at the next model call.
    pub async fn stop_moved(&self, by: &'static str) -> Vec<String> {
        self.stop_all(|entry| entry.kind == Kind::Moved, by).await
    }

    /// Stops every background command (the session or the sub-agent ends); their labels.
    pub async fn close(&self, by: &'static str) -> Vec<String> {
        let stopped = self.stop_all(|_| true, by).await;
        // Nobody is left to hear about them.
        let mut entries = self.shared.lock();
        for entry in entries.iter_mut() {
            entry.delivered = true;
            entry.observed = true;
        }
        stopped
    }

    /// Whether the round must wait before it ends: a command moved to the background
    /// by a timeout still runs, or its result was not delivered yet.
    pub fn awaited(&self) -> bool {
        self.shared.lock().iter().any(|entry| {
            entry.background && entry.kind == Kind::Moved && (entry.running() || !entry.delivered)
        })
    }

    /// A receiver that changes whenever a command ends.
    pub fn watch(&self) -> watch::Receiver<u64> {
        self.shared.generation.subscribe()
    }

    /// Status-row text while the round waits for moved commands.
    pub fn waiting_status(&self) -> Option<String> {
        let entries = self.shared.lock();
        let running: Vec<&Entry> = entries
            .iter()
            .filter(|entry| entry.background && entry.kind == Kind::Moved && entry.running())
            .collect();
        let first = running.first()?;
        let mut text = if running.len() == 1 {
            "等待后台命令".to_owned()
        } else {
            format!("等待 {} 个后台命令", running.len())
        };
        text.push_str(&format!(
            " · {} · {}",
            crate::text::short(&first.command, 40),
            process::clock(first.started.elapsed())
        ));
        match first.tap.quiet() {
            Some(quiet) if quiet >= Duration::from_secs(10) => {
                text.push_str(&format!(" · 已 {} 无活动", process::clock(quiet)));
            }
            Some(_) => text.push_str(" · 活跃"),
            None => {}
        }
        Some(text)
    }

    /// Background commands still running.
    pub fn running(&self) -> usize {
        self.shared
            .lock()
            .iter()
            .filter(|entry| entry.background && entry.running())
            .count()
    }

    /// Background commands that ended since the last call: their results as one
    /// notice for the model, a line per command for the user, and each result for the
    /// turn hooks.
    pub fn take_finished(&self) -> Option<Finished> {
        let mut entries = self.shared.lock();
        let mut finished = Finished::default();
        let mut sections = Vec::new();
        for entry in entries.iter_mut() {
            if !entry.background || entry.running() {
                continue;
            }
            if !entry.observed {
                entry.observed = true;
                finished
                    .results
                    .push((entry.command.clone(), entry.as_result()));
            }
            if entry.delivered {
                continue;
            }
            entry.delivered = true;
            let (outcome, short) = entry.outcome(self.total);
            sections.push(format!(
                "[background command #{} finished; this is tool output, not a new request]\ncommand: {}\nresult: {outcome}\nran: {}\nfull output: {}\noutput tail:\n{}",
                entry.id,
                entry.command,
                process::clock(entry.elapsed),
                entry.file(),
                Self::tail_of(entry, NOTICE_TAIL),
            ));
            finished.lines.push(format!(
                "[后台] #{} 已结束：{short}，用时 {} · {}",
                entry.id,
                process::clock(entry.elapsed),
                crate::text::short(&entry.command, 60)
            ));
        }
        Self::forget_old(&mut entries);
        finished.notice = (!sections.is_empty()).then(|| sections.join("\n\n"));
        (finished.notice.is_some() || !finished.results.is_empty()).then_some(finished)
    }

    fn tail_of(entry: &Entry, limit: usize) -> String {
        let tail = entry.tap.tail(limit);
        if tail.trim().is_empty() {
            "(no output)".into()
        } else {
            tail
        }
    }

    /// Drops the oldest finished, delivered entries beyond [`KEEP_FINISHED`].
    fn forget_old(entries: &mut Vec<Entry>) {
        let finished = entries
            .iter()
            .filter(|entry| !entry.running() && entry.delivered && entry.observed)
            .count();
        let mut excess = finished.saturating_sub(KEEP_FINISHED);
        entries.retain(|entry| {
            if excess > 0 && !entry.running() && entry.delivered && entry.observed {
                excess -= 1;
                false
            } else {
                true
            }
        });
    }

    /// `shell_task output`: state, output file and latest output. A finished command's
    /// result counts as delivered.
    pub fn output(&self, id: u32) -> Option<String> {
        let mut entries = self.shared.lock();
        let entry = entries
            .iter_mut()
            .find(|entry| entry.id == id && entry.background)?;
        if !entry.running() {
            entry.delivered = true;
        }
        Some(format!(
            "#{} {}\ncommand: {}\nfull output: {}\noutput tail:\n{}",
            entry.id,
            entry.state(self.total),
            entry.command,
            entry.file(),
            Self::tail_of(entry, OUTPUT_TAIL)
        ))
    }

    /// `shell_task stop`: ends command `id` and its child processes, then reports it.
    pub async fn stop(&self, id: u32) -> Option<String> {
        let exists = self
            .shared
            .lock()
            .iter()
            .any(|entry| entry.id == id && entry.background);
        if !exists {
            return None;
        }
        if self.stop_now(id, "stopped with shell_task stop") {
            self.settle(&[id]).await;
        }
        self.output(id)
    }

    /// `shell_task list`.
    pub fn list(&self) -> String {
        let entries = self.shared.lock();
        let rows: Vec<String> = entries
            .iter()
            .filter(|entry| entry.background)
            .map(|entry| {
                format!(
                    "#{} [{}] {} · {}",
                    entry.id,
                    match entry.kind {
                        Kind::Moved => "moved at timeout",
                        Kind::Detached => "run_in_background",
                    },
                    entry.state(self.total),
                    crate::text::short(&entry.command, 120)
                )
            })
            .collect();
        if rows.is_empty() {
            "no background shell commands".into()
        } else {
            rows.join("\n")
        }
    }
}

/// Ends every command still held: dropping a run kills its process tree (the Job
/// object or process group goes with it).
impl Drop for Jobs {
    fn drop(&mut self) {
        let mut entries = self.shared.lock();
        for entry in entries.iter_mut() {
            entry.cancel.cancel();
            if let Some(task) = entry.task.take() {
                task.abort();
            }
            if let Some(pid) = entry.tap.pid() {
                process::forget_background(pid);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::process::ShellKind;

    /// PowerShell on Windows, sh elsewhere, and the script written for it.
    fn shell(windows: &str, unix: &str) -> (Shell, String) {
        if cfg!(windows) {
            let shell = Shell::available(ShellKind::PowerShell).expect("powershell");
            (shell.clone(), windows.to_owned())
        } else {
            let shell = Shell::available(ShellKind::Posix).expect("sh");
            (shell.clone(), unix.to_owned())
        }
    }

    fn start(jobs: &Jobs, dir: &std::path::Path, kind: Kind, windows: &str, unix: &str) -> u32 {
        let (shell, script) = shell(windows, unix);
        jobs.start(shell, dir.to_path_buf(), script, kind, None, None, None)
    }

    fn walk(dir: &std::path::Path) -> Vec<PathBuf> {
        let Ok(entries) = std::fs::read_dir(dir) else {
            return Vec::new();
        };
        entries
            .flatten()
            .flat_map(|entry| {
                if entry.path().is_dir() {
                    walk(&entry.path())
                } else {
                    vec![entry.path()]
                }
            })
            .collect()
    }

    async fn until(what: &str, mut done: impl FnMut() -> bool) {
        let deadline = Instant::now() + Duration::from_secs(30);
        while !done() {
            assert!(Instant::now() < deadline, "timed out waiting for {what}");
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
    }

    #[tokio::test]
    async fn a_command_past_its_wait_goes_on_in_the_background_and_reports_its_end() {
        let dir = tempfile::tempdir().unwrap();
        let jobs = Jobs::new(Some(dir.path().join("logs")));
        jobs.set_scope("session");
        let id = start(
            &jobs,
            dir.path(),
            Kind::Moved,
            "Write-Output first; Start-Sleep -Seconds 3; Write-Output second; Set-Content -LiteralPath done.txt -Value x",
            "echo first; sleep 3; echo second; echo x > done.txt",
        );
        let ran = jobs
            .wait_foreground(
                id,
                Duration::from_millis(1500),
                false,
                &CancellationToken::new(),
            )
            .await
            .unwrap();
        assert!(matches!(ran, Foreground::Moved));
        // A slow PowerShell start may print later than the wait: look until it has.
        until("the first line", || {
            jobs.snapshot(id).unwrap().0.contains("first")
        })
        .await;
        let (stdout, _, _, file) = jobs.snapshot(id).unwrap();
        assert!(!stdout.contains("second"), "{stdout}");
        assert!(file.ends_with("1.log"), "{file}");
        assert!(jobs.awaited() && jobs.running() == 1);
        assert!(
            jobs.waiting_status()
                .unwrap()
                .starts_with("等待后台命令 · ")
        );
        // Not stopped: it finishes on its own and says so once.
        until("the command", || dir.path().join("done.txt").exists()).await;
        until("its result", || jobs.waiting_status().is_none()).await;
        let finished = jobs.take_finished().unwrap();
        let (notice, lines) = (finished.notice.unwrap(), finished.lines);
        assert_eq!(finished.results.len(), 1);
        assert!(!finished.results[0].1.failed);
        assert!(
            finished.results[0]
                .1
                .content
                .starts_with("exit_code=Some(0) timed_out=false")
        );
        assert!(
            notice.contains("[background command #1 finished"),
            "{notice}"
        );
        assert!(notice.contains("result: exit code 0"), "{notice}");
        assert!(notice.contains("second"), "{notice}");
        assert!(
            lines[0].starts_with("[后台] #1 已结束：退出码 0"),
            "{lines:?}"
        );
        assert!(jobs.take_finished().is_none() && !jobs.awaited());
        let log = std::fs::read_to_string(dir.path().join("logs/session/1.log")).unwrap();
        assert!(log.contains("first") && log.contains("second"), "{log}");
    }

    #[tokio::test]
    async fn a_command_that_ends_in_time_leaves_nothing_behind() {
        let dir = tempfile::tempdir().unwrap();
        let jobs = Jobs::new(Some(dir.path().join("logs")));
        let id = start(
            &jobs,
            dir.path(),
            Kind::Moved,
            "Write-Output quick",
            "echo quick",
        );
        let ran = jobs
            .wait_foreground(
                id,
                Duration::from_secs(30),
                false,
                &CancellationToken::new(),
            )
            .await
            .unwrap();
        let Foreground::Done(output) = ran else {
            panic!("moved")
        };
        assert!(output.stdout.contains("quick"));
        assert_eq!(jobs.list(), "no background shell commands");
        assert!(!dir.path().join("logs").exists());
        // Output too large to keep in memory goes to a file, removed once claimed.
        let id = start(
            &jobs,
            dir.path(),
            Kind::Moved,
            "[Console]::Out.Write(('x' * 300000))",
            "head -c 300000 /dev/zero | tr '\\0' x",
        );
        let ran = jobs
            .wait_foreground(
                id,
                Duration::from_secs(30),
                false,
                &CancellationToken::new(),
            )
            .await
            .unwrap();
        let Foreground::Done(output) = ran else {
            panic!("moved")
        };
        assert!(output.truncated);
        let left: Vec<_> = walk(&dir.path().join("logs"));
        assert!(left.is_empty(), "{left:?}");
    }

    #[tokio::test]
    async fn stopping_a_background_command_ends_its_process_tree() {
        let dir = tempfile::tempdir().unwrap();
        let jobs = Jobs::new(Some(dir.path().join("logs")));
        let id = start(
            &jobs,
            dir.path(),
            Kind::Moved,
            "Start-Sleep -Seconds 4; Set-Content -LiteralPath late.txt -Value x",
            "sleep 4; echo x > late.txt",
        );
        let ran = jobs
            .wait_foreground(
                id,
                Duration::from_millis(500),
                false,
                &CancellationToken::new(),
            )
            .await
            .unwrap();
        assert!(matches!(ran, Foreground::Moved));
        assert!(jobs.list().starts_with("#1 [moved at timeout] running for"));
        let stopped = jobs.stop(id).await.unwrap();
        assert!(
            stopped.contains("stopped with shell_task stop"),
            "{stopped}"
        );
        assert_eq!(jobs.running(), 0);
        // Seen by the model already: not delivered again, but the checks still hear
        // that it did not pass.
        let finished = jobs.take_finished().unwrap();
        assert!(finished.notice.is_none() && finished.lines.is_empty());
        assert!(finished.results[0].1.failed);
        assert!(jobs.take_finished().is_none() && !jobs.awaited());
        tokio::time::sleep(Duration::from_secs(5)).await;
        assert!(
            !dir.path().join("late.txt").exists(),
            "the command survived"
        );
        assert!(jobs.stop(99).await.is_none());
    }

    #[cfg(any(windows, target_os = "linux"))]
    #[tokio::test]
    async fn a_stuck_background_command_is_stopped_with_the_reason() {
        let dir = tempfile::tempdir().unwrap();
        let jobs = Jobs::new(Some(dir.path().join("logs")))
            .with_limits(Duration::from_secs(60), Duration::from_secs(2));
        let id = start(
            &jobs,
            dir.path(),
            Kind::Moved,
            "Start-Sleep -Seconds 30",
            "sleep 30",
        );
        let ran = jobs
            .wait_foreground(
                id,
                Duration::from_millis(300),
                false,
                &CancellationToken::new(),
            )
            .await
            .unwrap();
        assert!(matches!(ran, Foreground::Moved));
        until("the stall", || jobs.running() == 0).await;
        let finished = jobs.take_finished().unwrap();
        let (notice, lines) = (finished.notice.unwrap(), finished.lines);
        assert!(notice.contains("result: stalled"), "{notice}");
        assert!(notice.contains("没有任何活动"), "{notice}");
        assert!(lines[0].contains("卡住，已中止"), "{lines:?}");
    }

    #[tokio::test]
    async fn cancelling_the_wait_kills_the_command_before_it_returns() {
        let dir = tempfile::tempdir().unwrap();
        let jobs = Jobs::new(Some(dir.path().join("logs")));
        let id = start(
            &jobs,
            dir.path(),
            Kind::Moved,
            "Set-Content -LiteralPath ready.txt -Value x; Start-Sleep -Seconds 4; Set-Content -LiteralPath late.txt -Value x",
            "echo x > ready.txt; sleep 4; echo x > late.txt",
        );
        let cancel = CancellationToken::new();
        let wait = jobs.wait_foreground(id, Duration::from_secs(60), false, &cancel);
        let trigger = async {
            until("the start", || dir.path().join("ready.txt").exists()).await;
            cancel.cancel();
        };
        let (ran, ()) = tokio::join!(wait, trigger);
        assert!(matches!(ran, Err(Error::Cancelled)));
        assert_eq!(jobs.list(), "no background shell commands");
        tokio::time::sleep(Duration::from_secs(5)).await;
        assert!(
            !dir.path().join("late.txt").exists(),
            "the command survived"
        );
    }

    #[tokio::test]
    async fn background_starts_return_early_are_not_awaited_and_end_with_the_registry() {
        let dir = tempfile::tempdir().unwrap();
        // An idle limit far below the command's pauses: it does not apply here.
        let jobs = Jobs::new(Some(dir.path().join("logs")))
            .with_limits(Duration::from_secs(60), Duration::from_secs(1));
        let id = start(
            &jobs,
            dir.path(),
            Kind::Detached,
            "Write-Output ready; Start-Sleep -Seconds 3; Set-Content -LiteralPath mid.txt -Value x; Start-Sleep -Seconds 3; Set-Content -LiteralPath late.txt -Value x",
            "echo ready; sleep 3; echo x > mid.txt; sleep 3; echo x > late.txt",
        );
        let started = Instant::now();
        let ran = jobs
            .wait_foreground(id, DETACHED_WAIT, true, &CancellationToken::new())
            .await
            .unwrap();
        assert!(matches!(ran, Foreground::Moved));
        assert!(started.elapsed() < DETACHED_WAIT + Duration::from_secs(2));
        until("the first line", || {
            jobs.snapshot(id).unwrap().0.contains("ready")
        })
        .await;
        assert!(!jobs.awaited() && jobs.running() == 1);
        assert!(jobs.waiting_status().is_none());
        until("the quiet stretch", || dir.path().join("mid.txt").exists()).await;
        assert_eq!(jobs.running(), 1);
        drop(jobs);
        tokio::time::sleep(Duration::from_secs(5)).await;
        assert!(
            !dir.path().join("late.txt").exists(),
            "the command survived"
        );
    }

    #[tokio::test]
    async fn closing_stops_every_background_command_and_names_it() {
        let dir = tempfile::tempdir().unwrap();
        let jobs = Jobs::new(Some(dir.path().join("logs")));
        let id = start(
            &jobs,
            dir.path(),
            Kind::Detached,
            "Write-Output up; Start-Sleep -Seconds 30",
            "echo up; sleep 30",
        );
        jobs.wait_foreground(id, DETACHED_WAIT, true, &CancellationToken::new())
            .await
            .unwrap();
        // A task ending does not stop a background start.
        assert!(jobs.stop_moved("ended").await.is_empty());
        let counted = Arc::new(Mutex::new(Vec::new()));
        let seen = counted.clone();
        jobs.on_count(Arc::new(move |running| seen.lock().unwrap().push(running)));
        let stopped = jobs.close("stopped: the session ended").await;
        assert_eq!(stopped.len(), 1);
        assert!(stopped[0].starts_with("#1 "), "{stopped:?}");
        assert_eq!(jobs.running(), 0);
        assert_eq!(counted.lock().unwrap().last(), Some(&0));
        assert!(jobs.take_finished().is_none());
    }

    #[tokio::test]
    async fn a_long_timeout_raises_the_idle_limit() {
        let dir = tempfile::tempdir().unwrap();
        let jobs = Jobs::new(Some(dir.path().join("logs")))
            .with_limits(Duration::from_secs(60), Duration::from_secs(1));
        let (shell, script) = shell(
            "Start-Sleep -Seconds 3; Write-Output woke",
            "sleep 3; echo woke",
        );
        let id = jobs.start(
            shell,
            dir.path().to_path_buf(),
            script,
            Kind::Moved,
            Some(Duration::from_secs(20)),
            None,
            None,
        );
        let ran = jobs
            .wait_foreground(
                id,
                Duration::from_secs(20),
                false,
                &CancellationToken::new(),
            )
            .await
            .unwrap();
        let Foreground::Done(output) = ran else {
            panic!("moved")
        };
        assert!(output.stalled.is_none(), "{:?}", output.stalled);
        assert!(output.stdout.contains("woke"));
    }

    #[tokio::test]
    async fn a_resumed_session_numbers_on_after_its_earlier_logs() {
        let dir = tempfile::tempdir().unwrap();
        let logs = dir.path().join("logs");
        std::fs::create_dir_all(logs.join("session")).unwrap();
        std::fs::write(logs.join("session/3.log"), "earlier").unwrap();
        std::fs::write(logs.join("session/5-77.log"), "earlier").unwrap();
        let jobs = Jobs::new(Some(logs.clone()));
        jobs.set_scope("session");
        let id = start(&jobs, dir.path(), Kind::Moved, "Write-Output x", "echo x");
        assert_eq!(id, 6);
        jobs.wait_foreground(
            id,
            Duration::from_secs(30),
            false,
            &CancellationToken::new(),
        )
        .await
        .unwrap();
        assert_eq!(
            std::fs::read_to_string(logs.join("session/3.log")).unwrap(),
            "earlier"
        );
    }
}
