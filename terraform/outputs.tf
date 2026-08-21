output "server_public_ip" {
  description = "Public IPv4 the records point at"
  value       = var.server_public_ip
}

output "dns_records" {
  description = "DNS A records Terraform manages"
  value = {
    for k, v in alicloud_dns_record.nova :
    k => v.value
  }
}

output "apex_domain" {
  description = "Apex domain the records live under"
  value       = var.dns_domain
}