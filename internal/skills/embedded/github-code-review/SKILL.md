---
name: github-code-review
description: Review PRs on GitHub: read diffs, leave inline comments, approve or request changes.
---

# GitHub Code Review

Review open PRs with `gh` or `curl`, leave inline comments, approve or request changes.

## 1. Review Local Changes First

Always start with local review:

```bash
# Staged changes
git diff --staged --stat
git diff --staged

# Changes vs main
git diff main...HEAD --stat
git diff main...HEAD
```

## 2. Get PR Context

```bash
gh pr view $PR_NUMBER --json title,body,state,author,labels
gh pr diff $PR_NUMBER
```

Without `gh`:
```bash
OWNER=$(git remote get-url origin | sed -E 's|.*github\.com[:/]||; s|\.git$||' | cut -d/ -f1)
REPO=$(git remote get-url origin | sed -E 's|.*github\.com[:/]||; s|\.git$||' | cut -d/ -f2)
curl -H "Authorization: token $GITHUB_TOKEN" \
  "https://api.github.com/repos/$OWNER/$REPO/pulls/$PR_NUMBER"
```

## 3. Review Checklist

### Correctness
- [ ] Logic handles edge cases (empty, nil, boundary)
- [ ] Conditions are correct (not inverted)
- [ ] Loops terminate, recursion has base case

### Security
- [ ] No hardcoded secrets or credentials
- [ ] Input is validated/sanitized
- [ ] SQL/command injection risks addressed
- [ ] Error messages don't leak internals

### Performance
- [ ] No O(n^2) where O(n) exists
- [ ] No unnecessary allocations in hot paths
- [ ] Resource cleanup (files, connections)

### Style
- [ ] Follows project conventions
- [ ] Consistent naming
- [ ] No dead code or debug prints

### Testing
- [ ] New behavior has tests
- [ ] Edge cases covered
- [ ] Existing tests still pass

## 4. Leave Inline Comments

```bash
gh pr review $PR_NUMBER --comment --body "Specific issue on line 42: ..."
```

Without `gh`:
```bash
curl -H "Authorization: token $GITHUB_TOKEN" \
  "https://api.github.com/repos/$OWNER/$REPO/pulls/$PR_NUMBER/reviews" \
  -d '{"body":"Review summary","event":"COMMENT","comments":[{"path":"file.go","position":5,"body":"Issue description"}]}'
```

## 5. Submit Review

```bash
# Approve
gh pr review $PR_NUMBER --approve

# Request changes
gh pr review $PR_NUMBER --request-changes --body "Issues found: ..."
```

## Review Output Format

```
## PR #N Review: [title]

**Summary:** 1-2 sentence summary of changes

**Issues Found:**
- [ ] Critical: description (file.go:42)
- [ ] Minor: description (file.go:78)

**Verdict:** APPROVE / REQUEST_CHANGES / COMMENT
```
