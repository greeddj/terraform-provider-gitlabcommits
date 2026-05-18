---
description: Run `just check` — go vet + staticcheck + govulncheck + fieldalignment.
allowed-tools: Bash
---

```bash
just check
```

This is the static-analysis gate. Report each tool's result separately. govulncheck findings are particularly important — flag CVE IDs and the affected dependency clearly.
