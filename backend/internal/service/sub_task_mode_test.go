package service

import (
	"errors"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestSubTaskModeRoundTrip pins the READ path of sub_tasks.mode.
//
// The mode column rides on the shared SELECT column list that feeds the
// POSITIONAL scan (scanSubTask). Two things can break it:
//
//  1. the column is missing from one of the eight SELECT lists — that read
//     path then returns an empty mode (silently treated as 'auto');
//  2. the column is inserted in the MIDDLE of the list instead of appended
//     at the end — every following field is then mis-assigned (the model
//     column would be read as the mode, or vice versa).
//
// So the test asserts the mode survives a write → read → list round-trip AND
// that the NEIGHBOURING columns are still where they belong.
func TestSubTaskModeRoundTrip(t *testing.T) {
	d := newTestDB(t)
	svc := NewSubTaskService(d, nil)

	created, err := svc.Create(
		"req_1", "child", "do the thing", "model-x", "sid-src",
		"", 0, "", model.SubTaskSessionModeFork, "", model.SubTaskSourceManual,
		model.SubTaskReportModeAuto, model.SubTaskModePlan,
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Mode != model.SubTaskModePlan {
		t.Fatalf("Get mode = %q, want %q — the read path does not carry the column", got.Mode, model.SubTaskModePlan)
	}
	// The neighbouring columns must not have been shifted by the insert.
	if got.Model != "model-x" {
		t.Errorf("model = %q, want %q — the scan list is mis-aligned", got.Model, "model-x")
	}
	if got.Title != "child" {
		t.Errorf("title = %q, want %q — the scan list is mis-aligned", got.Title, "child")
	}
	if got.SessionID != "" || got.SourceSessionID != "sid-src" {
		t.Errorf("session columns shifted: session_id=%q source_session_id=%q", got.SessionID, got.SourceSessionID)
	}
	if got.ReportMode != model.SubTaskReportModeAuto {
		t.Errorf("report_mode = %q, want %q — the scan list is mis-aligned", got.ReportMode, model.SubTaskReportModeAuto)
	}

	// And through List (the panel's read path).
	list, err := svc.List("req_1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List returned %d rows, want 1", len(list))
	}
	if list[0].Mode != model.SubTaskModePlan {
		t.Errorf("listed mode = %q, want %q", list[0].Mode, model.SubTaskModePlan)
	}
	if list[0].Model != "model-x" {
		t.Errorf("listed model = %q, want %q — the scan list is mis-aligned", list[0].Model, "model-x")
	}
}

// TestSubTaskModeDefaultsAuto pins the default: an empty mode means 'auto',
// and every path that does NOT opt into plan mode stays on the historical
// behaviour (orchestrated children / Review rows / adjust children).
func TestSubTaskModeDefaultsAuto(t *testing.T) {
	d := newTestDB(t)
	svc := NewSubTaskService(d, nil)

	created, err := svc.Create(
		"req_1", "child", "do the thing", "", "",
		"", 0, "", model.SubTaskSessionModeFork, "", model.SubTaskSourceManual,
		model.SubTaskReportModeAuto, "",
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := svc.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Mode != model.SubTaskModeAuto {
		t.Errorf("mode = %q, want %q — an empty mode must normalize to auto", got.Mode, model.SubTaskModeAuto)
	}
}

// TestSubTaskPlanStore pins the plan table: upsert (not append), and a
// graceful empty result for an unknown id (the card shows "no plan", not a
// 500).
func TestSubTaskPlanStore(t *testing.T) {
	d := newTestDB(t)
	svc := NewSubTaskService(d, nil)

	created, err := svc.Create(
		"req_1", "child", "do the thing", "", "",
		"", 0, "", model.SubTaskSessionModeFork, "", model.SubTaskSourceManual,
		model.SubTaskReportModeAuto, model.SubTaskModePlan,
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Unknown id → empty string, no error.
	content, err := svc.GetPlan("st_nope")
	if err != nil {
		t.Fatalf("GetPlan(unknown): %v", err)
	}
	if content != "" {
		t.Errorf("GetPlan(unknown) = %q, want empty", content)
	}

	if err := svc.SavePlan(created.ID, "## 目标\n第一版"); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	content, err = svc.GetPlan(created.ID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if content != "## 目标\n第一版" {
		t.Errorf("GetPlan = %q, want the saved plan", content)
	}

	// A second save REPLACES the plan, it does not append to it.
	if err := svc.SavePlan(created.ID, "## 目标\n第二版"); err != nil {
		t.Fatalf("SavePlan (second): %v", err)
	}
	content, err = svc.GetPlan(created.ID)
	if err != nil {
		t.Fatalf("GetPlan (second): %v", err)
	}
	if content != "## 目标\n第二版" {
		t.Errorf("GetPlan = %q, want the REPLACED plan", content)
	}
}

// TestConvertPlanToDev pins the atomic plan → auto conversion: the status and
// the mode flip together, the saved plan survives, and a row that is not a
// finished plan-mode row is refused (and left untouched).
func TestConvertPlanToDev(t *testing.T) {
	d := newTestDB(t)
	svc := NewSubTaskService(d, nil)

	newRow := func(t *testing.T, mode, status string) *model.SubTask {
		t.Helper()
		row, err := svc.Create(
			"req_1", "child", "do the thing", "", "",
			"", 0, "", model.SubTaskSessionModeFork, "", model.SubTaskSourceManual,
			model.SubTaskReportModeAuto, mode,
		)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if status != model.SubTaskStatusPending {
			if _, err := d.Exec(`UPDATE sub_tasks SET status=? WHERE id=?`, status, row.ID); err != nil {
				t.Fatalf("seed status: %v", err)
			}
		}
		return row
	}

	// --- the happy path -----------------------------------------------------
	row := newRow(t, model.SubTaskModePlan, model.SubTaskStatusDone)
	if err := svc.SavePlan(row.ID, "## 改动点\n改 foo.go"); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	converted, err := svc.ConvertPlanToDev(row.ID)
	if err != nil {
		t.Fatalf("ConvertPlanToDev: %v", err)
	}
	if converted.Status != model.SubTaskStatusPending {
		t.Errorf("status = %q, want %q", converted.Status, model.SubTaskStatusPending)
	}
	if converted.Mode != model.SubTaskModeAuto {
		t.Errorf("mode = %q, want %q", converted.Mode, model.SubTaskModeAuto)
	}
	if converted.Artifact != "" {
		t.Errorf("artifact = %q, want it cleared", converted.Artifact)
	}
	// The plan must survive — the implementation phase injects it.
	plan, err := svc.GetPlan(row.ID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if plan != "## 改动点\n改 foo.go" {
		t.Errorf("plan = %q, want it preserved", plan)
	}

	// --- an ordinary (auto) row is never rolled back ------------------------
	autoRow := newRow(t, model.SubTaskModeAuto, model.SubTaskStatusDone)
	if _, err := svc.ConvertPlanToDev(autoRow.ID); !errors.Is(err, ErrAlreadyConverting) {
		t.Fatalf("ConvertPlanToDev(auto row) err = %v, want ErrAlreadyConverting", err)
	}
	unchanged, err := svc.Get(autoRow.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if unchanged.Status != model.SubTaskStatusDone || unchanged.Mode != model.SubTaskModeAuto {
		t.Errorf("auto row changed: status=%q mode=%q", unchanged.Status, unchanged.Mode)
	}

	// --- a failed plan row cannot be converted either ----------------------
	errRow := newRow(t, model.SubTaskModePlan, model.SubTaskStatusError)
	if _, err := svc.ConvertPlanToDev(errRow.ID); !errors.Is(err, ErrAlreadyConverting) {
		t.Fatalf("ConvertPlanToDev(error row) err = %v, want ErrAlreadyConverting", err)
	}
	stillErr, err := svc.Get(errRow.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stillErr.Status != model.SubTaskStatusError || stillErr.Mode != model.SubTaskModePlan {
		t.Errorf("error row changed: status=%q mode=%q", stillErr.Status, stillErr.Mode)
	}
}

// TestHasPlanFlag pins the "does this row carry a plan" signal the card
// renders the plan section from.
//
// It has to survive the plan → auto conversion: once the user approves the
// plan the row's mode flips to 'auto', but the plan section must stay
// visible above the implementation report — that is what makes the two-phase
// shape legible. So the flag follows the PLAN, not the mode.
func TestHasPlanFlag(t *testing.T) {
	d := newTestDB(t)
	svc := NewSubTaskService(d, nil)

	planned, err := svc.Create(
		"req_1", "planned", "do the thing", "", "",
		"", 0, "", model.SubTaskSessionModeFork, "", model.SubTaskSourceManual,
		model.SubTaskReportModeAuto, model.SubTaskModePlan,
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	plain, err := svc.Create(
		"req_1", "plain", "do the thing", "", "",
		"", 0, "", model.SubTaskSessionModeFork, "", model.SubTaskSourceManual,
		model.SubTaskReportModeAuto, model.SubTaskModeAuto,
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.SavePlan(planned.ID, "## 目标"); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}

	// Before the conversion: the planned row carries the flag, the ordinary
	// one does not.
	got, err := svc.Get(planned.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.HasPlan {
		t.Error("HasPlan = false on a row with a saved plan")
	}
	plainGot, err := svc.Get(plain.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if plainGot.HasPlan {
		t.Error("HasPlan = true on a row without a plan")
	}

	// List must carry the same signal (that is the payload the panel reads).
	list, err := svc.List("req_1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	flags := map[string]bool{}
	for _, row := range list {
		flags[row.ID] = row.HasPlan
	}
	if !flags[planned.ID] {
		t.Error("List: the planned row lost its HasPlan flag")
	}
	if flags[plain.ID] {
		t.Error("List: the ordinary row gained a HasPlan flag")
	}

	// After the conversion the flag must STILL be set — the plan stays.
	if _, err := d.Exec(`UPDATE sub_tasks SET status=? WHERE id=?`, model.SubTaskStatusDone, planned.ID); err != nil {
		t.Fatalf("seed status: %v", err)
	}
	converted, err := svc.ConvertPlanToDev(planned.ID)
	if err != nil {
		t.Fatalf("ConvertPlanToDev: %v", err)
	}
	if !converted.HasPlan {
		t.Error("HasPlan = false after the plan → auto conversion — the plan section would disappear")
	}
	if converted.Mode != model.SubTaskModeAuto {
		t.Errorf("mode = %q, want %q", converted.Mode, model.SubTaskModeAuto)
	}
}

// TestDeleteByIDsRemovesPlan pins the explicit cleanup: the plan table has no
// FK (SQLite never enables foreign_keys), so DeleteByIDs has to clear it
// itself — otherwise deleting a sub-task leaves its plan behind.
func TestDeleteByIDsRemovesPlan(t *testing.T) {
	d := newTestDB(t)
	svc := NewSubTaskService(d, nil)

	row, err := svc.Create(
		"req_1", "child", "do the thing", "", "",
		"", 0, "", model.SubTaskSessionModeFork, "", model.SubTaskSourceManual,
		model.SubTaskReportModeAuto, model.SubTaskModePlan,
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.SavePlan(row.ID, "## 目标"); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	if err := svc.DeleteByIDs([]string{row.ID}); err != nil {
		t.Fatalf("DeleteByIDs: %v", err)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM sub_task_plans WHERE sub_task_id=?`, row.ID).Scan(&n); err != nil {
		t.Fatalf("count plans: %v", err)
	}
	if n != 0 {
		t.Errorf("sub_task_plans rows = %d, want 0 — DeleteByIDs must clean the plan table", n)
	}
}

// TestRedoKeepsPlanMode pins the reset semantics: `mode` is a work-item
// property (like session_mode / report_mode), not a result of the previous
// attempt. A FAILED plan-mode sub-task that the user retries must re-run the
// PLAN phase, not silently become an implementation run.
func TestRedoKeepsPlanMode(t *testing.T) {
	d := newTestDB(t)
	svc := NewSubTaskService(d, nil)

	row, err := svc.Create(
		"req_1", "child", "do the thing", "", "",
		"", 0, "", model.SubTaskSessionModeFork, "", model.SubTaskSourceManual,
		model.SubTaskReportModeAuto, model.SubTaskModePlan,
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := d.Exec(`UPDATE sub_tasks SET status=? WHERE id=?`, model.SubTaskStatusError, row.ID); err != nil {
		t.Fatalf("seed status: %v", err)
	}
	rearmed, err := svc.RedoReset(row.ID, "")
	if err != nil {
		t.Fatalf("RedoReset: %v", err)
	}
	if rearmed.Mode != model.SubTaskModePlan {
		t.Errorf("mode = %q after redo, want %q — a failed plan must re-run the plan phase", rearmed.Mode, model.SubTaskModePlan)
	}
}
