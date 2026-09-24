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
	"bytes"
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
	svc        *service.AgentServerService
	jobs       *store.JobStore
	projectSvc *service.ProjectService
}

func NewAgentServerHandler(svc *service.AgentServerService, jobs *store.JobStore, projectSvc *service.ProjectService) *AgentServerHandler {
	return &AgentServerHandler{svc: svc, jobs: jobs, projectSvc: projectSvc}
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

// TestConnection performs a one-shot SSH handshake against the saved
// credential. Unlike Check (which runs `uname` + dep probes in the
// background and streams logs), this returns synchronously so the settings
// UI can render a "✅ connected to <host>" badge without setting up an SSE
// stream. POST /api/settings/agent-servers/{id}/test.
func (h *AgentServerHandler) TestConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "MISSING_ID", "缺少 agent server id")
		return
	}
	version, err := h.svc.TestConnection(r.Context(), id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
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
		{"gpg", "gpg"},
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

	// Git remote reachability probe — moved to AFTER SSH-bound checks so the
	// 30s runCheck ctx always has full budget for connection / deps /
	// settings.json / worker. Server-scoped (no project context), so
	// "no probeable remote" is not an error; only a failed probe is.
	// Run on the LOCAL host and bound to 15s; with GIT_HTTP_LOW_SPEED_*
	// set above, even network-blocked remotes give up in ~5s.
	job.Append(store.LogLine{Type: "phase", Content: "🔍 检查 git 远程访问..."})
	if h.projectSvc == nil {
		job.Append(store.LogLine{Type: "message", Content: "ℹ️ ProjectService 未注入，跳过 git 远程探测"})
	} else if rawURL, perr := h.projectSvc.FirstOriginURLForProbe(); perr != nil {
		// DB error → not a hard block; the rest of the check still runs.
		job.Append(store.LogLine{Type: "message", Content: "⚠️ 无法读取项目列表以探测 git 远程: " + perr.Error()})
	} else if rawURL == "" {
		job.Append(store.LogLine{Type: "message", Content: "ℹ️ 无可探测的 git 远程（尚无项目配置 remote_url），跳过"})
	} else {
		job.Append(store.LogLine{Type: "message", Content: "📡 探测: " + redactOriginForLog(rawURL)})
		// 15s timeout via the shared gitRunWithTimeout helper (worktree.go).
		// The env slice disables git's terminal prompt so a missing/unauthorized
		// credential fails fast instead of blocking on stdin; the
		// GIT_HTTP_LOW_SPEED_* pair tells libcurl to give up after 5s of
		// throughput <1B/s — critical when the network blocks github.com
		// (otherwise libcurl's default TCP-connect timer is ~75s and burns
		// the whole runCheck ctx before SSH-bound checks can finish).
		if _, gerr := gitRunWithTimeout(".", 15*time.Second,
			[]string{
				"GIT_TERMINAL_PROMPT=0",
				// libcurl low-speed cutoff: when the network blocks github.com (no SYN-ACK),
				// git-remote-https normally hangs for ~75s on its own TCP-connect timer.
				// Pinning low-speed to 5s/1B makes it give up in ~5s, so the 15s Go ctx
				// still has headroom for subsequent SSH-bound checks in runCheck.
				"GIT_HTTP_LOW_SPEED_TIME=5",
				"GIT_HTTP_LOW_SPEED_LIMIT=1",
			},
			"ls-remote", "--heads", rawURL); gerr != nil {
			// Take the first line of stderr (which gitRunWithTimeout prefixes
			// to the error) for a human-readable failure reason. Fall back to
			// a generic hint if it's empty.
			first := strings.TrimSpace(strings.SplitN(gerr.Error(), "\n", 2)[0])
			if first == "" {
				first = "ls-remote 失败"
			}
			// 软失败:git 远程不可达 ≠ Agent Server 不可用。
			// - status 保持 AgentServerStatusReady(由 deps + settings.json + worker 决定)
			// - summary 不污染,仅在日志里给出明确语义清晰的提示
			// - 后续真正的执行(wizard 远程执行 / 项目 clone/push)若依赖 github 访问,
			//   会走自己的 gitRunWithTimeout(已在 worktree.go:156 / push_pr_shell.go:202 等处
			//   有独立 ctx 与超时),由各自负责。
			job.Append(store.LogLine{Type: "warning", Content: "⚠ git 远程访问失败: " + first +
				"（仅影响从本地控制器拉取/推送项目代码;Agent Server 自身环境仍可正常用于远程执行任务）"})
		} else {
			job.Append(store.LogLine{Type: "message", Content: "✓ git 远程可访问"})
		}
	}

	_ = h.svc.UpdateStatus(serverID, status, summary)
	// Asset-inventory snapshot. Best-effort — never aborts the Check.
	// Persisted into agent_servers.system_info so the settings panel can
	// render OS / kernel / CPU / IPs / uptime without SSH'ing back.
	// We re-derive a background ctx here because runCheck is called from
	// a goroutine that doesn't carry a request ctx (see the ctx created
	// at the top of runCheck) and the SSH client's underlying conn has
	// its own deadline — collectSystemInfo will internally cap itself at
	// 12s.
	if infoJSON, _, ierr := h.collectSystemInfo(context.Background(), client, job); ierr == nil && infoJSON != "" {
		_ = h.svc.UpdateSystemInfo(serverID, infoJSON)
	}
	// Backfill runtime paths on Check too. Previously install was the
	// only writer of claude_bin / node_bin / extra_paths, so any server
	// installed before this code shipped (or whose install ran on a
	// pre-fix binary) kept those columns empty forever — even though
	// every successful Check has all the inputs needed (the SSH session
	// already resolved `command -v claude/node` for the dep probe, and
	// `cat ~/.novaworkbench/extra-paths` is one extra fetch). Cheap,
	// idempotent, matches user expectation that "Check success" implies
	// the asset panel is up-to-date.
	h.captureInstallRuntimeFacts(context.Background(), client, serverID, job)
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
	// Persist the job id BEFORE returning so a page refresh in the gap
	// between writeJSON and the goroutine actually starting can still find
	// the job via GET /api/settings/agent-servers/{id}. Without this write
	// the frontend only learns jobId from the response body, which is lost
	// on reload.
	if perr := h.svc.UpdateInstallJob(a.ID, job.ID); perr != nil {
		log.Printf("[agent-server] failed to persist install_job_id for %s: %v", a.ID, perr)
	}
	writeJSON(w, 200, map[string]string{"job_id": job.ID})
	go h.runInstall(job, a.ID)
}

func (h *AgentServerHandler) runInstall(job *store.Job, serverID string) {
	defer func() {
		// Always clear install_job_id on exit — whether normal Finish, error
		// Finish, or panic. JobStore is in-memory and can evict the job on
		// backend restart; leaving a stale id in the DB column would let a
		// later refresh mount-time reconnect attempt hit a 404 / empty
		// stream with no install actually running. Clearing unconditionally
		// makes "stale install_job_id" an impossible state.
		if cerr := h.svc.UpdateInstallJob(serverID, ""); cerr != nil {
			log.Printf("[agent-server] failed to clear install_job_id for %s: %v", serverID, cerr)
		}
		if rec := recover(); rec != nil {
			log.Printf("[agent-server] install panic for %s: %v", serverID, rec)
			job.Append(store.LogLine{Type: "error", Content: fmt.Sprintf("panic: %v", rec)})
			_ = h.svc.UpdateStatus(serverID, model.AgentServerStatusError, "内部错误")
			job.Finish(1, store.JobError)
		}
	}()

	// 30min budget for the full install (platform script + npm install +
	// systemd setup + worker fallback). 10min was too tight on slow networks
	// where `brew update` + `npm i -g @anthropic-ai/claude-code` + npm install
	// for the worker can eat 8-12min on their own, and the SSH ctx firing
	// would terminate the foreground shell with SIGTERM (exit=143) on the
	// nohup fallback. The actual install rarely needs this much; the headroom
	// just absorbs the long tail without false-positive failures.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
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
	// No `defer client.Close()` here: when the SSH user is root we swap `client`
	// for a re-dial as a non-root user partway through (see below), and a plain
	// defer would capture the original root client, leaking the replacement.
	// The closure re-reads `client` at return time so it always closes whichever
	// connection is active.
	defer func() { _ = client.Close() }()

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

	// Root handling: the Claude CLI refuses --dangerously-skip-permissions as
	// root, so a worker installed under root can never complete a coding pass.
	// On Linux + key auth we provision a non-root user and re-dial the SSH
	// session as that user below, so everything downstream (worker, git
	// worktree, claude session dir) runs under one non-root account. See the
	// provisioning section above for why this has to be a full user switch
	// rather than a worker-only drop of privileges.
	switchUser := false
	if isRoot, rerr := detectRoot(ctx, client); rerr != nil {
		job.Append(store.LogLine{Type: "error", Content: "❌ " + rerr.Error()})
		_ = h.svc.UpdateStatus(serverID, model.AgentServerStatusError, rerr.Error())
		job.Finish(1, store.JobError)
		return
	} else if isRoot {
		switch {
		case platform != "Linux":
			// macOS root is vanishingly rare; keep the existing behavior and let
			// the coding path's running_as_root diagnostic surface the fix.
		case srv.AuthType != model.AgentServerAuthKey:
			// Password-auth root: we can't hand a new user the same credential
			// (there's no authorized_keys to copy), so auto-provision is off the
			// table. Fail fast with the same guidance the diagnostic gives.
			msg := "Agent 服务器以 root + 密码认证运行，无法自动切换普通用户。请在服务器上创建普通用户并把 NovaWorkbench 所在机器的 SSH 公钥写入其 ~/.ssh/authorized_keys，然后在「设置 → Agent 服务器」把用户名改为该普通用户后重试。"
			job.Append(store.LogLine{Type: "error", Content: "❌ " + msg})
			_ = h.svc.UpdateStatus(serverID, model.AgentServerStatusError, msg)
			job.Finish(1, store.JobError)
			return
		default:
			job.Append(store.LogLine{Type: "phase", Content: "🛡️ 检测到 root 运行，自动创建普通用户 " + agentWorkerNonRootUser + "..."})
			if err := provisionNonRootUser(ctx, client, agentWorkerNonRootUser, job); err != nil {
				job.Append(store.LogLine{Type: "error", Content: "❌ " + err.Error()})
				_ = h.svc.UpdateStatus(serverID, model.AgentServerStatusError, err.Error())
				job.Finish(1, store.JobError)
				return
			}
			switchUser = true
		}
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

	// Platform deps (claude/node/npm) are now installed system-wide, so the
	// non-root user we just provisioned can reach them on the default PATH.
	// Re-dial the SSH session AS that user before deploying the worker, so the
	// worker, its systemd --user unit / nohup fallback, and every claude spawn
	// run as non-root. This is the step that actually satisfies the CLI's root
	// guard — merely installing the worker under root would not.
	if switchUser {
		_ = client.Close()
		client, err = gossh.Dial(ctx, srv.Host, srv.Port, agentWorkerNonRootUser, srv.AuthType, plain)
		if err != nil {
			msg := "以普通用户 " + agentWorkerNonRootUser + " 重新连接失败（请确认服务器允许该用户 SSH 登录，且 authorized_keys 已复制）: " + err.Error()
			job.Append(store.LogLine{Type: "error", Content: "❌ " + msg})
			_ = h.svc.UpdateStatus(serverID, model.AgentServerStatusError, msg)
			job.Finish(1, store.JobError)
			return
		}
		job.Append(store.LogLine{Type: "message", Content: "✓ 已以普通用户 " + agentWorkerNonRootUser + " 重新连接"})
	}

	// Step 1b: deploy nova-agent-worker. The platform install script above
	// only put `claude` on PATH; the worker is a separate Node.js service
	// that bridges NovaWorkbench → claude CLI (POST → spawn claude → SSE).
	// We SFTP the source files (embedded as Go constants in
	// agent_worker_files.go), install deps via npm, and start the service.
	// Without this step the wizard's remote coding flow has no /v1/run
	// endpoint to POST to.
	job.Append(store.LogLine{Type: "phase", Content: "🚀 部署 nova-agent-worker..."})
	if err := h.installNodeWorker(ctx, client, platform, serverID, job); err != nil {
		msg := fmt.Sprintf("nova-agent-worker 部署失败: %v", err)
		job.Append(store.LogLine{Type: "error", Content: msg})
		_ = h.svc.UpdateStatus(serverID, model.AgentServerStatusError, msg)
		job.Finish(1, store.JobError)
		return
	}

	// Only after the worker install succeeded do we flip the stored username.
	// Ordering matters: if we wrote `nova` and the install failed, a later
	// check/coding run would dial a half-provisioned account. Persisting last
	// keeps the record self-consistent on every failure path above.
	if switchUser {
		nova := agentWorkerNonRootUser
		if _, uerr := h.svc.Update(serverID, model.UpdateAgentServerReq{Username: &nova}); uerr != nil {
			job.Append(store.LogLine{Type: "message", Content: "⚠ 更新 SSH 用户名失败（请手动改用户名后重试）: " + uerr.Error()})
		} else {
			job.Append(store.LogLine{Type: "message", Content: "✓ 已将 SSH 用户切换为 " + agentWorkerNonRootUser + "，后续操作将以普通用户运行"})
		}
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
	if ver, err := probeWorkerHealth(ctx, client); err == nil {
		// The worker is listening, but it may be a stale process that survived
		// a previous install (an old worker still bound to 7000). Compare the
		// reported version against what this binary expects; a mismatch means
		// the deployed worker predates this build and must be re-installed.
		if ver != agentWorkerVersion {
			msg := "运行中的 worker 版本为 " + workerVersionDisplay(ver) +
				"，与当前期望 " + agentWorkerVersion + " 不一致：旧进程可能仍占用 7000 端口，请重新点「安装依赖」"
			job.Append(store.LogLine{Type: "error", Content: "❌ " + msg})
			return model.AgentServerStatusError, msg
		}
		job.Append(store.LogLine{Type: "message", Content: "✓ nova-agent-worker 已就绪（版本 " + ver + "）"})
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

// ---- non-root provisioning -------------------------------------------------
//
// The Claude CLI hard-refuses --dangerously-skip-permissions when the
// effective uid is root (or sudo is in effect) — its guard is sandbox-only,
// there is no env-var bypass (see the note in llm/gateway.go). The wizard's
// remote coding path depends on that flag for full tool access, so a worker
// running under root can never complete a coding pass. Rather than ask the
// operator to manually create a non-root user and re-point the Agent-server
// record at it, the install flow provisions one automatically (see
// runInstall): create a regular user, hand it the same SSH key the operator
// already uses for root, then re-dial the SSH session AS that user and
// install the worker under its $HOME. Subsequent check/coding runs dial the
// new user too, so the git worktree, claude session dir, and worker all live
// under one non-root account — which is exactly what makes the CLI's root
// guard happy.
const agentWorkerNonRootUser = "nova"

// detectRoot reports whether the SSH session's effective uid is 0. The CLI
// rejects --dangerously-skip-permissions on uid 0 precisely, so this is the
// single source of truth for whether provisioning is needed.
func detectRoot(ctx context.Context, client *gossh.Client) (bool, error) {
	var out strings.Builder
	if exit, err := client.Exec(ctx, "id -u", "", nil, &out, nil); err != nil || exit != 0 {
		return false, fmt.Errorf("读取远程 uid 失败（exit=%d err=%v）", exit, err)
	}
	return strings.TrimSpace(out.String()) == "0", nil
}

// nonRootProvisionScript builds the idempotent shell that provisions a
// non-root agent user on a Linux host. Run as root only. Steps:
//  1. create the user if missing (useradd, with an adduser fallback for
//     Debian-family images that ship adduser but not useradd);
//  2. enable-linger so the user's systemd --user manager survives SSH
//     sessions (root can do this without a polkit agent — the worker's own
//     `loginctl enable-linger $(id -u)` run AS the user often can't);
//  3. resolve the new user's $HOME and stage ~/.ssh;
//  4. copy root's authorized_keys so the SAME private key NovaWorkbench
//     already holds can authenticate as the new user (key-auth only).
//
// Kept as a pure function so the provisioning contract is unit-testable.
// PATH is widened first so useradd/adduser/loginctl (often under /usr/sbin,
// off the default non-interactive SSH PATH) resolve without absolute paths.
func nonRootProvisionScript(user string) string {
	return `export PATH="/usr/local/sbin:/usr/sbin:/sbin:$PATH"
if ! id ` + user + ` >/dev/null 2>&1; then
  if ! useradd -m -s /bin/bash ` + user + ` 2>/dev/null; then
    adduser --disabled-password --gecos "" ` + user + ` 2>/dev/null || true
  fi
fi
id ` + user + ` >/dev/null 2>&1 || { echo "[nova-agent] 无法创建普通用户 ` + user + `"; exit 1; }
loginctl enable-linger ` + user + ` 2>/dev/null || true
HOMEDIR=$(getent passwd ` + user + ` | cut -d: -f6)
if [ -n "$HOMEDIR" ]; then
  mkdir -p "$HOMEDIR/.ssh"
  if [ -f "$HOME/.ssh/authorized_keys" ]; then
    cp "$HOME/.ssh/authorized_keys" "$HOMEDIR/.ssh/authorized_keys"
  fi
  chown -R ` + user + `:` + user + ` "$HOMEDIR/.ssh"
  chmod 700 "$HOMEDIR/.ssh"
  chmod 600 "$HOMEDIR/.ssh/authorized_keys" 2>/dev/null || true
fi
echo "[nova-agent] provisioned non-root user ` + user + ` at ${HOMEDIR:-?}"
`
}

// provisionNonRootUser runs the provisioning script on the (root) SSH session
// and streams its output into the job log. Idempotent: re-running install
// re-copies the key and re-chowns, so a partially-provisioned or
// manually-created user converges to the same state.
func provisionNonRootUser(ctx context.Context, client *gossh.Client, user string, job *store.Job) error {
	if exit, err := client.RunScript(ctx, nonRootProvisionScript(user), "provision", nil, jobLineWriter(job)); err != nil || exit != 0 {
		return fmt.Errorf("创建普通用户 %s 失败（exit=%d err=%v）", user, exit, err)
	}
	return nil
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
func (h *AgentServerHandler) installNodeWorker(ctx context.Context, client *gossh.Client, platform, serverID string, job *store.Job) error {
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
	// Stamp the worker version into server.mjs before upload. agentWorkerVersion
	// is the binary's single source of truth; the running worker reports it back
	// via /v1/health so the install can confirm the new process (not a stale one
	// still bound to 7000) actually took over. Both the disk and embedded sources
	// carry the __WORKER_VERSION__ placeholder, so the stamp is uniform regardless
	// of which source served the file.
	serverBody := strings.ReplaceAll(workerSourceServerMJS(), "__WORKER_VERSION__", agentWorkerVersion)
	for _, f := range []struct {
		path string
		body string
		note string
	}{
		{installDir + "/server.mjs", serverBody, workerSourceLabel("server.mjs")},
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

	// Resolve the absolute path to `node` on the remote host so the service
	// unit can invoke it directly instead of through `/usr/bin/env node`.
	// The service manager (systemd --user on Linux, launchd on macOS) runs
	// with its own PATH that does NOT include user-space installs — nvm's
	// ~/.nvm/versions/node/*/bin, or /opt/homebrew/bin on Apple Silicon — so
	// a worker installed via the nvm/brew fallback would otherwise fail to
	// start under the manager even though `node` is on the interactive PATH.
	// Baking the absolute path makes the unit independent of that PATH.
	// nodeBin stays "" (leaving the unit's `/usr/bin/env node` in place) if
	// resolution fails or the result isn't absolute, so a broken probe never
	// blocks the install.
	var nodeBin string
	{
		var nodeBuf strings.Builder
		if exit, _ := client.Exec(ctx,
			`export PATH="$HOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"; `+
				`if [ -s "$HOME/.nvm/nvm.sh" ]; then . "$HOME/.nvm/nvm.sh" 2>/dev/null; fi; `+
				`hash -r 2>/dev/null; `+
				`command -v node`, "", nil, &nodeBuf, nil); exit == 0 {
			bin := strings.TrimSpace(nodeBuf.String())
			if strings.HasPrefix(bin, "/") {
				nodeBin = bin
			}
		}
	}

	// Read the install-time extra-paths file written by linuxInstallScript /
	// darwinInstallScript. Each line is a directory that contains the
	// actually-installed `claude` binary (e.g. /root/.npm-global/bin,
	// /root/.nvm/versions/node/22/bin, /opt/homebrew/bin). Without merging
	// these into the worker launch PATH, the worker process — which runs
	// under nova/ubuntu after provisionNonRootUser, NOT under the user that
	// ran npm install -g — cannot resolve `claude` by name and /v1/run's
	// preflight fails with `spawn claude ENOENT` (`cli_not_found`).
	//
	// We read this BEFORE writing the unit / plist so we can substitute the
	// final PATH into the systemd Environment=PATH=__WORKER_PATH__ /
	// launchd <key>PATH</key> placeholders. The nohup fallback below uses
	// the same value via the inline `env PATH=...` line.
	extraPathDirs := readExtraPaths(ctx, client, homeDir)
	if len(extraPathDirs) > 0 {
		job.Append(store.LogLine{Type: "message", Content: fmt.Sprintf("✓ 额外 PATH 段: %s", strings.Join(extraPathDirs, ":"))})
	} else {
		job.Append(store.LogLine{Type: "message", Content: "ℹ️  未发现额外 PATH 段（extra-paths 为空 / 不存在），将依赖默认 PATH"})
	}
	// Compose the worker PATH = (current SSH PATH, with extra dirs prepended)
	// + safe fallbacks. systemd --user starts with the bare PATH so the
	// extras MUST be present or spawn('claude') ENOENTs.
	workerPATH := composeWorkerPATH(ctx, client, extraPathDirs)
	job.Append(store.LogLine{Type: "message", Content: "✓ worker PATH = " + workerPATH})

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
		// Bake the resolved PATH into the Environment=PATH=__WORKER_PATH__
		// placeholder. Without this the systemd --user manager gives the
		// worker the bare PATH
		// (/usr/local/sbin:/usr/local/bin:/usr/bin:/usr/sbin:/sbin:/bin)
		// and the worker's spawn('claude', …) ENOENTs — see the comment
		// on workerPATH / extraPathDirs above.
		unitBody = strings.ReplaceAll(unitBody,
			"Environment=PATH=__WORKER_PATH__",
			"Environment=PATH="+workerPATH)
		// Bake the resolved node path into ExecStart too — see the nodeBin
		// comment above. `/usr/bin/env node` would consult the user manager's
		// own PATH, which misses nvm/brew node and leaves the service in a
		// crash loop (and the worker permanently on the nohup fallback).
		if nodeBin != "" {
			unitBody = strings.ReplaceAll(unitBody,
				"ExecStart=/usr/bin/env node server.mjs",
				"ExecStart="+nodeBin+" server.mjs")
		}
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
		// Bake the resolved PATH into the EnvironmentVariables.PATH
		// placeholder. Without this launchd starts the agent with the
		// system PATH (no /opt/homebrew/bin, no $HOME/.npm-global/bin) and
		// the worker's spawn('claude', …) ENOENTs.
		plistBody = strings.ReplaceAll(plistBody,
			"<key>PATH</key><string>__WORKER_PATH__</string>",
			"<key>PATH</key><string>"+workerPATH+"</string>")
		// launchd runs agents with the system PATH too, so `/usr/bin/env
		// node` misses Homebrew's /opt/homebrew/bin. Bake the resolved node
		// path into ProgramArguments the same way we do for systemd.
		if nodeBin != "" {
			plistBody = strings.ReplaceAll(plistBody,
				"<string>/usr/bin/env</string>\n    <string>node</string>",
				"<string>"+nodeBin+"</string>")
		}
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
	if ver, err := probeWorkerHealth(ctx, client); err == nil {
		if ver != agentWorkerVersion {
			// Worker is up but running stale code — the systemd restart was a
			// silent no-op and an old process still holds 7000. Fall through to
			// the nohup path, whose pkill kills the stale process before
			// relaunching from the freshly-written server.mjs.
			job.Append(store.LogLine{Type: "message", Content: "⚠ systemd 路径上的 worker 仍是旧版本 " + workerVersionDisplay(ver) + "，回落到 nohup（会先 kill 旧进程）"})
		} else {
			job.Append(store.LogLine{Type: "message", Content: "✓ worker 健康检查通过（版本 " + ver + "）"})
			if err := h.svc.UpdateWorkerVersion(serverID, agentWorkerVersion); err != nil {
				job.Append(store.LogLine{Type: "message", Content: "⚠ 记录 worker 版本失败（不影响安装）: " + err.Error()})
			}
			// Real claude probe. /v1/health only verifies the worker
			// bound 7000; if spawn('claude') inside the worker still
			// ENOENTs the user will see it as "preflight cli_not_found"
			// on the first wizard run. Surfacing the failure here (via
			// the worker's own SSH user so the test runs in the same env
			// the worker will spawn claude from) turns that into a hard
			// error NOW, with the actual stderr attached.
			if err := h.probeClaudeExecutable(ctx, client, serverID, job); err != nil {
				return err
			}
			// Asset inventory + claude/node bin persistence. Best-effort.
			h.captureInstallRuntimeFacts(ctx, client, serverID, job)
			if infoJSON, _, ierr := h.collectSystemInfo(ctx, client, job); ierr == nil && infoJSON != "" {
				_ = h.svc.UpdateSystemInfo(serverID, infoJSON)
			}
			return nil
		}
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
	//
	// The `grep -vx "$$"` is not optional: `pgrep -f` regex-matches the full
	// command line, and this shell's own argv contains the literal string
	// `nova-agent-worker/server.mjs` (in the `node .../server.mjs` line and
	// the trailing echo). Without excluding our own PID, OLD_PID resolves to
	// this shell and `kill "$OLD_PID"` SIGTERMs ourselves → exit 143, which
	// surfaces as a spurious "nohup 启动失败" even though nothing was wrong.
	launchCmd := `export PATH="$HOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"; ` +
		`if [ -s "$HOME/.nvm/nvm.sh" ]; then . "$HOME/.nvm/nvm.sh" 2>/dev/null; fi; ` +
		`hash -r 2>/dev/null; ` +
		`OLD_PID=$(pgrep -f nova-agent-worker/server.mjs | grep -vx "$$" | head -n1); ` +
		`if [ -n "$OLD_PID" ]; then kill "$OLD_PID" 2>/dev/null; sleep 1; kill -9 "$OLD_PID" 2>/dev/null || true; fi; ` +
		// TMPDIR=/tmp on the nohup env line guards against the macOS dev
		// box's SendEnv forwarding /var/folders/... into the SSH session
		// (which the nohup-launched worker would otherwise inherit).
		// server.mjs's resolveTmpdir also patches this per-spawn for the
		// claude child, but pinning it on the worker itself means Node's
		// own os.tmpdir() is sane before any user code runs.
		//
		// NOVA_AGENT_WORKER_EXTRA_PATHS is read by server.mjs's
		// resolveExtendedPath so the worker's spawn('claude') can locate
		// the CLI when systemd --user didn't take over. The leading PATH=
		// on this env line is belt-and-braces: even if a future server.mjs
		// forgets to honor NOVA_AGENT_WORKER_EXTRA_PATHS, the worker
		// process still has PATH for this launch.
		`nohup env NOVA_AGENT_WORKER_HOST=127.0.0.1 NOVA_AGENT_WORKER_PORT=7000 TMPDIR=/tmp ` +
		`NOVA_AGENT_WORKER_EXTRA_PATHS=` + shellQuote(workerPATH) + ` ` +
		`PATH=` + shellQuote(workerPATH) + ` ` +
		`node ` + installDir + `/server.mjs > ` + installDir + `/worker.log 2>&1 & ` +
		`disown 2>/dev/null || true; ` +
		`sleep 1; echo "[nova-agent] nohup launched, pid=$(pgrep -f nova-agent-worker/server.mjs | grep -vx "$$" | head -n1), killed_old=${OLD_PID:-none}"`
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
	// Final version confirmation. By now we've overwritten server.mjs and (in
	// the nohup path) killed any prior worker, so the running process must
	// report agentWorkerVersion. Anything else means a stale process is still
	// serving 7000 — a hard failure, not a silent "success" that would leave
	// the old worker handling every coding run.
	ver, err := probeWorkerHealth(ctx, client)
	if err != nil {
		return fmt.Errorf("读取 worker 版本失败: %w", err)
	}
	if ver != agentWorkerVersion {
		return fmt.Errorf("worker 已就绪但版本仍为 %s（应为 %s）：旧进程可能仍占用 7000 端口，请 SSH 到服务器执行 `ps aux | grep server.mjs` 手动 kill 后重试", workerVersionDisplay(ver), agentWorkerVersion)
	}
	job.Append(store.LogLine{Type: "message", Content: "✓ worker 健康检查通过 (nohup，版本 " + ver + ")"})
	if err := h.svc.UpdateWorkerVersion(serverID, agentWorkerVersion); err != nil {
		job.Append(store.LogLine{Type: "message", Content: "⚠ 记录 worker 版本失败（不影响安装）: " + err.Error()})
	}
	// Real claude probe (see the systemd branch for rationale).
	if err := h.probeClaudeExecutable(ctx, client, serverID, job); err != nil {
		return err
	}
	// Asset inventory + claude/node bin persistence (best-effort).
	h.captureInstallRuntimeFacts(ctx, client, serverID, job)
	if infoJSON, _, ierr := h.collectSystemInfo(ctx, client, job); ierr == nil && infoJSON != "" {
		_ = h.svc.UpdateSystemInfo(serverID, infoJSON)
	}
	return nil
}

// probeWorkerHealth opens an SSH direct-tcpip channel to 127.0.0.1:7000 on
// the remote host, does a GET /v1/health, and returns the worker's reported
// workerVersion (empty for a worker predating version reporting). Used by both
// install and check flows. Lives here (not in runCheck) so the install flow has
// its own copy without tangling the Check goroutine's status logic.
func probeWorkerHealth(ctx context.Context, client *gossh.Client) (string, error) {
	hc, hcCancel := context.WithTimeout(ctx, 8*time.Second)
	defer hcCancel()
	req, _ := http.NewRequestWithContext(hc, http.MethodGet, "http://127.0.0.1:7000/v1/health", nil)
	resp, err := (&http.Client{Transport: client.HTTPTransport("127.0.0.1:7000")}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var health struct {
		Status        string `json:"status"`
		ClaudeVersion string `json:"claudeVersion"`
		WorkerVersion string `json:"workerVersion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return "", err
	}
	return health.WorkerVersion, nil
}

// workerVersionDisplay returns a human-readable version label; an empty
// version (worker predates version reporting) shows as "未知".
func workerVersionDisplay(v string) string {
	if v == "" {
		return "未知"
	}
	return v
}

// probeClaudeExecutable runs `claude --version` inside the SSH session the
// worker will spawn from (i.e. same $PATH the worker's spawn('claude') will
// see) and surfaces a hard error if it fails. This catches the exact class
// of "ready but cli_not_found" install that motivated this whole flow: the
// Check command in runCheck runs through SSH with an augmented PATH and
// passes, but the worker process — launched under systemd --user or nohup
// — does NOT see the install's actual claude bin dir because of cross-user
// npm prefix / stripped PATH issues. Without this probe the install reports
// `ready` and the user only learns the truth on their first wizard run,
// surfacing as `preflight cli_not_found` mid-coding.
//
// We do NOT trust `which claude` here (it would only mirror what Check
// already does — fail at exactly the same point). Instead we exercise the
// worker's spawn surface area: same augmentations, same `command -v` style,
// then `claude --version` with stderr captured.
//
// Bounded 8s: claude --version is a fast no-API call, so this is plenty
// even on a slow VM. If the user's install path was wrong the failure
// surfaces within 1-2s; we keep the headroom so a momentarily slow node
// startup doesn't trigger a false negative.
func (h *AgentServerHandler) probeClaudeExecutable(ctx context.Context, client *gossh.Client, serverID string, job *store.Job) error {
	job.Append(store.LogLine{Type: "phase", Content: "🔍 真实 spawn claude 校验..."})
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var out strings.Builder
	var stderrBuf bytes.Buffer
	// Mirrors the augmentations installNodeWorker applies elsewhere in
	// this file: install-time PATH widening + nvm source + hash -r.
	cmd := `export PATH="$HOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"; ` +
		`if [ -s "$HOME/.nvm/nvm.sh" ]; then . "$HOME/.nvm/nvm.sh" 2>/dev/null; fi; ` +
		`hash -r 2>/dev/null; ` +
		`claude --version`
	exit, _ := client.Exec(probeCtx, cmd, "", nil, &out, &stderrBuf)
	v := strings.TrimSpace(out.String())
	if exit != 0 {
		// Echo both stdout and stderr to the install panel verbatim. The
		// SSH-session stderr from claude usually contains the real failure
		// reason (e.g. "command not found", "permission denied") and the
		// operator can act on it without SSH-ing in separately.
		msg := fmt.Sprintf("worker 已就绪，但 claude 仍不可达（exit=%d）：%s",
			exit, strings.TrimSpace(stderrBuf.String()))
		job.Append(store.LogLine{Type: "error", Content: "❌ " + msg})
		if serverID != "" {
			_ = h.svc.UpdateStatus(serverID, model.AgentServerStatusError, msg)
		}
		return fmt.Errorf("%s", msg)
	}
	if v == "" {
		v = "(unknown)"
	}
	job.Append(store.LogLine{Type: "message", Content: "✓ claude " + v})
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
		if _, err := probeWorkerHealth(ctx, client); err == nil {
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
	//
	// Resolve workerPATH at this call site too so a Check-driven revive
	// (startWorkerIfDown) carries the same claude-bin-dir fixup install did.
	// Re-reading extra-paths is cheap and lets a later install propagate
	// to a worker restart without persisted state.
	extraPathDirs := readExtraPaths(ctx, client, homeDir)
	workerPATH := composeWorkerPATH(ctx, client, extraPathDirs)
	launchCmd := `export PATH="$HOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"; ` +
		`if [ -s "$HOME/.nvm/nvm.sh" ]; then . "$HOME/.nvm/nvm.sh" 2>/dev/null; fi; ` +
		`hash -r 2>/dev/null; ` +
		// TMPDIR=/tmp guards against a macOS dev box's SendEnv forwarding
		// /var/folders/... into the SSH session. server.mjs also patches
		// this per-spawn for the claude child; pinning it on the worker
		// itself means Node's own os.tmpdir() is sane before any user
		// code runs.
		`nohup env NOVA_AGENT_WORKER_HOST=127.0.0.1 NOVA_AGENT_WORKER_PORT=7000 TMPDIR=/tmp ` +
		`NOVA_AGENT_WORKER_EXTRA_PATHS=` + shellQuote(workerPATH) + ` ` +
		`PATH=` + shellQuote(workerPATH) + ` ` +
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
    apt-get) apt-get update -y >/dev/null 2>&1; apt-get install -y nodejs npm gnupg2 || apt-get install -y nodejs npm gnupg ;;
    dnf) dnf install -y nodejs npm gnupg2 ;;
    yum) yum install -y nodejs npm gnupg2 ;;
  esac
}
if [ -n "$PM" ]; then
  if ! install_with_pm; then
    echo "[nova-agent] 系统包管理器失败，回落到 nvm 用户态安装"
    curl -fsSL https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.1/install.sh | bash >/dev/null
    export NVM_DIR="$HOME/.nvm"
    [ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh"
    nvm install --lts
    echo "[nova-agent] 注意：nvm 回落只装 node，gpg 仍需手动安装（apt: gnupg2 / gnupg，dnf|yum: gnupg2）"
  fi
else
  echo "[nova-agent] 未识别包管理器，使用 nvm 用户态安装"
  curl -fsSL https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.1/install.sh | bash >/dev/null
  export NVM_DIR="$HOME/.nvm"
  [ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh"
  nvm install --lts
  echo "[nova-agent] 注意：nvm 回落只装 node，gpg 仍需手动安装（apt: gnupg2 / gnupg，dnf|yum: gnupg2）"
fi
echo "[nova-agent] 安装 @anthropic-ai/claude-code..."
npm install -g @anthropic-ai/claude-code
echo "[nova-agent] 安装完成"

# --- 暴露 claude 真实 bin 路径到 worker ---------------------------------
# npm install -g 把 claude 装到 root (当前用户) 的 npm prefix 下：
#   * apt 安装的 npm: prefix=/usr → bin 在 /usr/local/bin 或 /usr/bin
#   * nvm 回落:        prefix=$HOME/.nvm/versions/node/<v>
#   * npm 10+ sudo-less: prefix=$HOME/.npm-global → bin 在 $HOME/.npm-global/bin
#
# worker 是以 nova/ubuntu 身份（provisionNonRootUser）启动的，看不到 root-only
# 的 bin 目录，所以 installNodeWorker 需要被告知"claude 真实在哪"。
#
# 我们解析出 CLAUDE_BIN 的绝对路径，把 dirname 写入
# $HOME/.novaworkbench/extra-paths（每行一个目录，幂等），供 installNodeWorker
# 读取并注入到 systemd Environment=PATH / launchd EnvironmentVariables.PATH /
# nohup env PATH=... 三处启动方式。
CLAUDE_BIN="$(command -v claude || true)"
if [ -z "$CLAUDE_BIN" ]; then
  # command -v 找不到的兜底（nvm 下 source 之后才在 PATH 上）
  if [ -s "$HOME/.nvm/nvm.sh" ]; then . "$HOME/.nvm/nvm.sh" 2>/dev/null; fi
  CLAUDE_BIN="$(command -v claude || true)"
fi
if [ -z "$CLAUDE_BIN" ]; then
  # 最后一道兜底——按常见 npm 安装位置盲扫
  for cand in /usr/local/bin/claude /usr/bin/claude \
              "$HOME/.npm-global/bin/claude" "$HOME/.local/bin/claude" \
              "$HOME/bin/claude" \
              /opt/homebrew/bin/claude; do
    if [ -x "$cand" ]; then CLAUDE_BIN="$cand"; break; fi
  done
fi
if [ -n "$CLAUDE_BIN" ]; then
  CLAUDE_DIR="$(dirname "$CLAUDE_BIN")"
  echo "[nova-agent] claude 真实路径 = $CLAUDE_BIN → PATH 段 = $CLAUDE_DIR"
  mkdir -p "$HOME/.novaworkbench"
  # 写入"额外 PATH 段"清单（去重 / 忽略空行 / 忽略已存在项），installNodeWorker 读取。
  EXISTING=""
  [ -f "$HOME/.novaworkbench/extra-paths" ] && EXISTING="$(cat "$HOME/.novaworkbench/extra-paths")"
  case ":$EXISTING:" in
    *":$CLAUDE_DIR:"*) ;;  # already present
    *)
      {
        [ -n "$EXISTING" ] && printf '%s\n' "$EXISTING"
        printf '%s\n' "$CLAUDE_DIR"
      } > "$HOME/.novaworkbench/extra-paths"
      echo "[nova-agent] 已写入 $HOME/.novaworkbench/extra-paths"
      ;;
  esac
  # 顺手把 node 实际路径也记一下（systemd unit 的 ExecStart 想要绝对路径）
  if command -v node >/dev/null 2>&1; then
    NODE_BIN="$(command -v node)"
    echo "$NODE_BIN" > "$HOME/.novaworkbench/node-bin"
    echo "[nova-agent] node 真实路径 = $NODE_BIN"
  fi
  # Machine-readable marker for the install goroutine to grep + persist
  # into agent_servers (claude_bin / node_bin / extra_paths columns). The
  # Go side resolves the same paths via 'command -v' and would otherwise
  # disagree with what this script wrote to disk if a non-default PATH
  # changed between runs — going through the script's own output keeps the
  # DB row and the on-disk file in lockstep. Emitted LAST so 'grep' picks
  # the freshest line in the install SSE log.
  echo "[nova-agent] RUNTIME_BIN CLAUDE_BIN=$CLAUDE_BIN NODE_BIN=${NODE_BIN:-}"
else
  echo "[nova-agent] ⚠️  无法定位 claude 二进制，installNodeWorker 将仅依赖默认 PATH"
  # Even when claude is missing we still want the marker line so Go can
  # parse safely (claude_bin defaults to empty). node_bin is best-effort
  # since systemd unit still wants an absolute path if we have one.
  if command -v node >/dev/null 2>&1; then
    echo "[nova-agent] RUNTIME_BIN CLAUDE_BIN= NODE_BIN=$(command -v node)"
  else
    echo "[nova-agent] RUNTIME_BIN CLAUDE_BIN= NODE_BIN="
  fi
fi
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
echo "[nova-agent] 安装 gnupg..."
brew install gnupg || true
echo "[nova-agent] 安装 @anthropic-ai/claude-code..."
npm install -g @anthropic-ai/claude-code
echo "[nova-agent] 安装完成"

# --- 暴露 claude 真实 bin 路径到 worker ---------------------------------
# launchd 在系统域运行 LaunchAgent 时给的 PATH 不含 /opt/homebrew/bin；worker
# 进程 spawn('claude', …) 就会 ENOENT。把 claude 实际所在的目录写入
# $HOME/.novaworkbench/extra-paths，供 installNodeWorker 拼接到 launchd plist
# 的 EnvironmentVariables.PATH / nohup env 的 PATH=...。
CLAUDE_BIN="$(command -v claude || true)"
if [ -z "$CLAUDE_BIN" ]; then
  for cand in /opt/homebrew/bin/claude /usr/local/bin/claude \
              "$HOME/.npm-global/bin/claude" "$HOME/.local/bin/claude"; do
    if [ -x "$cand" ]; then CLAUDE_BIN="$cand"; break; fi
  done
fi
if [ -n "$CLAUDE_BIN" ]; then
  CLAUDE_DIR="$(dirname "$CLAUDE_BIN")"
  echo "[nova-agent] claude 真实路径 = $CLAUDE_BIN → PATH 段 = $CLAUDE_DIR"
  mkdir -p "$HOME/.novaworkbench"
  EXISTING=""
  [ -f "$HOME/.novaworkbench/extra-paths" ] && EXISTING="$(cat "$HOME/.novaworkbench/extra-paths")"
  case ":$EXISTING:" in
    *":$CLAUDE_DIR:"*) ;;
    *)
      {
        [ -n "$EXISTING" ] && printf '%s\n' "$EXISTING"
        printf '%s\n' "$CLAUDE_DIR"
      } > "$HOME/.novaworkbench/extra-paths"
      echo "[nova-agent] 已写入 $HOME/.novaworkbench/extra-paths"
      ;;
  esac
  if command -v node >/dev/null 2>&1; then
    NODE_BIN="$(command -v node)"
    echo "$NODE_BIN" > "$HOME/.novaworkbench/node-bin"
    echo "[nova-agent] node 真实路径 = $NODE_BIN"
  fi
  # Machine-readable marker for the install goroutine to grep + persist
  # into agent_servers (claude_bin / node_bin / extra_paths columns). See
  # linuxInstallScript for the rationale; darwin uses the same shape.
  echo "[nova-agent] RUNTIME_BIN CLAUDE_BIN=$CLAUDE_BIN NODE_BIN=${NODE_BIN:-}"
else
  echo "[nova-agent] ⚠️  无法定位 claude 二进制，installNodeWorker 将仅依赖默认 PATH"
  if command -v node >/dev/null 2>&1; then
    echo "[nova-agent] RUNTIME_BIN CLAUDE_BIN= NODE_BIN=$(command -v node)"
  else
    echo "[nova-agent] RUNTIME_BIN CLAUDE_BIN= NODE_BIN="
  fi
fi
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
//  1. NOVA_AGENT_WORKER_SOURCE_DIR env var (operator-pinned path —
//     wins outright so production deployments can point at /opt/...)
//  2. "agent-worker/" relative to CWD — the layout used when running
//     the binary from the repo root (e.g. ./dist/nova after make build)
//  3. "../agent-worker/" relative to CWD — the layout used by
//     `make run` (`cd backend && go run ./cmd/server` lands the
//     process inside backend/, but the source tree lives at the
//     repo root, one level up)
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

// ---- asset inventory / runtime facts --------------------------------------
//
// collectSystemInfo / captureInstallRuntimeFacts persist the install-time
// truth (claude / node bin paths, OS / kernel / CPU / mem / IP / disk /
// uptime) into agent_servers columns so the settings UI can render an
// "Asset Inventory" panel without SSH-ing back to the host. Called from
// the runCheck goroutine (every successful check) and from runInstall
// (once on success). Both flows treat these writes as best-effort: a
// failure here never aborts the parent flow.

// systemInfoSnapshot is the in-memory shape collectSystemInfo produces
// before JSON-encoding it into agent_servers.system_info. Fields are all
// strings so the JSON shape is predictable for the frontend parser
// (missing / unreadable -> "").
type systemInfoSnapshot struct {
	OS            string   `json:"os"`
	Kernel        string   `json:"kernel"`
	Hostname      string   `json:"hostname"`
	CPUs          string   `json:"cpus"`
	MemTotal      string   `json:"mem_total"`
	DiskUsage     string   `json:"disk_usage"`
	IPs           []string `json:"ips"`
	Uptime        string   `json:"uptime"`
	ClaudeVersion string   `json:"claude_version"`
}

// collectSystemInfo runs a single SSH exec that gathers OS / kernel /
// hostname / CPU / mem / disk / IPs / uptime / claude_version. Each block
// is independent — a failure in one leaves that field empty rather than
// failing the whole collection. The Claude version probe is separate
// (and capped at 5s) so a slow network doesn't drag the snapshot out.
//
// Returns the JSON-encoded string ready for agent_servers.system_info +
// the parsed struct (the caller can also use the struct directly).
func (h *AgentServerHandler) collectSystemInfo(ctx context.Context, client *gossh.Client, job *store.Job) (string, *systemInfoSnapshot, error) {
	job.Append(store.LogLine{Type: "phase", Content: "🔍 系统盘点中..."})
	// 12s overall budget — most fields are local reads; the only network
	// round-trip is `ip` (no), and `cat /etc/os-release` which is tiny.
	// We keep headroom for slow VMs without holding the parent flow hostage.
	probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	snap := &systemInfoSnapshot{IPs: []string{}}

	// Single shell script keeps the SSH round-trips to 1 (vs. 10+). Each
	// section is wrapped in its own `if … fi` so a command failure (eg.
	// /etc/os-release missing on Alpine) leaves just that field empty.
	// `printf '__SECTION__=%s\n' "$value"` lets us split the combined
	// stdout back into named sections deterministically.
	script := `export LC_ALL=C PATH="$HOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"
if [ -s "$HOME/.nvm/nvm.sh" ]; then . "$HOME/.nvm/nvm.sh" 2>/dev/null; fi
printf '__UNAME__=%s\n' "$(uname -s 2>/dev/null || true)"
printf '__KERNEL__=%s\n' "$(uname -r 2>/dev/null || true)"
printf '__HOSTNAME__=%s\n' "$(hostname 2>/dev/null || true)"
printf '__CPUS__=%s\n' "$(nproc 2>/dev/null || echo '')"
printf '__MEM__=%s\n' "$(free -h 2>/dev/null | awk '/^Mem:/{print $2}' || echo '')"
printf '__DISK__=%s\n' "$(df -h / 2>/dev/null | awk 'NR==2{print $3"/"$2" ("$5")"}' || echo '')"
printf '__UPTIME__=%s\n' "$(uptime -p 2>/dev/null || uptime | sed -E 's/^[^,]+, +//; s/,.*//' || echo '')"
printf '__OS__=%s\n' "$(. /etc/os-release 2>/dev/null && echo "${PRETTY_NAME:-} (${VERSION_CODENAME:-})" || echo '')"
printf '__IPS__=%s\n' "$(ip -o -4 addr show 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | paste -sd, - || echo '')"
printf '__CLAUDE_VERSION__=%s\n' "$(claude --version 2>/dev/null | head -n1 || true)"`

	var out strings.Builder
	if _, err := client.Exec(probeCtx, script, "", nil, &out, nil); err != nil {
		job.Append(store.LogLine{Type: "message", Content: "⚠ 系统盘点命令执行失败: " + err.Error()})
		// Still return what we have — partial data is better than nothing.
	}

	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 4 || !strings.HasPrefix(line, "__") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key, val := line[:eq], line[eq+1:]
		switch key {
		case "__UNAME__":
			if snap.OS == "" && val != "" {
				snap.OS = val
			}
		case "__KERNEL__":
			snap.Kernel = val
		case "__HOSTNAME__":
			snap.Hostname = val
		case "__CPUS__":
			snap.CPUs = val
		case "__MEM__":
			snap.MemTotal = val
		case "__DISK__":
			snap.DiskUsage = val
		case "__UPTIME__":
			snap.Uptime = val
		case "__OS__":
			if val != "" {
				snap.OS = val
			}
		case "__IPS__":
			if val != "" {
				snap.IPs = strings.Split(val, ",")
			}
		case "__CLAUDE_VERSION__":
			snap.ClaudeVersion = val
		}
	}

	encoded, err := json.Marshal(snap)
	if err != nil {
		job.Append(store.LogLine{Type: "message", Content: "⚠ 系统盘点 JSON 序列化失败: " + err.Error()})
		return "", snap, err
	}

	// Surface a human-readable one-line summary so the install panel
	// shows progress without forcing the user to open the asset panel.
	summary := snap.OS
	if summary == "" {
		summary = snap.Hostname
	}
	if summary != "" {
		job.Append(store.LogLine{Type: "message", Content: "✓ 系统盘点完成（" + summary + "）"})
	} else {
		job.Append(store.LogLine{Type: "message", Content: "⚠ 系统盘点完成，但所有字段为空（agent 端命令可能受限）"})
	}
	return string(encoded), snap, nil
}

// captureInstallRuntimeFacts persists the resolved `claude` / `node`
// binary paths and the contents of ~/.novaworkbench/extra-paths into
// agent_servers (claude_bin / node_bin / extra_paths). Despite the
// name it's NOT install-only — runCheck also calls it so any successful
// Check backfills the columns for hosts that were installed before
// this code shipped (and so the asset panel reflects the current
// reality even when the operator never re-runs install).
//
// The on-disk ~/.novaworkbench/{extra-paths,node-bin} files remain the
// authoritative PATH source for the worker process — these DB columns
// mirror the same data for UI display + future per-server env injection.
//
// Failure here is best-effort: a DB write error doesn't abort the
// parent flow (Check / Install), just gets logged so an operator
// investigating a "why is claude_bin empty in the UI" question can see
// the underlying cause.
func (h *AgentServerHandler) captureInstallRuntimeFacts(ctx context.Context, client *gossh.Client, serverID string, job *store.Job) {
	// Re-run the same resolver the install script used — but read the
	// resolved paths back from the marker line instead of re-doing
	// `command -v` here. That keeps the DB column in lockstep with the
	// disk file the worker will actually use.
	//
	// PATH augmentation is non-optional: SSH non-interactive non-login
	// shells do NOT source ~/.bashrc, so nvm's ~/.nvm/versions/node/*/bin
	// (and Homebrew's /opt/homebrew/bin) are invisible to plain lookups.
	// Without this prefix `command -v claude` returns nothing on a fresh
	// nvm install — exactly the bug Tencent-SG002 hit: extra_paths got
	// /home/ubuntu/.nvm/.../bin (nvm source worked), but claude_bin /
	// node_bin were empty because the default PATH didn't include that
	// dir. Mirroring runCheck's dep-probe PATH augmentation (line ~232)
	// keeps captureInstallRuntimeFacts in lockstep with what Check sees.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out strings.Builder
	if _, err := client.Exec(probeCtx,
		`export PATH="$HOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:$PATH"; `+
			`if [ -s "$HOME/.nvm/nvm.sh" ]; then . "$HOME/.nvm/nvm.sh" 2>/dev/null; fi; `+
			`hash -r 2>/dev/null; `+
			`cat $HOME/.novaworkbench/extra-paths 2>/dev/null; `+
			`printf '__RUNTIME_BIN__\n'; `+
			`echo "[nova-agent] RUNTIME_BIN CLAUDE_BIN=$(command -v claude 2>/dev/null || true) NODE_BIN=$(command -v node 2>/dev/null || true)"`,
		"", nil, &out, nil); err != nil {
		job.Append(store.LogLine{Type: "message", Content: "⚠ 读取运行时路径失败: " + err.Error()})
		return
	}

	claudeBin, nodeBin, extraPaths := parseRuntimeFactsOutput(out.String())

	if err := h.svc.UpdateRuntime(serverID, claudeBin, nodeBin, extraPaths); err != nil {
		job.Append(store.LogLine{Type: "message", Content: "⚠ 持久化 runtime 失败: " + err.Error()})
		return
	}
	job.Append(store.LogLine{Type: "message", Content: fmt.Sprintf("✓ runtime 已持久化 (claude=%s node=%s extra_paths=%d 项)",
		shortPathForLog(claudeBin), shortPathForLog(nodeBin), strings.Count(extraPaths, "\n")+1)})
}

// parseRuntimeFactsOutput turns the combined `cat extra-paths ; sentinel ;
// RUNTIME_BIN marker` SSH output into (claudeBin, nodeBin, extraPaths).
// Extracted as a pure function so the parser is unit-testable without
// spinning up a fake SSH client.
//
// Format:
//   <lines from cat extra-paths, one PATH dir per line>
//   __RUNTIME_BIN__
//   [nova-agent] RUNTIME_BIN CLAUDE_BIN=<path> NODE_BIN=<path>
//
// Anything past the sentinel is ignored unless it carries the marker
// prefix; this keeps stray lines (echo noise, partial output from a
// flaky SSH session) from polluting extraPaths.
func parseRuntimeFactsOutput(out string) (claudeBin, nodeBin, extraPaths string) {
	var extraPathsLines []string
	sawSeparator := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if sawSeparator {
			if strings.HasPrefix(line, "[nova-agent] RUNTIME_BIN ") {
				rest := strings.TrimPrefix(line, "[nova-agent] RUNTIME_BIN ")
				for _, kv := range strings.Fields(rest) {
					eq := strings.IndexByte(kv, '=')
					if eq < 0 {
						continue
					}
					k, v := kv[:eq], kv[eq+1:]
					if k == "CLAUDE_BIN" {
						claudeBin = v
					} else if k == "NODE_BIN" {
						nodeBin = v
					}
				}
			}
			continue
		}
		if line == "__RUNTIME_BIN__" {
			sawSeparator = true
			continue
		}
		if line == "" {
			continue
		}
		// Anything before the sentinel is a line of extra-paths content
		// (one PATH dir per line, as written by linuxInstallScript /
		// darwinInstallScript). Pre-existing bug: a previous revision
		// used a switch with a `case line == "__RUNTIME_BIN__"` arm
		// that reset extraPathsLines = []string{}, which wiped the
		// already-appended previous line(s). The collapsed/skip-after
		// flag pattern here is the cleaner fix.
		extraPathsLines = append(extraPathsLines, line)
	}
	extraPaths = strings.Join(extraPathsLines, "\n")
	return
}

// shortPathForLog returns the basename of p for install-panel log lines,
// or "<未找到>" when p is empty (so the user immediately sees which
// binaries the install flow failed to resolve).
func shortPathForLog(p string) string {
	if p == "" {
		return "<未找到>"
	}
	if idx := strings.LastIndex(p, "/"); idx >= 0 {
		return p[idx+1:]
	}
	return p
}

// ---- worker launch PATH plumbing -----------------------------------------
//
// readExtraPaths / composeWorkerPATH / shellQuote are the helpers
// installNodeWorker + startWorkerIfDown use to inject the actual
// `claude` binary location into every worker-launch path
// (systemd --user Environment=PATH=..., launchd EnvironmentVariables.PATH=...,
// and the nohup env line). The directory list comes from
// $HOME/.novaworkbench/extra-paths, which linuxInstallScript /
// darwinInstallScript populate at install time by resolving
// `command -v claude` (with a fallback blind-scan of common npm prefix
// locations) — see those scripts for the populate-side rationale.

// readExtraPaths returns the list of directories written by the install
// scripts to $HOME/.novaworkbench/extra-paths. One directory per line;
// blank lines and lines starting with `#` are ignored. A missing or
// unreadable file yields an empty slice (not a hard error) so a partial
// install / non-Linux setup doesn't break the worker launch path.
//
// The SSH `homeDir` argument is used to build the path so a re-dialed
// non-root session (provisionNonRootUser) still finds the same file the
// root install session wrote.
func readExtraPaths(ctx context.Context, client *gossh.Client, homeDir string) []string {
	if homeDir == "" {
		homeDir = "/root"
	}
	path := homeDir + "/.novaworkbench/extra-paths"
	var out strings.Builder
	// exit!=0 (missing file) is fine — return empty list silently.
	_, _ = client.Exec(ctx, "cat "+path+" 2>/dev/null || true", "", nil, &out, nil)
	seen := map[string]bool{}
	dirs := []string{}
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "/") {
			continue // relative paths are noise; absolute required
		}
		if seen[line] {
			continue
		}
		seen[line] = true
		dirs = append(dirs, line)
	}
	return dirs
}

// composeWorkerPATH returns the colon-separated PATH value to give the
// worker process. Order: SSH session PATH (whatever the launcher can see
// right now, which usually already has nvm/brew), then install-supplied
// extra-paths (the directories that hold `claude`), then a safe fallback
// so systemd --user's bare PATH (`/usr/local/sbin:/usr/local/bin:/usr/bin:…`)
// is at least complete even when nothing else applies.
//
// We deliberately prepend rather than append: an SSH session PATH that
// already includes the right bin dir wins (e.g. on macOS where
// /opt/homebrew/bin is on every login shell's PATH), and the extras are
// there only as a safety net.
//
// On any failure the function still returns a usable PATH — never an
// empty one, since spawning `claude` with PATH="" guarantees ENOENT.
func composeWorkerPATH(ctx context.Context, client *gossh.Client, extra []string) string {
	const fallback = "/usr/local/bin:/usr/bin:/bin"
	var sessOut strings.Builder
	_, _ = client.Exec(ctx, "echo \"$PATH\"", "", nil, &sessOut, nil)
	sessPath := strings.TrimSpace(sessOut.String())
	if sessPath == "" {
		sessPath = fallback
	}

	merged := []string{}
	seen := map[string]bool{}
	add := func(d string) {
		if d == "" || seen[d] {
			return
		}
		seen[d] = true
		merged = append(merged, d)
	}
	// 1) extras first — they're the authoritative answer to "where did
	// install actually drop claude". Putting them at the front means the
	// spawn resolves to our binary even if some older install left a
	// different `claude` further down the SSH PATH.
	for _, d := range extra {
		add(d)
	}
	// 2) SSH session PATH (which has nvm / brew / apt paths).
	for _, d := range strings.Split(sessPath, ":") {
		add(d)
	}
	// 3) Safe fallback if both lists were empty.
	for _, d := range strings.Split(fallback, ":") {
		add(d)
	}
	return strings.Join(merged, ":")
}
