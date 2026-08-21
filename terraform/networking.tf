# Networking — VPC, VSwitch, Security Group.
# Created once and left in place; ECS / EIP / DNS records are owned by
# other modules.

resource "alicloud_vpc" "nova" {
  cidr_block = var.vpc_cidr
  vpc_name   = "nova-vpc"
  tags       = var.tags
}

resource "alicloud_vswitch" "nova" {
  cidr_block        = var.vswitch_cidr
  vpc_id            = alicloud_vpc.nova.id
  availability_zone = "${var.region}${var.zone_id}"
  vswitch_name      = "nova-vswitch"
  tags              = var.tags
}

resource "alicloud_security_group" "nova" {
  name        = "nova-sg"
  vpc_id      = alicloud_vpc.nova.id
  description = "NovaWorkbench edge security group"
  tags        = var.tags
}

# SSH — restrict via CIDR if you have a stable office / VPN range.
resource "alicloud_security_group_rule" "ssh" {
  type              = "ingress"
  ip_protocol       = "tcp"
  port_range        = "${var.ssh_port}/${var.ssh_port}"
  security_group_id = alicloud_security_group.nova.id
  cidr_ip           = "0.0.0.0/0"
}

# HTTP / HTTPS — open to the world, nginx-proxy handles TLS termination.
resource "alicloud_security_group_rule" "http" {
  type              = "ingress"
  ip_protocol       = "tcp"
  port_range        = "80/80"
  security_group_id = alicloud_security_group.nova.id
  cidr_ip           = "0.0.0.0/0"
}

resource "alicloud_security_group_rule" "https" {
  type              = "ingress"
  ip_protocol       = "tcp"
  port_range        = "443/443"
  security_group_id = alicloud_security_group.nova.id
  cidr_ip           = "0.0.0.0/0"
}

resource "alicloud_security_group_rule" "egress_all" {
  type              = "egress"
  ip_protocol       = "all"
  port_range        = "-1/-1"
  security_group_id = alicloud_security_group.nova.id
  cidr_ip           = "0.0.0.0/0"
}