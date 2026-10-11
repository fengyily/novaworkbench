package service

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

// newService builds a *IssueMonitorService with the global interval floor
// defaulting to 60s (=1 minute). Construction requires a non-nil *db.DB;
// we never call methods that touch the DB in this test file, so a nil DB
// is fine — we go through the package-private normalize / buildLaunchSpec
// paths.
func newService(t *testing.T) *IssueMonitorService {
	t.Helper()
	s := NewIssueMonitorService(nil)
	return s
}

// TestNormalize_RejectsEmptyLabel locks in the contract that a monitor
// row without a Label is rejected — without this guard the ticker would
// silently fetch ALL open Issues every poll and the user's dashboard
// would explode.
func TestNormalize_RejectsEmptyLabel(t *testing.T) {
	s := newService(t)
	mon := &model.IssueMonitor{
		ProjectID:  "proj_a",
		Label:      "",
		LaunchMode: model.LaunchModeCreateOnly,
	}
	err := s.normalize(mon)
	if err == nil {
		t.Fatal("expected normalize to reject empty label, got nil")
	}
	if !strings.Contains(err.Error(), "label") {
		t.Fatalf("error should mention label, got: %v", err)
	}
}

// TestNormalize_RequiresRecurTimeWhenScheduled replaces the old
// "agent_server_id required when exec_env=agent_server" case. That rule is
// gone: ExecEnvSelect's value IS the agent server id ("" = local), so
// there is no invalid combination left to reject. The scheduled branch does
// still need a well-formed recur_time, since buildLaunchSpec feeds it to
// NextRunAt.
func TestNormalize_RequiresRecurTimeWhenScheduled(t *testing.T) {
	s := newService(t)
	mon := &model.IssueMonitor{
		ProjectID:  "proj_a",
		Label:      "needs-fix",
		LaunchMode: model.LaunchModeScheduled,
		Recurrence: model.SchedRecurDaily,
		RecurTime:  "",
	}
	if err := s.normalize(mon); err == nil {
		t.Fatal("expected normalize to reject a scheduled monitor with empty recur_time")
	}

	mon.RecurTime = "09:00"
	if err := s.normalize(mon); err != nil {
		t.Fatalf("normalize rejected a valid scheduled monitor: %v", err)
	}
}

// TestNormalize_CoercesOnceToDaily: 'once' (and anything unrecognized) is
// meaningless on a poller — it imports Issues at unpredictable times, so an
// absolute run_at carries no meaning. normalize coerces rather than errors.
func TestNormalize_CoercesOnceToDaily(t *testing.T) {
	s := newService(t)
	for _, in := range []string{"once", "", "monthly", "bogus"} {
		mon := &model.IssueMonitor{
			ProjectID:  "proj_a",
			Label:      "needs-fix",
			LaunchMode: model.LaunchModeScheduled,
			Recurrence: in,
			RecurTime:  "09:00",
		}
		if err := s.normalize(mon); err != nil {
			t.Fatalf("normalize(%q): %v", in, err)
		}
		if mon.Recurrence != model.SchedRecurDaily {
			t.Fatalf("recurrence %q should coerce to %q, got %q", in, model.SchedRecurDaily, mon.Recurrence)
		}
	}
}

// TestNormalize_WeeklyRequiresRecurDays: weekly without a weekday list can
// never produce a next occurrence, so it must be rejected at write time
// rather than failing later inside NextRunAt.
func TestNormalize_WeeklyRequiresRecurDays(t *testing.T) {
	s := newService(t)
	mon := &model.IssueMonitor{
		ProjectID:  "proj_a",
		Label:      "needs-fix",
		LaunchMode: model.LaunchModeScheduled,
		Recurrence: model.SchedRecurWeekly,
		RecurTime:  "09:00",
		RecurDays:  "",
	}
	if err := s.normalize(mon); err == nil {
		t.Fatal("expected weekly without recur_days to be rejected")
	}

	mon.RecurDays = "1,3,5"
	if err := s.normalize(mon); err != nil {
		t.Fatalf("normalize rejected valid weekly days: %v", err)
	}
}

// TestNormalize_IgnoresScheduleFieldsWhenNotScheduled: an immediate /
// create_only monitor keeps whatever stale window values it has without
// being rejected — only the scheduled branch validates them.
func TestNormalize_IgnoresScheduleFieldsWhenNotScheduled(t *testing.T) {
	s := newService(t)
	mon := &model.IssueMonitor{
		ProjectID:  "proj_a",
		Label:      "needs-fix",
		LaunchMode: model.LaunchModeImmediate,
		RecurTime:  "totally-bogus",
	}
	if err := s.normalize(mon); err != nil {
		t.Fatalf("immediate monitor should not validate the schedule window: %v", err)
	}
}

// TestNormalize_ClampsInterval asserts that the interval floor is honored
// even when the user submits 0 or 1 (the latter is technically allowed but
// the env floor is the real minimum in production).
func TestNormalize_ClampsInterval(t *testing.T) {
	s := newService(t)
	cases := []struct {
		name           string
		initialMinutes int
		wantMinutes    int
	}{
		{"zero clamps to 1", 0, 1},
		{"negative clamps to 1", -5, 1},
		{"cap at one week", 60 * 24 * 30, 60 * 24 * 7},
		{"valid stays", 5, 5},
		{"valid high stays", 60 * 24, 60 * 24},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mon := &model.IssueMonitor{
				ProjectID:       "proj_a",
				Label:           "needs-fix",
				LaunchMode:      model.LaunchModeCreateOnly,
				IntervalMinutes: c.initialMinutes,
			}
			if err := s.normalize(mon); err != nil {
				t.Fatalf("normalize rejected valid input: %v", err)
			}
			if mon.IntervalMinutes != c.wantMinutes {
				t.Fatalf("got interval %d, want %d", mon.IntervalMinutes, c.wantMinutes)
			}
		})
	}
}

// TestNormalize_FillsDefaults covers the empty-launch_mode path — the form
// submits with whatever the user changed; an empty mode has to land on the
// safe default ("only create", i.e. no dispatch) rather than accidentally
// starting a pipeline. The handler layer fills this before normalize; the
// service defaults it again so a direct service call is also safe.
func TestNormalize_FillsDefaults(t *testing.T) {
	s := newService(t)
	mon := &model.IssueMonitor{
		ProjectID: "proj_a",
		Label:     "needs-fix",
	}
	if err := s.normalize(mon); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if mon.LaunchMode != model.LaunchModeCreateOnly {
		t.Fatalf("launch_mode got %q, want %q", mon.LaunchMode, model.LaunchModeCreateOnly)
	}
	if mon.IntervalMinutes < 1 {
		t.Fatalf("interval_minutes should be clamped >=1, got %d", mon.IntervalMinutes)
	}
}

// TestIntervalFloorRounding locks in the env-floor promotion: a NOVA_ISSUE_MONITOR_INTERVAL
// of 30s must still produce a 1-minute floor in normalize. Going below 60s
// never lets the user dial past the env knob.
func TestIntervalFloorRounding(t *testing.T) {
	s := newService(t)
	// Re-assign globalIntervalFloor directly via the package-private field
	// (test is in the same package, no public setter needed for the
	// production code path; the constructor reads env at boot).
	s.globalIntervalFloor = 30 * 1_000_000_000 // 30s in nanoseconds, below the parseDuration
	if got := s.intervalFloorMinutes(); got != 1 {
		t.Fatalf("30s floor should round up to 1 minute, got %d", got)
	}
	s.globalIntervalFloor = 90 * 1_000_000_000 // 90s
	if got := s.intervalFloorMinutes(); got != 2 {
		t.Fatalf("90s floor should round up to 2 minutes, got %d", got)
	}
}

// TestLaunchModeConstants ensures the model constants line up with the wire
// literals documented in the plan / frontend types. Drift here breaks the
// mode cards in ProjectIssueMonitor.tsx silently.
//
// The immediate/scheduled literals must ALSO equal LaunchSpec.Mode's values
// (asserted below) — buildLaunchSpec copies the field straight across, so a
// divergence would produce a dispatchLaunch 400 "launch.mode must be one of
// immediate/scheduled".
func TestLaunchModeConstants(t *testing.T) {
	for _, k := range []string{model.LaunchModeCreateOnly, model.LaunchModeImmediate, model.LaunchModeScheduled} {
		if !model.IsValidLaunchMode(k) {
			t.Fatalf("IsValidLaunchMode should accept %q", k)
		}
	}
	for _, bad := range []string{"manual_create", "create_and_push", "create_and_develop", ""} {
		if model.IsValidLaunchMode(bad) {
			t.Fatalf("IsValidLaunchMode should reject %q", bad)
		}
	}
	if model.LaunchModeImmediate != "immediate" || model.LaunchModeScheduled != "scheduled" {
		t.Fatal("launch-mode literals must match LaunchSpec.Mode wire values")
	}
}

// TestIsValidRecurrence: only the two repeating forms are valid — 'once'
// is deliberately excluded because the poller resolves the window into a
// concrete run_at itself.
func TestIsValidRecurrence(t *testing.T) {
	for _, k := range []string{model.SchedRecurDaily, model.SchedRecurWeekly} {
		if !model.IsValidRecurrence(k) {
			t.Fatalf("IsValidRecurrence should accept %q", k)
		}
	}
	for _, bad := range []string{model.SchedRecurOnce, "", "monthly"} {
		if model.IsValidRecurrence(bad) {
			t.Fatalf("IsValidRecurrence should reject %q", bad)
		}
	}
}

// TestBuildLaunchSpec_CreateOnly: no dispatch at all. Returning nil (rather
// than an empty spec) is the contract RunOne relies on to skip LaunchFn.
func TestBuildLaunchSpec_CreateOnly(t *testing.T) {
	s := newService(t)
	spec, err := s.buildLaunchSpec(&model.IssueMonitor{LaunchMode: model.LaunchModeCreateOnly})
	if err != nil {
		t.Fatalf("buildLaunchSpec: %v", err)
	}
	if spec != nil {
		t.Fatalf("create_only should produce a nil spec, got %+v", spec)
	}
}

// TestBuildLaunchSpec_Immediate: the per-stage fields must survive verbatim
// (this is the bug the old shim had — it dropped the claude config ids), and
// there must be no schedule block.
func TestBuildLaunchSpec_Immediate(t *testing.T) {
	s := newService(t)
	mon := &model.IssueMonitor{
		LaunchMode:           model.LaunchModeImmediate,
		DesignModel:          "design-model",
		DesignClaudeConfigID: "ccfg_design",
		DesignAgentServerID:  "agent_d",
		CodingModel:          "coding-model",
		CodingClaudeConfigID: "ccfg_coding",
		CodingAgentServerID:  "agent_c",
		AutoPush:             true,
	}
	spec, err := s.buildLaunchSpec(mon)
	if err != nil {
		t.Fatalf("buildLaunchSpec: %v", err)
	}
	if spec == nil {
		t.Fatal("immediate should produce a spec")
	}
	if spec.Mode != "immediate" {
		t.Fatalf("mode got %q, want immediate", spec.Mode)
	}
	if spec.Schedule != nil {
		t.Fatalf("immediate must not carry a schedule block, got %+v", spec.Schedule)
	}
	if spec.DesignModel != "design-model" || spec.DesignClaudeConfigID != "ccfg_design" || spec.DesignAgentServerID != "agent_d" {
		t.Fatalf("design stage fields not copied: %+v", spec)
	}
	if spec.CodingModel != "coding-model" || spec.CodingClaudeConfigID != "ccfg_coding" || spec.CodingAgentServerID != "agent_c" {
		t.Fatalf("coding stage fields not copied: %+v", spec)
	}
	if spec.AutoPushPR == nil || !*spec.AutoPushPR {
		t.Fatal("AutoPushPR must be a non-nil pointer carrying mon.AutoPush")
	}
	if !spec.ReadKnowledge || !spec.SplitTasks {
		t.Fatal("ReadKnowledge / SplitTasks must stay true (pre-existing monitor behavior)")
	}
}

// TestBuildLaunchSpec_ScheduledIsOneShot is the load-bearing assertion of
// this design. The monitor's window (daily/weekly) must NOT be forwarded as
// a recurrence: a repeating scheduled_tasks row is re-armed by the scheduler
// on finish, which would re-run design+coding on the SAME requirement every
// day. Instead the window is resolved to one concrete instant and emitted as
// a one-shot task.
func TestBuildLaunchSpec_ScheduledIsOneShot(t *testing.T) {
	s := newService(t)
	mon := &model.IssueMonitor{
		LaunchMode: model.LaunchModeScheduled,
		Recurrence: model.SchedRecurDaily,
		RecurTime:  "09:00",
		RecurTZ:    "UTC",
	}
	spec, err := s.buildLaunchSpec(mon)
	if err != nil {
		t.Fatalf("buildLaunchSpec: %v", err)
	}
	if spec.Mode != "scheduled" {
		t.Fatalf("mode got %q, want scheduled", spec.Mode)
	}
	if spec.Schedule == nil {
		t.Fatal("scheduled must carry a schedule block")
	}
	if spec.Schedule.Recurrence != model.SchedRecurOnce {
		t.Fatalf("schedule recurrence got %q, want %q (one-shot — a repeating row would re-arm forever)",
			spec.Schedule.Recurrence, model.SchedRecurOnce)
	}
	runAt, err := time.Parse(time.RFC3339, spec.Schedule.RunAt)
	if err != nil {
		t.Fatalf("run_at %q is not RFC3339: %v", spec.Schedule.RunAt, err)
	}
	// dispatchScheduled rejects run_at < now+30s; buildLaunchSpec passes
	// now+1min as its `after`, so the result must clear that bar.
	if !runAt.After(time.Now().Add(30 * time.Second)) {
		t.Fatalf("run_at %s is too close to now (dispatchScheduled would 400 RUN_AT_TOO_SOON)", runAt)
	}
}

// TestBuildLaunchSpec_WeeklyPicksConfiguredWeekday: the weekly window must
// actually land on one of the requested weekdays (in the requested tz),
// rather than silently falling back to "today".
func TestBuildLaunchSpec_WeeklyPicksConfiguredWeekday(t *testing.T) {
	s := newService(t)
	mon := &model.IssueMonitor{
		LaunchMode: model.LaunchModeScheduled,
		Recurrence: model.SchedRecurWeekly,
		RecurTime:  "09:00",
		RecurDays:  "1", // Monday
		RecurTZ:    "UTC",
	}
	spec, err := s.buildLaunchSpec(mon)
	if err != nil {
		t.Fatalf("buildLaunchSpec: %v", err)
	}
	runAt, err := time.Parse(time.RFC3339, spec.Schedule.RunAt)
	if err != nil {
		t.Fatalf("run_at %q is not RFC3339: %v", spec.Schedule.RunAt, err)
	}
	if got := runAt.In(time.UTC).Weekday(); got != time.Monday {
		t.Fatalf("run_at weekday got %s, want Monday", got)
	}
}

// TestIssueMonitor_UpsertRoundTrip exercises the persistence layer against a
// real (temp) SQLite DB. This is the test that actually catches a mismatched
// column enumeration: the service spells its columns out by hand in three
// SELECTs, an INSERT, an UPDATE and two scanners, and a drift between them
// surfaces as a runtime Scan error rather than a compile error. It also
// proves the schema migration really creates the new columns.
func TestIssueMonitor_UpsertRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "imon.db")
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: dbPath})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	defer d.Close()
	seedProject(t, d, "proj_1", "proj_1", "/tmp/proj_1")

	svc := NewIssueMonitorService(d)
	ctx := context.Background()

	mon := &model.IssueMonitor{
		ProjectID:            "proj_1",
		Label:                "needs-fix",
		LaunchMode:           model.LaunchModeScheduled,
		SkipDesign:           true,
		DesignModel:          "design-model",
		DesignClaudeConfigID: "ccfg_design",
		DesignAgentServerID:  "as_design",
		CodingModel:          "coding-model",
		CodingClaudeConfigID: "ccfg_coding",
		CodingAgentServerID:  "as_coding",
		Recurrence:           model.SchedRecurWeekly,
		RecurTime:            "02:30",
		RecurDays:            "1,3",
		RecurTZ:              "Asia/Shanghai",
		AutoPush:             true,
		CommentOnPR:          true,
		IntervalMinutes:      10,
		Active:               true,
	}
	if err := svc.Upsert(ctx, mon); err != nil {
		t.Fatalf("Upsert (insert): %v", err)
	}
	if mon.ID == "" {
		t.Fatal("Upsert should assign an id")
	}

	got, err := svc.GetByProject(ctx, "proj_1")
	if err != nil {
		t.Fatalf("GetByProject: %v", err)
	}
	if got == nil {
		t.Fatal("GetByProject returned nil after Upsert")
	}
	assertMonitorEqual(t, mon, got)

	// Update path — flips several fields including one back to its zero value
	// so a "column silently dropped from the UPDATE" bug shows up.
	upd := *mon
	upd.Label = "changed-label"
	upd.LaunchMode = model.LaunchModeImmediate
	upd.SkipDesign = false
	upd.CodingAgentServerID = ""
	upd.RecurDays = ""
	upd.AutoPush = false
	upd.IntervalMinutes = 30
	if err := svc.Upsert(ctx, &upd); err != nil {
		t.Fatalf("Upsert (update): %v", err)
	}
	if upd.ID != mon.ID {
		t.Fatalf("update must preserve the row id: got %q want %q", upd.ID, mon.ID)
	}

	got2, err := svc.GetByProject(ctx, "proj_1")
	if err != nil {
		t.Fatalf("GetByProject after update: %v", err)
	}
	assertMonitorEqual(t, &upd, got2)

	// ListActive goes through the multi-row scanner, which is a separate
	// Scan call site from the single-row one — exercise it too.
	list, err := svc.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListActive returned %d rows, want 1", len(list))
	}
	assertMonitorEqual(t, &upd, &list[0])

	// GetByID is the third SELECT; a nil-read here means its column list
	// drifted from the other two.
	byID, err := svc.GetByID(ctx, mon.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	assertMonitorEqual(t, &upd, byID)

	// Delete is idempotent.
	if err := svc.DeleteByProject(ctx, "proj_1"); err != nil {
		t.Fatalf("DeleteByProject: %v", err)
	}
	if again, err := svc.GetByProject(ctx, "proj_1"); err != nil || again != nil {
		t.Fatalf("after delete want (nil,nil), got (%+v,%v)", again, err)
	}
	if err := svc.DeleteByProject(ctx, "proj_1"); err != nil {
		t.Fatalf("second DeleteByProject should be a no-op: %v", err)
	}
}

// assertMonitorEqual compares every persisted field. LaunchMode/SkipDesign and
// the six stage fields are the ones this change introduced — asserting them
// field-by-field is the point of the round-trip test.
func assertMonitorEqual(t *testing.T, want, got *model.IssueMonitor) {
	t.Helper()
	if got == nil {
		t.Fatal("got nil monitor")
	}
	type pair struct {
		name     string
		want, gv string
	}
	for _, p := range []pair{
		{"ProjectID", want.ProjectID, got.ProjectID},
		{"Label", want.Label, got.Label},
		{"LaunchMode", want.LaunchMode, got.LaunchMode},
		{"DesignModel", want.DesignModel, got.DesignModel},
		{"DesignClaudeConfigID", want.DesignClaudeConfigID, got.DesignClaudeConfigID},
		{"DesignAgentServerID", want.DesignAgentServerID, got.DesignAgentServerID},
		{"CodingModel", want.CodingModel, got.CodingModel},
		{"CodingClaudeConfigID", want.CodingClaudeConfigID, got.CodingClaudeConfigID},
		{"CodingAgentServerID", want.CodingAgentServerID, got.CodingAgentServerID},
		{"Recurrence", want.Recurrence, got.Recurrence},
		{"RecurTime", want.RecurTime, got.RecurTime},
		{"RecurDays", want.RecurDays, got.RecurDays},
		{"RecurTZ", want.RecurTZ, got.RecurTZ},
	} {
		if p.want != p.gv {
			t.Fatalf("%s: got %q, want %q", p.name, p.gv, p.want)
		}
	}
	if want.SkipDesign != got.SkipDesign {
		t.Fatalf("SkipDesign: got %v, want %v", got.SkipDesign, want.SkipDesign)
	}
	if want.AutoPush != got.AutoPush {
		t.Fatalf("AutoPush: got %v, want %v", got.AutoPush, want.AutoPush)
	}
	if want.CommentOnPR != got.CommentOnPR {
		t.Fatalf("CommentOnPR: got %v, want %v", got.CommentOnPR, want.CommentOnPR)
	}
	if want.IntervalMinutes != got.IntervalMinutes {
		t.Fatalf("IntervalMinutes: got %d, want %d", got.IntervalMinutes, want.IntervalMinutes)
	}
	if want.Active != got.Active {
		t.Fatalf("Active: got %v, want %v", got.Active, want.Active)
	}
}
