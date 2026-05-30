package main

import (
	"encoding/base64"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/agentgo/internal/api"
)

var attachmentTokenRE = regexp.MustCompile(`@("[^"]+"|'[^']+'|\S+)`)

func buildUserMessage(input, cwd string, explicitPaths []string) (api.Message, error) {
	cleaned, inlinePaths := extractInlineAttachments(input)
	allPaths := append([]string{}, explicitPaths...)
	allPaths = append(allPaths, inlinePaths...)

	msg := api.Message{Role: "user", Content: strings.TrimSpace(cleaned)}
	if len(allPaths) == 0 {
		return msg, nil
	}

	seen := map[string]bool{}
	for _, p := range allPaths {
		part, absPath, err := buildAttachmentPart(cwd, p)
		if err != nil {
			return api.Message{}, err
		}
		if seen[absPath] {
			continue
		}
		seen[absPath] = true
		msg.Parts = append(msg.Parts, part)
	}
	return msg, nil
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

func buildAttachmentPart(cwd, rawPath string) (api.MessagePart, string, error) {
	absPath := strings.TrimSpace(rawPath)
	if absPath == "" {
		return api.MessagePart{}, "", fmt.Errorf("附件路径不能为空")
	}
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(cwd, absPath)
	}
	absPath = filepath.Clean(absPath)

	st, err := os.Stat(absPath)
	if err != nil {
		return api.MessagePart{}, "", fmt.Errorf("读取附件失败 %s: %w", rawPath, err)
	}
	if st.IsDir() {
		return api.MessagePart{}, "", fmt.Errorf("附件路径是目录，不是文件: %s", rawPath)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return api.MessagePart{}, "", fmt.Errorf("读取附件失败 %s: %w", rawPath, err)
	}
	mimeType := detectMimeType(absPath, data)
	name := filepath.Base(absPath)

	if strings.HasPrefix(mimeType, "image/") {
		if len(data) > 10*1024*1024 {
			return api.MessagePart{}, "", fmt.Errorf("图片过大(>10MB): %s", rawPath)
		}
		return api.MessagePart{
			Type:     "image",
			MimeType: mimeType,
			Data:     base64.StdEncoding.EncodeToString(data),
			FileName: name,
		}, absPath, nil
	}

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
		}, absPath, nil
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
	}, absPath, nil
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
