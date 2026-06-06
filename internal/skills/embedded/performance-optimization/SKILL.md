---
name: performance-optimization
description: Optimize based on measurement, never guesswork. Profile first, fix the real bottleneck, verify the win.
---

# Performance Optimization

**Core principle:** Measure, don't guess. Intuition about hotspots is usually wrong.

## The Iron Law

```
NO OPTIMIZATION WITHOUT A MEASUREMENT THAT JUSTIFIES IT.
```

## Phase 1: Establish a Baseline

BEFORE changing anything:

1. **Reproduce** the slow scenario reliably with `bash`.
2. **Measure** it — time it, benchmark it, or profile it. Record concrete numbers.
3. **Set a target**: "reduce p95 from 800ms to <200ms". Without a target you can't know when to stop.

## Phase 2: Find the Real Bottleneck

- **Profile**, don't eyeball. Use the language's profiler (e.g. `go test -bench`/`pprof`, `cProfile`, `perf`, browser devtools).
- Look for the dominant cost: the 20% of code taking 80% of the time.
- Common real culprits: N+1 queries, unbounded allocations in loops, repeated O(n) scans, missing indexes, blocking I/O, lock contention, re-computation that should be cached.

## Phase 3: Fix the Dominant Cost First

- Change ONE thing, then re-measure. Attributing wins requires isolation.
- Prefer algorithmic wins (O(n²)→O(n)) over micro-optimizations.
- Cache only when re-computation is proven expensive AND invalidation is tractable.
- Parallelize only after the serial bottleneck is understood — and bound the concurrency.

## Phase 4: Verify and Guard

1. Re-run the baseline measurement. Did you hit the target? By how much?
2. Confirm correctness — run the full test suite. A fast wrong answer is worthless.
3. Add a benchmark or assertion so the win doesn't silently regress.

## Red Flags — STOP

- Optimizing without a profile → you're guessing.
- "This feels slow" with no number → measure first.
- Micro-tuning a function that's 1% of runtime → wrong target.
- Trading readability for an unmeasured "win" → revert.
- Caching everything → you've created an invalidation-bug factory.
