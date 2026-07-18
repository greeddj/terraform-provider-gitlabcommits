# Read the branch HEAD, e.g. to bootstrap external last_commit_id flows or
# to wire downstream pipelines to the exact SHA terraform saw.
data "gitlabcommits_branch_head" "main" {
  project_id = "platform/gitops"
  branch     = "main"
}

output "main_head_sha" {
  value = data.gitlabcommits_branch_head.main.commit_sha
}

output "main_is_protected" {
  value = data.gitlabcommits_branch_head.main.protected
}
