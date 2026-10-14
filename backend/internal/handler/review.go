package handler

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/middleware"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/platform"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

type ReviewHandler struct {
	projectSvc  *service.ProjectService
	platformSvc *service.PlatformTokenService
	roleSvc     *service.RoleService
	llm         *llm.Gateway
	jobs        *store.JobStore
	jobLogSvc   *service.JobLogService
	claudeCfg   *service.ClaudeConfigService
	usageSvc    usageRecorder

	// Branch-Review dependencies, injected via setters rather than the
	// constructor so the existing NewReviewHandler call shape (and its
	// tests) keep working. Each one degrades gracefully when nil:
	// reviewSvc nil → session endpoints 503 but the legacy PR flow still
	// runs; attachSvc nil → no 📎 block in the system prompt; gitCredSvc
	// nil → `git fetch` runs with ambient credentials only.
	reviewSvc  *service.CodeReviewService
	attachSvc  *service.AttachmentService
	gitCredSvc *service.GitCredentialService
	// agentSvrSvc resolves the Agent Server a review was pinned to (the
	// "执行环境" picker). nil → the exec-env choice degrades to 本地 with an
	// explicit error line rather than silently running somewhere else.
	agentSvrSvc *service.AgentServerService
	// followupSvc persists the 追问 thread hanging off a finished review.
	// nil → the follow-up endpoints answer 503 and everything else in Review
	// behaves exactly as before.
	followupSvc *service.CodeReviewFollowupService
}

// SetCodeReviewFollowupSvc wires the 追问 thread store (see
// handler/review_followup.go).
func (h *ReviewHandler) SetCodeReviewFollowupSvc(svc *service.CodeReviewFollowupService) {
	h.followupSvc = svc
}

// SetCodeReviewSvc wires the persisted-session store. Without it the
// /api/projects/{id}/reviews family returns 503 and StartReview falls back
// to an ephemeral (unpersisted) run.
func (h *ReviewHandler) SetCodeReviewSvc(svc *service.CodeReviewService) { h.reviewSvc = svc }

// SetAttachmentSvc wires the REVIEW-scoped attachment store
// (service.NewCodeReviewAttachmentService) — not the requirement one. Its
// manifest is appended to the reviewer's system prompt so Claude can Read
// the uploaded context files.
func (h *ReviewHandler) SetAttachmentSvc(svc *service.AttachmentService) { h.attachSvc = svc }

// SetGitCredentialSvc wires the askpass env builder so `git fetch origin`
// works against private HTTPS remotes during ref resolution.
func (h *ReviewHandler) SetGitCredentialSvc(svc *service.GitCredentialService) { h.gitCredSvc = svc }

// SetAgentServerSvc wires remote execution. Without it a review pinned to an
// Agent Server fails with a clear message instead of quietly falling back to
// the local machine (which would review a different checkout than the user
// asked for).
func (h *ReviewHandler) SetAgentServerSvc(svc *service.AgentServerService) { h.agentSvrSvc = svc }

func NewReviewHandler(projectSvc *service.ProjectService, platformSvc *service.PlatformTokenService, roleSvc *service.RoleService, llmGateway *llm.Gateway, jobs *store.JobStore, jobLogSvc *service.JobLogService, claudeCfg *service.ClaudeConfigService, usageSvc usageRecorder) *ReviewHandler {
	return &ReviewHandler{
		projectSvc:  projectSvc,
		platformSvc: platformSvc,
		roleSvc:     roleSvc,
		llm:         llmGateway,
		jobs:        jobs,
		jobLogSvc:   jobLogSvc,
		claudeCfg:   claudeCfg,
		usageSvc:    usageSvc,
	}
}

// roleConfig loads a role's system prompt + effective model + Claude config
// binding by key. The returned model is the EFFECTIVE model (role override,
// else the role's bound config's default, else the global active config's
// default, else the "默认模型" literal) — pass it through cliModelArg
// before handing it to StreamOpts.Model. The returned configID is the
// binding the gateway uses to source auth + base URL; empty means fall back
// to the global active config.
//
// On error it returns empty system prompt + the resolved default model +
// the active config id (or empty when no config exists), so a missing/
// broken role config never blocks the review.
func (h *ReviewHandler) roleConfig(key string) (systemPrompt, model, configID string) {
	role, err := h.roleSvc.GetByKey(key)
	if err != nil {
		return "", h.effectiveModelFromConfig("", nil), h.activeConfigID()
	}
	cfg, _ := h.claudeCfg.ResolveRoleConfig(role)
	if cfg != nil {
		return role.SystemPrompt, effectiveModelFromValues(role.Model, cfg.DefaultModel), cfg.ID
	}
	return role.SystemPrompt, h.effectiveModelFromConfig(role.Model, role), h.activeConfigID()
}

// effectiveModel mirrors WizardHandler.effectiveModel: role override > active
// claude config default > "默认模型" literal. Defined here (rather than shared)
// because the two handlers resolve the active config independently and the
// resolution is trivial. Prefer effectiveModelFromConfig for the per-role
// binding path (this helper is the legacy single-active-config one).
func (h *ReviewHandler) effectiveModel(roleModel string) string {
	configDefault := ""
	if h.claudeCfg != nil {
		if _, dm, err := h.claudeCfg.ActiveModels(); err == nil {
			configDefault = dm
		}
	}
	return effectiveModelFromValues(roleModel, configDefault)
}

// effectiveModelFromConfig resolves the model using the role's bound Claude
// config (when present) instead of the global active config. A nil role
// means "no role row in hand" and falls through to the legacy global
// path so callers don't have to special case it.
func (h *ReviewHandler) effectiveModelFromConfig(roleModel string, role *model.Role) string {
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
func (h *ReviewHandler) activeConfigID() string {
	if h.claudeCfg == nil {
		return ""
	}
	c, err := h.claudeCfg.ActiveConfig()
	if err != nil || c == nil {
		return ""
	}
	return c.ID
}

// activeConfigMeta returns the currently-active claude config's id + currency
// for cost attribution on the review token row. Best-effort: empty when no
// config is active or on lookup error.
func (h *ReviewHandler) activeConfigMeta() (id, currency string) {
	if h.claudeCfg == nil {
		return "", ""
	}
	c, err := h.claudeCfg.ActiveConfig()
	if err != nil || c == nil {
		return "", ""
	}
	return c.ID, c.Currency
}

// PRListResponse wraps the PR list with a configuration flag.
type PRListResponse struct {
	Configured bool          `json:"configured"`
	PRs        []platform.PR `json:"prs"`
}

func gitRemoteURL(localPath string) (string, error) {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = localPath
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("无法读取 git remote origin，请确认项目已配置 remote 或在项目概览中填写 Remote URL")
	}
	return strings.TrimSpace(string(out)), nil
}

// effectiveRemoteURL returns remoteURL if set, otherwise reads from git remote origin.
//
// Public alias EffectiveRemoteURL exposed below so out-of-package callers
// (cmd/server/main.go's IssueMonitorProvider, internal/service/ when it
// grows, etc.) can resolve the same remote without re-implementing the
// `git remote get-url origin` fallback. The lowercase name is preserved
// at the original call sites in handler/review.go and handler/merge.go
// (those files import nothing that would rotate the symbol).
func effectiveRemoteURL(localPath, remoteURL string) (string, error) {
	if remoteURL != "" {
		return remoteURL, nil
	}
	return gitRemoteURL(localPath)
}

// EffectiveRemoteURL is the exported twin of effectiveRemoteURL. Same
// signature, same semantics; kept as a one-line wrapper to avoid
// touching every call site in handler/.
//
// Note for callers: this function shells out to `git remote get-url
// origin` when projects.remote_url is blank, which means the working
// directory's origin must already be set (e.g. a fresh clone). For a
// project registered before the user has run any commands, this returns
// an error and the issue-monitor poller logs it.
func EffectiveRemoteURL(localPath, remoteURL string) (string, error) {
	return effectiveRemoteURL(localPath, remoteURL)
}

// ListPRs fetches open PRs from the configured platform.
// GET /api/projects/{id}/prs
func (h *ReviewHandler) ListPRs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project, err := h.projectSvc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目不存在")
		return
	}

	if project.PlatformType == "" {
		writeJSON(w, http.StatusOK, PRListResponse{Configured: false, PRs: []platform.PR{}})
		return
	}

	// Token is optional — public repos work without one.
	var token, baseURL string
	if project.PlatformTokenID != "" {
		tok, err := h.platformSvc.Get(project.PlatformTokenID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "TOKEN_ERROR", "获取 Token 失败: "+err.Error())
			return
		}
		token = tok.Token
		baseURL = tok.BaseURL
	}

	client, err := platform.New(project.PlatformType, baseURL, token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "PLATFORM_ERROR", err.Error())
		return
	}

	repoURL, err := effectiveRemoteURL(project.LocalPath, project.RemoteURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "NO_REMOTE_URL", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	prs, err := client.ListOpenPRs(ctx, repoURL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "PLATFORM_API_ERROR", err.Error())
		return
	}
	if prs == nil {
		prs = []platform.PR{}
	}

	writeJSON(w, http.StatusOK, PRListResponse{Configured: true, PRs: prs})
}

// StartReviewReq is the body for starting a code review job.
type StartReviewReq struct {
	Branch            string `json:"branch"`
	BaseBranch        string `json:"base_branch"`
	PRNumber          int    `json:"pr_number"`
	PRTitle           string `json:"pr_title"`
	ExtraRequirements string `json:"extra_requirements"`
	// Model / ClaudeConfigID / AgentServerID mirror the branch-review
	// composer's toolbar (ModelSelect + ExecEnvSelect). All three are
	// optional: empty Model falls back to the reviewer role's model, empty
	// ClaudeConfigID to the role / active-config binding, empty
	// AgentServerID to 本地 execution.
	Model          string `json:"model"`
	ClaudeConfigID string `json:"claude_config_id"`
	AgentServerID  string `json:"agent_server_id"`
}

// StartReview is the legacy PR-review entry point, kept verbatim in shape
// (same route, same {"job_id": ...} response) so the existing PR tab and its
// comment-submit flow are untouched. Internally it is now a thin shell: it
// materialises a review_type='pr' session row and starts it, so PR reviews
// land in the same history + structured-findings pipeline as branch reviews.
// When the session store isn't wired the run still happens, just unpersisted.
// POST /api/projects/{id}/prs/review
func (h *ReviewHandler) StartReview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project, err := h.projectSvc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目不存在")
		return
	}

	var req StartReviewReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Branch == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "branch 字段不能为空")
		return
	}
	if req.BaseBranch == "" {
		req.BaseBranch = detectBaseBranch(project.LocalPath)
	}

	rv := &model.CodeReview{
		ProjectID:  project.ID,
		ReviewType: model.ReviewTypePR,
		Branch:     req.Branch,
		BaseBranch: req.BaseBranch,
		PRNumber:   req.PRNumber,
		Title:      req.PRTitle,
		// The PR entry point's single-line "额外审查要求" shares the
		// description column — see runReviewSession for the prompt-slot
		// mapping. Storing it means a re-run from history keeps it.
		Description:    req.ExtraRequirements,
		Model:          strings.TrimSpace(req.Model),
		ClaudeConfigID: strings.TrimSpace(req.ClaudeConfigID),
		AgentServerID:  strings.TrimSpace(req.AgentServerID),
		CreatedBy:      currentUserID(r),
	}
	if h.reviewSvc != nil {
		created, cerr := h.reviewSvc.Create(rv)
		if cerr != nil {
			log.Printf("[Review] persist PR review session failed: %v (running unpersisted)", cerr)
		} else {
			rv = created
		}
	}

	job := h.jobs.Create(id)
	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})

	go h.runReviewSession(job, rv, project)
}

// ── Review sessions (branch Review) ─────────────────────────────────────────

// CreateReviewReq is the body for creating a draft branch-review session.
type CreateReviewReq struct {
	Branch      string `json:"branch"`
	BaseBranch  string `json:"base_branch"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Toolbar bindings — see StartReviewReq for the fallback chain. Sent on
	// both create and update so a draft composed now reproduces the same
	// setup when it is re-run from history later.
	Model          string `json:"model"`
	ClaudeConfigID string `json:"claude_config_id"`
	AgentServerID  string `json:"agent_server_id"`
}

// CreateReview opens a draft session. The draft exists so attachments have a
// stable owner id to upload against before anything runs.
// POST /api/projects/{id}/reviews
func (h *ReviewHandler) CreateReview(w http.ResponseWriter, r *http.Request) {
	if h.reviewSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "Review 会话服务未启用")
		return
	}
	id := r.PathValue("id")
	project, err := h.projectSvc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目不存在")
		return
	}
	var req CreateReviewReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败")
		return
	}
	if strings.TrimSpace(req.Branch) == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "branch 字段不能为空")
		return
	}
	if strings.TrimSpace(req.BaseBranch) == "" {
		req.BaseBranch = detectBaseBranch(project.LocalPath)
	}
	rv, err := h.reviewSvc.Create(&model.CodeReview{
		ProjectID:      project.ID,
		ReviewType:     model.ReviewTypeBranch,
		Branch:         strings.TrimSpace(req.Branch),
		BaseBranch:     strings.TrimSpace(req.BaseBranch),
		Title:          strings.TrimSpace(req.Title),
		Description:    req.Description,
		Model:          strings.TrimSpace(req.Model),
		ClaudeConfigID: strings.TrimSpace(req.ClaudeConfigID),
		AgentServerID:  strings.TrimSpace(req.AgentServerID),
		CreatedBy:      currentUserID(r),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CREATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, rv)
}

// ListReviews returns the project's review history, newest first.
// GET /api/projects/{id}/reviews
func (h *ReviewHandler) ListReviews(w http.ResponseWriter, r *http.Request) {
	if h.reviewSvc == nil {
		writeJSON(w, http.StatusOK, []model.CodeReview{})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := h.reviewSvc.ListByProject(r.PathValue("id"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// GetReview returns one session (including the persisted report + findings).
// GET /api/reviews/{rid}
func (h *ReviewHandler) GetReview(w http.ResponseWriter, r *http.Request) {
	rv, ok := h.lookupReview(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, rv)
}

// UpdateReview patches description / branches / title on a draft or finished
// session. Running sessions are refused by the service layer.
// PUT /api/reviews/{rid}
func (h *ReviewHandler) UpdateReview(w http.ResponseWriter, r *http.Request) {
	cur, ok := h.lookupReview(w, r)
	if !ok {
		return
	}
	var req CreateReviewReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败")
		return
	}
	patch := &model.CodeReview{
		ReviewType:     cur.ReviewType,
		Branch:         strings.TrimSpace(req.Branch),
		BaseBranch:     strings.TrimSpace(req.BaseBranch),
		Title:          strings.TrimSpace(req.Title),
		Description:    req.Description,
		Model:          strings.TrimSpace(req.Model),
		ClaudeConfigID: strings.TrimSpace(req.ClaudeConfigID),
		AgentServerID:  strings.TrimSpace(req.AgentServerID),
	}
	rv, err := h.reviewSvc.Update(cur.ID, patch)
	if err != nil {
		writeError(w, http.StatusBadRequest, "UPDATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rv)
}

// DeleteReview removes the session plus its attachments (rows + files).
// DELETE /api/reviews/{rid}
func (h *ReviewHandler) DeleteReview(w http.ResponseWriter, r *http.Request) {
	rv, ok := h.lookupReview(w, r)
	if !ok {
		return
	}
	if rv.Status == model.ReviewStatusRunning {
		writeError(w, http.StatusConflict, "REVIEW_RUNNING", "Review 正在运行，无法删除")
		return
	}
	if err := h.reviewSvc.Delete(rv.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// StartReviewSession runs (or re-runs) a persisted session and returns the
// JobStore id. The SSE stream is the existing
// GET /api/projects/{id}/prs/jobs/{job_id}/stream — branch reviews
// deliberately do NOT open a second streaming channel.
// POST /api/reviews/{rid}/start
func (h *ReviewHandler) StartReviewSession(w http.ResponseWriter, r *http.Request) {
	rv, ok := h.lookupReview(w, r)
	if !ok {
		return
	}
	if rv.Status == model.ReviewStatusRunning {
		writeError(w, http.StatusConflict, "REVIEW_RUNNING", "该 Review 正在运行中")
		return
	}
	project, err := h.projectSvc.Get(rv.ProjectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目不存在")
		return
	}
	if strings.TrimSpace(rv.BaseBranch) == "" {
		rv.BaseBranch = detectBaseBranch(project.LocalPath)
	}

	job := h.jobs.Create(rv.ProjectID)
	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID, "review_id": rv.ID})

	go h.runReviewSession(job, rv, project)
}

// lookupReview resolves {rid} and writes the error response itself when the
// store is missing or the row doesn't exist. ok=false means "already
// responded, return now".
func (h *ReviewHandler) lookupReview(w http.ResponseWriter, r *http.Request) (*model.CodeReview, bool) {
	if h.reviewSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "Review 会话服务未启用")
		return nil, false
	}
	rv, err := h.reviewSvc.Get(r.PathValue("rid"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "REVIEW_NOT_FOUND", "Review 不存在")
			return nil, false
		}
		writeError(w, http.StatusInternalServerError, "GET_FAILED", err.Error())
		return nil, false
	}
	return rv, true
}

// currentUserID returns the authenticated principal's id, or "" when auth is
// disabled (the synthetic admin has no users row).
func currentUserID(r *http.Request) string {
	if u := middleware.CurrentUser(r); u != nil {
		return u.UserID
	}
	return ""
}

// StreamReviewJob streams a review job via SSE.
// GET /api/projects/{id}/prs/jobs/{job_id}/stream
func (h *ReviewHandler) StreamReviewJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	job, ok := h.jobs.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "JOB_NOT_FOUND", "任务不存在")
		return
	}

	streamJobSSE(w, r, job, func(status store.JobStatus, exitCode int) []byte {
		b, _ := json.Marshal(map[string]interface{}{
			"type":      "job_done",
			"status":    string(status),
			"exit_code": exitCode,
			"model":     job.Model,
		})
		return b
	})
}

// SubmitComment posts the review body as a comment on the PR.
// POST /api/projects/{id}/prs/{pr_number}/comment
func (h *ReviewHandler) SubmitComment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	prNumStr := r.PathValue("pr_number")
	prNumber, err := strconv.Atoi(prNumStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "pr_number 必须是整数")
		return
	}

	project, err := h.projectSvc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目不存在")
		return
	}

	if project.PlatformType == "" || project.PlatformTokenID == "" {
		writeError(w, http.StatusBadRequest, "NOT_CONFIGURED", "项目未配置平台 Token")
		return
	}

	var req struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Body == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "body 不能为空")
		return
	}

	tok, err := h.platformSvc.Get(project.PlatformTokenID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "TOKEN_ERROR", err.Error())
		return
	}

	client, err := platform.New(tok.Platform, tok.BaseURL, tok.Token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "PLATFORM_ERROR", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	repoURL, err := effectiveRemoteURL(project.LocalPath, project.RemoteURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "NO_REMOTE_URL", err.Error())
		return
	}

	if err := client.SubmitComment(ctx, repoURL, prNumber, req.Body); err != nil {
		writeError(w, http.StatusBadGateway, "PLATFORM_API_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "submitted"})
}

// runReviewSession drives one review run end to end: resolve the diff refs,
// build the prompt (with the user's description + attachment manifest),
// stream claude's output into the job, then persist the report + the parsed
// P0/P1/P3 findings back onto the session row.
func (h *ReviewHandler) runReviewSession(job *store.Job, rv *model.CodeReview, project *model.Project) {
	ctx := context.Background()

	// The reviewer persona lives in the configurable "reviewer" role; the
	// grading contract + checklist live in the task prompt (prompt.Build-
	// ReviewPrompt) so they apply even to installs whose persona was
	// customised before this feature existed.
	rolePrompt, modelName, claudeConfigID := h.roleConfig("reviewer")
	// Per-session overrides from the Review toolbar (ModelSelect +
	// ExecEnvSelect). An empty model keeps the role default; the sentinel
	// "默认模型" is the UI's way of saying the same thing, so it must never
	// be treated as a model id.
	if picked := strings.TrimSpace(rv.Model); picked != "" && picked != DefaultModelLabel {
		modelName = picked
	}
	claudeConfigID = h.resolveReviewConfigID(strings.TrimSpace(rv.ClaudeConfigID), modelName, claudeConfigID)
	job.SetModel(modelName)

	if h.reviewSvc != nil {
		if err := h.reviewSvc.MarkRunning(rv.ID, job.ID, modelName); err != nil {
			log.Printf("[Review] mark running %s: %v", rv.ID, err)
		}
	}

	// Execution environment: empty agent_server_id = 本地; anything else
	// dispatches the whole run (repo sync, attachment upload, claude) to the
	// Agent Server over SSH. We never silently fall back to local — that
	// would review a different checkout than the user picked.
	if strings.TrimSpace(rv.AgentServerID) != "" {
		h.runRemoteReviewSession(ctx, job, rv, project, rolePrompt, modelName, claudeConfigID)
		return
	}

	// Attachment manifest → system prompt. Claude reads the files on demand
	// via the absolute paths listed in the block (BuildManifest never inlines
	// the bytes, so a 2 GB upload can't blow the context).
	manifest := attachManifestFor(h.attachSvc, rv.ID, "")
	systemPrompt := appendManifest(rolePrompt, manifest)

	// Private HTTPS remotes need the askpass wiring for `git fetch origin`.
	var gitEnv []string
	cleanup := func() {}
	if h.gitCredSvc != nil {
		if env, c, err := h.gitCredSvc.BuildEnv(ctx, rv.ProjectID); err == nil {
			gitEnv, cleanup = env, c
		}
	}

	headRef, baseRef, err := resolveDiffRefs(ctx, project.LocalPath, rv.Branch, rv.BaseBranch, gitEnv)
	cleanup()
	if err != nil {
		msg := "无法解析对比分支: " + err.Error()
		job.Append(store.LogLine{Type: "error", Content: msg})
		h.finishReview(job, rv, modelName, model.ReviewStatusError, msg)
		return
	}

	reviewUsage := h.newReviewUsage(job.ID, rv, modelName)

	taskPrompt := buildReviewTaskPrompt(rv, headRef, baseRef, manifest != "")

	cmd := h.llm.StreamCmd(ctx, llm.StreamOpts{
		Prompt:         taskPrompt,
		WorkDir:        project.LocalPath,
		SystemPrompt:   systemPrompt,
		Model:          cliModelArg(modelName),
		ClaudeConfigID: claudeConfigID,
	})

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		msg := "启动 Claude 失败: " + err.Error()
		job.Append(store.LogLine{Type: "error", Content: msg})
		h.finishReview(job, rv, modelName, model.ReviewStatusError, msg)
		return
	}

	if err := cmd.Start(); err != nil {
		logClaudeExecDiag("review", cmd)
		msg := explainExecError(err)
		job.Append(store.LogLine{Type: "error", Content: msg})
		h.finishReview(job, rv, modelName, model.ReviewStatusError, msg)
		return
	}

	// finalReport accumulates the assistant text that makes up the report.
	// The `result` event's payload is the authoritative final answer when
	// present; the streamed text blocks are the fallback for runs that end
	// without one.
	var streamed strings.Builder
	finalReport := ""
	// sessionID is the claude session this run is writing into; captured once
	// from the stream so follow-up questions can resume it.
	sessionID := ""

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var evt map[string]interface{}
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}

		// Every stream-json event carries the run's session_id; the first one
		// (the system/init event) is enough. Persisting it is what lets a
		// later 追问 `--resume` this exact conversation instead of paying to
		// rebuild the diff context from scratch.
		if sessionID == "" {
			if sid, _ := evt["session_id"].(string); sid != "" {
				sessionID = sid
				h.rememberReviewSession(rv, sid)
			}
		}

		switch evt["type"] {
		case "assistant":
			if msg, ok := evt["message"].(map[string]interface{}); ok {
				if content, ok := msg["content"].([]interface{}); ok {
					for _, block := range content {
						bmap, ok := block.(map[string]interface{})
						if !ok {
							continue
						}
						switch bmap["type"] {
						case "text":
							if text, ok := bmap["text"].(string); ok && strings.TrimSpace(text) != "" {
								job.Append(store.LogLine{Type: "message", Content: text})
								streamed.WriteString(text)
								streamed.WriteString("\n\n")
							}
						case "tool_use":
							inp, _ := bmap["input"].(map[string]interface{})
							label := toolCallLabel(fmt.Sprint(bmap["name"]), inp)
							job.Append(store.LogLine{Type: "tool_call", Content: label})
						}
					}
				}
			}
		case "result":
			// Record token usage from the result event (best-effort).
			reviewUsage.recordFrom(evt)
			if sub, _ := evt["subtype"].(string); sub == "success" {
				if result, ok := evt["result"].(string); ok && strings.TrimSpace(result) != "" {
					job.Append(store.LogLine{Type: "message", Content: result})
					finalReport = result
				}
			} else if sub == "error" {
				if errMsg, ok := evt["error"].(string); ok {
					job.Append(store.LogLine{Type: "error", Content: errMsg})
				}
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		exitCode := 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
		if exitCode != 0 {
			log.Printf("[Review] Claude exited with code %d", exitCode)
		}
	}

	if strings.TrimSpace(finalReport) == "" {
		finalReport = streamed.String()
	}
	h.finishReview(job, rv, modelName, model.ReviewStatusDone, finalReport)
}

// newReviewUsage builds the token-usage recorder for a review run.
// requirement_id stays empty (reviews are project-level), so review rows
// never inflate requirement/project totals — they show up only in the
// project's review breakdown. review_id is carried in meta so the breakdown
// can link back to the session.
func (h *ReviewHandler) newReviewUsage(jobID string, rv *model.CodeReview, modelName string) *usageCtx {
	meta := fmt.Sprintf(`{"pr_number":%d,"pr_title":%q,"branch":%q,"review_id":%q,"review_type":%q}`,
		rv.PRNumber, rv.Title, rv.Branch, rv.ID, rv.ReviewType)
	configID, currency := h.activeConfigMeta()
	return &usageCtx{
		Rec:            h.usageSvc,
		ProjectID:      rv.ProjectID,
		JobID:          jobID,
		Step:           "review",
		Model:          modelName,
		ClaudeConfigID: configID,
		Currency:       currency,
		Meta:           meta,
	}
}

// finishReview is the single terminal path for a review run: parse the
// report's trailing JSON contract, persist the session row, snapshot the job
// log, and close the job.
//
// A contract miss is explicitly NOT an error — result_md keeps the whole
// report, findings_json stays empty, counts stay 0, and the frontend falls
// back to rendering the Markdown as-is (the report's own ## P0/P1/P3
// sections mean no information is lost).
func (h *ReviewHandler) finishReview(job *store.Job, rv *model.CodeReview, modelName, status, report string) {
	if h.reviewSvc != nil {
		if err := h.reviewSvc.MarkFinished(rv.ID, buildReviewOutcome(status, report)); err != nil {
			log.Printf("[Review] persist review %s: %v", rv.ID, err)
		}
	}

	// Persist the finished review job (incl. the effective reviewer model) so a
	// backend restart doesn't wipe the record and the UI can surface "本次 review
	// 使用模型". Review jobs aren't bound to a requirement, so requirement_id is
	// empty and the model lives on job_logs.model.
	lines, jobStatus, exitCode := job.Snapshot()
	if perr := h.jobLogSvc.Save(job.ID, "", string(jobStatus), exitCode, job.StartedAt, job.FinishedAt, lines, modelName); perr != nil {
		log.Printf("[Review] failed to persist job log %s: %v", job.ID, perr)
	}
	if status == model.ReviewStatusError {
		job.Finish(1, store.JobError)
		return
	}
	job.Finish(0, store.JobDone)
}

// rememberReviewSession persists the claude session id of the run in flight so
// 追问 can resume it. Best-effort by design: losing the id only costs the
// follow-up its resumed context (it then runs fresh with the report embedded
// in the prompt), so a write failure must never disturb the review.
func (h *ReviewHandler) rememberReviewSession(rv *model.CodeReview, sessionID string) {
	if h.reviewSvc == nil || rv == nil || sessionID == "" {
		return
	}
	rv.ClaudeSessionID = sessionID
	if err := h.reviewSvc.SetClaudeSessionID(rv.ID, sessionID); err != nil {
		log.Printf("[Review] persist session id for %s: %v", rv.ID, err)
	}
}

// resolveReviewConfigID mirrors WizardHandler.resolveConfigIDForRun for the
// review path: an explicit pick from the toolbar wins; otherwise the config
// that actually lists the effective model wins (so picking a model from a
// non-active config still gets that config's base URL + token); otherwise the
// reviewer role's binding; otherwise the globally active config.
func (h *ReviewHandler) resolveReviewConfigID(picked, modelName, roleCfgID string) string {
	if picked != "" {
		return picked
	}
	if modelName != "" && modelName != DefaultModelLabel && h.claudeCfg != nil {
		if cid, err := h.claudeCfg.ResolveConfigForModel(modelName); err == nil && cid != "" {
			return cid
		}
	}
	if roleCfgID != "" {
		return roleCfgID
	}
	return h.activeConfigID()
}

// appendManifest glues an attachment manifest block onto a system prompt.
// Empty manifest returns the prompt untouched, so callers can append
// unconditionally.
func appendManifest(systemPrompt, manifest string) string {
	if manifest == "" {
		return systemPrompt
	}
	return strings.TrimRight(systemPrompt, "\n") + "\n\n" + manifest
}

// buildReviewTaskPrompt renders the task prompt for a session. Shared by the
// local and Agent-server paths so the grading contract, the checklist and the
// description/extra-requirements split can never drift between them.
//
// rv.Description holds whatever free-text context the user supplied, but the
// two entry points mean different things by it: a branch review's is the
// 改动目标 the reviewer grades 目标达成 against, while the PR entry point's
// single-line field is 额外审查要求. One column, two prompt slots — so a re-run
// from history keeps the text either way, correctly labelled.
func buildReviewTaskPrompt(rv *model.CodeReview, headRef, baseRef string, hasAttachments bool) string {
	description, extra := rv.Description, ""
	if rv.ReviewType == model.ReviewTypePR {
		description, extra = "", rv.Description
	}
	return prompt.BuildReviewPrompt(prompt.ReviewPromptInput{
		ReviewType:        rv.ReviewType,
		Branch:            rv.Branch,
		BaseBranch:        rv.BaseBranch,
		HeadRef:           headRef,
		BaseRef:           baseRef,
		PRNumber:          rv.PRNumber,
		PRTitle:           rv.Title,
		Description:       description,
		ExtraRequirements: extra,
		HasAttachments:    hasAttachments,
	})
}

// buildReviewOutcome maps a finished report onto the row we persist.
//
// Happy path: the trailing JSON contract parses, so result_md is the report
// WITHOUT the contract block and the findings land structured + counted.
// Degraded path (contract missing or malformed): result_md keeps the whole
// report, findings_json stays empty and all counts stay 0, which is the
// signal the frontend uses to fall back to plain-Markdown rendering. The
// report's own ## P0/P1/P3 sections mean the user loses nothing.
func buildReviewOutcome(status, report string) service.ReviewOutcome {
	out := service.ReviewOutcome{Status: status, ResultMD: report}
	jsonPart, markdownPart := extractTrailingJSON(report)
	if jsonPart == "" {
		return out
	}
	findings, goalMet, ok := parseFindings(jsonPart)
	if !ok {
		return out
	}
	raw, err := json.Marshal(findings)
	if err != nil {
		return out
	}
	out.ResultMD = markdownPart
	out.FindingsJSON = string(raw)
	out.GoalMet = goalMet
	out.P0, out.P1, out.P3 = countBySeverity(findings)
	return out
}

// detectBaseBranch reads origin/HEAD or falls back to main/master.
func detectBaseBranch(projectPath string) string {
	cmd := exec.Command("git", "symbolic-ref", "refs/remotes/origin/HEAD")
	cmd.Dir = projectPath
	out, err := cmd.Output()
	if err == nil {
		ref := strings.TrimSpace(string(out))
		parts := strings.Split(ref, "/")
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}
	checkMain := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/main")
	checkMain.Dir = projectPath
	if checkMain.Run() == nil {
		return "main"
	}
	return "master"
}
