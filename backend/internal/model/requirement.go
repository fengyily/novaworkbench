package model

import "time"

type Requirement struct {
	ID                 string `json:"id"`
	ProjectID          string `json:"project_id"`
	Title              string `json:"title"`
	Description        string `json:"description"`
	Status             string `json:"status"`
	Priority           string `json:"priority"`
	Kind               string `json:"kind"`                // "issue" | "requirement" | "idea"; defaults to "requirement" for legacy rows
	AcceptanceCriteria string `json:"acceptance_criteria"` // JSON array
	DesignDocs         string `json:"design_docs"`         // JSON array
	ConversationIDs    string `json:"conversation_ids"`    // JSON array
	AssignedTo         string `json:"assigned_to"`
	CreatedBy          string `json:"created_by"`
	AnalysisSessionID  string `json:"analysis_session_id"`
	// SourceRequirementID links this row to the requirement it was promoted
	// from — typically an idea whose discussion was summarized into a brand-
	// new requirement by the "总结转需求" action. Empty for rows that were
	// created directly (no parent) or that predate this column. The original
	// row is left untouched so the discussion thread stays intact.
	SourceRequirementID string `json:"source_requirement_id"`
	DesignSessionID     string `json:"design_session_id"`
	DesignJobID         string `json:"design_job_id"`   // active architect-design JobStore job id; empty when no design job is running
	AnalysisJobID       string `json:"analysis_job_id"` // active analyst-chat JobStore job id; empty when no analyst turn is running
	ApplyJobID          string `json:"apply_job_id"`    // active apply-doc JobStore job id; empty when no apply is running
	CodingSessionID     string `json:"coding_session_id"`
	SkipAnalysis        bool   `json:"skip_analysis"` // when true, architect-design runs a fresh session instead of forking the analyst session
	SkipDesign          bool   `json:"skip_design"`   // when true, skip analyst+architect stages and go straight to coding ("直接开发")
	BranchName          string `json:"branch_name"`   // dev branch checked out in the worktree; empty = legacy in-place checkout
	WorktreePath        string `json:"worktree_path"` // absolute path of the isolated git worktree; empty = no worktree (legacy)
	// Effective model actually dispatched to the claude CLI for each stage
	// (the --model value, or the display literal "默认模型" when neither the
	// role nor the active claude config specified one). Written only on the
	// success path; empty = the stage hasn't run yet (or predates this column).
	AnalystModel   string `json:"analyst_model"`
	ArchitectModel string `json:"architect_model"`
	DeveloperModel string `json:"developer_model"`
	ReviewerModel  string `json:"reviewer_model"`
	// Context compression artifacts, one set per wizard stage. When the user
	// clicks "📦 压缩上下文", the wizard handler runs a one-off --resume turn
	// asking Claude to summarize the current session, stores the Chinese
	// summary here, stamps compressed_at with NOW(), and clears the matching
	// *_session_id so the next turn starts a fresh session with the summary
	// prepended to its first prompt. Pre-populated summaries let the user see
	// "已压缩" badges across refreshes without re-fetching the CLI.
	AnalystContextSummary string     `json:"analyst_context_summary"`
	AnalystCompressedAt   *time.Time `json:"analyst_compressed_at,omitempty"`
	DesignContextSummary  string     `json:"design_context_summary"`
	DesignCompressedAt    *time.Time `json:"design_compressed_at,omitempty"`
	CodingContextSummary  string     `json:"coding_context_summary"`
	CodingCompressedAt    *time.Time `json:"coding_compressed_at,omitempty"`
	// UsageSnapshots is a JSON blob keyed by wizard session
	// (analyst_chat/architect_design/coding) carrying each session's most
	// recent result-event token counts + context_window + model. Written by
	// runClaudeStream at the same point it emits the `usage` SSE event, so the
	// frontend can seed its usage bars from the Requirement GET and survive a
	// page refresh / panel collapse instead of dropping to 0%. Empty = no
	// snapshot recorded yet. Best-effort: write failures are logged, not
	// surfaced, so they never break a claude turn.
	UsageSnapshots string `json:"usage_snapshots"`
	// CodingPlan: the auto-orchestrate summary Markdown produced by the
	// developer main agent after every child sub-task in a batch has
	// finished. Empty = no orchestrated batch has produced a summary yet
	// (legacy rows / manual-only flow). Surfaced by SubTaskPanel as the
	// parent plan every child task forks from; persisted on completion so
	// a server restart / JobStore eviction doesn't lose the breakdown.
	CodingPlan string `json:"coding_plan"`
	// DevSource / AgentServerID record WHERE this requirement was developed.
	// DevSource is DevSourceAgent ("agent") or DevSourceLocal ("local"),
	// stamped once when the coding stage starts; empty = never coded.
	// AgentServerID is the agent_servers row id when DevSource == "agent"
	// (empty otherwise). Every follow-up action that mutates the working tree
	// (push+PR, worktree cleanup, sub-task dispatch) reads AgentServerID and
	// routes itself to that same server, so execution stays consistent with
	// the environment the code actually lives in.
	DevSource     string `json:"dev_source"`
	AgentServerID string `json:"agent_server_id"`
	// SyncMode records HOW code is shipped to / from the Agent server for this
	// requirement. "" == 远程/origin 传输 (legacy default: remote host clones
	// origin and pushes back to origin); "local" == git-bundle 传输 for local
	// self-hosted repos with no reachable remote (code rides SFTP bundle files
	// and is integrated locally via 本地合并). Stamped once by the coding stage
	// prologue and read by every follow-up action so the lifecycle stays
	// consistent. Empty for local (non-agent) execution and legacy rows.
	SyncMode string `json:"sync_mode"`
	// AutoPush controls whether the coding stage, once development finishes,
	// automatically dispatches the "提交 → 推送 → 创建 PR" sub-task (the same
	// child agent MergeHandler.Push triggers manually). Defaults to true so
	// every start-coding run ships its result by default; the user can turn it
	// off per-requirement from the start-coding preflight dialog for
	// exploratory runs. Stamped by the start-coding prologue and read at the
	// three development completion points (local non-split, Agent-server, and
	// the split orchestrator summary).
	AutoPush bool `json:"auto_push"`
	// DesignAgentServerID records WHERE this requirement's architect-design
	// stage was run. Stamped by the design stage prologue (mirror of
	// AgentServerID for the dev stage). Empty = 本地 (no remote agent). On a
	// re-run of the design stage with no override this stays empty so the
	// persisted badge accurately reflects what the user last chose — and
	// follow-up "继续设计" actions don't accidentally inherit a stale server
	// binding that the user has since cleared.
	DesignAgentServerID string `json:"design_agent_server_id"`
	// DesignAgentServerName is a display-only join of agent_servers.name for
	// DesignAgentServerID (NOT a requirements column). Populated by
	// RequirementService.List/Get so the design toolbar can render "🛰️ Agent
	// Server「<名称>」" without a second round-trip. Empty for local design runs
	// or when the server row was deleted. omitempty keeps Create responses clean.
	DesignAgentServerName string `json:"design_agent_server_name,omitempty"`
	// DevMode records HOW the coding stage was launched: "session" forks the
	// design session (legacy default — Claude inherits the full analysis+design
	// conversation), "design" starts a fresh session and hands the stored
	// design doc to the agent via the -p prompt. Stamped once when the coding
	// stage starts so the UI can show "本次开发基于会话/方案" and a follow-up
	// StartCoding that omits the field can default to the persisted value.
	// Empty = never coded / predates this column.
	DevMode string `json:"dev_mode"`
	// AgentServerName is a display-only join of agent_servers.name; it is NOT
	// a requirements column. Populated by RequirementService.List/Get so the
	// requirement list and detail pages can render "Agent Server 开发 · <名称>"
	// without a second round-trip. Empty when the server row was deleted.
	// omitempty keeps the Create-response JSON clean (Create does not join).
	AgentServerName string `json:"agent_server_name,omitempty"`
	// SubTaskCount is the number of sub_tasks rows linked to this requirement.
	// Populated by RequirementService.Get via a SELECT COUNT(*); used by the
	// frontend to decide whether to hide the requirement-level "追加调整"
	// entry (when the requirement has been decomposed into sub-tasks, all
	// further adjustments must go through the sub-task flow instead). Not
	// stored on the requirements row itself — it's a derived aggregate so
	// the count stays in sync without an extra migration.
	SubTaskCount int        `json:"sub_task_count"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	// Calendar-view scheduling fields. Both nullable: NULL = "未排期", the
	// frontend falls back to created_at so legacy rows are immediately usable
	// in the calendar without a backfill. Writes go through
	// RequirementService.UpdateSchedule (validates end >= start). Stored as
	// DATETIME — same as created_at — and serialized as RFC3339 over JSON so
	// the frontend can `new Date(...)` them directly.
	PlannedStartAt *time.Time `json:"planned_start_at,omitempty"`
	PlannedEndAt   *time.Time `json:"planned_end_at,omitempty"`
	// ScheduledRunAt is the earliest run_at of a pending scheduled_tasks row
	// pointing at this requirement. NOT a requirements column — populated by
	// RequirementService.attachScheduledRunAt for the Calendar endpoint so
	// the frontend can render the clock icon ("🕐") next to requirements that
	// have a future-dated wizard task waiting. Empty when no pending schedule
	// exists (and on Create, which does not join).
	ScheduledRunAt *time.Time `json:"scheduled_run_at,omitempty"`
}

type CreateRequirementReq struct {
	ProjectID    string `json:"project_id"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	Priority     string `json:"priority"`
	Kind         string `json:"kind"`          // "issue" | "requirement" | "idea"; empty → defaults to "requirement"
	SkipAnalysis *bool  `json:"skip_analysis"` // pointer: nil omits the field so Create defaults to true (skip) and Update preserves the existing value
	SkipDesign   *bool  `json:"skip_design"`   // pointer: nil → Create defaults to false; Update never references this column so it is preserved automatically
	// SkipOrganize: when true, the handler skips the LLM-organized description
	// pass that normally distills a title + structured Markdown body. Raw
	// description is stored verbatim and a fallback title (first line, capped)
	// is used. Pointer so the absence of the field keeps the previous default
	// (false = run the organizer) — older clients / scripts that don't send
	// this continue to get the structured output. UI default = true.
	SkipOrganize *bool `json:"skip_organize"`
	// SourceRequirementID: optional parent reference. Set by the "总结转需求"
	// action when an idea's discussion is summarized into a new requirement.
	// Validated in service.RequirementService (must point to an existing row in
	// the same project).
	SourceRequirementID string `json:"source_requirement_id"`
}

type UpdateStatusReq struct {
	Status string `json:"status"`
}

// UpdateScheduleReq is the body for PATCH /api/requirements/{id}/schedule.
// Both fields are *time.Time (not time.Time) so JSON omitempty + an explicit
// null lets the caller clear one or both fields (e.g. "reset to created_at
// anchor"). Validation (end >= start, end not before 1970) lives in the
// service layer.
type UpdateScheduleReq struct {
	PlannedStartAt *time.Time `json:"planned_start_at"`
	PlannedEndAt   *time.Time `json:"planned_end_at"`
}

type AnalysisResult struct {
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	TechnicalRisks     []string `json:"technical_risks"`
	RelatedModules     []string `json:"related_modules"`
	Summary            string   `json:"summary"`
}

type TechnDesign struct {
	Overview     string   `json:"overview"`
	Files        []string `json:"files"`
	Steps        []string `json:"steps"`
	ModelChanges string   `json:"model_changes"`
	Risks        []string `json:"risks"`
}
