---
name: refactor
description: Safe refactoring in small, reversible steps
conditional: true
paths: "*.go,*.py,*.js,*.ts,*.rs,*.java,*.rb,*.cpp,*.c"
---

# Safe Refactoring

## Workflow
1. Understand the existing code and its tests
2. Plan the refactoring in small, reversible steps
3. Run tests after each step
4. Keep the public API stable
5. Update documentation if behavior changes
