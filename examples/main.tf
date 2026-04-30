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
  description = "GitLab Personal/Project Access Token with the `api` scope and write_repository permission."
  type        = string
  sensitive   = true
}

# ----------------------------------------------------------------------
# Minimal example: one resource = one service = one commit per apply
# ----------------------------------------------------------------------
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
  description = "SHA of the commit produced by the frontend bundle."
}
