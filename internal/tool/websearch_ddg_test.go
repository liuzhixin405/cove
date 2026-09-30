package tool

import "testing"

// DuckDuckGo's HTML endpoint wraps every result in a redirect,
// //duckduckgo.com/l/?uddg=<target>, which is not http(s) and so every
// result was dropped.
func TestExtractWebSearchResultsDecodesDuckDuckGoRedirects(t *testing.T) {
	body := `
<div class="result">
  <h2><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2Feffective_go%3Fx%3D1%26y%3D2&amp;rut=abc123">Effective Go</a></h2>
  <a class="result__snippet" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2Feffective_go%3Fx%3D1%26y%3D2&amp;rut=abc123">Tips for writing clear, idiomatic Go code.</a>
</div>
<div class="result">
  <h2><a class="result__a" href="https://pkg.go.dev/fmt">Package fmt</a></h2>
</div>
<div class="result result--ad">
  <h2><a class="result__a" href="//duckduckgo.com/y.js?ad_domain=ads.example&amp;u3=x">Buy now</a></h2>
</div>
<div class="result">
  <h2><a class="result__a" href="/l/?uddg=https%3A%2F%2Fexample.org%2F%E4%B8%AD%E6%96%87">Relative redirect</a></h2>
</div>
<div class="result">
  <h2><a class="result__a" href="//duckduckgo.com/l/?uddg=javascript%3Aalert(1)">Not a web link</a></h2>
</div>
<a href="/html/?q=next+page">Next</a>
`
	got := extractWebSearchResults(body, 10)
	want := []struct{ title, url string }{
		{"Effective Go", "https://go.dev/doc/effective_go?x=1&y=2"},
		{"Package fmt", "https://pkg.go.dev/fmt"},
		{"Relative redirect", "https://example.org/中文"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d results %+v, want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i].title != w.title || got[i].url != w.url {
			t.Errorf("result %d = %q %q, want %q %q", i, got[i].title, got[i].url, w.title, w.url)
		}
	}
	if got[0].snippet == "" {
		t.Error("snippet lost for the redirected result")
	}
}
