# This resource manages four files on the main branch of platform/gitops.
resource "gitlabcommits_files" "service" {
  project_id     = "platform/gitops"
  branch         = "main"
  commit_message = "chore(frontend): sync gitops manifests via terraform"

  author_name  = "terraform-bot"
  author_email = "terraform@example.com"

  files = {
    "services/frontend/values/dev.yaml" = {
      content = yamlencode({
        replicas = 1
        image    = { tag = "1.2.3" }
      })
    }

    "services/frontend/values/prod.yaml" = {
      content = yamlencode({
        replicas = 5
        image    = { tag = "1.2.3" }
      })
    }

    "services/frontend/argocd/dev.yaml" = {
      content = yamlencode({
        apiVersion = "argoproj.io/v1alpha1"
        kind       = "Application"
        metadata   = { name = "frontend-dev", namespace = "argocd" }
      })
    }

    "scripts/run.sh" = {
      content          = "#!/bin/sh\nexec /app\n"
      execute_filemode = true
    }
  }
}
