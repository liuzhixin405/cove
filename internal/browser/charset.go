package browser

import (
	"bytes"
	"fmt"
	"mime"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"
)

// reMetaCharset finds a charset declared in an HTML <meta> tag, either
// <meta charset="gbk"> or <meta http-equiv="Content-Type" content="...; charset=gb2312">.
var reMetaCharset = regexp.MustCompile(`(?i)<meta[^>]+charset\s*=\s*["']?\s*([a-z0-9_\-:.]+)`)

// decodeBody turns a response body into UTF-8 text.
//
// The body used to be taken as UTF-8 whatever the server declared, so GBK
// pages — still common on Chinese sites — came back as mojibake, and a PDF or
// image came back as up to 100KB of raw bytes in the model's context.
func decodeBody(body []byte, contentType string, isHTML bool) (string, error) {
	if cs := declaredCharset(body, contentType, isHTML); cs != "" && !isUTF8Name(cs) {
		if enc, err := htmlindex.Get(cs); err == nil {
			if decoded, err := enc.NewDecoder().Bytes(body); err == nil {
				return string(decoded), nil
			}
		}
	}
	if looksBinary(body, contentType) {
		if contentType == "" {
			contentType = "unknown type"
		}
		return "", fmt.Errorf("binary content (%s, %d bytes); the browser tool only returns text", contentType, len(body))
	}
	return string(body), nil
}

func declaredCharset(body []byte, contentType string, isHTML bool) string {
	if _, params, err := mime.ParseMediaType(contentType); err == nil && params["charset"] != "" {
		return strings.ToLower(strings.TrimSpace(params["charset"]))
	}
	if !isHTML {
		return ""
	}
	head := body
	if len(head) > 2048 {
		head = head[:2048]
	}
	if m := reMetaCharset.FindSubmatch(head); m != nil {
		return strings.ToLower(string(m[1]))
	}
	return ""
}

func isUTF8Name(cs string) bool {
	return cs == "utf-8" || cs == "utf8"
}

// looksBinary reports whether body is not text: a NUL byte in the first 8KB,
// or bytes that are not UTF-8 under a content type that does not claim to be
// text (an undeclared legacy-encoded page is still text).
func looksBinary(body []byte, contentType string) bool {
	sample := body
	if len(sample) > 8192 {
		sample = sample[:8192]
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return true
	}
	if utf8.Valid([]byte(trimPartialTrailingRune(string(sample)))) {
		return false
	}
	ct := strings.ToLower(contentType)
	for _, textual := range []string{"text/", "json", "xml", "javascript", "html"} {
		if strings.Contains(ct, textual) {
			return false
		}
	}
	return true
}
