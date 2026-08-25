package service

import (
	"database/sql"

	"encoding/json"
	"fmt"
	"github.com/novaworkbench/backend/internal/db"
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
	where := "WHERE 1=1"
	args := []interface{}{}

	if projectID != "" {
		where += " AND project_id = ?"
		args = append(args, projectID)
	}
	if status != "" {
		if status == "active" {
			where += " AND status IN ('draft','analyzing','designing','designed','developing')"
		} else if status != "archived" {
			where += " AND status = ?"
			args = append(args, status)
		}
	} else {
		where += " AND status != 'archived'"
	}
	if priority != "" {
		where += " AND priority = ?"
		args = append(args, priority)
	}
	// kind filter — empty (or "all") means no filtering; a comma-separated list
	// (e.g. "issue,idea") becomes an IN clause for cross-project listings.
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

	rows, err := s.db.Query(
		"SELECT id,project_id,title,description,status,priority,kind,acceptance_criteria,design_docs,conversation_ids,assigned_to,created_by,source_requirement_id,analysis_session_id,design_session_id,design_job_id,analysis_job_id,apply_job_id,coding_session_id,skip_analysis,skip_design,branch_name,worktree_path,analyst_model,architect_model,developer_model,reviewer_model,created_at,updated_at,completed_at FROM requirements "+where+" ORDER BY CASE WHEN status = 'done' THEN 1 ELSE 0 END ASC, created_at DESC",
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
			&r.CreatedAt, &r.UpdatedAt, &r.CompletedAt); err != nil {
			return nil, err
		}
		items = append(items, r)
	}
	if items == nil {
		items = []model.Requirement{}
	}
	return items, nil
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
	err := s.db.QueryRow(
		"SELECT id,project_id,title,description,status,priority,kind,acceptance_criteria,design_docs,conversation_ids,assigned_to,created_by,source_requirement_id,analysis_session_id,design_session_id,design_job_id,analysis_job_id,apply_job_id,coding_session_id,skip_analysis,skip_design,branch_name,worktree_path,analyst_model,architect_model,developer_model,reviewer_model,created_at,updated_at,completed_at FROM requirements WHERE id = ?", id).
		Scan(&r.ID, &r.ProjectID, &r.Title, &r.Description, &r.Status, &r.Priority, &r.Kind,
			&r.AcceptanceCriteria, &r.DesignDocs, &r.ConversationIDs, &r.AssignedTo,
			&r.CreatedBy, &r.SourceRequirementID, &r.AnalysisSessionID, &r.DesignSessionID, &r.DesignJobID, &r.AnalysisJobID, &r.ApplyJobID, &r.CodingSessionID, &r.SkipAnalysis, &r.SkipDesign, &r.BranchName, &r.WorktreePath,
			&r.AnalystModel, &r.ArchitectModel, &r.DeveloperModel, &r.ReviewerModel,
			&r.CreatedAt, &r.UpdatedAt, &r.CompletedAt)
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

	_, err = s.db.Exec("UPDATE requirements SET status=?, updated_at=?, completed_at=? WHERE id=?", newStatus, now, completedAt, id)
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

// UpdateWorktree persists the dev branch name and the absolute path of the
// isolated git worktree created for parallel development. Pass "" for both to
// clear (after a merge/cleanup) so later stages fall back to the shared project
// checkout instead of a stale worktree path.
func (s *RequirementService) UpdateWorktree(id, branch, path string) error {
	_, err := s.db.Exec("UPDATE requirements SET branch_name=?, worktree_path=?, updated_at=? WHERE id=?",
		branch, path, time.Now(), id)
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
// status transition driven by the user.
func (s *RequirementService) UpdateDesign(id, designJSON string) (*model.Requirement, error) {
	now := time.Now()
	_, err := s.db.Exec("UPDATE requirements SET design_docs=?, status='designing', updated_at=? WHERE id=?", designJSON, now, id)
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
