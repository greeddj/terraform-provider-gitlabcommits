---
name: go-performance
description: Allocation-aware Go patterns for hot paths in this provider — preallocation, escape analysis, avoiding interface boxing, bounded concurrency with errgroup, sync.Pool when measured. Load when writing or reviewing code in Read / Update / diffActions or any goroutine-spawning code path.
---

# Allocation-aware Go for this provider

Per-tick allocations matter — the provider runs inside Terraform processes that may operate on hundreds of resources. The goal is not micro-optimization theater; it is **not allocating things you don't need to allocate**.

## Always

- **Preallocate slices and maps when the size is known.**
  ```go
  results := make([]fileRefreshResult, len(state.Files))   // good
  results := make([]fileRefreshResult, 0)                  // bad: grow-by-doubling
  ```
- **Preallocate map capacity** when you know the upper bound:
  ```go
  m := make(map[string]fileModel, len(plan.Files))
  ```
- **Use `strconv.*` instead of `fmt.Sprintf` for numbers.** `strconv.Itoa(n)` allocates one string; `fmt.Sprintf("%d", n)` allocates several intermediate things.
- **Avoid building paths with `+`** in loops; use `path.Join` once or a `strings.Builder` if joining many.
- **Pass small structs by value, large by pointer.** "Small" ≈ ≤ 3 machine words. `types.String` is small.
- **Return errors, not error-wrapped contexts in hot loops.** Wrap once at the boundary if you need context.

## Goroutine discipline

- **Bound goroutines.** `refreshParallelism` is the cap for Read; the parallel adopt-existing probes in Update follow the same pattern. **Never** spawn unbounded `go func()` over user-controlled input.
- **Use `errgroup.WithContext`** for parallel fan-out — it propagates the first error and cancellation:
  ```go
  g, gctx := errgroup.WithContext(ctx)
  g.SetLimit(refreshParallelism)
  paths := sortedKeys(state.Files)
  for i, p := range paths {
      g.Go(func() error {
          results[i] = probe(gctx, p)
          return nil
      })
  }
  if err := g.Wait(); err != nil { ... }
  // No `i, p := i, p` copy: go.mod targets go 1.26 (per-iteration loop vars)
  // and the copyloopvar linter rejects the redundant copy.
  ```
- **No map writes from goroutines** — each goroutine writes its own slice slot; serial pass after `g.Wait()` rebuilds the map. This is a load-bearing invariant in this repo.

## Escape analysis

Run `go build -gcflags="-m" ./internal/provider 2>&1 | grep "escapes to heap"` when a hot path is suspect. Common causes that move things to heap:

- Returning a pointer to a local.
- Capturing a local in a closure that outlives the function.
- Passing a value through an `interface{}` (or any interface, including `error` when the concrete type contains a non-pointer field).

You don't need to chase every escape — chase the ones in hot paths.

## Interface boxing

Each `var e error = &myErr{}` allocates a 2-word interface descriptor + the concrete value. In tight loops:

- Reuse a sentinel error: `var errNotFound = errors.New("not found")`.
- Don't wrap with `fmt.Errorf` unless you need the wrapped chain.

## `sync.Pool` — only when measured

`sync.Pool` is the right tool when you've measured high allocation rate of a reusable buffer (e.g. `*bytes.Buffer` for response decoding). Don't add it speculatively — it complicates correctness for no win when the allocation wasn't hot.

## Avoid in this codebase

- `init()` functions doing real work — they make testing harder and run order across packages is fragile.
- Global mutable state — the provider client is shared via `ResourceData`, not a package-level singleton.
- `panic` in non-test code — return a diagnostic.

## When in doubt — benchmark

```bash
go test ./internal/provider -bench=. -benchmem -run=^$
# allocs/op is the column that matters most for the provider
```

If you're proposing a "this is faster" change, attach the benchmark numbers. "Looks faster" doesn't ship.
