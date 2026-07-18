---
name: gitlab-provider-conventions
description: Repo-specific invariants for this Terraform GitLab provider - one-commit-per-apply, action-diffing, HEAD-style drift probes with opaque server-returned blob_id, serial state mutation, optimistic locking. Load before designing or editing internal/provider/ code, especially files_resource.go.
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
| content changed (bytewise, plan vs state) | `update` |
| `execute_filemode` flipped | separate `chmod` action |
| nothing changed | **no commit** — Update returns early, preserving computed fields |

`Delete` is symmetric: one commit removing every managed file, skipping paths already absent. `delete_on_destroy = false` makes destroy a state-only drop.

`Create` rewrites `create` → `update` per-path when `adopt_existing = true` (default).

## 4. Drift detection without re-downloading

`Read` probes each managed file via `RepositoryFiles.GetFileMetaData` (HEAD-style, no body), fanned out at `refreshParallelism` through an `errgroup`. `blob_id` is an **opaque server-returned identifier**: the probe's value is string-compared against what was stamped in state after the last commit. It is never computed locally - GitLab may serve SHA-1 or SHA-256 object formats, and the schema documents the format as GitLab-specific. Only when the blob actually drifted does Read pull the full payload via `GetFile`.

`detect_drift = false` makes Read a no-op.

## 5. blob_id is opaque - never hash locally

There is no local blob hashing anywhere in this provider, and there must not be: a locally computed git-style SHA-1 would break on SHA-256 object-format repositories and would couple the provider to git internals GitLab does not guarantee. Drift comparison stays hash-algorithm-agnostic string equality on the server-returned `blob_id`. `.golangci.yml` carries no gosec G401/G505 exclusions; if one ever seems necessary, the change that needs it is wrong.

## 6. Serial state mutation

Every goroutine in Read writes only into its own slot in a pre-sized `[]fileRefreshResult`. A second pass over the slice mutates `state.Files` in path order — single-threaded. **No locks. No map writes from goroutines.** New parallel code (e.g. parallel adopt-existing probes in Update) must follow the same pattern.

## 7. Optimistic locking

`optimistic_lock = true` (default) sends each action's `last_commit_id`. GitLab rejects with HTTP 400 if anything else touched the file. `apiErrorDiag` converts that 400 into a "Concurrent modification detected" diagnostic. After every successful commit, `stampBlobs` refreshes both `blob_id` and `last_commit_id`.

## 8. Validate at the boundary

Provider config and GitLab API responses get validated. Internal helpers don't. Custom validators live in `schema_validators.go`. Defaults are the framework's built-in `booldefault.StaticBool`, declared inline in the schema - there is no local defaults helper file. The `files` map has a `mapNonEmpty()` validator - an empty map at update time would mean "delete everything," which is almost never the intent; use `terraform destroy` for that.

## 9. Schema changes require doc regen

CI fails if `docs/` is out of sync. Any schema change → `just docs` → commit the diff.

## 10. Acceptance tests are namespaced

All acceptance-test paths live under `tf-acc-test/` (see `accTestPathPrefix`) so they're easy to spot and clean up after a stuck run.

## 11. Single shared client

`provider.go` builds a single `*gitlab.Client` and shares it via `ResourceData` / `DataSourceData`. Retry behavior (`max_retries`, `retry_wait_min_ms`, `retry_wait_max_ms`) is configured at client construction. Authentication is `GITLAB_TOKEN` env var or the `token` block attribute (`Sensitive: true`).
