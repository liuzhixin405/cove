# Contributing to agentgo

感谢您对 agentgo 的关注！我们欢迎各种形式的贡献。

## 行为准则

请参阅 [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)。

## 如何贡献

### 报告 Bug

1. 在 [Issues](https://github.com/agentgo/agentgo/issues) 中搜索是否已有相同问题
2. 使用 **Bug Report** 模板创建新 issue
3. 提供详细的复现步骤、期望行为和实际行为
4. 附上 `agentgo --doctor` 的诊断输出（如适用）

### 功能请求

1. 在 [Issues](https://github.com/agentgo/agentgo/issues) 中搜索类似请求
2. 使用 **Feature Request** 模板
3. 描述使用场景和期望的解决方案

### 提交代码

1. **Fork** 本仓库
2. 创建 feature 分支: `git checkout -b feature/my-feature`
3. 提交更改: `git commit -m 'feat: add some feature'`
4. 推送到分支: `git push origin feature/my-feature`
5. 提交 Pull Request

### Commit 规范

我们使用 [Conventional Commits](https://www.conventionalcommits.org/zh-hans/v1.0.0/)：

- `feat:` 新功能
- `fix:` 修复 bug
- `docs:` 文档更新
- `style:` 代码格式（不影响功能）
- `refactor:` 重构
- `test:` 测试相关
- `chore:` 构建/工具相关

### 开发环境

```bash
# 克隆仓库
git clone https://github.com/agentgo/agentgo.git
cd agentgo/agent

# 运行测试
go test ./...

# 构建
go build -o agentgo ./cmd/agentgo

# 运行 linter
golangci-lint run
```

### 代码风格

- 遵循 Go 标准代码风格 (`gofmt`, `go vet`)
- 为导出函数/类型添加文档注释
- 新功能需包含测试
- 保持向后兼容性

### Pull Request 检查清单

- [ ] 代码通过所有测试 (`go test ./...`)
- [ ] 通过 lint 检查 (`golangci-lint run`)
- [ ] 添加了必要的测试
- [ ] 更新了相关文档
- [ ] 遵循 commit 规范

## 许可证

贡献的代码将在 [MIT License](LICENSE) 下发布。
