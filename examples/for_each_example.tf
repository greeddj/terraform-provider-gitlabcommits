terraform {
  required_providers {
    gitlabcommits = {
      source = "greeddj/gitlab-commits"
    }
  }
}

provider "gitlabcommits" {
  token = var.gitlab_token
}

variable "gitlab_token" {
  description = "GitLab Personal Access Token / CI Job Token with push_repository permission"
  type        = string
  sensitive   = true
}

# ========================================
# Example 1: for_each with automatic batching
# ========================================

locals {
  config_files = {
    "app" = {
      name    = "myapp"
      version = "1.0.0"
      port    = 8080
    }
    "database" = {
      host = "localhost"
      port = 5432
      name = "mydb"
    }
    "cache" = {
      host = "redis"
      port = 6379
    }
  }
}

# All these files will be combined into ONE commit!
resource "gitlabcommits_file" "configs" {
  for_each = local.config_files

  project_id     = "my-group/my-project"
  branch         = "main"
  file_path      = "config/${each.key}.yaml"
  commit_message = "Update configuration files via Terraform"
  batch_mode     = true  # Enabled by default

  content = yamlencode({
    config = each.value
  })

  author_name  = "Terraform"
  author_email = "terraform@example.com"
}

# ========================================
# Example 2: Generating files from a list
# ========================================

variable "environments" {
  type = list(object({
    name     = string
    db_host  = string
    replicas = number
  }))
  default = [
    { name = "dev", db_host = "dev-db.example.com", replicas = 1 },
    { name = "staging", db_host = "staging-db.example.com", replicas = 2 },
    { name = "prod", db_host = "prod-db.example.com", replicas = 3 }
  ]
}

# All environment files in ONE commit
resource "gitlabcommits_file" "env_configs" {
  for_each = { for env in var.environments : env.name => env }

  project_id     = "my-group/my-project"
  branch         = "main"
  file_path      = "deploy/${each.key}/config.yaml"
  commit_message = "Generate environment configurations"
  batch_mode     = true

  content = templatefile("${path.module}/templates/env-config.yaml.tpl", {
    environment = each.key
    db_host     = each.value.db_host
    replicas    = each.value.replicas
  })
}

# ========================================
# Example 3: Working with templates
# ========================================

locals {
  services = ["api", "worker", "scheduler"]

  service_configs = {
    for service in local.services : service => {
      image    = "myapp/${service}"
      port     = service == "api" ? 8080 : 0
      replicas = service == "api" ? 3 : 1
    }
  }
}

# All services in one commit
resource "gitlabcommits_file" "k8s_deployments" {
  for_each = local.service_configs

  project_id     = "my-group/my-project"
  branch         = "main"
  file_path      = "k8s/${each.key}/deployment.yaml"
  commit_message = "Deploy services configuration"
  batch_mode     = true

  content = templatefile("${path.module}/templates/deployment.yaml.tpl", {
    service_name = each.key
    image        = each.value.image
    port         = each.value.port
    replicas     = each.value.replicas
  })
}

# ========================================
# Example 4: Mixed operations (create + update + delete)
# ========================================

locals {
  managed_files = {
    "keep_this.yaml" = {
      action  = "update"
      content = "existing: file"
    }
    "new_file.yaml" = {
      action  = "create"
      content = "brand: new"
    }
  }
}

resource "gitlabcommits_file" "managed" {
  for_each = local.managed_files

  project_id     = "my-group/my-project"
  branch         = "main"
  file_path      = "managed/${each.key}"
  action         = each.value.action
  commit_message = "Manage repository files"
  batch_mode     = true

  content = each.value.content
}

# ========================================
# Example 5: Disabling batching (if needed)
# ========================================

# This file will create a SEPARATE commit
resource "gitlabcommits_file" "important_standalone" {
  project_id     = "my-group/my-project"
  branch         = "main"
  file_path      = "IMPORTANT.md"
  commit_message = "Critical update - separate commit"
  batch_mode     = false  # Disable batching

  content = "This change needs its own commit"
}

# ========================================
# Example 6: Dynamic generation from map
# ========================================

variable "feature_flags" {
  type = map(object({
    enabled     = bool
    description = string
  }))
  default = {
    "new_ui" = {
      enabled     = true
      description = "New user interface"
    }
    "beta_api" = {
      enabled     = false
      description = "Beta API endpoints"
    }
  }
}

resource "gitlabcommits_file" "feature_flags" {
  for_each = var.feature_flags

  project_id     = "my-group/my-project"
  branch         = "main"
  file_path      = "features/${each.key}.json"
  commit_message = "Update feature flags configuration"
  batch_mode     = true

  content = jsonencode({
    name        = each.key
    enabled     = each.value.enabled
    description = each.value.description
    updated_at  = timestamp()
  })
}

# Outputs for tracking
output "batched_commit_sha" {
  description = "SHA of batched commit (all config files have the same SHA)"
  value       = [for f in gitlabcommits_file.configs : f.commit_sha][0]
}

output "all_files_created" {
  description = "List of all created files"
  value       = [for k, f in gitlabcommits_file.configs : f.file_path]
}
