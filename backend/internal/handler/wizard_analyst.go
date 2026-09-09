// wizard_analyst.go: extracted from wizard.go as part of the refactoring.

package handler

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	promptpkg "github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

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
