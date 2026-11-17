resource "google_compute_instance" "m" {
  metadata_startup_script = <<-EOT
    echo hi
    echo bye
  EOT
  name = "n"
  labels = {
    env  = "dev"
    team = "x"
  }
}
