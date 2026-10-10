package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

type KnowledgeHandler struct {
	svc   *service.KnowledgeService
	llm   *llm.Gateway
	usage *service.UsageService
}

func NewKnowledgeHandler(svc *service.KnowledgeService, llmGateway *llm.Gateway, usageSvc *service.UsageService) *KnowledgeHandler {
	return &KnowledgeHandler{svc: svc, llm: llmGateway, usage: usageSvc}
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
