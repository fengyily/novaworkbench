package handler

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/novaworkbench/backend/internal/middleware"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// AttachmentHandler is the per-requirement file-attachment HTTP surface.
// Routes (registered in main.go):
//
//	POST   /api/requirements/{id}/attachments              — multipart upload
//	GET    /api/requirements/{id}/attachments              — list rows
//	GET    /api/requirements/{id}/attachments/{att_id}/content — download
//	PUT    /api/requirements/{id}/attachments/{att_id}     — patch description
//	DELETE /api/requirements/{id}/attachments/{att_id}     — delete row + file
type AttachmentHandler struct{ svc *service.AttachmentService }

func NewAttachmentHandler(svc *service.AttachmentService) *AttachmentHandler {
	return &AttachmentHandler{svc: svc}
}

// requiredRequirementHeader reads X-Requirement-ID for handlers mounted at
// /api/requirements/{id}/... — Go 1.22 ServeMux already exposes it via
// r.PathValue("id"), so this isn't actually needed; placeholder kept to
// remind reviewers that the route uses the {id} segment directly.
func requiredRequirementHeader(r *http.Request) string {
	return r.PathValue("id")
}

// Upload — POST /api/requirements/{id}/attachments (multipart/form-data).
//
// Multipart shape:
//
//	field "file"        — required, the file content
//	field "description" — optional, user-written annotation (≤ 500 chars)
//
// Layered validation:
//   - http.MaxBytesReader caps the request body at service.MaxAttachmentSize
//     (currently 2 GB, up from the historical 20 MB so log dumps fit), returning
//     413 TOO_LARGE on overflow before any disk hit
//   - r.FormFile extracts the file after the cap succeeds
//   - filename is required (rejects empty submissions)
//   - streamed copy refuses beyond the cap as a second line of defense
//
// Returns 201 with the persisted model.Attachment.
func (h *AttachmentHandler) Upload(w http.ResponseWriter, r *http.Request) {
	reqID := requiredRequirementHeader(r)
	if strings.TrimSpace(reqID) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUIREMENT_ID", "requirement id missing")
		return
	}

	// Reject body before ParseMultipartForm so a multi-GB upload never
	// spills through the form parser — matches knowledge.UploadPdf.
	r.Body = http.MaxBytesReader(w, r.Body, service.MaxAttachmentSize+4096)

	if err := r.ParseMultipartForm(service.MaxAttachmentSize + 4096); err != nil {
		// http.MaxBytesError is the canonical signal that the
		// MaxBytesReader cap fired; map to a clearer 413.
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "TOO_LARGE",
				fmt.Sprintf("附件超过 %s 限制", humanBytes(service.MaxAttachmentSize)))
			return
		}
		writeError(w, http.StatusBadRequest, "INVALID_FORM",
			"Failed to parse multipart form: "+err.Error())
		return
	}

	f, fileHeader, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_FILE",
			`Form field "file" is required: `+err.Error())
		return
	}
	defer f.Close()

	if strings.TrimSpace(fileHeader.Filename) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_FILE", "filename is required")
		return
	}
	if fileHeader.Size > service.MaxAttachmentSize {
		writeError(w, http.StatusRequestEntityTooLarge, "TOO_LARGE",
			fmt.Sprintf("附件超过 %s 限制", humanBytes(service.MaxAttachmentSize)))
		return
	}

	description := strings.TrimSpace(r.FormValue("description"))
	if len(description) > 500 {
		writeError(w, http.StatusBadRequest, "DESCRIPTION_TOO_LONG",
			"description ≤ 500 字")
		return
	}

	uploadedBy := ""
	// Auth middleware injects the principal onto the context via
	// middleware.CtxUserID; empty string means "auth disabled" (synthetic
	// admin) — Upload still records it as the attribution hint for audits.
	if u := middleware.CurrentUser(r); u != nil {
		uploadedBy = u.UserID
	}

	a, err := h.svc.Upload(reqID, model.AttachmentUpload{
		Filename:    fileHeader.Filename,
		Reader:      f,
		Description: description,
		UploadedBy:  uploadedBy,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "UPLOAD_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

// List — GET /api/requirements/{id}/attachments. Returns all rows for the
// requirement in upload-order; an empty result is "[]", not null.
func (h *AttachmentHandler) List(w http.ResponseWriter, r *http.Request) {
	reqID := requiredRequirementHeader(r)
	atts, err := h.svc.List(reqID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	if atts == nil {
		atts = []model.Attachment{}
	}
	writeJSON(w, http.StatusOK, atts)
}

// Content — GET /api/requirements/{id}/attachments/{att_id}/content. Streams
// the on-disk file with the original filename. Used by the frontend's
// "download" link and (more importantly) for any external consumer that
// wants the bytes verbatim.
func (h *AttachmentHandler) Content(w http.ResponseWriter, r *http.Request) {
	reqID := requiredRequirementHeader(r)
	attID := r.PathValue("att_id")
	a, err := h.svc.Get(attID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "attachment not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "GET_FAILED", err.Error())
		return
	}
	if a.RequirementID != reqID {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "attachment not found")
		return
	}
	abs := h.svc.AbsolutePath(a)
	if _, err := os.Stat(abs); err != nil {
		writeError(w, http.StatusNotFound, "FILE_MISSING",
			"attachment file is missing on disk")
		return
	}

	w.Header().Set("Content-Type", a.MimeType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", a.SizeBytes))
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", a.Filename))
	http.ServeFile(w, r, abs)
}

// UpdateDescription — PUT /api/requirements/{id}/attachments/{att_id}.
// JSON body: {"description": "..."}. Empty string clears the annotation.
// Returns the post-update row.
func (h *AttachmentHandler) UpdateDescription(w http.ResponseWriter, r *http.Request) {
	reqID := requiredRequirementHeader(r)
	attID := r.PathValue("att_id")

	var body struct {
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	body.Description = strings.TrimSpace(body.Description)
	if len(body.Description) > 500 {
		writeError(w, http.StatusBadRequest, "DESCRIPTION_TOO_LONG",
			"description ≤ 500 字")
		return
	}

	// Scope check before update so a stray att_id from another requirement
	// 404s cleanly instead of returning a confusing row from the wrong req.
	if existing, err := h.svc.Get(attID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "attachment not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "GET_FAILED", err.Error())
		return
	} else if existing.RequirementID != reqID {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "attachment not found")
		return
	}

	a, err := h.svc.UpdateDescription(attID, body.Description)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "UPDATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// Delete — DELETE /api/requirements/{id}/attachments/{att_id}. Removes the
// row and its on-disk file. Returns 204 on success, 404 if the row doesn't
// belong to this requirement (or doesn't exist).
func (h *AttachmentHandler) Delete(w http.ResponseWriter, r *http.Request) {
	reqID := requiredRequirementHeader(r)
	attID := r.PathValue("att_id")

	if existing, err := h.svc.Get(attID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "attachment not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "GET_FAILED", err.Error())
		return
	} else if existing.RequirementID != reqID {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "attachment not found")
		return
	}

	if err := h.svc.Delete(attID); err != nil {
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Fingerprint is exposed for tests / debug — wraps the same sha256 + hex
// helper the service uses internally. Not part of the public API.
func Fingerprint(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// filepathBase is a thin wrapper for test injection points. Kept private.
func filepathBase(p string) string { return filepath.Base(p) }

// humanBytes renders a byte count as "2.0 GB" / "256 B" — used to surface the
// attachment cap in error messages without conflating GB and MB.
func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1024*1024*1024))
	}
}
