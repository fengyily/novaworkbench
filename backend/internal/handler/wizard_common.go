package handler

// Shared helpers used across the wizard pipeline (analyst → architect →
// coding → sub-task / orchestration). Moved out of wizard.go as part of the
// file-split refactor; this file holds no business logic — only utilities
// referenced by more than one cluster (role/model resolution, usage accounting
// metadata, worktree anchoring, SSE sendStatus).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

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

// resolveWorkDirLogged is the log-echoing variant of resolveWorkDir. See the
// doc comment on resolveWorkDir for the anchoring rationale — the only
// difference is that the worktree-creation step funnels its diagnostic lines
// (e.g. "🔄 已同步 origin/main", "⬆️ 已从 origin/main 更新") through logf when
// the caller (typically a wizard Job) wants to surface them in the SSE panel.
// logf may be nil; nil disables log echoing silently so the legacy callers
// keep working unchanged.
func (h *WizardHandler) resolveWorkDirLogged(req *model.Requirement, projectPath, defaultBranch string, logf func(string)) (string, error) {
	if req == nil || projectPath == "" {
		return projectPath, nil
	}
	// Validate the project path exists before any git operations. A missing
	// directory normally causes gitRun to fail with a generic error that
	// EnsureWorktreeLogged maps to ErrNotAGitRepo, and exec.Cmd.Start() then
	// chdirs to a non-existent path and fails with an opaque ENOENT. In Docker
	// deployments the workspace bind-mount may be empty after a container
	// rebuild, so auto-restore from the project's stored remote before giving
	// up.
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
	wtPath, err := EnsureWorktreeLogged(projectPath, req.ID, branch, defaultBranch, logf)
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
//
// This is the log-silent variant; pass resolveWorkDirLogged directly when the
// caller has a Job and wants the sync lines to surface in the SSE panel.
func (h *WizardHandler) resolveWorkDir(req *model.Requirement, projectPath, defaultBranch string) (string, error) {
	return h.resolveWorkDirLogged(req, projectPath, defaultBranch, nil)
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

// autoPushPR dispatches the shared "提交 → 推送 → 创建 PR" child agent for a
// requirement whose development has just finished. It is called (always in a
// goroutine) from the three development-completion points — local non-split
// coding, Agent-server coding, and the split orchestrator summary — gated on
// the requirement's auto_push toggle, so a finished requirement ships its
// result without a manual "推送并发起 PR" click.
//
// Every failure mode is a logged skip, never a hard error: auto-push is a
// convenience layer on top of an already-completed development run and must
// never turn a successful run into a visible failure. Guards (each returns
// after one log line): no sub-task runner wired, project lookup failure,
// detached HEAD / missing dev branch, or no git remote to push to.
//
// Re-running is safe: the push child is idempotent (git push -u repeats
// cleanly; PR creation returns the existing PR when one already exists), so
// an overlapping manual + automatic trigger, or a summary goroutine re-run
// after a restart, cannot corrupt anything.
func (h *WizardHandler) autoPushPR(reqRow *model.Requirement) {
	if reqRow == nil {
		return
	}
	if h.subTaskRunner == nil || h.subTaskSvc == nil {
		log.Printf("[auto-push] %s: sub-task runner not wired, skip", reqRow.ID)
		return
	}
	proj, perr := h.projectSvc.Get(reqRow.ProjectID)
	if perr != nil || proj == nil {
		log.Printf("[auto-push] %s: project load failed (%v), skip", reqRow.ID, perr)
		return
	}
	dir := proj.LocalPath
	base := proj.DefaultBranch
	if base == "" {
		base = "main"
	}
	platformType := proj.PlatformType

	// Resolve the dev branch the same way the manual push does. An
	// origin-transport Agent-server requirement has no local checkout, so the
	// branch name comes from the requirement row; every other case (local, or
	// local-sync Agent whose code was synced back to the local worktree) reads
	// the worktree/checkout HEAD.
	var dev string
	if codeLivesOnAgent(reqRow) {
		dev = remoteBranchFor(reqRow)
	} else {
		dev, _ = devBranchAndDir(reqRow, dir)
	}
	if dev == "" || dev == "HEAD" {
		log.Printf("[auto-push] %s: no dev branch (detached HEAD?), skip", reqRow.ID)
		return
	}

	// Need a remote to push to. Prefer the checkout's configured origin; fall
	// back to the project's stored remote_url (Agent-server / freshly-cloned
	// cases where the local checkout may not have origin set). No remote → skip
	// silently — a repo with no remote can't be pushed and that's not an error.
	remote := ""
	if dir != "" {
		remote = remoteURL(dir)
	}
	if remote == "" {
		remote = proj.RemoteURL
	}
	if remote == "" {
		log.Printf("[auto-push] %s: no git remote configured, skip", reqRow.ID)
		return
	}

	// Effective model: pr_author role (matches the manual push default). The
	// "默认模型" sentinel collapses to "" via cliModelArg so the runner falls
	// back to its own resolution instead of persisting the display literal.
	_, prModel, roleConfigID := h.roleConfig("pr_author")
	prModel = cliModelArg(prModel)

	jobID, subTaskID, err := dispatchPushPRSubTask(h.subTaskRunner, reqRow, dev, base, remote, platformType, "", prModel, roleConfigID)
	if err != nil {
		log.Printf("[auto-push] %s: dispatch failed: %v", reqRow.ID, err)
		return
	}
	log.Printf("[auto-push] %s: dispatched push+PR sub-task %s job %s branch=%s", reqRow.ID, subTaskID, jobID, dev)
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// isLikelyJSON reports whether s looks like a JSON object (starts with '{' after
// trimming whitespace). Used to distinguish plan-markdown design docs from the
// legacy JSON design schema.
func isLikelyJSON(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "{")
}

func sendStatus(w io.Writer, rc *http.ResponseController, typ string, content string) {
	jsonLine, _ := json.Marshal(map[string]interface{}{
		"type":    typ,
		"content": content,
		"at":      time.Now().UnixMilli(),
	})
	fmt.Fprintf(w, "data: %s\n\n", string(jsonLine))
}