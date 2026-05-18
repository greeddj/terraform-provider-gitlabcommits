---
description: Run `just lint` — golangci-lint over the whole module.
allowed-tools: Bash
---

```bash
just lint
```

Group findings by linter (govet, staticcheck, gosec, fieldalignment, etc.). If gosec flags G401/G505 anywhere outside the documented SHA-1 use in `gitBlobSHA`, raise that as a problem — that exclusion is narrowly intentional.
