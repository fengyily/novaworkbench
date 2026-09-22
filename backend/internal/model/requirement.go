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
	CodingJobID         string `json:"coding_job_id"`         // active start-coding / adjust-coding / continue-coding JobStore job id; empty when no coding job is running
	LastCodingJobID     string `json:"last_coding_job_id"`    // most recent coding job id, DURABLE (never cleared on terminal) so the detail page can replay the finished job's log after a restart via /api/wizard/jobs/{id} + job_logs fallback
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
	// Effective Claude-config (claude_configs.id) the user selected for the
	// architect-design / developer stage, persisted on the success path so the
	// config dropdown and the model both re-hydrate on refresh, and adjust /
	// continue runs fall back to the requirement's own config rather than the
	// global active one. Empty = stage not yet run (or predates this column).
	ArchitectConfigID string `json:"architect_config_id"`
	DeveloperConfigID string `json:"developer_config_id"`
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
	// CodingStepPlan: the plan-mode implementation-steps Markdown produced by
	// the planner persona at the head of the SPLIT coding path. It is the
	// INPUT the decomposition step parses into sub_tasks rows — the mirror
	// image of CodingPlan, which is the summary OUTPUT written once every
	// child has finished. Empty = the requirement never took the split path
	// (or predates it).
	CodingStepPlan string `json:"coding_step_plan"`
	// CodingPhase is the coding stage's sub-state: "" (idle) /
	// CodingPhasePlanning / CodingPhaseDecomposing. It drives the detail
	// page's progress hint on a fresh page load (before the SSE stream
	// reconnects) and doubles as start-coding's re-entry lock.
	CodingPhase string `json:"coding_phase"`
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
	// DesignBaseSHA is the 40-char HEAD SHA of origin/<default_branch> captured
	// at the moment the architect-design stage synced to it. Written by the
	// design-stage prologue (right after the hard sync succeeds, BEFORE claude
	// is spawned) so a failed claude run still records which baseline the agent
	// was about to design against. The merge stage surfaces it in the PR body
	// for traceability. The coding stage deliberately does NOT inherit this
	// value — by the time StartCoding runs, origin/<base> may have advanced and
	// coding should branch from the fresh upstream, not from the design-time
	// snapshot. Empty for legacy rows and requirements whose design stage was
	// skipped (no remote / non-git / unborn HEAD — Skipped branch).
	DesignBaseSHA string `json:"design_base_sha"`
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
	// Plan-analysis timing (requirements columns). AnalysisStartedAt is stamped
	// on entry into "analyzing" (reset on re-entry); AnalysisEndedAt on reaching
	// "designed". Both nullable; NULL for legacy rows / stages not yet reached.
	AnalysisStartedAt *time.Time `json:"analysis_started_at,omitempty"`
	AnalysisEndedAt   *time.Time `json:"analysis_ended_at,omitempty"`
	// Development-stage span — derived aggregates, NOT stored columns. Populated
	// by RequirementService.Get from MIN(sub_tasks.created_at) /
	// MAX(sub_tasks.completed_at): the first sub-task's creation and the last
	// finished sub-task's completion ("以最后一个任务结束时间为准"). Both NULL when
	// the requirement has no sub_tasks (e.g. legacy single-shot coding runs).
	DevStartedAt *time.Time `json:"dev_started_at,omitempty"`
	DevEndedAt   *time.Time `json:"dev_ended_at,omitempty"`
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
	// LaunchMode / LaunchScheduleID / LaunchError are display-only (NOT
	// requirements columns), populated ONLY by Create when the request
	// carried a launch spec. They let the three creation entry points
	// (ProjectDetail / RequirementsList / RequirementsCalendar) route the
	// post-create navigation without a second round-trip.
	LaunchMode       string `json:"launch_mode,omitempty"`        // "immediate" | "scheduled"
	LaunchScheduleID string `json:"launch_schedule_id,omitempty"` // scheduled_tasks.id
	LaunchError      string `json:"launch_error,omitempty"`       // create succeeded but dispatch failed
	// Tags is the JSON-array string of user-supplied short labels attached
	// to this requirement: free-form short strings such as "阻塞", "外部依赖",
	// "v2", "客户A". Always a valid JSON array string (empty "[]" when no
	// tags); trimmed, deduped, length- and count-capped by
	// RequirementService.UpdateTags. Empty in legacy rows before the column
	// existed (default value is "[]" so the frontend never sees null).
	Tags string `json:"tags"`
	// Marks is the JSON-array string of preset semantic tags attached to
	// this requirement. Preset values are drawn from the service-layer
	// MarkWhitelist (currently "important" / "follow_up" / "blocked" /
	// "at_risk"); unknown values are silently dropped at the service layer
	// so the column never holds an unrecognized code. Empty array is
	// written as "[]" so legacy rows render as "no marks" without a
	// separate backfill. Affects list sort order — rows with any mark float
	// above unmarked rows of the same done/active status. Maximum 5 marks
	// per requirement (maxMarksCount in RequirementService).
	Marks string `json:"marks"`
	// ClosedAt is stamped when the user force-closes a requirement via the
	// "关闭需求" action (service.RequirementService.Close). NULL for natural
	// completion (developer-complete gate flips status to "done" without
	// writing this column). Combined with ClosedReason, lets the UI
	// distinguish "开发完成" from "中途关闭". Both columns live on the
	// requirements row itself so they survive JobStore eviction / server
	// restart.
	ClosedAt *time.Time `json:"closed_at,omitempty"`
	// ClosedReason is the user-supplied rationale when the requirement was
	// force-closed (e.g. "重复需求", "本期不做"). Empty string when the
	// requirement was never force-closed or when the user left the reason
	// blank. Free-form text — the service does not enforce a vocabulary so
	// the UI can show whatever the user typed (truncated in lists).
	ClosedReason string `json:"closed_reason"`
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

	// Launch: optional execution plan attached at creation time. When set,
	// the handler dispatches the run synchronously (Mode=="immediate") or
	// inserts a scheduled_tasks row (Mode=="scheduled") as part of the same
	// Create call, and the response carries display-only LaunchMode /
	// LaunchScheduleID / LaunchError fields so the frontend can route the
	// user straight to the detail page. **When Launch is nil, Create behaves
	// exactly as before** — no dispatch, no scheduling, response shape
	// unchanged.
	Launch *LaunchSpec `json:"launch,omitempty"`
}

type UpdateStatusReq struct {
	Status string `json:"status"`
}

// LaunchSpec carries the optional execution plan attached at requirement
// creation time. When Create decodes a non-nil Launch it dispatches the run
// (immediate) or schedules it (scheduled) on the same request — see
// handler.requirement_launch.go for the flow → taskType table. When Launch
// is nil, Create behaves exactly as before (no dispatch, no scheduling).
//
// JSON tag names mirror the wire contracts of designCodingImmediateReq
// (handler/wizard_immediate.go) and createScheduleReq (handler/schedule.go)
// verbatim, so the launch-spec body can be translated into either request
// type without any field remapping. Required fields per mode:
//
//	immediate / design_and_coding: design_model + design_claude_config_id
//	immediate / coding:            coding_model + coding_claude_config_id
//	scheduled (any task_type):     schedule.recurrence + (schedule.run_at
//	                                for "once"; schedule.recur_time [+
//	                                schedule.recur_days for "weekly"])
type LaunchSpec struct {
	// Mode selects the dispatch path. "immediate" runs the task as soon as
	// Create resolves (a JobStore job is minted and the SSE stream attaches
	// from /api/wizard/jobs/{id}/stream). "scheduled" inserts a row into
	// scheduled_tasks; the scheduler tick picks it up at Schedule.RunAt.
	Mode string `json:"mode"` // "immediate" | "scheduled"

	// Per-stage model + Claude-config + Agent-server overrides. Both stages
	// are always present in the struct — the dispatch path picks the
	// relevant subset by taskType — so the JSON body is uniform regardless
	// of which mode the user picked on the form.
	DesignModel         string `json:"design_model,omitempty"`
	DesignClaudeConfigID string `json:"design_claude_config_id,omitempty"`
	DesignAgentServerID string `json:"design_agent_server_id,omitempty"`
	CodingModel         string `json:"coding_model,omitempty"`
	CodingClaudeConfigID string `json:"coding_claude_config_id,omitempty"`
	CodingAgentServerID  string `json:"coding_agent_server_id,omitempty"`

	// Shared options — apply to whichever stage the dispatch picks.
	ReadKnowledge bool   `json:"read_knowledge"`
	BranchName    string `json:"branch_name,omitempty"`
	BaseBranch    string `json:"base_branch,omitempty"`
	SplitTasks    bool   `json:"split_tasks"`
	// AutoPushPR is *bool so an omitted key means "use the row's persisted
	// value" (mirrors codingRunParams.AutoPushPR semantics). Defaults in
	// dispatch: false for *bool — callers that want the project default
	// must leave it nil.
	AutoPushPR *bool  `json:"auto_push_pr,omitempty"`
	DevMode    string `json:"dev_mode,omitempty"` // "session" | "design"
	SyncMode   string `json:"sync_mode,omitempty"`

	// Schedule is the schedule-only sub-spec. Only consumed when Mode ==
	// "scheduled". Ignored (and may be nil) for immediate dispatches so the
	// same LaunchSpec struct can be reused on the immediate form without
	// re-shaping the payload.
	Schedule *LaunchScheduleSpec `json:"schedule,omitempty"`
}

// LaunchScheduleSpec is the schedule block of LaunchSpec. Field semantics
// mirror createScheduleReq (handler/schedule.go) verbatim — in particular
// Recurrence defaults to "once" when empty, and RunAt is only required for
// "once" (daily/weekly derive the next fire time from RecurTime +
// RecurDays at scheduler tick time). RecurDays is a CSV of weekday numbers
// 0-6 (0=Sunday) and is only meaningful for "weekly" — see
// schedule_executor.NextRunAt for the canonical implementation.
type LaunchScheduleSpec struct {
	Recurrence string `json:"recurrence,omitempty"` // "once" | "daily" | "weekly"
	RunAt      string `json:"run_at,omitempty"`      // RFC3339 or "YYYY-MM-DDTHH:MM" (once only)
	RecurTime  string `json:"recur_time,omitempty"`  // "HH:MM" (daily/weekly)
	RecurDays  string `json:"recur_days,omitempty"`  // CSV 0-6 (weekly only)
	RecurTZ    string `json:"recur_tz,omitempty"`    // IANA tz name
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
