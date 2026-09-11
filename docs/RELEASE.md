# 发布流程（Release Please 驱动）

## 流程概览

NovaWorkbench 使用 [Google Release Please](https://github.com/googleapis/release-please)
做自动版本号 + CHANGELOG + git tag；`main` 上的 PR 合入后自动开/更新 Release PR，
维护者按 cadence 合并 Release PR → 自动创建 `vX.Y.Z` tag → 现有
[`.github/workflows/release.yml`](../.github/workflows/release.yml)
仍按 `v*` tag 触发，自动跑全流程（5 平台二进制 + SHA-256 + 多架构 GHCR + GitHub Release）。

两个工作流职责分离：

- **`release-please.yml`**：只负责「版本号 + CHANGELOG + git tag」。
- **`release.yml`**：只负责「5 平台二进制 + SHA-256 + 多架构 GHCR 镜像 + GitHub Release」，
  由 `v*` tag push 触发（伴生模式，零修改）。

版本号唯一真源是 git tag：源代码在编译期由 `release.yml` 的 ldflags 注入
（`-X .../internal/version.Version=<ver>`）；Release Please **不**改
`backend/internal/version/version.go`（`.release-please-config.json` 的
`extra-files: []` 显式声明），避免与 ldflags 形成双写入漂移。

## PR 标题规范

约定式提交（Conventional Commits）：

- `feat:` 新功能 → 触发 minor bump（pre-1.0 阶段）
- `fix:` 缺陷修复 → 当前 pre-1.0 阶段不自动 patch bump（`bump-patch-for-minor-pre-major: false`）
- `BREAKING CHANGE:`（写在 PR body 的 `## 🚨 Breaking Change` 段）→ 触发 major bump
- `chore:` / `docs:` / `refactor:` / `test:` / `build:` → 不触发版本 bump，但会进 CHANGELOG

无前缀的中文标题不会触发版本 bump，CHANGELOG 也不收录。约定式前缀靠约定，不靠机器卡
（本仓库不引入 commitlint / husky / title-校验 bot）；Release Please 解析器对非约定式
提交是宽容的，不识别就跳过，不影响版本号。

## Release PR 的合并节奏

Release Please 在每次 `feat:` / `fix:` / `BREAKING CHANGE:` 合入 `main` 后自动开/更新
一个 Release PR（标题形如 `chore(main): release 0.3.0`），该 PR 仅修改
`.release-please-manifest.json` 与 `CHANGELOG.md`。

**维护者不必在每次 PR 合并后立即合并 Release PR**。建议按以下任一节奏合并：

- **按时间**：每周一次、每两周一次、或每月一次
- **按里程碑**：完成一组功能后合并
- **按 sprint**：与项目管理节奏对齐

Release PR 累计了多个 `feat:` / `fix:` 之后，merge 时 Release Please 只按
**当前队列里的最高优先级变更类型**取一次 bump
（`BREAKING CHANGE:` > `feat:` > `fix:` > `chore:`），不会因为累积了 30 个 `feat:`
就把 minor 跳到 `0.30.0`。也就是说，**版本号的增长速率等于 Release PR 的合并频率**，
而不是 PR 合并到 `main` 的频率。

> 引导 PR 合并后第一个 `feat:` PR 就会触发首个 Release PR，这是预期行为。

### 跳过 Release PR

- **本次不发版**：直接 Close 当前 Release PR；下次有变更进来时 Release Please 会重新开。
- **本组合并无 bump-worthy 变更**：例如只有 `docs:` / `refactor:` / `chore(build):`，
  Release Please 仍会开/更新 Release PR 仅修改 CHANGELOG；可推迟合并直到下次发版窗口。
- **预演 / 修复 manifest 漂移**：用 `Actions → Release Please → Run workflow` 手动触发
  一次重新评估，不会覆盖 manifest。

## 仓库设置要求

- **Settings → General → Pull Requests → "Default commit message for squash merges"**
  必须设为 **"Pull request title and description"**（不是默认的 "Pull request title"）。
  否则 PR body 中的 `BREAKING CHANGE:` 行会被 squash commit 丢弃，major bump 永远不会触发。
- 保留 **"Allow squash merging"** 开启。
- PR 模板（`.github/PULL_REQUEST_TEMPLATE.md`）的 `## 🚨 Breaking Change` 段是
  Release Please 识别 BREAKING CHANGE 的唯一来源；没有破坏性变更时删除该段。
- Release Please 会创建/推送分支 `release-please--branches--main`；如未来启用分支保护，
  需为该分支名放行（否则 push 会被阻断）。

## 干跑

`Actions → Release Please → Run workflow` 仍可手动触发（用于重新评估或修复 manifest 漂移）。
`Actions → Release → Run workflow` 输入任意 tag 名（例如 `v0.0.0-test`）可端到端演练二进制/镜像
流水线而不发正式版（带 `-` 后缀的版本不会移动 Docker `latest` 指针）。

## Rollback / Cleanup

- 删本地+远端 tag：`git tag -d vX.Y.Z && git push origin :refs/tags/vX.Y.Z`。
- 删 GitHub Release：`gh release delete vX.Y.Z --yes`。
- 删 GHCR 镜像：`gh api -X DELETE /user/packages/container/nova/versions/<id>` 或在 GHCR Web UI 删。
- 重放：删除 Release 后重新 push tag，或 dispatch `release.yml` 重新跑一次。

## 本地镜像构建

```bash
VERSION=v0.3.0-rc1 scripts/build-all.sh --with-frontend --skip-deps-check
# 或
VERSION=v0.3.0 make build
./dist/nova   # /api/health 报告 v0.3.0
```

## 已知遗留

- `frontend/src/components/Layout.tsx` 当前硬编码 `v0.1.0` 字符串，与 build-time
  ldflags 不联动。是否要从 `/api/health.version` 拉取动态版本号是独立的 UI 改进
  （涉及 i18n key 与 React 状态），不在本次 Release Please 集成范围内。
