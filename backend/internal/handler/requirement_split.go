package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

// 「方案拆分」的 HTTP 层。
//
// 拆分走两段式，与项目里 analyst / architect / developer 三个阶段的闸门同构：
// AI 先产出**候选**（用户可编辑 / 删除 / 排序 / 指定前置），人工确认后才落库。
// AI 不自证完成 —— 这是 CLAUDE.md 记载的既有约定。
//
// 方法挂在 *WizardHandler 上（而不是 RequirementHandler）是因为拆分需要
// architect persona（roleSvc）、claude 配置（claudeCfg）、CLI 网关（llm）和
// JobStore（jobs），这四样 WizardHandler 全都有；RequirementHandler 缺前两样。
//
// 主路径是本地 claude CLI（禁写工具，只能读仓库把文件路径坐实）；CLI 不可用
// 时降级到直接 HTTP LLM 通道。
//
// 执行环境由用户在 UI 上选：留空 = 本机执行（上面的主路径）；填了
// agent_server_id = 跑在那台 Agent Server 上。远端路径是**只读**运行 ——
// scratch clone + 一次 nova-agent-worker 调用，与远端 Review 同构
// （见 requirement_split_remote.go）。

// splitSplitStallTimeout — 拆分这一轮的静默看门狗时限。
//
// 与阶段答疑的 qaStallTimeout 同量级（10 分钟）：拆分要读方案、可能要读仓库
// 把 scope 坐实，比纯文本转换慢，但不该无限等。
const splitSplitStallTimeout = 10 * time.Minute

// splitMaxCandidates — 落库前的候选数量上限（与 prompt 契约里的 12 一致）。
const splitMaxCandidates = 12

// SplitReq — POST /api/requirements/{id}/split 的请求体。
type SplitReq struct {
	// Model — 用户在 UI 上选的模型（空 = 用 architect persona 的默认模型）。
	Model string `json:"model"`
	// ClaudeConfigID — 用户在 UI 上选的 Claude 配置（空 = 用当前激活配置）。
	ClaudeConfigID string `json:"claude_config_id"`
	// AgentServerID — 用户在 UI 上选的执行环境（空 = 本机执行）。
	// 拆分是只读运行，远端走 scratch clone + nova-agent-worker，
	// 与远端 Review 同构（见 requirement_split_remote.go）。
	AgentServerID string `json:"agent_server_id"`
}

// splitExecTarget 描述这一轮拆分跑在哪里。
type splitExecTarget struct {
	// Remote = true 表示跑在 Agent Server 上。
	Remote bool
	// AgentServerID — Remote 为 true 时非空。
	AgentServerID string
}

// splitExecFunc — 拆分这一轮「跑一次 AI + 解析候选」的执行体。
//
// 生产为 nil → execSplitTasks 走默认实现（本地 claude CLI / 远端 nova-agent-worker）。
// 测试注入假实现来断言「用户选的执行环境有没有被真正用上」—— WizardHandler.llm
// 是具体类型 *llm.Gateway、SSH 也是具体类型 *gossh.Client，没有别的替身方式。
type splitExecFunc func(job *store.Job, reqRow *model.Requirement, target splitExecTarget,
	promptText, systemPrompt, modelName, configID string, usage *usageCtx) ([]prompt.SplitTask, string)

// ConfirmSplitReq — POST /api/requirements/{id}/split/confirm 的请求体。
type ConfirmSplitReq struct {
	// Tasks — 用户确认（可能编辑过）的候选子需求。depends_on 里存的是**批次
	// 内序号**（1 起算），不是数据库 id。
	Tasks []model.SplitTaskInput `json:"tasks"`
	// References — 用户勾选的「互相参考」子需求对，元素是 [引用方序号,
	// 被引用方序号]（均 1 起算）。依赖（闸门）与引用（上下文参考）是两件事：
	// 引用只影响方案提示词，不阻塞任何流程。
	References [][2]int `json:"references"`
	// SourceBranch — 用户选的**来源分支**：主需求自己的开发分支
	// （feat/req_<master>）由它 checkout 出来，拆分时还会被推送到远端。
	// 留空则沿用主需求当前的基线。
	SourceBranch string `json:"source_branch"`
}

// splitState holds the per-handler mutable state of the split feature.
type splitState struct {
	// runs — 权威登记处（状态 + 候选 + requirementID→jobID 映射）。
	runs *splitRuns
	// locks — 并发闸门（同一需求同时只允许跑一次拆分）。
	locks *splitLocks
}

// splitState returns the handler's split state, creating it on first use.
//
// The handler is normally built by NewWizardHandler (which initializes it),
// but tests build partial handlers with struct literals — those have a nil
// state, so it is created lazily here. The package-level mutex guards the
// lazy path; the initialized state is never replaced afterwards.
var splitStateInitMu sync.Mutex

func (h *WizardHandler) splitState() *splitState {
	splitStateInitMu.Lock()
	defer splitStateInitMu.Unlock()
	if h.split == nil {
		h.split = &splitState{runs: newSplitRuns(splitRunsCap), locks: newSplitLocks()}
	}
	return h.split
}

// ── POST /api/requirements/{id}/split ─────────────────────────────────────

// Split starts an AI split of the requirement's design into candidate
// sub-requirements.
//
// Preconditions (all rejected with an explicit error code BEFORE a job is
// created — the user must be told why, not shown a spinner that dies):
//
//	wiki kind                → SPLIT_KIND_UNSUPPORTED
//	no design docs           → SPLIT_NO_DESIGN
//	the requirement is itself a child → SPLIT_PARENT_IS_CHILD
//	archived                 → SPLIT_PARENT_ARCHIVED
//	already split (dev_delegated) → SPLIT_ALREADY_SPLIT
//	currently developing      → SPLIT_ALREADY_DEVELOPED
//	a split is already running → SPLIT_IN_PROGRESS (409)
//
// NOTE: the "already developed" test used to be `branch_name != ""`, which
// was wrong — the DESIGN stage writes branch_name (anchorWorktree), so a
// requirement that had merely been designed looked "already in development"
// and could never be split. The gate is the state machine plus the
// dev_delegated flag instead. See splitPreconditionError.
//
// Returns {"job_id": ...}; the candidate list arrives on the job's SSE stream.
func (h *WizardHandler) Split(w http.ResponseWriter, r *http.Request) {
	reqID := r.PathValue("id")

	var body SplitReq
	// A missing or malformed body is not fatal — the split works without a
	// model override. Ignore it and continue with the persona defaults.
	if err := decodeJSON(r, &body); err != nil && !errors.Is(err, io.EOF) {
		log.Printf("[split] %s: ignoring unreadable body: %v", reqID, err)
	}

	if h.reqSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "拆分服务未启用")
		return
	}

	reqRow, err := h.reqSvc.Get(reqID)
	if err != nil {
		writeError(w, http.StatusNotFound, "REQUIREMENT_NOT_FOUND", "需求不存在")
		return
	}

	if code, msg := splitPreconditionError(reqRow); code != "" {
		writeError(w, http.StatusBadRequest, code, msg)
		return
	}

	st := h.splitState()
	// Peek before creating the job: a refused second split must not leave a
	// stray job sitting in the ring buffer.
	if existing, running := st.locks.peek(reqID); running {
		writeError(w, http.StatusConflict, "SPLIT_IN_PROGRESS",
			fmt.Sprintf("已有拆分任务在运行（job %s），请等待结束", existing))
		return
	}

	job := h.jobs.Create(reqID)
	job.SetType("requirement_split")

	// The authoritative check (atomic). If another request won the race
	// between the peek and here, finish our job and bail.
	//
	// The run is registered only AFTER the lock is won: registering first
	// would let a job that never ran overwrite latest[reqID] and hide the
	// run that IS running.
	if existing, ok := st.locks.acquire(reqID, job.ID); !ok {
		job.Finish(1, store.JobError)
		writeError(w, http.StatusConflict, "SPLIT_IN_PROGRESS",
			fmt.Sprintf("已有拆分任务在运行（job %s），请等待结束", existing))
		return
	}

	// 登记 run —— 必须在 writeJSON 之前：前端拿到 job_id 后可能立刻查 state，
	// 若此时还没登记，state 会误报 "none"，弹窗就会以为「从没跑过」。
	st.runs.start(job.ID, reqID, strings.TrimSpace(body.Model), strings.TrimSpace(body.AgentServerID))

	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})

	go h.runSplit(job, reqRow, body, st)
}

// splitPreconditionError returns the error code + message when the
// requirement may NOT be split. Empty code means "may split".
//
// The "already developed" gate reads the STATE MACHINE, not branch_name:
// the design stage creates the worktree and writes branch_name (see
// anchorWorktree → UpdateWorktree), so a requirement that had only been
// designed would look like it was already in development. Judging by
// branch_name made branch-mode splitting a deadlock — 321 of the 476
// requirements that had a design in the wild were blocked by it.
//
// Two distinct gates remain:
//
//   - dev_delegated → SPLIT_ALREADY_SPLIT. The master already handed its
//     development down to a batch of children; splitting it again would
//     produce a second, conflicting batch.
//   - status == "developing" → SPLIT_ALREADY_DEVELOPED. The requirement is
//     mid-development right now, so its branch has real commits on it.
//
// A "done" requirement stays splittable: topping up the task breakdown of
// a finished requirement is a legitimate request.
func splitPreconditionError(req *model.Requirement) (code, msg string) {
	if req == nil {
		return "SPLIT_NOT_FOUND", "需求不存在"
	}
	if strings.EqualFold(strings.TrimSpace(req.Kind), service.KindWiki) {
		return "SPLIT_KIND_UNSUPPORTED", "知识库条目不支持拆分"
	}
	if strings.TrimSpace(req.DesignDocs) == "" || strings.TrimSpace(req.DesignDocs) == "[]" {
		return "SPLIT_NO_DESIGN", "请先生成技术方案，再拆分任务"
	}
	if strings.TrimSpace(req.ParentID) != "" {
		return "SPLIT_PARENT_IS_CHILD", "子需求不能再拆分（需求层级仅支持一层）"
	}
	if req.Status == "archived" {
		return "SPLIT_PARENT_ARCHIVED", "已归档的需求不能拆分"
	}
	if req.DevDelegated {
		return "SPLIT_ALREADY_SPLIT", "该需求已拆分，开发已下放给子需求，不能重复拆分"
	}
	if req.Status == "developing" {
		return "SPLIT_ALREADY_DEVELOPED", "该需求已进入开发，不能再拆分"
	}
	return "", ""
}

// runSplit is the goroutine body: build the prompt, run the split (locally or
// on an Agent Server), parse the result and publish it on the job.
//
// It is deliberately split into three phases — prepare / execute / publish —
// so the execution layer (execSplitTasks) is the single place where the
// "local vs remote" decision lives.
func (h *WizardHandler) runSplit(job *store.Job, reqRow *model.Requirement, body SplitReq, st *splitState) {
	defer st.locks.release(reqRow.ID)

	// The lock is released when the job ends — make sure that happens even
	// on a panic.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[split] %s: panic: %v", reqRow.ID, r)
			st.runs.finish(job.ID, store.JobError, "拆分过程异常终止", nil)
			job.Append(store.LogLine{Type: "error", Content: "拆分过程异常终止"})
			job.Finish(1, store.JobError)
		}
	}()

	existing, err := h.reqSvc.ListChildren(reqRow.ID)
	if err != nil {
		h.finishSplitError(job, st, "读取已有子需求失败: "+err.Error())
		return
	}

	systemPrompt, modelName, configID := h.roleConfig("architect")
	// An explicit per-request model / config wins over the persona default.
	if strings.TrimSpace(body.Model) != "" {
		modelName = body.Model
	}
	if strings.TrimSpace(body.ClaudeConfigID) != "" {
		configID = body.ClaudeConfigID
	}
	// The effective model is resolved here, not from the request body — the
	// state endpoint must report the model that actually ran.
	st.runs.update(job.ID, func(r *splitRun) { r.Model = modelName })

	promptText := prompt.BuildSplitPrompt(reqRow, reqRow.DesignDocs, existing)
	usage := h.usageCtxForConfig("requirement_split", reqRow.ID, reqRow.ProjectID, job.ID, modelName, "", "", configID)

	job.Append(store.LogLine{Type: "phase", Content: "🧩 正在分析方案并拆分任务…"})

	tasks, errMsg := h.execSplitTasks(job, reqRow, body, promptText, systemPrompt, modelName, configID, usage)
	if errMsg != "" {
		h.finishSplitError(job, st, errMsg)
		return
	}
	if len(tasks) == 0 {
		h.finishSplitError(job, st, "未能从技术方案中解析出候选子需求，请调整方案后重试")
		return
	}

	if len(tasks) > splitMaxCandidates {
		job.Append(store.LogLine{Type: "phase", Content: fmt.Sprintf("⚠️ 候选超过 %d 条，已截断", splitMaxCandidates)})
		tasks = tasks[:splitMaxCandidates]
	}

	// 登记终态必须在 job.Finish 之前 —— store.Job.Finish 会关闭所有订阅者
	// channel，订阅者醒来时读到的必须是已填好的状态。
	st.runs.finish(job.ID, store.JobDone, "", tasks)
	job.Append(store.LogLine{Type: "message", Content: prompt.SplitSummary(tasks)})
	job.Finish(0, store.JobDone)
}

// execSplitTasks is the single place where the split's execution environment
// is decided: an empty agent_server_id means "run on this machine", anything
// else means "run on that Agent Server".
//
// The injectable h.splitExec short-circuits the whole thing — production
// leaves it nil and the default local / remote implementations run.
func (h *WizardHandler) execSplitTasks(job *store.Job, reqRow *model.Requirement, body SplitReq,
	promptText, systemPrompt, modelName, configID string, usage *usageCtx) ([]prompt.SplitTask, string) {
	target := splitExecTarget{AgentServerID: strings.TrimSpace(body.AgentServerID)}
	target.Remote = target.AgentServerID != ""
	if h.splitExec != nil {
		return h.splitExec(job, reqRow, target, promptText, systemPrompt, modelName, configID, usage)
	}
	if target.Remote {
		return h.runRemoteSplitTasks(job, reqRow, target.AgentServerID, promptText, systemPrompt, modelName, configID, usage)
	}
	return h.runLocalSplitTasks(job, reqRow, promptText, systemPrompt, modelName, configID, usage)
}

// runLocalSplitTasks runs the split on this machine: the claude CLI with the
// write tools disabled (it may only read the repository so it can ground the
// scope in real file paths), falling back to the direct HTTP LLM channel when
// the CLI produced nothing parseable.
//
// The fallback is the local path's last line of defense — it can only reach
// the HTTP channel from here, which is why the remote path has no such
// fallback (see requirement_split_remote.go).
func (h *WizardHandler) runLocalSplitTasks(job *store.Job, reqRow *model.Requirement,
	promptText, systemPrompt, modelName, configID string, usage *usageCtx) ([]prompt.SplitTask, string) {
	workDir := h.splitWorkDir(reqRow)

	cmd := h.llm.StreamCmd(context.Background(), llm.StreamOpts{
		Prompt:         promptText,
		WorkDir:        workDir,
		SystemPrompt:   systemPrompt,
		Model:          cliModelArg(modelName),
		ClaudeConfigID: configID,
		// Read-only: the split must not touch the repository. Read / Glob /
		// Grep stay available so the model can ground the scope in real file
		// paths.
		DisallowedTools: []string{"Write", "Edit", "Bash", "NotebookEdit"},
	})

	out := runClaudeStream(jobSink{job}, cmd, "requirement-split", usage, nil, nil, 0, splitSplitStallTimeout)

	jsonPart, _ := extractTrailingJSON(out.finalResult)
	if strings.TrimSpace(jsonPart) == "" {
		jsonPart = out.planContent
	}
	tasks := prompt.NormalizeSplitTasks(jsonPart)

	if len(tasks) == 0 {
		// CLI path produced nothing usable — fall back to the direct HTTP
		// channel before giving up.
		job.Append(store.LogLine{Type: "phase", Content: "⚠️ 本地 CLI 未产出可解析的拆分结果，改用直接 LLM 通道重试…"})
		raw, ferr := h.llm.ExtractSubRequirementsFromDesign(reqRow.DesignDocs, out.errMsg)
		if ferr != nil {
			return nil, "拆分失败: " + ferr.Error()
		}
		tasks = prompt.NormalizeSplitTasks(raw)
		if len(tasks) == 0 {
			return nil, "未能从技术方案中解析出候选子需求，请调整方案后重试"
		}
	}

	return tasks, ""
}

// finishSplitError reports a failed split on the job stream.
//
// The failure reason is recorded BOTH on the registry (so the state endpoint
// can serve it after the modal is reopened) and as an "error" log line (so it
// shows up in the live stream).
func (h *WizardHandler) finishSplitError(job *store.Job, st *splitState, msg string) {
	st.runs.finish(job.ID, store.JobError, msg, nil)
	job.Append(store.LogLine{Type: "error", Content: msg})
	job.Finish(1, store.JobError)
}

// splitWorkDir picks the directory the split run may read from.
//
// The split is read-only, so the work dir only decides WHICH files the model
// can see. Prefer the requirement's worktree (the exact tree the design was
// written against), then the project root, then the temp dir.
func (h *WizardHandler) splitWorkDir(req *model.Requirement) string {
	if p := strings.TrimSpace(req.WorktreePath); p != "" {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p
		}
	}
	if h.projectSvc != nil && req.ProjectID != "" {
		if proj, err := h.projectSvc.Get(req.ProjectID); err == nil && proj != nil {
			if p := strings.TrimSpace(proj.LocalPath); p != "" {
				if st, err := os.Stat(p); err == nil && st.IsDir() {
					return p
				}
			}
		}
	}
	return os.TempDir()
}

// ── GET /api/requirements/{id}/split/{job_id}/stream ───────────────────────

// StreamSplitJob streams the split job's log lines and, on the terminal
// frame, the parsed candidate list.
//
// Why a dedicated endpoint instead of the shared GET /api/wizard/jobs/{id}
// stream: the shared handler's done frame has a fixed field set (type /
// status / exit_code / error_kind / timestamps) and store.LogLine has no
// structured payload field — so there is nowhere to put the candidates
// without changing a channel that ten other features depend on.
// streamJobSSE takes a doneFrame callback precisely so each domain can carry
// its own fields (see preflight.go / agent_server.go), and this is that same
// pattern.
func (h *WizardHandler) StreamSplitJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	if jobID == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "missing job id")
		return
	}
	job, ok := h.jobs.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found")
		return
	}

	st := h.splitState()
	streamJobSSE(w, r, job, func(status store.JobStatus, exitCode int) []byte {
		var result any
		var errMsg string
		if run := st.runs.get(jobID); run != nil {
			errMsg = run.Error
			// 终帧的 result 统一是对象 {"tasks":[...]}。
			//
			// 旧实现把候选裸数组塞进 result，而前端读的是 result.tasks ——
			// 两者永远对不上，拆分在 UI 上一直是坏的（每次都报"结果已失效"）。
			// 对象形态可扩展（将来加 model / exec_env 不破坏前端），前端
			// 同时兼容裸数组 / {tasks} / {candidates} 三种形态。
			tasks := run.Candidates
			if tasks == nil {
				tasks = []prompt.SplitTask{} // tasks 永远是数组，不是 null
			}
			result = map[string]any{"tasks": tasks}
		}
		doneData, _ := json.Marshal(map[string]interface{}{
			"type":        "job_done",
			"status":      string(status),
			"exit_code":   exitCode,
			"error_kind":  job.ErrorKind,
			// 新增：后端给的用户可见中文失败原因。前端拿它当错误条文案，
			// 不用再拼一个空泛的"拆分失败："。
			"error_message": errMsg,
			"started_at":    job.StartedAt.UnixMilli(),
			"finished_at":   job.FinishedAt.UnixMilli(),
			"duration_ms":   job.FinishedAt.Sub(job.StartedAt).Milliseconds(),
			"result":        result,
		})
		return doneData
	})
}

// ── GET /api/requirements/{id}/split/state ──────────────────────────────────

// GetSplitState reports the most recent split run of a requirement.
//
// 这是拆分唯一的"状态可发现"入口：弹窗挂载时先查这里，据此决定进入
// 配置 / 运行中 / 候选复核 三个阶段中的哪一个 —— 关掉弹窗再打开时，用户能看到
// 上一轮拆分的状态与候选，而不是被迫重跑一次 AI。
//
// "没跑过"是正常状态不是错误：回 200 + {"status":"none"}，不是 404。
func (h *WizardHandler) GetSplitState(w http.ResponseWriter, r *http.Request) {
	reqID := r.PathValue("id")
	st := h.splitState()
	run := st.runs.latestFor(reqID)
	if run == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":          "none",
			"job_id":          "",
			"running":         false,
			"error_message":   "",
			"candidates":      []prompt.SplitTask{},
			"model":           "",
			"exec_env":        "",
			"agent_server_id": "",
			"created_at":      int64(0),
			"finished_at":     int64(0),
		})
		return
	}
	execEnv := "local"
	if strings.TrimSpace(run.AgentServerID) != "" {
		execEnv = "agent-server"
	}
	// 零值 time 的 UnixMilli 是个很大的负数 —— 用 IsZero() 归一成 0，
	// 前端只在 finished_at > 0 时显示时间。
	var created, finished int64
	if !run.CreatedAt.IsZero() {
		created = run.CreatedAt.UnixMilli()
	}
	if !run.FinishedAt.IsZero() {
		finished = run.FinishedAt.UnixMilli()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          string(run.Status), // running | done | error
		"job_id":          run.JobID,
		"running":         run.Status == store.JobRunning,
		"error_message":   run.Error,
		"candidates":      splitCandidatesOrEmpty(run.Candidates), // 归一为 []，不吐 null
		"model":           run.Model,
		"exec_env":        execEnv, // local | agent-server
		"agent_server_id": run.AgentServerID,
		"created_at":      created,
		"finished_at":     finished, // 0 = 还没结束
	})
}

// splitCandidatesOrEmpty normalizes a nil candidate slice into an empty one —
// the wire format must always carry an array, never null.
func splitCandidatesOrEmpty(c []prompt.SplitTask) []prompt.SplitTask {
	if c == nil {
		return []prompt.SplitTask{}
	}
	return c
}

// ── POST /api/requirements/{id}/split/confirm ─────────────────────────────

// ConfirmSplit persists the user-confirmed candidates as sub-requirements.
//
// Everything lands in ONE transaction (service.CreateChildren): the child
// rows, the dependency edges and the master's dev_delegated flag. A failure
// anywhere leaves the database exactly as it was.
//
// The optional "互相参考" links are written AFTER the children commit,
// because SetReferences validates that its targets exist. A failure there is
// reported as a warning but does not roll back the children — the children
// are the primary fact; a missing reference only costs the model a bit of
// context.
func (h *WizardHandler) ConfirmSplit(w http.ResponseWriter, r *http.Request) {
	reqID := r.PathValue("id")
	if h.reqSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "拆分服务未启用")
		return
	}

	var body ConfirmSplitReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	if len(body.Tasks) == 0 {
		writeError(w, http.StatusBadRequest, "SPLIT_NO_TASKS", "没有要创建的子需求")
		return
	}
	if len(body.Tasks) > splitMaxCandidates {
		body.Tasks = body.Tasks[:splitMaxCandidates]
	}

	parent, err := h.reqSvc.Get(reqID)
	if err != nil {
		writeError(w, http.StatusNotFound, "REQUIREMENT_NOT_FOUND", "需求不存在")
		return
	}
	if code, msg := splitPreconditionError(parent); code != "" {
		writeError(w, http.StatusBadRequest, code, msg)
		return
	}

	// Prepare the stack bottom: the master's own dev branch, created from
	// the user-picked source branch and pushed to origin. Every child will
	// develop on it (stacked PRs), so it must exist remotely before the
	// children are persisted.
	var proj *model.Project
	if h.projectSvc != nil {
		if p, pErr := h.projectSvc.Get(parent.ProjectID); pErr == nil {
			proj = p
		}
	}
	stackBase, stackPushed, stackWarnings, stackErr := h.prepareSplitStackBase(parent, proj, body.SourceBranch)
	if stackErr != nil {
		var se *splitStackError
		if errors.As(stackErr, &se) {
			writeError(w, http.StatusBadRequest, se.code, se.msg)
			return
		}
		writeError(w, http.StatusInternalServerError, "SPLIT_CREATE_FAILED", stackErr.Error())
		return
	}

	// Order the candidates and pre-mint their ids so the dependency edges
	// (which reference real ids) can be inserted in the same transaction as
	// the rows themselves.
	tasks := make([]model.SplitTaskInput, len(body.Tasks))
	copy(tasks, body.Tasks)
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].Order < tasks[j].Order })

	ids := make([]string, len(tasks))
	for i := range tasks {
		if tasks[i].Order <= 0 {
			tasks[i].Order = i + 1
		}
		ids[i] = util.NewID("req")
		tasks[i].ID = ids[i]
	}

	// depends_on holds batch-local ordinals (1-based) → translate them into
	// real ids here.
	edges := []model.DependencyEdge{}
	for i, task := range tasks {
		for _, dep := range task.DependsOn {
			// dep is an ordinal into the ordered task list.
			idx := dep - 1
			if idx < 0 || idx >= len(tasks) || idx == i {
				continue
			}
			edges = append(edges, model.DependencyEdge{
				RequirementID: ids[i],
				DependsOnID:   ids[idx],
			})
		}
	}

	children, err := h.reqSvc.CreateChildren(parent, tasks, edges, service.SplitCreateOpts{
		BaseBranch: stackBase,
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrDependencyCycle):
			writeError(w, http.StatusBadRequest, "SPLIT_DEPENDENCY_CYCLE", "前置依赖成环，无法创建子需求")
		case errors.Is(err, service.ErrParentIsChild):
			writeError(w, http.StatusBadRequest, "SPLIT_PARENT_IS_CHILD", "子需求不能再拆分（需求层级仅支持一层）")
		case errors.Is(err, service.ErrParentArchived):
			writeError(w, http.StatusBadRequest, "SPLIT_PARENT_ARCHIVED", "已归档的需求不能拆分")
		case errors.Is(err, service.ErrParentCrossProject):
			writeError(w, http.StatusBadRequest, "SPLIT_PARENT_CROSS_PROJECT", "主需求属于其它项目")
		case errors.Is(err, service.ErrParentNotFound):
			writeError(w, http.StatusNotFound, "SPLIT_PARENT_NOT_FOUND", "主需求不存在")
		default:
			writeError(w, http.StatusInternalServerError, "SPLIT_CREATE_FAILED", err.Error())
		}
		return
	}

	warnings := append([]string{}, stackWarnings...)
	if len(body.References) > 0 {
		if err := h.applySplitReferences(children, body.References); err != nil {
			log.Printf("[split] %s: reference linking failed: %v", reqID, err)
			warnings = append(warnings, "子需求已创建，但互相引用关系设置失败："+err.Error())
		}
	}

	// Shape the response: the frontend needs the ids (to link), the base
	// branch (to show "跟随主需求") and the prerequisite ids (to render the
	// dependency markers).
	type childOut struct {
		ID               string   `json:"id"`
		Title            string   `json:"title"`
		Status           string   `json:"status"`
		BaseBranch       string   `json:"base_branch"`
		BaseBranchSource string   `json:"base_branch_source"`
		SplitBatchID     string   `json:"split_batch_id"`
		DependsOn        []string `json:"depends_on"`
	}
	preByChild := map[string][]string{}
	for _, e := range edges {
		preByChild[e.RequirementID] = append(preByChild[e.RequirementID], e.DependsOnID)
	}
	out := make([]childOut, 0, len(children))
	for _, c := range children {
		deps := preByChild[c.ID]
		if deps == nil {
			deps = []string{}
		}
		out = append(out, childOut{
			ID:               c.ID,
			Title:            c.Title,
			Status:           c.Status,
			BaseBranch:       c.BaseBranch,
			BaseBranchSource: c.BaseBranchSource,
			SplitBatchID:     c.SplitBatchID,
			DependsOn:        deps,
		})
	}

	// The stack bottom the children actually inherited — the master's own
	// development branch (feat/req_<master>), or whatever the service layer
	// derived when no stack could be prepared.
	effectiveBase := ""
	if len(children) > 0 {
		effectiveBase = children[0].BaseBranch
	}

	// 候选已经落库，这一轮 run 就"用掉了" —— 标记为已消费，state 端点随即回
	// status:none。用户重开弹窗回到配置阶段，不会看到一份已经落过库的旧候选
	//（会误以为还能再创建一批）。
	//
	// 用 consumeLatest 而不是按 job_id 消费：ConfirmSplitReq 里没有 job_id，
	// 而同一需求同时只允许跑一次拆分（splitLocks 保证），且确认后 dev_delegated
	// 会让 splitPreconditionError 挡掉二次拆分 —— 所以"最近一次 run"就是被确认
	// 的那一轮。
	h.splitState().runs.consumeLatest(reqID)

	writeJSON(w, http.StatusOK, map[string]any{
		"children":    out,
		"warnings":    warnings,
		"base_branch": effectiveBase,
		"stack_base":  stackBase,
		"pushed":      stackPushed,
	})
}

// applySplitReferences writes the user-picked "互相参考" links.
//
// SetReferences REPLACES the requirement's direct reference set, so each
// child must be written exactly once with its full list of siblings —
// calling it repeatedly would overwrite the previous call's links.
func (h *WizardHandler) applySplitReferences(children []*model.Requirement, refs [][2]int) error {
	// ordinal (1-based, into the ordered children) → child row
	byOrdinal := map[int]*model.Requirement{}
	for i, c := range children {
		byOrdinal[i+1] = c
	}
	// child id → the siblings it should reference
	links := map[string][]model.RequirementReferenceInput{}
	for _, pair := range refs {
		from, okFrom := byOrdinal[pair[0]]
		to, okTo := byOrdinal[pair[1]]
		if !okFrom || !okTo || from.ID == to.ID {
			continue
		}
		links[from.ID] = append(links[from.ID], model.RequirementReferenceInput{
			RequirementID: to.ID,
			Note:          "同批次拆分，互相关联",
		})
	}
	for childID, inputs := range links {
		if _, err := h.reqSvc.SetReferences(childID, inputs); err != nil {
			return fmt.Errorf("set references for %s: %w", childID, err)
		}
	}
	return nil
}

// ── GET /api/requirements/{id}/children ───────────────────────────────────

// ListChildren returns the requirement's sub-requirements and the dependency
// edges among them.
func (h *WizardHandler) ListChildren(w http.ResponseWriter, r *http.Request) {
	reqID := r.PathValue("id")
	if h.reqSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "拆分服务未启用")
		return
	}
	children, err := h.reqSvc.ListChildren(reqID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	deps, err := h.listSplitDependencies(reqID, children)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"children":     children,
		"dependencies": deps,
	})
}

// listSplitDependencies collects every dependency edge touching this
// requirement's children (in either direction), so the UI can render both
// "依赖 ①" and "被 ② 依赖" from one payload.
func (h *WizardHandler) listSplitDependencies(reqID string, children []*model.Requirement) ([]model.RequirementDependency, error) {
	out := []model.RequirementDependency{}
	seen := map[string]struct{}{}
	for _, c := range children {
		pre, post, err := h.reqSvc.ListDependencies(c.ID)
		if err != nil {
			return nil, err
		}
		for _, d := range pre {
			key := d.RequirementID + ">" + d.DependsOnID
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, d)
		}
		for _, d := range post {
			key := d.RequirementID + ">" + d.DependsOnID
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, d)
		}
	}
	return out, nil
}
