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
	ID                  string     `json:"id"`
	TaskType            string     `json:"task_type"`         // SchedTypeDesign | SchedTypeCoding | SchedTypeDesignCoding
	RequirementID       string     `json:"requirement_id"`
	ProjectID           string     `json:"project_id"`
	RequirementTitle    string     `json:"requirement_title"`
	RunAt               time.Time  `json:"run_at"`
	Model               string     `json:"model"`             // design: 方案模型；design_and_coding: 设计阶段模型；'' = 角色默认
	ReadKnowledge       bool       `json:"read_knowledge"`
	BranchName          string     `json:"branch_name"`      // coding / design_and_coding
	BaseBranch          string     `json:"base_branch"`      // coding / design_and_coding
	AgentServerID       string     `json:"agent_server_id"`  // design / coding / design_and_coding（设计阶段执行环境）；'' = 本地
	SplitTasks          bool       `json:"split_tasks"`      // coding / design_and_coding
	// CodingModel / CodingAgentServerID are only used by the merged
	// design_and_coding task type — they configure the second (developer)
	// stage independently from the design-stage fields above. Single-stage
	// design / coding tasks leave both empty so existing rows stay backward
	// compatible with the pre-merge schema.
	CodingModel         string     `json:"coding_model"`
	CodingAgentServerID string     `json:"coding_agent_server_id"`
	Status              string     `json:"status"`
	JobID               string     `json:"job_id"`
	ErrorMessage        string     `json:"error_message"`
	CreatedBy           string     `json:"created_by"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	ExecutedAt          *time.Time `json:"executed_at,omitempty"`
	// RequirementStatus is the LIVE status of the linked requirement at the
	// moment of query — populated by LEFT JOIN `requirements` so the
	// /schedules list can render a status chip next to the requirement
	// title (req_30080193f1c95255 post-mortem: a `succeeded` schedule with
	// the requirement still showing "方案完成" confused the user into
	// thinking development hadn't started). Empty when the linked
	// requirement is deleted (FK CASCADE) — the frontend treats '' as a
	// hidden chip rather than rendering "📝 ?".
	RequirementStatus   string     `json:"requirement_status,omitempty"`
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

	SchedTypeDesign        = "design"
	SchedTypeCoding        = "coding"
	SchedTypeDesignCoding  = "design_and_coding"
)