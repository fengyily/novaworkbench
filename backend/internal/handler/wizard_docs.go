package handler

// Wizard document refinement + context compression handlers.
//
// RefineDoc / ApplyDoc drive the iterative "edit the design or coding doc
// through a multi-turn Claude conversation" loop; CompressContext /
// GetContextSummary let the user ask Claude to summarize an in-progress
// stage's conversation so a fresh session can pick up where it left off.
// collectProjectContext + mentionedSkills / parseAtMentions are the helpers
// the analyst / architect / coding stages all rely on for project scoping
// and @-skill injection. Extracted from wizard.go as part of the file-split
// refactor; behaviour is unchanged.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

// collectProjectContext builds a BOUNDED context for the analyst first turn so
// Claude doesn't have to blindly wander a large project tree (the opennhp
// checkout is 3.4G / ~1000 files). The walk stats files but only reads a handful
// of small text files. It returns:
//   - docBlock: full content of AI-config docs (CLAUDE.md/AGENTS.md/.cursorrules/
//     README*) plus keyword-matched source files, capped at ~40KB total.
//   - readFiles: the relative paths whose content was injected (for SSE progress).
//   - treeSummary: a names-only directory listing (depth- and count-capped) so
//     Claude knows the layout without Glob'ing multi-GB of files.
func collectProjectContext(projectPath, requirementTitle string) (docBlock string, readFiles []string, treeSummary string) {
	const (
		maxDocBytes    = 40 * 1024
		maxTreeEntries = 240
		maxTreeDepth   = 3
	)

	docCandidates := []string{"CLAUDE.md", "AGENTS.md", ".cursorrules", "README.md", "README"}

	// Keywords from the title (lowercased) for source-file matching. Chinese
	// titles rarely match English paths, so docs + tree carry most of the weight;
	// keyword matching is a bonus when it hits.
	titleLower := strings.ToLower(requirementTitle)
	keywords := strings.FieldsFunc(titleLower, func(r rune) bool {
		return r == ' ' || r == '-' || r == '_' || r == '/' || r == ',' || r == '：' || r == ':' || r == '、'
	})
	filtered := keywords[:0]
	for _, kw := range keywords {
		if len(kw) >= 3 {
			filtered = append(filtered, kw)
		}
	}
	keywords = filtered

	skipExts := map[string]bool{
		".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true, ".svg": true,
		".woff": true, ".woff2": true, ".ttf": true, ".eot": true,
		".zip": true, ".tar": true, ".gz": true, ".sum": true, ".lock": true,
		".bin": true, ".exe": true, ".db": true, ".so": true, ".dll": true, ".dylib": true,
		".mp4": true, ".mp3": true, ".pdf": true, ".dat": true, ".pdb": true,
	}

	type fileEntry struct {
		path       string
		relPath    string
		isPriority bool
	}
	var toRead []fileEntry
	seen := map[string]bool{}
	var treePaths []string

	_ = filepath.Walk(projectPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(projectPath, path)
		// Skip hidden / generated / dependency dirs entirely.
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if part == "." {
				continue
			}
			if strings.HasPrefix(part, ".") || part == "vendor" || part == "node_modules" ||
				part == "dist" || part == "build" || part == "__pycache__" || part == "target" {
				return nil
			}
		}

		// Tree: collect names only (dirs and files), depth- and count-limited.
		if len(treePaths) < maxTreeEntries {
			depth := strings.Count(rel, string(filepath.Separator)) + 1
			if depth <= maxTreeDepth {
				if info.IsDir() {
					treePaths = append(treePaths, rel+"/")
				} else {
					treePaths = append(treePaths, rel)
				}
			}
		}

		if info.IsDir() {
			return nil
		}

		// Skip binary / large files from CONTENT injection (still listed in tree).
		ext := strings.ToLower(filepath.Ext(info.Name()))
		if skipExts[ext] || info.Size() > 200*1024 {
			return nil
		}

		nameLower := strings.ToLower(info.Name())
		relLower := strings.ToLower(rel)

		for _, doc := range docCandidates {
			// Only read AI-config / readme docs at the PROJECT ROOT. Nested
			// README.md files (docker/, endpoints/.../etc) are noise — they
			// bloat the prompt and push the model to read more. The tree still
			// lists them by name, so the model knows they exist.
			if nameLower == strings.ToLower(doc) && !strings.Contains(rel, string(filepath.Separator)) && !seen[rel] {
				seen[rel] = true
				toRead = append(toRead, fileEntry{path, rel, true})
				return nil
			}
		}
		for _, kw := range keywords {
			if strings.Contains(relLower, kw) && !seen[rel] {
				seen[rel] = true
				toRead = append(toRead, fileEntry{path, rel, false})
				return nil
			}
		}
		return nil
	})

	// Doc + keyword content (priority first), capped.
	var buf strings.Builder
	total := 0
	readOne := func(f fileEntry) {
		data, rerr := os.ReadFile(f.path)
		if rerr != nil {
			return
		}
		content := string(data)
		if total+len(content) > maxDocBytes {
			content = content[:maxDocBytes-total]
		}
		buf.WriteString(fmt.Sprintf("### %s\n```\n%s\n```\n\n", f.relPath, content))
		readFiles = append(readFiles, f.relPath)
		total += len(content)
	}
	for _, f := range toRead {
		if f.isPriority && total < maxDocBytes {
			readOne(f)
		}
	}
	for _, f := range toRead {
		if !f.isPriority && total < maxDocBytes {
			readOne(f)
		}
	}

	tree := "## 项目结构（仅名称）\n"
	if len(treePaths) == 0 {
		tree += "(未能列出项目结构)"
	} else {
		tree += strings.Join(treePaths, "\n")
		if len(treePaths) >= maxTreeEntries {
			tree += "\n…（已截断；如需更多请直接读取具体路径）"
		}
	}

	docBlock = buf.String()
	treeSummary = tree
	if docBlock == "" {
		docBlock = "(未预读到关键文档，请基于下方结构概览，按需读取具体文件)"
	}
	return
}

// RefineDoc streams a multi-turn conversation to refine a design doc or a coding instruction.
// doc_type: "design" | "coding"
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

// compressContextReq is the body of POST /api/wizard/compress-context.
// step selects which wizard stage's session to summarize; it must be one of
// "analyst_chat" / "architect_design" / "coding" (validated by
// service.ValidContextSummaryStep).
type compressContextReq struct {
	RequirementID string `json:"requirement_id"`
	Step          string `json:"step"`
}

// compressContextDone is the JSON payload of the terminal "done" event for
// the compress-context SSE stream. Carrying the summary here (instead of a
// follow-up GET) lets the modal pop with a single round-trip; the persisted
// row is fetched separately by the frontend's requirementsApi.get() so the
// rest of the detail page (sidebar / stepper) reflects the new compressed_at
// timestamp too.
type compressContextDone struct {
	Step        string `json:"step"`
	Summary     string `json:"summary"`
	TokensUsed  int    `json:"tokens_used"`
	Model       string `json:"model"`
	StartedAt   int64  `json:"started_at_ms"`
	CompletedAt int64  `json:"completed_at_ms"`
}

// CompressContext is the implementation of POST /api/wizard/compress-context.
// It runs a single --resume turn asking Claude to summarize the current
// wizard stage's conversation, writes the result to requirements.{step}_
// context_summary + stamps compressed_at, and clears the matching session id
// — all in one transaction via service.UpdateContextSummary so the next turn
// in this stage starts fresh and sees the summary as a prompt prefix.
//
// Stream protocol (SSE under text/event-stream):
//
//	phase       — human-readable status line
//	message     — Claude's summary text as it streams in
//	usage       — mirror of the result.usage block (powers the live usage bar)
//	error       — terminal failure (no DB write happens)
//	done        — terminal success; carries {step, summary, tokens_used, model}
//
// Failure policy: on any error path (stream failure, stale session, missing
// [COMPRESS_COMPLETE] marker) we emit `error` + `done{success:false}` and
// DELIBERATELY skip the DB write — the session stays intact so the user can
// retry without losing context.
func (h *WizardHandler) CompressContext(w http.ResponseWriter, r *http.Request) {
	var req compressContextReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "invalid JSON: "+err.Error())
		return
	}
	if req.RequirementID == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "requirement_id is required")
		return
	}
	if !service.ValidContextSummaryStep(req.Step) {
		writeError(w, http.StatusBadRequest, "INVALID", "step must be one of: analyst_chat, architect_design, coding")
		return
	}

	rc := http.NewResponseController(w)
	writeSSEHeaders(w)
	w.WriteHeader(http.StatusOK)
	rc.Flush()

	startedAtMs := time.Now().UnixMilli()

	requirement, err := h.reqSvc.Get(req.RequirementID)
	if err != nil {
		sendStatus(w, rc, "error", "未找到需求："+err.Error())
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	// Pick the matching session id; reject the request when there is no
	// resumable session so the user doesn't burn tokens summarizing nothing.
	var sourceSID string
	switch req.Step {
	case "analyst":
		sourceSID = requirement.AnalysisSessionID
	case "design":
		sourceSID = requirement.DesignSessionID
	case "coding":
		sourceSID = requirement.CodingSessionID
	}
	if sourceSID == "" {
		sendStatus(w, rc, "error", "该阶段尚无可压缩的会话（先与 AI 对话一轮再试）")
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	// Resolve project path the same way the other wizard stages do — anchor
	// the resumed turn to the requirement's worktree (or the project root when
	// no worktree exists) so absolute paths in the resume history still resolve.
	projectPath := ""
	defaultBranch := ""
	if proj, perr := h.projectSvc.Get(requirement.ProjectID); perr == nil {
		projectPath = proj.LocalPath
		defaultBranch = proj.DefaultBranch
	}
	workDir, werr := h.resolveWorkDir(requirement, projectPath, defaultBranch)
	if werr != nil {
		// Non-fatal: a missing worktree doesn't break --resume, which can run
		// without a cwd. Log it so debugging is possible but proceed.
		log.Printf("[compress-context] resolveWorkDir failed for %s: %v", req.RequirementID, werr)
		workDir = projectPath
	}

	// Use the analyst role's system prompt — compression is essentially a
	// summary-extraction skill, and reusing the analyst persona keeps the
	// output style consistent with the other analytical turns. The model
	// override follows the same precedence as other wizard handlers.
	systemPrompt, model, claudeConfigID := h.roleConfig("analyst")

	// Compression prompt (Chinese, fixed). The [COMPRESS_COMPLETE] sentinel
	// is parsed by this handler to know when Claude has finished writing —
	// text before the sentinel is the summary; text after (rare, in case the
	// model emits trailing chatter) is discarded. We DISALLOW every tool so
	// the resumed turn can only read the conversation history baked into
	// --resume and produce a single textual summary.
	const compressPrompt = `请把当前对话压缩为一段精炼的中文摘要（300-800 字），要求：
1. 保留关键决策、技术约束、已确认的需求点
2. 保留当前进展状态、待办事项
3. 保留涉及的具体文件、函数、模块名
4. 不要新增原对话没有的信息
5. 不要输出代码片段（除非是必须保留的命名/路径）
6. 末尾单独一行输出：[COMPRESS_COMPLETE]`

	noTools := []string{
		"Read", "Glob", "Grep", "Bash", "Write", "Edit",
		"WebFetch", "WebSearch", "NotebookEdit",
	}

	cmd := h.llm.StreamCmd(r.Context(), llm.StreamOpts{
		Prompt:          compressPrompt,
		WorkDir:         workDir,
		SessionID:       sourceSID,
		Resume:          true,
		SystemPrompt:    systemPrompt,
		Model:           cliModelArg(model),
		ClaudeConfigID:  claudeConfigID,
		DisallowedTools: noTools,
	})

	// Usage record: step is namespaced under "compress_<step>" so the existing
	// usageApi.requirement() aggregation includes compression cost without
	// confusing it with regular turn counts in the step dropdown.
	usageStep := "compress_" + req.Step
	uctx := h.usageCtxFor(usageStep, req.RequirementID, requirement.ProjectID, "", model, "", "")

	sink := sseSink{w: w, rc: rc}
	sink.emit(store.LogLine{Type: "phase", Content: "📦 正在压缩上下文..."})

	out := runClaudeStream(sink, cmd, "compress-context", uctx)

	if out.staleSession {
		// Disk session is gone (cleaned up, container restart, etc.). We can't
		// resume, so the right thing is to leave the (already-empty) sid
		// alone — the next regular turn will start a fresh session anyway.
		sendStatus(w, rc, "error", "⚠️ 该阶段的会话已过期（磁盘已被清理），无需压缩")
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}
	if out.errMsg != "" {
		sendStatus(w, rc, "error", "❌ "+out.errMsg)
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	// Extract the summary: everything before [COMPRESS_COMPLETE], trimmed.
	// A missing marker is treated as a soft failure so we don't store a half-
	// formed or empty summary that would mislead future turns.
	summary := out.finalResult
	if idx := strings.Index(summary, "[COMPRESS_COMPLETE]"); idx >= 0 {
		summary = summary[:idx]
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		sendStatus(w, rc, "error", "❌ Claude 未输出有效摘要（缺少 [COMPRESS_COMPLETE] 标记或摘要为空），请重试")
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	// Token count for the done payload — read straight from the stream
	// outcome we already have in scope (no DB roundtrip needed). The model
	// comes from the result event's actualModel (set by runClaudeStream) with
	// a fallback to the pre-dispatch role model.
	tokensUsed := out.lastUsage.InputTokens +
		out.lastUsage.OutputTokens +
		out.lastUsage.CacheCreationTokens +
		out.lastUsage.CacheReadTokens
	modelOut := out.actualModel
	if modelOut == "" {
		modelOut = model
	}

	// Persist + clear session id atomically. A failure here leaves the
	// summary un-saved so the user can retry — the original session stays
	// live (we never cleared the sid before the write).
	if perr := h.reqSvc.UpdateContextSummary(req.RequirementID, req.Step, summary); perr != nil {
		log.Printf("[compress-context] persist failed for %s step=%s: %v", req.RequirementID, req.Step, perr)
		sendStatus(w, rc, "error", "❌ 持久化失败："+perr.Error())
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":false}\n\n")
		rc.Flush()
		return
	}

	completedAtMs := time.Now().UnixMilli()
	done := compressContextDone{
		Step:        req.Step,
		Summary:     summary,
		TokensUsed:  tokensUsed,
		Model:       modelOut,
		StartedAt:   startedAtMs,
		CompletedAt: completedAtMs,
	}
	if b, mErr := json.Marshal(done); mErr == nil {
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":true,\"payload\":%s}\n\n", string(b))
	} else {
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"success\":true}\n\n")
	}
	rc.Flush()
}

// GetContextSummary returns the persisted compression summary for one
// requirement + step. Optional endpoint — the frontend can derive this from
// the regular Requirement GET (which now exposes the *_context_summary +
// *_compressed_at fields), but exposing a dedicated endpoint keeps the
// "📦 已压缩" UI affordance cheap to refresh on its own.
//
// GET /api/wizard/requirement/{id}/context-summary?step=analyst_chat|architect_design|coding
func (h *WizardHandler) GetContextSummary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "missing requirement id")
		return
	}
	step := r.URL.Query().Get("step")
	if !service.ValidContextSummaryStep(step) {
		writeError(w, http.StatusBadRequest, "INVALID", "step must be one of: analyst_chat, architect_design, coding")
		return
	}
	summary, err := h.reqSvc.GetContextSummary(id, step)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requirement_id": id,
		"step":           step,
		"summary":        summary,
	})
}
