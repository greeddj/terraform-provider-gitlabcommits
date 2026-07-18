# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Common commands

The canonical entry point is `just`; each target is a thin wrapper over a Go/Terraform invocation in the `Justfile`.

```bash
just build       # check + lint + test, then CGO-disabled static build into ./dist (version from `git describe`, falls back to 0.0.0-dev when no tags exist)
just test        # unit tests (go test ./...)
just lint        # golangci-lint over the whole module
just check       # go vet + staticcheck + govulncheck + fieldalignment
just docs        # regenerate docs/ via tfplugindocs (go generate ./...)
just docs-check  # regenerate docs and fail if anything changed
just headers     # apply copywrite license headers
just deps        # go mod tidy
just fix         # go fix + fieldalignment -fix
```

Run a single test:

```bash
go test ./internal/provider -run TestSomething -v
```

Acceptance tests hit a real GitLab project and are gated behind env vars (skipped otherwise):

```bash
TF_ACC=1 \
GITLAB_TOKEN=<api scope; see README Authentication> \
GITLAB_TEST_PROJECT_ID=<group/project or numeric id> \
GITLAB_TEST_BRANCH=tf-acc-test \      # optional; defaults to "tf-acc-test"; must pre-exist unless GITLAB_TEST_BRANCH_FROM is set
GITLAB_TEST_BRANCH_FROM=main \        # optional; materialise the branch from this ref, delete it after the test
GITLAB_BASE_URL=https://gitlab.example.com \  # optional, for self-hosted
go test -v ./internal/provider -run TestAccFiles_basic
```

`just ci` is the exact aggregate CI runs locally (`check` + `lint` + `test` + `check-tf-fmt` + `check-examples` + `docs-check` + `headers-check` + `check-deps`). Dependencies are resolved from the module cache (no `vendor/`); CI fails if `go.mod`, `go.sum`, `docs/`, or copywrite headers are out of sync.

## Architecture

This is a **Terraform Plugin Framework** provider (not SDKv2). Generated docs live under `docs/`, sourced from schema descriptions plus example HCL in `examples/`.

Minimum supported GitLab version: **18.x**.

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

`Read` probes each managed file with `RepositoryFiles.GetFileMetaData` (HEAD-style, no body), fanned out at `refreshParallelism` through an `errgroup`. The provider treats GitLab's returned `blob_id` as opaque — string-equal to what was stamped in state on the last commit. Only when the probe shows drift (`blob_id` or the exec bit differs from state) does Read pull the full payload via `GetFile` and decode it. Setting `detect_drift = false` makes Read a no-op (the resource becomes opaque after creation).

State mutation stays serial: every goroutine writes only into its own slot in a pre-sized `[]fileRefreshResult`, and a second pass over the slice deletes / updates `state.Files` in path order. No locks, no map writes from goroutines.

### Optimistic locking

`optimistic_lock = true` (default) sends each action's `last_commit_id` (the commit SHA we last observed touching the file). GitLab rejects the action with HTTP 400 if anything else has touched the file since. [`apiErrorDiag`](internal/provider/files_resource.go) inspects the response body to convert that 400 into a "Concurrent modification detected" diagnostic with actionable guidance, instead of a generic API error. After every successful commit, `stampBlobs` refreshes `blob_id` and `last_commit_id` via a parallel HEAD-style metadata fan-out (same `refreshParallelism` errgroup pattern as `Read`), probing at `Ref = commitSHA` (the commit just created, NOT branch HEAD) so a racing writer's values never land in state next to our content - the next locked apply still carries our commit SHA and GitLab rejects it with the concurrent-modification 400, while the next `Read` sees the racer's blob differ and surfaces the drift. Per-file `last_commit_id` comes from the probe response, so files the commit did not touch keep their older commit id. The commit SHA is only the fallback stamp when a probe fails - `blob_id` is then left null with a warning and the next `Read` repopulates it.

### Provider client

[`provider.go`](internal/provider/provider.go) builds a single `*gitlab.Client` and shares it via `ResourceData` / `DataSourceData`. Retry behaviour is configured at client construction (`max_retries`, `retry_wait_min_ms`, `retry_wait_max_ms`). Authentication is `GITLAB_TOKEN` env var or the `token` block attribute.

### Data sources

- [`gitlabcommits_file`](internal/provider/file_datasource.go) — read one file at a ref; returns content (text + base64), blob_id, last_commit_id, size, exec bit.
- [`gitlabcommits_branch_head`](internal/provider/branch_head_datasource.go) — branch HEAD SHA + protected flag, useful for bootstrapping `last_commit_id` flows externally.

### Validators and defaults

Custom schema helpers live in [schema_validators.go](internal/provider/schema_validators.go) (regex, non-empty, mutually-exclusive siblings, at-least-one-of file content, map-key regex). The Plugin Framework lacks built-in regex / mutual-exclusion validators, hence these. Defaults use the framework's built-in `booldefault.StaticBool`, declared inline in the schema - there are no local default helpers.

The `files` map has a `mapNonEmpty()` validator on purpose — an empty map at update time would translate to "delete everything", which is almost never what the user means. Use `terraform destroy` for that.

## Project conventions

- **Default to no comments.** Names should carry meaning. Comment only when the *why* is non-obvious — invariants, workarounds, or surprising behaviour. Existing comments tend to be why-comments; preserve them.
- **Validate at boundaries, trust internal code.** Provider config and API responses get validated; internal helpers don't get defensive checks.
- **No code for hypothetical future requirements.** When in doubt, do less.
- **Generated docs are committed.** Any schema change must be followed by `just docs`; CI fails on drift.
- **Acceptance test paths are namespaced** under `tf-acc-test/` (see `accTestPathPrefix`) so they're easy to spot and clean up after a stuck run.
- **Conventional commit titles** drive the goreleaser changelog (`feat:`, `fix:`, `docs:`, `chore:`, `test:`).

## Agents, skills, and commands (`.claude/`)

This repo ships a multi-agent workflow under [.claude/](.claude/). When you (Claude Code) are asked to do non-trivial work in this provider, **start with the architect** — do not jump straight to editing files.

### Roles

| Agent | Model | Owns | Never does |
| --- | --- | --- | --- |
| **architect** | opus | analysis, decisions, delegation, final review | writes code |
| **developer** | sonnet | code, why-comments, unit tests, self-verification | designs / decides scope |
| **tester** | sonnet | running tests, coverage gap analysis, classifying uncovered branches as needs-test / won't-test / dead-code | implements features |
| **security** | opus | CVE / attack-surface / interaction review across the change set and its neighbors | writes code, runs unrelated tests |
| **techwriter** | sonnet | comment audit (why-only rule), `just docs` regen, README/CLAUDE.md/MIGRATION.md updates | invents undocumented features |

### Standard loop (architect-led)

```text
main → architect (analyze, accept|reject)
       → developer (implement + self-verify)
       → tester (run + coverage gap report)
       → security (CVE + attack surface + interaction risks)
       → techwriter (comments + just docs + human docs)
       → architect (final summary)
       → main
```

The architect re-enters as many times as needed. "Looks fine" is never enough — only "the version a strict senior would ship" passes. The architect **must reject** plans that break invariants (one-commit-per-apply, HEAD-style drift probes, serial state mutation, `RequiresReplace` on `project_id`/`branch`, etc.) instead of writing the code anyway.

Two delegation modes are supported:

- **A**: architect calls subagents directly via the `Task` tool.
- **B**: architect emits dispatch instructions; main thread relays them to each subagent and feeds results back. Brain stays with the architect.

See [.claude/skills/multi-agent-workflow/SKILL.md](.claude/skills/multi-agent-workflow/SKILL.md) for the full loop, when to invoke each role, and the stop conditions.

### Slash commands

Justfile wrappers (each delegates to the matching `just` target):

- `/build`, `/test [pattern]`, `/lint`, `/check`, `/fix`, `/ci`
- `/docs`, `/docs-check`, `/tf-fmt [check]`, `/headers`, `/deps`

Not a Justfile wrapper:

- `/acc-test [pattern]` — runs `go test` directly, gated by `TF_ACC=1 GITLAB_TOKEN=... GITLAB_TEST_PROJECT_ID=...`

Workflow entry points:

- `/architect <task>` — hand off to the architect agent for analysis and orchestration.
- `/workflow <task>` — same, with explicit "run the full loop, no shortcuts" intent.

### Skills

Each loaded on demand when the relevant task starts:

- `gitlab-provider-conventions` — load before any change in `internal/provider/` (the load-bearing invariants).
- `terraform-framework-patterns` — load when touching Resource/DataSource/Provider plumbing.
- `go-performance` — load when writing or reviewing hot-path code (CRUD, goroutine fan-out, allocations).
- `acceptance-tests` — load before running or designing `TestAcc*`.
- `gitlab-api-docs` — load before verifying any GitLab REST API contract. Every `docs.gitlab.com/<path>/` page has an LLM-friendly twin at `<path>/index.md` (clean markdown, no HTML chrome); use that via WebFetch.
- `multi-agent-workflow` — load before kicking off any cross-role task.
