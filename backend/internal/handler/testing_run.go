package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// runPlaywrightTimeout caps one `npx playwright test` invocation. Larger than
// the agent turn budget: a suite that actually runs has no per-agent stall
// watchdog, and a large batch against a slow app legitimately takes an hour.
const runPlaywrightTimeout = 60 * time.Minute

// activeRuns maps envID → the in-flight run's job id. Two runs against one
// workspace would fight over the same working directory and the same
// runs/ directory, so the second is refused with a 409 carrying the job id
// to attach to.
var activeRuns = newEnvJobTracker()

// StartRun POST /api/testing/envs/{env_id}/runs
//
// Launches a batch run as a background job. Returns {run_id, job_id}.
func (h *TestingHandler) StartRun(w http.ResponseWriter, r *http.Request) {
	envID := r.PathValue("env_id")
	projectID := r.PathValue("id")

	env, err := h.envSvc.Get(envID)
	if errors.Is(err, service.ErrEnvNotFound) {
		writeError(w, http.StatusNotFound, "ENV_NOT_FOUND", "测试环境不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ENV_LOAD_FAILED", err.Error())
		return
	}
	if projectID != "" && env.ProjectID != projectID {
		writeError(w, http.StatusNotFound, "ENV_NOT_FOUND", "测试环境不存在")
		return
	}
	if env.InitStatus != model.TestEnvStatusReady {
		writeErrorSuggestion(w, http.StatusConflict, "ENV_NOT_READY",
			"环境尚未初始化完成，无法运行测试", "请先完成环境初始化。")
		return
	}

	var req model.StartRunReq
	// An empty body is valid: it means "run everything".
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	files, err := h.runSvc.ResolveScope(envID, req.Scope)
	if err != nil {
		writeError(w, http.StatusBadRequest, "SCOPE_INVALID", err.Error())
		return
	}

	// Reserve the environment before doing anything expensive.
	if existing := activeRuns.get(envID); existing != "" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]string{
				"code":    "ENV_BUSY",
				"message": "该环境已有测试正在运行",
			},
			"job_id": existing,
		})
		return
	}

	run, err := h.runSvc.Create(env.ProjectID, envID, req.Scope, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "RUN_CREATE_FAILED", err.Error())
		return
	}
	job := h.jobs.Create(run.ID)
	activeRuns.set(envID, job.ID)

	// Bind the run row to its job so the list view can show which run is live.
	if err := h.runSvc.SetJobID(run.ID, job.ID); err != nil {
		log.Printf("[testing] set job id for run %s: %v", run.ID, err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"run_id": run.ID,
		"job_id": job.ID,
		"status": model.TestRunStatusRunning,
	})

	go h.runPlaywrightBatch(job, run, env, files, req)
}

// CancelRun POST /api/testing/runs/{run_id}/cancel
func (h *TestingHandler) CancelRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	run, err := h.runSvc.Get(runID)
	if errors.Is(err, service.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, "RUN_NOT_FOUND", "测试运行不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "RUN_LOAD_FAILED", err.Error())
		return
	}
	if run.Status != model.TestRunStatusRunning {
		writeError(w, http.StatusConflict, "RUN_NOT_RUNNING", "该测试运行已结束")
		return
	}
	// Cancel through the JobStore so the ctx (and therefore the subprocess)
	// is torn down; runPlaywrightBatch records the 'canceled' status when it
	// observes the exit.
	if job, ok := h.jobs.Get(run.JobID); ok {
		job.Cancel()
	}
	_ = h.runSvc.FinishRun(runID, model.TestRunStatusCanceled, "用户取消")
	writeJSON(w, http.StatusOK, map[string]string{"status": model.TestRunStatusCanceled})
}

// GetRun GET /api/testing/runs/{run_id} — the run plus its cases.
func (h *TestingHandler) GetRun(w http.ResponseWriter, r *http.Request) {
	run, err := h.runSvc.Get(r.PathValue("run_id"))
	if errors.Is(err, service.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, "RUN_NOT_FOUND", "测试运行不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "RUN_LOAD_FAILED", err.Error())
		return
	}
	cases, err := h.runSvc.GetCases(run.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CASES_LOAD_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "cases": cases})
}

// ListRuns GET /api/projects/{id}/testing/runs?env_id=
func (h *TestingHandler) ListRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := h.runSvc.List(r.PathValue("id"), r.URL.Query().Get("env_id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "RUN_LIST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

// DeleteRun DELETE /api/testing/runs/{run_id}
func (h *TestingHandler) DeleteRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	run, err := h.runSvc.Get(runID)
	if errors.Is(err, service.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, "RUN_NOT_FOUND", "测试运行不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "RUN_LOAD_FAILED", err.Error())
		return
	}
	if run.Status == model.TestRunStatusRunning {
		writeError(w, http.StatusConflict, "RUN_RUNNING", "请先停止正在运行的测试")
		return
	}
	if err := h.runSvc.Delete(runID); err != nil {
		writeError(w, http.StatusInternalServerError, "RUN_DELETE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ServeArtifact GET /api/testing/runs/{run_id}/files/{path...}
//
// Serves screenshots and traces from the run directory. The path guard is
// the whole security story here: `path` comes straight from the URL, and
// the join happens against the run directory, so an unvalidated value would
// be a directory traversal primitive.
//
// The stored path (e.g. "artifacts/shot.png") maps 1:1 onto the URL, so the
// route segment is "files" rather than "artifacts" — otherwise Playwright's
// own artifacts/ directory would collide with a literal route segment and
// every URL would need a doubled "artifacts/artifacts".
func (h *TestingHandler) ServeArtifact(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	run, err := h.runSvc.Get(runID)
	if errors.Is(err, service.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, "RUN_NOT_FOUND", "测试运行不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "RUN_LOAD_FAILED", err.Error())
		return
	}
	runDir, err := h.runSvc.RunDir(run)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "RUN_DIR_FAILED", err.Error())
		return
	}

	// SafeRelativePath is the single guard: it resolves the path relative to
	// runDir and refuses anything that escapes, using filepath.Rel's ".."
	// check rather than a string prefix (which "runs/x-evil" would slip past).
	rel, ok := service.SafeRelativePath(r.PathValue("path"), runDir)
	if !ok {
		writeError(w, http.StatusBadRequest, "BAD_ARTIFACT_PATH", "非法的证据路径")
		return
	}

	abs := filepath.Join(runDir, filepath.FromSlash(rel))
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		writeError(w, http.StatusNotFound, "ARTIFACT_NOT_FOUND", "证据文件不存在")
		return
	}

	ctype := mime.TypeByExtension(strings.ToLower(filepath.Ext(abs)))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("inline; filename=%q", filepath.Base(abs)))
	http.ServeFile(w, r, abs)
}

// ── execution ──────────────────────────────────────────────────────────

// runPlaywrightBatch executes the resolved spec files, ingests the report
// and — when requested and useful — runs the healer chain.
func (h *TestingHandler) runPlaywrightBatch(job *store.Job, run *model.TestRun, env *model.TestEnvironment, files []string, req model.StartRunReq) {
	defer activeRuns.clear(env.ID)

	workspace, err := h.envSvc.EnsureWorkspace(env)
	if err != nil {
		h.failRun(job, run, "准备工作区失败: "+err.Error())
		return
	}

	exitCode, err := h.execPlaywright(job, workspace, env, run.ID, files)
	if err != nil {
		h.failRun(job, run, err.Error())
		return
	}

	// The JSON report is written by the reporter (see the config template in
	// testing_init.go) rather than parsed off stdout, because stdout also
	// carries the human-readable `list` output and the two would interleave
	// into something unparseable.
	reportPath := filepath.Join(workspace, "runs", run.ID, "report.json")
	if err := h.runSvc.IngestReport(run.ID, reportPath); err != nil {
		// A missing/corrupt report means we cannot tell pass from fail —
		// 'error' is the honest answer, not a silent pass.
		h.failRun(job, run, "解析测试报告失败: "+err.Error())
		return
	}
	_ = h.runSvc.SetReportPath(run.ID, reportPath)

	updated, err := h.runSvc.Get(run.ID)
	if err != nil {
		h.failRun(job, run, err.Error())
		return
	}

	// The healer only makes sense when something actually failed.
	if req.Healer && updated.Status == model.TestRunStatusFailed {
		h.runHealerChain(job, run, env, workspace, updated)
		return
	}

	// Trim AFTER a successful ingest so the run just recorded is not among
	// the victims of its own retention pass.
	if err := h.runSvc.TrimRetention(env.ID); err != nil {
		job.Append(store.LogLine{Type: "message", Content: "清理历史运行失败: " + err.Error()})
	}

	_ = exitCode
	job.Append(store.LogLine{Type: "done", Content: summarizeRun(updated)})
	job.Finish(0, store.JobDone)
	log.Printf("[testing] run %s finished: %s", run.ID, summarizeRun(updated))
}

// execPlaywright runs `npx playwright test` over the given files, streaming
// output into the job log. It returns the process exit code.
func (h *TestingHandler) execPlaywright(job *store.Job, workspace string, env *model.TestEnvironment, runID string, files []string) (int, error) {
	runDir := filepath.Join(workspace, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return 0, fmt.Errorf("创建运行目录失败: %w", err)
	}

	// --reporter=list keeps stdout readable for the user; the JSON reporter
	// is configured in playwright.config.ts to write to reportPath.
	args := append([]string{"playwright", "test"}, files...)
	args = append(args, "--reporter=list", "--output="+filepath.Join("runs", runID))

	ctx, cancel := context.WithTimeout(context.Background(), runPlaywrightTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "npx", args...)
	cmd.Dir = workspace

	cmd.Env = append(os.Environ(),
		// NOVA_BASE_URL is what playwright.config.ts reads for baseURL, so
		// editing an environment's base_url takes effect on the next run
		// with no re-init.
		"NOVA_BASE_URL="+env.BaseURL,
		"CI=1",
		"PLAYWRIGHT_JSON_OUTPUT_NAME="+filepath.Join("runs", runID, "report.json"),
	)
	// User-supplied env vars go in last so they can override the defaults
	// above if a project genuinely needs to.
	if kv, err := h.envSvc.EnvVarsForRun(env); err == nil {
		for k, v := range kv {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}

	job.SetSecrets(h.testingSecretValues(env))
	job.SetCmd(cmd, cancel)

	job.Append(store.LogLine{Type: "phase", Content: fmt.Sprintf("运行 %d 个测试文件", len(files))})
	for _, f := range files {
		job.Append(store.LogLine{Type: "message", Content: "· " + f})
	}

	exitCode, runErr := streamProcess(job, ctx, cmd, workspace)
	// Exit code 1 means "some tests failed" — a normal, expected outcome that
	// must NOT be treated as a process error. Anything else is a real
	// failure to even run the suite.
	if runErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return exitCode, fmt.Errorf("测试运行超时（超过 %s）", runPlaywrightTimeout)
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) && exitCode == 1 {
			return exitCode, nil
		}
		if exitCode != 0 && exitCode != 1 {
			return exitCode, fmt.Errorf("playwright test 异常退出（exit %d）: %v", exitCode, runErr)
		}
		return exitCode, runErr
	}
	return exitCode, nil
}

// runHealerChain asks the Playwright healer agent to repair the failing
// specs, then re-runs just those files to verify the fix. The verification
// is recorded as a NEW run pointing back at the original via healed_from,
// so the before/after comparison survives.
func (h *TestingHandler) runHealerChain(job *store.Job, run *model.TestRun, env *model.TestEnvironment, workspace string, failed *model.TestRun) {
	failing, err := h.runSvc.FailingCases(run.ID)
	if err != nil || len(failing) == 0 {
		h.finishRun(job, run, "无需修复")
		return
	}

	// Distinct files only — one entry per spec, not per case.
	fileSet := map[string]bool{}
	for _, c := range failing {
		if c.File != "" {
			fileSet[c.File] = true
		}
	}
	files := make([]string, 0, len(fileSet))
	for f := range fileSet {
		files = append(files, f)
	}

	job.Append(store.LogLine{Type: "phase", Content: fmt.Sprintf("Healer 正在修复 %d 个文件", len(files))})

	_ = h.runSvc.MarkHealerUsed(run.ID)

	outcome := h.runTestingAgent(job, agentRequest{
		Scope:   "testing-healer",
		Prompt:  healerPrompt(failing, files, env),
		Env:     env,
		WorkDir: workspace,
		Model:   cliModelArg(env.DefaultModel),
		Timeout: agentTurnTimeout,
	})
	if outcome.errMsg != "" {
		job.Append(store.LogLine{Type: "error", Content: "Healer 执行失败: " + outcome.errMsg})
		h.finishRun(job, run, "Healer 失败")
		return
	}

	// Verification run: same environment, only the repaired files, and
	// healed_from pointing back so the UI can show the pair.
	vRun, err := h.runSvc.Create(run.ProjectID, run.EnvID, model.RunScope{SpecFiles: files}, "")
	if err != nil {
		job.Append(store.LogLine{Type: "error", Content: "创建验证运行失败: " + err.Error()})
		h.finishRun(job, run, "验证运行创建失败")
		return
	}
	_ = h.runSvc.SetHealedFrom(vRun.ID, run.ID)

	if _, err := h.execPlaywright(job, workspace, env, vRun.ID, files); err != nil {
		_ = h.runSvc.FinishRun(vRun.ID, model.TestRunStatusError, err.Error())
		h.finishRun(job, run, "验证运行失败")
		return
	}
	vReport := filepath.Join(workspace, "runs", vRun.ID, "report.json")
	if err := h.runSvc.IngestReport(vRun.ID, vReport); err != nil {
		_ = h.runSvc.FinishRun(vRun.ID, model.TestRunStatusError, err.Error())
		h.finishRun(job, run, "验证报告解析失败")
		return
	}
	verified, _ := h.runSvc.Get(vRun.ID)

	if verified.Status == model.TestRunStatusPassed {
		job.Append(store.LogLine{Type: "done", Content: fmt.Sprintf("✅ Healer 修复成功（验证运行 %s）", vRun.ID)})
		job.Finish(0, store.JobDone)
		return
	}
	h.finishRun(job, run, fmt.Sprintf("Healer 未修复全部失败（验证运行 %s 仍有失败）", vRun.ID))
}

// failRun records a process-level failure.
func (h *TestingHandler) failRun(job *store.Job, run *model.TestRun, msg string) {
	job.Append(store.LogLine{Type: "error", Content: msg})
	_ = h.runSvc.FinishRun(run.ID, model.TestRunStatusError, msg)
	job.Append(store.LogLine{Type: "done", Content: "测试运行异常终止"})
	job.Finish(1, store.JobError)
	log.Printf("[testing] run %s errored: %s", run.ID, msg)
}

// finishRun records a normal terminal state for a run.
func (h *TestingHandler) finishRun(job *store.Job, run *model.TestRun, msg string) {
	_ = h.runSvc.TrimRetention(run.EnvID)
	job.Append(store.LogLine{Type: "done", Content: msg})
	job.Finish(0, store.JobDone)
}

// summarizeRun renders the terminal one-liner for the job log.
func summarizeRun(r *model.TestRun) string {
	if r == nil {
		return "测试运行完成"
	}
	state := "全部通过"
	if r.Status == model.TestRunStatusFailed {
		state = "存在失败"
	}
	return fmt.Sprintf("%s：%d 通过 / %d 失败 / %d 不稳定 / %d 跳过",
		state, r.Passed, r.Failed, r.Flaky, r.Skipped)
}

// healerPrompt hands the healer agent the failing cases so it can reproduce
// and repair them.
func healerPrompt(failing []*model.TestRunCase, files []string, env *model.TestEnvironment) string {
	var b strings.Builder
	b.WriteString("Use the playwright-test-healer subagent (see .claude/agents/playwright-test-healer.md).\n\n")
	b.WriteString("The following tests failed. Diagnose why and repair the spec files so they pass.\n")
	b.WriteString("Fix the TESTS, not the application — if a failure is a genuine product bug, leave the test failing and say so in your summary.\n\n")
	b.WriteString("Failing files:\n")
	for _, f := range files {
		fmt.Fprintf(&b, "- %s\n", f)
	}
	b.WriteString("\nFailure details:\n")
	for _, c := range failing {
		fmt.Fprintf(&b, "\n### %s\nfile: %s\nerror: %s\n", c.Title, c.File, truncateForPrompt(c.Error, 2000))
	}
	fmt.Fprintf(&b, "\nThe application under test is at %s.\n", env.BaseURL)
	return b.String()
}

// truncateForPrompt bounds one error excerpt so a single DOM-dumping failure
// cannot crowd out the rest.
func truncateForPrompt(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n… (已截断)"
}
