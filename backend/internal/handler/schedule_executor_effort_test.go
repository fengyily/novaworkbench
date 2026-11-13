package handler

import (
	"testing"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/scheduler"
)

// TestScheduledCodingParams_CarriesEffort pins that a "coding only"
// scheduled task forwards its reasoning-effort tier (claude CLI --effort)
// into the wizard's run parameters.
//
// Why this needs a test: the tier travels through three hops — the
// scheduled_tasks row → scheduler.CodingParams → codingRunParams → the
// CLI argv — and each hop is a struct literal where a field can be
// dropped by accident. The drop is silent: the run still works, the
// picker still shows a tier, and the setting simply has no effect. That
// is exactly what happened on the coding-only path: the design, wiki and
// merged (design_and_coding) paths all forwarded their tier, so a
// coding-only schedule was the odd one out and its picker was a dead
// control.
//
// The test asserts the whole mapping, not just the tier: a fix that
// added the tier while dropping another field would pass a
// tier-only assertion, and the next review would have to find it.
func TestScheduledCodingParams_CarriesEffort(t *testing.T) {
	req := &model.Requirement{
		ID:          "req_1",
		Title:       "current title",
		Description: "current description",
	}
	p := scheduler.CodingParams{
		RequirementID: "req_1",
		BranchName:    "feat/req_1",
		BaseBranch:    "main",
		Model:         "claude-opus-5",
		AgentServerID: "agent_1",
		SplitTasks:    true,
		ReadKnowledge: true,
		SyncMode:      "local",
		NoBranch:      true,
		// The tier the schedule was created with.
		Effort: llm.EffortHigh,
	}

	cp := scheduledCodingParams(p, req)
	if cp == nil {
		t.Fatal("scheduledCodingParams returned nil")
	}

	if cp.Effort != llm.EffortHigh {
		t.Errorf("codingRunParams.Effort = %q, want %q (the tier must reach the run)",
			cp.Effort, llm.EffortHigh)
	}

	// The rest of the mapping must survive the extraction unchanged.
	if cp.RequirementID != p.RequirementID {
		t.Errorf("RequirementID = %q, want %q", cp.RequirementID, p.RequirementID)
	}
	// Title / Desc come from the CURRENT requirement row, not from the
	// schedule — the user may have edited them after scheduling.
	if cp.RequirementTitle != req.Title {
		t.Errorf("RequirementTitle = %q, want %q", cp.RequirementTitle, req.Title)
	}
	if cp.RequirementDesc != req.Description {
		t.Errorf("RequirementDesc = %q, want %q", cp.RequirementDesc, req.Description)
	}
	if cp.BranchName != p.BranchName {
		t.Errorf("BranchName = %q, want %q", cp.BranchName, p.BranchName)
	}
	if cp.BaseBranch != p.BaseBranch {
		t.Errorf("BaseBranch = %q, want %q", cp.BaseBranch, p.BaseBranch)
	}
	if cp.Model != p.Model {
		t.Errorf("Model = %q, want %q", cp.Model, p.Model)
	}
	if cp.AgentServerID != p.AgentServerID {
		t.Errorf("AgentServerID = %q, want %q", cp.AgentServerID, p.AgentServerID)
	}
	if cp.SplitTasks != p.SplitTasks {
		t.Errorf("SplitTasks = %v, want %v", cp.SplitTasks, p.SplitTasks)
	}
	if cp.ReadKnowledge != p.ReadKnowledge {
		t.Errorf("ReadKnowledge = %v, want %v", cp.ReadKnowledge, p.ReadKnowledge)
	}
	if cp.SyncMode != p.SyncMode {
		t.Errorf("SyncMode = %q, want %q", cp.SyncMode, p.SyncMode)
	}
	// NoBranch is a POINTER on purpose: the wizard prologue reads
	// nil-vs-non-nil to mean "keep the row value" vs "stamp this value".
	// The scheduler always stamps an explicit value, so the pointer must
	// be non-nil here — otherwise the row's no_branch would never be
	// written back.
	if cp.NoBranch == nil {
		t.Fatal("NoBranch = nil, want a non-nil pointer (the scheduler always stamps an explicit value)")
	}
	if *cp.NoBranch != p.NoBranch {
		t.Errorf("NoBranch = %v, want %v", *cp.NoBranch, p.NoBranch)
	}
}

// TestScheduledCodingParams_EmptyEffortStaysEmpty pins that an unset tier
// is passed through as "" rather than being defaulted here.
//
// The defaulting lives in ONE place — llm.Gateway.ResolveEffort — so that
// a settings change takes effect on the next run without touching the
// stored row. Defaulting in the executor as well would freeze the tier
// at schedule-creation time and silently defeat that.
func TestScheduledCodingParams_EmptyEffortStaysEmpty(t *testing.T) {
	req := &model.Requirement{ID: "req_1", Title: "t", Description: "d"}
	cp := scheduledCodingParams(scheduler.CodingParams{RequirementID: "req_1"}, req)
	if cp.Effort != "" {
		t.Errorf("codingRunParams.Effort = %q, want \"\" (defaulting belongs to ResolveEffort)", cp.Effort)
	}
}
