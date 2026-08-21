# NovaWorkbench DNS-only infrastructure.
# Manages Aliyun DNS A records for *.nova.yishield.com and
# prod.nova.yishield.com. The ECS instance is provisioned manually
# (or out-of-band of this repo) — Terraform only owns the records.
#
# Run via the GitHub Actions "Terraform" workflow, or locally:
#   terraform init
#   terraform plan  -var-file=terraform.tfvars
#   terraform apply -var-file=terraform.tfvars

terraform {
  required_version = ">= 1.6"

  required_providers {
    alicloud = {
      source  = "aliyun/alicloud"
      version = "~> 1.230"
    }
  }
}

provider "alicloud" {
  # Region / credentials come from TF_VAR_* env vars so the same code
  # works locally and inside GitHub Actions.
  region = var.region
  # access_key / secret_key are read from ALICLOUD_ACCESS_KEY / _SECRET
  # env vars by the provider; we don't pin them here so secrets stay
  # out of state.
}