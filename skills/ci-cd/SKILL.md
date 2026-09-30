---
name: ci-cd
description: CI/CD: Actions, pipelines, testing.
---

# CI/CD

Set up or fix a pipeline that builds, tests and (optionally) releases the project. Match the CI system already in use (GitHub Actions, GitLab CI, Azure Pipelines…).

## 1. Inspect

- Read existing pipeline files (`.github/workflows/`, `.gitlab-ci.yml`, `azure-pipelines.yml`).
- Find the real local commands for format check, lint, build and test — the pipeline must run the same ones a developer runs.
- For a failing pipeline, read the failing job's log first and reproduce the step locally before editing YAML.

## 2. CI (every push and pull request)

- Jobs: lint → build → test; fail fast on the cheap checks.
- Pin the toolchain to the version the project declares; pin third-party actions to a tag or commit SHA.
- Cache dependencies keyed on the lock file.
- Use a matrix only for platforms/versions the project actually supports.
- Upload test reports and coverage as artifacts.
- Set `permissions:` to the minimum (usually `contents: read`) and a `timeout-minutes` on each job.
- Add `concurrency` to cancel superseded runs on the same branch.

Example (GitHub Actions, Go):

```yaml
name: CI
on:
  push:
    branches: [main]
  pull_request:
permissions:
  contents: read
concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true
jobs:
  test:
    runs-on: ubuntu-latest
    timeout-minutes: 15
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go vet ./...
      - run: go test -race ./...
```

## 3. CD (tags or protected branch only)

- Trigger releases on version tags or manual dispatch, never on every push.
- Secrets come from the CI secret store; never echo them. Use OIDC for cloud deploys where available.
- Deploy to an environment with required reviewers for production.
- Build once, promote the same artifact through environments.

## 4. Verify

Validate the YAML (e.g. `actionlint`), push to a branch, and check the run. Report what each job does and anything left for the user to configure (secrets, environments, branch protection).
