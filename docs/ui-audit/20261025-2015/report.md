# NovaWorkbench 全局 UI 一致性审计报告

| | |
|---|---|
| run id | `20261025-2015` |
| 被审 commit | `de3a60afb84cd8955ab97ca8aeb69bfe11605fe5` |
| 被审实例 | http://localhost:9601 |
| 采集时间 | 2026-10-25T12:18:50.018Z |
| 视口 | 1440×900 @2x |
| 浏览器 | Google Chrome |
| 覆盖路由 | 27 |
| 跳过路由 | 2 |
| 证据分支 | `audit/ui-20261025-2015` |
| 证据根路径 | `docs/ui-audit/20261025-2015` |

## 严重度分布

| 严重度 | 数量 |
|---|---|
| P0 | 0 |
| P1 | 11 |
| P2 | 22 |
| P3 | 2 |

## 规则命中分布

| 规则 | 数量 | 说明 |
|---|---|---|
| R1 | 1 | 调色板外的硬编码颜色 |
| R10 | 1 | Markdown 渲染点未挂 .md-prose |
| R11 | 15 | 同一控件在不同页面的计算样式不一致 |
| R12 | 1 | 占位页 |
| R13 | 1 |  |
| R2 | 1 | 令牌值被写成字面量而非 var(--token) |
| R3 | 1 | 同一颜色存在多种写法 |
| R4 | 4 | 使用了没有任何样式表定义的类名 |
| R5 | 5 | 类名存在前缀式/后缀式两套写法 |
| R6 | 1 | 同类容器存在多套类名 |
| R7 | 1 | 同类区块标题存在多套类名 |
| R8 | 2 | 状态色存在并行词汇表或旁路着色 |
| R9 | 1 | 内联样式密度过高，压过共享类 |

## 索引

| # | 严重度 | 规则 | 标题 | 证据 | Issue |
|---|---|---|---|---|---|
| 1 | P1 | R11 | 「btn」在 23 个页面的计算样式不一致 | [![/](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png)](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png) | — |
| 2 | P1 | R11 | 「select」在 23 个页面的计算样式不一致 | [![/](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png)](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png) | — |
| 3 | P1 | R11 | 「btn-primary」在 21 个页面的计算样式不一致 | [![/](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png)](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png) | — |
| 4 | P1 | R11 | 「badge」在 10 个页面的计算样式不一致 | [![/](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png)](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png) | — |
| 5 | P1 | R11 | 「input-text」在 13 个页面的计算样式不一致 | [![/wizard](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/wizard.png)](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/wizard.png) | — |
| 6 | P1 | R11 | 「btn-danger」在 3 个页面的计算样式不一致 | [![/projects](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects.png)](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects.png) | — |
| 7 | P1 | R11 | 「btn-ghost」在 3 个页面的计算样式不一致 | [![/settings/users](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-users.png)](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-users.png) | — |
| 8 | P1 | R11 | 「textarea」在 2 个页面的计算样式不一致 | [![/settings/roles](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-roles.png)](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-roles.png) | — |
| 9 | P1 | R11 | 「btn-secondary」在 3 个页面的计算样式不一致 | [![/settings/roles](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-roles.png)](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-roles.png) | — |
| 10 | P1 | R13 | 16 个「通用」类名被定义在页面私有样式表里，共 76 处引用 | — | — |
| 11 | P1 | R8 | 状态色存在两套并行词汇表（7 个需求状态 + 8 个运行状态） | — | — |
| 12 | P2 | R1 | 174 种颜色不在设计令牌里，共 546 处硬编码 | — | — |
| 13 | P2 | R10 | 13 处 <MarkdownRender> 没有挂在 .md-prose 上 | — | — |
| 14 | P2 | R11 | 「link」在 23 个页面的计算样式不一致 | [![/](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png)](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png) | — |
| 15 | P2 | R11 | 「card」在 9 个页面的计算样式不一致 | [![/](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png)](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png) | — |
| 16 | P2 | R11 | 「panel」在 13 个页面的计算样式不一致 | [![/](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png)](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png) | — |
| 17 | P2 | R11 | 「table」在 6 个页面的计算样式不一致 | [![/](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png)](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png) | — |
| 18 | P2 | R11 | 「input-choice」在 4 个页面的计算样式不一致 | [![/requirements](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements.png)](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements.png) | — |
| 19 | P2 | R11 | 「btn-icon」在 2 个页面的计算样式不一致 | [![/requirements/calendar](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements-calendar.png)](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements-calendar.png) | — |
| 20 | P2 | R2 | 28 个令牌的值被写死 786 处，而不是引用 var(--token) | — | — |
| 21 | P2 | R4 | 类名 .input 被用了 5 处，但没有任何样式表定义它 | — | — |
| 22 | P2 | R4 | 类名 .modal-label 被用了 4 处，但没有任何样式表定义它 | — | — |
| 23 | P2 | R4 | 类名 .test-result 被用了 4 处，但没有任何样式表定义它 | — | — |
| 24 | P2 | R4 | 类名 .form-label 被用了 4 处，但没有任何样式表定义它 | — | — |
| 25 | P2 | R5 | btn 类名存在两套写法：19 个 `btn-*` vs 20 个 `*-btn` | — | — |
| 26 | P2 | R5 | 18 个 `*-card` 后缀式类名，仓库里没有对应的 `card-*` 前缀式约定 | — | — |
| 27 | P2 | R5 | input 类名存在两套写法：1 个 `input-*` vs 17 个 `*-input` | — | — |
| 28 | P2 | R5 | 15 个 `*-panel` 后缀式类名，仓库里没有对应的 `panel-*` 前缀式约定 | — | — |
| 29 | P2 | R5 | badge 类名存在两套写法：1 个 `badge-*` vs 20 个 `*-badge` | — | — |
| 30 | P2 | R6 | 容器有 6 套并存的类名（32 处引用） | — | — |
| 31 | P2 | R7 | 区块标题有 34 套并存的类名（95 处引用） | — | — |
| 32 | P2 | R8 | 5 个状态类名绕开了 .status-badge 体系 | — | — |
| 33 | P2 | R9 | 3 个文件里塞了大量内联样式（合计 505 处 style={{ }}） | — | — |
| 34 | P3 | R12 | 2 个路由仍是占位页：/chat、/reports | [![/chat](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/chat.png)](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/chat.png) | — |
| 35 | P3 | R3 | 51 个颜色各有多种写法（大小写 / 简写不一致） | — | — |

## 详情

### 1. 「btn」在 23 个页面的计算样式不一致

- **规则**：R11 — 同一控件在不同页面的计算样式不一致
- **严重度**：P1
- **编号**：`R11-001`
- **复核**：未经模型复核（沿用规则文案）

同一个语义控件 `btn` 在不同页面上被渲染成了 9 种样式。差异出现在：fontSize、fontWeight、padding、borderRadius、border、color、background、height、minWidth。用户在同一趟操作里来回切换页面时，会看到"同一个按钮/输入框忽然换了个尺寸"，这是"整体不统一"最直接的观感来源。

差异明细：

- **fontSize** — /: 13 ｜ /wizard: 13 ｜ /projects: 13.3333 ｜ /requirements/calendar: 13 ｜ /schedules: 13 ｜ /knowledge: 14 ｜ /settings/skills: 13.3333 ｜ /projects/proj_ad4d4ac4b4bae7ca: 14 ｜ /reviews/crv_cd6c9d59e4b6f04c: 12
- **fontWeight** — /: 500 ｜ /wizard: 400 ｜ /projects: 400 ｜ /requirements/calendar: 400 ｜ /schedules: 400 ｜ /knowledge: 400 ｜ /settings/skills: 400 ｜ /projects/proj_ad4d4ac4b4bae7ca: 500 ｜ /reviews/crv_cd6c9d59e4b6f04c: 400
- **padding** — /: 8 16 8 16 ｜ /wizard: 2 4 2 4 ｜ /projects: 6 6 6 6 ｜ /requirements/calendar: 4 12 4 12 ｜ /schedules: 4 12 4 12 ｜ /knowledge: 8 16 8 16 ｜ /settings/skills: 6 16 6 16 ｜ /projects/proj_ad4d4ac4b4bae7ca: 8 16 8 16 ｜ /reviews/crv_cd6c9d59e4b6f04c: 4 10 4 10
- **borderRadius** — /: 6 ｜ /wizard: 3 ｜ /projects: 6 ｜ /requirements/calendar: 999 ｜ /schedules: 999 ｜ /knowledge: 0 ｜ /settings/skills: 0 ｜ /projects/proj_ad4d4ac4b4bae7ca: 0 ｜ /reviews/crv_cd6c9d59e4b6f04c: 999
- **border** — /: 1 1 1 1 ｜ /wizard: 0 0 0 0 ｜ /projects: 0 0 0 0 ｜ /requirements/calendar: 1 1 1 1 ｜ /schedules: 1 1 1 1 ｜ /knowledge: 0 0 2 0 ｜ /settings/skills: 0 0 2 0 ｜ /projects/proj_ad4d4ac4b4bae7ca: 0 0 2 0 ｜ /reviews/crv_cd6c9d59e4b6f04c: 1 1 1 1
- **color** — /: rgb(30, 41, 59) ｜ /wizard: rgb(100, 116, 139) ｜ /projects: rgb(0, 0, 0) ｜ /requirements/calendar: rgb(0, 0, 0) ｜ /schedules: rgb(0, 0, 0) ｜ /knowledge: rgb(100, 116, 139) ｜ /settings/skills: rgb(100, 116, 139) ｜ /projects/proj_ad4d4ac4b4bae7ca: rgb(100, 116, 139) ｜ /reviews/crv_cd6c9d59e4b6f04c: rgb(100, 116, 139)
- **background** — /: rgb(255, 255, 255) ｜ /wizard: rgba(0, 0, 0, 0) ｜ /projects: rgba(0, 0, 0, 0) ｜ /requirements/calendar: rgb(255, 255, 255) ｜ /schedules: rgb(255, 255, 255) ｜ /knowledge: rgba(0, 0, 0, 0) ｜ /settings/skills: rgba(0, 0, 0, 0) ｜ /projects/proj_ad4d4ac4b4bae7ca: rgba(0, 0, 0, 0) ｜ /reviews/crv_cd6c9d59e4b6f04c: rgb(255, 255, 255)
- **height** — /: 39 ｜ /wizard: 19 ｜ /projects: 36 ｜ /requirements/calendar: 31 ｜ /schedules: 28 ｜ /knowledge: 38 ｜ /settings/skills: 33 ｜ /projects/proj_ad4d4ac4b4bae7ca: 38 ｜ /reviews/crv_cd6c9d59e4b6f04c: 27
- **minWidth** — /: auto ｜ /wizard: 0 ｜ /projects: 0 ｜ /requirements/calendar: auto ｜ /schedules: auto ｜ /knowledge: auto ｜ /settings/skills: auto ｜ /projects/proj_ad4d4ac4b4bae7ca: auto ｜ /reviews/crv_cd6c9d59e4b6f04c: auto

证据：

- `/` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png) · sha256 `004ff765865e7d5b…`
- `/wizard` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/wizard.png) · sha256 `3218d52a241676a4…`
- `/projects` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects.png) · sha256 `c95ec8478c5be981…`
- `/projects/add` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects-add.png) · sha256 `5f1ba4a1b2115753…`
- `/requirements` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements.png) · sha256 `3c6a13839ba52b54…`
- `/settings` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings.png) · sha256 `df0d0040254d1a1a…`
- `/settings/users` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-users.png) · sha256 `058fe27b19e00e77…`
- `/settings/acl` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-acl.png) · sha256 `562dbec07838e016…`

涉及位置：

- `artifacts/ui-audit/20261025-2015/screenshots/root.png` — / → button.btn "添加项目"
- `artifacts/ui-audit/20261025-2015/screenshots/wizard.png` — /wizard → button.fp-crumb "/"
- `artifacts/ui-audit/20261025-2015/screenshots/projects.png` — /projects → button.app-hamburger
- `artifacts/ui-audit/20261025-2015/screenshots/projects-add.png` — /projects/add → button.app-hamburger
- `artifacts/ui-audit/20261025-2015/screenshots/requirements.png` — /requirements → button.app-hamburger
- `artifacts/ui-audit/20261025-2015/screenshots/settings.png` — /settings → button.app-hamburger
- `artifacts/ui-audit/20261025-2015/screenshots/settings-users.png` — /settings/users → button.app-hamburger
- `artifacts/ui-audit/20261025-2015/screenshots/settings-acl.png` — /settings/acl → button.app-hamburger
- `artifacts/ui-audit/20261025-2015/screenshots/settings-roles.png` — /settings/roles → button.app-hamburger
- `artifacts/ui-audit/20261025-2015/screenshots/settings-agent-servers.png` — /settings/agent-servers → button.app-hamburger
- `artifacts/ui-audit/20261025-2015/screenshots/settings-claude.png` — /settings/claude → button.app-hamburger
- `artifacts/ui-audit/20261025-2015/screenshots/settings-llm.png` — /settings/llm → button.app-hamburger

**建议**：把这些差异收敛到 index.css 的同一套类上：把 fontSize/fontWeight/padding/borderRadius/border/color/background/height/minWidth 这些属性提到共享规则里，页面私有差异用额外的修饰类叠加，而不是各页各写各的。

### 2. 「select」在 23 个页面的计算样式不一致

- **规则**：R11 — 同一控件在不同页面的计算样式不一致
- **严重度**：P1
- **编号**：`R11-002`
- **复核**：未经模型复核（沿用规则文案）

同一个语义控件 `select` 在不同页面上被渲染成了 3 种样式。差异出现在：fontSize、padding、color、background、height、minWidth。用户在同一趟操作里来回切换页面时，会看到"同一个按钮/输入框忽然换了个尺寸"，这是"整体不统一"最直接的观感来源。

差异明细：

- **fontSize** — /: 13 ｜ /requirements: 14 ｜ /settings/roles: 14
- **padding** — /: 3 6 3 6 ｜ /requirements: 10 12 10 12 ｜ /settings/roles: 10 12 10 12
- **color** — /: rgb(100, 116, 139) ｜ /requirements: rgb(30, 41, 59) ｜ /settings/roles: rgb(30, 41, 59)
- **background** — /: rgba(0, 0, 0, 0) ｜ /requirements: rgb(248, 250, 252) ｜ /settings/roles: rgb(248, 250, 252)
- **height** — /: 25 ｜ /requirements: 40 ｜ /settings/roles: 40
- **minWidth** — /: auto ｜ /requirements: 160 ｜ /settings/roles: auto

证据：

- `/` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png) · sha256 `004ff765865e7d5b…`
- `/wizard` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/wizard.png) · sha256 `3218d52a241676a4…`
- `/projects` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects.png) · sha256 `c95ec8478c5be981…`
- `/projects/add` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects-add.png) · sha256 `5f1ba4a1b2115753…`
- `/requirements/calendar` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements-calendar.png) · sha256 `18912bf3e2e33b8e…`
- `/schedules` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/schedules.png) · sha256 `6173562bbfc3a8e4…`
- `/knowledge` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/knowledge.png) · sha256 `8f4828fcf3463506…`
- `/settings` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings.png) · sha256 `df0d0040254d1a1a…`

涉及位置：

- `artifacts/ui-audit/20261025-2015/screenshots/root.png` — / → select.lang-switcher-select "中文English"
- `artifacts/ui-audit/20261025-2015/screenshots/wizard.png` — /wizard → select.lang-switcher-select "中文English"
- `artifacts/ui-audit/20261025-2015/screenshots/projects.png` — /projects → select.lang-switcher-select "中文English"
- `artifacts/ui-audit/20261025-2015/screenshots/projects-add.png` — /projects/add → select.lang-switcher-select "中文English"
- `artifacts/ui-audit/20261025-2015/screenshots/requirements-calendar.png` — /requirements/calendar → select.lang-switcher-select "中文English"
- `artifacts/ui-audit/20261025-2015/screenshots/schedules.png` — /schedules → select.lang-switcher-select "中文English"
- `artifacts/ui-audit/20261025-2015/screenshots/knowledge.png` — /knowledge → select.lang-switcher-select "中文English"
- `artifacts/ui-audit/20261025-2015/screenshots/settings.png` — /settings → select.lang-switcher-select "中文English"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-users.png` — /settings/users → select.lang-switcher-select "中文English"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-acl.png` — /settings/acl → select.lang-switcher-select "中文English"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-agent-servers.png` — /settings/agent-servers → select.lang-switcher-select "中文English"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-skills.png` — /settings/skills → select.lang-switcher-select "中文English"

**建议**：把这些差异收敛到 index.css 的同一套类上：把 fontSize/padding/color/background/height/minWidth 这些属性提到共享规则里，页面私有差异用额外的修饰类叠加，而不是各页各写各的。

### 3. 「btn-primary」在 21 个页面的计算样式不一致

- **规则**：R11 — 同一控件在不同页面的计算样式不一致
- **严重度**：P1
- **编号**：`R11-006`
- **复核**：未经模型复核（沿用规则文案）

同一个语义控件 `btn-primary` 在不同页面上被渲染成了 8 种样式。差异出现在：fontSize、padding、borderRadius、height、minWidth。用户在同一趟操作里来回切换页面时，会看到"同一个按钮/输入框忽然换了个尺寸"，这是"整体不统一"最直接的观感来源。

差异明细：

- **fontSize** — /: 13 ｜ /wizard: 12 ｜ /projects: 13 ｜ /requirements: 13 ｜ /knowledge: 12 ｜ /settings: 13 ｜ /settings/claude: 13 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 13
- **padding** — /: 8 16 8 16 ｜ /wizard: 4 12 4 12 ｜ /projects: 8 16 8 16 ｜ /requirements: 8 16 8 16 ｜ /knowledge: 4 10 4 10 ｜ /settings: 8 16 8 16 ｜ /settings/claude: 8 16 8 16 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 8 16 8 16
- **borderRadius** — /: 6 ｜ /wizard: 4 ｜ /projects: 6 ｜ /requirements: 6 ｜ /knowledge: 4 ｜ /settings: 6 ｜ /settings/claude: 6 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 6
- **height** — /: 36 ｜ /wizard: 27 ｜ /projects: 39 ｜ /requirements: 36 ｜ /knowledge: 27 ｜ /settings: 51 ｜ /settings/claude: 54 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 39
- **minWidth** — /: auto ｜ /wizard: auto ｜ /projects: auto ｜ /requirements: 0 ｜ /knowledge: auto ｜ /settings: auto ｜ /settings/claude: auto ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 0

证据：

- `/` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png) · sha256 `004ff765865e7d5b…`
- `/projects/add` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects-add.png) · sha256 `5f1ba4a1b2115753…`
- `/settings/users` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-users.png) · sha256 `058fe27b19e00e77…`
- `/settings/acl` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-acl.png) · sha256 `562dbec07838e016…`
- `/settings/roles` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-roles.png) · sha256 `9065eb583b1a7520…`
- `/settings/agent-servers` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-agent-servers.png) · sha256 `42fde69ac2aa311b…`
- `/settings/skills` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-skills.png) · sha256 `153087e7da0259e9…`
- `/settings/git-sync` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-git-sync.png) · sha256 `0369dd9d05d7b013…`

涉及位置：

- `artifacts/ui-audit/20261025-2015/screenshots/root.png` — / → button.btn.btn-primary.desktop-only "+ 添加"
- `artifacts/ui-audit/20261025-2015/screenshots/projects-add.png` — /projects/add → button.btn.btn-primary "开始添加"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-users.png` — /settings/users → button.btn.btn-primary "+ 添加用户"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-acl.png` — /settings/acl → button.btn.btn-primary "+ 添加角色"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-roles.png` — /settings/roles → button.btn.btn-primary "保存"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-agent-servers.png` — /settings/agent-servers → button.btn.btn-primary "+ 添加服务器"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-skills.png` — /settings/skills → button.btn.btn-primary "+ 新建 Skill"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-git-sync.png` — /settings/git-sync → button.btn.btn-primary "保存"
- `artifacts/ui-audit/20261025-2015/screenshots/reviews-crv-cd6c9d59e4b6f04c.png` — /reviews/crv_cd6c9d59e4b6f04c → button.btn.btn-primary "提交追问"
- `artifacts/ui-audit/20261025-2015/screenshots/wizard.png` — /wizard → button.fp-pick-btn.btn.btn-primary "选择"
- `artifacts/ui-audit/20261025-2015/screenshots/projects.png` — /projects → button.btn.btn-primary "🗑 回收站"
- `artifacts/ui-audit/20261025-2015/screenshots/requirements-calendar.png` — /requirements/calendar → button.btn.btn-primary.desktop-only "📋 返回列表"

**建议**：把这些差异收敛到 index.css 的同一套类上：把 fontSize/padding/borderRadius/height/minWidth 这些属性提到共享规则里，页面私有差异用额外的修饰类叠加，而不是各页各写各的。

### 4. 「badge」在 10 个页面的计算样式不一致

- **规则**：R11 — 同一控件在不同页面的计算样式不一致
- **严重度**：P1
- **编号**：`R11-008`
- **复核**：未经模型复核（沿用规则文案）

同一个语义控件 `badge` 在不同页面上被渲染成了 9 种样式。差异出现在：fontSize、lineHeight、fontWeight、padding、borderRadius、border、color、background、height、minWidth。用户在同一趟操作里来回切换页面时，会看到"同一个按钮/输入框忽然换了个尺寸"，这是"整体不统一"最直接的观感来源。

差异明细：

- **fontSize** — /: 12 ｜ /wizard: 10 ｜ /requirements: 14 ｜ /requirements/calendar: 14 ｜ /settings/users: 11 ｜ /settings/acl: 12 ｜ /projects/proj_ad4d4ac4b4bae7ca: 12 ｜ /requirements/req_6654d9308c6a5b8c: 13 ｜ /reviews/crv_cd6c9d59e4b6f04c: 11.5
- **lineHeight** — /: 18 ｜ /wizard: 15 ｜ /requirements: 21 ｜ /requirements/calendar: 21 ｜ /settings/users: 16.5 ｜ /settings/acl: 18 ｜ /projects/proj_ad4d4ac4b4bae7ca: 18 ｜ /requirements/req_6654d9308c6a5b8c: 19.5 ｜ /reviews/crv_cd6c9d59e4b6f04c: 17.25
- **fontWeight** — /: 400 ｜ /wizard: 700 ｜ /requirements: 400 ｜ /requirements/calendar: 400 ｜ /settings/users: 600 ｜ /settings/acl: 400 ｜ /projects/proj_ad4d4ac4b4bae7ca: 600 ｜ /requirements/req_6654d9308c6a5b8c: 400 ｜ /reviews/crv_cd6c9d59e4b6f04c: 400
- **padding** — /: 2 8 2 8 ｜ /wizard: 2 6 2 6 ｜ /requirements: 0 0 0 0 ｜ /requirements/calendar: 0 0 0 0 ｜ /settings/users: 1 7 1 7 ｜ /settings/acl: 2 8 2 8 ｜ /projects/proj_ad4d4ac4b4bae7ca: 4 10 4 10 ｜ /requirements/req_6654d9308c6a5b8c: 0 0 0 0 ｜ /reviews/crv_cd6c9d59e4b6f04c: 3 9 3 9
- **borderRadius** — /: 4 ｜ /wizard: 3 ｜ /requirements: 0 ｜ /requirements/calendar: 0 ｜ /settings/users: 10 ｜ /settings/acl: 10 ｜ /projects/proj_ad4d4ac4b4bae7ca: 6 ｜ /requirements/req_6654d9308c6a5b8c: 0 ｜ /reviews/crv_cd6c9d59e4b6f04c: 999
- **border** — /: 0 0 0 0 ｜ /wizard: 0 0 0 0 ｜ /requirements: 0 0 0 0 ｜ /requirements/calendar: 0 0 0 0 ｜ /settings/users: 0 0 0 0 ｜ /settings/acl: 0 0 0 0 ｜ /projects/proj_ad4d4ac4b4bae7ca: 0 0 0 0 ｜ /requirements/req_6654d9308c6a5b8c: 0 0 0 0 ｜ /reviews/crv_cd6c9d59e4b6f04c: 1 1 1 1
- **color** — /: rgb(79, 70, 229) ｜ /wizard: rgb(16, 185, 129) ｜ /requirements: rgb(30, 41, 59) ｜ /requirements/calendar: rgb(30, 41, 59) ｜ /settings/users: rgb(255, 255, 255) ｜ /settings/acl: rgb(79, 70, 229) ｜ /projects/proj_ad4d4ac4b4bae7ca: rgb(79, 70, 229) ｜ /requirements/req_6654d9308c6a5b8c: rgb(148, 163, 184) ｜ /reviews/crv_cd6c9d59e4b6f04c: rgb(100, 116, 139)
- **background** — /: rgb(238, 242, 255) ｜ /wizard: rgb(220, 252, 231) ｜ /requirements: rgba(0, 0, 0, 0) ｜ /requirements/calendar: rgba(0, 0, 0, 0) ｜ /settings/users: rgb(79, 70, 229) ｜ /settings/acl: rgb(238, 242, 255) ｜ /projects/proj_ad4d4ac4b4bae7ca: rgb(238, 242, 255) ｜ /requirements/req_6654d9308c6a5b8c: rgba(0, 0, 0, 0) ｜ /reviews/crv_cd6c9d59e4b6f04c: rgb(255, 255, 255)
- **height** — /: 22 ｜ /wizard: 19 ｜ /requirements: 31 ｜ /requirements/calendar: 31 ｜ /settings/users: 18.5 ｜ /settings/acl: 22 ｜ /projects/proj_ad4d4ac4b4bae7ca: 26 ｜ /requirements/req_6654d9308c6a5b8c: 24 ｜ /reviews/crv_cd6c9d59e4b6f04c: 25.25
- **minWidth** — /: 0 ｜ /wizard: auto ｜ /requirements: 0 ｜ /requirements/calendar: auto ｜ /settings/users: 0 ｜ /settings/acl: auto ｜ /projects/proj_ad4d4ac4b4bae7ca: auto ｜ /requirements/req_6654d9308c6a5b8c: auto ｜ /reviews/crv_cd6c9d59e4b6f04c: auto

证据：

- `/` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png) · sha256 `004ff765865e7d5b…`
- `/projects` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects.png) · sha256 `c95ec8478c5be981…`
- `/wizard` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/wizard.png) · sha256 `3218d52a241676a4…`
- `/requirements` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements.png) · sha256 `3c6a13839ba52b54…`
- `/requirements/calendar` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements-calendar.png) · sha256 `18912bf3e2e33b8e…`
- `/settings/users` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-users.png) · sha256 `058fe27b19e00e77…`
- `/settings/acl` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-acl.png) · sha256 `562dbec07838e016…`
- `/projects/proj_ad4d4ac4b4bae7ca` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca.png) · sha256 `9ad4f0079ad72c33…`

涉及位置：

- `artifacts/ui-audit/20261025-2015/screenshots/root.png` — / → span.type-tag "Unknown"
- `artifacts/ui-audit/20261025-2015/screenshots/projects.png` — /projects → span.type-tag "Unknown"
- `artifacts/ui-audit/20261025-2015/screenshots/wizard.png` — /wizard → span.fp-git-badge "Git"
- `artifacts/ui-audit/20261025-2015/screenshots/requirements.png` — /requirements → div.req-kind-chips "🐛 问题📋 需求💡 想法📚 知识库全部类型已选"
- `artifacts/ui-audit/20261025-2015/screenshots/requirements-calendar.png` — /requirements/calendar → div.req-kind-chips "🐛 问题📋 需求💡 想法📚 知识库"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-users.png` — /settings/users → span.admin-badge "管理员"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-acl.png` — /settings/acl → span.perm-chip "仪表盘"
- `artifacts/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca.png` — /projects/proj_ad4d4ac4b4bae7ca → span.detail-type-tag "Unknown"
- `artifacts/ui-audit/20261025-2015/screenshots/requirements-req-6654d9308c6a5b8c.png` — /requirements/req_6654d9308c6a5b8c → div.stage-step "方案设计"
- `artifacts/ui-audit/20261025-2015/screenshots/reviews-crv-cd6c9d59e4b6f04c.png` — /reviews/crv_cd6c9d59e4b6f04c → span.cr-meta-chip "main ← master"

**建议**：把这些差异收敛到 index.css 的同一套类上：把 fontSize/lineHeight/fontWeight/padding/borderRadius/border/color/background/height/minWidth 这些属性提到共享规则里，页面私有差异用额外的修饰类叠加，而不是各页各写各的。

### 5. 「input-text」在 13 个页面的计算样式不一致

- **规则**：R11 — 同一控件在不同页面的计算样式不一致
- **严重度**：P1
- **编号**：`R11-009`
- **复核**：未经模型复核（沿用规则文案）

同一个语义控件 `input-text` 在不同页面上被渲染成了 7 种样式。差异出现在：fontSize、padding、borderRadius、border、background、height、minWidth、boxShadow。用户在同一趟操作里来回切换页面时，会看到"同一个按钮/输入框忽然换了个尺寸"，这是"整体不统一"最直接的观感来源。

差异明细：

- **fontSize** — /wizard: 14 ｜ /projects/add: 14 ｜ /requirements: 13 ｜ /knowledge: 13 ｜ /settings/agent-servers: 14 ｜ /settings/database: 14 ｜ /requirements/req_6654d9308c6a5b8c: 12
- **padding** — /wizard: 10 12 10 12 ｜ /projects/add: 10 12 10 12 ｜ /requirements: 8 0 8 0 ｜ /knowledge: 6 10 6 10 ｜ /settings/agent-servers: 10 12 10 12 ｜ /settings/database: 10 12 10 12 ｜ /requirements/req_6654d9308c6a5b8c: 4 12 4 12
- **borderRadius** — /wizard: 6 ｜ /projects/add: 6 ｜ /requirements: 0 ｜ /knowledge: 6 ｜ /settings/agent-servers: 6 ｜ /settings/database: 6 ｜ /requirements/req_6654d9308c6a5b8c: 999
- **border** — /wizard: 1 1 1 1 ｜ /projects/add: 1 1 1 1 ｜ /requirements: 0 0 0 0 ｜ /knowledge: 1 1 1 1 ｜ /settings/agent-servers: 1 1 1 1 ｜ /settings/database: 1 1 1 1 ｜ /requirements/req_6654d9308c6a5b8c: 1 1 1 1
- **background** — /wizard: rgb(255, 255, 255) ｜ /projects/add: rgb(248, 250, 252) ｜ /requirements: rgba(0, 0, 0, 0) ｜ /knowledge: rgb(255, 255, 255) ｜ /settings/agent-servers: rgb(248, 250, 252) ｜ /settings/database: rgb(248, 250, 252) ｜ /requirements/req_6654d9308c6a5b8c: rgba(0, 0, 0, 0)
- **height** — /wizard: 38 ｜ /projects/add: 38 ｜ /requirements: 31 ｜ /knowledge: 29 ｜ /settings/agent-servers: 54 ｜ /settings/database: 38 ｜ /requirements/req_6654d9308c6a5b8c: 24
- **minWidth** — /wizard: 0 ｜ /projects/add: 0 ｜ /requirements: 0 ｜ /knowledge: 200 ｜ /settings/agent-servers: auto ｜ /settings/database: auto ｜ /requirements/req_6654d9308c6a5b8c: 140
- **boxShadow** — /wizard: rgba(79, 70, 229, 0.12) 0px 0px 0px 3px ｜ /projects/add: none ｜ /requirements: none ｜ /knowledge: none ｜ /settings/agent-servers: none ｜ /settings/database: none ｜ /requirements/req_6654d9308c6a5b8c: none

证据：

- `/wizard` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/wizard.png) · sha256 `3218d52a241676a4…`
- `/projects/add` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects-add.png) · sha256 `5f1ba4a1b2115753…`
- `/settings/llm` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-llm.png) · sha256 `47b0a6b8ca99149d…`
- `/settings/subtask` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-subtask.png) · sha256 `e7643e35f58cc75e…`
- `/settings/git-sync` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-git-sync.png) · sha256 `0369dd9d05d7b013…`
- `/projects/proj_ad4d4ac4b4bae7ca/issue-monitor` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca-issue-monitor.png) · sha256 `8a10ca34ee0876b0…`
- `/requirements` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements.png) · sha256 `3c6a13839ba52b54…`
- `/requirements/calendar` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements-calendar.png) · sha256 `18912bf3e2e33b8e…`

涉及位置：

- `artifacts/ui-audit/20261025-2015/screenshots/wizard.png` — /wizard → input.form-input
- `artifacts/ui-audit/20261025-2015/screenshots/projects-add.png` — /projects/add → input.form-input
- `artifacts/ui-audit/20261025-2015/screenshots/settings-llm.png` — /settings/llm → input.form-input
- `artifacts/ui-audit/20261025-2015/screenshots/settings-subtask.png` — /settings/subtask → input.form-input
- `artifacts/ui-audit/20261025-2015/screenshots/settings-git-sync.png` — /settings/git-sync → input.form-input
- `artifacts/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca-issue-monitor.png` — /projects/proj_ad4d4ac4b4bae7ca/issue-monitor → input.form-input
- `artifacts/ui-audit/20261025-2015/screenshots/requirements.png` — /requirements → input
- `artifacts/ui-audit/20261025-2015/screenshots/requirements-calendar.png` — /requirements/calendar → input
- `artifacts/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca.png` — /projects/proj_ad4d4ac4b4bae7ca → input
- `artifacts/ui-audit/20261025-2015/screenshots/knowledge.png` — /knowledge → input.kb-search
- `artifacts/ui-audit/20261025-2015/screenshots/settings-agent-servers.png` — /settings/agent-servers → input.form-input
- `artifacts/ui-audit/20261025-2015/screenshots/settings-database.png` — /settings/database → input.form-input

**建议**：把这些差异收敛到 index.css 的同一套类上：把 fontSize/padding/borderRadius/border/background/height/minWidth/boxShadow 这些属性提到共享规则里，页面私有差异用额外的修饰类叠加，而不是各页各写各的。

### 6. 「btn-danger」在 3 个页面的计算样式不一致

- **规则**：R11 — 同一控件在不同页面的计算样式不一致
- **严重度**：P1
- **编号**：`R11-010`
- **复核**：未经模型复核（沿用规则文案）

同一个语义控件 `btn-danger` 在不同页面上被渲染成了 3 种样式。差异出现在：fontSize、lineHeight、padding、borderRadius、border、color、background、height、minWidth。用户在同一趟操作里来回切换页面时，会看到"同一个按钮/输入框忽然换了个尺寸"，这是"整体不统一"最直接的观感来源。

差异明细：

- **fontSize** — /projects: 12 ｜ /settings/users: 14 ｜ /requirements/req_6654d9308c6a5b8c: 12
- **lineHeight** — /projects: normal ｜ /settings/users: 21 ｜ /requirements/req_6654d9308c6a5b8c: normal
- **padding** — /projects: 4 10 4 10 ｜ /settings/users: 0 0 0 0 ｜ /requirements/req_6654d9308c6a5b8c: 4 10 4 10
- **borderRadius** — /projects: 6 ｜ /settings/users: 4 ｜ /requirements/req_6654d9308c6a5b8c: 6
- **border** — /projects: 1 1 1 1 ｜ /settings/users: 0 0 0 0 ｜ /requirements/req_6654d9308c6a5b8c: 1 1 1 1
- **color** — /projects: rgb(255, 255, 255) ｜ /settings/users: rgb(79, 70, 229) ｜ /requirements/req_6654d9308c6a5b8c: rgb(255, 255, 255)
- **background** — /projects: rgb(239, 68, 68) ｜ /settings/users: rgba(0, 0, 0, 0) ｜ /requirements/req_6654d9308c6a5b8c: rgb(239, 68, 68)
- **height** — /projects: 44 ｜ /settings/users: 21 ｜ /requirements/req_6654d9308c6a5b8c: 27
- **minWidth** — /projects: 0 ｜ /settings/users: 0 ｜ /requirements/req_6654d9308c6a5b8c: auto

证据：

- `/projects` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects.png) · sha256 `c95ec8478c5be981…`
- `/settings/users` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-users.png) · sha256 `058fe27b19e00e77…`
- `/requirements/req_6654d9308c6a5b8c` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements-req-6654d9308c6a5b8c.png) · sha256 `6c3da44eafeac753…`

涉及位置：

- `artifacts/ui-audit/20261025-2015/screenshots/projects.png` — /projects → button.btn.btn-sm.btn-danger "删除"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-users.png` — /settings/users → button.btn-link.btn-danger-link "删除"
- `artifacts/ui-audit/20261025-2015/screenshots/requirements-req-6654d9308c6a5b8c.png` — /requirements/req_6654d9308c6a5b8c → button.btn.btn-sm.btn-danger "删除"

**建议**：把这些差异收敛到 index.css 的同一套类上：把 fontSize/lineHeight/padding/borderRadius/border/color/background/height/minWidth 这些属性提到共享规则里，页面私有差异用额外的修饰类叠加，而不是各页各写各的。

### 7. 「btn-ghost」在 3 个页面的计算样式不一致

- **规则**：R11 — 同一控件在不同页面的计算样式不一致
- **严重度**：P1
- **编号**：`R11-013`
- **复核**：未经模型复核（沿用规则文案）

同一个语义控件 `btn-ghost` 在不同页面上被渲染成了 3 种样式。差异出现在：fontSize、lineHeight、padding、border、height、minWidth。用户在同一趟操作里来回切换页面时，会看到"同一个按钮/输入框忽然换了个尺寸"，这是"整体不统一"最直接的观感来源。

差异明细：

- **fontSize** — /settings/users: 14 ｜ /settings/acl: 14 ｜ /requirements/req_6654d9308c6a5b8c: 12
- **lineHeight** — /settings/users: 21 ｜ /settings/acl: 21 ｜ /requirements/req_6654d9308c6a5b8c: normal
- **padding** — /settings/users: 0 0 0 0 ｜ /settings/acl: 0 0 0 0 ｜ /requirements/req_6654d9308c6a5b8c: 4 10 4 10
- **border** — /settings/users: 0 0 0 0 ｜ /settings/acl: 0 0 0 0 ｜ /requirements/req_6654d9308c6a5b8c: 1 1 1 1
- **height** — /settings/users: 21 ｜ /settings/acl: 21 ｜ /requirements/req_6654d9308c6a5b8c: 27
- **minWidth** — /settings/users: 0 ｜ /settings/acl: auto ｜ /requirements/req_6654d9308c6a5b8c: auto

证据：

- `/settings/users` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-users.png) · sha256 `058fe27b19e00e77…`
- `/settings/acl` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-acl.png) · sha256 `562dbec07838e016…`
- `/requirements/req_6654d9308c6a5b8c` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements-req-6654d9308c6a5b8c.png) · sha256 `6c3da44eafeac753…`

涉及位置：

- `artifacts/ui-audit/20261025-2015/screenshots/settings-users.png` — /settings/users → button.btn-link "编辑"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-acl.png` — /settings/acl → button.btn-link "编辑"
- `artifacts/ui-audit/20261025-2015/screenshots/requirements-req-6654d9308c6a5b8c.png` — /requirements/req_6654d9308c6a5b8c → button.btn.btn-sm.btn-ghost "或先进行需求分析完善需求→"

**建议**：把这些差异收敛到 index.css 的同一套类上：把 fontSize/lineHeight/padding/border/height/minWidth 这些属性提到共享规则里，页面私有差异用额外的修饰类叠加，而不是各页各写各的。

### 8. 「textarea」在 2 个页面的计算样式不一致

- **规则**：R11 — 同一控件在不同页面的计算样式不一致
- **严重度**：P1
- **编号**：`R11-014`
- **复核**：未经模型复核（沿用规则文案）

同一个语义控件 `textarea` 在不同页面上被渲染成了 2 种样式。差异出现在：fontSize、lineHeight、height。用户在同一趟操作里来回切换页面时，会看到"同一个按钮/输入框忽然换了个尺寸"，这是"整体不统一"最直接的观感来源。

差异明细：

- **fontSize** — /settings/roles: 12.5 ｜ /reviews/crv_cd6c9d59e4b6f04c: 14
- **lineHeight** — /settings/roles: 20 ｜ /reviews/crv_cd6c9d59e4b6f04c: 22.4
- **height** — /settings/roles: 222 ｜ /reviews/crv_cd6c9d59e4b6f04c: 89.1719

证据：

- `/settings/roles` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-roles.png) · sha256 `9065eb583b1a7520…`
- `/reviews/crv_cd6c9d59e4b6f04c` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/reviews-crv-cd6c9d59e4b6f04c.png) · sha256 `ca523020af6af7c4…`

涉及位置：

- `artifacts/ui-audit/20261025-2015/screenshots/settings-roles.png` — /settings/roles → textarea.form-input.role-prompt-input "你是一位资深软件工程师，负责评估需求的完整性，判断其是否可以直接交给 AI 编码"
- `artifacts/ui-audit/20261025-2015/screenshots/reviews-crv-cd6c9d59e4b6f04c.png` — /reviews/crv_cd6c9d59e4b6f04c → textarea.form-input.cr-desc

**建议**：把这些差异收敛到 index.css 的同一套类上：把 fontSize/lineHeight/height 这些属性提到共享规则里，页面私有差异用额外的修饰类叠加，而不是各页各写各的。

### 9. 「btn-secondary」在 3 个页面的计算样式不一致

- **规则**：R11 — 同一控件在不同页面的计算样式不一致
- **严重度**：P1
- **编号**：`R11-015`
- **复核**：未经模型复核（沿用规则文案）

同一个语义控件 `btn-secondary` 在不同页面上被渲染成了 3 种样式。差异出现在：height、minWidth。用户在同一趟操作里来回切换页面时，会看到"同一个按钮/输入框忽然换了个尺寸"，这是"整体不统一"最直接的观感来源。

差异明细：

- **height** — /settings/roles: 36 ｜ /settings/database: 39 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 39
- **minWidth** — /settings/roles: auto ｜ /settings/database: auto ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 0

证据：

- `/settings/roles` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-roles.png) · sha256 `9065eb583b1a7520…`
- `/settings/database` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-database.png) · sha256 `9752df8e0bba6393…`
- `/projects/proj_ad4d4ac4b4bae7ca/issue-monitor` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca-issue-monitor.png) · sha256 `8a10ca34ee0876b0…`

涉及位置：

- `artifacts/ui-audit/20261025-2015/screenshots/settings-roles.png` — /settings/roles → button.btn.btn-secondary "恢复默认提示词"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-database.png` — /settings/database → button.btn.btn-secondary "🔌 测试连接"
- `artifacts/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca-issue-monitor.png` — /projects/proj_ad4d4ac4b4bae7ca/issue-monitor → button.btn.btn-secondary "🧪 测试匹配"

**建议**：把这些差异收敛到 index.css 的同一套类上：把 height/minWidth 这些属性提到共享规则里，页面私有差异用额外的修饰类叠加，而不是各页各写各的。

### 10. 16 个「通用」类名被定义在页面私有样式表里，共 76 处引用

- **规则**：R13
- **严重度**：P1
- **编号**：`R13-017`
- **复核**：未经模型复核（沿用规则文案）

这些类名长得像全站通用规范（.btn-secondary、.btn-link、.btn-danger-link、.input-hint、.btn-launch、.btn-cancel），但它们只定义在 `frontend/src/pages/Settings.css`、`frontend/src/pages/SettingsAgentServers.css`、`frontend/src/pages/AddProject.css`、`frontend/src/pages/RequirementDetail.css`、`frontend/src/pages/WizardPage.css` 这些页面私有样式表里，而不是全局的 frontend/src/index.css。只要某个页面没有 import 那张样式表，它就用不到这条规则——同一个 .btn-secondary 在设置页是圆角带阴影的，在别的页面就是浏览器默认样式。这是"同一个按钮在不同页面长得不一样"最直接的成因。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)

涉及位置：

- `frontend/src/components/CreateRequirementForm/CreateRequirementForm.tsx:425` — .btn-secondary
- `frontend/src/pages/SettingsACLRoles.tsx:158` — .btn-link
- `frontend/src/pages/SettingsACLRoles.tsx:161` — .btn-danger-link
- `frontend/src/pages/AddProject.tsx:85` — .input-hint
- `frontend/src/components/DesignCodingImmediateModal.tsx:719` — .btn-launch
- `frontend/src/components/DesignCodingImmediateModal.tsx:725` — .btn-cancel
- `frontend/src/pages/RequirementDetail.tsx:5207` — .btn-split-item
- `frontend/src/pages/RequirementDetail.tsx:5202` — .btn-split-backdrop
- `frontend/src/pages/RequirementDetail.tsx:5203` — .btn-split-dropdown
- `frontend/src/pages/RequirementDetail.tsx:5182` — .btn-group-split

| 类名 | 引用次数 | 定义位置 |
|---|---|---|
| | `.btn-secondary` | 35 | `frontend/src/pages/Settings.css` | |
| | `.btn-link` | 13 | `frontend/src/pages/Settings.css`<br>`frontend/src/pages/SettingsAgentServers.css` | |
| | `.btn-danger-link` | 5 | `frontend/src/pages/Settings.css` | |
| | `.input-hint` | 3 | `frontend/src/pages/AddProject.css` | |
| | `.btn-launch` | 3 | `frontend/src/pages/RequirementDetail.css` | |
| | `.btn-cancel` | 3 | `frontend/src/pages/RequirementDetail.css` | |
| | `.btn-split-item` | 3 | `frontend/src/pages/RequirementDetail.css` | |
| | `.btn-split-backdrop` | 2 | `frontend/src/pages/RequirementDetail.css` | |
| | `.btn-split-dropdown` | 2 | `frontend/src/pages/RequirementDetail.css` | |
| | `.btn-group-split` | 1 | `frontend/src/pages/RequirementDetail.css` | |
| | `.btn-split-main` | 1 | `frontend/src/pages/RequirementDetail.css` | |
| | `.btn-split-caret` | 1 | `frontend/src/pages/RequirementDetail.css` | |
| | `.btn-split-menu-wrap` | 1 | `frontend/src/pages/RequirementDetail.css` | |
| | `.form-toast` | 1 | `frontend/src/pages/SettingsAgentServers.css` | |
| | `.modal-content` | 1 | `frontend/src/pages/SettingsAgentServers.css` | |
| | `.btn-lg` | 1 | `frontend/src/pages/WizardPage.css` | |

**建议**：把这些通用类名的定义搬进 frontend/src/index.css（与 .btn-primary / .btn-danger 放在一起），页面私有样式表只保留该页面真正独有的修饰类。

### 11. 状态色存在两套并行词汇表（7 个需求状态 + 8 个运行状态）

- **规则**：R8 — 状态色存在并行词汇表或旁路着色
- **严重度**：P1
- **编号**：`R8-015`
- **复核**：未经模型复核（沿用规则文案）

index.css 里 .status-badge.status-* 一共 15 个，其中需求生命周期用 draft/analyzing/designing/designed/developing/done/archived，另一套用的是 active/missing/error/pending/running/succeeded/failed/canceled。两套共用同一个 .status-badge 基类但语义域不同（例如 "done" 与 "succeeded"、"archived" 与 "canceled" 视觉完全一样），用户看到"一样的颜色"却对应不同含义，是最容易被误读的不一致。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)

涉及位置：

- `frontend/src/index.css:618` — active
- `frontend/src/index.css:619` — missing
- `frontend/src/index.css:621` — error
- `frontend/src/index.css:623` — pending
- `frontend/src/index.css:624` — running
- `frontend/src/index.css:625` — succeeded
- `frontend/src/index.css:626` — failed
- `frontend/src/index.css:627` — canceled

**建议**：给两套状态各自一个明确的基类（例如 .status-badge--req / .status-badge--run），或者把视觉上相同的两个状态合并成一个令牌。

### 12. 174 种颜色不在设计令牌里，共 546 处硬编码

- **规则**：R1 — 调色板外的硬编码颜色
- **严重度**：P2
- **编号**：`R1-001`
- **复核**：未经模型复核（沿用规则文案）

frontend/src/index.css 的 :root 只定义了 27 个色值令牌，但代码里还散落着 174 种调色板外的颜色（合计 546 处）。这些色值不会随令牌调整一起变化——改一次主题色，别处不会跟着动，是"同一个东西在不同页面颜色不一样"的主要来源。出现最多的几种：#0f172a（28 处）、#cbd5e1（21 处）、#475569（20 处）、#334155（18 处）、#e5e7eb（14 处）、#b91c1c（14 处）、#fca5a5（14 处）、#cffafe（13 处）。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `#0f172a`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `#cbd5e1`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `#475569`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `#334155`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `#e5e7eb`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `#b91c1c`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `#fca5a5`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `#cffafe`

涉及位置：

- `frontend/src/components/RequirementBatch/RequirementBatch.css:76` — #0F172A
- `frontend/src/components/SubTaskPanel.css:742` — #0F172A
- `frontend/src/components/SubTaskPanel.css:755` — #0F172A
- `frontend/src/components/SubTaskPanel.css:771` — #0F172A
- `frontend/src/components/SubTaskPanel.css:915` — #0F172A
- `frontend/src/components/SubTaskPanel.css:986` — #0F172A
- `frontend/src/components/AttachmentPanel.css:51` — #cbd5e1
- `frontend/src/components/AttachmentPanel.css:108` — #cbd5e1
- `frontend/src/components/AttachmentPanel.css:280` — #cbd5e1
- `frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:174` — #CBD5E1
- `frontend/src/components/RequirementBatch/RequirementBatch.css:100` — #CBD5E1
- `frontend/src/components/RequirementTreeToggle.css:194` — #CBD5E1

| 颜色 | 出现次数 | 位置 |
|---|---|---|
| | `#0f172a` | 28 | frontend/src/components/RequirementBatch/RequirementBatch.css:76<br>frontend/src/components/SubTaskPanel.css:742<br>frontend/src/components/SubTaskPanel.css:755<br>frontend/src/components/SubTaskPanel.css:771<br>frontend/src/components/SubTaskPanel.css:915<br>frontend/src/components/SubTaskPanel.css:986 | |
| | `#cbd5e1` | 21 | frontend/src/components/AttachmentPanel.css:51<br>frontend/src/components/AttachmentPanel.css:108<br>frontend/src/components/AttachmentPanel.css:280<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:174<br>frontend/src/components/RequirementBatch/RequirementBatch.css:100<br>frontend/src/components/RequirementTreeToggle.css:194 | |
| | `#475569` | 20 | frontend/src/components/DevSourceBadge.css:37<br>frontend/src/components/RequirementBatch/RequirementBatch.css:134<br>frontend/src/components/RichDescriptionEditor/RichDescriptionEditor.css:130<br>frontend/src/components/SubTaskPanel.css:169<br>frontend/src/components/SubTaskPanel.css:256<br>frontend/src/components/SubTaskPanel.css:638 | |
| | `#334155` | 18 | frontend/src/components/AttachmentPanel.css:71<br>frontend/src/components/AttachmentPanel.css:225<br>frontend/src/components/AttachmentPanel.css:285<br>frontend/src/components/RequirementBatch/RequirementBatch.css:105<br>frontend/src/components/RequirementBatch/RequirementBatch.css:127<br>frontend/src/components/SubTaskPanel.css:846 | |
| | `#e5e7eb` | 14 | frontend/src/components/DiagramGenerator.css:30<br>frontend/src/components/DiagramGenerator.css:53<br>frontend/src/components/DiagramGenerator.css:82<br>frontend/src/components/DiagramGenerator.css:106<br>frontend/src/components/DiagramGenerator.css:124<br>frontend/src/components/DiagramGenerator.css:141 | |
| | `#b91c1c` | 14 | frontend/src/components/ImportDesignDialog.css:185<br>frontend/src/components/PdfUploadDialog.css:43<br>frontend/src/components/RequirementBatch/RequirementBatch.css:148<br>frontend/src/components/RequirementBatch/RequirementBatch.css:149<br>frontend/src/components/SubTaskPanel.css:1976<br>frontend/src/index.css:644 | |
| | `#fca5a5` | 14 | frontend/src/components/SubTaskPanel.css:785<br>frontend/src/components/SubTaskPanel.css:791<br>frontend/src/components/SubTaskPanel.css:811<br>frontend/src/components/SubTaskPanel.css:812<br>frontend/src/components/SubTaskPanel.css:873<br>frontend/src/index.css:1255 | |
| | `#cffafe` | 13 | frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:579<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:821<br>frontend/src/index.css:476<br>frontend/src/index.css:614<br>frontend/src/index.css:624<br>frontend/src/index.css:646 | |
| | `#7c3aed` | 11 | frontend/src/pages/KnowledgePage.css:137<br>frontend/src/pages/ProjectDetail.css:1027<br>frontend/src/pages/RequirementDetail.css:81<br>frontend/src/pages/RequirementDetail.css:1816<br>frontend/src/pages/RequirementDetail.css:1840<br>frontend/src/pages/RequirementsList.css:341 | |
| | `#fef2f2` | 10 | frontend/src/components/AttachmentPanel.css:33<br>frontend/src/components/DiagramGenerator.css:151<br>frontend/src/components/MarkdownRender.css:40<br>frontend/src/pages/ProjectDetail.css:1125<br>frontend/src/pages/RequirementDetail.css:1331<br>frontend/src/pages/RequirementDetail.css:2084 | |
| | `#ede9fe` | 10 | frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:573<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:816<br>frontend/src/index.css:475<br>frontend/src/index.css:612<br>frontend/src/pages/KnowledgePage.css:137<br>frontend/src/pages/KnowledgePage.css:142 | |
| | `rgba(79,70,229,0.08)` | 9 | frontend/src/components/AttachmentPanel.css:321<br>frontend/src/components/DevSourceBadge.css:28<br>frontend/src/components/SubTaskPanel.css:694<br>frontend/src/components/SubTaskPanel.css:1232<br>frontend/src/index.css:180<br>frontend/src/pages/RequirementDetail.css:1925 | |
| | `#fffbeb` | 8 | frontend/src/components/AttachmentPanel.css:86<br>frontend/src/components/RequirementBatch/RequirementBatch.css:214<br>frontend/src/components/RichDescriptionEditor/RichDescriptionEditor.css:113<br>frontend/src/pages/RequirementDetail.css:2195<br>frontend/src/pages/RequirementDetail.css:3788<br>frontend/src/pages/RequirementsList.css:83 | |
| | `#6d28d9` | 8 | frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:574<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:817<br>frontend/src/index.css:475<br>frontend/src/index.css:612<br>frontend/src/pages/KnowledgePage.css:142<br>frontend/src/pages/ProjectIssueMonitor.css:152 | |
| | `#92400e` | 8 | frontend/src/components/ImportDesignDialog.css:175<br>frontend/src/components/RichDescriptionEditor/RichDescriptionEditor.css:136<br>frontend/src/components/SubTaskPanel.css:174<br>frontend/src/pages/ProjectDetail.css:426<br>frontend/src/pages/RequirementDetail.css:3781<br>frontend/src/pages/RequirementDetail.css:3792 | |
| | `#fbbf24` | 8 | frontend/src/components/SubTaskPanel.css:787<br>frontend/src/components/SubTaskPanel.css:814<br>frontend/src/components/SubTaskPanel.css:872<br>frontend/src/pages/RequirementDetail.css:1070<br>frontend/src/pages/RequirementDetail.css:1157<br>frontend/src/pages/RequirementDetail.css:3301 | |
| | `#991b1b` | 7 | frontend/src/components/AttachmentPanel.css:35<br>frontend/src/components/DiagramGenerator.css:154<br>frontend/src/components/MarkdownRender.css:42<br>frontend/src/components/RichDescriptionEditor/RichDescriptionEditor.css:146<br>frontend/src/components/SubTaskPanel.css:182<br>frontend/src/index.css:1263 | |
| | `#fde68a` | 6 | frontend/src/components/AttachmentPanel.css:87<br>frontend/src/components/RequirementBatch/RequirementBatch.css:215<br>frontend/src/components/SubTaskPanel.css:783<br>frontend/src/components/SubTaskPanel.css:810<br>frontend/src/pages/RequirementDetail.css:97<br>frontend/src/pages/RequirementDetail.css:3298 | |
| | `#f7f8fb` | 6 | frontend/src/components/DiagramGenerator.css:57<br>frontend/src/components/DiagramGenerator.css:140<br>frontend/src/components/MarkdownRender.css:11<br>frontend/src/components/MarkdownRender.css:29<br>frontend/src/components/MarkdownRender.css:61<br>frontend/src/utils/exportDesignPdf.tsx:84 | |
| | `rgba(79,70,229,0.1)` | 6 | frontend/src/components/SubTaskPanel.css:474<br>frontend/src/components/SubTaskPanel.css:605<br>frontend/src/index.css:1341<br>frontend/src/pages/ProjectIssueMonitor.css:35<br>frontend/src/pages/SettingsClaude.css:86<br>frontend/src/pages/SettingsRoles.css:133 | |
| | `#c7d2fe` | 6 | frontend/src/index.css:647<br>frontend/src/components/CodingChat.tsx:345<br>frontend/src/pages/SettingsSkills.tsx:296<br>frontend/src/pages/SettingsSkills.tsx:305<br>frontend/src/pages/SettingsSkills.tsx:306<br>frontend/src/utils/exportDesignPdf.tsx:77 | |
| | `#fcd34d` | 6 | frontend/src/pages/ProjectDetail.css:427<br>frontend/src/pages/RequirementDetail.css:3782<br>frontend/src/pages/RequirementsList.css:183<br>frontend/src/components/Layout.tsx:65<br>frontend/src/components/Layout.tsx:92<br>frontend/src/components/Layout.tsx:93 | |
| | `#fecaca` | 5 | frontend/src/components/AttachmentPanel.css:34<br>frontend/src/components/DiagramGenerator.css:152<br>frontend/src/components/MarkdownRender.css:41<br>frontend/src/pages/RequirementDetail.css:2085<br>frontend/src/utils/exportDesignPdf.tsx:86 | |
| | `rgba(239,68,68,0.08)` | 5 | frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:263<br>frontend/src/components/SubTaskPanel.css:513<br>frontend/src/components/SubTaskPanel.css:573<br>frontend/src/pages/Login.css:67<br>frontend/src/pages/SettingsAgentServers.css:296 | |
| | `#1f2937` | 5 | frontend/src/components/ImportDesignDialog.css:45<br>frontend/src/components/ImportDesignDialog.css:72<br>frontend/src/components/ImportDesignDialog.css:96<br>frontend/src/components/ImportDesignDialog.css:156<br>frontend/src/components/ImportDesignDialog.css:169 | |
| | `rgba(16,185,129,0.18)` | 5 | frontend/src/components/Layout.css:251<br>frontend/src/components/SubTaskPanel.css:1472<br>frontend/src/pages/RequirementDetail.css:1503<br>frontend/src/pages/RequirementsCalendar.css:203<br>frontend/src/pages/RequirementsCalendar.css:324 | |
| | `rgba(15,23,42,0.08)` | 5 | frontend/src/components/MarkdownRender.css:65<br>frontend/src/pages/KnowledgePage.css:188<br>frontend/src/pages/Login.css:20<br>frontend/src/pages/ProjectCodeReview.css:812<br>frontend/src/pages/RequirementDetail.css:2738 | |
| | `#7dd3fc` | 5 | frontend/src/components/SubTaskPanel.css:778<br>frontend/src/components/SubTaskPanel.css:807<br>frontend/src/pages/RequirementDetail.css:1064<br>frontend/src/pages/RequirementDetail.css:1188<br>frontend/src/pages/RequirementDetail.css:3293 | |
| | `#6ee7b7` | 5 | frontend/src/components/SubTaskPanel.css:786<br>frontend/src/components/SubTaskPanel.css:813<br>frontend/src/components/SubTaskPanel.css:874<br>frontend/src/pages/RequirementDetail.css:1069<br>frontend/src/pages/RequirementDetail.css:3300 | |
| | `#fb923c` | 5 | frontend/src/components/SubTaskPanel.css:791<br>frontend/src/components/SubTaskPanel.css:800<br>frontend/src/components/SubTaskPanel.css:994<br>frontend/src/components/SubTaskPanel.css:1018<br>frontend/src/components/SubTaskPanel.css:1019 | |
| | `rgba(79,70,229,0.12)` | 5 | frontend/src/index.css:1311<br>frontend/src/pages/Login.css:61<br>frontend/src/pages/RequirementDetail.css:2245<br>frontend/src/pages/SettingsAgentServers.css:411<br>frontend/src/pages/SettingsRoles.css:77 | |
| | `#f5f3ff` | 5 | frontend/src/pages/ProjectDetail.css:1026<br>frontend/src/pages/RequirementDetail.css:80<br>frontend/src/pages/RequirementDetail.css:3716<br>frontend/src/pages/RequirementsList.css:340<br>frontend/src/api/client.ts:3060 | |
| | `rgba(79,70,229,0.06)` | 5 | frontend/src/pages/RequirementDetail.css:1891<br>frontend/src/pages/RequirementDetail.css:3379<br>frontend/src/pages/RequirementDetail.css:3574<br>frontend/src/pages/RequirementDetail.css:3575<br>frontend/src/pages/RequirementsCalendar.css:108 | |
| | `rgba(79,70,229,0.04)` | 4 | frontend/src/components/AttachmentPanel.css:61<br>frontend/src/components/AttachmentPanel.css:293<br>frontend/src/components/SubTaskPanel.css:1193<br>frontend/src/pages/ProjectDetail.css:524 | |
| | `rgba(79,70,229,0.25)` | 4 | frontend/src/components/DevSourceBadge.css:29<br>frontend/src/components/DevSourceBadge.css:69<br>frontend/src/pages/SchedulesPage.css:55<br>frontend/src/pages/SettingsClaude.css:88 | |
| | `rgba(15,23,42,0.05)` | 4 | frontend/src/components/Layout.css:347<br>frontend/src/components/Layout.css:362<br>frontend/src/components/Layout.css:368<br>frontend/src/pages/SettingsAgentServers.css:132 | |
| | `rgba(245,158,11,0.08)` | 4 | frontend/src/components/SubTaskPanel.css:314<br>frontend/src/components/SubTaskPanel.css:335<br>frontend/src/index.css:516<br>frontend/src/pages/Settings.css:148 | |
| | `#a78bfa` | 4 | frontend/src/components/SubTaskPanel.css:779<br>frontend/src/components/SubTaskPanel.css:808<br>frontend/src/pages/RequirementDetail.css:1066<br>frontend/src/pages/RequirementDetail.css:3295 | |
| | `#5eead4` | 4 | frontend/src/components/SubTaskPanel.css:790<br>frontend/src/components/SubTaskPanel.css:816<br>frontend/src/pages/RequirementDetail.css:1072<br>frontend/src/pages/RequirementDetail.css:3303 | |
| | `#38bdf8` | 4 | frontend/src/components/SubTaskPanel.css:830<br>frontend/src/components/SubTaskPanel.css:935<br>frontend/src/components/SubTaskPanel.css:1001<br>frontend/src/pages/RequirementDetail.css:1095 | |
| … 另有 134 行 | | |

**建议**：在 index.css 的 :root 里为这 174 种颜色补上语义令牌（--color-<用途>），再逐处改成 var(--color-<用途>)。

### 13. 13 处 <MarkdownRender> 没有挂在 .md-prose 上

- **规则**：R10 — Markdown 渲染点未挂 .md-prose
- **严重度**：P2
- **编号**：`R10-019`
- **复核**：未经模型复核（沿用规则文案）

frontend/src/styles/markdown-prose.css 是全站唯一的 Markdown 排版来源，CLAUDE.md 里也写明了"不要再抄一份排版"。但这些渲染点的外层用的是各自发明的类（`chat-content chat-content-md`、`sub-log-text`、`sub-log-md sub-log-error-md`、`sub-log-md sub-log-done-md`、`sub-phase-panel-body`），于是同一份 Markdown 在不同页面会呈现出不同的字号、行高与间距。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)

涉及位置：

- `frontend/src/components/DeepRefineChat.tsx:590` — chat-content chat-content-md
- `frontend/src/components/SubTaskPanel.tsx:350` — sub-log-text
- `frontend/src/components/SubTaskPanel.tsx:364` — sub-log-md sub-log-error-md
- `frontend/src/components/SubTaskPanel.tsx:373` — sub-log-md sub-log-done-md
- `frontend/src/components/SubTaskPanel.tsx:433` — sub-phase-panel-body
- `frontend/src/components/SubTaskPanel.tsx:526` — sub-log-tool-result-body
- `frontend/src/components/SubTaskPanel.tsx:1732` — sub-plan-body
- `frontend/src/components/SubTaskPanel.tsx:1786` — sub-result-body
- `frontend/src/components/SubTaskPanel.tsx:1815` — sub-result-body
- `frontend/src/components/SubTaskPanel.tsx:1868` — sub-report-body
- `frontend/src/components/SubTaskPanel.tsx:2956` — sub-report-body
- `frontend/src/pages/RequirementDetail.tsx:304` — coding-line coding-line-message coding-message-md

| 当前外层类 | 位置 |
|---|---|
| | `chat-content chat-content-md` | frontend/src/components/DeepRefineChat.tsx:590 | |
| | `sub-log-text` | frontend/src/components/SubTaskPanel.tsx:350 | |
| | `sub-log-md sub-log-error-md` | frontend/src/components/SubTaskPanel.tsx:364 | |
| | `sub-log-md sub-log-done-md` | frontend/src/components/SubTaskPanel.tsx:373 | |
| | `sub-phase-panel-body` | frontend/src/components/SubTaskPanel.tsx:433 | |
| | `sub-log-tool-result-body` | frontend/src/components/SubTaskPanel.tsx:526 | |
| | `sub-plan-body` | frontend/src/components/SubTaskPanel.tsx:1732 | |
| | `sub-result-body` | frontend/src/components/SubTaskPanel.tsx:1786 | |
| | `sub-result-body` | frontend/src/components/SubTaskPanel.tsx:1815 | |
| | `sub-report-body` | frontend/src/components/SubTaskPanel.tsx:1868 | |
| | `sub-report-body` | frontend/src/components/SubTaskPanel.tsx:2956 | |
| | `coding-line coding-line-message coding-message-md` | frontend/src/pages/RequirementDetail.tsx:304 | |
| | `coding-line coding-line-message coding-message-md` | frontend/src/pages/RequirementDetail.tsx:317 | |

**建议**：给这些渲染点的外层元素补上 md-prose 类；页面私有的差异用 --md-prose-size / --md-prose-lh 变量覆盖，不要另起一套排版。

### 14. 「link」在 23 个页面的计算样式不一致

- **规则**：R11 — 同一控件在不同页面的计算样式不一致
- **严重度**：P2
- **编号**：`R11-003`
- **复核**：未经模型复核（沿用规则文案）

同一个语义控件 `link` 在不同页面上被渲染成了 2 种样式。差异出现在：padding、borderRadius、border、height。用户在同一趟操作里来回切换页面时，会看到"同一个按钮/输入框忽然换了个尺寸"，这是"整体不统一"最直接的观感来源。

差异明细：

- **padding** — /: 10 20 10 20 ｜ /settings: 8 16 8 16
- **borderRadius** — /: 0 ｜ /settings: 4
- **border** — /: 0 0 0 3 ｜ /settings: 0 0 2 0
- **height** — /: 41 ｜ /settings: 39

证据：

- `/` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png) · sha256 `004ff765865e7d5b…`
- `/wizard` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/wizard.png) · sha256 `3218d52a241676a4…`
- `/projects` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects.png) · sha256 `c95ec8478c5be981…`
- `/projects/add` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects-add.png) · sha256 `5f1ba4a1b2115753…`
- `/requirements` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements.png) · sha256 `3c6a13839ba52b54…`
- `/requirements/calendar` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements-calendar.png) · sha256 `18912bf3e2e33b8e…`
- `/schedules` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/schedules.png) · sha256 `6173562bbfc3a8e4…`
- `/knowledge` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/knowledge.png) · sha256 `8f4828fcf3463506…`

涉及位置：

- `artifacts/ui-audit/20261025-2015/screenshots/root.png` — / → a.nav-item "项目"
- `artifacts/ui-audit/20261025-2015/screenshots/wizard.png` — /wizard → a.nav-item "仪表盘"
- `artifacts/ui-audit/20261025-2015/screenshots/projects.png` — /projects → a.nav-item "仪表盘"
- `artifacts/ui-audit/20261025-2015/screenshots/projects-add.png` — /projects/add → a.nav-item "仪表盘"
- `artifacts/ui-audit/20261025-2015/screenshots/requirements.png` — /requirements → a.nav-item "仪表盘"
- `artifacts/ui-audit/20261025-2015/screenshots/requirements-calendar.png` — /requirements/calendar → a.nav-item "仪表盘"
- `artifacts/ui-audit/20261025-2015/screenshots/schedules.png` — /schedules → a.nav-item "仪表盘"
- `artifacts/ui-audit/20261025-2015/screenshots/knowledge.png` — /knowledge → a.nav-item "仪表盘"
- `artifacts/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca.png` — /projects/proj_ad4d4ac4b4bae7ca → a.nav-item "仪表盘"
- `artifacts/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca-issue-monitor.png` — /projects/proj_ad4d4ac4b4bae7ca/issue-monitor → a.nav-item "仪表盘"
- `artifacts/ui-audit/20261025-2015/screenshots/requirements-req-6654d9308c6a5b8c.png` — /requirements/req_6654d9308c6a5b8c → a.nav-item "仪表盘"
- `artifacts/ui-audit/20261025-2015/screenshots/reviews-crv-cd6c9d59e4b6f04c.png` — /reviews/crv_cd6c9d59e4b6f04c → a.nav-item "仪表盘"

**建议**：把这些差异收敛到 index.css 的同一套类上：把 padding/borderRadius/border/height 这些属性提到共享规则里，页面私有差异用额外的修饰类叠加，而不是各页各写各的。

### 15. 「card」在 9 个页面的计算样式不一致

- **规则**：R11 — 同一控件在不同页面的计算样式不一致
- **严重度**：P2
- **编号**：`R11-004`
- **复核**：未经模型复核（沿用规则文案）

同一个语义控件 `card` 在不同页面上被渲染成了 9 种样式。差异出现在：fontSize、lineHeight、fontWeight、padding、borderRadius、border、color、background、height、minWidth。用户在同一趟操作里来回切换页面时，会看到"同一个按钮/输入框忽然换了个尺寸"，这是"整体不统一"最直接的观感来源。

差异明细：

- **fontSize** — /: 14 ｜ /wizard: 14 ｜ /projects/add: 14 ｜ /requirements: 12 ｜ /settings/acl: 14 ｜ /settings/roles: 14 ｜ /projects/proj_ad4d4ac4b4bae7ca: 13 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 14 ｜ /requirements/req_6654d9308c6a5b8c: 10.5
- **lineHeight** — /: 21 ｜ /wizard: 21 ｜ /projects/add: 21 ｜ /requirements: 18 ｜ /settings/acl: 21 ｜ /settings/roles: 21 ｜ /projects/proj_ad4d4ac4b4bae7ca: 19.5 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 21 ｜ /requirements/req_6654d9308c6a5b8c: 10.5
- **fontWeight** — /: 400 ｜ /wizard: 400 ｜ /projects/add: 400 ｜ /requirements: 400 ｜ /settings/acl: 400 ｜ /settings/roles: 400 ｜ /projects/proj_ad4d4ac4b4bae7ca: 400 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 600 ｜ /requirements/req_6654d9308c6a5b8c: 600
- **padding** — /: 20 20 20 20 ｜ /wizard: 24 24 24 24 ｜ /projects/add: 24 24 24 24 ｜ /requirements: 0 0 0 0 ｜ /settings/acl: 20 20 20 20 ｜ /settings/roles: 20 20 20 20 ｜ /projects/proj_ad4d4ac4b4bae7ca: 0 0 0 0 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 0 0 0 0 ｜ /requirements/req_6654d9308c6a5b8c: 0 0 0 0
- **borderRadius** — /: 8 ｜ /wizard: 10 ｜ /projects/add: 8 ｜ /requirements: 0 ｜ /settings/acl: 8 ｜ /settings/roles: 8 ｜ /projects/proj_ad4d4ac4b4bae7ca: 0 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 0 ｜ /requirements/req_6654d9308c6a5b8c: 0
- **border** — /: 1 1 1 1 ｜ /wizard: 1 1 1 1 ｜ /projects/add: 1 1 1 1 ｜ /requirements: 0 0 0 0 ｜ /settings/acl: 1 1 1 1 ｜ /settings/roles: 1 1 1 1 ｜ /projects/proj_ad4d4ac4b4bae7ca: 0 0 0 0 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 0 0 0 0 ｜ /requirements/req_6654d9308c6a5b8c: 0 0 0 0
- **color** — /: rgb(30, 41, 59) ｜ /wizard: rgb(30, 41, 59) ｜ /projects/add: rgb(30, 41, 59) ｜ /requirements: rgb(100, 116, 139) ｜ /settings/acl: rgb(30, 41, 59) ｜ /settings/roles: rgb(30, 41, 59) ｜ /projects/proj_ad4d4ac4b4bae7ca: rgb(30, 41, 59) ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: rgb(30, 41, 59) ｜ /requirements/req_6654d9308c6a5b8c: rgb(148, 163, 184)
- **background** — /: rgb(255, 255, 255) ｜ /wizard: rgb(255, 255, 255) ｜ /projects/add: rgb(255, 255, 255) ｜ /requirements: rgba(0, 0, 0, 0) ｜ /settings/acl: rgb(255, 255, 255) ｜ /settings/roles: rgb(255, 255, 255) ｜ /projects/proj_ad4d4ac4b4bae7ca: rgba(0, 0, 0, 0) ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: rgba(0, 0, 0, 0) ｜ /requirements/req_6654d9308c6a5b8c: rgba(0, 0, 0, 0)
- **height** — /: 113.5 ｜ /wizard: 438 ｜ /projects/add: 360.5 ｜ /requirements: auto ｜ /settings/acl: 252 ｜ /settings/roles: 530 ｜ /projects/proj_ad4d4ac4b4bae7ca: 81.5 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 21 ｜ /requirements/req_6654d9308c6a5b8c: 10.5
- **minWidth** — /: auto ｜ /wizard: 0 ｜ /projects/add: 0 ｜ /requirements: 0 ｜ /settings/acl: auto ｜ /settings/roles: auto ｜ /projects/proj_ad4d4ac4b4bae7ca: 0 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: auto ｜ /requirements/req_6654d9308c6a5b8c: auto

证据：

- `/` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png) · sha256 `004ff765865e7d5b…`
- `/wizard` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/wizard.png) · sha256 `3218d52a241676a4…`
- `/projects/add` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects-add.png) · sha256 `5f1ba4a1b2115753…`
- `/requirements` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements.png) · sha256 `3c6a13839ba52b54…`
- `/settings/acl` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-acl.png) · sha256 `562dbec07838e016…`
- `/settings/roles` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-roles.png) · sha256 `9065eb583b1a7520…`
- `/projects/proj_ad4d4ac4b4bae7ca` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca.png) · sha256 `9ad4f0079ad72c33…`
- `/projects/proj_ad4d4ac4b4bae7ca/issue-monitor` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca-issue-monitor.png) · sha256 `8a10ca34ee0876b0…`

涉及位置：

- `artifacts/ui-audit/20261025-2015/screenshots/root.png` — / → div.stat-card "1项目数"
- `artifacts/ui-audit/20261025-2015/screenshots/wizard.png` — /wizard → div.wizard-card "📁 新建项目项目名称项目目录/›Users›f1›workspace↩..🔷"
- `artifacts/ui-audit/20261025-2015/screenshots/projects-add.png` — /projects/add → div.add-project-card "Git 仓库地址:分支（可选）:平台 Token（私有仓库必填）:— 无（仅适用"
- `artifacts/ui-audit/20261025-2015/screenshots/requirements.png` — /requirements → span.req-card-mobile-project "📁 nova-ui-audit-fixture"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-acl.png` — /settings/acl → div.role-card "开发者内置developer编辑可访问项目、知识库、周报、AI对话，可执行需求开"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-roles.png` — /settings/roles → div.role-card "需求分析师评估需求完整性，读取项目代码并提出关键问题，把需求打磨到可直接交给 A"
- `artifacts/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca.png` — /projects/proj_ad4d4ac4b4bae7ca → table.pr-table.table-cards "ID↕类型标题↕状态↕Agent 服务器Token成本创建时间↕更新时间↕req"
- `artifacts/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca-issue-monitor.png` — /projects/proj_ad4d4ac4b4bae7ca/issue-monitor → span.im-mode-card-head "立即"
- `artifacts/ui-audit/20261025-2015/screenshots/requirements-req-6654d9308c6a5b8c.png` — /requirements/req_6654d9308c6a5b8c → span.draft-field-card-caption "模型"

**建议**：把这些差异收敛到 index.css 的同一套类上：把 fontSize/lineHeight/fontWeight/padding/borderRadius/border/color/background/height/minWidth 这些属性提到共享规则里，页面私有差异用额外的修饰类叠加，而不是各页各写各的。

### 16. 「panel」在 13 个页面的计算样式不一致

- **规则**：R11 — 同一控件在不同页面的计算样式不一致
- **严重度**：P2
- **编号**：`R11-005`
- **复核**：未经模型复核（沿用规则文案）

同一个语义控件 `panel` 在不同页面上被渲染成了 12 种样式。差异出现在：fontSize、lineHeight、fontWeight、padding、borderRadius、border、background、height。用户在同一趟操作里来回切换页面时，会看到"同一个按钮/输入框忽然换了个尺寸"，这是"整体不统一"最直接的观感来源。

差异明细：

- **fontSize** — /: 14 ｜ /settings: 14 ｜ /settings/users: 14 ｜ /settings/acl: 14 ｜ /settings/roles: 15 ｜ /settings/agent-servers: 14 ｜ /settings/llm: 14 ｜ /settings/subtask: 14 ｜ /settings/git-sync: 14 ｜ /settings/database: 14 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 14 ｜ /requirements/req_6654d9308c6a5b8c: 14
- **lineHeight** — /: 21 ｜ /settings: 21 ｜ /settings/users: 21 ｜ /settings/acl: 21 ｜ /settings/roles: 22.5 ｜ /settings/agent-servers: 21 ｜ /settings/llm: 21 ｜ /settings/subtask: 21 ｜ /settings/git-sync: 21 ｜ /settings/database: 21 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 21 ｜ /requirements/req_6654d9308c6a5b8c: 21
- **fontWeight** — /: 400 ｜ /settings: 400 ｜ /settings/users: 400 ｜ /settings/acl: 400 ｜ /settings/roles: 700 ｜ /settings/agent-servers: 400 ｜ /settings/llm: 400 ｜ /settings/subtask: 400 ｜ /settings/git-sync: 400 ｜ /settings/database: 400 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 400 ｜ /requirements/req_6654d9308c6a5b8c: 400
- **padding** — /: 20 20 20 20 ｜ /settings: 20 20 20 20 ｜ /settings/users: 20 20 20 20 ｜ /settings/acl: 20 20 20 20 ｜ /settings/roles: 0 0 0 0 ｜ /settings/agent-servers: 20 20 20 20 ｜ /settings/llm: 20 20 20 20 ｜ /settings/subtask: 20 20 20 20 ｜ /settings/git-sync: 20 20 20 20 ｜ /settings/database: 20 20 20 20 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 20 20 20 20 ｜ /requirements/req_6654d9308c6a5b8c: 4 16 4 16
- **borderRadius** — /: 8 ｜ /settings: 8 ｜ /settings/users: 8 ｜ /settings/acl: 8 ｜ /settings/roles: 0 ｜ /settings/agent-servers: 8 ｜ /settings/llm: 8 ｜ /settings/subtask: 8 ｜ /settings/git-sync: 8 ｜ /settings/database: 8 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 8 ｜ /requirements/req_6654d9308c6a5b8c: 8
- **border** — /: 1 1 1 1 ｜ /settings: 1 1 1 1 ｜ /settings/users: 1 1 1 1 ｜ /settings/acl: 1 1 1 1 ｜ /settings/roles: 0 0 0 0 ｜ /settings/agent-servers: 1 1 1 1 ｜ /settings/llm: 1 1 1 1 ｜ /settings/subtask: 1 1 1 1 ｜ /settings/git-sync: 1 1 1 1 ｜ /settings/database: 1 1 1 1 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 1 1 1 1 ｜ /requirements/req_6654d9308c6a5b8c: 1 1 1 1
- **background** — /: rgb(255, 255, 255) ｜ /settings: rgb(255, 255, 255) ｜ /settings/users: rgb(255, 255, 255) ｜ /settings/acl: rgb(255, 255, 255) ｜ /settings/roles: rgba(0, 0, 0, 0) ｜ /settings/agent-servers: rgb(255, 255, 255) ｜ /settings/llm: rgb(255, 255, 255) ｜ /settings/subtask: rgb(255, 255, 255) ｜ /settings/git-sync: rgb(255, 255, 255) ｜ /settings/database: rgb(255, 255, 255) ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: rgb(255, 255, 255) ｜ /requirements/req_6654d9308c6a5b8c: rgb(255, 255, 255)
- **height** — /: 172 ｜ /settings: 200.5 ｜ /settings/users: 177.797 ｜ /settings/acl: 348 ｜ /settings/roles: 22.5 ｜ /settings/agent-servers: 509.438 ｜ /settings/llm: 471 ｜ /settings/subtask: 449.5 ｜ /settings/git-sync: 241.5 ｜ /settings/database: 667.781 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 908.5 ｜ /requirements/req_6654d9308c6a5b8c: 124.5

证据：

- `/` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png) · sha256 `004ff765865e7d5b…`
- `/settings` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings.png) · sha256 `df0d0040254d1a1a…`
- `/settings/claude` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-claude.png) · sha256 `a688263e38e7013b…`
- `/settings/users` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-users.png) · sha256 `058fe27b19e00e77…`
- `/settings/acl` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-acl.png) · sha256 `562dbec07838e016…`
- `/settings/roles` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-roles.png) · sha256 `9065eb583b1a7520…`
- `/settings/agent-servers` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-agent-servers.png) · sha256 `42fde69ac2aa311b…`
- `/settings/llm` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-llm.png) · sha256 `47b0a6b8ca99149d…`

涉及位置：

- `artifacts/ui-audit/20261025-2015/screenshots/root.png` — / → div.projects-section "项目列表+ 添加名称类型路径状态更新时间nova-ui-audit-fixtur"
- `artifacts/ui-audit/20261025-2015/screenshots/settings.png` — /settings → div.settings-section "平台 Token配置 GitHub / GitLab / Gitea / Bit"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-claude.png` — /settings/claude → div.settings-section.claude-configs "Claude CLI 配置管理多套 Claude 配置（名称 + Base UR"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-users.png` — /settings/users → div.settings-section "用户管理管理登录账号、为用户分配角色（决定权限范围）与项目（决定可见项目）。多角"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-acl.png` — /settings/acl → div.settings-section "角色权限管理用户角色及其权限范围。此处的「角色」是用户权限角色（区别于 AI 角"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-roles.png` — /settings/roles → h3.settings-section-title "角色管理"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-agent-servers.png` — /settings/agent-servers → div.settings-section "🖥️ Agent 服务器配置远程 Linux / macOS 服务器作为 Cl"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-llm.png` — /settings/llm → div.settings-section "直连 LLM 配置配置 OpenAI 兼容的直连 LLM 通道（如 DeepSe"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-subtask.png` — /settings/subtask → div.settings-section "子任务执行策略控制子任务的排队与失败恢复。并发数按「项目」生效：同一项目最多同时"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-git-sync.png` — /settings/git-sync → div.settings-section "Git 同步设置design 阶段硬同步相关配置同步超时（秒）design 阶段"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-database.png` — /settings/database → div.settings-section "数据库NovaWorkbench 默认使用本地 SQLite 文件，无需任何配置"
- `artifacts/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca-issue-monitor.png` — /projects/proj_ad4d4ac4b4bae7ca/issue-monitor → div.settings-section.issue-monitor "Issue 监控为此项目开启「指定 GitHub Label 的 Issue 自"

**建议**：把这些差异收敛到 index.css 的同一套类上：把 fontSize/lineHeight/fontWeight/padding/borderRadius/border/background/height 这些属性提到共享规则里，页面私有差异用额外的修饰类叠加，而不是各页各写各的。

### 17. 「table」在 6 个页面的计算样式不一致

- **规则**：R11 — 同一控件在不同页面的计算样式不一致
- **严重度**：P2
- **编号**：`R11-007`
- **复核**：未经模型复核（沿用规则文案）

同一个语义控件 `table` 在不同页面上被渲染成了 6 种样式。差异出现在：fontSize、lineHeight、fontWeight、padding、borderRadius、border、color、background、height、minWidth。用户在同一趟操作里来回切换页面时，会看到"同一个按钮/输入框忽然换了个尺寸"，这是"整体不统一"最直接的观感来源。

差异明细：

- **fontSize** — /: 14 ｜ /projects: 14 ｜ /requirements: 14 ｜ /requirements/calendar: 14 ｜ /settings/users: 14 ｜ /projects/proj_ad4d4ac4b4bae7ca: 10
- **lineHeight** — /: 21 ｜ /projects: 21 ｜ /requirements: 21 ｜ /requirements/calendar: 21 ｜ /settings/users: 21 ｜ /projects/proj_ad4d4ac4b4bae7ca: 15
- **fontWeight** — /: 400 ｜ /projects: 400 ｜ /requirements: 400 ｜ /requirements/calendar: 400 ｜ /settings/users: 400 ｜ /projects/proj_ad4d4ac4b4bae7ca: 600
- **padding** — /: 0 0 0 0 ｜ /projects: 0 0 0 0 ｜ /requirements: 0 0 0 0 ｜ /requirements/calendar: 4 4 6 4 ｜ /settings/users: 0 0 0 0 ｜ /projects/proj_ad4d4ac4b4bae7ca: 0 0 0 0
- **borderRadius** — /: 0 ｜ /projects: 0 ｜ /requirements: 8 ｜ /requirements/calendar: 0 ｜ /settings/users: 0 ｜ /projects/proj_ad4d4ac4b4bae7ca: 0
- **border** — /: 0 0 0 0 ｜ /projects: 0 0 0 0 ｜ /requirements: 1 1 1 1 ｜ /requirements/calendar: 0 1 0 0 ｜ /settings/users: 0 0 0 0 ｜ /projects/proj_ad4d4ac4b4bae7ca: 0 0 0 0
- **color** — /: rgb(30, 41, 59) ｜ /projects: rgb(30, 41, 59) ｜ /requirements: rgb(30, 41, 59) ｜ /requirements/calendar: rgb(30, 41, 59) ｜ /settings/users: rgb(30, 41, 59) ｜ /projects/proj_ad4d4ac4b4bae7ca: rgb(148, 163, 184)
- **background** — /: rgba(0, 0, 0, 0) ｜ /projects: rgba(0, 0, 0, 0) ｜ /requirements: rgb(255, 255, 255) ｜ /requirements/calendar: rgba(0, 0, 0, 0) ｜ /settings/users: rgba(0, 0, 0, 0) ｜ /projects/proj_ad4d4ac4b4bae7ca: rgba(0, 0, 0, 0)
- **height** — /: 82 ｜ /projects: 164 ｜ /requirements: 81.5 ｜ /requirements/calendar: 110 ｜ /settings/users: 81.7969 ｜ /projects/proj_ad4d4ac4b4bae7ca: 15
- **minWidth** — /: 0 ｜ /projects: 0 ｜ /requirements: 0 ｜ /requirements/calendar: auto ｜ /settings/users: 0 ｜ /projects/proj_ad4d4ac4b4bae7ca: 0

证据：

- `/` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/root.png) · sha256 `004ff765865e7d5b…`
- `/projects` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects.png) · sha256 `c95ec8478c5be981…`
- `/requirements` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements.png) · sha256 `3c6a13839ba52b54…`
- `/requirements/calendar` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements-calendar.png) · sha256 `18912bf3e2e33b8e…`
- `/settings/users` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-users.png) · sha256 `058fe27b19e00e77…`
- `/projects/proj_ad4d4ac4b4bae7ca` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca.png) · sha256 `9ad4f0079ad72c33…`

涉及位置：

- `artifacts/ui-audit/20261025-2015/screenshots/root.png` — / → div.project-table-wrap "名称类型路径状态更新时间nova-ui-audit-fixtureUnknown"
- `artifacts/ui-audit/20261025-2015/screenshots/projects.png` — /projects → table.project-table "名称简介类型路径状态需求数问题数想法数更新时间操作☆nova-ui-audit-"
- `artifacts/ui-audit/20261025-2015/screenshots/requirements.png` — /requirements → div.req-table-wrap.desktop-only "拖到此处移出主需求类型标题状态模型项目Agent更新时间需求UI 一致性审计夹具"
- `artifacts/ui-audit/20261025-2015/screenshots/requirements-calendar.png` — /requirements/calendar → div.cal-month-cell.is-creatable "1"
- `artifacts/ui-audit/20261025-2015/screenshots/settings-users.png` — /settings/users → table.project-table "用户名显示名角色项目数状态最近登录admin管理员管理员—0启用2026/10/"
- `artifacts/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca.png` — /projects/proj_ad4d4ac4b4bae7ca → span.pr-th-sortable-arrow "↕"

**建议**：把这些差异收敛到 index.css 的同一套类上：把 fontSize/lineHeight/fontWeight/padding/borderRadius/border/color/background/height/minWidth 这些属性提到共享规则里，页面私有差异用额外的修饰类叠加，而不是各页各写各的。

### 18. 「input-choice」在 4 个页面的计算样式不一致

- **规则**：R11 — 同一控件在不同页面的计算样式不一致
- **严重度**：P2
- **编号**：`R11-011`
- **复核**：未经模型复核（沿用规则文案）

同一个语义控件 `input-choice` 在不同页面上被渲染成了 3 种样式。差异出现在：height、minWidth。用户在同一趟操作里来回切换页面时，会看到"同一个按钮/输入框忽然换了个尺寸"，这是"整体不统一"最直接的观感来源。

差异明细：

- **height** — /requirements: 15 ｜ /settings/subtask: 13 ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 13
- **minWidth** — /requirements: 0 ｜ /settings/subtask: auto ｜ /projects/proj_ad4d4ac4b4bae7ca/issue-monitor: 0

证据：

- `/requirements` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements.png) · sha256 `3c6a13839ba52b54…`
- `/projects/proj_ad4d4ac4b4bae7ca` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca.png) · sha256 `9ad4f0079ad72c33…`
- `/settings/subtask` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/settings-subtask.png) · sha256 `e7643e35f58cc75e…`
- `/projects/proj_ad4d4ac4b4bae7ca/issue-monitor` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca-issue-monitor.png) · sha256 `8a10ca34ee0876b0…`

涉及位置：

- `artifacts/ui-audit/20261025-2015/screenshots/requirements.png` — /requirements → input.rb-check
- `artifacts/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca.png` — /projects/proj_ad4d4ac4b4bae7ca → input.rb-check
- `artifacts/ui-audit/20261025-2015/screenshots/settings-subtask.png` — /settings/subtask → input
- `artifacts/ui-audit/20261025-2015/screenshots/projects-proj-ad4d4ac4b4bae7ca-issue-monitor.png` — /projects/proj_ad4d4ac4b4bae7ca/issue-monitor → input

**建议**：把这些差异收敛到 index.css 的同一套类上：把 height/minWidth 这些属性提到共享规则里，页面私有差异用额外的修饰类叠加，而不是各页各写各的。

### 19. 「btn-icon」在 2 个页面的计算样式不一致

- **规则**：R11 — 同一控件在不同页面的计算样式不一致
- **严重度**：P2
- **编号**：`R11-012`
- **复核**：未经模型复核（沿用规则文案）

同一个语义控件 `btn-icon` 在不同页面上被渲染成了 2 种样式。差异出现在：fontSize、padding、borderRadius、border、background、height。用户在同一趟操作里来回切换页面时，会看到"同一个按钮/输入框忽然换了个尺寸"，这是"整体不统一"最直接的观感来源。

差异明细：

- **fontSize** — /requirements/calendar: 13 ｜ /requirements/req_6654d9308c6a5b8c: 12
- **padding** — /requirements/calendar: 8 16 8 16 ｜ /requirements/req_6654d9308c6a5b8c: 0 0 0 0
- **borderRadius** — /requirements/calendar: 6 ｜ /requirements/req_6654d9308c6a5b8c: 0
- **border** — /requirements/calendar: 1 1 1 1 ｜ /requirements/req_6654d9308c6a5b8c: 0 0 0 0
- **background** — /requirements/calendar: rgb(255, 255, 255) ｜ /requirements/req_6654d9308c6a5b8c: rgba(0, 0, 0, 0)
- **height** — /requirements/calendar: 33 ｜ /requirements/req_6654d9308c6a5b8c: 13

证据：

- `/requirements/calendar` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements-calendar.png) · sha256 `18912bf3e2e33b8e…`
- `/requirements/req_6654d9308c6a5b8c` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/requirements-req-6654d9308c6a5b8c.png) · sha256 `6c3da44eafeac753…`

涉及位置：

- `artifacts/ui-audit/20261025-2015/screenshots/requirements-calendar.png` — /requirements/calendar → button.btn.btn-icon "‹"
- `artifacts/ui-audit/20261025-2015/screenshots/requirements-req-6654d9308c6a5b8c.png` — /requirements/req_6654d9308c6a5b8c → svg.btn-icon

**建议**：把这些差异收敛到 index.css 的同一套类上：把 fontSize/padding/borderRadius/border/background/height 这些属性提到共享规则里，页面私有差异用额外的修饰类叠加，而不是各页各写各的。

### 20. 28 个令牌的值被写死 786 处，而不是引用 var(--token)

- **规则**：R2 — 令牌值被写成字面量而非 var(--token)
- **严重度**：P2
- **编号**：`R2-002`
- **复核**：未经模型复核（沿用规则文案）

这些位置直接写了令牌对应的色值字面量，而不是 var(--token) 引用。一旦调整令牌本身，这些位置不会跟着变——正是"改了一处、别处没跟上"的不一致来源。其中 index.css 自身就有 15 个令牌被就地写死。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `--color-text-secondary`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `--color-primary`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `--color-surface`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `--color-border`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `--color-text-muted`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `--color-text`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `--color-hover`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `--color-error`

涉及位置：

- `frontend/src/components/AttachmentPanel.css:28` — #64748b
- `frontend/src/components/AttachmentPanel.css:124` — #64748b
- `frontend/src/components/AttachmentPanel.css:206` — #64748b
- `frontend/src/components/AttachmentPanel.css:269` — #64748b
- `frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:45` — #64748B
- `frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:413` — #64748B
- `frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:447` — #64748B
- `frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:460` — #64748B
- `frontend/src/components/AttachmentPanel.css:60` — #4f46e5
- `frontend/src/components/AttachmentPanel.css:116` — #4f46e5
- `frontend/src/components/AttachmentPanel.css:135` — #4f46e5
- `frontend/src/components/AttachmentPanel.css:183` — #4f46e5

| 应替换为 | 出现次数 | 位置 |
|---|---|---|
| | `var(--color-text-secondary)` | 97 | frontend/src/components/AttachmentPanel.css:28<br>frontend/src/components/AttachmentPanel.css:124<br>frontend/src/components/AttachmentPanel.css:206<br>frontend/src/components/AttachmentPanel.css:269<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:45<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:413<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:447<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:460 | |
| | `var(--color-primary)` | 91 | frontend/src/components/AttachmentPanel.css:60<br>frontend/src/components/AttachmentPanel.css:116<br>frontend/src/components/AttachmentPanel.css:135<br>frontend/src/components/AttachmentPanel.css:183<br>frontend/src/components/AttachmentPanel.css:247<br>frontend/src/components/AttachmentPanel.css:292<br>frontend/src/components/AttachmentPanel.css:294<br>frontend/src/components/AttachmentPanel.css:310 | |
| | `var(--color-surface)` | 88 | frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:56<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:135<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:185<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:328<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:420<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:475<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:486<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:566 | |
| | `var(--color-border)` | 74 | frontend/src/components/AttachmentPanel.css:165<br>frontend/src/components/AttachmentPanel.css:261<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:133<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:326<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:440<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:474<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:568<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:606 | |
| | `var(--color-text-muted)` | 66 | frontend/src/components/AttachmentPanel.css:76<br>frontend/src/components/AttachmentPanel.css:158<br>frontend/src/components/AttachmentPanel.css:238<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:71<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:103<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:218<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:253<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:273 | |
| | `var(--color-text)` | 48 | frontend/src/components/AttachmentPanel.css:23<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:52<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:211<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:337<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:502<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:658<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:711<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:845 | |
| | `var(--color-hover)` | 39 | frontend/src/components/AttachmentPanel.css:234<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:23<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:396<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:548<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:645<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:690<br>frontend/src/components/DiagramGenerator.css:134<br>frontend/src/components/RichDescriptionEditor/RichDescriptionEditor.css:48 | |
| | `var(--color-error)` | 27 | frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:262<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:264<br>frontend/src/components/ImportDesignDialog.css:121<br>frontend/src/components/SubTaskPanel.css:514<br>frontend/src/components/SubTaskPanel.css:515<br>frontend/src/components/SubTaskPanel.css:785<br>frontend/src/components/SubTaskPanel.css:811<br>frontend/src/components/SubTaskPanel.css:873 | |
| | `var(--color-warning-deep)` | 24 | frontend/src/components/AttachmentPanel.css:198<br>frontend/src/components/RequirementBatch/RequirementBatch.css:216<br>frontend/src/components/SubTaskPanel.css:313<br>frontend/src/components/SubTaskPanel.css:336<br>frontend/src/components/SubTaskPanel.css:622<br>frontend/src/components/SubTaskPanel.css:633<br>frontend/src/index.css:474<br>frontend/src/index.css:517 | |
| | `var(--color-success)` | 23 | frontend/src/components/ImportDesignDialog.css:137<br>frontend/src/components/Layout.css:250<br>frontend/src/components/SubTaskPanel.css:1618<br>frontend/src/pages/ProjectCodeReview.css:1017<br>frontend/src/pages/ProjectDetail.css:90<br>frontend/src/pages/ProjectDetail.css:773<br>frontend/src/pages/RequirementDetail.css:1895<br>frontend/src/pages/RequirementDetail.css:2110 | |
| | `var(--color-bg)` | 20 | frontend/src/components/AttachmentPanel.css:53<br>frontend/src/components/AttachmentPanel.css:260<br>frontend/src/components/AttachmentPanel.css:282<br>frontend/src/components/ImportDesignDialog.css:145<br>frontend/src/components/MarkdownRender.css:85<br>frontend/src/components/RequirementBatch/RequirementBatch.css:98<br>frontend/src/components/RequirementBatch/RequirementBatch.css:175<br>frontend/src/components/RequirementTreeToggle.css:91 | |
| | `var(--color-warning-soft)` | 19 | frontend/src/components/AttachmentPanel.css:199<br>frontend/src/components/RichDescriptionEditor/RichDescriptionEditor.css:135<br>frontend/src/components/SubTaskPanel.css:173<br>frontend/src/index.css:474<br>frontend/src/index.css:552<br>frontend/src/index.css:611<br>frontend/src/index.css:619<br>frontend/src/pages/KnowledgePage.css:135 | |
| | `var(--color-accent-cool)` | 18 | frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:580<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:822<br>frontend/src/index.css:476<br>frontend/src/index.css:614<br>frontend/src/index.css:624<br>frontend/src/index.css:646<br>frontend/src/pages/ProjectIssueMonitor.css:153<br>frontend/src/pages/RequirementDetail.css:1613 | |
| | `var(--color-warning)` | 18 | frontend/src/index.css:451<br>frontend/src/index.css:1782<br>frontend/src/index.css:1783<br>frontend/src/pages/ProjectCodeReview.css:308<br>frontend/src/pages/ProjectDetail.css:95<br>frontend/src/pages/ProjectDetail.css:777<br>frontend/src/pages/RequirementDetail.css:2144<br>frontend/src/pages/RequirementDetail.css:2170 | |
| | `var(--analyst)` | 18 | frontend/src/index.css:451<br>frontend/src/index.css:1782<br>frontend/src/index.css:1783<br>frontend/src/pages/ProjectCodeReview.css:308<br>frontend/src/pages/ProjectDetail.css:95<br>frontend/src/pages/ProjectDetail.css:777<br>frontend/src/pages/RequirementDetail.css:2144<br>frontend/src/pages/RequirementDetail.css:2170 | |
| | `var(--color-active)` | 17 | frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:150<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:619<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:624<br>frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:882<br>frontend/src/components/RichDescriptionEditor/RichDescriptionEditor.css:17<br>frontend/src/components/SubTaskPanel.css:151<br>frontend/src/index.css:230<br>frontend/src/index.css:647 | |
| | `var(--color-success-deep)` | 17 | frontend/src/components/RequirementBatch/RequirementBatch.css:211<br>frontend/src/components/SubTaskPanel.css:479<br>frontend/src/components/SubTaskPanel.css:612<br>frontend/src/components/SubTaskPanel.css:646<br>frontend/src/components/SubTaskPanel.css:1829<br>frontend/src/components/SubTaskPanel.css:1969<br>frontend/src/index.css:615<br>frontend/src/index.css:618 | |
| | `var(--color-error-deep)` | 16 | frontend/src/components/AttachmentPanel.css:130<br>frontend/src/components/RequirementBatch/RequirementBatch.css:143<br>frontend/src/components/RequirementBatch/RequirementBatch.css:144<br>frontend/src/components/SubTaskPanel.css:1959<br>frontend/src/index.css:164<br>frontend/src/index.css:164<br>frontend/src/index.css:621<br>frontend/src/index.css:626 | |
| | `var(--color-error-soft)` | 14 | frontend/src/components/AttachmentPanel.css:220<br>frontend/src/components/PdfUploadDialog.css:42<br>frontend/src/components/RichDescriptionEditor/RichDescriptionEditor.css:145<br>frontend/src/components/SubTaskPanel.css:181<br>frontend/src/index.css:621<br>frontend/src/index.css:626<br>frontend/src/index.css:644<br>frontend/src/pages/KnowledgePage.css:146 | |
| | `var(--color-accent-warm)` | 11 | frontend/src/components/SubTaskPanel.css:1135<br>frontend/src/index.css:527<br>frontend/src/pages/KnowledgePage.css:135<br>frontend/src/pages/KnowledgePage.css:145<br>frontend/src/pages/SettingsAgentServers.css:34<br>frontend/src/pages/SettingsAgentServers.css:235<br>frontend/src/api/client.ts:3059<br>frontend/src/pages/ProjectDetail.tsx:235 | |
| | `var(--color-success-soft)` | 10 | frontend/src/components/RichDescriptionEditor/RichDescriptionEditor.css:140<br>frontend/src/components/SubTaskPanel.css:177<br>frontend/src/index.css:615<br>frontend/src/index.css:618<br>frontend/src/index.css:625<br>frontend/src/pages/KnowledgePage.css:138<br>frontend/src/pages/ProjectDetail.css:112<br>frontend/src/pages/RequirementDetail.css:64 | |
| | `var(--developer)` | 7 | frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:389<br>frontend/src/index.css:453<br>frontend/src/pages/RequirementDetail.css:1575<br>frontend/src/pages/SchedulesPage.css:185<br>frontend/src/pages/SchedulesPage.css:221<br>frontend/src/pages/SchedulesPage.css:277<br>frontend/src/pages/SchedulesPage.css:737 | |
| | `var(--shadow-card)` | 7 | frontend/src/components/SubTaskPanel.css:27<br>frontend/src/index.css:709<br>frontend/src/index.css:905<br>frontend/src/pages/ProjectDetail.css:893<br>frontend/src/pages/RequirementDetail.css:2609<br>frontend/src/pages/RequirementDetail.css:3408<br>frontend/src/pages/WizardPage.css:213 | |
| | `var(--color-primary-hover)` | 6 | frontend/src/components/DevSourceBadge.css:30<br>frontend/src/components/SubTaskPanel.css:607<br>frontend/src/index.css:645<br>frontend/src/index.css:647<br>frontend/src/pages/RequirementsList.css:78<br>frontend/src/pages/SchedulesPage.css:216 | |
| | `var(--architect)` | 5 | frontend/src/index.css:452<br>frontend/src/pages/RequirementDetail.css:1576<br>frontend/src/pages/RequirementsCalendar.css:200<br>frontend/src/pages/RequirementsCalendar.css:321<br>frontend/src/pages/SchedulesPage.css:53 | |
| | `var(--color-parchment)` | 3 | frontend/src/pages/RequirementDetail.css:2780<br>frontend/src/pages/RequirementDetail.css:3401<br>frontend/src/pages/RequirementDetail.css:3577 | |
| | `var(--shadow-popover)` | 2 | frontend/src/pages/RequirementDetail.css:3840<br>frontend/src/pages/RequirementDetail.css:3860 | |
| | `var(--shadow-modal)` | 1 | frontend/src/pages/ProjectDetail.css:965 | |

**建议**：把这 786 处替换成对应的 var(--token) 引用；index.css 里的规则尤其应当走令牌，否则改令牌等于没改。

### 21. 类名 .input 被用了 5 处，但没有任何样式表定义它

- **规则**：R4 — 使用了没有任何样式表定义的类名
- **严重度**：P2
- **编号**：`R4-004`
- **复核**：未经模型复核（沿用规则文案）

frontend/src 里引用了 .input（5 处，分布在 1 个文件），但 frontend/src/**/*.css 里没有任何选择器定义它。用了没有定义的类名，元素会退化成浏览器默认样式——同一个控件在不同页面就会呈现出不同的默认外观。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)

涉及位置：

- `frontend/src/pages/RequirementDetail.tsx:3849` — input
- `frontend/src/pages/RequirementDetail.tsx:3856` — input
- `frontend/src/pages/RequirementDetail.tsx:3870` — input
- `frontend/src/pages/RequirementDetail.tsx:3885` — input
- `frontend/src/pages/RequirementDetail.tsx:3903` — input

**建议**：要么在 index.css 里补上 .input 的定义（并纳入 .btn-* / .card-* 这类通用规范），要么把它换成已有的规范类名。

### 22. 类名 .modal-label 被用了 4 处，但没有任何样式表定义它

- **规则**：R4 — 使用了没有任何样式表定义的类名
- **严重度**：P2
- **编号**：`R4-005`
- **复核**：未经模型复核（沿用规则文案）

frontend/src 里引用了 .modal-label（4 处，分布在 1 个文件），但 frontend/src/**/*.css 里没有任何选择器定义它。用了没有定义的类名，元素会退化成浏览器默认样式——同一个控件在不同页面就会呈现出不同的默认外观。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)

涉及位置：

- `frontend/src/components/calendar/RequirementModal.tsx:125` — modal-label
- `frontend/src/components/calendar/RequirementModal.tsx:134` — modal-label
- `frontend/src/components/calendar/RequirementModal.tsx:144` — modal-label
- `frontend/src/components/calendar/RequirementModal.tsx:157` — modal-label

**建议**：要么在 index.css 里补上 .modal-label 的定义（并纳入 .btn-* / .card-* 这类通用规范），要么把它换成已有的规范类名。

### 23. 类名 .test-result 被用了 4 处，但没有任何样式表定义它

- **规则**：R4 — 使用了没有任何样式表定义的类名
- **严重度**：P2
- **编号**：`R4-006`
- **复核**：未经模型复核（沿用规则文案）

frontend/src 里引用了 .test-result（4 处，分布在 2 个文件），但 frontend/src/**/*.css 里没有任何选择器定义它。用了没有定义的类名，元素会退化成浏览器默认样式——同一个控件在不同页面就会呈现出不同的默认外观。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)

涉及位置：

- `frontend/src/pages/SettingsAgentServers.tsx:767` — test-result
- `frontend/src/pages/SettingsAgentServers.tsx:769` — test-result
- `frontend/src/pages/SettingsClaude.tsx:277` — test-result
- `frontend/src/pages/SettingsClaude.tsx:279` — test-result

**建议**：要么在 index.css 里补上 .test-result 的定义（并纳入 .btn-* / .card-* 这类通用规范），要么把它换成已有的规范类名。

### 24. 类名 .form-label 被用了 4 处，但没有任何样式表定义它

- **规则**：R4 — 使用了没有任何样式表定义的类名
- **严重度**：P2
- **编号**：`R4-007`
- **复核**：未经模型复核（沿用规则文案）

frontend/src 里引用了 .form-label（4 处，分布在 1 个文件），但 frontend/src/**/*.css 里没有任何选择器定义它。用了没有定义的类名，元素会退化成浏览器默认样式——同一个控件在不同页面就会呈现出不同的默认外观。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)

涉及位置：

- `frontend/src/pages/SettingsSkills.tsx:636` — form-label
- `frontend/src/pages/SettingsSkills.tsx:645` — form-label
- `frontend/src/pages/SettingsSkills.tsx:657` — form-label
- `frontend/src/pages/SettingsSkills.tsx:667` — form-label

**建议**：要么在 index.css 里补上 .form-label 的定义（并纳入 .btn-* / .card-* 这类通用规范），要么把它换成已有的规范类名。

### 25. btn 类名存在两套写法：19 个 `btn-*` vs 20 个 `*-btn`

- **规则**：R5 — 类名存在前缀式/后缀式两套写法
- **严重度**：P2
- **编号**：`R5-008`
- **复核**：未经模型复核（沿用规则文案）

仓库主流是前缀式（btn-*，共 398 处引用），但还有 20 个后缀式类名（41 处引用，分布在 12 个文件）：.tab-btn、.image-toolbar-btn、.kvp-topbar-btn、.rb-tray-btn、.merge-hint-btn、.sub-archive-action-btn、.design-toggle-btn、.pr-link-btn、.cal-view-toggle-btn、.attachment-panel__upload-btn 等。两套写法并存时，想统一改一个btn样式就得同时记住两种类名。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)

涉及位置：

- `frontend/src/pages/ProjectDetail.tsx:1829` — tab-btn
- `frontend/src/components/MarkdownRender.tsx:222` — image-toolbar-btn
- `frontend/src/pages/KnowledgeViewPage.tsx:162` — kvp-topbar-btn
- `frontend/src/components/RequirementBatch/RequirementBatch.tsx:130` — rb-tray-btn
- `frontend/src/pages/RequirementDetail.tsx:571` — merge-hint-btn
- `frontend/src/components/SubTaskPanel.tsx:782` — sub-archive-action-btn

| 类名 | 引用次数 | 首次出现 |
|---|---|---|
| | `.tab-btn` | 8 | frontend/src/pages/ProjectDetail.tsx:1829 | |
| | `.image-toolbar-btn` | 4 | frontend/src/components/MarkdownRender.tsx:222 | |
| | `.kvp-topbar-btn` | 4 | frontend/src/pages/KnowledgeViewPage.tsx:162 | |
| | `.rb-tray-btn` | 3 | frontend/src/components/RequirementBatch/RequirementBatch.tsx:130 | |
| | `.merge-hint-btn` | 3 | frontend/src/pages/RequirementDetail.tsx:571 | |
| | `.sub-archive-action-btn` | 2 | frontend/src/components/SubTaskPanel.tsx:782 | |
| | `.design-toggle-btn` | 2 | frontend/src/pages/RequirementDetail.tsx:4140 | |
| | `.pr-link-btn` | 2 | frontend/src/pages/RequirementDetail.tsx:6421 | |
| | `.cal-view-toggle-btn` | 2 | frontend/src/pages/RequirementsCalendar.tsx:207 | |
| | `.attachment-panel__upload-btn` | 1 | frontend/src/components/AttachmentPanel.tsx:264 | |
| | `.usage-bar-btn` | 1 | frontend/src/components/ContextUsageBar.tsx:155 | |
| | `.fp-pick-btn` | 1 | frontend/src/components/FolderPicker.tsx:122 | |
| | `.sidebar-close-btn` | 1 | frontend/src/components/Layout.tsx:283 | |
| | `.sub-card-stop-btn` | 1 | frontend/src/components/SubTaskPanel.tsx:1612 | |
| | `.sub-plan-toggle-btn` | 1 | frontend/src/components/SubTaskPanel.tsx:1743 | |
| | `.sub-result-toggle-btn` | 1 | frontend/src/components/SubTaskPanel.tsx:1792 | |
| | `.sub-summary-action-btn` | 1 | frontend/src/components/SubTaskPanel.tsx:2930 | |
| | `.sub-report-toggle-btn` | 1 | frontend/src/components/SubTaskPanel.tsx:2962 | |
| | `.sub-composer-image-btn` | 1 | frontend/src/components/SubTaskPanel.tsx:3148 | |
| | `.star-btn` | 1 | frontend/src/pages/ProjectsList.tsx:368 | |

**建议**：把后缀式类名统一改成前缀式（.btn-*），或者在 index.css 里为后缀式类名补一份与前缀式等价的定义。

### 26. 18 个 `*-card` 后缀式类名，仓库里没有对应的 `card-*` 前缀式约定

- **规则**：R5 — 类名存在前缀式/后缀式两套写法
- **严重度**：P2
- **编号**：`R5-009`
- **复核**：未经模型复核（沿用规则文案）

这些类名都用了后缀式 `*-card`（48 处引用，分布在 15 个文件）：.preflight-field-card、.draft-field-card、.stat-card、.launch-radio-card、.kb-card、.wizard-card、.role-card、.launch-base-card、.launch-mode-card、.sub-card 等。而仓库里没有任何 `card-*` 前缀式类名，也就是说这一类元素从来没有被约定过统一写法——每个页面都在自己发明名字，这正是不统一的来源。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)

涉及位置：

- `frontend/src/components/BranchPicker.tsx:77` — preflight-field-card
- `frontend/src/pages/RequirementDetail.tsx:4743` — draft-field-card
- `frontend/src/pages/Dashboard.tsx:40` — stat-card
- `frontend/src/components/CreateRequirementForm/LaunchPlanSection.tsx:616` — launch-radio-card
- `frontend/src/pages/KnowledgePage.tsx:153` — kb-card
- `frontend/src/pages/WizardPage.tsx:192` — wizard-card

| 类名 | 引用次数 | 首次出现 |
|---|---|---|
| | `.preflight-field-card` | 15 | frontend/src/components/BranchPicker.tsx:77 | |
| | `.draft-field-card` | 7 | frontend/src/pages/RequirementDetail.tsx:4743 | |
| | `.stat-card` | 4 | frontend/src/pages/Dashboard.tsx:40 | |
| | `.launch-radio-card` | 3 | frontend/src/components/CreateRequirementForm/LaunchPlanSection.tsx:616 | |
| | `.kb-card` | 3 | frontend/src/pages/KnowledgePage.tsx:153 | |
| | `.wizard-card` | 3 | frontend/src/pages/WizardPage.tsx:192 | |
| | `.role-card` | 2 | frontend/src/pages/SettingsACLRoles.tsx:149 | |
| | `.launch-base-card` | 1 | frontend/src/components/CreateRequirementForm/LaunchPlanSection.tsx:653 | |
| | `.launch-mode-card` | 1 | frontend/src/components/CreateRequirementForm/LaunchPlanSection.tsx:434 | |
| | `.sub-card` | 1 | frontend/src/components/SubTaskPanel.tsx:1467 | |
| | `.add-project-card` | 1 | frontend/src/pages/AddProject.tsx:74 | |
| | `.review-card` | 1 | frontend/src/pages/KnowledgePage.tsx:254 | |
| | `.login-card` | 1 | frontend/src/pages/Login.tsx:44 | |
| | `.im-mode-card` | 1 | frontend/src/pages/ProjectIssueMonitor.tsx:217 | |
| | `.spec-card` | 1 | frontend/src/pages/RequirementDetail.tsx:4122 | |
| | `.usage-receipt-card` | 1 | frontend/src/pages/RequirementDetail.tsx:4356 | |
| | `.adjust-history-card` | 1 | frontend/src/pages/RequirementDetail.tsx:4461 | |
| | `.server-card` | 1 | frontend/src/pages/SettingsAgentServers.tsx:722 | |

**建议**：在 index.css 里定下 `.card-*` 前缀式约定，把这 18 个类名改过去；同名元素只保留一个共享基类。

### 27. input 类名存在两套写法：1 个 `input-*` vs 17 个 `*-input`

- **规则**：R5 — 类名存在前缀式/后缀式两套写法
- **严重度**：P2
- **编号**：`R5-010`
- **复核**：未经模型复核（沿用规则文案）

仓库主流是前缀式（input-*，共 3 处引用），但还有 17 个后缀式类名（181 处引用，分布在 39 个文件）：.form-input、.preflight-field-input、.gpg-key-input、.search-input、.model-price-input、.model-select-input、.weekly-date-input、.role-model-input、.import-design-file-input、.pdf-upload-file-input 等。两套写法并存时，想统一改一个input样式就得同时记住两种类名。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)

涉及位置：

- `frontend/src/components/CodingChat.tsx:393` — form-input
- `frontend/src/components/BranchPicker.tsx:85` — preflight-field-input
- `frontend/src/components/GeneratedGPGKeyModal.tsx:119` — gpg-key-input
- `frontend/src/pages/ProjectDetail.tsx:2484` — search-input
- `frontend/src/pages/SettingsClaude.tsx:346` — model-price-input
- `frontend/src/components/ModelSelect.tsx:231` — model-select-input

| 类名 | 引用次数 | 首次出现 |
|---|---|---|
| | `.form-input` | 141 | frontend/src/components/CodingChat.tsx:393 | |
| | `.preflight-field-input` | 15 | frontend/src/components/BranchPicker.tsx:85 | |
| | `.gpg-key-input` | 4 | frontend/src/components/GeneratedGPGKeyModal.tsx:119 | |
| | `.search-input` | 3 | frontend/src/pages/ProjectDetail.tsx:2484 | |
| | `.model-price-input` | 3 | frontend/src/pages/SettingsClaude.tsx:346 | |
| | `.model-select-input` | 2 | frontend/src/components/ModelSelect.tsx:231 | |
| | `.weekly-date-input` | 2 | frontend/src/pages/ProjectWeeklyReport.tsx:179 | |
| | `.role-model-input` | 2 | frontend/src/pages/SettingsRoles.tsx:227 | |
| | `.import-design-file-input` | 1 | frontend/src/components/ImportDesignDialog.tsx:263 | |
| | `.pdf-upload-file-input` | 1 | frontend/src/components/PdfUploadDialog.tsx:113 | |
| | `.req-edit-title-input` | 1 | frontend/src/components/RequirementEditForm/RequirementEditForm.tsx:150 | |
| | `.weekly-author-input` | 1 | frontend/src/pages/ProjectWeeklyReport.tsx:224 | |
| | `.weekly-rule-input` | 1 | frontend/src/pages/ProjectWeeklyReport.tsx:277 | |
| | `.req-tag-input` | 1 | frontend/src/pages/RequirementDetail.tsx:4175 | |
| | `.model-name-input` | 1 | frontend/src/pages/SettingsClaude.tsx:340 | |
| | `.role-prompt-input` | 1 | frontend/src/pages/SettingsRoles.tsx:271 | |
| | `.review-requirements-input` | 1 | frontend/src/pages/codeReview/ReviewComposer.tsx:384 | |

**建议**：把后缀式类名统一改成前缀式（.input-*），或者在 index.css 里为后缀式类名补一份与前缀式等价的定义。

### 28. 15 个 `*-panel` 后缀式类名，仓库里没有对应的 `panel-*` 前缀式约定

- **规则**：R5 — 类名存在前缀式/后缀式两套写法
- **严重度**：P2
- **编号**：`R5-011`
- **复核**：未经模型复核（沿用规则文案）

这些类名都用了后缀式 `*-panel`（34 处引用，分布在 16 个文件）：.coding-panel、.modal-risk-panel、.chat-panel、.sub-phase-panel、.sub-panel、.session-panel、.draft-action-panel、.attachment-panel、.coding-chat-panel、.deep-refine-panel 等。而仓库里没有任何 `panel-*` 前缀式类名，也就是说这一类元素从来没有被约定过统一写法——每个页面都在自己发明名字，这正是不统一的来源。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)

涉及位置：

- `frontend/src/components/DocRefineChat.tsx:771` — coding-panel
- `frontend/src/components/DesignCodingImmediateModal.tsx:711` — modal-risk-panel
- `frontend/src/components/CodingChat.tsx:285` — chat-panel
- `frontend/src/components/SubTaskPanel.tsx:467` — sub-phase-panel
- `frontend/src/components/SubTaskPanel.tsx:2729` — sub-panel
- `frontend/src/pages/RequirementDetail.tsx:4223` — session-panel

| 类名 | 引用次数 | 首次出现 |
|---|---|---|
| | `.coding-panel` | 9 | frontend/src/components/DocRefineChat.tsx:771 | |
| | `.modal-risk-panel` | 5 | frontend/src/components/DesignCodingImmediateModal.tsx:711 | |
| | `.chat-panel` | 4 | frontend/src/components/CodingChat.tsx:285 | |
| | `.sub-phase-panel` | 2 | frontend/src/components/SubTaskPanel.tsx:467 | |
| | `.sub-panel` | 2 | frontend/src/components/SubTaskPanel.tsx:2729 | |
| | `.session-panel` | 2 | frontend/src/pages/RequirementDetail.tsx:4223 | |
| | `.draft-action-panel` | 2 | frontend/src/pages/RequirementDetail.tsx:4672 | |
| | `.attachment-panel` | 1 | frontend/src/components/AttachmentPanel.tsx:177 | |
| | `.coding-chat-panel` | 1 | frontend/src/components/CodingChat.tsx:250 | |
| | `.deep-refine-panel` | 1 | frontend/src/components/DeepRefineChat.tsx:535 | |
| | `.doc-refine-panel` | 1 | frontend/src/components/DocRefineChat.tsx:589 | |
| | `.review-panel` | 1 | frontend/src/pages/KnowledgePage.tsx:247 | |
| | `.knowledge-read-panel` | 1 | frontend/src/pages/RequirementDetail.tsx:420 | |
| | `.conflict-panel` | 1 | frontend/src/pages/RequirementDetail.tsx:6395 | |
| | `.merge-panel` | 1 | frontend/src/pages/RequirementDetail.tsx:6411 | |

**建议**：在 index.css 里定下 `.panel-*` 前缀式约定，把这 15 个类名改过去；同名元素只保留一个共享基类。

### 29. badge 类名存在两套写法：1 个 `badge-*` vs 20 个 `*-badge`

- **规则**：R5 — 类名存在前缀式/后缀式两套写法
- **严重度**：P2
- **编号**：`R5-012`
- **复核**：未经模型复核（沿用规则文案）

仓库主流是前缀式（badge-*，共 2 处引用），但还有 20 个后缀式类名（48 处引用，分布在 20 个文件）：.status-badge、.dev-source-badge、.kb-type-badge、.kind-badge、.dev-mode-badge、.compressed-badge、.gpg-badge、.cr-sev-badge、.attachment-panel__archive-badge、.fp-git-badge 等。两套写法并存时，想统一改一个badge样式就得同时记住两种类名。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)

涉及位置：

- `frontend/src/components/StatusChips.tsx:61` — status-badge
- `frontend/src/components/DevSourceBadge.tsx:37` — dev-source-badge
- `frontend/src/pages/KnowledgePage.tsx:155` — kb-type-badge
- `frontend/src/pages/ProjectDetail.tsx:1102` — kind-badge
- `frontend/src/pages/RequirementDetail.tsx:4030` — dev-mode-badge
- `frontend/src/pages/RequirementDetail.tsx:4077` — compressed-badge

| 类名 | 引用次数 | 首次出现 |
|---|---|---|
| | `.status-badge` | 11 | frontend/src/components/StatusChips.tsx:61 | |
| | `.dev-source-badge` | 5 | frontend/src/components/DevSourceBadge.tsx:37 | |
| | `.kb-type-badge` | 5 | frontend/src/pages/KnowledgePage.tsx:155 | |
| | `.kind-badge` | 4 | frontend/src/pages/ProjectDetail.tsx:1102 | |
| | `.dev-mode-badge` | 4 | frontend/src/pages/RequirementDetail.tsx:4030 | |
| | `.compressed-badge` | 3 | frontend/src/pages/RequirementDetail.tsx:4077 | |
| | `.gpg-badge` | 2 | frontend/src/pages/SettingsTokens.tsx:433 | |
| | `.cr-sev-badge` | 2 | frontend/src/pages/codeReview/ReviewReport.tsx:348 | |
| | `.attachment-panel__archive-badge` | 1 | frontend/src/components/AttachmentPanel.tsx:295 | |
| | `.fp-git-badge` | 1 | frontend/src/components/FolderPicker.tsx:119 | |
| | `.session-context-badge` | 1 | frontend/src/components/SessionContextStrip.tsx:74 | |
| | `.kb-badge` | 1 | frontend/src/pages/KnowledgePage.tsx:134 | |
| | `.run-status-badge` | 1 | frontend/src/pages/ProjectDetail.tsx:2367 | |
| | `.closed-badge` | 1 | frontend/src/pages/RequirementDetail.tsx:3970 | |
| | `.schedules-type-badge` | 1 | frontend/src/pages/SchedulesPage.tsx:423 | |
| | `.builtin-badge` | 1 | frontend/src/pages/SettingsACLRoles.tsx:153 | |
| | `.disabled-badge` | 1 | frontend/src/pages/SettingsACLRoles.tsx:154 | |
| | `.claude-active-badge` | 1 | frontend/src/pages/SettingsClaude.tsx:246 | |
| | `.platform-badge` | 1 | frontend/src/pages/SettingsTokens.tsx:420 | |
| | `.admin-badge` | 1 | frontend/src/pages/SettingsUsers.tsx:179 | |

**建议**：把后缀式类名统一改成前缀式（.badge-*），或者在 index.css 里为后缀式类名补一份与前缀式等价的定义。

### 30. 容器有 6 套并存的类名（32 处引用）

- **规则**：R6 — 同类容器存在多套类名
- **严重度**：P2
- **编号**：`R6-013`
- **复核**：未经模型复核（沿用规则文案）

同一类容器用了 6 个不同的类名：.modal-box（25）、.preflight-box（3）、.diagram-generator-box（1）、.import-design-box（1）、.md-viewer-box（1）、.pdf-upload-box（1）。它们多半想做同一件事（一个带边框圆角阴影的块），但各自定义了一套样式，视觉上就会漂移。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)

涉及位置：

- `frontend/src/components/CodingChat.tsx:436` — modal-box
- `frontend/src/components/DesignCodingImmediateModal.tsx:265` — preflight-box
- `frontend/src/components/DiagramGenerator.tsx:120` — diagram-generator-box
- `frontend/src/components/ImportDesignDialog.tsx:199` — import-design-box
- `frontend/src/components/MarkdownViewer.tsx:26` — md-viewer-box
- `frontend/src/components/PdfUploadDialog.tsx:99` — pdf-upload-box

**建议**：抽一个通用类（例如 .surface-box）写进 index.css，各页面改用它，页面私有差异再叠加自己的修饰类。

### 31. 区块标题有 34 套并存的类名（95 处引用）

- **规则**：R7 — 同类区块标题存在多套类名
- **严重度**：P2
- **编号**：`R7-014`
- **复核**：未经模型复核（沿用规则文案）

同一类区块标题用了 34 个不同的类名：.section-header（33）、.modal-header（7）、.detail-header（6）、.crv-header（4）、.deep-refine-header（3）、.preflight-header（3）、.kb-card-header（3）、.page-header（3）、.create-req-form-header（2）、.coding-line-phase-header（2） 等。它们多半想做同一件事（一个带边框圆角阴影的块），但各自定义了一套样式，视觉上就会漂移。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)

涉及位置：

- `frontend/src/pages/Dashboard.tsx:59` — section-header
- `frontend/src/components/CodingChat.tsx:440` — modal-header
- `frontend/src/pages/CodeReviewViewPage.tsx:194` — detail-header
- `frontend/src/pages/CodeReviewViewPage.tsx:194` — crv-header
- `frontend/src/components/CodingChat.tsx:251` — deep-refine-header
- `frontend/src/components/DesignCodingImmediateModal.tsx:266` — preflight-header
- `frontend/src/pages/KnowledgePage.tsx:154` — kb-card-header
- `frontend/src/pages/ProjectCodeReview.tsx:765` — page-header

**建议**：抽一个通用类（例如 .section-header）写进 index.css，各页面改用它，页面私有差异再叠加自己的修饰类。

### 32. 5 个状态类名绕开了 .status-badge 体系

- **规则**：R8 — 状态色存在并行词汇表或旁路着色
- **严重度**：P2
- **编号**：`R8-016`
- **复核**：未经模型复核（沿用规则文案）

这些类名承担状态着色，但不在 index.css 的 .status-badge.status-* 体系内：.status-chips、.status-badge、.kind-badge、.status-error、.status-active。旁路着色意味着同一状态在不同页面可能是不同颜色。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)

涉及位置：

- `frontend/src/components/StatusChips.tsx:54` — status-chips
- `frontend/src/components/StatusChips.tsx:61` — status-badge
- `frontend/src/pages/ProjectDetail.tsx:1102` — kind-badge
- `frontend/src/pages/ProjectWeeklyReport.tsx:361` — status-error
- `frontend/src/pages/SettingsUsers.tsx:185` — status-active

**建议**：把这些类名收敛到 .status-badge 体系，或者在 index.css 里给它们统一引用状态色令牌。

### 33. 3 个文件里塞了大量内联样式（合计 505 处 style={{ }}）

- **规则**：R9 — 内联样式密度过高，压过共享类
- **严重度**：P2
- **编号**：`R9-018`
- **复核**：未经模型复核（沿用规则文案）

内联样式优先级高于任何 class 规则，所以同一套 .btn 样式在这些页面上会被就地改写，造成"同一个按钮在不同页面内边距/圆角不一样"。当前最集中的是：frontend/src/pages/ProjectDetail.tsx（152 处）、frontend/src/pages/RequirementDetail.tsx（86 处）、frontend/src/pages/SettingsSkills.tsx（71 处），三者合计占全部内联样式的 61%。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/)

涉及位置：

- `frontend/src/pages/ProjectDetail.tsx:194` — 152 处 style={{ }}
- `frontend/src/pages/RequirementDetail.tsx:3207` — 86 处 style={{ }}
- `frontend/src/pages/SettingsSkills.tsx:286` — 71 处 style={{ }}

**建议**：把这些内联样式里出现多次的值（尤其是 padding / border-radius / color）提成 index.css 的令牌或修饰类。

### 34. 2 个路由仍是占位页：/chat、/reports

- **规则**：R12 — 占位页
- **严重度**：P3
- **编号**：`R12-016`
- **复核**：未经模型复核（沿用规则文案）

这几个路由渲染的是 PlaceholderPages 占位内容。它们没有真正的界面，因此不参与跨页面样式对比——评审 UI 一致性时应把它们排除在范围外。

证据：

- `/chat` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/chat.png) · sha256 `05c8e74492b61e8b…`
- `/reports` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/screenshots/reports.png) · sha256 `9eaecb23045ec322…`

涉及位置：

- `artifacts/ui-audit/20261025-2015/screenshots/chat.png` — /chat
- `artifacts/ui-audit/20261025-2015/screenshots/reports.png` — /reports

**建议**：要么实现这两个页面，要么在导航里先隐藏入口，避免用户点到空页面。

### 35. 51 个颜色各有多种写法（大小写 / 简写不一致）

- **规则**：R3 — 同一颜色存在多种写法
- **严重度**：P3
- **编号**：`R3-003`
- **复核**：未经模型复核（沿用规则文案）

同一个颜色值在不同地方用了不同的字面量写法，例如 #fff 与 #ffffff、#64748B 与 #64748b。搜索替换时容易漏掉其中一种，也让"这两个色是不是同一个"难以一眼判断。例如：#1e293b → #1e293b / #1E293B；#64748b → #64748b / #64748B；#fef2f2 → #fef2f2 / #FEF2F2；#991b1b → #991b1b / #991B1B；#cbd5e1 → #cbd5e1 / #CBD5E1；#f8fafc → #f8fafc / #F8FAFC。

证据：

- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `#1e293b`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `#64748b`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `#fef2f2`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `#991b1b`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `#cbd5e1`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `#f8fafc`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `#4f46e5`
- `` — [截图](https://raw.githubusercontent.com/fengyily/novaworkbench/audit/ui-20261025-2015/docs/ui-audit/20261025-2015/) · `#94a3b8`

涉及位置：

- `frontend/src/components/AttachmentPanel.css:23` — 18×
- `frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:52` — 31×
- `frontend/src/components/AttachmentPanel.css:28` — 34×
- `frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:45` — 64×
- `frontend/src/components/AttachmentPanel.css:33` — 6×
- `frontend/src/pages/ProjectDetail.css:1125` — 4×
- `frontend/src/components/AttachmentPanel.css:35` — 4×
- `frontend/src/components/RichDescriptionEditor/RichDescriptionEditor.css:146` — 3×
- `frontend/src/components/AttachmentPanel.css:51` — 10×
- `frontend/src/components/CreateRequirementForm/CreateRequirementForm.css:174` — 11×
- `frontend/src/components/AttachmentPanel.css:53` — 9×
- `frontend/src/components/RequirementBatch/RequirementBatch.css:98` — 12×

| 归一化颜色 | 出现的写法 |
|---|---|
| | `#1e293b` | `#1e293b` / `#1E293B` | |
| | `#64748b` | `#64748b` / `#64748B` | |
| | `#fef2f2` | `#fef2f2` / `#FEF2F2` | |
| | `#991b1b` | `#991b1b` / `#991B1B` | |
| | `#cbd5e1` | `#cbd5e1` / `#CBD5E1` | |
| | `#f8fafc` | `#f8fafc` / `#F8FAFC` | |
| | `#4f46e5` | `#4f46e5` / `#4F46E5` | |
| | `#94a3b8` | `#94a3b8` / `#94A3B8` | |
| | `#fffbeb` | `#fffbeb` / `#FFFBEB` | |
| | `#fde68a` | `#fde68a` / `#FDE68A` | |
| | `#dc2626` | `#dc2626` / `#DC2626` | |
| | `#e2e8f0` | `#e2e8f0` / `#E2E8F0` | |
| | `#b45309` | `#b45309` / `#B45309` | |
| | `#fef3c7` | `#fef3c7` / `#FEF3C7` | |
| | `#fee2e2` | `#fee2e2` / `#FEE2E2` | |
| | `#f1f5f9` | `#f1f5f9` / `#F1F5F9` | |
| | `#ffffff` | `#fff` / `#ffffff` / `#FFFFFF` / `#FFF` | |
| | `#eef2ff` | `#EEF2FF` / `#eef2ff` | |
| | `#ef4444` | `#EF4444` / `#ef4444` | |
| | `#ede9fe` | `#ede9fe` / `#EDE9FE` | |
| | `#6d28d9` | `#6d28d9` / `#6D28D9` | |
| | `#cffafe` | `#cffafe` / `#CFFAFE` | |
| | `#0e7490` | `#0e7490` / `#0E7490` | |
| | `#4338ca` | `#4338ca` / `#4338CA` | |
| | `#dcfce7` | `#dcfce7` / `#DCFCE7` | |
| | `#10b981` | `#10b981` / `#10B981` | |
| | `#92400e` | `#92400e` / `#92400E` | |
| | `rgba(239,68,68,0.1)` | `rgba(239, 68, 68, 0.1)` / `rgba(239, 68, 68, 0.10)` | |
| | `#b91c1c` | `#b91c1c` / `#B91C1C` | |
| | `#0f172a` | `#0F172A` / `#0f172a` | |
| | `#a7f3d0` | `#A7F3D0` / `#a7f3d0` | |
| | `#d1fae5` | `#D1FAE5` / `#d1fae5` | |
| | `rgba(79,70,229,0.1)` | `rgba(79, 70, 229, 0.10)` / `rgba(79, 70, 229, 0.1)` | |
| | `rgba(79,70,229,0.3)` | `rgba(79, 70, 229, 0.30)` / `rgba(79, 70, 229, 0.3)` | |
| | `rgba(16,185,129,0.1)` | `rgba(16, 185, 129, 0.10)` / `rgba(16, 185, 129, 0.1)` | |
| | `#7dd3fc` | `#7DD3FC` / `#7dd3fc` | |
| | `#a78bfa` | `#A78BFA` / `#a78bfa` | |
| | `#fca5a5` | `#FCA5A5` / `#fca5a5` | |
| | `#6ee7b7` | `#6EE7B7` / `#6ee7b7` | |
| | `#fbbf24` | `#FBBF24` / `#fbbf24` | |
| … 另有 11 行 | | |

**建议**：统一为小写 6 位十六进制写法，并让所有位置走同一个 var(--token)。

## 跳过的路由

- `/knowledge/view/:id` — no knowledge id available
- `(api)` — /api/knowledge returned no usable id

## 复现方式

```bash
node scripts/ui-audit/run.mjs all --base-url http://localhost:9601 --run-id 20261025-2015
```

每张截图的 sha256 记录在 `manifest.json` 中，可用于校验证据未被替换。
