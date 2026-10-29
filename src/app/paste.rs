//! Images entering through the input line: pasted (or dropped) image paths and,
//! on Windows, an image on the clipboard (Alt+V).

use std::path::{Path, PathBuf};

use crate::domain::{Error, Result};

/// Most paths one paste may attach.
const PASTE_PATHS: usize = 32;
/// Longest paste that is checked for paths.
const PASTE_LIMIT: usize = 64 * 1024;
const IMAGE_EXTENSIONS: [&str; 5] = ["png", "jpg", "jpeg", "gif", "webp"];
/// Largest image file attached from a paste.
const IMAGE_FILE_LIMIT: u64 = 32 * 1024 * 1024;

/// The image files a paste consists of, or `None` when it is (also) other text:
/// the whole text as one path, else whitespace-separated paths with quotes.
pub fn image_paths(text: &str, cwd: &Path) -> Option<Vec<PathBuf>> {
    let text = text.trim();
    if text.is_empty() || text.len() > PASTE_LIMIT {
        return None;
    }
    let whole = unquote(text);
    if let Some(path) = image(whole, cwd) {
        return Some(vec![path]);
    }
    let words = split(text)?;
    if words.is_empty() || words.len() > PASTE_PATHS {
        return None;
    }
    let mut paths: Vec<PathBuf> = Vec::new();
    for word in words {
        let path = image(&word, cwd)?;
        if !paths.contains(&path) {
            paths.push(path);
        }
    }
    Some(paths)
}

fn unquote(text: &str) -> &str {
    for quote in ['"', '\''] {
        if let Some(inner) = text
            .strip_prefix(quote)
            .and_then(|rest| rest.strip_suffix(quote))
        {
            return inner;
        }
    }
    text
}

/// Words separated by whitespace, `"..."` and `'...'` kept together; `None` on an
/// unclosed quote.
fn split(text: &str) -> Option<Vec<String>> {
    let mut words = Vec::new();
    let mut word = String::new();
    let mut quote: Option<char> = None;
    for c in text.chars() {
        match quote {
            Some(open) if c == open => quote = None,
            Some(_) => word.push(c),
            None if c == '"' || c == '\'' => quote = Some(c),
            None if c.is_whitespace() => {
                if !word.is_empty() {
                    words.push(std::mem::take(&mut word));
                }
            }
            None => word.push(c),
        }
    }
    if quote.is_some() {
        return None;
    }
    if !word.is_empty() {
        words.push(word);
    }
    Some(words)
}

/// `raw` as an existing, decodable image file of a supported type.
fn image(raw: &str, cwd: &Path) -> Option<PathBuf> {
    let raw = raw.strip_prefix("file://").unwrap_or(raw);
    let path = PathBuf::from(raw);
    let extension = path.extension()?.to_str()?.to_ascii_lowercase();
    if !IMAGE_EXTENSIONS.contains(&extension.as_str()) {
        return None;
    }
    let path = if path.is_absolute() {
        path
    } else {
        cwd.join(path)
    };
    let meta = std::fs::metadata(&path).ok()?;
    if !meta.is_file() || meta.len() > IMAGE_FILE_LIMIT {
        return None;
    }
    let bytes = std::fs::read(&path).ok()?;
    crate::model::images::sniff(&bytes)?;
    crate::model::images::dimensions(&bytes).ok()?;
    Some(path)
}

/// `name WxH 12.3 KB` for the status row.
pub fn describe(path: &Path) -> String {
    let name = path.file_name().map_or_else(
        || path.display().to_string(),
        |name| name.to_string_lossy().into_owned(),
    );
    let bytes = std::fs::read(path).unwrap_or_default();
    let size = bytes.len() as f64 / 1024.0;
    match crate::model::images::dimensions(&bytes) {
        Ok((width, height)) => format!("\"{name}\" {width}x{height} {size:.1} KB"),
        Err(_) => format!("\"{name}\" {size:.1} KB"),
    }
}

/// Saves the clipboard's image as `clipboard-<hash>.png` in `dir`.
#[cfg(windows)]
pub fn clipboard_image(dir: &Path) -> Result<PathBuf> {
    use sha2::Digest;
    if std::env::var_os("SSH_CONNECTION").is_some() || std::env::var_os("SSH_TTY").is_some() {
        return Err(Error::Session(
            "远程会话里读不到本机剪贴板；请用 @路径 附加图片文件".into(),
        ));
    }
    let image = arboard::Clipboard::new()
        .and_then(|mut clipboard| clipboard.get_image())
        .map_err(|_| {
            Error::Session("剪贴板里没有图片；先复制或截取一张图片，或用 @路径 附加文件".into())
        })?;
    let too_large = |_| Error::Session("剪贴板图片尺寸过大；请保存为文件后用 @路径 附加".into());
    let width = u32::try_from(image.width).map_err(too_large)?;
    let height = u32::try_from(image.height).map_err(too_large)?;
    let mut png = Vec::new();
    {
        let mut encoder = png::Encoder::new(&mut png, width, height);
        encoder.set_color(png::ColorType::Rgba);
        encoder.set_depth(png::BitDepth::Eight);
        let mut writer = encoder.write_header().map_err(|err| {
            Error::Session(format!(
                "剪贴板图片转换失败：{err}；请保存为文件后用 @路径 附加"
            ))
        })?;
        writer.write_image_data(&image.bytes).map_err(|err| {
            Error::Session(format!(
                "剪贴板图片转换失败：{err}；请保存为文件后用 @路径 附加"
            ))
        })?;
    }
    let hash = sha2::Sha256::digest(&png);
    let name: String = hash[..4].iter().map(|byte| format!("{byte:02x}")).collect();
    std::fs::create_dir_all(dir)?;
    let path = dir.join(format!("clipboard-{name}.png"));
    std::fs::write(&path, &png)?;
    Ok(path)
}

#[cfg(not(windows))]
pub fn clipboard_image(_dir: &Path) -> Result<PathBuf> {
    Err(Error::Session(
        "仅 Windows 支持粘贴剪贴板图片；请粘贴或拖入图片文件路径，或用 @路径 附加".into(),
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn png(path: &Path) {
        let mut bytes = Vec::new();
        let mut encoder = png::Encoder::new(&mut bytes, 2, 3);
        encoder.set_color(png::ColorType::Rgba);
        encoder
            .write_header()
            .unwrap()
            .write_image_data(&[0; 24])
            .unwrap();
        std::fs::write(path, bytes).unwrap();
    }

    #[test]
    fn pasted_image_paths_are_recognized_and_other_text_is_not() {
        let dir = tempfile::tempdir().unwrap();
        png(&dir.path().join("a.png"));
        png(&dir.path().join("my shot.png"));
        std::fs::write(dir.path().join("fake.png"), "not an image").unwrap();
        std::fs::write(dir.path().join("notes.txt"), "text").unwrap();
        let cwd = dir.path();
        let a = cwd.join("a.png");
        assert_eq!(image_paths("a.png", cwd), Some(vec![a.clone()]));
        let absolute = format!("\"{}\"", cwd.join("my shot.png").display());
        assert_eq!(
            image_paths(&absolute, cwd),
            Some(vec![cwd.join("my shot.png")])
        );
        assert_eq!(
            image_paths("a.png 'my shot.png' a.png", cwd),
            Some(vec![a.clone(), cwd.join("my shot.png")])
        );
        assert_eq!(image_paths("look at a.png", cwd), None);
        assert_eq!(image_paths("fake.png", cwd), None);
        assert_eq!(image_paths("notes.txt", cwd), None);
        assert_eq!(image_paths("missing.png", cwd), None);
        assert!(describe(&a).starts_with("\"a.png\" 2x3 "));
    }
}
