# Aliyun DNS records. The apex domain (var.dns_domain) must already
# exist in the account and be pointed at Aliyun's DNS servers;
# Terraform only manages A-record entries under it.
#
# Uses the v1.85+ replacement resources (alicloud_alidns_*), which
# supersede the deprecated alicloud_dns_record / alicloud_dns_domains.

# Validate the domain exists and pull its real domain_id. The record
# resource itself only needs var.dns_domain, but the data source is
# handy if you want to fail-fast on a typo.
data "alicloud_alidns_domains" "this" {
  key_word       = var.dns_domain
  enable_details = true
}

locals {
  # domains[].domain_id is the real ID; domains[].id is just the name.
  domain_id = data.alicloud_alidns_domains.this.domains[0].domain_id

  dns_entries = {
    for entry in var.dns_records :
    entry => {
      rr = entry
    }
  }
}

resource "alicloud_alidns_record" "nova" {
  for_each = local.dns_entries

  domain_name = var.dns_domain # apex, not the subdomain
  rr          = each.value.rr  # host label (e.g. "nova", "*.nova", "prod.nova")
  type        = "A"
  value       = var.server_public_ip
  ttl         = var.ttl
  line        = "default"
  status      = "ENABLE"
  # `domain_id` is NOT a resource argument — the resource looks up
  # the zone by `domain_name`. `priority` is only needed when type == "MX".
}