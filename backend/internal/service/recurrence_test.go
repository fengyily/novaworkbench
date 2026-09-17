package service

import (
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

// TestNextRunAt_Once — a one-shot task has no next occurrence.
func TestNextRunAt_Once(t *testing.T) {
	got, err := NextRunAt(model.SchedRecurOnce, "09:00", "", "UTC", time.Now())
	if err != nil {
		t.Fatalf("NextRunAt once: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("NextRunAt once = %s, want zero", got)
	}
	// Empty recurrence behaves like once.
	if got, _ := NextRunAt("", "09:00", "", "UTC", time.Now()); !got.IsZero() {
		t.Fatalf("NextRunAt empty = %s, want zero", got)
	}
}

// TestNextRunAt_Daily — the next 09:00 UTC strictly after `after`.
func TestNextRunAt_Daily(t *testing.T) {
	after := time.Date(2026, 9, 17, 8, 0, 0, 0, time.UTC)
	got, err := NextRunAt(model.SchedRecurDaily, "09:00", "", "UTC", after)
	if err != nil {
		t.Fatalf("NextRunAt daily: %v", err)
	}
	want := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("daily before target = %s, want %s", got, want)
	}

	// When `after` is past today's time, it rolls to tomorrow.
	after2 := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	got2, _ := NextRunAt(model.SchedRecurDaily, "09:00", "", "UTC", after2)
	want2 := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	if !got2.Equal(want2) {
		t.Fatalf("daily after target = %s, want %s", got2, want2)
	}

	// Exactly equal to the target must still advance (strictly after).
	after3 := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)
	got3, _ := NextRunAt(model.SchedRecurDaily, "09:00", "", "UTC", after3)
	want3 := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	if !got3.Equal(want3) {
		t.Fatalf("daily equal target = %s, want %s (must be strictly after)", got3, want3)
	}
}

// TestNextRunAt_DailyTimezone — the wall-clock time is interpreted in the
// given tz and the result is returned in UTC.
func TestNextRunAt_DailyTimezone(t *testing.T) {
	// after = 2026-09-17 00:30 UTC = 08:30 Asia/Shanghai. Next 09:00 local
	// == 01:00 UTC same day.
	after := time.Date(2026, 9, 17, 0, 30, 0, 0, time.UTC)
	got, err := NextRunAt(model.SchedRecurDaily, "09:00", "", "Asia/Shanghai", after)
	if err != nil {
		t.Fatalf("NextRunAt tz: %v", err)
	}
	want := time.Date(2026, 9, 17, 1, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("daily tz = %s, want %s", got, want)
	}
	if got.Location() != time.UTC {
		t.Fatalf("result location = %s, want UTC", got.Location())
	}
}

// TestNextRunAt_Weekly — picks the next allowed weekday at the target time.
func TestNextRunAt_Weekly(t *testing.T) {
	// 2026-09-17 is a Thursday (weekday 4). Ask for Mon(1)/Wed(3)/Fri(5).
	after := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	got, err := NextRunAt(model.SchedRecurWeekly, "09:00", "1,3,5", "UTC", after)
	if err != nil {
		t.Fatalf("NextRunAt weekly: %v", err)
	}
	// From Thu noon, next allowed is Fri 09:00 (2026-09-18).
	want := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("weekly = %s, want %s (weekday=%s)", got, want, got.Weekday())
	}
	if got.Weekday() != time.Friday {
		t.Fatalf("weekly weekday = %s, want Friday", got.Weekday())
	}
}

// TestNextRunAt_WeeklySunday — 0 means Sunday (JS getDay convention).
func TestNextRunAt_WeeklySunday(t *testing.T) {
	// 2026-09-17 Thursday. Only Sunday(0) allowed → next Sunday 2026-09-20.
	after := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	got, err := NextRunAt(model.SchedRecurWeekly, "08:30", "0", "UTC", after)
	if err != nil {
		t.Fatalf("NextRunAt weekly sunday: %v", err)
	}
	want := time.Date(2026, 9, 20, 8, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("weekly sunday = %s, want %s", got, want)
	}
}

// TestNextRunAt_InvalidInputs — malformed rules error, and a bad tz falls
// back to UTC rather than failing.
func TestNextRunAt_InvalidInputs(t *testing.T) {
	after := time.Date(2026, 9, 17, 8, 0, 0, 0, time.UTC)
	if _, err := NextRunAt(model.SchedRecurDaily, "25:00", "", "UTC", after); err == nil {
		t.Fatalf("expected error for hour 25")
	}
	if _, err := NextRunAt(model.SchedRecurDaily, "0900", "", "UTC", after); err == nil {
		t.Fatalf("expected error for missing colon")
	}
	if _, err := NextRunAt(model.SchedRecurWeekly, "09:00", "", "UTC", after); err == nil {
		t.Fatalf("expected error for empty weekly days")
	}
	if _, err := NextRunAt(model.SchedRecurWeekly, "09:00", "7", "UTC", after); err == nil {
		t.Fatalf("expected error for day 7 out of range")
	}
	if _, err := NextRunAt("monthly", "09:00", "", "UTC", after); err == nil {
		t.Fatalf("expected error for unknown recurrence")
	}
	// Bad tz → UTC fallback, no error.
	got, err := NextRunAt(model.SchedRecurDaily, "09:00", "", "Not/AZone", after)
	if err != nil {
		t.Fatalf("bad tz should fall back to UTC, got err: %v", err)
	}
	want := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("bad tz fallback = %s, want %s", got, want)
	}
}

// TestFinish_RecurringRearm — a recurring row that finishes goes back to
// pending with run_at advanced, last_* recorded, and run_count incremented.
func TestFinish_RecurringRearm(t *testing.T) {
	d := newScheduledTaskTestDB(t)
	seedScheduledTaskRequirement(t, d)
	svc := NewScheduledTaskService(d)

	created, err := svc.Create(&model.ScheduledTask{
		TaskType:      model.SchedTypeDesign,
		RequirementID: "req_seed_sched",
		ProjectID:     "proj_seed_sched",
		Recurrence:    model.SchedRecurDaily,
		RecurTime:     "09:00",
		RecurTZ:       "UTC",
	})
	if err != nil {
		t.Fatalf("Create recurring: %v", err)
	}
	if created.RunAt.IsZero() {
		t.Fatalf("Create should derive run_at from rule")
	}
	firstRunAt := created.RunAt

	// Simulate a dispatch + finish.
	if _, err := svc.Claim(created.ID, time.Now().UTC()); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := svc.Finish(created.ID, true, "job_abc", ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	got, err := svc.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != model.SchedStatusPending {
		t.Fatalf("after re-arm status = %s, want pending", got.Status)
	}
	if !got.RunAt.After(time.Now().UTC()) {
		t.Fatalf("re-armed run_at = %s should be in the future", got.RunAt)
	}
	if !got.RunAt.After(firstRunAt) && !got.RunAt.Equal(firstRunAt) {
		// run_at is recomputed relative to now; it should be >= the original.
		t.Fatalf("re-armed run_at = %s not advanced from %s", got.RunAt, firstRunAt)
	}
	if got.LastStatus != model.SchedStatusSucceeded {
		t.Fatalf("last_status = %q, want succeeded", got.LastStatus)
	}
	if got.LastJobID != "job_abc" {
		t.Fatalf("last_job_id = %q, want job_abc", got.LastJobID)
	}
	if got.RunCount != 1 {
		t.Fatalf("run_count = %d, want 1", got.RunCount)
	}
	if got.LastRunAt == nil {
		t.Fatalf("last_run_at should be set after a run")
	}
	if got.ExecutedAt != nil {
		t.Fatalf("executed_at should be cleared on re-arm, got %s", got.ExecutedAt)
	}
}

// TestFinish_PausedRecurringGoesTerminal — a paused recurring row is NOT
// re-armed; it retains the historical terminal behavior.
func TestFinish_PausedRecurringGoesTerminal(t *testing.T) {
	d := newScheduledTaskTestDB(t)
	seedScheduledTaskRequirement(t, d)
	svc := NewScheduledTaskService(d)

	created, err := svc.Create(&model.ScheduledTask{
		TaskType:      model.SchedTypeDesign,
		RequirementID: "req_seed_sched",
		ProjectID:     "proj_seed_sched",
		Recurrence:    model.SchedRecurDaily,
		RecurTime:     "09:00",
		RecurTZ:       "UTC",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Pause it.
	falv := false
	if _, err := svc.Update(created.ID, ScheduledTaskPatch{Active: &falv}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := svc.Finish(created.ID, true, "job_x", ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got, _ := svc.Get(created.ID)
	if got.Status != model.SchedStatusSucceeded {
		t.Fatalf("paused recurring finish status = %s, want succeeded (terminal)", got.Status)
	}
}

// TestFinish_OnceUnchanged — a one-shot row still goes terminal on Finish.
func TestFinish_OnceUnchanged(t *testing.T) {
	d := newScheduledTaskTestDB(t)
	seedScheduledTaskRequirement(t, d)
	svc := NewScheduledTaskService(d)

	created, err := svc.Create(&model.ScheduledTask{
		TaskType:      model.SchedTypeDesign,
		RequirementID: "req_seed_sched",
		ProjectID:     "proj_seed_sched",
		RunAt:         time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Finish(created.ID, false, "job_y", "boom"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got, _ := svc.Get(created.ID)
	if got.Status != model.SchedStatusFailed {
		t.Fatalf("once finish status = %s, want failed", got.Status)
	}
	if got.RunCount != 0 {
		t.Fatalf("once run_count = %d, want 0", got.RunCount)
	}
}

// TestDue_ActiveFilter — Due skips paused (active=0) rows.
func TestDue_ActiveFilter(t *testing.T) {
	d := newScheduledTaskTestDB(t)
	seedScheduledTaskRequirement(t, d)
	svc := NewScheduledTaskService(d)

	created, err := svc.Create(&model.ScheduledTask{
		TaskType:      model.SchedTypeDesign,
		RequirementID: "req_seed_sched",
		ProjectID:     "proj_seed_sched",
		Recurrence:    model.SchedRecurDaily,
		RecurTime:     "09:00",
		RecurTZ:       "UTC",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Backdate run_at so it is due now.
	if _, err := d.Exec(`UPDATE scheduled_tasks SET run_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Minute), created.ID); err != nil {
		t.Fatal(err)
	}
	due, _ := svc.Due(time.Now().UTC(), 50)
	if len(due) != 1 {
		t.Fatalf("active row Due = %d, want 1", len(due))
	}
	// Pause and re-check.
	falv := false
	if _, err := svc.Update(created.ID, ScheduledTaskPatch{Active: &falv}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// Update recomputes run_at forward on the rule; backdate again to isolate
	// the active filter.
	if _, err := d.Exec(`UPDATE scheduled_tasks SET run_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Minute), created.ID); err != nil {
		t.Fatal(err)
	}
	due, _ = svc.Due(time.Now().UTC(), 50)
	if len(due) != 0 {
		t.Fatalf("paused row Due = %d, want 0", len(due))
	}
}

// TestFailExpired_SkipsActiveRecurring — active recurring rows are not
// expired even when overdue; one-shot rows still are.
func TestFailExpired_SkipsActiveRecurring(t *testing.T) {
	d := newScheduledTaskTestDB(t)
	seedScheduledTaskRequirement(t, d)
	svc := NewScheduledTaskService(d)

	rec, err := svc.Create(&model.ScheduledTask{
		TaskType:      model.SchedTypeDesign,
		RequirementID: "req_seed_sched",
		ProjectID:     "proj_seed_sched",
		Recurrence:    model.SchedRecurDaily,
		RecurTime:     "09:00",
		RecurTZ:       "UTC",
	})
	if err != nil {
		t.Fatalf("Create recurring: %v", err)
	}
	once, err := svc.Create(&model.ScheduledTask{
		TaskType:      model.SchedTypeCoding,
		RequirementID: "req_seed_sched",
		ProjectID:     "proj_seed_sched",
		RunAt:         time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create once: %v", err)
	}
	// Backdate both far into the past.
	old := time.Now().UTC().Add(-48 * time.Hour)
	if _, err := d.Exec(`UPDATE scheduled_tasks SET run_at = ?`, old); err != nil {
		t.Fatal(err)
	}
	n, err := svc.FailExpired(time.Now().UTC(), 24*time.Hour, "expired")
	if err != nil {
		t.Fatalf("FailExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("FailExpired count = %d, want 1 (only the once row)", n)
	}
	recRow, _ := svc.Get(rec.ID)
	if recRow.Status != model.SchedStatusPending {
		t.Fatalf("recurring row status = %s, want pending (not expired)", recRow.Status)
	}
	onceRow, _ := svc.Get(once.ID)
	if onceRow.Status != model.SchedStatusFailed {
		t.Fatalf("once row status = %s, want failed", onceRow.Status)
	}
}

// TestRecoverInterrupted_RecurringRearm — a running recurring row is re-armed
// at boot rather than failed; a running one-shot row is failed.
func TestRecoverInterrupted_RecurringRearm(t *testing.T) {
	d := newScheduledTaskTestDB(t)
	seedScheduledTaskRequirement(t, d)
	svc := NewScheduledTaskService(d)

	rec, err := svc.Create(&model.ScheduledTask{
		TaskType:      model.SchedTypeDesign,
		RequirementID: "req_seed_sched",
		ProjectID:     "proj_seed_sched",
		Recurrence:    model.SchedRecurWeekly,
		RecurTime:     "09:00",
		RecurDays:     "1,3,5",
		RecurTZ:       "UTC",
	})
	if err != nil {
		t.Fatalf("Create recurring: %v", err)
	}
	once, err := svc.Create(&model.ScheduledTask{
		TaskType:      model.SchedTypeCoding,
		RequirementID: "req_seed_sched",
		ProjectID:     "proj_seed_sched",
		RunAt:         time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create once: %v", err)
	}
	// Force both into running (simulate an interrupted dispatch).
	if _, err := d.Exec(`UPDATE scheduled_tasks SET status = ?`, model.SchedStatusRunning); err != nil {
		t.Fatal(err)
	}
	n, err := svc.RecoverInterrupted()
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if n != 2 {
		t.Fatalf("recovered = %d, want 2", n)
	}
	recRow, _ := svc.Get(rec.ID)
	if recRow.Status != model.SchedStatusPending {
		t.Fatalf("recurring recovered status = %s, want pending", recRow.Status)
	}
	if recRow.LastStatus != model.SchedStatusFailed {
		t.Fatalf("recurring recovered last_status = %q, want failed", recRow.LastStatus)
	}
	onceRow, _ := svc.Get(once.ID)
	if onceRow.Status != model.SchedStatusFailed {
		t.Fatalf("once recovered status = %s, want failed", onceRow.Status)
	}
}

// TestUpdate_ResumeRecomputesRunAt — resuming a paused recurring row pushes
// run_at forward and returns it to pending.
func TestUpdate_ResumeRecomputesRunAt(t *testing.T) {
	d := newScheduledTaskTestDB(t)
	seedScheduledTaskRequirement(t, d)
	svc := NewScheduledTaskService(d)

	created, err := svc.Create(&model.ScheduledTask{
		TaskType:      model.SchedTypeDesign,
		RequirementID: "req_seed_sched",
		ProjectID:     "proj_seed_sched",
		Recurrence:    model.SchedRecurDaily,
		RecurTime:     "09:00",
		RecurTZ:       "UTC",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	falv, truv := false, true
	if _, err := svc.Update(created.ID, ScheduledTaskPatch{Active: &falv}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	resumed, err := svc.Update(created.ID, ScheduledTaskPatch{Active: &truv})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !resumed.Active {
		t.Fatalf("resumed row should be active")
	}
	if resumed.Status != model.SchedStatusPending {
		t.Fatalf("resumed status = %s, want pending", resumed.Status)
	}
	if !resumed.RunAt.After(time.Now().UTC()) {
		t.Fatalf("resumed run_at = %s should be in the future", resumed.RunAt)
	}
}
