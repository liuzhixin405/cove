---
name: github-pr-workflow
description: GitHub PR lifecycle: branch, commit, open PR, monitor CI, merge.
conditional: true
paths: "*.go,*.py,*.js,*.ts,*.rs,*.java"
---

# GitHub Pull Request Workflow

Manages the full PR lifecycle using `gh` CLI where available, `git` + `curl` as fallback.

## 1. Branch Creation

```bash
git fetch origin
git checkout main && git pull origin main
git checkout -b feat/descriptive-name
```

Conventions: `feat/`, `fix/`, `refactor/`, `docs/`, `ci/`, `test/`

## 2. Make Changes and Commit

Use `write` and `edit` tools for changes, then:

```bash
git add path/to/file.go
git commit -m "feat: brief description of change"
```

Commit message format: `type: description`
Types: feat, fix, refactor, test, docs, chore, ci

## 3. Push and Open PR

```bash
git push -u origin feat/descriptive-name
gh pr create --title "feat: description" --body "## Summary\n\n...\n\n## Test Plan\n\n..."
```

Without `gh`:
```bash
OWNER=$(git remote get-url origin | sed -E 's|.*github\.com[:/]||; s|\.git$||' | cut -d/ -f1)
REPO=$(git remote get-url origin | sed -E 's|.*github\.com[:/]||; s|\.git$||' | cut -d/ -f2)
curl -H "Authorization: token $GITHUB_TOKEN" \
  -H "Accept: application/vnd.github.v3+json" \
  "https://api.github.com/repos/$OWNER/$REPO/pulls" \
  -d '{"title":"feat: desc","head":"feat/descriptive-name","base":"main","body":"..."}'
```

## 4. Monitor CI

```bash
gh pr checks
gh pr view --json state,statusCheckRollup
```

## 5. Merge

```bash
gh pr merge --squash --delete-branch
git checkout main && git pull origin main
```

Without `gh`:
```bash
curl -X PUT -H "Authorization: token $GITHUB_TOKEN" \
  "https://api.github.com/repos/$OWNER/$REPO/pulls/$PR_NUMBER/merge" \
  -d '{"merge_method":"squash"}'
```
