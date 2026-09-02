# NovaWorkbench

> **本地优先、AI 原生的开发者工作台** — 通过 Web UI 统一管理多个本地项目，把 AI 上下文（记忆 / 知识库）、需求驱动的 AI 完善 / 分析 / 设计、Claude Agent 代码生成、`docker compose` 运行会话、以及 GitHub / GitLab / Gitea 上的 AI 辅助 PR 审查整合到同一处。

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![React](https://img.shields.io/badge/React-19-20232A?logo=react&logoColor=61DAFB)](https://react.dev)
[![Vite](https://img.shields.io/badge/Vite-8-646CFF?logo=vite&logoColor=FFD62E)](https://vitejs.dev)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Docker](https://img.shields.io/badge/Docker-2496ED?logo=docker&logoColor=white)](https://www.docker.com)

NovaWorkbench 把"本地代码仓库 + AI 协作"装进一个单二进制应用：后端用 Go 编写并通过 `//go:embed` 把 React SPA 直接打包进可执行文件，前端零独立部署。所有 AI 能力都通过本地 `claude` CLI 子进程调用，AI 既能读懂你的项目，也能写你的代码、审你的 PR。

---

## 目录

- [背景：解决什么问题](#背景解决什么问题)
- [核心功能](#核心功能)
- [技术架构](#技术架构)
- [快速启动](#快速启动)
- [使用指南](#使用指南)
  - [5. 设置 Claude / 角色 / 数据库](#5-设置-claude--角色--数据库)
  - [6. 配置 Agent 服务器](#6-配置-agent-服务器)
- [配置说明](#配置说明)
- [开发](#开发)
- [部署](#部署)
- [常见问题](#常见问题)
- [贡献](#贡献)
- [许可证](#许可证)

---

## 背景：解决什么问题

### 痛点

1. **AI 上下文碎片化**：项目记忆、知识库、需求文档、PR 审查散落在多个工具里，AI 助手每次都要"从零开始理解"。
2. **需求 → 代码没有闭环**：需求常常停留在文档或聊天记录里，AI 生成的代码与原始需求缺乏可追溯链路。
3. **PR 审查纯靠人肉**：Claude / Copilot 之类的 AI 在 IDE 里能写代码，但谁来监督合并请求？
4. **本地项目无统一面板**：开发者机器上跑的 `docker compose` 进程、worktree 分支、跑批任务分散在不同终端里。
5. **多 LLM 配置难切换**：不同 Claude 模型 / 自定义 base URL / API token 切换麻烦，subagent 还经常回退到默认模型。

### NovaWorkbench 的答案

- **本地优先**：默认 SQLite + 单文件部署，所有数据（项目、需求、知识库、AI 用量、Job 日志）都在你机器上。
- **AI 闭环**：一条需求 → analyst 多轮对话完善 → architect 生成技术方案 → developer 生成代码 → review 角色审查 PR，每个阶段都可追溯、可回放、可重新生成。
- **AI 真实读写项目**：不依赖云端 API，而是本地 `claude` CLI 子进程，能像真人一样 `Read` / `Write` / `Bash` / `Grep` 你的仓库。
- **可视化一切**：从需求看板、Dashboard、用量统计，到 JobStore 实时 SSE 日流面板、Claude 工具调用可视化，过程全可见。
- **多平台 PR 审查**：GitHub / GitLab / Gitea 一个抽象，AI 自动读取 diff 并回贴评论。

---

## 核心功能

### 🧠 AI 上下文管理
- **Memories（记忆）**：跨项目持久化的 AI 偏好与笔记。
- **Knowledge Base（知识库）**：项目扫描器自动索引 `CLAUDE.md` / `AGENTS.md` / `.cursorrules` 等 AI 配置文件，并在 `requirements` 完善 / 设计阶段作为上下文注入。
- **Skills（技能）**：可视化管理 `.claude/agents/` 中的 skill 文件，支持内置市场镜像源。

### 📋 需求驱动的三角色向导（Wizard）
需求状态机：`draft → analyzing → designing → designed → developing → done`

| 角色 | 任务 | 工具 |
|------|------|------|
| **需求分析师 (analyst)** | 多轮 SSE 对话完善需求，读项目文件澄清歧义 | `DeepRefineChat` 组件 |
| **架构师 (architect)** | 在 `--permission-mode plan` 下只读探索代码库，将技术方案写入 `~/.claude/plans/`，由 Nova 捕获并落库 | `DocRefineChat` |
| **开发者 (developer)** | 自动检出 dev 分支，调用 `claude` 生成代码，JobStore 实时回显工具调用与文本 | `CodingChat` + Job SSE |

每个阶段都有"手动完成门"，避免 AI 自作主张推进状态。

### 🤖 Claude Agent 代码生成
- `GenerateCode` 启动 `claude --output-format stream-json --dangerously-skip-permissions` 子进程，逐行解析事件。
- 工具调用会被本地化为中文标签（读取 / 编辑 / Bash / Grep 等）并写入 Job 日志。
- 自动从需求关键词匹配源文件，构建约 40 KB 的项目上下文 prompt。
- 支持子代理模型固定（`ANTHROPIC_DEFAULT_*_MODEL`），防止 subagent 在自定义 base URL 上回退失败。

### 🐳 Run Session（docker compose 会话）
- Web UI 一键 `docker compose up` 项目，跟踪每个项目的活跃进程。
- 实时流式日志（stdout / stderr 自动分流）。
- `Stop` 发送 SIGINT，5 秒未退出则强制 KILL。
- JobStore ring buffer（容量 50）记录最近会话，重启后清空。

### 🔍 AI 辅助 PR 审查
- 抽象 `platform.Client`，原生支持 **GitHub / GitLab / Gitea**。
- 一键开启 review job：AI 在项目 worktree 内分析 PR diff，调用 `claude` 流式生成评论。
- 自动 `SubmitComment` 把审查结果回贴到 PR。

### 📊 项目扫描器（Scanner）
- 自动识别项目类型（Go / Node / Python / …），记录 `projects.claude_files`。
- 索引 `CLAUDE.md` / `AGENTS.md` / `.cursorrules` 等文档到 `knowledge` 表。
- 用 LLM 自动生成项目描述（可在 UI 锁定为手动维护）。

### 📈 周报 & 用量统计
- 从 `git log` + 需求数据生成项目周报，可自定义规则 / 预设模板。
- 按需求 / 按项目聚合 LLM token 用量，review 类任务单独统计。

### 🌳 Worktree 管理
- 项目内多 worktree 并行开发，每个 worktree 可独立跑 run session。
- 列出 Git 分支，状态实时同步。

### 🔐 权限与多用户
- 内置 ACL（用户 / 角色 / 权限 / 绑定），首次启动创建默认 admin 账户并打印随机密码。
- 路由级 `middleware.RequirePermission(aclSvc, "permission.key")` 守卫。
- 会话基于 cookie，支持登入 / 登出 / `me` 查询。

### ⚙️ 灵活的配置体系
- **多 Claude 配置**：`claude_configs` 表存储多个 (auth_token, base_url, model list)，运行时一键激活，立刻切换。
- **角色配置**：analyst / architect / developer / reviewer 各自独立的 system prompt + model，可在 UI 修改并 `reset` 回默认值。
- **数据库切换**：SQLite / MySQL / PostgreSQL 三选一，env 优先 / `dbconfig.json` 兜底；提供一次性迁移工具 `go run ./cmd/server -migrate`。

### 🛰️ Agent 服务器资源
- **远端执行资源**：把"本机"换成一台或多台 Linux / macOS 远端服务器，Claude CLI 任务在远端运行；需求代码仍由本地数据库统一管理（git worktree 需求隔离、`--resume` 多轮开发）。
- **凭据 AES-256-GCM 加密存储**：SSH Key / 密码明文经 AES-256-GCM 落盘，API 永不返回密文，master key 在 `~/.novaworkbench/secret.key` (0600)。
- **环境自动检查与安装**：Check 阶段探测 `claude` / `node` / `npm` / `git`，缺什么自动装什么（Linux 走 apt/yum/dnf/nvm，macOS 走 Homebrew）。
- **执行选择**：在「开始开发」/「追加调整」/「继续开发」下拉中切换「本地」/「Agent 服务器」，选项每次都生效。

### 🛠 Preflight & 自安装
- 启动时探测 `claude` / `node` / `npm` / `git` / `docker`。
- 缺啥自动装啥（apt / brew / winget），安装过程通过 JobStore + SSE 实时回显。
- `NOVA_AUTOINSTALL=0` 可关闭自安装，纯手动管理依赖。

### 📦 单二进制部署
- `make build` 一键产出 `dist/nova`，前端 SPA 经 `//go:embed all:dist` 嵌入二进制。
- 启动后访问 `http://localhost:9527/` 即进入完整 UI，无须单独部署前端。

---

## 技术架构

```
┌──────────────────────────────────────────────────────────┐
│  Browser  ──▶  React 19 + Vite 8 SPA  (react-router v7) │
└──────────────────────┬───────────────────────────────────┘
                       │  /api/*  (SSE / JSON envelope)
┌──────────────────────▼───────────────────────────────────┐
│  Go 1.25  net/http  +  database/sql  (3 drivers, no CGO) │
│  ─ handler/ → service/ → *sql.DB                         │
│        │                                                 │
│        ├─ llm.Gateway  ──exec──▶  local `claude` CLI     │
│        ├─ store.JobStore (in-memory ring buffer, 50)     │
│        └─ platform.Client  ─HTTP─▶  GitHub / GitLab /    │
│                                    Gitea                 │
└──────────────────────────────────────────────────────────┘
                       │
        ┌──────────────┼──────────────┐
        ▼              ▼              ▼
   SQLite (WAL)     MySQL      PostgreSQL
   ~/.novaworkbench/          via NOVA_DB_DSN
   data/nova.db
```

**关键设计决策**
- **SSE 全双工流**：所有 AI 长任务（wizard / review / runner / report）都走 `text/event-stream`，前端用 `ReadableStream` 手动解析。
- **JobStore 共享**：三类后台任务（Wizard coding / Runner / Review）共用一个 `store.NewJobStore(50)`，SSE 形状统一。
- **三角色 stage-gate**：analyst / architect / developer 各自独立会话（`--resume --fork-session`），状态由用户手动推进，不让 AI 自己宣布完成。

---

## 快速启动

### 前置依赖

- **Go 1.25+**（项目使用 Go 1.22+ 路由特性）
- **Node.js 20.19+ / 22.12+**（Vite 8 要求）
- **npm**
- **git**
- **Claude CLI**：`npm install -g @anthropic-ai/claude-code`，并设置 `ANTHROPIC_API_KEY`（或自定义 `ANTHROPIC_BASE_URL`）
- 可选：**Docker**（启用 Run Session 功能）

> 一键自检：`make doctor` 或 `scripts/check-build-deps.sh --with-frontend`
> 自动安装缺失依赖：`INSTALL=1 make build`

### 方式一：Docker Compose（推荐）

```bash
git clone https://github.com/novaworkbench/novaworkbench.git
cd novaworkbench

# 可选：在环境变量里设置 Claude 凭据
export ANTHROPIC_API_KEY=sk-ant-xxxxx

docker-compose up -d
```

打开 <http://localhost:9527/> 即可使用。数据持久化到宿主机 `~/.novaworkbench/`。

### 方式二：本地源码构建

```bash
git clone https://github.com/novaworkbench/novaworkbench.git
cd novaworkbench

# 一键构建：前端 prod build → 嵌入到 Go 二进制
make build

# 运行（前端已嵌入，单进程即可访问完整 UI）
./dist/nova
```

### 方式三：开发模式（前后端分离热重载）

终端 A — 后端：
```bash
make run
# 监听 :9527，Go 代码改动自动重新编译
```

终端 B — 前端：
```bash
cd frontend
npm install
npm run dev
# Vite dev server :5173，/api 反向代理到 :9527
```

打开 <http://localhost:5173/> 即可开发前端。

### 方式四：本地 + PostgreSQL 调试

```bash
./run.sh
# 自动启动 nova-postgres 容器，注入 NOVA_DB_DRIVER=postgres / DSN
# 然后 go run ./cmd/server
```

### 首次登录

首次启动时，后端日志会打印默认管理员账号：

```
[acl] default admin account created — username: admin  password: <随机密码>
```

用此账号登录后请立即在 **设置 → 用户管理** 修改密码。

---

## 使用指南

### 1. 添加项目

进入 **项目** 页面 → **添加项目**，选择本地目录（`/api/fs/ls` 提供文件夹浏览器）。

- 路径必须包含 `.git` 目录或可通过 `init_git` 初始化。
- 扫描器自动识别项目类型、索引 AI 配置文件、生成项目描述。

### 2. 创建需求并启动 Wizard

**需求** → 新建需求 → 进入 `WizardPage`：

1. **Analyst（多轮对话完善）**：Claude 会读取你的项目文件来澄清需求。
2. **Architect（生成技术方案）**：进入 plan 模式，Claude 在只读探索后产出 Markdown 方案，写入 `~/.claude/plans/` 并被 Nova 持久化。
3. **Refine / Apply Doc**：可继续多轮迭代技术方案或开发指令。
4. **Start Coding**：检出 dev 分支，Claude 开始生成代码，实时流式显示工具调用与文本。

### 3. 启动 Run Session

项目详情页 → **Run** → 选择 `docker-compose.yml`，Nova 会：

- 启动 `docker compose up` 并把日志写入 JobStore。
- 你可以在 UI 上看到 stdout / stderr 实时滚动。
- `Stop` 优雅退出，5s 后强 KILL。

### 4. PR 审查

项目详情页 → **PRs** → 选择平台（GitHub / GitLab / Gitea）→ **Start Review**：

- Nova 调用 `claude` 在 worktree 内分析 diff。
- 流式显示审查过程，完成后自动 `SubmitComment` 把结论贴回 PR。

### 5. 设置 Claude / 角色 / 数据库

`设置` 页面提供：

- **Claude 配置**：管理多个 (auth_token, base_url, model) 组合，一键激活。
- **角色配置**：analyst / architect / developer / reviewer 各自的 system prompt + model。
- **LLM 通道**：用于轻量任务（如需求标题提炼）的直接 HTTP LLM。
- **数据库**：切换 SQLite / MySQL / PostgreSQL，或从 SQLite 一次性迁移。
- **依赖 (Preflight)**：探测并安装 CLI 工具。
- **Skills**：管理 `.claude/agents/` 中的 skill 文件。
- **用户 / 角色 / 权限**：完整的 ACL 管理面板。

### 6. 配置 Agent 服务器

Agent 服务器是一台远程 Linux / macOS 主机，作为 Claude CLI 的远端执行资源。所有 Claude 任务仍在平台统一调度（流式输出与本地一致），但 `claude` 子进程跑在 Agent 上，代码改动通过 git 在本地与远端之间同步。

#### 6.1 添加一台 Agent 服务器

1. 进入 **设置 → Agent 服务器** 页（与"平台 Token"同级）。
2. 点击 **新建**，填写表单：
   - **名称**（必填）：便于在列表中识别，例如 `prod-mac-01`
   - **IP**（必填）：Agent 服务器的 hostname 或 IP
   - **端口**：默认 `22`
   - **用户名**：默认 `root`，按需改为 `ec2-user` / `ubuntu` 等
   - **认证方式**：SSH Key（PEM 明文）或密码
   - **凭据**：粘贴 SSH 私钥全文（含 `BEGIN/END` 行），或输入密码
3. 点击 **保存**。新服务器状态为 `unknown`。
5. 点击 **检查环境**：平台 SSH 到目标主机探测 `claude` / `node` / `npm` / `git`，结果以 SSE 流式回显在卡片内。检查通过后状态变为 `ready`。
6. 若环境不满足，点击 **安装依赖**：按平台自动安装（Linux: apt/yum/dnf + nvm，macOS: Homebrew + node），实时日志显示。

#### 6.2 凭据加密机制

- 凭据字段（`auth_value`）保存到数据库时，使用 **AES-256-GCM**（12 字节随机 nonce，密文 + auth tag 拼接后 base64）加密。
- **Master key** 首次启动时随机生成在 `~/.novaworkbench/secret.key`（文件权限 0600），由平台进程加载；丢失则旧凭据不可恢复，需重新配置服务器。
- API 响应中 `auth_value` 字段被 `json:"-"` 屏蔽，UI 只能看到 `auth_value_set: true/false`。
- 可通过环境变量 `NOVA_SECRET_KEY_PATH` 覆盖 master key 路径（测试场景）。

> macOS 前置提示：若 Agent 服务器是 macOS 且未安装 Homebrew，安装脚本会引导执行 `curl Homebrew install.sh`（5 分钟超时）。若网络受限请先在目标 Mac 上手动安装 Homebrew。

#### 6.3 在需求开发时选择 Agent 服务器执行

进入需求详情 → 推进到 **开始开发** 阶段：

1. **选择执行环境**下拉位于「Model 选择」旁：
   - **本地执行**（默认）：Claude 在本机跑，与改动前行为完全一致
   - **Agent 服务器列表**：仅显示 `status === 'ready'` 的服务器（按 `name (host)` 展示）
2. 选择目标服务器后，点击 **开始开发**。Job 日志将依次出现：
   - `📥 准备 Agent 服务器工作目录（git worktree 隔离）...`
   - `📤 同步 Claude 会话历史（SFTP 上行）...`
   - `🤖 Agent 服务器开始执行...`
   - 工具调用 / 文本流（与本地一致）
   - `📥 同步会话结果回本地...`
   - `📤 推送代码变更到 origin...`
3. 每次执行都生成远端独立 worktree：`/tmp/nova-agent/<projectID>/<reqID>`，与本地的 `~/.novaworkbench/worktrees/<basename>/<reqID>` 一一对应，**多个需求互不干扰**。
4. **多轮开发（`adjust-coding` / `continue-coding`）**：`--resume <session_id>` 在远端同样生效——本地 `~/.claude/projects/<slug>/` 会话文件上行同步到远端对应目录，claude CLI 在远端读取 session jsonl；执行后下行同步回本地，形成闭环。

#### 6.4 常见故障

| 现象 | 原因 | 解决 |
|------|------|------|
| `SSH 连接失败: ...` | 22 端口未开放 / 防火墙 / 私钥格式错误 | 在 Agent 服务器本地测试 `ssh -i <key> user@host`；确认私钥为 PEM (RSA/ED25519/OpenSSH) 格式 |
| `项目未配置 git 远程仓库，无法在 Agent 服务器执行` | 项目没有 origin URL | 在 **项目详情** 中配置 Platform Token 与 remote_url，或在本地为项目添加 `git remote add origin ...` |
| `git push 失败（exit=...），请在远程 worktree 手动处理冲突` | 远端已有同名分支且分叉 | 在 Agent 服务器上 `cd /tmp/nova-agent/<projectID>/<reqID> && git pull origin <branch>` 解决冲突后再试 |
| `❌ 不支持的平台: Windows...` | Agent 服务器 OS 不是 Linux / macOS | 切换到 Linux/macOS 主机；Windows 暂不支持（无 Bash + sshd 标准环境） |
| macOS: `curl Homebrew install.sh` 失败 | 网络受限 | 手动在目标 Mac 安装 Homebrew，再重试 **安装依赖** |
| `环境检查` 后状态仍为 `error` | claude 旧版本缺失 `output-format stream-json` | 重新点击 **安装依赖**；若失败则手动在目标主机执行 `npm install -g @anthropic-ai/claude-code` |
| `secret.Init 失败 / master key has invalid length` | `~/.novaworkbench/secret.key` 损坏或被替换 | 删除该文件后重启后端（会重新生成）；注意旧凭据会失效，需重新配置服务器 |
| 切换到「Agent 服务器」执行后本地的需求代码没变化 | 远端 commit 未 push 或 push 失败 | 检查 Job 日志最后是否有 `推送失败`；必要时在 Agent 服务器上手动 `git push origin <branch>`，再在本地 `git pull` |

#### 6.5 安全提示

- **不要**把生产环境的 Agent 服务器与开发 Agent 混用同一份 secret key；建议每个部署用独立 master key。
- 在多人协作环境，把 `~/.novaworkbench/secret.key` 加入备份策略（与数据库一同备份）。
- API 仅返回 `auth_value_set` 布尔值与加密算法标识，不返回凭据任何片段；前端无法展示或导出凭据明文。

---

## 配置说明

### 环境变量

| 变量 | 默认 | 说明 |
|------|------|------|
| `NOVA_PORT` | `9527` | 后端监听端口 |
| `CLAUDE_BIN` | `claude` | `claude` CLI 可执行文件路径 |
| `CLAUDE_TIMEOUT` | `120s` | 普通 LLM 调用超时（编程任务硬性下限 30 分钟）|
| `NOVA_DB_DRIVER` | `sqlite` | `sqlite` / `mysql` / `postgres` |
| `NOVA_DB_DSN` | _空_ | 数据库连接字符串 |
| `NOVA_DB_PATH` | `~/.novaworkbench/data/nova.db` | SQLite 文件路径 |
| `NOVA_AUTOINSTALL` | `1` | 启动时是否自动安装缺失依赖（`0` 关闭）|
| `NOVA_SKIP_FRONTEND` | _空_ | 与 `make build-backend` 配合，跳过前端嵌入步骤 |
| `ANTHROPIC_API_KEY` | _空_ | Claude API 凭据（也可在 UI 中配置）|
| `ANTHROPIC_BASE_URL` | _空_ | 自定义 API base URL |
| `VITE_API_BASE` | `http://localhost:9527` | 前端指向的后端地址（仅前端构建时）|

### 数据存储位置

```
~/.novaworkbench/
├── data/nova.db              # SQLite 默认数据库（WAL）
├── dbconfig.json             # 数据库配置（env 优先）
├── claude/                   # Claude CLI 会话 / plans 持久化
└── ...

$HOME/workspace/              # 通过 docker-compose 挂载的项目根
```

---

## 开发

### 目录结构

```
.
├── backend/
│   ├── cmd/server/main.go      # 路由装配 / 服务初始化
│   ├── internal/
│   │   ├── handler/            # HTTP handlers（每个资源一个 struct）
│   │   ├── service/            # 业务逻辑 + 原始 SQL
│   │   ├── model/              # 纯结构体（无方法）
│   │   ├── db/                 # 驱动加载 + schema + 迁移
│   │   ├── llm/                # Claude CLI Gateway（stream-json 解析）
│   │   ├── platform/           # GitHub/GitLab/Gitea 抽象
│   │   ├── preflight/          # 依赖探测 & 自安装
│   │   ├── store/              # JobStore（in-memory ring buffer）
│   │   ├── middleware/         # Logger / CORS / 权限守卫
│   │   └── util/               # id.go 等通用工具
│   └── web/dist/               # 前端构建产物（嵌入用）
├── frontend/
│   ├── src/
│   │   ├── api/                # 单一 request() 包装器 + 资源 API
│   │   ├── components/         # Layout / 三个 Chat / FolderPicker…
│   │   ├── pages/              # Dashboard / Wizard / 需求详情…
│   │   └── utils/              # auth 等
│   └── public/
├── deploy/                     # 生产 / 预览 compose 栈
├── terraform/                  # 生产基础设施
├── scripts/                    # check-build-deps.sh 等
└── CLAUDE.md                   # 给 Claude Code 的项目指南
```

### 添加新表 / 新列

`backend/internal/db/schema.go` 中追加 `ALTER TABLE` 到 `alterColumns` 段，幂等运行。

### 添加新 HTTP 路由

使用 Go 1.22+ 路由模式：

```go
mux.HandleFunc("GET /api/projects/{id}", projectH.Get)
mux.HandleFunc("POST /api/projects/{id}/scan", scannerH.Scan)
```

读路径参数：`r.PathValue("id")`。

### Lint

```bash
# 后端
cd backend && go vet ./...

# 前端
cd frontend && npm run lint   # oxlint
```

### 数据迁移（SQLite → MySQL / PostgreSQL）

```bash
# 方式 A：CLI 一次性迁移
go run ./cmd/server -migrate -from ~/.novaworkbench/data/nova.db

# 方式 B：在设置 → 数据库页一键迁移（需重启）
```

---

## 部署

### 生产（nginx-proxy）

```bash
cd deploy
docker-compose -f docker-compose.prod.yml up -d
```

`deploy/docker-compose.prod.yml` + `nginx-proxy` 提供 TLS / 主机名路由。

### 预览环境

```bash
docker-compose -f docker-compose.preview.yml up -d
```

### Terraform

`terraform/` 目录包含生产基础设施定义，可使用：

```bash
cd terraform
terraform init
terraform apply
```

CI 工作流：`.github/workflows/deploy.yml`（镜像构建 + 部署）与 `terraform.yml`（plan / apply）。

---

## 常见问题

<details>
<summary><b>启动报 "未找到 claude CLI"</b></summary>

后端会打印手动安装命令：`npm install -g @anthropic-ai/claude-code`，或在前端 **设置 → 依赖** 触发自动安装。
</details>

<details>
<summary><b>subagent 用了别的模型导致自定义 base URL 报错</b></summary>

Nova 会在 spawn 子进程时根据 `--model` 同时固定 `ANTHROPIC_DEFAULT_HAIKU_MODEL` / `SONNET_MODEL` / `OPUS_MODEL`，确保子代理也走同一个 base URL。
</details>

<details>
<summary><b>想切换数据库</b></summary>

设置 → 数据库 → 选择 MySQL / PostgreSQL → 测试连接 → 保存 → 重启后端。一次性迁移可走 **Migrate** 按钮或 `-migrate` CLI flag。
</details>

<details>
<summary><b>PR 平台是 Gitea</b></summary>

Gitea 必须填 `base_url`，在 **设置 → 平台 Token** 中配置。
</details>

<details>
<summary><b>JobStore 重启后清空是 bug 吗？</b></summary>

不是设计如此 — JobStore 是 50 容量的内存环形缓冲，仅用于实时 SSE 回放。完整数据已落库（`requirements` / `reports` / `usage` 等）。
</details>

<details>
<summary><b>如何自定义 analyst / architect 的 system prompt？</b></summary>

设置 → 角色 → 选择对应角色 → 编辑 system prompt + model → 保存。`Reset` 可恢复内置默认值。
</details>

---

## 贡献

欢迎任何形式的贡献！建议流程：

1. Fork 本仓库。
2. 创建分支：`git checkout -b feat/your-feature`
3. 提交并写清楚 commit message。
4. 发起 PR，描述动机 / 实现方式 / 测试情况。
5. 在 PR 中 @ 维护者。

开发前可以读 `CLAUDE.md` 了解项目约定与架构概览。

---

## 许可证

本项目使用 [MIT License](LICENSE)。

---

## 致谢

- 后端基于 [Go](https://go.dev) 标准库 + 三个纯 Go 数据库驱动构建。
- 前端使用 [React](https://react.dev) / [Vite](https://vitejs.dev) / [react-router](https://reactrouter.com)。
- 所有 AI 能力由本地 [Claude Code CLI](https://www.npmjs.com/package/@anthropic-ai/claude-code) 提供。

如果你觉得这个项目有帮助，欢迎 Star ⭐！
