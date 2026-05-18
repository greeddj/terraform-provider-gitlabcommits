---
description: Run the exact CI gate locally (`just ci` = check + lint + test + check-tf-fmt + docs-check + headers-check).
allowed-tools: Bash
---

```bash
just ci
```

This is what CI runs. If it passes locally, the PR will pass. Surface failures in the order Justfile runs them (check → lint → test → tf-fmt → docs-check → headers-check) so the user knows which gate broke first.
