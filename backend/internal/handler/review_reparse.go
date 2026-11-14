package handler

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// ── 重新解析结构化结果 (re-parse a stored report's contract block) ────────
//
// A Review report is Markdown plus a trailing JSON contract block. The
// structured half of it (the graded findings, the counts, the 目标达成
// evidence table) is what makes the report ACTIONABLE: it is what the
// 「生成修复子任务」 button turns into work items, and what the ledger shows.
//
// The contract block used to be decoded with one strict Unmarshal into one
// strongly-typed struct. A single field of the wrong TYPE anywhere in it —
// and the commonest such field is `"line": 664` where the contract asked for
// `"line": "12-20"` — produced an UnmarshalTypeError for the WHOLE blob.
// The report then degraded to "no structured findings": the prose was still
// there, but the grading and the evidence table were gone, and no fix
// sub-task could be generated from it.
//
// The parser is now tolerant (see review_parse.go), so a report produced
// AFTER that fix never degrades this way. But a report produced BEFORE it
// is still degraded, and re-running the model to fix a parsing bug is a
// terrible trade: the user pays for a whole Review again (and gets a
// DIFFERENT report, which is not what they asked for).
//
// So this endpoint re-reads the contract block OUT OF THE STORED REPORT
// TEXT and back-fills the structured columns. No model runs; the report the
// user already read is the report they keep.
//
// Two rules keep it honest:
//   - it only touches the STRUCTURED columns (plus stripping the contract
//     block out of result_md, so the stored text matches what a normally
//     parsed report looks like). status / job_id / finished_at / revision_no
//     are untouched — no new run happened, so no new version either.
//   - a review that is still running is refused: its report is not final
//     yet, and the two writers would race.

const (
	// reparseContractMissing is the code for "the stored report has no
	// contract block at all" — as opposed to FINDINGS_UNAVAILABLE, which
	// means "there is a contract but we could not read it". The two need
	// different messages: the first is fixed by re-running the Review, the
	// second by fixing the model output.
	reparseContractMissing = "CONTRACT_UNAVAILABLE"
)

// ReparseReview re-reads the trailing JSON contract of a finished report and
// back-fills the structured columns (findings / counts / goal_met /
// goal_checks) from it.
//
// POST /api/reviews/{rid}/reparse
func (h *ReviewHandler) ReparseReview(w http.ResponseWriter, r *http.Request) {
	rv, ok := h.lookupReview(w, r)
	if !ok {
		return
	}
	if h.reviewSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "Review 会话服务未启用")
		return
	}
	if rv.Status == model.ReviewStatusRunning || rv.Status == model.ReviewStatusDraft {
		writeError(w, http.StatusConflict, "REVIEW_NOT_FINISHED",
			"本次 Review 尚未产出最终报告，结束后才能重新解析")
		return
	}
	if strings.TrimSpace(rv.ResultMD) == "" {
		writeError(w, http.StatusUnprocessableEntity, reparseContractMissing,
			"该 Review 没有报告正文，无法重新解析")
		return
	}

	jsonPart, markdownPart := extractTrailingJSON(rv.ResultMD)
	if jsonPart == "" {
		writeError(w, http.StatusUnprocessableEntity, reparseContractMissing,
			"报告正文中没有找到 JSON 契约块，无法重新解析")
		return
	}

	sections := parseContractSections(jsonPart)
	if !sections.FindingsOK {
		writeError(w, http.StatusUnprocessableEntity, "FINDINGS_UNAVAILABLE",
			"报告中的问题清单段落格式异常，无法重新解析")
		return
	}

	// The same shape buildReviewOutcome produces for a normally parsed
	// report: the contract block is stripped out of the report text and the
	// structured half lives in its own columns.
	raw, err := json.Marshal(sections.Findings)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "PARSE_FAILED", err.Error())
		return
	}
	out := service.ReviewOutcome{
		Status:         rv.Status,
		ResultMD:       markdownPart,
		FindingsJSON:   string(raw),
		GoalMet:        sections.GoalMet,
		GoalChecksJSON: sections.GoalChecksJSON,
	}
	out.P0, out.P1, out.P3 = countBySeverity(sections.Findings)

	if err := h.reviewSvc.UpdateParsedFindings(rv.ID, out); err != nil {
		writeError(w, http.StatusInternalServerError, "UPDATE_FAILED", err.Error())
		return
	}

	// Keep the frozen version in step with the row it mirrors. A re-parse
	// produced no new report, so it must not create a new version — it
	// corrects the one the review already serves.
	updated, err := h.reviewSvc.Get(rv.ID)
	if err == nil && h.revisionSvc != nil {
		if serr := h.revisionSvc.UpdateCurrent(updated); serr != nil {
			// The row is already corrected and visible; only the history
			// entry lagged behind. Not worth failing the request over.
			log.Printf("[Review] update current revision %s: %v", rv.ID, serr)
		}
	} else if err != nil {
		log.Printf("[Review] reload reparsed review %s: %v", rv.ID, err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"p0":       out.P0,
		"p1":       out.P1,
		"p3":       out.P3,
		"goal_met": out.GoalMet,
		"findings": len(sections.Findings),
	})
}
