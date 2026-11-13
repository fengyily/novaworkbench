package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestRequirementEffortColumnsRoundTrip guards the SELECT/Scan column
// alignment of RequirementService.Get.
//
// The Get query is a single ~90-column SELECT with a matching Scan list.
// The three effort columns were appended at the very END of both lists —
// deliberately. Inserting them in the middle (next to their *_model
// siblings, where they read more naturally) would shift every following
// column by one and silently mis-assign fields: architect_model would be
// read as analyst_model, and the run would look like it had an effort tier
// when it had a model instead. That failure mode compiles, passes a smoke
// test, and is nearly impossible to spot by eye — so it gets a test.
//
// The assertion therefore checks the model and the effort columns
// SEPARATELY: if the columns ever shift again, one of the two fields comes
// back holding the other's value and this fails.
func TestRequirementEffortColumnsRoundTrip(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_1", "proj_1", "/tmp/proj_1")
	s := NewRequirementService(d, nil)

	created, err := s.Create(model.CreateRequirementReq{ProjectID: "proj_1", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Distinct values per column: a swap between any two of them has to
	// show up as a wrong value, not as "both happened to be equal".
	if err := s.UpdateStageModel(created.ID, "analyst_model", "model-analyst"); err != nil {
		t.Fatalf("UpdateStageModel(analyst): %v", err)
	}
	if err := s.UpdateStageModel(created.ID, "architect_model", "model-architect"); err != nil {
		t.Fatalf("UpdateStageModel(architect): %v", err)
	}
	if err := s.UpdateStageModel(created.ID, "developer_model", "model-developer"); err != nil {
		t.Fatalf("UpdateStageModel(developer): %v", err)
	}
	if err := s.UpdateAnalystEffort(created.ID, "low"); err != nil {
		t.Fatalf("UpdateAnalystEffort: %v", err)
	}
	if err := s.UpdateArchitectEffort(created.ID, "high"); err != nil {
		t.Fatalf("UpdateArchitectEffort: %v", err)
	}
	if err := s.UpdateDeveloperEffort(created.ID, "medium"); err != nil {
		t.Fatalf("UpdateDeveloperEffort: %v", err)
	}

	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// Models first: these columns sit in the MIDDLE of the Scan list, so
	// they are the ones a shifted column list would corrupt.
	if got.AnalystModel != "model-analyst" {
		t.Errorf("analyst_model = %q, want %q", got.AnalystModel, "model-analyst")
	}
	if got.ArchitectModel != "model-architect" {
		t.Errorf("architect_model = %q, want %q", got.ArchitectModel, "model-architect")
	}
	if got.DeveloperModel != "model-developer" {
		t.Errorf("developer_model = %q, want %q", got.DeveloperModel, "model-developer")
	}

	// Then the effort columns, each holding its own value.
	if got.AnalystEffort != "low" {
		t.Errorf("analyst_effort = %q, want %q", got.AnalystEffort, "low")
	}
	if got.ArchitectEffort != "high" {
		t.Errorf("architect_effort = %q, want %q", got.ArchitectEffort, "high")
	}
	if got.DeveloperEffort != "medium" {
		t.Errorf("developer_effort = %q, want %q", got.DeveloperEffort, "medium")
	}
}

// TestUpdateStageEffortRejectsUnknownColumn pins the SQL-injection guard.
//
// UpdateStageEffort interpolates the column name into the UPDATE statement,
// so the name has to come from a fixed whitelist. A caller that passes
// anything else gets an error instead of a SQL fragment.
func TestUpdateStageEffortRejectsUnknownColumn(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_1", "proj_1", "/tmp/proj_1")
	s := NewRequirementService(d, nil)

	created, err := s.Create(model.CreateRequirementReq{ProjectID: "proj_1", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, bad := range []string{"", "id", "analyst_effort = 'x', title", "effort", "ANALYST_EFFORT"} {
		if err := s.UpdateStageEffort(created.ID, bad, "high"); err == nil {
			t.Errorf("UpdateStageEffort(column=%q) = nil error, want a rejection", bad)
		}
	}
}

// TestUpdateStageEffortDropsInvalidTier pins the write-side validation: an
// unknown tier is stored as "" ("not specified") rather than in a shape the
// read paths would have to defend against.
func TestUpdateStageEffortDropsInvalidTier(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_1", "proj_1", "/tmp/proj_1")
	s := NewRequirementService(d, nil)

	created, err := s.Create(model.CreateRequirementReq{ProjectID: "proj_1", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.UpdateArchitectEffort(created.ID, "ultra"); err != nil {
		t.Fatalf("UpdateArchitectEffort: %v", err)
	}
	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ArchitectEffort != "" {
		t.Errorf("architect_effort = %q, want \"\" for an invalid tier", got.ArchitectEffort)
	}
}
