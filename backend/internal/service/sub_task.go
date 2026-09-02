package service

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// SubTaskService is the persistence layer for manually-triggered child agents
// (sub-tasks) attached to a requirement's developing stage. Each sub-task is a
// independent claude CLI process that forks the requirement's main-agent
// session so it inherits the same project / design / conversation context.
//
// The service is a thin SQL layer (consistent with RequirementService and
// other services — no repository abstraction, raw queries through the dialect-
// aware *db.DB wrapper). All exported methods accept/return model.SubTask so
// callers (the wizard handler) never have to map columns themselves.
//
// Concurrency note: multiple sub-tasks can run in parallel for the same
// requirement (each spawns its own claude process and writes its own row).
// There is no cross-row locking here; the handler is responsible for the
// worktree path / branch isolation that prevents the children from stomping on
// each other's file edits.
type SubTaskService struct {
	db *db.DB
}

func NewSubTaskService(database *db.DB) *SubTaskService {
	return &SubTaskService{db: database}
}

// Create inserts a new sub-task row in the "pending" state. Title defaults to
// the first 40 characters of prompt (with an ellipsis when truncated) when the
// caller leaves it blank so the SubTaskPanel always has something to render in
// its card header. session_id / source_session_id / job_id stay empty — the
// handler fills them in as it pre-mints the claude session and creates the
// in-memory JobStore job, so a crash between Create and pre-mint still leaves
// a recoverable row (the handler can later UpdateSession + UpdateJobID).
func (s *SubTaskService) Create(reqID, title, prompt string) (*model.SubTask, error) {
	if reqID == "" {
		return nil, errors.New("requirement_id is required")
	}
	if prompt == "" {
		return nil, errors.New("prompt is required")
	}
	if title == "" {
		title = truncateForTitle(prompt, 40)
	} else if len(title) > 80 {
		// Hard cap so a runaway input can't produce a card header wider than
		// the panel — the rest still lives on the prompt.
		title = title[:80]
	}
	id := util.NewID("st")
	now := time.Now()
	_, err := s.db.Exec(`INSERT INTO sub_tasks (id, requirement_id, title, prompt, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, reqID, title, prompt, model.SubTaskStatusPending, now, now)
	if err != nil {
		return nil, fmt.Errorf("insert sub_task: %w", err)
	}
	return &model.SubTask{
		ID:            id,
		RequirementID: reqID,
		Title:         title,
		Prompt:        prompt,
		Status:        model.SubTaskStatusPending,
		CreatedAt:     now,
		UpdatedAt:     now,
	}, nil
}

// List returns every sub-task attached to reqID, oldest first (matching the
// order the wizard fires them and the order the UI renders the cards).
// Returns an empty slice when the requirement has none — never nil, so the
// frontend can map directly without a guard.
func (s *SubTaskService) List(reqID string) ([]model.SubTask, error) {
	rows, err := s.db.Query(`SELECT id, requirement_id, title, prompt, status,
		session_id, source_session_id, job_id, artifact, model,
		input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
		cost_cents, duration_seconds,
		created_at, updated_at, completed_at
		FROM sub_tasks WHERE requirement_id = ? ORDER BY created_at ASC, id ASC`, reqID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.SubTask{}
	for rows.Next() {
		st, err := scanSubTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *st)
	}
	return out, rows.Err()
}

// Get loads one sub-task by id. Returns sql.ErrNoRows when the id doesn't
// exist so callers can render 404; validation that the row's requirement_id
// matches the URL parameter happens in the handler, not here (this service
// stays a thin SQL wrapper).
func (s *SubTaskService) Get(id string) (*model.SubTask, error) {
	rows, err := s.db.Query(`SELECT id, requirement_id, title, prompt, status,
		session_id, source_session_id, job_id, artifact, model,
		input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
		cost_cents, duration_seconds,
		created_at, updated_at, completed_at
		FROM sub_tasks WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	return scanSubTask(rows)
}

// UpdateSession stores the pre-minted claude session id and the parent
// session id it forks from. Called once per sub-task, immediately after
// Create, before the goroutine spawns the claude CLI — that ordering means a
// crash between Create and Start still leaves the row recoverable: a follow-up
// UpdateSession + UpdateJobID + Start picks up where it left off.
func (s *SubTaskService) UpdateSession(id, sessionID, sourceSessionID string) error {
	_, err := s.db.Exec(`UPDATE sub_tasks SET session_id=?, source_session_id=?, updated_at=? WHERE id=?`,
		sessionID, sourceSessionID, time.Now(), id)
	return err
}

// UpdateJobID records the in-memory JobStore job id so a page refresh can
// reconnect to the running child agent via the existing /api/wizard/jobs/...
// SSE endpoint (the sub-task reuses the wizard job-stream protocol). Pass ""
// when the job has been evicted by the ring buffer so the UI can stop showing
// the spinner without losing the row.
func (s *SubTaskService) UpdateJobID(id, jobID string) error {
	_, err := s.db.Exec(`UPDATE sub_tasks SET job_id=?, updated_at=? WHERE id=?`,
		jobID, time.Now(), id)
	return err
}

// MarkRunning transitions pending → running when the goroutine actually
// spawns the claude CLI. Kept separate from Create so a Create that fails to
// ever spawn (e.g. pre-flight error) doesn't leave the row visible as
// "running" in the UI. Returns the timestamp used so the caller can pass it
// into Finish() to compute DurationSeconds without re-reading the row.
func (s *SubTaskService) MarkRunning(id string) (time.Time, error) {
	now := time.Now()
	_, err := s.db.Exec(`UPDATE sub_tasks SET status=?, updated_at=? WHERE id=?`,
		model.SubTaskStatusRunning, now, id)
	return now, err
}

// CreateAdjustment inserts a follow-up sub-task that resumes the parent
// sub-task's session id. The returned row carries the parent's session id in
// SourceSessionID so the wizard handler spawns the child with
// --resume <parent_sid> --fork-session, inheriting the parent's
// implementation transcript + edits the parent made, then writes a fresh
// adjustments report into Artifact.
//
// Title defaults to "调整: <parent title>". prompt is the user's follow-up
// instruction (e.g. "再补一行单元测试"). The new row's session id is pre-minted
// by the handler via UpdateSession — service layer only persists the parent
// reference.
func (s *SubTaskService) CreateAdjustment(reqID, parentID, prompt string) (*model.SubTask, error) {
	if reqID == "" || parentID == "" {
		return nil, errors.New("requirement_id and parent sub_task id are required")
	}
	if prompt == "" {
		return nil, errors.New("prompt is required")
	}
	parent, err := s.Get(parentID)
	if err != nil {
		return nil, fmt.Errorf("load parent sub_task: %w", err)
	}
	if parent.RequirementID != reqID {
		return nil, fmt.Errorf("parent sub_task belongs to requirement %s, not %s", parent.RequirementID, reqID)
	}
	id := util.NewID("st")
	now := time.Now()
	adjustTitle := "调整: " + parent.Title
	if len(adjustTitle) > 80 {
		adjustTitle = adjustTitle[:80]
	}
	_, err = s.db.Exec(`INSERT INTO sub_tasks (id, requirement_id, title, prompt, status,
		source_session_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, reqID, adjustTitle, prompt, model.SubTaskStatusPending,
		parent.SessionID, now, now)
	if err != nil {
		return nil, fmt.Errorf("insert adjustment sub_task: %w", err)
	}
	return &model.SubTask{
		ID:              id,
		RequirementID:   reqID,
		Title:           adjustTitle,
		Prompt:          prompt,
		Status:          model.SubTaskStatusPending,
		SourceSessionID: parent.SessionID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

// Finish is the terminal write: status (done | error), artifact Markdown, the
// effective model, terminal token usage, resolved cost (cents), and wall-
// clock duration. completed_at is stamped automatically when status moves
// out of running. artifact is the durable report — even if the JobStore ring
// buffer later evicts the live job, the Markdown stays.
//
// startTime is the wall-clock instant the sub-task entered "running" —
// produced by MarkRunning's return value. Pass time.Time{} to skip duration
// computation (e.g. when called from a CreateAdjustment path that doesn't
// care about wall-clock); the column then stays at its default 0.
func (s *SubTaskService) Finish(id, status, artifact, modelName string, tokens model.SubTaskTokens, costCents int, startTime time.Time) error {
	now := time.Now()
	duration := 0
	if !startTime.IsZero() {
		duration = int(now.Sub(startTime).Round(time.Second).Seconds())
		if duration < 0 {
			duration = 0
		}
	}
	_, err := s.db.Exec(`UPDATE sub_tasks SET status=?, artifact=?, model=?,
		input_tokens=?, output_tokens=?, cache_creation_tokens=?, cache_read_tokens=?,
		cost_cents=?, duration_seconds=?,
		completed_at=?, updated_at=? WHERE id=?`,
		status, artifact, modelName,
		tokens.Input, tokens.Output, tokens.CacheCreation, tokens.CacheRead,
		costCents, duration,
		now, now, id)
	return err
}

// RecoverInterrupted marks every sub-task left in running/pending state as
// error at server startup. Sub-task execution is driven by in-memory
// goroutines + JobStore jobs, so a restart orphans those rows forever
// (the UI would otherwise show an eternal spinner — req_9d24ef181a5ad5c4
// left one stuck for the whole session). The artifact explains the cause so
// the user knows to re-dispatch (e.g. via 重新拆分). Returns the number of
// rows recovered.
func (s *SubTaskService) RecoverInterrupted() (int64, error) {
	now := time.Now()
	res, err := s.db.Exec(`UPDATE sub_tasks SET status=?, artifact=?,
		completed_at=?, updated_at=? WHERE status IN (?, ?)`,
		model.SubTaskStatusError,
		"❌ 服务在子任务执行期间重启，任务中断。请通过「重新拆分」或手动启动重新执行。",
		now, now, model.SubTaskStatusRunning, model.SubTaskStatusPending)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// scanSubTask is a shared row→struct mapper. Pulled out so List / Get can
// share the column order without each method carrying its own Scan list.
func scanSubTask(rows *sql.Rows) (*model.SubTask, error) {
	var st model.SubTask
	var completedAt sql.NullTime
	if err := rows.Scan(
		&st.ID, &st.RequirementID, &st.Title, &st.Prompt, &st.Status,
		&st.SessionID, &st.SourceSessionID, &st.JobID, &st.Artifact, &st.Model,
		&st.InputTokens, &st.OutputTokens, &st.CacheCreationTokens, &st.CacheReadTokens,
		&st.CostCents, &st.DurationSeconds,
		&st.CreatedAt, &st.UpdatedAt, &completedAt,
	); err != nil {
		return nil, err
	}
	if completedAt.Valid {
		t := completedAt.Time
		st.CompletedAt = &t
	}
	return &st, nil
}

// truncateForTitle renders a single-line title preview from prompt. Replaces
// newlines / tabs with spaces so the card header never wraps mid-token, and
// appends an ellipsis when the input was longer than max chars so the user
// can tell the title is a preview. ASCII-only ellipsis to avoid any
// multi-byte confusion in cards.
func truncateForTitle(prompt string, max int) string {
	cleaned := make([]rune, 0, len(prompt))
	for _, r := range prompt {
		if r == '\n' || r == '\r' || r == '\t' {
			cleaned = append(cleaned, ' ')
			continue
		}
		cleaned = append(cleaned, r)
	}
	if len(cleaned) <= max {
		return string(cleaned)
	}
	return string(cleaned[:max]) + "..."
}
