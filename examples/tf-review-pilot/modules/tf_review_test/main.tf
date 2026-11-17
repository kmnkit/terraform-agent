resource "google_storage_bucket" "this" {
  project       = var.project_id
  name          = var.name
  location      = var.location
  force_destroy = true

  labels = {
    purpose = "tf-review-pilot"
  }
}
