---
name: gitlab-provider-conventions
description: Repo-specific invariants for this Terraform GitLab provider — one-commit-per-apply, action-diffing, HEAD-style drift probes, serial state mutation, optimistic locking, intentional SHA-1 use. Load before designing or editing internal/provider/ code, especially files_resource.go.
---

# GitLab Commits provider — invariants

These rules are load-bearing. Violating any of them breaks the design.

## 1. One commit per apply per resource

A single `gitlabcommits_files` resource owns a `map(path → file)` on **one branch** of **one project**. Every `terraform apply` produces **exactly one** GitLab commit per resource — even when many files changed. Multi-branch / multi-project setups use `for_each`, not multiple writes from one resource.

If a change would make `Update` issue more than one commit, redesign.

## 2. `project_id` and `branch` are `RequiresReplace`

Any change to either forces a recreate. Composite ID is `<project_id>::<branch>`. Do not "fix" this to update-in-place — it's deliberate.

## 3. Action-diffing, not file-diffing

`Update` calls `diffActions` to emit the **minimum** set of GitLab `CommitActionOptions`:

| Plan vs state | Action |
|---|---|
| new path in plan | `create` (or `update` if `adopt_existing` and path already exists in repo) |
| gone path in state | `delete` |
| content blob changed | `update` |
| `execute_filemode` flipped | separate `chmod` action |
| nothing changed | **no commit** — Update returns early, preserving computed fields |

`Delete` is symmetric: one commit removing every managed file, skipping paths already absent. `delete_on_destroy = false` makes destroy a state-only drop.

`Create` rewrites `create` → `update` per-path when `adopt_existing = true` (default).

## 4. Drift detection without re-downloading

`Read` probes each managed file via `RepositoryFiles.GetFileMetaData` (HEAD-style, no body), fanned out at `refreshParallelism` through an `errgroup`. Blob IDs are computed locally with `gitBlobSHA(content) = sha1("blob <size>\0<content>")` — the exact form git uses. Only when the blob actually drifted does Read pull the full payload via `GetFile`.

`detect_drift = false` makes Read a no-op.

## 5. SHA-1 is intentional

`gitBlobSHA` uses SHA-1 because git does. `.golangci.yml` excludes `gosec` G401/G505 narrowly for this reason. **Do not** switch to SHA-256 — it would break blob comparison.

## 6. Serial state mutation

Every goroutine in Read writes only into its own slot in a pre-sized `[]fileRefreshResult`. A second pass over the slice mutates `state.Files` in path order — single-threaded. **No locks. No map writes from goroutines.** New parallel code (e.g. parallel adopt-existing probes in Update) must follow the same pattern.

## 7. Optimistic locking

`optimistic_lock = true` (default) sends each action's `last_commit_id`. GitLab rejects with HTTP 400 if anything else touched the file. `apiErrorDiag` converts that 400 into a "Concurrent modification detected" diagnostic. After every successful commit, `stampBlobs` refreshes both `blob_id` and `last_commit_id`.

## 8. Validate at the boundary

Provider config and GitLab API responses get validated. Internal helpers don't. Custom validators live in `schema_validators.go`. Static defaults live in `schema_defaults.go`. The `files` map has a `mapNonEmpty()` validator — an empty map at update time would mean "delete everything," which is almost never the intent; use `terraform destroy` for that.

## 9. Schema changes require doc regen

CI fails if `docs/` is out of sync. Any schema change → `just docs` → commit the diff.

## 10. Acceptance tests are namespaced

All acceptance-test paths live under `tf-acc-test/` (see `accTestPathPrefix`) so they're easy to spot and clean up after a stuck run.

## 11. Single shared client

`provider.go` builds a single `*gitlab.Client` and shares it via `ResourceData` / `DataSourceData`. Retry behavior (`max_retries`, `retry_wait_min_ms`, `retry_wait_max_ms`) is configured at client construction. Authentication is `GITLAB_TOKEN` env var or the `token` block attribute (`Sensitive: true`).
