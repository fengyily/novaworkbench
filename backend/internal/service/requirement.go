package service

import (
	"database/sql"

	"encoding/json"
	"fmt"
	"github.com/novaworkbench/backend/internal/db"
	"regexp"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

type RequirementService struct {
	db *db.DB
}

func NewRequirementService(db *db.DB) *RequirementService {
	return &RequirementService{db: db}
}

// Requirement kind values. Broadens the legacy "需求" concept into three
// top-level categories: issue (a defect / bug report), requirement (a planned
// feature — the legacy default), and idea (an exploratory note that may or
// may not become an implementable feature). The wizard handler uses kind to
// inject tailored prompt context blocks; the frontend uses it for badges and
// CTA visibility. There is no CHECK constraint on the column — application-layer
// validation happens via ValidKind (Create rejects anything outside this set).
const (
	KindIssue       = "issue"
	KindRequirement = "requirement"
	KindIdea        = "idea"
)

// ValidKind reports whether k is one of the accepted requirement kinds. Empty
// is treated as valid at the boundary so the service can default it on the way
// in (Create defaults to "requirement" when the caller omits the field).
func ValidKind(k string) bool {
	switch k {
	case KindIssue, KindRequirement, KindIdea:
		return true
	case "":
		return true // caller may omit; the service defaults it
	default:
		return false
	}
}

// normalizeKind applies the default ("requirement") for empty/invalid-but-not-
// yet-rejected values. Callers that already validated with ValidKind can pass
// the result through unchanged; the only input that gets rewritten is "".
func normalizeKind(k string) string {
	if k == "" {
		return KindRequirement
	}
	return k
}

// Valid status transitions — two-role stage-gate lifecycle:
// draft → analyzing → designing → designed → developing → done
// (any state → archived). Each gate is completed by a manual user action.
// The analyst chat happens during "analyzing"; proceeding to architect-design
// transitions directly to "designing" (no separate "analyzed" finalization).
// "draft → designing" is the skip-analysis path: when a requirement has
// skip_analysis=true the user goes straight to architect-design without an
// analyst conversation.
// "draft → developing" is the skip-design path ("直接开发"): when a requirement
// has skip_design=true the user goes straight to coding without analyst OR
// architect stages.
var validTransitions = map[string][]string{
	"draft":      {"analyzing", "designing", "developing", "archived"},
	"analyzing":  {"designing", "draft", "archived"},
	"designing":  {"designed", "analyzing", "archived"},
	"designed":   {"developing", "archived"},
	"developing": {"done", "designed", "archived"},
	"done":       {"archived"},
	// archived is reversible: unarchive restores the requirement to "done" and
	// removes the knowledge entry it produced when archived.
	"archived": {"done"},
}

func (s *RequirementService) List(projectID string, status string, priority string, kind string) ([]model.Requirement, error) {
	// Every requirements column is qualified with the "r" alias because the
	// LEFT JOIN against agent_servers (below) introduces same-named columns
	// (status, created_at, updated_at) — unqualified references would be
	// ambiguous on MySQL/Postgres and silently wrong on SQLite.
	where := "WHERE 1=1"
	args := []interface{}{}

	if projectID != "" {
		where += " AND r.project_id = ?"
		args = append(args, projectID)
	}
	if status != "" {
		if status == "active" {
			where += " AND r.status IN ('draft','analyzing','designing','designed','developing')"
		} else if status != "archived" {
			where += " AND r.status = ?"
			args = append(args, status)
		}
	} else {
		where += " AND r.status != 'archived'"
	}
	if priority != "" {
		where += " AND r.priority = ?"
		args = append(args, priority)
	}
	// kind filter — empty (or "all") means no filtering; a comma-separated list
	// (e.g. "issue,idea") becomes an IN clause for cross-project listings.
	if kind != "" && kind != "all" {
		kinds := splitKinds(kind)
		if len(kinds) == 1 {
			where += " AND r.kind = ?"
			args = append(args, kinds[0])
		} else if len(kinds) > 1 {
			placeholders := make([]string, len(kinds))
			for i, k := range kinds {
				placeholders[i] = "?"
				args = append(args, k)
			}
			where += " AND r.kind IN (" + strings.Join(placeholders, ",") + ")"
		}
	}

	rows, err := s.db.Query(
		"SELECT r.id,r.project_id,r.title,r.description,r.status,r.priority,r.kind,r.acceptance_criteria,r.design_docs,r.conversation_ids,r.assigned_to,r.created_by,r.source_requirement_id,r.analysis_session_id,r.design_session_id,r.design_job_id,r.analysis_job_id,r.apply_job_id,r.coding_session_id,r.skip_analysis,r.skip_design,r.branch_name,r.worktree_path,r.analyst_model,r.architect_model,r.developer_model,r.reviewer_model,r.agent_server_id,COALESCE(ags.name,''),r.design_agent_server_id,COALESCE(dags.name,''),r.analyst_context_summary,r.analyst_compressed_at,r.design_context_summary,r.design_compressed_at,r.coding_context_summary,r.coding_compressed_at,r.usage_snapshots,r.coding_plan,r.dev_source,r.dev_mode,r.sync_mode,r.auto_push,r.created_at,r.updated_at,r.completed_at,r.analysis_started_at,r.analysis_ended_at"+
			" FROM requirements r LEFT JOIN agent_servers ags ON ags.id = r.agent_server_id LEFT JOIN agent_servers dags ON dags.id = r.design_agent_server_id"+
			" "+where+" ORDER BY CASE WHEN r.status = 'done' THEN 1 ELSE 0 END ASC, r.created_at DESC",
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []model.Requirement
	for rows.Next() {
		var r model.Requirement
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Title, &r.Description, &r.Status, &r.Priority, &r.Kind,
			&r.AcceptanceCriteria, &r.DesignDocs, &r.ConversationIDs, &r.AssignedTo,
			&r.CreatedBy, &r.SourceRequirementID, &r.AnalysisSessionID, &r.DesignSessionID, &r.DesignJobID, &r.AnalysisJobID, &r.ApplyJobID, &r.CodingSessionID, &r.SkipAnalysis, &r.SkipDesign, &r.BranchName, &r.WorktreePath,
			&r.AnalystModel, &r.ArchitectModel, &r.DeveloperModel, &r.ReviewerModel,
			&r.AgentServerID, &r.AgentServerName, &r.DesignAgentServerID, &r.DesignAgentServerName,
			&r.AnalystContextSummary, &r.AnalystCompressedAt, &r.DesignContextSummary, &r.DesignCompressedAt, &r.CodingContextSummary, &r.CodingCompressedAt,
			&r.UsageSnapshots, &r.CodingPlan, &r.DevSource, &r.DevMode, &r.SyncMode, &r.AutoPush,
			&r.CreatedAt, &r.UpdatedAt, &r.CompletedAt, &r.AnalysisStartedAt, &r.AnalysisEndedAt); err != nil {
			return nil, err
		}
		items = append(items, r)
	}
	if items == nil {
		items = []model.Requirement{}
	}
	s.attachAgentServerNames(items)
	return items, nil
}

// Calendar returns a slim slice of requirements overlapping the [from, to)
// // window (half-open, day-granular: from=00:00 of the first day, to=00:00 of
// the day AFTER the last day) for the calendar view. Only the columns the
// month/day/year grids need are read — chat history, design docs, usage
// snapshots and the per-stage model columns are intentionally skipped to keep
// the response small (and avoid dragging a few-hundred-KB design_docs JSON
// across the wire on every grid pan).
//
// Visible anchor rules (mirrored on the frontend in eventLayout.normalize):
//   - PlannedStartAt != NULL → the requirement sits between planned_start_at
//     and COALESCE(planned_end_at, planned_start_at) on the grid.
//   - both NULL → the requirement sits at created_at as a zero-duration
//     event (legacy rows fall into this bucket without a backfill).
//
// Archived rows are always excluded — same default as List().
func (s *RequirementService) Calendar(from, to time.Time, projectID, kind string) ([]model.Requirement, error) {
	where := "WHERE status != 'archived'"
	args := []interface{}{}

	// Project filter — same convention as List: empty string = all visible.
	if projectID != "" {
		where += " AND project_id = ?"
		args = append(args, projectID)
	}
	// Kind CSV — reuses splitKinds so the calendar filter behaves identically
	// to the list filter.
	if kind != "" && kind != "all" {
		kinds := splitKinds(kind)
		if len(kinds) == 1 {
			where += " AND kind = ?"
			args = append(args, kinds[0])
		} else if len(kinds) > 1 {
			placeholders := make([]string, len(kinds))
			for i, k := range kinds {
				placeholders[i] = "?"
				args = append(args, k)
			}
			where += " AND kind IN (" + strings.Join(placeholders, ",") + ")"
		}
	}

	// Range predicate. SQLite stores DATETIME as TEXT in mixed formats
	// (CURRENT_TIMESTAMP → "YYYY-MM-DD HH:MM:SS", Go time.Time driver write →
	// RFC3339), but every value parses to a comparable lexical prefix at the
	// day boundary. The window is [from, to) on a day-level prefix so a single
	// substr() expression matches all three dialects (the pattern lives in
	// usage.dateExpr / usage.DailyByProject). It covers BOTH scheduling modes:
	//   - legacy (planned_* NULL) → uses created_at at day-level
	//   - scheduled                → uses planned_start_at COALESCE end date
	fromKey := from.Format("2006-01-02")
	toKey := to.Format("2006-01-02")
	where += " AND ("
	where += "(planned_start_at IS NULL AND substr(created_at,1,10) >= ? AND substr(created_at,1,10) < ?)"
	args = append(args, fromKey, toKey)
	where += " OR (planned_start_at IS NOT NULL AND COALESCE(planned_end_at, planned_start_at) >= ? AND planned_start_at < ?)"
	// planned_* are only ever written by UpdateSchedule via time.Time params,
	// so they always serialize to RFC3339 — a direct lexical compare against
	// the formatted bounds is fine across all dialects (no substr() needed).
	fromRFC := from.Format(time.RFC3339)
	toRFC := to.Format(time.RFC3339)
	args = append(args, fromRFC, toRFC)
	where += ")"

	// Slim SELECT — only what the grid actually renders. SubTaskCount is not
	// joined here (calendar doesn't need it); AgentServerName is left empty
	// (calendar UI shows it on the detail panel, fetched lazily).
	q := "SELECT id, project_id, title, status, priority, kind, created_at, updated_at, completed_at, planned_start_at, planned_end_at FROM requirements " +
		where + " ORDER BY CASE WHEN status = 'done' THEN 1 ELSE 0 END ASC, created_at DESC"

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []model.Requirement
	for rows.Next() {
		var r model.Requirement
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Title, &r.Status, &r.Priority, &r.Kind,
			&r.CreatedAt, &r.UpdatedAt, &r.CompletedAt, &r.PlannedStartAt, &r.PlannedEndAt); err != nil {
			return nil, err
		}
		items = append(items, r)
	}
	if items == nil {
		items = []model.Requirement{}
	}
	// No agent-server join needed for the calendar grid; just attach the
	// scheduled-task marker.
	s.attachScheduledRunAt(items)
	return items, nil
}

// UpdateSchedule persists a requirement's calendar scheduling fields.
// Either field may be nil (caller-supplied or omitted JSON), in which case
// that column is cleared back to NULL — the frontend uses this to reset a
// row to the created_at anchor. Validates end >= start; both ends may also
// both be nil (clear the schedule entirely). updated_at is bumped so the
// detail page reflects the change on next GET without a refresh.
func (s *RequirementService) UpdateSchedule(id string, start, end *time.Time) error {
	if start != nil && end != nil && end.Before(*start) {
		return fmt.Errorf("planned_end_at must be >= planned_start_at")
	}
	// Sanity guard against obviously-broken inputs (epoch zero, year 9999)
	// which would otherwise poison the calendar with 1970-01-01 cells.
	check := func(t *time.Time) bool {
		return t == nil || (t.Year() >= 1970 && t.Year() <= 9998)
	}
	if !check(start) || !check(end) {
		return fmt.Errorf("planned time out of range")
	}
	_, err := s.db.Exec(
		"UPDATE requirements SET planned_start_at = ?, planned_end_at = ?, updated_at = ? WHERE id = ?",
		start, end, time.Now(), id)
	return err
}

// splitKinds parses a comma-separated kind filter (e.g. "issue,idea"), trims
// whitespace, lowercases, and drops any invalid entries so a malformed value
// never produces SQL surprises. Always returns at least the single trimmed
// input when non-empty.
func splitKinds(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		switch p {
		case KindIssue, KindRequirement, KindIdea:
			out = append(out, p)
		}
	}
	return out
}

func (s *RequirementService) Get(id string) (*model.Requirement, error) {
	var r model.Requirement
	// LEFT JOIN against agent_servers resolves the human-readable name for the
	// bound Agent Server (see model.Requirement.AgentServerName). All columns
	// are aliased with "r" because agent_servers also carries status /
	// created_at / updated_at — unqualified references would be ambiguous on
	// MySQL/Postgres.
	err := s.db.QueryRow(
		"SELECT r.id,r.project_id,r.title,r.description,r.status,r.priority,r.kind,r.acceptance_criteria,r.design_docs,r.conversation_ids,r.assigned_to,r.created_by,r.source_requirement_id,r.analysis_session_id,r.design_session_id,r.design_job_id,r.analysis_job_id,r.apply_job_id,r.coding_session_id,r.skip_analysis,r.skip_design,r.branch_name,r.worktree_path,r.analyst_model,r.architect_model,r.developer_model,r.reviewer_model,r.agent_server_id,COALESCE(ags.name,''),r.design_agent_server_id,COALESCE(dags.name,''),r.analyst_context_summary,r.analyst_compressed_at,r.design_context_summary,r.design_compressed_at,r.coding_context_summary,r.coding_compressed_at,r.usage_snapshots,r.coding_plan,r.dev_source,r.dev_mode,r.sync_mode,r.auto_push,r.created_at,r.updated_at,r.completed_at,r.analysis_started_at,r.analysis_ended_at"+
			" FROM requirements r LEFT JOIN agent_servers ags ON ags.id = r.agent_server_id LEFT JOIN agent_servers dags ON dags.id = r.design_agent_server_id"+
			" WHERE r.id = ?", id).
		Scan(&r.ID, &r.ProjectID, &r.Title, &r.Description, &r.Status, &r.Priority, &r.Kind,
			&r.AcceptanceCriteria, &r.DesignDocs, &r.ConversationIDs, &r.AssignedTo,
			&r.CreatedBy, &r.SourceRequirementID, &r.AnalysisSessionID, &r.DesignSessionID, &r.DesignJobID, &r.AnalysisJobID, &r.ApplyJobID, &r.CodingSessionID, &r.SkipAnalysis, &r.SkipDesign, &r.BranchName, &r.WorktreePath,
			&r.AnalystModel, &r.ArchitectModel, &r.DeveloperModel, &r.ReviewerModel,
			&r.AgentServerID, &r.AgentServerName, &r.DesignAgentServerID, &r.DesignAgentServerName,
			&r.AnalystContextSummary, &r.AnalystCompressedAt, &r.DesignContextSummary, &r.DesignCompressedAt, &r.CodingContextSummary, &r.CodingCompressedAt,
			&r.UsageSnapshots, &r.CodingPlan, &r.DevSource, &r.DevMode, &r.SyncMode, &r.AutoPush,
			&r.CreatedAt, &r.UpdatedAt, &r.CompletedAt, &r.AnalysisStartedAt, &r.AnalysisEndedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("requirement not found")
	}
	if err != nil {
		return nil, err
	}
	// Defensive: a legacy row upgraded before the kind column existed will scan
	// as "" (the column's DEFAULT runs at INSERT time, but a SELECT against a
	// pre-existing table with the column missing would have errored out — so a
	// blank kind here would only happen if the column was added with NULL then
	// backfilled partially). Normalize so callers never see "".
	if r.Kind == "" {
		r.Kind = KindRequirement
	}
	// Count linked sub_tasks so the requirement detail page can decide whether
	// the requirement-level "追加调整" entry should be hidden (i.e. the
	// requirement has already been decomposed into sub-tasks and further
	// adjustments must flow through the sub-task composer instead). A
	// separate COUNT is cheaper than re-fetching the sub-task list and avoids
	// loading artifacts / SSE job ids the detail page doesn't render.
	if err := s.db.QueryRow(
		"SELECT COUNT(*) FROM sub_tasks WHERE requirement_id = ?", id,
	).Scan(&r.SubTaskCount); err != nil {
		return nil, err
	}
	// Derive the development-stage span from the linked sub_tasks: the first
	// task's creation and the last finished task's completion ("以最后一个任务结束
	// 时间为准"). Both come back NULL when the requirement has no sub_tasks (a
	// legacy single-shot coding run) — nullable *time.Time absorbs that, and the
	// bounds recompute automatically when a redo/continue clears a task's
	// completed_at. ORDER BY … LIMIT 1 (instead of MIN()/MAX()) is deliberate:
	// SQLite drops a column's DATETIME affinity through an aggregate, so the
	// driver hands back an unparseable string; selecting the column value
	// directly preserves affinity and parses into time.Time on every dialect.
	// Best-effort: a scan error here must not fail the GET.
	_ = s.db.QueryRow(
		"SELECT created_at FROM sub_tasks WHERE requirement_id = ? ORDER BY created_at ASC LIMIT 1", id,
	).Scan(&r.DevStartedAt)
	_ = s.db.QueryRow(
		"SELECT completed_at FROM sub_tasks WHERE requirement_id = ? AND completed_at IS NOT NULL ORDER BY completed_at DESC LIMIT 1", id,
	).Scan(&r.DevEndedAt)
	// Resolve the agent server display name (no-op for local rows).
	one := []model.Requirement{r}
	s.attachAgentServerNames(one)
	r.AgentServerName = one[0].AgentServerName
	r.DesignAgentServerName = one[0].DesignAgentServerName
	return &r, nil
}

func (s *RequirementService) Create(req model.CreateRequirementReq) (*model.Requirement, error) {
	id := util.NewID("req")
	if req.Priority == "" {
		req.Priority = "medium"
	}
	// Validate kind at the boundary; reject unknown values so a typo in the
	// frontend or a future API consumer doesn't silently misclassify a
	// requirement. Empty is allowed here and normalized below.
	if !ValidKind(req.Kind) {
		return nil, fmt.Errorf("invalid kind: %q (allowed: issue, requirement, idea)", req.Kind)
	}
	kind := normalizeKind(req.Kind)
	// Default to skip-analysis (true) when the caller omits the field, so the
	// "default skip" product decision holds even for clients that don't send it.
	skipAnalysis := true
	if req.SkipAnalysis != nil {
		skipAnalysis = *req.SkipAnalysis
	}
	// Default to NOT skipping design (false) — "直接开发" is opt-in, the full
	// analyst→architect→developer pipeline stays the default.
	skipDesign := false
	if req.SkipDesign != nil {
		skipDesign = *req.SkipDesign
	}
	now := time.Now()

	// Validate source_requirement_id if provided: must point to an existing
	// requirement in the SAME project. Cross-project source references would
	// break the "summary lives next to its discussion" model — and a typo
	// would silently orphan the linkage.
	sourceID := ""
	if req.SourceRequirementID != "" {
		var ownerProject string
		err := s.db.QueryRow("SELECT project_id FROM requirements WHERE id = ?", req.SourceRequirementID).Scan(&ownerProject)
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("source_requirement_id %q not found", req.SourceRequirementID)
		}
		if err != nil {
			return nil, err
		}
		if ownerProject != req.ProjectID {
			return nil, fmt.Errorf("source_requirement_id %q belongs to a different project", req.SourceRequirementID)
		}
		sourceID = req.SourceRequirementID
	}

	_, err := s.db.Exec(
		"INSERT INTO requirements (id,project_id,title,description,status,priority,kind,acceptance_criteria,design_docs,conversation_ids,created_by,source_requirement_id,skip_analysis,skip_design,created_at,updated_at) VALUES (?,?,?,?,'draft',?,?,'[]','[]','[]','user',?,?,?,?,?)",
		id, req.ProjectID, req.Title, req.Description, req.Priority, kind, sourceID, skipAnalysis, skipDesign, now, now)
	if err != nil {
		return nil, err
	}

	return s.Get(id)
}

func (s *RequirementService) Update(id string, req model.CreateRequirementReq) (*model.Requirement, error) {
	// skip_analysis is a *bool: nil preserves the stored value (COALESCE keeps
	// the existing column when the param is NULL), a non-nil pointer updates it.
	// This lets the edit modal toggle the flag while other callers that only
	// touch title/description/priority leave it untouched.
	var skipArg interface{}
	if req.SkipAnalysis != nil {
		skipArg = *req.SkipAnalysis
	}
	_, err := s.db.Exec(
		"UPDATE requirements SET title=?, description=?, priority=?, skip_analysis=COALESCE(?,skip_analysis), updated_at=? WHERE id=?",
		req.Title, req.Description, req.Priority, skipArg, time.Now(), id)
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

func (s *RequirementService) UpdateStatus(id string, newStatus string) (*model.Requirement, error) {
	r, err := s.Get(id)
	if err != nil {
		return nil, err
	}

	// Validate transition
	allowed := validTransitions[r.Status]
	valid := false
	for _, s := range allowed {
		if s == newStatus {
			valid = true
			break
		}
	}
	if !valid {
		return nil, fmt.Errorf("invalid status transition: %s -> %s", r.Status, newStatus)
	}

	now := time.Now()
	var completedAt *time.Time
	if newStatus == "done" {
		completedAt = &now
	}

	// Plan-analysis timing. Only the columns relevant to this transition are
	// touched — an unconditional UPDATE would clobber the other timestamp to
	// NULL. Side paths (skip-analysis / skip-design) backfill idempotently with
	// COALESCE so a stage that was skipped still records a sensible bound.
	setClauses := []string{"status=?", "updated_at=?", "completed_at=?"}
	setArgs := []interface{}{newStatus, now, completedAt}
	switch newStatus {
	case "analyzing":
		// Every (re-)entry into analysis restarts the clock — start re-stamped,
		// end cleared — so "重做/重新进入分析" recomputes the analysis duration.
		setClauses = append(setClauses, "analysis_started_at=?", "analysis_ended_at=NULL")
		setArgs = append(setArgs, now)
	case "designing":
		// Skip-analysis path (draft → designing): the analyst stage never ran, so
		// stamp the analysis start now if it hasn't been recorded already.
		setClauses = append(setClauses, "analysis_started_at=COALESCE(analysis_started_at, ?)")
		setArgs = append(setArgs, now)
	case "designed":
		// Plan finalized — the analysis span ends here (analyst + architect).
		setClauses = append(setClauses, "analysis_ended_at=?")
		setArgs = append(setArgs, now)
	case "developing":
		// Skip-design path (draft/designed → developing without a "designed"
		// gate): close the analysis span now if it was never stamped.
		setClauses = append(setClauses, "analysis_ended_at=COALESCE(analysis_ended_at, ?)")
		setArgs = append(setArgs, now)
	}
	setArgs = append(setArgs, id)

	_, err = s.db.Exec("UPDATE requirements SET "+strings.Join(setClauses, ", ")+" WHERE id=?", setArgs...)
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

// UpdateAnalysisSession persists the claude CLI session id used for the analyst
// conversation so subsequent turns can resume it via --resume.
func (s *RequirementService) UpdateAnalysisSession(id, sessionID string) error {
	_, err := s.db.Exec("UPDATE requirements SET analysis_session_id=?, updated_at=? WHERE id=?",
		sessionID, time.Now(), id)
	return err
}

// UpdateDesignSession persists the claude CLI session id for the architect
// conversation (a fork off the analyst session). Subsequent design refine turns
// resume it via --resume.
func (s *RequirementService) UpdateDesignSession(id, sessionID string) error {
	_, err := s.db.Exec("UPDATE requirements SET design_session_id=?, updated_at=? WHERE id=?",
		sessionID, time.Now(), id)
	return err
}

// UpdateDesignJob persists the active architect-design JobStore job id so a page
// refresh can reconnect to the running job. Pass "" to clear it (on success,
// failure, or staleness) so the UI stops showing the "executing" state.
func (s *RequirementService) UpdateDesignJob(id, jobID string) error {
	_, err := s.db.Exec("UPDATE requirements SET design_job_id=?, updated_at=? WHERE id=?",
		jobID, time.Now(), id)
	return err
}

// UpdateAnalysisJob persists the active analyst-chat JobStore job id so a page
// refresh can reconnect to the running turn. Pass "" to clear it (on success,
// failure, or staleness) so the UI stops showing the "analyzing" spinner and
// the next turn can start.
func (s *RequirementService) UpdateAnalysisJob(id, jobID string) error {
	_, err := s.db.Exec("UPDATE requirements SET analysis_job_id=?, updated_at=? WHERE id=?",
		jobID, time.Now(), id)
	return err
}

// UpdateApplyJob persists the active apply-doc JobStore job id so a page refresh
// can reconnect to the running apply. Pass "" to clear it (on success, failure,
// or staleness) so the UI stops showing the "applying" state and a refresh
// doesn't try to reconnect to a finished job.
func (s *RequirementService) UpdateApplyJob(id, jobID string) error {
	_, err := s.db.Exec("UPDATE requirements SET apply_job_id=?, updated_at=? WHERE id=?",
		jobID, time.Now(), id)
	return err
}

// UpdateCodingSession persists the claude CLI session id for the developer
// conversation (a fork off the design session). Subsequent coding turns resume it.
func (s *RequirementService) UpdateCodingSession(id, sessionID string) error {
	_, err := s.db.Exec("UPDATE requirements SET coding_session_id=?, updated_at=? WHERE id=?",
		sessionID, time.Now(), id)
	return err
}

// contextSummaryColumns maps the wizard's step names ("analyst_chat" /
// "architect_design" / "coding") to the (summary_col, compressed_at_col,
// session_id_col) triple that the CompressContext wizard handler drives.
// Centralizing the mapping here keeps UpdateContextSummary /
// GetContextSummary / the wizard handler in lockstep — adding a fourth
// stage would only touch this one switch.
//
// The keys match the wizard's `usageCtxFor(step, ...)` names used
// everywhere else in the wizard pipeline (see wizard.go), so the frontend
// can pass the same step value it uses for the usage-bar step key and
// expect the right column to be written.
//
// The session_id column is cleared atomically with the summary write inside
// UpdateContextSummary so the next turn in this stage starts a fresh session
// (and the wizard handler injects the stored summary as a prompt prefix).
var contextSummaryColumns = map[string]struct {
	Summary      string
	CompressedAt string
	SessionID    string
}{
	"analyst_chat":     {"analyst_context_summary", "analyst_compressed_at", "analysis_session_id"},
	"architect_design": {"design_context_summary", "design_compressed_at", "design_session_id"},
	"coding":           {"coding_context_summary", "coding_compressed_at", "coding_session_id"},
}

// ValidContextSummaryStep reports whether step is one of the wizard stages
// that supports context compression. The wizard CompressContext handler uses
// this to reject bogus inputs early so a typo doesn't silently match the
// zero-value entry in contextSummaryColumns.
func ValidContextSummaryStep(step string) bool {
	_, ok := contextSummaryColumns[step]
	return ok
}

// UpdateContextSummary writes the Chinese summary produced by the compress-
// context wizard handler into the matching *_context_summary column, stamps
// compressed_at with NOW(), and clears the matching session id — all in a
// single transaction so a failure never leaves the requirement in a half-
// compressed state (summary without sid reset, or sid reset without summary).
//
// step must be one of "analyst_chat" / "architect_design" / "coding"
// (validated via ValidContextSummaryStep); an invalid step returns an
// error and writes nothing. summary is stored verbatim — the handler is
// responsible for stripping the [COMPRESS_COMPLETE] sentinel before calling.
func (s *RequirementService) UpdateContextSummary(id, step, summary string) error {
	cols, ok := contextSummaryColumns[step]
	if !ok {
		return fmt.Errorf("invalid step %q (allowed: analyst_chat, architect_design, coding)", step)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Ident() guards the column names against the (admittedly unlikely) future
	// case where one of these names becomes a reserved word in MySQL. It is a
	// no-op for SQLite/Postgres.
	q := "UPDATE requirements SET " +
		s.db.Ident(cols.Summary) + "=?, " +
		s.db.Ident(cols.CompressedAt) + "=CURRENT_TIMESTAMP, " +
		s.db.Ident(cols.SessionID) + "='', updated_at=? " +
		"WHERE id=?"
	if _, err := tx.Exec(q, summary, time.Now(), id); err != nil {
		return err
	}
	return tx.Commit()
}

// GetContextSummary returns the stored compression summary for one stage.
// Empty string + nil error means the stage hasn't been compressed yet.
// Invalid step names return an error so the handler can surface a 400.
func (s *RequirementService) GetContextSummary(id, step string) (string, error) {
	cols, ok := contextSummaryColumns[step]
	if !ok {
		return "", fmt.Errorf("invalid step %q (allowed: analyst_chat, architect_design, coding)", step)
	}
	q := "SELECT " + s.db.Ident(cols.Summary) + " FROM requirements WHERE id=?"
	var out string
	if err := s.db.QueryRow(q, id).Scan(&out); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("requirement not found")
		}
		return "", err
	}
	return out, nil
}

// usageSnapshotSessionKeys is the set of valid session keys inside the
// usage_snapshots JSON blob. Mirrors the keys of contextSummaryColumns so a
// single source of truth defines the wizard's three sessions; adding a fourth
// stage would only touch contextSummaryColumns.
func usageSnapshotSessionKeys() []string {
	// contextSummaryColumns is a map (unordered), so canonicalize for a
	// stable error message. Order doesn't matter for validation.
	return []string{"analyst_chat", "architect_design", "coding"}
}

// validUsageSnapshotKey reports whether key is one of the wizard session keys
// the usage_snapshots blob is allowed to carry.
func validUsageSnapshotKey(key string) bool {
	for _, k := range usageSnapshotSessionKeys() {
		if k == key {
			return true
		}
	}
	return false
}

// UpdateUsageSnapshot merges one session's latest token-usage snapshot into the
// requirements.usage_snapshots JSON blob. The blob is shaped
// {"analyst_chat":{…},"architect_design":{…},"coding":{…}}; this call replaces
// only the entry for `sessionKey` (the others are preserved verbatim).
//
// It runs as a read-modify-write inside a single transaction: SELECT the blob
// → unmarshal ({} when empty/missing) → overwrite the one key → marshal →
// UPDATE. SQLite is a single-writer connection so there's no race; on
// MySQL/Postgres two concurrent turns on the same requirement don't overlap in
// practice. The transaction keeps the blob consistent even if they did.
//
// snapshotJSON is the value verbatim (a JSON object the caller marshals).
// Errors are returned but callers should treat them as best-effort (log +
// continue) — a snapshot write must never break a claude turn, mirroring the
// usageCtx.recordFrom policy. An invalid sessionKey returns an error and
// writes nothing.
func (s *RequirementService) UpdateUsageSnapshot(id, sessionKey, snapshotJSON string) error {
	if !validUsageSnapshotKey(sessionKey) {
		return fmt.Errorf("invalid session key %q (allowed: analyst_chat, architect_design, coding)", sessionKey)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var blob string
	if err := tx.QueryRow("SELECT usage_snapshots FROM requirements WHERE id=?", id).Scan(&blob); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("requirement not found")
		}
		return err
	}

	// Parse existing blob (tolerant of empty / legacy rows). Unknown keys are
	// preserved so older frontends reading a blob with extra keys don't choke.
	snapshots := map[string]json.RawMessage{}
	if blob != "" {
		// A bare unmarshal into the map silently drops malformed JSON; log the
		// error so a corrupt blob is debuggable but don't fail the turn — start
		// from an empty map instead.
		if jErr := json.Unmarshal([]byte(blob), &snapshots); jErr != nil {
			snapshots = map[string]json.RawMessage{}
		}
	}
	snapshots[sessionKey] = json.RawMessage(snapshotJSON)

	merged, err := json.Marshal(snapshots)
	if err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE requirements SET "+s.db.Ident("usage_snapshots")+"=?, updated_at=? WHERE id=?",
		string(merged), time.Now(), id); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateWorktree persists the dev branch name and the absolute path of the
// isolated git worktree created for parallel development. Pass "" for both to
// clear (after a merge/cleanup) so later stages fall back to the shared project
// checkout instead of a stale worktree path.
func (s *RequirementService) UpdateWorktree(id, branch, path string) error {
	_, err := s.db.Exec("UPDATE requirements SET branch_name=?, worktree_path=?, updated_at=? WHERE id=?",
		branch, path, time.Now(), id)
	return err
}

// UpdateCodingPlan persists the auto-orchestrate summary Markdown produced by
// the developer main agent after every child sub-task in a batch has
// finished. The frontend renders it under the SubTaskPanel so the user can
// see "what the main agent thinks happened" without scrolling through every
// individual sub-task artifact. Empty plan overwrites the previous one
// (resubmitting a plan to a different requirement clears any leftover).
func (s *RequirementService) UpdateCodingPlan(id, plan string) error {
	_, err := s.db.Exec("UPDATE requirements SET coding_plan=?, updated_at=? WHERE id=?",
		plan, time.Now(), id)
	return err
}

// UpdateStageModel persists the effective model used for a stage on the
// success path only (callers skip the write when the run failed, so a failed
// run never clobbers the last good record). Each stage maps to its own column.
// The updated_at bump makes the next GET reflect the new value immediately.
func (s *RequirementService) UpdateStageModel(id, column, model string) error {
	_, err := s.db.Exec("UPDATE requirements SET "+s.db.Ident(column)+"=?, updated_at=? WHERE id=?",
		model, time.Now(), id)
	return err
}

// UpdateAnalystModel / UpdateArchitectModel / UpdateDeveloperModel /
// UpdateReviewerModel are thin wrappers over UpdateStageModel so callers don't
// hand-build column names. Reviewer is currently unused (review is a
// project-level job persisted on job_logs.model) but kept for symmetry / future
// per-requirement review binding.
func (s *RequirementService) UpdateAnalystModel(id, model string) error {
	return s.UpdateStageModel(id, "analyst_model", model)
}

func (s *RequirementService) UpdateArchitectModel(id, model string) error {
	return s.UpdateStageModel(id, "architect_model", model)
}

func (s *RequirementService) UpdateDeveloperModel(id, model string) error {
	return s.UpdateStageModel(id, "developer_model", model)
}

func (s *RequirementService) UpdateReviewerModel(id, model string) error {
	return s.UpdateStageModel(id, "reviewer_model", model)
}

// Development-environment provenance values for requirements.dev_source.
// Stamped once by the coding stage so the UI can render "Agent Server 开发"
// vs "本地开发", and so every follow-up action that touches the working tree
// (push+PR / worktree cleanup / sub-task dispatch) can route itself back to
// the environment the code actually lives in.
const (
	DevSourceLocal = "local"
	DevSourceAgent = "agent"
)

// Design-environment provenance values for requirements.design_agent_server_id.
// Mirrors DevSourceLocal / DevSourceAgent but for the architect-design stage:
// the dropdown on the design toolbar picks the Agent server that will run the
// plan-mode claude invocation. Empty / DesignSourceLocal means the run stays
// on the NovaWorkbench host; DesignSourceAgent + a server id means
// runRemoteArchitectDesign will dispatch the work to that server via SSH.
const (
	DesignSourceLocal = "local"
	DesignSourceAgent = "agent"
)

// UpdateDevSource stamps where this requirement is being developed. serverID
// is the agent_servers row id when source == DevSourceAgent, and is forced to
// "" for local runs so a requirement that moves from an Agent server back to
// local execution doesn't keep routing follow-ups to the stale server.
//
// Called at the START of the coding stage (not on success) so the provenance
// survives a failed/aborted run — the remote worktree exists either way and
// cleanup still has to happen on that host.
func (s *RequirementService) UpdateDevSource(id, source, serverID string) error {
	if source != DevSourceAgent {
		source, serverID = DevSourceLocal, ""
	}
	_, err := s.db.Exec(
		"UPDATE requirements SET dev_source = ?, agent_server_id = ?, updated_at = ? WHERE id = ?",
		source, serverID, time.Now(), id)
	return err
}

// UpdateDesignAgentServer stamps WHERE the architect-design stage was run.
// serverID is the agent_servers row id when an Agent server was picked, and
// the wizard handler forces it to "" when no server was picked so a
// requirement that flips back to local execution doesn't keep routing
// follow-up "继续设计" actions to the stale server.
//
// Called at the START of the design stage (mirroring UpdateDevSource) so
// the binding survives a failed/aborted run — the user can still inspect
// "this requirement tried server X" on a retry.
func (s *RequirementService) UpdateDesignAgentServer(id, serverID string) error {
	_, err := s.db.Exec(
		"UPDATE requirements SET design_agent_server_id = ?, updated_at = ? WHERE id = ?",
		serverID, time.Now(), id)
	return err
}

// Dev mode constants for the coding stage. Empty ("") is the legacy
// pre-feature value and is treated like DevModeSession (forks the design
// session) by the wizard handler — existing rows keep their current
// behavior without a backfill.
const (
	DevModeSession = "session" // fork the design/analysis session (legacy default)
	DevModeDesign  = "design"  // fresh session, hand the stored design doc to the agent via -p
)

// UpdateDevMode stamps how the coding stage was launched. Called from
// StartCoding at run-start (mirroring UpdateDevSource) so the UI can show
// "本次开发基于会话/方案" badges, a follow-up StartCoding that omits the
// field can default to the persisted value, and a compress-context pass
// can tell the two modes apart. Invalid values are normalized to empty so a
// bad client never corrupts the column.
func (s *RequirementService) UpdateDevMode(id, mode string) error {
	switch mode {
	case DevModeSession, DevModeDesign:
		// ok
	default:
		mode = ""
	}
	_, err := s.db.Exec(
		"UPDATE requirements SET dev_mode = ?, updated_at = ? WHERE id = ?",
		mode, time.Now(), id)
	return err
}

// Code-transport modes for Agent-server execution (requirements.sync_mode).
// SyncModeRemote ("") is the legacy default: the remote host clones origin and
// pushes commits back to origin, so a reachable git remote is required.
// SyncModeLocal ("local") ships code to / from the agent host as git-bundle
// files over SFTP and integrates locally via 本地合并 — the path for local
// self-hosted repos that have no remote reachable from the agent server.
const (
	SyncModeRemote = ""
	SyncModeLocal  = "local"
)

// UpdateSyncMode stamps how code is shipped to / from the Agent server for
// this requirement. Any value other than SyncModeLocal is normalized to
// SyncModeRemote ("") so a bad client can never corrupt the column. Kept as a
// standalone updater (rather than extending UpdateDevSource's signature) so
// the two provenance stamps stay independently writable.
func (s *RequirementService) UpdateSyncMode(id, mode string) error {
	if mode != SyncModeLocal {
		mode = SyncModeRemote
	}
	_, err := s.db.Exec(
		"UPDATE requirements SET sync_mode = ?, updated_at = ? WHERE id = ?",
		mode, time.Now(), id)
	return err
}

// UpdateAutoPush stamps whether this requirement should auto-dispatch the
// "提交 → 推送 → 创建 PR" sub-task when its coding stage finishes. Written by
// the start-coding prologue from the preflight-dialog toggle so the three
// development-completion points (local non-split, Agent-server, split
// orchestrator summary) — one of which may run asynchronously after a restart
// — all read the same persisted intent.
func (s *RequirementService) UpdateAutoPush(id string, autoPush bool) error {
	_, err := s.db.Exec(
		"UPDATE requirements SET auto_push = ?, updated_at = ? WHERE id = ?",
		autoPush, time.Now(), id)
	return err
}

// attachAgentServerNames fills the display-only AgentServerName on every row
// that carries an AgentServerID, using one grouped SELECT against
// agent_servers instead of a per-row lookup. Best-effort: on any error the
// rows are returned unchanged (the UI degrades to "Agent Server 开发" with no
// name rather than failing the whole list).
func (s *RequirementService) attachAgentServerNames(items []model.Requirement) {
	need := false
	for i := range items {
		if items[i].AgentServerID != "" || items[i].DesignAgentServerID != "" {
			need = true
			break
		}
	}
	if !need {
		return
	}
	rows, err := s.db.Query("SELECT id, name FROM agent_servers")
	if err != nil {
		return
	}
	defer rows.Close()
	names := map[string]string{}
	for rows.Next() {
		var id, name string
		if rows.Scan(&id, &name) == nil {
			names[id] = name
		}
	}
	for i := range items {
		if n, ok := names[items[i].AgentServerID]; ok {
			items[i].AgentServerName = n
		}
		if n, ok := names[items[i].DesignAgentServerID]; ok {
			items[i].DesignAgentServerName = n
		}
	}
}

// attachScheduledRunAt fills the display-only ScheduledRunAt on every row that
// has at least one pending scheduled_tasks row. Mirrors attachAgentServerNames
// (single grouped query, best-effort). Picks MIN(run_at) so the calendar shows
// the nearest pending dispatch — multiple pending rows collapse to one marker,
// which matches the "this requirement has a scheduled task waiting" affordance
// the clock icon conveys.
func (s *RequirementService) attachScheduledRunAt(items []model.Requirement) {
	if len(items) == 0 {
		return
	}
	ids := make([]interface{}, 0, len(items))
	placeholders := make([]string, 0, len(items))
	for _, r := range items {
		if r.ID == "" {
			continue
		}
		ids = append(ids, r.ID)
		placeholders = append(placeholders, "?")
	}
	if len(ids) == 0 {
		return
	}
	q := "SELECT requirement_id, MIN(run_at) FROM scheduled_tasks WHERE status = 'pending' AND requirement_id IN (" + strings.Join(placeholders, ",") + ") GROUP BY requirement_id"
	rows, err := s.db.Query(q, ids...)
	if err != nil {
		return
	}
	defer rows.Close()
	next := map[string]time.Time{}
	for rows.Next() {
		var id string
		var runAt time.Time
		if rows.Scan(&id, &runAt) == nil {
			next[id] = runAt
		}
	}
	for i := range items {
		if t, ok := next[items[i].ID]; ok {
			tt := t
			items[i].ScheduledRunAt = &tt
		}
	}
}

// Summarizer is the LLM-side contract used by PromoteFromIdea to convert an
// idea's discussion thread into a draft requirement. Defined as an interface so
// service doesn't import llm (and the real Gateway satisfies it without
// registering extra wiring).
//
// Token-usage recording is NOT part of this contract: service doesn't consume
// the usage object. Handlers that want to record tokens can do it against the
// underlying *llm.Gateway separately (matches the pattern in Create).
type Summarizer interface {
	SummarizeIdeaToRequirement(content string) (markdown, title string, criteria []string, err error)
}

// promoteSummaryErrUnconverged is returned by PromoteFromIdea when the LLM
// decides the discussion never converged into an implementable feature
// (empty Markdown + "（未达成共识）" title). The handler surfaces this as a
// 422 so the frontend can show "讨论还没有达成共识" instead of a generic error.
var promoteSummaryErrUnconverged = fmt.Errorf("discussion did not converge into a concrete requirement")

// PromoteFromIdea summarizes an idea's accumulated discussion into a brand-new
// requirement row in the SAME project. The new row carries kind="requirement"
// and source_requirement_id pointing back to the original idea; the idea row
// is left fully intact (its kind, status, chat history, session id all stay)
// so the user can keep discussing or re-run the summarize after more turns.
//
// The summarizer is invoked with a payload assembled from:
//   - the idea's original description
//   - its acceptance_criteria array (analyst-accumulated bullets)
//   - its chat history from refinement_chats (user/AI turn transcript)
//
// If the LLM returns empty Markdown ("discussion didn't converge"),
// PromoteFromIdea refuses to create the new requirement and returns
// promoteSummaryErrUnconverged so the caller can render a friendly error.
func (s *RequirementService) PromoteFromIdea(sourceID string, summarizer Summarizer) (*model.Requirement, error) {
	if summarizer == nil {
		return nil, fmt.Errorf("summarizer not configured")
	}
	src, err := s.Get(sourceID)
	if err != nil {
		return nil, fmt.Errorf("source requirement not found: %w", err)
	}
	if src.Kind != KindIdea {
		return nil, fmt.Errorf("only ideas can be promoted (kind=%q); got %q", KindIdea, src.Kind)
	}

	// Pull the chat history (analyst-side, user/AI turns). Empty string means
	// "no chat yet" — the description alone is enough to summarize.
	chatJSON, _ := s.GetRefinementChat(sourceID)
	payload := assemblePromotePayload(src, chatJSON)

	markdown, title, criteria, err := summarizer.SummarizeIdeaToRequirement(payload)
	if err != nil {
		return nil, fmt.Errorf("summarize failed: %w", err)
	}

	// Empty markdown = LLM decided the discussion didn't converge. Refuse so
	// the frontend can prompt the user to keep chatting before retrying.
	if title == "（未达成共识）" || strings.TrimSpace(title) == "" || strings.TrimSpace(markdown) == "" {
		return nil, promoteSummaryErrUnconverged
	}

	// Marshal criteria back into the JSON-array-string shape the schema stores.
	criteriaJSON := "[]"
	if len(criteria) > 0 {
		raw, mErr := json.Marshal(criteria)
		if mErr == nil {
			criteriaJSON = string(raw)
		}
	}

	id := util.NewID("req")
	now := time.Now()
	// Columns (16) = VALUES list (16). 10 ? placeholders + 6 hardcoded literals
	// ('draft', 2×'[]', 'user', 2×'0'). The LLM-summarized markdown is written
	// into description so the new requirement carries the full discussion
	// conclusions (背景/目标/功能要点/备注) into the analyst-readable body; the
	// LLM-summarized criteria are written into acceptance_criteria so the new
	// requirement carries them forward; the hardcoded '[]' defaults stay for
	// design_docs / conversation_ids (the promote action does NOT carry forward
	// design state), and skip_analysis/skip_design are explicitly 0 — the new
	// requirement starts in the full pipeline, no inherited skips.
	_, err = s.db.Exec(
		"INSERT INTO requirements (id,project_id,title,description,status,priority,kind,acceptance_criteria,design_docs,conversation_ids,created_by,source_requirement_id,skip_analysis,skip_design,created_at,updated_at) VALUES (?,?,?,?,'draft',?,?,?,'[]','[]','user',?,0,0,?,?)",
		id, src.ProjectID, title, markdown, "medium", KindRequirement, criteriaJSON, sourceID, now, now)
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

// assemblePromotePayload builds the user-content string the summarizer sees:
// the original description, the analyst-accumulated acceptance_criteria, and
// the multi-turn chat transcript. Format is plain Markdown so the model can
// pull context out of it without a rigid schema. The chat-history JSON is
// best-effort — malformed payloads degrade to "no chat" rather than erroring.
func assemblePromotePayload(src *model.Requirement, chatJSON string) string {
	var sb strings.Builder
	sb.WriteString("# 原始想法描述\n\n")
	sb.WriteString(strings.TrimSpace(src.Description))
	sb.WriteString("\n\n# 讨论累积的要点\n\n")
	if strings.TrimSpace(src.AcceptanceCriteria) != "" && src.AcceptanceCriteria != "[]" {
		var bullets []string
		if json.Unmarshal([]byte(src.AcceptanceCriteria), &bullets) == nil {
			for _, b := range bullets {
				if strings.TrimSpace(b) == "" {
					continue
				}
				sb.WriteString("- ")
				sb.WriteString(b)
				sb.WriteString("\n")
			}
		}
	} else {
		sb.WriteString("（暂无）\n")
	}
	sb.WriteString("\n# 与 AI 的完整对话记录\n\n")
	if strings.TrimSpace(chatJSON) == "" || chatJSON == "[]" {
		sb.WriteString("（暂无对话）\n")
	} else {
		var msgs []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if json.Unmarshal([]byte(chatJSON), &msgs) == nil {
			for _, m := range msgs {
				if m.Role == "user" {
					sb.WriteString("\n**用户**：")
				} else if m.Role == "ai" {
					sb.WriteString("\n**AI**：")
				} else {
					sb.WriteString("\n**")
					sb.WriteString(m.Role)
					sb.WriteString("**：")
				}
				sb.WriteString(strings.TrimSpace(m.Content))
				sb.WriteString("\n")
			}
		} else {
			sb.WriteString("（对话记录解析失败，忽略）\n")
		}
	}
	return sb.String()
}

// UpdateKind switches a requirement's kind — used by the "📋 转为需求" CTA in
// the detail page when the user decides an Issue has a confirmed fix path or
// an Idea is worth promoting into a real feature. The transition is gated to
// keep the data model honest:
//
//   - Source must be issue or idea (you can't "promote" a requirement).
//   - Target must be requirement (the wizard is not currently wired for
//     issue↔idea or requirement→something-else flows).
//   - The requirement must be in a terminal state (done / archived) so we never
//     rewrite kind mid-pipeline (a partially-analysed Idea becoming a
//     Requirement would leave stale session ids / docs).
//
// The status itself is not touched — the caller is expected to follow up with
// UpdateStatus if they want to re-open the row into "draft".
func (s *RequirementService) UpdateKind(id, newKind string) (*model.Requirement, error) {
	if !ValidKind(newKind) || newKind == "" {
		return nil, fmt.Errorf("invalid kind: %q (allowed: issue, requirement, idea)", newKind)
	}
	if newKind != KindRequirement {
		return nil, fmt.Errorf("can only promote to 'requirement' (got %q)", newKind)
	}
	r, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if r.Kind != KindIssue && r.Kind != KindIdea {
		return nil, fmt.Errorf("cannot promote a requirement of kind %q", r.Kind)
	}
	if r.Status != "done" && r.Status != "archived" {
		return nil, fmt.Errorf("can only promote finished requirements (current status: %s)", r.Status)
	}
	if _, err := s.db.Exec("UPDATE requirements SET kind=?, updated_at=? WHERE id=?", newKind, time.Now(), id); err != nil {
		return nil, err
	}
	return s.Get(id)
}

func (s *RequirementService) Delete(id string) error {
	_, err := s.db.Exec("DELETE FROM requirements WHERE id = ?", id)
	return err
}

func (s *RequirementService) GetRefinementChat(reqID string) (string, error) {
	var messages string
	err := s.db.QueryRow("SELECT messages FROM refinement_chats WHERE requirement_id = ?", reqID).Scan(&messages)
	if err == sql.ErrNoRows {
		return "[]", nil
	}
	if err != nil {
		return "[]", err
	}
	return messages, nil
}

func (s *RequirementService) SaveRefinementChat(reqID, messages string) error {
	_, err := s.db.Exec(
		"INSERT INTO refinement_chats (requirement_id, messages, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)"+
			s.db.OnConflict("requirement_id", "messages = ?, updated_at = CURRENT_TIMESTAMP"),
		reqID, messages, messages)
	return err
}

// GetCodingChat returns the persisted 追加调整 chat history for a
// requirement. Returned JSON is always a non-null array string ("[]" when no
// record exists). The history is the full developer-chat conversation
// (user + AI turns in order) so the chat panel can rehydrate on refresh.
func (s *RequirementService) GetCodingChat(reqID string) (string, error) {
	var messages string
	err := s.db.QueryRow("SELECT messages FROM coding_chats WHERE requirement_id = ?", reqID).Scan(&messages)
	if err == sql.ErrNoRows {
		return "[]", nil
	}
	if err != nil {
		return "[]", err
	}
	return messages, nil
}

// SaveCodingChat upserts the 追加调整 chat history for a requirement.
// messages is a JSON array of {role,content} entries; the caller is
// responsible for JSON-encoding (the handler keeps the in-memory copy). Best-
// effort: a failure must never break the in-flight chat turn.
func (s *RequirementService) SaveCodingChat(reqID, messages string) error {
	_, err := s.db.Exec(
		"INSERT INTO coding_chats (requirement_id, messages, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)"+
			s.db.OnConflict("requirement_id", "messages = ?, updated_at = CURRENT_TIMESTAMP"),
		reqID, messages, messages)
	return err
}

// UpdateDesign persists the generated technical design and marks the architect
// phase as in-progress (designing). The "design complete" gate is a separate
// status transition driven by the user. The payload is run through
// sanitizeDesignDoc to strip a single outer ```markdown``` fence that Claude
// sometimes wraps its final plan in (see sanitizeDesignDoc).
func (s *RequirementService) UpdateDesign(id, designJSON string) (*model.Requirement, error) {
	now := time.Now()
	cleaned := sanitizeDesignDoc(designJSON)
	// "生成技术方案" flips status directly to "designing" without going through
	// UpdateStatus, so backfill analysis_started_at here too (idempotent via
	// COALESCE) — otherwise the skip-analysis design path would never record an
	// analysis start.
	_, err := s.db.Exec("UPDATE requirements SET design_docs=?, status='designing', analysis_started_at=COALESCE(analysis_started_at, ?), updated_at=? WHERE id=?", cleaned, now, now, id)
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Archive turns a finished ("done") requirement into a knowledge-base entry so
// its final requirement + design docs become reusable AI context. The
// knowledge row is keyed by (source_ref=requirement id, source_type="requirement"),
// so re-archiving the same requirement overwrites the previous entry (idempotent
// upsert). The requirement status moves to "archived".
func (s *RequirementService) Archive(id string) (*model.Knowledge, error) {
	r, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if r.Status != "done" {
		return nil, fmt.Errorf("only requirements with status 'done' can be archived (current: %s)", r.Status)
	}

	content := "# " + r.Title + "\n\n" + r.Description + "\n\n## 技术方案\n\n" + r.DesignDocs
	// Idea rows typically have no design_docs (the wizard never wrote one) —
	// drop the dangling "## 技术方案" section header so the archived knowledge
	// entry reads cleanly instead of trailing with an empty section.
	if strings.TrimSpace(r.DesignDocs) == "" || r.DesignDocs == "[]" {
		content = "# " + r.Title + "\n\n" + r.Description
	}
	now := time.Now()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("UPDATE requirements SET status='archived', updated_at=? WHERE id=?", now, id); err != nil {
		return nil, err
	}

	var existingID string
	_ = tx.QueryRow(
		"SELECT id FROM knowledge WHERE project_id=? AND source_ref=? AND source_type='requirement'",
		r.ProjectID, id).Scan(&existingID)

	if existingID != "" {
		if _, err := tx.Exec(
			"UPDATE knowledge SET title=?, content=?, updated_at=? WHERE id=?",
			r.Title, content, now, existingID); err != nil {
			return nil, err
		}
	} else {
		existingID = util.NewID("kb")
		if _, err := tx.Exec(
			"INSERT INTO knowledge (id, project_id, title, content, category, source_type, source_ref, is_reviewed, is_approved, created_at, updated_at) VALUES (?,?,?,?, 'requirement', 'requirement', ?, 1, 1, ?, ?)",
			existingID, r.ProjectID, r.Title, content, id, now, now); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	var k model.Knowledge
	err = s.db.QueryRow(
		"SELECT id, project_id, title, content, category, source_type, source_ref, is_reviewed, is_approved, created_at, updated_at FROM knowledge WHERE id=?",
		existingID).
		Scan(&k.ID, &k.ProjectID, &k.Title, &k.Content, &k.Category, &k.SourceType, &k.SourceRef, &k.IsReviewed, &k.IsApproved, &k.CreatedAt, &k.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// Unarchive reverses Archive: the requirement status returns to "done" (with a
// fresh completed_at) and the knowledge entry it produced is removed, so the
// knowledge base stays in sync with the requirement's lifecycle.
func (s *RequirementService) Unarchive(id string) (*model.Requirement, error) {
	r, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if r.Status != "archived" {
		return nil, fmt.Errorf("only archived requirements can be unarchived (current: %s)", r.Status)
	}

	now := time.Now()
	completedAt := now

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("UPDATE requirements SET status='done', updated_at=?, completed_at=? WHERE id=?", now, completedAt, id); err != nil {
		return nil, err
	}
	if _, err := tx.Exec("DELETE FROM knowledge WHERE source_ref=? AND source_type='requirement'", id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.Get(id)
}

// sanitizeDesignDoc normalises the value that gets persisted into
// requirements.design_docs so downstream rendering can rely on a clean
// Markdown payload. Two cases are handled:
//
//  1. Claude sometimes wraps a plan in a single outer markdown fence:
//     ```markdown\n# ...\n```. When the whole trimmed input matches that
//     pattern the fence is stripped — ReactMarkdown would otherwise render
//     the inner # / ## / - markers as literal text inside a code block.
//  2. Claude occasionally prefixes the fenced block with prose such as
//     "Here's the updated plan:" or "Updated plan:". When the input
//     contains exactly one fenced code block and a leading prose prefix
//     before it, only the fence content is kept (the prose is dropped —
//     it is never useful in the stored doc and would render verbatim).
//
// Legacy JSON shapes ({overview, files, ...} or ["..."]) and well-formed
// plain Markdown are returned unchanged. The detection mirrors the
// frontend's parseDesign whitelist so any change here must be kept in
// sync with frontend/src/pages/RequirementDetail.tsx parseDesign.
func sanitizeDesignDoc(raw string) string {
	if raw == "" {
		return raw
	}
	trimmed := strings.TrimSpace(raw)
	// Strict outer-fence match: ``` optional lang tag, newline, body, newline, ```.
	outer := regexp.MustCompile("^```[^\\n]*\\n([\\s\\S]*?)\\n```\\s*$")
	// First: whole input is one fence → strip directly.
	if m := outer.FindStringSubmatch(trimmed); m != nil {
		body := strings.TrimSpace(m[1])
		// Allow up to 2 levels of nesting (rare double-wrapped output).
		for i := 0; i < 2; i++ {
			mm := outer.FindStringSubmatch(body)
			if mm == nil {
				break
			}
			body = strings.TrimSpace(mm[1])
		}
		return body + "\n"
	}
	// Second: leading prose + a single trailing fence. Common Claude pattern
	// is "Here's the updated plan:\n```markdown\n...\n```". We require exactly
	// one fence pair (no inner fences that would also count as separate pairs),
	// and the prose must precede the opening fence.
	openRe := regexp.MustCompile("(?m)^```[^\\n]*\\n")
	closeRe := regexp.MustCompile("(?m)\\n```\\s*$")
	opens := openRe.FindAllStringIndex(trimmed, -1)
	closes := closeRe.FindAllStringIndex(trimmed, -1)
	if len(opens) == 1 && len(closes) == 1 && opens[0][0] < closes[0][0] {
		// The opening must be at the start of a line and the closing at the end.
		body := trimmed[opens[0][1]:closes[0][0]]
		// Drop the leading prose (everything before the fence open).
		_ = body // body already excludes the prose prefix via slice
		body = strings.TrimSpace(body)
		for i := 0; i < 2; i++ {
			mm := outer.FindStringSubmatch(body)
			if mm == nil {
				break
			}
			body = strings.TrimSpace(mm[1])
		}
		if body != "" {
			return body + "\n"
		}
	}
	return raw
}
