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
//
// run 携带这一轮的执行期素材（附件目录 / --add-dir 授权 / 上下文块），让注入
// 的假实现也能断言「清单与授权是不是一起抵达了执行层」。
type splitExecFunc func(job *store.Job, reqRow *model.Requirement, target splitExecTarget,
	promptText, systemPrompt, modelName, configID string, run splitRuntime, usage *usageCtx) ([]prompt.SplitTask, string)

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

// splitJobResults — 拆分结果在进程内的暂存。
//
// 结果是「内存态」：刷新页面即丢（方案的 R15 接受这一点 —— 重跑一次比维护
// 第二套状态机便宜）。进程重启后 map 为空 → SSE 的 done 帧带 result=null →
// 前端提示「拆分结果已失效，请重新生成」。
type splitJobResults struct {
	mu   sync.RWMutex
	byID map[string][]prompt.SplitTask
}

func newSplitJobResults() *splitJobResults {
	return &splitJobResults{byID: map[string][]prompt.SplitTask{}}
}

func (r *splitJobResults) set(jobID string, tasks []prompt.SplitTask) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[jobID] = tasks
}

func (r *splitJobResults) get(jobID string) []prompt.SplitTask {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tasks, ok := r.byID[jobID]
	if !ok {
		return nil
	}
	out := make([]prompt.SplitTask, len(tasks))
	copy(out, tasks)
	return out
}

// splitLocks — 幂等锁：requirementID → 正在跑的 job id。
//
// 重复点击 / 多标签页并发拆分会产生两批重复子需求，所以同一需求的拆分在任一
// 时刻只允许一个。锁在 job 结束时释放。
type splitLocks struct {
	mu    sync.Mutex
	byReq map[string]string
}

func newSplitLocks() *splitLocks {
	return &splitLocks{byReq: map[string]string{}}
}

// acquire returns the existing job id when a split is already running for
// this requirement, and "" when the caller may proceed.
func (l *splitLocks) acquire(reqID, jobID string) (existing string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byReq == nil {
		l.byReq = map[string]string{}
	}
	if cur, running := l.byReq[reqID]; running {
		return cur, false
	}
	l.byReq[reqID] = jobID
	return "", true
}

func (l *splitLocks) release(reqID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byReq, reqID)
}

// peek reports the running job id for a requirement without taking the
// lock. It lets the handler bail out BEFORE creating a job, so a refused
// second split doesn't leave a stray job in the ring buffer.
func (l *splitLocks) peek(reqID string) (jobID string, running bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	jobID, running = l.byReq[reqID]
	return jobID, running
}

// splitState holds the per-handler mutable state of the split feature.
type splitState struct {
	results *splitJobResults
	locks   *splitLocks
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
		h.split = &splitState{results: newSplitJobResults(), locks: newSplitLocks()}
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
	if existing, ok := st.locks.acquire(reqID, job.ID); !ok {
		job.Finish(1, store.JobError)
		writeError(w, http.StatusConflict, "SPLIT_IN_PROGRESS",
			fmt.Sprintf("已有拆分任务在运行（job %s），请等待结束", existing))
		return
	}

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

	// The requirement's own material: attachments, granted directories and
	// referenced requirements — plus the --add-dir grants that make the
	// attachment / directory manifests actually readable. Assembled in ONE
	// place so the manifest and the grant can never drift apart.
	//
	// On a remote run this also syncs the attachments to the agent host, so
	// it happens BEFORE the "analyzing" phase line — the sync can take a
	// while and the user must not stare at an empty panel.
	if strings.TrimSpace(body.AgentServerID) != "" {
		job.Append(store.LogLine{Type: "phase", Content: "📤 同步附件到 Agent 服务器…"})
	}
	run := h.buildSplitContext(context.Background(), reqRow.ID, strings.TrimSpace(body.AgentServerID))

	promptText := prompt.BuildSplitPrompt(reqRow, reqRow.DesignDocs, existing, run.ContextBlock)
	usage := h.usageCtxForConfig("requirement_split", reqRow.ID, reqRow.ProjectID, job.ID, modelName, "", "", configID)

	job.Append(store.LogLine{Type: "phase", Content: "🧩 正在分析方案并拆分任务…"})

	tasks, errMsg := h.execSplitTasks(job, reqRow, body, promptText, systemPrompt, modelName, configID, run, usage)
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

	st.results.set(job.ID, tasks)
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
	promptText, systemPrompt, modelName, configID string, run splitRuntime, usage *usageCtx) ([]prompt.SplitTask, string) {
	target := splitExecTarget{AgentServerID: strings.TrimSpace(body.AgentServerID)}
	target.Remote = target.AgentServerID != ""
	if h.splitExec != nil {
		return h.splitExec(job, reqRow, target, promptText, systemPrompt, modelName, configID, run, usage)
	}
	if target.Remote {
		return h.runRemoteSplitTasks(job, reqRow, target.AgentServerID, promptText, systemPrompt, modelName, configID, run, usage)
	}
	return h.runLocalSplitTasks(job, reqRow, promptText, systemPrompt, modelName, configID, run, usage)
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
	promptText, systemPrompt, modelName, configID string, run splitRuntime, usage *usageCtx) ([]prompt.SplitTask, string) {
	workDir := h.splitWorkDir(reqRow)

	cmd := h.llm.StreamCmd(context.Background(), llm.StreamOpts{
		Prompt:         promptText,
		WorkDir:        workDir,
		SystemPrompt:   systemPrompt,
		Model:          cliModelArg(modelName),
		ClaudeConfigID: configID,
		// The directories the manifest points at must be readable, otherwise
		// the model sees the file names but not their content — and a
		// non-interactive run has nobody to approve the read.
		AddDirs: run.AddDirs,
		// Read-only: the split must not touch the repository. Read / Glob /
		// Grep stay available so the model can ground the scope in real file
		// paths.
		DisallowedTools: []string{"Write", "Edit", "Bash", "NotebookEdit"},
	})

	out := runClaudeStream(jobSink{job}, cmd, "requirement-split", usage, nil, nil, 0, withStallTimeout(splitSplitStallTimeout))

	jsonPart, _ := extractTrailingJSON(out.finalResult)
	if strings.TrimSpace(jsonPart) == "" {
		jsonPart = out.planContent
	}
	tasks := prompt.NormalizeSplitTasks(jsonPart)

	if len(tasks) == 0 {
		// CLI path produced nothing usable — fall back to the direct HTTP
		// channel before giving up. The context block travels with it, so the
		// fallback does not lose the requirement's material.
		job.Append(store.LogLine{Type: "phase", Content: "⚠️ 本地 CLI 未产出可解析的拆分结果，改用直接 LLM 通道重试…"})
		raw, ferr := h.llm.ExtractSubRequirementsFromDesign(reqRow.DesignDocs, run.ContextBlock, out.errMsg)
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
func (h *WizardHandler) finishSplitError(job *store.Job, st *splitState, msg string) {
	st.results.set(job.ID, nil)
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
		if tasks := st.results.get(jobID); len(tasks) > 0 {
			result = tasks
		}
		doneData, _ := json.Marshal(map[string]interface{}{
			"type":        "job_done",
			"status":      string(status),
			"exit_code":   exitCode,
			"error_kind":  job.ErrorKind,
			"started_at":  job.StartedAt.UnixMilli(),
			"finished_at": job.FinishedAt.UnixMilli(),
			"duration_ms": job.FinishedAt.Sub(job.StartedAt).Milliseconds(),
			"result":      result,
		})
		return doneData
	})
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

	// The children inherit the master's granted directories, so the access
	// the user authorized for the master keeps working once development is
	// delegated. Soft failure: the children are the primary fact, a missing
	// directory grant only costs them read access.
	if err := h.inheritSplitDirs(reqID, children); err != nil {
		log.Printf("[split] %s: directory inheritance failed: %v", reqID, err)
		warnings = append(warnings, "子需求已创建，但继承主需求的授权目录失败："+err.Error())
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
