# Manage a bundle of files in one branch of one project. Every change
# (create / update / delete / chmod) is batched into a single commit.
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
