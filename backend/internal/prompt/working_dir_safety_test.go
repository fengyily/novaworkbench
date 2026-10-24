package prompt

import (
	"strings"
	"testing"
)

// TestWorkingDirSafety_ContainsHardRules pins the three instructions that came
// out of the 2026-10-24 incident. The block is advisory text, so the only way
// to keep it honest is to assert its wording — a well-meaning edit that drops
// the "stop instead of re-cloning" line would silently re-open the failure.
func TestWorkingDirSafety_ContainsHardRules(t *testing.T) {
	required := []struct {
		name string
		frag string
	}{
		{"forbids deleting the working directory", "严禁删除、移动、重命名、清空"},
		{"names rm -rf", "rm -rf"},
		{"names git reset --hard", "git reset --hard"},
		{"names git worktree remove", "git worktree remove"},
		{"stop-and-report on a missing directory", "立即停止并如实报告"},
		{"forbids re-cloning as a repair", "不要删除后重新 clone"},
		{"forbids CI polling", "gh pr checks"},
	}
	for _, r := range required {
		if !strings.Contains(WorkingDirSafety, r.frag) {
			t.Errorf("WorkingDirSafety must %s (missing %q)", r.name, r.frag)
		}
	}
}
