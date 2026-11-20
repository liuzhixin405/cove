//! Cutting and cleaning text: one rule for each kind of limit.
//!
//! Text for the model and for files is limited in bytes ([`head`], [`clip`],
//! [`short`]) or, where a limit was set in characters, in characters
//! ([`clip_chars`], [`clip_counted`]). Text for the terminal is limited in
//! display columns ([`clip_width`], [`fit_width`]), where a CJK character takes
//! two columns. Untrusted text reaches the terminal only through [`visible`]
//! and its variants.

use std::borrow::Cow;

use unicode_width::UnicodeWidthChar;

/// The marker of text cut for the model or a file.
pub const TRUNCATED: &str = "\n[truncated]";

/// The longest prefix of at most `max` bytes that ends on a character boundary.
pub fn head(text: &str, max: usize) -> &str {
    &text[..text.floor_char_boundary(max)]
}

/// The longest suffix of at most `max` bytes that starts on a character boundary.
pub fn tail(text: &str, max: usize) -> &str {
    let start = text.ceil_char_boundary(text.len().saturating_sub(max));
    &text[start..]
}

/// `text` cut to `max` bytes, with `marker` appended when cut (the marker is not
/// counted in `max`).
pub fn clip(text: &str, max: usize, marker: &str) -> String {
    if text.len() <= max {
        text.to_owned()
    } else {
        format!("{}{marker}", head(text, max))
    }
}

/// One line of at most `max` bytes, ending with `…` when cut: summaries.
pub fn short(text: &str, max: usize) -> String {
    clip(&one_line(text), max, "…")
}

/// At most `max` characters, the `…` that marks a cut included.
pub fn clip_chars(text: &str, max: usize) -> String {
    if text.len() <= max || text.chars().count() <= max {
        return text.to_owned();
    }
    let mut out: String = text.chars().take(max.saturating_sub(1)).collect();
    out.push('…');
    out
}

/// At most `max` characters, followed by how many were left out.
pub fn clip_counted(text: &str, max: usize) -> Cow<'_, str> {
    if text.len() <= max {
        return Cow::Borrowed(text);
    }
    let count = text.chars().count();
    if count <= max {
        return Cow::Borrowed(text);
    }
    let end = text.char_indices().nth(max).map_or(text.len(), |(i, _)| i);
    Cow::Owned(format!("{}…[+{} chars]", &text[..end], count - max))
}

/// All runs of whitespace (newlines included) as single spaces, trimmed.
pub fn one_line(text: &str) -> String {
    text.split_whitespace().collect::<Vec<_>>().join(" ")
}

/// Display columns of one character; controls count as zero.
pub fn char_width(c: char) -> usize {
    c.width().unwrap_or(0)
}

/// Display columns of `text`.
pub fn width(text: &str) -> usize {
    text.chars().map(char_width).sum()
}

/// The longest prefix of at most `cols` display columns, unmarked (for layout).
pub fn fit_width(text: &str, cols: usize) -> &str {
    let mut used = 0;
    for (index, c) in text.char_indices() {
        used += char_width(c);
        if used > cols {
            return &text[..index];
        }
    }
    text
}

/// `text` in at most `cols` display columns, ending with `…` when cut (the `…`
/// counts toward `cols`).
pub fn clip_width(text: &str, cols: usize) -> String {
    if width(text) <= cols {
        return text.to_owned();
    }
    if cols == 0 {
        return String::new();
    }
    let mut out = fit_width(text, cols - 1).to_owned();
    out.push('…');
    out
}

/// Text safe to print, line structure kept: `\r` is dropped (so `\r\n` becomes
/// `\n`) and every other control character but newline and tab, ESC included,
/// is shown as U+FFFD.
pub fn visible(text: &str) -> String {
    neutralize(text, false, false)
}

/// [`visible`] on one row: newlines and tabs become spaces.
pub fn visible_row(text: &str) -> String {
    neutralize(text, true, false)
}

/// [`visible`] that keeps ESC, for output cove styled itself; untrusted parts
/// must have gone through [`visible`] first.
pub fn visible_styled(text: &str) -> String {
    neutralize(text, false, true)
}

fn neutralize(text: &str, row: bool, keep_escape: bool) -> String {
    text.chars()
        .filter(|c| *c != '\r')
        .map(|c| match c {
            '\n' | '\t' if row => ' ',
            '\n' | '\t' => c,
            '\x1b' if keep_escape => c,
            c if c.is_control() => '\u{fffd}',
            c => c,
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn byte_limits_end_on_character_boundaries() {
        assert_eq!(head("héllo", 2), "h");
        assert_eq!(head("héllo", 3), "hé");
        assert_eq!(head("ab", 10), "ab");
        assert_eq!(tail("héllo", 2), "lo");
        assert_eq!(tail("héllo", 3), "llo");
        assert_eq!(tail("中文", 3), "文");
        assert_eq!(tail("ab", 10), "ab");
        assert_eq!(clip("short", 10, TRUNCATED), "short");
        assert_eq!(clip("中文内容", 7, "…"), "中文…");
        assert_eq!(clip("abcdef", 3, TRUNCATED), "abc\n[truncated]");
        assert_eq!(short(" a\n b  c ", 3), "a b…");
    }

    #[test]
    fn character_limits() {
        assert_eq!(clip_chars("中文字", 3), "中文字");
        assert_eq!(clip_chars("中文字符", 3), "中文…");
        let line = "字".repeat(10);
        assert_eq!(clip_counted(&line, 4), "字字字字…[+6 chars]");
        assert_eq!(clip_counted("short", 4), "shor…[+1 chars]");
        assert_eq!(clip_counted("four", 4), "four");
    }

    #[test]
    fn display_limits_count_wide_characters_twice() {
        assert_eq!(width("ab中文"), 6);
        assert_eq!(fit_width("中文字", 5), "中文");
        assert_eq!(fit_width("abc", 5), "abc");
        assert_eq!(clip_width("中文字", 6), "中文字");
        assert_eq!(clip_width("中文字", 5), "中文…");
        assert_eq!(clip_width("中文字", 4), "中…");
        assert_eq!(clip_width("abcdef", 4), "abc…");
        assert_eq!(clip_width("abc", 0), "");
        assert!(width(&clip_width(&"中".repeat(100), 160)) <= 160);
    }

    #[test]
    fn controls_are_neutralized_and_line_endings_normalized() {
        assert_eq!(visible("a\r\nb\rc"), "a\nbc");
        assert_eq!(visible("a\tb\x1b[31m"), "a\tb\u{fffd}[31m");
        assert_eq!(visible_row("a\nb\tc\u{7}"), "a b c\u{fffd}");
        assert_eq!(visible_styled("\x1b[31mred\u{7}"), "\x1b[31mred\u{fffd}");
        assert_eq!(one_line("  a\n\n b\tc "), "a b c");
    }
}
