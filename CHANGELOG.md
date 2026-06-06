# Changelog

All notable changes to cove will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.0] - 2026-06-03

### Added
- **Self-Learning Pipeline**: Automatic memory extraction, background skill/memory review, and periodic cross-session memory consolidation (dream)
  - `extract` system: analyzes conversations after each turn, extracts durable facts into memory files
  - `backgroundReview`: auto-creates skills and memories from conversation patterns
  - `dream` system: periodic 4-phase consolidation (Orient â†?Gather â†?Consolidate â†?Prune) of all memory files
  - Triggered automatically via `runTurnEndPipeline` with built-in throttling
- **Skill Tools for Agent**: `skills_list` and `skill_view` tools â€?the agent can now discover and load skills autonomously
- **Conditional Skill Loading**: Skills with `paths` frontmatter now auto-inject when the agent opens matching files (e.g., opening a `.py` file loads Python-related skills)
- **23 Built-in Skills as Disk Files**: All skills now live in `~/.cove/skills/<name>/SKILL.md` â€?user-editable, with YAML frontmatter
- **Guardrail Time-Window Circuit Breaker**: Rapid failure detection â€?6+ consecutive failures in 30s triggers immediate block, success resets window
- **Session Notes Decision/Discovery Tracking**: Regex-based auto-detection of user decisions and code discoveries, saved to `session_notes.md`
- **Memory Deduplication**: New memories with >80% similarity to existing ones are merged instead of duplicated
- **Hooks Event System**: `PreToolUse`, `PostToolUse`, `SessionStart` events now fire in the engine for plugin extensibility
- **Checkpoint Auto-Trigger**: Git-based file snapshots automatically created before `write`/`edit` operations
- **`/dream` Command**: Manual memory consolidation trigger now available

### Changed
- **Skill System**: Hardcoded skill bundles replaced with disk-based SKILL.md files â€?loaded from `~/.cove/skills/`, `~/.claude/skills/`, and project directories
- **`skill_view`/`skills_list` Tools**: Agent tools replace the old user-only `/skill` invocation pattern
- **`Matching()` Fixed**: Skill conditional loading now uses `filepath.Match` glob patterns instead of name substring
- **`backgroundReview` Throttled**: Minimum 4 new messages required between reviews
- **`SetAutoExtract`**: No longer a no-op â€?background features are now enabled by default
- **`runTurnEndPipeline`**: Now orchestrates extract â†?backgroundReview â†?dream â†?session notes flush

### Removed
- **Buddy System**: Removed `internal/buddy/` (12 files) â€?virtual pet companion and all related commands (`/buddy`, `/pet`, `/companion`)
- **Suggest System**: Removed `internal/suggest/` â€?follow-up suggestion generation
- **Hardcoded Skill Bundles**: Replaced with disk-based SKILL.md files in `~/.cove/skills/`

### Security
- **Guardrail Enhancements**: Rapid-failure circuit breaker prevents runaway tool loops more aggressively
- **Project renamed from `cagentcli` to `cove`**: Repository, module path, binary, and data directory (`~/.cove/`) all updated
- **Directory structure flattened**: `agent/` subdirectory removed, Go module root is now repository root
- **`cli/cove/` renamed to `cli/cove/`**: Binary entry point updated
  
### Removed
- **Legacy fix scripts**: 20 `fix*.py` debug scripts deleted
- **`.claude/` config**: Claude-specific skills removed

### Added
- **Structured Diagnostic System** (`/diagnose`): 30+ error codes (E1xxx-E6xxx) covering config, API, network, model, shell, and data directory issues
  - `diagnostic.QuickCheck()` runs at startup to detect common problems before user interaction
  - `/diagnose full` ï¿?run all 9 diagnostic checks with detailed reports
  - `/diagnose quick` ï¿?fast subset of critical checks
  - `/diagnose codes` ï¿?list all known error codes and recovery actions
  - All fixes marked as HotFixable ï¿?applied immediately without restarting the exe
- **Integration Test Suite** (`engine_test.go`): 16 end-to-end tests covering all critical flow paths
  - Basic message flow, context cancellation, tool execution, permission flows
  - Tool panic recovery, API error handling, parallel tool execution
  - Unknown tool names, empty tool calls, multi-iteration conversations
  - Auto-permission mode, stream delta callbacks

### Fixed
- **Permission prompt hang**: Replaced `fmt.Scanln` with `bufio.Scanner` ï¿?empty input defaults to deny instead of blocking forever
- **Tool goroutine panic crash**: Added `defer recover()` in parallel tool execution goroutines ï¿?panics are caught and reported as tool errors
- **Engine loop after Ctrl+C**: Added `ctx.Err()` check at iteration start ï¿?immediately returns on context cancellation
- **WalkingIndicator race condition**: Synchronized `Stop()` with `doneCh` channel ï¿?prevents goroutine leak and race
- **Background goroutine panics**: Wrapped all `go` calls in `runTurnEndPipeline` with `recover()` ï¿?prevents uncaught panics from killing the process

### Changed
- Removed `NeedRestart` concept from diagnostic system ï¿?replaced with `HotFixable` field since all fixes apply immediately to the running process
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

[Unreleased]: https://github.com/liuzhixin405/cove/compare/v2.0.0...HEAD
[2.0.0]: https://github.com/liuzhixin405/cove/compare/v1.0.2...v2.0.0
[1.0.2]: https://github.com/liuzhixin405/cove/compare/v1.0.1...v1.0.2
[1.0.1]: https://github.com/liuzhixin405/cove/releases/tag/v1.0.1
