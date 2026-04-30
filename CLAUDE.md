# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Common commands

The canonical entry point is `just`; each target is a thin wrapper over a Go/Terraform invocation in the `Justfile`.

```bash
just build       # CGO-disabled static build into ./dist (version stamped from `git describe`)
just test        # unit tests (go test ./...)
just lint        # go vet + staticcheck + golangci-lint
just check       # check-tf-fmt + check-vet + check-staticcheck + check-govulncheck + check-fieldalignment
just docs        # regenerate docs/ via tfplugindocs (go generate ./...)
just docs-check  # regenerate docs and fail if anything changed
just headers     # apply copywrite license headers
just deps        # go mod tidy && go mod vendor
just fix         # go fix + fieldalignment -fix
```

Run a single test:

```bash
go test ./internal/provider -run TestSomething -v
```

Acceptance tests hit a real GitLab project and are gated behind env vars (skipped otherwise):

```bash
TF_ACC=1 \
GITLAB_TOKEN=<api scope, write_repository> \
GITLAB_TEST_PROJECT_ID=<group/project or numeric id> \
GITLAB_TEST_BRANCH=tf-acc-test \      # optional; defaults to "tf-acc-test"
GITLAB_BASE_URL=https://gitlab.example.com \  # optional, for self-hosted
go test -v ./internal/provider -run TestAccFiles_basic
```

`just ci` is the exact aggregate CI runs locally (`check` + `docs-check` + `headers-check`). Vendoring is mandatory — CI fails if `vendor/`, `go.mod`, `go.sum`, `docs/`, or copywrite headers are out of sync.

## Architecture

This is a **Terraform Plugin Framework** provider (not SDKv2). Generated docs live under `docs/`, sourced from schema descriptions plus example HCL in `examples/`.

### The one-resource model

There is exactly one resource: [`gitlabcommits_files`](internal/provider/files_resource.go). A single instance owns a `map(path → file)` on **one branch** of **one project**. Both `project_id` and `branch` are `RequiresReplace`, so any change to either forces a recreate. Composite ID is `<project_id>::<branch>`.

The whole point of the provider is **one commit per `terraform apply` per resource**, regardless of how many files changed. Multi-branch / multi-project setups are expressed via `for_each`, not by stretching a single resource.

### CRUD lifecycle is action-diffing, not file-diffing

`Update` does not push every file. It calls [`diffActions`](internal/provider/files_resource.go) which walks plan vs state and emits the **minimum** set of GitLab `CommitActionOptions`:

- new path in plan → `create` (or `update` if `adopt_existing` and the path already exists in repo)
- gone path in state → `delete`
- content blob changed → `update`
- `execute_filemode` flipped → separate `chmod` action
- nothing changed → **no commit is produced** (Update returns early, preserving computed fields)

`Delete` symmetric: one commit removing every managed file, skipping paths already absent. `delete_on_destroy = false` makes destroy a state-only drop.

`Create` rewrites `create` → `update` per-path when `adopt_existing = true` (default), so `terraform import` followed by `apply` converges without surgery.

### Drift detection without re-downloading content

`Read` compares each managed file's stored `blob_id` against the remote one returned by `RepositoryFiles.GetFile`. The provider computes blob IDs locally with [`gitBlobSHA`](internal/provider/files_resource.go) — `sha1("blob <size>\0<content>")`, the exact form git uses — so the comparison is byte-for-byte exact and free. Only when blobs differ does Read decode the remote payload to update state. Setting `detect_drift = false` makes Read a no-op (the resource becomes opaque after creation).

The deliberate SHA-1 use is why `gosec` G401/G505 are excluded in [.golangci.yml](.golangci.yml).

### Optimistic locking

`optimistic_lock = true` (default) sends each action's `last_commit_id` (the commit SHA we last observed touching the file). GitLab rejects the action with HTTP 400 if anything else has touched the file since. [`apiErrorDiag`](internal/provider/files_resource.go) inspects the response body to convert that 400 into a "Concurrent modification detected" diagnostic with actionable guidance, instead of a generic API error. After every successful commit, [`stampBlobs`](internal/provider/files_resource.go) refreshes both `blob_id` and `last_commit_id` for every entry in the files map.

### Provider client

[`provider.go`](internal/provider/provider.go) builds a single `*gitlab.Client` and shares it via `ResourceData` / `DataSourceData`. Retry behaviour is configured at client construction (`max_retries`, `retry_wait_min_ms`, `retry_wait_max_ms`). Authentication is `GITLAB_TOKEN` env var or the `token` block attribute.

### Data sources

- [`gitlabcommits_file`](internal/provider/file_datasource.go) — read one file at a ref; returns content (text + base64), blob_id, last_commit_id, size, exec bit.
- [`gitlabcommits_branch_head`](internal/provider/branch_head_datasource.go) — branch HEAD SHA + protected flag, useful for bootstrapping `last_commit_id` flows externally.

### Validators and defaults

Custom schema helpers live in [schema_validators.go](internal/provider/schema_validators.go) (regex, non-empty, mutually-exclusive siblings, map-key regex) and [schema_defaults.go](internal/provider/schema_defaults.go) (static bool defaults). The Plugin Framework lacks built-in regex / mutual-exclusion validators, hence these.

The `files` map has a `mapNonEmpty()` validator on purpose — an empty map at update time would translate to "delete everything", which is almost never what the user means. Use `terraform destroy` for that.

## Project conventions

- **Default to no comments.** Names should carry meaning. Comment only when the *why* is non-obvious — invariants, workarounds, or surprising behaviour. Existing comments tend to be why-comments; preserve them.
- **Validate at boundaries, trust internal code.** Provider config and API responses get validated; internal helpers don't get defensive checks.
- **No code for hypothetical future requirements.** When in doubt, do less.
- **Generated docs are committed.** Any schema change must be followed by `just docs`; CI fails on drift.
- **Acceptance test paths are namespaced** under `tf-acc-test/` (see `accTestPathPrefix`) so they're easy to spot and clean up after a stuck run.
- **Conventional commit titles** drive the goreleaser changelog (`feat:`, `fix:`, `docs:`, `chore:`, `test:`).
