terraform {
  required_providers {
    gitlabcommits = {
      source = "greeddj/gitlabcommits"
    }
  }
}

provider "gitlabcommits" {
  token    = var.gitlab_token
  base_url = "https://gitlab.example.com"
}

variable "gitlab_token" {
  description = "A personal, project or group access token with the api scope, or a fine-grained personal access token. See the provider page, Authentication."
  type        = string
  sensitive   = true
}

# One resource manages the files of one service. A create, an update or a
# destroy makes at most one commit. A replacement can make two commits.
resource "gitlabcommits_files" "frontend" {
  project_id     = "platform/gitops"
  branch         = "main"
  commit_message = "chore(frontend): sync values via terraform"

  author_name  = "terraform-bot"
  author_email = "terraform@example.com"

  files = {
    "services/frontend/values/dev.yaml" = {
      content = yamlencode({ replicas = 1, image = { tag = "1.2.3" } })
    }
    "services/frontend/values/staging.yaml" = {
      content = yamlencode({ replicas = 2, image = { tag = "1.2.3" } })
    }
    "services/frontend/values/prod.yaml" = {
      content = yamlencode({ replicas = 5, image = { tag = "1.2.3" } })
    }
    "services/frontend/argocd/dev.yaml" = {
      content = yamlencode({
        apiVersion = "argoproj.io/v1alpha1"
        kind       = "Application"
        metadata   = { name = "frontend-dev", namespace = "argocd" }
      })
    }
  }
}

output "frontend_commit" {
  value       = gitlabcommits_files.frontend.commit_sha
  description = "The SHA of the last commit that the frontend resource made. It is null until the resource makes a commit, and again after a replacement that makes no commit."
}
