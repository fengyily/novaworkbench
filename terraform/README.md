# Terraform — one-shot infrastructure provisioning

Spin up the entire NovaWorkbench hosting stack with a single `terraform apply`:

- Alibaba Cloud VPC + VSwitch + Security Group
- Ubuntu 22.04 ECS instance + EIP
- `nova` user with passwordless sudo + docker group membership
- Docker Engine + Compose v2 (installed by user-data on first boot)
- Aliyun DNS A records for `*.nova.yishield.com` and `prod.nova.yishield.com`
- nginx-proxy + acme.sh + `*.nova.yishield.com` wildcard cert (via remote-exec)

After `apply`, subsequent deploys are handled by the GitHub Actions
`deploy.yml` workflow (build → push → SSH to the server's EIP).

## Required secrets

The Terraform workflow reads these from **GitHub Actions Secrets**:

| Secret                  | Used by                                |
|-------------------------|----------------------------------------|
| `ALI_ACCESS_KEY_ID`     | Aliyun provider + acme.sh DNS-01       |
| `ALI_ACCESS_KEY_SECRET` | Aliyun provider + acme.sh DNS-01       |
| `TERRAFORM_SSH_KEY`     | SSH private key (ed25519) used by remote-exec and by the GitHub deploy workflow |

The SSH keypair pair (public in `terraform.tfvars`, private in `TERRAFORM_SSH_KEY`) must match. To generate:

```bash
ssh-keygen -t ed25519 -f ~/.ssh/nova -N ""
# ~/.ssh/nova       → add to GitHub secret TERRAFORM_SSH_KEY
# ~/.ssh/nova.pub   → put into terraform/terraform.tfvars as ssh_public_key
```

## Required Variables

| Variable         | Description                                           |
|------------------|-------------------------------------------------------|
| `ssh_public_key` | Public key matching `TERRAFORM_SSH_KEY`                |
| `region`         | Aliyun region (default `cn-hangzhou`)                 |

Other vars (`instance_type`, `vpc_cidr`, `vswitch_cidr`, `dns_domain`,
`dns_subdomain`, `dns_records`, `acme_email`, `ssh_port`) have sensible
defaults — override in `terraform.tfvars` if needed.

## Usage

### Manual (once, then automate)

```bash
cd terraform
terraform init
terraform plan  -var-file=terraform.tfvars
terraform apply -var-file=terraform.tfvars -auto-approve
```

`terraform.tfvars` (NOT committed, kept locally or in OSS):

```hcl
ssh_public_key = "ssh-ed25519 AAAA... user@host"

# The next two are usually supplied via TF_VAR_* env vars:
#   TF_VAR_aliyun_access_key_id
#   TF_VAR_aliyun_access_key_secret
# But you can also put them here for local development.

ssh_private_key_path = "~/.ssh/nova"
```

### Via GitHub Actions

1. Set `ALI_ACCESS_KEY_ID`, `ALI_ACCESS_KEY_SECRET`, `TERRAFORM_SSH_KEY`
   Secrets.
2. Open Actions → "Terraform" → Run workflow:
   - **Action**: `plan` (default, no side effects) or `apply` / `destroy`
   - **Auto approve**: `false` (default) or `true` (skips confirmation)

The workflow bootstraps an OSS-backed state file on first run so subsequent
applies share state across team members. If you'd rather keep state local,
delete the OSS backend block in `.github/workflows/terraform.yml`.

## Outputs (printed after apply)

```
server_public_ip   = "47.91.xx.xx"     # GitHub Variable PROD_SERVER_HOST
ssh_user           = "nova"
dns_records        = {
  "*.nova"     = "47.91.xx.xx"
  "nova"       = "47.91.xx.xx"
  "prod.nova"  = "47.91.xx.xx"
}
```

After the first successful `apply`, copy `server_public_ip` into GitHub
Variable `PROD_SERVER_HOST` and `ssh_user` into `PROD_SERVER_USER`. From
that point on, the deploy workflow can run entirely unattended.

## Destroy

```bash
terraform destroy -var-file=terraform.tfvars -auto-approve
```

Tears down ECS + EIP + DNS records + Security Group + VPC. Does NOT
delete Aliyun DNS domain registration.