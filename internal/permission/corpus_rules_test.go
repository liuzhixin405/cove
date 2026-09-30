package permission

// Rule scenarios (Manager.Check): what the four rule sets let through.
// allow-prefix-go-test: an allow prefix compares words as written, every
// command of the line must be covered (read-only companions count, except
// under cmd.exe), no substitution or file redirect. deny-git-push: deny is
// normalized and sees nested commands; an argument that merely mentions git
// push is not a push. ask-git-group: an ask on the git group beats auto
// mode's pre-approval for git writes but leaves read-only git alone.
// param-match-git-status: "git status*" is a word prefix applied per simple
// command. Sources: command_prefix_test.go, policy_rules_test.go,
// ask_precedence_test.go, review_fix*_test.go, rule_input_test.go, manual
// 授权提示 and 规则判定顺序与匹配.

// goAllowed is a go test line allow-prefix-go-test covers under kinds.
func goAllowed(kinds []ShellKind, cmd, note string) permCase {
	w := wantBuild().with(scAllowGo, DAllow).rem(rememberGroup)
	if len(kinds) == 1 && kinds[0] == ShellCmd {
		w = wantCmdBuild().with(scAllowGo, DAllow).rem(rememberGroup)
	}
	return in(kinds, cmd, w, note)
}

var corpusRules = []permCase{
	// allow-prefix-go-test: covered
	goAllowed(kCmd, "go test -v ./...", "cmd 回退下已记住的前缀规则仍免询问（手册）"),
	goAllowed(kPosixPS, "go test ./... 2>/dev/null", "丢弃重定向不影响前缀覆盖"),
	goAllowed(kPosixPS, "go test ./... && git status", "只读伴随命令视为已覆盖"),
	goAllowed(kPosixPS, "go test ./...; ls", "分号后只读命令视为已覆盖"),
	goAllowed(kPosixPS, "cd src && go test ./...", "cd 视为已覆盖"),
	goAllowed(kPOSIX, "go test \\\n  ./...", "bash 续行后的 go test 仍被覆盖"),
	goAllowed(kPosixPS, "go test -run 'TestA|TestB' ./...", "引号内 | 可信"),

	// allow-prefix-go-test: not covered
	in(kPosixPS, "go test ./... && rm -rf x", wantAsk(u, rememberMixed).with(scAskGit, DAsk), "手册：go test && rm -rf x 仍询问"),
	in(kPosixPS, "go vet ./internal/... && go test ./...", wantBuild().rem(rememberGroup).with(scAllowGo, DAsk), "go vet 不在 go test 前缀下"),
	in(kPosixPS, "go test ./... > out.txt", wantAsk(u, rememberNone), "输出重定向到文件不被覆盖"),
	in(kPosixPS, `go test -run "$(rm x)" ./...`, wantAsk(u, rememberNone), "引号内的 $( 仍展开，不被覆盖"),
	in(kPosixPS, "go test ./... && env rm -rf x", wantAsk(u, rememberNone), "伴随的 env 包裹命令不被覆盖"),
	in(kPosixPS, "/usr/local/go/bin/go test ./...", wantAsk(u, rememberPrefix), "手册：allow 不归一化全路径"),
	in(kPosixPS, "GO test ./...", wantBuild().with(scAllowGo, DAsk).rem(rememberGroup), "allow 逐词比较，大小写不同不覆盖"),
	in(kPOSIX, "go test ./... && find . -f\\\nls out.txt", wantAsk(u, rememberMixed), "bash 续行出 find -fls，不被覆盖"),
	in(kCmd, "go test ./... && git status", wantCmdBuild().with(scAllowGo, DAsk).rem(rememberMixed), "手册：cmd 回退时只读伴随也须被覆盖"),

	// deny-git-push: an argument mentioning git push is not a push
	in(kPosixPS, "echo git push", wantReadOnly(), "git push 只是 echo 的参数"),
	in(kPosixPS, "git log --grep push", wantReadOnly(), "push 只是 --grep 的值"),
	ask(`git commit -m "git push"`, g, rememberGroup, "引号内的 git push 是提交消息"),
	ask("git pushx", g, rememberPrefix, "deny 前缀按整词匹配：git pushx 不是 git push"),
	viaPush("GIT_DIR=x git push", u, rememberNone, "deny 剥掉 VAR= 前缀"),
	viaPush("git -C sub push origin main", g, rememberGroup, "deny 跳过 -C 全局选项"),
	in(kPosixPS, "git status; git push origin main", wantAsk(g, rememberGroup).with(scDenyPush, DDeny), "链式里的第二条 push"),
	viaPush("git push --force origin main", g, rememberPrefix, "deny 前缀也拦强推"),

	// ask-git-group (auto mode): git writes ask even when auto would approve
	// the rest of the line; read-only git and builds do not
	in(kPosixPS, "go test ./... && git add .", wantAsk(g, rememberGroup).with(scAllowGo, DAsk), "auto 下 go test 之后的 git add 被 ask 组命中"),
	in(kPosixPS, "git status && go test ./...", wantBuild().rem(rememberGroup).with(scAllowGo, DAllow), "ask 组不管只读 git 与构建"),
	in(kPosixPS, "git show --stat HEAD && go vet ./...", wantBuild().rem(rememberGroup).with(scAllowGo, DAsk), "只读 git show 不被 ask 组命中"),
	ask("git reset --hard origin/main", g, rememberPrefix, "手册：ask 组同样拦下组外的高风险 git 写法"),
	ask("git -c core.editor=vim commit", u, rememberNone, "手册：git -c … 被 ask 组命中"),

	// param-match-git-status (seen under cmd, where nothing is pre-approved)
	in(kCmd, "git status --porcelain", wantCmdReadOnly().with(scParamMatch, DAllow), "git status* 覆盖带参数的 git status"),
	in(kCmd, "GIT status", wantCmdReadOnly().with(scParamMatch, DAllow), "param_match 不区分大小写（与 glob 一致）"),
	in(kCmd, "git status && git status -s", wantCmdReadOnly().with(scParamMatch, DAllow), "每条命令都匹配即放行"),
	in(kCmd, "git status && git log", wantCmdReadOnly(), "git log 不匹配 git status*，cmd 下不放行"),
	in(kCmd, "git -C . status", wantCmdReadOnly().rem(rememberNone), "全局选项在前，不是 git status 前缀"),
	ask("git statusx", g, rememberPrefix, "手册：git status* 不匹配 git statusx"),
	ask("git status; rm -rf ./src", u, rememberPrefix, "手册：git status* 不再放行 git status; rm -rf ./src"),
	ask("git status > out.txt", u, rememberNone, "param_match allow 不覆盖写文件重定向"),
	ask("git status $(rm x)", u, rememberNone, "param_match allow 不覆盖命令替换"),
	ask("git status | tee out.txt", u, rememberPrefix, "管道里的 tee 不匹配"),
	ask("sudo git status", u, rememberNone, "包裹命令不匹配 git status*"),
}
