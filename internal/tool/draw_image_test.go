package tool

import (
	"context"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestDrawImage_GeneratesValidPNG(t *testing.T) {
	tmpDir := t.TempDir()
	outPath := filepath.Join(tmpDir, "test_output.png")

	tool := NewDrawImageTool()
	input := Input{
		"outputPath": outPath,
		"width":      100.0,
		"height":     100.0,
		"shapes": []any{
			map[string]any{
				"type":  "fill",
				"color": "#FFFFFF",
			},
			map[string]any{
				"type":  "rect",
				"x":     10.0,
				"y":     10.0,
				"w":     80.0,
				"h":     80.0,
				"color": "#FF0000",
			},
			map[string]any{
				"type":  "circle",
				"x":     50.0,
				"y":     50.0,
				"r":     30.0,
				"color": "#0000FF",
			},
		},
	}

	result, err := tool.Call(context.Background(), input, Context{Cwd: tmpDir})
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("Call returned isError: %s", result.Data)
	}
	t.Logf("Result: %s", result.Data)

	// Verify file exists and is a valid PNG
	if _, err := os.Stat(outPath); os.IsNotExist(err) {
		t.Fatal("output file was not created")
	}
	f, err := os.Open(outPath)
	if err != nil {
		t.Fatalf("cannot open output: %v", err)
	}
	defer f.Close()

	cfg, err := png.DecodeConfig(f)
	if err != nil {
		t.Fatalf("not a valid PNG: %v", err)
	}
	if cfg.Width != 100 || cfg.Height != 100 {
		t.Fatalf("expected 100x100, got %dx%d", cfg.Width, cfg.Height)
	}

	t.Logf("Valid PNG: %dx%d, %s", cfg.Width, cfg.Height, outPath)
}

func TestDrawImage_LineTool(t *testing.T) {
	tmpDir := t.TempDir()
	outPath := filepath.Join(tmpDir, "line_test.png")

	tool := NewDrawImageTool()
	input := Input{
		"outputPath": outPath,
		"width":      50.0,
		"height":     50.0,
		"shapes": []any{
			map[string]any{
				"type":  "line",
				"x":     5.0,
				"y":     5.0,
				"x2":    45.0,
				"y2":    45.0,
				"color": "#00FF00",
			},
		},
	}

	result, err := tool.Call(context.Background(), input, Context{Cwd: tmpDir})
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("Call returned isError: %s", result.Data)
	}
	t.Logf("Result: %s", result.Data)

	// Verify valid PNG
	f, err := os.Open(outPath)
	if err != nil {
		t.Fatalf("cannot open output: %v", err)
	}
	defer f.Close()

	if _, err := png.DecodeConfig(f); err != nil {
		t.Fatalf("not a valid PNG: %v", err)
	}
}

func TestDrawImage_InvalidPath(t *testing.T) {
	tool := NewDrawImageTool()
	input := Input{
		"outputPath": "",
		"width":      100.0,
		"height":     100.0,
		"shapes": []any{
			map[string]any{"type": "fill", "color": "#000"},
		},
	}
	result, err := tool.Call(context.Background(), input, Context{})
	if err != nil {
		t.Fatal("expected no error for missing path, got:", err)
	}
	if !result.IsError {
		t.Fatal("expected isError for missing path")
	}
	t.Logf("Got expected error: %s", result.Data)
}

func TestDrawImage_ColorParsing(t *testing.T) {
	tests := []struct {
		input string
		name  string
	}{
		{"#FF0000", "hex6"},
		{"#FF0000FF", "hex8"},
		{"rgba(255,0,0,255)", "rgba"},
		{"red", "named"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			outPath := filepath.Join(tmpDir, "color_test.png")

			tool := NewDrawImageTool()
			input := Input{
				"outputPath": outPath,
				"width":      10.0,
				"height":     10.0,
				"shapes": []any{
					map[string]any{"type": "fill", "color": tt.input},
				},
			}
			result, err := tool.Call(context.Background(), input, Context{Cwd: tmpDir})
			if err != nil {
				t.Fatalf("Call error: %v", err)
			}
			if result.IsError {
				t.Fatalf("unexpected isError: %s", result.Data)
			}
		})
	}
}
