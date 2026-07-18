---
description: Run `just lint` — golangci-lint over the whole module.
allowed-tools: Bash
---

```bash
just lint
```

Group findings by linter (govet, staticcheck, gosec, revive, etc.). If gosec flags G401/G505 anywhere, raise it as a real problem - this provider has no local hashing and no gosec exclusions; do not silence the finding.
