package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/liuzhixin405/cove/internal/api"
)

// Image processing limits (align with upstream API best practices)
const (
	maxImageDim    = 1568             // max pixels on longest side
	maxImageBytes  = 5 * 1024 * 1024  // 5MB target after compression
	maxRawImage    = 32 * 1024 * 1024 // 32MB raw file read limit
	jpegQuality    = 85
	minJPEGQuality = 20
)

var attachmentTokenRE = regexp.MustCompile(`@("[^"]+"|'[^']+'|\S+)`)

func buildUserMessage(input, cwd string, explicitPaths []string, model string) (api.Message, []string, error) {
	cleaned, inlinePaths := extractInlineAttachments(input)
	allPaths := append([]string{}, explicitPaths...)
	allPaths = append(allPaths, inlinePaths...)

	msg := api.Message{Role: "user", Content: strings.TrimSpace(cleaned)}
	if len(allPaths) == 0 {
		return msg, nil, nil
	}

	var warnings []string
	seen := map[string]bool{}
	for _, p := range allPaths {
		part, absPath, warn, err := buildAttachmentPart(cwd, p, model)
		if err != nil {
			return api.Message{}, nil, err
		}
		if warn != "" {
			warnings = append(warnings, warn)
		}
		if seen[absPath] {
			continue
		}
		seen[absPath] = true
		msg.Parts = append(msg.Parts, part)
	}
	return msg, warnings, nil
}

func handleAttachCommand(input, cwd string, attached *[]string) {
	argsText := strings.TrimSpace(strings.TrimPrefix(input, "/attach"))
	if argsText == "" || strings.EqualFold(argsText, "list") {
		printAttachmentList(*attached)
		return
	}

	args, err := splitQuotedFields(argsText)
	if err != nil {
		fmt.Printf("附件命令解析失败: %v\n", err)
		return
	}
	if len(args) == 0 {
		printAttachmentList(*attached)
		return
	}

	switch strings.ToLower(args[0]) {
	case "list", "ls":
		printAttachmentList(*attached)
	case "clear":
		*attached = nil
		fmt.Println("已清空附件列表")
	case "remove", "rm":
		removeAttachment(args[1:], attached)
	case "add":
		addAttachments(args[1:], cwd, attached)
	default:
		addAttachments(args, cwd, attached)
	}
}

func addAttachments(paths []string, cwd string, attached *[]string) {
	if len(paths) == 0 {
		fmt.Println("用法: /attach <文件...> | /attach list | /attach remove <序号> | /attach clear")
		return
	}
	seen := map[string]bool{}
	for _, existing := range *attached {
		seen[existing] = true
	}
	added := 0
	for _, rawPath := range paths {
		absPath, err := normalizeAttachmentPath(cwd, rawPath)
		if err != nil {
			fmt.Printf("跳过 %s: %v\n", rawPath, err)
			continue
		}
		if seen[absPath] {
			continue
		}
		*attached = append(*attached, absPath)
		seen[absPath] = true
		added++
	}
	fmt.Printf("已挂载 %d 个附件，当前共 %d 个。\n", added, len(*attached))
	printAttachmentList(*attached)
}

func removeAttachment(args []string, attached *[]string) {
	if len(args) == 0 {
		fmt.Println("用法: /attach remove <序号>")
		return
	}
	idx, err := strconv.Atoi(args[0])
	if err != nil || idx < 1 || idx > len(*attached) {
		fmt.Printf("无效附件序号: %s\n", args[0])
		return
	}
	removed := (*attached)[idx-1]
	*attached = append((*attached)[:idx-1], (*attached)[idx:]...)
	fmt.Printf("已移除附件: %s\n", removed)
	printAttachmentList(*attached)
}

func printAttachmentList(paths []string) {
	if len(paths) == 0 {
		fmt.Println("当前没有挂载附件。用 /attach <文件...> 添加。")
		return
	}
	fmt.Println("当前挂载附件:")
	for i, p := range paths {
		fmt.Printf("  %d. %s\n", i+1, p)
	}
}

func normalizeAttachmentPath(cwd, rawPath string) (string, error) {
	absPath := strings.TrimSpace(rawPath)
	absPath = strings.TrimPrefix(absPath, "@")
	absPath = strings.Trim(absPath, `"'`)
	if absPath == "" {
		return "", fmt.Errorf("路径不能为空")
	}
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(cwd, absPath)
	}
	absPath = filepath.Clean(absPath)
	st, err := os.Stat(absPath)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("路径是目录，不是文件")
	}
	return absPath, nil
}

func splitQuotedFields(input string) ([]string, error) {
	var fields []string
	var b strings.Builder
	var quote rune
	inToken := false
	for _, r := range input {
		if quote != 0 {
			if r == quote {
				quote = 0
				continue
			}
			b.WriteRune(r)
			inToken = true
			continue
		}

		switch r {
		case '\'', '"':
			quote = r
			inToken = true
		case ' ', '\t', '\r', '\n':
			if inToken {
				fields = append(fields, b.String())
				b.Reset()
				inToken = false
			}
		default:
			b.WriteRune(r)
			inToken = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("引号未闭合")
	}
	if inToken {
		fields = append(fields, b.String())
	}
	return fields, nil
}

func extractInlineAttachments(input string) (string, []string) {
	matches := attachmentTokenRE.FindAllStringSubmatchIndex(input, -1)
	if len(matches) == 0 {
		return input, nil
	}
	paths := make([]string, 0, len(matches))
	var out strings.Builder
	last := 0
	for _, m := range matches {
		start := m[0]
		end := m[1]
		pStart := m[2]
		pEnd := m[3]
		out.WriteString(input[last:start])
		raw := strings.TrimSpace(input[pStart:pEnd])
		raw = strings.Trim(raw, `"'`)
		if raw != "" {
			paths = append(paths, raw)
		}
		last = end
	}
	out.WriteString(input[last:])
	cleaned := strings.Join(strings.Fields(out.String()), " ")
	return cleaned, paths
}

// buildAttachmentPart reads a file and creates an api.MessagePart.
// Returns (part, absPath, warning, error).
// warning is non-empty for non-fatal issues (e.g., non-vision model with image).
func buildAttachmentPart(cwd, rawPath, model string) (api.MessagePart, string, string, error) {
	absPath := strings.TrimSpace(rawPath)
	if absPath == "" {
		return api.MessagePart{}, "", "", fmt.Errorf("附件路径不能为空")
	}
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(cwd, absPath)
	}
	absPath = filepath.Clean(absPath)

	st, err := os.Stat(absPath)
	if err != nil {
		return api.MessagePart{}, "", "", fmt.Errorf("读取附件失败 %s: %w", rawPath, err)
	}
	if st.IsDir() {
		return api.MessagePart{}, "", "", fmt.Errorf("附件路径是目录，不是文件: %s", rawPath)
	}

	// Read file (cap at 32MB for image processing headroom)
	readLimit := int64(maxRawImage)
	if st.Size() > readLimit {
		return api.MessagePart{}, "", "", fmt.Errorf(
			"文件过大 (%.1fMB > %.0fMB 限制): %s\n  提示: 请先用图片工具缩小尺寸后再挂载",
			float64(st.Size())/(1024*1024), float64(readLimit)/(1024*1024), rawPath)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return api.MessagePart{}, "", "", fmt.Errorf("读取附件失败 %s: %w", rawPath, err)
	}

	name := filepath.Base(absPath)
	mimeType := detectMimeType(absPath, data)

	if strings.HasPrefix(mimeType, "image/") {
		return buildImagePart(name, absPath, data, mimeType, model)
	}

	return buildTextPart(name, absPath, data, mimeType)
}

// buildImagePart processes an image file and creates a vision API part.
func buildImagePart(name, absPath string, raw []byte, mimeType, model string) (api.MessagePart, string, string, error) {
	// Check if model supports vision
	warning := ""
	if model != "" && !api.IsVisionCapableModel(model) {
		warning = fmt.Sprintf("⚠ 当前模型 %s 可能不支持图片视觉功能，已自动降级为文本提示。建议切换到视觉模型 (如 deepseek-chat / gpt-4o / claude-sonnet-4)", model)
		return api.MessagePart{
			Type:     "text",
			MimeType: "text/plain",
			FileName: name,
			Text:     fmt.Sprintf("[已挂载图片 %s，但当前模型 %s 可能不支持视觉输入，图片内容未发送。请切换视觉模型后重试。]", name, model),
		}, absPath, warning, nil
	}

	// Process image: resize + compress
	processed, finalMime, err := processImage(raw)
	if err != nil {
		// If processing fails, fall back to raw data (but still enforce size limit)
		if len(raw) > maxImageBytes {
			return api.MessagePart{}, "", "", fmt.Errorf(
				"图片过大且无法压缩 (%.1fMB > 5MB): %s\n  提示: 请手动缩小图片后再挂载，或用截图工具截取更小的区域",
				float64(len(raw))/(1024*1024), name)
		}
		processed = raw
		finalMime = mimeType
	}

	// Final size check
	if len(processed) > maxImageBytes*2 {
		return api.MessagePart{}, "", "", fmt.Errorf(
			"图片压缩后仍然过大 (%.1fMB): %s\n  提示: 请减小图片尺寸后再挂载",
			float64(len(processed))/(1024*1024), name)
	}

	return api.MessagePart{
		Type:     "image",
		MimeType: finalMime,
		Data:     base64.StdEncoding.EncodeToString(processed),
		FileName: name,
	}, absPath, warning, nil
}

// buildTextPart creates a text/file attachment part.
func buildTextPart(name, absPath string, data []byte, mimeType string) (api.MessagePart, string, string, error) {
	const textLimit = 200 * 1024
	const binLimit = 96 * 1024
	if utf8.Valid(data) {
		body := string(data)
		truncated := ""
		if len(body) > textLimit {
			body = body[:textLimit]
			truncated = "\n[内容已截断: 仅发送前 200KB]"
		}
		return api.MessagePart{
			Type:     "text",
			MimeType: mimeType,
			FileName: name,
			Text:     fmt.Sprintf("附件 %s (%s) 内容:\n```text\n%s\n```%s", name, mimeType, body, truncated),
		}, absPath, "", nil
	}

	payload := data
	truncated := ""
	if len(payload) > binLimit {
		payload = payload[:binLimit]
		truncated = " (已截断)"
	}
	encoded := base64.StdEncoding.EncodeToString(payload)
	return api.MessagePart{
		Type:     "text",
		MimeType: mimeType,
		FileName: name,
		Text:     fmt.Sprintf("附件 %s (%s) 为二进制文件，以下为 base64 片段%s:\n%s", name, mimeType, truncated, encoded),
	}, absPath, "", nil
}

// ---------------------------------------------------------------------------
// Image processing
// ---------------------------------------------------------------------------

// processImage decodes, resizes, and re-encodes an image for API submission.
// Returns (processed bytes, mime type, error).
// Falls back to original data if decoding fails.
func processImage(raw []byte) ([]byte, string, error) {
	img, err := decodeImage(raw)
	if err != nil {
		return nil, "", fmt.Errorf("decode image: %w", err)
	}

	// Resize if needed
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	longest := w
	if h > w {
		longest = h
	}
	if longest > maxImageDim {
		img = resizeImage(img, w, h, maxImageDim)
	}

	// Encode as JPEG with quality compression
	encoded, err := compressToSize(img, maxImageBytes, jpegQuality)
	if err != nil {
		return nil, "", fmt.Errorf("encode image: %w", err)
	}

	return encoded, "image/jpeg", nil
}

// decodeImage decodes PNG, JPEG, or GIF from raw bytes.
func decodeImage(raw []byte) (image.Image, error) {
	// Try each format
	if img, err := png.Decode(bytes.NewReader(raw)); err == nil {
		return img, nil
	}
	if img, err := jpeg.Decode(bytes.NewReader(raw)); err == nil {
		return img, nil
	}
	if img, err := gif.Decode(bytes.NewReader(raw)); err == nil {
		return img, nil
	}
	return nil, fmt.Errorf("unsupported image format (支持: PNG, JPEG, GIF)")
}

// resizeImage scales an image down so its longest side <= maxDim,
// preserving aspect ratio. Uses bilinear interpolation.
func resizeImage(img image.Image, srcW, srcH, maxDim int) image.Image {
	// Calculate new dimensions
	newW, newH := srcW, srcH
	if srcW >= srcH && srcW > maxDim {
		newW = maxDim
		newH = int(float64(srcH) * float64(maxDim) / float64(srcW))
	} else if srcH > maxDim {
		newH = maxDim
		newW = int(float64(srcW) * float64(maxDim) / float64(srcH))
	}
	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}

	return bilinearResize(img, srcW, srcH, newW, newH)
}

// bilinearResize performs bilinear interpolation downscaling.
func bilinearResize(src image.Image, sw, sh, dw, dh int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	xs := float64(sw) / float64(dw)
	ys := float64(sh) / float64(dh)

	for dy := 0; dy < dh; dy++ {
		sy := float64(dy)*ys + 0.5
		syi := int(sy)
		syf := sy - float64(syi)
		if syi >= sh-1 {
			syi = sh - 2
			syf = 1.0
		}
		if syi < 0 {
			syi = 0
			syf = 0
		}

		for dx := 0; dx < dw; dx++ {
			sx := float64(dx)*xs + 0.5
			sxi := int(sx)
			sxf := sx - float64(sxi)
			if sxi >= sw-1 {
				sxi = sw - 2
				sxf = 1.0
			}
			if sxi < 0 {
				sxi = 0
				sxf = 0
			}

			// Sample 4 neighbors
			c00r, c00g, c00b, c00a := src.At(sxi, syi).RGBA()
			c10r, c10g, c10b, c10a := src.At(sxi+1, syi).RGBA()
			c01r, c01g, c01b, c01a := src.At(sxi, syi+1).RGBA()
			c11r, c11g, c11b, c11a := src.At(sxi+1, syi+1).RGBA()

			// Bilinear interpolation
			ixf := 65535.0 - float64(sxf*65535)
			iyf := 65535.0 - float64(syf*65535)
			sxf32 := float64(sxf * 65535)
			syf32 := float64(syf * 65535)

			r := uint8((float64(c00r)*ixf*iyf + float64(c10r)*sxf32*iyf + float64(c01r)*ixf*syf32 + float64(c11r)*sxf32*syf32) / (65535.0 * 65535.0) / 257.0)
			g := uint8((float64(c00g)*ixf*iyf + float64(c10g)*sxf32*iyf + float64(c01g)*ixf*syf32 + float64(c11g)*sxf32*syf32) / (65535.0 * 65535.0) / 257.0)
			b := uint8((float64(c00b)*ixf*iyf + float64(c10b)*sxf32*iyf + float64(c01b)*ixf*syf32 + float64(c11b)*sxf32*syf32) / (65535.0 * 65535.0) / 257.0)
			_ = c00a + c10a + c01a + c11a // suppress unused warning

			dst.SetRGBA(dx, dy, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}

	return dst
}

// compressToSize encodes img as JPEG, iteratively reducing quality until
// the output is under maxBytes (or quality reaches minJPEGQuality).
func compressToSize(img image.Image, maxBytes int, startQuality int) ([]byte, error) {
	quality := startQuality

	// Strip alpha channel for JPEG encoding
	rgb := stripAlpha(img)

	for quality >= minJPEGQuality {
		var buf bytes.Buffer
		err := jpeg.Encode(&buf, rgb, &jpeg.Options{Quality: quality})
		if err != nil {
			return nil, err
		}
		if buf.Len() <= maxBytes {
			return buf.Bytes(), nil
		}
		quality -= 15
	}

	// Return smallest version even if over limit
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, rgb, &jpeg.Options{Quality: minJPEGQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// stripAlpha creates an opaque RGBA image (JPEG doesn't support alpha).
func stripAlpha(img image.Image) *image.RGBA {
	bounds := img.Bounds()
	dst := image.NewRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(r >> 8),
				G: uint8(g >> 8),
				B: uint8(b >> 8),
				A: 255,
			})
		}
	}
	return dst
}

func detectMimeType(path string, data []byte) string {
	m := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if m != "" {
		if idx := strings.Index(m, ";"); idx > 0 {
			return m[:idx]
		}
		return m
	}
	if len(data) > 0 {
		d := data
		if len(d) > 512 {
			d = d[:512]
		}
		return http.DetectContentType(d)
	}
	return "application/octet-stream"
}
