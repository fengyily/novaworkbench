package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestSplitGenerationColumnRoundTrip guards the SELECT/Scan column alignment
// of RequirementService.List and RequirementService.Get.
//
// Both queries are single ~90-column SELECTs with hand-written matching Scan
// lists. split_generation was inserted right after split_order in BOTH lists
// — in the middle, next to its split_* siblings, where it reads naturally.
// Inserting it in the middle shifts every following column by one and
// silently mis-assigns fields: cross_requirement_id would be read as
// split_generation, and the row would come back looking like it had been
// split a hundred times. That failure mode compiles, passes a smoke test, and
// is nearly impossible to spot by eye — so it gets a test.
//
// The assertion therefore checks split_generation SEPARATELY from its
// neighbours: if the columns ever shift again, one of the two fields comes
// back holding the other's value and this fails.
func TestSplitGenerationColumnRoundTrip(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_1", "proj_1", "/tmp/proj_1")
	s := NewRequirementService(d, nil)

	created, err := s.Create(model.CreateRequirementReq{ProjectID: "proj_1", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A distinctive value that cannot be confused with any neighbour column:
	// the neighbouring split_order is 0 and cross_requirement_id is "".
	//
	// The two anchor columns bracket the inserted one from both sides:
	// split_order sits right BEFORE split_generation in the Scan list, and
	// analysis_session_id sits two positions AFTER it. A shifted column list
	// would move the value into one of its neighbours — and since the list is
	// read positionally, a shift that keeps the count right would otherwise
	// go unnoticed.
	if _, err := d.Exec(
		"UPDATE requirements SET split_order = 3, split_generation = 7, analysis_session_id = 'sess-anchor' WHERE id = ?",
		created.ID,
	); err != nil {
		t.Fatalf("set split columns: %v", err)
	}

	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SplitGeneration != 7 {
		t.Errorf("Get: split_generation = %d, want 7", got.SplitGeneration)
	}
	// The columns around it must still hold their own values — if the lists
	// ever drift apart, one of them would come back holding another's value.
	if got.SplitOrder != 3 {
		t.Errorf("Get: split_order = %d, want 3 — the column lists are misaligned", got.SplitOrder)
	}
	if got.CrossRequirementID != "" {
		t.Errorf("Get: cross_requirement_id = %q, want \"\" — the column lists are misaligned", got.CrossRequirementID)
	}
	if got.AnalysisSessionID != "sess-anchor" {
		t.Errorf("Get: analysis_session_id = %q, want %q — the column lists are misaligned", got.AnalysisSessionID, "sess-anchor")
	}

	listed, err := s.List("proj_1", "", "", "", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, row := range listed {
		if row.ID != created.ID {
			continue
		}
		found = true
		if row.SplitGeneration != 7 {
			t.Errorf("List: split_generation = %d, want 7", row.SplitGeneration)
		}
		if row.SplitOrder != 3 {
			t.Errorf("List: split_order = %d, want 3 — the column lists are misaligned", row.SplitOrder)
		}
		if row.CrossRequirementID != "" {
			t.Errorf("List: cross_requirement_id = %q, want \"\" — the column lists are misaligned", row.CrossRequirementID)
		}
		if row.AnalysisSessionID != "sess-anchor" {
			t.Errorf("List: analysis_session_id = %q, want %q — the column lists are misaligned", row.AnalysisSessionID, "sess-anchor")
		}
	}
	if !found {
		t.Errorf("List: the requirement %s is missing from the result set", created.ID)
	}
}
