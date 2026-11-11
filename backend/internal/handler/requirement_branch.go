// Package handler — requirement branch naming and base-branch resolution:
// the single source of truth for "which branch does this requirement
// develop on, and what is it based on?".
//
// The branch-name convention is fixed: every requirement that enters
// development lands on exactly one branch, derived from its kind and id:
//
//	requirement / idea / wiki / legacy → feat/req_<id>
//	issue                            → fix/req_<id>
//
// The req_ prefix is ALWAYS kept — it is the id namespace the user sees in
// the URL and in the UI, and it keeps requirement branches from colliding
// with hand-made branches that happen to share a name.
//
// Before this file the convention lived in five separate hand-written
// concatenations plus three "requirement-<id>" fallbacks on the Agent
// Server path, so a change to the rule meant editing eight places and
// missing one produced a branch the UI could not predict. Everything now
// derives from requirementBranchName.
//
// The same file also owns the BASE-branch resolution chain
// (resolveEffectiveBase): which branch a run forks from. A child
// requirement (parent_id set) bases itself on the branch its parent
// requirement actually develops on — the stacked-PR stack bottom; a
// requirement without a usable parent branch falls back to the project's
// default branch.
//
// The frontend mirrors the branch-name convention in api/client.ts
// (branchPrefixForKind / defaultBranchName) — the two MUST stay in sync;
// push_pr_shell_fallback_test.go pins the Go side of that contract.
package handler

import (
	"strings"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// branchPrefixForKind returns the conventional branch prefix for a
// requirement kind: "fix" for issues, "feat" for everything else
// (requirement / idea / wiki / legacy empty). Used by
// requirementBranchName so the dev branch matches the change type the user
// declared when creating the requirement.
func branchPrefixForKind(kind string) string {
	if kind == service.KindIssue {
		return "fix"
	}
	return "feat"
}

// requirementBranchName returns the deterministic dev branch name for a
// requirement: "fix/req_xxx" for issues, "feat/req_xxx" otherwise.
//
// The design and coding stages share the same requirement id, so both
// resolve to the same branch and the coding stage reuses the design
// worktree (EnsureWorktree is idempotent per req+branch) instead of
// colliding with a second branch.
func requirementBranchName(kind, reqID string) string {
	return branchPrefixForKind(kind) + "/" + reqID
}

// isRequirementBranch reports whether name looks like a branch this app
// created for a requirement (feat/req_xxx / fix/req_xxx).
//
// Only such branches are safe to publish to origin on the user's behalf —
// arbitrary bases (release/*, develop, …) belong to the user.
//
// Deliberately prefix-based rather than a strict regexp: util.NewID emits
// req_ plus 8 or 16 hex characters depending on the caller, and legacy rows
// may carry longer ids, so the suffix length is not fixed.
func isRequirementBranch(name string) bool {
	for _, prefix := range []string{"feat/req_", "fix/req_"} {
		if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
			return true
		}
	}
	return false
}

// resolveAutoBase resolves the base branch WITHOUT the caller's override —
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
func resolveAutoBase(req *model.Requirement, projDefaultBranch, parentBranch string) (string, string) {
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

// resolveEffectiveBase is the full resolution chain: the caller's override
// (tier 1) on top of resolveAutoBase's tiers 2..6.
//
// Tier 1 fires ONLY when the incoming value is non-empty AND differs from
// the auto value. That comparison is what lets the frontend echo the
// effective value back verbatim: the frontend seeds its BASE picker with
// the backend-computed effective base, so an untouched picker sends back
// exactly the auto value and is NOT mistaken for a deliberate choice. A
// value the user actually edited always differs from auto and therefore
// wins.
//
// Note that the auto value INCLUDES tier 2 (a persisted explicit choice).
// If it did not, a requirement whose stored explicit base is "develop"
// would see an incoming "main" (== the project default == auto) as "not
// explicit", fall through to tier 2 and silently discard the user's edit.
func resolveEffectiveBase(req *model.Requirement, projDefaultBranch, incoming, parentBranch string) (string, string) {
	auto, autoSource := resolveAutoBase(req, projDefaultBranch, parentBranch)
	in := strings.TrimSpace(incoming)
	if in != "" && in != auto {
		return in, model.BaseSourceExplicit
	}
	return auto, autoSource
}

// parentBranchBaseWith returns the base branch the PARENT requirement works
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
func parentBranchBaseWith(lookup func(string) (*model.Requirement, error), req *model.Requirement) string {
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
	if parent.NoBranch {
		return ""
	}
	if b := strings.TrimSpace(parent.BranchName); b != "" {
		return b
	}
	if b := strings.TrimSpace(parent.BaseBranch); b != "" {
		return b
	}
	return ""
}

// parentBranchBase wires parentBranchBaseWith to the requirement service.
// The DB read is isolated here so the rules themselves remain a pure
// function.
func (h *WizardHandler) parentBranchBase(req *model.Requirement) string {
	if h == nil || h.reqSvc == nil {
		return ""
	}
	return parentBranchBaseWith(h.reqSvc.Get, req)
}

// resolveEffectiveBaseFor is the one-call entry point every stage uses to
// decide which base branch this run works from. It resolves the parent
// branch, reads the project's default branch and runs the full chain.
//
// Safe against a nil req and a nil proj: a missing project simply means
// "no project default" and a missing requirement means "nothing persisted
// to reuse" — both degrade to the next tier instead of panicking.
func (h *WizardHandler) resolveEffectiveBaseFor(req *model.Requirement, proj *model.Project, incoming string) (base, source string) {
	parentBranch := h.parentBranchBase(req)
	projDefault := ""
	if proj != nil {
		projDefault = proj.DefaultBranch
	}
	return resolveEffectiveBase(req, projDefault, incoming, parentBranch)
}
