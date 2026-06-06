---
name: systematic-debugging
description: 4-phase root cause debugging: understand bugs before fixing. Never guess at fixes.
conditional: true
paths: "*.go,*.py,*.js,*.ts,*.rs,*.java,*.rb,*.cpp,*.c"
---

# Systematic Debugging

**Core principle:** ALWAYS find root cause before attempting fixes. Symptom fixes are failure.

## The Iron Law

```
NO FIXES WITHOUT ROOT CAUSE INVESTIGATION FIRST
```

## Phase 1: Root Cause Investigation

BEFORE attempting ANY fix:

### 1. Read Error Messages Completely
- Don't skip past errors or warnings
- Read stack traces fully, note line numbers and error codes
- Use `read` to open relevant source files
- Use `grep` to find the error string in the codebase

### 2. Reproduce Consistently
- Use `bash` to run the failing test or trigger the bug
- If not reproducible: gather more data, don't guess
- Isolate to minimal reproduction case

### 3. Trace the Flow
- Start from the error site, work backwards
- Use `grep` to find callers, `read` to understand logic
- Map the complete path from input to error

### 4. Form a Hypothesis
- State: "I believe the root cause is X because Y"
- Identify the exact line(s) that need to change

## Phase 2: Design the Fix

- Write down what changes are needed and why
- Consider: could this fix break anything else?
- Design the test that proves the fix works

## Phase 3: Implement and Test

- Apply the minimal fix using `edit`
- Run the reproduction case: `bash` the test
- Verify no regressions: `bash` the full suite

## Phase 4: Prevent Recurrence

- Add a regression test if one doesn't exist
- Consider: could this class of bug exist elsewhere?

## When to Use

Use for ANY technical issue. Use ESPECIALLY when:
- Under time pressure (emergencies make guessing tempting)
- "Just one quick fix" seems obvious
- You've already tried multiple fixes
- Previous fix didn't work
