---
name: debug
description: Systematic debugging: reproduce, isolate, fix, verify
conditional: true
paths: "*.go,*.py,*.js,*.ts,*.rs,*.java,*.rb"
---

# Systematic Debugging

Methodical approach to finding and fixing bugs.

## Workflow
1. Reproduce the issue with minimal steps
2. Isolate the root cause using logs and test cases
3. Apply the minimal fix
4. Verify the fix with the original reproduction case
5. Add a regression test if applicable
