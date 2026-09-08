package handler

import (
	"fmt"
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

// DefaultSubTaskConcurrency caps how many claude CLI subprocesses the
// SubTaskRunner can have alive at once. Without this cap, a user clicking
// "追加子任务" repeatedly (or the auto-orchestrator fan-out path) can spawn
// many parallel Node.js children; on macOS this approaches the jetsam
// threshold and on Linux it triggers the OOM killer — both manifest as an
// unexpected SIGKILL on the parent nova process with no recoverable signal
// handler (see plan-ancient-snail.md for the root-cause analysis). Tunable
// via NOVA_SUBTASK_CONCURRENCY at construction time.
const DefaultSubTaskConcurrency = 4

// SubTaskRunner is the shared sub-task executor. It holds the dependencies
// required to spawn a child claude CLI subprocess for a sub_tasks row and
// persist the terminal artifact. Both WizardHandler (manual sub-tasks / auto-
// orchestrated children) and MergeHandler (push + PR sub-task) inject this so
// the runtime semantics stay in one place: any caller creating a sub-task row
// can launch it via Run without re-implementing the goroutine.
//
// Lifecycle (the same as the original WizardHandler.runSubTask):
//  1. Caller inserts a pending sub_tasks row + pre-mints a session id + creates
//     a JobStore job (see NewPendingSubTask for a one-shot helper that does all
//     three). The row's source_session_id must already be set so Run can fork
//     the right parent session.
//  2. Call Run in a goroutine. Run does MarkRunning, spawns the claude CLI
//     with full tool use (executor role persona), parses stream-json events
//     into the job, and on completion writes the Markdown artifact + token
//     usage + cost into sub_tasks.artifact / token columns and finishes the
//     job.
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
	// remoteCoding is set by the wizard handler at construction time so the
	// runner can dispatch children to the requirement's Agent server without
	// importing wizard.go (which would create a circular dep). Nil keeps every
	// child local; a non-nil value is required to honor dev_source="agent".
	remoteCoding func(*remoteCodingInput) claudeStreamOutcome
	skillSvc     *service.SkillService
	// agentSvrSvc resolves the Agent server a requirement was developed on.
	// When the parent requirement carries an agent_server_id, Run dispatches
	// the child to that server instead of spawning a local CLI — the child's
	// working tree lives on the agent host, so a local run would edit a stale
	// (or missing) checkout. Nil keeps every child local.
	agentSvrSvc *service.AgentServerService
	// runSem caps the number of claude CLI subprocesses that may be alive
	// at once. Buffered channel used as a counting semaphore: every Run
	// acquires a slot on entry (blocking when full) and releases on return.
	// Sized by NewSubTaskRunner's runConcurrency argument (env
	// NOVA_SUBTASK_CONCURRENCY; default DefaultSubTaskConcurrency).
	runSem chan struct{}
}

// NewSubTaskRunner wires the shared sub-task executor. All dependencies are
// required (the runner will panic-via-nil-deref if any is missing — same
// convention as the handler constructors, which all assume a fully wired main).
//
// runConcurrency caps how many concurrent Run invocations can hold a slot.
// Pass 0 to use DefaultSubTaskConcurrency; pass a positive int to override
// (env NOVA_SUBTASK_CONCURRENCY is the intended source). Negative values are
// treated as 0.
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
	agentSvrSvc *service.AgentServerService,
	remoteCoding func(*remoteCodingInput) claudeStreamOutcome,
	runConcurrency int,
) *SubTaskRunner {
	if runConcurrency <= 0 {
		runConcurrency = DefaultSubTaskConcurrency
	}
	return &SubTaskRunner{
		agentSvrSvc:  agentSvrSvc,
		projectSvc:   projectSvc,
		subTaskSvc:   subTaskSvc,
		jobs:         jobs,
		llm:          llm,
		roleSvc:      roleSvc,
		jobLogSvc:    jobLogSvc,
		claudeCfg:    claudeCfg,
		usageSvc:     usageSvc,
		skillSvc:     skillSvc,
		remoteCoding: remoteCoding,
		runSem:       make(chan struct{}, runConcurrency),
	}
}

// SetRemoteCoding injects WizardHandler.runRemoteCoding after construction.
// main builds the wizard handler with the runner, and the runner needs the
// wizard's remote path, so the reference is wired once both exist rather than
// forcing a constructor cycle. Passing nil disables remote dispatch (children
// always run locally).
func (r *SubTaskRunner) SetRemoteCoding(fn func(*remoteCodingInput) claudeStreamOutcome) {
	r.remoteCoding = fn
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
	configIDOverride string,
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

	// Concurrency cap: multiple sub-tasks can fan out at once (manual clicks
	// or auto-orchestrator fan-out), but unlimited concurrency tips the OS OOM /
	// macOS jetsam killer into SIGKILLing the parent nova process — a signal
	// Go cannot intercept, which is exactly the "system exits for no reason"
	// symptom. Block here when over cap; the wait log lands in the job panel
	// so the user sees the queue, not a frozen UI.
	select {
	case r.runSem <- struct{}{}:
		// slot acquired immediately
	default:
		job.Append(store.LogLine{Type: "phase", Content: fmt.Sprintf("⏳ 等待空闲 worker slot（并发上限 %d 已满）...", cap(r.runSem))})
		r.runSem <- struct{}{}
	}
	defer func() { <-r.runSem }()

	role := "🤖 调整子任务启动中..."
	if !adjust {
		role = "🤖 子任务启动中..."
	}
	job.Append(store.LogLine{Type: "phase", Content: role})
	job.Append(store.LogLine{Type: "message", Content: "📝 提示词: " + truncateForLog(body, 240)})

	// Resolve the developer role's model + its bound config id. The model
	// drives which base URL the child should hit: when the user picks a
	// model from a non-active claude_configs row (or the developer role is
	// bound to a different config than the executor role), the executor's
	// binding would send the request to the wrong gateway. We resolve the
	// matching config below so model and base URL always agree.
	_, devModel, devCfgID := r.roleConfig("developer")
	modelName := devModel
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

	execSystemPrompt, _, executorConfigID := r.roleConfig(executorRoleKey)
	// Pick the Claude config the resolved model actually belongs to. Priority:
	//   1. caller-supplied configIDOverride (merge push path — keeps the
	//      pr_author-role binding explicit and bypasses the model lookup)
	//   2. the model-override's owning config (user explicitly picked a
	//      model from another config; without this we'd send the override
	//      model to the developer/executor role's binding and the wrong
	//      gateway would 400 on the unknown model id)
	//   3. the developer role's bound config (covers the developer-default
	//      model + its role-bound gateway)
	//   4. the executor role's bound config (legacy fallback for users who
	//      rely on executor-role binding only; harmless when 1–3 match)
	var finalConfigID string
	switch {
	case configIDOverride != "":
		finalConfigID = configIDOverride
	case modelOverride != "":
		if cid, cerr := r.claudeCfg.ResolveConfigForModel(modelOverride); cerr == nil && cid != "" {
			finalConfigID = cid
		} else if devCfgID != "" {
			finalConfigID = devCfgID
		} else {
			finalConfigID = executorConfigID
		}
	case devCfgID != "":
		finalConfigID = devCfgID
	default:
		finalConfigID = executorConfigID
	}

	// "sub_task" step key — distinct from "coding" / "adjust_coding" so
	// token-usage rollups don't double-count.
	subUsage := r.usageCtxFor("sub_task", st.RequirementID, req.ProjectID, job.ID, modelName, "", body)

	// Execution-consistency: when the parent requirement was developed on an
	// Agent server, its code lives in that host's worktree — a locally-spawned
	// child would edit a stale (or missing) checkout and its commits would
	// never reach the branch. Dispatch the child to the SAME server; the
	// remote helper reuses the per-requirement remote worktree and pushes on
	// success, so the requirement's branch stays the single source of truth.
	// This covers every child dispatch that goes through Run: manual sub-tasks,
	// orchestrated children, and the merge push+PR sub-task.
	if req.AgentServerID != "" && r.agentSvrSvc != nil && r.remoteCoding != nil {
		out := r.remoteCoding(&remoteCodingInput{
			job:      job,
			serverID: req.AgentServerID,
			req: startCodingReq{
				RequirementTitle: req.Title + " / " + st.Title,
				RequirementID:    req.ID,
				BranchName:       req.BranchName,
				AgentServerID:    req.AgentServerID,
			},
			reqRow:         req,
			prompt:         prompt,
			sourceSID:      sourceSID,
			fork:           true,
			sessionArg:     sourceSID,
			forkSessionID:  newSID,
			model:          modelName,
			claudeConfigID: finalConfigID,
			usage:          subUsage,
		})
		r.finishSubTask(st, job, out, modelName, startTime)
		return
	}

	cmd, cancel := r.llm.GenerateCode(llm.StreamOpts{
		Prompt:         prompt,
		WorkDir:        workDir,
		SystemPrompt:   execSystemPrompt,
		Model:          cliModelArg(modelName),
		ClaudeConfigID: finalConfigID,
		// --resume <sourceSID> --fork-session --session-id <newSID>:
		// child agent inherits the parent's conversation context but
		// executes in its own session.
		SessionID:     sourceSID,
		Resume:        true,
		Fork:          true,
		ForkSessionID: newSID,
	})
	defer cancel()
	out := runClaudeStream(jobSink{job}, cmd, "sub-task", subUsage)
	r.finishSubTask(st, job, out, modelName, startTime)
}

// finishSubTask persists the terminal state shared by the local and remote
// sub-task paths: map the three failure shapes onto an error artifact, or
// record the result + token usage + cost, then finish the job.
func (r *SubTaskRunner) finishSubTask(st *model.SubTask, job *store.Job, out claudeStreamOutcome, modelName string, startTime time.Time) {
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
		// Belt-and-suspenders: out.finalResult is already cleaned in
		// runClaudeStream (wizard.go), but apply the same filter here so a
		// sub-task artifact that lands in sub_tasks.artifact is guaranteed
		// clean even if the upstream caller was bypassed.
		cleaned := store.CleanThinkTags(strings.TrimSpace(out.finalResult))
		if cleaned == "" {
			// MiniMax-M3 emitted only think-tag wrappers (no real result text).
			// Treat as an error to mirror the empty-finalResult branch above.
			finalStatus = model.SubTaskStatusError
			artifactBody = "❌ Claude 未返回有效结果，请重试"
			job.Append(store.LogLine{Type: "error", Content: artifactBody})
		} else {
			job.Append(store.LogLine{Type: "result", Content: cleaned})
			artifactBody = cleaned
		}
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

// roleConfig loads a role's system prompt + model + the Claude config the
// role is bound to (so the sub-task executor can run against the role's
// chosen gateway, not just the global active one). On miss it returns
// empty strings so a broken role config never blocks the sub-task.
func (r *SubTaskRunner) roleConfig(key string) (systemPrompt, model, configID string) {
	if r.roleSvc == nil {
		return "", "", ""
	}
	rr, err := r.roleSvc.GetByKey(key)
	if err != nil {
		log.Printf("[sub-task] role %q not found, using CLI defaults: %v", key, err)
		return "", "", ""
	}
	cfg, _ := r.claudeCfg.ResolveRoleConfig(rr)
	cid := ""
	if cfg != nil {
		cid = cfg.ID
	}
	return rr.SystemPrompt, rr.Model, cid
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
