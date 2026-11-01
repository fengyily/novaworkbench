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

// RequirementQAService persists the 阶段答疑 (stage Q&A) threads hanging off a
// requirement's design / coding session.
//
// Why a table instead of keeping the Q&A in the JobStore: jobs are an
// in-memory ring buffer (cap 50, wiped on restart), while a Q&A thread about
// "why did you design it this way" is something users come back to days later.
// The thread has to survive both.
//
// One row per question-and-answer turn (a "turn table", mirroring
// CodeReviewFollowupService) rather than a single transcript blob: the answer
// is produced by a background job, so the backend — not the browser — has to
// persist it, and status / job_id / resumed are inherently per-turn facts.
//
// Raw SQL with `?` placeholders through the *db.DB wrapper, per the repo-wide
// service convention (the wrapper rebinds to $N on Postgres).
type RequirementQAService struct {
	db *db.DB
}

func NewRequirementQAService(database *db.DB) *RequirementQAService {
	return &RequirementQAService{db: database}
}

const qaTurnCols = `id, requirement_id, stage, question, answer, status, job_id,
	model, session_id, source_session_id, resumed, created_by,
	created_at, updated_at, finished_at`

// Create inserts a turn already in `running`: the question is submitted and
// dispatched in a single request, so there is no draft window.
//
// SessionID is the pre-minted QA session id (the id the CLI is asked to use
// via --session-id). It is persisted BEFORE the run starts so a crash between
// "spawned" and "answered" still leaves a row that names the session the
// (possibly half-written) transcript belongs to.
func (s *RequirementQAService) Create(t *model.RequirementQATurn) (*model.RequirementQATurn, error) {
	if t == nil {
		return nil, errors.New("qa turn is required")
	}
	if strings.TrimSpace(t.RequirementID) == "" {
		return nil, errors.New("requirement_id is required")
	}
	if strings.TrimSpace(t.Question) == "" {
		return nil, errors.New("question is required")
	}
	stage := t.Stage
	if stage != model.QAStageCoding {
		stage = model.QAStageDesign
	}
	id := util.NewID("qat")
	now := time.Now().UTC()
	_, err := s.db.Exec(`INSERT INTO requirement_qa_turns (
		id, requirement_id, stage, question, answer, status, job_id, model,
		session_id, source_session_id, resumed, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, '', ?, ?, ?, ?, ?, 0, ?, ?, ?)`,
		id, t.RequirementID, stage, t.Question, model.QAStatusRunning,
		t.JobID, t.Model, t.SessionID, t.SourceSessionID, t.CreatedBy, now, now)
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Get returns one turn. sql.ErrNoRows when missing.
func (s *RequirementQAService) Get(id string) (*model.RequirementQATurn, error) {
	row := s.db.QueryRow(`SELECT `+qaTurnCols+` FROM requirement_qa_turns WHERE id = ?`, id)
	return scanQATurn(row)
}

// ListByStage returns one stage's thread oldest-first — it reads as a
// conversation, not as a history table.
func (s *RequirementQAService) ListByStage(requirementID, stage string) ([]model.RequirementQATurn, error) {
	rows, err := s.db.Query(`SELECT `+qaTurnCols+`
		FROM requirement_qa_turns WHERE requirement_id = ? AND stage = ?
		ORDER BY created_at ASC, id ASC`, requirementID, stage)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.RequirementQATurn{}
	for rows.Next() {
		t, err := scanQATurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// LatestTurn returns the most recently created turn of a stage, regardless of
// its status. It is the input to the concurrency gate: a still-running turn
// blocks the next question.
//
// sql.ErrNoRows when the thread is empty.
func (s *RequirementQAService) LatestTurn(requirementID, stage string) (*model.RequirementQATurn, error) {
	row := s.db.QueryRow(`SELECT `+qaTurnCols+`
		FROM requirement_qa_turns WHERE requirement_id = ? AND stage = ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, requirementID, stage)
	return scanQATurn(row)
}

// LatestAnsweredTurn returns the most recent turn that carries a usable
// answer — i.e. the newest turn that is not in the error state. It is the
// input to the session resolution: that turn's SessionID is the QA session to
// resume, and its SourceSessionID is the snapshot the current stage session
// is compared against to detect "the user re-ran the stage, so fork again".
//
// Errored turns are skipped on purpose — a failed turn may have left its
// session half-written, and resuming that would be worse than resuming the
// last turn that actually completed.
//
// sql.ErrNoRows when the thread has no answered turn yet.
func (s *RequirementQAService) LatestAnsweredTurn(requirementID, stage string) (*model.RequirementQATurn, error) {
	row := s.db.QueryRow(`SELECT `+qaTurnCols+`
		FROM requirement_qa_turns WHERE requirement_id = ? AND stage = ? AND status <> ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, requirementID, stage, model.QAStatusError)
	return scanQATurn(row)
}

// MarkFinished writes the terminal row. An error run stores the failure text
// in answer so the thread still shows what went wrong instead of a blank
// bubble.
//
// sessionID / sourceSessionID are the terminal values of the turn's session
// bookkeeping — they differ from the pre-minted values when the run degraded
// to a fresh session (source becomes "") or when the CLI reported a session id
// other than the one we asked for.
func (s *RequirementQAService) MarkFinished(id, status, answer, modelName, sessionID, sourceSessionID string, resumed bool) error {
	if status == "" {
		status = model.QAStatusDone
	}
	now := time.Now().UTC()
	_, err := s.db.Exec(`UPDATE requirement_qa_turns
		SET status = ?, answer = ?, model = ?, session_id = ?, source_session_id = ?,
		    resumed = ?, finished_at = ?, updated_at = ?
		WHERE id = ?`,
		status, answer, modelName, sessionID, sourceSessionID, boolToInt(resumed), now, now, id)
	return err
}

// MarkOrphaned flips a stuck `running` row to `error`.
//
// A turn is marked running before the claude process is spawned, so a backend
// restart (or a JobStore eviction) leaves a row that will never be answered.
// Rather than blocking the thread forever, the read/ask entry points repair
// such rows lazily: the user sees "后端已重启，请重新提问" instead of an
// eternal spinner.
func (s *RequirementQAService) MarkOrphaned(id, message string) error {
	if strings.TrimSpace(message) == "" {
		message = "后端已重启，请重新提问"
	}
	now := time.Now().UTC()
	_, err := s.db.Exec(`UPDATE requirement_qa_turns
		SET status = ?, answer = ?, finished_at = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		model.QAStatusError, message, now, now, id, model.QAStatusRunning)
	return err
}

// DeleteTurn removes one turn from the thread.
func (s *RequirementQAService) DeleteTurn(id string) error {
	_, err := s.db.Exec(`DELETE FROM requirement_qa_turns WHERE id = ?`, id)
	return err
}

// ResetThread removes every turn of one stage — the "清空答疑记录" action.
func (s *RequirementQAService) ResetThread(requirementID, stage string) error {
	_, err := s.db.Exec(`DELETE FROM requirement_qa_turns WHERE requirement_id = ? AND stage = ?`, requirementID, stage)
	return err
}

// DeleteByRequirement removes every turn of every stage. Called when the
// requirement itself is deleted: the schema declares ON DELETE CASCADE, but
// SQLite never enables PRAGMA foreign_keys, so the cascade silently no-ops
// and the rows would be orphaned.
func (s *RequirementQAService) DeleteByRequirement(requirementID string) error {
	_, err := s.db.Exec(`DELETE FROM requirement_qa_turns WHERE requirement_id = ?`, requirementID)
	return err
}

func scanQATurn(row rowScanner) (*model.RequirementQATurn, error) {
	var t model.RequirementQATurn
	var resumed int
	var finishedAt sql.NullTime
	var createdAt, updatedAt time.Time
	if err := row.Scan(&t.ID, &t.RequirementID, &t.Stage, &t.Question, &t.Answer, &t.Status,
		&t.JobID, &t.Model, &t.SessionID, &t.SourceSessionID, &resumed, &t.CreatedBy,
		&createdAt, &updatedAt, &finishedAt); err != nil {
		return nil, err
	}
	t.Resumed = resumed != 0
	t.CreatedAt = createdAt
	t.UpdatedAt = updatedAt
	if finishedAt.Valid {
		ft := finishedAt.Time
		t.FinishedAt = &ft
	}
	return &t, nil
}
