terraform {
  required_providers {
    gitlabcommits = {
      source  = "greeddj/gitlabcommits"
      version = "~> 0.2"
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
  description = "GitLab token with the `api` scope and write_repository on the target project."
  type        = string
  sensitive   = true
}
