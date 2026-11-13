package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
)

// TestSubTaskEffortRoundTrip pins the READ path of sub_tasks.effort.
//
// This column was originally write-only: UpdateEffort stored the tier, but
// none of the SELECT lists that feed scanSubTask carried the column, so
// every row came back with Effort == "". The bug was SILENT — it compiles,
// the write succeeds, and the only symptom is that the tier vanishes the
// moment the row is read back (which is every time the process restarts,
// and every time the panel re-lists the rows).
//
// Two things were broken by it:
//
//  1. SubTaskRunner's "reuse the tier the last run used" branch
//     (effortPick = st.Effort) could never fire for a row loaded from the
//     database, so a 重做 / 继续 after a restart silently fell back to the
//     stage default instead of the tier the user last chose;
//  2. the column list and the scan list had to stay in lockstep. They are
//     long and hand-maintained, and a mismatch in EITHER direction is a
//     runtime error ("expected N destination arguments") rather than a
//     compile error.
//
// The test therefore asserts the tier survives a write → read → list
// round-trip, and that the read path does not shift any NEIGHBOURING
// column: the model and claude_config_id fields sit right next to the
// effort column in the struct, so a mis-aligned scan would surface as one
// of them holding the tier's value.
func TestSubTaskEffortRoundTrip(t *testing.T) {
	d := newTestDB(t)
	svc := NewSubTaskService(d, nil)

	created, err := svc.Create(
		"req_1", "child", "do the thing", "model-x", "sid-src",
		"", 0, "", model.SubTaskSessionModeFork, "", model.SubTaskSourceManual,
		model.SubTaskReportModeAuto,
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A fresh row carries no tier — the column defaults to "".
	got, err := svc.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Effort != "" {
		t.Fatalf("fresh row effort = %q, want \"\"", got.Effort)
	}

	// Write a tier.
	if err := svc.UpdateEffort(created.ID, llm.EffortHigh); err != nil {
		t.Fatalf("UpdateEffort: %v", err)
	}

	// Read it back through Get (the runner's read path).
	got, err = svc.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after UpdateEffort: %v", err)
	}
	if got.Effort != llm.EffortHigh {
		t.Errorf("effort = %q, want %q — the read path does not carry the column", got.Effort, llm.EffortHigh)
	}
	// The neighbouring columns must not have been shifted by the insert.
	if got.Model != "model-x" {
		t.Errorf("model = %q, want %q — the scan list is mis-aligned", got.Model, "model-x")
	}
	if got.Title != "child" {
		t.Errorf("title = %q, want %q — the scan list is mis-aligned", got.Title, "child")
	}

	// And through List (the panel's read path).
	list, err := svc.List("req_1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List returned %d rows, want 1", len(list))
	}
	if list[0].Effort != llm.EffortHigh {
		t.Errorf("listed effort = %q, want %q", list[0].Effort, llm.EffortHigh)
	}
	if list[0].Model != "model-x" {
		t.Errorf("listed model = %q, want %q", list[0].Model, "model-x")
	}
}

// TestSubTaskEffortRejectsInvalidTier pins the write-side validation: an
// unknown tier is stored as "" ("not specified") rather than in a shape the
// read paths would have to defend against.
func TestSubTaskEffortRejectsInvalidTier(t *testing.T) {
	d := newTestDB(t)
	svc := NewSubTaskService(d, nil)

	created, err := svc.Create(
		"req_1", "child", "do the thing", "model-x", "sid-src",
		"", 0, "", model.SubTaskSessionModeFork, "", model.SubTaskSourceManual,
		model.SubTaskReportModeAuto,
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.UpdateEffort(created.ID, "ultra"); err != nil {
		t.Fatalf("UpdateEffort: %v", err)
	}
	got, err := svc.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Effort != "" {
		t.Errorf("effort = %q, want \"\" for an invalid tier", got.Effort)
	}
}
