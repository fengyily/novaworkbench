package model

import "time"

// ── 跨项目需求 · 方案调整对话 ──────────────────────────────────────────────
//
// 整体方案（cross_requirements.design_docs）是跨仓库的一次性产物：跑完方案
// 作业就定稿了。之后如果方案里留着「待决策事项」（接口超时取多少、幂等键用哪
// 种策略……），用户需要跟 AI 就这些点谈拢，谈完再重跑一次方案作业把结论吃
// 进去。
//
// 本模型描述的正是那段对话。形状与 RequirementQATurn 完全一致（一行一问一
// 答），差别只有两点：
//
//  1. owner 是跨项目需求而不是需求；
//  2. **没有 stage 列** —— 一个跨项目需求只有一条调整线程（调整的是整体方
//     案，不存在「设计阶段 / 开发阶段」之分）。
//
// 终态常量复用 QAStatusRunning / QAStatusDone / QAStatusError —— 同一个「有
// 没有答案」的语义，不另造词汇。

// CrossRequirementQATurn is one question-and-answer turn of a cross
// requirement's design-adjustment thread.
type CrossRequirementQATurn struct {
	ID string `json:"id"`
	// CrossRequirementID is the owning cross requirement (xreq_*).
	CrossRequirementID string `json:"cross_requirement_id"`
	// Question is the user's message; Answer is the model's reply.
	Question string `json:"question"`
	Answer   string `json:"answer"`
	// Status is one of QAStatusRunning / QAStatusDone / QAStatusError.
	Status string `json:"status"`
	// JobID is the JobStore handle for this turn's run — the frontend
	// re-subscribes to the SSE stream with it after a page refresh while the
	// turn is still running.
	JobID string `json:"job_id"`
	// Model is the effective model the turn ran with.
	Model string `json:"model"`
	// SessionID is the session this turn actually ran in: the pre-minted id
	// for a forked / fresh first turn, the previous turn's session for a
	// resumed one.
	SessionID string `json:"session_id"`
	// SourceSessionID is the snapshot of the **overall-design session** this
	// thread forked from. Empty on a degraded (fresh) turn.
	//
	// It is what detects "the design was regenerated since this thread
	// started": the design session id changed, so the thread's context no
	// longer describes the plan on file and it must fork again.
	SourceSessionID string `json:"source_session_id"`
	// Resumed is true when the turn continued the thread's own session (the
	// model still holds the forked context). False means the session was gone
	// and the answer came from a fresh run seeded with the current design +
	// the earlier turns.
	Resumed bool `json:"resumed"`
	// ErrorKind classifies a FAILED turn (empty-result / stale-session /
	// cli-crash / timeout). Empty on success.
	ErrorKind string `json:"error_kind"`
	// CreatedBy is the user id that asked the question.
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// CrossPendingDecision — one open question the overall design leaves for the
// human to settle.
//
// 它是从整体方案 Markdown 的「待决策事项」章节解析出来的（见
// prompt.ExtractCrossPendingDecisions），不是独立存储的事实：**方案改了，
// 这份列表就跟着变**（详情接口每次从 design_docs 现算）。
//
// Title is the short label ("接口超时时间"); Detail is the full line the
// model wrote (why it needs a human, what the options are).
type CrossPendingDecision struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
}
