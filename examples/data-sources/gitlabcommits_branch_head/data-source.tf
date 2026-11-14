# Read the head commit and the protection status of the main branch.
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
