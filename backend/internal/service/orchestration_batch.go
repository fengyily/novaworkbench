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

// OrchestrationBatchService is the persistence layer for orchestration_batches
// — the coordinator row that groups N sub_tasks into a single restart-safe
// dispatch run. tryAutoOrchestrate INSERTs one batch alongside N sub_tasks
// inside a single transaction; OrchestrationQueue's tick loop then walks the
// batch by batch_seq (atomic ClaimNextPending on the sub_tasks side) until
// every child has reached a terminal status, at which point the batch flips
// to "summarizing" and a final summary round writes requirements.coding_plan.
//
// Concurrency note: rows are mutated from three call sites — tryAutoOrchestrate
// (writes), OrchestrationQueue.tick (status transitions), and the per-batch
// goroutines (heartbeats). All writes go through the dialect-aware *db.DB
// wrapper so `?` placeholders are rebound for Postgres and bool/UTF-8 args
// are normalized centrally. Boot-time recovery reuses the same Mark* methods
// (no separate "reset" path) so the state machine has one source of truth.
type OrchestrationBatchService struct {
	db *db.DB
}

func NewOrchestrationBatchService(database *db.DB) *OrchestrationBatchService {
	return &OrchestrationBatchService{db: database}
}

// Create inserts a fresh orchestration_batches row in the "dispatching"
// state with summary_status='pending'. Used by callers that build children
// outside a transaction (none today — tryAutoOrchestrate uses CreateWithTx —
// but kept for completeness / future ad-hoc callers). Returns the full row
// re-SELECTed so callers see server-assigned timestamps.
func (s *OrchestrationBatchService) Create(reqID, orchestratorSID, modelName, workDir, cfgID string, totalChildren int) (*model.OrchestrationBatch, error) {
	if reqID == "" {
		return nil, errors.New("requirement_id is required")
	}
	if totalChildren < 0 {
		return nil, errors.New("total_children must be >= 0")
	}
	id := util.NewID("ob")
	now := time.Now()
	_, err := s.db.Exec(`INSERT INTO orchestration_batches (
		id, requirement_id, orchestrator_session_id, model, work_dir, claude_config_id,
		total_children, status, summary_status,
		created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, reqID, orchestratorSID, modelName, workDir, cfgID,
		totalChildren, model.BatchDispatching, model.SummaryPending,
		now, now)
	if err != nil {
		return nil, fmt.Errorf("insert orchestration_batch: %w", err)
	}
	return s.Get(id)
}

// CreateWithTx is the in-transaction sibling of Create. tryAutoOrchestrate
// calls this inside a *db.Tx so the batch INSERT and the N sub_task INSERTs
// commit atomically — a mid-transaction failure rolls back the whole batch
// instead of leaving an orphaned batch with zero children (or children with no
// parent batch row). Returns the new batch id; the row is re-SELECTed by the
// caller after Commit because reading through the tx is unnecessary here.
func (s *OrchestrationBatchService) CreateWithTx(tx *db.Tx, reqID, orchestratorSID, modelName, workDir, cfgID string, totalChildren int) (string, error) {
	if tx == nil {
		return "", errors.New("tx is required")
	}
	if reqID == "" {
		return "", errors.New("requirement_id is required")
	}
	if totalChildren < 0 {
		return "", errors.New("total_children must be >= 0")
	}
	id := util.NewID("ob")
	_, err := tx.Exec(`INSERT INTO orchestration_batches (
		id, requirement_id, orchestrator_session_id, model, work_dir, claude_config_id,
		total_children, status, summary_status,
		created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, reqID, orchestratorSID, modelName, workDir, cfgID,
		totalChildren, model.BatchDispatching, model.SummaryPending,
		time.Now(), time.Now())
	if err != nil {
		return "", fmt.Errorf("insert orchestration_batch (tx): %w", err)
	}
	return id, nil
}

// Get loads one batch by id. Returns sql.ErrNoRows so callers can distinguish
// "no such batch" from a real DB error.
func (s *OrchestrationBatchService) Get(id string) (*model.OrchestrationBatch, error) {
	if id == "" {
		return nil, errors.New("batch id is required")
	}
	rows, err := s.db.Query(batchSelectColumns+` WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	return scanBatch(rows)
}

// GetActiveByRequirement returns the most recent non-completed batch for a
// requirement, or (nil, nil) when none exists. Used by tryAutoOrchestrate and
// ReOrchestrate to gate re-entry into a still-running orchestration, and by
// the manual-summary handler to refuse creating a second concurrent batch.
// "Active" here means status IN ('dispatching','summarizing'); completed /
// errored batches are intentionally excluded so the gate clears as soon as
// the run finishes (or fails terminally).
func (s *OrchestrationBatchService) GetActiveByRequirement(reqID string) (*model.OrchestrationBatch, error) {
	if reqID == "" {
		return nil, errors.New("requirement_id is required")
	}
	rows, err := s.db.Query(batchSelectColumns+`
		WHERE requirement_id=? AND status IN (?, ?)
		ORDER BY created_at DESC, id DESC
		LIMIT 1`,
		reqID, model.BatchDispatching, model.BatchSummarizing)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return scanBatch(rows)
}

// GetLatestByRequirement returns the most recently created batch for a
// requirement regardless of status (active OR terminal), or (nil, nil) when
// no batch has ever been committed. Powers the
// GET /api/requirements/{id}/orchestration/batch endpoint — the
// SubTaskPanel reads the result to render summary CTAs and the
// page-level banner (status='completed' should still surface as "✅
// 已完成" so the user knows the run finished, not vanish).
func (s *OrchestrationBatchService) GetLatestByRequirement(reqID string) (*model.OrchestrationBatch, error) {
	if reqID == "" {
		return nil, errors.New("requirement_id is required")
	}
	rows, err := s.db.Query(batchSelectColumns+`
		WHERE requirement_id=?
		ORDER BY created_at DESC, id DESC
		LIMIT 1`,
		reqID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return scanBatch(rows)
}

// ListActive returns the active (dispatching or summarizing) batches,
// oldest-updated first so a tick that drains work doesn't starve batches
// that have been waiting longest. The limit caps per-tick work; the caller
// (OrchestrationQueue.tick) loops back on the next interval for whatever
// didn't fit.
func (s *OrchestrationBatchService) ListActive(limit int) ([]model.OrchestrationBatch, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(batchSelectColumns+`
		WHERE status IN (?, ?)
		ORDER BY updated_at ASC, id ASC
		LIMIT ?`,
		model.BatchDispatching, model.BatchSummarizing, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.OrchestrationBatch{}
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// MarkStatus is the generic status transition used for both
// dispatching→errored (cancel) and similar coarse changes. The
// updated_at=self-stamp means OrchestrationQueue's "ORDER BY updated_at ASC"
// tick keeps recently-touched batches from being re-picked too eagerly.
func (s *OrchestrationBatchService) MarkStatus(id, status string) error {
	if id == "" {
		return errors.New("batch id is required")
	}
	if status == "" {
		return errors.New("status is required")
	}
	_, err := s.db.Exec(`UPDATE orchestration_batches
		SET status=?, updated_at=CURRENT_TIMESTAMP
		WHERE id=?`,
		status, id)
	return err
}

// MarkSummarizing flips a dispatching batch into summarizing, resets the
// summary heartbeat, and queues the next summary round. The AND status=
// 'dispatching' guard makes the transition atomic — a concurrent tick that
// has already flipped the row to errored (e.g. user cancellation) won't have
// its work silently overwritten.
func (s *OrchestrationBatchService) MarkSummarizing(id string) error {
	if id == "" {
		return errors.New("batch id is required")
	}
	_, err := s.db.Exec(`UPDATE orchestration_batches
		SET status=?, summary_status=?, updated_at=CURRENT_TIMESTAMP, summary_heartbeat_at=NULL
		WHERE id=? AND status=?`,
		model.BatchSummarizing, model.SummaryPending, id, model.BatchDispatching)
	return err
}

// MarkSummary updates the summary_status column (pending | running | done |
// error) and refreshes updated_at. Used both to arm a new round (pending)
// and to record the terminal outcome (done / error). Heartbeat is managed
// separately via MarkSummaryHeartbeat so this stays a single-column write.
func (s *OrchestrationBatchService) MarkSummary(id, summaryStatus string) error {
	if id == "" {
		return errors.New("batch id is required")
	}
	if summaryStatus == "" {
		return errors.New("summary_status is required")
	}
	_, err := s.db.Exec(`UPDATE orchestration_batches
		SET summary_status=?, updated_at=CURRENT_TIMESTAMP
		WHERE id=?`,
		summaryStatus, id)
	return err
}

// MarkSummaryHeartbeat is the 5s-tick companion to RunOrchestratorSummary —
// the goroutine running the summary round calls this periodically so boot
// recovery and the live tick can distinguish a live summary from one
// orphaned by a backend crash. The status='running' guard makes a late
// heartbeat on an already-terminal summary a no-op rather than a zombie
// rewrite of a finalized row.
func (s *OrchestrationBatchService) MarkSummaryHeartbeat(id string) error {
	if id == "" {
		return errors.New("batch id is required")
	}
	_, err := s.db.Exec(`UPDATE orchestration_batches
		SET summary_heartbeat_at=CURRENT_TIMESTAMP
		WHERE id=? AND summary_status=?`,
		id, model.SummaryRunning)
	return err
}

// UpdateTotalChildren patches total_children after the batch has been
// inserted. Used by the manual-summary path: a hand-crafted batch starts
// at total_children=0 (Create's default for ad-hoc callers) and is bumped
// to match the number of existing manual sub-tasks once they're stamped
// onto this batch via SubTaskService.SetBatchID. Without this, the
// queue's CountTerminalByBatch gate would never flip the batch out of
// summarizing on its own (which is fine for the manual path — the summary
// writes once and we're done — but the count stays accurate for ops
// dashboards).
func (s *OrchestrationBatchService) UpdateTotalChildren(id string, total int) error {
	if id == "" {
		return errors.New("batch id is required")
	}
	if total < 0 {
		return errors.New("total must be >= 0")
	}
	_, err := s.db.Exec(`UPDATE orchestration_batches
		SET total_children=?, updated_at=CURRENT_TIMESTAMP
		WHERE id=?`,
		total, id)
	return err
}

// MarkCompleted is the terminal write for a happy-path run: status=completed,
// summary_status=done, completed_at stamped, updated_at refreshed. There's
// no guard on the prior status — both the summary goroutine (happy path)
// and MarkStatus (manual completion override) reach this, and a no-op
// double-call is harmless.
func (s *OrchestrationBatchService) MarkCompleted(id string) error {
	if id == "" {
		return errors.New("batch id is required")
	}
	_, err := s.db.Exec(`UPDATE orchestration_batches
		SET status=?, summary_status=?, updated_at=CURRENT_TIMESTAMP, completed_at=CURRENT_TIMESTAMP
		WHERE id=?`,
		model.BatchCompleted, model.SummaryDone, id)
	return err
}

// Recover is the boot-time companion to RecoverInterrupted on the sub_tasks
// side. It looks for batches whose summary_status='running' but whose
// summary_heartbeat_at is stale (NULL or older than 5 minutes) and resets
// them to summary_status='pending' so the next tick re-arms a fresh summary
// goroutine. Returns the number of batches it rewrote so main.go can log
// "reset N stale summaries" for ops visibility.
//
// Cutoff is computed in Go and passed as a parameter so all three dialects
// (SQLite/MySQL/Postgres) compare DATETIME against a Go-side time.Time the
// same way — no string-formatted datetime('now', ...) tricks to translate.
func (s *OrchestrationBatchService) Recover() (int, error) {
	cutoff := time.Now().Add(-5 * time.Minute)
	res, err := s.db.Exec(`UPDATE orchestration_batches
		SET summary_status=?, summary_heartbeat_at=NULL, updated_at=CURRENT_TIMESTAMP
		WHERE summary_status=?
		  AND (summary_heartbeat_at IS NULL OR summary_heartbeat_at < ?)`,
		model.SummaryPending, model.SummaryRunning, cutoff)
	if err != nil {
		return 0, fmt.Errorf("recover orchestration_batches: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// batchSelectColumns is the SELECT projection shared by every read-side
// method (Get / GetActiveByRequirement / ListActive). scanBatch scans in the
// same column order; if you add a column to the model, add it here and to
// scanBatch in lockstep.
const batchSelectColumns = `SELECT id, requirement_id, orchestrator_session_id,
	model, work_dir, claude_config_id, total_children,
	status, summary_status, summary_job_id, summary_heartbeat_at,
	created_at, updated_at, completed_at
	FROM orchestration_batches`

// scanBatch maps one row to model.OrchestrationBatch. summary_heartbeat_at
// and completed_at are nullable DATETIME columns, so we read them into
// sql.NullTime and unwrap when present — that keeps the Go side a plain
// *time.Time without forcing callers to handle sql.NullTime.
func scanBatch(rows *sql.Rows) (*model.OrchestrationBatch, error) {
	var b model.OrchestrationBatch
	var heartbeat sql.NullTime
	var completedAt sql.NullTime
	if err := rows.Scan(
		&b.ID, &b.RequirementID, &b.OrchestratorSessionID,
		&b.Model, &b.WorkDir, &b.ClaudeConfigID, &b.TotalChildren,
		&b.Status, &b.SummaryStatus, &b.SummaryJobID, &heartbeat,
		&b.CreatedAt, &b.UpdatedAt, &completedAt,
	); err != nil {
		return nil, err
	}
	if heartbeat.Valid {
		t := heartbeat.Time
		b.SummaryHeartbeatAt = &t
	}
	if completedAt.Valid {
		t := completedAt.Time
		b.CompletedAt = &t
	}
	return &b, nil
}