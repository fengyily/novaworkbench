# Aliyun DNS records. The domain (var.dns_domain) must already exist in
# the account and be pointed at Aliyun's DNS servers; Terraform only
# manages A-record entries under it.

data "alicloud_dns_domains" "this" {
  domain_name = var.dns_domain
  status      = "ENABLE"
}

locals {
  domain_id = data.alicloud_dns_domains.this.domains[0].id

  # Map our friendly names to (rr, type) tuples expected by the dns_record
  # resource. *.nova is fine as an Aliyun wildcard A record.
  dns_entries = {
    for entry in var.dns_records :
    entry => {
      rr  = entry
      type = "A"
    }
  }
}

resource "alicloud_dns_record" "nova" {
  for_each = local.dns_entries

  name        = var.dns_domain
  host_record = each.value.rr
  type        = each.value.type
  value       = alicloud_eip.nova.ip_address
  ttl         = 600
  priority    = 0
  line        = "default"
  status      = "ENABLE"
  domain_id   = local.domain_id
}