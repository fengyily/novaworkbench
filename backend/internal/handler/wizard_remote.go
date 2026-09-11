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

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	gossh "github.com/novaworkbench/backend/internal/ssh"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

// ---- Remote Agent-server execution -----------------------------------------

// remoteCodingInput is the bag of pre-computed values the StartCoding
// goroutine hands to runRemoteCoding. Pulling these into a struct keeps the
// signature readable and forces callers to acknowledge the same dependencies
// the local branch already resolved (prompt, workDir, session threading).
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
	// FreshSession skips the --resume path entirely: when true, no
	// `--resume <sourceSID>` flag is sent to the worker (a brand-new
	// session id is still minted and passed as `--session-id` so the
	// outcome records are coherent), Step 3 SFTP upload is skipped
	// (nothing to resume → no jsonl to push), and the wizard_remote
	// caller's --fork-session flag is also dropped (a pure "new
	// session" run shouldn't fork anything). The runner injects a
	// ## 父任务上下文 block at the top of the prompt so the new session
	// still knows the requirement title / design / parent context.
	FreshSession bool
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
	// baseBranch fallback chain: UI-provided BaseBranch > project.DefaultBranch
	// > literal "main". Mirrors the local execStartCoding chain so the remote
	// worktree is rooted at the same base the local wizard uses — a divergence
	// here would mean adjust-coding landed on a different line of history than
	// what continue-coding (or merge) sees.
	baseBranch := in.req.BaseBranch
	if baseBranch == "" {
		if proj, perr := h.projectSvc.Get(in.reqRow.ProjectID); perr == nil && proj != nil && proj.DefaultBranch != "" {
			baseBranch = proj.DefaultBranch
		}
		if baseBranch == "" {
			baseBranch = "main"
		}
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
	// Always (re)fetch the project's main branch into origin/<baseBranch> so
	// the worktree strategies below branch off the latest upstream — without
	// this line the first-ever clone would skip the `git fetch origin --prune`
	// branch and leave origin/<baseBranch> stale, defeating the "based on the
	// freshest main" intent. Best-effort by design: any failure is logged but
	// does not abort the run (the worktree fallback strategies cover a missing
	// origin/<baseBranch>).
	client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git fetch origin "+shellQuoteSingle(baseBranch), "", nil, &jobWriter{job: in.job}, nil)
	client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree prune", "", nil, &jobWriter{job: in.job}, nil)

	if !client.Exists(wtPath) {
		// Strategy 1: branch off origin/<baseBranch> (the freshly-updated ref
		// from the fetch above) so the new requirement starts from the
		// upstream HEAD. Matches the local EnsureWorktreeLogged strategy
		// order. Strategies 2/3 remain as fallbacks for repos where
		// origin/<base> doesn't exist (no remote on base, base never pushed).
		exit, _ := client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree add -b "+shellQuoteSingle(branch)+" "+shellQuoteSingle(wtPath)+" origin/"+shellQuoteSingle(baseBranch), "", nil, &jobWriter{job: in.job}, nil)
		if exit != 0 {
			// Strategy 2: branch off HEAD (always valid; matches the legacy
			// EnsureWorktree behaviour).
			exit, _ = client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree add -b "+shellQuoteSingle(branch)+" "+shellQuoteSingle(wtPath), "", nil, &jobWriter{job: in.job}, nil)
			if exit != 0 {
				// Strategy 3: attach to an already-existing branch
				// (adjust/continue reuse case).
				if exit, _ = client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree add "+shellQuoteSingle(wtPath)+" "+shellQuoteSingle(branch), "", nil, &jobWriter{job: in.job}, nil); exit != 0 {
					return claudeStreamOutcome{errMsg: "git worktree 创建失败（exit=" + fmtInt(exit) + "），请检查仓库状态"}
				}
			}
		}
	} else {
		// adjust-coding / continue-coding: pull the latest remote commits
		// onto the existing branch. --ff-only protects against silent
		// divergence; on failure we log a hint and proceed with the local
		// copy (the user can resolve the divergence manually). The chained
		// merge --ff-only origin/<baseBranch> step then pulls in any
		// upstream commits on the project main that landed since this
		// branch was first created — mirroring the local EnsureWorktree
		// reuse path. The trailing `|| echo ...` keeps the whole pipeline
		// non-blocking: a diverged local branch (or missing origin ref)
		// just logs a hint and lets coding continue from the existing
		// commit, exactly like the pull step above.
		client.Exec(ctx,
			"cd "+shellQuoteSingle(wtPath)+" && (git checkout "+shellQuoteSingle(branch)+" 2>/dev/null || true) && (git pull --ff-only origin "+shellQuoteSingle(branch)+" 2>&1 || echo \"[nova-agent] pull 跳过（无跟踪或已分叉）\") && (git merge --ff-only origin/"+shellQuoteSingle(baseBranch)+" 2>&1 || echo \"[nova-agent] 主分支快进更新跳过\")",
			"", nil, &jobWriter{job: in.job}, nil)
	}

	// Step 2.5: configure git identity + (optionally) GPG signing in the
	// remote worktree. This must run BEFORE Step 3 (session sync) and
	// before any `git commit` — Claude's Bash-tool commits inside Step 5
	// inherit the worktree-level config and will go through the same
	// `gpg.program` wrapper as Nova's Step 7 commit. Signing is driven by
	// repo config (commit.gpgsign=true + user.signingkey + gpg.program)
	// rather than per-invocation `-S`, because `-S` would only catch
	// commits we explicitly make — it would not catch Claude's own
	// commits, which is exactly the failure mode this requirement
	// addresses.
	//
	// Failure policy: GPG enabled + key material present + provision
	// fails → abort the whole run (return errMsg). Returning an unsigned
	// commit under "Require signed commits" branch protection would be
	// strictly worse than failing. GPG disabled → write identity only;
	// failure there is a warning (we already have the existing
	// GitHub-side fallback for an unsigned push).
	gitName, gitEmail := lookupGitIdentity(h.projectSvc, h.platformSvc, in.reqRow)
	gnupgHome := "/tmp/nova-agent/" + in.reqRow.ProjectID + "/" + in.reqRow.ID + ".gnupg"
	if project, pErr := h.projectSvc.Get(in.reqRow.ProjectID); pErr == nil && project != nil && project.PlatformTokenID != "" {
		enabled, _, armored, passphrase, gpgErr := h.platformSvc.GPGSigningMaterial(project.PlatformTokenID)
		switch {
		case gpgErr != nil:
			in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 读取 GPG 配置失败：" + gpgErr.Error() + "，按未启用处理"})
		case enabled && armored != "":
			in.job.Append(store.LogLine{Type: "phase", Content: "🔐 在 Agent 服务器上配置 GPG 签名..."})
			keyID, cleanup, provErr := provisionRemoteGPG(ctx, client, in.job, gnupgHome, wtPath, baseRepo, armored, passphrase, gitName, gitEmail)
			if provErr != nil {
				// Best-effort cleanup before we bail.
				if cleanup != nil {
					cleanup()
				}
				return claudeStreamOutcome{errMsg: provErr.Error()}
			}
			defer cleanup()
			// ctx.Done() fallback cleanup. The run can outlive the
			// ctx (job goroutine continues after the handler returns)
			// but cleanup itself is bounded: it issues a single SSH
			// exec and returns. If the SSH conn is already torn down
			// by then, cleanup silently fails and logs a warning.
			go func() {
				<-ctx.Done()
				cleanup()
			}()
			in.job.Append(store.LogLine{Type: "message", Content: "✅ GPG 已就绪（keyid=" + keyID + "）"})
			// Back-fill the keyid so the UI shows it on the next list
			// reload. Ignore errors — the key is already usable on the
			// remote host; a stale empty keyid is a UI-only nit.
			_ = h.platformSvc.UpdateGPGKeyID(project.PlatformTokenID, keyID)
		case enabled:
			// Enabled but no key material — the user toggled the
			// checkbox without uploading a private key. Continue
			// without signing and warn loudly so the resulting
			// unsigned push (if any) doesn't come as a surprise.
			in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 已启用 GPG 签名但未保存私钥，本次提交未签名"})
		}
	}
	// GPG-disabled case: still bake the committer identity into the
	// worktree. The remote path used to skip identity injection entirely
	// (only the local path injected GIT_AUTHOR_*); on hosts without a
	// global ~/.gitconfig (e.g. minimal Docker) the resulting commits
	// would have an empty committer and GitHub would reject the push.
	if idErr := provisionRemoteGitIdentity(ctx, client, in.job, wtPath, baseRepo, gitName, gitEmail); idErr != nil {
		in.job.Append(store.LogLine{Type: "message", Content: "⚠️ " + idErr.Error()})
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
	//
	// FreshSession skips this whole step — there's nothing to resume, no
	// jsonl to push. Step 4 below also drops --resume / --fork-session.
	remoteProjectsRoot := "~/.claude/projects/"
	remoteSlug := util.EncodeClaudeSlug(wtPath)
	remoteSlugDir := remoteProjectsRoot + remoteSlug
	var sessionMissingSide string
	if in.FreshSession {
		in.job.Append(store.LogLine{Type: "phase", Content: "🆕 跳过会话上行：本次走「新会话」模式（不续接父会话）"})
	} else {
		in.job.Append(store.LogLine{Type: "phase", Content: "📤 同步 Claude 会话历史（SFTP 上行）..."})
		slugDir, slugErr := h.claudeProjectsSlugDir(in.reqRow)
		switch {
		case slugErr != nil:
			// "local" side: we could not locate the local session dir at
			// all. Surface a precise message — most often this is the
			// known mismatch where the local CLI wrote to ~/.claude/ but
			// the backend looked under ~/.novaworkbench/claude/ (see
			// claudeSessionHome()).
			in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 无法定位本地 claude session 目录（" + slugErr.Error() + "），将无 resume 启动新会话"})
			sessionMissingSide = "local"
		case slugDir == "":
			// Both branches that USED to return ("", nil) silently. Now
			// we treat that as "local missing" so the user gets a
			// targeted hint instead of an opaque failure downstream.
			in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 本地未找到匹配的 claude session 目录（project slug 未缓存），将无 resume 启动新会话"})
			sessionMissingSide = "local"
		default:
			client.Mkdirp(remoteSlugDir)
			if sftpErr := client.SyncDirUpMapped(slugDir, remoteSlugDir); sftpErr != nil {
				// "sync-failed" side: SFTP itself errored. The remote
				// CLI will look for a jsonl we never landed.
				in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 会话上行失败（将无 resume 启动新会话）: " + sftpErr.Error()})
				sessionMissingSide = "sync-failed"
			} else if in.sourceSID != "" {
				// Pre-flight: confirm the jsonl actually landed under
				// the remote slug dir. Catches the "0 files uploaded
				// because slug mismatch is silent" case that
				// historically surfaced as a generic "No conversation
				// found" downstream.
				remoteSidPath := remoteSlugDir + "/" + in.sourceSID + ".jsonl"
				exists, statErr := client.RemoteFileExists(remoteSidPath)
				if statErr != nil {
					in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 远端 session 文件探测失败（" + statErr.Error() + "），将按「sync-failed」分类"})
					sessionMissingSide = "sync-failed"
				} else if !exists {
					in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 远端未找到 session 文件 " + remoteSidPath + "（上行 0 文件或 slug 不匹配），将按「remote」分类"})
					sessionMissingSide = "remote"
				} else {
					in.job.Append(store.LogLine{Type: "message", Content: "✅ 远端 session 文件就绪: " + remoteSidPath})
				}
			}
		}
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
	// FreshSession: skip --resume / --fork-session entirely. The worker
	// still gets --session-id <newSID> (so the resulting JSONL is keyed
	// correctly for downstream resume), but no parent JSONL is consulted
	// at startup. Combined with Step 3 skipping SFTP, this is the "新会话
	// （含需求上下文）" recovery path for the source-session-missing bug.
	resume := in.sourceSID != ""
	fork := in.fork
	if in.FreshSession {
		resume = false
		fork = false
	}
	opts := llm.StreamOpts{
		Prompt:                 remotePrompt,
		WorkDir:                wtPath,
		SystemPrompt:           "",
		Model:                  cliModelArg(in.model),
		ClaudeConfigID:         in.claudeConfigID,
		SessionID:              in.sessionArg,
		Resume:                 resume,
		Fork:                   fork,
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
	// Stamp the pre-flight session-missing classification onto the
	// outcome so finishSubTask / ExecuteOrchestratedChild can pick the
	// right artifact text. Only meaningful when staleSession is true
	// (i.e. the remote CLI actually returned a "No conversation found"
	// error). On the happy path it stays empty and is ignored.
	out.SessionFileMissingSide = sessionMissingSide

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
	//
	// No `-S` here on purpose: signing is driven by the worktree config set
	// up in Step 2.5 (commit.gpgsign=true + user.signingkey + gpg.program).
	// Doing it via config — instead of per-invocation `-S` — also covers
	// any `git commit` Claude issues from its own Bash tool during Step 5,
	// which is the exact failure mode this requirement addresses.
	if out.errMsg == "" && out.finalResult != "" {
		in.job.Append(store.LogLine{Type: "phase", Content: "📤 推送代码变更到 origin..."})
		var pushStderr bytes.Buffer
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
		if exit, _ := client.Exec(ctx, commitScript, "", nil, &jobWriter{job: in.job}, &pushStderr); exit != 0 {
			// GPG signing failures show up here with very specific
			// stderr patterns (wrong passphrase, expired key, GH006,
			// …) — classify them into a precise Chinese message so
			// the user knows whether to fix the GPG token, rotate
			// the key, or re-check the branch protection. Falls
			// back to the generic wording when stderr is empty or
			// doesn't match any bucket (e.g. plain merge conflict
			// on push).
			msg := classifyGitSignFailure(pushStderr.String())
			if msg == "" {
				msg = "❌ 推送失败（exit=" + fmtInt(exit) + "），请在远程 worktree 手动处理冲突"
			}
			in.job.Append(store.LogLine{Type: "error", Content: msg})
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
type jobWriter struct{ job *store.Job }

func (w *jobWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(string(p), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		w.job.Append(store.LogLine{Type: "message", Content: line})
	}
	return len(p), nil
}

// workerRunBody is the JSON body sent to nova-agent-worker's POST /v1/run.
// It mirrors the worker's buildRunRequest helper (see agent-worker/server.mjs):
// every field here corresponds to one CLI flag the worker assembles. Keeping
// the shape on the Go side means wire-format changes require only one update.
//
// Why a typed struct (instead of `map[string]any`): the worker validates
// required fields at startup, and unknown fields would be silently dropped.
// A struct makes typos like `OverrideSettingSoures` a compile error rather
// than a "worker returns 400 with no detail" at runtime.
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

// claudeSessionHome resolves the directory Claude CLI uses as its config
// home (where the projects/ subdir with session jsonls lives). Priority:
//
//  1. CLAUDE_CONFIG_DIR — what the local `claude` subprocess writes to when
//     we (or the docker-entrypoint script) have asked it to use a non-default
//     location. Highest priority because the CLI's actual write path is what
//     matters; the upload below must read from the same dir the CLI writes to.
//  2. NOVA_CLAUDE_HOME — historical NovaWorkbench override. Kept for
//     backward compatibility with deployments where CLAUDE_CONFIG_DIR is not
//     set (legacy env contract).
//  3. ~/.claude — the Claude CLI's built-in default. This is what the local
//     CLI uses today, since the backend has never set either env var.
//
// Returning an absolute path keeps the JSONL upload (Step 3 of
// runRemoteCoding) pointing at the dir the CLI actually wrote to — without
// this alignment, SFTP SyncDirUpMapped silently uploads 0 files and the
// remote `claude --resume <sid>` immediately fails with "No conversation
// found" / "源会话已失效" (the bug this helper was extracted to fix).
func claudeSessionHome() string {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("NOVA_CLAUDE_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
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
	root := filepath.Join(claudeSessionHome(), "projects")
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
	// Prefer the requirement's worktree path when present: coding actually
	// runs with cwd == worktree_path (a per-requirement git worktree under
	// ~/.novaworkbench/worktrees/<basename>/<reqID>), and the Claude CLI
	// derives its slug from THAT cwd, not from the project root. Without
	// this preference the wizard's project-local slug (root) and the CLI's
	// actual slug (worktree) diverge and SyncDirUpMapped uploads 0 files.
	probePaths := []string{proj.LocalPath}
	if reqRow.WorktreePath != "" {
		probePaths = append([]string{reqRow.WorktreePath}, probePaths...)
	}
	// 1) Cache hit — use the persisted slug verbatim.
	if proj.ClaudeProjectSlug != "" {
		if _, statErr := os.Stat(filepath.Join(root, proj.ClaudeProjectSlug)); statErr == nil {
			return filepath.Join(root, proj.ClaudeProjectSlug), nil
		}
		// Stale slug (project moved / dir deleted). Fall through to
		// re-discovery so we don't keep returning a dead path.
	}
	// 2) Cache miss / stale — scan and persist. We probe each candidate
	// path in priority order (worktree > project root) so the discovered
	// slug matches the cwd the CLI actually used.
	var lastErr error
	for _, p := range probePaths {
		slug, derr := h.projectSvc.DiscoverAndCacheClaudeProjectSlug(proj.ID, p)
		if derr != nil {
			lastErr = derr
			continue
		}
		if slug != "" {
			return filepath.Join(root, slug), nil
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", nil
}