// wizard_remote.go: extracted from wizard.go as part of the refactoring.

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	gossh "github.com/novaworkbench/backend/internal/ssh"
	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

type remoteCodingInput struct {
	job            *store.Job
	serverID       string
	req            startCodingReq
	reqRow         *model.Requirement
	prompt         string
	workDir        string // local worktree path (used only for SFTP upload source)
	sourceSID      string
	fork           bool
	sessionArg     string
	forkSessionID  string
	model          string
	claudeConfigID string // role-bound claude config; empty = global active
	usage          *usageCtx
}

// startCodingReq mirrors the anonymous struct StartCoding decodes so the
// remote helper doesn't have to redefine field tags. Keeping this as a named
// type keeps runRemoteCoding self-documenting.
type startCodingReq struct {
	ProjectPath      string
	RequirementTitle string
	RequirementDesc  string
	RequirementID    string
	BranchName       string
	BaseBranch       string
	Model            string
	ReadKnowledge    bool
	AgentServerID    string
}

// RunRemoteCoding exposes runRemoteCoding as a func value for SubTaskRunner.
// main injects it via SubTaskRunner.SetRemoteCoding so every child dispatch
// (manual sub-task / orchestrated child / merge push+PR sub-task) can run on
// the Agent server the parent requirement was developed on without the runner
// depending on the wizard handler's full dependency set.
func (h *WizardHandler) RunRemoteCoding(in *remoteCodingInput) claudeStreamOutcome {
	return h.runRemoteCoding(in)
}

// runRemoteCoding is the Agent-server equivalent of the local runClaudeStream
// block in StartCoding. It opens an SSH session, ensures the project lives in
// a per-requirement git worktree under /tmp/nova-agent/<projectID>/<reqID>,
// uploads the local claude session dir so --resume picks up the right jsonl,
// runs the same claude CLI invocation, parses the stream-json output, then
// pushes the resulting commits back to origin and re-syncs the session dir
// back to local. Returns the same claudeStreamOutcome shape as the local
// path so the caller can reuse its terminal-state handling.
func (h *WizardHandler) runRemoteCoding(in *remoteCodingInput) claudeStreamOutcome {
	if h.agentSvrSvc == nil {
		return claudeStreamOutcome{errMsg: "Agent 服务器服务未初始化"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()

	// Load the (decrypted) credential before anything else — a missing master
	// key surfaces here as a clear error instead of a generic SSH failure.
	srv, plain, err := h.agentSvrSvc.GetWithCredential(in.serverID)
	if err != nil {
		return claudeStreamOutcome{errMsg: "无法读取 Agent 服务器凭据: " + err.Error()}
	}
	in.job.Append(store.LogLine{Type: "phase", Content: "🔌 连接到 Agent 服务器 " + srv.Name + " (" + srv.Host + ")"})

	client, err := gossh.Dial(ctx, srv.Host, srv.Port, srv.Username, srv.AuthType, plain)
	if err != nil {
		return claudeStreamOutcome{errMsg: "SSH 连接失败: " + err.Error()}
	}
	defer client.Close()

	// Step 2: code sync via git. baseRepo hosts a single origin clone for the
	// project; wtPath is the per-requirement worktree that mirrors the local
	// branch isolation model. Without a remote_url on the project the entire
	// remote path is dead — fail early with a clear message instead of an
	// opaque "git clone exit 128".
	if in.reqRow == nil {
		return claudeStreamOutcome{errMsg: "远程执行需要已保存的需求记录（缺 Requirement）"}
	}
	originURL, err := h.projectSvc.OriginURL(in.reqRow.ProjectID)
	if err != nil || originURL == "" {
		return claudeStreamOutcome{errMsg: "项目未配置 git 远程仓库，无法在 Agent 服务器执行。请先在项目设置中配置 origin。" + errString(err)}
	}
	baseRepo := "/tmp/nova-agent/" + in.reqRow.ProjectID + "/base"
	wtPath := "/tmp/nova-agent/" + in.reqRow.ProjectID + "/" + in.reqRow.ID
	branch := in.req.BranchName
	if branch == "" {
		branch = "requirement-" + in.reqRow.ID
	}
	baseBranch := in.req.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}

	in.job.Append(store.LogLine{Type: "phase", Content: "📥 准备 Agent 服务器代码（git worktree 隔离）..."})
	if !client.Exists(baseRepo) {
		in.job.Append(store.LogLine{Type: "message", Content: "📦 首次 clone " + redactOriginForLog(originURL)})
		if exit, _ := client.Exec(ctx, "git clone "+shellQuoteSingle(originURL)+" "+shellQuoteSingle(baseRepo), "", nil, &jobWriter{job: in.job}, nil); exit != 0 {
			return claudeStreamOutcome{errMsg: "git clone 失败（exit=" + fmtInt(exit) + "），请检查 origin 凭据"}
		}
	} else {
		client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git fetch origin --prune", "", nil, &jobWriter{job: in.job}, nil)
	}
	client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree prune", "", nil, &jobWriter{job: in.job}, nil)

	if !client.Exists(wtPath) {
		// Strategy 1: branch off HEAD (always valid; matches EnsureWorktree).
		// Strategy 2: off origin/<base> when strategy 1 fails. Strategy 3:
		// attach to an already-existing branch (adjust/continue reuse case).
		exit, _ := client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree add -b "+shellQuoteSingle(branch)+" "+shellQuoteSingle(wtPath), "", nil, &jobWriter{job: in.job}, nil)
		if exit != 0 {
			exit, _ = client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree add -b "+shellQuoteSingle(branch)+" "+shellQuoteSingle(wtPath)+" origin/"+shellQuoteSingle(baseBranch), "", nil, &jobWriter{job: in.job}, nil)
			if exit != 0 {
				if exit, _ = client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree add "+shellQuoteSingle(wtPath)+" "+shellQuoteSingle(branch), "", nil, &jobWriter{job: in.job}, nil); exit != 0 {
					return claudeStreamOutcome{errMsg: "git worktree 创建失败（exit=" + fmtInt(exit) + "），请检查仓库状态"}
				}
			}
		}
	} else {
		// adjust-coding / continue-coding: pull the latest remote commits
		// onto the existing branch. --ff-only protects against silent
		// divergence; on failure we log a hint and proceed with the local
		// copy (the user can resolve the divergence manually).
		client.Exec(ctx,
			"cd "+shellQuoteSingle(wtPath)+" && (git checkout "+shellQuoteSingle(branch)+" 2>/dev/null || true) && (git pull --ff-only origin "+shellQuoteSingle(branch)+" 2>&1 || echo \"[nova-agent] pull 跳过（无跟踪或已分叉）\")",
			"", nil, &jobWriter{job: in.job}, nil)
	}

	// Step 3: session sync (up) so the remote claude can --resume the same
	// session. We push the project-level ~/.claude/projects/<slug>/ contents
	// (the small set of jsonl files the CLI uses for session state). A missing
	// local dir is fine — the project has never been coded on before, and the
	// CLI on the remote will mint a brand-new session id.
	//
	// The remote slug is derived deterministically from the remote cwd
	// (wtPath, set up in step 1). Mapping local slug → remote slug lets the
	// remote claude find the jsonl files under the directory matching its
	// own cwd, which is what `--resume <session_id>` consults. Without this
	// mapping the local slug (-Users-f1-...-req_xxx) lands at the remote
	// projects root, while the remote CLI looks for sessions under its own
	// slug (-tmp-nova-agent-<projectID>-<reqID>), producing "No conversation
	// found" / "源会话已失效" on every run.
	remoteProjectsRoot := "~/.claude/projects/"
	remoteSlug := util.EncodeClaudeSlug(wtPath)
	remoteSlugDir := remoteProjectsRoot + remoteSlug
	in.job.Append(store.LogLine{Type: "phase", Content: "📤 同步 Claude 会话历史（SFTP 上行）..."})
	if slugDir, slugErr := h.claudeProjectsSlugDir(in.reqRow); slugErr == nil && slugDir != "" {
		client.Mkdirp(remoteSlugDir)
		if sftpErr := client.SyncDirUpMapped(slugDir, remoteSlugDir); sftpErr != nil {
			in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 会话上行失败（将无 resume 启动新会话）: " + sftpErr.Error()})
		}
	} else if slugErr != nil {
		in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 无法定位本地 claude session 目录（" + slugErr.Error() + "），将无 resume 启动新会话"})
	}

	// Step 4: build the worker POST body. The mapping (NovaWorkbench
	// StreamOpts → worker RunRequest) lives here so the wire format and the
	// CLI invocation shape stay in one place; the worker mirrors the field
	// names in its buildRunRequest helper and translates them to the
	// matching `claude` CLI flags.
	//
	// Auth precedence on the remote path: the platform's active
	// claude_configs row is the ONLY source of ANTHROPIC_AUTH_TOKEN /
	// ANTHROPIC_BASE_URL / model pinning. We pass it via `env` (built by
	// BuildRemoteEnvPairsWithConfig below) AND we set IgnoreLocalSettings=true
	// so the worker invokes claude with --setting-sources "" (load no settings
	// files at all — user / project / local). The agent host's
	// ~/.claude/settings.json — which the install script seeds with a
	// placeholder token — must not be able to shadow the platform config,
	// and any project / local settings left in the worktree by accident
	// shouldn't either. The worker folds the `env` map into its inline
	// --settings JSON (buildSettingsArg), so the remote launch carries the
	// pins exactly the way the local path does.
	ignoreLocal := true
	// The -p prompt built upstream (StartCoding / AdjustCoding /
	// ContinueCoding / tryAutoOrchestrate) bakes the LOCAL worktree path
	// into the persona header via agentDirectPrompt / developerDecomposePrompt:
	//
	//   "现在切换到「Agent 开发者」角色，正在执行需求（需求：<title>，工作目录：<workDir>）"
	//
	// On the local path that's correct — the agent is sitting in <workDir>.
	// On the remote path <workDir> is a /Users/f1/.novaworkbench/...
	// worktree that doesn't exist on the agent host (the remote cwd is
	// /tmp/nova-agent/<projectID>/<reqID>). Handing the original prompt
	// through verbatim confuses the agent's "先读取项目中的相关文件" step —
	// it tries to read a path that's not on its filesystem and either
	// errors or falls back to its own cwd, which defeats the "based on
	// workdir" intent the header expresses.
	//
	// Rewrite the persona header's workDir to the remote cwd before
	// posting to the worker. The substitution locates the "工作目录："
	// label in the persona header (a fixed string the prompt builders
	// always emit right before the path) and replaces from there through
	// the next "）" full-width closing paren. This way the requirement
	// title and any other tokens between the opening "（" and the label
	// are preserved verbatim — only the workDir segment is swapped.
	// Embedded file content from collectProjectContext is left untouched.
	// No-op when in.workDir is empty or doesn't appear in the prompt.
	remotePrompt := rewritePersonaWorkDir(in.prompt, in.workDir, wtPath)
	opts := llm.StreamOpts{
		Prompt:                 remotePrompt,
		WorkDir:                wtPath,
		SystemPrompt:           "",
		Model:                  cliModelArg(in.model),
		ClaudeConfigID:         in.claudeConfigID,
		SessionID:              in.sessionArg,
		Resume:                 in.sourceSID != "",
		Fork:                   in.fork,
		ForkSessionID:          in.forkSessionID,
		PermissionMode:         "",
		OverrideSettingSources: &ignoreLocal, // legacy flag, kept true
	}
	// Use BuildRemoteEnvPairs (NOT a plain env inheritance): the remote worker
	// spawns claude inside the agent host's own environment, so we must only
	// send the platform-pinned keys (auth token / base URL / model pins).
	// Inheriting os.Environ() of the NovaWorkbench host would leak macOS
	// HOME=/Users/... + TMPDIR=/var/folders/... into the Linux agent, making
	// `claude --print ping` hang and fail the preflight (preflight_timeout).
	// The pairs come from the SAME settingsEnvOverrides map the local path
	// serializes into its --settings JSON, so the two surfaces cannot drift.
	envPairs := h.llm.BuildRemoteEnvPairsWithConfig(opts.Model, in.claudeConfigID)
	runBody := workerRunRequest(opts, envPairs, in)

	// Step 5: POST to nova-agent-worker via SSH direct-tcpip channel. The
	// HTTPTransport opens one channel per request through the existing SSH
	// connection — no new TCP port on the network, and the worker is bound
	// to 127.0.0.1 on the remote host, so even on the remote side it's not
	// exposed. The response is a streaming NDJSON body (one JSON event per
	// line, same shape as the old `claude --output-format stream-json`
	// output) that the existing parseStreamJSONFromReader consumes directly.
	in.job.Append(store.LogLine{Type: "phase", Content: "🤖 Agent 服务器开始执行（nova-agent-worker）..."})

	workerAddr := "127.0.0.1:7000"
	httpClient := &http.Client{Transport: client.HTTPTransport(workerAddr)}

	// Pre-flight: GET /v1/health. The previous direct-CLI path had a
	// `command -v claude` probe that surfaced "ENOENT" up front; this is
	// its worker equivalent. A failed health probe is the most likely cause
	// of "stream interrupted, 0 events" right now (the worker is new and
	// a pre-existing Agent server without it would otherwise look like an
	// opaque failure).
	healthCtx, healthCancel := context.WithTimeout(ctx, 10*time.Second)
	healthReq, hReqErr := http.NewRequestWithContext(healthCtx, http.MethodGet, "http://"+workerAddr+"/v1/health", nil)
	if hReqErr != nil {
		healthCancel()
		return claudeStreamOutcome{errMsg: "构造健康检查请求失败: " + hReqErr.Error()}
	}
	healthResp, healthErr := httpClient.Do(healthReq)
	if healthErr != nil {
		healthCancel()
		return claudeStreamOutcome{errMsg: "无法连接 nova-agent-worker（" + workerAddr + "）。请在「设置 → Agent 服务器」对该服务器点「安装依赖」后再试。详细: " + healthErr.Error()}
	}
	healthResp.Body.Close()
	healthCancel()
	if healthResp.StatusCode != http.StatusOK {
		return claudeStreamOutcome{errMsg: fmt.Sprintf("nova-agent-worker 健康检查失败: HTTP %d", healthResp.StatusCode)}
	}

	// POST /v1/run with the JSON body. No overall http.Client timeout —
	// the per-request ctx carries the 35-minute coding deadline.
	bodyBytes, mErr := json.Marshal(runBody)
	if mErr != nil {
		return claudeStreamOutcome{errMsg: "序列化 worker 请求失败: " + mErr.Error()}
	}
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+workerAddr+"/v1/run", bytes.NewReader(bodyBytes))
	if reqErr != nil {
		return claudeStreamOutcome{errMsg: "构造 worker 请求失败: " + reqErr.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, doErr := httpClient.Do(req)
	if doErr != nil {
		return claudeStreamOutcome{errMsg: "POST /v1/run 失败: " + doErr.Error()}
	}
	defer resp.Body.Close()

	// Non-200 before the stream starts = worker rejected the request
	// outright (bad JSON, missing fields, SDK query() threw on startup).
	// Read the full body and surface it verbatim — usually a JSON message
	// with the actual reason.
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return claudeStreamOutcome{errMsg: fmt.Sprintf("worker 返回 HTTP %d: %s", resp.StatusCode, truncateStr(string(errBody), 600))}
	}

	out := parseStreamJSONFromReader(resp.Body, jobSink{in.job}, "start-coding", in.usage)

	// Step 6: session sync (down) — copy any new session jsonl the remote
	// run created back to local so adjust/continue on the next round find
	// it. Same forward-only semantics as Step 3, and routed via the same
	// remote-slug → local-slug mapping so the jsonl lands in the directory
	// whose slug matches the local cwd.
	in.job.Append(store.LogLine{Type: "phase", Content: "📥 同步会话结果回本地..."})
	if slugDir, slugErr := h.claudeProjectsSlugDir(in.reqRow); slugErr == nil && slugDir != "" {
		if sftpErr := client.SyncDirDownMapped(remoteSlugDir, slugDir); sftpErr != nil {
			in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 会话下行失败: " + sftpErr.Error()})
		}
	}

	// Step 7: git commit + push back to origin. Skip when the run errored out
	// (no real result) so we don't propagate half-broken state. The user can
	// always retry adjust-coding on the remote worktree via ContinueCoding.
	if out.errMsg == "" && out.finalResult != "" {
		in.job.Append(store.LogLine{Type: "phase", Content: "📤 推送代码变更到 origin..."})
		title := "nova-agent: " + in.req.RequirementTitle
		if title == "nova-agent: " {
			title = "nova-agent: " + in.reqRow.Title
		}
		// git commit -F - reads the message from stdin; we pipe via heredoc to
		// sidestep the SSH argv limit on long titles.
		commitScript := "cd " + shellQuoteSingle(wtPath) +
			" && git add -A" +
			" && git diff --cached --quiet || git commit -m " + shellQuoteSingle(title) +
			" && git push origin " + shellQuoteSingle(branch)
		if exit, _ := client.Exec(ctx, commitScript, "", nil, &jobWriter{job: in.job}, nil); exit != 0 {
			in.job.Append(store.LogLine{Type: "error", Content: "❌ 推送失败（exit=" + fmtInt(exit) + "），请在远程 worktree 手动处理冲突"})
			// Non-fatal: the user can still see the work locally via the
			// pushed-back session dir + the captured result text. Don't
			// override out.errMsg — let the run's own result stand.
		} else {
			in.job.Append(store.LogLine{Type: "message", Content: "✅ 已推送到 origin/" + branch})
		}
	}

	return out
}

// jobWriter adapts *store.Job to io.Writer so remote Exec output can land
// directly in the job's log (one message line per non-empty stdout/stderr
// chunk). Empty lines are dropped to avoid spamming the SSE panel.
type workerRunBody struct {
	WorkDir         string            `json:"workDir"`
	Prompt          string            `json:"prompt"`
	Model           string            `json:"model,omitempty"`
	SystemPrompt    string            `json:"systemPrompt,omitempty"`
	SessionID       string            `json:"sessionId,omitempty"`
	Resume          bool              `json:"resume,omitempty"`
	Fork            bool              `json:"fork,omitempty"`
	ForkSessionID   string            `json:"forkSessionId,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	AllowedTools    []string          `json:"allowedTools,omitempty"`
	DisallowedTools []string          `json:"disallowedTools,omitempty"`
	// ClaudeConfigID records which claude_configs row was used to source the
	// auth token + base URL carried in Env. The worker mirrors it into the
	// inline --settings JSON's "env" block as ANTHROPIC_MODEL so the CLI's
	// own env precedence can't be shadowed by a stale project / local
	// settings file. Empty when the global active config was used.
	ClaudeConfigID string `json:"claudeConfigId,omitempty"`
	// OverrideSettingSources is the legacy "drop the user source" flag —
	// when true, the worker invokes claude with --setting-sources project,local.
	// Kept for callers that still want project-level hooks etc.
	OverrideSettingSources bool `json:"overrideSettingSources,omitempty"`
	// IgnoreLocalSettings, when true, is the strict "drop EVERY settings
	// source" flag — the worker invokes claude with --setting-sources ""
	// so the platform's claude_configs row (delivered via `env`) is the
	// only source of ANTHROPIC_AUTH_TOKEN / ANTHROPIC_BASE_URL / model
	// pinning. The remote Agent-server path always sets this true: the
	// install script seeds ~/.claude/settings.json with a placeholder
	// token, and any stale value there would silently shadow the active row.
	IgnoreLocalSettings bool `json:"ignoreLocalSettings,omitempty"`
}

// workerRunRequest builds the POST body for /v1/run from the NovaWorkbench
// shape (llm.StreamOpts + remoteCodingInput). The env map is parsed from
// envPairs (each entry is "KEY=VALUE"); the worker hands this map to the
// claude subprocess's process env, so we don't need to strip the
// ANTHROPIC_* keys — the worker passes the map straight to the child.
//
// systemPrompt is intentionally left empty for the wizard remote path: the
// developer's persona is passed in the prompt itself (the -p payload
// includes the role system prompt as a preamble), matching the previous
// CLI invocation's behavior. If a future caller wants to pass it via
// --system-prompt, set opts.SystemPrompt before this is called.
func workerRunRequest(opts llm.StreamOpts, envPairs []string, in *remoteCodingInput) workerRunBody {
	envMap := make(map[string]string, len(envPairs))
	for _, kv := range envPairs {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		envMap[kv[:eq]] = kv[eq+1:]
	}
	override := false
	if opts.OverrideSettingSources != nil {
		override = *opts.OverrideSettingSources
	}
	return workerRunBody{
		WorkDir:       opts.WorkDir,
		Prompt:        opts.Prompt,
		Model:         opts.Model,
		SessionID:     opts.SessionID,
		Resume:        opts.Resume,
		Fork:          opts.Fork,
		ForkSessionID: opts.ForkSessionID,
		Env:           envMap,
		// Pass the config id so the worker can mirror ANTHROPIC_MODEL into
		// the inline --settings JSON (the worker's buildSettingsArg already
		// does this for opts.model; claudeConfigId is informational today but
		// keeps the wire format ready for future per-config settings tweaks).
		ClaudeConfigID:         opts.ClaudeConfigID,
		OverrideSettingSources: override,
		// Always drop local settings on the remote Agent-server path. The
		// wizard remote-coding call site passes OverrideSettingSources=true
		// (kept for backward compat with the legacy "drop user source"
		// semantics) and now also gets IgnoreLocalSettings=true so the
		// worker invokes claude with --setting-sources "" (load NO
		// settings files). The platform env passed via `env` above is the
		// sole source of auth / base URL / model pinning.
		IgnoreLocalSettings: true,
	}
}

// shellQuoteSingle mirrors the local ssh client's quoting: single-quoted
// strings with embedded single quotes escaped via close-quote / escape /
// open-quote. Empty strings become ”.
func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// redactOriginForLog strips userinfo (the embedded token) from the origin
// URL before showing it in the job log — same helper exists in
// service/project.go but we keep a local copy so the handler doesn't have to
// grow its dependency surface.
func redactOriginForLog(raw string) string {
	if i := strings.Index(raw, "@"); i > 0 {
		if j := strings.Index(raw[:i], "://"); j > 0 {
			return raw[:j+3] + "<redacted>@" + raw[i+1:]
		}
	}
	return raw
}

// fmtInt returns the decimal string for an int (kept as a tiny shim so
// the failure-path error messages read naturally without pulling in fmt
// solely for Sprintf("%d", x)).
func fmtInt(n int) string { return fmt.Sprintf("%d", n) }

// errString returns err.Error() or "" when err is nil.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return " (" + err.Error() + ")"
}

// claudeProjectsSlugDir locates the on-disk directory where claude stores
// session jsonls for the given requirement's project. The slug is derived
// from the project's local_path; we read the cached value on the project
// row when available, otherwise fall back to scanning the parent dir for
// a matching basename.
//
// The previous implementation always returned the first subdir of the
// projects root — fine for a single-project setup but wrong when multiple
// projects share ~/.claude/projects/. This implementation is precise: the
// cached claude_project_slug (or the freshly-discovered one) is used
// verbatim so the Agent Server sync path can map it to the remote slug.
func (h *WizardHandler) claudeProjectsSlugDir(reqRow *model.Requirement) (string, error) {
	if reqRow == nil {
		return "", fmt.Errorf("no requirement")
	}
	if h.projectSvc == nil {
		return "", fmt.Errorf("projectSvc not wired")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	claudeHome := os.Getenv("NOVA_CLAUDE_HOME")
	if claudeHome == "" {
		claudeHome = filepath.Join(home, ".novaworkbench", "claude")
	}
	root := filepath.Join(claudeHome, "projects")
	proj, err := h.projectSvc.Get(reqRow.ProjectID)
	if err != nil || proj == nil {
		// Project row missing (e.g. soft-deleted) — degrade to the
		// legacy "first subdir" behaviour so a misconfigured caller
		// still gets a best-effort path rather than nothing. The Agent
		// Server sync is best-effort anyway.
		entries, rerr := os.ReadDir(root)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				return "", nil
			}
			return "", rerr
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			return filepath.Join(root, e.Name()), nil
		}
		return "", nil
	}
	// 1) Cache hit — use the persisted slug verbatim.
	if proj.ClaudeProjectSlug != "" {
		if _, statErr := os.Stat(filepath.Join(root, proj.ClaudeProjectSlug)); statErr == nil {
			return filepath.Join(root, proj.ClaudeProjectSlug), nil
		}
		// Stale slug (project moved / dir deleted). Fall through to
		// re-discovery so we don't keep returning a dead path.
	}
	// 2) Cache miss / stale — scan and persist.
	slug, derr := h.projectSvc.DiscoverAndCacheClaudeProjectSlug(proj.ID, proj.LocalPath)
	if derr != nil || slug == "" {
		return "", derr
	}
	return filepath.Join(root, slug), nil
}

// parseStreamJSONFromReader is the io.Reader-only counterpart of
// runClaudeStream. It scans NDJSON events off the supplied reader and emits
// the same LogLine shape the local path uses (phase / tool_call / message /
// usage / knowledge_result). It does NOT own a subprocess; the caller is
// responsible for piping the remote claude's stdout into r and closing it
// after the remote command exits.
//
// All heavy lifting (event dispatch, model pinning, token recording, usage
// persistence) is mirrored from runClaudeStream so the resulting
// claudeStreamOutcome is interchangeable. The differences are:
//   - no process group / killProcessGroup — the remote shell is the parent's
//     equivalent and we don't have access to its pgid over SSH
//   - no stall watchdog — a stuck remote claude is killed by closing the
//     SSH session from the caller's defer (client.Close kills the channel)
//   - no stderr fallback for staleness detection (we never see the remote
//     stderr in this scope)
func rewritePersonaWorkDir(prompt, localWorkDir, remoteWorkDir string) string {
	if prompt == "" || localWorkDir == "" || localWorkDir == remoteWorkDir {
		return prompt
	}
	const label = "工作目录："
	const closeParen = "）"
	idx := strings.Index(prompt, label)
	if idx < 0 {
		return prompt
	}
	// Find the closing paren after the label. If absent, bail out
	// and leave the prompt alone — the label showed up but the
	// header structure we expect wasn't there.
	end := strings.Index(prompt[idx+len(label):], closeParen)
	if end < 0 {
		return prompt
	}
	end += idx + len(label)
	// Verify the slice between the label and the closing paren
	// actually equals localWorkDir. If it doesn't match (e.g. the
	// label appears in some unrelated text), return the prompt
	// unchanged rather than corrupting it.
	between := prompt[idx+len(label) : end]
	if between != localWorkDir {
		return prompt
	}
	return prompt[:idx+len(label)] + remoteWorkDir + prompt[end:]
}
// finalResult and verifies the [SUBTASKS_READY] sentinel is present. Returns
// nil when either is missing — caller treats that as "main agent answered a
// normal question, not a decompose request" and just renders the chat reply.
//
// We intentionally do NOT use the existing extractJSON() brace matcher
// because the agent typically wraps the JSON in a ```json fence; this
// function locates the first ```json block, then JSON-decodes its contents.
// Falls back to a brace match when no fence is found (more permissive, lets
// the agent omit the fence in low-token responses).
