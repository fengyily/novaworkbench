package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// ── 基于追问对话重新生成报告 + 报告版本history ─────────────────────────────
//
// 追问 answers questions about a report but deliberately never rewrites it
// (see review_followup.go). That leaves the useful part of the conversation
// stranded: the user has corrected a false positive, supplied the log line
// that proves a goal WAS met, and asked for the part that was unclear — and
// the report still says what it said before any of that.
//
// Regeneration closes the loop. It replays the report plus the whole 追问
// thread back to the reviewer and asks for the report it would have written
// knowing all of it, under the same contract (same checklist, same P0/P1/P3
// ladder, same trailing JSON block) so the output lands in exactly the same
// columns.
//
// The one rule that shapes the whole file: **a regeneration must never cost
// the user the report they already have.**
//
//   - The current version is frozen into code_review_revisions BEFORE the run
//     starts, so it stays readable and restorable whatever happens next.
//   - MarkRegenerating does not clear result_md / findings_json / the counts /
//     claude_session_id (unlike MarkRunning), so a crash leaves the row intact.
//   - Every failure path calls MarkRegenerateFailed, which flips the status
//     back to `done` and leaves the previous report in place. The failure is
//     reported through the job stream, not by damaging the row.
//
// Streaming reuses the review SSE channel
// (GET /api/projects/{id}/prs/jobs/{job_id}/stream) — no second protocol.

// maxRegenerateReportChars caps how much of the stored report is pasted into
// a fresh (non-resumed) regeneration prompt. Larger than the follow-up cap:
// here the report is the thing being rewritten, not background for a
// question, so losing its tail would silently drop findings from v2.
const maxRegenerateReportChars = 48000

// maxRegenerateAnswerChars caps each replayed follow-up answer. Questions are
// kept whole — they carry the user's intent, which is the whole input signal.
const maxRegenerateAnswerChars = 6000

// SetCodeReviewRevisionSvc wires the report version history. nil → the
// revision endpoints answer 503 and regeneration is refused (running it
// without a snapshot would make it destructive, which is the one thing it
// must not be).
func (h *ReviewHandler) SetCodeReviewRevisionSvc(svc *service.CodeReviewRevisionService) {
	h.revisionSvc = svc
}

// ListRevisions returns a review's report versions, oldest first.
// GET /api/reviews/{rid}/revisions
func (h *ReviewHandler) ListRevisions(w http.ResponseWriter, r *http.Request) {
	rv, ok := h.lookupReview(w, r)
	if !ok {
		return
	}
	if h.revisionSvc == nil {
		writeJSON(w, http.StatusOK, []model.CodeReviewRevision{})
		return
	}
	rows, err := h.revisionSvc.ListByReview(rv.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// GetRevision returns one version in full (report + findings), so the UI can
// show v1 without destroying the current v2.
// GET /api/reviews/{rid}/revisions/{n}
func (h *ReviewHandler) GetRevision(w http.ResponseWriter, r *http.Request) {
	rv, ok := h.lookupReview(w, r)
	if !ok {
		return
	}
	rev, ok := h.lookupRevision(w, rv.ID, r.PathValue("n"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, rev)
}

// RestoreRevision makes an older version current again. Nothing is deleted:
// the version list records what was PRODUCED, not which one is selected, so
// switching back and forth is free.
// POST /api/reviews/{rid}/revisions/{n}/restore
func (h *ReviewHandler) RestoreRevision(w http.ResponseWriter, r *http.Request) {
	rv, ok := h.lookupReview(w, r)
	if !ok {
		return
	}
	if rv.Status == model.ReviewStatusRunning {
		writeError(w, http.StatusConflict, "REVIEW_RUNNING", "Review 正在运行中，请等待本次运行结束后再切换版本")
		return
	}
	rev, ok := h.lookupRevision(w, rv.ID, r.PathValue("n"))
	if !ok {
		return
	}
	if err := h.revisionSvc.RestoreAsCurrent(rv.ID, rev.RevNo); err != nil {
		writeError(w, http.StatusInternalServerError, "RESTORE_FAILED", err.Error())
		return
	}
	updated, err := h.reviewSvc.Get(rv.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "GET_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// lookupRevision resolves {n} against a review and writes the error response
// itself. ok=false means "already responded, return now".
func (h *ReviewHandler) lookupRevision(w http.ResponseWriter, reviewID, raw string) (*model.CodeReviewRevision, bool) {
	if h.revisionSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "Review 版本服务未启用")
		return nil, false
	}
	revNo, err := strconv.Atoi(raw)
	if err != nil || revNo <= 0 {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "版本号必须是正整数")
		return nil, false
	}
	rev, err := h.revisionSvc.Get(reviewID, revNo)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "REVISION_NOT_FOUND", "该版本不存在")
			return nil, false
		}
		writeError(w, http.StatusInternalServerError, "GET_FAILED", err.Error())
		return nil, false
	}
	return rev, true
}

// RegenerateReview starts a 「基于追问对话重新生成」 run and returns the
// JobStore id to stream from (the review's own SSE channel).
// POST /api/reviews/{rid}/regenerate
func (h *ReviewHandler) RegenerateReview(w http.ResponseWriter, r *http.Request) {
	rv, ok := h.lookupReview(w, r)
	if !ok {
		return
	}
	if h.revisionSvc == nil {
		// Without the snapshot store a regeneration would overwrite the
		// current report with no way back — exactly the failure mode this
		// feature exists to avoid. Refuse rather than degrade.
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "Review 版本服务未启用，无法重新生成")
		return
	}
	if rv.Status == model.ReviewStatusRunning {
		writeError(w, http.StatusConflict, "REVIEW_RUNNING", "Review 正在运行中，请等待本次运行结束后再重新生成")
		return
	}
	if strings.TrimSpace(rv.ResultMD) == "" {
		writeError(w, http.StatusBadRequest, "NO_REPORT", "该 Review 还没有生成报告，无法重新生成")
		return
	}
	project, err := h.projectSvc.Get(rv.ProjectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目不存在")
		return
	}

	// Make sure the version we are about to replace is frozen BEFORE anything
	// else can fail. Normally the run that produced it already did this
	// (finishReview → snapshotFinishedReview); EnsureSnapshot covers the
	// review that finished before this table existed. A failure here aborts
	// the whole request: running on would make the regeneration destructive.
	if err := h.revisionSvc.EnsureSnapshot(rv, h.answeredFollowupCount(rv.ID)); err != nil {
		writeError(w, http.StatusInternalServerError, "SNAPSHOT_FAILED", "保存当前报告版本失败，已取消重新生成: "+err.Error())
		return
	}

	job := h.jobs.Create(rv.ProjectID)
	if err := h.reviewSvc.MarkRegenerating(rv.ID, job.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "START_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID, "review_id": rv.ID})

	go h.runRegenerate(job, rv, project)
}

// answeredFollowupCount is how many turns of the thread actually produced an
// answer. Errored turns are excluded for the same reason followupHistory
// skips them: they are not part of the conversation the report is based on,
// so counting them would overstate what v2 took in.
func (h *ReviewHandler) answeredFollowupCount(reviewID string) int {
	return len(h.followupHistory(reviewID, ""))
}

// runRegenerate drives one regeneration end to end. Every exit path goes
// through finishRegenerate, so a crashed run never leaves the row stuck in
// `running` with a report the UI refuses to show.
func (h *ReviewHandler) runRegenerate(job *store.Job, rv *model.CodeReview, project *model.Project) {
	ctx := context.Background()

	// Same binding chain as the review itself, so v2 is written by the model
	// that wrote v1 (and billed to the same config).
	rolePrompt, modelName, claudeConfigID := h.roleConfig("reviewer")
	if picked := strings.TrimSpace(rv.Model); picked != "" && picked != DefaultModelLabel {
		modelName = picked
	}
	claudeConfigID = h.resolveReviewConfigID(strings.TrimSpace(rv.ClaudeConfigID), modelName, claudeConfigID)
	job.SetModel(modelName)

	history := h.followupHistoryCapped(rv.ID)
	job.Append(store.LogLine{Type: "phase", Content: fmt.Sprintf("♻️ 基于 %d 条追问重新生成 Review 报告...", len(history))})

	hasAttachments := attachManifestFor(h.attachSvc, rv.ID, "") != ""
	spec := reviewTurnSpec{
		scope:             "review-regenerate",
		prompt:            func(resumed bool) string { return regeneratePrompt(rv, history, resumed, hasAttachments) },
		usage:             func() *usageCtx { return h.newRegenerateUsage(job.ID, rv, modelName) },
		resumePhase:       "🔗 续接本次 Review 的会话上下文...",
		staleNotice:       "⚠️ 原 Review 会话已失效，改用「上一版报告 + 追问对话 + 仓库代码」重新生成",
		remoteMissingRepo: "Agent 服务器上的仓库副本已不存在，请重新运行一次 Review 后再重新生成报告",
		remoteNoService:   "Agent 服务器服务未初始化，无法在远端重新生成报告",
	}

	report, _, errMsg := h.runReviewTurn(ctx, job, rv, project, rolePrompt, modelName, claudeConfigID, spec)
	h.finishRegenerate(job, rv, modelName, report, errMsg)
}

// finishRegenerate is the single terminal path of a regeneration.
//
// Success: the new report replaces the current one through the ordinary
// MarkFinished path (same parsing, same columns, so every reader downstream
// is unchanged) and is frozen as the next version.
//
// Failure — including the quiet one where claude returns nothing at all —
// restores `done` and keeps the previous report. The user ends the attempt
// exactly where they started, which is the contract this feature promises.
func (h *ReviewHandler) finishRegenerate(job *store.Job, rv *model.CodeReview, modelName, report, errMsg string) {
	if errMsg == "" && strings.TrimSpace(report) == "" {
		errMsg = "模型未返回任何报告内容，已保留上一版报告"
	}
	if errMsg != "" {
		job.Append(store.LogLine{Type: "error", Content: errMsg})
		if err := h.reviewSvc.MarkRegenerateFailed(rv.ID); err != nil {
			log.Printf("[Review] roll back regeneration %s: %v", rv.ID, err)
		}
		h.saveReviewJobLog(job, modelName)
		job.Finish(1, store.JobError)
		return
	}

	outcome := buildReviewOutcome(model.ReviewStatusDone, report)
	if err := h.reviewSvc.MarkFinished(rv.ID, outcome); err != nil {
		log.Printf("[Review] persist regenerated review %s: %v", rv.ID, err)
	}
	// Snapshot from the freshly persisted row rather than from `outcome`, so
	// the revision carries exactly what the review now serves (including the
	// effective model stamped at run time).
	if updated, err := h.reviewSvc.Get(rv.ID); err == nil {
		if _, serr := h.revisionSvc.Snapshot(updated, model.RevisionTriggerRegenerate, h.answeredFollowupCount(rv.ID)); serr != nil {
			// The report itself is already saved and visible; only its entry
			// in the version list is missing. Not worth failing the job.
			log.Printf("[Review] snapshot regenerated revision %s: %v", rv.ID, serr)
		}
	} else {
		log.Printf("[Review] reload regenerated review %s: %v", rv.ID, err)
	}

	h.saveReviewJobLog(job, modelName)
	job.Finish(0, store.JobDone)
}

// saveReviewJobLog persists a finished review-family job so a backend restart
// doesn't wipe the record (the JobStore is a memory-only ring buffer). Review
// jobs aren't bound to a requirement, so requirement_id stays empty and the
// effective model lives on job_logs.model.
func (h *ReviewHandler) saveReviewJobLog(job *store.Job, modelName string) {
	lines, jobStatus, exitCode := job.Snapshot()
	if err := h.jobLogSvc.Save(job.ID, "", string(jobStatus), exitCode, job.StartedAt, job.FinishedAt, lines, modelName); err != nil {
		log.Printf("[Review] failed to persist job log %s: %v", job.ID, err)
	}
}

// followupHistoryCapped is the whole answered thread, with each answer
// trimmed to the regeneration budget. Unlike followupHistory's use in 追问
// (where the thread is context for one question) nothing is excluded here:
// the conversation IS the input.
func (h *ReviewHandler) followupHistoryCapped(reviewID string) []prompt.FollowupTurn {
	turns := h.followupHistory(reviewID, "")
	for i := range turns {
		turns[i].Answer = truncateStr(turns[i].Answer, maxRegenerateAnswerChars)
	}
	return turns
}

// regeneratePrompt renders the task prompt for one regeneration attempt.
// resumed selects the short form (the session still holds the diff and the
// report) or the self-contained one (report + thread pasted in).
func regeneratePrompt(rv *model.CodeReview, history []prompt.FollowupTurn, resumed, hasAttachments bool) string {
	in := prompt.RegeneratePromptInput{
		Resumed:        resumed,
		ReviewType:     rv.ReviewType,
		History:        history,
		HasAttachments: hasAttachments,
	}
	if !resumed {
		in.Branch = rv.Branch
		in.BaseBranch = rv.BaseBranch
		in.PRNumber = rv.PRNumber
		in.PRTitle = rv.Title
		in.Description = rv.Description
		in.ReportMD = truncateStr(rv.ResultMD, maxRegenerateReportChars)
		// The resolved refs aren't persisted, so the prompt names the branch
		// pair and lets the model resolve it if it wants the diff again.
		in.HeadRef, in.BaseRef = rv.Branch, rv.BaseBranch
	}
	return prompt.BuildRegeneratePrompt(in)
}

// newRegenerateUsage mirrors newReviewUsage but tags the row as a
// regeneration, so the cost of iterating on a report is visible separately
// from the cost of producing the first one.
func (h *ReviewHandler) newRegenerateUsage(jobID string, rv *model.CodeReview, modelName string) *usageCtx {
	u := h.newReviewUsage(jobID, rv, modelName)
	u.Step = "review-regenerate"
	return u
}
