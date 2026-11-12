package handler

import (
	"context"
	"fmt"
	"strings"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	gossh "github.com/novaworkbench/backend/internal/ssh"
	"github.com/novaworkbench/backend/internal/store"
)

// ── Agent-server execution for the 方案拆分 (requirement split) ─────────────
//
// A split is a strictly read-only run: it reads the master's technical plan,
// reads the repository to ground the candidate scope in real file paths, and
// returns a candidate list. It never writes anything.
//
// That makes the remote path far smaller than the wizard's
// (handler/wizard_remote.go), which has to carry a per-requirement git
// worktree, branch creation, GPG identity, Claude session up/down sync and a
// commit+push epilogue. None of that applies here, so this file deliberately
// does NOT reuse prepareRemoteAgentRun — it would demand a *model.Requirement
// we don't have and perform a pile of work a split has no use for.
//
// What IS shared with the remote Review path (handler/review_remote.go) is the
// genuinely common piece: the SSH client, the scratch-clone layout
// (<worktreeRoot>/<project_id>/base), ensureRemoteOrigin, ensureRemoteWorkerReady,
// workerRunRequest and parseStreamJSONFromReader — all reached through the
// package-level runRemoteWorkerStreamWith.
//
// Two deliberate differences from the local split path:
//
//  1. No "CLI produced nothing → fall back to the direct HTTP LLM channel"
//     fallback. That fallback is this machine's last line of defense; the HTTP
//     channel is reachable from here only, so a remote run has no equivalent.
//     A remote failure is reported as an error and the message tells the user
//     they can switch back to local execution and regenerate.
//
//  2. The agent host only ever sees the project's origin, so unpushed local
//     work is invisible to the remote run. The authoritative input — the
//     master's plan — travels inside the prompt, so this only affects how
//     precisely the model can cite file paths.

// runRemoteSplitTasks runs the split on an Agent Server and returns the parsed
// candidate list. It returns either the candidates or a user-facing error
// message, never both.
func (h *WizardHandler) runRemoteSplitTasks(job *store.Job, reqRow *model.Requirement, agentServerID,
	promptText, systemPrompt, modelName, configID string, usage *usageCtx) ([]prompt.SplitTask, string) {
	if h.agentSvrSvc == nil {
		return nil, "Agent 服务器服务未初始化，无法在远端执行拆分。可切回本地执行后重新生成。"
	}
	if h.projectSvc == nil {
		return nil, "项目服务未初始化，无法在远端执行拆分。可切回本地执行后重新生成。"
	}

	// Rolling ctx: the idle window resets on every NDJSON line from the
	// worker, with an absolute cap on top so a wedged remote claude can't
	// hold the job forever. Same pair the wizard's remote path uses.
	runCtx, cancel, hb := newRollingCtx("remote-split", agentIdleTimeout())
	defer cancel()
	runCtx, absCancel := context.WithTimeout(runCtx, agentMaxTotalTimeout())
	defer absCancel()

	srv, plain, err := h.agentSvrSvc.GetWithCredential(agentServerID)
	if err != nil {
		return nil, "无法读取 Agent 服务器凭据: " + err.Error()
	}
	job.Append(store.LogLine{Type: "phase", Content: "🔌 连接到 Agent 服务器 " + srv.Name + " (" + srv.Host + ")"})

	client, err := gossh.Dial(runCtx, srv.Host, srv.Port, srv.Username, srv.AuthType, plain)
	if err != nil {
		return nil, "SSH 连接失败: " + err.Error()
	}
	defer client.Close()

	// The agent host mirrors the project through its origin clone. Without a
	// remote the whole remote path is dead — say so instead of letting a
	// `git clone` fail with exit 128.
	originURL, err := h.projectSvc.OriginURL(reqRow.ProjectID)
	if err != nil || originURL == "" {
		return nil, "项目未配置 git 远程仓库，无法在 Agent 服务器执行拆分。请先在项目设置中配置 origin，或切回本地执行。"
	}

	worktreeRoot := agentWorktreeRootFor(srv)
	baseRepo := worktreeRoot + "/" + reqRow.ProjectID + "/base"

	// The scratch clone below lives under the work root. Create it if needed
	// and verify this SSH user can write there before running git — the same
	// guard the wizard paths use, so a read-only root fails with guidance
	// instead of `git clone` EACCES.
	if werr := ensureRemoteWorkRoot(runCtx, client, worktreeRoot, job, false); werr != nil {
		return nil, werr.Error()
	}

	job.Append(store.LogLine{Type: "phase", Content: "📥 准备 Agent 服务器代码..."})
	if !client.Exists(baseRepo) {
		job.Append(store.LogLine{Type: "message", Content: "📦 首次 clone " + redactOriginForLog(originURL)})
		if exit, _ := client.Exec(runCtx, "git clone "+shellQuoteSingle(originURL)+" "+shellQuoteSingle(baseRepo), "", nil, &jobWriter{job: job}, nil); exit != 0 {
			return nil, fmt.Sprintf("git clone 失败（exit=%s），请检查 origin 凭据，或切回本地执行后重新生成", fmtInt(exit))
		}
	}
	// The scratch clone is long-lived, so its origin still carries whichever
	// credential was current when it was created. Re-point it at the
	// project's CURRENT token before fetching. Non-fatal.
	if oErr := ensureRemoteOrigin(runCtx, client, job, baseRepo, originURL, remoteOriginAuthed(h.projectSvc, reqRow.ProjectID)); oErr != nil {
		job.Append(store.LogLine{Type: "message", Content: "⚠️ " + oErr.Error()})
	}
	// Best-effort, exactly like the local fetchOrigin: a stale mirror still
	// lets the model read whatever refs it already has.
	if exit, _ := client.Exec(runCtx, "cd "+shellQuoteSingle(baseRepo)+" && git fetch origin --prune", "", nil, &jobWriter{job: job}, nil); exit != 0 {
		job.Append(store.LogLine{Type: "message", Content: "⚠️ git fetch origin 失败，继续使用远端已有的引用"})
	}

	ignoreLocal := true
	opts := llm.StreamOpts{
		Prompt:         promptText,
		WorkDir:        baseRepo,
		SystemPrompt:   systemPrompt,
		Model:          cliModelArg(modelName),
		ClaudeConfigID: configID,
		// Read-only, same contract as the local split: the model may Read /
		// Glob / Grep to ground the scope in real file paths, but it must not
		// touch the repository.
		DisallowedTools:        []string{"Write", "Edit", "Bash", "NotebookEdit"},
		OverrideSettingSources: &ignoreLocal,
	}

	job.Append(store.LogLine{Type: "phase", Content: "🤖 Agent 服务器开始执行拆分（nova-agent-worker）..."})
	out, errMsg := runRemoteWorkerStreamWith(h.llm, runCtx, client, srv, job, opts, configID, "requirement-split", usage, hb)
	if errMsg != "" {
		return nil, errMsg
	}

	// finalResult is the authoritative output; streamText is the fallback for
	// runs that ended without a `result` event (seen with some OpenAI-
	// compatible proxies).
	raw := out.finalResult
	if strings.TrimSpace(raw) == "" {
		raw = out.streamText
	}
	if strings.TrimSpace(raw) == "" {
		return nil, "Agent 服务器未返回任何拆分内容。可切回本地执行后重新生成。"
	}

	jsonPart, _ := extractTrailingJSON(raw)
	tasks := prompt.NormalizeSplitTasks(jsonPart)
	if len(tasks) == 0 {
		return nil, "Agent 服务器未返回可解析的候选子需求。可切回本地执行后重新生成。"
	}
	return tasks, ""
}
