package handler

// review_subtask.go — the requirement-level Review, expressed as a SUB-TASK.
//
// The gap this file closes: a Code Review used to be reachable only from the
// PROJECT page, and only over "some branch vs some base branch". A
// requirement that finished development had no way back into the review
// pipeline — the user had to remember the branch name, walk to the project
// page, retype the 改动目标, and then copy every P0/P1 out of the report by
// hand.
//
// The chain this file wires together:
//
//	需求详情页 → 发起 Review（可选身份 / 模型）
//	          → code_reviews 行（报告的权威源）
//	          → task_kind='review' 的子任务（执行面复用 SubTaskRunner）
//	          → findings 转成修复子子任务（parent = 本 Review 子任务）
//	          → 回归 Review（parent = 上一轮，round +1）
//	          → 直到 P0/P1 清零
//
// Nothing here re-implements a review pipeline. The report面 is the existing
// code_reviews table + prompt.BuildReviewPrompt + review_parse.go's contract
// parser; the execution面 is the existing SubTaskRunner. This file only
// glues them and owns the requirement-scoped orchestration (which round,
// which parent, which fix sub-tasks).
//
// API:
//	POST /api/requirements/{id}/reviews                    → 发起第 1 轮
//	GET  /api/requirements/{id}/reviews                    → 轮次链
//	POST /api/requirements/{id}/reviews/{rid}/fixes        → findings → 修复子任务
//	POST /api/requirements/{id}/reviews/{rid}/recheck      → 回归 Review

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

// reviewerRoleKey is the roles.key the Review identity defaults to. It
// matches the project-level Review's default persona (handler/review.go
// passes "reviewer" to roleConfig), so a user who never picks an identity
// gets exactly the same reviewer.
const reviewerRoleKey = "reviewer"

// reviewDisallowedTools blocks the file-mutating tools during a Review.
//
// A Review is a READ-ONLY activity: the reviewer must look at code and
// report on it, never change it. The report comes back as the assistant's
// final text, so nothing legitimate is lost by denying Write / Edit.
//
// The tools the reviewer DOES need — Read, Grep, Glob, Bash (for `git diff`)
// — stay available.
var reviewDisallowedTools = []string{"Write", "Edit", "NotebookEdit"}

// RequirementReviewHandler owns the requirement-scoped Review endpoints.
//
// Every dependency is optional in the sense that a nil service disables its
// own endpoints with a 503 rather than crashing the server — the same
// convention the other handlers follow (see ReviewHandler's setters).
type RequirementReviewHandler struct {
	reqSvc      *service.RequirementService
	projectSvc  *service.ProjectService
	subTaskSvc  *service.SubTaskService
	reviewSvc   *service.CodeReviewService
	revisionSvc *service.CodeReviewRevisionService
	roleSvc     *service.RoleService
	claudeCfg   *service.ClaudeConfigService
	gitCredSvc  *service.GitCredentialService
	runner      *SubTaskRunner
	jobs        *store.JobStore
}

// NewRequirementReviewHandler wires the handler. main.go passes the same
// service instances the wizard / review handlers already use, so there is
// exactly one SubTaskRunner and one JobStore in the process.
func NewRequirementReviewHandler(
	reqSvc *service.RequirementService,
	projectSvc *service.ProjectService,
	subTaskSvc *service.SubTaskService,
	reviewSvc *service.CodeReviewService,
	revisionSvc *service.CodeReviewRevisionService,
	roleSvc *service.RoleService,
	claudeCfg *service.ClaudeConfigService,
	gitCredSvc *service.GitCredentialService,
	runner *SubTaskRunner,
	jobs *store.JobStore,
) *RequirementReviewHandler {
	return &RequirementReviewHandler{
		reqSvc:      reqSvc,
		projectSvc:  projectSvc,
		subTaskSvc:  subTaskSvc,
		reviewSvc:   reviewSvc,
		revisionSvc: revisionSvc,
		roleSvc:     roleSvc,
		claudeCfg:   claudeCfg,
		gitCredSvc:  gitCredSvc,
		runner:      runner,
		jobs:        jobs,
	}
}

// StartRequirementReviewReq is the body of POST /api/requirements/{id}/reviews.
//
// It is deliberately a separate type from the project-level StartReviewReq:
// the two endpoints have nothing in common but the word "review", and
// sharing one struct would let a project-level field (branch / pr_number)
// leak into the requirement-scoped one.
type StartRequirementReviewReq struct {
	// Description is the 改动目标 — the sole yardstick the reviewer grades
	// 「目标是否达成」 against.
	Description string `json:"description"`
	// RoleKey is the AI persona (roles.key) the review runs under. Empty
	// defaults to "reviewer".
	RoleKey string `json:"role_key"`
	// Model is the user's pick from the ModelSelect. The "默认模型" sentinel
	// means "no specific model" and is never treated as a model id.
	Model string `json:"model"`
	// ClaudeConfigID pins the gateway the review runs against.
	ClaudeConfigID string `json:"claude_config_id"`
	// AgentServerID is the execution environment. v1 only supports 本地 —
	// a remote pick is refused with an explicit message (see the handler).
	AgentServerID string `json:"agent_server_id"`
	// BaseBranch overrides the requirement's base branch.
	BaseBranch string `json:"base_branch"`
	// SessionMode is "with_context" (default: new session + requirement
	// context) or "fork" (inherit the developer's coding session).
	SessionMode string `json:"session_mode"`
}

// StartReview creates the review session + its sub-task and starts the run.
//
// POST /api/requirements/{id}/reviews
func (h *RequirementReviewHandler) StartReview(w http.ResponseWriter, r *http.Request) {
	if h.reviewSvc == nil || h.subTaskSvc == nil || h.runner == nil || h.reqSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "需求级 Review 服务未启用")
		return
	}
	reqID := r.PathValue("id")
	req, err := h.reqSvc.Get(reqID)
	if err != nil {
		writeError(w, http.StatusNotFound, "REQUIREMENT_NOT_FOUND", "需求不存在")
		return
	}
	var body StartRequirementReviewReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败")
		return
	}
	if strings.TrimSpace(body.AgentServerID) != "" {
		writeError(w, http.StatusBadRequest, "REMOTE_NOT_SUPPORTED",
			"需求级 Review v1 仅支持本地执行 —— 回归 Review 需要看到工作区未提交的修复，远端只能看到 origin 上的代码")
		return
	}

	roleKey := strings.TrimSpace(body.RoleKey)
	if roleKey == "" {
		roleKey = reviewerRoleKey
	}
	persona, modelName, configID, err := h.resolveReviewIdentity(roleKey, body.Model, body.ClaudeConfigID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}

	// The scope is resolved BEFORE the run starts, because the prompt has to
	// describe the diff the reviewer will actually see. A Review therefore
	// runs in a directory that already exists (never a freshly created
	// worktree) — see resolveReviewWorkDir.
	workDir, branch, base, scope, err := h.resolveScope(req, body.BaseBranch)
	if err != nil {
		writeError(w, http.StatusBadRequest, "SCOPE_FAILED", err.Error())
		return
	}

	rv, err := h.reviewSvc.Create(&model.CodeReview{
		ProjectID:      req.ProjectID,
		ReviewType:     model.ReviewTypeRequirement,
		RequirementID:  req.ID,
		RoleKey:        roleKey,
		Branch:         branch,
		BaseBranch:     base,
		Title:          reviewTitle(req, body.Description),
		Description:    body.Description,
		Model:          modelName,
		ClaudeConfigID: configID,
		CreatedBy:      currentUserID(r),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CREATE_FAILED", err.Error())
		return
	}

	// The Review sub-task forks the requirement's coding session only when
	// the user explicitly asks for it. By default it starts a FRESH session
	// with the requirement context: a reviewer that inherits the developer's
	// conversation tends to defend the developer's decisions, which is
	// exactly the bias a Review must not have.
	sessionMode := strings.TrimSpace(body.SessionMode)
	if sessionMode == "" {
		sessionMode = model.SubTaskSessionModeWithContext
	}
	sourceSID := ""
	if sessionMode == model.SubTaskSessionModeFork {
		sourceSID = subTaskSourceSID(req, "")
	}
	st, job, newSID, err := h.createReviewSubTask(req, rv, sessionMode, sourceSID, modelName, 1, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CREATE_FAILED", err.Error())
		return
	}

	h.launchReview(req, st, job, newSID, sourceSID, rv, persona, modelName, configID, scope, workDir, sessionMode)

	writeJSON(w, http.StatusOK, map[string]string{
		"job_id":      job.ID,
		"sub_task_id": st.ID,
		"review_id":   rv.ID,
	})
}

// ReviewRound is one entry of the requirement's Review round chain.
//
// It is a FLAT projection (not the full code_reviews row): the requirement
// page renders the chain from it, and the full report is fetched separately
// from GET /api/code-reviews/{rid}.
type ReviewRound struct {
	ReviewID    string     `json:"review_id"`
	SubTaskID   string     `json:"sub_task_id"`
	ReviewRound int        `json:"review_round"`
	Status      string     `json:"status"`
	Verdict     string     `json:"verdict"`
	P0Count     int        `json:"p0_count"`
	P1Count     int        `json:"p1_count"`
	P3Count     int        `json:"p3_count"`
	GoalMet     string     `json:"goal_met"`
	Title       string     `json:"title"`
	Model       string     `json:"model"`
	RoleKey     string     `json:"role_key"`
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

// ListReviews returns the requirement's Review rounds, newest first.
//
// GET /api/requirements/{id}/reviews
func (h *RequirementReviewHandler) ListReviews(w http.ResponseWriter, r *http.Request) {
	if h.reviewSvc == nil {
		writeJSON(w, http.StatusOK, []ReviewRound{})
		return
	}
	reqID := r.PathValue("id")
	rows, err := h.reviewSvc.ListByRequirement(reqID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	out := make([]ReviewRound, 0, len(rows))
	for i := range rows {
		rv := rows[i]
		round := ReviewRound{
			ReviewID:    rv.ID,
			ReviewRound: 1,
			Status:      rv.Status,
			Verdict:     rv.Verdict,
			P0Count:     rv.P0Count,
			P1Count:     rv.P1Count,
			P3Count:     rv.P3Count,
			GoalMet:     rv.GoalMet,
			Title:       rv.Title,
			Model:       rv.Model,
			RoleKey:     rv.RoleKey,
			CreatedAt:   rv.CreatedAt,
			FinishedAt:  rv.FinishedAt,
		}
		// The sub-task row carries the round number and the link back to the
		// card. A review row without a sub-task (e.g. created by an older
		// build) still shows up — the round number then stays at 1.
		if h.subTaskSvc != nil {
			if st, serr := h.subTaskSvc.ByReviewID(rv.ID); serr == nil && st != nil {
				round.SubTaskID = st.ID
				if st.ReviewRound > 0 {
					round.ReviewRound = st.ReviewRound
				}
			}
		}
		out = append(out, round)
	}
	writeJSON(w, http.StatusOK, out)
}

// CreateFixesReq is the body of POST /api/requirements/{id}/reviews/{rid}/fixes.
type CreateFixesReq struct {
	// FindingRef selects ONE finding ("P0-1"). Empty = the batch mode.
	FindingRef string `json:"finding_ref"`
	// Severities filters the batch mode. Empty = ["P0", "P1"] (P3 findings
	// are nits nobody files).
	Severities []string `json:"severities"`
}

// CreateFixes turns a Review's findings into fix sub-tasks.
//
// The fix sub-tasks are CHILDREN of the Review sub-task (parent_subtask_id =
// the review's sub-task id), so the requirement page renders them one level
// deeper under the round that produced them — the visual trace of
// "this finding came from that review".
//
// POST /api/requirements/{id}/reviews/{rid}/fixes
func (h *RequirementReviewHandler) CreateFixes(w http.ResponseWriter, r *http.Request) {
	if h.reviewSvc == nil || h.subTaskSvc == nil || h.reqSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "需求级 Review 服务未启用")
		return
	}
	reqID := r.PathValue("id")
	rid := r.PathValue("rid")
	rv, err := h.reviewSvc.Get(rid)
	if err != nil {
		writeError(w, http.StatusNotFound, "REVIEW_NOT_FOUND", "Review 不存在")
		return
	}
	if rv.RequirementID != reqID {
		writeError(w, http.StatusBadRequest, "REVIEW_MISMATCH", "该 Review 不属于当前需求")
		return
	}
	var body CreateFixesReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败")
		return
	}
	// The persisted column holds the findings ARRAY (not the contract
	// object) — see parseFindingsArray.
	findings, ok := parseFindingsArray(rv.FindingsJSON)
	if !ok || len(findings) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "FINDINGS_UNAVAILABLE",
			"本次 Review 未产出结构化问题清单，无法自动生成修复任务")
		return
	}
	items := selectFixFindings(findings, body)
	if len(items) == 0 {
		writeError(w, http.StatusBadRequest, "NO_FINDINGS_SELECTED", "没有选中的问题可转为修复任务")
		return
	}

	// The fix sub-tasks hang off the Review sub-task. When the review has no
	// sub-task row (created by an older build, or the row was deleted), they
	// attach to the requirement root instead of being lost.
	parentID := ""
	if h.subTaskSvc != nil {
		if st, serr := h.subTaskSvc.ByReviewID(rv.ID); serr == nil && st != nil {
			parentID = st.ID
		}
	}
	req, err := h.reqSvc.Get(reqID)
	if err != nil {
		writeError(w, http.StatusNotFound, "REQUIREMENT_NOT_FOUND", "需求不存在")
		return
	}

	created := []string{}
	for _, it := range items {
		fixPrompt := prompt.BuildFixPrompt(prompt.FixPromptInput{
			Severity:        it.finding.Severity,
			Title:           it.finding.Title,
			Detail:          it.finding.Detail,
			Suggestion:      it.finding.Suggestion,
			File:            it.finding.File,
			Line:            it.finding.Line,
			Ref:             it.ref,
			RequirementTitle: req.Title,
		})
		title := fixTaskTitle(it.finding, it.ref)
		// The fix must edit the SAME tree the next round will read, or the
		// "fix → re-review until it passes" loop never converges: the review
		// would keep re-grading the pre-fix code. A requirement-level Review
		// is local-only in v1, so the fix runs locally too.
		//
		// The one exception is a requirement whose code only exists on an
		// Agent Server (no local worktree at all): there is nothing local to
		// edit, so the fix follows the requirement to its server.
		fixAgentServerID := ""
		if strings.TrimSpace(req.WorktreePath) == "" && strings.TrimSpace(req.AgentServerID) != "" {
			fixAgentServerID = req.AgentServerID
		}
		// The fixer inherits the DEVELOPER's session, not the reviewer's: it
		// needs to know how the code was written in order to fix it, while
		// the reviewer's opinion would only add noise.
		st, cerr := h.subTaskSvc.Create(reqID, title, fixPrompt, rv.Model,
			subTaskSourceSID(req, ""), "", 0, fixAgentServerID, model.SubTaskSessionModeFork, parentID,
			model.SubTaskSourceManual, model.SubTaskReportModeAuto)
		if cerr != nil {
			writeError(w, http.StatusInternalServerError, "CREATE_FAILED",
				fmt.Sprintf("为 %s 建修复子任务失败: %v", it.ref, cerr))
			return
		}
		created = append(created, st.ID)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"sub_task_ids": created})
}

// RecheckReq is the body of POST /api/requirements/{id}/reviews/{rid}/recheck.
type RecheckReq struct {
	Model          string `json:"model"`
	ClaudeConfigID string `json:"claude_config_id"`
	RoleKey        string `json:"role_key"`
}

// Recheck starts the next round of the Review chain.
//
// The scope is RESOLVED AGAIN — the whole point of a 回归 Review is that the
// working tree changed since the last round (the fix sub-tasks edited it),
// so reusing the previous round's scope would re-grade the pre-fix code.
//
// POST /api/requirements/{id}/reviews/{rid}/recheck
func (h *RequirementReviewHandler) Recheck(w http.ResponseWriter, r *http.Request) {
	if h.reviewSvc == nil || h.subTaskSvc == nil || h.runner == nil || h.reqSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "需求级 Review 服务未启用")
		return
	}
	reqID := r.PathValue("id")
	rid := r.PathValue("rid")
	prev, err := h.reviewSvc.Get(rid)
	if err != nil {
		writeError(w, http.StatusNotFound, "REVIEW_NOT_FOUND", "Review 不存在")
		return
	}
	if prev.RequirementID != reqID {
		writeError(w, http.StatusBadRequest, "REVIEW_MISMATCH", "该 Review 不属于当前需求")
		return
	}
	req, err := h.reqSvc.Get(reqID)
	if err != nil {
		writeError(w, http.StatusNotFound, "REQUIREMENT_NOT_FOUND", "需求不存在")
		return
	}
	var body RecheckReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败")
		return
	}

	// The identity carries over from the previous round unless overridden.
	roleKey := strings.TrimSpace(body.RoleKey)
	if roleKey == "" {
		roleKey = strings.TrimSpace(prev.RoleKey)
	}
	if roleKey == "" {
		roleKey = reviewerRoleKey
	}
	pickedModel := strings.TrimSpace(body.Model)
	if pickedModel == "" {
		pickedModel = strings.TrimSpace(prev.Model)
	}
	pickedConfig := strings.TrimSpace(body.ClaudeConfigID)
	if pickedConfig == "" {
		pickedConfig = strings.TrimSpace(prev.ClaudeConfigID)
	}
	persona, modelName, configID, err := h.resolveReviewIdentity(roleKey, pickedModel, pickedConfig)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}

	workDir, branch, base, scope, err := h.resolveScope(req, prev.BaseBranch)
	if err != nil {
		writeError(w, http.StatusBadRequest, "SCOPE_FAILED", err.Error())
		return
	}

	rv, err := h.reviewSvc.Create(&model.CodeReview{
		ProjectID:      prev.ProjectID,
		ReviewType:     model.ReviewTypeRequirement,
		RequirementID:  req.ID,
		RoleKey:        roleKey,
		Branch:         branch,
		BaseBranch:     base,
		Title:          prev.Title,
		Description:    prev.Description,
		Model:          modelName,
		ClaudeConfigID: configID,
		CreatedBy:      currentUserID(r),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CREATE_FAILED", err.Error())
		return
	}

	// The new round hangs off the LATEST round's sub-task, so the chain is
	// walkable from the sub-task tree.
	//
	// "Latest", not "the round the user clicked": the re-check button is
	// rendered on every round card, and anchoring a new round to an OLD one
	// would mint a DUPLICATE round number (round 2 twice) and fork the chain
	// in two. The chain therefore always grows at its tip.
	latest, latestSub, err := h.latestReviewRound(reqID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	if latest != nil && (latest.Status == model.ReviewStatusRunning || latest.Status == model.ReviewStatusDraft) {
		writeError(w, http.StatusConflict, "REVIEW_RUNNING", "上一轮 Review 还在运行中，请等它结束后再发起回归 Review")
		return
	}
	parentID := ""
	round := 1
	if latestSub != nil {
		parentID = latestSub.ID
		round = latestSub.ReviewRound + 1
		if round < 1 {
			round = 1
		}
	}
	st, job, newSID, err := h.createReviewSubTask(req, rv, model.SubTaskSessionModeWithContext, "", modelName, round, parentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CREATE_FAILED", err.Error())
		return
	}
	h.launchReview(req, st, job, newSID, "", rv, persona, modelName, configID, scope, workDir, model.SubTaskSessionModeWithContext)

	writeJSON(w, http.StatusOK, map[string]string{
		"job_id":      job.ID,
		"sub_task_id": st.ID,
		"review_id":   rv.ID,
	})
}

// ── internals ───────────────────────────────────────────────────────────────

// resolveReviewIdentity resolves the persona + effective model + gateway for
// a Review round.
//
// The model precedence mirrors the project-level Review (handler/review.go):
//
//	explicit pick → the config that owns that model → the persona's own
//	config binding → the globally active config
//
// The "默认模型" literal is the UI's way of saying "no specific model" and
// must never be sent to the CLI as a model id.
func (h *RequirementReviewHandler) resolveReviewIdentity(roleKey, pickedModel, pickedConfig string) (persona, modelName, configID string, err error) {
	if h.roleSvc == nil {
		return "", DefaultModelLabel, "", fmt.Errorf("角色服务不可用")
	}
	role, rerr := h.roleSvc.GetByKey(roleKey)
	if rerr != nil {
		return "", "", "", fmt.Errorf("Review 身份 %q 不存在: %v", roleKey, rerr)
	}
	persona = role.SystemPrompt

	roleCfgID := ""
	cfgDefaultModel := ""
	if h.claudeCfg != nil {
		if cfg, cerr := h.claudeCfg.ResolveRoleConfig(role); cerr == nil && cfg != nil {
			roleCfgID = cfg.ID
			cfgDefaultModel = cfg.DefaultModel
		}
	}
	modelName = effectiveModelFromValues(role.Model, cfgDefaultModel)
	if m := strings.TrimSpace(pickedModel); m != "" && m != DefaultModelLabel {
		modelName = m
	}

	configID = strings.TrimSpace(pickedConfig)
	if configID == "" {
		if modelName != "" && modelName != DefaultModelLabel && h.claudeCfg != nil {
			if cid, cerr := h.claudeCfg.ResolveConfigForModel(modelName); cerr == nil && cid != "" {
				configID = cid
			}
		}
	}
	if configID == "" {
		configID = roleCfgID
	}
	if configID == "" && h.claudeCfg != nil {
		if cfg, cerr := h.claudeCfg.ActiveConfig(); cerr == nil && cfg != nil {
			configID = cfg.ID
		}
	}
	return persona, modelName, configID, nil
}

// resolveScope resolves the review's diff scope.
//
// It returns the working directory, the head / base branch names (for the
// review row), the head override (the no-branch mode's literal "HEAD") and
// the resolved scope block.
func (h *RequirementReviewHandler) resolveScope(req *model.Requirement, baseOverride string) (workDir, branch, base string, scope reviewScope, err error) {
	projPath := ""
	if h.projectSvc != nil {
		if p, perr := h.projectSvc.Get(req.ProjectID); perr == nil {
			projPath = p.LocalPath
		}
	}
	workDir = resolveReviewWorkDir(req, projPath)
	if workDir == "" {
		return "", "", "", scope, fmt.Errorf("无法定位需求的工作目录")
	}

	base = strings.TrimSpace(baseOverride)
	if base == "" {
		base = strings.TrimSpace(req.BaseBranch)
	}
	if base == "" && h.projectSvc != nil {
		if p, perr := h.projectSvc.Get(req.ProjectID); perr == nil {
			base = p.DefaultBranch
		}
	}
	if base == "" {
		base = "main"
	}
	branch = strings.TrimSpace(req.BranchName)

	// Askpass env so `git fetch origin` works against a private HTTPS remote.
	var gitEnv []string
	cleanup := func() {}
	if h.gitCredSvc != nil {
		if env, c, cerr := h.gitCredSvc.BuildEnv(context.Background(), req.ProjectID); cerr == nil {
			gitEnv, cleanup = env, c
		}
	}
	defer cleanup()

	scReq := reviewScopeRequest{RepoDir: workDir, Base: base, Env: gitEnv}
	if req.NoBranch {
		// The requirement developed directly on the base branch, so the
		// interesting committed range is `base...HEAD` inside the checkout.
		scReq.HeadOverride = "HEAD"
	} else {
		if branch == "" {
			return "", "", "", scope, fmt.Errorf("需求没有开发分支，无法解析 Review 范围")
		}
		scReq.Branch = branch
	}
	scope, err = resolveReviewScope(context.Background(), scReq)
	if err != nil {
		return "", "", "", scope, err
	}
	return workDir, branch, base, scope, nil
}

// createReviewSubTask inserts the task_kind='review' sub-task row and binds
// it to a JobStore job.
func (h *RequirementReviewHandler) createReviewSubTask(req *model.Requirement, rv *model.CodeReview, sessionMode, sourceSID, modelName string, round int, parentID string) (*model.SubTask, *store.Job, string, error) {
	title := rv.Title
	if strings.TrimSpace(title) == "" {
		title = fmt.Sprintf("Review 第 %d 轮", round)
	}
	// The stored prompt is the review's HUMAN input — the 改动目标. The full
	// task prompt (the review contract plus the resolved diff scope) is NOT
	// stored on the row: it is ~4 KB of boilerplate that would bloat the
	// artifact header (which quotes the prompt verbatim) without telling the
	// reader anything the scope block in the job log doesn't. The scope is
	// re-resolved on every round anyway, so a stale copy would be misleading
	// rather than useful.
	promptText := strings.TrimSpace(rv.Description)
	if promptText == "" {
		promptText = fmt.Sprintf("Review 第 %d 轮（未填写改动目标）", round)
	}
	st, err := h.subTaskSvc.CreateReviewTask(req.ID, title, promptText, modelName, sourceSID, "", sessionMode, parentID, rv.ID, round)
	if err != nil {
		return nil, nil, "", err
	}
	newSID := util.NewUUID()
	if perr := h.subTaskSvc.UpdateSession(st.ID, newSID, sourceSID); perr != nil {
		log.Printf("[req-review] failed to persist session for %s: %v", st.ID, perr)
	}
	job := h.jobs.Create(req.ID)
	if perr := h.subTaskSvc.UpdateJobID(st.ID, job.ID); perr != nil {
		log.Printf("[req-review] failed to persist job_id for %s: %v", st.ID, perr)
	}
	return st, job, newSID, nil
}

// launchReview builds the review prompt and dispatches the run.
func (h *RequirementReviewHandler) launchReview(
	req *model.Requirement,
	st *model.SubTask,
	job *store.Job,
	newSID, sourceSID string,
	rv *model.CodeReview,
	persona, modelName, configID string,
	scope reviewScope,
	workDir, sessionMode string,
) {
	taskPrompt := buildRequirementReviewPrompt(rv, scope)

	spec := subTaskRunSpec{
		body:                 "",
		modelOverride:        modelName,
		configIDOverride:     configID,
		fork:                 true,
		freshSession:         sessionMode != model.SubTaskSessionModeFork,
		// 'full', not 'auto': the auto heuristic would look at the (empty)
		// body and pick 'brief', which would stamp a misleading chip on the
		// row. A Review always produces a full report.
		reportMode:           model.SubTaskReportModeFull,
		promptOverride:       taskPrompt,
		systemPromptOverride: persona,
		roleKey:              rv.RoleKey,
		disallowedTools:      reviewDisallowedTools,
		workDirOverride:      workDir,
	}

	// The report is written back onto the code_reviews row when the run ends.
	// The transform strips the trailing JSON contract block so the sub-task
	// card renders the report body — the same口径 as the project-level
	// Review's result_md — while the contract itself is parsed into the
	// structured findings.
	var outcome service.ReviewOutcome
	spec.transformReport = func(finalResult string) string {
		outcome = buildReviewOutcome(model.ReviewStatusDone, finalResult)
		return outcome.ResultMD
	}
	spec.onFinished = func(st *model.SubTask, out claudeStreamOutcome, artifactBody string) {
		h.persistReviewOutcome(rv, st, out, outcome, artifactBody)
	}

	if scope.Warning != "" {
		job.Append(store.LogLine{Type: "warning", Content: scope.Warning})
	}
	if strings.TrimSpace(req.AgentServerID) != "" {
		// The requirement was developed on an Agent Server. A
		// requirement-level Review runs LOCALLY in v1, so it can only see
		// what reached the local worktree — say so instead of letting the
		// user read a report that silently skipped the remote commits.
		job.Append(store.LogLine{Type: "warning", Content: "⚠️ 本需求在 Agent Server 上开发：需求级 Review 只覆盖本地工作区中可见的改动，远端未同步回本地的提交不在本次范围内。"})
	}
	if scope.Mode == reviewScopeRefs {
		job.Append(store.LogLine{Type: "warning", Content: "⚠️ 本次 Review 只覆盖已提交的改动，工作区未提交的内容不在范围内。"})
	}

	// Bind the job before the goroutine starts so the SSE channel the
	// frontend subscribes to already knows the job id.
	if err := h.reviewSvc.MarkRunning(rv.ID, job.ID, modelName); err != nil {
		log.Printf("[req-review] mark running %s: %v", rv.ID, err)
	}
	go h.runner.RunReview(req, st, job, newSID, sourceSID, spec)
}

// buildRequirementReviewPrompt renders the review task prompt for one
// requirement-level round.
//
// It reuses prompt.BuildReviewPrompt wholesale — the three-dimension
// checklist, the P0/P1/P3 ladder, the Mermaid directive and the trailing
// JSON contract are byte-identical to the project-level Review's, so the
// two surfaces parse and render the same report.
func buildRequirementReviewPrompt(rv *model.CodeReview, scope reviewScope) string {
	return prompt.BuildReviewPrompt(prompt.ReviewPromptInput{
		ReviewType:     model.ReviewTypeRequirement,
		Branch:         rv.Branch,
		BaseBranch:     rv.BaseBranch,
		HeadRef:        scope.HeadRef,
		BaseRef:        scope.BaseRef,
		Description:    rv.Description,
		ScopeBlock:     scope.Block,
		HasAttachments: false,
	})
}

// persistReviewOutcome writes the finished round back onto the code_reviews
// row (the authoritative report) and freezes it as version v1.
//
// The failure path is as important as the success path: a Review whose run
// died must not stay 'running' forever, or the requirement page would show
// a spinner that never resolves.
func (h *RequirementReviewHandler) persistReviewOutcome(rv *model.CodeReview, st *model.SubTask, out claudeStreamOutcome, outcome service.ReviewOutcome, artifactBody string) {
	if out.errMsg != "" || out.staleSession || out.finalResult == "" {
		msg := "❌ Review 未产出报告"
		switch {
		case out.staleSession:
			msg = "❌ Review 的源会话已失效（session 文件不存在），请重新发起 Review。"
		case out.errMsg != "":
			msg = "❌ " + out.errMsg
		default:
			// The user stopped the round: say so, instead of claiming the
			// model failed to answer.
			if cur, gerr := h.subTaskSvc.Get(st.ID); gerr == nil && cur != nil && cur.Status == model.SubTaskStatusStopped {
				msg = "⏹ Review 已被用户中止，未产出报告。"
			} else {
				msg = "❌ Claude 未返回结果，请重试"
			}
		}
		if err := h.reviewSvc.MarkReviewFailed(rv.ID, msg); err != nil {
			log.Printf("[req-review] mark failed %s: %v", rv.ID, err)
		}
		return
	}
	if outcome.Status == "" {
		outcome.Status = model.ReviewStatusDone
	}
	if err := h.reviewSvc.MarkFinished(rv.ID, outcome); err != nil {
		log.Printf("[req-review] persist review %s: %v", rv.ID, err)
	}
	// Freeze the report as the round's first version so 「重新生成」 has
	// something to fall back to and the version switcher has something to
	// switch between — the same guarantee the project-level Review gives.
	if h.revisionSvc != nil {
		fresh, gerr := h.reviewSvc.Get(rv.ID)
		if gerr == nil {
			if _, serr := h.revisionSvc.Snapshot(fresh, model.RevisionTriggerInitial, 0); serr != nil {
				log.Printf("[req-review] snapshot review %s: %v", rv.ID, serr)
			}
		}
	}
	// Persist the session the run actually used so 追问 can --resume it.
	if out.sessionID != "" {
		if err := h.reviewSvc.SetClaudeSessionID(rv.ID, out.sessionID); err != nil {
			log.Printf("[req-review] persist session id for %s: %v", rv.ID, err)
		}
	}
}

// latestReviewRound returns the newest Review round of a requirement plus its
// sub-task row.
//
// The sub-task is nil when the round has no sub-task row (a row created by an
// older build, or one whose sub-task was deleted) — the caller then anchors
// the new round at the requirement root.
func (h *RequirementReviewHandler) latestReviewRound(reqID string) (*model.CodeReview, *model.SubTask, error) {
	rows, err := h.reviewSvc.ListByRequirement(reqID)
	if err != nil {
		return nil, nil, err
	}
	if len(rows) == 0 {
		return nil, nil, nil
	}
	// The tip of the chain is the round with the HIGHEST round number — not
	// merely the newest row. Two rounds created inside the same millisecond
	// would tie on created_at, and the id tie-breaker is a random hex, so
	// ordering by time alone is not reliable enough to decide which round
	// the chain grows from.
	bestIdx := 0
	bestRound := -1
	var bestSub *model.SubTask
	for i := range rows {
		round := 0
		if h.subTaskSvc != nil {
			if st, serr := h.subTaskSvc.ByReviewID(rows[i].ID); serr == nil && st != nil {
				round = st.ReviewRound
				if round > bestRound {
					bestSub = st
				}
			}
		}
		if round > bestRound {
			bestRound = round
			bestIdx = i
		}
	}
	return &rows[bestIdx], bestSub, nil
}

// selectFixFindings resolves the request into the list of findings to turn
// into fix sub-tasks.
//
// The batch order follows the report's own grouping (P0 first, then P1,
// then P3) so the created tasks appear in the order the user reads them.
func selectFixFindings(findings []model.ReviewFinding, body CreateFixesReq) []findingToIssue {
	items := []findingToIssue{}
	if ref := strings.TrimSpace(body.FindingRef); ref != "" {
		idx, err := resolveFindingRef(ref, findings)
		if err != nil {
			return nil
		}
		return []findingToIssue{{finding: findings[idx], ref: ref}}
	}
	sevs := body.Severities
	if len(sevs) == 0 {
		sevs = []string{prompt.SevP0, prompt.SevP1}
	}
	for _, sev := range []string{prompt.SevP0, prompt.SevP1, prompt.SevP3} {
		if !containsString(sevs, sev) {
			continue
		}
		bucket := bucketBySeverity(findings)[sev]
		for i, f := range bucket {
			items = append(items, findingToIssue{finding: f, ref: fmt.Sprintf("%s-%d", sev, i+1)})
		}
	}
	return items
}

// fixTaskTitle renders the card header of a fix sub-task.
func fixTaskTitle(f model.ReviewFinding, ref string) string {
	title := strings.TrimSpace(f.Title)
	if title == "" {
		title = strings.TrimSpace(f.File)
	}
	if title == "" {
		title = "修复 Review 问题"
	}
	prefix := fmt.Sprintf("[%s] ", ref)
	return strings.TrimSpace(prefix + title)
}

// reviewTitle renders the title of a review session from the requirement +
// the user's 改动目标.
func reviewTitle(req *model.Requirement, description string) string {
	title := fmt.Sprintf("Review · %s", req.Title)
	if d := strings.TrimSpace(description); d != "" {
		// One line, capped — the title is a card header, not the description.
		oneLine := strings.ReplaceAll(strings.ReplaceAll(d, "\n", " "), "\r", " ")
		runes := []rune(oneLine)
		if len(runes) > 40 {
			oneLine = string(runes[:40]) + "…"
		}
		title = fmt.Sprintf("%s · %s", title, oneLine)
	}
	return title
}
