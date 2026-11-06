package handler

import "testing"

// TestRequirementBranchName pins the branch-name convention every
// requirement develops on: feat/req_<id> for requirement / idea / wiki /
// legacy rows, fix/req_<id> for issues.
//
// The req_ prefix is part of the contract (the user sees it in the URL and
// the UI), so the expected values keep it verbatim.
func TestRequirementBranchName(t *testing.T) {
	cases := []struct {
		name string
		kind string
		want string
	}{
		{"requirement", "requirement", "feat/req_x"},
		{"idea", "idea", "feat/req_x"},
		{"wiki", "wiki", "feat/req_x"},
		{"legacy empty kind", "", "feat/req_x"},
		{"issue", "issue", "fix/req_x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := requirementBranchName(c.kind, "req_x"); got != c.want {
				t.Fatalf("requirementBranchName(%q, %q) = %q, want %q", c.kind, "req_x", got, c.want)
			}
		})
	}
}

// TestIsRequirementBranch guards the predicate that decides whether we may
// publish a branch to origin on the user's behalf. Only branches this app
// created for a requirement qualify; anything else (the project default,
// a release line, a hand-made branch) belongs to the user.
//
// The suffix length is deliberately NOT constrained — requirement ids are
// req_ plus 8 or 16 hex characters depending on the caller, and legacy
// rows may carry longer ids.
func TestIsRequirementBranch(t *testing.T) {
	cases := []struct {
		name   string
		branch string
		want   bool
	}{
		{"feat requirement branch", "feat/req_abc", true},
		{"fix requirement branch", "fix/req_abc", true},
		{"feat with long id", "feat/req_92a0381bc53bb526", true},
		{"fix with long id", "fix/req_92a0381bc53bb526", true},
		{"project default", "main", false},
		{"develop", "develop", false},
		{"release line", "release/1.0", false},
		{"prefix without id", "feat/", false},
		{"fix prefix without id", "fix/", false},
		{"empty", "", false},
		{"similar but not prefixed", "feature/req_abc", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isRequirementBranch(c.branch); got != c.want {
				t.Fatalf("isRequirementBranch(%q) = %v, want %v", c.branch, got, c.want)
			}
		})
	}
}
