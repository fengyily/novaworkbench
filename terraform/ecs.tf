# ECS instance + EIP.  Destroying the ECS resource will also release
# its EIP (force = true). The instance boots ubuntu 22.04; everything
# else (docker, nginx-proxy, acme.sh) is installed by server-init.tf.

resource "alicloud_instance" "nova" {
  instance_name              = "nova-server"
  instance_type              = var.instance_type
  image_id                   = data.alicloud_images.ubuntu_22_04.images[0].id
  vswitch_id                 = alicloud_vswitch.nova.id
  security_groups            = [alicloud_security_group.nova.id]
  internet_max_bandwidth_out = 5
  system_disk_category       = "cloud_essd"
  system_disk_size           = 40
  key_name                   = alicloud_key_pair.nova.key_name
  instance_charge_type       = "PostPaid"
  user_data                  = base64encode(templatefile("${path.module}/user_data.sh.tftpl", {
    deploy_user = "nova"
  }))

  tags = var.tags

  lifecycle {
    ignore_changes = [
      # image_id can roll forward as new Ubuntu AMIs publish; don't fight it.
      image_id,
    ]
  }
}

# EIP paid by traffic, associated with the ECS primary ENI.
resource "alicloud_eip" "nova" {
  address_name         = "nova-eip"
  internet_charge_type = "PayByTraffic"
  bandwidth            = 5
  description          = "NovaWorkbench edge EIP"
  tags                 = var.tags
}

resource "alicloud_eip_association" "nova" {
  allocation_id = alicloud_eip.nova.id
  instance_id   = alicloud_instance.nova.id
}

# SSH key pair — we register the public key and let Terraform track it.
resource "alicloud_key_pair" "nova" {
  key_pair_name = "nova-deployer"
  public_key    = var.ssh_public_key
  tags         = var.tags
}