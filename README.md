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
- **Read** — fetches each managed file from the GitLab API and compares its
  `blob_id` with the one stamped in state. The provider computes blob ids
  locally (the same `sha1("blob <size>\0<content>")` git itself uses) so the
  comparison is exact and free. Any drift updates state, so the next plan
  shows real differences against the repo.
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
- A GitLab token with `api` scope and write permission on the target repo
- Go >= 1.25 (development only)

## Provider configuration

```hcl
terraform {
  required_providers {
    gitlabcommits = {
      source  = "greeddj/gitlabcommits"
      version = "~> 0.2"
    }
  }
}

provider "gitlabcommits" {
  token    = var.gitlab_token              # or GITLAB_TOKEN
  base_url = "https://gitlab.example.com"  # optional; or GITLAB_BASE_URL
}
```

## Resource: `gitlabcommits_files`

| Argument | Type | Required | Description |
| --- | --- | --- | --- |
| `project_id` | string | yes | Project ID or URL-encoded path. ForceNew. |
| `branch` | string | yes | Target branch (must exist). ForceNew. |
| `commit_message` | string | yes | Used for any commit produced (create / update / destroy). |
| `author_name` | string | no | Override commit author name. |
| `author_email` | string | no | Override commit author email. |
| `detect_drift` | bool | no | Default `true`. If false, Read is a no-op. |
| `delete_on_destroy` | bool | no | Default `true`. If false, destroy only drops state. |
| `adopt_existing` | bool | no | Default `true`. Rewrite `create` to `update` for paths that already exist (needed for clean import). |
| `files` | map of object | yes | See below. |
| `commit_sha` | string | computed | SHA of the most recent commit produced by this resource. |

### `files` entry

| Field | Type | Notes |
| --- | --- | --- |
| `content` | string | text content; mutually exclusive with `content_base64` |
| `content_base64` | string | base64-encoded content (use for binaries); mutually exclusive with `content` |
| `execute_filemode` | bool | default `false`; toggling triggers a `chmod` action |
| `blob_id` | string | computed git blob SHA-1, used for drift detection |

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

A complete example lives in [`examples/for_each_example.tf`](examples/for_each_example.tf).

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
- **No optimistic locking.** Two `terraform apply` runs against the same
  branch concurrently will both succeed and the second one wins. Serialise
  through your CI/CD orchestration if you need stronger guarantees.
- **`commit_message` is per-apply**, not per-file. The same message is used
  for create / update / destroy commits. This is by design — one resource,
  one logical change, one message.

## Development

```bash
just build       # build the provider
just test        # unit tests
just lint        # vet + staticcheck + golangci-lint
just check       # fmt-check + test + lint
just testacc     # acceptance tests (require GITLAB_TOKEN, GITLAB_TEST_PROJECT_ID, TF_ACC=1)
```

## License

MIT — see [LICENSE](LICENSE).

## Author

Dmitrij Shishkin ([@greeddj](https://github.com/greeddj))
