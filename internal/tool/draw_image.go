package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
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
				"outputPath": {"type":"string","description":"Absolute path to save the PNG file"},
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
							"strokeWidth": {"type":"number","description":"Stroke width in pixels (default 1)"}
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

	shapesRaw, ok := input["shapes"].([]any)
	if !ok || len(shapesRaw) == 0 {
		return Result{Data: "Error: shapes array required with at least one shape", IsError: true}, nil
	}

	// Resolve output path relative to cwd
	fullPath, err := resolvePathInCwd(outPath, tctx, true)
	if err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}

	// Create the image with white background
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	// Default background: white
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)

	// Process shapes
	painted := 0
	for i, raw := range shapesRaw {
		shape, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		shapeType, _ := shape["type"].(string)
		if err := t.drawShape(img, shape, width, height); err != nil {
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

	// Save as PNG
	f, err := os.Create(fullPath)
	if err != nil {
		return Result{Data: "Error: create: " + err.Error(), IsError: true}, nil
	}
	defer f.Close()

	if err := png.Encode(f, img); err != nil {
		return Result{Data: "Error: encode PNG: " + err.Error(), IsError: true}, nil
	}

	return Result{
		Data: fmt.Sprintf("Saved PNG (%dx%d, %d shapes drawn) to %s", width, height, painted, fullPath),
	}, nil
}

func (t *DrawImageTool) drawShape(img *image.RGBA, shape map[string]any, imgW, imgH int) error {
	shapeType, _ := shape["type"].(string)

	switch shapeType {
	case "fill":
		return t.drawFill(img, shape)
	case "rect":
		return t.drawRect(img, shape)
	case "circle":
		return t.drawCircle(img, shape)
	case "line":
		return t.drawLine(img, shape)
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
	x, _ := toFloat(shape["x"])
	y, _ := toFloat(shape["y"])
	w, _ := toFloat(shape["w"])
	h, _ := toFloat(shape["h"])

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

// drawCircle draws a filled circle using the midpoint circle algorithm.
func (t *DrawImageTool) drawCircle(img *image.RGBA, shape map[string]any) error {
	cx, _ := toFloat(shape["x"])
	cy, _ := toFloat(shape["y"])
	rad, _ := toFloat(shape["r"])

	col, err := parseColor(shape, "color")
	if err != nil {
		return fmt.Errorf("circle color: %w", err)
	}

	r := int(math.Round(rad))
	ix := int(math.Round(cx))
	iy := int(math.Round(cy))
	bounds := img.Bounds()

	// Fill circle using bounding box scan
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			if dx*dx+dy*dy <= r*r {
				px := ix + dx
				py := iy + dy
				if px >= bounds.Min.X && px < bounds.Max.X && py >= bounds.Min.Y && py < bounds.Max.Y {
					img.Set(px, py, col)
				}
			}
		}
	}
	return nil
}

// drawLine draws a line using Bresenham's algorithm.
func (t *DrawImageTool) drawLine(img *image.RGBA, shape map[string]any) error {
	x0, _ := toFloat(shape["x"])
	y0, _ := toFloat(shape["y"])
	x1, _ := toFloat(shape["x2"])
	y1, _ := toFloat(shape["y2"])

	col, err := parseColor(shape, "color")
	if err != nil {
		return fmt.Errorf("line color: %w", err)
	}

	strokeWidth := 1.0
	if sw, ok := shape["strokeWidth"]; ok {
		if v, err := toFloat(sw); err == nil && v > 0 {
			strokeWidth = v
		}
	}

	ix0, iy0 := int(math.Round(x0)), int(math.Round(y0))
	ix1, iy1 := int(math.Round(x1)), int(math.Round(y1))

	bresenhamLine(img, ix0, iy0, ix1, iy1, col, strokeWidth)
	return nil
}

// bresenhamLine draws a line with optional stroke width.
func bresenhamLine(img *image.RGBA, x0, y0, x1, y1 int, col color.Color, strokeW float64) {
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
	sw := int(math.Max(1, math.Round(strokeW)))

	for {
		// Draw a small square around each point for stroke width
		for wy := -sw / 2; wy <= sw/2; wy++ {
			for wx := -sw / 2; wx <= sw/2; wx++ {
				px := x0 + wx
				py := y0 + wy
				if px >= img.Bounds().Min.X && px < img.Bounds().Max.X &&
					py >= img.Bounds().Min.Y && py < img.Bounds().Max.Y {
					img.Set(px, py, col)
				}
			}
		}

		if x0 == x1 && y0 == y1 {
			break
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
			return color.RGBA{uint8(r), uint8(g), uint8(b), uint8(a)}, nil
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
		return color.RGBA{uint8(r), uint8(g), uint8(b), 255}, nil
	}

	return color.Black, nil
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
