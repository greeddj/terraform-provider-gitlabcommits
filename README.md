# Terraform Provider: GitLab Commits

A Terraform provider that manages **bundles of files** in a GitLab repository.
Every change a resource makes — create, update, delete or chmod — is batched
into **one commit per `terraform apply`**, so you get **one CI pipeline run per
resource** instead of one per file.

It exists to solve a very specific operational problem: a GitOps repository
holding huge fan-outs of nearly-identical YAML (helm values, ArgoCD
applications) for *N services × M environments*. With `gitlab_repository_file`
each file becomes its own commit, which is a non-starter at any reasonable scale.
This provider lets you express the bundle as Terraform and emit exactly one
commit per service.

## How it actually works

A single resource (`gitlabcommits_files`) owns a `map(path → file)` on one
branch of one project. The provider:

- **Create** — pushes one commit that creates every file. If `adopt_existing`
  is true (default) and a path already exists in the repo, the action is
  silently rewritten from `create` to `update`, so the apply does not fail.
- **Read** — probes each managed file via a HEAD-style metadata call
  (`GetFileMetaData`) and compares the GitLab-returned `blob_id` with the
  one in state. Only when the blob has actually drifted does it pull the
  full content. Any drift updates state, so the next plan shows real
  differences against the repo.
- **Update** — diffs plan vs state and emits the **minimum** set of actions:
  new paths → `create`, gone paths → `delete`, content changed → `update`,
  exec bit flipped → `chmod`. If nothing changed, no commit is produced.
- **Delete** — pushes one commit that removes every managed file. Files
  already absent on the remote are skipped (idempotent against external
  cleanup). Disable with `delete_on_destroy = false`.

The composite ID is `<project_id>::<branch>`. Import with that format; the
files map starts empty and is reconciled on the next plan + apply.

## Requirements

- Terraform >= 1.5
- GitLab >= 18.x (older versions may work for basic operations but are not supported)
- A token that can call the GitLab REST API on the target project (see Authentication below)
- Go >= 1.26 (development only)

## Provider configuration

```hcl
terraform {
  required_providers {
    gitlabcommits = {
      source = "greeddj/gitlabcommits"
      # Pin a version once you depend on released behaviour, e.g.:
      # version = "~> 1.0"
    }
  }
}

provider "gitlabcommits" {
  token    = var.gitlab_token              # or GITLAB_TOKEN
  base_url = "https://gitlab.example.com"  # optional; or GITLAB_BASE_URL
}
```

## Authentication

The provider needs a token that can call the GitLab REST API on the target
project. Supported token types:

- **Personal Access Token** with the `api` scope.
- **Project Access Token** (or **Group Access Token**) with the `api` scope.
  Recommended for CI/CD — scope a token to exactly the project(s) you manage.

The token's user (or the token itself, for Project/Group tokens) needs the
**Developer** role, or **Maintainer** to push to a protected branch.

Pass the token via the `GITLAB_TOKEN` environment variable or the `token`
attribute on the provider block. In CI, prefer a CI variable such as
`TF_VAR_gitlab_token` over committing the token.

> The `write_repository` scope is for Git-over-HTTP (push/pull) and does
> **not** authenticate REST API calls — `api` is the only scope that does.
> `CI_JOB_TOKEN` is not supported: GitLab's job-token allowlist permits only
> GET on Commits/Files/Branches APIs, while this provider needs
> `POST /repository/commits`.

## Resource: `gitlabcommits_files`

| Argument | Type | Required | Description |
| --- | --- | --- | --- |
| `project_id` | string | yes | Project ID or URL-encoded path. ForceNew. |
| `branch` | string | yes | Target branch (must exist, or set `create_branch_from`). ForceNew. |
| `commit_message` | string | yes | Used for any commit produced (create / update / destroy). |
| `author_name` | string | no | Override commit author name. |
| `author_email` | string | no | Override commit author email. |
| `create_branch_from` | string | no | If set and `branch` does not yet exist, create it from this source ref (typically `main`) on first apply. |
| `detect_drift` | bool | no | Default `true`. If false, Read is a no-op. |
| `delete_on_destroy` | bool | no | Default `true`. If false, destroy only drops state. |
| `adopt_existing` | bool | no | Default `true`. Rewrite `create` to `update` for paths that already exist (needed for clean import). |
| `optimistic_lock` | bool | no | Default `true`. Send each file's `last_commit_id` so GitLab rejects concurrent updates with HTTP 400. Set to `false` to opt out. |
| `files` | map of object | yes | See below. |
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

## Typical layout: 20 services × 30 environments

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

20 resources → 20 commits per apply → 20 pipeline runs. Not 600.

A complete example lives in [`examples/for_each/main.tf`](examples/for_each/main.tf).

## Import

```bash
terraform import 'gitlabcommits_files.service["frontend"]' 'platform/gitops::main'
```

After import the files map is empty in state. The next plan will produce
`create` actions for every file; with `adopt_existing = true` (default) those
that already exist in the repo are silently turned into `update`s, so the
apply converges without manual surgery.

## Caveats worth knowing

- **One resource = one branch = one project**. To target multiple branches or
  repos, use multiple resources (typically via `for_each`).
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
  for create / update / destroy commits. This is by design — one resource,
  one logical change, one message.

## Limits and retries

- **Commit request size.** GitLab caps the commit request body (300 MB by
  default). Because this provider batches every file change into one request,
  a very large bundle can hit that cap; the provider surfaces the 413 with
  advice to split the bundle across multiple resources (`for_each`), or raise
  `GITLAB_COMMITS_MAX_REQUEST_SIZE_BYTES` on self-managed GitLab.
- **Rate limits.** 429 and transient 5xx responses are retried with
  exponential backoff (`max_retries`, `retry_wait_min_ms`,
  `retry_wait_max_ms` on the provider block). GitLab.com additionally
  throttles writes above ~20 MB.
- **Commits are not idempotent.** `POST /repository/commits` has no request
  deduplication, so if the network fails after GitLab accepted the commit but
  before the response arrived, a retry replays the request. With
  `optimistic_lock = true` (default) the replay fails loudly as a
  "Concurrent modification detected" conflict while the original commit
  stands - run `terraform plan` to reconcile. With the lock disabled a replay
  can land a duplicate commit. If your pipeline is sensitive to duplicate
  pipelines per apply, keep the lock on.

## Development

```bash
just build       # check + lint + test, then a static binary in ./dist
just test        # unit tests (go test ./...)
just lint        # golangci-lint
just check       # go vet + staticcheck + govulncheck + fieldalignment
just ci          # the full CI gate: check + lint + test + tf-fmt + examples + docs + headers + deps
```

Acceptance tests run against a real GitLab project and are gated by env vars:

```bash
TF_ACC=1 \
GITLAB_TOKEN='...' \
GITLAB_TEST_PROJECT_ID='you/sandbox' \
GITLAB_TEST_BRANCH_FROM='main' \
go test -v -timeout=20m -run '^TestAcc' ./internal/...
```

## License

MIT — see [LICENSE](LICENSE).

## Author

Dmitrij Shishkin ([@greeddj](https://github.com/greeddj))
