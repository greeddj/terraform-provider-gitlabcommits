terraform {
  required_providers {
    gitlabcommits = {
      source = "greeddj/gitlabcommits"
      # Pin a version once you depend on released behaviour, e.g.:
      # version = "~> 0.1.0"
    }
  }
}

# Token and base_url can also be supplied via the GITLAB_TOKEN and GITLAB_BASE_URL
# environment variables, in which case this block can be omitted entirely.
provider "gitlabcommits" {
  token    = var.gitlab_token
  base_url = "https://gitlab.com"
}

variable "gitlab_token" {
  description = "GitLab token with the `api` scope (Personal, Project, or Group access token), or a fine-grained personal access token with the permissions listed in the provider docs."
  type        = string
  sensitive   = true
}
