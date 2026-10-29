//! The interpreter that runs the model's shell commands, chosen once per process.
//!
//! The shell tool description, the system prompt's environment line and the kernel's
//! dialect note all read `Shell::current()`, so the model is told the syntax it really gets.

use std::{
    path::{Path, PathBuf},
    sync::OnceLock,
};

use crate::domain::{Error, Result};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ShellKind {
    /// bash or a POSIX sh, run with `-c`.
    Posix,
    /// Windows PowerShell or pwsh; the script is fed through stdin.
    PowerShell,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Shell {
    pub kind: ShellKind,
    pub path: PathBuf,
}

static CURRENT: OnceLock<Shell> = OnceLock::new();

impl Shell {
    /// PowerShell on Windows and sh elsewhere: the dialect of hooks and verification commands.
    pub fn legacy() -> Self {
        if cfg!(windows) {
            // Exactly the interpreter hooks were written for, even when pwsh is installed.
            Self {
                kind: ShellKind::PowerShell,
                path: "powershell.exe".into(),
            }
        } else {
            Self {
                kind: ShellKind::Posix,
                path: "sh".into(),
            }
        }
    }

    fn powershell() -> Self {
        let path = ["pwsh", "powershell"]
            .into_iter()
            .find_map(|name| which::which(name).ok())
            .unwrap_or_else(|| "powershell.exe".into());
        Self {
            kind: ShellKind::PowerShell,
            path,
        }
    }

    /// Git Bash, then a non-WSL bash on PATH, then PowerShell on Windows; bash or sh elsewhere.
    pub fn detect() -> Self {
        if !cfg!(windows) {
            let path = which::which("bash")
                .or_else(|_| which::which("sh"))
                .unwrap_or_else(|_| "/bin/sh".into());
            return Self {
                kind: ShellKind::Posix,
                path,
            };
        }
        if let Some(path) = git_bash(
            which::which("git").ok().as_deref(),
            &|name| std::env::var(name).ok(),
            &|path| path.is_file(),
        ) {
            return Self {
                kind: ShellKind::Posix,
                path,
            };
        }
        if let Ok(path) = which::which("bash")
            && !is_wsl_launcher(&path)
        {
            return Self {
                kind: ShellKind::Posix,
                path,
            };
        }
        Self::powershell()
    }

    /// `auto`, `bash`, `powershell`/`pwsh`, `sh`, or a path to a bash/sh/PowerShell executable.
    pub fn from_setting(value: &str) -> Result<Self> {
        let value = value.trim();
        Ok(match value.to_ascii_lowercase().as_str() {
            "" | "auto" => Self::detect(),
            "powershell" | "pwsh" => Self::powershell(),
            "bash" | "sh" => {
                let detected = Self::detect();
                if detected.kind == ShellKind::Posix {
                    detected
                } else {
                    return Err(Error::Tool(format!("shell {value:?}: no bash found")));
                }
            }
            _ => {
                let path = PathBuf::from(value);
                if !path.is_file() {
                    return Err(Error::Tool(format!("shell {value:?} is not a file")));
                }
                let stem = path
                    .file_stem()
                    .map(|stem| stem.to_string_lossy().to_ascii_lowercase())
                    .unwrap_or_default();
                let kind = if stem == "pwsh" || stem == "powershell" {
                    ShellKind::PowerShell
                } else {
                    ShellKind::Posix
                };
                Self { kind, path }
            }
        })
    }

    /// Fixes the process-wide shell; later calls keep the first choice. Returns the current one.
    pub fn install(shell: Shell) -> &'static Shell {
        CURRENT.get_or_init(|| shell)
    }

    /// The process-wide shell: installed by the application, else `COVE_SHELL`, else detected.
    pub fn current() -> &'static Shell {
        CURRENT.get_or_init(|| {
            std::env::var("COVE_SHELL")
                .ok()
                .and_then(|value| Self::from_setting(&value).ok())
                .unwrap_or_else(Self::detect)
        })
    }

    /// Short dialect name for prompts: `bash`, `sh` or `PowerShell`.
    pub fn dialect(&self) -> &'static str {
        match self.kind {
            ShellKind::PowerShell => "PowerShell",
            ShellKind::Posix => {
                let stem = self
                    .path
                    .file_stem()
                    .map(|stem| stem.to_string_lossy().to_ascii_lowercase());
                if stem.as_deref() == Some("sh") {
                    "POSIX sh"
                } else {
                    "bash"
                }
            }
        }
    }

    /// Dialect plus where it lives, e.g. `bash (D:\Program Files\Git\bin\bash.exe)`.
    pub fn describe(&self) -> String {
        let mut text = format!("{} ({})", self.dialect(), self.path.display());
        if cfg!(windows) && self.kind == ShellKind::Posix {
            text.push_str(" on Windows; use POSIX paths like /d/dir or forward slashes");
        }
        text
    }
}

/// Git for Windows' bash. `Git\bin\bash.exe` is the wrapper that puts coreutils on PATH.
fn git_bash(
    git: Option<&Path>,
    env: &dyn Fn(&str) -> Option<String>,
    exists: &dyn Fn(&Path) -> bool,
) -> Option<PathBuf> {
    let mut roots = Vec::new();
    if let Some(dir) = git.and_then(Path::parent) {
        // git.exe lives in <root>\cmd, <root>\bin or <root>\mingw64\bin.
        roots.extend(dir.parent().map(Path::to_path_buf));
        roots.extend(dir.parent().and_then(Path::parent).map(Path::to_path_buf));
    }
    for name in ["ProgramFiles", "ProgramW6432", "LOCALAPPDATA"] {
        if let Some(base) = env(name).filter(|base| !base.is_empty()) {
            let base = PathBuf::from(base);
            roots.push(if name == "LOCALAPPDATA" {
                base.join("Programs").join("Git")
            } else {
                base.join("Git")
            });
        }
    }
    roots.into_iter().find_map(|root| {
        [
            root.join("bin").join("bash.exe"),
            root.join("usr").join("bin").join("bash.exe"),
        ]
        .into_iter()
        .find(|candidate| exists(candidate))
    })
}

/// `System32\bash.exe` and `WindowsApps\bash.exe` hand commands to a WSL distro.
fn is_wsl_launcher(path: &Path) -> bool {
    let lower = path
        .to_string_lossy()
        .to_ascii_lowercase()
        .replace('/', "\\");
    lower.ends_with("\\system32\\bash.exe")
        || lower.ends_with("\\sysnative\\bash.exe")
        || lower.contains("\\windowsapps\\")
}

/// Variables that keep commands from waiting on input nobody can type (editor, pager,
/// credential prompt), strip colour escapes, and disable repository-configured fsmonitor
/// programs and implicit bare repositories. `inherited_count` is the `GIT_CONFIG_COUNT`
/// the child already has, so user-supplied entries keep their indices.
pub fn noninteractive_env(inherited_count: usize) -> Vec<(String, String)> {
    let mut env: Vec<(String, String)> = [
        ("GIT_EDITOR", "true"),
        ("GIT_SEQUENCE_EDITOR", "true"),
        ("GIT_TERMINAL_PROMPT", "0"),
        ("GIT_PAGER", "cat"),
        ("PAGER", "cat"),
        ("NO_COLOR", "1"),
        ("TERM", "dumb"),
        ("CLICOLOR", "0"),
        ("FORCE_COLOR", "0"),
        ("GCM_INTERACTIVE", "never"),
        ("DEBIAN_FRONTEND", "noninteractive"),
    ]
    .into_iter()
    .map(|(key, value)| (key.to_owned(), value.to_owned()))
    .collect();
    let n = inherited_count;
    env.push(("GIT_CONFIG_COUNT".into(), (n + 2).to_string()));
    env.push((format!("GIT_CONFIG_KEY_{n}"), "core.fsmonitor".into()));
    env.push((format!("GIT_CONFIG_VALUE_{n}"), "false".into()));
    env.push((
        format!("GIT_CONFIG_KEY_{}", n + 1),
        "safe.bareRepository".into(),
    ));
    env.push((format!("GIT_CONFIG_VALUE_{}", n + 1), "explicit".into()));
    env
}

/// The `GIT_CONFIG_COUNT` of this process, or 0 when unset or not a count git accepts.
pub fn inherited_git_config_count() -> usize {
    std::env::var("GIT_CONFIG_COUNT")
        .ok()
        .and_then(|value| value.trim().parse::<usize>().ok())
        .filter(|count| *count <= 1 << 20)
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn git_bash_is_found_next_to_git_or_in_program_files() {
        let exists = |path: &Path| {
            path == Path::new("D:/Git/bin/bash.exe")
                || path == Path::new("C:/PF/Git/usr/bin/bash.exe")
        };
        let none = |_: &str| None;
        assert_eq!(
            git_bash(Some(Path::new("D:/Git/cmd/git.exe")), &none, &exists),
            Some(PathBuf::from("D:/Git/bin/bash.exe"))
        );
        let env = |name: &str| (name == "ProgramFiles").then(|| "C:/PF".to_owned());
        assert_eq!(
            git_bash(None, &env, &exists),
            Some(PathBuf::from("C:/PF/Git/usr/bin/bash.exe"))
        );
        assert_eq!(git_bash(None, &none, &exists), None);
    }

    #[test]
    fn wsl_launchers_are_not_used_as_bash() {
        assert!(is_wsl_launcher(Path::new(
            "C:\\Windows\\System32\\bash.exe"
        )));
        assert!(is_wsl_launcher(Path::new(
            "C:/Users/a/AppData/Local/Microsoft/WindowsApps/bash.exe"
        )));
        assert!(!is_wsl_launcher(Path::new("D:\\Git\\bin\\bash.exe")));
    }

    #[test]
    fn environment_extends_existing_git_config_entries() {
        let env = noninteractive_env(2);
        let get = |key: &str| {
            env.iter()
                .find(|(name, _)| name == key)
                .map(|(_, value)| value.as_str())
        };
        assert_eq!(get("GIT_CONFIG_COUNT"), Some("4"));
        assert_eq!(get("GIT_CONFIG_KEY_2"), Some("core.fsmonitor"));
        assert_eq!(get("GIT_CONFIG_VALUE_3"), Some("explicit"));
        assert_eq!(get("GIT_TERMINAL_PROMPT"), Some("0"));
        assert_eq!(get("NO_COLOR"), Some("1"));
    }

    #[test]
    fn settings_name_the_dialect() {
        let shell = Shell {
            kind: ShellKind::Posix,
            path: "/usr/bin/bash".into(),
        };
        assert_eq!(shell.dialect(), "bash");
        let shell = Shell {
            kind: ShellKind::PowerShell,
            path: "powershell.exe".into(),
        };
        assert_eq!(shell.dialect(), "PowerShell");
        assert!(Shell::from_setting("definitely/not/a/shell").is_err());
    }
}
