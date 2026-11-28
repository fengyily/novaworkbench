// Package handler — the 「实施发现」 REST endpoints and the design-stage
// findings producer.
//
// Findings are the structured channel that carries what a child requirement
// discovered back to its master. Before this existed, a child that found the
// master plan to be wrong, incomplete or conflicting had nowhere to say so:
// the master plan kept describing the world as it looked before the split,
// and the later children kept designing against that stale picture.
//
// The producer is free: the architect's plan contract already asks for a
// 「### 与主需求的偏差」 subsection, so the handler simply parses it out of the
// stored plan at persist time. No extra LLM call, no extra model turn.
//
// The review side is human: the master requirement's detail page lists the
// findings, the user accepts or rejects each one, and the accepted ones
// become a mandatory checklist in the master plan's refine prompt — which is
// how a confirmed change finally gets absorbed into the master plan.
package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
	promptpkg "github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

// recordDesignFindings harvests the 「与主需求的偏差」 section out of a
// freshly produced plan and files each entry as a finding against the master.
//
// Called from finalizeArchitectRun right after the plan is persisted.
//
// Best-effort by design: a findings failure must never fail a design run
// that already produced a good plan. The same applies to the section being
// absent — a model that skipped it simply contributes nothing.
func (h *WizardHandler) recordDesignFindings(reqID string, parent *model.Requirement, planMarkdown string) int {
	if h.findingSvc == nil || parent == nil || strings.TrimSpace(reqID) == "" {
		return 0
	}
	bullets := promptpkg.ExtractSectionBullets(planMarkdown, promptpkg.FindingsSourceSection)
	if len(bullets) == 0 {
		return 0
	}
	parentHash := util.SHA256Hex(parent.DesignDocs)
	recorded := 0
	for _, bullet := range bullets {
		title, detail, severity := parseFindingBullet(bullet)
		if title == "" {
			continue
		}
		err := h.findingSvc.Record(model.RequirementFinding{
			RequirementID:    reqID,
			ParentID:         parent.ID,
			Stage:            service.FindingStageDesign,
			Severity:         severity,
			Title:            title,
			Detail:           detail,
			SourceSection:    promptpkg.FindingsSourceSection,
			ParentDesignHash: parentHash,
		})
		if err != nil {
			log.Printf("[findings] failed to record finding for %s: %v", reqID, err)
			continue
		}
		recorded++
	}
	return recorded
}

// parseFindingBullet splits one bullet of the 「与主需求的偏差」 section into
// its parts.
//
// The contract asks for
//
//	- [类型: 冲突|新事实|局部决策] 结论 —— 依据（文件:行 / 章节）
//
// but the model is free-form, so every part is optional: the bracket may be
// missing, the "——" separator may be missing, and the bullet may be a
// placeholder like 「无」.
//
// The bracket becomes the severity, the part before "——" becomes the title
// (the one-line conclusion) and the part after becomes the detail (the
// evidence).
func parseFindingBullet(bullet string) (title, detail, severity string) {
	s := strings.TrimSpace(bullet)
	if s == "" {
		return "", "", ""
	}
	// A placeholder bullet is not a finding.
	switch strings.TrimSpace(strings.Trim(s, "。.!！")) {
	case "无", "没有", "无。", "N/A", "n/a", "None", "none":
		return "", "", ""
	}
	severity = service.FindingSeverityInfo
	if strings.HasPrefix(s, "[") {
		if end := strings.Index(s, "]"); end > 1 {
			tag := s[1:end]
			s = strings.TrimSpace(s[end+1:])
			switch {
			case strings.Contains(tag, "冲突"):
				severity = service.FindingSeverityConflict
			case strings.Contains(tag, "偏差"):
				severity = service.FindingSeverityDeviation
			case strings.Contains(tag, "阻塞"):
				severity = service.FindingSeverityBlocker
			}
		}
	}
	sep := "——"
	idx := strings.Index(s, sep)
	if idx < 0 {
		sep = "—"
		idx = strings.Index(s, sep)
	}
	if idx < 0 {
		title = util.TruncateRunes(s, 120, "…")
		return title, "", severity
	}
	title = strings.TrimSpace(s[:idx])
	detail = strings.TrimSpace(s[idx+len(sep):])
	if title == "" {
		title = util.TruncateRunes(detail, 120, "…")
		detail = ""
	}
	return util.TruncateRunes(title, 120, "…"), detail, severity
}

// ListRequirementFindings — GET /api/requirements/{id}/findings
//
// Returns the findings addressed to this requirement when it is a master
// (its children's findings), or the findings this requirement filed when it is
// a child. `?child_id=` narrows to one child; `?status=` filters by verdict.
func (h *WizardHandler) ListRequirementFindings(w http.ResponseWriter, r *http.Request) {
	if h.findingSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "实施发现功能未启用")
		return
	}
	id := r.PathValue("id")
	childID := strings.TrimSpace(r.URL.Query().Get("child_id"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))

	var items []model.RequirementFinding
	var err error
	if childID != "" {
		items, err = h.findingSvc.ListByRequirement(childID)
	} else if reqRow, gerr := h.reqSvc.Get(id); gerr == nil && strings.TrimSpace(reqRow.ParentID) != "" {
		// A child: the findings it filed itself.
		items, err = h.findingSvc.ListByRequirement(id)
	} else {
		// A master: the queue its children filed against it.
		items, err = h.findingSvc.ListByParent(id)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "FINDINGS_FAILED", err.Error())
		return
	}
	if status != "" {
		filtered := make([]model.RequirementFinding, 0, len(items))
		for _, f := range items {
			if f.Status == status {
				filtered = append(filtered, f)
			}
		}
		items = filtered
	}
	if items == nil {
		items = []model.RequirementFinding{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"findings": items,
		"counts":   countFindings(items),
	})
}

// countFindings summarises the queue by verdict so the UI can render the
// "N 条待复核" badge without walking the array.
func countFindings(items []model.RequirementFinding) map[string]int {
	counts := map[string]int{
		service.FindingStatusPending:  0,
		service.FindingStatusAccepted: 0,
		service.FindingStatusRejected: 0,
	}
	for _, f := range items {
		counts[f.Status]++
	}
	return counts
}

// UpdateRequirementFinding — PATCH /api/requirements/{id}/findings/{fid}
//
// Body: {"status": "accepted" | "rejected" | "pending", "resolution": "..."}
//
// The finding must belong to this requirement (a master owns the queue; a
// child owns what it filed). A mismatch is a 404 rather than a silent
// no-op — the reviewer must not be told a verdict was recorded when it was
// not.
func (h *WizardHandler) UpdateRequirementFinding(w http.ResponseWriter, r *http.Request) {
	if h.findingSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "实施发现功能未启用")
		return
	}
	var body struct {
		Status     string `json:"status"`
		Resolution string `json:"resolution"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON")
		return
	}
	id := r.PathValue("id")
	fid := r.PathValue("fid")

	owner := ""
	if reqRow, err := h.reqSvc.Get(id); err == nil && strings.TrimSpace(reqRow.ParentID) != "" {
		owner = id
	} else {
		// Master: the finding must be addressed to it.
		owner = id
	}
	if err := h.findingSvc.SetStatus(fid, body.Status, body.Resolution, owner); err != nil {
		if errors.Is(err, service.ErrFindingNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "实施发现不存在或不属于该需求")
			return
		}
		writeError(w, http.StatusBadRequest, "FINDINGS_UPDATE_FAILED", err.Error())
		return
	}
	items, err := h.findingSvc.ListByParent(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "FINDINGS_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"findings": items,
		"counts":   countFindings(items),
	})
}

// CreateRequirementFinding — POST /api/requirements/{id}/findings
//
// Manual filing: the model sometimes skips the 「与主需求的偏差」 section, and
// the reviewer may still want to record something. Body:
//
//	{"title": "...", "detail": "...", "severity": "conflict", "child_id": "req_x"}
//
// child_id defaults to the requirement itself.
func (h *WizardHandler) CreateRequirementFinding(w http.ResponseWriter, r *http.Request) {
	if h.findingSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "实施发现功能未启用")
		return
	}
	var body struct {
		Title    string `json:"title"`
		Detail   string `json:"detail"`
		Severity string `json:"severity"`
		ChildID  string `json:"child_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON")
		return
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		writeError(w, http.StatusBadRequest, "TITLE_REQUIRED", "标题不能为空")
		return
	}
	id := r.PathValue("id")
	childID := strings.TrimSpace(body.ChildID)
	if childID == "" {
		childID = id
	}
	parentID := id
	if reqRow, err := h.reqSvc.Get(childID); err == nil && strings.TrimSpace(reqRow.ParentID) != "" {
		parentID = reqRow.ParentID
	}
	severity := strings.TrimSpace(body.Severity)
	if severity == "" {
		severity = service.FindingSeverityInfo
	}
	err := h.findingSvc.Record(model.RequirementFinding{
		RequirementID: childID,
		ParentID:      parentID,
		Stage:         service.FindingStageDesign,
		Severity:      severity,
		Status:        service.FindingStatusPending,
		Title:         util.TruncateRunes(title, 120, "…"),
		Detail:        strings.TrimSpace(body.Detail),
		SourceSection: "手动录入",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "FINDINGS_CREATE_FAILED", err.Error())
		return
	}
	items, lerr := h.findingSvc.ListByParent(parentID)
	if lerr != nil {
		writeError(w, http.StatusInternalServerError, "FINDINGS_FAILED", lerr.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"findings": items,
		"counts":   countFindings(items),
	})
}

// findingsAbsorbBlock renders the accepted-but-unabsorbed findings of a
// MASTER requirement as the mandatory checklist a refine / apply turn has to
// reconcile into the plan.
//
// Returns "" when the requirement is a child (a child has no queue) or when
// there is nothing outstanding — so the common case leaves the prompt
// byte-identical to before.
func (h *WizardHandler) findingsAbsorbBlock(req *model.Requirement) string {
	if h.findingSvc == nil || req == nil {
		return ""
	}
	if strings.TrimSpace(req.ParentID) != "" {
		// Only a master requirement owns a queue.
		return ""
	}
	findings, err := h.findingSvc.ListUnabsorbed(req.ID)
	if err != nil {
		log.Printf("[findings] list unabsorbed for %s: %v", req.ID, err)
		return ""
	}
	return promptpkg.BuildFindingsAbsorbBlock(findings)
}

// absorbFindingsAfterPlanWrite marks the accepted findings of a requirement
// as folded into its plan.
//
// Called after a design-doc write on a MASTER requirement. The accounting is
// deliberately coarse — the model rewrites the whole document, so there is no
// reliable way to attribute which finding it adopted; the honest rule is "the
// user refined the master plan while these findings were outstanding". The
// refine prompt asks the model to record an explicit rejection with a reason
// for anything it did not adopt, so the outcome stays visible in the plan.
//
// Best-effort: a failure here only leaves the queue looking un-absorbed.
func (h *WizardHandler) absorbFindingsAfterPlanWrite(req *model.Requirement) {
	if h.findingSvc == nil || req == nil {
		return
	}
	if strings.TrimSpace(req.ParentID) != "" {
		// Only a master has a queue to absorb.
		return
	}
	seq, err := h.findingSvc.NextAbsorbSeq(req.ID)
	if err != nil {
		log.Printf("[findings] next absorb seq for %s: %v", req.ID, err)
		return
	}
	if _, err := h.findingSvc.MarkAbsorbed(req.ID, seq, req.DesignRevisionNo); err != nil {
		log.Printf("[findings] mark absorbed for %s: %v", req.ID, err)
	}
}

// jobLogFindings appends a one-line summary of the findings a plan produced
// to the job log, so the user sees the harvest happen without opening the
// review panel.
func jobLogFindings(job *store.Job, n int) {
	if job == nil || n <= 0 {
		return
	}
	job.Append(store.LogLine{Type: "phase", Content: "📝 已上报 " + strconv.Itoa(n) + " 条实施发现，等待主需求复核"})
}
