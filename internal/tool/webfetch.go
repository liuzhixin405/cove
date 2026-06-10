package tool

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type WebFetchTool struct{ baseTool }

func NewWebFetchTool() Tool {
	return &WebFetchTool{baseTool{def: Def{
		Name: "webfetch", Description: "Fetch content from a URL. Returns the page content. HTTP upgraded to HTTPS.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"url":{"type":"string","description":"The URL to fetch"},
				"format":{"type":"string","description":"Return format: text, markdown, or html"}
			},
			"required":["url"]
		}`),
		IsReadOnly: true, IsConcurrencySafe: true, UserFacingName: "WebFetch",
	}}}
}

func (t *WebFetchTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	url, _ := input["url"].(string)
	if url == "" {
		return Result{Data: "Error: url required", IsError: true}, nil
	}
	format, _ := input["format"].(string)
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		format = "text"
	}
	if format != "text" && format != "markdown" && format != "html" {
		return Result{Data: "Error: format must be one of text, markdown, html", IsError: true}, nil
	}

	if !strings.HasPrefix(url, "http") {
		url = "https://" + url
	}
	if strings.HasPrefix(url, "http://") {
		url = "https://" + strings.TrimPrefix(url, "http://")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}
	req.Header.Set("User-Agent", "cove/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return Result{Data: "Error fetching: " + err.Error(), IsError: true}, nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return Result{Data: "Error reading: " + err.Error(), IsError: true}, nil
	}

	content := string(body)
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	isHTML := strings.Contains(contentType, "text/html") || strings.Contains(contentType, "application/xhtml+xml")

	if isHTML {
		switch format {
		case "text":
			content = htmlToText(content)
		case "markdown":
			content = htmlToMarkdown(content)
		case "html":
			// Keep as-is.
		}
	}

	limit := 100000
	if len(content) > limit {
		content = content[:limit] + "\n... [truncated from " + strconv.Itoa(len(body)) + " bytes]"
	}

	return Result{Data: "URL: " + url + "\nStatus: " + strconv.Itoa(resp.StatusCode) + "\nFormat: " + format + "\n\n" + strings.TrimSpace(content)}, nil
}

var (
	reScript     = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reStyle      = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reTag        = regexp.MustCompile(`(?is)<[^>]+>`)
	reHeading1   = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	reHeading2   = regexp.MustCompile(`(?is)<h2[^>]*>(.*?)</h2>`)
	reHeading3   = regexp.MustCompile(`(?is)<h3[^>]*>(.*?)</h3>`)
	reHeading4   = regexp.MustCompile(`(?is)<h4[^>]*>(.*?)</h4>`)
	reHeading5   = regexp.MustCompile(`(?is)<h5[^>]*>(.*?)</h5>`)
	reHeading6   = regexp.MustCompile(`(?is)<h6[^>]*>(.*?)</h6>`)
	rePre        = regexp.MustCompile(`(?is)<pre[^>]*>(.*?)</pre>`)
	reCode       = regexp.MustCompile(`(?is)<code[^>]*>(.*?)</code>`)
	reAnchor     = regexp.MustCompile(`(?is)<a[^>]*href=["']([^"']+)["'][^>]*>(.*?)</a>`)
	reListItem   = regexp.MustCompile(`(?is)<li[^>]*>(.*?)</li>`)
	reBlockBreak = regexp.MustCompile(`(?is)</?(p|div|section|article|br|hr|ul|ol|table|tr|td|th|blockquote)[^>]*>`)
	reMultiNL    = regexp.MustCompile(`\n{3,}`)
	reMultiSpace = regexp.MustCompile(`[ \t]+`)
)

func htmlToText(s string) string {
	s = reScript.ReplaceAllString(s, " ")
	s = reStyle.ReplaceAllString(s, " ")
	s = reBlockBreak.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)

	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(reMultiSpace.ReplaceAllString(lines[i], " "))
	}
	s = strings.Join(lines, "\n")
	s = reMultiNL.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func htmlToMarkdown(s string) string {
	s = reScript.ReplaceAllString(s, "\n")
	s = reStyle.ReplaceAllString(s, "\n")
	s = rePre.ReplaceAllStringFunc(s, func(m string) string {
		inner := rePre.ReplaceAllString(m, "$1")
		inner = htmlToText(inner)
		return "\n```\n" + inner + "\n```\n"
	})
	s = reHeading1.ReplaceAllString(s, "\n# $1\n")
	s = reHeading2.ReplaceAllString(s, "\n## $1\n")
	s = reHeading3.ReplaceAllString(s, "\n### $1\n")
	s = reHeading4.ReplaceAllString(s, "\n#### $1\n")
	s = reHeading5.ReplaceAllString(s, "\n##### $1\n")
	s = reHeading6.ReplaceAllString(s, "\n###### $1\n")
	s = reAnchor.ReplaceAllString(s, "[$2]($1)")
	s = reCode.ReplaceAllString(s, "`$1`")
	s = reListItem.ReplaceAllString(s, "\n- $1")
	s = reBlockBreak.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)

	lines := strings.Split(s, "\n")
	for i := range lines {
		line := strings.TrimSpace(reMultiSpace.ReplaceAllString(lines[i], " "))
		if strings.HasPrefix(line, "- ") {
			lines[i] = line
		} else {
			lines[i] = strings.TrimSpace(line)
		}
	}
	s = strings.Join(lines, "\n")
	s = reMultiNL.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func (t *WebFetchTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	u, _ := input["url"].(string)
	if isPrivateURL(u) {
		return Denied("access to private/internal URLs is blocked")
	}
	return Allowed("webfetch is read-only")
}

func isPrivateURL(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	if !strings.HasPrefix(rawURL, "http") {
		rawURL = "https://" + rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	host := parsed.Hostname()
	if host == "localhost" || host == "metadata.google.internal" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Try resolving
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			return false
		}
		ip = ips[0]
	}
	privateRanges := []string{
		"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12",
		"192.168.0.0/16", "169.254.0.0/16", "::1/128", "fc00::/7",
	}
	for _, cidr := range privateRanges {
		_, network, _ := net.ParseCIDR(cidr)
		if network.Contains(ip) {
			return true
		}
	}
	return false
}
