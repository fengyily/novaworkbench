package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/util"
)

// pdfUploadMaxMemory is the in-memory threshold for r.ParseMultipartForm —
// uploads larger than this are spilled to a temp file on disk so we don't
// hold a 20 MB PDF in heap.
const pdfUploadMaxMemory = 20 << 20 // 20 MB

// pdfUploadMaxBody is the hard cap on the total request body. Set on
// http.MaxBytesReader so a 1 GB upload is rejected at the body-read layer
// before we ever start spilling through ParseMultipartForm.
const pdfUploadMaxBody = 50 << 20 // 50 MB

type KnowledgeHandler struct {
	svc   *service.KnowledgeService
	llm   *llm.Gateway
	usage *service.UsageService

	// pdfExtract / pdfPolish are the side-effecting steps that the upload
	// pipeline delegates to. They default to util.GetText (pure-Go PDF text
	// extraction) and the LLM Gateway's ConvertPdfTextToMarkdown, but tests
	// override them to avoid standing up a real extractor or LLM channel.
	pdfExtract func(path string) (string, error)
	pdfPolish  func(rawText, file string) (string, *llm.Usage, error)
}

func NewKnowledgeHandler(svc *service.KnowledgeService, llmGateway *llm.Gateway, usageSvc *service.UsageService) *KnowledgeHandler {
	return &KnowledgeHandler{
		svc:        svc,
		llm:        llmGateway,
		usage:      usageSvc,
		pdfExtract: util.GetText,
		pdfPolish:  llmGateway.ConvertPdfTextToMarkdown,
	}
}

func (h *KnowledgeHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	projectID := q.Get("project_id")
	category := q.Get("category")
	sourceType := q.Get("source_type")
	search := q.Get("search")
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	items, total, err := h.svc.List(projectID, category, sourceType, search, limit, offset)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{"items": items, "total": total})
}

func (h *KnowledgeHandler) Get(w http.ResponseWriter, r *http.Request) {
	k, err := h.svc.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, 200, k)
}

func (h *KnowledgeHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req model.CreateKnowledgeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	k, err := h.svc.Create(req)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 201, k)
}

func (h *KnowledgeHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req model.CreateKnowledgeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	k, err := h.svc.Update(r.PathValue("id"), req)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, k)
}

func (h *KnowledgeHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Delete(r.PathValue("id")); err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (h *KnowledgeHandler) ListForReview(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	items, err := h.svc.ListForReview(projectID)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, items)
}

func (h *KnowledgeHandler) BatchReview(w http.ResponseWriter, r *http.Request) {
	var req model.ReviewActionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	if err := h.svc.BatchReview(req); err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (h *KnowledgeHandler) Search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	projectID := r.URL.Query().Get("project_id")

	items, _, err := h.svc.List(projectID, "", "", q, 10, 0)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, items)
}

// GenerateDiagramReq — body of POST /api/knowledge/generate-diagram.
//
// Fields:
//   - ProjectID (required) — knowledge rows are always project-scoped.
//   - Input    (required) — free-form text the LLM will visualize.
//   - Kind     (optional) — flowchart / sequenceDiagram / stateDiagram-v2 /
//     classDiagram / erDiagram / gantt hint. Empty lets the model pick.
//   - PreviewOnly (optional, default false) — true means call the LLM but
//     skip persistence (returns the generated mermaid for the frontend's
//     preview pane). false means LLM + UpsertAIDiagram (the default
//     "generate and save" path).
//   - MermaidOverride (optional) — when non-empty on a non-preview call,
//     bypasses the LLM and persists this user-tweaked mermaid source as-is
//     (title/description default to "（用户自定义）"). Lets the user accept
//     an LLM preview, edit the source by hand, and save the hand-edited
//     version without paying for a second LLM call.
type GenerateDiagramReq struct {
	ProjectID        string `json:"project_id"`
	Input            string `json:"input"`
	Kind             string `json:"kind,omitempty"`
	PreviewOnly      bool   `json:"preview_only,omitempty"`
	MermaidOverride  string `json:"mermaid_override,omitempty"`
}

// GenerateDiagramResp — POST /api/knowledge/generate-diagram response. On a
// preview-only call ID is empty (no persistence); on a save call ID is the
// new/updated knowledge row id. Title / Mermaid / Description / Kind carry the
// final values regardless of which path produced them.
type GenerateDiagramResp struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Kind        string `json:"kind"`
	Mermaid     string `json:"mermaid"`
	Description string `json:"description"`
	CreatedAt   string `json:"created_at"`
}

// GenerateDiagram — POST /api/knowledge/generate-diagram. Modes:
//
//   1. Default (PreviewOnly=false, MermaidOverride=""): call the LLM, then
//      UpsertAIDiagram the result keyed by sha256(project + "|" + input)
//      so a duplicate request refreshes the same row instead of producing
//      parallel copies. Token usage written to token_usage on success + on
//      the failure path (so a wasted LLM call still shows up in billing).
//
//   2. PreviewOnly=true: call the LLM but skip persistence. The frontend
//      uses this to render the preview pane WITHOUT writing a row.
//
//   3. MermaidOverride non-empty (PreviewOnly=false): skip the LLM call
//      entirely; persist the user's hand-edited mermaid source directly.
//      Title and description default to a "(用户自定义)" placeholder so the
//      row is still listable. Hash is computed against (project + "|" +
//      input + "|" + mermaid_override) so override-saved rows don't collide
//      with LLM-saved rows for the same input.
func (h *KnowledgeHandler) GenerateDiagram(w http.ResponseWriter, r *http.Request) {
	var req GenerateDiagramReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	if req.ProjectID == "" {
		writeError(w, 400, "INVALID", "project_id required")
		return
	}
	if req.Input == "" {
		writeError(w, 400, "INVALID", "input required")
		return
	}

	var (
		title, mermaid, kind, desc string
		usage                      *llm.Usage
		llmErr                     error
	)

	if req.MermaidOverride != "" {
		// Hand-edit save path: skip LLM entirely. Title / desc are placeholders
		// so the row can still be listed + browsed.
		mermaid = strings.TrimSpace(req.MermaidOverride)
		title = "（用户自定义架构图）"
		kind = req.Kind
		desc = "由用户在前端编辑器手调后保存，未经过 LLM。"
	} else {
		title, mermaid, kind, desc, usage, llmErr = h.llm.GenerateDiagram(req.Input, req.Kind)
		if llmErr != nil {
			h.recordUsage(req.ProjectID, "knowledge.generate_diagram", usage)
			writeError(w, 500, "LLM", llmErr.Error())
			return
		}
	}

	if req.PreviewOnly {
		// Preview path — LLM ran (or override pre-supplied), no DB write.
		if req.MermaidOverride == "" {
			h.recordUsage(req.ProjectID, "knowledge.generate_diagram_preview", usage)
		}
		writeJSON(w, 200, GenerateDiagramResp{
			Title:       title,
			Kind:        kind,
			Mermaid:     mermaid,
			Description: desc,
		})
		return
	}

	// Save path — write to knowledge table. The hash includes mermaid_override
	// so a hand-edited version doesn't collide with an LLM-generated version.
	hashInput := req.ProjectID + "|" + req.Input + "|" + req.MermaidOverride
	hash := sha256.Sum256([]byte(hashInput))
	sourceRef := "ai:" + hex.EncodeToString(hash[:])[:16]

	k, err := h.svc.UpsertAIDiagram(req.ProjectID, sourceRef, title, mermaid, kind, desc)
	if err != nil {
		if req.MermaidOverride == "" {
			h.recordUsage(req.ProjectID, "knowledge.generate_diagram", usage)
		}
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	if req.MermaidOverride == "" {
		h.recordUsage(req.ProjectID, "knowledge.generate_diagram", usage)
	}
	writeJSON(w, 200, GenerateDiagramResp{
		ID:          k.ID,
		Title:       k.Title,
		Kind:        k.Category,
		Mermaid:     mermaid,
		Description: desc,
		CreatedAt:   k.CreatedAt.Format(time.RFC3339),
	})
}

// ExtractDiagramsReq — POST /api/knowledge/extract-diagrams body. Carries the
// mermaid source blocks extracted on the frontend (via DOM walk over the
// rendered design-doc panel); the backend writes each as its own knowledge
// row with source_ref = "plan:<reqID>:<index>" so a subsequent extraction
// UPDATEs in place instead of multiplying rows.
type ExtractDiagramsReq struct {
	ProjectID     string              `json:"project_id"`
	RequirementID string              `json:"requirement_id"`
	Blocks        []ExtractDiagramBock `json:"blocks"`
}

type ExtractDiagramBock struct {
	Mermaid string `json:"mermaid"`
	Title   string `json:"title,omitempty"`
}

// ExtractDiagrams — POST /api/knowledge/extract-diagrams. Persists each
// Mermaid block the user explicitly selected on the design-doc panel as its
// own knowledge row (no LLM call — the model already emitted the blocks when
// generating the plan). Returns the new ids so the frontend can refresh.
//
// Each block becomes a fresh INSERT (not an upsert) keyed by
// source_ref="plan:<reqID>:<index>" — same source_ref across calls so the
// knowledge page can dedupe via filter, but a re-extract creates a new row
// (the user may have tweaked titles). The service.Create handles the INSERT
// + id mint; we just pass a complete CreateKnowledgeReq with category /
// source_type already set to the AI-diagram values.
func (h *KnowledgeHandler) ExtractDiagrams(w http.ResponseWriter, r *http.Request) {
	var req ExtractDiagramsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	if req.ProjectID == "" || req.RequirementID == "" {
		writeError(w, 400, "INVALID", "project_id and requirement_id required")
		return
	}
	if len(req.Blocks) == 0 {
		writeJSON(w, 200, map[string]any{"ids": []string{}})
		return
	}
	ids := make([]string, 0, len(req.Blocks))
	for i, blk := range req.Blocks {
		if strings.TrimSpace(blk.Mermaid) == "" {
			continue
		}
		title := strings.TrimSpace(blk.Title)
		if title == "" {
			title = fmt.Sprintf("架构图 #%d", i+1)
		}
		content := buildAIDiagramContentForHandler(blk.Mermaid, "from plan", "")
		created, err := h.svc.Create(model.CreateKnowledgeReq{
			ProjectID:  req.ProjectID,
			Title:      title,
			Content:    content,
			Category:   service.CategoryTechDesign,
			SourceType: service.SourceTypeAIDiagram,
			SourceRef:  fmt.Sprintf("plan:%s:%d", req.RequirementID, i),
		})
		if err != nil {
			writeError(w, 500, "INTERNAL", err.Error())
			return
		}
		ids = append(ids, created.ID)
	}
	writeJSON(w, 200, map[string]any{"ids": ids})
}

// buildAIDiagramContentForHandler is the handler-side twin of
// service.buildAIDiagramContent (kept private to each package; same body).
// Used by ExtractDiagrams to assemble knowledge.content for plan-extracted
// blocks. Wrapped in fenced ```mermaid``` + an optional caption.
func buildAIDiagramContentForHandler(mermaid, kind, description string) string {
	var parts []string
	mermaid = strings.TrimSpace(mermaid)
	if mermaid != "" {
		parts = append(parts, "```mermaid\n"+mermaid+"\n```")
	}
	if description != "" {
		if kind != "" {
			parts = append(parts, fmt.Sprintf("_%s · %s_", kind, description))
		} else {
			parts = append(parts, "_"+description+"_")
		}
	}
	return strings.Join(parts, "\n\n")
}

// recordUsage — write a token_usage row when the LLM reported token counts.
// Best-effort: a record failure must never turn a successful diagram
// generation into an error response. projectID lets the operator see the cost
// in the project's usage dashboard; requirementID is empty since diagram
// generation is project-scoped, not tied to a specific requirement.
// Mirrors the pattern in handler/report_archive.go:179-193.
func (h *KnowledgeHandler) recordUsage(projectID, step string, usage *llm.Usage) {
	if h.usage == nil || usage == nil {
		return
	}
	u := model.TokenUsage{
		ProjectID:    projectID,
		Step:         step,
		Model:        usage.Model,
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
	}
	if rerr := h.usage.Record(u); rerr != nil {
		fmt.Printf("[knowledge.generate-diagram] record usage failed: %v (ignored)\n", rerr)
	}
}

// UploadPdf — POST /api/knowledge/upload-pdf (multipart/form-data).
//
// Companion of GenerateDiagram (LLM-driven, no file) and Create (JSON
// metadata only) on the knowledge-write surface: where Create takes a
// {title, content} body and GenerateDiagram asks the LLM to mint a Mermaid
// diagram, UploadPdf takes a binary PDF and walks the full PDF → Markdown
// pipeline. Body shape:
//
//	field name "file"    — required, the PDF blob
//	field name "project_id" — optional in form, falls back to ?project_id=
//	                            query string; required (the knowledge row
//	                            must be project-scoped to satisfy the FK).
//
// Validation is layered: http.MaxBytesReader caps the request body up front
// (returns 413 TOO_LARGE on overflow), ParseMultipartForm then spills the
// in-memory threshold to a temp file, and the file type is double-checked
// (Content-Type prefix + .pdf extension) before extraction. On success
// returns 201 with the same KnowledgeItem shape as Create; on failure
// returns 400 / 413 / 500 with the project's standard writeError envelope.
//
// Token usage from the polish step is recorded via recordUsage so a PDF
// upload shows up in the project's token_usage dashboard alongside
// diagram/report-archive calls.
func (h *KnowledgeHandler) UploadPdf(w http.ResponseWriter, r *http.Request) {
	// http.MaxBytesReader must wrap the body BEFORE ParseMultipartForm so
	// oversize requests are rejected at the body-read layer (it surfaces as
	// a *http.MaxBytesError on the next read). Without this cap, a 1 GB
	// upload would happily spill through ParseMultipartForm before we
	// realized anything was wrong.
	r.Body = http.MaxBytesReader(w, r.Body, pdfUploadMaxBody)

	if err := r.ParseMultipartForm(pdfUploadMaxMemory); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "TOO_LARGE",
				fmt.Sprintf("PDF 上传超过 %d MB 限制", pdfUploadMaxBody>>20))
			return
		}
		writeError(w, http.StatusBadRequest, "INVALID_FORM",
			"Failed to parse multipart form: "+err.Error())
		return
	}

	// project_id is required; accept either form field or ?project_id= query
	// for clients that find query-string simpler to thread.
	projectID := strings.TrimSpace(r.FormValue("project_id"))
	if projectID == "" {
		projectID = strings.TrimSpace(r.URL.Query().Get("project_id"))
	}
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_PROJECT_ID", "project_id is required")
		return
	}

	f, fileHeader, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_FILE", "Form field \"file\" is required: "+err.Error())
		return
	}
	defer f.Close()
	if fileHeader.Size > pdfUploadMaxBody {
		writeError(w, http.StatusRequestEntityTooLarge, "TOO_LARGE",
			fmt.Sprintf("PDF 上传超过 %d MB 限制", pdfUploadMaxBody>>20))
		return
	}

	// Type check: Content-Type prefix + filename extension. Browsers always
	// send application/pdf for picker-selected PDFs, but we still require
	// the .pdf extension as belt-and-suspenders since the Content-Type can
	// be spoofed.
	ct := fileHeader.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/pdf") {
		writeError(w, http.StatusBadRequest, "INVALID_FILE_TYPE",
			fmt.Sprintf("Only PDF files are accepted, got Content-Type=%q", ct))
		return
	}
	if strings.ToLower(filepath.Ext(fileHeader.Filename)) != ".pdf" {
		writeError(w, http.StatusBadRequest, "INVALID_FILE_TYPE",
			fmt.Sprintf("Only .pdf files are accepted, got filename=%q", fileHeader.Filename))
		return
	}

	// Spill the upload to a temp file so the extractor can read it via
	// os.Open (rather than a multipart.File) — util.GetText expects a real
	// filesystem path. MkdirTemp root defaults to os.TempDir() which is
	// already per-user on every supported platform. Cleanup is unconditional
	// via defer so a panic in the pipeline still leaves no orphans.
	tmpFile, err := os.CreateTemp("", "pdf-upload-*.pdf")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "Failed to create temp file: "+err.Error())
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmpFile, f); err != nil {
		f.Close()
		tmpFile.Close()
		// io.Copy against a MaxBytesReader-wrapped body surfaces an
		// *http.MaxBytesError when the temp spill itself exceeded the cap
		// (e.g. multipart overhead pushed it over). Map that to TOO_LARGE
		// instead of a generic 500.
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "TOO_LARGE",
				fmt.Sprintf("PDF 上传超过 %d MB 限制", pdfUploadMaxBody>>20))
			return
		}
		writeError(w, http.StatusInternalServerError, "INVALID_FILE", "Failed to read uploaded file: "+err.Error())
		return
	}
	if err := tmpFile.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "Failed to close temp file: "+err.Error())
		return
	}

	// Pull the two pipeline steps off the handler so the same call site
	// works in production (util.GetText + LLM gateway) and in tests (mocked
	// stubs). The defaults are wired in NewKnowledgeHandler.
	extract := h.pdfExtract
	polish := h.pdfPolish
	if extract == nil {
		extract = util.GetText
	}
	if polish == nil {
		polish = h.llm.ConvertPdfTextToMarkdown
	}

	k, usage, err := h.svc.CreateFromPdf(
		service.CreateKnowledgeFromPdfReq{
			ProjectID:  projectID,
			SourceFile: fileHeader.Filename,
			FilePath:   tmpPath,
		},
		extract,
		polish,
	)
	if err != nil {
		// Empty-PDF (zero bytes / blank extraction) is a client problem,
		// not a server error — surface as 400 so the frontend can show a
		// actionable toast instead of a generic failure.
		if errors.Is(err, util.ErrEmptyPdfInput) || errors.Is(err, llm.ErrEmptyInput) {
			writeError(w, http.StatusBadRequest, "INVALID_FILE",
				"PDF 内容为空或无法提取文本")
			return
		}
		// Extract-failure often manifests as "the file is not a valid PDF"
		// (e.g. someone renamed a .exe to .pdf) — keep the legacy 500
		// since this is rarely recoverable in production and the frontend
		// just needs a human message.
		writeError(w, http.StatusInternalServerError, "EXTRACT_FAILED",
			"PDF 解析失败: "+err.Error())
		return
	}

	if usage != nil {
		h.recordUsage(projectID, "knowledge.upload_pdf", usage)
	}
	writeJSON(w, http.StatusCreated, k)
}
