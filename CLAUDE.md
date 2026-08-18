# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

NovaWorkbench is a local-first, AI-native developer workbench. It manages multiple local projects through a Web UI and unifies: AI context (memories, knowledge bases), requirement tracking with AI-driven refinement/analysis/design, Claude Agent-driven code generation, `docker compose` run-session management, and AI-assisted PR review against GitHub/GitLab/Gitea.

The backend shells out to the **`claude` CLI** (`@anthropic-ai/claude-code`) for all AI features — there is no direct Anthropic API HTTP client. AI requests run `claude -p ... --output-format stream-json --dangerously-skip-permissions` as a subprocess with the project directory as CWD, giving Claude full tool access to read/write project files.

## Tech Stack

- **Backend**: Go 1.25, stdlib `net/http` + `database/sql` with three pure-Go drivers (no CGO): SQLite via `modernc.org/sqlite`, MySQL via `github.com/go-sql-driver/mysql`, PostgreSQL via `github.com/jackc/pgx/v5/stdlib`. Go 1.22+ router pattern (`METHOD /path/{id}`).
- **Frontend**: React 19 + TypeScript 6 + Vite 8 + React Router v7. Lint: `oxlint`.
- **Storage**: SQLite by default (WAL mode, single writer `MaxOpenConns=1`) at `~/.novaworkbench/data/nova.db`; optionally MySQL/PostgreSQL via `NOVA_DB_DRIVER`/`NOVA_DB_DSN` or the 设置→数据库 page (saved to `~/.novaworkbench/dbconfig.json`; env wins; restart required to switch). One-shot data copy: `go run ./cmd/server -migrate [-from <sqlite path>]`.
- **Dev**: Docker Compose (backend `:9527`, frontend `:5173`).

## Commands

```bash
# Backend (run from backend/)
go run ./cmd/server              # dev server on :9527
go build -o /app/server ./cmd/server   # what the Dockerfile builds
go vet ./...                     # lint
# There is no test suite yet.

# Frontend (run from frontend/)
npm run dev                      # Vite dev server on :5173, proxies /api -> :9527
npm run build                    # tsc -b && vite build
npm run lint                     # oxlint
npm run preview                  # serve the production build

# Docker Compose (from repo root)
docker-compose up                # both services; backend mounts ~/.novaworkbench and $HOME/workspace
```

Env vars the backend reads: `NOVA_PORT` (default `9527`), `CLAUDE_BIN` (default `claude`), `CLAUDE_TIMEOUT` (default `120s`; coding jobs floor it to `30m`), `NOVA_DB_DRIVER` (`sqlite` default | `mysql` | `postgres`), `NOVA_DB_DSN`, `NOVA_DB_PATH` (sqlite file, default `~/.novaworkbench/data/nova.db`). The frontend reads `VITE_API_BASE` (default `http://localhost:9527`).

## Architecture

### Layering (backend)

`cmd/server/main.go` wires everything; routes are registered on a single `http.ServeMux` with the Go 1.22 method+path pattern. Dependency flow:

```
handler/  ->  service/  ->  *sql.DB (model/ holds structs, no behavior)
                |
        llm.Gateway (shells out to claude CLI)
        store.JobStore (in-memory ring buffer of background jobs)
        platform.Client (github/gitlab/gitea HTTP)
```

- `handler/` — HTTP handlers. One struct per resource, constructor takes its service deps. `response.go` defines `writeJSON`/`writeError` (the `{success, data, error}` envelope) and `sendStatus` (SSE helper).
- `service/` — business logic + raw SQL queries. Each service holds a `*sql.DB`. There is no repository abstraction; services run SQL directly.
- `model/` — plain structs with JSON tags; no methods.
- `util/id.go` — `NewID(prefix)`: `<prefix>_<8 hex chars>` from `crypto/rand`.
- `middleware/` — `Logger` + `CORS` wrappers applied in `main.go`.

`main.go` constructs a single `store.NewJobStore(50)` and passes it to the three handlers that run background work (`WizardHandler`, `RunnerHandler`, `ReviewHandler`), so jobs are shared in-process across those features.

### Database layer & migrations (`internal/db/`)

`db.Init(cfg)` opens the driver selected by `LoadConfig()` (env > `dbconfig.json` > sqlite default) and runs `migrate()`. SQLite keeps WAL + `MaxOpenConns=1`; MySQL/Postgres get a small pool. The schema lives in one canonical SQLite-flavored DDL block (`schema.go`); `fixupSchema` translates it per dialect (Postgres: `DATETIME`→`TIMESTAMP`; MySQL: indexed `TEXT`→`VARCHAR`, backticked `` `key` ``, expression defaults `DEFAULT ('…')` on TEXT). Statements run one by one; idempotency relies on per-dialect "duplicate column"/"duplicate key name" error matching.

Services hold `*db.DB` (not `*sql.DB`) — a thin wrapper that rebinds `?`→`$N` on PostgreSQL (`Exec`/`Query`/`QueryRow`/`Begin`), quotes reserved identifiers via `Ident` (the `key` column is reserved in MySQL), and builds upsert suffixes via `OnConflict` (`ON CONFLICT … DO UPDATE` vs `ON DUPLICATE KEY UPDATE`). **Conventions for new SQL**: write `?` placeholders, go through the wrapper, use `Ident`/`OnConflict` where relevant. New columns still follow the ad-hoc pattern: append an `ALTER TABLE` to `alterColumns` in `schema.go` — no separate migration files.

`db.Migrate(src, dst)` copies all 12 tables parent-first, preserving IDs and skipping duplicate PKs; it backs both the `-migrate` CLI flag and `POST /api/settings/database/migrate` (settings UI).

Tables: `projects`, `memories`, `requirements`, `knowledge`, `conversations`, `refinement_chats`, `project_run_configs`, `platform_tokens`, `roles`. ID prefixes: `proj_`, `mem_`, `req_`, `kb_`, `sess_`, `job_`, `rc_`, `role_`.

### Role config (`roles` table, `internal/service/role.go` + `role_defaults.go`)

The roles (analyst / architect / developer for the wizard, reviewer for PR code review, extensible) each have a user-editable **system prompt** and **model** stored in the `roles` table, seeded from `service.DefaultRoles()` (`RoleService.SeedDefaults()` is called from `main.go`; seeding is per-key, so roles added in later releases are backfilled into existing DBs). Managed via `GET/PUT /api/settings/roles[/{id}]` and `POST .../{id}/reset` (reset restores the built-in system prompt, leaves model untouched). The review handler loads the `reviewer` role the same way wizard handlers load theirs, and runs via `llm.StreamCmd` so `--system-prompt`/`--model` and the configured Claude env apply.

These drive two claude CLI flags in `Gateway.streamArgs` (used by `StreamCmd` / `runClaudeStreamJSON` / `GenerateFinalRequirement` / `GenerateCode`): `--system-prompt <prompt>` (full replace) when non-empty and `PermissionMode != "plan"`, or `--append-system-prompt <prompt>` when in plan mode (to preserve the CLI's plan-mode instructions); `--model <id>` when non-empty (empty = CLI default). `PermissionMode` ("plan" or empty) selects `--permission-mode plan` vs `--dangerously-skip-permissions`. The wizard handlers load the active role via `WizardHandler.roleConfig(key)` (returns empty strings on miss so a broken role config never blocks the pipeline). The persona lives in the role's system prompt; the `-p` prompt carries only dynamic task content (requirement title, conversation history, JSON schema, etc.). `RefineDoc`/`ApplyDoc` are cross-role doc-refinement loops and currently pass empty system prompt/model (CLI defaults).

### LLM Gateway (`internal/llm/gateway.go`)

`Gateway` wraps the local `claude` CLI. Two execution modes:

1. **`runClaude`** — single-shot, `--output-format text`. Used for quick JSON-returning prompts (`ChatRefine`, `Analyze`). Falls back to `analyzeStub` if the CLI is not in PATH.
2. **`runClaudeStreamJSON`** — `--output-format stream-json --verbose --dangerously-skip-permissions`. Runs Claude with full tool use; parses each stdout line as a JSON event and extracts the `"result"` event's text. Used by `GenerateDesign`, `GenerateFinalRequirement`, `GenerateCode`.

`GenerateCode` returns an `*exec.Cmd` (unstarted) so the **handler** owns the lifecycle and parses the stream itself — this is the pattern for anything that needs live progress.

### SSE + JobStore streaming pattern (the key architectural piece)

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

`toolCallLabel` (`wizard.go`) maps Claude tool names to Chinese labels (Read/Bash/Glob/Grep/Write/Edit). `extractJSON` brace-matches to pull JSON out of prose/fenced output. `isLikelyJSON` checks whether a stored doc starts with `{` (distinguishes plan-Markdown design docs from legacy JSON). `runClaudeStream` also captures `out.planContent` — the full Markdown from a plan-mode `Write` tool_use to `~/.claude/plans/*.md`, used by `ArchitectDesign`. `scanner.Buffer` is raised to `256K→4M` everywhere long output is expected — keep that when adding streaming handlers.

**B. JobStore + SSE** — for truly background work. `store.JobStore` is an in-memory **ring buffer** (cap 50). `Job.Append` fans each `LogLine` out to subscriber channels; `Job.Subscribe` pre-seeds a channel with existing lines then stays open until `Finish` closes it. `POST .../start` creates a job, returns its ID immediately, and runs `claude`/`docker compose` in a goroutine appending lines. `GET .../jobs/{id}/stream` subscribes via SSE (replays history first, then pushes new lines, then a `job_done` frame). `GET .../jobs/{id}` returns a JSON snapshot. Used by `wizard.StartCoding` (Claude codegen), `runner.Start` (docker compose up), and `review.StartReview` (PR review).

Because jobs are in-memory only, they do not survive a restart. The `RunSession` map in `RunnerHandler` (keyed by project ID) tracks live `docker compose` processes so `Stop` can SIGINT them, with a 5s force-kill fallback.

### Wizard pipeline (requirement → code)

The full flow lives in `handler/wizard.go` and the `RequirementDetail`/`DeepRefineChat`/`DocRefineChat`/`CodingChat` frontend components. A requirement moves through **three role-gated stages**, each completed by a manual user action (no AI self-declaration):

1. `analyst-chat` (was `deep-refine`) — multi-turn SSE conversation. The requirement analyst reads project files (via tools) and refines the requirement. Conversation history is threaded through each request and returned in the `done` event. Completion is **manual** (the prompt no longer asks Claude to emit `[ANALYSIS_COMPLETE]`).
2. `architect-design` (was `design-sse` on `/api/requirements/{id}/...`) — runs Claude in **plan mode** (`--permission-mode plan`), which restricts Claude to read-only tools + the plan-file Write. Claude explores the codebase, writes a technical implementation plan (Markdown) to `~/.claude/plans/<slug>.md`, and the handler captures the full plan content from the `Write` tool_use event in the stream (`runClaudeStream` → `out.planContent`). The plan Markdown is persisted to `requirements.design_docs` via `reqSvc.UpdateDesign`, status → `designing`. In plan mode the role persona is passed via `--append-system-prompt` (not `--system-prompt`) so the CLI's plan-mode instructions survive. Legacy JSON-format designs (from older runs) are still rendered field-by-field; `parseDesign` on the frontend detects plan Markdown by trying `JSON.parse` and falling back to `{ plan_markdown: raw }`. The architect stage forks off the analyst session (`--resume <analysis_sid> --fork-session`) so it inherits the full analysis conversation — no separate "需求完善完成" finalization step exists; the user proceeds from analyst chat directly to "生成技术方案".
3. `refine-doc` / `apply-doc` — iterative refinement of a stored doc. `doc_type` is `"design"` (updates `design_docs`) or `"coding"` (returns a plain-text dev instruction, no DB write). `refine-doc` ends when Claude emits `[REFINE_COMPLETE]`. For `design` docs, `apply-doc` detects whether the stored doc is plan Markdown (`!isLikelyJSON`) and asks Claude to output updated Markdown (not the legacy JSON schema); the raw Markdown is persisted directly (no `extractJSON`).
4. `start-coding` — creates a JobStore job, checks out the dev branch, and runs `llm.GenerateCode` in a goroutine, streaming `tool_call`/`message`/`tool_result`/`done` events into the job. On `job_done` the frontend sets status `developing`; the user manually marks `done`.

The gates map to status transitions: `draft → analyzing` (enter analyst chat) → `designing` (architect-design) → `designed` (manual 方案完成) → `developing` (start-coding) → `done` (manual 开发完成).

`readProjectContext` (`wizard.go`) builds a ~40 KB prompt context by walking the project tree, always including doc files (`CLAUDE.md`, `AGENTS.md`, `README*`, `.cursorrules`) and source files whose paths match keywords from the requirement title. It skips `.git`/`node_modules`/`vendor`/`dist` and binary/large files.

### Platform abstraction (`internal/platform/`)

`platform.Client` interface (`ListOpenPRs`, `SubmitComment`) with three impls: `githubClient`, `gitlabClient`, `giteaClient`. `platform.New(platform, baseURL, token)` returns the right one — GitHub hits `api.github.com`; GitLab/Gitea need a `base_url` (Gitea requires it). Tokens are stored in the `platform_tokens` table and linked to a project via `projects.platform_token_id`. `review.go` lists PRs, starts an AI review job (Claude reads the diff in the project workdir and posts a comment), and streams the job.

### Scanner (`service/scanner.go`)

`Scan` detects project type from indicator files (`go.mod` → Go, `package.json` → Node.js, etc.), records AI config files (`CLAUDE.md`/`AGENTS.md`/`.cursorrules`) in `projects.claude_files`, and indexes those docs plus a top-level structure summary into the `knowledge` table (upsert keyed on `source_ref`/`source_type`).

## Frontend

- `api/client.ts` — single `request<T>` wrapper enforcing the `{success, data, error}` envelope; `api` object with `get/post/put/patch/delete`. All endpoint clients (`projectsApi`, `requirementsApi`, `reviewApi`, `runnerApi`, `wizardApi`, `knowledgeApi`, `memoriesApi`, `scannerApi`, `platformApi`, `fsApi`, `dashboardApi`) are grouped here with their TypeScript interfaces. `VITE_API_BASE` selects the backend.
- `App.tsx` — `BrowserRouter` with a single `<Layout>` outlet and child routes: `dashboard`, `wizard`, `projects`, `projects/:id`, `requirements`, `requirements/:id`, `knowledge`, `chat`, `reports`, `settings`, `projects/add`. Several are still placeholders (`PlaceholderPages`).
- `components/` — `Layout` (app shell), `FolderPicker` (filesystem browser backed by `/api/fs/*`), and the three chat components (`DeepRefineChat`, `DocRefineChat`, `CodingChat`) that consume SSE streams over `fetch` with manual `ReadableStream` parsing.
- `vite.config.ts` — dev server proxies `/api` → `:9527`.
- `Dockerfile` — `dev` (npm install + `vite --host`), `build` (tsc + vite build), `prod` (nginx serving `dist`).

## Key Conventions

- **API envelope**: every JSON response is `{success: bool, data: T, error: {code, message, suggestion?}}` via `writeJSON`/`writeError`. Throw on the client if `success` is false.
- **SSE frames**: `data: <json>\n\n`, flushed per event. Job-stream handlers replay history, push live lines, then emit one `job_done`/`done` frame and return.
- **Go router**: `mux.HandleFunc("METHOD /path/{id}", ...)`; read path params with `r.PathValue("id")`.
- **`WriteTimeout: 0`** on the server — SSE and long background jobs manage their own timeouts; do not add a global write timeout.
- **Language**: code/comments in English; UI text and AI prompts in Chinese.
- **Design system** (CSS variables in `index.css`): Indigo primary `#4F46E5`, Slate text `#64748B`, Emerald success `#10B981`.
- **Requirement status lifecycle**: `draft → analyzing → designing → designed → developing → done` (plus `archived`). Three role-gated stages (analyst / architect / developer); each completion gate is a manual user action.
