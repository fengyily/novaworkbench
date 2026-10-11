package model

import "time"

// IssueMonitor is the per-project row that drives the "auto-import Issue"
// feature (v0.5.x). One row per project at most — the handler enforces
// uniqueness on PUT (404 on GET /api/projects/{id}/issue-monitor when no
// row exists; the service is "upsert" semantics on the project_id column).
//
// Lifecycle:
//
//	created (PUT /api/projects/{id}/issue-monitor)
//	  → active=1: scheduler.IssueMonitorQueue.tick polls every
//	              NOVA_ISSUE_MONITOR_INTERVAL and, for each monitor whose
//	              `time.Since(last_run_at) >= interval_minutes`, dispatches
//	              service.IssueMonitorService.RunOne
//	  → active=0: ticker ignores the row on every subsequent tick
//
// `last_*` columns follow the same pattern as scheduled_tasks so the UI
// can render "上次运行 N 分钟前 · 成功 / 失败" without extra round-trips.
//
// Dispatch shape (镜像创建需求的「启动计划」): LaunchMode × SkipDesign ×
// AutoPush 三个正交开关，加上按阶段拆分的模型 / Claude 配置 / 执行环境。
//
//	LaunchMode  = create_only → 只建需求，不派发
//	            = immediate   → 立即走 design+coding
//	            = scheduled   → 落到时间窗 Recurrence+RecurTime+RecurDays
//	SkipDesign  = true  → 直接开发（coding only）
//	            = false → 方案开发（design + coding）
//	AutoPush    = 开发完成后是否自动 commit / push / PR
type IssueMonitor struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Label     string `json:"label"` // GitHub Label string; "" = monitor disabled (the handler refuses non-empty monitors without a label)

	// --- 执行方式 ---
	LaunchMode string `json:"launch_mode"` // LaunchMode* constants below

	// --- 开发流程 ---
	SkipDesign bool `json:"skip_design"` // false = 方案开发 (design+coding), true = 直接开发 (coding only)

	// --- 阶段配置（设计阶段）---
	DesignModel          string `json:"design_model"`            // "" = 该阶段角色默认
	DesignClaudeConfigID string `json:"design_claude_config_id"` // claude_configs.id; "" = 全局默认配置
	DesignAgentServerID  string `json:"design_agent_server_id"`  // "" = 本地执行

	// --- 阶段配置（开发阶段）---
	CodingModel          string `json:"coding_model"`
	CodingClaudeConfigID string `json:"coding_claude_config_id"`
	CodingAgentServerID  string `json:"coding_agent_server_id"` // "" = 本地执行

	// --- 时间窗（仅 LaunchModeScheduled 有意义）---
	//
	// 存的是「时间窗定义」而不是直接透传给 scheduled_tasks 的 recurrence：
	// 派发时用 service.NextRunAt 算出下一个具体时刻，落一条 recurrence='once'
	// 的定时任务。理由见 service/issue_monitor.go 的 buildLaunchSpec —— 周期行
	// 会被 scheduler 反复 re-arm，导致同一个需求每天重复跑。
	Recurrence string `json:"recurrence"` // SchedRecurDaily | SchedRecurWeekly
	RecurTime  string `json:"recur_time"` // "HH:MM"
	RecurDays  string `json:"recur_days"` // weekday CSV 0-6, 0=Sunday (JS getDay convention); weekly only
	RecurTZ    string `json:"recur_tz"`   // IANA tz name, e.g. "Asia/Shanghai"

	AutoPush      bool `json:"auto_push"`      // 开发完成后自动 commit/push/PR；issue-monitor 行默认 false
	CommentOnPR   bool `json:"comment_on_pr"`  // when true and a PR is created, the auto-push hook posts a single comment on the originating Issue
	CommentPosted bool `json:"comment_posted"` // one-shot guard against double-commenting

	IntervalMinutes int        `json:"interval_minutes"` // 1..60*24*7; clamped at write time to >=1 and to ceil(NOVA_ISSUE_MONITOR_INTERVAL / 60)
	Active          bool       `json:"active"`
	LastRunAt       *time.Time `json:"last_run_at,omitempty"`
	LastStatus      string     `json:"last_status"` // "ok" | "partial" | "failed"
	LastError       string     `json:"last_error"`
	RunCount        int        `json:"run_count"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// IssueMonitor LaunchMode constants. The immediate / scheduled literals are
// DELIBERATELY identical to model.LaunchSpec.Mode's wire values so the
// poller can copy the field straight into a LaunchSpec and hand it to the
// same handler.dispatchLaunch path requirement creation uses.
//
//	create_only → no dispatch at all; the imported requirement sits in the
//	              list for a human to drive.
//	immediate   → dispatchLaunch(mode="immediate") right after import.
//	scheduled   → buildLaunchSpec resolves the Recurrence/RecurTime/RecurDays
//	              window into a concrete run_at and creates a one-shot
//	              scheduled_tasks row (see service/issue_monitor.go).
//
// These three values are mirrored in IssueMonitorLaunchMode in
// frontend/src/api/client.ts.
const (
	LaunchModeCreateOnly = "create_only"
	LaunchModeImmediate  = "immediate"
	LaunchModeScheduled  = "scheduled"
)

// IsValidLaunchMode returns true when s is one of the three LaunchMode*
// values. The PUT handler uses this to reject an obviously-bad input early
// before the service-layer guard kicks in.
func IsValidLaunchMode(s string) bool {
	switch s {
	case LaunchModeCreateOnly, LaunchModeImmediate, LaunchModeScheduled:
		return true
	}
	return false
}

// IsValidRecurrence restricts the monitor's time-window recurrence to the
// two repeating forms. SchedRecurOnce is intentionally NOT accepted: a
// poller imports Issues at unpredictable times, so a single absolute
// run_at carries no meaning. The service coerces anything else to daily.
func IsValidRecurrence(s string) bool {
	switch s {
	case SchedRecurDaily, SchedRecurWeekly:
		return true
	}
	return false
}
