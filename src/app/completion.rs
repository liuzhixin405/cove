//! Input completion for the REPL: slash commands and their arguments, skills,
//! plugin commands and `@path` attachments. Command arguments come from the
//! `/help` usage lines (`/status agents [stop|park <id>]`), so a new subcommand
//! is completed as soon as its help line names it.

use std::path::PathBuf;

/// Most `@path` candidates offered at once.
const PATH_LIMIT: usize = 60;
/// Most expansions one usage line may produce; deeper grammars are cut off.
const EXPANSION_LIMIT: usize = 512;

/// One completion: `text` replaces the word being typed.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Candidate {
    pub text: String,
    pub description: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum Token {
    Word(String),
    /// A `<placeholder>`: any one word.
    Hole,
}

#[derive(Debug)]
enum Node {
    Word(String),
    Hole,
    Seq(Vec<Node>),
    Alt(Vec<Node>),
    Opt(Box<Node>),
}

/// A command's usage: its argument sequences, fully expanded.
struct Usage {
    command: String,
    arguments: Vec<Vec<Token>>,
    description: String,
}

pub struct Completer {
    usages: Vec<Usage>,
    /// Skill and plugin commands as (`/name`, description).
    extra: Vec<(String, String)>,
    skills: Vec<String>,
    cwd: PathBuf,
}

impl Completer {
    /// `help` holds `/help`-style lines: usages, each optionally followed by two
    /// spaces and a description, several per line allowed.
    pub fn new(
        help: &[&str],
        skills: Vec<(String, String)>,
        plugins: Vec<(String, String)>,
        cwd: PathBuf,
    ) -> Self {
        let mut usages = Vec::new();
        for line in help {
            usages.extend(parse_line(line));
        }
        // `/<skill> [args]` describes a family; its members come from `skills`.
        usages.retain(|usage| !usage.command.contains('<'));
        let commands: Vec<String> = usages.iter().map(|usage| usage.command.clone()).collect();
        let mut extra = Vec::new();
        for (name, description) in plugins.iter().chain(&skills) {
            let name = format!("/{name}");
            if !commands.contains(&name) && !extra.iter().any(|(known, _)| *known == name) {
                extra.push((name, description.clone()));
            }
        }
        Self {
            usages,
            extra,
            skills: skills.into_iter().map(|(name, _)| name).collect(),
            cwd,
        }
    }

    /// Whether `command` (`/name`) takes arguments: some usage line names one, or it
    /// is a skill or plugin command (those take free text). Unknown names count as
    /// taking arguments.
    pub fn takes_arguments(&self, command: &str) -> bool {
        let usages: Vec<&Usage> = self
            .usages
            .iter()
            .filter(|usage| usage.command == command)
            .collect();
        if usages.is_empty() {
            return true;
        }
        usages
            .iter()
            .any(|usage| usage.arguments.iter().any(|sequence| !sequence.is_empty()))
    }

    /// Candidates for the word at the end of `line`.
    pub fn suggest(&self, line: &str) -> Vec<Candidate> {
        let current = current_word(line);
        if let Some(path) = current.strip_prefix('@') {
            return self.paths(path);
        }
        let done: Vec<&str> = line[..line.len() - current.len()]
            .split_whitespace()
            .collect();
        let Some(command) = done.first() else {
            return if current.starts_with('/') {
                self.commands(current)
            } else {
                Vec::new()
            };
        };
        if !command.starts_with('/') {
            return Vec::new();
        }
        let arguments = &done[1..];
        let mut found = match (*command, arguments) {
            ("/skill", []) => self
                .skills
                .iter()
                .map(|name| candidate(name, "技能"))
                .collect(),
            ("/help", []) => self
                .usages
                .iter()
                .map(|usage| candidate(usage.command.trim_start_matches('/'), ""))
                .collect(),
            _ => self.arguments(command, arguments),
        };
        found.retain(|found| starts_with(&found.text, current));
        rank(found, current)
    }

    fn commands(&self, typed: &str) -> Vec<Candidate> {
        let mut found: Vec<Candidate> = Vec::new();
        for usage in &self.usages {
            if starts_with(&usage.command, typed)
                && !found.iter().any(|known| known.text == usage.command)
            {
                // The bare command's own line describes it best.
                let description = self
                    .usages
                    .iter()
                    .find(|other| {
                        other.command == usage.command && other.arguments.iter().any(Vec::is_empty)
                    })
                    .unwrap_or(usage);
                found.push(candidate(&usage.command, &description.description));
            }
        }
        for (name, description) in &self.extra {
            if starts_with(name, typed) {
                found.push(candidate(name, description));
            }
        }
        rank(found, typed)
    }

    fn arguments(&self, command: &str, typed: &[&str]) -> Vec<Candidate> {
        let mut found: Vec<Candidate> = Vec::new();
        for usage in self.usages.iter().filter(|usage| usage.command == command) {
            for sequence in &usage.arguments {
                let matches = sequence.len() > typed.len()
                    && sequence.iter().zip(typed).all(|(token, word)| match token {
                        Token::Word(expected) => expected == word,
                        Token::Hole => true,
                    });
                if matches
                    && let Token::Word(next) = &sequence[typed.len()]
                    && !found.iter().any(|known| known.text == *next)
                {
                    found.push(candidate(next, &usage.description));
                }
            }
        }
        found
    }

    fn paths(&self, typed: &str) -> Vec<Candidate> {
        let quoted = typed.starts_with('"');
        let typed = typed.trim_start_matches('"').replace('\\', "/");
        let (dir, base) = match typed.rfind('/') {
            Some(at) => typed.split_at(at + 1),
            None => ("", typed.as_str()),
        };
        let Ok(entries) = std::fs::read_dir(self.cwd.join(dir)) else {
            return Vec::new();
        };
        let mut found = Vec::new();
        for entry in entries.flatten() {
            let name = entry.file_name().to_string_lossy().into_owned();
            if (name.starts_with('.') && !base.starts_with('.')) || !starts_with(&name, base) {
                continue;
            }
            let directory = entry.file_type().is_ok_and(|kind| kind.is_dir());
            let path = format!("{dir}{name}{}", if directory { "/" } else { "" });
            let text = if quoted || path.contains(' ') {
                format!("@\"{path}{}", if directory { "" } else { "\"" })
            } else {
                format!("@{path}")
            };
            found.push(candidate(&text, if directory { "目录" } else { "" }));
        }
        found.sort_by_key(|found| found.text.to_lowercase());
        found.truncate(PATH_LIMIT);
        found
    }
}

/// The word at the end of `line`; empty after trailing whitespace.
pub fn current_word(line: &str) -> &str {
    let start = line
        .char_indices()
        .rev()
        .find(|(_, c)| c.is_whitespace())
        .map_or(0, |(at, c)| at + c.len_utf8());
    &line[start..]
}

/// The longest prefix, in whole characters, that all candidates share.
pub fn common_prefix(candidates: &[Candidate]) -> String {
    let Some(first) = candidates.first() else {
        return String::new();
    };
    let mut prefix: Vec<char> = first.text.chars().collect();
    for other in &candidates[1..] {
        let shared = prefix
            .iter()
            .zip(other.text.chars())
            .take_while(|(a, b)| **a == *b)
            .count();
        prefix.truncate(shared);
    }
    prefix.into_iter().collect()
}

fn candidate(text: &str, description: &str) -> Candidate {
    Candidate {
        text: text.to_owned(),
        description: description.to_owned(),
    }
}

fn starts_with(text: &str, prefix: &str) -> bool {
    text.to_lowercase().starts_with(&prefix.to_lowercase())
}

/// Exact match first, then alphabetical.
fn rank(mut found: Vec<Candidate>, typed: &str) -> Vec<Candidate> {
    found.sort_by(|a, b| {
        (a.text != typed, a.text.to_lowercase()).cmp(&(b.text != typed, b.text.to_lowercase()))
    });
    found
}

/// Usages on one help line: chunks separated by two or more spaces, a chunk
/// starting with `/` being a usage and the next other chunk its description.
fn parse_line(line: &str) -> Vec<Usage> {
    let mut usages: Vec<Usage> = Vec::new();
    let mut undescribed = 0;
    for chunk in line
        .split("  ")
        .map(str::trim)
        .filter(|chunk| !chunk.is_empty())
    {
        if chunk.starts_with('/') {
            let (command, rest) = chunk.split_once(' ').unwrap_or((chunk, ""));
            let mut arguments = expand(&parse(rest));
            arguments.truncate(EXPANSION_LIMIT);
            usages.push(Usage {
                command: command.to_owned(),
                arguments,
                description: String::new(),
            });
            undescribed += 1;
        } else {
            let at = usages.len();
            for usage in &mut usages[at - undescribed.min(at)..] {
                usage.description = chunk.to_owned();
            }
            undescribed = 0;
        }
    }
    usages
}

enum Lexeme {
    Open,
    Close,
    Bar,
    Word(String),
    Hole,
}

fn lex(text: &str) -> Vec<Lexeme> {
    let mut lexemes = Vec::new();
    let mut word = String::new();
    let mut chars = text.chars();
    let flush = |word: &mut String, lexemes: &mut Vec<Lexeme>| {
        if !word.is_empty() {
            lexemes.push(Lexeme::Word(std::mem::take(word)));
        }
    };
    while let Some(c) = chars.next() {
        match c {
            '[' | ']' | '|' => {
                flush(&mut word, &mut lexemes);
                lexemes.push(match c {
                    '[' => Lexeme::Open,
                    ']' => Lexeme::Close,
                    _ => Lexeme::Bar,
                });
            }
            '<' => {
                flush(&mut word, &mut lexemes);
                for c in chars.by_ref() {
                    if c == '>' {
                        break;
                    }
                }
                lexemes.push(Lexeme::Hole);
            }
            c if c.is_whitespace() => flush(&mut word, &mut lexemes),
            c => word.push(c),
        }
    }
    flush(&mut word, &mut lexemes);
    lexemes
}

fn parse(text: &str) -> Node {
    let lexemes = lex(text);
    let mut at = 0;
    alternatives(&lexemes, &mut at)
}

fn alternatives(lexemes: &[Lexeme], at: &mut usize) -> Node {
    let mut options = vec![sequence(lexemes, at)];
    while matches!(lexemes.get(*at), Some(Lexeme::Bar)) {
        *at += 1;
        options.push(sequence(lexemes, at));
    }
    if options.len() == 1 {
        return options.pop().expect("one option");
    }
    // `stop|park|discard <id> [confirm]` means `(stop|park|discard) <id> [confirm]`:
    // when only the last option has more than its first word, the rest is shared.
    // `fast <name>|provider <name>` keeps one sequence per option.
    let single = |node: &Node| matches!(node, Node::Seq(items) if items.len() == 1);
    let (last, earlier) = options.split_last_mut().expect("several options");
    if earlier.iter().all(single)
        && let Node::Seq(items) = last
        && items.len() > 1
        && matches!(items[0], Node::Word(_))
    {
        let tail: Vec<Node> = items.drain(1..).collect();
        let head = std::mem::replace(last, Node::Seq(Vec::new()));
        let mut heads: Vec<Node> = options.drain(..options.len() - 1).collect();
        heads.push(head);
        let mut items = vec![Node::Alt(heads)];
        items.extend(tail);
        return Node::Seq(items);
    }
    // Likewise `budget auto|off|<usd>` means `budget (auto|off|<usd>)`.
    let (first, later) = options.split_first_mut().expect("several options");
    if later.iter().all(single)
        && let Node::Seq(items) = first
        && items.len() > 1
    {
        let last = items.pop().expect("several items");
        let mut prefix = std::mem::take(items);
        let mut tails = vec![last];
        tails.extend(options.drain(1..));
        prefix.push(Node::Alt(tails));
        return Node::Seq(prefix);
    }
    Node::Alt(options)
}

fn sequence(lexemes: &[Lexeme], at: &mut usize) -> Node {
    let mut items = Vec::new();
    while let Some(lexeme) = lexemes.get(*at) {
        match lexeme {
            Lexeme::Bar => break,
            Lexeme::Close => {
                // A stray `]` at the top level is skipped; inside a group it ends it.
                break;
            }
            Lexeme::Open => {
                *at += 1;
                let inner = alternatives(lexemes, at);
                if matches!(lexemes.get(*at), Some(Lexeme::Close)) {
                    *at += 1;
                }
                items.push(Node::Opt(Box::new(inner)));
            }
            Lexeme::Word(word) => {
                *at += 1;
                items.push(Node::Word(word.clone()));
            }
            Lexeme::Hole => {
                *at += 1;
                items.push(Node::Hole);
            }
        }
    }
    Node::Seq(items)
}

fn expand(node: &Node) -> Vec<Vec<Token>> {
    match node {
        Node::Word(word) => vec![vec![Token::Word(word.clone())]],
        Node::Hole => vec![vec![Token::Hole]],
        Node::Opt(inner) => {
            let mut all = vec![Vec::new()];
            all.extend(expand(inner));
            all
        }
        Node::Alt(options) => {
            let mut all: Vec<Vec<Token>> = options.iter().flat_map(expand).collect();
            all.truncate(EXPANSION_LIMIT);
            all
        }
        Node::Seq(items) => {
            let mut all = vec![Vec::new()];
            for item in items {
                let tails = expand(item);
                let mut next = Vec::new();
                'outer: for head in &all {
                    for tail in &tails {
                        if next.len() == EXPANSION_LIMIT {
                            break 'outer;
                        }
                        let mut joined = head.clone();
                        joined.extend(tail.iter().cloned());
                        next.push(joined);
                    }
                }
                all = next;
            }
            all
        }
    }
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    use super::*;

    fn completer(cwd: &Path) -> Completer {
        Completer::new(
            &[
                "/status  session, model, context and spend",
                "/status agents [stop|park|resume <id|main> [confirm]]  sub-agents",
                "/status cost [month|today] [export [file]]  usage",
                "/skills  list skills   /skill <name>  show one skill",
                "/model [<name>|fast <name|off>|provider <name> [model]|profile [list|switch] <name>]",
                "/config budget auto|off|<usd>|save  /config setup",
            ],
            vec![
                ("tdd".into(), "test first".into()),
                ("status".into(), "shadowed".into()),
            ],
            vec![("deploy".into(), "plugin deploy".into())],
            cwd.to_owned(),
        )
    }

    fn texts(found: &[Candidate]) -> Vec<&str> {
        found.iter().map(|found| found.text.as_str()).collect()
    }

    #[test]
    fn command_names_complete_with_descriptions_and_skills() {
        let dir = tempfile::tempdir().unwrap();
        let completer = completer(dir.path());
        let found = completer.suggest("/s");
        assert_eq!(texts(&found), ["/skill", "/skills", "/status"]);
        assert_eq!(found[2].description, "session, model, context and spend");
        assert_eq!(texts(&completer.suggest("/skill")), ["/skill", "/skills"]);
        assert_eq!(texts(&completer.suggest("/t")), ["/tdd"]);
        assert!(!completer.takes_arguments("/skills"));
        assert!(completer.takes_arguments("/status"));
        assert!(completer.takes_arguments("/tdd"));
        assert_eq!(texts(&completer.suggest("/d")), ["/deploy"]);
        assert!(completer.suggest("hello /s").is_empty());
        assert!(completer.suggest("plain text").is_empty());
    }

    #[test]
    fn arguments_follow_the_usage_grammar() {
        let dir = tempfile::tempdir().unwrap();
        let completer = completer(dir.path());
        assert_eq!(texts(&completer.suggest("/status ")), ["agents", "cost"]);
        assert_eq!(texts(&completer.suggest("/status a")), ["agents"]);
        assert_eq!(
            texts(&completer.suggest("/status agents ")),
            ["park", "resume", "stop"]
        );
        assert_eq!(
            texts(&completer.suggest("/status agents stop ab12 ")),
            ["confirm"]
        );
        assert_eq!(texts(&completer.suggest("/status cost today ")), ["export"]);
        assert_eq!(
            texts(&completer.suggest("/model ")),
            ["fast", "profile", "provider"]
        );
        assert!(completer.suggest("/model fast ").is_empty());
        assert_eq!(
            texts(&completer.suggest("/model profile ")),
            ["list", "switch"]
        );
        assert_eq!(texts(&completer.suggest("/config ")), ["budget", "setup"]);
        assert_eq!(
            texts(&completer.suggest("/config budget ")),
            ["auto", "off", "save"]
        );
        assert_eq!(texts(&completer.suggest("/skill ")), ["status", "tdd"]);
        assert!(completer.suggest("/tdd ").is_empty());
    }

    #[test]
    fn at_paths_complete_files_and_directories() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("src/app")).unwrap();
        std::fs::write(dir.path().join("src/main.rs"), "").unwrap();
        std::fs::write(dir.path().join("my notes.md"), "").unwrap();
        std::fs::write(dir.path().join(".hidden"), "").unwrap();
        let completer = completer(dir.path());
        assert_eq!(texts(&completer.suggest("look at @s")), ["@src/"]);
        assert_eq!(
            texts(&completer.suggest("look at @src/")),
            ["@src/app/", "@src/main.rs"]
        );
        assert_eq!(texts(&completer.suggest("@src\\m")), ["@src/main.rs"]);
        assert_eq!(texts(&completer.suggest("@my")), ["@\"my notes.md\""]);
        assert_eq!(texts(&completer.suggest("@.h")), ["@.hidden"]);
        assert!(!texts(&completer.suggest("@")).contains(&"@.hidden"));
    }

    #[test]
    fn words_and_prefixes_respect_characters() {
        assert_eq!(current_word("/status ag"), "ag");
        assert_eq!(current_word("/status "), "");
        assert_eq!(current_word("看看 @档"), "@档");
        let found = vec![candidate("@档案a", ""), candidate("@档案b", "")];
        assert_eq!(common_prefix(&found), "@档案");
    }
}
