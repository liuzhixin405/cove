---
name: safe-refactoring
description: Refactor code without changing behavior. Small steps, green tests after every change, no mixing with feature work.
---

# Safe Refactoring

**Core principle:** Refactoring changes structure, NOT behavior. If tests change meaning, it is not a refactor.

## The Iron Law

```
NEVER mix refactoring with behavior changes in the same step.
```

If you need to do both, do them as separate, clearly-labeled changes.

## Pre-Flight Checklist

BEFORE touching any code:

1. **Confirm a safety net exists.** Run the test suite with `bash`. If it is red, stop — fix or characterize first.
2. **If there are no tests**, add characterization tests that pin the CURRENT behavior (even bugs) before refactoring.
3. **Identify the smallest unit** you can change and re-verify independently.

## The Refactor Loop

Repeat in tiny increments:

1. Make ONE structural change (extract function, rename, inline, move).
2. Run the relevant tests with `bash`.
3. Green → commit-worthy checkpoint. Red → revert immediately, do not "fix forward".
4. Repeat.

## Safe Transformations (prefer these)

- **Rename** — use a rename tool/IDE refactor when available, not blind find-replace.
- **Extract function/variable** — pull a named concept out; behavior identical.
- **Inline** — collapse needless indirection.
- **Move** — relocate a function/type to a better home, update imports only.
- **Introduce parameter object** — group related args.

## Red Flags — STOP

- "While I'm here, let me also fix..." → separate change.
- Test assertions need editing to pass → you changed behavior.
- A single step touches many files AND many concerns → split it.
- You can't explain why behavior is preserved → don't ship it.

## Output Discipline

- Keep each diff focused and reviewable.
- State explicitly: "This is a pure refactor; behavior unchanged; tests green."
