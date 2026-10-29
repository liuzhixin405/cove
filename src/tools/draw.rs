//! `draw_image`: a small rasterizer for diagrams and placeholder art, written as
//! PNG through the workspace checkpoint (so `/undo` restores or removes it).

use std::path::{Component, Path, PathBuf};

use serde::Deserialize;
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Result, ToolDefinition},
    safety::permission::{Authorizer, Effect},
    session::workspace::Workspace,
};

const MAX_SIDE: u32 = 16_384;
const MAX_PIXELS: u64 = 64 * 1024 * 1024;
const MAX_STROKE: f64 = 64.0;
const MAX_COORD: f64 = (1 << 24) as f64;
const MAX_SHAPES: usize = 10_000;
pub const PNG_SIGNATURE: &[u8] = b"\x89PNG\r\n\x1a\n";

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DrawArgs {
    #[serde(alias = "output_path")]
    #[serde(rename = "outputPath")]
    output_path: PathBuf,
    width: u32,
    height: u32,
    shapes: Vec<Shape>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Shape {
    #[serde(rename = "type")]
    kind: ShapeKind,
    #[serde(default)]
    x: f64,
    #[serde(default)]
    y: f64,
    #[serde(default)]
    w: f64,
    #[serde(default)]
    h: f64,
    #[serde(default)]
    r: f64,
    #[serde(default)]
    x2: f64,
    #[serde(default)]
    y2: f64,
    #[serde(default)]
    color: Option<String>,
    #[serde(default, rename = "strokeWidth", alias = "stroke_width")]
    stroke_width: Option<f64>,
}

#[derive(Clone, Copy, Deserialize)]
#[serde(rename_all = "lowercase")]
enum ShapeKind {
    Rect,
    Circle,
    Line,
    Fill,
}

pub fn definition() -> ToolDefinition {
    ToolDefinition {
        name: "draw_image".into(),
        description: "Draw a PNG (white canvas) from shapes: fill (whole canvas), rect (x,y,w,h), circle (center x,y, r), line (x,y to x2,y2, strokeWidth<=64). color: #RRGGBB, #RRGGBBAA or rgba(r,g,b,a). outputPath is a workspace .png; an existing file is replaced only if it is a PNG.".into(),
        schema: json!({"type":"object","properties":{
            "outputPath":{"type":"string"},
            "width":{"type":"integer","minimum":1,"maximum":MAX_SIDE},
            "height":{"type":"integer","minimum":1,"maximum":MAX_SIDE},
            "shapes":{"type":"array","minItems":1,"items":{"type":"object","properties":{
                "type":{"enum":["rect","circle","line","fill"]},
                "x":{"type":"number"},"y":{"type":"number"},"w":{"type":"number"},"h":{"type":"number"},
                "r":{"type":"number"},"x2":{"type":"number"},"y2":{"type":"number"},
                "color":{"type":"string"},"strokeWidth":{"type":"number"}
            },"required":["type"],"additionalProperties":false}}
        },"required":["outputPath","width","height","shapes"],"additionalProperties":false}),
    }
}

/// RGBA; alpha 255 is opaque.
pub fn parse_color(text: Option<&str>) -> Result<[u8; 4]> {
    let Some(text) = text.map(str::trim).filter(|text| !text.is_empty()) else {
        return Ok([0, 0, 0, 255]);
    };
    let lower = text.to_ascii_lowercase();
    if let Some(hex) = lower.strip_prefix('#') {
        let byte = |i: usize| u8::from_str_radix(&hex[i..i + 2], 16);
        let parsed = match hex.len() {
            6 => (byte(0), byte(2), byte(4), Ok(255)),
            8 => (byte(0), byte(2), byte(4), byte(6)),
            _ => return Err(Error::Tool(format!("invalid color {text:?}"))),
        };
        if let (Ok(r), Ok(g), Ok(b), Ok(a)) = parsed {
            return Ok([r, g, b, a]);
        }
        return Err(Error::Tool(format!("invalid color {text:?}")));
    }
    for (prefix, count) in [("rgba(", 4), ("rgb(", 3)] {
        if let Some(inner) = lower.strip_prefix(prefix).and_then(|t| t.strip_suffix(')')) {
            let parts: Vec<f64> = inner
                .split(',')
                .map(|part| part.trim().parse::<f64>())
                .collect::<std::result::Result<_, _>>()
                .map_err(|_| Error::Tool(format!("invalid color {text:?}")))?;
            if parts.len() != count || parts.iter().any(|v| !v.is_finite()) {
                return Err(Error::Tool(format!("invalid color {text:?}")));
            }
            let channel = |v: f64| v.clamp(0.0, 255.0).round() as u8;
            // CSS alpha is 0..1; values above 1 are read as 0..255.
            let alpha = parts.get(3).map_or(255, |&a| {
                if a <= 1.0 {
                    (a.max(0.0) * 255.0).round() as u8
                } else {
                    channel(a)
                }
            });
            return Ok([
                channel(parts[0]),
                channel(parts[1]),
                channel(parts[2]),
                alpha,
            ]);
        }
    }
    Ok(match lower.as_str() {
        "black" => [0, 0, 0, 255],
        "white" => [255, 255, 255, 255],
        "red" => [255, 0, 0, 255],
        "green" => [0, 128, 0, 255],
        "blue" => [0, 0, 255, 255],
        "yellow" => [255, 255, 0, 255],
        "gray" | "grey" => [128, 128, 128, 255],
        "orange" => [255, 165, 0, 255],
        "purple" => [128, 0, 128, 255],
        "transparent" => [0, 0, 0, 0],
        _ => return Err(Error::Tool(format!("invalid color {text:?}"))),
    })
}

/// Opaque RGB canvas; shapes are alpha-blended onto it.
pub struct Canvas {
    width: u32,
    height: u32,
    pixels: Vec<u8>,
}

impl Canvas {
    pub fn new(width: u32, height: u32) -> Result<Self> {
        if width == 0 || height == 0 || width > MAX_SIDE || height > MAX_SIDE {
            return Err(Error::Tool(format!(
                "width and height must be 1..{MAX_SIDE} px"
            )));
        }
        if u64::from(width) * u64::from(height) > MAX_PIXELS {
            return Err(Error::Tool(format!(
                "canvas {width}x{height} exceeds {MAX_PIXELS} pixels"
            )));
        }
        Ok(Self {
            width,
            height,
            pixels: vec![255; width as usize * height as usize * 3],
        })
    }

    fn blend(&mut self, x: i64, y: i64, [r, g, b, a]: [u8; 4]) {
        if x < 0 || y < 0 || x >= i64::from(self.width) || y >= i64::from(self.height) || a == 0 {
            return;
        }
        let index = (y as usize * self.width as usize + x as usize) * 3;
        for (offset, value) in [r, g, b].into_iter().enumerate() {
            let old = u32::from(self.pixels[index + offset]);
            let new = (u32::from(value) * u32::from(a) + old * (255 - u32::from(a)) + 127) / 255;
            self.pixels[index + offset] = new as u8;
        }
    }

    fn span(&mut self, y: i64, x0: i64, x1: i64, color: [u8; 4]) {
        let x0 = x0.max(0);
        let x1 = x1.min(i64::from(self.width) - 1);
        for x in x0..=x1 {
            self.blend(x, y, color);
        }
    }

    fn fill(&mut self, [r, g, b, a]: [u8; 4]) {
        if a == 255 {
            for pixel in self.pixels.as_chunks_mut::<3>().0 {
                pixel.copy_from_slice(&[r, g, b]);
            }
        } else {
            for y in 0..i64::from(self.height) {
                self.span(y, 0, i64::from(self.width) - 1, [r, g, b, a]);
            }
        }
    }

    fn rect(&mut self, x: f64, y: f64, w: f64, h: f64, color: [u8; 4]) {
        let (x0, x1) = ordered(x.round(), (x + w).round());
        let (y0, y1) = ordered(y.round(), (y + h).round());
        let y0 = (y0 as i64).max(0);
        let y1 = (y1 as i64).min(i64::from(self.height));
        for row in y0..y1 {
            self.span(row, x0 as i64, x1 as i64 - 1, color);
        }
    }

    fn circle(&mut self, cx: f64, cy: f64, radius: f64, color: [u8; 4]) {
        let r = radius.abs().round() as i64;
        let (cx, cy) = (cx.round() as i64, cy.round() as i64);
        let y0 = (cy - r).max(0);
        let y1 = (cy + r).min(i64::from(self.height) - 1);
        for y in y0..=y1 {
            let dy = y - cy;
            let half = ((r * r - dy * dy).max(0) as f64).sqrt() as i64;
            self.span(y, cx - half, cx + half, color);
        }
    }

    fn line(&mut self, from: (f64, f64), to: (f64, f64), stroke: f64, color: [u8; 4]) {
        let stroke = stroke.clamp(1.0, MAX_STROKE).round() as i64;
        let pad = (stroke / 2 + 1) as f64;
        let Some(((x0, y0), (x1, y1))) = clip(
            from,
            to,
            (-pad, -pad),
            (f64::from(self.width) + pad, f64::from(self.height) + pad),
        ) else {
            return;
        };
        let (x0, y0, x1, y1) = (
            x0.round() as i64,
            y0.round() as i64,
            x1.round() as i64,
            y1.round() as i64,
        );
        let (dx, dy) = ((x1 - x0).abs(), -(y1 - y0).abs());
        let (sx, sy) = (if x0 < x1 { 1 } else { -1 }, if y0 < y1 { 1 } else { -1 });
        let (mut x, mut y, mut error) = (x0, y0, dx + dy);
        let low = (stroke - 1) / 2;
        let high = stroke / 2;
        loop {
            for row in y - low..=y + high {
                if row >= 0 && row < i64::from(self.height) {
                    self.span(row, x - low, x + high, color);
                }
            }
            if x == x1 && y == y1 {
                break;
            }
            let doubled = 2 * error;
            if doubled >= dy {
                error += dy;
                x += sx;
            }
            if doubled <= dx {
                error += dx;
                y += sy;
            }
        }
    }

    pub fn png(&self) -> Result<Vec<u8>> {
        let mut bytes = Vec::new();
        let mut encoder = png::Encoder::new(&mut bytes, self.width, self.height);
        encoder.set_color(png::ColorType::Rgb);
        encoder.set_depth(png::BitDepth::Eight);
        encoder.set_compression(png::Compression::Fast);
        let mut writer = encoder
            .write_header()
            .map_err(|err| Error::Tool(format!("PNG encoding failed: {err}")))?;
        writer
            .write_image_data(&self.pixels)
            .map_err(|err| Error::Tool(format!("PNG encoding failed: {err}")))?;
        writer
            .finish()
            .map_err(|err| Error::Tool(format!("PNG encoding failed: {err}")))?;
        Ok(bytes)
    }
}

fn ordered(a: f64, b: f64) -> (f64, f64) {
    if a <= b { (a, b) } else { (b, a) }
}

/// Liang-Barsky clipping of a segment to a box; `None` when fully outside.
fn clip(
    (x0, y0): (f64, f64),
    (x1, y1): (f64, f64),
    (min_x, min_y): (f64, f64),
    (max_x, max_y): (f64, f64),
) -> Option<((f64, f64), (f64, f64))> {
    let (dx, dy) = (x1 - x0, y1 - y0);
    let (mut t0, mut t1) = (0.0_f64, 1.0_f64);
    for (p, q) in [
        (-dx, x0 - min_x),
        (dx, max_x - x0),
        (-dy, y0 - min_y),
        (dy, max_y - y0),
    ] {
        if p == 0.0 {
            if q < 0.0 {
                return None;
            }
        } else {
            let t = q / p;
            if p < 0.0 {
                t0 = t0.max(t);
            } else {
                t1 = t1.min(t);
            }
            if t0 > t1 {
                return None;
            }
        }
    }
    Some(((x0 + t0 * dx, y0 + t0 * dy), (x0 + t1 * dx, y0 + t1 * dy)))
}

fn coord(value: f64) -> f64 {
    if value.is_nan() {
        0.0
    } else {
        value.clamp(-MAX_COORD, MAX_COORD)
    }
}

pub fn render(args: &DrawArgs, cancel: &CancellationToken) -> Result<Vec<u8>> {
    if args.shapes.is_empty() || args.shapes.len() > MAX_SHAPES {
        return Err(Error::Tool(format!("1..{MAX_SHAPES} shapes are required")));
    }
    let mut canvas = Canvas::new(args.width, args.height)?;
    for (index, shape) in args.shapes.iter().enumerate() {
        if cancel.is_cancelled() {
            return Err(Error::Cancelled);
        }
        let color = parse_color(shape.color.as_deref())
            .map_err(|err| Error::Tool(format!("shape {}: {err}", index + 1)))?;
        let (x, y) = (coord(shape.x), coord(shape.y));
        match shape.kind {
            ShapeKind::Fill => canvas.fill(color),
            ShapeKind::Rect => canvas.rect(x, y, coord(shape.w), coord(shape.h), color),
            ShapeKind::Circle => canvas.circle(x, y, coord(shape.r), color),
            ShapeKind::Line => canvas.line(
                (x, y),
                (coord(shape.x2), coord(shape.y2)),
                shape.stroke_width.unwrap_or(1.0),
                color,
            ),
        }
    }
    canvas.png()
}

/// Resolves a new or existing workspace file like the file tools do: no parent
/// traversal, no Git metadata, no alternate data streams, links must stay inside.
pub fn resolve_output(root: &Path, path: &Path) -> Result<PathBuf> {
    if path.as_os_str().is_empty() || path.components().any(|c| matches!(c, Component::ParentDir)) {
        return Err(Error::Tool(
            "empty paths and parent traversal are not allowed".into(),
        ));
    }
    let root = root.canonicalize()?;
    let candidate = if path.is_absolute() {
        path.to_path_buf()
    } else {
        root.join(path)
    };
    let name = candidate
        .file_name()
        .ok_or_else(|| Error::Tool("missing file name".into()))?
        .to_owned();
    if name.to_string_lossy().contains(':') {
        return Err(Error::Tool("alternate data streams are not allowed".into()));
    }
    let parent = candidate
        .parent()
        .ok_or_else(|| Error::Tool("missing parent".into()))?
        .canonicalize()
        .map_err(|_| Error::Tool("the parent directory must exist".into()))?;
    let resolved = parent.join(name);
    if !resolved.starts_with(&root) {
        return Err(Error::Tool("path is outside the workspace".into()));
    }
    if resolved
        .strip_prefix(&root)
        .map_err(|_| Error::Tool("invalid root".into()))?
        .components()
        .any(|c| c.as_os_str().to_string_lossy().eq_ignore_ascii_case(".git"))
    {
        return Err(Error::Tool(
            "direct access to Git metadata is not allowed".into(),
        ));
    }
    Ok(resolved)
}

/// Writes PNG bytes to a workspace path through the checkpoint; refuses to replace
/// anything that is not already a PNG.
pub async fn write_png(
    root: &Path,
    workspace: &Workspace,
    authorizer: &dyn Authorizer,
    path: &Path,
    bytes: &[u8],
    cancel: &CancellationToken,
) -> Result<PathBuf> {
    let resolved = resolve_output(root, path)?;
    if !resolved
        .extension()
        .is_some_and(|ext| ext.eq_ignore_ascii_case("png"))
    {
        return Err(Error::Tool("output path must end in .png".into()));
    }
    let existing = match std::fs::symlink_metadata(&resolved) {
        Ok(metadata) if !metadata.is_file() => {
            return Err(Error::Tool(
                "output exists and is not a regular file".into(),
            ));
        }
        Ok(_) => {
            let current = std::fs::read(&resolved)?;
            if !current.starts_with(PNG_SIGNATURE) {
                return Err(Error::Tool(
                    "output exists and is not a PNG; refusing to overwrite it".into(),
                ));
            }
            Some(current)
        }
        Err(_) => None,
    };
    authorizer
        .authorize(Effect::Write(resolved.clone()), cancel.clone())
        .await?;
    workspace.replace_bytes(&resolved, existing.as_deref(), bytes)?;
    Ok(resolved)
}

pub async fn execute(
    root: &Path,
    workspace: &Workspace,
    authorizer: &dyn Authorizer,
    input: Value,
    cancel: CancellationToken,
) -> Result<String> {
    let args: DrawArgs = serde_json::from_value(input)?;
    let shapes = args.shapes.len();
    let (width, height) = (args.width, args.height);
    let path = args.output_path.clone();
    let task_cancel = cancel.clone();
    let bytes = tokio::task::spawn_blocking(move || render(&args, &task_cancel))
        .await
        .map_err(|err| Error::Tool(format!("draw task failed: {err}")))??;
    let saved = write_png(root, workspace, authorizer, &path, &bytes, &cancel).await?;
    Ok(format!(
        "Saved PNG ({width}x{height}, {shapes} shapes, {} bytes) to {}",
        bytes.len(),
        crate::workflows::changes::plain(&saved).display()
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn pixel(canvas: &Canvas, x: usize, y: usize) -> [u8; 3] {
        let i = (y * canvas.width as usize + x) * 3;
        [canvas.pixels[i], canvas.pixels[i + 1], canvas.pixels[i + 2]]
    }

    #[test]
    fn colors_parse_hex_rgba_and_names() {
        assert_eq!(parse_color(Some("#ff0000")).unwrap(), [255, 0, 0, 255]);
        assert_eq!(parse_color(Some("#00ff0080")).unwrap(), [0, 255, 0, 128]);
        assert_eq!(
            parse_color(Some("rgba(1,2,3,0.5)")).unwrap(),
            [1, 2, 3, 128]
        );
        assert_eq!(
            parse_color(Some("rgba(1,2,3,200)")).unwrap(),
            [1, 2, 3, 200]
        );
        assert_eq!(parse_color(None).unwrap(), [0, 0, 0, 255]);
        assert!(parse_color(Some("#12")).is_err());
        assert!(parse_color(Some("rgba(1,2)")).is_err());
    }

    #[test]
    fn shapes_paint_expected_pixels_and_clip_far_coordinates() {
        let mut canvas = Canvas::new(20, 20).unwrap();
        canvas.rect(2.0, 2.0, 4.0, 4.0, [255, 0, 0, 255]);
        assert_eq!(pixel(&canvas, 3, 3), [255, 0, 0]);
        assert_eq!(pixel(&canvas, 6, 6), [255, 255, 255]);
        canvas.circle(15.0, 15.0, 3.0, [0, 0, 255, 255]);
        assert_eq!(pixel(&canvas, 15, 15), [0, 0, 255]);
        canvas.line((-1e7, 10.0), (1e7, 10.0), 1.0, [0, 0, 0, 255]);
        assert_eq!(pixel(&canvas, 0, 10), [0, 0, 0]);
        assert_eq!(pixel(&canvas, 19, 10), [0, 0, 0]);
        canvas.rect(0.0, 0.0, 1.0, 1.0, [0, 0, 0, 128]);
        assert_eq!(pixel(&canvas, 0, 0), [127, 127, 127]);
        let png = canvas.png().unwrap();
        assert!(png.starts_with(PNG_SIGNATURE));
    }

    #[test]
    fn canvas_limits_are_enforced() {
        assert!(Canvas::new(0, 1).is_err());
        assert!(Canvas::new(16_385, 1).is_err());
        assert!(Canvas::new(16_384, 16_384).is_err());
    }

    #[tokio::test]
    async fn binary_writes_are_checkpointed_and_undone_byte_for_byte() {
        let root = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        let store = crate::session::Store::open(data.path()).unwrap();
        let workspace = Workspace::new(root.path().to_path_buf(), store).unwrap();
        let authorizer = crate::safety::permission::LocalAuthorizer {
            mode: crate::safety::permission::Mode::Auto,
            input: None,
        };
        let cancel = CancellationToken::new();
        let first = Canvas::new(3, 3).unwrap().png().unwrap();
        std::fs::write(root.path().join("old.png"), &first).unwrap();
        workspace.begin().unwrap();
        let mut second = Canvas::new(5, 5).unwrap();
        second.fill([1, 2, 3, 255]);
        let second = second.png().unwrap();
        let path = Path::new("old.png");
        write_png(root.path(), &workspace, &authorizer, path, &second, &cancel)
            .await
            .unwrap();
        write_png(
            root.path(),
            &workspace,
            &authorizer,
            Path::new("new.png"),
            &first,
            &cancel,
        )
        .await
        .unwrap();
        assert!(workspace.diff().unwrap().contains("Binary file"));
        assert!(workspace.fingerprint().unwrap().is_some());
        workspace.undo().unwrap();
        assert_eq!(std::fs::read(root.path().join("old.png")).unwrap(), first);
        assert!(!root.path().join("new.png").exists());
        assert!(
            write_png(
                root.path(),
                &workspace,
                &authorizer,
                Path::new("x.txt"),
                &first,
                &cancel
            )
            .await
            .is_err()
        );
    }
}
