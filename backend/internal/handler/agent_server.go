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
	"fmt"
	"log"
	"net/http"
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
	if exit, _ := client.Exec(ctx, "uname -s", "", nil, &unameOut); exit != 0 {
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
		exit, _ := client.Exec(ctx, cmd, "", nil, &out)
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
	_ = h.svc.UpdateStatus(serverID, status, summary)
	job.Append(store.LogLine{Type: "done", Content: summary})
	job.Finish(0, store.JobDone)
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
	client.Exec(ctx, "uname -s", "", nil, &unameOut)
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
		exit, _ := client.Exec(ctx, cmd, "", nil, &out)
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
