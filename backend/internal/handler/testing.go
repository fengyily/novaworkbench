package handler

import (
	"errors"
	"net/http"
	"sync"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// TestingHandler serves the 自动化测试 module: the per-project Playwright
// workspace, AI-authored test plans, batch runs and the failure→issue
// close-loop.
//
// Everything expensive here is asynchronous and JobStore-backed. Init
// (npm + browser download), the planner/generator agents and the playwright
// run all routinely take minutes, so every endpoint that starts work returns
// {job_id} immediately and the frontend subscribes to the single shared SSE
// route at GET /api/projects/{id}/testing/stream/{job_id}.
type TestingHandler struct {
	projectSvc *service.ProjectService
	envSvc     *service.TestEnvService
	planSvc    *service.TestPlanService
	runSvc     *service.TestRunService
	llm        *llm.Gateway
	jobs       *store.JobStore
	jobLogSvc  *service.JobLogService
	claudeCfg  *service.ClaudeConfigService
	usageSvc   usageRecorder

	// platformSvc backs the "建 Issue" close-loop. Injected by setter so a
	// build without platform tokens still serves every other endpoint;
	// nil → POST /api/testing/cases/{id}/issue answers 503.
	platformSvc *service.PlatformTokenService
	// reqSvc backs "转为本地需求". nil → that endpoint answers 503.
	reqSvc *service.RequirementService

	// initJobs maps envID → the JobStore id of the in-flight init chain, so
	// a second init attempt can answer 409 with the job_id the caller should
	// attach to instead of leaving the user watching a dead button.
	initJobs *envJobTracker
}

func NewTestingHandler(
	projectSvc *service.ProjectService,
	envSvc *service.TestEnvService,
	planSvc *service.TestPlanService,
	runSvc *service.TestRunService,
	llmGateway *llm.Gateway,
	jobs *store.JobStore,
	jobLogSvc *service.JobLogService,
	claudeCfg *service.ClaudeConfigService,
	usageSvc usageRecorder,
) *TestingHandler {
	return &TestingHandler{
		projectSvc: projectSvc,
		envSvc:     envSvc,
		planSvc:    planSvc,
		runSvc:     runSvc,
		llm:        llmGateway,
		jobs:       jobs,
		jobLogSvc:  jobLogSvc,
		claudeCfg:  claudeCfg,
		usageSvc:   usageSvc,
		initJobs:   newEnvJobTracker(),
	}
}

// SetPlatformSvc wires the "create a GitHub/GitLab/Gitea issue for this
// failing case" action.
func (h *TestingHandler) SetPlatformSvc(svc *service.PlatformTokenService) { h.platformSvc = svc }

// SetRequirementSvc wires "turn this failing case into a local requirement".
func (h *TestingHandler) SetRequirementSvc(svc *service.RequirementService) { h.reqSvc = svc }

// ── shared helpers ──────────────────────────────────────────────────────

// resolveEnvForProject loads an environment and enforces that it belongs to
// the project named in the URL.
//
// The /api/testing/envs/{env_id} routes carry no project id, so without this
// check a user could reach another project's environment — and its base_url,
// seed test and decrypted env vars — by guessing an id. Every direct-resource
// handler routes through here.
func (h *TestingHandler) resolveEnvForProject(w http.ResponseWriter, r *http.Request, projectID, envID string) (envIDResolved string, ok bool) {
	env, err := h.envSvc.Get(envID)
	if errors.Is(err, service.ErrEnvNotFound) {
		writeError(w, http.StatusNotFound, "ENV_NOT_FOUND", "测试环境不存在")
		return "", false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ENV_LOAD_FAILED", err.Error())
		return "", false
	}
	// A project-scoped route (…/projects/{id}/testing/envs/{env_id}) has the
	// project in the path; a direct one must be checked against the row.
	if projectID != "" && env.ProjectID != projectID {
		// 404 rather than 403: confirming the id exists at all is itself a
		// cross-project oracle.
		writeError(w, http.StatusNotFound, "ENV_NOT_FOUND", "测试环境不存在")
		return "", false
	}
	return env.ID, true
}

// workspaceFor resolves the absolute Playwright workspace for an env,
// creating it if needed.
func (h *TestingHandler) workspaceFor(envID string) (string, error) {
	env, err := h.envSvc.Get(envID)
	if err != nil {
		return "", err
	}
	return h.envSvc.EnsureWorkspace(env)
}

// envJobIDs tracks which JobStore job currently owns each environment, so a
// 409 can hand the frontend the in-flight job_id to attach to instead of
// leaving the user watching nothing while npm installs.
type envJobTracker struct {
	mu   sync.Mutex
	jobs map[string]string // envID → jobID
}

func newEnvJobTracker() *envJobTracker {
	return &envJobTracker{jobs: map[string]string{}}
}

func (t *envJobTracker) set(envID, jobID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.jobs[envID] = jobID
}

func (t *envJobTracker) clear(envID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.jobs, envID)
}

func (t *envJobTracker) get(envID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.jobs[envID]
}
