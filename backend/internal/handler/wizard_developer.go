// wizard_developer.go: extracted from wizard.go as part of the refactoring.

package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	promptpkg "github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

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

// toolCallLabel returns a human-readable Chinese label for a tool call event.
