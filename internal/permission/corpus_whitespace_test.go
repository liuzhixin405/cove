package permission

// Unusual whitespace and invisible characters (dimension 不寻常空白):
// lone CR, FF, VT, NBSP and other Unicode spaces, U+2028/U+2029/NEL, zero
// width and bidi format characters, controls, invalid UTF-8. A line holding
// one is never read-only, never auto-approved, never covered or remembered;
// the hard block reads through them. Ordinary blanks (double spaces, tabs,
// CRLF line ends, leading/trailing blanks) change nothing.
// Sources: internal/safety review_fix4_test.go (TestHasHostileCharacters,
// TestCatastrophicCommandSeesThroughUnusualWhitespace), review_fix4_test.go
// in this package, manual 无法确定含义时一律询问.

// hostile is a line with a hostile character that is not catastrophic: it
// asks everywhere and nothing covers it (allow-go-test and param-match ask).
func hostile(cmd, note string) permCase {
	return permCase{cmd: cmd, want: wantAsk(u, rememberNone), note: note}
}

var corpusWhitespace = join([]permCase{
	// hostile characters in otherwise read-only or build lines
	hostile("ls\rRemove-Item -Recurse -Force src", "手册：PowerShell 把单独 CR 当换行"),
	hostile("ls\rrm -rf x", "R3：单独 CR 不是空白"),
	hostile("git status\r", "行尾单独 CR 也算（trim 之前检查）"),
	hostile("echo 'a\rb'", "引号内的单独 CR"),
	hostile("ls\f-la", "FF 在 PowerShell 是参数分隔"),
	hostile("ls\v-la", "VT 在 PowerShell 是参数分隔"),
	hostile("ls\u00a0-la", "NBSP"),
	hostile("ls\u3000-la", "全角空格"),
	hostile("ls\u2003-la", "EM SPACE"),
	hostile("ls\u2028pwd", "U+2028 行分隔符"),
	hostile("ls\u2029pwd", "U+2029 段分隔符"),
	hostile("ls\u0085pwd", "NEL"),
	hostile("git\u200b status", "零宽空格"),
	hostile("git st\u200batus", "词内零宽空格"),
	hostile("git status\u200d", "行尾零宽连接符"),
	hostile("ls \u202e", "RLO 双向控制"),
	hostile("ls\x01", "C0 控制字符"),
	hostile("ls\x7f", "DEL 控制字符"),
	hostile("ls \xff", "非法 UTF-8"),
	hostile("go test ./...\rRemove-Item -Recurse -Force src", "R4：前缀规则不覆盖含单独 CR 的行"),
	hostile("go test\u00a0./...", "NBSP 的 go test 不被前缀覆盖"),
	hostile("git status\rrm -rf src", "param_match git status* 不放行含 CR 的行"),
	hostile("git status\u2028rm -rf src", "param_match 不放行含 U+2028 的行"),
	{cmd: "git\u200b push", want: wantAsk(u, rememberNone).with(scDenyPush, DDeny),
		note: "deny 按去掉零宽字符后的形式匹配"},
	{cmd: "git push\u00a0origin main", want: wantAsk(u, rememberNone).with(scDenyPush, DDeny),
		note: "NBSP 分隔的 git push 仍被 deny"},

	// hostile characters in catastrophic lines: still hard-blocked
	boom("echo hi\rRemove-Item -Recurse -Force C:\\Windows", "手册：echo hi<CR>Remove-Item C:\\Windows"),
	boom("ls\rrm -r -fo C:\\Users", "CR 后的 PowerShell 删除"),
	boom("ls\u0085rm -rf /", "NEL 后的删根"),
	boom("ls\u2028rm -rf ~", "U+2028 后的删家目录"),
	boom("ls\u2029Remove-Item -Recurse -Force C:\\", "U+2029 后的删盘符根"),
	boom("rm\f-rf\f/", "FF 分隔的删根"),
	boom("rm\v-rf\v/", "VT 分隔的删根"),
	boom("rm\u00a0-rf\u00a0/", "NBSP 分隔的删根"),
	boom("rm\u2003-rf\u3000/", "Unicode 空格分隔的删根"),
	boom("rm\u202f-rf\u205f/", "窄 NBSP / 数学空格"),
	boom("r\u200bm -rf /", "手册：r<零宽空格>m -rf /"),
	boom("rm -rf /\u200d", "路径后零宽连接符"),
	boom("rm \u202e-rf /", "双向控制字符"),

	// ordinary blanks: same result as without them
	{cmd: "git status\r\n", kinds: kPosixPS, want: wantReadOnly(), note: "CRLF 行尾是普通行尾"},
	{cmd: "git status\r\ngit log -1\r\n", kinds: kPosixPS, want: wantReadOnly(), note: "CRLF 分隔的两条只读命令"},
	{cmd: "ls -la\n", kinds: kPosixPS, want: wantReadOnly(), note: "行尾换行"},
	{cmd: "grep -rn 'x\ty' .", kinds: kPosixPS, want: wantReadOnly(), note: "引号内的 Tab 不是敌意字符"},
	{cmd: "go test ./...\r\n", kinds: kPosixPS, want: wantBuild().with(scAllowGo, DAllow).rem(rememberGroup),
		note: "CRLF 行尾的 go test 仍被前缀覆盖"},
	{cmd: "git push\r\n", want: wantAsk(g, rememberGroup).with(scDenyPush, DDeny), note: "CRLF 行尾的 git push 仍被 deny"},
	{cmd: "rm -rf /\r\n", want: wantCatastrophic(), note: "CRLF 行尾的灾难命令仍拦截"},
},
	ro("git  status", "双空格不改判", cmdDecides(scParamMatch, DAllow)),
	ro("git\tstatus", "Tab 分隔不改判", cmdDecides(scParamMatch, DAllow)),
	ro("  git status", "行首空白不改判", cmdDecides(scParamMatch, DAllow)),
	ro("git status  ", "行尾空白不改判", cmdDecides(scParamMatch, DAllow)),
	ro("\tls -la", "行首 Tab"),
	ro("git status &&  git diff", "操作符旁的多余空白"),
)
