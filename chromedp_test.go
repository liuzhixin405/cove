//go:build chromedp

package main

import (
	"context"
	"fmt"
	"time"
	"github.com/liuzhixin405/cove/internal/browser"
)

func main() {
	br := browser.New(browser.Config{
		AllowLocalhost: false,
		Timeout:        15 * time.Second,
		MaxBodySize:    5 * 1024 * 1024,
	})
	fmt.Println("Chrome available:", br.ChromeAvailable())

	ctx := context.Background()
	res, err := br.FetchRendered(ctx, "https://httpbin.org/headers", "text")
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	fmt.Printf("✓ Rendered page: status=%d, content=%d bytes\n", res.StatusCode, len(res.Content))
	fmt.Println(res.Content[:500])
}
