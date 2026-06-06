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
