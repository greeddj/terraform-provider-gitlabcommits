---
description: Apply copywrite license headers across the repo (`just headers`).
allowed-tools: Bash
---

```bash
just headers
```

Then show `git status` so the user sees which files received headers. If you applied headers to files the user wasn't expecting to touch (e.g. test fixtures, generated files), call that out.
