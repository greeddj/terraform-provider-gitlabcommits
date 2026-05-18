---
description: Regenerate docs and verify there is no drift (`just docs-check`). The same gate CI runs.
allowed-tools: Bash
---

```bash
just docs-check
```

On failure, the diff of regenerated vs committed docs is the answer — show the user a short summary of which doc files drifted (`git diff --stat -- docs`) and remind them to commit the regenerated files.
