//go:build chromedp

package browser

import (
	"testing"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
)

// Every request the page makes is paused by the Fetch domain and answered by
// pausedRequestAction: a private target is failed, a public one continued.
// (Driving a real Chrome is not needed to check that decision.)
func TestPausedRequestActionFailsPrivateRequests(t *testing.T) {
	paused := func(u string) *fetch.EventRequestPaused {
		return &fetch.EventRequestPaused{RequestID: "r1", Request: &network.Request{URL: u}}
	}
	for _, u := range []string{"http://127.0.0.1:9222/json", "http://169.254.169.254/", "http://[64:ff9b::a00:1]/", "file:///etc/passwd"} {
		a, ok := pausedRequestAction(paused(u)).(*fetch.FailRequestParams)
		if !ok {
			t.Errorf("request to %s was not failed", u)
			continue
		}
		if a.ErrorReason != network.ErrorReasonBlockedByClient || a.RequestID != "r1" {
			t.Errorf("request to %s failed with %+v", u, a)
		}
	}
	if _, ok := pausedRequestAction(paused("https://" + publicIP + "/app.js")).(*fetch.ContinueRequestParams); !ok {
		t.Error("a public request was not continued")
	}
	if _, ok := pausedRequestAction(&fetch.EventRequestPaused{RequestID: "r2"}).(*fetch.FailRequestParams); !ok {
		t.Error("a paused event without a request must fail closed")
	}
}
