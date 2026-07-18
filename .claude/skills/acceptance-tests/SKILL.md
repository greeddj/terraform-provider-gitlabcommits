---
name: acceptance-tests
description: How to run, gate, and write acceptance tests against a real GitLab instance for this provider. Covers the required env vars, the namespaced cleanup convention, retry semantics, and the difference between unit and acceptance scope. Load before running or designing any TestAcc* test.
---

# Acceptance tests — playbook

Acceptance tests in this provider hit a **real GitLab project**. They're gated by `TF_ACC=1` and skipped otherwise (the standard Terraform Plugin Framework convention).

## Required env vars

| Var | Required | Default | Notes |
|---|---|---|---|
| `TF_ACC` | yes | — | Must be `1`. Without it, acceptance tests skip. |
| `GITLAB_TOKEN` | yes | — | Personal, Project, or Group access token with the `api` scope. See README Authentication. |
| `GITLAB_TEST_PROJECT_ID` | yes | — | Group/project path (e.g. `my-group/sandbox`) or numeric ID. |
| `GITLAB_TEST_BRANCH` | no | `tf-acc-test` | Branch to run against. It must already exist: the test config does not set `create_branch_from`, and no test code creates or deletes branches. |
| `GITLAB_BASE_URL` | no | gitlab.com | Set for self-hosted GitLab. |

## Run one test

```bash
TF_ACC=1 \
GITLAB_TOKEN=glpat-xxx \
GITLAB_TEST_PROJECT_ID=my-group/sandbox \
go test -v ./internal/provider -run TestAccFiles_basic -timeout 10m
```

## Run all acceptance tests

```bash
TF_ACC=1 GITLAB_TOKEN=... GITLAB_TEST_PROJECT_ID=... \
  go test -v ./internal/provider -run '^TestAcc' -timeout 30m
```

## Cleanup convention

Every path written by acceptance tests lives under `tf-acc-test/` (the constant `accTestPathPrefix` in the test code). If a run hangs, find leftovers with:

```bash
glab api -X GET "projects/<id>/repository/tree?path=tf-acc-test&recursive=true"
```

Then delete the test branch (`GITLAB_TEST_BRANCH`) to wipe everything in one operation.

## What acceptance tests cover that units don't

- Real GitLab API responses (including rate limits, retries, optimistic-lock 400s).
- Real network errors and timeouts.
- `terraform import` against a real branch.
- End-to-end `apply` → `plan` → `apply` no-op → `destroy` cycles.

## What stays in unit-test land

- `diffActions` shape (in/out comparison).
- bytewise content comparison in diffActions (state vs plan equality).
- Validator regex behavior.
- Plan-modifier behavior.
- `apiErrorDiag` parsing of canned GitLab error bodies.

If you can verify it without a real GitLab, **keep it in unit tests** — they're 100x faster and don't burn API quota.

## Acceptance test structure

Use `resource.Test` from `github.com/hashicorp/terraform-plugin-testing/helper/resource`, mirroring the existing helpers (`testAccPreCheck`, `accBranch`, `accConfig`, `accCheckFileExists` / `accCheckFileGone`):

```go
func TestAccFiles_basic(t *testing.T) {
    testAccPreCheck(t)
    project := os.Getenv("GITLAB_TEST_PROJECT_ID")
    branch := accBranch(t)
    resource.Test(t, resource.TestCase{
        ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
        Steps: []resource.TestStep{
            { Config: accConfig(project, branch, files), Check: resource.ComposeAggregateTestCheckFunc(...) },
        },
    })
}
```

`testAccPreCheck` skips the test when `TF_ACC` is unset and fails (`t.Fatal`) when `GITLAB_TOKEN` or `GITLAB_TEST_PROJECT_ID` is missing.

## Convergence assertion after import

There is no dedicated convergence helper; the pattern lives in `TestAccFiles_import`: a create step, an `ImportState: true` step with `ImportStateId "<project>::<branch>"` and `ImportStateVerify: false` (the files map is intentionally empty right after import), then a re-apply of the same config - `adopt_existing` rewrites the spurious creates into updates and the framework's automatic plan-after-apply check enforces zero drift. New import-related tests should follow that shape.

## Don't

- Don't hardcode project IDs. Always read from env.
- Don't commit a real token. Ever.
- Don't leave acceptance tests un-namespaced (no `tf-acc-test/` prefix).
- Don't run acceptance tests in CI without throttling — GitLab.com rate limits will hurt.
