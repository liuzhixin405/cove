---
name: test
description: Write comprehensive tests: unit, integration, edge cases
conditional: true
paths: "*_test.go,*_test.py,*.test.js,*.test.ts,*.spec.js,*.spec.ts"
---

# Testing Workflow

## Approach
1. Start with the happy path
2. Add edge cases (empty, nil, boundary values)
3. Test error conditions
4. Use table-driven tests for multiple cases
5. Aim for meaningful coverage, not 100%
