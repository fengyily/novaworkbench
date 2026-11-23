package service

import "testing"

// TestCrossRequirementPermissionSplit guards the read/write split of the
// cross-requirement permission keys: the menu key only opens the pages
// (read), the manage key is required by every mutating route. A viewer that
// accidentally receives the manage key can delete cross-requirements and
// launch token-consuming AI jobs.
func TestCrossRequirementPermissionSplit(t *testing.T) {
	permKeys := map[string]bool{}
	for _, p := range DefaultPermissions() {
		permKeys[p.Key] = true
	}
	for _, key := range []string{"menu.cross_requirements", "cross_requirement.manage"} {
		if !permKeys[key] {
			t.Fatalf("permission %q is missing from the seed catalog", key)
		}
	}

	rolePerms := DefaultRolePermissions()
	for _, role := range []string{"developer", "viewer"} {
		if _, ok := rolePerms[role]; !ok {
			t.Fatalf("built-in role %q is missing from DefaultRolePermissions", role)
		}
	}

	has := func(role, key string) bool {
		for _, k := range rolePerms[role] {
			if k == key {
				return true
			}
		}
		return false
	}

	if !has("viewer", "menu.cross_requirements") {
		t.Error("viewer must keep menu.cross_requirements so it can read the progress")
	}
	if has("viewer", "cross_requirement.manage") {
		t.Error("viewer must NOT hold cross_requirement.manage — it is read-only")
	}
	if !has("developer", "menu.cross_requirements") {
		t.Error("developer must hold menu.cross_requirements")
	}
	if !has("developer", "cross_requirement.manage") {
		t.Error("developer must hold cross_requirement.manage so it can create and split cross-requirements")
	}
}
