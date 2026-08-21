# NovaWorkbench infrastructure — single Alibaba Cloud region.
# Run plan / apply via the GitHub Actions "Terraform" workflow, or locally:
#   terraform init
#   terraform plan  -var-file=secrets.auto.tfvars
#   terraform apply -var-file=secrets.auto.tfvars

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

# Latest Ubuntu 22.04 LTS image, refreshed every apply so a fresh
# security patch becomes the new default.
data "alicloud_images" "ubuntu_22_04" {
  name_regex  = "^ubuntu_22_04_x64"
  most_recent = true
  owners      = "system"
  status      = "Available"
}