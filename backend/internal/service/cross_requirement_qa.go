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

// CrossRequirementQAService persists the 方案调整对话 (design-adjustment
// thread) hanging off a cross-project requirement's overall design.
//
// Why a table instead of keeping the thread in the JobStore: jobs are an
// in-memory ring buffer (cap 50, wiped on restart), while "the decisions we
// settled before re-completing the plan" is something users come back to days
// later — and, more importantly, it is **input to the next design run**: the
// overall-design job reads this thread to know which conclusions to fold
// into the revised plan. That read has to survive a restart.
//
// One row per question-and-answer turn (a "turn table", mirroring
// RequirementQAService) rather than a single transcript blob: the answer is
// produced by a background job, so the backend — not the browser — has to
// persist it, and status / job_id / resumed are inherently per-turn facts.
//
// Raw SQL with `?` placeholders through the *db.DB wrapper, per the repo-wide
// service convention (the wrapper rebinds to $N on Postgres).
type CrossRequirementQAService struct {
	db *db.DB
}

func NewCrossRequirementQAService(database *db.DB) *CrossRequirementQAService {
	return &CrossRequirementQAService{db: database}
}

const crossQATurnCols = `id, cross_requirement_id, question, answer, status, job_id,
	model, session_id, source_session_id, resumed, error_kind, created_by,
	created_at, updated_at, finished_at`

// Create inserts a turn already in `running`: the question is submitted and
// dispatched in a single request, so there is no draft window.
//
// SessionID is the pre-minted session id (the id the CLI is asked to use via
// --session-id / --fork-session). It is persisted BEFORE the run starts so a
// crash between "spawned" and "answered" still leaves a row that names the
// session the (possibly half-written) transcript belongs to.
func (s *CrossRequirementQAService) Create(t *model.CrossRequirementQATurn) (*model.CrossRequirementQATurn, error) {
	if t == nil {
		return nil, errors.New("adjustment turn is required")
	}
	if strings.TrimSpace(t.CrossRequirementID) == "" {
		return nil, errors.New("cross_requirement_id is required")
	}
	if strings.TrimSpace(t.Question) == "" {
		return nil, errors.New("question is required")
	}
	id := util.NewID("xqat")
	now := time.Now().UTC()
	_, err := s.db.Exec(`INSERT INTO cross_requirement_qa_turns (
		id, cross_requirement_id, question, answer, status, job_id, model,
		session_id, source_session_id, resumed, created_by, created_at, updated_at)
		VALUES (?, ?, ?, '', ?, ?, ?, ?, ?, 0, ?, ?, ?)`,
		id, t.CrossRequirementID, t.Question, model.QAStatusRunning,
		t.JobID, t.Model, t.SessionID, t.SourceSessionID, t.CreatedBy, now, now)
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Get returns one turn. sql.ErrNoRows when missing.
func (s *CrossRequirementQAService) Get(id string) (*model.CrossRequirementQATurn, error) {
	row := s.db.QueryRow(`SELECT `+crossQATurnCols+` FROM cross_requirement_qa_turns WHERE id = ?`, id)
	return scanCrossQATurn(row)
}

// GetByJobID returns the turn that belongs to a JobStore job.
//
// The SSE terminal frame needs it: the frame must name the turn it is
// reporting on. The job id is not the turn id (they are different id
// spaces), so the lookup goes through the job_id column.
//
// sql.ErrNoRows when the job has no turn.
func (s *CrossRequirementQAService) GetByJobID(jobID string) (*model.CrossRequirementQATurn, error) {
	if strings.TrimSpace(jobID) == "" {
		return nil, sql.ErrNoRows
	}
	row := s.db.QueryRow(`SELECT `+crossQATurnCols+`
		FROM cross_requirement_qa_turns WHERE job_id = ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, jobID)
	return scanCrossQATurn(row)
}

// List returns a cross requirement's whole thread oldest-first — it reads as
// a conversation, not as a history table.
func (s *CrossRequirementQAService) List(crossRequirementID string) ([]model.CrossRequirementQATurn, error) {
	rows, err := s.db.Query(`SELECT `+crossQATurnCols+`
		FROM cross_requirement_qa_turns WHERE cross_requirement_id = ?
		ORDER BY created_at ASC, id ASC`, crossRequirementID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.CrossRequirementQATurn{}
	for rows.Next() {
		t, err := scanCrossQATurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// LatestTurn returns the most recently created turn, regardless of its
// status. It is the input to the concurrency gate: a still-running turn
// blocks the next question.
//
// sql.ErrNoRows when the thread is empty.
func (s *CrossRequirementQAService) LatestTurn(crossRequirementID string) (*model.CrossRequirementQATurn, error) {
	row := s.db.QueryRow(`SELECT `+crossQATurnCols+`
		FROM cross_requirement_qa_turns WHERE cross_requirement_id = ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, crossRequirementID)
	return scanCrossQATurn(row)
}

// LatestAnsweredTurn returns the most recent turn that carries a usable
// answer — i.e. the newest turn that is not in the error state. It is the
// input to the session resolution: that turn's SessionID is the thread
// session to resume, and its SourceSessionID is the snapshot the current
// design session is compared against to detect "the design was regenerated, so
// fork again".
//
// Errored turns are skipped on purpose — a failed turn may have left its
// session half-written, and resuming that would be worse than resuming the
// last turn that actually completed.
//
// sql.ErrNoRows when the thread has no answered turn yet.
func (s *CrossRequirementQAService) LatestAnsweredTurn(crossRequirementID string) (*model.CrossRequirementQATurn, error) {
	row := s.db.QueryRow(`SELECT `+crossQATurnCols+`
		FROM cross_requirement_qa_turns WHERE cross_requirement_id = ? AND status <> ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, crossRequirementID, model.QAStatusError)
	return scanCrossQATurn(row)
}

// MarkFinished writes the terminal row. An error run stores the failure text
// in answer so the thread still shows what went wrong instead of a blank
// bubble.
//
// sessionID / sourceSessionID are the terminal values of the turn's session
// bookkeeping — they differ from the pre-minted values when the run degraded
// to a fresh session (source becomes "") or when the CLI reported a session id
// other than the one we asked for.
//
// errorKind classifies a failed run (empty-result / stale-session / cli-crash
// / timeout). It is empty on success.
func (s *CrossRequirementQAService) MarkFinished(id, status, answer, modelName, sessionID, sourceSessionID string, resumed bool, errorKind string) error {
	if status == "" {
		status = model.QAStatusDone
	}
	now := time.Now().UTC()
	_, err := s.db.Exec(`UPDATE cross_requirement_qa_turns
		SET status = ?, answer = ?, model = ?, session_id = ?, source_session_id = ?,
		    resumed = ?, error_kind = ?, finished_at = ?, updated_at = ?
		WHERE id = ?`,
		status, answer, modelName, sessionID, sourceSessionID, boolToInt(resumed), errorKind, now, now, id)
	return err
}

// MarkOrphaned flips a stuck `running` row to `error`.
//
// A turn is marked running before the claude process is spawned, so a backend
// restart (or a JobStore eviction) leaves a row that will never be answered.
// Rather than blocking the thread forever, the read/ask entry points repair
// such rows lazily: the user sees "后端已重启，请重新提问" instead of an
// eternal spinner.
func (s *CrossRequirementQAService) MarkOrphaned(id string, message string) error {
	if strings.TrimSpace(message) == "" {
		message = "后端已重启，请重新提问"
	}
	now := time.Now().UTC()
	_, err := s.db.Exec(`UPDATE cross_requirement_qa_turns
		SET status = ?, answer = ?, finished_at = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		model.QAStatusError, message, now, now, id, model.QAStatusRunning)
	return err
}

// DeleteTurn removes one turn from the thread.
func (s *CrossRequirementQAService) DeleteTurn(id string) error {
	_, err := s.db.Exec(`DELETE FROM cross_requirement_qa_turns WHERE id = ?`, id)
	return err
}

// ResetThread removes every turn of one cross requirement — the "清空讨论记录"
// action.
//
// It also has a second effect the UI must know about: the next overall-design
// run stops folding these conclusions into the plan, because the design job
// reads exactly this thread.
func (s *CrossRequirementQAService) ResetThread(crossRequirementID string) error {
	_, err := s.db.Exec(`DELETE FROM cross_requirement_qa_turns WHERE cross_requirement_id = ?`, crossRequirementID)
	return err
}

// DeleteByCrossRequirement removes every turn of one cross requirement. Called
// when the cross requirement is purged: the schema declares ON DELETE CASCADE,
// but SQLite never enables PRAGMA foreign_keys, so the cascade silently
// no-ops and the rows would be orphaned.
func (s *CrossRequirementQAService) DeleteByCrossRequirement(crossRequirementID string) error {
	_, err := s.db.Exec(`DELETE FROM cross_requirement_qa_turns WHERE cross_requirement_id = ?`, crossRequirementID)
	return err
}

func scanCrossQATurn(row rowScanner) (*model.CrossRequirementQATurn, error) {
	var t model.CrossRequirementQATurn
	var resumed int
	var finishedAt sql.NullTime
	var createdAt, updatedAt time.Time
	if err := row.Scan(&t.ID, &t.CrossRequirementID, &t.Question, &t.Answer, &t.Status,
		&t.JobID, &t.Model, &t.SessionID, &t.SourceSessionID, &resumed, &t.ErrorKind, &t.CreatedBy,
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
