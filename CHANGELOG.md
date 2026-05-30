# Changelog

All notable changes to agentgo will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **Structured Diagnostic System** (`/diagnose`): 30+ error codes (E1xxx-E6xxx) covering config, API, network, model, shell, and data directory issues
  - `diagnostic.QuickCheck()` runs at startup to detect common problems before user interaction
  - `/diagnose full` — run all 9 diagnostic checks with detailed reports
  - `/diagnose quick` — fast subset of critical checks
  - `/diagnose codes` — list all known error codes and recovery actions
  - All fixes marked as HotFixable — applied immediately without restarting the exe
- **Integration Test Suite** (`engine_test.go`): 16 end-to-end tests covering all critical flow paths
  - Basic message flow, context cancellation, tool execution, permission flows
  - Tool panic recovery, API error handling, parallel tool execution
  - Unknown tool names, empty tool calls, multi-iteration conversations
  - Auto-permission mode, stream delta callbacks

### Fixed
- **Permission prompt hang**: Replaced `fmt.Scanln` with `bufio.Scanner` — empty input defaults to deny instead of blocking forever
- **Tool goroutine panic crash**: Added `defer recover()` in parallel tool execution goroutines — panics are caught and reported as tool errors
- **Engine loop after Ctrl+C**: Added `ctx.Err()` check at iteration start — immediately returns on context cancellation
- **WalkingIndicator race condition**: Synchronized `Stop()` with `doneCh` channel — prevents goroutine leak and race
- **Background goroutine panics**: Wrapped all `go` calls in `runTurnEndPipeline` with `recover()` — prevents uncaught panics from killing the process

### Changed
- Removed `NeedRestart` concept from diagnostic system — replaced with `HotFixable` field since all fixes apply immediately to the running process
- Repository structure cleaned up: docs moved to `docs/`, stray files removed, `.gitignore` updated

### Added
- Buddy system: interactive companion character with mood engine and sprite display
- Dream system: background memory consolidation and task processing
- Checkpoint system for session state persistence
- Delegate system for task offloading
- Guardrail system for safety and input validation
- Notes system for persistent note-taking
- Onboarding hints for new users
- Context subdirectory hints for better code navigation
- Advanced tools: batch operations, team orchestration, cron scheduling
- MCP SSE transport support
- Skills marketplace and installation
- Session export functionality
- Conversation state resumption with application state preservation
- Enhanced API key management with key pool and rotation

### Changed
- Refactored internal package structure with 25+ specialized packages
- Improved configuration system with migrations and environment variable detection
- Enhanced REPL with color support and better readline integration
- Upgraded cost tracking with more detailed token accounting
- Improved tool registry with pluggable architecture
- Better error handling and recovery across all subsystems

## [1.0.2] - 2025-05-24
### Added
- Cross-platform release artifacts for Windows, Linux, macOS (amd64/arm64)
- Automated release build script with checksum generation

### Fixed
- Various stream processing fixes
- API compatibility improvements

## [1.0.1] - 2025-05-XX
### Added
- Initial public release
- Multi-provider AI backend support (Anthropic, OpenAI, DeepSeek + 9 OpenAI-compatible)
- Interactive REPL with slash commands
- Git integration (commit, review, diff)
- Permission system (default, plan, auto, bypass)
- Config management with env vars, user config, and project-level overrides
- Token tracking and cost estimation
- Session save/resume
- Memory persistence
- MCP (Model Context Protocol) server support
- Plugin system
- Skills system

[Unreleased]: https://github.com/agentgo/agentgo/compare/v2.0.0...HEAD
[2.0.0]: https://github.com/agentgo/agentgo/compare/v1.0.2...v2.0.0
[1.0.2]: https://github.com/agentgo/agentgo/compare/v1.0.1...v1.0.2
[1.0.1]: https://github.com/agentgo/agentgo/releases/tag/v1.0.1
