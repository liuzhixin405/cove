---
name: executing-plans
description: Use when you have a written implementation plan to execute in a separate session with review checkpoints
conditional: true
---

# Executing Plans

When the user has an existing plan file or asks you to execute a plan:

## Workflow

1. **Read the plan** — load the `.cove/plans/` file or the plan the user references
2. **Convert to tasks** — use `todowrite` to create tasks matching the plan structure
   - Use `depends:<task-id>` for dependencies between tasks
   - Set appropriate priorities (high/medium)
3. **Execute** — call `execute_plan` with `parallel:true` for independent tasks
4. **Report** — summarize results, note any failures or skipped tasks

## Rules

- Execute one task at a time when they have sequential dependencies
- Run independent tasks (no `depends:` prefix in the same level) in parallel
- If a task fails, its dependent tasks are automatically skipped
- The user can interrupt and adjust: skip a task, change priority, or add new tasks
- After all tasks complete, verify the overall goal is met
