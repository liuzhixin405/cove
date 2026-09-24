package api

import (
	"context"
	"testing"
)

// Every English complexity keyword needs its Chinese counterpart: "debug this
// crash" went to the premium model while "帮我调试这个崩溃" went to the fast
// one, so Chinese-speaking users got the weaker model for the same request.
func TestModelRouter_ChineseKeywordsMatchEnglishOnes(t *testing.T) {
	mr := NewModelRouter("premium", "fast")
	pairs := [][2]string{
		{"debug this crash", "帮我调试这个崩溃"},
		{"optimize this loop", "优化这个循环"},
		{"fix the performance of the parser", "解析器性能太差"},
		{"do a security audit of auth", "对登录做安全审计"},
	}
	for _, p := range pairs {
		en := mr.Route(context.Background(), p[0]).Model
		zh := mr.Route(context.Background(), p[1]).Model
		if en != "premium" || zh != en {
			t.Errorf("%q -> %s, %q -> %s; want both premium", p[0], en, p[1], zh)
		}
	}
}
