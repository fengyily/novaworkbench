package service

import (
	"fmt"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
)

// Base-branch resolution — the single source of truth for "which branch does
// this requirement develop from?".
//
// The rules used to live in the handler package (handler/requirement_branch.go)
// but the requirement payload is assembled in the service package, so the read
// path could not reuse them: the row the frontend receives never carried the
// effective base, and the frontend therefore seeded its BASE picker with the
// project default. On a child requirement that value differs from the
// inherited one, so the very first design / coding run sent back a value the
// resolver read as "the user deliberately chose this" — pinning the base and
// killing the parent inheritance for good.
//
// The functions below are therefore shared by BOTH sides of the seam:
//
//   - the runtime path (handler/requirement_branch.go resolves the base for a
//     run and stamps it onto the row), and
//   - the read path (attachEffectiveBase computes model.Requirement.
//     BaseBranchEffective, the value the frontend seeds its picker with).
//
// Because both sides run the SAME function on the SAME inputs, the value the
// frontend echoes back is by construction the auto value — so tier 1 of the
// chain ("the caller overrode the base") never fires for an untouched picker.

// ResolveAutoBase resolves the base branch WITHOUT the caller's override —
// i.e. tiers 2..6 of the resolution chain.
//
// The tiers, in order:
//
//  2. explicit    — the user picked this base last time and we remembered
//     it (requirements.base_branch_source == "explicit").
//  3. parent      — a child requirement bases itself on the branch its
//     parent requirement actually develops on (feat/req_<parent> — the
//     stack bottom of a split).
//  4. requirement — this requirement's own previously persisted value, but
//     ONLY when that value was not merely derived from the
//     project default.
//  5. project     — projects.default_branch.
//  6. default     — the hard-coded "main" fallback.
//
// Tier 4 deliberately does NOT reuse a value whose source is "project",
// "default" or empty (legacy rows). Those are DERIVED values: the project
// default branch is a moving target, and freezing whatever happened to be
// current when the row was written would silently ignore a later change of
// projects.default_branch — old requirements would keep developing on a
// branch the project no longer uses. The only value genuinely worth
// remembering across runs is the user's own explicit choice, which tier 2
// already covers.
func ResolveAutoBase(req *model.Requirement, projDefaultBranch, parentBranch string) (string, string) {
	if req != nil {
		if req.BaseBranchSource == model.BaseSourceExplicit && strings.TrimSpace(req.BaseBranch) != "" {
			return req.BaseBranch, model.BaseSourceExplicit
		}
	}
	if b := strings.TrimSpace(parentBranch); b != "" {
		return b, model.BaseSourceParent
	}
	if req != nil {
		if req.ParentID == "" && strings.TrimSpace(req.BaseBranch) != "" &&
			req.BaseBranchSource != "" &&
			req.BaseBranchSource != model.BaseSourceProject &&
			req.BaseBranchSource != model.BaseSourceDefault {
			return req.BaseBranch, model.BaseSourceRequirement
		}
	}
	if b := strings.TrimSpace(projDefaultBranch); b != "" {
		return b, model.BaseSourceProject
	}
	return "main", model.BaseSourceDefault
}

// ParentBranchBase returns the base branch the PARENT requirement works
// from, or "" when there is nothing to inherit.
//
// SEMANTICS: a child requirement bases itself on the branch its parent
// actually DEVELOPS on (parent.BranchName, i.e. feat/req_<parent>), and
// only falls back to the parent's base when the parent has no development
// branch of its own.
//
// Why branch-first: this is the stacked-PR model. Every requirement that
// enters development lands on feat/req_<id>; a split master keeps that
// branch as the STACK BOTTOM and every child branches off it, so the
// children's PRs target the master's branch and the whole stack merges
// bottom-up. Reading the parent's BASE instead would flatten every child
// onto the same line (the project default), producing independent sibling
// PRs — and would also contradict what the UI promises ("子需求基于主需求
// 分支开发").
//
// The base fallback keeps pre-split data working: a master that was
// created before the branch convention (or that runs in no_branch mode)
// has no branch_name, so its children inherit its base.
//
// Every failure degrades to "" and never blocks the run:
//   - no parent (nil row / empty parent_id)
//   - the parent row was deleted, or the lookup failed
//   - the parent runs in "develop on base" mode (no_branch) — it has no
//     branch of its own to inherit
//   - the parent has neither a development branch nor a base
//
// The lookup is injected so the rules stay a pure function and can be
// unit-tested without a database.
func ParentBranchBase(lookup func(string) (*model.Requirement, error), req *model.Requirement) string {
	if req == nil || req.ParentID == "" {
		return ""
	}
	if lookup == nil {
		return ""
	}
	parent, err := lookup(req.ParentID)
	if err != nil || parent == nil {
		return ""
	}
	return parentInheritBase(parent)
}

// parentInheritBase is THE rule for what a child requirement inherits from
// its master: the master's DEVELOPMENT branch, or the master's own base when
// it has not developed a branch yet.
//
// A master in "develop on base" mode (no_branch) has NO branch of its own —
// it commits straight onto its base. There is therefore nothing to inherit:
// the function returns "" and the caller falls back to the project default.
//
// The rule is shared by both sides of the seam, and they MUST agree:
//
//   - the read path (ParentBranchBase → the resolver's tier 3) computes the
//     base a child would develop on;
//   - the creation paths (inheritedBaseBranch) persist that same base.
//
// When they disagreed, the child row carried a base the resolver would never
// reproduce: the frontend seeded its picker with the resolver's value and
// sent it back, so the next design / coding run overwrote the persisted
// value with a different one — the child's PR target silently drifted away
// from the master's line.
func parentInheritBase(parent *model.Requirement) string {
	if parent == nil {
		return ""
	}
	if parent.NoBranch {
		return ""
	}
	if b := strings.TrimSpace(parent.BranchName); b != "" {
		return b
	}
	return strings.TrimSpace(parent.BaseBranch)
}

// attachEffectiveBase fills model.Requirement.BaseBranchEffective on every
// row of items — the base branch the requirement WOULD use right now.
//
// The field is NOT persisted (see the model comment): it is recomputed on
// every read so it always reflects the CURRENT parent branch and the CURRENT
// project default. The frontend seeds its BASE picker with it, which is what
// makes an untouched picker round-trip as "not an explicit choice" instead of
// silently overriding the parent-inheritance rule.
//
// Cost: two extra queries per call — one for the distinct project default
// branches, one for the distinct parent rows. Both are primary-key lookups;
// the parent lookup is a single IN query so a list of N children costs one
// round-trip, not N.
func (s *RequirementService) attachEffectiveBase(items []model.Requirement) {
	if len(items) == 0 {
		return
	}

	projIDs := make([]string, 0, 4)
	seenProj := map[string]bool{}
	parentIDs := make([]string, 0, 4)
	seenParent := map[string]bool{}
	for i := range items {
		if id := strings.TrimSpace(items[i].ProjectID); id != "" && !seenProj[id] {
			seenProj[id] = true
			projIDs = append(projIDs, id)
		}
		if id := strings.TrimSpace(items[i].ParentID); id != "" && !seenParent[id] {
			seenParent[id] = true
			parentIDs = append(parentIDs, id)
		}
	}

	projDefaults := s.projectDefaultBranches(projIDs)
	parents := s.parentRequirementRows(parentIDs)

	// The lookup mirrors the handler's runtime lookup (ParentBranchBase takes
	// an injected lookup to stay a pure function): a missing parent row is an
	// error, which the rules degrade to "" — never a block.
	lookup := func(id string) (*model.Requirement, error) {
		if p, ok := parents[id]; ok {
			return p, nil
		}
		return nil, fmt.Errorf("requirement %s not found", id)
	}

	for i := range items {
		parentBranch := ParentBranchBase(lookup, &items[i])
		base, _ := ResolveAutoBase(&items[i], projDefaults[items[i].ProjectID], parentBranch)
		items[i].BaseBranchEffective = base
	}
}

// projectDefaultBranches reads projects.default_branch for the given project
// ids in ONE query. A missing project row is simply absent from the map —
// the resolver then falls through to the "main" sentinel.
func (s *RequirementService) projectDefaultBranches(ids []string) map[string]string {
	out := map[string]string{}
	if len(ids) == 0 {
		return out
	}
	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := s.db.Query(
		"SELECT id, default_branch FROM projects WHERE id IN ("+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, def string
		if err := rows.Scan(&id, &def); err != nil {
			continue
		}
		out[id] = def
	}
	return out
}

// parentRequirementRows reads the columns the parent-branch rules need for
// the given requirement ids in ONE query: the development branch, the
// fallback base and the no-branch flag.
func (s *RequirementService) parentRequirementRows(ids []string) map[string]*model.Requirement {
	out := map[string]*model.Requirement{}
	if len(ids) == 0 {
		return out
	}
	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := s.db.Query(
		"SELECT id, branch_name, base_branch, no_branch FROM requirements WHERE id IN ("+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var r model.Requirement
		if err := rows.Scan(&r.ID, &r.BranchName, &r.BaseBranch, &r.NoBranch); err != nil {
			continue
		}
		row := r
		out[r.ID] = &row
	}
	return out
}

// inheritedParentRow loads the columns the base-inheritance rule needs from
// the parent requirement — ONE targeted query, without the derived columns
// (sub-task counts, effective base, …) a full Get would pull in.
//
// no_branch is part of the read: a master in "develop on base" mode has no
// branch of its own to inherit, and the creation path must apply the same
// rule the read path applies (parentInheritBase) — otherwise the persisted
// base is one the resolver would never reproduce.
//
// Returns nil when the row is gone or unreadable; the caller then falls back
// to the project default.
func (s *RequirementService) inheritedParentRow(parentID string) *model.Requirement {
	parentID = strings.TrimSpace(parentID)
	if parentID == "" {
		return nil
	}
	var p model.Requirement
	err := s.db.QueryRow(
		"SELECT id, project_id, branch_name, base_branch, no_branch FROM requirements WHERE id = ?", parentID,
	).Scan(&p.ID, &p.ProjectID, &p.BranchName, &p.BaseBranch, &p.NoBranch)
	if err != nil {
		return nil
	}
	return &p
}

// inheritedBaseBranch resolves the base branch a CHILD requirement inherits
// from its parent — the shared rule behind BOTH creation paths:
//
//   - the manual path (RequirementService.Create, a hand-made sub-requirement)
//   - the split path (RequirementService.CreateChildren, an AI-decomposed one)
//
// The mapping is the identity on the parent's DEVELOPMENT branch: every
// requirement that enters development lands on feat/req_<id>, and a master
// keeps that branch as the stack bottom its children branch off.
// Fallbacks: parent development branch → parent base → project default →
// "main".
//
// The no_branch rule comes from parentInheritBase — the very same rule the
// read path applies — so a master that develops on its base contributes
// NOTHING to inherit (it has no branch of its own) and the child falls back
// to the project default, exactly like the resolver would resolve it.
//
// The source is ALWAYS model.BaseSourceParent — never explicit. "explicit"
// means "the user picked this on THIS requirement", and the user did not:
// they picked it on the master, and the child merely follows.
//
// The parent may be nil (deleted between the validation and the read) — the
// caller then falls back to the project default.
func (s *RequirementService) inheritedBaseBranch(parent *model.Requirement) string {
	if b := parentInheritBase(parent); b != "" {
		return b
	}
	return s.projectDefaultBranchFor(parent)
}

// projectDefaultBranchFor returns the project's default branch, or "main"
// when the project is unknown / has no default.
func (s *RequirementService) projectDefaultBranchFor(parent *model.Requirement) string {
	projectID := ""
	if parent != nil {
		projectID = parent.ProjectID
	}
	if projectID == "" {
		return "main"
	}
	var def string
	if err := s.db.QueryRow("SELECT default_branch FROM projects WHERE id = ?", projectID).Scan(&def); err == nil {
		if b := strings.TrimSpace(def); b != "" {
			return b
		}
	}
	return "main"
}
