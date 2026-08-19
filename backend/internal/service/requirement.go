package service

import (
	"database/sql"

	"fmt"
	"github.com/novaworkbench/backend/internal/db"
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

// Valid status transitions — two-role stage-gate lifecycle:
// draft → analyzing → designing → designed → developing → done
// (any state → archived). Each gate is completed by a manual user action.
// The analyst chat happens during "analyzing"; proceeding to architect-design
// transitions directly to "designing" (no separate "analyzed" finalization).
// "draft → designing" is the skip-analysis path: when a requirement has
// skip_analysis=true the user goes straight to architect-design without an
// analyst conversation.
var validTransitions = map[string][]string{
	"draft":      {"analyzing", "designing", "archived"},
	"analyzing":  {"designing", "draft", "archived"},
	"designing":  {"designed", "analyzing", "archived"},
	"designed":   {"developing", "archived"},
	"developing": {"done", "designed", "archived"},
	"done":       {"archived"},
	// archived is reversible: unarchive restores the requirement to "done" and
	// removes the knowledge entry it produced when archived.
	"archived": {"done"},
}

func (s *RequirementService) List(projectID string, status string, priority string, sprint string) ([]model.Requirement, error) {
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
	if sprint != "" {
		where += " AND sprint = ?"
		args = append(args, sprint)
	}

	rows, err := s.db.Query(
		"SELECT id,project_id,title,description,status,priority,acceptance_criteria,design_docs,conversation_ids,assigned_to,sprint,created_by,analysis_session_id,design_session_id,design_job_id,analysis_job_id,apply_job_id,coding_session_id,skip_analysis,branch_name,worktree_path,analyst_model,architect_model,developer_model,reviewer_model,created_at,updated_at,completed_at FROM requirements "+where+" ORDER BY CASE WHEN status = 'done' THEN 1 ELSE 0 END ASC, created_at DESC",
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []model.Requirement
	for rows.Next() {
		var r model.Requirement
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Title, &r.Description, &r.Status, &r.Priority,
			&r.AcceptanceCriteria, &r.DesignDocs, &r.ConversationIDs, &r.AssignedTo, &r.Sprint,
			&r.CreatedBy, &r.AnalysisSessionID, &r.DesignSessionID, &r.DesignJobID, &r.AnalysisJobID, &r.ApplyJobID, &r.CodingSessionID, &r.SkipAnalysis, &r.BranchName, &r.WorktreePath,
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

func (s *RequirementService) Get(id string) (*model.Requirement, error) {
	var r model.Requirement
	err := s.db.QueryRow(
		"SELECT id,project_id,title,description,status,priority,acceptance_criteria,design_docs,conversation_ids,assigned_to,sprint,created_by,analysis_session_id,design_session_id,design_job_id,analysis_job_id,apply_job_id,coding_session_id,skip_analysis,branch_name,worktree_path,analyst_model,architect_model,developer_model,reviewer_model,created_at,updated_at,completed_at FROM requirements WHERE id = ?", id).
		Scan(&r.ID, &r.ProjectID, &r.Title, &r.Description, &r.Status, &r.Priority,
			&r.AcceptanceCriteria, &r.DesignDocs, &r.ConversationIDs, &r.AssignedTo, &r.Sprint,
			&r.CreatedBy, &r.AnalysisSessionID, &r.DesignSessionID, &r.DesignJobID, &r.AnalysisJobID, &r.ApplyJobID, &r.CodingSessionID, &r.SkipAnalysis, &r.BranchName, &r.WorktreePath,
			&r.AnalystModel, &r.ArchitectModel, &r.DeveloperModel, &r.ReviewerModel,
			&r.CreatedAt, &r.UpdatedAt, &r.CompletedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("requirement not found")
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *RequirementService) Create(req model.CreateRequirementReq) (*model.Requirement, error) {
	id := util.NewID("req")
	if req.Priority == "" {
		req.Priority = "medium"
	}
	// Default to skip-analysis (true) when the caller omits the field, so the
	// "default skip" product decision holds even for clients that don't send it.
	skipAnalysis := true
	if req.SkipAnalysis != nil {
		skipAnalysis = *req.SkipAnalysis
	}
	now := time.Now()

	_, err := s.db.Exec(
		"INSERT INTO requirements (id,project_id,title,description,status,priority,acceptance_criteria,design_docs,conversation_ids,sprint,created_by,skip_analysis,created_at,updated_at) VALUES (?,?,?,?,'draft',?,'[]','[]','[]',?,'user',?,?,?)",
		id, req.ProjectID, req.Title, req.Description, req.Priority, req.Sprint, skipAnalysis, now, now)
	if err != nil {
		return nil, err
	}

	return s.Get(id)
}

func (s *RequirementService) Update(id string, req model.CreateRequirementReq) (*model.Requirement, error) {
	// skip_analysis is a *bool: nil preserves the stored value (COALESCE keeps
	// the existing column when the param is NULL), a non-nil pointer updates it.
	// This lets the edit modal toggle the flag while other callers that only
	// touch title/description/priority/sprint leave it untouched.
	var skipArg interface{}
	if req.SkipAnalysis != nil {
		skipArg = *req.SkipAnalysis
	}
	_, err := s.db.Exec(
		"UPDATE requirements SET title=?, description=?, priority=?, sprint=?, skip_analysis=COALESCE(?,skip_analysis), updated_at=? WHERE id=?",
		req.Title, req.Description, req.Priority, req.Sprint, skipArg, time.Now(), id)
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
