# Read one file at a ref, e.g. to compare rendered HCL against what is
# currently committed, or to feed its metadata into other resources.
data "gitlabcommits_file" "renovate_config" {
  project_id = "platform/gitops"
  branch     = "main"
  file_path  = "renovate.json"
}

output "renovate_blob_id" {
  value = data.gitlabcommits_file.renovate_config.blob_id
}

output "renovate_content" {
  # Null for binary (non-UTF-8) files; content_base64 is always set.
  value = data.gitlabcommits_file.renovate_config.content
}
