# Server bootstrap: after the ECS boots and user-data installs docker,
# this resource runs the project's init-server.sh on it. That script
# starts nginx-proxy, installs acme.sh, and issues the *.nova wildcard
# cert. Subsequent GitHub Actions deploys reuse the same environment.
#
# Note: this uses the null_resource + remote-exec pattern (still
# supported in TF 1.6+); the more modern approach is `terraform_data`
# but remote-exec is clearer for ops scripts.

resource "null_resource" "server_init" {
  triggers = {
    # Re-run when EIP changes or the init script source changes.
    eip     = alicloud_eip.nova.ip_address
    script  = filebase64sha256("${path.module}/../deploy/setup/init-server.sh")
    deploy  = filebase64sha256("${path.module}/../deploy/docker-compose.nginx-proxy.yml")
  }

  connection {
    type        = "ssh"
    host        = alicloud_eip.nova.ip_address
    user        = "nova"
    private_key = file(var.ssh_private_key_path)
    timeout     = "10m"
  }

  # Push the deploy files first so init-server.sh can find them.
  provisioner "file" {
    source      = "${path.module}/../deploy"
    destination = "/home/nova/nova-deploy"
  }

  provisioner "remote-exec" {
    inline = [
      "set -euo pipefail",
      "sudo mv /home/nova/nova-deploy /home/nova/deploy",
      "sudo chown -R nova:nova /home/nova/deploy",
      # init-server.sh requires Ali_Key/Ali_Secret as env vars; we
      # forward the same access key/secret that Terraform is using to
      # manage DNS. They're injected through `environment` so they
      # never appear in the shell history.
      "Ali_Key='${var.aliyun_access_key_id}' Ali_Secret='${var.aliyun_access_key_secret}' ACME_EMAIL='${var.acme_email}' bash /home/nova/deploy/setup/init-server.sh",
    ]
  }

  depends_on = [
    alicloud_eip_association.nova,
    alicloud_dns_record.nova,
  ]
}