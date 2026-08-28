# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Terraform provider (`greeddj/gitlabcommits`) built on the **Terraform Plugin Framework** (protocol 6, not SDKv2). It manages a bundle of files on one branch of one GitLab project and batches every change into **one commit per `terraform apply` per resource**. All Go code lives in the single package `internal/provider/`; `main.go` only serves it. GitLab access goes through `gitlab.com/gitlab-org/api/client-go`. Minimum supported GitLab is 18.x.

## Commands

`just` is the entry point; every target is a thin wrapper over a Go/Terraform invocation (see `Justfile`). The analysis tools (`staticcheck`, `govulncheck`, `fieldalignment`, `tfplugindocs`, `copywrite`) are declared in the `tool` block of `go.mod` and run via `go tool ...`, so there is nothing to install. `golangci-lint` is the one external binary (CI pins v2.13.1; `.golangci.yml` enables an explicit linter list with `default: none`, so a new golangci-lint release cannot quietly add linters).

```bash
just ci              # the exact gate CI runs: check + lint + test + check-tf-fmt + check-examples + docs-check + headers-check + check-deps
just check           # go vet + staticcheck + govulncheck + fieldalignment
just lint            # golangci-lint run ./...
just test            # go test -race ./...   (CI adds a coverage profile)
just build           # check + lint + test, then a CGO_ENABLED=0 static binary in ./dist (version from `git describe --tags`, falls back to 0.0.0-dev)
just docs            # go generate ./... -> tfplugindocs regenerates docs/
just docs-check      # regenerate and fail if docs/ is dirty
just fix             # go fix + fieldalignment -fix
just headers         # apply copywrite license headers (headers-check verifies)
just tf-fmt          # terraform fmt -recursive examples/ (check-tf-fmt verifies)
just check-examples  # scripts/validate-examples.sh: builds the provider, then terraform validate over examples/{complete,for_each,provider} via dev_overrides
just deps            # go mod tidy (check-deps runs go mod tidy -diff)
```

Single test (everything is in one package):

```bash
go test ./internal/provider -run 'TestDiffActions' -v
go test -race ./internal/provider -run 'TestRead_' -v   # use -race for anything touching the goroutine fan-outs
```

### Acceptance tests

`TestAcc*` in `files_resource_acceptance_test.go` run against a real GitLab project. They `t.Skip` without `TF_ACC` and `t.Fatal` when `TF_ACC` is set but the token or project is missing, so plain `just test` stays green.

```bash
export TF_ACC=1
export GITLAB_TOKEN='...'                  # api scope (see README Authentication)
export GITLAB_TEST_PROJECT_ID='you/sandbox' # plain path (not URL-encoded) or numeric ID
export GITLAB_TEST_BRANCH='tf-acc-test'    # optional; this is the default; must pre-exist unless GITLAB_TEST_BRANCH_FROM is set
export GITLAB_TEST_BRANCH_FROM='main'      # optional; tests add create_branch_from and delete the branch afterwards
export GITLAB_BASE_URL='https://gitlab.example.com'  # optional, self-hosted
go test -v -timeout=20m -run '^TestAcc' ./internal/...
```

Every acceptance file path lives under `tf-acc-test/` (`accTestPathPrefix`) so a stuck run is easy to clean up. In CI (`.github/workflows/acceptance.yml`) they run on manual dispatch, a nightly cron, and pushes to `main` touching `internal/**`, on a unique per-attempt branch `tf-acc-test-<run_id>-<run_attempt>` created from `main`; an `if: always()` step deletes it over the API even when the job times out or is cancelled.

## Architecture

### Files

- `provider.go` - provider schema and `Configure`; builds one `*gitlab.Client`, hands it to data sources through `DataSourceData`, and to resources through `ResourceData` as `*resourceDeps` (client + that configuration's `branchLocks` + whether the commit request may be retried).
- `files_resource.go` - the only resource, `gitlabcommits_files`: schema, CRUD, import, `diffActions`, `stampBlobs`, `apiErrorDiag`, `decodeRemoteContent`.
- `file_datasource.go`, `branch_head_datasource.go` - `gitlabcommits_file` (one file at a ref) and `gitlabcommits_branch_head` (head SHA + protected flag).
- `schema_validators.go` - validators the framework lacks: regex, non-empty, sibling mutual exclusion, exactly-one-of file content, map-key repository path check.

### Provider client (`provider.go`)

The token comes from the `token` attribute or `GITLAB_TOKEN`; the base URL from `base_url` or `GITLAB_BASE_URL`. Retries (`max_retries` default 5, `retry_wait_min_ms` / `retry_wait_max_ms` 1000 / 30000) are configured at client construction; an unknown value at configure time is an error, never a silent default. client-go's default retry check replays any request on 5xx regardless of method, so `POST /repository/commits` carries its own per-request policy (`commitRetryPolicy`, attached by `commitRequestOptions`): retry only on 429 and on DNS / dial failures that happen before the request is sent, never on 5xx or a broken connection, because a replay after GitLab already landed the commit would be a second commit for one apply. `max_retries = 0` disables that policy too (`resourceDeps.retryCommits`), since a per-request check would otherwise bypass `WithoutRetries`. The retry wait bounds only shape the 429 backoff; client-go uses a fixed 700-900 ms schedule for 5xx. `crossHostRedirectGuard` is installed as the HTTP client's `CheckRedirect`: net/http does not strip the `Private-Token` header on cross-host redirects, so off-host redirects and https->http downgrades are refused.

### Per-branch commit serialisation

Terraform applies resource instances concurrently (`-parallelism`, default 10), and the documented `for_each` layout puts many resources on one branch. Two `CreateCommit` calls racing on the same branch tip make Gitaly reject the loser with HTTP 400 "reference update: reference does not point to expected object". `branchLocks` (one per provider configuration, created in `Configure` and shared through `resourceDeps`; Terraform runs each provider block in its own process, so two aliased provider blocks are not serialised against each other) serialises commits per `(project_id, branch)`: Create holds the lock from `branchExists` through `CreateCommit` (two instances materialising the same branch must see each other's work) and releases it before `stampBlobs`, which probes the immutable commit; Update and Delete hold it only around `CreateCommit`. `acquire` is channel-based so it honours ctx cancellation, and the release it returns is idempotent (`sync.OnceFunc`), so every call site defers it and also calls it right after the commit. The key is the verbatim `project_id` string, so resources sharing a branch must spell it the same way (numeric ID and path are different keys), and only the target branch is locked, not the `create_branch_from` source. The lock changes ordering, never the number of commits. Writers outside the process (another pipeline, a manual push, an aliased provider block) are not covered by design; `apiErrorDiag` recognises the Gitaly text and says so.

### The one-resource model

`gitlabcommits_files` owns `map(path -> file)` on one branch of one project. `project_id` and `branch` are `RequiresReplace`; the ID is `<project_id>::<branch>`. Multi-branch or multi-project setups are expressed with `for_each` over resources, never by widening a single resource. An apply that changes anything produces exactly one commit; an apply that changes nothing produces none. This is the load-bearing invariant of the whole provider.

### CRUD as action-diffing

- **Create** checks the branch (`branchExists`). If it is missing, `missingBranchPreflight` distinguishes an empty repository (no ref to branch from) from a merely missing `create_branch_from`. With `adopt_existing` (default true) it probes each path with `GetFileMetaData` - against the branch, or against the `create_branch_from` ref when the branch does not exist yet - and `adoptAwareActions` rewrites a `create` for an existing path into an `update`, plus a companion `chmod` when the exec bit differs (the commits API honours `execute_filemode` only on create and chmod). A missing branch is materialised by `start_branch` on the commit itself (`start_sha` when `create_branch_from` is a 40/64-hex commit SHA; tags are not supported), never by a separate `CreateBranch` call: GitLab creates the branch and the commit in one operation, so a server-side rejection (push rule, pre-receive hook) leaves no orphaned empty branch and CI sees one push event; the diagnostic for that failure names the branch and the `create_branch_from` ref.
- **Read** is a no-op when `detect_drift = false`. Otherwise it fans out `GetFileMetaData` (HEAD-style, no body) at `refreshParallelism` (16) through an `errgroup`, compares `blob_id` and the exec bit with state, and pulls full content with `GetFile` only for files that actually drifted. A 404 drops the file from state; if *every* file 404s it checks the branch and, when that is gone too, removes the whole resource so the next apply can recreate it. Goroutines write only to their own slot of a pre-sized `[]fileRefreshResult`; a serial second pass mutates `state.Files` in path order. Whichever of `content` / `content_base64` the user chose is preserved on drift; binary drift into `content` is an error because cty would silently corrupt the bytes.
- **Update** calls `diffActions`: new path -> create or adopt-update, content changed (`contentChanged`: cheap string compare first, bytewise fallback only for differing base64 or a form switch) -> `update`, exec bit flipped -> `chmod`, path gone from the plan -> `delete`. Zero actions means no commit; computed fields are carried over from state.
- **Delete** with `delete_on_destroy` (default true) probes every path and emits one commit deleting the ones that exist. A probe *error* (anything but a 404) fails the destroy; it must never be treated as "absent".
- **Import** takes `<project_id>::<branch>`; the files map starts empty and the next apply converges through adoption.

### Optimistic locking and blob stamping

With `optimistic_lock` (default true) every update / delete / chmod action carries the file's `last_commit_id` from state (or from the probe, for adopt-updates), and GitLab rejects it with HTTP 400 if someone else touched the file. `apiErrorDiag` recognises that 400/409 body (`last_commit_id` / `last commit` / `has changed since`) and turns it into a "Concurrent modification detected" diagnostic, and the Gitaly ref-race body (`expected object`) into "Branch changed while the commit was being created" (a writer outside this terraform run); it also maps 401 / 403 / 404 / 413 / 429 to actionable text and truncates bodies to `maxDiagBodyChars`. client-go collapses every 404 into the bare `gitlab.ErrNotFound` sentinel (no `*ErrorResponse` survives), so 404 must be matched with `errors.Is`, never by status code.

After every successful commit `stampBlobs` refreshes `blob_id` and `last_commit_id` through a parallel metadata fan-out probing at `Ref = commitSHA` - the commit just created, **not** branch HEAD - so a racing writer's values can never be stamped next to our content. Per-file `last_commit_id` comes from the probe, so files the commit did not touch keep their older id and do not trip false lock conflicts. A probe failure, or a `blob_id` longer than `maxBlobIDLen` (256, treated as hostile), leaves `blob_id` null with a warning; the next Read repopulates it.

### Validators and schema conventions

Custom validators live in `schema_validators.go`; defaults use the framework's `booldefault.StaticBool` inline in the schema. `files` carries `mapNonEmpty()` on purpose: `files = {}` would mean "delete everything" on update, which is what `terraform destroy` is for. `mapKeysValidRepoPath` uses explicit segment logic instead of a character regex so legal git names like `..config` stay usable; only `.` / `..` segments, empty segments, NUL bytes and leading whitespace are rejected, the rest is GitLab's to validate. `content` / `content_base64` are exactly-one-of via `stringConflictsWithSibling` plus `objectFileContentRequired`, and neither is `Sensitive` by design (see SECURITY.md).

### Hardening guards worth keeping

Nil guards exist for a 2xx JSON-null body from `CreateCommit` (nil `*Commit`), `GetFile` (nil `*File`) and `GetBranch` (nil `Commit`); `decodeRemoteContent` fails loudly on an unknown encoding. `hardening_test.go` pins each of these.

## Testing patterns

- Unit tests fake GitLab with `httptest.NewServer` and `gitlab.NewClient("tok", gitlab.WithBaseURL(srv.URL+"/"), gitlab.WithoutRetries())` (retries off so a faked 5xx does not stall behind the backoff schedule). CRUD is driven directly: build `tfsdk.State` / `tfsdk.Plan` from the resource schema and call `Read` / `Create` / `Update` / `Delete`. Reuse `runRead`, `readState`, `newReadClient`, `newTestResource` (`read_test.go`) and `runCreate`, `runUpdate`, `runDelete` (`crud_test.go`) rather than adding new harnesses. `newRetryingResource` builds the `max_retries > 0` wiring (retrying client with millisecond backoff plus `commitRetryPolicy`) for retry tests; the `run*On` variants drive a pre-built `filesResource`; `createRequest` / `updateRequest` build the request on the test goroutine so concurrency tests can call `Create` / `Update` from workers without `t.Fatalf` leaving the test goroutine.
- Acceptance tests use `terraform-plugin-testing` (`resource.Test` with `testAccProtoV6ProviderFactories`), render HCL with `accConfig`, and assert against the API with `accCheckFileExists` / `accCheckFileGone` / `accCheckFileExec`.

## Docs, examples, and release

- `docs/` is generated by `tfplugindocs` (`//go:generate` in `main.go`) from schema descriptions plus `examples/provider/provider.tf`, `examples/resources/gitlabcommits_files/{resource.tf,import.sh}` and `examples/data-sources/<name>/data-source.tf`; there is no `templates/` directory. Any schema or description change must be followed by `just docs` with the result committed; CI fails on drift.
- `examples/complete`, `examples/for_each` and `examples/provider` are full modules that CI runs `terraform validate` on against the locally built provider; keep them valid. New API surface needs an example there so the docs pick it up.
- License headers are enforced by `copywrite` (`.copywrite.hcl`; `examples/`, `.github/`, `docs/` are exempt).
- A `v*` tag triggers `.github/workflows/release.yml` -> goreleaser (GPG-signed, `main.version` set via ldflags). The changelog is built from conventional commit titles (`feat:`, `fix:`, `docs:`; `chore:` and `test:` are excluded); there is no CHANGELOG file. Dependabot PRs use `fix(deps)` / `fix(ci)`.
- `go.mod` is the single source of truth for the Go version (1.27); `.golangci.yml` mirrors it.

## Code conventions

- Default to no comments; names carry meaning. Comment only when the *why* is non-obvious (invariants, workarounds, surprising GitLab behaviour). The existing comments are all of that kind; keep them.
- Validate at boundaries (user config, API responses); trust internal code.
- Do not write code for hypothetical future requirements.
- New code in `internal/provider/` needs unit tests in the same package, table-driven where it helps.
- Conventional commit titles (`feat:`, `fix:`, `docs:`, `chore:`, `test:`) for anything user-visible.
