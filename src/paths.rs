//! Paths as Windows' `canonicalize` returns them (`\\?\C:\…`, `\\?\UNC\…`),
//! turned into what git accepts, what users read, and what compares equal.

use std::path::{Path, PathBuf};

const VERBATIM: &str = r"\\?\";

/// Without the verbatim prefix of a drive path (`\\?\C:\work` → `C:\work`);
/// a verbatim UNC path keeps it, as git cannot take it otherwise.
pub fn plain(path: &Path) -> PathBuf {
    let text = path.to_string_lossy();
    match text.strip_prefix(VERBATIM) {
        Some(rest) if rest.as_bytes().get(1) == Some(&b':') => PathBuf::from(rest),
        _ => path.to_path_buf(),
    }
}

/// A path as users write it: `C:\work`, and `\\server\share` for UNC.
pub fn display(path: &Path) -> String {
    let text = path.to_string_lossy();
    match text.strip_prefix(VERBATIM) {
        Some(rest) => match rest.strip_prefix(r"UNC\") {
            Some(share) => format!(r"\\{share}"),
            None => rest.to_owned(),
        },
        None => text.into_owned(),
    }
}

/// A comparable form: [`display`] with `/` separators and no trailing one (so
/// the root is empty), lower case on Windows.
pub fn key(path: &Path) -> String {
    let text = display(path).replace('\\', "/");
    let text = text.trim_end_matches('/');
    if cfg!(windows) {
        text.to_lowercase()
    } else {
        text.to_owned()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn verbatim_prefixes() {
        assert_eq!(plain(Path::new(r"\\?\C:\work")), PathBuf::from(r"C:\work"));
        assert_eq!(
            plain(Path::new(r"\\?\UNC\server\share")),
            PathBuf::from(r"\\?\UNC\server\share")
        );
        assert_eq!(
            plain(Path::new("src/main.rs")),
            PathBuf::from("src/main.rs")
        );
        assert_eq!(display(Path::new(r"\\?\C:\work")), r"C:\work");
        assert_eq!(
            display(Path::new(r"\\?\UNC\server\share\x")),
            r"\\server\share\x"
        );
        assert_eq!(display(Path::new("/home/me")), "/home/me");
    }

    #[test]
    fn keys_compare_spellings_of_one_path() {
        assert_eq!(key(Path::new(r"\\?\C:\Work\")), key(Path::new("C:/Work")));
        assert_eq!(key(Path::new("/")), "");
        if cfg!(windows) {
            assert_eq!(key(Path::new(r"C:\Work")), "c:/work");
        }
    }
}
