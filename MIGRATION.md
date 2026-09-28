# Migration guide

The resources this guide replaces, `gitlabcommits_commit` and the
`gitlabcommits_file` resource, existed only in pre-release source builds.
Every tagged release, v0.1.0 onward, ships `gitlabcommits_files` as its only
resource, and `gitlabcommits_file` there is a read-only data source.

## From `gitlabcommits_commit` to `gitlabcommits_files`

The old `gitlabcommits_commit` modelled a commit as a Terraform resource. That
model never fit Terraform's desired-state semantics: Read couldn't detect
drift, Update silently created another commit on top, and Delete was a no-op.
The new resource (`gitlabcommits_files`) treats a *bundle of files in a branch*
as the unit of state - which is what users actually want to manage.

### What changes

| Old | New |
| --- | --- |
| `gitlabcommits_commit` | `gitlabcommits_files` |
| `files = [{ file_path = "...", action = "create", content = "..." }, ...]` (list, explicit action) | `files = { "path/to/file" = { content = "..." } }` (map, action inferred from diff) |
| `Update` always pushed a new commit with the entire list | Update emits the *minimum* set of actions; produces zero commits when nothing changed |
| `Delete` was a no-op | Delete pushes one commit removing every managed file (toggle via `delete_on_destroy`) |
| Read only verified the SHA existed | Read fetches each file's `blob_id` and reconciles state with reality |
| No protection against concurrent writes | `optimistic_lock = true` (default) sends `last_commit_id` per action |

### Step-by-step upgrade

1. **Pin the release you are upgrading to** in `required_providers` (any
   release that ships `gitlabcommits_files`).
2. **Re-write each `gitlabcommits_commit` resource** to `gitlabcommits_files`:
   - Convert the `files` list to a map keyed by `file_path`.
   - Drop the `action` attribute - the provider computes it from the diff. A
     `move` becomes the old path removed from the map and the new path
     added: a delete and a create in one commit.
   - Drop the `encoding` attribute: `content` holds text, and setting
     `content_base64` instead is what marks base64 content (use it for
     binaries).
   - `project_id` takes a numeric ID or the plain project path
     (`group/subgroup/project`); a URL-encoded path fails validation.
3. **Remove old state**: `terraform state rm <addr>` for every
   `gitlabcommits_commit.*` resource.
4. **Apply with `adopt_existing = true`** (the default). For each new
   `gitlabcommits_files` resource the provider probes every path
   (`GetFileMetaData`, plus one `GetFile` for each path that already exists)
   and rewrites `create` to `update` for already-existing paths, so the apply
   converges without "file already exists" errors. A path whose content and
   mode already match the rendered configuration needs no action at all (for
   a Git LFS-tracked file, a pointer that names the rendered bytes counts as
   a match), and one that differs only in its exec bit needs only a `chmod`.
   So a resource whose files all match the repository makes no commit and
   leaves `commit_sha` null, and anything wired to `commit_sha` sees null
   until the resource commits a real change; a resource with differing files
   pushes one adoption commit carrying only those. A path that cannot be read
   stops the apply before anything is committed, so re-running it is safe.
   So does a Git LFS-tracked file whose pointer names other content than the
   rendered bytes, because the commits API would store the update as a
   regular blob outside LFS: change that file with git and Git LFS first, or
   render the content its pointer names (see Limits in the provider
   documentation).
5. **Inspect once** - run `terraform plan` again; it should report no
   changes.

### When using a custom CI orchestrator

If your CI already serialises Terraform applies (a `resource_group` in
GitLab, an `interlock` in CircleCI, a `concurrency` group in GitHub Actions),
you can leave `optimistic_lock = true` (default) and the provider becomes a
hard backstop against accidentally racing pipelines. There is no
configuration tax.

If multiple distinct systems (Terraform + a CI bot + humans) intentionally
co-edit the same files, set `optimistic_lock = false` per resource so
update/delete actions don't fail with HTTP 400 when the file moved
underneath you. The trade-off is exactly what you'd expect: silent
last-write-wins.

### Example before / after

```hcl
# BEFORE - gitlabcommits_commit
resource "gitlabcommits_commit" "frontend" {
  project_id     = "platform/gitops"
  branch         = "main"
  commit_message = "sync frontend"

  files = [
    { file_path = "values/dev.yaml",  action = "create", content = yamlencode(local.dev) },
    { file_path = "values/prod.yaml", action = "create", content = yamlencode(local.prod) },
    { file_path = "argocd/dev.yaml",  action = "create", content = yamlencode(local.argocd_dev) },
  ]
}
```

```hcl
# AFTER - gitlabcommits_files
resource "gitlabcommits_files" "frontend" {
  project_id     = "platform/gitops"
  branch         = "main"
  commit_message = "sync frontend"

  files = {
    "values/dev.yaml"  = { content = yamlencode(local.dev) }
    "values/prod.yaml" = { content = yamlencode(local.prod) }
    "argocd/dev.yaml"  = { content = yamlencode(local.argocd_dev) }
  }
}
```

The first apply after the migration produces at most one adoption commit per
resource, none for a resource whose files already match the repository;
every later apply with no changes produces zero commits.

## From the `gitlabcommits_file` resource to `gitlabcommits_files`

The pre-release `gitlabcommits_file` resource managed one file per resource.
With `batch_mode` (the default) it gathered the files created on the same
project and branch within five seconds of the first into one commit, under
that first file's `commit_message`; every update was a commit of its own.
Its destroy only dropped state. The name
now belongs to a read-only data source, so `data "gitlabcommits_file"` is not
a replacement.

1. **Fold the resources into bundles**: one `gitlabcommits_files` resource
   per project and branch, whose `files` map is keyed by each old resource's
   `file_path` and holds its `content` or `content_base64`. Move
   `commit_message`, `author_name` and `author_email` to the new resource and
   drop `action`, `encoding` and `batch_mode`: a resource makes at most one
   commit per apply without them. `project_id` takes a numeric ID or the
   plain project path (`group/subgroup/project`); a URL-encoded path fails
   validation.
2. **Remove old state**: `terraform state rm <addr>` for every
   `gitlabcommits_file.*` resource.
3. **Apply and inspect** as in steps 4 and 5 above: existing files are
   adopted, without a commit where they already match.

Unlike the old resource, destroying a `gitlabcommits_files` resource deletes
its files in one commit; set `delete_on_destroy = false` and apply to keep
them.

## `blob_id` is now opaque

Early pre-release builds computed each file's `blob_id` locally using
`sha1("blob <size>\0<content>")` - git's own format - so the value in
`terraform.tfstate` was always a 40-character SHA-1. The provider now
stores whatever `blob_id` GitLab returns, treating it as an opaque
string. On SHA-1 repositories the value is unchanged; on SHA-256
repositories (an opt-in experiment since GitLab 16.7, behind the
`support_sha256_repositories` feature flag and chosen at project creation)
it will be a 64-character SHA-256.

No user action is required: `blob_id` is `Computed`, so the first plan /
apply after the upgrade overwrites it from the GitLab API. If you
reference `blob_id` from another resource or output, expect the value to
change in state on the next apply.
