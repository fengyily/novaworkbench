package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// 「跨项目需求」的 HTTP 层（REST 部分）。
//
// 跨项目需求是一个**独立实体**（cross_requirements），它的子需求是普通的
// requirements 行。所以这一层的职责边界很清楚：
//
//   - 本文件：实体 CRUD / 参与项目 / 阶段 / 拆分确认（写库）
//   - cross_requirement_jobs.go：三个 AI 作业（整体方案 / 拆分 / 阶段验证）
//     + 它们的 SSE 端点
//
// AI 作业不复用 wizard_architect.go —— 那个文件与 *model.Requirement 的状态机
// 深度耦合（design_docs / session / revision / 附件 / QA），硬套会引入大量
// 分支与回归风险。这里只复用稳定的底层件：llm.StreamOpts / runClaudeStream /
// store.JobStore / SSE helper。

// CrossRequirementHandler serves the cross-project requirement endpoints.
type CrossRequirementHandler struct {
	svc        *service.CrossRequirementService
	reqSvc     *service.RequirementService
	projectSvc projectService
	llm        *llm.Gateway
	jobs       *store.JobStore
	roleSvc    *service.RoleService
	claudeCfg  *service.ClaudeConfigService
	usageSvc   usageRecorder
	// worktreeH — 建跨项目需求的隔离 worktree（每个参与仓库一份，路径由
	// 跨项目需求 ID 决定）。注入而不是走包级单例：包级单例在生产环境是
	// nil（SetDefaultWorktreeHandler 从未被调用），走它会丢掉平台 token 的
	// 凭据注入 —— 私有 HTTPS 仓库的 fetch 会拿不到授权。
	worktreeH *WorktreeHandler
	// settingSvc — 读 git 同步超时等 KV 设置。
	settingSvc *service.SettingService
	// runs — 进程内的作业登记处（状态 / 产物 / 并发闸门）。
	runs *crossRequirementRuns
	// attachSvc — 跨项目需求的附件存储（owner 列 cross_requirement_id）。
	//
	// 与需求 / Review 的附件服务同型，只是换了表与 owner 列。它让跨项目作业
	// 的提示词能带上附件清单（attachManifestFor），并把描述里的
	// nova://attachment/<id> 标记重写成真实磁盘路径
	// （rewriteAttachmentMarkers）—— 否则模型看到的是一串打不开的协议串。
	//
	// 可能为 nil（未接线）：两个辅助函数都对 nil 做了处理，届时附件只是
	// 不会出现在提示词里，不影响作业本身。
	attachSvc *service.AttachmentService
}

// NewCrossRequirementHandler builds the handler.
func NewCrossRequirementHandler(
	svc *service.CrossRequirementService,
	reqSvc *service.RequirementService,
	projectSvc projectService,
	gateway *llm.Gateway,
	jobs *store.JobStore,
	roleSvc *service.RoleService,
	claudeCfg *service.ClaudeConfigService,
	usageSvc usageRecorder,
	worktreeH *WorktreeHandler,
	settingSvc *service.SettingService,
	attachSvc *service.AttachmentService,
) *CrossRequirementHandler {
	return &CrossRequirementHandler{
		svc:        svc,
		reqSvc:     reqSvc,
		projectSvc: projectSvc,
		llm:        gateway,
		jobs:       jobs,
		roleSvc:    roleSvc,
		claudeCfg:  claudeCfg,
		usageSvc:   usageSvc,
		worktreeH:  worktreeH,
		settingSvc: settingSvc,
		attachSvc:  attachSvc,
		runs:       newCrossRequirementRuns(crossRunsCap),
	}
}

// crossRunsCap — 作业登记处的容量上限。
const crossRunsCap = 32

// crossRunKind — 作业种类。
const (
	crossRunDesign = "design"
	crossRunSplit  = "split"
	crossRunVerify = "verify"
)

// crossRun — 一次跨项目需求 AI 作业的登记记录。
type crossRun struct {
	JobID              string
	CrossRequirementID string
	// Kind — design | split | verify。
	Kind string
	// PhaseID — 阶段验证作业对应的阶段 id（其余种类为空）。
	PhaseID string
	Status  store.JobStatus
	Error   string
	// Design — 整体方案 Markdown（design 作业的产物）。
	Design string
	// Candidates — 拆分候选（split 作业的产物）。
	Candidates []prompt.CrossSplitCandidate
	// Report — 阶段验证报告（verify 作业的产物）。
	Report string
	// Model — 这一轮实际生效的模型。
	Model string
	// Consumed — 产物已被用户确认落库。
	Consumed   bool
	CreatedAt  time.Time
	FinishedAt time.Time
}

// crossRequirementRuns — 进程内的跨项目需求作业登记处。
//
// 登记处的存在意义是**状态可发现**：整体方案 / 拆分 / 阶段验证的产物在确认
// 之前都是临时的（刷新页面即丢），但用户必须能看到「上一轮跑到哪了、产出了
// 什么」，而不是被迫重跑一次 AI。
type crossRequirementRuns struct {
	mu sync.RWMutex
	// byJob 按 jobID 索引。
	byJob map[string]*crossRun
	// latest 按 (kind, crossRequirementID) 索引最近一次作业的 job id。
	latest map[string]string
	// locks 并发闸门：(kind, crossRequirementID) → 正在跑的 job id。
	locks map[string]string
	order []string
	cap   int
}

func newCrossRequirementRuns(capacity int) *crossRequirementRuns {
	if capacity <= 0 {
		capacity = crossRunsCap
	}
	return &crossRequirementRuns{
		byJob:  map[string]*crossRun{},
		latest: map[string]string{},
		locks:  map[string]string{},
		cap:    capacity,
	}
}

// crossRunKey builds the registry key of a run.
func crossRunKey(kind, xreqID, phaseID string) string {
	return kind + "\x00" + xreqID + "\x00" + phaseID
}

// start registers a new run. It must be called AFTER the lock is acquired.
func (r *crossRequirementRuns) start(jobID, xreqID, kind, phaseID, modelName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run := &crossRun{
		JobID:              jobID,
		CrossRequirementID: xreqID,
		Kind:               kind,
		PhaseID:            phaseID,
		Status:             store.JobRunning,
		Model:              modelName,
		CreatedAt:          time.Now(),
	}
	r.byJob[jobID] = run
	r.latest[crossRunKey(kind, xreqID, phaseID)] = jobID
	r.order = append(r.order, jobID)
	r.evictOldestLocked()
}

// finish writes the terminal state of a run. Must be called BEFORE
// job.Finish (which closes the subscriber channels).
func (r *crossRequirementRuns) finish(jobID string, status store.JobStatus, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run := r.byJob[jobID]
	if run == nil {
		return
	}
	run.Status = status
	run.Error = errMsg
	run.FinishedAt = time.Now()
}

// setResult stores a run's product.
func (r *crossRequirementRuns) setResult(jobID string, design string, candidates []prompt.CrossSplitCandidate, report string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run := r.byJob[jobID]
	if run == nil {
		return
	}
	run.Design = design
	run.Report = report
	if candidates != nil {
		run.Candidates = make([]prompt.CrossSplitCandidate, len(candidates))
		copy(run.Candidates, candidates)
	}
}

// get returns a copy of a run.
func (r *crossRequirementRuns) get(jobID string) *crossRun {
	r.mu.RLock()
	defer r.mu.RUnlock()
	run := r.byJob[jobID]
	if run == nil {
		return nil
	}
	out := *run
	if run.Candidates != nil {
		out.Candidates = make([]prompt.CrossSplitCandidate, len(run.Candidates))
		copy(out.Candidates, run.Candidates)
	}
	return &out
}

// latestFor returns the most recent run of a kind for a cross requirement.
func (r *crossRequirementRuns) latestFor(kind, xreqID, phaseID string) *crossRun {
	r.mu.RLock()
	jobID := r.latest[crossRunKey(kind, xreqID, phaseID)]
	r.mu.RUnlock()
	if jobID == "" {
		return nil
	}
	return r.get(jobID)
}

// consumeLatest marks the most recent run as consumed (its product was
// persisted) and drops the product.
func (r *crossRequirementRuns) consumeLatest(kind, xreqID, phaseID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	jobID := r.latest[crossRunKey(kind, xreqID, phaseID)]
	if jobID == "" {
		return
	}
	if run := r.byJob[jobID]; run != nil {
		run.Consumed = true
		run.Candidates = nil
	}
}

// evictOldestLocked drops finished runs until the registry is under capacity.
func (r *crossRequirementRuns) evictOldestLocked() {
	for len(r.byJob) > r.cap {
		evicted := false
		for i, jobID := range r.order {
			run := r.byJob[jobID]
			if run == nil || run.Status == store.JobRunning {
				continue
			}
			delete(r.byJob, jobID)
			r.order = append(r.order[:i], r.order[i+1:]...)
			for key, latest := range r.latest {
				if latest == jobID {
					delete(r.latest, key)
				}
			}
			evicted = true
			break
		}
		if !evicted {
			return
		}
	}
}

// acquire takes the concurrency gate for a (kind, cross requirement) pair.
func (r *crossRequirementRuns) acquire(kind, xreqID, phaseID, jobID string) (existing string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := crossRunKey(kind, xreqID, phaseID)
	if cur, running := r.locks[key]; running {
		return cur, false
	}
	r.locks[key] = jobID
	return "", true
}

// release drops the concurrency gate.
func (r *crossRequirementRuns) release(kind, xreqID, phaseID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.locks, crossRunKey(kind, xreqID, phaseID))
}

// peek reports the running job id without taking the gate.
func (r *crossRequirementRuns) peek(kind, xreqID, phaseID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	jobID, running := r.locks[crossRunKey(kind, xreqID, phaseID)]
	return jobID, running
}

// ── REST: 实体 CRUD ────────────────────────────────────────────────────────

// List returns the cross requirements with their live progress.
func (h *CrossRequirementHandler) List(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	keyword := strings.TrimSpace(r.URL.Query().Get("keyword"))

	// 生命周期状态是**派生**的：子需求在别处完成之后，落库的状态可能还是旧的
	// （状态只在写路径上刷新）。所以列表页在读路径上把状态刷新一遍，并按
	// **刷新后的**状态做筛选 —— 否则筛选条件与页面上显示的状态会互相矛盾。
	//
	// 「已归档」是用户显式状态，派生逻辑不会改动它，所以按状态筛选时归档行
	// 单独查。
	queryStatus := ""
	if status == model.XReqStatusArchived {
		queryStatus = model.XReqStatusArchived
	}
	items, err := h.svc.List(queryStatus, keyword)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}

	progress := map[string]model.CrossRequirementProgress{}
	out := make([]model.CrossRequirement, 0, len(items))

	// 进展 / 阶段**一次性**聚合：整页只跑固定的几条语句，与行数、子需求个数
	// 无关。原来是对每条需求各跑一次聚合，而聚合内部又为每个子需求单跑一次
	// 前置依赖查询 —— 20 条需求 × 10 个子需求 ≈ 200+ 条语句，每刷新一次页面
	// 就重来一遍。
	ids := make([]string, 0, len(items))
	for i := range items {
		ids = append(ids, items[i].ID)
	}
	agg, aggErr := h.svc.LoadAggregate(ids)
	if aggErr != nil {
		log.Printf("[xreq] load aggregate: %v", aggErr)
	}

	for i := range items {
		item := &items[i]
		if aggErr == nil {
			// 刷新派生状态（幂等：状态没变就不写库），并把生效的状态回填到
			// 响应里 —— 否则列表页显示的是子需求在别处完成前的旧状态。
			//
			// 阶段随聚合一起取回，所以这里不必再让服务层单独查一次。
			if _, sErr := h.svc.ApplyDerivedStatus(item, agg.Progress[item.ID], agg.Phases[item.ID]); sErr != nil {
				log.Printf("[xreq] refresh derived status %s: %v", item.ID, sErr)
			}
		}
		if status != "" && status != "all" && item.Status != status {
			continue
		}
		if aggErr == nil {
			progress[item.ID] = agg.Progress[item.ID]
		}
		out = append(out, *item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    out,
		"progress": progress,
	})
}

// Create creates a cross requirement together with its participating
// projects.
func (h *CrossRequirementHandler) Create(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	var body model.CreateCrossRequirementReq
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	created, err := h.svc.Create(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "CREATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// Get returns the full detail of a cross requirement.
func (h *CrossRequirementHandler) Get(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	id := r.PathValue("id")
	// The lifecycle status is refreshed on the read path (idempotent — only
	// writes when it actually changed), so the detail page never shows a
	// stale status after a child requirement was completed elsewhere.
	//
	// 刷新结果会回填进载荷（DetailFresh），所以响应里的 status 一定是刷新
	// **之后**的值 —— 先装配再刷新的话，响应里带的永远是刷新前的旧值。
	detail, err := h.svc.DetailFresh(id)
	if err != nil {
		if errors.Is(err, service.ErrCrossRequirementNotFound) {
			writeError(w, http.StatusNotFound, "CROSS_REQUIREMENT_NOT_FOUND", "跨项目需求不存在")
			return
		}
		writeError(w, http.StatusInternalServerError, "GET_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// Update writes the editable fields of a cross requirement.
func (h *CrossRequirementHandler) Update(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	id := r.PathValue("id")
	var body model.UpdateCrossRequirementReq
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	// 状态先校验：非法状态必须在写其它字段**之前**被拒，否则会留下写了一半
	// 的更新（字段改了、状态没改）。
	status := strings.TrimSpace(body.Status)
	if status != "" {
		if err := service.ValidateCrossRequirementStatus(status); err != nil {
			writeError(w, http.StatusBadRequest, "UPDATE_FAILED", err.Error())
			return
		}
	}
	if err := h.svc.Update(id, body); err != nil {
		writeCrossErr(w, err, http.StatusBadRequest, "UPDATE_FAILED", "更新失败：")
		return
	}
	if status != "" {
		// 人工设置状态 → 锁定它，读路径的派生刷新不再覆盖（见 SetStatus）。
		if err := h.svc.SetStatus(id, status); err != nil {
			writeError(w, http.StatusBadRequest, "UPDATE_FAILED", err.Error())
			return
		}
	}
	detail, err := h.svc.DetailFresh(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "GET_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// Delete moves a cross requirement to the trash.
//
// 软删，不是抹掉：跨项目需求被拆过之后，整体方案 / 联调阶段 / 跨项目依赖是
// **不可再生产**的东西 —— 子需求里的代码还在，但「三个仓库怎么对齐」的上下文
// 丢了就是丢了。所以删除先落到回收站，用户确认不要了再彻底删除。
//
// 子需求完全不受影响：它们继续在各自的项目里活着，开发不中断。
func (h *CrossRequirementHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	id := r.PathValue("id")
	if err := h.svc.MoveToTrash(id); err != nil {
		switch {
		case errors.Is(err, service.ErrCrossRequirementNotFound):
			writeError(w, http.StatusNotFound, "CROSS_REQUIREMENT_NOT_FOUND", "跨项目需求不存在")
		case errors.Is(err, service.ErrCrossRequirementInTrash):
			writeError(w, http.StatusConflict, "XREQ_IN_TRASH", "该跨项目需求已经在回收站里了")
		default:
			writeError(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"trashed": true})
}

// Purge permanently removes a cross requirement that sits in the trash.
//
// 只有回收站里的行能被彻底删除（NOT_IN_TRASH 闸门）—— 误点在「进行中」的行
// 上点彻底删除时，应该被挡下来并引导用户先走软删。
//
// 子需求**脱离**而不是被删：它们背后可能已经产出了代码、分支和 PR，级联删掉
// 会造成不可恢复的损失。
func (h *CrossRequirementHandler) Purge(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	id := r.PathValue("id")
	// Read the baseline ledger BEFORE the purge: it is the only record of
	// which worktrees this cross requirement created.
	bases, _ := h.svc.ListBases(id)
	if err := h.svc.Purge(id); err != nil {
		switch {
		case errors.Is(err, service.ErrCrossRequirementNotFound):
			writeError(w, http.StatusNotFound, "CROSS_REQUIREMENT_NOT_FOUND", "跨项目需求不存在")
		case errors.Is(err, service.ErrCrossRequirementNotInTrash):
			writeError(w, http.StatusConflict, "NOT_IN_TRASH", "该跨项目需求不在回收站里，无法彻底删除")
		default:
			writeError(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		}
		return
	}
	// The rows are gone; the filesystem artifacts they pointed at are not.
	// Remove the per-repository worktrees and the job working directory so a
	// purged cross requirement does not leave detached worktrees registered
	// in the user's repositories.
	h.cleanupCrossWorktrees(id, bases)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// Restore brings a cross requirement back from the trash.
//
// 恢复后返回详情载荷（含按当前事实重算过的状态）—— 读路径的派生逻辑在这里
// 就有机会跑一次，前端不必再拉一次详情。
func (h *CrossRequirementHandler) Restore(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	id := r.PathValue("id")
	if err := h.svc.Restore(id); err != nil {
		switch {
		case errors.Is(err, service.ErrCrossRequirementNotFound):
			writeError(w, http.StatusNotFound, "CROSS_REQUIREMENT_NOT_FOUND", "跨项目需求不存在")
		case errors.Is(err, service.ErrCrossRequirementNotInTrash):
			writeError(w, http.StatusConflict, "NOT_IN_TRASH", "该跨项目需求不在回收站里，无需恢复")
		default:
			writeError(w, http.StatusInternalServerError, "RESTORE_FAILED", err.Error())
		}
		return
	}
	detail, err := h.svc.DetailFresh(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "GET_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// Trash returns the cross requirements sitting in the trash, most recently
// deleted first.
//
// 回收站里的行**不刷新派生状态**（状态冻结在「被移走的那一刻」），但子需求
// 的进展照常聚合 —— 用户判断「这条还有没有用」靠的正是子需求做到哪一步了。
func (h *CrossRequirementHandler) Trash(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	items, err := h.svc.ListTrash()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	progress := map[string]model.CrossRequirementProgress{}
	out := make([]model.CrossRequirement, 0, len(items))
	ids := make([]string, 0, len(items))
	for i := range items {
		ids = append(ids, items[i].ID)
	}
	if agg, aggErr := h.svc.LoadAggregate(ids); aggErr == nil {
		for i := range items {
			progress[items[i].ID] = agg.Progress[items[i].ID]
		}
	} else {
		log.Printf("[xreq] load trash aggregate: %v", aggErr)
	}
	out = append(out, items...)
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    out,
		"progress": progress,
	})
}

// rejectIfTrashed refuses a write against a cross requirement that sits in
// the trash.
//
// 回收站里的行是**只读**的：整体方案 / 拆分 / 阶段 / 验证 / 改参与项目一律
// 拒绝。否则用户会在垃圾堆里干活，而作业跑完会把结果写回一条已经被「删掉」
// 的行。
func rejectIfTrashed(w http.ResponseWriter, xreq *model.CrossRequirement) bool {
	if xreq == nil || xreq.DeletedAt == "" {
		return false
	}
	writeError(w, http.StatusConflict, "XREQ_IN_TRASH", "该跨项目需求在回收站中，请先恢复再操作")
	return true
}

// writeCrossErr maps a service-layer error onto the HTTP status the
// cross-requirement endpoints use.
//
// 「在回收站里」是 409 而不是 400：请求本身合法，只是这条实体此刻不可写 ——
// 前端要靠这个码把按钮置灰 / 提示「请先恢复」，而不是弹一个看不懂的报错。
func writeCrossErr(w http.ResponseWriter, err error, genericStatus int, genericCode, genericMsg string) {
	switch {
	case errors.Is(err, service.ErrCrossRequirementNotFound):
		writeError(w, http.StatusNotFound, "CROSS_REQUIREMENT_NOT_FOUND", "跨项目需求不存在")
	case errors.Is(err, service.ErrCrossRequirementInTrash):
		writeError(w, http.StatusConflict, "XREQ_IN_TRASH", "该跨项目需求在回收站中，请先恢复再操作")
	case errors.Is(err, service.ErrCrossRequirementNotInTrash):
		writeError(w, http.StatusConflict, "NOT_IN_TRASH", "该跨项目需求不在回收站里")
	default:
		writeError(w, genericStatus, genericCode, genericMsg+err.Error())
	}
}

// SetProjects replaces the participating project set.
func (h *CrossRequirementHandler) SetProjects(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	id := r.PathValue("id")
	var body model.SetCrossRequirementProjectsReq
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	if err := h.svc.SetProjects(id, body.Projects); err != nil {
		if errors.Is(err, service.ErrDuplicateProject) {
			writeError(w, http.StatusBadRequest, "DUPLICATE_PROJECT", err.Error())
			return
		}
		writeCrossErr(w, err, http.StatusBadRequest, "SET_PROJECTS_FAILED", "设置参与项目失败：")
		return
	}
	projects, err := h.svc.ListProjects(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

// SaveDesign stores a hand-edited overall design document.
//
// The mandatory 「跨项目接口契约」 section is validated here: a design without
// it would let every repository invent its own interface, and the mismatch
// only shows up at integration time (the most expensive moment).
func (h *CrossRequirementHandler) SaveDesign(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	id := r.PathValue("id")
	var body model.SaveCrossRequirementDesignReq
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	markdown := strings.TrimSpace(body.DesignDocs)
	if markdown == "" {
		writeError(w, http.StatusBadRequest, "DESIGN_EMPTY", "整体方案不能为空")
		return
	}
	if !prompt.HasCrossContractSection(markdown) {
		writeErrorSuggestion(w, http.StatusBadRequest, "DESIGN_MISSING_CONTRACT",
			"整体方案缺少「跨项目接口契约」章节",
			"补上该章节后再保存，或重新生成整体方案")
		return
	}
	if err := h.svc.SaveDesign(id, markdown, "", ""); err != nil {
		writeCrossErr(w, err, http.StatusInternalServerError, "SAVE_FAILED", "保存失败：")
		return
	}
	if err := h.svc.RefreshDerivedStatus(id); err != nil {
		_ = err
	}
	detail, err := h.svc.Detail(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "GET_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// ── REST: 拆分确认 ─────────────────────────────────────────────────────────

// ConfirmSplit persists the user-confirmed candidates as sub-requirements.
//
// 项目归属由服务端校验（候选的 project_id 必须 ∈ 参与集合）—— AI 的建议值
// 不能直接落库。非法时整批拒绝（事务回滚），不产生任何残留行。
//
// 拆分**只能做一次**：已经拆过的跨项目需求再确认一次 → 409
// XREQ_ALREADY_SPLIT（服务端原子抢占，见 service.ConfirmSplit）。这条闸门
// 是「双击确认按钮」的最后一道防线 —— 前端的 submitting 态只是让用户不必
// 看见这个错误。
func (h *CrossRequirementHandler) ConfirmSplit(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	id := r.PathValue("id")
	var body model.ConfirmCrossRequirementSplitReq
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	if len(body.Candidates) == 0 {
		writeError(w, http.StatusBadRequest, "SPLIT_NO_TASKS", "没有要创建的子需求")
		return
	}
	children, err := h.svc.ConfirmSplit(id, body.Candidates)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrProjectNotInScope):
			writeError(w, http.StatusBadRequest, "SPLIT_PROJECT_OUT_OF_SCOPE", err.Error())
		case errors.Is(err, service.ErrDependencyCycle):
			writeError(w, http.StatusBadRequest, "SPLIT_DEPENDENCY_CYCLE", "前置依赖成环，无法创建子需求")
		case errors.Is(err, service.ErrCrossAlreadySplit):
			writeError(w, http.StatusConflict, "XREQ_ALREADY_SPLIT", "该跨项目需求已经拆分过了，不能重复拆分")
		default:
			writeCrossErr(w, err, http.StatusInternalServerError, "SPLIT_CREATE_FAILED", "创建子需求失败：")
		}
		return
	}
	// The candidates are persisted — this run is "used up".
	h.runs.consumeLatest(crossRunSplit, id, "")

	detail, err := h.svc.Detail(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "GET_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"children": children,
		"detail":   detail,
	})
}

// ── REST: 阶段 ─────────────────────────────────────────────────────────────

// CreatePhase adds a phase.
func (h *CrossRequirementHandler) CreatePhase(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	id := r.PathValue("id")
	var body model.CrossRequirementPhaseReq
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	phase, err := h.svc.CreatePhase(id, body)
	if err != nil {
		writeCrossErr(w, err, http.StatusBadRequest, "CREATE_PHASE_FAILED", "创建阶段失败：")
		return
	}
	writeJSON(w, http.StatusCreated, phase)
}

// UpdatePhase updates a phase (name / description / members / verdict).
func (h *CrossRequirementHandler) UpdatePhase(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	id := r.PathValue("id")
	phaseID := r.PathValue("pid")
	var body model.UpdateCrossRequirementPhaseReq
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	phase, err := h.svc.UpdatePhase(id, phaseID, body)
	if err != nil {
		writeCrossErr(w, err, http.StatusBadRequest, "UPDATE_PHASE_FAILED", "更新阶段失败：")
		return
	}
	writeJSON(w, http.StatusOK, phase)
}

// DeletePhase removes a phase.
func (h *CrossRequirementHandler) DeletePhase(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	id := r.PathValue("id")
	phaseID := r.PathValue("pid")
	if err := h.svc.DeletePhase(id, phaseID); err != nil {
		writeCrossErr(w, err, http.StatusBadRequest, "DELETE_PHASE_FAILED", "删除阶段失败：")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// ── 工具 ───────────────────────────────────────────────────────────────────

// crossProjectInputs assembles the participating projects for a prompt.
//
// The path handed to the model is the **workspace read path** — the detached
// worktree pinned to the locked baseline commit, not the shared checkout.
// The baseline SHA rides along so the model can tell exactly which tree it is
// reasoning about.
//
// A project whose checkout is missing (deleted / not cloned yet) still appears
// in the list — the model must know it exists — but it contributes no path.
func (h *CrossRequirementHandler) crossProjectInputs(xreqID string, ws *crossWorkspace) ([]prompt.CrossProjectInput, error) {
	projects, err := h.svc.ListProjects(xreqID)
	if err != nil {
		return nil, err
	}
	byProject := map[string]crossWorkspaceProject{}
	if ws != nil {
		for _, p := range ws.Projects {
			byProject[p.ProjectID] = p
		}
	}
	out := make([]prompt.CrossProjectInput, 0, len(projects))
	for _, p := range projects {
		item := prompt.CrossProjectInput{
			ID:         p.ProjectID,
			Name:       p.ProjectName,
			Role:       p.Role,
			BaseBranch: strings.TrimSpace(p.BaseBranch),
		}
		var localPath string
		if h.projectSvc != nil {
			if proj, pErr := h.projectSvc.Get(p.ProjectID); pErr == nil && proj != nil {
				item.Summary = strings.TrimSpace(proj.Description)
				localPath = strings.TrimSpace(proj.LocalPath)
			}
		}
		if wp, ok := byProject[p.ProjectID]; ok {
			item.Path = wp.ReadPath()
			item.BaseSHA = wp.BaseSHA
			if item.BaseBranch == "" {
				item.BaseBranch = wp.BaseBranch
			}
		} else {
			// No workspace entry (the job did not build one) — fall back to
			// the shared checkout so the model still has something to read.
			item.Path = localPath
		}
		out = append(out, item)
	}
	return out, nil
}

// crossAcceptance decodes the JSON acceptance criteria of a cross requirement.
func crossAcceptance(raw string) []string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil
	}
	return out
}
