---
name: review
description: Code review: correctness, style, performance, security
conditional: true
paths: "*.go,*.py,*.js,*.ts,*.rs,*.java,*.rb,*.cpp,*.c"
---

# Code Review Checklist

## Review Points
1. Correctness: does the logic handle edge cases?
2. Style: does it follow project conventions?
3. Performance: any O(n²) issues or unnecessary allocations?
4. Security: input validation, error handling, secret management
5. Tests: are the important paths covered?
