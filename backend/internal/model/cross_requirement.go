package model

// 跨项目需求（Cross-Project Requirement）——横跨多个项目 / 仓库的顶层需求实体。
//
// 它**不是** requirements 表的一种 kind：requirements.project_id 是 NOT NULL +
// FK + CASCADE，且既有数十条路径假设「一个需求 = 一个仓库」（worktree 隔离 /
// git 分支 / PR / review / 子任务编排 / token 统计）。把 project_id 改成可空会
// 波及全部既有路径。
//
// 所以这里新增一个独立的顶层实体，子需求仍然是**普通的 requirements 行**
// （各归各的项目），通过 requirements.cross_requirement_id 挂回本实体 ——
// 子需求因此自动继承现有的 analyst / architect / coding 三阶段与全部下游能力。

// 跨项目需求的生命周期状态。
//
// 注意：「进展」不落在这些状态里 —— 进展（完成百分比、各项目完成数）是读路径
// 实时聚合出来的（见 CrossRequirementProgress），不冗余存储，避免与子需求的
// 真实状态不一致。
const (
	// XReqStatusDraft — 刚创建，还没有整体方案。
	XReqStatusDraft = "draft"
	// XReqStatusDesigning — 整体方案作业运行中。
	XReqStatusDesigning = "designing"
	// XReqStatusDesigned — 整体方案已落库。
	XReqStatusDesigned = "designed"
	// XReqStatusDeveloping — 已拆分出子需求，开发进行中。
	XReqStatusDeveloping = "developing"
	// XReqStatusIntegrating — 进入阶段验证（联调 / 集成测试）。
	XReqStatusIntegrating = "integrating"
	// XReqStatusDone — 全部子需求完成，且（若有）阶段全部通过或跳过。
	XReqStatusDone = "done"
	// XReqStatusArchived — 归档。
	XReqStatusArchived = "archived"
)

// 阶段状态。
//
// pending / ready 是**派生**状态（读路径算出来，不落库）：pending = 成员还没
// 全部完成；ready = 成员全部完成且依赖满足，可以开始验证。passed / failed /
// skipped 是**人工结论**，落库且不会被刷新覆盖。
const (
	// PhaseStatusPending — 阶段成员尚未全部完成。
	PhaseStatusPending = "pending"
	// PhaseStatusReady — 阶段成员全部完成且依赖满足，可以开始验证。
	PhaseStatusReady = "ready"
	// PhaseStatusVerifying — 阶段验证作业运行中。
	PhaseStatusVerifying = "verifying"
	// PhaseStatusPendingReview — 验证作业完成，产出报告，等待人工判定。
	PhaseStatusPendingReview = "pending_review"
	// PhaseStatusPassed — 人工判定通过。
	PhaseStatusPassed = "passed"
	// PhaseStatusFailed — 人工判定不通过（需返工）。
	PhaseStatusFailed = "failed"
	// PhaseStatusSkipped — 人工跳过该阶段。
	PhaseStatusSkipped = "skipped"
)

// CrossRequirement — 跨项目需求顶层实体。
type CrossRequirement struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Status — 见 XReqStatus* 常量。
	Status string `json:"status"`
	// Priority — low | medium | high | urgent，与 requirements 同口径。
	Priority string `json:"priority"`
	// Kind — requirement | issue，与 requirements 同口径。
	Kind string `json:"kind"`
	// AcceptanceCriteria — JSON 数组（原文存储）。
	AcceptanceCriteria string `json:"acceptance_criteria"`
	// DesignDocs — 整体方案，JSON 数组，形状与 requirements.design_docs 一致。
	DesignDocs string `json:"design_docs"`
	// DesignSessionID / DesignJobID — 整体方案作业的会话 / 作业 id（便于刷新后
	// 重连 SSE）。
	DesignSessionID string `json:"design_session_id"`
	DesignJobID     string `json:"design_job_id"`
	// SplitBatchID — 拆分批次 id（spb_xxx）。空 = 从未拆分。
	//
	// 一个跨项目需求**只能拆一次**：确认拆分时用这一列做原子抢占（带条件的
	// UPDATE），并发重复确认只有一方能成功，另一方拿到 409。前端据此禁用
	// 「按项目拆分」按钮，不再让用户走到一个必然被拒的请求。
	SplitBatchID string `json:"split_batch_id"`
	// StatusLocked reports that the lifecycle status was set **by a human**
	// (archive / manual "mark as done") and must not be recomputed by the
	// read-path derivation.
	//
	// Without it, a manual status is silently overwritten by the next read:
	// the derivation recomputes draft/designed/developing/integrating/done
	// from the children + phases and writes the result back, so a manually
	// set "done" flips back to "developing" as soon as a child is still open.
	//
	// The lock is released when the facts behind the status change (a new
	// overall design / a new batch of sub-requirements).
	StatusLocked bool `json:"status_locked"`
	CreatedBy    string `json:"created_by"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	CompletedAt  string `json:"completed_at"`
	// DeletedAt — 回收站标记。空串 = 不在回收站；非空 = 被移入回收站的时间。
	//
	// 与 Status **正交**：status 是生命周期（草稿 → … → done / 归档），
	// deleted_at 是「这条要不要出现在列表里」。分开的理由是「归档」（保留，
	// 只是收起来）与「删除」（移进回收站，可恢复）是两件事 —— 混成一个状态，
	// 「恢复」就无从谈起。
	//
	// 回收站里的行**只读**：设计 / 拆分 / 阶段 / 验证等写路径一律拒绝
	// （service 层的 ErrCrossRequirementInTrash）。
	DeletedAt string `json:"deleted_at"`
}

// CrossRequirementProject — 参与项目映射（一个跨项目需求 ↔ N 个项目）。
type CrossRequirementProject struct {
	ID                 string `json:"id"`
	CrossRequirementID string `json:"cross_requirement_id"`
	ProjectID          string `json:"project_id"`
	// ProjectName — 只读派生字段（读路径 JOIN 出来），用于 UI 展示。
	ProjectName string `json:"project_name"`
	// Role — 自由文本（前端 / 后端 / 移动端 …），用于 UI 展示。
	Role string `json:"role"`
	// BaseBranch — 该参与项目的**基础分支**：这个仓库里的子需求从哪条分支
	// 开发。
	//
	// 跨项目需求里每个仓库常常处在不同的发布线上（A 在 develop、B 在
	// release/2026q1），所以基础分支必须逐项目指定。空串 = 不指定，子需求
	// 沿用该项目的默认分支（projects.default_branch）。
	BaseBranch string `json:"base_branch"`
	// SortOrder — 展示顺序（1 起算）。
	SortOrder int `json:"sort_order"`
}

// CrossRequirementPhase — 阶段（联调 / 集成测试单元）。
type CrossRequirementPhase struct {
	ID                 string `json:"id"`
	CrossRequirementID string `json:"cross_requirement_id"`
	Name               string `json:"name"`
	Description        string `json:"description"`
	SortOrder          int    `json:"sort_order"`
	// Status — 见 PhaseStatus* 常量。pending / ready 由读路径派生。
	Status string `json:"status"`
	// VerifyJobID — 阶段验证作业的 JobStore id（空 = 没跑过 / 已清空）。
	VerifyJobID string `json:"verify_job_id"`
	// VerifyReport — 阶段验证报告（Markdown）。**落库**，不依赖 JobStore 的
	// 内存环 —— 后端重启后仍可回看。
	VerifyReport string `json:"verify_report"`
	VerifiedAt   string `json:"verified_at"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	// Items — 阶段包含的子需求（只读派生字段，读路径装配）。
	Items []CrossRequirementPhaseItem `json:"items"`
}

// CrossRequirementPhaseItem — 阶段 ↔ 子需求 的成员行。
type CrossRequirementPhaseItem struct {
	ID            string `json:"id"`
	PhaseID       string `json:"phase_id"`
	RequirementID string `json:"requirement_id"`
	// Status — 只读派生字段：成员需求当前的状态（读路径 JOIN 出来），用于
	// 计算阶段的 pending / ready。
	Status string `json:"status"`
}

// CrossRequirementChild — 跨项目需求下的子需求（普通 requirements 行）。
type CrossRequirementChild struct {
	ID          string `json:"id"`
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	Priority    string `json:"priority"`
	SplitOrder  int    `json:"split_order"`
	// BaseBranch — 该子需求实际使用的基础分支（读路径 JOIN 出来）。
	//
	// 它来自「参与项目 → 基础分支」的映射，在拆分时盖到子需求行上；空 = 沿用
	// 该项目的默认分支。
	BaseBranch string `json:"base_branch"`
	// DependsOn — 前置依赖的子需求 id（跨项目也可以）。
	DependsOn []string `json:"depends_on"`
	// BlockedBy — 尚未完成的前置依赖 id 列表（闸门实际拦住的那些）。
	BlockedBy []string `json:"blocked_by"`
}

// CrossRequirementProjectProgress — 单个参与项目的进展。
type CrossRequirementProjectProgress struct {
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	Role        string `json:"role"`
	// BaseBranch — 该参与项目的基础分支（空 = 沿用项目默认分支）。
	BaseBranch string `json:"base_branch"`
	// Total / Done / Blocked — 该项目下的子需求计数。
	Total   int `json:"total"`
	Done    int `json:"done"`
	Blocked int `json:"blocked"`
}

// CrossRequirementProgress — 跨项目需求的整体进展（实时聚合，不落库）。
type CrossRequirementProgress struct {
	// Total / Done — 子需求总数与已完成数。
	Total int `json:"total"`
	Done  int `json:"done"`
	// Blocked — 被未完成的前置依赖拦住的子需求数。
	Blocked int `json:"blocked"`
	// Percent — 完成百分比（0–100，Total=0 时为 0）。
	Percent int `json:"percent"`
	// Projects — 各参与项目的进展（按 sort_order 排列）。
	Projects []CrossRequirementProjectProgress `json:"projects"`
	// StatusCounts — 子需求状态分布（status → 数量）。
	StatusCounts map[string]int `json:"status_counts"`
}

// CrossRequirementDetail — 详情页聚合载荷。
type CrossRequirementDetail struct {
	CrossRequirement
	// Projects — 参与项目（含进展）。
	Projects []CrossRequirementProjectProgress `json:"projects"`
	// Children — 子需求列表（按 split_order / 创建时间排列）。
	Children []CrossRequirementChild `json:"children"`
	// Dependencies — 跨项目依赖边（requirement_id → depends_on_id）。
	Dependencies []CrossRequirementDependency `json:"dependencies"`
	// Phases — 阶段列表（含成员）。
	Phases []CrossRequirementPhase `json:"phases"`
	// Progress — 整体进展。
	Progress CrossRequirementProgress `json:"progress"`
	// Bases — 各参与仓库的基线台账（整体方案作业写）。
	//
	// 空数组 = 还没跑过整体方案（或跑的时候没能锁定任何基线）。
	Bases []CrossRequirementBase `json:"bases"`
}

// CrossRequirementBase — 某个参与仓库的基线台账行。
//
// 它回答的是「这份整体方案是对着哪个提交写的」：base_sha 是锁定的提交，
// worktree_path 是承载这份代码的隔离 worktree（detached HEAD）。
//
// Degraded = true 表示这个仓库没能锁定基线（非 git 仓库 / 没有 origin /
// 仓库还没有提交）—— 此时方案读的是共享检出，可能不是最新内容。
type CrossRequirementBase struct {
	ProjectID string `json:"project_id"`
	// BaseBranch — 该参与项目这次锁定的基础分支。
	BaseBranch string `json:"base_branch"`
	// BaseSHA — 锁定的提交（40 hex）。空 = 未锁定。
	BaseSHA string `json:"base_sha"`
	// WorktreePath — 隔离 worktree 的绝对路径。
	WorktreePath string `json:"worktree_path"`
	// Degraded — true = 没能锁定基线，读的是共享检出。
	Degraded bool `json:"degraded"`
}

// CrossRequirementDependency — 一条跨项目依赖边。
type CrossRequirementDependency struct {
	// RequirementID — 依赖方（后置子需求）。
	RequirementID string `json:"requirement_id"`
	// DependsOnID — 被依赖方（前置子需求）。
	DependsOnID string `json:"depends_on_id"`
	// Titles — 两端的标题（读路径 JOIN 出来，便于 UI 直接展示）。
	RequirementTitle string `json:"requirement_title"`
	DependsOnTitle   string `json:"depends_on_title"`
	// CrossProject — 两端是否属于不同项目（跨项目依赖的标记）。
	CrossProject bool `json:"cross_project"`
	// Met — 前置是否已完成（false ⇒ 该子需求被闸门拦住）。
	Met bool `json:"met"`
}

// ── 请求 DTO ────────────────────────────────────────────────────────────────

// CreateCrossRequirementReq — POST /api/cross-requirements 的请求体。
type CreateCrossRequirementReq struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Priority    string `json:"priority"`
	Kind        string `json:"kind"`
	// AcceptanceCriteria — 验收标准（字符串数组，落库为 JSON）。
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	// ProjectIDs — 参与项目 id 列表（创建时一并写入映射）。
	//
	// 与 Projects 二选一：只给 id 时用这个（基础分支留空 = 沿用项目默认分支）；
	// 需要逐项目指定基础分支时给 Projects。
	ProjectIDs []string `json:"project_ids"`
	// Projects — 参与项目的完整描述（id + 角色 + 基础分支）。
	//
	// 非空时优先于 ProjectIDs。
	Projects []CrossRequirementProjectInput `json:"projects"`
	// Draft — 草稿行标志。
	//
	// 面板式创建流程（与「新建需求」同构）需要在用户填完之前就有一条
	// **可写入**的行：插图 / 附件必须有一个已存在的宿主行才能落库。
	// Draft=true 时允许标题为空（落库用占位标题）与零参与项目，提交时
	// 由前端补齐两者。
	//
	// 「至少一个项目」这条不变量的真正落点**不在创建**，而在开始设计 /
	// 开始拆分 —— 那两个动作没有参与项目就没有工作区可跑（见
	// handler/cross_requirement_jobs.go 的两处 len(projects)==0 守卫）。
	// 所以放开创建这一处不会让一个没有项目的跨项目需求进入设计流程。
	Draft bool `json:"draft"`
}

// UpdateCrossRequirementReq — PUT /api/cross-requirements/{id} 的请求体。
type UpdateCrossRequirementReq struct {
	Title string `json:"title"`
	// Description — nil = 保持原值；非 nil = 覆盖（"" = 清空）。
	//
	// 指针而不是普通字符串，因为面板式编辑必须能区分「没传这个字段」与
	// 「把这个字段清空」：普通字符串下两者都是 ""，用户**删不掉**描述。
	// Title / Priority / Kind 保持原有的「空 = 不改」语义（标题本来就不
	// 允许为空，优先级 / 类型空值一律回落到存量值）。
	Description        *string  `json:"description"`
	Priority           string   `json:"priority"`
	Kind               string   `json:"kind"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	// Status — 可选：手动推进 / 回退生命周期状态（归档等）。
	Status string `json:"status"`
}

// CrossRequirementProjectInput — PUT .../projects 的一个参与项目。
type CrossRequirementProjectInput struct {
	ProjectID string `json:"project_id"`
	Role      string `json:"role"`
	// BaseBranch — 该参与项目的基础分支（子需求从这条分支开发）。
	//
	// 空串 = 不指定 → 子需求沿用该项目的默认分支（projects.default_branch）。
	BaseBranch string `json:"base_branch"`
}

// SetCrossRequirementProjectsReq — PUT /api/cross-requirements/{id}/projects。
type SetCrossRequirementProjectsReq struct {
	Projects []CrossRequirementProjectInput `json:"projects"`
}

// SaveCrossRequirementDesignReq — PUT /api/cross-requirements/{id}/design。
//
// DesignDocs 是 Markdown 正文（不是 JSON 数组）—— 整体方案是**一份**文档，
// 而不是一个文档数组，所以这里直接存 Markdown，落库时包成 JSON 数组以保持
// 与 requirements.design_docs 同形。
type SaveCrossRequirementDesignReq struct {
	DesignDocs string `json:"design_docs"`
}

// CrossRequirementSplitCandidate — 一条待确认的子需求候选。
type CrossRequirementSplitCandidate struct {
	// Title — 子需求标题。
	Title string `json:"title"`
	// Description — 自包含正文。
	Description string `json:"description"`
	// Scope — 涉及范围。
	Scope []string `json:"scope"`
	// Acceptance — 验收标准。
	Acceptance []string `json:"acceptance"`
	// Boundary — 与其它子需求的边界 / 接口契约。
	Boundary string `json:"boundary"`
	// Project — AI 建议的项目名（仅供 UI 回显模型的建议值）。
	Project string `json:"project"`
	// Order — 展示顺序（1 起算）。
	Order int `json:"order"`
	// ProjectID — **建议的**项目归属。服务端会以参与项目集合校验它；非法
	// 则整批拒绝。
	ProjectID string `json:"project_id"`
	// DependsOn — 前置依赖（批次内序号，1 起算）。
	DependsOn []int `json:"depends_on"`
}

// ConfirmCrossRequirementSplitReq — POST .../split/confirm 的请求体。
type ConfirmCrossRequirementSplitReq struct {
	Candidates []CrossRequirementSplitCandidate `json:"candidates"`
}

// CrossRequirementPhaseReq — POST /api/cross-requirements/{id}/phases。
type CrossRequirementPhaseReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// RequirementIDs — 阶段包含的子需求 id。
	RequirementIDs []string `json:"requirement_ids"`
}

// UpdateCrossRequirementPhaseReq — PUT .../phases/{pid}。
type UpdateCrossRequirementPhaseReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// RequirementIDs — 替换阶段成员（nil = 不改）。
	RequirementIDs []string `json:"requirement_ids"`
	// Status — 人工结论（passed / failed / skipped）。空 = 不改。
	Status string `json:"status"`
	// VerifyReport — 手工编辑验证报告。空 = 不改。
	VerifyReport string `json:"verify_report"`
}
