variable "region" {
  description = "Alibaba Cloud region where the domain is hosted (e.g. cn-hangzhou)"
  type        = string
  default     = "cn-hangzhou"
}

variable "dns_domain" {
  description = "Apex domain hosted on Aliyun DNS (must already be a registered domain in the account)"
  type        = string
  default     = "yishield.com"
}

variable "dns_subdomain" {
  description = "Sub-prefix that scopes *.nova.{domain} (informational only — record names below are explicit)"
  type        = string
  default     = "nova"
}

variable "server_public_ip" {
  description = "Public IPv4 of the ECS that should receive *.nova and prod.nova traffic"
  type        = string
}

variable "dns_records" {
  description = "List of hostnames (relative to dns_domain) to A-record to server_public_ip"
  type        = list(string)
  default = [
    "nova",
    "prod.nova",
    "*.nova",
  ]
}

variable "ttl" {
  description = "DNS TTL in seconds"
  type        = number
  default     = 600
}

variable "tags" {
  description = "Common resource tags"
  type        = map(string)
  default = {
    project = "novaworkbench"
    managed = "terraform"
  }
}

# Required by the alicloud provider. Pass via TF_VAR_* env vars
# (GitHub Actions Secrets map directly), never commit.
variable "aliyun_access_key_id" {
  description = "Aliyun AccessKey ID"
  type        = string
  sensitive   = true
}

variable "aliyun_access_key_secret" {
  description = "Aliyun AccessKey Secret"
  type        = string
  sensitive   = true
}