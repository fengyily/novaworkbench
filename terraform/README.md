# Terraform — Aliyun DNS only

Manages the `*.nova.yishield.com` and `prod.nova.yishield.com` A records
on Aliyun DNS. The ECS itself is provisioned manually (or out-of-band
of this repo) — Terraform only owns the records.

After Terraform is applied once, the GitHub Actions `deploy.yml` flow
takes over: every push to `prod` or `feat/**` builds a new image,
pushes it to GHCR, and SSH-deploys it to the same EIP you give
Terraform here.

## Required secrets (GitHub Actions Secrets)

| Secret                  | Used by                                |
|-------------------------|----------------------------------------|
| `ALI_ACCESS_KEY_ID`     | Aliyun provider (DNS management)       |
| `ALI_ACCESS_KEY_SECRET` | Aliyun provider (DNS management)       |

These are the same AK/SK that acme.sh uses on the server to sign
the wildcard cert via DNS-01. Grant them only `AliyunDNSFullAccess`
to keep the blast radius small.

## Required Variables

Supply `server_public_ip` either as the workflow's **Server IP** dispatch
input (see below) or via the `SERVER_PUBLIC_IP` repo Actions variable; the
input wins when both are set. Pre-populate a local `terraform.tfvars` for
manual runs.

| Variable           | Description                                               |
|--------------------|-----------------------------------------------------------|
| `server_public_ip` | Public IPv4 of the ECS hosting the NovaWorkbench stack   |
| `region`           | Aliyun region (default `cn-hangzhou`)                    |

Other vars (`dns_domain`, `dns_subdomain`, `dns_records`, `ttl`) have
sensible defaults — override in `terraform.tfvars` if needed.

## Usage

### Via GitHub Actions

1. Set `ALI_ACCESS_KEY_ID` and `ALI_ACCESS_KEY_SECRET` Secrets.
2. Open Actions → "Terraform" → Run workflow:
   - **Action**: `plan` (default, no side effects) or `apply` / `destroy`
   - **Auto approve**: `false` (default) or `true`
   - **Server IP**: your ECS's public IPv4 (e.g. `47.91.xx.xx`)

The workflow injects the AK/SK as `TF_VAR_aliyun_access_key_id` /
`TF_VAR_aliyun_access_key_secret` Secrets and `server_public_ip` as the
`TF_VAR_server_public_ip` env var. `server_public_ip` is taken from the
**Server IP** dispatch input, falling back to the `SERVER_PUBLIC_IP` repo
Actions variable when the input is left empty. The workflow fails fast
with a clear error if neither is set (Aliyun's `AddDomainRecord` rejects an
empty `Value`).

### Manual (once, then automate)

```bash
cd terraform
terraform init
terraform plan  -var-file=terraform.tfvars
terraform apply -var-file=terraform.tfvars -auto-approve
```

`terraform.tfvars` (NOT committed, kept locally):

```hcl
server_public_ip = "47.91.xx.xx"

# The next two are usually supplied via TF_VAR_* env vars:
#   TF_VAR_aliyun_access_key_id
#   TF_VAR_aliyun_access_key_secret
```

## Outputs (printed after apply)

```
server_public_ip   = "47.91.xx.xx"
apex_domain        = "yishield.com"
dns_records        = {
  "nova"       = "47.91.xx.xx"
  "prod.nova"  = "47.91.xx.xx"
  "*.nova"     = "47.91.xx.xx"
}
```

## Destroy

```bash
terraform destroy -var-file=terraform.tfvars -auto-approve
```

Removes the A records. Does NOT delete the Aliyun DNS domain registration
itself, and does NOT touch the ECS.