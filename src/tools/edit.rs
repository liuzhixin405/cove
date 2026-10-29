//! String replacement for the `edit` tool: exact matches first, then a whitespace-tolerant
//! fallback that applies only to a unique match and writes the new text in the file's
//! indentation. Pure functions over text; encoding and journaling are the caller's.

use std::collections::{HashMap, HashSet};

/// Changed regions shown in a result, and match lines listed in an error.
const SNIPPETS: usize = 3;
const SNIPPET_CONTEXT: usize = 2;
/// The fuzzy fallback is skipped above this size, so it never becomes a performance cliff.
const FUZZY_MAX_LINES: usize = 20_000;

pub struct Applied {
    pub result: String,
    pub replacements: usize,
    /// Byte offsets in `result` where replaced text starts (at most `SNIPPETS`).
    pub spots: Vec<usize>,
    pub inserted_len: usize,
    /// Explains a fuzzy match, empty for an exact one.
    pub note: String,
}

/// Replaces `old` with `new` in `content`. `Err` is a model-facing explanation.
pub fn apply(content: &str, old: &str, new: &str, all: bool) -> Result<Applied, String> {
    let count = content.matches(old).count();
    if count == 1 || (count > 1 && all) {
        let positions: Vec<usize> = content
            .match_indices(old)
            .take(if all { SNIPPETS } else { 1 })
            .map(|(index, _)| index)
            .collect();
        let result = if all {
            content.replace(old, new)
        } else {
            content.replacen(old, new, 1)
        };
        let shift = new.len() as isize - old.len() as isize;
        let spots = positions
            .iter()
            .enumerate()
            .map(|(k, position)| (*position as isize + k as isize * shift) as usize)
            .collect();
        return Ok(Applied {
            result,
            replacements: if all { count } else { 1 },
            spots,
            inserted_len: new.len(),
            note: String::new(),
        });
    }
    if count > 1 {
        return Err(format!(
            "old matches {count} times ({}). Use replace_all=true or include more surrounding context.",
            match_lines(content, old)
        ));
    }
    match fuzzy(content, old, new) {
        Fuzzy::Applied {
            result,
            start,
            inserted,
            first,
            last,
            reindented,
        } => Ok(Applied {
            result,
            replacements: 1,
            spots: vec![start],
            inserted_len: inserted.len(),
            note: format!(
                " (whitespace-insensitive match at lines {first}-{last}{})",
                if reindented {
                    "; new text re-indented to the file's indentation"
                } else {
                    ""
                }
            ),
        }),
        Fuzzy::Refused(message) => Err(message),
        Fuzzy::Missing(hint) => Err(match hint {
            Some(hint) => format!("old was not found in the file. {hint}"),
            None => "old was not found in the file; read it again and copy the exact text.".into(),
        }),
    }
}

/// "lines 1, 3, 5" for the occurrences of `old`.
fn match_lines(content: &str, old: &str) -> String {
    const LISTED: usize = 20;
    let mut parts = Vec::new();
    let (mut line, mut last) = (1, 0);
    for (index, (offset, _)) in content.match_indices(old).enumerate() {
        if index == LISTED {
            parts.push("...".to_owned());
            break;
        }
        line += content[last..offset].matches('\n').count();
        last = offset;
        parts.push(line.to_string());
    }
    format!("lines {}", parts.join(", "))
}

/// Each changed region of `result` with two lines of context, numbered like `read` output.
pub fn snippets(result: &str, spots: &[usize], inserted_len: usize) -> String {
    let mut lines: Vec<&str> = result.split('\n').collect();
    if lines.len() > 1 && lines.last() == Some(&"") {
        lines.pop();
    }
    let mut out = String::new();
    for (index, &start) in spots.iter().enumerate() {
        if start > result.len() {
            continue;
        }
        let end = (start + inserted_len).min(result.len());
        let first = result[..start].matches('\n').count();
        let mut last = first + result[start..end].matches('\n').count();
        if end > start && result.as_bytes()[end - 1] == b'\n' && last > first {
            last -= 1;
        }
        let from = first.saturating_sub(SNIPPET_CONTEXT);
        let to = (last + SNIPPET_CONTEXT).min(lines.len().saturating_sub(1));
        if index > 0 {
            out.push_str("---\n");
        }
        for (number, line) in lines.iter().enumerate().take(to + 1).skip(from) {
            out.push_str(&format!(
                "{}: {}\n",
                number + 1,
                line.trim_end_matches('\r')
            ));
        }
    }
    crate::tools::text_encoding::clip_bytes(out.trim_end(), 4000, "\n... (truncated)").into_owned()
}

enum Fuzzy {
    Applied {
        result: String,
        start: usize,
        inserted: String,
        first: usize,
        last: usize,
        reindented: bool,
    },
    /// A unique block matched, but the new text cannot be applied safely.
    Refused(String),
    Missing(Option<String>),
}

fn normalize(line: &str) -> String {
    line.split_whitespace().collect::<Vec<_>>().join(" ")
}

/// Lines without their `\n` (CRLF lines keep `\r`), with byte widths including terminators.
fn split_widths(content: &str) -> (Vec<&str>, Vec<usize>) {
    let mut lines = Vec::new();
    let mut widths = Vec::new();
    let mut start = 0;
    for (index, byte) in content.bytes().enumerate() {
        if byte == b'\n' {
            lines.push(&content[start..index]);
            widths.push(index - start + 1);
            start = index + 1;
        }
    }
    if start < content.len() || content.is_empty() {
        lines.push(&content[start..]);
        widths.push(content.len() - start);
    }
    (lines, widths)
}

fn fuzzy(content: &str, old: &str, new: &str) -> Fuzzy {
    let mut old_lines: Vec<&str> = old.split('\n').collect();
    let mut trailing_newline = false;
    if old_lines.len() > 1 && old_lines.last() == Some(&"") {
        old_lines.pop();
        trailing_newline = true;
    }
    let n = old_lines.len();
    let normalized: Vec<String> = old_lines.iter().map(|line| normalize(line)).collect();
    let (lines, widths) = split_widths(content);
    if lines.is_empty() || lines.len() > FUZZY_MAX_LINES || n > lines.len() {
        return Fuzzy::Missing(None);
    }
    let starts: Vec<usize> = (0..=lines.len() - n)
        .filter(|&start| (0..n).all(|k| normalize(lines[start + k]) == normalized[k]))
        .collect();
    if let [start] = starts[..] {
        let byte_start: usize = widths[..start].iter().sum();
        let mut width: usize = widths[start..start + n - 1].iter().sum();
        if trailing_newline {
            width += widths[start + n - 1];
        } else {
            // Stop before a CRLF line's `\r`, or the replacement leaves one LF line behind.
            let last = lines[start + n - 1];
            width += if widths[start + n - 1] > last.len() {
                last.trim_end_matches('\r').len()
            } else {
                last.len()
            };
        }
        let byte_end = byte_start + width;
        if byte_end > content.len() {
            return Fuzzy::Missing(None);
        }
        let Some((text, reindented)) = reindent(
            &lines[start..start + n],
            &old_lines,
            new,
            file_indent_unit(&lines),
        ) else {
            return Fuzzy::Refused(format!(
                "old matched lines {}-{} only after whitespace normalization, and the indentation there does not map consistently onto old's (mixed tabs/spaces or uneven levels), so a multi-line new text cannot be re-indented safely. The file was not changed; read lines {}-{} again and retry with their exact text.",
                start + 1,
                start + n,
                start + 1,
                start + n
            ));
        };
        return Fuzzy::Applied {
            result: format!("{}{text}{}", &content[..byte_start], &content[byte_end..]),
            start: byte_start,
            inserted: text,
            first: start + 1,
            last: start + n,
            reindented,
        };
    }
    if starts.len() > 1 {
        let ranges: Vec<String> = starts
            .iter()
            .map(|start| {
                if n <= 1 {
                    (start + 1).to_string()
                } else {
                    format!("{}-{}", start + 1, start + n)
                }
            })
            .collect();
        return Fuzzy::Missing(Some(format!(
            "Found {} whitespace-normalized matches (ambiguous) at lines {}. Add more surrounding context to old.",
            starts.len(),
            ranges.join(", ")
        )));
    }
    let mut best = (0, 0.0_f64);
    for start in 0..=lines.len() - n {
        let score = similarity(&lines[start..start + n], &normalized);
        if score > best.1 {
            best = (start, score);
        }
    }
    if best.1 < 0.6 {
        return Fuzzy::Missing(None);
    }
    let snippet = lines[best.0..best.0 + n].join("\n");
    Fuzzy::Missing(Some(format!(
        "Closest candidate ({:.0}% similar) is at lines {}-{}:\n---\n{}\n---\nCompare carefully against old (spacing, quotes, identifiers) and retry with the exact text.",
        best.1 * 100.0,
        best.0 + 1,
        best.0 + n,
        crate::tools::text_encoding::clip_bytes(&snippet, 800, "\n... (truncated)")
    )))
}

/// Mean per-line word-set Jaccard similarity; cheap enough for every window of a large file.
fn similarity(window: &[&str], normalized: &[String]) -> f64 {
    if window.is_empty() || window.len() != normalized.len() {
        return 0.0;
    }
    let total: f64 = window
        .iter()
        .zip(normalized)
        .map(|(line, old)| {
            let a: HashSet<&str> = line.split_whitespace().collect();
            let b: HashSet<&str> = old.split_whitespace().collect();
            let union = a.union(&b).count();
            if union == 0 {
                1.0
            } else {
                a.intersection(&b).count() as f64 / union as f64
            }
        })
        .sum();
    total / window.len() as f64
}

fn blank(line: &str) -> bool {
    line.trim().is_empty()
}

fn leading(line: &str) -> &str {
    let end = line
        .bytes()
        .position(|byte| byte != b' ' && byte != b'\t')
        .unwrap_or(line.len());
    &line[..end]
}

fn spaces(indent: &str) -> usize {
    indent.bytes().filter(|byte| *byte == b' ').count()
}

/// One level per tab, one per `unit` spaces; `None` when spaces are not whole units.
fn level(indent: &str, unit: usize) -> Option<usize> {
    let tabs = indent.len() - spaces(indent);
    let spaces = spaces(indent);
    if spaces == 0 {
        return Some(tabs);
    }
    (unit > 0 && spaces.is_multiple_of(unit)).then(|| tabs + spaces / unit)
}

fn gcd(a: usize, b: usize) -> usize {
    if b == 0 { a } else { gcd(b, a % b) }
}

/// Spaces per level: the gcd of the space counts, or 4/2 when all counts are equal.
fn space_unit(indents: &[&str]) -> usize {
    let mut g = 0;
    let mut distinct = HashSet::new();
    for indent in indents {
        let n = spaces(indent);
        if n > 0 {
            g = gcd(g, n);
            distinct.insert(n);
        }
    }
    if distinct.len() == 1 {
        if g % 4 == 0 {
            return 4;
        }
        if g % 2 == 0 {
            return 2;
        }
    }
    g
}

/// The file's most common positive indentation step between space-indented lines.
fn file_indent_unit(lines: &[&str]) -> usize {
    let mut counts: HashMap<usize, usize> = HashMap::new();
    let mut previous: Option<usize> = None;
    for line in lines.iter().filter(|line| !blank(line)) {
        let indent = leading(line);
        if indent.contains('\t') {
            previous = None;
            continue;
        }
        if let Some(previous) = previous
            && indent.len() > previous
        {
            *counts.entry(indent.len() - previous).or_default() += 1;
        }
        previous = Some(indent.len());
    }
    counts
        .into_iter()
        .max_by(|a, b| a.1.cmp(&b.1).then(b.0.cmp(&a.0)))
        .map_or(0, |(step, _)| step)
}

/// The matched block's space unit; a single-depth block uses the file-wide `hint` when it
/// maps the block onto the depth `old` has.
fn block_unit(file: &[&str], old: &[&str], model_unit: usize, hint: usize) -> usize {
    let own = space_unit(file);
    let depths: HashSet<usize> = file.iter().map(|i| spaces(i)).filter(|n| *n > 0).collect();
    let fits = hint > 0
        && file
            .iter()
            .all(|indent| spaces(indent).is_multiple_of(hint));
    if depths.len() != 1 || !fits || old.is_empty() {
        return own;
    }
    for (index, indent) in file.iter().enumerate() {
        if indent.is_empty() {
            continue;
        }
        let Some(old_indent) = old.get(index) else {
            return own;
        };
        return match (level(indent, hint), level(old_indent, model_unit)) {
            (Some(a), Some(b)) if a == b => hint,
            _ => own,
        };
    }
    own
}

/// Rewrites `new`'s indentation from `old`'s style into the matched file lines' style.
/// `None` refuses a multi-line `new` whose mapping is ambiguous.
fn reindent(file: &[&str], old: &[&str], new: &str, hint: usize) -> Option<(String, bool)> {
    let mut file_indents = Vec::new();
    let mut old_indents = Vec::new();
    let mut identical = true;
    for (file_line, old_line) in file.iter().zip(old) {
        if blank(old_line) || blank(file_line) {
            continue;
        }
        let (f, o) = (leading(file_line), leading(old_line));
        identical &= f == o;
        file_indents.push(f);
        old_indents.push(o);
    }
    if identical {
        return Some((new.to_owned(), false));
    }
    let multi_line = new.trim_end_matches(['\r', '\n']).contains('\n');
    let fail = || (!multi_line).then(|| (new.to_owned(), false));
    let mut new_lines: Vec<String> = new.split('\n').map(str::to_owned).collect();
    let new_indents: Vec<&str> = new
        .split('\n')
        .filter(|line| !blank(line))
        .map(leading)
        .collect();
    let tabs = file_indents.iter().any(|indent| indent.contains('\t'));
    let uses_spaces = file_indents.iter().any(|indent| spaces(indent) > 0);
    if tabs && uses_spaces {
        return fail();
    }
    let model: Vec<&str> = old_indents.iter().chain(&new_indents).copied().collect();
    let model_unit = space_unit(&model);
    let file_unit = block_unit(&file_indents, &old_indents, model_unit, hint);
    let unit = if uses_spaces {
        " ".repeat(file_unit)
    } else if !tabs && model_unit > 0 {
        " ".repeat(model_unit)
    } else {
        "\t".to_owned()
    };
    let mut delta: Option<isize> = None;
    for (file_indent, old_indent) in file_indents.iter().zip(&old_indents) {
        let (Some(f), Some(o)) = (level(file_indent, file_unit), level(old_indent, model_unit))
        else {
            return fail();
        };
        let d = f as isize - o as isize;
        match delta {
            None => delta = Some(d),
            Some(existing) if existing != d => return fail(),
            Some(_) => {}
        }
    }
    let delta = delta.unwrap_or(0);
    for line in &mut new_lines {
        if blank(line) {
            continue;
        }
        let indent = leading(line).to_owned();
        let Some(lvl) = level(&indent, model_unit) else {
            return fail();
        };
        let target = lvl as isize + delta;
        if target < 0 {
            return fail();
        }
        *line = format!("{}{}", unit.repeat(target as usize), &line[indent.len()..]);
    }
    let text = new_lines.join("\n");
    let changed = text != new;
    Some((text, changed))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn exact_matches_replace_once_or_all_and_list_lines_when_ambiguous() {
        let applied = apply("a\nb\na\n", "a", "c", true).unwrap();
        assert_eq!(applied.result, "c\nb\nc\n");
        assert_eq!(applied.replacements, 2);
        let err = apply("a\nb\na\n", "a", "c", false).err().unwrap();
        assert!(err.contains("2 times (lines 1, 3)"), "{err}");
    }

    #[test]
    fn fuzzy_match_reindents_to_the_files_tabs() {
        let content = "func main() {\n\tif x {\n\t\tdo()\n\t}\n}\n";
        let old = "if x {\n    do()\n}";
        let new = "if y {\n    do()\n    more()\n}";
        let applied = apply(content, old, new, false).unwrap();
        assert_eq!(
            applied.result,
            "func main() {\n\tif y {\n\t\tdo()\n\t\tmore()\n\t}\n}\n"
        );
        assert!(applied.note.contains("re-indented"), "{}", applied.note);
    }

    #[test]
    fn fuzzy_match_keeps_crlf_and_refuses_ambiguity() {
        let content = "a  =  1\r\nb = 2\r\n";
        let applied = apply(content, "a = 1", "a = 3", false).unwrap();
        assert_eq!(applied.result, "a = 3\r\nb = 2\r\n");
        let err = apply("x  y\nx y\n", "x   y", "z", false).err().unwrap();
        assert!(err.contains("ambiguous"), "{err}");
    }

    #[test]
    fn missing_text_names_the_closest_candidate() {
        let content = "fn alpha(one: u32, two: u32) {}\nfn beta() {}\n";
        let err = apply(content, "fn alpha(one: u32, three: u32) {}", "x", false)
            .err()
            .unwrap();
        assert!(err.contains("Closest candidate"), "{err}");
        assert!(err.contains("lines 1-1"), "{err}");
    }

    #[test]
    fn inconsistent_indentation_is_refused_for_multi_line_text() {
        let content = "\tif x {\n    do()\n\t}\n";
        let err = apply(content, "if x {\ndo()\n}", "if y {\n  a()\n}", false)
            .err()
            .unwrap();
        assert!(err.contains("cannot be re-indented"), "{err}");
    }

    #[test]
    fn snippets_show_context_around_changes() {
        let result = "1\n2\n3\nNEW\n5\n6\n7\n";
        let start = result.find("NEW").unwrap();
        assert_eq!(
            snippets(result, &[start], 3),
            "2: 2\n3: 3\n4: NEW\n5: 5\n6: 6"
        );
    }
}
