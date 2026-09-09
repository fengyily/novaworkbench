# 发版手册（Release）

NovaWorkbench 的 Release 由 [`.github/workflows/release.yml`](../.github/workflows/release.yml) 驱动：推送一个 `v*` tag 即自动完成 **5 平台二进制构建 → 打包 + 校验和 → 多架构 Docker 镜像 → GitHub Release** 全流程。与 `deploy.yml`（main / feat 分支的镜像 + 部署）完全解耦，互不影响。

## 产物清单

每次发版产出：

| 产物 | 说明 |
|------|------|
| `novaworkbench-<ver>-{darwin-arm64, darwin-amd64, linux-amd64, linux-arm64}.tar.gz` | macOS / Linux 单二进制（前端已嵌入） |
| `novaworkbench-<ver>-windows-amd64.zip` | Windows 单二进制 |
| `novaworkbench-<ver>-checksums.txt` | 全部压缩包的 SHA-256 校验和 |
| `ghcr.io/<owner>/nova:<ver>`（+ `sha-<sha>`，稳定版另有 `latest`） | linux/amd64 + linux/arm64 多架构镜像 |
| GitHub Release | 上述附件 + 自动生成的 changelog（`--generate-notes`） |

版本号通过 `go build -ldflags -X .../internal/version.Version=<ver>` 注入，`GET /api/health` 的 `version` 字段即构建版本（本地 `make build` 不注入时显示 `dev`）。

## 版本号约定

- 遵循 [semver](https://semver.org/)，tag 统一带 `v` 前缀：`v0.2.0`、`v1.0.0-rc1`。
- tag **只从 `main` 分支打**。在 `feat/*` 分支上打 tag 会同时触发 `deploy.yml` 的 preview 部署（允许但不推荐）。
- 预发布后缀（`-rc1`、`-test` 等）**不会**移动 Docker 的 `latest` 指针，只有干净的 `vX.Y.Z` 会。

## 发版步骤

1. 确认 `main` 分支 CI 绿。
2. 决定版本号（semver，`v` 前缀）。
3. 打 tag 并推送：

   ```bash
   git checkout main && git pull
   git tag v0.2.0
   git push origin v0.2.0
   ```

4. `release.yml` 自动触发：构建 → 启动二进制校验 `/api/health` 版本 → 打包 → 推镜像 → 创建 GitHub Release。
5. 在 Release 页面补充 changelog（`--generate-notes` 已自动汇总 PR 列表）。

## 不打 tag 的演练（workflow_dispatch）

在 GitHub 仓库 → **Actions → Release → Run workflow**，输入任意 tag 名（例如 `v0.0.0-test`）：

- 用于端到端验证流水线而不发正式版——tag **不需要**预先创建，workflow 会 checkout 默认分支 HEAD，把输入值当作版本号 stamp 到二进制 / 镜像 / Release。
- `-test` / `-rc1` 等带 `-` 后缀的版本**不会**移动 Docker `latest` 指针。
- 同一 tag 重复运行受 `concurrency` 锁串行化；已存在的 Release 会被删除重建（不修改 tag 本身）。

## 回滚 / 清理

- **误发 tag**：`git tag -d v0.2.0 && git push origin :refs/tags/v0.2.0`。
  注意：CI 不会自动回滚已发布的产物——
  - GitHub Release：`gh release delete v0.2.0 --yes`
  - GHCR 镜像：`gh api -X DELETE /user/packages/container/nova/versions/<id>`（或在 GitHub → Packages 页面删除）
- **重跑**：删除 Release 后重推同一 tag，或直接用 workflow_dispatch 重放（见上）。

## 本地复刻构建

```bash
# 与 CI 等价的本地构建（5 平台 + 版本注入）
VERSION=v0.2.0-rc1 scripts/build-all.sh --with-frontend --skip-deps-check

# 仅本机单二进制
VERSION=v0.2.0 make build
./dist/nova   # /api/health 报告 v0.2.0
```
