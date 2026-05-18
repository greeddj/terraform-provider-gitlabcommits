---
description: Run automated fixes (`just fix`) — go fix + fieldalignment -fix.
allowed-tools: Bash
---

```bash
just fix
```

Then show `git status` and `git diff --stat`. fieldalignment will reorder struct fields silently — review the diff before committing to make sure nothing semantically meaningful changed (it shouldn't, but verify).
