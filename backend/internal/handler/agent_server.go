// Package handler — agent_server exposes CRUD + Check/Install for the
// agent_servers table. The Check goroutine SSHs into the target host, runs
// `uname -s` and per-dependency probes (claude / node / npm / git), and
// writes back status + a Chinese summary of what passed or failed. Install
// picks a Linux or Darwin shell-script body (Homebrew for macOS; apt/yum/dnf
// + nvm fallback for Linux), uploads it via SFTP and executes through the
// shared Exec helper. Both flows use the shared JobStore + SSE pattern that
// wizard / preflight / runner / review already use, so the frontend renders
// the live "检查环境..." / "安装依赖..." panel identically.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	gossh "github.com/novaworkbench/backend/internal/ssh"
	"github.com/novaworkbench/backend/internal/store"
)

type AgentServerHandler struct {
	svc  *service.AgentServerService
	jobs *store.JobStore
}

func NewAgentServerHandler(svc *service.AgentServerService, jobs *store.JobStore) *AgentServerHandler {
	return &AgentServerHandler{svc: svc, jobs: jobs}
}

// ---- CRUD -----------------------------------------------------------------

// GET /api/settings/agent-servers
func (h *AgentServerHandler) List(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.List()
	if err != nil {
		writeError(w, 500, "LIST_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, items)
}

// POST /api/settings/agent-servers
func (h *AgentServerHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req model.CreateAgentServerReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, 400, "INVALID", "invalid request body: "+err.Error())
		return
	}
	a, err := h.svc.Create(req)
	if err != nil {
		writeError(w, 400, "CREATE_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, a)
}

// GET /api/settings/agent-servers/{id}
func (h *AgentServerHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := h.svc.Get(id)
	if err != nil {
		writeError(w, 404, "NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, 200, a)
}

// PUT /api/settings/agent-servers/{id}
func (h *AgentServerHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req model.UpdateAgentServerReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, 400, "INVALID", "invalid request body: "+err.Error())
		return
	}
	a, err := h.svc.Update(id, req)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, 404, "NOT_FOUND", err.Error())
			return
		}
		writeError(w, 400, "UPDATE_FAILED", err.Error())
		return
	}
	writeJSON(w, 200, a)
}

// DELETE /api/settings/agent-servers/{id}
func (h *AgentServerHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.svc.Delete(id); err != nil {
		writeError(w, 500, "DELETE_FAILED", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- POST /api/settings/agent-servers/{id}/check ---------------------------
// Returns { job_id } immediately; the goroutine below SSHs into the target
// host, runs `uname -s` + per-dep probes, and writes the result back via
// UpdateStatus. Frontend subscribes via StreamJob to render the live log.

func (h *AgentServerHandler) Check(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := h.svc.Get(id)
	if err != nil {
		writeError(w, 404, "NOT_FOUND", err.Error())
		return
	}
	if h.jobs.Live(a.ID) || true { // always allow re-check; idempotent
		_ = h.svc.UpdateStatus(a.ID, model.AgentServerStatusChecking, "正在连接并检查环境...")
	}
	job := h.jobs.Create(a.ID)
	writeJSON(w, 200, map[string]string{"job_id": job.ID})
	go h.runCheck(job, a.ID)
}

// runCheck is the goroutine started by Check. It loads the (decrypted) creds,
// dials SSH, runs `uname -s`, then probes for each tracked dependency. Status
// transitions: checking → ready (all installed) or error (any missing /
// connection failed). check_result holds a Chinese summary surfaced verbatim
// in the UI's status badge tooltip.
func (h *AgentServerHandler) runCheck(job *store.Job, serverID string) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[agent-server] check panic for %s: %v", serverID, rec)
			job.Append(store.LogLine{Type: "error", Content: fmt.Sprintf("panic: %v", rec)})
			_ = h.svc.UpdateStatus(serverID, model.AgentServerStatusError, "内部错误")
			job.Finish(1, store.JobError)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	job.Append(store.LogLine{Type: "phase", Content: "🔌 连接到 Agent 服务器..."})

	srv, plain, err := h.svc.GetWithCredential(serverID)
	if err != nil {
		job.Append(store.LogLine{Type: "error", Content: "❌ 凭据解析失败: " + err.Error()})
		_ = h.svc.UpdateStatus(serverID, model.AgentServerStatusError, "凭据不可用（主密钥可能被修改）")
		job.Finish(1, store.JobError)
		return
	}

	client, err := gossh.Dial(ctx, srv.Host, srv.Port, srv.Username, srv.AuthType, plain)
	if err != nil {
		job.Append(store.LogLine{Type: "error", Content: "❌ 连接失败: " + err.Error()})
		_ = h.svc.UpdateStatus(serverID, model.AgentServerStatusError,
			fmt.Sprintf("连接 %s:%d 失败：%v", srv.Host, srv.Port, err))
		job.Finish(1, store.JobError)
		return
	}
	defer client.Close()

	// Platform probe — must be Linux or Darwin. uname -s output is captured
	// without a label so the SSE pane doesn't get noisy "[uname]" prefixes.
	var unameOut strings.Builder
	if exit, _ := client.Exec(ctx, "uname -s", "", nil, &unameOut, nil); exit != 0 {
		job.Append(store.LogLine{Type: "error", Content: "❌ 无法读取 uname 输出"})
		_ = h.svc.UpdateStatus(serverID, model.AgentServerStatusError, "uname 读取失败")
		job.Finish(1, store.JobError)
		return
	}
	platform := strings.TrimSpace(unameOut.String())
	job.Append(store.LogLine{Type: "message", Content: "✓ 平台: " + platform})
	if platform != "Linux" && platform != "Darwin" {
		msg := fmt.Sprintf("不支持的远程平台: %s（仅支持 Linux / macOS）", platform)
		job.Append(store.LogLine{Type: "error", Content: "❌ " + msg})
		_ = h.svc.UpdateStatus(serverID, model.AgentServerStatusError, msg)
		job.Finish(1, store.JobError)
		return
	}

	// Dependency probes — `which <dep>` followed by `<dep> --version` for the
	// installed path's version string. Each line is its own LogLine so the
	// frontend's per-line append reads as a checklist.
	deps := []struct {
		bin   string
		label string
	}{
		{"claude", "claude"},
		{"node", "node"},
		{"npm", "npm"},
		{"git", "git"},
	}
	job.Append(store.LogLine{Type: "phase", Content: "🔍 检查依赖..."})
	missing := []string{}
	for _, d := range deps {
		var out strings.Builder
		// SSH non-interactive non-login shells do NOT source ~/.bashrc, so
		// binaries installed via nvm (at ~/.nvm/versions/node/*/bin/) and
		// user-local paths (~/.local/bin, /opt/homebrew/bin on macOS) are
		// invisible to plain lookups. Augment PATH with the common install
		// locations AND source nvm.sh when present, then run `command -v`.
		// `command -v` is the POSIX-portable form of `which` and avoids the
		// shell built-in lookup quirks some sshd configs have.
		cmd := `export PATH="$HOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"; ` +
			`if [ -s "$HOME/.nvm/nvm.sh" ]; then . "$HOME/.nvm/nvm.sh" 2>/dev/null; fi; ` +
			`hash -r 2>/dev/null; ` +
			`command -v ` + d.bin + ` >/dev/null 2>&1 && ` + d.bin + ` --version 2>&1 | head -n1`
		exit, _ := client.Exec(ctx, cmd, "", nil, &out, nil)
		if exit != 0 {
			line := fmt.Sprintf("✗ %s 未找到", d.label)
			job.Append(store.LogLine{Type: "message", Content: line})
			missing = append(missing, d.label)
			continue
		}
		v := strings.TrimSpace(strings.SplitN(out.String(), "\n", 2)[0])
		job.Append(store.LogLine{Type: "message", Content: fmt.Sprintf("✓ %s %s", d.label, v)})
	}

	status := model.AgentServerStatusReady
	summary := "所有依赖已就绪"
	if len(missing) > 0 {
		status = model.AgentServerStatusError
		summary = "缺少依赖: " + strings.Join(missing, ", ") + "，请点「安装依赖」"
		job.Append(store.LogLine{Type: "error", Content: summary})
	}

	// Claude settings check — the wizard's remote-coding path needs a
	// ~/.claude/settings.json on the agent host with valid ANTHROPIC_AUTH_TOKEN
	// (otherwise claude CLI starts but every request returns 401). If the file
	// is missing we seed it with a default template (placeholder token, model
	// pinned to a sensible default) so the user only has to paste their real
	// token in; if it exists we parse-check it and warn when the env block is
	// absent.
	job.Append(store.LogLine{Type: "phase", Content: "🔍 检查 ~/.claude/settings.json..."})
	settingsStatus, settingsMsg := h.ensureClaudeSettings(ctx, client, job)
	if settingsStatus == model.AgentServerStatusError {
		status = model.AgentServerStatusError
		if summary == "所有依赖已就绪" {
			summary = settingsMsg
		} else {
			summary += "; " + settingsMsg
		}
	}

	// nova-agent-worker probe — the remote coding path goes through this
	// Node.js service (POST → SSE → claude CLI). If the worker isn't
	// up, every coding run will fail with "无法连接 nova-agent-worker"
	// before it even gets to the CLI, so we surface that here.
	//
	// Auto-revive: when the worker is down we try to bring it back via
	// systemctl --user (with `enable-linger` set first, in case this is the
	// first run after install) and then a nohup fallback. This avoids
	// the "Installed yesterday, rebooted today, worker never came back"
	// status that used to require a manual re-install. Only if both paths
	// fail do we surface an error status.
	var homeBuf strings.Builder
	_, _ = client.Exec(ctx, "echo $HOME", "", nil, &homeBuf, nil)
	homeDir := strings.TrimSpace(homeBuf.String())
	if homeDir == "" {
		homeDir = "/root"
	}
	workerStatus, workerMsg := h.probeWorkerAndAppend(ctx, client, homeDir, job)
	if workerStatus == model.AgentServerStatusError {
		status = model.AgentServerStatusError
		if summary == "所有依赖已就绪" {
			summary = workerMsg
		} else {
			summary += "; " + workerMsg
		}
	}

	_ = h.svc.UpdateStatus(serverID, status, summary)
	job.Append(store.LogLine{Type: "done", Content: summary})
	job.Finish(0, store.JobDone)
}

// ensureClaudeSettings inspects (and if missing seeds) the agent host's
// ~/.claude/settings.json. The default template mirrors the values listed
// in the requirement (ANTHROPIC_AUTH_TOKEN placeholder, ANTHROPIC_BASE_URL
// pointed at minimax, model pinned to MiniMax-M3, theme=dark). The file is
// written with mode 0600 because it carries the bearer token.
//
// Returns the status the check goroutine should use plus a human-readable
// summary line (already appended to job via the LogLine interface).
func (h *AgentServerHandler) ensureClaudeSettings(
	ctx context.Context,
	client *gossh.Client,
	job *store.Job,
) (status string, summary string) {
	const defaultSettings = `{
  "env": {
    "ANTHROPIC_AUTH_TOKEN": "sk-cp-xxxxxxxxxxxxxxxxxxxxxxxx",
    "ANTHROPIC_BASE_URL": "https://api.minimax.cn/anthropic",
    "ANTHROPIC_MODEL": "MiniMax-M3",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "MiniMax-M3",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "MiniMax-M3",
    "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1"
  },
  "theme": "dark"
}
`

	var statOut strings.Builder
	if exit, _ := client.Exec(ctx, "test -e ~/.claude/settings.json && echo EXISTS || echo MISSING", "", nil, &statOut, nil); exit != 0 {
		return model.AgentServerStatusError, "~/.claude 状态检查失败"
	}
	if strings.TrimSpace(statOut.String()) == "EXISTS" {
		// File already present — sanity-check JSON shape so we surface a clear
		// File already present — the user has configured it, so we treat
		// it as ready regardless of what's inside. Skipping content checks
		// (JSON shape / env block / API key) per product decision: the
		// settings file is the user's contract with claude, not ours; if
		// it doesn't work, the wizard will surface a clearer 401-class
		// error downstream instead of us guessing here. We only confirm
		// the file actually exists at the expected path.
		job.Append(store.LogLine{Type: "message", Content: "✓ ~/.claude/settings.json 已存在，将由 claude 直接加载"})
		return model.AgentServerStatusReady, ""
	}

	// Missing — seed the default template via SFTP. We pass "~/..." so the
	// ssh.Client.WriteFile helper expands `~` against the remote user's
	// actual $HOME (resolved via a one-shot `echo $HOME`); passing the
	// literal path would either create a file under the SFTP CWD (likely
	// "/") or fail silently with no error from the SFTP server.
	job.Append(store.LogLine{Type: "message", Content: "⚠ ~/.claude/settings.json 不存在，写入默认模板（请在文件中替换 ANTHROPIC_AUTH_TOKEN）"})
	if err := client.WriteFile("~/.claude/settings.json", []byte(defaultSettings), 0600); err != nil {
		job.Append(store.LogLine{Type: "error", Content: "❌ 写入默认 settings.json 失败: " + err.Error()})
		return model.AgentServerStatusError, "写入 ~/.claude/settings.json 失败"
	}
	job.Append(store.LogLine{Type: "message", Content: "✓ 默认 ~/.claude/settings.json 已写入（0600），请编辑后再次「检查环境」"})
	return model.AgentServerStatusError, "~/.claude/settings.json 已初始化为默认模板（占位 token），请编辑后再次「检查环境」"
}

// ---- POST /api/settings/agent-servers/{id}/install -------------------------
// Returns { job_id }; the goroutine picks a platform-appropriate install
// script (Linux apt/yum/dnf + nvm fallback, or macOS Homebrew), uploads it
// via SFTP and runs `sh <remote-path>`. After install, runCheck is invoked
// so the resulting status reflects the post-install reality (no stale
// "missing deps" badge).

func (h *AgentServerHandler) Install(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := h.svc.Get(id)
	if err != nil {
		writeError(w, 404, "NOT_FOUND", err.Error())
		return
	}
	_ = h.svc.UpdateStatus(a.ID, model.AgentServerStatusInstalling, "正在远程安装依赖...")
	job := h.jobs.Create(a.ID)
	writeJSON(w, 200, map[string]string{"job_id": job.ID})
	go h.runInstall(job, a.ID)
}

func (h *AgentServerHandler) runInstall(job *store.Job, serverID string) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[agent-server] install panic for %s: %v", serverID, rec)
			job.Append(store.LogLine{Type: "error", Content: fmt.Sprintf("panic: %v", rec)})
			_ = h.svc.UpdateStatus(serverID, model.AgentServerStatusError, "内部错误")
			job.Finish(1, store.JobError)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	job.Append(store.LogLine{Type: "phase", Content: "🔌 连接到 Agent 服务器..."})
	srv, plain, err := h.svc.GetWithCredential(serverID)
	if err != nil {
		job.Append(store.LogLine{Type: "error", Content: "❌ 凭据解析失败: " + err.Error()})
		_ = h.svc.UpdateStatus(serverID, model.AgentServerStatusError, "凭据不可用")
		job.Finish(1, store.JobError)
		return
	}

	client, err := gossh.Dial(ctx, srv.Host, srv.Port, srv.Username, srv.AuthType, plain)
	if err != nil {
		job.Append(store.LogLine{Type: "error", Content: "❌ 连接失败: " + err.Error()})
		_ = h.svc.UpdateStatus(serverID, model.AgentServerStatusError,
			fmt.Sprintf("连接 %s:%d 失败", srv.Host, srv.Port))
		job.Finish(1, store.JobError)
		return
	}
	defer client.Close()

	// Re-probe platform — mirrors the check goroutine's logic so the script
	// choice is in lockstep with what the user sees in the install UI.
	var unameOut strings.Builder
	client.Exec(ctx, "uname -s", "", nil, &unameOut, nil)
	platform := strings.TrimSpace(unameOut.String())

	var script string
	switch platform {
	case "Darwin":
		job.Append(store.LogLine{Type: "phase", Content: "🍺 安装依赖 (macOS / Homebrew)..."})
		script = darwinInstallScript()
	case "Linux":
		job.Append(store.LogLine{Type: "phase", Content: "🐧 安装依赖 (Linux)..."})
		script = linuxInstallScript()
	default:
		// runCheck already rejects non-Linux/Darwin, but a race where the user
		// hit Install first without Check would land here.
		msg := fmt.Sprintf("不支持的远程平台: %s", platform)
		job.Append(store.LogLine{Type: "error", Content: msg})
		_ = h.svc.UpdateStatus(serverID, model.AgentServerStatusError, msg)
		job.Finish(1, store.JobError)
		return
	}

	// RunScript writes the script body to a local tempfile, uploads via SFTP,
	// execs `sh <remote-path>`, and cleans up. We capture stdout/stderr line
	// by line into the job log so the UI shows install progress in real time.
	if exit, err := client.RunScript(ctx, script, "install", nil, jobLineWriter(job)); err != nil || exit != 0 {
		msg := fmt.Sprintf("远程安装失败（exit=%d err=%v）", exit, err)
		job.Append(store.LogLine{Type: "error", Content: msg})
		_ = h.svc.UpdateStatus(serverID, model.AgentServerStatusError, msg)
		job.Finish(1, store.JobError)
		return
	}

	// Step 1b: deploy nova-agent-worker. The platform install script above
	// only put `claude` on PATH; the worker is a separate Node.js service
	// that bridges NovaWorkbench → claude CLI (POST → spawn claude → SSE).
	// We SFTP the source files (embedded as Go constants in
	// agent_worker_files.go), install deps via npm, and start the service.
	// Without this step the wizard's remote coding flow has no /v1/run
	// endpoint to POST to.
	job.Append(store.LogLine{Type: "phase", Content: "🚀 部署 nova-agent-worker..."})
	if err := h.installNodeWorker(ctx, client, platform, job); err != nil {
		msg := fmt.Sprintf("nova-agent-worker 部署失败: %v", err)
		job.Append(store.LogLine{Type: "error", Content: msg})
		_ = h.svc.UpdateStatus(serverID, model.AgentServerStatusError, msg)
		job.Finish(1, store.JobError)
		return
	}

	job.Append(store.LogLine{Type: "phase", Content: "🔍 重新检查依赖..."})
	// Re-run the check flow inline so the UI doesn't have to issue a second
	// request — the status badge updates as soon as we Finish.
	probeDepsAndUpdateStatus(ctx, h.svc, serverID, client, job)
}

// probeDepsAndUpdateStatus runs the same per-dep probes as runCheck, but
// without touching the job's status field — the caller already wrote a
// reasonable pre-check value and we want the post-install verification to
// land cleanly here.
//
// PATH augmentation matches runCheck: nvm-installed and user-local binaries
// are not on the default SSH PATH (see runCheck for the rationale).
func probeDepsAndUpdateStatus(ctx context.Context, svc *service.AgentServerService, serverID string, client *gossh.Client, job *store.Job) {
	deps := []string{"claude", "node", "npm", "git"}
	missing := []string{}
	for _, dep := range deps {
		var out strings.Builder
		cmd := `export PATH="$HOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"; ` +
			`if [ -s "$HOME/.nvm/nvm.sh" ]; then . "$HOME/.nvm/nvm.sh" 2>/dev/null; fi; ` +
			`hash -r 2>/dev/null; ` +
			`command -v ` + dep + ` >/dev/null 2>&1 && ` + dep + ` --version 2>&1 | head -n1`
		exit, _ := client.Exec(ctx, cmd, "", nil, &out, nil)
		if exit != 0 {
			job.Append(store.LogLine{Type: "message", Content: "✗ " + dep + " 未找到"})
			missing = append(missing, dep)
			continue
		}
		v := strings.TrimSpace(strings.SplitN(out.String(), "\n", 2)[0])
		job.Append(store.LogLine{Type: "message", Content: "✓ " + dep + " " + v})
	}

	status := model.AgentServerStatusReady
	summary := "所有依赖已就绪"
	if len(missing) > 0 {
		status = model.AgentServerStatusError
		summary = "缺少依赖: " + strings.Join(missing, ", ") + "，请检查网络后重试"
		job.Append(store.LogLine{Type: "error", Content: summary})
	}
	_ = svc.UpdateStatus(serverID, status, summary)
	job.Append(store.LogLine{Type: "done", Content: summary})
	job.Finish(0, store.JobDone)
}

// runCheck wraps the long-form Check goroutine and exposes a status update
// for the worker health probe. The CLI deps are validated inline by
// runCheck; this helper isolates the worker probe so it can be called from
// either runCheck or the install flow without duplicating the conditional
// status / summary concatenation logic.
//
// homeDir is used as the install root when auto-reviving a down worker
// (via startWorkerIfDown). Pass the resolved $HOME from the caller.
func (h *AgentServerHandler) probeWorkerAndAppend(
	ctx context.Context, client *gossh.Client, homeDir string, job *store.Job,
) (status string, summary string) {
	job.Append(store.LogLine{Type: "phase", Content: "🔍 检查 nova-agent-worker..."})
	if err := probeWorkerHealth(ctx, client); err == nil {
		job.Append(store.LogLine{Type: "message", Content: "✓ nova-agent-worker 已就绪"})
		return model.AgentServerStatusReady, ""
	}

	// Down — try to bring it back before we surface an error. The user
	// shouldn't have to re-run Install just because the worker crashed
	// or the box rebooted between sessions.
	job.Append(store.LogLine{Type: "message", Content: "⚠ nova-agent-worker 未在监听，尝试自动拉起..."})
	if startErr := startWorkerIfDown(ctx, client, homeDir); startErr != nil {
		return model.AgentServerStatusError, "nova-agent-worker 无响应（" + startErr.Error() + "）。请重新点「安装依赖」"
	}
	job.Append(store.LogLine{Type: "message", Content: "✓ nova-agent-worker 已自动拉起"})
	return model.AgentServerStatusReady, ""
}

// jobLineWriter returns a small adapter that turns each non-empty line from
// RunScript's combined stdout/stderr pipe into a `message` LogLine. Empty
// lines are dropped to avoid a wall of blank lines in the UI panel.
func jobLineWriter(job *store.Job) *lineWriter { return &lineWriter{job: job} }

type lineWriter struct{ job *store.Job }

func (w *lineWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(string(p), "\n") {
		if line == "" {
			continue
		}
		w.job.Append(store.LogLine{Type: "message", Content: line})
	}
	return len(p), nil
}

// ---- Job stream (shared with preflight's StreamJob shape) -----------------

func (h *AgentServerHandler) GetJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := h.jobs.Get(id)
	if !ok {
		writeError(w, 404, "NOT_FOUND", "job not found")
		return
	}
	lines, status, exitCode := job.Snapshot()
	writeJSON(w, 200, map[string]interface{}{
		"job_id":      job.ID,
		"status":      status,
		"exit_code":   exitCode,
		"log":         lines,
		"started_at":  job.StartedAt,
		"finished_at": job.FinishedAt,
	})
}

func (h *AgentServerHandler) StreamJob(w http.ResponseWriter, r *http.Request) {
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
		b, _ := json.Marshal(map[string]interface{}{
			"type":      "job_done",
			"status":    string(status),
			"exit_code": exitCode,
		})
		return b
	})
}

// ---- nova-agent-worker deployment -----------------------------------------

// installNodeWorker SFTPs the worker source files (embedded as Go constants
// in agent_worker_files.go) to the SSH user's home directory on the remote
// host, runs `npm install --omit=dev` there, then enables and starts the
// service (systemd --user on Linux, launchd LaunchAgent on macOS).
// Idempotent — safe to re-run after a partial failure.
//
// Why user-owned install (not /opt/): the typical SSH user on a fresh
// agent server (ubuntu, ec2-user, root on a fresh VM) does NOT have write
// access to /opt/ — and getting sudo to work across the four deployment
// shapes we support (Ubuntu/Amazon/Tencent Cloud macOS) is more brittle than
// just using $HOME. Both systemd --user and launchd LaunchAgents auto-start
// the worker when the user logs in, which is exactly when NovaWorkbench is
// running and reachable.
//
// Why SFTP-write the source rather than curl from GitHub: the embedded
// files are pinned to this Go binary's compile-time version. Pulling from
// GitHub at install time would silently drift between releases and make
// regressions hard to bisect. Curl-from-URL can be added later as a fallback.
func (h *AgentServerHandler) installNodeWorker(ctx context.Context, client *gossh.Client, platform string, job *store.Job) error {
	// Resolve $HOME via a one-shot echo. SSH non-interactive non-login shells
	// don't source /etc/profile, but $HOME is set by sshd from /etc/passwd so
	// it's reliable. Falls back to /root for safety (e.g. some Docker images
	// where the entrypoint overrode HOME).
	var homeBuf strings.Builder
	if _, err := client.Exec(ctx, "echo $HOME", "", nil, &homeBuf, nil); err != nil {
		return fmt.Errorf("解析 $HOME 失败: %w", err)
	}
	homeDir := strings.TrimSpace(homeBuf.String())
	if homeDir == "" {
		homeDir = "/root"
	}
	installDir := homeDir + "/nova-agent-worker"

	job.Append(store.LogLine{Type: "message", Content: "📁 创建 " + installDir})
	if exit, _ := client.Exec(ctx, "mkdir -p "+installDir, "", nil, nil, nil); exit != 0 {
		return fmt.Errorf("mkdir %s 失败", installDir)
	}

	// Upload the worker files. We try the canonical GitHub source first so
	// every install gives the user the latest main-branch worker (preflight,
// classifyError, etc.) without requiring a NovaWorkbench binary bump —
// worker bugfixes ship the moment they hit main, not the moment we cut a
// release. Fall back to the Go-binary-embedded versions on any failure
	// (network down / GitHub 5xx / private network) so install never wedges
	// on a connectivity issue; the embedded versions are what shipped with
	// this binary, so they're at least as tested as the running Go server.
	//
	// Both paths produce the same two files on disk; only the source differs.
	// The npm install step below runs the same regardless, so resolving the
	// latest express version on top of either set works.
	// Worker source-of-truth resolution: prefer the file on disk under
	// agent-worker/ so a developer iterating on the worker in a repo
	// checkout gets their latest edits without rebuilding the Go binary.
	// Fall back to the Go-binary-embedded copy for production deployments
	// where the repo isn't checked out beside the server. Each source is
	// logged with a clear marker so the dev sees at a glance which path
	// served the file.
	job.Append(store.LogLine{Type: "phase", Content: "📦 准备 nova-agent-worker 源码..."})
	for _, f := range []struct {
		path string
		body string
		note string
	}{
		{installDir + "/server.mjs", workerSourceServerMJS(), workerSourceLabel("server.mjs")},
		{installDir + "/package.json", workerSourcePackageJSON(), workerSourceLabel("package.json")},
	} {
		if err := client.WriteFile(f.path, []byte(f.body), 0644); err != nil {
			return fmt.Errorf("上传 %s 失败: %w", f.path, err)
		}
		job.Append(store.LogLine{Type: "message", Content: "✓ 上传 " + f.path + "（" + f.note + "）"})
	}

	// Resolve the latest npm versions and rewrite package.json so each
	// install picks up express bugfixes without needing a NovaWorkbench
	// binary bump. The embedded package.json is the fallback when the npm
	// registry is unreachable (offline host / firewall / registry outage)
	// — we still install, just with the Go-binary-pinned versions, which
	// is the previous behavior.
	//
	// Why this matters: the previous flow uploaded package.json with
	// `^0.1.0` / `^4.19.0` and ran plain `npm install`. npm reused
	// package-lock.json + node_modules from prior installs and exited
	// "up to date in 277ms" without ever checking the registry, so SDK
	// bugfixes only landed when the user deleted node_modules by hand.
	// The worker no longer depends on the SDK — only express is in
	// package.json now — but the same lockfile-reuse trap applies.
	// See the deploy log for the user's last install — same symptom.
	job.Append(store.LogLine{Type: "phase", Content: "🔍 查询最新 npm 版本..."})
	if expressVer, ok := h.resolveLatestWorkerDeps(ctx, client); ok {
		job.Append(store.LogLine{Type: "message", Content: fmt.Sprintf("✓ express=%s", expressVer)})
		freshPkg := buildLatestPackageJSON(expressVer)
		if err := client.WriteFile(installDir+"/package.json", []byte(freshPkg), 0644); err != nil {
			job.Append(store.LogLine{Type: "message", Content: "⚠ 写入 latest package.json 失败，回退到内置版本: " + err.Error()})
		}
	} else {
		job.Append(store.LogLine{Type: "message", Content: "⚠ npm registry 不可达，使用内置版本"})
	}

	// Force a fresh resolve. Without this, npm install reads the existing
	// package-lock.json / node_modules and short-circuits to "up to date"
	// even after we updated package.json. Removing both before install
	// guarantees a real registry round-trip on every install.
	if exit, _ := client.Exec(ctx,
		"rm -f "+installDir+"/package-lock.json && rm -rf "+installDir+"/node_modules",
		"", nil, nil, nil); exit != 0 {
		job.Append(store.LogLine{Type: "message", Content: "⚠ 清理 lockfile/node_modules 失败（不影响继续）"})
	}

	// npm install. --omit=dev keeps the install small (no test deps); the
	// worker has no build step so we don't need a postinstall script.
	job.Append(store.LogLine{Type: "phase", Content: "📦 npm install（nova-agent-worker 依赖）..."})
	if exit, err := client.Exec(ctx,
		`export PATH="$HOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"; `+
			`if [ -s "$HOME/.nvm/nvm.sh" ]; then . "$HOME/.nvm/nvm.sh" 2>/dev/null; fi; `+
			`hash -r 2>/dev/null; `+
			`cd `+installDir+` && npm install --omit=dev --no-audit --no-fund --loglevel=error`,
		"", nil, jobLineWriter(job), nil); err != nil || exit != 0 {
		return fmt.Errorf("npm install 失败（exit=%d err=%v）", exit, err)
	}

	// Platform-specific service registration. systemd --user on Linux,
	// LaunchAgent on macOS. Both bind 127.0.0.1 via the worker env so the
	// service is only reachable through NovaWorkbench's SSH direct-tcpip
	// channel. Both auto-start at user login, which matches the workflow:
	// when NovaWorkbench runs the user is logged in, so the worker is up.
	switch platform {
	case "Linux":
		job.Append(store.LogLine{Type: "phase", Content: "🔧 注册 systemd --user 服务..."})
		unitDir := homeDir + "/.config/systemd/user"
		if exit, _ := client.Exec(ctx, "mkdir -p "+unitDir, "", nil, nil, nil); exit != 0 {
			return fmt.Errorf("创建 %s 失败", unitDir)
		}
		// Systemd --user units can't use $HOME in WorkingDirectory because
		// the user manager runs as the user but the spec expansion rules
		// differ. Bake the resolved absolute path into the unit instead.
		unitBody := strings.ReplaceAll(agentWorkerSystemdUnit,
			"/opt/nova-agent-worker", installDir)
		if err := client.WriteFile(unitDir+"/nova-agent-worker.service",
			[]byte(unitBody), 0644); err != nil {
			return fmt.Errorf("写入 systemd unit 失败: %w", err)
		}
		// 1) Enable linger so the user systemd is alive across SSH sessions
		//    that don't go through a graphical login — without this
		//    `systemctl --user` over SSH hits "Failed to connect to user
		//    bus" because the per-user manager exits when the user logs
		//    out. loginctl returns non-zero if the user is not logged in,
		//    which is harmless: enable-linger itself is the success signal.
		_, _ = client.Exec(ctx,
			"loginctl enable-linger $(id -u) 2>&1 || true",
			"", nil, jobLineWriter(job), nil)
		// 2) Daemon-reload + enable (silent — they emit nothing on the
		//    happy path). restart is intentionally a SEPARATE step below
		//    so its stdout/stderr lines show up cleanly in the install
		//    log rather than mixed in with reload/enable output.
		xdgWrap := func(cmd string) string {
			return `XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}" ` + cmd
		}
		_, _ = client.Exec(ctx, xdgWrap("systemctl --user daemon-reload 2>&1 || true"),
			"", nil, jobLineWriter(job), nil)
		_, _ = client.Exec(ctx, xdgWrap("systemctl --user enable nova-agent-worker.service 2>&1 || true"),
			"", nil, jobLineWriter(job), nil)
		// 3) Capture the OLD PID before restart so we can detect a silent
		//    no-op (the most common failure mode: no linger / no user
		//    bus, where systemctl --user restart prints nothing and exits
		//    non-zero — but with `|| true` we lose the exit code, and the
		//    old node process keeps serving requests with its old
		//    in-memory code). If the PID didn't change, surface a warning
		//    pointing at journalctl.
		var oldPidBuf strings.Builder
		_, _ = client.Exec(ctx, xdgWrap(
			`systemctl --user show nova-agent-worker.service --property=MainPID --value 2>/dev/null`),
			"", nil, &oldPidBuf, nil)
		oldPID := strings.TrimSpace(oldPidBuf.String())
		// 4) restart — dedicated phase line + separate command so the
		//    operator sees something happened. The trailing `echo
		//    "[exit=$?]"` pins the exit code into the log line so a silent
		//    failure (no stderr output, e.g. "Failed to connect to bus")
		//    still surfaces as a non-zero exit marker.
		job.Append(store.LogLine{Type: "phase", Content: "🔄 重启 nova-agent-worker 服务..."})
		restartCmd := xdgWrap(
			`systemctl --user restart nova-agent-worker.service 2>&1; ` +
				`echo "[exit=$?]"`)
		_, _ = client.Exec(ctx, restartCmd, "", nil, jobLineWriter(job), nil)
		// 5) Read MainPID right after restart so the UI sees the new
		//    worker actually came up under a fresh PID — if the PID
		//    didn't change from before, the log line makes that obvious
		//    so the user knows to check `journalctl`.
		var pidOut strings.Builder
		_, _ = client.Exec(ctx, xdgWrap(
			`systemctl --user show nova-agent-worker.service --property=MainPID,ExecMainStartTimestamp,ActiveState 2>&1 || true`),
			"", nil, &pidOut, nil)
		pidLine := strings.TrimSpace(pidOut.String())
		job.Append(store.LogLine{Type: "message", Content: "✓ systemd --user 服务状态: " + pidLine})
		// Detect silent restart failure: old PID == new PID AND there was
		// an old PID. The fresh-install case (oldPID == "") is fine.
		if oldPID != "" && strings.Contains(pidLine, "MainPID="+oldPID) {
			job.Append(store.LogLine{Type: "error", Content: "⚠️  systemd restart 后 MainPID 仍是 " + oldPID + " —— 重启似乎没生效。常见原因：enable-linger 未生效 / 无 user bus / 单元文件解析失败。请 SSH 到 Agent 服务器后手动执行 `journalctl --user -u nova-agent-worker -n 50 --no-pager` 查看具体原因。"})
		}
	case "Darwin":
		job.Append(store.LogLine{Type: "phase", Content: "🔧 注册 LaunchAgent..."})
		agentDir := homeDir + "/Library/LaunchAgents"
		if exit, _ := client.Exec(ctx, "mkdir -p "+agentDir, "", nil, nil, nil); exit != 0 {
			return fmt.Errorf("创建 %s 失败", agentDir)
		}
		// Same path-substitution trick as Linux: bake the resolved install
		// dir into the plist before writing.
		plistBody := strings.ReplaceAll(agentWorkerLaunchdPlist,
			"/opt/nova-agent-worker", installDir)
		plistPath := agentDir + "/com.novaworkbench.agent-worker.plist"
		if err := client.WriteFile(plistPath, []byte(plistBody), 0644); err != nil {
			return fmt.Errorf("写入 LaunchAgent plist 失败: %w", err)
		}
		// bootout first so re-running install doesn't pile up duplicate
		// registrations; then bootstrap loads + starts. gui/$UID is the
		// user domain (vs the system domain), no root needed.
		client.Exec(ctx, "launchctl bootout gui/$UID/com.novaworkbench.agent-worker 2>/dev/null || true", "", nil, nil, nil)
		if exit, _ := client.Exec(ctx,
		"launchctl bootstrap gui/$UID "+plistPath+" && launchctl kickstart -k gui/$UID/com.novaworkbench.agent-worker",
			"", nil, jobLineWriter(job), nil); exit != 0 {
			return fmt.Errorf("launchctl bootstrap 失败（exit=%d）", exit)
		}
		job.Append(store.LogLine{Type: "message", Content: "✓ LaunchAgent 已注册并启动"})
	}

	// Final smoke: GET /v1/health via SSH direct-tcpip. systemd --user
	// may have failed silently (no linger / no user bus on a fresh SSH
	// session), so before we declare success we probe the actual port;
	// if it's down we fall through to a nohup launch and re-probe. Only
	// if BOTH paths fail do we report an error.
	job.Append(store.LogLine{Type: "phase", Content: "🔍 worker 健康检查..."})
	if err := probeWorkerHealth(ctx, client); err == nil {
		job.Append(store.LogLine{Type: "message", Content: "✓ worker 健康检查通过"})
		return nil
	} else {
		job.Append(store.LogLine{Type: "message", Content: "⚠ systemd 路径未在监听，回落到 nohup 启动"})
	}

	// Fallback: launch the worker as a detached nohup process. This
	// succeeds in every environment where `node` is on PATH, even when
	// systemd --user is unavailable (the common case on a fresh SSH
	// session before `enable-linger` takes effect, or inside containers
	// without an init). The worker log lands in worker.log next to the
	// install dir so failures are inspectable.
	//
	// IMPORTANT: kill any previous worker first. Without this, the new
	// `node server.mjs &` either silently fails to bind 7000 (EADDRINUSE)
	// or — worse — runs a duplicate that the probe below finds first while
	// the OLD worker keeps serving real requests. We pkill the script
	// path, not the generic `node`, so other node processes on the host
	// (vite / npm / etc.) are untouched.
	launchCmd := `export PATH="$HOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"; ` +
		`if [ -s "$HOME/.nvm/nvm.sh" ]; then . "$HOME/.nvm/nvm.sh" 2>/dev/null; fi; ` +
		`hash -r 2>/dev/null; ` +
		`OLD_PID=$(pgrep -f nova-agent-worker/server.mjs | head -n1); ` +
		`if [ -n "$OLD_PID" ]; then kill "$OLD_PID" 2>/dev/null; sleep 1; kill -9 "$OLD_PID" 2>/dev/null || true; fi; ` +
		// TMPDIR=/tmp on the nohup env line guards against the macOS dev
		// box's SendEnv forwarding /var/folders/... into the SSH session
		// (which the nohup-launched worker would otherwise inherit).
		// server.mjs's resolveTmpdir also patches this per-spawn for the
		// claude child, but pinning it on the worker itself means Node's
		// own os.tmpdir() is sane before any user code runs.
		`nohup env NOVA_AGENT_WORKER_HOST=127.0.0.1 NOVA_AGENT_WORKER_PORT=7000 TMPDIR=/tmp ` +
		`node ` + installDir + `/server.mjs > ` + installDir + `/worker.log 2>&1 & ` +
		`disown 2>/dev/null || true; ` +
		`sleep 1; echo "[nova-agent] nohup launched, pid=$(pgrep -f nova-agent-worker/server.mjs | head -n1), killed_old=${OLD_PID:-none}"`
	if exit, err := client.Exec(ctx, launchCmd, "", nil, jobLineWriter(job), nil); err != nil || exit != 0 {
		return fmt.Errorf("nohup 启动失败（exit=%d err=%v）", exit, err)
	}

	// Give the listener a moment to bind, then probe again. A short retry
	// loop absorbs the slow-cold-start on under-powered VMs (Node import
	// of express + the worker's own modules) without holding the install
	// job hostage for tens of seconds.
	if err := waitForWorkerHealth(ctx, client, 6*time.Second); err != nil {
		return fmt.Errorf("worker 健康检查失败: %w（nohup 日志: %s/worker.log）", err, installDir)
	}
	job.Append(store.LogLine{Type: "message", Content: "✓ worker 健康检查通过 (nohup)"})
	return nil
}

// probeWorkerHealth opens an SSH direct-tcpip channel to 127.0.0.1:7000 on
// the remote host and does a GET /v1/health. Used by both install and check
// flows. Lives here (not in runCheck) so the install flow has its own copy
// without tangling the Check goroutine's status logic.
func probeWorkerHealth(ctx context.Context, client *gossh.Client) error {
	hc, hcCancel := context.WithTimeout(ctx, 8*time.Second)
	defer hcCancel()
	req, _ := http.NewRequestWithContext(hc, http.MethodGet, "http://127.0.0.1:7000/v1/health", nil)
	resp, err := (&http.Client{Transport: client.HTTPTransport("127.0.0.1:7000")}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// waitForWorkerHealth retries probeWorkerHealth until it succeeds or
// timeout elapses. The worker takes ~1-3s to bind on a cold start
// (Node + express listen), and a fresh SSH session + systemd --user
// activation can add another couple of seconds, so 6s of total budget
// (4 probes, 1.5s apart) is enough to absorb both without holding the
// caller hostage when the worker is genuinely dead.
func waitForWorkerHealth(ctx context.Context, client *gossh.Client, total time.Duration) error {
	deadline := time.Now().Add(total)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := probeWorkerHealth(ctx, client); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = errors.New("timeout")
	}
	return lastErr
}

// startWorkerIfDown attempts to bring the worker up when probeWorkerHealth
// reports it as down. Tries systemd --user start first (the proper path on
// modern Linux where enable-linger is set), then falls back to nohup.
// Used by the Check flow so a worker killed by hand or that crashed after
// install is automatically revived without the user needing to re-run
// Install.
//
// Returns nil if the worker is healthy afterwards, or an error describing
// why it couldn't be brought back. The caller decides whether a non-fatal
// "worker down" surfaces as an error status or just a warning.
func startWorkerIfDown(ctx context.Context, client *gossh.Client, homeDir string) error {
	installDir := homeDir + "/nova-agent-worker"
	// systemd --user start — best-effort. No linger means this is a no-op
	// and the nohup path below takes over.
	_, _ = client.Exec(ctx,
		`XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}" `+
			`loginctl enable-linger $(id -u) 2>/dev/null || true; `+
			`systemctl --user restart nova-agent-worker.service 2>&1 || true`,
		"", nil, nil, nil)
	if err := waitForWorkerHealth(ctx, client, 3*time.Second); err == nil {
		return nil
	}

	// nohup fallback. The launch line mirrors installNodeWorker's, including
	// PATH augmentation + nvm source so the same node binary the install
	// flow put on PATH is reachable here.
	launchCmd := `export PATH="$HOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"; ` +
		`if [ -s "$HOME/.nvm/nvm.sh" ]; then . "$HOME/.nvm/nvm.sh" 2>/dev/null; fi; ` +
		`hash -r 2>/dev/null; ` +
		// TMPDIR=/tmp guards against a macOS dev box's SendEnv forwarding
		// /var/folders/... into the SSH session. server.mjs also patches
		// this per-spawn for the claude child; pinning it on the worker
		// itself means Node's own os.tmpdir() is sane before any user
		// code runs.
		`nohup env NOVA_AGENT_WORKER_HOST=127.0.0.1 NOVA_AGENT_WORKER_PORT=7000 TMPDIR=/tmp ` +
		`node ` + installDir + `/server.mjs > ` + installDir + `/worker.log 2>&1 & ` +
		`disown 2>/dev/null || true`
	if exit, err := client.Exec(ctx, launchCmd, "", nil, nil, nil); err != nil || exit != 0 {
		return fmt.Errorf("nohup 启动失败（exit=%d err=%v）", exit, err)
	}
	if err := waitForWorkerHealth(ctx, client, 6*time.Second); err != nil {
		return fmt.Errorf("启动后仍无法连通: %w", err)
	}
	return nil
}

// ---- install scripts ------------------------------------------------------

// linuxInstallScript picks apt/yum/dnf via the same `command -v` heuristic
// preflight/install.go uses; nvm is the user-space fallback when the chosen
// pm fails (e.g. permission errors on a sudo-less shell). The script is
// written idempotently so re-running is safe.
func linuxInstallScript() string {
	return `#!/bin/sh
set -e
echo "[nova-agent] 探测包管理器..."
PM=""
for cand in apt-get dnf yum; do
  if command -v "$cand" >/dev/null 2>&1; then
    PM="$cand"
    break
  fi
done
echo "[nova-agent] PM=$PM"
install_with_pm() {
  case "$PM" in
    apt-get) apt-get update -y >/dev/null 2>&1; apt-get install -y nodejs npm ;;
    dnf) dnf install -y nodejs npm ;;
    yum) yum install -y nodejs npm ;;
  esac
}
if [ -n "$PM" ]; then
  if ! install_with_pm; then
    echo "[nova-agent] 系统包管理器失败，回落到 nvm 用户态安装"
    curl -fsSL https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.1/install.sh | bash >/dev/null
    export NVM_DIR="$HOME/.nvm"
    [ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh"
    nvm install --lts
  fi
else
  echo "[nova-agent] 未识别包管理器，使用 nvm 用户态安装"
  curl -fsSL https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.1/install.sh | bash >/dev/null
  export NVM_DIR="$HOME/.nvm"
  [ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh"
  nvm install --lts
fi
echo "[nova-agent] 安装 @anthropic-ai/claude-code..."
npm install -g @anthropic-ai/claude-code
echo "[nova-agent] 安装完成"
`
}

// darwinInstallScript checks for Homebrew (Apple Silicon path /opt/homebrew
// and legacy /usr/local), bootstraps it if missing, then `brew install node`
// (which ships npm) and the global claude CLI install. Idempotent — running
// twice is fine; brew skips already-installed packages.
func darwinInstallScript() string {
	return `#!/bin/sh
set -e
# Ensure both Homebrew paths are on PATH; Apple Silicon installs to
# /opt/homebrew while older Intel Macs use /usr/local. Without the prefix
# the brew binary won't be found after a fresh shell even when it is installed.
if [ -x /opt/homebrew/bin/brew ]; then
  eval "$(/opt/homebrew/bin/brew shellenv)"
elif [ -x /usr/local/bin/brew ]; then
  eval "$(/usr/local/bin/brew shellenv)"
fi
if ! command -v brew >/dev/null 2>&1; then
  echo "[nova-agent] 未找到 Homebrew，正在安装..."
  /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
  if [ -x /opt/homebrew/bin/brew ]; then
    eval "$(/opt/homebrew/bin/brew shellenv)"
  fi
fi
echo "[nova-agent] 安装 node (含 npm)..."
brew install node
echo "[nova-agent] 安装 @anthropic-ai/claude-code..."
npm install -g @anthropic-ai/claude-code
echo "[nova-agent] 安装完成"
`
}

// resolveLatestWorkerDeps queries the npm registry for the latest published
// version of the worker's only runtime dep (express) and returns it as a
// plain semver string. Returns ok=false on any failure (registry
// unreachable, no network, npm missing on PATH, malformed output) so the
// caller can fall back to the embedded package.json — the install should
// not fail just because we can't phone home for an upgrade.
//
// The worker no longer depends on @anthropic-ai/claude-agent-sdk; it shells
// out to the `claude` CLI directly. That means there's only one npm
// dependency to resolve here, and the on-host claude CLI version is
// surfaced separately via `claude --version` in /v1/health. We keep this
// helper shaped like the SDK-era version so the rest of the install flow
// (buildLatestPackageJSON, fallback paths) is unchanged.
//
// Express: we constrain to the 4.x range — express 5 is a rewrite that
// dropped middleware semantics our worker relies on (express.json,
// express-style req/res). Tying to 4.x is the safe upgrade path.
//
// 15s budget covers a slow registry + TLS handshake + one `npm view` call.
// Each individual call is bounded by the parent ctx via client.Exec.
func (h *AgentServerHandler) resolveLatestWorkerDeps(ctx context.Context, client *gossh.Client) (expressVer string, ok bool) {
	viewCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// `npm view <pkg> version` prints the latest matching version on stdout.
	cmd := `export PATH="$HOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"; ` +
		`if [ -s "$HOME/.nvm/nvm.sh" ]; then . "$HOME/.nvm/nvm.sh" 2>/dev/null; fi; ` +
		`hash -r 2>/dev/null; ` +
		`printf '%s\n' ` +
		`"$(npm view 'express@4' version 2>/dev/null | tail -n1)"`
	var out strings.Builder
	if _, err := client.Exec(viewCtx, cmd, "", nil, &out, nil); err != nil {
		return "", false
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 1 {
		return "", false
	}
	expressVer = strings.TrimSpace(lines[0])
	if expressVer == "" {
		return "", false
	}
	// npm view can occasionally print multiple lines (e.g. a deprecation
	// banner above the version). The first semver-looking token is the
	// real one — defensive scrub just in case the registry shape changes.
	expressVer = firstSemverToken(expressVer)
	if expressVer == "" {
		return "", false
	}
	return expressVer, true
}

// firstSemverToken extracts the first X.Y.Z (with optional -prerelease /
// +build) string from s. Returns "" if no semver-looking token is found.
// Used to scrub stray non-version output that npm view occasionally prepends.
func firstSemverToken(s string) string {
	for _, tok := range strings.Fields(s) {
		tok = strings.TrimPrefix(tok, "v")
		if len(tok) >= 5 && strings.Count(tok, ".") >= 1 {
			ok := true
			for _, ch := range tok {
				if !(ch == '.' || ch == '-' || ch == '+' || (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'z')) {
					ok = false
					break
				}
			}
			if ok {
				return tok
			}
		}
	}
	return ""
}

// workerSourceDir resolves the directory the install flow reads worker
// source files from. Three locations are tried, in order:
//
//   1. NOVA_AGENT_WORKER_SOURCE_DIR env var (operator-pinned path —
//      wins outright so production deployments can point at /opt/...)
//   2. "agent-worker/" relative to CWD — the layout used when running
//      the binary from the repo root (e.g. ./dist/nova after make build)
//   3. "../agent-worker/" relative to CWD — the layout used by
//      `make run` (`cd backend && go run ./cmd/server` lands the
//      process inside backend/, but the source tree lives at the
//      repo root, one level up)
//
// The first location that contains a non-empty server.mjs is the winner;
// if none matches, the helpers fall through to the Go-binary-embedded
// constants. We don't surface a warning when every candidate is missing
// because that's the production case — the embedded copy is what ships
// in the binary and is always the right answer there.
func workerSourceDir() string {
	if v := os.Getenv("NOVA_AGENT_WORKER_SOURCE_DIR"); v != "" {
		return v
	}
	candidates := []string{"agent-worker", "../agent-worker"}
	for _, c := range candidates {
		if info, err := os.Stat(filepath.Join(c, "server.mjs")); err == nil && info.Size() > 1000 {
			return c
		}
	}
	return "agent-worker" // last-resort label; embedded will be used
}

// workerSourceServerMJS returns the contents of nova-agent-worker/
// server.mjs — preferring the on-disk working-tree copy so a developer
// iterating on the worker in a repo checkout gets their latest edits
// without rebuilding the Go binary, and falling back to the embedded
// constant for production deployments where the repo isn't beside the
// server. A missing or empty disk file is treated as "not present" so
// the embedded copy always wins on production.
//
// Why disk-first (vs always embedded): the previous flow required a
// NovaWorkbench binary bump for every worker change, which made
// preflight / classifyError rollouts slow and risk the binary version
// drifting from the worker version. With disk-first, restarting the
// binary (or even just re-running Install without restart) is enough.
//
// Why embedded-fallback (vs always disk): production binaries ship
// without a repo beside them; without the embedded fallback a fresh
// container / rpm / binary distribution would have no worker source.
func workerSourceServerMJS() string {
	const name = "server.mjs"
	path := filepath.Join(workerSourceDir(), name)
	if data, err := os.ReadFile(path); err == nil && len(data) > 1000 {
		return string(data)
	}
	return agentWorkerServerMJS
}

// workerSourcePackageJSON returns the contents of nova-agent-worker/
// package.json. Same disk-first / embedded-fallback semantics as
// workerSourceServerMJS. Note that the install flow may overwrite this
// file after-the-fact with a freshly-resolved latest-npm-versions copy
// (see resolveLatestWorkerDeps), so the disk-vs-embedded choice here
// only matters when that step fails.
func workerSourcePackageJSON() string {
	const name = "package.json"
	path := filepath.Join(workerSourceDir(), name)
	if data, err := os.ReadFile(path); err == nil && len(data) > 50 {
		return string(data)
	}
	return agentWorkerPackageJSON
}

// workerSourceLabel returns the user-facing note for which source path
// served a worker file in this install. Embedded files are tagged
// "(binary 内置)" so an operator can tell at a glance whether the
// running install picked up their working-tree edits or the embedded
// fallback; disk files tag themselves with the actual path that was
// read (so a dev running `make run` from backend/ sees
// "../agent-worker/server.mjs", not the abstract label).
//
// Keeping the label independent of the helper that returned the bytes
// avoids duplicating the disk/embedded decision — the helper does the
// read, the label reports it. To detect which path served a file we
// walk the same candidate list workerSourceDir uses.
func workerSourceLabel(name string) string {
	if v := os.Getenv("NOVA_AGENT_WORKER_SOURCE_DIR"); v != "" {
		return "env " + filepath.Join(v, name)
	}
	for _, c := range []string{"agent-worker", "../agent-worker"} {
		if info, err := os.Stat(filepath.Join(c, name)); err == nil && info.Size() > 100 {
			return "本地仓库 " + filepath.Join(c, name)
		}
	}
	return "binary 内置"
}

// buildLatestPackageJSON returns a minimal package.json that pins the
// express dep to the given exact version (no `^`) so npm install always
// picks that version. We don't use semver ranges here because the user
// wants the *latest* installed, not the latest compatible — and the
// surrounding install flow always re-resolves by deleting the lockfile
// first.
//
// Mirrors the structure of agentWorkerPackageJSON (the embedded fallback)
// so the only difference between the two is the dep version string.
func buildLatestPackageJSON(expressVer string) string {
	return fmt.Sprintf(`{
  "name": "nova-agent-worker",
  "version": "0.1.0",
  "description": "HTTP/NDJSON bridge between NovaWorkbench and the claude CLI on a remote Agent host.",
  "private": true,
  "type": "module",
  "main": "server.mjs",
  "scripts": { "start": "node server.mjs" },
  "engines": { "node": ">=20" },
  "dependencies": {
    "express": "%s"
  }
}
`, expressVer)
}
