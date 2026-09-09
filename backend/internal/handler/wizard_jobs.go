// wizard_jobs.go: extracted from wizard.go as part of the refactoring.

package handler

import (
	"encoding/json"
	"net/http"
	"github.com/novaworkbench/backend/internal/store"
)

func (h *WizardHandler) StreamJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, 400, "INVALID", "missing job id")
		return
	}
	job, ok := h.jobs.Get(id)
	if !ok {
		writeError(w, 404, "NOT_FOUND", "job not found")
		return
	}

	streamJobSSE(w, r, job, func(status store.JobStatus, exitCode int) []byte {
		doneData, _ := json.Marshal(map[string]interface{}{
			"type":        "job_done",
			"status":      string(status),
			"exit_code":   exitCode,
			"started_at":  job.StartedAt.UnixMilli(),
			"finished_at": job.FinishedAt.UnixMilli(),
			"duration_ms": job.FinishedAt.Sub(job.StartedAt).Milliseconds(),
		})
		return doneData
	})
}

// ArchitectDesign is the architect-phase design generator. It creates a
// background JobStore job, persists its id on the requirement (so a page refresh
// can reconnect to the running job and show "executing" instead of the start
// button), and returns the job id immediately. Claude then runs in plan mode
// (stream-json) in a goroutine, writing progress into the job. On success the
// plan markdown is persisted to design_docs and the design_job_id is cleared;
// the requirement stays status=designing until the user manually marks 方案完成.
//
// Subscribe to the live stream via GET /api/wizard/jobs/{job_id}/stream and
// poll the snapshot via GET /api/wizard/jobs/{job_id} (same pattern as
// start-coding).
// designRunParams is the prepared-shape output of prepareArchitectDesign —
// every input the goroutine body needs after the synchronous validation +
// job creation + prompt build has succeeded. Splitting it out lets the
// scheduler path reuse the exact same exec body via RunScheduledDesign, so
// HTTP-driven and time-driven dispatches share one implementation of the
// architect stage (no parallel maintenance).
func (h *WizardHandler) GetJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, 400, "INVALID", "missing job id")
		return
	}
	job, ok := h.jobs.Get(id)
	if !ok {
		// Backend may have restarted since the job ran — try the durable log
		// store so the user can still review the development record.
		status, exitCode, startedAt, finishedAt, jobModel, lines, perr := h.jobLogSvc.Get(id)
		if perr != nil {
			writeError(w, 404, "NOT_FOUND", "job not found")
			return
		}
		writeJSON(w, 200, map[string]interface{}{
			"job_id":      id,
			"status":      status,
			"exit_code":   exitCode,
			"log":         lines,
			"model":       jobModel,
			"started_at":  startedAt,
			"finished_at": finishedAt,
		})
		return
	}
	logLines, status, exitCode := job.Snapshot()
	writeJSON(w, 200, map[string]interface{}{
		"job_id":      job.ID,
		"status":      status,
		"exit_code":   exitCode,
		"log":         logLines,
		"model":       job.Model,
		"started_at":  job.StartedAt,
		"finished_at": job.FinishedAt,
	})
}

// GetActiveJobs returns all currently-running wizard jobs across the process.
// Used by list / detail pages to badge "Claude 工作中" without N+1 polling
// each requirement's *_job_id columns. Returns an empty array (not null) when
// no jobs are running so the JSON shape stays stable for the frontend.
func (h *WizardHandler) GetActiveJobs(w http.ResponseWriter, r *http.Request) {
	jobs := h.jobs.ActiveJobs()
	if jobs == nil {
		jobs = []store.ActiveJob{}
	}
	writeJSON(w, 200, map[string]any{"jobs": jobs})
}

// toolResultContent extracts and truncates the content of a tool_result block.
