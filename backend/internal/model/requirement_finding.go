package model

import "time"

// RequirementFinding is one discovery a child requirement made while it was
// being designed or implemented, addressed to the master requirement.
//
// A finding is a *proposal*, not a fact: status decides whether it enters
// the master plan. The master's review queue is "every accepted finding that
// has not been folded into the master plan yet".
type RequirementFinding struct {
	ID string `json:"id"`
	// RequirementID is the CHILD requirement that produced the finding.
	RequirementID string `json:"requirement_id"`
	// ParentID is the master requirement the finding is addressed to. Empty
	// when the producer had no master (a top-level requirement).
	ParentID string `json:"parent_id"`
	// Stage — "design" | "coding".
	Stage string `json:"stage"`
	// Severity — "info" | "conflict" | "deviation" | "blocker".
	Severity string `json:"severity"`
	// Status — "pending" | "accepted" | "rejected".
	Status string `json:"status"`
	// Title is the one-line conclusion.
	Title string `json:"title"`
	// Detail is the full text (what the master said / what this child does
	// instead / why).
	Detail string `json:"detail"`
	// Resolution is the reviewer's conclusion — why it was accepted or
	// rejected.
	Resolution string `json:"resolution"`
	// SourceSection names the plan subsection the finding came from, e.g.
	// 「与主需求的偏差」.
	SourceSection string `json:"source_section"`
	// ParentDesignHash is the master plan's hash at the time the finding was
	// filed — it tells the reviewer whether the finding predates a master-plan
	// change.
	ParentDesignHash string `json:"parent_design_hash"`
	// AbsorbedSeq is the absorption counter stamped when the finding was
	// folded into the master plan. 0 = not yet absorbed.
	AbsorbedSeq int `json:"absorbed_seq"`
	// AbsorbedAtRev is the master's design_revision_no at absorption time
	// (display only).
	AbsorbedAtRev int `json:"absorbed_at_rev"`

	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ReviewedAt *time.Time `json:"reviewed_at"`
}
