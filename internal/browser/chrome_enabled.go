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

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/liuzhixin405/cove/internal/log"
)

// chromeAvailable reports whether headless Chrome rendering is compiled in.
func chromeAvailable() bool { return true }

// renderHeadless launches headless Chrome, navigates to rawURL, waits for the
// document body and returns the fully rendered outer HTML. With guard set,
// every request the page makes is checked by shouldBlockRequest.
func renderHeadless(ctx context.Context, rawURL string, timeout time.Duration, guard bool) (string, error) {
	taskCtx, cancel := newChromeTask(ctx, timeout)
	defer cancel()

	var htmlContent string
	err := chromedp.Run(taskCtx, withGuard(taskCtx, guard,
		chromedp.Navigate(rawURL),
		chromedp.WaitReady("body"),
		chromedp.OuterHTML("html", &htmlContent),
	)...)
	if err != nil {
		return "", fmt.Errorf("headless render failed: %w", err)
	}
	return htmlContent, nil
}

// captureScreenshot renders rawURL in headless Chrome and returns a full-page
// PNG screenshot.
func captureScreenshot(ctx context.Context, rawURL string, timeout time.Duration, guard bool) ([]byte, error) {
	taskCtx, cancel := newChromeTask(ctx, timeout)
	defer cancel()

	var buf []byte
	err := chromedp.Run(taskCtx, withGuard(taskCtx, guard,
		chromedp.Navigate(rawURL),
		chromedp.WaitReady("body"),
		chromedp.FullScreenshot(&buf, 90),
	)...)
	if err != nil {
		return nil, fmt.Errorf("headless screenshot failed: %w", err)
	}
	return buf, nil
}

func newChromeTask(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	taskCtx, cancelTask := chromedp.NewContext(allocCtx)
	taskCtx, cancelTimeout := context.WithTimeout(taskCtx, timeout)
	return taskCtx, func() {
		cancelTimeout()
		cancelTask()
		cancelAlloc()
	}
}

// withGuard prepends request interception to actions when guard is set.
//
// Validating the start URL is not enough in a real browser: the page can
// redirect, load subresources from, or fetch() a private address, and Chrome
// resolves and connects on its own. The Fetch domain pauses every request
// (redirect hops included) until it is answered; pausedRequestAction answers
// it.
//
// Known limits: only the page's own target is intercepted — requests from
// out-of-process iframes and workers (separate targets) and WebSocket
// connections are not paused; and Chrome resolves the host again after the
// check, so DNS rebinding between the two is not prevented.
func withGuard(ctx context.Context, guard bool, actions ...chromedp.Action) []chromedp.Action {
	if !guard {
		return actions
	}
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		paused, ok := ev.(*fetch.EventRequestPaused)
		if !ok {
			return
		}
		// A listener must not block the event loop: answer asynchronously.
		go func() {
			c := chromedp.FromContext(ctx)
			if c == nil || c.Target == nil {
				return
			}
			if err := pausedRequestAction(paused).Do(cdp.WithExecutor(ctx, c.Target)); err != nil {
				// Usually the page or the task context went away first.
				url := ""
				if paused.Request != nil {
					url = paused.Request.URL
				}
				log.Debugf("headless: answering paused request %s (%s) failed: %v", paused.RequestID, url, err)
			}
		}()
	})
	return append([]chromedp.Action{fetch.Enable()}, actions...)
}

// pausedRequestAction is the answer to one paused request: continue it when
// its target is public, fail it otherwise (and when it carries no request).
func pausedRequestAction(ev *fetch.EventRequestPaused) chromedp.Action {
	if ev.Request != nil && !shouldBlockRequest(ev.Request.URL) {
		return fetch.ContinueRequest(ev.RequestID)
	}
	return fetch.FailRequest(ev.RequestID, network.ErrorReasonBlockedByClient)
}
