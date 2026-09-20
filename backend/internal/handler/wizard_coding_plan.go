// Package handler — wizard pipeline (requirement → code).
//
// wizard_coding_plan.go is the plan-mode SPLIT path of the coding stage, taken
// when the user ticks 「拆分并自动派发」 and runs locally (split_tasks=true +
// no Agent server). It replaces the old "write-enabled developer agent emits
// [SUBTASKS_READY]" decomposition with two explicit phases:
//
//	planning     — claude runs with --permission-mode plan (read-only) under the
//	               planner persona and drafts an implementation-step list,
//	               captured from the plan-file Write and persisted to
//	               requirements.coding_step_plan.
//	decomposing  — the step list is parsed into the {"subtasks":[…]} envelope
//	               over the lightweight HTTP LLM channel and committed as N
//	               sub_tasks + 1 orchestration_batches row.
//
// From there the existing machinery takes over unchanged: OrchestrationQueue
// dispatches children in batch_seq order, sub_task_runner executes each one
// with --dangerously-skip-permissions, and RunOrchestratorSummary writes the
// summary + triggers auto-push once every child is terminal.
//
// Entry point: execStartCoding (wizard_coding.go) branches here after the
// shared prologue — worktree, branch checkout, provenance stamping, session
// pre-mint and model/config resolution are all already done by then.

package handler

import (
	"fmt"
	"log"
	"strings"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	promptpkg "github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// planSplitInput carries the state execStartCoding already resolved into
// execCodingPlanSplit. It is a struct rather than a long parameter list
// because every field is "whatever the shared prologue decided" — grouping
// them makes it obvious at the call site that nothing is recomputed here.
type planSplitInput struct {
	p      *codingRunParams
	job    *store.Job
	reqRow *model.Requirement
	// workDir is the isolated worktree (or the project checkout for non-git
	// projects) the plan turn explores and every child later edits.
	workDir string
	// systemPrompt is the planner persona. In plan mode the gateway passes it
	// via --append-system-prompt so the CLI's own plan-mode instructions
	// survive (see llm.Gateway.streamArgs).
	systemPrompt   string
	model          string
	claudeConfigID string
	// Session threading, resolved by the prologue: sourceSID is the design (or
	// analysis) session to fork, fork says whether to fork it, and
	// newCodingSID is the pre-minted id already persisted as
	// requirements.coding_session_id.
	sourceSID    string
	fork         bool
	newCodingSID string
}

// execCodingPlanSplit owns the whole split run, including job.Finish. The
// caller returns immediately after invoking it.
//
// It does NOT trigger autoPushPR: on the split path development isn't done
// when this function returns — it's done when the last child finishes, so
// RunOrchestratorSummary owns that call (unchanged behavior).
func (h *WizardHandler) execCodingPlanSplit(in *planSplitInput) {
	reqID := in.p.RequirementID
	job := in.job

	// Take the re-entry lock and guarantee its release. Registered before any
	// work so every exit path — success, early error, or a panic caught by
	// execStartCoding's recover defer — clears the column. defer ordering
	// matters here: execStartCoding registered its recover defer BEFORE
	// calling us, so our defer (deeper in the stack) runs first and the lock
	// is already clear by the time the job is forced to a terminal state.
	if reqID != "" {
		if perr := h.reqSvc.UpdateCodingPhase(reqID, service.CodingPhasePlanning); perr != nil {
			log.Printf("[coding-plan] failed to set coding_phase=planning for %s: %v", reqID, perr)
		}
		defer func() {
			if perr := h.reqSvc.UpdateCodingPhase(reqID, ""); perr != nil {
				log.Printf("[coding-plan] failed to clear coding_phase for %s: %v", reqID, perr)
			}
		}()
	}

	// ---------------------------------------------------------------- planning
	planMarkdown, ok := h.runCodingPlanTurn(in)
	if !ok {
		return // runCodingPlanTurn already appended the error + finished the job
	}

	// ------------------------------------------------------------ decomposing
	if reqID != "" {
		if perr := h.reqSvc.UpdateCodingPhase(reqID, service.CodingPhaseDecomposing); perr != nil {
			log.Printf("[coding-plan] failed to set coding_phase=decomposing for %s: %v", reqID, perr)
		}
	}
	job.Append(store.LogLine{Type: "phase", Content: "🧩 正在把实施步骤拆分为子任务…"})

	payload, stepsJSON := h.decomposePlanIntoSteps(reqID, planMarkdown, in.reqRow, job)
	job.Append(store.LogLine{Type: "message", Content: fmt.Sprintf("📋 拆分完成：%d 个步骤", len(payload.Subtasks))})

	// ------------------------------------------------------------------ commit
	// Record the effective model + resolved config (success path only), and
	// cache the CLI slug — both mirror the direct-implementation path so a
	// refresh re-hydrates the dropdowns and a later Agent-server run can map
	// local session files to the remote cwd's slug.
	if reqID != "" {
		if perr := h.reqSvc.UpdateDeveloperModel(reqID, in.model); perr != nil {
			log.Printf("[coding-plan] failed to persist developer_model for %s: %v", reqID, perr)
		}
		if perr := h.reqSvc.UpdateDeveloperConfig(reqID, in.claudeConfigID); perr != nil {
			log.Printf("[coding-plan] failed to persist developer_config_id for %s: %v", reqID, perr)
		}
	}
	if in.reqRow != nil && in.reqRow.ProjectID != "" {
		if proj, perr := h.projectSvc.Get(in.reqRow.ProjectID); perr == nil && proj != nil {
			if _, derr := h.projectSvc.DiscoverAndCacheClaudeProjectSlug(proj.ID, proj.LocalPath); derr != nil {
				log.Printf("[coding-plan] failed to cache claude_project_slug for %s: %v", proj.ID, derr)
			}
		}
	}

	// Double-fire guard, same rule the legacy auto-orchestrate applied: refuse
	// to stack a second batch on a requirement that already has one in flight.
	// Checked here (rather than at entry) because the planning turn takes
	// minutes — a batch could have been created by a concurrent manual
	// re-split in the meantime.
	if h.batchSvc != nil {
		if existing, gerr := h.batchSvc.GetActiveByRequirement(reqID); gerr == nil && existing != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ 该需求已有正在执行的编排批次（" + existing.Status + "），本次拆分未派发。请等待其完成后重试。"})
			job.Finish(1, store.JobError)
			return
		}
	}

	job.Append(store.LogLine{Type: "done", Content: "✅ 实施步骤已拆分为子任务，开始派发执行"})
	job.Finish(0, store.JobDone)
	log.Printf("[coding-plan] job %s finished planning+decomposing for %s (%d steps)", job.ID, reqID, len(payload.Subtasks))

	// orchestratorSID = the planning session: every child forks it, so each
	// sub-agent inherits the full plan conversation (same relationship the
	// legacy path had between the developer turn and its children).
	h.commitOrchestrationBatch(reqID, in.newCodingSID, payload, in.workDir, in.model, in.claudeConfigID, stepsJSON, "coding-plan")
}

// runCodingPlanTurn runs the plan-mode claude turn and persists the captured
// implementation-step Markdown. Returns (plan, true) on success; on failure it
// has already appended the error line and finished the job, so the caller just
// returns.
func (h *WizardHandler) runCodingPlanTurn(in *planSplitInput) (string, bool) {
	job := in.job
	reqID := in.p.RequirementID
	job.Append(store.LogLine{Type: "phase", Content: "📐 正在制定实施步骤（plan 模式只读探索代码）…"})

	prompt := h.buildPlanPrompt(in)

	// Session threading mirrors the direct path: fork the design/analysis
	// session when there is one (--resume <src> --fork-session --session-id
	// <new>), otherwise start fresh on the pre-minted id (--session-id <new>).
	sessionArg := in.sourceSID
	forkSessionID := ""
	if in.fork {
		forkSessionID = in.newCodingSID
	} else if in.sourceSID == "" {
		sessionArg = in.newCodingSID
	}

	// GenerateCode is used (not StreamCmd) purely for its timeout: it floors
	// the context deadline at 30 minutes, which is exactly the coding-stage
	// budget this phase is supposed to share. PermissionMode:"plan" is what
	// turns it into a read-only plan run — StreamOpts already carries the
	// field through to --permission-mode plan, and the persona switches to
	// --append-system-prompt automatically (llm.Gateway.streamArgs).
	cmd, cancel := h.llm.GenerateCode(llm.StreamOpts{
		Prompt:         prompt,
		WorkDir:        in.workDir,
		SystemPrompt:   in.systemPrompt,
		Model:          cliModelArg(in.model),
		ClaudeConfigID: in.claudeConfigID,
		SessionID:      sessionArg,
		Resume:         in.sourceSID != "",
		Fork:           in.fork,
		ForkSessionID:  forkSessionID,
		PermissionMode: "plan",
	})
	defer cancel()

	projectID := ""
	if in.reqRow != nil {
		projectID = in.reqRow.ProjectID
	}
	usage := h.usageCtxForConfig("coding_plan", reqID, projectID, job.ID, in.model, "", "", in.claudeConfigID)
	// codingStallTimeout (20m) rather than architectStallTimeout (10m): the
	// planner may sit silent while Explore sub-agents work, and this phase
	// shares the coding stage's budget by design.
	out := runClaudeStream(jobSink{job}, cmd, "coding-plan", usage, codingStallTimeout)

	// Correct the session id only if the CLI reported one different from what
	// we pre-minted (safety net for --session-id override semantics changing).
	if in.newCodingSID != "" && out.sessionID != "" && out.sessionID != in.newCodingSID {
		if perr := h.reqSvc.UpdateCodingSession(reqID, out.sessionID); perr != nil {
			log.Printf("[coding-plan] failed to persist coding session for %s: %v", reqID, perr)
		}
	}

	if out.staleSession {
		job.Append(store.LogLine{Type: "error", Content: "❌ 源会话已失效，请重新发起对应阶段后再开始开发。"})
		job.Finish(1, store.JobError)
		return "", false
	}

	// Plan capture, same precedence as the architect stage: the plan-file
	// Write tool_use is the primary channel, ExitPlanMode's plan argument the
	// fallback (both land in out.planContent), and the result text the last
	// resort for gateways that don't emit tool_use blocks.
	planMarkdown := strings.TrimSpace(out.planContent)
	if planMarkdown == "" {
		planMarkdown = strings.TrimSpace(out.finalResult)
	}

	if out.errMsg != "" {
		// The plan Write fires BEFORE the model's final API call, so a 504 on
		// that trailing call leaves us holding a usable plan from a run that
		// actually failed. Persist it so the exploration isn't lost, but still
		// fail the job — silently dispatching children off a possibly-partial
		// plan is the worse outcome.
		if planMarkdown != "" && reqID != "" {
			if perr := h.reqSvc.UpdateCodingStepPlan(reqID, planMarkdown); perr != nil {
				log.Printf("[coding-plan] failed to save partial step plan for %s: %v", reqID, perr)
			}
			job.Append(store.LogLine{Type: "message", Content: "ℹ️ 已保存本次捕获到的部分实施步骤，可重新发起开发继续。"})
		}
		job.Append(store.LogLine{Type: "error", Content: "❌ " + out.errMsg})
		if out.stalledByWatchdog != nil && out.stalledByWatchdog.Load() {
			job.SetErrorKind("stalled")
		}
		job.Finish(1, store.JobError)
		return "", false
	}
	if planMarkdown == "" {
		job.Append(store.LogLine{Type: "error", Content: "❌ Claude 未产出实施步骤计划，请重试"})
		job.Finish(1, store.JobError)
		return "", false
	}

	if reqID != "" {
		if perr := h.reqSvc.UpdateCodingStepPlan(reqID, planMarkdown); perr != nil {
			log.Printf("[coding-plan] failed to persist coding_step_plan for %s: %v", reqID, perr)
		}
	}
	job.Append(store.LogLine{Type: "result", Content: planMarkdown})
	return planMarkdown, true
}

// buildPlanPrompt assembles the -p message for the planning turn.
//
// Two shapes, mirroring the direct path's fork/fresh split:
//   - forked off the design (or analysis) session → the conversation already
//     carries the requirement, the analysis and the design, so the lead-in
//     just points at it.
//   - fresh session ("基于方案开发", skip-design rows, or legacy rows with no
//     session chain) → the stored design doc is hand-fed in the prompt, since
//     this session never joined the design conversation.
func (h *WizardHandler) buildPlanPrompt(in *planSplitInput) string {
	// designMarkdown is only attached on the fresh-session path; on a fork the
	// design is already in the conversation and re-feeding it wastes context.
	designMarkdown := ""
	if in.sourceSID == "" && in.reqRow != nil {
		designMarkdown = strings.TrimSpace(in.reqRow.DesignDocs)
	}

	var b strings.Builder
	if in.sourceSID == "" {
		b.WriteString("请基于已确定的技术方案，把本需求拆解为可逐条执行的**实施步骤列表**。\n")
		b.WriteString("你处于 plan 模式：先读取项目中的相关文件核实方案涉及的文件与符号，再输出步骤，不要修改任何代码。\n\n")
	} else {
		b.WriteString("基于已完成的需求分析与技术方案，请把本需求拆解为可逐条执行的**实施步骤列表**。\n")
		b.WriteString("你处于 plan 模式：可按需读取代码核实细节，但不要修改任何代码。\n\n")
	}
	b.WriteString("## 需求\n\n" + in.p.RequirementTitle + "\n\n")
	b.WriteString("## 工作目录\n\n" + in.workDir + "\n\n")
	b.WriteString("## 输出\n\n")
	b.WriteString("一份 Markdown 实施计划，包含编号的步骤列表。每个步骤写清：做什么 / 涉及文件 / 产物形式 / 验收点。\n")
	b.WriteString("**每个步骤必须自包含** —— 执行它的子 Agent 看不到本次对话，也看不到其他步骤，只能看到该步骤的文字。\n")
	b.WriteString("完成后用 Write 工具把计划写入 `~/.claude/plans/<slug>.md`。\n")

	prompt := b.String()
	if designMarkdown != "" {
		prompt += "\n## 技术方案（来自 requirements.design_docs）\n\n" + designMarkdown + "\n"
	}
	if desc := strings.TrimSpace(in.p.RequirementDesc); desc != "" {
		prompt += "\n## 用户在开发前的追加说明\n\n" + desc + "\n"
	}
	// Kind-specific developer tail (currently only fires for kind=issue) keeps
	// an Issue's plan anchored to "最小改动、修复根因" framing.
	if in.reqRow != nil {
		if block := promptpkg.DeveloperBlock(in.reqRow.Kind, in.reqRow); block != "" {
			prompt += "\n" + block + "\n"
		}
	}
	// Context-compression handoff: when the coding stage was previously
	// compressed, prepend the summary so the planner inherits that history.
	// Goes at the TOP so it reads as ground truth rather than a new instruction.
	if in.reqRow != nil && in.reqRow.CodingContextSummary != "" {
		prompt = "## 上下文压缩摘要（之前的开发对话已被压缩，请基于此继续工作，不要当作新指令）\n" +
			in.reqRow.CodingContextSummary + "\n\n" + prompt
	}
	if in.reqRow != nil {
		if block := llm.BuildSkillsBlock(h.mentionedSkills(in.reqRow.Title + " " + in.reqRow.Description)); block != "" {
			prompt = block + prompt
		}
	}
	return prompt
}

// decomposePlanIntoSteps turns the plan Markdown into a dispatchable payload,
// plus the raw step JSON to stash on the batch row for provenance.
//
// Never returns nil: when the HTTP channel is unconfigured, errors, or hands
// back something undecodable, it degrades to a single child carrying the whole
// plan. The UX promise is "勾了拆分就一定有子 Agent 在干活" — stalling at zero
// children because a side-channel LLM was misconfigured would be a much worse
// failure than one coarse-grained task.
func (h *WizardHandler) decomposePlanIntoSteps(
	reqID, planMarkdown string,
	reqRow *model.Requirement,
	job *store.Job,
) (*orchestratorPayload, string) {
	fallback := func(reason string) (*orchestratorPayload, string) {
		log.Printf("[coding-plan] %s: step extraction unavailable (%s) — dispatching the whole plan as one child", reqID, reason)
		job.Append(store.LogLine{Type: "message", Content: "⚠️ 步骤解析不可用（" + reason + "），已降级为 1 个子任务承载完整实施计划。"})
		title := "执行完整实施计划"
		if reqRow != nil && strings.TrimSpace(reqRow.Title) != "" {
			title = truncateForLog(reqRow.Title, 40)
		}
		prompt := "## 实施计划\n\n" + planMarkdown +
			"\n\n> 说明：步骤解析失败，请依据以上完整计划完成整个需求。"
		return &orchestratorPayload{Subtasks: []orchestratedSubtask{{Title: title, Prompt: prompt}}}, ""
	}

	raw, err := h.llm.ExtractStepsFromPlan(planMarkdown)
	if err != nil {
		return fallback(err.Error())
	}
	payload := normalizePayload(decodeSubtasksPayload(extractJSON(raw)))
	if payload == nil || len(payload.Subtasks) == 0 {
		return fallback("模型未返回可解析的步骤列表")
	}
	log.Printf("[coding-plan] %s: extracted %d steps from the implementation plan", reqID, len(payload.Subtasks))
	return payload, raw
}
