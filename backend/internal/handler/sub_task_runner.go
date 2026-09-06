package handler

import (
	"log"
	"os"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

// SubTaskRunner is the shared sub-task executor. It holds the dependencies
// required to spawn a child claude CLI subprocess for a sub_tasks row and
// persist the terminal artifact. Both WizardHandler (manual sub-tasks / auto-
// orchestrated children) and MergeHandler (push + PR sub-task) inject this so
// the runtime semantics stay in one place: any caller creating a sub-task row
// can launch it via Run without re-implementing the goroutine.
//
// Lifecycle (the same as the original WizardHandler.runSubTask):
//   1. Caller inserts a pending sub_tasks row + pre-mints a session id + creates
//      a JobStore job (see NewPendingSubTask for a one-shot helper that does all
//      three). The row's source_session_id must already be set so Run can fork
//      the right parent session.
//   2. Call Run in a goroutine. Run does MarkRunning, spawns the claude CLI
//      with full tool use (executor role persona), parses stream-json events
//      into the job, and on completion writes the Markdown artifact + token
//      usage + cost into sub_tasks.artifact / token columns and finishes the
//      job.
//
// The model is resolved as: explicit modelOverride > developer role's
// configured model. Pass "" through modelOverride to fall back to the role's
// configured model (which itself may be empty → CLI default).
type SubTaskRunner struct {
	projectSvc *service.ProjectService
	subTaskSvc *service.SubTaskService
	jobs       *store.JobStore
	llm        *llm.Gateway
	roleSvc    *service.RoleService
	jobLogSvc  *service.JobLogService
	claudeCfg  *service.ClaudeConfigService
	usageSvc   usageRecorder
	skillSvc   *service.SkillService
}

// NewSubTaskRunner wires the shared sub-task executor. All dependencies are
// required (the runner will panic-via-nil-deref if any is missing — same
// convention as the handler constructors, which all assume a fully wired main).
func NewSubTaskRunner(
	projectSvc *service.ProjectService,
	subTaskSvc *service.SubTaskService,
	jobs *store.JobStore,
	llm *llm.Gateway,
	roleSvc *service.RoleService,
	jobLogSvc *service.JobLogService,
	claudeCfg *service.ClaudeConfigService,
	usageSvc usageRecorder,
	skillSvc *service.SkillService,
) *SubTaskRunner {
	return &SubTaskRunner{
		projectSvc: projectSvc,
		subTaskSvc: subTaskSvc,
		jobs:       jobs,
		llm:        llm,
		roleSvc:    roleSvc,
		jobLogSvc:  jobLogSvc,
		claudeCfg:  claudeCfg,
		usageSvc:   usageSvc,
		skillSvc:   skillSvc,
	}
}

// NewPendingSubTask is a convenience that performs the three pre-Run writes
// (insert the sub_tasks row, persist the pre-minted session id, persist the
// JobStore job id) and returns the row + the job + the newSID that was
// persisted. It does NOT call MarkRunning — that happens inside Run when the
// goroutine actually spawns the CLI, so a crash between NewPendingSubTask
// and the goroutine launch leaves the row in "pending" (RecoverInterrupted
// will surface it as interrupted on restart).
//
// title falls back to truncateForTitle(prompt, 40) when empty so the
// SubTaskPanel always has a card header to render.
//
// modelDisplay is persisted up-front so the SubTaskPanel can show the model
// badge from the moment the row is visible (before MarkRunning stamps anything
// else). Pass "" when the model is unspecified.
func (r *SubTaskRunner) NewPendingSubTask(reqID, title, prompt, modelDisplay, sourceSID string) (*model.SubTask, *store.Job, string, error) {
	st, err := r.subTaskSvc.Create(reqID, title, prompt)
	if err != nil {
		return nil, nil, "", err
	}
	newSID := util.NewUUID()
	if perr := r.subTaskSvc.UpdateSession(st.ID, newSID, sourceSID); perr != nil {
		log.Printf("[sub-task] failed to persist session for %s: %v", st.ID, perr)
	}
	job := r.jobs.Create(reqID)
	if perr := r.subTaskSvc.UpdateJobID(st.ID, job.ID); perr != nil {
		log.Printf("[sub-task] failed to persist job_id for %s: %v", st.ID, perr)
	}
	if modelDisplay != "" {
		if perr := r.subTaskSvc.UpdateModel(st.ID, modelDisplay); perr != nil {
			log.Printf("[sub-task] failed to persist model for %s: %v", st.ID, perr)
		}
		st.Model = modelDisplay
	}
	return st, job, newSID, nil
}

// Run spawns the claude CLI subprocess for a sub-task row and writes the
// final artifact to sub_tasks.artifact on completion.
//
// Side effects on success:
//   - sub_tasks.status transitions to running (via MarkRunning)
//   - the spawned JobStore job is appended with live phase/message lines
//   - on completion, sub_tasks.artifact is filled with a Markdown report
//     wrapping the claude finalResult, and job.Finish is called
//
// Pre: the sub_tasks row is already created with status=pending and has
// its source_session_id populated. NewPendingSubTask should be used to set
// that up; see StartSubTask in wizard.go for the canonical ordering.
//
// body is the user's free-text sub-task description (becomes the prompt body).
// modelOverride, when non-empty, wins over the developer role's configured
// model. adjust flips the prompt header between "## 子任务" and "## 追加调整"
// so the child agent's contextualization stays consistent with the wizard's
// manual sub-task composer.
func (r *SubTaskRunner) Run(
	req *model.Requirement,
	st *model.SubTask,
	job *store.Job,
	newSID string,
	sourceSID string,
	body string,
	modelOverride string,
	adjust bool,
) {
	startTime, mErr := r.subTaskSvc.MarkRunning(st.ID)
	if mErr != nil {
		log.Printf("[sub-task] failed to mark running for %s: %v", st.ID, mErr)
	}
	// Best-effort persistence: backend restart mid-run won't lose the log.
	defer func() {
		lines, status, exitCode := job.Snapshot()
		if perr := r.jobLogSvc.Save(job.ID, st.RequirementID, string(status), exitCode, job.StartedAt, job.FinishedAt, lines, job.Model); perr != nil {
			log.Printf("[sub-task] failed to persist job log %s: %v", job.ID, perr)
		}
	}()

	role := "🤖 调整子任务启动中..."
	if !adjust {
		role = "🤖 子任务启动中..."
	}
	job.Append(store.LogLine{Type: "phase", Content: role})
	job.Append(store.LogLine{Type: "message", Content: "📝 提示词: " + truncateForLog(body, 240)})

	// Resolve the developer role's model, but run the child under the
	// "executor" role system prompt: the sub-task forks the coding session,
	// which carries the developer (统筹协调) persona that decomposes instead
	// of implementing. Without the override the child re-emits
	// [SUBTASKS_READY] and writes no code.
	_, modelName := r.roleConfig("developer")
	if modelOverride != "" {
		modelName = modelOverride
	}
	job.SetModel(modelName)

	// Workdir: prefer the requirement's isolated worktree. Fallback to
	// project checkout for legacy rows.
	workDir := ""
	if r.projectSvc != nil {
		if proj, perr := r.projectSvc.Get(req.ProjectID); perr == nil {
			workDir = proj.LocalPath
		}
	}
	if req.WorktreePath != "" {
		if _, statErr := os.Stat(req.WorktreePath); statErr == nil {
			workDir = req.WorktreePath
		}
	}
	if workDir == "" {
		job.Append(store.LogLine{Type: "error", Content: "❌ 无法解析工作目录"})
		job.Finish(1, store.JobError)
		r.subTaskSvc.Finish(st.ID, model.SubTaskStatusError, buildSubTaskArtifact(st, modelName, "无法解析工作目录", time.Now()), modelName, model.SubTaskTokens{}, 0, startTime)
		return
	}

	var prompt string
	if adjust {
		prompt = "## 追加调整\n\n" + body + "\n"
	} else {
		prompt = "## 子任务\n\n" + body + "\n"
	}
	prompt += "\n> 你是执行者：请直接动手实现本子任务并落盘代码改动，不要再做任务拆分。\n"
	if r.skillSvc != nil {
		if block := llm.BuildSkillsBlock(r.mentionedSkills(req.Title + " " + body)); block != "" {
			prompt = block + prompt
		}
	}

	execSystemPrompt, _ := r.roleConfig(executorRoleKey)
	cmd, cancel := r.llm.GenerateCode(llm.StreamOpts{
		Prompt:       prompt,
		WorkDir:      workDir,
		SystemPrompt: execSystemPrompt,
		Model:        cliModelArg(modelName),
		// --resume <sourceSID> --fork-session --session-id <newSID>:
		// child agent inherits the parent's conversation context but
		// executes in its own session.
		SessionID:     sourceSID,
		Resume:        true,
		Fork:          true,
		ForkSessionID: newSID,
	})
	defer cancel()

	// "sub_task" step key — distinct from "coding" / "adjust_coding" so
	// token-usage rollups don't double-count.
	subUsage := r.usageCtxFor("sub_task", st.RequirementID, req.ProjectID, job.ID, modelName, "", body)
	out := runClaudeStream(jobSink{job}, cmd, "sub-task", subUsage)

	finalStatus := model.SubTaskStatusDone
	var artifactBody string
	switch {
	case out.staleSession:
		finalStatus = model.SubTaskStatusError
		artifactBody = "❌ 源会话已失效（session 文件不存在），请重新发起 coding 后再试。"
		job.Append(store.LogLine{Type: "error", Content: artifactBody})
	case out.errMsg != "":
		finalStatus = model.SubTaskStatusError
		artifactBody = "❌ " + out.errMsg
		job.Append(store.LogLine{Type: "error", Content: artifactBody})
	case out.finalResult == "":
		finalStatus = model.SubTaskStatusError
		artifactBody = "❌ Claude 未返回结果，请重试"
		job.Append(store.LogLine{Type: "error", Content: artifactBody})
	default:
		job.Append(store.LogLine{Type: "result", Content: strings.TrimSpace(out.finalResult)})
		artifactBody = out.finalResult
	}
	job.Append(store.LogLine{Type: "done", Content: "✅ 子任务完成！"})

	artifact := buildSubTaskArtifact(st, modelName, artifactBody, time.Now())
	tokens := model.SubTaskTokens{
		Input:         out.lastUsage.InputTokens,
		Output:        out.lastUsage.OutputTokens,
		CacheCreation: out.lastUsage.CacheCreationTokens,
		CacheRead:     out.lastUsage.CacheReadTokens,
	}
	costCents := computeSubTaskCostCents(modelName, tokens, r.claudeCfg)
	if perr := r.subTaskSvc.Finish(st.ID, finalStatus, artifact, modelName, tokens, costCents, startTime); perr != nil {
		log.Printf("[sub-task] failed to persist finish for %s: %v", st.ID, perr)
	}
	job.Finish(0, store.JobDone)
	log.Printf("[sub-task] job %s finished for %s status=%s", job.ID, st.ID, finalStatus)
}

// roleConfig loads a role's system prompt + model by key. On miss it returns
// empty strings so a broken role config never blocks the sub-task.
func (r *SubTaskRunner) roleConfig(key string) (systemPrompt, model string) {
	if r.roleSvc == nil {
		return "", ""
	}
	rr, err := r.roleSvc.GetByKey(key)
	if err != nil {
		log.Printf("[sub-task] role %q not found, using CLI defaults: %v", key, err)
		return "", ""
	}
	return rr.SystemPrompt, rr.Model
}

// usageCtxFor builds a usageCtx for one sub-task claude invocation. Mirrors
// WizardHandler.usageCtxFor but takes the runner's deps directly.
func (r *SubTaskRunner) usageCtxFor(step, requirementID, projectID, jobID, model, meta, summary string) *usageCtx {
	configID, currency := activeConfigMetaFor(r.claudeCfg)
	return &usageCtx{
		Rec:            r.usageSvc,
		RequirementID:  requirementID,
		ProjectID:      projectID,
		JobID:          jobID,
		Step:           step,
		Model:          model,
		ClaudeConfigID: configID,
		Currency:       currency,
		Meta:           meta,
		Summary:        summary,
	}
}

// mentionedSkills parses @slug mentions from text and returns the matching
// skill rows. Same as WizardHandler.mentionedSkills but operates on the
// runner's skillSvc so the runner stays decoupled from WizardHandler.
func (r *SubTaskRunner) mentionedSkills(text string) []struct{ Slug, Content string } {
	if r.skillSvc == nil {
		return nil
	}
	slugs := parseAtMentions(text)
	if len(slugs) == 0 {
		return nil
	}
	skills, _ := r.skillSvc.SkillsBySlug(slugs)
	return skills
}

// activeConfigMetaFor is a free-function equivalent of the per-handler
// activeConfigMeta methods. Returns the active claude config's id + currency;
// empty on no config / any lookup error so a pricing gap never blocks a
// sub-task run.
func activeConfigMetaFor(claudeCfg *service.ClaudeConfigService) (id, currency string) {
	if claudeCfg == nil {
		return "", ""
	}
	c, err := claudeCfg.ActiveConfig()
	if err != nil || c == nil {
		return "", ""
	}
	return c.ID, c.Currency
}
