package handler

import (
	"encoding/json"
	"log"
	"net/http"

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
// top-level "usage" field. The CLI emits these as JSON numbers, which decode
// into float64 via map[string]interface{}; truncate to int. Missing/zero
// fields default to 0.
func extractUsage(evt map[string]interface{}) (in, out, cc, cr int) {
	u, ok := evt["usage"].(map[string]interface{})
	if !ok {
		return
	}
	return toInt(u["input_tokens"]), toInt(u["output_tokens"]),
		toInt(u["cache_creation_input_tokens"]), toInt(u["cache_read_input_tokens"])
}

// toInt coerces a JSON number (float64) or json.Number to int. 0 for anything else.
func toInt(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
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
