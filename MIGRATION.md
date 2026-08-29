# Migration guide

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
   - Drop the `action` attribute - the provider computes it from the diff.
   - Optional fields (`previous_path`, `encoding`) that were used for `move`
     and explicit base64 are no longer needed; use `content_base64` for
     binaries and let the diff handle moves as create/delete pairs.
3. **Remove old state**: `terraform state rm <addr>` for every
   `gitlabcommits_commit.*` resource.
4. **Apply with `adopt_existing = true`** (the default). For each new
   `gitlabcommits_files` resource the provider does a preflight
   `GetFileMetaData` per path and rewrites `create` to `update` for
   already-existing paths, so the apply converges without "file already
   exists" errors. Note that this first apply pushes **one adoption commit
   per resource** (its updates simply re-write the same bytes when the
   rendered content already matches the repo); only subsequent applies with
   no changes produce zero commits.
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

The first apply after the migration produces one adoption commit per
resource; every later apply with no changes produces zero commits.

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
