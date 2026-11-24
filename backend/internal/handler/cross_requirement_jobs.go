package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// 「跨项目需求」的 AI 作业层：整体方案 / 按项目拆分 / 阶段验证。
//
// 三个作业共享同一套骨架：
//
//	启动 → 建 job → 抢并发闸门 → 登记 run → 返回 job_id
//	（后台）→ 备 prompt → claude CLI（只读）→ 解析 → 落库 → 结束 job
//
// 与 wizard_architect.go 的关系：**不复用**。那个文件与 *model.Requirement 的
// 状态机深度耦合（状态迁移 / design_docs / session / revision / 附件 / QA），
// 硬套进来会引入大量分支与回归风险。这里只复用稳定的底层件：llm.StreamOpts /
// runClaudeStream / store.JobStore / SSE helper。
//
// 运行形态：**仅本地**（D9）。远端执行要求 Agent Server 上已经 clone 了全部
// 参与仓库并同步到同一版本，MVP 不引入这个复杂度。

// crossJobStallTimeout — 跨项目作业的静默看门狗时限。
//
// 整体方案要读 N 个仓库，阶段验证要读更多，比纯文本转换慢得多，但不该无限等。
const crossJobStallTimeout = 10 * time.Minute

// crossJobReq — 启动跨项目作业的请求体。
type crossJobReq struct {
	// Model — 用户在 UI 上选的模型（空 = 用 architect persona 的默认模型）。
	Model string `json:"model"`
	// ClaudeConfigID — 用户选的 Claude 配置（空 = 当前激活配置）。
	ClaudeConfigID string `json:"claude_config_id"`
	// Effort — 用户选的推理档位（空 = 「方案设计」阶段的默认档位）。
	Effort string `json:"effort"`
}

// crossJobSpec — 一个跨项目作业的 claude 调用参数。
type crossJobSpec struct {
	// Prompt — 已经组装好的提示词（**含各参与仓库的项目约定**）。
	Prompt string
	// WorkDir — 作业工作目录（~/.novaworkbench/xreq/<xreqID>/<kind>）。
	WorkDir string
	// SystemPrompt — persona（architect）。
	SystemPrompt string
	// Model — 生效的模型（空 = CLI 默认）。
	Model string
	// Effort — 推理档位。
	Effort string
	// ClaudeConfigID — 生效的 claude 配置（空 = 当前激活配置）。
	ClaudeConfigID string
	// Workspace — 各参与仓库的隔离 worktree（读取路径 + 锁定的基线 SHA）。
	// 作业从这里读代码，而不是从共享检出。
	Workspace *crossWorkspace
	// ExtraDirs — 额外授权给 CLI 的目录（阶段验证用它挂上子需求的实现
	// worktree）。与 Workspace 的目录合并后去重。
	ExtraDirs []string
}

// crossStreamOpts 组装一个跨项目作业的 claude 调用参数。
//
// 三个作业（整体方案 / 按项目拆分 / 阶段验证）的调用参数只差提示词与工作目录，
// 所以它们共用这一个构造器 —— 约定（参与仓库的项目级 CLAUDE.md）必须**在**
// 提示词里，而提示词是在**调用本函数之前**组装好的。
//
// 为什么单独抽出来：llm.StreamOpts.Prompt 是**值拷贝**。提示词组装好之后
// 再去改那个字符串（比如事后追加一段约定）对已经构造好的 opts 没有任何影响 ——
// 约定就那样静默地留在了本地变量里，模型永远看不到。参数集中在这里之后，
// 「提示词在构造 opts 之前就已经完整」这件事由类型和调用顺序共同保证，
// 并且可以被一个测试直接断言（见 handler/cross_requirement_test.go）。
func (h *CrossRequirementHandler) crossStreamOpts(spec crossJobSpec) llm.StreamOpts {
	return llm.StreamOpts{
		Prompt:         spec.Prompt,
		WorkDir:        spec.WorkDir,
		SystemPrompt:   spec.SystemPrompt,
		Model:          cliModelArg(spec.Model),
		Effort:         spec.Effort,
		ClaudeConfigID: spec.ClaudeConfigID,
		// The participating repositories must be readable, otherwise the
		// model cannot ground the contract in real file paths. The paths are
		// the **workspace read paths** — detached worktrees pinned to the
		// locked baseline commit, not the shared checkouts.
		AddDirs: mergeCrossDirs(crossWorkspaceDirs(spec.Workspace), spec.ExtraDirs),
		// Read-only: producing a design / a split / a verification report
		// must not touch any repository.
		DisallowedTools: []string{"Write", "Edit", "Bash", "NotebookEdit"},
	}
}

// mergeCrossDirs merges two directory lists, dropping duplicates and paths
// that do not exist on disk (the CLI rejects an unreadable --add-dir).
func mergeCrossDirs(lists ...[]string) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, list := range lists {
		for _, d := range list {
			d = strings.TrimSpace(d)
			if d == "" {
				continue
			}
			if _, dup := seen[d]; dup {
				continue
			}
			if st, err := os.Stat(d); err != nil || !st.IsDir() {
				continue
			}
			seen[d] = struct{}{}
			out = append(out, d)
		}
	}
	return out
}

// ── 项目约定的送达方式 ─────────────────────────────────────────────────────
// 项目约定（CLAUDE.md）在跨项目作业里的送达方式 —— 为什么走提示词而不是原生通道。
//
// 单项目阶段由 claudeDocInjection 决定怎么送：
//
//   - native 模式：把受管副本 materialize 成一份真实的 CLAUDE.md，通过
//     --add-dir 授权 + CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD 让 CLI 自己
//     加载（llm.StreamOpts.ClaudeMdDir）。
//   - prompt 模式：把文档正文拼进提示词。
//
// 跨项目作业**只能走 prompt 模式**，原因是原生通道一次只能带**一份**文档：
// llm.StreamOpts.ClaudeMdDir 是单个目录，N 个参与仓库的受管副本没法都放进去
// （后一个会覆盖前一个）。而且原生通道不带归属标注 —— 跨仓库方案必须让模型
// 知道「哪份约定约束哪个仓库」，一份无标注的文档表达不了这件事。
//
// 所以约定随提示词一起送达，每个参与仓库一节、标题带仓库名（见
// handler/cross_claude_doc.go 与 prompt/cross_claude_doc.go）。代价是受管副本
// 不再经过 CLI 自己的项目记忆通道 —— 但那份通道在跨项目作业里本来就用不上：
// 作业的工作目录不属于任何仓库（~/.novaworkbench/xreq/<xreqID>/<kind>），
// CLI 没有任何项目文档可读（--add-dir 只给读权限，不让 CLI 加载目录里的
// CLAUDE.md；要它加载必须打开那个进程级的环境变量，而打开它会让 gateway 把
// 所有 --add-dir 目录的 CLAUDE.md 都算进排除列表）。
//
// 结论：这里刻意**不**调用 claudeDocInjection.ApplyTo —— 它会设置
// opts.ClaudeMdDir，于是 gateway 打开那个环境变量、并把参与仓库目录里的
// CLAUDE.md 排除掉；受管副本只会有最后一个被 CLI 加载（其余 N-1 份仍然只
// 在提示词里），而那最后一份会在提示词与原生通道里各出现一次。

// ── 三个作业的提示词组装 ───────────────────────────────────────────────────
//
// 每个作业的提示词都必须带上**参与仓库的项目级 CLAUDE.md 约定**：跨项目作业
// 的工作目录不属于任何仓库，CLI 的项目记忆通道读不到任何项目文档（见上一节），
// 约定只能随提示词一起送达。约定取自**本次作业读的那棵树**（隔离 worktree，
// 落在锁定的基线上），与模型读到的代码同源。
//
// 提示词在**构造 llm.StreamOpts 之前**组装完毕 —— StreamOpts.Prompt 是值拷贝，
// 事后往那个字符串上追加内容对已经构造好的 opts 没有任何影响。

// crossAttachDir — 跨项目需求的附件在本地磁盘上的根目录。
//
// 跨项目作业**只在本地跑**（工作目录 ~/.novaworkbench/xreq/<id>/<kind>，没有
// 远端执行环境），所以附件目录恒为本地根，不需要 resolveAttachDirFor 那套
// SSH 同步。
func crossAttachDir(xreqID string) string {
	return filepath.Join(service.AttachmentRoot(), xreqID)
}

// crossAttachManifest — 跨项目需求的附件清单（Markdown）。
//
// 空串 = 该跨项目需求没有附件（或附件服务未接线）—— 调用方据此让提示词保持
// 原样。
func (h *CrossRequirementHandler) crossAttachManifest(xreqID string) string {
	if h.attachSvc == nil {
		return ""
	}
	return attachManifestFor(h.attachSvc, xreqID, crossAttachDir(xreqID))
}

// crossDescriptionForPrompt — 把描述里的 nova://attachment/<id> 标记重写成
// 真实磁盘路径。
//
// 不重写的话模型看到的是一串它打不开的协议串 —— 图片在它眼里就等于不存在。
// 跨需求引用的 id 刻意不重写（rewriteAttachmentMarkers 的既有语义），留在
// 文本里让模型自己发现那是别的需求的附件。
func (h *CrossRequirementHandler) crossDescriptionForPrompt(xreq *model.CrossRequirement) string {
	if h.attachSvc == nil {
		return xreq.Description
	}
	return rewriteAttachmentMarkers(h.attachSvc, xreq.ID, crossAttachDir(xreq.ID), xreq.Description)
}

// crossDesignPrompt 组装整体方案的提示词。
//
// ws 是本次作业读代码的工作目录集合 —— 约定也从**同一棵树**里取，这样提示词里
// 的规矩与模型实际读到的代码出自同一处基线。
func (h *CrossRequirementHandler) crossDesignPrompt(xreq *model.CrossRequirement, projects []prompt.CrossProjectInput, previousDesign string, ws *crossWorkspace) string {
	return prompt.BuildCrossDesignPrompt(xreq.Title, h.crossDescriptionForPrompt(xreq),
		crossAcceptance(xreq.AcceptanceCriteria), projects, previousDesign,
		h.crossConventions(xreq.ID, ws), h.crossAttachManifest(xreq.ID))
}

// crossSplitPrompt 组装按项目拆分的提示词。
func (h *CrossRequirementHandler) crossSplitPrompt(xreq *model.CrossRequirement, design string, projects []prompt.CrossProjectInput, ws *crossWorkspace) string {
	return prompt.BuildCrossSplitPrompt(xreq.Title, design, projects,
		h.crossConventions(xreq.ID, ws), h.crossAttachManifest(xreq.ID))
}

// crossPhasePrompt 组装阶段验证的提示词。
//
// 只有本阶段涉及的仓库贡献约定 —— 阶段验证的是这一片交付物，拿别的仓库的
// 规矩去判它，结论就没有意义。
func (h *CrossRequirementHandler) crossPhasePrompt(xreq *model.CrossRequirement, phase *model.CrossRequirementPhase, members []prompt.CrossPhaseMember) string {
	return prompt.BuildCrossPhasePrompt(phase.Name, phase.Description, members,
		service.DesignDocsToMarkdown(xreq.DesignDocs), h.crossPhaseConventions(members),
		h.crossAttachManifest(xreq.ID))
}

// ── 整体方案 ───────────────────────────────────────────────────────────────

// StartDesign launches the overall-design job.
//
// Preconditions (rejected BEFORE a job is created — the user must be told
// why, not shown a spinner that dies):
//
//	no participating project → XREQ_NO_PROJECTS
//	a design job is running   → XREQ_IN_PROGRESS (409)
//	archived                  → XREQ_ARCHIVED
//
// Returns {"job_id": ...}; the design document arrives on the job's SSE stream.
func (h *CrossRequirementHandler) StartDesign(w http.ResponseWriter, r *http.Request) {
	xreqID := r.PathValue("id")
	if h.svc == nil || h.jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	var body crossJobReq
	if err := decodeJSON(r, &body); err != nil && !errors.Is(err, io.EOF) {
		_ = err // a malformed body is not fatal — the persona defaults apply
	}

	xreq, err := h.svc.Get(xreqID)
	if err != nil {
		writeError(w, http.StatusNotFound, "CROSS_REQUIREMENT_NOT_FOUND", "跨项目需求不存在")
		return
	}
	if rejectIfTrashed(w, xreq) {
		return
	}
	if xreq.Status == model.XReqStatusArchived {
		writeError(w, http.StatusBadRequest, "XREQ_ARCHIVED", "已归档的跨项目需求不能生成方案")
		return
	}
	projects, err := h.svc.ListProjects(xreqID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	if len(projects) == 0 {
		writeError(w, http.StatusBadRequest, "XREQ_NO_PROJECTS", "请先选择参与项目")
		return
	}
	if existing, running := h.runs.peek(crossRunDesign, xreqID, ""); running {
		writeError(w, http.StatusConflict, "XREQ_IN_PROGRESS",
			fmt.Sprintf("已有整体方案任务在运行（job %s），请等待结束", existing))
		return
	}

	job := h.jobs.Create(xreqID)
	job.SetType("cross_design")

	if existing, ok := h.runs.acquire(crossRunDesign, xreqID, "", job.ID); !ok {
		job.Finish(1, store.JobError)
		writeError(w, http.StatusConflict, "XREQ_IN_PROGRESS",
			fmt.Sprintf("已有整体方案任务在运行（job %s），请等待结束", existing))
		return
	}

	systemPrompt, modelName, configID := h.crossRoleConfig()
	if strings.TrimSpace(body.Model) != "" {
		modelName = body.Model
	}
	if strings.TrimSpace(body.ClaudeConfigID) != "" {
		configID = body.ClaudeConfigID
	}
	h.runs.start(job.ID, xreqID, crossRunDesign, "", modelName)

	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})

	go h.runDesign(job, xreq, body, systemPrompt, modelName, configID)
}

// runDesign is the goroutine body of the overall-design job.
func (h *CrossRequirementHandler) runDesign(job *store.Job, xreq *model.CrossRequirement, body crossJobReq,
	systemPrompt, modelName, configID string) {
	defer h.runs.release(crossRunDesign, xreq.ID, "")
	defer func() {
		if r := recover(); r != nil {
			h.finishCrossError(job, crossRunDesign, xreq.ID, "", "整体方案生成过程异常终止")
		}
	}()

	// Prepare the working set BEFORE the prompt is built: the prompt tells the
	// model which paths to read and which baseline it is reasoning about, so
	// the paths have to exist first. A failed fetch aborts the job — a design
	// written against a tree we know is stale is worse than no design.
	ws, wsErr := h.buildCrossWorkspace(context.Background(), xreq.ID, func(s string) {
		job.Append(store.LogLine{Type: "message", Content: s})
	})
	if wsErr != nil {
		h.finishCrossError(job, crossRunDesign, xreq.ID, "", "准备仓库基线失败: "+wsErr.Error())
		return
	}

	projects, err := h.crossProjectInputs(xreq.ID, ws)
	if err != nil {
		h.finishCrossError(job, crossRunDesign, xreq.ID, "", "读取参与项目失败: "+err.Error())
		return
	}
	previous := service.DesignDocsToMarkdown(xreq.DesignDocs)
	promptText := h.crossDesignPrompt(xreq, projects, previous, ws)

	workDir, err := crossWorkspaceDir(xreq.ID, crossRunDesign)
	if err != nil {
		h.finishCrossError(job, crossRunDesign, xreq.ID, "", "创建工作目录失败: "+err.Error())
		return
	}

	opts := h.crossStreamOpts(crossJobSpec{
		Prompt:         promptText,
		WorkDir:        workDir,
		SystemPrompt:   systemPrompt,
		Model:          modelName,
		Effort:         resolveEffort(h.llm, body.Effort, llm.StageDesign),
		ClaudeConfigID: configID,
		Workspace:      ws,
	})

	job.Append(store.LogLine{Type: "phase", Content: "🧭 正在生成跨项目整体方案…"})

	out := runClaudeStream(jobSink{job}, h.llm.StreamCmd(context.Background(), opts), "cross-design",
		h.crossUsageCtx("cross_design", xreq.ID, job.ID, modelName, configID), nil, nil, 0,
		withStallTimeout(crossJobStallTimeout))

	markdown := strings.TrimSpace(out.finalResult)
	if markdown == "" {
		markdown = strings.TrimSpace(out.planContent)
	}
	if markdown == "" {
		h.finishCrossError(job, crossRunDesign, xreq.ID, "", "未产出整体方案，请重试")
		return
	}
	if !prompt.HasCrossContractSection(markdown) {
		// The contract section is mandatory — a design without it would let
		// each repository invent its own interface. The report is still
		// surfaced so the user can see what came back.
		h.runs.setResult(job.ID, markdown, nil, "")
		job.Append(store.LogLine{Type: "error", Content: "⚠️ 整体方案缺少「跨项目接口契约」章节，请重新生成或手工补齐"})
		job.Append(store.LogLine{Type: "message", Content: markdown})
		h.runs.finish(job.ID, store.JobError, "整体方案缺少「跨项目接口契约」章节")
		job.Finish(1, store.JobError)
		return
	}

	if err := h.svc.SaveDesign(xreq.ID, markdown, "", job.ID); err != nil {
		h.finishCrossError(job, crossRunDesign, xreq.ID, "", "保存整体方案失败: "+err.Error())
		return
	}
	// Record which commit each repository was read at. The ledger is what
	// makes "这份方案是对着哪个提交写的" answerable later — and it is the
	// only record of which worktrees this cross requirement created.
	if err := h.svc.UpsertBases(xreq.ID, crossWorkspaceBases(ws)); err != nil {
		job.Append(store.LogLine{Type: "message", Content: "⚠️ 写入基线台账失败：" + err.Error()})
	}
	if err := h.svc.RefreshDerivedStatus(xreq.ID); err != nil {
		_ = err
	}
	h.runs.setResult(job.ID, markdown, nil, "")
	h.runs.finish(job.ID, store.JobDone, "")
	job.Append(store.LogLine{Type: "message", Content: prompt.CrossDesignSummary(len(projects))})
	job.Finish(0, store.JobDone)
}

// ── 按项目拆分 ─────────────────────────────────────────────────────────────

// StartSplit launches the cross-project split job.
//
// 拆分**只能做一次**：已经拆过的跨项目需求 → 409 XREQ_ALREADY_SPLIT。
// 在这里就拒绝（而不是等到确认那一步）有两个理由：
//   - 用户立刻知道「不能再拆了」，而不是白等一次 AI 作业；
//   - 反复拆分累积子需求的路径从入口就被堵死。
//
// Returns {"job_id": ...}; the candidate list arrives on the job's SSE stream
// and can also be polled through GET .../split/state.
func (h *CrossRequirementHandler) StartSplit(w http.ResponseWriter, r *http.Request) {
	xreqID := r.PathValue("id")
	if h.svc == nil || h.jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	var body crossJobReq
	if err := decodeJSON(r, &body); err != nil && !errors.Is(err, io.EOF) {
		_ = err
	}

	xreq, err := h.svc.Get(xreqID)
	if err != nil {
		writeError(w, http.StatusNotFound, "CROSS_REQUIREMENT_NOT_FOUND", "跨项目需求不存在")
		return
	}
	if rejectIfTrashed(w, xreq) {
		return
	}
	if xreq.Status == model.XReqStatusArchived {
		writeError(w, http.StatusBadRequest, "XREQ_ARCHIVED", "已归档的跨项目需求不能拆分")
		return
	}
	split, err := h.svc.AlreadySplit(xreqID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	if split {
		writeError(w, http.StatusConflict, "XREQ_ALREADY_SPLIT", "该跨项目需求已经拆分过了，不能重复拆分")
		return
	}
	design := service.DesignDocsToMarkdown(xreq.DesignDocs)
	if design == "" {
		writeError(w, http.StatusBadRequest, "XREQ_NO_DESIGN", "请先生成整体方案，再按项目拆分")
		return
	}
	projects, err := h.crossProjectInputs(xreqID, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	if len(projects) == 0 {
		writeError(w, http.StatusBadRequest, "XREQ_NO_PROJECTS", "请先选择参与项目")
		return
	}
	if existing, running := h.runs.peek(crossRunSplit, xreqID, ""); running {
		writeError(w, http.StatusConflict, "XREQ_IN_PROGRESS",
			fmt.Sprintf("已有拆分任务在运行（job %s），请等待结束", existing))
		return
	}

	job := h.jobs.Create(xreqID)
	job.SetType("cross_split")

	if existing, ok := h.runs.acquire(crossRunSplit, xreqID, "", job.ID); !ok {
		job.Finish(1, store.JobError)
		writeError(w, http.StatusConflict, "XREQ_IN_PROGRESS",
			fmt.Sprintf("已有拆分任务在运行（job %s），请等待结束", existing))
		return
	}

	systemPrompt, modelName, configID := h.crossRoleConfig()
	if strings.TrimSpace(body.Model) != "" {
		modelName = body.Model
	}
	if strings.TrimSpace(body.ClaudeConfigID) != "" {
		configID = body.ClaudeConfigID
	}
	h.runs.start(job.ID, xreqID, crossRunSplit, "", modelName)

	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})

	go h.runSplit(job, xreq, body, design, systemPrompt, modelName, configID)
}

// runSplit is the goroutine body of the cross-project split job.
func (h *CrossRequirementHandler) runSplit(job *store.Job, xreq *model.CrossRequirement, body crossJobReq,
	design string, systemPrompt, modelName, configID string) {
	defer h.runs.release(crossRunSplit, xreq.ID, "")
	defer func() {
		if r := recover(); r != nil {
			h.finishCrossError(job, crossRunSplit, xreq.ID, "", "拆分过程异常终止")
		}
	}()

	// Same working set as the overall design: the split reads the very same
	// repositories the design was written against, so it prepares them the
	// same way — fetch, lock the baseline, hand the model the pinned worktree.
	ws, wsErr := h.buildCrossWorkspace(context.Background(), xreq.ID, func(s string) {
		job.Append(store.LogLine{Type: "message", Content: s})
	})
	if wsErr != nil {
		h.finishCrossError(job, crossRunSplit, xreq.ID, "", "准备仓库基线失败: "+wsErr.Error())
		return
	}
	projects, pErr := h.crossProjectInputs(xreq.ID, ws)
	if pErr != nil {
		h.finishCrossError(job, crossRunSplit, xreq.ID, "", "读取参与项目失败: "+pErr.Error())
		return
	}
	promptText := h.crossSplitPrompt(xreq, design, projects, ws)

	workDir, err := crossWorkspaceDir(xreq.ID, crossRunSplit)
	if err != nil {
		h.finishCrossError(job, crossRunSplit, xreq.ID, "", "创建工作目录失败: "+err.Error())
		return
	}

	opts := h.crossStreamOpts(crossJobSpec{
		Prompt:         promptText,
		WorkDir:        workDir,
		SystemPrompt:   systemPrompt,
		Model:          modelName,
		Effort:         resolveEffort(h.llm, body.Effort, llm.StageDesign),
		ClaudeConfigID: configID,
		Workspace:      ws,
	})

	job.Append(store.LogLine{Type: "phase", Content: "🧩 正在按项目拆分整体方案…"})

	out := runClaudeStream(jobSink{job}, h.llm.StreamCmd(context.Background(), opts), "cross-split",
		h.crossUsageCtx("cross_split", xreq.ID, job.ID, modelName, configID), nil, nil, 0,
		withStallTimeout(crossJobStallTimeout))

	jsonPart, _ := extractTrailingJSON(out.finalResult)
	if strings.TrimSpace(jsonPart) == "" {
		jsonPart = out.planContent
	}
	candidates := prompt.NormalizeCrossSplitCandidates(jsonPart, projects)
	if len(candidates) == 0 {
		h.finishCrossError(job, crossRunSplit, xreq.ID, "", "未能从整体方案中解析出候选子需求，请完善方案后重试")
		return
	}

	h.runs.setResult(job.ID, "", candidates, "")
	h.runs.finish(job.ID, store.JobDone, "")
	// Refresh the ledger: the split may have run on a newer baseline than the
	// design did (the fetch moves with the upstream), and the ledger should
	// say which commit the CURRENT reading came from.
	if err := h.svc.UpsertBases(xreq.ID, crossWorkspaceBases(ws)); err != nil {
		job.Append(store.LogLine{Type: "message", Content: "⚠️ 写入基线台账失败：" + err.Error()})
	}
	job.Append(store.LogLine{Type: "message", Content: prompt.CrossSplitSummary(candidates)})
	job.Finish(0, store.JobDone)
}

// SplitState reports the most recent split run of a cross requirement.
//
// 「没跑过」是正常状态不是错误：回 200 + {"status":"none"}。
func (h *CrossRequirementHandler) SplitState(w http.ResponseWriter, r *http.Request) {
	xreqID := r.PathValue("id")
	run := h.runs.latestFor(crossRunSplit, xreqID, "")
	if run == nil || run.Consumed {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":        "none",
			"job_id":        "",
			"running":       false,
			"error_message": "",
			"candidates":    []prompt.CrossSplitCandidate{},
			"model":         "",
			"created_at":    int64(0),
			"finished_at":   int64(0),
		})
		return
	}
	var created, finished int64
	if !run.CreatedAt.IsZero() {
		created = run.CreatedAt.UnixMilli()
	}
	if !run.FinishedAt.IsZero() {
		finished = run.FinishedAt.UnixMilli()
	}
	candidates := run.Candidates
	if candidates == nil {
		candidates = []prompt.CrossSplitCandidate{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        string(run.Status),
		"job_id":        run.JobID,
		"running":       run.Status == store.JobRunning,
		"error_message": run.Error,
		"candidates":    candidates,
		"model":         run.Model,
		"created_at":    created,
		"finished_at":   finished,
	})
}

// DesignState reports the most recent overall-design run of a cross requirement.
//
// 「没跑过」是正常状态不是错误：回 200 + {"status":"none"}。
//
// This endpoint is the ONLY way for a reopened page to find the job it left
// behind: the job itself lives in the JobStore (closing the page does not
// kill it — runDesign uses context.Background()), but its job id only exists
// in the in-process registry.
//
// It deliberately does NOT read cross_requirements.design_job_id: that column
// means "the job that produced the current design" (SaveDesign writes it
// only on success) — empty while the job runs, filled once it finished. Using
// it as "the running job" would leave the page without an anchor for exactly
// the window that matters.
func (h *CrossRequirementHandler) DesignState(w http.ResponseWriter, r *http.Request) {
	xreqID := r.PathValue("id")
	run := h.runs.latestFor(crossRunDesign, xreqID, "")
	if run == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":        "none",
			"job_id":        "",
			"running":       false,
			"error_message": "",
			"design":        "",
			"model":         "",
			"created_at":    int64(0),
			"finished_at":   int64(0),
		})
		return
	}
	var created, finished int64
	if !run.CreatedAt.IsZero() {
		created = run.CreatedAt.UnixMilli()
	}
	if !run.FinishedAt.IsZero() {
		finished = run.FinishedAt.UnixMilli()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        string(run.Status),
		"job_id":        run.JobID,
		"running":       run.Status == store.JobRunning,
		"error_message": run.Error,
		"design":        run.Design,
		"model":         run.Model,
		"created_at":    created,
		"finished_at":   finished,
	})
}

// ── 阶段验证 ───────────────────────────────────────────────────────────────

// StartVerify launches the phase-verification job.
func (h *CrossRequirementHandler) StartVerify(w http.ResponseWriter, r *http.Request) {
	xreqID := r.PathValue("id")
	phaseID := r.PathValue("pid")
	if h.svc == nil || h.jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "跨项目需求服务未启用")
		return
	}
	var body crossJobReq
	if err := decodeJSON(r, &body); err != nil && !errors.Is(err, io.EOF) {
		_ = err
	}

	if _, err := h.svc.GetPhase(xreqID, phaseID); err != nil {
		writeError(w, http.StatusNotFound, "PHASE_NOT_FOUND", "阶段不存在")
		return
	}
	xreq, err := h.svc.Get(xreqID)
	if err != nil {
		writeError(w, http.StatusNotFound, "CROSS_REQUIREMENT_NOT_FOUND", "跨项目需求不存在")
		return
	}
	if rejectIfTrashed(w, xreq) {
		return
	}
	if existing, running := h.runs.peek(crossRunVerify, xreqID, phaseID); running {
		writeError(w, http.StatusConflict, "XREQ_IN_PROGRESS",
			fmt.Sprintf("已有阶段验证任务在运行（job %s），请等待结束", existing))
		return
	}

	job := h.jobs.Create(xreqID)
	job.SetType("cross_verify")

	if existing, ok := h.runs.acquire(crossRunVerify, xreqID, phaseID, job.ID); !ok {
		job.Finish(1, store.JobError)
		writeError(w, http.StatusConflict, "XREQ_IN_PROGRESS",
			fmt.Sprintf("已有阶段验证任务在运行（job %s），请等待结束", existing))
		return
	}

	systemPrompt, modelName, configID := h.crossRoleConfig()
	if strings.TrimSpace(body.Model) != "" {
		modelName = body.Model
	}
	if strings.TrimSpace(body.ClaudeConfigID) != "" {
		configID = body.ClaudeConfigID
	}
	h.runs.start(job.ID, xreqID, crossRunVerify, phaseID, modelName)

	if err := h.svc.SetPhaseVerifyJob(phaseID, job.ID); err != nil {
		_ = err
	}

	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})

	go h.runVerify(job, xreqID, phaseID, body, systemPrompt, modelName, configID)
}

// runVerify is the goroutine body of the phase-verification job.
func (h *CrossRequirementHandler) runVerify(job *store.Job, xreqID, phaseID string, body crossJobReq,
	systemPrompt, modelName, configID string) {
	defer h.runs.release(crossRunVerify, xreqID, phaseID)
	defer func() {
		if r := recover(); r != nil {
			h.finishCrossError(job, crossRunVerify, xreqID, phaseID, "阶段验证过程异常终止")
		}
	}()

	phase, err := h.svc.GetPhase(xreqID, phaseID)
	if err != nil {
		h.finishCrossError(job, crossRunVerify, xreqID, phaseID, "读取阶段失败: "+err.Error())
		return
	}
	xreq, err := h.svc.Get(xreqID)
	if err != nil {
		h.finishCrossError(job, crossRunVerify, xreqID, phaseID, "读取跨项目需求失败: "+err.Error())
		return
	}

	members := h.crossPhaseMembers(xreqID, phase)
	promptText := h.crossPhasePrompt(xreq, phase, members)

	// The verification reads TWO sets of directories:
	//
	//   - the **baseline** worktrees (the cross requirement's own), so the
	//     model can see the tree the contract was written against;
	//   - each member's **implementation** worktree — the code that was
	//     actually written for this phase. Without this the verification
	//     inspects the base branch and "verifies" code that does not exist.
	//
	// The baseline fetch is best-effort here: the implementation is what is
	// being judged, so a repository that cannot be synced must not abort the
	// verification — it just means the contract side is unavailable.
	ws, wsErr := h.buildCrossWorkspace(context.Background(), xreqID, func(s string) {
		job.Append(store.LogLine{Type: "message", Content: s})
	})
	if wsErr != nil {
		job.Append(store.LogLine{Type: "error", Content: "⚠️ 准备基线工作区失败（不影响验证实现代码）：" + wsErr.Error()})
		ws = &crossWorkspace{Projects: []crossWorkspaceProject{}}
	}

	implDirs := h.crossPhaseImplDirs(members)

	workDir, err := crossWorkspaceDir(xreqID, crossRunVerify)
	if err != nil {
		h.finishCrossError(job, crossRunVerify, xreqID, phaseID, "创建工作目录失败: "+err.Error())
		return
	}

	opts := h.crossStreamOpts(crossJobSpec{
		Prompt:         promptText,
		WorkDir:        workDir,
		SystemPrompt:   systemPrompt,
		Model:          modelName,
		Effort:         resolveEffort(h.llm, body.Effort, llm.StageDesign),
		ClaudeConfigID: configID,
		Workspace:      ws,
		ExtraDirs:      implDirs,
	})

	job.Append(store.LogLine{Type: "phase", Content: "🔍 正在做阶段验证…"})

	out := runClaudeStream(jobSink{job}, h.llm.StreamCmd(context.Background(), opts), "cross-verify",
		h.crossUsageCtx("cross_verify", xreqID, job.ID, modelName, configID), nil, nil, 0,
		withStallTimeout(crossJobStallTimeout))

	report := strings.TrimSpace(out.finalResult)
	if report == "" {
		report = strings.TrimSpace(out.planContent)
	}
	if report == "" {
		h.finishCrossError(job, crossRunVerify, xreqID, phaseID, "未产出阶段验证报告，请重试")
		return
	}

	// The report is PERSISTED on the phase — it must survive a backend
	// restart (the JobStore ring buffer does not).
	if err := h.svc.SetPhaseVerifyReport(phaseID, report); err != nil {
		h.finishCrossError(job, crossRunVerify, xreqID, phaseID, "保存验证报告失败: "+err.Error())
		return
	}
	if err := h.svc.RefreshDerivedStatus(xreqID); err != nil {
		_ = err
	}
	h.runs.setResult(job.ID, "", nil, report)
	h.runs.finish(job.ID, store.JobDone, "")
	job.Append(store.LogLine{Type: "message", Content: prompt.CrossPhaseSummary(len(members))})
	job.Finish(0, store.JobDone)
}

// crossPhaseMembers builds the prompt-side view of a phase's members.
func (h *CrossRequirementHandler) crossPhaseMembers(xreqID string, phase *model.CrossRequirementPhase) []prompt.CrossPhaseMember {
	out := []prompt.CrossPhaseMember{}
	if phase == nil {
		return out
	}
	children, err := h.svc.ListChildren(xreqID)
	if err != nil {
		return out
	}
	byID := map[string]model.CrossRequirementChild{}
	for _, c := range children {
		byID[c.ID] = c
	}
	for _, item := range phase.Items {
		child, ok := byID[item.RequirementID]
		if !ok {
			continue
		}
		member := prompt.CrossPhaseMember{
			RequirementID: child.ID,
			Title:         child.Title,
			ProjectID:     child.ProjectID,
			ProjectName:   child.ProjectName,
			Status:        child.Status,
		}
		if h.reqSvc != nil {
			if row, rErr := h.reqSvc.Get(child.ID); rErr == nil && row != nil {
				member.Description = row.Description
				// The implementation lives in the member's own worktree —
				// that is the code the phase has to verify.
				member.WorktreePath = strings.TrimSpace(row.WorktreePath)
			}
		}
		if h.projectSvc != nil {
			if proj, pErr := h.projectSvc.Get(child.ProjectID); pErr == nil && proj != nil {
				member.ProjectPath = proj.LocalPath
			}
		}
		out = append(out, member)
	}
	return out
}

// crossPhaseImplDirs returns the implementation directories of a phase's
// members — their own worktrees.
//
// A member that has not started development yet has no worktree; it is
// skipped here and the prompt says so, so the model knows the implementation
// is missing rather than silently reading the base branch and reporting the
// phase as verified.
func (h *CrossRequirementHandler) crossPhaseImplDirs(members []prompt.CrossPhaseMember) []string {
	dirs := []string{}
	for _, m := range members {
		if m.WorktreePath == "" {
			continue
		}
		dirs = append(dirs, m.WorktreePath)
	}
	return dirs
}

// ── SSE ────────────────────────────────────────────────────────────────────

// StreamDesignJob streams the overall-design job.
//
// The terminal frame carries the design document in `result.design` so the
// page can render it without a second round trip.
func (h *CrossRequirementHandler) StreamDesignJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	job, ok := h.jobs.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found")
		return
	}
	if !crossJobBelongsTo(job, r.PathValue("id")) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found")
		return
	}
	streamJobSSE(w, r, job, func(status store.JobStatus, exitCode int) []byte {
		var result any
		var errMsg string
		if run := h.runs.get(jobID); run != nil {
			errMsg = run.Error
			result = map[string]any{"design": run.Design}
		}
		return crossDoneFrame(job, status, exitCode, errMsg, result)
	})
}

// StreamSplitJob streams the split job; the terminal frame carries the
// candidates in `result.candidates`.
func (h *CrossRequirementHandler) StreamSplitJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	job, ok := h.jobs.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found")
		return
	}
	if !crossJobBelongsTo(job, r.PathValue("id")) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found")
		return
	}
	streamJobSSE(w, r, job, func(status store.JobStatus, exitCode int) []byte {
		var result any
		var errMsg string
		if run := h.runs.get(jobID); run != nil {
			errMsg = run.Error
			candidates := run.Candidates
			if candidates == nil {
				candidates = []prompt.CrossSplitCandidate{}
			}
			result = map[string]any{"candidates": candidates}
		}
		return crossDoneFrame(job, status, exitCode, errMsg, result)
	})
}

// StreamVerifyJob streams the phase-verification job; the terminal frame
// carries the report in `result.report`.
func (h *CrossRequirementHandler) StreamVerifyJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	job, ok := h.jobs.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found")
		return
	}
	if !crossJobBelongsTo(job, r.PathValue("id")) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found")
		return
	}
	streamJobSSE(w, r, job, func(status store.JobStatus, exitCode int) []byte {
		var result any
		var errMsg string
		if run := h.runs.get(jobID); run != nil {
			errMsg = run.Error
			result = map[string]any{"report": run.Report}
		}
		return crossDoneFrame(job, status, exitCode, errMsg, result)
	})
}

// crossJobBelongsTo reports whether a job was created for a cross requirement.
//
// The SSE endpoints look a job up by id alone. Without this check a job id
// (which is only 8 hex chars and therefore guessable) would let anyone with
// read access to one cross requirement stream the output of another. The job
// carries the owning id in RequirementID (set by JobStore.Create).
func crossJobBelongsTo(job *store.Job, xreqID string) bool {
	if job == nil {
		return false
	}
	return job.RequirementID == xreqID
}

// crossDoneFrame builds the terminal SSE frame of a cross-requirement job.
func crossDoneFrame(job *store.Job, status store.JobStatus, exitCode int, errMsg string, result any) []byte {
	data, _ := json.Marshal(map[string]any{
		"type":          "job_done",
		"status":        string(status),
		"exit_code":     exitCode,
		"error_kind":    job.ErrorKind,
		"error_message": errMsg,
		"started_at":    job.StartedAt.UnixMilli(),
		"finished_at":   job.FinishedAt.UnixMilli(),
		"duration_ms":   job.FinishedAt.Sub(job.StartedAt).Milliseconds(),
		"result":        result,
	})
	return data
}

// ── 辅助 ───────────────────────────────────────────────────────────────────

// finishCrossError reports a failed job on the stream.
//
// The failure reason is recorded BOTH on the registry (so the state endpoint
// can serve it after the page is reopened) and as an "error" log line (so it
// shows up in the live stream).
func (h *CrossRequirementHandler) finishCrossError(job *store.Job, kind, xreqID, phaseID, msg string) {
	h.runs.finish(job.ID, store.JobError, msg)
	job.Append(store.LogLine{Type: "error", Content: msg})
	job.Finish(1, store.JobError)
}

// crossRoleConfig resolves the persona / model / claude config for a
// cross-requirement job.
//
// The architect persona is the closest fit (it is the "方案设计" role). A
// missing persona never blocks the run — the CLI defaults apply.
func (h *CrossRequirementHandler) crossRoleConfig() (systemPrompt, modelName, configID string) {
	if h.roleSvc == nil {
		return "", "", ""
	}
	r, err := h.roleSvc.GetByKey("architect")
	if err != nil || r == nil {
		return "", "", ""
	}
	if h.claudeCfg != nil {
		if cfg, cErr := h.claudeCfg.ResolveRoleConfig(r); cErr == nil && cfg != nil {
			return r.SystemPrompt, effectiveModelFromValues(r.Model, cfg.DefaultModel), cfg.ID
		}
	}
	return r.SystemPrompt, r.Model, ""
}

// crossUsageCtx builds the usage recorder context for a cross-requirement
// job. Token usage must be attributed to the run, otherwise the cost
// dashboards cannot see the AI work this feature triggers.
func (h *CrossRequirementHandler) crossUsageCtx(step, xreqID, jobID, modelName, configID string) *usageCtx {
	ctx := &usageCtx{
		Rec:            h.usageSvc,
		RequirementID:  xreqID,
		JobID:          jobID,
		Step:           step,
		Model:          modelName,
		ClaudeConfigID: configID,
		Summary:        "跨项目需求",
	}
	return ctx
}
