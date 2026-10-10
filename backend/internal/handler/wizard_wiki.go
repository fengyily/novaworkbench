// Package handler — wizard pipeline (requirement → code).
//
// wizard_wiki.go is the wiki-kind generator. It mirrors the architect-design
// shape (JobStore job → plan-mode claude stream → persist plan markdown) but
// the result lives in requirements.wiki_docs (NOT design_docs) and the kind
// guard rejects anything that is not service.KindWiki. The flow never reaches
// the developer stage: 4-entry guards in wizard_orchestration / wizard_coding
// / wizard_immediate / schedule.go reject wiki rows before any commit-push-PR
// is even possible, so a wiki doc is a read-only knowledge artifact that can
// only be archived into the knowledge table via the dedicated wiki-archive
// endpoint (handler/requirement.go WikiArchive).
//
// Reused infrastructure (no new tables / state machines / streams):
//   - Gateway.StreamCmd plan-mode + --disallowedTools
//   - runClaudeStream → captures out.planContent (the Write tool_use body)
//   - JobStore (cap 50) + SSE replay/live via /api/wizard/jobs/{id}/stream
//   - sanitizeDesignDoc (only strips outer ```markdown``` fence; Mermaid
//     blocks survive untouched — the wiki block explicitly asks for them)
//   - existing prepareDesignWorkspace pattern, duplicated locally to avoid
//     coupling the wiki path to the architect path's status / session
//     resolution. The duplication is intentional: architect-design has a
//     deep session-thread (analyst → design fork) that wiki does not need.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	promptpkg "github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

// wikiStallTimeout mirrors architectStallTimeout. Wiki never fans out Explore
// sub-agents (the wiki block explicitly forbids destructive tools), so 10
// minutes is generous; matches the architect stage so the watchdog behavior
// is uniform across plan-mode stages.
const wikiStallTimeout = 10 * time.Minute

// wikiDisallowedTools is the closed list passed via StreamOpts.DisallowedTools
// to the claude CLI. Plan-mode already denies Write/Edit/NotebookEdit, but
// passing them explicitly is defense in depth: a misbehaving proxy that
// ignores plan-mode cannot physically touch the project tree. Mirrors the
// safety reasoning documented in runRemoteArchitectDesign / wizard_coding.
var wikiDisallowedTools = []string{"Write", "Edit", "NotebookEdit"}

// wikiRunParams is the prepared-shape output of prepareWikiDoc. Mirrors
// designRunParams but skips the analyst → design session fork and the
// analysis_started_at backfill — wiki does not have an analyst stage.
type wikiRunParams struct {
	Req            *model.Requirement
	ProjectPath    string
	DefaultBranch  string
	// NewWikiSID is the freshly minted claude session id (always created on
	// the wiki path — there is no analyst session to fork from).
	NewWikiSID string
	// ResumeSID is the existing wiki_session_id when this is a re-run;
	// empty on the first run. The handler chooses between ResumeSID and
	// NewWikiSID the same way prepareArchitectDesign does.
	ResumeSID string
	WorkDir   string
	Prompt    string
	// SystemPrompt is the "wiki role" persona prompt (loaded via
	// roleConfig("wiki")) injected as the claude --system-prompt. Empty
	// when the user has not configured a wiki role (CLI falls back to
	// the default persona). Kept separate from Prompt so plan-mode's
	// CLI instructions always survive even if the role persona is empty.
	SystemPrompt   string
	Model          string
	ClaudeConfigID string
	ReadKnowledge  bool
	AgentServerID  string
}

// GenerateWikiDoc is the HTTP entry for the wiki-kind document generator.
// Mirrors ArchitectDesign (handler/wizard_architect.go:85): parse body,
// call the prepare step (returns *apiFailure on validation failure), mint
// a JobStore job, return the job id immediately, and dispatch the exec
// body in a goroutine. The frontend streams the live output via
// GET /api/wizard/jobs/{job_id}/stream and re-queries GET /api/requirements/{id}
// on terminal to fetch the persisted wiki_docs.
//
// Body: { requirement_id, model?, claude_config_id?, agent_server_id?, read_knowledge? }.
// 400 WIKI when the requirement is not kind=wiki; 404 NOT_FOUND when the id
// doesn't resolve; 200 {job_id} on success.
func (h *WizardHandler) GenerateWikiDoc(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequirementID string `json:"requirement_id"`
		Model         string `json:"model"`
		ClaudeConfigID string `json:"claude_config_id"`
		AgentServerID string `json:"agent_server_id"`
		ReadKnowledge bool   `json:"read_knowledge"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	p, job, af := h.prepareWikiDoc(r.Context(), body.RequirementID, body.Model, body.ClaudeConfigID, body.AgentServerID, body.ReadKnowledge)
	if writeIfAPIError(w, af) {
		return
	}
	writeJSON(w, 200, map[string]string{"job_id": job.ID})
	go h.execWikiDoc(p, job, nil)
}

// prepareWikiDoc runs the synchronous portion of the wiki-doc stage:
// requirement lookup, kind guard, project path / default branch resolution,
// session-id pre-mint, JobStore creation, status promotion. Returns
// (*wikiRunParams, *store.Job, *apiFailure); a non-nil apiFailure is a
// 4xx the HTTP path writes verbatim. The exec body consumes the params +
// job and writes progress into JobStore.
//
// Unlike prepareArchitectDesign this function does NOT need a source
// session to fork from — the wiki kind is the only kind whose first turn
// starts a fresh plan-mode conversation with no analyst to inherit from.
func (h *WizardHandler) prepareWikiDoc(ctx context.Context, requirementID, modelOverride, claudeConfigIDOverride string, agentServerID string, readKnowledge bool) (*wikiRunParams, *store.Job, *apiFailure) {
	id := requirementID
	if id == "" {
		return nil, nil, fail(400, "INVALID", "missing requirement id")
	}
	req, err := h.reqSvc.Get(id)
	if err != nil {
		return nil, nil, fail(404, "NOT_FOUND", "requirement not found")
	}
	// Hard kind guard: the wiki endpoint MUST only accept wiki rows. Any
	// other kind here is a client bug (the frontend hides the CTA for
	// issue/requirement/idea); reject loudly so it surfaces in tests.
	if req.Kind != service.KindWiki {
		return nil, nil, fail(400, "WIKI", "「知识库」文档生成仅适用于 kind=wiki 的需求")
	}
	project, _ := h.projectSvc.Get(req.ProjectID)
	projectPath := ""
	defaultBranch := ""
	if project != nil {
		projectPath = project.LocalPath
		defaultBranch = project.DefaultBranch
	}

	// Session threading: re-use the existing wiki session when one is
	// stored, otherwise mint a fresh id and persist it BEFORE spawning
	// claude so a mid-run crash still records the id. Mirrors the
	// design-stage pre-mint pattern in prepareArchitectDesign.
	resumeSID := req.WikiSessionID
	newWikiSID := ""
	if resumeSID == "" {
		newWikiSID = util.NewUUID()
		if perr := h.reqSvc.UpdateWikiSession(id, newWikiSID); perr != nil {
			log.Printf("[wiki-generate] failed to persist wiki session for %s: %v", id, perr)
		}
	}

	// Promote status: draft → designing so the detail page's stage gate
	// (status='designing' → architect CTAs) renders the wiki stream panel
	// during the run. Re-runs from 'designing' / 'designed' / 'archived'
	// stay put (we never downgrade; re-runs are explicitly idempotent
	// overwrites of wiki_docs).
	switch req.Status {
	case "draft":
		if _, serr := h.reqSvc.UpdateStatus(id, "designing"); serr != nil {
			log.Printf("[wiki-generate] failed to promote status to designing for %s: %v", id, serr)
		}
		req.Status = "designing"
	}

	// Job + job_id pre-mint, same shape as architect-design: persist
	// wiki_job_id via a sentinel column. There is no WikiJobID on the
	// model today (the column was deliberately not added to keep the
	// schema minimal), so we only persist the job in JobStore; a
	// server restart loses the active pointer, but the user can re-run
	// from the wiki tab — the session is preserved.
	job := h.jobs.Create(id)
	job.SetType("wiki_generate")

	// Persona: load the wiki role (system prompt + default model) via
	// the shared roleConfig helper. roleConfig returns "" when the user
	// has not configured a wiki role — the CLI then falls back to the
	// default persona, which is acceptable.
	systemPrompt, model, claudeConfigID := h.roleConfig("wiki")
	if modelOverride != "" {
		model = modelOverride
	}
	claudeConfigID = h.resolveConfigIDForRun(claudeConfigIDOverride, model, claudeConfigID)
	job.SetModel(model)

	return &wikiRunParams{
		Req:            req,
		ProjectPath:    projectPath,
		DefaultBranch:  defaultBranch,
		NewWikiSID:     newWikiSID,
		ResumeSID:      resumeSID,
		WorkDir:        "", // populated by prepareWikiWorkspace in the goroutine
		Prompt:         "",
		SystemPrompt:   systemPrompt,
		Model:          model,
		ClaudeConfigID: claudeConfigID,
		ReadKnowledge:  readKnowledge,
		AgentServerID:  agentServerID,
	}, job, nil
}

// prepareWikiWorkspace is the goroutine-side counterpart of prepareWikiDoc.
// It owns the parts that have to live off the HTTP request thread so the
// SSE panel can stream them live: hard git sync to origin/<default_branch>,
// worktree anchor, plan-mode prompt construction.
//
// Returns true on success (p.WorkDir and p.Prompt populated), false on any
// failure (the job has already been Finished with JobError; the caller
// should return immediately).
func (h *WizardHandler) prepareWikiWorkspace(p *wikiRunParams, job *store.Job) bool {
	job.Append(store.LogLine{Type: "phase", Content: "🔄 同步仓库基线到 origin/" + p.DefaultBranch + "…"})

	timeout, _ := h.settingSvc.GitSyncTimeout()
	workDir, _, err := h.resolveWorkDirSynced(context.Background(), p.Req, p.ProjectPath, p.DefaultBranch, timeout, func(s string) {
		job.Append(store.LogLine{Type: "message", Content: s})
	})
	if err != nil {
		// Mirror architect-design's gate-error formatting.
		var gateErr *service.SyncGateError
		if errors.As(err, &gateErr) {
			job.Append(store.LogLine{Type: "error", Content: gateErr.Msg})
			if gateErr.Stderr != "" {
				job.Append(store.LogLine{Type: "error", Content: gateErr.Stderr})
			}
		} else {
			job.Append(store.LogLine{Type: "error", Content: err.Error()})
		}
		job.Finish(1, store.JobError)
		return false
	}
	p.WorkDir = workDir

	// Plan-mode task prompt. The wiki kind skips the analyst stage so we
	// always seed a fresh-context conversation (or, on a re-run, the
	// resumed session already carries the prior knowledge-doc context).
	// The wiki block (appended below) is the textual hard constraint;
	// DisallowedTools is the structural one.
	prompt := "## 需求标题\n" + p.Req.Title + "\n\n" +
		"现在切换到「知识库」角色。请阅读项目相关源文件，" +
		"基于需求描述与项目现状，沉淀一份可复用的知识库文档（Markdown 正文）。" +
		"文档应涵盖：背景与目标、关键概念、相关源文件清单、典型使用方式 / 调用路径、风险与注意事项。" +
		"请先复述你对需求的理解，再输出文档正文。"
	if p.ResumeSID == "" && p.Req.AnalystContextSummary == "" {
		// Fresh-session path: prepend project structure / docs pre-read so
		// the model has the project shape without burning a turn on `ls`.
		docBlock, _, treeSummary := collectProjectContext(workDir, p.Req.Title)
		prompt = "现在切换到「知识库」角色。请阅读相关源文件，沉淀一份可复用的知识库文档。\n\n" +
			"## 需求标题\n" + p.Req.Title + "\n\n" +
			"## 需求描述\n" + p.Req.Description + "\n\n" +
			"## 项目上下文\n" + docBlock + "\n" + treeSummary + "\n\n" +
			"文档应涵盖：背景与目标、关键概念、相关源文件清单、典型使用方式 / 调用路径、风险与注意事项。\n" +
			"请先复述你对需求的理解，再输出文档正文。"
	}
	// Tail: append the wiki kind-specific block (read-only hard constraint).
	if block := promptpkg.WikiBlock(p.Req.Kind, p.Req); block != "" {
		prompt += "\n\n" + block
	}
	p.Prompt = prompt
	return true
}

// execWikiDoc runs the goroutine body of the wiki-doc stage. It is the
// local-only counterpart of the architect pattern: there is no remote
// branch today (wiki doesn't run on agent servers), so the body is a
// single plan-mode claude invocation followed by persistence. Future
// expansion to a remote branch is a drop-in `else if` next to the local
// branch (mirroring runRemoteArchitectDesign).
func (h *WizardHandler) execWikiDoc(p *wikiRunParams, job *store.Job, cb *runCallbacks) {
	defer func() {
		lines, status, exitCode := job.Snapshot()
		if perr := h.jobLogSvc.Save(job.ID, p.Req.ID, string(status), exitCode, job.StartedAt, job.FinishedAt, lines, job.Model); perr != nil {
			log.Printf("[wiki-generate] failed to persist job log %s: %v", job.ID, perr)
		}
		if cb != nil && cb.OnFinish != nil {
			cb.OnFinish(job.ID, status == store.JobDone)
		}
	}()

	if !h.prepareWikiWorkspace(p, job) {
		return
	}

	id := p.Req.ID
	model := p.Model
	claudeConfigID := p.ClaudeConfigID
	prompt := p.Prompt
	req := p.Req
	workDir := p.WorkDir

	log.Printf("[wiki-generate] job %s started for %s", job.ID, id)

	// Optional knowledge pre-read: inject the project knowledge relevant
	// to this requirement and surface what was read via a "knowledge" SSE
	// event. Default off (read_knowledge=false) keeps the legacy
	// behavior untouched.
	if p.ReadKnowledge {
		if kbBlock, kbTitles := h.buildKnowledgeBlock(req.ProjectID, req.Title); kbBlock != "" {
			prompt = kbBlock + "\n" + prompt
			emitKnowledgeEvent(job, kbTitles)
		}
	}

	// Session threading: resume the stored wiki session when one exists,
	// otherwise seed the freshly-minted new id.
	sessionArg := p.ResumeSID
	forkSessionID := ""
	if sessionArg == "" {
		sessionArg = p.NewWikiSID
	}

	job.Append(store.LogLine{Type: "phase", Content: "📚 Claude 正在 plan 模式下阅读代码并生成知识库文档..."})
	cmd := h.llm.StreamCmd(context.Background(), llm.StreamOpts{
		Prompt:          prompt,
		WorkDir:         workDir,
		SystemPrompt:    p.SystemPrompt,
		Model:           cliModelArg(model),
		ClaudeConfigID:  claudeConfigID,
		SessionID:       sessionArg,
		Resume:          p.ResumeSID != "",
		Fork:            false,
		ForkSessionID:   forkSessionID,
		PermissionMode:  "plan",
		DisallowedTools: wikiDisallowedTools,
	})
	out := runClaudeStream(jobSink{job}, cmd, "wiki-generate",
		h.usageCtxForConfig("wiki_generate", id, req.ProjectID, job.ID, model, "", "", claudeConfigID),
		nil,
		nil,
		0,
		wikiStallTimeout,
	)

	h.finalizeWikiRun(out, p, job, id, model)
}

// finalizeWikiRun is the terminal-state handler for the wiki-doc stage.
// Mirrors finalizeArchitectRun's split (stale session / error / success)
// but persists to wiki_docs / wiki_session_id and never touches
// design_docs. Wiki has no analyst-stage fallback, so the stale-session
// branch is simpler: just clear the stored id and ask the user to retry.
func (h *WizardHandler) finalizeWikiRun(
	out claudeStreamOutcome,
	p *wikiRunParams,
	job *store.Job,
	id, model string,
) {
	if out.staleSession {
		// Clear the stale wiki session id so the next run mints a fresh
		// one. The wiki flow has no analyst stage to roll back to.
		_ = h.reqSvc.UpdateWikiSession(id, "")
		job.Append(store.LogLine{Type: "error", Content: "知识库会话已过期，请重新生成知识库文档。"})
		job.Finish(1, store.JobError)
		return
	}

	// Correct the pre-minted session id if the CLI reported a different
	// one (parity with finalizeArchitectRun's safety net).
	if out.sessionID == "" {
		if p.NewWikiSID != "" {
			if perr := h.reqSvc.UpdateWikiSession(id, ""); perr != nil {
				log.Printf("[wiki-generate] failed to clear unestablished wiki session for %s: %v", id, perr)
			}
		}
	} else if out.sessionID != p.NewWikiSID && out.sessionID != p.ResumeSID {
		if perr := h.reqSvc.UpdateWikiSession(id, out.sessionID); perr != nil {
			log.Printf("[wiki-generate] failed to persist wiki session for %s: %v", id, perr)
		}
	}

	// In plan mode, the full plan markdown is captured from the Write
	// tool_use event that lands in ~/.claude/plans/*.md. Fall back to
	// the result text if capture missed it.
	planMarkdown := out.planContent
	if planMarkdown == "" {
		planMarkdown = out.finalResult
	}
	// Mid-stream failure with partial plan: persist the partial as a
	// fallback so the exploration work isn't lost, but mark the job
	// errored so the user knows to retry.
	if out.errMsg != "" {
		if planMarkdown != "" {
			if _, err := h.reqSvc.UpdateWikiDoc(id, planMarkdown); err != nil {
				log.Printf("[wiki-generate] failed to save partial wiki doc for %s: %v", id, err)
			}
		}
		job.Append(store.LogLine{Type: "error", Content: out.errMsg})
		job.Finish(1, store.JobError)
		return
	}
	if planMarkdown == "" {
		errMsg := out.errMsg
		if errMsg == "" {
			errMsg = "Claude 未返回结果，请重试"
		}
		job.Append(store.LogLine{Type: "error", Content: errMsg})
		job.Finish(1, store.JobError)
		return
	}

	// Persist the wiki doc (sets status=designing) and clear the active
	// job pointer so a refresh shows the finished doc instead of
	// "executing".
	if _, err := h.reqSvc.UpdateWikiDoc(id, planMarkdown); err != nil {
		log.Printf("[wiki-generate] failed to save wiki doc for %s: %v", id, err)
		job.Append(store.LogLine{Type: "error", Content: "保存知识库文档失败: " + err.Error()})
		job.Finish(1, store.JobError)
		return
	}

	// Auto-promote designing → designed so the user immediately sees the
	// 微调文档 / 应用文档 / 归档到知识库 CTAs (no manual "知识库完成" gate
	// is required — wiki docs are single-shot, unlike technical designs
	// which the user reviews before declaring complete). Mirrors
	// finalizeArchitectRun's auto-promote.
	if _, perr := h.reqSvc.UpdateStatus(id, "designed"); perr != nil {
		log.Printf("[wiki-generate] auto-promote %s to designed failed: %v", id, perr)
	}

	job.Append(store.LogLine{Type: "done", Content: "✅ 知识库文档已生成！"})
	job.Finish(0, store.JobDone)
	log.Printf("[wiki-generate] job %s finished for %s", job.ID, id)
}
