//! Text encodings the file tools and shell output handle: UTF-8 (with or without BOM),
//! GBK/GB18030 (common on Chinese Windows), and the ones they refuse (UTF-16, binary).

use std::borrow::Cow;

use encoding_rs::{Encoding, GB18030, GBK};

pub const UTF8_BOM: &str = "\u{feff}";

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TextKind {
    Utf8,
    Utf16,
    Binary,
    /// Not UTF-8; possibly GBK or another ANSI code page.
    Legacy,
}

/// Classifies file bytes. UTF-16 (PowerShell 5's `>`) is recognised by its byte-order mark;
/// any other NUL byte means binary.
pub fn sniff(bytes: &[u8]) -> TextKind {
    if bytes.starts_with(&[0xFF, 0xFE]) || bytes.starts_with(&[0xFE, 0xFF]) {
        return TextKind::Utf16;
    }
    if bytes.contains(&0) {
        return TextKind::Binary;
    }
    if std::str::from_utf8(bytes).is_ok() {
        TextKind::Utf8
    } else {
        TextKind::Legacy
    }
}

/// A file encoding other than UTF-8 that text can be written back in.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Legacy {
    encoding: &'static Encoding,
}

impl Legacy {
    pub fn name(&self) -> &'static str {
        if self.encoding == GB18030 {
            "GB18030"
        } else {
            "GBK"
        }
    }

    /// Encodes `text` back, naming the first character the encoding lacks instead of
    /// writing a replacement for it.
    pub fn encode(&self, text: &str) -> std::result::Result<Vec<u8>, String> {
        let (bytes, _, unmappable) = self.encoding.encode(text);
        if !unmappable {
            return Ok(bytes.into_owned());
        }
        let bad = text
            .chars()
            .find(|c| self.encoding.encode(&c.to_string()).2)
            .unwrap_or('\u{fffd}');
        Err(format!(
            "{} cannot represent {bad:?} (U+{:04X})",
            self.name(),
            bad as u32
        ))
    }
}

/// Share of double-byte codes that must lie in GB2312 rows before bytes count as GBK.
/// Structural validity alone accepts Latin-1 text such as "caféine".
const GBK_COMMON_RATIO: f64 = 0.8;

fn gb2312_pair(c0: u8, c1: u8) -> bool {
    (0xA1..=0xFE).contains(&c1) && ((0xA1..=0xA9).contains(&c0) || (0xB0..=0xF7).contains(&c0))
}

/// Decodes `bytes` as GBK (GB18030 when four-byte sequences occur) when it plausibly is
/// Chinese text and the decoded text encodes back to exactly `bytes`, so unchanged regions
/// keep their bytes after an edit.
pub fn decode_gbk(bytes: &[u8]) -> Option<(String, Legacy)> {
    let (mut common, mut other, mut four) = (0_usize, 0_usize, false);
    let mut i = 0;
    while i < bytes.len() {
        let c0 = bytes[i];
        if c0 < 0x80 {
            i += 1;
            continue;
        }
        if c0 == 0x80 {
            other += 1;
            i += 1;
            continue;
        }
        if c0 == 0xFF || i + 1 >= bytes.len() {
            return None;
        }
        let c1 = bytes[i + 1];
        if (0x30..=0x39).contains(&c1) {
            let ok = i + 3 < bytes.len()
                && bytes[i + 2] >= 0x81
                && bytes[i + 2] != 0xFF
                && (0x30..=0x39).contains(&bytes[i + 3]);
            if !ok {
                return None;
            }
            four = true;
            i += 4;
        } else if (0x40..=0xFE).contains(&c1) && c1 != 0x7F {
            if gb2312_pair(c0, c1) {
                common += 1;
            } else {
                other += 1;
            }
            i += 2;
        } else {
            return None;
        }
    }
    if common == 0 || (common as f64) < GBK_COMMON_RATIO * (common + other) as f64 {
        return None;
    }
    let legacy = Legacy {
        encoding: if four { GB18030 } else { GBK },
    };
    let text = GBK.decode_without_bom_handling_and_without_replacement(bytes)?;
    let back = legacy.encode(&text).ok()?;
    (back == bytes).then(|| (text.into_owned(), legacy))
}

/// Decoded file text and how to write it back.
pub struct Decoded {
    pub text: String,
    /// `None` for UTF-8.
    pub legacy: Option<Legacy>,
}

/// Text a file tool may edit, or a model-facing reason why not.
pub fn decode_editable(bytes: &[u8], path: &str) -> std::result::Result<Decoded, String> {
    match sniff(bytes) {
        TextKind::Utf8 => Ok(Decoded {
            text: String::from_utf8_lossy(bytes).into_owned(),
            legacy: None,
        }),
        TextKind::Utf16 => Err(utf16_advice(path)),
        TextKind::Binary => Err(format!(
            "{path} is a binary file; file tools only change text"
        )),
        TextKind::Legacy => match decode_gbk(bytes) {
            Some((text, legacy)) => Ok(Decoded {
                text,
                legacy: Some(legacy),
            }),
            None => Err(format!(
                "{path} is not valid UTF-8, and not GBK either (probably another ANSI code page). Changing it would mix encodings in one file; convert it to UTF-8 first (e.g. iconv -f <encoding> -t UTF-8)."
            )),
        },
    }
}

pub fn utf16_advice(path: &str) -> String {
    format!(
        "{path} is UTF-16 text (often written by PowerShell's > or Out-File). Convert it first, e.g. iconv -f UTF-16 -t UTF-8, or Get-Content | Set-Content -Encoding utf8."
    )
}

/// Whether most line breaks of `text` are CRLF.
pub fn uses_crlf(text: &str) -> bool {
    let crlf = text.matches("\r\n").count();
    crlf > 0 && crlf >= text.matches('\n').count() - crlf
}

pub fn to_crlf(text: &str) -> String {
    text.replace("\r\n", "\n").replace('\n', "\r\n")
}

/// Adapts text the model wrote (LF, no BOM) to the conventions of the file it replaces.
pub fn match_existing(text: &str, existing: &str) -> String {
    let mut text = if uses_crlf(existing) {
        to_crlf(text)
    } else {
        text.to_owned()
    };
    if existing.starts_with(UTF8_BOM) && !text.starts_with(UTF8_BOM) {
        text.insert_str(0, UTF8_BOM);
    }
    text
}

/// One line of process output: UTF-8 as is; otherwise (Windows console code page) GBK
/// when it decodes cleanly; else lossy. The flag reports a lossy decode.
pub fn decode_output_line(bytes: &[u8]) -> (Cow<'_, str>, bool) {
    if let Ok(text) = std::str::from_utf8(bytes) {
        return (Cow::Borrowed(text), false);
    }
    if cfg!(windows)
        && let Some(text) = GBK.decode_without_bom_handling_and_without_replacement(bytes)
    {
        return (Cow::Owned(text.into_owned()), false);
    }
    (String::from_utf8_lossy(bytes), true)
}

/// `decode_output_line` over every line of `bytes`.
pub fn decode_output(bytes: &[u8]) -> (String, bool) {
    if let Ok(text) = std::str::from_utf8(bytes) {
        return (text.to_owned(), false);
    }
    let mut out = String::with_capacity(bytes.len());
    let mut lossy = false;
    for line in bytes.split_inclusive(|byte| *byte == b'\n') {
        let (text, bad) = decode_output_line(line);
        lossy |= bad;
        out.push_str(&text);
    }
    (out, lossy)
}

/// Cuts `text` to at most `max` characters, saying how many were left out.
pub fn clip_chars(text: &str, max: usize) -> Cow<'_, str> {
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

/// Cuts `text` to at most `max` bytes on a character boundary, appending `marker`.
pub fn clip_bytes<'a>(text: &'a str, max: usize, marker: &str) -> Cow<'a, str> {
    if text.len() <= max {
        return Cow::Borrowed(text);
    }
    Cow::Owned(format!(
        "{}{marker}",
        &text[..text.floor_char_boundary(max)]
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn gbk_round_trips_and_latin1_is_not_taken_for_it() {
        let (bytes, _, _) = GBK.encode("中文注释：你好\r\n");
        let (text, legacy) = decode_gbk(&bytes).unwrap();
        assert_eq!(text, "中文注释：你好\r\n");
        assert_eq!(legacy.name(), "GBK");
        assert_eq!(legacy.encode(&text).unwrap(), bytes.as_ref());
        assert!(decode_gbk(b"caf\xe9ine").is_none());
        assert!(legacy.encode("emoji 😀").unwrap_err().contains("U+1F600"));
    }

    #[test]
    fn sniff_classifies_utf16_binary_and_legacy() {
        assert_eq!(sniff(b"\xff\xfeh\0i\0"), TextKind::Utf16);
        assert_eq!(sniff(b"a\0b"), TextKind::Binary);
        assert_eq!(sniff("中".as_bytes()), TextKind::Utf8);
        assert_eq!(sniff(b"\xd6\xd0"), TextKind::Legacy);
    }

    #[test]
    fn existing_line_endings_and_bom_are_kept() {
        assert_eq!(
            match_existing("a\nb\n", "\u{feff}x\r\ny\r\n"),
            "\u{feff}a\r\nb\r\n"
        );
        assert_eq!(match_existing("a\nb", "x\ny"), "a\nb");
        assert!(!uses_crlf("a\nb\r\nc\n"));
    }

    #[test]
    fn long_lines_are_clipped_by_characters() {
        let line = "字".repeat(10);
        assert_eq!(clip_chars(&line, 4), "字字字字…[+6 chars]");
        assert_eq!(clip_chars("short", 4), "shor…[+1 chars]");
        assert_eq!(clip_chars("four", 4), "four");
    }

    #[cfg(windows)]
    #[test]
    fn console_output_in_gbk_is_decoded() {
        let (bytes, _, _) = GBK.encode("系统找不到指定的文件。\r\n");
        let (text, lossy) = decode_output(&bytes);
        assert_eq!(text, "系统找不到指定的文件。\r\n");
        assert!(!lossy);
    }
}
