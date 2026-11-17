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
// The rules live in the service package (service.ResolveAutoBase) because
// the READ path needs them too: it computes the effective base the frontend
// seeds its BASE picker with, so an untouched picker echoes the auto value
// back and is not mistaken for a deliberate choice. Both sides must run the
// SAME function — a second copy here would drift from the one the frontend
// is handed.
func resolveAutoBase(req *model.Requirement, projDefaultBranch, parentBranch string) (string, string) {
	return service.ResolveAutoBase(req, projDefaultBranch, parentBranch)
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
// The rules live in the service package (service.ParentBranchBase) so the
// read path can compute the same value when it fills
// model.Requirement.BaseBranchEffective — the frontend seeds its BASE picker
// with it, and tier 1 of the resolution chain only treats an incoming value
// as an explicit choice when it DIFFERS from the auto value.
func parentBranchBaseWith(lookup func(string) (*model.Requirement, error), req *model.Requirement) string {
	return service.ParentBranchBase(lookup, req)
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
