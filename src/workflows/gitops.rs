//! Git for `/diff git` and `/commit`: read-only listings, and a commit that takes
//! only staged content unless `--all`/`--only` select files through a temporary
//! index built from HEAD (the user's own index is never rewritten by the selection).

use std::path::{Component, Path, PathBuf};

use tokio::process::Command;

use crate::domain::{Error, Result};

const DIFF_LIMIT: usize = 256 * 1024;

pub struct Git {
    pub root: PathBuf,
    /// Temporary index (`GIT_INDEX_FILE`) for `--all`/`--only`.
    index: Option<PathBuf>,
}

/// Runs git in `dir`; a failure carries git's own message.
async fn git_in(dir: &Path, index: Option<&Path>, args: &[&str]) -> Result<String> {
    let mut command = Command::new("git");
    command
        .arg("--literal-pathspecs")
        .args(args)
        .current_dir(dir)
        .env("GIT_TERMINAL_PROMPT", "0")
        .stdin(std::process::Stdio::null())
        .kill_on_drop(true);
    if let Some(index) = index {
        command.env("GIT_INDEX_FILE", index);
    }
    let output = command
        .output()
        .await
        .map_err(|err| Error::Tool(format!("找不到 git（{err}）；请安装 git 并加入 PATH")))?;
    let stdout = String::from_utf8_lossy(&output.stdout).into_owned();
    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        let detail = if stderr.trim().is_empty() {
            stdout.trim()
        } else {
            stderr.trim()
        };
        return Err(Error::Tool(format!(
            "git {} 失败：{}",
            args.first().copied().unwrap_or_default(),
            crate::app::diagnostics::clip(detail, 2000)
        )));
    }
    Ok(stdout)
}

fn bounded(mut text: String) -> String {
    if text.len() > DIFF_LIMIT {
        text.truncate(text.floor_char_boundary(DIFF_LIMIT));
        text.push_str("\n[diff 超过 256 KiB，已截断]\n");
    }
    text
}

/// `/diff git` (full patches) or `/diff git --stat` (file lists, untracked included).
pub async fn diff(cwd: &Path, stat: bool) -> Result<String> {
    let sections: &[(&str, &[&str])] = if stat {
        &[
            ("Staged", &["diff", "--cached", "--stat"]),
            ("Unstaged", &["diff", "--stat"]),
            ("Untracked", &["ls-files", "--others", "--exclude-standard"]),
        ]
    } else {
        &[
            ("Staged (git diff --cached)", &["diff", "--cached"]),
            ("Unstaged (git diff)", &["diff"]),
        ]
    };
    let mut out = String::new();
    for (title, args) in sections {
        let text = git_in(cwd, None, args).await?;
        if !text.trim().is_empty() {
            out.push_str(&format!("# {title}\n{}\n", text.trim_end()));
        }
    }
    if out.is_empty() {
        out.push_str("没有 Git 改动。\n");
    }
    Ok(bounded(out))
}

#[derive(Debug, PartialEq, Eq)]
pub enum Scope {
    Staged,
    All,
    Only(Vec<String>),
}

#[derive(Debug, PartialEq, Eq)]
pub struct CommitArgs {
    pub preview: bool,
    pub scope: Scope,
    pub message: String,
}

pub const COMMIT_USAGE: &str = "/commit [--preview] [--all | --only <paths>... --] [message]";

/// Parses the words after `/commit`.
pub fn parse_commit(args: &[&str]) -> std::result::Result<CommitArgs, String> {
    let mut rest = args;
    let preview = rest.first() == Some(&"--preview");
    if preview {
        rest = &rest[1..];
    }
    let scope = match rest.first().copied() {
        Some("--all") => {
            rest = &rest[1..];
            Scope::All
        }
        Some("--only") => {
            let end = rest
                .iter()
                .position(|arg| *arg == "--")
                .filter(|end| *end >= 2)
                .ok_or_else(|| format!("用法：{COMMIT_USAGE}"))?;
            let paths = rest[1..end].iter().map(|path| (*path).to_owned()).collect();
            rest = &rest[end + 1..];
            Scope::Only(paths)
        }
        _ => Scope::Staged,
    };
    if let Some(flag) = rest.first().filter(|arg| arg.starts_with("--")) {
        return Err(format!("未知的 /commit 选项 {flag}；用法：{COMMIT_USAGE}"));
    }
    let message = rest.join(" ");
    let message = message
        .strip_prefix('"')
        .and_then(|inner| inner.strip_suffix('"'))
        .unwrap_or(&message)
        .trim()
        .to_owned();
    Ok(CommitArgs {
        preview,
        scope,
        message: if message.is_empty() {
            "auto-commit".into()
        } else {
            message
        },
    })
}

/// `base/relative` without `.`/`..`, lexically (the path may be deleted).
fn normalize(base: &Path, relative: &Path) -> PathBuf {
    let mut out = PathBuf::new();
    for part in base.join(relative).components() {
        match part {
            Component::ParentDir => {
                out.pop();
            }
            Component::CurDir => {}
            other => out.push(other),
        }
    }
    out
}

impl Git {
    /// The repository containing `cwd`.
    pub async fn open(cwd: &Path) -> Result<Self> {
        let root = git_in(cwd, None, &["rev-parse", "--show-toplevel"]).await?;
        let root = PathBuf::from(root.trim()).canonicalize()?;
        Ok(Self { root, index: None })
    }

    pub async fn run(&self, args: &[&str]) -> Result<String> {
        git_in(&self.root, self.index.as_deref(), args).await
    }

    /// Repository-relative, `/`-separated paths for `--only`; each must stay inside.
    pub fn repo_paths(&self, cwd: &Path, paths: &[String]) -> Result<Vec<String>> {
        paths
            .iter()
            .map(|path| {
                let absolute = normalize(cwd, Path::new(path));
                let relative = absolute
                    .strip_prefix(&self.root)
                    .ok()
                    .filter(|relative| !path.is_empty() && !relative.as_os_str().is_empty())
                    .ok_or_else(|| Error::Tool(format!("提交的路径必须在仓库内：{path}")))?;
                Ok(relative.to_string_lossy().replace('\\', "/"))
            })
            .collect()
    }

    /// Builds the temporary index from HEAD (empty before the first commit) plus `paths`
    /// (everything when `None`). The returned directory owns the index file.
    pub async fn select(&mut self, paths: Option<&[String]>) -> Result<tempfile::TempDir> {
        let dir = tempfile::Builder::new()
            .prefix("cove-commit-index-")
            .tempdir()?;
        self.index = Some(dir.path().join("index"));
        let tree = if self
            .run(&["rev-parse", "--verify", "-q", "HEAD"])
            .await
            .is_ok()
        {
            "HEAD"
        } else {
            "--empty"
        };
        self.run(&["read-tree", tree]).await?;
        let mut add = vec!["add", "-A"];
        if let Some(paths) = paths {
            add.push("--");
            add.extend(paths.iter().map(String::as_str));
        }
        self.run(&add).await?;
        Ok(dir)
    }

    pub fn uses_temporary_index(&self) -> bool {
        self.index.is_some()
    }

    /// Paths of the selected index content.
    pub async fn staged_files(&self) -> Result<Vec<String>> {
        Ok(self
            .run(&["diff", "--cached", "--name-only", "-z"])
            .await?
            .split('\0')
            .filter(|name| !name.is_empty())
            .map(str::to_owned)
            .collect())
    }

    /// Selected files whose working-tree content differs from what would be committed.
    pub async fn unstaged_among(&self, files: &[String]) -> Result<String> {
        let mut args = vec!["diff", "--name-only", "--"];
        args.extend(files.iter().map(String::as_str));
        Ok(self.run(&args).await?.trim().to_owned())
    }

    /// After a commit through the temporary index, the user's index learns the
    /// committed content of those files (other staged entries are kept).
    pub async fn sync_index(&self) -> Result<()> {
        let committed = git_in(
            &self.root,
            None,
            &[
                "diff-tree",
                "--root",
                "--no-commit-id",
                "--name-only",
                "-r",
                "-z",
                "HEAD",
            ],
        )
        .await?;
        let names: Vec<&str> = committed.split('\0').filter(|n| !n.is_empty()).collect();
        if names.is_empty() {
            return Ok(());
        }
        let mut args = vec!["reset", "-q", "HEAD", "--"];
        args.extend(names);
        git_in(&self.root, None, &args).await.map(drop)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn commit_arguments() {
        assert_eq!(
            parse_commit(&[]).unwrap(),
            CommitArgs {
                preview: false,
                scope: Scope::Staged,
                message: "auto-commit".into()
            }
        );
        assert_eq!(
            parse_commit(&["--preview", "--only", "a.rs", "b.rs", "--", "\"fix", "it\""]).unwrap(),
            CommitArgs {
                preview: true,
                scope: Scope::Only(vec!["a.rs".into(), "b.rs".into()]),
                message: "fix it".into()
            }
        );
        assert_eq!(parse_commit(&["--all", "m"]).unwrap().scope, Scope::All);
        assert!(parse_commit(&["--only", "--"]).is_err());
        assert!(parse_commit(&["--push"]).is_err());
    }

    #[test]
    fn paths_must_stay_in_the_repository() {
        let root = std::env::temp_dir().canonicalize().unwrap();
        let git = Git {
            root: root.clone(),
            index: None,
        };
        let sub = root.join("sub");
        assert_eq!(
            git.repo_paths(&sub, &["../a/./b.rs".into()]).unwrap(),
            ["a/b.rs"]
        );
        assert!(git.repo_paths(&root, &["../outside".into()]).is_err());
        assert!(git.repo_paths(&root, &[".".into()]).is_err());
    }
}
