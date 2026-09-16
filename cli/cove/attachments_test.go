package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBuildUserMessageParsesInlineAttachments(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(filePath, []byte("hello attachment"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	msg, _, err := buildUserMessage("请总结 @note.txt", dir, nil, "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("buildUserMessage returned error: %v", err)
	}
	if got, want := msg.Content, "请总结"; got != want {
		t.Fatalf("Content = %q, want %q", got, want)
	}
	if len(msg.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(msg.Parts))
	}
	if msg.Parts[0].Type != "text" {
		t.Fatalf("part type = %q, want text", msg.Parts[0].Type)
	}
	if !strings.Contains(msg.Parts[0].Text, "hello attachment") {
		t.Fatalf("text part missing file content: %q", msg.Parts[0].Text)
	}
}

func TestBuildUserMessageBuildsImagePart(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "img.png")

	// Create a valid 100x100 PNG image
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	// Fill with red
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			img.Set(x, y, color.RGBA{R: 255, G: 0, B: 0, A: 255})
		}
	}
	f, err := os.Create(imgPath)
	if err != nil {
		t.Fatalf("create png: %v", err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}

	msg, warnings, err := buildUserMessage("看图", dir, []string{imgPath}, "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("buildUserMessage returned error: %v", err)
	}
	if len(warnings) > 0 {
		t.Logf("warnings (expected none for vision model): %v", warnings)
	}
	if len(msg.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(msg.Parts))
	}
	part := msg.Parts[0]
	if part.Type != "image" {
		t.Fatalf("part type = %q, want image", part.Type)
	}
	// After processing, it should be JPEG
	if part.MimeType != "image/jpeg" {
		t.Fatalf("mime = %q, want image/jpeg (images are converted to JPEG)", part.MimeType)
	}
	// Verify it's valid base64
	if _, err := base64.StdEncoding.DecodeString(part.Data); err != nil {
		t.Fatalf("invalid base64: %v", err)
	}
}

func TestBuildUserMessageWarnsNonVisionModel(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "img.png")
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	f, err := os.Create(imgPath)
	if err != nil {
		t.Fatalf("create png: %v", err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatalf("encode png: %v", err)
	}
	f.Close()

	_, warnings, err := buildUserMessage("看图", dir, []string{imgPath}, "deepseek-reasoner")
	if err != nil {
		t.Fatalf("buildUserMessage returned error: %v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "可能不支持图片") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected warning for non-vision model, got: %v", warnings)
	}
}

func TestBuildUserMessageReturnsErrorForMissingAttachment(t *testing.T) {
	_, _, err := buildUserMessage("分析 @missing.txt", t.TempDir(), nil, "")
	if err == nil {
		t.Fatalf("expected error for missing file")
	}
}

func TestSplitQuotedFieldsSupportsSpacesInPaths(t *testing.T) {
	got, err := splitQuotedFields(`"screen shot.png" logs/app.log 'notes final.txt'`)
	if err != nil {
		t.Fatalf("splitQuotedFields returned error: %v", err)
	}
	want := []string{"screen shot.png", "logs/app.log", "notes final.txt"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("fields = %#v, want %#v", got, want)
	}
}

func TestAddAttachmentsNormalizesAndDeduplicates(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(filePath, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	var attached []string
	addAttachments([]string{"note.txt", "@note.txt"}, dir, &attached)
	if len(attached) != 1 {
		t.Fatalf("attached = %#v, want one normalized path", attached)
	}
	if attached[0] != filePath {
		t.Fatalf("attached path = %q, want %q", attached[0], filePath)
	}
}

func TestProcessImageResizeAndCompress(t *testing.T) {
	// Create a large 2048x2048 PNG (should be resized to 1568x1568)
	img := image.NewRGBA(image.Rect(0, 0, 2048, 2048))
	for y := 0; y < 2048; y++ {
		for x := 0; x < 2048; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: uint8((x + y) % 256), A: 255})
		}
	}
	// Encode as PNG then process
	f, err := os.CreateTemp(t.TempDir(), "large-*.png")
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}

	// Process
	processed, mime, err := processImage(raw)
	if err != nil {
		t.Fatalf("processImage failed: %v", err)
	}
	if mime != "image/jpeg" {
		t.Fatalf("expected image/jpeg, got %s", mime)
	}
	if len(processed) > maxImageBytes {
		t.Fatalf("processed image too large: %d bytes > %d", len(processed), maxImageBytes)
	}
	// Decode result to verify dimensions
	decoded, _, err := decodeAndCheck(processed)
	if err != nil {
		t.Fatalf("failed to decode processed image: %v", err)
	}
	bounds := decoded.Bounds()
	if bounds.Dx() > maxImageDim || bounds.Dy() > maxImageDim {
		t.Fatalf("processed image dimensions %dx%d exceed max %d", bounds.Dx(), bounds.Dy(), maxImageDim)
	}
	t.Logf("original=%d bytes -> processed=%d bytes (%dx%d)", len(raw), len(processed), bounds.Dx(), bounds.Dy())
}

func decodeAndCheck(data []byte) (image.Image, string, error) {
	return image.Decode(bytes.NewReader(data))
}

func TestDecodeImageFormats(t *testing.T) {
	dir := t.TempDir()

	// PNG
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	pngPath := filepath.Join(dir, "test.png")
	f, _ := os.Create(pngPath)
	png.Encode(f, img)
	f.Close()
	raw, _ := os.ReadFile(pngPath)
	if _, err := decodeImage(raw); err != nil {
		t.Errorf("failed to decode PNG: %v", err)
	}

	// Unsupported format
	if _, err := decodeImage([]byte("not an image")); err == nil {
		t.Error("expected error for invalid image data")
	}
}

func TestStripAlpha(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 10, 10))
	result := stripAlpha(img)
	if result == nil {
		t.Fatal("stripAlpha returned nil")
	}
	if result.Bounds().Dx() != 10 || result.Bounds().Dy() != 10 {
		t.Error("stripAlpha changed dimensions")
	}
}

func TestDetectMimeType(t *testing.T) {
	// PNG magic bytes
	pngSig := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	mime := detectMimeType("screenshot.png", pngSig)
	if !strings.HasPrefix(mime, "image/png") {
		t.Errorf("expected image/png, got %s", mime)
	}

	// Extension-based
	mime = detectMimeType("document.pdf", []byte("%PDF"))
	if !strings.Contains(mime, "pdf") {
		t.Errorf("expected pdf mime, got %s", mime)
	}
}

func TestResizeImage(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4000, 2000))
	resized := resizeImage(img, 4000, 2000, 1568)
	bounds := resized.Bounds()
	if bounds.Dx() != 1568 || bounds.Dy() != 784 {
		t.Errorf("expected 1568x784, got %dx%d", bounds.Dx(), bounds.Dy())
	}

	// Tall image
	img2 := image.NewRGBA(image.Rect(0, 0, 100, 3000))
	resized2 := resizeImage(img2, 100, 3000, 1568)
	bounds2 := resized2.Bounds()
	if bounds2.Dx() != 52 || bounds2.Dy() != 1568 {
		t.Errorf("expected 52x1568, got %dx%d", bounds2.Dx(), bounds2.Dy())
	}
}

// TestBuildTextPartKeepsValidUTF8 is the regression test for truncating an
// attached text file on a byte boundary: the data is validated as UTF-8 and
// then a raw body[:limit] cut a multi-byte rune in half, shipping invalid
// UTF-8 to the provider.
func TestBuildTextPartKeepsValidUTF8(t *testing.T) {
	// Well over the 200KB text limit, all multi-byte runes, and sized so the
	// limit lands mid-rune (200*1024 is not a multiple of 3).
	data := []byte(strings.Repeat("配", 120*1024))
	if !utf8.Valid(data) {
		t.Fatal("fixture is not valid UTF-8")
	}

	part, _, _, err := buildTextPart("big.md", "/tmp/big.md", data, "text/markdown")
	if err != nil {
		t.Fatalf("buildTextPart: %v", err)
	}
	if !utf8.ValidString(part.Text) {
		t.Fatal("attachment text is not valid UTF-8 after truncation")
	}
	if !strings.Contains(part.Text, "内容已截断") {
		t.Fatal("truncation notice missing")
	}
}

// TestDecodeImageRejectsDeclaredBomb covers the header check added so a small
// file declaring enormous dimensions is refused instead of driving the
// decoder's up-front width*height allocation.
func TestDecodeImageRejectsDeclaredBomb(t *testing.T) {
	// Build a real PNG, then rewrite IHDR to declare 60000x60000 (3.6e9 pixels)
	// and fix the chunk CRC so the header parses. The pixel data stays tiny —
	// which is exactly the shape of a decompression bomb.
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	raw := buf.Bytes()

	// Layout: 8-byte signature, then IHDR (4 len + 4 type + 13 data + 4 CRC).
	const ihdrType = 8 + 4 // offset of the "IHDR" type field
	const ihdrData = ihdrType + 4
	binary.BigEndian.PutUint32(raw[ihdrData+0:], 60000) // width
	binary.BigEndian.PutUint32(raw[ihdrData+4:], 60000) // height
	crc := crc32.ChecksumIEEE(raw[ihdrType : ihdrData+13])
	binary.BigEndian.PutUint32(raw[ihdrData+13:], crc)

	_, err := decodeImage(raw)
	if err == nil {
		t.Fatal("decodeImage accepted a 60000x60000 declaration")
	}
	if !strings.Contains(err.Error(), "图片过大") {
		t.Fatalf("expected a size refusal, got: %v", err)
	}
}

// TestDecodeImageAcceptsSmallPNG guards against the header check rejecting
// ordinary images.
func TestDecodeImageAcceptsSmallPNG(t *testing.T) {
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	got, err := decodeImage(buf.Bytes())
	if err != nil {
		t.Fatalf("decodeImage rejected a valid 8x8 PNG: %v", err)
	}
	if got.Bounds().Dx() != 8 || got.Bounds().Dy() != 8 {
		t.Fatalf("decoded bounds = %v, want 8x8", got.Bounds())
	}
}
