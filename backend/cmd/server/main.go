package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/handler"
	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/middleware"
	"github.com/novaworkbench/backend/internal/preflight"
	"github.com/novaworkbench/backend/internal/secret"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/web"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	migrateFlag := flag.Bool("migrate", false,
		"one-shot data migration: copy all data from a SQLite file into the configured target database (NOVA_DB_DRIVER/NOVA_DB_DSN or dbconfig.json), then exit")
	fromFlag := flag.String("from", db.DefaultSQLitePath,
		"SQLite source path for -migrate")
	flag.Parse()

	if *migrateFlag {
		runMigration(*fromFlag)
		return
	}

	log.Println("NovaWorkbench Backend starting...")

	// Database — driver from env > ~/.novaworkbench/dbconfig.json > sqlite default.
	cfg := db.LoadConfig()
	database, err := db.Init(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer database.Close()

	// Load (or generate) the master encryption key used by internal/secret to
	// seal Agent-server credentials. Failure here is fatal — there is no
	// recoverable mode for a missing master key, and silently degrading to
	// plaintext storage would defeat the whole feature.
	if err := secret.Init(); err != nil {
		log.Fatalf("Failed to initialize secret store: %v", err)
	}
	log.Printf("[secret] master key loaded from %s", secret.KeyPath())

	// Services
	platformSvc := service.NewPlatformTokenService(database)
	projectSvc := service.NewProjectService(database, platformSvc)
	memorySvc := service.NewMemoryService(database)
	knowledgeSvc := service.NewKnowledgeService(database)
	reqSvc := service.NewRequirementService(database)
	roleSvc := service.NewRoleService(database)
	settingSvc := service.NewSettingService(database)
	reportSvc := service.NewReportService(database)
	jobLogSvc := service.NewJobLogService(database)
	usageSvc := service.NewUsageService(database)
	aclSvc := service.NewACLService(database)
	skillSvc := service.NewSkillService(database)
	agentSvrSvc := service.NewAgentServerService(database)
	subTaskSvc := service.NewSubTaskService(database)

	// Seed built-in roles on first run (idempotent).
	if err := roleSvc.SeedDefaults(); err != nil {
		log.Printf("[main] role seed: %v", err)
	}
	// Migrate the developer role prompt on upgrade: existing databases whose
	// developer role still carries the pre-sub-task-collaboration "执行者"
	// persona get rewritten to the new "统筹协调者" persona so the start-coding
	// turn actually emits a task breakdown (without this, users who upgraded
	// would still see Claude write code directly because SeedDefaults leaves
	// existing rows alone). MigrateDeveloperRole is a no-op when the row
	// already carries the new prompt or was genuinely user-customized.
	if migrated, err := roleSvc.MigrateDeveloperRole(); err != nil {
		log.Printf("[main] developer role migrate: %v", err)
	} else if migrated {
		log.Println("[main] developer role prompt 已升级到「统筹协调者」版本")
	}
	// Second-generation upgrade: v2 coordinator prompt (text JSON block) →
	// v3 (Write tool writes subtasks.json; backend captures the structured
	// tool_use). Same idempotent, per-boot style as MigrateDeveloperRole.
	if migrated, err := roleSvc.MigrateDeveloperRoleWriteChannel(); err != nil {
		log.Printf("[main] developer role write-channel migrate: %v", err)
	} else if migrated {
		log.Println("[main] developer role prompt 已升级到「Write 工具提交拆分」版本")
	}

	// Sub-task execution is driven by in-memory goroutines — a restart leaves
	// running/pending rows orphaned (eternal spinner in the UI). Recover them
	// to error so the user re-dispatches via 重新拆分.
	if n, err := subTaskSvc.RecoverInterrupted(); err != nil {
		log.Printf("[main] sub-task orphan recovery: %v", err)
	} else if n > 0 {
		log.Printf("[main] 回收了 %d 个因服务重启而中断的子任务", n)
	}

	// Seed the RBAC catalog (permissions / roles / bindings) and a default
	// admin account on first run. The default admin password is printed once
	// so the first login can bootstrap user/project assignment.
	if pw, err := aclSvc.SeedDefaults(); err != nil {
		log.Printf("[main] acl seed: %v", err)
	} else if pw != "" {
		log.Printf("[acl] default admin account created — username: admin  password: %s  (change it after first login)", pw)
	}

	// Claude CLI configurations (multi-config). Constructed before the gateway
	// so it can serve as the ClaudeEnvProvider. MigrateLegacy is one-way +
	// idempotent: it seeds a single active "默认配置" from the legacy settings
	// keys the first time the new table is empty.
	claudeCfgSvc := service.NewClaudeConfigService(database)
	if err := claudeCfgSvc.MigrateLegacy(); err != nil {
		log.Printf("[main] claude config legacy migrate: %v", err)
	}

	// Preflight dependency registry — probes the host for Claude CLI / Node /
	// npm / git / docker and (when NOVA_AUTOINSTALL is not "0") tries to
	// install anything missing in the background. Same advisory style as
	// llm.New: never halts the server; AI features already degrade to stub
	// responses when claude is missing (see gateway.go:53).
	pfRegistry := preflight.New(os.Getenv("CLAUDE_BIN"))
	pfResults := pfRegistry.CheckAll(context.Background())
	for _, r := range pfResults {
		if !r.Installed {
			log.Printf("[preflight] ⚠️  %s 未安装: %s (手动: %s)", r.Label, r.Err, r.Manual)
		} else {
			log.Printf("[preflight] ✅ %s: %s (%s)", r.Label, r.Path, r.Version)
		}
	}
	if os.Getenv("NOVA_AUTOINSTALL") != "0" {
		// Best-effort background install. Uses a noop sink at startup since
		// the shared JobStore isn't constructed yet — installs run but
		// progress isn't streamed until the user opens the settings page.
		go pfRegistry.EnsureAll(context.Background(), nil)
	}

	// Shared LLM gateway (wraps the claude CLI). The active claude_configs row
	// supplies ANTHROPIC_AUTH_TOKEN / ANTHROPIC_BASE_URL (via claudeCfgSvc, the
	// ClaudeEnvProvider); settingSvc supplies the direct HTTP LLM channel for
	// lightweight tasks. Switching the active config applies immediately on
	// the next subprocess — no restart needed.
	llmGateway := llm.New(claudeCfgSvc, settingSvc)

	// Scanner depends on the LLM gateway (auto project description generation)
	// and the project service (description persistence).
	scannerSvc := service.NewScannerService(database, projectSvc, llmGateway)

	// Handlers
	projectH := handler.NewProjectHandler(projectSvc, scannerSvc)
	healthH := handler.NewHealthHandler(pfRegistry)
	dashboardH := handler.NewDashboardHandler(projectSvc)
	fsH := handler.NewFsHandler()
	memoryH := handler.NewMemoryHandler(memorySvc)
	knowledgeH := handler.NewKnowledgeHandler(knowledgeSvc)
	scannerH := handler.NewScannerHandler(scannerSvc)
	sharedJobs := store.NewJobStore(50)
	preflightH := handler.NewPreflightHandler(pfRegistry, sharedJobs)
	reqH := handler.NewRequirementHandler(reqSvc, llmGateway, sharedJobs, usageSvc)
wizardH := handler.NewWizardHandler(projectSvc, reqSvc, knowledgeSvc, llmGateway, sharedJobs, roleSvc, jobLogSvc, claudeCfgSvc, usageSvc, skillSvc, platformSvc, agentSvrSvc, subTaskSvc)
	runnerH := handler.NewRunnerHandler(projectSvc, sharedJobs, database)
	reviewH := handler.NewReviewHandler(projectSvc, platformSvc, roleSvc, llmGateway, sharedJobs, jobLogSvc, claudeCfgSvc, usageSvc)
	reportH := handler.NewReportHandler(projectSvc, reportSvc, llmGateway, sharedJobs)
	mergeH := handler.NewMergeHandler(projectSvc, reqSvc, llmGateway, sharedJobs, roleSvc, platformSvc, jobLogSvc, claudeCfgSvc, usageSvc)
	platformH := handler.NewPlatformHandler(platformSvc)
	roleH := handler.NewRoleHandler(roleSvc, claudeCfgSvc)
	settingH := handler.NewSettingHandler(settingSvc)
	claudeCfgH := handler.NewClaudeConfigHandler(claudeCfgSvc)
	databaseH := handler.NewDatabaseHandler(database, cfg)
	usageH := handler.NewUsageHandler(usageSvc)
	authH := handler.NewAuthHandler(aclSvc)
	aclH := handler.NewACLHandler(aclSvc)
	skillH := handler.NewSkillHandler(skillSvc)

	// Agent-server resource: SSH targets for remote claude execution. The
	// credential is sealed by internal/secret (AES-256-GCM); the wizard's
	// StartCoding remote branch consumes this service when a request carries
	// agent_server_id.
	agentSvrH := handler.NewAgentServerHandler(agentSvrSvc, sharedJobs)

	// Router
	mux := http.NewServeMux()

	// Health
	mux.HandleFunc("GET /api/health", healthH.Health)

	// Preflight — dependency snapshot + background install jobs. Reuses the
	// shared JobStore + the same SSE shape as wizardH.StreamJob so the
	// frontend can subscribe identically.
	mux.HandleFunc("GET /api/preflight", preflightH.Snapshot)
	mux.HandleFunc("POST /api/preflight/install", preflightH.Install)
	mux.HandleFunc("GET /api/preflight/jobs/{id}", preflightH.GetJob)
	mux.HandleFunc("GET /api/preflight/jobs/{id}/stream", preflightH.StreamJob)

	// Auth (login is public; logout/me require a session — enforced by the
	// Auth middleware, which is applied to the whole mux below).
	mux.HandleFunc("POST /api/auth/login", authH.Login)
	mux.HandleFunc("POST /api/auth/logout", authH.Logout)
	mux.HandleFunc("GET /api/auth/me", authH.Me)

	// ACL — user / role / permission management. Every route is guarded by
	// the setting.users (user management) or setting.acl (role/permission
	// management) permission; admins ("*") always pass.
	mux.HandleFunc("GET /api/acl/users", middleware.RequirePermission(aclSvc, "setting.users")(http.HandlerFunc(aclH.ListUsers)).ServeHTTP)
	mux.HandleFunc("POST /api/acl/users", middleware.RequirePermission(aclSvc, "setting.users")(http.HandlerFunc(aclH.CreateUser)).ServeHTTP)
	mux.HandleFunc("GET /api/acl/users/{id}", middleware.RequirePermission(aclSvc, "setting.users")(http.HandlerFunc(aclH.GetUser)).ServeHTTP)
	mux.HandleFunc("PUT /api/acl/users/{id}", middleware.RequirePermission(aclSvc, "setting.users")(http.HandlerFunc(aclH.UpdateUser)).ServeHTTP)
	mux.HandleFunc("DELETE /api/acl/users/{id}", middleware.RequirePermission(aclSvc, "setting.users")(http.HandlerFunc(aclH.DeleteUser)).ServeHTTP)
	mux.HandleFunc("PUT /api/acl/users/{id}/projects", middleware.RequirePermission(aclSvc, "setting.users")(http.HandlerFunc(aclH.AssignProjects)).ServeHTTP)

	mux.HandleFunc("GET /api/acl/roles", middleware.RequirePermission(aclSvc, "setting.acl")(http.HandlerFunc(aclH.ListRoles)).ServeHTTP)
	mux.HandleFunc("POST /api/acl/roles", middleware.RequirePermission(aclSvc, "setting.acl")(http.HandlerFunc(aclH.CreateRole)).ServeHTTP)
	mux.HandleFunc("GET /api/acl/roles/{id}", middleware.RequirePermission(aclSvc, "setting.acl")(http.HandlerFunc(aclH.GetRole)).ServeHTTP)
	mux.HandleFunc("PUT /api/acl/roles/{id}", middleware.RequirePermission(aclSvc, "setting.acl")(http.HandlerFunc(aclH.UpdateRole)).ServeHTTP)
	mux.HandleFunc("DELETE /api/acl/roles/{id}", middleware.RequirePermission(aclSvc, "setting.acl")(http.HandlerFunc(aclH.DeleteRole)).ServeHTTP)

	mux.HandleFunc("GET /api/acl/permissions", middleware.RequirePermission(aclSvc, "setting.acl")(http.HandlerFunc(aclH.ListPermissions)).ServeHTTP)

	// Dashboard
	mux.HandleFunc("GET /api/dashboard", dashboardH.Dashboard)

	// Projects
	mux.HandleFunc("GET /api/projects", projectH.List)
	mux.HandleFunc("POST /api/projects", middleware.RequirePermission(aclSvc, "project.manage")(http.HandlerFunc(projectH.Add)).ServeHTTP)
	mux.HandleFunc("GET /api/projects/trash", projectH.Trash)
	mux.HandleFunc("GET /api/projects/{id}", projectH.Get)
	mux.HandleFunc("DELETE /api/projects/{id}", middleware.RequirePermission(aclSvc, "project.manage")(http.HandlerFunc(projectH.Remove)).ServeHTTP)
	mux.HandleFunc("POST /api/projects/{id}/restore", middleware.RequirePermission(aclSvc, "project.manage")(http.HandlerFunc(projectH.Restore)).ServeHTTP)
	mux.HandleFunc("DELETE /api/projects/{id}/purge", middleware.RequirePermission(aclSvc, "project.manage")(http.HandlerFunc(projectH.Purge)).ServeHTTP)
	mux.HandleFunc("POST /api/projects/{id}/scan", scannerH.Scan)

	// Run sessions (docker compose)
	mux.HandleFunc("POST /api/projects/{id}/run/start", runnerH.Start)
	mux.HandleFunc("POST /api/projects/{id}/run/stop", runnerH.Stop)
	mux.HandleFunc("GET /api/projects/{id}/run/status", runnerH.Status)

	// PR list + Claude Review
	mux.HandleFunc("GET /api/projects/{id}/prs", reviewH.ListPRs)
	mux.HandleFunc("POST /api/projects/{id}/prs/review", reviewH.StartReview)
	mux.HandleFunc("GET /api/projects/{id}/prs/jobs/{job_id}/stream", reviewH.StreamReviewJob)
	mux.HandleFunc("POST /api/projects/{id}/prs/{pr_number}/comment", reviewH.SubmitComment)

	// Project platform config
	mux.HandleFunc("PATCH /api/projects/{id}", middleware.RequirePermission(aclSvc, "project.manage")(http.HandlerFunc(projectH.UpdateBasicInfo)).ServeHTTP)
	mux.HandleFunc("PATCH /api/projects/{id}/platform", projectH.UpdatePlatform)

	// Project description (AI-generated from CLAUDE.md, manual-edit lockable)
	mux.HandleFunc("PUT /api/projects/{id}/description", projectH.UpdateDescription)
	mux.HandleFunc("POST /api/projects/{id}/description/regenerate", projectH.RegenerateDescription)
	mux.HandleFunc("POST /api/projects/descriptions/backfill", projectH.BackfillDescriptions)

	// Weekly reports (AI-generated from git log + requirement data)
	mux.HandleFunc("GET /api/projects/{id}/reports", reportH.List)
	mux.HandleFunc("GET /api/projects/{id}/reports/rule", reportH.GetRule)
	mux.HandleFunc("PUT /api/projects/{id}/reports/rule", reportH.SaveRule)
	mux.HandleFunc("GET /api/projects/{id}/reports/rule-presets", reportH.GetRulePresets)
	mux.HandleFunc("GET /api/projects/{id}/reports/git-info", reportH.GetGitInfo)
	mux.HandleFunc("POST /api/projects/{id}/reports/generate", reportH.Generate)
	mux.HandleFunc("GET /api/projects/{id}/reports/jobs/{job_id}/stream", reportH.StreamJob)
	mux.HandleFunc("GET /api/projects/{id}/reports/{report_id}", reportH.Get)
	mux.HandleFunc("DELETE /api/projects/{id}/reports/{report_id}", reportH.Delete)

	// Platform tokens (settings)
	mux.HandleFunc("GET /api/settings/tokens", platformH.List)
	mux.HandleFunc("POST /api/settings/tokens", platformH.Create)
	mux.HandleFunc("PUT /api/settings/tokens/{id}", platformH.Update)
	mux.HandleFunc("DELETE /api/settings/tokens/{id}", platformH.Delete)

	// Agent servers (settings) — remote Linux/macOS execution targets with
	// AES-256-GCM-encrypted credentials. CRUD + Check/Install (background
	// JobStore jobs streamed over SSE, same shape as wizard/preflight).
	mux.HandleFunc("GET /api/settings/agent-servers", agentSvrH.List)
	mux.HandleFunc("POST /api/settings/agent-servers", agentSvrH.Create)
	mux.HandleFunc("GET /api/settings/agent-servers/{id}", agentSvrH.Get)
	mux.HandleFunc("PUT /api/settings/agent-servers/{id}", agentSvrH.Update)
	mux.HandleFunc("DELETE /api/settings/agent-servers/{id}", agentSvrH.Delete)
	mux.HandleFunc("POST /api/settings/agent-servers/{id}/check", agentSvrH.Check)
	mux.HandleFunc("POST /api/settings/agent-servers/{id}/install", agentSvrH.Install)
	mux.HandleFunc("GET /api/settings/agent-servers/jobs/{id}", agentSvrH.GetJob)
	mux.HandleFunc("GET /api/settings/agent-servers/jobs/{id}/stream", agentSvrH.StreamJob)

	// Roles (settings) — per-role system prompt + model, drives claude CLI flags
	mux.HandleFunc("GET /api/settings/roles", roleH.List)
	mux.HandleFunc("GET /api/settings/roles/{id}", roleH.Get)
	mux.HandleFunc("PUT /api/settings/roles/{id}", roleH.Update)
	mux.HandleFunc("POST /api/settings/roles/{id}/reset", roleH.Reset)

	// Skills (settings) — managed skill files written into .claude/agents/ before
	// each claude CLI invocation; market endpoint proxies a remote registry manifest.
	mux.HandleFunc("GET /api/settings/skills", skillH.List)
	mux.HandleFunc("POST /api/settings/skills", skillH.Create)
	mux.HandleFunc("PUT /api/settings/skills/{id}", skillH.Update)
	mux.HandleFunc("DELETE /api/settings/skills/{id}", skillH.Delete)
	mux.HandleFunc("GET /api/settings/skills/markets", skillH.Markets)
	mux.HandleFunc("GET /api/settings/skills/market", skillH.Market)

	// Claude CLI configurations (settings) — multiple named configs (auth
	// token + base URL + model list); the active one is injected as env vars
	// into every claude subprocess, and switching it also re-points all roles
	// to its default model.
	mux.HandleFunc("GET /api/settings/claude/configs", claudeCfgH.List)
	mux.HandleFunc("POST /api/settings/claude/configs", claudeCfgH.Create)
	mux.HandleFunc("PUT /api/settings/claude/configs/{id}", claudeCfgH.Update)
	mux.HandleFunc("DELETE /api/settings/claude/configs/{id}", claudeCfgH.Delete)
	mux.HandleFunc("POST /api/settings/claude/configs/{id}/activate", claudeCfgH.Activate)
	mux.HandleFunc("GET /api/settings/claude/configs/active", claudeCfgH.Active)

	// Direct HTTP LLM channel (settings) — base URL + API key + model for
	// lightweight tasks (requirement title distillation). Bypasses claude CLI.
	mux.HandleFunc("GET /api/settings/llm", settingH.GetLLM)
	mux.HandleFunc("PUT /api/settings/llm", settingH.UpdateLLM)

	// Database (settings) — driver info, connection test, save (takes effect
	// on restart), and one-shot SQLite → MySQL/Postgres data migration.
	mux.HandleFunc("GET /api/settings/database", databaseH.Get)
	mux.HandleFunc("POST /api/settings/database/test", databaseH.Test)
	mux.HandleFunc("PUT /api/settings/database", databaseH.Save)
	mux.HandleFunc("POST /api/settings/database/migrate", databaseH.Migrate)

	// File system browser
	mux.HandleFunc("GET /api/fs/ls", fsH.ListDir)
	mux.HandleFunc("GET /api/fs/validate", fsH.ValidatePath)
	mux.HandleFunc("GET /api/fs/git-branches", fsH.GitBranches)

	// Requirements
	mux.HandleFunc("GET /api/requirements", reqH.List)
	mux.HandleFunc("POST /api/requirements", reqH.Create)
	mux.HandleFunc("GET /api/requirements/{id}", reqH.Get)
	mux.HandleFunc("PUT /api/requirements/{id}", reqH.Update)
	mux.HandleFunc("PATCH /api/requirements/{id}/status", reqH.UpdateStatus)
	mux.HandleFunc("PATCH /api/requirements/{id}/kind", reqH.UpdateKind)
	// PromoteFromIdea: turns a finished idea-discussion thread into a new
	// requirement row (kind=requirement, source_requirement_id=idea.id). The
	// original idea is left fully intact — see service.PromoteFromIdea.
	mux.HandleFunc("POST /api/requirements/{id}/promote", reqH.PromoteFromIdea)
	mux.HandleFunc("DELETE /api/requirements/{id}", reqH.Delete)
	mux.HandleFunc("GET /api/requirements/{id}/chat-history", reqH.GetChatHistory)
	mux.HandleFunc("PUT /api/requirements/{id}/chat-history", reqH.SaveChatHistory)
	mux.HandleFunc("GET /api/requirements/{id}/coding-chat", reqH.GetCodingChat)
	mux.HandleFunc("PUT /api/requirements/{id}/coding-chat", reqH.SaveCodingChat)
	mux.HandleFunc("DELETE /api/requirements/{id}/analysis-session", reqH.ClearAnalysisSession)
	mux.HandleFunc("POST /api/requirements/{id}/archive", reqH.Archive)
	mux.HandleFunc("POST /api/requirements/{id}/unarchive", reqH.Unarchive)

	// Token usage (per-requirement / per-project aggregation; review rows
	// are recorded but excluded from project totals — surfaced separately).
	mux.HandleFunc("GET /api/usage/requirement/{id}", usageH.Requirement)
	mux.HandleFunc("GET /api/usage/requirement/{id}/rows", usageH.Rows)
	mux.HandleFunc("GET /api/usage/by-requirement", usageH.ByRequirement)
	mux.HandleFunc("GET /api/usage/project/{id}", usageH.Project)
	// By-job usage summary — drives the per-sub-task 🪙 token strip on the
	// SubTaskPanel. Returns the zero-value summary when no token_usage row
	// exists yet (still-running / failed before result event).
	mux.HandleFunc("GET /api/usage/job/{jobId}", usageH.ByJobID)

	// Wizard (requirement refinement + coding via Claude CLI)
	// Three-role stage-gate: analyst → architect → developer.
	mux.HandleFunc("POST /api/wizard/analyst-chat", wizardH.AnalystChat)
	mux.HandleFunc("POST /api/wizard/developer-chat", wizardH.DeveloperChat)
	mux.HandleFunc("POST /api/wizard/architect-design", wizardH.ArchitectDesign)
	mux.HandleFunc("POST /api/wizard/start-coding", wizardH.StartCoding)
	mux.HandleFunc("POST /api/wizard/adjust-coding", wizardH.AdjustCoding)
	mux.HandleFunc("POST /api/wizard/continue-coding", wizardH.ContinueCoding)
	mux.HandleFunc("GET /api/wizard/jobs/{id}", wizardH.GetJob)
	mux.HandleFunc("GET /api/wizard/jobs/{id}/stream", wizardH.StreamJob)
	mux.HandleFunc("POST /api/wizard/refine-doc", wizardH.RefineDoc)
	mux.HandleFunc("POST /api/wizard/apply-doc", wizardH.ApplyDoc)

	// Context compression (analyst / architect / coding) — see wizard
	// CompressContext for the SSE protocol and persist semantics. The
	// lightweight GET mirrors a single stage's stored summary + the
	// compressed_at timestamp for the "📦 已压缩" badge.
	mux.HandleFunc("POST /api/wizard/compress-context", wizardH.CompressContext)
	mux.HandleFunc("GET /api/wizard/requirement/{id}/context-summary", wizardH.GetContextSummary)

	// Sub-task (子任务) endpoints — manually-triggered child agents that
	// fork the requirement's coding_session_id so they share the main
	// agent's context. Three REST routes; SSE streams reuse the existing
	// /api/wizard/jobs/{id}/stream endpoint (sub_task.job_id is the wire
	// that lets SubTaskPanel subscribe to the same event flow as
	// CodingChat).
	mux.HandleFunc("POST /api/requirements/{id}/sub-tasks", wizardH.StartSubTask)
	mux.HandleFunc("GET /api/requirements/{id}/sub-tasks", wizardH.ListSubTasks)
	mux.HandleFunc("GET /api/requirements/{id}/sub-tasks/{sid}", wizardH.GetSubTask)
	// Append a follow-up instruction to an existing sub-task (resumes the
	// parent's claude session via --fork-session). Same scope as
	// AdjustCoding — reuses the spawn helper in wizard.go.
	mux.HandleFunc("POST /api/requirements/{id}/sub-tasks/{sid}/adjust", wizardH.AdjustSubTask)
	// Re-run a FAILED sub-task with its original prompt (optionally switching
	// models). Forks the failed run's source session for a clean retry.
	mux.HandleFunc("POST /api/requirements/{id}/sub-tasks/{sid}/redo", wizardH.RedoSubTask)
	// Manual re-split: resumes the coding session with the decomposition
	// trigger and runs the same parse+dispatch pipeline as StartCoding's
	// auto-orchestrate. Escape hatch for when auto-orchestration produced
	// no children (or the user wants a fresh split).
	mux.HandleFunc("POST /api/requirements/{id}/re-orchestrate", wizardH.ReOrchestrate)
	// NOTE: /api/requirements/{id}/orchestrate is no longer registered —
	// the old manual "一键编排" endpoint is replaced by StartCoding's auto
	// dispatch (wizard.tryAutoOrchestrate). The main agent outputs
	// [SUBTASKS_READY] as part of its normal decomposition turn and the
	// handler fans out automatically.

	// Merge / PR step (post-coding 合入). Local merge + AI conflict
	// resolution, or push + create-PR link. Jobs reuse the wizard job stream.
	mux.HandleFunc("GET /api/requirements/{id}/merge/state", mergeH.State)
	mux.HandleFunc("POST /api/requirements/{id}/merge/local", mergeH.LocalMerge)
	mux.HandleFunc("POST /api/requirements/{id}/merge/abort", mergeH.Abort)
	mux.HandleFunc("POST /api/requirements/{id}/merge/continue", mergeH.Continue)
	mux.HandleFunc("POST /api/requirements/{id}/merge/resolve", mergeH.Resolve)
	mux.HandleFunc("POST /api/requirements/{id}/merge/push", mergeH.Push)

	// Worktree cleanup — remove a finished/abandoned requirement's isolated
	// dev worktree + branch so parallel dev dirs don't accumulate on disk.
	mux.HandleFunc("POST /api/requirements/{id}/worktree/cleanup", mergeH.Cleanup)

	// Memories
	mux.HandleFunc("GET /api/memories", memoryH.List)
	mux.HandleFunc("POST /api/memories", memoryH.Create)
	mux.HandleFunc("GET /api/memories/{id}", memoryH.Get)
	mux.HandleFunc("PUT /api/memories/{id}", memoryH.Update)
	mux.HandleFunc("DELETE /api/memories/{id}", memoryH.Delete)

	// Knowledge
	mux.HandleFunc("GET /api/knowledge", knowledgeH.List)
	mux.HandleFunc("GET /api/knowledge/search", knowledgeH.Search)
	mux.HandleFunc("POST /api/knowledge", knowledgeH.Create)
	mux.HandleFunc("GET /api/knowledge/{id}", knowledgeH.Get)
	mux.HandleFunc("PUT /api/knowledge/{id}", knowledgeH.Update)
	mux.HandleFunc("DELETE /api/knowledge/{id}", knowledgeH.Delete)
	mux.HandleFunc("GET /api/knowledge/review/list", knowledgeH.ListForReview)
	mux.HandleFunc("POST /api/knowledge/review/batch", knowledgeH.BatchReview)

	// Apply middleware. Auth sits inside Logger/CORS so unauthenticated
	// requests are still logged and CORS headers are present on 401s.
	// (CORS is outermost so preflight OPTIONS never reaches Auth.)
	//
	// Two mux separation: apiMux is auth-gated (every /api/... route needs
	// a session); spaMux serves the embedded frontend at / and is PUBLIC so
	// the /login page can render before the user signs in. A top-level
	// dispatcher routes by URL prefix.
	apiHandler := middleware.CORS(middleware.Logger(middleware.Auth(aclSvc)(mux)))

	// Register the embedded SPA (frontend/dist via //go:embed) on a separate
	// public mux. Must run AFTER every precise /api/... route is registered
	// on `mux` so the catch-all only fires for non-API paths on the SPA mux.
	// No-ops when no frontend was embedded (backend-only mode via
	// NOVA_SKIP_FRONTEND=1).
	spaMux := http.NewServeMux()
	web.Register(spaMux)

	rootHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			apiHandler.ServeHTTP(w, r)
			return
		}
		spaMux.ServeHTTP(w, r)
	})

	port := os.Getenv("NOVA_PORT")
	if port == "" {
		port = "9527"
	}

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      rootHandler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // disabled — background jobs and SSE handlers manage their own timeouts
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	log.Printf("Server listening on http://localhost:%s", port)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

// runMigration implements the `-migrate` CLI command: copy all data from a
// SQLite file into the configured target database, print a per-table report,
// and exit.
func runMigration(from string) {
	cfg := db.LoadConfig()
	if db.Dialect(cfg.Driver) == db.SQLite || cfg.Driver == "" {
		log.Fatal("-migrate requires a MySQL/PostgreSQL target: set NOVA_DB_DRIVER+NOVA_DB_DSN or save a config in the settings UI first")
	}

	src, err := db.OpenSQLite(from)
	if err != nil {
		log.Fatalf("open source sqlite %s: %v", from, err)
	}
	defer src.Close()

	dst, err := db.Init(cfg) // opens + creates schema on the target
	if err != nil {
		log.Fatalf("connect target: %v", err)
	}
	defer dst.Close()

	log.Printf("Migrating %s → %s ...", from, cfg.Driver)
	stats, err := db.Migrate(src, dst, func(msg string) { log.Printf("[migrate] %s", msg) })
	if err != nil {
		log.Fatalf("migration failed: %v", err)
	}
	inserted, skipped := 0, 0
	for _, st := range stats {
		inserted += st.Inserted
		skipped += st.Skipped
	}
	log.Printf("Migration complete: %d rows inserted, %d skipped across %d tables. Restart the server to switch to %s.",
		inserted, skipped, len(stats), cfg.Driver)
}
