package permission

// Build and test lines (danger level 构建测试): auto mode runs them unasked
// under Git Bash and PowerShell, default mode asks, plan refuses, cmd.exe
// never pre-approves. Sources: classifier_test.go, classifier_shellkind_test.go,
// manual 权限模式 (auto 行) and 无法确定含义时一律询问 ("auto 模式下与 go test 一致").

// goTest is a build pair whose line allow-prefix-go-test covers.
func goTest(cmd, note string, opts ...pairOpt) []permCase {
	return build(cmd, note, append([]pairOpt{remAll(rememberGroup), decides(scAllowGo, DAllow)}, opts...)...)
}

// grouped is a build pair of a program in a routine group (go, cargo,
// dotnet, npm/pnpm): [a]/[p] remember the group.
func grouped(cmd, note string, opts ...pairOpt) []permCase {
	return build(cmd, note, append([]pairOpt{remAll(rememberGroup)}, opts...)...)
}

var corpusBuild = join(
	// go
	goTest("go test ./...", "手册示例 go test 在 auto 下免询问"),
	goTest("go test -race ./...", "go test 带选项"),
	goTest("go test -run TestX ./internal/...", "go test -run"),
	goTest("go test -count=1 -v ./internal/permission/", "go test 多个选项"),
	// cmd.exe halves of these two are in corpus_shellkind_test.go.
	goTest("go test -run 'a|b' ./...", "引号内的 | 是 -run 参数（POSIX/PS 信任引号）")[:1],
	goTest("cd src && go test ./... && echo done", "手册：cd/echo 只读伴随 go test 被前缀覆盖")[:1],
	grouped("go build ./...", "go build 构建"),
	grouped("go build -o bin/cove ./cli/cove", "go build -o 写的是构建产物，仍是构建类"),
	grouped("go vet ./...", "go vet"),
	grouped("go list ./...", "手册：go list 按构建类"),
	grouped("go build ./... && go test ./...", "链式构建 + 测试仍是构建类"),
	grouped("go vet ./... && go test ./...", "链式 vet + test"),
	build("gofmt -l .", "手册：gofmt 按构建类"),
	build("gofmt -d main.go", "gofmt -d 只输出 diff"),

	// cargo
	grouped("cargo test", "cargo test"),
	grouped("cargo build", "cargo build"),
	grouped("cargo build --release", "cargo build --release"),
	grouped("cargo check", "cargo check"),

	// make
	build("make", "手册：不带参数的 make"),
	build("make test", "手册：make test"),

	// dotnet
	grouped("dotnet build", "手册：dotnet build"),
	grouped("dotnet test", "手册：dotnet test"),
	grouped("dotnet run", "手册：dotnet run"),
	grouped("dotnet build -c Release", "dotnet build 带选项"),

	// pytest
	build("pytest", "手册：pytest"),
	build("pytest -q tests", "pytest 带选项"),
	build("pytest -k smoke tests/unit", "pytest -k"),

	// npm / pnpm scripts
	grouped("npm test", "手册：npm test"),
	grouped("npm run test", "npm run test"),
	grouped("npm run test:unit", "手册：带冒号后缀的 test 脚本"),
	grouped("npm run build", "npm run build"),
	grouped("npm run build:prod", "手册：build:prod"),
	grouped("npm run lint", "npm run lint"),
	grouped("npm run lint:fix", "手册：lint:fix"),
	grouped("npm run typecheck", "手册：typecheck"),
	grouped("npm run check", "npm run check"),
	grouped("pnpm test", "手册：pnpm test"),
	grouped("pnpm build", "pnpm build"),
	grouped("pnpm run lint", "pnpm run lint"),
)
