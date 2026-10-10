package handler

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

type RequirementHandler struct {
	svc      *service.RequirementService
	llm      *llm.Gateway
	jobs     *store.JobStore
	usageSvc usageRecorder
	// wizardH / schedSvc power the optional 启动计划 (launch spec) on Create —
	// dispatchLaunch (requirement_launch.go) needs both to dispatch an
	// immediate wizard run or to insert a scheduled_tasks row.
	wizardH *WizardHandler
	schedSvc *service.ScheduledTaskService
}

func NewRequirementHandler(svc *service.RequirementService, llmGateway *llm.Gateway, jobs *store.JobStore, usageSvc usageRecorder, wizardH *WizardHandler, schedSvc *service.ScheduledTaskService) *RequirementHandler {
	return &RequirementHandler{svc: svc, llm: llmGateway, jobs: jobs, usageSvc: usageSvc, wizardH: wizardH, schedSvc: schedSvc}
}

func (h *RequirementHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	items, err := h.svc.List(q.Get("project_id"), q.Get("status"), q.Get("priority"), q.Get("kind"), q.Get("mark"))
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, items)
}

func (h *RequirementHandler) Get(w http.ResponseWriter, r *http.Request) {
	item, err := h.svc.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "NOT_FOUND", err.Error())
		return
	}
	h.healStaleJobs(item)
	writeJSON(w, 200, item)
}

// healStaleJobs clears job-id pointers on the requirement that reference jobs
// which are no longer live (evicted from the in-memory ring buffer, or finished
// — the goroutine normally clears these itself on every terminal path, but a
// server restart / crash between the DB write of the id and the clearing call
// leaves a stale pointer that wedges the frontend: the architect-design panel
// gates its "⏳ …" spinner on !!req.design_job_id and hides the retry button
// behind !req.design_job_id, so a stale id = a perpetual spinner with no way
// out. Reconciling against the JobStore on every Get self-heals it: the next
// page load / refresh drops the stale id and the UI recovers on its own.
func (h *RequirementHandler) healStaleJobs(req *model.Requirement) {
	if h.jobs == nil {
		return
	}
	type pending struct {
		val    string
		clear  func(id, jobID string) error
		field  *string
	}
	checks := []pending{
		{req.DesignJobID, h.svc.UpdateDesignJob, &req.DesignJobID},
		{req.AnalysisJobID, h.svc.UpdateAnalysisJob, &req.AnalysisJobID},
		{req.ApplyJobID, h.svc.UpdateApplyJob, &req.ApplyJobID},
		{req.WikiJobID, h.svc.UpdateWikiJob, &req.WikiJobID},
	}
	for _, c := range checks {
		if c.val == "" {
			continue
		}
		if h.jobs.Live(c.val) {
			continue
		}
		if err := c.clear(req.ID, ""); err != nil {
			log.Printf("[requirement] failed to clear stale job id %s for %s: %v", c.val, req.ID, err)
			continue
		}
		*c.field = ""
	}
}

func (h *RequirementHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req model.CreateRequirementReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	if req.ProjectID == "" || req.Description == "" {
		writeError(w, 400, "INVALID", "project_id and description are required")
		return
	}
	// Title is no longer entered by the user — distill it from the requirement
	// content via the LLM. Fall back to the first line of the content if the
	// LLM is unavailable (e.g. claude CLI not installed) so creation never fails.
	//
	// skip_organize (UI default true): the caller can opt out of the LLM-
	// organized description pass entirely. When set, the raw description is
	// stored as-is and a fallback title (first line) is used; no LLM round-
	// trip and no token_usage row is recorded. Older clients that don't send
	// the field keep the previous behavior (run the organizer).
	var httpUsage *llm.Usage
	if req.Title == "" {
		skipOrganize := req.SkipOrganize != nil && *req.SkipOrganize
		if skipOrganize {
			req.Title = fallbackTitle(req.Description)
		} else {
			// Reorganize the raw, free-form content into structured Markdown AND
			// distill a title in a single LLM round, so the title and body stay
			// consistent and the content is transmitted once. Each half falls back
			// independently on failure — creation must not fail just because the
			// formatter is unavailable.
			markdown, title, usage, err := h.llm.GenerateDescriptionAndTitle(req.Description, req.Kind)
			switch {
			case err != nil:
				log.Printf("[requirement] GenerateDescriptionAndTitle failed: %v — using raw content and fallback title", err)
				req.Title = fallbackTitle(req.Description)
			case markdown == "" || title == "":
				// Shouldn't happen (the gateway returns an error in these cases),
				// but guard against a partial result by filling the missing half.
				if markdown != "" {
					req.Description = markdown
				}
				if title != "" {
					req.Title = title
				} else {
					req.Title = fallbackTitle(req.Description)
				}
			default:
				req.Description = markdown
				req.Title = title
			}
			// usage may be nil (channel unconfigured / gateway omitted usage);
			// keep it so the token row can be recorded after Create mints the id.
			httpUsage = usage
		}
	}
	item, err := h.svc.Create(req)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	// Record the requirement-creation (title distillation) token usage. Only
	// when the HTTP LLM channel was used and reported a usage object. Best-effort.
	if httpUsage != nil && h.usageSvc != nil {
		u := model.TokenUsage{
			RequirementID: item.ID,
			ProjectID:     req.ProjectID,
			Step:          "requirement_create",
			Model:         httpUsage.Model,
			InputTokens:   httpUsage.PromptTokens,
			OutputTokens:  httpUsage.CompletionTokens,
		}
		if rerr := h.usageSvc.Record(u); rerr != nil {
			log.Printf("[requirement] record usage for %s failed: %v (ignored)", item.ID, rerr)
		}
	}
	// Optional launch spec — when the caller attaches one to Create, dispatch
	// the chosen execution plan (immediate wizard run / scheduled task) before
	// serializing the response. dispatchLaunch handles its own validation
	// (resolveLaunchTaskType: full+immediate → 400 LAUNCH_NEEDS_ANALYSIS;
	// idea → 400 LAUNCH_NOT_ALLOWED) and writes schedule id onto item.LaunchScheduleID.
	//
	// Failure handling: when dispatchLaunch returns an *apiFailure the
	// requirement row has already been inserted — rolling back would discard
	// the user's description. We surface the failure on the response as
	// item.LaunchError and still return 201, letting the frontend prompt the
	// user to start the run manually from the detail page (which already has
	// full manual start controls). No compensating delete.
	if req.Launch != nil {
		mode, scheduleID, dispatchErr := dispatchLaunch(item, req.Launch, h.wizardH, h.schedSvc)
		if dispatchErr != nil {
			item.LaunchError = launchErrorMessage(dispatchErr)
			log.Printf("[requirement] launch dispatch for %s failed: %s (requirement preserved)", item.ID, dispatchErr.Msg)
		} else {
			item.LaunchMode = mode
			item.LaunchScheduleID = scheduleID
		}
	}
	writeJSON(w, 201, item)
}

// launchErrorMessage flattens an *apiFailure into a single user-facing
// message string (Code + Msg). The frontend's launch_failed copy
// surfaces this verbatim — keeping the code prefix makes cross-referencing
// translation keys straightforward.
func launchErrorMessage(af *apiFailure) string {
	if af == nil {
		return ""
	}
	if af.Code == "" {
		return af.Msg
	}
	return af.Code + ": " + af.Msg
}

// fallbackTitle derives a short title from the requirement content when the LLM
// is unavailable: the first non-empty line, capped at 60 runes. The title is
// only a display label (the full intent lives in the description, which the
// analyst chat reads from the DB), so we keep it readable rather than truncating
// hard at 20 runes.
func fallbackTitle(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return "新需求"
	}
	first := content
	if idx := strings.IndexByte(content, '\n'); idx >= 0 {
		first = strings.TrimSpace(content[:idx])
	}
	r := []rune(first)
	if len(r) > 60 {
		first = string(r[:60]) + "..."
	}
	return first
}

func (h *RequirementHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req model.CreateRequirementReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	item, err := h.svc.Update(r.PathValue("id"), req)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, item)
}

// UpdateKind promotes a finished Issue or Idea into a Requirement (one-way
// upgrade; see service.UpdateKind for the validation rules). The status is
// intentionally untouched — the caller can follow up with PATCH .../status if
// they want to re-open the row into "draft" for a fresh analyst pass.
func (h *RequirementHandler) UpdateKind(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	item, err := h.svc.UpdateKind(r.PathValue("id"), body.Kind)
	if err != nil {
		writeError(w, 400, "UPDATE_KIND_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, item)
}

// UpdateTags replaces the requirement's tag list with the caller-supplied
// values. Body shape: {"tags": ["阻塞", "v2", ...]}. Tags are normalized
// server-side (trim / dedupe / length- and count-cap) so a malformed
// payload never reaches the JSON column — see service.RequirementService.
// UpdateTags for the cap values.
func (h *RequirementHandler) UpdateTags(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	item, err := h.svc.UpdateTags(r.PathValue("id"), body.Tags)
	if err != nil {
		writeError(w, 400, "UPDATE_TAGS_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, item)
}

// UpdateMarks replaces the requirement's preset mark list with the
// caller-supplied values. Body shape: {"marks": ["important", "follow_up",
// ...]}. Marks are normalized server-side against MarkWhitelist (unknown
// values silently dropped) + count-capped (max 5) — see service.
// RequirementService.UpdateMarks / normalizeMarks for the rules. Unlike
// tags, marks directly affect list sort order (rows with any mark float
// above unmarked rows), so the whitelist is enforced even for direct
// callers that bypass the UI.
func (h *RequirementHandler) UpdateMarks(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Marks []string `json:"marks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	item, err := h.svc.UpdateMarks(r.PathValue("id"), body.Marks)
	if err != nil {
		writeError(w, 400, "UPDATE_MARKS_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, item)
}

// Close force-closes a requirement from any non-archived state. Unlike
// PATCH /status (which is gated by validTransitions and only reachable from
// `developing` → `done` via the natural "开发完成" gate), Close lets the
// user cut the pipeline short from any active stage. Body shape:
// {"reason": "..."} (optional, capped server-side). Returns the refreshed
// requirement; archived rows are rejected with 400.
func (h *RequirementHandler) Close(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	// Body is optional — decode best-effort so an empty body lands as
	// reason="".
	_ = json.NewDecoder(r.Body).Decode(&body)
	item, err := h.svc.Close(r.PathValue("id"), body.Reason)
	if err != nil {
		writeError(w, 400, "CLOSE_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, item)
}

func (h *RequirementHandler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	var req model.UpdateStatusReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	item, err := h.svc.UpdateStatus(r.PathValue("id"), req.Status)
	if err != nil {
		writeError(w, 400, "INVALID_STATUS", err.Error())
		return
	}
	writeJSON(w, 200, item)
}

// Calendar serves the slim requirements slice the calendar view needs for
// the visible [from, to) window. Defaults: from = first day of the current
// month, to = first day of the next month when the caller omits the params
// (handler-side default; the frontend always sends explicit bounds). Time
// strings are accepted in either RFC3339 or "YYYY-MM-DD" — the service
// always normalizes to day boundaries so a tiny calendar tick never lands
// off-grid.
func (h *RequirementHandler) Calendar(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	to := from.AddDate(0, 1, 0)
	if v := strings.TrimSpace(r.URL.Query().Get("from")); v != "" {
		if t, err := parseBoundary(v, from); err == nil {
			from = t
		}
	}
	if v := strings.TrimSpace(r.URL.Query().Get("to")); v != "" {
		if t, err := parseBoundary(v, to); err == nil {
			to = t
		}
	}
	if !to.After(from) {
		writeError(w, 400, "BAD_RANGE", "to must be after from")
		return
	}
	q := r.URL.Query()
	items, err := h.svc.Calendar(from, to, q.Get("project_id"), q.Get("kind"))
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, items)
}

// UpdateSchedule writes the calendar scheduling fields. Either field may be
// null (or omitted) to clear that column; both-null resets the row to the
// created_at anchor. end < start is rejected with INVALID_SCHEDULE.
func (h *RequirementHandler) UpdateSchedule(w http.ResponseWriter, r *http.Request) {
	var req model.UpdateScheduleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	if err := h.svc.UpdateSchedule(r.PathValue("id"), req.PlannedStartAt, req.PlannedEndAt); err != nil {
		writeError(w, 400, "INVALID_SCHEDULE", err.Error())
		return
	}
	item, err := h.svc.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, item)
}

// parseBoundary accepts "YYYY-MM-DD" (day-boundary, expanded to 00:00 local)
// or any RFC3339 timestamp the time package can parse. Returns the parsed
// time and a sentinel error so the handler falls back to its default without
// failing the whole request.
func parseBoundary(s string, fallback time.Time) (time.Time, error) {
	if len(s) == 10 && s[4] == '-' && s[7] == '-' {
		t, err := time.ParseInLocation("2006-01-02", s, time.Local)
		if err != nil {
			return fallback, err
		}
		return t, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return fallback, err
	}
	return t, nil
}

func (h *RequirementHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Delete(r.PathValue("id")); err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// Archive turns a finished ("done") requirement into a project knowledge-base
// entry (final requirement + design docs). Returns the created/updated
// knowledge row. The requirement status moves to "archived".
func (h *RequirementHandler) Archive(w http.ResponseWriter, r *http.Request) {
	kb, err := h.svc.Archive(r.PathValue("id"))
	if err != nil {
		writeError(w, 400, "ARCHIVE_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, kb)
}

// Unarchive reverses Archive: status returns to "done" and the knowledge entry
// produced by archiving is removed.
func (h *RequirementHandler) Unarchive(w http.ResponseWriter, r *http.Request) {
	item, err := h.svc.Unarchive(r.PathValue("id"))
	if err != nil {
		writeError(w, 400, "UNARCHIVE_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, item)
}

// WikiArchive turns a wiki-kind requirement's Markdown body into a
// knowledge-base entry. The requirement status moves to "archived"; the
// knowledge row is created (or updated) with source_type="wiki_doc" and
// category="wiki_doc" so the /knowledge page can list wiki entries
// distinctly from completed-requirement entries. Rejects non-wiki rows
// and rows without a generated wiki_docs body.
func (h *RequirementHandler) WikiArchive(w http.ResponseWriter, r *http.Request) {
	kb, err := h.svc.WikiArchive(r.PathValue("id"))
	if err != nil {
		code := "WIKI_ARCHIVE_FAILED"
		if err.Error() == "only wiki-kind requirements can be wiki-archived (current kind: "+service.KindIssue+")" ||
			err.Error() == "only wiki-kind requirements can be wiki-archived (current kind: "+service.KindRequirement+")" ||
			err.Error() == "only wiki-kind requirements can be wiki-archived (current kind: "+service.KindIdea+")" {
			code = "WIKI_KIND_REQUIRED"
		}
		writeError(w, 400, code, err.Error())
		return
	}
	writeJSON(w, 200, kb)
}

// WikiUnarchive reverses WikiArchive: status returns to "designed" and
// the wiki knowledge entry is removed. The wiki_docs body is preserved
// on the requirement row, so a re-archive produces an identical
// knowledge entry.
func (h *RequirementHandler) WikiUnarchive(w http.ResponseWriter, r *http.Request) {
	item, err := h.svc.WikiUnarchive(r.PathValue("id"))
	if err != nil {
		writeError(w, 400, "WIKI_UNARCHIVE_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, item)
}

func (h *RequirementHandler) GetChatHistory(w http.ResponseWriter, r *http.Request) {
	messages, err := h.svc.GetRefinementChat(r.PathValue("id"))
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, messages)
}

func (h *RequirementHandler) SaveChatHistory(w http.ResponseWriter, r *http.Request) {
	var req struct{ Messages string `json:"messages"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	if err := h.svc.SaveRefinementChat(r.PathValue("id"), req.Messages); err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// GetCodingChat returns the persisted 追加调整 (developer-chat) history for
// a requirement, so the chat panel can rehydrate after a refresh. The
// returned JSON is always a non-null array string.
func (h *RequirementHandler) GetCodingChat(w http.ResponseWriter, r *http.Request) {
	messages, err := h.svc.GetCodingChat(r.PathValue("id"))
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, messages)
}

// SaveCodingChat upserts the 追加调整 chat history for a requirement. The
// frontend calls this after each completed turn so the conversation survives
// a page refresh; the message array is the full developer-chat history.
func (h *RequirementHandler) SaveCodingChat(w http.ResponseWriter, r *http.Request) {
	var req struct{ Messages string `json:"messages"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}
	if err := h.svc.SaveCodingChat(r.PathValue("id"), req.Messages); err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// ClearAnalysisSession clears the stored claude analyst session id for a
// requirement, so the next analyst-chat turn mints a fresh conversation instead
// of --resume-ing a broken or over-long one. The chat's "clear" action calls
// this so the user can recover from a wedged session without leaving the chat.
// The displayed chat messages are cleared separately by the caller.
func (h *RequirementHandler) ClearAnalysisSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, 400, "INVALID", "missing requirement id")
		return
	}
	if err := h.svc.UpdateAnalysisSession(id, ""); err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// promoteFromIdeaReq is the optional body for POST /api/requirements/{id}/promote.
// Empty body (no Design) preserves the legacy behavior — only the new requirement
// is created. With Design set, the architect stage is dispatched immediately after
// the row is INSERTed (terminal state: designed).
//
// Design reuses the wizard_immediate.go designCodingImmediateReq struct verbatim —
// the Coding*/Branch*/SplitTasks/AutoPushPR/DevMode/SyncMode fields are accepted but
// ignored on this path (the frontend TS type PromoteDesignConfig only exposes the
// four design_* / read_knowledge fields to users).
type promoteFromIdeaReq struct {
	Design *designCodingImmediateReq `json:"design,omitempty"`
}

// PromoteFromIdea summarizes an idea's accumulated discussion (description +
// analyst-accumulated acceptance_criteria + multi-turn chat) into a brand-new
// requirement row, leaving the original idea intact (its kind stays "idea").
// The new row's source_requirement_id points back to the idea for trace.
//
// Returns 422 when the LLM judges the discussion didn't converge into a
// concrete feature (returns the empty-markdown sentinel) — the frontend turns
// this into "讨论还没有达成共识，请继续完善".
//
// Optional body {design: designCodingImmediateReq}: when Design is set, the
// architect stage is dispatched immediately after the new requirement row is
// INSERTed. The terminal state is "designed" (no chained coding); on dispatch
// failure the error is surfaced via item.LaunchError and the response is still
// 201 — the user can manually retry design from the detail page.
func (h *RequirementHandler) PromoteFromIdea(w http.ResponseWriter, r *http.Request) {
	if h.llm == nil {
		writeError(w, 500, "INTERNAL", "llm gateway not configured")
		return
	}
	id := r.PathValue("id")

	// Body parse is best-effort: an empty/missing body falls back to legacy behavior
	// (no design dispatch). EOF from r.Body == nil is swallowed via _ =.
	var body promoteFromIdeaReq
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	item, err := h.svc.PromoteFromIdea(id, h.llm)
	if err != nil {
		if err.Error() == "discussion did not converge into a concrete requirement" {
			writeError(w, 422, "NOT_CONVERGED", err.Error())
			return
		}
		writeError(w, 500, "PROMOTE_FAILED", err.Error())
		return
	}

	// Optional design dispatch: only when the caller explicitly opts in
	// AND the wizard handler is wired. Mirror Create's launchErrorMessage
	// pattern: any failure inside design dispatch becomes LaunchError on
	// the returned Requirement — we still write 201 so the new requirement
	// is visible (the user can manually retry design from the detail page).
	if body.Design != nil && h.wizardH != nil {
		// 2a: flip skip_analysis BEFORE launchDesignOnly, so the architect's
		// fresh-session branch (wizard_architect.go:189-192) doesn't fail
		// with NO_SESSION. INSERT just hardcoded skip_analysis=0 above.
		if uerr := h.svc.UpdateSkipAnalysis(true, item.ID); uerr != nil {
			item.LaunchError = "设置 skip_analysis 失败: " + uerr.Error()
		} else {
			jid, af := h.wizardH.launchDesignOnly(item.ID, body.Design)
			if af != nil {
				item.LaunchError = af.Msg
			} else {
				// Re-fetch to pick up the freshly-written design_job_id and
				// status='designing' / design_agent_server_id / etc.
				if refreshed, gerr := h.svc.Get(item.ID); gerr == nil {
					item = refreshed
				}
				// item.DesignJobID is now the job id; frontend reads it.
				_ = jid // already on item.DesignJobID via re-fetch
			}
		}
	}

	writeJSON(w, 201, item)
}
