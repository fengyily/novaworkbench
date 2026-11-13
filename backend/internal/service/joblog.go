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
// the effective model the claude CLI ran with (used by the review job to
// surface "本次 review 使用模型" in the UI); pass "" when not applicable.
//
// errorKind is variadic so the existing call sites keep compiling: the first
// non-empty value is persisted ("stalled" / "idle-timeout" / "terminated").
// It is what makes the durable record agree with what the user saw — before
// it existed, a failed job was persisted as status=done / exit_code=0.
func (s *JobLogService) Save(jobID, reqID, status string, exitCode int, startedAt, finishedAt time.Time, lines []store.LogLine, model string, errorKind ...string) error {
	blob, err := json.Marshal(lines)
	if err != nil {
		return err
	}
	kind := ""
	if len(errorKind) > 0 {
		kind = errorKind[0]
	}
	_, err = s.db.Exec(`INSERT INTO job_logs (job_id, requirement_id, status, exit_code, started_at, finished_at, log, model, error_kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`+
		s.db.OnConflict("job_id", `
			requirement_id = excluded.requirement_id,
			status = excluded.status,
			exit_code = excluded.exit_code,
			started_at = excluded.started_at,
			finished_at = excluded.finished_at,
			log = excluded.log,
			model = excluded.model,
			error_kind = excluded.error_kind`),
		jobID, reqID, status, exitCode, startedAt.Format(time.RFC3339), finishedAt.Format(time.RFC3339), string(blob), model, kind)
	return err
}

// Get loads a persisted job snapshot. Returns sql.ErrNoRows when the job was
// never persisted (e.g. the backend restarted mid-run before the goroutine
// could finish); the handler turns that into a 404. model is the effective
// model recorded at save time (empty when the job predates this column or was
// never given one). errorKind is the machine-readable failure reason
// ("stalled" / "idle-timeout" / "terminated"), empty for a successful job.
func (s *JobLogService) Get(jobID string) (status string, exitCode int, startedAt, finishedAt, model, errorKind string, lines []store.LogLine, err error) {
	var logBlob string
	err = s.db.QueryRow(`SELECT status, exit_code, started_at, finished_at, model, error_kind, log FROM job_logs WHERE job_id = ?`, jobID).
		Scan(&status, &exitCode, &startedAt, &finishedAt, &model, &errorKind, &logBlob)
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
