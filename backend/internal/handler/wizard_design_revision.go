// Package handler — wizard design-revision REST endpoints.
//
// wizard_design_revision.go exposes the version history of a requirement's
// design plan over HTTP. requirements.design_docs is the CURRENT version
// (every other read path keeps reading it as before); design_revisions is
// the history, and these three endpoints let the front-end list, fetch, and
// restore the older versions. Pattern is parallel to handler/review.go's
// code-review endpoint family (CodeReviewRevisionService + 3 routes) so
// the front-end's mental model and the route shape are identical.
package handler

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// ListDesignRevisions — GET /api/requirements/{id}/design-revisions
//
// Returns the requirement's design plan version history oldest-first
// (v1, v2, …). The current version IS NOT in this list — every revision
// row stores a version that USED TO BE current (see Snapshot's doc
// comment in service/design_revision.go). The current version lives on
// requirements.design_docs and is rendered by the detail page's normal
// read path.
//
// Empty array when the requirement has no versions (a requirement that
// has never been regenerated). The front-end uses that to decide whether
// to render the version switcher at all.
func (h *WizardHandler) ListDesignRevisions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "missing requirement id")
		return
	}
	if h.designRevSvc == nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR",
			"design revision service unavailable")
		return
	}
	req, err := h.reqSvc.Get(id)
	if err != nil {
		if errors.Is(err, service.ErrRequirementNotFound) {
			writeError(w, http.StatusNotFound, "REQUIREMENT_NOT_FOUND", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	// Per-kind filter mirrors the wizard stage discrimination:
	// design revisions on wiki rows would be confusing because wiki
	// never enters the architect pipeline. The check is defensive —
	// the front-end hides the switcher for kind=wiki — but rejecting
	// here keeps the API from serving meaningless rows on a future
	// caller that forgets the filter.
	if req.Kind == service.KindWiki {
		writeError(w, http.StatusBadRequest, "KIND_NOT_SUPPORTED",
			"wiki-kind requirements do not have design revisions (use wiki_docs)")
		return
	}
	revs, err := h.designRevSvc.ListByRequirement(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	if revs == nil {
		revs = []model.DesignRevision{}
	}
	// Include the requirement's CURRENT rev_no alongside the history so
	// the front-end can label the dropdown "当前版 · v3" without a
	// separate round-trip.
	writeJSON(w, http.StatusOK, map[string]any{
		"revisions":          revs,
		"current_revision":   req.DesignRevisionNo,
		"current_session_id": req.DesignSessionID,
	})
}

// GetDesignRevision — GET /api/requirements/{id}/design-revisions/{n}
//
// Returns one version's full body. The front-end opens this when the user
// previews an old version without committing to a restore. 404 when the
// version does not exist.
func (h *WizardHandler) GetDesignRevision(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "missing requirement id")
		return
	}
	if h.designRevSvc == nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR",
			"design revision service unavailable")
		return
	}
	revNo, err := parseRevisionNo(r.PathValue("n"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", err.Error())
		return
	}
	rev, err := h.designRevSvc.Get(id, revNo)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "REVISION_NOT_FOUND",
				fmt.Sprintf("design revision v%d not found for %s", revNo, id))
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rev)
}

// RestoreDesignRevision — POST /api/requirements/{id}/design-revisions/{n}/restore
//
// Copies an older version's design_docs back onto requirements,
// making it the plan every other read path serves. No new revision row
// is written — the version list is a record of what was PRODUCED, not
// which one is selected, so v1↔v2 can flip back and forth indefinitely.
// design_session_id is deliberately untouched: the session belongs to
// the most recent RUN, not to the selected version, so the doc-refine
// chat keeps resuming its conversation regardless of which version the
// user picked to look at.
//
// 404 when the version does not exist. After a successful restore the
// front-end should re-GET the requirement to pick up the new
// design_docs and refreshed design_revision_no.
func (h *WizardHandler) RestoreDesignRevision(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "missing requirement id")
		return
	}
	if h.designRevSvc == nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR",
			"design revision service unavailable")
		return
	}
	revNo, err := parseRevisionNo(r.PathValue("n"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", err.Error())
		return
	}
	if err := h.designRevSvc.RestoreAsCurrent(id, revNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "REVISION_NOT_FOUND",
				fmt.Sprintf("design revision v%d not found for %s", revNo, id))
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	// Return the freshly-loaded requirement so the front-end can swap it
	// into local state without a second GET (same envelope as the
	// architect-design / import-design success responses).
	req, gerr := h.reqSvc.Get(id)
	if gerr != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", gerr.Error())
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// parseRevisionNo accepts path values like "1", "v1", "v10" (URL paths
// don't allow leading "-" on consecutive segments cleanly) and fails
// fast on anything else. The wizard-architect + code-review precedents
// use an integer URL segment for the same reason; we accept the "v"
// prefix so the front-end's dropdown can click straight through.
func parseRevisionNo(s string) (int, error) {
	v := s
	if len(v) > 1 && v[0] == 'v' {
		v = v[1:]
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid revision number %q", s)
	}
	return n, nil
}
