package service

import (
	"encoding/json"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/store"
)

// JobLogService persists finished coding job logs to SQLite so they survive a
// backend restart (the in-memory JobStore is lost on restart). Only the final
// snapshot is written — once per job, on completion.
type JobLogService struct {
	db *db.DB
}

func NewJobLogService(db *db.DB) *JobLogService {
	return &JobLogService{db: db}
}

// Save upserts a finished job's full log snapshot. startedAt/finishedAt are
// passed in by the caller (the Job's timestamps) — this func must not call
// time.Now() itself, to stay consistent with the in-memory Job state. model is
// the effective model the job ran with (empty when unset); it is persisted so
// the UI can show which model a finished review/coding job used even after a
// backend restart evicts the in-memory JobStore ring buffer.
func (s *JobLogService) Save(jobID, reqID, status string, exitCode int, startedAt, finishedAt time.Time, lines []store.LogLine, model string) error {
	blob, err := json.Marshal(lines)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO job_logs (job_id, requirement_id, status, exit_code, started_at, finished_at, log, model)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`+
		s.db.OnConflict("job_id", `
			requirement_id = excluded.requirement_id,
			status = excluded.status,
			exit_code = excluded.exit_code,
			started_at = excluded.started_at,
			finished_at = excluded.finished_at,
			log = excluded.log,
			model = excluded.model`),
		jobID, reqID, status, exitCode, startedAt.Format(time.RFC3339), finishedAt.Format(time.RFC3339), string(blob), model)
	return err
}

// Get loads a persisted job snapshot. Returns sql.ErrNoRows when the job was
// never persisted (e.g. the backend restarted mid-run before the goroutine
// could finish); the handler turns that into a 404.
func (s *JobLogService) Get(jobID string) (status string, exitCode int, startedAt, finishedAt, model string, lines []store.LogLine, err error) {
	var logBlob string
	err = s.db.QueryRow(`SELECT status, exit_code, started_at, finished_at, model, log FROM job_logs WHERE job_id = ?`, jobID).
		Scan(&status, &exitCode, &startedAt, &finishedAt, &model, &logBlob)
	if err != nil {
		return
	}
	if logBlob != "" {
		_ = json.Unmarshal([]byte(logBlob), &lines)
	}
	if lines == nil {
		lines = []store.LogLine{}
	}
	return
}
