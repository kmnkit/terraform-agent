# Plan-only unit tests with a mocked provider: no credentials, no real resources.
mock_provider "google" {}

variables {
  project_id = "test-project"
  name       = "tf-review-test-bucket"
}

run "creates_bucket_from_inputs" {
  command = plan

  assert {
    condition     = google_storage_bucket.this.name == "tf-review-test-bucket"
    error_message = "Bucket name must come from var.name."
  }

  assert {
    condition     = google_storage_bucket.this.project == "test-project"
    error_message = "Bucket must be created in var.project_id."
  }

  assert {
    condition     = google_storage_bucket.this.location == "ASIA-NORTHEAST1"
    error_message = "Default location must be ASIA-NORTHEAST1."
  }

  assert {
    condition     = google_storage_bucket.this.labels["purpose"] == "tf-review-pilot"
    error_message = "Bucket must carry the purpose=tf-review-pilot label."
  }

  assert {
    condition     = google_storage_bucket.this.force_destroy == true
    error_message = "force_destroy must be true so the pilot bucket can be removed."
  }
}

run "overrides_location" {
  command = plan

  variables {
    location = "US"
  }

  assert {
    condition     = google_storage_bucket.this.location == "US"
    error_message = "location must follow var.location."
  }
}

run "rejects_uppercase_name" {
  command = plan

  variables {
    name = "Invalid_Bucket"
  }

  expect_failures = [var.name]
}

run "rejects_too_short_name" {
  command = plan

  variables {
    name = "ab"
  }

  expect_failures = [var.name]
}
