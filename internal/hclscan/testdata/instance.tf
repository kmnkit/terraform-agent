resource "google_compute_instance" "web" {
  name         = "web"
  machine_type = "e2-medium"
  provider     = google-beta

  network_interface {
    network = "default"

    access_config {
      nat_ip = "1.2.3.4"
    }
  }
}

resource "google_storage_bucket" "logs" {
  name     = "logs"
  location = "US"
}

module "vpc" {
  source  = "terraform-google-modules/network/google"
  version = "~> 9.0"
}

module "dyn" {
  source = var.src
}
