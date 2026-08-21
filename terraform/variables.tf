variable "region" {
  description = "Alibaba Cloud region (e.g. cn-hangzhou)"
  type        = string
  default     = "cn-hangzhou"
}

variable "zone_id" {
  description = "Availability zone letter suffix (a/b/c) within the region"
  type        = string
  default     = "a"
}

variable "vpc_cidr" {
  description = "CIDR block for the NovaWorkbench VPC"
  type        = string
  default     = "172.16.0.0/16"
}

variable "vswitch_cidr" {
  description = "CIDR block for the ECS VSwitch"
  type        = string
  default     = "172.16.1.0/24"
}

variable "instance_type" {
  description = "ECS instance type — adjust for prod vs preview workloads"
  type        = string
  default     = "ecs.g6.large"
}

variable "ssh_public_key" {
  description = "Public key (ssh-rsa AAAA...) pushed as the only authorized key for the deploy user"
  type        = string
}

variable "ssh_port" {
  description = "SSH port to open in the security group"
  type        = number
  default     = 22
}

variable "dns_domain" {
  description = "Apex domain hosted on Aliyun DNS (must already be a registered domain in the account)"
  type        = string
  default     = "yishield.com"
}

variable "dns_subdomain" {
  description = "Sub-prefix that scopes *.nova.{domain} (e.g. 'nova' → *.nova.yishield.com)"
  type        = string
  default     = "nova"
}

# Set to true to also issue a prod-nova.{domain} A record (separate
# wildcard cert isn't needed — both *.nova and prod.nova are covered
# by the same cert).
variable "dns_records" {
  description = "List of hostnames (relative to dns_domain) to A-record to the server's EIP"
  type        = list(string)
  default     = ["nova", "prod.nova", "*.nova"]
}

variable "aliyun_dns_rr_separator" {
  description = "Aliyun DNS rr separator ('.' for subdomains, '@' for apex)"
  type        = string
  default     = "."
}

variable "acme_email" {
  description = "Email used to register the Let's Encrypt account (acme.sh)"
  type        = string
  default     = "admin@yishield.com"
}

variable "tags" {
  description = "Common resource tags"
  type        = map(string)
  default = {
    project = "novaworkbench"
    env     = "shared"
    managed = "terraform"
  }
}

# Required by server-init.tf to run init-server.sh remotely.
# Provide via env / tfvars file, never commit to git.
variable "ssh_private_key_path" {
  description = "Filesystem path to the deploy SSH private key (matches ssh_public_key)"
  type        = string
}

variable "aliyun_access_key_id" {
  description = "Aliyun AccessKey ID — used by both Terraform and acme.sh on the server"
  type        = string
  sensitive   = true
}

variable "aliyun_access_key_secret" {
  description = "Aliyun AccessKey Secret"
  type        = string
  sensitive   = true
}