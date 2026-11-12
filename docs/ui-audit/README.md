# UI 审计证据 (`docs/ui-audit/`)

这个目录存放 **UI 全局一致性审计** 的证据产物。

审计本身由 [`scripts/ui-audit/`](../../scripts/ui-audit/) 执行，它**只读**前端代码与运行中的应用，
不改任何 UI。发现的不一致点会逐条开成 GitHub Issue（label `ui-audit`），Issue 正文里内嵌这里的截图。

## 目录结构

```
docs/ui-audit/<run-id>/
├── report.md          # 审计报告（概览 + 逐条详情 + 证据索引）
├── findings.json      # 机器可读的全部发现（含规则编号、位置、证据、建议）
├── manifest.json      # 采集清单：路由 → 截图文件、sha256、被审 git commit
├── screenshots/       # 每个路由一张整页截图
└── probe/             # 每个路由一份计算样式指纹（语义控件 → computed style）
```

## 为什么在专用分支上

审计证据是**一次运行的快照**，体积不小，而且只有对应的那次审计才有意义。
所以它不跟着主分支走，而是推到 `audit/ui-<run-id>` 这样的专用分支：

- 主分支和常规 PR 不会被图片撑大；
- 证据与它所描述的 commit 绑定在一起（`manifest.json` 里记了 `gitCommit`）；
- Issue 正文用 `raw.githubusercontent.com/<owner>/<repo>/<branch>/...` 绝对链接引用截图，
  仓库一改名或分支一删，图片就失效——所以分支**不要随便删**。

## 怎么从一条 Issue 找回它的证据

Issue 正文末尾有一行机器可读的标记：

```
<!-- ui-audit-run: 20261025-2015 -->
```

用它定位：

```bash
git fetch origin audit/ui-20261025-2015
git show audit/ui-20261025-2015:docs/ui-audit/20261025-2015/report.md
```

`manifest.json` 里每张截图都带 `sha256`，可以直接校验证据是否被替换过：

```bash
shasum -a 256 docs/ui-audit/<run-id>/screenshots/root.png
```

## 怎么再跑一次

```bash
make ui-audit-deps
make ui-audit BASE_URL=http://localhost:9527
```

细节见 [`scripts/ui-audit/README.md`](../../scripts/ui-audit/README.md)。
