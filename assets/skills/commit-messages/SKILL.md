---
name: commit-messages
description: Write clear Conventional Commits. Imperative subject, explain why not what, one logical change per commit.
---

# Commit Messages

Write commits that a reviewer (and future you) can understand without reading the diff.

## Format (Conventional Commits)

```
<type>(<optional scope>): <imperative subject, <=50 chars>

<body: wrap at ~72 cols. Explain WHY, not WHAT. The diff shows what.>

<optional footer: BREAKING CHANGE:, Refs #123, Co-authored-by:>
```

## Types

- `feat` — a new user-facing capability
- `fix` — a bug fix
- `refactor` — structure change, no behavior change
- `perf` — performance improvement
- `test` — add or fix tests only
- `docs` — documentation only
- `build` / `ci` — build system or pipeline
- `chore` — tooling, deps, housekeeping

## Rules

1. **Imperative mood**: "add", "fix", "remove" — not "added"/"fixes"/"adding".
2. **Subject <= 50 chars**, no trailing period, capitalized first word.
3. **Body explains WHY**: the motivation, trade-offs, and context. The code already shows what changed.
4. **One logical change per commit.** If you wrote "and" in the subject, consider splitting.
5. **Reference issues** in the footer (`Refs #42`, `Closes #42`).
6. **Flag breaking changes** with a `BREAKING CHANGE:` footer.

## Before Committing

- Run `bash` to check `git diff --staged` — does every hunk belong to this message?
- Unrelated changes? Unstage them (`git restore --staged <file>`) and commit separately.
- Never commit commented-out code, debug prints, or secrets.

## Good vs Bad

Bad:
```
update stuff
```

Good:
```
fix(parser): handle empty tool-call arguments

DeepSeek streams an empty string for tool calls with no args,
which broke JSON decoding and dropped the call. Treat empty
args as an empty object instead of skipping the call.

Refs #128
```
