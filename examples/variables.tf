variable "app_version" {
  description = "Application version"
  type        = string
  default     = "1.0.0"
}

variable "replicas" {
  description = "Number of replicas"
  type        = number
  default     = 3
}

variable "db_hosts" {
  description = "Database hosts per environment"
  type        = map(string)
  default = {
    dev     = "dev-db.example.com"
    staging = "staging-db.example.com"
    prod    = "prod-db.example.com"
  }
}

variable "db_port" {
  description = "Database port"
  type        = number
  default     = 5432
}
