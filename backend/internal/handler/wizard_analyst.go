package handler

// Analyst + Developer chat surface for the wizard pipeline.
//
// AnalystChat is the first role-gated stage (requirement analyst): a
// JobStore-backed SSE conversation that refines a requirement against the
// project tree. DeveloperChat is the post-coding "追加调整" panel that runs
// against the developer role (a discussion-only turn — Write/Edit are
// blocked, real edits run in a subsequent start-coding job).
//
// The session-thread / stale-resume recovery plumbing lives here alongside
// the handlers, plus the small set of disallowed-tool lists and the
// first-turn prompt builder used by analyst-chat. Stream parsing and the
// SSE event sink live in wizard_stream.go; shared role/model helpers and
// the usage context live in wizard_common.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	promptpkg "github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

// AnalystChat starts one analyst-chat turn as a background JobStore job and
// returns the job id immediately (same pattern as architect-design /
// start-coding). Claude runs in a goroutine with context.Background(), so its
// lifetime is decoupled from this HTTP request — a page refresh no longer kills
// the in-flight turn. The active job id is persisted on the requirement
// (analysis_job_id) so a refresh reconnects to the running job via
// GET /api/wizard/jobs/{id}/stream (which replays history first) instead of
// relaunching the turn. The job's log lines carry the analyst's message /
// tool_call / phase events; on job_done the frontend finalizes the turn.
//
// Session threading: the first turn mints a session id (--session-id, persisted
// on the requirement as analysis_session_id); subsequent turns resume it
// (--resume). A stale --resume transparently falls back to a fresh first turn.
func (h *WizardHandler) AnalystChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectPath      string `json:"project_path"`
		RequirementID    string `json:"requirement_id"`
		RequirementTitle string `json:"requirement_title"`
		CurrentAnalysis  string `json:"current_analysis"`
		UserMessage      string `json:"user_message"`
		Model            string `json:"model"`
		// ClaudeConfigID — user-picked claude_configs row id (UI ModelSelect);
		// empty = backend resolves via the priority chain in
		// resolveConfigIDForRun.
		ClaudeConfigID string `json:"claude_config_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("[analyst-chat] JSON decode error: %v", err)
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	if req.RequirementID == "" {
		writeError(w, 400, "INVALID", "missing requirement_id")
		return
	}

	requirement, err := h.reqSvc.Get(req.RequirementID)
	if err != nil {
		writeError(w, 404, "NOT_FOUND", "requirement not found")
		return
	}

	// Resolve session threading from the DB (source of truth). The request
	// body's title/analysis are only fallbacks for the prompt builder.
	storedSessionID := requirement.AnalysisSessionID
	isFirstRound := storedSessionID == ""
	sessionID := storedSessionID
	if isFirstRound {
		sessionID = util.NewUUID()
		// Persist the freshly minted id BEFORE the claude turn runs so it
		// survives a mid-run backend restart (the session lives on disk; the id
		// must already be in the DB for a later --resume to find it).
		if perr := h.reqSvc.UpdateAnalysisSession(req.RequirementID, sessionID); perr != nil {
			log.Printf("[analyst-chat] failed to persist analysis session for %s: %v", req.RequirementID, perr)
		}
	}
	resumePrompt := req.UserMessage

	systemPrompt, model, claudeConfigID := h.roleConfig("analyst")
	// Per-request model override (highest precedence); empty means role default.
	if req.Model != "" {
		model = req.Model
	}
	// Align the gateway config with the picked model. See resolveConfigIDForRun.
	claudeConfigID = h.resolveConfigIDForRun(req.ClaudeConfigID, model, claudeConfigID)

	// Create the job, persist its id so a refresh can reconnect, and return the
	// job id immediately. The claude turn runs in a goroutine writing progress
	// into the job store.
	job := h.jobs.Create(req.RequirementID)
	job.SetType("analyst_chat")
	job.SetModel(model)
	if perr := h.reqSvc.UpdateAnalysisJob(req.RequirementID, job.ID); perr != nil {
		log.Printf("[analyst-chat] failed to persist analysis_job_id for %s: %v", req.RequirementID, perr)
	}
	writeJSON(w, 200, map[string]string{"job_id": job.ID})

	projectPath := req.ProjectPath
	defaultBranch := ""
	if proj, perr := h.projectSvc.Get(requirement.ProjectID); perr == nil {
		if projectPath == "" {
			projectPath = proj.LocalPath
		}
		defaultBranch = proj.DefaultBranch
	}

	go func() {
		log.Printf("[analyst-chat] job %s started for %s (resume=%v)", job.ID, req.RequirementID, !isFirstRound)
		defer func() {
			// Clear the active-job pointer on every exit path so a refresh after
			// the turn ends shows the idle chat (ready for the next message)
			// instead of a stale "running" spinner.
			_ = h.reqSvc.UpdateAnalysisJob(req.RequirementID, "")
		}()
		sink := jobSink{job}
		job.Append(store.LogLine{Type: "phase", Content: "🤖 Claude 正在准备分析..."})

		// Anchor the analyst stage to the isolated worktree (created here if
		// missing) so the whole session chain — analysis → design → coding — is
		// rooted in the worktree and never leaks original-dir absolute paths.
		workDir, err := h.resolveWorkDir(requirement, projectPath, defaultBranch)
		if err != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ " + err.Error()})
			job.Finish(1, store.JobError)
			return
		}

		// firstTurnPrompt pre-reads a BOUNDED slice of the project (AI docs +
		// a names-only structure tree) and emits each pre-read file as
		// progress, so the user sees live activity before Claude responds.
		// Only invoked for a first-turn run (genuine first turn, or the
		// stale-resume fallback); resume turns just send the new user message.
		title := req.RequirementTitle
		desc := ""
		analysis := req.CurrentAnalysis
		if title == "" {
			title = requirement.Title
		}
		desc = requirement.Description
		if analysis == "" || analysis == "[]" {
			analysis = requirement.AcceptanceCriteria
		}
		firstTurnPrompt := func() string {
			sink.emit(store.LogLine{Type: "phase", Content: "📖 正在预读项目上下文（不遍历整个仓库）..."})
			docBlock, readFiles, treeSummary := collectProjectContext(workDir, title)
			for _, rf := range readFiles {
				sink.emit(store.LogLine{Type: "tool_call", Content: "📖 预读: " + rf})
			}
			log.Printf("[analyst-chat] pre-read %d files, docBlock=%d bytes, tree=%d bytes, desc=%d bytes",
				len(readFiles), len(docBlock), len(treeSummary), len(desc))
			base := buildAnalystFirstPrompt(requirement, desc, analysis, req.UserMessage, docBlock, treeSummary)
			if block := promptpkg.AnalystBlock(requirement.Kind, requirement); block != "" {
				return base + "\n\n" + block
			}
			return base
		}

		// context.Background(): the HTTP request has already returned, so we
		// must not tie the claude subprocess's lifetime to r.Context() (which
		// is cancelled the moment the handler returns — that was the bug that
		// killed the turn on page refresh).
		skillsBlock := llm.BuildSkillsBlock(h.mentionedSkills(requirement.Title + " " + requirement.Description))
		origFirstTurnPrompt := firstTurnPrompt
		if skillsBlock != "" {
			firstTurnPrompt = func() string { return skillsBlock + origFirstTurnPrompt() }
			resumePrompt = skillsBlock + resumePrompt
		}
		analystUsage := h.usageCtxFor("analyst_chat", req.RequirementID, requirement.ProjectID, job.ID, model, "", "")
		finalResult, newSessionID, err := h.runAnalystTurn(context.Background(), firstTurnPrompt, resumePrompt, workDir, systemPrompt, model, claudeConfigID, sessionID, !isFirstRound, sink, analystUsage)
		if err != nil {
			log.Printf("[analyst-chat] turn failed: %v", err)
			job.Append(store.LogLine{Type: "error", Content: err.Error()})
			job.Finish(1, store.JobError)
			return
		}

		// Persist the session id that actually landed on disk. On the first
		// turn we always persist (the freshly minted id). On a resume that
		// fell back from a stale id, newSessionID is a fresh id that differs
		// from what was stored — persist it so the next turn resumes the right
		// conversation. A normal successful resume leaves newSessionID ==
		// storedSessionID, so we skip the write.
		if newSessionID != "" && newSessionID != storedSessionID {
			if perr := h.reqSvc.UpdateAnalysisSession(req.RequirementID, newSessionID); perr != nil {
				log.Printf("[analyst-chat] Failed to persist session for %s: %v", req.RequirementID, perr)
			}
		}

		// The authoritative conversation context lives in the resumed claude
		// session; the job's message lines already carry the assistant text,
		// so the frontend reconstructs the turn from them. Emit a terminal
		// result + done line for cosmetics / debugging.
		job.Append(store.LogLine{Type: "result", Content: strings.TrimSpace(finalResult)})
		job.Append(store.LogLine{Type: "done", Content: "✅ 分析完成！"})
		// Record the effective model for this stage on the success path only
		// (a failed run above returns before reaching here, so the last good
		// record is never clobbered).
		if perr := h.reqSvc.UpdateAnalystModel(req.RequirementID, model); perr != nil {
			log.Printf("[analyst-chat] failed to persist analyst_model for %s: %v", req.RequirementID, perr)
		}
		job.Finish(0, store.JobDone)
		log.Printf("[analyst-chat] job %s finished for %s", job.ID, req.RequirementID)
	}()
}

// DeveloperChat runs one "追加调整" conversation turn against the DEVELOPER role
// (not the analyst). It is the post-coding adjustment dialog: the user describes
// a tweak to the already-implemented code, and the developer confirms
// understanding and proposes an approach WITHOUT editing files — the actual
// edits run in a subsequent start-coding re-run triggered by the frontend's
// "确认，开始修改" button. Routing this to the developer (resuming the coding
// session) keeps the adjustment a DEVELOPMENT change; the old wiring posted the
// same panel to analyst-chat, which refined the requirement instead.
//
// Session threading mirrors StartCoding: resume coding_session_id (the 追加调整
// panel only renders after a first coding pass, so it is normally set);
// otherwise fork off the design/analysis session so the developer inherits the
// requirement+design context. A stale --resume falls back to a fresh session.
func (h *WizardHandler) DeveloperChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectPath      string `json:"project_path"`
		RequirementID    string `json:"requirement_id"`
		RequirementTitle string `json:"requirement_title"`
		UserMessage      string `json:"user_message"`
		Model            string `json:"model"`
		// ClaudeConfigID — user-picked claude_configs row id (UI ModelSelect);
		// empty = backend resolves via the priority chain in
		// resolveConfigIDForRun.
		ClaudeConfigID string `json:"claude_config_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("[developer-chat] JSON decode error: %v", err)
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}

	rc := http.NewResponseController(w)
	writeSSEHeaders(w)
	w.WriteHeader(http.StatusOK)
	rc.Flush()

	log.Printf("[developer-chat] Starting for requirement: %s in %s", req.RequirementTitle, req.ProjectPath)

	// Resolve the source conversation: prefer the coding session (the 追加调整
	// panel only shows after a first coding pass, so this is normally set);
	// otherwise fork off the design/analysis session so the developer still
	// inherits the requirement+design context. Same resolution as StartCoding.
	var requirement *model.Requirement
	if req.RequirementID != "" {
		if existing, err := h.reqSvc.Get(req.RequirementID); err == nil {
			requirement = existing
		}
	}
	sourceSID := ""
	fork := false
	if requirement != nil {
		if requirement.CodingSessionID != "" {
			sourceSID = requirement.CodingSessionID
		} else if requirement.DesignSessionID != "" {
			sourceSID = requirement.DesignSessionID
			fork = true
		} else if requirement.AnalysisSessionID != "" {
			sourceSID = requirement.AnalysisSessionID
			fork = true
		}
	}

	// Pre-mint + persist the coding session id BEFORE spawning claude for the
	// fork and fresh cases (a plain resume reuses coding_session_id). The forked
	// id is pre-assigned via --session-id so it's known and persisted up front.
	newSID := ""
	if fork || sourceSID == "" {
		newSID = util.NewUUID()
		if req.RequirementID != "" {
			if perr := h.reqSvc.UpdateCodingSession(req.RequirementID, newSID); perr != nil {
				log.Printf("[developer-chat] failed to persist coding session for %s: %v", req.RequirementID, perr)
			}
		}
	}

	sendStatus(w, rc, "phase", "🤖 开发者正在理解追加调整...")
	rc.Flush()

	// Echo the user request as a dedicated SSE event so the output box can
	// render it as a "👤 调整请求" bubble — this is the same content that is
	// persisted into token_usage.meta.summary for the token-stats table.
	if msg := strings.TrimSpace(req.UserMessage); msg != "" {
		payload, _ := json.Marshal(map[string]string{"type": "user_input", "content": msg})
		fmt.Fprintf(w, "data: %s\n\n", payload)
		rc.Flush()
	}
	rc.Flush()

	// Anchor to the worktree so the resumed coding session's conversation stays
	// rooted in the isolated dir (this chat is read-only, but keeps cwd consistent).
	projectPath := req.ProjectPath
	defaultBranch := ""
	if requirement != nil {
		if proj, perr := h.projectSvc.Get(requirement.ProjectID); perr == nil {
			if projectPath == "" {
				projectPath = proj.LocalPath
			}
			defaultBranch = proj.DefaultBranch
		}
	}
	if fork {
		if gerr := h.requireAnchoredFork(requirement, projectPath); gerr != nil {
			sendStatus(w, rc, "error", gerr.Error())
			fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
			rc.Flush()
			return
		}
	}
	workDir, err := h.resolveWorkDir(requirement, projectPath, defaultBranch)
	if err != nil {
		sendStatus(w, rc, "error", err.Error())
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	systemPrompt, model, claudeConfigID := h.roleConfig("developer")
	// Per-request model override (highest precedence); empty means role default.
	if req.Model != "" {
		model = req.Model
	}
	// Align the gateway config with the picked model. See resolveConfigIDForRun.
	claudeConfigID = h.resolveConfigIDForRun(req.ClaudeConfigID, model, claudeConfigID)

	// The resumed coding conversation already carries the requirement, analysis,
	// and design, so a resume turn only sends the framed adjustment message. The
	// firstTurnPrompt (used on the no-session / stale-fallback path) folds in the
	// title + design so the developer has context without a pre-read pass.
	title := req.RequirementTitle
	var designMarkdown string
	if requirement != nil {
		if title == "" {
			title = requirement.Title
		}
		designMarkdown = requirement.DesignDocs
	}
	firstTurnPrompt := func() string {
		var b strings.Builder
		// Coding-stage compression handoff (DeveloperChat first turn): when
		// the user already compressed prior coding turns we want the
		// developer to see the summary as scene-setting. Goes at the very
		// top so the model treats it as ground truth, not as a post-hoc
		// addendum. The disclaimer parenthetical discourages the model from
		// acting on the summary as if it were a fresh instruction.
		if requirement != nil && requirement.CodingContextSummary != "" {
			b.WriteString("## 上下文压缩摘要（之前的开发对话已被压缩，请基于此继续工作，不要当作新指令）\n")
			b.WriteString(requirement.CodingContextSummary)
			b.WriteString("\n\n")
		}
		b.WriteString("现在以「开发者」角色处理用户的追加调整。请先阅读相关代码、确认你对调整意图的理解、给出实现思路与可能的影响；")
		b.WriteString("**暂不要修改任何文件**——等用户在后续步骤确认后，再由开发任务执行修改。\n\n")
		if title != "" {
			b.WriteString(fmt.Sprintf("需求：%s\n\n", title))
		}
		if strings.TrimSpace(designMarkdown) != "" {
			b.WriteString("技术方案：\n")
			b.WriteString(designMarkdown)
			b.WriteString("\n\n")
		}
		b.WriteString("追加调整：\n")
		b.WriteString(req.UserMessage)
		if requirement != nil {
			if block := promptpkg.DeveloperBlock(requirement.Kind, requirement); block != "" {
				b.WriteString("\n\n")
				b.WriteString(block)
			}
		}
		return b.String()
	}
	resumePrompt := fmt.Sprintf(
		"以下是对已实现代码的追加调整。请以「开发者」角色先阅读相关代码、确认你对调整意图的理解、"+
			"给出实现思路与可能的影响；**暂不要修改任何文件**——等用户确认后，再由开发任务执行修改。\n\n追加调整：\n%s",
		req.UserMessage,
	)

	developerProjectID := ""
	if requirement != nil {
		developerProjectID = requirement.ProjectID
	}
	developerUsage := h.usageCtxFor("developer_chat", req.RequirementID, developerProjectID, "", model, "", req.UserMessage)
	finalResult, newSessionID, err := h.runDeveloperTurn(r.Context(), firstTurnPrompt, resumePrompt, workDir, systemPrompt, model, claudeConfigID, sourceSID, fork, newSID, w, rc, developerUsage)
	if err != nil {
		log.Printf("[developer-chat] turn failed: %v", err)
		sendStatus(w, rc, "error", err.Error())
		fmt.Fprintf(w, "data: {\"type\":\"done\"}\n\n")
		rc.Flush()
		return
	}

	// The coding session id is already persisted upfront. Correct it only if the
	// run produced a DIFFERENT id than we pre-minted — either a stale-fallback
	// fresh session (runDeveloperTurn minted a fresh id) or the CLI reporting an
	// id other than our --session-id override (a safety net).
	if req.RequirementID != "" && newSessionID != "" && newSessionID != sourceSID && newSessionID != newSID {
		if perr := h.reqSvc.UpdateCodingSession(req.RequirementID, newSessionID); perr != nil {
			log.Printf("[developer-chat] Failed to persist coding session for %s: %v", req.RequirementID, perr)
		}
	}

	// Build a lightweight local-history string for the frontend's chat display.
	// The authoritative conversation context lives in the resumed claude session,
	// so this is just for client-side rendering.
	var historyParts []string
	if req.UserMessage != "" {
		historyParts = append(historyParts, "User: "+req.UserMessage)
	}
	historyParts = append(historyParts, "AI: "+strings.TrimSpace(finalResult))
	updatedHistory := strings.Join(historyParts, "\n")

	doneData, _ := json.Marshal(map[string]interface{}{"type": "done", "history": updatedHistory, "model": model})
	fmt.Fprintf(w, "data: %s\n\n", string(doneData))
	rc.Flush()
}

// analystFirstTurnDisallowedTools blocks file/code tools on the analyst first
// turn so Claude answers from the pre-read context without tool use. The
// atlascloud proxy mangles multi-turn tool-use streaming ("Content block not
// found"); the first turn already has the pre-read docs + tree, so it doesn't
// need to read files. Resume turns keep all tools.
var analystFirstTurnDisallowedTools = []string{"Read", "Glob", "Grep", "Bash", "Write", "Edit"}

// runAnalystTurn runs one analyst-chat turn with automatic stale-session
// recovery. firstTurnPrompt is a lazy builder for the first-turn prompt (it
// pre-reads bounded project context and emits the pre-read files as progress);
// it is only invoked when doing a first-turn run — the genuine first turn, or
// the stale-resume fallback — so resume turns don't pay the pre-read cost.
// resumePrompt is just the new user message for a resumed turn. When resume==true
// and the target conversation no longer exists on disk (Claude reports "No
// conversation found"), it transparently falls back: mints a fresh session id and
// re-runs as a first turn, so a stale id stuck in the DB never wedges the chat.
// sink receives the turn's log lines (phase / tool_call / message); in the
// JobStore flow it is a jobSink so the lines survive a page refresh via the job's
// replay buffer. Returns the final result text and the session id that actually
// landed on disk (which the caller persists).
func (h *WizardHandler) runAnalystTurn(ctx context.Context, firstTurnPrompt func() string, resumePrompt, projectPath, systemPrompt, model, claudeConfigID, sessionID string, resume bool, sink streamSink, uctx *usageCtx) (finalResult, newSessionID string, err error) {
	prompt := resumePrompt
	if !resume {
		prompt = firstTurnPrompt()
	}
	log.Printf("[analyst-chat] Running claude stream-json (session=%s, resume=%v), prompt=%d bytes", sessionID, resume, len(prompt))

	// On a first-turn run (genuine first turn or stale-resume fallback), block
	// file/code tools so Claude answers purely from the pre-read context. The
	// atlascloud proxy mangles multi-turn tool-use streaming ("Content block not
	// found"), and the first turn has the pre-read docs + tree so it doesn't
	// need to read files. Resume turns keep tools (the user may ask Claude to
	// verify specific code in follow-up).
	var disallowed []string
	if !resume {
		disallowed = analystFirstTurnDisallowedTools
	}
	cmd := h.llm.StreamCmd(ctx, llm.StreamOpts{
		Prompt:          prompt,
		WorkDir:         projectPath,
		SystemPrompt:    systemPrompt,
		Model:           cliModelArg(model),
		ClaudeConfigID:  claudeConfigID,
		SessionID:       sessionID,
		Resume:          resume,
		DisallowedTools: disallowed,
	})
	out := runClaudeStream(sink, cmd, "analyst-chat", uctx)

	if out.staleSession && resume {
		// Stale --resume: the session file is gone (typically a stale id left by
		// an older build before the "persist after success" guard). Recover by
		// starting a fresh conversation, folding the user's latest message into
		// the first-turn prompt.
		freshID := util.NewUUID()
		log.Printf("[analyst-chat] stale session %s — falling back to fresh first turn %s", sessionID, freshID)
		sink.emit(store.LogLine{Type: "phase", Content: "🔄 检测到过期会话，正在重新开始分析..."})
		prompt = firstTurnPrompt()
		cmd = h.llm.StreamCmd(ctx, llm.StreamOpts{
			Prompt:          prompt,
			WorkDir:         projectPath,
			SystemPrompt:    systemPrompt,
			Model:           cliModelArg(model),
			ClaudeConfigID:  claudeConfigID,
			SessionID:       freshID,
			DisallowedTools: analystFirstTurnDisallowedTools,
		})
		out = runClaudeStream(sink, cmd, "analyst-chat", uctx)
		sessionID = freshID
	}

	if out.finalResult == "" {
		if out.errMsg != "" {
			return "", sessionID, fmt.Errorf("%s", out.errMsg)
		}
		return "", sessionID, fmt.Errorf("Claude 未返回结果，请重试")
	}
	return out.finalResult, sessionID, nil
}

// developerChatDisallowedTools blocks file-mutation tools during a developer
// "追加调整" chat turn so Claude discusses the adjustment (reads code, proposes
// an approach) WITHOUT editing — the actual edits run in a later start-coding
// job. Read/Glob/Grep/Bash stay available so the developer can ground its
// understanding in the real code.
var developerChatDisallowedTools = []string{"Write", "Edit"}

// runDeveloperTurn runs one developer-chat turn with automatic stale-session
// recovery, mirroring runAnalystTurn but for the developer role. firstTurnPrompt
// is a lazy self-contained prompt (no pre-read — the developer reads files via
// tools instead); resumePrompt is the framed user message. Write/Edit are
// disallowed so the turn stays discussion-only. newSID is the pre-minted id the
// caller assigned for a fork (--session-id on --fork-session) or a fresh session
// (--session-id); it is "" on a plain resume. Returns the final result text and
// the session id that actually landed on disk (a forked or fresh id differs
// from the input; the caller persists it).
func (h *WizardHandler) runDeveloperTurn(ctx context.Context, firstTurnPrompt func() string, resumePrompt, projectPath, systemPrompt, model, claudeConfigID, sessionID string, fork bool, newSID string, w http.ResponseWriter, rc *http.ResponseController, uctx *usageCtx) (finalResult, newSessionID string, err error) {
	resume := sessionID != ""
	prompt := resumePrompt
	if !resume {
		prompt = firstTurnPrompt()
	}
	log.Printf("[developer-chat] Running claude stream-json (session=%s, resume=%v, fork=%v, new=%s), prompt=%d bytes", sessionID, resume, fork, newSID, len(prompt))

	// sessionID is the --resume source; newSID is the pre-minted id to assign —
	// as the forked session id (--session-id on --fork-session) when fork is set,
	// or as the fresh session id (--session-id) when starting with no session.
	sessionArg := sessionID
	forkSessionID := ""
	if resume {
		if fork {
			forkSessionID = newSID
		}
	} else {
		sessionArg = newSID
	}
	cmd := h.llm.StreamCmd(ctx, llm.StreamOpts{
		Prompt:          prompt,
		WorkDir:         projectPath,
		SystemPrompt:    systemPrompt,
		Model:           cliModelArg(model),
		ClaudeConfigID:  claudeConfigID,
		SessionID:       sessionArg,
		Resume:          resume,
		Fork:            fork,
		ForkSessionID:   forkSessionID,
		DisallowedTools: developerChatDisallowedTools,
	})
	out := runClaudeStream(sseSink{w, rc}, cmd, "developer-chat", uctx)

	if out.staleSession && resume {
		// Stale --resume: the target conversation no longer exists on disk.
		// Recover by starting a fresh developer conversation with the
		// self-contained first-turn prompt (no --resume, no --fork-session).
		freshID := util.NewUUID()
		log.Printf("[developer-chat] stale session %s — falling back to fresh session %s", sessionID, freshID)
		sendStatus(w, rc, "phase", "🔄 检测到过期会话，正在重新开始...")
		rc.Flush()
		prompt = firstTurnPrompt()
		cmd = h.llm.StreamCmd(ctx, llm.StreamOpts{
			Prompt:          prompt,
			WorkDir:         projectPath,
			SystemPrompt:    systemPrompt,
			Model:           cliModelArg(model),
			ClaudeConfigID:  claudeConfigID,
			SessionID:       freshID,
			DisallowedTools: developerChatDisallowedTools,
		})
		out = runClaudeStream(sseSink{w, rc}, cmd, "developer-chat", uctx)
		sessionID = freshID
	}

	if out.finalResult == "" {
		if out.errMsg != "" {
			return "", sessionID, fmt.Errorf("%s", out.errMsg)
		}
		return "", sessionID, fmt.Errorf("Claude 未返回结果，请重试")
	}
	// For a forked run runClaudeStream captured the NEW session id from the
	// system/init event; surface it so the caller persists it. For a plain
	// resume it equals the input id; if init never fired it is empty, so keep
	// the input id.
	if out.sessionID != "" {
		return out.finalResult, out.sessionID, nil
	}
	return out.finalResult, sessionID, nil
}

// buildAnalystFirstPrompt assembles the first-turn analyst prompt. description is
// the FULL requirement text (the title is just a short label and may be
// truncated, so the description is the authoritative intent). It also folds in
// the current (rough) analysis, the user's message, and the bounded project
// context pre-read by collectProjectContext (docBlock + a names-only structure
// tree). It is reused both for the genuine first turn and for the stale-resume
// fallback (where the prior conversation is lost, so we restart from this
// scaffold). The instruction deliberately tells Claude NOT to blindly traverse
// the whole project — it already has the layout and key docs.
// buildAnalystFirstPrompt renders the first-turn prompt for the analyst
// stage. When the requirement carries an AnalystContextSummary (set by the
// "📦 压缩上下文" action on a prior session) it is prepended as a
// "## 上下文压缩摘要（之前的对话已被压缩）" block so the new session inherits
// the compressed history instead of starting cold. The summary is plain
// text — the wizard handler is responsible for stripping the
// [COMPRESS_COMPLETE] sentinel before persisting.
func buildAnalystFirstPrompt(req *model.Requirement, description, currentAnalysis, userMessage, docBlock, treeSummary string) string {
	var b strings.Builder
	if req != nil && req.AnalystContextSummary != "" {
		b.WriteString("## 上下文压缩摘要（之前的对话已被压缩）\n")
		b.WriteString(req.AnalystContextSummary)
		b.WriteString("\n\n")
	}
	if description != "" {
		b.WriteString(fmt.Sprintf("%s\n\n", description))
	}
	if currentAnalysis != "" && currentAnalysis != "[]" {
		b.WriteString(fmt.Sprintf("当前初步分析：\n%s\n\n", currentAnalysis))
	}
	if userMessage != "" {
		b.WriteString(fmt.Sprintf("用户消息：\n%s\n\n", userMessage))
	}
	if docBlock != "" {
		b.WriteString("以下是已预读的项目关键文档与代码（无需重复读取这些文件）：\n")
		b.WriteString(docBlock)
		b.WriteString("\n")
	}
	if treeSummary != "" {
		b.WriteString(treeSummary)
		b.WriteString("\n\n")
	}
	b.WriteString("请**仅基于以上预读上下文**完成工作。**禁止调用工具读取任何文件**——预读已提供关键文档与项目结构；" +
		"若信息不足以判断某点，把该缺失点作为关键问题提给用户，让用户来回答，而不是自己去读文件。\n\n" +
		"1. **分析现有代码**：基于预读内容指出与本需求直接相关的文件、函数、数据结构\n" +
		"2. **识别实现路径**：需要新增或修改哪些部分，有哪些可复用\n" +
		"3. **提出关键问题**：2-3 个你从预读信息中无法确定、必须由用户决策的问题（纯业务/产品决策）\n\n" +
		"先给出你的分析与实现思路，再提问题。\n")
	return b.String()
}