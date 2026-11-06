// Package handler — requirement branch naming: the single source of truth
// for "which branch does this requirement develop on?".
//
// The convention is fixed: every requirement that enters development lands
// on exactly one branch, derived from its kind and id:
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
// The frontend mirrors this convention in api/client.ts
// (branchPrefixForKind / defaultBranchName) — the two MUST stay in sync;
// push_pr_shell_fallback_test.go pins the Go side of that contract.
package handler

import (
	"strings"

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
