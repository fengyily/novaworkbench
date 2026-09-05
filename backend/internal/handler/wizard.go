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
	"regexp"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	promptpkg "github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	gossh "github.com/novaworkbench/backend/internal/ssh"
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
	skillSvc     *service.SkillService
	platformSvc  *service.PlatformTokenService
	// subTaskSvc handles persistence for sub-tasks (manually-triggered child
	// agents that fork the requirement's main-agent session). Optional — when
	// nil the sub-task endpoints are not registered (legacy / standalone
	// deployments without the feature).
	subTaskSvc *service.SubTaskService
	// agentSvrSvc exposes remote Linux/macOS SSH targets whose sealed
	// credentials are used when StartCoding runs against agent_server_id.
	// nil in standalone / non-distributed deployments.
	agentSvrSvc *service.AgentServerService
}

func NewWizardHandler(projectSvc *service.ProjectService, reqSvc *service.RequirementService, knowledgeSvc *service.KnowledgeService, llmGateway *llm.Gateway, jobs *store.JobStore, roleSvc *service.RoleService, jobLogSvc *service.JobLogService, claudeCfg *service.ClaudeConfigService, usageSvc usageRecorder, skillSvc *service.SkillService, platformSvc *service.PlatformTokenService, agentSvrSvc *service.AgentServerService, subTaskSvc *service.SubTaskService) *WizardHandler {
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
		skillSvc:     skillSvc,
		platformSvc:  platformSvc,
		subTaskSvc:   subTaskSvc,
		agentSvrSvc:  agentSvrSvc,
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

// executorRoleKey is the roles-table key for the sub-task executor persona.
// Child agents fork the orchestrator session (developer role = 统筹协调, which
// decomposes and emits [SUBTASKS_READY]), so every child launch must override
// the system prompt with this role or it re-decomposes instead of coding.
const executorRoleKey = "executor"

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
//
// summary is a short Chinese description of the invocation (e.g. the truncated
// text of a 追加调整 request). It is persisted into the meta JSON so the
// requirement-detail token table can show a per-row description. Pass "" for
// steps that don't have a meaningful single-line summary.
func (h *WizardHandler) usageCtxFor(step, requirementID, projectID, jobID, model, meta, summary string) *usageCtx {
	configID, currency := h.activeConfigMeta()
	// Build the snapshot-persist closure up front. snapshotStep maps the usage
	// step to the wizard session whose usage_snapshots entry this turn should
	// update; "" means "don't persist" (compress_* turns — see snapshotStep).
	// The closure captures requirementID + sessionKey and routes to
	// reqSvc.UpdateUsageSnapshot, swallowing errors so a write failure never
	// breaks the claude turn (mirrors usageCtx.recordFrom's best-effort policy).
	var persist func(sessionKey, snapshotJSON string)
	if sessionKey := snapshotStep(step); sessionKey != "" && requirementID != "" && h.reqSvc != nil {
		persist = func(key, snapshotJSON string) {
			if err := h.reqSvc.UpdateUsageSnapshot(requirementID, key, snapshotJSON); err != nil {
				log.Printf("[usage-snapshot] persist failed for %s key=%s: %v", requirementID, key, err)
			}
		}
	}
	return &usageCtx{
		Rec:             h.usageSvc,
		RequirementID:   requirementID,
		ProjectID:       projectID,
		JobID:           jobID,
		Step:            step,
		Model:           model,
		ClaudeConfigID:  configID,
		Currency:        currency,
		Meta:            meta,
		Summary:         summary,
		PersistSnapshot: persist,
	}
}

// snapshotStep maps a usageCtx.Step (the wizard invocation label) to the wizard
// session key whose usage_snapshots JSON entry this turn should update. Multiple
// usage steps touch the SAME session — every coding-adjacent turn (coding /
// adjust_coding / continue_coding / developer_chat) writes into the "coding"
// session because they all --resume the coding_session_id. "" means "don't
// persist": compress_* turns describe the summarize prompt rather than the
// session's real fill and the session is cleared on success anyway, so
// overwriting the snapshot would be misleading.
func snapshotStep(step string) string {
	switch step {
	case "analyst_chat":
		return "analyst_chat"
	case "architect_design":
		return "architect_design"
	case "coding", "adjust_coding", "continue_coding", "developer_chat":
		return "coding"
	}
	return ""
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
	// Validate the project path exists before any git operations. A missing
	// directory normally causes gitRun to fail with a generic error that
	// EnsureWorktree maps to ErrNotAGitRepo, and exec.Cmd.Start() then chdirs
	// to a non-existent path and fails with an opaque ENOENT. In Docker
	// deployments the workspace bind-mount may be empty after a container
	// rebuild, so auto-restore from the project's stored remote before
	// giving up.
	if _, err := os.Stat(projectPath); err != nil {
		if restoreErr := h.projectSvc.EnsureCloned(req.ProjectID); restoreErr != nil {
			return "", fmt.Errorf("project directory not found on this host: %s — %w", projectPath, restoreErr)
		}
		if _, err := os.Stat(projectPath); err != nil {
			return "", fmt.Errorf("project directory not found on this host: %s", projectPath)
		}
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
			base := buildAnalystFirstPrompt(requirement, desc, analysis, req.UserMessage, docBlock, treeSummary)
			if block := promptpkg.AnalystBlock(requirement.Kind, requirement); block != "" {
				return base + "\n\n" + block
			}
			return base
		}

		// context.Background(): the HTTP request has already returned, so we
		// must not tie the claude subprocess's lifetime to r.Context() (which
		// is cancelled the moment the handler returns — that was the bug that
		// killed the turn on page refresh).
		skillsBlock := llm.BuildSkillsBlock(h.mentionedSkills(requirement.Title + " " + requirement.Description))
		origFirstTurnPrompt := firstTurnPrompt
		if skillsBlock != "" {
			firstTurnPrompt = func() string { return skillsBlock + origFirstTurnPrompt() }
			resumePrompt = skillsBlock + resumePrompt
		}
		analystUsage := h.usageCtxFor("analyst_chat", req.RequirementID, requirement.ProjectID, job.ID, model, "", "")
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
	writeSSEHeaders(w)
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

	// Echo the user request as a dedicated SSE event so the output box can
	// render it as a "👤 调整请求" bubble — this is the same content that is
	// persisted into token_usage.meta.summary for the token-stats table.
	if msg := strings.TrimSpace(req.UserMessage); msg != "" {
		payload, _ := json.Marshal(map[string]string{"type": "user_input", "content": msg})
		fmt.Fprintf(w, "data: %s\n\n", payload)
		rc.Flush()
	}
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
		// Coding-stage compression handoff (DeveloperChat first turn): when
		// the user already compressed prior coding turns we want the
		// developer to see the summary as scene-setting. Goes at the very
		// top so the model treats it as ground truth, not as a post-hoc
		// addendum. The disclaimer parenthetical discourages the model from
		// acting on the summary as if it were a fresh instruction.
		if requirement != nil && requirement.CodingContextSummary != "" {
			b.WriteString("## 上下文压缩摘要（之前的开发对话已被压缩，请基于此继续工作，不要当作新指令）\n")
			b.WriteString(requirement.CodingContextSummary)
			b.WriteString("\n\n")
		}
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
		if requirement != nil {
			if block := promptpkg.DeveloperBlock(requirement.Kind, requirement); block != "" {
				b.WriteString("\n\n")
				b.WriteString(block)
			}
		}
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
	developerUsage := h.usageCtxFor("developer_chat", req.RequirementID, developerProjectID, "", model, "", req.UserMessage)
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
		AgentServerID    string `json:"agent_server_id"` // empty = local execution; otherwise remote Agent server
		SplitTasks       bool   `json:"split_tasks"`     // false (default) = developer persona implements directly; true = current decomposition + auto-orchestrate flow
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}

	// Optional "读取项目知识库" switch: when true, the project's knowledge
	// relevant to this requirement is injected into the claude prompt and a
	// "knowledge" SSE event is emitted before the coding job starts.
	readKnowledge := req.ReadKnowledge

	// "是否拆分任务"开关：默认 false（不拆分，由 developer persona 直接实现），
	// 勾选时走原有的拆分子任务 + tryAutoOrchestrate 自动派发链路。Agent-Server
	// 路径（roleKey == "agent"）始终 agentDirectPrompt 直跑，该字段被忽略。
	splitTasks := req.SplitTasks

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

		// Recover the project directory if a Docker rebuild / fresh workspace
		// mount left it absent. Without this, EnsureWorktree below returns
		// ErrNotAGitRepo and the in-place checkout fails with the user-facing
		// "git checkout 失败" error. Re-clone uses the project's stored
		// remote_url + platform token; when there is no remote to restore from
		// EnsureCloned returns a clear error naming the missing path.
		if reqRow != nil {
			if cerr := h.projectSvc.EnsureCloned(reqRow.ProjectID); cerr != nil {
				job.Append(store.LogLine{Type: "error", Content: "❌ " + cerr.Error()})
				job.Finish(1, store.JobError)
				return
			}
		}
		// Defensive guard: an Idea must not reach the coding stage. The
		// frontend hides the "🚀 开始开发" CTA when kind=idea and only Idea
		// rows promoted via "📋 转为需求" can move on, but a stray API call
		// (curl, the legacy /wizard quick-start path, or a future caller) must
		// not silently start coding on a not-yet-defined requirement. Reject
		// with a clear message before any claude subprocess is spawned.
		if reqRow != nil && reqRow.Kind == "idea" {
			job.Append(store.LogLine{Type: "error", Content: "❌ 「想法」类需求暂不支持进入开发阶段，请先在详情页点击「📋 转为需求」升级。"})
			job.Finish(1, store.JobError)
			return
		}

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
		// (`git worktree add` already checked the branch out) and skipped for
		// non-git repos (nothing to check out — proceed in place rather than
		// failing with the misleading "git checkout 失败"). For git repos we
		// mirror EnsureWorktree's robust strategy: create off the base, switch
		// to an already-existing branch, or branch off HEAD so a missing base
		// ref never produces a cryptic error.
		if req.BranchName != "" && !useWorktree {
			if _, gerr := gitRun(branchDir, "rev-parse", "--is-inside-work-tree"); gerr != nil {
				job.Append(store.LogLine{Type: "message", Content: "ℹ️ 非 git 仓库，跳过分支切换，在项目目录直接开发"})
			} else {
				checkoutOK := false
				var lastErrOut string
				attempt := func(args ...string) (string, bool) {
					c := exec.Command("git", args...)
					c.Dir = branchDir
					out, err := c.CombinedOutput()
					trimmed := strings.TrimSpace(string(out))
					if err == nil {
						return trimmed, true
					}
					lastErrOut = trimmed
					return "", false
				}
				// 1. Create the branch off the requested base (typical happy path).
				if out, ok := attempt("checkout", "-b", req.BranchName, baseBranch); ok {
					job.Append(store.LogLine{Type: "message", Content: "🌿 " + out})
					checkoutOK = true
				} else if _, ok := attempt("checkout", req.BranchName); ok {
					// 2. Branch already exists locally — switch to it.
					job.Append(store.LogLine{Type: "message", Content: "🌿 切换到已有分支: " + req.BranchName})
					checkoutOK = true
				} else if out, ok := attempt("checkout", "-b", req.BranchName); ok {
					// 3. Base ref missing locally — fall back to branching off HEAD
					//    (matches EnsureWorktree's strategy).
					job.Append(store.LogLine{Type: "message", Content: "🌿 " + out})
					checkoutOK = true
				}
				if !checkoutOK {
					job.Append(store.LogLine{Type: "error", Content: "❌ git checkout 失败: " + lastErrOut})
					job.Finish(1, store.JobError)
					return
				}
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

		// Role selection:
		//   • "agent" — Agent-Server execution (remote) OR local with split_tasks=false
		//     (the user explicitly chose "不拆分任务"). The agent persona's system
		//     prompt says "不要先拆分子任务" + "一次会话内完成端到端开发", which is
		//     exactly the behavior we want when the main agent is supposed to
		//     implement the requirement itself.
		//   • "developer" — local execution with split_tasks=true (default = true
		//     for legacy / no-split-switch callers). The developer persona is the
		//     统筹协调者 that decomposes into sub-tasks + emits [SUBTASKS_READY]
		//     so tryAutoOrchestrate dispatches children.
		//
		// Why not "developer" + a "直接实现" -p override when split_tasks=false?
		// The developer system prompt explicitly says "**不要直接编写项目代码**——
		// 所有具体实现工作由子Agent完成" and has the "## 何时拆分任务" block keyed
		// on "进入执行实现阶段"-style triggers; even with the prompt rewritten to
		// "直接实现需求", models reflexively follow the system prompt and split
		// anyway, defeating the user's "不拆分" choice. Routing through the agent
		// role is the only reliable fix: its persona + system prompt consistently
		// say "don't decompose, implement end-to-end" (see role_defaults.go).
		roleKey := "developer"
		if req.AgentServerID != "" || !req.SplitTasks {
			roleKey = "agent"
		}
		systemPrompt, model := h.roleConfig(roleKey)
		// Per-request model override (highest precedence); empty means role default.
		if req.Model != "" {
			model = req.Model
		}
		job.SetModel(model)
		var prompt string
		if sourceSID == "" {
			// Fresh-session path: no design/analysis session to fork (skip-design
			// "直接开发" rows, or legacy rows without session chaining). This
			// branch used to feed a bare "## title\n\n desc" which is a generic
			// "请实现该需求" prompt — and the developer role's system prompt
			// only emits the [SUBTASKS_READY] sentinel when the -p message
			// carries an explicit "开始开发/进入执行实现阶段" trigger. Without
			// that trigger the agent did the work itself and auto-orchestration
			// never fired (see req_04acb22d06fe3525). So we now send the SAME
			// decomposition trigger as the fork branch — the only difference is
			// wording: there is no "已完成的需求分析与技术方案" to reference, so we
			// ask the agent to read the relevant files first to build context.
			job.Append(store.LogLine{Type: "message", Content: "ℹ️ 未关联需求会话，使用独立会话开始开发。"})
			if roleKey == "agent" {
				// agent persona (Agent-Server remote OR local split_tasks=false):
				// implement the requirement directly end-to-end. No decomposition
				// trigger in the -p message, no [SUBTASKS_READY] expected, no
				// .novaworkbench/subtasks.json Write. The agent system prompt says
				// "不要先拆分子任务" and "一次会话内完成端到端开发", so the model
				// consistently implements instead of splitting.
				prompt = agentDirectPrompt(req.RequirementTitle,
					"请先读取项目中的相关文件理解现有代码结构与需求上下文，然后直接实现需求：\n", workDir)
			} else {
				// developer persona + fresh-session path + split_tasks=true.
				// Keep the original decomposition trigger so the developer role
				// emits [SUBTASKS_READY] + writes .novaworkbench/subtasks.json
				// and tryAutoOrchestrate dispatches children. The split_tasks=false
				// fresh-session case is handled by the roleKey=="agent" branch
				// above (we route those requests through the agent role entirely).
				prompt = developerDecomposePrompt(req.RequirementTitle,
					"请先读取项目中的相关文件理解现有代码结构与需求上下文，然后立即完成**任务拆分**：\n", workDir)
			}
			if desc := strings.TrimSpace(req.RequirementDesc); desc != "" {
				if roleKey == "agent" {
					prompt += "\n\n用户在开发前的追加说明：\n" + desc
				} else {
					prompt += "\n\n用户在开发前的追加说明：\n" + desc
				}
			}
			// Context-compression handoff (legacy fresh-session path): when
			// the coding stage was previously compressed we still want the
			// summary prepended so the new coding session inherits the
			// compressed history. This branch covers pre-existing rows that
			// never had a design_session_id (and the rare user who hits
			// "重新开发" after a design session was already compressed and
			// invalidated). The summary goes at the TOP so the developer
			// treats it as ground truth; the parenthetical tells the model
			// not to act on it as if it were a fresh instruction.
			if reqRow != nil && reqRow.CodingContextSummary != "" {
				prompt = "## 上下文压缩摘要（之前的开发对话已被压缩，请基于此继续工作，不要当作新指令）\n" +
					reqRow.CodingContextSummary + "\n\n" + prompt
			}
		} else {
			// The resumed conversation carries the requirement, analysis, and
			// design. The developer role is now a coordinator (统筹协调者) that
			// does NOT write code itself — it decomposes the work into sub-tasks
			// and emits a [SUBTASKS_READY] sentinel so tryAutoOrchestrate (called
			// at the end of this job) dispatches each child agent. The -p prompt
			// MUST carry the explicit "进入执行实现阶段" trigger that the
			// developer system prompt keys its JSON+sentinel emission on — a
			// generic "请实现该需求" does NOT hit that branch, so the agent hedges
			// into a prose plan + "等待确认" and auto-orchestration never fires.
			// The per-turn -p instruction also overrides the system prompt's
			// "always ask for confirmation" guidance so the agent emits the
			// sentinel immediately instead of waiting on the user.
			if roleKey == "agent" {
				// agent persona (Agent-Server remote OR local split_tasks=false):
				// fork/resume variant — the conversation already carries the
				// requirement + analysis + design, so the leadIn references that
				// history instead of asking the agent to re-read files.
				prompt = agentDirectPrompt(req.RequirementTitle,
					"基于已完成的需求分析与技术方案，请直接实现需求：\n", workDir)
			} else {
				// developer persona + fork/resume path + split_tasks=true.
				// Decompose the work into sub-tasks so tryAutoOrchestrate (called
				// below, gated on splitTasks) can dispatch children. The
				// split_tasks=false fork/resume case is handled by roleKey=="agent"
				// above — we route those requests through the agent role entirely.
				prompt = developerDecomposePrompt(req.RequirementTitle,
					"基于已完成的需求分析与技术方案，请立即完成**任务拆分**：\n", workDir)
			}
			if desc := strings.TrimSpace(req.RequirementDesc); desc != "" {
				if roleKey == "agent" {
					prompt += "\n\n用户在开发前的追加调整说明：\n" + desc
				} else {
					prompt += "\n\n用户在开发前的追加调整说明：\n" + desc
				}
			}
			// Coding-stage compression handoff: even when resuming the design
			// session, the prior coding turns may have been compressed. The
			// summary goes at the TOP so the developer treats it as ground
			// truth; the parenthetical tells the model not to act on it as if
			// it were a fresh instruction.
			if reqRow != nil && reqRow.CodingContextSummary != "" {
				prompt = "## 上下文压缩摘要（之前的开发对话已被压缩，请基于此继续工作，不要当作新指令）\n" +
					reqRow.CodingContextSummary + "\n\n" + prompt
			}
		}
		// Kind-specific developer tail (currently only fires for kind=issue).
		// Idea never reaches here — the frontend hides the "开始开发" CTA and
		// we double-protect by checking reqRow.Kind below.
		if reqRow != nil {
			if block := promptpkg.DeveloperBlock(reqRow.Kind, reqRow); block != "" {
				prompt += "\n\n" + block
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
		skillText := ""
		if reqRow != nil {
			skillText = reqRow.Title + " " + reqRow.Description
		}
		if block := llm.BuildSkillsBlock(h.mentionedSkills(skillText)); block != "" {
			prompt = block + prompt
		}
		// Inject the project's git committer identity (from its platform
		// token) as GIT_AUTHOR_*/GIT_COMMITTER_* env into the claude
		// subprocess. git reads these env vars over any config, so when the
		// developer role runs `git commit` via its Bash tool it carries a
		// real identity on hosts without ~/.gitconfig (e.g. the Docker
		// container). Empty on miss → no injection, git falls back to its
		// own config lookup (preserves dev-machine behaviour). Mirrors
		// MergeHandler.gitIdentityForReq via the shared lookupGitIdentity.
		var codingExtraEnv []string
		if name, email := lookupGitIdentity(h.projectSvc, h.platformSvc, reqRow); name != "" || email != "" {
			if name != "" {
				codingExtraEnv = append(codingExtraEnv, "GIT_AUTHOR_NAME="+name, "GIT_COMMITTER_NAME="+name)
			}
			if email != "" {
				codingExtraEnv = append(codingExtraEnv, "GIT_AUTHOR_EMAIL="+email, "GIT_COMMITTER_EMAIL="+email)
			}
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
			ExtraEnv:      codingExtraEnv,
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
		codingUsage := h.usageCtxFor("coding", req.RequirementID, codingProjectID, job.ID, model, "", "")

		// Remote Agent-server branch: SSHs into the target, syncs the claude
		// session dir so --resume works, executes the same claude flag list on
		// the remote worktree, parses the stream-json output, and pushes the
		// code back to origin. The job log + final result semantics match the
		// local path so the frontend doesn't have to special-case anything.
		if req.AgentServerID != "" && h.agentSvrSvc != nil {
			out := h.runRemoteCoding(&remoteCodingInput{
				job:      job,
				serverID: req.AgentServerID,
				req: startCodingReq{
					ProjectPath:      req.ProjectPath,
					RequirementTitle: req.RequirementTitle,
					RequirementDesc:  req.RequirementDesc,
					RequirementID:    req.RequirementID,
					BranchName:       req.BranchName,
					BaseBranch:       req.BaseBranch,
					Model:            req.Model,
					ReadKnowledge:    req.ReadKnowledge,
					AgentServerID:    req.AgentServerID,
				},
				reqRow:        reqRow,
				prompt:        prompt,
				workDir:       workDir,
				sourceSID:     sourceSID,
				fork:          fork,
				sessionArg:    sessionArg,
				forkSessionID: forkSessionID,
				model:         model,
				usage:         codingUsage,
			})
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
			job.Append(store.LogLine{Type: "done", Content: "✅ 开发完成！"})
			if req.RequirementID != "" {
				if perr := h.reqSvc.UpdateDeveloperModel(req.RequirementID, model); perr != nil {
					log.Printf("[start-coding] failed to persist developer_model for %s: %v", req.RequirementID, perr)
				}
			}
			job.Finish(0, store.JobDone)
			log.Printf("[start-coding] remote job %s finished status=%s exit=%d", job.ID, job.Status, job.ExitCode)
			return
		}

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
		// Capture the main agent's task breakdown (if any) into coding_plan
		// so SubTaskPanel can show it as the suggested sub-task list. The
		// helper accepts both sentinel-wrapped and "## 任务分解"-heading
		// forms; empty result leaves coding_plan unchanged (we don't want
		// to wipe a previously-persisted plan when the agent happens to
		// re-run without emitting one).
		if req.RequirementID != "" {
			if plan := extractCodingPlan(out.finalResult); plan != "" {
				if perr := h.reqSvc.UpdateCodingPlan(req.RequirementID, plan); perr != nil {
					log.Printf("[start-coding] failed to persist coding_plan for %s: %v", req.RequirementID, perr)
				} else {
					job.Append(store.LogLine{Type: "message", Content: "📋 已捕获主Agent任务分解，存入 coding_plan"})
				}
			}
		}
		// Record the effective developer model (success path only).
		if req.RequirementID != "" {
			if perr := h.reqSvc.UpdateDeveloperModel(req.RequirementID, model); perr != nil {
				log.Printf("[start-coding] failed to persist developer_model for %s: %v", req.RequirementID, perr)
			}
		}
		job.Finish(0, store.JobDone)
		log.Printf("[start-coding] job %s finished status=%s exit=%d", job.ID, job.Status, job.ExitCode)

		// === AUTO-ORCHESTRATE ============================================
		// 主 Agent 在 start-coding 阶段已经掌握需求 / 设计 / 项目上下文。
		// 用户希望"一键编排 = 主 Agent 自动派发"——main agent 一返回
		// finalResult，立刻交给 tryAutoOrchestrate：有 [SUBTASKS_READY]
		// sentinel + JSON 时串行派发子 Agent + 异步汇总；没命中就把
		// coding_plan 当作普通任务分解展示，但不派发（保持现有行为）。
		//
		// Agent-Server 路径走 "agent" 角色，该角色的 system prompt 与 -p 指令
		// 都不要求 [SUBTASKS_READY] 哨兵 / subtasks.json；为了一致性直接跳过
		// orchestrator（不调用，即便没有 sentinel 也会安全 no-op，但调用
		// 本身会引入无谓的 goroutine + 日志噪音）。
		//
		// 当 split_tasks=false（用户选择"不拆分任务"）时，StartCoding 已经把
		// roleKey 切到 "agent" 并使用 agentDirectPrompt，-p 消息不携带任何触发
		// 语、agent 的 system prompt 也明确"不要拆分子任务"；此时再调
		// tryAutoOrchestrate 只会扫到空 payload 然后空转派发 0 个子任务，
		// 等价于一次 no-op，但仍然多开一个 goroutine + 一段 resolveSubtasksPayload
		// 的日志噪音，所以一并短路。split_tasks=true + 本地 = roleKey=="developer"，
		// 走原 developerDecomposePrompt + 派发链路，行为与改动前一致。
		//
		// 该调用改用独立 goroutine，不阻塞 start-coding 自身的 job_done
		// 信号，用户的开发启动 SSE 立即结束；子任务的进度仍由
		// dispatchOneChild 的 JobStore job 推流。
		if roleKey != "agent" && req.RequirementID != "" && newCodingSID != "" && h.subTaskSvc != nil && splitTasks {
			go h.tryAutoOrchestrate(req.RequirementID, newCodingSID, out.finalResult, out.subTasksJSON, reqRow, workDir, model)
		}
	}()
}

// AdjustCoding starts a background JobStore job that resumes the prior coding
// session (--resume coding_session_id) to apply a follow-up adjustment to
// already-implemented code. Because the resumed session already carries the
// requirement, analysis, design, and the persona set by StartCoding
// (developer for local execution; agent for Agent-Server execution), we send
// ONLY the user's follow-up message as -p and inject NEITHER the role system
// prompt NOR the readProjectContext project context — re-feeding them would be
// redundant and could distort the resumed conversation. The model field is
// honored (--model) so the user's latest setting applies; we keep the lookup
// against the developer role for backward compat (the resumed session's
// persona is what determines behaviour — model only affects token routing).
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

		// Echo the user request so the job-stream SSE can render a "👤 调整请求"
		// bubble before the first tool/phase line. The same text is persisted
		// into token_usage.meta.summary for the project/requirement token table.
		if msg := strings.TrimSpace(body.Message); msg != "" {
			job.Append(store.LogLine{Type: "user_input", Content: msg})
		}

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
		adjustPrompt := body.Message
		if block := llm.BuildSkillsBlock(h.mentionedSkills(req.Title + " " + req.Description + " " + body.Message)); block != "" {
			adjustPrompt = block + body.Message
		}
		// Tail the kind-specific developer block so a resumed Issue session
		// stays anchored to "最小改动、修复根因" framing on every follow-up
		// turn. For requirement rows it's a no-op.
		if block := promptpkg.DeveloperBlock(req.Kind, req); block != "" {
			adjustPrompt += "\n\n" + block
		}
		cmd := h.llm.GenerateCode(llm.StreamOpts{
			Prompt:       adjustPrompt,
			WorkDir:      workDir,
			SystemPrompt: "", // resume 已携带 developer persona，不再注入
			Model:        cliModelArg(model),
			SessionID:    req.CodingSessionID,
			Resume:       true,
			Fork:         false,
		})
		adjustUsage := h.usageCtxFor("adjust_coding", body.RequirementID, req.ProjectID, job.ID, model, "", body.Message)
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
		// Kind-specific developer tail — keeps an Issue session's framing
		// ("最小改动、修复根因") present on every continue round.
		if block := promptpkg.DeveloperBlock(req.Kind, req); block != "" {
			prompt += "\n\n" + block
		}
		cmd := h.llm.GenerateCode(llm.StreamOpts{
			Prompt:       prompt,
			WorkDir:      workDir,
			SystemPrompt: "", // resume 已携带 developer persona，不再注入
			Model:        cliModelArg(model),
			SessionID:    req.CodingSessionID,
			Resume:       true,
			Fork:         false,
		})
		continueUsage := h.usageCtxFor("continue_coding", body.RequirementID, req.ProjectID, job.ID, model, "", "")
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

	streamJobSSE(w, r, job, func(status store.JobStatus, exitCode int) []byte {
		doneData, _ := json.Marshal(map[string]interface{}{
			"type":        "job_done",
			"status":      string(status),
			"exit_code":   exitCode,
			"started_at":  job.StartedAt.UnixMilli(),
			"finished_at": job.FinishedAt.UnixMilli(),
			"duration_ms": job.FinishedAt.Sub(job.StartedAt).Milliseconds(),
		})
		return doneData
	})
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
		// Context-compression handoff (fresh-session path only): when the
		// design stage was previously compressed, the requirement carries a
		// Chinese summary we want the architect to see as scene-setting
		// context. We only inject on the fresh-session path because the
		// resume path (skipAnalysis==false) inherits the analyst conversation
		// natively via --resume, where the prior design summary isn't
		// applicable. The prefix also goes BEFORE the rest of the prompt so
		// the model treats it as ground truth rather than as a post-hoc
		// addendum, and the parenthetical disclaimer discourages the model
		// from acting on it as if it were a fresh instruction.
		if req.DesignContextSummary != "" {
			prompt = "## 上下文压缩摘要（之前的方案设计对话已被压缩，请基于此继续工作，不要当作新指令）\n" +
				req.DesignContextSummary + "\n\n" + prompt
		}
	}
	// Tail: append the kind-specific block (Issue / Idea framing). For an Idea
	// the user would normally have hidden this CTA in the frontend; we still
	// inject the block defensively so an out-of-band call (e.g. curl, the
	// wizard page, or a future "重新生成技术方案" path) sees consistent
	// guidance. For Requirement the block is empty.
	if block := promptpkg.ArchitectBlock(req.Kind, req); block != "" {
		prompt += "\n\n" + block
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
		if block := llm.BuildSkillsBlock(h.mentionedSkills(req.Title + " " + req.Description)); block != "" {
			prompt = block + prompt
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
		out := runClaudeStream(jobSink{job}, cmd, "architect-design", h.usageCtxFor("architect_design", id, req.ProjectID, job.ID, model, "", ""))

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
	jsonLine, _ := json.Marshal(map[string]interface{}{
		"type":    typ,
		"content": content,
		"at":      time.Now().UnixMilli(),
	})
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

// extractStreamError pulls every diagnostic field we can out of a top-level
// {"type":"error",...} NDJSON event. The Claude CLI and upstream proxies
// each use their own conventions for the message:
//   - claude CLI: {"type":"error","error":"<string>"} or
//     {"type":"error","error":{"message":"<string>", ...}}
//   - third-party relays sometimes nest as {"type":"error","message":"..."}
//   - some payloads carry a sibling "api_error_status" / "error_type" hint
//
// nova-agent-worker additionally serializes a few extra fields from the
// non-zero child-process exit: code / signal / stderr. The CLI's actual
// error line ("401 Unauthorized", "ENOTFOUND api.anthropic.com", "model
// not found") lives in `stderr` and is by far the most useful diagnostic —
// we surface it after the top-level message, capped to ~1.5KB so a chatty
// CLI can't blow up the SSE envelope. When the message / stderr hint at an
// upstream 400 / not found, append the same proxy-config guidance
// claudeResultError does.
//
// The worker also attaches an `errorCategory` (computed via its
// classifyError) — auth_failed / network_unreachable / model_not_found /
// unrecognized_model / etc. — that we map to a tailored Chinese fix hint
// appended after stderr. This is what turns the previously opaque
// "Claude Code process exited with code 1" into actionable guidance.
func extractStreamError(evt map[string]interface{}) string {
	var msg string
	switch v := evt["error"].(type) {
	case string:
		msg = v
	case map[string]interface{}:
		if s, ok := v["message"].(string); ok && s != "" {
			msg = s
		}
	}
	if msg == "" {
		if s, ok := evt["message"].(string); ok && s != "" {
			msg = s
		}
	}
	if msg == "" {
		return ""
	}

	// Append the CLI's stderr (if the worker captured it) — this is where
	// the actionable diagnostic usually is. Truncate to 1.5KB to keep the
	// SSE message readable; the full text is also available in the backend
	// log via the raw json.Marshal below.
	if stderr, ok := evt["stderr"].(string); ok && strings.TrimSpace(stderr) != "" {
		stderr = strings.TrimSpace(stderr)
		if len(stderr) > 1500 {
			stderr = stderr[:1500] + "\n…[truncated]"
		}
		msg += "\n[stderr]\n" + stderr
	}

	// Append the exit code if present, for quick scanning.
	if code, ok := evt["code"]; ok {
		msg += fmt.Sprintf("\n[exit_code] %v", code)
	} else if code, ok := evt["exitCode"]; ok {
		msg += fmt.Sprintf("\n[exit_code] %v", code)
	}

	// Append nested cause (one level) — sometimes an upstream relay wraps
	// a transport error inside a higher-level Error and only the inner
	// one names the host. Kept for forward-compat with the previous
	// SDK-shaped payloads an older worker may still emit.
	if cause, ok := evt["cause"].(string); ok && cause != "" {
		msg += "\n[cause] " + cause
	}

	// Worker-classified category → tailored fix hint. When the worker emits
	// a preflight failure we hoist the hint above the generic 400 check so
	// the user sees the actionable reason first. Categories must match the
	// strings nova-agent-worker/server.mjs:classifyError emits; if a new
	// category is added there, add a hint here too.
	if cat, _ := evt["errorCategory"].(string); cat != "" {
		if hint := workerCategoryHint(cat, msg); hint != "" {
			msg += "\n[诊断] " + hint
		}
	}

	lower := strings.ToLower(msg)
	if strings.Contains(lower, "400") &&
		(strings.Contains(lower, "bad request") || strings.Contains(lower, "not found")) {
		msg += "\n（这是上游代理返回的 400，通常是 BASE_URL / Token / 模型 配置有误或额度耗尽，请在「设置」里检查 Claude 配置）"
	}
	if raw, err := json.Marshal(evt); err == nil {
		log.Printf("[stream-error] %s", truncateStr(string(raw), 1200))
	}
	return msg
}

// workerCategoryHint maps nova-agent-worker's errorCategory to a Chinese
// fix hint. Keep the categories in sync with classifyError in server.mjs
// (the worker writes the strings verbatim — a typo here silently breaks
// the hint without surfacing).
//
// Hint scope: actionable and bounded. We deliberately do NOT explain every
// possible root cause (the stderr already does that); we tell the user
// what knob to turn next.
func workerCategoryHint(cat, msg string) string {
	switch cat {
	case "cli_not_found":
		return "Claude CLI 未找到。请在 Agent 服务器上确认 `claude --version` 可执行，或重新「安装依赖」。"
	case "auth_failed":
		return "鉴权失败（401）。请在「设置 → Claude 配置」检查 ANTHROPIC_AUTH_TOKEN 是否已填写并生效。"
	case "auth_forbidden":
		return "权限不足（403）。Token 可能有效但缺少调用该模型的权限，或 base URL 指向了无权访问的端点。"
	case "model_not_found":
		return "上游 API 不认识这个 model（404）。请检查「设置 → Claude 配置」里的 model 与 base URL，或确认模型名拼写正确。"
	case "unrecognized_model":
		// We pass --model MiniMax-M3 (or similar custom id) plus the env
		// var CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1 to
		// suppress Claude Code's "model isn't in my local catalog, I'll
		// assume 200k context and might fail" warning — see gateway.go:
		// BuildEnvPairs. If the warning still surfaces here, either the
		// env var didn't reach the worker (stale server.mjs) or the CLI
		// version on the agent server doesn't honor that knob.
		//
		// The `[1m]` / `[0m]` markers in the stderr are Claude Code's
		// own ANSI color escapes leaking into the JSON it emits — a CLI
		// bug, not our model name. The actual id is whatever's set in
		// 「设置 → Claude 配置」.
		return "Claude Code 不在本地 model 目录里认识这个 model（自定义 model 走私有 base URL 时常见）。gateway.go 已自动注入 CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1 让 CLI 跳过 catalog 检查，但本机仍报此错通常意味着：(1) worker 还在跑旧版 server.mjs，没拿到新的 env（请在「设置 → Agent 服务器」点「安装依赖」）；(2) Agent 服务器上的 Claude Code 版本过旧不识别该 env 变量（请运行 `claude --version` 升级）。详细：stderr 里的 `[1m]`/`[0m` 是 Claude Code CLI 自带的 ANSI 颜色控制符泄漏进 JSON，不是 model 名真的带这些字符。"
	case "rate_limited":
		return "上游限流（429）。请稍候几分钟重试，或降低并发。"
	case "quota_exceeded":
		return "账户额度耗尽。请充值或换用其他 Claude 配置后重试。"
	case "network_unreachable":
		return "Agent 服务器无法访问到上游 API。请检查出口网络/防火墙/代理。"
	case "dns_unresolved":
		return "Agent 服务器 DNS 解析失败。请检查 /etc/resolv.conf 或上游 base URL 域名拼写。"
	case "connection_refused":
		return "上游连接被拒（ECONNREFUSED）。通常是 base URL 端口错，或上游服务未启动。"
	case "connection_reset":
		return "上游连接被重置（ECONNRESET）。通常是代理或防火墙打断了长连接，重试即可。"
	case "permission_denied":
		// When the EACCES path is /var/folders the real cause is almost
		// always that $TMPDIR was inherited from a macOS dev box and
		// forwarded into the SSH session on a Linux/Windows agent host
		// where /var/folders doesn't exist. claude's internal tmpdir
		// setup then EACCESes on the missing parent before --print ping
		// even starts. nova-agent-worker now auto-overrides TMPDIR
		// (existing → $HOME → /tmp → cwd → bare /tmp) before each
		// spawn, and the systemd/launchd unit + nohup launcher all pin
		// TMPDIR=/tmp so the worker process itself starts with a sane
		// tmpdir. This hint therefore usually points at the worker not
		// having picked up the new server.mjs yet (i.e. re-Install is
		// the missing step), or at a stale node process holding the
		// old in-memory code after a partial install.
		if strings.Contains(msg, "/var/folders") {
			return "Claude 进程的 $TMPDIR 指向 /var/folders，但该路径在 Agent 服务器上不存在（开发机是 macOS，$TMPDIR 跟随 SSH 会话转发到了 Linux 远端）。worker 已自动覆盖 TMPDIR（现有 → $HOME → /tmp → 兜底 /tmp），仍报错通常是 worker 还没拉到新版 server.mjs 或 systemd 未重启。请 SSH 到 Agent 服务器执行 `systemctl --user restart nova-agent-worker.service`（或在「设置 → Agent 服务器」点一次「安装依赖」），然后查看 ~/nova-agent-worker/worker.log 中 `[nova-agent-worker] resolved TMPDIR via …` 那行确认 TMPDIR 已切到 /home/<user>/.nova-agent-worker-XXXX 或 /tmp/.nova-agent-worker-XXXX。若日志显示 `/tmp` 兜底分支，说明 $HOME 不可写，请检查 Agent 服务器上 nova-agent-worker 进程对 $HOME 目录是否有写权限。"
		}
		return "本地文件系统权限不足（EACCES）。请检查 worktree 路径对当前 SSH 用户是否可写。"
	case "session_not_found":
		return "找不到要 resume 的会话。本地会话 jsonl 未上传到 Agent 服务器，或 slug 不匹配，建议重新分析或开新会话。"
	case "max_turns":
		return "Claude 达到单轮最大工具调用次数。请把需求拆小，或在提示词里限制工具调用总数。"
	case "preflight_timeout":
		return "preflight 5 秒内未完成（`claude --print ping` 卡住）。通常是 Agent 服务器无法访问 API，请检查网络。"
	}
	return ""
}

// claudeStreamOutcome is the result of running one claude stream-json command to
// completion. finalResult holds the "result" event text on success. On failure
// errMsg is a human-readable message; staleSession is true when the failure was
// specifically a --resume against a conversation that no longer exists on disk,
// so the caller can transparently fall back to a fresh session instead of
// surfacing a hard error.
type claudeStreamOutcome struct {
	finalResult      string
	sessionID        string // session_id of this run, read from the system/init event. For a --fork-session run this is the NEW forked id.
	staleSession     bool
	errMsg           string
	hadStreamEvents  bool   // true if any stream_event/content_block_delta arrived
	eventCount       int    // total NDJSON events parsed (any type)
	streamEventCount int    // subset that are stream_event
	lastEventType    string // type field of the most recent event, used for EOF postmortem
	planContent      string // full markdown captured from a plan-mode Write tool_use to ~/.claude/plans/*.md
	// subTasksJSON is the authoritative sub-task decomposition payload,
	// captured from a Write tool_use whose target path ends with
	// /.novaworkbench/subtasks.json. Unlike the free-text JSON block +
	// [SUBTASKS_READY] sentinel, the Write tool_use input arrives as one
	// structured event — immune to embedded code fences, truncation, or
	// markdown mangling (req_9d24ef181a5ad5c4). tryAutoOrchestrate prefers
	// this over every text-parsing fallback.
	subTasksJSON string
	actualModel  string   // model id returned by the API, captured from the assistant event's message.model
	toolFiles    []string // file paths / patterns touched by Read/Write/Edit/Grep/Glob tool calls (for knowledge-usage evaluation)
	// lastUsage captures the four token counts from the terminal result event
	// (or zero values when the stream ended before reaching a result). The
	// compress-context handler reads this to populate the `done` payload's
	// tokens_used field without a second DB roundtrip.
	lastUsage lastUsageSnapshot
}

// lastUsageSnapshot is the token-count view of a single claude turn, derived
// from the result.usage block. Stored on claudeStreamOutcome so handlers that
// need to surface the cost of a turn (compress-context modal, future SSE
// telemetry) can read it without re-extracting from the original event.
type lastUsageSnapshot struct {
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
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
	if line.At == 0 {
		line.At = time.Now().UnixMilli()
	}
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
	// Single concise startup line in the happy path so the server log doesn't
	// flood with LookPath / EvalSymlinks / stat / magic / ldd dumps every
	// time a sub-task forks. The full diagnostic only fires when Start()
	// fails — that's the case that actually needs the snapshot to pinpoint
	// the cause (ENOENT alone is ambiguous).
	log.Printf("[%s] claude 启动中 (binary=%s args=%d)", scope, filepath.Base(cmd.Path), len(cmd.Args))
	if err := cmd.Start(); err != nil {
		logClaudeExecDiag(scope, cmd)
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
							// Primary orchestration channel: the developer main
							// agent is asked to Write its decomposition to
							// .novaworkbench/subtasks.json. Capture the content
							// from the structured tool_use input instead of
							// parsing the assistant's free text.
							if strings.HasSuffix(filepath.ToSlash(fp), "/"+subTasksFileRelPath) {
								if c, ok := input["content"].(string); ok && c != "" {
									out.subTasksJSON = c
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

			// Stash the same counts onto the outcome so handlers (compress-
			// context, future telemetry) can read the cost of THIS turn
			// without going back to the original stream event.
			if inTok, outTok, cc, cr, ok := llm.ParseStreamUsage(evt); ok {
				out.lastUsage = lastUsageSnapshot{
					InputTokens:         inTok,
					OutputTokens:        outTok,
					CacheCreationTokens: cc,
					CacheReadTokens:     cr,
				}
			}

			// Mirror the same usage data through SSE so the wizard chat panel
			// can render a live "上下文 X%" bar without polling /api/usage/*.
			// We emit even on failure paths (realSuccess==false) because the
			// model still spent tokens before erroring out — those tokens
			// count against the user's budget just the same. Only skip when
			// the event carried no usage at all (ParseStreamUsage.ok=false).
			if sink != nil && uctx != nil {
				if inTok, outTok, cc, cr, ok := llm.ParseStreamUsage(evt); ok && (inTok+outTok+cc+cr) > 0 {
					modelName := uctx.Model
					if modelName == "" {
						modelName = out.actualModel
					}
					payload := map[string]any{
						"step":                  uctx.Step,
						"model":                 modelName,
						"input_tokens":          inTok,
						"output_tokens":         outTok,
						"cache_creation_tokens": cc,
						"cache_read_tokens":     cr,
						"context_window":        service.ModelContextWindow(modelName),
					}
					if b, mErr := json.Marshal(payload); mErr == nil {
						sink.emit(store.LogLine{Type: "usage", Content: string(b)})
						// Same payload, second outlet: persist into the
						// requirements.usage_snapshots blob so the frontend can
						// seed its usage bars from the Requirement GET and
						// survive a page refresh / panel collapse instead of
						// dropping to 0%. snapshotStep maps the usage step to
						// the wizard session; "" (compress_* turns) skips. The
						// closure swallows its own errors — never breaks the
						// stream. The SSE payload carries `step` (the usage
						// label) while the snapshot is keyed by session, so we
						// rebuild the snapshot value without `step`.
						if uctx.PersistSnapshot != nil {
							if key := snapshotStep(uctx.Step); key != "" {
								snap := map[string]any{
									"model":                 modelName,
									"input_tokens":          inTok,
									"output_tokens":         outTok,
									"cache_creation_tokens": cc,
									"cache_read_tokens":     cr,
									"context_window":        service.ModelContextWindow(modelName),
								}
								if sb, sErr := json.Marshal(snap); sErr == nil {
									uctx.PersistSnapshot(key, string(sb))
								}
							}
						}
					}
				}
			}
			gotResult = true
		case "error":
			// stream-json sometimes emits a top-level error event before any
			// result/system (e.g. claude CLI exited non-zero on startup, auth
			// rejected at the proxy, model rejected, network DNS failure).
			// Without this case the error body is silently dropped and the
			// EOF branch only knows lastEventType="error" — leaving the user
			// guessing among "API hung", "401", "OOM", "argv truncated". The
			// CLI and any relay both use a string `error` field; some
			// payloads nest it as an object with a `message` sub-field, so
			// accept both shapes.
			msg := extractStreamError(evt)
			if msg != "" {
				if out.errMsg == "" {
					out.errMsg = msg
				}
				sink.emit(store.LogLine{Type: "error", Content: msg})
			}
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

// ---- Remote Agent-server execution -----------------------------------------

// remoteCodingInput is the bag of pre-computed values the StartCoding
// goroutine hands to runRemoteCoding. Pulling these into a struct keeps the
// signature readable and forces callers to acknowledge the same dependencies
// the local branch already resolved (prompt, workDir, session threading).
type remoteCodingInput struct {
	job           *store.Job
	serverID      string
	req           startCodingReq
	reqRow        *model.Requirement
	prompt        string
	workDir       string // local worktree path (used only for SFTP upload source)
	sourceSID     string
	fork          bool
	sessionArg    string
	forkSessionID string
	model         string
	usage         *usageCtx
}

// startCodingReq mirrors the anonymous struct StartCoding decodes so the
// remote helper doesn't have to redefine field tags. Keeping this as a named
// type keeps runRemoteCoding self-documenting.
type startCodingReq struct {
	ProjectPath      string
	RequirementTitle string
	RequirementDesc  string
	RequirementID    string
	BranchName       string
	BaseBranch       string
	Model            string
	ReadKnowledge    bool
	AgentServerID    string
}

// runRemoteCoding is the Agent-server equivalent of the local runClaudeStream
// block in StartCoding. It opens an SSH session, ensures the project lives in
// a per-requirement git worktree under /tmp/nova-agent/<projectID>/<reqID>,
// uploads the local claude session dir so --resume picks up the right jsonl,
// runs the same claude CLI invocation, parses the stream-json output, then
// pushes the resulting commits back to origin and re-syncs the session dir
// back to local. Returns the same claudeStreamOutcome shape as the local
// path so the caller can reuse its terminal-state handling.
func (h *WizardHandler) runRemoteCoding(in *remoteCodingInput) claudeStreamOutcome {
	if h.agentSvrSvc == nil {
		return claudeStreamOutcome{errMsg: "Agent 服务器服务未初始化"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()

	// Load the (decrypted) credential before anything else — a missing master
	// key surfaces here as a clear error instead of a generic SSH failure.
	srv, plain, err := h.agentSvrSvc.GetWithCredential(in.serverID)
	if err != nil {
		return claudeStreamOutcome{errMsg: "无法读取 Agent 服务器凭据: " + err.Error()}
	}
	in.job.Append(store.LogLine{Type: "phase", Content: "🔌 连接到 Agent 服务器 " + srv.Name + " (" + srv.Host + ")"})

	client, err := gossh.Dial(ctx, srv.Host, srv.Port, srv.Username, srv.AuthType, plain)
	if err != nil {
		return claudeStreamOutcome{errMsg: "SSH 连接失败: " + err.Error()}
	}
	defer client.Close()

	// Step 2: code sync via git. baseRepo hosts a single origin clone for the
	// project; wtPath is the per-requirement worktree that mirrors the local
	// branch isolation model. Without a remote_url on the project the entire
	// remote path is dead — fail early with a clear message instead of an
	// opaque "git clone exit 128".
	if in.reqRow == nil {
		return claudeStreamOutcome{errMsg: "远程执行需要已保存的需求记录（缺 Requirement）"}
	}
	originURL, err := h.projectSvc.OriginURL(in.reqRow.ProjectID)
	if err != nil || originURL == "" {
		return claudeStreamOutcome{errMsg: "项目未配置 git 远程仓库，无法在 Agent 服务器执行。请先在项目设置中配置 origin。" + errString(err)}
	}
	baseRepo := "/tmp/nova-agent/" + in.reqRow.ProjectID + "/base"
	wtPath := "/tmp/nova-agent/" + in.reqRow.ProjectID + "/" + in.reqRow.ID
	branch := in.req.BranchName
	if branch == "" {
		branch = "requirement-" + in.reqRow.ID
	}
	baseBranch := in.req.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}

	in.job.Append(store.LogLine{Type: "phase", Content: "📥 准备 Agent 服务器代码（git worktree 隔离）..."})
	if !client.Exists(baseRepo) {
		in.job.Append(store.LogLine{Type: "message", Content: "📦 首次 clone " + redactOriginForLog(originURL)})
		if exit, _ := client.Exec(ctx, "git clone "+shellQuoteSingle(originURL)+" "+shellQuoteSingle(baseRepo), "", nil, &jobWriter{job: in.job}, nil); exit != 0 {
			return claudeStreamOutcome{errMsg: "git clone 失败（exit=" + fmtInt(exit) + "），请检查 origin 凭据"}
		}
	} else {
		client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git fetch origin --prune", "", nil, &jobWriter{job: in.job}, nil)
	}
	client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree prune", "", nil, &jobWriter{job: in.job}, nil)

	if !client.Exists(wtPath) {
		// Strategy 1: branch off HEAD (always valid; matches EnsureWorktree).
		// Strategy 2: off origin/<base> when strategy 1 fails. Strategy 3:
		// attach to an already-existing branch (adjust/continue reuse case).
		exit, _ := client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree add -b "+shellQuoteSingle(branch)+" "+shellQuoteSingle(wtPath), "", nil, &jobWriter{job: in.job}, nil)
		if exit != 0 {
			exit, _ = client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree add -b "+shellQuoteSingle(branch)+" "+shellQuoteSingle(wtPath)+" origin/"+shellQuoteSingle(baseBranch), "", nil, &jobWriter{job: in.job}, nil)
			if exit != 0 {
				if exit, _ = client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree add "+shellQuoteSingle(wtPath)+" "+shellQuoteSingle(branch), "", nil, &jobWriter{job: in.job}, nil); exit != 0 {
					return claudeStreamOutcome{errMsg: "git worktree 创建失败（exit=" + fmtInt(exit) + "），请检查仓库状态"}
				}
			}
		}
	} else {
		// adjust-coding / continue-coding: pull the latest remote commits
		// onto the existing branch. --ff-only protects against silent
		// divergence; on failure we log a hint and proceed with the local
		// copy (the user can resolve the divergence manually).
		client.Exec(ctx,
			"cd "+shellQuoteSingle(wtPath)+" && (git checkout "+shellQuoteSingle(branch)+" 2>/dev/null || true) && (git pull --ff-only origin "+shellQuoteSingle(branch)+" 2>&1 || echo \"[nova-agent] pull 跳过（无跟踪或已分叉）\")",
			"", nil, &jobWriter{job: in.job}, nil)
	}

	// Step 3: session sync (up) so the remote claude can --resume the same
	// session. We push the project-level ~/.claude/projects/<slug>/ contents
	// (the small set of jsonl files the CLI uses for session state). A missing
	// local dir is fine — the project has never been coded on before, and the
	// CLI on the remote will mint a brand-new session id.
	remoteClaudeHome := "~/.claude/projects/"
	in.job.Append(store.LogLine{Type: "phase", Content: "📤 同步 Claude 会话历史（SFTP 上行）..."})
	if slugDir, slugErr := claudeProjectsSlugDir(in.reqRow); slugErr == nil && slugDir != "" {
		client.Mkdirp(remoteClaudeHome)
		if sftpErr := client.SyncDirUp(slugDir, remoteClaudeHome); sftpErr != nil {
			in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 会话上行失败（将无 resume 启动新会话）: " + sftpErr.Error()})
		}
	} else if slugErr != nil {
		in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 无法定位本地 claude session 目录（" + slugErr.Error() + "），将无 resume 启动新会话"})
	}

	// Step 4: build the worker POST body. The mapping (NovaWorkbench
	// StreamOpts → worker RunRequest) lives here so the wire format and the
	// CLI invocation shape stay in one place; the worker mirrors the field
	// names in its buildRunRequest helper and translates them to the
	// matching `claude` CLI flags.
	//
	// Auth precedence on the remote path: the platform's active
	// claude_configs row is the ONLY source of ANTHROPIC_AUTH_TOKEN /
	// ANTHROPIC_BASE_URL / model pinning. We pass it via `env` (built by
	// h.llm.BuildEnvPairs above) AND we set IgnoreLocalSettings=true so the
	// worker invokes claude with --setting-sources "" (load no settings
	// files at all — user / project / local). The agent host's
	// ~/.claude/settings.json — which the install script seeds with a
	// placeholder token — must not be able to shadow the platform config,
	// and any project / local settings left in the worktree by accident
	// shouldn't either. The `env` field carries every key the CLI needs.
	ignoreLocal := true
	opts := llm.StreamOpts{
		Prompt:                 in.prompt,
		WorkDir:                wtPath,
		SystemPrompt:           "",
		Model:                  cliModelArg(in.model),
		SessionID:              in.sessionArg,
		Resume:                 in.sourceSID != "",
		Fork:                   in.fork,
		ForkSessionID:          in.forkSessionID,
		PermissionMode:         "",
		OverrideSettingSources: &ignoreLocal, // legacy flag, kept true
	}
	// Use BuildRemoteEnvPairs, NOT BuildEnvPairs: the remote worker spawns
	// claude inside the agent host's own environment, so we must only send the
	// platform-pinned keys (auth token / base URL / model pins). BuildEnvPairs
	// inherits os.Environ() of the NovaWorkbench host and would leak macOS
	// HOME=/Users/... + TMPDIR=/var/folders/... into the Linux agent, making
	// `claude --print ping` hang and fail the preflight (preflight_timeout).
	envPairs := h.llm.BuildRemoteEnvPairs(opts.Model)
	runBody := workerRunRequest(opts, envPairs, in)

	// Step 5: POST to nova-agent-worker via SSH direct-tcpip channel. The
	// HTTPTransport opens one channel per request through the existing SSH
	// connection — no new TCP port on the network, and the worker is bound
	// to 127.0.0.1 on the remote host, so even on the remote side it's not
	// exposed. The response is a streaming NDJSON body (one JSON event per
	// line, same shape as the old `claude --output-format stream-json`
	// output) that the existing parseStreamJSONFromReader consumes directly.
	in.job.Append(store.LogLine{Type: "phase", Content: "🤖 Agent 服务器开始执行（nova-agent-worker）..."})

	workerAddr := "127.0.0.1:7000"
	httpClient := &http.Client{Transport: client.HTTPTransport(workerAddr)}

	// Pre-flight: GET /v1/health. The previous direct-CLI path had a
	// `command -v claude` probe that surfaced "ENOENT" up front; this is
	// its worker equivalent. A failed health probe is the most likely cause
	// of "stream interrupted, 0 events" right now (the worker is new and
	// a pre-existing Agent server without it would otherwise look like an
	// opaque failure).
	healthCtx, healthCancel := context.WithTimeout(ctx, 10*time.Second)
	healthReq, hReqErr := http.NewRequestWithContext(healthCtx, http.MethodGet, "http://"+workerAddr+"/v1/health", nil)
	if hReqErr != nil {
		healthCancel()
		return claudeStreamOutcome{errMsg: "构造健康检查请求失败: " + hReqErr.Error()}
	}
	healthResp, healthErr := httpClient.Do(healthReq)
	if healthErr != nil {
		healthCancel()
		return claudeStreamOutcome{errMsg: "无法连接 nova-agent-worker（" + workerAddr + "）。请在「设置 → Agent 服务器」对该服务器点「安装依赖」后再试。详细: " + healthErr.Error()}
	}
	healthResp.Body.Close()
	healthCancel()
	if healthResp.StatusCode != http.StatusOK {
		return claudeStreamOutcome{errMsg: fmt.Sprintf("nova-agent-worker 健康检查失败: HTTP %d", healthResp.StatusCode)}
	}

	// POST /v1/run with the JSON body. No overall http.Client timeout —
	// the per-request ctx carries the 35-minute coding deadline.
	bodyBytes, mErr := json.Marshal(runBody)
	if mErr != nil {
		return claudeStreamOutcome{errMsg: "序列化 worker 请求失败: " + mErr.Error()}
	}
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+workerAddr+"/v1/run", bytes.NewReader(bodyBytes))
	if reqErr != nil {
		return claudeStreamOutcome{errMsg: "构造 worker 请求失败: " + reqErr.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, doErr := httpClient.Do(req)
	if doErr != nil {
		return claudeStreamOutcome{errMsg: "POST /v1/run 失败: " + doErr.Error()}
	}
	defer resp.Body.Close()

	// Non-200 before the stream starts = worker rejected the request
	// outright (bad JSON, missing fields, SDK query() threw on startup).
	// Read the full body and surface it verbatim — usually a JSON message
	// with the actual reason.
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return claudeStreamOutcome{errMsg: fmt.Sprintf("worker 返回 HTTP %d: %s", resp.StatusCode, truncateStr(string(errBody), 600))}
	}

	out := parseStreamJSONFromReader(resp.Body, jobSink{in.job}, "start-coding", in.usage)

	// Step 6: session sync (down) — copy any new session jsonl the remote
	// run created back to local so adjust/continue on the next round find
	// it. Same forward-only semantics as Step 3.
	in.job.Append(store.LogLine{Type: "phase", Content: "📥 同步会话结果回本地..."})
	if slugDir, slugErr := claudeProjectsSlugDir(in.reqRow); slugErr == nil && slugDir != "" {
		if sftpErr := client.SyncDirDown(remoteClaudeHome, slugDir); sftpErr != nil {
			in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 会话下行失败: " + sftpErr.Error()})
		}
	}

	// Step 7: git commit + push back to origin. Skip when the run errored out
	// (no real result) so we don't propagate half-broken state. The user can
	// always retry adjust-coding on the remote worktree via ContinueCoding.
	if out.errMsg == "" && out.finalResult != "" {
		in.job.Append(store.LogLine{Type: "phase", Content: "📤 推送代码变更到 origin..."})
		title := "nova-agent: " + in.req.RequirementTitle
		if title == "nova-agent: " {
			title = "nova-agent: " + in.reqRow.Title
		}
		// git commit -F - reads the message from stdin; we pipe via heredoc to
		// sidestep the SSH argv limit on long titles.
		commitScript := "cd " + shellQuoteSingle(wtPath) +
			" && git add -A" +
			" && git diff --cached --quiet || git commit -m " + shellQuoteSingle(title) +
			" && git push origin " + shellQuoteSingle(branch)
		if exit, _ := client.Exec(ctx, commitScript, "", nil, &jobWriter{job: in.job}, nil); exit != 0 {
			in.job.Append(store.LogLine{Type: "error", Content: "❌ 推送失败（exit=" + fmtInt(exit) + "），请在远程 worktree 手动处理冲突"})
			// Non-fatal: the user can still see the work locally via the
			// pushed-back session dir + the captured result text. Don't
			// override out.errMsg — let the run's own result stand.
		} else {
			in.job.Append(store.LogLine{Type: "message", Content: "✅ 已推送到 origin/" + branch})
		}
	}

	return out
}

// jobWriter adapts *store.Job to io.Writer so remote Exec output can land
// directly in the job's log (one message line per non-empty stdout/stderr
// chunk). Empty lines are dropped to avoid spamming the SSE panel.
type jobWriter struct{ job *store.Job }

func (w *jobWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(string(p), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		w.job.Append(store.LogLine{Type: "message", Content: line})
	}
	return len(p), nil
}

// workerRunBody is the JSON body sent to nova-agent-worker's POST /v1/run.
// It mirrors the worker's buildRunRequest helper (see agent-worker/server.mjs):
// every field here corresponds to one CLI flag the worker assembles. Keeping
// the shape on the Go side means wire-format changes require only one update.
//
// Why a typed struct (instead of `map[string]any`): the worker validates
// required fields at startup, and unknown fields would be silently dropped.
// A struct makes typos like `OverrideSettingSoures` a compile error rather
// than a "worker returns 400 with no detail" at runtime.
type workerRunBody struct {
	WorkDir         string            `json:"workDir"`
	Prompt          string            `json:"prompt"`
	Model           string            `json:"model,omitempty"`
	SystemPrompt    string            `json:"systemPrompt,omitempty"`
	SessionID       string            `json:"sessionId,omitempty"`
	Resume          bool              `json:"resume,omitempty"`
	Fork            bool              `json:"fork,omitempty"`
	ForkSessionID   string            `json:"forkSessionId,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	AllowedTools    []string          `json:"allowedTools,omitempty"`
	DisallowedTools []string          `json:"disallowedTools,omitempty"`
	// OverrideSettingSources is the legacy "drop the user source" flag —
	// when true, the worker invokes claude with --setting-sources project,local.
	// Kept for callers that still want project-level hooks etc.
	OverrideSettingSources bool `json:"overrideSettingSources,omitempty"`
	// IgnoreLocalSettings, when true, is the strict "drop EVERY settings
	// source" flag — the worker invokes claude with --setting-sources ""
	// so the platform's claude_configs row (delivered via `env`) is the
	// only source of ANTHROPIC_AUTH_TOKEN / ANTHROPIC_BASE_URL / model
	// pinning. The remote Agent-server path always sets this true: the
	// install script seeds ~/.claude/settings.json with a placeholder
	// token, and any stale value there would silently shadow the active row.
	IgnoreLocalSettings bool `json:"ignoreLocalSettings,omitempty"`
}

// workerRunRequest builds the POST body for /v1/run from the NovaWorkbench
// shape (llm.StreamOpts + remoteCodingInput). The env map is parsed from
// envPairs (each entry is "KEY=VALUE"); the worker hands this map to the
// claude subprocess's process env, so we don't need to strip the
// ANTHROPIC_* keys — the worker passes the map straight to the child.
//
// systemPrompt is intentionally left empty for the wizard remote path: the
// developer's persona is passed in the prompt itself (the -p payload
// includes the role system prompt as a preamble), matching the previous
// CLI invocation's behavior. If a future caller wants to pass it via
// --system-prompt, set opts.SystemPrompt before this is called.
func workerRunRequest(opts llm.StreamOpts, envPairs []string, in *remoteCodingInput) workerRunBody {
	envMap := make(map[string]string, len(envPairs))
	for _, kv := range envPairs {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		envMap[kv[:eq]] = kv[eq+1:]
	}
	override := false
	if opts.OverrideSettingSources != nil {
		override = *opts.OverrideSettingSources
	}
	return workerRunBody{
		WorkDir:                opts.WorkDir,
		Prompt:                 opts.Prompt,
		Model:                  opts.Model,
		SessionID:              opts.SessionID,
		Resume:                 opts.Resume,
		Fork:                   opts.Fork,
		ForkSessionID:          opts.ForkSessionID,
		Env:                    envMap,
		OverrideSettingSources: override,
		// Always drop local settings on the remote Agent-server path. The
		// wizard remote-coding call site passes OverrideSettingSources=true
		// (kept for backward compat with the legacy "drop user source"
		// semantics) and now also gets IgnoreLocalSettings=true so the
		// worker invokes claude with --setting-sources "" (load NO
		// settings files). The platform env passed via `env` above is the
		// sole source of auth / base URL / model pinning.
		IgnoreLocalSettings: true,
	}
}

// shellQuoteSingle mirrors the local ssh client's quoting: single-quoted
// strings with embedded single quotes escaped via close-quote / escape /
// open-quote. Empty strings become ”.
func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// redactOriginForLog strips userinfo (the embedded token) from the origin
// URL before showing it in the job log — same helper exists in
// service/project.go but we keep a local copy so the handler doesn't have to
// grow its dependency surface.
func redactOriginForLog(raw string) string {
	if i := strings.Index(raw, "@"); i > 0 {
		if j := strings.Index(raw[:i], "://"); j > 0 {
			return raw[:j+3] + "<redacted>@" + raw[i+1:]
		}
	}
	return raw
}

// fmtInt returns the decimal string for an int (kept as a tiny shim so
// the failure-path error messages read naturally without pulling in fmt
// solely for Sprintf("%d", x)).
func fmtInt(n int) string { return fmt.Sprintf("%d", n) }

// errString returns err.Error() or "" when err is nil.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return " (" + err.Error() + ")"
}

// claudeProjectsSlugDir locates the on-disk directory where claude stores
// session jsonls for the given requirement's project. The slug is derived
// from the project's local_path; we read the cached value on the project row
// when available, otherwise fall back to scanning the parent dir for a
// matching basename.
func claudeProjectsSlugDir(reqRow *model.Requirement) (string, error) {
	if reqRow == nil {
		return "", fmt.Errorf("no requirement")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	claudeHome := os.Getenv("NOVA_CLAUDE_HOME")
	if claudeHome == "" {
		claudeHome = filepath.Join(home, ".novaworkbench", "claude")
	}
	root := filepath.Join(claudeHome, "projects")
	// Prefer an exact-match lookup: the first subdir of root whose
	// decoded path ends with the project's basename. claude CLI's slug
	// is an internal encoding — there's no public mapping, so this
	// best-effort scan is good enough.
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	// The cached slug (when present) is the canonical answer. We don't
	// have it in this scope — the project row is loaded by the caller —
	// so we always scan here. Most setups have a single project
	// subdir under ~/.claude/projects/, so this stays cheap.
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		return filepath.Join(root, e.Name()), nil
	}
	return "", nil
}

// parseStreamJSONFromReader is the io.Reader-only counterpart of
// runClaudeStream. It scans NDJSON events off the supplied reader and emits
// the same LogLine shape the local path uses (phase / tool_call / message /
// usage / knowledge_result). It does NOT own a subprocess; the caller is
// responsible for piping the remote claude's stdout into r and closing it
// after the remote command exits.
//
// All heavy lifting (event dispatch, model pinning, token recording, usage
// persistence) is mirrored from runClaudeStream so the resulting
// claudeStreamOutcome is interchangeable. The differences are:
//   - no process group / killProcessGroup — the remote shell is the parent's
//     equivalent and we don't have access to its pgid over SSH
//   - no stall watchdog — a stuck remote claude is killed by closing the
//     SSH session from the caller's defer (client.Close kills the channel)
//   - no stderr fallback for staleness detection (we never see the remote
//     stderr in this scope)
func parseStreamJSONFromReader(r io.Reader, sink streamSink, scope string, uctx *usageCtx) claudeStreamOutcome {
	var out claudeStreamOutcome
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)
	// Track the first few non-JSON lines so the postmortem in runRemoteCoding
	// can show what claude actually printed before the EOF (claude CLI often
	// emits a warning or an error message on stdout/stderr that isn't valid
	// NDJSON — e.g. "NotLoggedIn", "SyntaxError", or proxy banner lines).
	var firstNonJSON []string
	for scanner.Scan() {
		out.eventCount++
		line := scanner.Text()
		if line == "" {
			continue
		}
		var evt map[string]interface{}
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			if strings.TrimSpace(line) != "" {
				log.Printf("[%s] non-json line: %s", scope, truncateStr(line, 500))
				if len(firstNonJSON) < 3 {
					firstNonJSON = append(firstNonJSON, truncateStr(line, 240))
				}
			}
			continue
		}
		evtType, _ := evt["type"].(string)
		out.lastEventType = evtType
		switch evtType {
		case "system":
			sub, _ := evt["subtype"].(string)
			switch sub {
			case "init":
				if sid, ok := evt["session_id"].(string); ok && sid != "" {
					out.sessionID = sid
				}
				sink.emit(store.LogLine{Type: "phase", Content: "🤖 Claude 已连接（远程），正在思考…"})
			case "thinking_tokens":
				if tokens, ok := evt["estimated_tokens"].(float64); ok {
					sink.emit(store.LogLine{Type: "phase", Content: fmt.Sprintf("🤔 模型思考中… (%d tokens)", int(tokens))})
				}
			}
		case "stream_event":
			out.streamEventCount++
			out.lastEventType = "stream_event"
			inner, _ := evt["event"].(map[string]interface{})
			if inner == nil {
				continue
			}
			switch inner["type"] {
			case "content_block_delta":
				delta, _ := inner["delta"].(map[string]interface{})
				if delta == nil {
					continue
				}
				switch delta["type"] {
				case "text_delta":
					text, _ := delta["text"].(string)
					if text != "" {
						out.hadStreamEvents = true
						sink.emit(store.LogLine{Type: "message", Content: text})
					}
				}
			case "content_block_start":
				block, _ := inner["content_block"].(map[string]interface{})
				if block != nil && block["type"] == "tool_use" {
					if name, _ := block["name"].(string); name != "" {
						sink.emit(store.LogLine{Type: "tool_call", Content: toolCallLabel(name, nil)})
					}
				}
			}
		case "assistant":
			msg, _ := evt["message"].(map[string]interface{})
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
					if toolName == "Write" && input != nil {
						if fp, ok := input["file_path"].(string); ok {
							if strings.Contains(filepath.ToSlash(fp), "/.claude/plans/") && strings.HasSuffix(fp, ".md") {
								if c, ok := input["content"].(string); ok && c != "" {
									out.planContent = c
								}
							}
						}
					}
					if p := inputToolPath(toolName, input); p != "" {
						out.toolFiles = append(out.toolFiles, p)
					}
					if !out.hadStreamEvents {
						sink.emit(store.LogLine{Type: "tool_call", Content: toolCallLabel(toolName, input)})
					}
				case "text":
					if !out.hadStreamEvents {
						if text, _ := b["text"].(string); text != "" {
							sink.emit(store.LogLine{Type: "message", Content: text})
						}
					}
				}
			}
		case "result":
			subtype, _ := evt["subtype"].(string)
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
				if isStaleSessionError(evt, "") {
					out.staleSession = true
				}
			}
			if out.actualModel != "" && uctx != nil {
				uctx.Model = out.actualModel
			}
			uctx.recordFrom(evt)
			if inTok, outTok, cc, cr, ok := llm.ParseStreamUsage(evt); ok {
				out.lastUsage = lastUsageSnapshot{InputTokens: inTok, OutputTokens: outTok, CacheCreationTokens: cc, CacheReadTokens: cr}
				if uctx != nil && (inTok+outTok+cc+cr) > 0 {
					modelName := uctx.Model
					if modelName == "" {
						modelName = out.actualModel
					}
					payload := map[string]any{
						"step":                  uctx.Step,
						"model":                 modelName,
						"input_tokens":          inTok,
						"output_tokens":         outTok,
						"cache_creation_tokens": cc,
						"cache_read_tokens":     cr,
						"context_window":        service.ModelContextWindow(modelName),
					}
					if b, mErr := json.Marshal(payload); mErr == nil {
						sink.emit(store.LogLine{Type: "usage", Content: string(b)})
						if uctx.PersistSnapshot != nil {
							if key := snapshotStep(uctx.Step); key != "" {
								snap := map[string]any{
									"model":                 modelName,
									"input_tokens":          inTok,
									"output_tokens":         outTok,
									"cache_creation_tokens": cc,
									"cache_read_tokens":     cr,
									"context_window":        service.ModelContextWindow(modelName),
								}
								if sb, sErr := json.Marshal(snap); sErr == nil {
									uctx.PersistSnapshot(key, string(sb))
								}
							}
						}
					}
				}
			}
			return out
		case "error":
			// nova-agent-worker emits {type:"error", error:"..."} when the
			// spawned claude CLI exits non-zero during initialization (auth
			// rejected at the proxy, DNS to api.anthropic.com failed, model
			// rejected, etc.). Without this case the body is dropped and
			// the EOF branch only knows lastEventType="error" — surfacing
			// the generic hint about API unreachable / OOM / argv
			// truncation, none of which pinpoints the actual cause. Accept
			// both string and object {message:...} shapes since the CLI
			// and any relay disagree.
			msg := extractStreamError(evt)
			if msg != "" {
				if out.errMsg == "" {
					out.errMsg = msg
				}
				sink.emit(store.LogLine{Type: "error", Content: msg})
			}
		}
	}
	// EOF without a result event — the remote claude exited before
	// completing the turn (network drop, etc.). The diagnostic now includes
	// stream-event count + last event type + any non-JSON preamble so the
	// user can tell "API hung silently after init" from "claude crashed
	// before producing anything".
	scanErr := scanner.Err()
	if out.finalResult == "" && out.errMsg == "" {
		var summary string
		if out.streamEventCount == 0 {
			summary = fmt.Sprintf("已收到 %d 个 NDJSON 事件（system/init 也未出现），最后类型=%q", out.eventCount, out.lastEventType)
		} else {
			summary = fmt.Sprintf("已收到 %d 个 NDJSON 事件，含 %d 个 stream_event（Claude 在输出文本/工具过程中断），最后类型=%q", out.eventCount, out.streamEventCount, out.lastEventType)
		}
		if len(firstNonJSON) > 0 {
			summary += "；非 JSON 前导输出: " + strings.Join(firstNonJSON, " | ")
		}
		if scanErr != nil {
			summary += "；scanner 错误: " + scanErr.Error()
		}
		out.errMsg = "远程 Claude 未返回结果（流中断 — " + summary + "）。最常见原因：Agent 服务器无法访问 api.anthropic.com（超时/DNS/防火墙），或 claude 进程崩溃/被 OOM kill，或 sshd exec argv 限制触发命令字符串被截断。"
	}
	return out
}

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
// buildAnalystFirstPrompt renders the first-turn prompt for the analyst
// stage. When the requirement carries an AnalystContextSummary (set by the
// "📦 压缩上下文" action on a prior session) it is prepended as a
// "## 上下文压缩摘要（之前的对话已被压缩）" block so the new session inherits
// the compressed history instead of starting cold. The summary is plain
// text — the wizard handler is responsible for stripping the
// [COMPRESS_COMPLETE] sentinel before persisting.
func buildAnalystFirstPrompt(req *model.Requirement, description, currentAnalysis, userMessage, docBlock, treeSummary string) string {
	var b strings.Builder
	if req != nil && req.AnalystContextSummary != "" {
		b.WriteString("## 上下文压缩摘要（之前的对话已被压缩）\n")
		b.WriteString(req.AnalystContextSummary)
		b.WriteString("\n\n")
	}
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
	writeSSEHeaders(w)
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
		// Fresh-session refine path (no resumable doc stage session). Seed
		// the conversation with the doc body + user message + completion
		// instructions, and when the matching wizard stage was previously
		// compressed, prepend the summary so the new session inherits the
		// compressed discussion context.
		prompt = "以下是当前的「" + docLabel + "」文档：\n\n" + requirement.DesignDocs +
			fmt.Sprintf("\n\n用户消息：\n%s\n\n", req.UserMessage) +
			"请基于上述文档回应用户对「" + docLabel + "」的修改意见，" +
			"完整列出每一个修改点的具体内容（包含涉及的表/字段/接口/逻辑），不要中途截断或留空。" +
			"若用户确认修改已完成，在回复最后单独一行追加：[REFINE_COMPLETE]\n用中文。"
		// Inject the matching stage's compressed summary on the fresh-session
		// path only — the resume path inherits the conversation natively and
		// doesn't need the prefix. Coding docs are routed through Coding*
		// columns; design docs through Design*.
		if req.DocType == "coding" && requirement.CodingContextSummary != "" {
			prompt = "## 上下文压缩摘要（之前的开发对话已被压缩，请基于此继续工作，不要当作新指令）\n" +
				requirement.CodingContextSummary + "\n\n" + prompt
		} else if req.DocType == "design" && requirement.DesignContextSummary != "" {
			prompt = "## 上下文压缩摘要（之前的方案设计对话已被压缩，请基于此继续工作，不要当作新指令）\n" +
				requirement.DesignContextSummary + "\n\n" + prompt
		}
	} else {
		prompt = fmt.Sprintf("用户消息：\n%s\n\n", req.UserMessage) +
			"请基于我们的对话上下文回应用户对「" + docLabel + "」的修改意见，" +
			"完整列出每一个修改点的具体内容（包含涉及的表/字段/接口/逻辑），不要中途截断或留空。" +
			"若用户确认修改已完成，在回复最后单独一行追加：[REFINE_COMPLETE]\n用中文。"
	}

	skillText := req.UserMessage
	if requirement != nil {
		skillText = requirement.Title + " " + requirement.Description + " " + req.UserMessage
	}
	if block := llm.BuildSkillsBlock(h.mentionedSkills(skillText)); block != "" {
		prompt = block + prompt
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
	refineUsage := h.usageCtxFor("refine_doc", req.RequirementID, refineProjectID, "", model, fmt.Sprintf("{\"doc_type\":%q}", req.DocType), "")
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

		applyPrompt := prompt
		if block := llm.BuildSkillsBlock(h.mentionedSkills(requirement.Title + " " + requirement.Description)); block != "" {
			applyPrompt = block + prompt
		}

		cmd := h.llm.StreamCmd(context.Background(), llm.StreamOpts{
			Prompt:       applyPrompt,
			WorkDir:      workDir,
			SystemPrompt: systemPrompt,
			Model:        cliModelArg(model),
			SessionID:    sourceSID,
			Resume:       true,
		})
		applyUsage := h.usageCtxFor("apply_doc", reqID, requirement.ProjectID, job.ID, model, fmt.Sprintf("{\"doc_type\":%q}", docType), "")
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

// mentionedSkills parses @slug mentions from text and returns the matching
// skill files. Only skills present in the DB are returned; unknown slugs are
// silently ignored. A nil skillSvc returns nil without error.
func (h *WizardHandler) mentionedSkills(text string) []struct{ Slug, Content string } {
	if h.skillSvc == nil {
		return nil
	}
	slugs := parseAtMentions(text)
	if len(slugs) == 0 {
		return nil
	}
	skills, _ := h.skillSvc.SkillsBySlug(slugs)
	return skills
}

// parseAtMentions extracts unique @slug tokens from text.
// A slug may contain letters, digits, hyphens, and underscores.
var atMentionRe = regexp.MustCompile(`@([A-Za-z0-9_-]+)`)

func parseAtMentions(text string) []string {
	matches := atMentionRe.FindAllStringSubmatch(text, -1)
	seen := make(map[string]bool)
	var slugs []string
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			slugs = append(slugs, m[1])
		}
	}
	return slugs
}

// isLikelyJSON reports whether s looks like a JSON object (starts with '{' after
// trimming whitespace). Used to distinguish plan-markdown design docs from the
// legacy JSON design schema.
func isLikelyJSON(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "{")
}

// compressContextReq is the body of POST /api/wizard/compress-context.
// step selects which wizard stage's session to summarize; it must be one of
// "analyst_chat" / "architect_design" / "coding" (validated by
// service.ValidContextSummaryStep).
type compressContextReq struct {
	RequirementID string `json:"requirement_id"`
	Step          string `json:"step"`
}

// compressContextDone is the JSON payload of the terminal "done" event for
// the compress-context SSE stream. Carrying the summary here (instead of a
// follow-up GET) lets the modal pop with a single round-trip; the persisted
// row is fetched separately by the frontend's requirementsApi.get() so the
// rest of the detail page (sidebar / stepper) reflects the new compressed_at
// timestamp too.
type compressContextDone struct {
	Step        string `json:"step"`
	Summary     string `json:"summary"`
	TokensUsed  int    `json:"tokens_used"`
	Model       string `json:"model"`
	StartedAt   int64  `json:"started_at_ms"`
	CompletedAt int64  `json:"completed_at_ms"`
}

// CompressContext is the implementation of POST /api/wizard/compress-context.
// It runs a single --resume turn asking Claude to summarize the current
// wizard stage's conversation, writes the result to requirements.{step}_
// context_summary + stamps compressed_at, and clears the matching session id
// — all in one transaction via service.UpdateContextSummary so the next turn
// in this stage starts fresh and sees the summary as a prompt prefix.
//
// Stream protocol (SSE under text/event-stream):
//
//	phase       — human-readable status line
//	message     — Claude's summary text as it streams in
//	usage       — mirror of the result.usage block (powers the live usage bar)
//	error       — terminal failure (no DB write happens)
//	done        — terminal success; carries {step, summary, tokens_used, model}
//
// Failure policy: on any error path (stream failure, stale session, missing
// [COMPRESS_COMPLETE] marker) we emit `error` + `done{success:false}` and
// DELIBERATELY skip the DB write — the session stays intact so the user can
// retry without losing context.
func (h *WizardHandler) CompressContext(w http.ResponseWriter, r *http.Request) {
	var req compressContextReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "invalid JSON: "+err.Error())
		return
	}
	if req.RequirementID == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "requirement_id is required")
		return
	}
	if !service.ValidContextSummaryStep(req.Step) {
		writeError(w, http.StatusBadRequest, "INVALID", "step must be one of: analyst_chat, architect_design, coding")
		return
	}

	rc := http.NewResponseController(w)
	writeSSEHeaders(w)
	w.WriteHeader(http.StatusOK)
	rc.Flush()

	startedAtMs := time.Now().UnixMilli()

	requirement, err := h.reqSvc.Get(req.RequirementID)
	if err != nil {
		sendStatus(w, rc, "error", "未找到需求："+err.Error())
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	// Pick the matching session id; reject the request when there is no
	// resumable session so the user doesn't burn tokens summarizing nothing.
	var sourceSID string
	switch req.Step {
	case "analyst":
		sourceSID = requirement.AnalysisSessionID
	case "design":
		sourceSID = requirement.DesignSessionID
	case "coding":
		sourceSID = requirement.CodingSessionID
	}
	if sourceSID == "" {
		sendStatus(w, rc, "error", "该阶段尚无可压缩的会话（先与 AI 对话一轮再试）")
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	// Resolve project path the same way the other wizard stages do — anchor
	// the resumed turn to the requirement's worktree (or the project root when
	// no worktree exists) so absolute paths in the resume history still resolve.
	projectPath := ""
	defaultBranch := ""
	if proj, perr := h.projectSvc.Get(requirement.ProjectID); perr == nil {
		projectPath = proj.LocalPath
		defaultBranch = proj.DefaultBranch
	}
	workDir, werr := h.resolveWorkDir(requirement, projectPath, defaultBranch)
	if werr != nil {
		// Non-fatal: a missing worktree doesn't break --resume, which can run
		// without a cwd. Log it so debugging is possible but proceed.
		log.Printf("[compress-context] resolveWorkDir failed for %s: %v", req.RequirementID, werr)
		workDir = projectPath
	}

	// Use the analyst role's system prompt — compression is essentially a
	// summary-extraction skill, and reusing the analyst persona keeps the
	// output style consistent with the other analytical turns. The model
	// override follows the same precedence as other wizard handlers.
	systemPrompt, model := h.roleConfig("analyst")

	// Compression prompt (Chinese, fixed). The [COMPRESS_COMPLETE] sentinel
	// is parsed by this handler to know when Claude has finished writing —
	// text before the sentinel is the summary; text after (rare, in case the
	// model emits trailing chatter) is discarded. We DISALLOW every tool so
	// the resumed turn can only read the conversation history baked into
	// --resume and produce a single textual summary.
	const compressPrompt = `请把当前对话压缩为一段精炼的中文摘要（300-800 字），要求：
1. 保留关键决策、技术约束、已确认的需求点
2. 保留当前进展状态、待办事项
3. 保留涉及的具体文件、函数、模块名
4. 不要新增原对话没有的信息
5. 不要输出代码片段（除非是必须保留的命名/路径）
6. 末尾单独一行输出：[COMPRESS_COMPLETE]`

	noTools := []string{
		"Read", "Glob", "Grep", "Bash", "Write", "Edit",
		"WebFetch", "WebSearch", "NotebookEdit",
	}

	cmd := h.llm.StreamCmd(r.Context(), llm.StreamOpts{
		Prompt:          compressPrompt,
		WorkDir:         workDir,
		SessionID:       sourceSID,
		Resume:          true,
		SystemPrompt:    systemPrompt,
		Model:           cliModelArg(model),
		DisallowedTools: noTools,
	})

	// Usage record: step is namespaced under "compress_<step>" so the existing
	// usageApi.requirement() aggregation includes compression cost without
	// confusing it with regular turn counts in the step dropdown.
	usageStep := "compress_" + req.Step
	uctx := h.usageCtxFor(usageStep, req.RequirementID, requirement.ProjectID, "", model, "", "")

	sink := sseSink{w: w, rc: rc}
	sink.emit(store.LogLine{Type: "phase", Content: "📦 正在压缩上下文..."})

	out := runClaudeStream(sink, cmd, "compress-context", uctx)

	if out.staleSession {
		// Disk session is gone (cleaned up, container restart, etc.). We can't
		// resume, so the right thing is to leave the (already-empty) sid
		// alone — the next regular turn will start a fresh session anyway.
		sendStatus(w, rc, "error", "⚠️ 该阶段的会话已过期（磁盘已被清理），无需压缩")
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}
	if out.errMsg != "" {
		sendStatus(w, rc, "error", "❌ "+out.errMsg)
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	// Extract the summary: everything before [COMPRESS_COMPLETE], trimmed.
	// A missing marker is treated as a soft failure so we don't store a half-
	// formed or empty summary that would mislead future turns.
	summary := out.finalResult
	if idx := strings.Index(summary, "[COMPRESS_COMPLETE]"); idx >= 0 {
		summary = summary[:idx]
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		sendStatus(w, rc, "error", "❌ Claude 未输出有效摘要（缺少 [COMPRESS_COMPLETE] 标记或摘要为空），请重试")
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	// Token count for the done payload — read straight from the stream
	// outcome we already have in scope (no DB roundtrip needed). The model
	// comes from the result event's actualModel (set by runClaudeStream) with
	// a fallback to the pre-dispatch role model.
	tokensUsed := out.lastUsage.InputTokens +
		out.lastUsage.OutputTokens +
		out.lastUsage.CacheCreationTokens +
		out.lastUsage.CacheReadTokens
	modelOut := out.actualModel
	if modelOut == "" {
		modelOut = model
	}

	// Persist + clear session id atomically. A failure here leaves the
	// summary un-saved so the user can retry — the original session stays
	// live (we never cleared the sid before the write).
	if perr := h.reqSvc.UpdateContextSummary(req.RequirementID, req.Step, summary); perr != nil {
		log.Printf("[compress-context] persist failed for %s step=%s: %v", req.RequirementID, req.Step, perr)
		sendStatus(w, rc, "error", "❌ 持久化失败："+perr.Error())
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	completedAtMs := time.Now().UnixMilli()
	done := compressContextDone{
		Step:        req.Step,
		Summary:     summary,
		TokensUsed:  tokensUsed,
		Model:       modelOut,
		StartedAt:   startedAtMs,
		CompletedAt: completedAtMs,
	}
	if b, mErr := json.Marshal(done); mErr == nil {
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":true,\"payload\":%s}\n\n", string(b))
	} else {
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":true}\n\n")
	}
	rc.Flush()
}

// GetContextSummary returns the persisted compression summary for one
// requirement + step. Optional endpoint — the frontend can derive this from
// the regular Requirement GET (which now exposes the *_context_summary +
// *_compressed_at fields), but exposing a dedicated endpoint keeps the
// "📦 已压缩" UI affordance cheap to refresh on its own.
//
// GET /api/wizard/requirement/{id}/context-summary?step=analyst_chat|architect_design|coding
func (h *WizardHandler) GetContextSummary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "missing requirement id")
		return
	}
	step := r.URL.Query().Get("step")
	if !service.ValidContextSummaryStep(step) {
		writeError(w, http.StatusBadRequest, "INVALID", "step must be one of: analyst_chat, architect_design, coding")
		return
	}
	summary, err := h.reqSvc.GetContextSummary(id, step)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requirement_id": id,
		"step":           step,
		"summary":        summary,
	})
}

// ----------------------------------------------------------------------------
// Sub-task (子任务) endpoints
//
// A sub-task is a manually-triggered child agent that runs under a
// requirement's developing stage. It forks the requirement's main-agent
// session (coding_session_id, with design_session_id as fallback) so every
// child agent shares the parent's context — project structure, design docs,
// prior conversation — but executes in its own claude process and writes its
// own Markdown artifact to sub_tasks.artifact on completion.
//
// Endpoints:
//   POST /api/requirements/{id}/sub-tasks     → start a new child agent
//   GET  /api/requirements/{id}/sub-tasks     → list child agents for the req
//   GET  /api/requirements/{id}/sub-tasks/{sid} → fetch one (incl. artifact)
//
// The SSE stream for a running sub-task reuses the existing
// /api/wizard/jobs/{jobId}/stream endpoint — the SubTaskPanel wires its
// createEventStream to that path with sub_task.job_id so the panel gets the
// exact same phase/tool_call/message/result event flow the developer stage
// already uses.
// ----------------------------------------------------------------------------

// requireSubTaskSvc is a small guard helper. When the service was never
// injected (legacy / standalone deployment) the sub-task endpoints must 503
// rather than 500 from a nil-pointer panic. Returns false after writing the
// 503 response; the caller returns immediately.
func (h *WizardHandler) requireSubTaskSvc(w http.ResponseWriter) bool {
	if h.subTaskSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "sub-task service not initialized")
		return false
	}
	return true
}

// resolveSubTaskSource picks the session id a sub-task should fork off. The
// primary source is the sub-task's explicit `source_session_id` (set by the
// caller — either the requirement's coding_session_id for a fresh sub-task,
// or another sub-task's session_id for an adjustment). Empty forces the
// caller to error out (no main agent session exists yet).
func subTaskSourceSID(req *model.Requirement, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if req != nil && req.CodingSessionID != "" {
		return req.CodingSessionID
	}
	if req != nil && req.DesignSessionID != "" {
		return req.DesignSessionID
	}
	return ""
}

// runSubTask spawns the claude CLI subprocess for a sub-task row and writes
// the final artifact to sub_tasks.artifact on completion. shared by
// StartSubTask and AdjustSubTask so the only thing callers vary is the
// source session id.
//
// Side effects on success:
//   - sub_tasks.status transitions to running (via MarkRunning)
//   - the spawned JobStore job is appended with live phase/message lines
//   - on completion, sub_tasks.artifact is filled with a Markdown report
//     wrapping the claude finalResult, and job.Finish is called
//
// Pre: the sub_tasks row is already created with status=pending and has
// its source_session_id populated. Pre-minting a session id via
// subTaskSvc.UpdateSession + a JobStore job via jobs.Create should happen
// in the caller before invoking this function — see StartSubTask for the
// canonical ordering.
func (h *WizardHandler) runSubTask(
	req *model.Requirement,
	st *model.SubTask,
	job *store.Job,
	newSID string,
	sourceSID string,
	body string,
	modelOverride string,
	adjust bool,
) {
	startTime, mErr := h.subTaskSvc.MarkRunning(st.ID)
	if mErr != nil {
		log.Printf("[sub-task] failed to mark running for %s: %v", st.ID, mErr)
	}
	// Best-effort persistence: backend restart mid-run won't lose the log.
	defer func() {
		lines, status, exitCode := job.Snapshot()
		if perr := h.jobLogSvc.Save(job.ID, st.RequirementID, string(status), exitCode, job.StartedAt, job.FinishedAt, lines, job.Model); perr != nil {
			log.Printf("[sub-task] failed to persist job log %s: %v", job.ID, perr)
		}
	}()

	role := "🤖 调整子任务启动中..."
	if !adjust {
		role = "🤖 子任务启动中..."
	}
	job.Append(store.LogLine{Type: "phase", Content: role})
	job.Append(store.LogLine{Type: "message", Content: "📝 提示词: " + truncateForLog(body, 240)})

	// Resolve the developer role's model, but run the child under the
	// "executor" role system prompt: the sub-task forks the coding session,
	// which carries the developer (统筹协调) persona that decomposes instead
	// of implementing. Without the override the child re-emits
	// [SUBTASKS_READY] and writes no code.
	_, modelName := h.roleConfig("developer")
	if modelOverride != "" {
		modelName = modelOverride
	}
	job.SetModel(modelName)

	// Workdir: prefer the requirement's isolated worktree. Fallback to
	// project checkout for legacy rows.
	workDir := ""
	if proj, perr := h.projectSvc.Get(req.ProjectID); perr == nil {
		workDir = proj.LocalPath
	}
	if req.WorktreePath != "" {
		if _, statErr := os.Stat(req.WorktreePath); statErr == nil {
			workDir = req.WorktreePath
		}
	}
	if workDir == "" {
		job.Append(store.LogLine{Type: "error", Content: "❌ 无法解析工作目录"})
		job.Finish(1, store.JobError)
		h.subTaskSvc.Finish(st.ID, model.SubTaskStatusError, buildSubTaskArtifact(st, modelName, "无法解析工作目录", time.Now()), modelName, model.SubTaskTokens{}, 0, startTime)
		return
	}

	var prompt string
	if adjust {
		prompt = "## 追加调整\n\n" + body + "\n"
	} else {
		prompt = "## 子任务\n\n" + body + "\n"
	}
	prompt += "\n> 你是执行者：请直接动手实现本子任务并落盘代码改动，不要再做任务拆分。\n"
	if block := llm.BuildSkillsBlock(h.mentionedSkills(req.Title + " " + body)); block != "" {
		prompt = block + prompt
	}

	execSystemPrompt, _ := h.roleConfig(executorRoleKey)
	cmd := h.llm.GenerateCode(llm.StreamOpts{
		Prompt:       prompt,
		WorkDir:      workDir,
		SystemPrompt: execSystemPrompt,
		Model:        cliModelArg(modelName),
		// --resume <sourceSID> --fork-session --session-id <newSID>:
		// child agent inherits the parent's conversation context but
		// executes in its own session.
		SessionID:     sourceSID,
		Resume:        true,
		Fork:          true,
		ForkSessionID: newSID,
	})

	// "sub_task" step key — distinct from "coding" / "adjust_coding" so
	// token-usage rollups don't double-count.
	subUsage := h.usageCtxFor("sub_task", st.RequirementID, req.ProjectID, job.ID, modelName, "", body)
	out := runClaudeStream(jobSink{job}, cmd, "sub-task", subUsage)

	finalStatus := model.SubTaskStatusDone
	var artifactBody string
	switch {
	case out.staleSession:
		finalStatus = model.SubTaskStatusError
		artifactBody = "❌ 源会话已失效（session 文件不存在），请重新发起 coding 后再试。"
		job.Append(store.LogLine{Type: "error", Content: artifactBody})
	case out.errMsg != "":
		finalStatus = model.SubTaskStatusError
		artifactBody = "❌ " + out.errMsg
		job.Append(store.LogLine{Type: "error", Content: artifactBody})
	case out.finalResult == "":
		finalStatus = model.SubTaskStatusError
		artifactBody = "❌ Claude 未返回结果，请重试"
		job.Append(store.LogLine{Type: "error", Content: artifactBody})
	default:
		job.Append(store.LogLine{Type: "result", Content: strings.TrimSpace(out.finalResult)})
		artifactBody = out.finalResult
	}
	job.Append(store.LogLine{Type: "done", Content: "✅ 子任务完成！"})

	artifact := buildSubTaskArtifact(st, modelName, artifactBody, time.Now())
	tokens := model.SubTaskTokens{
		Input:         out.lastUsage.InputTokens,
		Output:        out.lastUsage.OutputTokens,
		CacheCreation: out.lastUsage.CacheCreationTokens,
		CacheRead:     out.lastUsage.CacheReadTokens,
	}
	costCents := computeSubTaskCostCents(modelName, tokens, h.claudeCfg)
	if perr := h.subTaskSvc.Finish(st.ID, finalStatus, artifact, modelName, tokens, costCents, startTime); perr != nil {
		log.Printf("[sub-task] failed to persist finish for %s: %v", st.ID, perr)
	}
	job.Finish(0, store.JobDone)
	log.Printf("[sub-task] job %s finished for %s status=%s", job.ID, st.ID, finalStatus)
}

// computeSubTaskCostCents resolves the run's USD-equivalent cost in cents
// against the active claude config's per-model unit price (input / output
// per million tokens, same convention the dashboard's usage rollup uses).
// Cache creation + cache reads are billed as input. Returns 0 when there's
// no active config, the model isn't priced, or the config hasn't
// configured unit prices yet — the SubTaskPanel renders "—" rather than
// "$0.00" in that case so the user knows pricing wasn't available.
//
// Best-effort: failures are silently swallowed (the artifact / status /
// tokens are the durable record; cost is decorative). The active config
// lookup mirrors the one roleConfig uses at sub-task start time, so this
// resolves against the same source-of-truth the run actually billed.
func computeSubTaskCostCents(modelName string, tokens model.SubTaskTokens, claudeCfg *service.ClaudeConfigService) int {
	if claudeCfg == nil {
		return 0
	}
	cfg, err := claudeCfg.ActiveConfig()
	if err != nil || cfg == nil {
		return 0
	}
	for _, p := range cfg.Models {
		if p.Model != modelName {
			continue
		}
		if p.InputPrice == 0 && p.OutputPrice == 0 {
			return 0
		}
		inUSD := float64(tokens.Input+tokens.CacheCreation+tokens.CacheRead) / 1e6 * p.InputPrice
		outUSD := float64(tokens.Output) / 1e6 * p.OutputPrice
		cents := int((inUSD + outUSD) * 100)
		if cents < 0 {
			return 0
		}
		return cents
	}
	return 0
}

// StartSubTask handles POST /api/requirements/{id}/sub-tasks.
//
// Body: { "prompt": "...", "title": "..." }  (title optional)
//
// Response: 200 { "job_id": "...", "sub_task_id": "..." }
//
// Creates a fresh sub_task row that forks the requirement's main-agent
// session (coding_session_id, with design_session_id as fallback). The
// orchestration work (validate, persist session/job ids) happens inline;
// the actual claude subprocess spawn is delegated to runSubTask so it can
// be shared with AdjustSubTask.
func (h *WizardHandler) StartSubTask(w http.ResponseWriter, r *http.Request) {
	if !h.requireSubTaskSvc(w) {
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
		Title  string `json:"title"`
		Model  string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "prompt 不能为空")
		return
	}
	id := r.PathValue("id")
	req, err := h.reqSvc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "requirement not found")
		return
	}
	sourceSID := subTaskSourceSID(req, "")
	if sourceSID == "" {
		writeError(w, http.StatusConflict, "NO_SESSION",
			"需求尚未启动 coding 或 design session，无法创建子任务")
		return
	}

	st, err := h.subTaskSvc.Create(id, strings.TrimSpace(body.Title), body.Prompt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	newSID := util.NewUUID()
	if perr := h.subTaskSvc.UpdateSession(st.ID, newSID, sourceSID); perr != nil {
		log.Printf("[sub-task] failed to persist session for %s: %v", st.ID, perr)
	}
	job := h.jobs.Create(id)
	if perr := h.subTaskSvc.UpdateJobID(st.ID, job.ID); perr != nil {
		log.Printf("[sub-task] failed to persist job_id for %s: %v", st.ID, perr)
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"job_id":      job.ID,
		"sub_task_id": st.ID,
	})

	go h.runSubTask(req, st, job, newSID, sourceSID, body.Prompt, body.Model, false)
}

// ReOrchestrate handles POST /api/requirements/{id}/re-orchestrate — the
// manual "🔄 重新拆分" escape hatch for when StartCoding's auto-orchestration
// produced no children (main agent ignored every decomposition channel) or
// the user simply wants a fresh split. It resumes the requirement's coding
// session with the SAME decomposition trigger prompt StartCoding sends
// (including the Write-tool subtasks.json instruction), then feeds the turn
// through the exact same parse+dispatch pipeline as tryAutoOrchestrate
// (resolveSubtasksPayload → dispatchChildrenSequential → summaryKickoff).
//
// Returns { job_id } immediately; the job carries the main agent's live
// stream so the frontend can show progress via the usual job SSE endpoint.
// 409 when a child is still running — re-splitting mid-batch would fork the
// session concurrently and double-dispatch work.
//
// Body: { "model"?: "..." }
func (h *WizardHandler) ReOrchestrate(w http.ResponseWriter, r *http.Request) {
	if !h.requireSubTaskSvc(w) {
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	id := r.PathValue("id")
	req, err := h.reqSvc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "requirement not found")
		return
	}
	if req.Kind == "idea" {
		writeError(w, http.StatusConflict, "IDEA", "「想法」类需求不支持任务拆分，请先转为需求")
		return
	}
	// Refuse to re-split while any child is alive — the children fork the
	// coding session, and a concurrent decomposition turn would race them.
	if existing, lerr := h.subTaskSvc.List(id); lerr == nil {
		for _, st := range existing {
			if st.Status == model.SubTaskStatusRunning || st.Status == model.SubTaskStatusPending {
				writeError(w, http.StatusConflict, "BUSY", "有子任务正在执行，请等待完成后再重新拆分")
				return
			}
		}
	}

	job := h.jobs.Create(id)
	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})

	go func() {
		// Best-effort persistence: backend restart mid-run won't lose the log.
		defer func() {
			lines, status, exitCode := job.Snapshot()
			if perr := h.jobLogSvc.Save(job.ID, id, string(status), exitCode, job.StartedAt, job.FinishedAt, lines, job.Model); perr != nil {
				log.Printf("[re-orchestrate] failed to persist job log %s: %v", job.ID, perr)
			}
		}()

		// Workdir: prefer the requirement's isolated worktree (mirrors
		// runSubTask so both paths agree on where the code lives).
		workDir := ""
		if proj, perr := h.projectSvc.Get(req.ProjectID); perr == nil {
			workDir = proj.LocalPath
		}
		if req.WorktreePath != "" {
			if _, statErr := os.Stat(req.WorktreePath); statErr == nil {
				workDir = req.WorktreePath
			}
		}
		if workDir == "" {
			job.Append(store.LogLine{Type: "error", Content: "❌ 无法解析工作目录"})
			job.Finish(1, store.JobError)
			return
		}

		_, modelName := h.roleConfig("developer")
		if body.Model != "" {
			modelName = body.Model
		}
		job.SetModel(modelName)

		// Session threading: resume the existing coding session so the main
		// agent re-decomposes with full context. No coding session yet →
		// fresh session carrying the requirement title + description.
		systemPrompt, _ := h.roleConfig("developer")
		sessionID := req.CodingSessionID
		resume := sessionID != ""
		var prompt string
		if resume {
			prompt = developerDecomposePrompt(req.Title,
				"上次未能成功完成任务拆分。请基于已有的需求与上下文，重新立即完成**任务拆分**：\n", workDir)
		} else {
			sessionID = util.NewUUID()
			if perr := h.reqSvc.UpdateCodingSession(id, sessionID); perr != nil {
				log.Printf("[re-orchestrate] failed to persist coding session for %s: %v", id, perr)
			}
			prompt = developerDecomposePrompt(req.Title,
				"请先读取项目中的相关文件理解现有代码结构与需求上下文，然后立即完成**任务拆分**：\n", workDir)
			if d := strings.TrimSpace(req.Description); d != "" {
				prompt += "\n\n需求描述：\n" + d
			}
		}

		job.Append(store.LogLine{Type: "phase", Content: "🔄 主 Agent 重新拆分任务中…"})
		cmd := h.llm.GenerateCode(llm.StreamOpts{
			Prompt:       prompt,
			WorkDir:      workDir,
			SystemPrompt: systemPrompt,
			Model:        cliModelArg(modelName),
			SessionID:    sessionID,
			Resume:       resume,
		})
		usage := h.usageCtxFor("re_orchestrate", id, req.ProjectID, job.ID, modelName, "", "")
		out := runClaudeStream(jobSink{job}, cmd, "re-orchestrate", usage)

		switch {
		case out.staleSession:
			// Coding session file is gone — clear it so the next attempt
			// takes the fresh-session path instead of failing again.
			job.Append(store.LogLine{Type: "error", Content: "❌ 原开发会话已失效，请重新点击「重新拆分」（下次将使用全新会话）"})
			if perr := h.reqSvc.UpdateCodingSession(id, ""); perr != nil {
				log.Printf("[re-orchestrate] failed to clear coding session for %s: %v", id, perr)
			}
			job.Finish(1, store.JobError)
			return
		case out.errMsg != "":
			job.Append(store.LogLine{Type: "error", Content: "❌ " + out.errMsg})
			job.Finish(1, store.JobError)
			return
		case out.finalResult == "" && out.subTasksJSON == "":
			job.Append(store.LogLine{Type: "error", Content: "❌ Claude 未返回结果，请重试"})
			job.Finish(1, store.JobError)
			return
		}
		if out.finalResult != "" {
			job.Append(store.LogLine{Type: "result", Content: strings.TrimSpace(out.finalResult)})
		}

		// Same parse chain as the auto path. NOTE: unlike tryAutoOrchestrate
		// the manual path does NOT fall back to a single whole-requirement
		// child — the user explicitly asked for a SPLIT, so a total parse
		// failure is reported, not silently converted.
		payload := h.resolveManualReSplit(id, out.finalResult, out.subTasksJSON)
		// Consume-then-clean: never leave subtasks.json in the worktree.
		if rerr := os.Remove(subTasksFilePath(workDir)); rerr != nil && !os.IsNotExist(rerr) {
			log.Printf("[re-orchestrate] %s: failed to remove %s: %v", id, subTasksFilePath(workDir), rerr)
		}
		if payload == nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ 主 Agent 仍未输出可解析的任务拆分。可在下方手动创建子任务，或调整提示词后重试。"})
			job.Finish(1, store.JobError)
			return
		}
		job.Append(store.LogLine{Type: "message", Content: fmt.Sprintf("📋 拆分完成：%d 个子任务，开始串行派发…", len(payload.Subtasks))})
		job.Append(store.LogLine{Type: "done", Content: "✅ 重新拆分完成，子任务派发中"})
		job.Finish(0, store.JobDone)

		// Dispatch runs after job.Finish so the re-split SSE closes promptly;
		// each child's own job streams its progress like the auto path.
		subTaskIDs := dispatchChildrenSequential(id, sessionID, req, h, payload.Subtasks, workDir, modelName)
		if len(subTaskIDs) == 0 {
			log.Printf("[re-orchestrate] %s: dispatch produced 0 children", id)
			return
		}
		log.Printf("[re-orchestrate] %s: dispatched %d children, scheduling summary", id, len(subTaskIDs))
		summaryKickoff(id, sessionID, req, h, workDir, modelName, subTaskIDs)
	}()
}

// resolveManualReSplit is the manual re-split's parse chain: identical to
// resolveSubtasksPayload MINUS the whole-requirement fallback child — a user
// who clicked 重新拆分 asked for a decomposition, so a total parse failure
// must surface as an error instead of silently executing everything as one
// task.
func (h *WizardHandler) resolveManualReSplit(reqID, finalResult, capturedJSON string) *orchestratorPayload {
	if p := decodeSubtasksPayload(capturedJSON); p != nil {
		log.Printf("[re-orchestrate] %s: using Write-captured subtasks.json (%d subtasks)", reqID, len(p.Subtasks))
		return p
	}
	if strings.TrimSpace(finalResult) == "" {
		return nil
	}
	if p, ok := extractSubtasksPayload(finalResult); ok {
		return p
	}
	if p := extractSubtasksFromMarkdown(finalResult); p != nil {
		return p
	}
	return h.extractSubtasksWithLLM(reqID, finalResult)
}

// AdjustSubTask handles POST /api/requirements/{id}/sub-tasks/{sid}/adjust.
//
// Body: { "prompt": "...", "model"?: "..." }
//
// Creates a NEW sub_task row that forks the parent sub_task's session id —
// letting the user push follow-up instructions into the same implementation
// thread without re-forking from the (much older) main-agent session. The
// prompt is the only new instruction; the rest of the context (project
// files, design docs, prior code edits) is inherited automatically.
func (h *WizardHandler) AdjustSubTask(w http.ResponseWriter, r *http.Request) {
	if !h.requireSubTaskSvc(w) {
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
		Model  string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "prompt 不能为空")
		return
	}
	id := r.PathValue("id")
	sid := r.PathValue("sid")
	req, err := h.reqSvc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "requirement not found")
		return
	}

	// Look up the parent to validate ownership and capture its session id
	// (the source for the adjustment's --fork-session resume).
	parent, err := h.subTaskSvc.Get(sid)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "sub-task not found")
		return
	}
	if parent.RequirementID != id {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "sub-task does not belong to this requirement")
		return
	}
	if parent.SessionID == "" {
		writeError(w, http.StatusConflict, "NO_SESSION",
			"原子任务尚未生成 session（可能仍在启动或运行），无法追加调整")
		return
	}

	// Adjusting a failed sub-task is allowed but its session file may have
	// rolled back to a state before the failure — surfaced via staleSession
	// at run time, same UX as the main adjust-coding path.
	st, err := h.subTaskSvc.CreateAdjustment(id, sid, body.Prompt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	newSID := util.NewUUID()
	if perr := h.subTaskSvc.UpdateSession(st.ID, newSID, parent.SessionID); perr != nil {
		log.Printf("[sub-task adjust] failed to persist session for %s: %v", st.ID, perr)
	}
	job := h.jobs.Create(id)
	if perr := h.subTaskSvc.UpdateJobID(st.ID, job.ID); perr != nil {
		log.Printf("[sub-task adjust] failed to persist job_id for %s: %v", st.ID, perr)
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"job_id":      job.ID,
		"sub_task_id": st.ID,
	})

	// Re-use the shared spawn helper with adjust=true. This runs the same
	// prompt prefix + system prompt as a fresh sub-task, but the
	// source_session_id is the parent's session id (not the main agent),
	// so the conversation inherits the parent's edits.
	go h.runSubTask(req, st, job, newSID, parent.SessionID, body.Prompt, body.Model, true)
}

// ListSubTasks handles GET /api/requirements/{id}/sub-tasks.
// Returns the sub-tasks ordered oldest-first so the panel renders them in
// firing order. Validates the requirement exists first so a typo returns
// 404 rather than an empty array (which would silently look like "no
// sub-tasks yet").
func (h *WizardHandler) ListSubTasks(w http.ResponseWriter, r *http.Request) {
	if !h.requireSubTaskSvc(w) {
		return
	}
	id := r.PathValue("id")
	if _, err := h.reqSvc.Get(id); err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "requirement not found")
		return
	}
	items, err := h.subTaskSvc.List(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// GetSubTask handles GET /api/requirements/{id}/sub-tasks/{sid}.
// Verifies the row's requirement_id matches the URL {id} so a forged URL
// can't be used to fetch a sub-task that belongs to another requirement —
// the front-end only knows ids from List, but a tampering curl call should
// still hit this guard.
func (h *WizardHandler) GetSubTask(w http.ResponseWriter, r *http.Request) {
	if !h.requireSubTaskSvc(w) {
		return
	}
	id := r.PathValue("id")
	sid := r.PathValue("sid")
	st, err := h.subTaskSvc.Get(sid)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "sub-task not found")
		return
	}
	if st.RequirementID != id {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "sub-task does not belong to this requirement")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// buildSubTaskArtifact composes the final Markdown report persisted into
// sub_tasks.artifact. The header carries the title / prompt / model /
// timestamps so the report reads standalone — useful when the SubTaskPanel
// downloads it or the user opens it from disk later. body is the claude
// output verbatim (already trimmed upstream); error strings start with "❌"
// so the panel can render them in the error style without further
// inspection.
func buildSubTaskArtifact(st *model.SubTask, model, body string, finishedAt time.Time) string {
	var b strings.Builder
	b.WriteString("# 子任务: ")
	b.WriteString(st.Title)
	b.WriteString("\n\n")
	b.WriteString("**提示词**: ")
	b.WriteString(strings.TrimSpace(st.Prompt))
	b.WriteString("\n\n")
	b.WriteString("**完成时间**: ")
	b.WriteString(finishedAt.Format("2006-01-02 15:04:05"))
	b.WriteString("  **模型**: ")
	b.WriteString(model)
	b.WriteString("\n\n---\n\n")
	b.WriteString(strings.TrimSpace(body))
	return b.String()
}

// truncateForLog shortens prompt for the "📝 提示词:" log line so a multi-KB
// prompt doesn't spam the coding panel. Uses the same char-budget as the
// SubTaskPanel card title (240) so the panel preview matches the panel.
func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// codingPlanSectionRe finds the main agent's "任务分解" (task breakdown)
// section inside a freeform claude response. The developer role's system
// prompt instructs the main agent to emit a Markdown section between two
// sentinel markers so we can locate it without resorting to full Markdown
// AST parsing.
//
// Two layouts are accepted (both intentional — the agent picks whichever
// reads better given the requirements):
//
//  1. Sentinel-wrapped (preferred for stable parsing):
//     <!-- CODING_PLAN_START -->
//     ... plan content ...
//     <!-- CODING_PLAN_END -->
//
//  2. Heading-based fallback (when the agent forgets the sentinels but
//     follows the "## 任务分解" instruction in the role prompt):
//     ## 任务分解
//     ... plan content ...
//     ## 其他章节
//
// Returns the trimmed inner Markdown (sentinels stripped), or "" when no
// plan section was found. The returned string is the value persisted into
// requirements.coding_plan; the SubTaskPanel renders it directly.
var (
	codingPlanStart = regexp.MustCompile(`(?s)<!--\s*CODING_PLAN_START\s*-->`)
	codingPlanEnd   = regexp.MustCompile(`(?s)<!--\s*CODING_PLAN_END\s*-->`)
	codingPlanHead  = regexp.MustCompile(`(?m)^#{1,3}\s*任务分解\s*$`)
)

// extractCodingPlan pulls the main agent's task breakdown section out of a
// freeform claude response. Tries sentinel-wrapped first (more robust against
// nested ## sections), falls back to a "## 任务分解" heading scan. Returns ""
// when no plan section is detected — callers persist "" to clear stale plans.
func extractCodingPlan(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	// Layout 1: sentinel-wrapped.
	startIdx := codingPlanStart.FindStringIndex(text)
	endIdx := codingPlanEnd.FindStringIndex(text)
	if startIdx != nil && endIdx != nil && endIdx[0] > startIdx[1] {
		return strings.TrimSpace(text[startIdx[1]:endIdx[0]])
	}
	// Layout 2: heading-based. Take from the heading line through the next
	// heading of equal or higher level (## or #), or end-of-text.
	loc := codingPlanHead.FindStringIndex(text)
	if loc == nil {
		return ""
	}
	after := text[loc[1]:]
	// Find the next heading at level 1 or 2 (## or #). Level 3+ (###) is
	// sub-content of the plan and stays inside the captured block.
	nextHead := regexp.MustCompile(`(?m)^#{1,2}\s+\S`).FindStringIndex(after)
	if nextHead == nil {
		return strings.TrimSpace(text[loc[0]:])
	}
	return strings.TrimSpace(text[loc[0] : loc[1]+nextHead[0]])
}

// ----------------------------------------------------------------------------
// Auto-orchestrate (主 Agent 自动编排子任务)
//
// 工作流:
//   1. 用户在 developer-chat（或直接 POST /orchestrate）说"开始执行"
//   2. wizard handler 像普通 developer-chat 一样跑主Agent一轮（system prompt
//      已被改写，要求输出 Markdown 表格 + JSON 子任务列表 + [SUBTASKS_READY]）
//   3. finalResult 中包含 [SUBTASKS_READY] 时，解析 JSON，自动创建 sub_tasks 行
//      并**串行**调度每个子Agent（沿用 StartSubTask 的 fork 主会话逻辑）
//   4. 所有子任务结束后，自动 fork 主Agent的 session（--resume <main_sid>），
//      把每个子任务的 artifact 作为上下文注入，触发主Agent生成汇总报告
//   5. 汇总报告写入 requirements.coding_plan，前端 SubTaskPanel 顶部展示
//
// 设计取舍:
//   - 串行而非并行：避免子任务改同一文件（worktree 已隔离但语义上仍冲突）
//   - 不阻塞原始 SSE：编排启动后立即返回，主Agent 输出 + 子任务进度通过各自
//     job_id 推给前端（不通过本编排 SSE 直接流），汇总报告则由前端轮询 Get
//   - 与手动 StartSubTask 共用 sub_tasks 表：自动批次与手动创建的子任务在
//     UI 上表现一致（状态、artifact、token 计量都一样）
// ----------------------------------------------------------------------------

// orchestratedSubtask is the JSON shape the main agent emits inside a code
// fence when the user asks for "开始执行". Parsed in orchestrator; non-nil
// prompt required (the orchestrator rejects empty prompts rather than
// spawning a useless child agent).
type orchestratedSubtask struct {
	Title  string `json:"title"`
	Prompt string `json:"prompt"`
}

// orchestratorPayload is the parsed JSON the main agent emits. The handler
// reads this after extracting the ```json block from finalResult.
type orchestratorPayload struct {
	Subtasks []orchestratedSubtask `json:"subtasks"`
}

// SUBTASKS_READY is the sentinel token the main agent must end its
// auto-orchestrate output with. Any text after the sentinel (until the next
// newline / EOF) is ignored; the sentinel itself is stripped from the saved
// chat history so the user-facing rendering never leaks "[SUBTASKS_READY]".
const SUBTASKS_READY = "[SUBTASKS_READY]"

// developerDecomposePrompt builds the -p prompt that triggers the developer
// role's task-decomposition + [SUBTASKS_READY] emission. Used by BOTH the
// fresh-session (sourceSID=="") and fork/resume (sourceSID!="") StartCoding
// branches so auto-orchestration fires regardless of whether the requirement
// went through the analyst/architect stages — the original bug was that only
// the fork branch carried this trigger, so skip-design / "直接开发" rows got a
// bare "## title\n\n desc" and the agent did the work itself (no sentinel →
// no children dispatched). leadIn is the single line that differs: the fresh
// path has no prior design to reference, so it asks the agent to read files
// first; the fork path references the already-completed analysis + design.
// Everything else (Markdown table → JSON block → [SUBTASKS_READY] sentinel,
// "don't write code yourself", "don't ask for confirmation") is shared
// verbatim so the two paths can never drift out of sync.
// subTasksFileRelPath is the well-known path (relative to the coding work
// dir) the developer main agent writes its sub-task decomposition to via the
// Write tool. The backend captures the Write tool_use input from the
// stream-json events (claudeStreamOutcome.subTasksJSON) — a structured
// channel that can't be broken by embedded code fences or prose, unlike the
// legacy "```json block + [SUBTASKS_READY] sentinel" text convention, which
// stays as the fallback.
const subTasksFileRelPath = ".novaworkbench/subtasks.json"

// subTasksFilePath returns the absolute path the decompose prompt tells the
// main agent to Write to. workDir is the coding worktree (or project dir).
func subTasksFilePath(workDir string) string {
	return filepath.Join(workDir, subTasksFileRelPath)
}

func developerDecomposePrompt(title, leadIn, workDir string) string {
	return fmt.Sprintf(
		"现在切换到「开发者」角色。用户已点击「开始开发」，正式进入执行实现阶段（需求：%s）。\n"+
			leadIn+
			"1. 输出一份 Markdown 任务分解表，让用户直观看到拆分结果；\n"+
			"2. **然后必须调用 Write 工具**，把拆分结果以 JSON 写入文件 %s ，格式：\n"+
			"   {\"subtasks\":[{\"title\":\"...\",\"prompt\":\"...\"}]}，每个子任务的 prompt 必须包含足够上下文（涉及文件、做什么改动、产物形式）；\n"+
			"3. 最后在回复中单独一行输出 [SUBTASKS_READY] 哨兵。\n"+
			"注意：第 2 步的 Write 文件是后端调度子Agent 的主要依据，务必调用 Write 工具完成，不要只在回复里贴 JSON。\n"+
			"要求：不要直接编写项目代码（由子Agent完成）；不要输出『等待确认』——直接给出拆分结果，后端检测到拆分文件后会自动串行调度子Agent执行并在全部完成后交回主Agent汇总。",
		title, subTasksFilePath(workDir))
}

// agentDirectPrompt builds the -p prompt for the "agent" role on Agent-Server
// execution. It deliberately omits the "开始开发/进入执行实现阶段" trigger that
// the developer role keys its sub-task orchestration emission on — the agent
// persona implements the requirement directly on the remote server in a single
// session (no sub-task orchestration, no sentinel). leadIn mirrors
// developerDecomposePrompt's wording so the two paths share intent.
//
// NB: the literal sentinel string is intentionally NOT mentioned anywhere in
// this prompt — past experience shows models occasionally honor an explicit
// "do not emit X" instruction by emitting X anyway (req_9d24ef181a5ad5c4).
// Routing this prompt through the wizard is what guarantees no sentinel: the
// orchestrator is short-circuited (see StartCoding) and the -p message
// contains no trigger phrase.
func agentDirectPrompt(title, leadIn, workDir string) string {
	return fmt.Sprintf(
		"现在切换到「Agent 开发者」角色，正在远程 Agent 服务器上执行需求（需求：%s，工作目录：%s）。\n"+
			leadIn+
			"直接使用 Read / Edit / Write / Bash 工具完成代码实现、构建与基础验证，并在结束时进行 git commit。\n"+
			"完成后在最终回复里简要说明：做了什么、关键文件、验证方式。",
		title, workDir)
}

// extractSubtasksPayload pulls the {"subtasks":[…]} JSON block out of
// finalResult and verifies the [SUBTASKS_READY] sentinel is present. Returns
// nil when either is missing — caller treats that as "main agent answered a
// normal question, not a decompose request" and just renders the chat reply.
//
// We intentionally do NOT use the existing extractJSON() brace matcher
// because the agent typically wraps the JSON in a ```json fence; this
// function locates the first ```json block, then JSON-decodes its contents.
// Falls back to a brace match when no fence is found (more permissive, lets
// the agent omit the fence in low-token responses).
func extractSubtasksPayload(text string) (*orchestratorPayload, bool) {
	// Also accept a truncated sentinel "[SUBTASKS_READY" (missing the closing
	// "]") — the model occasionally drops the last character when the stream
	// ends right at the sentinel boundary.
	hasSentinel := strings.Contains(text, SUBTASKS_READY) ||
		strings.Contains(text, "[SUBTASKS_READY")
	if !hasSentinel {
		return nil, false
	}
	body := text
	// Strip the sentinel itself so it doesn't bleed into the user-visible
	// chat rendering (front-end would otherwise render "[SUBTASKS_READY]" as
	// raw text after a successful orchestration).
	body = strings.ReplaceAll(body, SUBTASKS_READY, "")
	body = strings.ReplaceAll(body, "[SUBTASKS_READY", "") // truncated form

	// Prefer the ```json fence. Do NOT cut at the first "```" after the
	// fence: a subtask prompt may itself embed a code fence (e.g. a
	// "\n```css\n…\n```\n" snippet inside a JSON string value), and the
	// naive first-fence cut truncates the payload mid-JSON — the parse then
	// fails and the caller silently degrades to the Markdown table heuristic
	// (req_9d24ef181a5ad5c4). extractJSON brace-matches string-aware, so
	// fences inside quoted strings are skipped and the trailing fence is
	// ignored.
	var candidates []string
	if fenceStart := strings.Index(body, "```json"); fenceStart >= 0 {
		candidates = append(candidates, extractJSON(body[fenceStart+len("```json"):]))
	}
	// Fallback: take the first {...} JSON block in the whole response so
	// agents that omit the fence (or accidentally include prose around the
	// JSON) still parse correctly.
	candidates = append(candidates, extractJSON(body))
	for _, raw := range candidates {
		if p := decodeSubtasksPayload(raw); p != nil {
			return p, true
		}
	}
	log.Printf("[orchestrate] JSON parse failed: no usable subtasks payload in %d candidate(s)", len(candidates))
	return nil, false
}

// decodeSubtasksPayload unmarshals one candidate JSON blob and normalizes it.
// Returns nil on any parse/validation failure so the caller can try the next
// candidate. Kept separate from extractSubtasksPayload so the fence and
// brace-match candidates share one strict decode path.
func decodeSubtasksPayload(raw string) *orchestratorPayload {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var p orchestratorPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil
	}
	return normalizePayload(&p)
}

// extractSubtasksFromMarkdown is a permissive fallback used when the main
// agent emits a Markdown task breakdown table but forgets to wrap the
// matching JSON in a code fence (or forgets the [SUBTASKS_READY] sentinel).
// The UX promise is "主Agent 拆分任务 → 后端自动派发", so we degrade
// gracefully instead of leaving the user staring at a "未派发" panel.
//
// Heuristic (matches what the developer role prompt asks the agent to write):
//  1. Locate the "## 任务分解" / "## 子任务" / "## 子任务清单" / "## 任务清单"
//     heading (case-insensitive, trimmed).
//  2. From the heading line onward, grab consecutive list items:
//     - "- " or "* " or numbered "1. " markdown items
//     - "**N. 标题**：提示词" — the agent's compressed form, separated by "：" / ":"
//     - "| 列 | 列 |" table rows starting from the 2nd data row
//  3. Skip blank lines; require at least 2 items to consider it a real plan
//     (one-liner instructions are usually prose, not a decomposition).
//
// Returns nil when nothing usable is found; caller logs + skips dispatch.
func extractSubtasksFromMarkdown(text string) *orchestratorPayload {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	// 1. Find the breakdown heading.
	headingRe := regexp.MustCompile(`(?m)^#{1,3}\s*(任务分解表?|子任务|子任务清单|任务清单|任务拆分|子Agent\s*协作|子Agent协作)\s*$`)
	loc := headingRe.FindStringIndex(text)
	if loc == nil {
		return nil
	}
	after := text[loc[1]:]
	// Stop scanning at the next sibling heading (level 1 or 2). Level 3+ is
	// sub-content of the plan and stays inside.
	if next := regexp.MustCompile(`(?m)^#{1,2}\s+\S`).FindStringIndex(after); next != nil {
		after = after[:next[0]]
	}

	// Trailing prose lines like "等待用户确认" / "完成后..." / "其他内容略。"
	// aren't part of the task list — stop the scan when the line is plain
	// prose with no list/table markers. We still keep lines whose content
	// looks like a "标题：xxx" entry (heuristic: has a colon before the first
	// non-space char of length >= 2, OR starts with a list marker, OR is a
	// table row).
	type entry struct {
		title  string
		prompt string
	}
	var entries []entry
	lines := strings.Split(after, "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}

		// Skip obvious non-entries (table dividers, code fences).
		if regexp.MustCompile(`^[-=|:\s]+$`).MatchString(line) {
			continue
		}
		if strings.HasPrefix(line, "```") {
			continue
		}

		// Skip plain prose lines without list / table markers — these are
		// the agent's closing remark, not a task entry. The check is
		// strict: the line must start with one of the recognized markers.
		isListItem := false
		for _, prefix := range []string{"- ", "* ", "• ", "‣ ", "| "} {
			if strings.HasPrefix(line, prefix) {
				isListItem = true
				break
			}
		}
		isNumbered := regexp.MustCompile(`^\d+\.\s+\S`).MatchString(line)
		if !isListItem && !isNumbered {
			// Plain prose → stop scanning, the plan section is over.
			break
		}

		// Strip common markdown list / numbering prefixes.
		stripped := line
		for _, prefix := range []string{"- ", "* ", "• ", "‣ "} {
			if strings.HasPrefix(stripped, prefix) {
				stripped = strings.TrimSpace(stripped[len(prefix):])
				break
			}
		}
		// Numbered prefix: "1. ", "2. ", … (single trailing dot).
		if m := regexp.MustCompile(`^\d+\.\s+`).FindStringIndex(stripped); m != nil {
			stripped = strings.TrimSpace(stripped[m[1]:])
		}

		// Strip ALL ** (markdown bold) — pairs of asterisks can sit
		// anywhere in the line ("**修复登录 bug**" → "修复登录 bug").
		stripped = stripAllBold(stripped)

		// Table row: split into cells so "| 实现缓存层 | 在 …" →
		// title="实现缓存层", prompt="在 …". We handle this BEFORE the
		// colon/separator step so a table row never falls through to the
		// default "whole line is the prompt" branch.
		isTableRow := strings.HasPrefix(stripped, "|")
		if isTableRow {
			cells := splitTableRowCells(stripped)
			if len(cells) >= 2 {
				// Lead index column: tables shaped "| # | 子任务 | 涉及文件 | …"
				// put the real title in the SECOND cell. Without this shift the
				// header row becomes a bogus child (title="#", prompt="子任务 | …")
				// and every data row gets a numbered title ("1", "2", …) — this
				// is exactly what dispatched the garbage sub-task in
				// req_9d24ef181a5ad5c4.
				if isTableIndexCell(cells[0]) {
					cells = cells[1:]
				}
				tableTitle := cells[0]
				tablePrompt := strings.Join(cells[1:], " | ")
				if tablePrompt == "" {
					// Single-content-column row after the shift — keep the
					// title as the prompt so normalizePayload doesn't drop it.
					tablePrompt = tableTitle
				}
				// Filter the header row (its title cell is one of the marker
				// words) and any other obvious non-entries.
				if isTableHeaderCell(tableTitle) {
					continue
				}
				entries = append(entries, entry{title: tableTitle, prompt: tablePrompt})
				continue
			}
		}

		// Two shapes (non-table rows):
		//   "标题：提示词" / "标题: 提示词"
		//   "标题 — 提示词" / "标题 - 提示词"
		var title, prompt string
		for _, sep := range []string{"：", ":", "—", " — ", " - "} {
			if idx := strings.Index(stripped, sep); idx > 0 && idx < len(stripped)-1 {
				title = strings.TrimSpace(stripped[:idx])
				prompt = strings.TrimSpace(stripped[idx+len(sep):])
				break
			}
		}
		if title == "" {
			// Whole line is the title/prompt; treat first 40 chars as title
			// and the whole thing as the prompt so the panel renders
			// something usable.
			prompt = stripped
			title = truncateForLog(stripped, 40)
		}
		// Filter: skip table headers like "子任务" / "提示词" / "描述" that
		// the agent sometimes leaves in row 1.
		lowerTitle := strings.ToLower(title)
		if title == "" || strings.HasPrefix(lowerTitle, "子任务") || strings.HasPrefix(lowerTitle, "任务") || strings.HasPrefix(lowerTitle, "提示词") || strings.HasPrefix(lowerTitle, "说明") || strings.HasPrefix(lowerTitle, "步骤") {
			continue
		}
		entries = append(entries, entry{title: title, prompt: prompt})
	}
	if len(entries) < 2 {
		// A single bullet is usually prose ("- 等等") — don't auto-dispatch
		// on it; the user can still copy-paste into the manual creator.
		return nil
	}
	p := &orchestratorPayload{Subtasks: make([]orchestratedSubtask, 0, len(entries))}
	for _, e := range entries {
		p.Subtasks = append(p.Subtasks, orchestratedSubtask{Title: e.title, Prompt: e.prompt})
	}
	return normalizePayload(p) // normalizePayload may still return nil if everything cleaned out
}

// splitTableRowCells turns a Markdown table row into its trimmed cells:
// "| a | b |" → ["a", "b"]. Empty cells are kept so column positions stay
// aligned (the index-shift heuristic in the caller depends on position).
func splitTableRowCells(row string) []string {
	row = strings.TrimSpace(strings.Trim(row, "|"))
	parts := strings.Split(row, "|")
	cells := make([]string, 0, len(parts))
	for _, c := range parts {
		cells = append(cells, strings.TrimSpace(c))
	}
	return cells
}

// isTableIndexCell reports whether a leading table cell is just a row index
// ("#", "1", "42", "序号", "编号") rather than real content — the cue that the
// actual task title lives in the NEXT column.
func isTableIndexCell(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if s == "#" || s == "序号" || s == "编号" {
		return true
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isTableHeaderCell reports whether a table title cell is really a header
// label (子任务 / 提示词 / 涉及文件 / …) that must not become a sub-task.
// Broader than the inline list-item filter because breakdown tables carry
// more column kinds (涉及文件 / 关键改动 / 产物).
func isTableHeaderCell(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	if lower == "" {
		return true
	}
	for _, marker := range []string{
		"子任务", "任务", "提示词", "说明", "步骤",
		"标题", "描述", "涉及文件", "关键改动", "产物",
		"序号", "编号", "#",
	} {
		if strings.HasPrefix(lower, marker) {
			return true
		}
	}
	return false
}

// stripAllBold removes every pair of ** asterisks from s, including
// mid-string occurrences (so "**修复登录 bug** — 在 auth/login.go" →
// "修复登录 bug — 在 auth/login.go"). The Markdown `*single*` form is left
// alone — only the bold double-asterisk variant collides with our
// title/prompt separator heuristics.
func stripAllBold(s string) string {
	for strings.Contains(s, "**") {
		s = strings.ReplaceAll(s, "**", "")
	}
	return s
}

// normalizePayload drops empty / malformed entries (prompt required) and
// auto-fills a missing title from the first 40 chars of the prompt. Returns
// nil when no usable subtask survived — caller treats that as "no plan".
func normalizePayload(p *orchestratorPayload) *orchestratorPayload {
	if p == nil {
		return nil
	}
	cleaned := p.Subtasks[:0]
	for _, s := range p.Subtasks {
		if strings.TrimSpace(s.Prompt) == "" {
			continue
		}
		if s.Title == "" {
			s.Title = truncateForLog(s.Prompt, 40)
		}
		cleaned = append(cleaned, s)
	}
	p.Subtasks = cleaned
	if len(p.Subtasks) == 0 {
		return nil
	}
	return p
}

// dispatchChildrenSequential is a small free function that runs dispatchOneChild
// in a strict loop. Shared by StartCoding's auto-orchestrate path (the new
// behavior; see tryAutoOrchestrate) — kept free-standing so each call site
// stays one-liner clean. Errors per-child are logged and skipped; the returned
// slice is whatever ids survived — empty means the entire batch failed.
func dispatchChildrenSequential(
	reqID, orchestratorSID string,
	req *model.Requirement,
	h *WizardHandler,
	subtasks []orchestratedSubtask,
	workDir, modelName string,
) []string {
	subTaskIDs := make([]string, 0, len(subtasks))
	for _, t := range subtasks {
		st, err := h.dispatchOneChild(reqID, orchestratorSID, t, req, workDir, modelName)
		if err != nil {
			log.Printf("[orchestrate] failed to dispatch child %q: %v", t.Title, err)
			continue
		}
		subTaskIDs = append(subTaskIDs, st.ID)
	}
	return subTaskIDs
}

// summaryKickoff launches the orchestrator summary round in its own goroutine.
// Used by both AutoOrchestrate and tryAutoOrchestrate so the trigger sites stay
// symmetric.
func summaryKickoff(
	reqID, orchestratorSID string,
	req *model.Requirement,
	h *WizardHandler,
	workDir, modelName string,
	subTaskIDs []string,
) {
	go h.runOrchestratorSummary(reqID, orchestratorSID, req, workDir, modelName, subTaskIDs)
}

// tryAutoOrchestrate is the auto-dispatch path called by StartCoding right
// after the main agent turn finishes. It resolves the main agent's
// decomposition via resolveSubtasksPayload (Write-captured JSON → sentinel
// text → markdown table → LLM extractor → single fallback child) and
// dispatches each sub-task SEQUENTIALLY through the same dispatchOneChild
// path the manual orchestrator uses, then schedules an orchestrator summary
// round.
//
// This is the entry point for the new "一键编排 = 主 Agent 自动派发" UX:
// StartCoding returns its job_id immediately, and the orchestrator-side
// progress (parse → dispatch N children → write summary) runs as a separate
// background goroutine. Frontend progress is fully observable via:
//   - /api/requirements/{id}/sub-tasks          → live status of each child
//   - /api/wizard/jobs/{child_job_id}/stream    → live tool calls of each
//     child
//   - requirements.coding_plan refresh          → final summary Markdown
//   - /api/requirements/{id}                    → requirements.coding_plan
//     surfaces the summary on the next GET.
func (h *WizardHandler) tryAutoOrchestrate(
	reqID string,
	orchestratorSID string,
	finalResult string,
	capturedJSON string,
	req *model.Requirement,
	workDir, modelName string,
) {
	if h.subTaskSvc == nil {
		return
	}
	if req == nil {
		// Defensive: shouldn't happen (StartCoding fetched it before
		// dispatching this goroutine), but skip cleanly if so.
		log.Printf("[auto-orchestrate] %s: requirement row missing, skipping dispatch", reqID)
		return
	}
	payload := h.resolveSubtasksPayload(reqID, finalResult, capturedJSON, req)
	// The subtasks.json the main agent Wrote into the worktree has been
	// consumed (or rejected) — remove it so it never pollutes the dev branch
	// or gets mistaken for a fresh decomposition on the next turn.
	if workDir != "" {
		if rerr := os.Remove(subTasksFilePath(workDir)); rerr != nil && !os.IsNotExist(rerr) {
			log.Printf("[orchestrate] %s: failed to remove %s: %v", reqID, subTasksFilePath(workDir), rerr)
		}
	}
	if payload == nil {
		return
	}

	// Children share orchestratorSID (the coding session forked in
	// StartCoding) so they inherit the main agent's project / design /
	// conversation context. Sequential dispatch keeps file edits safe in
	// the shared worktree.
	subTaskIDs := dispatchChildrenSequential(reqID, orchestratorSID, req, h, payload.Subtasks, workDir, modelName)
	if len(subTaskIDs) == 0 {
		log.Printf("[auto-orchestrate] %s: dispatch produced 0 children; ending", reqID)
		return
	}
	log.Printf("[auto-orchestrate] %s: dispatched %d children, scheduling summary", reqID, len(subTaskIDs))
	summaryKickoff(reqID, orchestratorSID, req, h, workDir, modelName, subTaskIDs)
}

// resolveSubtasksPayload turns the main agent's turn output into a concrete
// sub-task list. The channels are tried in strict reliability order:
//
//  1. Write-captured subtasks.json (structured tool_use input — the primary
//     channel; cannot be mangled by prose / code fences).
//  2. ```json fence + [SUBTASKS_READY] sentinel in the reply text.
//  3. Markdown breakdown table heuristic.
//  4. A cheap single-shot LLM extractor over the raw reply (one retry with
//     the parse error fed back).
//  5. A single fallback child covering the whole requirement — the UX
//     promise is "开始开发后一定有子 Agent 在工作", so the pipeline never
//     stalls at zero children.
//
// Returns nil only when there is nothing to dispatch at all (empty reply
// AND empty requirement prompt source). Shared by tryAutoOrchestrate and
// ReOrchestrate so the manual re-split behaves identically.
func (h *WizardHandler) resolveSubtasksPayload(
	reqID, finalResult, capturedJSON string,
	req *model.Requirement,
) *orchestratorPayload {
	// 1. Write-captured JSON (primary).
	if p := decodeSubtasksPayload(capturedJSON); p != nil {
		log.Printf("[orchestrate] %s: using Write-captured subtasks.json (%d subtasks)", reqID, len(p.Subtasks))
		return p
	}
	if strings.TrimSpace(capturedJSON) != "" {
		log.Printf("[orchestrate] %s: Write-captured subtasks.json failed to decode, falling through to text parsing", reqID)
	}

	if strings.TrimSpace(finalResult) != "" {
		// 2. Sentinel + JSON text block.
		if p, ok := extractSubtasksPayload(finalResult); ok {
			log.Printf("[orchestrate] %s: using sentinel+JSON text block (%d subtasks)", reqID, len(p.Subtasks))
			return p
		}
		// 3. Markdown table heuristic.
		if p := extractSubtasksFromMarkdown(finalResult); p != nil {
			log.Printf("[orchestrate] %s: no sentinel; using markdown plan (%d subtasks)", reqID, len(p.Subtasks))
			return p
		}
		// 4. LLM extractor (single-shot, one retry with the parse error).
		if p := h.extractSubtasksWithLLM(reqID, finalResult); p != nil {
			log.Printf("[orchestrate] %s: using LLM-extracted subtasks (%d)", reqID, len(p.Subtasks))
			return p
		}
	}

	// 5. Single fallback child: the whole requirement as one task.
	title := req.Title
	prompt := "## 需求\n\n" + req.Title
	if d := strings.TrimSpace(req.Description); d != "" {
		prompt += "\n\n" + d
	}
	prompt += "\n\n> 说明：主 Agent 未能给出可用的任务拆分，请直接基于项目上下文完成整个需求。"
	if title == "" {
		title = "执行整个需求"
	}
	log.Printf("[auto-orchestrate] %s: all parse channels failed — dispatching whole requirement as one fallback child", reqID)
	return &orchestratorPayload{Subtasks: []orchestratedSubtask{{Title: truncateForLog(title, 40), Prompt: prompt}}}
}

// extractSubtasksWithLLM is fallback channel 4: asks a cheap single-shot
// claude call to convert the main agent's prose reply into the
// {"subtasks":[…]} JSON, retrying once with the decode error fed back.
// Returns nil when both attempts fail or the extraction yields no usable
// entries.
func (h *WizardHandler) extractSubtasksWithLLM(reqID, finalResult string) *orchestratorPayload {
	feedback := ""
	for attempt := 1; attempt <= 2; attempt++ {
		raw, err := h.llm.ExtractSubtasksJSON(finalResult, feedback)
		if err != nil {
			log.Printf("[orchestrate] %s: extractor attempt %d failed: %v", reqID, attempt, err)
			return nil
		}
		if p := decodeSubtasksPayload(extractJSON(raw)); p != nil {
			return p
		}
		feedback = truncateForLog(raw, 200)
		log.Printf("[orchestrate] %s: extractor attempt %d returned undecodable JSON, retrying", reqID, attempt)
	}
	return nil
}

// dispatchOneChild is the inner loop of AutoOrchestrate: persists a
// sub_tasks row, spawns the claude process, blocks until it finishes, and
// returns the final sub-task record. Errors are non-fatal — the caller
// skips and continues with remaining children.
func (h *WizardHandler) dispatchOneChild(
	reqID, parentSID string,
	t orchestratedSubtask,
	req *model.Requirement,
	workDir, modelName string,
) (*model.SubTask, error) {
	st, err := h.subTaskSvc.Create(reqID, t.Title, t.Prompt)
	if err != nil {
		return nil, fmt.Errorf("create sub_task: %w", err)
	}
	// Pre-mint child session id (forked from the orchestrator/main session).
	childSID := util.NewUUID()
	if perr := h.subTaskSvc.UpdateSession(st.ID, childSID, parentSID); perr != nil {
		log.Printf("[orchestrate] failed to persist child session for %s: %v", st.ID, perr)
	}
	job := h.jobs.Create(reqID)
	if perr := h.subTaskSvc.UpdateJobID(st.ID, job.ID); perr != nil {
		log.Printf("[orchestrate] failed to persist child job_id for %s: %v", st.ID, perr)
	}

	// Start the child agent (same code path as StartSubTask). The "executor"
	// role system prompt MUST be injected: the child forks the orchestrator
	// session, which carries the developer (统筹协调) persona telling it to
	// decompose and emit [SUBTASKS_READY]. Without an explicit override the
	// child inherits that persona and re-emits the sentinel instead of
	// writing any code.
	execSystemPrompt, _ := h.roleConfig(executorRoleKey)
	executorPrompt := "## 子任务\n\n" + t.Prompt + "\n\n" +
		"> 本任务通过 --fork-session 继承了主 Agent 的项目上下文与代码库访问权限。\n" +
		"> 如需补充信息，可正常读取项目文件或调用工具。\n" +
		"> 你是执行者：请直接动手实现本子任务并落盘代码改动，不要再做任务拆分。\n"
	cmd := h.llm.GenerateCode(llm.StreamOpts{
		Prompt:        executorPrompt,
		WorkDir:       workDir,
		SystemPrompt:  execSystemPrompt,
		Model:         cliModelArg(modelName),
		SessionID:     parentSID,
		Resume:        true,
		Fork:          true,
		ForkSessionID: childSID,
	})
	startTime, err := h.subTaskSvc.MarkRunning(st.ID)
	if err != nil {
		log.Printf("[orchestrate] failed to mark running for %s: %v", st.ID, err)
	}

	job.Append(store.LogLine{Type: "phase", Content: "🤖 [编排] 子任务启动: " + t.Title})
	job.Append(store.LogLine{Type: "message", Content: "📝 提示词: " + truncateForLog(t.Prompt, 240)})
	job.SetModel(modelName)

	childUsage := h.usageCtxFor("sub_task", reqID, req.ProjectID, job.ID, modelName, "", t.Prompt)
	out := runClaudeStream(jobSink{job}, cmd, "sub-task", childUsage)

	status := model.SubTaskStatusDone
	artifactBody := out.finalResult
	if out.staleSession {
		status = model.SubTaskStatusError
		artifactBody = "❌ 源会话已失效（session 文件不存在），请重新发起 coding 后再试。"
	} else if out.errMsg != "" {
		status = model.SubTaskStatusError
		artifactBody = "❌ " + out.errMsg
	} else if out.finalResult == "" {
		status = model.SubTaskStatusError
		artifactBody = "❌ Claude 未返回结果，请重试"
	}
	if status != model.SubTaskStatusError {
		job.Append(store.LogLine{Type: "result", Content: strings.TrimSpace(out.finalResult)})
	} else {
		job.Append(store.LogLine{Type: "error", Content: artifactBody})
	}
	job.Append(store.LogLine{Type: "done", Content: "✅ 子任务完成！"})

	artifact := buildSubTaskArtifact(st, modelName, artifactBody, time.Now())
	tokens := model.SubTaskTokens{
		Input:         out.lastUsage.InputTokens,
		Output:        out.lastUsage.OutputTokens,
		CacheCreation: out.lastUsage.CacheCreationTokens,
		CacheRead:     out.lastUsage.CacheReadTokens,
	}
	if perr := h.subTaskSvc.Finish(st.ID, status, artifact, modelName, tokens, 0, startTime); perr != nil {
		log.Printf("[orchestrate] failed to persist finish for %s: %v", st.ID, perr)
	}
	// Persist job log too (mirrors StartSubTask's defer — survives restart).
	lines, jstatus, exitCode := job.Snapshot()
	if perr := h.jobLogSvc.Save(job.ID, reqID, string(jstatus), exitCode, job.StartedAt, job.FinishedAt, lines, modelName); perr != nil {
		log.Printf("[orchestrate] failed to persist job log %s: %v", job.ID, perr)
	}
	job.Finish(0, store.JobDone)
	log.Printf("[orchestrate] child %s finished status=%s", st.ID, status)

	// Return a fresh read of the row (Finish updated artifact / status).
	return h.subTaskSvc.Get(st.ID)
}

// runOrchestratorSummary forks the orchestrator (or main) session and asks
// the agent to summarize all completed children. The summary is the final
// user-facing report the SubTaskPanel surfaces under the children's cards.
//
// Invoked from AutoOrchestrate as a goroutine so the HTTP response can
// return child ids immediately. Errors are logged, never returned — a failed
// summary just leaves coding_plan empty (the user can manually inspect
// each child's artifact).
func (h *WizardHandler) runOrchestratorSummary(
	reqID, orchestratorSID string,
	req *model.Requirement,
	workDir, modelName string,
	subTaskIDs []string,
) {
	// Collect each child's artifact + status. Sort by created_at so the
	// summary reads in execution order.
	children := make([]model.SubTask, 0, len(subTaskIDs))
	for _, sid := range subTaskIDs {
		st, err := h.subTaskSvc.Get(sid)
		if err != nil {
			continue
		}
		children = append(children, *st)
	}
	if len(children) == 0 {
		return
	}

	// Build the summary prompt: stitch each child's artifact in execution
	// order. Cap each child at 4KB so a chatty child doesn't blow up the
	// main agent's context.
	var summaryB strings.Builder
	summaryB.WriteString("所有编排子任务已完成。请基于以下子任务产物，输出一份 Markdown 汇总报告，")
	summaryB.WriteString("用于让用户一眼看到：\n1. 整体进展概述\n2. 各子任务的关键成果\n3. 修改的文件清单（按子任务组织）\n4. 整体遗留风险\n\n")
	summaryB.WriteString("## 子任务产物\n\n")
	for i, st := range children {
		fmt.Fprintf(&summaryB, "### %d. %s (%s)\n", i+1, st.Title, st.Status)
		body := st.Artifact
		if len(body) > 4096 {
			body = body[:4096] + "\n…（已截断）"
		}
		summaryB.WriteString(body)
		summaryB.WriteString("\n\n")
	}
	summaryB.WriteString("---\n请直接输出汇总报告 Markdown。")

	// Resume the orchestrator session — it's the main-agent thread that
	// already saw the decompose prompt, so re-resuming lets it carry
	// forward the requirements/design context plus its own decompose
	// reasoning. ForkSession=false: we want a continuation, not a new
	// session (the summary is a follow-up message in the same thread).
	job := h.jobs.Create(reqID)
	job.Append(store.LogLine{Type: "phase", Content: "📊 主 Agent 正在汇总子任务产物..."})
	job.SetModel(modelName)

	cmd := h.llm.GenerateCode(llm.StreamOpts{
		Prompt:       summaryB.String(),
		WorkDir:      workDir,
		SystemPrompt: "", // resumed session already has developer persona
		Model:        cliModelArg(modelName),
		SessionID:    orchestratorSID,
		Resume:       true,
		Fork:         false,
	})

	summaryUsage := h.usageCtxFor("orchestrate_summary", reqID, req.ProjectID, job.ID, modelName, "", "auto-summary")
	out := runClaudeStream(jobSink{job}, cmd, "orchestrate-summary", summaryUsage)

	if out.errMsg != "" || out.finalResult == "" {
		log.Printf("[orchestrate] summary turn failed: %s / empty=%v", out.errMsg, out.finalResult == "")
		job.Finish(1, store.JobError)
		return
	}

	// Persist the Markdown summary on the requirement. The SubTaskPanel
	// reads it on the next GET and renders it above the children.
	if perr := h.reqSvc.UpdateCodingPlan(reqID, out.finalResult); perr != nil {
		log.Printf("[orchestrate] failed to persist coding_plan for %s: %v", reqID, perr)
	}
	job.Append(store.LogLine{Type: "result", Content: strings.TrimSpace(out.finalResult)})
	job.Append(store.LogLine{Type: "done", Content: "✅ 汇总完成！"})
	job.Finish(0, store.JobDone)
	log.Printf("[orchestrate] summary saved to requirements.coding_plan for %s", reqID)
}

// silentSink is a streamSink that discards log output. AutoOrchestrate runs
// the orchestrator's main-agent turn inline (synchronously) so the
// streaming output is invisible to the user — only the terminal
// finalResult matters. We still go through runClaudeStream so we get the
// same token-usage recording as developer-chat.
type silentSink struct{}

func (silentSink) emit(line store.LogLine) {}
