package handler

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// reportSourceMaxBytes caps the LLM input. A finished dev report can run
// tens of KB; 48 KB mirrors the per-item budget that buildKnowledgeBlock
// already enforces for the read path, so the input/output budgets are
// self-consistent (an item this big costs a 48 KB prompt + 4 KB completion
// at most). A "已截断" footer is appended so the LLM (and any human reader
// of the fallback row) sees a hard cut-off rather than a silent mid-UTF8
// slice.
const reportSourceMaxBytes = 48 * 1024

// ReportArchiveHandler exposes three endpoints under /api/requirements/{id}/
// that turn finished reports into knowledge rows:
//
//	POST   .../dev-report/archive          — main-agent summary (requirements.coding_plan)
//	POST   .../sub-tasks/{sid}/archive     — single sub-task (sub_tasks.artifact)
//	GET    .../report-archives             — list of archived kb ids keyed by source_ref
//
// Each POST is sync: the click triggers one LLM round (HTTP channel, not
// the claude CLI), then a single knowledge upsert, then a token_usage
// record. The "never fail" guarantee is enforced at the LLM boundary — when
// the HTTP channel is unconfigured or the model errors, the handler falls
// back to archiving the raw report with a clearly-marked fallback header
// so the user's "归档" never returns an error. LLM errors are still logged
// (a silent fallback would hide a misconfigured channel).
//
// Unarchive reuses the existing DELETE /api/knowledge/{id} endpoint — no new
// route here, the frontend calls knowledgeApi.delete directly.
type ReportArchiveHandler struct {
	reqSvc     *service.RequirementService
	subTaskSvc *service.SubTaskService
	archiveSvc *service.ReportArchiveService
	llm        *llm.Gateway
	usageSvc   *service.UsageService
}

func NewReportArchiveHandler(
	reqSvc *service.RequirementService,
	subTaskSvc *service.SubTaskService,
	archiveSvc *service.ReportArchiveService,
	gw *llm.Gateway,
	usageSvc *service.UsageService,
) *ReportArchiveHandler {
	return &ReportArchiveHandler{reqSvc: reqSvc, subTaskSvc: subTaskSvc, archiveSvc: archiveSvc, llm: gw, usageSvc: usageSvc}
}

// ArchiveDevReport distills requirements.coding_plan into a knowledge row.
// 404 NOT_FOUND when the requirement doesn't exist; 400 NO_REPORT when
// coding_plan is empty (a "归档" click on a requirement whose main agent
// never produced a summary must not silently write a blank row).
func (h *ReportArchiveHandler) ArchiveDevReport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	req, err := h.reqSvc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "requirement not found")
		return
	}
	if strings.TrimSpace(req.CodingPlan) == "" {
		writeError(w, http.StatusBadRequest, "NO_REPORT", "development report is empty")
		return
	}
	source := "# " + req.Title + "\n\n" + req.CodingPlan
	h.runArchive(w, req.ProjectID, id, service.SourceTypeDevReport,
		"「"+req.Title+"」开发实现报告", source, req.Title, id)
}

// ArchiveSubTaskReport distills one sub_task's artifact into a knowledge row.
// The 404 guards match WizardHandler.GetSubTask's two-stage check (sub-task
// missing → NOT_FOUND, sub-task belongs to a different requirement →
// NOT_FOUND) so a misrouted sid never silently archives against the wrong
// parent. artifact is the sub-task's full report (the wizard handler already
// prepends a title / prompt / model / timestamp header so the row reads
// standalone).
func (h *ReportArchiveHandler) ArchiveSubTaskReport(w http.ResponseWriter, r *http.Request) {
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
	if strings.TrimSpace(st.Artifact) == "" {
		writeError(w, http.StatusBadRequest, "NO_REPORT", "sub-task report is empty")
		return
	}
	// sub_task rows don't carry project_id; resolve it from the parent
	// requirement. A missing parent is impossible (the 404 guard above
	// verified the link), so the only failure mode is a DB read error
	// which surfaces as the standard 5xx.
	parent, perr := h.reqSvc.Get(id)
	if perr != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", perr.Error())
		return
	}
	h.runArchive(w, parent.ProjectID, sid, service.SourceTypeSubtaskReport,
		"「"+st.Title+"」子任务报告", st.Artifact, parent.Title, id)
}

// ListArchives returns the {source_ref: knowledge_id} map for one
// requirement's archived reports. Drives the frontend "已归档" badge so a
// page refresh after archiving shows the correct state without waiting for
// the next list query. Returns {"items": {}} on an empty result.
func (h *ReportArchiveHandler) ListArchives(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	items, err := h.archiveSvc.ListForRequirement(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	if items == nil {
		items = map[string]string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// runArchive is the shared post-validation body. It does the LLM distillation
// (with the raw-content fallback), appends a source-of-origin footer, calls
// the upsert, records token usage, and writes the response. Centralizing
// here keeps the two public handlers thin and ensures the fallback path
// behaves identically for both reports.
func (h *ReportArchiveHandler) runArchive(w http.ResponseWriter, projectID, sourceRef, sourceType, fallbackTitle, sourceText, reqTitle, reqID string) {
	// 1. Length cap (defensive — a multi-MB coding_plan would still work, but
	// the LLM context gets wasteful and the 30s chat timeout gets racy).
	if len(sourceText) > reportSourceMaxBytes {
		sourceText = sourceText[:reportSourceMaxBytes] + "\n…（已截断）"
	}

	// 2. LLM distillation. An error OR an empty markdown triggers the
	// fallback so the user's click is never punished for a misconfigured
	// channel or a model that returned garbage.
	title, markdown, usage, err := h.llm.ExtractReportKnowledge(sourceType, sourceText, "")
	if err != nil || strings.TrimSpace(markdown) == "" {
		if err != nil {
			log.Printf("[report-archive] LLM extract failed (source=%s, err=%v); falling back to raw archive", sourceRef, err)
		}
		kindLabel := "开发实现报告"
		if sourceType == service.SourceTypeSubtaskReport {
			kindLabel = "子任务报告"
		}
		title = fallbackTitle
		markdown = "> 本条由" + kindLabel + "原文归档（AI 提取不可用）\n\n" + sourceText
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = fallbackTitle
	}

	// 3. Source-of-origin footer. The Markdown link target uses the
	// frontend route /requirements/<id> so a click in the knowledge viewer
	// navigates back to the requirement (the new tab / SPA picks it up).
	nowStr := time.Now().Format("2006-01-02 15:04")
	markdown = markdown + "\n\n---\n*来源：需求 [" + reqTitle + "](/requirements/" + reqID + ") · 归档于 " + nowStr + "*"

	// 4. Upsert. A failure here is the only path that can return 5xx;
	// every prior step has a fallback so the user sees a real error only
	// when the database itself is broken.
	kb, err := h.archiveSvc.Upsert(projectID, sourceType, sourceRef, title, markdown)
	if err != nil {
		log.Printf("[report-archive] upsert %s/%s failed: %v", sourceType, sourceRef, err)
		writeError(w, http.StatusInternalServerError, "ARCHIVE_FAILED", err.Error())
		return
	}

	// 5. Token usage. Best-effort — a usage-recording failure must not
	// turn a successful archive into an error response.
	if usage != nil && h.usageSvc != nil {
		u := model.TokenUsage{
			RequirementID: reqID,
			ProjectID:     projectID,
			Step:          "report_archive",
			Model:         usage.Model,
			InputTokens:   usage.PromptTokens,
			OutputTokens:  usage.CompletionTokens,
		}
		if rerr := h.usageSvc.Record(u); rerr != nil {
			log.Printf("[report-archive] record usage for %s failed: %v (ignored)", reqID, rerr)
		}
	}

	writeJSON(w, http.StatusOK, kb)
}
