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
	ResultMD     string     `json:"result_md"`
	FindingsJSON string     `json:"findings_json"`
	P0Count      int        `json:"p0_count"`
	P1Count      int        `json:"p1_count"`
	P3Count      int        `json:"p3_count"`
	GoalMet      string     `json:"goal_met"`
	Model        string     `json:"model"`
	CreatedBy    string     `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
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
