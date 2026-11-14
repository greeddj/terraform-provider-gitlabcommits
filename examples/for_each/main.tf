# This configuration puts the files of many services and environments on one
# branch, for example 20 services in 30 environments.

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
  type      = string
  sensitive = true
}

variable "gitops_project" {
  description = "The path of the GitLab project, for example \"platform/gitops\". The Argo CD repoURL also uses it."
  type        = string
}

variable "gitops_branch" {
  description = "The branch that the provider commits to."
  type        = string
  default     = "main"
}

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

# Each service has one resource with the files of all environments. A create,
# an update or a destroy of a resource makes at most one commit.
resource "gitlabcommits_files" "service" {
  for_each = var.services

  # A change to var.gitops_project or var.gitops_branch replaces every resource.
  # The destroy can make a delete commit before the create. If the new branch
  # does not exist, the create fails. See the resource page, Caveats, Replacement.
  project_id     = var.gitops_project
  branch         = var.gitops_branch
  commit_message = "chore(${each.key}): sync gitops manifests via terraform"

  author_name  = "terraform-bot"
  author_email = "terraform@example.com"

  files = merge(
    # Helm values for each environment.
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

    # An Argo CD Application for each environment.
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

output "service_commits" {
  description = "The SHA of the last commit that each resource made. The keys are the service names. Each SHA is null until its resource makes a commit, and again after a replacement that makes no commit."
  value       = { for k, r in gitlabcommits_files.service : k => r.commit_sha }
}

# Example values for a .tfvars file:
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
