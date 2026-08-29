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
  `commit_sha` unset. When the branch does not exist yet and
  `create_branch_from` is set, the branch is created from that ref in the
  same operation as the commit (one push event); with nothing to commit it is
  created on its own.
- **Read** - probes each managed file via a HEAD-style metadata call
  (`GetFileMetaData`) and compares the GitLab-returned `blob_id` and exec
  bit with state. Only when the blob has actually drifted does it pull the
  full content. Any drift updates state, so the next plan shows real
  differences against the repo.
- **Update** - diffs plan vs state and emits the **minimum** set of actions:
  gone paths -> `delete` (emitted first), new paths -> `create`, or nothing
  when the path already exists with identical content, content changed ->
  `update`, exec bit flipped -> `chmod`. If nothing changed, no commit is
  produced.
- **Delete** - pushes one commit that removes every managed file. Files
  already absent on the remote are skipped (idempotent against external
  cleanup). Disable with `delete_on_destroy = false`.

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
- Go >= 1.27 (development only)

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
| `project_id` | string | yes | Numeric project ID or plain project path (`group/subgroup/project`, not URL-encoded). Changing it forces replacement; see Caveats. |
| `branch` | string | yes | Target branch (must exist, or set `create_branch_from`). Changing it forces replacement; see Caveats. |
| `commit_message` | string | yes | Used for any commit produced (create / update / destroy). |
| `author_name` | string | no | Override commit author name. |
| `author_email` | string | no | Override commit author email. |
| `create_branch_from` | string | no | If set and `branch` does not yet exist, create it from this branch name or full commit SHA (typically `main`; tags are not supported) together with the first commit, or on its own when there is nothing to commit. The branch is not deleted on destroy. |
| `detect_drift` | bool | no | Default `true`. If false, Read is a no-op. |
| `delete_on_destroy` | bool | no | Default `true`. If false, destroy only drops state. Read from the state of the last apply; see Caveats. |
| `adopt_existing` | bool | no | Default `true`. Rewrite `create` to `update` for paths that already exist, or to no action when their content already matches (needed for clean import). |
| `optimistic_lock` | bool | no | Default `true`. Send each file's `last_commit_id` so GitLab rejects concurrent updates with HTTP 400. Set to `false` to opt out. For the destroy commit, read from the state of the last apply. |
| `files` | map of object | yes | See below. Must not be empty: `files = {}` would mean "delete everything", which is what `terraform destroy` is for. |
| `id` | string | computed | Composite identifier `<project_id>::<branch>`. |
| `commit_sha` | string | computed | SHA of the most recent commit produced by this resource. |

### `files` entry

| Field | Type | Computed | Notes |
| --- | --- | --- | --- |
| `content` | string | no | text content; mutually exclusive with `content_base64` |
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
  (a numeric ID and a path are different lock keys), and a second provider
  block (alias) runs in its own process. Any other writer (another pipeline,
  a manual push, an aliased provider) is reported as "Branch changed while
  the commit was being created" and you re-run the apply.
- **Changing `branch` or `project_id` replaces the resource.** Replacement is
  destroy-then-create, and with the default `delete_on_destroy = true` the
  destroy pushes a commit deleting every managed file from the OLD branch
  before the files are created on the new one. To re-point without emptying
  the old branch, set `delete_on_destroy = false` and apply first, or use a
  new resource address.
- **`delete_on_destroy` and `optimistic_lock` apply as last applied.**
  Terraform does not evaluate configuration during `terraform destroy`, so the
  destroy commit uses the values recorded in state by the last apply. Change
  the flag in HCL, run `terraform apply`, then destroy.
- **State holds your file content.** If you set `content_base64` to the bytes
  of a 10 MB binary, those bytes live in `terraform.tfstate`. Use a secrets
  backend and avoid committing huge binaries through this provider.
- **Optimistic locking is on by default.** Each update / delete action sends
  the file's `last_commit_id` so GitLab rejects the action with HTTP 400 if
  someone else has touched the file since this resource last did. The
  provider surfaces those as "Concurrent modification detected" diagnostics
  with a hint to run `terraform apply -refresh-only`. Set
  `optimistic_lock = false` per resource to opt out (e.g. when an external
  bot intentionally co-edits the same files); the trade-off is silent
  last-write-wins.
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
  did not.

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
