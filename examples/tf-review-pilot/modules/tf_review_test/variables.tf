variable "project_id" {
  description = "Google Cloud project ID where the bucket is created."
  type        = string
}

variable "name" {
  description = "Globally unique bucket name (3-63 chars, lowercase letters, digits, hyphens)."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$", var.name))
    error_message = "name must be 3-63 characters of lowercase letters, digits and hyphens, starting and ending with a letter or digit."
  }
}

variable "location" {
  description = "Bucket location."
  type        = string
  default     = "ASIA-NORTHEAST1"
}
