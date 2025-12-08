terraform {
  required_providers {
    gitlabcommits = {
      source = "greeddj/gitlab-commits"
    }
  }
}

provider "gitlabcommits" {
  token    = var.gitlab_token
  # base_url = "https://gitlab.example.com" # for self-hosted instances
}

variable "gitlab_token" {
  description = "GitLab Personal Access Token"
  type        = string
  sensitive   = true
}

# Example 1: Creating multiple files in a single commit
resource "gitlabcommits_commit" "example_create_files" {
  project_id     = "my-group/my-project"
  branch         = "main"
  commit_message = "Add multiple configuration files"

  author_name  = "Terraform"
  author_email = "terraform@example.com"

  files = [
    {
      file_path = "config/app.yaml"
      action    = "create"
      content   = <<-EOT
        app:
          name: myapp
          version: 1.0.0
          port: 8080
      EOT
    },
    {
      file_path = "config/database.yaml"
      action    = "create"
      content   = <<-EOT
        database:
          host: localhost
          port: 5432
          name: mydb
      EOT
    },
    {
      file_path = "config/secrets.yaml"
      action    = "create"
      content   = <<-EOT
        secrets:
          api_key: ${random_password.api_key.result}
      EOT
    }
  ]
}

# Example 2: Updating existing files
resource "gitlabcommits_commit" "example_update_files" {
  project_id     = "my-group/my-project"
  branch         = "develop"
  commit_message = "Update deployment configurations"

  files = [
    {
      file_path = "deploy/k8s/deployment.yaml"
      action    = "update"
      content   = templatefile("${path.module}/templates/deployment.yaml.tpl", {
        image_tag = var.app_version
        replicas  = var.replicas
      })
    },
    {
      file_path = "deploy/k8s/service.yaml"
      action    = "update"
      content   = file("${path.module}/k8s/service.yaml")
    }
  ]
}

# Example 3: Working with binary files via base64
resource "gitlabcommits_commit" "example_binary_files" {
  project_id     = "my-group/my-project"
  branch         = "main"
  commit_message = "Add binary assets"

  files = [
    {
      file_path      = "assets/logo.png"
      action         = "create"
      content_base64 = filebase64("${path.module}/assets/logo.png")
      encoding       = "base64"
    }
  ]
}

# Example 4: Complex scenario - create, update and delete
resource "gitlabcommits_commit" "example_mixed_actions" {
  project_id     = "my-group/my-project"
  branch         = "feature/new-config"
  commit_message = "Restructure configuration files"

  files = [
    # Create new file
    {
      file_path = "config/v2/app.yaml"
      action    = "create"
      content   = yamlencode({
        app = {
          name    = "myapp"
          version = "2.0.0"
        }
      })
    },
    # Update existing file
    {
      file_path = "README.md"
      action    = "update"
      content   = "# My Project\n\nUpdated documentation..."
    },
    # Delete old file
    {
      file_path = "config/old-config.yaml"
      action    = "delete"
    }
  ]
}

# Example 5: Generating multiple files from modules
locals {
  environments = ["dev", "staging", "prod"]
}

resource "gitlabcommits_commit" "example_generated_configs" {
  project_id     = "my-group/my-project"
  branch         = "main"
  commit_message = "Generate environment-specific configurations"

  files = [
    for env in local.environments : {
      file_path = "config/${env}/settings.yaml"
      action    = "create"
      content   = templatefile("${path.module}/templates/settings.yaml.tpl", {
        environment = env
        db_host     = var.db_hosts[env]
        db_port     = var.db_port
      })
    }
  ]
}

# Example 6: Using with dynamic data
resource "random_password" "api_key" {
  length  = 32
  special = true
}

resource "gitlabcommits_commit" "example_dynamic_content" {
  project_id     = "my-group/my-project"
  branch         = "main"
  commit_message = "Update API keys"

  files = [
    {
      file_path = ".env"
      action    = "update"
      content   = <<-EOT
        API_KEY=${random_password.api_key.result}
        ENVIRONMENT=production
        VERSION=${var.app_version}
      EOT
    }
  ]
}

# Output for tracking the commit
output "commit_sha" {
  value       = gitlabcommits_commit.example_create_files.commit_sha
  description = "SHA of the created commit"
}
