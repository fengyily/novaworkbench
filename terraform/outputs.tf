output "server_public_ip" {
  description = "Public IPv4 address of the NovaWorkbench ECS — set this as GitHub Variable PROD_SERVER_HOST"
  value       = alicloud_eip.nova.ip_address
}

output "ssh_user" {
  description = "SSH user to use for GitHub Actions deploys"
  value       = "nova"
}

output "dns_records" {
  description = "DNS A records Terraform manages"
  value = {
    for k, v in alicloud_dns_record.nova :
    k => v.value
  }
}

output "wildcard_cert_status" {
  description = "Always 'managed by init-server.sh' — placeholder so the workflow can sanity-check"
  value       = "issued via acme.sh on the ECS at boot"
}

output "terraform_state" {
  description = "Path of the local state file"
  value       = "terraform.tfstate (commit to a private backend, e.g. OSS)"
}