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
  # Wire the sensitive vars (TF_VAR_aliyun_access_key_id /
  # _secret in CI) into the provider explicitly. The alicloud
  # provider's default credential chain only auto-reads
  # ALICLOUD_ACCESS_KEY / ALICLOUD_SECRET_KEY env vars, which the
  # GitHub Actions workflow does NOT set — without these arguments
  # it errors with "no valid credential sources".
  access_key = var.aliyun_access_key_id
  secret_key = var.aliyun_access_key_secret
}