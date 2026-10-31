//! Risk categories of shell command lines, judged on the tokenized line.

use super::{
    safety,
    shell::{self, Dialect, Line, Simple},
};

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum Category {
    /// Read-only; never changes anything.
    Safe,
    /// Build or test; runs project code and may write build artifacts.
    Build,
    /// Git operation that may change the repository.
    Git,
    /// Package installation; may change the system.
    Install,
    /// Not recognized or not statically judgeable.
    Unknown,
    /// Irreversible damage outside the project; always blocked.
    Dangerous,
}

impl Category {
    pub fn label(self) -> &'static str {
        match self {
            Self::Safe => "只读",
            Self::Build => "构建/测试",
            Self::Git => "git（可能修改仓库）",
            Self::Install => "安装依赖包",
            Self::Unknown => "未识别",
            Self::Dangerous => "危险",
        }
    }
}

/// Riskiest category of all simple commands in `command`.
pub fn classify(command: &str, dialect: Dialect) -> Category {
    classify_parsed(command, dialect).0
}

pub(crate) fn classify_parsed(command: &str, dialect: Dialect) -> (Category, Line) {
    let line = shell::parse(command, dialect);
    if command.trim().is_empty() {
        return (Category::Unknown, line);
    }
    if safety::catastrophic(command).is_some() {
        return (Category::Dangerous, line);
    }
    if !line.opaque.is_empty() || line.pipelines.is_empty() {
        return (Category::Unknown, line);
    }
    let worst = line
        .commands()
        .map(|simple| classify_simple(simple, dialect))
        .max()
        .unwrap_or(Category::Unknown);
    (worst, line)
}

/// Auto-allowed in every mode except plan restrictions: read-only and no network.
pub fn read_only(command: &str, dialect: Dialect) -> bool {
    let (category, line) = classify_parsed(command, dialect);
    category == Category::Safe && !line.commands().any(fetches)
}

/// Auto-allowed in `auto` mode: read-only or build/test, and no network fetch.
pub fn auto_approvable(command: &str, dialect: Dialect) -> bool {
    let (category, line) = classify_parsed(command, dialect);
    matches!(category, Category::Safe | Category::Build) && !line.commands().any(fetches)
}

/// Git steps that stay in the local repository and can be undone (`add`, `commit`,
/// `tag`), alone or with read-only commands: no approval in default or auto mode.
/// Push and anything forced, deleting or rewriting still asks.
pub fn local_git(command: &str, dialect: Dialect) -> bool {
    const LOCAL: &[&str] = &["add", "commit", "tag"];
    let (category, line) = classify_parsed(command, dialect);
    if category > Category::Git || !line.opaque.is_empty() {
        return false;
    }
    let git = group("git");
    let mut any = false;
    for simple in line.commands() {
        if classify_simple(simple, dialect) == Category::Safe && !fetches(simple) {
            continue;
        }
        let words = simple.argv();
        let local = words
            .first()
            .is_some_and(|program| shell::program_name(program) == "git")
            && git_subcommand(&words[1..]).is_some_and(|(sub, _)| LOCAL.contains(&sub))
            && git.is_some_and(|group| group.covers(&words))
            && simple
                .redirects
                .iter()
                .all(|r| r.op.starts_with('<') || discards(&r.target));
        if !local {
            return false;
        }
        any = true;
    }
    any
}

fn fetches(simple: &Simple) -> bool {
    simple.words.first().is_some_and(|word| {
        matches!(
            shell::program_name(&word.text).as_str(),
            "curl" | "wget" | "iwr" | "irm" | "invoke-webrequest" | "invoke-restmethod"
        )
    })
}

fn discards(target: &str) -> bool {
    let target = target.to_ascii_lowercase();
    target.starts_with('&')
        || matches!(
            target.as_str(),
            "/dev/null" | "$null" | "nul" | "/dev/stdout" | "/dev/stderr"
        )
}

fn classify_simple(simple: &Simple, dialect: Dialect) -> Category {
    if simple.words.is_empty()
        || simple.words[0].dynamic
        || simple
            .redirects
            .iter()
            .any(|r| !r.op.starts_with('<') && !discards(&r.target))
        || simple
            .redirects
            .iter()
            .any(|r| r.op.starts_with('<') && r.op != "<")
    {
        return Category::Unknown;
    }
    let words: Vec<&str> = simple.argv();
    if dialect == Dialect::PowerShell
        && matches!(
            shell::program_name(words[0]).as_str(),
            "where"
                | "where-object"
                | "?"
                | "foreach"
                | "foreach-object"
                | "%"
                | "invoke-expression"
                | "iex"
        )
    {
        return Category::Unknown;
    }
    classify_words(&words)
}

fn has(args: &[&str], wanted: &[&str]) -> bool {
    args.iter().any(|arg| wanted.contains(arg))
}

fn unc(word: &str) -> bool {
    let word = word.trim_matches(['"', '\'']);
    (word.starts_with("\\\\") || word.starts_with("//"))
        && word
            .chars()
            .nth(2)
            .is_some_and(|c| !matches!(c, '/' | '\\' | ' '))
}

pub fn classify_words(words: &[&str]) -> Category {
    let Some(exe) = words.first() else {
        return Category::Unknown;
    };
    if exe.contains(['/', '\\', '$', '=', '*', '?', '[']) || exe.starts_with('-') {
        return Category::Unknown;
    }
    let name = shell::program_name(exe);
    let args = &words[1..];
    if args.iter().any(|arg| unc(arg)) {
        return Category::Unknown;
    }
    match name.as_str() {
        "git" => classify_git(args),
        "ls" | "dir" | "pwd" | "echo" | "cat" | "head" | "tail" | "wc" | "du" | "df"
        | "printenv" | "which" | "where" | "whoami" | "uname" | "uptime" | "id" | "groups"
        | "grep" | "egrep" | "fgrep" | "stat" | "type" | "realpath" | "basename" | "dirname"
        | "true" | "sort" | "uniq" | "cut" | "diff" | "cmp" | "comm" | "nl" | "cd" | "pushd"
        | "popd" | "get-childitem" | "gci" | "get-content" | "gc" | "get-location" | "gl"
        | "select-string" | "sls" | "test-path" | "get-item" | "gi" | "resolve-path"
        | "get-command" | "gcm" | "set-location" | "sl" | "write-output" | "write-host"
        | "select-object" | "measure-object" | "sort-object" | "get-date" | "format-list"
        | "format-table" | "out-string" | "get-filehash" | "split-path" | "join-path"
        | "get-itemproperty" | "strings" | "cygpath" | "file" | "readlink" | "tac" | "rev"
        | "tr" | "od" | "hexdump" | "md5sum" | "sha1sum" | "sha256sum" | "seq" | "column"
        | "expand" | "fold" | "paste" | "jq" => {
            if name == "sort"
                && args
                    .iter()
                    .any(|a| a.starts_with("-o") || a.starts_with("--output"))
            {
                Category::Unknown
            } else {
                Category::Safe
            }
        }
        "command" if args.len() >= 2 && matches!(args[0], "-v" | "-V") => Category::Safe,
        // `timeout [options] DURATION COMMAND...` is as safe as the command it runs.
        "timeout" => {
            let mut rest = args;
            while let Some(first) = rest.first().filter(|a| a.starts_with('-')) {
                rest = match *first {
                    "-s" | "-k" | "--signal" | "--kill-after" => rest.get(2..).unwrap_or(&[]),
                    _ => &rest[1..],
                };
            }
            match rest.split_first() {
                Some((_, command)) if !command.is_empty() => classify_words(command),
                _ => Category::Unknown,
            }
        }
        "sed" => {
            if sed_prints_only(args) {
                Category::Safe
            } else {
                Category::Unknown
            }
        }
        "hostname" | "date" if args.is_empty() || args.iter().all(|a| a.starts_with('+')) => {
            Category::Safe
        }
        "rg" => {
            if args.iter().take_while(|a| **a != "--").any(|a| {
                let name = a.split('=').next().unwrap_or(a);
                matches!(name, "--pre" | "--pre-glob" | "--hostname-bin")
            }) {
                Category::Unknown
            } else {
                Category::Safe
            }
        }
        "tree" => {
            if args.iter().any(|a| {
                a.starts_with("--output")
                    || (a.starts_with('-') && !a.starts_with("--") && a.contains(['o', 'H', 'R']))
            }) {
                Category::Unknown
            } else {
                Category::Safe
            }
        }
        "find" => {
            if args.iter().any(|a| {
                matches!(
                    *a,
                    "-delete" | "-exec" | "-execdir" | "-ok" | "-okdir" | "-fls"
                ) || a.starts_with("-fprint")
            }) {
                Category::Unknown
            } else {
                Category::Safe
            }
        }
        "go" | "cargo" | "rustc" | "javac" | "tsc" | "make" | "cmake" | "ninja" | "bazel"
        | "meson" | "gradle" | "mvn" => classify_build(&name, args),
        "dotnet" => match args.first() {
            Some(&("test" | "build" | "run")) if !args.iter().any(|a| a.starts_with("--force")) => {
                Category::Build
            }
            Some(&("--version" | "--info" | "--list-sdks")) => Category::Safe,
            _ => Category::Unknown,
        },
        "pytest" | "gofmt" | "rustfmt" | "jest" | "vitest" | "mocha" | "eslint" | "prettier"
        | "ruff" | "mypy" => {
            if args.iter().any(|a| {
                a.starts_with("--basetemp")
                    || a.contains("cpuprofile")
                    || *a == "--write"
                    || *a == "-w"
                    || *a == "--fix"
            }) {
                Category::Unknown
            } else {
                Category::Build
            }
        }
        "python" | "python3" | "py" => match args {
            [
                "-m",
                "pytest" | "unittest" | "compileall" | "mypy" | "ruff",
                ..,
            ] => Category::Build,
            ["--version" | "-V"] => Category::Safe,
            _ => Category::Unknown,
        },
        "node" | "rustup" | "npx" if matches!(args, ["--version" | "-v"]) => Category::Safe,
        "npm" | "yarn" | "pnpm" | "pip" | "pip3" | "gem" | "composer" | "nuget" | "apt"
        | "apt-get" | "yum" | "dnf" | "brew" | "choco" | "winget" | "pacman" | "zypper"
        | "snap" | "flatpak" => classify_package(&name, args),
        "docker" | "podman" | "nerdctl" => classify_docker(args),
        "certutil" if matches!(args, [flag, _, ..] if flag.eq_ignore_ascii_case("-hashfile")) => {
            Category::Safe
        }
        "curl" | "wget" => classify_fetch(&name, args),
        _ => Category::Unknown,
    }
}

/// `sed` that only prints: no in-place editing, no script file, and scripts made of
/// address/print commands or substitutions without the `w` and `e` flags (the
/// commands that write files or run programs).
fn sed_prints_only(args: &[&str]) -> bool {
    let mut scripts = Vec::new();
    let mut iter = args.iter();
    let mut positional = Vec::new();
    while let Some(arg) = iter.next() {
        match *arg {
            "-n" | "--quiet" | "--silent" | "-E" | "-r" | "--regexp-extended" | "-s"
            | "--separate" | "-u" | "--unbuffered" | "-z" | "--null-data" | "--posix" => {}
            "-e" | "--expression" => match iter.next() {
                Some(script) => scripts.push(*script),
                None => return false,
            },
            arg if arg.starts_with("--expression=") => {
                scripts.push(&arg["--expression=".len()..]);
            }
            arg if arg.starts_with('-') && arg != "-" => return false,
            arg => positional.push(arg),
        }
    }
    if scripts.is_empty() {
        match positional.first() {
            Some(script) => scripts.push(script),
            None => return false,
        }
    }
    !scripts.is_empty() && scripts.iter().all(|script| sed_script_prints_only(script))
}

fn sed_script_prints_only(script: &str) -> bool {
    let script = script.trim_matches(['"', '\'']);
    if script.contains([';', '\n', '{', '}']) {
        return false;
    }
    // Addresses (`10`, `$`, `10,20`, `/re/`), then the command.
    let command = script
        .trim_start_matches(|c: char| c.is_ascii_digit() || matches!(c, ',' | '$' | ' ' | '!'));
    if command.is_empty() {
        return false;
    }
    if command.starts_with('/') {
        // `/regex/p` and `/a/,/b/p`.
        return command.ends_with('p') && !command.contains(" w") && !command.contains(" e");
    }
    match command.chars().next() {
        Some('p' | 'd' | '=' | 'q' | 'Q') => command.len() == 1,
        Some('s') => {
            let mut chars = command.chars().skip(1);
            let Some(delimiter) = chars.next() else {
                return false;
            };
            let rest: String = chars.collect();
            let mut parts = Vec::new();
            let mut current = String::new();
            let mut escaped = false;
            for c in rest.chars() {
                if escaped {
                    current.push(c);
                    escaped = false;
                } else if c == '\\' {
                    current.push(c);
                    escaped = true;
                } else if c == delimiter {
                    parts.push(std::mem::take(&mut current));
                } else {
                    current.push(c);
                }
            }
            parts.len() == 2
                && current
                    .chars()
                    .all(|flag| matches!(flag, 'g' | 'p' | 'i' | 'I' | '0'..='9'))
        }
        _ => false,
    }
}

fn git_subcommand<'a>(args: &'a [&'a str]) -> Option<(&'a str, &'a [&'a str])> {
    let mut rest = args;
    while let Some(first) = rest.first().filter(|a| a.starts_with('-')) {
        match *first {
            "-C" => rest = rest.get(2..)?,
            "--no-pager" | "-P" | "--no-optional-locks" | "--no-replace-objects" => {
                rest = &rest[1..]
            }
            _ => return None,
        }
    }
    let (sub, rest) = rest.split_first()?;
    Some((sub, rest))
}

const GIT_READ_ONLY: &[&str] = &[
    "status",
    "log",
    "diff",
    "show",
    "ls-files",
    "ls-tree",
    "rev-parse",
    "rev-list",
    "describe",
    "blame",
    "shortlog",
    "whatchanged",
    "cherry",
    "merge-base",
    "for-each-ref",
    "cat-file",
    "count-objects",
    "range-diff",
    "show-ref",
    "name-rev",
    "check-ignore",
    "check-attr",
    "version",
    "show-branch",
];

fn classify_git(args: &[&str]) -> Category {
    if matches!(args, ["--version" | "--help"]) {
        return Category::Safe;
    }
    let Some((sub, rest)) = git_subcommand(args) else {
        return Category::Unknown;
    };
    // An external diff tool, pager or output file runs or writes something.
    if rest.iter().any(|a| {
        a.starts_with("--output")
            || a.starts_with("--ext-diff")
            || a.starts_with("--textconv")
            || a.starts_with("--exec")
    }) {
        return Category::Unknown;
    }
    let first = rest.first().copied();
    match sub {
        "grep" => {
            if rest
                .iter()
                .take_while(|a| **a != "--")
                .any(|a| a.starts_with("-O") || a.starts_with("--open-files-in-pager"))
            {
                Category::Unknown
            } else {
                Category::Safe
            }
        }
        "branch" => {
            let listing = rest.is_empty()
                || rest.iter().all(|a| {
                    matches!(
                        *a,
                        "-a" | "--all"
                            | "-r"
                            | "--remotes"
                            | "-v"
                            | "-vv"
                            | "--verbose"
                            | "-l"
                            | "--list"
                            | "--show-current"
                            | "--merged"
                            | "--no-merged"
                            | "--contains"
                    ) || a.starts_with("--format")
                        || a.starts_with("--sort")
                });
            if listing {
                Category::Safe
            } else {
                Category::Git
            }
        }
        "tag"
            if rest.is_empty()
                || (has(rest, &["-l", "--list"])
                    && !has(
                        rest,
                        &[
                            "-d", "--delete", "-a", "-s", "-f", "--force", "-m", "-F", "-u",
                        ],
                    )) =>
        {
            Category::Safe
        }
        "remote"
            if rest.is_empty()
                || rest.iter().all(|a| matches!(*a, "-v" | "--verbose"))
                || first == Some("get-url") =>
        {
            Category::Safe
        }
        "stash" if matches!(first, Some("list" | "show")) => Category::Safe,
        "config"
            if has(
                rest,
                &["--get", "--get-all", "--get-regexp", "--list", "-l"],
            ) && !has(
                rest,
                &[
                    "--unset",
                    "--unset-all",
                    "--add",
                    "--replace-all",
                    "--rename-section",
                    "--remove-section",
                    "-e",
                    "--edit",
                ],
            ) =>
        {
            Category::Safe
        }
        "worktree" if first == Some("list") => Category::Safe,
        "submodule" if first == Some("status") => Category::Safe,
        "reflog" if first.is_none_or(|a| a == "show" || a.starts_with('-')) => Category::Safe,
        "notes" if matches!(first, Some("show" | "list")) => Category::Safe,
        sub if GIT_READ_ONLY.contains(&sub) => Category::Safe,
        _ => Category::Git,
    }
}

fn classify_build(name: &str, args: &[&str]) -> Category {
    let Some(sub) = args.first() else {
        return if name == "make" || name == "ninja" {
            Category::Build
        } else {
            Category::Unknown
        };
    };
    if name == "go" {
        if args.iter().any(|a| {
            a.starts_with("-exec") || a.starts_with("-toolexec") || a.starts_with("--exec")
        }) {
            return Category::Unknown;
        }
        match *sub {
            "version" => return Category::Safe,
            "env" => {
                return if has(args, &["-w", "-u"]) {
                    Category::Unknown
                } else {
                    Category::Safe
                };
            }
            "list" => return Category::Build,
            _ => {}
        }
    }
    if matches!(name, "cargo" | "rustc") && matches!(*sub, "--version" | "-V") {
        return Category::Safe;
    }
    match *sub {
        "build" | "test" | "bench" | "compile" | "lint" | "vet" | "fmt" | "check" | "clippy"
        | "doc" | "metadata" | "tree" => Category::Build,
        _ if name == "make" && !sub.starts_with('-') => Category::Build,
        _ => Category::Unknown,
    }
}

const PACKAGE_SCRIPTS: &[&str] = &[
    "test",
    "build",
    "lint",
    "check",
    "typecheck",
    "compile",
    "bench",
];

fn package_script(script: &str) -> bool {
    let base = script.split(':').next().unwrap_or(script);
    PACKAGE_SCRIPTS.contains(&base)
        && script
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || "_.:-".contains(c))
}

fn classify_package(name: &str, args: &[&str]) -> Category {
    let Some((sub, rest)) = args.split_first() else {
        return Category::Install;
    };
    if matches!(name, "npm" | "pnpm" | "yarn") {
        let script = match *sub {
            "run" | "run-script" => rest
                .first()
                .filter(|s| package_script(s))
                .map(|_| &rest[1..]),
            "test" | "t" | "tst" => Some(rest),
            s if name != "npm" && package_script(s) => Some(rest),
            _ => None,
        };
        if let Some(rest) = script {
            let flags_ok = rest
                .iter()
                .take_while(|a| **a != "--")
                .all(|a| !a.starts_with('-') || *a == "--if-present" || *a == "--silent");
            if flags_ok {
                return Category::Build;
            }
        }
    }
    if matches!(*sub, "--version" | "-v" | "--help") && rest.is_empty() {
        return Category::Safe;
    }
    match *sub {
        "list" | "ls" | "info" | "show" | "view" | "search" | "outdated" | "why" | "explain"
        | "freeze" | "doctor"
            if name != "yarn" =>
        {
            Category::Safe
        }
        "audit" if !rest.iter().any(|a| a.contains("fix")) => Category::Safe,
        "config" if matches!(rest.first(), Some(&("list" | "get"))) => Category::Safe,
        _ => Category::Install,
    }
}

fn classify_docker(args: &[&str]) -> Category {
    match args {
        [sub, ..]
            if matches!(
                *sub,
                "ps" | "images" | "inspect" | "logs" | "stats" | "info" | "version"
            ) =>
        {
            Category::Safe
        }
        [a, b, ..]
            if matches!(
                (*a, *b),
                ("network", "ls")
                    | ("network", "inspect")
                    | ("volume", "ls")
                    | ("volume", "inspect")
                    | ("compose", "ps")
                    | ("compose", "logs")
                    | ("context", "ls")
                    | ("system", "info")
                    | ("system", "df")
                    | ("image", "ls")
                    | ("image", "inspect")
                    | ("container", "ls")
                    | ("container", "inspect")
                    | ("container", "logs")
            ) =>
        {
            Category::Safe
        }
        _ => Category::Unknown,
    }
}

/// curl/wget that only GET to stdout.
fn classify_fetch(name: &str, args: &[&str]) -> Category {
    let mut to_stdout = name == "curl";
    let mut index = 0;
    while index < args.len() {
        let arg = args[index];
        let (flag, inline) = match arg.split_once('=') {
            Some((flag, value)) if flag.starts_with("--") => (flag, Some(value)),
            _ => (arg, None),
        };
        let mut value = || {
            inline.map(str::to_owned).or_else(|| {
                index += 1;
                args.get(index).map(|v| (*v).to_owned())
            })
        };
        match flag {
            "-s"
            | "-S"
            | "-sS"
            | "-f"
            | "-L"
            | "-sL"
            | "-fsSL"
            | "-sSL"
            | "-I"
            | "-i"
            | "-v"
            | "--silent"
            | "--show-error"
            | "--fail"
            | "--location"
            | "--head"
            | "--include"
            | "--compressed"
            | "-q"
            | "--quiet"
            | "--no-progress-meter" => {}
            "-X" | "--request" => {
                if !value().is_some_and(|v| {
                    v.eq_ignore_ascii_case("get") || v.eq_ignore_ascii_case("head")
                }) {
                    return Category::Unknown;
                }
            }
            "-H" | "--header" | "-A" | "--user-agent" | "-m" | "--max-time" => {
                if value().is_none_or(|v| v.starts_with('@')) {
                    return Category::Unknown;
                }
            }
            "-o" | "-O" | "--output" | "--output-document" | "-qO" => {
                let target = if flag == "-O" && name == "curl" {
                    None
                } else {
                    value()
                };
                if target.as_deref() != Some("-") {
                    return Category::Unknown;
                }
                to_stdout = true;
            }
            "-qO-" | "-O-" => to_stdout = true,
            flag if flag.starts_with('-') => return Category::Unknown,
            _ => {}
        }
        index += 1;
    }
    if to_stdout {
        Category::Safe
    } else {
        Category::Unknown
    }
}

/// Routine command groups a remembered approval may cover ("git" covers add/commit/push...).
pub struct Group {
    pub id: &'static str,
    pub label: &'static str,
    programs: &'static [&'static str],
    subcommands: &'static [&'static str],
    refused: &'static [&'static str],
    refused_short: &'static str,
}

pub const GROUPS: &[Group] = &[
    Group {
        id: "git",
        label: "git routine operations (add/commit/push/pull/switch...; not --force, reset, clean or checkout)",
        programs: &["git"],
        subcommands: &[
            "init",
            "add",
            "commit",
            "push",
            "pull",
            "fetch",
            "switch",
            "merge",
            "rebase",
            "stash",
            "branch",
            "tag",
            "cherry-pick",
            "mv",
            "status",
            "log",
            "diff",
            "show",
        ],
        refused: &[
            "--interactive",
            "--patch",
            "--edit",
            "--force",
            "--force-with-lease",
            "--force-if-includes",
            "--delete",
            "--mirror",
            "--prune",
            "--receive-pack",
            "--upload-pack",
            "--exec",
            "--discard-changes",
            "--edit-todo",
        ],
        refused_short: "ipefdDMCxc",
    },
    Group {
        id: "cargo",
        label: "cargo routine operations (build/test/check/run/fmt/clippy/add...; not install, publish or clean)",
        programs: &["cargo"],
        subcommands: &[
            "build", "test", "check", "run", "fmt", "clippy", "add", "update", "doc", "bench",
            "tree", "metadata",
        ],
        refused: &["--force"],
        refused_short: "",
    },
    Group {
        id: "npm",
        label: "npm/pnpm/yarn routine operations (install/run/test/build...; not publish, global installs or exec)",
        programs: &["npm", "pnpm", "yarn"],
        subcommands: &[
            "install",
            "i",
            "ci",
            "run",
            "test",
            "init",
            "uninstall",
            "remove",
            "build",
            "ls",
            "list",
            "outdated",
            "audit",
        ],
        refused: &["--force", "--global", "--prefix", "--location"],
        refused_short: "g",
    },
    Group {
        id: "go",
        label: "go routine operations (build/test/vet/run/mod/get...; not install, clean or env -w)",
        programs: &["go"],
        subcommands: &[
            "build", "test", "vet", "run", "fmt", "mod", "get", "generate", "list", "doc",
            "version",
        ],
        refused: &["-exec", "-toolexec"],
        refused_short: "",
    },
    Group {
        id: "dotnet",
        label: "dotnet routine operations (new/sln/add/restore/build/test/run...; not publish, tool or --force)",
        programs: &["dotnet"],
        subcommands: &[
            "new", "sln", "add", "remove", "restore", "build", "test", "run", "format", "clean",
            "pack", "list",
        ],
        refused: &["--force"],
        refused_short: "",
    },
];

pub fn group(id: &str) -> Option<&'static Group> {
    GROUPS.iter().find(|group| group.id == id)
}

impl Group {
    /// Whether one simple command's words are a routine operation of this group.
    pub fn covers(&self, words: &[&str]) -> bool {
        let Some(exe) = words.first() else {
            return false;
        };
        if exe.contains(['/', '\\']) || !self.programs.contains(&shell::program_name(exe).as_str())
        {
            return false;
        }
        let args = &words[1..];
        let (sub, rest) = if self.id == "git" {
            match git_subcommand(args) {
                Some(found) => found,
                None => return false,
            }
        } else {
            match args.split_first() {
                Some((sub, rest)) if !sub.starts_with('-') => (*sub, rest),
                _ => return false,
            }
        };
        if !self.subcommands.contains(&sub) {
            return false;
        }
        for arg in rest.iter().take_while(|a| **a != "--") {
            if let Some(long) = arg.strip_prefix("--") {
                let name = long.split('=').next().unwrap_or(long);
                if name.len() >= 2
                    && self.refused.iter().any(|r| {
                        r.trim_start_matches('-').starts_with(name)
                            || name.starts_with(r.trim_start_matches('-'))
                    })
                {
                    return false;
                }
            } else if let Some(short) = arg.strip_prefix('-') {
                if self.refused.contains(arg) {
                    return false;
                }
                // git commit -m/-F values are free text, not flags.
                if !short.is_empty()
                    && short.chars().all(|c| c.is_ascii_alphabetic())
                    && short.chars().any(|c| self.refused_short.contains(c))
                {
                    if self.id == "git" && sub == "commit" && !short.contains(['c', 'e', 'p', 'i'])
                    {
                        continue;
                    }
                    return false;
                }
            }
        }
        // `git push origin +main` is a force push.
        !(self.id == "git" && sub == "push" && rest.iter().any(|a| a.starts_with('+')))
    }
}

/// The group that covers every simple command of `command`, if one does.
pub fn line_group(command: &str, dialect: Dialect) -> Option<&'static Group> {
    let line = shell::parse(command, dialect);
    if !line.opaque.is_empty() || safety::catastrophic(command).is_some() {
        return None;
    }
    let commands: Vec<&Simple> = line.commands().collect();
    let first = commands.first()?;
    let program = shell::program_name(first.words.first()?.text.as_str());
    let group = GROUPS
        .iter()
        .find(|g| g.programs.contains(&program.as_str()))?;
    commands
        .iter()
        .all(|simple| {
            simple.redirects.is_empty()
                && simple.words.iter().all(|w| !w.dynamic)
                && group.covers(&simple.argv())
        })
        .then_some(group)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn posix(command: &str) -> Category {
        classify(command, Dialect::Posix)
    }

    #[test]
    fn reading_commands_from_real_sessions_need_no_approval() {
        for command in [
            "cd /d/repo/mt4proxy && cat CLAUDE.md; git ls-files | sed -n 80,237p; ls docs",
            "cd ~/.nuget/x && strings -n 5 a.dll | grep -iE \"journal|Log\" | sort -u | head -40",
            "cd /d/repo && timeout 100 grep -rIl \"LOG_TYPE_FULL\" Libraries mtconnector 2>/dev/null | head",
            "grep -rn \"Time\" a.cs | head -20; sed -n 1,80p c.cs",
            "sed 's/=.*/=***/' notes.txt",
            "sed -n '/fn main/,/^fn /p' src/main.rs",
            "sed -E 's#a#b#g' a.txt",
            "cygpath -w /c/Users",
            "ls -la \"$USERPROFILE/.cove\" 2>/dev/null | head -40; echo ---; ls \"$USERPROFILE/.cove/logs\" 2>/dev/null | tail -20",
            "cd \"$USERPROFILE/.cove\" && tail -c 4000 errors.log",
            "grep -n -i \"panic\" \"$USERPROFILE/.cove/errors.log\" | tail -20",
        ] {
            assert_eq!(posix(command), Category::Safe, "{command}");
        }
        for command in [
            "sed -i 's/a/b/' a.txt",
            "sed -i.bak 's/a/b/' a.txt",
            "sed 's/a/b/w out.txt' a.txt",
            "sed 's/a/b/e' a.txt",
            "sed -n '1p;w out' a.txt",
            "sed -f script.sed a.txt",
            "sed '1e rm -rf x' a.txt",
            "timeout 5 rm -rf build",
            "timeout 5",
            "env",
        ] {
            assert_ne!(posix(command), Category::Safe, "{command}");
        }
    }

    #[test]
    fn local_git_steps_need_no_approval_but_push_and_forcing_do() {
        let local = |command| local_git(command, Dialect::Posix);
        for command in [
            "git add -A",
            "git add src/a.rs && git commit -m 'fix: x'",
            "git commit -am wip",
            "git tag v1.2.0",
            "git tag -a v1.2.0 -m release",
            "git status && git add . && git commit -m x && git log --oneline -3",
        ] {
            assert!(local(command), "{command}");
        }
        for command in [
            "git push",
            "git commit -m x && git push origin main",
            "git push --tags",
            "git tag -d v1",
            "git tag -f v1",
            "git add -f build/out.txt",
            "git add -p",
            "git reset --hard",
            "git commit -m \"$(cat msg.txt)\"",
            "git commit -m x > log.txt",
            "git status",
            "rm -rf x && git add .",
        ] {
            assert!(!local(command), "{command}");
        }
    }

    #[test]
    fn categories_follow_the_classifier_rules() {
        assert_eq!(posix("ls -la && git status | head -5"), Category::Safe);
        assert_eq!(posix("cargo test --all"), Category::Build);
        assert_eq!(posix("npm run build"), Category::Build);
        assert_eq!(posix("npm install left-pad"), Category::Install);
        assert_eq!(posix("git commit -m x"), Category::Git);
        assert_eq!(posix("git status; cargo build"), Category::Build);
        assert_eq!(posix("echo hi > file.txt"), Category::Unknown);
        assert_eq!(posix("echo hi 2>/dev/null"), Category::Safe);
        assert_eq!(posix("cat $(which x)"), Category::Unknown);
        assert_eq!(posix("find . -name x -delete"), Category::Unknown);
        assert_eq!(posix("git diff --ext-diff"), Category::Unknown);
        assert_eq!(posix("rm -rf /"), Category::Dangerous);
        assert_eq!(posix("curl -s https://example.com"), Category::Safe);
        assert_eq!(posix("curl -o x https://example.com"), Category::Unknown);
        assert_eq!(
            posix(r#"certutil -hashfile "model.gguf" SHA256"#),
            Category::Safe
        );
        assert_eq!(posix("python -m pytest -q"), Category::Build);
        assert_eq!(posix("/bin/ls"), Category::Unknown);
    }

    #[test]
    fn powershell_lines() {
        let ps = |c| classify(c, Dialect::PowerShell);
        assert_eq!(
            ps("Get-ChildItem -Recurse | Select-String foo"),
            Category::Safe
        );
        assert_eq!(
            ps("Get-ChildItem | Where-Object { $_.Length -gt 1 }"),
            Category::Unknown
        );
        assert_eq!(ps("Set-Content x.txt hi"), Category::Unknown);
        assert_eq!(ps("dotnet test"), Category::Build);
        assert_eq!(ps("Get-Content a.txt > b.txt"), Category::Unknown);
    }

    #[test]
    fn read_only_and_auto_exclude_network() {
        assert!(read_only("git log -3", Dialect::Posix));
        assert!(!read_only("curl -s https://x", Dialect::Posix));
        assert!(!read_only("cargo test", Dialect::Posix));
        assert!(auto_approvable(
            "cargo test && go vet ./...",
            Dialect::Posix
        ));
        assert!(!auto_approvable("cargo install x", Dialect::Posix));
    }

    #[test]
    fn routine_groups() {
        assert_eq!(
            line_group("git add -A && git commit -m 'msg -f'", Dialect::Posix).map(|g| g.id),
            Some("git")
        );
        assert!(line_group("git push --force", Dialect::Posix).is_none());
        assert!(line_group("git push -f origin", Dialect::Posix).is_none());
        assert!(line_group("git push origin +main", Dialect::Posix).is_none());
        assert!(line_group("git reset --hard", Dialect::Posix).is_none());
        assert!(line_group("git add . && npm test", Dialect::Posix).is_none());
        assert_eq!(
            line_group("npm install", Dialect::Posix).map(|g| g.id),
            Some("npm")
        );
        assert!(line_group("npm install -g x", Dialect::Posix).is_none());
    }
}
