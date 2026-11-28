package handler

import (
	"strings"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
)

// 「验收核对」的进度对账 —— 把模型报告的结论与子需求**真实**的进度对齐。
//
// WHY THIS EXISTS: an acceptance check is meant to be runnable at ANY moment,
// not only after every child finished. The user asks for a check while the
// children are still being developed, because they want to know where the
// work stands. The prompt already tells the model that "not written yet" is
// not "not implemented" — but a prompt is a REQUEST, and a model that did
// not read the stage notes (or that graded a directory mid-edit) will still
// emit "未通过" for work that is merely in flight.
//
// So the reconciliation happens here, where the truth is known: the database
// says which children finished and which did not. Two rules follow, and both
// of them protect the user from a wrong conclusion:
//
//  1. A verdict may not certify a delivery that has not happened yet. While
//     any child is unfinished, 「已达成」 is impossible — the master can only
//     be 「部分达成」 at best. The verdict becomes a snapshot, and the result
//     says so (Provisional + Pending).
//
//  2. A point may not be failed against a child that has not delivered. A
//     "no" owned by a child that is still being developed is really "not
//     written yet" → 「部分通过」. A "no" owned by a child whose code is not
//     on this machine is really "cannot be seen" → 「无法验证」.
//
// Without this, a check run mid-flight produces the worst possible answer:
// a confident 「未达成」 that blames children for work they are still doing,
// and a master requirement the user closes (or does not close) on it.

// applyAcceptanceProgress reconciles a parsed report with the children's
// development state at check time.
//
// It is a PURE function: the same summary + the same members always produce
// the same result, which is what makes the rules testable without a model.
func applyAcceptanceProgress(summary model.AcceptanceSummary, members []prompt.AcceptanceMember) model.AcceptanceSummary {
	stages := make(map[string]string, len(members))
	pending := []model.AcceptanceChild{}
	for _, m := range members {
		stage := m.Stage
		if stage == "" {
			stage = prompt.AcceptanceStage(m.Status, m.WorktreePath)
		}
		if m.RequirementID != "" {
			stages[m.RequirementID] = stage
		}
		if stage == model.AcceptanceStageDone {
			continue
		}
		pending = append(pending, model.AcceptanceChild{
			ChildID:    m.RequirementID,
			ChildTitle: strings.TrimSpace(m.Title),
			Status:     strings.TrimSpace(m.Status),
			Stage:      stage,
		})
	}

	summary.Pending = pending
	summary.Provisional = len(pending) > 0

	// Rule 1a: an unfinished child means the master cannot be certified as
	// delivered. 「已达成」 becomes 「部分达成」 — the honest verdict for a
	// snapshot.
	if summary.Provisional && summary.Verdict == model.AcceptanceVerdictYes {
		summary.Verdict = model.AcceptanceVerdictPartial
	}

	// Rule 2: a point may not fail against a child that has not delivered.
	failed := 0
	for i := range summary.Checks {
		c := &summary.Checks[i]
		stage, known := stages[c.ChildID]
		if !known || c.ChildID == "" {
			if c.Status == model.AcceptanceVerdictNo {
				failed++
			}
			continue
		}
		c.ChildState = stage
		switch stage {
		case model.AcceptanceStageInProgress:
			// Still being developed: what is missing is "not written yet".
			if c.Status == model.AcceptanceVerdictNo {
				c.Status = model.AcceptanceVerdictPartial
			}
		case model.AcceptanceStageNotStarted, model.AcceptanceStageUnavailable:
			// Nothing to read: the verdict cannot be a failure either.
			if c.Status == model.AcceptanceVerdictNo {
				c.Status = model.AcceptanceCheckUnknown
			}
		}
		if c.Status == model.AcceptanceVerdictNo {
			failed++
		}
	}

	// Rule 1b: 「未达成」 needs a FINAL failure — a point that a finished
	// child missed. When every failure was against work still in flight, the
	// master is not "not met"; it is "not finished". The verdict follows the
	// points, so the panel never shows "未达成" above a table of
	// 「部分通过」.
	if summary.Verdict == model.AcceptanceVerdictNo && len(summary.Checks) > 0 && failed == 0 {
		summary.Verdict = model.AcceptanceVerdictPartial
	}

	// A gap inherits its owner's stage, so the UI can tell "not delivered
	// YET" apart from "not implemented". The gap itself is kept: the user
	// wants to see what is still open, only labelled honestly.
	for i := range summary.Gaps {
		g := &summary.Gaps[i]
		if g.ChildID == "" {
			continue
		}
		if stage, known := stages[g.ChildID]; known {
			g.ChildState = stage
		}
	}

	return summary
}
