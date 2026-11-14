# Migration guide

This guide is for users of the first pre-release build (December 2025), with the source address `greeddj/gitlab-commits` and the resources `gitlabcommits_commit` and `gitlabcommits_file`.
All releases have the source address `greeddj/gitlabcommits` and one resource: `gitlabcommits_files`.
The name `gitlabcommits_file` now belongs to a read-only data source, which cannot replace the old resource.

## What changes

| Old | New |
| --- | --- |
| `gitlabcommits_commit`, or one `gitlabcommits_file` for each file | One `gitlabcommits_files` resource for each project and branch |
| `files` is a list of entries with `file_path` and `action` | `files` is a map with the paths as keys. The provider calculates the actions. |
| `encoding = "base64"` with base64 text in `content` | `content_base64` |
| `batch_mode` on `gitlabcommits_file` | No attribute. Each create, update or destroy makes a maximum of one commit. |
| No attribute for the executable bit | `execute_filemode`, default `false` |
| A destroy only removed the resource from state | A destroy deletes the managed files in a maximum of one commit. To keep the files, set `delete_on_destroy = false` and apply this change before the destroy. |
| The `gitlabcommits_commit` refresh checked only the commit SHA | With `detect_drift = true` (the default), the refresh reads each file and records drift. With `detect_drift = false`, the refresh does nothing. |
| No protection against external changes | `optimistic_lock = true` (the default) sends `last_commit_id` with most update, delete and chmod actions. See [Optimistic locking](docs/resources/files.md#optimistic-locking). |

The `token` and `base_url` attributes and the `GITLAB_TOKEN` and `GITLAB_BASE_URL` variables stay.

## Upgrade procedure

1. In `required_providers`, change `source` from `"greeddj/gitlab-commits"` to `"greeddj/gitlabcommits"`.
2. Set `version` to the release that you upgrade to.
3. Write `gitlabcommits_files` resources in place of the old resources.
   - Write `files` as a map, with the old `file_path` values as the keys.
   - Remove `action`, `encoding` and `batch_mode`.
   - Remove each entry whose `action` was `delete`. An entry that you keep needs content, and the first apply creates the file again.
   - For a `move` entry, add only the new path. The first apply does not delete the old path.
   - For an entry with `encoding = "base64"`, move the `content` value to `content_base64`. Otherwise the provider commits the base64 text.
   - Set `execute_filemode = true` for each file that must stay executable. Otherwise the first apply removes the executable bit of an adopted file.
   - Move `commit_message`, `author_name` and `author_email` to the new resource. These values apply to all its files.
   - Keep `adopt_existing = true` (the default), so that the first apply adopts the files on the branch.
4. Remove the old resources from state before the `terraform init` that installs the new provider.
   `terraform init` fails while state holds resources of `greeddj/gitlab-commits`, because the registry does not have this provider.
   1. If the working directory is new, run `terraform init`. It fails, but it sets up the backend.
   2. Run `terraform state rm <address>` for each old resource. The files stay on the branch.
5. Run `terraform init`.
6. Run `terraform apply`.

   The first apply deletes no file. It creates the missing files and changes what is different.
   It fails if a path holds a directory or an LFS pointer to other content.
   A resource whose files all match makes no commit, and its `commit_sha` is null.
7. Run `terraform plan`. Make sure that it shows no changes.

## Example

```hcl
# Before
resource "gitlabcommits_commit" "frontend" {
  project_id     = "platform/gitops"
  branch         = "main"
  commit_message = "sync frontend"

  files = [
    { file_path = "values/dev.yaml",  action = "create", content = yamlencode(local.dev) },
    { file_path = "values/prod.yaml", action = "create", content = yamlencode(local.prod) },
  ]
}
```

```hcl
# After
resource "gitlabcommits_files" "frontend" {
  project_id     = "platform/gitops"
  branch         = "main"
  commit_message = "sync frontend"

  files = {
    "values/dev.yaml"  = { content = yamlencode(local.dev) }
    "values/prod.yaml" = { content = yamlencode(local.prod) }
  }
}
```
