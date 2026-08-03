package service

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

type RequirementService struct {
	db *sql.DB
}

func NewRequirementService(db *sql.DB) *RequirementService {
	return &RequirementService{db: db}
}

// Valid status transitions — two-role stage-gate lifecycle:
// draft → analyzing → designing → designed → developing → done
// (any state → archived). Each gate is completed by a manual user action.
// The analyst chat happens during "analyzing"; proceeding to architect-design
// transitions directly to "designing" (no separate "analyzed" finalization).
var validTransitions = map[string][]string{
	"draft":      {"analyzing", "archived"},
	"analyzing":  {"designing", "draft", "archived"},
	"designing":  {"designed", "analyzing", "archived"},
	"designed":   {"developing", "archived"},
	"developing": {"done", "designed", "archived"},
	"done":       {"archived"},
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
		"SELECT id,project_id,title,description,status,priority,acceptance_criteria,design_docs,conversation_ids,assigned_to,sprint,created_by,analysis_session_id,design_session_id,coding_session_id,created_at,updated_at,completed_at FROM requirements "+where+" ORDER BY updated_at DESC",
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
			&r.CreatedBy, &r.AnalysisSessionID, &r.DesignSessionID, &r.CodingSessionID, &r.CreatedAt, &r.UpdatedAt, &r.CompletedAt); err != nil {
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
		"SELECT id,project_id,title,description,status,priority,acceptance_criteria,design_docs,conversation_ids,assigned_to,sprint,created_by,analysis_session_id,design_session_id,coding_session_id,created_at,updated_at,completed_at FROM requirements WHERE id = ?", id).
		Scan(&r.ID, &r.ProjectID, &r.Title, &r.Description, &r.Status, &r.Priority,
			&r.AcceptanceCriteria, &r.DesignDocs, &r.ConversationIDs, &r.AssignedTo, &r.Sprint,
			&r.CreatedBy, &r.AnalysisSessionID, &r.DesignSessionID, &r.CodingSessionID, &r.CreatedAt, &r.UpdatedAt, &r.CompletedAt)
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
	now := time.Now()

	_, err := s.db.Exec(
		"INSERT INTO requirements (id,project_id,title,description,status,priority,acceptance_criteria,design_docs,conversation_ids,sprint,created_by,created_at,updated_at) VALUES (?,?,?,?,'draft',?,'[]','[]','[]',?,'user',?,?)",
		id, req.ProjectID, req.Title, req.Description, req.Priority, req.Sprint, now, now)
	if err != nil {
		return nil, err
	}

	return s.Get(id)
}

func (s *RequirementService) Update(id string, req model.CreateRequirementReq) (*model.Requirement, error) {
	_, err := s.db.Exec(
		"UPDATE requirements SET title=?, description=?, priority=?, sprint=?, updated_at=? WHERE id=?",
		req.Title, req.Description, req.Priority, req.Sprint, time.Now(), id)
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

// UpdateCodingSession persists the claude CLI session id for the developer
// conversation (a fork off the design session). Subsequent coding turns resume it.
func (s *RequirementService) UpdateCodingSession(id, sessionID string) error {
	_, err := s.db.Exec("UPDATE requirements SET coding_session_id=?, updated_at=? WHERE id=?",
		sessionID, time.Now(), id)
	return err
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
		"INSERT INTO refinement_chats (requirement_id, messages, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP) ON CONFLICT(requirement_id) DO UPDATE SET messages = ?, updated_at = CURRENT_TIMESTAMP",
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
