package service

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// CodeReviewService persists Review sessions (code_reviews). A session is
// created in `draft` so the user can hang a description + attachments off a
// stable owner id BEFORE any claude process starts; StartReviewSession then
// flips it to `running` and the stream tail writes the terminal row.
//
// Both review kinds live here: the branch Review the user composes by hand
// (review_type='branch') and the legacy "review this open PR" entry point
// (review_type='pr'), so the history list is a single query per project.
//
// Raw SQL with `?` placeholders through the *db.DB wrapper, per the
// repo-wide service convention (the wrapper rebinds to $N on Postgres).
type CodeReviewService struct {
	db *db.DB
	// attachSvc is the review-scoped attachment store. Delete() walks it so
	// deleting a session also reclaims the on-disk bytes — the FK CASCADE
	// only cleans the rows. nil is tolerated (row-only delete).
	attachSvc *AttachmentService
}

func NewCodeReviewService(database *db.DB, attachSvc *AttachmentService) *CodeReviewService {
	return &CodeReviewService{db: database, attachSvc: attachSvc}
}

const codeReviewCols = `id, project_id, review_type, branch, base_branch, pr_number,
	title, description, status, job_id, result_md, findings_json,
	p0_count, p1_count, p3_count, goal_met, model, claude_config_id,
	agent_server_id, created_by, created_at, updated_at, finished_at`

// Create inserts a draft session. branch is required for branch reviews;
// PR reviews carry pr_number instead and may leave branch blank only when
// the caller genuinely has no head branch (the prompt then falls back to
// the PR number alone).
func (s *CodeReviewService) Create(rv *model.CodeReview) (*model.CodeReview, error) {
	if rv == nil {
		return nil, errors.New("review is required")
	}
	if strings.TrimSpace(rv.ProjectID) == "" {
		return nil, errors.New("project_id is required")
	}
	if rv.ReviewType == "" {
		rv.ReviewType = model.ReviewTypeBranch
	}
	if rv.ReviewType == model.ReviewTypeBranch && strings.TrimSpace(rv.Branch) == "" {
		return nil, errors.New("branch is required for a branch review")
	}
	id := util.NewID("crv")
	now := time.Now().UTC()
	_, err := s.db.Exec(`INSERT INTO code_reviews (
		id, project_id, review_type, branch, base_branch, pr_number,
		title, description, status, job_id, result_md, findings_json,
		p0_count, p1_count, p3_count, goal_met, model, claude_config_id,
		agent_server_id, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '', 0, 0, 0, '', ?, ?, ?, ?, ?, ?)`,
		id, rv.ProjectID, rv.ReviewType, rv.Branch, rv.BaseBranch, rv.PRNumber,
		rv.Title, rv.Description, model.ReviewStatusDraft,
		rv.Model, rv.ClaudeConfigID, rv.AgentServerID, rv.CreatedBy, now, now)
	if err != nil {
		return nil, fmt.Errorf("insert code_review: %w", err)
	}
	return s.Get(id)
}

// Get returns one session. sql.ErrNoRows when missing — handlers map it to
// 404 REVIEW_NOT_FOUND.
func (s *CodeReviewService) Get(id string) (*model.CodeReview, error) {
	row := s.db.QueryRow(`SELECT `+codeReviewCols+` FROM code_reviews WHERE id = ?`, id)
	return scanCodeReview(row)
}

// ListByProject returns the project's sessions newest-first. limit <= 0
// falls back to 50 (the history panel's page size).
func (s *CodeReviewService) ListByProject(projectID string, limit int) ([]model.CodeReview, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+codeReviewCols+`
		FROM code_reviews WHERE project_id = ?
		ORDER BY created_at DESC, id DESC LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.CodeReview{}
	for rows.Next() {
		rv, err := scanCodeReview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rv)
	}
	return out, rows.Err()
}

// Update patches the user-editable fields (description / branches / title,
// plus the model / claude-config / agent-server bindings from the toolbar).
// Refused while the session is running — the prompt has already been built
// at that point, so accepting an edit would silently lie about what was
// reviewed. draft and the two terminal states are both editable (editing a
// finished session is how a user sets up a re-run).
func (s *CodeReviewService) Update(id string, rv *model.CodeReview) (*model.CodeReview, error) {
	cur, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if cur.Status == model.ReviewStatusRunning {
		return nil, errors.New("review 正在运行，暂不可修改")
	}
	if rv.ReviewType == model.ReviewTypeBranch && strings.TrimSpace(rv.Branch) == "" {
		rv.Branch = cur.Branch
	}
	_, err = s.db.Exec(`UPDATE code_reviews
		SET branch = ?, base_branch = ?, title = ?, description = ?,
		    model = ?, claude_config_id = ?, agent_server_id = ?, updated_at = ?
		WHERE id = ?`,
		rv.Branch, rv.BaseBranch, rv.Title, rv.Description,
		rv.Model, rv.ClaudeConfigID, rv.AgentServerID, time.Now().UTC(), id)
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

// MarkRunning stamps the job binding + effective model and clears the
// previous run's result so a re-run doesn't show stale findings while the
// new stream is still going.
func (s *CodeReviewService) MarkRunning(id, jobID, modelName string) error {
	_, err := s.db.Exec(`UPDATE code_reviews
		SET status = ?, job_id = ?, model = ?, result_md = '', findings_json = '',
		    p0_count = 0, p1_count = 0, p3_count = 0, goal_met = '',
		    finished_at = NULL, updated_at = ?
		WHERE id = ?`,
		model.ReviewStatusRunning, jobID, modelName, time.Now().UTC(), id)
	return err
}

// ReviewOutcome is the terminal payload written by the stream tail.
type ReviewOutcome struct {
	Status       string // model.ReviewStatusDone | ReviewStatusError
	ResultMD     string
	FindingsJSON string
	P0, P1, P3   int
	GoalMet      string
}

// MarkFinished writes the terminal row. A parse miss is not a failure: the
// caller passes the full report as ResultMD with empty FindingsJSON and zero
// counts, and the UI degrades to plain-Markdown rendering.
func (s *CodeReviewService) MarkFinished(id string, out ReviewOutcome) error {
	if out.Status == "" {
		out.Status = model.ReviewStatusDone
	}
	now := time.Now().UTC()
	_, err := s.db.Exec(`UPDATE code_reviews
		SET status = ?, result_md = ?, findings_json = ?,
		    p0_count = ?, p1_count = ?, p3_count = ?, goal_met = ?,
		    finished_at = ?, updated_at = ?
		WHERE id = ?`,
		out.Status, out.ResultMD, out.FindingsJSON,
		out.P0, out.P1, out.P3, out.GoalMet, now, now, id)
	return err
}

// Delete drops the session plus every attachment it owns — rows AND the
// on-disk files. The FK CASCADE would handle the rows alone, which would
// leave orphan bytes under ~/.novaworkbench/attachments/<crv_id>/, so the
// explicit walk comes first. Attachment failures are not fatal: a file that
// is already gone must not block deleting the session.
func (s *CodeReviewService) Delete(id string) error {
	if s.attachSvc != nil {
		atts, err := s.attachSvc.List(id)
		if err == nil {
			for i := range atts {
				_ = s.attachSvc.Delete(atts[i].ID)
			}
		}
	}
	_, err := s.db.Exec(`DELETE FROM code_reviews WHERE id = ?`, id)
	return err
}

// rowScanner unifies *sql.Row and *sql.Rows for scanCodeReview.
type rowScanner interface{ Scan(dest ...any) error }

func scanCodeReview(row rowScanner) (*model.CodeReview, error) {
	var rv model.CodeReview
	var finishedAt sql.NullTime
	var createdAt, updatedAt time.Time
	if err := row.Scan(&rv.ID, &rv.ProjectID, &rv.ReviewType, &rv.Branch,
		&rv.BaseBranch, &rv.PRNumber, &rv.Title, &rv.Description, &rv.Status,
		&rv.JobID, &rv.ResultMD, &rv.FindingsJSON, &rv.P0Count, &rv.P1Count,
		&rv.P3Count, &rv.GoalMet, &rv.Model, &rv.ClaudeConfigID,
		&rv.AgentServerID, &rv.CreatedBy,
		&createdAt, &updatedAt, &finishedAt); err != nil {
		return nil, err
	}
	rv.CreatedAt = createdAt
	rv.UpdatedAt = updatedAt
	if finishedAt.Valid {
		t := finishedAt.Time
		rv.FinishedAt = &t
	}
	return &rv, nil
}
