//! Git worktrees: helpers shared by the `worktree`/`exit_worktree` tools and the
//! isolated background agents.
//!
//! Design: the parent's file and shell tools are never re-rooted. The parent's checkpoint,
//! verification gate, `/undo` and project context all describe the main tree, and switching
//! the root mid-task would split one task across two trees. Instead, work inside a worktree
//! runs as a sub-agent whose tools are rooted there (`agent` with `worktree=<branch>`), with
//! its own checkpoint journal; the branch is the rollback unit.

use std::{
    path::{Path, PathBuf},
    process::Stdio,
    sync::Arc,
    time::Duration,
};

use serde::Deserialize;
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Result, ToolDefinition},
    safety::permission::{Authorizer, Effect},
    tools::ToolOutput,
};

const GIT_OUTPUT_LIMIT: usize = 64 * 1024;

pub struct GitOutput {
    pub ok: bool,
    pub stdout: String,
    pub stderr: String,
}

impl GitOutput {
    pub fn text(&self) -> String {
        format!("{}{}", self.stdout, self.stderr).trim().to_owned()
    }
}

/// Runs git without a shell. `core.fsmonitor` is disabled so a repository config cannot
/// run a program through read-only queries; mutating calls are authorized by the caller.
pub async fn git(dir: &Path, args: &[&str], cancel: &CancellationToken) -> Result<GitOutput> {
    if cancel.is_cancelled() {
        return Err(Error::Cancelled);
    }
    let mut command = tokio::process::Command::new("git");
    command
        .args(["-c", "core.fsmonitor=false", "-c", "core.quotepath=false"])
        .args(args)
        .current_dir(dir)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .env("GIT_TERMINAL_PROMPT", "0")
        .kill_on_drop(true);
    let child = command
        .spawn()
        .map_err(|err| Error::Tool(format!("cannot run git: {err}")))?;
    let output = tokio::select! {
        _ = cancel.cancelled() => return Err(Error::Cancelled),
        _ = tokio::time::sleep(Duration::from_secs(120)) => {
            return Err(Error::Tool(format!("git {} timed out", args.join(" "))))
        }
        output = child.wait_with_output() => output?,
    };
    let clip = |bytes: &[u8]| {
        let text = String::from_utf8_lossy(bytes);
        let mut text = text.into_owned();
        if text.len() > GIT_OUTPUT_LIMIT {
            text.truncate(text.floor_char_boundary(GIT_OUTPUT_LIMIT));
            text.push_str("\n[truncated]");
        }
        text
    };
    Ok(GitOutput {
        ok: output.status.success(),
        stdout: clip(&output.stdout),
        stderr: clip(&output.stderr),
    })
}

async fn git_ok(dir: &Path, args: &[&str], cancel: &CancellationToken) -> Result<String> {
    let output = git(dir, args, cancel).await?;
    if !output.ok {
        return Err(Error::Tool(format!(
            "git {} failed: {}",
            args.first().copied().unwrap_or_default(),
            output.text()
        )));
    }
    Ok(output.stdout.trim().to_owned())
}

pub async fn head(dir: &Path, cancel: &CancellationToken) -> Result<String> {
    git_ok(dir, &["rev-parse", "HEAD"], cancel).await
}

/// `git status --porcelain` lines (untracked files included); empty when clean.
pub async fn status(dir: &Path, cancel: &CancellationToken) -> Result<String> {
    git_ok(
        dir,
        &["status", "--porcelain", "--untracked-files=all"],
        cancel,
    )
    .await
}

pub async fn commits_since(
    dir: &Path,
    base: &str,
    branch: &str,
    cancel: &CancellationToken,
) -> Result<u64> {
    let range = format!("{base}..{branch}");
    git_ok(dir, &["rev-list", "--count", &range], cancel)
        .await?
        .parse()
        .map_err(|_| Error::Tool("unexpected git rev-list output".into()))
}

/// Branch names git would accept and that cannot act as options or walk out of a directory.
pub fn validate_branch(branch: &str) -> Result<()> {
    let bad = branch.is_empty()
        || branch.len() > 100
        || branch.starts_with(['-', '/', '.'])
        || branch.ends_with(['/', '.'])
        || branch.ends_with(".lock")
        || branch.contains("..")
        || branch.contains("//")
        || branch.contains("/.")
        || !branch
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.' | '/'));
    if bad {
        return Err(Error::Tool(format!(
            "invalid branch name {branch:?}: use letters, digits, '-', '_', '.', '/' without '..' or a leading '-'"
        )));
    }
    Ok(())
}

fn same_path(a: &Path, b: &Path) -> bool {
    match (a.canonicalize(), b.canonicalize()) {
        (Ok(a), Ok(b)) => a == b,
        _ => false,
    }
}

/// `path` as git accepts it: without the Windows verbatim `\\?\` prefix of canonical paths.
pub fn plain(path: &Path) -> String {
    let text = path.to_string_lossy();
    match text.strip_prefix(r"\\?\") {
        Some(rest) if !rest.starts_with("UNC\\") => rest.to_owned(),
        _ => text.into_owned(),
    }
}

/// Whether `path` is a worktree git has registered for `project`.
pub async fn registered(project: &Path, path: &Path, cancel: &CancellationToken) -> Result<bool> {
    let list = git_ok(project, &["worktree", "list", "--porcelain"], cancel).await?;
    Ok(list
        .lines()
        .filter_map(|line| line.strip_prefix("worktree "))
        .any(|listed| same_path(Path::new(listed), path)))
}

/// Uses the repository's identity when configured, else a local placeholder; `--no-verify`
/// keeps repository hooks from running inside automatic commits.
async fn commit_args(dir: &Path, cancel: &CancellationToken) -> Vec<String> {
    let configured = git(dir, &["config", "user.email"], cancel)
        .await
        .is_ok_and(|output| output.ok && !output.stdout.trim().is_empty());
    if configured {
        Vec::new()
    } else {
        [
            "-c",
            "user.name=cove",
            "-c",
            "user.email=cove@localhost",
        ]
        .map(str::to_owned)
        .to_vec()
    }
}

/// Commits every change in `dir`; `false` when there was nothing to commit.
pub async fn commit_all(dir: &Path, message: &str, cancel: &CancellationToken) -> Result<bool> {
    if status(dir, cancel).await?.is_empty() {
        return Ok(false);
    }
    git_ok(dir, &["add", "-A"], cancel).await?;
    let mut args = commit_args(dir, cancel).await;
    args.extend(["commit", "--no-verify", "-q", "-m", message].map(str::to_owned));
    let args: Vec<&str> = args.iter().map(String::as_str).collect();
    git_ok(dir, &args, cancel).await?;
    Ok(true)
}

pub async fn add_worktree(
    project: &Path,
    path: &Path,
    branch: &str,
    base: &str,
    cancel: &CancellationToken,
) -> Result<String> {
    validate_branch(branch)?;
    let target = plain(path);
    git_ok(
        project,
        &["worktree", "add", "-b", branch, &target, base],
        cancel,
    )
    .await
}

pub async fn remove_worktree(
    project: &Path,
    path: &Path,
    force: bool,
    cancel: &CancellationToken,
) -> Result<()> {
    let target = plain(path);
    let mut args = vec!["worktree", "remove"];
    if force {
        args.push("--force");
    }
    args.push(&target);
    git_ok(project, &args, cancel).await.map(|_| ())
}

pub async fn delete_branch(
    project: &Path,
    branch: &str,
    force: bool,
    cancel: &CancellationToken,
) -> Result<()> {
    validate_branch(branch)?;
    git_ok(
        project,
        &["branch", if force { "-D" } else { "-d" }, branch],
        cancel,
    )
    .await
    .map(|_| ())
}

/// Merges `branch` into the checked-out branch of `project`. Requires a clean tree;
/// fast-forwards when `project` is still at `base`. Conflicts are reported and the merge is
/// aborted, never resolved automatically. `Ok(Err(report))` is a refused or conflicting merge.
pub async fn merge_branch(
    project: &Path,
    branch: &str,
    base: Option<&str>,
    cancel: &CancellationToken,
) -> Result<std::result::Result<String, String>> {
    validate_branch(branch)?;
    let dirty = status(project, cancel).await?;
    if !dirty.is_empty() {
        return Ok(Err(format!(
            "the working tree has uncommitted changes; commit or stash them before merging:\n{dirty}"
        )));
    }
    let current = head(project, cancel).await?;
    let mut note = String::new();
    let output = if base == Some(current.as_str()) {
        git(project, &["merge", "--ff-only", branch], cancel).await?
    } else {
        if let Some(base) = base {
            note = format!(
                "baseline moved ({base:.10} -> {current:.10}); merged with a merge commit. "
            );
        }
        let mut args = commit_args(project, cancel).await;
        args.extend(["merge", "--no-ff", "--no-edit", branch].map(str::to_owned));
        let args: Vec<&str> = args.iter().map(String::as_str).collect();
        git(project, &args, cancel).await?
    };
    if output.ok {
        return Ok(Ok(format!("{note}{}", output.text())));
    }
    let conflicts = git(project, &["diff", "--name-only", "--diff-filter=U"], cancel)
        .await
        .map(|output| output.stdout)
        .unwrap_or_default();
    let aborted = git(project, &["merge", "--abort"], cancel)
        .await
        .is_ok_and(|output| output.ok);
    Ok(Err(format!(
        "merge of {branch} failed and was {}: {}\nconflicting files:\n{}",
        if aborted {
            "aborted (tree unchanged)"
        } else {
            "NOT aborted; inspect git status"
        },
        output.text(),
        if conflicts.trim().is_empty() {
            "(none reported)"
        } else {
            conflicts.trim()
        }
    )))
}

/// The `worktree` and `exit_worktree` tools.
pub struct Worktrees {
    pub project: PathBuf,
    pub authorizer: Arc<dyn Authorizer>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct EnterArgs {
    branch: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ExitArgs {
    branch: String,
    #[serde(default)]
    merge: bool,
    #[serde(default)]
    discard: bool,
}

pub fn definitions() -> Vec<ToolDefinition> {
    vec![
        ToolDefinition {
            name: "worktree".into(),
            description: "Create a git worktree ../<branch> on a new branch for isolated work (requires approval). Your own tools stay in the main tree; \
                          run the work inside it with agent(worktree=<branch>), then exit_worktree."
                .into(),
            schema: json!({"type":"object","properties":{"branch":{"type":"string"}},"required":["branch"],"additionalProperties":false}),
        },
        ToolDefinition {
            name: "exit_worktree".into(),
            description: "Remove the worktree of <branch> (requires approval). merge=true first merges its committed work into the main tree \
                          (both trees must be clean; conflicts abort the merge and are reported). A worktree with uncommitted changes is only \
                          removed with discard=true. The branch is kept unless it was merged."
                .into(),
            schema: json!({"type":"object","properties":{"branch":{"type":"string"},"merge":{"type":"boolean"},"discard":{"type":"boolean"}},
                "required":["branch"],"additionalProperties":false}),
        },
    ]
}

fn failed(content: impl Into<String>) -> Result<ToolOutput> {
    Ok(ToolOutput {
        content: content.into(),
        failed: true,
        ..Default::default()
    })
}

impl Worktrees {
    pub fn path_for(&self, branch: &str) -> Result<PathBuf> {
        validate_branch(branch)?;
        let parent = self
            .project
            .parent()
            .ok_or_else(|| Error::Tool("the project has no parent directory".into()))?;
        Ok(parent.join(branch.replace('/', "-")))
    }

    /// The registered worktree of `branch`, for sub-agents rooted there.
    pub async fn resolve(&self, branch: &str, cancel: &CancellationToken) -> Result<PathBuf> {
        let path = self.path_for(branch)?;
        if !path.is_dir() || !registered(&self.project, &path, cancel).await? {
            return Err(Error::Tool(format!(
                "no worktree for branch {branch}; create it with the worktree tool first"
            )));
        }
        Ok(path.canonicalize()?)
    }

    pub async fn execute(
        &self,
        name: &str,
        input: Value,
        cancel: CancellationToken,
    ) -> Result<ToolOutput> {
        let result = match name {
            "worktree" => match serde_json::from_value::<EnterArgs>(input) {
                Ok(args) => self.enter(args, &cancel).await,
                Err(err) => failed(err.to_string()),
            },
            _ => match serde_json::from_value::<ExitArgs>(input) {
                Ok(args) => self.exit(args, &cancel).await,
                Err(err) => failed(err.to_string()),
            },
        };
        match result {
            Err(err @ (Error::Approval(_) | Error::Cancelled | Error::Uncertain(_))) => Err(err),
            Err(err) => failed(err.to_string()),
            ok => ok,
        }
    }

    async fn enter(&self, args: EnterArgs, cancel: &CancellationToken) -> Result<ToolOutput> {
        let path = self.path_for(&args.branch)?;
        if path.exists() {
            return failed(format!("{} already exists", path.display()));
        }
        let base = head(&self.project, cancel).await?;
        self.authorizer
            .authorize(
                Effect::Shell(format!(
                    "git worktree add -b {} {}",
                    args.branch,
                    path.display()
                )),
                cancel.clone(),
            )
            .await?;
        let output = add_worktree(&self.project, &path, &args.branch, &base, cancel).await?;
        Ok(ToolOutput {
            content: format!(
                "Worktree created at {} on new branch {} (from {base:.10}). Your tools still act on the main tree; \
                 delegate work inside it with agent(worktree=\"{}\") and finish with exit_worktree. /undo does not cover it; \
                 the branch is its history.\n{output}",
                path.display(),
                args.branch,
                args.branch
            ),
            failed: false,
            ..Default::default()
        })
    }

    async fn exit(&self, args: ExitArgs, cancel: &CancellationToken) -> Result<ToolOutput> {
        let path = self.resolve(&args.branch, cancel).await?;
        let dirty = status(&path, cancel).await?;
        let mut report = String::new();
        if args.merge {
            if !dirty.is_empty() {
                return failed(format!(
                    "worktree has uncommitted changes; commit them inside it before merging:\n{dirty}"
                ));
            }
            self.authorizer
                .authorize(
                    Effect::Shell(format!("git merge {} into the main tree", args.branch)),
                    cancel.clone(),
                )
                .await?;
            match merge_branch(&self.project, &args.branch, None, cancel).await? {
                Ok(text) => report.push_str(&format!("merged {}: {text}\n", args.branch)),
                Err(text) => return failed(text),
            }
        } else if !dirty.is_empty() && !args.discard {
            return failed(format!(
                "worktree has uncommitted changes; commit them, or pass discard=true to remove them:\n{dirty}"
            ));
        }
        let force = !dirty.is_empty();
        self.authorizer
            .authorize(
                Effect::Shell(format!(
                    "git worktree remove {}{}",
                    if force { "--force " } else { "" },
                    path.display()
                )),
                cancel.clone(),
            )
            .await?;
        remove_worktree(&self.project, &path, force, cancel).await?;
        if args.merge {
            delete_branch(&self.project, &args.branch, false, cancel).await?;
            report.push_str(&format!("branch {} deleted after merge\n", args.branch));
        } else {
            report.push_str(&format!("branch {} kept\n", args.branch));
        }
        Ok(ToolOutput {
            content: format!("worktree {} removed\n{report}", path.display()),
            failed: false,
            ..Default::default()
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn branch_names_are_validated() {
        for good in ["feature", "fix/login-1", "v1.2_x"] {
            validate_branch(good).unwrap();
        }
        for bad in [
            "", "-f", "../x", "a/../b", "a//b", "x.lock", "a b", "a:b", ".hidden", "a/.b", "a\\b",
        ] {
            assert!(validate_branch(bad).is_err(), "{bad}");
        }
    }
}
