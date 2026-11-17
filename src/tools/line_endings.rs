//! Line endings, kept per line. The model always works in LF (`read` shows lines without
//! `\r`); a file keeps the ending of every line the model did not change, a changed line
//! inherits the ending of the line it replaces, and only added lines take the file's
//! dominant ending. This is how Codex's `apply_patch` and OpenCode's edit tool behave, and
//! it never rewrites untouched lines (the "one CRLF makes the whole file CRLF" approach
//! of other agents flattens mixed files into a whole-file diff).
//!
//! A new file follows the project's convention: `.editorconfig` `end_of_line`, then the
//! ending Git would check the path out with (`eol`/`text` attributes, `core.autocrlf`,
//! `core.eol`), else LF.

use std::{
    path::Path,
    time::{Duration, Instant},
};

use similar::{Algorithm, DiffOp, capture_diff_slices_deadline};

pub const CRLF: &str = "\r\n";
pub const LF: &str = "\n";

/// `text` with one `\r` removed before each `\n`: the LF view the model works in.
pub fn to_lf(text: &str) -> String {
    text.replace("\r\n", "\n")
}

/// Lines of `text` with their endings (`"\r\n"`, `"\n"`, or `""` for a last line without
/// one). The contents are exactly the lines of [`to_lf`]`(text)`.
fn split_original(text: &str) -> Vec<(&str, &'static str)> {
    let mut lines = Vec::new();
    let mut rest = text;
    while let Some(end) = rest.find('\n') {
        let line = &rest[..end];
        lines.push(match line.strip_suffix('\r') {
            Some(content) => (content, CRLF),
            None => (line, LF),
        });
        rest = &rest[end + 1..];
    }
    if !rest.is_empty() {
        lines.push((rest, ""));
    }
    lines
}

/// Lines of LF text with whether each ends with `\n`.
fn split_lf(text: &str) -> Vec<(&str, bool)> {
    let mut lines: Vec<(&str, bool)> = text
        .split_inclusive('\n')
        .map(|line| match line.strip_suffix('\n') {
            Some(content) => (content, true),
            None => (line, false),
        })
        .collect();
    if text.is_empty() {
        lines.clear();
    }
    lines
}

/// CRLF and LF line ends in `text`.
pub fn counts(text: &str) -> (usize, usize) {
    let crlf = text.matches("\r\n").count();
    (crlf, text.matches('\n').count() - crlf)
}

/// The ending most lines of `text` use; LF when it has none.
pub fn dominant(text: &str) -> &'static str {
    let (crlf, lf) = counts(text);
    if crlf > lf { CRLF } else { LF }
}

/// `edited`, an LF edit of `to_lf(original)`, with `original`'s line endings: unchanged
/// lines keep theirs, a replaced line inherits the one it replaces, added lines get the
/// dominant ending. Lines are aligned with a Myers diff (bounded by a deadline; past it the
/// alignment is coarser, never wrong about which text is written).
pub fn restore(original: &str, edited: &str) -> String {
    let old = split_original(original);
    let new = split_lf(edited);
    let fallback = dominant(original);
    let old_text: Vec<&str> = old.iter().map(|(text, _)| *text).collect();
    let new_text: Vec<&str> = new.iter().map(|(text, _)| *text).collect();
    let deadline = Instant::now() + Duration::from_millis(500);
    let mut endings: Vec<&str> = vec![fallback; new.len()];
    for op in capture_diff_slices_deadline(Algorithm::Myers, &old_text, &new_text, Some(deadline)) {
        match op {
            DiffOp::Equal {
                old_index,
                new_index,
                len,
            } => {
                for k in 0..len {
                    endings[new_index + k] = old[old_index + k].1;
                }
            }
            DiffOp::Replace {
                old_index,
                old_len,
                new_index,
                new_len,
            } => {
                // Within a replaced block, a line edited in place (same text ignoring case
                // and surrounding space) pairs with the line it was; the rest by position.
                let fold = |lines: &[&str]| -> Vec<String> {
                    lines
                        .iter()
                        .map(|line| line.trim().to_lowercase())
                        .collect()
                };
                let before = fold(&old_text[old_index..old_index + old_len]);
                let after = fold(&new_text[new_index..new_index + new_len]);
                for sub in
                    capture_diff_slices_deadline(Algorithm::Myers, &before, &after, Some(deadline))
                {
                    let (from, to, len) = match sub {
                        DiffOp::Equal {
                            old_index: from,
                            new_index: to,
                            len,
                        } => (from, to, len),
                        DiffOp::Replace {
                            old_index: from,
                            old_len,
                            new_index: to,
                            new_len,
                        } => (from, to, old_len.min(new_len)),
                        DiffOp::Insert { .. } | DiffOp::Delete { .. } => continue,
                    };
                    for k in 0..len {
                        endings[new_index + to + k] = old[old_index + from + k].1;
                    }
                }
            }
            DiffOp::Insert { .. } | DiffOp::Delete { .. } => {}
        }
    }
    let mut out = String::with_capacity(edited.len() + new.len());
    for ((text, newline), ending) in new.iter().zip(endings) {
        out.push_str(text);
        if *newline {
            // The old last line had no ending; a line now following it needs one.
            out.push_str(if ending.is_empty() { fallback } else { ending });
        }
    }
    out
}

/// What the model is told about a file's line endings; `None` for plain LF files.
pub fn describe(text: &str) -> Option<String> {
    match counts(text) {
        (0, _) => None,
        (_, 0) => Some(
            "CRLF line endings, kept by edit/write (sed, echo and cat <<EOF in the shell write LF)"
                .into(),
        ),
        (crlf, lf) => Some(format!(
            "mixed line endings ({crlf} CRLF, {lf} LF), each line's kept by edit/write"
        )),
    }
}

/// `content` (LF, as the model writes it) in the line endings a new file at `path`
/// should have; content with `\r` is the model's deliberate choice and stays as written.
pub fn for_new_file(root: &Path, path: &Path, content: &str) -> String {
    if content.contains('\r') || new_file_ending(root, path) == LF {
        return content.to_owned();
    }
    content.replace('\n', CRLF)
}

/// See [`for_new_file`].
pub fn new_file_ending(root: &Path, path: &Path) -> &'static str {
    editorconfig(root, path)
        .or_else(|| git_checkout_ending(root, path))
        .unwrap_or(LF)
}

/// `end_of_line` from the `.editorconfig` files between `root` and `path`: farther files
/// first, nearer ones and later sections override, a file with `root = true` stops the
/// search upwards.
fn editorconfig(root: &Path, path: &Path) -> Option<&'static str> {
    let mut files = Vec::new();
    let mut dir = path.parent();
    while let Some(current) = dir {
        let file = current.join(".editorconfig");
        if let Ok(text) = std::fs::read_to_string(&file) {
            let is_root = text
                .lines()
                .take_while(|line| !line.trim_start().starts_with('['))
                .any(|line| {
                    line.split_once('=').is_some_and(|(key, value)| {
                        key.trim().eq_ignore_ascii_case("root")
                            && value.trim().eq_ignore_ascii_case("true")
                    })
                });
            files.push((current.to_path_buf(), text));
            if is_root {
                break;
            }
        }
        if current == root {
            break;
        }
        dir = current.parent();
    }
    let mut found = None;
    for (dir, text) in files.iter().rev() {
        let Ok(relative) = path.strip_prefix(dir) else {
            continue;
        };
        let relative = relative.to_string_lossy().replace('\\', "/");
        let mut matches = false;
        for line in text.lines().map(str::trim) {
            if line.starts_with('#') || line.starts_with(';') || line.is_empty() {
                continue;
            }
            if let Some(section) = line
                .strip_prefix('[')
                .and_then(|line| line.strip_suffix(']'))
            {
                matches = section_matches(section, &relative);
            } else if matches
                && let Some((key, value)) = line.split_once('=')
                && key.trim().eq_ignore_ascii_case("end_of_line")
            {
                found = match value.trim().to_ascii_lowercase().as_str() {
                    "crlf" => Some(CRLF),
                    "lf" => Some(LF),
                    _ => found,
                };
            }
        }
    }
    found
}

/// EditorConfig globs: without `/` a pattern matches the file name at any depth; with one
/// it is relative to the `.editorconfig` directory. `*` stops at `/`, `**` does not.
fn section_matches(section: &str, relative: &str) -> bool {
    let (anchored, pattern) = match section.strip_prefix('/') {
        Some(rest) => (true, rest),
        None => (section.contains('/'), section),
    };
    let mut regex = String::from(if anchored { "^" } else { "^(?:.*/)?" });
    let mut chars = pattern.chars().peekable();
    let mut braces = 0usize;
    while let Some(c) = chars.next() {
        match c {
            '*' if chars.peek() == Some(&'*') => {
                chars.next();
                regex.push_str(".*");
            }
            '*' => regex.push_str("[^/]*"),
            '?' => regex.push_str("[^/]"),
            '[' => {
                let mut class = String::from("[");
                if chars.peek() == Some(&'!') {
                    chars.next();
                    class.push('^');
                }
                let mut closed = false;
                for c in chars.by_ref() {
                    if c == ']' {
                        closed = true;
                        break;
                    }
                    if c == '\\' || c == '[' || c == '^' {
                        class.push('\\');
                    }
                    class.push(c);
                }
                if closed {
                    regex.push_str(&class);
                    regex.push(']');
                } else {
                    regex.push_str(&regex::escape(&class));
                }
            }
            '{' => {
                braces += 1;
                regex.push_str("(?:");
            }
            ',' if braces > 0 => regex.push('|'),
            '}' if braces > 0 => {
                braces -= 1;
                regex.push(')');
            }
            '\\' => {
                if let Some(next) = chars.next() {
                    regex.push_str(&regex::escape(&next.to_string()));
                }
            }
            other => regex.push_str(&regex::escape(&other.to_string())),
        }
    }
    if braces > 0 {
        return false;
    }
    regex.push('$');
    regex::Regex::new(&regex).is_ok_and(|regex| regex.is_match(relative))
}

/// The ending Git would check `path` out with in the project at `root`; `None` outside
/// a Git work tree.
fn git_checkout_ending(root: &Path, path: &Path) -> Option<&'static str> {
    let relative = path
        .strip_prefix(root)
        .ok()?
        .to_string_lossy()
        .replace('\\', "/");
    let git = |args: &[&str]| -> Option<String> {
        let output = std::process::Command::new("git")
            .args(args)
            .current_dir(root)
            .env_remove("GIT_DIR")
            .env_remove("GIT_WORK_TREE")
            .env_remove("GIT_INDEX_FILE")
            .stdin(std::process::Stdio::null())
            .output()
            .ok()?;
        output
            .status
            .success()
            .then(|| String::from_utf8_lossy(&output.stdout).into_owned())
    };
    let attributes = git(&["check-attr", "text", "eol", "--", &relative])?;
    let attribute = |name: &str| {
        attributes.lines().find_map(|line| {
            let (_, rest) = line.rsplit_once(&format!(": {name}: "))?;
            Some(rest.trim().to_owned())
        })
    };
    let (text, eol) = (attribute("text"), attribute("eol"));
    match eol.as_deref() {
        Some("crlf") => return Some(CRLF),
        Some("lf") => return Some(LF),
        _ => {}
    }
    if text.as_deref() == Some("unset") {
        return Some(LF);
    }
    let config = |key: &str| {
        git(&["config", "--get", key])
            .map(|value| value.trim().to_ascii_lowercase())
            .unwrap_or_default()
    };
    if config("core.autocrlf") == "true" {
        return Some(CRLF);
    }
    if matches!(text.as_deref(), Some("set" | "auto")) && config("core.eol") == "crlf" {
        return Some(CRLF);
    }
    Some(LF)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Applies an LF edit to `original`'s LF view and restores the endings.
    fn edit(original: &str, old: &str, new: &str) -> String {
        let view = to_lf(original);
        assert!(view.contains(old), "{old:?} not in {view:?}");
        restore(original, &view.replacen(old, new, 1))
    }

    #[test]
    fn uniform_files_keep_their_style() {
        assert_eq!(
            edit("a\r\nb\r\nc\r\n", "b", "B\nB2"),
            "a\r\nB\r\nB2\r\nc\r\n"
        );
        assert_eq!(edit("a\nb\nc\n", "b", "B\nB2"), "a\nB\nB2\nc\n");
    }

    #[test]
    fn mixed_files_keep_every_untouched_line_and_changed_lines_inherit() {
        // OpenCode's case: kept lines keep theirs, changed ones inherit, extras dominate.
        assert_eq!(
            edit(
                "one\r\ntwo\r\nthree\nfour\r\n",
                "one\ntwo\nthree\nfour",
                "ONE\nUNO\ntwO\nTHREE\nFOur"
            ),
            "ONE\r\nUNO\r\ntwO\r\nTHREE\nFOur\r\n"
        );
        assert_eq!(
            edit("a\r\nb\nc\r\nd\r\n", "b\nc", "X\nY"),
            "a\r\nX\nY\r\nd\r\n"
        );
        assert_eq!(edit("a\nb\r\nc\nd\n", "b\nc", "X\nY"), "a\nX\r\nY\nd\n");
    }

    #[test]
    fn last_line_without_newline_and_lone_cr_survive() {
        assert_eq!(edit("a\r\nb", "b", "b\nc"), "a\r\nb\r\nc");
        assert_eq!(edit("a\r\nb\r\n", "b\n", "b"), "a\r\nb");
        assert_eq!(edit("x\ry\rz\r", "y", "Y"), "x\rY\rz\r");
        // "\r\r\n" keeps the stray `\r` as text.
        assert_eq!(edit("a\r\r\nb\r\n", "b", "B"), "a\r\r\nB\r\n");
        assert_eq!(restore("", "new\nfile\n"), "new\nfile\n");
    }

    #[test]
    fn whole_file_rewrites_only_touch_changed_lines() {
        let original = "keep\r\nchange\nkeep too\r\n";
        assert_eq!(
            restore(original, "keep\nchanged\nkeep too\nadded\n"),
            "keep\r\nchanged\nkeep too\r\nadded\r\n"
        );
    }

    #[test]
    fn descriptions_and_counts() {
        assert_eq!(describe("a\nb\n"), None);
        assert!(
            describe("a\r\nb\r\n")
                .unwrap()
                .starts_with("CRLF line endings")
        );
        assert!(
            describe("a\r\nb\n")
                .unwrap()
                .starts_with("mixed line endings (1 CRLF, 1 LF)")
        );
        assert_eq!(dominant(""), LF);
        assert_eq!(to_lf("q\r\nr\r\r\n"), "q\nr\r\n");
    }

    #[test]
    fn new_files_follow_editorconfig_then_git_then_lf() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        // No Git, no .editorconfig: LF.
        assert_eq!(new_file_ending(root, &root.join("a.cs")), LF);
        assert_eq!(for_new_file(root, &root.join("a.cs"), "x\ny\n"), "x\ny\n");
        std::fs::write(
            root.join(".editorconfig"),
            "root = true\n[*]\nend_of_line = lf\n[*.{cs,csproj}]\nend_of_line = crlf\n[docs/**.md]\nend_of_line = crlf\n",
        )
        .unwrap();
        std::fs::create_dir_all(root.join("src/deep")).unwrap();
        assert_eq!(new_file_ending(root, &root.join("src/deep/a.cs")), CRLF);
        assert_eq!(new_file_ending(root, &root.join("src/a.rs")), LF);
        assert_eq!(new_file_ending(root, &root.join("docs/x/y.md")), CRLF);
        assert_eq!(new_file_ending(root, &root.join("y.md")), LF);
        // A nearer file overrides.
        std::fs::write(root.join("src/.editorconfig"), "[*.cs]\nend_of_line = lf\n").unwrap();
        assert_eq!(new_file_ending(root, &root.join("src/deep/a.cs")), LF);
        // Deliberate `\r` stays as written.
        assert_eq!(
            for_new_file(root, &root.join("docs/a/b.md"), "a\r\nb\n"),
            "a\r\nb\n"
        );
        assert_eq!(
            for_new_file(root, &root.join("docs/a/b.md"), "a\nb\n"),
            "a\r\nb\r\n"
        );
    }

    fn git_available() -> bool {
        std::process::Command::new("git")
            .arg("--version")
            .output()
            .is_ok_and(|output| output.status.success())
    }

    fn git(root: &Path, args: &[&str]) {
        let status = std::process::Command::new("git")
            .args(args)
            .current_dir(root)
            .env_remove("GIT_DIR")
            .env_remove("GIT_WORK_TREE")
            .status()
            .unwrap();
        assert!(status.success());
    }

    #[test]
    fn new_files_without_editorconfig_follow_how_git_checks_them_out() {
        if !git_available() {
            return;
        }
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        git(root, &["init", "-q"]);
        git(root, &["config", "core.autocrlf", "false"]);
        git(root, &["config", "core.eol", "lf"]);
        assert_eq!(new_file_ending(root, &root.join("a.txt")), LF);
        git(root, &["config", "core.autocrlf", "true"]);
        assert_eq!(new_file_ending(root, &root.join("a.txt")), CRLF);
        std::fs::write(
            root.join(".gitattributes"),
            "*.sh text eol=lf\n*.bat text eol=crlf\n*.bin -text\n",
        )
        .unwrap();
        assert_eq!(new_file_ending(root, &root.join("run.sh")), LF);
        assert_eq!(new_file_ending(root, &root.join("run.bat")), CRLF);
        assert_eq!(new_file_ending(root, &root.join("x.bin")), LF);
        git(root, &["config", "core.autocrlf", "false"]);
        assert_eq!(new_file_ending(root, &root.join("run.bat")), CRLF);
        assert_eq!(new_file_ending(root, &root.join("a.txt")), LF);
    }
}
