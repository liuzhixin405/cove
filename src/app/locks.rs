//! A command blocked on a lock another process holds (cargo's build directory,
//! yarn, Bazel's server, apt/dpkg, MSBuild's locked outputs): name the holders
//! and let the user end them or keep waiting.

use std::{
    collections::HashSet,
    sync::{Arc, LazyLock},
    time::Duration,
};

use futures_util::future::BoxFuture;
use tokio_util::sync::CancellationToken;

use crate::{
    app::ui::print_text,
    domain::UserInput,
    process::{Argv, LockWait, run_argv},
};

/// A known "waiting for another process" message.
struct Lock {
    /// Lowercase fragment of the output line.
    pattern: &'static str,
    tool: &'static str,
    /// Process names that hold it, when the message does not name a PID.
    holders: &'static [&'static str],
    /// Ending the holder is safe; package managers mid-install are not.
    killable: bool,
}

const LOCKS: &[Lock] = &[
    Lock {
        pattern: "blocking waiting for file lock",
        tool: "cargo",
        holders: &["cargo"],
        killable: true,
    },
    Lock {
        pattern: "waiting for the other yarn instance to finish",
        tool: "yarn",
        holders: &[],
        killable: true,
    },
    Lock {
        pattern: "another command (pid=",
        tool: "bazel",
        holders: &[],
        killable: true,
    },
    Lock {
        pattern: "the file is locked by:",
        tool: "msbuild",
        holders: &[],
        killable: true,
    },
    Lock {
        pattern: "could not get lock /var/lib/",
        tool: "apt/dpkg",
        holders: &["apt", "apt-get", "dpkg", "unattended-upgr"],
        killable: false,
    },
];

static PATTERNS: LazyLock<Vec<&'static str>> =
    LazyLock::new(|| LOCKS.iter().map(|lock| lock.pattern).collect());

/// `pid=123`, `pid 123`, `held by process 123`, `"app (123)"`.
static PID: LazyLock<regex::Regex> = LazyLock::new(|| {
    regex::Regex::new(r"(?i)(?:\bpid\b[=:\s]*|held by process\s+|\()(\d{1,7})\b").unwrap()
});

pub struct ToolLocks {
    pub input: Option<Arc<dyn UserInput>>,
}

struct Proc {
    pid: u32,
    ppid: u32,
    name: String,
    command: String,
}

impl LockWait for ToolLocks {
    fn patterns(&self) -> &'static [&'static str] {
        &PATTERNS
    }

    fn waiting(
        &self,
        child: u32,
        line: String,
        cancel: CancellationToken,
    ) -> BoxFuture<'static, ()> {
        let input = self.input.clone();
        Box::pin(async move {
            let lower = line.to_lowercase();
            let Some(lock) = LOCKS.iter().find(|lock| lower.contains(lock.pattern)) else {
                return;
            };
            let tool = lock.tool;
            let all = processes(&cancel).await.unwrap_or_default();
            let holders = holders(&all, child, lock, &line);
            if cancel.is_cancelled() {
                return;
            }
            if holders.is_empty() {
                let _ = print_text(
                    &format!(
                        "[锁等待] {tool} 在等待另一个进程释放锁，没找到占用者（{}）；继续等待，按两次 Esc 可中断\n",
                        clip(&line, 100)
                    ),
                    true,
                );
                return;
            }
            let list: String = holders
                .iter()
                .take(5)
                .map(|proc| format!("\n  PID {}  {}", proc.pid, clip(&proc.command, 100)))
                .collect();
            let more = if holders.len() > 5 {
                format!("\n  …另有 {} 个", holders.len() - 5)
            } else {
                String::new()
            };
            let Some(input) = input.filter(|_| lock.killable) else {
                let why = if lock.killable {
                    ""
                } else {
                    "（中途结束可能损坏包数据库，请等它完成）"
                };
                let _ = print_text(
                    &format!("[锁等待] {tool} 在等待锁，占用者：{list}{more}\n继续等待{why}\n"),
                    true,
                );
                return;
            };
            let prompt = format!(
                "{tool} 在等待锁，被以下进程占用：{list}{more}\n1. 结束这些进程 (k)\n2. 继续等待 (w)\n选择？[k/W] "
            );
            let Ok(answer) = input.answer(prompt, cancel.clone()).await else {
                return;
            };
            if !matches!(answer.trim().to_lowercase().as_str(), "k" | "1" | "kill" | "结束") {
                return;
            }
            let mut ended = 0;
            for proc in &holders {
                if kill(proc.pid, &cancel).await {
                    ended += 1;
                }
            }
            let _ = print_text(
                &format!("[锁等待] 已结束 {ended}/{} 个占用锁的进程\n", holders.len()),
                true,
            );
        })
    }
}

fn clip(text: &str, max: usize) -> String {
    let text = text.trim();
    if text.chars().count() <= max {
        return text.to_owned();
    }
    let mut out: String = text.chars().take(max).collect();
    out.push('…');
    out
}

/// The holders outside the waiting command's own process tree: the PIDs the
/// message names, else processes with the lock's holder names.
fn holders<'a>(all: &'a [Proc], child: u32, lock: &Lock, line: &str) -> Vec<&'a Proc> {
    let mut own = HashSet::from([child]);
    // Parents may be listed after their children: repeat until nothing joins.
    loop {
        let before = own.len();
        for proc in all {
            if own.contains(&proc.ppid) {
                own.insert(proc.pid);
            }
        }
        if own.len() == before {
            break;
        }
    }
    let named: HashSet<u32> = PID
        .captures_iter(line)
        .filter_map(|captures| captures[1].parse().ok())
        .collect();
    all.iter()
        .filter(|proc| {
            let name = proc.name.to_ascii_lowercase();
            let name = name.strip_suffix(".exe").unwrap_or(&name);
            let holds = if named.is_empty() {
                lock.holders.contains(&name)
            } else {
                named.contains(&proc.pid)
            };
            holds && !own.contains(&proc.pid) && proc.pid != std::process::id()
        })
        .collect()
}

async fn processes(cancel: &CancellationToken) -> Option<Vec<Proc>> {
    let cwd = std::env::temp_dir();
    #[cfg(windows)]
    let argv = Argv::new(cwd, "powershell").args([
        "-NoLogo",
        "-NoProfile",
        "-NonInteractive",
        "-Command",
        "[Console]::OutputEncoding=[Text.Encoding]::UTF8; Get-CimInstance Win32_Process | Select-Object ProcessId,ParentProcessId,Name,CommandLine | ConvertTo-Json -Compress",
    ]);
    #[cfg(unix)]
    let argv = Argv::new(cwd, "ps").args(["-eo", "pid=,ppid=,args="]);
    let output = run_argv(
        argv.direct()
            .timeout(Duration::from_secs(20))
            .limit(16 * 1024 * 1024),
        cancel,
    )
    .await
    .ok()
    .filter(|output| output.success())?;
    parse(&output.stdout_text())
}

#[cfg(windows)]
fn parse(text: &str) -> Option<Vec<Proc>> {
    let value: serde_json::Value = serde_json::from_str(text.trim()).ok()?;
    let rows = match value {
        serde_json::Value::Array(rows) => rows,
        row => vec![row],
    };
    Some(
        rows.iter()
            .filter_map(|row| {
                Some(Proc {
                    pid: row.get("ProcessId")?.as_u64()? as u32,
                    ppid: row.get("ParentProcessId")?.as_u64()? as u32,
                    name: row.get("Name")?.as_str()?.to_owned(),
                    command: row
                        .get("CommandLine")
                        .and_then(serde_json::Value::as_str)
                        .unwrap_or_default()
                        .to_owned(),
                })
            })
            .collect(),
    )
}

#[cfg(unix)]
fn parse(text: &str) -> Option<Vec<Proc>> {
    Some(
        text.lines()
            .filter_map(|line| {
                let mut words = line.split_whitespace();
                let pid = words.next()?.parse().ok()?;
                let ppid = words.next()?.parse().ok()?;
                let command: Vec<&str> = words.collect();
                let program = command.first()?;
                Some(Proc {
                    pid,
                    ppid,
                    name: program.rsplit('/').next().unwrap_or(program).to_owned(),
                    command: command.join(" "),
                })
            })
            .collect(),
    )
}

/// Ends `pid` and, on Windows, the processes it started (rustc, build scripts).
async fn kill(pid: u32, cancel: &CancellationToken) -> bool {
    let cwd = std::env::temp_dir();
    let pid = pid.to_string();
    #[cfg(windows)]
    let argv = Argv::new(cwd, "taskkill").args(["/PID", &pid, "/T", "/F"]);
    #[cfg(unix)]
    let argv = Argv::new(cwd, "kill").args(["-TERM", &pid]);
    run_argv(argv.direct().timeout(Duration::from_secs(10)), cancel)
        .await
        .is_ok_and(|output| output.success())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn proc(pid: u32, ppid: u32, name: &str) -> Proc {
        Proc {
            pid,
            ppid,
            name: name.into(),
            command: format!("{name} build"),
        }
    }

    fn lock(line: &str) -> &'static Lock {
        let lower = line.to_lowercase();
        LOCKS.iter().find(|lock| lower.contains(lock.pattern)).unwrap()
    }

    #[test]
    fn holders_skip_the_waiting_commands_own_tree() {
        let all = [
            // The waiting command: shell 10 → cargo 11 (listed before its parent).
            proc(11, 10, "cargo.exe"),
            proc(10, 1, "powershell.exe"),
            // Another terminal's build and its compiler.
            proc(20, 2, "cargo.exe"),
            proc(21, 20, "rustc.exe"),
            proc(30, 3, "cargo"),
        ];
        let line = "Blocking waiting for file lock on build directory";
        let pids: Vec<u32> = holders(&all, 10, lock(line), line)
            .iter()
            .map(|proc| proc.pid)
            .collect();
        assert_eq!(pids, [20, 30]);
    }

    #[test]
    fn a_pid_named_in_the_message_is_the_holder_whatever_its_name() {
        let all = [proc(40, 1, "node.exe"), proc(41, 1, "node.exe"), proc(50, 1, "java")];
        for (line, holder) in [
            ("warning Waiting for the other yarn instance to finish (pid 41)", 41),
            ("Another command (pid=50) is running. Waiting for it to complete on the server", 50),
            (
                r#"warning MSB3026: Could not copy "a.dll". The file is locked by: "MyApp (40)""#,
                40,
            ),
            (
                "E: Could not get lock /var/lib/dpkg/lock-frontend. It is held by process 41 (apt)",
                41,
            ),
        ] {
            let pids: Vec<u32> = holders(&all, 99, lock(line), line)
                .iter()
                .map(|proc| proc.pid)
                .collect();
            assert_eq!(pids, [holder], "{line}");
        }
        assert!(!lock("It is held by process 41 (apt) Could not get lock /var/lib/dpkg").killable);
    }

    #[tokio::test]
    async fn the_process_list_includes_this_process() {
        let all = processes(&CancellationToken::new()).await.unwrap();
        let me = all.iter().find(|proc| proc.pid == std::process::id()).unwrap();
        assert!(me.ppid > 0 && !me.name.is_empty());
    }
}
