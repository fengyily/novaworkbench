package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// The acceptance-check HTTP layer.
//
// Once a master requirement has handed its development down to the
// sub-requirements it was split into, it writes no code of its own — but
// its acceptance criteria do not disappear with the code. The existing
// guard only looks at the children's STATUS (all done → the master can be
// closed), never at their OUTPUT: four sub-requirements can all be marked
// done and the master can still be closed while they collectively failed
// to implement what it asked for.
//
// This handler closes that gap: it runs ONE read-only AI check that stands
// in the reviewer's seat and answers four questions —
//
//	1. did the children's development reach the master's goal?
//	2. is it done, and how far did it get (per acceptance point)?
//	3. what was NOT implemented?
//	4. does it need splitting further (which sub-requirements to add)?
//
// How it divides the work with the requirement-level Review: a Review
// grades CODE (findings / P0P1P3 / fix sub-tasks), an acceptance check
// grades the CONTRACT (did it land, what is missing, split again?). Both
// results live in the same code_reviews table, separated by review_type:
// 'requirement' feeds the Review round chain, 'acceptance' feeds the
// acceptance panel.
//
// Execution environment: **local only**. A remote Agent Server cannot see
// the children's local worktrees, so the check would read no implementation
// at all — the same reason the cross-requirement phase verification is
// local-only.
//
// WHEN it may run: at ANY moment. The user does not wait for every child to
// finish before asking where the work stands — a check on a half-built split
// is a legitimate thing to want, and they ask again once the children are
// done. So the result is a SNAPSHOT of the current progress: the verdict is
// capped at 「部分达成」 while any child is unfinished, the children still in
// flight are listed with the result, and work that is merely unfinished is
// never reported as "not implemented" (see requirement_acceptance_progress.go).

// acceptanceStallTimeout — the silence watchdog of one acceptance run.
//
// Wider than the cross-requirement phase verification (10 minutes): a check
// reads N implementation directories and compares them point by point
// against the master's acceptance criteria, which is slower than looking
// at one repository. It must still not wait forever.
const acceptanceStallTimeout = 15 * time.Minute

// acceptanceRoleKey is the roles.key the acceptance check runs under.
//
// It reuses the existing code-reviewer persona: an acceptance check IS a
// review — of the delivery against the contract, not of the code quality.
// A new "acceptor" persona would mean a new seed row, a settings-page entry
// and a pair of i18n files, for a persona whose system prompt would differ
// from the reviewer's by a paragraph.
const acceptanceRoleKey = "reviewer"

// AcceptanceHandler serves the acceptance-check endpoints.
//
// It owns its own per-requirement lock: one acceptance run per master
// requirement at a time. Two concurrent runs would burn the tokens twice and
// produce two competing verdicts for the same state of the code.
type AcceptanceHandler struct {
	reqSvc     *service.RequirementService
	projectSvc *service.ProjectService
	reviewSvc  *service.CodeReviewService
	roleSvc    *service.RoleService
	claudeCfg  *service.ClaudeConfigService
	jobs       *store.JobStore
	llm        *llm.Gateway
	// usageSvc records the run's token usage. Nil disables recording.
	usageSvc usageRecorder
	// locks is the per-requirement concurrency gate.
	locks *acceptanceLocks
}

// NewAcceptanceHandler wires the handler.
//
// main.go passes the same service instances the wizard / review handlers
// already use, so there is exactly one JobStore and one gateway in the
// process.
func NewAcceptanceHandler(
	reqSvc *service.RequirementService,
	projectSvc *service.ProjectService,
	reviewSvc *service.CodeReviewService,
	roleSvc *service.RoleService,
	claudeCfg *service.ClaudeConfigService,
	jobs *store.JobStore,
	gateway *llm.Gateway,
	usageSvc usageRecorder,
) *AcceptanceHandler {
	return &AcceptanceHandler{
		reqSvc:     reqSvc,
		projectSvc: projectSvc,
		reviewSvc:  reviewSvc,
		roleSvc:    roleSvc,
		claudeCfg:  claudeCfg,
		jobs:       jobs,
		llm:        gateway,
		usageSvc:   usageSvc,
		locks:      newAcceptanceLocks(),
	}
}

// acceptanceLocks is the per-requirement concurrency gate.
//
// It mirrors handler/requirement_split_registry.go's splitLocks: an
// in-memory map keyed by requirement id, holding the running job id.
//
// In-memory on purpose: the gate exists to stop the user from starting two
// runs from the same page (and the UI's own button state does the same).
// A restart clears it, which is correct — the restarted backend has no
// running acceptance job to protect.
type acceptanceLocks struct {
	mu    sync.Mutex
	byReq map[string]string
}

func newAcceptanceLocks() *acceptanceLocks {
	return &acceptanceLocks{byReq: map[string]string{}}
}

// acquire returns the existing job id when an acceptance run is already in
// flight for this requirement, and "" when the caller may proceed.
func (l *acceptanceLocks) acquire(reqID, jobID string) (existing string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byReq == nil {
		l.byReq = map[string]string{}
	}
	if cur, running := l.byReq[reqID]; running {
		return cur, false
	}
	l.byReq[reqID] = jobID
	return "", true
}

func (l *acceptanceLocks) release(reqID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byReq, reqID)
}

// peek reports the running job id for a requirement without taking the
// lock. It lets the handler bail out BEFORE creating a job, so a refused
// second run doesn't leave a stray job in the ring buffer.
func (l *acceptanceLocks) peek(reqID string) (jobID string, running bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	jobID, running = l.byReq[reqID]
	return jobID, running
}

// AcceptanceStartReq is the POST body of the start endpoint.
type AcceptanceStartReq struct {
	// Model — the model the user picked in the UI (empty = the reviewer
	// persona's default model).
	Model string `json:"model"`
	// ClaudeConfigID — the Claude config the user picked (empty = the
	// currently active config).
	ClaudeConfigID string `json:"claude_config_id"`
	// Effort — the reasoning tier the user picked (claude CLI --effort).
	// Empty = the "other tasks" stage default.
	Effort string `json:"effort"`
}

// StartCheck launches an acceptance check on a master requirement.
//
// POST /api/requirements/{id}/acceptance-check → {job_id, review_id}
//
// Preconditions (all rejected with an explicit error code BEFORE a job is
// created — the user must be told why, not shown a spinner that dies):
//
//	requirement not found      → 404 REQUIREMENT_NOT_FOUND
//	not split (dev_delegated)  → 409 ACCEPTANCE_NOT_DELEGATED
//	no children                → 409 ACCEPTANCE_NO_CHILDREN
//	no child entered dev       → 409 ACCEPTANCE_NO_IMPLEMENTATION
//	an acceptance run is live   → 409 ACCEPTANCE_IN_PROGRESS
//
// Note what is NOT a precondition: **every child must be finished**. A check
// on a half-built split is a legitimate thing to want — the user asks for it
// to see where the work stands, and asks again when the children are done.
// The result is then a snapshot: applyAcceptanceProgress caps the verdict at
// 「部分达成」 and keeps work in flight out of the "not implemented" list.
//
// The run itself is READ-ONLY: Write / Edit / NotebookEdit are denied at the
// CLI level, and the mutating git commands are denied too (see
// crossDisallowedTools), because a check must never change the code it is
// judging.
func (h *AcceptanceHandler) StartCheck(w http.ResponseWriter, r *http.Request) {
	if h.reqSvc == nil || h.reviewSvc == nil || h.jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "验收核对服务未启用")
		return
	}
	reqID := r.PathValue("id")
	req, err := h.reqSvc.Get(reqID)
	if err != nil {
		writeError(w, http.StatusNotFound, "REQUIREMENT_NOT_FOUND", "需求不存在")
		return
	}
	if !req.DevDelegated {
		writeError(w, http.StatusConflict, "ACCEPTANCE_NOT_DELEGATED",
			"该需求没有拆分过子需求，开发仍由它自己完成，无需验收核对")
		return
	}
	children, err := h.reqSvc.ListChildren(reqID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", "读取子需求失败: "+err.Error())
		return
	}
	if len(children) == 0 {
		writeError(w, http.StatusConflict, "ACCEPTANCE_NO_CHILDREN",
			"该需求没有拆分子需求，无法核对")
		return
	}
	if !anyChildImplemented(children) {
		writeError(w, http.StatusConflict, "ACCEPTANCE_NO_IMPLEMENTATION",
			"所有子需求都还没进入开发，没有可核对的实现")
		return
	}
	if existing, running := h.locks.peek(reqID); running {
		writeError(w, http.StatusConflict, "ACCEPTANCE_IN_PROGRESS",
			fmt.Sprintf("已有验收核对在运行（job %s），请等待结束", existing))
		return
	}

	var body AcceptanceStartReq
	if err := decodeJSON(r, &body); err != nil && !errors.Is(err, io.EOF) {
		_ = err
	}

	persona, modelName, configID, err := resolveRoleIdentity(h.roleSvc, h.claudeCfg,
		acceptanceRoleKey, body.Model, body.ClaudeConfigID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	effort := resolveEffort(h.llm, body.Effort, llm.StageOther)

	job := h.jobs.Create(reqID)
	job.SetType("acceptance")
	job.SetModel(modelName)

	if existing, ok := h.locks.acquire(reqID, job.ID); !ok {
		job.Finish(1, store.JobError)
		writeError(w, http.StatusConflict, "ACCEPTANCE_IN_PROGRESS",
			fmt.Sprintf("已有验收核对在运行（job %s），请等待结束", existing))
		return
	}

	// Every run creates a NEW row: the history of acceptance checks is the
	// record of "what did we think, and when" — a re-check must not erase
	// the previous verdict.
	rv, err := h.reviewSvc.Create(&model.CodeReview{
		ProjectID:      req.ProjectID,
		ReviewType:     model.ReviewTypeAcceptance,
		RequirementID:  req.ID,
		RoleKey:        acceptanceRoleKey,
		Title:          acceptanceTitle(req),
		Description:    "核对子需求的开发是否达成主需求的目的",
		Model:          modelName,
		Effort:         effort,
		ClaudeConfigID: configID,
		CreatedBy:      currentUserID(r),
	})
	if err != nil {
		h.locks.release(reqID)
		job.Finish(1, store.JobError)
		writeError(w, http.StatusInternalServerError, "CREATE_FAILED", err.Error())
		return
	}
	if err := h.reviewSvc.MarkRunning(rv.ID, job.ID, modelName); err != nil {
		log.Printf("[acceptance] mark review %s running: %v", rv.ID, err)
	}
	if err := h.reviewSvc.MarkEffort(rv.ID, effort); err != nil {
		log.Printf("[acceptance] mark effort for review %s: %v", rv.ID, err)
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"job_id":    job.ID,
		"review_id": rv.ID,
	})

	go h.runAcceptanceCheck(job, rv.ID, reqID, acceptanceSpec{
		persona:   persona,
		modelName: modelName,
		configID:  configID,
		effort:    effort,
	})
}

// acceptanceSpec is the resolved execution binding of one acceptance run.
type acceptanceSpec struct {
	// persona is the reviewer's system prompt.
	persona string
	// modelName is the effective model ("" = the CLI's default).
	modelName string
	// configID is the claude_configs row the gateway sources auth from.
	configID string
	// effort is the reasoning tier (claude CLI --effort).
	effort string
}

// anyChildImplemented reports whether at least one child has an
// implementation to read.
//
// "Something to read" means it has a worktree (its code lives somewhere, even
// if it is a partial snapshot) OR it is already done (it may have been
// developed and its worktree cleaned up). A child with neither has nothing to
// verify — checking it would only produce "unverifiable" for everything it
// covers.
//
// Note what this gate does NOT require: every child to be finished. A check
// on a half-built split is exactly what the user wants while the work is
// still going on — the result is then a snapshot of the current progress
// (see applyAcceptanceProgress).
func anyChildImplemented(children []*model.Requirement) bool {
	for _, c := range children {
		if c == nil {
			continue
		}
		switch prompt.AcceptanceStage(c.Status, c.WorktreePath) {
		case model.AcceptanceStageDone, model.AcceptanceStageInProgress:
			return true
		}
	}
	return false
}

// acceptanceTitle renders the review row's title.
func acceptanceTitle(req *model.Requirement) string {
	if req == nil {
		return "验收核对"
	}
	return "验收核对 · " + strings.TrimSpace(req.Title)
}

// runAcceptanceCheck is the goroutine body of one acceptance run.
//
// The flow mirrors the cross-requirement phase verification (see
// handler/cross_requirement_jobs.go runVerify): prepare the prompt, run the
// claude CLI read-only, parse the report, persist it, finish the job.
//
// The report is PERSISTED on the code_reviews row — not on the job — so it
// survives a backend restart (the JobStore ring buffer does not).
func (h *AcceptanceHandler) runAcceptanceCheck(job *store.Job, reviewID, reqID string, spec acceptanceSpec) {
	defer h.locks.release(reqID)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[acceptance] panic in run %s: %v", reviewID, r)
			h.finishAcceptanceError(job, reviewID, "验收核对过程异常终止")
		}
	}()

	req, err := h.reqSvc.Get(reqID)
	if err != nil {
		h.finishAcceptanceError(job, reviewID, "读取需求失败: "+err.Error())
		return
	}
	children, err := h.reqSvc.ListChildren(reqID)
	if err != nil {
		h.finishAcceptanceError(job, reviewID, "读取子需求失败: "+err.Error())
		return
	}

	members, err := h.acceptanceMembers(children)
	if err != nil {
		h.finishAcceptanceError(job, reviewID, "读取子需求依赖失败: "+err.Error())
		return
	}

	// The work dir is the master's own worktree (the stack base). It is where
	// the CLI runs and what it sees as "the project"; the children's
	// worktrees are granted as extra read paths.
	workDir := h.acceptanceWorkDir(req)
	if workDir == "" {
		h.finishAcceptanceError(job, reviewID, "找不到可用的工作目录（主需求没有工作区，项目也没有本地检出）")
		return
	}

	opts := llm.StreamOpts{
		Prompt:         h.acceptancePrompt(req, members, workDir),
		WorkDir:        workDir,
		SystemPrompt:   spec.persona,
		Model:          cliModelArg(spec.modelName),
		Effort:         spec.effort,
		ClaudeConfigID: spec.configID,
		// Every child's implementation directory is granted read access, so
		// ONE session can look at all of them (the CLI's --add-dir).
		AddDirs: mergeCrossDirs(h.acceptanceImplDirs(members)),
		// The check is READ-ONLY. Write / Edit / NotebookEdit are denied, and
		// so are the git commands that would change a repository — a check
		// that "fixed" the code it is judging would invalidate its own
		// verdict.
		DisallowedTools: crossDisallowedTools(),
	}

	job.Append(store.LogLine{Type: "phase", Content: "🔍 正在核对子需求的实现…"})

	// The guard protects every directory the session may read: the master's
	// own worktree (the stack base) AND each child's implementation
	// directory. Granting a directory and protecting it are the same
	// decision, so both come from the same list.
	protected := mergeCrossDirs(append([]string{workDir}, opts.AddDirs...))

	out := runClaudeStream(crossJobSink{jobSink: jobSink{job}, workDir: workDir, protected: protected},
		h.llm.StreamCmd(context.Background(), opts), "acceptance",
		h.acceptanceUsageCtx(reqID, job.ID, spec.modelName, spec.configID), nil, nil, 0,
		withStallTimeout(acceptanceStallTimeout))

	report := strings.TrimSpace(out.finalResult)
	if report == "" {
		report = strings.TrimSpace(out.planContent)
	}
	if report == "" {
		h.finishAcceptanceError(job, reviewID, crossEmptyOutputMessage(out, "未产出验收核对报告，请重试"))
		return
	}

	resultMD, summary, perr := parseAcceptanceReport(report)
	if perr != nil {
		// A broken contract block is NOT a failed run: the Markdown report
		// exists and is readable. Record the degradation and keep it.
		log.Printf("[acceptance] report %s: %v (keeping the markdown report)", reviewID, perr)
		job.Append(store.LogLine{Type: "message", Content: "⚠️ 结构化结论解析失败，已保留核对报告正文。"})
	}

	// Reconcile the report with the children's real progress: a check may
	// run while the children are still being developed, and the verdict must
	// then be a snapshot, not a sign-off (and must not blame work in flight).
	summary = applyAcceptanceProgress(summary, members)

	acceptanceJSON := ""
	if len(summary.Checks) > 0 || len(summary.Gaps) > 0 || len(summary.Proposals) > 0 ||
		summary.Verdict != "" || summary.Summary != "" || len(summary.Pending) > 0 {
		if raw, mErr := json.Marshal(summary); mErr == nil {
			acceptanceJSON = string(raw)
		} else {
			log.Printf("[acceptance] marshal summary for %s: %v", reviewID, mErr)
		}
	}

	if err := h.reviewSvc.MarkFinished(reviewID, service.ReviewOutcome{
		Status:        model.ReviewStatusDone,
		ResultMD:      resultMD,
		GoalMet:       summary.Verdict,
		AcceptanceJSON: acceptanceJSON,
	}); err != nil {
		h.finishAcceptanceError(job, reviewID, "保存核对结论失败: "+err.Error())
		return
	}

	job.Append(store.LogLine{Type: "message", Content: prompt.AcceptanceSummaryText(len(members), len(summary.Pending))})
	job.Finish(0, store.JobDone)
}

// acceptanceMembers turns the child rows into the prompt's member list.
//
// The dependency edges come along for the ride: a child whose predecessor is
// not finished yet cannot be held responsible for a shared acceptance point,
// so the prompt tells the model to call those points "无法验证".
func (h *AcceptanceHandler) acceptanceMembers(children []*model.Requirement) ([]prompt.AcceptanceMember, error) {
	out := make([]prompt.AcceptanceMember, 0, len(children))
	byID := map[string]*model.Requirement{}
	for _, c := range children {
		if c != nil {
			byID[c.ID] = c
		}
	}
	for _, c := range children {
		if c == nil {
			continue
		}
		m := prompt.AcceptanceMember{
			RequirementID:      c.ID,
			Title:              strings.TrimSpace(c.Title),
			Status:             c.Status,
			Description:        c.Description,
			AcceptanceCriteria: decodeStringArray(c.AcceptanceCriteria),
			WorktreePath:       strings.TrimSpace(c.WorktreePath),
			// The development stage is what keeps "not written yet" and
			// "not implemented" apart in the report — the prompt tells the
			// model which state each child is in.
			Stage: prompt.AcceptanceStage(c.Status, c.WorktreePath),
		}
		if h.reqSvc != nil {
			pre, _, dErr := h.reqSvc.ListDependencies(c.ID)
			if dErr != nil {
				return nil, dErr
			}
			for _, dep := range pre {
				m.DependsOn = append(m.DependsOn, strings.TrimSpace(dep.Title))
				if dep.Status != "done" {
					m.DependsOnUnmet = true
				}
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// acceptanceImplDirs returns the children's implementation directories.
func (h *AcceptanceHandler) acceptanceImplDirs(members []prompt.AcceptanceMember) []string {
	dirs := []string{}
	for _, m := range members {
		if d := strings.TrimSpace(m.WorktreePath); d != "" {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// acceptanceWorkDir picks the directory the CLI runs in.
//
// It prefers the master's own worktree (the stack base the children will be
// merged onto) and falls back to the project's local checkout. The fallback
// matters: a master that was never developed itself (its development was
// delegated!) has no worktree — and refusing to run then would make the
// feature useless exactly where it matters most.
func (h *AcceptanceHandler) acceptanceWorkDir(req *model.Requirement) string {
	if req != nil {
		if wd := strings.TrimSpace(req.WorktreePath); wd != "" {
			if st, err := os.Stat(wd); err == nil && st.IsDir() {
				return wd
			}
		}
	}
	if h.projectSvc != nil && req != nil {
		if proj, err := h.projectSvc.Get(req.ProjectID); err == nil && proj != nil {
			if p := strings.TrimSpace(proj.LocalPath); p != "" {
				if st, serr := os.Stat(p); serr == nil && st.IsDir() {
					return p
				}
			}
		}
	}
	return ""
}

// acceptancePrompt assembles the acceptance-check prompt.
//
// The project conventions go in as a prompt block (the cross-requirement
// flow does the same, for the same reason: the native channel can carry only
// ONE document, and the acceptance check judges against exactly one project).
func (h *AcceptanceHandler) acceptancePrompt(req *model.Requirement, members []prompt.AcceptanceMember, workDir string) string {
	master := prompt.AcceptanceContractInput{
		Title:              strings.TrimSpace(req.Title),
		Description:        req.Description,
		AcceptanceCriteria: decodeStringArray(req.AcceptanceCriteria),
		DesignDocs:         service.DesignDocsToMarkdown(req.DesignDocs),
		WorktreePath:       strings.TrimSpace(req.WorktreePath),
	}
	return prompt.BuildAcceptancePrompt(master, members, h.acceptanceConventions(req, workDir))
}

// acceptanceConventions renders the project's managed CLAUDE.md as a prompt
// section.
//
// It is best-effort: a project without a managed document (or a read error)
// simply contributes nothing — the conventions are helpful, not load-bearing
// for the verdict.
func (h *AcceptanceHandler) acceptanceConventions(req *model.Requirement, workDir string) string {
	if h.projectSvc == nil || req == nil {
		return ""
	}
	doc, err := h.projectSvc.ClaudeDoc(req.ProjectID)
	if err != nil || doc == nil {
		return ""
	}
	return prompt.BuildProjectClaudeDocBlock(doc.Content, service.DiskClaudeDocExists(workDir))
}

// acceptanceUsageCtx builds the token-usage identity of one run.
func (h *AcceptanceHandler) acceptanceUsageCtx(reqID, jobID, modelName, configID string) *usageCtx {
	if h.usageSvc == nil {
		return nil
	}
	return &usageCtx{
		Rec:            h.usageSvc,
		RequirementID:  reqID,
		JobID:          jobID,
		Step:           "acceptance_check",
		Model:          modelName,
		ClaudeConfigID: configID,
		Summary:        "验收核对",
	}
}

// finishAcceptanceError records a failed acceptance run.
//
// The reason lands on BOTH the job (so the live stream shows it) and the
// code_reviews row (so the panel shows it after a refresh).
func (h *AcceptanceHandler) finishAcceptanceError(job *store.Job, reviewID, msg string) {
	if h.reviewSvc != nil && reviewID != "" {
		if err := h.reviewSvc.MarkReviewFailed(reviewID, msg); err != nil {
			log.Printf("[acceptance] mark review %s failed: %v", reviewID, err)
		}
	}
	job.Append(store.LogLine{Type: "error", Content: msg})
	job.Finish(1, store.JobError)
}

// ListChecks returns the requirement's acceptance checks, newest first.
//
// GET /api/requirements/{id}/acceptance-check
func (h *AcceptanceHandler) ListChecks(w http.ResponseWriter, r *http.Request) {
	if h.reviewSvc == nil {
		writeJSON(w, http.StatusOK, []model.CodeReview{})
		return
	}
	rows, err := h.reviewSvc.ListAcceptanceChecks(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// GetCheck returns one acceptance check.
//
// GET /api/requirements/{id}/acceptance-check/{cid}
//
// The row must belong to THIS requirement — otherwise a guessed id could
// read another requirement's acceptance report.
func (h *AcceptanceHandler) GetCheck(w http.ResponseWriter, r *http.Request) {
	if h.reviewSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "验收核对服务未启用")
		return
	}
	cid := r.PathValue("cid")
	rv, err := h.reviewSvc.Get(cid)
	if err != nil {
		writeError(w, http.StatusNotFound, "ACCEPTANCE_NOT_FOUND", "验收核对记录不存在")
		return
	}
	if rv.ReviewType != model.ReviewTypeAcceptance || rv.RequirementID != r.PathValue("id") {
		writeError(w, http.StatusNotFound, "ACCEPTANCE_NOT_FOUND", "验收核对记录不存在")
		return
	}
	writeJSON(w, http.StatusOK, rv)
}

// StreamCheckJob streams the acceptance job's live output.
//
// GET /api/requirements/{id}/acceptance-check/jobs/{job_id}/stream
//
// The terminal frame carries the review id and the verdict, so the panel can
// refresh the persisted result without a second round trip.
func (h *AcceptanceHandler) StreamCheckJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	job, ok := h.jobs.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found")
		return
	}
	// The job id is only 8 hex chars, so it is guessable — the job must
	// belong to the requirement in the path.
	if job.RequirementID != r.PathValue("id") {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found")
		return
	}
	streamJobSSE(w, r, job, func(status store.JobStatus, exitCode int) []byte {
		result := map[string]any{}
		if h.reviewSvc != nil {
			if rv, err := h.reviewSvc.Get(h.acceptanceReviewID(job)); err == nil && rv != nil {
				result["review_id"] = rv.ID
				result["verdict"] = rv.GoalMet
			}
		}
		data, _ := json.Marshal(map[string]any{
			"type":          "job_done",
			"status":        string(status),
			"exit_code":     exitCode,
			"error_kind":    job.ErrorKind,
			"error_message": acceptanceJobError(job),
			"started_at":    job.StartedAt.UnixMilli(),
			"finished_at":   job.FinishedAt.UnixMilli(),
			"duration_ms":   job.FinishedAt.Sub(job.StartedAt).Milliseconds(),
			"result":        result,
		})
		return data
	})
}

// acceptanceJobError returns the failure message of a finished job.
//
// The JobStore has no "error message" slot — the failure reason travels as
// an "error" log line (that is what the live stream shows). So the terminal
// frame recovers it from the log: the LAST error line is the reason.
//
// A non-empty error_message is what the frontend uses to decide between
// "failed" and "finished" — without it a failed run would look like a run
// that produced nothing.
func acceptanceJobError(job *store.Job) string {
	if job == nil {
		return ""
	}
	log, _, _ := job.Snapshot()
	for i := len(log) - 1; i >= 0; i-- {
		if log[i].Type == "error" {
			return log[i].Content
		}
	}
	return ""
}

// acceptanceReviewID returns the code_reviews row a job produced.
//
// The JobStore has no payload slot, so the id is recovered from the review
// row's job_id column: the run stamps MarkRunning(reviewID, job.ID), which
// makes code_reviews.job_id the reverse pointer.
func (h *AcceptanceHandler) acceptanceReviewID(job *store.Job) string {
	if h.reviewSvc == nil || job == nil {
		return ""
	}
	rows, err := h.reviewSvc.ListAcceptanceChecks(job.RequirementID)
	if err != nil {
		return ""
	}
	for _, rv := range rows {
		if rv.JobID == job.ID {
			return rv.ID
		}
	}
	return ""
}

// decodeStringArray decodes a JSON array column into a string slice.
//
// The column is a JSON array of strings ('[]' when empty). A value that is
// not an array (a legacy row, a hand-edited database) degrades to a
// single-element slice rather than an error: the acceptance check must not
// die because one column is malformed.
func decodeStringArray(raw string) []string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(trimmed), &out); err == nil {
		res := make([]string, 0, len(out))
		for _, s := range out {
			if t := strings.TrimSpace(s); t != "" {
				res = append(res, t)
			}
		}
		return res
	}
	// Not an array: take it as one plain value.
	return []string{trimmed}
}
