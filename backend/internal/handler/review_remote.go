package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	gossh "github.com/novaworkbench/backend/internal/ssh"
	"github.com/novaworkbench/backend/internal/store"
)

// ── Agent-server execution for Review sessions ──────────────────────────────
//
// A review is a strictly read-only run: it diffs two refs, reads source files
// and writes a report back over the stream. That makes the remote path far
// smaller than the wizard's (handler/wizard_remote.go), which has to carry a
// per-requirement git worktree, branch creation, GPG identity, Claude session
// up/down sync and a commit+push epilogue. None of that applies here, so this
// file deliberately does NOT reuse prepareRemoteAgentRun — it would demand a
// *model.Requirement we don't have and perform a pile of work a review has no
// use for. What IS shared are the genuinely common pieces: the SSH client, the
// scratch-clone layout (/tmp/nova-agent/<project_id>/base), ensureRemoteOrigin,
// ensureRemoteWorkerReady, workerRunRequest and parseStreamJSONFromReader.
//
// Scope note: because the agent host only ever sees the project's origin, a
// branch that was never pushed cannot be reviewed remotely. That is reported
// as a clear error rather than silently diffing something else — local
// execution remains the answer for unpushed work.

// runRemoteReviewSession runs one review on an Agent Server and persists the
// outcome. It owns the terminal state exactly like the local branch of
// runReviewSession does: every exit path goes through h.finishReview.
//
// rolePrompt is the reviewer persona WITHOUT the attachment manifest — the
// manifest can only be rendered after the files have been uploaded, since it
// has to cite the paths they landed on over there.
func (h *ReviewHandler) runRemoteReviewSession(ctx context.Context, job *store.Job, rv *model.CodeReview, project *model.Project, rolePrompt, modelName, claudeConfigID string) {
	report, errMsg := h.execRemoteReview(ctx, job, rv, project, rolePrompt, modelName, claudeConfigID)
	if errMsg != "" {
		job.Append(store.LogLine{Type: "error", Content: errMsg})
		h.finishReview(job, rv, modelName, model.ReviewStatusError, errMsg)
		return
	}
	h.finishReview(job, rv, modelName, model.ReviewStatusDone, report)
}

// execRemoteReview is the body of the remote run. It returns either the final
// report or a user-facing error message (never both); the caller maps that
// onto the session row.
func (h *ReviewHandler) execRemoteReview(ctx context.Context, job *store.Job, rv *model.CodeReview, project *model.Project, rolePrompt, modelName, claudeConfigID string) (report, errMsg string) {
	if h.agentSvrSvc == nil {
		return "", "Agent 服务器服务未初始化，无法在远端执行 Review"
	}

	// Rolling ctx: the idle window resets on every NDJSON line from the
	// worker, with an absolute cap on top so a wedged remote claude can't
	// hold the job forever. Same pair the wizard's remote path uses.
	runCtx, cancel, hb := newRollingCtx("remote-review", agentIdleTimeout())
	defer cancel()
	runCtx, absCancel := context.WithTimeout(runCtx, agentMaxTotalTimeout())
	defer absCancel()

	srv, plain, err := h.agentSvrSvc.GetWithCredential(rv.AgentServerID)
	if err != nil {
		return "", "无法读取 Agent 服务器凭据: " + err.Error()
	}
	job.Append(store.LogLine{Type: "phase", Content: "🔌 连接到 Agent 服务器 " + srv.Name + " (" + srv.Host + ")"})

	client, err := gossh.Dial(runCtx, srv.Host, srv.Port, srv.Username, srv.AuthType, plain)
	if err != nil {
		return "", "SSH 连接失败: " + err.Error()
	}
	defer client.Close()

	// The agent host mirrors the project through its origin clone. Without a
	// remote the whole remote path is dead — say so instead of letting a
	// `git clone` fail with exit 128.
	originURL, err := h.projectSvc.OriginURL(project.ID)
	if err != nil || originURL == "" {
		return "", "项目未配置 git 远程仓库，无法在 Agent 服务器执行 Review。请先在项目设置中配置 origin。" + errString(err)
	}
	worktreeRoot := agentWorktreeRootFor(srv)
	baseRepo := worktreeRoot + "/" + project.ID + "/base"

	// The scratch clone below lives under the work root. Create it if needed
	// and verify this SSH user can write there before running git — the same
	// guard the wizard paths use, so a read-only root fails with guidance
	// instead of `git clone` EACCES.
	if werr := ensureRemoteWorkRoot(runCtx, client, worktreeRoot, job, false); werr != nil {
		return "", werr.Error()
	}

	job.Append(store.LogLine{Type: "phase", Content: "📥 准备 Agent 服务器代码..."})
	if !client.Exists(baseRepo) {
		job.Append(store.LogLine{Type: "message", Content: "📦 首次 clone " + redactOriginForLog(originURL)})
		if exit, _ := client.Exec(runCtx, "git clone "+shellQuoteSingle(originURL)+" "+shellQuoteSingle(baseRepo), "", nil, &jobWriter{job: job}, nil); exit != 0 {
			return "", "git clone 失败（exit=" + fmtInt(exit) + "），请检查 origin 凭据"
		}
	}
	// The scratch clone is long-lived, so its origin still carries whichever
	// credential was current when it was created. Re-point it at the
	// project's CURRENT token before fetching. Non-fatal.
	if oErr := ensureRemoteOrigin(runCtx, client, job, baseRepo, originURL, remoteOriginAuthed(h.projectSvc, project.ID)); oErr != nil {
		job.Append(store.LogLine{Type: "message", Content: "⚠️ " + oErr.Error()})
	}
	// Best-effort, exactly like the local fetchOrigin: a stale mirror still
	// reviews whatever refs it already has.
	if exit, _ := client.Exec(runCtx, "cd "+shellQuoteSingle(baseRepo)+" && git fetch origin --prune", "", nil, &jobWriter{job: job}, nil); exit != 0 {
		job.Append(store.LogLine{Type: "message", Content: "⚠️ git fetch origin 失败，继续使用远端已有的引用"})
	}

	headRef, baseRef, err := resolveRemoteDiffRefs(runCtx, client, baseRepo, rv.Branch, rv.BaseBranch)
	if err != nil {
		return "", "无法在 Agent 服务器上解析对比分支: " + err.Error()
	}
	job.Append(store.LogLine{Type: "message", Content: "🔍 对比范围: " + baseRef + "..." + headRef})

	// Attachments have to be on the agent host before the manifest can name
	// their paths. A sync failure is not fatal — the review then runs without
	// the 📎 block, which is strictly better than not running at all.
	attachDir := ""
	if h.attachSvc != nil {
		if rd, serr := h.attachSvc.SyncToAgentServer(runCtx, rv.AgentServerID, client, rv.ID); serr != nil {
			job.Append(store.LogLine{Type: "message", Content: "⚠️ 附件同步到 Agent 服务器失败（本次 Review 不含附件上下文）: " + serr.Error()})
		} else if rd != "" {
			attachDir = rd
		}
	}
	manifest := attachManifestFor(h.attachSvc, rv.ID, attachDir)
	systemPrompt := appendManifest(rolePrompt, manifest)

	taskPrompt := buildReviewTaskPrompt(rv, headRef, baseRef, manifest != "")

	ignoreLocal := true
	opts := llm.StreamOpts{
		Prompt:                 taskPrompt,
		WorkDir:                baseRepo,
		SystemPrompt:           systemPrompt,
		Model:                  cliModelArg(modelName),
		ClaudeConfigID:         claudeConfigID,
		OverrideSettingSources: &ignoreLocal,
	}

	job.Append(store.LogLine{Type: "phase", Content: "🤖 Agent 服务器开始执行 Review（nova-agent-worker）..."})
	out, errMsg := h.runRemoteWorkerStream(runCtx, client, srv, job, opts, claudeConfigID, "review",
		h.newReviewUsage(job.ID, rv, modelName), hb)
	if errMsg != "" {
		return "", errMsg
	}
	// The remote run's session_id is what a later 追问 resumes (on the same
	// agent host, in the same scratch clone).
	//
	// KNOWN GAP (deliberately out of scope for req_6a9b88dd80fe8f01): the
	// remote Review path has NO session transcript sync at all — the .jsonl
	// stays on the agent host, so the conversation cannot be continued from
	// this machine and does not survive an agent-server switch. The
	// development path got the full treatment (discovery-based remote
	// session-dir lookup + per-session down-sync + a cross-server pull-back
	// keyed on requirements.session_server_id — see wizard_remote.go's
	// "Remote session directory discovery" block). The same building blocks
	// apply here; only the wiring is missing.
	h.rememberReviewSession(rv, out.sessionID)
	// finalResult is the authoritative report; streamText is the fallback for
	// runs that ended without a `result` event (seen with some OpenAI-
	// compatible proxies).
	report = out.finalResult
	if strings.TrimSpace(report) == "" {
		report = out.streamText
	}
	if strings.TrimSpace(report) == "" {
		return "", "Agent 服务器未返回任何 Review 内容"
	}
	return report, ""
}

// runRemoteWorkerStream POSTs one run to the agent host's nova-agent-worker
// and parses the NDJSON stream back. Shared by the remote review and the
// remote 追问 so the worker-readiness check, the Connection: close handling
// and the error wording can never drift between them.
//
// Returns either a parsed outcome or a user-facing error message, never both.
func (h *ReviewHandler) runRemoteWorkerStream(ctx context.Context, client *gossh.Client, srv *model.AgentServer, job *store.Job, opts llm.StreamOpts, claudeConfigID, scope string, uctx *usageCtx, hb chan<- struct{}) (claudeStreamOutcome, string) {
	var zero claudeStreamOutcome
	if err := ensureRemoteWorkerReady(ctx, client, srv, job); err != nil {
		return zero, err.Error()
	}

	runBody := workerRunRequest(opts, h.llm.BuildRemoteEnvPairsWithConfig(opts.Model, claudeConfigID), srv)

	const workerAddr = "127.0.0.1:7000"
	bodyBytes, mErr := json.Marshal(runBody)
	if mErr != nil {
		return zero, "序列化 worker 请求失败: " + mErr.Error()
	}
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+workerAddr+"/v1/run", bytes.NewReader(bodyBytes))
	if reqErr != nil {
		return zero, "构造 worker 请求失败: " + reqErr.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	// parseStreamJSONFromReader returns on the `result` event, leaving the
	// rest of the body unread; Close() would then DRAIN it and block until
	// the remote claude actually exits. Connection: close takes the
	// tear-down path instead. Same reasoning as wizard_remote.go.
	req.Close = true

	httpClient := &http.Client{Transport: client.HTTPTransport(workerAddr)}
	resp, doErr := httpClient.Do(req)
	if doErr != nil {
		return zero, "POST /v1/run 失败: " + doErr.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return zero, fmt.Sprintf("worker 返回 HTTP %d: %s", resp.StatusCode, truncateStr(string(errBody), 600))
	}

	out := parseStreamJSONFromReader(ctx, resp.Body, jobSink{job}, scope, uctx, hb)
	if out.errMsg != "" {
		return out, out.errMsg
	}
	return out, ""
}

// resolveRemoteDiffRefs is resolveDiffRefs' remote twin: same origin-first /
// local-fallback preference, evaluated on the agent host's scratch clone.
//
// In practice only `origin/<x>` exists over there (the clone is made fresh and
// nothing ever checks a branch out), so a branch that was never pushed fails
// here with a message that names the fix instead of producing an empty diff.
func resolveRemoteDiffRefs(ctx context.Context, client *gossh.Client, repoDir, branch, base string) (headRef, baseRef string, err error) {
	branch = strings.TrimSpace(branch)
	base = strings.TrimSpace(base)
	if branch == "" {
		return "", "", fmt.Errorf("目标分支不能为空")
	}
	if base == "" {
		return "", "", fmt.Errorf("基础分支不能为空")
	}
	headRef, ok := pickRemoteRef(ctx, client, repoDir, branch)
	if !ok {
		return "", "", fmt.Errorf("分支 %q 在 Agent 服务器的仓库副本中不存在（未推送到 origin 的本地分支请改用「本地执行」）", branch)
	}
	baseRef, ok = pickRemoteRef(ctx, client, repoDir, base)
	if !ok {
		return "", "", fmt.Errorf("基础分支 %q 在 Agent 服务器的仓库副本中不存在", base)
	}
	return headRef, baseRef, nil
}

// pickRemoteRef mirrors pickRef: prefer origin/<name>, fall back to the bare
// name, and honour a name the user already qualified.
func pickRemoteRef(ctx context.Context, client *gossh.Client, repoDir, name string) (string, bool) {
	candidates := []string{"origin/" + name, name}
	if strings.HasPrefix(name, "origin/") || strings.HasPrefix(name, "refs/") {
		candidates = []string{name, strings.TrimPrefix(name, "origin/")}
	}
	for _, ref := range candidates {
		cmd := "cd " + shellQuoteSingle(repoDir) + " && git rev-parse --verify --quiet " + shellQuoteSingle(ref+"^{commit}")
		if exit, execErr := client.Exec(ctx, cmd, "", nil, io.Discard, nil); execErr == nil && exit == 0 {
			return ref, true
		}
	}
	return "", false
}
