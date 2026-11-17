terraform {
  required_providers {
    google = {
      source                = "acme-corp/google"
      version               = "~> 6.0"
      configuration_aliases = [google.west, google.east]
    }
    loc = {
      source = "localhost:5000/ns/name"
    }
    ex = {
      source = "example.com/ns/name"
    }
    pub = {
      source = "registry.terraform.io/hashicorp/google"
    }
    hc = {
      source = "hashicorp/google"
    }
  }
}
