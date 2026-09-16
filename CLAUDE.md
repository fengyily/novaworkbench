# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> Last updated for v0.4.0 (2026-09-14). See `CHANGELOG.md` / `REQUIREMENTS.md` for release notes.

## Project Overview

NovaWorkbench is a local-first, AI-native developer workbench managing multiple local projects through a Web UI and unifying: AI context (memories / knowledge bases / **Skills**), requirement tracking with AI-driven refinement/analysis/design, Claude Agent-driven code generation with **sub-task orchestration** + **auto commit-push-PR**, `docker compose` run-session management, AI-assisted PR review against GitHub/GitLab/Gitea (with **GPG signing**), **ACL/RBAC** user management, **Agent Server** remote execution (SSH + Node bridge), **weekly reports**, **scheduled tasks**, calendar view, and a token-usage dashboard. Backend shells out to **`claude` CLI** (`@anthropic-ai/claude-code`) for full-feature AI tasks(wizard/review/codegen/design) and a direct HTTP OpenAI-compatible channel for lightweight calls(title/description);`claude -p ... --output-format stream-json --dangerously-skip-permissions` as subprocess with project dir as CWD.

## Tech Stack

- **Backend**: Go 1.25, stdlib `net/http` + `database/sql` with three pure-Go drivers (no CGO): SQLite via `modernc.org/sqlite`, MySQL via `github.com/go-sql-driver/mysql`, PostgreSQL via `github.com/jackc/pgx/v5/stdlib`. **SSH/SFTP**: `golang.org/x/crypto` + `github.com/pkg/sftp` (Agent Server remote channel). Go 1.22+ router pattern (`METHOD /path/{id}`). Session auth is **32-byte random hex** derived via `util.NewID` (NOT JWT — no `golang-jwt` dep); SSE uses stdlib `http.Flusher` (NOT WebSocket).
- **Frontend**: React 19 + TypeScript 6 + Vite 8 + React Router v7. **i18n**: `i18next ^26` + `react-i18next ^17` (`frontend/src/i18n/`, 18 zh-CN/en-US module pairs, LanguageSwitcher in Layout/Login). Lint: `oxlint`.
- **Storage**: SQLite by default (WAL mode, single writer `MaxOpenConns=1`) at `~/.novaworkbench/data/nova.db`; optionally MySQL/PostgreSQL via `NOVA_DB_DRIVER`/`NOVA_DB_DSN` or the 设置→数据库 page (saved to `~/.novaworkbench/dbconfig.json`; env wins; restart required to switch). One-shot data copy: `go run ./cmd/server -migrate [-from <sqlite path>]`. **AES-256-GCM master key** at `~/.novaworkbench/secret.key` (0600, override by `NOVA_SECRET_KEY_PATH`).
- **Dev**: Docker Compose (backend `:9527`, frontend `:5173`).

## Commands

The canonical build is **single-binary**: the frontend SPA is built into `frontend/dist`, copied to `backend/web/dist/`, then embedded into the Go binary via `//go:embed all:dist` (see `backend/web/embed.go`). The resulting Go binary serves the SPA at `/` with a react-router fallback.

```bash
make build                       # frontend build -> copy -> CGO_ENABLED=0 go build -> dist/nova
make build-frontend              # npm ci + npm run build -> backend/web/dist
make build-backend               # backend only (pair with NOVA_SKIP_FRONTEND=1)
make run                         # dev backend only (frontend HMR runs separately)
make doctor                      # scripts/check-build-deps.sh --with-frontend
make clean                       # remove embedded dist, built binary, .deps-checked sentinel
INSTALL=1 make build             # auto-install missing toolchain via scripts/check-build-deps.sh
SKIP_DEPS_CHECK=1 make build     # bypass the preflight (CI cache)

# Backend (from backend/)
go run ./cmd/server              # dev server on :9527
go run ./cmd/server -migrate -from <src.db>     # SQLite -> MySQL/Postgres one-shot copy
go run ./cmd/server -port N                    # override NOVA_PORT
go vet ./...                     # lint

# Nova CLI subcommands (intercepted by main.go before flag.Parse; install/uninstall Linux-only)
nova install                     # writes /etc/systemd/system/nova.service + daemon-reload + enable --now
nova uninstall                   # removes unit (data dir preserved)
nova version                     # prints Version/Commit/BuildDate (from ldflags)

# Frontend (from frontend/)
npm run dev                      # Vite :5173, proxies /api -> :9527
npm run build                    # tsc -b && vite build
npm run lint                     # oxlint
npm run preview                  # serve the production build

# Docker Compose (repo root)
docker-compose up                # backend (uid 1000) mounts ~/.novaworkbench(+ claude subdir) and $HOME/workspace

# Local debug launcher with PostgreSQL
./run.sh                         # starts nova-postgres container, exports NOVA_DB_DRIVER=postgres
```

Env vars the backend reads: `NOVA_PORT` (default `9527`), `CLAUDE_BIN` (default `claude`), `CLAUDE_TIMEOUT` (default `120s`; coding jobs floor to `30m`), `NOVA_DB_DRIVER` (`sqlite` default | `mysql` | `postgres`), `NOVA_DB_DSN`, `NOVA_DB_PATH` (sqlite file), `NOVA_AUTOINSTALL` (default `1`; `0` disables preflight auto-install), `NOVA_SKIP_FRONTEND` (pair with `make build-backend`), `NOVA_SECRET_KEY_PATH` (AES master key override), `NOVA_SCHED_INTERVAL` / `NOVA_SCHED_CONCURRENCY` / `NOVA_SCHED_MAX_LATENESS` (scheduler tick tuning). The frontend reads `VITE_API_BASE` (default `http://localhost:9527`).

## Architecture

`cmd/server/main.go` wires everything; ~140 routes registered on a single `http.ServeMux` (Go 1.22 method+path pattern). Dependency flow:

```
handler/  ->  service/  ->  *db.DB (model/ holds structs, no behavior)
                |
        llm.Gateway (claude CLI subprocess + direct HTTP LLM)
        store.JobStore (in-memory ring buffer of background jobs, cap 50)
        platform.Client (github/gitlab/gitea HTTP)
        scheduler.Scheduler (scheduled_tasks + orchestration_batches ticks)
```

### Layering (`backend/internal/`)

18 sub-packages: **业务层** `handler/` (~46 files, one struct per resource; `response.go` envelope + SSE helper) / `service/` (~22 files, 业务逻辑 + 原始 SQL, no repository abstraction) / `model/` (~17 files, 纯 struct + JSON tags) / `util/` (`NewID(prefix)` `<prefix>_<8 hex chars>` + claude session slug 编解码) / `middleware/` (`Logger` + `CORS` + `Auth` + `RequirePermission(aclSvc, key)`); **基础设施** `db/` (三方言连接 + `*db.DB` 包装,`Ident`/`OnConflict`/`$N` 占位符) / `llm/` (Claude CLI 网关 `Gateway` + 直接 HTTP LLM 通道 `httpchat.go` + Skills 注入 `inject_skills.go`) / `platform/` (`Client` 接口 + GitHub/GitLab/Gitea + GPG 签名 + commit-language 检测) / `preflight/` (运行时依赖探测/安装) / `prompt/` (按 `requirement.kind` 切分的角色无关 prompt 片段) / `secret/` (AES-256-GCM 主密钥管理,master key 在 `~/.novaworkbench/secret.key` 0600) / `scheduler/` (两独立 tick loop:`scheduled_tasks` 30s + `orchestration_batches` 10s) / `ssh/` (SSH/SFTP 客户端 + agent-worker HTTP transport) / `store/` (`JobStore` ring buffer cap 50) / `version/` (ldflags 注入 `Version`/`Commit`/`BuildDate`) / `servicemgr/` (`nova install/uninstall` 写 systemd unit,Linux `//go:build linux` + 其他平台 stub). `main.go` 共享 `store.NewJobStore(50)` 给 8 个 handler:`WizardHandler` / `RunnerHandler` / `ReviewHandler` / `SubTaskRunner` / `ReportHandler` / `ScheduleExecutor` / `AgentServerHandler` / `PreflightHandler`.

### Database layer & migrations (`internal/db/`)

`db.Init(cfg)` opens the driver selected by `LoadConfig()` (env > `dbconfig.json` > sqlite default) and runs `migrate()`. SQLite keeps WAL + `MaxOpenConns=1`; MySQL/Postgres get a small pool. The schema lives in one canonical SQLite-flavored DDL block (`schema.go`); `fixupSchema` translates it per dialect (Postgres: `DATETIME`→`TIMESTAMP`; MySQL: indexed `TEXT`→`VARCHAR`, backticked `` `key` ``, expression defaults `DEFAULT ('…')` on TEXT). Statements run one by one; idempotency relies on per-dialect "duplicate column"/"duplicate key name" error matching.

Services hold `*db.DB` (not `*sql.DB`) — a thin wrapper that rebinds `?`→`$N` on PostgreSQL (`Exec`/`Query`/`QueryRow`/`Begin`), quotes reserved identifiers via `Ident` (the `key` column is reserved in MySQL), and builds upsert suffixes via `OnConflict` (`ON CONFLICT … DO UPDATE` vs `ON DUPLICATE KEY UPDATE`). **Conventions for new SQL**: write `?` placeholders, go through the wrapper, use `Ident`/`OnConflict` where relevant. New columns still follow the ad-hoc pattern: append an `ALTER TABLE` to `alterColumns` in `schema.go` — no separate migration files.

`db.Migrate(src, dst)` copies all **27 tables** parent-first, preserving IDs and skipping duplicate PKs; it backs both the `-migrate` CLI flag and `POST /api/settings/database/migrate` (settings UI).

**27 tables** (`schema.go:14,31,46,74,89,109,116,123,137,147,160,166,178,191,202,225,243,255,265,273,281,291,300,317,344,382,407`):

| 表 | ID 前缀 | 一句话 |
|---|---|---|
| `projects` / `memories` / `knowledge` | `proj_` / `mem_` / `kb_` | 项目元数据 / 跨项目 AI 记忆 / 文档扫描条目(upsert by source_ref/source_type) |
| `requirements` / `conversations` / `refinement_chats` / `coding_chats` | `req_` / (uuid) / (PK=req_id) | 需求+kind/status/design_docs/calendar / Claude 会话 / 分析师会话 / 开发者会话 |
| `project_run_configs` / `platform_tokens` / `roles` / `acl_roles` / `permissions` | (PK=proj_id) / `tok_` / 固定 / `arole_` / 固定 | docker-compose run 配置 / GitHub/GitLab/Gitea Token / **AI persona**(注 system prompt) / **RBAC 角色**(`RequirePermission` 用) / 权限枚举 |
| `settings` / `claude_configs` / `weekly_reports` / `job_logs` / `token_usage` | (PK=key) / `ccfg_` / - / (PK=job_id) / `tu_` | KV 设置 / 多 Claude 配置 / 项目周报 / Job 持久化日志 / LLM token 用量 |
| `users` / `acl_role_permissions` / `acl_user_roles` / `user_projects` / `sessions` | `usr_` / 复合 PK / 复合 PK / 复合 PK / 32-byte hex | ACL 用户(含 `locale`) / 角色-权限 / 用户-角色 / 用户-项目授权 / **session token**(非 JWT) |
| `skills` / `agent_servers` / `sub_tasks` / `orchestration_batches` / `scheduled_tasks` | `skill_` / `agent_` / `st_` / `ob_` / `sched_` | AI Skills(`.claude/agents/`) / 远端 Agent 主机(凭据 AES-256-GCM 加密) / 需求子任务 / 子任务批派发 / 定时任务(design/coding/design_and_coding) |

> **`roles` vs `acl_roles`**: 两个"角色"语义独立。`roles` = AI persona(由 wizard/review 注入 system prompt);`acl_roles` = RBAC 角色(由 `middleware/auth.go` `RequirePermission` 消费)。

### Authentication & ACL

ACL 落地在 7 张表(`users`/`sessions`/`acl_roles`/`permissions`/`acl_role_permissions`/`acl_user_roles`/`user_projects`);`handler/acl.go` 注册 `/api/acl/{users,roles,permissions,...}`。`middleware/auth.go:78` 的 `RequirePermission(aclSvc, "perm.key")` 守卫路由级权限;未登录返回 401。前端 `utils/auth.tsx` 提供 `AuthProvider` + `useAuth`,401 时跳 `/login`。**Session token 是 32-byte random hex(非 JWT)**;首次启动 seed admin 账户并把随机密码打到日志。`users.locale` 列支持每用户语言偏好(`PUT /api/auth/locale`)。

### Agent Servers (`agent_servers` + `secret` + `ssh` + `agent-worker/`)

远端执行资源: 把"本机"换成一/多台 Linux/macOS 远端服务器,Claude CLI 任务在远端运行,需求数据由本地数据库统一管理(经 git worktree 同步 + `--resume` 多轮开发)。链路:`llm.Gateway.BuildRemoteEnvPairs*` 构造 env → `ssh/client.go` SSH → 目标机 `agent-worker/server.mjs`(Node 18+)listen `127.0.0.1:7000` → 调本机 `claude` CLI。**凭据**(SSH Key / 密码)**用 AES-256-GCM 加密**存 `agent_servers.auth_value`,master key 在 `~/.novaworkbench/secret.key`(0600);API 响应 `auth_value` 字段 `json:"-"` 屏蔽,前端只看到 `auth_value_set: true/false`。`servicemgr_linux.go` 实现 `nova install`(写 `/etc/systemd/system/nova.service` + `daemon-reload` + `enable --now`);UI 在 `SettingsAgentServers.tsx`(`/settings/agent-servers`),含 check / install 实时 SSE 流。

### Sub-tasks & Orchestration

需求在 coding 阶段可被**自动拆分为子任务**(`handler/wizard_subtask.go` + `wizard_orchestration.go`),由 `scheduler/orchestration_queue.go`(10s tick)派发,经 `handler/sub_task_runner.go` 执行;UI 在 `components/SubTaskPanel.tsx`。`scheduled_tasks` 表 + `handler/schedule.go` 支持 `design`/`coding`/`design_and_coding` 三种一次性定时任务(`scheduler/scheduler.go` 30s tick, 并发 2);UI 在 `SchedulesPage.tsx`(`/schedules`)。

**执行准入(两条路径共享)**: `service/project_limiter.go` 的 `ProjectLimiter` 是**按 `project_id` 的内存闸门**——编排 tick 与 `SubTaskRunner.Run`(含手动 重做/继续)都先过项目闸门、再过**进程级 OOM 上限**(env `NOVA_SUBTASK_CONCURRENCY`,两条路径共享同一 chan),顺序固定以免互锁。闸门上限来自 KV 设置 `subtask.concurrency`(默认 1,**按项目独立生效**),tick/Run 每次入口 `SetMax` 刷新 → 改设置 ≤10s 生效无需重启。tick 满槽时**不认领 DB 行**(行保持 pending,卡片显示「排队中」);手动路径则轮询等待。**失败自动重做**: `subtask.auto_retry`(默认 **false**,即默认手动恢复)+ `subtask.retry_max`,开启后 tick 在「无 pending 可认领」时经 `SubTaskService.ReArmErroredForRetry` 把 `error` 子任务翻回 `pending`(`sub_tasks.retry_count` 计数封顶,镜像 `summary_attempts` 模式),仅作用于编排子任务。三项配置在 `GET/PUT /api/settings/subtask` + `SettingsSubTask.tsx`(`/settings/subtask`,复用 `setting.llm` 权限键)。

### Merge & Weekly Reports

`handler/merge.go` + `merge_agent.go` 在 coding 完成后**自动 commit-push-PR**(支持本地 + Agent Server 远端),UI 在 `RequirementDetail` 的 SubTaskPanel 内嵌入口。`weekly_reports` 表 + `handler/report.go` + `service/report.go` 出项目周报(`git log` + 需求聚合);UI 在 `ProjectWeeklyReport.tsx`(`/projects/:id/reports`)。`token_usage` 表 + `service/usage.go` 聚合按需求/项目/Job 的 LLM token 用量(`/api/usage/*`)。

### Role config (`roles` 表 + `acl_roles` 表 — 两个"角色"含义不同)

- **`roles` 表(AI persona)** — wizard 的 analyst/architect/developer + review 的 reviewer + extensible,每个有 user-editable **system prompt** 和 **model**,seeded from `service.DefaultRoles()`(`RoleService.SeedDefaults()` called from `main.go`; per-key seed so new roles are backfilled into existing DBs). Managed via `GET/PUT /api/settings/roles[/{id}]` and `POST .../{id}/reset`. Review handler 加载 reviewer role 走 `llm.StreamCmd`,`--system-prompt`/`--model` 与配置的 Claude env 都生效。
- **`acl_roles` 表(RBAC)** — 由 `middleware/auth.go` 的 `RequirePermission` 消费,管用户/角色/权限/项目绑定。**与 `roles` 表语义独立**,不要混淆。

`Gateway.streamArgs` 驱动三个 CLI flag:`--system-prompt <prompt>`(非 plan 模式,全量替换)/ `--append-system-prompt <prompt>`(plan 模式,保留 CLI 指令)/ `--model <id>`(非空时);`PermissionMode`("plan" 或空) 选 `--permission-mode plan` vs `--dangerously-skip-permissions`。`WizardHandler.roleConfig(key)` 加载 active role(miss 时返回空串,broken config 不阻塞 pipeline);`RefineDoc`/`ApplyDoc` 跨角色,目前传空 system prompt/model(CLI 默认)。

### LLM Gateway (`internal/llm/`)

`Gateway` wraps the local `claude` CLI. **Public methods** (handlers consume via these — internal `runClaude*` methods are NOT the entry point anymore): `StreamCmd(ctx, opts) *exec.Cmd` 流式启动 claude 子进程(handler 拥有生命周期) | `GenerateCode(opts) (*exec.Cmd, ctx.CancelFunc)` 一次性代码生成 | `GenerateDescriptionAndTitle(content, kind)` 描述+标题生成 | `SummarizeIdeaToRequirement(content)` idea → requirement 提炼 | `ExtractSubtasksJSON(mainReply, feedback)` 子任务 JSON 抽取 | `GenerateProjectSummary(projectPath, claudeMD)` 项目摘要 | `BuildStreamArgs(opts) []string` 构造 `--system-prompt`/`--model`/`--permission-mode` | `BuildRemoteEnvPairs*(model, extras...)` 远端 Agent Server env 注入 | `GetBinPath() string` 暴露 `claude` 二进制路径。

`Gateway.mergedEnv` 注入 `ANTHROPIC_AUTH_TOKEN`/`ANTHROPIC_BASE_URL`(从 `claude_configs`);传 `--model` 时同步 pin `ANTHROPIC_DEFAULT_{HAIKU,SONNET,OPUS}_MODEL`(防 subagent 在自定义 base URL 上回退失败,如 DeepSeek 400 not found)。

**Direct HTTP LLM channel** (`llm/httpchat.go` + `LLMConfigProvider`): lightweight tasks (title formatting, description regeneration) — OpenAI-compatible `/v1/chat/completions`. Configured via `GET/PUT /api/settings/llm`. **Skills injection** (`llm/inject_skills.go` `BuildSkillsBlock`): active `skills` rows → `--system-prompt` before each spawn. UI in `SettingsSkills.tsx`. **Remote execution**: see "Agent Servers" subsection above.

### SSE + JobStore streaming pattern

Two distinct ways AI/long-running output reaches the browser, both under `text/event-stream`:

**A. Direct SSE** — handler starts `claude` (or another subprocess), parses `stream-json` events itself, and writes `data: {...}\n\n` frames with `http.ResponseController.Flush()` after each. Used by `wizard.DeepRefine`, `wizard.AnalyzeRequirement`, `wizard.GenerateDesignSSE`, `wizard.RefineDoc`, `wizard.ApplyDoc`, `review.StreamReviewJob`. The shared event protocol:

| `type`        | meaning                                  |
|---------------|------------------------------------------|
| `phase`       | human-readable status line ("🤖 ...")     |
| `tool_call`   | Claude is calling a tool (labeled by `toolCallLabel`) |
| `message`     | a line of assistant text                 |
| `tool_result` | truncated output of a tool call          |
| `error`       | failure                                  |
| `done` / `job_done` | terminal; carries final `history`/`result`/`status`/`exit_code` |

`toolCallLabel`(`wizard.go`) 映射 Claude 工具名到中文标签(Read/Bash/Glob/Grep/Write/Edit);`extractJSON` brace-match 抽 JSON;`isLikelyJSON` 区分 plan-Markdown vs legacy JSON;`runClaudeStream` 捕获 `out.planContent`(plan-mode `Write` tool_use 写到 `~/.claude/plans/*.md` 的全文,给 `ArchitectDesign`);`scanner.Buffer` 长输出场景提到 `256K→4M`(新增 streaming handler 保留)。**B. JobStore + SSE** — `store.JobStore` 内存 ring buffer cap 50;`Job.Append` 广播到订阅 channel,`Job.Subscribe` 先重放历史再持续推送直到 `Finish`;`POST .../start` 创建 job 立即返回 ID,`GET .../jobs/{id}/stream` 订阅 SSE(replay + live + `job_done` 终止帧);**用户**:`wizard.StartCoding`(codegen)、`runner.Start`(docker compose up)、`review.StartReview`(PR review)、`sub_task_runner`(sub-task 派发)、`report.Generate`(weekly report)、`schedule_executor`(定时任务)、`agent_server.Check/Install`(远端环境)、`preflight.Install`(CLI 工具)。Jobs 内存 only,重启清空;`RunnerHandler.RunSession` 按 project_id 跟踪 live `docker compose` 进程,`Stop` SIGINT + 5s force-kill。

### Wizard pipeline (requirement → code)

The full flow lives in `handler/wizard*.go` (`wizard.go` + `wizard_analyst.go` + `wizard_architect.go` + `wizard_coding.go` + `wizard_subtask.go` + `wizard_orchestration.go` + `wizard_remote.go` + `wizard_stream.go` + `wizard_docs.go` + `wizard_common.go`) and the `RequirementDetail`/`DeepRefineChat`/`DocRefineChat`/`CodingChat` frontend components. A requirement moves through **three role-gated stages**, each completed by a manual user action (no AI self-declaration):

1. **`analyst-chat`** — multi-turn SSE conversation. The requirement analyst reads project files (via tools) and refines the requirement. Conversation history threaded through each request, returned in the `done` event. Completion is **manual** (the prompt no longer asks Claude to emit `[ANALYSIS_COMPLETE]`).
2. **`architect-design`** — runs Claude in **plan mode** (`--permission-mode plan`), restricting Claude to read-only tools + the plan-file Write. Claude explores the codebase, writes a technical implementation plan (Markdown) to `~/.claude/plans/<slug>.md`, and the handler captures the full plan content from the `Write` tool_use event in the stream (`runClaudeStream` → `out.planContent`). The plan Markdown is persisted to `requirements.design_docs` via `reqSvc.UpdateDesign`, status → `designing`. In plan mode the role persona is passed via `--append-system-prompt` (not `--system-prompt`) so the CLI's plan-mode instructions survive. Legacy JSON-format designs (from older runs) are still rendered field-by-field; `parseDesign` on the frontend detects plan Markdown by trying `JSON.parse` and falling back to `{ plan_markdown: raw }`. The architect stage forks off the analyst session (`--resume <analysis_sid> --fork-session`) so it inherits the full analysis conversation.
3. **`refine-doc` / `apply-doc`** — iterative refinement of a stored doc. `doc_type` is `"design"` (updates `design_docs`) or `"coding"` (returns a plain-text dev instruction, no DB write). `refine-doc` ends when Claude emits `[REFINE_COMPLETE]`. For `design` docs, `apply-doc` detects whether the stored doc is plan Markdown (`!isLikelyJSON`) and asks Claude to output updated Markdown (not the legacy JSON schema); the raw Markdown is persisted directly (no `extractJSON`).
4. **`start-coding`** — creates a JobStore job, checks out the dev branch, and runs `llm.GenerateCode` in a goroutine, streaming `tool_call`/`message`/`tool_result`/`done` events into the job. On `job_done` the frontend sets status `developing`; the user manually marks `done`.

Beyond the linear stages: **Sub-task 自动编排** — coding 阶段后,Claude 可将剩余工作拆分为 `sub_tasks` 行,`wizard_orchestration.go` 创建 `orchestration_batches`,`scheduler/orchestration_queue.go` 10s tick 派发,`sub_task_runner.go` 独立执行(UI: `SubTaskPanel.tsx`)。**Auto commit-push-PR** — 全部子任务完成后,`handler/merge.go` 触发 `git commit` + `git push` + 自动创建 PR(走 `platform.Client.CreatePR`)。**执行环境 (ExecEnv)** — design / coding / sub-task 三阶段都暴露 `local / agent-server` 二选一(`ExecEnvSelect.tsx`),后端经 `BuildRemoteEnvPairs*` 决定本地 spawn 或 SSH 远端执行。

The gates map to status transitions: `draft → analyzing` (enter analyst chat) → `designing` (architect-design) → `designed` (manual 方案完成) → `developing` (start-coding) → `done` (manual 开发完成,触发 auto commit-push-PR).

### Preflight (`internal/preflight/`)

`preflight.New(CLAUDE_BIN)` builds a registry of runtime deps (`claude`, `node`, `npm`, `git`, `docker`) — `claude`/`node`/`npm` are required, `git`/`docker` are optional (docker enables the runner's compose sessions). `Registry.CheckAll` is network-free (LookPath + `--version`, 5s timeout); runs at startup, never halts the server. Surfaced at `GET /api/preflight` and folded into `/api/health` (`ready` flag = claude installed).

`EnsureAll` / `Install` perform best-effort platform installs — **macOS: Homebrew**; **Debian/Ubuntu: apt + nvm**; **Windows: choco/winget (best-effort)** — and stream per-line progress into a `*store.Job` via the shared JobStore+SSE pattern, so the frontend renders a live "正在安装 Claude CLI..." panel. `NOVA_AUTOINSTALL=0` disables startup auto-install; the user can still trigger `POST /api/preflight/install` from the settings page. The `claude` dep's `Install` installs via `npm install -g @anthropic-ai/claude-code`; its `DependsOn` ensures `npm` (and thus `node`) is installed first. AI features degrade gracefully — `llm.Gateway` falls back to `analyzeStub` when the CLI is missing, so a host without claude still boots.

### Platform abstraction (`internal/platform/`)

`platform.Client` interface (`ListOpenPRs`, `SubmitComment`, `CreatePR`, **commit-language detection**) with three impls: `githubClient`, `gitlabClient`, `giteaClient`. `platform.New(platform, baseURL, token)` returns the right one — GitHub hits `api.github.com`; GitLab/Gitea need a `base_url` (Gitea requires it). Tokens are stored in the `platform_tokens` table and linked to a project via `projects.platform_token_id`. **GPG 签名**: `handler/gpg.go` + `gpg_local.go` + `gpg_remote.go`;**commit-language detection** 在 Scanner 中推断 commit message 语言,写入 `projects.commit_lang_override`,merge 阶段注入 prompt。

### Scanner (`service/scanner.go`)

`Scan` detects project type from indicator files (`go.mod` → Go, `package.json` → Node.js, etc.), records AI config files (`CLAUDE.md`/`AGENTS.md`/`.cursorrules`) in `projects.claude_files`, indexes those docs plus a top-level structure summary into the `knowledge` table (upsert keyed on `source_ref`/`source_type`), and runs **commit-language detection** 推断 commit/PR message 语言(配合 Platform 的 `CreatePR`)。

### Deployment surfaces

- **本地开发**: `make run` + 独立 `npm run dev`(HMR)。
- **systemd 安装**: `sudo nova install`(`servicemgr_linux.go` 写 `/etc/systemd/system/nova.service` + `daemon-reload` + `enable --now`);卸载 `sudo nova uninstall`(保留数据目录)。
- **Agent Server**: `deploy/agent-worker/` 提供 systemd unit 模板;`agent-worker/server.mjs` Node 18+ listen `127.0.0.1:7000` 作为 nova ↔ 远端 `claude` CLI 桥接。
- **打包**: `packaging/nfpm.yaml`(nfpm 出 deb/rpm);`apt` 源配置见 `README.md → apt 安装`。
- **Release + CI/CD**: `docs/RELEASE.md` 详述 Release Please + ldflags 注入 `version.Version`;`.github/workflows/{deploy,terraform,release}.yml` 跑 CI/CD;`terraform/` IaC prod provisioning。
- **Docker Compose**: `deploy/docker-compose.{prod,preview}.yml` + `docker-compose.nginx-proxy.yml`(nginx-proxy TLS/host 路由);`deploy/setup/` host bootstrap 脚本;`scripts/start.{sh,ps1}` 启动辅助;`scripts/check-i18n-keys.mjs` + `scripts/check-i18n-cjk.sh` CI i18n 完整性检查。

## Frontend

- **API 客户端 (`frontend/src/api/`)** — **单文件 `client.ts` (1,880 行)** 集中 25 个 `*Api` 对象 + `api.get/post/put/patch/delete` 包装;`stream.ts` (93 行) 是 `createEventStream()` SSE 替代品。25 个 `*Api`:`projectsApi` / `dashboardApi` / `fsApi` / `memoriesApi` / `knowledgeApi` / `scannerApi` / `subTasksApi` / `requirementsApi` / `wizardApi` / `runnerApi` / `mergeApi` / `reviewApi` / `platformApi` / `claudeApi` / `llmApi` / `databaseApi` / **`rolesApi`(AI persona)** / `skillsApi` / `reportsApi` / `usageApi` / `authApi` / **`aclApi`(RBAC, 含义与 `rolesApi` 不同)** / `preflightApi` / `agentServersApi` / `schedulesApi`。
- **路由表 (`App.tsx:55-83`, `RequireAuth` + `<Layout>` 包裹)**: `/login` `Login`; `/` `Dashboard`; `/wizard` `WizardPage`; `/projects/add` `AddProject`; `/projects` `ProjectsList`; `/projects/:id` `ProjectDetail`; `/requirements` `RequirementsList`; `/requirements/calendar` `RequirementsCalendar`; `/requirements/:id` `RequirementDetail`; `/schedules` `SchedulesPage`; `/knowledge` `KnowledgePage`; `/chat` `Chat`(占位); `/reports` `Reports`(占位); `/settings` `Settings`(嵌套 `index=SettingsTokens` / `users=SettingsUsers` / `acl=SettingsACLRoles` / `roles=SettingsRoles` / `agent-servers=SettingsAgentServers` / `skills=SettingsSkills` / `claude=SettingsClaude` / `llm=SettingsLLM` / `subtask=SettingsSubTask` / `database=SettingsDatabase` / `preflight=SettingsPreflight`)。

- **i18n (`frontend/src/i18n/`)** — `i18next ^26` + `react-i18next ^17`,顶层 init 在 `i18n/index.ts`(模块顶层自调用 `initI18n()`,先于 App 挂载)。支持 `zh-CN` / `en-US`(`i18n/constants.ts`),18 对模块(`chat`/`common`/`components`/`dashboard`/`errors`/`knowledge`/`login`/`nav`/`projects`/`reports`/`requirements`/`schedules`/`settings`/`status`/`time`/`wizard` 等)。`LanguageSwitcher` 嵌在 `Layout` 与 `Login`。`utils/lang.ts` 持久化到 localStorage + `users.locale`。`utils/intl.ts` 提供 `fmtDate`/`fmtDateTime`。
- **登录**: `/login` + `Login.tsx` + `AuthProvider`(`utils/auth.tsx` 提供 `useAuth` hook)。401 → 自动跳 `/login`(`api/stream.ts:46-50`)。
- **components/** + **utils/**: `Layout` + `FolderPicker` + 三个 Chat(`DeepRefineChat`/`DocRefineChat`/`CodingChat`)消费 SSE over `fetch` + 手动 `ReadableStream`;另含 `AtMentionTextarea` / `ContextUsageBar` / `ExecEnvBadge` / `ExecEnvSelect` / `FullscreenButton` / `MarkdownViewer` / `ModelSelect` / `ScheduleModal` / `SessionContextStrip` / `StatusChips` / `SubTaskPanel` / `SummarizeToRequirementModal`,子目录 `calendar/` + `icons/` + `CreateRequirementForm/`;utils 含 13 个工具文件 `auth.tsx`(`AuthProvider`)/ `errMsg.ts` / `exportDesignPdf.tsx` / `intl.ts` / `lang.ts` / `logLines.ts` / `modelConfig.ts` / `modelWindow.ts` / `phaseGroups.ts` / `preview.ts` / `statusChips.ts` / `time.ts` / `useFullscreen.ts`。
- **vite.config.ts** — dev server proxies `/api` → `:9527`。
- **Dockerfile** — `dev` (npm install + `vite --host`), `build` (tsc + vite build), `prod` (nginx serving `dist`)。

## Key Conventions

- **API envelope**: every JSON response is `{success: bool, data: T, error: {code, message, suggestion?}}` via `writeJSON`/`writeError`. Throw on the client if `success` is false.
- **SSE frames**: `data: <json>\n\n`, flushed per event. Job-stream handlers replay history, push live lines, then emit one `job_done`/`done` frame and return.
- **Go router**: `mux.HandleFunc("METHOD /path/{id}", ...)`; read path params with `r.PathValue("id")`.
- **`WriteTimeout: 0`** on the server — SSE and long background jobs manage their own timeouts; do not add a global write timeout.
- **Language**: code/comments in English; UI text and AI prompts in Chinese.
- **Design system** (CSS variables in `index.css`): Indigo primary `#4F46E5`, Slate text `#64748B`, Emerald success `#10B981`.
- **Requirement status lifecycle**: `draft → analyzing → designing → designed → developing → done` (plus `archived`). Three role-gated stages (analyst / architect / developer); each completion gate is a manual user action.
- **i18n key 规范**: 增删 i18n key 必须同步更新 `frontend/src/i18n/locales/{zh-CN,en-US}.ts`;CI 跑 `scripts/check-i18n-keys.mjs`(zh/en 同步校验)与 `scripts/check-i18n-cjk.sh`(扫 zh 漏译)。
- **ExecEnv 约定**: `local / agent-server` 必须在 design / coding / sub-task 三阶段都通过 `ExecEnvSelect.tsx` 暴露选择;后端经 `BuildRemoteEnvPairs*` 决定本地 spawn 或 SSH 远端执行。