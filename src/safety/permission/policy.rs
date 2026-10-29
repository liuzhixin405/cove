//! `policies.json` rules: allow, deny or ask by tool and input.

use std::{collections::BTreeMap, path::Path};

use serde::{Deserialize, Serialize};

use super::{
    Effect, classify,
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
            let line = shell::parse(command, dialect);
            let commands: Vec<_> = line.commands().collect();
            let category = classify::classify(command, dialect);
            if !line.opaque.is_empty()
                || commands.len() != 1
                || matches!(category, classify::Category::Dangerous)
            {
                return None;
            }
            let words = commands[0].argv();
            if words.iter().any(|w| w.contains(['/', '\\'])) && words.len() == 1 {
                return None;
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
        assert!(remember_rule(&shell("a && b"), project, Dialect::Posix).is_none());
        let path = dir.path().join("policies.json");
        append(&path, &make).unwrap();
        let rules = append(&path, &make).unwrap();
        assert_eq!(rules.len(), 1);
        assert_eq!(load(&path).unwrap(), rules);
    }
}
