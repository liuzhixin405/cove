//! Reviewable patches from several sources (race candidates, automation inbox
//! results, the current task) under one model, plus the Git plumbing they share:
//! baselines, detached worktrees, patch capture and guarded apply.
//!
//! Nothing here touches the user's working tree except `apply`, which first
//! re-checks the recorded baseline (root, commit, branch), a clean tree, the last
//! checks and the patch hash, all under a per-project lock.

use std::{
    ffi::OsString,
    fs::OpenOptions,
    path::{Path, PathBuf},
    time::Duration,
};

use fs2::FileExt;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Result},
    process::{Argv, RawOutput, run_argv},
};

/// Patches larger than this are a failed capture, never silently truncated.
pub const PATCH_LIMIT: usize = 4 * 1024 * 1024;
/// Environment marker of a worker started by race or automation; workers refuse to nest.
pub const NESTED_ENV: &str = "COVE_NESTED_WORKER";
const GIT_TIMEOUT: Duration = Duration::from_secs(60);

pub fn sha256_hex(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

/// Drops the Windows verbatim prefix (`\\?\`) that `canonicalize` adds; Git and
/// most tools expect ordinary drive paths.
pub fn plain(path: &Path) -> PathBuf {
    let text = path.to_string_lossy();
    match text.strip_prefix(r"\\?\") {
        Some(rest) if rest.as_bytes().get(1) == Some(&b':') => PathBuf::from(rest),
        _ => path.to_path_buf(),
    }
}

/// Stable short key for per-project state directories and locks.
pub fn project_key(root: &Path) -> String {
    let mut text = plain(root).to_string_lossy().into_owned();
    if cfg!(windows) {
        text = text.to_lowercase();
    }
    sha256_hex(text.as_bytes())[..16].to_owned()
}

pub fn nested() -> bool {
    std::env::var_os(NESTED_ENV).is_some()
}

pub fn refuse_nested() -> Result<()> {
    if nested() {
        return Err(Error::Tool(
            "nested race/automation runs are refused inside a worker".into(),
        ));
    }
    Ok(())
}

/// Runs git with a scrubbed `GIT_*` environment, no prompts and no hooks.
pub async fn git_raw(
    dir: &Path,
    args: &[&str],
    stdin: Option<Vec<u8>>,
    limit: usize,
    cancel: &CancellationToken,
) -> Result<RawOutput> {
    let mut argv = Argv::new(plain(dir), "git")
        .args(["-c", "core.fsmonitor=false"])
        .args(args.iter().copied())
        .env("GIT_TERMINAL_PROMPT", "0")
        .env("GIT_OPTIONAL_LOCKS", "0")
        .timeout(GIT_TIMEOUT)
        .limit(limit)
        .direct();
    argv.env_remove = std::env::vars_os()
        .map(|(key, _)| key)
        .filter(|key| {
            key.to_string_lossy()
                .to_ascii_uppercase()
                .starts_with("GIT_")
                && key != "GIT_TERMINAL_PROMPT"
                && key != "GIT_OPTIONAL_LOCKS"
        })
        .collect::<Vec<OsString>>();
    argv.stdin = stdin;
    run_argv(argv, cancel).await
}

/// Successful git stdout, or an error with its stderr.
pub async fn git(dir: &Path, args: &[&str], cancel: &CancellationToken) -> Result<Vec<u8>> {
    let output = git_raw(dir, args, None, PATCH_LIMIT + 1, cancel).await?;
    if !output.success() {
        return Err(Error::Tool(format!(
            "git {} failed: {}",
            args.join(" "),
            output.stderr_text().trim()
        )));
    }
    if output.stdout.len() > PATCH_LIMIT {
        return Err(Error::Tool(format!(
            "git {} output exceeds 4 MiB",
            args.join(" ")
        )));
    }
    Ok(output.stdout)
}

async fn git_text(dir: &Path, args: &[&str], cancel: &CancellationToken) -> Result<String> {
    Ok(String::from_utf8_lossy(&git(dir, args, cancel).await?)
        .trim()
        .to_owned())
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Baseline {
    pub root: PathBuf,
    pub commit: String,
    /// `refs/heads/<name>`; empty for a detached HEAD (such results cannot be applied).
    pub branch: String,
}

impl Baseline {
    /// Project root, HEAD and branch. `clean` additionally requires no tracked
    /// changes and no untracked files (ignored files are allowed: they are not part
    /// of any patch, and `git apply` refuses to overwrite one).
    pub async fn capture(dir: &Path, clean: bool, cancel: &CancellationToken) -> Result<Self> {
        let root = dir.canonicalize()?;
        if !root.join(".git").exists() {
            return Err(Error::Tool(
                "需要在本地 Git 仓库的根目录（含 .git）中运行；无法模拟隔离环境".into(),
            ));
        }
        let top = git_text(&root, &["rev-parse", "--show-toplevel"], cancel).await?;
        if PathBuf::from(&top).canonicalize()? != root {
            return Err(Error::Tool(
                "需要在 Git 项目根目录运行，而不是子目录；请用 --cwd 指向仓库根目录".into(),
            ));
        }
        if clean {
            let status = git(
                &root,
                &["status", "--porcelain=v1", "--untracked-files=all"],
                cancel,
            )
            .await?;
            if !status.is_empty() {
                return Err(Error::Tool(
                    "需要干净的仓库（没有已跟踪的改动和未跟踪文件），以免遗漏或覆盖你的改动；请先提交（/commit）或 git stash"
                        .into(),
                ));
            }
        }
        let commit = git_text(&root, &["rev-parse", "--verify", "HEAD^{commit}"], cancel)
            .await
            .map_err(|_| Error::Tool("仓库需要至少有一次提交（HEAD）；请先提交".into()))?;
        let branch = git_raw(
            &root,
            &["symbolic-ref", "--quiet", "HEAD"],
            None,
            4096,
            cancel,
        )
        .await?;
        let branch = if branch.success() {
            branch.stdout_text().trim().to_owned()
        } else {
            String::new()
        };
        Ok(Self {
            root,
            commit,
            branch,
        })
    }

    pub fn same(&self, other: &Self) -> bool {
        let root = |path: &Path| {
            let text = plain(path).to_string_lossy().into_owned();
            if cfg!(windows) {
                text.to_lowercase()
            } else {
                text
            }
        };
        self.commit == other.commit
            && self.branch == other.branch
            && root(&self.root) == root(&other.root)
    }

    /// A detached worktree at the recorded commit, with hooks disabled.
    pub async fn add_worktree(&self, path: &Path, cancel: &CancellationToken) -> Result<()> {
        let hooks = path.with_extension("nohooks");
        std::fs::create_dir_all(&hooks)?;
        let hooks_config = format!("core.hooksPath={}", plain(&hooks).display());
        let target = plain(path).to_string_lossy().into_owned();
        git(
            &self.root,
            &[
                "-c",
                &hooks_config,
                "worktree",
                "add",
                "--detach",
                "--",
                &target,
                &self.commit,
            ],
            cancel,
        )
        .await
        .map(|_| ())
    }

    /// Best effort and bounded; returns a description of what was left behind.
    pub async fn remove_worktree(&self, path: &Path) -> Option<String> {
        let cancel = CancellationToken::new();
        let target = plain(path).to_string_lossy().into_owned();
        let removed = tokio::time::timeout(
            Duration::from_secs(15),
            git(
                &self.root,
                &["worktree", "remove", "--force", "--", &target],
                &cancel,
            ),
        )
        .await;
        let _ = std::fs::remove_dir_all(path.with_extension("nohooks"));
        if path.exists() {
            let _ = std::fs::remove_dir_all(path);
        }
        let _ = tokio::time::timeout(
            Duration::from_secs(15),
            git(&self.root, &["worktree", "prune"], &cancel),
        )
        .await;
        match removed {
            Ok(Ok(_)) if !path.exists() => None,
            _ if !path.exists() => None,
            _ => Some(format!("worktree left at {}", path.display())),
        }
    }

    /// Re-checks the project against this baseline; refuses any drift.
    pub async fn check(&self, cancel: &CancellationToken) -> Result<Self> {
        let now = Self::capture(&self.root, true, cancel).await?;
        if !now.same(self) {
            return Err(Error::Tool(
                "项目根目录、分支或基准提交在基线之后发生了变化".into(),
            ));
        }
        Ok(now)
    }

    /// Applies `patch` after re-checking the baseline twice; the final `git apply`
    /// is not cancellable (a half-applied patch is worse than waiting).
    pub async fn apply(&self, patch: &[u8], hash: &str, cancel: &CancellationToken) -> Result<()> {
        if patch.is_empty() {
            return Err(Error::Tool("这个变更集没有补丁".into()));
        }
        if hash.is_empty() || sha256_hex(patch) != hash {
            return Err(Error::Tool("补丁哈希不匹配".into()));
        }
        if self.branch.is_empty() {
            return Err(Error::Tool(
                "基线没有分支（detached HEAD）；请改用 export 导出补丁".into(),
            ));
        }
        self.check(cancel).await?;
        let checked = git_raw(
            &self.root,
            &["apply", "--check", "--whitespace=nowarn", "-"],
            Some(patch.to_vec()),
            64 * 1024,
            cancel,
        )
        .await?;
        if !checked.success() {
            return Err(Error::Tool(format!(
                "git apply --check 失败：{}",
                checked.stderr_text().trim()
            )));
        }
        self.check(cancel).await?;
        let applied = git_raw(
            &self.root,
            &["apply", "--whitespace=nowarn", "-"],
            Some(patch.to_vec()),
            64 * 1024,
            &CancellationToken::new(),
        )
        .await?;
        if !applied.success() {
            return Err(Error::Tool(format!(
                "git apply 失败：{}；重试前请先检查 `git status`",
                applied.stderr_text().trim()
            )));
        }
        Ok(())
    }
}

fn safe_patch_path(path: &str) -> bool {
    !path.is_empty()
        && !path.starts_with('/')
        && !path.contains(['\\', ':', '\0', '\r', '\n'])
        && path.split('/').all(|part| {
            !part.is_empty() && part != "." && part != ".." && !part.eq_ignore_ascii_case(".git")
        })
}

/// Binary patch of everything in `dir` (tracked and untracked, ignored files
/// removed first) against `commit`. Symlinks, submodules and unsafe paths fail.
pub async fn capture_patch(
    dir: &Path,
    commit: &str,
    cancel: &CancellationToken,
) -> Result<Vec<u8>> {
    git(dir, &["clean", "-fdX", "-q", "--", "."], cancel).await?;
    let ignored = git(
        dir,
        &[
            "ls-files",
            "--others",
            "--ignored",
            "--exclude-standard",
            "-z",
        ],
        cancel,
    )
    .await?;
    if !ignored.is_empty() {
        return Err(Error::Tool(
            "candidate keeps ignored files that cannot be part of the patch".into(),
        ));
    }
    git(dir, &["add", "-A", "--", "."], cancel).await?;
    let entries = git(dir, &["ls-files", "--stage", "-z"], cancel).await?;
    for entry in entries
        .split(|byte| *byte == 0)
        .filter(|entry| !entry.is_empty())
    {
        let entry = String::from_utf8_lossy(entry);
        let Some((meta, path)) = entry.split_once('\t') else {
            return Err(Error::Tool("unreadable index entry".into()));
        };
        if !safe_patch_path(path) || !(meta.starts_with("100644 ") || meta.starts_with("100755 ")) {
            return Err(Error::Tool(format!(
                "unsafe candidate path or mode (symlink, submodule or nested repository): {path:?}"
            )));
        }
    }
    git(
        dir,
        &[
            "-c",
            "color.ui=false",
            "-c",
            "diff.noprefix=false",
            "-c",
            "diff.mnemonicPrefix=false",
            "-c",
            "diff.relative=false",
            "-c",
            "diff.renames=false",
            "diff",
            "--cached",
            "--binary",
            "--no-ext-diff",
            "--no-textconv",
            "--full-index",
            "--no-color",
            "--src-prefix=a/",
            "--dst-prefix=b/",
            "--no-renames",
            commit,
            "--",
            ".",
        ],
        cancel,
    )
    .await
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CheckStatus {
    Passed,
    Failed,
    /// Not run (cancelled, start error, timeout).
    Unverified,
}

/// One verifier argv and its outcome; pass/fail is the exit code only.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Check {
    pub argv: Vec<String>,
    pub exit_code: Option<i32>,
    pub status: CheckStatus,
    #[serde(default)]
    pub attempt: u32,
    pub duration_ms: u64,
    #[serde(default)]
    pub output: String,
}

/// Keeps the last `limit` bytes of `text` on a character boundary.
pub fn tail(text: &str, limit: usize) -> String {
    if text.len() <= limit {
        return text.to_owned();
    }
    let mut start = text.len() - limit;
    while !text.is_char_boundary(start) {
        start += 1;
    }
    format!("[...]{}", &text[start..])
}

/// Runs every argv in `dir` (gated process tree, credentials removed from the
/// environment); stops at the first failure, the rest are recorded unverified.
pub async fn run_checks(
    dir: &Path,
    commands: &[Vec<String>],
    attempt: u32,
    deadline: tokio::time::Instant,
    cancel: &CancellationToken,
) -> Vec<Check> {
    let mut checks = Vec::new();
    let mut failed = false;
    for argv in commands {
        let started = std::time::Instant::now();
        let remaining = deadline.saturating_duration_since(tokio::time::Instant::now());
        if failed || remaining.is_zero() || cancel.is_cancelled() {
            checks.push(Check {
                argv: argv.clone(),
                exit_code: None,
                status: CheckStatus::Unverified,
                attempt,
                duration_ms: 0,
                output: if failed {
                    "not run after an earlier failure".into()
                } else {
                    "not run: deadline or cancellation".into()
                },
            });
            continue;
        }
        let mut command = Argv::new(plain(dir), &argv[0])
            .args(argv[1..].iter())
            .env(NESTED_ENV, "1")
            .timeout(remaining)
            .limit(256 * 1024);
        command.env_remove = ["COVE_API_KEY".into()].into();
        let (exit_code, status, output) = match run_argv(command, cancel).await {
            Ok(output) => {
                let text = format!("{}{}", output.stdout_text(), output.stderr_text());
                let status = if output.success() {
                    CheckStatus::Passed
                } else if output.timed_out {
                    CheckStatus::Unverified
                } else {
                    CheckStatus::Failed
                };
                (output.code, status, tail(&text, 16 * 1024))
            }
            Err(err) => (None, CheckStatus::Unverified, err.to_string()),
        };
        failed |= status != CheckStatus::Passed;
        checks.push(Check {
            argv: argv.clone(),
            exit_code,
            status,
            attempt,
            duration_ms: started.elapsed().as_millis() as u64,
            output,
        });
    }
    checks
}

pub fn validate_argvs(commands: &[Vec<String>]) -> Result<()> {
    if commands.len() > 16 {
        return Err(Error::Tool("at most 16 verifier commands".into()));
    }
    for argv in commands {
        if argv.is_empty() || argv[0].trim().is_empty() {
            return Err(Error::Tool("empty verifier command".into()));
        }
        if argv
            .iter()
            .any(|arg| arg.contains('\0') || arg.len() > 4096)
        {
            return Err(Error::Tool("invalid verifier argument".into()));
        }
    }
    Ok(())
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Kind {
    Race,
    Inbox,
    Turn,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Decision {
    #[default]
    Pending,
    Accepted,
    Rejected,
    Applied,
}

impl Decision {
    pub fn parse(text: &str) -> Result<Self> {
        match text {
            "accepted" | "accept" => Ok(Self::Accepted),
            "rejected" | "reject" => Ok(Self::Rejected),
            _ => Err(Error::Tool("评审结论只能是 accepted 或 rejected".into())),
        }
    }
}

/// A patch against a baseline with the evidence needed to decide on it.
#[derive(Clone, Debug, Serialize)]
pub struct Changeset {
    pub id: String,
    pub kind: Kind,
    pub title: String,
    pub baseline: Option<Baseline>,
    pub patch_sha256: String,
    pub patch_bytes: usize,
    pub checks: Vec<Check>,
    pub review: Decision,
    /// `None` when it can be applied; otherwise the reason it cannot.
    pub not_applicable: Option<String>,
    pub created: u64,
    #[serde(skip)]
    pub patch_path: Option<PathBuf>,
}

impl Changeset {
    /// Checks of the highest attempt only.
    pub fn last_checks(&self) -> Vec<&Check> {
        let last = self
            .checks
            .iter()
            .map(|check| check.attempt)
            .max()
            .unwrap_or(0);
        self.checks
            .iter()
            .filter(|check| check.attempt == last)
            .collect()
    }

    pub fn checks_passed(&self) -> bool {
        let last = self.last_checks();
        !last.is_empty()
            && last
                .iter()
                .all(|check| check.status == CheckStatus::Passed && check.exit_code == Some(0))
    }

    pub fn summary(&self) -> String {
        let checks = if self.checks.is_empty() {
            "no checks".to_owned()
        } else if self.checks_passed() {
            "checks passed".to_owned()
        } else {
            "checks NOT passed".to_owned()
        };
        format!(
            "{:<44} {:?}/{:?} {} bytes, {checks}{} - {}",
            self.id,
            self.kind,
            self.review,
            self.patch_bytes,
            self.not_applicable
                .as_ref()
                .map_or(String::new(), |reason| format!(
                    ", not applicable: {reason}"
                )),
            self.title
        )
    }

    pub fn load_patch(&self) -> Result<Vec<u8>> {
        let path = self
            .patch_path
            .as_ref()
            .ok_or_else(|| Error::Tool("这个变更集没有保存的补丁".into()))?;
        let bytes = std::fs::read(path)?;
        if bytes.len() != self.patch_bytes || sha256_hex(&bytes) != self.patch_sha256 {
            return Err(Error::Tool("保存的补丁大小或哈希不匹配".into()));
        }
        Ok(bytes)
    }

    /// The four apply preconditions, in order: applicable, baseline (clean, no
    /// drift), last checks passed, patch size and hash. Returns the patch.
    pub async fn preflight(&self, project: &Path, cancel: &CancellationToken) -> Result<Vec<u8>> {
        if let Some(reason) = &self.not_applicable {
            return Err(Error::Tool(format!("这个变更集不能应用：{reason}")));
        }
        match self.review {
            Decision::Applied => return Err(Error::Tool("这个变更集已经应用过了".into())),
            Decision::Rejected => return Err(Error::Tool("这个变更集已被拒绝".into())),
            _ => {}
        }
        let baseline = self
            .baseline
            .as_ref()
            .ok_or_else(|| Error::Tool("这个变更集没有基线".into()))?;
        let current = Baseline::capture(project, true, cancel).await?;
        if !current.same(baseline) {
            return Err(Error::Tool(
                "项目根目录、分支或基准提交与变更集的基线不一致；请切回原分支和提交，或改用 export 导出补丁".into(),
            ));
        }
        if !self.checks_passed() {
            return Err(Error::Tool(
                "最近一次记录的验证没有通过（或没有记录）；请先重新验证".into(),
            ));
        }
        self.load_patch()
    }
}

/// Exclusive per-project apply lock (`<data>/changesets/<key>.lock`), waited for up to `timeout`.
pub async fn project_lock(
    data_dir: &Path,
    project: &Path,
    timeout: Duration,
) -> Result<std::fs::File> {
    let directory = data_dir.join("changesets");
    std::fs::create_dir_all(&directory)?;
    let file = OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .open(directory.join(format!("{}.lock", project_key(project))))?;
    let deadline = tokio::time::Instant::now() + timeout;
    loop {
        if file.try_lock_exclusive().is_ok() {
            return Ok(file);
        }
        if tokio::time::Instant::now() >= deadline {
            return Err(Error::Tool(
                "另一个应用操作正占用这个项目的锁；请稍后重试".into(),
            ));
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

/// Validates a user-supplied output path: a new file outside the project.
pub fn new_outside_file(path: &Path, project: &Path) -> Result<PathBuf> {
    let absolute = if path.is_absolute() {
        path.to_path_buf()
    } else {
        std::env::current_dir()?.join(path)
    };
    let parent = absolute
        .parent()
        .ok_or_else(|| Error::Tool("输出路径没有上级目录".into()))?
        .canonicalize()?;
    let target = parent.join(
        absolute
            .file_name()
            .ok_or_else(|| Error::Tool("输出路径没有文件名".into()))?,
    );
    if target.starts_with(project.canonicalize()?) {
        return Err(Error::Tool(
            "不能导出到项目目录内；请选择项目外的路径".into(),
        ));
    }
    if std::fs::symlink_metadata(&target).is_ok() {
        return Err(Error::Tool("输出文件已存在；请换一个文件名".into()));
    }
    Ok(target)
}

/// Writes `bytes` to a new file, never replacing an existing one.
pub fn write_new(path: &Path, bytes: &[u8]) -> Result<()> {
    use std::io::Write;
    let mut file = OpenOptions::new().write(true).create_new(true).open(path)?;
    file.write_all(bytes)?;
    file.sync_all()?;
    Ok(())
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    pub(crate) fn git_sync(dir: &Path, args: &[&str]) {
        let status = std::process::Command::new("git")
            .args([
                "-c",
                "user.name=t",
                "-c",
                "user.email=t@t",
                "-c",
                "commit.gpgsign=false",
            ])
            .args(args)
            .current_dir(dir)
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .status()
            .unwrap();
        assert!(status.success(), "git {args:?}");
    }

    pub(crate) fn repo() -> tempfile::TempDir {
        let dir = tempfile::tempdir().unwrap();
        git_sync(dir.path(), &["init", "-q", "-b", "main"]);
        std::fs::write(dir.path().join("a.txt"), "one\n").unwrap();
        std::fs::write(dir.path().join(".gitignore"), "target/\n").unwrap();
        git_sync(dir.path(), &["add", "-A"]);
        git_sync(dir.path(), &["commit", "-q", "-m", "init"]);
        dir
    }

    #[test]
    fn patch_paths_reject_traversal_and_git_metadata() {
        assert!(safe_patch_path("src/a.rs"));
        for bad in ["", "/abs", "a/../b", ".git/config", "a\\b", "c:x", "a//b"] {
            assert!(!safe_patch_path(bad), "{bad}");
        }
    }

    #[test]
    fn last_attempt_decides_and_empty_checks_never_pass() {
        let check = |attempt, status| Check {
            argv: vec!["x".into()],
            exit_code: Some(if status == CheckStatus::Passed { 0 } else { 1 }),
            status,
            attempt,
            duration_ms: 0,
            output: String::new(),
        };
        let mut set = Changeset {
            id: "x".into(),
            kind: Kind::Inbox,
            title: String::new(),
            baseline: None,
            patch_sha256: String::new(),
            patch_bytes: 0,
            checks: vec![],
            review: Decision::Pending,
            not_applicable: None,
            created: 0,
            patch_path: None,
        };
        assert!(!set.checks_passed());
        set.checks = vec![check(1, CheckStatus::Failed), check(2, CheckStatus::Passed)];
        assert!(set.checks_passed());
        set.checks.push(check(2, CheckStatus::Unverified));
        assert!(!set.checks_passed());
    }

    #[tokio::test]
    async fn worktree_patch_and_guarded_apply_roundtrip() {
        let repo = repo();
        let cancel = CancellationToken::new();
        let baseline = Baseline::capture(repo.path(), true, &cancel).await.unwrap();
        assert_eq!(baseline.branch, "refs/heads/main");
        let scratch = tempfile::tempdir().unwrap();
        let tree = scratch.path().join("wt");
        baseline.add_worktree(&tree, &cancel).await.unwrap();
        std::fs::write(tree.join("a.txt"), "two\n").unwrap();
        std::fs::write(tree.join("new.txt"), "new\n").unwrap();
        std::fs::create_dir_all(tree.join("target")).unwrap();
        std::fs::write(tree.join("target/cache"), "x").unwrap();
        let patch = capture_patch(&tree, &baseline.commit, &cancel)
            .await
            .unwrap();
        let text = String::from_utf8_lossy(&patch);
        assert!(text.contains("b/new.txt") && !text.contains("target/cache"));
        assert!(baseline.remove_worktree(&tree).await.is_none());
        // The user's tree was never touched.
        assert_eq!(
            std::fs::read_to_string(repo.path().join("a.txt")).unwrap(),
            "one\n"
        );
        assert!(baseline.apply(&patch, "bad", &cancel).await.is_err());
        std::fs::write(repo.path().join("dirty.txt"), "x").unwrap();
        assert!(
            baseline
                .apply(&patch, &sha256_hex(&patch), &cancel)
                .await
                .is_err()
        );
        std::fs::remove_file(repo.path().join("dirty.txt")).unwrap();
        baseline
            .apply(&patch, &sha256_hex(&patch), &cancel)
            .await
            .unwrap();
        assert_eq!(
            std::fs::read_to_string(repo.path().join("a.txt"))
                .unwrap()
                .trim_end(),
            "two"
        );
    }

    #[tokio::test]
    async fn subdirectories_and_non_git_projects_are_refused() {
        let cancel = CancellationToken::new();
        let plain_dir = tempfile::tempdir().unwrap();
        assert!(
            Baseline::capture(plain_dir.path(), false, &cancel)
                .await
                .is_err()
        );
        let repo = repo();
        std::fs::create_dir(repo.path().join("sub")).unwrap();
        assert!(
            Baseline::capture(&repo.path().join("sub"), false, &cancel)
                .await
                .is_err()
        );
    }

    #[tokio::test]
    async fn project_lock_is_exclusive() {
        let data = tempfile::tempdir().unwrap();
        let project = tempfile::tempdir().unwrap();
        let held = project_lock(data.path(), project.path(), Duration::from_secs(1))
            .await
            .unwrap();
        assert!(
            project_lock(data.path(), project.path(), Duration::from_millis(200))
                .await
                .is_err()
        );
        drop(held);
        assert!(
            project_lock(data.path(), project.path(), Duration::from_millis(200))
                .await
                .is_ok()
        );
    }
}
