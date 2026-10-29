//! Which version of each file the model has seen, so `write` cannot replace a file it never
//! read or that changed on disk since (user edits, formatters, shell commands).
//!
//! Any successful read counts, partial ones too: the snapshot always covers the whole file,
//! and a 2000-line window must not make big files unwritable. Content (size + SHA-256)
//! rather than mtime: identical rewrites stay valid, sub-second changes are caught.

use std::{
    collections::HashMap,
    path::{Path, PathBuf},
    sync::Mutex,
};

use sha2::{Digest, Sha256};

#[derive(Clone, Copy, PartialEq, Eq)]
struct Snapshot {
    size: u64,
    sum: [u8; 32],
}

impl Snapshot {
    fn of(bytes: &[u8]) -> Self {
        Self {
            size: bytes.len() as u64,
            sum: Sha256::digest(bytes).into(),
        }
    }
}

/// Shared by every `LocalTools` view of one session (including read-only sub-agents).
#[derive(Default)]
pub struct FileTracker {
    seen: Mutex<HashMap<PathBuf, Snapshot>>,
}

impl FileTracker {
    /// `path` is canonical (as `LocalTools::resolve` returns it).
    pub fn record(&self, path: &Path, bytes: &[u8]) {
        if let Ok(mut seen) = self.seen.lock() {
            seen.insert(path.to_path_buf(), Snapshot::of(bytes));
        }
    }

    /// `Err` (worded for the model) unless `current` is the version last read or written.
    pub fn check(&self, path: &Path, shown: &str, current: &[u8]) -> Result<(), String> {
        let known = self
            .seen
            .lock()
            .map_err(|_| "file tracker lock poisoned".to_owned())?
            .get(path)
            .copied();
        match known {
            None => Err(format!(
                "{shown} exists and has not been read in this session; read it first so the change is based on its current content"
            )),
            Some(snapshot) if snapshot != Snapshot::of(current) => Err(format!(
                "{shown} has changed since you last read it (edited by the user or a command); read it again so those changes are not lost"
            )),
            Some(_) => Ok(()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn writes_need_the_current_version() {
        let tracker = FileTracker::default();
        let path = Path::new("/w/a.txt");
        assert!(
            tracker
                .check(path, "a.txt", b"x")
                .unwrap_err()
                .contains("read it first")
        );
        tracker.record(path, b"x");
        assert!(tracker.check(path, "a.txt", b"x").is_ok());
        assert!(
            tracker
                .check(path, "a.txt", b"y")
                .unwrap_err()
                .contains("changed")
        );
    }
}
