package handler

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// usageRecorder is the narrow dependency the streaming handlers need to record
// a token-usage row. *service.UsageService satisfies it. Implementations must
// be best-effort — a recording error must never break the claude turn.
type usageRecorder interface {
	Record(model.TokenUsage) error
}

// usageCtx carries the identity of one claude invocation so its result event
// can be recorded as a token_usage row. Counts are filled from the result
// event's usage field at record time. ClaudeConfigID is the platform whose
// config was active when the request ran; Currency is that config's currency
// snapshot — both let cost be recomputed from the config's current prices.
// Pass nil to skip recording (e.g. when no UsageService is wired).
//
// Summary is a short Chinese label for what produced this row (e.g. the
// truncated text of a 追加调整 request). It is persisted into the meta JSON
// under the "summary" key so the requirement-detail token table can show a
// collapsible per-row description per invocation. Empty stays empty.
type usageCtx struct {
	Rec            usageRecorder
	RequirementID  string
	ProjectID      string
	JobID          string
	Step           string
	Model          string
	ClaudeConfigID string
	Currency       string
	Meta           string
	Summary        string
	// PersistSnapshot, when non-nil, is called by runClaudeStream at the same
	// point it emits the `usage` SSE event (end of a claude turn) to write the
	// session's latest token-usage snapshot into requirements.usage_snapshots.
	// The closure receives (sessionKey, snapshotJSON) — sessionKey is the
	// wizard session the snapshot belongs to (analyst_chat / architect_design
	// / coding), already mapped from Step by usageCtxFor via snapshotStep.
	// nil means "don't persist" (e.g. compress_* turns, where the snapshot
	// would describe the summarize prompt rather than the session's real fill
	// and the session is about to be cleared anyway). Best-effort: the closure
	// must swallow its own errors so a DB hiccup never breaks the claude turn.
	PersistSnapshot func(sessionKey, snapshotJSON string)
}

// summaryMetaMaxLen caps the persisted summary so a long 追加调整 prompt
// doesn't bloat the token_usage row. 200 chars is enough to recognize the
// adjustment in the log; longer text is elided with "…".
const summaryMetaMaxLen = 200

// buildSummaryMeta returns the JSON to drop into the token_usage.meta column.
// If extra already contains a "summary" key, it is overwritten with the
// supplied summary (keeping any other caller-provided keys intact). If extra
// is empty/non-JSON, the result is a single-key {"summary":…} object. The
// summary is truncated to summaryMetaMaxLen runes.
func buildSummaryMeta(extra, summary string) string {
	if summary == "" {
		return extra
	}
	truncated := summary
	if r := []rune(summary); len(r) > summaryMetaMaxLen {
		truncated = string(r[:summaryMetaMaxLen]) + "…"
	}
	out := map[string]any{}
	if extra != "" {
		// Preserve caller-supplied fields (e.g. {"doc_type":"design"}); on parse
		// failure fall through and overwrite — the caller is the only writer.
		_ = json.Unmarshal([]byte(extra), &out)
	}
	out["summary"] = truncated
	b, err := json.Marshal(out)
	if err != nil {
		return extra
	}
	return string(b)
}

// extractUsage reads the four token counts from a stream-json result event's
// top-level "usage" field. Thin wrapper around llm.ParseStreamUsage so the
// token-counting logic lives in one place (the gateway package). Missing/zero
// fields default to 0.
func extractUsage(evt map[string]interface{}) (in, out, cc, cr int) {
	in, out, cc, cr, _ = llm.ParseStreamUsage(evt)
	return
}

// recordFrom is called at a result event. It fills the counts from the event
// and persists the row, swallowing ALL errors (only logs) so a recording
// failure can never abort the claude stream. A result with zero usage (e.g.
// an interrupted run that never reached the API) is not recorded.
func (c *usageCtx) recordFrom(evt map[string]interface{}) {
	if c == nil || c.Rec == nil {
		return
	}
	in, out, cc, cr := extractUsage(evt)
	if in == 0 && out == 0 && cc == 0 && cr == 0 {
		return
	}
	u := model.TokenUsage{
		RequirementID:       c.RequirementID,
		ProjectID:           c.ProjectID,
		JobID:               c.JobID,
		Step:                c.Step,
		Model:               c.Model,
		ClaudeConfigID:      c.ClaudeConfigID,
		Currency:            c.Currency,
		Meta:                buildSummaryMeta(c.Meta, c.Summary),
		InputTokens:         in,
		OutputTokens:        out,
		CacheCreationTokens: cc,
		CacheReadTokens:     cr,
	}
	if err := c.Rec.Record(u); err != nil {
		log.Printf("[usage] record %s for %s failed: %v (ignored)", c.Step, c.RequirementID, err)
	}
}

// UsageHandler exposes the token-usage aggregation endpoints.
type UsageHandler struct {
	svc *service.UsageService
}

func NewUsageHandler(svc *service.UsageService) *UsageHandler {
	return &UsageHandler{svc: svc}
}

// Rows returns one token_usage row per invocation for a requirement, optionally
// filtered by step. The requirement-detail UI uses this to show every
// individual 追加调整 (model, tokens, cost, time, summary) — the per-step
// rollup hides which model each adjustment actually used. Empty step returns
// all rows for the requirement.
//
// GET /api/usage/requirement/{id}/rows?step=adjust_coding
func (h *UsageHandler) Rows(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "missing requirement id")
		return
	}
	step := r.URL.Query().Get("step")
	rows, err := h.svc.RowsByStep(id, step)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "USAGE_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// Requirement returns per-step + total token usage for one requirement.
// GET /api/usage/requirement/{id}
func (h *UsageHandler) Requirement(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "missing requirement id")
		return
	}
	summary, err := h.svc.RequirementSummary(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "USAGE_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// ByRequirement returns per-requirement token totals for a project (excludes
// review rows). GET /api/usage/by-requirement?project_id=
func (h *UsageHandler) ByRequirement(w http.ResponseWriter, r *http.Request) {
	projID := r.URL.Query().Get("project_id")
	if projID == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "missing project_id")
		return
	}
	rows, err := h.svc.RequirementsByProject(projID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "USAGE_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// Project returns the project total (excl review), per-requirement totals,
// and the review breakdown. GET /api/usage/project/{id}
func (h *UsageHandler) Project(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "missing project id")
		return
	}
	summary, err := h.svc.ProjectSummary(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "USAGE_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, summary)
}
