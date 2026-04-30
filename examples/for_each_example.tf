# ============================================================================
# Real-world layout: 20 services × 30 environments
#   - one terraform resource per service
#   - one commit per service per apply
#   - all per-env helm values and ArgoCD application manifests in that commit
# ============================================================================

terraform {
  required_providers {
    gitlabcommits = {
      source = "greeddj/gitlabcommits"
    }
  }
}

provider "gitlabcommits" {
  token = var.gitlab_token
}

variable "gitlab_token" {
  type      = string
  sensitive = true
}

variable "gitops_project" {
  description = "GitLab project hosting the GitOps repo (e.g. \"platform/gitops\")."
  type        = string
}

variable "gitops_branch" {
  description = "Branch the provider commits to."
  type        = string
  default     = "main"
}

# 30 environments × 20 services = 600 (service, env) pairs handled below.
variable "environments" {
  type = map(object({
    cluster   = string
    domain    = string
    namespace = string
    replicas  = number
  }))
}

variable "services" {
  type = map(object({
    image_repo = string
    image_tag  = string
    port       = number
    resources = object({
      cpu    = string
      memory = string
    })
  }))
}

# ----------------------------------------------------------------------
# One resource per service. Inside each resource we generate every file
# for every environment, so each terraform apply creates exactly one
# commit per service (and one CI pipeline run per service).
# ----------------------------------------------------------------------
resource "gitlabcommits_files" "service" {
  for_each = var.services

  project_id     = var.gitops_project
  branch         = var.gitops_branch
  commit_message = "chore(${each.key}): sync gitops manifests via terraform"

  author_name  = "terraform-bot"
  author_email = "terraform@example.com"

  files = merge(
    # Helm values per environment.
    {
      for env_name, env in var.environments :
      "services/${each.key}/values/${env_name}.yaml" => {
        content = yamlencode({
          image = {
            repository = each.value.image_repo
            tag        = each.value.image_tag
          }
          replicaCount = env.replicas
          service = {
            port = each.value.port
          }
          resources = {
            requests = each.value.resources
            limits   = each.value.resources
          }
          ingress = {
            enabled = true
            host    = "${each.key}.${env.domain}"
          }
        })
      }
    },

    # ArgoCD Application manifest per environment.
    {
      for env_name, env in var.environments :
      "services/${each.key}/argocd/${env_name}.yaml" => {
        content = yamlencode({
          apiVersion = "argoproj.io/v1alpha1"
          kind       = "Application"
          metadata = {
            name      = "${each.key}-${env_name}"
            namespace = "argocd"
            labels = {
              "app.kubernetes.io/name"       = each.key
              "app.kubernetes.io/instance"   = env_name
              "app.kubernetes.io/managed-by" = "terraform"
            }
          }
          spec = {
            project = "default"
            source = {
              repoURL        = "https://gitlab.example.com/${var.gitops_project}.git"
              targetRevision = var.gitops_branch
              path           = "services/${each.key}/chart"
              helm = {
                valueFiles = ["../values/${env_name}.yaml"]
              }
            }
            destination = {
              server    = env.cluster
              namespace = env.namespace
            }
            syncPolicy = {
              automated = {
                prune    = true
                selfHeal = true
              }
            }
          }
        })
      }
    },
  )
}

# ----------------------------------------------------------------------
# Outputs: per-service commit SHAs (handy for downstream pipelines).
# ----------------------------------------------------------------------
output "service_commits" {
  description = "Map service_name -> commit SHA produced by this apply."
  value       = { for k, r in gitlabcommits_files.service : k => r.commit_sha }
}

# ----------------------------------------------------------------------
# Example variable values (replace with real ones in tfvars / env)
# ----------------------------------------------------------------------
# environments = {
#   eu-prod-1 = { cluster = "https://eu1.example", domain = "eu1.example.com", namespace = "prod",  replicas = 5 }
#   us-prod-1 = { cluster = "https://us1.example", domain = "us1.example.com", namespace = "prod",  replicas = 5 }
#   eu-dev-1  = { cluster = "https://dev.example", domain = "dev.example.com", namespace = "dev",   replicas = 1 }
#   ...
# }
#
# services = {
#   frontend = { image_repo = "registry.example.com/frontend", image_tag = "1.2.3", port = 8080, resources = { cpu = "500m", memory = "512Mi" } }
#   api      = { image_repo = "registry.example.com/api",      image_tag = "4.5.6", port = 9000, resources = { cpu = "1",    memory = "1Gi"  } }
#   ...
# }
