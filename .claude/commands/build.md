---
description: Run `just build` — full pipeline (check + lint + test + static-linked binary into ./dist)
allowed-tools: Bash
---

Run the project build via the Justfile. Report success or relay any failure verbatim.

```bash
just build
```

If `just build` fails, do not retry blindly — read the failure, identify whether it's a check / lint / test failure, and surface the actionable bit. Don't try to "fix" the build without user confirmation.
