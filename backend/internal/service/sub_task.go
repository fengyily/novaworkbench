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
// its card header. session_id / job_id stay empty — the handler fills them in
// as it pre-mints the claude session and creates the in-memory JobStore job,
// so a crash between Create and pre-mint still leaves a recoverable row (the
// handler can later UpdateSession + UpdateJobID).
//
// modelDisplay and sourceSID are stamped up-front so the SubTaskPanel renders
// the model badge from the moment the row appears (mirrors UpdateModel's
// behavior) and so an auto-orchestrated sub-task's source_session_id is
// available for the fork-session path before UpdateSession overwrites it with
// the freshly pre-minted id. Pass "" for both when the caller doesn't know
// (manual-create path that fills them via UpdateSession/UpdateModel anyway).
//
// batchID + batchSeq tag the row for an orchestration_batches run. Pass ""
// and 0 for manual sub-tasks; pass the batch id and 1..N sequence number for
// children of tryAutoOrchestrate. OrchestrationQueue's tick uses these to
// dispatch children in batch_seq order.
func (s *SubTaskService) Create(reqID, title, prompt, modelDisplay, sourceSID, batchID string, batchSeq int) (*model.SubTask, error) {
	if reqID == "" {
		return nil, errors.New("requirement_id is required")
	}
	if prompt == "" {
		return nil, errors.New("prompt is required")
	}
	if title == "" {
		title = truncateForTitle(prompt, 40)
	} else {
		// Hard cap so a runaway input can't produce a card header wider than
		// the panel — the rest still lives on the prompt.
		title = capTitle(title, 80)
	}
	id := util.NewID("st")
	now := time.Now()
	_, err := s.db.Exec(`INSERT INTO sub_tasks (id, requirement_id, title, prompt, status,
		model, source_session_id, batch_id, batch_seq, source,
		created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, reqID, title, prompt, model.SubTaskStatusPending,
		modelDisplay, sourceSID, batchID, batchSeq, model.SubTaskSourceManual,
		now, now)
	if err != nil {
		return nil, fmt.Errorf("insert sub_task: %w", err)
	}
	return &model.SubTask{
		ID:              id,
		RequirementID:   reqID,
		Title:           title,
		Prompt:          prompt,
		Status:          model.SubTaskStatusPending,
		Model:           modelDisplay,
		SourceSessionID: sourceSID,
		BatchID:         batchID,
		BatchSeq:        batchSeq,
		Source:          model.SubTaskSourceManual,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

// CreateWithBatchTx is the in-transaction sibling of Create. tryAutoOrchestrate
// uses it inside a *db.Tx so the N child inserts and the orchestration_batches
// insert commit atomically — partial state from a mid-transaction failure is
// rolled back instead of leaving an orphaned batch with no children (or vice
// versa). The return value is the same as Create; the caller does not need
// the tx reference again because the caller owns the rollback/commit.
func (s *SubTaskService) CreateWithBatchTx(tx *db.Tx, reqID, title, prompt, modelDisplay, sourceSID, batchID string, batchSeq int) (*model.SubTask, error) {
	if tx == nil {
		return nil, errors.New("tx is required")
	}
	if reqID == "" {
		return nil, errors.New("requirement_id is required")
	}
	if prompt == "" {
		return nil, errors.New("prompt is required")
	}
	if title == "" {
		title = truncateForTitle(prompt, 40)
	} else {
		title = capTitle(title, 80)
	}
	id := util.NewID("st")
	now := time.Now()
	_, err := tx.Exec(`INSERT INTO sub_tasks (id, requirement_id, title, prompt, status,
		model, source_session_id, batch_id, batch_seq, source,
		created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, reqID, title, prompt, model.SubTaskStatusPending,
		modelDisplay, sourceSID, batchID, batchSeq, model.SubTaskSourceAuto,
		now, now)
	if err != nil {
		return nil, fmt.Errorf("insert sub_task: %w", err)
	}
	return &model.SubTask{
		ID:              id,
		RequirementID:   reqID,
		Title:           title,
		Prompt:          prompt,
		Status:          model.SubTaskStatusPending,
		Model:           modelDisplay,
		SourceSessionID: sourceSID,
		BatchID:         batchID,
		BatchSeq:        batchSeq,
		Source:          model.SubTaskSourceAuto,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

// List returns every sub-task attached to reqID, **newest first** —
// matching the SubTaskPanel sort and the user's expectation that the most
// recently created card sits at the top. id DESC is the tie-breaker so
// rows with the same created_at (millisecond ties possible across DBs)
// keep a stable order.
func (s *SubTaskService) List(reqID string) ([]model.SubTask, error) {
	rows, err := s.db.Query(`SELECT id, requirement_id, title, prompt, status,
		session_id, source_session_id, job_id, artifact, model,
		input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
		cost_cents, duration_seconds,
		created_at, updated_at, completed_at,
		batch_id, batch_seq, batch_id_seq_run, source
		FROM sub_tasks WHERE requirement_id = ? ORDER BY created_at DESC, id DESC`, reqID)
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
		created_at, updated_at, completed_at,
		batch_id, batch_seq, batch_id_seq_run, source
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

// ListByBatch returns every sub-task attached to batchID, ordered by
// batch_seq ASC (then created_at ASC as a tie-breaker for the legacy
// batch_seq=0 manual rows). Used by OrchestrationQueue to enumerate a batch's
// children when computing summary inputs. Returns an empty slice when the
// batch has no rows — never nil.
func (s *SubTaskService) ListByBatch(batchID string) ([]model.SubTask, error) {
	rows, err := s.db.Query(`SELECT id, requirement_id, title, prompt, status,
		session_id, source_session_id, job_id, artifact, model,
		input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
		cost_cents, duration_seconds,
		created_at, updated_at, completed_at,
		batch_id, batch_seq, batch_id_seq_run
		FROM sub_tasks WHERE batch_id = ?
		ORDER BY batch_seq ASC, created_at ASC, id ASC`, batchID)
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

// CountTerminalByBatch reports the number of children in terminal states
// (done and error) for batchID. OrchestrationQueue compares this against
// batch.TotalChildren to decide when to flip a dispatching batch into
// summarizing. Both counts are returned in a single row scan so the tick
// goroutine can decide without a second round-trip.
func (s *SubTaskService) CountTerminalByBatch(batchID string) (done, errored int, err error) {
	row := s.db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN status=? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status=? THEN 1 ELSE 0 END), 0)
		FROM sub_tasks WHERE batch_id=?`,
		model.SubTaskStatusDone, model.SubTaskStatusError, batchID)
	var d, e int64
	if err := row.Scan(&d, &e); err != nil {
		return 0, 0, fmt.Errorf("count terminal sub_tasks: %w", err)
	}
	return int(d), int(e), nil
}

// ClaimNextPending atomically picks the lowest batch_seq pending row for
// batchID and flips it to running. Used by OrchestrationQueue's tick loop so
// the (batch_id, batch_seq) ordering is preserved across crash/restart — only
// one caller can ever own a given (batch_id, batch_seq) at a time, and
// ClaimNextPending is the single entry point that grants that ownership.
//
// Implementation: an UPDATE with the candidate id resolved by a correlated
// subquery, evaluated by the database inside the same statement. The
// `AND status='pending'` clause on the outer WHERE makes the flip
// conditional — if another writer has already changed status, RowsAffected
// drops to 0 and the second bool return is false; no row is leaked to a stale
// caller. On success the batch_id_seq_run column is stamped with the current
// time as a heartbeat that RecoverInterrupted inspects on boot.
//
// On RowsAffected==1 we re-SELECT the row with a WHERE batch_id=? AND
// status='running' ORDER BY batch_seq ASC LIMIT 1 — there's exactly one
// running row we just produced under SQLite's single-writer model and READ
// COMMITTED semantics on MySQL/Postgres, so the re-select is unambiguous.
func (s *SubTaskService) ClaimNextPending(batchID string) (*model.SubTask, bool, error) {
	if batchID == "" {
		return nil, false, errors.New("batch_id is required")
	}
	res, err := s.db.Exec(`UPDATE sub_tasks
		SET status=?, updated_at=CURRENT_TIMESTAMP, batch_id_seq_run=CURRENT_TIMESTAMP
		WHERE id = (
			SELECT id FROM sub_tasks
			 WHERE batch_id=? AND status=?
			 ORDER BY batch_seq ASC, created_at ASC
			 LIMIT 1
		)
		AND status=?`,
		model.SubTaskStatusRunning, batchID, model.SubTaskStatusPending, model.SubTaskStatusPending)
	if err != nil {
		return nil, false, fmt.Errorf("claim pending sub_task: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, false, nil
	}
	rows, err := s.db.Query(`SELECT id, requirement_id, title, prompt, status,
		session_id, source_session_id, job_id, artifact, model,
		input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
		cost_cents, duration_seconds,
		created_at, updated_at, completed_at,
		batch_id, batch_seq, batch_id_seq_run, source
		FROM sub_tasks
		 WHERE batch_id=? AND status=?
		 ORDER BY batch_seq ASC, created_at ASC
		 LIMIT 1`,
		batchID, model.SubTaskStatusRunning)
	if err != nil {
		return nil, false, fmt.Errorf("re-select claimed sub_task: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, false, fmt.Errorf("claim succeeded but row vanished for batch %s", batchID)
	}
	st, err := scanSubTask(rows)
	if err != nil {
		return nil, false, err
	}
	return st, true, nil
}

// MarkHeartbeat refreshes batch_id_seq_run to the current timestamp for a
// running sub-task. OrchestrationQueue's per-child goroutine calls this every
// 5s so RecoverInterrupted can distinguish a live running row from one
// orphaned by a backend crash — a row whose heartbeat is older than 5 minutes
// at boot time is treated as orphaned and flipped back to pending for
// re-dispatch. The status='running' guard means a Finish() that races the
// heartbeat doesn't accidentally reset the heartbeat on a terminal row.
func (s *SubTaskService) MarkHeartbeat(subTaskID string) error {
	_, err := s.db.Exec(`UPDATE sub_tasks SET batch_id_seq_run=CURRENT_TIMESTAMP
		WHERE id=? AND status=?`,
		subTaskID, model.SubTaskStatusRunning)
	return err
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

// SetBatchID stamps an existing sub_task row with the given batch_id and
// batch_seq. Used by the manual-summary path (GenerateSubTaskSummary) to
// retroactively attach manual children (batch_id='', batch_seq=0) to a
// freshly-created summarizing batch so RunOrchestratorSummary's
// ListByBatch picks them up. Idempotent — re-stamping is harmless because
// the same row already has the same id, and batch_seq follows the user's
// existing list order.
//
// Auto-orchestrated rows (those already carrying a non-empty batch_id) are
// silently skipped by the caller (GenerateSubTaskSummary filters them
// first) so this method doesn't need its own guard — but it does require
// the row to be in a terminal state when stamped, since a running child
// forked from the wrong session context would corrupt the next dispatch.
func (s *SubTaskService) SetBatchID(subTaskID, batchID string, batchSeq int) error {
	if subTaskID == "" || batchID == "" {
		return errors.New("sub_task_id and batch_id are required")
	}
	_, err := s.db.Exec(`UPDATE sub_tasks SET batch_id=?, batch_seq=?, updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND status IN (?, ?)`,
		batchID, batchSeq, subTaskID, model.SubTaskStatusDone, model.SubTaskStatusError)
	return err
}

// UpdateModel records the effective model that will be (or was) dispatched to
// the child agent. The runner persists it up-front (before MarkRunning) so
// the SubTaskPanel can render the "🪙 claude-sonnet" badge from the moment
// the row appears, even if Run never runs (e.g. pre-flight error). Finish
// also stamps this column on terminal success — keeping them in sync is the
// runner's responsibility.
func (s *SubTaskService) UpdateModel(id, modelName string) error {
	_, err := s.db.Exec(`UPDATE sub_tasks SET model=?, updated_at=? WHERE id=?`,
		modelName, time.Now(), id)
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
	adjustTitle := capTitle("调整: "+parent.Title, 80)
	_, err = s.db.Exec(`INSERT INTO sub_tasks (id, requirement_id, title, prompt, status,
		source_session_id, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, reqID, adjustTitle, prompt, model.SubTaskStatusPending,
		parent.SessionID, model.SubTaskSourceManual, now, now)
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
		Source:          model.SubTaskSourceManual,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

// Redo creates a NEW sub-task row that re-runs a failed parent sub-task with
// its original prompt. Unlike CreateAdjustment — which forks the parent's own
// session to inherit its edits — a redo forks the parent's SOURCE session
// (the session the failed run originally forked from), so the child re-executes
// the original task from a clean starting point rather than inheriting a
// broken/partial attempt. The redo row carries the parent's source_session_id
// in its own source_session_id; the handler fills in a fallback when empty.
//
// Title defaults to "重做: <parent title>". The model is NOT persisted here —
// the handler passes the chosen model into runSubTask, which stamps it via
// Finish (mirrors Create/CreateAdjustment).
func (s *SubTaskService) Redo(reqID, parentID string) (*model.SubTask, error) {
	if reqID == "" || parentID == "" {
		return nil, errors.New("requirement_id and parent sub_task id are required")
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
	redoTitle := capTitle("重做: "+parent.Title, 80)
	_, err = s.db.Exec(`INSERT INTO sub_tasks (id, requirement_id, title, prompt, status,
		source_session_id, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, reqID, redoTitle, parent.Prompt, model.SubTaskStatusPending,
		parent.SourceSessionID, model.SubTaskSourceManual, now, now)
	if err != nil {
		return nil, fmt.Errorf("insert redo sub_task: %w", err)
	}
	return &model.SubTask{
		ID:              id,
		RequirementID:   reqID,
		Title:           redoTitle,
		Prompt:          parent.Prompt,
		Status:          model.SubTaskStatusPending,
		SourceSessionID: parent.SourceSessionID,
		Source:          model.SubTaskSourceManual,
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

// RecoverInterrupted reconciles sub-task state with the freshly-booted
// backend. It runs in two passes so the manual path keeps its "mark error and
// tell the user to redo" behavior while the auto-orchestrated path gets the
// restart-safe behavior the new OrchestrationQueue expects:
//
//  1. Manual path (batch_id='' OR NULL): pending/running rows become error
//     with a recovery artifact, exactly like the pre-batch behavior. The
//     sub-task was driven by an in-memory JobStore goroutine that died with
//     the backend, so there's nothing to recover — the user must re-trigger.
//  2. Orchestrated path (batch_id != ''): running rows whose heartbeat
//     (batch_id_seq_run) is older than 5 minutes are flipped back to
//     pending. The owning goroutine died with the backend, but the row
//     itself is intact in the DB; OrchestrationQueue's next tick will
//     re-claim it via ClaimNextPending. Rows with a fresh heartbeat (the
//     queue tick re-claimed them between restart and this call) are left
//     alone so we don't double-dispatch.
//
// pending rows under a batch_id are NOT touched — they're the unclaimed
// queue for OrchestrationQueue and remain pending for the next tick to pick
// up.
//
// Returns the total number of rows the two passes affected. main.go logs
// this so an ops dashboard can spot "many orchestrations interrupted at
// boot" patterns.
func (s *SubTaskService) RecoverInterrupted() (int64, error) {
	now := time.Now()

	// Pass 1: manual sub-tasks — preserve the old mark-error behavior.
	res, err := s.db.Exec(`UPDATE sub_tasks
		SET status=?, artifact=?,
		    completed_at=?, updated_at=?
		WHERE status IN (?, ?)
		  AND (batch_id = '' OR batch_id IS NULL)`,
		model.SubTaskStatusError,
		"❌ 服务在子任务执行期间重启，任务中断。请通过「重新拆分」或手动启动重新执行。",
		now, now,
		model.SubTaskStatusRunning, model.SubTaskStatusPending)
	if err != nil {
		return 0, fmt.Errorf("recover manual sub_tasks: %w", err)
	}
	manualAffected, _ := res.RowsAffected()

	// Pass 2: orchestrated sub-tasks — flip stale running rows back to
	// pending so OrchestrationQueue re-dispatches them. cutoff is the Go
	// time threshold; the SQL parameter binding puts it into the dialect's
	// DATETIME comparison natively (SQLite/MySQL/Postgres all accept
	// time.Time as a parameter and compare correctly against stored
	// DATETIME/TIMESTAMP values).
	cutoff := now.Add(-5 * time.Minute)
	res2, err := s.db.Exec(`UPDATE sub_tasks
		SET status=?, batch_id_seq_run=NULL, updated_at=?
		WHERE status=?
		  AND batch_id != '' AND batch_id IS NOT NULL
		  AND batch_id_seq_run IS NOT NULL
		  AND batch_id_seq_run < ?`,
		model.SubTaskStatusPending, now,
		model.SubTaskStatusRunning, cutoff)
	if err != nil {
		return manualAffected, fmt.Errorf("recover orchestrated sub_tasks: %w", err)
	}
	orchestratedAffected, _ := res2.RowsAffected()
	return manualAffected + orchestratedAffected, nil
}

// scanSubTask is a shared row→struct mapper. Pulled out so List / Get can
// share the column order without each method carrying its own Scan list.
// The SELECT must include `source` as the last column.
func scanSubTask(rows *sql.Rows) (*model.SubTask, error) {
	var st model.SubTask
	var completedAt sql.NullTime
	var heartbeat sql.NullTime
	if err := rows.Scan(
		&st.ID, &st.RequirementID, &st.Title, &st.Prompt, &st.Status,
		&st.SessionID, &st.SourceSessionID, &st.JobID, &st.Artifact, &st.Model,
		&st.InputTokens, &st.OutputTokens, &st.CacheCreationTokens, &st.CacheReadTokens,
		&st.CostCents, &st.DurationSeconds,
		&st.CreatedAt, &st.UpdatedAt, &completedAt,
		&st.BatchID, &st.BatchSeq, &heartbeat,
		&st.Source,
	); err != nil {
		return nil, err
	}
	if heartbeat.Valid {
		t := heartbeat.Time
		st.BatchIDSeqRun = &t
	}
	if completedAt.Valid {
		t := completedAt.Time
		st.CompletedAt = &t
	}
	return &st, nil
}

// capTitle hard-caps a title at max runes. Byte-slicing here would split a
// multi-byte rune and hand PostgreSQL an invalid UTF-8 sequence
// (SQLSTATE 22021) when the result is INSERTed — CJK titles hit this
// immediately when the rune boundary doesn't fall on the byte boundary.
// Inputs are presumed already-valid UTF-8 (the DB round-trips would have
// rejected invalid bytes at write time).
func capTitle(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
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
