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

// ScheduledTaskService is the persistence layer for one-shot, future-dated
// wizard tasks. It mirrors SubTaskService in style — thin SQL wrapper over
// *db.DB; the scheduler package owns the polling/claiming loop, the wizard
// executor owns the actual claude invocation. The service is only responsible
// for the rows themselves plus the atomic claim used by the scheduler.
//
// Concurrency: Claim uses UPDATE ... WHERE status='pending' so two pollers
// racing on the same row only see RowsAffected==1 once; the loser sees 0 and
// moves on. HasPending is a read-side guard used by the create handler to
// enforce "at most one pending per (requirement, task_type)" before insert.
type ScheduledTaskService struct {
	db *db.DB
}

// ErrAlreadyScheduled is returned by Create when (requirement_id, task_type)
// already has a pending row. The handler maps it to a 409 ALREADY_SCHEDULED.
var ErrAlreadyScheduled = errors.New("a pending scheduled task already exists for this requirement and task_type")

// ErrNotPending is returned by Cancel when the row is no longer pending
// (running, succeeded, failed, canceled). The handler maps it to a 409.
var ErrNotPending = errors.New("scheduled task is not in pending state")

// ErrNotFound is returned by Get / Delete when no row matches the id.
var ErrNotFound = errors.New("scheduled task not found")

func NewScheduledTaskService(database *db.DB) *ScheduledTaskService {
	return &ScheduledTaskService{db: database}
}

// Create inserts a new pending row. Validates task_type, fills id/created_at/
// updated_at, and short-circuits on a duplicate pending row for the same
// (requirement_id, task_type). Returns the persisted row so the handler can
// surface the new id without a second round-trip.
func (s *ScheduledTaskService) Create(t *model.ScheduledTask) (*model.ScheduledTask, error) {
	if t.TaskType != model.SchedTypeDesign && t.TaskType != model.SchedTypeCoding {
		return nil, fmt.Errorf("invalid task_type %q (want design|coding)", t.TaskType)
	}
	if t.RequirementID == "" {
		return nil, errors.New("requirement_id is required")
	}
	if t.RunAt.IsZero() {
		return nil, errors.New("run_at is required")
	}
	dup, err := s.HasPending(t.RequirementID, t.TaskType)
	if err != nil {
		return nil, err
	}
	if dup {
		return nil, ErrAlreadyScheduled
	}
	if t.ID == "" {
		t.ID = util.NewID("sched")
	}
	// Normalize to UTC before persisting. The modernc SQLite driver stores
	// time.Time via time.Time.String(), and that layout uses the zone name
	// in the trailing field; a time in a FixedZone whose name is empty
	// (which is exactly what time.Parse(RFC3339, "...+08:00") returns)
	// round-trips as "2026-09-07 23:30:00 +0800 +0800" — a string the
	// driver can't read back, so Scan into *time.Time fails on List. UTC
	// is always emitted as "... +0000 UTC", which IS parseable. The
	// absolute moment is preserved (UTC and the original offset refer to
	// the same instant); the caller already chose the wall-clock value via
	// parseRunAt, which preserves the offset in the returned time.Time.
	t.RunAt = t.RunAt.UTC()
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	if t.Status == "" {
		t.Status = model.SchedStatusPending
	}
	_, err = s.db.Exec(`INSERT INTO scheduled_tasks
		(id, task_type, requirement_id, project_id, requirement_title,
		 run_at, model, read_knowledge, branch_name, base_branch,
		 agent_server_id, split_tasks, status, job_id, error_message,
		 created_by, created_at, updated_at, executed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.TaskType, t.RequirementID, t.ProjectID, t.RequirementTitle,
		t.RunAt, t.Model, t.ReadKnowledge, t.BranchName, t.BaseBranch,
		t.AgentServerID, t.SplitTasks, t.Status, t.JobID, t.ErrorMessage,
		t.CreatedBy, t.CreatedAt, t.UpdatedAt, t.ExecutedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert scheduled_task: %w", err)
	}
	return t, nil
}

// List returns every scheduled task matching the filters, oldest run_at first.
// Empty filters return all tasks (small table, no pagination needed yet). Any
// nil/empty slice is replaced with []model.ScheduledTask{} so the frontend can
// map directly. Filter on task_type is done Go-side because task_type is a
// non-indexed TEXT column (MySQL can't index it without the indexed-column
// whitelist); status and requirement_id use indexes.
func (s *ScheduledTaskService) List(status, taskType, requirementID string) ([]model.ScheduledTask, error) {
	var clauses []string
	var args []any
	if status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, status)
	}
	if requirementID != "" {
		clauses = append(clauses, "requirement_id = ?")
		args = append(args, requirementID)
	}
	query := `SELECT id, task_type, requirement_id, project_id, requirement_title,
		run_at, model, read_knowledge, branch_name, base_branch,
		agent_server_id, split_tasks, status, job_id, error_message,
		created_by, created_at, updated_at, executed_at
		FROM scheduled_tasks`
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY run_at ASC, id ASC"
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ScheduledTask{}
	for rows.Next() {
		t, err := scanScheduledTask(rows)
		if err != nil {
			return nil, err
		}
		if taskType != "" && t.TaskType != taskType {
			continue
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// Get loads one row by id. Returns ErrNotFound when no row matches.
func (s *ScheduledTaskService) Get(id string) (*model.ScheduledTask, error) {
	rows, err := s.db.Query(`SELECT id, task_type, requirement_id, project_id, requirement_title,
		run_at, model, read_knowledge, branch_name, base_branch,
		agent_server_id, split_tasks, status, job_id, error_message,
		created_by, created_at, updated_at, executed_at
		FROM scheduled_tasks WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrNotFound
	}
	return scanScheduledTask(rows)
}

// Due returns every pending row whose run_at is at or before now, oldest
// first. The scheduler caps to 50 per tick to bound the SELECT cost and the
// per-tick dispatch fan-out.
func (s *ScheduledTaskService) Due(now time.Time, limit int) ([]model.ScheduledTask, error) {
	rows, err := s.db.Query(`SELECT id, task_type, requirement_id, project_id, requirement_title,
		run_at, model, read_knowledge, branch_name, base_branch,
		agent_server_id, split_tasks, status, job_id, error_message,
		created_by, created_at, updated_at, executed_at
		FROM scheduled_tasks
		WHERE status = ? AND run_at <= ?
		ORDER BY run_at ASC LIMIT ?`,
		model.SchedStatusPending, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ScheduledTask{}
	for rows.Next() {
		t, err := scanScheduledTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// Claim atomically transitions a row from pending → running and stamps
// executed_at. Returns true on success (the caller should dispatch), false
// when the row was already taken by another poller or the user canceled it
// in the window between Due and Claim.
func (s *ScheduledTaskService) Claim(id string, now time.Time) (bool, error) {
	res, err := s.db.Exec(`UPDATE scheduled_tasks SET status = ?, executed_at = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		model.SchedStatusRunning, now, now, id, model.SchedStatusPending)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Finish records the terminal state of a dispatched task: succeeded/failed,
// the JobStore job id (so the list page can deep-link to /api/wizard/jobs/...),
// and any error string. Pass ok=true for success, false for failure.
func (s *ScheduledTaskService) Finish(id string, ok bool, jobID, errMsg string) error {
	status := model.SchedStatusSucceeded
	if !ok {
		status = model.SchedStatusFailed
	}
	_, err := s.db.Exec(`UPDATE scheduled_tasks SET status = ?, job_id = ?, error_message = ?, updated_at = ?
		WHERE id = ?`,
		status, jobID, errMsg, time.Now(), id)
	return err
}

// Cancel transitions a pending row to canceled. Returns ErrNotPending for
// any other status (the caller surfaces a 409 telling the user the task is
// already running or already finished and cannot be canceled).
func (s *ScheduledTaskService) Cancel(id string) error {
	res, err := s.db.Exec(`UPDATE scheduled_tasks SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		model.SchedStatusCanceled, time.Now(), id, model.SchedStatusPending)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotPending
	}
	return nil
}

// Delete removes the row regardless of status — pending, running, and
// terminal-state rows all delete. The atomic Claim (UPDATE ... WHERE id=? AND
// status='pending') guards the scheduler race: a row deleted between the
// scheduler's Due() scan and its Claim() simply has no rows to update, and
// the dispatcher skips it. The pre-check that used to forbid pending deletes
// lived in the handler; it was removed because it forced users into a
// two-step "cancel then delete" with no real safety benefit.
func (s *ScheduledTaskService) Delete(id string) error {
	res, err := s.db.Exec(`DELETE FROM scheduled_tasks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// FailExpired marks pending rows older than cutoff as failed. Called by the
// scheduler on each tick to retire zombie tasks that piled up while the
// server was offline (run_at may be days/weeks in the past). cutoff is the
// floor — tasks older than (now - cutoff) are stale; tasks within the window
// get a normal dispatch attempt.
func (s *ScheduledTaskService) FailExpired(now time.Time, cutoff time.Duration, message string) (int64, error) {
	floor := now.Add(-cutoff)
	res, err := s.db.Exec(`UPDATE scheduled_tasks SET status = ?, error_message = ?, updated_at = ?
		WHERE status = ? AND run_at < ?`,
		model.SchedStatusFailed, message, now, model.SchedStatusPending, floor)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RecoverInterrupted marks every running row as failed at server startup —
// the goroutine that owned them died with the previous process. Mirrors
// SubTaskService.RecoverInterrupted; the scheduler calls this once before
// Start. Returns the number of rows recovered.
func (s *ScheduledTaskService) RecoverInterrupted() (int64, error) {
	now := time.Now()
	res, err := s.db.Exec(`UPDATE scheduled_tasks SET status = ?, error_message = ?, updated_at = ?
		WHERE status = ?`,
		model.SchedStatusFailed,
		"服务在定时任务执行期间重启，执行中断。请到需求详情页手动重试。",
		now, model.SchedStatusRunning)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// HasPending reports whether the (requirement_id, task_type) pair already has
// a pending row. Used by Create to enforce uniqueness and by the wizard
// detail page to render the "已定时 HH:MM" hint.
func (s *ScheduledTaskService) HasPending(requirementID, taskType string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM scheduled_tasks
		WHERE requirement_id = ? AND task_type = ? AND status = ?`,
		requirementID, taskType, model.SchedStatusPending).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// scanScheduledTask is the shared row→struct mapper for List / Get / Due.
// Pulled out so the column order lives in exactly one spot.
//
// run_at is scanned as a string rather than *time.Time because the modernc
// SQLite driver stores Go time.Time via time.Time.String(), which produces
// formats that aren't always round-trippable: when the time is in a
// FixedZone with no 3-letter name (which is exactly what
// time.Parse(time.RFC3339, "+08:00"-suffixed input) produces), the stored
// text looks like "2026-09-07 23:30:00 +0800 +0800" and the driver's
// parseTime parser falls through every format it knows about. Direct
// Scan into *time.Time then fails with
//
//	sql: Scan error on column index 5, name "run_at":
//	unsupported Scan, storing driver.Value type string into type *time.Time
//
// — which is what surfaced on the user's prod list. parseRunAtString
// tries every format the modernc driver uses (plus the standard RFC3339 /
// datetime-local forms) so legacy rows from before this fix continue to
// list without erroring the whole request. See
// service.TestScheduledTask_LegacyRunAtString for the regression pin.
func scanScheduledTask(rows *sql.Rows) (*model.ScheduledTask, error) {
	var t model.ScheduledTask
	var executedAt sql.NullTime
	var runAtRaw sql.NullString
	if err := rows.Scan(
		&t.ID, &t.TaskType, &t.RequirementID, &t.ProjectID, &t.RequirementTitle,
		&runAtRaw, &t.Model, &t.ReadKnowledge, &t.BranchName, &t.BaseBranch,
		&t.AgentServerID, &t.SplitTasks, &t.Status, &t.JobID, &t.ErrorMessage,
		&t.CreatedBy, &t.CreatedAt, &t.UpdatedAt, &executedAt,
	); err != nil {
		return nil, err
	}
	if runAtRaw.Valid {
		t.RunAt = parseRunAtString(runAtRaw.String)
	}
	if executedAt.Valid {
		tt := executedAt.Time
		t.ExecutedAt = &tt
	}
	return &t, nil
}

// runAtParseFormats is the ordered list of layouts parseRunAtString tries.
// Order matters: more specific / longer layouts come first so the optional
// fractional-seconds variants aren't matched against the bare datetime-local
// fallback (which has no offset). The set mirrors what modernc.org/sqlite
// uses internally plus the wire formats the handler accepts in
// parseRunAt (RFC3339, datetime-local).
var runAtParseFormats = []string{
	// modernc driver output for time.Time.String() — used by writes from
	// this service until the UTC normalization in Create, and by any pre-fix
	// rows already in the DB.
	"2006-01-02 15:04:05.999999999 -0700 MST",
	// modernc driver output when _time_format=sqlite is set on the DSN.
	"2006-01-02 15:04:05.999999999-07:00",
	// Standard wire formats the frontend may post via the API.
	time.RFC3339Nano,
	time.RFC3339,
	// Variants without zone — produced by `time.Time.Format` calls or by
	// hand-crafted SQL inserts; the user-facing API rejects these but the
	// DB might still hold one.
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	// datetime-local without seconds — legacy handler-side fallback in
	// parseRunAt. time.Parse interprets this as UTC (no zone in layout),
	// which differs from the handler's ParseInLocation semantic but is
	// still a meaningful absolute moment for the list endpoint to show.
	"2006-01-02T15:04",
	"2006-01-02",
}

// parseRunAtString best-effort parses a run_at cell that came back from
// SQLite. It returns time.Time{} on total failure rather than an error
// because the row's other fields are still useful — failing the whole list
// because one legacy row has a bad run_at is the bug we're fixing.
//
// Callers that need to distinguish "unset" from "valid but zero" should
// check runAtRaw.Valid at the Scan site; this helper never sees that bit.
func parseRunAtString(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range runAtParseFormats {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}