package model

import "time"

// ScheduledTask is a one-shot, future-dated wizard action (architect-design or
// start-coding) that fires automatically at run_at against a specific
// requirement, with a pre-selected model. The scheduler (internal/scheduler)
// polls the table, atomically claims due rows, dispatches them through the
// wizard's exec path, and writes the terminal status / job_id back.
//
// Lifecycle (no automatic retry — failed tasks are surfaced to the user):
//
//	pending → running → succeeded | failed
//	pending → canceled                (user cancel via /api/schedules/{id}/cancel)
//	running → failed                  (server restart recovery on next boot)
//
// Fields stored redundantly (requirement_title / project_id) are denormalized
// for list rendering — avoids a JOIN on every row of /api/schedules and
// keeps the FK CASCADE behavior intact when the requirement is deleted.
type ScheduledTask struct {
	ID               string     `json:"id"`
	TaskType         string     `json:"task_type"`         // SchedTypeDesign | SchedTypeCoding
	RequirementID    string     `json:"requirement_id"`
	ProjectID        string     `json:"project_id"`
	RequirementTitle string     `json:"requirement_title"`
	RunAt            time.Time  `json:"run_at"`
	Model            string     `json:"model"`             // '' = 角色默认（执行时由 roleConfig 解析）
	ReadKnowledge    bool       `json:"read_knowledge"`
	BranchName       string     `json:"branch_name"`      // coding only
	BaseBranch       string     `json:"base_branch"`      // coding only
	AgentServerID    string     `json:"agent_server_id"`  // coding only; '' = 本地
	SplitTasks       bool       `json:"split_tasks"`      // coding only
	Status           string     `json:"status"`
	JobID            string     `json:"job_id"`
	ErrorMessage     string     `json:"error_message"`
	CreatedBy        string     `json:"created_by"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	ExecutedAt       *time.Time `json:"executed_at,omitempty"`
}

// ScheduledTask status / task_type constants. Plain strings to mirror the SQL
// column values and the frontend type union — same pattern as
// SubTaskStatusPending / etc.
const (
	SchedStatusPending   = "pending"
	SchedStatusRunning   = "running"
	SchedStatusSucceeded = "succeeded"
	SchedStatusFailed    = "failed"
	SchedStatusCanceled  = "canceled"

	SchedTypeDesign = "design"
	SchedTypeCoding = "coding"
)