terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 5.0"
    }
    acme = {
      source = "registry.terraform.io/acme-corp/acme"
    }
    private = {
      source = "registry.example.com/corp/private"
    }
    legacy = "~> 2.0"
  }
}
