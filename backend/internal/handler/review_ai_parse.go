package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// ── AI 解析结构化结果（第 2 层：LLM 兜底） ─────────────────────────────────
//
// A Review report's structured half has three sources, and this endpoint is
// the third:
//
//  1. the report's trailing ```json contract block — primary, free;
//  2. the report's own P0/P1/P3 sections — deterministic, free, no model
//     (handler/review_mdparse.go);
//  3. an LLM extraction from the report body — THIS endpoint.
//
// The first two cover almost everything, because a report that spells out
// "P0: this file, this line, this problem" IS structured, even if the
// model forgot the machine-readable block. The third exists for the report
// whose prose is too free-form for the deterministic parser — and it is the
// only one that COSTS TOKENS and can take minutes, so it is never automatic:
// the user presses a button.
//
// Two rules shape the whole file:
//
//   - it never touches the report TEXT. The user read a report; this
//     endpoint adds the structured half to it, it does not rewrite it. A
//     re-graded report that no longer matches the prose would be worse than
//     no grading at all.
//   - it is ASYNCHRONOUS (a JobStore job + the shared SSE channel), because
//     a synchronous request would be cut off by any reverse proxy's read
//     timeout long before a model finished.
//
// The concurrency gate matters: two clicks would run two extractions and
// race to write the same row, wasting a model call each.

const (
	// aiParseInProgress is the code for "an extraction for this review is
	// already running". It is a 409, not a silent no-op: the user asked for
	// something that is already happening, and saying so is more useful than
	// pretending to start a second one.
	aiParseInProgress = "AI_PARSE_IN_PROGRESS"
)

// aiParseGates is the per-review concurrency gate.
//
// It is process-local, exactly like the requirement-split gate
// (handler/requirement_split_registry.go's splitLocks): the JobStore is
// process-local too, so a second process would have its own gate — which is
// no worse than the JobStore's own process-local semantics.
var aiParseGates = struct {
	mu sync.Mutex
	m  map[string]bool // review id → an extraction is in flight
}{m: map[string]bool{}}

// acquireAIParse takes the gate for a review id. ok=false means another
// extraction is already running for it.
func acquireAIParse(reviewID string) bool {
	aiParseGates.mu.Lock()
	defer aiParseGates.mu.Unlock()
	if aiParseGates.m[reviewID] {
		return false
	}
	aiParseGates.m[reviewID] = true
	return true
}

// releaseAIParse frees the gate for a review id.
func releaseAIParse(reviewID string) {
	aiParseGates.mu.Lock()
	defer aiParseGates.mu.Unlock()
	delete(aiParseGates.m, reviewID)
}

// ReparseReviewWithAI extracts the structured findings from a finished
// report's body using an LLM, and back-fills the structured columns.
//
// POST /api/reviews/{rid}/reparse/ai → {"job_id": "..."}
//
// It returns a JobStore id immediately; the extraction runs in the
// background and its log lines stream over the shared wizard job channel
// (GET /api/wizard/jobs/{job_id}/stream), the same channel the QA chat and
// the wizard use.
func (h *ReviewHandler) ReparseReviewWithAI(w http.ResponseWriter, r *http.Request) {
	rv, ok := h.lookupReview(w, r)
	if !ok {
		return
	}
	if h.extractFindings == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "LLM 通道未启用")
		return
	}
	if rv.Status == model.ReviewStatusRunning || rv.Status == model.ReviewStatusDraft {
		writeError(w, http.StatusConflict, "REVIEW_NOT_FINISHED",
			"本次 Review 尚未产出最终报告，结束后才能解析")
		return
	}
	if strings.TrimSpace(rv.ResultMD) == "" {
		writeError(w, http.StatusUnprocessableEntity, reparseContractMissing,
			"该 Review 没有报告正文，无法解析")
		return
	}
	if !acquireAIParse(rv.ID) {
		writeError(w, http.StatusConflict, aiParseInProgress,
			"本次 Review 正在解析中，请等待当前解析结束")
		return
	}

	job := h.jobs.Create(rv.ProjectID)
	writeJSON(w, http.StatusOK, map[string]any{"job_id": job.ID, "review_id": rv.ID})

	go h.runAIParse(job, rv)
}

// runAIParse drives one extraction end to end.
//
// Every exit path releases the gate, so a failed run never leaves the
// review permanently un-parsable.
func (h *ReviewHandler) runAIParse(job *store.Job, rv *model.CodeReview) {
	defer releaseAIParse(rv.ID)

	job.Append(store.LogLine{Type: "phase", Content: "🤖 正在用 AI 从报告正文中提取分级问题清单…"})

	raw, err := h.extractFindings(rv.ResultMD)
	if err != nil {
		job.Append(store.LogLine{Type: "error", Content: "❌ AI 解析失败：" + err.Error()})
		h.closeReviewJob(job, "", true)
		return
	}

	// The extractor answers in the SAME contract shape the reviewer was
	// asked to emit, so it goes through the same tolerant decoder — one
	// shape, one parser, no second implementation.
	sections := parseContractSections(raw)
	if !sections.FindingsOK || len(sections.Findings) == 0 {
		job.Append(store.LogLine{Type: "error",
			Content: "⚠️ AI 没有从报告正文中解析出分级问题清单。报告正文可能没有写明等级；请检查报告内容。"})
		h.closeReviewJob(job, "", true)
		return
	}

	marshaled, err := json.Marshal(sections.Findings)
	if err != nil {
		job.Append(store.LogLine{Type: "error", Content: "❌ 解析结果序列化失败：" + err.Error()})
		h.closeReviewJob(job, "", true)
		return
	}

	// The report text is NOT touched: the user read it, and it stays as it
	// is. Only the structured columns change.
	out := service.ReviewOutcome{
		Status:         rv.Status,
		ResultMD:       rv.ResultMD,
		FindingsJSON:   string(marshaled),
		FindingsSource: reviewFindingsSourceLLM,
		GoalMet:        sections.GoalMet,
		GoalChecksJSON: sections.GoalChecksJSON,
	}
	out.P0, out.P1, out.P3 = countBySeverity(sections.Findings)

	if err := h.reviewSvc.UpdateParsedFindings(rv.ID, out); err != nil {
		job.Append(store.LogLine{Type: "error", Content: "❌ 解析结果落库失败：" + err.Error()})
		h.closeReviewJob(job, "", true)
		return
	}

	// Keep the frozen version in step with the row it mirrors — same as
	// the deterministic re-parse does. No new version is created: no new
	// report was produced.
	if updated, err := h.reviewSvc.Get(rv.ID); err == nil {
		if h.revisionSvc != nil {
			if serr := h.revisionSvc.UpdateCurrent(updated); serr != nil {
				log.Printf("[Review] update current revision after AI parse %s: %v", rv.ID, serr)
			}
		}
	} else {
		log.Printf("[Review] reload AI-parsed review %s: %v", rv.ID, err)
	}

	job.Append(store.LogLine{Type: "phase",
		Content: fmt.Sprintf("✅ 已解析出 P0 %d / P1 %d / P3 %d", out.P0, out.P1, out.P3)})
	h.closeReviewJob(job, "", false)
}
