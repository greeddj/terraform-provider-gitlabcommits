---
description: Run `just test` — unit tests (go test ./...). Pass `$ARGUMENTS` as a -run pattern to target one test.
allowed-tools: Bash
argument-hint: [optional -run pattern]
---

If `$ARGUMENTS` is empty, run the full unit suite:

```bash
just test
```

Otherwise run a focused single test (skipping the Justfile wrapper for the pattern flag):

```bash
go test ./internal/provider -v -run "$ARGUMENTS"
```

Report pass/fail per package. On failure, surface the failing test name and the first error line — do not paste the full log unless asked.
