package handler

import (
	"context"
	"strings"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	gossh "github.com/novaworkbench/backend/internal/ssh"
	"github.com/novaworkbench/backend/internal/store"
)

// ── Resumable turns on a finished Review ────────────────────────────────────
//
// Two features run "one more claude turn against a review that already
// happened": 追问 (ask the report a question) and 「基于追问对话重新生成」
// (rewrite the report in light of that conversation). Mechanically they are
// the same operation and share every hard-won detail:
//
//   - Resume the review's claude session when it still exists. That session
//     holds the diff, the files the reviewer opened and its own reasoning;
//     rebuilding that from a cold start costs tokens AND accuracy.
//   - Detect the specific "that session is gone" failure (isStaleSessionError)
//     and retry once with a self-contained prompt instead of failing the
//     request. A pruned jsonl must degrade, never break.
//   - On an Agent Server, run in the SAME scratch clone the review used
//     (/tmp/nova-agent/<project_id>/base) — the resumable session lives on
//     that host, so resuming from anywhere else finds nothing.
//
// The two differ only in the prompt they send, the phase lines they log and
// how their token usage is attributed. Those are the knobs in reviewTurnSpec;
// everything else lives here so the 追问 path and the regenerate path cannot
// drift apart.

// reviewTurnSpec is the per-feature half of a resumable turn.
type reviewTurnSpec struct {
	// scope tags the run in logs and token-usage rows ("review-followup",
	// "review-regenerate").
	scope string
	// prompt renders the task prompt. resumed=true selects the short form
	// (the session already holds the report); false the self-contained one.
	prompt func(resumed bool) string
	// usage builds a fresh usage recorder per attempt — a retried turn is a
	// second claude invocation and bills separately.
	usage func() *usageCtx
	// resumePhase is logged before the resume attempt; staleNotice is logged
	// when that attempt comes back with a dead session and the fresh retry
	// takes over. Both are user-facing Chinese, per the repo convention.
	resumePhase string
	staleNotice string
	// remoteMissingRepo is the error shown when the agent host no longer has
	// the review's scratch clone, phrased for this feature.
	remoteMissingRepo string
	// remoteNoService is the error shown when the Agent Server service isn't
	// wired at all.
	remoteNoService string
}

// runLocalReviewTurn executes one turn on the local checkout: resume first
// when there is a session, fall back to a fresh run when that session is
// gone. resumed reports which of the two produced the returned text, so the
// caller can record it (the 追问 row's `resumed` column) or say so in the UI.
func (h *ReviewHandler) runLocalReviewTurn(ctx context.Context, job *store.Job, rv *model.CodeReview, project *model.Project, rolePrompt, modelName, claudeConfigID string, spec reviewTurnSpec) (text string, resumed bool, errMsg string) {
	// No --add-dir grant here — review runs with --dangerously-skip-permissions,
	// so reads outside the working directory are already allowed. Only the
	// requirement-side stages need the grant (see handler/attachment_grant.go).
	manifest := attachManifestFor(h.attachSvc, rv.ID, "")
	systemPrompt := appendManifest(rolePrompt, manifest)
	sessionID := strings.TrimSpace(rv.ClaudeSessionID)

	if sessionID != "" {
		job.Append(store.LogLine{Type: "phase", Content: spec.resumePhase})
		out, stale, msg := h.streamReviewTurn(ctx, job, project.LocalPath, systemPrompt,
			spec.prompt(true), modelName, claudeConfigID, sessionID, true, spec.usage())
		if !stale {
			return out, true, msg
		}
		// The jsonl is gone (CLI pruned it, or the review ran elsewhere).
		// Fall through to a fresh run rather than failing the request.
		job.Append(store.LogLine{Type: "message", Content: spec.staleNotice})
	}

	out, _, msg := h.streamReviewTurn(ctx, job, project.LocalPath, systemPrompt,
		spec.prompt(false), modelName, claudeConfigID, "", false, spec.usage())
	return out, false, msg
}

// runRemoteReviewTurn is runLocalReviewTurn's Agent-Server twin. It reuses
// the scratch clone the review created, which is both the working directory
// and the home of the resumable session.
func (h *ReviewHandler) runRemoteReviewTurn(ctx context.Context, job *store.Job, rv *model.CodeReview, project *model.Project, rolePrompt, modelName, claudeConfigID string, spec reviewTurnSpec) (text string, resumed bool, errMsg string) {
	if h.agentSvrSvc == nil {
		return "", false, spec.remoteNoService
	}
	runCtx, cancel, hb := newRollingCtx("remote-"+spec.scope, agentIdleTimeout())
	defer cancel()
	runCtx, absCancel := context.WithTimeout(runCtx, agentMaxTotalTimeout())
	defer absCancel()

	srv, plain, err := h.agentSvrSvc.GetWithCredential(rv.AgentServerID)
	if err != nil {
		return "", false, "无法读取 Agent 服务器凭据: " + err.Error()
	}
	client, err := gossh.Dial(runCtx, srv.Host, srv.Port, srv.Username, srv.AuthType, plain)
	if err != nil {
		return "", false, "SSH 连接失败: " + err.Error()
	}
	defer client.Close()

	baseRepo := agentWorktreeRootFor(srv) + "/" + project.ID + "/base"
	if !client.Exists(baseRepo) {
		// The clone is what both the review's session file and its working
		// directory hang off. Without it there is nothing to resume and no
		// code to read, so re-running the Review is the honest answer.
		return "", false, spec.remoteMissingRepo
	}

	// Attachments are only re-uploaded for a fresh session; a resumed one
	// already read whatever it needed during the review.
	sessionID := strings.TrimSpace(rv.ClaudeSessionID)
	attachDir := ""
	if sessionID == "" && h.attachSvc != nil {
		if rd, serr := h.attachSvc.SyncToAgentServer(runCtx, rv.AgentServerID, client, rv.ID); serr == nil {
			attachDir = rd
		}
	}
	// No --add-dir grant here — review runs with --dangerously-skip-permissions
	// (see handler/attachment_grant.go for the requirement-side grant).
	systemPrompt := appendManifest(rolePrompt, attachManifestFor(h.attachSvc, rv.ID, attachDir))

	ignoreLocal := true
	build := func(resume bool) llm.StreamOpts {
		opts := llm.StreamOpts{
			Prompt:                 spec.prompt(resume),
			WorkDir:                baseRepo,
			SystemPrompt:           systemPrompt,
			Model:                  cliModelArg(modelName),
			ClaudeConfigID:         claudeConfigID,
			OverrideSettingSources: &ignoreLocal,
		}
		if resume {
			opts.Resume = true
			opts.SessionID = sessionID
		}
		return opts
	}

	if sessionID != "" {
		job.Append(store.LogLine{Type: "phase", Content: spec.resumePhase})
		out, msg := h.runRemoteWorkerStream(runCtx, client, srv, job, build(true), claudeConfigID, spec.scope,
			spec.usage(), hb)
		if msg == "" {
			return firstNonEmpty(out.finalResult, out.streamText), true, ""
		}
		if !out.staleSession {
			return "", true, msg
		}
		job.Append(store.LogLine{Type: "message", Content: spec.staleNotice})
	}

	out, msg := h.runRemoteWorkerStream(runCtx, client, srv, job, build(false), claudeConfigID, spec.scope,
		spec.usage(), hb)
	if msg != "" {
		return "", false, msg
	}
	return firstNonEmpty(out.finalResult, out.streamText), false, ""
}
