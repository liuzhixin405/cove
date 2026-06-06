---
name: verify
description: Verify changes: build, lint, test, manual check
conditional: true
paths: "*.go,*.py,*.js,*.ts,*.rs"
---

# Verification Checklist

## Steps
1. Build succeeds without errors or warnings
2. Lint/typecheck passes
3. Tests pass (existing + new)
4. Manual verification if applicable
5. No debug code or comments left behind
6. Git status is clean or changes are intentional
