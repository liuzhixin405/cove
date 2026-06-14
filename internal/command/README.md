# internal/command layout

This package is split by command domain to keep files short and easy to maintain.

## File responsibilities

- commands_types.go: command structs and constructor functions (New*Cmd).
- command.go: shared interfaces, Input/Output, and registry.
- commands_helpers.go: shared helpers used by multiple commands.
- commands_git.go: commit/review/diff workflow commands.
- commands_memory.go: memory management commands.
- commands_session.go: session state and context commands.
- commands_session_misc.go: compact/cost/resume commands.
- commands_runtime.go: runtime/config/doctor commands.
- commands_project.go: init and dream commands.
- commands_integrations.go: mcp/plugin/skills commands.

## Editing guideline

When adding a command, place it in the closest domain file first. If no domain fits, create a focused file instead of growing unrelated ones.
