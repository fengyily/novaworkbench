package model

import "time"

// Revision triggers live in code_review.go (model.RevisionTriggerInitial /
// model.RevisionTriggerRegenerate) — the design-revision table reuses them
// verbatim so the storage layer + UI badge taxonomy stay uniform across
// the two version histories. Note on what doesn't exist: the architect
// stage also lets a user paste a plan via PUT /api/wizard/import-design
// (ImportDesign). We deliberately do NOT add 'import' as a trigger value:
// ImportDesign doesn't go through any revision service today (it bypasses
// the run-path entirely), and retroactively classifying those rows would
// be more bookkeeping than the user-facing UI needs. Future work could
// add it.

// DesignRevision is one frozen version of a requirement's design plan.
//
// requirements.design_docs is ALWAYS the current version (every existing
// read path — the detail page, the doc-refine chat, the coding stage's
// plan-mode resumption — keeps working untouched). This table holds every
// version that was ever current, so 「重新生成技术方案」 can produce a v2
// without destroying the v1 the user has already read, and so a
// disappointing v2 can be switched back. Disappointing the user twice by
// silently overwriting an already-good plan is the bug this table exists
// to prevent.
type DesignRevision struct {
	ID             string `json:"id"`
	RequirementID  string `json:"requirement_id"`
	// RevNo is 1-based and unique per requirement. The newest row's RevNo
	// equals Requirement.DesignRevisionNo. Older versions stay readable
	// here so the version switcher can list them.
	RevNo int `json:"rev_no"`
	// Trigger mirrors model.RevisionTrigger* — what produced THIS frozen
	// version. Future triggers ("import", "seed"…) can slot in without
	// touching the storage layer.
	Trigger string `json:"trigger"`
	// DesignDocs is the Markdown plan that was current at snapshot time.
	DesignDocs     string `json:"design_docs"`
	DesignBaseSHA  string `json:"design_base_sha"`
	Model          string `json:"model"`
	ClaudeConfigID string `json:"claude_config_id"`
	CreatedAt      time.Time `json:"created_at"`
}
