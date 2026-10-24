package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// ListEnvs GET /api/projects/{id}/testing/envs
func (h *TestingHandler) ListEnvs(w http.ResponseWriter, r *http.Request) {
	envs, err := h.envSvc.List(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ENV_LIST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, envs)
}

// CreateEnv POST /api/projects/{id}/testing/envs
func (h *TestingHandler) CreateEnv(w http.ResponseWriter, r *http.Request) {
	var req model.CreateTestEnvReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	env, err := h.envSvc.Create(r.PathValue("id"), req)
	if errors.Is(err, service.ErrEnvNameTaken) {
		writeError(w, http.StatusConflict, "ENV_NAME_TAKEN", "该项目下已存在同名测试环境")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "ENV_CREATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, env)
}

// GetEnv GET /api/testing/envs/{env_id}
func (h *TestingHandler) GetEnv(w http.ResponseWriter, r *http.Request) {
	envID := r.PathValue("env_id")
	if _, ok := h.resolveEnvForProject(w, r, r.PathValue("id"), envID); !ok {
		return
	}
	env, err := h.envSvc.Get(envID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ENV_LOAD_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, env)
}

// UpdateEnv PUT /api/testing/envs/{env_id}
func (h *TestingHandler) UpdateEnv(w http.ResponseWriter, r *http.Request) {
	envID := r.PathValue("env_id")
	if _, ok := h.resolveEnvForProject(w, r, r.PathValue("id"), envID); !ok {
		return
	}
	var req model.UpdateTestEnvReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	env, err := h.envSvc.Update(envID, req)
	if errors.Is(err, service.ErrEnvNameTaken) {
		writeError(w, http.StatusConflict, "ENV_NAME_TAKEN", "该项目下已存在同名测试环境")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "ENV_UPDATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, env)
}

// DeleteEnv DELETE /api/testing/envs/{env_id}[?purge=1]
//
// purge=1 additionally deletes the on-disk workspace. It is opt-in because
// the generated specs may be worth keeping even after detaching the env from
// NovaWorkbench.
func (h *TestingHandler) DeleteEnv(w http.ResponseWriter, r *http.Request) {
	envID := r.PathValue("env_id")
	if _, ok := h.resolveEnvForProject(w, r, r.PathValue("id"), envID); !ok {
		return
	}
	purge := r.URL.Query().Get("purge") == "1"
	if err := h.envSvc.Delete(envID, purge); err != nil {
		if errors.Is(err, service.ErrEnvNotFound) {
			writeError(w, http.StatusNotFound, "ENV_NOT_FOUND", "测试环境不存在")
			return
		}
		writeError(w, http.StatusInternalServerError, "ENV_DELETE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// GetEnvStatus GET /api/testing/envs/{env_id}/status
//
// Init state plus the workspace's on-disk footprint, which is what explains
// a filling disk when retention has not yet caught up.
func (h *TestingHandler) GetEnvStatus(w http.ResponseWriter, r *http.Request) {
	envID := r.PathValue("env_id")
	if _, ok := h.resolveEnvForProject(w, r, r.PathValue("id"), envID); !ok {
		return
	}
	st, err := h.envSvc.Status(envID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "STATUS_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// StreamTestJob GET /api/projects/{id}/testing/stream/{job_id}
//
// The single SSE route shared by every asynchronous testing job: init,
// planner, generator, run and heal. Subscribers get the replayed history
// first, then live frames, then one terminal job_done.
//
// The project in the path is checked against the environment that owns the
// job so a job id cannot be used to read another project's log.
func (h *TestingHandler) StreamTestJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	projectID := r.PathValue("id")

	job, ok := h.jobs.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "JOB_NOT_FOUND", "任务不存在或已结束")
		return
	}

	// job.RequirementID carries the owning row's id (plan / run / env id)
	// for testing jobs; verify it resolves to this project before streaming.
	if !h.jobBelongsToProject(job, projectID) {
		writeError(w, http.StatusNotFound, "JOB_NOT_FOUND", "任务不存在")
		return
	}

	streamJobSSE(w, r, job, func(status store.JobStatus, exitCode int) []byte {
		b, _ := json.Marshal(map[string]any{
			"type":      "job_done",
			"status":    string(status),
			"exit_code": exitCode,
		})
		return b
	})
}

// jobBelongsToProject authorizes a testing job stream.
//
// Jobs are keyed by the id of whatever row spawned them (env / plan / run),
// so ownership is resolved by looking the row up. An id that matches none of
// the three tables is rejected rather than allowed.
func (h *TestingHandler) jobBelongsToProject(job *store.Job, projectID string) bool {
	owner := job.RequirementID
	if owner == "" {
		// No owner recorded: refuse. Every testing job sets one, so this is
		// either a different feature's job or a malformed row.
		return false
	}
	if ok, err := h.envSvc.OwnsProject(owner, projectID); err == nil && ok {
		return true
	}
	if ok, err := h.planSvc.OwnsProject(owner, projectID); err == nil && ok {
		return true
	}
	if ok, err := h.runSvc.OwnsProject(owner, projectID); err == nil && ok {
		return true
	}
	return false
}
