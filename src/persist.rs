//! Durable files: atomic replacement, bounded JSON state, and cross-process
//! locks. Every file cove rewrites goes through [`write_atomic`] (or
//! [`atomic_json`]); every file several processes rewrite is changed under
//! [`with_lock`].

use std::{
    fs::{File, OpenOptions},
    io::{Read, Write},
    path::{Path, PathBuf},
    time::Duration,
};

use fs2::FileExt;
use serde::Serialize;

use crate::domain::{Error, Result};

/// Largest JSON state file read or written.
pub const STATE_LIMIT: u64 = 32 * 1024 * 1024;

/// Writes `bytes` to a temporary file next to `path`, syncs it and moves it
/// over `path`. `private` makes the file owner-only on Unix (it may hold keys).
pub fn write_atomic(path: &Path, bytes: &[u8], private: bool) -> Result<()> {
    let parent = path
        .parent()
        .filter(|parent| !parent.as_os_str().is_empty())
        .unwrap_or(Path::new("."));
    std::fs::create_dir_all(parent).map_err(|err| io_context("create", parent, err))?;
    let mut file = tempfile::NamedTempFile::new_in(parent)
        .map_err(|err| io_context("create temporary file in", parent, err))?;
    #[cfg(unix)]
    if private {
        use std::os::unix::fs::PermissionsExt;
        file.as_file()
            .set_permissions(std::fs::Permissions::from_mode(0o600))
            .map_err(|err| io_context("restrict", file.path(), err))?;
    }
    #[cfg(not(unix))]
    let _ = private;
    file.write_all(bytes)
        .map_err(|err| io_context("write", file.path(), err))?;
    file.as_file()
        .sync_all()
        .map_err(|err| io_context("sync", file.path(), err))?;
    replace_with(file, path).map_err(|err| {
        let holders = file_holders(path);
        let op = if holders.is_empty() {
            "replace".to_owned()
        } else {
            format!("replace (in use by {holders})")
        };
        io_context(&op, path, err)
    })?;
    #[cfg(unix)]
    File::open(parent)?.sync_all()?;
    Ok(())
}

/// Opens (creating it and its directory) the lock file at `path` and locks it
/// exclusively, retrying for up to `patience`. A lock held longer is reported
/// through `busy`; other failures as I/O errors. The lock lasts as long as the
/// returned file.
pub fn try_lock(
    path: &Path,
    patience: Duration,
    busy: impl FnOnce(std::io::Error) -> Error,
) -> Result<File> {
    let file = open_lock(path)?;
    let deadline = std::time::Instant::now() + patience;
    loop {
        match file.try_lock_exclusive() {
            Ok(()) => return Ok(file),
            Err(err) if std::time::Instant::now() >= deadline => return Err(busy(err)),
            Err(_) => std::thread::sleep(Duration::from_millis(50)),
        }
    }
}

/// [`try_lock`] for async callers: waits without blocking the runtime.
pub async fn try_lock_async(
    path: &Path,
    patience: Duration,
    busy: impl FnOnce(std::io::Error) -> Error,
) -> Result<File> {
    let file = open_lock(path)?;
    let deadline = tokio::time::Instant::now() + patience;
    loop {
        match file.try_lock_exclusive() {
            Ok(()) => return Ok(file),
            Err(err) if tokio::time::Instant::now() >= deadline => return Err(busy(err)),
            Err(_) => tokio::time::sleep(Duration::from_millis(100)).await,
        }
    }
}

/// Waits as long as it takes for the lock at `path`.
pub fn lock(path: &Path) -> Result<File> {
    let file = open_lock(path)?;
    file.lock_exclusive()
        .map_err(|err| io_context("lock", path, err))?;
    Ok(file)
}

/// Runs `change` while holding `<path>.lock`: the way to read, change and write
/// back a file other cove processes rewrite too.
pub fn with_lock<R, E: From<Error>>(
    path: &Path,
    change: impl FnOnce() -> std::result::Result<R, E>,
) -> std::result::Result<R, E> {
    let _lock = lock_for(path)?;
    change()
}

/// [`lock`] on `<path>.lock`, for a change that spans more than one call.
pub fn lock_for(path: &Path) -> Result<File> {
    lock(&lock_path(path))
}

fn lock_path(path: &Path) -> PathBuf {
    let mut name = path.file_name().unwrap_or_default().to_os_string();
    name.push(".lock");
    path.with_file_name(name)
}

fn open_lock(path: &Path) -> Result<File> {
    if let Some(parent) = path
        .parent()
        .filter(|parent| !parent.as_os_str().is_empty())
    {
        std::fs::create_dir_all(parent).map_err(|err| io_context("create", parent, err))?;
    }
    OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .open(path)
        .map_err(|err| io_context("open lock", path, err))
}

pub fn read_json<T: serde::de::DeserializeOwned>(path: &Path) -> Result<T> {
    let mut data = Vec::new();
    File::open(path)
        .map_err(|err| io_context("open", path, err))?
        .take(STATE_LIMIT + 1)
        .read_to_end(&mut data)
        .map_err(|err| io_context("read", path, err))?;
    if data.len() as u64 > STATE_LIMIT {
        return Err(Error::Session("state file exceeds 32 MiB".into()));
    }
    Ok(serde_json::from_slice(&data)?)
}

/// Processes that have `path` open, as the Windows Restart Manager reports them
/// ("name (pid)", comma separated); empty when none or unknown.
#[cfg(windows)]
fn file_holders(path: &Path) -> String {
    use windows_sys::Win32::System::RestartManager::{
        RM_PROCESS_INFO, RmEndSession, RmGetList, RmRegisterResources, RmStartSession,
    };
    let wide: Vec<u16> = crate::paths::display(path)
        .encode_utf16()
        .chain(Some(0))
        .collect();
    let mut key = [0_u16; 64];
    let mut session = 0_u32;
    // All pointers refer to live local buffers of the declared sizes.
    unsafe {
        if RmStartSession(&mut session, 0, key.as_mut_ptr()) != 0 {
            return String::new();
        }
        let files = [wide.as_ptr()];
        let mut names = Vec::new();
        if RmRegisterResources(
            session,
            1,
            files.as_ptr(),
            0,
            std::ptr::null(),
            0,
            std::ptr::null(),
        ) == 0
        {
            let mut needed = 0_u32;
            let mut count = 16_u32;
            let mut reasons = 0_u32;
            let mut infos: Vec<RM_PROCESS_INFO> = vec![std::mem::zeroed(); 16];
            if RmGetList(
                session,
                &mut needed,
                &mut count,
                infos.as_mut_ptr(),
                &mut reasons,
            ) == 0
            {
                for info in &infos[..count as usize] {
                    let end = info.strAppName.iter().position(|c| *c == 0).unwrap_or(256);
                    names.push(format!(
                        "{} ({})",
                        String::from_utf16_lossy(&info.strAppName[..end]),
                        info.Process.dwProcessId
                    ));
                }
            }
        }
        RmEndSession(session);
        names.join(", ")
    }
}

#[cfg(not(windows))]
fn file_holders(_: &Path) -> String {
    String::new()
}

/// Moves a written temporary file over `path`. `NamedTempFile::persist` uses
/// `MoveFileExW`, which on Windows fails with "access denied" while any process (a
/// reader, an editor, a virus scanner, the search indexer) has the target open, even
/// with delete sharing; `std::fs::rename` uses POSIX rename semantics there and
/// replaces the file anyway, as it does on Unix.
pub fn replace_with(file: tempfile::NamedTempFile, path: &Path) -> std::io::Result<()> {
    let temporary = file.into_temp_path();
    // A scanner or indexer that opened the target without delete sharing holds it only
    // briefly; wait it out instead of failing the save (about 1.5s in all).
    let mut attempt = 0;
    while let Err(err) = std::fs::rename(&temporary, path) {
        if attempt >= 5 || err.kind() != std::io::ErrorKind::PermissionDenied {
            return Err(err);
        }
        attempt += 1;
        std::thread::sleep(std::time::Duration::from_millis(100 * attempt));
    }
    let _ = temporary.keep();
    Ok(())
}

/// An I/O error that names the operation and the file, keeping its kind.
pub fn io_context(op: &str, path: &Path, err: std::io::Error) -> Error {
    Error::Io(std::io::Error::new(
        err.kind(),
        format!("{op} {}: {err}", crate::paths::display(path)),
    ))
}

pub fn atomic_json(path: &Path, value: &impl Serialize) -> Result<()> {
    let data = serde_json::to_vec(value)?;
    if data.len() as u64 > STATE_LIMIT {
        return Err(Error::Session(
            "state exceeds 32 MiB; no data was overwritten".into(),
        ));
    }
    write_atomic(path, &data, false)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[cfg(windows)]
    #[test]
    fn file_holders_names_the_process_that_has_a_file_open() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("held.json");
        std::fs::write(&path, "{}").unwrap();
        let held = File::open(&path).unwrap();
        let holders = file_holders(&path.canonicalize().unwrap());
        assert!(
            holders.contains(&format!("({})", std::process::id())),
            "{holders}"
        );
        drop(held);
        assert!(file_holders(&path).is_empty());
    }

    #[test]
    fn atomic_json_replaces_a_file_that_a_reader_has_open() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("state.json");
        atomic_json(&path, &1).unwrap();
        let reader = File::open(&path).unwrap();
        atomic_json(&path, &2).unwrap();
        drop(reader);
        assert_eq!(std::fs::read_to_string(&path).unwrap(), "2");
        assert_eq!(std::fs::read_dir(dir.path()).unwrap().count(), 1);
    }

    #[test]
    fn locked_changes_from_many_threads_all_land() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("list.json");
        let threads: Vec<_> = (0..8)
            .map(|n| {
                let path = path.clone();
                std::thread::spawn(move || {
                    with_lock(&path, || -> Result<()> {
                        let mut list: Vec<u32> = if path.exists() {
                            read_json(&path)?
                        } else {
                            Vec::new()
                        };
                        std::thread::sleep(Duration::from_millis(5));
                        list.push(n);
                        atomic_json(&path, &list)
                    })
                    .unwrap();
                })
            })
            .collect();
        for thread in threads {
            thread.join().unwrap();
        }
        let mut list: Vec<u32> = read_json(&path).unwrap();
        list.sort();
        assert_eq!(list, (0..8).collect::<Vec<_>>());
        assert!(dir.path().join("list.json.lock").exists());
    }

    #[test]
    fn a_held_lock_is_busy_until_released() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("sub").join("x.lock");
        let held = try_lock(&path, Duration::ZERO, |_| Error::Session("busy".into())).unwrap();
        let again = try_lock(&path, Duration::from_millis(120), |_| {
            Error::Session("busy".into())
        });
        assert!(matches!(again, Err(Error::Session(text)) if text == "busy"));
        drop(held);
        assert!(try_lock(&path, Duration::ZERO, |_| Error::Session("busy".into())).is_ok());
    }
}
