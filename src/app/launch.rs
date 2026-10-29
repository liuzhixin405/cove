//! Process-level CLI behavior: `-p` stdin piping and `--file` attachments, exit
//! codes, and the re-exec behind `/restart` and `/cd`.

use std::{
    ffi::OsString,
    io::Read,
    path::{Path, PathBuf},
    sync::Mutex,
    time::Duration,
};

/// Most piped stdin `-p` sends; the rest is dropped with a warning.
pub const STDIN_LIMIT: usize = 8 * 1024 * 1024;
/// Wait for the first byte when nobody can see a notice (IDE, CI, another agent).
pub const STDIN_FIRST_BYTE: Duration = Duration::from_secs(3);
const FILE_LIMIT: u64 = 256 * 1024;

pub const EXIT_FAILURE: u8 = 1;
pub const EXIT_USAGE: u8 = 2;
pub const EXIT_INTERRUPTED: u8 = 130;

pub enum Piped {
    Text {
        text: String,
        truncated: bool,
    },
    /// Nothing arrived within the first-byte wait.
    NoData,
}

/// Reads `reader` to EOF keeping `limit` bytes, waiting at most `first_byte` for the
/// first chunk (`None` waits forever). A timed-out read thread is left behind; that
/// only happens in a one-shot run.
pub fn read_piped(
    reader: impl Read + Send + 'static,
    first_byte: Option<Duration>,
    limit: usize,
) -> std::io::Result<Piped> {
    let (sender, receiver) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        let mut reader = reader;
        let mut first = vec![0_u8; 64 * 1024];
        let result = reader.read(&mut first).and_then(|count| {
            first.truncate(count);
            if count > 0 {
                reader
                    .take((limit + 1).saturating_sub(count) as u64)
                    .read_to_end(&mut first)?;
            }
            Ok(first)
        });
        let _ = sender.send(result);
    });
    let data = match first_byte {
        Some(wait) => match receiver.recv_timeout(wait) {
            Ok(result) => result?,
            Err(_) => return Ok(Piped::NoData),
        },
        None => receiver
            .recv()
            .map_err(|_| std::io::Error::other("读取标准输入的线程意外停止，请重试"))??,
    };
    let data = data.strip_prefix(b"\xEF\xBB\xBF").unwrap_or(&data);
    let truncated = data.len() > limit;
    let data = &data[..data.len().min(limit)];
    // A cut inside a multi-byte character must not ship invalid UTF-8.
    let text = match std::str::from_utf8(data) {
        Ok(text) => text.to_owned(),
        Err(err) if truncated && err.error_len().is_none() => {
            String::from_utf8_lossy(&data[..err.valid_up_to()]).into_owned()
        }
        Err(_) => String::from_utf8_lossy(data).into_owned(),
    };
    Ok(Piped::Text { text, truncated })
}

/// The first-byte wait: none for a regular file or when a person watches stderr.
pub fn first_byte_wait(stdin_is_file: bool, stderr_terminal: bool) -> Option<Duration> {
    (!stdin_is_file && !stderr_terminal).then_some(STDIN_FIRST_BYTE)
}

/// True when stdin is redirected from a regular file.
pub fn stdin_is_file() -> bool {
    #[cfg(unix)]
    {
        use std::os::fd::AsFd;
        std::io::stdin()
            .as_fd()
            .try_clone_to_owned()
            .map(std::fs::File::from)
            .and_then(|file| file.metadata())
            .is_ok_and(|meta| meta.is_file())
    }
    // Not `metadata().is_file()`: a pipe has no attributes, so std reports it as a
    // file, and `-p` then waited forever on a pipe nobody writes or closes.
    #[cfg(windows)]
    {
        use std::os::windows::io::AsRawHandle;
        use windows_sys::Win32::Storage::FileSystem::{FILE_TYPE_DISK, GetFileType};
        // SAFETY: GetFileType only inspects the process's own stdin handle.
        unsafe { GetFileType(std::io::stdin().as_raw_handle()) == FILE_TYPE_DISK }
    }
}

/// The `-p` message from the prompt argument and piped stdin; either may be empty.
pub fn combine(prompt: &str, stdin: &str) -> String {
    match (prompt.trim(), stdin.trim()) {
        (prompt, "") => prompt.to_owned(),
        ("", stdin) => stdin.to_owned(),
        (prompt, stdin) => format!("{prompt}\n\n{stdin}"),
    }
}

/// Appends `--file` text attachments, each wrapped as data with its path.
pub fn attach_files(prompt: &str, files: &[PathBuf]) -> anyhow::Result<String> {
    let mut out = prompt.to_owned();
    for path in files {
        let meta = std::fs::metadata(path).map_err(|err| {
            anyhow::anyhow!("--file {}：{err}；请检查路径是否正确", path.display())
        })?;
        anyhow::ensure!(
            meta.is_file() && meta.len() <= FILE_LIMIT,
            "--file {}：只能附加不超过 256 KiB 的普通文件；更大的文件可放在工作区里，让模型自己读取",
            path.display()
        );
        let data = std::fs::read(path)?;
        let data = data.strip_prefix(b"\xEF\xBB\xBF").unwrap_or(&data);
        let text = std::str::from_utf8(data).map_err(|_| {
            anyhow::anyhow!(
                "--file {}：不是 UTF-8 文本（图片请用 --image）",
                path.display()
            )
        })?;
        out.push_str(&format!(
            "\n\n{}",
            crate::tools::external::wrap(&format!("attached file {}", path.display()), text)
        ));
    }
    Ok(out)
}

/// What a finished run asks the binary to start next.
static RESTART: Mutex<Option<Vec<OsString>>> = Mutex::new(None);

/// Requests a restart with `args` once the application has shut down.
pub fn request_restart(args: Vec<OsString>) {
    if let Ok(mut slot) = RESTART.lock() {
        *slot = Some(args);
    }
}

pub fn take_restart() -> Option<Vec<OsString>> {
    RESTART.lock().ok().and_then(|mut slot| slot.take())
}

/// Options whose value only applied to the first start.
const ONE_TIME_VALUES: &[&str] = &["-r", "--resume", "--file", "--image", "-p", "--print"];
const ONE_TIME_FLAGS: &[&str] = &["--continue", "--dump-system-prompt", "--doctor"];

/// The original arguments without what only made sense at the first start (and,
/// with `cwd`, without `--cwd`), plus `extra`.
pub fn restart_args(original: &[OsString], cwd: bool, extra: &[OsString]) -> Vec<OsString> {
    let mut out = Vec::new();
    let mut index = 0;
    while index < original.len() {
        let arg = original[index].to_string_lossy();
        let name = arg.split_once('=').map_or(arg.as_ref(), |(name, _)| name);
        let takes_value = ONE_TIME_VALUES.contains(&name) || (cwd && name == "--cwd");
        if takes_value {
            index += if arg.contains('=') { 1 } else { 2 };
            continue;
        }
        if !ONE_TIME_FLAGS.contains(&name) {
            out.push(original[index].clone());
        }
        index += 1;
    }
    out.extend_from_slice(extra);
    out
}

/// Starts the binary again with `args`: exec on Unix (never returns on success); on
/// Windows a child inherits the console and this process exits with its code.
pub fn restart(args: &[OsString]) -> anyhow::Result<u8> {
    let exe = std::env::current_exe()?;
    let exe = PathBuf::from(
        exe.to_string_lossy()
            .trim_end_matches(" (deleted)")
            .to_owned(),
    );
    restart_with(&exe, args)
}

#[cfg(unix)]
fn restart_with(exe: &Path, args: &[OsString]) -> anyhow::Result<u8> {
    use std::os::unix::process::CommandExt;
    let err = std::process::Command::new(exe).args(args).exec();
    Err(anyhow::anyhow!(
        "无法重新启动 {}：{err}；请手动再次运行 cove",
        exe.display()
    ))
}

#[cfg(windows)]
fn restart_with(exe: &Path, args: &[OsString]) -> anyhow::Result<u8> {
    use windows_sys::Win32::{Foundation::BOOL, System::Console::SetConsoleCtrlHandler};
    // Ctrl+C belongs to the child; this process only waits. A handler (unlike the
    // "ignore" flag) is not inherited, so the child still receives Ctrl+C itself.
    unsafe extern "system" fn handled(_: u32) -> BOOL {
        1
    }
    // SAFETY: registers a static handler function that touches no state.
    unsafe {
        SetConsoleCtrlHandler(Some(handled), 1);
    }
    let status = std::process::Command::new(exe).args(args).status()?;
    Ok(status.code().map_or(EXIT_FAILURE, |code| {
        u8::try_from(code).unwrap_or(EXIT_FAILURE)
    }))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn args(list: &[&str]) -> Vec<OsString> {
        list.iter().map(OsString::from).collect()
    }

    #[test]
    fn restart_drops_one_time_options() {
        let original = args(&[
            "--model",
            "m",
            "-r",
            "abc",
            "--cwd",
            "x",
            "--file=a.txt",
            "--continue",
        ]);
        assert_eq!(
            restart_args(&original, false, &args(&["--resume", "id"])),
            args(&["--model", "m", "--cwd", "x", "--resume", "id"])
        );
        assert_eq!(
            restart_args(&original, true, &args(&["--cwd", "y"])),
            args(&["--model", "m", "--cwd", "y"])
        );
    }

    #[test]
    fn piped_input_strips_bom_caps_and_times_out() {
        let data = b"\xEF\xBB\xBFhello".to_vec();
        match read_piped(std::io::Cursor::new(data), None, 100).unwrap() {
            Piped::Text { text, truncated } => {
                assert_eq!(text, "hello");
                assert!(!truncated);
            }
            Piped::NoData => panic!(),
        }
        let data = "界".repeat(10).into_bytes();
        match read_piped(std::io::Cursor::new(data), None, 7).unwrap() {
            Piped::Text { text, truncated } => {
                assert_eq!(text, "界界");
                assert!(truncated);
            }
            Piped::NoData => panic!(),
        }
        struct Never;
        impl Read for Never {
            fn read(&mut self, _: &mut [u8]) -> std::io::Result<usize> {
                std::thread::sleep(Duration::from_secs(30));
                Ok(0)
            }
        }
        assert!(matches!(
            read_piped(Never, Some(Duration::from_millis(50)), 10).unwrap(),
            Piped::NoData
        ));
        assert_eq!(first_byte_wait(true, false), None);
        assert_eq!(first_byte_wait(false, true), None);
        assert_eq!(first_byte_wait(false, false), Some(STDIN_FIRST_BYTE));
        assert_eq!(combine(" ask ", "\nlog\n"), "ask\n\nlog");
        assert_eq!(combine("", "log"), "log");
    }

    #[test]
    fn attachments_are_wrapped_text_files() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("notes.txt");
        std::fs::write(&path, "remember this").unwrap();
        let text = attach_files("Q", std::slice::from_ref(&path)).unwrap();
        assert!(text.starts_with("Q\n\n<external_content source=\"attached file"));
        assert!(text.contains("remember this"));
        std::fs::write(&path, [0xff_u8, 0xfe, 0x00]).unwrap();
        assert!(attach_files("Q", &[path]).is_err());
    }
}
