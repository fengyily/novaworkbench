package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// Playwright install can take a long time on a cold cache (browser binaries
// are ~150 MB for chromium alone) and the agent generation is quick. The
// per-step budgets are deliberately lopsided.
const (
	initNpmInstallTimeout = 5 * time.Minute
	// Browser download is the slow step and the one most likely to hit a
	// network stall, so it gets its own generous budget rather than
	// sharing the npm one.
	initBrowserTimeout   = 20 * time.Minute
	initAgentsTimeout    = 2 * time.Minute
	initNpmInitTimeout   = 60 * time.Second
	initVersionTimeout   = 30 * time.Second
	playwrightDepVersion = "@playwright/test@^1.56"
)

// playwrightAgentFiles are the subagent definitions `init-agents` generates.
// Their presence is what proves the step worked, and the planner handler
// refuses to start without the planner one — an agent that silently isn't
// there would otherwise spin for 30 minutes against a bare workspace.
var playwrightAgentFiles = []string{
	"playwright-test-planner",
	"playwright-test-generator",
	"playwright-test-healer",
}

// StartEnvInit POST /api/testing/envs/{env_id}/init
//
// Runs the one-time workspace setup as a background job. Returns
// {job_id} immediately; the frontend subscribes to the shared testing SSE
// route to watch the five steps stream in.
func (h *TestingHandler) StartEnvInit(w http.ResponseWriter, r *http.Request) {
	envID := r.PathValue("env_id")
	projectID := r.PathValue("id") // empty on the direct route
	if _, ok := h.resolveEnvForProject(w, r, projectID, envID); !ok {
		return
	}

	// ClaimInit is the atomic guard: it refuses when a chain is already
	// running, which is what stops a double-click from launching two npm
	// installs into one directory.
	env, err := h.envSvc.ClaimInit(envID)
	if err != nil {
		if errors.Is(err, service.ErrEnvBusy) {
			// Hand back the in-flight job so the caller can attach to it.
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": map[string]string{
					"code":    "ENV_BUSY",
					"message": "该环境正在初始化中",
				},
				"job_id": h.initJobs.get(envID),
			})
			return
		}
		if errors.Is(err, service.ErrEnvNotFound) {
			writeError(w, http.StatusNotFound, "ENV_NOT_FOUND", "测试环境不存在")
			return
		}
		writeError(w, http.StatusInternalServerError, "INIT_CLAIM_FAILED", err.Error())
		return
	}

	job := h.jobs.Create(envID)
	h.initJobs.set(envID, job.ID)

	writeJSON(w, http.StatusOK, map[string]any{
		"env_id": envID,
		"job_id": job.ID,
		"status": model.TestEnvStatusInitializing,
	})

	go h.runInitChain(job, env)
}

// CancelEnvInit POST /api/testing/envs/{env_id}/init/cancel
func (h *TestingHandler) CancelEnvInit(w http.ResponseWriter, r *http.Request) {
	envID := r.PathValue("env_id")
	projectID := r.PathValue("id")
	if _, ok := h.resolveEnvForProject(w, r, projectID, envID); !ok {
		return
	}
	jobID := h.initJobs.get(envID)
	if jobID == "" {
		writeError(w, http.StatusNotFound, "NO_ACTIVE_INIT", "当前没有正在进行的初始化")
		return
	}
	job, ok := h.jobs.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "JOB_NOT_FOUND", "任务不存在或已结束")
		return
	}
	job.Cancel()
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceling"})
}

// runInitChain executes the five setup steps in order, streaming each one's
// output into the job log. Any non-zero exit aborts the chain and marks the
// environment failed with the failing step named — a half-initialized
// workspace is worse than an obviously broken one.
func (h *TestingHandler) runInitChain(job *store.Job, env *model.TestEnvironment) {
	defer h.initJobs.clear(env.ID)

	ctx, cancel := context.WithCancel(context.Background())
	job.SetCmd(exec.CommandContext(ctx, "true"), cancel)
	defer cancel()

	fail := func(step string, err error) {
		msg := fmt.Sprintf("%s 失败: %v", step, err)
		job.Append(store.LogLine{Type: "error", Content: msg})
		_ = h.envSvc.MarkFailed(env.ID, msg)
		job.Append(store.LogLine{Type: "done", Content: "初始化失败"})
		job.Finish(1, store.JobError)
	}

	workspace, err := h.envSvc.EnsureWorkspace(env)
	if err != nil {
		fail("准备工作区", err)
		return
	}
	log.Printf("[testing] init job %s env %s workspace %s", job.ID, env.ID, workspace)

	step := func(n int, total int, label string) {
		job.Append(store.LogLine{Type: "phase", Content: fmt.Sprintf("[%d/%d] %s", n, total, label)})
	}

	const totalSteps = 5

	// 1 — package.json. `npm init -y` is only needed on a fresh workspace;
	// re-running init must NOT clobber an existing package.json.
	step(1, totalSteps, "初始化 npm 包")
	pkg := filepath.Join(workspace, "package.json")
	if _, statErr := os.Stat(pkg); os.IsNotExist(statErr) {
		if err := runStreamed(job, ctx, workspace, "npm",
			[]string{"init", "-y", "--silent"}, initNpmInitTimeout); err != nil {
			fail("npm init", err)
			return
		}
	} else {
		job.Append(store.LogLine{Type: "message", Content: "package.json 已存在，跳过"})
	}

	// 2 — the Playwright test runner.
	step(2, totalSteps, "安装 @playwright/test")
	if err := runStreamed(job, ctx, workspace, "npm",
		[]string{"install", "-D", playwrightDepVersion, "--no-fund", "--no-audit"},
		initNpmInstallTimeout); err != nil {
		fail("npm install @playwright/test", err)
		return
	}

	// 3 — browser binaries. The most failure-prone step on Linux, where the
	// browser needs shared libraries only root can install.
	step(3, totalSteps, "下载浏览器 ("+env.Browsers+")")
	if err := runStreamed(job, ctx, workspace, "npx",
		[]string{"playwright", "install", env.Browsers}, initBrowserTimeout); err != nil {
		job.Append(store.LogLine{Type: "error", Content: missingDepsHint})
		fail("playwright install", err)
		return
	}

	// 4 — the official planner/generator/healer subagents plus the
	// playwright-test MCP server. The --loop flag is only accepted by newer
	// releases, so fall back to the bare command rather than failing init
	// over a cosmetic argument.
	step(4, totalSteps, "生成 Playwright Test Agents")
	if err := runStreamed(job, ctx, workspace, "npx",
		[]string{"playwright", "init-agents", "--loop=claude"}, initAgentsTimeout); err != nil {
		job.Append(store.LogLine{Type: "message", Content: "重试 init-agents（不带 --loop 参数）"})
		if err2 := runStreamed(job, ctx, workspace, "npx",
			[]string{"playwright", "init-agents"}, initAgentsTimeout); err2 != nil {
			fail("playwright init-agents", err2)
			return
		}
	}
	if missing := missingAgents(workspace); len(missing) > 0 {
		fail("playwright init-agents", fmt.Errorf("未生成以下 agent 文件: %s", strings.Join(missing, ", ")))
		return
	}

	// 5 — files the backend owns. Generated directly rather than shelled out
	// so their content is version-controlled with this code.
	step(5, totalSteps, "写入 playwright.config.ts / seed test / MCP 设置")
	if err := writeWorkspaceFiles(workspace, env); err != nil {
		fail("写入工作区文件", err)
		return
	}

	// Capture the installed version for diagnostics: agent definitions are
	// version-coupled, so "the healer misbehaves" is usually explained by a
	// version skew between what was recorded and what is installed.
	version, err := capturePlaywrightVersion(ctx, workspace)
	if err != nil {
		// Non-fatal — the workspace is fully usable without it.
		job.Append(store.LogLine{Type: "message", Content: "未能读取 playwright 版本: " + err.Error()})
		version = "unknown"
	}

	if err := h.envSvc.MarkReady(env.ID, version); err != nil {
		fail("保存初始化结果", err)
		return
	}
	job.Append(store.LogLine{Type: "done", Content: "✅ 初始化完成，Playwright " + version})
	job.Finish(0, store.JobDone)
	log.Printf("[testing] init job %s env %s done (playwright %s)", job.ID, env.ID, version)
}

// missingDepsHint points at the settings page when playwright could not
// install browsers for lack of system libraries — by far the most common
// init failure on Linux, and the raw error text is otherwise opaque.
const missingDepsHint = "⚠️ 浏览器可能缺少系统依赖库。Linux 上请运行 `sudo npx playwright install-deps`，" +
	"或前往「设置 → 环境依赖」查看 Playwright 状态。"

// missingAgents reports which of the three agent definitions init-agents
// failed to produce.
func missingAgents(workspace string) []string {
	var missing []string
	for _, name := range playwrightAgentFiles {
		p := filepath.Join(workspace, ".claude", "agents", name+".md")
		if _, err := os.Stat(p); err != nil {
			missing = append(missing, name+".md")
		}
	}
	return missing
}

// playwrightConfigTS is written verbatim on init. Two choices matter:
//
//   - baseURL comes from NOVA_BASE_URL, injected per run. A literal URL
//     baked in here would make editing an environment's base_url require a
//     full re-init.
//   - the JSON reporter writes to PLAYWRIGHT_JSON_OUTPUT_NAME for the same
//     reason: each run needs its own report file under runs/<run_id>/.
const playwrightConfigTS = `import { defineConfig } from '@playwright/test';

// Generated by NovaWorkbench 自动化测试 during environment init.
// baseURL and the JSON report path are injected per run via environment
// variables, so editing an environment never requires re-initializing.
export default defineConfig({
  testDir: './tests',
  fullyParallel: true,
  reporter: [
    ['list'],
    ['json', { outputFile: process.env.PLAYWRIGHT_JSON_OUTPUT_NAME ?? 'report.json' }],
  ],
  use: {
    baseURL: process.env.NOVA_BASE_URL ?? 'http://localhost:3000',
    screenshot: 'on',
    trace: 'retain-on-failure',
  },
});
`

// defaultSeedTest is used when the environment has no seed test configured.
// It is deliberately minimal but real: the planner agent runs it to explore
// the app, so an empty file would leave the agent with no way to learn the
// app's structure.
const defaultSeedTest = `import { test, expect } from '@playwright/test';

// Seed test generated by NovaWorkbench as the exploration entry point for
// the Playwright planner agent. Replace it with a real login flow to give
// the generated specs an authenticated session.
test('seed', async ({ page }) => {
  await page.goto('/');
  await expect(page.locator('body')).toBeVisible();
});
`

// writeWorkspaceFiles writes the three backend-owned files. All are
// idempotent so re-init safely overwrites.
func writeWorkspaceFiles(workspace string, env *model.TestEnvironment) error {
	if err := os.WriteFile(filepath.Join(workspace, "playwright.config.ts"), []byte(playwrightConfigTS), 0o644); err != nil {
		return fmt.Errorf("write playwright.config.ts: %w", err)
	}

	seed := env.SeedTest
	if strings.TrimSpace(seed) == "" {
		seed = defaultSeedTest
	}
	if err := os.MkdirAll(filepath.Join(workspace, "tests"), 0o755); err != nil {
		return fmt.Errorf("mkdir tests: %w", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "tests", "seed.spec.ts"), []byte(seed), 0o644); err != nil {
		return fmt.Errorf("write tests/seed.spec.ts: %w", err)
	}

	return enableProjectMCPServers(workspace)
}

// enableProjectMCPServers merges "enableAllProjectMcpServers": true into the
// workspace's .claude/settings.json.
//
// This is the single most load-bearing line in the whole module. Under the
// non-interactive `-p` mode the planner/generator agents run in, a
// project-scoped .mcp.json (where `init-agents` puts playwright-test) is NOT
// trusted by default — the server would have to be approved interactively,
// which never happens in a headless subprocess. Without this flag the agents
// simply have no browser tools and produce an empty plan after burning their
// full timeout.
//
// MERGED, not overwritten: init-agents may already have written a settings
// file, and clobbering it would drop its configuration.
func enableProjectMCPServers(workspace string) error {
	dir := filepath.Join(workspace, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir .claude: %w", err)
	}
	path := filepath.Join(dir, "settings.json")

	settings := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
		// A malformed file must not abort init — start from a clean object
		// rather than leaving the agent blocked with no MCP.
		if err := json.Unmarshal(raw, &settings); err != nil {
			log.Printf("[testing] %s is not valid JSON, rewriting: %v", path, err)
			settings = map[string]any{}
		}
	}
	settings["enableAllProjectMcpServers"] = true

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings.json: %w", err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("write settings.json: %w", err)
	}
	return nil
}

// capturePlaywrightVersion reads the installed version string.
func capturePlaywrightVersion(ctx context.Context, workspace string) (string, error) {
	vctx, cancel := context.WithTimeout(ctx, initVersionTimeout)
	defer cancel()

	out, err := exec.CommandContext(vctx, "npx", "playwright", "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// runStreamed runs one subprocess, merging stdout+stderr into the job log
// line by line, and returns a non-zero exit as an error.
//
// The merge matters: npm and playwright interleave progress on both streams,
// and splitting them would reorder the "added 1 package" / "downloading"
// lines into an unreadable mess. The 4 MB scanner ceiling matches the
// docker-compose runner — a single `npm install` line can exceed 256 KB.
func runStreamed(job *store.Job, ctx context.Context, dir, bin string, args []string, timeout time.Duration) error {
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(stepCtx, bin, args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()

	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("pipe: %w", err)
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		// exec.ErrNotFound is by far the most likely failure here (npm not on
		// PATH) and deserves a clearer message than "executable file not found".
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("找不到 %s，请确认已安装 Node.js 与 npm（设置 → 环境依赖）", bin)
		}
		return err
	}
	// Close our copy of the write end so the scanner sees EOF when the child exits.
	pw.Close()

	job.SetCmd(cmd, cancel)

	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lineType := "message"
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "npm err") {
			lineType = "error"
		}
		job.Append(store.LogLine{Type: lineType, Content: line})
	}
	pr.Close()

	waitErr := cmd.Wait()
	if waitErr == nil {
		return nil
	}
	// A timeout is worth distinguishing — "playwright install took longer than
	// 20 minutes" is a different problem from "the install crashed".
	if errors.Is(stepCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("命令超时（超过 %s）: %s %s", timeout, bin, strings.Join(args, " "))
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return fmt.Errorf("命令退出码 %d: %s %s", exitErr.ExitCode(), bin, strings.Join(args, " "))
	}
	return waitErr
}
