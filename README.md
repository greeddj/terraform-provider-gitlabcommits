# Terraform Provider: GitLab Commits

[![CI](https://github.com/greeddj/terraform-provider-gitlabcommits/actions/workflows/ci.yml/badge.svg)](https://github.com/greeddj/terraform-provider-gitlabcommits/actions/workflows/ci.yml)
[![Release](https://github.com/greeddj/terraform-provider-gitlabcommits/actions/workflows/release.yml/badge.svg)](https://github.com/greeddj/terraform-provider-gitlabcommits/actions/workflows/release.yml)
[![codecov](https://codecov.io/gh/greeddj/terraform-provider-gitlabcommits/graph/badge.svg)](https://codecov.io/gh/greeddj/terraform-provider-gitlabcommits)

The `gitlabcommits` provider manages a bundle of files on one branch of a GitLab project.
Each create, update or destroy of a `gitlabcommits_files` resource puts all its file changes into one commit.

A GitOps repository can hold many similar YAML files, for example Helm values for each service and environment.
With the `gitlab_repository_file` resource of the GitLab provider, each file gets its own commit, and each commit can start a CI pipeline.
With this provider, one resource manages the files of one service.

> **Note:** The author made this project in collaboration with Claude Code.

## Requirements

- Terraform 1.5 or later.
- GitLab 19.x, the supported version.
- A GitLab token that can use the REST API on the project. See [Authentication](docs/index.md#authentication).
- On macOS and Windows, a value in `SSL_CERT_FILE` or `SSL_CERT_DIR` replaces the system certificate store (the Go 1.27 default).
  If only the system store holds the CA certificate of your GitLab instance, unset these variables or set `GODEBUG=x509sslcertoverrideplatform=0`.

## Quick start

```hcl
terraform {
  required_providers {
    gitlabcommits = {
      source  = "greeddj/gitlabcommits"
      version = "~> 0.3.0"
    }
  }
}

provider "gitlabcommits" {
  # Optional. The default is GITLAB_BASE_URL, then https://gitlab.com.
  base_url = "https://gitlab.example.com"
}

resource "gitlabcommits_files" "demo" {
  project_id     = "platform/gitops"
  branch         = "main"
  commit_message = "chore(demo): sync the demo values"

  files = {
    "services/demo/values.yaml" = {
      content = yamlencode({ replicas = 2 })
    }
  }
}
```

1. Put this configuration, with the values of your project, in a `.tf` file.
2. Set the `GITLAB_TOKEN` environment variable to your token.
3. Run `terraform init`.
4. Run `terraform apply`.

## How it works

- One `gitlabcommits_files` resource manages the `files` map on one branch of one project.
  For more branches or projects, use more resources, for example with `for_each`.
- An update that changes no content, no executable bit and no path makes no commit.
- When Terraform destroys a resource, also when you remove it from the configuration, the provider deletes its files (`delete_on_destroy`, default `true`).
  A change to `project_id` or `branch` replaces the resource, so one apply can make a delete commit and a create commit.
- When a new path already holds a file, the resource adopts it (`adopt_existing`, default `true`).
  The commit then changes only the content or the executable bit that is different.
- A refresh records external changes in state (`detect_drift`, default `true`), so the next plan shows the changes that restore your configuration.
- Most commits that update or delete a file, or change its executable bit, send the `last_commit_id` of that file (`optimistic_lock`, default `true`).
  If the file changes after the provider reads that value, GitLab refuses the commit (see [Optimistic locking](docs/resources/files.md#optimistic-locking)).

For the full behavior, see [How the resource works](docs/resources/files.md#how-the-resource-works) and [Caveats](docs/resources/files.md#caveats).

## Example: many services and environments

Here, one resource manages the Helm values and the Argo CD applications of one service for all environments.

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
          }
        })
      }
    },
  )
}
```

With 20 services and 30 environments, the 20 resources manage 1,200 files.
An apply without a replacement makes at most 20 commits. With one commit for each file, the first apply makes 1,200 commits.
A replacement of all 20 resources can make 40 commits in one apply.
The provider makes the commits of these resources one at a time, so they do not conflict (see [Many resources on one branch](docs/resources/files.md#many-resources-on-one-branch)).

A complete example is in [`examples/for_each/main.tf`](examples/for_each/main.tf).

## Documentation

- [Provider](docs/index.md): the configuration, [Authentication](docs/index.md#authentication) and [Limits](docs/index.md#limits).
- [`gitlabcommits_files` resource](docs/resources/files.md): the attributes, the behavior, [Caveats](docs/resources/files.md#caveats) and [Import](docs/resources/files.md#import).
- [`gitlabcommits_file` data source](docs/data-sources/file.md): one file at a branch, a tag or a commit.
- [`gitlabcommits_branch_head` data source](docs/data-sources/branch_head.md): the head commit of a branch and its protection status.
- [Examples](examples/): complete configurations and short examples.

## Development

To build and test the provider, you need Go 1.27.4 or later, `just`, `golangci-lint`, the `terraform` CLI and `git`.

```bash
just build   # check, lint and test, then a static binary in ./dist
just test    # go test -race ./...; TF_ACC adds the acceptance tests
just docs    # generate docs/ from the schema, the templates and the examples
just ci      # the CI checks, without the goreleaser snapshot build
```

For the development procedure and the acceptance tests, see [CONTRIBUTING.md](CONTRIBUTING.md).

## More

- [MIGRATION.md](MIGRATION.md): the upgrade from the pre-release `gitlabcommits_commit` and `gitlabcommits_file` resources.
- [SECURITY.md](SECURITY.md): the threat model and how to report a vulnerability.

## License

MIT. See [LICENSE](LICENSE).

## Author

Dmitrij Shishkin ([@greeddj](https://github.com/greeddj))
