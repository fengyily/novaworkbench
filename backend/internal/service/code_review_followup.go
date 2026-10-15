package service

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// CodeReviewFollowupService persists the 追问 thread hanging off a finished
// Review (code_review_followups). One row per question, carrying both the
// question and the model's answer.
//
// Why a table instead of keeping the Q&A in the JobStore: jobs are an
// in-memory ring buffer (cap 50, wiped on restart), while a review report is
// something users come back to days later. The thread has to survive both.
//
// Raw SQL with `?` placeholders through the *db.DB wrapper, per the
// repo-wide service convention (the wrapper rebinds to $N on Postgres).
type CodeReviewFollowupService struct {
	db *db.DB
}

func NewCodeReviewFollowupService(database *db.DB) *CodeReviewFollowupService {
	return &CodeReviewFollowupService{db: database}
}

const followupCols = `id, review_id, question, answer, status, job_id, model,
	resumed, created_by, created_at, updated_at, finished_at`

// Create inserts a follow-up already in `running`: the question is submitted
// and dispatched in a single request, so there is no draft window.
func (s *CodeReviewFollowupService) Create(fu *model.CodeReviewFollowup) (*model.CodeReviewFollowup, error) {
	if fu == nil {
		return nil, errors.New("followup is required")
	}
	if strings.TrimSpace(fu.ReviewID) == "" {
		return nil, errors.New("review_id is required")
	}
	if strings.TrimSpace(fu.Question) == "" {
		return nil, errors.New("question is required")
	}
	id := util.NewID("crf")
	now := time.Now().UTC()
	_, err := s.db.Exec(`INSERT INTO code_review_followups (
		id, review_id, question, answer, status, job_id, model, resumed,
		created_by, created_at, updated_at)
		VALUES (?, ?, ?, '', ?, ?, ?, 0, ?, ?, ?)`,
		id, fu.ReviewID, fu.Question, model.FollowupStatusRunning,
		fu.JobID, fu.Model, fu.CreatedBy, now, now)
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Get returns one follow-up. sql.ErrNoRows when missing.
func (s *CodeReviewFollowupService) Get(id string) (*model.CodeReviewFollowup, error) {
	row := s.db.QueryRow(`SELECT `+followupCols+` FROM code_review_followups WHERE id = ?`, id)
	return scanFollowup(row)
}

// ListByReview returns the thread oldest-first — it reads as a conversation,
// not as a history table.
func (s *CodeReviewFollowupService) ListByReview(reviewID string) ([]model.CodeReviewFollowup, error) {
	rows, err := s.db.Query(`SELECT `+followupCols+`
		FROM code_review_followups WHERE review_id = ?
		ORDER BY created_at ASC, id ASC`, reviewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.CodeReviewFollowup{}
	for rows.Next() {
		fu, err := scanFollowup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *fu)
	}
	return out, rows.Err()
}

// MarkFinished writes the terminal row. An error run stores the failure text
// in answer so the thread still shows what went wrong instead of a blank
// bubble.
func (s *CodeReviewFollowupService) MarkFinished(id, status, answer, modelName string, resumed bool) error {
	if status == "" {
		status = model.FollowupStatusDone
	}
	now := time.Now().UTC()
	_, err := s.db.Exec(`UPDATE code_review_followups
		SET status = ?, answer = ?, model = ?, resumed = ?, finished_at = ?, updated_at = ?
		WHERE id = ?`,
		status, answer, modelName, boolToInt(resumed), now, now, id)
	return err
}

// Delete removes one follow-up from the thread.
func (s *CodeReviewFollowupService) Delete(id string) error {
	_, err := s.db.Exec(`DELETE FROM code_review_followups WHERE id = ?`, id)
	return err
}

func scanFollowup(row rowScanner) (*model.CodeReviewFollowup, error) {
	var fu model.CodeReviewFollowup
	var resumed int
	var finishedAt sql.NullTime
	var createdAt, updatedAt time.Time
	if err := row.Scan(&fu.ID, &fu.ReviewID, &fu.Question, &fu.Answer, &fu.Status,
		&fu.JobID, &fu.Model, &resumed, &fu.CreatedBy,
		&createdAt, &updatedAt, &finishedAt); err != nil {
		return nil, err
	}
	fu.Resumed = resumed != 0
	fu.CreatedAt = createdAt
	fu.UpdatedAt = updatedAt
	if finishedAt.Valid {
		t := finishedAt.Time
		fu.FinishedAt = &t
	}
	return &fu, nil
}
