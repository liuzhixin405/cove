//go:build chromedp

// Package browser headless-Chrome backend, compiled only with the "chromedp"
// build tag:
//
//	go build -tags chromedp ./...
//
// It requires a Chrome/Chromium binary to be installed on the host. Without the
// tag, chrome_disabled.go provides stubs and the HTTP fetch path is used.
package browser

import (
	"context"
	"fmt"
	"time"

	"github.com/chromedp/chromedp"
)

// chromeAvailable reports whether headless Chrome rendering is compiled in.
func chromeAvailable() bool { return true }

// renderHeadless launches headless Chrome, navigates to rawURL, waits for the
// document body and returns the fully rendered outer HTML.
func renderHeadless(ctx context.Context, rawURL string, timeout time.Duration) (string, error) {
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	defer cancelAlloc()

	taskCtx, cancelTask := chromedp.NewContext(allocCtx)
	defer cancelTask()

	taskCtx, cancelTimeout := context.WithTimeout(taskCtx, timeout)
	defer cancelTimeout()

	var htmlContent string
	err := chromedp.Run(taskCtx,
		chromedp.Navigate(rawURL),
		chromedp.WaitReady("body"),
		chromedp.OuterHTML("html", &htmlContent),
	)
	if err != nil {
		return "", fmt.Errorf("headless render failed: %w", err)
	}
	return htmlContent, nil
}

// captureScreenshot renders rawURL in headless Chrome and returns a full-page
// PNG screenshot.
func captureScreenshot(ctx context.Context, rawURL string, timeout time.Duration) ([]byte, error) {
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	defer cancelAlloc()

	taskCtx, cancelTask := chromedp.NewContext(allocCtx)
	defer cancelTask()

	taskCtx, cancelTimeout := context.WithTimeout(taskCtx, timeout)
	defer cancelTimeout()

	var buf []byte
	err := chromedp.Run(taskCtx,
		chromedp.Navigate(rawURL),
		chromedp.WaitReady("body"),
		chromedp.FullScreenshot(&buf, 90),
	)
	if err != nil {
		return nil, fmt.Errorf("headless screenshot failed: %w", err)
	}
	return buf, nil
}
