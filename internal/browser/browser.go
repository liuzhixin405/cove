// Package browser provides HTTP-based web content retrieval with SSRF protection.
//
// It performs HTTP GET requests with browser-like headers and converts HTML to
// readable text or Markdown. This is a pragmatic alternative to headless Chrome
// for the >90% of web pages that don't require JavaScript rendering.
//
// Full chrome-based rendering can be layered on later via chromedp behind
// a build tag (//go:build chromedp).
package browser

import (
	"context"
	"errors"
	"fmt"
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

// ErrChromeUnavailable is returned by headless rendering when the binary was
// not built with the "chromedp" tag.
var ErrChromeUnavailable = errors.New("headless browser not available: rebuild with -tags chromedp (requires Chrome installed)")

// Browser fetches web content via HTTP with safety checks.
type Browser struct {
	timeout        time.Duration
	allowLocalhost bool
	maxBodySize    int64
}

// Config holds browser configuration.
type Config struct {
	AllowLocalhost bool
	Timeout        time.Duration
	MaxBodySize    int64 // 0 = default (5MB)
}

// DefaultConfig returns safe defaults.
func DefaultConfig() Config {
	return Config{
		AllowLocalhost: false,
		Timeout:        30 * time.Second,
		MaxBodySize:    5 * 1024 * 1024,
	}
}

// New creates a Browser with the given configuration.
func New(cfg Config) *Browser {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxBodySize <= 0 {
		cfg.MaxBodySize = 5 * 1024 * 1024
	}
	return &Browser{
		timeout:        cfg.Timeout,
		allowLocalhost: cfg.AllowLocalhost,
		maxBodySize:    cfg.MaxBodySize,
	}
}

// FetchResult is the result of a page fetch.
type FetchResult struct {
	URL        string
	StatusCode int
	Content    string
	Format     string // "text", "markdown", or "html"
}

// FetchHeadless navigates to a URL and returns rendered content as text.
// For static/server-rendered pages this works identically to a browser.
// For SPAs that require JavaScript, use FetchMarkdown with mode=headless.
func (b *Browser) FetchHeadless(ctx context.Context, rawURL string) (*FetchResult, error) {
	return b.fetch(ctx, rawURL, "text")
}

// FetchMarkdown fetches a URL and converts HTML to Markdown.
func (b *Browser) FetchMarkdown(ctx context.Context, rawURL string) (*FetchResult, error) {
	return b.fetch(ctx, rawURL, "markdown")
}

// FetchHTML fetches a URL and returns raw HTML.
func (b *Browser) FetchHTML(ctx context.Context, rawURL string) (*FetchResult, error) {
	return b.fetch(ctx, rawURL, "html")
}

// ChromeAvailable reports whether headless Chrome rendering was compiled in
// (build with -tags chromedp).
func (b *Browser) ChromeAvailable() bool { return chromeAvailable() }

// FetchRendered renders a URL with headless Chrome (executing JavaScript) and
// returns the result in the requested format ("text", "markdown" or "html").
// Falls back with an error if the binary was not built with -tags chromedp.
func (b *Browser) FetchRendered(ctx context.Context, rawURL, format string) (*FetchResult, error) {
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	if err := b.validateURL(rawURL); err != nil {
		return nil, err
	}
	htmlContent, err := renderHeadless(ctx, rawURL, b.timeout)
	if err != nil {
		return nil, err
	}
	content := htmlContent
	switch format {
	case "text":
		content = HTMLToText(htmlContent)
	case "markdown":
		content = HTMLToMarkdown(htmlContent)
	case "html":
		// keep as-is
	default:
		content = HTMLToText(htmlContent)
		format = "text"
	}
	const outputLimit = 100000
	if len(content) > outputLimit {
		content = content[:outputLimit] + "\n... [truncated from " + strconv.Itoa(len(htmlContent)) + " bytes]"
	}
	return &FetchResult{
		URL:        rawURL,
		StatusCode: 200,
		Content:    strings.TrimSpace(content),
		Format:     format,
	}, nil
}

// Screenshot renders a URL with headless Chrome and returns a full-page PNG.
// Requires a build with -tags chromedp.
func (b *Browser) Screenshot(ctx context.Context, rawURL string) ([]byte, error) {
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	if err := b.validateURL(rawURL); err != nil {
		return nil, err
	}
	return captureScreenshot(ctx, rawURL, b.timeout)
}

func (b *Browser) fetch(ctx context.Context, rawURL string, format string) (*FetchResult, error) {
	// Add default scheme if missing
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}

	// Validate URL safety
	if err := b.validateURL(rawURL); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	client := &http.Client{Timeout: b.timeout}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("request creation failed: %w", err)
	}
	req.Header.Set("User-Agent", "cove/1.0 (Mozilla/5.0 compatible)")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, b.maxBodySize))
	if err != nil {
		return nil, fmt.Errorf("read failed: %w", err)
	}

	content := string(body)
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	isHTML := strings.Contains(contentType, "text/html") || strings.Contains(contentType, "application/xhtml+xml")

	if isHTML {
		switch format {
		case "text":
			content = HTMLToText(content)
		case "markdown":
			content = HTMLToMarkdown(content)
		case "html":
			// Keep as-is
		}
	}

	// Truncate if too large
	const outputLimit = 100000
	if len(content) > outputLimit {
		content = content[:outputLimit] + "\n... [truncated from " + strconv.Itoa(len(body)) + " bytes]"
	}

	return &FetchResult{
		URL:        rawURL,
		StatusCode: resp.StatusCode,
		Content:    strings.TrimSpace(content),
		Format:     format,
	}, nil
}

// validateURL checks that the URL is safe to fetch.
func (b *Browser) validateURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme: %s (only http/https allowed)", u.Scheme)
	}

	if b.allowLocalhost {
		return nil
	}

	if isPrivateHost(u.Hostname()) {
		return fmt.Errorf("access to private/internal hosts is blocked: %s", u.Hostname())
	}

	return nil
}

// isPrivateHost checks if a hostname resolves to a private/internal IP.
func isPrivateHost(host string) bool {
	if host == "localhost" || host == "metadata.google.internal" {
		return true
	}

	ip := net.ParseIP(host)
	if ip != nil {
		return isPrivateIP(ip)
	}

	// Try resolving
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, resolved := range ips {
		if isPrivateIP(resolved) {
			return true
		}
	}
	return false
}

func isPrivateIP(ip net.IP) bool {
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

// — HTML conversion (copied from tool/webfetch.go to keep browser self-contained) —

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

// HTMLToText strips HTML tags and returns plain text.
func HTMLToText(s string) string {
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

// HTMLToMarkdown converts HTML to a readable Markdown representation.
func HTMLToMarkdown(s string) string {
	s = reScript.ReplaceAllString(s, "\n")
	s = reStyle.ReplaceAllString(s, "\n")
	s = rePre.ReplaceAllStringFunc(s, func(m string) string {
		inner := rePre.FindStringSubmatch(m)
		if len(inner) > 1 {
			return "\n```\n" + HTMLToText(inner[1]) + "\n```\n"
		}
		return m
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
