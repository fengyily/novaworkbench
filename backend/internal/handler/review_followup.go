package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// ── 追问 (follow-up questions on a finished Review) ──────────────────────────
//
// A review report answers "what is wrong"; a follow-up answers "why, and what
// exactly should I do about it". The two are deliberately separate runs:
//
//   - A follow-up NEVER re-grades the review. result_md, findings_json and
//     the P0·P1·P3 counts are untouched, so the numbers in the history table
//     always refer to the report the user read.
//   - It resumes the review's claude session when that session still exists
//     (`--resume <sid>`), which is the whole point: the model already holds
//     the diff, the files it opened and its own reasoning, so the answer is
//     grounded instead of re-derived. The session id is captured from the
//     stream during the review run (ReviewHandler.rememberReviewSession).
//   - When the session is gone — an old review from before this feature, a
//     CLI that pruned the jsonl, a re-created agent host — the run falls back
//     to a fresh session whose prompt embeds the stored report plus the
//     earlier Q&A of the thread. Degraded, never broken; the row records
//     which of the two happened in `resumed` so the UI can say so.
//
// Streaming reuses the existing review SSE channel
// (GET /api/projects/{id}/prs/jobs/{job_id}/stream) — no second protocol.

// maxFollowupReportChars caps how much of the stored report is pasted into a
// fresh (non-resumed) follow-up prompt. Reports are usually a few KB; the cap
// only bites on pathological ones, where losing the tail of the P3 section
// costs far less than blowing the context window.
const maxFollowupReportChars = 24000

// maxFollowupAnswerChars caps each earlier answer replayed into a fresh
// session's prompt. The questions are kept whole (they are short and carry
// the user's intent); the answers are the part that can run long.
const maxFollowupAnswerChars = 4000

// AskFollowupReq is the body for asking a follow-up question.
type AskFollowupReq struct {
	Question string `json:"question"`
}

// ListFollowups returns a review's 追问 thread, oldest first.
// GET /api/reviews/{rid}/followups
func (h *ReviewHandler) ListFollowups(w http.ResponseWriter, r *http.Request) {
	rv, ok := h.lookupReview(w, r)
	if !ok {
		return
	}
	if h.followupSvc == nil {
		writeJSON(w, http.StatusOK, []model.CodeReviewFollowup{})
		return
	}
	rows, err := h.followupSvc.ListByReview(rv.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// AskFollowup records a question and dispatches the run, returning the
// JobStore id to stream from (same SSE channel as the review itself).
// POST /api/reviews/{rid}/followups
func (h *ReviewHandler) AskFollowup(w http.ResponseWriter, r *http.Request) {
	rv, ok := h.lookupReview(w, r)
	if !ok {
		return
	}
	if h.followupSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "追问服务未启用")
		return
	}
	var req AskFollowupReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Question) == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "追问内容不能为空")
		return
	}
	// Asking about a report that doesn't exist yet would send the model off
	// to improvise one. Refuse early with a message that names the fix.
	if rv.Status == model.ReviewStatusRunning {
		writeError(w, http.StatusConflict, "REVIEW_RUNNING", "Review 正在运行中，请等待本次 Review 结束后再追问")
		return
	}
	if strings.TrimSpace(rv.ResultMD) == "" {
		writeError(w, http.StatusBadRequest, "NO_REPORT", "该 Review 还没有生成报告，无法追问")
		return
	}
	project, err := h.projectSvc.Get(rv.ProjectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目不存在")
		return
	}

	job := h.jobs.Create(rv.ProjectID)
	fu, err := h.followupSvc.Create(&model.CodeReviewFollowup{
		ReviewID:  rv.ID,
		Question:  strings.TrimSpace(req.Question),
		JobID:     job.ID,
		CreatedBy: currentUserID(r),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CREATE_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID, "followup_id": fu.ID})

	go h.runFollowup(job, rv, fu, project)
}

// DeleteFollowup drops one turn from the thread.
// DELETE /api/reviews/{rid}/followups/{fid}
func (h *ReviewHandler) DeleteFollowup(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.lookupReview(w, r); !ok {
		return
	}
	if h.followupSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "追问服务未启用")
		return
	}
	if err := h.followupSvc.Delete(r.PathValue("fid")); err != nil {
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// runFollowup drives one follow-up end to end. Every exit path goes through
// finishFollowup, so a crashed run never leaves a row stuck in `running`.
func (h *ReviewHandler) runFollowup(job *store.Job, rv *model.CodeReview, fu *model.CodeReviewFollowup, project *model.Project) {
	ctx := context.Background()

	// Same binding chain as the review itself, so a follow-up is answered by
	// the model that wrote the report (and billed to the same config).
	rolePrompt, modelName, claudeConfigID := h.roleConfig("reviewer")
	if picked := strings.TrimSpace(rv.Model); picked != "" && picked != DefaultModelLabel {
		modelName = picked
	}
	claudeConfigID = h.resolveReviewConfigID(strings.TrimSpace(rv.ClaudeConfigID), modelName, claudeConfigID)
	job.SetModel(modelName)

	history := h.followupHistory(rv.ID, fu.ID)

	job.Append(store.LogLine{Type: "phase", Content: "💬 追问：" + fu.Question})

	spec := reviewTurnSpec{
		scope:             "review-followup",
		prompt:            func(resumed bool) string { return h.followupPrompt(rv, fu, history, resumed) },
		usage:             func() *usageCtx { return h.newFollowupUsage(job.ID, rv, fu, modelName) },
		resumePhase:       "🔗 续接本次 Review 的会话上下文...",
		staleNotice:       "⚠️ 原 Review 会话已失效，改用「报告原文 + 仓库代码」重新作答",
		remoteMissingRepo: "Agent 服务器上的仓库副本已不存在，请重新运行一次 Review 后再追问",
		remoteNoService:   "Agent 服务器服务未初始化，无法在远端执行追问",
	}

	answer, resumed, errMsg := h.runReviewTurn(ctx, job, rv, project, rolePrompt, modelName, claudeConfigID, spec)
	h.finishFollowup(job, fu, modelName, resumed, answer, errMsg)
}

// runReviewTurn dispatches a resumable turn to the review's execution
// environment: 本地 when agent_server_id is empty, the Agent Server
// otherwise. Never a silent local fallback — a remote review's session only
// exists over there.
func (h *ReviewHandler) runReviewTurn(ctx context.Context, job *store.Job, rv *model.CodeReview, project *model.Project, rolePrompt, modelName, claudeConfigID string, spec reviewTurnSpec) (text string, resumed bool, errMsg string) {
	if strings.TrimSpace(rv.AgentServerID) != "" {
		return h.runRemoteReviewTurn(ctx, job, rv, project, rolePrompt, modelName, claudeConfigID, spec)
	}
	return h.runLocalReviewTurn(ctx, job, rv, project, rolePrompt, modelName, claudeConfigID, spec)
}

// streamReviewTurn runs one local claude turn against a finished review and
// streams it into the job. Shared by 追问 and 「重新生成」 — it only knows how
// to run a prompt, not what the prompt is for.
//
// stale=true means the run failed specifically because `--resume` pointed at
// a conversation the CLI no longer has, which the caller retries fresh.
func (h *ReviewHandler) streamReviewTurn(ctx context.Context, job *store.Job, workDir, systemPrompt, taskPrompt, modelName, claudeConfigID, sessionID string, resume bool, uctx *usageCtx) (answer string, stale bool, errMsg string) {
	opts := llm.StreamOpts{
		Prompt:         taskPrompt,
		WorkDir:        workDir,
		SystemPrompt:   systemPrompt,
		Model:          cliModelArg(modelName),
		ClaudeConfigID: claudeConfigID,
	}
	if resume {
		opts.Resume = true
		opts.SessionID = sessionID
	}
	cmd := h.llm.StreamCmd(ctx, opts)

	// Stderr is where the CLI reports "No conversation found with session ID",
	// so it has to be captured for the staleness check rather than discarded.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", false, "启动 Claude 失败: " + err.Error()
	}
	if err := cmd.Start(); err != nil {
		logClaudeExecDiag("review-followup", cmd)
		return "", false, explainExecError(err)
	}

	var streamed strings.Builder
	final := ""
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var evt map[string]interface{}
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		switch evt["type"] {
		case "assistant":
			if msg, ok := evt["message"].(map[string]interface{}); ok {
				if content, ok := msg["content"].([]interface{}); ok {
					for _, block := range content {
						bmap, ok := block.(map[string]interface{})
						if !ok {
							continue
						}
						switch bmap["type"] {
						case "text":
							if text, ok := bmap["text"].(string); ok && strings.TrimSpace(text) != "" {
								job.Append(store.LogLine{Type: "message", Content: text})
								streamed.WriteString(text)
								streamed.WriteString("\n\n")
							}
						case "tool_use":
							inp, _ := bmap["input"].(map[string]interface{})
							job.Append(store.LogLine{Type: "tool_call", Content: toolCallLabel(fmt.Sprint(bmap["name"]), inp)})
						}
					}
				}
			}
		case "result":
			uctx.recordFrom(evt)
			if sub, _ := evt["subtype"].(string); sub == "success" {
				if result, ok := evt["result"].(string); ok && strings.TrimSpace(result) != "" {
					job.Append(store.LogLine{Type: "message", Content: result})
					final = result
				}
			} else if isStaleSessionError(evt, "") {
				stale = true
			} else if errText, ok := evt["error"].(string); ok && errText != "" {
				errMsg = errText
			}
		}
	}

	waitErr := cmd.Wait()
	if waitErr != nil {
		exitCode := 1
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
		if exitCode != 0 {
			log.Printf("[Review] follow-up claude exited with code %d", exitCode)
		}
	}
	if !stale && resume && isStaleSessionError(nil, stderr.String()) {
		stale = true
	}

	answer = firstNonEmpty(final, streamed.String())
	if stale {
		return "", true, ""
	}
	if strings.TrimSpace(answer) == "" && errMsg == "" {
		errMsg = "Claude 未返回任何追问回答"
		if s := strings.TrimSpace(truncateStr(stderr.String(), 400)); s != "" {
			errMsg += "：" + s
		}
	}
	return answer, false, errMsg
}

// finishFollowup is the single terminal path: persist the turn, snapshot the
// job log and close the job. An error is stored as the answer text so the
// thread shows what happened instead of an empty bubble.
//
// The answer goes through service.SanitizeMermaidBlocks for the same reason
// the review report does (buildReviewOutcome): BuildFollowupPrompt invites a
// Mermaid diagram when the question is about a call chain or a timing
// problem, and a block the Mermaid 11 lexer rejects would render as a red
// error card inside the thread. No-op when the answer has no diagram.
func (h *ReviewHandler) finishFollowup(job *store.Job, fu *model.CodeReviewFollowup, modelName string, resumed bool, answer, errMsg string) {
	status := model.FollowupStatusDone
	body := service.SanitizeMermaidBlocks(answer)
	if errMsg != "" {
		status = model.FollowupStatusError
		body = errMsg
		job.Append(store.LogLine{Type: "error", Content: errMsg})
	}
	if h.followupSvc != nil {
		if err := h.followupSvc.MarkFinished(fu.ID, status, body, modelName, resumed); err != nil {
			log.Printf("[Review] persist follow-up %s: %v", fu.ID, err)
		}
	}
	lines, jobStatus, exitCode := job.Snapshot()
	if perr := h.jobLogSvc.Save(job.ID, "", string(jobStatus), exitCode, job.StartedAt, job.FinishedAt, lines, modelName); perr != nil {
		log.Printf("[Review] failed to persist follow-up job log %s: %v", job.ID, perr)
	}
	if status == model.FollowupStatusError {
		job.Finish(1, store.JobError)
		return
	}
	job.Finish(0, store.JobDone)
}

// followupPrompt renders the task prompt for one turn. resumed selects the
// short form (the session already holds the context) or the self-contained
// one (report + thread pasted in).
func (h *ReviewHandler) followupPrompt(rv *model.CodeReview, fu *model.CodeReviewFollowup, history []prompt.FollowupTurn, resumed bool) string {
	in := prompt.FollowupPromptInput{
		Resumed:  resumed,
		Question: fu.Question,
		History:  history,
	}
	if !resumed {
		in.Branch = rv.Branch
		in.BaseBranch = rv.BaseBranch
		in.PRNumber = rv.PRNumber
		in.PRTitle = rv.Title
		in.Description = rv.Description
		in.ReportMD = truncateStr(rv.ResultMD, maxFollowupReportChars)
		// The resolved refs aren't persisted, so the prompt names the branch
		// pair and lets the model resolve it if it wants the diff again.
		in.HeadRef, in.BaseRef = rv.Branch, rv.BaseBranch
	}
	return prompt.BuildFollowupPrompt(in)
}

// followupHistory returns the finished turns of this thread (excluding the
// one being answered), newest last. Errored turns are skipped — replaying a
// failure message as if it were an answer would only confuse the model.
func (h *ReviewHandler) followupHistory(reviewID, excludeID string) []prompt.FollowupTurn {
	if h.followupSvc == nil {
		return nil
	}
	rows, err := h.followupSvc.ListByReview(reviewID)
	if err != nil {
		return nil
	}
	out := make([]prompt.FollowupTurn, 0, len(rows))
	for i := range rows {
		if rows[i].ID == excludeID || rows[i].Status != model.FollowupStatusDone {
			continue
		}
		out = append(out, prompt.FollowupTurn{
			Question: rows[i].Question,
			Answer:   truncateStr(rows[i].Answer, maxFollowupAnswerChars),
		})
	}
	return out
}

// newFollowupUsage mirrors newReviewUsage but tags the row with the
// follow-up id, so a thread's cost is attributable turn by turn instead of
// being folded into the review that spawned it.
func (h *ReviewHandler) newFollowupUsage(jobID string, rv *model.CodeReview, fu *model.CodeReviewFollowup, modelName string) *usageCtx {
	u := h.newReviewUsage(jobID, rv, modelName)
	u.Step = "review-followup"
	u.Meta = fmt.Sprintf(`{"branch":%q,"review_id":%q,"review_type":%q,"followup_id":%q}`,
		rv.Branch, rv.ID, rv.ReviewType, fu.ID)
	return u
}

// firstNonEmpty returns the first argument with non-blank content.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
