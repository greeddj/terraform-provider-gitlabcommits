---
description: Run acceptance tests against a real GitLab project. Requires TF_ACC, GITLAB_TOKEN, GITLAB_TEST_PROJECT_ID in the env. Pass `$ARGUMENTS` as a -run pattern (default `^TestAcc`).
allowed-tools: Bash
argument-hint: [optional -run pattern, e.g. TestAccFiles_basic]
---

First confirm the env is set up:

```bash
if [ -z "$TF_ACC" ] || [ -z "$GITLAB_TOKEN" ] || [ -z "$GITLAB_TEST_PROJECT_ID" ]; then
    echo "Missing required env: TF_ACC=1, GITLAB_TOKEN=..., GITLAB_TEST_PROJECT_ID=..."
    echo "Optional: GITLAB_TEST_BRANCH (default tf-acc-test), GITLAB_BASE_URL (for self-hosted)."
    exit 2
fi
```

Then run the suite:

```bash
PATTERN="${ARGUMENTS:-^TestAcc}"
go test -v ./internal/provider -run "$PATTERN" -timeout 30m
```

Reminders:

- All test paths are namespaced under `tf-acc-test/`. If a run hangs and leaves files, delete the `tf-acc-test` branch in the test project to wipe them.
- Acceptance tests burn real API quota — don't loop them locally.
- Do not commit any value of `GITLAB_TOKEN`. Ever.

See `.claude/skills/acceptance-tests/SKILL.md` for full details.
