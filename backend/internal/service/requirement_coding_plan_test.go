package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestCodingStepPlanAndPhaseRoundTrip locks down the two columns the plan-mode
// split path added. It is deliberately an end-to-end DB test rather than a unit
// test: the failure mode these columns invite is not bad logic but bad
// plumbing — an ALTER that never ran, or a column added to the SELECT list but
// not to the matching Scan (requirements has two ~55-column query paths that
// must stay in lockstep). Only a real migrate + Get/List catches that.
func TestCodingStepPlanAndPhaseRoundTrip(t *testing.T) {
	d := newTestDB(t)
	reqSvc := NewRequirementService(d)

	created, err := reqSvc.Create(model.CreateRequirementReq{ProjectID: "proj_1", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Fresh rows are idle: an empty coding_phase is what lets start-coding run.
	if created.CodingPhase != "" || created.CodingStepPlan != "" {
		t.Fatalf("fresh row should be idle, got phase=%q plan=%q", created.CodingPhase, created.CodingStepPlan)
	}

	plan := "# 实施计划\n\n1. 加列\n2. 写 handler\n"
	if err := reqSvc.UpdateCodingStepPlan(created.ID, plan); err != nil {
		t.Fatalf("UpdateCodingStepPlan: %v", err)
	}
	if err := reqSvc.UpdateCodingPhase(created.ID, CodingPhasePlanning); err != nil {
		t.Fatalf("UpdateCodingPhase: %v", err)
	}

	got, err := reqSvc.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CodingStepPlan != plan {
		t.Fatalf("coding_step_plan = %q, want %q", got.CodingStepPlan, plan)
	}
	if got.CodingPhase != CodingPhasePlanning {
		t.Fatalf("coding_phase = %q, want %q", got.CodingPhase, CodingPhasePlanning)
	}

	// The List path has its own SELECT + Scan; a column added to one and not
	// the other shifts every subsequent field by one.
	rows, err := reqSvc.List("proj_1", "", "", "", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].CodingPhase != CodingPhasePlanning || rows[0].CodingStepPlan != plan {
		t.Fatalf("List lost the new columns: phase=%q planLen=%d", rows[0].CodingPhase, len(rows[0].CodingStepPlan))
	}

	// Clearing the phase is what releases the start-coding re-entry lock, so
	// "" must be a persistable value and not be coerced to a default.
	if err := reqSvc.UpdateCodingPhase(created.ID, ""); err != nil {
		t.Fatalf("clear phase: %v", err)
	}
	cleared, err := reqSvc.Get(created.ID)
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if cleared.CodingPhase != "" {
		t.Fatalf("coding_phase should clear to empty, got %q", cleared.CodingPhase)
	}
	// Clearing the lock must not wipe the plan the run produced.
	if cleared.CodingStepPlan != plan {
		t.Fatal("clearing coding_phase must not touch coding_step_plan")
	}
}

// TestCodingStepPlanIsSeparateFromCodingPlan is the regression guard for the
// column-naming decision: coding_plan (orchestrator summary, rendered by
// SubTaskPanel) and coding_step_plan (planner input, parsed into sub_tasks)
// are written by different stages of the same run. If they ever collapsed onto
// one column the summary round would silently destroy the step list the batch
// was built from.
func TestCodingStepPlanIsSeparateFromCodingPlan(t *testing.T) {
	d := newTestDB(t)
	reqSvc := NewRequirementService(d)

	created, err := reqSvc.Create(model.CreateRequirementReq{ProjectID: "proj_1", Title: "t", Description: "d"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := reqSvc.UpdateCodingStepPlan(created.ID, "STEPS"); err != nil {
		t.Fatalf("UpdateCodingStepPlan: %v", err)
	}
	if err := reqSvc.UpdateCodingPlan(created.ID, "SUMMARY"); err != nil {
		t.Fatalf("UpdateCodingPlan: %v", err)
	}

	got, err := reqSvc.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CodingStepPlan != "STEPS" {
		t.Fatalf("summary write clobbered the step plan: %q", got.CodingStepPlan)
	}
	if got.CodingPlan != "SUMMARY" {
		t.Fatalf("coding_plan = %q, want SUMMARY", got.CodingPlan)
	}
}
