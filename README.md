# Terraform Provider: GitLab Commits

[![CI](https://github.com/greeddj/terraform-provider-gitlabcommits/actions/workflows/ci.yml/badge.svg)](https://github.com/greeddj/terraform-provider-gitlabcommits/actions/workflows/ci.yml)
[![Release](https://github.com/greeddj/terraform-provider-gitlabcommits/actions/workflows/release.yml/badge.svg)](https://github.com/greeddj/terraform-provider-gitlabcommits/actions/workflows/release.yml)
[![codecov](https://codecov.io/gh/greeddj/terraform-provider-gitlabcommits/graph/badge.svg)](https://codecov.io/gh/greeddj/terraform-provider-gitlabcommits)

A Terraform provider that manages **bundles of files** in a GitLab repository.
Every change a resource makes - create, update, delete or chmod - is batched
into **one commit per `terraform apply`**, so you get **one CI pipeline run per
resource** instead of one per file.

It exists to solve a very specific operational problem: a GitOps repository
holding huge fan-outs of nearly-identical YAML (helm values, ArgoCD
applications) for *N services x M environments*. With `gitlab_repository_file`
each file becomes its own commit, which is a non-starter at any reasonable scale.
This provider lets you express the bundle as Terraform and emit exactly one
commit per service.

> **Note:** This project was created in collaboration with Claude Code.

## How it actually works

A single resource (`gitlabcommits_files`) owns a `map(path -> file)` on one
branch of one project. The provider:

- **Create** - pushes one commit that creates every file. If `adopt_existing`
  is true (default) and a path already exists in the repo, the action is
  silently rewritten from `create` to `update`, so the apply does not fail.
  A path whose content and mode already match the plan needs no action, so
  an apply that only adopts identical files makes no commit and leaves
  `commit_sha` unset. A path that cannot be read for adoption fails the
  apply before anything is committed. When the branch does not exist yet and
  `create_branch_from` is set, the branch is created from that ref in the
  same operation as the commit (one push event); with nothing to commit it is
  created on its own. A branch name is resolved to its head commit first,
  and the files are compared with that commit and the branch created from
  it, even if the source branch moves in the meantime. In a repository with
  no commits yet, leave `create_branch_from` unset: the first commit creates
  the branch.
- **Read** - probes each managed file via a HEAD-style metadata call
  (`GetFileMetaData`) and compares the GitLab-returned `blob_id` and exec
  bit with state. Only when the blob has actually drifted does it pull the
  full content. Any drift updates state, so the next plan shows real
  differences against the repo. A file managed through `content` that
  drifts to bytes that are not valid UTF-8 is recorded in `content_base64`
  instead, with a warning: the next apply restores the configured text, and
  switching the file to `content_base64` keeps the new bytes.
- **Update** - diffs plan vs state and emits the **minimum** set of actions:
  gone paths -> `delete` (emitted first), new paths -> `create`, or nothing
  when the path already exists with identical content, content changed ->
  `update`, exec bit flipped -> `chmod`. A file is never created where the
  branch holds a directory (see Caveats). If nothing changed, no commit is
  produced. A delete or chmod that carries no lock token (for example with
  `optimistic_lock = false`) is probed first: a delete of a path that no
  longer holds a file is dropped, and a chmod of one fails. A delete of a
  path another resource took over in the same apply is dropped as well (see
  Caveats).
- **Delete** - pushes one commit that removes every managed file. With
  `optimistic_lock` the commit goes out without probing: GitLab checks each
  file's `last_commit_id` itself and rejects the commit when a file was
  changed or removed out of band. Only then are the files probed, the ones
  already gone dropped, and the commit retried once, so a destroy still lands
  at most one commit. A file without a lock token is probed before the commit.
  Files already absent are skipped (idempotent against external cleanup), and
  a destroy that finds none left makes no commit and says so in a warning.
  A file another resource created or adopted in the same apply is kept, with
  a warning (see Caveats). Disable with `delete_on_destroy = false`.

The composite ID is `<project_id>::<branch>`. Import with that format; the
files map starts empty and is reconciled on the next plan + apply.

## Requirements

- Terraform >= 1.5
- GitLab 19.x (what CI tests against); 18.x works, older versions may work
  for basic operations but are not supported
- A token that can call the GitLab REST API on the target project (see Authentication below)
- On macOS and Windows, `SSL_CERT_FILE` / `SSL_CERT_DIR`, when set, replace the
  system certificate store for the provider's TLS connections (Go 1.27 default);
  unset them if your GitLab's CA lives only in the system store
- Go >= 1.27.1 (development only)

## Provider configuration

```hcl
terraform {
  required_providers {
    gitlabcommits = {
      source = "greeddj/gitlabcommits"
      # Pin a version once you depend on released behaviour, e.g.:
      # version = "~> 0.1.0"
    }
  }
}

provider "gitlabcommits" {
  token    = var.gitlab_token              # or GITLAB_TOKEN
  base_url = "https://gitlab.example.com"  # optional; or GITLAB_BASE_URL
}
```

| Argument | Default | Description |
| --- | --- | --- |
| `token` | `GITLAB_TOKEN` | Token for the REST API; see Authentication. |
| `base_url` | `GITLAB_BASE_URL`, else `https://gitlab.com` | Base URL of a self-hosted instance. |
| `max_retries` | `5` | Retries on 429 and transient 5xx for read and probe requests. The commit request is retried only on 429 and on connection failures before it is sent (see Limits and retries). `0` disables retries. |
| `retry_wait_min_ms` | `1000` | Base wait between 429 retries; doubles per attempt and is extended by GitLab's `RateLimit-Reset` header. |
| `retry_wait_max_ms` | `30000` | Bound on the random jitter added to each 429 wait, not on the total wait. |

## Authentication

The provider needs a token that can call the GitLab REST API on the target
project. Supported token types:

- **Personal Access Token** with the `api` scope.
- **Project Access Token** (or **Group Access Token**) with the `api` scope.
  Recommended for CI/CD - scope a token to exactly the project(s) you manage.
- **Fine-grained Personal Access Token** (GitLab 19.2+) with these permissions
  on the target project (or its group): `Commit: Create`, `Repository: Read`,
  `Branch: Read`, plus `Branch: Create` when a resource sets
  `create_branch_from`. Required once your group or instance enforces
  fine-grained tokens: legacy `api` tokens are refused after the enforcement
  date.

The token's user (or the token itself, for Project/Group tokens) needs the
**Developer** role, or **Maintainer** to push to a protected branch.

Pass the token via the `GITLAB_TOKEN` environment variable or the `token`
attribute on the provider block. In CI, prefer a CI variable such as
`TF_VAR_gitlab_token` over committing the token.

> The `write_repository` scope is for Git-over-HTTP (push/pull) and does
> **not** authenticate REST API calls - of the legacy scopes only `api` does.
> `CI_JOB_TOKEN` is not supported: GitLab's job-token allowlist permits only
> GET on the Commits, Files and Branches APIs (fine-grained job token
> permissions add nothing beyond `READ_REPOSITORIES` there), while this
> provider needs `POST /repository/commits`.

## Resource: `gitlabcommits_files`

| Argument | Type | Required | Description |
| --- | --- | --- | --- |
| `project_id` | string | yes | Numeric project ID or plain project path (`group/subgroup/project`, not URL-encoded). Changing it, respelling it, or a value unknown at plan time forces replacement; see Caveats. |
| `branch` | string | yes | Target branch (must exist, or set `create_branch_from`; in a repository with no commits yet the first commit creates it). Changing it, or a value unknown at plan time, forces replacement; see Caveats. |
| `commit_message` | string | yes | Used for any commit produced (create / update / destroy). |
| `author_name` | string | no | Override commit author name. |
| `author_email` | string | no | Override commit author email. |
| `create_branch_from` | string | no | If set and `branch` does not yet exist, create it from this branch name or full commit SHA (typically `main`; tags are not supported) together with the first commit, or on its own when there is nothing to commit. A branch name is resolved to its head commit once, when the resource is created. Must be unset in a repository with no commits yet. The branch is not deleted on destroy. |
| `detect_drift` | bool | no | Default `true`. If false, Read is a no-op. A refresh reads the value from state, so a new value affects refreshes only once an apply has recorded it; see Caveats. |
| `delete_on_destroy` | bool | no | Default `true`. If false, destroy only drops state. Read from the state of the last apply; see Caveats. |
| `adopt_existing` | bool | no | Default `true`. Rewrite `create` to `update` for paths that already exist, or to no action when their content already matches (needed for clean import). A path that cannot be read fails the apply without a commit. |
| `optimistic_lock` | bool | no | Default `true`. Send each file's `last_commit_id` so GitLab rejects concurrent updates with HTTP 400. Set to `false` to opt out. Not sent on the first commit of a branch created from `create_branch_from`. For the destroy commit, read from the state of the last apply. |
| `files` | map of object | yes | See below. Must not be empty: `files = {}` would mean "delete everything", which is what `terraform destroy` is for. |
| `id` | string | computed | Composite identifier `<project_id>::<branch>`. |
| `commit_sha` | string | computed | SHA of the most recent commit produced by this resource. |

### `files` entry

| Field | Type | Computed | Notes |
| --- | --- | --- | --- |
| `content` | string | no | text content; mutually exclusive with `content_base64`; drift to bytes that are not valid UTF-8 is recorded in `content_base64` (see Read above) |
| `content_base64` | string | no | base64-encoded content (use for binaries); mutually exclusive with `content` |
| `execute_filemode` | bool | no | default `false`; toggling triggers a `chmod` action |
| `blob_id` | string | yes | opaque blob identifier returned by GitLab; used for drift detection (git SHA-1 today, possibly SHA-256 on SHA-256 repos) |
| `last_commit_id` | string | yes | SHA of the last commit through which this resource touched the file; sent on update / delete when `optimistic_lock = true` |

## Data sources

- `gitlabcommits_file` reads one file at a branch, tag or commit SHA. Inputs:
  `project_id`, `branch`, `file_path`. Outputs: `content` (null when the file
  is not valid UTF-8), `content_base64` (always set), `blob_id`,
  `last_commit_id`, `execute_filemode`, `size`. Useful for comparing rendered
  HCL with what is committed.
- `gitlabcommits_branch_head` returns `commit_sha` and `protected` for a
  branch, e.g. to wire downstream pipelines to the exact SHA terraform saw.

Examples live in [`examples/data-sources/`](examples/data-sources/); the full
attribute reference for the provider, the resource and both data sources is
the generated [`docs/`](docs/).

## Typical layout: 20 services x 30 environments

```hcl
resource "gitlabcommits_files" "service" {
  for_each = var.services

  project_id     = "platform/gitops"
  branch         = "main"
  commit_message = "chore(${each.key}): sync gitops manifests via terraform"

  files = merge(
    {
      for env_name, env in var.environments :
      "services/${each.key}/values/${env_name}.yaml" => {
        content = yamlencode({
          image        = { repository = each.value.image_repo, tag = each.value.image_tag }
          replicaCount = env.replicas
          ingress      = { enabled = true, host = "${each.key}.${env.domain}" }
        })
      }
    },
    {
      for env_name, env in var.environments :
      "services/${each.key}/argocd/${env_name}.yaml" => {
        content = yamlencode({
          apiVersion = "argoproj.io/v1alpha1"
          kind       = "Application"
          metadata   = { name = "${each.key}-${env_name}", namespace = "argocd" }
          spec = {
            source      = { path = "services/${each.key}/chart", helm = { valueFiles = ["../values/${env_name}.yaml"] } }
            destination = { server = env.cluster, namespace = env.namespace }
            syncPolicy  = { automated = { prune = true, selfHeal = true } }
          }
        })
      }
    },
  )
}
```

20 resources -> 20 commits per apply -> 20 pipeline runs. Not 600.

A complete example lives in [`examples/for_each/main.tf`](examples/for_each/main.tf).

## Import

```bash
terraform import 'gitlabcommits_files.service["frontend"]' 'platform/gitops::main'
```

Import checks that the branch exists. After import the files map is empty in
state. The next plan will produce `create` actions for every file; with
`adopt_existing = true` (default) those that already exist in the repo are
compared with the plan: identical content needs no action, differing content
becomes an `update`. A configuration that matches the repository therefore
converges without a commit.

## Caveats worth knowing

- **One resource = one branch = one project**. To target multiple branches or
  repos, use multiple resources (typically via `for_each`).
- **Many resources on one branch are safe within one apply.** Terraform runs
  resources in parallel, and concurrent commits to the same branch would race
  on its tip (GitLab rejects the loser with HTTP 400 "reference does not point
  to expected object"). The provider serialises its commits per branch within
  one provider configuration, so the `for_each` layout above never hits that;
  each resource still lands exactly one commit. Two things stay outside that
  guarantee: resources sharing a branch must spell `project_id` the same way
  (a numeric ID and a path are different lock keys; changing the spelling of
  an existing resource is a replacement, see below), and a second provider
  block (alias) runs in its own process. Any other writer (another pipeline,
  a manual push, an aliased provider) is reported as "Branch changed while
  the commit was being created" and you re-run the apply; if that repeats on
  every run, look for a tag with the branch's name (see Limits).
- **Within one apply, a file handed from one resource to another is kept.**
  Moving a file from one resource's `files` to another's on the same branch,
  renaming a resource address without a `moved` block, and a replacement
  under `create_before_destroy` all make one resource delete a path that
  another creates or adopts in the same apply, and Terraform does not order
  the delete before the adoption. The provider remembers, in memory and for
  the length of the run, every path a resource creates or adopts (all of a
  new resource's files, the files an update adds). Another resource's delete
  of such a path is dropped with a warning naming it, and a resource that
  takes a path over looks at the repository only after any delete commit
  already in flight on the branch has landed. Either way the file ends up on
  the branch and in the state of the resource that now manages it; the
  one-commit-per-resource rule is unchanged.
  - A claim under another spelling of the same project (numeric ID vs path),
    as a respelled replacement makes, is recognised too. When a delete meets
    a claim under another spelling, two numeric IDs are compared as numbers
    and each path spelling is looked up once per run; that also happens for
    another project that shares the branch name and the path (the same
    `.gitlab-ci.yml` on `main` in several projects) when either is spelled
    by path. If a lookup fails, the operation fails and commits nothing, and
    the error says how to go on: when both spellings name the same project,
    simply applying again would delete the file. The wait for a delete in
    flight comes from the branch lock, so it still needs the same spelling.
  - Not covered: other runs, and a second provider block (alias), which is
    another process. So move a file in one apply: adding it to the new
    resource in one apply and removing it from the old one in a later apply
    makes the later apply delete it, and the new resource finds it missing
    on its next refresh. A re-run of a failed apply is another run too: when
    a resource left a file in place for another one and then failed (its
    lookup or its commit), applying again retries that operation, which
    then deletes the file. To keep it, remove the failed resource from state
    (`terraform state rm <address>`, which also drops a replaced object
    still waiting for its destroy and leaves every file in place), import
    it again if the configuration still declares it, and apply.
- **Changing `branch` or `project_id` replaces the resource**, and so do
  respelling the same project (a numeric ID vs a path),
  `terraform apply -replace`, `terraform taint` and `replace_triggered_by`.
  A value that is unknown at plan time counts as a change: when `project_id`
  or `branch` comes from a data source read during apply (a data source or
  module with `depends_on`) or from an attribute of a resource being
  replaced, Terraform plans `(known after apply) # forces replacement` even
  if the final value is the same. Keep both known at plan time (literals,
  variables, data sources without `depends_on`); if they cannot be, an
  applied `delete_on_destroy = false` makes such a replacement commit-free,
  since the destroy then leaves the files and the create adopts them.
  - Without `create_before_destroy`, replacement is destroy-then-create: with
    the default `delete_on_destroy = true` the destroy pushes a commit
    deleting every managed file from the OLD target before the files are
    created on the new one. On the same branch that is two commits with none
    of the files in between, which a push-triggered pipeline or an ArgoCD
    sync with `prune` can act on.
  - Under `create_before_destroy` (set on this resource, or imposed by
    Terraform when a resource that depends on it has it) the new object is
    created first. On another branch or project the old files are then
    deleted; on the same branch and project the new object adopts them
    (without a commit when they already match) and the old object's destroy
    leaves them in place.
  - To re-point without a delete commit, set `delete_on_destroy = false` and
    apply before changing the target, or hand the files over: drop the old
    instance from state without destroying it, with a
    `removed { from = <address> lifecycle { destroy = false } }` block
    (Terraform 1.7+) or `terraform state rm <address>`, then declare the
    resource for the new target; its first apply adopts matching files
    without a commit. Renaming the resource address alone does not help:
    without a `moved` block the old address is destroyed, and with one a
    changed target is still a replacement. For a pure address refactor
    (a renamed resource or `for_each` key) use
    `moved { from = <old address> to = <new address> }`.
  - Replacing is never needed to re-push files: with `detect_drift = true`
    (default) a plain `terraform apply` already restores drifted files.
- **`delete_on_destroy` and `optimistic_lock` apply as last applied.**
  Terraform does not evaluate configuration during `terraform destroy`, so the
  destroy commit uses the values recorded in state by the last apply. Change
  the flag in HCL, run `terraform apply`, then destroy.
- **`detect_drift` applies as last applied, too.** With
  `detect_drift = false` a refresh leaves state as the last apply left it.
  A refresh is handed the state, not the configuration, so it reads the
  recorded value, and an apply that fails keeps that value: turning
  `detect_drift` back on takes effect only once an apply has recorded it.
  Until then, under `optimistic_lock` an update, chmod or delete of a file
  changed out of band fails with GitLab's 400 on every apply and every
  destroy, and `terraform apply -refresh-only` changes nothing. A file
  deleted out of band stays in state; an update that removes it from
  `files` probes the path and drops the delete, and a destroy skips it. To
  catch up with the repository, set `detect_drift = true` with `files` as
  last applied and apply (this makes no commit), then put back any change
  you were applying and plan again: that plan compares `files` with the
  branch, so with `files` still as last applied it would revert what
  changed there. A failed commit's diagnostic gives this advice when it
  applies.
- **State holds your file content.** If you set `content_base64` to the bytes
  of a 10 MB binary, those bytes live in `terraform.tfstate`. Use a secrets
  backend and avoid committing huge binaries through this provider.
- **Optimistic locking is on by default.** Each update / delete action sends
  the file's `last_commit_id` so GitLab rejects the action with HTTP 400 if
  someone else has touched the file since this resource last did. The
  provider surfaces those as "Concurrent modification detected" diagnostics
  with a hint to run `terraform apply -refresh-only` (with
  `detect_drift = false` recorded in state that refresh changes nothing,
  and the diagnostic says what to do instead; see above). Set
  `optimistic_lock = false` per resource to opt out (e.g. when an external
  bot intentionally co-edits the same files); the trade-off is silent
  last-write-wins. Without the token the provider probes each path before a
  delete or chmod, because GitLab would apply the action to whatever sits at
  the path, including a directory that replaced the file. The first commit
  of a branch created from `create_branch_from` carries no token: GitLab
  would check it against the default branch instead of the commit the new
  branch starts from, and that commit cannot change.
- **A file never replaces a directory.** GitLab lets a created file replace
  a directory at the same path together with everything in it, so a new
  path where the branch holds a directory fails the apply without a commit.
  The one exception is a directory holding only files the resource manages
  and drops from `files` in the same apply: the commit deletes them first,
  then creates the file. A new path also fails when another resource in the
  same apply adds or adopts a file inside a directory of that name, even
  one it has not committed yet.
- **`commit_message` is per-apply**, not per-file. The same message is used
  for create / update / destroy commits. This is by design - one resource,
  one logical change, one message.

## Limits and retries

- **Commit request size.** GitLab caps the commit request body (300 MB by
  default). Because this provider batches every file change into one request,
  a very large bundle can hit that cap; the provider surfaces the 413 with
  advice to split the bundle across multiple resources (`for_each`), or raise
  `GITLAB_COMMITS_MAX_REQUEST_SIZE_BYTES` on self-managed GitLab.
- **Rate limits.** Read and probe requests are retried on 429 and transient
  5xx (`max_retries`, default 5). `retry_wait_min_ms` is the base wait between
  429 retries (it doubles per attempt and GitLab's `RateLimit-Reset` header
  extends it); `retry_wait_max_ms` bounds the random jitter added on top, not
  the total wait. 5xx retries use the client's fixed 700-900 ms schedule. On
  GitLab.com, commit requests above 20 MB (3 per 30 s) and reads of blobs
  above 10 MB (5 per minute) are throttled separately; self-managed instances
  configure such limits independently. Those limits send no rate-limit
  headers, so a 429 without `Retry-After` can mean one of them.
- **A Gitaly failure can look like a missing file.** GitLab answers a Files
  API request with 404 when Gitaly times out resolving the ref, exactly as
  for a file that does not exist; releases up to at least 19.4 do the same
  when Gitaly is unavailable. Before a 404 lets the provider drop a file
  from state or skip a delete, it looks the branch up, which reports the
  same failure as an error, and fails the refresh, apply or destroy if that
  lookup fails. That narrows the window without closing it: a timeout that
  has cleared by the time of the branch lookup still reads as a missing
  file, which a refresh drops from state (the next apply adopts it again
  without a commit, with `adopt_existing`, the default) and a destroy can
  leave in the repository.
- **Timeouts and redirects.** Every request waits at most five minutes for
  GitLab's response headers once it has been sent, so a wedged instance or
  proxy fails the apply instead of hanging it (uploads are not bounded by
  that). A redirect to another host or from https to http is never followed,
  because the request carries the token; it is reported with the target so
  you can point `base_url` at the final address.
- **Commits are not idempotent, so the commit request is never replayed on
  5xx.** `POST /repository/commits` has no request deduplication: if a proxy
  answered 502/504 after GitLab had already accepted the commit, a retry
  would land a second commit for the same apply. The provider therefore
  retries the commit request only on 429 (rejected before processing) and on
  connection failures that happen before the request is sent. A 5xx or a
  dropped connection fails the apply with the status in the diagnostic; run
  `terraform plan` to see whether the commit landed, and apply again if it
  did not. With `detect_drift = false` recorded in state the plan cannot
  show that: first set `detect_drift = true` with `files` as last applied
  and apply (this makes no commit), then put the change back in `files` and
  plan. A destroy can simply be run again, since files already deleted are
  skipped.
- **A branch that shares its name with a tag is not supported.** GitLab
  resolves the bare name to the tag first, and the provider, like GitLab's
  commits API, names the branch that way. File reads (refreshes, adoption,
  the probes before a delete) then see the tag's files, so a destroy can
  leave in place files the tag does not hold, and GitLab checks each commit
  against the tag: `last_commit_id` against its history, and the branch tip
  it expects against the tagged commit, so every commit to the branch fails
  unless the tag points at the branch tip. Such a failure reads as "Branch
  changed while the commit was being created" or "Concurrent modification
  detected", and both diagnostics say how to check for the tag
  (`git ls-remote <remote> refs/tags/<branch>`). Rename or delete the tag.

## Development

```bash
just build       # check + lint + test, then a static binary in ./dist
just test        # unit tests (go test -race ./...)
just lint        # golangci-lint
just check       # go vet + staticcheck + govulncheck + fieldalignment
just docs        # regenerate docs/ from the schema (CI fails on drift)
just ci          # the full CI gate: check + lint + test + tf-fmt + examples + docs + headers + deps
```

Acceptance tests run against a real GitLab project and are gated by env vars:

```bash
TF_ACC=1 \
GITLAB_TOKEN='...' \
GITLAB_TEST_PROJECT_ID='you/sandbox' \
GITLAB_TEST_BRANCH='tf-acc-test' \
GITLAB_TEST_BRANCH_FROM='main' \
GITLAB_BASE_URL='https://gitlab.example.com' \
go test -v -timeout=20m -run '^TestAcc' ./internal/...
```

`GITLAB_TEST_BRANCH` defaults to `tf-acc-test` and must pre-exist unless
`GITLAB_TEST_BRANCH_FROM` is set, in which case the tests create it from that
ref and delete it afterwards; `GITLAB_BASE_URL` is only needed for
self-hosted GitLab. See [CONTRIBUTING.md](CONTRIBUTING.md) for the full
development loop.

## More

- [CONTRIBUTING.md](CONTRIBUTING.md) - development loop, acceptance tests, PR conventions.
- [MIGRATION.md](MIGRATION.md) - upgrading from the earlier `gitlabcommits_commit` resource.
- [SECURITY.md](SECURITY.md) - threat model and how to report a vulnerability.

## License

MIT - see [LICENSE](LICENSE).

## Author

Dmitrij Shishkin ([@greeddj](https://github.com/greeddj))
