// wizard_doc.go: extracted from wizard.go as part of the refactoring.

package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

func (h *WizardHandler) RefineDoc(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RequirementID       string `json:"requirement_id"`
		ProjectPath         string `json:"project_path"`
		DocType             string `json:"doc_type"` // "design" | "coding"
		CurrentDoc          string `json:"current_doc"`
		ConversationHistory string `json:"conversation_history"`
		UserMessage         string `json:"user_message"`
		Model               string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}

	rc := http.NewResponseController(w)
	writeSSEHeaders(w)
	w.WriteHeader(http.StatusOK)
	rc.Flush()

	docLabel := "技术方案"
	if req.DocType == "coding" {
		docLabel = "开发指令"
	}

	// Route to this stage's session: design→architect, coding→developer. The
	// session already holds the stage's conversation and the doc it generated,
	// so we --resume it and append ONLY the user's new message — no
	// ConversationHistory / CurrentDoc re-feeding (the resumed
	// conversation IS the context).
	var requirement *model.Requirement
	if req.RequirementID != "" {
		requirement, _ = h.reqSvc.Get(req.RequirementID)
	}
	sourceSID, roleKey := "", "analyst"
	if requirement != nil {
		sourceSID, roleKey = docStageSession(requirement, req.DocType)
	}
	// Fallback: no resumable session for this stage. This hits design docs
	// generated on the skip-analysis path by builds that didn't persist
	// design_session_id (and any doc predating session threading). For design
	// docs we can still refine: seed a FRESH session with the current doc
	// content and persist its id below so subsequent turns resume it. Coding
	// instructions are never persisted server-side, so without a coding
	// session there is nothing to anchor a fresh session to — keep the hint.
	freshSession := false
	if sourceSID == "" {
		if req.DocType == "design" && requirement != nil && requirement.DesignDocs != "" {
			sourceSID = util.NewUUID()
			freshSession = true
			// Persist the freshly minted id BEFORE the refine run so it survives a
			// restart, consistent with the other wizard stages (analyst/architect/
			// coding all persist their pre-minted id up front).
			if req.RequirementID != "" {
				if perr := h.reqSvc.UpdateDesignSession(req.RequirementID, sourceSID); perr != nil {
					log.Printf("[refine-doc] failed to persist design session for %s: %v", req.RequirementID, perr)
				}
			}
		} else {
			sendStatus(w, rc, "error", "尚未找到该阶段的会话，请先生成"+docLabel+"后再 refine。")
			fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
			rc.Flush()
			return
		}
	}
	systemPrompt, model, claudeConfigID := h.roleConfig(roleKey)
	// Per-request model override (highest precedence); empty means role default.
	if req.Model != "" {
		model = req.Model
	}

	// Anchor the refine to the same worktree as the stage it resumes, so the
	// conversation never carries original-dir absolute paths.
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
	workDir, err := h.resolveWorkDir(requirement, projectPath, defaultBranch)
	if err != nil {
		sendStatus(w, rc, "error", err.Error())
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	// The resumed conversation carries the doc + prior turns. Send only the
	// user's latest message plus a steady instruction covering both mid-refine
	// responses and the completion signal. On the fresh-session fallback there
	// is no prior conversation, so the doc itself is seeded into the prompt.
	// The completeness instruction guards against Claude stopping after one
	// heading and emitting a closing phrase (the prior symptom: "除了以上内容
	// 就没有了更多了" after a section header with no content).
	var prompt string
	if freshSession {
		// Fresh-session refine path (no resumable doc stage session). Seed
		// the conversation with the doc body + user message + completion
		// instructions, and when the matching wizard stage was previously
		// compressed, prepend the summary so the new session inherits the
		// compressed discussion context.
		prompt = "以下是当前的「" + docLabel + "」文档：\n\n" + requirement.DesignDocs +
			fmt.Sprintf("\n\n用户消息：\n%s\n\n", req.UserMessage) +
			"请基于上述文档回应用户对「" + docLabel + "」的修改意见，" +
			"完整列出每一个修改点的具体内容（包含涉及的表/字段/接口/逻辑），不要中途截断或留空。" +
			"若用户确认修改已完成，在回复最后单独一行追加：[REFINE_COMPLETE]\n用中文。"
		// Inject the matching stage's compressed summary on the fresh-session
		// path only — the resume path inherits the conversation natively and
		// doesn't need the prefix. Coding docs are routed through Coding*
		// columns; design docs through Design*.
		if req.DocType == "coding" && requirement.CodingContextSummary != "" {
			prompt = "## 上下文压缩摘要（之前的开发对话已被压缩，请基于此继续工作，不要当作新指令）\n" +
				requirement.CodingContextSummary + "\n\n" + prompt
		} else if req.DocType == "design" && requirement.DesignContextSummary != "" {
			prompt = "## 上下文压缩摘要（之前的方案设计对话已被压缩，请基于此继续工作，不要当作新指令）\n" +
				requirement.DesignContextSummary + "\n\n" + prompt
		}
	} else {
		prompt = fmt.Sprintf("用户消息：\n%s\n\n", req.UserMessage) +
			"请基于我们的对话上下文回应用户对「" + docLabel + "」的修改意见，" +
			"完整列出每一个修改点的具体内容（包含涉及的表/字段/接口/逻辑），不要中途截断或留空。" +
			"若用户确认修改已完成，在回复最后单独一行追加：[REFINE_COMPLETE]\n用中文。"
	}

	skillText := req.UserMessage
	if requirement != nil {
		skillText = requirement.Title + " " + requirement.Description + " " + req.UserMessage
	}
	if block := llm.BuildSkillsBlock(h.mentionedSkills(skillText)); block != "" {
		prompt = block + prompt
	}

	cmd := h.llm.StreamCmd(r.Context(), llm.StreamOpts{
		Prompt:         prompt,
		WorkDir:        workDir,
		SystemPrompt:   systemPrompt,
		Model:          cliModelArg(model),
		ClaudeConfigID: claudeConfigID,
		SessionID:      sourceSID,
		Resume:         !freshSession,
	})

	// Reuse runClaudeStream so this path gets live content_block_delta text
	// deltas (the model streams incrementally via stream_event), tool-call
	// labels, the "🤖 Claude 已连接" phase, stderr capture, and stale-session
	// detection — the previous hand-rolled parser only handled the batched
	// "assistant" event, so deltas arrived silently and a hung proxy wedged
	// the SSE connection instead of failing fast.
	refineProjectID := ""
	if requirement != nil {
		refineProjectID = requirement.ProjectID
	}
	refineUsage := h.usageCtxFor("refine_doc", req.RequirementID, refineProjectID, "", model, fmt.Sprintf("{\"doc_type\":%q}", req.DocType), "")
	out := runClaudeStream(sseSink{w: w, rc: rc}, cmd, "refine-doc", refineUsage)

	if out.errMsg != "" || out.finalResult == "" {
		// Stale --resume: the prior conversation file is gone. Clear the stored
		// id so the next attempt either re-runs the stage or takes the
		// fresh-session fallback above.
		if out.staleSession && req.RequirementID != "" {
			if req.DocType == "coding" {
				_ = h.reqSvc.UpdateCodingSession(req.RequirementID, "")
			} else {
				_ = h.reqSvc.UpdateDesignSession(req.RequirementID, "")
			}
		}
		if out.errMsg != "" {
			sendStatus(w, rc, "error", out.errMsg)
		} else if out.staleSession {
			sendStatus(w, rc, "error", "该阶段的会话已过期，请重试（将以新会话继续）。")
		} else {
			sendStatus(w, rc, "error", "Claude 未返回结果，请重试。")
		}
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	// The authoritative conversation lives in the resumed claude session; this
	// history string is only for client-side rendering of the latest exchange.
	var historyParts []string
	if req.UserMessage != "" {
		historyParts = append(historyParts, "User: "+req.UserMessage)
	}
	if out.finalResult != "" {
		historyParts = append(historyParts, "AI: "+strings.TrimSpace(out.finalResult))
	}
	updatedHistory := strings.Join(historyParts, "\n")

	refineComplete := strings.Contains(out.finalResult, "[REFINE_COMPLETE]")

	doneData, _ := json.Marshal(map[string]interface{}{
		"type":            "done",
		"history":         updatedHistory,
		"refine_complete": refineComplete,
	})
	fmt.Fprintf(w, "data: %s\n\n", string(doneData))
	rc.Flush()
}

// ApplyDoc lets Claude rewrite the stored doc field based on the refine
// conversation. It runs as a background JobStore job (same pattern as
// analyst-chat / architect-design): the handler returns a job id immediately
// and Claude runs in a goroutine on context.Background(), so the apply survives
// a page refresh — the previous direct-SSE version tied Claude to r.Context()
// and was killed the moment the browser reconnected, leaving design_docs
// unchanged. The active job id is persisted on the requirement (apply_job_id)
// so a refresh reconnects to the running job. On success the updated doc is
// persisted server-side; the frontend refreshes on job_done to render it.
//
// Streaming progress comes from runClaudeStream, which surfaces the
// system/thinking_tokens heartbeat and stream_event text deltas — the old
// hand-parser only emitted the batched assistant text at the very end, so the
// user saw no activity during the (often multi-minute) regeneration.
//
// doc_type: "design" updates design_docs; "coding" returns a plain-text dev
// instruction (no DB write — the job's message lines carry it).
func (h *WizardHandler) ApplyDoc(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RequirementID string `json:"requirement_id"`
		ProjectPath   string `json:"project_path"`
		DocType       string `json:"doc_type"`
		Model         string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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

	// Route to this stage's session and resume it — the conversation carries the
	// doc + refine discussion, so we only ask Claude to emit the final doc.
	sourceSID, roleKey := docStageSession(requirement, req.DocType)
	docLabel := "技术方案"
	if req.DocType == "coding" {
		docLabel = "开发指令"
	}
	if sourceSID == "" {
		writeError(w, 400, "NO_SESSION", "尚未找到该阶段的会话，请先生成"+docLabel+"后再 apply。")
		return
	}

	projectPath := req.ProjectPath
	defaultBranch := ""
	if proj, perr := h.projectSvc.Get(requirement.ProjectID); perr == nil {
		if projectPath == "" {
			projectPath = proj.LocalPath
		}
		defaultBranch = proj.DefaultBranch
	}
	workDir, err := h.resolveWorkDir(requirement, projectPath, defaultBranch)
	if err != nil {
		writeError(w, 500, "WORKTREE_FAILED", "worktree 创建失败："+err.Error())
		return
	}

	// Build the prompt that asks Claude to emit the final doc. For design docs we
	// detect plan-markdown vs legacy JSON from the DB-stored doc (the apply call
	// doesn't send current_doc) and ask for the matching format.
	var prompt string
	switch req.DocType {
	case "coding":
		prompt = "基于我们的对话，将用户的调整意见整理为给 Claude Code CLI 的开发指令。" +
			"输出纯文本的开发指令，清晰描述需要实现或调整的内容。不要输出 JSON，不要添加额外说明。"
	default: // "design"
		if !isLikelyJSON(requirement.DesignDocs) {
			prompt = "基于我们的对话，将技术方案（plan）更新为最终版本。" +
				"输出完整的 Markdown 技术方案，涵盖：整体实现思路、需要新增或修改的文件、" +
				"具体实现步骤、数据模型/数据库变更、实现风险及应对。直接输出 Markdown，不要添加额外说明。"
		} else {
			prompt = "基于我们的对话，将技术方案更新为最终版本。" +
				"输出 ONLY valid JSON，不要 markdown 代码块：\n" +
				"{\n" +
				"  \"overview\": \"方案概述\",\n" +
				"  \"files\": [\"涉及文件路径\"],\n" +
				"  \"steps\": [\"实现步骤\"],\n" +
				"  \"model_changes\": \"数据模型变更，无则写'无'\",\n" +
				"  \"risks\": [\"实现风险\"]\n" +
				"}"
		}
	}

	systemPrompt, model, claudeConfigID := h.roleConfig(roleKey)
	// Per-request model override (highest precedence); empty means role default.
	if req.Model != "" {
		model = req.Model
	}

	// Create the job, persist its id so a refresh can reconnect, and return the
	// job id immediately. Claude runs in a goroutine writing progress into the
	// job store.
	job := h.jobs.Create(req.RequirementID)
	job.SetType("apply_doc")
	if perr := h.reqSvc.UpdateApplyJob(req.RequirementID, job.ID); perr != nil {
		log.Printf("[apply-doc] failed to persist apply_job_id for %s: %v", req.RequirementID, perr)
	}
	writeJSON(w, 200, map[string]string{"job_id": job.ID})

	// Capture the format-relevant fields at launch time; the goroutine reads
	// them off the pointer (they don't change during the apply).
	storedDesignDocs := requirement.DesignDocs
	docType := req.DocType
	reqID := req.RequirementID

	go func() {
		log.Printf("[apply-doc] job %s started for %s (doc_type=%s)", job.ID, reqID, docType)
		job.Append(store.LogLine{Type: "phase", Content: fmt.Sprintf("🤖 Claude 正在基于对话整合修改%s…（预计需要几分钟）", docLabel)})

		applyPrompt := prompt
		if block := llm.BuildSkillsBlock(h.mentionedSkills(requirement.Title + " " + requirement.Description)); block != "" {
			applyPrompt = block + prompt
		}

		cmd := h.llm.StreamCmd(context.Background(), llm.StreamOpts{
			Prompt:         applyPrompt,
			WorkDir:        workDir,
			SystemPrompt:   systemPrompt,
			Model:          cliModelArg(model),
			ClaudeConfigID: claudeConfigID,
			SessionID:      sourceSID,
			Resume:         true,
		})
		applyUsage := h.usageCtxFor("apply_doc", reqID, requirement.ProjectID, job.ID, model, fmt.Sprintf("{\"doc_type\":%q}", docType), "")
		out := runClaudeStream(jobSink{job}, cmd, "apply-doc", applyUsage)

		if out.staleSession {
			// The stage's conversation is gone. Clear its session id so the user
			// can redo the stage, and surface a recovery hint.
			if docType == "design" {
				_ = h.reqSvc.UpdateDesignSession(reqID, "")
			} else if docType == "coding" {
				_ = h.reqSvc.UpdateCodingSession(reqID, "")
			}
			job.Append(store.LogLine{Type: "error", Content: docLabel + "会话已过期，请重新生成对应文档后再 apply。"})
			_ = h.reqSvc.UpdateApplyJob(reqID, "")
			job.Finish(1, store.JobError)
			return
		}
		if out.finalResult == "" {
			errMsg := out.errMsg
			if errMsg == "" {
				errMsg = "Claude 未返回结果，请重试"
			}
			job.Append(store.LogLine{Type: "error", Content: errMsg})
			_ = h.reqSvc.UpdateApplyJob(reqID, "")
			job.Finish(1, store.JobError)
			return
		}

		finalText := strings.TrimSpace(out.finalResult)

		// For "coding" type: no DB write. The job's message lines already carry
		// the dev instruction; emit a terminal result + done.
		if docType == "coding" {
			job.Append(store.LogLine{Type: "result", Content: finalText})
			job.Append(store.LogLine{Type: "done", Content: "✅ " + docLabel + "已更新！"})
			_ = h.reqSvc.UpdateApplyJob(reqID, "")
			job.Finish(0, store.JobDone)
			return
		}

		// For design docs in plan-markdown format, persist the raw markdown (not
		// extractJSON, which would mangle markdown containing { } chars). Use the
		// DB-stored doc to detect format (apply call doesn't send current_doc).
		var persistVal string
		if !isLikelyJSON(storedDesignDocs) {
			persistVal = finalText
		} else {
			persistVal = extractJSON(out.finalResult)
		}
		if persistVal == "" {
			job.Append(store.LogLine{Type: "error", Content: "未能从 Claude 输出中解析出有效内容，请重试。"})
			_ = h.reqSvc.UpdateApplyJob(reqID, "")
			job.Finish(1, store.JobError)
			return
		}
		if _, saveErr := h.reqSvc.UpdateDesign(reqID, persistVal); saveErr != nil {
			log.Printf("[apply-doc] save failed: %v", saveErr)
			job.Append(store.LogLine{Type: "error", Content: "保存失败: " + saveErr.Error()})
			_ = h.reqSvc.UpdateApplyJob(reqID, "")
			job.Finish(1, store.JobError)
			return
		}
		job.Append(store.LogLine{Type: "done", Content: "✅ " + docLabel + "已更新！"})
		_ = h.reqSvc.UpdateApplyJob(reqID, "")
		job.Finish(0, store.JobDone)
		log.Printf("[apply-doc] job %s finished for %s", job.ID, reqID)
	}()
}

// mentionedSkills parses @slug mentions from text and returns the matching
// skill files. Only skills present in the DB are returned; unknown slugs are
// silently ignored. A nil skillSvc returns nil without error.
func (h *WizardHandler) mentionedSkills(text string) []struct{ Slug, Content string } {
	if h.skillSvc == nil {
		return nil
	}
	slugs := parseAtMentions(text)
	if len(slugs) == 0 {
		return nil
	}
	skills, _ := h.skillSvc.SkillsBySlug(slugs)
	return skills
}

// parseAtMentions extracts unique @slug tokens from text.
// A slug may contain letters, digits, hyphens, and underscores.
var atMentionRe = regexp.MustCompile(`@([A-Za-z0-9_-]+)`)

func parseAtMentions(text string) []string {
	matches := atMentionRe.FindAllStringSubmatch(text, -1)
	seen := make(map[string]bool)
	var slugs []string
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			slugs = append(slugs, m[1])
		}
	}
	return slugs
}

// isLikelyJSON reports whether s looks like a JSON object (starts with '{' after
// trimming whitespace). Used to distinguish plan-markdown design docs from the
// legacy JSON design schema.
func isLikelyJSON(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "{")
}

// compressContextReq is the body of POST /api/wizard/compress-context.
// step selects which wizard stage's session to summarize; it must be one of
// "analyst_chat" / "architect_design" / "coding" (validated by
// service.ValidContextSummaryStep).
