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
//   - SanitizeDesignDoc (only strips outer ```markdown``` fence; Mermaid
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
	"strings"
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
//
// "Task" is added so Claude cannot launch Explore / Plan sub-agents inside a
// wiki run — those sub-agents can hang waiting for each other (their stream
// never gets a final result) and the parent turn ends with a one-line
// "等待回收中…" preamble instead of the actual plan. We want Claude to read
// files directly and emit the doc in a single turn.
var wikiDisallowedTools = []string{"Write", "Edit", "NotebookEdit", "Task"}

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
	// wiki_job_id BEFORE the goroutine spawns so a page refresh during
	// the in-flight run can reconnect to the live SSE stream via the
	// frontend's streamWikiJob-on-mount logic. Cleared on every terminal
	// path (success / error / stale session) inside finalizeWikiRun so
	// the next run can mint a fresh id. healStaleJobs also self-heals a
	// stale pointer when the JobStore no longer holds the job (e.g. the
	// server restarted between the DB write and the clearing call).
	job := h.jobs.Create(id)
	job.SetType("wiki_generate")
	if perr := h.reqSvc.UpdateWikiJob(id, job.ID); perr != nil {
		log.Printf("[wiki-generate] failed to persist wiki_job_id for %s: %v", id, perr)
	}

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
	//
	// Anti-pattern note (req_b37e6f385d371b7e): a prior version of this
	// prompt ended with "请先复述你对需求的理解，再输出文档正文" — Claude
	// interpreted "复述" as "structure summary" and emitted a numbered
	// TOC (1. 背景与目标：... 2. 关键概念：...) instead of the actual
	// body. The backend's looksLikeWikiOutline heuristic now catches that
	// shape, but the cleaner fix is to forbid the recap entirely here so
	// the model doesn't burn a turn producing throwaway output.
	prompt := "## 需求标题\n" + p.Req.Title + "\n\n" +
		"现在切换到「知识库」角色。请直接用 Read / Glob / Grep 阅读项目相关源文件（不要启动子代理），" +
		"基于需求描述与项目现状，一次性沉淀出一份完整的、可复用的知识库文档（Markdown 正文）。" +
		"文档应涵盖：背景与目标、关键概念、相关源文件清单、典型使用方式 / 调用路径、风险与注意事项；" +
		"每一节都必须填实（具体文件路径、关键函数签名、Mermaid 块或代码片段），禁止以一句话标签收尾。" +
		"不要发\"我已启动\"、\"等待回收中\"之类的进度消息，也不要\"先复述需求 / 列大纲\"再写正文——直接给出最终 Markdown 全文。"
	if p.ResumeSID == "" && p.Req.AnalystContextSummary == "" {
		// Fresh-session path: prepend project structure / docs pre-read so
		// the model has the project shape without burning a turn on `ls`.
		docBlock, _, treeSummary := collectProjectContext(workDir, p.Req.Title)
		prompt = "现在切换到「知识库」角色。请直接用 Read / Glob / Grep 阅读相关源文件（不要启动子代理），一次性沉淀出一份完整的、可复用的知识库文档。\n\n" +
			"## 需求标题\n" + p.Req.Title + "\n\n" +
			"## 需求描述\n" + p.Req.Description + "\n\n" +
			"## 项目上下文\n" + docBlock + "\n" + treeSummary + "\n\n" +
			"文档应涵盖：背景与目标、关键概念、相关源文件清单、典型使用方式 / 调用路径、风险与注意事项；每一节都必须填实（具体文件路径、关键函数签名、Mermaid 块或代码片段），禁止以一句话标签收尾。\n" +
			"不要发\"我已启动\"、\"等待回收中\"之类的进度消息，也不要\"先复述需求 / 列大纲\"再写正文——直接给出最终 Markdown 全文。"
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
	// Clear wiki_job_id on every terminal path so a page refresh after the
	// run finishes doesn't try to reconnect to a dead JobStore slot. The
	// goroutine returns after this function, so a single defer covers every
	// branch (success, stale session, error, preamble rejection, empty
	// sanitize, UpdateWikiDoc failure). The persistent log on
	// execWikiDoc's deferred line still fires independently — clearing
	// wiki_job_id here doesn't disturb the job_logs persistence.
	defer func() {
		if cerr := h.reqSvc.UpdateWikiJob(id, ""); cerr != nil {
			log.Printf("[wiki-generate] failed to clear wiki_job_id for %s: %v", id, cerr)
		}
	}()

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
	// the result text if capture missed it, then to the cumulative
	// streamed text as a last resort (see claudeStreamOutcome.streamText
	// in wizard_stream.go — non-Anthropic proxies and ExitPlan exits in
	// plan mode often leave planContent + finalResult both empty).
	planMarkdown := out.planContent
	if planMarkdown == "" {
		planMarkdown = out.finalResult
	}
	if planMarkdown == "" {
		planMarkdown = out.streamText
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
			errMsg = "Claude 未返回可保存的知识库正文，请重试"
		}
		job.Append(store.LogLine{Type: "error", Content: errMsg})
		job.Finish(1, store.JobError)
		return
	}

	// Sanitize in-place BEFORE the SQL write so we can short-circuit on
	// empty (e.g. planContent was a bare "```markdown\n```" fence or some
	// other shape that SanitizeDesignDoc reduces to ""). Without this
	// guard we used to write empty wiki_docs AND flip status to 'designed'
	// — leaving the row in an unrecoverable inconsistent state where
	// "归档到知识库" errors "wiki-kind requirement has no document yet".
	// Doing it here (in addition to the same guard inside UpdateWikiDoc)
	// is belt-and-braces: even if the service-level guard is removed in
	// the future, the handler-side check still prevents the corruption.
	cleaned := service.SanitizeDesignDoc(planMarkdown)
	if cleaned == "" {
		job.Append(store.LogLine{Type: "error", Content: "Claude 未返回可保存的知识库正文，请重试"})
		job.Finish(1, store.JobError)
		// Leave status at 'designing' so the user can retry from the CTA
		// instead of staring at a permanently broken "归档" button.
		return
	}

	// Mermaid 11.x syntax repair. Mermaid tightened its lexer in 10.x /
// 11.x and several constructs the upstream model emits (semicolons
// inside sequenceDiagram messages, parallelogram labels with a
// missing closing '/', square labels with "(", ")", "{", "}" or
// stray '"') cause the renderer to surface "Syntax error in text" at
// the bottom of the doc. SanitizeMermaidBlocks applies three small,
// empirically validated fixes — see the package comment in
// service/mermaid.go for the full rationale and the failure modes
// observed on req_b37e6f385d371b7e.
	cleaned = service.SanitizeMermaidBlocks(cleaned)

	// Preamble-rejection heuristic. Even with DisallowedTools=[...,Task] and
	// the strengthened prompt forbidding sub-agents, a misbehaving model can
	// still stream a one-line status message ("我已并行启动 2 个 Explore
	// 子代理深入调研 ... 等待回收中…") and end its turn there without ever
	// producing the actual document. The tier-3 streamText fallback would
	// otherwise grab this preamble and persist it as the wiki doc — the user
	// would see a useless 1-line message instead of the real content and
	// "归档到知识库" would happily archive it.
	//
	// Reject when the cleaned content is suspiciously short AND lacks any
	// markdown structure (no heading, no code block, no list, no table).
	// A real knowledge doc almost always has at least one of these.
	if looksLikeWikiPreamble(cleaned) {
		log.Printf("[wiki-generate] rejected preamble for %s (len=%d): %q", id, len(cleaned), truncateForLog(cleaned, 80))
		job.Append(store.LogLine{Type: "error", Content: "Claude 未输出可保存的知识库正文（疑似只发了进度消息），请重试"})
		job.Finish(1, store.JobError)
		// Leave status at 'designing' so the user can retry.
		return
	}

	// Outline-pattern detection. Catches a different failure mode than
	// looksLikeWikiPreamble: a long, structurally-marked-up "table of
	// contents" reply that lists what the doc *would* contain but never
	// produces the body itself. Canonical example (req_b37e6f385d371b7e):
	//
	//   知识库正文已完整输出。文档覆盖：
	//   1. **背景与目标**：MCP 在 Controller 内的定位...
	//   2. **关键概念**：模块分层图、三道门...
	//   ... (no Mermaid block, no file paths, no real content)
	//
	// The preamble heuristic missed this (1073 chars > 200, has list
	// structure). The outline heuristic rejects when EITHER a "self-praise"
	// tell is present OR the doc has 3+ label-only list items AND no
	// concrete markers (backticks / fences). See looksLikeWikiOutline
	// below for the exact rules.
	if looksLikeWikiOutline(cleaned) {
		log.Printf("[wiki-generate] rejected outline for %s (len=%d): %q", id, len(cleaned), truncateForLog(cleaned, 80))
		job.Append(store.LogLine{Type: "error", Content: "Claude 仅输出了纲要 / 进度描述，未输出可保存的知识库正文，请重试"})
		job.Finish(1, store.JobError)
		// Leave status at 'designing' so the user can retry.
		return
	}

	// Persist the wiki doc (sets status=designing) and clear the active
	// job pointer so a refresh shows the finished doc instead of
	// "executing".
	if _, err := h.reqSvc.UpdateWikiDoc(id, cleaned); err != nil {
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

// looksLikeWikiPreamble reports whether the given cleaned content looks more
// like a Claude status / progress message than an actual knowledge-base
// document. The heuristic is intentionally conservative (prefers false
// negatives to false positives — we only reject when BOTH signals fire):
//
//   1. Content is suspiciously short (< 200 chars after sanitize). A real
//      knowledge doc covering 背景与目标 / 关键概念 / 相关源文件清单 / 调用
//      路径 / 风险与注意事项 is virtually always longer.
//   2. Content lacks ANY markdown structure — no heading (#/##/###),
//      no code fence, no list item (- / 1. / *), no table row (|).
//
// The combined rule keeps the false-positive rate near zero: a 50-char
// reply that happens to include a `# 标题` line still passes, and a long
// prose reply without headings but with bullet points still passes.
//
// Origin: claude in plan mode can emit a one-line preamble ("我已并行启动
// 2 个 Explore 子代理深入调研 ... 等待回收中…") and end its turn there
// without producing the doc. The tier-3 streamText fallback in
// finalizeWikiRun would otherwise grab this preamble and persist it as the
// wiki doc — silently corrupting the requirement row. We catch it here.
func looksLikeWikiPreamble(cleaned string) bool {
	if len(cleaned) >= 200 {
		return false
	}
	// Strip leading whitespace, then look for any structural marker.
	trimmed := strings.TrimLeft(cleaned, " \t\n\r")
	hasHeading := strings.HasPrefix(trimmed, "#") ||
		strings.Contains(trimmed, "\n#") ||
		strings.Contains(trimmed, "\n##")
	hasCodeFence := strings.Contains(cleaned, "```")
	hasList := false
	for _, line := range strings.Split(cleaned, "\n") {
		l := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(l, "- ") || strings.HasPrefix(l, "* ") ||
			(len(l) > 2 && l[0] >= '0' && l[0] <= '9' && (l[1] == '.' || l[1] == ')') && l[2] == ' ') {
			hasList = true
			break
		}
	}
	hasTable := strings.Contains(cleaned, "|") && strings.Contains(cleaned, "\n")
	return !hasHeading && !hasCodeFence && !hasList && !hasTable
}

// looksLikeWikiOutline reports whether the cleaned content is a
// "table-of-contents-only" emission — a structurally-marked-up reply that
// *lists* what the document would cover but never actually delivers the
// body. This is a different failure mode than looksLikeWikiPreamble
// (which catches one-line progress messages); outline-shaped output is
// long, has list/heading structure, and slips past the preamble guard.
//
// Reject when ANY of these fires:
//
//   (c) "Self-praise tell" — the model openly announces it has produced
//       the doc, even though all it actually emitted is a TOC. Catches
//       the canonical failure phrase "知识库正文已完整输出。文档覆盖：..."
//       as well as siblings like "包含 N 张图" / "包含 N 个表" / "以下章节"
//       used as a preamble to a list rather than as actual section
//       headers. This branch alone is sufficient to reject.
//
//   (a) "Label-only outline" — three or more list items whose body is
//       a one-clause label terminated by `：` (or `:`). The label regex
//       matches `1. **Foo**：bar`, `- **Foo**：bar`, `* **Foo**: bar`,
//       etc. ANDed with:
//
//   (b) "No concrete markers" — fewer than 4 backticks total in the
//       entire doc (no inline code, no ``` fences, no file paths).
//       A real knowledge doc virtually always has at least one of those.
//
// Branch (a)+(b) together reject the "long TOC with markdown structure"
// case; branch (c) catches the variant where the model labels the
// preamble with a self-praise sentence before the TOC.
//
// Origin: req_b37e6f385d371b7e (the wiki doc was a 1073-char TOC about
// "MCP in Controller" — no Mermaid block, no file paths, no real
// content). The preamble heuristic passed it (1073 > 200 + has list),
// so this second heuristic was added.
func looksLikeWikiOutline(cleaned string) bool {
	if cleaned == "" {
		return false
	}
	// Branch (c): self-praise tells. Each phrase is what the model emits
	// right before (or instead of) the actual body. Trim whitespace
	// defensively so a stray leading newline doesn't dodge the match.
	trimmed := strings.TrimSpace(cleaned)
	for _, tell := range wikiOutlineSelfPraiseTells {
		if strings.Contains(trimmed, tell) {
			return true
		}
	}
	// Branch (a)+(b): label-only outline AND no concrete markers.
	backtickCount := strings.Count(cleaned, "`")
	if backtickCount >= 4 {
		return false
	}
	labelItem := 0
	for _, line := range strings.Split(cleaned, "\n") {
		l := strings.TrimLeft(line, " \t")
		if isWikiOutlineLabelItem(l) {
			labelItem++
		}
	}
	return labelItem >= 3
}

// wikiOutlineSelfPraiseTells are phrases that, when present, indicate
// the model emitted a meta-description of the doc instead of the doc
// itself. Substring match — any of these in the cleaned content is
// sufficient (alone) to reject.
var wikiOutlineSelfPraiseTells = []string{
	"知识库正文已完整输出",
	"文档覆盖：",
	"文档覆盖:",
	"包含一张图速记",
	"包含一张速记图",
	"包含一张图",
	"包含两张",
	"包含三张",
	"包含 N 张",
	"以下章节将",
	"下面章节将",
	"本文档涵盖",
}

// isWikiOutlineLabelItem reports whether a single trimmed line matches
// the "label-only outline item" pattern: a list bullet (number, dash,
// or asterisk) followed by an emphasized title and a Chinese-colon or
// ASCII-colon separator. Examples that match:
//
//	1. **背景与目标**：MCP 在 Controller 内的定位...
//	- **关键概念**：模块分层图...
//	* **源文件清单**：按层...
//
// Examples that DO NOT match (a real section header without a label
// pattern, or prose without a list marker): "# 背景与目标", "本节介绍
// 模块分层图".
func isWikiOutlineLabelItem(trimmed string) bool {
	if trimmed == "" {
		return false
	}
	// Strip the leading list marker. We accept "1.", "1)", "-", "*".
	rest := trimmed
	switch {
	case len(rest) >= 3 && rest[0] >= '0' && rest[0] <= '9' && (rest[1] == '.' || rest[1] == ')') && rest[2] == ' ':
		rest = rest[3:]
	case len(rest) >= 2 && rest[0] == '-' && rest[1] == ' ':
		rest = rest[2:]
	case len(rest) >= 2 && rest[0] == '*' && rest[1] == ' ':
		rest = rest[2:]
	default:
		return false
	}
	// Require an emphasized title: "**...**" or "**...**：" right after
	// the marker. We deliberately don't try to match the whole title
	// shape — `**` is enough signal.
	if !strings.HasPrefix(rest, "**") {
		return false
	}
	// Require a colon (Chinese full-width or ASCII) somewhere after the
	// title's closing "**". We accept either, since models vary.
	closer := strings.Index(rest[2:], "**")
	if closer < 0 {
		return false
	}
	tail := rest[2+closer+2:]
	return strings.Contains(tail, "：") || strings.Contains(tail, ":")
}
