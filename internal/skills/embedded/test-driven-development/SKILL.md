---
name: test-driven-development
description: TDD: enforce RED-GREEN-REFACTOR cycle. Tests before code, always.
conditional: true
paths: "*.go,*.py,*.js,*.ts,*.rs,*.java"
---

# Test-Driven Development

**Core principle:** If you didn't watch the test fail, you don't know if it tests the right thing.

## The Iron Law

```
NO PRODUCTION CODE WITHOUT A FAILING TEST FIRST
```

Write code before the test? Delete it. Start over. No exceptions.

## RED-GREEN-REFACTOR Cycle

### RED — Write Failing Test

Write one minimal test showing what should happen:

```go
func TestRetriesFailedOperations(t *testing.T) {
    attempts := 0
    operation := func() error {
        attempts++
        if attempts < 3 {
            return errors.New("fail")
        }
        return nil
    }
    err := retryOperation(operation)
    if err != nil {
        t.Fatalf("expected success, got %v", err)
    }
    if attempts != 3 {
        t.Fatalf("expected 3 attempts, got %d", attempts)
    }
}
```

Use `bash` to run it:
```bash
go test -run TestRetriesFailedOperations ./path/
```
Expected: FAIL — function not defined.

### GREEN — Minimal Implementation

Write the absolute minimum code to make the test pass:

```go
func retryOperation(op func() error) error {
    var err error
    for i := 0; i < 3; i++ {
        err = op()
        if err == nil {
            return nil
        }
    }
    return err
}
```

Run again: `go test -run TestRetriesFailedOperations ./path/`
Expected: PASS

### REFACTOR — Clean Up

Now that tests pass, improve the code:
- Remove duplication
- Improve names
- Add error wrapping if needed

Run tests after each refactor step.

## Red Flags

If you find yourself thinking:
- "This is simple, I can write the code first" → Stop. Write the test.
- "I'll add tests later" → No. Now.
- "Testing this is hard" → The design might be wrong. Consider refactoring for testability.
