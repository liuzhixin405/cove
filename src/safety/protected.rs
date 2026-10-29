//! Directories no tool may read, list or write, however wide the workspace:
//! cove's own data directory (configuration with API keys, sessions,
//! permission rules) when it lies inside the workspace, as it does when cove
//! starts in the home directory.

use std::{
    path::{Path, PathBuf},
    sync::RwLock,
};

static PROTECTED: RwLock<Vec<String>> = RwLock::new(Vec::new());

/// A comparable form: no `\\?\` prefix, `/` separators, lower case on Windows.
fn key(path: &Path) -> String {
    let text = path.to_string_lossy();
    let text = text
        .strip_prefix(r"\\?\")
        .unwrap_or(&text)
        .replace('\\', "/");
    let text = text.trim_end_matches('/').to_owned();
    if cfg!(windows) {
        text.to_lowercase()
    } else {
        text
    }
}

/// Keeps every tool out of `dir` from now on.
pub fn protect(dir: &Path) {
    let dir = key(&dir.canonicalize().unwrap_or_else(|_| dir.to_owned()));
    if let Ok(mut protected) = PROTECTED.write()
        && !protected.contains(&dir)
    {
        protected.push(dir);
    }
}

/// Whether `path` is a protected directory or lies below one. `path` should be
/// absolute; the walkers and the path resolver pass canonical paths.
pub fn is_protected(path: &Path) -> bool {
    let path = key(path);
    PROTECTED.read().is_ok_and(|protected| {
        protected
            .iter()
            .any(|dir| path == *dir || path.starts_with(&format!("{dir}/")))
    })
}

/// The protected directories below `root`, relative with `/` separators (for the
/// exclude lists of ripgrep and the snapshot repository).
pub fn below(root: &Path) -> Vec<String> {
    let root = key(&root.canonicalize().unwrap_or_else(|_| root.to_owned()));
    PROTECTED
        .read()
        .map(|protected| {
            protected
                .iter()
                .filter_map(|dir| dir.strip_prefix(&format!("{root}/")).map(str::to_owned))
                .collect()
        })
        .unwrap_or_default()
}

/// Why `dir` is too wide for a workspace: the home directory or a filesystem root.
pub fn too_wide(dir: &Path) -> Option<&'static str> {
    let dir = dir.canonicalize().unwrap_or_else(|_| dir.to_owned());
    if dir.parent().is_none() || key(&dir).ends_with(':') {
        return Some("a filesystem root");
    }
    let home: Option<PathBuf> = crate::extensions::skills::home_dir();
    home.filter(|home| key(&home.canonicalize().unwrap_or_else(|_| home.clone())) == key(&dir))
        .map(|_| "your home directory")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn protected_directories_and_their_contents_are_recognized() {
        let root = tempfile::tempdir().unwrap();
        let data = root.path().join(".cove");
        std::fs::create_dir_all(data.join("sessions")).unwrap();
        protect(&data);
        let canonical = data.canonicalize().unwrap();
        assert!(is_protected(&canonical));
        assert!(is_protected(&canonical.join("config.json")));
        assert!(!is_protected(
            &root.path().canonicalize().unwrap().join(".cove-other")
        ));
        assert_eq!(below(root.path()), [".cove"]);
        assert!(too_wide(Path::new("/")).is_some());
        assert!(too_wide(root.path()).is_none());
    }
}
