---
name: spike
description: Throwaway experiments to validate an idea before building. Disposable by design.
conditional: true
paths: "*.go,*.py,*.js,*.ts,*.rs,*.java"
---

# Spike

Throwaway experiments to validate feasibility before committing to a real build. Spikes are disposable — throw them away once they've answered the question.

## When to Use

- "Let me try this first"
- "I want to see if X works"
- "Spike this out"
- "Before I commit to Y"
- "Is this even possible?"
- Comparing approach A vs B

## When NOT to Use

- Answer is knowable from docs or reading code — just research
- Production path — use the `plan` skill instead
- Already validated — jump to implementation

## Method

### 1. Decompose

Break the idea into 2-5 independent feasibility questions:

| # | Spike | Validates | Risk |
|---|-------|-----------|------|
| 001 | websocket-streaming | Given WS connection, when LLM streams, then chunks < 100ms | High |
| 002a | pdf-parse-libA | Given PDF, when parsed with libA, then text extractable | Medium |
| 002b | pdf-parse-libB | Same question, different library | Medium |

Order by risk. The spike most likely to kill the idea runs first.

### 2. Build

- Create a standalone file (not in main source tree)
- Use `write` to create `_spike_<name>.go` or similar
- Use `bash` to run it
- Keep it minimal — 20-50 lines

### 3. Verdict

After each spike, state clearly:
- **PASS**: Approach works, can proceed
- **FAIL**: Approach doesn't work, here's why
- **MIXED**: Works but with caveats

### 4. Cleanup

Delete spike files when done:
```bash
rm _spike_*.go
```
