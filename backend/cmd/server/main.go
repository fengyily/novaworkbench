package main

import (
	"context"
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
	log.Println("NovaWorkbench Backend starting...")

	// Database
	database, err := db.Init("~/.novaworkbench/data/nova.db")
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer database.Close()

	// Services
	projectSvc := service.NewProjectService(database)
	memorySvc := service.NewMemoryService(database)
	knowledgeSvc := service.NewKnowledgeService(database)
	scannerSvc := service.NewScannerService(database)
	reqSvc := service.NewRequirementService(database)
	platformSvc := service.NewPlatformTokenService(database)
	roleSvc := service.NewRoleService(database)
	settingSvc := service.NewSettingService(database)
	reportSvc := service.NewReportService(database)

	// Seed built-in roles on first run (idempotent).
	if err := roleSvc.SeedDefaults(); err != nil {
		log.Printf("[main] role seed: %v", err)
	}

	// Shared LLM gateway (wraps the claude CLI) — used by the requirement and wizard handlers.
	// The gateway pulls ANTHROPIC_AUTH_TOKEN / ANTHROPIC_BASE_URL from the settings table
	// (via settingSvc, an llm.EnvProvider) and injects them into every claude subprocess.
	llmGateway := llm.New(settingSvc)

	// Handlers
	projectH := handler.NewProjectHandler(projectSvc)
	healthH := handler.NewHealthHandler()
	dashboardH := handler.NewDashboardHandler(projectSvc)
	fsH := handler.NewFsHandler()
	memoryH := handler.NewMemoryHandler(memorySvc)
	knowledgeH := handler.NewKnowledgeHandler(knowledgeSvc)
	scannerH := handler.NewScannerHandler(scannerSvc)
	reqH := handler.NewRequirementHandler(reqSvc, llmGateway)
	sharedJobs := store.NewJobStore(50)
	wizardH := handler.NewWizardHandler(projectSvc, reqSvc, llmGateway, sharedJobs, roleSvc)
	runnerH := handler.NewRunnerHandler(projectSvc, sharedJobs, database)
	reviewH := handler.NewReviewHandler(projectSvc, platformSvc, llmGateway, sharedJobs)
	reportH := handler.NewReportHandler(projectSvc, reportSvc, llmGateway, sharedJobs)
	platformH := handler.NewPlatformHandler(platformSvc)
	roleH := handler.NewRoleHandler(roleSvc)
	settingH := handler.NewSettingHandler(settingSvc)

	// Router
	mux := http.NewServeMux()

	// Health
	mux.HandleFunc("GET /api/health", healthH.Health)

	// Dashboard
	mux.HandleFunc("GET /api/dashboard", dashboardH.Dashboard)

	// Projects
	mux.HandleFunc("GET /api/projects", projectH.List)
	mux.HandleFunc("POST /api/projects", projectH.Add)
	mux.HandleFunc("GET /api/projects/{id}", projectH.Get)
	mux.HandleFunc("DELETE /api/projects/{id}", projectH.Remove)
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

	// Claude CLI configuration (settings) — auth token + base URL, injected as
	// env vars into every claude subprocess.
	mux.HandleFunc("GET /api/settings/claude", settingH.GetClaude)
	mux.HandleFunc("PUT /api/settings/claude", settingH.UpdateClaude)

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

	// Wizard (requirement refinement + coding via Claude CLI)
	// Three-role stage-gate: analyst → architect → developer.
	mux.HandleFunc("POST /api/wizard/analyst-chat", wizardH.AnalystChat)
	mux.HandleFunc("POST /api/wizard/analyst-complete", wizardH.AnalystComplete)
	mux.HandleFunc("POST /api/wizard/analyst-analyze", wizardH.AnalystAnalyze)
	mux.HandleFunc("POST /api/wizard/architect-design", wizardH.ArchitectDesign)
	mux.HandleFunc("POST /api/wizard/start-coding", wizardH.StartCoding)
	mux.HandleFunc("GET /api/wizard/jobs/{id}", wizardH.GetJob)
	mux.HandleFunc("GET /api/wizard/jobs/{id}/stream", wizardH.StreamJob)
	mux.HandleFunc("POST /api/wizard/refine-doc", wizardH.RefineDoc)
	mux.HandleFunc("POST /api/wizard/apply-doc", wizardH.ApplyDoc)

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
