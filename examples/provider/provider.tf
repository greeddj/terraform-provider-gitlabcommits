terraform {
  required_providers {
    gitlabcommits = {
      source = "greeddj/gitlabcommits"
      # Pin the provider version, for example:
      # version = "~> 0.3.0"
    }
  }
}

# You can set the token and the GitLab URL in GITLAB_TOKEN and GITLAB_BASE_URL.
# Then you do not need this provider block.
provider "gitlabcommits" {
  token    = var.gitlab_token
  base_url = "https://gitlab.com" # This is the default. For a self-managed GitLab, set its URL.
}

variable "gitlab_token" {
  description = "GitLab token: an access token with the api scope, or a fine-grained personal access token. See Authentication on the provider page."
  type        = string
  sensitive   = true
}
