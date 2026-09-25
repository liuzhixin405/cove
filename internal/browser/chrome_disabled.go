//go:build !chromedp

package browser

import (
	"context"
	"time"
)

// chromeAvailable reports whether headless Chrome rendering is compiled in.
func chromeAvailable() bool { return false }

func renderHeadless(ctx context.Context, rawURL string, timeout time.Duration, guard bool) (string, error) {
	return "", ErrChromeUnavailable
}

func captureScreenshot(ctx context.Context, rawURL string, timeout time.Duration, guard bool) ([]byte, error) {
	return nil, ErrChromeUnavailable
}
