package render

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// T4 渲染清洗：Markdown 流渲染器的表格测试（spec §4.5）。
//
// The renderer only ever holds back a possible marker, so its output must
// not depend on where the transport cut the stream. Every corpus document is
// rendered whole and then cut every byte, every rune, every byte with empty
// chunks between and 200 random ways (fixed seed, chunkings in
// table_sanitize_test.go), with both glyph sets; every cut must print
// exactly what the whole did. The runner also checks that the renderer adds
// nothing but SGR to text that has no escapes of its own, and that valid
// UTF-8 stays valid.

type mdCase struct {
	dim, name, in, note string
	// plain are substrings the ANSI-stripped Unicode rendering must contain.
	plain []string
	skip  string
}

func tableMarkdownCorpus() []mdCase {
	long := strings.Repeat("长行文字", 500)
	return []mdCase{
		// Folded in from markdown_test.go / ui_features_test.go.
		{dim: "sample", name: "mdSample", in: mdSample, note: "迁入 TestMarkdownStreamIsIndependentOfChunking（原测试只切两刀）",
			plain: []string{"标题", "• 项"}},
		{dim: "sample", name: "sample with quote", in: "## 标题\n正文 **粗** `code`\n```go\nfmt.Println()\n```\n- 项\n> 引用\n",
			plain: []string{"│ 引用"}},
		{dim: "sample", name: "flush mid heading", in: "## 正在"},
		{dim: "sample", name: "does not wait", in: "我来看看文件，先 **重点** 5 *"},
		{dim: "table", name: "table and code", in: "| 名称 | 值 |\n|---|---|\n| a | 1 |\n| 长一些 | 22 |\n\n```go\nreturn nil\n```\n",
			plain: []string{"│ 名称   │ 值 │", "│ 长一些 │ 22 │"}},

		// Fences.
		{dim: "fence", name: "go fence", in: "```go\nfunc main() { s := \"hi\" // done\n}\n```\nafter\n"},
		{dim: "fence", name: "tilde fence not formatted", in: "~~~\nx := **y** `z`\n# not a heading\n~~~\nafter **b**\n",
			plain: []string{"x := **y** `z`", "# not a heading"}},
		{dim: "fence", name: "unterminated fence", in: "```py\nprint(1)\nno close", note: "流结束时代码块仍未关闭"},
		{dim: "fence", name: "unterminated fence no newline", in: "```", note: "只有开栏"},
		{dim: "fence", name: "longer outer fence", in: "````\n```\ninner\n````\nout\n", plain: []string{"```", "inner"}},
		{dim: "fence", name: "backtick in info string", in: "```a`b\nnot code\n", note: "CommonMark：信息串含反引号不是代码栏"},
		{dim: "fence", name: "indented fence", in: "  ```\n  code\n  ```\n"},
		{dim: "fence", name: "closing fence trailing spaces", in: "```\nx\n```   \ny\n"},
		{dim: "fence", name: "mismatched closing marker", in: "```\nx\n~~~\ny\n```\n", plain: []string{"~~~"}},
		{dim: "fence", name: "two backticks", in: "``\n``x``\n"},
		{dim: "fence", name: "empty code block", in: "```\n```\n"},
		{dim: "fence", name: "fence at EOF", in: "```\ncode\n```"},
		{dim: "fence", name: "short closing run", in: "````\ncode\n```\nstill code\n````\n"},
		{dim: "fence", name: "fence with CRLF", in: "```go\r\nreturn nil\r\n```\r\ntext\r\n"},
		{dim: "fence", name: "blank lines in code", in: "```\na\n\n   \nb\n```\n"},
		{dim: "fence", name: "long info string", in: "```" + strings.Repeat("x", 1000) + "\ncode\n```\n"},

		// Inline code and emphasis spanning chunks.
		{dim: "inline", name: "code containing bold", in: "a `code with **bold**` b\n", plain: []string{"code with **bold**"}},
		{dim: "inline", name: "bold containing code", in: "**bold `code` bold**\n"},
		{dim: "inline", name: "unclosed code", in: "`unclosed code\nnext **x**\n"},
		{dim: "inline", name: "stars", in: "a * b ** c *** d **** e\n"},
		{dim: "inline", name: "triple star", in: "***triple***\n"},
		{dim: "inline", name: "trailing star", in: "trailing star*"},
		{dim: "inline", name: "trailing double star", in: "trailing double**"},
		{dim: "inline", name: "escaped stars", in: "\\*escaped\\* and \\`tick\\`\n"},
		{dim: "inline", name: "long inline code", in: "`" + strings.Repeat("代码", 300) + "`\n"},

		// Headings.
		{dim: "heading", name: "levels", in: "# h1\n## h2\n###### h6\n####### h7\n#hashtag\n#\n##\n",
			plain: []string{"h1", "####### h7", "#hashtag"}},
		{dim: "heading", name: "heading with bold", in: "# 标题 **粗**\n"},
		{dim: "heading", name: "tab heading", in: "#\ttab heading\n"},
		{dim: "heading", name: "indented heading", in: "   ## indented\n"},
		{dim: "heading", name: "heading at EOF", in: "###"},
		{dim: "heading", name: "long heading", in: "# " + long + "\n"},

		// Lists and quotes.
		{dim: "list", name: "nested lists", in: "- a\n  - b\n    - c\n* d\n+ e\n-\n- \n1. x\n  2. y\n", plain: []string{"  • b", "    • c"}},
		{dim: "list", name: "no space after marker", in: "-no space\n*no\n+no\n"},
		{dim: "list", name: "task list", in: "- [ ] task\n- [x] done\n"},
		{dim: "list", name: "rule", in: "----\n***\n___\n"},
		{dim: "list", name: "tab bullet", in: "-\titem\n"},
		{dim: "quote", name: "quotes", in: "> q\n>q2\n>\n> > nested\n"},
		{dim: "quote", name: "quote at EOF", in: ">"},

		// Tables.
		{dim: "table", name: "table then blank", in: "| 名称 | 值 |\n|---|---|\n| a | 1 |\n\n"},
		{dim: "table", name: "table at EOF", in: "| a | b |\n|---|---|\n| 1 | 2 |"},
		{dim: "table", name: "table then text", in: "|a|b|\n|-|-|\n|1|2|\ntext after\n"},
		{dim: "table", name: "ragged table", in: "|a|b|\n|:-|-:|\n|1|2|3|\n"},
		{dim: "table", name: "table with inline markup", in: "| `x` | **y** |\n|---|---|\n| 中 | 文 |\n"},
		{dim: "table", name: "lone row", in: "| lone |\n"},
		{dim: "table", name: "indented table", in: "  | a |\n  |---|\n  | 1 |\n",
			note: "缩进单独落在分块末尾时表格曾被拆成两张（startLine 先结束表格再判断“只有缩进”）"},
		{dim: "table", name: "table unindented rows", in: "| a |\n|---|\n| 1 |\n", note: "对照：无缩进时各切分一致"},
		{dim: "table", name: "table then indented text", in: "|a|\n|-|\n  text\n", note: "表格后接缩进正文：表格本就该结束"},
		{dim: "table", name: "long table cell", in: "| " + strings.Repeat("格", 800) + " |\n|---|\n"},
		{dim: "table", name: "table then fence", in: "|a|\n|-|\n```\ncode\n```\n"},

		// Links.
		{dim: "link", name: "links", in: "see [docs](https://example.com/a_b*c) and <https://x.y/p?q=1&r=2>\n"},
		{dim: "link", name: "image", in: "![img](p.png) ![](x)\n"},

		// Long lines and CJK.
		{dim: "long", name: "long CJK line", in: long + "\n" + long},
		{dim: "long", name: "long words", in: strings.Repeat("word ", 400) + "\n"},
		{dim: "CJK", name: "Chinese paragraph", in: "中文段落，包含**加粗**和`代码`。\n第二行\n"},
		{dim: "CJK", name: "Japanese Korean", in: "日本語のテキスト\n한국어 텍스트\n"},
		{dim: "CJK", name: "emoji", in: "- " + u(man, zwj, woman, zwj, girl) + " 家庭\n# 🏳️‍🌈\n"},
		{dim: "CJK", name: "fullwidth marks", in: "＃ 不是标题\n－ 不是列表\n"},

		// Line endings and blanks.
		{dim: "blank", name: "empty", in: ""},
		{dim: "blank", name: "newlines only", in: "\n\n\n"},
		{dim: "blank", name: "whitespace lines", in: "   \n\t\n  x\n   "},
		{dim: "blank", name: "CRLF document", in: "# h\r\n- a\r\n> q\r\n**b**\r\n"},

		// Everything at once.
		{dim: "mixed", name: "whole document", in: "# 标题\n\n正文 **粗** 与 `代码`。\n\n- 一\n  - 二\n> 引用\n\n| 列 | 值 |\n|---|---|\n| a | 1 |\n\n```go\nfunc f() int { return 1 }\n```\n~~~\nraw **x**\n~~~\n结束"},
	}
}

// markdownSGR matches the styling the renderer (and highlightCode) adds.
var markdownSGR = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func TestTableMarkdownStream(t *testing.T) {
	for idx, c := range tableMarkdownCorpus() {
		t.Run(fmt.Sprintf("%03d %s", idx, c.name), func(t *testing.T) {
			if c.skip != "" {
				t.Skip(c.skip)
			}
			in := c.in
			ctx := fmt.Sprintf("in=%.120q dim=%q", in, c.dim)
			if len(in) > 120 {
				ctx = fmt.Sprintf("in=%.120q...(len %d) dim=%q", in, len(in), c.dim)
			}
			cuts := chunkings(in, 1000+idx, 200)
			for _, gs := range []struct {
				name string
				g    mdGlyphs
			}{{"unicode", mdUnicode}, {"ascii", mdASCII}} {
				whole := runMarkdown(gs.g, []string{in})
				if strings.IndexByte(in, 0x1b) < 0 {
					if rest := markdownSGR.ReplaceAllString(whole, ""); strings.IndexByte(rest, 0x1b) >= 0 {
						t.Errorf("%s outlet=MarkdownStream glyphs=%s chunking=whole: an escape other than SGR\n got %q", ctx, gs.name, whole)
					}
				}
				if utf8.ValidString(in) && !utf8.ValidString(whole) {
					t.Errorf("%s outlet=MarkdownStream glyphs=%s chunking=whole: invalid UTF-8\n got %q", ctx, gs.name, whole)
				}
				if gs.name == "unicode" {
					plain := ansi.Strip(whole)
					for _, p := range c.plain {
						if !strings.Contains(plain, p) {
							t.Errorf("%s outlet=MarkdownStream glyphs=%s chunking=whole: lacks %q\n got %q", ctx, gs.name, p, plain)
						}
					}
				}
				fails := 0
				for _, ch := range cuts[1:] {
					if got := runMarkdown(gs.g, ch.parts); got != whole {
						t.Errorf("%s outlet=MarkdownStream glyphs=%s chunking=%s\n got %q\nwant %q (whole)", ctx, gs.name, ch.name, got, whole)
						if fails++; fails == 3 {
							break
						}
					}
				}
			}
		})
	}
}
