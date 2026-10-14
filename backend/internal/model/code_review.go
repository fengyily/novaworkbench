package model

import "time"

// Review session lifecycle. A row is created in StatusDraft so attachments
// have a stable owner BEFORE the run starts; StartReviewSession flips it to
// running, and the stream's tail writes the terminal status.
const (
	ReviewStatusDraft   = "draft"
	ReviewStatusRunning = "running"
	ReviewStatusDone    = "done"
	ReviewStatusError   = "error"
)

// Review type discriminator. Both kinds share one table so the history list
// is a single query; PR reviews keep their legacy entry point.
const (
	ReviewTypeBranch = "branch"
	ReviewTypePR     = "pr"
)

// CodeReview is one persisted Review session: the owner of its attachments,
// the carrier of the user-written description (the "改动目标" the reviewer
// checks against), and the landing spot for the finished report.
type CodeReview struct {
	ID          string `json:"id"`
	ProjectID   string `json:"project_id"`
	ReviewType  string `json:"review_type"`
	Branch      string `json:"branch"`
	BaseBranch  string `json:"base_branch"`
	PRNumber    int    `json:"pr_number"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
	JobID       string `json:"job_id"`
	// ResultMD is the Markdown report with the trailing JSON contract block
	// stripped; FindingsJSON is that block's `findings` array verbatim (empty
	// when the model didn't honour the contract — the UI then renders
	// ResultMD alone).
	ResultMD     string `json:"result_md"`
	FindingsJSON string `json:"findings_json"`
	P0Count      int    `json:"p0_count"`
	P1Count      int    `json:"p1_count"`
	P3Count      int    `json:"p3_count"`
	GoalMet      string `json:"goal_met"`
	// Model is the review's model binding: the user's pick from the Review
	// toolbar when one was made, overwritten with the EFFECTIVE model (role
	// override → config default → "默认模型" literal) once a run starts, so
	// the picker always shows what the last run actually used.
	Model string `json:"model"`
	// ClaudeConfigID is the claude_configs row the gateway sources auth +
	// base URL from. Empty = fall back to the reviewer role's binding, then
	// the global active config.
	ClaudeConfigID string `json:"claude_config_id"`
	// AgentServerID is the execution environment: empty = 本地, otherwise an
	// agent_servers.id the run is dispatched to over SSH.
	AgentServerID string `json:"agent_server_id"`
	// ClaudeSessionID is the stream session_id of the review's LAST run.
	// Follow-up questions resume it so the answer is grounded in the diff and
	// the report the model already produced. Empty = no session to resume
	// (old rows, or a run that never emitted an init event); the follow-up
	// then starts fresh with the report embedded in its prompt.
	ClaudeSessionID string     `json:"claude_session_id"`
	CreatedBy       string     `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
}

// ReviewFinding is one graded issue from the report's trailing JSON block.
// Severity is one of prompt.SevP0 / SevP1 / SevP3; anything else is dropped
// by the parser so the frontend's three-bucket grouping stays total.
type ReviewFinding struct {
	Severity   string `json:"severity"`
	File       string `json:"file"`
	Line       string `json:"line"`
	Title      string `json:"title"`
	Detail     string `json:"detail"`
	Suggestion string `json:"suggestion"`
}

// Follow-up (追问) lifecycle. A follow-up is created already `running` — it
// has no draft window, since the question is submitted and dispatched in one
// request.
const (
	FollowupStatusRunning = "running"
	FollowupStatusDone    = "done"
	FollowupStatusError   = "error"
)

// CodeReviewFollowup is one question-and-answer turn asked against a finished
// Review. The thread is persisted (rather than living in the memory-only
// JobStore) so reopening the review from history shows the whole discussion.
//
// A follow-up is strictly read-only with respect to the review: it never
// rewrites result_md, findings_json or the severity counts.
type CodeReviewFollowup struct {
	ID       string `json:"id"`
	ReviewID string `json:"review_id"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Status   string `json:"status"`
	JobID    string `json:"job_id"`
	Model    string `json:"model"`
	// Resumed is true when the turn continued the review's claude session.
	// False means the session was gone and the answer came from a fresh run
	// seeded with the stored report — still useful, but without the tool
	// history of the original review.
	Resumed    bool       `json:"resumed"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}
