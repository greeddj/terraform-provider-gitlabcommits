# Read renovate.json from the main branch.
data "gitlabcommits_file" "renovate_config" {
  project_id = "platform/gitops"
  branch     = "main"
  file_path  = "renovate.json"
}

output "renovate_blob_id" {
  value = data.gitlabcommits_file.renovate_config.blob_id
}

output "renovate_content" {
  # For a binary file, content is null. Use content_base64 instead.
  value = data.gitlabcommits_file.renovate_config.content
}
