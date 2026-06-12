---
name: plan
description: Write an actionable markdown plan before implementation. Bite-sized tasks, exact paths, complete code.
conditional: true
paths: "*.go,*.py,*.js,*.ts,*.rs,*.java,*.rb"
---

# Plan Mode

When asked to plan, do NOT implement code. Your deliverable is a markdown plan.

## Core Rules

- Do not implement code
- Do not run mutating bash commands, commit, or push
- You may read files, grep, and glob for context
- Save the plan as `.cove/plans/YYYY-MM-DD_HHMMSS-<slug>.md`

## Plan Structure

Each plan must include:

```markdown
# [Feature Name] Implementation Plan

**Goal:** One sentence

**Architecture:** 2-3 sentences about approach

---

### Task N: [Name]

**Objective:** What this accomplishes

**Files:**
- Create: `exact/path/file.go`
- Modify: `exact/path/file.go:45-67`

**Step 1: Implementation**
[Exact code]

**Step 2: Verification**
Run: `go test ./path/...`
Expected: PASS
```

## Principles

- Bite-sized tasks (2-5 minutes each)
- Exact file paths, not "the config file"
- Complete code examples that are copy-pasteable
- One action per task
- DRY, YAGNI, TDD

## After Saving

Tell the user the plan path and ask if they want execution to begin.

## Task Dependencies

When using `todowrite` to convert a plan into executable tasks, declare dependencies
using the `depends:<task-id>` prefix in the content field:

- `depends:task-1` — single dependency, task runs after task-1 completes
- `depends:task-1,task-2` — multiple dependencies, task waits for all
- No `depends:` prefix — no dependencies, ready to execute immediately

Example:
```json
{
  "todos": [
    {"content": "Create auth middleware", "status": "pending", "priority": "high"},
    {"content": "depends:task-1 Create auth tests", "status": "pending", "priority": "high"},
    {"content": "depends:task-1,task-2 Wire up routes", "status": "pending", "priority": "medium"}
  ]
}
```

After defining tasks, call `execute_plan` with parallel=true to run independent tasks
concurrently. This respects the dependency graph automatically.
