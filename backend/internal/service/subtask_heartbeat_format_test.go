package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

// The tests in this file cover the heartbeat column's WRITE path, which the
// pre-existing self-heal tests (subtask_double_dispatch_test.go) do not:
// seedRunningRow stages batch_id_seq_run with a bound Go time.Time, while
// production stamped it with SQL CURRENT_TIMESTAMP. That mismatch is exactly
// what let the double-dispatch loop ship green — the stale predicate compares
// the column against a bound time.Time cutoff, so a column written through a
// different representation is compared as text, not as an instant.
//
// Every test here therefore goes through ClaimNextPending / MarkHeartbeat
// rather than seeding the timestamp directly.

// TestClaimNextPending_StampsHeartbeatComparableToCutoff is the direct
// regression for the production loop: a child claimed *just now* was
// immediately judged stale against a 2-minute cutoff, self-healed back to
// pending, re-claimed by the next tick, and launched a second claude process
// for the same sub-task (logs showed heartbeat_age≈4s being healed).
func TestClaimNextPending_StampsHeartbeatComparableToCutoff(t *testing.T) {
	d := newTestDB(t)
	seedRunningRow(t, d, "st_claim", "batch_ts", "", "", model.SubTaskStatusPending, 0)

	svc := NewSubTaskService(d)
	st, ok, err := svc.ClaimNextPending("batch_ts")
	if err != nil {
		t.Fatalf("ClaimNextPending: %v", err)
	}
	if !ok || st == nil {
		t.Fatal("ClaimNextPending did not claim the pending row")
	}

	// A heartbeat stamped microseconds ago must not match a 2-minute cutoff.
	rows, err := svc.RecoverStaleRunningInBatch("batch_ts", 2*time.Minute)
	if err != nil {
		t.Fatalf("RecoverStaleRunningInBatch: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a just-claimed row was judged stale (flipped %d rows: %+v) — "+
			"the heartbeat write and the stale cutoff are not comparable as instants", len(rows), rows)
	}
	if row, _ := svc.Get("st_claim"); row.Status != model.SubTaskStatusRunning {
		t.Errorf("st_claim status = %q, want running", row.Status)
	}
}

// TestClaimNextPending_HeartbeatRoundTripsAsInstant asserts the stamped
// heartbeat reads back as the instant it was written. A UTC-vs-local
// representation bug shows up here as a whole-timezone-offset skew (the
// reporting host ran at UTC+8), which is what made the Go-side
// heartbeat_age log and the SQL-side predicate disagree about the same row.
func TestClaimNextPending_HeartbeatRoundTripsAsInstant(t *testing.T) {
	d := newTestDB(t)
	seedRunningRow(t, d, "st_rt", "batch_rt", "", "", model.SubTaskStatusPending, 0)

	svc := NewSubTaskService(d)
	before := time.Now()
	if _, ok, err := svc.ClaimNextPending("batch_rt"); err != nil || !ok {
		t.Fatalf("ClaimNextPending: ok=%v err=%v", ok, err)
	}

	row, err := svc.Get("st_rt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.BatchIDSeqRun == nil {
		t.Fatal("ClaimNextPending left batch_id_seq_run NULL")
	}
	// Allow a second of slack for CURRENT_TIMESTAMP-style truncation, but
	// nothing near a timezone offset.
	age := time.Since(*row.BatchIDSeqRun)
	if age < -time.Second || age > 30*time.Second {
		t.Errorf("heartbeat age = %s (stamped %s, claim started %s) — want ~0s; "+
			"a multi-hour skew means the column and time.Now() disagree on the zone",
			age, row.BatchIDSeqRun.Format(time.RFC3339Nano), before.Format(time.RFC3339Nano))
	}
}

// TestMarkHeartbeat_RefreshClearsStaleness locks the 5s heartbeat's whole
// purpose: refreshing an already-stale row must pull it back inside the
// cutoff. With the column written via CURRENT_TIMESTAMP the refresh landed in
// a representation the predicate could not compare, so a live child stayed
// "stale" no matter how often it heartbeat.
func TestMarkHeartbeat_RefreshClearsStaleness(t *testing.T) {
	d := newTestDB(t)
	seedRunningRow(t, d, "st_live", "batch_hb", "sid-live", "job-live", model.SubTaskStatusRunning, 3*time.Minute)

	svc := NewSubTaskService(d)
	if err := svc.MarkHeartbeat("st_live", "sid-live"); err != nil {
		t.Fatalf("MarkHeartbeat: %v", err)
	}

	rows, err := svc.RecoverStaleRunningInBatch("batch_hb", 2*time.Minute)
	if err != nil {
		t.Fatalf("RecoverStaleRunningInBatch: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("refreshed row still judged stale (flipped %+v)", rows)
	}
	row, _ := svc.Get("st_live")
	if row.Status != model.SubTaskStatusRunning {
		t.Errorf("st_live status = %q, want running after heartbeat refresh", row.Status)
	}
	if row.SessionID != "sid-live" || row.JobID != "job-live" {
		t.Errorf("st_live identity clobbered: session_id=%q job_id=%q", row.SessionID, row.JobID)
	}
}

// seedLegacyHeartbeatRow stages a running row whose heartbeat was written the
// way a pre-fix build wrote it: SQL CURRENT_TIMESTAMP, i.e. second-resolution
// UTC text with no zone suffix. offsetSec shifts it into the past. A live
// nova.db already holds rows in this shape, so the stale predicate has to
// keep ordering them correctly after the fix.
//
// The datetime('now', ...) shift is SQLite-specific, which is fine — newTestDB
// builds a SQLite database, and SQLite is the dialect whose text comparison
// made this a bug in the first place.
func seedLegacyHeartbeatRow(t *testing.T, d *db.DB, id, batchID string, batchSeq, offsetSec int) {
	t.Helper()
	d.Exec("INSERT INTO projects (id, name, local_path) VALUES ('proj_lg', 'p', '/tmp/p')")
	d.Exec("INSERT INTO requirements (id, project_id, title) VALUES ('req_lg', 'proj_lg', 'req')")
	if _, err := d.Exec(`INSERT INTO sub_tasks (id, requirement_id, title, prompt, status,
		session_id, source_session_id, job_id, model, source,
		batch_id, batch_seq, batch_id_seq_run, created_at, updated_at)
		VALUES (?, 'req_lg', 't', 'p', ?, 'sid-legacy', 'src', 'job-legacy', '', 'auto', ?, ?,
			datetime('now', ?), CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		id, model.SubTaskStatusRunning, batchID, batchSeq,
		fmt.Sprintf("-%d seconds", offsetSec)); err != nil {
		t.Fatalf("seed legacy sub_task %s: %v", id, err)
	}
}

// TestRecoverStaleRunningInBatch_LegacyCurrentTimestampRows is the upgrade
// path: after the fix, rows still carrying a CURRENT_TIMESTAMP heartbeat must
// be judged by age, not by text layout. Before the fix a fresh legacy row was
// unconditionally stale under UTC+N, which is what re-dispatched live
// children.
func TestRecoverStaleRunningInBatch_LegacyCurrentTimestampRows(t *testing.T) {
	d := newTestDB(t)
	seedLegacyHeartbeatRow(t, d, "st_legacy_fresh", "batch_lg", 1, 3)   // 3s old  -> live
	seedLegacyHeartbeatRow(t, d, "st_legacy_stale", "batch_lg", 2, 600) // 10m old -> stale

	svc := NewSubTaskService(d)
	rows, err := svc.RecoverStaleRunningInBatch("batch_lg", 2*time.Minute)
	if err != nil {
		t.Fatalf("RecoverStaleRunningInBatch: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("flipped %d rows, want exactly 1 (only the 10m-old legacy row): %+v", len(rows), rows)
	}
	if rows[0].ID != "st_legacy_stale" {
		t.Errorf("flipped %q, want st_legacy_stale", rows[0].ID)
	}
	if row, _ := svc.Get("st_legacy_fresh"); row.Status != model.SubTaskStatusRunning {
		t.Errorf("st_legacy_fresh status = %q, want running — a 3s-old legacy heartbeat "+
			"was judged stale, which re-dispatches a live child", row.Status)
	}
	if row, _ := svc.Get("st_legacy_stale"); row.Status != model.SubTaskStatusPending {
		t.Errorf("st_legacy_stale status = %q, want pending", row.Status)
	}
}

// TestRecoverStaleRunningInBatch_ReportsOnlyFlippedRows covers the
// observability contract that the positional-truncation bug broke: the
// per-row breadcrumb must name the rows actually flipped. A batch holding one
// genuinely stale row and one live (just-claimed) row must report exactly the
// stale one — previously the SELECT and the UPDATE evaluated the predicate
// independently and `affected[:n]` attributed flips by sort order.
func TestRecoverStaleRunningInBatch_ReportsOnlyFlippedRows(t *testing.T) {
	d := newTestDB(t)
	// Seeded stale row sorts first by id; the live row is claimed below.
	seedRunningRow(t, d, "st_aaa_stale", "batch_mix", "sid-stale", "job-stale", model.SubTaskStatusRunning, 5*time.Minute)
	seedRunningRow(t, d, "st_zzz_live", "batch_mix", "", "", model.SubTaskStatusPending, 0)

	svc := NewSubTaskService(d)
	if _, ok, err := svc.ClaimNextPending("batch_mix"); err != nil || !ok {
		t.Fatalf("ClaimNextPending: ok=%v err=%v", ok, err)
	}

	rows, err := svc.RecoverStaleRunningInBatch("batch_mix", 2*time.Minute)
	if err != nil {
		t.Fatalf("RecoverStaleRunningInBatch: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("flipped %d rows, want exactly 1 (the stale one): %+v", len(rows), rows)
	}
	if rows[0].ID != "st_aaa_stale" {
		t.Errorf("reported flipped row = %q, want st_aaa_stale", rows[0].ID)
	}
	if rows[0].SessionID != "sid-stale" || rows[0].JobID != "job-stale" {
		t.Errorf("breadcrumb metadata = %+v, want session_id=sid-stale job_id=job-stale", rows[0])
	}
	if rows[0].HeartbeatAge < 4*time.Minute || rows[0].HeartbeatAge > 6*time.Minute {
		t.Errorf("reported heartbeat_age = %s, want ~5m", rows[0].HeartbeatAge)
	}
	// The freshly claimed row keeps running.
	if row, _ := svc.Get("st_zzz_live"); row.Status != model.SubTaskStatusRunning {
		t.Errorf("st_zzz_live status = %q, want running", row.Status)
	}
}
