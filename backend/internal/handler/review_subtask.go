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
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
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
	// settingSvc reads the fix sub-tasks' session mode
	// (subtask.fix_session_mode). Nil keeps the default (a curated
	// parent-context block instead of forking the developer's session).
	settingSvc *service.SettingService
}

// NewRequirementReviewHandler wires the handler. main.go passes the same
// service instances the wizard / review handlers already use, so there is
// exactly one SubTaskRunner and one JobStore in the process.
//
// settingSvc may be nil — the fix session mode then stays at its default.
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
	settingSvc *service.SettingService,
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
		settingSvc:  settingSvc,
	}
}

// gateway returns the claude gateway used to resolve reasoning-effort tiers.
//
// The requirement-level Review drives its rounds THROUGH the SubTaskRunner,
// which owns the gateway. Asking the runner for it keeps both sides on one
// instance — and therefore on one effort provider and one settings snapshot
// — without adding an eleventh constructor parameter to either type.
func (h *RequirementReviewHandler) gateway() *llm.Gateway {
	if h == nil || h.runner == nil {
		return nil
	}
	return h.runner.Gateway()
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
	// Effort is the reasoning-effort tier (claude CLI --effort). Empty =
	// the 「其他任务」 stage default. A Review grades other people's work,
	// so it belongs to 「其他任务」 rather than 「开发实现」.
	Effort string `json:"effort"`
	// AutoIterate turns the manual 「修复 → 回归 Review」 loop into an
	// automatic one: when it is on, the backend creates the P0/P1 fix
	// sub-tasks of every round on its own and starts the next round as soon
	// as they are done, until the code is clean or the budget runs out.
	//
	// Off (the default) the behaviour is EXACTLY what it was before this
	// flag existed: one round, nothing automatic.
	AutoIterate bool `json:"auto_iterate"`
	// AutoIterateRounds is how many EXTRA rounds to run after the first
	// one. 1 (the default) means "one more round after this one", so the
	// chain runs at most 2 rounds. Legal range: 1..10.
	AutoIterateRounds int `json:"auto_iterate_rounds"`
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
	// The auto-iteration configuration. The round budget is validated here
	// rather than silently clamped: a silently corrected pick would make the
	// user believe they got more rounds than they did.
	autoRounds := 1
	autoState := ""
	if body.AutoIterate {
		rounds, ok := normalizeAutoIterateRounds(body.AutoIterateRounds)
		if !ok {
			writeError(w, http.StatusBadRequest, "AUTO_ITERATE_ROUNDS_INVALID",
				fmt.Sprintf("自动迭代轮次必须在 1..%d 之间（收到 %d）", autoIterateMaxRounds, body.AutoIterateRounds))
			return
		}
		// A requirement whose code only exists on an Agent Server cannot run
		// an auto-iteration chain: the fix tasks would run on the server,
		// while the next round's Review only sees the LOCAL working tree —
		// so the chain would re-review the same unfixed code until the
		// budget ran out, burning tokens for nothing. Refuse it up front.
		if strings.TrimSpace(req.WorktreePath) == "" && strings.TrimSpace(req.AgentServerID) != "" {
			writeError(w, http.StatusBadRequest, "REMOTE_NOT_SUPPORTED",
				"该需求在 Agent Server 上开发，无法开启自动迭代 —— 修复任务在远端执行，下一轮 Review 只看得到本地工作区，链永远收敛不了。请改为本地执行或手动逐轮推进。")
			return
		}
		autoRounds = rounds
		autoState = autoIterateStateCurrent
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
		Effort:         resolveEffort(h.gateway(), body.Effort, llm.StageOther),
		ClaudeConfigID: configID,
		// The chain's configuration. autoState stays empty when the user did
		// not ask for auto-iteration, which is what makes the chain's state
		// the single source of truth for "this row is part of a chain".
		AutoIterateRounds: autoRounds,
		AutoIterateState:  autoState,
		CreatedBy:         currentUserID(r),
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
	// The auto-iteration chain's state of this round. Empty state = the
	// round is not part of a chain (the ordinary, manual Review).
	AutoIterateRounds int        `json:"auto_iterate_rounds"`
	AutoIterateState  string     `json:"auto_iterate_state"`
	AutoIterateNote   string     `json:"auto_iterate_note"`
	CreatedAt         time.Time  `json:"created_at"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
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
			AutoIterateRounds: rv.AutoIterateRounds,
			AutoIterateState:  rv.AutoIterateState,
			AutoIterateNote:   rv.AutoIterateNote,
			CreatedAt:         rv.CreatedAt,
			FinishedAt:        rv.FinishedAt,
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
	// Effort is the reasoning-effort tier (claude CLI --effort) the fix
	// children run at. Empty = the 「开发实现」 stage default — a fix writes
	// code, so it belongs to the coding stage even though the finding it
	// answers came from a Review.
	Effort string `json:"effort"`
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
	// The runner is what actually spawns the child. Without it the fix rows
	// could be created but never executed — the exact "orphaned pending row"
	// trap this handler used to fall into (the row was INSERTed and nothing
	// ever claimed it, so the card sat at 排队中 forever and the
	// "fix → re-review until P0/P1 clear" loop could never converge).
	if h.runner == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "子任务执行器未初始化")
		return
	}

	created, err := h.createFixTasks(req, rv, parentID, items, body.Effort, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CREATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"sub_task_ids": created})
}

// createFixTasks turns findings into fix sub-tasks under parentID.
//
// It is the shared body of the manual 「生成修复子任务」 button and the
// auto-iteration chain: the rows, the prompts, the session mode and the
// dispatch are IDENTICAL in both paths — only the terminal hook differs (the
// chain has to learn when its fix tasks are done so it can start the next
// round).
//
// The rows are DISPATCHED, not merely inserted. A pending row without a
// job_id belongs to no dispatcher: the orchestration tick only claims
// batch-scoped rows and the manual path only runs on an explicit user
// action, so such a row would sit at 「排队中」 forever.
func (h *RequirementReviewHandler) createFixTasks(
	req *model.Requirement,
	rv *model.CodeReview,
	parentID string,
	items []findingToIssue,
	effortPick string,
	onChildFinished func(st *model.SubTask, out claudeStreamOutcome, artifactBody string),
) ([]string, error) {
	if h.runner == nil {
		return nil, fmt.Errorf("子任务执行器未初始化")
	}
	// The fixer's context comes from the DEVELOPER's session, not the
	// reviewer's: it needs to know how the code was written in order to fix
	// it, while the reviewer's opinion would only add noise.
	sourceSID := subTaskSourceSID(req, "")
	// How that context reaches the fixer.
	//
	// "with_context" (default): a NEW claude session that receives a
	// curated context block — requirement + design docs + the parent's
	// recent turns + the sibling fix tasks' digests, capped at 32 KB.
	//
	// "fork" (the historical behaviour): --resume the developer's session,
	// inheriting its FULL conversation. The recorded incident showed why
	// that is the wrong default: the developer session carried 95 KB of
	// prompt and 197 tool calls, and the fixer restated the same conclusion
	// over and over because it kept re-reading (and re-auditing) that
	// history. The setting keeps the old behaviour available as an escape
	// hatch for the rare fixer that genuinely needs the full transcript.
	fixSessionMode := service.SubTaskFixSessionModeWithContext
	if h.settingSvc != nil {
		if mode, err := h.settingSvc.SubTaskFixSessionMode(); err == nil {
			fixSessionMode = mode
		} else {
			log.Printf("[review-fix] read fix session mode: %v (keeping %s)", err, fixSessionMode)
		}
	}
	useFork := fixSessionMode == service.SubTaskFixSessionModeFork
	// The fix runs on the model the Review ran on, so the two halves of the
	// loop speak the same dialect. The "默认模型" sentinel means "no specific
	// model" and must never reach the CLI — fall back to the runner's own
	// resolution (the developer role's model) in that case.
	modelOverride := strings.TrimSpace(rv.Model)
	if modelOverride == DefaultModelLabel {
		modelOverride = ""
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
			// The parent review's own report tells the fixer what was
			// ALREADY found, so it does not re-derive it. Without this the
			// fixer re-reads the whole code base to rediscover a finding
			// the report already states.
			PreviousFindings: summarizeFindings(rv.FindingsJSON),
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
		// NewPendingSubTask creates the row AND mints the JobStore job and
		// the pre-cast claude session id — the same three things every
		// dispatched row carries. A row without a job_id belongs to no
		// dispatcher: the orchestration tick only claims batch-scoped rows
		// and the manual path only runs on an explicit user action, so such a
		// row would sit at "排队中" forever. Creating the job here is what
		// makes the row dispatchable.
		//
		// The row's session_mode records which strategy produced it, so the
		// audit trail says "this fixer ran with a curated context" (or "it
		// forked the developer session") without having to infer it.
		rowSessionMode := model.SubTaskSessionModeWithContext
		if useFork {
			rowSessionMode = model.SubTaskSessionModeFork
		}
		// sourceSID is empty for the with_context mode: there is no session
		// to resume — the context arrives through the prompt instead. The
		// runner clears it again on the fresh-session path, but keeping the
		// row honest from the start avoids a window where the row claims a
		// source it never resumed.
		rowSourceSID := sourceSID
		if !useFork {
			rowSourceSID = ""
		}
		st, job, newSID, cerr := h.runner.NewPendingSubTask(req.ID, title, fixPrompt, rv.Model,
			rowSourceSID, fixAgentServerID, rowSessionMode, parentID, model.SubTaskReportModeAuto, "")
		if cerr != nil {
			return created, fmt.Errorf("为 %s 建修复子任务失败: %w", it.ref, cerr)
		}
		// A fix writes code → 「开发实现」 stage, even though the finding it
		// answers came from a Review.
		stampSubTaskEffort(h.gateway(), h.subTaskSvc, st, effortPick, llm.StageCoding)
		created = append(created, st.ID)
		// Dispatch it. The runner takes the per-project admission gate first
		// and only flips the row to 'running' once it wins a slot, so several
		// fixes created at once serialize behind the project's concurrency cap
		// instead of spawning a claude process each.
		//
		// fork=true → --resume <sourceSID> --fork-session: the fixer starts
		// from the developer's conversation.
		// freshSession=true → a new session with the curated parent-context
		// block (requirement + design docs + recent parent turns + sibling
		// digests), which is the default: it carries the same essential
		// information in a fraction of the tokens.
		//
		// fork stays TRUE in both cases (it selects the "## 子任务" prompt
		// header and the sub-task's execution shape); freshSession is what
		// decides the SESSION strategy. Passing fork=false would flip the
		// header to "## 继续执行", which is wrong for a fix task.
		go h.runner.RunFix(req, st, job, newSID, sourceSID, fixPrompt, modelOverride, "",
			false /*adjust*/, true /*fork*/, !useFork /*freshSession*/, false /*bare*/, "", model.SubTaskReportModeAuto,
			onChildFinished)
	}
	return created, nil
}

// summarizeFindings renders the parent review's finding list as a short
// Markdown block for the fix prompt.
//
// Why: the fixer is handed ONE finding, but the review found several. Without
// the list, the fixer has no idea whether the other findings are already
// handled — so it re-investigates them (or worse, "fixes" them again), which
// is wasted work and another source of the repeated conclusions.
//
// The block is capped so a review with 30 findings cannot blow up the
// prompt; only the first few are listed.
func summarizeFindings(findingsJSON string) string {
	findingsJSON = strings.TrimSpace(findingsJSON)
	if findingsJSON == "" {
		return ""
	}
	var findings []struct {
		Ref      string `json:"ref"`
		Severity string `json:"severity"`
		Title    string `json:"title"`
		File     string `json:"file"`
	}
	if err := json.Unmarshal([]byte(findingsJSON), &findings); err != nil {
		return ""
	}
	if len(findings) == 0 {
		return ""
	}
	var b strings.Builder
	for i, f := range findings {
		if i >= 10 {
			b.WriteString("- …（其余发现见 Review 报告）\n")
			break
		}
		ref := strings.TrimSpace(f.Ref)
		if ref == "" {
			ref = "?"
		}
		title := strings.TrimSpace(f.Title)
		if title == "" {
			title = "(未命名)"
		}
		loc := strings.TrimSpace(f.File)
		if loc != "" {
			b.WriteString("- " + ref + " " + title + "（" + loc + "）\n")
		} else {
			b.WriteString("- " + ref + " " + title + "\n")
		}
	}
	return b.String()
}

// RecheckReq is the body of POST /api/requirements/{id}/reviews/{rid}/recheck.
type RecheckReq struct {
	Model          string `json:"model"`
	ClaudeConfigID string `json:"claude_config_id"`
	RoleKey        string `json:"role_key"`
	// Effort is the reasoning-effort tier (claude CLI --effort) for the
	// next round. Empty = inherit the previous round's tier, else the
	// 「其他任务」 stage default.
	Effort string `json:"effort"`
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
	// The reasoning depth carries over from the previous round unless the
	// user picked another one: a 回归 Review grades the same kind of work
	// and should do it at the same depth, so the chain stays comparable.
	effortPick := strings.TrimSpace(body.Effort)
	if effortPick == "" {
		effortPick = strings.TrimSpace(prev.Effort)
	}

	// The chain's critical section. It is taken HERE (and not inside
	// startNextRoundLocked) so the "is the chain busy?" check and the
	// creation of the new round are atomic with respect to the automatic
	// advancement — otherwise a manual 回归 Review and the chain could each
	// mint a "round N+1" and fork the chain in two.
	var rv *model.CodeReview
	var st *model.SubTask
	var job *store.Job
	var startErr error
	withChainLock(reqID, func() {
		// A manual 回归 Review while the automatic chain is still waiting
		// for its fix sub-tasks would run two rounds in parallel. Refuse it
		// explicitly — the user can wait, or stop the fix sub-tasks (which
		// stops the chain).
		busy, berr := h.chainBusy(reqID)
		if berr != nil {
			startErr = &reviewStartError{http.StatusInternalServerError, "LIST_FAILED", berr.Error()}
			return
		}
		if busy {
			startErr = &reviewStartError{http.StatusConflict, "REVIEW_CHAIN_BUSY",
				"自动迭代链正在进行中（正在等待本轮修复子任务完成），请等它结束，或先停止相关修复子任务。"}
			return
		}
		rv, st, job, startErr = h.startNextRoundLocked(req, prev, roleKey, pickedModel, pickedConfig, effortPick)
	})
	if startErr != nil {
		h.writeStartError(w, startErr)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"job_id":      job.ID,
		"sub_task_id": st.ID,
		"review_id":   rv.ID,
	})
}

// chainBusy reports whether the requirement's auto-iteration chain is
// currently waiting for its fix sub-tasks to finish.
//
// It is the guard behind the manual 回归 Review button: while the chain is
// in that state, a manual round would run next to the automatic one.
func (h *RequirementReviewHandler) chainBusy(reqID string) (bool, error) {
	tip, tipSub, err := h.latestReviewRound(reqID)
	if err != nil {
		return false, err
	}
	if tip == nil || tipSub == nil {
		return false, nil
	}
	if tip.AutoIterateState != autoIterateStateCurrent {
		return false, nil
	}
	children, err := h.subTaskSvc.ListChildren(tipSub.ID)
	if err != nil {
		return false, err
	}
	for i := range children {
		switch children[i].Status {
		case model.SubTaskStatusPending, model.SubTaskStatusRunning:
			return true, nil
		}
	}
	return false, nil
}

// reviewStartError is a failure that happened while preparing a Review
// round. It carries the HTTP status + the stable error code the API answers
// with, so the manual path and the automatic path report the same thing.
type reviewStartError struct {
	status  int
	code    string
	message string
}

func (e *reviewStartError) Error() string { return e.message }

// writeStartError maps a start-round failure onto the API envelope.
func (h *RequirementReviewHandler) writeStartError(w http.ResponseWriter, err error) {
	var se *reviewStartError
	if errors.As(err, &se) {
		writeError(w, se.status, se.code, se.message)
		return
	}
	writeError(w, http.StatusInternalServerError, "CREATE_FAILED", err.Error())
}

// startNextRoundLocked creates AND launches the next Review round.
//
// It is the ONE implementation of "start another round", shared by the
// manual 「回归 Review」 button and the auto-iteration chain: both need the
// same thing — a fresh code_reviews row + a fresh review sub-task parented
// to the chain's tip, with the diff scope resolved AGAIN (the fixes changed
// the working tree, so reusing the previous round's scope would re-grade
// the pre-fix code).
//
// The chain's configuration is INHERITED from the chain's tip: the tip is
// the round that carries the chain's live configuration, and the round the
// user clicked may be an older one. A manual round on a chain that is not
// running (it never was, or it already stopped) is an ordinary manual round.
//
// The caller must hold the requirement's chain lock.
func (h *RequirementReviewHandler) startNextRoundLocked(
	req *model.Requirement,
	prev *model.CodeReview,
	roleKey, pickedModel, pickedConfig, effortPick string,
) (*model.CodeReview, *model.SubTask, *store.Job, error) {
	// The new round hangs off the LATEST round's sub-task, so the chain is
	// walkable from the sub-task tree.
	//
	// "Latest", not "the round the user clicked": the re-check button is
	// rendered on every round card, and anchoring a new round to an OLD one
	// would mint a DUPLICATE round number (round 2 twice) and fork the chain
	// in two. The chain therefore always grows at its tip.
	latest, latestSub, err := h.latestReviewRound(req.ID)
	if err != nil {
		return nil, nil, nil, &reviewStartError{http.StatusInternalServerError, "LIST_FAILED", err.Error()}
	}
	if latest != nil && (latest.Status == model.ReviewStatusRunning || latest.Status == model.ReviewStatusDraft) {
		return nil, nil, nil, &reviewStartError{http.StatusConflict, "REVIEW_RUNNING",
			"上一轮 Review 还在运行中，请等它结束后再发起回归 Review"}
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
	// The chain configuration is inherited from the chain's tip.
	chainRounds := 1
	chainState := ""
	if latest != nil {
		chainRounds = latest.AutoIterateRounds
		if latest.AutoIterateState == autoIterateStateCurrent {
			chainState = autoIterateStateCurrent
		}
	}
	if chainRounds < 1 {
		chainRounds = 1
	}

	persona, modelName, configID, err := h.resolveReviewIdentity(roleKey, pickedModel, pickedConfig)
	if err != nil {
		return nil, nil, nil, &reviewStartError{http.StatusBadRequest, "BAD_REQUEST", err.Error()}
	}

	workDir, branch, base, scope, err := h.resolveScope(req, prev.BaseBranch)
	if err != nil {
		return nil, nil, nil, &reviewStartError{http.StatusBadRequest, "SCOPE_FAILED", err.Error()}
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
		Effort:         resolveEffort(h.gateway(), effortPick, llm.StageOther),
		ClaudeConfigID: configID,
		// The chain's configuration is inherited, so a manual 回归 Review
		// does not break an automatic chain, and a chain's next round keeps
		// the same budget.
		AutoIterateRounds: chainRounds,
		AutoIterateState:  chainState,
		CreatedBy:         prev.CreatedBy,
	})
	if err != nil {
		return nil, nil, nil, &reviewStartError{http.StatusInternalServerError, "CREATE_FAILED", err.Error()}
	}

	st, job, newSID, err := h.createReviewSubTask(req, rv, model.SubTaskSessionModeWithContext, "", modelName, round, parentID)
	if err != nil {
		return nil, nil, nil, &reviewStartError{http.StatusInternalServerError, "CREATE_FAILED", err.Error()}
	}
	h.launchReview(req, st, job, newSID, "", rv, persona, modelName, configID, scope, workDir, model.SubTaskSessionModeWithContext)
	return rv, st, job, nil
}

// ── internals ───────────────────────────────────────────────────────────────

// resolveReviewIdentity resolves the persona + effective model + gateway for
// a Review round.
//
// It delegates to resolveRoleIdentity — the shared implementation, so the
// acceptance check resolves its persona exactly the way a Review does.
func (h *RequirementReviewHandler) resolveReviewIdentity(roleKey, pickedModel, pickedConfig string) (persona, modelName, configID string, err error) {
	return resolveRoleIdentity(h.roleSvc, h.claudeCfg, roleKey, pickedModel, pickedConfig)
}

// resolveRoleIdentity resolves the persona + effective model + gateway for
// any AI run that is bound to a roles.row.
//
// The model precedence mirrors the project-level Review (handler/review.go):
//
//	explicit pick → the config that owns that model → the persona's own
//	config binding → the globally active config
//
// The "默认模型" literal is the UI's way of saying "no specific model" and
// must never be sent to the CLI as a model id.
//
// It is a package-level function (not a method) so the Review handler and the
// acceptance handler share ONE implementation: two copies of this chain would
// drift, and a drift would show up as "the acceptance check ran on a
// different gateway than the Review".
func resolveRoleIdentity(
	roleSvc *service.RoleService,
	claudeCfg *service.ClaudeConfigService,
	roleKey, pickedModel, pickedConfig string,
) (persona, modelName, configID string, err error) {
	if roleSvc == nil {
		return "", DefaultModelLabel, "", fmt.Errorf("角色服务不可用")
	}
	role, rerr := roleSvc.GetByKey(roleKey)
	if rerr != nil {
		return "", "", "", fmt.Errorf("Review 身份 %q 不存在: %v", roleKey, rerr)
	}
	persona = role.SystemPrompt

	roleCfgID := ""
	cfgDefaultModel := ""
	if claudeCfg != nil {
		if cfg, cerr := claudeCfg.ResolveRoleConfig(role); cerr == nil && cfg != nil {
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
		if modelName != "" && modelName != DefaultModelLabel && claudeCfg != nil {
			if cid, cerr := claudeCfg.ResolveConfigForModel(modelName); cerr == nil && cid != "" {
				configID = cid
			}
		}
	}
	if configID == "" {
		configID = roleCfgID
	}
	if configID == "" && claudeCfg != nil {
		if cfg, cerr := claudeCfg.ActiveConfig(); cerr == nil && cfg != nil {
			configID = cfg.ID
		}
	}
	return persona, modelName, configID, nil
}

// preferLocalHeadFor decides whether a requirement-level Review resolves
// its head ref to the LOCAL branch instead of `origin/<branch>`.
//
// It answers one question: "is the local worktree the copy of the code the
// user wants graded?"
//
//   - Locally developed, own worktree → YES. The branch tip IS the worktree's
//     HEAD and may carry commits the user never pushed; diffing against
//     `origin/<branch>` would silently review the pre-push code, so the
//     「修复 → 回归 Review」 loop would never converge.
//   - Developed on an Agent Server → NO. The code was written on the server
//     and pushed there; the local worktree is only the branch isolation
//     shell, so the remote ref is the copy that holds the work.
//   - No local worktree (degraded mode) → NO. The pushed code is all there
//     is.
func preferLocalHeadFor(req *model.Requirement, workDir, projPath string) bool {
	if req == nil {
		return false
	}
	if strings.TrimSpace(req.AgentServerID) != "" {
		return false
	}
	if workDir == "" || workDir == projPath {
		return false
	}
	return workDir == req.WorktreePath
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
		// When the Review runs in the requirement's OWN worktree AND the
		// requirement was developed LOCALLY, the local branch is the truth —
		// it may carry commits that were never pushed, and reviewing
		// `origin/<branch>` instead would grade code the user already
		// replaced (the 「修复 → 回归 Review」 loop would then never
		// converge).
		//
		// A requirement developed on an Agent Server keeps origin-first: its
		// code lives on the server and was pushed there, so the remote ref is
		// the only copy that holds the work (the local worktree is just the
		// branch isolation shell). Same for the degraded mode (no local
		// worktree at all) — there, the pushed code is all there is.
		scReq.PreferLocalHead = preferLocalHeadFor(req, workDir, projPath)
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
		// The Review grades other people's work → 「其他任务」 stage. The
		// tier is resolved once, here, and carried on the row so the
		// sub-task card and any re-run show what actually ran.
		effort:             rv.Effort,
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
		// The auto-iteration chain advances from here: the round is over,
		// the report is on the row, so the chain can decide whether to
		// create this round's fix sub-tasks or to stop.
		//
		// It runs in its own goroutine because the advancement may resolve
		// a git scope (a `git fetch` against a slow remote), and that must
		// not hold the round's SSE stream open.
		go h.onReviewRoundFinished(rv, st)
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
