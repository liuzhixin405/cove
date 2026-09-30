package tool

import (
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// callDrawWithin runs draw_image and fails if it does not return within d:
// the circle and line loops used to scan their whole unclipped extent.
func callDrawWithin(t *testing.T, ctx context.Context, d time.Duration, input Input, tctx Context) Result {
	t.Helper()
	type out struct {
		res Result
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := NewDrawImageTool().Call(ctx, input, tctx)
		done <- out{res, err}
	}()
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatal(o.err)
		}
		return o.res
	case <-time.After(d):
		t.Fatalf("draw_image did not return within %v", d)
		return Result{}
	}
}

func TestDrawImageHugeShapesAreClipped(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "big.png")
	res := callDrawWithin(t, context.Background(), 5*time.Second, Input{
		"outputPath": out, "width": 100.0, "height": 100.0,
		"shapes": []any{
			map[string]any{"type": "circle", "x": 50.0, "y": 50.0, "r": 100000.0, "color": "red"},
			map[string]any{"type": "line", "x": -1e9, "y": -1e9, "x2": 1e9, "y2": 1e9, "strokeWidth": 1e6, "color": "blue"},
			map[string]any{"type": "rect", "x": -1e18, "y": -1e18, "w": 1e19, "h": 1e19, "color": "#00FF0080"},
		},
	}, Context{Cwd: dir})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Data)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	// The circle covers the whole canvas.
	if r, _, b, _ := img.At(0, 99).RGBA(); r>>8 < 100 || b>>8 != 0 {
		t.Errorf("corner not painted by the clipped circle: %v", img.At(0, 99))
	}
	// The line passes through the diagonal.
	if _, _, b, _ := img.At(50, 50).RGBA(); b>>8 < 100 {
		t.Errorf("diagonal not painted by the clipped line: %v", img.At(50, 50))
	}
}

func TestDrawImageLineClippingKeepsInsidePixels(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "l.png")
	res := callDrawWithin(t, context.Background(), 5*time.Second, Input{
		"outputPath": out, "width": 20.0, "height": 20.0,
		"shapes": []any{
			map[string]any{"type": "line", "x": -1000.0, "y": 10.0, "x2": 1000.0, "y2": 10.0, "color": "black"},
		},
	}, Context{Cwd: dir})
	if res.IsError {
		t.Fatal(res.Data)
	}
	f, _ := os.Open(out)
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	for x := 0; x < 20; x++ {
		if r, _, _, _ := img.At(x, 10).RGBA(); r != 0 {
			t.Errorf("pixel (%d,10) not on the clipped line", x)
		}
		if r, _, _, _ := img.At(x, 9).RGBA(); r == 0 {
			t.Errorf("pixel (%d,9) painted by a 1px line", x)
		}
	}
}

func TestDrawImageHonoursCancel(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := callDrawWithin(t, ctx, 5*time.Second, Input{
		"outputPath": filepath.Join(dir, "c.png"), "width": 100.0, "height": 100.0,
		"shapes": []any{map[string]any{"type": "circle", "x": 50.0, "y": 50.0, "r": 40.0}},
	}, Context{Cwd: dir})
	if !res.IsError {
		t.Errorf("cancelled draw reported success: %s", res.Data)
	}
	if _, err := os.Stat(filepath.Join(dir, "c.png")); err == nil {
		t.Error("cancelled draw still wrote the file")
	}
}

// outputPath went straight to os.Create, so a wrong path silently truncated
// whatever was there — main.go included.
func TestDrawImageRefusesToClobberNonImage(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "notes.png")
	if err := os.WriteFile(fake, []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	shapes := []any{map[string]any{"type": "fill", "color": "red"}}
	for _, p := range []string{src, fake, filepath.Join(dir, "new.txt")} {
		res, err := NewDrawImageTool().Call(context.Background(),
			Input{"outputPath": p, "width": 4.0, "height": 4.0, "shapes": shapes}, Context{Cwd: dir})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Errorf("%s: write accepted: %s", filepath.Base(p), res.Data)
		}
	}
	if b, _ := os.ReadFile(src); string(b) != "package main\n" {
		t.Errorf("main.go changed: %q", b)
	}
	if b, _ := os.ReadFile(fake); string(b) != "not an image" {
		t.Errorf("notes.png changed: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err == nil {
		t.Error("new.txt created")
	}

	// An existing PNG may be regenerated; the extension check is case-blind.
	img := filepath.Join(dir, "chart.PNG")
	for i := 0; i < 2; i++ {
		res, err := NewDrawImageTool().Call(context.Background(),
			Input{"outputPath": img, "width": 4.0, "height": 4.0, "shapes": shapes}, Context{Cwd: dir})
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Fatalf("pass %d: %s", i, res.Data)
		}
	}
	if !strings.HasSuffix(img, ".PNG") {
		t.Fatal("unreachable")
	}
}

// The span fill paints exactly the pixels the old per-pixel test did.
func TestDrawCircleMatchesPixelTest(t *testing.T) {
	for _, c := range []struct{ x, y, r int }{{10, 10, 0}, {10, 10, 1}, {10, 10, 7}, {0, 19, 12}, {-3, 5, 9}, {15, 15, 30}} {
		img := image.NewRGBA(image.Rect(0, 0, 20, 20))
		err := (&DrawImageTool{}).drawCircle(context.Background(), img,
			map[string]any{"x": float64(c.x), "y": float64(c.y), "r": float64(c.r), "color": "black"})
		if err != nil {
			t.Fatal(err)
		}
		for py := 0; py < 20; py++ {
			for px := 0; px < 20; px++ {
				dx, dy := px-c.x, py-c.y
				want := dx*dx+dy*dy <= c.r*c.r
				got := img.RGBAAt(px, py).A != 0
				if got != want {
					t.Errorf("circle %+v: pixel (%d,%d) painted=%v, want %v", c, px, py, got, want)
				}
			}
		}
	}
}
