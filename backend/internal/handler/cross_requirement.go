package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
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
	settingSvc *service.SettingService
	// runs — 进程内的作业登记处（状态 / 产物 / 并发闸门）。
	runs *crossRequirementRuns
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
	settingSvc *service.SettingService,
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
		settingSvc: settingSvc,
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
	items, err := h.svc.List(status, keyword)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	progress := map[string]model.CrossRequirementProgress{}
	for _, item := range items {
		p, pErr := h.svc.Progress(item.ID)
		if pErr != nil {
			continue
		}
		progress[item.ID] = p
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    items,
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
	detail, err := h.svc.Detail(id)
	if err != nil {
		if errors.Is(err, service.ErrCrossRequirementNotFound) {
			writeError(w, http.StatusNotFound, "CROSS_REQUIREMENT_NOT_FOUND", "跨项目需求不存在")
			return
		}
		writeError(w, http.StatusInternalServerError, "GET_FAILED", err.Error())
		return
	}
	// The lifecycle status is refreshed on the read path (idempotent — only
	// writes when it actually changed), so the detail page never shows a
	// stale status after a child requirement was completed elsewhere.
	if err := h.svc.RefreshDerivedStatus(id); err != nil {
		// A failed refresh must not break the read.
		_ = err
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
	if err := h.svc.Update(id, body); err != nil {
		if errors.Is(err, service.ErrCrossRequirementNotFound) {
			writeError(w, http.StatusNotFound, "CROSS_REQUIREMENT_NOT_FOUND", "跨项目需求不存在")
			return
		}
		writeError(w, http.StatusBadRequest, "UPDATE_FAILED", err.Error())
		return
	}
	if status := strings.TrimSpace(body.Status); status != "" {
		if err := h.svc.SetStatus(id, status); err != nil {
			writeError(w, http.StatusBadRequest, "UPDATE_FAILED", err.Error())
			return
		}
	}
	detail, err := h.svc.Detail(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "GET_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// Delete removes a cross requirement. Its children are **detached**, not
// deleted — the code / branches / PRs behind them must survive.
func (h *CrossRequirementHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	id := r.PathValue("id")
	if err := h.svc.Delete(id); err != nil {
		if errors.Is(err, service.ErrCrossRequirementNotFound) {
			writeError(w, http.StatusNotFound, "CROSS_REQUIREMENT_NOT_FOUND", "跨项目需求不存在")
			return
		}
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
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
		writeError(w, http.StatusBadRequest, "SET_PROJECTS_FAILED", err.Error())
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
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", err.Error())
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
		case errors.Is(err, service.ErrCrossRequirementNotFound):
			writeError(w, http.StatusNotFound, "CROSS_REQUIREMENT_NOT_FOUND", "跨项目需求不存在")
		default:
			writeError(w, http.StatusInternalServerError, "SPLIT_CREATE_FAILED", err.Error())
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
		writeError(w, http.StatusBadRequest, "CREATE_PHASE_FAILED", err.Error())
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
		writeError(w, http.StatusBadRequest, "UPDATE_PHASE_FAILED", err.Error())
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
		writeError(w, http.StatusBadRequest, "DELETE_PHASE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// ── 工具 ───────────────────────────────────────────────────────────────────

// crossWorkDir creates the scratch working directory for a cross-requirement
// job.
//
// 跨项目需求不属于任何单一项目，所以它没有 worktree 可用 —— 用一个临时目录做
// 工作区，各参与项目通过 --add-dir 授权只读访问（D5）。临时目录保证了对 N 个
// 仓库的对称性：用某个项目的 worktree 会让模型误以为那是「主仓库」。
func crossWorkDir(prefix string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "nova-xreq-"+prefix+"-")
	if err != nil {
		return "", func() {}, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// crossProjectInputs assembles the participating projects for a prompt.
//
// A project whose checkout is missing (deleted / not cloned yet) still appears
// in the list — the model must know it exists — but it contributes no path.
func (h *CrossRequirementHandler) crossProjectInputs(xreqID string) ([]prompt.CrossProjectInput, error) {
	projects, err := h.svc.ListProjects(xreqID)
	if err != nil {
		return nil, err
	}
	out := make([]prompt.CrossProjectInput, 0, len(projects))
	for _, p := range projects {
		item := prompt.CrossProjectInput{ID: p.ProjectID, Name: p.ProjectName, Role: p.Role}
		if h.projectSvc != nil {
			if proj, pErr := h.projectSvc.Get(p.ProjectID); pErr == nil && proj != nil {
				item.Path = strings.TrimSpace(proj.LocalPath)
				item.Summary = strings.TrimSpace(proj.Description)
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// crossProjectDirs returns the local paths of the participating projects —
// the directories handed to the CLI via --add-dir.
func (h *CrossRequirementHandler) crossProjectDirs(xreqID string) []string {
	projects, err := h.svc.ListProjects(xreqID)
	if err != nil {
		return nil
	}
	dirs := []string{}
	for _, p := range projects {
		if h.projectSvc == nil {
			continue
		}
		proj, pErr := h.projectSvc.Get(p.ProjectID)
		if pErr != nil || proj == nil {
			continue
		}
		path := strings.TrimSpace(proj.LocalPath)
		if path == "" {
			continue
		}
		if st, sErr := os.Stat(path); sErr != nil || !st.IsDir() {
			continue
		}
		dirs = append(dirs, path)
	}
	return dirs
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
