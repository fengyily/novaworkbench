package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/platform"
	"github.com/novaworkbench/backend/internal/util"
)

// WizardLaunchFn is the dependency-injected boundary the poller uses to
// dispatch a freshly-imported Issue's development run without importing
// internal/handler (which would create a cycle: handler → service →
// handler). main.go wires it via setter to
// handler.WizardHandler.RunLaunchForMonitor, which routes the spec through
// the SAME dispatchLaunch path requirement creation uses — so immediate vs
// scheduled, design+coding vs coding-only, and the per-stage claude config
// ids all behave identically to the create-requirement flow.
//
// Passing a full model.LaunchSpec (rather than an ad-hoc field bag) is what
// keeps the two paths from drifting: a new LaunchSpec knob reaches the
// monitor for free.
//
// Returns (mode, scheduleID, errCode, errMsg). An empty errCode means
// success — mode is "immediate" or "scheduled", and scheduleID is the
// scheduled_tasks.id when mode=="scheduled" (empty otherwise). A non-empty
// errCode is the "soft launch failure" the user sees surfaced in
// last_status='partial' or 'failed'.
type WizardLaunchFn func(ctx context.Context, requirementID string, spec model.LaunchSpec) (mode string, scheduleID string, errCode string, errMsg string)

// ProjectClientProvider resolves a project's git remote URL and platform
// credential WITHOUT reaching into the handler package. main.go passes in
// a closure backed by handler.review.go's effectiveRemoteURL + the
// platform_tokens fetch. Returning an empty string from resolveRemote is
// fine: RunOne falls back to `git remote get-url origin` exactly the way
// handler/review.go does.
type ProjectClientProvider func(ctx context.Context, projectID string) (platformType, baseURL, token, remoteURL string, err error)

// IssueMonitorService is the persistence + orchestration wrapper around
// the issue_monitors table. CRUD is straightforward SQL; RunOne is the
// interesting method — it ties together: resolve project → call
// platform → dedup → upsert requirements → optionally launch wizard →
// record last_*. Concurrency: per-monitor mutex via a sync.Map ensures
// the scheduler's queue can never run the same monitor concurrently;
// duplicate (project, Issue) tuples are skipped by SELECT-then-INSERT
// inside a single transaction.
type IssueMonitorService struct {
	db *db.DB

	// LaunchFn is the optional wizard entry point. nil means "auto-launch
	// disabled globally" (the legacy v0.5.x dev path before the wizard
	// hook was wired); main.go always sets it in production.
	LaunchFn WizardLaunchFn

	// Provider supplies the project's effective remote + per-platform
	// credentials. nil means "RunOne cannot fetch Issues"; the ticker
	// catches the nil and logs but does NOT panic — it just marks the
	// monitor last_status='failed'.
	Provider ProjectClientProvider

	// runMu is the per-monitor lock surface. Keys are monitor.ID.
	// Compared with CompareAndSwap so two goroutines waiting on the same
	// monitor only let ONE through; the others observe a "busy" return.
	runMu sync.Map

	// Body cap for Issue body → requirement description. GitHub rarely
	// exceeds 64 KB but the poller guards against degenerate payloads so
	// the wizard's first prompt doesn't blow up the context window.
	bodyCapBytes int

	// globalIntervalFloor is the env-driven global floor
	// (NOVA_ISSUE_MONITOR_INTERVAL). Read on construction so an operator
	// hot-reload does not silently lower the floor mid-flight.
	globalIntervalFloor time.Duration
}

// NewIssueMonitorService wires the optional LaunchFn and Provider
// separately so callers (tests, in-process batch tools) can build a
// service without those dependencies and never accidentally hit them.
func NewIssueMonitorService(database *db.DB) *IssueMonitorService {
	floor, _ := time.ParseDuration(strOr("NOVA_ISSUE_MONITOR_INTERVAL", "60s"))
	if floor < time.Second {
		floor = time.Second
	}
	return &IssueMonitorService{
		db:                   database,
		bodyCapBytes:         32 * 1024,
		globalIntervalFloor:  floor,
	}
}

// SetLaunchFn injects the wizard entry — called from main.go once the
// wizard handler is constructed. Returns the receiver for fluent wiring.
func (s *IssueMonitorService) SetLaunchFn(fn WizardLaunchFn) *IssueMonitorService {
	s.LaunchFn = fn
	return s
}

// SetProvider injects the project credential resolver. Same fluent style
// as SetLaunchFn.
func (s *IssueMonitorService) SetProvider(p ProjectClientProvider) *IssueMonitorService {
	s.Provider = p
	return s
}

// strOr returns env[name] when set, otherwise def. Used for the env knob
// + default fallback in one line at construction time.
func strOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// intervalFloorMinutes rounds the env floor up to whole minutes (the
// user-facing field). A 30s env still clamps `interval_minutes` to 1.
func (s *IssueMonitorService) intervalFloorMinutes() int {
	secs := int(s.globalIntervalFloor.Seconds())
	if secs < 60 {
		return 1
	}
	return (secs + 59) / 60
}

// GetByProject returns the monitor row for a single project, or nil
// with no error when there isn't one (the handler maps nil to 404). The
// project_id column is unique by application convention; the migration
// doesn't enforce it because the handler's PUT upserts on conflict.
func (s *IssueMonitorService) GetByProject(ctx context.Context, projectID string) (*model.IssueMonitor, error) {
	if projectID == "" {
		return nil, errors.New("project_id required")
	}
	row := s.db.QueryRow(
		`SELECT id, project_id, label,
				launch_mode, skip_design,
				design_model, design_claude_config_id, design_agent_server_id,
				coding_model, coding_claude_config_id, coding_agent_server_id,
				recurrence, recur_time, recur_days, recur_tz,
				auto_push, comment_on_pr, comment_posted, interval_minutes,
				active, last_run_at, last_status, last_error, run_count,
				created_at, updated_at
		 FROM issue_monitors WHERE project_id = ?`, projectID)
	m, err := scanMonitor(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// GetByID is the symmetric lookup used by the queue's per-monitor
// mutex-prime path so a row disabled mid-run is detected reliably. NOT
// exposed via the HTTP layer (handlers always go through GetByProject).
func (s *IssueMonitorService) GetByID(ctx context.Context, id string) (*model.IssueMonitor, error) {
	row := s.db.QueryRow(
		`SELECT id, project_id, label,
				launch_mode, skip_design,
				design_model, design_claude_config_id, design_agent_server_id,
				coding_model, coding_claude_config_id, coding_agent_server_id,
				recurrence, recur_time, recur_days, recur_tz,
				auto_push, comment_on_pr, comment_posted, interval_minutes,
				active, last_run_at, last_status, last_error, run_count,
				created_at, updated_at
		 FROM issue_monitors WHERE id = ?`, id)
	m, err := scanMonitor(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// ListActive returns every monitor with active=1 — the scheduler's tick
// hot path. cheap query (idx_issue_monitors_active covers it) so the
// service does no further filtering.
func (s *IssueMonitorService) ListActive(ctx context.Context) ([]model.IssueMonitor, error) {
	rows, err := s.db.Query(
		`SELECT id, project_id, label,
				launch_mode, skip_design,
				design_model, design_claude_config_id, design_agent_server_id,
				coding_model, coding_claude_config_id, coding_agent_server_id,
				recurrence, recur_time, recur_days, recur_tz,
				auto_push, comment_on_pr, comment_posted, interval_minutes,
				active, last_run_at, last_status, last_error, run_count,
				created_at, updated_at
		 FROM issue_monitors WHERE active = 1 ORDER BY last_run_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.IssueMonitor{}
	for rows.Next() {
		m, err := scanMonitorRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, nil
}

// ListAllActive is the GET /api/issue-monitors/active route. Mirrors
// ListActive but returns errors instead of empty slices so handlers can
// surface 5xx without burying the cause.
func (s *IssueMonitorService) ListAllActive(ctx context.Context) ([]model.IssueMonitor, error) {
	return s.ListActive(ctx)
}

// Upsert writes the monitor row for mon.ProjectID. If a row already
// exists it is updated in place (so the project_id column stays unique
// by application convention); the id column is preserved so the queue's
// in-memory mutex cache (keyed on id) survives across PUTs.
func (s *IssueMonitorService) Upsert(ctx context.Context, mon *model.IssueMonitor) error {
	if mon.ProjectID == "" {
		return errors.New("project_id required")
	}
	if err := s.normalize(mon); err != nil {
		return err
	}
	now := time.Now()
	existing, err := s.GetByProject(ctx, mon.ProjectID)
	if err != nil {
		return err
	}
	if existing == nil {
		if mon.ID == "" {
			mon.ID = util.NewID("imon")
		}
		_, err := s.db.Exec(
			`INSERT INTO issue_monitors
				(id, project_id, label,
				 launch_mode, skip_design,
				 design_model, design_claude_config_id, design_agent_server_id,
				 coding_model, coding_claude_config_id, coding_agent_server_id,
				 recurrence, recur_time, recur_days, recur_tz,
				 auto_push, comment_on_pr, comment_posted, interval_minutes,
				 active, run_count, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			mon.ID, mon.ProjectID, mon.Label,
			mon.LaunchMode, boolInt(mon.SkipDesign),
			mon.DesignModel, mon.DesignClaudeConfigID, mon.DesignAgentServerID,
			mon.CodingModel, mon.CodingClaudeConfigID, mon.CodingAgentServerID,
			mon.Recurrence, mon.RecurTime, mon.RecurDays, mon.RecurTZ,
			boolInt(mon.AutoPush), boolInt(mon.CommentOnPR), boolInt(mon.CommentPosted), mon.IntervalMinutes,
			boolInt(mon.Active), mon.RunCount, now, now)
		if err != nil {
			return err
		}
		mon.CreatedAt = now
		mon.UpdatedAt = now
		return nil
	}
	// Update path: preserve id + comment_posted so a hot-reload of the
	// form doesn't accidentally reset the "we already posted the comment"
	// guard. Every other field is overwritten by the request body.
	mon.ID = existing.ID
	mon.CreatedAt = existing.CreatedAt
	mon.CommentPosted = existing.CommentPosted
	mon.RunCount = existing.RunCount
	_, err = s.db.Exec(
		`UPDATE issue_monitors SET
			label = ?, launch_mode = ?, skip_design = ?,
			design_model = ?, design_claude_config_id = ?, design_agent_server_id = ?,
			coding_model = ?, coding_claude_config_id = ?, coding_agent_server_id = ?,
			recurrence = ?, recur_time = ?, recur_days = ?, recur_tz = ?,
			auto_push = ?, comment_on_pr = ?, interval_minutes = ?,
			active = ?, updated_at = ?
		 WHERE id = ?`,
		mon.Label, mon.LaunchMode, boolInt(mon.SkipDesign),
		mon.DesignModel, mon.DesignClaudeConfigID, mon.DesignAgentServerID,
		mon.CodingModel, mon.CodingClaudeConfigID, mon.CodingAgentServerID,
		mon.Recurrence, mon.RecurTime, mon.RecurDays, mon.RecurTZ,
		boolInt(mon.AutoPush), boolInt(mon.CommentOnPR), mon.IntervalMinutes,
		boolInt(mon.Active), now, mon.ID)
	if err != nil {
		return err
	}
	mon.UpdatedAt = now
	return nil
}

// DeleteByProject removes the monitor row. NotFound is silent (Delete
// is idempotent) so the DELETE handler can return 204 either way.
func (s *IssueMonitorService) DeleteByProject(ctx context.Context, projectID string) error {
	if projectID == "" {
		return errors.New("project_id required")
	}
	_, err := s.db.Exec(`DELETE FROM issue_monitors WHERE project_id = ?`, projectID)
	return err
}

// MarkCommentPosted is called by the auto-push hook AFTER a successful
// PostIssueComment so the monitor knows not to comment a second time.
// Wrapping the UPDATE in a single statement keeps the last_status field
// untouched (the comment hook runs on the wizard's path, not the
// poll's, so we don't want to clobber the last polled status).
func (s *IssueMonitorService) MarkCommentPosted(monitorID string) error {
	if monitorID == "" {
		return nil
	}
	_, err := s.db.Exec(
		`UPDATE issue_monitors SET comment_posted = 1, updated_at = ? WHERE id = ?`,
		time.Now(), monitorID)
	return err
}

// Test fetches the next 5 Issues matching the saved config WITHOUT
// creating any requirements. Lets the operator check the trigger label
// before turning the monitor on. Returns the same shape ListIssues does.
func (s *IssueMonitorService) Test(ctx context.Context, projectID string) ([]platform.Issue, error) {
	if s.Provider == nil {
		return nil, errors.New("project credential provider not configured")
	}
	mon, err := s.GetByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if mon == nil {
		return nil, errors.New("no issue monitor configured for this project")
	}
	if mon.Label == "" {
		return nil, errors.New("monitor has empty label; nothing to test")
	}
	platformType, baseURL, token, remoteURL, err := s.Provider(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("resolve project credentials: %w", err)
	}
	client, err := platform.New(platformType, baseURL, token)
	if err != nil {
		return nil, fmt.Errorf("platform client: %w", err)
	}
	issues, err := client.ListIssues(ctx, remoteURL, mon.Label)
	if err != nil {
		return nil, err
	}
	if len(issues) > 5 {
		issues = issues[:5]
	}
	return issues, nil
}

// RunOne is the per-monitor poll body — see §6 of the plan file. The
// outer mutex (sync.Map keyed by monitor.ID) prevents two scheduler
// ticks from running the same monitor concurrently: a second invocation
// while the first is in flight returns immediately ("busy") instead of
// double-importing the same Issue.
func (s *IssueMonitorService) RunOne(ctx context.Context, monitorID string) error {
	if monitorID == "" {
		return errors.New("monitor_id required")
	}
	// Per-monitor lock via TryLock. A blocking Lock would make the second
	// tick WAIT for the first (serializing two slow GitHub calls back to
	// back); failing fast lets the next tick pick it up once the
	// in-flight run has finished. The mutex is stored in the sync.Map so
	// concurrent callers for DIFFERENT monitors never contend.
	muAny, _ := s.runMu.LoadOrStore(monitorID, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	if !mu.TryLock() {
		log.Printf("[issue-monitor] monitor=%s already running, skipping this tick", monitorID)
		return nil
	}
	defer mu.Unlock()

	mon, err := s.GetByID(ctx, monitorID)
	if err != nil {
		return err
	}
	if mon == nil {
		return fmt.Errorf("monitor %s not found", monitorID)
	}
	// Re-check active inside the lock so a PUT to active=0 mid-tick does
	// NOT cancel an in-flight pull (the row that arrived mid-flight is
	// already being imported) — but it does prevent a freshly paused
	// monitor from firing the SAME tick again.
	if !mon.Active {
		return nil
	}
	// Interval gate: per-monitor minimum delay between two runs. Drift
	// tolerable to ~5s because the goroutine scheduling jitter doesn't
	// matter for a 1-minute minimum.
	if mon.LastRunAt != nil {
		next := mon.LastRunAt.Add(time.Duration(mon.IntervalMinutes) * time.Minute)
		if time.Now().Before(next) {
			return nil
		}
	}
	if mon.Label == "" {
		return s.recordOutcome(ctx, mon, "failed", "monitor has empty label", 0)
	}
	if s.Provider == nil {
		return s.recordOutcome(ctx, mon, "failed", "project credential provider not configured", 0)
	}
	platformType, baseURL, token, remoteURL, err := s.Provider(ctx, mon.ProjectID)
	if err != nil {
		return s.recordOutcome(ctx, mon, "failed", fmt.Sprintf("resolve project credentials: %v", err), 0)
	}
	if platformType != "github" {
		return s.recordOutcome(ctx, mon, "failed", fmt.Sprintf("unsupported platform %q for issue-monitor (v0.5.x GitHub-only)", platformType), 0)
	}
	client, err := platform.New(platformType, baseURL, token)
	if err != nil {
		return s.recordOutcome(ctx, mon, "failed", fmt.Sprintf("platform client: %v", err), 0)
	}
	issues, err := client.ListIssues(ctx, remoteURL, mon.Label)
	if err != nil {
		return s.recordOutcome(ctx, mon, "failed", fmt.Sprintf("GitHub ListIssues: %v", err), 0)
	}
	created := 0
	var lastCreateErr error
	for _, iss := range issues {
		if err := ctx.Err(); err != nil {
			return s.recordOutcome(ctx, mon, "partial", fmt.Sprintf("context cancelled: %v", err), created)
		}
		id, err := s.createRequirementFromIssue(ctx, mon, client, iss, remoteURL)
		if err != nil {
			log.Printf("[issue-monitor] monitor=%s issue=#%d failed: %v", monitorID, iss.Number, err)
			lastCreateErr = err
			continue
		}
		if id == "" {
			// Already imported (dedup hit) — not an error.
			continue
		}
		created++
		// Dispatch the development run. buildLaunchSpec returns nil for
		// launch_mode=create_only, in which case the requirement is left for
		// a human to drive manually. A nil LaunchFn (early-boot race)
		// degrades the same way instead of failing the whole tick.
		spec, specErr := s.buildLaunchSpec(mon)
		if specErr != nil {
			log.Printf("[issue-monitor] monitor=%s req=%s build launch spec failed: %v", monitorID, id, specErr)
			lastCreateErr = specErr
		} else if spec != nil && s.LaunchFn != nil {
			mode, schedID, errCode, errMsg := s.LaunchFn(ctx, id, *spec)
			if errCode != "" {
				log.Printf("[issue-monitor] monitor=%s req=%s launch(%s) failed: %s: %s", monitorID, id, spec.Mode, errCode, errMsg)
				lastCreateErr = fmt.Errorf("wizard launch %s: %s", errCode, errMsg)
			} else if mode == model.LaunchModeScheduled {
				log.Printf("[issue-monitor] monitor=%s req=%s scheduled task=%s run_at=%s", monitorID, id, schedID, spec.Schedule.RunAt)
			}
		}
	}
	status := "ok"
	errMsg := ""
	switch {
	case created == 0 && lastCreateErr == nil:
		// No new issues — perfectly happy no-op.
	case created == 0 && lastCreateErr != nil:
		status = "partial"
		errMsg = lastCreateErr.Error()
	case lastCreateErr != nil:
		status = "partial"
		errMsg = lastCreateErr.Error()
	}
	return s.recordOutcome(ctx, mon, status, errMsg, created)
}

// recordOutcome stamps the last_* columns on the monitor row. Used as
// the unwind path for every error / success branch in RunOne. Uses
// COALESCE on last_run_at so a failing monitor still records the
// attempted run timestamp (helps the UI distinguish "never ran" from
// "ran and failed").
func (s *IssueMonitorService) recordOutcome(ctx context.Context, mon *model.IssueMonitor, status, errMsg string, created int) error {
	now := time.Now()
	_, err := s.db.Exec(
		`UPDATE issue_monitors SET
			last_run_at = COALESCE(last_run_at, ?),
			last_status = ?,
			last_error = ?,
			run_count = run_count + 1,
			updated_at = ?
		 WHERE id = ?`,
		now, status, errMsg, now, mon.ID)
	if err != nil {
		return err
	}
	return nil
}

// createRequirementFromIssue is the per-Issue work. Returns the new
// requirement id, or "" when the (project, source, Issue) tuple already
// has an imported row (dedup hit). All errors are returned and logged by
// the caller.
//
// Concurrency safety: the per-monitor mutex (sync.Map in RunOne) means
// two goroutines from the same backend cannot interleave on the same
// monitor. Cross-process / multi-instance setups would race on the
// SELECT-then-INSERT pair — acceptable for v0.5.x since the feature
// targets a single NovaWorkbench host. Future hardening path: add a
// UNIQUE INDEX on (project_id, source_platform, source_external_id) and
// handle the unique-violation error as "already imported, skip".
func (s *IssueMonitorService) createRequirementFromIssue(ctx context.Context, mon *model.IssueMonitor, client platform.Client, iss platform.Issue, remoteURL string) (string, error) {
	body := iss.Body
	if len(body) > s.bodyCapBytes {
		body = body[:s.bodyCapBytes] + "\n\n…(description truncated to fit)…"
	}
	// Dedup: per-monitor SELECT before INSERT.
	var existing string
	err := s.db.QueryRow(
		`SELECT id FROM requirements
		 WHERE project_id = ? AND source_platform = ? AND source_external_id = ?
		 LIMIT 1`,
		mon.ProjectID, "github", strconv.Itoa(iss.Number),
	).Scan(&existing)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if existing != "" {
		return "", nil
	}
	// Read the Issue row's full JSON for source_data (audit trail).
	payload, _ := json.Marshal(map[string]any{
		"number":   iss.Number,
		"title":    iss.Title,
		"state":    iss.State,
		"author":   iss.Author,
		"labels":   iss.Labels,
		"url":      iss.HTMLURL,
		"raw_body": truncateForAudit(iss.Body, s.bodyCapBytes),
	})
	title := iss.Title
	if title == "" {
		title = fmt.Sprintf("Issue #%d", iss.Number)
	}
	if len(title) > 255 {
		title = title[:255]
	}
	req := model.CreateRequirementReq{
		ProjectID:    mon.ProjectID,
		Title:        title,
		Description:  body,
		Priority:     "medium",
		Kind:         KindIssue,
		SkipAnalysis: ptrBool(true),
		// 开发流程: SkipDesign=true (直接开发) makes resolveLaunchTaskType
		// return SchedTypeCoding; false (方案开发) lands on
		// SchedTypeDesignCoding. Setting it here — rather than in the
		// dispatch layer — is what lets the same dispatchLaunch path serve
		// both flows with no monitor-specific branching.
		SkipDesign:       ptrBool(mon.SkipDesign),
		SkipOrganize:     ptrBool(true),
		SourcePlatform:   "github",
		SourceExternalID: strconv.Itoa(iss.Number),
		SourceURL:        iss.HTMLURL,
		SourceState:      iss.State,
		SourceData:       string(payload),
	}
	r, err := (&RequirementService{db: s.db}).Create(req)
	if err != nil {
		return "", err
	}
	return r.ID, nil
}

// normalize clamps the input monitor to safe ranges before it hits the
// DB. Runs both on Upsert (writes) and on every RunOne (so a tampered
// row still respects limits).
func (s *IssueMonitorService) normalize(mon *model.IssueMonitor) error {
	if mon.Label == "" {
		return errors.New("label is required")
	}
	if mon.LaunchMode == "" {
		mon.LaunchMode = model.LaunchModeCreateOnly
	}
	if !model.IsValidLaunchMode(mon.LaunchMode) {
		return fmt.Errorf("invalid launch_mode %q (allowed: create_only, immediate, scheduled)", mon.LaunchMode)
	}
	if mon.LaunchMode == model.LaunchModeScheduled {
		// 'once'/empty is meaningless for a poller (it imports Issues at
		// unpredictable times, so an absolute run_at carries no meaning).
		// Coerce anything unrecognized to daily rather than erroring — the
		// UI only ever offers daily/weekly.
		if !model.IsValidRecurrence(mon.Recurrence) {
			mon.Recurrence = model.SchedRecurDaily
		}
		if !validRecurTimeStr(mon.RecurTime) {
			return fmt.Errorf("invalid recur_time %q (want HH:MM)", mon.RecurTime)
		}
		if mon.Recurrence == model.SchedRecurWeekly && !validRecurDaysStr(mon.RecurDays) {
			return errors.New("recur_days is required for weekly (CSV of weekday numbers 0-6, 0=Sunday)")
		}
	}
	mon.Label = strings.TrimSpace(mon.Label)
	// Interval clamp: >=1 minute and >= the env floor (rounded up).
	floor := s.intervalFloorMinutes()
	if mon.IntervalMinutes < 1 {
		mon.IntervalMinutes = 1
	}
	if mon.IntervalMinutes < floor {
		mon.IntervalMinutes = floor
	}
	if mon.IntervalMinutes > 60*24*7 {
		mon.IntervalMinutes = 60 * 24 * 7 // one week cap
	}
	return nil
}

// buildLaunchSpec converts a monitor row into the LaunchSpec the wizard's
// dispatchLaunch consumes. Returns (nil, nil) for create_only — the caller
// then skips dispatch entirely.
//
// Why this lives in the service (not as a model method): the scheduled
// branch needs NextRunAt, and model cannot import service.
//
// The scheduled branch is the subtle part. Recall that a scheduled_tasks
// row with a repeating recurrence is never terminal — the scheduler re-arms it
// on finish (status → pending, run_at → next occurrence), which is right
// for an alarm but wrong here: it would re-run design+coding on the SAME
// requirement every day and fail once the requirement left the developable
// statuses. So we resolve the monitor's time WINDOW (recurrence/recur_time/
// recur_days/recur_tz) into one concrete instant and emit a one-shot
// (recurrence='once') task — exactly one development run per imported Issue.
//
// `after` is now+1min rather than now: dispatchScheduled rejects a run_at
// less than 30s in the future (requirement_launch.go RUN_AT_TOO_SOON), and
// without the margin a monitor firing at 01:59:45 would compute a 15s-away
// run_at and get rejected.
func (s *IssueMonitorService) buildLaunchSpec(mon *model.IssueMonitor) (*model.LaunchSpec, error) {
	if mon.LaunchMode == model.LaunchModeCreateOnly {
		return nil, nil
	}
	// Always non-nil so the wizard ENFORCES the monitor's own auto_push
	// instead of falling back to the requirement row's persisted value.
	autoPush := mon.AutoPush
	spec := &model.LaunchSpec{
		Mode:                 mon.LaunchMode, // "immediate" | "scheduled" — same literals as LaunchSpec.Mode
		DesignModel:          mon.DesignModel,
		DesignClaudeConfigID: mon.DesignClaudeConfigID,
		DesignAgentServerID:  mon.DesignAgentServerID,
		CodingModel:          mon.CodingModel,
		CodingClaudeConfigID: mon.CodingClaudeConfigID,
		CodingAgentServerID:  mon.CodingAgentServerID,
		// Preserved from the pre-0.5.x monitor behavior: an imported Issue
		// always gets the project knowledge injected and always splits into
		// sub-tasks. Neither is user-configurable on this form.
		ReadKnowledge: true,
		SplitTasks:    true,
		AutoPushPR:    &autoPush,
	}
	if mon.LaunchMode == model.LaunchModeScheduled {
		runAt, err := NextRunAt(mon.Recurrence, mon.RecurTime, mon.RecurDays, mon.RecurTZ, time.Now().Add(time.Minute))
		if err != nil {
			return nil, fmt.Errorf("resolve schedule window: %w", err)
		}
		spec.Schedule = &model.LaunchScheduleSpec{
			Recurrence: model.SchedRecurOnce,
			RunAt:      runAt.Format(time.RFC3339),
		}
	}
	return spec, nil
}

// validRecurTimeStr mirrors handler/schedule.go's unexported validRecurTime.
// Duplicated rather than exported because the service must not import
// handler (handler → service would become a cycle). Accepts "HH:MM" in
// 00:00-23:59.
func validRecurTimeStr(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	hh, err1 := strconv.Atoi(s[0:2])
	mm, err2 := strconv.Atoi(s[3:5])
	if err1 != nil || err2 != nil {
		return false
	}
	return hh >= 0 && hh <= 23 && mm >= 0 && mm <= 59
}

// validRecurDaysStr mirrors handler/schedule.go's unexported validRecurDays:
// a non-empty CSV of weekday numbers 0-6 (0 = Sunday, the JS getDay
// convention).
func validRecurDaysStr(s string) bool {
	if s == "" {
		return false
	}
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 0 || n > 6 {
			return false
		}
	}
	return true
}

// scanMonitor / scanMonitorRows split a single-row vs multi-row
// scan so the SELECT lists in Get/GetByID/ListActive stay in lockstep
// and we don't accidentally drop a column.
func scanMonitor(row *sql.Row) (*model.IssueMonitor, error) {
	var m model.IssueMonitor
	if err := row.Scan(
		&m.ID, &m.ProjectID, &m.Label,
		&m.LaunchMode, &m.SkipDesign,
		&m.DesignModel, &m.DesignClaudeConfigID, &m.DesignAgentServerID,
		&m.CodingModel, &m.CodingClaudeConfigID, &m.CodingAgentServerID,
		&m.Recurrence, &m.RecurTime, &m.RecurDays, &m.RecurTZ,
		&m.AutoPush, &m.CommentOnPR, &m.CommentPosted, &m.IntervalMinutes,
		&m.Active, &m.LastRunAt, &m.LastStatus, &m.LastError, &m.RunCount,
		&m.CreatedAt, &m.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &m, nil
}

func scanMonitorRows(rows *sql.Rows) (*model.IssueMonitor, error) {
	var m model.IssueMonitor
	if err := rows.Scan(
		&m.ID, &m.ProjectID, &m.Label,
		&m.LaunchMode, &m.SkipDesign,
		&m.DesignModel, &m.DesignClaudeConfigID, &m.DesignAgentServerID,
		&m.CodingModel, &m.CodingClaudeConfigID, &m.CodingAgentServerID,
		&m.Recurrence, &m.RecurTime, &m.RecurDays, &m.RecurTZ,
		&m.AutoPush, &m.CommentOnPR, &m.CommentPosted, &m.IntervalMinutes,
		&m.Active, &m.LastRunAt, &m.LastStatus, &m.LastError, &m.RunCount,
		&m.CreatedAt, &m.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &m, nil
}

// ptrBool returns a *bool for the specified value so the Create path's
// optional-pointer contract is honored (SkipAnalysis and SkipOrganize
// are *bool).
func ptrBool(b bool) *bool { return &b }

// boolInt converts a bool to the 0/1 representation the SQLite/MySQL
// INTEGER columns expect. Kept as a helper to make the INSERT/UPDATE
// statements read cleanly.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// truncateForAudit shrinks a payload to `max` bytes for the source_data
// JSON audit trail. Different from the description cap because we want
// the audit to keep the *first* slice, while the description can omit.
func truncateForAudit(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated)…"
}

// IssueMonitorListResponse is the API shape the handler returns from
// the LIST endpoint. Kept here next to the service so the handler can
// import it without re-deriving the type. (Renamed from IssueMonitor
// list to avoid colliding with the model struct's name on the wire.)
type IssueMonitorListResponse struct {
	Items []model.IssueMonitor `json:"items"`
}

// decodeURL is a tiny helper that wraps url.Parse + a sentinel for the
// "remote url empty" case. Used by the test preview when the project's
// remote_url column is blank (which handler/review.go's
// effectiveRemoteURL resolves via `git remote get-url origin`).
func decodeURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("remote_url empty")
	}
	return url.Parse(raw)
}
