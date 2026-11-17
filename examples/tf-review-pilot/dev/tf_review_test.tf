# Pilot-only module call. Open the MR, check the agent comments, then close it without merging.
module "tf_review_test" {
  source = "../modules/tf_review_test" # adjust to the real relative path

  project_id = "jit-j-flow-dev" # or the existing project variable, e.g. var.project_id
  name       = "jit-j-flow-dev-tf-review-test"
}
