---
name: requesting-code-review
description: Pre-commit verification: security scan, quality gates, auto-fix before committing.
---

# Pre-Commit Code Verification

**Core principle:** No agent should verify its own work. Fresh context finds what you miss.

## When to Use

- Before `git commit` or `git push`
- When user says "commit", "push", "ship", "done", "verify"
- After completing a task with 2+ file edits

## Step 1 — Get the Diff

```bash
git diff --staged
```
If empty, try `git diff` then `git diff HEAD~1 HEAD`. If still empty, nothing to verify.

## Step 2 — Security Scan

```bash
# Hardcoded secrets
git diff --staged | grep "^+" | grep -iE "(api_key|secret|password|token|passwd)\s*=\s*['\"][^'\"]{6,}['\"]"

# Shell injection risk
git diff --staged | grep "^+" | grep -E "os\.system\(|exec\.Command.*\+"

# Unsafe operations
git diff --staged | grep "^+" | grep -E "io/ioutil\.ReadFile|os\.Chmod\(.*0777"
```

Any match is a blocking concern.

## Step 3 — Run Tests

```bash
go test ./... -count=1
```
Only new failures block the commit. If tests already fail, note them but don't block.

## Step 4 — Lint Check

```bash
# Go
go vet ./...

# Also check formatting
gofmt -l .
```
Any `go vet` warnings in changed code must be fixed.

## Step 5 — Review Checklist

- [ ] No debug print statements (`fmt.Println("DEBUG")`, `log.Print`)
- [ ] No commented-out code
- [ ] Error handling for all fallible operations
- [ ] No hardcoded paths or secrets
- [ ] Git status shows only intended changes

## Step 6 — Fix Issues

For each issue found, apply the minimal fix with `edit`, then re-run tests.

## Step 7 — Report

Summarize: what was checked, issues found, issues fixed, remaining concerns.
