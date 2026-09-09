// wizard_helpers.go: extracted from wizard.go as part of the refactoring.

package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/store"
)

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

// roleConfig loads a role's system prompt + effective model + the
// Claude config the role is bound to. The returned model is the EFFECTIVE
// model (role override → role's bound config's default → global active
// config's default → "默认模型" literal). The configID is non-empty whenever
// the role has its own binding or a global active config exists; it must be
// threaded into StreamOpts.ClaudeConfigID so the gateway injects the right
// auth + base URL into the claude subprocess. Pass the model through
// cliModelArg before handing it to StreamOpts.Model so the sentinel never
// reaches the CLI.
//
// A missing/broken role config never blocks the wizard pipeline: we return
// empty system prompt + the resolved default model + the active config id
// (or "" when no config exists), and log a one-line warning.
func (h *WizardHandler) roleConfig(key string) (systemPrompt, model, configID string) {
	r, err := h.roleSvc.GetByKey(key)
	if err != nil {
		log.Printf("[wizard] role %q not found, using CLI defaults: %v", key, err)
		return "", h.effectiveModelFromConfig("", nil), h.activeConfigID()
	}
	cfg, _ := h.claudeCfg.ResolveRoleConfig(r)
	if cfg != nil {
		return r.SystemPrompt, effectiveModelFromValues(r.Model, cfg.DefaultModel), cfg.ID
	}
	return r.SystemPrompt, h.effectiveModelFromConfig(r.Model, r), h.activeConfigID()
}

// resolveConfigIDForRun returns the claude_config_id the wizard should pass
// into llm.StreamOpts for a stage run, honouring the user-picked config and
// aligning it with the user-picked model.
//
// Priority:
//  1. requestCl — explicit per-request override from the UI picker
//     (frontend ModelSelect lifts the picked config id into the request body)
//  2. ResolveConfigForModel(model) — the config whose models list owns the
//     picked model id; covers the case where the user picks a model from a
//     non-active config without explicitly choosing the config (e.g. the
//     legacy wizard page that has no ModelSelect)
//  3. devCfgID — role binding resolved by roleConfig (covers developer-
//     default model + role-bound gateway)
//  4. h.activeConfigID() — legacy global fallback
//
// Any empty/error step falls through; the chain never errors out so a broken
// picker state cannot block a run. Mirrors the same priority chain used by
// sub_task_runner.go for sub-task dispatch (see f10e1cc).
func (h *WizardHandler) resolveConfigIDForRun(requestCl, model, devCfgID string) string {
	if requestCl != "" {
		return requestCl
	}
	if model != "" {
		if cid, err := h.claudeCfg.ResolveConfigForModel(model); err == nil && cid != "" {
			return cid
		}
	}
	if devCfgID != "" {
		return devCfgID
	}
	return h.activeConfigID()
}

// effectiveModel resolves the model that will actually be dispatched to the
// claude CLI for a role. roleModel is the role's explicit override. Falls
// back to the role's bound config's default, then the global active
// config's default, then the "默认模型" literal. See effectiveModelFromValues.
//
// This helper is the legacy "no role row in hand" path — prefer
// effectiveModelFromConfig for the modern role binding resolution.
func (h *WizardHandler) effectiveModel(roleModel string) string {
	configDefault := ""
	if h.claudeCfg != nil {
		if _, dm, err := h.claudeCfg.ActiveModels(); err == nil {
			configDefault = dm
		}
	}
	return effectiveModelFromValues(roleModel, configDefault)
}

// effectiveModelFromConfig is the per-role variant: it uses the role's bound
// Claude config (when present) instead of the global active config. A nil
// role → the legacy global-active path so the caller doesn't have to special
// case "no role row" before calling.
func (h *WizardHandler) effectiveModelFromConfig(roleModel string, role *model.Role) string {
	configDefault := ""
	if h.claudeCfg != nil {
		if cfg, _ := h.claudeCfg.ResolveRoleConfig(role); cfg != nil {
			configDefault = cfg.DefaultModel
		}
	}
	return effectiveModelFromValues(roleModel, configDefault)
}

// activeConfigID returns the active claude_configs row's id (or "" when no
// config is active). Used as the StreamOpts.ClaudeConfigID fallback when a
// role has no binding of its own.
func (h *WizardHandler) activeConfigID() string {
	if h.claudeCfg == nil {
		return ""
	}
	c, err := h.claudeCfg.ActiveConfig()
	if err != nil || c == nil {
		return ""
	}
	return c.ID
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
