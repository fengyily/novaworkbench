// Package handler — wizard pipeline (requirement → code).
//
// wizard_architect.go is the architect-stage design generator:
// the ArchitectDesign HTTP entry point, the scheduler-facing
// RunScheduledDesign, and the shared prepare/exec body. The design is
// captured from Claude's plan-mode stream (see wizard_stream.go) and
// persisted to design_docs. See wizard_common.go for shared helpers,
// wizard_stream.go for the CLI stream parser, and wizard_jobs.go for
// job introspection endpoints.

package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	promptpkg "github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

// ArchitectDesign is the architect-phase design generator. It creates a
// background JobStore job, persists its id on the requirement (so a page refresh
// can reconnect to the running job and show "executing" instead of the start
// button), and returns the job id immediately. Claude then runs in plan mode
// (stream-json) in a goroutine, writing progress into the job. On success the
// plan markdown is persisted to design_docs and the design_job_id is cleared;
// the requirement stays status=designing until the user manually marks 方案完成.
//
// Subscribe to the live stream via GET /api/wizard/jobs/{job_id}/stream and
// poll the snapshot via GET /api/wizard/jobs/{job_id} (same pattern as
// start-coding).
// designRunParams is the prepared-shape output of prepareArchitectDesign —
// every input the goroutine body needs after the synchronous validation +
// job creation + prompt build has succeeded. Splitting it out lets the
// scheduler path reuse the exact same exec body via RunScheduledDesign, so
// HTTP-driven and time-driven dispatches share one implementation of the
// architect stage (no parallel maintenance).
type designRunParams struct {
	Req            *model.Requirement
	ProjectPath    string
	DefaultBranch  string
	SourceSID      string
	Fork           bool
	SkipAnalysis   bool
	NewDesignSID   string
	WorkDir        string
	Prompt         string
	SystemPrompt   string
	Model          string // 已经应用请求体覆盖
	ClaudeConfigID string
	ReadKnowledge  bool
	// AgentServerID — agent_servers.id chosen in the design toolbar
	// dropdown (mirrors the dev-stage picker). Empty means the local
	// branch stays in effect; non-empty routes the run through
	// runRemoteArchitectDesign which dispatches the plan-mode claude
	// invocation to that Agent server over SSH.
	AgentServerID string
}

func (h *WizardHandler) ArchitectDesign(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequirementID string `json:"requirement_id"`
		Model         string `json:"model"`
		// ClaudeConfigID — user-picked claude_configs row id (UI ModelSelect);
		// empty = backend resolves via the priority chain in
		// resolveConfigIDForRun.
		ClaudeConfigID string `json:"claude_config_id"`
		// AgentServerID — agent_servers.id for the remote plan-mode run;
		// empty = backend stays on the local claude CLI invocation. Placed
		// adjacent to ClaudeConfigID so the JSON wire format stays grouped
		// (model / config / runtime env).
		AgentServerID string `json:"agent_server_id"`
		ReadKnowledge bool   `json:"read_knowledge"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	p, job, af := h.prepareArchitectDesign(body.RequirementID, body.Model, body.ClaudeConfigID, body.AgentServerID, body.ReadKnowledge)
	if writeIfAPIError(w, af) {
		return
	}
	writeJSON(w, 200, map[string]string{"job_id": job.ID})
	go h.execArchitectDesign(p, job, nil)
}

// RunScheduledDesign is the scheduler-facing entry point. It performs the
// same prepare step as ArchitectDesign (the validation is shared) and
// dispatches the exec body in a goroutine tagged with the scheduler's
// callback. Returns the JobStore job id (the scheduler records this in
// scheduled_tasks.job_id so /api/wizard/jobs/{id} can replay the log).
//
// agentServerID is the scheduled_tasks.agent_server_id value the scheduler
// already extracted at dispatch time (the scheduled task stores it on its
// own row); empty = local execution. Empty today is the common case —
// schedule_executor.go forwards p.AgentServerID directly.
func (h *WizardHandler) RunScheduledDesign(requirementID, model string, readKnowledge bool, agentServerID string, cb *runCallbacks) (string, error) {
	p, job, af := h.prepareArchitectDesign(requirementID, model, "", agentServerID, readKnowledge)
	if af != nil {
		return "", af
	}
	go h.execArchitectDesign(p, job, cb)
	return job.ID, nil
}

// prepareArchitectDesign runs the synchronous portion of the architect-design
// stage: requirement lookup, project path / default branch resolution,
// session-thread resolution (design→analyst fallback), anchor check,
// session-id pre-mint, worktree creation, JobStore creation, and prompt
// construction. Returns (*designRunParams, *store.Job, *apiFailure); any
// non-nil apiFailure is a 4xx/5xx the HTTP path writes verbatim. The exec
// body (execArchitectDesign) consumes the params + job and writes progress
// into JobStore.
//
// NOTE: this is a refactor of the original ArchitectDesign — the body is
// line-for-line the same as the original synchronous section; only the
// writeError calls have been replaced with returning *apiFailure so the
// scheduler can reuse the same validation outcomes.
func (h *WizardHandler) prepareArchitectDesign(requirementID, modelOverride, claudeConfigIDOverride string, agentServerID string, readKnowledge bool) (*designRunParams, *store.Job, *apiFailure) {
	id := requirementID
	if id == "" {
		return nil, nil, fail(400, "INVALID", "missing requirement id")
	}

	req, err := h.reqSvc.Get(id)
	if err != nil {
		return nil, nil, fail(404, "NOT_FOUND", "requirement not found")
	}
	project, _ := h.projectSvc.Get(req.ProjectID)
	projectPath := ""
	defaultBranch := ""
	if project != nil {
		projectPath = project.LocalPath
		defaultBranch = project.DefaultBranch
	}

	// Prologue: persist the design-stage agent-server binding BEFORE any
	// SSH / worktree work so a later failure still leaves a record of what
	// the user picked. Mirrors execStartCoding's UpdateDevSource prologue
	// (wizard_coding.go:157-187). Empty input clears the column so a
	// requirement that flips back to local execution doesn't keep
	// routing follow-up "继续设计" actions to a stale server.
	if uerr := h.reqSvc.UpdateDesignAgentServer(id, agentServerID); uerr != nil {
		log.Printf("[architect-design] failed to persist design_agent_server_id for %s: %v", id, uerr)
	}

	// Session threading: the architect stage continues the SAME conversation
	// thread as the analyst. On the first design pass we fork off the analyst
	// session (--resume <analysis_sid> --fork-session) so the architect inherits
	// the full analysis discussion and swaps in the architect persona via
	// --system-prompt; the forked session gets a new id we read back from the
	// stream and persist as design_session_id. On a re-run (design_session_id
	// already set) we just --resume it. We do NOT re-feed Description /
	// AcceptanceCriteria — the resumed conversation already has them.
	sourceSID := ""
	fork := false
	if req.DesignSessionID != "" {
		sourceSID = req.DesignSessionID
	} else if req.AnalysisSessionID != "" {
		sourceSID = req.AnalysisSessionID
		fork = true
	}
	// Skip-analysis path: when skip_analysis is set and there is no analyst
	// session to fork, run architect-design as a FRESH claude conversation
	// (no --resume/--fork-session) seeded only with the requirement title/
	// description + collectProjectContext. This bypasses the analyst stage.
	skipAnalysis := req.SkipAnalysis
	if sourceSID == "" && !skipAnalysis {
		return nil, nil, fail(400, "NO_SESSION", "尚未找到需求分析会话，请先完成「需求分析」再生成技术方案。")
	}

	// Forking the analyst session requires it to have been anchored to the
	// isolated worktree — otherwise the design (and later the coding stage that
	// forks the design) inherits original-dir absolute paths.
	if fork {
		if gerr := h.requireAnchoredFork(req, projectPath); gerr != nil {
			return nil, nil, fail(409, "UNANCHORED_SESSION", gerr.Error())
		}
	}

	// Pre-mint + persist the design session id BEFORE spawning claude so it
	// survives a mid-run restart. Two cases produce a NEW id: forking off the
	// analyst session (--resume <src> --fork-session --session-id <new>) and the
	// skip-analysis fresh session (--session-id <new>). A re-run just resumes the
	// stored design_session_id (no new id).
	newDesignSID := ""
	if fork || sourceSID == "" {
		newDesignSID = util.NewUUID()
		if perr := h.reqSvc.UpdateDesignSession(id, newDesignSID); perr != nil {
			log.Printf("[architect-design] failed to persist design session for %s: %v", id, perr)
		}
	}

	// Create the job BEFORE resolveWorkDirLogged so the log echo closure can
	// fan the worktree sync lines ("🔄 已同步 origin/<base>" etc.) into the SSE
	// panel. We persist design_job_id now and clear it on terminal failure
	// paths below; any fail(...) above (req not found, no source session,
	// unanchored fork) returns before reaching here so a "stuck" design_job_id
	// never leaks across runs.
	job := h.jobs.Create(id)
	job.SetType("architect_design")
	if perr := h.reqSvc.UpdateDesignJob(id, job.ID); perr != nil {
		log.Printf("[architect-design] failed to persist design_job_id for %s: %v", id, perr)
	}

	// Anchor the architect stage to the isolated worktree (created here if the
	// analyst stage was skipped) so the plan and its session are rooted in the
	// worktree — otherwise the coding stage forks this session and follows the
	// original-dir absolute paths back to the shared checkout.
	//
	// The plan-mode claude run happens in a goroutine writing progress into the
	// job store (execArchitectDesign). Using resolveWorkDirLogged (instead of
	// the log-silent resolveWorkDir) means the user sees the worktree sync
	// lines in the Job panel as soon as the architect stage starts — the
	// alternative (deferred to coding) would leave them blind during the design
	// pass.
	workDir, wdErr := h.resolveWorkDirLogged(req, projectPath, defaultBranch, func(s string) {
		job.Append(store.LogLine{Type: "message", Content: s})
	})
	if wdErr != nil {
		// Roll back the design_job_id so the next attempt can mint a fresh
		// job; the user shouldn't see a dead "executing" pointer after the
		// worktree stage failed.
		_ = h.reqSvc.UpdateDesignJob(id, "")
		return nil, nil, fail(500, "WORKTREE_FAILED", "worktree 创建失败："+wdErr.Error())
	}

	// Plan-mode task prompt. When resuming/forking an existing conversation
	// (analyst session present), the resumed thread already carries the
	// requirement and its analysis — we just ask Claude to switch to the
	// architect role and produce a plan. On the skip-analysis path (no
	// session) we must seed the fresh conversation with the requirement plus
	// pre-read project context, since there is no prior discussion to inherit.
	prompt := "现在切换到「架构师」角色。基于我们刚才完成的需求分析对话，" +
		"请阅读项目相关源文件核实技术细节，制定具体可执行的技术实现方案（plan）。" +
		"方案应涵盖：整体实现思路、需要新增或修改的文件、具体实现步骤、数据模型/数据库变更、实现风险及应对。"
	if skipAnalysis && sourceSID == "" {
		docBlock, _, treeSummary := collectProjectContext(workDir, req.Title)
		prompt = "现在切换到「架构师」角色。请基于以下需求与项目信息，阅读相关源文件核实技术细节，" +
			"制定具体可执行的技术实现方案（plan）。\n\n" +
			"## 需求标题\n" + req.Title + "\n\n" +
			"## 需求描述\n" + req.Description + "\n\n" +
			"## 项目上下文\n" + docBlock + "\n" + treeSummary + "\n\n" +
			"方案应涵盖：整体实现思路、需要新增或修改的文件、具体实现步骤、数据模型/数据库变更、实现风险及应对。" +
			"请先复述你对需求的理解，再给出方案。"
		// Context-compression handoff (fresh-session path only): when the
		// design stage was previously compressed, the requirement carries a
		// Chinese summary we want the architect to see as scene-setting
		// context. We only inject on the fresh-session path because the
		// resume path (skipAnalysis==false) inherits the analyst conversation
		// natively via --resume, where the prior design summary isn't
		// applicable. The prefix also goes BEFORE the rest of the prompt so
		// the model treats it as ground truth rather than as a post-hoc
		// addendum, and the parenthetical disclaimer discourages the model
		// from acting on it as if it were a fresh instruction.
		if req.DesignContextSummary != "" {
			prompt = "## 上下文压缩摘要（之前的方案设计对话已被压缩，请基于此继续工作，不要当作新指令）\n" +
				req.DesignContextSummary + "\n\n" + prompt
		}
	}
	// Tail: append the kind-specific block (Issue / Idea framing). For an Idea
	// the user would normally have hidden this CTA in the frontend; we still
	// inject the block defensively so an out-of-band call (e.g. curl, the
	// wizard page, or a future "重新生成技术方案" path) sees consistent
	// guidance. For Requirement the block is empty.
	if block := promptpkg.ArchitectBlock(req.Kind, req); block != "" {
		prompt += "\n\n" + block
	}

	systemPrompt, model, claudeConfigID := h.roleConfig("architect")
	// Per-request model override (highest precedence); empty means role default.
	if modelOverride != "" {
		model = modelOverride
	}
	// Align the gateway config with the picked model. See resolveConfigIDForRun.
	claudeConfigID = h.resolveConfigIDForRun(claudeConfigIDOverride, model, claudeConfigID)
	job.SetModel(model)

	return &designRunParams{
		Req:            req,
		ProjectPath:    projectPath,
		DefaultBranch:  defaultBranch,
		SourceSID:      sourceSID,
		Fork:           fork,
		SkipAnalysis:   skipAnalysis,
		NewDesignSID:   newDesignSID,
		WorkDir:        workDir,
		Prompt:         prompt,
		SystemPrompt:   systemPrompt,
		Model:          model,
		ClaudeConfigID: claudeConfigID,
		ReadKnowledge:  readKnowledge,
		AgentServerID:  agentServerID,
	}, job, nil
}

// execArchitectDesign runs the goroutine body of the architect-design stage.
// Same line-for-line as the original anonymous goroutine in ArchitectDesign,
// except `body.X` references are now `p.X`. Additions:
//   - A deferred persist of the finished job log (jobLogSvc.Save) so the
//     architect-design execution log survives a backend restart — same
//     pattern StartCoding already had, now extended here to close the
//     existing data-loss gap noted in plan-stateful-stardust.md (fact 1).
//   - The OnFinish callback fires after Save so the scheduler can flip
//     scheduled_tasks.running → succeeded/failed with the same job_id.
//   - A remote branch: when designRunParams.AgentServerID is set the run
//     dispatches to an Agent server via runRemoteArchitectDesign, mirroring
//     the dev-stage local/remote split in StartCoding. The terminal-state
//     handling is identical between branches so any future change only
//     needs to land in one place.
func (h *WizardHandler) execArchitectDesign(p *designRunParams, job *store.Job, cb *runCallbacks) {
	defer func() {
		lines, status, exitCode := job.Snapshot()
		if perr := h.jobLogSvc.Save(job.ID, p.Req.ID, string(status), exitCode, job.StartedAt, job.FinishedAt, lines, job.Model); perr != nil {
			log.Printf("[architect-design] failed to persist job log %s: %v", job.ID, perr)
		}
		if cb != nil && cb.OnFinish != nil {
			cb.OnFinish(job.ID, status == store.JobDone)
		}
	}()

	id := p.Req.ID
	fork := p.Fork
	sourceSID := p.SourceSID
	newDesignSID := p.NewDesignSID
	workDir := p.WorkDir
	model := p.Model
	claudeConfigID := p.ClaudeConfigID
	prompt := p.Prompt
	skipAnalysis := p.SkipAnalysis
	req := p.Req

	log.Printf("[architect-design] job %s started for %s (fork=%v skip=%v agent=%q)", job.ID, id, fork, skipAnalysis && sourceSID == "", p.AgentServerID)

	// Optional knowledge pre-read: inject the project knowledge relevant to
	// this requirement and surface what was read via a "knowledge" SSE event.
	// Default off (read_knowledge=false) keeps the legacy behavior untouched.
	var kbReadTitles []string
	if p.ReadKnowledge {
		kbBlock, kbTitles := h.buildKnowledgeBlock(req.ProjectID, req.Title)
		if kbBlock != "" {
			prompt = kbBlock + "\n" + prompt
		}
		emitKnowledgeEvent(job, kbTitles)
		kbReadTitles = kbTitles
	}

	// Session threading is shared between local and remote — both branches
	// invoke claude with the same --resume / --fork-session / --session-id
	// flags, so a user who picked the same requirement's previous analyst
	// session to resume sees the same behavior regardless of execution
	// surface.
	sessionArg := sourceSID
	forkSessionID := ""
	if fork {
		forkSessionID = newDesignSID
	} else if sourceSID == "" {
		sessionArg = newDesignSID // skip-analysis fresh session
	}
	if block := llm.BuildSkillsBlock(h.mentionedSkills(req.Title + " " + req.Description)); block != "" {
		prompt = block + prompt
	}

	var out claudeStreamOutcome
	if p.AgentServerID != "" && h.agentSvrSvc != nil {
		// Remote branch — plan-mode claude on the picked Agent server.
		// runRemoteArchitectDesign owns SSH dial / git worktree / session
		// sync / worker invocation / NDJSON parse; this exec body only
		// owns the post-run persistence (UpdateDesign / UpdateArchitectModel
		// / knowledge result event), same as the local branch below.
		job.Append(store.LogLine{Type: "phase", Content: "📐 Agent 服务器 plan 模式下探索代码并制定技术方案..."})
		out = h.runRemoteArchitectDesign(&remoteArchitectInput{
			remoteRunInput: &remoteRunInput{
				job:            job,
				serverID:       p.AgentServerID,
				reqRow:         req,
				prompt:         prompt,
				workDir:        workDir,
				sourceSID:      sessionArg,
				fork:           fork,
				sessionArg:     sessionArg,
				forkSessionID:  forkSessionID,
				model:          model,
				claudeConfigID: claudeConfigID,
				usage:          h.usageCtxFor("architect_design", id, req.ProjectID, job.ID, model, "", ""),
				PermissionMode: "plan",
			},
		})
	} else {
		// Local branch — direct claude CLI invocation on the NovaWorkbench host.
		// context.Background(): the HTTP request has already returned, so we
		// must not tie the claude subprocess's lifetime to r.Context() (which
		// is cancelled the moment the handler returns).
		job.Append(store.LogLine{Type: "phase", Content: "📐 Claude 正在 plan 模式下探索代码并制定技术方案..."})
		cmd := h.llm.StreamCmd(context.Background(), llm.StreamOpts{
			Prompt:         prompt,
			WorkDir:        workDir,
			SystemPrompt:   p.SystemPrompt,
			Model:          cliModelArg(model),
			ClaudeConfigID: claudeConfigID,
			SessionID:      sessionArg,
			Resume:         sourceSID != "",
			Fork:           fork,
			ForkSessionID:  forkSessionID,
			PermissionMode: "plan",
		})
		out = runClaudeStream(jobSink{job}, cmd, "architect-design", h.usageCtxFor("architect_design", id, req.ProjectID, job.ID, model, "", ""))
	}

	h.finalizeArchitectRun(out, p, job, kbReadTitles, sourceSID, newDesignSID, id, model)
}

// finalizeArchitectRun is the shared terminal-state handler for both the local
// and the remote branch of execArchitectDesign. Extracted so the
// stale-session / partial-result / success paths exist in exactly one place —
// any future tweak to error wording or persistence order lands here and
// automatically applies to both execution surfaces. Returns nothing; the
// caller already set up the deferred job-log / OnFinish hook.
//
// Sibling of the dev-stage inline terminal-state code in StartCoding (which
// can't be shared because it has to commit + push). architect-design has
// no commit / push step so a single helper covers both branches cleanly.
func (h *WizardHandler) finalizeArchitectRun(
	out claudeStreamOutcome,
	p *designRunParams,
	job *store.Job,
	kbReadTitles []string,
	sourceSID, newDesignSID, id, model string,
) {
	if out.staleSession {
		// The source conversation is gone. Clear whichever session id was
		// stale so the user can redo the prior stage, surface a recovery
		// hint, and clear the active job pointer. On the skip-analysis path
		// there is no source session to be stale, so this branch is a
		// no-op guard; we still surface a generic recovery hint.
		if sourceSID == "" {
			job.Append(store.LogLine{Type: "error", Content: "会话异常，请重试生成技术方案。"})
		} else if p.Fork {
			_ = h.reqSvc.UpdateAnalysisSession(id, "")
			job.Append(store.LogLine{Type: "error", Content: "需求分析会话已过期。请重新进行「需求分析」后再生成技术方案。"})
		} else {
			_ = h.reqSvc.UpdateDesignSession(id, "")
			job.Append(store.LogLine{Type: "error", Content: "技术方案会话已过期。请重新生成技术方案。"})
		}
		_ = h.reqSvc.UpdateDesignJob(id, "")
		job.Finish(1, store.JobError)
		return
	}

	// The design session id is already persisted upfront. Correct it only if
	// the CLI reported a different id than the one we pre-minted (a safety
	// net in case the --session-id override semantics ever change). The
	// remote branch's parseStreamJSONFromReader emits session_id on the same
	// `--session-id` events as the local CLI, so the correction path applies
	// to both surfaces uniformly.
	if out.sessionID != "" && out.sessionID != newDesignSID && out.sessionID != sourceSID {
		if perr := h.reqSvc.UpdateDesignSession(id, out.sessionID); perr != nil {
			log.Printf("[architect-design] failed to persist design session for %s: %v", id, perr)
		}
	}

	// In plan mode, the full plan markdown is captured from the Write
	// tool_use event that lands in ~/.claude/plans/*.md (runClaudeStream
	// stores it in out.planContent; the remote branch's
	// parseStreamJSONFromReader does the same). Fall back to the result text
	// if capture missed it (e.g. a proxy that doesn't emit tool_use blocks
	// in the assistant event).
	planMarkdown := out.planContent
	if planMarkdown == "" {
		planMarkdown = out.finalResult
	}
	// The run ended with an error (upstream proxy 504, api_error result
	// event, or non-zero exit). The Write tool_use that populates
	// planContent fires BEFORE the model's final API call, so a 504 on that
	// trailing call leaves planMarkdown non-empty while the run actually
	// failed. Treating that as success appends a green ✅ and the user has
	// no idea the run was interrupted (and the captured plan may be
	// partial). Surface the error instead: save the captured plan as a
	// fallback so the exploration work isn't lost, but mark the job errored
	// so the UI shows the failure and the user can retry.
	if out.errMsg != "" {
		if planMarkdown != "" {
			if _, err := h.reqSvc.UpdateDesign(id, planMarkdown); err != nil {
				log.Printf("[architect-design] failed to save partial design for %s: %v", id, err)
			}
		}
		_ = h.reqSvc.UpdateDesignJob(id, "")
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
		_ = h.reqSvc.UpdateDesignJob(id, "")
		job.Finish(1, store.JobError)
		return
	}

	// Persist the design (sets status=designing) and clear the active job
	// pointer so a refresh shows the finished design instead of "executing".
	if _, err := h.reqSvc.UpdateDesign(id, planMarkdown); err != nil {
		log.Printf("[architect-design] failed to save design for %s: %v", id, err)
		job.Append(store.LogLine{Type: "error", Content: "保存技术方案失败: " + err.Error()})
		_ = h.reqSvc.UpdateDesignJob(id, "")
		job.Finish(1, store.JobError)
		return
	}
	_ = h.reqSvc.UpdateDesignJob(id, "")

	// Record the effective model for the architect stage (success path only).
	if perr := h.reqSvc.UpdateArchitectModel(id, model); perr != nil {
		log.Printf("[architect-design] failed to persist architect_model for %s: %v", id, perr)
	}
	// Close the knowledge loop: mark which read entries the run actually used.
	if len(kbReadTitles) > 0 {
		items, used := evaluateKnowledgeUsage(kbReadTitles, out.toolFiles, planMarkdown)
		emitKnowledgeResultEvent(job, items, used)
	}
	job.Append(store.LogLine{Type: "done", Content: "✅ 技术方案已生成！"})
	job.Finish(0, store.JobDone)
	log.Printf("[architect-design] job %s finished for %s", job.ID, id)
}
