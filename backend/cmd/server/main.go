package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/handler"
	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/middleware"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
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

	// Services
	projectSvc := service.NewProjectService(database)
	memorySvc := service.NewMemoryService(database)
	knowledgeSvc := service.NewKnowledgeService(database)
	reqSvc := service.NewRequirementService(database)
	platformSvc := service.NewPlatformTokenService(database)
	roleSvc := service.NewRoleService(database)
	settingSvc := service.NewSettingService(database)
	reportSvc := service.NewReportService(database)
	jobLogSvc := service.NewJobLogService(database)

	// Seed built-in roles on first run (idempotent).
	if err := roleSvc.SeedDefaults(); err != nil {
		log.Printf("[main] role seed: %v", err)
	}

	// Claude CLI configurations (multi-config). Constructed before the gateway
	// so it can serve as the ClaudeEnvProvider. MigrateLegacy is one-way +
	// idempotent: it seeds a single active "默认配置" from the legacy settings
	// keys the first time the new table is empty.
	claudeCfgSvc := service.NewClaudeConfigService(database)
	if err := claudeCfgSvc.MigrateLegacy(); err != nil {
		log.Printf("[main] claude config legacy migrate: %v", err)
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
	healthH := handler.NewHealthHandler()
	dashboardH := handler.NewDashboardHandler(projectSvc)
	fsH := handler.NewFsHandler()
	memoryH := handler.NewMemoryHandler(memorySvc)
	knowledgeH := handler.NewKnowledgeHandler(knowledgeSvc)
	scannerH := handler.NewScannerHandler(scannerSvc)
	sharedJobs := store.NewJobStore(50)
	reqH := handler.NewRequirementHandler(reqSvc, llmGateway, sharedJobs)
	wizardH := handler.NewWizardHandler(projectSvc, reqSvc, llmGateway, sharedJobs, roleSvc, jobLogSvc, claudeCfgSvc)
	runnerH := handler.NewRunnerHandler(projectSvc, sharedJobs, database)
	reviewH := handler.NewReviewHandler(projectSvc, platformSvc, roleSvc, llmGateway, sharedJobs, jobLogSvc, claudeCfgSvc)
	reportH := handler.NewReportHandler(projectSvc, reportSvc, llmGateway, sharedJobs)
	mergeH := handler.NewMergeHandler(projectSvc, reqSvc, llmGateway, sharedJobs, roleSvc, platformSvc)
	platformH := handler.NewPlatformHandler(platformSvc)
	roleH := handler.NewRoleHandler(roleSvc, claudeCfgSvc)
	settingH := handler.NewSettingHandler(settingSvc)
	claudeCfgH := handler.NewClaudeConfigHandler(claudeCfgSvc)
	databaseH := handler.NewDatabaseHandler(database, cfg)

	// Router
	mux := http.NewServeMux()

	// Health
	mux.HandleFunc("GET /api/health", healthH.Health)

	// Dashboard
	mux.HandleFunc("GET /api/dashboard", dashboardH.Dashboard)

	// Projects
	mux.HandleFunc("GET /api/projects", projectH.List)
	mux.HandleFunc("POST /api/projects", projectH.Add)
	mux.HandleFunc("GET /api/projects/trash", projectH.Trash)
	mux.HandleFunc("GET /api/projects/{id}", projectH.Get)
	mux.HandleFunc("DELETE /api/projects/{id}", projectH.Remove)
	mux.HandleFunc("POST /api/projects/{id}/restore", projectH.Restore)
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
	mux.HandleFunc("DELETE /api/settings/tokens/{id}", platformH.Delete)

	// Roles (settings) — per-role system prompt + model, drives claude CLI flags
	mux.HandleFunc("GET /api/settings/roles", roleH.List)
	mux.HandleFunc("GET /api/settings/roles/{id}", roleH.Get)
	mux.HandleFunc("PUT /api/settings/roles/{id}", roleH.Update)
	mux.HandleFunc("POST /api/settings/roles/{id}/reset", roleH.Reset)

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
	mux.HandleFunc("DELETE /api/requirements/{id}", reqH.Delete)
	mux.HandleFunc("GET /api/requirements/{id}/chat-history", reqH.GetChatHistory)
	mux.HandleFunc("PUT /api/requirements/{id}/chat-history", reqH.SaveChatHistory)
	mux.HandleFunc("DELETE /api/requirements/{id}/analysis-session", reqH.ClearAnalysisSession)
	mux.HandleFunc("POST /api/requirements/{id}/archive", reqH.Archive)
	mux.HandleFunc("POST /api/requirements/{id}/unarchive", reqH.Unarchive)

	// Wizard (requirement refinement + coding via Claude CLI)
	// Three-role stage-gate: analyst → architect → developer.
	mux.HandleFunc("POST /api/wizard/analyst-chat", wizardH.AnalystChat)
	mux.HandleFunc("POST /api/wizard/developer-chat", wizardH.DeveloperChat)
	mux.HandleFunc("POST /api/wizard/architect-design", wizardH.ArchitectDesign)
	mux.HandleFunc("POST /api/wizard/start-coding", wizardH.StartCoding)
	mux.HandleFunc("POST /api/wizard/adjust-coding", wizardH.AdjustCoding)
	mux.HandleFunc("GET /api/wizard/jobs/{id}", wizardH.GetJob)
	mux.HandleFunc("GET /api/wizard/jobs/{id}/stream", wizardH.StreamJob)
	mux.HandleFunc("POST /api/wizard/refine-doc", wizardH.RefineDoc)
	mux.HandleFunc("POST /api/wizard/apply-doc", wizardH.ApplyDoc)

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

	// Apply middleware
	handler := middleware.CORS(middleware.Logger(mux))

	port := os.Getenv("NOVA_PORT")
	if port == "" {
		port = "9527"
	}

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      handler,
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
