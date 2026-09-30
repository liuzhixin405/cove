package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type DrawImageTool struct{ baseTool }

func NewDrawImageTool() Tool {
	return &DrawImageTool{baseTool{def: Def{
		Name: "draw_image",
		Description: "Generate a PNG image programmatically using drawing primitives. " +
			"Supports rectangles, circles, lines, and solid fill. " +
			"Colors are specified as hex (#RRGGBB or #RRGGBBAA) or rgba(r,g,b,a).",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"outputPath": {"type":"string","description":"Absolute path to save the PNG file (must end in .png; an existing file is only replaced if it is a PNG)"},
				"width": {"type":"integer","description":"Image width in pixels","minimum":1},
				"height": {"type":"integer","description":"Image height in pixels","minimum":1},
				"shapes": {
					"type":"array",
					"items":{
						"type":"object",
						"properties":{
							"type": {"type":"string","enum":["rect","circle","line","fill"],"description":"Shape type"},
							"x": {"type":"number","description":"X coordinate (or center X for circle, start X for line)"},
							"y": {"type":"number","description":"Y coordinate (or center Y for circle, start Y for line)"},
							"w": {"type":"number","description":"Width (rect only)"},
							"h": {"type":"number","description":"Height (rect only)"},
							"r": {"type":"number","description":"Radius (circle only)"},
							"x2": {"type":"number","description":"End X (line only)"},
							"y2": {"type":"number","description":"End Y (line only)"},
							"color": {"type":"string","description":"Stroke/fill color as hex or rgba"},
							"strokeWidth": {"type":"number","description":"Stroke width in pixels (default 1, at most 64)"}
						},
						"required":["type"]
					}
				}
			},
			"required":["outputPath","width","height","shapes"]
		}`),
		IsReadOnly: false, IsConcurrencySafe: false, UserFacingName: "DrawImage",
	}}}
}

// Canvas limits for the draw_image tool. The pixel cap is the binding one:
// maxDrawPixels * 4 bytes/pixel ≈ 256 MB of RGBA, which is already far more
// than any diagram this tool is meant to produce.
const (
	maxDrawDimension = 16384
	maxDrawPixels    = 64 << 20 // 67,108,864 pixels
	// maxDrawStroke caps strokeWidth: every line point stamps a sw x sw
	// square, so an uncapped 1e6 meant 1e12 pixel writes per point.
	maxDrawStroke = 64
	// maxDrawCoord bounds coordinates and sizes before they are rounded to
	// int. Far beyond any canvas, yet small enough that x+w and r*r cannot
	// overflow.
	maxDrawCoord = 1 << 24
)

// pngSignature starts every PNG file.
var pngSignature = []byte("\x89PNG\r\n\x1a\n")

func (t *DrawImageTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	outPath, _ := input["outputPath"].(string)
	width, _ := toInt(input["width"])
	height, _ := toInt(input["height"])

	if outPath == "" {
		return Result{Data: "Error: outputPath required", IsError: true}, nil
	}
	if width <= 0 || height <= 0 {
		return Result{Data: "Error: width and height must be positive", IsError: true}, nil
	}
	// Cap the canvas. image.NewRGBA allocates width*height*4 bytes eagerly, so
	// an LLM-supplied 100000x100000 (40 GB) killed the process outright — and on
	// a 32-bit build the product overflows int before any of it is checked.
	if width > maxDrawDimension || height > maxDrawDimension {
		return Result{Data: fmt.Sprintf(
			"Error: width and height must each be at most %d px (got %dx%d)",
			maxDrawDimension, width, height), IsError: true}, nil
	}
	if int64(width)*int64(height) > maxDrawPixels {
		return Result{Data: fmt.Sprintf(
			"Error: canvas too large: %dx%d is %d pixels, limit is %d",
			width, height, int64(width)*int64(height), int64(maxDrawPixels)), IsError: true}, nil
	}

	shapesRaw, ok := input["shapes"].([]any)
	if !ok || len(shapesRaw) == 0 {
		return Result{Data: "Error: shapes array required with at least one shape", IsError: true}, nil
	}

	// Resolve output path relative to cwd
	fullPath, err := resolvePathInCwd(outPath, tctx, true)
	if err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}
	// The output used to go straight to os.Create, so a mistaken outputPath
	// (main.go, a config) was silently truncated and replaced by PNG bytes,
	// with none of write's read-before-write protection. Only .png paths are
	// accepted, and an existing file is replaced only if it already is a PNG.
	if hasStreamSeparator(fullPath, runtime.GOOS) {
		return Result{Data: "Error: outputPath must not contain ':' (on Windows it names an alternate data stream of another file): " + fullPath, IsError: true}, nil
	}
	if !strings.EqualFold(filepath.Ext(fullPath), ".png") {
		return Result{Data: "Error: outputPath must end in .png: " + fullPath, IsError: true}, nil
	}
	if err := checkReplaceableImage(fullPath); err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}

	// Create the image with white background
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	// Default background: white
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)

	// Process shapes
	painted := 0
	for i, raw := range shapesRaw {
		if ctx.Err() != nil {
			return Result{Data: "Error: draw_image cancelled", IsError: true}, nil
		}
		shape, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		shapeType, _ := shape["type"].(string)
		if err := t.drawShape(ctx, img, shape); err != nil {
			if ctx.Err() != nil {
				return Result{Data: "Error: draw_image cancelled", IsError: true}, nil
			}
			return Result{
				Data:    fmt.Sprintf("Error drawing shape %d (%s): %s", i+1, shapeType, err.Error()),
				IsError: true,
			}, nil
		}
		painted++
	}

	// Ensure output directory exists
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return Result{Data: "Error: mkdir: " + err.Error(), IsError: true}, nil
	}

	// Encode first, then replace atomically like write does: a failed
	// encode or a full disk leaves any previous image intact.
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return Result{Data: "Error: encode PNG: " + err.Error(), IsError: true}, nil
	}
	if err := replaceFile(fullPath, buf.Bytes()); err != nil {
		return Result{Data: "Error: write: " + err.Error(), IsError: true}, nil
	}

	return Result{
		Data: fmt.Sprintf("Saved PNG (%dx%d, %d shapes drawn) to %s", width, height, painted, fullPath),
	}, nil
}

// checkReplaceableImage refuses an existing path that is not a PNG file.
func checkReplaceableImage(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s exists and is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, len(pngSignature))
	n, _ := f.Read(head)
	if !bytes.Equal(head[:n], pngSignature) {
		return fmt.Errorf("%s exists and is not a PNG image; refusing to overwrite it", path)
	}
	return nil
}

// drawCoord reads a coordinate or size, clamped to +-maxDrawCoord (NaN is 0)
// so that rounding to int and the arithmetic after it cannot overflow.
func drawCoord(shape map[string]any, key string) float64 {
	v, _ := toFloat(shape[key])
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(-maxDrawCoord, math.Min(maxDrawCoord, v))
}

func (t *DrawImageTool) drawShape(ctx context.Context, img *image.RGBA, shape map[string]any) error {
	shapeType, _ := shape["type"].(string)

	switch shapeType {
	case "fill":
		return t.drawFill(img, shape)
	case "rect":
		return t.drawRect(img, shape)
	case "circle":
		return t.drawCircle(ctx, img, shape)
	case "line":
		return t.drawLine(ctx, img, shape)
	default:
		return fmt.Errorf("unknown shape type: %s", shapeType)
	}
}

// drawFill fills the entire canvas with a solid color.
func (t *DrawImageTool) drawFill(img *image.RGBA, shape map[string]any) error {
	c, err := parseColor(shape, "color")
	if err != nil {
		return fmt.Errorf("fill color: %w", err)
	}
	draw.Draw(img, img.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
	return nil
}

// drawRect draws a filled rectangle.
func (t *DrawImageTool) drawRect(img *image.RGBA, shape map[string]any) error {
	x, y := drawCoord(shape, "x"), drawCoord(shape, "y")
	w, h := drawCoord(shape, "w"), drawCoord(shape, "h")

	c, err := parseColor(shape, "color")
	if err != nil {
		return fmt.Errorf("rect color: %w", err)
	}

	ix := int(math.Round(x))
	iy := int(math.Round(y))
	iw := int(math.Round(w))
	ih := int(math.Round(h))

	rect := image.Rect(ix, iy, ix+iw, iy+ih)
	draw.Draw(img, rect, image.NewUniform(c), image.Point{}, draw.Over)
	return nil
}

// drawCircle draws a filled circle.
//
// It used to scan the whole (2r+1)^2 bounding box and clip each pixel, so
// r=100000 on a 100x100 canvas meant 4e10 iterations. Now only the rows
// inside the canvas are visited, each filling one clipped span.
func (t *DrawImageTool) drawCircle(ctx context.Context, img *image.RGBA, shape map[string]any) error {
	cx, cy, rad := drawCoord(shape, "x"), drawCoord(shape, "y"), drawCoord(shape, "r")

	col, err := parseColor(shape, "color")
	if err != nil {
		return fmt.Errorf("circle color: %w", err)
	}

	r := int64(math.Round(rad))
	ix := int64(math.Round(cx))
	iy := int64(math.Round(cy))
	b := img.Bounds()

	y0 := max(iy-r, int64(b.Min.Y))
	y1 := min(iy+r, int64(b.Max.Y-1))
	for py := y0; py <= y1; py++ {
		if (py-y0)%256 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		dy := py - iy
		// The widest dx with dx*dx+dy*dy <= r*r, the test the per-pixel scan
		// made; the float estimate is corrected in integers.
		half := int64(math.Sqrt(float64(r*r - dy*dy)))
		for half > 0 && half*half+dy*dy > r*r {
			half--
		}
		for (half+1)*(half+1)+dy*dy <= r*r {
			half++
		}
		x0 := max(ix-half, int64(b.Min.X))
		x1 := min(ix+half, int64(b.Max.X-1))
		for px := x0; px <= x1; px++ {
			img.Set(int(px), int(py), col)
		}
	}
	return nil
}

// drawLine draws a line using Bresenham's algorithm.
func (t *DrawImageTool) drawLine(ctx context.Context, img *image.RGBA, shape map[string]any) error {
	col, err := parseColor(shape, "color")
	if err != nil {
		return fmt.Errorf("line color: %w", err)
	}

	strokeWidth := 1.0
	if sw, ok := shape["strokeWidth"]; ok {
		if v, err := toFloat(sw); err == nil && v > 0 {
			strokeWidth = min(v, maxDrawStroke)
		}
	}

	x0, y0 := math.Round(drawCoord(shape, "x")), math.Round(drawCoord(shape, "y"))
	x1, y1 := math.Round(drawCoord(shape, "x2")), math.Round(drawCoord(shape, "y2"))

	// Bresenham used to step every point of the segment, off-canvas ones
	// included, so a line from -1e9 to 1e9 never finished. Clip it to the
	// canvas widened by half the stroke first; a segment already inside is
	// drawn exactly as before.
	sw := int(math.Max(1, math.Round(strokeWidth)))
	pad := float64(sw/2) + 1
	b := img.Bounds()
	cx0, cy0, cx1, cy1, ok := clipSegment(x0, y0, x1, y1,
		float64(b.Min.X)-pad, float64(b.Min.Y)-pad, float64(b.Max.X-1)+pad, float64(b.Max.Y-1)+pad)
	if !ok {
		return nil
	}
	return bresenhamLine(ctx, img,
		int(math.Round(cx0)), int(math.Round(cy0)), int(math.Round(cx1)), int(math.Round(cy1)), col, sw)
}

// clipSegment clips the segment (x0,y0)-(x1,y1) to the rectangle
// [minX,maxX]x[minY,maxY] (Liang-Barsky). ok is false when no part of it is
// inside.
func clipSegment(x0, y0, x1, y1, minX, minY, maxX, maxY float64) (cx0, cy0, cx1, cy1 float64, ok bool) {
	dx, dy := x1-x0, y1-y0
	t0, t1 := 0.0, 1.0
	for _, e := range [4][2]float64{{-dx, x0 - minX}, {dx, maxX - x0}, {-dy, y0 - minY}, {dy, maxY - y0}} {
		p, q := e[0], e[1]
		if p == 0 {
			if q < 0 {
				return 0, 0, 0, 0, false
			}
			continue
		}
		r := q / p
		if p < 0 {
			t0 = math.Max(t0, r)
		} else {
			t1 = math.Min(t1, r)
		}
		if t0 > t1 {
			return 0, 0, 0, 0, false
		}
	}
	return x0 + t0*dx, y0 + t0*dy, x0 + t1*dx, y0 + t1*dy, true
}

// bresenhamLine draws a line sw pixels wide, checking ctx as it goes.
func bresenhamLine(ctx context.Context, img *image.RGBA, x0, y0, x1, y1 int, col color.Color, sw int) error {
	dx := abs(x1 - x0)
	dy := abs(y1 - y0)
	sx := 1
	if x0 > x1 {
		sx = -1
	}
	sy := 1
	if y0 > y1 {
		sy = -1
	}
	err := dx - dy
	b := img.Bounds()

	for step := 0; ; step++ {
		if step%1024 == 0 {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
		}
		// Draw a small square around each point for stroke width
		for wy := -sw / 2; wy <= sw/2; wy++ {
			for wx := -sw / 2; wx <= sw/2; wx++ {
				px := x0 + wx
				py := y0 + wy
				if px >= b.Min.X && px < b.Max.X && py >= b.Min.Y && py < b.Max.Y {
					img.Set(px, py, col)
				}
			}
		}

		if x0 == x1 && y0 == y1 {
			return nil
		}
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			x0 += sx
		}
		if e2 < dx {
			err += dx
			y0 += sy
		}
	}
}

// parseColor extracts a color from a shape map, supporting hex (#RRGGBB, #RRGGBBAA)
// and rgba(r,g,b,a) format.
func parseColor(shape map[string]any, key string) (color.Color, error) {
	raw, ok := shape[key]
	if !ok {
		return color.Black, nil
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return color.Black, nil
	}

	s = strings.TrimSpace(s)

	// rgba(r,g,b,a) format
	if strings.HasPrefix(s, "rgba(") && strings.HasSuffix(s, ")") {
		inner := strings.TrimPrefix(s, "rgba(")
		inner = strings.TrimSuffix(inner, ")")
		parts := strings.Split(inner, ",")
		if len(parts) == 4 {
			r, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
			g, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
			b, _ := strconv.Atoi(strings.TrimSpace(parts[2]))
			a, _ := strconv.Atoi(strings.TrimSpace(parts[3]))
			return color.RGBA{clampByte(r), clampByte(g), clampByte(b), clampByte(a)}, nil
		}
	}

	// Hex format
	if strings.HasPrefix(s, "#") {
		hex := s[1:]
		if len(hex) == 6 {
			r, _ := strconv.ParseUint(hex[0:2], 16, 8)
			g, _ := strconv.ParseUint(hex[2:4], 16, 8)
			b, _ := strconv.ParseUint(hex[4:6], 16, 8)
			return color.RGBA{uint8(r), uint8(g), uint8(b), 255}, nil
		}
		if len(hex) == 8 {
			r, _ := strconv.ParseUint(hex[0:2], 16, 8)
			g, _ := strconv.ParseUint(hex[2:4], 16, 8)
			b, _ := strconv.ParseUint(hex[4:6], 16, 8)
			a, _ := strconv.ParseUint(hex[6:8], 16, 8)
			return color.RGBA{uint8(r), uint8(g), uint8(b), uint8(a)}, nil
		}
	}

	// Named colors
	switch strings.ToLower(s) {
	case "red":
		return color.RGBA{255, 0, 0, 255}, nil
	case "green":
		return color.RGBA{0, 255, 0, 255}, nil
	case "blue":
		return color.RGBA{0, 0, 255, 255}, nil
	case "black":
		return color.RGBA{0, 0, 0, 255}, nil
	case "white":
		return color.RGBA{255, 255, 255, 255}, nil
	case "yellow":
		return color.RGBA{255, 255, 0, 255}, nil
	case "cyan":
		return color.RGBA{0, 255, 255, 255}, nil
	case "magenta":
		return color.RGBA{255, 0, 255, 255}, nil
	case "gray", "grey":
		return color.RGBA{128, 128, 128, 255}, nil
	case "orange":
		return color.RGBA{255, 165, 0, 255}, nil
	case "purple":
		return color.RGBA{128, 0, 128, 255}, nil
	case "pink":
		return color.RGBA{255, 192, 203, 255}, nil
	case "brown":
		return color.RGBA{165, 42, 42, 255}, nil
	}

	// Fallback: parse as plain RGB integers
	if parts := strings.Split(s, ","); len(parts) == 3 {
		r, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
		g, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
		b, _ := strconv.Atoi(strings.TrimSpace(parts[2]))
		return color.RGBA{clampByte(r), clampByte(g), clampByte(b), 255}, nil
	}

	return color.Black, nil
}

// clampByte clamps an integer color component parsed from user input (which
// may be out of the 0-255 range or negative) to a valid uint8 instead of
// silently wrapping on conversion.
func clampByte(n int) uint8 {
	if n < 0 {
		return 0
	}
	if n > 255 {
		return 255
	}
	return uint8(n)
}

func (t *DrawImageTool) Validate(input Input) string {
	outPath, _ := input["outputPath"].(string)
	if outPath == "" {
		return "outputPath is required"
	}
	w, _ := toInt(input["width"])
	if w <= 0 {
		return "width must be a positive integer"
	}
	h, _ := toInt(input["height"])
	if h <= 0 {
		return "height must be a positive integer"
	}
	shapes, ok := input["shapes"].([]any)
	if !ok || len(shapes) == 0 {
		return "shapes array is required"
	}
	return ""
}

func (t *DrawImageTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	switch tctx.PermissionMode {
	case "bypass", "auto":
		return Allowed("mode: " + tctx.PermissionMode)
	case "plan":
		return Denied("plan mode: write not allowed")
	}
	return Asked("draw_image creates a PNG file on the filesystem")
}

// --- Helpers ---

func toInt(v any) (int, error) {
	switch n := v.(type) {
	case float64:
		return int(n), nil
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case json.Number:
		return strconv.Atoi(n.String())
	case string:
		return strconv.Atoi(n)
	}
	return 0, fmt.Errorf("cannot convert %T to int", v)
}

func toFloat(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case json.Number:
		return n.Float64()
	case string:
		return strconv.ParseFloat(n, 64)
	}
	return 0, fmt.Errorf("cannot convert %T to float", v)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
