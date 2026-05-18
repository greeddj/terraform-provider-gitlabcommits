---
description: Sync go.mod / go.sum via `just deps` (go mod tidy).
allowed-tools: Bash
---

```bash
just deps
```

Then show `git diff -- go.mod go.sum` so the user can review which deps moved. New direct deps should be justified — flag them and ask before committing if anything unexpected appeared.
