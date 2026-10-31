//! `policies.json` rules: allow, deny or ask by tool and input.

use std::{collections::BTreeMap, path::Path};

use serde::{Deserialize, Serialize};

use super::{
    Effect, classify, safety,
    shell::{self, Dialect},
};
use crate::domain::{Error, Result};

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Action {
    Allow,
    Deny,
    Ask,
}

fn enabled() -> bool {
    true
}

#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct PolicyRule {
    #[serde(default)]
    pub id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub description: String,
    /// Glob over tool names: `shell` (also `bash`, `powershell`), `write`/`edit`, `web_fetch`/`web_search`.
    #[serde(default)]
    pub tool_pattern: String,
    pub action: Option<Action>,
    /// Higher wins between allow and ask rules.
    #[serde(default)]
    pub priority: i64,
    #[serde(default = "enabled")]
    pub enabled: bool,
    /// Field glob patterns: `command`, `path`, `url`.
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub param_match: BTreeMap<String, String>,
    /// Shell: the command starts with these words (each simple command, for allow).
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub command_prefix: String,
    /// Shell: the complete command line must match exactly (for explicit approvals).
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub command_exact: String,
    /// Shell: local model completion endpoint plus its fixed output file.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub command_endpoint: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub command_output: String,
    /// Shell: read-only help/version probes of executables in one directory.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub command_directory: String,
    /// Shell: invocations of one executable at a fixed path.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub command_executable: String,
    /// Shell: a routine group (`git`, `cargo`, `npm`, `go`, `dotnet`).
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub command_group: String,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub input_equals: BTreeMap<String, String>,
    /// Project root the rule is limited to; empty applies everywhere.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub scope: String,
}

/// Tool names an effect is checked as.
fn tools(effect: &Effect) -> &'static [&'static str] {
    match effect {
        Effect::Shell(_) => &["shell", "bash", "powershell"],
        Effect::Write(_) => &["write", "edit"],
        Effect::Network(_) => &["web_fetch", "web_search", "webfetch", "network"],
        Effect::Process(_) => &["mcp", "hook", "process"],
        Effect::Read => &["read"],
    }
}

fn field(effect: &Effect, name: &str) -> Option<String> {
    match (effect, name) {
        (Effect::Shell(command), "command") => Some(command.clone()),
        (Effect::Write(path), "path" | "file_path" | "filePath" | "file") => {
            Some(path.display().to_string())
        }
        (Effect::Network(request), "url" | "query" | "request") => Some(request.clone()),
        (Effect::Process(process), "command" | "process") => Some(process.clone()),
        _ => None,
    }
}

/// Case-insensitive glob with `*` and `?`.
pub fn glob(pattern: &str, value: &str) -> bool {
    let pattern: Vec<char> = pattern.to_lowercase().chars().collect();
    let value: Vec<char> = value.to_lowercase().chars().collect();
    let (mut p, mut v) = (0, 0);
    let (mut star, mut mark) = (None, 0);
    while v < value.len() {
        if p < pattern.len() && (pattern[p] == '?' || pattern[p] == value[v]) {
            p += 1;
            v += 1;
        } else if p < pattern.len() && pattern[p] == '*' {
            star = Some(p);
            mark = v;
            p += 1;
        } else if let Some(s) = star {
            p = s + 1;
            mark += 1;
            v = mark;
        } else {
            return false;
        }
    }
    while p < pattern.len() && pattern[p] == '*' {
        p += 1;
    }
    p == pattern.len()
}

pub fn same_project(a: &Path, b: &Path) -> bool {
    if cfg!(windows) {
        a.to_string_lossy()
            .trim_end_matches(['\\', '/'])
            .eq_ignore_ascii_case(b.to_string_lossy().trim_end_matches(['\\', '/']))
    } else {
        a == b
    }
}

/// Simple commands of a shell line in both dialects, runner-stripped, lowercase program.
fn simple_commands(command: &str) -> Vec<Vec<String>> {
    let mut out = Vec::new();
    for dialect in [Dialect::Posix, Dialect::PowerShell] {
        let line = shell::parse(command, dialect);
        for simple in line.commands() {
            let argv: Vec<String> = simple.argv().iter().map(|w| (*w).to_owned()).collect();
            let words = shell::strip_runners(&argv).to_vec();
            if !words.is_empty() && !out.contains(&words) {
                out.push(words);
            }
        }
        for inner in &line.substitutions {
            out.extend(simple_commands(inner));
        }
    }
    out
}

fn has_prefix(words: &[String], prefix: &[&str]) -> bool {
    words.len() >= prefix.len()
        && words
            .iter()
            .zip(prefix)
            .enumerate()
            .all(|(index, (word, wanted))| {
                if index == 0 {
                    shell::program_name(word) == shell::program_name(wanted)
                } else {
                    word.eq_ignore_ascii_case(wanted)
                }
            })
}

impl PolicyRule {
    pub fn action(&self) -> Action {
        self.action.unwrap_or(Action::Ask)
    }

    fn tool_pattern(&self) -> &str {
        if self.tool_pattern.is_empty()
            && (!self.command_prefix.is_empty() || !self.command_group.is_empty())
        {
            "shell"
        } else {
            &self.tool_pattern
        }
    }

    fn applies(&self, effect: &Effect, project: &Path) -> bool {
        self.enabled
            && (self.scope.is_empty() || same_project(Path::new(&self.scope), project))
            && !self.tool_pattern().is_empty()
            && tools(effect)
                .iter()
                .any(|tool| glob(self.tool_pattern(), tool))
    }

    /// A pooled allow rule: covers shell lines command by command.
    fn pooled(&self) -> bool {
        !self.command_prefix.is_empty() || !self.command_group.is_empty()
    }

    /// Covers one simple command (allow pooling).
    fn covers(&self, words: &[&str]) -> bool {
        if !self.command_prefix.is_empty() {
            let prefix: Vec<&str> = self.command_prefix.split_whitespace().collect();
            let owned: Vec<String> = words.iter().map(|w| (*w).to_owned()).collect();
            return has_prefix(&owned, &prefix);
        }
        classify::group(&self.command_group).is_some_and(|group| group.covers(words))
    }

    /// Whether this (non-pooled, or deny/ask) rule matches the effect.
    fn matches(&self, effect: &Effect, project: &Path, dialect: Dialect) -> bool {
        if !self.applies(effect, project) {
            return false;
        }
        let action = self.action();
        if let Effect::Shell(command) = effect {
            if !self.command_executable.is_empty()
                && external_executable(command, dialect, Path::new(&self.scope)).as_deref()
                    != Some(self.command_executable.as_str())
            {
                return false;
            }
            if !self.command_directory.is_empty()
                && external_help_directory(command, dialect).as_deref()
                    != Some(self.command_directory.as_str())
            {
                return false;
            }
            if !self.command_endpoint.is_empty()
                && !local_completion_request(
                    command,
                    dialect,
                    &self.command_endpoint,
                    &self.command_output,
                )
            {
                return false;
            }
            if !self.command_exact.is_empty() && command != &self.command_exact {
                return false;
            }
            if !self.command_prefix.is_empty() {
                let prefix: Vec<&str> = self.command_prefix.split_whitespace().collect();
                return simple_commands(command)
                    .iter()
                    .any(|words| has_prefix(words, &prefix))
                    || (action != Action::Allow
                        && command
                            .to_lowercase()
                            .contains(&self.command_prefix.to_lowercase()));
            }
            if !self.command_group.is_empty() {
                let Some(group) = classify::group(&self.command_group) else {
                    return false;
                };
                return simple_commands(command).iter().any(|words| {
                    let words: Vec<&str> = words.iter().map(String::as_str).collect();
                    group.covers(&words)
                });
            }
        }
        for (name, wanted) in &self.input_equals {
            if field(effect, name).as_deref() != Some(wanted.as_str()) {
                return false;
            }
        }
        for (name, pattern) in &self.param_match {
            let Some(value) = field(effect, name) else {
                return false;
            };
            if let (Effect::Shell(command), "command") = (effect, name.as_str()) {
                let commands = simple_commands(command);
                let ok = if action == Action::Allow {
                    // Every command of the line must match, and the line must be judgeable.
                    let line = shell::parse(command, dialect);
                    line.opaque.is_empty()
                        && !commands.is_empty()
                        && commands.iter().all(|words| glob(pattern, &words.join(" ")))
                } else {
                    glob(pattern, command)
                        || commands.iter().any(|words| glob(pattern, &words.join(" ")))
                };
                if !ok {
                    return false;
                }
                continue;
            }
            if !glob(pattern, &value) {
                return false;
            }
        }
        true
    }
}

/// Outcome of rule evaluation.
#[derive(Debug, PartialEq, Eq)]
pub enum RuleDecision {
    Deny,
    Ask,
    Allow,
    None,
}

pub fn deny(rules: &[PolicyRule], effect: &Effect, project: &Path, dialect: Dialect) -> bool {
    rules
        .iter()
        .any(|rule| rule.action() == Action::Deny && rule.matches(effect, project, dialect))
}

/// Allow and ask rules by priority; ask wins ties.
pub fn allow_or_ask(
    rules: &[PolicyRule],
    effect: &Effect,
    project: &Path,
    dialect: Dialect,
) -> RuleDecision {
    let ask = rules
        .iter()
        .filter(|rule| rule.action() == Action::Ask && rule.matches(effect, project, dialect))
        .map(|rule| rule.priority)
        .max();
    let mut allow = rules
        .iter()
        .filter(|rule| {
            rule.action() == Action::Allow
                && !rule.pooled()
                && rule.matches(effect, project, dialect)
        })
        .map(|rule| rule.priority)
        .max();
    if let Effect::Shell(command) = effect {
        let pool: Vec<&PolicyRule> = rules
            .iter()
            .filter(|rule| {
                rule.action() == Action::Allow && rule.pooled() && rule.applies(effect, project)
            })
            .collect();
        if !pool.is_empty() {
            let line = shell::parse(command, dialect);
            let covered = line.opaque.is_empty()
                && line.pipelines.iter().all(|p| !p.is_empty())
                && line.commands().next().is_some()
                && line.commands().all(|simple| {
                    let argv = simple.argv();
                    simple.redirects.iter().all(|r| {
                        r.target.starts_with('&')
                            || r.target.eq_ignore_ascii_case("/dev/null")
                            || r.target.eq_ignore_ascii_case("$null")
                    }) && !simple.words.iter().any(|w| w.dynamic)
                        && pool.iter().any(|rule| rule.covers(&argv))
                });
            if covered {
                let priority = pool.iter().map(|rule| rule.priority).min().unwrap_or(0);
                allow = Some(allow.map_or(priority, |p| p.max(priority)));
            }
        }
    }
    match (ask, allow) {
        (Some(ask), Some(allow)) if allow > ask => RuleDecision::Allow,
        (Some(_), _) => RuleDecision::Ask,
        (None, Some(_)) => RuleDecision::Allow,
        (None, None) => RuleDecision::None,
    }
}

/// The rule "remember for this project" appends, when the effect has a reasonable one.
pub fn remember_rule(effect: &Effect, project: &Path, dialect: Dialect) -> Option<PolicyRule> {
    let scope = project.display().to_string();
    let mut rule = PolicyRule {
        action: Some(Action::Allow),
        enabled: true,
        scope,
        ..PolicyRule::default()
    };
    match effect {
        Effect::Shell(command) => {
            rule.tool_pattern = "shell".into();
            if let Some(group) = classify::line_group(command, dialect) {
                rule.command_group = group.id.into();
                rule.id = format!("allow-shell-group-{}", group.id);
                rule.description = group.label.into();
                return Some(rule);
            }
            if let Some((endpoint, output)) = local_completion(command, dialect) {
                use sha2::{Digest, Sha256};
                let digest = Sha256::digest(format!("{endpoint}\n{output}").as_bytes());
                let suffix: String = digest.iter().map(|byte| format!("{byte:02x}")).collect();
                rule.command_endpoint = endpoint;
                rule.command_output = output;
                rule.id = format!("allow-shell-local-completion-{suffix}");
                rule.description = "local model completion request".into();
                return Some(rule);
            }
            if let Some(directory) = external_help_directory(command, dialect) {
                use sha2::{Digest, Sha256};
                let digest = Sha256::digest(directory.as_bytes());
                let suffix: String = digest.iter().map(|byte| format!("{byte:02x}")).collect();
                rule.command_directory = directory;
                rule.id = format!("allow-shell-help-directory-{suffix}");
                rule.description = "read-only help and version queries in one directory".into();
                return Some(rule);
            }
            if let Some(executable) = external_executable(command, dialect, project) {
                use sha2::{Digest, Sha256};
                let digest = Sha256::digest(executable.as_bytes());
                let suffix: String = digest.iter().map(|byte| format!("{byte:02x}")).collect();
                rule.command_executable = executable;
                rule.id = format!("allow-shell-executable-{suffix}");
                rule.description = "one external executable".into();
                return Some(rule);
            }
            let line = shell::parse(command, dialect);
            let commands: Vec<_> = line.commands().collect();
            let category = classify::classify(command, dialect);
            if matches!(category, classify::Category::Dangerous) {
                return None;
            }
            if !line.opaque.is_empty()
                || commands.is_empty()
                || commands
                    .iter()
                    .any(|simple| simple.words.iter().any(|word| word.dynamic))
            {
                // Parsing uncertainty must never create a reusable family rule, but an
                // explicit user choice may remember this exact line if it has no secrets.
                return exact_shell_rule(&mut rule, command);
            }
            if commands.len() != 1
                || commands[0]
                    .words
                    .first()
                    .is_some_and(|word| word.text.contains(['/', '\\']))
            {
                return exact_shell_rule(&mut rule, command);
            }
            let words = commands[0].argv();
            if words.iter().any(|w| w.contains(['/', '\\'])) && words.len() == 1 {
                return None;
            }
            if !matches!(
                category,
                classify::Category::Safe | classify::Category::Build
            ) {
                return exact_shell_rule(&mut rule, command);
            }
            // Program and its subcommand: `make test`, `cargo nextest`, `pytest`.
            let mut prefix = vec![words[0]];
            if let Some(sub) = words
                .get(1)
                .filter(|s| !s.starts_with('-') && !s.contains(['/', '\\', '.', '=']))
            {
                prefix.push(sub);
            }
            rule.command_prefix = prefix.join(" ");
            rule.id = format!("allow-shell-{}", rule.command_prefix);
            Some(rule)
        }
        Effect::Write(_) => {
            rule.tool_pattern = "write".into();
            rule.id = "allow-write".into();
            rule.description = "file writes in this project".into();
            Some(rule)
        }
        Effect::Network(_) => {
            rule.tool_pattern = "web_*".into();
            rule.id = "allow-web".into();
            rule.description = "network access from web tools".into();
            Some(rule)
        }
        Effect::Process(_) | Effect::Read => None,
    }
}

/// Exact-line memory is safe for opaque commands because it never generalizes their
/// parsed structure. Avoid storing likely credentials or excessively large scripts.
fn exact_shell_rule(rule: &mut PolicyRule, command: &str) -> Option<PolicyRule> {
    if command.is_empty() || command.len() > 8192 || !safety::secrets(command).is_empty() {
        return None;
    }
    use sha2::{Digest, Sha256};
    let digest = Sha256::digest(command.as_bytes());
    let suffix: String = digest.iter().map(|byte| format!("{byte:02x}")).collect();
    rule.command_exact = command.to_owned();
    rule.id = format!("allow-shell-exact-{suffix}");
    rule.description = "exact shell command".into();
    Some(rule.clone())
}

/// Recognizes a narrowly scoped local inference request. The prompt body may change,
/// while the loopback endpoint and output path remain fixed. No other shell steps,
/// headers, methods, or output destinations are covered.
fn local_completion(command: &str, dialect: Dialect) -> Option<(String, String)> {
    let line = shell::parse(command, dialect);
    if !line.opaque.is_empty()
        || !(line.pipelines.len() == 1 || line.pipelines.len() == 2)
        || line.pipelines.iter().any(|pipeline| pipeline.len() != 1)
    {
        return None;
    }
    let request = &line.pipelines[0][0];
    if request.words.iter().any(|word| word.dynamic) {
        return None;
    }
    let words = request.argv();
    if words
        .first()
        .is_none_or(|word| shell::program_name(word) != "curl")
    {
        return None;
    }
    let mut endpoint = None;
    let mut json_body = None;
    let mut index = 1;
    while index < words.len() {
        match words[index] {
            "-s" | "--silent" => index += 1,
            "-H" | "--header" => {
                let value = *words.get(index + 1)?;
                if !value.eq_ignore_ascii_case("content-type: application/json") {
                    return None;
                }
                index += 2;
            }
            "-d" | "--data" | "--data-raw" => {
                if json_body.replace(*words.get(index + 1)?).is_some() {
                    return None;
                }
                index += 2;
            }
            value if value.starts_with("http://") => {
                if endpoint.replace(value).is_some() {
                    return None;
                }
                index += 1;
            }
            _ => return None,
        }
    }
    let body = json_body?;
    if !serde_json::from_str::<serde_json::Value>(body)
        .ok()?
        .is_object()
    {
        return None;
    }
    let url = url::Url::parse(endpoint?).ok()?;
    let loopback = match url.host()? {
        url::Host::Ipv4(ip) => ip.is_loopback(),
        url::Host::Ipv6(ip) => ip.is_loopback(),
        url::Host::Domain(domain) => domain.eq_ignore_ascii_case("localhost"),
    };
    if !loopback
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || !matches!(
            url.path(),
            "/completion" | "/v1/completions" | "/v1/chat/completions"
        )
    {
        return None;
    }
    let output = match request.redirects.as_slice() {
        [redirect] if redirect.op == ">" && !redirect.target.is_empty() => redirect.target.clone(),
        _ => return None,
    };
    if line.pipelines.len() == 2 {
        let readback = &line.pipelines[1][0];
        if readback.words.iter().any(|word| word.dynamic)
            || readback.argv().as_slice() != ["cat", output.as_str()]
            || !readback.redirects.is_empty()
        {
            return None;
        }
    }
    Some((
        format!("{}{}", url.origin().ascii_serialization(), url.path()),
        output,
    ))
}

fn local_completion_request(command: &str, dialect: Dialect, endpoint: &str, output: &str) -> bool {
    local_completion(command, dialect).is_some_and(|(actual_endpoint, actual_output)| {
        actual_endpoint == endpoint && actual_output == output
    })
}

/// Finds an external executable's directory when a line only asks for its help or
/// version and optionally filters that output. No executable arguments are remembered.
fn external_help_directory(command: &str, dialect: Dialect) -> Option<String> {
    let line = shell::parse(command, dialect);
    if !line.opaque.is_empty() || line.pipelines.is_empty() {
        return None;
    }
    let mut working_dir = None;
    let mut executable_dir = None;
    let mut executable_count = 0;
    for simple in line.commands() {
        if simple.words.iter().any(|word| word.dynamic) {
            return None;
        }
        let words = simple.argv();
        let Some(program) = words.first() else {
            return None;
        };
        let name = shell::program_name(program);
        if matches!(name.as_str(), "cd" | "set-location" | "sl") {
            if working_dir.is_some() || words.len() != 2 || !simple.redirects.is_empty() {
                return None;
            }
            working_dir = Some(normalize_dir(words[1], None)?);
            continue;
        }
        if program.contains(['/', '\\']) {
            executable_count += 1;
            if executable_count > 1
                || words.len() != 2
                || !matches!(words[1], "--help" | "-h" | "-?" | "--version" | "-V")
                || simple.redirects.iter().any(|redirect| {
                    !(redirect.op == ">&"
                        && (redirect.target.starts_with('&') || redirect.target == "-"))
                })
            {
                return None;
            }
            let parent = Path::new(program).parent()?.to_string_lossy().to_string();
            executable_dir = Some(if program.starts_with("./") || program.starts_with(".\\") {
                normalize_dir(&parent, working_dir.as_deref())?
            } else {
                normalize_dir(&parent, None)?
            });
            continue;
        }
        if !matches!(
            name.as_str(),
            "head"
                | "tail"
                | "grep"
                | "egrep"
                | "fgrep"
                | "sed"
                | "tr"
                | "cut"
                | "sort"
                | "uniq"
                | "wc"
        ) || classify::classify_words(&words) != classify::Category::Safe
            || !simple.redirects.is_empty()
        {
            return None;
        }
    }
    if executable_count != 1 {
        return None;
    }
    let executable_dir = executable_dir?;
    if working_dir
        .as_deref()
        .is_some_and(|cwd| !same_dir(cwd, &executable_dir))
    {
        return None;
    }
    Some(executable_dir)
}

fn normalize_dir(path: &str, base: Option<&str>) -> Option<String> {
    let path = path.replace('\\', "/");
    let absolute = path.starts_with('/')
        || path
            .as_bytes()
            .get(1..3)
            .is_some_and(|drive| drive[0] == b':' && drive[1] == b'/');
    if !absolute && base.is_none() {
        return None;
    }
    let combined = if absolute {
        path
    } else {
        format!("{}/{}", base.unwrap().trim_end_matches('/'), path)
    };
    let mut parts = Vec::new();
    for part in combined.split('/') {
        match part {
            "" | "." => {}
            ".." => {
                parts.pop()?;
            }
            part => parts.push(part),
        }
    }
    let mut normalized = format!("/{}", parts.join("/"));
    if cfg!(windows) {
        normalized = normalized.to_ascii_lowercase();
    }
    Some(normalized)
}

fn same_dir(left: &str, right: &str) -> bool {
    if cfg!(windows) {
        left.eq_ignore_ascii_case(right)
    } else {
        left == right
    }
}

/// Identifies a single external executable invocation, optionally after one literal
/// `cd`. No compound shell work, pipelines, redirects, or dynamic arguments are saved.
fn external_executable(command: &str, dialect: Dialect, project: &Path) -> Option<String> {
    let line = shell::parse(command, dialect);
    if !line.opaque.is_empty()
        || !(line.pipelines.len() == 1 || line.pipelines.len() == 2)
        || line.pipelines.iter().any(|pipeline| pipeline.len() != 1)
    {
        return None;
    }
    let commands: Vec<_> = line.commands().collect();
    let (program_command, cwd) = match commands.as_slice() {
        [command] => (*command, project.to_string_lossy().to_string()),
        [change_dir, command]
            if change_dir.argv().first().is_some_and(|program| {
                matches!(
                    shell::program_name(program).as_str(),
                    "cd" | "set-location" | "sl"
                )
            }) && change_dir.words.iter().all(|word| !word.dynamic)
                && change_dir.words.len() == 2
                && change_dir.redirects.is_empty() =>
        {
            (*command, change_dir.argv()[1].to_owned())
        }
        _ => return None,
    };
    if !program_command.redirects.is_empty()
        || program_command.words.iter().any(|word| word.dynamic)
    {
        return None;
    }
    let words = program_command.argv();
    let program = *words.first()?;
    if !program.contains(['/', '\\']) {
        return None;
    }
    let basename = program.rsplit(['/', '\\']).next()?;
    let lower_basename = basename.to_ascii_lowercase();
    let executable_name = shell::program_name(basename);
    if matches!(
        executable_name.as_str(),
        "sh" | "bash"
            | "dash"
            | "zsh"
            | "fish"
            | "cmd"
            | "powershell"
            | "pwsh"
            | "python"
            | "python3"
            | "py"
            | "node"
            | "deno"
            | "bun"
            | "ruby"
            | "perl"
            | "php"
            | "lua"
            | "wscript"
            | "cscript"
            | "rundll32"
            | "regsvr32"
            | "mshta"
            | "env"
            | "busybox"
            | "sudo"
            | "doas"
    ) || (cfg!(windows)
        && !lower_basename.ends_with(".exe")
        && !lower_basename.ends_with(".com"))
        || [".bat", ".cmd", ".ps1", ".sh"]
            .iter()
            .any(|suffix| lower_basename.ends_with(suffix))
    {
        return None;
    }
    let parent = Path::new(program).parent()?.to_string_lossy().to_string();
    let directory = if program.starts_with("./") || program.starts_with(".\\") {
        normalize_dir(&parent, Some(&normalize_dir(&cwd, None)?))?
    } else if program.starts_with('/')
        || program
            .as_bytes()
            .get(1..3)
            .is_some_and(|drive| drive[0] == b':' && drive[1] == b'/' || drive[1] == b'\\')
    {
        normalize_dir(&parent, None)?
    } else {
        normalize_dir(&parent, Some(&normalize_dir(&cwd, None)?))?
    };
    Some(format!("{directory}/{basename}"))
}

pub fn load(path: &Path) -> Result<Vec<PolicyRule>> {
    if !path.is_file() {
        return Ok(Vec::new());
    }
    let data = std::fs::read(path)?;
    let data = data.strip_prefix(b"\xEF\xBB\xBF").unwrap_or(&data);
    serde_json::from_slice(data)
        .map_err(|err| Error::Session(format!("invalid {}: {err}", path.display())))
}

/// Adds or replaces (same ID and scope) `rule` in the file.
pub fn append(path: &Path, rule: &PolicyRule) -> Result<Vec<PolicyRule>> {
    let mut rules = load(path)?;
    match rules
        .iter_mut()
        .find(|r| r.id == rule.id && same_project(Path::new(&r.scope), Path::new(&rule.scope)))
    {
        Some(existing) => *existing = rule.clone(),
        None => rules.push(rule.clone()),
    }
    crate::session::atomic_json(path, &rules)?;
    Ok(rules)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn rule(json: serde_json::Value) -> PolicyRule {
        serde_json::from_value(json).unwrap()
    }

    fn shell(command: &str) -> Effect {
        Effect::Shell(command.into())
    }

    #[test]
    fn glob_matches_case_insensitively() {
        assert!(glob("npm *", "NPM install"));
        assert!(glob("web_*", "web_fetch"));
        assert!(glob("a?c", "abc"));
        assert!(!glob("git push*", "git status"));
    }

    #[test]
    fn deny_rules_see_every_command_of_a_line() {
        let project = Path::new("/p");
        let rules = vec![rule(
            serde_json::json!({"tool_pattern":"bash","action":"deny","command_prefix":"git push"}),
        )];
        assert!(deny(
            &rules,
            &shell("git add . && git push origin"),
            project,
            Dialect::Posix
        ));
        assert!(deny(
            &rules,
            &shell("bash -c 'git push'"),
            project,
            Dialect::Posix
        ));
        assert!(!deny(&rules, &shell("git status"), project, Dialect::Posix));
        let rules = vec![rule(
            serde_json::json!({"tool_pattern":"shell","action":"deny","param_match":{"command":"*secret*"}}),
        )];
        assert!(deny(
            &rules,
            &shell("cat secret.txt"),
            project,
            Dialect::Posix
        ));
    }

    #[test]
    fn allow_rules_must_cover_the_whole_line() {
        let project = Path::new("/p");
        let rules = vec![
            rule(serde_json::json!({"action":"allow","command_prefix":"make"})),
            rule(serde_json::json!({"action":"allow","command_group":"git"})),
        ];
        let decide = |c| allow_or_ask(&rules, &shell(c), project, Dialect::Posix);
        assert_eq!(
            decide("make test && git add -A && git commit -m x"),
            RuleDecision::Allow
        );
        assert_eq!(decide("make test && rm -rf build"), RuleDecision::None);
        assert_eq!(decide("make $(id)"), RuleDecision::None);
        assert_eq!(decide("git push --force"), RuleDecision::None);
        let mut rules = rules;
        rules.push(rule(serde_json::json!({"tool_pattern":"shell","action":"ask","priority":5,"command_prefix":"git commit"})));
        assert_eq!(
            allow_or_ask(&rules, &shell("git commit -m x"), project, Dialect::Posix),
            RuleDecision::Ask
        );
    }

    #[test]
    fn scope_limits_rules_to_a_project() {
        let rules = vec![rule(
            serde_json::json!({"tool_pattern":"write","action":"allow","scope":"/p"}),
        )];
        let effect = Effect::Write("/p/a.txt".into());
        assert_eq!(
            allow_or_ask(&rules, &effect, Path::new("/p"), Dialect::Posix),
            RuleDecision::Allow
        );
        assert_eq!(
            allow_or_ask(&rules, &effect, Path::new("/q"), Dialect::Posix),
            RuleDecision::None
        );
    }

    #[test]
    fn remembered_rules_and_storage() {
        let dir = tempfile::tempdir().unwrap();
        let project = dir.path();
        let git = remember_rule(
            &shell("git add -A && git commit -m x"),
            project,
            Dialect::Posix,
        )
        .unwrap();
        assert_eq!(git.command_group, "git");
        let make = remember_rule(&shell("make test -j4"), project, Dialect::Posix).unwrap();
        assert_eq!(make.command_prefix, "make test");
        assert!(remember_rule(&shell("rm -rf / "), project, Dialect::Posix).is_none());
        assert_eq!(
            remember_rule(&shell("a && b"), project, Dialect::Posix)
                .unwrap()
                .command_exact,
            "a && b"
        );
        let path = dir.path().join("policies.json");
        append(&path, &make).unwrap();
        let rules = append(&path, &make).unwrap();
        assert_eq!(rules.len(), 1);
        assert_eq!(load(&path).unwrap(), rules);
    }

    #[test]
    fn executable_help_approval_covers_only_read_only_queries_in_the_same_directory() {
        let project = Path::new("/p");
        let command = r#"cd "/d/tools" && ./llama-server.exe --help 2>&1 | grep -i temp"#;
        let same_dir = r#"cd "/d/tools" && ./llama-bench.exe --help 2>&1 | head -200"#;
        let rule = remember_rule(&shell(command), project, Dialect::Posix).unwrap();
        assert_eq!(rule.command_directory, "/d/tools");
        let rules = vec![rule];
        assert_eq!(
            allow_or_ask(&rules, &shell(same_dir), project, Dialect::Posix),
            RuleDecision::Allow
        );
        assert_eq!(
            allow_or_ask(
                &rules,
                &shell(r#"cd "/d/other" && ./llama-bench.exe --help 2>&1 | head -200"#),
                project,
                Dialect::Posix
            ),
            RuleDecision::None
        );
        assert_eq!(
            allow_or_ask(
                &rules,
                &shell(same_dir),
                Path::new("/other"),
                Dialect::Posix
            ),
            RuleDecision::None
        );
        for denied in [
            r#"cd "/d/tools" && ./llama-bench.exe --models"#,
            r#"cd "/d/tools" && ./llama-bench.exe --help; rm x"#,
            r#"cd "/d/tools" && ./llama-bench.exe --help > result.txt"#,
            r#"cd "/d/tools" && ./llama-bench.exe --help $FLAGS"#,
        ] {
            assert_eq!(
                allow_or_ask(&rules, &shell(denied), project, Dialect::Posix),
                RuleDecision::None,
                "{denied}"
            );
        }

        // Opaque control flow and dynamic variables never get a reusable approval.
        let loop_rule = remember_rule(
            &shell("for ub in 128 256; do ./llama-fit-params -ub $ub; done"),
            project,
            Dialect::Posix,
        )
        .unwrap();
        assert!(loop_rule.command_prefix.is_empty());
        assert_eq!(
            loop_rule.command_exact,
            "for ub in 128 256; do ./llama-fit-params -ub $ub; done"
        );
        assert!(
            remember_rule(
                &shell(r#""/d/tool.exe" --token=sk-abcdefghijklmnopqrstuvwxyz0123456789 | grep x"#),
                project,
                Dialect::Posix
            )
            .is_none()
        );
    }

    #[test]
    fn remembered_native_executable_scope_is_path_bound_and_excludes_shell_scripts() {
        let project = Path::new("/p");
        let first =
            r#"cd "/d/llama" && ./llama-fit-params.exe -m models/Qwen.gguf -c 12288 -ub 128"#;
        let varied =
            r#"cd "/d/llama" && ./llama-fit-params.exe -m models/Qwen.gguf -c 12288 -ub 256"#;
        let rule = remember_rule(&shell(first), project, Dialect::Posix).unwrap();
        assert_eq!(rule.command_executable, "/d/llama/llama-fit-params.exe");
        let rules = vec![rule];
        assert_eq!(
            allow_or_ask(&rules, &shell(varied), project, Dialect::Posix),
            RuleDecision::Allow
        );
        for denied in [
            r#"cd "/d/llama" && ./llama-other.exe -m models/Qwen.gguf -ub 256"#,
            r#"cd "/d/other" && ./llama-fit-params.exe -m models/Qwen.gguf -ub 256"#,
            r#"cd "/d/llama" && ./llama-fit-params.exe -ub $UB"#,
            r#"cd "/d/llama" && ./llama-fit-params.exe -ub 256 > result.txt"#,
            r#"cd "/d/llama" && ./llama-fit-params.exe -ub 256 | head"#,
        ] {
            assert_eq!(
                allow_or_ask(&rules, &shell(denied), project, Dialect::Posix),
                RuleDecision::None,
                "{denied}"
            );
        }
        for interpreter in [
            r#"cd "/d/llama" && ./run-params.sh -ub 128"#,
            r#"cd "/d/llama" && ./python.exe -c "print('hi')""#,
        ] {
            let rule = remember_rule(&shell(interpreter), project, Dialect::Posix).unwrap();
            assert!(rule.command_executable.is_empty());
            assert_eq!(rule.command_exact, interpreter);
        }
    }

    #[test]
    fn opaque_shell_commands_can_only_be_remembered_as_the_exact_secret_free_line() {
        let project = Path::new("/p");
        let command = r#"Get-WinEvent -LogName Application | Where-Object { $_.Message -match 'cove' } | Select-Object -First 10"#;
        let rule = remember_rule(&shell(command), project, Dialect::PowerShell).unwrap();
        assert_eq!(rule.command_exact, command);
        assert_eq!(rule.command_prefix, "");
        assert_eq!(rule.command_executable, "");
        let rules = vec![rule];
        assert_eq!(
            allow_or_ask(&rules, &shell(command), project, Dialect::PowerShell),
            RuleDecision::Allow
        );
        assert_eq!(
            allow_or_ask(
                &rules,
                &shell(&command.replace("Application", "System")),
                project,
                Dialect::PowerShell
            ),
            RuleDecision::None
        );
        assert!(
            remember_rule(
                &shell(
                    r#"powershell -Command 'Write-Output sk-abcdefghijklmnopqrstuvwxyz0123456789'"#
                ),
                project,
                Dialect::PowerShell
            )
            .is_none()
        );
    }

    #[test]
    fn local_completion_approval_covers_only_the_same_endpoint_and_output_file() {
        let project = Path::new("/p");
        let first = "curl -s http://127.0.0.1:9931/completion -H \"Content-Type: application/json\" -d '{\"prompt\":\"Hello\",\"n_predict\":64}' > /tmp/llama_timing.json; cat /tmp/llama_timing.json";
        let varied_body = "curl -s http://127.0.0.1:9931/completion -H \"Content-Type: application/json\" -d '{\"prompt\":\"Summarize\",\"n_predict\":128}' > /tmp/llama_timing.json; cat /tmp/llama_timing.json";
        let rule = remember_rule(&shell(first), project, Dialect::Posix).unwrap();
        assert_eq!(rule.command_endpoint, "http://127.0.0.1:9931/completion");
        assert_eq!(rule.command_output, "/tmp/llama_timing.json");
        let rules = vec![rule];
        assert_eq!(
            allow_or_ask(&rules, &shell(varied_body), project, Dialect::Posix),
            RuleDecision::Allow
        );
        assert_eq!(
            allow_or_ask(
                &rules,
                &shell(&varied_body.replace("llama_timing.json", "other.json")),
                project,
                Dialect::Posix
            ),
            RuleDecision::None
        );
        assert_eq!(
            allow_or_ask(
                &rules,
                &shell(&varied_body.replace("/completion", "/admin")),
                project,
                Dialect::Posix
            ),
            RuleDecision::None
        );
        assert_eq!(
            allow_or_ask(
                &rules,
                &shell(&format!("{varied_body}; rm x")),
                project,
                Dialect::Posix
            ),
            RuleDecision::None
        );
    }
}
