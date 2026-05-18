---
description: Regenerate provider docs via `just docs` (tfplugindocs).
allowed-tools: Bash
---

```bash
just docs
```

After regen, show `git status -- docs/` so the user can see what changed. If nothing changed, say so explicitly.

Remind the user that schema changes require committing the regenerated docs — CI fails on drift.
