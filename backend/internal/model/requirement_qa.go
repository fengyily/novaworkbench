package model

import "time"

// ── 阶段答疑对话 (Stage Q&A Chat) ──────────────────────────────────────────
//
// 需求详情页有两条"生产型" AI 会话:方案设计会话(architect,
// requirements.design_session_id)与开发会话(developer,
// requirements.coding_session_id)。用户想针对"方案为什么这样设计 / 这段实现是
// 什么意思"快速答疑时,现有的 refine-doc / developer-chat 入口都有副作用:
// 前者会铸新 id 写回 design_session_id,后者会续写或覆盖 coding_session_id,
// 把答疑内容混进生产主线。
//
// 本模型描述的是**第三条路径**:从阶段会话 fork 出一个独立的 QA 会话,在里面
// 多轮答疑。fork 继承原会话的全部上下文,答疑的任何一轮都不写回原会话 ——
// design_session_id / coding_session_id 及其 jsonl 转录零污染。
//
// 一行一问一答(turn)而不是整段转录:答疑由后端 job 化执行,答案必须由后端
// 落库(用户关掉浏览器答案不能丢),且 status / job_id / resumed 天然是 per-turn
// 字段。参见 service.RequirementQAService。

// QA 阶段常量。design=方案设计会话,coding=开发会话。
const (
	QAStageDesign = "design"
	QAStageCoding = "coding"
)

// 答疑轮的终态。running = 已派发但未落答案;done = 有答案;error = 失败(错误
// 文本写在 answer 里,前端照样能显示发生了什么,而不是一个空气泡)。
const (
	QAStatusRunning = "running"
	QAStatusDone    = "done"
	QAStatusError   = "error"
)

// RequirementQATurn is one question-and-answer turn of a stage Q&A thread.
//
// 会话元数据记在轮上而不是另设线程表:当前 QA 会话 = 最新一条 done 轮的
// SessionID;源会话重跑(重新设计 / 重新开发换了 id)由 SourceSessionID 失配
// 检出 → 自动重新 fork,旧问答保留展示。
type RequirementQATurn struct {
	ID            string `json:"id"`
	RequirementID string `json:"requirement_id"`
	// Stage is QAStageDesign or QAStageCoding — which production session this
	// thread is asking about.
	Stage string `json:"stage"`
	// Question is the user's message; Answer is the model's reply.
	Question string `json:"question"`
	Answer   string `json:"answer"`
	// Status is one of QAStatusRunning / QAStatusDone / QAStatusError.
	Status string `json:"status"`
	// JobID is the JobStore handle for this turn's run — it lets the frontend
	// re-subscribe to the SSE stream after a page refresh while the turn is
	// still running.
	JobID string `json:"job_id"`
	// Model is the effective model the turn ran with.
	Model string `json:"model"`
	// SessionID is the QA session this turn actually ran in. For a forked
	// first turn it is the pre-minted id handed to the CLI via --session-id;
	// for a resumed turn it is the previous turn's session; for a degraded
	// (fresh) turn it is a newly minted id.
	SessionID string `json:"session_id"`
	// SourceSessionID is the snapshot of the stage session this thread forked
	// from. Empty on a degraded (fresh) turn. A mismatch against the
	// requirement's current stage session means the user re-ran the stage and
	// the thread must fork again from the new session.
	SourceSessionID string `json:"source_session_id"`
	// Resumed is true when the turn continued an existing QA session (the
	// model still holds the forked context). False means the session was gone
	// and the answer came from a fresh run seeded with the design doc + the
	// earlier Q&A — still useful, but without the original tool history.
	Resumed bool `json:"resumed"`
	// CreatedBy is the user id that asked the question.
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}
