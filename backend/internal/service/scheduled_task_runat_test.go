package service

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

// contains is a tiny helper so the assertions above read naturally
// without dragging in strings.Contains at every call site.
func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

// TestScheduledTask_LegacyRunAtString reproduces the prod bug:
//
//	"sql: Scan error on column index 5, name 'run_at':
//	 unsupported Scan, storing driver.Value type string into type *time.Time"
//
// Reported: /api/schedules returned 500 on the prod (Sqlite) deployment,
// so the timed-task list page rendered empty. Root cause: time.Time values
// inserted in a zone whose name isn't a 3-letter abbreviation (e.g.
// time.Parse(time.RFC3339, "...+08:00") yields a FixedZone with an empty
// name) round-trip through modernc.org/sqlite's t.String() write path as
// text like "2026-09-07 23:30:00 +0800 +0800", which the driver's
// parseTime parser cannot match against any of its known formats. The
// raw string is then handed to database/sql Scan, which rejects it
// because the destination is *time.Time.
//
// The fix has two halves: (a) Create() now normalizes RunAt to UTC before
// the INSERT so future writes always emit the parseable "+0000 UTC"
// form, and (b) scanScheduledTask reads run_at as a string and parses it
// with parseRunAtString, which tries every format modernc / the wire
// layer uses — so legacy rows already in the prod DB continue to list
// instead of failing the whole request.
//
// This test pins both halves by inserting the exact unparseable string
// the modernc driver would have written, then calling List/Get and
// asserting no error. Pre-fix this returned
// "Scan error ... storing driver.Value type string into type *time.Time"
// and the whole list call 500'd.
func TestScheduledTask_LegacyRunAtString(t *testing.T) {
	d := newScheduledTaskTestDB(t)
	seedScheduledTaskRequirement(t, d)

	svc := NewScheduledTaskService(d)

	// Seed a parent requirement is fine — the bug is independent of FK.
	if _, err := d.Exec(`INSERT INTO requirements (id, project_id, title) VALUES (?, ?, ?)`,
		"req_legacy", "proj_legacy", "Legacy"); err != nil {
		t.Fatalf("seed requirement: %v", err)
	}

	// This is the exact string the modernc driver writes for a time
	// produced by time.Parse(time.RFC3339, "...+08:00") — the trailing
	// "+0800" is the zone name (empty FixedZone gets serialized as the
	// offset again), and the driver's parseTime can't match it.
	const legacyRunAt = "2026-09-07 23:30:00 +0800 +0800"
	if _, err := d.Exec(`INSERT INTO scheduled_tasks
		(id, task_type, requirement_id, project_id, requirement_title,
		 run_at, model, read_knowledge, branch_name, base_branch,
		 agent_server_id, split_tasks, status, job_id, error_message,
		 created_by, created_at, updated_at, executed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"sched_legacy", model.SchedTypeDesign, "req_legacy", "proj_legacy", "Legacy",
		legacyRunAt, "", false, "", "", "", false, model.SchedStatusPending, "", "",
		"user", "2026-09-07 15:00:00 +0000 UTC", "2026-09-07 15:00:00 +0000 UTC", nil,
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	// Pre-fix this call failed with the reported Scan error. Post-fix
	// parseRunAtString tries the FixedZone-named layout first, which DOES
	// match when the trailing segment is a 3-letter zone — but not here,
	// because the trailing "+0800" isn't a name. parseRunAtString then
	// falls through to zero-time and the row still appears (with the bad
	// run_at surfaced as an empty value). The point is: no error, no 500.
	list, err := svc.List("", "", "")
	if err != nil {
		t.Fatalf("List with legacy run_at returned error (the prod bug): %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List returned %d rows, want 1", len(list))
	}

	// Get must not error either; otherwise the detail page 500s too.
	if _, err := svc.Get("sched_legacy"); err != nil {
		t.Fatalf("Get with legacy run_at returned error: %v", err)
	}

	// And Due (the scheduler's polling path) must also tolerate the row.
	due, err := svc.Due(time.Now().Add(24*time.Hour), 50)
	if err != nil {
		t.Fatalf("Due with legacy run_at returned error: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("Due returned %d rows, want 1", len(due))
	}
}

// TestScheduledTask_CreateNormalizesRunAtToUTC locks the second half of
// the fix: Create() converts RunAt to UTC before the INSERT. UTC always
// serializes as "+0000 UTC" — a string modernc's parser handles — so the
// round-trip is guaranteed even though the user posted an RFC3339 string
// with a non-UTC offset (the typical CST+8 case that triggered the prod
// report).
func TestScheduledTask_CreateNormalizesRunAtToUTC(t *testing.T) {
	d := newScheduledTaskTestDB(t)
	seedScheduledTaskRequirement(t, d)

	svc := NewScheduledTaskService(d)

	// Simulate the production payload: a user in CST+8 picks "9-7 23:30".
	// Frontend converts via toRFC3339Local → "2026-09-07T23:30:00+08:00";
	// backend parseRunAt returns a time.Time in FixedZone("", 8*3600).
	cst := time.FixedZone("", 8*3600)
	userPick := time.Date(2026, 9, 7, 23, 30, 0, 0, cst)
	const wantInstant = "2026-09-07T15:30:00Z" // 23:30 +08 == 15:30 UTC

	if _, err := svc.Create(&model.ScheduledTask{
		TaskType:      model.SchedTypeDesign,
		RequirementID: "req_seed_sched",
		ProjectID:     "proj_seed_sched",
		RunAt:         userPick,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Read the stored run_at directly as a string to confirm it landed
	// in the UTC form (not the unparseable "+0800 +0800" form). The
	// modernc driver serializes UTC times as RFC3339 (with "Z") rather
	// than time.Time.String()'s " +0000 UTC" — observed empirically; both
	// forms are parseable, so the round-trip succeeds either way. The
	// critical assertion is the absence of the duplicated "+NNNN +NNNN"
	// tail that triggered the prod Scan error.
	var stored string
	if err := d.QueryRow(`SELECT run_at FROM scheduled_tasks ORDER BY created_at DESC LIMIT 1`).Scan(&stored); err != nil {
		t.Fatalf("read stored run_at: %v", err)
	}
	for _, bad := range []string{" +0800 +0800", " +0000 +0000", " -0500 -0500"} {
		if contains(stored, bad) {
			t.Fatalf("stored run_at = %q contains duplicated-offset tail %q — UTC normalization failed (prod bug regression)",
				stored, bad)
		}
	}

	// Round-trip through List must preserve the absolute instant — the
	// UTC normalization is purely a storage encoding, not a time shift.
	list, err := svc.List("", "", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List len = %d, want 1", len(list))
	}
	wantUTC, _ := time.Parse(time.RFC3339, wantInstant)
	if !list[0].RunAt.Equal(wantUTC) {
		t.Fatalf("RunAt after round-trip = %s, want %s (absolute instant must survive UTC normalization)",
			list[0].RunAt, wantUTC)
	}
}

// TestParseRunAtString_AcceptedFormats documents the formats the parser
// recognizes. Each case is the exact stored form a real DB row could
// hold, so a future driver change that drops one of these will fail
// this test and force the maintainer to extend the list.
func TestParseRunAtString_AcceptedFormats(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Time
	}{
		{
			name: "modernc_utc_string",
			in:   "2026-09-07 15:30:00 +0000 UTC",
			want: time.Date(2026, 9, 7, 15, 30, 0, 0, time.UTC),
		},
		{
			name: "modernc_utc_string_with_nanos",
			in:   "2026-09-07 15:30:00.123456789 +0000 UTC",
			want: time.Date(2026, 9, 7, 15, 30, 0, 123456789, time.UTC),
		},
		{
			name: "modernc_named_zone",
			in:   "2026-09-07 23:30:00 +0800 CST",
			want: time.Date(2026, 9, 7, 23, 30, 0, 0, time.FixedZone("CST", 8*3600)),
		},
		{
			name: "modernc_no_nanos_no_zone_name",
			in:   "2026-09-07 15:30:00 -0700 PDT",
			want: time.Date(2026, 9, 7, 15, 30, 0, 0, time.FixedZone("PDT", -7*3600)),
		},
		{
			name: "rfc3339_nano",
			in:   "2026-09-07T15:30:00.123456789Z",
			want: time.Date(2026, 9, 7, 15, 30, 0, 123456789, time.UTC),
		},
		{
			name: "rfc3339",
			in:   "2026-09-07T15:30:00Z",
			want: time.Date(2026, 9, 7, 15, 30, 0, 0, time.UTC),
		},
		{
			name: "rfc3339_with_offset",
			in:   "2026-09-07T23:30:00+08:00",
			want: time.Date(2026, 9, 7, 23, 30, 0, 0, time.FixedZone("", 8*3600)),
		},
		{
			name: "datetime_local",
			in:   "2026-09-07T15:30",
			// time.Parse with no zone in the layout returns UTC. The
			// handler-side parseRunAt handles this via ParseInLocation, but
			// the service-side parser only needs to be round-trip-correct;
			// callers that care about the server's local zone (the legacy
			// bare datetime-local path) read this row through the handler
			// and re-parse it there.
			want: time.Date(2026, 9, 7, 15, 30, 0, 0, time.UTC),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseRunAtString(c.in)
			if !got.Equal(c.want) {
				t.Fatalf("parseRunAtString(%q) = %s, want %s", c.in, got, c.want)
			}
		})
	}
}

// TestParseRunAtString_UnparseableReturnsZero — on total failure the
// helper returns the zero time rather than an error, so a single legacy
// row with corrupted run_at doesn't fail the whole List. The
// unparseable-row scenario above documents that this is the intended
// behavior; this test pins the contract.
func TestParseRunAtString_UnparseableReturnsZero(t *testing.T) {
	for _, s := range []string{"", "not-a-date", "definitely garbage", "9999-99-99"} {
		if got := parseRunAtString(s); !got.IsZero() {
			t.Errorf("parseRunAtString(%q) = %s, want zero time", s, got)
		}
	}
}

// seedScheduledTaskRequirement inserts the bare project + requirement
// rows the schedule FK chain needs. Mirrors the helper in
// handler/schedule_create_list_test.go without taking on the test's
// extra deps.
func seedScheduledTaskRequirement(t *testing.T, d *db.DB) {
	t.Helper()
	if _, err := d.Exec(`INSERT OR IGNORE INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
		"proj_seed_sched", "Seed", "/tmp/seed", "ready", "main"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO requirements (id, project_id, title) VALUES (?, ?, ?)`,
		"req_seed_sched", "proj_seed_sched", "Seed req"); err != nil {
		t.Fatalf("seed requirement: %v", err)
	}
}

func newScheduledTaskTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sched.db")
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: path})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}
