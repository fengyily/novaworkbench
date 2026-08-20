package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

type WizardHandler struct {
	projectSvc   *service.ProjectService
	reqSvc       *service.RequirementService
	knowledgeSvc *service.KnowledgeService
	llm          *llm.Gateway
	jobs         *store.JobStore
	roleSvc      *service.RoleService
	jobLogSvc    *service.JobLogService
	claudeCfg    *service.ClaudeConfigService
	usageSvc     usageRecorder
}

func NewWizardHandler(projectSvc *service.ProjectService, reqSvc *service.RequirementService, knowledgeSvc *service.KnowledgeService, llmGateway *llm.Gateway, jobs *store.JobStore, roleSvc *service.RoleService, jobLogSvc *service.JobLogService, claudeCfg *service.ClaudeConfigService, usageSvc usageRecorder) *WizardHandler {
	return &WizardHandler{
		projectSvc:   projectSvc,
		reqSvc:       reqSvc,
		knowledgeSvc: knowledgeSvc,
		llm:          llmGateway,
		jobs:         jobs,
		roleSvc:      roleSvc,
		jobLogSvc:    jobLogSvc,
		claudeCfg:    claudeCfg,
		usageSvc:     usageSvc,
	}
}

// buildKnowledgeBlock loads the project knowledge most relevant to a
// requirement and renders it as a prompt section (## 项目知识库). It returns an
// empty block when nothing relevant exists (the caller then leaves the prompt
// unchanged). Per-entry content is capped at ~8KB and the whole block at ~60KB
// so a large knowledge base can't blow up the prompt budget. The returned
// titles drive the SSE "knowledge" event so the UI can show what was read.
func (h *WizardHandler) buildKnowledgeBlock(projectID, requirementTitle string) (block string, titles []string) {
	if h.knowledgeSvc == nil {
		return "", nil
	}
	items, err := h.knowledgeSvc.ListForRequirement(projectID, requirementTitle, 20)
	if err != nil || len(items) == 0 {
		if err != nil {
			log.Printf("[wizard] load knowledge %s: %v", projectID, err)
		}
		return "", nil
	}
	const (
		perItem  = 8 * 1024
		maxBlock = 60 * 1024
	)
	var b strings.Builder
	b.WriteString("## 项目知识库\n\n以下是与本需求相关的项目知识库内容，请先阅读再进行分析：\n\n")
	titles = make([]string, 0, len(items))
	total := 0
	omitted := 0
	for _, k := range items {
		content := k.Content
		if len(content) > perItem {
			content = content[:perItem] + "\n…（已截断）"
		}
		if total+len(content) > maxBlock {
			omitted++
			continue
		}
		b.WriteString(fmt.Sprintf("### %s\n%s\n\n", k.Title, content))
		titles = append(titles, k.Title)
		total += len(content)
	}
	if omitted > 0 {
		b.WriteString(fmt.Sprintf("…（知识库内容较多，略去 %d 条）\n", omitted))
	}
	block = b.String()
	if block == "" {
		block = "(项目知识库为空)"
	}
	return block, titles
}

// emitKnowledgeEvent writes the wizard's SSE "knowledge" event into a JobStore
// job so subscribers can render the entries read before the stage started. The
// event carries only titles (full content goes into the claude prompt, not the
// SSE stream). Emit it BEFORE invoking claude, once per job.
func emitKnowledgeEvent(job *store.Job, titles []string) {
	items := make([]map[string]string, 0, len(titles))
	for _, t := range titles {
		items = append(items, map[string]string{"title": t})
	}
	data, _ := json.Marshal(map[string]interface{}{
		"type":  "knowledge",
		"count": len(titles),
		"items": items,
	})
	job.Append(store.LogLine{Type: "knowledge", Content: string(data)})
}

// inputToolPath extracts the file path / search pattern a tool_use input
// targets, for the "was the injected knowledge actually used?" evaluation.
// Read/Write/Edit carry file_path; Grep/Glob a pattern. Bash is skipped (its
// command is too noisy). Empty string means "nothing path-like".
func inputToolPath(name string, input map[string]interface{}) string {
	if input == nil {
		return ""
	}
	switch name {
	case "Read", "Write", "Edit":
		if p, ok := input["file_path"].(string); ok {
			return p
		}
	case "Glob":
		if p, ok := input["pattern"].(string); ok {
			return p
		}
	case "Grep":
		if p, ok := input["pattern"].(string); ok {
			return p
		}
	}
	return ""
}

// knowledgeUseItem is one evaluated knowledge entry: did the run actually
// touch the file the entry describes, or mention the entry's title in its
// final output?
type knowledgeUseItem struct {
	Title string `json:"title"`
	Used  bool   `json:"used"`
}

// evaluateKnowledgeUse marks whether each read knowledge entry left a trace of
// actual use in the run: (1) a tool call touched a path whose basename matches
// the entry title (e.g. knowledge "CLAUDE.md" and the CLI Read'ed CLAUDE.md),
// or (2) the entry title appears in the final result text. This is a cheap
// signal derived from events already captured — NO extra LLM call. It is
// intentionally conservative: a not-marked entry isn't proof it was useless
// (e.g. Project Structure informs behavior without being re-read or named).
func evaluateKnowledgeUsage(titles []string, toolFiles []string, resultText string) (items []knowledgeUseItem, usedCount int) {
	if len(titles) == 0 {
		return nil, 0
	}
	lowerResult := strings.ToLower(resultText)
	tools := make([]string, 0, len(toolFiles))
	for _, f := range toolFiles {
		tools = append(tools, strings.ToLower(filepath.ToSlash(f)))
	}
	items = make([]knowledgeUseItem, 0, len(titles))
	for _, t := range titles {
		normTitle := strings.TrimSpace(strings.TrimSuffix(t, "/"))
		used := false
		if normTitle == "" {
			items = append(items, knowledgeUseItem{Title: t, Used: false})
			continue
		}
		lower := strings.ToLower(normTitle)
		for _, tf := range tools {
			base := tf
			if i := strings.LastIndexByte(tf, '/'); i >= 0 {
				base = tf[i+1:]
			}
			// Basename equality, or a direct basename containment (covers
			// tool paths like ".../docs/CLAUDE.md" against title "CLAUDE.md").
			if base == lower || (len(lower) >= 3 && strings.Contains(base, lower)) {
				used = true
				break
			}
		}
		if !used && lowerResult != "" && strings.Contains(lowerResult, lower) {
			used = true
		}
		if used {
			usedCount++
		}
		items = append(items, knowledgeUseItem{Title: t, Used: used})
	}
	return items, usedCount
}

// emitKnowledgeResultEvent closes the "读取项目知识库" loop with the evaluation
// of each entry's actual use (tool trace + result-text mentions). Emitted once
// from the stage that performed the read, on the success path, so the UI can
// mark each entry as 已引用 / 未直接引用.
func emitKnowledgeResultEvent(job *store.Job, items []knowledgeUseItem, usedCount int) {
	if len(items) == 0 {
		return
	}
	jsonItems := make([]map[string]interface{}, 0, len(items))
	for _, it := range items {
		jsonItems = append(jsonItems, map[string]interface{}{"title": it.Title, "used": it.Used})
	}
	data, _ := json.Marshal(map[string]interface{}{
		"type":  "knowledge_result",
		"total": len(items),
		"used":  usedCount,
		"items": jsonItems,
	})
	job.Append(store.LogLine{Type: "knowledge_result", Content: string(data)})
}

// DefaultModelLabel is the display + persistence literal used when neither the
// role nor the active claude config specifies a model. It is NOT a real model
// id: cliModelArg translates it to "" (no --model flag) so the CLI falls back
// to its internal default, while the persisted/displayed value still tells the
// user "no specific model was selected for this stage".
const DefaultModelLabel = "默认模型"

// effectiveModelFromValues resolves the effective model from its two sources:
// the role's explicit per-role override (roleModel) and the active claude
// config's default model (configDefaultModel). Precedence: role override >
// config default > "默认模型" literal. Returns a value suitable for BOTH the
// --model CLI flag (via cliModelArg) and DB persistence — they stay in sync so
// the displayed model is always the one we asked the CLI to use.
func effectiveModelFromValues(roleModel, configDefaultModel string) string {
	if roleModel != "" {
		return roleModel
	}
	if configDefaultModel != "" {
		return configDefaultModel
	}
	return DefaultModelLabel
}

// cliModelArg converts an effective model to the --model CLI argument value.
// The "默认模型" sentinel becomes "" (no --model flag → CLI internal default);
// any other value is passed through verbatim. Call this at every StreamOpts.Model
// site so the CLI never receives the display literal as a model id.
func cliModelArg(effModel string) string {
	if effModel == DefaultModelLabel {
		return ""
	}
	return effModel
}

// roleConfig loads a role's system prompt + effective model by key. The
// returned model is the EFFECTIVE model (role override, else active config
// default, else the "默认模型" literal) — pass it through cliModelArg before
// handing it to StreamOpts.Model so the sentinel never reaches the CLI. On
// error it returns empty system prompt + the resolved default model (and logs)
// so a missing/broken role config never blocks the wizard pipeline.
func (h *WizardHandler) roleConfig(key string) (systemPrompt, model string) {
	r, err := h.roleSvc.GetByKey(key)
	if err != nil {
		log.Printf("[wizard] role %q not found, using CLI defaults: %v", key, err)
		return "", h.effectiveModel("")
	}
	return r.SystemPrompt, h.effectiveModel(r.Model)
}

// effectiveModel resolves the model that will actually be dispatched to the
// claude CLI for a role. roleModel is the role's explicit override (pass the
// already-loaded role.Model to avoid a second DB hit, or "" to look it up by
// key — though roleConfig always loads the role first, so callers normally
// pass r.Model directly). Falls back to the active claude config's default
// model, then to the "默认模型" literal. See effectiveModelFromValues.
func (h *WizardHandler) effectiveModel(roleModel string) string {
	configDefault := ""
	if h.claudeCfg != nil {
		if _, dm, err := h.claudeCfg.ActiveModels(); err == nil {
			configDefault = dm
		}
	}
	return effectiveModelFromValues(roleModel, configDefault)
}

// usageCtxFor builds a usageCtx for one claude invocation. The returned ctx
// records the result event's tokens (best-effort) under the given step.
// projectID may be empty when the requirement couldn't be loaded (legacy
// no-requirement coding path) — the row is still recorded for global counts.
// The active claude config's id + currency are stamped so cost can later be
// recomputed from that platform's current model prices.
func (h *WizardHandler) usageCtxFor(step, requirementID, projectID, jobID, model, meta string) *usageCtx {
	configID, currency := h.activeConfigMeta()
	return &usageCtx{
		Rec:            h.usageSvc,
		RequirementID:  requirementID,
		ProjectID:      projectID,
		JobID:          jobID,
		Step:           step,
		Model:          model,
		ClaudeConfigID: configID,
		Currency:       currency,
		Meta:           meta,
	}
}

// activeConfigMeta returns the currently-active claude config's id + currency.
// Best-effort: empty values when no config is active or on any lookup error.
func (h *WizardHandler) activeConfigMeta() (id, currency string) {
	if h.claudeCfg == nil {
		return "", ""
	}
	c, err := h.claudeCfg.ActiveConfig()
	if err != nil || c == nil {
		return "", ""
	}
	return c.ID, c.Currency
}

// resolveWorkDir returns the directory the current claude stage should run in:
// the requirement's isolated git worktree when it exists (or can be created),
// else the project checkout. Anchoring the WHOLE pipeline (analysis → design →
// coding) to the same worktree means the forked/resumed conversation never
// carries absolute paths back to the shared project checkout — without this the
// coding stage inherits the analyst/architect's original-dir absolute paths and
// edits the original files instead of the worktree.
//
// Non-git projects (and empty paths) return the project checkout with a nil
// error (legacy in-place behavior). A git repo whose worktree can't be created
// returns a non-nil error so the caller fails loudly instead of silently coding
// in-place and poisoning the session chain with original-dir paths.
func (h *WizardHandler) resolveWorkDir(req *model.Requirement, projectPath, defaultBranch string) (string, error) {
	if req == nil || projectPath == "" {
		return projectPath, nil
	}
	// Validate the project path exists before any git operations — a missing
	// directory causes gitRun to fail with a generic error that EnsureWorktree
	// maps to ErrNotAGitRepo, which then returns the non-existent path as the
	// work dir. exec.Cmd.Start() will chdir to it and fail with a misleading
	// ENOENT attributed to the binary rather than the directory.
	if _, err := os.Stat(projectPath); err != nil {
		return "", fmt.Errorf("project directory not found on this host: %s", projectPath)
	}
	if req.WorktreePath != "" {
		if _, err := os.Stat(req.WorktreePath); err == nil {
			return req.WorktreePath, nil
		}
		// Persisted path is gone — fall through to recreate it.
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	branch := "feat/" + req.ID
	wtPath, err := EnsureWorktree(projectPath, req.ID, branch, defaultBranch)
	if err != nil {
		if errors.Is(err, ErrNotAGitRepo) {
			return projectPath, nil // non-git repo → legacy in-place
		}
		return "", err // git repo but worktree add failed → hard error
	}
	if wtPath == "" {
		return projectPath, nil
	}
	if perr := h.reqSvc.UpdateWorktree(req.ID, branch, wtPath); perr != nil {
		log.Printf("[wizard] persist worktree for %s: %v", req.ID, perr)
	}
	return wtPath, nil
}

// requireAnchoredFork guards against forking a source session that was created
// in-place (no persisted worktree_path) on a git repo. Forking such a session
// carries its original-dir absolute paths into the new session, so the stage
// edits the shared checkout instead of the worktree. Non-git projects have no
// worktree by design and pass through.
func (h *WizardHandler) requireAnchoredFork(req *model.Requirement, projectPath string) error {
	if req == nil || req.WorktreePath != "" || projectPath == "" {
		return nil
	}
	if _, err := gitRun(projectPath, "rev-parse", "--is-inside-work-tree"); err != nil {
		return nil // non-git repo → legacy in-place
	}
	return fmt.Errorf("上游会话未在隔离 worktree 中生成，请重新执行「需求分析」或「生成技术方案」后再继续")
}

// docStageSession maps a refine/apply doc_type to the requirement's stored
// session id for that stage and the role key whose persona it runs under.
// Returns sid="" when the stage hasn't been run yet (caller surfaces a hint).
//
//	design → design_session_id   / architect
//	coding → coding_session_id   / developer
func docStageSession(req *model.Requirement, docType string) (sid, roleKey string) {
	switch docType {
	case "design":
		return req.DesignSessionID, "architect"
	case "coding":
		return req.CodingSessionID, "developer"
	}
	return "", "analyst"
}

// AnalystChat starts one analyst-chat turn as a background JobStore job and
// returns the job id immediately (same pattern as architect-design /
// start-coding). Claude runs in a goroutine with context.Background(), so its
// lifetime is decoupled from this HTTP request — a page refresh no longer kills
// the in-flight turn. The active job id is persisted on the requirement
// (analysis_job_id) so a refresh reconnects to the running job via
// GET /api/wizard/jobs/{id}/stream (which replays history first) instead of
// relaunching the turn. The job's log lines carry the analyst's message /
// tool_call / phase events; on job_done the frontend finalizes the turn.
//
// Session threading: the first turn mints a session id (--session-id, persisted
// on the requirement as analysis_session_id); subsequent turns resume it
// (--resume). A stale --resume transparently falls back to a fresh first turn.
func (h *WizardHandler) AnalystChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectPath      string `json:"project_path"`
		RequirementID    string `json:"requirement_id"`
		RequirementTitle string `json:"requirement_title"`
		CurrentAnalysis  string `json:"current_analysis"`
		UserMessage      string `json:"user_message"`
		Model            string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("[analyst-chat] JSON decode error: %v", err)
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	if req.RequirementID == "" {
		writeError(w, 400, "INVALID", "missing requirement_id")
		return
	}

	requirement, err := h.reqSvc.Get(req.RequirementID)
	if err != nil {
		writeError(w, 404, "NOT_FOUND", "requirement not found")
		return
	}

	// Resolve session threading from the DB (source of truth). The request
	// body's title/analysis are only fallbacks for the prompt builder.
	storedSessionID := requirement.AnalysisSessionID
	isFirstRound := storedSessionID == ""
	sessionID := storedSessionID
	if isFirstRound {
		sessionID = util.NewUUID()
		// Persist the freshly minted id BEFORE the claude turn runs so it
		// survives a mid-run backend restart (the session lives on disk; the id
		// must already be in the DB for a later --resume to find it).
		if perr := h.reqSvc.UpdateAnalysisSession(req.RequirementID, sessionID); perr != nil {
			log.Printf("[analyst-chat] failed to persist analysis session for %s: %v", req.RequirementID, perr)
		}
	}
	resumePrompt := req.UserMessage

	systemPrompt, model := h.roleConfig("analyst")
	// Per-request model override (highest precedence); empty means role default.
	if req.Model != "" {
		model = req.Model
	}

	// Create the job, persist its id so a refresh can reconnect, and return the
	// job id immediately. The claude turn runs in a goroutine writing progress
	// into the job store.
	job := h.jobs.Create(req.RequirementID)
	job.SetModel(model)
	if perr := h.reqSvc.UpdateAnalysisJob(req.RequirementID, job.ID); perr != nil {
		log.Printf("[analyst-chat] failed to persist analysis_job_id for %s: %v", req.RequirementID, perr)
	}
	writeJSON(w, 200, map[string]string{"job_id": job.ID})

	projectPath := req.ProjectPath
	defaultBranch := ""
	if proj, perr := h.projectSvc.Get(requirement.ProjectID); perr == nil {
		if projectPath == "" {
			projectPath = proj.LocalPath
		}
		defaultBranch = proj.DefaultBranch
	}

	go func() {
		log.Printf("[analyst-chat] job %s started for %s (resume=%v)", job.ID, req.RequirementID, !isFirstRound)
		defer func() {
			// Clear the active-job pointer on every exit path so a refresh after
			// the turn ends shows the idle chat (ready for the next message)
			// instead of a stale "running" spinner.
			_ = h.reqSvc.UpdateAnalysisJob(req.RequirementID, "")
		}()
		sink := jobSink{job}
		job.Append(store.LogLine{Type: "phase", Content: "🤖 Claude 正在准备分析..."})

		// Anchor the analyst stage to the isolated worktree (created here if
		// missing) so the whole session chain — analysis → design → coding — is
		// rooted in the worktree and never leaks original-dir absolute paths.
		workDir, err := h.resolveWorkDir(requirement, projectPath, defaultBranch)
		if err != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ " + err.Error()})
			job.Finish(1, store.JobError)
			return
		}

		// firstTurnPrompt pre-reads a BOUNDED slice of the project (AI docs +
		// a names-only structure tree) and emits each pre-read file as
		// progress, so the user sees live activity before Claude responds.
		// Only invoked for a first-turn run (genuine first turn, or the
		// stale-resume fallback); resume turns just send the new user message.
		title := req.RequirementTitle
		desc := ""
		analysis := req.CurrentAnalysis
		if title == "" {
			title = requirement.Title
		}
		desc = requirement.Description
		if analysis == "" || analysis == "[]" {
			analysis = requirement.AcceptanceCriteria
		}
		firstTurnPrompt := func() string {
			sink.emit(store.LogLine{Type: "phase", Content: "📖 正在预读项目上下文（不遍历整个仓库）..."})
			docBlock, readFiles, treeSummary := collectProjectContext(workDir, title)
			for _, rf := range readFiles {
				sink.emit(store.LogLine{Type: "tool_call", Content: "📖 预读: " + rf})
			}
			log.Printf("[analyst-chat] pre-read %d files, docBlock=%d bytes, tree=%d bytes, desc=%d bytes",
				len(readFiles), len(docBlock), len(treeSummary), len(desc))
			return buildAnalystFirstPrompt(title, desc, analysis, req.UserMessage, docBlock, treeSummary)
		}

		// context.Background(): the HTTP request has already returned, so we
		// must not tie the claude subprocess's lifetime to r.Context() (which
		// is cancelled the moment the handler returns — that was the bug that
		// killed the turn on page refresh).
		analystUsage := h.usageCtxFor("analyst_chat", req.RequirementID, requirement.ProjectID, job.ID, model, "")
		finalResult, newSessionID, err := h.runAnalystTurn(context.Background(), firstTurnPrompt, resumePrompt, workDir, systemPrompt, model, sessionID, !isFirstRound, sink, analystUsage)
		if err != nil {
			log.Printf("[analyst-chat] turn failed: %v", err)
			job.Append(store.LogLine{Type: "error", Content: err.Error()})
			job.Finish(1, store.JobError)
			return
		}

		// Persist the session id that actually landed on disk. On the first
		// turn we always persist (the freshly minted id). On a resume that
		// fell back from a stale id, newSessionID is a fresh id that differs
		// from what was stored — persist it so the next turn resumes the right
		// conversation. A normal successful resume leaves newSessionID ==
		// storedSessionID, so we skip the write.
		if newSessionID != "" && newSessionID != storedSessionID {
			if perr := h.reqSvc.UpdateAnalysisSession(req.RequirementID, newSessionID); perr != nil {
				log.Printf("[analyst-chat] Failed to persist session for %s: %v", req.RequirementID, perr)
			}
		}

		// The authoritative conversation context lives in the resumed claude
		// session; the job's message lines already carry the assistant text,
		// so the frontend reconstructs the turn from them. Emit a terminal
		// result + done line for cosmetics / debugging.
		job.Append(store.LogLine{Type: "result", Content: strings.TrimSpace(finalResult)})
		job.Append(store.LogLine{Type: "done", Content: "✅ 分析完成！"})
		// Record the effective model for this stage on the success path only
		// (a failed run above returns before reaching here, so the last good
		// record is never clobbered).
		if perr := h.reqSvc.UpdateAnalystModel(req.RequirementID, model); perr != nil {
			log.Printf("[analyst-chat] failed to persist analyst_model for %s: %v", req.RequirementID, perr)
		}
		job.Finish(0, store.JobDone)
		log.Printf("[analyst-chat] job %s finished for %s", job.ID, req.RequirementID)
	}()
}

// DeveloperChat runs one "追加调整" conversation turn against the DEVELOPER role
// (not the analyst). It is the post-coding adjustment dialog: the user describes
// a tweak to the already-implemented code, and the developer confirms
// understanding and proposes an approach WITHOUT editing files — the actual
// edits run in a subsequent start-coding re-run triggered by the frontend's
// "确认，开始修改" button. Routing this to the developer (resuming the coding
// session) keeps the adjustment a DEVELOPMENT change; the old wiring posted the
// same panel to analyst-chat, which refined the requirement instead.
//
// Session threading mirrors StartCoding: resume coding_session_id (the 追加调整
// panel only renders after a first coding pass, so it is normally set);
// otherwise fork off the design/analysis session so the developer inherits the
// requirement+design context. A stale --resume falls back to a fresh session.
func (h *WizardHandler) DeveloperChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectPath      string `json:"project_path"`
		RequirementID    string `json:"requirement_id"`
		RequirementTitle string `json:"requirement_title"`
		UserMessage      string `json:"user_message"`
		Model            string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("[developer-chat] JSON decode error: %v", err)
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	rc.Flush()

	log.Printf("[developer-chat] Starting for requirement: %s in %s", req.RequirementTitle, req.ProjectPath)

	// Resolve the source conversation: prefer the coding session (the 追加调整
	// panel only shows after a first coding pass, so this is normally set);
	// otherwise fork off the design/analysis session so the developer still
	// inherits the requirement+design context. Same resolution as StartCoding.
	var requirement *model.Requirement
	if req.RequirementID != "" {
		if existing, err := h.reqSvc.Get(req.RequirementID); err == nil {
			requirement = existing
		}
	}
	sourceSID := ""
	fork := false
	if requirement != nil {
		if requirement.CodingSessionID != "" {
			sourceSID = requirement.CodingSessionID
		} else if requirement.DesignSessionID != "" {
			sourceSID = requirement.DesignSessionID
			fork = true
		} else if requirement.AnalysisSessionID != "" {
			sourceSID = requirement.AnalysisSessionID
			fork = true
		}
	}

	// Pre-mint + persist the coding session id BEFORE spawning claude for the
	// fork and fresh cases (a plain resume reuses coding_session_id). The forked
	// id is pre-assigned via --session-id so it's known and persisted up front.
	newSID := ""
	if fork || sourceSID == "" {
		newSID = util.NewUUID()
		if req.RequirementID != "" {
			if perr := h.reqSvc.UpdateCodingSession(req.RequirementID, newSID); perr != nil {
				log.Printf("[developer-chat] failed to persist coding session for %s: %v", req.RequirementID, perr)
			}
		}
	}

	sendStatus(w, rc, "phase", "🤖 开发者正在理解追加调整...")
	rc.Flush()

	// Anchor to the worktree so the resumed coding session's conversation stays
	// rooted in the isolated dir (this chat is read-only, but keeps cwd consistent).
	projectPath := req.ProjectPath
	defaultBranch := ""
	if requirement != nil {
		if proj, perr := h.projectSvc.Get(requirement.ProjectID); perr == nil {
			if projectPath == "" {
				projectPath = proj.LocalPath
			}
			defaultBranch = proj.DefaultBranch
		}
	}
	if fork {
		if gerr := h.requireAnchoredFork(requirement, projectPath); gerr != nil {
			sendStatus(w, rc, "error", gerr.Error())
			fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
			rc.Flush()
			return
		}
	}
	workDir, err := h.resolveWorkDir(requirement, projectPath, defaultBranch)
	if err != nil {
		sendStatus(w, rc, "error", err.Error())
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	systemPrompt, model := h.roleConfig("developer")
	// Per-request model override (highest precedence); empty means role default.
	if req.Model != "" {
		model = req.Model
	}

	// The resumed coding conversation already carries the requirement, analysis,
	// and design, so a resume turn only sends the framed adjustment message. The
	// firstTurnPrompt (used on the no-session / stale-fallback path) folds in the
	// title + design so the developer has context without a pre-read pass.
	title := req.RequirementTitle
	var designMarkdown string
	if requirement != nil {
		if title == "" {
			title = requirement.Title
		}
		designMarkdown = requirement.DesignDocs
	}
	firstTurnPrompt := func() string {
		var b strings.Builder
		b.WriteString("现在以「开发者」角色处理用户的追加调整。请先阅读相关代码、确认你对调整意图的理解、给出实现思路与可能的影响；")
		b.WriteString("**暂不要修改任何文件**——等用户在后续步骤确认后，再由开发任务执行修改。\n\n")
		if title != "" {
			b.WriteString(fmt.Sprintf("需求：%s\n\n", title))
		}
		if strings.TrimSpace(designMarkdown) != "" {
			b.WriteString("技术方案：\n")
			b.WriteString(designMarkdown)
			b.WriteString("\n\n")
		}
		b.WriteString("追加调整：\n")
		b.WriteString(req.UserMessage)
		return b.String()
	}
	resumePrompt := fmt.Sprintf(
		"以下是对已实现代码的追加调整。请以「开发者」角色先阅读相关代码、确认你对调整意图的理解、"+
			"给出实现思路与可能的影响；**暂不要修改任何文件**——等用户确认后，再由开发任务执行修改。\n\n追加调整：\n%s",
		req.UserMessage,
	)

	developerProjectID := ""
	if requirement != nil {
		developerProjectID = requirement.ProjectID
	}
	developerUsage := h.usageCtxFor("developer_chat", req.RequirementID, developerProjectID, "", model, "")
	finalResult, newSessionID, err := h.runDeveloperTurn(r.Context(), firstTurnPrompt, resumePrompt, workDir, systemPrompt, model, sourceSID, fork, newSID, w, rc, developerUsage)
	if err != nil {
		log.Printf("[developer-chat] turn failed: %v", err)
		sendStatus(w, rc, "error", err.Error())
		fmt.Fprintf(w, "data: {\"type\":\"done\"}\n\n")
		rc.Flush()
		return
	}

	// The coding session id is already persisted upfront. Correct it only if the
	// run produced a DIFFERENT id than we pre-minted — either a stale-fallback
	// fresh session (runDeveloperTurn minted a fresh id) or the CLI reporting an
	// id other than our --session-id override (a safety net).
	if req.RequirementID != "" && newSessionID != "" && newSessionID != sourceSID && newSessionID != newSID {
		if perr := h.reqSvc.UpdateCodingSession(req.RequirementID, newSessionID); perr != nil {
			log.Printf("[developer-chat] Failed to persist coding session for %s: %v", req.RequirementID, perr)
		}
	}

	// Build a lightweight local-history string for the frontend's chat display.
	// The authoritative conversation context lives in the resumed claude session,
	// so this is just for client-side rendering.
	var historyParts []string
	if req.UserMessage != "" {
		historyParts = append(historyParts, "User: "+req.UserMessage)
	}
	historyParts = append(historyParts, "AI: "+strings.TrimSpace(finalResult))
	updatedHistory := strings.Join(historyParts, "\n")

	doneData, _ := json.Marshal(map[string]interface{}{"type": "done", "history": updatedHistory, "model": model})
	fmt.Fprintf(w, "data: %s\n\n", string(doneData))
	rc.Flush()
}

// toolCallLabel returns a human-readable Chinese label for a tool call event.
func toolCallLabel(toolName string, input map[string]interface{}) string {
	switch toolName {
	case "Read":
		if path, ok := input["file_path"].(string); ok {
			return "📖 读取文件: " + path
		}
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			short := cmd
			if len(short) > 60 {
				short = short[:60] + "..."
			}
			return "⚡ 执行命令: " + short
		}
	case "Glob":
		if pattern, ok := input["pattern"].(string); ok {
			return "🔍 搜索文件: " + pattern
		}
	case "Grep":
		if pattern, ok := input["pattern"].(string); ok {
			return "🔍 搜索内容: " + pattern
		}
	case "Write":
		if path, ok := input["file_path"].(string); ok {
			return "✏️ 写入文件: " + path
		}
	case "Edit":
		if path, ok := input["file_path"].(string); ok {
			return "✏️ 编辑文件: " + path
		}
	}
	return "🔧 " + toolName
}

// collectProjectContext builds a BOUNDED context for the analyst first turn so
// Claude doesn't have to blindly wander a large project tree (the opennhp
// checkout is 3.4G / ~1000 files). The walk stats files but only reads a handful
// of small text files. It returns:
//   - docBlock: full content of AI-config docs (CLAUDE.md/AGENTS.md/.cursorrules/
//     README*) plus keyword-matched source files, capped at ~40KB total.
//   - readFiles: the relative paths whose content was injected (for SSE progress).
//   - treeSummary: a names-only directory listing (depth- and count-capped) so
//     Claude knows the layout without Glob'ing multi-GB of files.
func collectProjectContext(projectPath, requirementTitle string) (docBlock string, readFiles []string, treeSummary string) {
	const (
		maxDocBytes    = 40 * 1024
		maxTreeEntries = 240
		maxTreeDepth   = 3
	)

	docCandidates := []string{"CLAUDE.md", "AGENTS.md", ".cursorrules", "README.md", "README"}

	// Keywords from the title (lowercased) for source-file matching. Chinese
	// titles rarely match English paths, so docs + tree carry most of the weight;
	// keyword matching is a bonus when it hits.
	titleLower := strings.ToLower(requirementTitle)
	keywords := strings.FieldsFunc(titleLower, func(r rune) bool {
		return r == ' ' || r == '-' || r == '_' || r == '/' || r == ',' || r == '：' || r == ':' || r == '、'
	})
	filtered := keywords[:0]
	for _, kw := range keywords {
		if len(kw) >= 3 {
			filtered = append(filtered, kw)
		}
	}
	keywords = filtered

	skipExts := map[string]bool{
		".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true, ".svg": true,
		".woff": true, ".woff2": true, ".ttf": true, ".eot": true,
		".zip": true, ".tar": true, ".gz": true, ".sum": true, ".lock": true,
		".bin": true, ".exe": true, ".db": true, ".so": true, ".dll": true, ".dylib": true,
		".mp4": true, ".mp3": true, ".pdf": true, ".dat": true, ".pdb": true,
	}

	type fileEntry struct {
		path       string
		relPath    string
		isPriority bool
	}
	var toRead []fileEntry
	seen := map[string]bool{}
	var treePaths []string

	_ = filepath.Walk(projectPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(projectPath, path)
		// Skip hidden / generated / dependency dirs entirely.
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if part == "." {
				continue
			}
			if strings.HasPrefix(part, ".") || part == "vendor" || part == "node_modules" ||
				part == "dist" || part == "build" || part == "__pycache__" || part == "target" {
				return nil
			}
		}

		// Tree: collect names only (dirs and files), depth- and count-limited.
		if len(treePaths) < maxTreeEntries {
			depth := strings.Count(rel, string(filepath.Separator)) + 1
			if depth <= maxTreeDepth {
				if info.IsDir() {
					treePaths = append(treePaths, rel+"/")
				} else {
					treePaths = append(treePaths, rel)
				}
			}
		}

		if info.IsDir() {
			return nil
		}

		// Skip binary / large files from CONTENT injection (still listed in tree).
		ext := strings.ToLower(filepath.Ext(info.Name()))
		if skipExts[ext] || info.Size() > 200*1024 {
			return nil
		}

		nameLower := strings.ToLower(info.Name())
		relLower := strings.ToLower(rel)

		for _, doc := range docCandidates {
			// Only read AI-config / readme docs at the PROJECT ROOT. Nested
			// README.md files (docker/, endpoints/.../etc) are noise — they
			// bloat the prompt and push the model to read more. The tree still
			// lists them by name, so the model knows they exist.
			if nameLower == strings.ToLower(doc) && !strings.Contains(rel, string(filepath.Separator)) && !seen[rel] {
				seen[rel] = true
				toRead = append(toRead, fileEntry{path, rel, true})
				return nil
			}
		}
		for _, kw := range keywords {
			if strings.Contains(relLower, kw) && !seen[rel] {
				seen[rel] = true
				toRead = append(toRead, fileEntry{path, rel, false})
				return nil
			}
		}
		return nil
	})

	// Doc + keyword content (priority first), capped.
	var buf strings.Builder
	total := 0
	readOne := func(f fileEntry) {
		data, rerr := os.ReadFile(f.path)
		if rerr != nil {
			return
		}
		content := string(data)
		if total+len(content) > maxDocBytes {
			content = content[:maxDocBytes-total]
		}
		buf.WriteString(fmt.Sprintf("### %s\n```\n%s\n```\n\n", f.relPath, content))
		readFiles = append(readFiles, f.relPath)
		total += len(content)
	}
	for _, f := range toRead {
		if f.isPriority && total < maxDocBytes {
			readOne(f)
		}
	}
	for _, f := range toRead {
		if !f.isPriority && total < maxDocBytes {
			readOne(f)
		}
	}

	tree := "## 项目结构（仅名称）\n"
	if len(treePaths) == 0 {
		tree += "(未能列出项目结构)"
	} else {
		tree += strings.Join(treePaths, "\n")
		if len(treePaths) >= maxTreeEntries {
			tree += "\n…（已截断；如需更多请直接读取具体路径）"
		}
	}

	docBlock = buf.String()
	treeSummary = tree
	if docBlock == "" {
		docBlock = "(未预读到关键文档，请基于下方结构概览，按需读取具体文件)"
	}
	return
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// StartCoding creates a background job and immediately returns its ID.
// The claude CLI runs in a goroutine; progress is parsed by runClaudeStream
// (stream_event increments + init phase + tool_call labels) and written to the
// job store, so the coding panel shows live progress instead of a frozen blank
// until the turn's batched assistant event. Subscribe via
// GET /api/wizard/jobs/{id}/stream; snapshot via GET /api/wizard/jobs/{id}.
func (h *WizardHandler) StartCoding(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectPath      string `json:"project_path"`
		RequirementTitle string `json:"requirement_title"`
		RequirementDesc  string `json:"requirement_desc"`
		RequirementID    string `json:"requirement_id"`
		BranchName       string `json:"branch_name"`
		BaseBranch       string `json:"base_branch"`
		Model            string `json:"model"`
		ReadKnowledge    bool   `json:"read_knowledge"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}

	// Optional "读取项目知识库" switch: when true, the project's knowledge
	// relevant to this requirement is injected into the claude prompt and a
	// "knowledge" SSE event is emitted before the coding job starts.
	readKnowledge := req.ReadKnowledge

	job := h.jobs.Create(req.RequirementID)
	writeJSON(w, 200, map[string]string{"job_id": job.ID})

	go func() {
		defer func() {
			// Persist the finished job's full log so a backend restart doesn't
			// wipe the development record. All exit paths above call job.Finish,
			// so by the time this defer runs the snapshot is terminal. The
			// effective model is read back from job.Model (set by SetModel
			// below once roleConfig resolves it) so the defer doesn't capture a
			// `model` local that would shadow the model package.
			lines, status, exitCode := job.Snapshot()
			if perr := h.jobLogSvc.Save(job.ID, req.RequirementID, string(status), exitCode, job.StartedAt, job.FinishedAt, lines, job.Model); perr != nil {
				log.Printf("[start-coding] failed to persist job log %s: %v", job.ID, perr)
			}
		}()
		log.Printf("[start-coding] job %s started for %q in %s", job.ID, req.RequirementTitle, req.ProjectPath)

		// Load the requirement row up front so we can (a) detect whether the
		// source session was created in an isolated worktree, and (b) reuse it for
		// the fork resolution below without a second Get.
		var reqRow *model.Requirement
		if req.RequirementID != "" {
			if r, err := h.reqSvc.Get(req.RequirementID); err == nil {
				reqRow = r
			}
		}
		// hadWorktree records whether the upstream stage had already persisted a
		// worktree before THIS coding run — false means the design/analysis session
		// we're about to fork was created in-place (un-isolated).
		hadWorktree := reqRow != nil && reqRow.WorktreePath != ""

		// Resolve the working directory for coding. When a branch is requested
		// AND the project is a git repo with a requirement id to key on, develop
		// in an isolated git worktree per requirement so parallel requirements
		// don't stomp each other's checkout or edit the same files. The legacy
		// path (no branch, non-git project, or no requirement_id) falls back to
		// coding directly in the project directory as before.
		workDir := req.ProjectPath
		branchDir := req.ProjectPath // where git checkout/pull run
		useWorktree := false
		baseBranch := req.BaseBranch
		if baseBranch == "" {
			baseBranch = "main"
		}
		if req.BranchName != "" && req.RequirementID != "" {
			wtPath, wtErr := EnsureWorktree(req.ProjectPath, req.RequirementID, req.BranchName, baseBranch)
			switch {
			case wtErr == nil && wtPath != "":
				workDir = wtPath
				branchDir = wtPath
				useWorktree = true
				job.Append(store.LogLine{Type: "message", Content: "🌿 已创建/复用隔离 worktree: " + wtPath})
			case errors.Is(wtErr, ErrNotAGitRepo):
				// Fall through to the legacy in-place checkout below.
				job.Append(store.LogLine{Type: "message", Content: "ℹ️ 非 git 仓库，在项目目录直接开发"})
			default:
				job.Append(store.LogLine{Type: "error", Content: "❌ 创建 worktree 失败: " + wtErr.Error()})
				job.Finish(1, store.JobError)
				return
			}
		}

		// Checkout the development branch before coding. Skipped for worktrees
		// — `git worktree add` already checked the branch out in the worktree.
		if req.BranchName != "" && !useWorktree {
			gitCmd := exec.Command("git", "checkout", "-b", req.BranchName, baseBranch)
			gitCmd.Dir = branchDir
			out, err := gitCmd.CombinedOutput()
			if err != nil {
				// Branch may already exist, try switching to it
				gitCmd2 := exec.Command("git", "checkout", req.BranchName)
				gitCmd2.Dir = branchDir
				out2, err2 := gitCmd2.CombinedOutput()
				if err2 != nil {
					job.Append(store.LogLine{Type: "error", Content: "❌ git checkout 失败: " + strings.TrimSpace(string(out2))})
					job.Finish(1, store.JobError)
					return
				}
				job.Append(store.LogLine{Type: "message", Content: "🌿 切换到已有分支: " + req.BranchName})
			} else {
				job.Append(store.LogLine{Type: "message", Content: "🌿 " + strings.TrimSpace(string(out))})
			}
		}

		// Best-effort pull: try to update the dev branch from its upstream so
		// coding starts from the remote HEAD. This must NOT abort the coding
		// job — the repo may have no remote at all, or the branch may have no
		// upstream tracking info, in which case there is simply nothing to
		// pull and we proceed on the already-checked-out branch.
		if req.BranchName != "" {
			pullCmd := exec.Command("git", "pull", "--ff-only")
			pullCmd.Dir = branchDir
			pullOut, pullErr := pullCmd.CombinedOutput()
			if pullErr != nil {
				// No upstream on the current branch — retry against origin/<base>
				// if a remote exists. Missing remote / diverged history just means
				// "nothing to pull"; log it and keep going.
				fallbackCmd := exec.Command("git", "pull", "--ff-only", "origin", baseBranch)
				fallbackCmd.Dir = branchDir
				fbOut, fbErr := fallbackCmd.CombinedOutput()
				if fbErr != nil {
					job.Append(store.LogLine{Type: "message", Content: "ℹ️ 跳过 git pull（无远程跟踪或已分叉），继续在当前分支开发: " + strings.TrimSpace(string(append(pullOut, fbOut...)))})
				} else {
					job.Append(store.LogLine{Type: "message", Content: "⬇️ " + strings.TrimSpace(string(fbOut))})
				}
			} else {
				job.Append(store.LogLine{Type: "message", Content: "⬇️ " + strings.TrimSpace(string(pullOut))})
			}
		}

		// Persist the worktree location + branch so adjust-coding and the merge
		// step can find the isolated working tree (jobs are in-memory; the path
		// must live in the DB to survive a restart).
		if useWorktree && req.RequirementID != "" {
			if perr := h.reqSvc.UpdateWorktree(req.RequirementID, req.BranchName, workDir); perr != nil {
				log.Printf("[start-coding] failed to persist worktree for %s: %v", req.RequirementID, perr)
			}
		}

		// Session threading: the developer stage forks off the design session
		// (--resume <design_sid> --fork-session) so the developer inherits the
		// full analysis+design discussion and swaps in the developer persona; the
		// forked session gets a new id we read from the stream and persist as
		// coding_session_id. We do NOT re-feed the requirement desc / design JSON
		// — the resumed conversation already has them.
		//
		// "重新开发"语义: when a coding_session_id already exists (a prior
		// coding pass ran) we STILL fork off the design session instead of
		// --resume'ing the prior coding session. Forking mints a NEW session that
		// inherits only the requirement+design conversation, so leftover tool_use
		// / half-written code / mid-run errors from the last coding pass cannot
		// pollute the new round. The forked id overwrites coding_session_id
		// below (fork && out.sessionID != "" guard), realizing "重新开发 = 新会话".
		// First-ever coding (coding_session_id == "") takes the same path, so
		// behavior is unchanged for the genuine first pass.
		//
		// Graceful fallback: the legacy /wizard quick-start page has no
		// Requirement row (no requirement_id / session ids), so it can't join
		// the session chain. When no source session exists we fall back to a
		// fresh session and feed the full desc — i.e. the pre-chain behavior —
		// so that path keeps working.
		sourceSID := ""
		fork := false
		if reqRow != nil {
			if reqRow.DesignSessionID != "" {
				// 重新开发 / 首次开发: fork from the design session so the new
				// coding session carries only requirement+design, never the prior
				// coding pass's history.
				sourceSID = reqRow.DesignSessionID
				fork = true
			} else if reqRow.AnalysisSessionID != "" {
				// No design session yet but an analyst session exists — keep
				// forking off the analyst session (skip-analysis-first path).
				sourceSID = reqRow.AnalysisSessionID
				fork = true
			} else if reqRow.CodingSessionID != "" && !reqRow.SkipDesign {
				// Legacy fallback: no design / analysis session at all AND not a
				// skip-design ("直接开发") row. Resume the prior coding session
				// rather than losing threading for old data rows that predate
				// session chaining. skip-design rows fall through to the fresh
				// path below so "重新开发" mints a brand-new session instead of
				// resuming the prior one.
				sourceSID = reqRow.CodingSessionID
			}
		}

		// Pre-mint + persist the coding session id BEFORE spawning claude so it
		// survives a mid-run restart. This happens for BOTH a fork off the
		// design/analysis session (--resume <src> --fork-session --session-id
		// <new>) AND a fresh session (--session-id <new>) — the latter covers
		// skip-design "直接开发" (no analysis/design session to fork, but a
		// requirement row exists), so an interrupted direct-development run can
		// still be resumed via 继续开发 instead of redoing everything. Only the
		// legacy quick-start path (no requirement row) keeps a CLI-generated id
		// and never persists it.
		newCodingSID := ""
		if req.RequirementID != "" && (fork || sourceSID == "") {
			newCodingSID = util.NewUUID()
			if perr := h.reqSvc.UpdateCodingSession(req.RequirementID, newCodingSID); perr != nil {
				log.Printf("[start-coding] failed to persist coding session for %s: %v", req.RequirementID, perr)
			}
		}

		// Forking a source session created in-place (no persisted worktree)
		// leaks its original-dir absolute paths into the coding session, so
		// Claude edits the shared checkout instead of the worktree. Guard only
		// when we actually established a worktree this run (useWorktree=true),
		// which also excludes non-git projects (legacy in-place coding).
		if fork && useWorktree && !hadWorktree {
			job.Append(store.LogLine{Type: "error", Content: "❌ 上游会话未在隔离 worktree 中生成，请重新执行「生成技术方案」后再开始开发。"})
			job.Finish(1, store.JobError)
			return
		}

		systemPrompt, model := h.roleConfig("developer")
		// Per-request model override (highest precedence); empty means role default.
		if req.Model != "" {
			model = req.Model
		}
		job.SetModel(model)
		var prompt string
		if sourceSID == "" {
			// Legacy fresh-session path: feed the full title+desc as before.
			prompt = fmt.Sprintf("## %s\n\n%s", req.RequirementTitle, req.RequirementDesc)
			job.Append(store.LogLine{Type: "message", Content: "ℹ️ 未关联需求会话，使用独立会话开始开发。"})
		} else {
			// The resumed conversation carries the requirement, analysis, and
			// design. We only tell Claude to switch to the developer role and
			// implement. A one-line pointer to the title helps orientation.
			// When the user ran a pre-coding "追加调整" chat, its outcome is
			// passed as RequirementDesc — fold it in as an explicit adjustment
			// note so it reaches the developer (the coding session forks off the
			// DESIGN conversation, which doesn't itself contain that chat).
			prompt = fmt.Sprintf("现在切换到「开发者」角色。基于我们刚才的需求分析与技术方案，请实现该需求（%s）。", req.RequirementTitle)
			if strings.TrimSpace(req.RequirementDesc) != "" {
				prompt += "\n\n用户在开发前的追加调整说明：\n" + req.RequirementDesc
			}
		}

		// Optional knowledge pre-read: inject the project knowledge relevant to
		// this requirement and surface what was read via a "knowledge" SSE event.
		// Default off (read_knowledge=false) keeps the legacy behavior untouched.
		var kbReadTitles []string
		if readKnowledge {
			codingProjID := ""
			if reqRow != nil {
				codingProjID = reqRow.ProjectID
			}
			kbBlock, kbTitles := h.buildKnowledgeBlock(codingProjID, req.RequirementTitle)
			if kbBlock != "" {
				prompt = kbBlock + "\n" + prompt
			}
			emitKnowledgeEvent(job, kbTitles)
			kbReadTitles = kbTitles
		}
		sessionArg := sourceSID
		forkSessionID := ""
		if fork {
			forkSessionID = newCodingSID
		} else if sourceSID == "" {
			sessionArg = newCodingSID // fresh (skip-design 直接开发): --session-id <new>
		}
		cmd := h.llm.GenerateCode(llm.StreamOpts{
			Prompt:        prompt,
			WorkDir:       workDir,
			SystemPrompt:  systemPrompt,
			Model:         cliModelArg(model),
			SessionID:     sessionArg,
			Resume:        sourceSID != "",
			Fork:          fork,
			ForkSessionID: forkSessionID,
		})

		// runClaudeStream owns the subprocess lifecycle (Start/Wait) and parses
		// stream-json events into job log lines via jobSink — including the
		// stream_event/content_block_delta increments the hand-written parser
		// used here previously dropped, which made the coding panel look frozen
		// until the turn's batched assistant event arrived. It also surfaces an
		// immediate "🤖 Claude 已连接" phase on the system/init event and a
		// tool_call label on content_block_start, giving live progress.
		codingProjectID := ""
		if reqRow != nil {
			codingProjectID = reqRow.ProjectID
		}
		codingUsage := h.usageCtxFor("coding", req.RequirementID, codingProjectID, job.ID, model, "")
		out := runClaudeStream(jobSink{job}, cmd, "start-coding", codingUsage)

		// The coding session id is already persisted upfront. Correct it only if
		// the CLI reported a different id than the one we pre-minted (a safety
		// net in case the --session-id override semantics ever change).
		if newCodingSID != "" && out.sessionID != "" && out.sessionID != newCodingSID {
			if perr := h.reqSvc.UpdateCodingSession(req.RequirementID, out.sessionID); perr != nil {
				log.Printf("[start-coding] Failed to persist coding session for %s: %v", req.RequirementID, perr)
			}
		}

		if out.staleSession {
			job.Append(store.LogLine{Type: "error", Content: "❌ 源会话已失效，请重新发起对应阶段后再开发。"})
			job.Finish(1, store.JobError)
			return
		}
		if out.errMsg != "" {
			job.Append(store.LogLine{Type: "error", Content: "❌ " + out.errMsg})
			job.Finish(1, store.JobError)
			return
		}
		if out.finalResult == "" {
			job.Append(store.LogLine{Type: "error", Content: "❌ Claude 未返回结果，请重试"})
			job.Finish(1, store.JobError)
			return
		}
		job.Append(store.LogLine{Type: "result", Content: strings.TrimSpace(out.finalResult)})
		// Close the knowledge loop: mark which read entries the run actually used.
		if len(kbReadTitles) > 0 {
			items, used := evaluateKnowledgeUsage(kbReadTitles, out.toolFiles, out.finalResult)
			emitKnowledgeResultEvent(job, items, used)
		}
		job.Append(store.LogLine{Type: "done", Content: "✅ 开发完成！"})
		// Record the effective developer model (success path only).
		if req.RequirementID != "" {
			if perr := h.reqSvc.UpdateDeveloperModel(req.RequirementID, model); perr != nil {
				log.Printf("[start-coding] failed to persist developer_model for %s: %v", req.RequirementID, perr)
			}
		}
		job.Finish(0, store.JobDone)
		log.Printf("[start-coding] job %s finished status=%s exit=%d", job.ID, job.Status, job.ExitCode)
	}()
}

// AdjustCoding starts a background JobStore job that resumes the prior coding
// session (--resume coding_session_id) to apply a follow-up adjustment to
// already-implemented code. Because the resumed session already carries the
// requirement, analysis, design, and the developer persona, we send ONLY the
// user's follow-up message as -p and inject NEITHER the role system prompt NOR
// the readProjectContext project context — re-feeding them would be redundant
// and could distort the resumed conversation. The developer role's current model
// is still honored (--model) so the user's latest model setting applies.
//
// Only requirements with status in {"done","developing"} and a non-empty
// coding_session_id may adjust (developing = first coding pass just finished;
// done = user marked complete). The job_done handler does NOT change status and
// does NOT update coding_session_id — every adjust round resumes the SAME
// original coding session. Stale --resume (session file gone) surfaces a clear
// error instead of silently starting a fresh session.
//
// POST /api/wizard/adjust-coding { requirement_id, message } -> { job_id }
func (h *WizardHandler) AdjustCoding(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequirementID string `json:"requirement_id"`
		Message       string `json:"message"`
		Model         string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		log.Printf("[adjust-coding] JSON decode error: %v", err)
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		writeError(w, 400, "INVALID", "message 不能为空")
		return
	}

	req, err := h.reqSvc.Get(body.RequirementID)
	if err != nil {
		writeError(w, 404, "NOT_FOUND", "requirement not found")
		return
	}
	if req.Status != "done" && req.Status != "developing" {
		writeError(w, 409, "INVALID_STATUS", "仅开发完成或开发中的需求可追加调整（当前状态: "+req.Status+"）")
		return
	}
	if req.CodingSessionID == "" {
		writeError(w, 409, "NO_SESSION", "无 coding session，无法 resume，请重新发起 coding")
		return
	}

	proj, err := h.projectSvc.Get(req.ProjectID)
	if err != nil {
		writeError(w, 404, "NOT_FOUND", "project not found")
		return
	}

	// Only the developer role's MODEL is honored (so the user's latest model
	// setting applies to follow-up turns). The system prompt is deliberately
	// omitted: the resumed coding session already carries the developer
	// persona, and re-injecting --system-prompt would replace it.
	_, model := h.roleConfig("developer")
	// Per-request model override (highest precedence); empty means role default.
	if body.Model != "" {
		model = body.Model
	}

	job := h.jobs.Create(body.RequirementID)
	job.SetModel(model)
	writeJSON(w, 200, map[string]string{"job_id": job.ID})

	go func() {
		defer func() {
			// Persist the finished job's full log so a backend restart doesn't
			// wipe the adjustment record (same durability pattern as StartCoding).
			lines, status, exitCode := job.Snapshot()
			if perr := h.jobLogSvc.Save(job.ID, body.RequirementID, string(status), exitCode, job.StartedAt, job.FinishedAt, lines, model); perr != nil {
				log.Printf("[adjust-coding] failed to persist job log %s: %v", job.ID, perr)
			}
		}()
		log.Printf("[adjust-coding] job %s started for %s (resume %s)", job.ID, body.RequirementID, req.CodingSessionID)
		job.Append(store.LogLine{Type: "phase", Content: "🤖 Claude 正在续接开发会话，处理追加调整..."})

		// GenerateCode wraps StreamCmd with a long (>=30m) timeout — a real
		// code edit can take minutes. SystemPrompt is left empty so the resumed
		// session's persona is preserved; Prompt carries ONLY the user's
		// follow-up message (no readProjectContext project context).
		//
		// WorkDir: prefer the requirement's isolated worktree (so follow-up
		// edits land in the same worktree as the first coding pass, keeping
		// parallel requirements isolated); fall back to the project checkout
		// for legacy requirements without a worktree.
		workDir := proj.LocalPath
		if req.WorktreePath != "" {
			if _, statErr := os.Stat(req.WorktreePath); statErr == nil {
				workDir = req.WorktreePath
			}
		}
		cmd := h.llm.GenerateCode(llm.StreamOpts{
			Prompt:       body.Message,
			WorkDir:      workDir,
			SystemPrompt: "", // resume 已携带 developer persona，不再注入
			Model:        cliModelArg(model),
			SessionID:    req.CodingSessionID,
			Resume:       true,
			Fork:         false,
		})
		adjustUsage := h.usageCtxFor("adjust_coding", body.RequirementID, req.ProjectID, job.ID, model, "")
		out := runClaudeStream(jobSink{job}, cmd, "adjust-coding", adjustUsage)

		// Stale --resume: the coding session file is gone (~/.claude/ cleaned
		// or too old). Surface a clear error rather than silently starting a
		// fresh session — the user must re-run start-coding to mint a new one.
		if out.staleSession {
			job.Append(store.LogLine{Type: "error", Content: "❌ 原 coding 会话已失效（session 文件不存在），请重新发起 coding。"})
			job.Finish(1, store.JobError)
			return
		}
		if out.errMsg != "" {
			job.Append(store.LogLine{Type: "error", Content: "❌ " + out.errMsg})
			job.Finish(1, store.JobError)
			return
		}
		if out.finalResult == "" {
			job.Append(store.LogLine{Type: "error", Content: "❌ Claude 未返回结果，请重试"})
			job.Finish(1, store.JobError)
			return
		}
		// job_done: keep status="done" (no UpdateStatus call) and do NOT update
		// coding_session_id (no UpdateCodingSession call) — every later adjust
		// round resumes the SAME original coding session.
		job.Append(store.LogLine{Type: "result", Content: strings.TrimSpace(out.finalResult)})
		job.Append(store.LogLine{Type: "done", Content: "✅ 追加调整完成！"})
		// Record the effective developer model for this adjust round (success
		// path only — "most recent successful run" semantics).
		if perr := h.reqSvc.UpdateDeveloperModel(body.RequirementID, model); perr != nil {
			log.Printf("[adjust-coding] failed to persist developer_model for %s: %v", body.RequirementID, perr)
		}
		job.Finish(0, store.JobDone)
		log.Printf("[adjust-coding] job %s finished for %s", job.ID, body.RequirementID)
	}()
}

// ContinueCoding resumes an interrupted/cleared coding task by --resume'ing the
// persisted coding session and asking Claude to pick up where it left off. This
// is the recovery path for the "开发任务已完成（日志因服务重启已清空）" state:
// the in-memory job log is gone after a backend restart, but the coding session
// lives on disk (~/.claude/) and coding_session_id is persisted in the DB, so a
// --resume lets Claude continue the work and re-report what was done.
//
// Only requirements in status "developing" with a non-empty coding_session_id
// may continue (developing = the first coding pass was launched; the lost-log
// recovery block renders exactly in this state). Unlike start-coding — which
// FORKS off the design session to realize "重新开发" — continue-coding RESUMES
// the original coding session (Fork:false) and re-feeds NOTHING except a short
// "continue" instruction, since the resumed conversation already carries the
// requirement, design, developer persona, and prior coding progress. The
// job_done handler does NOT change status and does NOT update coding_session_id
// (every continue round resumes the SAME session). A stale --resume surfaces a
// clear error instead of silently starting a fresh session.
//
// POST /api/wizard/continue-coding { requirement_id } -> { job_id }
func (h *WizardHandler) ContinueCoding(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequirementID string `json:"requirement_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		log.Printf("[continue-coding] JSON decode error: %v", err)
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}

	req, err := h.reqSvc.Get(body.RequirementID)
	if err != nil {
		writeError(w, 404, "NOT_FOUND", "requirement not found")
		return
	}
	if req.Status != "developing" {
		writeError(w, 409, "INVALID_STATUS", "仅开发中的需求可继续开发（当前状态: "+req.Status+"）")
		return
	}
	if req.CodingSessionID == "" {
		writeError(w, 409, "NO_SESSION", "无 coding session，无法 resume，请重新发起 coding")
		return
	}

	proj, err := h.projectSvc.Get(req.ProjectID)
	if err != nil {
		writeError(w, 404, "NOT_FOUND", "project not found")
		return
	}

	// Only the developer role's MODEL is honored (so the user's latest model
	// setting applies to the continuation). The system prompt is deliberately
	// omitted: the resumed coding session already carries the developer persona,
	// and re-injecting --system-prompt would replace it (same as AdjustCoding).
	_, model := h.roleConfig("developer")

	job := h.jobs.Create(body.RequirementID)
	job.SetModel(model)
	writeJSON(w, 200, map[string]string{"job_id": job.ID})

	go func() {
		defer func() {
			// Persist the finished job's full log so a backend restart doesn't
			// wipe this continuation record (same durability pattern as
			// StartCoding / AdjustCoding). This is what "fills back" the lost
			// development record after a restart.
			lines, status, exitCode := job.Snapshot()
			if perr := h.jobLogSvc.Save(job.ID, body.RequirementID, string(status), exitCode, job.StartedAt, job.FinishedAt, lines, model); perr != nil {
				log.Printf("[continue-coding] failed to persist job log %s: %v", job.ID, perr)
			}
		}()
		log.Printf("[continue-coding] job %s started for %s (resume %s)", job.ID, body.RequirementID, req.CodingSessionID)
		job.Append(store.LogLine{Type: "phase", Content: "🤖 Claude 正在续接开发会话，继续完成任务..."})

		// The resumed coding session already carries the requirement, design,
		// developer persona, and prior coding progress, so the prompt is only a
		// short "continue" instruction: re-inspect the workdir, finish whatever
		// is incomplete, and report what was done. No project context re-feed.
		workDir := proj.LocalPath
		if req.WorktreePath != "" {
			if _, statErr := os.Stat(req.WorktreePath); statErr == nil {
				workDir = req.WorktreePath
			}
		}
		prompt := "继续完成之前中断的开发任务。请先检查当前代码与工作区状态，" +
			"判断哪些部分已完成、哪些未完成或需要修复；然后基于技术方案继续完成剩余工作、补齐缺失内容。" +
			"最后用中文总结本次完成的内容。"
		cmd := h.llm.GenerateCode(llm.StreamOpts{
			Prompt:       prompt,
			WorkDir:      workDir,
			SystemPrompt: "", // resume 已携带 developer persona，不再注入
			Model:        cliModelArg(model),
			SessionID:    req.CodingSessionID,
			Resume:       true,
			Fork:         false,
		})
		continueUsage := h.usageCtxFor("continue_coding", body.RequirementID, req.ProjectID, job.ID, model, "")
		out := runClaudeStream(jobSink{job}, cmd, "continue-coding", continueUsage)

		// Stale --resume: the coding session file is gone. Surface a clear error
		// rather than silently starting fresh — the user can still 重新开发
		// (which forks off the design session) as a fallback.
		if out.staleSession {
			job.Append(store.LogLine{Type: "error", Content: "❌ 原 coding 会话已失效（session 文件不存在），请重新发起 coding 或使用重新开发。"})
			job.Finish(1, store.JobError)
			return
		}
		if out.errMsg != "" {
			job.Append(store.LogLine{Type: "error", Content: "❌ " + out.errMsg})
			job.Finish(1, store.JobError)
			return
		}
		if out.finalResult == "" {
			job.Append(store.LogLine{Type: "error", Content: "❌ Claude 未返回结果，请重试"})
			job.Finish(1, store.JobError)
			return
		}
		// job_done: keep status="developing" (no UpdateStatus call) and do NOT
		// update coding_session_id (no UpdateCodingSession call) — every continue
		// round resumes the SAME original coding session.
		job.Append(store.LogLine{Type: "result", Content: strings.TrimSpace(out.finalResult)})
		job.Append(store.LogLine{Type: "done", Content: "✅ 继续开发完成！"})
		// Record the effective developer model for this continue round (success
		// path only — "most recent successful run" semantics).
		if perr := h.reqSvc.UpdateDeveloperModel(body.RequirementID, model); perr != nil {
			log.Printf("[continue-coding] failed to persist developer_model for %s: %v", body.RequirementID, perr)
		}
		job.Finish(0, store.JobDone)
		log.Printf("[continue-coding] job %s finished for %s", job.ID, body.RequirementID)
	}()
}

// StreamJob streams a background job's log lines via SSE.
// Replays all existing lines first, then pushes new ones until the job finishes or the client disconnects.
func (h *WizardHandler) StreamJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, 400, "INVALID", "missing job id")
		return
	}
	job, ok := h.jobs.Get(id)
	if !ok {
		writeError(w, 404, "NOT_FOUND", "job not found")
		return
	}

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	rc.Flush()

	ch, _ := job.Subscribe()
	defer job.Unsubscribe(ch)

	for {
		select {
		case <-r.Context().Done():
			return
		case line, open := <-ch:
			if !open {
				// Job finished — send final status event and close
				_, status, exitCode := job.Snapshot()
				doneData, _ := json.Marshal(map[string]interface{}{
					"type":      "job_done",
					"status":    string(status),
					"exit_code": exitCode,
				})
				fmt.Fprintf(w, "data: %s\n\n", string(doneData))
				rc.Flush()
				return
			}
			data, _ := json.Marshal(line)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			rc.Flush()
		}
	}
}

// ArchitectDesign is the architect-phase design generator. It creates a
// background JobStore job, persists its id on the requirement (so a page refresh
// can reconnect to the running job and show "executing" instead of the start
// button), and returns the job id immediately. Claude then runs in plan mode
// (stream-json) in a goroutine, writing progress into the job. On success the
// plan markdown is persisted to design_docs and the design_job_id is cleared;
// the requirement stays status=designing until the user manually marks 方案完成.
//
// Subscribe to the live stream via GET /api/wizard/jobs/{job_id}/stream and
// poll the snapshot via GET /api/wizard/jobs/{job_id} (same pattern as
// start-coding).
func (h *WizardHandler) ArchitectDesign(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequirementID string `json:"requirement_id"`
		Model         string `json:"model"`
		ReadKnowledge bool   `json:"read_knowledge"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	id := body.RequirementID
	if id == "" {
		writeError(w, 400, "INVALID", "missing requirement id")
		return
	}

	req, err := h.reqSvc.Get(id)
	if err != nil {
		writeError(w, 404, "NOT_FOUND", "requirement not found")
		return
	}
	project, _ := h.projectSvc.Get(req.ProjectID)
	projectPath := ""
	defaultBranch := ""
	if project != nil {
		projectPath = project.LocalPath
		defaultBranch = project.DefaultBranch
	}

	// Session threading: the architect stage continues the SAME conversation
	// thread as the analyst. On the first design pass we fork off the analyst
	// session (--resume <analysis_sid> --fork-session) so the architect inherits
	// the full analysis discussion and swaps in the architect persona via
	// --system-prompt; the forked session gets a new id we read back from the
	// stream and persist as design_session_id. On a re-run (design_session_id
	// already set) we just --resume it. We do NOT re-feed Description /
	// AcceptanceCriteria — the resumed conversation already has them.
	sourceSID := ""
	fork := false
	if req.DesignSessionID != "" {
		sourceSID = req.DesignSessionID
	} else if req.AnalysisSessionID != "" {
		sourceSID = req.AnalysisSessionID
		fork = true
	}
	// Skip-analysis path: when skip_analysis is set and there is no analyst
	// session to fork, run architect-design as a FRESH claude conversation
	// (no --resume/--fork-session) seeded only with the requirement title/
	// description + collectProjectContext. This bypasses the analyst stage.
	skipAnalysis := req.SkipAnalysis
	if sourceSID == "" && !skipAnalysis {
		writeError(w, 400, "NO_SESSION", "尚未找到需求分析会话，请先完成「需求分析」再生成技术方案。")
		return
	}

	// Forking the analyst session requires it to have been anchored to the
	// isolated worktree — otherwise the design (and later the coding stage that
	// forks the design) inherits original-dir absolute paths.
	if fork {
		if gerr := h.requireAnchoredFork(req, projectPath); gerr != nil {
			writeError(w, 409, "UNANCHORED_SESSION", gerr.Error())
			return
		}
	}

	// Pre-mint + persist the design session id BEFORE spawning claude so it
	// survives a mid-run restart. Two cases produce a NEW id: forking off the
	// analyst session (--resume <src> --fork-session --session-id <new>) and the
	// skip-analysis fresh session (--session-id <new>). A re-run just resumes the
	// stored design_session_id (no new id).
	newDesignSID := ""
	if fork || sourceSID == "" {
		newDesignSID = util.NewUUID()
		if perr := h.reqSvc.UpdateDesignSession(id, newDesignSID); perr != nil {
			log.Printf("[architect-design] failed to persist design session for %s: %v", id, perr)
		}
	}

	// Anchor the architect stage to the isolated worktree (created here if the
	// analyst stage was skipped) so the plan and its session are rooted in the
	// worktree — otherwise the coding stage forks this session and follows the
	// original-dir absolute paths back to the shared checkout.
	workDir, wdErr := h.resolveWorkDir(req, projectPath, defaultBranch)
	if wdErr != nil {
		writeError(w, 500, "WORKTREE_FAILED", "worktree 创建失败："+wdErr.Error())
		return
	}

	// Create the job, persist its id so a refresh can reconnect, and return
	// the job id immediately. The plan-mode claude run happens in a goroutine
	// writing progress into the job store.
	job := h.jobs.Create(id)
	if perr := h.reqSvc.UpdateDesignJob(id, job.ID); perr != nil {
		log.Printf("[architect-design] failed to persist design_job_id for %s: %v", id, perr)
	}
	writeJSON(w, 200, map[string]string{"job_id": job.ID})

	// Plan-mode task prompt. When resuming/forking an existing conversation
	// (analyst session present), the resumed thread already carries the
	// requirement and its analysis — we just ask Claude to switch to the
	// architect role and produce a plan. On the skip-analysis path (no
	// session) we must seed the fresh conversation with the requirement plus
	// pre-read project context, since there is no prior discussion to inherit.
	prompt := "现在切换到「架构师」角色。基于我们刚才完成的需求分析对话，" +
		"请阅读项目相关源文件核实技术细节，制定具体可执行的技术实现方案（plan）。" +
		"方案应涵盖：整体实现思路、需要新增或修改的文件、具体实现步骤、数据模型/数据库变更、实现风险及应对。"
	if skipAnalysis && sourceSID == "" {
		docBlock, _, treeSummary := collectProjectContext(workDir, req.Title)
		prompt = "现在切换到「架构师」角色。请基于以下需求与项目信息，阅读相关源文件核实技术细节，" +
			"制定具体可执行的技术实现方案（plan）。\n\n" +
			"## 需求标题\n" + req.Title + "\n\n" +
			"## 需求描述\n" + req.Description + "\n\n" +
			"## 项目上下文\n" + docBlock + "\n" + treeSummary + "\n\n" +
			"方案应涵盖：整体实现思路、需要新增或修改的文件、具体实现步骤、数据模型/数据库变更、实现风险及应对。" +
			"请先复述你对需求的理解，再给出方案。"
	}

	systemPrompt, model := h.roleConfig("architect")
	// Per-request model override (highest precedence); empty means role default.
	if body.Model != "" {
		model = body.Model
	}
	job.SetModel(model)

	go func() {
		log.Printf("[architect-design] job %s started for %s (fork=%v skip=%v)", job.ID, id, fork, skipAnalysis && sourceSID == "")

		// Optional knowledge pre-read: inject the project knowledge relevant to
		// this requirement and surface what was read via a "knowledge" SSE event.
		// Default off (read_knowledge=false) keeps the legacy behavior untouched.
		var kbReadTitles []string
		if body.ReadKnowledge {
			kbBlock, kbTitles := h.buildKnowledgeBlock(req.ProjectID, req.Title)
			if kbBlock != "" {
				prompt = kbBlock + "\n" + prompt
			}
			emitKnowledgeEvent(job, kbTitles)
			kbReadTitles = kbTitles
		}

		job.Append(store.LogLine{Type: "phase", Content: "📐 Claude 正在 plan 模式下探索代码并制定技术方案..."})

		// context.Background(): the HTTP request has already returned, so we
		// must not tie the claude subprocess's lifetime to r.Context() (which
		// is cancelled the moment the handler returns).
		sessionArg := sourceSID
		forkSessionID := ""
		if fork {
			forkSessionID = newDesignSID
		} else if sourceSID == "" {
			sessionArg = newDesignSID // skip-analysis fresh session
		}
		cmd := h.llm.StreamCmd(context.Background(), llm.StreamOpts{
			Prompt:         prompt,
			WorkDir:        workDir,
			SystemPrompt:   systemPrompt,
			Model:          cliModelArg(model),
			SessionID:      sessionArg,
			Resume:         sourceSID != "",
			Fork:           fork,
			ForkSessionID:  forkSessionID,
			PermissionMode: "plan",
		})
		out := runClaudeStream(jobSink{job}, cmd, "architect-design", h.usageCtxFor("architect_design", id, req.ProjectID, job.ID, model, ""))

		if out.staleSession {
			// The source conversation is gone. Clear whichever session id was
			// stale so the user can redo the prior stage, surface a recovery
			// hint, and clear the active job pointer. On the skip-analysis path
			// there is no source session to be stale, so this branch is a
			// no-op guard; we still surface a generic recovery hint.
			if sourceSID == "" {
				job.Append(store.LogLine{Type: "error", Content: "会话异常，请重试生成技术方案。"})
			} else if fork {
				_ = h.reqSvc.UpdateAnalysisSession(id, "")
				job.Append(store.LogLine{Type: "error", Content: "需求分析会话已过期。请重新进行「需求分析」后再生成技术方案。"})
			} else {
				_ = h.reqSvc.UpdateDesignSession(id, "")
				job.Append(store.LogLine{Type: "error", Content: "技术方案会话已过期。请重新生成技术方案。"})
			}
			_ = h.reqSvc.UpdateDesignJob(id, "")
			job.Finish(1, store.JobError)
			return
		}

		// The design session id is already persisted upfront. Correct it only if
		// the CLI reported a different id than the one we pre-minted (a safety
		// net in case the --session-id override semantics ever change).
		if out.sessionID != "" && out.sessionID != newDesignSID && out.sessionID != sourceSID {
			if perr := h.reqSvc.UpdateDesignSession(id, out.sessionID); perr != nil {
				log.Printf("[architect-design] failed to persist design session for %s: %v", id, perr)
			}
		}

		// In plan mode, the full plan markdown is captured from the Write
		// tool_use event that lands in ~/.claude/plans/*.md (runClaudeStream
		// stores it in out.planContent). Fall back to the result text if
		// capture missed it (e.g. a proxy that doesn't emit tool_use blocks in
		// the assistant event).
		planMarkdown := out.planContent
		if planMarkdown == "" {
			planMarkdown = out.finalResult
		}
		// The run ended with an error (upstream proxy 504, api_error result
		// event, or non-zero exit). The Write tool_use that populates
		// planContent fires BEFORE the model's final API call, so a 504 on that
		// trailing call leaves planMarkdown non-empty while the run actually
		// failed. Treating that as success appends a green ✅ and the user has
		// no idea the run was interrupted (and the captured plan may be
		// partial). Surface the error instead: save the captured plan as a
		// fallback so the exploration work isn't lost, but mark the job errored
		// so the UI shows the failure and the user can retry.
		if out.errMsg != "" {
			if planMarkdown != "" {
				if _, err := h.reqSvc.UpdateDesign(id, planMarkdown); err != nil {
					log.Printf("[architect-design] failed to save partial design for %s: %v", id, err)
				}
			}
			_ = h.reqSvc.UpdateDesignJob(id, "")
			job.Append(store.LogLine{Type: "error", Content: out.errMsg})
			job.Finish(1, store.JobError)
			return
		}
		if planMarkdown == "" {
			errMsg := out.errMsg
			if errMsg == "" {
				errMsg = "Claude 未返回结果，请重试"
			}
			job.Append(store.LogLine{Type: "error", Content: errMsg})
			_ = h.reqSvc.UpdateDesignJob(id, "")
			job.Finish(1, store.JobError)
			return
		}

		// Persist the design (sets status=designing) and clear the active job
		// pointer so a refresh shows the finished design instead of "executing".
		if _, err := h.reqSvc.UpdateDesign(id, planMarkdown); err != nil {
			log.Printf("[architect-design] failed to save design for %s: %v", id, err)
			job.Append(store.LogLine{Type: "error", Content: "保存技术方案失败: " + err.Error()})
			_ = h.reqSvc.UpdateDesignJob(id, "")
			job.Finish(1, store.JobError)
			return
		}
		_ = h.reqSvc.UpdateDesignJob(id, "")

		// Record the effective model for the architect stage (success path only).
		if perr := h.reqSvc.UpdateArchitectModel(id, model); perr != nil {
			log.Printf("[architect-design] failed to persist architect_model for %s: %v", id, perr)
		}
		// Close the knowledge loop: mark which read entries the run actually used.
		if len(kbReadTitles) > 0 {
			items, used := evaluateKnowledgeUsage(kbReadTitles, out.toolFiles, planMarkdown)
			emitKnowledgeResultEvent(job, items, used)
		}
		job.Append(store.LogLine{Type: "done", Content: "✅ 技术方案已生成！"})
		job.Finish(0, store.JobDone)
		log.Printf("[architect-design] job %s finished for %s", job.ID, id)
	}()
}

// GetJob returns the current state and full log of a background coding job.
func (h *WizardHandler) GetJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, 400, "INVALID", "missing job id")
		return
	}
	job, ok := h.jobs.Get(id)
	if !ok {
		// Backend may have restarted since the job ran — try the durable log
		// store so the user can still review the development record.
		status, exitCode, startedAt, finishedAt, jobModel, lines, perr := h.jobLogSvc.Get(id)
		if perr != nil {
			writeError(w, 404, "NOT_FOUND", "job not found")
			return
		}
		writeJSON(w, 200, map[string]interface{}{
			"job_id":      id,
			"status":      status,
			"exit_code":   exitCode,
			"log":         lines,
			"model":       jobModel,
			"started_at":  startedAt,
			"finished_at": finishedAt,
		})
		return
	}
	logLines, status, exitCode := job.Snapshot()
	writeJSON(w, 200, map[string]interface{}{
		"job_id":      job.ID,
		"status":      status,
		"exit_code":   exitCode,
		"log":         logLines,
		"model":       job.Model,
		"started_at":  job.StartedAt,
		"finished_at": job.FinishedAt,
	})
}

// toolResultContent extracts and truncates the content of a tool_result block.
func toolResultContent(b map[string]interface{}) string {
	switch v := b["content"].(type) {
	case string:
		return truncateStr(v, 200)
	case []interface{}:
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				if text, ok := m["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return truncateStr(strings.Join(parts, " "), 200)
	}
	return ""
}

// extractJSON strips prose and code fences surrounding a JSON object.
// It finds the first '{' and the matching closing '}', returning that substring.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	start := strings.Index(s, "{")
	if start == -1 {
		return s
	}
	depth := 0
	inStr := false
	escape := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if c == '\\' && inStr {
			escape = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				return strings.TrimSpace(s[start : i+1])
			}
		}
	}
	return strings.TrimSpace(s[start:])
}

func sendStatus(w io.Writer, rc *http.ResponseController, typ string, content string) {
	jsonLine, _ := json.Marshal(map[string]string{"type": typ, "content": content})
	fmt.Fprintf(w, "data: %s\n\n", string(jsonLine))
}

// claudeResultError extracts a human-readable error message from a non-success
// claude "result" event. The CLI surfaces auth/transport failures in the
// "error", "api_error_status", or "result" fields; we also log the full event.
func claudeResultError(scope string, evt map[string]interface{}) string {
	msg := "Claude 执行出错"
	if v, ok := evt["error"].(string); ok && v != "" {
		msg = v
	} else if v, ok := evt["result"].(string); ok && v != "" {
		msg = v
	} else if v, ok := evt["api_error_status"].(float64); ok && v != 0 {
		msg = fmt.Sprintf("API error (HTTP %v)", v)
	}
	// An upstream proxy 400 (bad request / not found) usually means the
	// configured base URL / token / model is wrong or the account is out of
	// quota — point the user at the settings rather than blaming the model.
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "400") &&
		(strings.Contains(lower, "bad request") || strings.Contains(lower, "not found")) {
		msg += "\n（这是上游代理返回的 400，通常是 BASE_URL / Token / 模型 配置有误或额度耗尽，请在「设置」里检查 Claude 配置）"
	}
	if raw, err := json.Marshal(evt); err == nil {
		log.Printf("[%s] claude result event: %s", scope, truncateStr(string(raw), 800))
	}
	return msg
}

// claudeStreamOutcome is the result of running one claude stream-json command to
// completion. finalResult holds the "result" event text on success. On failure
// errMsg is a human-readable message; staleSession is true when the failure was
// specifically a --resume against a conversation that no longer exists on disk,
// so the caller can transparently fall back to a fresh session instead of
// surfacing a hard error.
type claudeStreamOutcome struct {
	finalResult     string
	sessionID       string // session_id of this run, read from the system/init event. For a --fork-session run this is the NEW forked id.
	staleSession    bool
	errMsg          string
	hadStreamEvents bool     // true if any stream_event/content_block_delta arrived
	planContent     string   // full markdown captured from a plan-mode Write tool_use to ~/.claude/plans/*.md
	actualModel     string   // model id returned by the API, captured from the assistant event's message.model
	toolFiles       []string // file paths / patterns touched by Read/Write/Edit/Grep/Glob tool calls (for knowledge-usage evaluation)
}

// isStaleSessionError reports whether a non-success result event (optionally
// combined with stderr) indicates the --resume target conversation doesn't exist
// on disk. The claude CLI surfaces this as an "errors" array entry of the form
// "No conversation found with session ID: <uuid>" (subtype error_during_execution).
// evt may be nil, in which case only stderr is checked.
func isStaleSessionError(evt map[string]interface{}, stderr string) bool {
	contains := func(s string) bool { return strings.Contains(s, "No conversation found") }
	if stderr != "" && contains(stderr) {
		return true
	}
	if evt == nil {
		return false
	}
	if s, ok := evt["error"].(string); ok && contains(s) {
		return true
	}
	if s, ok := evt["result"].(string); ok && contains(s) {
		return true
	}
	if s, ok := evt["message"].(string); ok && contains(s) {
		return true
	}
	if errs, ok := evt["errors"].([]interface{}); ok {
		for _, e := range errs {
			if s, ok := e.(string); ok && contains(s) {
				return true
			}
		}
	}
	return false
}

// streamSink consumes one parsed claude stream-json event as a UI log line.
// The SSE sink frames it as a "data: {...}\n\n" SSE event and flushes per line;
// the job sink appends it to a JobStore job so background-job subscribers
// receive it (and survive a page refresh via the job's replay buffer).
type streamSink interface {
	emit(line store.LogLine)
}

// sseSink writes log lines directly to an SSE response, flushed per event.
type sseSink struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (s sseSink) emit(line store.LogLine) {
	data, _ := json.Marshal(line)
	fmt.Fprintf(s.w, "data: %s\n\n", string(data))
	s.rc.Flush()
}

// jobSink appends log lines to a JobStore job, fanning them out to subscribers.
type jobSink struct {
	job *store.Job
}

func (s jobSink) emit(line store.LogLine) {
	s.job.Append(line)
}

// runClaudeStream starts a claude stream-json cmd, parses its events, and
// translates them to UI log lines via the sink (phase / tool_call / message
// frames). It does NOT emit terminal error/done frames — the caller owns those.
// On a non-success result it returns the error message (and flags a stale
// --resume so the caller can recover). The caller must NOT have started cmd.
//
// uctx, when non-nil, records the result event's token usage as a token_usage
// row (best-effort — errors are logged and never break the stream). Pass the
// same uctx to both calls of a stale-fallback retry so each invocation's
// tokens are recorded.
func runClaudeStream(sink streamSink, cmd *exec.Cmd, scope string, uctx *usageCtx) claudeStreamOutcome {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return claudeStreamOutcome{errMsg: "启动 Claude 失败: " + err.Error()}
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	// Put the claude subprocess in its own process group so we can kill the
	// whole group (claude + any tool/MCP subprocesses it spawned). Some
	// third-party proxies keep the upstream connection open after the model's
	// final event: the CLI never exits on its own, holding stdout open so a
	// plain scanner.Scan() blocks forever and the job never finishes. Killing
	// the group on completion lets runClaudeStream return promptly.
	//
	// The Setpgid assignment + group-kill logic live in wizard_proc_unix.go
	// (//go:build !windows) because process groups are a POSIX concept. On
	// Windows the helpers in wizard_proc_windows.go fall back to signaling
	// just the direct process — no orphan grand-children issue exists there.
	setProcessGroup(cmd)
	logClaudeExecDiag(scope, cmd)
	if err := cmd.Start(); err != nil {
		log.Printf("[%s] exec diag: cmd.Start() failed: %T %v", scope, err, err)
		return claudeStreamOutcome{errMsg: "启动 Claude 失败: " + err.Error()}
	}
	logClaudeEnvConfig(scope, cmd)
	logClaudeCmd(scope, cmd)

	var out claudeStreamOutcome
	// thinkingTokens tracks the last token count we reported so we can
	// throttle thinking_tokens progress messages (every 50 tokens).
	var lastThinkingReport int
	// gotResult is set when the terminal "result" event arrives; once set we
	// stop reading and tear the process down — there is nothing useful after
	// the result event, and waiting for stdout EOF can hang forever on proxies
	// that don't close the stream.
	gotResult := false
	// Stall watchdog: armed after the first stdout line. If no new line
	// arrives for stallTimeout (proxy went silent without emitting a result
	// event), kill the group so the scan loop unblocks. Without this a proxy
	// that drops the connection mid-stream would wedge the job permanently.
	const stallTimeout = 3 * time.Minute
	heartbeats := make(chan struct{}, 1)
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		timer := time.NewTimer(stallTimeout)
		if !timer.Stop() {
			<-timer.C
		}
		var armed bool
		for {
			select {
			case _, ok := <-heartbeats:
				if !ok {
					return
				}
				if !armed {
					armed = true
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(stallTimeout)
			case <-timer.C:
				log.Printf("[%s] stream stalled %v with no new events; terminating", scope, stallTimeout)
				killProcessGroup(cmd)
				return
			}
		}
	}()
	beat := func() {
		select {
		case heartbeats <- struct{}{}:
		default:
		}
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		beat()
		if line == "" {
			continue
		}
		var evt map[string]interface{}
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			// stream-json is one JSON object per line; a non-JSON line is usually
			// a fatal error printed by the CLI. Surface it in the logs.
			if strings.TrimSpace(line) != "" {
				log.Printf("[%s] non-json stdout: %s", scope, truncateStr(line, 500))
			}
			continue
		}
		evtType, _ := evt["type"].(string)
		switch evtType {
		case "system":
			sub, _ := evt["subtype"].(string)
			switch sub {
			case "init":
				// The init event arrives as soon as Claude connects (a few seconds
				// in). Some proxies buffer the whole model response and only deliver
				// it at completion, so without this the client would see nothing for
				// the entire wait. Surface it as a phase so the user knows Claude is
				// connected and thinking.
				if sid, ok := evt["session_id"].(string); ok && sid != "" {
					out.sessionID = sid
				}
				sink.emit(store.LogLine{Type: "phase", Content: "🤖 Claude 已连接，正在思考…"})
			case "thinking_tokens":
				// Third-party proxy models (e.g. zai-org/glm) don't stream text
				// token-by-token; they batch everything into the final assistant event.
				// But they DO stream thinking_tokens incrementally, giving us a
				// heartbeat we can use to show progress. Report every 50 tokens so
				// the user sees live activity instead of a frozen spinner.
				if tokens, ok := evt["estimated_tokens"].(float64); ok {
					t := int(tokens)
					if t-lastThinkingReport >= 50 {
						lastThinkingReport = t
						sink.emit(store.LogLine{Type: "phase", Content: fmt.Sprintf("🤔 模型思考中… (%d tokens)", t)})
					}
				}
			}
		case "stream_event":
			// stream-json emits incremental content_block_delta events as Claude
			// generates text token-by-token. Surfacing these gives the user
			// real-time output instead of waiting for the batched "assistant" event.
			inner, _ := evt["event"].(map[string]interface{})
			if inner == nil {
				continue
			}
			innerType, _ := inner["type"].(string)
			switch innerType {
			case "content_block_delta":
				delta, _ := inner["delta"].(map[string]interface{})
				if delta == nil {
					continue
				}
				deltaType, _ := delta["type"].(string)
				switch deltaType {
				case "text_delta":
					text, _ := delta["text"].(string)
					if text == "" {
						continue
					}
					out.hadStreamEvents = true
					sink.emit(store.LogLine{Type: "message", Content: text})
				case "input_json_delta":
					// tool input being assembled — not surfaced to the client
				}
			case "content_block_start":
				// A new tool_use block starting — surface the tool name immediately
				// so the user sees "calling tool X" before the input is assembled.
				block, _ := inner["content_block"].(map[string]interface{})
				if block == nil {
					continue
				}
				if block["type"] == "tool_use" {
					toolName, _ := block["name"].(string)
					if toolName != "" {
						sink.emit(store.LogLine{Type: "tool_call", Content: toolCallLabel(toolName, nil)})
					}
				}
			}
		case "assistant":
			// The "assistant" event is a batched summary of the full turn. When the
			// model supports streaming (stream_event/content_block_delta), text is
			// already delivered incrementally and we skip it here. When the model
			// does NOT emit stream_event (e.g. third-party proxy models), the
			// assistant event carries the only copy of the text, so we emit it now.
			// We track whether any stream_event text arrived to decide which path to take.
			msg, _ := evt["message"].(map[string]interface{})
			// The message carries the model id the API actually served — capture
			// it so the recorded token_usage row matches a model in the config's
			// price list (used for cost attribution; overrides the pre-dispatch
			// role-config model at the result event below).
			if m, ok := msg["model"].(string); ok {
				out.actualModel = m
			}
			content, _ := msg["content"].([]interface{})
			for _, block := range content {
				b, _ := block.(map[string]interface{})
				switch b["type"] {
				case "tool_use":
					toolName, _ := b["name"].(string)
					input, _ := b["input"].(map[string]interface{})
					// Capture the plan file content from a plan-mode Write to
					// ~/.claude/plans/*.md. The assistant event carries the complete
					// tool_use input (unlike stream_event input_json_delta which is
					// fragmentary), so this is the authoritative source.
					if toolName == "Write" && input != nil {
						if fp, ok := input["file_path"].(string); ok {
							if strings.Contains(filepath.ToSlash(fp), "/.claude/plans/") && strings.HasSuffix(fp, ".md") {
								if c, ok := input["content"].(string); ok && c != "" {
									out.planContent = c
								}
							}
						}
					}
					// Harvest the touched path/pattern for the "was the injected
					// knowledge actually used?" evaluation (cheap: no LLM call, just
					// string capture from events that are already flowing).
					if p := inputToolPath(toolName, input); p != "" {
						out.toolFiles = append(out.toolFiles, p)
					}
					// Emit with full input context (content_block_start fired bare label already,
					// but only if stream_event was delivered — safe to emit again, client dedupes by rendering order).
					if !out.hadStreamEvents {
						sink.emit(store.LogLine{Type: "tool_call", Content: toolCallLabel(toolName, input)})
					}
				case "text":
					// Only emit if we never received stream_event deltas for this turn,
					// i.e. the model batches everything into the assistant event.
					if !out.hadStreamEvents {
						text, _ := b["text"].(string)
						if text != "" {
							sink.emit(store.LogLine{Type: "message", Content: text})
						}
					}
				}
			}
		case "result":
			subtype, _ := evt["subtype"].(string)
			// The CLI sometimes emits subtype:"success" even when the upstream
			// proxy rejected the request — it carries api_error_status (a number),
			// is_error:true, terminal_reason:"api_error", and a "result" of
			// "API Error: 400 ...". Treat those as failures so the error surfaces
			// via SSE instead of being returned as if it were the model's output.
			apiErrStatus := 0.0
			if v, ok := evt["api_error_status"].(float64); ok {
				apiErrStatus = v
			}
			isErr, _ := evt["is_error"].(bool)
			terminalReason, _ := evt["terminal_reason"].(string)
			realSuccess := subtype == "success" && !isErr &&
				terminalReason != "api_error" && apiErrStatus == 0
			if realSuccess {
				out.finalResult, _ = evt["result"].(string)
			} else {
				out.errMsg = claudeResultError(scope, evt)
				if isStaleSessionError(evt, stderrBuf.String()) {
					out.staleSession = true
					log.Printf("[%s] stale --resume detected (session not on disk)", scope)
				}
			}
			// Override the pre-dispatch model with the API-returned one so the
			// recorded row costs against the right model entry. No-op when the
			// proxy didn't report a model (fall back to the effective model).
			if out.actualModel != "" && uctx != nil {
				uctx.Model = out.actualModel
			}
			// Record token usage from the result event (success or failure —
			// tokens are consumed either way). Best-effort: recordFrom swallows
			// all errors so a DB hiccup can never break the claude stream.
			uctx.recordFrom(evt)
			gotResult = true
		}
		if gotResult {
			break
		}
	}

	// Stop the stall watchdog and tear the process group down. The CLI may
	// still be alive (proxy held the connection open), and tool/MCP
	// grandchildren may still hold the stdout pipe open; killing the group
	// unblocks cmd.Wait() and lets us return what we captured.
	close(heartbeats)
	<-watchdogDone
	killProcessGroup(cmd)

	if err := cmd.Wait(); err != nil {
		stderrTrim := strings.TrimSpace(stderrBuf.String())
		log.Printf("[%s] claude exited: %v stderr=%s", scope, err, stderrTrim)
		// A non-zero exit is only fatal if we never got a result; some error
		// events still carry a usable result field.
		if out.finalResult == "" && out.errMsg == "" {
			msg := "Claude 异常退出: " + err.Error()
			if stderrTrim != "" {
				msg = "Claude 异常退出: " + stderrTrim
			}
			out.errMsg = msg
		}
	}
	// Re-check staleness against stderr in case the CLI printed the error there
	// instead of emitting a result event.
	if !out.staleSession && isStaleSessionError(nil, stderrBuf.String()) {
		out.staleSession = true
	}
	if out.staleSession && out.errMsg == "" {
		out.errMsg = "分析对话会话已过期"
	}
	return out
}

// killProcessGroup is implemented per-platform in wizard_proc_unix.go and
// wizard_proc_windows.go — process groups are a POSIX concept, so the
// Windows build falls back to signaling just the direct process.

// analystFirstTurnDisallowedTools blocks file/code tools on the analyst first
// turn so Claude answers from the pre-read context without tool use. The
// atlascloud proxy mangles multi-turn tool-use streaming ("Content block not
// found"); the first turn already has the pre-read docs + tree, so it doesn't
// need to read files. Resume turns keep all tools.
var analystFirstTurnDisallowedTools = []string{"Read", "Glob", "Grep", "Bash", "Write", "Edit"}

// runAnalystTurn runs one analyst-chat turn with automatic stale-session
// recovery. firstTurnPrompt is a lazy builder for the first-turn prompt (it
// pre-reads bounded project context and emits the pre-read files as progress);
// it is only invoked when doing a first-turn run — the genuine first turn, or
// the stale-resume fallback — so resume turns don't pay the pre-read cost.
// resumePrompt is just the new user message for a resumed turn. When resume==true
// and the target conversation no longer exists on disk (Claude reports "No
// conversation found"), it transparently falls back: mints a fresh session id and
// re-runs as a first turn, so a stale id stuck in the DB never wedges the chat.
// sink receives the turn's log lines (phase / tool_call / message); in the
// JobStore flow it is a jobSink so the lines survive a page refresh via the job's
// replay buffer. Returns the final result text and the session id that actually
// landed on disk (which the caller persists).
func (h *WizardHandler) runAnalystTurn(ctx context.Context, firstTurnPrompt func() string, resumePrompt, projectPath, systemPrompt, model, sessionID string, resume bool, sink streamSink, uctx *usageCtx) (finalResult, newSessionID string, err error) {
	prompt := resumePrompt
	if !resume {
		prompt = firstTurnPrompt()
	}
	log.Printf("[analyst-chat] Running claude stream-json (session=%s, resume=%v), prompt=%d bytes", sessionID, resume, len(prompt))

	// On a first-turn run (genuine first turn or stale-resume fallback), block
	// file/code tools so Claude answers purely from the pre-read context. The
	// atlascloud proxy mangles multi-turn tool-use streaming ("Content block not
	// found"), and the first turn has the pre-read docs + tree so it doesn't
	// need to read files. Resume turns keep tools (the user may ask Claude to
	// verify specific code in follow-up).
	var disallowed []string
	if !resume {
		disallowed = analystFirstTurnDisallowedTools
	}
	cmd := h.llm.StreamCmd(ctx, llm.StreamOpts{
		Prompt:          prompt,
		WorkDir:         projectPath,
		SystemPrompt:    systemPrompt,
		Model:           cliModelArg(model),
		SessionID:       sessionID,
		Resume:          resume,
		DisallowedTools: disallowed,
	})
	out := runClaudeStream(sink, cmd, "analyst-chat", uctx)

	if out.staleSession && resume {
		// Stale --resume: the session file is gone (typically a stale id left by
		// an older build before the "persist after success" guard). Recover by
		// starting a fresh conversation, folding the user's latest message into
		// the first-turn prompt.
		freshID := util.NewUUID()
		log.Printf("[analyst-chat] stale session %s — falling back to fresh first turn %s", sessionID, freshID)
		sink.emit(store.LogLine{Type: "phase", Content: "🔄 检测到过期会话，正在重新开始分析..."})
		prompt = firstTurnPrompt()
		cmd = h.llm.StreamCmd(ctx, llm.StreamOpts{
			Prompt:          prompt,
			WorkDir:         projectPath,
			SystemPrompt:    systemPrompt,
			Model:           cliModelArg(model),
			SessionID:       freshID,
			DisallowedTools: analystFirstTurnDisallowedTools,
		})
		out = runClaudeStream(sink, cmd, "analyst-chat", uctx)
		sessionID = freshID
	}

	if out.finalResult == "" {
		if out.errMsg != "" {
			return "", sessionID, fmt.Errorf("%s", out.errMsg)
		}
		return "", sessionID, fmt.Errorf("Claude 未返回结果，请重试")
	}
	return out.finalResult, sessionID, nil
}

// developerChatDisallowedTools blocks file-mutation tools during a developer
// "追加调整" chat turn so Claude discusses the adjustment (reads code, proposes
// an approach) WITHOUT editing — the actual edits run in a later start-coding
// job. Read/Glob/Grep/Bash stay available so the developer can ground its
// understanding in the real code.
var developerChatDisallowedTools = []string{"Write", "Edit"}

// runDeveloperTurn runs one developer-chat turn with automatic stale-session
// recovery, mirroring runAnalystTurn but for the developer role. firstTurnPrompt
// is a lazy self-contained prompt (no pre-read — the developer reads files via
// tools instead); resumePrompt is the framed user message. Write/Edit are
// disallowed so the turn stays discussion-only. newSID is the pre-minted id the
// caller assigned for a fork (--session-id on --fork-session) or a fresh session
// (--session-id); it is "" on a plain resume. Returns the final result text and
// the session id that actually landed on disk (a forked or fresh id differs
// from the input; the caller persists it).
func (h *WizardHandler) runDeveloperTurn(ctx context.Context, firstTurnPrompt func() string, resumePrompt, projectPath, systemPrompt, model, sessionID string, fork bool, newSID string, w http.ResponseWriter, rc *http.ResponseController, uctx *usageCtx) (finalResult, newSessionID string, err error) {
	resume := sessionID != ""
	prompt := resumePrompt
	if !resume {
		prompt = firstTurnPrompt()
	}
	log.Printf("[developer-chat] Running claude stream-json (session=%s, resume=%v, fork=%v, new=%s), prompt=%d bytes", sessionID, resume, fork, newSID, len(prompt))

	// sessionID is the --resume source; newSID is the pre-minted id to assign —
	// as the forked session id (--session-id on --fork-session) when fork is set,
	// or as the fresh session id (--session-id) when starting with no session.
	sessionArg := sessionID
	forkSessionID := ""
	if resume {
		if fork {
			forkSessionID = newSID
		}
	} else {
		sessionArg = newSID
	}
	cmd := h.llm.StreamCmd(ctx, llm.StreamOpts{
		Prompt:          prompt,
		WorkDir:         projectPath,
		SystemPrompt:    systemPrompt,
		Model:           cliModelArg(model),
		SessionID:       sessionArg,
		Resume:          resume,
		Fork:            fork,
		ForkSessionID:   forkSessionID,
		DisallowedTools: developerChatDisallowedTools,
	})
	out := runClaudeStream(sseSink{w, rc}, cmd, "developer-chat", uctx)

	if out.staleSession && resume {
		// Stale --resume: the target conversation no longer exists on disk.
		// Recover by starting a fresh developer conversation with the
		// self-contained first-turn prompt (no --resume, no --fork-session).
		freshID := util.NewUUID()
		log.Printf("[developer-chat] stale session %s — falling back to fresh session %s", sessionID, freshID)
		sendStatus(w, rc, "phase", "🔄 检测到过期会话，正在重新开始...")
		rc.Flush()
		prompt = firstTurnPrompt()
		cmd = h.llm.StreamCmd(ctx, llm.StreamOpts{
			Prompt:          prompt,
			WorkDir:         projectPath,
			SystemPrompt:    systemPrompt,
			Model:           cliModelArg(model),
			SessionID:       freshID,
			DisallowedTools: developerChatDisallowedTools,
		})
		out = runClaudeStream(sseSink{w, rc}, cmd, "developer-chat", uctx)
		sessionID = freshID
	}

	if out.finalResult == "" {
		if out.errMsg != "" {
			return "", sessionID, fmt.Errorf("%s", out.errMsg)
		}
		return "", sessionID, fmt.Errorf("Claude 未返回结果，请重试")
	}
	// For a forked run runClaudeStream captured the NEW session id from the
	// system/init event; surface it so the caller persists it. For a plain
	// resume it equals the input id; if init never fired it is empty, so keep
	// the input id.
	if out.sessionID != "" {
		return out.finalResult, out.sessionID, nil
	}
	return out.finalResult, sessionID, nil
}

// buildAnalystFirstPrompt assembles the first-turn analyst prompt. description is
// the FULL requirement text (the title is just a short label and may be
// truncated, so the description is the authoritative intent). It also folds in
// the current (rough) analysis, the user's message, and the bounded project
// context pre-read by collectProjectContext (docBlock + a names-only structure
// tree). It is reused both for the genuine first turn and for the stale-resume
// fallback (where the prior conversation is lost, so we restart from this
// scaffold). The instruction deliberately tells Claude NOT to blindly traverse
// the whole project — it already has the layout and key docs.
func buildAnalystFirstPrompt(title, description, currentAnalysis, userMessage, docBlock, treeSummary string) string {
	var b strings.Builder
	if description != "" {
		b.WriteString(fmt.Sprintf("%s\n\n", description))
	}
	if currentAnalysis != "" && currentAnalysis != "[]" {
		b.WriteString(fmt.Sprintf("当前初步分析：\n%s\n\n", currentAnalysis))
	}
	if userMessage != "" {
		b.WriteString(fmt.Sprintf("用户消息：\n%s\n\n", userMessage))
	}
	if docBlock != "" {
		b.WriteString("以下是已预读的项目关键文档与代码（无需重复读取这些文件）：\n")
		b.WriteString(docBlock)
		b.WriteString("\n")
	}
	if treeSummary != "" {
		b.WriteString(treeSummary)
		b.WriteString("\n\n")
	}
	b.WriteString("请**仅基于以上预读上下文**完成工作。**禁止调用工具读取任何文件**——预读已提供关键文档与项目结构；" +
		"若信息不足以判断某点，把该缺失点作为关键问题提给用户，让用户来回答，而不是自己去读文件。\n\n" +
		"1. **分析现有代码**：基于预读内容指出与本需求直接相关的文件、函数、数据结构\n" +
		"2. **识别实现路径**：需要新增或修改哪些部分，有哪些可复用\n" +
		"3. **提出关键问题**：2-3 个你从预读信息中无法确定、必须由用户决策的问题（纯业务/产品决策）\n\n" +
		"先给出你的分析与实现思路，再提问题。\n")
	return b.String()
}

// logClaudeEnvConfig logs which auth-related env vars are present on the claude
// subprocess (presence only — never values, so secrets stay out of the logs).
// Use it to confirm the configured ANTHROPIC_AUTH_TOKEN / ANTHROPIC_BASE_URL are
// applied and that a conflicting inherited ANTHROPIC_API_KEY has been stripped.
func logClaudeEnvConfig(scope string, cmd *exec.Cmd) {
	has := func(k string) bool {
		for _, kv := range cmd.Env {
			if key, _, _ := strings.Cut(kv, "="); key == k {
				return true
			}
		}
		return false
	}
	log.Printf("[%s] claude env present: ANTHROPIC_AUTH_TOKEN=%v ANTHROPIC_BASE_URL=%v ANTHROPIC_API_KEY=%v",
		scope, has("ANTHROPIC_AUTH_TOKEN"), has("ANTHROPIC_BASE_URL"), has("ANTHROPIC_API_KEY"))
}

// logClaudeExecDiag dumps a diagnostic snapshot of the binary Go is about to
// exec. The single-line ENOENT returned by cmd.Start() is one of the most
// misleading errors in Go/Linux — the kernel returns ENOENT for at least
// half a dozen distinct causes (file missing, symlink target missing, ELF
// PT_INTERP missing, shebang script interpreter missing, noexec mount, …).
// When the next deploy hits this, we need enough raw state in the log to
// pinpoint the cause without another round-trip. Always log BEFORE Start so
// the failure case still gets the snapshot.
func logClaudeExecDiag(scope string, cmd *exec.Cmd) {
	if cmd.Path == "" {
		log.Printf("[%s] exec diag: cmd.Path is empty (LookPath must have failed earlier)", scope)
		return
	}
	log.Printf("[%s] exec diag: cmd.Path=%q args=%d env=%d", scope, cmd.Path, len(cmd.Args), len(cmd.Env))

	// 1. LookPath — does Go itself still find the path it just resolved?
	if _, err := exec.LookPath(cmd.Path); err != nil {
		log.Printf("[%s] exec diag: LookPath(%q) failed: %v", scope, cmd.Path, err)
	}

	// 2. Resolve symlinks — the kernel uses the resolved path for exec().
	resolved, err := filepath.EvalSymlinks(cmd.Path)
	if err != nil {
		log.Printf("[%s] exec diag: EvalSymlinks(%q) failed: %v", scope, cmd.Path, err)
		resolved = cmd.Path
	} else {
		log.Printf("[%s] exec diag: resolved=%q", scope, resolved)
	}

	// 3. stat the resolved file so we can see mode, size, mtime.
	if info, err := os.Stat(resolved); err != nil {
		log.Printf("[%s] exec diag: stat(%q) failed: %v", scope, resolved, err)
	} else {
		log.Printf("[%s] exec diag: stat mode=%s size=%d mtime=%s", scope, info.Mode(), info.Size(), info.ModTime().Format(time.RFC3339))
	}

	// 4. Magic bytes — distinguish script (#!) from ELF (\\x7fELF) from junk.
	if f, openErr := os.Open(resolved); openErr == nil {
		var head [4]byte
		n, _ := io.ReadFull(f, head[:])
		_ = f.Close()
		switch {
		case n >= 4 && head[0] == 0x7f && head[1] == 'E' && head[2] == 'L' && head[3] == 'F':
			log.Printf("[%s] exec diag: magic=ELF (pkg-bundled binary, no shebang)", scope)
		case n >= 2 && head[0] == '#' && head[1] == '!':
			log.Printf("[%s] exec diag: magic=#! (script), first line likely contains the interpreter path", scope)
		default:
			log.Printf("[%s] exec diag: magic=\\% x (n=%d)", scope, head[:n], n)
		}
	} else {
		log.Printf("[%s] exec diag: open(%q) failed: %v", scope, resolved, openErr)
	}

	// 5. ldd — surfaces missing shared libs (ENOENT-class) and the ELF PT_INTERP.
	if lddOut, lddErr := exec.Command("ldd", resolved).CombinedOutput(); lddErr == nil {
		out := strings.TrimSpace(string(lddOut))
		if out == "" {
			log.Printf("[%s] exec diag: ldd: (no output — binary not dynamic?)", scope)
		} else {
			log.Printf("[%s] exec diag: ldd:\n%s", scope, out)
		}
	} else {
		log.Printf("[%s] exec diag: ldd failed (%v): %s", scope, lddErr, strings.TrimSpace(string(lddOut)))
	}

	// 6. Critical env vars — PATH tells us where exec will look for the
	// interpreter if this is a script; HOME matters for nm/nvm-managed nodes.
	for _, kv := range cmd.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "PATH", "HOME", "PWD", "NODE_HOME", "NVM_HOME", "NVM_BIN":
			log.Printf("[%s] exec diag: env %s=%s", scope, k, v)
		}
	}

	// 7. CWD — both the server's own cwd and cmd.Dir (the project dir).
	if cwd, err := os.Getwd(); err == nil {
		log.Printf("[%s] exec diag: server cwd=%q", scope, cwd)
	}
	if cmd.Dir != "" {
		if _, err := os.Stat(cmd.Dir); err != nil {
			log.Printf("[%s] exec diag: cmd.Dir=%q DOES NOT EXIST ← likely real ENOENT cause: %v", scope, cmd.Dir, err)
		} else {
			log.Printf("[%s] exec diag: cmd.Dir=%q (exists)", scope, cmd.Dir)
		}
	}

	// 8. uid/gid — the Go process is the same throughout, but past bugs have
	// caught us when a service drops privileges. Cheap to log.
	log.Printf("[%s] exec diag: uid=%d gid=%d euid=%d egid=%d", scope, os.Getuid(), os.Getgid(), os.Geteuid(), os.Getegid())
}

// logClaudeCmd logs the actual claude CLI invocation as a shell command that
// can be copied and run directly in a terminal.
func logClaudeCmd(scope string, cmd *exec.Cmd) {
	parts := make([]string, 0, len(cmd.Args))
	for _, a := range cmd.Args {
		if len(a) > 200 {
			a = a[:200] + fmt.Sprintf("…(%d bytes total)", len(a))
		}
		parts = append(parts, shellQuote(a))
	}
	shell := ""
	if cmd.Dir != "" {
		shell = "cd " + shellQuote(cmd.Dir) + " && "
	}
	shell += strings.Join(parts, " ")
	log.Printf("[%s] claude cmd (copy-paste ready):\n%s", scope, shell)
}

// shellQuote wraps s in single quotes, escaping any single quotes inside.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func streamLines(r io.Reader, w io.Writer, rc *http.ResponseController, streamType string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		jsonLine, _ := json.Marshal(map[string]string{
			"type":    streamType,
			"content": line,
		})
		fmt.Fprintf(w, "data: %s\n\n", string(jsonLine))
		rc.Flush()
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[wizard] stream error (%s): %v", streamType, err)
	}
}

// RefineDoc streams a multi-turn conversation to refine a design doc or a coding instruction.
// doc_type: "design" | "coding"
func (h *WizardHandler) RefineDoc(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RequirementID       string `json:"requirement_id"`
		ProjectPath         string `json:"project_path"`
		DocType             string `json:"doc_type"` // "design" | "coding"
		CurrentDoc          string `json:"current_doc"`
		ConversationHistory string `json:"conversation_history"`
		UserMessage         string `json:"user_message"`
		Model               string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	rc.Flush()

	docLabel := "技术方案"
	if req.DocType == "coding" {
		docLabel = "开发指令"
	}

	// Route to this stage's session: design→architect, coding→developer. The
	// session already holds the stage's conversation and the doc it generated,
	// so we --resume it and append ONLY the user's new message — no
	// ConversationHistory / CurrentDoc re-feeding (the resumed
	// conversation IS the context).
	var requirement *model.Requirement
	if req.RequirementID != "" {
		requirement, _ = h.reqSvc.Get(req.RequirementID)
	}
	sourceSID, roleKey := "", "analyst"
	if requirement != nil {
		sourceSID, roleKey = docStageSession(requirement, req.DocType)
	}
	// Fallback: no resumable session for this stage. This hits design docs
	// generated on the skip-analysis path by builds that didn't persist
	// design_session_id (and any doc predating session threading). For design
	// docs we can still refine: seed a FRESH session with the current doc
	// content and persist its id below so subsequent turns resume it. Coding
	// instructions are never persisted server-side, so without a coding
	// session there is nothing to anchor a fresh session to — keep the hint.
	freshSession := false
	if sourceSID == "" {
		if req.DocType == "design" && requirement != nil && requirement.DesignDocs != "" {
			sourceSID = util.NewUUID()
			freshSession = true
			// Persist the freshly minted id BEFORE the refine run so it survives a
			// restart, consistent with the other wizard stages (analyst/architect/
			// coding all persist their pre-minted id up front).
			if req.RequirementID != "" {
				if perr := h.reqSvc.UpdateDesignSession(req.RequirementID, sourceSID); perr != nil {
					log.Printf("[refine-doc] failed to persist design session for %s: %v", req.RequirementID, perr)
				}
			}
		} else {
			sendStatus(w, rc, "error", "尚未找到该阶段的会话，请先生成"+docLabel+"后再 refine。")
			fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
			rc.Flush()
			return
		}
	}
	systemPrompt, model := h.roleConfig(roleKey)
	// Per-request model override (highest precedence); empty means role default.
	if req.Model != "" {
		model = req.Model
	}

	// Anchor the refine to the same worktree as the stage it resumes, so the
	// conversation never carries original-dir absolute paths.
	projectPath := req.ProjectPath
	defaultBranch := ""
	if requirement != nil {
		if proj, perr := h.projectSvc.Get(requirement.ProjectID); perr == nil {
			if projectPath == "" {
				projectPath = proj.LocalPath
			}
			defaultBranch = proj.DefaultBranch
		}
	}
	workDir, err := h.resolveWorkDir(requirement, projectPath, defaultBranch)
	if err != nil {
		sendStatus(w, rc, "error", err.Error())
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	// The resumed conversation carries the doc + prior turns. Send only the
	// user's latest message plus a steady instruction covering both mid-refine
	// responses and the completion signal. On the fresh-session fallback there
	// is no prior conversation, so the doc itself is seeded into the prompt.
	// The completeness instruction guards against Claude stopping after one
	// heading and emitting a closing phrase (the prior symptom: "除了以上内容
	// 就没有了更多了" after a section header with no content).
	var prompt string
	if freshSession {
		prompt = "以下是当前的「" + docLabel + "」文档：\n\n" + requirement.DesignDocs +
			fmt.Sprintf("\n\n用户消息：\n%s\n\n", req.UserMessage) +
			"请基于上述文档回应用户对「" + docLabel + "」的修改意见，" +
			"完整列出每一个修改点的具体内容（包含涉及的表/字段/接口/逻辑），不要中途截断或留空。" +
			"若用户确认修改已完成，在回复最后单独一行追加：[REFINE_COMPLETE]\n用中文。"
	} else {
		prompt = fmt.Sprintf("用户消息：\n%s\n\n", req.UserMessage) +
			"请基于我们的对话上下文回应用户对「" + docLabel + "」的修改意见，" +
			"完整列出每一个修改点的具体内容（包含涉及的表/字段/接口/逻辑），不要中途截断或留空。" +
			"若用户确认修改已完成，在回复最后单独一行追加：[REFINE_COMPLETE]\n用中文。"
	}

	cmd := h.llm.StreamCmd(r.Context(), llm.StreamOpts{
		Prompt:       prompt,
		WorkDir:      workDir,
		SystemPrompt: systemPrompt,
		Model:        cliModelArg(model),
		SessionID:    sourceSID,
		Resume:       !freshSession,
	})

	// Reuse runClaudeStream so this path gets live content_block_delta text
	// deltas (the model streams incrementally via stream_event), tool-call
	// labels, the "🤖 Claude 已连接" phase, stderr capture, and stale-session
	// detection — the previous hand-rolled parser only handled the batched
	// "assistant" event, so deltas arrived silently and a hung proxy wedged
	// the SSE connection instead of failing fast.
	refineProjectID := ""
	if requirement != nil {
		refineProjectID = requirement.ProjectID
	}
	refineUsage := h.usageCtxFor("refine_doc", req.RequirementID, refineProjectID, "", model, fmt.Sprintf("{\"doc_type\":%q}", req.DocType))
	out := runClaudeStream(sseSink{w: w, rc: rc}, cmd, "refine-doc", refineUsage)

	if out.errMsg != "" || out.finalResult == "" {
		// Stale --resume: the prior conversation file is gone. Clear the stored
		// id so the next attempt either re-runs the stage or takes the
		// fresh-session fallback above.
		if out.staleSession && req.RequirementID != "" {
			if req.DocType == "coding" {
				_ = h.reqSvc.UpdateCodingSession(req.RequirementID, "")
			} else {
				_ = h.reqSvc.UpdateDesignSession(req.RequirementID, "")
			}
		}
		if out.errMsg != "" {
			sendStatus(w, rc, "error", out.errMsg)
		} else if out.staleSession {
			sendStatus(w, rc, "error", "该阶段的会话已过期，请重试（将以新会话继续）。")
		} else {
			sendStatus(w, rc, "error", "Claude 未返回结果，请重试。")
		}
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	// The authoritative conversation lives in the resumed claude session; this
	// history string is only for client-side rendering of the latest exchange.
	var historyParts []string
	if req.UserMessage != "" {
		historyParts = append(historyParts, "User: "+req.UserMessage)
	}
	if out.finalResult != "" {
		historyParts = append(historyParts, "AI: "+strings.TrimSpace(out.finalResult))
	}
	updatedHistory := strings.Join(historyParts, "\n")

	refineComplete := strings.Contains(out.finalResult, "[REFINE_COMPLETE]")

	doneData, _ := json.Marshal(map[string]interface{}{
		"type":            "done",
		"history":         updatedHistory,
		"refine_complete": refineComplete,
	})
	fmt.Fprintf(w, "data: %s\n\n", string(doneData))
	rc.Flush()
}

// ApplyDoc lets Claude rewrite the stored doc field based on the refine
// conversation. It runs as a background JobStore job (same pattern as
// analyst-chat / architect-design): the handler returns a job id immediately
// and Claude runs in a goroutine on context.Background(), so the apply survives
// a page refresh — the previous direct-SSE version tied Claude to r.Context()
// and was killed the moment the browser reconnected, leaving design_docs
// unchanged. The active job id is persisted on the requirement (apply_job_id)
// so a refresh reconnects to the running job. On success the updated doc is
// persisted server-side; the frontend refreshes on job_done to render it.
//
// Streaming progress comes from runClaudeStream, which surfaces the
// system/thinking_tokens heartbeat and stream_event text deltas — the old
// hand-parser only emitted the batched assistant text at the very end, so the
// user saw no activity during the (often multi-minute) regeneration.
//
// doc_type: "design" updates design_docs; "coding" returns a plain-text dev
// instruction (no DB write — the job's message lines carry it).
func (h *WizardHandler) ApplyDoc(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RequirementID string `json:"requirement_id"`
		ProjectPath   string `json:"project_path"`
		DocType       string `json:"doc_type"`
		Model         string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	if req.RequirementID == "" {
		writeError(w, 400, "INVALID", "missing requirement_id")
		return
	}

	requirement, err := h.reqSvc.Get(req.RequirementID)
	if err != nil {
		writeError(w, 404, "NOT_FOUND", "requirement not found")
		return
	}

	// Route to this stage's session and resume it — the conversation carries the
	// doc + refine discussion, so we only ask Claude to emit the final doc.
	sourceSID, roleKey := docStageSession(requirement, req.DocType)
	docLabel := "技术方案"
	if req.DocType == "coding" {
		docLabel = "开发指令"
	}
	if sourceSID == "" {
		writeError(w, 400, "NO_SESSION", "尚未找到该阶段的会话，请先生成"+docLabel+"后再 apply。")
		return
	}

	projectPath := req.ProjectPath
	defaultBranch := ""
	if proj, perr := h.projectSvc.Get(requirement.ProjectID); perr == nil {
		if projectPath == "" {
			projectPath = proj.LocalPath
		}
		defaultBranch = proj.DefaultBranch
	}
	workDir, err := h.resolveWorkDir(requirement, projectPath, defaultBranch)
	if err != nil {
		writeError(w, 500, "WORKTREE_FAILED", "worktree 创建失败："+err.Error())
		return
	}

	// Build the prompt that asks Claude to emit the final doc. For design docs we
	// detect plan-markdown vs legacy JSON from the DB-stored doc (the apply call
	// doesn't send current_doc) and ask for the matching format.
	var prompt string
	switch req.DocType {
	case "coding":
		prompt = "基于我们的对话，将用户的调整意见整理为给 Claude Code CLI 的开发指令。" +
			"输出纯文本的开发指令，清晰描述需要实现或调整的内容。不要输出 JSON，不要添加额外说明。"
	default: // "design"
		if !isLikelyJSON(requirement.DesignDocs) {
			prompt = "基于我们的对话，将技术方案（plan）更新为最终版本。" +
				"输出完整的 Markdown 技术方案，涵盖：整体实现思路、需要新增或修改的文件、" +
				"具体实现步骤、数据模型/数据库变更、实现风险及应对。直接输出 Markdown，不要添加额外说明。"
		} else {
			prompt = "基于我们的对话，将技术方案更新为最终版本。" +
				"输出 ONLY valid JSON，不要 markdown 代码块：\n" +
				"{\n" +
				"  \"overview\": \"方案概述\",\n" +
				"  \"files\": [\"涉及文件路径\"],\n" +
				"  \"steps\": [\"实现步骤\"],\n" +
				"  \"model_changes\": \"数据模型变更，无则写'无'\",\n" +
				"  \"risks\": [\"实现风险\"]\n" +
				"}"
		}
	}

	systemPrompt, model := h.roleConfig(roleKey)
	// Per-request model override (highest precedence); empty means role default.
	if req.Model != "" {
		model = req.Model
	}

	// Create the job, persist its id so a refresh can reconnect, and return the
	// job id immediately. Claude runs in a goroutine writing progress into the
	// job store.
	job := h.jobs.Create(req.RequirementID)
	if perr := h.reqSvc.UpdateApplyJob(req.RequirementID, job.ID); perr != nil {
		log.Printf("[apply-doc] failed to persist apply_job_id for %s: %v", req.RequirementID, perr)
	}
	writeJSON(w, 200, map[string]string{"job_id": job.ID})

	// Capture the format-relevant fields at launch time; the goroutine reads
	// them off the pointer (they don't change during the apply).
	storedDesignDocs := requirement.DesignDocs
	docType := req.DocType
	reqID := req.RequirementID

	go func() {
		log.Printf("[apply-doc] job %s started for %s (doc_type=%s)", job.ID, reqID, docType)
		job.Append(store.LogLine{Type: "phase", Content: fmt.Sprintf("🤖 Claude 正在基于对话整合修改%s…（预计需要几分钟）", docLabel)})

		cmd := h.llm.StreamCmd(context.Background(), llm.StreamOpts{
			Prompt:       prompt,
			WorkDir:      workDir,
			SystemPrompt: systemPrompt,
			Model:        cliModelArg(model),
			SessionID:    sourceSID,
			Resume:       true,
		})
		applyUsage := h.usageCtxFor("apply_doc", reqID, requirement.ProjectID, job.ID, model, fmt.Sprintf("{\"doc_type\":%q}", docType))
		out := runClaudeStream(jobSink{job}, cmd, "apply-doc", applyUsage)

		if out.staleSession {
			// The stage's conversation is gone. Clear its session id so the user
			// can redo the stage, and surface a recovery hint.
			if docType == "design" {
				_ = h.reqSvc.UpdateDesignSession(reqID, "")
			} else if docType == "coding" {
				_ = h.reqSvc.UpdateCodingSession(reqID, "")
			}
			job.Append(store.LogLine{Type: "error", Content: docLabel + "会话已过期，请重新生成对应文档后再 apply。"})
			_ = h.reqSvc.UpdateApplyJob(reqID, "")
			job.Finish(1, store.JobError)
			return
		}
		if out.finalResult == "" {
			errMsg := out.errMsg
			if errMsg == "" {
				errMsg = "Claude 未返回结果，请重试"
			}
			job.Append(store.LogLine{Type: "error", Content: errMsg})
			_ = h.reqSvc.UpdateApplyJob(reqID, "")
			job.Finish(1, store.JobError)
			return
		}

		finalText := strings.TrimSpace(out.finalResult)

		// For "coding" type: no DB write. The job's message lines already carry
		// the dev instruction; emit a terminal result + done.
		if docType == "coding" {
			job.Append(store.LogLine{Type: "result", Content: finalText})
			job.Append(store.LogLine{Type: "done", Content: "✅ " + docLabel + "已更新！"})
			_ = h.reqSvc.UpdateApplyJob(reqID, "")
			job.Finish(0, store.JobDone)
			return
		}

		// For design docs in plan-markdown format, persist the raw markdown (not
		// extractJSON, which would mangle markdown containing { } chars). Use the
		// DB-stored doc to detect format (apply call doesn't send current_doc).
		var persistVal string
		if !isLikelyJSON(storedDesignDocs) {
			persistVal = finalText
		} else {
			persistVal = extractJSON(out.finalResult)
		}
		if persistVal == "" {
			job.Append(store.LogLine{Type: "error", Content: "未能从 Claude 输出中解析出有效内容，请重试。"})
			_ = h.reqSvc.UpdateApplyJob(reqID, "")
			job.Finish(1, store.JobError)
			return
		}
		if _, saveErr := h.reqSvc.UpdateDesign(reqID, persistVal); saveErr != nil {
			log.Printf("[apply-doc] save failed: %v", saveErr)
			job.Append(store.LogLine{Type: "error", Content: "保存失败: " + saveErr.Error()})
			_ = h.reqSvc.UpdateApplyJob(reqID, "")
			job.Finish(1, store.JobError)
			return
		}
		job.Append(store.LogLine{Type: "done", Content: "✅ " + docLabel + "已更新！"})
		_ = h.reqSvc.UpdateApplyJob(reqID, "")
		job.Finish(0, store.JobDone)
		log.Printf("[apply-doc] job %s finished for %s", job.ID, reqID)
	}()
}

// isLikelyJSON reports whether s looks like a JSON object (starts with '{' after
// trimming whitespace). Used to distinguish plan-markdown design docs from the
// legacy JSON design schema.
func isLikelyJSON(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "{")
}
