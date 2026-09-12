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
	"github.com/novaworkbench/backend/internal/service"
	gossh "github.com/novaworkbench/backend/internal/ssh"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

// ---- Remote Agent-server execution -----------------------------------------

// remoteCodingInput is the bag of pre-computed values the StartCoding
// goroutine hands to runRemoteCoding. Pulling these into a struct keeps the
// signature readable and forces callers to acknowledge the same dependencies
// the local branch already resolved (prompt, workDir, session threading).
//
// Dev-stage retains this struct because it carries dev-specific fields
// (startCodingReq + FreshSession) that the architect stage does not need.
// The shared SSH / worktree / session-sync / worker-call trunk lives in
// prepareRemoteAgentRun and is fed by remoteRunInput (below).
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

// remoteRunInput is the SHARED parameter struct consumed by
// prepareRemoteAgentRun. It owns every field the Agent-server side of a
// Claude CLI task needs up to and including the worker invocation + stream
// parse — SSH dial, git worktree / base repo / branch strategy, session
// jsonl up-sync, workerRunBody construction, env pinning, health probe,
// POST /v1/run, NDJSON parse, session jsonl down-sync. Callers (dev coding,
// architect design) diverge only AFTER this helper returns — commit + push
// for coding, design_docs write for architect.
//
// Field shape is a subset of remoteCodingInput minus dev-only fields
// (startCodingReq, FreshSession). PermissionMode is the new one: the
// architect stage runs in plan mode (Claude is read-only + writes its plan
// to ~/.claude/plans/<slug>.md); the dev stage leaves it empty so the
// worker falls back to --dangerously-skip-permissions.
type remoteRunInput struct {
	job            *store.Job
	serverID       string
	reqRow         *model.Requirement
	prompt         string
	workDir        string // local worktree path (used only for SFTP upload source)
	sourceSID      string
	fork           bool
	sessionArg     string
	forkSessionID  string
	model          string
	claudeConfigID string
	usage          *usageCtx
	// PermissionMode is forwarded to the worker (see workerRunBody).
	// Empty = worker uses --dangerously-skip-permissions (dev default).
	// "plan" = worker uses --permission-mode plan (architect default).
	PermissionMode string
	// branch is the feature-branch name the helper should check out /
	// create on the remote worktree. Only meaningful for dev; architect
	// sets it to "" since the plan-mode run does not need a branch and
	// git operations on the remote worktree are reduced to the
	// worktree creation step.
	branch string
	// baseBranch lets the architect caller pin the base branch for
	// git worktree creation; empty falls back to project.DefaultBranch
	// then "main". Dev callers already resolve this on their side and
	// pre-populate it.
	baseBranch string
}

// remoteArchitectInput is the architect-stage wrapper around remoteRunInput.
// Defined as a thin named type so future architect-specific knobs (a
// post-run design_docs write helper, plan-mode-only logs, etc.) have a place
// to land without growing the shared struct.
type remoteArchitectInput struct {
	*remoteRunInput
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

	// Step 2: code sync. baseRepo / wtPath give the per-requirement worktree
	// that mirrors the local branch-isolation model; HOW code gets into that
	// worktree (and back out) is delegated to a codeTransport so the two
	// strategies — origin clone/push vs local git-bundle over SFTP — don't
	// clutter this trunk. See originTransport / bundleTransport below.
	if in.reqRow == nil {
		return claudeStreamOutcome{errMsg: "远程执行需要已保存的需求记录（缺 Requirement）"}
	}
	// Local-sync mode (self-hosted repo with no reachable remote) is decided by
	// the persisted sync_mode and completely bypasses the OriginURL check — the
	// bundle transport never touches origin.
	localSync := in.reqRow.SyncMode == service.SyncModeLocal
	var tr codeTransport = originTransport{}
	if !localSync {
		originURL, oerr := h.projectSvc.OriginURL(in.reqRow.ProjectID)
		if oerr != nil || originURL == "" {
			return claudeStreamOutcome{errMsg: "项目未配置 git 远程仓库，无法在 Agent 服务器执行。请先在项目设置中配置 origin，或在启动开发时选择「本地仓库同步」。" + errString(oerr)}
		}
		tr = originTransport{originURL: originURL}
	}
	baseRepo := "/tmp/nova-agent/" + in.reqRow.ProjectID + "/base"
	wtPath := "/tmp/nova-agent/" + in.reqRow.ProjectID + "/" + in.reqRow.ID
	branch := in.req.BranchName
	if branch == "" {
		branch = "requirement-" + in.reqRow.ID
	}
	// baseBranch fallback chain: UI-provided BaseBranch > project.DefaultBranch
	// > literal "main". Mirrors the local execStartCoding chain so the remote
	// worktree is rooted at the same base the local wizard uses. The project
	// lookup here also resolves LocalPath for the bundle transport (the local
	// repo whose object store receives the down-bundle commits).
	baseBranch := in.req.BaseBranch
	localRepoPath := ""
	if proj, perr := h.projectSvc.Get(in.reqRow.ProjectID); perr == nil && proj != nil {
		if baseBranch == "" && proj.DefaultBranch != "" {
			baseBranch = proj.DefaultBranch
		}
		localRepoPath = proj.LocalPath
	}
	if baseBranch == "" {
		baseBranch = "main"
	}
	if localSync {
		// localWt: this run's isolated worktree. StartCoding passes in.workDir;
		// adjust/continue/sub-task leave it empty → fall back to the persisted
		// worktree_path. localRepo is always the project's main checkout.
		localWt := in.workDir
		if localWt == "" {
			localWt = in.reqRow.WorktreePath
		}
		tr = bundleTransport{localRepo: localRepoPath, localWt: localWt}
	}

	if perr := tr.PrepareRemote(ctx, client, in, baseRepo, wtPath, branch, baseBranch); perr != nil {
		return claudeStreamOutcome{errMsg: perr.Error()}
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
	// Local-sync mode skips the remote commit.gpgsign provision: the remote
	// commits are a transport detail landed back into the LOCAL object store,
	// and the user-visible integration commit is signed locally by
	// LocalMerge's resolveLocalGPGSigning. The committer identity below still
	// runs unconditionally so the remote commits carry a real author.
	if !localSync {
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
	if in.FreshSession || in.sourceSID == "" {
		// Nothing to --resume → nothing worth pushing. This covers explicit
		// FreshSession runs AND every sourceSID=="" case: "基于方案开发"
		// (dev_mode==design deliberately drops the session chain), skip-design
		// "直接开发" rows, and legacy rows with no recorded session ids. In all
		// of these the remote worker starts a brand-new session keyed by
		// --session-id, so uploading the project's other session jsonl is
		// useless on the remote (the worker only reads the resumed <sid>.jsonl).
		// Mirrors the same guard in prepareRemoteAgentRun (Step 3).
		reason := "本次走「新会话」模式（不续接父会话）"
		if !in.FreshSession {
			reason = "本次无 source session（fresh run）"
		}
		in.job.Append(store.LogLine{Type: "phase", Content: "🆕 跳过会话上行：" + reason})
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
			wantSIDs := requirementSessionIDs(in.reqRow, in.sourceSID)
			if sftpErr := h.syncRequirementSessionsUp(client, in.job, slugDir, remoteSlugDir, wantSIDs); sftpErr != nil {
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
	runBody := workerRunRequest(opts, envPairs)

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
	// parseStreamJSONFromReader returns as soon as it sees the `result` event
	// (see wizard_stream.go), so this response body is routinely left unread.
	// HTTPTransport uses DisableKeepAlives:false, which means Body.Close()
	// then DRAINS the remainder — and the worker only calls res.end() once the
	// child process exits. If a plan-mode claude lingers after emitting
	// `result`, that drain blocks indefinitely, wedging the run right after
	// the "📥 同步会话结果回本地..." phase line with no error and no
	// completion. Connection: close makes net/http take the early-close path
	// (tear the channel down) instead of draining. Cost is one extra SSH
	// channel setup per run — negligible against a 5–15 minute plan pass.
	req.Close = true
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
	// Routed through the timeout-hardened helper (same one the architect chain
	// has used since cec11ce). The bare client.SyncDirDownMapped below took
	// neither a context nor a deadline, so an unusable SSH connection or an
	// exhausted channel budget blocked forever and the coding job never
	// reached its terminal state — the remote claude had already finished,
	// while Nova stayed pinned to this phase line.
	h.syncSessionDownWithTimeout(ctx, client, in.job, in.reqRow, remoteSlugDir)

	// Step 7: collect the result. Skip when the run errored out (no real
	// result) so we don't propagate half-broken state. The user can always
	// retry adjust-coding on the remote worktree via ContinueCoding. The
	// transport decides WHERE the committed result goes — origin (push) for
	// originTransport, the local isolated worktree (bundle down-sync) for
	// bundleTransport.
	if out.errMsg == "" && out.finalResult != "" {
		title := "nova-agent: " + in.req.RequirementTitle
		if title == "nova-agent: " {
			title = "nova-agent: " + in.reqRow.Title
		}
		tr.CollectResult(ctx, client, in, baseRepo, wtPath, branch, baseBranch, title)
	}

	return out
}

// prepareRemoteAgentRun owns the Agent-server side of a Claude CLI task
// up to and including the worker invocation + NDJSON stream parse. It is
// the SHARED trunk between dev coding and architect design — both stages
// need the same SSH dial, git worktree / base-repo / branch strategy,
// local-session jsonl SFTP upload, worker POST body, env pinning,
// health probe, POST /v1/run, and post-run session jsonl download.
//
// Callers (runRemoteCoding, runRemoteArchitectDesign) feed it a
// remoteRunInput and receive (claudeStreamOutcome, cleanup, error). The
// cleanup function closes the SSH connection established by this helper;
// callers MUST defer it (a bare `defer client.Close()` inside the helper
// would tear the connection down before the helper returns the outcome).
//
// This helper does NOT perform post-run persistence (commit + push for
// coding, design_docs write for architect) — those are stage-specific and
// live in the caller.
func (h *WizardHandler) prepareRemoteAgentRun(in *remoteRunInput) (claudeStreamOutcome, func(), error) {
	if h.agentSvrSvc == nil {
		return claudeStreamOutcome{errMsg: "Agent 服务器服务未初始化"}, func() {}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()

	// Step 1: load the (decrypted) credential before anything else — a missing
	// master key surfaces here as a clear error instead of a generic SSH failure.
	srv, plain, err := h.agentSvrSvc.GetWithCredential(in.serverID)
	if err != nil {
		return claudeStreamOutcome{errMsg: "无法读取 Agent 服务器凭据: " + err.Error()}, func() {}, nil
	}
	in.job.Append(store.LogLine{Type: "phase", Content: "🔌 连接到 Agent 服务器 " + srv.Name + " (" + srv.Host + ")"})

	client, err := gossh.Dial(ctx, srv.Host, srv.Port, srv.Username, srv.AuthType, plain)
	if err != nil {
		return claudeStreamOutcome{errMsg: "SSH 连接失败: " + err.Error()}, func() {}, nil
	}
	// Cleanup closes the SSH connection; the helper defers nothing here so
	// the connection survives long enough for the caller to read the
	// outcome and run its own post-run logic (commit/push for dev, plan
	// capture for architect).
	cleanup := func() { client.Close() }

	// Step 2: code sync via git. baseRepo hosts a single origin clone for the
	// project; wtPath is the per-requirement worktree that mirrors the local
	// branch isolation model. Without a remote_url on the project the entire
	// remote path is dead — fail early with a clear message instead of an
	// opaque "git clone exit 128".
	if in.reqRow == nil {
		return claudeStreamOutcome{errMsg: "远程执行需要已保存的需求记录（缺 Requirement）"}, cleanup, nil
	}
	originURL, err := h.projectSvc.OriginURL(in.reqRow.ProjectID)
	if err != nil || originURL == "" {
		return claudeStreamOutcome{errMsg: "项目未配置 git 远程仓库，无法在 Agent 服务器执行。请先在项目设置中配置 origin。" + errString(err)}, cleanup, nil
	}
	baseRepo := "/tmp/nova-agent/" + in.reqRow.ProjectID + "/base"
	wtPath := "/tmp/nova-agent/" + in.reqRow.ProjectID + "/" + in.reqRow.ID
	// branch defaults to requirement-<id> when the caller did not pin one.
	// Architect passes "" → falls back to the default (the branch label is
	// unused for the plan-mode run; git worktree still needs a name to
	// attach to).
	branch := in.branch
	if branch == "" {
		branch = "requirement-" + in.reqRow.ID
	}
	// baseBranch fallback chain: caller-provided baseBranch > project.DefaultBranch
	// > literal "main". Mirrors the local execStartCoding chain so the remote
	// worktree is rooted at the same base the local wizard uses.
	baseBranch := in.baseBranch
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
			return claudeStreamOutcome{errMsg: "git clone 失败（exit=" + fmtInt(exit) + "），请检查 origin 凭据"}, cleanup, nil
		}
	} else {
		client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git fetch origin --prune", "", nil, &jobWriter{job: in.job}, nil)
	}
	// Always (re)fetch the project's main branch into origin/<baseBranch> so
	// the worktree strategies below branch off the latest upstream.
	client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git fetch origin "+shellQuoteSingle(baseBranch), "", nil, &jobWriter{job: in.job}, nil)
	client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree prune", "", nil, &jobWriter{job: in.job}, nil)

	if !client.Exists(wtPath) {
		// Strategy 1: branch off origin/<baseBranch>.
		exit, _ := client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree add -b "+shellQuoteSingle(branch)+" "+shellQuoteSingle(wtPath)+" origin/"+shellQuoteSingle(baseBranch), "", nil, &jobWriter{job: in.job}, nil)
		if exit != 0 {
			// Strategy 2: branch off HEAD.
			exit, _ = client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree add -b "+shellQuoteSingle(branch)+" "+shellQuoteSingle(wtPath), "", nil, &jobWriter{job: in.job}, nil)
			if exit != 0 {
				// Strategy 3: attach to an already-existing branch.
				if exit, _ = client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree add "+shellQuoteSingle(wtPath)+" "+shellQuoteSingle(branch), "", nil, &jobWriter{job: in.job}, nil); exit != 0 {
					return claudeStreamOutcome{errMsg: "git worktree 创建失败（exit=" + fmtInt(exit) + "），请检查仓库状态"}, cleanup, nil
				}
			}
		}
	} else {
		// adjust / continue reuse path: pull the latest remote commits onto
		// the existing branch (--ff-only, best-effort).
		client.Exec(ctx,
			"cd "+shellQuoteSingle(wtPath)+" && (git checkout "+shellQuoteSingle(branch)+" 2>/dev/null || true) && (git pull --ff-only origin "+shellQuoteSingle(branch)+" 2>&1 || echo \"[nova-agent] pull 跳过（无跟踪或已分叉）\") && (git merge --ff-only origin/"+shellQuoteSingle(baseBranch)+" 2>&1 || echo \"[nova-agent] 主分支快进更新跳过\")",
			"", nil, &jobWriter{job: in.job}, nil)
	}
	logRemoteLatestCommit(ctx, client, in.job, wtPath)

	// Step 2.5: configure git identity + (optionally) GPG signing in the
	// remote worktree. Failure policy mirrors runRemoteCoding: GPG enabled +
	// provision fails → abort the run.
	gitName, gitEmail := lookupGitIdentity(h.projectSvc, h.platformSvc, in.reqRow)
	gnupgHome := "/tmp/nova-agent/" + in.reqRow.ProjectID + "/" + in.reqRow.ID + ".gnupg"
	if project, pErr := h.projectSvc.Get(in.reqRow.ProjectID); pErr == nil && project != nil && project.PlatformTokenID != "" {
		enabled, _, armored, passphrase, gpgErr := h.platformSvc.GPGSigningMaterial(project.PlatformTokenID)
		switch {
		case gpgErr != nil:
			in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 读取 GPG 配置失败：" + gpgErr.Error() + "，按未启用处理"})
		case enabled && armored != "":
			in.job.Append(store.LogLine{Type: "phase", Content: "🔐 在 Agent 服务器上配置 GPG 签名..."})
			keyID, gpgCleanup, provErr := provisionRemoteGPG(ctx, client, in.job, gnupgHome, wtPath, baseRepo, armored, passphrase, gitName, gitEmail)
			if provErr != nil {
				if gpgCleanup != nil {
					gpgCleanup()
				}
				return claudeStreamOutcome{errMsg: provErr.Error()}, cleanup, nil
			}
			defer gpgCleanup()
			go func() {
				<-ctx.Done()
				gpgCleanup()
			}()
			in.job.Append(store.LogLine{Type: "message", Content: "✅ GPG 已就绪（keyid=" + keyID + "）"})
			_ = h.platformSvc.UpdateGPGKeyID(project.PlatformTokenID, keyID)
		case enabled:
			in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 已启用 GPG 签名但未保存私钥，本次提交未签名"})
		}
	}
	if idErr := provisionRemoteGitIdentity(ctx, client, in.job, wtPath, baseRepo, gitName, gitEmail); idErr != nil {
		in.job.Append(store.LogLine{Type: "message", Content: "⚠️ " + idErr.Error()})
	}

	// Step 3: session sync (up) so the remote claude can --resume the same
	// session. The remote slug is derived deterministically from wtPath.
	remoteProjectsRoot := "~/.claude/projects/"
	remoteSlug := util.EncodeClaudeSlug(wtPath)
	// Resolve "~" NOW, before any consumer sees this string.
	//
	// The remote claude CLI runs with cwd == wtPath and therefore stores its
	// session jsonl under $HOME/.claude/projects/<slug>/. A literal
	// "~/.claude/projects/<slug>" is NOT interchangeable: Mkdirp builds a
	// single-quoted `mkdir -p '~/.claude/...'` (the shell does not expand a
	// tilde inside quotes) and pkg/sftp does not understand "~" either, so
	// every op below would address a directory literally named "~" — where the
	// remote CLI never looks. The observable damage is a permanent no-op on
	// Step 6 ("📥 同步会话结果回本地...") plus a bogus "远端未找到 session
	// 文件" pre-flight warning on every run. Resolving once here also makes the
	// path printed in the job log match the path actually used, which is what
	// the pre-flight check below logs.
	remoteSlugDir := remoteProjectsRoot + remoteSlug
	if expanded, eerr := client.ExpandHome(remoteSlugDir); eerr == nil {
		remoteSlugDir = expanded
	} else {
		in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 无法解析远端 $HOME（" + eerr.Error() + "），会话同步可能失效"})
	}
	var sessionMissingSide string
	if in.sourceSID == "" {
		// No source session to resume → no jsonl to push. The architect
		// fresh-session path (skip-analysis) lands here too. Log a
		// targeted hint so the user can see WHY we skipped the sync.
		in.job.Append(store.LogLine{Type: "message", Content: "🆕 跳过会话上行：本次无 source session（fresh run）"})
	} else {
		in.job.Append(store.LogLine{Type: "phase", Content: "📤 同步 Claude 会话历史（SFTP 上行）..."})
		slugDir, slugErr := h.claudeProjectsSlugDir(in.reqRow)
		switch {
		case slugErr != nil:
			in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 无法定位本地 claude session 目录（" + slugErr.Error() + "），将无 resume 启动新会话"})
			sessionMissingSide = "local"
		case slugDir == "":
			in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 本地未找到匹配的 claude session 目录（project slug 未缓存），将无 resume 启动新会话"})
			sessionMissingSide = "local"
		default:
			wantSIDs := requirementSessionIDs(in.reqRow, in.sourceSID)
			if sftpErr := h.syncRequirementSessionsUp(client, in.job, slugDir, remoteSlugDir, wantSIDs); sftpErr != nil {
				in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 会话上行失败（将无 resume 启动新会话）: " + sftpErr.Error()})
				sessionMissingSide = "sync-failed"
			} else {
				// Pre-flight: confirm the jsonl actually landed under the
				// remote slug dir.
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

	// Step 4: build the worker POST body.
	ignoreLocal := true
	// Rewrite the persona header's workDir to the remote cwd before posting.
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
		PermissionMode:         in.PermissionMode,
		OverrideSettingSources: &ignoreLocal,
	}
	envPairs := h.llm.BuildRemoteEnvPairsWithConfig(opts.Model, in.claudeConfigID)
	runBody := workerRunRequest(opts, envPairs)

	// Step 5: POST to nova-agent-worker via SSH direct-tcpip channel.
	in.job.Append(store.LogLine{Type: "phase", Content: "🤖 Agent 服务器开始执行（nova-agent-worker）..."})

	workerAddr := "127.0.0.1:7000"
	httpClient := &http.Client{Transport: client.HTTPTransport(workerAddr)}

	healthCtx, healthCancel := context.WithTimeout(ctx, 10*time.Second)
	healthReq, hReqErr := http.NewRequestWithContext(healthCtx, http.MethodGet, "http://"+workerAddr+"/v1/health", nil)
	if hReqErr != nil {
		healthCancel()
		return claudeStreamOutcome{errMsg: "构造健康检查请求失败: " + hReqErr.Error()}, cleanup, nil
	}
	healthResp, healthErr := httpClient.Do(healthReq)
	if healthErr != nil {
		healthCancel()
		return claudeStreamOutcome{errMsg: "无法连接 nova-agent-worker（" + workerAddr + "）。请在「设置 → Agent 服务器」对该服务器点「安装依赖」后再试。详细: " + healthErr.Error()}, cleanup, nil
	}
	healthResp.Body.Close()
	healthCancel()
	if healthResp.StatusCode != http.StatusOK {
		return claudeStreamOutcome{errMsg: fmt.Sprintf("nova-agent-worker 健康检查失败: HTTP %d", healthResp.StatusCode)}, cleanup, nil
	}

	bodyBytes, mErr := json.Marshal(runBody)
	if mErr != nil {
		return claudeStreamOutcome{errMsg: "序列化 worker 请求失败: " + mErr.Error()}, cleanup, nil
	}
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+workerAddr+"/v1/run", bytes.NewReader(bodyBytes))
	if reqErr != nil {
		return claudeStreamOutcome{errMsg: "构造 worker 请求失败: " + reqErr.Error()}, cleanup, nil
	}
	req.Header.Set("Content-Type", "application/json")
	// parseStreamJSONFromReader returns as soon as it sees the `result` event
	// (see wizard_stream.go), so this response body is routinely left unread.
	// HTTPTransport uses DisableKeepAlives:false, which means Body.Close()
	// then DRAINS the remainder — and the worker only calls res.end() once the
	// child process exits. If a plan-mode claude lingers after emitting
	// `result`, that drain blocks indefinitely, wedging the run right after
	// the "📥 同步会话结果回本地..." phase line with no error and no
	// completion. Connection: close makes net/http take the early-close path
	// (tear the channel down) instead of draining. Cost is one extra SSH
	// channel setup per run — negligible against a 5–15 minute plan pass.
	req.Close = true
	resp, doErr := httpClient.Do(req)
	if doErr != nil {
		return claudeStreamOutcome{errMsg: "POST /v1/run 失败: " + doErr.Error()}, cleanup, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return claudeStreamOutcome{errMsg: fmt.Sprintf("worker 返回 HTTP %d: %s", resp.StatusCode, truncateStr(string(errBody), 600))}, cleanup, nil
	}

	out := parseStreamJSONFromReader(resp.Body, jobSink{in.job}, "architect-design", in.usage)
	out.SessionFileMissingSide = sessionMissingSide

	// Step 6: session sync (down) — copy any new session jsonl back to local.
	in.job.Append(store.LogLine{Type: "phase", Content: "📥 同步会话结果回本地..."})
	h.syncSessionDownWithTimeout(ctx, client, in.job, in.reqRow, remoteSlugDir)

	return out, cleanup, nil
}

// syncSessionDownWithTimeout wraps Step 6's local-slug lookup and SFTP
// download of the remote session directory.
//
// Why a timeout: SyncDirDownMapped bottoms out in Client.sftp() ->
// sftp.NewClient(conn), which takes neither a context nor a timeout and can
// block indefinitely when the SSH connection is unusable or the server is out
// of channels. This step sits AFTER the "📥 同步会话结果回本地..." phase line
// and BEFORE prepareRemoteAgentRun returns, so a block here means the caller
// never receives an outcome: the job neither errors nor completes and the UI
// stays pinned to the last phase line forever.
//
// Why every path appends a line: success, failure and timeout all emit a
// message line, so that phase line can never be the last thing the user sees.
// A stall becomes a visible conclusion instead of a silent hang.
//
// Note: the timeout only stops US waiting. Go cannot safely cancel the SFTP
// call itself, so the worker goroutine may finish its transfer in the
// background after the timeout has already been reported. That is acceptable
// here — the download is forward-only and idempotent, so a late-completing
// transfer just lands the same files.
func (h *WizardHandler) syncSessionDownWithTimeout(ctx context.Context, client *gossh.Client, job *store.Job, reqRow *model.Requirement, remoteSlugDir string) {
	done := make(chan string, 1)
	sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- "⚠️ 会话下行异常: " + fmt.Sprint(r)
			}
		}()
		slugDir, slugErr := h.claudeProjectsSlugDir(reqRow)
		if slugErr != nil || slugDir == "" {
			// No local slug directory to write into — nothing to sync. This is
			// the pre-existing "best effort" semantics (see claudeProjectsSlugDir).
			// It returns immediately rather than blocking, so it cannot pin the
			// UI on the phase line; emitting nothing keeps the log quiet.
			done <- ""
			return
		}
		if sftpErr := client.SyncDirDownMapped(remoteSlugDir, slugDir); sftpErr != nil {
			done <- "⚠️ 会话下行失败: " + sftpErr.Error()
			return
		}
		done <- "✅ 会话结果已同步回本地"
	}()
	select {
	case msg := <-done:
		if msg != "" {
			job.Append(store.LogLine{Type: "message", Content: msg})
		}
	case <-sctx.Done():
		job.Append(store.LogLine{Type: "message", Content: "⚠️ 会话下行超时（60s），已跳过。远端会话文件仍在，可稍后重试或手工同步。"})
	}
}

// runRemoteArchitectDesign is the architect-stage counterpart of
// runRemoteCoding: it runs the plan-mode claude invocation on the chosen
// Agent server and returns the same claudeStreamOutcome shape the local
// exec body consumes. No commit, no push — plan-mode is read-only and the
// plan markdown rides back inside the assistant message's Write tool_use
// input (captured by parseStreamJSONFromReader into out.planContent).
//
// Callers (execArchitectDesign) are responsible for the post-run
// persistence: UpdateDesign(planMarkdown) + UpdateArchitectModel +
// UpdateDesignJob("") on the success path, stale-session cleanup or
// partial-result fallback on the error paths — mirroring the local
// branch's terminal-state handling.
func (h *WizardHandler) runRemoteArchitectDesign(in *remoteArchitectInput) claudeStreamOutcome {
	if in == nil || in.remoteRunInput == nil {
		return claudeStreamOutcome{errMsg: "缺少 remoteArchitectInput"}
	}
	out, cleanup, err := h.prepareRemoteAgentRun(in.remoteRunInput)
	defer cleanup()
	if err != nil {
		return claudeStreamOutcome{errMsg: err.Error()}
	}
	return out
}

// codeTransport abstracts the two Agent-server code-shipping strategies so
// runRemoteCoding's main body stays linear. Step 2 (get code onto the remote
// worktree) maps to PrepareRemote; Step 7 (get the committed result back) maps
// to CollectResult. originTransport is the legacy origin clone/push path;
// bundleTransport is the local-sync git-bundle-over-SFTP path.
type codeTransport interface {
	// PrepareRemote makes the per-requirement worktree exist on the agent host
	// at wtPath, checked out on branch and seeded with the code to work on.
	// Returns a hard error only when the worktree can't be prepared (the run
	// then aborts before any claude invocation).
	PrepareRemote(ctx context.Context, client *gossh.Client, in *remoteCodingInput, baseRepo, wtPath, branch, baseBranch string) error
	// CollectResult ships the remote worktree's committed result to wherever
	// it belongs — origin for originTransport, the local isolated worktree for
	// bundleTransport. Called only on the success path
	// (out.errMsg == "" && out.finalResult != ""). Failures are logged into
	// the job, never fatal — the user can still see the captured result text.
	CollectResult(ctx context.Context, client *gossh.Client, in *remoteCodingInput, baseRepo, wtPath, branch, baseBranch, title string)
}

// originTransport is the legacy Agent-server code path: the remote host holds a
// single origin clone under baseRepo, adds a per-requirement worktree branched
// off origin/<baseBranch>, and pushes the resulting commits back to origin.
// Requires the project to have a git remote reachable from the agent host.
//
// PrepareRemote / CollectResult are a verbatim extraction of the previous
// inline Step 2 / Step 7 — zero behavior change for the origin path.
type originTransport struct {
	originURL string // resolved via project.OriginURL before dispatch
}

func (t originTransport) PrepareRemote(ctx context.Context, client *gossh.Client, in *remoteCodingInput, baseRepo, wtPath, branch, baseBranch string) error {
	in.job.Append(store.LogLine{Type: "phase", Content: "📥 准备 Agent 服务器代码（git worktree 隔离）..."})
	if !client.Exists(baseRepo) {
		in.job.Append(store.LogLine{Type: "message", Content: "📦 首次 clone " + redactOriginForLog(t.originURL)})
		if exit, _ := client.Exec(ctx, "git clone "+shellQuoteSingle(t.originURL)+" "+shellQuoteSingle(baseRepo), "", nil, &jobWriter{job: in.job}, nil); exit != 0 {
			return fmt.Errorf("git clone 失败（exit=%d），请检查 origin 凭据", exit)
		}
	} else {
		client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git fetch origin --prune", "", nil, &jobWriter{job: in.job}, nil)
	}
	// Always (re)fetch the project's main branch into origin/<baseBranch> so
	// the worktree strategies below branch off the latest upstream. Best-effort:
	// any failure is logged but does not abort (the fallback strategies cover a
	// missing origin/<baseBranch>).
	client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git fetch origin "+shellQuoteSingle(baseBranch), "", nil, &jobWriter{job: in.job}, nil)
	client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree prune", "", nil, &jobWriter{job: in.job}, nil)

	if !client.Exists(wtPath) {
		// Strategy 1: branch off origin/<baseBranch> (freshly fetched above).
		exit, _ := client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree add -b "+shellQuoteSingle(branch)+" "+shellQuoteSingle(wtPath)+" origin/"+shellQuoteSingle(baseBranch), "", nil, &jobWriter{job: in.job}, nil)
		if exit != 0 {
			// Strategy 2: branch off HEAD (always valid).
			exit, _ = client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree add -b "+shellQuoteSingle(branch)+" "+shellQuoteSingle(wtPath), "", nil, &jobWriter{job: in.job}, nil)
			if exit != 0 {
				// Strategy 3: attach to an already-existing branch (reuse case).
				if exit, _ = client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree add "+shellQuoteSingle(wtPath)+" "+shellQuoteSingle(branch), "", nil, &jobWriter{job: in.job}, nil); exit != 0 {
					return fmt.Errorf("git worktree 创建失败（exit=%d），请检查仓库状态", exit)
				}
			}
		}
	} else {
		// adjust/continue reuse: pull latest onto the existing branch, then
		// fast-forward from origin/<baseBranch>. Non-blocking `|| echo` keeps a
		// diverged branch from aborting the run.
		client.Exec(ctx,
			"cd "+shellQuoteSingle(wtPath)+" && (git checkout "+shellQuoteSingle(branch)+" 2>/dev/null || true) && (git pull --ff-only origin "+shellQuoteSingle(branch)+" 2>&1 || echo \"[nova-agent] pull 跳过（无跟踪或已分叉）\") && (git merge --ff-only origin/"+shellQuoteSingle(baseBranch)+" 2>&1 || echo \"[nova-agent] 主分支快进更新跳过\")",
			"", nil, &jobWriter{job: in.job}, nil)
	}
	logRemoteLatestCommit(ctx, client, in.job, wtPath)
	return nil
}

func (t originTransport) CollectResult(ctx context.Context, client *gossh.Client, in *remoteCodingInput, baseRepo, wtPath, branch, baseBranch, title string) {
	in.job.Append(store.LogLine{Type: "phase", Content: "📤 推送代码变更到 origin..."})
	var pushStderr bytes.Buffer
	// No `-S` here on purpose: signing is driven by the worktree config set up
	// in Step 2.5, which also covers commits Claude makes from its own Bash
	// tool during the run.
	// Push source refspec is HEAD (not <branch>): the remote worktree's HEAD may
	// sit on a detached commit or an alias branch — code is committed by Claude's
	// own Bash tool during the run, not by Nova — so `git push origin <branch>`
	// can fail with `src refspec ... does not match any` when no local ref named
	// <branch> resolves. HEAD always resolves; we publish it to the full target
	// ref refs/heads/<branch>. The `{ diff || commit; }` grouping removes the
	// &&/|| precedence ambiguity: clean tree skips commit but still pushes; a
	// dirty tree commits then pushes; a failed commit skips the push.
	commitScript := "cd " + shellQuoteSingle(wtPath) +
		" && git add -A" +
		" && { git diff --cached --quiet || git commit -m " + shellQuoteSingle(title) + "; }" +
		" && git push origin " + shellQuoteSingle("HEAD:refs/heads/"+branch)
	if exit, _ := client.Exec(ctx, commitScript, "", nil, &jobWriter{job: in.job}, &pushStderr); exit != 0 {
		// GPG signing / push failures carry specific stderr patterns; classify
		// them into a precise Chinese message, falling back to generic wording.
		msg := classifyGitSignFailure(pushStderr.String())
		if msg == "" {
			msg = "❌ 推送失败（exit=" + fmtInt(exit) + "），请在远程 worktree 手动处理冲突"
		}
		in.job.Append(store.LogLine{Type: "error", Content: msg})
		// Non-fatal: the user can still see the work via the result text.
	} else {
		in.job.Append(store.LogLine{Type: "message", Content: "✅ 已推送到 origin/" + branch})
	}
}

// bundleTransport is the local-sync Agent-server code path for projects with no
// git remote reachable from the agent host. It ships commits as git bundle
// files over SFTP: the local isolated worktree is the source of truth each
// round, so PrepareRemote hard-resets the remote worktree to the just-uploaded
// local tip, and CollectResult lands the remote's new commits back into the
// local worktree (later integrated via 本地合并 / LocalMerge).
//
// The bundle's heads are fetched into a refs/bundle/* namespace on the agent
// host so baseRepo's HEAD stays an unborn branch — we never fetch into
// refs/heads/* (which would fail with exit 128 on a checked-out branch).
type bundleTransport struct {
	localRepo string // project.LocalPath (main checkout — object store for down commits)
	localWt   string // the local isolated worktree (in.workDir, else reqRow.WorktreePath)
}

func (t bundleTransport) PrepareRemote(ctx context.Context, client *gossh.Client, in *remoteCodingInput, baseRepo, wtPath, branch, baseBranch string) error {
	if t.localWt == "" {
		return fmt.Errorf("本地同步模式缺少隔离 worktree 路径（workDir / worktree_path 均为空），请先发起一次开发以创建 worktree")
	}
	in.job.Append(store.LogLine{Type: "phase", Content: "📦 准备 Agent 服务器代码（本地仓库同步 · git bundle 上行）..."})

	// 1. Build the up bundle — only the two refs (never --all) so no other
	//    local branch leaks and each round transfers just base + feature.
	upLocal, err := os.CreateTemp("", "nova-up-*.bundle")
	if err != nil {
		return fmt.Errorf("创建本地 bundle 临时文件失败: %w", err)
	}
	upLocalPath := upLocal.Name()
	_ = upLocal.Close()
	defer os.Remove(upLocalPath)
	if out, berr := gitRun(t.localWt, "bundle", "create", upLocalPath, baseBranch, branch); berr != nil {
		return fmt.Errorf("生成 git bundle 失败（%s）: %v", strings.TrimSpace(out), berr)
	}

	// 2. Upload to a per-requirement namespaced path on the agent host.
	upRemote := "/tmp/nova-agent/" + in.reqRow.ProjectID + "/" + in.reqRow.ID + ".up.bundle"
	if perr := client.PutFile(upLocalPath, upRemote, 0644); perr != nil {
		return fmt.Errorf("上传 git bundle 失败: %w", perr)
	}
	in.job.Append(store.LogLine{Type: "message", Content: "⬆️ 已上传代码 bundle 到 Agent 服务器"})
	defer client.Exec(ctx, "rm -f "+shellQuoteSingle(upRemote), "", nil, nil, nil)

	// 3. Idempotent baseRepo init — never clone (the bundle may carry no HEAD).
	if exit, _ := client.Exec(ctx, "git init "+shellQuoteSingle(baseRepo), "", nil, &jobWriter{job: in.job}, nil); exit != 0 {
		return fmt.Errorf("git init 失败（exit=%d）", exit)
	}
	// 4. Fetch the bundle's heads into refs/bundle/* (HEAD stays unborn).
	if exit, _ := client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git fetch "+shellQuoteSingle(upRemote)+" '+refs/heads/*:refs/bundle/*'", "", nil, &jobWriter{job: in.job}, nil); exit != 0 {
		return fmt.Errorf("git fetch bundle 失败（exit=%d）", exit)
	}
	client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree prune", "", nil, &jobWriter{job: in.job}, nil)

	// 5. Create or hard-reset the remote worktree to the just-uploaded tip.
	//    Local is the per-round source of truth, so a hard reset is correct and
	//    safe (the /tmp worktree is disposable).
	bundleRef := shellQuoteSingle("refs/bundle/" + branch)
	if !client.Exists(wtPath) {
		if exit, _ := client.Exec(ctx, "cd "+shellQuoteSingle(baseRepo)+" && git worktree add -b "+shellQuoteSingle(branch)+" "+shellQuoteSingle(wtPath)+" "+bundleRef, "", nil, &jobWriter{job: in.job}, nil); exit != 0 {
			return fmt.Errorf("git worktree 创建失败（exit=%d）", exit)
		}
	} else {
		if exit, _ := client.Exec(ctx,
			"cd "+shellQuoteSingle(wtPath)+" && (git checkout "+shellQuoteSingle(branch)+" 2>/dev/null || git switch -c "+shellQuoteSingle(branch)+" "+bundleRef+") && git reset --hard "+bundleRef,
			"", nil, &jobWriter{job: in.job}, nil); exit != 0 {
			return fmt.Errorf("重置远端 worktree 失败（exit=%d）", exit)
		}
	}
	logRemoteLatestCommit(ctx, client, in.job, wtPath)
	return nil
}

func (t bundleTransport) CollectResult(ctx context.Context, client *gossh.Client, in *remoteCodingInput, baseRepo, wtPath, branch, baseBranch, title string) {
	in.job.Append(store.LogLine{Type: "phase", Content: "📥 同步代码变更回本地隔离 worktree（git bundle 下行）..."})

	// 1. Commit on the remote (no push).
	commitScript := "cd " + shellQuoteSingle(wtPath) +
		" && git add -A && (git diff --cached --quiet || git commit -m " + shellQuoteSingle(title) + ")"
	if exit, _ := client.Exec(ctx, commitScript, "", nil, &jobWriter{job: in.job}, nil); exit != 0 {
		in.job.Append(store.LogLine{Type: "error", Content: "❌ 远端提交失败（exit=" + fmtInt(exit) + "）"})
		return
	}

	// 2. Empty-range guard: `git bundle create A..B` aborts on 0 commits with
	//    "Refusing to create empty bundle". Skip the whole down path if the
	//    remote produced nothing new relative to the uploaded base.
	baseRef := "refs/bundle/" + baseBranch
	var countBuf bytes.Buffer
	client.Exec(ctx, "git -C "+shellQuoteSingle(wtPath)+" rev-list --count "+shellQuoteSingle(baseRef+"..HEAD"), "", nil, &countBuf, nil)
	if strings.TrimSpace(countBuf.String()) == "0" {
		in.job.Append(store.LogLine{Type: "message", Content: "ℹ️ 远端无新增提交，跳过代码下行同步"})
		return
	}

	// 3. Build the incremental down bundle (range base = refs/bundle/<baseBranch>).
	downRemote := "/tmp/nova-agent/" + in.reqRow.ProjectID + "/" + in.reqRow.ID + ".down.bundle"
	if exit, _ := client.Exec(ctx, "git -C "+shellQuoteSingle(wtPath)+" bundle create "+shellQuoteSingle(downRemote)+" "+shellQuoteSingle(baseRef+".."+branch), "", nil, &jobWriter{job: in.job}, nil); exit != 0 {
		in.job.Append(store.LogLine{Type: "error", Content: "❌ 生成下行 bundle 失败（exit=" + fmtInt(exit) + "）"})
		return
	}
	defer client.Exec(ctx, "rm -f "+shellQuoteSingle(downRemote), "", nil, nil, nil)

	// 4. Download the bundle to a local temp file.
	downLocal, err := os.CreateTemp("", "nova-down-*.bundle")
	if err != nil {
		in.job.Append(store.LogLine{Type: "error", Content: "❌ 创建本地下行 bundle 临时文件失败: " + err.Error()})
		return
	}
	downLocalPath := downLocal.Name()
	_ = downLocal.Close()
	defer os.Remove(downLocalPath)
	if gerr := client.GetFile(downRemote, downLocalPath); gerr != nil {
		in.job.Append(store.LogLine{Type: "error", Content: "❌ 下载下行 bundle 失败: " + gerr.Error()})
		return
	}

	// 5. Land the commits back into the local isolated worktree. The branch tip
	//    should not have moved during the remote run (Nova-managed), so ff-only
	//    almost always succeeds; the reset --hard fallback recovers the rare
	//    divergence by following the remote result (the expected source of
	//    truth for this branch).
	if out, ferr := gitRun(t.localWt, "fetch", downLocalPath, branch); ferr != nil {
		in.job.Append(store.LogLine{Type: "error", Content: "❌ 本地 fetch bundle 失败（" + strings.TrimSpace(out) + "）: " + ferr.Error()})
		return
	}
	if _, merr := gitRun(t.localWt, "merge", "--ff-only", "FETCH_HEAD"); merr != nil {
		if out, rerr := gitRun(t.localWt, "reset", "--hard", "FETCH_HEAD"); rerr != nil {
			in.job.Append(store.LogLine{Type: "error", Content: "❌ 落地远端提交失败（" + strings.TrimSpace(out) + "）: " + rerr.Error()})
			return
		}
		in.job.Append(store.LogLine{Type: "message", Content: "⚠️ 本地分支无法快进，已硬重置到远端结果（该分支由 Nova 托管）"})
	}
	in.job.Append(store.LogLine{Type: "message", Content: "✅ 远端代码变更已同步回本地 worktree: " + t.localWt})
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

// logRemoteLatestCommit prints the HEAD commit of the remote worktree at wtPath
// into the job log ("📌 分支最新提交: <hash> <subject> (<author>, <date>)"), so
// the user can cross-check that the remote checkout landed on the expected
// commit after the fetch/worktree/reset dance above. Best-effort: the output
// (including any git stderr on an unborn/empty repo) is streamed through
// jobWriter; the exit code is ignored so this line can never abort a run.
func logRemoteLatestCommit(ctx context.Context, client *gossh.Client, job *store.Job, wtPath string) {
	if client == nil || job == nil || wtPath == "" {
		return
	}
	client.Exec(ctx, "cd "+shellQuoteSingle(wtPath)+
		" && git log -1 --format='📌 分支最新提交: %h %s (%an, %ad)' --date=format:'%Y-%m-%d %H:%M'",
		"", nil, &jobWriter{job: job}, nil)
}

// requirementSessionIDs returns the deduplicated, non-empty set of Claude
// session ids relevant to this requirement — analysis / design / coding — plus
// sourceSID as a fallback so the file that will actually be `--resume`d is
// always in the set. Returns nil (empty) when the requirement has no recorded
// session ids and sourceSID is empty, which signals syncRequirementSessionsUp
// to upload nothing (the remote starts a fresh session; there is no jsonl to
// resume).
func requirementSessionIDs(reqRow *model.Requirement, sourceSID string) []string {
	var ids []string
	seen := map[string]bool{}
	add := func(sid string) {
		if sid == "" || seen[sid] {
			return
		}
		seen[sid] = true
		ids = append(ids, sid)
	}
	if reqRow != nil {
		add(reqRow.AnalysisSessionID)
		add(reqRow.DesignSessionID)
		add(reqRow.CodingSessionID)
	}
	add(sourceSID)
	return ids
}

// humanSize renders a byte count as a compact human-readable string
// (B / KB / MB), used only for the per-session sync detail lines.
func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// syncRequirementSessionsUp uploads only the requirement-relevant <sid>.jsonl
// files (wantSIDs) to the remote slug dir, printing a per-file detail line so
// the user can see exactly which sessions were synced. When wantSIDs is empty
// (an old requirement with no recorded session ids) it uploads nothing and
// returns nil: the remote worker only ever reads the resumed <sid>.jsonl, so
// blindly pushing the whole project session directory has no effect there —
// the downstream RemoteFileExists pre-flight then classifies the run as a fresh
// (no-resume) session, which is the correct outcome for a requirement we can't
// thread.
//
// In practice this empty case is unreachable from the coding / architect paths:
// both callers now skip session sync entirely when sourceSID == "" (see the
// guards in runRemoteCoding / prepareRemoteAgentRun), and requirementSessionIDs
// always includes sourceSID, so a non-empty sourceSID yields a non-empty set.
//
// Returning an error keeps the caller on its existing "sync-failed" branch.
func (h *WizardHandler) syncRequirementSessionsUp(client *gossh.Client, job *store.Job, slugDir, remoteSlugDir string, wantSIDs []string) error {
	if len(wantSIDs) == 0 {
		job.Append(store.LogLine{Type: "message", Content: "ℹ️ 未记录需求会话 ID，跳过会话上行（远端将以新会话启动，无需同步整个项目会话目录）"})
		return nil
	}
	if merr := client.Mkdirp(remoteSlugDir); merr != nil {
		return merr
	}
	synced := 0
	for _, sid := range wantSIDs {
		local := filepath.Join(slugDir, sid+".jsonl")
		fi, statErr := os.Stat(local)
		if statErr != nil {
			job.Append(store.LogLine{Type: "message", Content: "  • 跳过 " + sid + ".jsonl（本地不存在）"})
			continue
		}
		if perr := client.PutFile(local, remoteSlugDir+"/"+sid+".jsonl", 0644); perr != nil {
			return perr
		}
		synced++
		job.Append(store.LogLine{Type: "message", Content: "  • " + sid + ".jsonl (" + humanSize(fi.Size()) + ")"})
	}
	if synced == 0 {
		job.Append(store.LogLine{Type: "message", Content: "⚠️ 未找到任何需求相关会话文件，将无 resume 启动新会话"})
	} else {
		job.Append(store.LogLine{Type: "message", Content: "✅ 已同步 " + fmtInt(synced) + " 个需求相关会话历史"})
	}
	return nil
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
	PermissionMode  string            `json:"permissionMode,omitempty"`
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
// shape (llm.StreamOpts). The env map is parsed from envPairs (each entry
// is "KEY=VALUE"); the worker hands this map to the claude subprocess's
// process env, so we don't need to strip the ANTHROPIC_* keys — the worker
// passes the map straight to the child.
//
// systemPrompt is intentionally left empty for the wizard remote path: the
// developer's persona is passed in the prompt itself (the -p payload
// includes the role system prompt as a preamble), matching the previous
// CLI invocation's behavior. If a future caller wants to pass it via
// --system-prompt, set opts.SystemPrompt before this is called.
//
// The PermissionMode field is forwarded verbatim: "plan" → worker emits
// --permission-mode plan (architect stage), "" → worker emits
// --dangerously-skip-permissions (dev stage). See agent-worker/server.mjs
// buildRunRequest / buildClaudeArgs.
//
// This helper depends only on opts (not on a stage-specific input struct)
// so both runRemoteCoding and prepareRemoteAgentRun can call it without
// each needing to project their input into a different shape.
func workerRunRequest(opts llm.StreamOpts, envPairs []string) workerRunBody {
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
		WorkDir:        opts.WorkDir,
		Prompt:         opts.Prompt,
		Model:          opts.Model,
		SessionID:      opts.SessionID,
		Resume:         opts.Resume,
		Fork:           opts.Fork,
		ForkSessionID:  opts.ForkSessionID,
		PermissionMode: opts.PermissionMode,
		Env:            envMap,
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
